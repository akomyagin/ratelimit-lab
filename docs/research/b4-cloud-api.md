# B4 — облака, SaaS и публичные API

> Статус: **черновик, в работе.** Файл наполняется по ходу исследования.
> Каждое утверждение сопровождается ссылкой на официальную документацию или
> первоисточник. Где проверить не удалось — стоит пометка «**не проверено**».
> Даты обращения указаны у разделов: документация облаков меняется.

Тема: какие алгоритмы и **какая наблюдаемая семантика** заявлены у облачных
gateway/WAF и у публичных API — что именно возвращается вызывающему за
пределами «пустили / не пустили».

## AWS

_Обращение: 6 сентября 2026._

### API Gateway — token bucket, явно названный

Документация называет алгоритм прямо
(<https://docs.aws.amazon.com/apigateway/latest/developerguide/api-gateway-request-throttling.html>):

> API Gateway throttles requests to your API using the token bucket algorithm,
> where a token counts for a request.

Отображение настроек на параметры бакета — дословно:

> You can specify a *throttling rate*, which is the rate, in requests per second,
> that tokens are added to the token bucket. You can also specify a *throttling
> burst*, which is the capacity of the token bucket.

То есть `rate` = скорость пополнения, `burst` = **ёмкость** бакета. Ровно наша
пара `rate`/`capacity` в `tokenbucket.go`.

Два замечания из той же страницы, важных для семантики контракта:

> Both throttles and quotas are applied on a best-effort basis and should be
> thought of as targets rather than guaranteed request ceilings.

> In the token bucket algorithm, a burst can allow pre-defined overrun of those
> limits, but other factors can also cause limits to be overrun in some cases.

При отказе клиент получает `429 Too Many Requests`. Порядок применения лимитов
(**четыре уровня подряд**, все должны пропустить):

1. per-client / per-method лимиты usage plan;
2. per-method лимиты стадии;
3. account-level throttling per Region;
4. AWS Regional throttling.

Числа (<https://docs.aws.amazon.com/apigateway/latest/developerguide/limits.html>):
дефолтная account-level квота — **10 000 RPS** на аккаунт на регион по всем
HTTP/REST/WebSocket API, «with an additional burst capacity provided by the token
bucket algorithm, using a maximum bucket capacity of **5 000 requests**». Для
списка «малых» регионов (Cape Town, Milan, Jakarta, UAE, Hyderabad, Melbourne,
Spain, Zurich, Tel Aviv, Calgary, Malaysia, Thailand, Mexico Central) — 2500 RPS
и burst 1250. RPS-квота увеличиваема, **burst — нет**: «It is not a quota that a
customer can control or request changes to».

Usage plan — **вторая, независимая ось**: квота на длинном окне поверх token
bucket. `QuotaSettings`
(<https://docs.aws.amazon.com/apigateway/latest/api/API_QuotaSettings.html>):

- `limit` — «The target maximum number of requests that can be made in a given
  time period»;
- `period` — «Valid Values: `DAY | WEEK | MONTH`»;
- `offset` — «The number of requests subtracted from the given limit in the
  initial time period» — сдвиг для выравнивания первого (неполного) периода.

### EC2 API — token bucket в двух измерениях

<https://docs.aws.amazon.com/ec2/latest/devguide/ec2-api-throttling.html>

> Amazon EC2 uses the token bucket algorithm to implement API throttling. […] The
> number of tokens in the bucket represents your throttling limit at any given
> second.

Механика пополнения описана буквально как наш `tokenbucket.go`:

> Buckets automatically refill at a set rate. If the bucket is below its maximum
> capacity, a set number of tokens is added back to it every second until it
> reaches its maximum capacity. **If the bucket is full when refill tokens arrive,
> they are discarded.** The bucket can't hold more than its maximum number of
> tokens.

**Ключевое для нашего `AllowN`: EC2 держит ДВА независимых бакета на одно и то же
действие.**

> Amazon EC2 implements two types of API throttling: Request rate limiting […]
> Resource rate limiting.

- **Request rate limiting** — «Each request that you make removes one token from
  the API's bucket», то есть стоимость всегда 1 (это `Allow()`).
- **Resource rate limiting** — «These API actions have a separate resource token
  bucket that depletes based on **the number of resources that are impacted by the
  request**» — это ровно `AllowN(n)`, где `n` = число инстансов в запросе.

Дословный пример из документа: «the resource token bucket size for `RunInstances`
is 1000 tokens, and the refill rate is two tokens per second. Therefore, you can
immediately launch 1000 instances, using any number of API requests, such as one
request for 1000 instances or four requests for 250 instances».

Отказ по любому из двух бакетов режет запрос: «If you exceed a specific bucket
limit for an API, including when a bucket has not yet refilled to support the next
API request, the action of the API is limited even though you have not reached the
total API throttle limit». Код ошибки — `RequestLimitExceeded`.

Числа по категориям (дословно из таблицы «Request token bucket sizes and refill
rates»):

| Категория | Bucket maximum capacity | Bucket refill rate |
|---|---|---|
| Non-mutating (`Describe*`/`List*`/`Search*`/`Get*`) | 100 | 20 |
| Unfiltered and unpaginated non-mutating (`DescribeInstances`, `DescribeVolumes`, …) | 50 | 10 |
| Mutating | 50 | 5 |
| Resource-intensive | 50 | 5 |
| Console non-mutating | 100 | 10 |

Resource token buckets: `RunInstances` 1000/2, `TerminateInstances` 1000/20,
`StartInstances` 1000/2, `StopInstances` 1000/20.

**Заметное:** дробный refill rate в таблице «Uncategorized actions» —
`AdvertiseByoipCidr` 1 / **0.1**, `CreateVpcEndpoint` 4 / **0.3**,
`DescribeCapacityBlockOfferings` 10 / **0.15**. То есть AWS сознательно
использует нецелую скорость пополнения — прямой аргумент за наш `float64`-refill,
а не за «целые токены в тик».

Service Quotas выставляет ровно две ручки на действие: «`{API_NAME}` request
bucket maximum capacity — Burst rate» и «`{API_NAME}` request bucket refill rate —
Sustained rate».

### AWS WAF — rate-based rules: намеренно приблизительный счётчик

<https://docs.aws.amazon.com/waf/latest/developerguide/waf-rule-statement-type-rate-based-high-level-settings.html>

**Evaluation window** — «Valid settings are 60 (1 minute), 120 (2 minutes), 300 (5
minutes), and 600 (10 minutes), and 300 (5 minutes) is the default». Важно:
«This setting doesn't determine how often AWS WAF checks the rate, but how far
back it looks each time it checks». API-справочник добавляет частоту проверки:
«AWS WAF checks the rate about every 10 seconds»
(<https://docs.aws.amazon.com/waf/latest/APIReference/API_RateBasedStatement.html>).

**Rate limit** — `Limit`, «Valid Range: Minimum value of 10. Maximum value of
2000000000» (там же).

**Алгоритм — явно не точный счётчик окна.** Дословно из caveats
(<https://docs.aws.amazon.com/waf/latest/developerguide/waf-rule-statement-type-rate-based-caveats.html>):

> AWS WAF rate limiting is designed to control high request rates and protect your
> application's availability in the most efficient and effective way possible.
> **It's not intended for precise request-rate limiting.**
>
> AWS WAF estimates the current request rate using **an algorithm that gives more
> importance to more recent requests**. Because of this, AWS WAF will apply rate
> limiting near the limit that you set, but does not guarantee an exact limit
> match.
>
> […] it's possible for requests to be coming in at too high a rate for **up to
> several minutes** before AWS WAF detects and rate limits them. Similarly. the
> request rate can be below the limit for a period of time before AWS WAF detects
> the decrease and discontinues the rate limiting action. Usually, this delay is
> below 30 seconds.
>
> If you change any of the rate limit settings in a rule that's in use, the change
> **resets the rule's rate limiting counts**. This can pause the rule's rate
> limiting activities for up to a minute.

«Gives more importance to more recent requests» — это экспоненциально взвешенная
оценка, а не наше точное скользящее окно. Конкретная формула AWS не публикует —
**не проверено**, какая именно.

**Гранулярность (aggregation keys).** `AggregateKeyType`: «Valid Values: `IP |
FORWARDED_IP | CUSTOM_KEYS | CONSTANT`». `CustomKeys` — «Array Members: Maximum
number of **5** items», то есть до 5-tuple ключа: «For any n-tuple of aggregation
keys, each unique combination of values for the keys defines a separate
aggregation instance, which AWS WAF counts and rate-limits individually». Лимит
применяется к каждому инстансу агрегации отдельно. `CONSTANT` — один общий
счётчик на весь scope-down statement.

Интроспекция: «you can retrieve the list of IP addresses that AWS WAF is currently
rate limiting for a rule through the API call `GetRateBasedStatementManagedKeys`»
— только для агрегации по IP/forwarded IP.

## Google Cloud (Apigee, Cloud Armor)

_Обращение: 6 сентября 2026._

### Apigee SpikeArrest — «сглаживание», разобранное точно

<https://docs.apigee.com/api-platform/reference/policies/spike-arrest-policy>

Документ сам предупреждает, что поведение не то, которого ждут от «N в минуту»:

> The runtime Spike Arrest behavior differs from what you might expect to see from
> the literal per-minute or per-second values you enter.

Механика дословно:

> To prevent spike-like behavior, Spike Arrest smooths the number of full requests
> allowed by dividing your settings into smaller intervals:
>
> - Per-minute rates get smoothed into full requests allowed in intervals **of
>   seconds**. For example, 30pm gets smoothed like this: 60 seconds (1 minute) /
>   30pm = 2-second intervals, or 1 request allowed every 2 seconds. **A second
>   request inside of 2 seconds will fail. Also, a 31st request within a minute
>   will fail.**
> - Per-second rates get smoothed into full requests allowed in intervals **of
>   milliseconds**. For example, 10ps gets smoothed like this: 1000 milliseconds (1
>   second) / 10ps = 100-millisecond intervals, or 1 request allowed every 100
>   milliseconds. **A second request inside of 100ms will fail. Also, an 11th
>   request within a second will fail.**

Плюс дискретность:

> In rate smoothing, the number of requests is always a whole number greater than
> zero. Smoothing never involves calculating fractions of requests.

**Как это читать.** Это **token bucket с ёмкостью 1** (плюс отдельный
`maxBurstMessageCount`), а не «N запросов в окне». Документ прямо так и называет
алгоритм: «The SpikeArrest policy uses a "token bucket" algorithm that smooths
traffic spikes by dividing the rate limit that you specify into smaller
intervals. **A drawback of this approach is that multiple legitimate requests
coming in over a short time interval can potentially be denied.**»

Внутренние атрибуты, которые документ перечисляет явно:
`messagesPerPeriod` («if a policy is configured for '10ps' […] would be 10»),
`periodInMicroseconds` («For a '10ps' configuration, this value would be
1,000,000»), `maxBurstMessageCount` («the maximum number of requests that can be
allowed instantly or in a short burst at the beginning of a new interval»).

**Масштабирование на несколько Message Processors** — контринтуитивно:

> By default, Spike Arrest is not distributed unless you enable
> `<UseEffectiveCount>`. That means request counts are not synchronized across
> MPs. […] With one message processor, a 30pm rate smooths traffic to 1 request
> every 2 seconds (60 / 30). With two message processors (the default for Edge
> cloud), that number doubles to 2 requests every 2 seconds.

- `UseEffectiveCount=false` — «Each MP's spike rate limit is simply the value of
  its `<Rate>`. The aggregate limit is the sum of the rates of all the MPs».
- `UseEffectiveCount=true` — «An MP's spike rate limit is the [`<Rate>`] divided
  by the current number of MPs in the same pod. The aggregate limit is the value
  of `<Rate>`».

То есть настроенное число значит либо «на узел», либо «на кластер» — в
зависимости от одного булева флага. Для нашей single-process библиотеки это
прямая аналогия: лимит на экземпляр против лимита на процесс.

**Стоимость запроса — есть.** `<MessageWeight ref="flow_variable"/>`: «accepts a
custom value (the weight header) that adjusts message weights for specific apps or
clients». Невалидный (нецелый) вес — ошибка `policies.ratelimit.InvalidMessageWeight`.

**Отказ:** `policies.ratelimit.SpikeArrestViolation`, HTTP **429** («For Private
Cloud, the default HTTP status code returned is **500**» — это документированное
расхождение между вариантами продукта).

### Apigee Quota — вторая ось, с полным набором счётчиков наружу

<https://docs.apigee.com/api-platform/reference/policies/quota-policy>

Разделение ролей задано в документации прямо: «Use Quota to enforce business
contracts or SLAs with developers and partners, rather than for operational
traffic management. Use Spike Arrest to protect against sudden spikes in API
traffic».

Четыре типа сброса счётчика:

| Тип | Когда счётчик сбрасывается |
|---|---|
| default | по календарной границе: «Start of next minute», «Top of next hour», «Midnight GMT of the current day», «Midnight GMT Sunday at the end of the week», «Midnight GMT of the last day of the month» |
| `calendar` | «One hour after `<StartTime>`» и т.д. — от явно заданного начала |
| `flexi` | «One hour after first request» — окно стартует с первого запроса приложения |
| `rollingwindow` | не сбрасывается вовсе: «the counter never resets, but is recalculated on each request» |

`rollingwindow` описан дословно: «you define a two hour window that allows 1000
requests. A new request comes in at 4:45 PM. The policy calculates the quota count
for the past two hour window, meaning the number of requests since 2:45 PM. […]
One minute later, at 4:46 PM, another request comes in. Now the policy calculates
the quota count since 2:46 PM». Это точное скользящее окно — прямой аналог нашего
`slidingwindow.go`.

Замечание про гранулярность: «The policy also supports `second` as the time unit.
However, `second` is only supported for non-distributed counters, as defined by
`Distributed=false`. Instead of using `second`, Apigee recommends that you use the
SpikeArrest policy». То есть точный счётчик на секундной шкале в распределённом
режиме признан нецелесообразным.

**`<MessageWeight>` — стоимость запроса, и она документирована через пример:**

> For example, you want to count POST messages as being twice as "heavy" or
> expensive, as GET messages. Therefore, you set the MessageWeight to 2 for a POST
> and 1 for a GET. **You can even set the MessageWeight to 0 so the request does
> not affect the counter.**

Вес 0 — интересная деталь: «пропустить, не списывая». У нас `AllowN(0)` семантику
имеет тривиальную, но осмысленную.

**Наблюдаемый контракт Quota — самый богатый из встреченных в gateway.** Flow-
переменные (все Read-Only), которые политика выставляет и которые прокси может
отдать заголовками:

| Переменная | Смысл (дословно) |
|---|---|
| `ratelimit.{policy}.allowed.count` | «Returns the allowed quota count» |
| `ratelimit.{policy}.used.count` | «Returns the current quota used within a quota interval» |
| `ratelimit.{policy}.available.count` | «Returns the available quota count in the quota interval» |
| `ratelimit.{policy}.exceed.count` | «Returns 1 after the quota is exceeded» |
| `ratelimit.{policy}.total.exceed.count` | «Returns 1 after the quota is exceeded» |
| `ratelimit.{policy}.expiry.time` | «Returns the UTC time in milliseconds which determines when the quota expires […] When the Quota policy type is `rollingwindow`, this value is not valid because the quota interval never expires» |
| `ratelimit.{policy}.identifier` | «Returns the (client) identifier reference attached to the policy» |
| `ratelimit.{policy}.class` | «Returns the class associated with the client identifier» |
| `ratelimit.{policy}.class.{allowed,used,available,exceed,total.exceed}.count` | те же счётчики в разрезе класса клиента |
| `ratelimit.{policy}.failed` | «Indicates whether or not the policy failed (true or false)» |

Обратить внимание: **`expiry.time` объявлен невалидным для `rollingwindow`.** Это
и есть цена «точного скользящего окна»: у него нет момента сброса, поэтому поле
`reset` из контракта выпадает. Наш `slidingwindow.go` имеет ровно эту же
структурную особенность.

Документ показывает и канонический способ вернуть это клиенту — политикой
`AssignMessage`: `<Header name="QuotaLimit">{ratelimit.QuotaPolicy.allowed.count}</Header>`,
`<Header name="QuotaResetUTC">{ratelimit.QuotaPolicy.expiry.time}</Header>`.
Мотивация названа явно: «as it gets close to the quota limit, return the current
quota counter to an app» — то самое «near limit» предупреждение.

**Отказ:** `policies.ratelimit.QuotaViolation`, документированный статус — **500**
(в отличие от 429 у SpikeArrest).

**Согласованность счётчика — явный размен, вынесенный в конфиг.** `<Synchronous>`:

> Set to `true` to update a distributed quota counter synchronously. […] Set to
> `true` if it is essential that you not allow any API calls over the quota. […]
> there is the potential for performance impacts and lower throughputs.
>
> Set to `false` to update the quota counter asynchronously. This means that it is
> possible that **some API calls exceeding the quota will go through**, depending
> on when the quota counter in the central repository is asynchronously updated.
> […] The default asynchronous update interval is 10 seconds.

Дефолт — `false`, то есть **по умолчанию квота переливается**. Точность лимита
здесь — настраиваемый параметр, а не инвариант.

### Cloud Armor — rate limiting как средство защиты, а не квотирования

<https://cloud.google.com/armor/docs/rate-limiting-overview> (обращение 06.09.2026)

Два типа правил: `throttle` («throttling limits the rate of requests to a defined
maximum. Throttling allows some traffic to pass through, but at a controlled
rate») и `rate_based_ban` («blocks all further requests from the source […] for a
specified period»).

Параметры `throttle`:

- `rate_limit_threshold_count` — «The minimum value is 1 and the maximum value is
  1,000,000»;
- `interval_sec` — «The value must be **10, 30, 60, 120, 180, 240, 300, 600, 900,
  1200, 1800, 2700, or 3600 seconds**» (фиксированный набор, не произвольное
  число);
- `exceed_action` — `deny(status)` со статусом из «403 Forbidden, 404 Page Not
  Found, 429 Too Many Requests, and 502 Bad Gateway. **We recommend using the 429
  Too Many Requests status code**», либо `redirect` (в том числе на
  `GOOGLE_RECAPTCHA` — то есть отказ может быть не «нет», а «докажи, что человек»);
- `conform_action` — «This action is **always an allow** action».

`rate_based_ban` добавляет вторую ступень: `ban_threshold_count` +
`ban_threshold_interval_sec` + `ban_duration_sec` («the additional number of
seconds for which a client is banned after the `interval_sec` period elapses»,
допустимые значения 60…3600). Логика двухуровневая дословно: «the client is
banned for the configured `ban_duration_sec` only if the request rate crosses the
configured `ban_threshold_count`. If the request rate doesn't exceed the
`ban_threshold_count`, the requests keep getting throttled to
`rate_limit_threshold_count`».

**Ключи агрегации** (`enforce_on_key`): `ALL`, `IP`, `HTTP_HEADER`, `XFF_IP`,
`HTTP_COOKIE`, `HTTP_PATH`, `SNI`, `REGION_CODE`, `TLS_JA4_FINGERPRINT`,
`TLS_JA3_FINGERPRINT`, `USER_IP`, `ASN`. Значения обрезаются: «The key value is
truncated to the first 128 bytes». Комбинировать можно «up to three keys».
Отсутствие поля не роняет правило, а деградирует к `ALL` или к `IP` — то есть
**fail-open по ключу**, а не отказ.

**Прямое признание неточности** (важнейшая цитата раздела):

> The configured thresholds for throttling and rate-based bans are enforced
> independently in each of the Google Cloud regions […] If the configured
> threshold is set to 5,000 requests, the backend service might receive 5,000
> requests from one region and 5,000 requests from the second region.
>
> It is important to note that **the enforced rate limits are approximate and might
> not be strictly accurate compared to the configured thresholds**. […] For these
> reasons, **we recommend that you use rate limiting only for abuse mitigation or
> maintaining application and service availability, not for enforcing strict quota
> or licensing requirements.**

Shadow-режим есть штатно: «You can **preview** the effects of rate limiting rules
in a security policy by using preview mode and examining your request logs».

Дефолт при создании политики через LB: «the default threshold is 500 requests
during each one-minute interval (a `rate_limit_threshold_count` and `interval_sec`
of 500 and 60, respectively)».

## Cloudflare

_Обращение: 6 сентября 2026._

### Rate Limiting Rules — параметры и гранулярность

<https://developers.cloudflare.com/waf/rate-limiting-rules/parameters/> (страница
датирована «Last updated Apr 29, 2026»)

- `requests_per_period` — «The number of requests over the period of time that
  will trigger the rule».
- `period` — «The available API values are: **10, 60 (one minute), 120 (two
  minutes), 300 (five minutes), 600 (10 minutes), or 3600 (one hour)**».
- `mitigation_timeout` / «For duration» — «Once the rate is reached, the rate
  limiting rule applies the rule action to further requests for the period of time
  defined in this field». Значения: «0, 10, 60, 120, 300, 600, 3600, or 86400 (one
  day)». То есть отказ **залипает** на настроенный срок, а не снимается сразу, как
  только счётчик опустился.
- `characteristics` — ключ агрегации, произвольный набор полей (IP, заголовок,
  cookie, query, JSON-поле, JWT-claim, форма).
- **Счётчик и матчинг разведены:** `counting_expression` — «By default, the
  counting expression is the same as the rule matching expression. […] The counting
  expression can include HTTP response fields. **When there are response fields in
  the counting expression, the counting will happen after the response is sent.**»

Последнее — заметная семантическая развилка: «допускать» и «списывать» — разные
решения, и списание может происходить **после** обработки, по факту результата
(например, считать только ответы с кодом 403).

### Гранулярность счётчиков: per-datacenter, не глобально

<https://developers.cloudflare.com/waf/rate-limiting-rules/request-rate/>
(«Last updated Apr 16, 2026»)

> Cloudflare does not support global rate limiting counters across the entire
> network. Each data center maintains its own counters. The exception is when
> Cloudflare has multiple data centers associated with a given geographical
> location.
>
> Every rate limiting rule includes the Cloudflare data center ID (`cf.colo.id`)
> as a **mandatory characteristic**. […] When creating rate limiting rules via API,
> you must include the `cf.colo.id` characteristic explicitly.

То есть настроенный лимит — «на дата-центр», и это зашито в модель как
обязательная часть ключа.

### Complexity-based rate limiting — стоимость запроса, известная только постфактум

Там же, раздел «Complexity-based rate limiting» (Enterprise + Advanced Rate
Limiting):

> Not all requests cost the same to serve. […] Request-count-based rate limiting
> treats these equally — 100 lightweight requests and 100 expensive requests
> increment the same counter.
>
> Complexity-based rate limiting addresses this by tracking a **cost score** that
> your origin server assigns to each request […] To use complexity-based rate
> limiting, your origin server must return an HTTP response header containing a
> numeric score for each request. **The value must be between 1 and 1,000,000.**
> You configure which header name the rule reads from.
>
> If the origin server does not provide the HTTP response header with a score value
> or if the score value is outside of the allowed range, **the corresponding rate
> limiting counter will not be updated.**

Параметры: `score_per_period` («Maximum score per period»), `period`, «Response
header name». Пример из документа: `/graphql`, score per period 400, period 1
minute, header `x-score`.

**Это `AllowN(n)`, где `n` становится известным только после выполнения работы.**
Наш порт `AllowN(n int) bool` требует знать `n` заранее. Cloudflare решает это
разделением: пропустить запрос, а списать — по факту; недобор компенсируется на
следующем запросе. Плюс явный fail-open, если origin не вернул score.

### Заявленная погрешность sliding window approximation

Первоисточник — инженерный блог Cloudflare, «How we built rate limiting capable of
scaling to millions of domains»
(<https://blog.cloudflare.com/counting-things-a-lot-of-different-things/>).

Формула дословно:

> Let's say I set a limit of 50 requests per minute on an API endpoint. […] I did
> 18 requests during the current minute, which started 15 seconds ago, and 42
> requests during the entire previous minute. Based on this information, the rate
> approximation is calculated like this:
>
>     rate = 42 * ((60-15)/60) + 18
>          = 49.5 requests
>
> This algorithm assumes a constant rate of requests during the previous sampling
> period […] this is why the result is only an approximation of the actual rate.

**Заявленная погрешность (дословно, на выборке «400 million requests from 270,000
distinct sources»):**

> - **0.003% of requests have been wrongly allowed or rate limited**
> - **An average difference of 6% between real rate and the approximate rate**
> - 3 sources have been allowed despite generating traffic slightly above the
>   threshold (false negatives), the actual rate was less than 15% above the
>   threshold rate
> - **None of the mitigated sources was below the threshold (false positives)**

Свойства, за которые выбрали именно это: «Tiny memory usage: only two numbers per
counter», «Incrementing a counter can be done by sending a single INCR command».

**Почему отвергли leaky bucket** — прямая цитата, полезная нам как контрапункт:

> However, in our case, this approach has two drawbacks: It has two parameters
> (average rate and burst) that are not always easy to tune properly. We were
> constrained to use the memcached protocol and this algorithm requires multiple
> distinct operations that we cannot do atomically.

В сноске уточняется, почему не спас и CAS: «Memcache does support CAS
(Compare-And-Set) operations and so optimistic transactions are possible, but it
is hard to use in our case: during attacks, we will have a lot of requests,
creating a lot of contention, in turn resulting in a lot of CAS transactions
failing». Это ровно тот эффект, который у нас меряется бенчмарком lock-free versus
mutex: под высокой конкуренцией CAS-retry деградирует. Оговорка: у Cloudflare CAS
шёл по сети в memcached, у нас — на одном ядре в памяти, масштаб издержек разный.

Наконец, применение митигации у них **асинхронно относительно допуска**: «the
increment jobs are run asynchronously without slowing down the requests. If the
request rate is above the threshold, another piece of data is stored asking all
servers in the PoP to start applying the mitigation for that client. Only this bit
of information is checked during request processing». То есть горячий путь читает
булев флаг, а счёт идёт в стороне.

## Azure API Management

_Обращение: 6 сентября 2026._

Три политики, три разных контракта — и они **явно противопоставлены по коду
ответа**.

### `rate-limit` / `rate-limit-by-key`

<https://learn.microsoft.com/en-us/azure/api-management/rate-limit-policy>,
<https://learn.microsoft.com/en-us/azure/api-management/rate-limit-by-key-policy>

> The `rate-limit` policy prevents API usage spikes on a per subscription basis by
> limiting the call rate to a specified number per a specified time period. When
> the call rate is exceeded, the caller receives a **429 Too Many Requests**
> response status code.

Атрибуты (дословно):

- `calls` — «The maximum total number of calls allowed during the time interval
  specified in `renewal-period`»;
- `renewal-period` — «The length in seconds of the **sliding window** during which
  the number of allowed requests should not exceed the value specified in `calls`.
  **Maximum allowed value: 300 seconds**»;
- `retry-after-header-name` — «The name of a custom response header whose value is
  the recommended retry interval in seconds», **default `Retry-After`**;
- `retry-after-variable-name`;
- `remaining-calls-header-name` / `remaining-calls-variable-name` — «the number of
  remaining calls allowed for the time interval»;
- `total-calls-header-name` — «The name of a response header whose value is the
  value specified in `calls`».

**Алгоритм зависит от тарифа, и это документировано:**

> The **v2 tiers use a token bucket algorithm** for rate limiting, which differs
> from the **sliding window algorithm in classic tiers**. Because of this
> implementation difference, when you configure token limits in the v2 tiers at
> multiple scopes by using the same `counter-key`, make sure that the
> `tokens-per-minute` value is consistent across all policy instances. Inconsistent
> values can cause unpredictable behavior.

**Поведение при нескольких единицах масштаба — счётчики НЕ агрегируются:**

> This policy tracks calls independently at each gateway where it is applied,
> including workspace gateways and regional gateways in a multi-region deployment.
> **It doesn't aggregate call data across the entire instance.**

> Rate limit counts in a self-hosted gateway can be configured to synchronize
> locally (among gateway instances across cluster nodes) […] However, **rate limit
> counts don't synchronize with other gateway resources** configured in the API
> Management instance, including the managed gateway in the cloud.

`rate-limit-by-key` добавляет ровно то, чего нет у нашего `Limiter`:

- `counter-key` — произвольный ключ («API Management uses a single counter for each
  `counter-key` value that you specify in the policy. The counter is updated at all
  scopes at which the policy is configured with that key value»);
- `increment-condition` — «Optional increment condition can be added to specify
  which requests should be counted towards the limit»;
- `increment-count` — стоимость запроса (`AllowN`).

И документированная плата за постфактум-подсчёт:

> When `increment-condition` or `increment-count` are defined using expressions,
> evaluation and increment of the rate limit counter are **postponed to the end of
> outbound pipeline** to allow for policy expressions based on the response. Limit
> exceeded condition is not evaluated at the same time in this case and will be
> evaluated on next incoming call. This leads to cases where **429 Too Many
> Requests status code is returned 1 call later than usual.**

Пример из документа — счёт только успешных ответов:
`increment-condition="@(context.Response.StatusCode == 200)"`,
`counter-key="@(context.Request.IpAddress)"`.

### `quota` — длинное окно, и это уже НЕ 429

<https://learn.microsoft.com/en-us/azure/api-management/quota-policy>

> The `quota` policy enforces a renewable or lifetime call volume and/or bandwidth
> quota, on a per subscription basis. When the quota is exceeded, the caller
> receives a **403 Forbidden** response status code, and the response includes a
> **`Retry-After`** header whose value is the recommended retry interval in seconds.

**Двумерность в одной политике:** `calls` (число вызовов) и `bandwidth`
(килобайты) — «Either `calls`, `bandwidth`, or both together must be specified».
Пример из документа: `<quota calls="10000" bandwidth="40000" renewal-period="3600" />`.

`renewal-period` — «The length in seconds of the **fixed window** after which the
quota resets. The start of each period is calculated relative to the start time of
the subscription. **When `renewal-period` is set to 0, the period is set to
infinite**» (пожизненная квота).

Вложенность `<api>`/`<operation>` создаёт независимые счётчики: «Product and API
call quotas are applied independently», «Product, API, and operation call quotas
are applied independently».

Честная оговорка про fail-open при рестарте:

> When underlying compute resources restart in the service platform, API Management
> might continue to handle requests for a short period after a quota is reached.

### `llm-token-limit` — RPM+TPM прямо в gateway

<https://learn.microsoft.com/en-us/azure/api-management/llm-token-limit-policy>

Наиболее близкий к нашей теме образец «двумерного лимита», реализованного не
провайдером модели, а посредником:

> The `llm-token-limit` policy prevents large language model (LLM) API usage spikes
> on a per key basis by limiting consumption of language model tokens to either a
> specified rate (number per minute), a quota over a specified period, or both.
> **When a specified token rate limit is exceeded, the caller receives a 429 Too
> Many Requests response status code. When a specified quota is exceeded, the
> caller receives a 403 Forbidden response status code.**

Поддерживаемые схемы: «OpenAI Chat Completions or Responses API», «Anthropic
Messages API (currently supported in API Management v2 tiers)», «Google Vertex AI
API».

Атрибуты: `tokens-per-minute` («The maximum number of tokens consumed by prompt and
completion per minute»), `token-quota` + `token-quota-period` (`Hourly | Daily |
Weekly | Monthly | Yearly`), «Either a rate limit (`tokens-per-minute`), a quota
(`token-quota` over a `token-quota-period`), or both must be specified».

Наблюдаемый контракт — **шесть** выходных значений (по три пары
header/variable): `remaining-tokens-*` (остаток по TPM), `remaining-quota-tokens-*`
(остаток по квоте), `tokens-consumed-*` (сколько списано этим вызовом), плюс
`retry-after-*`.

**Самое интересное — как решают проблему «стоимость неизвестна до выполнения»**
(раздел «Considerations for token counts and estimation», дословно):

> Without prompt token estimation (`estimate-prompt-tokens="false"`): The policy
> uses actual token usage values from the `usage` section of the LLM API response.
> **Prompts may be sent to the backend even when the limit is exceeded; this is
> detected from the response, after which subsequent requests are blocked** until
> the limit resets.
>
> With prompt token estimation (`estimate-prompt-tokens="true"`): The policy
> estimates prompt tokens from the prompt schema in the API definition **before
> sending the request**. This can reduce unnecessary backend requests when the
> limit is already exceeded, but may reduce performance.
>
> **Concurrency**: Because the exact number of tokens consumed can't be determined
> until responses are received from the backend, **concurrent or near-concurrent
> requests can temporarily exceed the configured token limit.** Once responses are
> processed and the limit is exceeded, subsequent requests are blocked until the
> limit resets.
>
> **Remaining quota accuracy**: The estimated remaining token quota […] **may be
> larger than expected** based on actual token consumption, and becomes more
> accurate as the quota is approached.

Плюс отдельная оговорка про изображения: «when streaming is enabled or
`estimate-prompt-tokens` is set to true, the policy **overcounts each image as a
maximum of 1200 tokens**» — то есть при неизвестной стоимости оценивают
консервативно вверх.

## GitHub

_Обращение: 6 сентября 2026._

### Два независимых слоя: primary и secondary

<https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api>

**Primary** (числа дословно): unauthenticated — «60 requests per hour»;
authenticated user — «5,000 requests per hour», GitHub App/OAuth app, принадлежащий
GHEC-организации — «15,000 requests per hour»; GitHub App installation — «5,000
requests per hour», на GHEC — «15,000»; `GITHUB_TOKEN` в Actions — «1,000 requests
per hour per repository».

Тонкость, которая ломает наивную модель «у каждого свой бакет»:

> However, requests made by a higher-limit app reduce the remaining budget
> available for lower-limit authentication methods. For example, if an app with a
> 15,000 request limit makes 10,000 requests on your behalf, you will have
> exhausted the 5,000 request budget for your personal access tokens, even though
> the app has 5,000 requests remaining.

То есть **вложенные бюджеты**: несколько лимитов над одним счётчиком расхода.

**Secondary** — про частоту и про создание контента:

> Make too many requests to a single endpoint per minute. **No more than 900 points
> per minute are allowed for REST API endpoints, and no more than 2,000 points per
> minute are allowed for the GraphQL API endpoint.**
>
> Create too much content on GitHub in a short amount of time. In general, **no
> more than 80 content-generating requests per minute and no more than 500
> content-generating requests per hour** are allowed.
>
> These secondary rate limits are subject to change without notice. **You may also
> encounter a secondary rate limit for undisclosed reasons.**

Стоимость запроса в «points» задана таблицей дословно:

| Request | Points |
|---|---|
| GraphQL requests without mutations | 1 |
| GraphQL requests with mutations | 5 |
| Most REST API `GET`, `HEAD`, and `OPTIONS` requests | 1 |
| Most REST API `POST`, `PATCH`, `PUT`, or `DELETE` requests | 5 |

Плюс оговорка: «Some REST API endpoints have a different point cost that is **not
shared publicly**».

### Заголовки — дословно из документации

| Header | Описание (дословно) |
|---|---|
| `x-ratelimit-limit` | «The maximum number of requests that you can make per hour» |
| `x-ratelimit-remaining` | «The number of requests remaining in the current rate limit window» |
| `x-ratelimit-used` | «The number of requests you have made in the current rate limit window» |
| `x-ratelimit-reset` | «The time at which the current rate limit window resets, in UTC epoch seconds» |
| `x-ratelimit-resource` | «The rate limit resource that the request counted against» |

Обратить внимание: **`x-ratelimit-resource`** — то же, что `pk`/policy name в
IETF-драфте: «какой именно счётчик списан».

Отказ — **и 403, и 429**, семантика разная:

> If you exceed your primary rate limit, you will receive a **403 or 429** response,
> and the `x-ratelimit-remaining` header will be 0. You should not retry your
> request until after the time specified by the `x-ratelimit-reset` header.
>
> If you exceed a secondary rate limit, you will receive a **403 or 429** response
> and an error message that indicates that you exceeded a secondary rate limit. If
> the `retry-after` response header is present, you should not retry your request
> until after that many seconds has elapsed. If the `x-ratelimit-remaining` header
> is 0, you should not retry […] Otherwise, **wait for at least one minute** before
> retrying.

И признание асимметрии наблюдаемости: «**There is not a way to check the status of
your secondary rate limit.**» Есть отдельный endpoint `GET /rate_limit`: «Calling
this endpoint does not count against your primary rate limit, but it can count
against your secondary rate limit».

### GraphQL — «points» вместо запросов, с самоописанием стоимости

<https://docs.github.com/en/graphql/overview/rate-limits-and-node-limits-for-the-graphql-api>

> The GraphQL API assigns points to each query and limits the points that you can
> use within a specific amount of time.

Числа: «For users: **5,000 points per hour** per user»; GHEC — 10,000. «For GitHub
App installations not on a GitHub Enterprise Cloud organization: 5,000 points per
hour per installation. Installations that have more than 20 repositories receive
another 50 points per hour for each repository. […] **The rate limit cannot
increase beyond 12,500 points per hour**».

Объект `rateLimit` в самом GraphQL-ответе даёт `limit`, `remaining`, `used`,
`resetAt` — и, отдельно, **`cost`**: «You can return the point value of a query by
querying the `cost` field on the `rateLimit` object». То есть API умеет сказать
«сколько бы стоил этот запрос» — прямой аналог dry-run/`Peek`. Плюс клиентская
оценка заранее: «Add up the number of requests needed to fulfill each unique
connection in the call. Assume every request will reach the first or last argument
limits. Divide the number by 100 and round the result».

## Stripe

_Обращение: 6 сентября 2026._

Инженерный блог: «Scaling your API with rate limiters»
(<https://stripe.com/blog/rate-limiters>). Ценность для нас — **классификация**:
Stripe различает *rate limiters* (решают по пользователю) и *load shedders*
(решают по состоянию системы).

> A **load shedder** makes its decisions based on the whole state of the system
> rather than the user who is making the request. Load shedders help you deal with
> emergencies, since they keep the core part of your business working.

Четыре описанных механизма:

| Механизм | Назначение (дословно/близко к тексту) |
|---|---|
| **Request rate limiter** | «restricts each user to *N* requests per second» |
| **Concurrent requests limiter** | «You can only have 20 API requests in progress at the same time» — ограничение **одновременности**, а не темпа |
| **Fleet usage load shedder** | «ensures that a certain percentage of your fleet will always be available for your most important API requests» |
| **Worker utilization load shedder** | последняя линия: при насыщении воркеров «shed lower-priority traffic, starting with test mode traffic» |

Коды: 429 для rate limiting, 503 для load shedding.

**Важное для нашего порта:** «concurrent requests limiter» — это лимитер, у
которого **нет времени в модели вообще**. Он считает не «сколько за интервал», а
«сколько прямо сейчас», и требует парного вызова release. Наш `Limiter` с одним
методом `Allow() bool` такой семантики выразить не может в принципе — ей нужен
`Acquire()/Release()`.

## Shopify

_Обращение: 6 сентября 2026._

<https://shopify.dev/docs/api/usage/rate-limits>

**Алгоритм назван прямо:** «All Shopify APIs use a **leaky bucket algorithm** to
manage requests». Метафора из документа дословно:

> - Each app has access to a bucket. It can hold, say, 60 "marbles".
> - Each API request tosses **some number of marbles** into the bucket.
> - Each second, a marble is removed from the bucket (if there are any). This
>   restores capacity for more marbles.
> - If the bucket gets full, you get a throttle error and have to wait for more
>   bucket capacity to become available.

Это **leaky bucket as a meter** (счётчик, вытекающий с постоянной скоростью) —
ровно модель нашего `leakybucket.go`. И — «tosses **some number of** marbles» —
переменная стоимость, `AllowN`.

Лимиты (дословно из таблицы): GraphQL Admin API — «Calculated query cost», 100
points/second (Standard), 200 (Advanced), 1000 (Plus), 2000 (Commerce Components);
Storefront API — «None»; Payments Apps API — 27300 points/second и выше.

**Стоимость поля** задана схемой: Scalar 0, Enum 0, Object 1, Interface — «Maximum
of possible selections», Union — то же, Connection — «Sized by `first` and `last`
arguments», Mutation 10. «A single query may not exceed a cost of **1,000 points**,
regardless of plan limits».

### Самое ценное: requested cost vs actual cost — это резервирование с возвратом

> Shopify calculates the cost of a query **both before and after execution**.
>
> - The **requested cost** is based on the composition of fields selected in the
>   request.
> - The **actual cost** is based on the query results, and may be lower than
>   requested cost […]
>
> Rate limits use a combination of the requested and actual query cost. **Before
> execution begins, an app's bucket must have enough capacity for the requested
> cost of a query. When execution is complete, the bucket is refunded the
> difference between the requested cost and the actual cost of the query.**

Это буквально паттерн `Reserve` → `Commit(actual)` / частичный `Return`. Наш
`AllowN(n) bool` покрывает только первую половину; вернуть неиспользованное он не
умеет.

Наблюдаемый контракт возвращается **в теле ответа**, не в заголовках:

```json
"extensions": {
  "cost": {
    "requestedQueryCost": 101,
    "actualQueryCost": 46,
    "throttleStatus": {
      "maximumAvailable": 1000,
      "currentlyAvailable": 954,
      "restoreRate": 50
    }
  }
}
```

`throttleStatus` — это **полное состояние бакета наружу**: ёмкость, текущий
остаток, скорость восстановления. Плюс debug-режим по заголовку запроса
`Shopify-GraphQL-Cost-Debug=1`, который даёт разбивку стоимости по полям
(`definedCost`, `requestedTotalCost`, `requestedChildrenCost`).

Отказ по ресурсным лимитам — «429 Too Many Requests […] and a message that a
throttle has been applied». Отдельная экзотика: у Storefront API checkout-throttle
возвращает **`200 Throttled`** (успешный HTTP-код с ошибкой в теле), а
подозрительный трафик — «430 Shopify Security Rejection».

Ресурсные лимиты как третья ось: «The following GraphQL Admin API types have an
additional throttle that takes effect when a store has 500,000 product variants.
After this threshold is reached, no more than 10,000 new variants can be created
per day».

Классический REST-заголовок `X-Shopify-Shop-Api-Call-Limit` в текущей странице
`shopify.dev/docs/api/usage/rate-limits` (обращение 06.09.2026) **не найден** —
REST Admin API выведен из основного потока документации. Считать имя заголовка
**не проверенным** по актуальному первоисточнику.

## Slack

_Обращение: 6 сентября 2026._

<https://docs.slack.dev/apis/web-api/rate-limits/>

Модель — **тарифные полки на метод**, а не единый лимит:

| Tier | Лимит (дословно) | Про всплески |
|---|---|---|
| Tier 1 | «1+ per minute» | «Access tier 1 methods infrequently. A small amount of burst behavior is tolerated» |
| Tier 2 | «20+ per minute» | «occasional bursts of more requests» |
| Tier 3 | «50+ per minute» | «Sporadic bursts are welcome» |
| Tier 4 | «100+ per minute» | «generous burst behavior» |
| Special | «Rate limiting conditions are unique for methods with this tier» | смотреть документацию метода |

Отказ:

> Slack will return a `HTTP 429 Too Many Requests` error, and a `Retry-After` HTTP
> header containing the number of seconds until you can retry.

Пример из документа — `HTTP/1.1 429 Too Many Requests` + `Retry-After: 30`.

Обратить внимание на форму «**1+ per minute**», «20+ per minute»: лимит объявлен
**нижней границей гарантии**, а не точным потолком. Это честнее, чем «ровно N», и
согласуется с формулировкой AWS «targets rather than guaranteed request ceilings».

## Discord

_Обращение: 6 сентября 2026._

<https://discord.com/developers/docs/topics/rate-limits>

Самый явно выраженный контракт «идентифицируй бакет» из всех рассмотренных.

> Per-route rate limits exist for many individual endpoints, and may include the
> HTTP method (`GET`, `POST`, `PUT`, or `DELETE`). In some cases, per-route limits
> will be shared across a set of similar endpoints, indicated in the
> `X-RateLimit-Bucket` header. **It's recommended to use this header as a unique
> identifier for a rate limit, which will allow you to group shared limits as you
> encounter them.**

Заголовки (дословно):

| Header | Описание |
|---|---|
| `X-RateLimit-Limit` | «The number of requests that can be made» |
| `X-RateLimit-Remaining` | «The number of remaining requests that can be made» |
| `X-RateLimit-Reset` | «Epoch time […] at which the rate limit resets» |
| `X-RateLimit-Reset-After` | «Total time (in seconds) of when the current rate limit bucket will reset. **Can have decimals** to match previous millisecond ratelimit precision» |
| `X-RateLimit-Bucket` | «A unique string denoting the rate limit being encountered (non-inclusive of top-level resources in the path)» |
| `X-RateLimit-Global` | «Returned **only on HTTP 429** responses if the rate limit encountered is the global rate limit (not per-route)» |
| `X-RateLimit-Scope` | «Returned only on HTTP 429 responses. Value can be `user` (per bot or user limit), `global` (per bot or user global limit), or `shared` (per resource limit)» |

Тело 429 — JSON с полями `message`, `retry_after` (**float**, «The number of
seconds to wait before submitting another request»), `global` (bool), `code?`.

Три яруса лимитов сразу:

1. per-route (ключ — `X-RateLimit-Bucket`);
2. global: «All bots can make up to **50 requests per second** to our API. If no
   authorization header is provided, then the limit is applied to the IP address.
   This is independent of any individual rate limit on a route»;
3. «Invalid Request Limit aka Cloudflare bans»: «IP addresses that make too many
   invalid HTTP requests are automatically and temporarily restricted […]
   Currently, this limit is **10,000 per 10 minutes**. **An invalid request is one
   that results in 401, 403, or 429 statuses.**»

Третий ярус — лимит **на неудачи**, а не на запросы: наказание за плохое поведение
клиента, включая сами 429. Прямая защита от retry-шторма.

Честная оговорка про неточность в частном случае: «Routes for controlling emojis do
not follow the normal rate limit conventions. […] This means that **the quota
returned by our APIs may be inaccurate**, and you may encounter 429s».

## Twilio

Автоматическое получение официальных страниц Twilio по rate limiting в этой сессии
не удалось (`twilio.com/docs/messaging/guides/api-rate-limits` вернул пустой
документ, `twilio.com/docs/usage/requests-to-twilio` не содержит раздела о
лимитах). **Не проверено** — числа и имена заголовков Twilio в отчёт не вносятся.
Тема закрыта как не давшая проверяемого материала; Atlassian и Discord покрывают
тот же класс контракта лучше.

## Atlassian

_Обращение: 6 сентября 2026._

<https://developer.atlassian.com/cloud/jira/platform/rate-limiting/>

**Три одновременно действующие системы** — самый явный пример составного лимита:

> Jira Cloud enforces **three independent rate limiting systems that work
> simultaneously** […] Your app or integration must handle all three:
>
> - **Points-based quota (per-hour)** […]
> - **Burst API rate limits (per-second)** […]
> - **Per-issue write limits** […]

Стоимость в points: «Each request starts with a base cost of 1 point, and
additional points are added for each object involved. **Write requests are charged
only the base cost, with no additional points.**» Таблица: core domain objects
(GET/query) — 1 point; identity & access (Users, Groups, Permissions) — **2
points**; write/modify/delete — 1 point; others — 1 point.

**Burst — token bucket, названный прямо:**

> Jira implements Burst API Rate Limit using the **token bucket algorithm**. […]
> For each tenant, Jira maintains a separate token bucket for every API endpoint.
> […] **Steady-state refill rate**: The sustained number of requests per second
> […] **Burst buffer**: The total bucket size that allows for temporary traffic
> spikes above the steady-state rate (e.g., 100 tokens).
>
> Buckets automatically refill at the steady-state rate. […] **Tokens that would
> exceed the maximum capacity are discarded.**

Дефолты по методу (дословно): GET 100 RPS, POST 100 RPS, PUT 50 RPS, DELETE 50 RPS;
плюс список эндпоинтов с индивидуальными значениями (от 5 до 500 RPS).

**Per-issue write — два окна одновременно** (чистый «составной лимит» в нашем
смысле):

> - Short window: **20 write operations per 2 seconds**
> - Long window: **100 write operations per 30 seconds**

**Отказ говорит, КАКОЙ лимит сработал** — отдельным заголовком `RateLimit-Reason`:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 1
X-RateLimit-Limit: 350
X-RateLimit-Remaining: 0
X-RateLimit-Reset: 2026-01-01T01:01:01Z
RateLimit-Reason: jira-burst-based
```

Значения `RateLimit-Reason`: `jira-quota-global-based`, `jira-quota-tenant-based`,
`jira-burst-based`, `jira-per-issue-on-write`. Практическая ценность названа прямо:
«For `jira-per-issue-on-write`: Add delays between writes to the same issue, **but
you can continue making other API requests normally**». Без указателя на конкретный
лимит клиент вынужден тормозить всё.

**«Near limit» — отдельный заголовок:** `X-RateLimit-NearLimit` — «Returns `true`
when **less than 20% of capacity remains**. Not used for request rate limiting».

**Atlassian внедряет IETF-драфт — и делает это в shadow-режиме.** Это единственный
найденный публичный адоптер структурных полей `RateLimit`/`RateLimit-Policy`:

> **Beta headers are informational only and do not trigger enforcement or
> throttling.** You can use them now to monitor your usage and prepare for future
> enforcement. At enforcement `Beta-` prefix will be dropped from all beta headers.

Параметры совпадают с draft-11: `q` — «Total quota», `w` — «Time window in
seconds», `r` — «Remaining quota. **Optionally included. If absent, your app is
well within its limits**», `t` — «Seconds until reset». Примеры дословно:

```
# норма (r опущен намеренно)
Beta-RateLimit-Policy: "global-app-quota";q=65000;w=3600
Beta-RateLimit:        "global-app-quota";t=3200

# близко к лимиту (<~20% остатка)
Beta-RateLimit-Policy: "global-app-quota";q=65000;w=3600
Beta-RateLimit:        "global-app-quota";r=11000;t=600

# две политики сразу
Beta-RateLimit-Policy: "global-app-quota";q=65000;w=3600,"jira-burst-based";q=100;w=1
Beta-RateLimit:        "global-app-quota";t=200,"jira-burst-based";r=90;t=1

# квота исчерпана (в бете — без 429)
Beta-RateLimit: "global-app-quota";r=0;t=50
Beta-Retry-After: 50
```

Приём «`r` присылаем только когда близко к лимиту» — экономный способ реализовать
near-limit-предупреждение в рамках стандарта.

## OpenAI

_Обращение: 6 сентября 2026._

**Оговорка о проверяемости.** Официальные страницы
`platform.openai.com/docs/guides/rate-limits` и её преемник
`developers.openai.com/api/docs/guides/rate-limits`, а также help-центр OpenAI на
06.09.2026 отдают **HTTP 403** автоматическим запросам. Поэтому ниже — только то,
что подтверждено первоисточниками, до которых удалось дойти; конкретные имена
заголовков `x-ratelimit-*` и числа лимитов по тирам **не проверены** и здесь не
приводятся.

Подтверждено официальным OpenAI Cookbook (репозиторий `openai/openai-cookbook`,
`examples/How_to_handle_rate_limits.ipynb`):

- отказ выглядит как `429: 'Too Many Requests'` / `RateLimitError`;
- текст ошибки называет измерение и оба числа:
  `«Rate limit reached for default-codex in organization org-{id} on requests per
  min. Limit: 20.000000 / min. Current: 24.000000 / min.»` — то есть в теле ошибки
  сообщается лимит и текущее наблюдаемое значение;
- рекомендация — «retry requests with a **random exponential backoff**», причём
  джиттер обоснован явно: «Adding random jitter to the delay helps retries from all
  hitting at the same time»;
- **неудачные запросы тоже расходуют лимит:** «Note that unsuccessful requests
  contribute to your per-minute limit, so continuously resending a request won't
  work». Это отдельный, редко проговариваемый инвариант: списание происходит на
  входе, а не по факту успеха.

Подтверждено чтением официального SDK `openai/openai-python`
(`src/openai/_base_client.py`): клиент разбирает заголовки `retry-after` **и
`retry-after-ms`** — то есть OpenAI отдаёт задержку ещё и в миллисекундах,
нестандартным полем (RFC 9110 знает только целые секунды или HTTP-date).

Двумерность (RPM + TPM) у OpenAI как таковая широко известна, но **дословную
формулировку из официальной документации подтвердить не удалось** — она изложена
ниже только по Anthropic и Azure APIM, где источник читается.

## Anthropic

_Обращение: 6 сентября 2026._

<https://docs.anthropic.com/en/api/rate-limits>

**Алгоритм назван прямо, и это token bucket:**

> The API uses the **token bucket algorithm** to do rate limiting. This means that
> your capacity is **continuously replenished** up to your maximum limit, rather
> than being reset at fixed intervals.

С оговоркой про эффективное окно:

> You might hit rate limits over shorter time intervals. For instance, **a rate of
> 60 requests per minute (RPM) might be enforced as 1 request per second.** Short
> bursts of requests can exceed the limit and trigger rate limit errors.

Это ровно семантика Apigee SpikeArrest, только описанная честно как следствие
малой ёмкости бакета.

**Трёхмерный лимит, не двумерный:**

> The rate limits for the Messages API are measured in **requests per minute (RPM),
> input tokens per minute (ITPM), and output tokens per minute (OTPM)** for each
> model class. If you exceed **any** of the rate limits you will get a 429 error
> **describing which rate limit was exceeded**, along with a `retry-after` header
> indicating how long to wait.

Плюс четвёртое, неявное измерение — «acceleration limits»: «You might also
encounter 429 errors because of **acceleration limits** on the API if your
organization has a sharp increase in usage. To avoid hitting acceleration limits,
ramp up your traffic gradually and maintain consistent usage patterns». То есть
ограничивается не только уровень, но и **производная** нагрузки.

**Разные измерения списываются в разные моменты:**

- ITPM — «ITPM rate limits are **estimated at the beginning of each request**, and
  the estimate is **adjusted during the request** to reflect the actual number of
  input tokens used» (оценка вперёд + коррекция — снова паттерн Shopify);
- OTPM — «OTPM rate limits are **evaluated in real time as output tokens are
  produced**, counting only the actual tokens generated. The `max_tokens` parameter
  does not factor into OTPM rate limit calculations» (списание потоком, по мере
  генерации).

Cache-aware ITPM: «`input_tokens` […] ✓ Count toward ITPM»,
«`cache_creation_input_tokens` […] ✓ Count toward ITPM»,
«`cache_read_input_tokens` […] ✗ Do NOT count toward ITPM for most models» (с
исключением: Claude Haiku 3.5 «also counts `cache_read_input_tokens` toward ITPM
rate limits»). То есть **стоимость запроса зависит от состояния кэша сервера** —
клиент не может вычислить её сам.

Заголовки — дословный перечень:

| Header | Описание |
|---|---|
| `retry-after` | «The number of seconds to wait until you can retry the request. Earlier retries will fail. **Not sent with the spend-cap 429**» |
| `anthropic-ratelimit-requests-limit` / `-remaining` / `-reset` | лимит/остаток/момент полного восстановления по запросам; reset — «in **RFC 3339** format» |
| `anthropic-ratelimit-tokens-limit` / `-remaining` / `-reset` | по токенам суммарно; remaining — «**rounded to the nearest thousand**» |
| `anthropic-ratelimit-input-tokens-limit` / `-remaining` / `-reset` | по входным токенам |
| `anthropic-ratelimit-output-tokens-limit` / `-remaining` / `-reset` | по выходным токенам |
| `anthropic-priority-input-tokens-*`, `anthropic-priority-output-tokens-*` | то же для Priority Tier |
| `anthropic-workspace-id` | «carries the ID of the Workspace that your API key or access token resolved to» |

Два приёма, которых нет больше нигде в выборке:

1. **Агрегирующее поле показывает самый узкий из действующих лимитов:**
   «The `anthropic-ratelimit-tokens-*` headers display the values for **the most
   restrictive limit currently in effect**. For instance, if you have exceeded the
   Workspace per-minute token limit, the headers will contain the Workspace
   per-minute token rate limit values.» Это способ отдать «одно число» при
   нескольких политиках, не заставляя клиента разбирать все.
2. **Намеренное огрубление:** остаток по токенам округляется до тысяч. Точное
   состояние счётчика наружу не отдаётся.

Отдельный класс отказа — **spend cap**, который выглядит как rate limit, но им не
является:

> Once you reach your tier's spend cap, API usage pauses until 00:00 UTC on the
> first day of the next month […] While usage is paused, API requests return HTTP
> 429 […] **The error type is `rate_limit_error`, the same as for a rate limit, but
> the response has no `retry-after` header. Retrying, including the SDKs' automatic
> retries, fails until access resumes.**

Отсутствие `retry-after` здесь несёт смысл: «ретраить бессмысленно». Отказ с
`retry-after` и отказ без него — разные контракты под одним кодом.

## Прочее (Upstash, Unkey, Zuplo)

_Обращение: 6 сентября 2026._

### Upstash Ratelimit — библиотечный аналог нашего порта, но не `bool`

<https://upstash.com/docs/redis/sdks/ratelimit-ts/methods>

Сигнатура и возвращаемый тип дословно:

```ts
ratelimit.limit(
  identifier: string,
  req?: { geo?: Geo; rate?: number, ip?: string, userAgent?: string, country?: string },
): Promise<RatelimitResponse>

export type RatelimitResponse = {
  success: boolean;        // "Whether the request may pass(true) or exceeded the limit(false)"
  limit: number;           // "Maximum number of requests allowed within a window."
  remaining: number;       // "How many requests the user has left within the current window."
  reset: number;           // "Unix timestamp in milliseconds when the limits are reset."
  pending: Promise<unknown>;
  reason?: RatelimitResponseType;
  ...
}
```

Здесь сразу три вещи, релевантные нашему `Limiter`:

- **`rate` в аргументах** — «The `rate` field determines the amount of
  tokens/requests to subtract from the state of the algorithm» — это наш
  `AllowN(n)`, только опциональный параметр, а не отдельный метод;
- **`reason`** — почему именно отказ: «Is set to **`"timeout"`** when request times
  out», «`"cacheBlock"` when an identifier is blocked through cache without calling
  redis because it was rate limited previously», «`"denyList"` when identifier or
  one of ip/user-agent/country parameters is in deny list»; «Is set to `undefined`
  if rate limit check had to use Redis». Значение `"timeout"` — это
  документированный **fail-open**: при недоступности хранилища библиотека
  принимает решение сама;
- **`pending`** — отложенная работа (синхронизация между регионами, аналитика),
  которую вызывающий обязан довести: «On Vercel Edge or Cloudflare workers, you need
  to explicitly handle the pending Promise». Признание, что горячий путь и учёт —
  разные фазы.

<https://upstash.com/docs/redis/sdks/ratelimit-ts/algorithms> — три алгоритма,
описанных с плюсами и минусами:

- **Fixed Window**: «Very cheap in terms of data size and computation», против «Can
  cause high bursts at the window boundaries to leak through», «Causes request
  stampedes […] whenever a new window begins».
- **Sliding Window** — **та же формула, что у Cloudflare**, дословно:
  `rate = 4 * ((60 - 15) / 60) + 5 = 8; return rate < limit`. Минус назван прямо:
  «**Is only an approximation**, because it assumes a uniform request flow in the
  previous window, but this is fine in most cases». Плюс важная деталь контракта:
  «`reset` field in the `limit` and `getRemaining` methods of sliding window **do
  not provide an exact reset time**. Instead, the reset time is the start time of
  the next window».
- **Token Bucket**: «Allows to set a higher initial burst limit by setting
  `maxTokens` higher than `refillRate`», минус — «**Expensive in terms of
  computation**» (в их случае — из-за Lua-скрипта в Redis, не из-за арифметики).

Отдельно про границы окна: «In fixed & sliding window algorithms, the reset time is
based on **fixed time boundaries** (which depend on the period), **not on when the
first request was made**» — прямая противоположность режиму `flexi` у Apigee.

### Unkey, Zuplo

Рассмотрены поверхностно и **отброшены** как не дающие нового по сравнению с уже
разобранным: обе — коммерческие обёртки над теми же тремя алгоритмами (fixed
window / sliding window / token bucket) с распределённым счётчиком; новой семантики
контракта (сверх `limit`/`remaining`/`reset`/`cost`) в них не обнаружено.
Утверждать что-либо про их внутреннюю арифметику по официальным источникам в этой
сессии не проверялось — **не проверено**.

## Наблюдаемый контракт: что возвращают кроме bool

_в работе_

## Составные и многомерные лимиты

_в работе_

## Поведение при отказе и shadow-режимы

_в работе_

## Стандарты заголовков

_Обращение: 6 сентября 2026._

### IETF `draft-ietf-httpapi-ratelimit-headers` — текущее состояние

**Статус на 06.09.2026: активный Internet-Draft рабочей группы HTTPAPI, версия
`-11` от 23 мая 2026, срок истечения 24 ноября 2026. RFC НЕ выпущен**
(datatracker показывает стадию «I-D Exists», Intended RFC status — «(None)»;
early-review HTTPDIR по версии `-10` от 16 января 2026 — вердикт «Not ready»).
Документ заменяет `draft-polli-ratelimit-headers`.
Источник: <https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/>

**Важно: привычные `RateLimit-Limit` / `RateLimit-Remaining` / `RateLimit-Reset`
из спецификации выброшены.** История имён по ревизиям (проверено чтением самих
ревизий):

| Ревизия | Дата | Какие поля определены |
|---|---|---|
| `-05` | 6 июля 2022 | **четыре** отдельных поля: `RateLimit-Limit` (§5.1), `RateLimit-Policy` (§5.2), `RateLimit-Remaining` (§5.3), `RateLimit-Reset` (§5.4). «RateLimit-Limit and RateLimit-Reset fields are REQUIRED; RateLimit-Remaining field is RECOMMENDED» |
| `-07` | 24 июня 2023 | **два** поля: `RateLimit` (Dictionary с ключами `limit`/`remaining`/`reset`) и `RateLimit-Policy` |
| `-08` | 7 октября 2024 | **два** поля, рефакторинг в списки Items с параметрами. Changelog: «Refactored both fields to lists of Items that identify policy and use parameters», «Added quota unit parameter», «Added partition key parameter» |
| `-11` | 23 мая 2026 | **два** поля: `RateLimit-Policy` (§3) и `RateLimit` (§4) |

Ссылки на ревизии: [-05](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers-05),
[-07](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers-07),
[-08](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers-08),
[-11](https://www.ietf.org/archive/id/draft-ietf-httpapi-ratelimit-headers-11.txt).

### Точный синтаксис draft-11 (дословно из текста `-11`)

`RateLimit-Policy` — «a non-empty List[SF] of Quota Policy Items». Параметры:

- `q` — «The REQUIRED "q" parameter indicates the quota allocated by this policy
  measured in quota units». Значение — non-negative Integer.
- `qu` — «The OPTIONAL "qu" parameter value conveys the quota units associated to
  the "q" parameter. The default quota unit is "requests"». Спецификация
  определяет **три** единицы: `requests`, `content-bytes`, `concurrent-requests`.
- `w` — «The OPTIONAL "w" parameter value conveys a time window».
- `pk` — «The OPTIONAL "pk" parameter value conveys the partition key associated
  to the corresponding request»; «Quotas are allocated per partition key».

`RateLimit` — «represents the quota currently available under a specific policy».
Параметры:

- `r` — «This REQUIRED parameter value conveys the available quota under the
  identified policy». Non-negative Integer в quota units.
- `t` — «This OPTIONAL parameter value conveys the effective window under the
  identified policy: the time within which the client can use no more than the
  available quota». Целое число секунд, совместимое с `delay-seconds`; выбор
  относительного времени обоснован явно: «it does not rely on clock
  synchronization and is resilient to clock adjustment and clock skew».
- `pk` — partition key.

Примеры из документа:

```
RateLimit-Policy: "burst";q=100;w=60,"daily";q=1000;w=86400
RateLimit: "default";r=50;t=30
```

### Что draft-11 говорит про семантику отказа

- **Никакой связи с кодом ответа не требуется:** «A server MAY return RateLimit
  header fields independently of the response status code. This includes
  throttled responses. This document does not mandate any correlation between the
  RateLimit header field values and the returned status code» (§6).
- **`r > 0` — не гарантия допуска:** «Clients MUST NOT assume that a positive
  available quota is a guarantee that further requests will be served» (§4.1.1) и
  «Clients MUST NOT consider the available quota parameter as a service level
  agreement. In case of resource saturation, the server MAY artificially lower
  the returned values or not serve the request regardless of the advertised
  quotas» (§8.3).
- **Окно не фиксировано:** «The effective window does not imply anything about
  when the server will increase the available quota. The effective window does
  not necessarily end at a fixed point in time. Subsequent requests might return
  a higher effective window value to limit concurrency or implement dynamic or
  adaptive throttling policies» (§8.4).
- **Взаимодействие с `Retry-After`:** «If a response contains both the
  Retry-After and the RateLimit header fields, the Retry-After field value SHOULD
  NOT reference a point in time earlier than the end of the effective window»
  (§6). В §7 (клиентская сторона): «If a response contains both the RateLimit and
  Retry-After fields, the Retry-After field MUST take precedence».
- **Проблема синхронного возврата толпы названа явно:** «When returning effective
  window values, servers must be aware that many throttled clients may come back
  at the very moment specified. This is true for Retry-After too» (§8.5).

### Problem types (draft-11 §5) — машиночитаемая причина отказа

Три типа задачи, все с расширением `violated-policies` (массив имён политик):

| Problem type | Пример статуса в документе | Смысл |
|---|---|---|
| `…http-problem-types#quota-exceeded` | 429 | клиент превысил одну или несколько квот |
| `…http-problem-types#temporary-reduced-capacity` | 503 | временное снижение ёмкости сервера; сервер MAY отдать `RateLimit-Policy` с новой пониженной квотой |
| `…http-problem-types#abnormal-usage-detected` | 429 | обнаружен паттерн, похожий на злоупотребление |

Это ровно ответ на вопрос «что вернуть кроме bool»: **какая именно политика из
нескольких сработала**. Одного `false` для этого не хватает.

### RFC 6585 §4 — 429 Too Many Requests

Дословно (<https://www.rfc-editor.org/rfc/rfc6585.txt>, §4):

> The 429 status code indicates that the user has sent too many requests in a
> given amount of time ("rate limiting").
>
> The response representations SHOULD include details explaining the condition,
> and MAY include a Retry-After header indicating how long to wait before making
> a new request.
>
> […] Responses with the 429 status code MUST NOT be stored by a cache.

Спецификация сознательно не фиксирует алгоритм: «this specification does not
define how the origin server identifies the user, nor how it counts requests».

### RFC 9110 §10.2.3 — `Retry-After`: для каких кодов он определён

**Проверено по тексту RFC (строки 4799–4824 `rfc9110.txt`): 429 в §10.2.3 не
упоминается вообще.** Дословно:

> Servers send the "Retry-After" header field to indicate how long the user agent
> ought to wait before making a follow-up request. When sent with a 503 (Service
> Unavailable) response, Retry-After indicates how long the service is expected
> to be unavailable to the client. When sent with any 3xx (Redirection) response,
> Retry-After indicates the minimum time that the user agent is asked to wait
> before issuing the redirected request.
>
>     Retry-After = HTTP-date / delay-seconds
>     delay-seconds  = 1*DIGIT

То есть §10.2.3 явно называет только **503** и **любые 3xx**. Право слать
`Retry-After` вместе с 429 идёт из **RFC 6585 §4** («MAY include a Retry-After
header»), а не из RFC 9110. Формат — либо `HTTP-date`, либо целое число секунд
(`Retry-After: 120`).

## Сводная таблица

_в работе_

## Применимость к ratelimit-lab

_в работе_

## Что рассмотрено и отброшено

_в работе_

## Источники

_в работе_
