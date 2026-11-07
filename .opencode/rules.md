# Project Context: petar
- Корень проекта: /opt/go-pet
- Модуль Go: petar (Go 1.23+)
- База данных: PostgreSQL 18 (Docker контейнер petar_postgres, БД petar_db, пользователь petar_user)
- Конфиги и миграции базы: строго в /opt/go-pet/postgres/
- Каталог миграций: /opt/go-pet/postgres/migrations/
- Подход: строгий TDD (Red -> Green -> Refactor).
- Правило взаимодействия: перед созданием файлов показывай схему/план на утверждение человеку.
