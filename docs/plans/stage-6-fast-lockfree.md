# Этап 6 — AtomicTokenBucket: CAS с продуманной раскладкой памяти

План реализации для исполнителя (Opus). Составлен 2026-09-06 по итогам
исследования [`../RESEARCH.md`](../RESEARCH.md) (§1.3–§1.5, §2.1, §2.2) и
замеров основной сессии. Ветка `stage-6/fast-lockfree` заведена и активна.
Формат — по образцу [`stage-4-lockfree-tokenbucket.md`](stage-4-lockfree-tokenbucket.md);
конвенции кода — `.claude/skills/go-ratelimit-dev/SKILL.md`.

**Исполнителю: работай сам, инструментами Read/Edit/Write/Bash напрямую. НЕ
вызывай Agent-тул, НЕ делегируй, НЕ спавни под-агентов. Никаких git-команд.**
Go: `export PATH="$HOME/sdk/go/bin:$PATH"` (1.23.4).

---

## Решения, принятые пользователем (НЕ переоткрывать)

1. **Пятая реализация рядом; `LockFreeTokenBucket` не трогается вообще.**
   Её проигрыш втрое — задокументированный результат Этапа 5 и точка сравнения
   «наивный CAS». Правило «не плоди `*_v2.go`» здесь не применяется: это не
   новая версия того же, а третья точка сравнения (mutex / наивный CAS /
   CAS с раскладкой).
2. **Порт `Clock` не меняется.** Добавляется опциональный второй интерфейс
   (быстрый путь), проверяемый type assertion, с деградацией к `Clock.Now()`.
   Существующие четыре лимитера и их тесты не трогаются вовсе.
3. **`admitEpsilon` остаётся, но становится относительным** — снимает потолок
   2^24, не меняя поведение на текущих лимитах. Все существующие тесты обязаны
   остаться зелёными **без правки ожиданий**.

Опора — замеры основной сессии (i7-11700, 16 ядер, Go 1.23.4, parallel-16,
один бинарь, средние `-count=5`): наивный CAS 1460 нс → одно слово 1132 →
часы до петли 628 → **+ вынос констант + падинг 128 Б = 194.6 нс** против
466.8 у mutex. Четыре приёма обязательны, все четыре закладываются в дизайн
ниже. Числа не переоткрывать и в код/доки как обещание не вписывать —
см. «Риски».

## Цель и объём

| Файл | Действие |
|---|---|
| `internal/limiter/atomic_tokenbucket.go` | **создать**: тип `AtomicTokenBucket` |
| `internal/limiter/atomic_tokenbucket_test.go` | **создать**: тесты, включая стресс |
| `internal/limiter/limiter.go` | добавить `SinceClock` + `Since` у `realClock`; `admitEpsilon` const → функция; переписать её doc-комментарий; дополнить package doc |
| `internal/limiter/limiter_test.go` | **создать**: тесты относительного эпсилона |
| `internal/limiter/tokenbucket.go` | одна строка: call site эпсилона |
| `internal/limiter/lockfree_tokenbucket.go` | одна строка: call site эпсилона |
| `internal/limiter/leakybucket.go` | одна строка: call site эпсилона |
| `internal/limiter/slidingwindow.go` | одна строка: call site эпсилона |
| `internal/limiter/limiter_bench_test.go` | добавить бенчмарки atomic + no-op базовые линии |
| `cmd/bench/main.go` | `-algo=atomic`: const, allAlgos, makeLimiter, валидация |
| `cmd/bench/main_test.go` | **добавить** (не править) кейсы для atomic |
| `README.md`, `CLAUDE.md` | обновить (см. «Документация») |

Существующие тестовые файлы (`tokenbucket_test.go`, `lockfree_tokenbucket_test.go`,
`leakybucket_test.go`, `slidingwindow_test.go`, `clock_test.go`) **не редактируются**.
Только стандартная библиотека.

## Имя типа и файла — `AtomicTokenBucket`, `atomic_tokenbucket.go`

Обоснование: суть реализации — не «быстрее», а **всё изменяемое состояние в
одном атомарном слове** (`atomic.Int64`) с раскладкой памяти под CAS. Оба
существующих варианта тоже lock-free; различает их представление состояния:
`LockFreeTokenBucket` = CAS по указателю на снапшот, `AtomicTokenBucket` =
CAS по одному слову. Имя повторяет индустрию: Envoy `AtomicTokenBucketImpl`
(RESEARCH §2.1), uber-go `atomicInt64Limiter`. Отвергнуто: `FastLockFree...`
(обещание числа, зависящего от сборки), `GCRATokenBucket` (алгоритм — тот же
token bucket, меняется представление, RESEARCH §2.1 прямо снимает возражение
«GCRA — другой алгоритм»).

Конструктор — сигнатура один-в-один с соседями (бенчмарки гоняют все
реализации по одному паттерну):

```go
func NewAtomicTokenBucket(rate float64, capacity int, clk Clock) *AtomicTokenBucket
```

Имя в `cmd/bench`: `-algo=atomic`.

## 1. Схема представления состояния — точно и с выкладками

### Представление

Хранится **время, а не токены** (Envoy/GCRA, RESEARCH §2.1, подтверждено
прогоном на целых наносекундах: ровно 1000 допусков там, где текущая схема без
эпсилона даёт 910). Единственное изменяемое поле:

```go
vt atomic.Int64 // "virtual zero time": момент (нс от origin), в который запас токенов равен нулю
```

Константы, вычисляемые один раз в конструкторе:

```go
T    = int64(math.Round(1e9 / rate)) // наносекунд на один токен (эмиссионный интервал)
capT = int64(capacity) * T           // временной эквивалент полного бакета; 0 при capacity <= 0
```

Инвариант представления: `tokens(now) = min(capacity, (nowNs - vt) / T)`,
где `nowNs` — наносекунды от `origin` (момент `clk.Now()` в конструкторе).
Бакет стартует полным: `vt0 = -capT`.

### Переход (чистая функция)

```go
// nextAtomicState is the pure transition function of the CAS loop.
// eff clamps the credit at capacity; admission is exact int64 arithmetic.
func nextAtomicState(vt, nowNs, capT, need int64) (next int64, ok bool) {
    eff := vt
    if floor := nowNs - capT; eff < floor {
        eff = floor // older vt would credit more than a full bucket — clamp
    }
    if nowNs-eff < need { // need = int64(n) * T
        return 0, false
    }
    return eff + need, true
}
```

Сравнение допуска **включительное** (`>=` в форме `< need -> deny`): advance
ровно на K токенов допускает ровно K — как у baseline.

### Проверка арифметики на сценариях (сделано на бумаге, кодеру не повторять)

- *Полный бакет, rate=1, cap=3*: `vt0=-3e9`, now=0 → допуски двигают vt:
  −2e9, −1e9, 0; четвёртый: avail=0 < 1e9 → deny. ✓
- *Advance ровно K, rate=2, cap=10*: T=5e8; слив 10 → vt=0; +3s → avail=3e9 =
  6·T → ровно 6 допусков, 7-й deny. ✓
- *Кламп после простоя, rate=1, cap=5*: слив → vt=0; +5000s: floor=5e12−5e9,
  eff=floor, avail=5e9 → ровно 5. ✓
- *Дробный refill, rate=1, cap=1*: +0.5s: avail=5e8 < 1e9 deny; ещё +0.5s:
  avail=1e9 admit, vt=now. ✓
- *Steady-state 100 мс шаг, rate=1, cap=1, 10000 шагов*: T=1e9 точен, шаг
  1e8 нс точен, вся арифметика целая → допуски на шагах 1, 11, 21, … = ровно
  1000. Эпсилон не нужен вообще. ✓

### Границы и переполнения

- **Диапазон времени.** `nowNs = int64` нс от origin: 2^63−1 нс ≈ 292 года
  аптайма процесса. Достаточно; задокументировать.
- **Бюджет на span бакета:** `const maxBucketSpanNanos = 1 << 62` (≈146 лет).
  Требования конструктора: `1 <= T <= maxBucketSpanNanos` и
  `capT <= maxBucketSpanNanos`.
- **Отсутствие переполнений в горячем пути** (при соблюдении бюджета):
  `floor = nowNs - capT >= -2^62` (нет underflow, nowNs ≥ 0);
  `avail = nowNs - eff <= capT <= 2^62` сверху; снизу `eff` может прийти от
  конкурента с более поздним `now`, тогда avail слегка отрицателен —
  корректный deny, переполнения нет (разница ограничена расхождением двух
  чтений часов); `need = n·T <= capT` (гарантируется ранним `n > capacity ->
  deny`); `eff + need <= nowNs` в ветке допуска. Всё в пределах int64. ✓
- **ABA невозможна структурно:** в ветке допуска `eff >= vt` и `need >= T >= 1`,
  значит `vt` строго монотонно растёт — прежнее значение не повторяется.
  Зафиксировать в doc-комментарии (у соседа ABA снимал GC, здесь — монотонность).
- **Квантование rate — задокументированная граница.** Фактически
  насаждаемая скорость — `1e9/T`, а не `rate`; относительная ошибка
  калибровки ≤ `0.5/(1e9/rate)` = `rate/2e9` и **не накапливается** (это
  фикс-ошибка периода, как у Rust `governor` и uber-go). Примеры: rate ≤ 2000
  → ошибка ≤ 1e-6; rate=1e6 → ≤ 5e-4; rate=6e8 → до 20% (1.67 нс округляется
  до 2). Дефолт бенчмарков rate=1e9 даёт T=1 **точно**. Честно описать в
  doc-комментарии конструктора; не «чинить».

### Вырожденные аргументы конструктора

Правило проекта (`TECHNICAL_PLAN.md §6 Этап 2/3`): паникует только тот
конструктор, чья математика **делит на параметр**. `NewAtomicTokenBucket`
делит `1e9 / rate` — значит валидирует и паникует, как `NewSlidingWindow`:

- `rate` NaN или `rate <= 0` → panic (деление бессмысленно; NaN проверять
  явно через `math.IsNaN` — сравнения с NaN ложны);
- `math.Round(1e9/rate) < 1` (т.е. `rate > 2e9`) → panic: период меньше 1 нс
  непредставим в целых наносекундах (`+Inf` в rate попадает сюда же: 1e9/Inf=0);
- `math.Round(1e9/rate) > maxBucketSpanNanos` (т.е. `rate < ~2.2e-10`) → panic;
- `capacity > 0 && int64(capacity) > maxBucketSpanNanos/T` → panic: `capT`
  молча переполнил бы int64 — ровно ловушка `tau overflow` из RESEARCH §2
  (rate=1/час, burst=3e6 переполняет), молчать нельзя.
- `capacity <= 0` — **принимается без паники** (консистентно с соседями:
  вырождение лишь ужесточает — ранний deny по `n > capacity` отбивает всё);
  `capT = 0`.

Тексты паник — информативные, с фактическими значениями (`fmt.Sprintf`).

## 2. Раскладка памяти и структура

```go
type AtomicTokenBucket struct {
    // vt is the only mutable word — the CAS target. Everything below the pad
    // is written once in the constructor and only read afterwards.
    vt atomic.Int64

    // 120 bytes of padding put the read-only fields at offset 128, two full
    // cache lines away from the CAS target. One line (64 B) is not enough:
    // the adjacent-line prefetcher pairs lines, so invalidations of vt's line
    // still hit line+1 — measured 831 ns with 64 B vs 228 with 128 B
    // (RESEARCH §1.3); same 128-byte figure as in sync/pool.go.
    _ [120]byte

    nanosPerToken int64 // T = round(1e9/rate): emission interval, ns per token
    capT          int64 // capacity * T: full-bucket span in ns (0 if capacity <= 0)
    capacity      int64 // int64(capacity): early-deny bound for AllowN
    origin        time.Time
    clk           Clock      // fallback time source
    sinceClk      SinceClock // non-nil iff clk supports the monotonic fast path
}

var _ Limiter = (*AtomicTokenBucket)(nil)
```

Go не гарантирует выравнивание кучи по кэш-линии, но гарантированные 128 байт
**разделения** дают ≥1 полную нетронутую линию между целью CAS и константами
при любом базовом адресе — этого и добивается приём. Смещение пришпилить
тестом (`unsafe.Offsetof`, см. тесты).

## 3. Форма CAS-петли — `AllowN`

Все четыре измеренных приёма: (1) константные поля — в локальные переменные
**до** петли; (2) падинг — в структуре; (3) часы читаются **один раз до**
петли и не перечитываются при ретрае; (4) состояние — одно слово, ноль
аллокаций.

```go
func (b *AtomicTokenBucket) Allow() bool { return b.AllowN(1) }

func (b *AtomicTokenBucket) AllowN(n int) bool {
    if n <= 0 {
        return true // port contract: degenerate batch, state untouched
    }
    if int64(n) > b.capacity {
        return false // can never fit; also guards need below from overflow
    }
    // Hoist every field the loop needs into locals BEFORE the loop: after a
    // failed CAS (a full memory barrier) the compiler must otherwise re-read
    // them from the invalidated cache line (RESEARCH §1.3, item 1).
    need := int64(n) * b.nanosPerToken
    capT := b.capT
    nowNs := b.elapsedNanos() // one clock read, before the loop (item 3)

    vt := b.vt.Load()
    for {
        next, ok := nextAtomicState(vt, nowNs, capT, need)
        if !ok {
            return false // deny without CAS — no state write, no retry
        }
        if b.vt.CompareAndSwap(vt, next) {
            return true
        }
        vt = b.vt.Load() // CAS lost — retry against fresh state, same nowNs
    }
}
```

- **`now` при ретрае не перечитывается** — это и есть выигрыш ×1.27
  (§1.4). Корректно: более старое `now` даёт меньше кредита — консервативно;
  вызов линеаризуется по своему `nowNs`.
- Конкурент мог установить `vt` больше нашего `nowNs` (его часы позже) —
  тогда avail < 0 и мы денаим; допустимо «во времени», как у соседа.
- **Backoff — нет**, голый retry (решение Этапа 4, довод тот же).
- Deny без CAS — сохраняется (см. §5 про наблюдаемость).

`elapsedNanos`:

```go
func (b *AtomicTokenBucket) elapsedNanos() int64 {
    if b.sinceClk != nil {
        return int64(b.sinceClk.Since(b.origin)) // monotonic-only read, ~73 ns
    }
    return b.clk.Now().Sub(b.origin).Nanoseconds() // degraded path, ~113 ns
}
```

## 4. Опциональный быстрый путь часов — `SinceClock`

В `limiter.go`, рядом с `Clock` (сам порт `Clock` не меняется):

```go
// SinceClock is an optional extension of Clock: a time source that can
// measure the elapsed duration since an earlier Now() reading without
// constructing a full time.Time. For the system clock this reads only the
// monotonic clock (~73 ns vs ~113 ns for Now, RESEARCH §1.5), which matters
// in a hot path that does nothing else of comparable cost.
//
// Contract: Since(t) must agree with Now().Sub(t), and for a t obtained from
// an earlier Now() it is non-negative and non-decreasing (follows from the
// Clock contract). Limiters that can exploit the fast path detect it with a
// type assertion at construction time and fall back to Now() otherwise.
type SinceClock interface {
    Clock
    Since(t time.Time) time.Duration
}
```

- `realClock` получает метод `func (realClock) Since(t time.Time)
  time.Duration { return time.Since(t) }` → `SystemClock` поддерживает
  быстрый путь автоматически. Добавить compile-time assertion
  `var _ SinceClock = realClock{}`.
- В конструкторе `AtomicTokenBucket`: `origin := clk.Now()`; затем
  `if sc, ok := clk.(SinceClock); ok { b.sinceClk = sc }` — **однократно**,
  не в горячем пути.
- Существующие четыре лимитера про `SinceClock` не знают — ни строчки в них
  не меняется (кроме call site эпсилона, что относится к решению 3).
- `fakeClock` в `clock_test.go` **не трогать** — он остаётся `Now`-only, и
  тесты atomic на нём проверяют деградацию. Для быстрого пути в
  `atomic_tokenbucket_test.go` завести обёртку:

```go
type fakeSinceClock struct{ fakeClock }

func (c *fakeSinceClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }
```

## 5. Соотношение с `TokenBucket` наблюдаемо

Зафиксировать в doc-комментарии `AllowN` (тон и объём — как у соседей):

**Совпадает:** контракт порта целиком (all-or-nothing, `n <= 0 -> true` без
касания состояния, старт полным, кламп по capacity, дробный refill копится);
вся сценарная таблица baseline проходит без изменений при rate с целым
периодом (1, 2, 10 — все тестовые).

**Расходится, и почему допустимо:**

1. **Deny без CAS — сохраняется** (как у `LockFreeTokenBucket`), но статус
   этого расхождения здесь **сильнее**: у соседа deny-без-CAS отличим от
   baseline «с точностью до порога admitEpsilon» (задокументированные ~7% на
   конфигурации, севшей на порог). Здесь порога нет: кредит каждый раз
   выводится из `(nowNs - vt)` заново целочисленно, кламп ассоциативен на
   целых точно, поэтому deny-с-CAS и deny-без-CAS **неотличимы вообще**, а
   не до эпсилона. Отдельно: RESEARCH §3.1 показал, что не персистить refill
   на отказе — поведение `x/time/rate`, т.е. эталона.
2. **Квантование скорости**: насаждается `1e9/round(1e9/rate)`, а не `rate`;
   ошибка ≤ `rate/2e9` относительных, не копится (см. §1). Baseline
   насаждает float64-точную rate — на не-целых периодах счётчики допусков
   за длинный прогон разойдутся в пределах этой ошибки.
3. **Эпсилона нет.** Дрейф float64 структурно отсутствует (вся арифметика
   горячего пути — int64), поэтому `admitEpsilon` в сравнении не участвует.
   Прецедент оформления — сосед: там эпсилон закреплён unit-тестом на чистую
   функцию; здесь его отсутствие закрепить сценарным steady-state тестом и
   пояснением в doc-комментарии.
4. **Конструктор паникует на вырожденной rate** (baseline принимает всё) —
   по правилу «делишь на параметр — валидируй», как `NewSlidingWindow`.

## 6. Относительный `admitEpsilon`

> **Постфактум-уточнение (ревью после реализации).** Формулировка ниже «снимает
> потолок 2^24» верна только для наполняемой стороны (`SlidingWindow`,
> `LeakyBucket`), где эпсилон прибавляется к тому же операнду
> (`capacity`/`limit`), по которому масштабируется. На спендящей стороне
> (`TokenBucket.tokenbucket.go:75`, `LockFreeTokenBucket.lockfree_tokenbucket.go:150`)
> эпсилон прибавляется к накопленному `tokens`, а масштабируется по `n` батча —
> дрейф в `tokens` ограничен `capacity`, а не `n`, поэтому при большой `capacity`
> и малом `n` (например `NewTokenBucket(1, 1<<25, clk)` с `n=1`) прежний потолок
> 2^24 остаётся. Это не регрессия и не ошибка реализации ниже — план это не
> предвидел явно, но и не запрещал; масштабирование по `max(n, capacity)`
> отдельно рассмотрено и отвергнуто (при `capacity=1<<30` слак стал бы
> наблюдаемым — 1.07 токена). См. итоговые doc-комментарии в `limiter.go`.

### Формула

В `limiter.go` константу заменить парой:

```go
// admitEpsilonRel is the relative slack of the admission comparison: the
// tolerance is admitEpsilonRel of the limit being compared against, not an
// absolute constant. <... переписать обоснование: дрейф ~1e-16 на единицу;
// абсолютная 1e-9 вырождалась в no-op при лимитах >= 2^24 (ULP(2^24)=3.7e-9);
// триггер «мерить объёмы» подтверждён внешне: IETF считает content-bytes,
// Azure APIM — килобайты, Cloudflare допускает score до миллиона
// (RESEARCH §2.2). Долг допуска ограничен 1e-9 ОТ ЛИМИТА за всё время жизни
// лимитера — относительная формулировка прежнего довода. ...>
const admitEpsilonRel = 1e-9

// admitEpsilon returns the admission slack for a comparison against limit.
func admitEpsilon(limit float64) float64 { return limit * admitEpsilonRel }
```

Имя функции = имя прежней константы, поэтому меняются ровно 4 call site и ни
один тест (проверено: идентификатор `admitEpsilon` в `_test.go` встречается
только в комментариях и сообщениях ошибок):

| Файл:строка | Было | Станет |
|---|---|---|
| `tokenbucket.go:75` | `b.tokens+admitEpsilon >= float64(n)` | `b.tokens+admitEpsilon(float64(n)) >= float64(n)` |
| `lockfree_tokenbucket.go:150` | `tokens+admitEpsilon >= float64(n)` | `tokens+admitEpsilon(float64(n)) >= float64(n)` |
| `leakybucket.go:96` | `b.level+float64(n) <= b.capacity+admitEpsilon` | `... <= b.capacity+admitEpsilon(b.capacity)` |
| `slidingwindow.go:102` | `estimate+float64(n) <= float64(w.limit)+admitEpsilon` | `... <= float64(w.limit)+admitEpsilon(float64(w.limit))` |

Масштаб — **тот операнд, с которым сравнивают** (n у спендящих, capacity/limit
у наполняемых): именно его ULP съедал абсолютную константу.

### Доказательство неизменности поведения на текущих лимитах

- Для `n = 1` (все `Allow()`-пути): `1.0 * 1e-9 == 1e-9` — умножение на 1.0
  точное, сравнение **бит-в-бит идентично** старому. Единственный тест,
  зажимающий эпсилон с двух сторон
  (`TestNextLockFreeState_AdmitEpsilonCoversFloatResidue`), работает с n=1:
  admit-кейс (дефицит 1 ULP < 1e-9) и deny-кейс (дефицит 1e-6 > 1e-9) —
  оба вердикта не меняются.
- Для `n > 1` и capacity/limit > 1 слак растёт с 1e-9 до `x*1e-9`. Вердикт
  мог бы измениться только у кейса с дефицитом в полосе `(1e-9, x*1e-9]`.
  Аудит тестов: табличные кейсы имеют целочисленные дефициты ≥ 1;
  steady-state тесты имеют остаток ~1e-16 (ниже обеих границ); других
  граничных дефицитов нет. Максимальные лимиты в тестах: capacity ≤ 8000
  (стресс) → слак ≤ 8e-6, на 5+ порядков ниже любого тестового дефицита.
- Семантика не ослаблена: слак ≤ 1e-9 **относительных** от лимита при любом
  масштабе (для лимита 2^24 — 0.017 единицы; наблюдателю неотличимо от
  конфигурации лимита, заданного с относительной точностью 1e-9).

### Чем закрепить (новый файл `internal/limiter/limiter_test.go`)

- `TestAdmitEpsilon_ExactAtUnitBatch` — `admitEpsilon(1) == 1e-9` **точно**
  (якорь обратной совместимости, сравнение через `==`).
- `TestAdmitEpsilon_LiftsFloat64Ceiling` — прямая регрессия снятого потолка:
  для `x = 1<<24` и `1<<30` проверить `x+admitEpsilon(x) > x` (со старой
  константой было `==` — no-op; это и был документированный отказ).
- `TestNextLockFreeState_AdmitEpsilonScalesWithBatch` — функциональная
  проверка через существующую чистую функцию (вызов из нового файла, пакет
  один): `nextLockFreeState(&lockFreeState{tokens: math.Nextafter(1<<24, 0),
  last: now}, now, 1, 1<<25, 1<<24)` обязан **допустить** (дефицит 1 ULP от
  2^24 = 2^-28; старый абсолютный эпсилон здесь молча денаил — тест на нём
  падает, что и доказывает смену поведения ровно в целевой точке); бракет с
  другой стороны: `tokens = (1<<24) * (1 - 1e-6)` (дефицит ≈16.8 ≫ слака
  0.017) обязан **денаить** — эпсилон не стал бесплатным токеном.

## 7. Тесты `atomic_tokenbucket_test.go` — полный список

Хелперы `step`/`allow` из `tokenbucket_test.go` и `fakeClock` из
`clock_test.go` переиспользовать; `fakeSinceClock` — завести здесь (§4).
Счётчики конкурентных тестов — **только `atomic.Int64` с `Add(1)` на каждый
допуск**; «локальный счётчик + один слив» ЗАПРЕЩЁН (SKILL §5, PR #7).

1. `TestAtomicTokenBucket_Scenarios` — зеркало сценарной таблицы
   `TestLockFreeTokenBucket_Scenarios` (все 8 кейсов работают на этой схеме —
   проверено выкладками §1), **прогнанная дважды через субтесты**: с
   `&fakeClock{}` (деградация к `Now()`) и с `&fakeSinceClock{}` (быстрый
   путь) — оба пути часов обязаны давать идентичные вердикты. Механика
   параметризации — на усмотрение исполнителя (например, фабрика
   `func() (Clock, func(time.Duration))`).
2. `TestAtomicTokenBucket_AllowNNonPositiveKeepsPendingCredit` — аналог
   `..._AllowNNonPositiveKeepsRefillTimestamp` соседа: allow → +0.5s →
   `AllowN(0)`, `AllowN(-1)` → true → +0.5s → allow true, extra false.
3. `TestAtomicTokenBucket_SteadyStateNonBinaryExactStep` — rate 1/с, шаг
   100 мс, 10000 шагов, **ровно** 1000 допусков. Scope-комментарий по образцу
   соседа: здесь тест закрепляет, что целочисленная схема не требует эпсилона
   вовсе (дрейфу негде копиться), а не проверяет эпсилон.
4. `TestNextAtomicState_Transitions` — table-driven по чистой функции:
   кламп floor'ом (старый vt, большой elapsed); допуск ровно на границе
   `avail == need`; deny при `avail == need-1`; монотонность (`next > vt` при
   ok); deny при отрицательном avail (vt от «более позднего» конкурента).
5. `TestAtomicTokenBucket_ConcurrentNoOverAdmit` — центральный стресс,
   паттерн SKILL §4 и зеркало соседского: замороженное время, 32 горутины ×
   500 вызовов, **точное равенство** `admitted == capacity`, две ёмкости
   (100 — путь deny; goroutines*callsPer/2 = 8000 — путь CAS). Гонять с
   `-race`, `-count>1`, `GOMAXPROCS>1`.
6. `TestAtomicTokenBucket_ConcurrentWithRefill` — конкурентный тест с
   движущимся временем, зеркало соседского: capacity=50, rate=100 (T=1e7
   точен), 20 тиков × 10 мс, точное равенство `admitted == 70`; те же три
   свойства драйвера (слив до тиков; воркеры до стопа; refill ≪ capacity).
7. `TestNewAtomicTokenBucket_Validation` — table-driven паники/непаники:
   panic на `rate=0`, `-1`, `NaN`, `+Inf`, `3e9` (период < 1 нс), `1e-10`
   (период переполняет int64); panic на `capacity` с переполнением `capT`
   (например rate=1.0/3600 → T=3.6e12, capacity=3_000_000 — ловушка
   governor из RESEARCH §2); **без** паники: `capacity=0` и `capacity=-1`
   (лимитер денаит любой n ≥ 1 — проверить).
8. `TestNewAtomicTokenBucket_RateQuantization` — white-box по полю
   `nanosPerToken`: rate=1e9 → 1; rate=3 → 333333333; rate=0.5 → 2e9;
   rate=1.5e9 → 1 (round(0.667)). Пришпиливает задокументированную
   калибровку.
9. `TestAtomicTokenBucket_ClockFastPathSelection` — white-box: конструктор с
   `&fakeSinceClock{}` даёт `sinceClk != nil`; с `&fakeClock{}` — nil; плюс
   runtime-проверка `_, ok := SystemClock.(SinceClock); ok == true` (прод
   реально получает быстрый путь).
10. `TestAtomicTokenBucket_FieldPaddingLayout` — white-box, `unsafe.Offsetof`:
    смещение `nanosPerToken` ≥ 128. Пришпиливает раскладку против будущего
    «наведения порядка в полях» (падинг — измеренная часть дизайна, а не
    мусор).

Doc-комментарии типа/конструктора/AllowN — по-английски, в тоне соседей:
обоснование одного слова и «время вместо токенов», четыре приёма раскладки
со ссылкой на измерения, непригодность нулевого значения (нет Clock → panic
на первом Allow), фраза «safe for concurrent use by multiple goroutines»,
монотонность vt как замена GC-довода про ABA, квантование rate, deny без CAS.

## 8. Бенчмарки

### `internal/limiter/limiter_bench_test.go` (существующие функции не трогать)

- `BenchmarkAtomicTokenBucket_Serial` / `BenchmarkAtomicTokenBucket_Parallel`
  — через существующие `benchSerial`/`benchParallel`, конструктор
  `NewAtomicTokenBucket(benchRate, benchCapacity, SystemClock)`. Проверка
  границ: `benchRate=1e9` → T=1 (точен); `benchCapacity=1<<30` → `capT ≈
  1.07e9 нс` — валиден; бакет стартует полным (2^30 токенов) и рефилится
  1e9/с при потреблении ~1e7/с — не пересыхает, путь допуска. `SystemClock`
  проходит быстрым путём (это и есть измеряемый прод-режим).
- **No-op базовая линия** (RESEARCH §1.5: одно чтение часов — 73 нс из ~92 нс
  всей операции; без базовой линии serial-числа интерпретируются неверно):

```go
// noopLimiter is the measurement floor: interface dispatch plus loop only.
type noopLimiter struct{}

func (noopLimiter) Allow() bool      { return true }
func (noopLimiter) AllowN(int) bool  { return true }

func BenchmarkBaseline_NoopLimiter_Serial(b *testing.B)   { benchSerial(b, noopLimiter{}) }
func BenchmarkBaseline_NoopLimiter_Parallel(b *testing.B) { benchParallel(b, noopLimiter{}) }
```

- Две базовые линии часов (стоки — package-level переменные, чтобы компилятор
  не выбросил вызов): `BenchmarkBaseline_ClockNow` (`SystemClock.Now()`) и
  `BenchmarkBaseline_ClockSince` (`SystemClock.(SinceClock).Since(base)`, base
  взят до `ResetTimer`). Их разность — цена, которую снимает `SinceClock`;
  doc-комментарий блока баз должен прямо говорить, как ими пользоваться
  (вычитать пол из serial-чисел, не из parallel — там доминирует contention,
  RESEARCH §1.5).

### `cmd/bench`

- `main.go`: `const algoAtomic = "atomic"`; `allAlgos = [...token, sliding,
  leaky, lockfree, atomic]` (тесты сравнивают с `allAlgos` символически —
  расширение их не ломает); case в `makeLimiter` →
  `limiter.NewAtomicTokenBucket(cfg.rate, cfg.capacity, limiter.SystemClock)`;
  обновить help-текст `-algo` и сообщение `unknown -algo`.
- **Валидация в `parseConfig`** (по образцу sliding-блока), чтобы CLI-ввод
  не долетал паникой из конструктора: для `algoAtomic` (в т.ч. через `all`)
  требовать `rate <= 2e9` и
  `math.Round(1e9/rate) * float64(capacity) <= float64(int64(1)<<62)`;
  иначе — `error` с объяснением. Комментарий-перекрёсток в обе стороны
  (parseConfig ↔ конструктор), потому что границы продублированы и могут
  разъехаться.
- `main_test.go` — **добавить, не править**: строку
  `{algoAtomic, (*limiter.AtomicTokenBucket)(nil)}` в таблицу makeLimiter;
  error-кейсы parseConfig (`-algo=atomic -rate=3e9`; переполнение
  capacity·T); существующие проверки (`DeepEqual` с `allAlgos`, циклы по
  `allAlgos`) подхватят пятый алгоритм сами.
- Предупреждения `deniedWarning`/`percentileNote` работают для atomic без
  изменений (deny-путь у него тоже дешевле — warning уже покрывает).

## 9. Документация

- `README.md`: перегенерировать блок микробенчмарков (`go test -bench=.
  -benchmem ./internal/limiter/`) и таблицу харнесса (`go run ./cmd/bench
  -format=markdown`) **одной сборкой на этой машине**; дописать в «Что дал
  CAS и чего он стоил» продолжение: наивный CAS проигрывает ~3× (результат
  Этапа 5, остаётся как точка сравнения), CAS с раскладкой (одно слово,
  часы и константы до петли, падинг 128 Б) обгоняет mutex; **обязательная
  оговорка**: числа lock-free из разных сборок несравнимы (один исходник —
  591…976 нс в шести бинарях), поэтому все сравнения в README — из одного
  прогона одного бинаря, кратные разрывы значимы, разницы <10% — нет
  (RESEARCH §1.3, §1.5).
- `CLAUDE.md`: строка в таблицу структуры (`atomic_tokenbucket.go`), правка
  абзаца про lock-free («две lock-free реализации: наивный CAS по снапшоту и
  CAS по одному слову»), упоминание относительного `admitEpsilon` вместо
  абсолютного, `-algo` список.
- `TECHNICAL_PLAN.md` (раздел «Этап 6») и `POST_MVP_PLAN.md §4а` (техдолг
  «вклады не разделены» закрыт исследованием) — **итоговые формулировки
  пишет основная сессия на шаге 7**; исполнителю их не трогать, только
  оставить в отчёте список расхождений, которые он заметил.

## 10. Риски и что НЕ делать

- **НЕ «оптимизировать» существующие четыре лимитера** — ни выноса часов, ни
  падинга, ничего: их числа — задокументированный результат Этапа 5 и
  контрольная точка «наивный CAS». Единственная разрешённая правка — call
  site `admitEpsilon` (§6).
- **НЕ менять порт `Clock`** и сигнатуру `Now()`. `SinceClock` — только
  добавление.
- **НЕ вписывать в код/доки конкретные «194.6 нс» как обещание** — число
  сборко- и машинно-зависимое. Формулировки — качественные («обгоняет
  mutex под конкуренцией на этой машине, см. таблицу»), все числа — из
  одного локального прогона.
- **НЕ добавлять эпсилон в `AtomicTokenBucket` «для единообразия»** — в
  целочисленной схеме он не поглощает дрейф (его нет), а раздаёт реальный
  кредит.
- **НЕ ограничивать число ретраев CAS** (unkey-паттерн из RESEARCH §3.2) и
  не добавлять backoff — решение Этапа 4 сохраняется, тема отдельная.
- **НЕ использовать `fakeClock` в бенчмарках** (мьютекс внутри Now
  сериализует всё — правило уже в doc-комментарии bench-файла).
- Риск: полоса относительного эпсилона `(1e-9, x*1e-9]` теоретически может
  задеть незамеченный тест — аудит §6 её очистил, но финальный прогон всей
  сборки обязателен; если какой-то существующий тест упал — **не править его
  ожидания**, а вернуться к формуле (это сигнал, что масштаб выбран не тем
  операндом).
- Риск: дублирование границ конструктора в `parseConfig` — принято осознанно
  (панике из CLI не место), закреплено перекрёстными комментариями.
- Риск: квантование rate при больших значениях (до 20% на rate=6e8) —
  документируется, не чинится; прецедент — governor/uber-go.
- Git-команды не выполнять; коммитит основная сессия.

## Критерий готовности

- `go build ./... && go vet ./... && go test -race ./...` — чисто;
  `gofmt -l .` — пусто.
- Все существующие тесты зелёные **без правки ожиданий**; diff существующих
  `_test.go` файлов пуст.
- Стресс-тест стабильно зелёный: `GOMAXPROCS=4 go test -race -count=5 -run
  'AtomicTokenBucket' ./internal/limiter/`.
- `go test -bench=. -benchmem ./internal/limiter/` содержит строки
  `AtomicTokenBucket` и `Baseline_*`; `allocs/op` у atomic — **0** в обоих
  профилях (ноль аллокаций — проверяемое свойство схемы, не пожелание).
- `go run ./cmd/bench` печатает пять строк; `go run ./cmd/bench -algo=atomic`
  работает; `-algo=atomic -rate=3e9` даёт ошибку, не панику.
- Compile-time assertions на месте (`Limiter`, `SinceClock`); doc-комментарии
  по-английски в тоне соседей.

## Порядок работ

1. `limiter.go`: `SinceClock` + `realClock.Since` + assertion; относительный
   `admitEpsilon` + 4 call site; прогнать `go test ./...` — существующие
   тесты обязаны быть зелёными уже здесь.
2. `limiter_test.go`: тесты эпсилона (§6).
3. `atomic_tokenbucket.go` (§1–§5), затем `atomic_tokenbucket_test.go` (§7).
4. Бенчмарки и `cmd/bench` (§8), включая тестовые добавления.
5. Полный гейт, стресс с `-count=5`, локальный прогон бенчей одной сборкой,
   README/CLAUDE.md (§9).
