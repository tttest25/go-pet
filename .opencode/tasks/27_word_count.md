# T27 — word-count v1: backend и доказательство расширяемости

Добавить действие, которое считает слова в `data.text` и публикует их количество. Клиент обнаруживает действие в каталоге, отправляет обычный POST и читает результат по job ID; для этого не нужны специальные HTTP-обработчики или изменения wiki.

## Приоритет и статус

`todo.md`: T27 — единственный открытый пункт; T22–T26 закрыты. Источник зависимостей: T27 в `.opencode/tasks/go_pet_actions_roadmap.md`. Согласовано: сейчас принять и закоммитить **backend T27**; весь T27 оставить открытым до фактического wiki E2E после PHP-06/07. Один шаг, один коммит. Не реализовывать PHP, универсальную систему плагинов или новые маршруты. Пользователь согласовал изменение схемы и условия отката; это не финальный sign-off.

## Что подтверждено чтением репозитория

- `postgres/migrations/000006_universal_json_input.up.sql`: `sm_create_action` уже принимает v1, отвергает необъект и подмену `tool`, вызывает `sm_create_input`. Та проверяет валидатор и активность до записи (`000005_action_catalog.up.sql`). Поэтому тот же валидный `word-count` будет доступен через legacy-вход; `source` и остальные поля не переписывать сверх существующего контракта.
- В `000004_validate_tool_payload.up.sql` ветка ELSE выдаёт `22023`; в `000005` нет seed новой пары; в `000002_create_state_machine.up.sql` именованный `sm_jobs_tool_name_check` разрешает только три имени. Одного INSERT в каталог недостаточно. Триггер создания job уже переносит `payload.tool`.
- В `scripts/spool-runner.sh` есть проверка версии, `validated_tool`, выполнение и `prepare_public`, но нет ветвей `word-count`. Публикация до callback и выдача `public.json` по job ID уже реализованы. На стороне Go менять production-код для нового типа не требуется.
- Опоры для тестов: `internal/db/create_action_test.go` (`TestCreateActionContract/enqueue`, подсчёт строк, savepoint для отказов), `internal/db/action_catalog_test.go`, `scripts/spool_runner_test.go` (`TestRunnerPublicEcho`, `TestRunnerUnsupportedActionVersion`, `TestRunnerStalePublicResult`), `internal/db/test_lifecycle_test.go` (явный порядок up), `scripts/check-fresh-bootstrap.sh` (ровно три активные пары и откатываемая проба).
- Это статическая проверка; RED, GREEN, состояние запущенной БД и wiki E2E на этапе Plan не наблюдались.

## Приёмка и TDD по порядку

1. **Первый RED — enqueue реальным PostgreSQL.** В таблицу положительных случаев `testActionEnqueue` добавить `word-count`, `{"text":"один два три"}`, ожидаемый payload с доверенным `"tool":"word-count"`; в SQL проверки цепочки включить `j.action_version=1`. Прогнать только новый subtest на проверенной схеме `000006`. Ожидается конкретно ошибка `unsupported payload.tool` (`22023`), а не ошибка подключения, пропуск теста или сбой подготовки.
2. **GREEN БД.** Добавить только новую пару `postgres/migrations/000007_word_count.{up,down}.sql`; исторические миграции не редактировать. Up добавляет активную пару `(word-count,1)` с title `Word count`, расширяет именованный CHECK на одно имя и валидатор на одну ветвь: `text` обязательно JSON-строка (включая пустую); дополнительные поля остаются без нормализации. Остальные правила идентичны `000004`. Не менять функции T24, ни цепочку, ни схему HTTP. На up должна создаться ровно одна pending-цепочка с v1 и исходным текстом.
3. **Границы и каталог.** В той же SQL-таблице отказов проверить отсутствующий `text`, JSON null, число, массив, объект и boolean; отключённую v1, v2, попытку подмены `tool`. Результат — `22023` и нулевые дельты всех трёх таблиц. Добавить положительные `""` и строку из разделителей; проверить через legacy один валидный input. В тесте каталога/HTTP ожидать четыре начальные пары в порядке `echo-test, fabric, word-count, yt-dlp`; отключение скрывает запись, не историю. Зафиксировать соответствующие ожидания до их реализации; часть отрицательных тестов пройдёт сразу благодаря общим проверкам — не создавать искусственный RED. Обновить только действительно устаревшие seed-ожидания, включая проверку готовности тестовой схемы; не ослаблять сравнения до «хотя бы три».
4. **Runner RED → GREEN.** Добавить `TestRunnerWordCount` по образцу `TestRunnerPublicEcho`: канонический job, реальный скрипт, изолированный spool и перехват callback. Сейчас ожидается `failed` вместо `completed`. Разрешить v1 в существующем выборе с точным совпадением имени и строковым `.payload.text`. Посчитать один раз из декодированного JSON, без интерполяции текста в shell/argv, locale и внешнего word-count CLI (python3 уже нужен runner). Слово — максимальная последовательность символов вне Unicode White_Space; не делить по пунктуации/дефисам. Использовать точную семантику разделителя (например, `str.isspace()` за исключением U+001C…U+001F, которые Python дополнительно считает пробельными). Записать число во внутренний `result.json`, построить из него публичную оболочку существующим механизмом атомарной публикации.
   - При `in_progress → completed` уже на момент терминального callback на диске ровно `{"type":"word-count","version":1,"data":{"words":3}}` для примера карты (сравнение по JSON, число, без служебных полей); стандартный `result_path`, подтверждение 204, `processing` очищен. Пустой/только whitespace → 0; соседние пробелы, LF, TAB, NBSP, Unicode EM SPACE отделяют слова; `раз-два,` → 1. Проверить разделители и на границах строки.
   - Подложенный нестроковый `text`, несовпадающий payload.tool или v2 → `failed`, без `public.json`, включая повтор со старым результатом. Сохранить T25-правила отсутствующей версии и неподтверждённых callback. GREEN только через общую проверку/публикацию, не через обход callbacks.
5. **Откат и bootstrap — тест до корректировки lifecycle.** На отдельной одноразовой PG18 БД того же образа проверить `000001…000006 up → 000007 up → down → up`. До первого up создать старый job и сохранить ID. После down должны остаться старый job, прежний CHECK и рабочие три правила; v1 word-count отсутствует и не создаёт цепочку. После повторного up действие снова работает. Затем оставить его job и отдельно попытаться выполнить down: ожидается понятная ошибка, без удаления job, отключения пары или частичного отката, после ошибки новый word-count всё ещё создаётся. Реализовать down с защитой **до мутаций**, под блокировкой от конкурентной вставки job и в одной транзакции; никогда не удалять историю. Удалять из каталога только собственную пару. В clean initdb файлы `down` исполняются **до одноимённых up**: проверить этот путь, не требовать существования новой пары в down. Не запускать rollback drill на общей `petar_test` или `petar_db`.
   Добавить проверку такого цикла в одноразовый контур (например, `scripts/check-word-count-migration.sh`, psql с `ON_ERROR_STOP=1` и транзакцией на файл, собственный PGDATA под `/opt/go-pet/postgres/`, cleanup только своего контейнера и каталога). В `TestLifecycleMigrationOrder` и списке подготовки добавить `000007` последней; при проверке schema baseline отличать наличие миграции от текущего операторского флага `enabled`. В fresh bootstrap проверить точные четыре активные пары и реальный откатываемый enqueue word-count, сохранив legacy-пробу. В README и architecture обновить затронутые перечни, результат, инструкции обновления и границу down.
6. **Доказательство backend без правок HTTP.** Добавить целевой integration-тест `TestWordCountBackend` на выделенной `petar_test` и `t.TempDir()` для spool: реальный router/PG, общий dispatch/writer, реальный runner и его HTTP callbacks, без работающего production worker или хостового runner на этом spool. `GET actions` содержит новую пару; POST `{"action":"word-count","version":1,"data":{"text":"один два три"}}` → 201; найти **job ID**, не input ID, через GET процесса; после dispatch и одного прохода runner GET результата этого job → 200 и точная оболочка; неверный тип → HTTP 400 без новой цепочки. До реализации runner этот путь не может получить 200 с `words:3`; после изменений он должен пройти с неизменёнными `internal/api` production handlers и wiki. Допустимо использовать существующий guarded API integration harness и `httptest` с настоящим callback URL. Тест должен быть сериализован, ограничен по времени и не забирать чужую очередь. Обычный unit-тест handler с заранее положенным `public.json` не заменяет этот сценарий.

## Запуск и порядок завершения

Из `/opt/go-pet`, с Go проекта, `petar_postgres` PG18 на loopback, Docker, bash, jq, python3, GNU coreutils, curl и flock; образ PG18 доступен локально. Не подставлять `DB_DSN` приложения; рабочие данные и spool не использовать. Не запускать конкурирующие проверки на одной БД.

```bash
cd /opt/go-pet
export PETAR_TEST_DB_DSN='postgres://petar_user:petar_password@127.0.0.1:5432/petar_test?sslmode=disable'
./scripts/test-db.sh prepare
./scripts/test-db.sh check
# После добавления первого теста, до смены baseline:
go test -v -p=1 -count=1 -race ./internal/db -run '^TestCreateActionContract$/^enqueue$/^word-count$'
```

После записи миграции и изменения lifecycle на уже проверенной `000006` штатный prepare **не обновляет** готовую БД. Проверить цель, ready-маркер, baseline и отсутствие иных пользователей под общим lock по README; применить ровно новый up к маркированной тестовой БД. На новой пустой выделенной среде вместо ручного up использовать новый `prepare`. При неоднозначном состоянии остановиться, не делать reset. Для подтверждённого обновления:

```bash
flock -x /opt/go-pet/postgres/.petar-test-db.lock docker exec -i petar_postgres psql -X -1 -v ON_ERROR_STOP=1 -U petar_user -d petar_test < postgres/migrations/000007_word_count.up.sql
./scripts/test-db.sh check
go test -v -p=1 -count=1 -race ./internal/db -run 'TestCreateActionContract|TestActionCatalog|TestCreateInputPayloadContract|TestLifecycleMigrationOrder'
go test -v -p=1 -count=1 -race ./scripts -run 'TestRunnerWordCount|TestRunnerUnsupportedActionVersion|TestRunnerStalePublicResult'
go test -v -p=1 -count=1 -race ./internal/api -run 'TestWordCountBackend|TestCreateInputPayloadIntegration'
./scripts/check-word-count-migration.sh
./scripts/check-fresh-bootstrap.sh
go build ./...
GOFLAGS=-count=1 ./scripts/quality-gate.sh
git diff --check
```

Целевые тесты каждого следующего сценария запускать сначала с новым ожиданием, затем после минимального изменения. Финальный штатный gate под общим lock включает `go vet ./...` и `go test -p=1 -count=1 -v -race -timeout 30s ./...`; не заменять его параллельным обходом. Проверить также новые файлы в diff для review. Сохранить фактические результаты команд и причину первого RED.

**todo.md — обновить существующую строку, не добавлять дубль.** До приёмки формулировка:

> - [ ] **T27: Доказательство расширяемости на word-count.** Backend: каталог → POST → runner → GET по job ID, `{"text":"один два три"}` → `data:{"words":3}` без правок HTTP-обработчиков и wiki. Сквозная wiki-приёмка после PHP-06/07. План: [.opencode/tasks/27_word_count.md](.opencode/tasks/27_word_count.md).

При sign-off backend отразить «backend принят; ожидается wiki E2E», сохранив `[ ]`; до подтверждения использовать формулировку без заявления о приёмке. В `[x]` переводить только после отдельной фактической wiki-проверки.

**Завершение:** точная копия этого плана в `.opencode/tasks/27_word_count.md`; при необходимости только дополнение «Фактически выполнено» после исходного текста. Независимый `review` обязателен по протоколу дорожной карты: проверить особенно атомарный отказ down с историей, оба порядка bootstrap, подсчёт Unicode и сквозную выдачу через неизменённый API. После успешных проверок Build сам передаёт агенту review полный diff и приёмку; исправляет существенные замечания, повторяет затронутые проверки и gate при изменении реализации. Показать пользователю diff, результаты, итог review и открытый статус wiki; дождаться явного Human Sign-off **до** одного атомарного коммита backend T27.

## Фактически выполнено

- На PostgreSQL 18.6 с проверенной схемой `000006` первый запуск `TestCreateActionContract/enqueue/word-count` завершился RED: `ERROR: unsupported payload.tool (SQLSTATE 22023)`. На `000007` тот же subtest прошёл.
- До правки runner `TestRunnerWordCount/example` получил `in_progress → failed` вместо `completed`, а `TestWordCountBackend` с уже обновлённой БД получил HTTP 409 вместо 200. После исправления оба прошли; результат по job ID — `{"type":"word-count","version":1,"data":{"words":3}}`.
- При обновлении выделенной `petar_test` `psql` вызван с `-f -` для применения `-1` к stdin; проверка цели, baseline, ready-маркера и отсутствия других подключений выполнена под тем же `flock` до up. Рабочая БД и spool не менялись.
- `test-db.sh check/prepare`, целевые race-тесты БД, runner и API, `go build ./...` и `GOFLAGS=-count=1 ./scripts/quality-gate.sh` прошли. Одноразовые проверки вернули `WORD-COUNT MIGRATION PASSED` (в том числе ожидаемый отказ down с сохранением истории) и `FRESH BOOTSTRAP PASSED` (initdb выполнил `000007.down` перед `000007.up`).
