# C2 — Go lock-free вглубь

> Статус: **в работе** (файл пишется по ходу исследования).
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
| `atomic.Int64.Load` | 1.24 нс | 0.25 нс/оп (агрегат) | масштабируется линейно |
| `c++` (неатомарный) | 1.31 нс | — | — |
| `atomic.Int64.Add` | 21.7 нс | 43.0 нс | 2.0× |
| `atomic.Int64.Store` | 25.1 нс | — | — |
| `CAS` без ретрая (Load+CAS) | 39.0 нс | 74.3 нс | 1.9× |
| **`CAS` с ретраем** до успеха | 39.0 нс | **221.1 нс** | **5.7×** |
| `sync.Mutex` + `c++` | — | 286.6 нс | — |

Три вывода, которые стоят всей таблицы:

1. **Читать разделяемую линию бесплатно.** `Load` под 16 горутинами даёт
   0.25 нс/оп — линия живёт в S-состоянии во всех L1 одновременно, трафика
   когерентности нет вообще. Это ключ к TTAS и к тому, почему шардирование
   на чтение не нужно.
2. **Пишущий доступ к разделяемой линии не масштабируется, но и не
   катастрофичен сам по себе**: `Add` 21.7 → 43.0 нс, CAS-без-ретрая
   39.0 → 74.3 нс. Это «всего» 2×. Одна операция записи в линию под 16 ядрами
   стоит примерно вдвое дороже, чем под одним.
3. **Катастрофу делает ретрай.** Тот же CAS, но в петле до успеха, — 221 нс
   против 74 нс без ретрая, то есть **≈3 бесполезных витка на каждый
   полезный**. И это на теле петли из двух инструкций; в лимитере тело
   длиннее, и коэффициент растёт.

Оговорка про абсолютные значения: губернатор частоты на стенде — `powersave`
(`intel_pstate`), в простое 1.1 ГГц, под нагрузкой машина бустится. Поэтому
пересчёт нс→циклы ненадёжен; **сравнивать между собой строки одной таблицы
можно, переводить в циклы — нет**. Также на CPU включён полный набор
митигаций (Enhanced IBRS, MDS clear и т.д.), что объясняет, почему
uncontended `Add` = 21.7 нс выглядит дороже «учебных» 20 циклов.

### 2.2. Почему uncontended атомик всё же не бесплатен

21.7 нс на неконкурентный `Add` против 1.3 нс на обычный `c++` — это 16×.
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
| `struct{ v atomic.Int64 }` — плотно, 8 байт на ячейку | **34.66** | 14.3× хуже |
| `struct{ v atomic.Int64; _ [56]byte }` — 64 байта | **2.43** | базовая |
| `struct{ v atomic.Int64; _ [120]byte }` — 128 байт | 2.58 | 1.06× |

**14× разницы на пустом месте** — это и есть цена false sharing на этой машине.
128-байтовое выравнивание здесь ничего не добавило (в пределах шума даже чуть
хуже) — но это одна конкретная машина (одна NUMA-нода, Rocket Lake); на
многосокетных системах, как отмечает xsync, выигрыш может быть ~30%. **Не
проверено** на многосокетном железе.

Отдельно: 2.43 нс/оп на выровненном варианте против 21.7 нс на uncontended
`Add` в одной горутине — потому что 2.43 это агрегат по 16 потокам
(16 × ~39 нс латентности / 16). То есть выровненные счётчики масштабируются
почти линейно, невыровненные — не масштабируются вообще.

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
за единицы наносекунд. На стенде я намерил его в **2.5 нс/оп** под 16
горутинами (против 1.1 нс у `procPin`).

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

Как с этим живут реальные системы:

- **Distributed rate limiting** — ровно тот же приём, но между машинами.
  Классический подход, описанный в статье Stripe о rate limiter'ах и в
  инженерных блогах: делить квоту между узлами и периодически перебалансировать
  (<https://stripe.com/blog/rate-limiters>).
- **Envoy / global rate limiting** — противоположный выбор: вынести решение в
  общий сервис (`ratelimit`), заплатив сетевым RTT за точность
  (<https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting>).
- **Периодическая перебалансировка** — шард, исчерпавший квоту, «занимает» у
  общего пула; так работают схемы вроде max-min fair share в Doorman
  (<https://github.com/youtube/doorman>). Ценой становится ещё одна
  разделяемая структура — то есть частичный возврат к исходной проблеме.
- **Work stealing между шардами** — компромисс: при отказе своего шарда
  попробовать соседний (`xsync.Counter` делает ровно это, но для счётчика:
  «Give a try with another randomly selected stripe»). Для лимитера это
  восстанавливает утилизацию ценой обхода нескольких линий на пути отказа —
  а путь отказа как раз тот, который мы не меряем в `ratelimit-lab`.

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

### 5.3. Результат: `runtime.Gosched()` после каждого проигранного CAS

Это оказалось единственной работающей стратегией, и работает она хорошо.
Числа и обсуждение — в разделе «Прямой ответ» и «Собственные замеры».

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
