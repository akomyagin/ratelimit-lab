# B3 — прокси, балансировщики, API-gateway

> Исследование от 2026-09-06. Каждое нетривиальное утверждение сопровождается
> ссылкой на файл/функцию исходника или на официальную документацию. Где проверить
> не удалось — стоит пометка «**не проверено**». Ветки репозиториев — по умолчанию
> (`master`/`main`) на дату чтения; номера строк могут разъехаться при обновлениях
> вверх по течению, имена функций устойчивее.

Тема: какие алгоритмы rate limiting реально реализованы в инфраструктурном ПО
(reverse-proxy, балансировщики, API-gateway) и **как именно** — арифметика,
хранение состояния, модель конкурентности.

## nginx

Разобрано по коду `master` (nginx/nginx), файлы
`src/http/modules/ngx_http_limit_req_module.c` (1102 строки) и
`ngx_http_limit_conn_module.c` (758 строк).

### `ngx_http_limit_req_module` — leaky bucket «как счётчик»

Официальная документация называет алгоритм прямо: «The limitation is done using the
**"leaky bucket"** method»
([nginx.org, ngx_http_limit_req_module](https://nginx.org/en/docs/http/ngx_http_limit_req_module.html)).
Это **не** очередь запросов, а «leaky bucket as a meter» — ровно та же схема, что и
в нашем `leakybucket.go`: хранится текущая «глубина» очереди, а не сами элементы.

**Арифметика — целочисленная, с фиксированным масштабом 1000.** Это зафиксировано
комментарием прямо в объявлении полей (`ngx_http_limit_req_module.c:26`,
одинаковый комментарий продублирован трижды — на `excess`, на `rate` и на
`burst`/`delay`):

```c
typedef struct {
    ...
    ngx_msec_t                   last;
    /* integer value, 1 corresponds to 0.001 r/s */
    ngx_uint_t                   excess;
    ngx_uint_t                   count;
    u_char                       data[1];
} ngx_http_limit_req_node_t;
```
(`ngx_http_limit_req_module.c:20-30`)

Масштабирование происходит один раз, на разборе конфигурации:

* `ctx->rate = rate * 1000 / scale;` — строка 939; `scale = 1` для `r/s`,
  `scale = 60` для `r/m` (строки 908-915). То есть `rate` внутри — это
  «тысячных запроса в секунду».
* `limit->burst = burst * 1000; limit->delay = delay * 1000;` — строки 1060-1061.
* `nodelay` реализован не отдельным флагом, а через ту же величину:
  `delay = NGX_MAX_INT_T_VALUE / 1000;` (строка 1021) — то есть порог задержки
  ставится в практическую бесконечность, и ни один запрос не задерживается,
  а `burst` продолжает работать как порог отказа.

**Что такое `excess`.** Это заполнение ведра в тысячных долях запроса. Ядро всего
модуля — одна строка (`ngx_http_limit_req_lookup`, строка 454):

```c
excess = lr->excess - ctx->rate * ms / 1000 + 1000;

if (excess < 0) {
    excess = 0;
}
```

Читается так: от прежнего заполнения вычитается «утечка» за `ms` миллисекунд
(`rate * ms / 1000` — так как `rate` уже в тысячных за секунду, деление на 1000
переводит миллисекунды в секунды) и прибавляется **ровно 1000** — стоимость одного
запроса. Отрицательное заполнение зажимается в ноль (ведро не может быть «более чем
пустым»). `ms` предварительно санируется от прыжков часов (строки 445-452: при
`ms < -60000` берётся `ms = 1`, при `ms < 0` — `ms = 0`).

**Как реализован `burst`.** Это просто порог на `excess` — никакой отдельной
очереди:

```c
if ((ngx_uint_t) excess > limit->burst) {
    return NGX_BUSY;      /* → 503 (или status_code) */
}
```
(строка 462). Поскольку `burst` умножен на 1000, `burst=5` означает «допускается
накопить до 5 запросов сверх нормы». Обратите внимание на **строгое** `>`: при
`burst=0` (значение по умолчанию) запрос, попавший ровно в темп, даёт `excess = 1000`
после первого запроса — и следующий немедленный запрос уже отвергается. Отсюда
известное поведение «`rate=1r/s` без `burst` отвергает второй запрос в ту же
секунду».

**Задержка (`delay` / без `nodelay`)** считается обратным преобразованием
(`ngx_http_limit_req_account`, строки 552 и 596):

```c
max_delay = (excess - (*limit)->delay) * 1000 / ctx->rate;
```
— то есть «сколько миллисекунд ведру течь, чтобы `excess` опустился до порога
`delay`». Умножение на 1000 и деление на `rate` (в тысячных r/s) даёт миллисекунды.
Если `excess <= delay`, задержки нет.

**Двухфазность.** `lookup` вызывается с флагом `account`, равным
`(n == lrcf->limits.nelts - 1)` (строка 249) — то есть состояние немедленно
записывается только для *последней* зоны в списке `limit_req`. Для более ранних зон
`lookup` возвращает `NGX_AGAIN`, инкрементирует `lr->count` (строка 476) и
откладывает запись до `ngx_http_limit_req_account` (строка 585). Если какая-то из
последующих зон отвергла запрос — вызывается `ngx_http_limit_req_unlock`
(строки 276, 609-625), и счётчики ранее пройденных зон откатываются. Это
самодельная «атомарность по нескольким лимитам»: запрос, отвергнутый вторым
лимитом, не должен тратить бюджет первого.

**Хранение состояния — red-black tree в разделяемой памяти.** `ctx->sh` —
структура `ngx_http_limit_req_shctx_t` из rbtree + sentinel + LRU-очереди
(строки 33-37), лежащая в shm-зоне (`ngx_shared_memory_add`, строка 941). Ключ узла
— `ngx_crc32_short(key.data, key.len)` (строка 244); коллизии CRC32 разрешаются
сравнением самого ключа (`ngx_memn2cmp`, строка 439). Доступ сериализуется
**одним мьютексом на зону**: `ngx_shmtx_lock(&ctx->shpool->mutex)` вокруг всего
`lookup` (строки 246-251) и вокруг `account` (строки 563-588). То есть nginx —
это mutex-based решение, разделяемое **между процессами-воркерами**, а не между
потоками.

**Вытеснение.** Память зоны конечна, поэтому `ngx_http_limit_req_expire` (строки
630-695) при каждом создании узла вычищает хвост LRU-очереди: узел удаляется, если
`lr->count == 0` и (для второго и далее узла) он не трогался ≥ 60 секунд и его
`excess` уже утёк в ноль (строки 670-684). При невозможности выделить память
логируется `could not allocate node` (строка 501) — то есть переполнение зоны
деградирует до отказа, а не до молчаливого пропуска.

### `ngx_http_limit_conn_module` — не rate limiting вовсе

Это не алгоритм ограничения темпа, а **счётчик одновременных соединений**:
`u_short conn;` в узле (`ngx_http_limit_conn_module.c:21`), инкремент на входе
запроса и декремент в cleanup-хендлере (`ngx_http_limit_conn_cleanup`, строки
392-420). Хранение то же — rbtree в shm под `ctx->shpool->mutex`
(строки 224, 404). Времени в состоянии нет вообще, поэтому «арифметика» здесь
тривиально целочисленная. Для нашей библиотеки прямого аналога нет: наш порт
`Limiter` неблокирующий и не владеет временем жизни запроса, а `limit_conn`
принципиально требует парного release.

## HAProxy

Разобрано по коду `master` (haproxy/haproxy): `src/freq_ctr.c` (261 строка),
`include/haproxy/freq_ctr.h` (441 строка), `include/haproxy/freq_ctr-t.h`,
плюс `src/stick_table.c` для контекста хранения.

**Это вторая реализация приближённого скользящего окна, напрямую сравнимая с нашей —
и, похоже, самая близкая к `slidingwindow.go` из всего разобранного.**

### Состояние — ровно три `unsigned int`

```c
struct freq_ctr {
	unsigned int curr_tick; /* start date of current period (wrapping ticks) */
	unsigned int curr_ctr; /* cumulated value for current period */
	unsigned int prev_ctr; /* value for last period */
};
```
(`include/haproxy/freq_ctr-t.h`)

Это в точности наша тройка «начало текущего окна / счётчик текущего окна / счётчик
предыдущего окна». Комментарий над структурой: «The period is measured in ticks and
must be **at least 2 ticks long**» — причина «двух» вскроется ниже, это не
округление, а следствие того, что младший бит `curr_tick` отдан под флаг блокировки.

### Формула — та же взвешенная, но домноженная на период

Ядро — `_freq_ctr_total_from_values` (`src/freq_ctr.c:79-103`):

```c
remain = tick + period - HA_ATOMIC_LOAD(global_now_ms);
if (unlikely(remain < 0)) {
    /* We're past the first period, check if we can still report a
     * part of last period or if we're too far away.
     */
    remain += period;
    past = (remain >= 0) ? curr : 0;
    curr = 0;
}

if (pend < 0) {
    /* enable flapping correction at very low rates */
    pend = 0;
    if (!curr && past <= 1)
        return past * period;
}

/* compute the total number of confirmed events over the period */
return past * remain + (curr + pend) * period;
```

и потребитель (`include/haproxy/freq_ctr.h:90-95`):

```c
static inline uint read_freq_ctr_period(const struct freq_ctr *ctr, uint period)
{
	ullong total = freq_ctr_total(ctr, period, -1);

	return div64_32(total, period);
}
```

**Сравнение с нашей формулой.** У нас (`internal/limiter/slidingwindow.go:99-100`):

```go
	overlap := float64(w.window-elapsed) / float64(w.window)
	estimate := w.prevCount*overlap + w.currCount
```

У HAProxy после деления на `period`:

```
rate = past * (remain / period) + (curr + pend)
```

где `remain = tick + period - now` — сколько миллисекунд текущего периода **ещё не
истекло**. У нас ровно то же самое записано как `float64(w.window-elapsed) /
float64(w.window)`: `w.window - elapsed` и есть `remain`, `w.window` и есть `period`.
**Формула математически идентична нашей**, вплоть до способа получения веса.

Отличие ровно одно и оно принципиальное: **HAProxy домножает всё выражение на
`period` и держит целые числа**, откладывая единственное деление до самого конца
(`div64_32`). Промежуточный результат имеет тип `ullong`, чтобы `past * remain` не
переполнился. Нашего `admitEpsilon` там нет и не может быть — при целочисленной
арифметике накопление ошибки округления структурно невозможно.

Дополнительные детали, которых у нас нет:

* **Отсечка «два периода назад»** (строки 85-92): если `remain < 0`, значит текущий
  период уже истёк, но ротацию ещё никто не выполнил. Тогда `curr` переезжает в роль
  `past`, а `curr` обнуляется — то есть чтение корректно работает и без записи. Если
  и после `remain += period` результат отрицателен, `past = 0` — счётчик слишком
  старый, окно ушло целиком.
* **«Flapping correction»** (строки 94-99): при `pend < 0` (сигнал «читаем для
  отчётности, не для проверки лимита») и `past <= 1` возвращается `past * period`,
  то есть ровно `past` после деления. Комментарий в заголовке объясняет зачем
  (`freq_ctr.h:82-88`): без этого низкие частоты «мигали» бы между значениями по мере
  утекания окна. Существенно: **для проверки лимитов эта коррекция сознательно не
  применяется** — «For immediate limit checking, it's recommended to use
  `freq_ctr_period_remain()` instead which does not have the flapping correction, so
  that even frequencies as low as one event/period are properly handled».
  Это ровно то различение «оценка для человека» против «оценка для решения», которое
  у нас пока не разведено.
* **`freq_ctr_overshoot_period`** (`src/freq_ctr.c:194-254`) — отдельная, более
  строгая проверка: сравнивает `curr` с линейным равномерным расходом
  `linear_usage = div64_32((uint64_t)elapsed * freq, period)` и возвращает превышение.
  Комментарий: «The caller may safely add new events if result is zero». То есть у
  HAProxy сосуществуют *две* модели: «средняя по скользящему окну» и «не опережай
  равномерный график» — вторая ближе по духу к leaky bucket.

### Ротация и конкурентность — CAS с битом-замком в поле времени

Быстрый путь (`freq_ctr.h:49-66`) вообще не использует CAS:

```c
curr_tick  = HA_ATOMIC_LOAD(&ctr->curr_tick);
if (likely(now_ms - curr_tick < period))
	return HA_ATOMIC_ADD_FETCH(&ctr->curr_ctr, inc);

return update_freq_ctr_period_slow(ctr, period, inc);
```

— то есть в подавляющем большинстве случаев обновление счётчика это один
атомарный `fetch_add`, без цикла и без чтения глобальных часов (комментарий строк
53-60 отдельно оговаривает, что локальные часы потока используются намеренно, ибо
«accessing this shared variable is extremely expensive»).

Медленный путь `update_freq_ctr_period_slow` (`src/freq_ctr.c:26-65`) выполняет
ротацию под самодельным замком: **младший бит `curr_tick` работает как lock-бит**,
захватываемый через CAS:

```c
if (!(curr_tick & 1) &&
    HA_ATOMIC_CAS(&ctr->curr_tick, &curr_tick, curr_tick | 0x1))
	break;
```

Далее ротация делается атомарным обменом, чтобы не потерять конкурентные инкременты:
`HA_ATOMIC_STORE(&ctr->prev_ctr, HA_ATOMIC_XCHG(&ctr->curr_ctr, inc));` (строка 54).
Комментарий поясняет, почему это безопасно: ротирующий один (он взял бит), остальные
только прибавляют к `curr_ctr`. Пропуск двух и более периодов обрабатывается явно
(строки 56-60: `prev_ctr = 0`, `curr_tick = now_ms_tmp`).

Комментарий в заголовке `update_freq_ctr_period_slow` даёт **измеренную частоту
медленного пути: «falls back to this one when needed (less than 0.003% of the
time)»** — то есть авторы явно оптимизировали под то, что CAS-цикл почти никогда не
исполняется.

Чтение (`freq_ctr_total`, строки 115-165) — не блокирующее, а **seqlock-подобное**:
значения читаются дважды, и если между чтениями что-то изменилось или взведён
lock-бит, чтение повторяется (метки `redo0`..`redo3`). Это гарантирует
согласованный снимок тройки без взятия замка на чтение. Обратите внимание: снимок
строится из трёх независимых атомарных загрузок с проверкой стабильности, а не из
одного указателя на неизменяемую структуру, как у нас в `lockfree_tokenbucket.go`.
Обе техники решают одну задачу «получить согласованный снимок нескольких полей»;
наша дешевле по чтению, но платит аллокацией на успешную запись.

### Хранение состояния — stick tables

`freq_ctr` не живёт сам по себе: он лежит полем в записи stick-table
(`STKTABLE_DT_HTTP_REQ_RATE`, `.std_type = STD_T_FRQP`,
`src/stick_table.c:1875`). Чтение выборкой `sc_http_req_rate` идёт через
`smp_fetch_http_req_rate` (`src/stick_table.c:4765-4791`) и берёт **rwlock записи**
поверх уже атомарного `freq_ctr`:

```c
HA_RWLOCK_RDLOCK(STK_SESS_LOCK, &stkctr_entry(stkctr)->lock);
smp->data.u.sint = read_freq_ctr_period(&stktable_data_cast(ptr, std_t_frqp),
                       stkctr->table->data_arg[STKTABLE_DT_HTTP_REQ_RATE].u);
HA_RWLOCK_RDUNLOCK(STK_SESS_LOCK, &stkctr_entry(stkctr)->lock);
```

Сама таблица шардирована на `buckets[CONFIG_HAP_TBL_BUCKETS]`, у каждого ведра свой
`sh_lock` (`include/haproxy/stick_table-t.h:214-219`), плюс отдельный `updt_lock`,
про который в структуре прямо написано «this lock is heavily used and must be on its
own cache line» (строки 233-234). То есть иерархия: шардированные rwlock'и на
структуру таблицы → rwlock на запись → атомарные операции внутри `freq_ctr`.

Важное отличие от nginx: **у HAProxy это межпоточная синхронизация внутри одного
процесса** (HAProxy многопоточный), а не межпроцессная через shm. Это ближе к нашей
модели, чем nginx.

Существенно и то, что **HAProxy не предоставляет готовой директивы «ограничь до N
r/s»**: `sc_http_req_rate` — это *выборка*, значение, которое пользователь сам
сравнивает с порогом в ACL. Ограничитель собирается из кирпичей в конфигурации, а не
включается флагом. Для нас это релевантно как аргумент в пользу того, что порт
`Limiter` — не единственная разумная форма API; форма «дай текущую оценку темпа»
(что-то вроде `Rate() float64`) — самостоятельная альтернатива.

## Envoy

Разобрано по коду `main` (envoyproxy/envoy):
`source/common/common/token_bucket_impl.{h,cc}` (134 + 118 строк) и
`source/extensions/filters/common/local_ratelimit/local_ratelimit_impl.{h,cc}`
(179 + 393 строки).

**Алгоритм — token bucket, арифметика — `double` (float64).** Это самый ценный
контрпример к «индустрия считает целыми».

### Две реализации: не-потокобезопасная и атомарная

`TokenBucketImpl` (заголовок, строка 11-12: «A class that implements token bucket
interface (**not thread-safe**)») — прямой аналог нашего `tokenbucket.go`, только без
мьютекса вообще: синхронизацию обеспечивает вызывающий. Поля — `double max_tokens_`,
`double fill_rate_`, `double tokens_`, `MonotonicTime last_fill_`
(`token_bucket_impl.h:31-35`). Пополнение (`token_bucket_impl.cc:19-38`):

```c++
if (tokens_ < max_tokens_) {
    const auto time_now = time_source_.monotonicTime();
    tokens_ = std::min((std::chrono::duration<double>(time_now - last_fill_).count() * fill_rate_) +
                           tokens_,
                       max_tokens_);
    last_fill_ = time_now;
}
```

Это буквально наш `refill`: дельта времени в секундах (тоже `double`) × `fill_rate`,
зажатое по `max_tokens`. Обратите внимание на охрану `if (tokens_ < max_tokens_)` —
на полном ведре часы вообще не читаются, что экономит вызов `monotonicTime()`. У нас
такой охраны нет.

Также заметьте: **проверка допуска у Envoy — простое `if (tokens_ < tokens) return 0;`
(строка 32), без всякой эпсилон-поправки.** Наш `admitEpsilon` — это не общепринятая
практика; ближайший по конструкции код её не делает.

### `AtomicTokenBucketImpl` — CAS не по токенам, а по времени

Вот это стоит прочитать целиком (`token_bucket_impl.h:56-81`), потому что это прямое
и более экономное решение той же задачи, что решает наш `lockfree_tokenbucket.go`:

```c++
// This reference https://github.com/facebook/folly/blob/main/folly/TokenBucket.h.
template <class GetConsumedTokens> double consume(const GetConsumedTokens& cb) {
    const double time_now = timeNowInSeconds();

    double time_old = time_in_seconds_.load(std::memory_order_relaxed);
    double time_new{};
    double consumed{};
    do {
      const double total_tokens = std::min(max_tokens_, (time_now - time_old) * fill_rate_);
      if (consumed = cb(total_tokens); consumed == 0) {
        return 0;
      }
      ...
      const double total_tokens_new = total_tokens - consumed;
      time_new = time_now - (total_tokens_new / fill_rate_);
    } while (
        !time_in_seconds_.compare_exchange_weak(time_old, time_new, std::memory_order_relaxed));

    return consumed;
}
```

Всё состояние — **одно поле `std::atomic<double> time_in_seconds_`**
(`token_bucket_impl.h:130`). Количество токенов не хранится вообще, оно *выводится*
из времени: `total_tokens = min(max_tokens, (now - time_old) * fill_rate)`.
Потребление токена реализуется не как декремент счётчика, а как **сдвиг «времени
последнего опустошения» вперёд**. Это классический трюк виртуального времени
(в теории очередей известен как virtual scheduling / GCRA); авторы ссылаются на
`folly/TokenBucket.h`.

Три вывода, важных для нашего проекта:

1. **Снапшот не нужен.** Наш `lockfree_tokenbucket.go` держит
   `atomic.Pointer` на неизменяемую структуру, потому что состояние — пара
   (токены, время). Envoy сводит состояние к **одному** `double`, и тогда хватает
   обычного CAS на 8 байтах: **ни аллокации на успешную запись, ни указателя, ни
   давления на GC.** Это строго дешевле нашей схемы. В Go эквивалент —
   `atomic.Uint64` + `math.Float64bits`/`math.Float64frombits`.
2. **Отказ без CAS — общепринятое решение, а не наша самодеятельность.** Строки
   64-66: `if (consumed = cb(total_tokens); consumed == 0) { return 0; }` — выход из
   цикла **до** `compare_exchange_weak`. Это ровно то расхождение с mutex-версией,
   которое зафиксировано в наших doc-комментариях `lockfree_tokenbucket.go` и в
   `docs/plans/stage-4-lockfree-tokenbucket.md`. Envoy делает так же и по той же
   причине. Наше решение подтверждается независимо.
3. **`compare_exchange_weak` с `memory_order_relaxed`.** Порядок памяти самый слабый
   из возможных: у лимитера нет данных, которые он должен «опубликовать» другим
   потокам, кроме самого счётчика. В Go выбора нет — `atomic` из `sync/atomic` даёт
   sequentially consistent семантику, так что наш CAS дороже принципиально.

Отдельно любопытно, что «отрицательное потребление» используется как штатный приём
возврата токенов: `RateLimitTokenBucket::refill` (`local_ratelimit_impl.cc:90-104`)
передаёт колбэк, возвращающий отрицательное значение, и комментарий в шаблоне это
разрешает («The consumed is negative. It means the token is added back to the
bucket», `token_bucket_impl.h:70`).

### `fill_interval` — это не таймер, а способ задать дробную ставку

Вопреки названию, в текущем коде `fill_interval` не порождает периодического
таймера. Он немедленно сворачивается в непрерывную ставку
(`local_ratelimit_impl.cc:77-83`):

```c++
    : token_bucket_(max_tokens, time_source,
                    // Calculate the fill rate in tokens per second.
                    tokens_per_fill / std::chrono::duration<double>(fill_interval).count()),
```

То есть `tokens_per_fill=100, fill_interval=1s` даёт `fill_rate_ = 100.0` токенов в
секунду, и пополнение получается непрерывным, а не ступенчатым. Пара
(`tokens_per_fill`, `fill_interval`) существует лишь как удобный способ выразить
дробную ставку целыми числами в конфигурации — например «5 запросов в минуту» без
записи `0.0833`. Ограничение `fill_interval >= 50ms` осталось и проверяется явно
(строки 124-126), сообщение об ошибке — `"local rate limit token bucket fill timer
must be >= 50ms"`; слово «timer» выдаёт, что это наследие прежней реализации на
таймере. **Не проверено**, когда именно произошёл переход с таймера на непрерывную
ставку.

### Модель конкурентности и «share factor»

Официальная документация формулирует прямо: «Depending on the value of the config
`local_rate_limit_per_downstream_connection`, the token bucket is either **shared
across all workers** or on a per connection basis. This results in the local rate
limits being applied either per Envoy process or per downstream connection. By default
the rate limits are applied per Envoy process»
([Envoy docs, local rate limit filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter)).
Именно поэтому ведро потокобезопасное: **одно ведро на процесс, доступное всем
worker-потокам**. Это ровно наша модель — в отличие от nginx (межпроцессная shm) и от
per-worker шардирования.

Отдельный механизм — `ShareProvider`. `DefaultEvenShareMonitor`
(`local_ratelimit_impl.cc:20-32`) вычисляет `share_factor = num == 0 ? 1.0 : 1.0 / num`,
где `num` — число хостов в локальном кластере, и хранит его в `std::atomic<double>`.
Затем `RateLimitTokenBucket::consume` (строки 84-88) делает:

```c++
auto cb = [tokens = to_consume / factor](double total) { return total < tokens ? 0.0 : tokens; };
```

— то есть при меньшей доле **списывается больше токенов за запрос**, а не
уменьшается ёмкость ведра. Это дешёвая аппроксимация глобального лимита без
координации: каждый инстанс режет себе бюджет пропорционально числу соседей.

### Global rate limit (gRPC RLS)

Глобальный вариант — вынесенный сервис по протоколу
[`RateLimitService`](https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ratelimit/v3/rls.proto);
эталонная реализация — отдельный проект `envoyproxy/ratelimit` на Go с Redis/Memcached.
Алгоритм там **не** token bucket. **Детально не разбирался** — тема относится к
распределённым лимитерам, которые вне границ MVP `ratelimit-lab` (нет сети). См.
раздел «Что рассмотрено и отброшено».

## lua-resty-limit-traffic (OpenResty)

Прочитано целиком, ветка `master` (openresty/lua-resty-limit-traffic),
`lib/resty/limit/{req,count,conn,traffic}.lua` — 153 + 138 + 125 + 58 строк. Это
действительно самый читаемый код в подборке: три алгоритма умещаются в ~400 строк.

Ключевая архитектурная особенность всех трёх: **они не решают, а возвращают
задержку.** Сигнатура `incoming(key, commit)` возвращает `delay, state` при допуске
и `nil, "rejected"` при отказе. Решение «спать или отвергать» принимает вызывающий
Lua-код. То есть библиотека сознательно отделяет расчёт от политики — у нас это
слито в `bool`.

Второй общий приём: **аргумент `commit`**. `incoming(key, false)` считает, но не
записывает состояние; `incoming(key, true)` записывает. Это ровно nginx'овская
двухфазность, вынесенная в публичный API, и `traffic.lua:21-55` использует её для
комбинирования нескольких лимитеров с откатом:

```lua
        if not delay then
            for j = 1, i - 1 do
                -- we intentionally ignore any errors returned below.
                limiters[j]:uncommit(keys[j])
            end
            limiters[n]:uncommit(keys[n])
            return nil, err
        end
```

Для нашего порта `Limiter` это релевантно: у нас нет ни `commit`, ни `uncommit`, и
скомбинировать два лимитера «атомарно» невозможно — второй отказ уже потратил бюджет
первого.

### `resty.limit.req` — leaky bucket, порт nginx

Заголовочный комментарий: «This library is an approximate Lua port of the standard
`ngx_limit_req` module» (`req.lua:3-4`). И действительно, там тот же масштаб 1000 и
даже тот же комментарий:

```lua
ffi.cdef[[
    struct lua_resty_limit_req_rec {
        unsigned long        excess;
        uint64_t             last;  /* time in milliseconds */
        /* integer value, 1 corresponds to 0.001 r/s */
    };
]]
```
(`req.lua:25-31`)

Формула (`req.lua:95-96`):

```lua
excess = max(tonumber(rec.excess) - rate * abs(elapsed) / 1000 + 1000,
             0)
```

— идентична nginx'овской, включая слагаемое `+ 1000` и зажим в ноль; `rate` и
`burst` так же домножены на 1000 в конструкторе (`req.lua:60-61`).

**Гибрид арифметики.** Здесь тонкость, которой нет в nginx: в Lua *все* числа —
`double`, поэтому `excess` считается в плавающей точке. Но хранится он в
`unsigned long` внутри FFI-структуры, то есть при `rec_cdata.excess = excess`
(строка 109) дробная часть **отбрасывается**. Получается «считаем во float,
персистим в int» — и это само по себе избавляет от накопления дрейфа: усечение при
каждой записи не даёт ошибке накапливаться между вызовами. Наш `admitEpsilon` решает
ту же задачу иначе — терпимостью на сравнении, а не квантованием состояния.

Отличие от nginx: **порога `delay` нет вовсе**, возвращается
`excess / rate, excess / 1000` (строка 115) — задержка в секундах и `excess` в
запросах. Аналог `nodelay` достигается тем, что вызывающий просто игнорирует
`delay`. Это подтверждает, что `delay`/`nodelay` в nginx — вопрос политики, а не
алгоритма.

**Честно признанная гонка.** Комментарий перед `incoming` (`req.lua:70-72`):

```
-- FIXME we have a (small) race-condition window between dict:get() and
-- dict:set() across multiple nginx worker processes. The size of the
-- window is proportional to the number of workers.
```

То есть `lua_shared_dict` даёт атомарность на одной операции, но не на паре
get/set — и авторы сознательно приняли неточность вместо блокировки. Для нас это
прямая иллюстрация того, чего стоит наш инвариант «все реализации безопасны при
конкурентном использовании»: продовая библиотека его *не* держит и живёт с этим.

### `resty.limit.count` — фиксированное окно, GitHub-style

Комментарий в первой строке (`count.lua:1-2`): «implement GitHub request rate
limiting: https://developer.github.com/v3/#rate-limiting». Это **фиксированное окно**
на счётчике с TTL, целочисленное:

```lua
    if commit then
        remaining, err = dict:incr(key, -1, limit, window)
        ...
    else
        remaining = (dict:get(key) or limit) - 1
    end

    if remaining < 0 then
        return nil, "rejected"
    end
```
(`count.lua:56-67`)

Счётчик идёт **вниз** от `limit`, а окно задаётся TTL ключа — четвёртый аргумент
`incr` (`init_ttl`). Есть ветвь `incoming_old` для OpenResty старше 0.10.12
(`count.lua:73-115`), где `incr` без `init_ttl` и TTL ставится отдельным `expire` —
и там видна вся возня с гонкой: если ключ истёк между `incr` и `expire`, всё
повторяется (строки 88-99). Хорошая иллюстрация того, почему атомарный
«инкремент-с-TTL» ценен.

Никакой арифметики со временем внутри нет вообще: окно реализовано **истечением
ключа**, а не вычислением. Это самый дешёвый из всех разобранных алгоритмов и, как
следствие, самый «дырявый» — классическая проблема всплеска `2×limit` на стыке окон.

### `resty.limit.conn` — счётчик соединений с адаптивной задержкой

Целочисленный `incr` по ключу, порог `max + burst` (`conn.lua:58`), парный
`leaving(key, req_latency)`. Занятная деталь — **самонастраивающаяся оценка задержки**
(`conn.lua:98-101`):

```lua
    if req_latency then
        local unit_delay = self.unit_delay
        self.unit_delay = (req_latency + unit_delay) / 2
    end
```

— экспоненциальное скользящее среднее с коэффициентом 0.5 по фактической латентности
запроса, чтобы оценить, сколько ждать избыточным соединениям:
`self.unit_delay * floor((conn - 1) / max)` (строка 76). Никакого времени в
разделяемом состоянии при этом нет — `unit_delay` живёт per-worker в объекте
лимитера. Прямого аналога у нас нет и не планируется.

## Kong

Разобрано по коду `master` (Kong/kong): `kong/plugins/rate-limiting/policies/init.lua`,
`kong/plugins/rate-limiting/handler.lua`, `kong/tools/timestamp.lua`.

**Алгоритм — фиксированные окна, выровненные по календарю, счётчики целые.** Это
единственный продукт в подборке, где окна привязаны к настенным часам, а не к моменту
первого запроса.

`kong/tools/timestamp.lua`, функция `get_timestamps(now)` возвращает таблицу из шести
меток, каждая — начало соответствующего календарного периода:

```lua
  timetable.sec = math_floor(timetable.sec)   -- reduce to second precision
  stamps.second = timetable:timestamp() * 1000

  timetable.sec = 0
  stamps.minute = timetable:timestamp() * 1000

  timetable.min = 0
  stamps.hour = timetable:timestamp() * 1000
  ...
```

То есть поля времени последовательно обнуляются: `second`, `minute`, `hour`, `day`,
`month`, `year`. Ключ счётчика собирается из идентификатора, имени периода и **этой
метки** (`get_local_key`, `policies/init.lua:68`), поэтому смена окна происходит не
по таймеру, а автоматически — при переходе через границу меняется ключ.

Инкремент в политике `local` (`policies/init.lua:302-316`) — обычный целочисленный
`incr` по `lua_shared_dict` с TTL:

```lua
          local cache_key = get_local_key(conf, identifier, period, period_date)
          local newval, err = shm:incr(cache_key, value, 0, EXPIRATION[period])
```

Никакой арифметики со временем внутри лимитера нет вообще — всё делает выбор ключа
плюс TTL. Следствия:

* лимиты **комбинируются**: можно задать `second=5, minute=100, hour=1000`
  одновременно, каждый — свой набор счётчиков;
* «минута» означает астрономическую минуту с секунды 00, а не «последние 60 секунд»;
* всплеск на границе окон — как у любого fixed window; в пределе `2×limit`.

Три стратегии различаются **только хранилищем**, не алгоритмом
(`policies/init.lua:302, 330, 366`):

* `local` — `ngx.shared.kong_rate_limiting_counters`, счётчики per-node, не
  синхронизируются между узлами Kong;
* `cluster` — счётчики в БД Kong (`kong.db`) через `policy.increment(db.connector, ...)`;
* `redis` — Redis, причём с необязательной пакетной синхронизацией: локальный буфер
  `cur_usage[db_key]` и фоновый `sync_to_redis`, который сливает накопленную дельту
  скриптом `redis.call("incrby", key, value)` (`policies/init.lua:177-204`), а
  `clear_local_counter` (строки 168-173) буфер сбрасывает. Есть режим
  `SYNC_RATE_REALTIME` без буферизации. То есть точность обменивается на число
  round-trip'ов — параметром конфигурации.

### `rate-limiting-advanced`

Плагин **не с открытым исходным кодом** — он входит в Kong Enterprise, репозитория
`kong-ee` в публичном доступе нет, поэтому **по коду не проверено**. По официальной
документации, плагин поддерживает `window_type` со значениями `fixed` и `sliding`, а
sliding описывается как взвешенная оценка по предыдущему окну
([Kong docs, Rate Limiting Advanced](https://developer.konghq.com/plugins/rate-limiting-advanced/)).
Утверждать что-либо о его внутренней арифметике по коду я не могу — доступа нет.

## Apache APISIX

Разобрано по коду `master` (apache/apisix): `apisix/plugins/limit-req.lua` (196
строк), `limit-count.lua` (49 строк), `limit-conn.lua` (145 строк),
`apisix/plugins/limit-count/limit-count-local.lua`,
`apisix/plugins/limit-count/sliding-window/sliding-window.lua` (218 строк) и
`.../store/shared-dict.lua`.

**Ключевой факт: `limit-req` — это не своя реализация, а прямой вызов
`lua-resty-limit-traffic`.** `limit-req.lua:17`:

```lua
local limit_req_new                     = require("resty.limit.req").new
```

и создание для политики `local` (строка 112):

```lua
    if conf.policy == "local" then
        return limit_req_new("plugin-limit-req", conf.rate, conf.burst)
```

То есть весь разбор из раздела про OpenResty применим дословно: leaky bucket,
масштаб 1000, `excess`. Помимо `local` есть `redis` и `redis-cluster` —
переписывания того же алгоритма на Lua-скриптах Redis
(`apisix/plugins/limit-req/limit-req-redis.lua`, **детально не читал**).
`limit-count` для не-sliding режима так же делегирует в `resty.limit.count`
(`limit-count-local.lua`, `local limit_count = require("resty.limit.count")`).

### Своя реализация есть — sliding window в `limit-count`

`limit-count/sliding-window/sliding-window.lua` — **третья независимая реализация
взвешенного скользящего окна в этой подборке, и формула снова наша.**

Расчёт (`sliding-window.lua:116-118, 144-146`):

```lua
    local counter_key = get_counter_key(self, key, now)
    local last_counter_key = get_counter_key(self, key, now - self.window_size)
    local remaining_time = self.window_size - now % self.window_size
    ...
    local accepted, count, last_count = res[1], res[2], res[3]
    local last_rate = last_count / self.window_size
    local estimated_last_window_count = last_rate * remaining_time
```

и решение в хранилище (`store/shared-dict.lua:63-70`):

```lua
    if last > limit then
        last = limit
    end
    ...
    local estimated = last / window_size * remaining_time + cur
    if cur >= limit or estimated >= limit then
        return {0, cur, last}
    end
```

Сопоставление с нашим `slidingwindow.go`:

| | ratelimit-lab | APISIX |
|---|---|---|
| формула | `prevCount*overlap + currCount` | `last / window_size * remaining_time + cur` |
| вес | `overlap ∈ [0,1)` | `remaining_time / window_size` |
| тип | `float64` | Lua `number` (= `double`) |
| зажим `prev` | нет | `if last > limit then last = limit end` |
| доп. проверка | нет | `cur >= limit` отдельно от `estimated >= limit` |

Формула совпадает, арифметика тоже с плавающей точкой. Но **два защитных приёма у нас
отсутствуют**:

1. **Зажим предыдущего счётчика по лимиту.** Если в прошлом окне каким-то образом
   накопилось больше `limit` (гонка, изменение конфигурации, `cost > 1`), нашей
   формуле это даст завышенную оценку на всё следующее окно. APISIX зажимает.
2. **Отдельная проверка `cur >= limit`.** Взвешенная оценка на позднем этапе окна
   почти вырождается в `cur` (вес прошлого окна стремится к нулю), но около границы
   `estimated` может остаться ниже лимита даже при уже исчерпанном текущем окне.
   Отдельная проверка это закрывает.

Окна выбираются нарезкой оси времени, а не привязкой к первому запросу
(`get_window_id`, строка 36-37): `return tostring(math_floor(time / self.window_size))`
— то есть окна выровнены по эпохе, ключ содержит номер окна, а TTL ставится
`window_size * 2` (строка 130), чтобы предыдущее окно ещё было доступно.

Комментарии в коде фиксируют интересные грабли (`sliding-window.lua:162-171,
210-213`), в частности: репортить `limit - new_count` вместо взвешенного остатка
нельзя, иначе «degrading the sliding window to a fixed one». Ещё одно подтверждение,
что «оценка для отчёта» и «оценка для решения» должны считаться одинаково.

`limit-conn` — снова обёртка над счётчиком соединений, как у OpenResty/nginx.

## Traefik

Разобрано по коду `master` (traefik/traefik), директория
`pkg/middlewares/ratelimiter/`: `rate_limiter.go` (189 строк),
`in_memory_limiter.go` (69 строк), `lua.go`, `redis_limiter.go`, `ttlmap/`.

**Ответ на прямой вопрос из брифа: да, Traefik использует `golang.org/x/time/rate`,
это подтверждается кодом.** Импорт в обоих файлах, и в `in_memory_limiter.go`:

```go
type inMemoryRateLimiter struct {
	rate  rate.Limit // reqs/s
	burst int64
	...
	buckets *ttlmap.Map[*rate.Limiter] // actual buckets, keyed by source.
}
```

Заголовочный комментарий пакета формулирует алгоритм прямо: «Package ratelimiter
implements a rate limiting and traffic shaping middleware with a set of **token
buckets**» (`rate_limiter.go:1`).

Что здесь интересного помимо самого выбора:

* **Собственного алгоритма нет вообще.** Один экземпляр `rate.Limiter` на источник
  трафика, ключом служит IP/заголовок/хост. Traefik пишет только обвязку: хранение,
  вытеснение, преобразование конфигурации.
* **`Reserve()`, а не `Allow()`.** `in_memory_limiter.go` берёт резервацию и смотрит
  на задержку; если она больше `maxDelay`, резервация отменяется:
  ```go
  res := bucket.Reserve()
  if !res.OK() {
      return nil, nil
  }
  delay := res.Delay()
  if delay > i.maxDelay {
      res.Cancel()
  }
  ```
  Это способ получить «сколько ждать» вместе с решением — то, чего наш `bool`-порт
  не даёт. Обратите внимание, что `Cancel()` возвращает токены обратно — то есть
  отказ по `maxDelay` не тратит бюджет.
* **`maxDelay` выведено из ставки** (`rate_limiter.go:82-89`): `time.Second /
  (time.Duration(rtl) * 2)`, то есть половина периода между запросами. Комментарий
  честно говорит «For now it is somewhat arbitrarily set to 1/(2*rate)»
  (`rate_limiter.go:34-35`), а для ставок ниже 1 r/s это не масштабируется и
  зажимается в 500 мс (строки 82-86).
* **Ограничение памяти — `maxSources = 65536`** (`rate_limiter.go:22`) плюс TTL,
  вычисляемый обратно пропорционально ставке (строки 96-101): чем реже лимитер
  используется, тем дольше он живёт. Это ровно та проблема, которую nginx решает
  LRU-вытеснением из shm-зоны.
* **Redis-вариант — Lua-переписывание того же token bucket** (`lua.go`,
  `AllowTokenBucketRaw`): `local delta = bucket.limit * elapsed; local tokens =
  bucket.tokens + delta; tokens = math.min(tokens, bucket.burst); tokens = tokens - 1`.
  Числа в Lua — `double`, то есть арифметика снова с плавающей точкой. Токенам
  разрешено уходить в минус (модель «долга» из `x/time/rate`), и при превышении
  `max_delay` они возвращаются: `tokens = tokens + 1`.

### `golang.org/x/time/rate` — эталон Go-экосистемы, проверено по коду

Поскольку это ближайший к нам по языку ориентир, стоит зафиксировать его устройство
(golang/time, `rate/rate.go`):

* Состояние: `mu sync.Mutex`, `limit Limit`, `burst int`, **`tokens float64`**, `last
  time.Time`, `lastEvent time.Time` (строки 57-63). То есть **mutex + float64** —
  ровно наш `tokenbucket.go`.
* Пополнение — `func (lim *Limiter) advance(t time.Time) (newTokens float64)`
  (строка 387): `delta := lim.limit.tokensFromDuration(elapsed); tokens :=
  lim.tokens + delta`, затем зажим по `burst`.
* Конверсии — `durationFromTokens(tokens float64) time.Duration` (строка 405) и
  `tokensFromDuration(d time.Duration) float64` (строка 422).
* **Эпсилон-поправки в допуске нет**: `ok := n <= lim.burst && waitDuration <=
  maxFutureReserve` (строка 362), а до этого `tokens -= float64(n)` и проверка знака.

Это второй после Envoy весомый контрпример «индустрия считает целыми»: самая
используемая Go-реализация token bucket работает на `float64` и без эпсилона.

## Caddy (caddy-ratelimit)

Разобрано по коду `master` (mholt/caddy-ratelimit): `ringbuffer.go` (212 строк),
`handler.go`, `README.md`. Модуль неофициальный, что README оговаривает явно
(«This is not an official repository of the Caddy Web Server organization»).

**Это пятый алгоритм, которого у нас нет: точный sliding window log на кольцевом
буфере фиксированного размера.**

```go
type ringBufferRateLimiter struct {
	mu     sync.Mutex
	window time.Duration
	ring   []time.Time // len(ring) == maxEvents
	cursor int         // always points to the oldest timestamp
}
```
(`ringbuffer.go:26-31`)

Вся логика допуска — четыре строки (`ringbuffer.go:66-75`):

```go
func (r *ringBufferRateLimiter) allowed() bool {
	if len(r.ring) == 0 {
		return false
	}
	if now().Sub(r.ring[r.cursor]) > r.window {
		r.reserve()
		return true
	}
	return false
}
```

Идея: кольцо длины `maxEvents` хранит времена последних `maxEvents` событий, курсор
всегда указывает на **самое старое**. Если самое старое событие вышло за окно —
значит в окне меньше `maxEvents` событий, допускаем и затираем самую старую ячейку
текущим временем (`reserve` + `advance`, строки 81-94).

Свойства, интересные для нас:

* **Арифметики над состоянием нет вообще.** Ни счётчиков, ни токенов, ни `float64`,
  ни целых с масштабом — только сравнение `time.Time`. Вопрос о дрейфе и об
  `admitEpsilon` в такой конструкции не возникает в принципе. **Это самый сильный
  аргумент «а можно вообще без арифметики»** из всей подборки.
* **Точность — абсолютная**, в отличие и от нашего взвешенного sliding window, и от
  HAProxy: никакой аппроксимации, окно настоящее скользящее.
* **Цена — память O(maxEvents) на ключ.** README прямо формулирует: «Memory `O(Kn)`
  where: `K` = events allowed in window (constant, configurable), `n` = number of
  rate limits allocated in zone». Для `max_events = 100000` это 100k `time.Time`
  (24 байта каждый) на один ключ — то есть алгоритм применим только при небольшом
  лимите. Наши три алгоритма все дают O(1) на ключ; это осознанный размен, и Caddy
  разменивает в другую сторону.
* **Подсчёт занятых слотов — O(n)**, и автор это знает: комментарий над
  `countUnsynced` (строка 182) — «TODO: this is currently O(n) but could probably
  become O(log n)...». Но на горячем пути (`allowed`) счёт не нужен, там честный
  O(1); `Count` используется только для распределённого режима и метрик.
* **Инжекция времени — переменная пакетного уровня**, а не интерфейс:
  ```go
  // Current time function, to be substituted by tests
  var now = time.Now
  ```
  (`ringbuffer.go:211-212`). Это дешевле нашего `Clock`-интерфейса (нет
  косвенного вызова через iface), но и заметно хуже: подмена глобальна, значит
  тесты нельзя гонять параллельно, и разные лимитеры не могут жить на разных часах.
  Наш выбор интерфейса подтверждается как более правильный; выигрыш пакетной
  переменной чисто микрооптимизационный.
* **Блокировка — обычный `sync.Mutex`**, lock-free варианта нет.
* README отдельно противопоставляет модель памяти nginx'у: «Unlike nginx's rate
  limit module, this one does not require you to set a memory bound. Instead, rate
  limiters are scanned every so often and expired ones are deleted... Caddy does not
  drop rate limiters on the floor and forget events like nginx does». Уборка — одна
  фоновая горутина (`go h.sweepRateLimiters(ctx)`, `handler.go:190`).
* Распределённый режим (`distributed.go`) — периодический обмен состоянием через
  общее хранилище; README называет его «inherently approximate, but also eventually
  consistent». Вне наших границ.

## Прочее

### Tyk — четыре алгоритма на выбор, и у них есть `epsilon`

Код: TykTechnologies/tyk, `internal/rate/limiter/` (файлы
`limiter_token_bucket.go`, `limiter_sliding_window.go`, `limiter_fixed_window.go`,
`limiter_leaky_bucket.go`) — тонкие обёртки над отдельной библиотекой
`github.com/TykTechnologies/exp/pkg/limiters`, где лежат все четыре реализации.
Плюс `internal/rate/sliding_log.go` — sliding **log** на Redis-ZSET
(`ZRemRangeByScore` + `ZCard` + `ZAdd`, строки 94-102), точный аналог кольцевого
буфера Caddy, только распределённый.

Самое ценное здесь для нас — `exp/pkg/limiters/sliding_window.go`, потому что это
**единственная найденная реализация с явным эпсилоном**, и она снова использует нашу
формулу:

```go
	total := float64(prev*int64(ttl))/float64(s.rate) + float64(curr)
	if total-float64(s.capacity) >= s.epsilon {
```
(`sliding_window.go:41-42`)

Комментарий к конструктору (строка 24): «Epsilon is the max-allowed range of
difference when comparing the current weighted number of requests with capacity».
Обратите внимание на устройство: `prev*int64(ttl)` считается **в целых**, и только
затем делится на `float64(s.rate)` — то есть точность сохраняется дольше, чем при
нашем `prevCount * overlap`, где `overlap` уже округлён.

**Но эпсилон там про другое, и в проде он выключен.** Вызывающий код
(`tyk/internal/rate/limiter/limiter_sliding_window.go`) передаёт `0` и объясняет
почему:

```go
	// TODO: when doing rate sliding rate limits, the counts for two windows are
	//       used, ...
	//       the epsilon value is used to allow some requests to go over the defined
	//       rate limit at any point of the calculation (start of window, end of ...).
	limiter := limiters.NewSlidingWindow(capacity, ttl, storage, l.clock, 0)
```

То есть у Tyk `epsilon` — **ручка снисходительности** («разрешить чуть-чуть выйти за
лимит»), а не поправка на дрейф `float64`, и она установлена в ноль. Это единственный
найденный аналог нашего `admitEpsilon`, и семантика у него другая.

Ещё деталь, совпадающая с нашей: `clock Clock` — инжектируемые часы через интерфейс
(`sliding_window.go:15`), как у нас.

### KrakenD — token bucket на целых, с ленивым пополнением

Код: krakend/krakend-ratelimit, `tokenbucket.go` (136 строк). API почти буквально
наш:

```go
type TokenBucket struct {
	fillInterval time.Duration
	capacity     uint64
	tokens       uint64
	clock        Clock
	lastRefill   time.Time
	mu           *sync.Mutex
}

func (t *TokenBucket) Allow() bool {
```

плюс `Clock` — интерфейс с `Now()` и `Since()` (строки 14-18). То есть внешне это
наш `tokenbucket.go`. Внутри — два принципиальных отличия.

**1. Токены целые (`uint64`), ставка выражена интервалом пополнения:**

```go
		fillInterval: time.Duration(int64(1e9 / r)),
```
(конструктор). То есть `rate float64` превращается в «наносекунд на один токен» — и
дальше вся арифметика идёт в `time.Duration` (это `int64`) и `uint64`. Пополнение:

```go
	tokensToAdd := uint64(t.clock.Since(t.lastRefill) / t.fillInterval)

	if tokensToAdd == 0 {
		return false
	}

	// update the time of the last refill depending on how many tokens we added
	t.lastRefill = t.lastRefill.Add(time.Duration(tokensToAdd) * t.fillInterval)
```

**Остаток не теряется**, потому что `lastRefill` двигается ровно на
`tokensToAdd * fillInterval`, а не на «сейчас». Это канонический способ сделать
token bucket на целых числах без дрейфа: квантовать по `fillInterval` и хранить
остаток во времени. Единственная потеря точности — однократное округление
`int64(1e9 / r)` при конструировании.

**2. Ленивое пополнение — быстрый путь вообще не читает часы:**

```go
func (t *TokenBucket) canConsume() bool {
	if t.tokens > 0 {
		// delay the refill until the bucket is empty
		t.tokens--
		return true
	}
	...
```

Пополнение откладывается до момента, когда ведро опустело. На непустом ведре
`Allow()` — это `mutex.Lock`, декремент и `Unlock`, **без `time.Now()`**. Для нашего
бенчмарка это прямо релевантно: `time.Now()` на Linux — это vDSO-вызов, и в
`BenchmarkTokenBucket_Parallel` он вполне может быть заметной долей стоимости.
Побочный эффект — ёмкость ведра фактически чуть меньше номинала (пока копится
`Since`, токены не начисляются), это осознанный размен.

### Cloudflare

Открытого кода edge-лимитера нет; есть инженерный блог. В посте
[«Counting things, a lot of different things»](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/)
описан **приближённый sliding window по двум счётчикам** — та же схема, что у нас, у
HAProxy, у APISIX и у Tyk, с примером расчёта:

> «rate = 42 * ((60-15)/60) + 18 = 42 * 0.75 + 18 = 49.5 requests»

Там же приводятся измерения на 400 млн запросов: «0.003% of requests have been
wrongly allowed or rate limited» и «An average difference of 6% between real rate and
the approximate rate». Мотивация отказа от точного sliding log — «huge processing and
memory requirements», против «two numbers per counter» у приближённого варианта.

**Это самая ценная внешняя цифра во всём отчёте:** она количественно оправдывает
именно тот алгоритм, который у нас реализован, и даёт готовый ориентир для
формулировки в README — 0.003% ошибочных решений при 6% средней погрешности оценки
темпа. По коду проверить нельзя, источник — блог вендора.

## Целочисленная арифметика против float64 — что выбирает индустрия

Гипотеза из брифа была: «если целочисленная преобладает — это довод против нашего
`admitEpsilon`-подхода». **Гипотеза не подтвердилась.** Раскол примерно пополам, и он
проходит не по «старое/новое» и не по «быстрое/медленное», а **по алгоритму**.

| Продукт | Тип состояния | Где смотреть |
|---|---|---|
| nginx `limit_req` | **целое**, масштаб ×1000 | `ngx_http_limit_req_module.c:26, 454, 939` |
| nginx `limit_conn` | **целое** (`u_short conn`) | `ngx_http_limit_conn_module.c:21` |
| HAProxy `freq_ctr` | **целое**, домножение на `period` | `freq_ctr-t.h`; `freq_ctr.c:102` |
| Envoy `TokenBucketImpl` | **`double`** | `token_bucket_impl.h:31-33` |
| Envoy `AtomicTokenBucketImpl` | **`std::atomic<double>`** | `token_bucket_impl.h:130` |
| `x/time/rate` (→ Traefik) | **`float64`** | `rate/rate.go:61` |
| Traefik Redis-скрипт | **Lua `number` = double** | `lua.go`, `AllowTokenBucketRaw` |
| `resty.limit.req` | гибрид: счёт в double, хранение в `unsigned long` | `req.lua:27, 95, 109` |
| `resty.limit.count` / `.conn` | **целое** (`dict:incr`) | `count.lua:57`, `conn.lua:53` |
| Kong `rate-limiting` | **целое** (`shm:incr`) | `policies/init.lua:308` |
| APISIX sliding window | **double** | `store/shared-dict.lua:68` |
| Caddy ring buffer | **арифметики нет**, сравнение `time.Time` | `ringbuffer.go:70` |
| KrakenD token bucket | **целое** (`uint64` + `time.Duration`) | `tokenbucket.go`, `canConsume` |
| Tyk sliding window | **float64** (+ параметр `epsilon`) | `sliding_window.go:41-42` |

Счёт: 6 целочисленных, 6 с плавающей точкой, 1 гибрид, 1 без арифметики. Ничего не
«преобладает».

### Настоящая закономерность: целочисленность даётся выбором единицы, а не типом

Все целочисленные реализации получили целочисленность одинаково — **сменой единицы
измерения так, чтобы дробей не возникало в принципе**:

* nginx и `resty.limit.req` меряют `excess` в **тысячных запроса** — тогда «утечка за
  `ms` миллисекунд» это `rate * ms / 1000`, целое деление;
* HAProxy держит **числитель дроби**, домножая всё на `period` и деля один раз в
  самом конце (`return past * remain + (curr + pend) * period;`, затем
  `div64_32(total, period)`);
* KrakenD меряет ставку в **наносекундах на токен**, и остаток естественно живёт в
  `lastRefill`, который двигается на целое число интервалов;
* Kong и `resty.limit.count` не считают время вообще — окно задаётся ключом и TTL.

То есть **целочисленность — не самоцель и не «оптимизация», а следствие того, что
подобрана единица, в которой состояние по природе дискретно**. Там, где такой единицы
нет (непрерывное пополнение с произвольной дробной ставкой), все берут `double`.

### Что это значит для `admitEpsilon`

Три вывода, и они не в одну сторону.

1. **`admitEpsilon` — не индустриальная практика.** Ни `x/time/rate`, ни Envoy (обе
   реализации), ни APISIX, ни Traefik не делают эпсилон-поправки при сравнении. Они
   пишут `if (tokens_ < tokens) return 0;` и `if total-capacity >= 0` — прямое
   сравнение `double`. Единственная найденная реализация с эпсилоном — Tyk, и там он
   означает другое (умышленное послабление лимита) **и передаётся нулём**. Наше
   решение — редкое; называть его общепринятым нельзя.
2. **Но и «float64 — ошибка» не следует.** Половина продовых реализаций, включая
   самую нагруженную (`x/time/rate` в бесчисленных Go-сервисах) и самую
   производительную (Envoy), живёт на `double` без всякой поправки. Значит проблема,
   которую лечит `admitEpsilon`, на практике не считается блокирующей.
3. **Есть третий путь, которого мы не рассматривали: убрать дрейф конструктивно.**
   Три независимых приёма из разобранного кода:
   * **Единое поле «время» вместо пары (токены, время)** — Envoy
     `AtomicTokenBucketImpl`. Токены не хранятся, ошибка накапливаться не может, потому
     что накапливать нечего.
   * **Двигать `lastRefill` на целое число интервалов** — KrakenD. Остаток остаётся во
     времени, а не в дробной части счётчика.
   * **Квантовать состояние при персисте** — `resty.limit.req` (усечение в
     `unsigned long`). Считаем во float, храним в int.

   Любой из трёх делает `admitEpsilon` ненужным. Это важнее, чем спор «int против
   float»: **дрейф — свойство конкретной схемы хранения, а не типа данных.** Наш
   собственный `lockfree_tokenbucket.go` это уже подтверждает — там дрейф структурно
   не копится (отказ без CAS), и это зафиксировано в `CLAUDE.md`.

## Какой алгоритм побеждает в проде

Счёт по разобранным продуктам (считая по тому, что реализовано, а не по числу
конфигураций):

**Token bucket — 4**: Envoy (local rate limit), Traefik (через `x/time/rate`),
KrakenD, Tyk (один из четырёх режимов). Плюс `x/time/rate` как де-факто стандарт
Go-экосистемы.

**Leaky bucket (as a meter) — 3**: nginx `limit_req`, `lua-resty-limit-traffic`
`limit.req`, APISIX `limit-req` (делегирует в предыдущий). Фактически это **одна
реализация и два её порта** — nginx'овский `excess` разошёлся по экосистеме
OpenResty целиком.

**Приближённый sliding window (два счётчика со взвешиванием) — 4**: HAProxy
`freq_ctr`, APISIX `limit-count` (режим sliding), Tyk, Cloudflare. Формула у всех
четырёх **одна и та же** и совпадает с нашей.

**Fixed window — 3**: Kong `rate-limiting` (календарные окна),
`resty.limit.count`, Tyk (один из режимов).

**Точный sliding log — 2**: Caddy (кольцевой буфер в памяти), Tyk `sliding_log`
(Redis ZSET).

**Счётчик соединений (не rate limiting)**: nginx `limit_conn`, `resty.limit.conn`,
APISIX `limit-conn` — отдельная задача, к нашему порту неприменима.

### Какие причины называют

* **За приближённый sliding window** — прямо и количественно, Cloudflare: точный лог
  требует «huge processing and memory requirements», приближённый обходится «two
  numbers per counter», ценой 0.003% неверных решений. Это самый убедительный
  аргумент из всех найденных, и он на стороне нашего `slidingwindow.go`.
* **За token bucket** — не аргументом, а поведением: он единственный, кто нативно
  отвечает на вопрос «через сколько можно?». `Reserve()`/`Delay()` в
  `x/time/rate` (и его использование в Traefik), `nextTokenAvailable()` в Envoy — из
  состояния «токенов не хватает на `n`» время ожидания вычисляется точно и дёшево.
  У sliding window это делается заметно грязнее (см. ветвление в Tyk
  `sliding_window.go:43-49` с отдельным случаем `prev == 0`).
* **За leaky bucket** — сглаживание. Он единственный, кто по построению даёт
  *равномерный* выход; nginx поэтому и делает `delay` режимом по умолчанию, а
  `nodelay` — опцией. Формулировка в документации nginx: «Excessive requests are
  delayed until their number exceeds the maximum burst size in which case the request
  is terminated with an error».
* **За fixed window** — не производительность, а **выразимость в чужом хранилище**.
  Kong и `resty.limit.count` выбрали его потому, что весь алгоритм сводится к
  «атомарный `incr` с TTL», а это единственный примитив, который одинаково есть и в
  `lua_shared_dict`, и в Redis, и в SQL. Алгоритм подстроен под хранилище, а не под
  точность.
* **За точный sliding log** — предсказуемость и честность; README Caddy
  противопоставляет себя nginx именно на этом: «Caddy does not drop rate limiters on
  the floor and forget events like nginx does».

**Сводный вывод: победителя нет, есть устойчивое соответствие «задача → алгоритм».**
Нужно сгладить трафик — leaky bucket. Нужно разрешить всплеск и сказать, сколько
ждать — token bucket. Нужно ограничить именно частоту, дёшево и почти точно —
приближённый sliding window. Нужно уложиться в чужое key-value хранилище — fixed
window. Наш набор из четырёх реализаций накрывает три из четырёх ниш; не хватает
fixed window, и он же — самый простой.

## Модели хранения состояния

Разобранные продукты дают четыре разные модели, и **три из них нам недоступны в
рамках MVP** (нет сети, нет отдельных процессов):

**1. Разделяемая память между процессами — nginx, OpenResty, Kong (`local`), APISIX.**
nginx воркеры — отдельные процессы, поэтому состояние лежит в shm-зоне
(`ngx_shared_memory_add`), а согласованность обеспечивает межпроцессный мьютекс
`ctx->shpool->mutex`. Следствия, которых у нас нет:
* **память жёстко ограничена** размером зоны, поэтому нужен LRU и вытеснение
  (`ngx_http_limit_req_expire`);
* **состояние не может содержать указателей** — только plain-old-data по смещениям,
  отсюда FFI-структура фиксированного размера в `resty.limit.req` и комментарий
  «TODO: we could avoid the tricky FFI cdata when lua_shared_dict supports
  hash-typed values as in redis» (`req.lua:23-24`);
* **атомарность ограничена одной операцией словаря**, отсюда честный FIXME про гонку
  между `dict:get()` и `dict:set()` (`req.lua:70-72`).

**2. Общая структура между потоками одного процесса — HAProxy, Envoy.** Это **наша
модель.** Различаются реализацией:
* HAProxy — иерархия: шардированные rwlock'и на вёдра таблицы
  (`buckets[CONFIG_HAP_TBL_BUCKETS]`, каждое со своим `sh_lock`), rwlock на запись,
  атомарные операции внутри `freq_ctr`, и seqlock-подобное чтение без блокировки.
  **Шардирование замка по ключу — приём, которого у нас нет**, и он ортогонален
  выбору mutex/CAS: он снижает конкуренцию за счёт того, что разные ключи не
  сталкиваются.
* Envoy — одно потокобезопасное ведро на процесс, разделяемое всеми worker-потоками,
  вся синхронизация внутри `AtomicTokenBucketImpl` на одном `std::atomic<double>`.
  Альтернатива задаётся конфигурацией: `local_rate_limit_per_downstream_connection`
  переключает на ведро на соединение — то есть **отказ от разделяемого состояния как
  режим работы**, а не как ограничение.

**3. Внешнее хранилище — Kong (`redis`/`cluster`), Traefik (Redis), Tyk, APISIX
(redis).** Общий приём во всех: **алгоритм переписывается на Lua и исполняется
внутри Redis**, чтобы получить атомарность (Traefik `AllowTokenBucketRaw`, Kong
`sync_to_redis`). Второй общий приём — **буферизация локальной дельты с
периодическим сливом**: Kong накапливает в `cur_usage[db_key]` и сливает фоновым
`sync_to_redis`, обменивая точность на число round-trip'ов, с параметром
`sync_rate`.

**4. Аппроксимация глобального лимита без общего состояния — Envoy `ShareProvider`,
Caddy `distributed`.** Envoy делит бюджет на число соседей
(`share_factor = 1.0 / num`) и списывает `to_consume / factor` токенов за запрос.
Caddy периодически пишет своё состояние в общее хранилище и читает чужое; README
называет это «inherently approximate, but also eventually consistent». Ни то, ни
другое не требует координации на горячем пути.

**Что из этого переносимо к нам без нарушения границ MVP:** только модель 2, и в ней
конкретно — **шардирование состояния по ключу** (HAProxy) и **сведение состояния к
одному атомарному слову** (Envoy). Обе техники не требуют ни сети, ни зависимостей.

## Сводная таблица

| Продукт | Алгоритм | Арифметика | Состояние на ключ | Синхронизация | Эпсилон |
|---|---|---|---|---|---|
| nginx `limit_req` | leaky bucket (meter) | целое ×1000 | `excess` + `last` | межпроцессный mutex на shm-зону | нет |
| nginx `limit_conn` | счётчик соединений | целое | `u_short conn` | тот же mutex | нет |
| HAProxy `freq_ctr` | приближённый sliding window | целое (×`period`) | 3×`uint` | atomic + lock-бит в `curr_tick`; seqlock на чтение | нет |
| Envoy `TokenBucketImpl` | token bucket | `double` | токены + `last_fill` | внешняя (класс не потокобезопасен) | нет |
| Envoy `AtomicTokenBucketImpl` | token bucket (виртуальное время) | `double` | **одно** `atomic<double>` | CAS `relaxed`, отказ без CAS | нет |
| `x/time/rate` → Traefik | token bucket | `float64` | токены + 2×`time.Time` | `sync.Mutex` | нет |
| `resty.limit.req` → APISIX | leaky bucket (meter) | double→усечение в int | `excess` + `last` | нет (задокументированная гонка) | нет |
| `resty.limit.count` | fixed window | целое | счётчик + TTL | атомарный `incr` словаря | нет |
| Kong `rate-limiting` | fixed window по календарю | целое | счётчик на период + TTL | атомарный `incr` (shm/redis/db) | нет |
| APISIX `limit-count` sliding | приближённый sliding window | double | 2 счётчика + TTL | атомарный `incr`, не атомарный check | нет |
| Caddy `caddy-ratelimit` | точный sliding log | нет (сравнение времён) | `[]time.Time` длины `maxEvents` | `sync.Mutex` | нет |
| KrakenD | token bucket, ленивый refill | целое (`uint64`/`Duration`) | токены + `lastRefill` | `sync.Mutex` | нет |
| Tyk | 4 на выбор + sliding log | `float64` | зависит от режима | Redis / локально | **есть, но = 0** |
| Cloudflare | приближённый sliding window | **не проверено** | 2 счётчика | **не проверено** | **не проверено** |
| **ratelimit-lab** | 4: TB / SW / LB / lock-free TB | `float64` | зависит | `sync.Mutex` + `atomic.Pointer` | **`admitEpsilon = 1e-9`** |

## Применимость к ratelimit-lab

Отобрано по критерию «переносимо в учебную Go-библиотеку **без сети и без внешних
зависимостей**». Порядок — по убыванию ценности; это находки, а не решения — решение
за пользователем.

### 1. Lock-free token bucket на одном `atomic.Uint64` вместо `atomic.Pointer`

Прямой перенос `AtomicTokenBucketImpl` из Envoy. Состояние сводится к одному
`float64` — «время, до которого ведро исчерпано»; токены выводятся как
`min(capacity, (now - t) * rate)`, потребление — сдвиг `t` вперёд. В Go:
`atomic.Uint64` + `math.Float64bits`/`math.Float64frombits`.

Что это даёт по сравнению с текущей реализацией:
* **исчезает аллокация на успешную запись** — сейчас каждый успешный CAS создаёт
  новый снапшот, то есть нагружает GC пропорционально throughput;
* `allocs/op` в `BenchmarkLockFreeTokenBucket_Parallel` должен упасть до нуля, а это
  **прямо тот результат, ради которого на Этапе 5 вводился `-benchmem`**;
* `admitEpsilon` в этой реализации становится окончательно не нужен.

Ценность для учебной цели высокая: две lock-free схемы на одну задачу (снапшот через
указатель против упаковки в слово) — хороший материал для сравнения, и обе можно
померить существующим харнессом. Замечание: сравнение честно только если старую
реализацию оставить рядом, а не заменить.

### 2. Ленивое пополнение — не читать часы на непустом ведре

Приём KrakenD (`canConsume`: `if t.tokens > 0 { t.tokens--; return true }`) и охрана
Envoy (`if (tokens_ < max_tokens_)`). У нас `refill` вызывается безусловно, значит
`Clock.Now()` дёргается на каждом `Allow()`. `time.Now()` — vDSO-вызов, десятки
наносекунд; на фоне `mutex.Lock`+декремент это может быть заметная доля.
**Это гипотеза, не измерение** — проверяется одним прогоном
`go test -bench=. -benchmem ./internal/limiter/`. Если подтвердится, это самая
дешёвая оптимизация из списка. Побочный эффект (ведро наполняется чуть медленнее
номинала) нужно либо принять явно, либо отвергнуть приём.

### 3. Зажим `prevCount` по лимиту в sliding window

Из APISIX (`if last > limit then last = limit end`) и косвенно из HAProxy
(отсечка «два периода назад»). У нас `prevCount` в оценку идёт как есть; если он
превысил лимит, завышенная оценка держится всё следующее окно. Правка на две строки,
но меняет поведение — то есть по нашему пайплайну это локальный режим с тестом на
граничный случай.

Рядом — вторая находка оттуда же: **отдельная проверка `currCount >= limit`** помимо
взвешенной оценки.

### 4. Развести «оценку для решения» и «оценку для отчёта»

Явно сформулировано в двух независимых местах: HAProxy (`freq_ctr.h:82-88` — flapping
correction нужна для чтения, но не для проверки лимита) и APISIX
(`sliding-window.lua:210-213` — репорт по `limit - new_count` «degrading the sliding
window to a fixed one»). У нас такого разделения нет, потому что нет и метода
«сколько осталось». Если он когда-нибудь появится, эта грабля уже описана.

### 5. Шардирование по ключу как отдельный уровень

HAProxy `buckets[CONFIG_HAP_TBL_BUCKETS]`. Сейчас у нас лимитер — один объект без
ключей, и вопрос не стоит. Но если появится «лимитер на ключ» (а именно так устроены
все разобранные продукты без исключения), шардирование мьютексов — стандартный ответ,
и он ортогонален выбору mutex/CAS: интересно померить «шардированный mutex против
одного lock-free» на многих ключах. Это кандидат в отдельный Этап, не в правку.

### 6. Fixed window как пятый алгоритм

Самый простой из всех (Kong, `resty.limit.count`), у нас его нет. Учебная ценность —
показать на бенчмарке, чем платят за его дешевизну: всплеск до `2×limit` на границе
окон. Хорошо ложится в существующий харнесс и в fake-clock тесты. Календарное
выравнивание Kong (`get_timestamps`) для нас избыточно, достаточно нарезки по эпохе,
как в APISIX (`math_floor(time / window_size)`).

### 7. Sliding log на кольцевом буфере как шестой алгоритм

Caddy `ringbuffer.go` — ~50 строк на горячий путь, точный результат, O(maxEvents)
памяти. Ценность в том, что он даёт **эталон** для проверки точности приближённого
sliding window: можно прогнать оба на одном потоке событий и измерить расхождение —
получив собственную версию цифры Cloudflare «0.003%». Это уже не бенчмарк
производительности, а бенчмарк **корректности**, которого в проекте нет.

### 8. Цифра Cloudflare для README

«0.003% неверных решений, 6% средней погрешности оценки темпа на 400 млн запросов» —
готовое внешнее обоснование того, почему приближённый sliding window вообще
применяют. Ссылка на источник обязательна.

### Что переносить не стоит

* **`nodelay` через `NGX_MAX_INT_T_VALUE / 1000`** — остроумно, но это трюк
  конфигурации nginx, у нас нет режима задержки.
* **Инжекция часов пакетной переменной** (Caddy `var now = time.Now`) — дешевле
  интерфейса, но ломает параллельные тесты. Наш `Clock` лучше; зафиксировать это как
  осознанный выбор, а не переоткрывать.
* **Масштаб ×1000 в целых** (nginx) — работает, но привязывает точность к жёстко
  зашитой константе (миллисекунды и тысячные запроса). Если уходить от `float64`, то
  приёмом KrakenD (наносекунды на токен), а не nginx'овским.
* **Двухфазность `commit`/`uncommit`** (OpenResty) — красиво решает комбинирование
  лимитеров, но это изменение порта `Limiter`, то есть смена контракта. Отмечено как
  возможность, не как рекомендация.

## Что рассмотрено и отброшено

**Рассмотрено по исходному коду** (все ссылки на файлы и строки в разделах выше):
nginx `ngx_http_limit_req_module.c` и `ngx_http_limit_conn_module.c`; HAProxy
`src/freq_ctr.c`, `include/haproxy/freq_ctr.h`, `freq_ctr-t.h`, `src/stick_table.c`,
`include/haproxy/stick_table-t.h`; Envoy `token_bucket_impl.{h,cc}` и
`local_ratelimit_impl.{h,cc}`; `lua-resty-limit-traffic` — все четыре файла
библиотеки целиком; Kong `rate-limiting/policies/init.lua` и `kong/tools/timestamp.lua`;
Traefik `rate_limiter.go`, `in_memory_limiter.go`, `lua.go` плюс `golang.org/x/time/rate`;
APISIX `limit-req.lua`, `limit-count.lua`, `limit-conn.lua`, `limit-count-local.lua`,
`sliding-window/sliding-window.lua`, `sliding-window/store/shared-dict.lua`;
Caddy `ringbuffer.go`, `handler.go`; Tyk `internal/rate/sliding_log.go`,
`internal/rate/limiter/*.go`, `TykTechnologies/exp/pkg/limiters/sliding_window.go`;
KrakenD `krakend-ratelimit/tokenbucket.go`.

**Рассмотрено по официальной документации** (кода не читал): Envoy local rate limit
filter — вопрос «одно ведро на процесс или на соединение»; nginx `limit_req` —
формулировка «leaky bucket method».

**Отброшено сознательно:**

* **Envoy global rate limit (gRPC RLS) и эталонный сервер `envoyproxy/ratelimit`** —
  распределённый лимитер с Redis/Memcached. Вне границ MVP (без сети). Разобрана
  только точка интеграции, алгоритм сервера не проверял.
* **Redis-стратегии Kong (`cluster`, `redis`), Traefik, APISIX, Tyk** — разобраны
  ровно настолько, чтобы показать *приёмы* (Lua-скрипт ради атомарности, буферизация
  дельты). Сами скрипты построчно не читал: без сети они к нам не применимы.
* **Caddy `distributed.go`** — то же основание.
* **Kong `rate-limiting-advanced`** — **исходников нет в открытом доступе**
  (Kong Enterprise). Единственное, что можно сказать — по документации. Явно помечено
  в разделе про Kong.
* **Cloudflare** — открытого кода edge-лимитера нет; взяты только опубликованные
  вендором формула и измерения. Помечено как непроверяемое по коду.
* **AWS ALB / GCP Cloud Armor / Azure Front Door и прочие управляемые сервисы** — не
  рассматривались вовсе: закрытые реализации, проверить нечего.
* **Istio** — не рассматривался отдельно: rate limiting там делается тем же
  Envoy-фильтром, что уже разобран; своего алгоритма нет.
* **`ngx_http_limit_conn_module`, `resty.limit.conn`, APISIX `limit-conn`** —
  разобраны, но помечены как **не rate limiting**: это ограничение одновременности,
  требующее парного release; к неблокирующему порту `Limiter` неприменимо.
* **APISIX `limit-req/limit-req-redis.lua`** — не читал построчно (Redis, вне границ).

## Источники

Исходный код (ветка по умолчанию на момент чтения — сентябрь 2026):

* nginx — https://github.com/nginx/nginx/blob/master/src/http/modules/ngx_http_limit_req_module.c
* nginx — https://github.com/nginx/nginx/blob/master/src/http/modules/ngx_http_limit_conn_module.c
* HAProxy — https://github.com/haproxy/haproxy/blob/master/src/freq_ctr.c
* HAProxy — https://github.com/haproxy/haproxy/blob/master/include/haproxy/freq_ctr.h
* HAProxy — https://github.com/haproxy/haproxy/blob/master/include/haproxy/freq_ctr-t.h
* HAProxy — https://github.com/haproxy/haproxy/blob/master/src/stick_table.c
* HAProxy — https://github.com/haproxy/haproxy/blob/master/include/haproxy/stick_table-t.h
* Envoy — https://github.com/envoyproxy/envoy/blob/main/source/common/common/token_bucket_impl.h
* Envoy — https://github.com/envoyproxy/envoy/blob/main/source/common/common/token_bucket_impl.cc
* Envoy — https://github.com/envoyproxy/envoy/blob/main/source/extensions/filters/common/local_ratelimit/local_ratelimit_impl.cc
* OpenResty — https://github.com/openresty/lua-resty-limit-traffic/blob/master/lib/resty/limit/req.lua
* OpenResty — https://github.com/openresty/lua-resty-limit-traffic/blob/master/lib/resty/limit/count.lua
* OpenResty — https://github.com/openresty/lua-resty-limit-traffic/blob/master/lib/resty/limit/conn.lua
* OpenResty — https://github.com/openresty/lua-resty-limit-traffic/blob/master/lib/resty/limit/traffic.lua
* Kong — https://github.com/Kong/kong/blob/master/kong/plugins/rate-limiting/policies/init.lua
* Kong — https://github.com/Kong/kong/blob/master/kong/tools/timestamp.lua
* Traefik — https://github.com/traefik/traefik/blob/master/pkg/middlewares/ratelimiter/rate_limiter.go
* Traefik — https://github.com/traefik/traefik/blob/master/pkg/middlewares/ratelimiter/in_memory_limiter.go
* Traefik — https://github.com/traefik/traefik/blob/master/pkg/middlewares/ratelimiter/lua.go
* Go — https://github.com/golang/time/blob/master/rate/rate.go
* APISIX — https://github.com/apache/apisix/blob/master/apisix/plugins/limit-req.lua
* APISIX — https://github.com/apache/apisix/blob/master/apisix/plugins/limit-count/limit-count-local.lua
* APISIX — https://github.com/apache/apisix/blob/master/apisix/plugins/limit-count/sliding-window/sliding-window.lua
* APISIX — https://github.com/apache/apisix/blob/master/apisix/plugins/limit-count/sliding-window/store/shared-dict.lua
* Caddy — https://github.com/mholt/caddy-ratelimit/blob/master/ringbuffer.go
* Caddy — https://github.com/mholt/caddy-ratelimit/blob/master/README.md
* Tyk — https://github.com/TykTechnologies/exp/blob/main/pkg/limiters/sliding_window.go
* Tyk — https://github.com/TykTechnologies/tyk/blob/master/internal/rate/limiter/limiter_sliding_window.go
* Tyk — https://github.com/TykTechnologies/tyk/blob/master/internal/rate/sliding_log.go
* KrakenD — https://github.com/krakend/krakend-ratelimit/blob/master/tokenbucket.go
* folly (упомянута Envoy как источник приёма) — https://github.com/facebook/folly/blob/main/folly/TokenBucket.h — **не читал**

Документация:

* nginx — https://nginx.org/en/docs/http/ngx_http_limit_req_module.html
* Envoy — https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter
* Envoy RLS proto — https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ratelimit/v3/rls.proto
* Kong — https://developer.konghq.com/plugins/rate-limiting-advanced/
* Cloudflare — https://blog.cloudflare.com/counting-things-a-lot-of-different-things/
