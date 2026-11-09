# Petar (Оркестратор фоновых задач и обработки знаний)

**petar** — легковесный, надежный оркестратор фоновых задач и конвейеров обработки данных, спроектированный под жесткие лимиты ресурсов (1 vCPU, 1 GB RAM).

Архитектура построена по принципу **DB-Centric State Machine** (машина состояний внутри PostgreSQL 18) в сочетании с концепцией **Dumb Executor** (Go-сервис и bash-скрипты на хосте выполняют роль легковесных транспортов без тяжелого Docker-in-Docker оверхеда).

---

## 🏗 Архитектура проекта

### Ключевые компоненты
1. **PostgreSQL 18 (`pgvector/pgvector:pg18`)**:
   - Ядро системы и источник правды. Вся логика переходов состояний реализована через таблицы `sm_inputs`, `sm_processes`, `sm_jobs`, `sm_job_logs`, `sm_outputs`, а также триггеры СУБД.
   - Механизм `LISTEN/NOTIFY` (`смикшером` событий `sm_jobs_channel`) мгновенно уведомляет Go-воркер о появлении новых задач.
2. **Go-сервис (`cmd/petar`, `internal/...`)**:
   - **DB / Queue Worker**: Слушает `LISTEN/NOTIFY` (с fallback на `time.Ticker`), захватывает задачи с помощью `SELECT ... FOR UPDATE SKIP LOCKED` и атомарно выгружает их в файловый буфер `/spool/in/`.
   - **HTTP API Server**: Стандартный `net/http` (Go 1.23+ routing). Предоставляет пробы `/healthz`, `/readyz`, эндпоинт создания инпутов `/api/v1/inputs`, а также колбэки обновления статусов задач `/api/v1/jobs/{id}/status` от хостового ранера.
   - **Middleware**: Автоматическое добавление/сохранение `X-Request-ID`, извлечение реального IP клиента (`X-Forwarded-For`, `X-Real-IP`, `RemoteAddr`) и структурированное логирование через `log/slog`.
3. **Хостовый Spool Runner (`scripts/spool-runner.sh`)**:
   - Демон, обрабатывающий файлы в разделяемой директории `/spool/` (`/spool/in` -> `/spool/processing` -> `/spool/out`), вызывающий утилиты (`yt-dlp`, `fabric`, `echo-test`) и отправляющий HTTP-колбэки в Go-сервис.

---

## 📂 Структура директорий

```text
/opt/go-pet/
├── architecture.md               # Архитектурный манифест
├── rules.md                      # Правила разработки и стандарты кода
├── todo.md                       # Пошаговый план MVP
├── cmd/
│   └── petar/
│       └── main.go               # Точка входа, DI, Graceful Shutdown
├── internal/
│   ├── config/                   # Конфигурация из ENV
│   ├── db/                       # Пул pgx/v5, LISTEN/NOTIFY, SQL-запросы
│   ├── spool/                    # Атомарная запись в /spool/in и логика воркера
│   └── api/                      # HTTP-сервер, роутинг, middleware, хендлеры
├── postgres/
│   ├── data/                     # Bind mount БД (в .gitignore)
│   ├── migrations/               # Миграции базы данных
│   └── postgresql.conf           # Конфигурация PostgreSQL 18
├── scripts/
│   └── spool-runner.sh           # Хостовый скрипт обработки задач из spool
├── spool/
│   ├── in/                       # Входящие задачи от воркера Go
│   ├── processing/               # Задачи в работе у bash-скрипта
│   └── out/                      # Артефакты выполнения
├── Dockerfile                    # Multi-stage сборка Go-приложения (Alpine 3.20)
└── docker-compose.yml            # Оркестрация контейнеров petar_postgres и petar_app
```

---

## 🔄 Текстовая схема взаимодействия (Data & Task Flow)

```text
 [Клиент / API] 
       │
       ▼  POST /api/v1/inputs
┌──────────────────────────────────────┐
│ PostgreSQL 18 (sm_inputs / sm_jobs)  │
│  ├─► Trigger: создает процесс и задачу │
│  └─► Trigger: pg_notify('sm_jobs')   │
└──────────────────┬───────────────────┘
                   │
                   │ (NOTIFY / Ticker)
                   ▼
┌──────────────────────────────────────┐
│ Go Worker (internal/spool & internal/db)
│  ├─► SELECT ... FOR UPDATE SKIP LOCKED
│  └─► Атомарная запись /spool/in/{id}.json
└──────────────────┬───────────────────┘
                   │
                   ▼
       Shared Volume (/spool/)
                   │
                   ▼
┌──────────────────────────────────────┐
│ Host Spool Runner (spool-runner.sh)  │
│  ├─► Сдвигает в /spool/processing/   │
│  ├─► POST /api/v1/jobs/{id}/status   │  (in_progress)
│  ├─► Запускает утилиту (yt-dlp/etc.) │
│  └─► POST /api/v1/jobs/{id}/status   │  (completed / failed)
└──────────────────────────────────────┘
```

---

## 🚀 Инструкция по сборке и запуск

### Требования
- Docker & Docker Compose (v2+)
- Go 1.23+ (для локальной разработки и тестирования)

### Запуск полного стека через Docker Compose
Для сборки и запуска всей системы (PostgreSQL 18 + Go приложение с healthcheck и пробросом volumes) выполните:

```bash
docker compose up -d --build
```

### Проверка работоспособности
1. Проверка liveness пробы:
   ```bash
   curl -i http://127.0.0.1:8080/healthz
   ```
2. Проверка готовности (подключение к БД):
   ```bash
   curl -i http://127.0.0.1:8080/readyz
   ```

### Локальное тестирование
Для запуска всех тестов с детектером гонок (`-race`):
```bash
go test -v -race ./...
```
