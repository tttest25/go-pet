# petar — Deployment, Operations Runbook & Testing

Легковесный оркестратор для 1 vCPU / 1 GB RAM: PostgreSQL 18 управляет состояниями,
Go доставляет задания в spool, хостовый Bash runner запускает CLI без Docker-in-Docker.
Архитектура, модель данных и диаграммы: [architecture.md](architecture.md).

### HTTP: пределы ожидания в эксплуатации

Фиксированные transport-лимиты header/read/write/idle — 5/15/20/60 секунд.
Каждый вызов БД из HTTP после валидации имеет бюджет до 5 секунд, включая ожидание
соединения; более ранний дедлайн и отмена клиента наследуются. При ошибке из-за
собственного DB-дедлайна API возвращает JSON 504, readiness — JSON 503.
Обычные сетевые ошибки и отмена клиентом не классифицируются как серверный 504;
успешный результат сохраняется даже при позднем срабатывании таймера.
DB-бюджет не ограничивает выполнение всего handler. Транспорт может разорвать
соединение; для медленного тела JSON не гарантируется. После 504 мутации нельзя
считать её откаченной и автоматически повторять: сначала проверить состояние.
SIGINT/SIGTERM дают HTTP до 10 секунд на Shutdown, затем останавливают worker
и закрывают БД; активное ожидание БД ограничено своим бюджетом. При ошибке Shutdown
HTTP принудительно закрывается до остановки worker. Новых ENV для лимитов нет.

## 1. Deployment

### Требования и конфигурация

Корень проекта — `/opt/go-pet`. Нужны Docker Compose v2, Bash, jq, curl; для
разработки — Go 1.23+ и C toolchain для `-race`. PostgreSQL 18 поставляется образом
`pgvector/pgvector:pg18`. Реальная обработка требует установленных `yt-dlp` и `fabric`
с настроенным провайдером и паттерном `summarize`. В WSL для unit нужен работающий systemd.

| Go ENV | Локальный default | Compose |
| --- | --- | --- |
| `DB_DSN` | `postgres://petar_user:petar_password@127.0.0.1:5432/petar_db?sslmode=disable` | `postgres://petar_user:petar_password@postgres:5432/petar_db?sslmode=disable` |
| `SPOOL_DIR` | `/opt/go-pet/spool` | `/app/spool` |
| `HTTP_ADDR` | `127.0.0.1:8080` | `0.0.0.0:8080` |
| `POLL_INTERVAL` | `45s` (положительная Go duration) | не задан, default `45s` |

Compose содержит буквальные ENV: `export DB_DSN=...` на хосте **не** меняет контейнер.
Менять deployment-конфигурацию нужно оператору явно; этот документ не меняет Compose.

| Runner ENV / аргумент | Default / смысл |
| --- | --- |
| `SPOOL_DIR` | `/opt/go-pet/spool` |
| `API_BASE_URL` | `http://127.0.0.1:8080/api/v1` |
| `PATH` | наследуется; должен содержать реальные CLI |
| `--loop` | проход по `in/*.json`, затем пауза 2 s; без аргумента — один проход |

### Пути, права и границы

| Объект | Хост | Контейнер |
| --- | --- | --- |
| Spool | `/opt/go-pet/spool/{in,processing,out}` | app: `/app/spool/...`; postgres: `/spool/...` |
| PG data | `/opt/go-pet/postgres/data` | `/var/lib/postgresql` (не `/data`) |
| Конфигурация PG | `/opt/go-pet/postgres/postgresql.conf` | `/etc/postgresql/postgresql.conf` |

Runner передаёт `result_path=/spool/out/<job_id>/result.json`. Это метаданные
пути, видимого в postgres, **не URL и не путь app**. На хосте читайте
`/opt/go-pet/spool/out/<job_id>/result.json`. API не открывает этот путь и не
раздаёт внутренние артефакты. Для завершённого задания он читает только
`SPOOL_DIR/out/<job_id>/public.json`.

App работает с UID 1000, writer создаёт файлы с mode `0600`. Поэтому готовый
вариант ниже требует хостового пользователя с **тем же UID 1000**:
одних групп/default ACL недостаточно для чтения новых файлов с mode `0600`.
Runner должен читать файлы app, перемещать их между каталогами и писать `out`.
App, в свою очередь, должен иметь доступ к каталогам результата и `public.json`,
который runner создаёт через `mktemp` с mode `0600`. Не расширяйте права на весь spool.
Проверьте текущую учётную запись; если UID другой, остановитесь и согласуйте
deployment-права/пользователя отдельно, не запускайте runner от root.

```bash
set -euo pipefail
cd /opt/go-pet
test "$(id -u)" -eq 1000
RUNNER_USER="$(id -un)"
sudo mkdir -p spool/{in,processing,out}
# Только каталоги spool, не postgres/data и не всё дерево проекта.
sudo chown "$RUNNER_USER:$(id -gn)" spool spool/{in,processing,out}
sudo chmod u+rwx spool spool/{in,processing,out}
test -r scripts/spool-runner.sh
for d in spool spool/in spool/processing spool/out; do test -w "$d" && test -x "$d"; done
```

Проверьте также право прохода по `/opt/go-pet` для UID 1000. Не используйте
`chmod 777`; существующие файлы должны принадлежать UID 1000, проверяйте их отдельно.

### PostgreSQL и bootstrap миграций

Compose services: `postgres`, `app`; контейнеры: `petar_postgres`, `petar_app`.
Роль / БД / пароль: `petar_user` / `petar_db` / `petar_password`.
Порты опубликованы только на loopback: `127.0.0.1:5432`, `127.0.0.1:8080`.
Сеть: `petar_shared_net` (ключ Compose `petar_net`).
Пароль примера не подходит для публичного production; API не имеет встроенной авторизации.

**Сначала проверьте схему.** На пустом PGDATA Compose запускает
`postgres/bootstrap.sh`: только `*.up.sql` по порядку имён, отдельная транзакция
на файл, остановка при первой ошибке. Down-файлы не выполняются. На существующем
PGDATA initdb повторно не запускается; никогда не удаляйте `postgres/data` ради
bootstrap. Для обновления с `000007` следуйте [runbook T28](#переход-с-000007-на-000008-t28).

Проверка: `./scripts/check-fresh-bootstrap.sh` запускает тот же образ и bootstrap
на одноразовом PGDATA под `postgres/`, без опубликованных портов. `FRESH BOOTSTRAP
PASSED` означает, что проверены схема, четыре точные активные пары v1,
`action_version`, отсутствие старой функции и откатываемые пробы `echo-test`
и `word-count` через `sm_create_action`.

```bash
cd /opt/go-pet
docker compose up -d postgres
docker logs petar_postgres
docker exec petar_postgres pg_isready -U petar_user -d petar_db
docker exec -i petar_postgres psql -U petar_user -d petar_db -v ON_ERROR_STOP=1 <<'SQL'
SELECT extname FROM pg_extension ORDER BY extname;
SELECT srvname FROM pg_foreign_server;
SELECT to_regclass('sm_inputs'), to_regclass('sm_processes'), to_regclass('sm_jobs'),
       to_regclass('sm_job_logs'), to_regclass('sm_outputs'), to_regclass('public.sm_actions');
SELECT to_regprocedure('public.sm_create_input(text,jsonb)') IS NULL AS legacy_retired,
       to_regprocedure('public.sm_create_action(text,integer,jsonb)'),
       to_regprocedure('sm_validate_tool_payload(jsonb)'),
       to_regprocedure('sm_pick_next_job()'),
       to_regprocedure('public.sm_pick_next_job_with_version()'),
       to_regprocedure('sm_update_job_status(uuid,text,text,text)');
SELECT data_type, is_nullable, column_default FROM information_schema.columns
WHERE table_schema='public' AND table_name='sm_jobs' AND column_name='action_version';
SQL
```

На свежей схеме после `000008` запрос `SELECT action, version FROM
public.sm_actions WHERE enabled ORDER BY action, version;` должен вернуть
`echo-test`, `fabric`, `word-count`, `yt-dlp` (все версии 1). В уже используемом каталоге
отключённые записи допустимы.

Существующая БД: при наличии объектов не повторять `000002` — она не идемпотентна.
Пустое окружение: после завершения initdb проверьте результаты выше и логи.
Следующая функция позволяет **явно** применить только недостающие up-файлы
в порядке `000001` → … → `000008`; выполняйте соответствующие вызовы лишь
после проверки, не все автоматически:

```bash
cd /opt/go-pet
apply_up() {
  docker exec -i petar_postgres psql -X -1 -U petar_user -d petar_db \
    -v ON_ERROR_STOP=1 -f - < "/opt/go-pet/postgres/migrations/$1"
}
# Если отсутствуют расширения vector/file_fdw/pg_trgm или сервер dokuwiki_files:
# apply_up 000001_init_extensions.up.sql
# Если отсутствуют ВСЕ пять sm-таблиц:
# apply_up 000002_create_state_machine.up.sql
# Если отсутствуют хранимые функции:
# apply_up 000003_db_state_functions.up.sql
# Если база уже на 000003, но нет sm_validate_tool_payload:
# apply_up 000004_validate_tool_payload.up.sql
# Если база уже на 000004, но нет public.sm_actions:
# apply_up 000005_action_catalog.up.sql
# Если база уже на 000005, а функции T24 и action_version отсутствуют:
# apply_up 000006_universal_json_input.up.sql
# Если база уже на 000006, а правила и пара word-count v1 отсутствуют:
# apply_up 000007_word_count.up.sql
# 000008 на существующей БД — только через runbook T28 ниже.
```

`000001` использует IF NOT EXISTS, `000003` — CREATE OR REPLACE.
При частично созданной схеме, ошибках initdb или несовместимых объектах
**остановитесь и разберите несоответствие**, не запускайте down/повторный `000002`
на рабочей БД. После применения повторите проверку объектов.
В `postgresql.conf` для WSL стоит `io_method='worker'`.

При обновлении с `000003` приостановите приём новых input, примените только
`000004_validate_tool_payload.up.sql`, разверните совместимый Go-код и возобновите
приём. Старый API может вернуть 500 вместо 400, если новая функция уже отклоняет
payload. Для отката используйте `000004_validate_tool_payload.down.sql` и
прежнюю версию приложения в том же окне остановленного приёма. Откат восстанавливает
прежнее тело `sm_create_input`; он не удаляет записи и не переобрабатывает очередь.

При обновлении с `000004` приостановите приём новых input, подтвердите схему
`000004` и отсутствие `public.sm_actions`, примените только
`000005_action_catalog.up.sql`, затем проверьте каталог и возобновите приём.
Откат через `000005_action_catalog.down.sql` восстанавливает валидатор в
штатном пути `sm_create_input`, но удаляет каталог и его настройки доступности.
Очередь и история не изменяются.

При переходе с `000005` остановите приложение на время обновления, подтвердите
схему `000005`, примените только `000006_universal_json_input.up.sql`, проверьте
новые функции и колонку, затем запустите новую версию приложения. Существующие
задания получают `action_version=1`. Для отката остановите новую версию, примените
`000006_universal_json_input.down.sql` и запустите прежнее приложение.
Down удаляет функции T24 и колонку версии, но оставляет очередь, историю и каталог.

При переходе с `000006` остановите приём и runner, подтвердите baseline и
примените только `000007_word_count.up.sql`. Разверните новый `spool-runner.sh`
до возобновления приёма; HTTP-обработчики не меняются. Проверьте `GET /api/v1/actions`
и пробное задание `word-count`. Для отката остановите приём и runner: выполняйте
`000007_word_count.down.sql` только если **нет ни одного** задания `word-count`
(любого статуса или версии). Down блокирует изменения `sm_jobs` на время проверки,
атомарно возвращает прежние правила и удаляет только пару `word-count` v1. При
наличии истории откат отказывается без изменений. Не удаляйте историю ради отката:
при необходимости прекратить новый приём отключите пару, оставив совместимый runner
для ранее принятых заданий.

### Переход с 000007 на 000008 (T28)

Обновление рабочей БД требует отдельного согласования окна и проверенной
резервной копии. До окна перечислите HTTP-клиентов (BFF, proxy, scripts, cron) и
SQL-клиентов (включая процедуры/обёртки); подтвердите переход каждого на
`POST /api/v1/actions` или `public.sm_create_action(text,integer,jsonb)`.
Проверьте зависимости старой функции в `pg_depend` и отдельно код потребителей:
не все вызовы отслеживаются каталогом. Сохраните определения, владельцев и
`proacl` обеих функций (включая исходный `PUBLIC`, роли и grant options), например:

```sql
SELECT oid::regprocedure, pg_get_userbyid(proowner), proacl, pg_get_functiondef(oid)
FROM pg_proc
WHERE oid IN ('public.sm_create_input(text,jsonb)'::regprocedure,
              'public.sm_create_action(text,integer,jsonb)'::regprocedure);
```

1. Закройте новый приём на границе HTTP и у SQL-клиентов, включая прямых writers;
   **оставьте `POST /api/v1/jobs/{id}/status` доступным runner**. Не допускайте
   запуска дополнительных исполнителей или рестарта runner во время ограждения.
   Остановите новые захваты. Если диспетчер Go работает вместе с API, в отдельной открытой
   операторской транзакции захватите `SELECT id FROM public.sm_jobs WHERE
   status='pending' FOR UPDATE` (установите `SET LOCAL lock_timeout='5s'`).
   После получения блокировок `SKIP LOCKED` пропускает эти строки; не закрывайте
   сеанс до остановки старого app. Дождитесь публикации уже захваченных Go заданий.
   Запретите новые захваты runner на границе подачи работы, не посылая SIGTERM
   активному CLI. Если у локального loop runner нет такого переключателя, после
   блокировки pending и подтверждения, что writer больше не публикует файлы,
   атомарно переименуйте только `spool/in` в свободный соседний каталог на той
   же ФС: путь `in/*.json` перестанет давать новые задания. Не переносите
   `processing`; учтите захват, успевший выиграть гонку с ограждением.
2. Дождитесь подтверждённых терминальных callbacks для уже исполняемых задач.
   Сверьте БД и spool по UUID: pending без входного файла, dispatched с
   единственным файлом в `in` (или временно отведённом каталоге) и без файла
   в `processing`. При оставшемся неоднозначном `processing`, потерянном
   подтверждении, незавершённой публикации или дубликате остановите переключение;
   не возвращайте задание в очередь вслепую. Остановите runner и app, закройте
   транзакцию блокировки, верните отведённый `in` под исходное имя, не смешивая
   каталоги. Подтвердите отсутствие других writers и автоматических запусков.
3. Снимите согласованную копию: `pg_dump -Fc` **с владельцами и ACL**,
   отдельно необходимые роли и членства (например, защищённый
   `pg_dumpall --globals-only`), `spool/{in,processing,out}` и фактическое
   хранилище результатов, если оно вне spool. Сохраните манифест ID, связей,
   статусов, версий, аудита и контрольных сумм файлов. Восстановите комплект в
   отдельной PG18 с нужными ролями и правами (`pg_restore --exit-on-error` без
   `--no-owner`/`--no-acl`), в отдельном spool и хранилище; сравните снимок и
   подтвердите старую работоспособность откатываемой пробой. Не загружайте
   globals поверх работающего кластера. См. также [backup / restore](#5-operations-backup--restore).
4. На изолированном стенде отрепетируйте apply–сверку–смену app–возобновление
   `bash scripts/check-retire-legacy-input.sh`. В согласованном окне применяйте
   только `apply_up 000008_retire_legacy_input.up.sql` (с `-1`,
   `ON_ERROR_STOP=1`); отказ `DROP FUNCTION` не обходите через `CASCADE`.
   До возобновления захвата сравните снимки, убедитесь, что `to_regprocedure`
   не находит старую сигнатуру и что под ролью приложения новый SQL-вход
   проходит в откатываемой пробе. Запустите новую версию app с ограждёнными
   submit и захватами (при необходимости снова держите pending row locks),
   проверьте готовность, 404 на старом POST и чтение старых результатов.
   Освободите блокировки, запустите один runner, проверьте старые ID и только
   после сверки возобновите внешний submit. Контрольный новый POST должен создать
   ровно одну цепочку; callback-контракт не меняется.
5. При сбое до возобновления остановите новые компоненты и примените
   `000008_retire_legacy_input.down.sql` (`psql -X -1 -v ON_ERROR_STOP=1`),
   либо восстановите проверенный согласованный комплект; только затем
   запускайте старый app. Down возвращает старую SQL-функцию и делегирование
   из универсальной. При нестандартных owner/ACL добавьте проверенные команды
   восстановления прав из снимка в **ту же транзакцию**, чтобы не открыть
   доступ по умолчанию между операциями. Проверьте обе прежние точки входа и целостность
   очереди. После новых записей или выполнений не накатывайте старый снимок
   поверх них: снова оградите writers, сохраните актуальное состояние и
   согласуйте восстановление без потери новых эффектов.

### Старт приложения

Только после проверки схемы:

```bash
cd /opt/go-pet
docker compose up -d --build
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
docker logs --tail 100 petar_app
```

Compose уже содержит `build.network: host`. При прямой сборке:

```bash
docker build --network host -t petar:local .
```

В WSL не чините DNS внутри build-контейнера, не перебирайте Alpine mirrors
и не создавайте vendor ради обхода сети.

### Хостовый runner через systemd

Unit создаёт **оператор**, не приложение. Пример использует текущего
непривилегированного пользователя, а не вымышленную учётную запись `petar`.
Сначала настройте права из раздела выше, HOME, CLI и конфигурацию fabric.
Команды рассчитаны на обычные имена/пути без пробелов и кавычек.
На хосте runner нужны Bash, `jq`, `python3` (только стандартная библиотека),
`curl`, GNU coreutils (включая `timeout` и `mktemp`)
и `flock` из util-linux. `yt-dlp` и `fabric` нужны для заданий соответствующих типов.

```bash
set -euo pipefail
test "$(id -u)" -eq 1000
RUNNER_USER="$(id -un)"
RUNNER_HOME="$(getent passwd "$RUNNER_USER" | cut -d: -f6)"
RUNNER_PATH="$RUNNER_HOME/.local/bin:$RUNNER_HOME/go/bin:/usr/local/bin:/usr/bin:/bin"
env HOME="$RUNNER_HOME" PATH="$RUNNER_PATH" bash -c 'command -v jq python3 curl timeout mktemp flock yt-dlp fabric'
sudo tee /etc/systemd/system/petar-spool-runner.service >/dev/null <<EOF
[Unit]
Description=petar host spool runner
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=$RUNNER_USER
WorkingDirectory=/opt/go-pet
Environment="HOME=$RUNNER_HOME"
Environment="PATH=$RUNNER_PATH"
Environment=SPOOL_DIR=/opt/go-pet/spool
Environment=API_BASE_URL=http://127.0.0.1:8080/api/v1
ExecStart=/usr/bin/bash /opt/go-pet/scripts/spool-runner.sh --loop
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
curl -fsS http://127.0.0.1:8080/readyz
sudo systemctl daemon-reload
sudo systemctl enable --now petar-spool-runner.service
sudo systemctl status petar-spool-runner.service --no-pager
sudo journalctl -u petar-spool-runner.service -f
```

`After=` задаёт порядок, не готовность API. Runner обрабатывает только `in/*.json`:
без аргументов — один проход, с `--loop` — повторные проходы. На один `SPOOL_DIR`
допускается один runner: второй сразу завершится с ошибкой блокировки. Не удаляйте
постоянный файл `.runner.lock`. Оставшиеся `processing/*.json` не возобновляются
даже при рестарте unit.

Каждый callback ограничен 3 с на подключение и 10 с на весь запрос. Только
HTTP 204 при успешном завершении `curl` подтверждает статус; редиректы и повторные POST
не выполняются. При неподтверждённом статусе runner завершится с ошибкой и оставит
файл в `processing` для сверки по job ID.

Для `yt-dlp` и `fabric` `RUNNER_TOOL_TIMEOUT` задаёт целое число секунд от 1 до
86400 (по умолчанию 300); после TERM даётся ещё 1 с до KILL. Нет CLI, ненулевой
код или таймаут → `failed`, а не `completed`. Подтверждённый `failed` не считается
сбоем runner. Невалидное значение таймаута отклоняется до захвата заданий.

```bash
sudo systemctl stop petar-spool-runner.service
find /opt/go-pet/spool/processing -maxdepth 1 -type f -name '*.json' -print
# Для оставшихся файлов сначала выполните диагностику/recovery из раздела 4.
sudo systemctl start petar-spool-runner.service
```

### Внешний DokuWiki BFF и reverse proxy

DokuWiki BFF — **серверный клиент** API; браузер обращается к BFF, а не напрямую
к petar. Исходники/конфигурация плагина здесь не поставляются: upstream URL
настраивается средствами конкретного плагина. PHP actions, имена его параметров,
CORS и auth middleware здесь не определяются.

Ниже три **альтернативных**, независимых варианта. Домен `petar.example.test`
и entrypoint `web` — параметры примера, не текущая конфигурация репозитория.
Не публикуйте незащищённый API в интернет: ограничьте доступ доверенным BFF/сетям,
организуйте TLS и авторизацию на внешнем периметре. Пробы не входят в API-router.
Ни один вариант не использует StripPrefix: `/api/v1` должен сохраниться ровно один раз.

#### Вариант A: Traefik Docker provider

После создания сети основным Compose оператор размещает, например,
`/opt/go-pet/compose.proxy.yml` (добавление к основному Compose):

```yaml
services:
  app:
    labels:
      traefik.enable: "true"
      traefik.docker.network: petar_shared_net
      traefik.http.routers.petar.rule: "Host(`petar.example.test`) && PathPrefix(`/api/v1`)"
      traefik.http.routers.petar.entrypoints: web
      traefik.http.routers.petar.service: petar-api
      traefik.http.services.petar-api.loadbalancer.server.port: "8080"
```

Отдельный пример `/opt/go-pet/traefik-docker.compose.yml` для внешнего proxy:

```yaml
services:
  traefik:
    image: traefik:v3
    command:
      - --entrypoints.web.address=:80
      - --providers.docker=true
      - --providers.docker.exposedbydefault=false
    ports:
      - "127.0.0.1:8081:80"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks: [shared]
networks:
  shared:
    external: true
    name: petar_shared_net
```

Docker socket даёт чувствительный доступ к Docker API даже при mount `ro`;
для production используйте защищённый socket proxy. Запуск оператором:

```bash
cd /opt/go-pet
docker compose -f docker-compose.yml -f compose.proxy.yml up -d app
docker compose -p petar-proxy -f traefik-docker.compose.yml up -d
```

#### Вариант B: Traefik file provider

Оператор размещает static `/opt/go-pet/traefik/traefik.yml`:

```yaml
entryPoints:
  web:
    address: ":80"
providers:
  file:
    filename: /etc/traefik/dynamic.yml
    watch: true
```

И dynamic `/opt/go-pet/traefik/dynamic.yml`:

```yaml
http:
  routers:
    petar:
      rule: "Host(`petar.example.test`) && PathPrefix(`/api/v1`)"
      entryPoints: [web]
      service: petar-api
  services:
    petar-api:
      loadBalancer:
        servers:
          - url: "http://petar_app:8080"
```

Самостоятельный `/opt/go-pet/traefik-file.compose.yml`:

```yaml
services:
  traefik:
    image: traefik:v3
    ports:
      - "127.0.0.1:8081:80"
    volumes:
      - /opt/go-pet/traefik/traefik.yml:/etc/traefik/traefik.yml:ro
      - /opt/go-pet/traefik/dynamic.yml:/etc/traefik/dynamic.yml:ro
    networks: [shared]
networks:
  shared:
    external: true
    name: petar_shared_net
```

```bash
docker compose -p petar-proxy-file -f /opt/go-pet/traefik-file.compose.yml up -d
```

Для Traefik **на хосте** вместо контейнерного backend используйте
`http://127.0.0.1:8080`, а `providers.file.filename` замените на реальный host path
`/opt/go-pet/traefik/dynamic.yml`. Выберите свободный listen port (например `:8081`).
Для Traefik в контейнере localhost обозначает сам Traefik, а не app.

#### Вариант C: Nginx на хосте

Оператор помещает `/etc/nginx/conf.d/petar.conf` в контекст `http`:

```nginx
server {
    listen 127.0.0.1:8081;
    server_name petar.example.test;
    client_max_body_size 1m;

    location /api/v1/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Request-ID $http_x_request_id;
        proxy_pass_header X-Request-ID;
    }
}
```

`proxy_pass` без URI сохраняет путь и query; метод и тело передаются по умолчанию.
В контейнерном Nginx, подключённом к `petar_shared_net`, backend должен быть
`http://petar_app:8080`, не localhost. Ответ `X-Request-ID` не скрывается.

```bash
sudo nginx -t
sudo systemctl reload nginx
```

#### Проверка BFF upstream через выбранный proxy

BFF на хосте может использовать base `http://petar.example.test:8081/api/v1`
при разрешении домена в loopback. BFF в контейнере должен иметь сетевой доступ
к proxy (например `http://traefik:80/api/v1` в общей сети с корректным Host);
его `127.0.0.1` не является хостом. Настройка Host/upstream зависит от плагина.
Проверка с хоста (для любого из примеров на 8081):

```bash
PROXY_URL=http://127.0.0.1:8081/api/v1
RESPONSE="$(curl -fsS -H 'Host: petar.example.test' -H 'X-Request-ID: proxy-check' \
  -H 'Content-Type: application/json' -d '{"action":"echo-test","version":1,"data":{"message":"proxy"}}' \
  "$PROXY_URL/actions")"
PROCESS_ID="$(jq -er .process_id <<<"$RESPONSE")"
curl -fsS -D - -H 'Host: petar.example.test' "$PROXY_URL/processes/$PROCESS_ID"
curl -fsS --get -H 'Host: petar.example.test' \
  --data-urlencode "process_id=$PROCESS_ID" --data-urlencode 'limit=20' "$PROXY_URL/jobs"
```

## 2. HTTP API и наблюдаемость

| Метод и путь | Успех | Ошибки обработчика |
| --- | --- | --- |
| `GET /healthz` | 200 `{"status":"ok"}` | — |
| `GET /readyz` | 200 `{"status":"ready"}` | 503 |
| `POST /api/v1/jobs/{id}/status` | 204 без тела | 400, 404, 500 |
| `GET /api/v1/processes/{id}` | 200 объект | 400, 404, 500 |
| `GET /api/v1/jobs` | 200 массив | 400, 500 |
| `GET /api/v1/jobs/{id}/result` | 200 публичный JSON, 202 статус задания | 400, 404, 409, 410, 500, 504 |
| `GET /api/v1/actions` | 200 массив активных версий | 500, 504 |
| `POST /api/v1/actions` | 201, IDs и статус input | 400, 500, 504 |

`GET /api/v1/actions` читает каталог PostgreSQL на каждый запрос и возвращает
массив без обёртки, например `[{"action":"echo-test","version":1,"title":"Echo test"}]`.
В списке все включённые пары в порядке имени и числовой версии по возрастанию,
без пагинации; пустой список — `[]`. Отключение версии убирает её из списка,
но не меняет историю задач. Включение пары в каталоге не устанавливает для неё
правила запуска. Истечение 5-секундного бюджета БД при ошибке
чтения даёт JSON 504; прочая ошибка БД — JSON 500 без внутренних деталей.

POST принимает ровно один JSON-документ до 1 MiB, неизвестные поля верхнего
уровня отклоняются. Снятый `POST /api/v1/inputs` возвращает 404, а прежняя
SQL-функция отсутствует. `sm_create_action` проверяет точное имя действия и поля `data`:

| `action` | Поля `data` |
| --- | --- |
| `echo-test` | `message` необязателен: отсутствие означает `default echo`; если задан, это непустая, не состоящая из пробелов строка. |
| `yt-dlp` | Обязательный `url`: абсолютный HTTP(S) URL с непустым host (для IDN используйте ASCII/Punycode). Проверка синтаксическая, без сетевого запроса. |
| `fabric` | Обязательный непустой строковый `text`; необязательный `pattern` из 1–128 символов `[A-Za-z0-9_-]`, при отсутствии — `summarize`. |
| `word-count` | Обязательный строковый `text`; пустая строка и строка из одних разделителей допустимы. |

Отсутствующий необязательный ключ не равен явному `null`: `null` отклоняется.
Дополнительные поля допускаются и сохраняются, строки не переписываются.
Неверные поля дают 400 без создания input/process/job. Эти правила не распространяются на прямой
`INSERT INTO sm_inputs`, который остаётся под прежними ограничениями БД.
Ошибки обработчиков имеют `{"error":"..."}`; стандартные ответы ServeMux
(например 405) не обязаны иметь такой JSON.

Пример ответа (ID иллюстративные):

```json
{
  "input_id": "01900000-0000-7000-8000-000000000001",
  "process_id": "01900000-0000-7000-8000-000000000002",
  "status": "new",
  "id": "01900000-0000-7000-8000-000000000001"
}
```

Legacy `id=input_id`, это **не job ID**. Input остаётся `new` и после
завершения процесса: автоматический перевод в `processed/error` не реализован.

Универсальный вход:

```http
POST /api/v1/actions
Content-Type: application/json

{"action":"echo-test","version":1,"data":{"message":"hello"}}
```

`action` — непустая строка, имя сравнивается точно; `version` — JSON-целое
от 1 до 2147483647. `data` обязателен: отсутствие → 400 до обращения к БД,
явный `null` передаётся в БД и отклоняется правилами действия. `source`
не принимается, в БД используется `api`. PostgreSQL принимает только активные
пары с реализованными правилами: `echo-test`, `fabric`, `word-count`, `yt-dlp` версии 1.
`data` должен быть объектом без ключа `tool` на верхнем уровне; БД подставляет
`tool` из `action` и валидирует данные по таблице выше.
Ошибки правил, включая активную v2 без валидатора, дают JSON 400 без вставок.
Другие ошибки БД → JSON 500; истечение собственного DB-бюджета при ошибке
→ JSON 504. Ответ 201 имеет те же четыре поля, что и выше; существующие GET
не меняются. Runner выбирает обработчик по `tool_name` и корневому
`action_version`: для этих четырёх действий поддерживается только
число 1. Только отсутствие поля означает v1; версия из payload игнорируется.

Пример `word-count`: отправьте `{"action":"word-count","version":1,"data":{"text":"один два три"}}`,
возьмите job ID из `jobs` в `GET /api/v1/processes/{process_id}`, затем запросите
`GET /api/v1/jobs/{id}/result`. После завершения ответ: `{"type":"word-count","version":1,"data":{"words":3}}`.

Callback разрешён только для исходных `dispatched/in_progress`:
`in_progress`, `completed` с непустым `result_path`, `failed` с непустым `error_msg`.
`pending` через HTTP не принимается. Отсутствующая/необновляемая задача — 404;
повтор terminal callback тоже 404. Повтор `in_progress` допустим, но аудит
добавляется только при изменении статуса.

```json
{"status":"in_progress"}
```

```json
{"status":"completed","result_path":"/spool/out/01900000-0000-7000-8000-000000000003/result.json"}
```

```json
{"status":"failed","error_msg":"tool exited with code 1"}
```

GET process возвращает поля:

- Процесс: `id,input_id,state,context,created_at,updated_at,jobs,outputs`.
- Job: `id,process_id,tool_name,payload,status,priority,result_path,error_msg,created_at,updated_at`.
- Output: `id,process_id,artifact_type,content,metadata,created_at`.
- Nullable строки — `null`, пустые коллекции — `[]`, JSONB — вложенный JSON,
  времена — JSON-сериализация Go `time.Time` (RFC3339 с дробной частью при наличии).
- Jobs и outputs внутри процесса упорядочены `created_at ASC,id ASC`.
- Успешное завершение через функцию создаёт output `artifact_type="result"`,
  `content=null`, `metadata={"result_path":"/spool/out/.../result.json"}`.

Пример завершённого процесса:

```json
{
  "id": "01900000-0000-7000-8000-000000000002",
  "input_id": "01900000-0000-7000-8000-000000000001",
  "state": "completed",
  "context": {},
  "created_at": "2026-11-16T10:00:00Z",
  "updated_at": "2026-11-16T10:00:02Z",
  "jobs": [{
    "id": "01900000-0000-7000-8000-000000000003",
    "process_id": "01900000-0000-7000-8000-000000000002",
    "tool_name": "echo-test",
    "payload": {"tool":"echo-test","message":"hello"},
    "status": "completed",
    "priority": 0,
    "result_path": "/spool/out/01900000-0000-7000-8000-000000000003/result.json",
    "error_msg": null,
    "created_at": "2026-11-16T10:00:00Z",
    "updated_at": "2026-11-16T10:00:02Z"
  }],
  "outputs": [{
    "id": "01900000-0000-7000-8000-000000000004",
    "process_id": "01900000-0000-7000-8000-000000000002",
    "artifact_type": "result",
    "content": null,
    "metadata": {"result_path":"/spool/out/01900000-0000-7000-8000-000000000003/result.json"},
    "created_at": "2026-11-16T10:00:02Z"
  }]
}
```

GET jobs — массив таких job-объектов **без обёртки**, total или next_cursor.
Фильтры: `process_id`, `status`, `limit`, `cursor`. Статусы:
`pending,dispatched,in_progress,completed,failed`. Limit default 20, положительное
целое, сверху ограничен 100. Пустые/повторные известные параметры, неверные UUID,
status, limit и malformed query дают 400. Неизвестные имена игнорируются, если
query синтаксически корректен. Порядок `created_at DESC,id DESC`; cursor — ID
существующей задачи, граница по паре `(created_at,id)`, не только UUID.
Следующая страница использует последний ID предыдущей и те же фильтры.
Несуществующий cursor даёт `[]`, не 404.

Middleware сохраняет входящий `X-Request-ID` либо создаёт UUID и возвращает его
в заголовке. JSON slog в stdout: `request_id`, `client_ip`, `method`, `path`,
`status`, `latency_ms`, `response_size`. IP: первый X-Forwarded-For, затем
X-Real-IP, затем RemoteAddr. Trusted-proxy validation нет: периметр должен
контролировать подставляемые заголовки. Request ID не сохраняется в БД/spool
и автоматически не связывает callbacks с исходным POST.

## 3. Operations: ручной E2E

### Echo-test, GET и пагинация

Все следующие блоки — Bash. Нужны работающие app и runner (systemd или отдельный
`bash scripts/spool-runner.sh --loop`, но не оба одновременно).
Выполняйте сценарий в одной Bash-сессии с `set -euo pipefail`.

```bash
set -euo pipefail
cd /opt/go-pet
API_BASE_URL=http://127.0.0.1:8080/api/v1
MESSAGE='hello from operator'
REQUEST_ID="manual-$(date +%s)"
HEADERS="$(mktemp "${TMPDIR:-/tmp}/petar-headers.XXXXXX")"
RESPONSE="$(jq -n --arg text "$MESSAGE" \
  '{action:"echo-test",version:1,data:{message:$text}}' |
  curl -fsS -D "$HEADERS" -H "X-Request-ID: $REQUEST_ID" \
    -H 'Content-Type: application/json' --data-binary @- "$API_BASE_URL/actions")"
jq . <<<"$RESPONSE"
PROCESS_ID="$(jq -er .process_id <<<"$RESPONSE")"
grep -i '^X-Request-ID:' "$HEADERS"
rm "$HEADERS"
docker logs --since 5m petar_app 2>&1 | grep -F "$REQUEST_ID"
curl -fsS "$API_BASE_URL/processes/$PROCESS_ID" | jq .

# Общая ограниченная функция ожидания; return 1=failed, 2=timeout.
wait_process() {
  local pid="$1" snapshot state n
  for ((n=0; n<120; n++)); do
    snapshot="$(curl -fsS --max-time 5 "$API_BASE_URL/processes/$pid")" || return 3
    state="$(jq -er .state <<<"$snapshot")" || return 3
    case "$state" in
      completed) printf '%s\n' "$snapshot"; return 0 ;;
      failed) printf '%s\n' "$snapshot" >&2; echo 'process failed' >&2; return 1 ;;
    esac
    sleep 1
  done
  echo "timeout waiting for $pid" >&2
  return 2
}
DETAILS="$(wait_process "$PROCESS_ID")"
JOB_ID="$(jq -er '.jobs[0].id' <<<"$DETAILS")"
cat "/opt/go-pet/spool/out/$JOB_ID/result.json"
jq -e --arg path "/spool/out/$JOB_ID/result.json" \
  '.outputs | any(.artifact_type=="result" and .content==null and .metadata.result_path==$path)' \
  <<<"$DETAILS"

PAGE="$(curl -fsS --get "$API_BASE_URL/jobs" \
  --data-urlencode "process_id=$PROCESS_ID" --data-urlencode 'status=completed' \
  --data-urlencode 'limit=20')"
jq . <<<"$PAGE"
CURSOR="$(jq -r 'last.id // empty' <<<"$PAGE")"
if [[ -n "$CURSOR" ]]; then
  curl -fsS --get "$API_BASE_URL/jobs" \
    --data-urlencode "process_id=$PROCESS_ID" --data-urlencode 'status=completed' \
    --data-urlencode 'limit=20' --data-urlencode "cursor=$CURSOR" | jq .
fi
```

После получения `JOB_ID` выполните аудит из раздела 4. Ожидаются переходы
pending → dispatched → in_progress → completed; записи об исходном INSERT
pending в `sm_job_logs` нет.

### Реальный yt-dlp + fabric: два input, два процесса

Это **операторская передача данных**, не автоматический pipeline/DAG.
`yt-dlp --dump-json` возвращает метаданные, не скачивает аудио и не транскрибирует.
Предварительно выполните `command -v yt-dlp fabric` с HOME/PATH пользователя unit
и настройте провайдера/pattern fabric в его HOME. Проверьте unit через
`systemctl cat petar-spool-runner.service`; окружение интерактивного shell
может отличаться. Пример проверки для пользователя текущего unit:

```bash
RUNNER_USER="$(systemctl show -p User --value petar-spool-runner.service)"
RUNNER_HOME="$(getent passwd "$RUNNER_USER" | cut -d: -f6)"
RUNNER_PATH="$RUNNER_HOME/.local/bin:$RUNNER_HOME/go/bin:/usr/local/bin:/usr/bin:/bin"
sudo -u "$RUNNER_USER" env HOME="$RUNNER_HOME" PATH="$RUNNER_PATH" \
  bash -c 'command -v yt-dlp && command -v fabric'
```

Следующий блок использует `API_BASE_URL` и `wait_process` из echo-примера;
ожидание ограничено 120 итерациями (пауза 1 s, каждый HTTP-запрос до 5 s).
Для длинных CLI увеличьте число итераций осознанно.

```bash
read -r -p 'URL одного видео (не playlist): ' VIDEO_URL
test -n "$VIDEO_URL"
YTDLP_RESPONSE="$(jq -n --arg url "$VIDEO_URL" \
  '{action:"yt-dlp",version:1,data:{url:$url}}' |
  curl -fsS -H 'Content-Type: application/json' --data-binary @- "$API_BASE_URL/actions")"
YTDLP_PROCESS="$(jq -er .process_id <<<"$YTDLP_RESPONSE")"
YTDLP_DETAILS="$(wait_process "$YTDLP_PROCESS")"
YTDLP_JOB="$(jq -er '.jobs[0].id' <<<"$YTDLP_DETAILS")"
META="/opt/go-pet/spool/out/$YTDLP_JOB/result.json"
jq . "$META"
# Только доступные title/description, НЕ транскрипт. Максимум 16k символов.
TEXT="$(jq -sr 'map([.title // "", .description // ""] | join("\n")) | join("\n") | .[0:16000]' "$META")"
test -n "$TEXT"
FABRIC_BODY="$(jq -n --arg text "$TEXT" \
  '{action:"fabric",version:1,data:{pattern:"summarize",text:$text}}')"
test "$(printf '%s' "$FABRIC_BODY" | wc -c)" -lt 1048576
FABRIC_RESPONSE="$(curl -fsS -H 'Content-Type: application/json' \
  --data-binary "$FABRIC_BODY" "$API_BASE_URL/actions")"
FABRIC_PROCESS="$(jq -er .process_id <<<"$FABRIC_RESPONSE")"
FABRIC_DETAILS="$(wait_process "$FABRIC_PROCESS")"
FABRIC_JOB="$(jq -er '.jobs[0].id' <<<"$FABRIC_DETAILS")"
RESULT="/opt/go-pet/spool/out/$FABRIC_JOB/result.json"
jq . "$RESULT"
cat "/opt/go-pet/spool/out/$FABRIC_JOB/output.md"
```

Fabric читает `payload.text` через stdin, запускается с `--pattern payload.pattern`
и пишет stdout в `output.md`; при успехе `result.json` содержит `output_path`
(host path runner). `yt-dlp` пишет stdout в `result.json`. Stderr обоих CLI
сохраняется отдельно в `out/<job_id>/stderr.log`, в том числе при ошибке.
Частичный stdout при `failed` — диагностика, а не подтверждённый результат.
Артефакты могут содержать чувствительные данные.

Перед `completed` runner атомарно переименовывает подготовленный файл в
`out/<job_id>/public.json`. В файле ровно три поля: `type` (имя действия),
`version` (число `1`, версия формата результата) и `data`:

| `type` | `data` |
| --- | --- |
| `echo-test` | `{"echo":"hello"}`; если `message` отсутствует, используется `"default echo"` |
| `fabric` | `{"text":"…"}`; полный непустой UTF-8 из `output.md`, включая конечные переводы строк |
| `word-count` | `{"words":3}`; число последовательностей символов вне Unicode White_Space. Дефисы и пунктуация не разделяют слова, пустой или полностью пробельный текст даёт `0`. |
| `yt-dlp` | `{"items":[…]}`; один элемент на объект stdout в исходном порядке, только строковые `title` и/или `description` (хотя бы одно из полей в каждом элементе) |

Служебные пути, stderr и остальные метаданные в публичный документ не попадают;
сам текст не редактируется. Неподдерживаемая версия, недоступный CLI или
невалидный вывод приводят к `failed` без публикации, но диагностические файлы
сохраняются. При ручном повторе runner удаляет старый `public.json` только после
подтверждения нового `in_progress`; если старую публикацию невозможно очистить,
runner останавливается и сохраняет `processing` для сверки. Если не получено
подтверждение `completed`, подготовленный публичный файл и `processing`
сохраняются, а callback не повторяется.

Legacy callback по-прежнему передаёт `/spool/out/<job_id>/result.json`.
Для чтения публичного результата используйте UUID **задания**, не процесса
или входа:

```bash
JOB_ID=019a0000-0000-7000-8000-000000000001
curl -sS -i "http://127.0.0.1:8080/api/v1/jobs/$JOB_ID/result"
```

| HTTP | Значение |
| --- | --- |
| 200 | Задание `completed`; исходный публичный JSON, не более 1 048 576 байт включительно (с пробельными символами) |
| 202 | `{"status":"pending"}`, `{"status":"dispatched"}` или `{"status":"in_progress"}`, даже если файл уже опубликован |
| 400 / 404 | Неверный UUID / неизвестное задание |
| 409 | `{"error":"job_failed"}`, без диагностики runner |
| 410 | `{"error":"result_unavailable"}` — завершённое задание без публичного файла, в том числе старое legacy-задание |
| 500 | `{"error":"result_too_large"}` при превышении лимита; повреждённый или нечитаемый файл и обычные ошибки БД дают нейтральную JSON-ошибку |
| 504 | Истёк собственный бюджет запроса статуса в БД |

При ошибке размера или формата API не выдаёт часть документа и не пытается
читать `result.json` или другие внутренние файлы. Старые завершённые задания не
обрабатываются повторно.

## 4. Operations: аудит и recovery

### Диагностика

```bash
docker logs --tail 100 petar_app
docker logs --tail 100 petar_postgres
sudo journalctl -u petar-spool-runner.service --since '15 minutes ago' --no-pager
find /opt/go-pet/spool/{in,processing,out} -maxdepth 2 -type f -print
# JOB_ID и PROCESS_ID получены из API, не путать с input_id.
docker exec -i petar_postgres psql -U petar_user -d petar_db \
  -v ON_ERROR_STOP=1 -v job_id="$JOB_ID" -v process_id="$PROCESS_ID" <<'SQL'
SELECT * FROM sm_jobs WHERE id = :'job_id'::uuid;
SELECT * FROM sm_processes WHERE id = :'process_id'::uuid;
SELECT id,job_id,old_status,new_status,message,created_at
FROM sm_job_logs WHERE job_id = :'job_id'::uuid ORDER BY created_at,id;
SELECT l.id,l.job_id,l.old_status,l.new_status,l.message,l.created_at
FROM sm_job_logs l JOIN sm_jobs j ON j.id=l.job_id
WHERE j.process_id=:'process_id'::uuid ORDER BY l.created_at,l.id;
SQL
```

### Зависшие dispatched/in_progress

Захват БД и публикация файла — не общая транзакция. Сбой после dispatch может
оставить задачу без файла. При неподтверждённом callback до ручного решения
сверьте статус и аудит в БД по job ID (запросы выше), затем проверьте `processing`
и артефакты: сервер мог применить переход, даже если runner не получил HTTP 204.
При неподтверждённом `in_progress` CLI не запускается; при сбое терминального
callback он уже мог выполнить внешнее действие. Не повторяйте POST или CLI
только на основании ошибки сети или HTTP-кода.
Exactly-once, lease, автоматического retry и идемпотентности CLI нет.
15 минут ниже — эвристика оператора, не встроенный timeout.

```bash
docker exec -i petar_postgres psql -U petar_user -d petar_db -v ON_ERROR_STOP=1 <<'SQL'
SELECT id,process_id,status,updated_at,result_path,error_msg
FROM sm_jobs
WHERE status IN ('dispatched','in_progress') AND updated_at < NOW()-INTERVAL '15 minutes'
ORDER BY updated_at;
SQL
```

1. Выберите **один** job. Остановите runner и app; проверьте отсутствие других
   runner/локальных Go-сервисов/активных CLI, включая запущенные вручную.
   Остановка unit может прервать CLI; убедитесь, что внешняя операция действительно
   прекращена. Сохраните исходные записи/артефакты.
2. Архивируйте только файлы выбранного ID, уберите их из активных in/processing.
   **Не** переносите processing обратно в in одновременно с DB requeue.
3. Просмотрите запись непосредственно перед операцией. Requeue либо failed —
   варианты только для `dispatched/in_progress`. Если `completed` уже записан
   и output указывает на сохранённый результат, оставьте результат по исходному
   пути и ограничьтесь архивированием `processing`: без SQL-мутации и без повторного POST.
4. Проверьте audit/process, запустите app, дождитесь readyz, затем runner.

```bash
set -euo pipefail
read -r -p 'Job UUID для recovery: ' JOB_ID
[[ "$JOB_ID" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]
JOB_ID="${JOB_ID,,}"
sudo systemctl stop petar-spool-runner.service
docker stop petar_app
pgrep -af 'spool-runner|yt-dlp|fabric|petar' || true
# ПРОВЕРКА ОПЕРАТОРА: других исполнителей/CLI нет; только после неё продолжать.
ARCHIVE="/opt/go-pet/spool/recovery/$JOB_ID-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$ARCHIVE"/{in,processing}
docker exec -i petar_postgres psql -U petar_user -d petar_db \
  -v ON_ERROR_STOP=1 -v job_id="$JOB_ID" > "$ARCHIVE/db-before.txt" <<'SQL'
SELECT * FROM sm_jobs WHERE id=:'job_id'::uuid;
SELECT p.* FROM sm_processes p JOIN sm_jobs j ON j.process_id=p.id WHERE j.id=:'job_id'::uuid;
SELECT * FROM sm_job_logs WHERE job_id=:'job_id'::uuid ORDER BY created_at,id;
SELECT o.* FROM sm_outputs o JOIN sm_jobs j ON j.process_id=o.process_id WHERE j.id=:'job_id'::uuid;
SQL
for stage in in processing; do
  for ext in json tmp; do
    f="/opt/go-pet/spool/$stage/$JOB_ID.$ext"
    if [[ -f "$f" ]]; then mv -- "$f" "$ARCHIVE/$stage/"; fi
  done
done
if [[ -d "/opt/go-pet/spool/out/$JOB_ID" ]]; then
  cp -a "/opt/go-pet/spool/out/$JOB_ID" "$ARCHIVE/out"
fi
cat "$ARCHIVE/db-before.txt"
```

**Вариант A — административный возврат в pending:**

```bash
docker exec -i petar_postgres psql -U petar_user -d petar_db \
  -v ON_ERROR_STOP=1 -v job_id="$JOB_ID" <<'SQL'
BEGIN;
UPDATE sm_jobs
SET status = 'pending', result_path = NULL, error_msg = NULL,
    updated_at = NOW()
WHERE id = :'job_id'::uuid
  AND status IN ('dispatched', 'in_progress')
RETURNING id, OLD.status AS old_status, NEW.status AS new_status;
COMMIT;
SQL
```

Это административный обход callback-функции: триггеры запишут аудит,
переведут процесс в init и опубликуют NOTIFY после commit. App также выгружает
pending на старте. UPDATE 0 означает отсутствие ID или уже иной статус:
проверить запись, не расширять WHERE. CLI может выполниться повторно, артефакты
могут перезаписаться; копия сохранена в recovery.

**Вариант B — завершить ошибкой через функцию (не выполнять после A):**

```bash
docker exec -i petar_postgres psql -U petar_user -d petar_db \
  -v ON_ERROR_STOP=1 -v job_id="$JOB_ID" <<'SQL'
SELECT sm_update_job_status(
    :'job_id'::uuid, 'failed', NULL, 'manual recovery: abandoned job'
);
SQL
```

Функция принимает только dispatched/in_progress; при конкурентной смене
статуса выдаст ошибку. Не выполняйте массовый UPDATE незавершённых задач.
Прямой UPDATE в completed **не** эквивалентен функции: он не создаёт sm_outputs.

После любого одного варианта:

```bash
docker exec -i petar_postgres psql -U petar_user -d petar_db \
  -v ON_ERROR_STOP=1 -v job_id="$JOB_ID" <<'SQL'
SELECT j.id,j.status,p.state FROM sm_jobs j JOIN sm_processes p ON p.id=j.process_id
WHERE j.id=:'job_id'::uuid;
SELECT id,job_id,old_status,new_status,message,created_at
FROM sm_job_logs WHERE job_id=:'job_id'::uuid ORDER BY created_at,id;
SQL
docker start petar_app
for ((n=0; n<30; n++)); do
  if curl -fsS --max-time 5 http://127.0.0.1:8080/readyz; then break; fi
  sleep 1
done
curl -fsS http://127.0.0.1:8080/readyz
sudo systemctl start petar-spool-runner.service
```

## 5. Operations: backup / restore

Дамп БД не сохраняет spool и серверные роли. Для согласованной копии остановите
все источники записи, runner и app, убедитесь в отсутствии активных CLI.
PostgreSQL остаётся доступен. Сохраните БД и spool в одно окно остановки:

```bash
set -euo pipefail
cd /opt/go-pet
sudo systemctl stop petar-spool-runner.service
docker stop petar_app
# Подтвердите отсутствие других исполнителей/внешних записей в БД.
mkdir -p /opt/go-pet/postgres/backups
BACKUP="/opt/go-pet/postgres/backups/petar_db-$(date +%Y%m%d-%H%M%S).dump"
docker exec petar_postgres pg_dump -U petar_user -d petar_db -Fc > "$BACKUP"
test -s "$BACKUP"
docker exec -i petar_postgres pg_restore --list < "$BACKUP"
tar -C /opt/go-pet -czf "${BACKUP%.dump}-spool.tar.gz" spool
tar -tzf "${BACKUP%.dump}-spool.tar.gz" >/dev/null
```

Храните копии вне этого хоста с подходящими правами/шифрованием; отдельно
сохраните deployment-секреты и необходимые роли. Роль `petar_user` и база
`petar_db` должны существовать до restore.

**Восстановление разрушительно**: выберите конкретный проверенный backup,
заранее сделайте копию текущего состояния. App/runner и внешние writers
остановлены, PostgreSQL работает. `--clean --if-exists` удаляет восстанавливаемые
объекты перед загрузкой, не очищает произвольные посторонние объекты.
Расширения из dump должны быть доступны в образе.

```bash
set -euo pipefail
read -r -p 'Полный путь к выбранному .dump: ' BACKUP
test -s "$BACKUP"
docker exec -i petar_postgres pg_restore \
  -U petar_user -d petar_db --clean --if-exists --no-owner \
  --exit-on-error < "$BACKUP"
# Spool восстанавливайте из ПАРНОГО архива, не смешивая с текущими файлами.
test -s "${BACKUP%.dump}-spool.tar.gz"
SAVED_SPOOL="/opt/go-pet/spool-before-restore-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$SAVED_SPOOL"
# Не переименовывать сам bind-mount root: контейнеры держат его inode.
find /opt/go-pet/spool -mindepth 1 -maxdepth 1 -exec mv -t "$SAVED_SPOOL" -- {} +
tar -C /opt/go-pet -xzf "${BACKUP%.dump}-spool.tar.gz"
```

Проверьте владельцев/ACL, таблицы и функции командами bootstrap выше,
просмотрите незавершённые jobs и processing: они сами не возобновятся.
Согласуйте их адресно через recovery перед запуском runner.
Затем `docker start petar_app`, ограниченное ожидание readyz (как в recovery),
`curl -fsS http://127.0.0.1:8080/healthz` и
`sudo systemctl start petar-spool-runner.service`.
Не выполняйте restore/down на рабочей базе ради проверки документации.

## 6. Testing & Quality Gate

Это unit **и интеграционные** тесты: нужна PostgreSQL 18 со схемой.
Тесты меняют/чистят очередь и используют только `PETAR_TEST_DB_DSN`, без fallback
на `DB_DSN` или production defaults. Разрешена только локальная
`127.0.0.1:5432/petar_test`, пользователь `petar_user`, контейнер `petar_postgres`.
URL и keyword/value DSN разбираются драйвером; неоднозначные маршруты, service,
fallback hosts и runtime options отклоняются. Ошибки не должны содержать пароль.

### Воспроизводимый CI на чистом Linux runner

GitHub Actions использует Go из `go.mod`, канонический `/opt/go-pet` и
доступный пользователю `/tmp/opencode`. Для ручного воспроизведения нужны Docker,
Go/C toolchain, Bash, jq, `python3`, curl, `flock`, GNU coreutils и `psql`. Следующие
команды — **только для одноразового runner** без существующего `petar_postgres`
и с незанятым `127.0.0.1:5432`. Не заменяйте рабочий контейнер.

```bash
cd /opt/go-pet
./scripts/provision-ci-postgres.sh
export PETAR_TEST_DB_DSN='postgres://petar_user:petar_password@127.0.0.1:5432/petar_test?sslmode=disable'
./scripts/ci-checks.sh
docker build --network host .
```

Provisioning проверяет SQL по опубликованному TCP: PG18 готов, `petar_test`
ещё отсутствует. Используются новый bind-каталог под `postgres/` и начальная
БД `postgres`, без initdb mount и без app. `ci-checks.sh` последовательно
выполняет `prepare`, `check`, настоящий gate с `GOFLAGS=-count=1` и
`go build ./...`; первый сбой прекращает цепочку. Сборка образа в Actions
зависит от этого job. Проверка fresh Compose bootstrap — отдельный job.

### Подготовка отдельной тестовой БД

Подготовка отдельная и явная. Gate и прямые тесты не создают/не ремонтируют БД.
При отсутствии DSN, ready-маркера или объектов схемы они завершаются с ошибкой,
не пропускают интеграционные тесты.

```bash
cd /opt/go-pet
export PETAR_TEST_DB_DSN='postgres://petar_user:petar_password@127.0.0.1:5432/petar_test?sslmode=disable'
./scripts/test-db.sh prepare
./scripts/test-db.sh check
GOFLAGS='-count=1' ./scripts/quality-gate.sh
```

`prepare` создаёт отсутствующую `petar_test`, ставит комментарий
`petar:test-db:lifecycle=v2;state=preparing`, применяет только up `000001`–`000008`
и после проверки ставит `petar:test-db:lifecycle=v2;state=ready`.
Повторный prepare готовой БД только проверяет её. Guard требует новую функцию,
отсутствие старой и наличие `word-count` v1 (не обязательно включённого).
БД с прежним маркером `v1` не принимается, не обновляется и не удаляется
автоматически. Для T28 используйте чистый одноразовый CI; обновление имеющейся
тестовой БД требует отдельного согласования под общим `flock`.

Существующая БД без маркера не «усыновляется»; частичная подготовка требует
отдельного явного reset. Маркер защищает от случайного удаления, не от атак.

**Reset уничтожает все данные только маркированной `petar_test`** и пересоздаёт
её тем же путём. Требуется точное подтверждение:

```bash
./scripts/test-db.sh reset --confirm petar_test
```

Reset неизвестной БД запрещён; активные соединения вызывают отказ.
Никакого DROP FORCE, завершения чужих сессий или остановки app.
App на `petar_db` продолжает работать; app на `petar_test` вызывает отказ.
Runner и app **никогда не должны использовать тестовую БД**. Systemd не нужен.

Gate под общим межпроцессным lock проверяет PG18, ready-маркер, таблицы,
extensions, функции и штатные триггеры, затем выполняет `go vet ./...` и
`go test -p=1 -count=1 -v -race -timeout 30s ./...`.
Только при успехе печатает `QUALITY GATE PASSED`.
Проверка объектов не доказывает идемпотентность/обратимость миграций.

Один advisory flock в `postgres/.petar-test-db.lock` удерживается на весь
lifecycle/gate или интеграционный test binary, включая cleanup. Поскольку
`go test` не наследует произвольные FD в test binaries, gate передаёт тот же
проверенный FD через приватный test-only Unix socket (`SCM_RIGHTS`), без
самоблокировки. Недоступный relay не разрешает обход lock; socket очищается
при завершении gate. Сам файл lock не удаляется.
Ожидание ограничено. Это кооперативная локальная защита, не распределённый lock:
нельзя запускать нескоординированные тесты из другого checkout/хоста, менять
конфигурацию app во время тестов или вручную обходить guards.
В частности, проверка маркера и `DROP DATABASE` — отдельные SQL-команды:
нескоординированный администратор может изменить/подменить БД между ними.
Reset допустим только при исключённом внешнем администрировании `petar_test`;
flock сериализует участников протокола, но не суперпользователя PostgreSQL.
Интеграционный spool остаётся в `t.TempDir()`, общий spool не очищается.

### Recovery drills

После `prepare`/`check` с тем же тестовым DSN, без app/runner на `petar_test`:

```bash
go test -p=1 -count=1 -v -race ./internal/spool \
  -run '^TestRecovery(DispatchWithoutFile|CommittedCallbackResponseLost)$'
```

- **Dispatch без файла:** тест делает pick без записи в spool; job/process —
  `dispatched/running`, активных файлов нет, повторный pick пуст. Адресный
  guarded UPDATE из варианта A возвращает тот же UUID в `pending`; штатные
  pick/write и один проход runner дают `completed`, правильный echo, один
  output и аудит двух dispatch и возврата.
- **Потеря ответа после commit:** loopback handler сначала фиксирует `completed`
  через штатную функцию БД, затем закрывает HTTP/1.1 соединение без 204.
  Runner завершается ошибкой, сохраняя `processing` и результат; SELECT уже
  показывает `completed` и один output. После архивирования только `processing`
  второй проход не отправляет callbacks и не меняет аудит, output или результат.

Оба drill выводят job UUID до и после восстановления, используют временный
spool и не затрагивают рабочие файлы. Они проверяют ручную сверку, а не exactly-once.

### Прямые race-тесты и SKIP LOCKED

Прямой запуск выбирает ту же БД и проверяет её до fixture-мутаций.
Штатно сериализуйте пакеты (`-p=1`); lock также защищает от независимых запусков:

```bash
GOFLAGS='-p=1 -count=1' go test -v -race ./...
go test -count=1 -v -race ./internal/db \
  -run '^TestPickNextJob(SkipsLockedRow|Concurrent)$'
```

Первый SKIP LOCKED тест удерживает row lock и доказывает захват другой задачи;
второй проверяет, что конкурентные захваты не получают один ID дважды.
Никаких остановки/возврата сервисов для этих тестов не требуется.

### Обратимость миграций: up → down → up

**Только пустая одноразовая БД `petar_migration_test`, без app/runner,
не рабочая `petar_db`.** Создание существующего имени должно завершиться ошибкой.
Все SQL-файлы остаются в `/opt/go-pet/postgres/migrations/`; не создавайте
SQL-файлы снаружи postgres и не используйте glob `*.sql`.

```bash
set -euo pipefail
cd /opt/go-pet
docker exec petar_postgres createdb -U petar_user petar_migration_test
migration() {
  docker exec -i petar_postgres psql -U petar_user -d petar_migration_test \
    -v ON_ERROR_STOP=1 < "/opt/go-pet/postgres/migrations/$1"
}
check_up() {
  docker exec -i petar_postgres psql -U petar_user -d petar_migration_test -v ON_ERROR_STOP=1 <<'SQL'
DO $$
BEGIN
  IF to_regclass('sm_inputs') IS NULL OR to_regclass('sm_processes') IS NULL
     OR to_regclass('sm_jobs') IS NULL OR to_regclass('sm_job_logs') IS NULL
     OR to_regclass('sm_outputs') IS NULL
     OR to_regprocedure('sm_create_input(text,jsonb)') IS NULL
     OR to_regprocedure('sm_validate_tool_payload(jsonb)') IS NULL
     OR to_regprocedure('sm_pick_next_job()') IS NULL
     OR to_regprocedure('sm_update_job_status(uuid,text,text,text)') IS NULL THEN
    RAISE EXCEPTION 'missing state machine objects';
  END IF;
  IF (SELECT count(*) FROM pg_extension WHERE extname IN ('vector','file_fdw','pg_trgm')) <> 3
     OR NOT EXISTS (SELECT FROM pg_foreign_server WHERE srvname='dokuwiki_files') THEN
    RAISE EXCEPTION 'missing extensions/server';
  END IF;
END $$;
SQL
}
migration 000001_init_extensions.up.sql
migration 000002_create_state_machine.up.sql
migration 000003_db_state_functions.up.sql
migration 000004_validate_tool_payload.up.sql
check_up
migration 000004_validate_tool_payload.down.sql
migration 000003_db_state_functions.down.sql
migration 000002_create_state_machine.down.sql
migration 000001_init_extensions.down.sql
docker exec -i petar_postgres psql -U petar_user -d petar_migration_test -v ON_ERROR_STOP=1 <<'SQL'
DO $$
BEGIN
  IF to_regclass('sm_inputs') IS NOT NULL OR to_regclass('sm_processes') IS NOT NULL
     OR to_regclass('sm_jobs') IS NOT NULL OR to_regclass('sm_job_logs') IS NOT NULL
     OR to_regclass('sm_outputs') IS NOT NULL
     OR to_regprocedure('sm_create_input(text,jsonb)') IS NOT NULL
     OR to_regprocedure('sm_validate_tool_payload(jsonb)') IS NOT NULL
     OR to_regprocedure('sm_pick_next_job()') IS NOT NULL
     OR to_regprocedure('sm_update_job_status(uuid,text,text,text)') IS NOT NULL
     OR EXISTS (SELECT FROM pg_extension WHERE extname IN ('vector','file_fdw','pg_trgm'))
     OR EXISTS (SELECT FROM pg_foreign_server WHERE srvname='dokuwiki_files') THEN
    RAISE EXCEPTION 'objects remained after down';
  END IF;
END $$;
SQL
migration 000001_init_extensions.up.sql
migration 000002_create_state_machine.up.sql
migration 000003_db_state_functions.up.sql
migration 000003_db_state_functions.up.sql
migration 000004_validate_tool_payload.up.sql
check_up
docker exec -i petar_postgres psql -U petar_user -d petar_migration_test -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
SELECT * FROM sm_create_input('migration-smoke','{"tool":"echo-test","message":"smoke"}'::jsonb);
SELECT id AS job_id FROM sm_pick_next_job() \gset
SELECT sm_update_job_status(:'job_id'::uuid,'completed','/spool/out/smoke/result.json',NULL);
SELECT j.status,p.state,o.artifact_type,o.metadata
FROM sm_jobs j JOIN sm_processes p ON p.id=j.process_id
JOIN sm_outputs o ON o.process_id=p.id WHERE j.id=:'job_id'::uuid;
ROLLBACK;
SQL
docker exec petar_postgres dropdb -U petar_user petar_migration_test
```

Повтор `000003` проверяет CREATE OR REPLACE, не идемпотентность `000002`.
При полном down `000002.down` удаляет таблицы с данными. Для проверки **только**
`000004` на одноразовой базе примените `000001`–`000003` и создайте контрольный
input с задачей. Выполните `000004.up → 000004.down → 000004.up`: один и тот же
новый `{"tool":"fabric"}` через `sm_create_input` должен дать соответственно
SQLSTATE `22023`, успешный ответ, снова `22023`. Контрольные ID должны
сохраниться на всех трёх шагах.

Для отдельной проверки `000005` создайте новую одноразовую БД тем же способом
и примените только `000001`–`000004.up` по порядку. Создайте и зафиксируйте
контрольный echo, завершите его задачу через `sm_update_job_status` после
диспетчеризации и сохраните ID и снимок input, process, job, аудита и output.
Затем, используя `migration` выше:

1. `migration 000005_action_catalog.up.sql`: проверьте три активные пары v1.
   Отключите `echo-test` v1 и убедитесь, что новый валидный echo отклоняется
   с `22023`, не создавая строк.
2. `migration 000005_action_catalog.down.sql`: `to_regclass('public.sm_actions')`
   возвращает `NULL`. Валидный echo снова принимается, но `message: null`
   по-прежнему даёт `22023`. Успешную пробу можно выполнить с `ROLLBACK`.
3. `migration 000005_action_catalog.up.sql`: снова три активные пары v1;
   отключение `echo-test` v1 снова блокирует ввод. Up восстанавливает seed,
   а не настройки, удалённые при down.

Сравните сохранённые строки по исходным ID после каждого перехода. После
каждого ожидаемого отказа проверьте отсутствие input с источником пробы и
неизменность количества input/process/job. Удалите только свою одноразовую БД.

Для `000006` в новой одноразовой БД примените `000001`–`000005.up`, создайте
legacy input и сохраните ID его цепочки. После `000006.up` проверьте v1 у
старого задания и создайте второй input через `sm_create_action`. После
`000006.down` проверьте отсутствие функций T24 и колонки, сохранность обеих
цепочек и каталога и рабочий legacy-вход. После повторного `000006.up` проверьте
v1 у сохранённых заданий и успешный вызов `sm_create_action`.

Для `000007` выполните `./scripts/check-word-count-migration.sh`. Скрипт в
отдельном контейнере PostgreSQL 18 и собственном PGDATA проверяет цикл up → down
→ up, сохранность старого задания и атомарный отказ down при наличии истории
`word-count`. Общие `petar_db`, `petar_test` и рабочий spool не используются.

Для `000008` отдельно запускайте обе изолированные репетиции (не параллельно
со штатным gate, долгий Go drill по умолчанию пропускается):

```bash
PETAR_T28_DRILL=1 go test -p=1 -count=1 -v -race -timeout 5m ./scripts \
  -run '^TestRetireLegacyInputMigration$'
bash scripts/check-retire-legacy-input.sh
```

Go-тест проверяет снимок строк и байтов до возобновления, up → down → up,
вызов с отдельной ролью, сохранение цепочек и отказ `22023` без вставок.
Вторая репетиция проверяет восстановление согласованного комплекта, HTTP,
возобновление старой очереди и новый submit. Оба стенда работают без рабочей БД
или рабочего spool; это не гарантия exactly-once за пределами проверенного сценария.

Проверки внешних systemd, proxy, DokuWiki и реальных CLI требуют отдельного
deployment-окружения; наличие примеров в README не означает, что они установлены.
