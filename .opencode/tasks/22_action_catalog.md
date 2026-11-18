# T22 → Build: каталог действий PostgreSQL

Один открытый пункт T22 из `todo.md`, один коммит после приёмки. Первый тест — `TestActionCatalogInitialEntries`, второй — `TestActionCatalogDisabledRejectsInput`. Ни API чтения каталога, ни нового формата запроса, ни изменений runner в этом шаге нет. Ниже — команды для Build; результаты ещё не получены.

## 1. RED: каталог на подготовленной 000004

Из `/opt/go-pet`, при работающем `petar_postgres` PG18, до изменений миграций и schema guards:

```bash
export PETAR_TEST_DB_DSN='postgres://petar_user:petar_password@127.0.0.1:5432/petar_test?sslmode=disable'
./scripts/test-db.sh prepare
./scripts/test-db.sh check
```

Использовать штатный guard/lock пакета `db_test`, без параллельных клиентов, app/runner или общего spool на тестовом контуре. В `internal/db/action_catalog_test.go` добавить только первый тест: выбрать `action, version` из `public.sm_actions WHERE enabled ORDER BY action COLLATE "C", version`; сравнить результат с `[(echo-test,1),(fabric,1),(yt-dlp,1)]`, проверить ошибку итератора. Тест сам не создаёт таблицу и не вставляет seed.

```bash
go test -p=1 -count=1 -race ./internal/db -run '^TestActionCatalogInitialEntries$'
```

Принять RED только из запроса: таблица отсутствует (`42P01`). Ошибки DSN, Docker, ready-маркера или baseline-схемы — проблемы подготовки.

## 2. GREEN: минимальная 000005; затем сразу второй RED

- Добавить пару `postgres/migrations/000005_action_catalog.{up,down}.sql`. На первом GREEN up создаёт `public.sm_actions(action text COLLATE "C", version integer CHECK (version > 0), title text NOT NULL CHECK (btrim(title) <> ''), enabled boolean NOT NULL, PRIMARY KEY (action,version))` и три именованные активные записи v1. Down — `DROP TABLE IF EXISTS public.sm_actions` без `CASCADE`. Не менять старые миграции, `CHECK` имени инструмента у jobs, триггеры или существующие строки. Это промежуточная редакция до коммита.
- **Обновление ready-базы:** `runTestLifecycle` в `internal/db/test_lifecycle_test.go` на существующей ready `petar_test` вызывает только `store.check`, в том числе при `prepare`. Это не мигратор. До изменения guard зафиксировать успешный baseline `check`. Перед DDL сверить тестовую цель с `petar_postgres`, роль `petar_user`, PG18, комментарий `petar:test-db:lifecycle=v1;state=ready`, наличие валидатора и схемы 000004, отсутствие `sm_actions` и других клиентов. Применить **только** 000005.up под тем же lock. Например, при подтверждённых условиях для канонического локального DSN:

  ```bash
  flock -x /opt/go-pet/postgres/.petar-test-db.lock \
    docker exec -i petar_postgres psql -X -1 -v ON_ERROR_STOP=1 -U petar_user -d petar_test \
    < postgres/migrations/000005_action_catalog.up.sql
  ```

  Использовать для промежуточных GREEN только выделенную тестовую БД этой итерации. Для уже заполненной 000004, которую нужно сохранить, применять этим способом **готовую** 000005 один раз после второго GREEN. При расхождении схемы остановиться и взять чистый изолированный контур: не повторять 000001–000004, не делать автоматический reset. Повторить первый тест — PASS.
- Написать `TestActionCatalogDisabledRejectsInput`: все SQL через один `pgx.Tx`, гарантированный внешний `ROLLBACK`. Создать валидный контрольный echo через `sm_create_input`, подтвердить ровно один связанный process/job. Для содержательной истории перевести именно этот job в допустимое исходное состояние для callback и завершить через `sm_update_job_status`; проверить наличие audit и output. Снять снимок строк контрольного input/process/job, логов и outputs по их ID (в стабильном порядке) и количества строк пяти таблиц **до отключения**.
- `UPDATE public.sm_actions SET enabled=false WHERE action='echo-test' AND version=1 AND enabled` должен изменить одну строку. **После UPDATE** создать `SAVEPOINT attempt`; через `tx.QueryRow(...).Scan(...)` вызвать `sm_create_input` с новым source и валидным `{"tool":"echo-test"}`. Требовать `errors.As(...,*pgconn.PgError)` с `Code=="22023"`. Успех вызова — немедленный провал, даже если внешний rollback впоследствии уберёт строки. После ожидаемой ошибки — `ROLLBACK TO SAVEPOINT attempt`; убедиться, что действие всё ещё отключено, новый source отсутствует, количества и полный снимок истории равны исходным. Проверки выполнять до внешнего rollback.

  ```bash
  go test -p=1 -count=1 -race ./internal/db -run '^TestActionCatalogDisabledRejectsInput$'
  ```

  Ожидаемый второй RED: функция из 000004 приняла корректный payload, несмотря на отключение.

## 3. GREEN: доступность в штатном пути создания

Дополнить тот же 000005.up: `CREATE OR REPLACE` для `public.sm_create_input(text,jsonb)` с прежними именами аргументов и `RETURNS TABLE(input_id uuid, process_id uuid, status text)`. Оставить первым `PERFORM public.sm_validate_tool_payload(p_payload)` из 000004; перед `INSERT INTO sm_inputs` проверить точное `action=p_payload->>'tool'`, `version=1`, `enabled` в той же транзакции. Нет активной строки → `RAISE ... USING ERRCODE='22023'`. Не выбирать максимальную версию. При успехе оставить прежнюю вставку, триггер и возврат результата. Не переписывать payload или дефолты.

В окончательном down **сначала** восстановить тело `sm_create_input` из **000004.up**, включая валидатор, **затем** удалить только таблицу каталога. `000004.down` вернул бы поведение до валидации и здесь неприменим. На собственной промежуточной TDD-базе без пользовательских настроек применить окончательные down и up в этом порядке под тем же lock с `ON_ERROR_STOP` (для каждой команды подставить соответствующий файл в команду выше); не накатывать `CREATE TABLE` повторно. Оба целевых теста должны стать зелёными.

## 4. Lifecycle, bootstrap и обратимость

1. Сначала расширить ожидаемый список в `TestLifecycleMigrationOrderIncludesPayloadValidation`, увидеть RED, затем дописать `000005_action_catalog.up.sql` последним в `testMigrationNames`. Обновить `checkTestSchema` во всех трёх `internal/{db,api,spool}/test_guard_test.go`: отсутствие `public.sm_actions` должно ломать `check` на уровне схемы. Проверку начальных активных значений держать отдельно от проверки структуры: выключение записи — допустимая операция. Проверить `./scripts/test-db.sh check` на обновлённой БД и `prepare`/`check` на изначально отсутствующей `petar_test` отдельного штатного CI-контура.
2. `scripts/check-fresh-bootstrap.sh` проверяет **реальный Docker entrypoint**, не список lifecycle: Compose монтирует весь каталог, в том числе down перед up одного номера. Поэтому 000005.down должен выполняться после 000004.up даже при ещё отсутствующем каталоге. В скрипте усилить обязательный `schema_ok` проверкой таблицы и трёх точных активных пар; сохранить rollback-пробу `sm_create_input` с единственным pending echo. Проверка должна завершаться ошибкой, а не только печатать результат SELECT.
3. Отдельно от `petar_test` и рабочего `petar_db` провести drill на **новой одноразовой БД** по процедуре README «up → down → up». В одноразовом контуре определить `migration` как в README (psql с `ON_ERROR_STOP` в целевую временную БД). Начать только с 000001–000004.up; создать и зафиксировать контрольную legacy-цепочку, записать ID и снимок истории. Затем:

   ```bash
   migration 000005_action_catalog.up.sql
   # проверить seed, отключить echo v1, получить 22023, сравнить историю
   migration 000005_action_catalog.down.sql
   # to_regclass('public.sm_actions') IS NULL; валидатор на месте;
   # валидный echo снова принят, явно null message всё ещё даёт 22023
   migration 000005_action_catalog.up.sql
   # снова три активные v1 и работающий запрет после отключения
   ```

   После каждого перехода сравнить исторические строки по исходным ID; после отказов проверить отсутствие добавленных цепочек. Завершить очисткой только собственной одноразовой БД. Down удаляет данные самого каталога, включая изменённый флаг; повторный up восстанавливает seed, не прежние настройки.
4. Актуализировать в `README.md` порядок up, проверку объектов, адресное обновление ready 000004 и drill; в `architecture.md` — таблицу, диапазон миграций и точку проверки. Не заменять историческую проверку обратимости 000004 проверкой 000005.

## 5. Команды приёмки и передача человеку

```bash
./scripts/test-db.sh check
go test -p=1 -count=1 -race ./internal/db -run 'TestActionCatalog|TestCreateInputPayloadContract|TestLifecycleMigrationOrder'
go test -p=1 -count=1 -race ./internal/api -run '^TestCreateInputPayloadIntegration$'
./scripts/check-fresh-bootstrap.sh
go build ./...
GOFLAGS=-count=1 ./scripts/quality-gate.sh
git diff --check
```

- Legacy `POST /api/v1/inputs` по-прежнему принимает `source,payload.tool`; результат `201`, `id=input_id`, `process_id`, `status:"new"`. Проверить матрицу трёх инструментов, echo без `message`, сохранение дополнительных полей, неизвестный tool и невалидный payload (`22023` → `db.ErrInvalidInput` → HTTP 400). Доступность решает SQL, не Go. Для отказов нет новых input/process/job. Полный gate включает `go vet ./...` и сериализованный `go test -p=1 -count=1 -v -race -timeout 30s ./...`.
- **Обязательный независимый `@review`** после gate: дать полный diff, включая новые файлы; проверить обе причины RED, seed/ключ, точку отказа, доказательство сохранности истории, обновление 000004 без потери данных, fresh bootstrap, down и legacy-контракт. Замечания исправить, повторить затронутые проверки и gate, получить review PASS.
- **Human Sign-off:** показать пользователю diff, RED/GREEN, результаты bootstrap, drill, build, gate и review. Дождаться явного подтверждения. Только затем отметить существующий T22 в `todo.md` и сделать один атомарный коммит `feat(db): add versioned action catalog`. При блокере или без подтверждения T22 остаётся открытым.
