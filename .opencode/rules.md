# Coding & Engineering Rules: Project `petar`

## 1. Environment & Infrastructure Rules
* **OS & Shell:** Ubuntu 20.04 WSL, оболочка **Zsh**.
* **Project Root:** `/opt/go-pet` (Go module: `petar`).
* **Database Isolation:** Все файлы PostgreSQL 18 (конфиги, миграции, том данных) хранятся **строго** в `/opt/go-pet/postgres/`. Запрещено создавать SQL-файлы или папки миграций вне `/opt/go-pet/postgres/`.
* **Spool Directories:** Находятся в `/opt/go-pet/spool/{in,processing,out}`.

## 2. Go 1.23+ Standards (Pragmatic Dumb Executor)
* **Никакого оверинжиниринга:** Запрещено плодить пустые интерфейсы и слои `domain -> usecase -> adapter -> repository`. Код делится на 4 понятных пакета: `config`, `db`, `spool`, `api`.
* **Драйвер БД:** Только `github.com/jackc/pgx/v5/pgxpool` (без GORM, sqlx и тяжелых ORM).
* **Роутер HTTP:** Только стандартная библиотека `net/http` с синтаксисом маршрутов Go 1.22+ (`mux.HandleFunc("POST /api/v1/jobs/{id}/status", ...)`).
* **Контекст и завершение:** Любая функция ввода-вывода (БД, HTTP, ожидание канала) обязана принимать `ctx context.Context` первым аргументом. Обязателен Graceful Shutdown по `SIGTERM` / `SIGINT`.
* **Обработка ошибок:** Каждая ошибка оборачивается с контекстом через `fmt.Errorf("spool write job %s: %w", jobID, err)`. Никаких `panic` в рантайме.
* **Логирование:** Только стандартный структурированный логгер `log/slog` (JSON-формат в stdout).
* **Атомарность файловых операций:** Запись в `/spool/in/` выполняется строго через запись в `{id}.tmp` -> `file.Sync()` -> `file.Close()` -> `os.Rename("{id}.tmp", "{id}.json")`.

## 3. PostgreSQL 18 Specifics
* Использовать встроенный `uuidv7()` для всех `UUID` первичных ключей.
* Использовать `RETURNING OLD.status, NEW.status` там, где требуется возврат перехода состояния.
* В Docker Compose том данных монтируется в `/var/lib/postgresql` (не в `/var/lib/postgresql/data`).
* Параметр `io_method` в WSL2 установлен в `'worker'`.

## 4. Workflow & TDD Protocol for CLI Agent
1. **Strict TDD:** Сначала пишется тест (`*_test.go`), проверяющий контракт (включая интеграционные тесты с работающим контейнером `petar_postgres`), затем код реализации.
2. **Проверка перед коммитом:** Каждый шаг обязан проходить `go vet ./...` и `go test -v -race ./...`.
3. **One Step = One Commit:** Выполняется строго один пункт из `todo.md` за итерацию. После прохождения тестов делается атомарный коммит (`git commit -m "..."`).
4. **Согласование:** Перед изменением схемы БД или созданием нового пакета выведи краткий план и дождись подтверждения, если иное не указано пользователем.
