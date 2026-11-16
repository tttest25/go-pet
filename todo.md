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

- [ ] **Step 13: Слой чтения статуса процесса и списка задач в `internal/db`**
  - Добавить в `internal/db/models.go` структуры `ProcessDetails` (данные `sm_processes` + вложенный список его `sm_jobs` и `sm_outputs`) и `JobFilter` (`ProcessID`, `Status`, `Limit`, `CursorID`).
  - Реализовать в `internal/db/queue.go` методы `GetProcess(ctx, processID)` (возврат `pgx.ErrNoRows`, если процесс не найден) и `ListJobs(ctx, filter)` (сортировка `ORDER BY created_at DESC, id DESC` с keyset-пагинацией по `uuidv7()`).
  - Покрыть интеграционными тестами в `internal/db/queue_test.go`.
  - Сделать git commit: `feat(db): add GetProcess and ListJobs queries`

- [ ] **Step 14: HTTP API для статуса процесса и списка задач (`internal/api`)**
  - Расширить интерфейс `databaseAPI` в `internal/api/server.go` методами `GetProcess` и `ListJobs`.
  - Добавить эндпоинты (Go 1.23 `http.ServeMux`):
    - `GET /api/v1/processes/{id}` — возврат `200 JSON` со статусом процесса, задачами и выходными артефактами (`400` на невалидный UUID, `404` если не найден).
    - `GET /api/v1/jobs` — фильтрация по query-параметрам `?process_id=...&status=...&limit=...&cursor=...`.
  - Покрыть тестами в `internal/api/server_test.go` (валидация, 404, сериализация JSON) и прогнать `./scripts/quality-gate.sh`.
  - Сделать git commit: `feat(api): add process status and jobs list endpoints`
