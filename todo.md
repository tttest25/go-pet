# MVP Implementation Checklist (`todo.md`)

Правило для агента: Выполняй строго по одному шагу. Для каждого шага: **Тест -> Реализация -> `go test -v -race ./...` -> `git commit`**.

- [x] **Step 0: Инфраструктура PostgreSQL 18 и структура проекта**
  - Контейнер `petar_postgres` (`pgvector/pgvector:pg18`) запущен.
  - Данные в `/opt/go-pet/postgres/data`, конфиг в `/opt/go-pet/postgres/postgresql.conf`.
  - Миграция `000001_init_extensions` (`vector`, `file_fdw`, `pg_trgm`) применена.

- [x] **Step 1: Миграция State Machine (`000002_create_state_machine`)**
  - Создать `/opt/go-pet/postgres/migrations/000002_create_state_machine.up.sql` и `.down.sql`.
  - Таблицы: `sm_inputs`, `sm_processes`, `sm_jobs`, `sm_job_logs`, `sm_outputs` с `uuidv7()`.
  - Триггеры: авто-создание процесса и первой задачи из `sm_inputs`, аудит статусов в `sm_job_logs`, `pg_notify('sm_jobs_channel', ...)` при `status = 'pending'`.
  - Применить миграцию в `petar_db` и проверить SQL-скриптом создание цепочки `sm_inputs -> sm_processes -> sm_jobs`.
  - Сделать git commit.

- [x] **Step 2: Конфигурация и пул соединений БД (`internal/config`, `internal/db`)**
  - Инициализировать зависимости (`pgx/v5`).
  - Реализовать `internal/config` (чтение `DB_DSN`, `SPOOL_DIR`, `HTTP_ADDR`, `POLL_INTERVAL` из ENV с дефолтами для локальной разработки).
  - Реализовать `internal/db`: подключение `pgxpool`, метод `Ping(ctx)` и интеграционный тест `db_test.go`.
  - Сделать git commit.

- [x] **Step 3: Выборка задач и `LISTEN/NOTIFY` в `internal/db`**
  - Написать метод `CreateInput(ctx, source, payload)` для вставки события в `sm_inputs`.
  - Написать метод `PickNextJob(ctx)` с `FOR UPDATE SKIP LOCKED`, переводящий задачу из `pending` в `dispatched` (с использованием PG 18 `RETURNING`).
  - Написать метод `UpdateJobStatus(ctx, jobID, status, resultPath, errorMsg)` для обновления статуса и записи артефакта в `sm_outputs` при `completed`.
  - Написать `WaitForNotification(ctx)` на базе выделенного коннекта `LISTEN sm_jobs_channel`.
  - Покрыть интеграционными тестами в `internal/db/queue_test.go` (включая проверку конкурентного `SKIP LOCKED`).
  - Сделать git commit.

- [x] **Step 4: Файловый мост Spool (`internal/spool`)**
  - Создать каталог `/opt/go-pet/spool/processing` (с `.gitkeep`).
  - Реализовать пакет `internal/spool`: атомарная запись задачи в `/spool/in/{job_id}.tmp` с `Sync()` и переименованием в `/spool/in/{job_id}.json`.
  - Реализовать фоновый цикл `Worker.Run(ctx)`: гибрид `LISTEN sm_jobs_channel` + fallback ticker (45s) -> `PickNextJob` -> `Spool.WriteJob`.
  - Написать unit/integration тесты `internal/spool/spool_test.go`.
  - Сделать git commit.

- [x] **Step 5: HTTP Callback API и Health Probes (`internal/api`)**
  - Реализовать HTTP-сервер на стандартном `net/http` (Go 1.23):
    - `GET /healthz` (200 OK)
    - `GET /readyz` (проверка `db.Ping`)
    - `POST /api/v1/inputs` (регистрация входного задания)
    - `POST /api/v1/jobs/{id}/status` (колбэк от хостового скрипта: `in_progress`, `completed`, `failed`).
  - Покрыть тестами через `httptest` в `internal/api/server_test.go`.
  - Сделать git commit.

- [x] **Step 6: Точка входа `cmd/petar/main.go` и Graceful Shutdown**
  - Собрать все компоненты в `cmd/petar/main.go`.
  - Настроить перехват `SIGINT` / `SIGTERM` (`signal.NotifyContext`), корректную остановку HTTP-сервера и Spool-воркера.
  - Проверить сборку `go build ./cmd/petar`.
  - Сделать git commit.

- [x] **Step 7: Хостовый скрипт `scripts/spool-runner.sh`**
  - Написать скрипт `/opt/go-pet/scripts/spool-runner.sh` (с `jq` и `curl`).
  - Логика: мониторинг `/spool/in/*.json` -> перенос в `/spool/processing/` -> `POST in_progress` -> выполнение инструмента (`echo-test`, `yt-dlp`, `fabric`) с сохранением вывода в `/spool/out/{job_id}/` -> `POST completed/failed` -> удаление файла из `processing/`.
  - Провести сквозной (E2E) тест: вставка записи в `sm_inputs` -> файл в `/spool/in` -> обработка `spool-runner.sh` -> статус `completed` в `sm_jobs` и запись в `sm_outputs`.
  - Сделать git commit.

- [x] **Step 8: Сквозное логирование, Request ID и фиксация IP-адресов**
  - Добавить HTTP Middleware для генерации / чтения сквозного идентификатора (`X-Request-ID` / UUID) и логирования IP-адреса клиента.
  - Интегрировать `request_id` и `client_ip` в структурированные JSON-логи (`log/slog`) для всех запросов к HTTP API (`/api/v1/inputs` и `/api/v1/jobs/{id}/status`).
  - Опционально расширить схему базы данных (`sm_inputs` / логи) для сохранения `request_id` и `client_ip` при создании задач.
  - Покрыть изменения юнит/интеграционными тестами и проверить сквозную прослеживаемость в логах.
  - Сделать git commit.

- [x] **Step 9: Multi-stage `Dockerfile` и интеграция в `docker-compose.yml`**
  - Создать легковесный Multi-stage `Dockerfile` (`golang:1.23-alpine` -> `alpine:3.20`, непривилегированный пользователь, образ ~20 МБ).
  - Добавить сервис `app` (`petar_app`) и `healthcheck` для `postgres` в `/opt/go-pet/docker-compose.yml` с сохранением всех путей `/opt/go-pet/postgres/`.
  - Запустить весь стек через `docker compose up -d --build` и проверить прохождение E2E-теста.
  - Сделать финальный git commit этапа MVP.
- [x] **Step 10: Расширение ответа `POST /api/v1/inputs` до контракта интеграции с DokuWiki**
  - Контекст: BFF-плагин `petar` для DokuWiki (репозиторий `php-wiki-plugin`) валидирует ответ по контракту: `201 {"input_id":"<uuid>","process_id":"<uuid>","status":"new"}` (см. его `docs/architecture.md` §4.5). Фактический ответ `201 {"id":"<uuid>"}` отклоняется прокси с `502 Incomplete backend response structure` — интеграция не работает.
  - `internal/db/queue.go`: расширить `createInputQuery` до `RETURNING id, status`; добавить запрос выборки процесса, авто-созданного триггером: `SELECT id FROM sm_processes WHERE input_id = $1 ORDER BY created_at ASC LIMIT 1`; изменить сигнатуру `CreateInput` — возвращать структуру (например `*CreatedInput{InputID, ProcessID, Status uuid.UUID/uuid.UUID/Status`); процесс виден в той же сессии сразу после INSERT (AFTER INSERT trigger).
  - `internal/api/server.go`: обновить интерфейс `databaseAPI` и `createInputResponse` — отдавать `input_id`, `process_id`, `status`; легаси-поле `id` (равное `input_id`) сохранить для совместимости. `status` брать из статуса записи `sm_inputs` (после вставки — `'new'`).
  - Адаптировать тесты и фейки `databaseAPI` (`internal/api/server_test.go`, `internal/db/queue_test.go`): новые ожидания пишутся первыми и должны падать (red), затем реализация (green); проверить валидность UUID `input_id`/`process_id` и `status == "new"`.
  - Проверка: `go test -v -race ./...`, `go build ./cmd/petar`, пересборка и рестарт контейнера `petar_app`, probe-команда возвращает три обязательных поля, отправка формы из DokuWiki (пользователь @ai/@admin) показывает Input ID / Process ID и пишет `data-input-id`/`data-process-id` в DOM.
  - Сделать git commit: `fix: return input_id/process_id/status from POST /api/v1/inputs`



---

## Phase 2: PostgreSQL State Machine Functions & Query API

- [x] **Step 11: Миграция хранимых функций БД (`000003_db_state_functions`)**
  - Создать `/opt/go-pet/postgres/migrations/000003_db_state_functions.up.sql` и `.down.sql`.
  - Реализовать PL/pgSQL-функции для инкапсуляции мутаций:
    - `sm_create_input(p_source TEXT, p_payload JSONB)` — вставка в `sm_inputs` и возврат `(input_id UUID, process_id UUID, status TEXT)`.
    - `sm_pick_next_job()` — атомарный захват `pending`-задачи (`FOR UPDATE SKIP LOCKED`), перевод в `dispatched` с `RETURNING OLD.status, NEW.status` и возврат полей задачи.
    - `sm_update_job_status(p_job_id UUID, p_status TEXT, p_result_path TEXT, p_error_msg TEXT)` — валидация перехода (`dispatched`/`in_progress` -> `in_progress`/`completed`/`failed`), обновление `sm_jobs` и запись в `sm_outputs` при `completed`.
  - Проверить идемпотентность миграции и работу функций SQL-тестом в `petar_postgres`.
  - Сделать git commit: `feat(db): add PostgreSQL 18 state machine functions`

- [x] **Step 12: Перевод `internal/db` на вызов хранимых функций**
  - Обновить `internal/db/queue.go`: заменить сырые SQL-транзакции в `CreateInput`, `PickNextJob` и `UpdateJobStatus` на вызовы `SELECT * FROM sm_create_input(...)`, `SELECT * FROM sm_pick_next_job()`, `SELECT sm_update_job_status(...)`.
  - Сохранить контракты возвращаемых ошибок (включая маппинг отсутствующей/необновляемой задачи в `pgx.ErrNoRows`).
  - Прогнать существующие интеграционные тесты `internal/db/queue_test.go` и `internal/spool/spool_test.go` через `./scripts/quality-gate.sh` без изменения их логики.
  - Сделать git commit: `refactor(db): delegate queue mutations to PostgreSQL functions`

- [x] **Step 13: Слой чтения статуса процесса и списка задач в `internal/db`**
  - Добавить в `internal/db/models.go` структуры `ProcessDetails` (данные `sm_processes` + вложенный список его `sm_jobs` и `sm_outputs`) и `JobFilter` (`ProcessID`, `Status`, `Limit`, `CursorID`).
  - Реализовать в `internal/db/queue.go` методы `GetProcess(ctx, processID)` (возврат `pgx.ErrNoRows`, если процесс не найден) и `ListJobs(ctx, filter)` (сортировка `ORDER BY created_at DESC, id DESC` с keyset-пагинацией по `uuidv7()`).
  - Покрыть интеграционными тестами в `internal/db/queue_test.go`.
  - Сделать git commit: `feat(db): add GetProcess and ListJobs queries`

- [x] **Step 14: HTTP API для статуса процесса и списка задач (`internal/api`)**
  - Расширить интерфейс `databaseAPI` в `internal/api/server.go` методами `GetProcess` и `ListJobs`.
  - Добавить эндпоинты (Go 1.23 `http.ServeMux`):
    - `GET /api/v1/processes/{id}` — возврат `200 JSON` со статусом процесса, задачами и выходными артефактами (`400` на невалидный UUID, `404` если не найден).
    - `GET /api/v1/jobs` — фильтрация по query-параметрам `?process_id=...&status=...&limit=...&cursor=...`.
  - Покрыть тестами в `internal/api/server_test.go` (валидация, 404, сериализация JSON) и прогнать `./scripts/quality-gate.sh`.
  - Сделать git commit: `feat(api): add process status and jobs list endpoints`


---

## Phase 3: Documentation, Operations Runbook & Deployment Guide

- [x] **T19: Ограничить ожидание HTTP и БД.** HTTP header/read/write/idle: 5/15/20/60s; вызов БД из HTTP — до 5s (API 504, readiness 503). Приёмка: медленное тело, дедлайн БД, обычный запрос, shutdown с активным запросом, сборка и race/gate. План: `.opencode/tasks/19_http_db_timeouts.md`.

- [x] **T16: Безопасный тестовый lifecycle и quality gate**
  - Единый `PETAR_TEST_DB_DSN`, изолированная `petar_test`, явные prepare/check/reset.
  - Общий lock lifecycle/gate/прямых тестов; без остановки app на другой БД.
  - Strict TDD, два gate, прямой race-прогон и независимый review PASS; Human Sign-off получен.
  - Подробный план: `.opencode/tasks/16_test_lifecycle_implementation_plan.md`.

- [x] **Step 15: Актуализация `architecture.md` и `README.md` (схемы, развертывание, эксплуатация, тесты)**
  - Обновить `architecture.md`:
    - Привести описание в соответствие с реализацией Phase 1–2 (хранимые функции PG 18 `sm_create_input`, `sm_pick_next_job`, `sm_update_job_status`, HTTP API чтения `GET /api/v1/processes/{id}` и `GET /api/v1/jobs`, сквозной `X-Request-ID`).
    - Добавить четкие ASCII/Unicode-диаграммы: общая топология компонентов (DokuWiki BFF -> Traefik -> `petar_app` -> PostgreSQL 18 <-> Spool <-> `spool-runner.sh`), жизненный цикл состояний (`sm_inputs` / `sm_processes` / `sm_jobs` / `sm_outputs`) и последовательность атомарного обмена через файловый мост `/spool`.
  - Обновить `README.md` с добавлением практических разделов:
    - **Развертывание (`Deployment`):** запуск стека через `docker compose up -d --build`, конфигурация переменных окружения, готовый `systemd`-юнит (`petar-spool-runner.service`) для хостового `scripts/spool-runner.sh` и конфигурация маршрутизации за reverse proxy (Traefik / Nginx) для интеграции с DokuWiki.
    - **Эксплуатация (`Operations Runbook`):** рабочие примеры `curl` (включая `echo-test` и реальный пайплайн `yt-dlp` + `fabric`), просмотр аудита переходов в `sm_job_logs`, ручное восстановление зависших задач (`dispatched` / `in_progress`), бэкап и восстановление `petar_db` (`pg_dump` / `pg_restore`).
    - **Тестирование (`Testing & Quality Gate`):** описание `./scripts/quality-gate.sh`, запуск unit/integration-тестов (`go test -v -race ./...`), проверка обратимости миграций (`up -> down -> up`), проверка конкурентного захвата `FOR UPDATE SKIP LOCKED` и полный ручной E2E-сценарий.
  - Строго соблюсти протокол Gate -> Cross-Review (`@review`) -> Human Sign-off перед коммитом.
  - Сделать git commit: `docs: update architecture and add operations, deployment, and testing guides`

- [x] **T17: Проверять payload в PostgreSQL до постановки задания в очередь.** Миграция `000004` вводит проверку в `sm_create_input` для `echo-test`, `yt-dlp`, `fabric`; отсутствие `message` сохраняет `default echo`. Неверный payload → SQLSTATE `22023` → HTTP 400, без создания input/process/job. Приёмка: SQL/HTTP regression, обратимость `up → down → up`, quality gate. План: `.opencode/tasks/17_todo_entry.md`.

- [x] **T18: Сохранить опрос очереди при потере LISTEN.** Worker журналирует потерю подписки и продолжает публиковать pending-задания по `POLL_INTERVAL`. Попытки reconnect идут по одной, с паузой 1 с после ошибки. Приёмка: один ERROR на эпизод потери, файл опубликован до восстановления, после него работают уведомления; штатная отмена без ложной тревоги; race-тесты и quality gate.

- [x] **T20: Исключить ложный успех хостового runner.** Нет CLI, nonzero или timeout → `failed`; только HTTP 204 подтверждает callback, иначе `processing` сохраняется для сверки БД. Приёмка: Go harness проверяет argv/stdin, артефакты, ошибки callback, UUID и flock; race и quality gate. План: `.opencode/tasks/20_spool_runner_hardening.md`.

- [x] **T21: Воспроизводимый CI и ручное восстановление MVP.** Чистый PG18 проходит prepare/check, vet, сериализованный race без cache и сборку; отдельно проверены fresh bootstrap, dispatch без файла и потеря ответа после commit callback. План: `.opencode/tasks/21_reproducible_ci.md`.

---

## Каталог действий и универсальное выполнение — согласованная дорожная карта

Пункты ниже — отдельные шаги, не разрешение выполнить их одним коммитом.
Перед реализацией каждого шага уточнить атомарный TDD-план; независимый review
и Human Sign-off обязательны перед коммитом.

- [x] **T22: Каталог действий в PostgreSQL.** Хранить идентификатор, версию, название и признак доступности; начальные действия — `echo-test`, `yt-dlp`, `fabric` версии 1. Проверять каталог в `sm_create_input` до вставки. Приёмка: корректный ввод создаёт одну цепочку; неизвестное/отключённое действие → `22023`, без input/process/job; legacy-контракт сохранён, миграция обратима на одноразовой БД. План: [.opencode/tasks/go_pet_actions_roadmap.md](.opencode/tasks/go_pet_actions_roadmap.md).

- [ ] **T23: API чтения каталога.** `GET /api/v1/actions` читает доступные действия и версии из БД, без отдельного списка Go. Приёмка: новая запись появляется в API, отключённая исчезает из выбора, история заданий сохраняется. Зависит от T22. План: [.opencode/tasks/go_pet_actions_roadmap.md](.opencode/tasks/go_pet_actions_roadmap.md).

- [ ] **T24: Универсальный JSON-вход.** Go проверяет лимит, один JSON-документ и оболочку `action/version/data`, PostgreSQL — правила действия; `data` передаётся без повторного кодирования, отсутствие отличается от `null`. Приёмка: неверный JSON → 400 без вызова БД; отказ БД → 400 без новых строк; успех → 201; прежний `POST /api/v1/inputs` и успешный ответ совместимы. Зависит от T22, не требует T23 для прямых API-тестов. План: [.opencode/tasks/go_pet_actions_roadmap.md](.opencode/tasks/go_pet_actions_roadmap.md).

- [ ] **T25: Контракт обработчика и публичного результата.** Runner выбирает установленный обработчик по действию и версии, выдаёт публичный JSON с типом и версией, сохраняя совместимость старых заданий. Приёмка: echo возвращает ожидаемые данные; недоступный обработчик/невалидный вывод → failed, не completed; пути и stderr не являются публичным результатом, произвольные команды не запускаются. Передачу версии согласовать с T24. План: [.opencode/tasks/go_pet_actions_roadmap.md](.opencode/tasks/go_pet_actions_roadmap.md).

- [ ] **T26: Чтение публичного результата по job_id.** Различать неготовое, успешное, ошибочное и отсутствующее задание; ограничить размер ответа. Приёмка: для двух jobs одного процесса возвращается только запрошенный результат, без внутренних путей/логов. До реализации закрепить HTTP-статусы, привязку job → результат и поведение при превышении размера. Зависит от T25. План: [.opencode/tasks/go_pet_actions_roadmap.md](.opencode/tasks/go_pet_actions_roadmap.md).

- [ ] **T27: Доказательство расширяемости на word-count.** Зарегистрировать действие, валидатор и обработчик. Приёмка: `{"text":"один два три"}` → `{"words":3}` без изменения HTTP-обработчиков и кода wiki под это действие. Backend-проверка после T22–T26; сквозная wiki-проверка после PHP-06/07. План: [.opencode/tasks/go_pet_actions_roadmap.md](.opencode/tasks/go_pet_actions_roadmap.md).
