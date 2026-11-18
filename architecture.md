# Архитектура petar

## 1. Назначение и принципы

petar — лёгкий оркестратор фоновых задач и обработки знаний с целевым бюджетом
1 vCPU / 1 GB RAM. Это ориентир развёртывания, не гарантия потребления внешними CLI.
Документ описывает реализованные Phase 1–2, включая HTTP API чтения Step 14.
Команды развёртывания, эксплуатации, восстановления и проверок — в [README](README.md).

- **DB-centric state machine:** PostgreSQL 18 хранит очередь, состояния, аудит и
  метаданные результатов; мутации Go делегирует трём хранимым функциям.
- **Dumb Executor:** Go 1.23+ обслуживает HTTP, слушает уведомления и публикует задания
  в файловом мосте. Он не строит DAG и не принимает решений о следующем инструменте.
- **Без Docker-in-Docker:** хостовый Bash runner запускает `echo-test`, `yt-dlp`, `fabric`.
  Установку и конфигурацию внешних CLI обеспечивает оператор.

## 2. Компоненты и границы

```text
Внешняя интеграция (отсутствует в текущем Compose)
[Клиент] --> [DokuWiki BFF] --> [Traefik; альтернатива: Nginx]
                                         |
                +------------------------+
                | HTTP /api/v1, без StripPrefix
                v
Docker: petar_shared_net (ключ Compose: petar_net)
+--------------------------------+       +--------------------------------+
| petar_app (service: app)        | SQL   | petar_postgres                 |
| HTTP API + Go worker / writer  | <---->| service: postgres, PG 18       |
| /app/spool                     |       | очередь, функции, NOTIFY       |
+---------------+----------------+       +--------------------------------+
                | запись                         (не выполняет файловый I/O)
                v
Хост: общий spool /opt/go-pet/spool/
[in/*.json] ---- mv ----> [processing/*.json]      [out/<job_id>/]
        |                         ^                     ^
        v                         |                     |
[scripts/spool-runner.sh на хосте] +---------------------+
        |                           выполнение CLI, запись out
        |
        +---- callback HTTP --> petar_app через 127.0.0.1:8080
```

DokuWiki BFF — серверный клиент API, а не прямой доступ браузера к petar.
Настройки внешнего плагина здесь не определяются: его исходники не входят в проверенные
источники. Traefik/Nginx, TLS и контроль доступа устанавливаются отдельно; текущий Compose
содержит только `postgres` и `app`. Встроенных auth/CORS middleware и проверки trusted
proxies нет. Примеры proxy и systemd приведены в README.

### Адреса и общий файловый мост

| Объект | Фактическое значение |
| --- | --- |
| PostgreSQL | `pgvector/pgvector:pg18`, контейнер `petar_postgres` |
| Роль / БД / пароль Compose | `petar_user` / `petar_db` / `petar_password` |
| PostgreSQL на хосте | `127.0.0.1:5432` |
| HTTP на хосте | `127.0.0.1:8080` |
| `DB_DSN` в Compose | `postgres://petar_user:petar_password@postgres:5432/petar_db?sslmode=disable` |
| Локальный `DB_DSN` | `postgres://petar_user:petar_password@127.0.0.1:5432/petar_db?sslmode=disable` |
| Spool на хосте | `/opt/go-pet/spool/{in,processing,out}` |
| Тот же spool в app | `/app/spool/{in,processing,out}` |
| Тот же spool в postgres | `/spool/{in,processing,out}` |
| Данные PostgreSQL | `/opt/go-pet/postgres/data` → `/var/lib/postgresql` |
| Runner callback base | `http://127.0.0.1:8080/api/v1` |

`result_path="/spool/out/<job_id>/result.json"` — значение, которое отправляет runner.
На хосте это `/opt/go-pet/spool/out/<job_id>/result.json`; это **не URL и не путь внутри
app**. Go не читает содержимое `out`, а API возвращает метаданные, не скачивает файлы.
Наличие mount в postgres не делает БД исполнителем файловых операций.

Go читает `DB_DSN`, `SPOOL_DIR`, `HTTP_ADDR`, `POLL_INTERVAL`. Локально spool —
`/opt/go-pet/spool`, HTTP — `127.0.0.1:8080`; в Compose они переопределены на `/app/spool`
и `0.0.0.0:8080`. `POLL_INTERVAL` по умолчанию `45s`. Compose задаёт буквальные ENV:
экспорт переменной на хосте сам по себе их не изменяет. App работает с UID 1000,
задания создаются с правами `0600`; доступ хостового runner должен быть согласован.
PostgreSQL ограничен `mem_limit: 450m`, `max_connections=20`, `io_method='worker'`.

## 3. Актуальные каталоги

```text
/opt/go-pet/
├── architecture.md              # Компоненты и контракты
├── README.md                    # Deployment, Operations Runbook, Testing
├── rules.md                     # Правила разработки
├── todo.md                      # Этапы проекта и согласование
├── .opencode/                   # Настройки и задания агента
├── cmd/petar/main.go             # Сборка компонентов, SIGINT/SIGTERM shutdown
├── internal/
│   ├── config/                  # ENV
│   ├── db/                      # pgxpool, функции БД, LISTEN, запросы чтения
│   ├── spool/                   # Go worker и атомарный writer (не reader out)
│   └── api/                     # net/http ServeMux, JSON, logging middleware
├── postgres/                    # Вся инфраструктура и SQL БД
│   ├── data/                    # Bind mount данных, исключён из Git
│   ├── migrations/              # Пары up/down: 000001–000004
│   └── postgresql.conf          # PostgreSQL 18, лимиты, io_method
├── scripts/
│   ├── spool-runner.sh          # Хостовый исполнитель
│   └── quality-gate.sh          # Проверки с общей интеграционной БД
├── spool/
│   ├── in/                      # Публикуемые Go JSON-задания
│   ├── processing/              # Задания, перемещённые runner
│   └── out/                     # result.json и, для fabric, output.md
├── Dockerfile                   # Go builder, Alpine runtime, UID 1000
├── docker-compose.yml           # postgres + app, build.network: host
├── go.mod                       # module petar
└── go.sum
```

## 4. Модель данных и функции

```text
[sm_inputs]
     |
     | sm_processes.input_id -> sm_inputs.id
     v
[sm_processes] ---- sm_outputs.process_id -> sm_processes.id ----> [sm_outputs]
     |
     | sm_jobs.process_id -> sm_processes.id
     v
[sm_jobs] --------- sm_job_logs.job_id -> sm_jobs.id ------------> [sm_job_logs]

Стрелки показывают родитель -> дочерние записи.
Все перечисленные FK имеют ON DELETE CASCADE.
```

UUID PK таблиц используют нативную `uuidv7()` PostgreSQL 18. Исключение:
`sm_job_logs.id` — `BIGINT GENERATED ALWAYS AS IDENTITY`.

| Таблица | Данные и ограничения |
| --- | --- |
| `sm_inputs` | `id,source,payload,status,created_at`; JSONB payload; статус `new/processed/error`, default `new` |
| `sm_processes` | `id,input_id,state,context,created_at,updated_at`; JSONB context default `{}`, state default `init` |
| `sm_jobs` | `id,process_id,tool_name,payload,status,priority,result_path,error_msg,created_at,updated_at`; payload default `{}`, priority default 0 |
| `sm_job_logs` | `id,job_id,old_status,new_status,message,created_at`; message берётся из `NEW.error_msg` |
| `sm_outputs` | `id,process_id,artifact_type,content,metadata,created_at`; metadata JSONB default `{}`, content nullable |

`tool_name` ограничен `echo-test/yt-dlp/fabric`; статусы задач и процессов показаны ниже.
`result_path` и `error_msg` nullable. Время хранится как `TIMESTAMPTZ`, по умолчанию `NOW()`.
Текущий workflow **не переводит input из `new` автоматически**.

### Три функции мутаций

| Функция | Контракт |
| --- | --- |
| `sm_create_input(p_source,p_payload)` | Вызывает `sm_validate_tool_payload` до вставки, затем создаёт input; возвращает `input_id,process_id,status` |
| `sm_pick_next_job()` | Атомарно выбирает pending по `priority DESC,created_at ASC`, `FOR UPDATE SKIP LOCKED`, переводит в dispatched; возвращает `id,process_id,tool_name,payload,old_status,status` |
| `sm_update_job_status(p_job_id,p_status,p_result_path,p_error_msg)` | Принимает только исходные `dispatched/in_progress` и целевые `in_progress/completed/failed`; возвращает VOID; пустые строки путей/ошибок превращает в NULL |

`trg_sm_inputs_after_insert` создаёт **один процесс и одну задачу** из `payload->>'tool'`
в той же транзакции; ошибка откатывает всю цепочку.
Валидация payload — контракт `sm_create_input`, а не новое ограничение триггера
или таблицы. Прямой INSERT сохраняет прежнее поведение. Миграция `000004`
не меняет ранее созданные задания; её down восстанавливает прежнее тело функции.
`trg_sm_jobs_notify` вызывает `pg_notify('sm_jobs_channel', NEW.id::text)` при INSERT pending
или изменении статуса на pending. Уведомление доставляется после COMMIT.
`trg_sm_jobs_status_audit` срабатывает только при фактическом изменении статуса,
пишет OLD/NEW и синхронизирует процесс. Аудита исходного INSERT pending нет.

При completed именно `sm_update_job_status` вставляет `sm_outputs`:
`artifact_type='result'`, `content=NULL`, `metadata={"result_path":"..."}`.
Прямой UPDATE в completed не эквивалентен функции: output он не создаёт.
Триггер процесса отражает статус **изменившейся задачи**, не агрегирует несколько задач/DAG.

## 5. Состояния и файловый протокол

### Переходы задания

```text
pending ---- sm_pick_next_job ----> dispatched
                                       |
                +----------------------+----------------------+
                |                      |                      |
                v                      v                      v
           in_progress             completed                failed
                |                      ^                      ^
                +-- in_progress        |                      |
                +----------------------+----------------------+

Callback: dispatched или in_progress -> in_progress / completed / failed.
Повторный in_progress допустим; без изменения статуса аудит не добавляется.
completed и failed терминальны для callbacks; повторный callback даёт 404.

Административно (не callback): dispatched / in_progress -> pending.
Это адресное восстановление оператором при остановленных исполнителях.
```

```text
sm_jobs.status                sm_processes.state
--------------------------    ------------------
pending                   --> init
dispatched / in_progress  --> running
completed                 --> completed
failed                    --> failed
```

### Последовательность выполнения

```text
Время  Отправитель        Действие / получатель
-----  ----------------  --------------------------------------------------------------
  01   Клиент       -->  API: POST /api/v1/inputs
  02   API          -->  PostgreSQL: sm_create_input
  03   PostgreSQL   -->  trigger: sm_inputs -> sm_processes -> sm_jobs
  04   PostgreSQL   -->  COMMIT; NOTIFY sm_jobs_channel -> Go worker
  05   API          -->  Клиент: 201 (не ждёт выполнения CLI)
  06   Go worker    -->  PostgreSQL: sm_pick_next_job -> dispatched
  07   Go writer    -->  spool/in/<id>.tmp: write -> Sync -> Close
  08   Go writer    -->  Rename: spool/in/<id>.json
  09   Runner       -->  mv: in/<id>.json -> processing/<id>.json
  10   Runner       -->  API: callback in_progress -> sm_update_job_status -> 204
  11   Runner       -->  CLI: выполнение -> spool/out/<id>/result.json
  12   Runner       -->  API: callback completed / failed
  13   API          -->  PostgreSQL: sm_update_job_status
  14   PostgreSQL   -->  при completed: INSERT sm_outputs; COMMIT
  15   API          -->  Runner: 204
  16   Runner       -->  удаление processing/<id>.json
```

Ответ 201 и пробуждение worker могут наблюдаться в разном порядке после COMMIT.

Worker выгружает все pending на старте, затем пробуждается по выделенному
`LISTEN sm_jobs_channel` и fallback ticker `POLL_INTERVAL=45s`.
При потере подписки worker пишет один структурированный `ERROR` на эпизод
(`msg="spool listener connection lost; reconnecting"`, `channel="sm_jobs_channel"`,
`error` — причина) перед первой попыткой reconnect. Штатная отмена не считается
потерей подписки. Попытки выполняются последовательно: первая сразу, после
неудачной — отменяемая пауза 1 с. Тикер продолжает будить диспетчер даже во
время попытки или паузы, если пул БД и запись файлов исправны; polling не
устраняет недоступность БД. После восстановления worker сразу проверяет очередь,
не дожидаясь тика, и снова принимает уведомления. NOTIFY — сигнал проверить очередь,
а не отдельная надёжная очередь сообщений.

Файл `in/<job_id>.json` содержит `id,process_id,tool_name,payload,old_status,status`
(обычно `pending/dispatched`). Writer выполняет `.tmp -> file.Sync() -> file.Close()
-> os.Rename(.json)`, поэтому runner не читает частично записанный JSON.
**Захват в БД и публикация файла не образуют общую транзакцию.**
Сбой между dispatch и Rename оставляет dispatched без опубликованного задания;
ошибка writer останавливает worker/приложение, автоматического requeue нет.
Это не обещание exactly-once или полной crash-durability файлового протокола.

Runner сканирует только `in/*.json`, перемещает файл в `processing`, отправляет
in_progress, выполняет CLI, затем отправляет completed/failed и удаляет файл задания
при успешном прохождении callback. Ошибки могут оставить processing и остановить runner.
Автовозобновления `processing/*.json` нет. Без аргументов — один проход,
с `--loop` — проходы с паузой 2 секунды. Конфигурация runner:
`SPOOL_DIR`, `API_BASE_URL` и окружение `PATH`/CLI.

| Инструмент | Реальное поведение |
| --- | --- |
| `echo-test` | `payload.message` (default `default echo`) → `result.json` с `status:"ok",echo` |
| `yt-dlp` | Требует `payload.url`, запускает `yt-dlp --dump-json` → метаданные в `result.json`; не скачивает аудио и не делает транскрипцию |
| `fabric` | `payload.text`, `payload.pattern` (default `summarize`) → `fabric --pattern`, `output.md` и `result.json` с `pattern,output_path` |

Если `yt-dlp` или `fabric` отсутствует в PATH, runner пишет simulated-результат с
`warning` и **может успешно завершить задачу**. Для реальной обработки CLI должны быть
установлены и настроены. Связка yt-dlp + fabric сейчас — два input/процесса,
соединённые оператором, не автоматический конвейер.

## 6. HTTP-контракты и наблюдаемость

Используется стандартный `net/http.ServeMux` с method/path patterns Go 1.22+.

| Метод и путь | Успех | Основные ошибки |
| --- | --- | --- |
| `GET /healthz` | 200 `{"status":"ok"}` | — |
| `GET /readyz` | 200 `{"status":"ready"}` после db.Ping | 503 |
| `POST /api/v1/inputs` | 201, IDs и статус input | 400, 500 |
| `POST /api/v1/jobs/{id}/status` | 204, без тела | 400, 404, 500 |
| `GET /api/v1/processes/{id}` | 200, объект процесса | 400, 404, 500 |
| `GET /api/v1/jobs` | 200, массив задач | 400, 500 |

POST принимает один JSON-документ, тело не более 1 MiB; неизвестные поля верхнего
уровня отклоняются. Ошибки обработчиков имеют `{"error":"..."}`, но это не обещание
JSON-формата для всех стандартных ответов ServeMux (например, неизвестного маршрута).

### Создание input и callback

HTTP transport ограничен: header/read/write/idle — 5/15/20/60 секунд.
После валидации каждый вызов БД из HTTP получает отдельный бюджет до 5 секунд,
включая ожидание соединения; более ранний дедлайн и отмена запроса сохраняются.
Ошибка при истечении собственного DB-бюджета даёт JSON 504 для четырёх API-операций,
readiness сохраняет JSON 503. Отмена клиентом и обычная сетевая ошибка не становятся
серверным 504; успешный результат не заменяется ошибкой из-за позднего таймера.
Это не таймаут выполнения всего Go-handler: транспорт может оборвать соединение,
и для медленного тела JSON-ответ не гарантируется. 504 при мутации не доказывает
откат и не разрешает автоматический повтор.

Вход: `{"source":"manual","payload":{"tool":"echo-test","message":"hello"}}`.
`source` после trim непустой; payload — JSON-объект. Функция БД проверяет точное
имя `tool` и поля инструмента: `echo-test.message` необязателен (по умолчанию
`default echo`); `yt-dlp.url` — абсолютный HTTP(S) URL с host; `fabric.text` —
непустая строка, необязательный `pattern` — `[A-Za-z0-9_-]{1,128}` (по умолчанию
`summarize`). Явный `null` не заменяет отсутствующее поле. Строки и дополнительные
поля не переписываются. Неверный payload даёт SQLSTATE `22023` → HTTP 400
без вставок; остальные ошибки создания в БД дают HTTP 500.

Ответ содержит `input_id,process_id,status:"new",id`; легаси `id` равен `input_id`,
**не job ID**. Job ID получают через GET процесса или списка задач.

Callback использует job UUID и тело:

```json
{"status":"completed","result_path":"/spool/out/019a0000-0000-7000-8000-000000000001/result.json"}
```

Допустимы также `{"status":"in_progress"}` и
`{"status":"failed","error_msg":"tool failed"}`. Для completed обязателен непустой
после trim `result_path`, для failed — `error_msg`. `pending` не принимается.
Невалидный UUID даёт 400; отсутствующая или необновляемая задача — 404.
API не проверяет существование файла по переданному пути.

### Чтение процесса

`GET /api/v1/processes/{id}` возвращает:

- Верхний уровень: `id,input_id,state,context,created_at,updated_at,jobs,outputs`.
- Job: `id,process_id,tool_name,payload,status,priority,result_path,error_msg,created_at,updated_at`.
- Output: `id,process_id,artifact_type,content,metadata,created_at`.

Nullable строки сериализуются как `null`, пустые коллекции — `[]`; JSONB — вложенный
JSON, не строка. Время — JSON-сериализация Go `time.Time` (RFC3339 с возможной дробной
частью). Вложенные jobs и outputs сортируются `created_at ASC,id ASC`.
Чтение выполняется несколькими запросами, не является единым snapshot всей структуры.

### Список задач и пагинация

`GET /api/v1/jobs` возвращает **массив без обёртки**, без `total` или `next_cursor`;
элементы имеют тот же Job-контракт. Query-параметры:

| Параметр | Правило |
| --- | --- |
| `process_id` | UUID процесса |
| `status` | `pending/dispatched/in_progress/completed/failed` |
| `limit` | Положительное целое, default 20, значения выше 100 ограничиваются до 100 |
| `cursor` | UUID существующей задачи; граница по её `(created_at,id)` |

Сортировка — `created_at DESC,id DESC`. Для следующей страницы передают последний
job ID предыдущей, сохраняя фильтры. Несуществующий cursor даёт `[]`, не 404.
Пустые/повторные известные параметры, неверные UUID/status/limit и malformed query
дают 400; неизвестные имена игнорируются при синтаксически корректном query.

### Наблюдаемость

Middleware сохраняет входящий `X-Request-ID` или генерирует UUID, возвращает этот
заголовок и пишет структурированный HTTP-лог через `slog`:
`request_id,client_ip,method,path,status,latency_ms,response_size`.
Приложение выводит JSON-логи в stdout.

Приоритет IP: первый адрес `X-Forwarded-For`, затем `X-Real-IP`, затем `RemoteAddr`.
Проверки trusted proxies нет: оператор обязан обеспечить доверенную границу proxy;
эти заголовки нельзя считать доказательством личности клиента.
Request ID не сохраняется в таблицах/spool и автоматически не связывает отдельные
callbacks с исходным POST. Аудит состояния — отдельные строки `sm_job_logs`.
Liveness не проверяет БД; readiness проверяет её доступность, но не готовность runner/CLI.

## 7. Ограничения и развитие

- Нет lease, автоматического retry/requeue, exactly-once или гарантии идемпотентности
  внешних инструментов. Административный возврат dispatched/in_progress в pending
  может повторить внешние эффекты; порядок остановки и согласования spool — в README.
- Терминальные callbacks не идемпотентны; потерянный ответ после commit требует
  проверки БД, а не предположения, что операция не выполнилась.
- DB backup не включает spool и серверные роли; согласованный backup и restore
  требуют отдельного сохранения файлов и остановки исполнителей.
- Compose монтирует одновременно `.up.sql` и `.down.sql` в initdb: entrypoint
  обрабатывает SQL по имени, а не по смыслу миграции. Один `up` на пустом data dir
  не гарантирует готовую схему. Initdb не повторяется при обычном рестарте.
  Bootstrap с проверкой объектов и явным порядком up описан в README;
  `000002` не идемпотентна.
- SIGINT/SIGTERM запускают graceful shutdown HTTP (до 10 секунд), остановку worker
  и закрытие БД; это не завершает автоматически работу хостового CLI.
  Активный HTTP-запрос, ожидающий БД, ограничен своим 5-секундным бюджетом;
  при ошибке Shutdown сервер принудительно закрывается до остановки worker.
- Внешние DokuWiki/proxy, TLS/auth, автоматический DAG, чтение/скачивание артефактов
  через Go, Gemini API, Telegram, SSE/WebSocket остаются отдельными направлениями,
  а не реализованными возможностями.

Источники контрактов: [API](internal/api/server.go),
[middleware](internal/api/middleware.go), [DB queries](internal/db/queue.go),
[state machine](postgres/migrations/000002_create_state_machine.up.sql),
[stored functions](postgres/migrations/000003_db_state_functions.up.sql),
[payload validation](postgres/migrations/000004_validate_tool_payload.up.sql),
[writer](internal/spool/writer.go), [worker](internal/spool/worker.go),
[runner](scripts/spool-runner.sh), [Compose](docker-compose.yml).
