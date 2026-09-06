# C2 — Go lock-free вглубь

> Стенд: Linux amd64, `11th Gen Intel Core i7-11700 @ 2.50GHz`, 8 физических /
> 16 логических ядер, 1 сокет, L1d 384 KiB (8×48K), L2 4 MiB (8×512K),
> L3 16 MiB (общий), `coherency_line_size = 64`, Go 1.23.4, `clocksource=tsc`.
> Все свои числа — медианы `go test -bench ... -count=5`, сырой вывод в разделе
> «Собственные замеры».

Отправная точка — то, что уже измерено основной сессией (не переоткрывается):

| Реализация | Serial | Parallel-16 | alloc/op |
|---|---|---|---|
| mutex token bucket | 222.5 нс | **478.6 нс** | 0 |
| CAS + `atomic.Pointer` на снапшот | 281.1 нс | 1473 нс | 1 / 8 B |
| GCRA в одном `atomic.Int64`, часы внутри петли | 101.6 нс | 1016 нс | 0 |
| GCRA в одном `atomic.Int64`, часы до петли | **92.5 нс** | 802.7 нс | 0 |

Контроль: пустая петля 1.2 нс, одно `time.Since` — 73.1 нс. Свип
`GOMAXPROCS` для GCRA in-loop: P=1 110 нс, P=2 279, P=4 715, P=8 1198.
Деградация P1→P8: mutex 1.95×, CAS+Pointer 3.8×, GCRA 10.9×.

**Сверка с моими числами.** Мои реализации — отдельные, в своём модуле, поэтому
абсолютные значения не обязаны совпадать; важно, что совпадают отношения.

- Мьютексный вариант у меня стабилен: **345–380 нс** во всех шести сборках
  (против 478.6 в отправной таблице — я читаю часы вне критической секции, что
  для мьютекса выгоднее).
- Наивный GCRA у меня дал **591…976 нс в разных сборках одного и того же
  исходника** при разбросе *внутри* прогона меньше 5%. Числа основной сессии
  (802.7 и 1016) попадают в этот диапазон.

Сам этот разброс — отдельная находка (раздел «Прямой ответ», подраздел про
разброс между сборками). Поэтому все выводы ниже опираются на сравнения
**внутри одного бинаря и одного прогона**; там, где числа взяты из разных
прогонов, это оговорено явно.

---

## Коротко

1. **Да, `sync.Mutex` обгоняется — двумя независимыми приёмами, оба чистый
   stdlib, оба начиная с ~4 ядер.** (а) вынести loop-invariant поля из
   CAS-петли в локальные переменные; (б) `runtime.Gosched()` после **каждого**
   проигранного CAS. Вместе — 182.9 нс против 360.8 у мьютекса при 16
   горутинах на одном разделяемом лимитере (1.97×), ноль аллокаций, ноль
   линкнеймов, семантика не тронута.
2. **Главный виновник — не CAS, а перечитывание констант внутри петли.**
   Компилятор обязан перезагружать `interval`/`burst` после каждого
   `LOCK CMPXCHG` (это барьер), а лежат они на той же кэш-линии, что и атомик,
   которую чужой CAS только что инвалидировал. Одна строка `iv, bu :=
   l.interval, l.burst` перед петлёй даёт **591 → 190 нс**.
3. **Падинга в 64 байта не хватает — нужно 128.** Раскладка
   «prepadding + атомик + postpadding» с шагом 64 сделала **хуже** (831 нс),
   с шагом 128 — помогла (228 нс). Ровно тот компромисс, который прописан в
   `sync/pool.go:74-77`. При этом `internal/cpu.CacheLinePadSize` на amd64
   равен 64.
4. **Наивная CAS-структура — лотерея по сборкам.** Один и тот же исходник дал
   591…976 нс в шести разных бинарях при разбросе внутри прогона <5%. Сравнивать
   lock-free варианты можно только внутри одного бинаря.
5. **Шардирование выигрывает от двух ядер и масштабируется линейно** (до 33×
   при P=16 на одинаковом теле), но цена жёсткая и измеренная:
   **утилизация ≈ (число активных шардов) / k**. При k=16 и одном активном
   источнике лимитер на 100 000 rps пропускает 6 300.
6. **`sync.Mutex` дорог не механизмом, а гарантиями.** Он выигрывает медиану
   (p50 383 нс против 500), но проигрывает хвост (p99 214 мкс против 80.7) и
   пропускную способность. Разница — цена FIFO и starvation mode, которых у
   lock-free варианта нет вовсе.

---

## 1. Почему `sync.Mutex` в Go так хорош под contention

Ключ к результату «mutex быстрее голого CAS-цикла» в том, что `sync.Mutex` —
это **не** «спин-лок с парковкой». Это гибрид из четырёх механизмов, каждый из
которых по отдельности гасит именно ту патологию, от которой страдает наивный
CAS-цикл. Все ссылки ниже — на `$GOROOT/src` установленного Go 1.23.4;
онлайн-зеркало: <https://github.com/golang/go/blob/go1.23.4/src/sync/mutex.go>.

### 1.1. Структура: 8 байт состояния + семафор

```go
type Mutex struct {
	state int32
	sema  uint32
}
```
`src/sync/mutex.go:36-39`. Биты `state`:
`mutexLocked = 1`, `mutexWoken = 2`, `mutexStarving = 4`,
остальные 29 бит — счётчик ожидающих (`mutexWaiterShift = 3`),
`src/sync/mutex.go:47-51`.

Быстрый путь `Lock` — **один** `CompareAndSwapInt32(&m.state, 0, mutexLocked)`
(`mutex.go:85`); всё остальное вынесено в `lockSlow`, «outlined so that the fast
path can be inlined» (`mutex.go:91`). Быстрый путь `Unlock` — один
`atomic.AddInt32(&m.state, -mutexLocked)` (`mutex.go:221`). То есть в
неконкурентном случае мьютекс стоит ровно два атомарных RMW — столько же, сколько
один виток «прочитал–посчитал–CAS» в lock-free петле. **Мьютекс не дороже CAS в
uncontended-случае**; вся разница проявляется под нагрузкой.

### 1.2. Адаптивный активный спин: `runtime_canSpin` / `runtime_doSpin`

`lockSlow` перед тем, как парковаться, крутится в спине:

```go
if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
	...
	runtime_doSpin()
	iter++
	old = m.state
	continue
}
```
`src/sync/mutex.go:128-140`.

`runtime_canSpin` — это `sync_runtime_canSpin` в
`src/runtime/proc.go:7149-7162` (подключён через `//go:linkname`, там же
`proc.go:7147`):

```go
if i >= active_spin || ncpu <= 1 || gomaxprocs <= sched.npidle.Load()+sched.nmspinning.Load()+1 {
	return false
}
if p := getg().m.p.ptr(); !runqempty(p) {
	return false
}
return true
```

Три условия отказа от спина, и все три содержательные:

1. `i >= active_spin` — не больше **4** витков (`active_spin = 4`,
   `src/runtime/lock_futex.go:30`). Спин строго ограничен: наивная CAS-петля
   крутится сколько потребуется, мьютекс — максимум 4 раза.
2. `ncpu <= 1 || gomaxprocs <= npidle + nmspinning + 1` — спинить бессмысленно,
   если больше некому бежать: держатель лока может быть не на CPU вовсе.
3. `!runqempty(p)` — если у текущего P есть своя работа в локальной очереди,
   спин — прямой убыток: лучше отдать P другой горутине.

`runtime_doSpin` = `procyield(active_spin_cnt)`, `src/runtime/proc.go:7176-7178`,
где `active_spin_cnt = 30` (`lock_futex.go:31`), а `procyield` на amd64 —
буквально цикл из инструкций `PAUSE`:

```asm
TEXT runtime·procyield(SB),NOSPLIT,$0-0
	MOVL	cycles+0(FP), AX
again:
	PAUSE
	SUBL	$1, AX
	JNZ	again
	RET
```
`src/runtime/asm_amd64.s:803-809`.

Итого верхняя граница спина — 4 × 30 = **120 `PAUSE`**, после чего горутина
уходит спать. На Skylake+ (в т.ч. Rocket Lake, наш i7-11700) `PAUSE` — около
**140 циклов** (Intel Optimization Manual: задержка `PAUSE` выросла с ~10 циклов
в pre-Skylake до ~140 в Skylake-микроархитектуре; см. Intel SDM/Opt. Manual,
раздел про `PAUSE`, и обсуждение в
<https://www.agner.org/optimize/instruction_tables.pdf>). То есть даже
максимальный спин — это микросекунды, не «сколько влезет».

Важный нюанс в комментарии: спин помечает `mutexWoken`
(`mutex.go:132-135`), чтобы `Unlock` **не будил** ещё одну горутину — то есть
активный спин не только ждёт, но и подавляет лишние пробуждения.

### 1.3. Парковка через семафор вместо кручения

Если спин не помог, горутина инкрементирует счётчик ожидающих и вызывает
`runtime_SemacquireMutex(&m.sema, queueLifo, 1)` (`mutex.go:173`) →
`sync_runtime_SemacquireMutex` (`src/runtime/sema.go:94`) → `semacquire1`
(`sema.go:132`). Там: `acquireSudog()`, постановка в очередь `root.queue(...)`
и `goparkunlock(...)` — `sema.go:177-178`. Горутина **снимается с P**, P уходит
исполнять чужую работу.

Это и есть главное отличие от голой CAS-петли: проигравший CAS в lock-free
коде продолжает жечь такты и, что важнее, **продолжает дёргать общую
кэш-линию**. Проигравший гонку за мьютекс — засыпает и линию больше не трогает.
Под 16 горутинами на одной линии это разница между 16 постоянными
запросами на эксклюзивное владение и 1–2.

Очереди ожидания живут не в самом мьютексе, а в глобальной хэш-таблице
семафоров: `semtable` из 251 корня (`sema.go:46-54`), **и каждый корень
выровнен на кэш-линию**:

```go
type semTable [semTabSize]struct {
	root semaRoot
	pad  [cpu.CacheLinePadSize - unsafe.Sizeof(semaRoot{})]byte
}
```
`src/runtime/sema.go:51-54`. Это готовый пример из stdlib, что борьба с false
sharing в Go — обычная практика (см. §3).

### 1.4. Два режима и FIFO голодающих

`sync.Mutex` работает в двух режимах, документированных прямо в исходнике
(`src/sync/mutex.go:53-77`):

- **normal mode** — очередь FIFO, но разбуженный ожидающий **не владеет**
  мьютексом, он конкурирует с новоприбывшими; те в выигрышном положении, так
  как уже на CPU. Проигравший встаёт **в голову** очереди (`queueLifo :=
  waitStartTime != 0`, `mutex.go:169`);
- **starvation mode** — включается, когда ожидающий не смог взять лок дольше
  `starvationThresholdNs = 1e6` (1 мс, `mutex.go:77`, проверка `mutex.go:174`).
  В этом режиме владение **передаётся напрямую** от разблокирующего к голове
  очереди, новоприбывшие даже не пробуют взять «свободный» мьютекс и **не
  спинят** (`mutex.go:128`: спин запрещён, если взведён `mutexStarving`).

Прямая передача видна в `unlockSlow`: `runtime_Semrelease(&m.sema, true, 1)`
(`mutex.go:259`), и дальше в `semrelease1` при `handoff` выставляется
`s.ticket = 1` и делается `goyield()` — «Direct G handoff», разбуженная
горутина **наследует квант времени** разблокирующего
(`src/runtime/sema.go:246-268`, там же ссылка на issue 33747:
<https://go.dev/issue/33747>).

Комментарий в исходнике честно называет цену: «Normal mode has considerably
better performance… Starvation mode is important to prevent pathological cases
of tail latency» (`mutex.go:74-76`) и «Starvation mode is so inefficient, that
two goroutines can go lock-step infinitely once they switch mutex to starvation
mode» (`mutex.go:188-190`).

### 1.5. Почему это гасит ping-pong лучше голой петли CAS

Собирая: наивная CAS-петля под N ядрами имеет три патологии, и мьютекс лечит
все три.

| Патология CAS-петли | Что делает `sync.Mutex` |
|---|---|
| Все N ядер непрерывно шлют RFO (Request For Ownership) на одну линию → линия мигрирует между L1 ~N раз за операцию | Не более 4 витков спина, потом парковка: линию трогают 1–2 ядра |
| Проигравший CAS **выбрасывает** проделанную работу (пересчёт `tokens`, `time.Since`) и делает её заново | Победитель гонки делает работу **ровно один раз** под локом; проигравшие не считают ничего |
| Нет справедливости: возможен livelock/старвейшн одного участника | FIFO + starvation mode с прямой передачей владения |

Дополнительный, часто упускаемый пункт: у CAS-петли работа внутри витка
**конкурирует сама с собой**. Чем дольше виток (а `time.Since` — 73 нс, см.
измерения ниже), тем выше вероятность, что за это время линию угнали, и тем
больше витков. Это положительная обратная связь: рост contention удлиняет
эффективную критическую секцию, что увеличивает contention. Мьютекс эту петлю
разрывает, потому что «критическая секция» у него — это время удержания лока,
и оно не растёт от того, что снаружи ждут ещё 15 горутин.

Именно этим объясняется наблюдение из отправной таблицы: деградация P1→P8 у
mutex **1.95×**, у GCRA-CAS **10.9×**. Мьютекс масштабируется почти линейно по
«одна операция за раз», CAS-петля — сверхлинейно вырождается.

---

## 2. Стоимость атомиков и когерентность кэша

### 2.1. Что физически делает `LOCK CMPXCHG`

`atomic.CompareAndSwap*` на amd64 компилируется в `LOCK CMPXCHGQ`. Современные
x86 не блокируют шину (это делалось до P6): вместо этого ядро **берёт кэш-линию
в эксклюзивное владение** и удерживает её на время операции — «cache locking».
Инструкция при этом ещё и полный барьер памяти (`LOCK`-префикс имеет семантику
`mfence`).

Стоимость поэтому определяется не «латентностью инструкции», а состоянием
линии в протоколе когерентности. На Intel это MESIF (MESI + состояние
Forward), у AMD — MOESI:

| Состояние линии в L1 инициатора | Что нужно сделать | Порядок стоимости |
|---|---|---|
| **M**odified / **E**xclusive | ничего, линия уже наша | десятки циклов (собственно RMW) |
| **S**hared | послать RFO (Request For Ownership), дождаться инвалидации у всех остальных | сотни циклов |
| **I**nvalid (линия в M у соседа) | RFO + перенос линии из чужого L1/L2 через L3 | сотни циклов |

Отсюда типовые числа, которые встречаются в литературе:

- uncontended `LOCK CMPXCHG` — **≈20 циклов**, contended на многоядерном
  (не многосокетном) — **≈200 циклов**, то есть порядок разницы; Joe Duffy,
  «Some performance implications of CAS operations»,
  <https://joeduffyblog.com/2009/01/08/some-performance-implications-of-cas-operations/>;
- «стоимость инструкции определяется поведением кэша и шины, а не латентностью
  самой инструкции» — там же и в тредах Intel Community
  (<https://community.intel.com/t5/Intel-Moderncode-for-Parallel/lock-xchg-performance/td-p/917107>).

Свои измерения (стенд из шапки, `-count=5`, медианы; **сырой вывод — в разделе
«Собственные замеры»**):

| Операция | 1 горутина | 16 горутин (`RunParallel`) | отношение |
|---|---|---|---|
| `atomic.Int64.Load` → `MOVQ` | 1.31 нс | 0.23 нс/оп (агрегат) | масштабируется линейно |
| `c++` (неатомарный) | 1.21 нс | — | — |
| `atomic.Int64.Add` → `LOCK XADDQ` | 19.58 нс | 43.46 нс | 2.2× |
| `atomic.Int64.Store` → `XCHGQ` | 20.03 нс | — | — |
| `CAS` без ретрая (Load+`LOCK CMPXCHGQ`) | 36.90 нс | 72.14 нс | 2.0× |
| **`CAS` с ретраем** до успеха | 36.90 нс | **218.0 нс** | **5.9×** |
| `sync.Mutex` + `c++` | — | 284.0 нс | — |

(Кодогенерация проверена на стенде через `go build -gcflags=-S`: `Load` →
`MOVQ`, `Store` → `XCHGQ`, `Add` → `LOCK XADDQ`, `CompareAndSwap` →
`LOCK CMPXCHGQ`.)

Три вывода, которые стоят всей таблицы:

1. **Читать разделяемую линию бесплатно.** `Load` под 16 горутинами даёт
   0.23 нс/оп — линия живёт в S-состоянии во всех L1 одновременно, трафика
   когерентности нет вообще. Это ключ к TTAS и к тому, почему шардирование
   на чтение не нужно.
2. **Пишущий доступ к разделяемой линии не масштабируется, но и не
   катастрофичен сам по себе**: `Add` 19.6 → 43.5 нс, CAS-без-ретрая
   36.9 → 72.1 нс. Это «всего» 2×. Одна операция записи в линию под 16 ядрами
   стоит примерно вдвое дороже, чем под одним.
3. **Катастрофу делает ретрай.** Тот же CAS, но в петле до успеха, — 218 нс
   против 72 нс без ретрая, то есть **≈3 бесполезных витка на каждый
   полезный**. И это на теле петли из двух инструкций; в лимитере тело
   длиннее, и коэффициент растёт.

Оговорка про абсолютные значения: губернатор частоты на стенде — `powersave`
(`intel_pstate`), в простое 1.1 ГГц, под нагрузкой машина бустится. Поэтому
пересчёт нс→циклы ненадёжен; **сравнивать между собой строки одной таблицы
можно, переводить в циклы — нет**. Также на CPU включён полный набор
митигаций (Enhanced IBRS, MDS clear и т.д.), что объясняет, почему
uncontended `Add` = 19.6 нс выглядит дороже «учебных» 20 циклов.

### 2.2. Почему uncontended атомик всё же не бесплатен

19.6 нс на неконкурентный `Add` против 1.2 нс на обычный `c++` — это 16×.
Причина не в когерентности (линия уже в M), а в том, что `LOCK`-префикс —
барьер: он не даёт store buffer'у оставить запись «в полёте», требует
дренажа буфера и запрещает переупорядочивание вокруг себя. Внеочередное
исполнение вокруг такой инструкции резко сужается.

Практическое следствие для лимитера: **один атомарный RMW на вызов — это
пол/уже потраченного бюджета**, если весь вызов должен уложиться в 100 нс.
Второй атомарный RMW (или один ретрай) удваивает цену.

---

## 3. False sharing и padding

### 3.1. Как это выглядит в Go

Канонический способ — `cpu.CacheLinePad` из `golang.org/x/sys/cpu`:
<https://pkg.go.dev/golang.org/x/sys/cpu#CacheLinePad>. Определение —
`type CacheLinePad struct{ _ [cacheLineSize]byte }`.

**В stdlib аналога нет**, и это стоит зафиксировать точно. Тип
`cpu.CacheLinePad` существует в `internal/cpu`
(`$GOROOT/src/internal/cpu/cpu.go:16-22`):

```go
// CacheLinePad is used to pad structs to avoid false sharing.
type CacheLinePad struct{ _ [CacheLinePadSize]byte }

// CacheLineSize is the CPU's assumed cache line size.
// There is currently no runtime detection of the real cache line size
// so we use the constant per GOARCH CacheLinePadSize as an approximation.
var CacheLineSize uintptr = CacheLinePadSize
```

— но пакет `internal/cpu` **недоступен извне stdlib**: правило `internal/`
запрещает импорт компилятором. Проверено на стенде: попытка импорта из
стороннего модуля не собирается. Из stdlib он используется широко:
`runtime/sema.go:53` (падинг корней таблицы семафоров),
`runtime/mheap.go:125,201`, `runtime/mgc.go:320,322`, `runtime/mgcpacer.go:366`,
`runtime/stack.go:152`, `runtime/tracemap.go:28,30`.

Внутри `sync` (тоже stdlib, но пакет не может импортировать `internal/cpu`
без цикла) падинг написан руками — `$GOROOT/src/sync/pool.go:72-78`:

```go
type poolLocal struct {
	poolLocalInternal

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}
```

Обратите внимание: **128, а не 64**. Причина — аппаратный adjacent-line
prefetch у Intel, который тянет линии парами; выравнивание на 64 не всегда
спасает. Тот же компромисс явно проговорён в `xsync`
(<https://github.com/puzpuzpuz/xsync/blob/main/util.go>):

```go
// cacheLineSize is used in paddings to prevent false sharing;
// 64B are used instead of 128B as a compromise between
// memory footprint and performance; 128B usage may give ~30%
// improvement on NUMA machines.
cacheLineSize = 64
```

Итого, что доступно чистому Go-проекту без зависимостей: **написать падинг
руками**, `_ [64]byte` / `_ [64 - unsafe.Sizeof(...)%64]byte`. Это и делают
все библиотеки.

### 3.2. Как это делает `uber-go/ratelimit`

`limiter_atomic_int64.go`
(<https://github.com/uber-go/ratelimit/blob/main/limiter_atomic_int64.go>):
структура зажата падингом **с обеих сторон**:

```go
type atomicInt64Limiter struct {
	//lint:ignore U1000 Padding is unused but it is crucial to maintain performance
	// of this rate limiter in case of collocation with other frequently accessed memory.
	prepadding [64]byte // cache line size = 64; created to avoid false sharing.
	state      int64    // unix nanoseconds of the next permissions issue.
	//lint:ignore U1000 like prepadding.
	postpadding [56]byte // cache line size - state pointer size = 64 - 8; created to avoid false sharing.
	...
}
```

`prepadding` нужен потому, что защита нужна не только от полей **после**
`state`, но и от чужой памяти **до** структуры: аллокатор может положить
`atomicInt64Limiter` вплотную к другому горячему объекту. Это ровно тот
случай, который в комментарии назван «collocation with other frequently
accessed memory».

Сам алгоритм у uber — это GCRA (`state` = «unix-наносекунды следующей
выдачи разрешения») в **одном** `int64` с CAS-петлёй, то есть в точности та
конструкция, которую мы здесь и меряем.

### 3.3. Свой замер эффекта

16 горутин, каждая инкрементирует **свою** ячейку в массиве из 64 ячеек
(логической конкуренции нет вообще, только физическая):

| Раскладка | нс/оп (16 горутин) | против выровненного |
|---|---|---|
| `struct{ v atomic.Int64 }` — плотно, 8 байт на ячейку | **34.44** | 13.7× хуже |
| `struct{ v atomic.Int64; _ [56]byte }` — 64 байта | **2.52** | базовая |
| `struct{ v atomic.Int64; _ [120]byte }` — 128 байт | 2.63 | 1.04× |

**13.7× разницы на пустом месте** — это и есть цена false sharing на этой машине.
128-байтовое выравнивание здесь ничего не добавило (в пределах шума даже чуть
хуже) — но это одна конкретная машина (одна NUMA-нода, Rocket Lake); на
многосокетных системах, как отмечает xsync, выигрыш может быть ~30%. **Не
проверено** на многосокетном железе.

**Важная оговорка, которая всплыла позже.** Этот замер — «каждый поток пишет в
свою ячейку», то есть чистый паттерн write–write. Там 64 байт достаточно. Но
в паттерне «горячая атомарная запись + рядом лежащее чтение констант»,
который и встречается в CAS-петле лимитера, **64 байт оказалось мало, а 128
хватило** (831 против 228 нс). Подробный разбор — в разделе «Прямой ответ»,
подраздел про падинг. То есть «выровняй на кэш-линию» — недостаточно точная
рекомендация: правильный шаг зависит от паттерна доступа, и `sync/pool.go`
не зря использует 128.

Отдельно: 2.52 нс/оп на выровненном варианте против 19.6 нс на uncontended
`Add` в одной горутине — потому что 2.52 это агрегат по 16 потокам
(≈40 нс латентности каждого / 16). То есть выровненные счётчики
масштабируются почти линейно, невыровненные — не масштабируются вообще.

---

## 4. Шардирование

### 4.1. Три способа выбрать шард из обычного Go

**(а) По ключу.** Если у лимитера есть естественный ключ (user id, IP, tenant),
шард — это `hash(key) % k`. Этот случай к нашему вопросу отношения не имеет:
там нет «одного разделяемого лимитера», там их k независимых, и вопрос
масштабирования решается сам собой. Разбирать интересно только случай **одного
логического лимита**, разложенного на k физических шардов.

**(б) Per-P через `runtime_procPin` / `procUnpin`.** Именно так делает
`sync.Pool` (`$GOROOT/src/sync/pool.go:202-221`):

```go
pid := runtime_procPin()
s := runtime_LoadAcquintptr(&p.localSize)
l := p.local
if uintptr(pid) < s {
	return indexLocal(l, pid), pid
}
```

`procPin` возвращает индекс текущего P и **запрещает вытеснение** (`m.locks++`),
`procUnpin` отпускает. Пока горутина «пришпилена», она гарантированно не
мигрирует на другой P, значит доступ к `shards[pid]` эксклюзивен по построению.

Почему это `//go:linkname`-хак: `runtime.procPin` — приватная функция рантайма,
в `sync` она попадает **push-линкнеймом** `//go:linkname sync_runtime_procPin
sync.runtime_procPin` (`$GOROOT/src/runtime/proc.go:7126-7133`). Обычный
пользовательский пакет может дотянуться до неё **pull-линкнеймом**:

```go
import _ "unsafe"

//go:linkname runtime_procPin runtime.procPin
func runtime_procPin() int

//go:linkname runtime_procUnpin runtime.procUnpin
func runtime_procUnpin()
```

— плюс пустой `.s`-файл в пакете, чтобы компилятор разрешил объявление функции
без тела. **Проверено на стенде: в Go 1.23.4 это собирается, линкуется и
работает** (`procPin` вернул номер P, см. `procpin.go` / `procpin_test.go` в
приложении).

Работает оно потому, что Go 1.23 ввёл `-checklinkname=1`, который запрещает
pull-линкнеймы к неотмеченным символам, но `runtime.procPin` **отмечен
явно** — и комментарий рядом не оставляет иллюзий
(`$GOROOT/src/runtime/proc.go:7074-7085`):

```go
// procPin should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/gopkg
//   - github.com/choleraehyq/pid
//   - github.com/songzhibin97/gkit
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname procPin
```

То есть Go team знает про этот хак, терпит его ради совместимости и называет
его пользователей «залом позора». Ссылка: <https://go.dev/issue/67401>.
**Легального способа получить номер P из обычного Go нет.**

**(в) `sync.Pool` как готовый per-P механизм — легальный обходной путь.**
Именно им пользуется `xsync.Counter`
(<https://github.com/puzpuzpuz/xsync/blob/main/counter.go>):

```go
// pool for P tokens
var ptokenPool sync.Pool

// a P token is used to point at the current OS thread (P)
// on which the goroutine is run; exact identity of the thread,
// as well as P migration tolerance, is not important since
// it's used to as a best effort mechanism for assigning
// concurrent operations (goroutines) to different stripes of
// the counter
type ptoken struct {
	idx uint32
	_   [cacheLineSize - 4]byte // padding
}
```

`Pool.Get()` почти всегда отдаёт объект, положенный **тем же P**, поэтому
`ptoken.idx` де-факто закреплён за P — без линкнейма. Ключевая фраза в
комментарии: «best effort mechanism», точная привязка не требуется.

**(г) Псевдослучайный выбор — тоже per-P, но легально.** Верхнеуровневые
функции `math/rand/v2` (`Uint64`, `Uint64N`, `IntN`) не имеют глобального
состояния: они идут в `runtime.rand`, а тот читает **per-m chacha8**
(`$GOROOT/src/math/rand/v2/rand.go:258-265` — `//go:linkname runtime_rand
runtime.rand`; реализация `$GOROOT/src/runtime/rand.go:124-149`):

```go
// rand returns a random uint64 from the per-m chacha8 state.
```

Комментарий там же сообщает измеренную цену: «The performance difference on a
16-core AMD is 3.7ns/call this way versus 4.3ns/call with acquirem». То есть
`rand/v2.Uint64()` — легальный, stdlib-only, неконкурирующий селектор шарда
за единицы наносекунд. На стенде я намерил его в **2.50 нс/оп** под 16
горутинами (против 1.09 нс у `procPin`).

### 4.2. Striped counters: почему счётчик шардируется, а лимитер — нет

Аналоги JVM-ного `LongAdder` в Go существуют:

- `xsync.Counter` — «a striped int64 counter… inspired by the j.u.c.a.LongAdder
  class», <https://pkg.go.dev/github.com/puzpuzpuz/xsync#Counter>;
- `chen3feng/atomiccounter` — «в сценариях высококонкурентной записи и редкого
  чтения даёт в десятки раз большую производительность записи, чем
  `sync/atomic`», <https://github.com/chen3feng/atomiccounter>;
- в `prometheus/client_golang` эта идея обсуждалась и **не была принята**
  именно из-за отсутствия легального per-CPU в Go:
  <https://github.com/prometheus/client_golang/issues/677>.

Важно понять, **почему** счётчик шардируется бесплатно, а лимитер — нет.

`LongAdder` работает, потому что сложение коммутативно и ассоциативно:
`Add` можно применить к любому шарду, а `Value()` — это сумма. Никакого
решения по ходу `Add` принимать не нужно, и суммирование можно отложить до
момента чтения.

`Allow()` — это **не** `Add`. Это `Add` **плюс немедленное решение**,
основанное на глобальном состоянии. Отложить решение нельзя: ответ нужен
прямо сейчас. Чтобы принять корректное решение «есть ли ещё бюджет», нужно
прочитать **все** k шардов — а это k атомарных чтений плюс отсутствие
атомарности всего снимка. То есть либо мы читаем всё (и теряем весь выигрыш),
либо мы **делим бюджет заранее** — и меняем семантику.

### 4.3. Что именно ломается при делении бюджета

Формально инвариант «суммарно не больше лимита» **сохраняется**: если каждому
из k шардов дать `rate/k` и `burst/k`, сумма пропусков за любое окно не
превысит исходного лимита. Ломается **обратное**: лимитер перестаёт быть
«не меньше, чем разрешено» — шард может отказать, когда у соседа есть запас.

То есть шардирование меняет тип ошибки: было «точно», стало **«безопасно в
сторону отказа»** (fail-closed по утилизации). Для rate limiter'а это может
быть приемлемо или нет — зависит от того, зачем он стоит:

- **защита downstream от перегрузки** — приемлемо: недобор пропуска
  безопаснее перебора;
- **SLA/биллинг «клиенту положено N rps»** — неприемлемо: клиент платит за N,
  а получает 6% от N при неудачном распределении (числа ниже — реальные).

Как с этим живут реальные системы (то же деление квоты, только между
машинами, а не между кэш-линиями):

- **Аренда квоты с перебалансировкой.** Doorman (<https://github.com/youtube/doorman>)
  — «a solution for Global Distributed Client Side Rate Limiting»: центральный
  мастер выдаёт клиентам не фиксированную долю, а **лизы** — «Doorman only
  gives out capacity for a limited amount of time, in the form of leases»,
  типичная длина лизы пять минут, refresh interval — пять секунд. При каждом
  refresh мастер заново прогоняет алгоритм апортионирования исходя из
  фактического спроса каждого клиента. То есть перекос лечится **тем, что
  доля пересчитывается по спросу**, а не задаётся раз и навсегда. Цена —
  централизованный компонент и то, что система «cooperative»: клиенты обязаны
  соблюдать выданное.
- **Двухступенчатая схема: local + global.** Envoy
  (<https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting>)
  держит **обе** реализации: локальный лимитер на инстансе как грубая первая
  ступень и глобальный gRPC-сервис как точная вторая — «Local rate limiting
  can be used in conjunction with global rate limiting to reduce load on the
  global rate limit service». Прямая аналогия для нас: шард — дешёвый фильтр,
  общий лимитер — источник истины; шард отсекает основную массу, точность
  обеспечивает общий.
- **Work stealing между шардами.** При отказе своего шарда попробовать
  соседний. `xsync.Counter` делает ровно это (для счётчика): «Give a try with
  another randomly selected stripe». Для лимитера это восстанавливает
  утилизацию ценой обхода нескольких линий **на пути отказа** — а путь отказа
  как раз тот, который в `ratelimit-lab` осознанно не меряется, и который
  ломает сравнение mutex/CAS (см. CLAUDE.md проекта).

Численные последствия деления бюджета — в разделе «Шардирование: числа и цена
в семантике».

---

## 5. Backoff при проигранном CAS

### 5.1. Что вообще доступно из обычного Go, без ассемблера

| Приём | Доступен из обычного Go? | Как |
|---|---|---|
| `runtime.Gosched()` | **да, легально** | экспортированный API; отдаёт P планировщику, горутина уходит в **глобальную** очередь выполнения |
| TTAS: спин на `atomic.Load` вместо CAS | **да, легально** | обычный код; читающий спин держит линию в S-состоянии и не шлёт RFO |
| Экспоненциальный backoff | **да, легально** | счётчик витков + удлиняющийся спин на чтении |
| `PAUSE` через `runtime.procyield` | **да, но линкнейм-хак** | см. ниже |
| `PAUSE` напрямую | нет | нужен свой ассемблерный файл (`.s`), это уже не «обычный Go» |
| `goyield` (уступка в **локальную** очередь, как в `semrelease1`) | **нет** | приватная функция рантайма, линкнеймом не помечена |
| `time.Sleep` с джиттером | формально да | гранулярность — десятки микросекунд; для 100-наносекундной операции неприменимо |

Про `PAUSE` — важная поправка, которую легко пропустить. `runtime.procyield`
**помечен** pull-линкнеймом ровно так же, как `procPin`
(`$GOROOT/src/runtime/stubs.go:268-279`):

```go
// procyield should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/sagernet/sing-tun
//   - github.com/slackhq/nebula
//   - github.com/tailscale/wireguard-go
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname procyield
func procyield(cycles uint32)
```

Так что «спин как в `sync.Mutex`» воспроизводим — тем же хаком и с тем же
статусом «зала позора». **Проверено на стенде: собирается и работает**
(см. `backoff.go` в приложении). Недоступна только вторая половина —
`goyield`, уступка в *локальную* очередь P; `runtime.Gosched` кладёт горутину
в *глобальную*, что дороже.

### 5.2. Почему TTAS теоретически должен помогать

Классический TTAS (test-and-test-and-set): вместо того, чтобы бить CAS'ом,
сначала **читать** до тех пор, пока состояние не станет благоприятным. Читающий
спин не забирает линию в эксклюзив (она остаётся в S у всех), поэтому N
читателей не создают N RFO. Это соображение подтверждается измерением из §2:
`Load` под 16 горутинами — 0.25 нс/оп, то есть буквально бесплатно.

Но у нашего лимитера **нет «благоприятного состояния»**, которого можно
дождаться: `tat` меняется постоянно, и любое новое значение годится. Ждать
нечего — читающий спин просто тратит время до следующей попытки. Это и
показали измерения (см. ниже): TTAS-вариант **хуже** голой петли.

### 5.3. Что показали замеры

Полные числа — в разделе «Прямой ответ»; коротко:

| Стратегия (P=16, TIGHT) | нс/оп |
|---|---|
| `runtime.Gosched()` после **каждого** проигранного CAS | **184–188** |
| `procyield(64)` (64 `PAUSE`) на виток | 230.7 |
| `procyield(30)` + `Gosched` после первого же провала | 197.7 |
| **`sync.Mutex` (базовая)** | **356–361** |
| `procyield(30)` на виток | 377.1 |
| `Gosched` после каждого 2-го провала | 326 |
| «имитация `sync.Mutex`»: 4 × `procyield(30)`, затем `Gosched` | 387.6 |
| экспоненциальный `PAUSE` от 4 | 395.9 |
| `procyield(1)` / `(4)` / `(16)` на виток | 420 / 434 / 451 |
| TTAS (спин на чтении с ростом) | 831.6 |
| `Gosched` после каждого 4-го / 8-го / 16-го | 639 / 737 / 827 |

Три вывода:

1. **Оптимум вырожденный: уступать сразу.** Зависимость от порога backoff
   монотонная (1 → 188, 2 → 326, 4 → 639, 8 → 737, 16 → 827) — любое
   «подождём ещё чуть-чуть» только вредит.
2. **`PAUSE` работает хуже уступки** и в нашем сценарии обгоняет мьютекс
   только при очень длинной паузе (64 `PAUSE` на виток). Замерянная стоимость
   `procyield`: `procyield(1)` — 13.55 нс/оп, `procyield(30)` — 351.3 нс/оп
   (агрегат по 16 горутинам). То есть один `PAUSE` обходится порядка сотни
   наносекунд на поток; пересчёт в циклы **не проверен** (губернатор
   `powersave`, а петля из одних `PAUSE` наверняка роняет частоту).
3. **Ручная имитация `sync.Mutex` проигрывает настоящему `sync.Mutex`**
   (387.6 против 356.1). Не хватает ровно того, что недоступно снаружи
   рантайма: `goyield` в локальную очередь P и парковка через семафор.

---

## 6. Модель памяти Go

Официальный документ — «The Go Memory Model», <https://go.dev/ref/mem>,
переписан в 2022 (для Go 1.19) после серии статей Расса Кокса.

### 6.1. Что гарантирует `sync/atomic`

Дословно из <https://go.dev/ref/mem> (раздел «Atomic Values»):

> The APIs in the `sync/atomic` package are collectively "atomic operations"
> that can be used to synchronize the execution of different goroutines. If the
> effect of an atomic operation *A* is observed by atomic operation *B*, then
> *A* is synchronized before *B*. **All the atomic operations executed in a
> program behave as though executed in some sequentially consistent order.**
>
> The preceding definition has the same semantics as C++'s sequentially
> consistent atomics and Java's `volatile` variables.

То же продублировано в doc-комментарии пакета,
<https://pkg.go.dev/sync/atomic>.

### 6.2. Почему в Go нет relaxed / acquire-release

Обоснование даёт Расс Кокс в «Updating the Go Memory Model (Memory Models,
Part 3)», <https://research.swtch.com/gomm>:

> The fundamental problem is that using acquire/release atomics to make a
> program data-race-free does not result in a program that behaves in a
> sequentially consistent manner, because the atomics themselves don't.

и

> In the absence of sequentially consistent semantics… people will still write
> code like this; it will just fail mysteriously, and only in certain contexts.

То есть выбор идеологический: Go держит DRF-SC (data-race-free ⇒ sequentially
consistent) как свойство всего языка, а acquire/release это свойство ломает.
Дать «быстрый, но хитрый» примитив — значит гарантировать, что кто-нибудь
применит его неправильно и получит невоспроизводимый баг.

### 6.3. Во что это обходится на практике

На **amd64 — почти ни во что**. Модель памяти x86-TSO и так близка к SC:

- `atomic.Load` компилируется в обычный `MOVQ` (никаких барьеров);
- `atomic.Store` — в `XCHGQ` (обмен неявно `LOCK`-ed, отсюда 25.1 нс против
  1.3 нс у обычной записи — единственное реальное место, где Go платит за SC
  на x86: в C++ `store(relaxed)`/`store(release)` был бы обычным `MOV`);
- `CompareAndSwap` — `LOCK CMPXCHGQ`, в C++ он такой же и для `seq_cst`, и для
  `relaxed`.

То есть **для CAS-петли лимитера цена SC на amd64 равна нулю**: CAS есть CAS.
Цена появилась бы на arm64, где `atomic.Store` компилируется в `STLR`
(store-release), а `Load` — в `LDAR` (load-acquire), тогда как relaxed-версии
были бы просто `STR`/`LDR`. Кокс отдельно отмечает, что «ARMv8 предоставляет
прямую поддержку sequential consistency через `ldar` и `stlr`», то есть и там
разрыв невелик. **На arm64 не проверено** — стенда нет.

Практический вывод для `ratelimit-lab`: искать выигрыш в «более слабой
модели памяти» бессмысленно — его там нет.

### 6.4. Совет самого документа

Документ открывается тем, что в контексте этого исследования звучит как
предупреждение (<https://go.dev/ref/mem>, раздел «Advice»):

> If you must read the rest of this document to understand the behavior of your
> program, you are being too clever.
>
> **Don't be clever.**

---

## 7. Позиция Go team про «lock-free вместо мьютекса»

### 7.1. Doc-комментарий `sync/atomic`

<https://pkg.go.dev/sync/atomic>, первый абзац:

> Package atomic provides low-level atomic memory primitives useful for
> implementing synchronization algorithms.
>
> **These functions require great care to be used correctly.** Except for
> special, low-level applications, synchronization is better done with channels
> or the facilities of the sync package. Share memory by communicating; don't
> communicate by sharing memory.

Это не мнение, это документация: `sync/atomic` позиционируется как примитив
для **реализации** синхронизационных алгоритмов, а не для их применения.

### 7.2. Сам `sync.Mutex` — это работа Вьюкова

Starvation mode добавил Дмитрий Вьюков в CL 34310, вошло в Go 1.9
(<https://github.com/golang/go/commit/0556e26273f704db73df9e7c4c3d2e8434dec7be>,
Gerrit: <https://go-review.googlesource.com/c/go/+/34310>). Комментарий в
`mutex.go:53-77` — это текст того самого CL. Полезная перспектива: человек,
который в Go-сообществе больше всех известен работой над lock-free
структурами и race-детектором, вложил свою экспертизу **в мьютекс**, а не
рядом с ним.

Позднее туда же добавили прямую передачу P (issue 33747, «sync: Mutex
performance collapses with high concurrency»,
<https://github.com/golang/go/issues/33747>) — то есть мьютекс продолжают
целенаправленно оптимизировать под именно тот сценарий, который мы меряем.

### 7.3. Обратная сторона: Вьюков же продвигал lock-free там, где он нужен

В треде golang-dev «Atomic pointer operations and unsafe»
(<https://groups.google.com/g/golang-dev/c/SBmIen68ys0>) Вьюков описывает
замену мьютекса в `encoding/gob` на `atomic.Load/StorePointer` и приводит
масштаб выигрыша на многоядерных машинах. Ключевое в его аргументации — не
«атомики быстрее», а что **масштабируемые алгоритмы синхронизации по своей
природе требуют `unsafe`**, и это осознанный компромисс, а не оптимизация
по умолчанию.

### 7.4. Как это резюмировать честно

Позиция Go team не «мьютекс всегда быстрее» — это было бы неправдой, и наши
собственные измерения это показывают. Позиция такая:

1. дефолт — мьютекс или канал; `sync/atomic` требует «great care»;
2. модель памяти намеренно не даёт слабых порядков, чтобы у «хитрого» кода
   не было соблазна стать ещё хитрее;
3. per-P и прочие механизмы масштабирования — приватные детали рантайма,
   доступные только через линкнейм, который официально числится «залом
   позора» (<https://go.dev/issue/67401>);
4. при этом сам мьютекс годами оптимизируют именно под высокую конкуренцию,
   так что планка, которую нужно перепрыгнуть, — высокая и растёт.

---

## Прямой ответ: обгоняет ли что-нибудь `sync.Mutex`

### Ответ: **да — и не одна конструкция, а две независимые, начиная с 4 ядер**

Обе — чистый stdlib, ноль аллокаций, ноль линкнеймов, **один разделяемый
лимитер**, никакого шардирования и никакой смены семантики:

1. **Убрать из CAS-петли всё, кроме атомика.** Loop-invariant поля
   (`interval`, `burst`) прочитать в локальные переменные **до** петли —
   или, что эквивалентно, вынести их с кэш-линии атомика.
2. **`runtime.Gosched()` после каждого проигранного CAS.**

Оба приёма дают примерно одно и то же и **комбинируются** — вместе чуть
лучше каждого по отдельности.

Все числа ниже — **из одного бинаря и одного прогона** (`BenchmarkAB`),
TIGHT, `b.RunParallel`, медианы `-count=5`. Это важно: см. «разброс между
сборками» ниже.

| Конструкция (P=16, 16 горутин) | нс/оп | против mutex |
|---|---|---|
| GCRA, конфиг в регистрах **+** `Gosched` | **182.9** | **1.97×** |
| GCRA + `Gosched` после каждого проигранного CAS | 184.5 | 1.96× |
| GCRA, конфиг в локальных переменных до петли | 189.9 | 1.90× |
| GCRA, конфиг в **отдельном объекте** кучи | 198.8 | 1.81× |
| GCRA, «шардированный» с k=1 (тот же эффект) | 203.7 | 1.77× |
| `sync.Mutex` + token bucket | **360.8** | базовая |
| `sync.Mutex` + то же тело GCRA | 360.0 | 1.00× |
| GCRA, наивная петля (конфиг рядом с атомиком) | 591.4 | 0.61× |
| GCRA, конфиг отодвинут падингом на **64** байта | 831.3 | 0.43× |

### Открытие, которое переворачивает диагноз: платит не CAS, а перечитывание

Наивная петля выглядит так (`go build -gcflags=-S`, тело цикла
`c2lab.(*GCRA).Allow`, смещения 59–110):

```asm
0x003b  MOVQ  (DX), CX        ; l.tat.Load()   — смещение 0
0x003e  MOVQ  8(DX), BX       ; l.interval     — смещение 8   ← в петле!
0x0042  MOVQ  16(DX), SI      ; l.burst        — смещение 16  ← в петле!
...
0x0064  LOCK
0x0065  CMPXCHGQ  BX, (DX)
0x006e  JEQ  56               ; назад в петлю
```

`interval` и `burst` — **константы за всё время жизни лимитера**, но
компилятор обязан перечитывать их на каждом витке: `LOCK CMPXCHGQ` — барьер,
и он не может доказать, что чужая горутина их не изменила. А лежат они
**на той же кэш-линии, что и атомик**. Значит каждый чужой CAS
инвалидирует линию, и следующий виток платит **три** промаха вместо одного.

Достаточно вынести их в локальные переменные до петли — компилятор поднимает
загрузки за цикл (`MOVQ 8(DX), SI` и `MOVQ 16(DX), DI` оказываются **до**
метки цикла), и петля начинает трогать ровно одну кэш-линию:

```go
now := nowNanos()
iv, bu := l.interval, l.burst   // ← одна строка, 3.1× разницы
for {
	old := l.tat.Load()
	...
	nt := t + iv
	if nt-now > bu { return false }
	if l.tat.CompareAndSwap(old, nt) { return true }
}
```

**591.4 → 189.9 нс.** Ни одного изменения в алгоритме, ни одной новой
зависимости, ни грамма «хитрости» из тех, про которые предупреждает модель
памяти. Это самая дешёвая оптимизация во всём отчёте.

### Падинг на 64 байта делает **хуже**, на 128 — помогает

Здесь стенд преподнёс результат, который стоит отдельного абзаца, потому что
он противоречит «здравому смыслу про 64 байта» и при этом ровно совпадает с
тем, что stdlib делает у себя.

Раскладка «`prepadding` + атомик + `postpadding` + конфиг», как в
`uber-go/ratelimit`, но с шагом **64**:

| Вариант | P=16, нс/оп | воспроизведено в |
|---|---|---|
| без падинга (24-байтовая структура) | 591–605 | 3 независимых сборках |
| падинг **64** байта | 802–831 | 3 независимых сборках |
| падинг **128** байт | **227.9** | 1 сборке |
| конфиг в отдельном объекте кучи | 184–199 | 3 независимых сборках |
| конфиг в регистрах (без падинга вообще) | 190–212 | 3 независимых сборках |

То есть **64 байта не хватает: атомик и конфиг остаются в одной паре
смежных линий**. 128 байт — хватает.

Это ровно тот компромисс, который зафиксирован в самой stdlib
(`$GOROOT/src/sync/pool.go:74-77`):

```go
// Prevents false sharing on widespread platforms with
// 128 mod (cache line size) = 0 .
pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
```

— и в комментарии `xsync` («128B usage may give ~30% improvement on NUMA
machines»). Причём `internal/cpu.CacheLinePadSize` на amd64 равен 64, то есть
формально «правильный» падинг из рантайма здесь тоже был бы недостаточен.
Точный микроархитектурный механизм (adjacent-line prefetch, гранулярность
пары линий на Rocket Lake) — **не проверен**, я меряю только эффект.

Отдельно стоит отметить, что на **чистых счётчиках** (§3.3, каждая ячейка
пишется своим потоком) 128-байтовое выравнивание не дало ничего сверх
64-байтового (2.52 против 2.63 нс). Значит эффект специфичен для пары
«горячая запись + рядом лежащее чтение», а не универсален.

### Разброс между сборками: наивная структура — лотерея

Один и тот же исходник `GCRA` дал **591.4, 603.5, 605.7, 674.2, 674.5 и
976.2 нс** в шести разных сборках — при разбросе **внутри** каждой сборки
меньше 5% (например, 973.6 / 998.2 / 980.8 / 959.3 / 976.2 в одной и
566.3 / 575.7 / 512.4 / 438.3 / 560.6 в другой). Разница до **1.65×** — это
не шум прогона, это **расположение 24-байтового объекта в куче**: попадёт ли
он целиком в одну линию, ляжет ли рядом с чужой горячей памятью.

Это и есть та проблема, которую `uber-go/ratelimit` называет своим именем в
комментарии к `prepadding`: «crucial to maintain performance of this rate
limiter **in case of collocation with other frequently accessed memory**».
Практический вывод: **сравнивать lock-free реализации между сборками нельзя**,
только внутри одного бинаря и одного прогона.

### Как это соотносится с `sync.Mutex`

Обе выигрышные конструкции работают по одному принципу — **сократить работу,
которую приходится переделывать на ретрае**, — но с разных сторон:

| | наивная петля | конфиг в регистрах | `Gosched` |
|---|---|---|---|
| сколько кэш-линий трогает виток | 1–2, все инвалидируемые | 1 | 1 |
| сколько витков на успех | много | много | мало |
| откуда выигрыш | — | дешевле виток | меньше витков |

Механизм `Gosched` — тот же, что у мьютекса: убрать проигравшего с
кэш-линии. Но втрое дешевле, потому что не делает ничего из этого:

| `sync.Mutex`, медленный путь | `Gosched`-backoff |
|---|---|
| до 4 × 30 = 120 `PAUSE` (`runtime_doSpin`) | ничего |
| `acquireSudog()` + очередь в `semtable` под `root.lock` | ничего |
| `goparkunlock` — парковка и пробуждение | горутина остаётся runnable |
| CAS'ы на `m.state` (счётчик ожидающих, `mutexWoken`, `mutexStarving`) | состояния «жду» нет вообще |
| **FIFO + starvation mode** | **никаких гарантий справедливости** |

Последняя строка — плата. Lock-free гарантия («система в целом
прогрессирует») сохраняется, wait-free — нет и не было.

**И честная оговорка про механизм.** `runtime.Gosched()` идёт в
`goschedImpl` (`$GOROOT/src/runtime/proc.go:4105-4133`), который берёт
**глобальный** `sched.lock` и кладёт горутину в **глобальную** очередь:

```go
dropg()
lock(&sched.lock)
globrunqput(gp)
unlock(&sched.lock)
```

(приватный `goyield` — `proc.go:4234-4248` — делает `runqput(pp, gp, false)`,
то есть в *локальную* очередь P и без глобального лока; именно его использует
`semrelease1`, и он недоступен извне рантайма.)

То есть выигрыш достигается **переносом конкуренции с нашей кэш-линии на
`sched.lock` рантайма**. На 16 логических ядрах размен окупается вдвое.
Окупится ли он на 64 или 128 ядрах, где `sched.lock` сам становится узким
местом, — **не проверено**, стенда нет. Это главное ограничение вывода.
Приём «конфиг в регистрах» этим недостатком **не** страдает: он ничего не
перекладывает на рантайм.

### Что это стоит по латентности

Отдельный харнесс (не `RunParallel`): 16 горутин, по 200 000 вызовов, каждый
обмерян парой `time.Now()`. Оверхед часов (~190 нс) одинаков во всех строках,
поэтому строки сравнимы между собой, но **не** с `ns/op` выше.

| Реализация | p50 | p99 | p99.9 | max | ops/sec |
|---|---|---|---|---|---|
| `sync.Mutex` TB | **383 нс** | 214 мкс | 613 мкс | 2.84 мс | 2 154 516 |
| GCRA + `Gosched`(1) | 500 нс | **80.7 мкс** | **227 мкс** | 7.91 мс | **2 727 210** |
| GCRA, наивная петля | 2.28 мкс | 65.2 мкс | 302 мкс | 11.1 мс | 1 766 900 |
| CAS + `atomic.Pointer` | 6.60 мкс | 78.5 мкс | 310 мкс | 15.4 мс | 1 108 212 |

При 64 горутинах на 16 P (4-кратная переподписка) картина сохраняется:
`Gosched`(1) — 2 746 103 оп/с против 1 832 549 у мьютекса, а хвост мьютекса
разъезжается сильнее (p99 950 мкс против 302 мкс).

Итог: **мьютекс выигрывает медиану, lock-free выигрывает хвост и пропускную
способность.** Отличная медиана у мьютекса — прямое следствие normal mode
(§1.4), который специально позволяет «взять лок несколько раз подряд»;
хвост — плата за парковку.

### На скольких ядрах это начинается

Всё из одного бинаря (`BenchmarkAB`), TIGHT, число горутин = `GOMAXPROCS`:

| P | MutexTB | Mutex+GCRA | GCRA наивная | GCRA конфиг в регистрах | GCRA + `Gosched` | GCRA конфиг+`Gosched` |
|---|---|---|---|---|---|---|
| 2 | 142.5 | **132.4** | 264.6 | 199.4 | 205.2 | 147.1 |
| 4 | 208.0 | 179.0 | 393.8 | 262.6 | 209.2 | **167.3** |
| 8 | 326.1 | 314.8 | 524.5 | 268.2 | 207.5 | **185.9** |
| 16 | 360.8 | 360.0 | 591.4 | 189.9 | 184.5 | **182.9** |

(P=1, из отдельного свипа: mutex 107.3, наивный GCRA 84.1, `Gosched` 85.2 —
конкуренции нет, побеждает то, у чего короче тело, и «lock-free» тут ни при чём.)

- **P = 2** — **мьютекс выигрывает** (132.4 против 147.1). Конкуренции хватает,
  чтобы CAS начал ретраить, но мало, чтобы мьютекс ушёл в парковку: он живёт
  на спине, а спин на двух ядрах почти бесплатен.
- **P = 4 — перелом.** 167.3 против 179.0 у мьютекса с тем же телом
  (и 208.0 у token-bucket-варианта).
- **P = 8 и выше** — разрыв растёт: 1.69× при P=8, 1.97× при P=16.

**Граница проходит по P ≈ 4.** Один и тот же вывод устойчив в трёх
независимых свипах.

### Что НЕ обогнало мьютекс — и это тоже ответ

- **Наивная CAS-петля** — не обгоняет никогда при P ≥ 2, в любом
  представлении состояния: один `atomic.Int64` — 591 нс против 361 у мьютекса
  (одна сборка); `atomic.Pointer` на снапшот — 1286 нс против 361
  (другая сборка, поэтому сравнивать эти две строки между собой нельзя, а
  каждую со «своим» мьютексом — можно).
- **TTAS** (спин на `atomic.Load` перед повторным CAS) — **не помогает**:
  831.6 против 976.2 при P=16 и 503.6 против 495.6 при P=4, то есть ничья с
  наивной петлёй и вдвое хуже мьютекса. Причина в §5.2: ждать нечего, любое
  новое значение годится.
- **`PAUSE`-backoff через `runtime.procyield`** (линкнейм-хак) — почти всегда
  хуже мьютекса: 1 `PAUSE` на виток 420.5, 4 → 434.4, 16 → 450.6, 30 → 377.1,
  экспоненциальный от 4 → 395.9 (мьютекс 356.1). Единственное исключение —
  64 `PAUSE` на виток: 230.7 нс, обгоняет мьютекс, но всё равно хуже
  немедленного `Gosched` (187.3).
- **Имитация `sync.Mutex` вручную** — 30 `PAUSE` × 4 витка, затем уступка:
  387.6 нс, **хуже настоящего мьютекса**. Что логично: у нас нет `goyield`,
  только `Gosched` с глобальным локом, и нет парковки.
- **Уход от аллокаций** сам по себе — 1.3× (1286 → 975 в первой сборке), до
  мьютекса не дотягивает.
- **Отложенный `Gosched`** — монотонно хуже немедленного: порог 1 → 188,
  2 → 326, 4 → 639, 8 → 737, 16 → 827 нс.

Общий вывод из отрицательных результатов: **всё, что пытается пересидеть
конкуренцию на месте, проигрывает**. Работают только две вещи — сделать виток
дешевле и сделать витков меньше.

## Шардирование: числа и цена в семантике

### На скольких ядрах шардированный лимитер обгоняет mutex

Чтобы ответ не смешивал «шардирование» с «сменой алгоритма», сравниваем
**одно и то же тело** — token bucket под `sync.Mutex` — в двух вариантах:
один общий лимитер против 16 шардов, шард выбирается по номеру P
(`runtime_procPin`), каждый шард выровнен на 64 байта, `rate/16` и `cap/16`.
TIGHT, медианы `-count=5`, число горутин = `GOMAXPROCS`:

| P | `MutexTB` общий | `MutexTB` per-P × 16 | ускорение |
|---|---|---|---|
| 1 | **114.9** | 124.9 | **0.92× (хуже)** |
| 2 | 140.3 | **57.99** | 2.42× |
| 4 | 224.9 | **30.56** | 7.36× |
| 8 | 324.8 | **15.48** | 21.0× |
| 16 | 355.8 | **10.73** | **33.2×** |

То же для GCRA на `atomic.Int64`:

| P | `GCRA` общий | `GCRA` per-P × 16 | ускорение |
|---|---|---|---|
| 1 | **90.62** | 102.4 | **0.89× (хуже)** |
| 2 | 328.2 | **47.50** | 6.91× |
| 4 | 460.9 | **24.93** | 18.5× |
| 8 | 652.5 | **12.67** | 51.5× |
| 16 | 613.9 | **9.32** | 65.9× |

**Ответ: шардированный лимитер обгоняет mutex начиная с двух ядер, и с этого
момента ускорение растёт примерно линейно по числу ядер.** На одном ядре он
*проигрывает* — на 8–13%, ровно на стоимость селектора шарда (`procPin`
1.09 нс + индексация) при отсутствии выигрыша.

Это принципиально другая форма зависимости, чем у всего остального в этом
отчёте: шардированный вариант **масштабируется**, а не «деградирует
медленнее». `MutexTB` per-P идёт 124.9 → 57.99 → 30.56 → 15.48 → 10.73 нс,
то есть агрегатная пропускная способность растёт почти вдвое на каждое
удвоение P. Ни одна нешардированная конструкция так не умеет и не может:
там одна кэш-линия, и её пропускная способность — константа.

### Цена: утилизация падает пропорционально доле активных шардов

Эксперимент: `rate = 100 000/с`, `capacity = 1000`, окно 200 мс, бесконечный
спрос. Идеал для единого лимитера — `1000 + 100000 × 0.2 = 21 000` пропусков.

| Конфигурация | admitted | % идеала |
|---|---|---|
| единый GCRA, 16 горутин | 22 208 | 105.8% |
| единый GCRA, 1 горутина | 21 044 | 100.2% |
| шардированный k=4, трафик равномерно | 21 450 | 102.1% |
| шардированный k=16, трафик равномерно | 21 024 | 100.1% |
| шардированный k=64, трафик равномерно | 21 788 | 103.8% |
| шардированный k=16, весь трафик в **1** шард | **1 385** | **6.6%** |
| шардированный k=16, весь трафик в 2 шарда | 2 786 | 13.3% |
| шардированный k=16, весь трафик в 4 шарда | 5 588 | 26.6% |
| шардированный k=16, весь трафик в 8 шардов | 10 680 | 50.9% |
| per-P k=16, **1** активная горутина | **1 314** | **6.3%** |
| per-P k=16, 2 активные горутины | 2 628 | 12.5% |
| per-P k=16, 4 активные горутины | 5 441 | 25.9% |
| per-P k=16, 8 активных горутин | 11 005 | 52.4% |
| per-P k=16, 16 активных горутин | 21 032 | 100.2% |

(Проценты чуть выше 100 — это доля пропусков за 200 мс с учётом стартового
burst и погрешности окна; на выводы не влияет.)

Закон простой и точный:

> **утилизация ≈ (число активных шардов) / k**

При k = 16 и одном активном источнике нагрузки лимитер, настроенный на
100 000 rps, пропускает **6 300 rps**. Это не «немного консервативнее» —
это **в 16 раз меньше настроенного**.

**Это делает per-P шардирование ловушкой особого рода**: число активных
шардов зависит не от конфигурации, а от того, сколько горутин сейчас
работает и как их разложил планировщик. Лимитер, который на нагрузочном
тесте с 16 горутинами показывал ровно настроенный rate, ночью при одном
активном воркере начнёт душить трафик в 16 раз — и логи не покажут ничего,
кроме «rate limited».

### Формально инвариант не нарушается — и именно это опасно

Сумма пропускных способностей шардов равна исходному лимиту, поэтому
«суммарно не больше лимита» держится **всегда**. Ошибка односторонняя:
лимитер может отказать там, где не должен был, но не может пропустить
лишнее. Это fail-closed.

Поэтому шардированный лимитер:

- **годится** как защита downstream от перегрузки: недобор безопаснее
  перебора, а под настоящей перегрузкой нагрузка обычно и так размазана по
  всем шардам, то есть утилизация близка к 100%;
- **не годится** там, где сам rate — это обещание («клиенту положено N rps»),
  потому что обещание нарушается тихо и в 16 раз.

Как это лечат в реальных системах — §4.3: динамическое перераспределение
квоты по фактическому спросу (Doorman: лизы + апортионирование на каждый
refresh), двухступенчатая схема local+global (Envoy), либо work stealing
между шардами (`xsync.Counter`: «Give a try with another randomly selected
stripe»). Все три возвращают часть разделяемого состояния — то есть часть
исходной проблемы.

### Свип по числу шардов k

TIGHT, 16 горутин, `GOMAXPROCS=16`, медианы `-count=5`. «rand» — выбор шарда
через `math/rand/v2`, «per-P» — через `runtime_procPin`:

| k | GCRA rand, выровнен | GCRA rand, **не** выровнен | GCRA per-P | MutexTB rand | MutexTB per-P |
|---|---|---|---|---|---|
| 1 | 194.7 | 198.6 | 195.3 | 380.5 | 363.4 |
| 2 | 216.0 | 183.1 | 155.7 | 310.2 | 180.7 |
| 4 | 128.9 | 178.8 | 116.7 | 223.4 | 89.97 |
| 8 | 69.50 | 147.8 | 44.15 | 123.4 | 29.26 |
| 16 | 41.29 | 95.54 | **9.34** | 81.52 | 10.67 |
| 32 | 31.47 | 65.35 | 9.55 | 65.86 | 11.20 |
| 64 | 27.20 | 43.84 | 9.85 | 50.85 | 11.35 |

Что здесь видно:

1. **Выбор шарда решает больше, чем число шардов.** При k=16 per-P даёт
   9.34 нс, а случайный выбор — 41.29 нс, вчетверо хуже. Причина очевидна:
   `procPin` **гарантирует** эксклюзивность шарда (пока горутина пришпилена,
   на этом P никто больше не бежит), а случайный выбор при 16 горутинах на
   16 шардов даёт коллизии по «задаче о днях рождения» — коллизия есть почти
   всегда.
2. **Насыщение ровно на k = числу логических ядер.** per-P: 9.34 (k=16),
   9.55 (k=32), 9.85 (k=64) — дальше расти некуда, шардов и так больше, чем
   P. Случайный выбор продолжает медленно улучшаться (41.3 → 27.2), потому что
   разреживание уменьшает вероятность коллизии.
3. **Падинг даёт 2.3× при k=16** (41.29 против 95.54) и 2.1× при k=8. Без
   выравнивания 16 шардов по 8 байт укладываются в две кэш-линии, и
   шардирование частично обесценивается — это тот же эффект, что в §3.3
   измерен как 13.7× на чистых счётчиках. Исключение — k=2, где невыровненный
   вариант оказался чуть *быстрее* (183.1 против 216.0): при двух шардах
   конкуренция всё равно предельная, а падинг добавляет только шаг по памяти.
4. **k = 1 — это не «шардирование», это контроль**, и именно он дал самую
   неожиданную находку всего отчёта: 203.7 нс против 591.4 нс у «голого»
   `GCRA` при идентичной логике (числа из одного бинаря). Причина не в
   шардировании — при k=1 шард один, — а в том, что у `ShardedGCRA` поля
   `interval`/`burst` лежат **в другом объекте**, чем атомик, и потому не
   инвалидируются чужим CAS. Разбор — раздел «Прямой ответ», подраздел
   «платит не CAS, а перечитывание».

---

## 8. Применимость к `ratelimit-lab`

### 8.1. Что подтвердилось в текущей реализации

`internal/limiter/lockfree_tokenbucket.go` — CAS-петля над
`atomic.Pointer[lockFreeState]` со снапшотом `{tokens float64, last time.Time}`.
Три вещи из её doc-комментариев исследование подтверждает:

1. **`atomic.Pointer` на снапшот структурно исключает ABA** («a snapshot's
   address cannot be reused while any CAS loop still holds the old pointer»).
   Это верно и это не мелочь.
2. **Аллокация на виток — не главная беда.** Уход от `atomic.Pointer` к одному
   `atomic.Int64` даёт ~1.3×, но до мьютекса не дотягивает. Основную часть
   разрыва делают ретраи и работа внутри витка.
3. **Отказ без CAS — правильное решение.** §2 объясняет количественно: CAS в
   разделяемую линию стоит минимум 72 нс, до всяких ретраев. Отказ, не
   трогающий линию, экономит ровно эту сумму.

### 8.2. Находка, которая применима прямо к этому файлу

Структура лимитера (`lockfree_tokenbucket.go:47-54`):

```go
type LockFreeTokenBucket struct {
	state atomic.Pointer[lockFreeState]  // смещение 0  — сюда бьёт CAS

	rate     float64                     // смещение 8   ┐
	capacity float64                     // смещение 16  │ читаются
	                                     //              │ на каждом
	clk Clock                            // смещение 24  ┘ витке петли
}
```

Итого 40 байт — **всё в одной кэш-линии с CAS-целью**. А тело петли
(`AllowN`, `lockfree_tokenbucket.go:119-134`) на каждом витке читает
`b.clk` (интерфейс: два слова + косвенный вызов), `b.rate` и `b.capacity`:

```go
for {
	old := b.state.Load()
	now := b.clk.Now()                                   // ← b.clk
	next, ok := nextLockFreeState(old, now, b.rate, b.capacity, n)  // ← b.rate, b.capacity
	...
	if b.state.CompareAndSwap(old, next) { return true }
}
```

Это **в точности та патология**, которую я измерил на GCRA: 591 → 190 нс
(3.1×) от одного лишь выноса loop-invariant полей за петлю. Здесь она даже
выраженнее, потому что читается на четыре слова больше (интерфейс `Clock` —
это itab + data).

Правка — три строки и **нулевое изменение семантики**:

```go
func (b *LockFreeTokenBucket) AllowN(n int) bool {
	if n <= 0 {
		return true
	}
	clk, rate, capacity := b.clk, b.rate, b.capacity   // ← loop-invariant, за петлю
	for {
		old := b.state.Load()
		now := clk.Now()
		next, ok := nextLockFreeState(old, now, rate, capacity, n)
		...
	}
}
```

Инвариант «часы читаются **после** `state.Load()`» (`lockfree_tokenbucket.go:121-124`)
при этом сохраняется полностью: за петлю выносится только *значение поля*
`b.clk`, а сам вызов `clk.Now()` остаётся внутри и на том же месте.

**Это не проверено на самом `ratelimit-lab`** — я мерил свою модельную
реализацию в отдельном модуле. Проверять надо микробенчмарком
`internal/limiter/limiter_bench_test.go` в одном прогоне до/после (см. §8.3
про разброс между сборками).

### 8.3. Методологическое предупреждение для `cmd/bench`

Один и тот же исходник наивной CAS-структуры дал **591…976 нс** в шести
разных сборках при разбросе внутри прогона меньше 5%. Причина —
расположение объекта в куче относительно границ кэш-линий и соседей.

Для проекта это значит буквально следующее: **числа из `cmd/bench`,
полученные в разных сборках, несравнимы для lock-free реализаций.** Разница
до 1.65× возникает без единой правки кода. Мьютексные реализации этим не
страдают (`MutexTB` держался в 345–380 нс во всех шести сборках) — потому
что под локом линия и так эксклюзивна.

Практический вывод: сравнительную таблицу собирать **одним прогоном одного
бинаря**, и если алгоритмы добавляются/удаляются — перемерять всё, а не
дописывать строку к старой таблице.

### 8.4. Что исследование ставит под вопрос

Doc-комментарий `AllowN` (`lockfree_tokenbucket.go:107-113`):

> Backoff on a failed CAS: none — the loop retries immediately. […] Yielding
> (runtime.Gosched) or spinning with pauses would trade that simplicity for
> scheduler latency without a correctness benefit, and the project favors
> readable coordination code over benchmark-tuned backoff (SKILL §6).

Корректностная часть верна. Производительностная — **измеримо неверна на
этой машине**: `runtime.Gosched()` после каждого проигранного CAS даёт не
потерю, а 3.2× относительно петли без backoff и 1.96× относительно
`sync.Mutex`. Про «spinning with pauses» комментарий, наоборот, прав:
`PAUSE`-backoff действительно почти всегда хуже (§5.3).

Решение принято сознательно и в пользу читаемости, менять его без спроса
нельзя. Но формулировку стоит либо сузить до «мы выбрали читаемость», либо
привести числа — сейчас она утверждает больше, чем проверено.

### 8.5. Нюанс, который нельзя потерять: часы внутри петли

Вынос `Now()` **за** петлю (не путать с выносом *поля* `b.clk`, §8.2)
изменит семантику, и это задокументировано (`lockfree_tokenbucket.go:121-124`):

> Read the clock after loading the state: old.last came from a Now() call that
> completed before the snapshot was published, so a Now() issued after the load
> is >= old.last under the Clock contract and elapsed below is never negative.

Прочитать часы до `state.Load()` — значит допустить отрицательный
`now.Sub(old.last)`, а с ним ложный отказ. Это fail-closed, но это другое
поведение. Мой модельный `GCRA` выносит часы за петлю безопасно только
потому, что состояние там — TAT в наносекундах, и ветка `if t < now { t = now }`
сама нормализует отрицательный разрыв. **Перенос приёма требует сначала
сменить представление состояния.**

### 8.6. Практические выводы, отсортированные по цене/эффекту

| Приём | Эффект (P=16) | Цена |
|---|---|---|
| **Вынести loop-invariant поля за CAS-петлю** | 591 → 190 нс (3.1×) | одна строка, семантика не меняется |
| `runtime.Gosched()` после проигранного CAS | 591 → 185 нс (3.2×) | 2 строки; портит p50 (383 → 500 нс), улучшает p99 (214 → 81 мкс); перекладывает конкуренцию на `sched.lock` |
| Оба приёма вместе | 591 → 183 нс | — |
| Падинг **128** байт вокруг атомика | 831 → 228 нс относительно падинга 64 | +256 байт на лимитер; 64 байта делают хуже |
| Уход от `atomic.Pointer` к `atomic.Int64` | ~1.3×, 0 аллокаций | смена представления, потеря float-точности `tokens` |
| Шардирование | 361 → 10.7 нс (33×) | **меняет семантику**: утилизация ≈ активных шардов / k |
| Вынос часов за петлю | ×1.27 (данные основной сессии) | требует смены представления состояния (§8.5) |

### 8.7. Что стоит и чего не стоит тащить в проект

**Стоит:**

- правку §8.2 (loop-invariant за петлю) — она бесплатна, не меняет семантику
  и является хорошей учебной иллюстрацией того, что «lock-free медленный» —
  часто утверждение не про CAS, а про раскладку данных;
- GCRA в одном `atomic.Int64` как отдельный алгоритм (это то же, что делает
  `uber-go/ratelimit`) — показывает, что представление состояния решает
  больше, чем механизм синхронизации;
- вариант с `Gosched`-backoff — на нём хорошо видно, чем платит lock-free
  (p50 хуже, p99 лучше);
- предупреждение §8.3 в `cmd/bench` или в CLAUDE.md.

**Не стоит без явного решения пользователя:**

- шардированный лимитер в общей сравнительной таблице: он несравним по
  семантике, и таблица начнёт сравнивать несравнимое. Если добавлять — то
  отдельной секцией с обязательным столбцом «утилизация» рядом с «нс/оп»;
- `//go:linkname` на `runtime.procPin` / `runtime.procyield`: проект
  декларирует «только стандартная библиотека», а линкнейм — не stdlib-API,
  а хак с официальным статусом «зала позора» (<https://go.dev/issue/67401>).
  Для учебной демонстрации в отдельном файле с явной пометкой — допустимо;
  как часть основного набора — нет.

## Собственные замеры (код и сырой вывод)

### Методика

- Отдельный Go-модуль вне репозитория (`module c2lab`, Go 1.23.4), чтобы не
  трогать `ratelimit-lab`.
- Все параллельные замеры — `b.RunParallel` на **одном разделяемом** лимитере,
  `GOMAXPROCS=16` по умолчанию.
- **Меряется только путь допуска.** Каждый параллельный бенчмарк считает
  отказы и падает через `b.Fatalf`, если хоть один отказ случился, — иначе
  сравнение mutex/CAS бессмысленно (это правило проекта, CLAUDE.md).
- Два режима параметров, оба без отказов:
  - **LOOSE** — `rate = 1e9/с`, `capacity = 1e9`: GCRA-шный TAT отстаёт от
    `now`, срабатывает ветка `t = now`;
  - **TIGHT** — `rate = 1e6/с`, `capacity = 1e9`: спрос выше rate, но burst
    огромен, поэтому TAT идёт **впереди** `now`, и каждый `Allow` обязан
    продвинуть строго монотонно растущее разделяемое значение. Это регим
    максимальной конкуренции; именно он воспроизводит числа основной сессии.
- Медианы по `-count=5`.
- **Сравнения только внутри одного бинаря.** Раскладка объекта в куче меняет
  результат наивной CAS-петли до 1.65× между сборками (см. «Прямой ответ»),
  поэтому итоговая таблица собрана одним бенчмарком (`BenchmarkAB`) в одном
  прогоне. Где приходится ссылаться на другую сборку — это оговорено в тексте.
- Замеры, которые пишут в один и тот же CPU, **не запускались параллельно**:
  весь набор прогонялся последовательно одним скриптом.

### Ключевые реализации

Общий источник времени (одно чтение часов на вызов, вне CAS-петли):

```go
var base = time.Now()
func nowNanos() int64 { return int64(time.Since(base)) }
```

**Baseline — mutex token bucket** (часы читаются вне критической секции —
это самый выгодный для мьютекса вариант):

```go
type MutexTB struct {
	mu     sync.Mutex
	tokens float64
	last   int64
	rate   float64 // токенов на наносекунду
	cap    float64
}

func (l *MutexTB) Allow() bool {
	now := nowNanos()
	l.mu.Lock()
	l.tokens += float64(now-l.last) * l.rate
	if l.tokens > l.cap {
		l.tokens = l.cap
	}
	l.last = now
	ok := l.tokens >= 1
	if ok {
		l.tokens--
	}
	l.mu.Unlock()
	return ok
}
```

**CAS + `atomic.Pointer` на снапшот** — форма текущей реализации
`ratelimit-lab`:

```go
type tbState struct {
	tokens float64
	last   int64
}

type CASPtrTB struct {
	st   atomic.Pointer[tbState]
	rate float64
	cap  float64
}

func (l *CASPtrTB) Allow() bool {
	now := nowNanos()
	for {
		old := l.st.Load()
		tok := old.tokens + float64(now-old.last)*l.rate
		if tok > l.cap {
			tok = l.cap
		}
		if tok < 1 {
			return false
		}
		nw := &tbState{tokens: tok - 1, last: now}
		if l.st.CompareAndSwap(old, nw) {
			return true
		}
	}
}
```

**GCRA в одном `atomic.Int64`** — состояние это TAT (theoretical arrival
time) в наносекундах; ноль аллокаций, одна кэш-линия:

```go
type GCRA struct {
	tat      atomic.Int64
	interval int64 // 1/rate, нс
	burst    int64 // interval * capacity
}

func (l *GCRA) Allow() bool {
	now := nowNanos()
	for {
		old := l.tat.Load()
		t := old
		if t < now {
			t = now
		}
		nt := t + l.interval
		if nt-now > l.burst {
			return false
		}
		if l.tat.CompareAndSwap(old, nt) {
			return true
		}
	}
}
```

**Победитель — тот же GCRA плюс `runtime.Gosched()` после каждого
проигранного CAS** (`thr = 1`):

```go
type GCRABackoff struct {
	tat      atomic.Int64
	interval int64
	burst    int64
	thr      int  // после скольких проигранных CAS уступать планировщику
	refresh  bool // перечитывать ли часы после уступки
}

func (l *GCRABackoff) Allow() bool {
	now := nowNanos()
	fails := 0
	for {
		old := l.tat.Load()
		t := old
		if t < now {
			t = now
		}
		nt := t + l.interval
		if nt-now > l.burst {
			return false
		}
		if l.tat.CompareAndSwap(old, nt) {
			return true
		}
		fails++
		if fails >= l.thr {
			runtime.Gosched()
			fails = 0
			if l.refresh {
				now = nowNanos()
			}
		}
	}
}
```

Про `refresh`: с `refresh=false` часы остаются «старыми», из-за чего
`nt - now` завышается и вызов скорее откажет, чем допустит — то есть ошибка
fail-closed, лимит не нарушается. Разница в скорости между `true` и `false`
в пределах шума (189.3 против 188.4 нс на TIGHT), поэтому **правильнее брать
`refresh = true`**.

**Шардированный GCRA** — k независимых TAT, каждый с `interval*k` (то есть
`rate/k`) и `burst/k`; выравнивание на кэш-линию обязательно:

```go
type gcraShardPadded struct {
	tat atomic.Int64
	_   [64 - 8]byte
}

// селектор шарда: math/rand/v2 читает per-m состояние, не конкурирует
func (l *ShardedGCRA) Allow() bool {
	i := rand.Uint64() & l.mask
	return l.allowShard(&l.shards[i].tat, nowNanos())
}
```

**Per-P шардирование через линкнейм** (плюс пустой `.s`-файл в пакете):

```go
import _ "unsafe"

//go:linkname runtime_procPin runtime.procPin
func runtime_procPin() int

//go:linkname runtime_procUnpin runtime.procUnpin
func runtime_procUnpin()

func (l *PerPGCRA) Allow() bool {
	pid := runtime_procPin()
	runtime_procUnpin()
	s := &l.shards[pid%l.n].tat
	// ... тот же CAS-цикл по s
}
```

**Три варианта «убрать конфиг с горячей линии»** — все три дают один и тот
же эффект, различаются только ценой:

```go
// (а) в локальные переменные — 189.9 нс, ничего не стоит
func (l *GCRALocalCfg) Allow() bool {
	now := nowNanos()
	iv, bu := l.interval, l.burst
	for {
		old := l.tat.Load()
		t := old
		if t < now { t = now }
		nt := t + iv
		if nt-now > bu { return false }
		if l.tat.CompareAndSwap(old, nt) { return true }
	}
}

// (б) атомик в отдельном объекте кучи — 198.8 нс, +1 указатель
type GCRASepCfg struct {
	tat      *atomic.Int64
	interval int64
	burst    int64
}

// (в) падинг на 128 байт (64 НЕ работает) — 227.9 нс, +256 байт
type GCRAPad128 struct {
	_        [128]byte
	tat      atomic.Int64
	_        [128 - 8]byte
	interval int64
	burst    int64
}
```

**Замер латентности** (перцентили, отдельно от `RunParallel`):

```go
func measureLatency(mk func() limiter, g, opsPerG int) (p50, p99, p999, max time.Duration, thr float64) {
	l := mk()
	samples := make([][]int64, g)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < g; i++ {
		samples[i] = make([]int64, opsPerG)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := samples[i]
			<-start
			for j := 0; j < opsPerG; j++ {
				t0 := time.Now()
				l.Allow()
				s[j] = int64(time.Since(t0))
			}
		}(i)
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	// ... сортировка all, квантили, throughput = g*opsPerG / elapsed
}
```

**Замер семантики шардирования** — бесконечный спрос в фиксированном окне,
считаем допущенные:

```go
const (
	semRate   = 100000.0
	semCap    = 1000.0
	semWindow = 200 * time.Millisecond
)
// идеал = semCap + semRate * 0.2 = 21 000
// drain(window, g, call) запускает g горутин, каждая крутит call(id)
// до закрытия stop-канала, и суммирует допущенные.
```

### Проверки, сделанные отдельно

| Утверждение | Как проверено | Результат |
|---|---|---|
| `internal/cpu` не импортируется извне | отдельный модуль с `import "internal/cpu"` | `use of internal package internal/cpu not allowed` |
| `runtime.procPin` доступен pull-линкнеймом в Go 1.23.4 | сборка + `go test` | собралось, слинковалось, `procPin` вернул номер P |
| `runtime.procyield` доступен pull-линкнеймом | то же | собралось и работает |
| Кодогенерация атомиков на amd64 | `go build -gcflags=-S` | `Load`→`MOVQ`, `Store`→`XCHGQ`, `Add`→`LOCK XADDQ`, `CAS`→`LOCK CMPXCHGQ` |

---

### Сырой вывод: медианы `-count=5`

Ниже — медианы по каждому файлу сырого вывода. Полный сырой вывод
(по 5 строк на бенчмарк) занял бы несколько сотен строк; медианы
посчитаны скриптом, который берёт `ns/op` из каждой строки
`go test -bench` и возвращает медиану по 5 прогонам.

**Все варианты GCRA в одном бинаре (главная таблица «Прямого ответа»)** (`ab.txt`):

```
BenchmarkAB/MutexTB                        360.80 ns/op   (n=5)
BenchmarkAB/MutexGCRA                      360.00 ns/op   (n=5)
BenchmarkAB/GCRA_plain                     591.40 ns/op   (n=5)
BenchmarkAB/GCRA_padded                    831.30 ns/op   (n=5)
BenchmarkAB/GCRA_sepcfg                    198.80 ns/op   (n=5)
BenchmarkAB/GCRA_sharded_k1                203.70 ns/op   (n=5)
BenchmarkAB/GCRA_localcfg                  189.90 ns/op   (n=5)
BenchmarkAB/GCRA_gosched1                  184.50 ns/op   (n=5)
BenchmarkAB/GCRA_localcfg_gosched          182.90 ns/op   (n=5)
BenchmarkAB/P=2/MutexTB                    142.50 ns/op   (n=5)
BenchmarkAB/P=2/MutexGCRA                  132.40 ns/op   (n=5)
BenchmarkAB/P=2/GCRA_plain                 264.60 ns/op   (n=5)
BenchmarkAB/P=2/GCRA_padded                274.10 ns/op   (n=5)
BenchmarkAB/P=2/GCRA_sepcfg                217.80 ns/op   (n=5)
BenchmarkAB/P=2/GCRA_sharded_k1            230.20 ns/op   (n=5)
BenchmarkAB/P=2/GCRA_localcfg              199.40 ns/op   (n=5)
BenchmarkAB/P=2/GCRA_gosched1              205.20 ns/op   (n=5)
BenchmarkAB/P=2/GCRA_localcfg_gosched      147.10 ns/op   (n=5)
BenchmarkAB/P=4/MutexTB                    208.00 ns/op   (n=5)
BenchmarkAB/P=4/MutexGCRA                  179.00 ns/op   (n=5)
BenchmarkAB/P=4/GCRA_plain                 393.80 ns/op   (n=5)
BenchmarkAB/P=4/GCRA_padded                396.50 ns/op   (n=5)
BenchmarkAB/P=4/GCRA_sepcfg                256.70 ns/op   (n=5)
BenchmarkAB/P=4/GCRA_sharded_k1            275.30 ns/op   (n=5)
BenchmarkAB/P=4/GCRA_localcfg              262.60 ns/op   (n=5)
BenchmarkAB/P=4/GCRA_gosched1              209.20 ns/op   (n=5)
BenchmarkAB/P=4/GCRA_localcfg_gosched      167.30 ns/op   (n=5)
BenchmarkAB/P=8/MutexTB                    326.10 ns/op   (n=5)
BenchmarkAB/P=8/MutexGCRA                  314.80 ns/op   (n=5)
BenchmarkAB/P=8/GCRA_plain                 524.50 ns/op   (n=5)
BenchmarkAB/P=8/GCRA_padded                689.40 ns/op   (n=5)
BenchmarkAB/P=8/GCRA_sepcfg                303.50 ns/op   (n=5)
BenchmarkAB/P=8/GCRA_sharded_k1            340.60 ns/op   (n=5)
BenchmarkAB/P=8/GCRA_localcfg              268.20 ns/op   (n=5)
BenchmarkAB/P=8/GCRA_gosched1              207.50 ns/op   (n=5)
BenchmarkAB/P=8/GCRA_localcfg_gosched      185.90 ns/op   (n=5)
```

**Падинг 64 против 128 байт** (`pad128.txt`):

```
BenchmarkPad128/GCRA_plain         603.50 ns/op   (n=5)
BenchmarkPad128/GCRA_pad64         802.40 ns/op   (n=5)
BenchmarkPad128/GCRA_pad128        227.90 ns/op   (n=5)
BenchmarkPad128/GCRA_sepcfg        184.00 ns/op   (n=5)
BenchmarkPad128/GCRA_localcfg      211.60 ns/op   (n=5)
BenchmarkPad128/MutexTB            372.50 ns/op   (n=5)
```

**Атомики, false sharing, часы, селекторы шардов** (`atomics.txt`):

```
BenchmarkCAS_Uncontended                    36.90 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkAdd_Uncontended                    19.58 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkLoad_Uncontended                    1.31 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkStore_Uncontended                  20.03 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkPlainInc_Uncontended                1.21 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkCAS_Contended                     218.00 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkCAS_Contended_NoRetry              72.14 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkAdd_Contended                      43.46 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkLoad_Contended                      0.23 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkMutexIncr_Contended               284.00 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkFalseSharing_Unpadded              34.44 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkFalseSharing_Padded64               2.52 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkFalseSharing_Padded128              2.63 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkTimeNow                           119.30 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkTimeSince                          69.83 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkTimeNow_Parallel                    9.90 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkShardSelector/rand_v2_Uint64        2.50 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkShardSelector/procPin               1.09 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkShardSelector/Gosched            1333.00 ns/op   (n=5)        0 B/op    0 allocs/op
```

**Serial / Parallel по реализациям** (`limiters.txt`):

```
BenchmarkSerial/LOOSE/MutexTB            113.70 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkSerial/LOOSE/CASPtrTB           170.50 ns/op   (n=5)       16 B/op    1 allocs/op
BenchmarkSerial/LOOSE/GCRA                86.92 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkSerial/LOOSE/GCRATTAS            93.54 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkSerial/LOOSE/GCRAGosched         89.26 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkSerial/TIGHT/MutexTB            118.30 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkSerial/TIGHT/CASPtrTB           178.30 ns/op   (n=5)       16 B/op    1 allocs/op
BenchmarkSerial/TIGHT/GCRA                88.04 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkSerial/TIGHT/GCRATTAS            88.96 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkSerial/TIGHT/GCRAGosched         89.16 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkParallel/LOOSE/MutexTB          370.80 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkParallel/LOOSE/CASPtrTB        1269.00 ns/op   (n=5)      137 B/op    8 allocs/op
BenchmarkParallel/LOOSE/GCRA             679.50 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkParallel/LOOSE/GCRATTAS         838.70 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkParallel/LOOSE/GCRAGosched      289.50 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkParallel/TIGHT/MutexTB          361.10 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkParallel/TIGHT/CASPtrTB        1286.00 ns/op   (n=5)      132 B/op    8 allocs/op
BenchmarkParallel/TIGHT/GCRA             976.20 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkParallel/TIGHT/GCRATTAS         831.60 ns/op   (n=5)        0 B/op    0 allocs/op
BenchmarkParallel/TIGHT/GCRAGosched      291.20 ns/op   (n=5)        0 B/op    0 allocs/op
```

**Порог Gosched-backoff и TTAS** (`backoff.txt`):

```
BenchmarkBackoff/LOOSE/Gosched_thr=1_refresh=true        201.70 ns/op   (n=5)
BenchmarkBackoff/LOOSE/Gosched_thr=1_refresh=false       189.30 ns/op   (n=5)
BenchmarkBackoff/LOOSE/Gosched_thr=2_refresh=true        327.30 ns/op   (n=5)
BenchmarkBackoff/LOOSE/Gosched_thr=2_refresh=false       326.00 ns/op   (n=5)
BenchmarkBackoff/LOOSE/Gosched_thr=4_refresh=true        654.20 ns/op   (n=5)
BenchmarkBackoff/LOOSE/Gosched_thr=4_refresh=false       650.40 ns/op   (n=5)
BenchmarkBackoff/LOOSE/Gosched_thr=8_refresh=true        730.00 ns/op   (n=5)
BenchmarkBackoff/LOOSE/Gosched_thr=8_refresh=false       707.70 ns/op   (n=5)
BenchmarkBackoff/LOOSE/Gosched_thr=16_refresh=true       799.10 ns/op   (n=5)
BenchmarkBackoff/LOOSE/Gosched_thr=16_refresh=false      838.50 ns/op   (n=5)
BenchmarkBackoff/LOOSE/SpinGosched_spins=1               314.90 ns/op   (n=5)
BenchmarkBackoff/LOOSE/SpinGosched_spins=2               460.20 ns/op   (n=5)
BenchmarkBackoff/LOOSE/SpinGosched_spins=4               484.50 ns/op   (n=5)
BenchmarkBackoff/LOOSE/MutexTB_ref                       375.70 ns/op   (n=5)
BenchmarkBackoff/TIGHT/Gosched_thr=1_refresh=true        189.30 ns/op   (n=5)
BenchmarkBackoff/TIGHT/Gosched_thr=1_refresh=false       188.40 ns/op   (n=5)
BenchmarkBackoff/TIGHT/Gosched_thr=2_refresh=true        328.20 ns/op   (n=5)
BenchmarkBackoff/TIGHT/Gosched_thr=2_refresh=false       326.20 ns/op   (n=5)
BenchmarkBackoff/TIGHT/Gosched_thr=4_refresh=true        627.70 ns/op   (n=5)
BenchmarkBackoff/TIGHT/Gosched_thr=4_refresh=false       639.30 ns/op   (n=5)
BenchmarkBackoff/TIGHT/Gosched_thr=8_refresh=true        731.40 ns/op   (n=5)
BenchmarkBackoff/TIGHT/Gosched_thr=8_refresh=false       737.00 ns/op   (n=5)
BenchmarkBackoff/TIGHT/Gosched_thr=16_refresh=true       836.50 ns/op   (n=5)
BenchmarkBackoff/TIGHT/Gosched_thr=16_refresh=false      826.80 ns/op   (n=5)
BenchmarkBackoff/TIGHT/SpinGosched_spins=1               311.60 ns/op   (n=5)
BenchmarkBackoff/TIGHT/SpinGosched_spins=2               461.60 ns/op   (n=5)
BenchmarkBackoff/TIGHT/SpinGosched_spins=4               482.70 ns/op   (n=5)
BenchmarkBackoff/TIGHT/MutexTB_ref                       362.40 ns/op   (n=5)
```

**PAUSE-backoff через runtime.procyield** (`pause.txt`):

```
BenchmarkPauseBackoff/LOOSE/MutexTB_ref                359.60 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/MutexGCRA_ref              362.20 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/GCRA_plain_ref             672.80 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/GCRA_gosched1_ref          186.40 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/pause=1                    352.60 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/pause=4                    392.50 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/pause=16                   462.70 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/pause=30                   370.40 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/pause=64                   232.80 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/pause=4_exp                409.30 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/pause=30_yield_after4      397.70 ns/op   (n=5)
BenchmarkPauseBackoff/LOOSE/pause=30_yield_after1      200.70 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/MutexTB_ref                356.10 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/MutexGCRA_ref              363.40 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/GCRA_plain_ref             674.50 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/GCRA_gosched1_ref          187.30 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/pause=1                    420.50 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/pause=4                    434.40 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/pause=16                   450.60 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/pause=30                   377.10 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/pause=64                   230.70 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/pause=4_exp                395.90 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/pause=30_yield_after4      387.60 ns/op   (n=5)
BenchmarkPauseBackoff/TIGHT/pause=30_yield_after1      197.70 ns/op   (n=5)
```

**Свип GOMAXPROCS по реализациям** (`procs.txt`):

```
BenchmarkProcsSweep/P=1/MutexTB           126.10 ns/op   (n=5)
BenchmarkProcsSweep/P=1/CASPtrTB          207.80 ns/op   (n=5)
BenchmarkProcsSweep/P=1/GCRA              111.30 ns/op   (n=5)
BenchmarkProcsSweep/P=1/GCRATTAS           98.69 ns/op   (n=5)
BenchmarkProcsSweep/P=1/GCRAGosched       102.00 ns/op   (n=5)
BenchmarkProcsSweep/P=2/MutexTB           141.60 ns/op   (n=5)
BenchmarkProcsSweep/P=2/CASPtrTB          497.70 ns/op   (n=5)
BenchmarkProcsSweep/P=2/GCRA              335.60 ns/op   (n=5)
BenchmarkProcsSweep/P=2/GCRATTAS          311.60 ns/op   (n=5)
BenchmarkProcsSweep/P=2/GCRAGosched       226.50 ns/op   (n=5)
BenchmarkProcsSweep/P=4/MutexTB           228.90 ns/op   (n=5)
BenchmarkProcsSweep/P=4/CASPtrTB          769.50 ns/op   (n=5)
BenchmarkProcsSweep/P=4/GCRA              495.60 ns/op   (n=5)
BenchmarkProcsSweep/P=4/GCRATTAS          503.60 ns/op   (n=5)
BenchmarkProcsSweep/P=4/GCRAGosched       261.20 ns/op   (n=5)
BenchmarkProcsSweep/P=8/MutexTB           324.90 ns/op   (n=5)
BenchmarkProcsSweep/P=8/CASPtrTB         1194.00 ns/op   (n=5)
BenchmarkProcsSweep/P=8/GCRA              872.60 ns/op   (n=5)
BenchmarkProcsSweep/P=8/GCRATTAS          861.30 ns/op   (n=5)
BenchmarkProcsSweep/P=8/GCRAGosched       314.90 ns/op   (n=5)
BenchmarkProcsSweep/P=16/MutexTB          367.50 ns/op   (n=5)
BenchmarkProcsSweep/P=16/CASPtrTB        1285.00 ns/op   (n=5)
BenchmarkProcsSweep/P=16/GCRA             975.40 ns/op   (n=5)
BenchmarkProcsSweep/P=16/GCRATTAS         974.10 ns/op   (n=5)
BenchmarkProcsSweep/P=16/GCRAGosched      299.00 ns/op   (n=5)
```

**Свип GOMAXPROCS: победитель против mutex и шардов** (`winner.txt`):

```
BenchmarkWinnerProcsSweep/P=1/MutexTB             107.30 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=1/MutexGCRA            99.86 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=1/GCRA_plain           84.10 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=1/GCRA_gosched1        85.22 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=1/GCRA_perP16          93.60 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=1/GCRA_rand16         110.00 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=2/MutexTB             137.00 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=2/MutexGCRA           132.90 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=2/GCRA_plain          337.20 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=2/GCRA_gosched1       207.70 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=2/GCRA_perP16          47.39 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=2/GCRA_rand16         111.50 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=4/MutexTB             249.90 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=4/MutexGCRA           188.70 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=4/GCRA_plain          496.70 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=4/GCRA_gosched1       201.40 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=4/GCRA_perP16          23.99 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=4/GCRA_rand16          74.62 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=8/MutexTB             321.00 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=8/MutexGCRA           314.40 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=8/GCRA_plain          741.70 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=8/GCRA_gosched1       188.10 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=8/GCRA_perP16          12.52 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=8/GCRA_rand16          49.49 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=16/MutexTB            348.70 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=16/MutexGCRA          361.10 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=16/GCRA_plain         674.20 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=16/GCRA_gosched1      201.00 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=16/GCRA_perP16          8.81 ns/op   (n=5)
BenchmarkWinnerProcsSweep/P=16/GCRA_rand16         41.23 ns/op   (n=5)
```

**Свип GOMAXPROCS: общий лимитер против per-P шардов** (`shardedprocs.txt`):

```
BenchmarkShardedProcsSweep/P=1/MutexTB_shared       114.90 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=1/GCRA_shared           90.62 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=1/GCRA_perP            102.40 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=1/MutexTB_perP         124.90 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=2/MutexTB_shared       140.30 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=2/GCRA_shared          328.20 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=2/GCRA_perP             47.50 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=2/MutexTB_perP          57.99 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=4/MutexTB_shared       224.90 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=4/GCRA_shared          460.90 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=4/GCRA_perP             24.93 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=4/MutexTB_perP          30.56 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=8/MutexTB_shared       324.80 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=8/GCRA_shared          652.50 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=8/GCRA_perP             12.67 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=8/MutexTB_perP          15.48 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=16/MutexTB_shared      355.80 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=16/GCRA_shared         613.90 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=16/GCRA_perP             9.32 ns/op   (n=5)
BenchmarkShardedProcsSweep/P=16/MutexTB_perP         10.73 ns/op   (n=5)
```

**Свип по числу шардов k** (`sharded.txt`):

```
BenchmarkSharded/LOOSE/GCRA_rand/k=1                195.00 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand_unpadded/k=1       200.00 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_perP/k=1                193.00 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_rand/k=1             384.10 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_perP/k=1             364.00 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand/k=2                215.40 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand_unpadded/k=2       185.60 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_perP/k=2                189.20 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_rand/k=2             311.80 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_perP/k=2             185.80 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand/k=4                130.40 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand_unpadded/k=4       183.30 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_perP/k=4                110.40 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_rand/k=4             230.20 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_perP/k=4              87.68 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand/k=8                 70.37 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand_unpadded/k=8       150.70 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_perP/k=8                 44.45 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_rand/k=8             125.60 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_perP/k=8              29.91 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand/k=16                41.28 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand_unpadded/k=16       94.86 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_perP/k=16                 9.13 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_rand/k=16             82.50 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_perP/k=16             10.70 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand/k=32                31.50 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand_unpadded/k=32       64.07 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_perP/k=32                 9.33 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_rand/k=32             60.45 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_perP/k=32             10.70 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand/k=64                26.05 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_rand_unpadded/k=64       42.95 ns/op   (n=5)
BenchmarkSharded/LOOSE/GCRA_perP/k=64                 9.14 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_rand/k=64             44.14 ns/op   (n=5)
BenchmarkSharded/LOOSE/MutexTB_perP/k=64             10.22 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand/k=1                194.70 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand_unpadded/k=1       198.60 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_perP/k=1                195.30 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_rand/k=1             380.50 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_perP/k=1             363.40 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand/k=2                216.00 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand_unpadded/k=2       183.10 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_perP/k=2                155.70 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_rand/k=2             310.20 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_perP/k=2             180.70 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand/k=4                128.90 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand_unpadded/k=4       178.80 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_perP/k=4                116.70 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_rand/k=4             223.40 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_perP/k=4              89.97 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand/k=8                 69.50 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand_unpadded/k=8       147.80 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_perP/k=8                 44.15 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_rand/k=8             123.40 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_perP/k=8              29.26 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand/k=16                41.29 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand_unpadded/k=16       95.54 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_perP/k=16                 9.34 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_rand/k=16             81.52 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_perP/k=16             10.67 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand/k=32                31.47 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand_unpadded/k=32       65.35 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_perP/k=32                 9.55 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_rand/k=32             65.86 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_perP/k=32             11.20 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand/k=64                27.20 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_rand_unpadded/k=64       43.84 ns/op   (n=5)
BenchmarkSharded/TIGHT/GCRA_perP/k=64                 9.85 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_rand/k=64             50.85 ns/op   (n=5)
BenchmarkSharded/TIGHT/MutexTB_perP/k=64             11.35 ns/op   (n=5)
```

**sync.Pool как селектор шарда; стоимость procyield** (`pool.txt`):

```
BenchmarkPoolSharded/TIGHT/pool/k=4          100.40 ns/op   (n=5)
BenchmarkPoolSharded/TIGHT/procPin/k=4        96.78 ns/op   (n=5)
BenchmarkPoolSharded/TIGHT/rand/k=4          134.60 ns/op   (n=5)
BenchmarkPoolSharded/TIGHT/pool/k=16          23.64 ns/op   (n=5)
BenchmarkPoolSharded/TIGHT/procPin/k=16        8.98 ns/op   (n=5)
BenchmarkPoolSharded/TIGHT/rand/k=16          41.45 ns/op   (n=5)
BenchmarkPoolSharded/TIGHT/pool/k=64          13.65 ns/op   (n=5)
BenchmarkPoolSharded/TIGHT/procPin/k=64        9.02 ns/op   (n=5)
BenchmarkPoolSharded/TIGHT/rand/k=64          26.71 ns/op   (n=5)
BenchmarkPoolSelector/syncPool_GetPut          4.59 ns/op   (n=5)
BenchmarkPoolSelector/procyield_1             13.55 ns/op   (n=5)
BenchmarkPoolSelector/procyield_30           351.30 ns/op   (n=5)
```

**Гипотеза о падинге: plain / localcfg / padded** (`padding.txt`):

```
BenchmarkPaddingHypothesis/LOOSE/GCRA_plain               560.60 ns/op   (n=5)
BenchmarkPaddingHypothesis/LOOSE/GCRA_localcfg            198.30 ns/op   (n=5)
BenchmarkPaddingHypothesis/LOOSE/GCRA_padded              835.10 ns/op   (n=5)
BenchmarkPaddingHypothesis/LOOSE/GCRA_padded_gosched      183.00 ns/op   (n=5)
BenchmarkPaddingHypothesis/LOOSE/GCRA_gosched1            199.50 ns/op   (n=5)
BenchmarkPaddingHypothesis/LOOSE/MutexTB_ref              361.10 ns/op   (n=5)
BenchmarkPaddingHypothesis/TIGHT/GCRA_plain               576.20 ns/op   (n=5)
BenchmarkPaddingHypothesis/TIGHT/GCRA_localcfg            200.70 ns/op   (n=5)
BenchmarkPaddingHypothesis/TIGHT/GCRA_padded              875.20 ns/op   (n=5)
BenchmarkPaddingHypothesis/TIGHT/GCRA_padded_gosched      182.50 ns/op   (n=5)
BenchmarkPaddingHypothesis/TIGHT/GCRA_gosched1            185.20 ns/op   (n=5)
BenchmarkPaddingHypothesis/TIGHT/MutexTB_ref              345.50 ns/op   (n=5)
```

**То же по GOMAXPROCS** (`paddingprocs.txt`):

```
BenchmarkPaddingProcsSweep/P=1/MutexTB                   112.20 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=1/GCRA_plain                 90.95 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=1/GCRA_padded                89.15 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=1/GCRA_padded_gosched        90.19 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=2/MutexTB                   137.50 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=2/GCRA_plain                292.00 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=2/GCRA_padded               307.10 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=2/GCRA_padded_gosched       165.40 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=4/MutexTB                   207.10 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=4/GCRA_plain                374.70 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=4/GCRA_padded               399.50 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=4/GCRA_padded_gosched       170.00 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=8/MutexTB                   328.90 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=8/GCRA_plain                512.20 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=8/GCRA_padded               752.90 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=8/GCRA_padded_gosched       184.50 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=16/MutexTB                  378.10 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=16/GCRA_plain               605.70 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=16/GCRA_padded              819.50 ns/op   (n=5)
BenchmarkPaddingProcsSweep/P=16/GCRA_padded_gosched      182.50 ns/op   (n=5)
```


---

## Источники

**Исходники Go 1.23.4** (пути от `$GOROOT/src`; онлайн —
`https://github.com/golang/go/blob/go1.23.4/src/<путь>`):

- `sync/mutex.go` — `Mutex.Lock` (:83), `lockSlow` (:119), `Unlock` (:214),
  `unlockSlow` (:229), константы режимов (:47-78);
- `runtime/proc.go` — `sync_runtime_canSpin` (:7149), `sync_runtime_doSpin`
  (:7176), `procPin`/`procUnpin` с линкнейм-пометкой и «hall of shame»
  (:7074-7106);
- `runtime/lock_futex.go` — `active_spin = 4`, `active_spin_cnt = 30`,
  `passive_spin = 1` (:30-32);
- `runtime/asm_amd64.s` — `procyield` как цикл `PAUSE` (:803-809);
- `runtime/stubs.go` — `//go:linkname procyield` и «hall of shame» (:268-279);
- `runtime/sema.go` — `semTable` с падингом на кэш-линию (:46-58),
  `semacquire1` (:132), `semrelease1` с direct G handoff (:193-270);
- `sync/pool.go` — `poolLocal` с падингом на 128 байт (:72-78), `pin` через
  `runtime_procPin` (:202-221);
- `internal/cpu/cpu.go` — `CacheLinePad` / `CacheLinePadSize` (:16-22);
- `math/rand/v2/rand.go` — `//go:linkname runtime_rand runtime.rand` (:258);
- `runtime/rand.go` — `rand()` из per-m chacha8 (:124-149).

**Документация и статьи:**

- The Go Memory Model — <https://go.dev/ref/mem>
- `sync/atomic` package docs — <https://pkg.go.dev/sync/atomic>
- Russ Cox, «Updating the Go Memory Model (Memory Models, Part 3)» —
  <https://research.swtch.com/gomm>
- `golang.org/x/sys/cpu.CacheLinePad` —
  <https://pkg.go.dev/golang.org/x/sys/cpu#CacheLinePad>
- go.dev/issue/67401 (регламент `//go:linkname` в Go 1.23) —
  <https://go.dev/issue/67401>
- go.dev/issue/33747 («sync: Mutex performance collapses with high
  concurrency», привело к direct G handoff) — <https://github.com/golang/go/issues/33747>
- CL 34310 «sync: make Mutex more fair» (Дмитрий Вьюков, Go 1.9) —
  <https://github.com/golang/go/commit/0556e26273f704db73df9e7c4c3d2e8434dec7be>,
  <https://go-review.googlesource.com/c/go/+/34310>
- golang-dev, «Atomic pointer operations and unsafe» (Вьюков про масштабируемую
  синхронизацию и `unsafe`) —
  <https://groups.google.com/g/golang-dev/c/SBmIen68ys0>

**Аппаратная часть:**

- Joe Duffy, «Some performance implications of CAS operations» (≈20 циклов
  uncontended, ≈200 contended) —
  <https://joeduffyblog.com/2009/01/08/some-performance-implications-of-cas-operations/>
- Intel: рост задержки `PAUSE` с ~10 до ~140 циклов в Skylake; Intel 64 and
  IA-32 Architectures Optimization Reference Manual, разд. 2.6.4 «Pause Latency
  in Skylake Client Microarchitecture». Вторичный источник с этой ссылкой —
  <https://support.microsoft.com/en-us/help/4527212/long-spin-wait-loops-in-net-framework-on-intel-skylake>
- Agner Fog, instruction tables —
  <https://www.agner.org/optimize/instruction_tables.pdf>
- False sharing (обзор) — <https://en.wikipedia.org/wiki/False_sharing>

**Библиотеки:**

- `uber-go/ratelimit`, `limiter_atomic_int64.go` (GCRA в одном `int64`,
  `prepadding`/`postpadding`) —
  <https://github.com/uber-go/ratelimit/blob/main/limiter_atomic_int64.go>
- `puzpuzpuz/xsync`, `counter.go` (striped counter в духе `LongAdder`,
  per-P через `sync.Pool`) —
  <https://github.com/puzpuzpuz/xsync/blob/main/counter.go>,
  `util.go` (комментарий про 64 vs 128 байт) —
  <https://github.com/puzpuzpuz/xsync/blob/main/util.go>
- `chen3feng/atomiccounter` — <https://github.com/chen3feng/atomiccounter>
- `prometheus/client_golang` issue 677 («Improve counter performance with
  per-CPU sharded values once they are available in Go») —
  <https://github.com/prometheus/client_golang/issues/677>
- Doorman (global distributed client-side rate limiting, лизы + апортионирование)
  — <https://github.com/youtube/doorman>
- Envoy global rate limiting (внешний gRPC-сервис + локальный лимитер как
  первая ступень) —
  <https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting>
