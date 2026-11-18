# T23 — API чтения каталога

**Результат.** Клиент получает список доступных версий действий одним `GET /api/v1/actions`. Следующее чтение отражает зафиксированные изменения каталога без перезапуска Go; отключение версии не скрывает ранее созданные задания.

**Почему сейчас.** В `todo.md` T23 — первый открытый шаг согласованной `.opencode/tasks/go_pet_actions_roadmap.md`. Его зависимость T22 закрыта в `todo.md` и подтверждена миграцией и тестами. Независимая готовность T24 не меняет порядка карты. Один шаг, один коммит: только чтение каталога, его тесты и документация HTTP-контракта. Не нужны миграция, новый пакет, изменение POST, runner, UI или управления доступностью.

### Исходное состояние и согласованный контракт

- `postgres/migrations/000005_action_catalog.up.sql`: `public.sm_actions`, PK `(action,version)`, `action` с коллацией `"C"`, `version integer`, `title`, `enabled`; начальные активные версии `echo-test`, `fabric`, `yt-dlp` — v1. `sm_create_input` проверяет именно v1. Наличие другой версии в списке не меняет контракт запуска.
- `internal/db/action_catalog_test.go`: `TestActionCatalogInitialEntries` читает исходные пары напрямую SQL; `TestActionCatalogDisabledRejectsInput` в транзакции уже сравнивает полный снимок input/process/job/logs/outputs до и после отключения. Это покрытие T22 сохранить; оно не проверяет HTTP.
- `internal/db/queue.go` содержит `ListJobs`, но метода чтения каталога нет. В `internal/api/server.go` нет маршрута или операции каталога в `databaseAPI`; `newServer`, `handleListJobs`, `writeDatabaseTimeout`, `writeJSON` задают образец. В `internal/api/server_test.go` есть `performRequest`, `requireJSONContentType`, `fakeDatabase` и `TestServerIntegration`; в `internal/api/timeouts_test.go` — `waitingDatabase`.
- Пользователь подтвердил: **200**, JSON-массив без обёртки, ровно `action` (строка), `version` (число), `title` (строка) на элемент; все и только активные пары, `ORDER BY action ASC, version ASC`, пустой ответ `[]`. Пример: `[{"action":"echo-test","version":1,"title":"Echo test"}]`. Без `enabled`, группировки, пагинации или выбора только последней версии. Общий HTTP-контракт ошибок БД: собственный бюджет 5 с → JSON 504 при ошибке, прочая ошибка → JSON 500 без внутренней причины в теле.
- Наличие объектов и отсутствие маршрута подтверждены по репозиторию. Готовность локального контейнера не проверялась. Ни RED, ни GREEN при планировании не запускались.

### TDD и приёмка

1. **Первым написать `TestListActionsReturnsEnabledVersions` в `internal/api/server_test.go`.** Взять настоящий `db.New` и `NewServer(...).Handler()` как в `TestServerIntegration`; DSN только через `requireTestDSN`. До изменений production-кода сделать GET через `performRequest` с тем же экземпляром сервера до и после SQL INSERT через отдельный verification pool. Изначально уникального имени (`t23-` + UUID) нет; вставить две активные версии, например 10 и 2 в таком порядке, с разными title, и ещё одну отключённую. Запрос после commit должен дать 200, штатный JSON Content-Type и массив: обе новые версии с точными заголовками в порядке 2, 10, отсутствует отключённая; проверить порядок всего списка по `(action,version)` и точный набор JSON-ключей независимо от production DTO. Использовать имя/версии фикстуры, не зашитый полный список каталога. Первая проверка статуса на неизменённом mux должна дать **404 вместо 200**, а не ошибку компиляции нового метода. Зафиксировать фактический RED.
2. **Минимальный GREEN для сквозного теста.** В `internal/db/models.go` добавить тип строки каталога без JSON-тегов; в `internal/db/queue.go` — `(*DB).ListActions(ctx context.Context)` с одним запросом `SELECT action, version, title FROM public.sm_actions WHERE enabled ORDER BY action ASC, version ASC`. Закрытие rows, проверка Scan/rows.Err, обёртки `%w` как в `ListJobs`. В `internal/api/server.go` добавить одноимённый метод интерфейса, method/path pattern и handler с отдельным response DTO, через существующий middleware. Fake в тестах привести в соответствие с интерфейсом. SQL — единственный источник списка; порядок обеспечивает БД.
3. **Пустой каталог и ошибки.** `TestListActionsEmpty`: fake возвращает пустую выборку; ожидать 200 и JSON `[]`, не `null`. При RED на `null` формировать срез ответа через `make` длины результата. `TestListActionsDatabaseErrors`: fake возвращает ошибку с контрольным приватным текстом → 500 и JSON `error` без этого текста; блокирующий fake ждёт `ctx.Done()` при коротком тестовом `server.databaseTimeout` → 504, при более ранней отмене родителя — не 504. Проверить передачу стандартного дедлайна по примеру `TestListJobsHasDatabaseDeadline`. GREEN — `WithTimeoutCause` от request context, `defer cancel`, проверка `writeDatabaseTimeout` только в ветке ошибки, иначе безопасный `writeError` и структурированный лог. Не ждать 5 с в тесте и не имитировать 504 простой подстановкой ошибки дедлайна.
4. **История через API, не только SQL.** `TestListActionsDisablePreservesJob`: на том же guard создать один `echo-test` через действующий POST с уникальным source, снять через `GET /api/v1/jobs?process_id=...` исходную запись (один job, сохранить ID и данные). Проверить присутствие `echo-test` v1 в GET каталога, адресно отключить эту пару в БД, зафиксировать изменение, повторить оба GET через тот же сервер. Каталог не содержит этой пары, исходный job по тому же процессу полностью совпадает. Если это уже GREEN после шага 2, зафиксировать как подтверждение, не менять рабочий код ради RED. При расхождении исправлять источник списка/фильтр, не изменять legacy-чтение истории. То же правило для других уже зелёных сценариев.
5. **Изоляция фикстур.** Не использовать `t.Parallel`. В обоих интеграционных тестах регистрировать `t.Cleanup` до мутаций, держать guard до конца cleanup, проверять результаты и ошибки очистки с отдельным ограниченным контекстом. Удалять только свои каталоговые строки и свой input (FK cascade); восстановить исходное значение `enabled` затронутой seed-пары, не переинициализировать весь каталог. HTTP читает через другое соединение — незакоммиченные изменения тестовой транзакции не видны. В конце прогнать исходные тесты T22, чтобы проверить отсутствие утечки фикстур. Кратко добавить маршрут, пример и ошибки в HTTP-раздел `architecture.md` и в описание API в README, если там перечислены маршруты.

### Порядок проверок Build

Рабочий каталог `/opt/go-pet`, доступный локальный PostgreSQL 18 `petar_postgres`; общая блокировка предоставляется штатным lifecycle и guard тестов. Все команды последовательно:

```sh
cd /opt/go-pet
export PETAR_TEST_DB_DSN='postgres://petar_user:petar_password@127.0.0.1:5432/petar_test?sslmode=disable'
./scripts/test-db.sh prepare
./scripts/test-db.sh check
# После добавления только первого теста — RED; после вертикальной реализации — повтор до GREEN:
go test -count=1 -v -run '^TestListActionsReturnsEnabledVersions$' ./internal/api
# При добавлении следующих сценариев, затем в полном составе:
go test -count=1 -v -race -run '^TestListActions' ./internal/api
go test -count=1 -v -race -run '^TestActionCatalog' ./internal/db
go vet ./...
go test -p 1 -count=1 -v -race ./...
GOFLAGS='-count=1' ./scripts/quality-gate.sh
go build ./...
```

Сбой lifecycle/DSN — блокер среды, не RED контракта. Не использовать `DB_DSN` приложения, `petar_db`, автоматический reset или отключение guard. Для старой схемы — сверить baseline с разделом README о подготовке `petar_test` до любых действий с БД.

### Учёт и завершение

В `todo.md` **обновить существующий T23 на месте**, оставить открытым до приёмки; соседние шаги не менять. Текст:

> - [ ] **T23: API чтения каталога.** `GET /api/v1/actions` возвращает активные пары из PostgreSQL с `action,version,title` по возрастанию имени и версии; пустой каталог → `[]`. Приёмка: новая версия видна, отключённая исчезает, ранее созданный job доступен без изменений. Зависит от T22. План: [.opencode/tasks/23_actions_read_api.md](.opencode/tasks/23_actions_read_api.md).

**Независимый review обязателен** по протоколу карты: особенно проверить реальное чтение из PostgreSQL, числовой порядок версий, публичный JSON, бюджет БД и восстановление seed после теста. Сохранить точную копию этого плана как `/opt/go-pet/.opencode/tasks/23_actions_read_api.md`; допускается только дописать после него «Фактически выполнено» с существенными отклонениями и результатами. Gate → Build сам вызывает `review` с diff и критериями → устраняет существенные замечания и повторяет затронутые проверки → показывает пользователю diff и фактические результаты, ждёт финальный Human Sign-off → отмечает T23 выполненным и делает один атомарный коммит `feat(api): list enabled action versions`. В коммит включить копию плана, даже если каталог игнорируется, но не включать посторонние изменения.

### Фактически выполнено

- `test-db.sh prepare` и `check` прошли. До изменения реализации `TestListActionsReturnsEnabledVersions` завершился с кодом 1: `GET /api/v1/actions status = 404, want 200; body: 404 page not found`. После добавления маршрута этот тест прошёл на той же тестовой БД.
- На первой реализации дополнительные сценарии зафиксировали `null` вместо `[]`, отсутствие 5-секундного дедлайна и `500` вместо `504` при истечении бюджета. Сценарий сохранения job уже проходил, legacy-чтение не менялось.
- Целевые тесты API и T22 с `-race`, `go vet ./...`, полный `go test -p 1 -count=1 -v -race ./...` и `go build ./...` прошли. Первый запуск quality gate достиг штатного 30-секундного лимита пакета `internal/db` на `TestLifecycleLockInheritanceAndContention`, когда остальные пакеты прошли.
- Повтор с `GORACE=atexit_sleep_ms=0` также достиг лимита (`TestLifecycleRealGoRunnerPreservesLock`); с дополнительным `GOMAXPROCS=4` тот же gate напечатал `QUALITY GATE PASSED` (`internal/db`: 29,400 с), затем `go build ./...` прошёл. Скрипт gate, его дедлайн, `-race` и блокировка БД не менялись.
- Независимый `review`: `PASS`, без существенных замечаний; правки кода после review не потребовались.
