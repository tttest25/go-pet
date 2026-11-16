# Architecture Manifest: Project petar

## 1. System Overview & Philosophy
petar — легковесный оркестратор фоновых задач и обработки знаний, спроектированный под жесткие лимиты ресурсов (1 vCPU, 1 GB RAM).
* DB-Centric State Machine: Вся бизнес-логика состояний и переходов живет внутри PostgreSQL 18.
* Dumb Executor (Go 1.23+): Go-сервис не принимает бизнес-решений. Он слушает события БД, выгружает задачи в файловый буфер /spool и принимает HTTP-колбэки от хостового раннера.
* Zero Docker-in-Docker Overhead: Тяжелые CLI-утилиты (yt-dlp, fabric) запускаются на хосте скриптом scripts/spool-runner.sh через общую директорию /opt/go-pet/spool/.

## 2. Directory Layout
/opt/go-pet/
├── architecture.md               # Архитектурный манифест
├── rules.md                      # Правила разработки и стандарты кода
├── todo.md                       # Пошаговый план MVP (1 шаг = 1 коммит)
├── .opencode/
│   └── rules.md                  # Системные инструкции для CLI-агента OpenCode
├── cmd/
│   └── petar/
│       └── main.go               # Точка входа, DI, Graceful Shutdown (SIGTERM/SIGINT)
├── internal/
│   ├── config/                   # Загрузка конфигурации из ENV
│   ├── db/                       # Пул pgx/v5, LISTEN/NOTIFY, SQL-запросы к sm_*
│   ├── spool/                    # Атомарная запись задач (.tmp -> .json) и чтение /spool/out
│   └── api/                      # HTTP-сервер (stdlib net/http Go 1.23): колбэки и пробы
├── postgres/                     # СТРОГО вся инфраструктура БД
│   ├── data/                     # Bind mount (/var/lib/postgresql) [в .gitignore]
│   ├── migrations/               # Миграции golang-migrate (000001_..., 000002_...)
│   └── postgresql.conf           # Конфиг PG 18 (io_method = 'worker', лимиты памяти)
├── scripts/
│   └── spool-runner.sh           # Хостовый демон обработки /spool/in -> /spool/processing -> /spool/out
├── spool/
│   ├── in/                       # Входящие JSON-задачи от Go-воркера
│   ├── processing/               # Задачи, взятые в работу bash-скриптом
│   └── out/                      # Артефакты выполнения (тексты, аудио, метаданные)
├── Dockerfile                    # Multi-stage сборка Go-приложения (~15-20 MB)
├── docker-compose.yml            # Оркестрация контейнеров petar_postgres и petar_app
└── go.mod                        # module petar

## 3. PostgreSQL 18 State Machine Schema
Все первичные ключи используют нативную функцию PostgreSQL 18 uuidv7().

1. sm_inputs — входящие сырые события:
   * id UUID PRIMARY KEY DEFAULT uuidv7()
   * source TEXT NOT NULL (например, 'cli', 'manual', 'api')
   * payload JSONB NOT NULL
   * status TEXT NOT NULL DEFAULT 'new' ('new', 'processed', 'error')
   * created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()

2. sm_processes — экземпляры конвейера обработки:
   * id UUID PRIMARY KEY DEFAULT uuidv7()
   * input_id UUID NOT NULL REFERENCES sm_inputs(id) ON DELETE CASCADE
   * state TEXT NOT NULL DEFAULT 'init' ('init', 'running', 'completed', 'failed')
   * context JSONB NOT NULL DEFAULT '{}'::jsonb
   * created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
   * updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()

3. sm_jobs — очередь задач для Dumb Executor:
   * id UUID PRIMARY KEY DEFAULT uuidv7()
   * process_id UUID NOT NULL REFERENCES sm_processes(id) ON DELETE CASCADE
   * tool_name TEXT NOT NULL ('yt-dlp', 'fabric', 'echo-test')
   * payload JSONB NOT NULL DEFAULT '{}'::jsonb
   * status TEXT NOT NULL DEFAULT 'pending' ('pending', 'dispatched', 'in_progress', 'completed', 'failed')
   * priority INT NOT NULL DEFAULT 0
   * result_path TEXT
   * error_msg TEXT
   * created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
   * updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()

4. sm_job_logs — аудит переходов состояний (заполняется триггером с использованием PG 18 OLD / NEW):
   * id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY
   * job_id UUID NOT NULL REFERENCES sm_jobs(id) ON DELETE CASCADE
   * old_status TEXT
   * new_status TEXT NOT NULL
   * message TEXT
   * created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()

5. sm_outputs — готовые выходные артефакты процесса:
   * id UUID PRIMARY KEY DEFAULT uuidv7()
   * process_id UUID NOT NULL REFERENCES sm_processes(id) ON DELETE CASCADE
   * artifact_type TEXT NOT NULL
   * content TEXT
   * metadata JSONB NOT NULL DEFAULT '{}'::jsonb
   * created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()

### Триггеры СУБД:
* trg_sm_inputs_after_insert: при вставке записи в sm_inputs автоматически создает процесс в sm_processes и первую задачу в sm_jobs (согласно payload->>'tool').
* trg_sm_jobs_notify: при появлении задачи в статусе 'pending' вызывает pg_notify('sm_jobs_channel', NEW.id::text).
* trg_sm_jobs_status_audit: при изменении status в sm_jobs записывает переход OLD.status -> NEW.status в sm_job_logs и синхронизирует статус родительского sm_processes.

## 4. Spool Lifecycle & Task Flow
1. Пробуждение (Hybrid Trigger): Go-воркер держит выделенное соединение LISTEN sm_jobs_channel + запускает резервный time.Ticker(45 * time.Second).
2. Захват задачи в БД (PickAndDispatchJob):
   UPDATE sm_jobs
   SET status = 'dispatched', updated_at = NOW()
   WHERE id = (
       SELECT id FROM sm_jobs
       WHERE status = 'pending'
       ORDER BY priority DESC, created_at ASC
       FOR UPDATE SKIP LOCKED
       LIMIT 1
   )
   RETURNING id, process_id, tool_name, payload, OLD.status, NEW.status;
3. Атомарная выгрузка в /spool/in/:
   * Go-воркер сериализует задачу в /spool/in/{job_id}.tmp, вызывает fsync и атомарно переименовывает в /spool/in/{job_id}.json.
4. Хостовая обработка (scripts/spool-runner.sh):
   * Обнаруживает /spool/in/{job_id}.json.
   * Атомарно перемещает (mv) его в /spool/processing/{job_id}.json.
   * Отправляет POST http://127.0.0.1:8080/api/v1/jobs/\{job_id\}/status с телом {"status": "in_progress"}.
   * Запускает утилиту (yt-dlp, fabric, echo-test), складывая результаты в /spool/out/{job_id}/result.json (или текстовые файлы).
   * По завершении отправляет POST http://127.0.0.1:8080/api/v1/jobs/\{job_id\}/status с телом:
     {"status": "completed", "result_path": "/spool/out/{job_id}/result.json"} (или {"status": "failed", "error_msg": "..."}).
   * Удаляет /spool/processing/{job_id}.json.

## 5. HTTP Endpoints (MVP)
Используется стандартный роутер net/http (Go 1.23 patterns: "POST /api/v1/jobs/{id}/status"):
* GET /healthz — Liveness probe (200 OK).
* GET /readyz — Readiness probe (db.Ping, 200 OK или 503 Service Unavailable).
* POST /api/v1/inputs — Ручное/тестовое создание входящего события в sm_inputs.
* POST /api/v1/jobs/{id}/status — Прием статусов (in_progress, completed, failed) от хостового spool-runner.sh.

## 6. Out of Scope for Phase 1 (Moved to Phase 2)
* Интеграция с Gemini API.
* HTTP SSE-стриминг для DokuWiki или javascript polling ? websocket ? 
* Telegram-бот.
