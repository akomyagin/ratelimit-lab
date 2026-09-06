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
