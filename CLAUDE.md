# CLAUDE.md

Инструкции для AI-сессий (Claude Code) в репозитории `ratelimit-lab`. Эти
инструкции имеют приоритет над поведением по умолчанию — следуй им точно.

## Что это за репозиторий

`ratelimit-lab` — учебная **библиотека rate limiting на Go** плюс бенчмарк-сьют.
Три single-process алгоритма (token bucket, sliding window, leaky bucket) + CAS-
based lock-free token bucket, сравнение throughput/latency под конкурентной
нагрузкой. Соло pet-проект с приоритетом **обучения Go и конкурентности**;
реализация ведётся AI-ассистентом. Не бизнес — экономическая целесообразность,
монетизация и дистрибуция не оцениваются. Бюджет ≈ **$0** (чистая библиотека, без
инфраструктуры и сети). Никакого distributed-варианта в MVP — только single-process.

Полное видение — [`docs/PLAN.md`](docs/PLAN.md); технический план и разбивка по
Этапам — [`docs/TECHNICAL_PLAN.md`](docs/TECHNICAL_PLAN.md); за пределами MVP —
[`docs/POST_MVP_PLAN.md`](docs/POST_MVP_PLAN.md).

## Структура репозитория

| Путь | Содержимое |
|---|---|
| `docs/` | `PLAN.md`, `TECHNICAL_PLAN.md`, `POST_MVP_PLAN.md` |
| `.claude/skills/go-ratelimit-dev/SKILL.md` | Конвенции написания кода именно этого проекта |
| `internal/limiter/limiter.go` | Порт `Limiter` (`Allow`/`AllowN`) + абстракция `Clock` |
| `internal/limiter/tokenbucket.go` | Mutex-based token bucket (baseline, Этап 1) |
| `internal/limiter/slidingwindow.go` | Sliding window (Этап 2) |
| `internal/limiter/leakybucket.go` | Leaky bucket (Этап 3) |
| `internal/limiter/lockfree_tokenbucket.go` | CAS-based lock-free token bucket (Этап 4) |
| `cmd/bench/main.go` | Тонкий CLI-харнесс: нагрузка + сравнительный отчёт (Этап 5) |

`pkg/` намеренно нет — всё ядро в `internal/` (импорт извне запрещён компилятором;
обоснование — `TECHNICAL_PLAN.md §2`). **Docker/Compose нет и не планируется**
(чистая библиотека без сервиса/БД/сети; обоснование — `TECHNICAL_PLAN.md §8`).

## Ключевые архитектурные решения (не переоткрывай без причины)

- **Один порт `Limiter`** (`Allow() bool` / `AllowN(n int) bool`) — за ним все
  алгоритмы; каждый — отдельный файл. Инвариант: **все реализации безопасны для
  конкурентного использования из множества горутин** (SKILL §2).
- **Абстракция `Clock`** — время инжектится, тесты корректности детерминированны
  через fake-clock, без `time.Sleep` (SKILL §3, `TECHNICAL_PLAN.md §4`).
- **Lock-free token bucket — центральная точка проекта**, а не «ещё вариант».
  CAS-цикл на `sync/atomic`; под него — отдельный stress-тест на гонки и
  обязательный `-race` (SKILL §5).
- **Только стандартная библиотека.** Внешние модули не добавляем без веской
  причины — учебная цель в том, чтобы понять примитивы, а не собрать зависимости.
- **Заглушки Этапа 0 помечены `TODO(Этап N)`** — при реализации заменяем
  содержимое файла, не плодим параллельные `*_v2.go`.

## Команды

```bash
export PATH="$HOME/sdk/go/bin:$PATH"   # Go 1.23 в ~/sdk/go/bin
go build ./...
go vet ./...
go test ./...
go test -race ./...                    # ОБЯЗАТЕЛЬНО перед коммитом кода этапов 1+
go test -bench=. ./internal/limiter/   # бенчмарки (появятся в Этапе 5)
go run ./cmd/bench                      # CLI-харнесс (реализация — Этап 5)
```

Перед коммитом кода: `go build ./... && go vet ./... && go test -race ./...`
должны проходить чисто.

## Dev-workflow (обязательный процесс на каждый этап/задачу, с Fable 5)

Главная ветка — **`master`**.

1. **Sonnet 5** (основной чат) — проверка готовности перед началом этапа.
2. **Opus 4.8** — планирование, только если этап требует детального плана
   (отдельный Agent-вызов, `model: opus`). Пишет план, не код.
3. **Fable 5** — программирование по плану (или напрямую, если план не
   потребовался) — отдельный Agent-вызов, `model: claude-fable-5`.
4. **Sonnet 5** (основной чат) — проверка качества покрытия тестами,
   тестирование, проверка работоспособности, проверка покрытия новых функций.
5. **Opus** (Agent-тул, `model: opus`) — независимое ревью: skill `/code-review`
   на diff ветки, фиксируем замечания.
6. **Цикл исправлений — до 3 итераций:** Sonnet правит замечания → тесты снова.
7. **Commit + push + PR** — conventional-commit с русским subject, PR в ветку
   `master`.

Git-workflow: **новая ветка от `master` на каждый Этап → PR → merge**. Коммить/пуш
только когда этап завершён (или пользователь явно попросил). Завершай commit-
сообщения трейлером `Co-Authored-By: Claude`.

## Язык

Документация и subject коммитов — по-русски; код, идентификаторы и комментарии в
коде — по-английски.
