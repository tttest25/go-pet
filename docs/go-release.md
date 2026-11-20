# Выпуск Go API и комплекта runner/БД

Версия комплекта Go (например, `v0.1.0`) не совпадает с версией совместного
развёртывания. Образ, архив и метаданные должны ссылаться на один Git SHA.

## Выпуск

1. Убедитесь, что нужная ревизия опубликована в `master`, её CI прошёл, а
   версия свободна в Git, GitHub Releases и GHCR. До первого тега владелец
   включает **Immutable releases** и активирует ruleset для тегов `v*`,
   запрещающий их обновление и удаление, но не создание. Разрешение записи
   в GHCR оставляет доверенному release workflow и необходимым
   администраторам: проверка версии образа перед push не атомарна с записью.
   Выпускайте только после согласования настроек, версии и SHA. Создайте
   новый тег `vX.Y.Z` на этом SHA и отправьте только его; обновление тега
   не запускает публикацию.
2. Tag workflow снова запускает интеграционные проверки Go с PG18, чистый
   bootstrap, Docker-сборку и проверку архива. Затем публикует образ
   `ghcr.io/tttest25/go-pet` для `linux/amd64`, создаёт draft с тремя
   файлами, сверяет скачанные байты и только после этого открывает релиз.
   Перед записью в GHCR отдельный job с правами на Releases проверяет, что
   под этим тегом нет даже черновика.
3. Проверьте, что пакет GHCR имеет видимость **Public** (она задаётся
   отдельно от видимости репозитория), и выполните пробное скачивание из
   чистого окружения без авторизации. Не используйте версию, пока эта
   проверка не прошла.

При частичном сбое сначала сверьте тег, версию образа, digest и состояние
релиза/draft. Workflow не предназначен для перезаписи версии или
автоматического продолжения после частичной публикации. Не перемещайте
тег и не заменяйте образ/файлы; согласуйте дальнейшие действия по
зафиксированным результатам.

## Получение без Go и checkout

При одобренном выпуске `v0.1.0` в пустом каталоге проверки скачайте образ и
файлы. Для распаковки файлов PostgreSQL выбирайте отдельный каталог под
`/opt/go-pet/postgres/`, не PGDATA.

```bash
set -euo pipefail
version=v0.1.0
base="https://github.com/tttest25/go-pet/releases/download/$version"
archive="go-pet-$version.tar.gz"
for file in "$archive" SHA256SUMS release.json; do
  curl -q -fLsS "$base/$file" -o "$file"
done
sha256sum --check SHA256SUMS
test "$(sha256sum "$archive" | cut -d' ' -f1)" = "$(jq -er .archive_sha256 release.json)"
test "$(jq -er .version release.json)" = "$version"
test "$(jq -er .archive release.json)" = "$archive"
sha=$(jq -er '.git_sha | select(test("^[0-9a-f]{40}$"))' release.json)
image=$(jq -er '.image | select(test("^ghcr\\.io/tttest25/go-pet@sha256:[0-9a-f]{64}$"))' release.json)
docker pull --platform linux/amd64 "$image"
test "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image")" = "$sha"
tar -tvzf "$archive"
```

Устанавливайте именно `image` с digest из `release.json`, а не тег GHCR.
Сверьте `git_sha` с коммитом тега и с SHA успешного прогона CI. Для
аннотированного тега используйте SHA строки `refs/tags/<версия>^{}`.

В архиве разрешены только обычные файлы `scripts/spool-runner.sh` и
`postgres/bootstrap.sh` (оба `0755`), `postgres/postgresql.conf` (`0644`)
и пары `postgres/migrations/*.up.sql` / `*.down.sql` (`0644`) из дерева
этого SHA. Для `v0.1.0` ожидаются восемь пар от `000001` до `000008`.
До распаковки сверьте список с деревом тега; не принимайте лишние пути,
ссылки, специальные файлы или дубли имён. После проверки выполните
`tar --no-same-owner -xzf "$archive"` и проверьте `test -x` для обоих
скриптов. Этот документ не запускает API, PostgreSQL и runner.
