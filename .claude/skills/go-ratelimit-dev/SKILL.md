---
name: go-ratelimit-dev
description: Конвенции проекта ratelimit-lab — Go-библиотека rate limiting (token bucket, sliding window, leaky bucket + lock-free CAS token bucket) + бенчмарк-сьют. Порт Limiter, инжектируемый Clock, паттерн lock-free CAS-цикла, тестирование гонок (-race + stress-тесты с большим числом горутин), паттерн бенчмарков testing.B/RunParallel. Использовать при реализации любого этапа кодирования ratelimit-lab.
---

# SKILL: go-ratelimit-dev — конвенции проекта `ratelimit-lab`

Конкретные конвенции **именно этого проекта** для написания Go-кода. Не общий
гайд «как писать Go», а специфика `ratelimit-lab`. Применяй при реализации
любого этапа. Опорные документы:
[`../../../docs/TECHNICAL_PLAN.md`](../../../docs/TECHNICAL_PLAN.md),
[`../../../docs/PLAN.md`](../../../docs/PLAN.md).

---

## 1. Структура: `internal/` + `cmd/`, почему нет `pkg/`

- Всё ядро — в `internal/limiter/`, компилятор запрещает внешний импорт: это
  учебное приложение, а не публикуемая библиотека. `pkg/` не заводить (вынос —
  только при реальном спросе, см. `POST_MVP_PLAN.md`).
- **По файлу на алгоритм:** `tokenbucket.go`, `slidingwindow.go`,
  `leakybucket.go`, `lockfree_tokenbucket.go`. Общий контракт и абстракции
  (`Limiter`, `Clock`) — в `limiter.go`.
- `cmd/bench/main.go` — **тонкий** харнесс: флаги, драйвер нагрузки, рендер
  таблицы. Никакой алгоритмической логики в `main`.
- Заглушки Этапа 0 помечены `TODO(Этап N)` и содержат `panic(...)` в теле +
  compile-time assertion `var _ Limiter = (*T)(nil)`. При реализации **заменяй
  содержимое файла**, не создавай `*_v2.go`. Assertion не удаляй — он держит
  контракт.

## 2. Порт `Limiter` — центральный паттерн

Все алгоритмы за одним интерфейсом (ports & adapters):

```go
type Limiter interface {
    Allow() bool        // одно решение, без блокировки
    AllowN(n int) bool  // батч из n единиц
}
```

- `Allow()` реализуй как `return x.AllowN(1)` — вся логика в `AllowN`, чтобы не
  дублировать refill/leak-математику.
- **Главный инвариант каждой реализации: безопасность для конкурентного вызова из
  множества горутин.** Реализация, корректная только на одном потоке, — баг.
- Конструктор на каждый алгоритм: `NewTokenBucket(rate float64, capacity int, clk Clock) *TokenBucket`
  (и аналоги). Clock — последним параметром; в проде передаём `SystemClock`.
- Никаких `Allow()` без блокировки-обещаний: MVP-контракт **не блокирует** —
  денаем немедленно возвращают `false` (blocking `Wait` — post-MVP).

## 3. Абстракция `Clock` — детерминизм тестов

- Внутри лимитеров **никогда не зови `time.Now()` напрямую** — только через
  инжектированный `Clock`. Иначе тесты корректности станут флейки на `Sleep`.
- Прод — `SystemClock` (уже есть в `limiter.go`). Тесты — управляемый fake:

```go
type fakeClock struct{ mu sync.Mutex; now time.Time }
func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }
```

- fakeClock заводим в Этапе 1 (в `internal/limiter`, `_test.go` или хелпер) и
  переиспользуем всеми. Он должен быть потокобезопасен — его читают из lock-free
  тестов параллельно.

## 4. Тестирование: три яруса

- **Корректность (table-driven + fake-clock).** Детерминированно, без `Sleep`:
  прокручивай `fakeClock.Advance` и проверяй `Allow`. Кейсы: пустой/полный
  bucket, refill/leak во времени, граница окна (sliding), `AllowN`.
- **Гонки (`-race`).** `go test -race ./...` — на **весь пакет**, не только на
  lock-free. Mutex-версии тоже гоняем под гонки. Обязательно перед коммитом
  кода этапов 1+.
- **Stress-тест инварианта «≤ лимита» (Этап 4, центральный).** Паттерн:

```go
func TestLockFreeTokenBucket_NoOverAdmit(t *testing.T) {
    clk := &fakeClock{now: time.Unix(0, 0)}
    lim := NewLockFreeTokenBucket(rate, capacity, clk) // время не двигаем → refill=0
    var allowed atomic.Int64
    var wg sync.WaitGroup
    for i := 0; i < goroutines; i++ {
        wg.Add(1)
        go func() { defer wg.Done()
            for j := 0; j < callsPer; j++ { if lim.Allow() { allowed.Add(1) } }
        }()
    }
    wg.Wait()
    if got := allowed.Load(); got > int64(capacity) {
        t.Fatalf("over-admit: allowed %d > capacity %d (гонка/потерянное обновление)", got, capacity)
    }
}
```

  Замораживаем время (refill=0) → сколько бы горутин ни било, суммарно
  разрешённых **не больше `capacity`**. Превышение = гонка. Гоняй с `-race`,
  `-count=N`, `GOMAXPROCS>1`. Опционально — `testing/quick` для быстрого property.

## 5. Lock-free token bucket — паттерн CAS-цикла (Этап 4)

Центральный конкурентный код проекта. Правила:

- Состояние (токены + метка времени) упаковать так, чтобы обновлять **одним**
  `atomic.CompareAndSwap`. Варианты: `atomic.Pointer[state]` на неизменяемый
  снапшот, либо упаковка в один `uint64` (битовые поля), если влезает.
- Канонический CAS-цикл:

```go
for {
    old := b.state.Load()              // atomic read
    next, ok := computeNext(old, now)  // чистая функция: refill + попытка списать токен
    if !ok { return false }            // токенов нет — денай, состояние не трогаем
    if b.state.CompareAndSwap(old, next) { // atomic write
        return true
    }
    // CAS не прошёл (кто-то опередил) — повторяем с новым old
}
```

- `computeNext` — **чистая** (без side-effect): читает старое состояние + `now`,
  возвращает новое. Весь недетерминизм — только в CAS-петле. Это и делает stress-
  тест значимым.
- Остерегайся **ABA/потерянных обновлений**: не полагайся на «значение не
  менялось» без CAS; всегда read→compute→CAS→retry. Никаких read-modify-write
  без CAS.
- `atomic.Value`/`atomic.Pointer` для снапшота-состояния; счётчики в тестах —
  `atomic.Int64`, не `int` под mutex.

## 6. Бенчмарки — паттерн `testing.B` (Этап 5)

- Микробенчмарки живут рядом с кодом: `internal/limiter/*_bench_test.go`.
- Конкурентный профиль — через `b.RunParallel` (именно он показывает разницу
  mutex vs lock-free под contention):

```go
func BenchmarkTokenBucket_Parallel(b *testing.B) {
    lim := NewTokenBucket(1e9, 1<<30, SystemClock) // ёмкость велика → меряем механику, не отказы
    b.ResetTimer()
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() { lim.Allow() }
    })
}
```

- Бенчмарки — по одному паттерну на все алгоритмы, чтобы сравнение было честным
  (одинаковая нагрузка, одинаковый профиль). `cmd/bench` — человеко-
  ориентированный харнесс поверх тех же лимитеров (флаги `-algo/-goroutines/
  -rate/-duration`), печатает сравнительную таблицу для README.
- Не микрооптимизируй под бенчмарк ценой корректности — цель проекта в честном
  сравнении, а не в подгонке цифр.

## 7. Чек-лист перед коммитом кода этапа

```bash
export PATH="$HOME/sdk/go/bin:$PATH"
go build ./... && go vet ./... && go test -race ./...
gofmt -l .   # пусто = отформатировано
```

Только стандартная библиотека; внешние модули — не добавлять без веской причины.
