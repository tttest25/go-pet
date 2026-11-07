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

- [ ] **Step 7: Хостовый скрипт `scripts/spool-runner.sh`**
  - Написать скрипт `/opt/go-pet/scripts/spool-runner.sh` (с `jq` и `curl`).
  - Логика: мониторинг `/spool/in/*.json` -> перенос в `/spool/processing/` -> `POST in_progress` -> выполнение инструмента (`echo-test`, `yt-dlp`, `fabric`) с сохранением вывода в `/spool/out/{job_id}/` -> `POST completed/failed` -> удаление файла из `processing/`.
  - Провести сквозной (E2E) тест: вставка записи в `sm_inputs` -> файл в `/spool/in` -> обработка `spool-runner.sh` -> статус `completed` в `sm_jobs` и запись в `sm_outputs`.
  - Сделать git commit.

- [ ] **Step 8: Multi-stage `Dockerfile` и интеграция в `docker-compose.yml`**
  - Создать легковесный Multi-stage `Dockerfile` (`golang:1.23-alpine` -> `alpine:3.20`, непривилегированный пользователь, образ ~20 МБ).
  - Добавить сервис `app` (`petar_app`) и `healthcheck` для `postgres` в `/opt/go-pet/docker-compose.yml` с сохранением всех путей `/opt/go-pet/postgres/`.
  - Запустить весь стек через `docker compose up -d --build` и проверить прохождение E2E-теста.
  - Сделать финальный git commit этапа MVP.
