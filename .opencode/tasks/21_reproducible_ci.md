# T21 — Воспроизводимый CI и проверенное ручное recovery

**Решение.** В `todo.md` нет открытого шага; T16–T20 отмечены `[x]`. Следующий и последний пункт `.opencode/tasks/16_phase1_hardening_plan.md:144–163` — T21, P2. Используем уже принятые lifecycle, worker и runner. Результат одного коммита: чистый Linux CI проходит реальный gate и сборку, отдельно измерен fresh bootstrap, два сбоя воспроизведены с проверкой восстановления. Не менять runtime-контракты, схему, алгоритм доставки и другие проекты. Не устанавливать Fabric/systemd, не чистить рабочий spool. Изменения пользователя `.gitignore` и `.opencode/opencode.json` не включать.

## На что опираться

- `.github/workflows/ci.yml` уже существует: push/PR для main/master; `test` запускает `go test -v -race -cover ./...` без provisioning, DSN, prepare и vet. Зависимый `docker` уже собирает образ с `network: host`. Это не доказательство прохождения текущего workflow.
- `scripts/test-db.sh prepare|check` → `internal/db/test_lifecycle_test.go:runTestDBCommand`: явный список up 000001–000004, готовность маркированной `petar_test`; `scripts/quality-gate.sh` через ту же команду запускает vet и `go test -p=1 -count=1 -v -race -timeout 30s ./...` под общей блокировкой. Не реплицировать эти реализации в YAML. `internal/db/test_guard_test.go:checkTestDocker` сверяет SQL system_identifier с **именованным** работающим `petar_postgres`, проверяет состояние `petar_app`; анонимный Actions service сам по себе не удовлетворяет контракту.
- `testProjectRoot()`, lock и relay привязаны к `/opt/go-pet`; `scripts/test_db_test.go` использует `/tmp/opencode`. На свежем hosted runner обеспечить эти пути, права checkout-владельца, Docker, jq, curl, flock, GNU timeout и Go из `go.mod`. Не добавлять обходов guard для CI.
- `docker-compose.yml` монтирует одновременно up и down в initdb, а данные — в `postgres/data`. README, раздел «PostgreSQL и bootstrap миграций», уже различает fresh init и дальнейший `prepare`. Фактический исход запуска на пустом PGDATA в этом планировании не измерялся.
- `internal/spool/spool_test.go:TestSpoolWorkerIntegration` проверяет обычную публикацию; `scripts/spool_runner_test.go:TestRunnerCallback` подменяет curl и проверяет судьбу processing, но не commit в PostgreSQL. README, «Административный возврат в pending», уже содержит адресный UPDATE и архивирование. Нужны свидетельства по одному и тому же ID от состояния сбоя до согласованного конца, а не повтор этих unit-тестов.

## TDD и приёмка

1. **Первый RED:** создать `scripts/ci_checks_test.go`, `package scripts_test`, тест `TestCIChecksStopsOnPrepareFailure`. Запустить будущий `scripts/ci-checks.sh` как дочерний Bash в изолированной копии структуры проекта с подставными исполняемыми `scripts/test-db.sh`, `scripts/quality-gate.sh` и `go` в PATH. Prepare пишет вызов в журнал и выходит 17. Проверить, что prepare действительно вызван один раз, exit ненулевой и check/gate/build не запускались. Сейчас entrypoint отсутствует: ожидается RED на отсутствии вызова, а не зелёный тест на любой ненулевой exit. Сохранить реальный вывод до написания реализации.
2. **GREEN:** минимальный fail-fast entrypoint из корня своего checkout с явным `./scripts/test-db.sh prepare`. Затем happy-path тест `TestCIChecksRunsGateBeforeBuild`: передать тестовый DSN, проверить порядок `prepare → check → gate → go build ./...`, передачу DSN и `GOFLAGS=-count=1` в gate. Ожидаемый RED — неполная цепочка; добавить оставшиеся вызовы. Сценарий ошибки gate должен исключать сборку. Это быстрый тест оркестрации; приёмка YAML — реальным прогоном, не grep по тексту.
3. **CI-интеграция:** сохранить триггеры и зависимую Docker-сборку. На одноразовой Ubuntu VM дать checkout канонический адрес `/opt/go-pet` (например, symlink на `$GITHUB_WORKSPACE` при отсутствующем пути), создать `/tmp/opencode` с правами runner. Запустить изолированный `pgvector/pgvector:pg18` с именем `petar_postgres`, `POSTGRES_USER=petar_user`, паролем как в Compose, начальной БД `postgres` (не создавать `petar_test` entrypoint-ом), bind `127.0.0.1:5432:5432`, отдельным пустым bind-каталогом под `/opt/go-pet/postgres/` в `/var/lib/postgresql`. Без initdb mount, app и рабочего PGDATA. Это provisioning для gate, а не опыт с Compose ниже. Ограниченно ждать успешного SQL по опубликованному TCP после готовности (дедлайн, ошибка с диагностикой), передать канонический DSN из раздела команд и вызвать `./scripts/ci-checks.sh`. Локально не пытаться заменить контейнер с тем же именем. Проверка: свежий runner без подготовленной `petar_test`, успешные prepare/check, строка `QUALITY GATE PASSED`, `go build ./...`, успешная сборка образа. Если hosted run до коммита недоступен, показать эквивалентный чистый runner-прогон до sign-off и явно отделить его от результата GitHub; не выдавать проверку старого окружения за fresh.
4. **Fresh bootstrap — отдельный эксперимент до правки документации.** Независимый CI job/проверка с тем же образом, конфигом, командой postgres и read-only initdb mount миграций как в Compose. Смонтировать новый уникальный пустой PGDATA под `/opt/go-pet/postgres/` в `/var/lib/postgresql`, задать уникальное имя контейнера, без общей сети/порта приложения, без restart loop. Параметры роли/БД — как у Compose, это отдельный кластер. Дождаться финального сервера после initdb (не временного Unix socket) либо фиксировать первый сбой/таймаут. Записать exit/log, версию 18, результаты проверок README для extensions (`vector,file_fdw,pg_trgm`), таблиц, функций, триггеров и пробного `sm_create_input` в откатываемой транзакции при готовой схеме. До этого результат — гипотеза. Не применять up вручную до снимка исхода. Записать наблюдаемый baseline и проверку, отличающую его от произвольного отказа (сбой Docker, timeout или иной SQL error — не успешная проверка ожидаемого дефекта). Если нужен явный bootstrap — доказать рецепт README на отдельной чистой БД явными up, не повторять 000002 поверх готовых объектов. Down допускаются только внутри одноразового кластера. Удалять только собственные контейнер и данные после сбора свидетельств. Не «чинить» старые миграции в T21.
5. **Два recovery-drill: объявить до запуска** ID сценария, точку разрыва и доказательство ниже. Добавить тесты `TestRecoveryDispatchWithoutFile` и `TestRecoveryCommittedCallbackResponseLost` рядом с `TestSpoolWorkerIntegration` (`internal/spool`, использовать существующие `requireTestDSN`, `db.New`, pgxpool, writer и cleanup по уникальному source; никаких app/runner на `petar_test`). Под «runner» в тесте ниже понимается только ограниченный дочерний скрипт на `t.TempDir()` с тестовым loopback callback, а не сервис, подключённый к БД. Не запускать тесты параллельно; до pick убедиться в отсутствии чужих pending, сверить ID, не очищать чужие строки. Весь пакет должен укладываться в штатный gate timeout.
   - **После dispatch, до Rename:** создать echo input, вызвать штатный pick без записи файла (моделируем остановку между операциями), проверить `dispatched`, процесс `running`, in/processing пусты. Контроль до вмешательства: повторный pick не возвращает это задание. Выполнить по runbook один guarded UPDATE именно этого UUID, проверить одну строку и `dispatched → pending`, процесс `init`; затем штатные pick/write, тот же ID в `in`, один проход echo c подтверждёнными callback через тестовый HTTP handler и реальные функции БД. Критерий — completed у job/process, ожидаемые echo/result_path, один output, audit обоих dispatch и возврата; активных файлов нет.
   - **После commit completed, до доставки 204:** другой echo input, настоящий pick/write и скрипт в своём временном spool. Тестовый HTTP handler для in_progress подтверждает обновление 204; для completed вызывает `UpdateJobStatus`, дожидается успеха и **закрывает соединение без ответа** (loopback HTTP/1.1). Проверить одновременно: runner завершился ошибкой, processing/result сохранились, job/process completed, один output с верным путём. Ручное решение после SELECT: архивировать processing, оставить result на прежнем пути, **не requeue и не повторять callback**. Ещё один проход не делает вызовов и не меняет audit/outputs; активные in/processing пусты. Успех не выводить только из exit code. Для обоих drill сначала проверить наблюдаемое состояние сбоя; добавить шаг сверки/ручного действия в тестовый сценарий и runbook, затем показать конечные assertions GREEN. Если контракт штатных компонентов сразу GREEN, сохранить baseline; не ломать их ради RED. Первый настоящий RED относится к недостающему CI entrypoint.
6. README: коротко разделить provisioning CI, наблюдение fresh init и ручной recovery, с командами воспроизведения и ожидаемым доказательством каждого сбоя. В `architecture.md` уточнить измеренный bootstrap и исход потерянного ответа ровно настолько, насколько подтверждено. Сохранить ограничение MVP: ручная сверка, возможны повторные внешние эффекты, exactly-once не гарантируется.

## Порядок запусков и завершения

Все команды локально из `/opt/go-pet`, Bash; исправен Docker с проектным `petar_postgres`, loopback, инструменты runner, доступные `/tmp/opencode` и `postgres/`. Тестовые временные файлы spool — вне рабочего spool. Не использовать `DB_DSN` вместо тестового DSN.

```bash
cd /opt/go-pet
go test -count=1 -v ./scripts -run '^TestCIChecksStopsOnPrepareFailure$'  # после добавления теста, до кода
# После минимального изменения повторить тот же тест; затем следующий RED/GREEN:
go test -count=1 -v ./scripts -run '^TestCIChecks'
export PETAR_TEST_DB_DSN='postgres://petar_user:petar_password@127.0.0.1:5432/petar_test?sslmode=disable'
./scripts/test-db.sh prepare
./scripts/test-db.sh check
go test -p=1 -count=1 -v -race ./internal/spool -run '^TestRecovery(DispatchWithoutFile|CommittedCallbackResponseLost)$'
```

После отдельного fresh-bootstrap опыта прогнать на чистом runner `./scripts/ci-checks.sh` с тем же DSN и сборку образа (`docker build --network host .` для прямого запуска). Финальная локальная проверка: `go build ./...`, затем `GOFLAGS='-count=1' ./scripts/quality-gate.sh` (включает обязательные vet и race). Сохранить команды, выходы, состояния и ID drill, вывод fresh init и вид runner; не публиковать секреты. Ошибка инфраструктуры — блокер, не skip/PASS.

**Пункт для `todo.md`** (добавить один раз открытым):

- [ ] **T21: Воспроизводимый CI и ручное восстановление MVP.** Чистый PG18 проходит prepare/check, vet, сериализованный race без cache и сборку; отдельно проверены fresh bootstrap, dispatch без файла и потеря ответа после commit callback. План: `.opencode/tasks/21_reproducible_ci.md`.

**Review обязателен:** протокол T21 требует независимой проверки; передать diff и критерии, особо проверить изоляцию PGDATA/DSN, реальный gate и доказательство commit до потери ответа. После успешных проверок вызвать `review`, устранить существенные замечания и повторить затронутые проверки. Сохранить точную копию данного плана в `.opencode/tasks/21_reproducible_ci.md`; при необходимости только дописать «Фактически выполнено». Показать пользователю diff и фактические логи, дождаться Human Sign-off, только после приёмки закрыть T21 и сделать один коммит `ci: provision PostgreSQL and run reproducible integration checks`.

## Фактически выполнено

- Первый RED до реализации, внешний exit 1: `calls="" error=exit status 127`,
  `bash: …/scripts/ci-checks.sh: No such file or directory`. После минимального prepare
  первый тест прошёл; второй RED записал только `prepare` вместо всей цепочки.
  После реализации три `TestCIChecks*` прошли.
- Fresh Compose bootstrap: отдельные PGDATA/контейнер без портов. Первая
  предварительная попытка с каталогом `0700` не прошла (postgres не мог пройти
  mount root); её результат не принят за baseline миграций. С корнем `0755`
  контейнер `running`, `exit=0`, версия `18.6`; восемь файлов выполнились по имени,
  найдены `vector`, `file_fdw`, `pg_trgm`, `dokuwiki_files`, пять таблиц, четыре
  функции и три включённых триггера. `sm_create_input` вернул `new`; транзакция
  откатилась. Явный ручной bootstrap не требовался.
- Первый целевой race-прогон обоих drill после исправления ошибки компиляции
  прошёл с реальной `petar_test`. Dispatch `01a1ed47-9cf9-72af-9bb2-3d972e4dd839`:
  `dispatched/running`, пустой spool и пустой повторный pick; guarded requeue,
  тот же UUID, `completed/completed`, один output, ожидаемый аудит. Потеря ответа
  `01a1ed47-a5ba-7fd0-b41a-0b3304f1b173`: curl 52 после commit,
  сохранены processing/echo, завершённая job и один output; архивирование без
  POST/requeue, состояние и артефакт не изменились при втором проходе.
- Эквивалент чистого runner до коммита: Ubuntu 24.04.5, UID 1000, Go 1.23.2,
  отдельный Docker 29.9.3 (overlay2), пустой PGDATA, исходно нет `petar_test`.
  `prepare`/`check` прошли, `QUALITY GATE PASSED`, `go build ./...` и
  `docker build --network host -t petar:t21 .` успешны; image
  `sha256:35b4c894f4aa9661bd0b5e9d5988ce457b9267ed8aeaf32bf5b596d5538e5d6c`.
  Hosted GitHub Actions до коммита не запускался. Первые прогоны в WSL с
  параллельным I/O (в том числе вложенный Docker с `vfs`) дали инфраструктурные
  таймауты и не засчитаны. В последующем последовательном локальном
  `go build ./...` и `GOFLAGS=-count=1 ./scripts/quality-gate.sh` — PASS
  со штатными лимитами, включая vet и race.
- Независимый review обнаружил, что job fresh bootstrap не закреплял
  `/opt/go-pet`; исправлены workflow и проверка пути скриптом. Повторён реальный
  fresh bootstrap через symlink `/opt/go-pet` на checkout вне `/opt`, а также
  локально: `running`, `exit=0`, `ROLLBACK`, `FRESH BOOTSTRAP PASSED`.
  Повторный review: PASS.
