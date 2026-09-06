# B4 — облака, SaaS и публичные API

> Статус: **черновик, в работе.** Файл наполняется по ходу исследования.
> Каждое утверждение сопровождается ссылкой на официальную документацию или
> первоисточник. Где проверить не удалось — стоит пометка «**не проверено**».
> Даты обращения указаны у разделов: документация облаков меняется.

Тема: какие алгоритмы и **какая наблюдаемая семантика** заявлены у облачных
gateway/WAF и у публичных API — что именно возвращается вызывающему за
пределами «пустили / не пустили».

## AWS

_в работе_

## Google Cloud (Apigee, Cloud Armor)

_в работе_

## Cloudflare

_в работе_

## Azure API Management

_в работе_

## GitHub

_в работе_

## Stripe

_в работе_

## Shopify

_в работе_

## Slack

_в работе_

## Discord

_в работе_

## Twilio

_в работе_

## Atlassian

_в работе_

## OpenAI

_в работе_

## Anthropic

_в работе_

## Прочее (Upstash, Unkey, Zuplo)

_в работе_

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
