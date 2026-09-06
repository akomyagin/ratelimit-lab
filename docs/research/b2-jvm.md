# B2 — JVM-экосистема

> Статус: **в работе** (черновик пополняется по ходу исследования).
> Тема: как JVM-библиотеки rate limiting решают задачи, актуальные для
> `ratelimit-lab` — CAS на `AtomicReference`, представление состояния,
> аллокации в петле, множественные лимиты, абстракция часов.

## Bucket4j

Версия по исходникам: **8.19.0** (`pom.xml`, `<artifactId>bucket4j_jdk17</artifactId>`),
разбор по коммиту `92553c54b47d7308095ed4c438a7a170371a400f` (2 июля 2026), ветка `master`.
Лицензия Apache-2.0. [1]

### Модель Bandwidth и несколько лимитов на одном бакете

Лимит у Bucket4j называется **Bandwidth**. Класс —
`bucket4j-core/src/main/java/io/github/bucket4j/Bandwidth.java`. Поля (строки 46–53):

```java
final long capacity;
final long refillPeriodNanos;
final long refillTokens;
final boolean refillIntervally;
final long timeOfFirstRefillMillis;
final boolean useAdaptiveInitialTokens;
final String id;
```

Бакет содержит **массив** bandwidth: `BucketConfiguration.getBandwidths()` возвращает
`Bandwidth[]`. Все операции над состоянием — циклы по этому массиву
(`BucketState64BitsInteger.refillAllBandwidth`, `.consume`, `.addTokens`, `.reset`).
Семантика комбинирования — жёсткое **AND по минимуму**:

```java
// BucketState64BitsInteger.java:309-315
public long getAvailableTokens() {
    long availableTokens = getCurrentSize(0);
    for (int i = 1; i < configuration.getBandwidths().length; i++) {
        availableTokens = Math.min(availableTokens, getCurrentSize(i));
    }
    return availableTokens;
}
```

а списание — безусловно из **всех** bandwidth сразу
(`BucketState64BitsInteger.consume`, строки 318–322): `for (...) consume(i, toConsume);`.
То есть «100/мин И 1000/час» — это два элемента массива; допуск = минимум доступного,
списание идёт из обоих счётчиков. Задержка до возможности списать — **максимум** по
bandwidth (`calculateDelayNanosAfterWillBePossibleToConsume`, строки 325–334:
`Math.max`).

Стоимость: одна операция — `O(числа bandwidth)`, три `long` на bandwidth в состоянии
(`BANDWIDTH_SIZE = 3`, `BucketState64BitsInteger.java:41`), никаких дополнительных
объектов. `Bandwidth` неизменяем и лежит в конфигурации, а не в состоянии.

Отдельная деталь: `Bandwidth.withId(String id)` — идентификатор нужен для
`replaceConfiguration` c `TokensInheritanceStrategy` (`AS_IS` / `PROPORTIONALLY` /
`ADDITIVE` / `RESET`), чтобы при смене конфигурации сопоставить старые bandwidth с
новыми и перенести накопленные токены (`BucketState64BitsInteger.replaceConfiguration`,
строки 137–186).

### Целочисленное состояние: `BucketState64BitsInteger`

Файл `bucket4j-core/src/main/java/io/github/bucket4j/BucketState64BitsInteger.java`,
662 строки. Состояние — **один плоский `long[]`**, по 3 слота на bandwidth
(строки 598–620):

| Слот | Смысл |
|---|---|
| `stateData[i*3 + 0]` | `lastRefillTimeNanos` |
| `stateData[i*3 + 1]` | `currentSize` — доступные токены, целое |
| `stateData[i*3 + 2]` | `roundingError` — **числитель незавершённой дроби** |

Ядро — метод `refill(int bandwidthIndex, Bandwidth bandwidth, long currentTimeNanos)`
(строки 447–525). Существенный фрагмент:

```java
long roundingError = getRoundingError(bandwidthIndex);
long dividedWithoutError = multiplyExactOrReturnMaxValue(refillTokens, durationSinceLastRefillNanos);
long divided = dividedWithoutError + roundingError;
if (divided < 0 || dividedWithoutError == Long.MAX_VALUE) {
    // arithmetic overflow happens.
    // there is no sense to stay in integer arithmetic when having deal with so big numbers
    long calculatedRefill = (long) ((double) durationSinceLastRefillNanos / (double) refillPeriodNanos * (double) refillTokens);
    newSize += calculatedRefill;
    roundingError = 0;
} else {
    long calculatedRefill = divided / refillPeriodNanos;
    if (calculatedRefill == 0) {
        roundingError = divided;
    } else {
        newSize += calculatedRefill;
        roundingError = divided % refillPeriodNanos;
    }
}
```

Это **точная рациональная арифметика** с фиксированным знаменателем
`refillPeriodNanos`: целая часть уходит в `currentSize`, остаток от деления
переносится в `roundingError` и участвует в следующем refill. Дрейф не «маленький» —
его **нет вообще**, по построению. Ни эпсилона, ни допуска на границе в коде нет.

Дополнительно в целочисленной версии явно ловятся ситуации, которых в float-версии
не бывает, и все они — про **переполнение `long`**, а не про точность:

- `multiplyExactOrReturnMaxValue` (строки ~640–655) — копия `Math.multiplyExact`,
  но вместо исключения возвращает `Long.MAX_VALUE`;
- проверка `if (newSize < currentSize)` после сложения — «arithmetic overflow happens.
  This mean that tokens reached Long.MAX_VALUE tokens. just reset bandwidth state»
  (комментарий в исходнике, встречается дважды: строки ~485 и ~515);
- при переполнении умножения код **сознательно переходит в double** на один шаг и
  обнуляет `roundingError` — то есть double используется как аварийный запасной путь
  для чисел, где точность уже не важна.

Есть и отдельный набор тестов ровно на это: `HandlingArithmeticOverflowSpecification.groovy`
в `bucket4j-core/src/test/`.

### `BucketStateIEEE754` — что это было и почему исчезло

Класс **удалён** и в текущем коде отсутствует. Единственный след — закомментированная
строка в
`bucket4j-core/src/main/java/io/github/bucket4j/distributed/serialization/SerializationHandles.java:42`:

```java
//            BucketStateIEEE754.SERIALIZATION_HANDLE, // 4
```

Удаляющий коммит: `0c072e0edc8208673014e90c8440f79501a644c0`, автор Vladimir Buhtoyarov,
**24 октября 2023**, сообщение `#410 remove experimental code`. Диффстат: `-392` строки
самого класса, `-399` строк `BucketStateSpecification.groovy`, минус ветка `IEEE_754`
из `MathType` и из `BucketState`.

**Главное — это была не альтернатива по точности.** Javadoc в удалённой версии
`MathType.java` (получено через `git show 0c072e0e^:.../MathType.java`):

```java
/**
 * Experimental math precision that uses IEEE-754 arithmetic.
 *
 * <p>
 * <b>Warning: </b> you should not use this precision in production, because intention of this precision is the testing purpose for backends written in Lua or JS,
 * in other words for testing backends that do not provide 64-bit integer arithmetic.
 */
@Experimental
IEEE_754;
```

То есть IEEE754-версия существовала, чтобы **эмулировать бэкенды без 64-битной целой
арифметики** (Lua 5.1, JavaScript `Number`) и проверять, что распределённые скрипты
для них дают тот же результат. Продакшн-путь всегда был целочисленный. Подтверждение
из кода: `LocalBucketBuilder.java:135-137` жёстко зашивает `MathType.INTEGER_64_BITS`
для всех трёх стратегий синхронизации — выбора у пользователя локального бакета не было
и раньше.

Устройство удалённого класса (из `git show 0c072e0e^:...BucketStateIEEE754.java`):

- состояние — `double[] tokens` + `long[] lastRefillTime`, то есть **два слота** на
  bandwidth вместо трёх: слот `roundingError` не нужен;
- `getRoundingError` возвращает константу с комментарием
  `// accumulated computational error is always zero for this type of state` —
  формально верно в том смысле, что отдельного счётчика остатка нет; фактически ошибка
  живёт в мантиссе `double`;
- refill (строки ~230–269): `double calculatedRefill = (double) durationSinceLastRefillNanos / (double) refillPeriodNanos * (double) refillTokens;` — прямое
  деление, ровно наш подход;
- **эпсилона нет**. Вместо него — усечение на выходе:
  `public long getCurrentSize(int bandwidth) { return (long) tokens[bandwidth]; }`
  и `getAvailableTokens()` тоже приводит `(long)`.

Ограничения каждого варианта:

| | `INTEGER_64_BITS` | `IEEE_754` (удалён) |
|---|---|---|
| Дрейф | отсутствует (точный остаток) | накапливается в мантиссе |
| Слотов на bandwidth | 3 `long` | 1 `double` + 1 `long` |
| Верхняя граница | `Long.MAX_VALUE`, переполнение ловится явно | ±2^53 на точную целую часть |
| Портируемость на Lua/JS | нет | да, это и была цель |
| Статус | единственный production-путь | `@Experimental`, удалён в 10.2023 |

### `LockFreeBucket` — CAS-реализация

Файл `bucket4j-core/src/main/java/io/github/bucket4j/local/LockFreeBucket.java`.
Поле — `private final AtomicReference<BucketState> stateRef;`. Канонический вид петли
(`tryConsumeImpl`, строки ~86–106):

```java
protected boolean tryConsumeImpl(long tokensToConsume) {
    BucketState previousState = stateRef.get();
    BucketState newState = previousState.copy();
    long currentTimeNanos = timeMeter.currentTimeNanos();

    while (true) {
        newState.refillAllBandwidth(currentTimeNanos);
        long availableToConsume = newState.getAvailableTokens();
        if (tokensToConsume > availableToConsume) {
            return false;
        }
        newState.consume(tokensToConsume);
        if (stateRef.compareAndSet(previousState, newState)) {
            return true;
        } else {
            previousState = stateRef.get();
            newState.copyStateFrom(previousState);
        }
    }
}
```

Ответы на поставленные вопросы:

- **Аллокация — одна на вызов, не на итерацию.** `previousState.copy()` вынесен
  *перед* циклом. На неудачном CAS объект `newState` **переиспользуется**:
  `newState.copyStateFrom(previousState)` копирует содержимое в уже выделенный объект.
  В `BucketState64BitsInteger.copyStateFrom` это `System.arraycopy` в существующий
  массив, если конфигурация та же (строки ~296–305). Ретраи бесплатны по памяти.
- **Backoff отсутствует.** Ни `Thread.onSpinWait()`, ни yield, ни экспоненциальной
  паузы в файле нет (проверено grep по `onSpinWait|yield|sleep|park` — совпадений нет).
- **Петля неограниченна.** Счётчика ретраев нет; выход только по успешному CAS или по
  раннему `return`.
- **`time.Now()` берётся один раз на вызов**, до цикла, и переиспользуется во всех
  ретраях. То есть при ретрае состояние пересчитывается на *том же* моменте времени.
- **Отказ возвращается без CAS.** Ветка `if (tokensToConsume > availableToConsume) return false;` выходит до `compareAndSet`. Это ровно то же решение, что в нашем
  `lockfree_tokenbucket.go`: refill, вычисленный при отказе, не персистится.
  Аллокация `copy()` при этом всё равно уже сделана — в отличие от нашей версии.
- `estimateAbilityToConsumeImpl` (строки ~124–140) вообще **не содержит петли**: один
  `get()`, `copy()`, refill на копии, ответ. Наблюдение без мутации.

Отдельно `getAvailableTokens()` и `getAvailableTokensVerboseImpl()` тоже read-only:
`stateRef.get().copy()` + refill на копии, без CAS.

### `SynchronizedBucket` — mutex-baseline

Файл `bucket4j-core/src/main/java/io/github/bucket4j/local/SynchronizedBucket.java`.
Несмотря на имя и javadoc `SynchronizationStrategy.SYNCHRONIZED`
(«Blocking strategy based on java `synchronized` keyword»), реально используется
**`java.util.concurrent.locks.ReentrantLock`** (поле `private final Lock lock;`,
конструктор `this(configuration, mathType, timeMeter, listener, new ReentrantLock())`).
Javadoc в этой части устарел.

Микро-деталь, которую стоит отметить: `timeMeter.currentTimeNanos()` вызывается
**до** `lock.lock()`, то есть чтение часов вынесено из критической секции:

```java
protected boolean tryConsumeImpl(long tokensToConsume) {
    long currentTimeNanos = timeMeter.currentTimeNanos();
    lock.lock();
    try {
        state.refillAllBandwidth(currentTimeNanos);
        ...
```

При наших замеренных 73.1 нс на `time.Now()` это существенная доля критической секции.

### Как сами авторы формулируют компромисс

`bucket4j-core/src/main/java/io/github/bucket4j/local/SynchronizationStrategy.java`,
javadoc-и enum-констант — самая прямая формулировка того, что мы измеряли:

> **LOCK_FREE.** Advantages: This strategy is tolerant to high contention usage
> scenario, threads do not block each other. Disadvantages: The sequence
> "read-clone-update-save" needs to allocate one object per each invocation of
> consumption method. Usage recommendations: when you are not sure what kind of
> strategy is better for you.

> **SYNCHRONIZED.** Advantages: Never allocates memory. Disadvantages: Thread which
> acquired the lock (and superseded from CPU by OS scheduler) can block another threads
> for significant time. Usage recommendations: when your primary goal is avoiding of
> memory allocation, and you do not care about contention.

`LOCK_FREE` — дефолт: `LocalBucketBuilder.build()` без параметров даёт его
(`LocalBucketBuilder.java:135`).

Важно: это **утверждение, а не измерение**. Числа рядом с ним не приведены.

### JMH-бенчмарки Bucket4j

Модуль `bucket4j-benchmarks/`. Классы:

- `benchmark/TryConsumeMostlySuccess.java`
- `benchmark/ConsumeMostlySuccess.java`
- `benchmark/BaseLineWithoutSynchronization.java`
- `benchmark/MemoryBenchmark.java`

`TryConsumeMostlySuccess` — прямое кросс-библиотечное сравнение пяти реализаций в одном
прогоне: `LockFree`, `Synchronized`, **Guava `RateLimiter`**, **Resilience4j
`SemaphoreBasedRateLimiter`**, **Resilience4j `AtomicRateLimiter`**. Конфигурация
прогона (метод `benchmark(int threadCount)`): `warmupIterations(10)`,
`measurementIterations(10)`, `forks(1)`, `addProfiler(GCProfiler.class)`, режимы
`Mode.Throughput` и `Mode.AverageTime`, единица `MICROSECONDS`. Готовые точки входа —
вложенные классы `OneThread`, `TwoThreads`, `FourThreads`.

Ключевая деталь методики: бакет намеренно сделан **неисчерпаемым**, чтобы мерить
только путь допуска. `LocalLockFreeState.java`:

```java
public final Bucket unlimitedBucket = Bucket.builder()
        .addLimit(Bandwidth.simple(Long.MAX_VALUE / 2, Duration.ofNanos(Long.MAX_VALUE / 2)))
        .build();
```

и симметрично `RateLimiter.create(Long.MAX_VALUE / 2.0)` для Guava,
`limitForPeriod(Integer.MAX_VALUE / 2)` для Resilience4j. Это независимое
подтверждение того же решения, которое принято в `ratelimit-lab` («бенчмарки меряют
только путь допуска, и это осознанно»).

**Числа не найдены.** В репозитории нет опубликованных результатов прогонов: grep по
`asciidoc/` и `README.md` на `benchmark|ops/|throughput` даёт только одно упоминание
бенчмарков в контексте «когда нужна наносекундная точность часов»
(`asciidoc/src/main/docs/asciidoc/basic/quick-start.adoc:116`) и одно нерелевантное.
Сравнительных таблиц LockFree против Synchronized с цифрами в исходниках нет —
**не проверено**, публиковались ли они где-то вне репозитория.

Замечание по чистоте сравнения: `LocalSynchronizedState` собран с
`.withMillisecondPrecision()` (то есть `TimeMeter.SYSTEM_MILLISECONDS`,
`System.currentTimeMillis()`), а `LocalLockFreeState` — без этого вызова, то есть с
дефолтным `TimeMeter`. Часы у двух сравниваемых реализаций разные — а стоимость чтения
часов у `currentTimeMillis()` и `nanoTime()` различается. Это делает прямое сравнение
этих двух бенчмарков некорректным.

### Полный набор семантик

`bucket4j-core/src/main/java/io/github/bucket4j/Bucket.java`:

| Метод | Строка | Семантика |
|---|---|---|
| `tryConsume(long)` | 70 | неблокирующий всё-или-ничего → `boolean`. Наш `AllowN` |
| `tryConsumeAndReturnRemaining(long)` | 104 | то же + `ConsumptionProbe`: остаток, наносекунды до возможности, наносекунды до полного восстановления |
| `estimateAbilityToConsume(long)` | 113 | «можно ли было бы» **без списания** → `EstimationProbe` |
| `tryConsumeAsMuchAsPossible()` / `(long limit)` | 120 / 130 | частичное потребление, возвращает сколько удалось |
| `consumeIgnoringRateLimits(long)` | 95 | списывает **всегда**, уводя баланс в минус; возвращает величину нарушения в наносекундах |
| `addTokens(long)` / `forceAddTokens(long)` | 155 / 177 | вернуть токены; `force` может превысить capacity |
| `reset()` | 182 | заполнить до capacity |
| `getAvailableTokens()` | 192 | снимок |
| `replaceConfiguration(cfg, strategy)` | 259 | смена лимитов на живом бакете |
| `toListenable(BucketListener)` | 271 | обёртка со счётчиками событий |
| `asBlocking()` / `asScheduler()` / `asVerbose()` | 45 / 54 / 61 | смена «режима» вместо новых методов |

Про `consumeIgnoringRateLimits` в javadoc (строки 82–95) прямо описан сценарий: бакет
10 токенов/сек, доступно 2, вызывают `consumeIgnoringRateLimits(6)` → вернётся
`300_000_000` нс нарушения, баланс станет `-3`, и любой `tryConsume` будет неуспешен
следующие ~400 мс. Это «взять в долг» — полезно, когда стоимость запроса становится
известна только постфактум.

`asBlocking()` → `BlockingBucket`: `tryConsume(numTokens, maxWaitTimeNanos, BlockingStrategy)`,
плюс варианты `...Uninterruptibly`. Дефолтная стратегия — `BlockingStrategy.PARKING`.
`asScheduler()` → `SchedulingBucket`: `CompletableFuture<Boolean> tryConsume(long, long, ScheduledExecutorService)` — вместо блокировки потока планируется
продолжение. Оба построены поверх одного примитива
`reserveAndCalculateTimeToSleepImpl(tokensToConsume, waitIfBusyNanosLimit)`, который
**списывает токены сразу** и возвращает, сколько наносекунд надо подождать.

### Greedy против intervally

`BandwidthBuilder.BandwidthBuilderRefillStage`, javadoc:

> **`refillGreedy(tokens, period)`.** «Configures refill that does refill of tokens in
> greedy manner, it will try to add the tokens to bucket as soon as possible. For
> example refill "10 tokens per 1 second" will add 1 token per each 100 millisecond,
> in other words refill will not wait 1 second to regenerate a bunch of 10 tokens.»

> **`refillIntervally(tokens, period)`.** «"Intervally" in opposite to "greedy" will
> wait until whole `period` will be elapsed before regenerate `tokens`.»

В коде различие — три строки в `refill()`
(`BucketState64BitsInteger.java:452-455`):

```java
if (bandwidth.isRefillIntervally()) {
    long incompleteIntervalCorrection = (currentTimeNanos - previousRefillNanos) % bandwidth.getRefillPeriodNanos();
    currentTimeNanos -= incompleteIntervalCorrection;
}
```

То есть intervally = округлить «сейчас» вниз до границы периода перед пополнением.
Предикат — `Bandwidth.isRefillIntervally()` / `isGready()` (опечатка в имени — их).

Есть третий вариант: `refillIntervallyAligned(tokens, period, timeOfFirstRefill)` —
привязка границы интервала к стенным часам (начало часа/суток), плюс
`useAdaptiveInitialTokens`, где начальное число токенов считается по формуле из
javadoc: `Math.min(capacity, Math.max(0, bandwidthCapacity - refillTokens) + (timeOfFirstRefillMillis - nowMillis)/refillPeriod * refillTokens)`. Это ровно та
семантика, которую ждут от «1000 запросов в календарные сутки».

Наш `ratelimit-lab` реализует только greedy — и token bucket, и leaky bucket
пополняются непрерывно.

### `TimeMeter` против нашего `Clock`

`bucket4j-core/src/main/java/io/github/bucket4j/TimeMeter.java` — интерфейс из
**двух** методов:

```java
long currentTimeNanos();
boolean isWallClockBased();
```

Две реализации-константы: `TimeMeter.SYSTEM_NANOTIME` (`System.nanoTime()`,
`isWallClockBased() == false`) и `TimeMeter.SYSTEM_MILLISECONDS`
(`System.currentTimeMillis()` → наносекунды, `isWallClockBased() == true`).
Дефолт для локального бакета — `SYSTEM_MILLISECONDS`; `SYSTEM_NANOTIME` включается
через `.withNanosecondPrecision()`.

Отличия от нашего `Clock`:

1. **Возвращается `long` наносекунд, а не структура времени.** У нас снапшот хранит
   `time.Time` (24 байта, с указателем на `*Location`), у них — примитив. Для
   упакованного в один `atomic.Int64` состояния наш `time.Time` был бы невозможен —
   и GCRA-эксперимент это уже показал.
2. **`isWallClockBased()` — предикат, а не деталь.** Он определяет корректность в
   распределённом сценарии; и он же используется как гейт сериализации:
   `LockFreeBucket.SERIALIZATION_HANDLE.serialize` бросает
   `NotSerializableException("Only TimeMeter.SYSTEM_MILLISECONDS can be serialized safely")`, если часы не стенные. У нас аналога нет и в single-process не нужен.
3. Тестовый двойник — `bucket4j-core/src/test/java/io/github/bucket4j/mock/TimeMeterMock.java`,
   то есть та же идея, что наш `fakeClock` в `clock_test.go`.

### Прочее, замеченное по коду

- `ThreadUnsafeBucket` (`local/ThreadUnsafeBucket.java`) — третья стратегия `NONE`:
  без синхронизации вообще, для случая, когда внешний код уже гарантирует
  однопоточность. Javadoc честно пишет: «If your code or third-party library code has
  errors then bucket state will be corrupted».
- `lincheck-tests/` — модуль с формальной проверкой линеаризуемости (Lincheck), но в
  нём **только** тесты батчинга распределённых команд
  (`BatchingExecutorLincheckTest.kt`, `BatchingAsyncExecutorLincheckTest.kt`).
  `LockFreeBucket` под Lincheck **не проверяется**.
- `bucket4j-core/src/test/java/io/github/bucket4j/local/LockFreeBucketLayout.java` —
  судя по имени, проверка раскладки объекта в памяти (JOL); содержимое не разбиралось.

## Resilience4j RateLimiter

Разбор по коммиту `a8a33164256ddb6e4bbc168f5c47ac34a348ee7f` (31 августа 2026),
ветка `master`. Номер релизной версии в репозитории не зашит (`build.gradle:16`:
`version = version == "unspecified" ? "0.1.0-SNAPSHOT" : version` — версия подаётся
снаружи при сборке), поэтому **точную версию назвать не могу**. Лицензия Apache-2.0. [2]

Файл: `resilience4j-ratelimiter/src/main/java/io/github/resilience4j/ratelimiter/internal/AtomicRateLimiter.java`,
504 строки.

### Модель «циклов» вместо непрерывного пополнения

Классовый javadoc (строки 39–47) формулирует модель прямо:

> `AtomicRateLimiter` splits all nanoseconds from the start of epoch into cycles. Each
> cycle has duration of `RateLimiterConfig#getLimitRefreshPeriod` in nanoseconds. By
> contract on start of each cycle `AtomicRateLimiter` should set `State#activePermissions`
> to `RateLimiterConfig#getLimitForPeriod`. For the `AtomicRateLimiter` callers it is
> really looks so, but under the hood there is some optimisations that will skip this
> refresh if `AtomicRateLimiter` is not used actively.

Ядро — `calculateNextState` (строки 226–250):

```java
long currentNanos = currentNanoTime();
long currentCycle = currentNanos / cyclePeriodInNanos;

long nextCycle = activeState.activeCycle;
int nextPermissions = activeState.activePermissions;
if (nextCycle != currentCycle) {
    long elapsedCycles = currentCycle - nextCycle;
    long accumulatedPermissions = elapsedCycles * permissionsPerCycle;
    nextCycle = currentCycle;
    nextPermissions = (int) min(nextPermissions + accumulatedPermissions, permissionsPerCycle);
}
```

Что это упрощает:

1. **Арифметика целиком целочисленная.** `activeCycle` — `long`, `activePermissions` —
   `int`, всё пополнение — умножение `elapsedCycles * permissionsPerCycle`. Ни одного
   `double` в состоянии. Задачи «дрейф на границе допуска» просто не существует —
   **эпсилон не нужен и его нет**.
2. **Нет частичных токенов.** Между границами циклов баланс не меняется вообще. Отсюда
   и «оптимизация», о которой пишет javadoc: пересчёт делается лениво, по разнице
   номеров циклов, а не тикающим таймером.
3. **Точное время нужно только для деления на цикл**, `currentNanos / cyclePeriodInNanos`.
   Момент внутри цикла влияет только на расчёт «сколько ждать»
   (`nanosToWaitForPermission`, строки 264–276), а не на баланс.

Цена: это **не** token bucket, а **fixed window** со скользящим по эпохе выравниванием.
Burst-семантика другая: `min(nextPermissions + accumulated, permissionsPerCycle)` —
накопление ограничено одним периодом, «одолжить» вперёд нельзя, а на стыке двух циклов
возможен классический пик 2× `limitForPeriod`. Наш token bucket от этого свободен.

Часы инжектируются как `Supplier<Long> nanoTimeSupplier` (конструктор, строка 70;
дефолт — `System::nanoTime`) и всегда пересчитываются в относительное время от старта
лимитера: `currentNanoTime() { return this.nanoTimeSupplier.get() - nanoTimeStart; }`
(строки 115–117). Абстракция беднее нашей `Clock`, но её достаточно и она не требует
интерфейса.

### Аллокации: да, объект `State` создаётся на каждой попытке

Прямой ответ на главный вопрос. Состояние — `private static class State` (строки 421–441),
неизменяемое, 4 поля: `RateLimiterConfig config`, `long activeCycle`,
`int activePermissions`, `long nanosToWait`.

Петля (`updateStateWithBackOff`, строки 178–186):

```java
private State updateStateWithBackOff(final int permits, final long timeoutInNanos) {
    AtomicRateLimiter.State prev;
    AtomicRateLimiter.State next;
    do {
        prev = state.get();
        next = calculateNextState(permits, timeoutInNanos, prev);
    } while (!compareAndSet(prev, next));
    return next;
}
```

`calculateNextState` в конце вызывает `reservePermissions`, который **безусловно**
завершается конструктором (строки 428–441 → строка ~434):

```java
return new State(config, cycle, permissionsWithReservation, nanosToWait);
```

Следствия, все три существенные:

- **Аллокация на каждый вызов** — как у Bucket4j;
- **и дополнительно на каждую итерацию ретрая**, потому что `calculateNextState` стоит
  *внутри* `do-while`. Bucket4j этого избежал переиспользованием буфера
  (`copyStateFrom`); Resilience4j — нет, потому что их `State` неизменяем и
  переиспользовать нечего;
- **отказ тоже проходит через CAS.** В `reservePermissions` при `!canAcquireInTime`
  разрешения не списываются, но новый `State` (с обновлённым `activeCycle`) всё равно
  публикуется. Раннего `return false` до CAS, как у Bucket4j и у нас, здесь нет.

То есть избежать аллокации им **не удалось** — они пошли другим путём: сделали объект
состояния маленьким (4 поля, в TLAB это дёшево на JVM) и добавили backoff, чтобы ретраев
было меньше.

### Backoff: есть, и со ссылкой на статью

В отличие от Bucket4j, здесь backoff явный (строки 209–215):

```java
private boolean compareAndSet(final State current, final State next) {
    if (state.compareAndSet(current, next)) {
        return true;
    }
    parkNanos(1); // back-off
    return false;
}
```

Javadoc метода (строки 170–177 и 188–208) обосновывает:

> It differs from `AtomicReference#updateAndGet(UnaryOperator)` by constant back off. It
> means that after one try to `AtomicReference#compareAndSet(Object, Object)` this method
> will wait for a while before try one more time. This technique was originally described
> in this [paper](https://arxiv.org/abs/1305.5800) and showed great results with
> `AtomicRateLimiter` in benchmark tests.

Ссылка ведёт на arXiv:1305.5800. [3] Backoff — **константный**, `LockSupport.parkNanos(1)`,
не экспоненциальный. Ограничения по числу ретраев нет — петля неограниченна, как у
Bucket4j.

Это готовая гипотеза под наш замер «CAS+Pointer 1473 нс на parallel-16»: у нас в петле
backoff-а нет вообще. Go-аналог `parkNanos(1)` — `runtime.Gosched()` или пустой цикл
с `runtime.procyield`; прямого эквивалента «припарковаться на наносекунду» в Go нет.

### `SemaphoreBasedRateLimiter` — альтернатива

Файл `.../internal/SemaphoreBasedRateLimiter.java`. Устройство (javadoc, строки 42–44):
`java.util.concurrent.Semaphore` **плюс** `ScheduledExecutorService`, который по
расписанию возвращает разрешения:

```java
this.semaphore = new Semaphore(this.rateLimiterConfig.get().getLimitForPeriod(), true);   // fair
...
scheduler.scheduleAtFixedRate(this::refreshLimit, ...);
```

Разница по свойствам:

| | `AtomicRateLimiter` | `SemaphoreBasedRateLimiter` |
|---|---|---|
| Состояние | `AtomicReference<State>` + CAS | `Semaphore(fair=true)` |
| Пополнение | лениво, при обращении | активно, фоновым планировщиком |
| Ресурсы | никаких | отдельный поток-планировщик; нужен `shutdown()`, иначе объект не собирается GC (javadoc, строки 266–268) |
| `reservePermission` | поддержан | **не поддержан**, javadoc: «Reserving permissions is not supported in the semaphore based implementation» (строка 182) |
| Справедливость | нет | FIFO по контракту `Semaphore(fair=true)` |

Для нас содержательно другое: **семафор — это в принципе иной примитив**, чем
token bucket. Он моделирует конкурентность (сколько одновременно), а не темп, и
превращается в rate limiter только за счёт внешнего таймера. Тот же приём — основа
Netflix `concurrency-limits` (ниже).

### Прочие возможности

`RateLimiterConfig` (строки 40–44): `timeoutDuration` (сколько вызывающий готов ждать
разрешения — ключевое отличие от нашего неблокирующего порта), `limitRefreshPeriod`,
`limitForPeriod`, `writableStackTraceEnabled` (можно отключить заполнение stack trace
у `RequestNotPermitted` — экономия на горячем пути отказа) и
`drainPermissionsOnResult` — предикат `Predicate<Either<? extends Throwable, ?>>`,
по которому после вызова можно **сбросить все накопленные разрешения**. Типичный
сценарий — увидели HTTP 429 от апстрима и разом обнулили локальный бюджет.
Минимально допустимый период — `ACCEPTABLE_REFRESH_PERIOD = Duration.ofNanos(1L)`
(строка 37).

Семантики порта: `acquirePermission(int permits)` → `boolean` (может **блокировать** до
`timeoutDuration`), `reservePermission(int permits)` → `long` наносекунд (0 — можно
сразу, положительное — столько ждать, `-1` — не успеть; резервирование уже сделано,
`activePermissions` уходит в минус), `drainPermissions()`. Плюс большой слой
декораторов в `RateLimiter.java`: `decorateSupplier`, `decorateCheckedRunnable`,
`decorateCompletionStage`, `decorateFuture`, `decorateFunction`, включая варианты с
`permitsCalculator` — функцией, вычисляющей стоимость запроса из его аргумента.

**Метрики** — `Metrics#getNumberOfWaitingThreads()` (счётчик `AtomicInteger waitingThreads`,
инкрементируется в `waitForPermission`) и `getAvailablePermissions()`. Важная деталь
реализации `AtomicRateLimiterMetrics` (строки 460–498): метрики считаются как
**`calculateNextState(1, -1, currentState)` без публикации результата** — то есть
чистая функция переиспользована как «что было бы», аналог `estimateAbilityToConsume`
у Bucket4j. Отдельного кода для метрик нет.

**События** — `RateLimiterEvent.Type`: `SUCCESSFUL_ACQUIRE`, `FAILED_ACQUIRE`, `DRAINED`
(`event/RateLimiterEvent.java:36-39`). Публикация защищена проверкой
`if (!eventProcessor.hasConsumers()) return;` (строка 414) — без подписчиков горячий
путь не платит ничего.

### JMH-бенчмарк Resilience4j

`resilience4j-ratelimiter/src/jmh/java/io/github/resilience4j/ratelimiter/RateLimiterBenchmark.java`.
Сравниваются **только две их собственные реализации**, чужих библиотек нет.
Параметры: `FORK_COUNT = 2`, `WARMUP_COUNT = 10`, `ITERATION_COUNT = 10`,
`THREAD_COUNT = 2`, `@BenchmarkMode(Mode.All)`, `@OutputTimeUnit(MICROSECONDS)`,
профайлер `GCProfiler`. Конфигурация лимитера в `@Setup`:
`limitForPeriod(Integer.MAX_VALUE)`, `limitRefreshPeriod(Duration.ofNanos(10))`,
`timeoutDuration(Duration.ofSeconds(5))` — снова «неисчерпаемый» лимитер, то есть
меряется только путь допуска. Полезная нагрузка — `Blackhole.consumeCPU(1)`.

**Числа не найдены**: результатов прогона в репозитории нет. Единственное утверждение о
результатах — фраза «showed great results with `AtomicRateLimiter` in benchmark tests» в
javadoc `compareAndSet`, без цифр. **Не проверено**, публиковались ли числа вне репозитория.

## Guava RateLimiter

Разбор по коммиту `5fb424c43ae38bad86b18841954337af791b6685` (4 сентября 2026),
ветка `master`, файлы
`guava/src/com/google/common/util/concurrent/RateLimiter.java` (498 строк) и
`SmoothRateLimiter.java` (394 строки). Лицензия Apache-2.0. [4]

### Не lock-free вообще

Вопреки распространённому мнению, `RateLimiter` в Guava — **mutex-based**, причём на
`synchronized` по лениво создаваемому объекту (`RateLimiter.java:221-234`):

```java
private volatile @Nullable Object mutexDoNotUseDirectly;

private Object mutex() {
  Object mutex = mutexDoNotUseDirectly;
  if (mutex == null) {
    synchronized (this) {
      mutex = mutexDoNotUseDirectly;
      if (mutex == null) {
        mutexDoNotUseDirectly = mutex = new Object();
      }
    }
  }
  return mutex;
}
```

Это double-checked locking ради того, чтобы не публиковать монитор наружу (иначе чужой
код мог бы синхронизироваться на самом `RateLimiter`). Все четыре точки входа —
`setRate`, `getRate`, `reserve`, `tryAcquire` — берут `synchronized (mutex())`.
Точность — **микросекунды**, не наносекунды; в шапке класса висит нерешённый
`// TODO(user): switch to nano precision`.

### Ключевая семантика: «платит следующий»

Это самое неочевидное решение Guava, и оно явно задокументировано (`RateLimiter.java`,
javadoc класса):

> It is important to note that the number of permits requested *never* affects the
> throttling of the request itself (an invocation to `acquire(1)` and an invocation to
> `acquire(1000)` will result in exactly the same throttling, if any), but it affects the
> throttling of the *next* request.

Механика — в поле `nextFreeTicketMicros` (`SmoothRateLimiter.java:329`):

> The time when the next request (no matter its size) will be granted. After granting a
> request, this is pushed further in the future. Large requests push this further than
> small requests.

Из комментария разработчиков (`SmoothRateLimiter.java`, строки ~125–140):

> consider a RateLimiter with rate of 1 permit per second, currently completely unused,
> and an expensive `acquire(100)` request comes. It would be nonsensical to just wait for
> 100 seconds, and *then* start the actual task. Why wait without doing anything? … This
> has important consequences: it means that the RateLimiter doesn't remember the time of
> the *last* request, but it remembers the (expected) time of the *next* request.

То есть состояние — не «сколько токенов», а «когда откроется окно», плюс отдельно
`storedPermits` для учёта простоя. Побочная выгода, которую они называют сами: раз
момент следующей выдачи известен всегда, `tryAcquire(timeout)` отвечает **мгновенно и
точно**, без спекуляции — `canAcquire` (строка 428) это одна строка:

```java
private boolean canAcquire(long nowMicros, long timeoutMicros) {
  return queryEarliestAvailable(nowMicros) - timeoutMicros <= nowMicros;
}
```

Наш порт `Allow()` — это ровно `tryAcquire` с нулевым таймаутом.

### `SmoothBursty` — дефолт

`RateLimiter.create(permitsPerSecond)` возвращает `new SmoothBursty(stopwatch, /* maxBurstSeconds= */ 1.0)`
(строка 138). То есть накопить можно не более **одной секунды** работы. Обоснование
именно такого дефолта дано комментарием в коде (строки 118–132): при 1 qps и четырёх
потоках, зовущих `acquire()` в моменты 0 / 1.05 / 2 / 3 сек, без запаса T2 пришлось бы
ждать до 2.05, а T3 — до 3.05, то есть однократная задержка навсегда сдвигала бы всю
последовательность.

`SmoothBursty.storedPermitsToWaitTime(...)` возвращает `0L` — накопленные разрешения
отдаются мгновенно. Это и есть token bucket.

### `SmoothWarmingUp` — семантика, которой почти нигде больше нет

Идея (javadoc фабрики `create(permitsPerSecond, warmupPeriod, unit)`):

> Creates a `RateLimiter` with the specified stable throughput … and a *warmup period*,
> during which the `RateLimiter` smoothly ramps up its rate, until it reaches its maximum
> rate at the end of the period … The returned `RateLimiter` is intended for cases where
> the resource that actually fulfills the requests (e.g., a remote server) needs "warmup"
> time, rather than being immediately accessed at the stable (maximum) rate.

Инверсия по сравнению с обычным bucket: у token bucket простой = **накопленный кредит**,
разрешения тратятся быстрее номинала. У `SmoothWarmingUp` простой = **сигнал, что
ресурс остыл** (кэши протухли, JIT деоптимизировался, сервер только загрузился), и
накопленные разрешения отдаются **медленнее** номинала.

Формально: `storedPermitsToWaitTime(storedPermits, permitsToTake)` — это интеграл
кусочно-линейной функции «интервал между разрешениями» по `storedPermits`. В коде эта
функция нарисована ASCII-графиком (`SmoothRateLimiter.java:143-162`): горизонтальная
линия на уровне `stableInterval` от 0 до `thresholdPermits`, дальше наклонная вверх до
`coldInterval = coldFactor * stableInterval` при `maxPermits`. `coldFactor` жёстко
задан **3.0** в публичной фабрике (`RateLimiter.java:199`); параметризованная перегрузка
существует, но помечена `@VisibleForTesting`.

Расчёт (`SmoothWarmingUp.doSetRate`, строки ~224–229):

```java
thresholdPermits = 0.5 * warmupPeriodMicros / stableIntervalMicros;
maxPermits = thresholdPermits + 2.0 * warmupPeriodMicros / (stableIntervalMicros + coldIntervalMicros);
slope = (coldIntervalMicros - stableIntervalMicros) / (maxPermits - thresholdPermits);
```

Почему интеграл, а не что-то проще, — обосновано в комментарии (строки ~105–113):

> Using integrals guarantees that the effect of a single `acquire(3)` is equivalent to
> `{ acquire(1); acquire(1); acquire(1); }`, or `{ acquire(2); acquire(1); }`, etc … This
> guarantees that we handle correctly requests of varying weight (permits), /no matter/
> what the actual function is — so we can tweak the latter freely.

Это красивый инвариант: аддитивность по числу разрешений при произвольной функции
стоимости. Наш `AllowN(n)` его тоже выполняет, но тривиально — у нас функция стоимости
константна.

Разница между двумя режимами сведена к **двум переопределённым методам**:
`storedPermitsToWaitTime` и `coolDownIntervalMicros`. У `SmoothBursty` это `0L` и
`stableIntervalMicros`, у `SmoothWarmingUp` — интеграл и `warmupPeriodMicros / maxPermits`.
Всё остальное (`resync`, `reserveEarliestAvailable`) общее. Компактная точка расширения.

### Арифметика: чистый `double`, без эпсилона

`storedPermits`, `maxPermits`, `stableIntervalMicros` — `double`
(`SmoothRateLimiter.java:314-324`). `nextFreeTicketMicros` — `long`. Никакого
`roundingError`, никакого эпсилона на границе. Причина, почему это сходит с рук:
**сравнения «хватает ли токенов» в коде нет вообще**. Вместо него —
`reserveEarliestAvailable`, который считает время:

```java
double storedPermitsToSpend = min(requiredPermits, this.storedPermits);
double freshPermits = requiredPermits - storedPermitsToSpend;
long waitMicros = storedPermitsToWaitTime(this.storedPermits, storedPermitsToSpend)
        + (long) (freshPermits * stableIntervalMicros);
this.nextFreeTicketMicros = LongMath.saturatedAdd(nextFreeTicketMicros, waitMicros);
this.storedPermits -= storedPermitsToSpend;
```

Решение «пускать или нет» принимается сравнением **двух `long`** микросекунд в
`canAcquire`. Дрейф `double` смещает `waitMicros` на доли микросекунды, но не
переворачивает булев ответ на границе. Это третий по счёту способ обойти нашу проблему
(после точного остатка Bucket4j и целочисленных циклов Resilience4j): **вынести решение
из области дробных величин в область времени**.

### Почему до сих пор `@Beta`

Класс помечен `@Beta` с версии **13.0** (`@since 13.0` в javadoc), и на сентябрь 2026
пометка на месте. Обсуждение — GitHub issue [google/guava#5612], **открыт** с 17 июня
2021, ассигнован `cpovirk`, метка `type=debeta`, `P3`. [5] Более ранние
issue #2797 (2017), #3300 (2018), #5959 (2022) закрыты как дубликаты. [6][7][8]

Единственный содержательный ответ мейнтейнера — cpovirk, 26 апреля 2017, в #2797:

> We'll take a look. There are a number of things about it that we've hoped to eventually
> change, but it's hard to see when we would ever get around to them, so perhaps we should
> just finalize it.

То есть **технической блокирующей причины не названо** — это приоритеты, не дефект.
В треде участники указывают два оставшихся `TODO` в коде (оба присутствуют в текущем
исходнике): переход на наносекундную точность и приведение порядка параметров
`SleepingStopwatch` к обычной конвенции. Сам cpovirk в #5612 (18 июня 2021) на вопрос
об альтернативах отвечает, что команда сама присматривалась к Resilience4j и Failsafe,
но не нашла времени разобраться.

Важное уточнение из того же треда (darshitpp, 14 июня 2022): Guava `RateLimiter` —
**блокирующий** по основному контракту (`acquire()` спит), а Resilience4j бросает
`RequestNotPermitted`; это разные API, не взаимозаменяемые «в лоб».

### Абстракция часов

`SleepingStopwatch` (`RateLimiter.java:463-497`) — абстрактный класс с двумя методами:
`readMicros()` и `sleepMicrosUninterruptibly(long)`. То есть часы **и** способ спать
объединены в один инжектируемый объект. Дефолт — `createFromSystemTimer()` поверх
`com.google.common.base.Stopwatch`.

Отличие от нашего `Clock` принципиальное: у Guava «поспать» — часть абстракции времени,
потому что порт блокирующий. У нас порт неблокирующий, и sleep в `Clock` не нужен.
Если бы мы когда-нибудь добавили `Wait(ctx)`, то тестовый двойник должен был бы
контролировать и сон тоже — иначе тест блокирующего пути перестаёт быть
детерминированным. Guava решает это тем же приёмом, что мы для `fakeClock`.

## Netflix concurrency-limits

Разбор по коммиту `78a74b9878d38c4c048b0304ce12a162ab7b7222` (12 января 2026),
ветка `main`, модуль `concurrency-limits-core`. Лицензия Apache-2.0. [9]

### Смежный класс: почему это не rate limiter

README формулирует претензию к rate limiting прямо (раздел «Background»):

> When thinking of service availability operators traditionally think in terms of RPS
> (requests per second). Stress tests are normally performed to determine the RPS at which
> point the service tips over. RPS limits are then set somewhere below this tipping point
> (say 75% of this value) and enforced via a token bucket. However, in large distributed
> systems that auto-scale this value quickly goes out of date and the service falls over
> by becoming non-responsive as it is unable to gracefully shed excess load. Instead of
> thinking in terms of RPS, we should be thinking in terms of concurrent requests …
> This relationship is covered very nicely with Little's Law where
> `Limit = Average RPS * Average Latency`.

То есть тезис: RPS-лимит — это **человеком заданная константа**, которая протухает.
Конкурентный лимит можно **измерять**, потому что рост очереди виден по латентности.

### Порт: acquire/release вместо Allow

`concurrency-limits-core/src/main/java/com/netflix/concurrency/limits/Limiter.java`:

```java
@FunctionalInterface
public interface Limiter<ContextT> {
    interface Listener {
        void onSuccess();
        void onIgnore();
        void onDropped();
    }
    Optional<Listener> acquire(ContextT context);
}
```

Два принципиальных отличия от нашего `Limiter`:

1. **Операция парная.** `acquire` возвращает `Optional<Listener>`; пустой `Optional` —
   отказ. Полученный `Listener` **обязан** быть закрыт одним из трёх методов. Наш
   `Allow() bool` одноразовый и ничего не требует после.
2. **Три исхода, а не два.** `onSuccess` — измерить RTT как валидный сэмпл;
   `onIgnore` — не учитывать (запрос упал до того, как измерение стало осмысленным);
   `onDropped` — таймаут/отказ внешнего лимита, повод агрессивно снизить лимит.
   Обратная связь от результата — то, чего в rate-limiting-порте нет в принципе.

### Счётчик конкурентности

`limiter/SimpleLimiter.java`: сам счётчик — `Semaphore`, а не CAS-петля:

```java
private static final class AdjustableSemaphore extends Semaphore {
    AdjustableSemaphore(int permits) { super(permits); }
    @Override public void reducePermits(int reduction) { super.reducePermits(reduction); }
}
```

Приём аккуратный: `Semaphore.reducePermits` в JDK — `protected`, и подкласс открывает
его, чтобы менять лимит на живом семафоре. `onNewLimit(int newLimit)` вызывает
`release(delta)` или `reducePermits(-delta)` в зависимости от знака.

Отдельно `AbstractLimiter` держит `private final AtomicInteger inFlight` — но это
только для метрик и для передачи `inflight` в алгоритм, не для решения о допуске.
Часы инжектируются: `private LongSupplier clock = System::nanoTime`, с билдером
`nanoClock(LongSupplier)` (`AbstractLimiter.java:54,81,119`).

### Алгоритмы

Все реализуют `Limit#onSample(long startTime, long rtt, int inflight, boolean didDrop)`.

**`AIMDLimit`** — самый простой, буквально TCP Reno (`limit/AIMDLimit.java:102-112`):

```java
if (didDrop || rtt > timeout) {
    currentLimit = (int) (currentLimit * backoffRatio);
} else if (inflight * 2 >= currentLimit) {
    currentLimit = currentLimit + 1;
}
return Math.min(maxLimit, Math.max(minLimit, currentLimit));
```

Условие `inflight * 2 >= currentLimit` — защита от «дрейфа вверх на холостом ходу»:
если система недогружена, лимит не растёт, потому что его никто не проверял.

**`VegasLimit`** — delay-based, оценка длины очереди по формуле из README
`L * (1 - minRTT/sampleRtt)`. В коде (`limit/VegasLimit.java:281`):

```java
final int queueSize = (int) Math.ceil(estimatedLimit * (1 - (double) rtt_noload / rtt));
```

Дальше — сравнение с порогами `alpha` / `beta` / `threshold` (все настраиваются
функциями от текущего лимита), рост при малой очереди, снижение при большой,
сглаживание `newLimit = (1 - smoothing) * estimatedLimit + smoothing * newLimit`.
Есть механизм периодического «прощупывания» (`shouldProbe()` / `resetProbeJitter()`),
который сбрасывает `rtt_noload`, чтобы оценка минимального RTT не залипала навсегда
на устаревшем значении.

**`Gradient2Limit`** — README:

> This algorithm attempts to address bias and drift when using minimum latency
> measurements. To do this the algorithm tracks the measure of divergence between two
> exponential averages over a long and short time window.

Ядро (`limit/Gradient2Limit.java:306-308`):

```java
final double gradient = Math.max(0.5, Math.min(1.0, tolerance * longRtt / shortRtt));
double newLimit = estimatedLimit * gradient + queueSize;
newLimit = estimatedLimit * (1 - smoothing) + newLimit * smoothing;
```

Клипы `max(0.5, ...)` и `min(1.0, ...)` — прямая защита от того, чтобы выброс латентности
срезал лимит вдвое за один сэмпл.

Прочее: `WindowedLimit` (агрегирует сэмплы в окна перед пересчётом), `FixedLimit`,
`SettableLimit`, `TracingLimitDecorator`, окна `ImmutableAverageSampleWindow` /
`ImmutablePercentileSampleWindow`.

### Стратегии применения

README, раздел «Enforcement Strategies»: `Simple` (один счётчик) и `Percentage` —
разбиение лимита на партиции с гарантированными долями. Класс —
`limiter/AbstractPartitionedLimiter.java`, конфигурация вида
`.partitionByHeader(GROUP_HEADER).partition("live", 0.9).partition("batch", 0.1)`.
Это функционально ближайший аналог «нескольких лимитов на одном бакете» у Bucket4j, но
семантика противоположная: у Bucket4j это **конъюнкция** ограничений на один поток, тут —
**разделение одного ресурса** между потоками.

Ещё есть `BlockingLimiter` и `LifoBlockingLimiter` — обёртки, блокирующие вызывающего
вместо отказа; LIFO-очередь ожидающих выбрана намеренно (при перегрузке лучше обслужить
свежие запросы, чем те, чьи клиенты уже отвалились по таймауту).

### Что из этого применимо к нам

Прямо — почти ничего: адаптивные лимиты требуют обратной связи о результате запроса,
а наш порт `Allow() bool` её структурно не имеет, и добавление сломало бы центральный
инвариант проекта (неблокирующий, всё-или-ничего, без состояния у вызывающего).

Косвенно ценны две вещи. Первая — **`Semaphore` как отдельный примитив ограничения**:
он ограничивает конкурентность, а не темп, и это ортогональная ось. Вторая — идея
`onIgnore` как третьего исхода: наш `AllowN` тоже мог бы иметь смысл «списали, но
операция не состоялась, верните токены», аналог `Bucket.addTokens` у Bucket4j.

## Прочее (Spring Cloud Gateway, Sentinel, Pekko, Failsafe)

### Apache Pekko `TokenBucket` — самое ценное здесь

Файл `actor/src/main/scala/org/apache/pekko/util/TokenBucket.scala` (ветка `main`),
получен через GitHub Contents API. [10] Заголовок файла: «This file is part of the
Apache Pekko project, which was derived from Akka», копирайт Lightbend 2015–2022 —
то есть это дословно тот же код, что в Akka.

Класс помечен `private[pekko] abstract class TokenBucket(capacity: Long, nanosBetweenTokens: Long)`,
состояние — два обычных `var`:

```scala
private var availableTokens: Long = 0L
private var lastUpdate: Long = 0L
```

Синхронизации нет вообще — потокобезопасность обеспечивается снаружи: бакет живёт
внутри GraphStage оператора `Flow.throttle`, а Pekko Streams гарантирует, что стадия
исполняется одним потоком за раз. Это четвёртая стратегия, помимо mutex / CAS / без
синхронизации у Bucket4j: **не синхронизировать, потому что модель исполнения не даёт
конкурентного доступа**.

Ядро `offer(cost: Long): Long` — и вот тут главное:

```scala
val now = currentTime
val timeElapsed = now - lastUpdate

val tokensArrived =
  if (timeElapsed >= nanosBetweenTokens) {
    if (timeElapsed < nanosBetweenTokens * 2) {
      lastUpdate += nanosBetweenTokens
      1
    } else {
      val tokensArrived = timeElapsed / nanosBetweenTokens
      lastUpdate += tokensArrived * nanosBetweenTokens
      tokensArrived
    }
  } else 0

availableTokens = math.min(availableTokens + tokensArrived, capacity)

if (cost <= availableTokens) {
  availableTokens -= cost
  0
} else { ... }
```

**`lastUpdate` двигается только на целое число периодов токена**, а не на `now`. Остаток
`now - lastUpdate` остаётся «в часах» и учитывается на следующем вызове. Результат тот
же, что у `roundingError` в Bucket4j — **нулевой дрейф**, — но **без дополнительного
поля состояния**. Это самая экономная из найденных конструкций.

Побочно там же — микрооптимизация: если прошёл ровно один период, целочисленное деление
пропускается (`// Ok, no choice, do the slow integer division` в ветке else).
Комментарий к `currentTime`: «The returned value is monotonic, might wrap over and has no
relationship with wall-clock» — то есть контракт часов явно монотонный, как у нашего
`Clock` de facto. Ограничение задокументировано честно: «The method does not handle
overflow, if an element is to be delayed longer in nanoseconds than what can be
represented as a positive Long then an undefined value is returned».

Реализация часов — `final class NanoTimeTokenBucket` поверх `System.nanoTime()`;
абстрактный `def currentTime: Long` — та же инъекция времени, что у нас, но через
наследование.

Семантика на уровне API — `ThrottleMode`
(`stream/src/main/scala/org/apache/pekko/stream/ThrottleMode.scala`) [11]:

- `Shaping` — «Tells throttle to make pauses before emitting messages to meet throttle rate»;
- `Enforcing` — «Makes throttle fail with exception when upstream is faster than throttle
  rate», с `class RateExceededException`.

Ровно противопоставление leaky bucket (сгладить) против token bucket (отклонить), но
поднятое на уровень конфигурации одного оператора, а не выбора алгоритма. У нас это
два разных типа.

### Alibaba Sentinel

Разбор по ветке `1.8`, пакет
`sentinel-core/src/main/java/com/alibaba/csp/sentinel/slots/block/flow/`. [12]

Sentinel устроен не как «лимитер», а как **правило** (`FlowRule`) плюс сменный
`TrafficShapingController`. Реализации контроллера:

**`DefaultController`** — счётчик за окно, отказ при превышении
(`controller/DefaultController.java:49-69`). Дополнительно поддерживает `prioritized`-
запросы: при превышении вызывает `node.tryOccupyNext(...)`, и если ожидание укладывается
в `OccupyTimeoutProperty`, **занимает квоту будущего окна**, спит и бросает
`PriorityWaitException`. Аналог `consumeIgnoringRateLimits` Bucket4j, но с ограничением
по горизонту заимствования.

**`ThrottlingController`** (`@since 2.0`, «Refactored from legacy RateLimitController of
Sentinel 1.x») — GCRA-подобный: всё состояние это `AtomicLong latestPassedTime`.
Существенный фрагмент (`checkPassUsingNanoSeconds`, строки 70–103):

```java
final long costTimeNs = Math.round(1.0d * MS_TO_NS_OFFSET * statDurationMs * acquireCount / maxCountPerStat);
long expectedTime = costTimeNs + latestPassedTime.get();

if (expectedTime <= currentTime) {
    // Contention may exist here, but it's okay.
    latestPassedTime.set(currentTime);
    return true;
} else {
    ...
    long oldTime = latestPassedTime.addAndGet(costTimeNs);
    waitTime = oldTime - curNanos;
    if (waitTime > maxQueueingTimeNs) {
        latestPassedTime.addAndGet(-costTimeNs);   // откат
        return false;
    }
    ...
}
```

Три наблюдения, каждое по делу для нас:

1. **Гонка на быстром пути принята сознательно и закомментирована**:
   `// Contention may exist here, but it's okay.` Между `get()` и `set()` нет CAS.
   Прямая противоположность инварианту `ratelimit-lab`, где `-race` обязателен. Но
   решение не безумное: в их модели превышение на несколько запросов при гонке дешевле,
   чем стоимость CAS-петли на горячем пути.
2. **Медленный путь — не CAS, а fetch-and-add с компенсирующим откатом**
   (`addAndGet(costTimeNs)` … `addAndGet(-costTimeNs)`). Петли нет вообще — операция
   завершается за фиксированное число атомарных инструкций. Наш GCRA-эксперимент на
   одном `atomic.Int64` использовал CAS; версия на `Add` без петли — отдельная гипотеза,
   которую стоит замерить.
3. **`Math.round` на границе вместо эпсилона**: `costTimeNs` считается в `double` и
   немедленно округляется до `long`; дальше всё сравнение идёт в целых наносекундах.

**`WarmUpController`** — порт warm-up из Guava, о чём сказано в javadoc открытым текстом:

> The principle idea comes from Guava. However, the calculation of Guava is rate-based,
> which means that we need to translate rate to QPS. … Guava's implementation focuses on
> adjusting the request interval, which is similar to leaky bucket. Sentinel pays more
> attention to controlling the count of incoming requests per second without calculating
> its interval, which resembles token bucket algorithm.

В коде видны буквально те же формулы, что в `SmoothWarmingUp`, с гуавовскими именами в
комментариях:

```java
// thresholdPermits = 0.5 * warmupPeriod / stableInterval.
warningToken = (int)(warmUpPeriodInSec * count) / (coldFactor - 1);
// maxPermits = thresholdPermits + 2 * warmupPeriod / (stableInterval + coldInterval)
maxToken = warningToken + (int)(2 * warmUpPeriodInSec * count / (1.0 + coldFactor));
slope = (coldFactor - 1.0) / count / (maxToken - warningToken);
```

Дефолтный `coldFactor` — тоже 3, как у Guava. Есть и `WarmUpRateLimiterController` —
комбинация warm-up с очередью.

**И вот прямое попадание в тему `admitEpsilon`** (`WarmUpController.canPass`, строка ~124):

```java
double warningQps = Math.nextUp(1.0 / (aboveToken * slope + 1.0 / count));
if (passQps + acquireCount <= warningQps) {
    return true;
}
```

`Math.nextUp(x)` — следующее представимое `double` вверх, то есть **эпсилон величиной
ровно в один ULP**, приклеенный к порогу. Это тот же приём, что наш `admitEpsilon`, но
с двумя отличиями: (а) он масштабируется вместе с величиной (ULP относителен), тогда как
наша константа `1e-9` абсолютна; (б) он минимально возможный, а не «с запасом».

**`AbstractTokenBucket` / `StrictTokenBucket`** (пакет `.../flow/tokenbucket/`) — более
новый и отдельный token bucket. Тут показательное разделение:

- `AbstractTokenBucket.tryConsume` работает с `protected volatile long currentTokenNum`
  и делает `currentTokenNum -= tokenNum` — **read-modify-write по volatile, то есть
  настоящая гонка**. `DefaultTokenBucket` наследует это без изменений;
- `StrictTokenBucket` переопределяет и `tryConsume`, и `refreshCurrentTokenNum`, добавляя
  **два раздельных монитора** (`refreshLock`, `consumeLock`) и double-checked locking:

```java
if (tokenNum <= currentTokenNum) {
    synchronized (consumeLock) {
        if (tokenNum <= currentTokenNum) {
            currentTokenNum -= tokenNum;
            return true;
        }
    }
}
return false;
```

То есть библиотека **явно предлагает выбор между быстрым-неточным и медленным-точным**,
именами классов (`Default` против `Strict`). Арифметика целочисленная; `nextProduceTime`
двигается по границам интервалов
(`nextProduceTime = intervalInMs - ((currentTimestamp - startTime) % intervalInMs) + currentTimestamp`),
то есть та же идея «не терять остаток», что у Pekko.

### Failsafe

Репозиторий `failsafe-lib/failsafe`, пакет `core/src/main/java/dev/failsafe/`. [13]
Две реализации, различающиеся только классом статистики:

- `internal/SmoothRateLimiterStats` — javadoc: «evenly distributes permits over time …
  focuses on the interval between permits, and tracks the next interval in which a permit
  is free». Состояние — **одно поле** `long nextFreePermitNanos`. То есть чистый GCRA,
  и по смыслу это Guava `SmoothBursty` без `storedPermits`;
- `internal/BurstyRateLimiterStats` — javadoc: «allows bursts of executions, up to the max
  permits per period. This implementation tracks the current period and available permits,
  **which can go into a deficit**». Состояние — `long availablePermits` (может быть
  отрицательным) + `long currentPeriod`. Это fixed window с заимствованием вперёд,
  близко к `AtomicRateLimiter` Resilience4j.

Синхронизация — `public synchronized long acquirePermits(...)` в обоих классах, то есть
mutex; CAS-варианта нет. Арифметика **целиком целочисленная в наносекундах**, `double`
не встречается; для округления вниз к границе интервала есть отдельный хелпер
`Maths.roundDown(currentNanos, intervalNanos)`, для сложения с насыщением — `Maths.add`.

Порт (`internal/RateLimiterImpl`): `tryAcquirePermits(int)` реализован как
`reservePermits(permits, Duration.ZERO) == 0` — то есть неблокирующий `Allow` выражен
через общий «зарезервировать с максимальным ожиданием». Есть также `acquirePermits`
(блокирующий) и `reservePermits` (возвращает `Duration`), плюс исключение
`RateLimitExceededException`. Часы инжектируются классом `internal/Stopwatch`.

Косвенное свидетельство веса библиотеки: cpovirk, мейнтейнер Guava, упоминает Failsafe
в guava#5612 как одну из двух альтернатив, к которым присматривалась команда Guava. [5]

### Spring Cloud Gateway `RequestRateLimiter`

Дефолтная реализация — **распределённая, на Redis**, и потому лишь косвенно относится к
нашей single-process задаче. Вся логика — в Lua-скрипте
`spring-cloud-gateway-server-webflux/src/main/resources/META-INF/scripts/request_rate_limiter.lua`
(ветка `main`, получено через raw.githubusercontent). [14] Скрипт целиком:

```lua
redis.replicate_commands()

local tokens_key = KEYS[1]
local timestamp_key = KEYS[2]

local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local now = tonumber(ARGV[3]) or redis.call('TIME')[1]
local requested = tonumber(ARGV[4])

local fill_time = capacity / rate
local ttl = math.floor(fill_time * 2)

local last_tokens = tonumber(redis.call("get", tokens_key)) or capacity
local last_refreshed = tonumber(redis.call("get", timestamp_key)) or 0

local delta = math.max(0, now-last_refreshed)
local filled_tokens = math.min(capacity, last_tokens+(delta*rate))
local allowed = filled_tokens >= requested
local new_tokens = allowed and filled_tokens - requested or filled_tokens

if ttl > 0 then
  redis.call("setex", tokens_key, ttl, new_tokens)
  redis.call("setex", timestamp_key, ttl, now)
end

return { allowed and 1 or 0, new_tokens }
```

Что здесь любопытно именно для нас:

- Это **тот же token bucket, что наш**, включая `math.min(capacity, last_tokens + delta*rate)`
  и `filled_tokens >= requested`;
- **арифметика — числа Lua, то есть IEEE754 `double`**, и никакого эпсилона нет.
  Сходит с рук по двум причинам: `now` берётся из `redis.call('TIME')[1]`, то есть в
  **целых секундах**, а состояние **переписывается целиком** каждой операцией через
  `setex` — то есть между операциями `tokens` хранится как строка и парсится заново.
  Дрейфа в нашем смысле нет, потому что `delta` — целое число секунд;
- атомарность обеспечена тем, что Redis исполняет скрипт целиком; никакой CAS-петли,
  никаких блокировок в коде приложения. Это ровно то, что упоминание «Compare&Swap
  implementation details for Redis» в свежем коммите Bucket4j (`92553c54`, июль 2026)
  обсуждает как альтернативу;
- **не проверено**: есть ли в актуальном Spring Cloud Gateway single-process (in-memory)
  реализация `RateLimiter` кроме Redis-овой; я смотрел только скрипт и не разбирал
  Java-классы фильтра.

## Целочисленное состояние против IEEE754 и наш admitEpsilon

### Короткий ответ

Формулировка задачи предполагала, что у Bucket4j есть два равноправных представления
состояния и что целочисленное «ловит какие-то баги» дробного. Это **не так**, и
уточнение важное:

- `BucketStateIEEE754` **никогда не был production-вариантом**. Его javadoc:
  «you should not use this precision in production, because intention of this precision is
  the testing purpose for backends written in Lua or JS, in other words for testing
  backends that do not provide 64-bit integer arithmetic». Он существовал, чтобы
  проверять поведение распределённых бэкендов, у которых нет `long`;
- он **удалён** 24 октября 2023 (коммит `0c072e0e`, «#410 remove experimental code»)
  вместе со всей веткой `MathType.IEEE_754`;
- `LocalBucketBuilder` жёстко зашивает `MathType.INTEGER_64_BITS` для всех трёх
  стратегий синхронизации — то есть у пользователя локального бакета выбора не было и
  до удаления.

Отсюда прямой вывод: **Bucket4j не «лечил» дрейф float64, он его не допускал.** Никакого
аналога `admitEpsilon` в их коде нет и не было.

### Четыре разных способа обойтись без эпсилона

Разбор всех пяти библиотек даёт ровно четыре техники, и все они сводятся к одному
принципу: **не сравнивать дробные величины на границе**.

**1. Точный целочисленный остаток (Bucket4j).** Токены — `long`, дробная часть refill
хранится отдельным `long` как числитель над `refillPeriodNanos`:

```java
long divided = refillTokens * durationSinceLastRefillNanos + roundingError;
long calculatedRefill = divided / refillPeriodNanos;
roundingError = divided % refillPeriodNanos;
```

Цена: третий слот состояния на каждый bandwidth и обязательная обработка переполнения
`long` (у них — `multiplyExactOrReturnMaxValue` и явный fallback в `double`).
Гарантия: дрейфа нет вообще, при любом числе шагов.

**2. Не двигать часы дальше последнего целого токена (Pekko/Akka).** Тот же результат,
но **без третьего поля**:

```scala
val tokensArrived = timeElapsed / nanosBetweenTokens
lastUpdate += tokensArrived * nanosBetweenTokens   // не now!
```

Остаток живёт в разнице `now - lastUpdate` и учитывается на следующем вызове. Состояние —
два `long`. Это самая экономная из найденных конструкций и лучший кандидат на перенос.

**3. Квантование времени в целые «циклы»/«периоды» (Resilience4j, Failsafe, Sentinel
`AbstractTokenBucket`).** Баланс меняется только на границах периода:
`currentCycle = currentNanos / cyclePeriodInNanos`, пополнение —
`elapsedCycles * permissionsPerCycle`. Дробей нет по построению. Цена: это уже не token
bucket, а fixed window; burst-семантика другая (возможен пик 2× на стыке окон).

**4. Перенос решения из «сколько токенов» в «когда откроется окно» (Guava, Failsafe
`SmoothRateLimiterStats`, Sentinel `ThrottlingController`).** `double` в состоянии
допустим, потому что булево решение принимается сравнением **двух целых меток времени**:

```java
// Guava, RateLimiter.java:428
return queryEarliestAvailable(nowMicros) - timeoutMicros <= nowMicros;
```

Дрейф `double` сдвигает `nextFreeTicketMicros` на доли микросекунды, но не переворачивает
ответ.

### Единственный найденный аналог эпсилона

Он ровно один на все пять библиотек — Alibaba Sentinel, `WarmUpController.canPass`,
строка 127:

```java
double warningQps = Math.nextUp(1.0 / (aboveToken * slope + 1.0 / count));
if (passQps + acquireCount <= warningQps) { return true; }
```

`Math.nextUp` — следующее представимое `double`, то есть эпсилон в **один ULP**.
Отличия от нашего `admitEpsilon`:

| | наш `admitEpsilon` | `Math.nextUp` у Sentinel |
|---|---|---|
| Величина | абсолютная константа `1e-9` | относительная, один ULP от порога |
| Масштабируемость | ломается при пороге ≥ 2^24 (задокументировано в `limiter.go:28-34`) | работает на любом масштабе |
| Область применения | все реализации, где состояние — `float64` | только один контроллер из пяти |

То есть даже там, где на JVM эпсилон применили, взяли **относительный** эпсилон, а не
абсолютный. Для нас это конкретное улучшение с оценённой стоимостью: `math.Nextafter(x, math.Inf(1))`
в Go — прямой аналог `Math.nextUp`.

### Что это говорит про наш `admitEpsilon`

1. **Наш выбор — единственный среди пяти, и он следствие более раннего решения**: хранить
   токены как `float64`. Все, кто не хранит дробные токены, эпсилон не пишут.
2. **Задокументированный потолок 2^24 — реальное ограничение, а не педантизм.**
   `limiter.go:28-34` уже фиксирует, что при пороге ≥ 16777216 `capacity+admitEpsilon == capacity`
   и эпсилон превращается в no-op. Ни одна из пяти библиотек такого потолка не имеет,
   потому что ни одна не сравнивает дробные величины напрямую. Отсюда практический
   вывод: перед «мерить байты» (упомянуто в том же комментарии как триггер пересмотра)
   надо не расширять эпсилон, а менять представление.
3. **Замеренный недобор ~9% (910 из 1000) без эпсилона — не наш дефект, а известный
   класс проблемы.** Bucket4j решает его `roundingError`, Pekko — невыдвижением
   `lastUpdate`. Стоит зафиксировать в доке, что мы выбрали третий путь сознательно.
4. **Учебная ценность высока именно в контрасте.** «Мы держим дробные токены и лечим
   границу эпсилоном; Bucket4j держит целые и точный остаток; Pekko держит целые и
   не двигает часы; Resilience4j квантует время; Guava сравнивает моменты» — это пять
   разных ответов на один вопрос, каждый со своей ценой. Такой разбор — готовое
   содержание для раздела в `docs/`.

## Аллокации в CAS-петле: как решают на JVM

### Сводка по реализациям

| Реализация | Синхронизация | Аллокация на вызов | Аллокация на ретрай | Backoff | Лимит ретраев |
|---|---|---|---|---|---|
| Bucket4j `LockFreeBucket` | `AtomicReference<BucketState>` + CAS | **1** (`previousState.copy()` до петли) | **0** (`copyStateFrom` в тот же буфер) | нет | нет |
| Resilience4j `AtomicRateLimiter` | `AtomicReference<State>` + CAS | 1 | **1 на каждую итерацию** (`new State(...)` внутри `do-while`) | **`parkNanos(1)`** | нет |
| Bucket4j `SynchronizedBucket` | `ReentrantLock` | 0 | — | — | — |
| Guava `RateLimiter` | `synchronized (mutex())` | 0 | — | — | — |
| Failsafe `RateLimiterImpl` | `synchronized` метод | 0 | — | — | — |
| Sentinel `ThrottlingController` | `AtomicLong` fetch-and-add, **петли нет** | 0 | — | — | — |
| Sentinel `StrictTokenBucket` | два `synchronized`-монитора + DCL | 0 | — | — | — |
| Pekko `TokenBucket` | никакой (однопоточная стадия) | 0 | — | — | — |
| **наш `LockFreeTokenBucket`** | `atomic.Pointer` + CAS | 1 | **1 на каждую итерацию** | нет | нет |

Замеренные нами 8 alloc/op на parallel-16 согласуются с этой строкой: примерно 8
итераций петли, каждая со своим `&lockFreeState{}`.

### Приём Bucket4j: буфер, переиспользуемый между ретраями

Единственная из пяти библиотек, кто действительно снял аллокации с ретраев. Ключ —
объект состояния **изменяемый**, а CAS идёт по идентичности ссылки:

```java
BucketState previousState = stateRef.get();
BucketState newState = previousState.copy();      // ← одна аллокация, вне цикла
long currentTimeNanos = timeMeter.currentTimeNanos();

while (true) {
    newState.refillAllBandwidth(currentTimeNanos);
    ...
    if (stateRef.compareAndSet(previousState, newState)) return true;
    previousState = stateRef.get();
    newState.copyStateFrom(previousState);        // ← перезапись, без new
}
```

Корректность держится на том, что `newState` **не опубликован**, пока CAS не прошёл: до
успеха ссылку не видит никто, кроме этой нити, поэтому мутировать её безопасно. После
успеха объект больше не переиспользуется — нить выходит из метода.

`copyStateFrom` для целочисленного состояния — `System.arraycopy` в существующий массив
(`BucketState64BitsInteger.java:298-306`), то есть даже без выделения массива, если
конфигурация та же.

**Прямой перенос в Go есть и он несложный.** Наша петля сейчас:

```go
for {
    old := b.state.Load()
    now := b.clk.Now()
    next, ok := nextLockFreeState(old, now, b.rate, b.capacity, n)  // ← &lockFreeState{} внутри
    if !ok { return false }
    if b.state.CompareAndSwap(old, next) { return true }
}
```

Вариант «по-бакет4джейски»: выделить один `next := new(lockFreeState)` перед циклом и на
каждой итерации записывать в его поля, а не создавать новый. Ограничение то же самое —
`next` нельзя переиспользовать после успешного CAS, но после успеха мы и так выходим.
Это должно свести аллокации с ~8 к 1 на вызов на 16 горутинах.

Оговорка: наши замеры показали, что **аллокации — не главная причина отставания**
(GCRA в одном `atomic.Int64`, 0 alloc, дал 802.7 нс против 478.6 нс у mutex на
parallel-16, то есть всё равно проиграл). Значит, приём Bucket4j снимет GC-давление, но
не перевернёт вывод. Ценность — учебная и измерительная: он изолирует вклад аллокаций
от вклада самой contention на CAS.

### Приём Resilience4j: не бороться с аллокацией, а сократить число ретраев

Противоположная стратегия. `State` неизменяем и создаётся заново каждую итерацию —
переиспользовать нечего. Вместо этого добавлен backoff:

```java
private boolean compareAndSet(final State current, final State next) {
    if (state.compareAndSet(current, next)) return true;
    parkNanos(1); // back-off
    return false;
}
```

со ссылкой в javadoc на arXiv:1305.5800 [3] и утверждением «showed great results with
`AtomicRateLimiter` in benchmark tests» (цифры не приведены — **не проверено**).

Логика: каждая пауза уменьшает вероятность немедленного повторного столкновения, а раз
итераций меньше — меньше и аллокаций, и трафика когерентности кэша. Мелкий `State`
(4 поля) в TLAB на JVM стоит недорого, так что бороться именно с аллокацией смысла мало.

**Перенос в Go менее прямой.** `LockSupport.parkNanos(1)` паркует поток на уровне ОС;
в Go ближайшие аналоги — `runtime.Gosched()` (перепланировать горутину, дороже) или
`runtime.procyield` / пустой цикл с `PAUSE` (недоступно из обычного кода без ассемблера).
Практически доступный вариант — `runtime.Gosched()` в ветке неудачного CAS. Это ровно та
гипотеза, которую наш doc-комментарий в `lockfree_tokenbucket.go` (строки ~107–113)
отвергает по стилистическим соображениям («the project favors readable coordination code
over benchmark-tuned backoff»). Учитывая, что зрелая библиотека сделала обратный выбор и
сослалась на статью, эту строку стоит как минимум переформулировать из «не нужно» в
«взвешено и отклонено, вот довод против».

### Третий путь: убрать петлю совсем (Sentinel)

`ThrottlingController` вообще не имеет CAS-петли: быстрый путь — `get()` + `set()` с
принятой гонкой, медленный — `addAndGet(cost)` с компенсирующим `addAndGet(-cost)` при
отказе. Число атомарных операций на вызов фиксировано, аллокаций ноль, ретраев нет.

Цена — заявленная в комментарии неточность (`// Contention may exist here, but it's okay.`)
и невозможность прогнать под детектором гонок «начисто». Для `ratelimit-lab` это
неприемлемо как реализация (инвариант проекта — `-race` чисто), но интересно как
**замеряемый контрпример**: показать, сколько именно стоит корректность, если рядом
поставить заведомо гоночную версию и сравнить и throughput, и фактический недобор/перебор.

## Сводная таблица возможностей

| | Bucket4j 8.19 | Resilience4j | Guava | Failsafe | Sentinel | Pekko | `ratelimit-lab` |
|---|---|---|---|---|---|---|---|
| Неблокирующий `Allow` | `tryConsume` | `acquirePermission` с `timeout=0` | `tryAcquire(0)` | `tryAcquirePermits` | `canPass` | — (Enforcing) | `Allow`/`AllowN` |
| Блокирующий | `asBlocking()` | да, `timeoutDuration` | `acquire()` | `acquirePermits` | да (`sleep`) | `Shaping` | нет |
| Асинхронный | `asScheduler()` → `CompletableFuture` | `decorateCompletionStage` | нет | да | нет | нативно (Streams) | нет |
| Несколько лимитов | **да**, `Bandwidth[]`, AND по min | нет | нет | нет | да, список `FlowRule` | нет | нет |
| Взять в долг | `consumeIgnoringRateLimits` | `reservePermission` | «платит следующий» | deficit в Bursty | `tryOccupyNext` | нет | нет |
| Вернуть токены | `addTokens` / `forceAddTokens` | `drainPermissions` (обратное) | нет | нет | нет | нет | нет |
| Проба без списания | `estimateAbilityToConsume` | метрики через `calculateNextState` | нет | нет | нет | нет | нет |
| Смена лимита на живом | `replaceConfiguration` + 4 стратегии | `changeLimitForPeriod` | `setRate` | нет | да, через `FlowRuleManager` | нет | нет |
| Warm-up | нет | нет | **`SmoothWarmingUp`** | нет | `WarmUpController` (порт Guava) | нет | нет |
| Greedy / intervally | **обе** + выравнивание по стенным часам | только «intervally» (циклы) | greedy | обе (Smooth/Bursty) | обе | greedy | только greedy |
| Абстракция часов | `TimeMeter` (2 метода) | `Supplier<Long>` | `SleepingStopwatch` (+sleep) | `Stopwatch` | нет (статический `TimeUtil`) | `def currentTime: Long` | `Clock.Now() time.Time` |
| Представление токенов | `long` + точный остаток | `int` за цикл | `double` + `long` момент | `long` нанос | `long` / `double` | `Long` | **`float64`** |
| Эпсилон на границе | нет | нет | нет | нет | `Math.nextUp` в одном месте | нет | **`admitEpsilon = 1e-9`** |
| Lock-free вариант | **да**, дефолт | **да**, дефолт | нет | нет | частично (`AtomicLong`) | нет | **да** |
| События/метрики | `BucketListener` | 3 события + `Metrics` | нет | нет | обширные | нет | нет |
| Распределённый | да (JCache/Redis/SQL/…) | нет | нет | нет | да (кластер) | нет | нет, и не планируется |

## Применимость к ratelimit-lab

_(в работе)_

## Что рассмотрено и отброшено

_(в работе)_

## Источники

_(в работе)_
