---
name: fd-update
description: Bring the server, plugin, and DB up to date. "갱신" · "업데이트" · "최신인지 확인" · "새 버전 나왔다" · use when the board is stale or cannot read branches.
---

# Update

Staleness has three axes — **plugin · server · DB**. They go stale separately; fix one and the rest stay as they were.

## 1. Measure what is stale

```
fd status | head -1
fd doctor
claude plugin list
```

**The first line of `fd status` is the verdict.** It must read `파생 git@…` for the server to be reading git.
If it reads `파생 db@…` or `브랜치 ?(못 읽음)`, **that server's overlaps, branches, and footprints are all
untrustworthy** — it is a target to update, not a source to consult.

**Do not use `ok=true` from `/healthz` as a freshness verdict.** A server months old and a server that cannot
read git both return `ok` alike. It says only "it is up".

## 2. There is an order

**Push → marketplace → plugin → container.** The marketplace is a git remote, so if you bump the version in
`plugin.json` and **do not push, the update never arrives at all.** Skip the order and you reinstall the old
build and believe it is current.

## 3. Ask the new build before you change anything

```
fd selfcheck --db ~/.flightdeck/fd.db
```

It answers only **may this be restarted** — does that binary run, is the DB migration plan not a refusal.
**Ask this first when rolling back too.** If the DB version is higher than that build knows, a refusal comes back.
(If the build is old enough to print `모르는 명령: selfcheck`, that build's `serve` stops at startup for the same reason.) **On a Postgres ledger** (the selection file `/etc/flightdeck/compose.env` exists — README "Running on Postgres") ask after the plugin update in step 4, from the new directory: `if [ -e ~/.flightdeck/compose.env ]; then echo '선택 파일의 옛 자리 ~/.flightdeck/compose.env 는 서버가 쓸 수 있는 /data 안이라 더는 읽지 않는다 - 내용을 확인한 뒤 /etc/flightdeck/compose.env 로 옮기고 다시 돌린다: sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck && mv -n ~/.flightdeck/compose.env /etc/flightdeck/compose.env (README 「Postgres 로 띄우기」)' >&2; false; fi && COMPOSE_ENV_FILES="$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)" docker compose run --rm --build --no-deps migrate selfcheck`. The down line in step 4 first checks three things and, if any fails, stops without taking anything down — **then do not bring it up; stop and ask**. (1) A file is left at the selection file's old place `~/.flightdeck/compose.env`: it is inside `/data`, which the server writes, so it is no longer read (the line prints the command that moves it). (2) The running server's `FD_DB` (`docker inspect`) and the selection file disagree: `postgres://` without the file tries to start the default path (SQLite) — with the move marker (`~/.flightdeck/fd.db.moved-to-postgres`) migrate refuses and the server stays down, without it the old SQLite ledger gets the writes — and SQLite with the file tries to start on Postgres without a move. (3) A dump started by hand (`run … db-backup once`) is running: `down` does not stop it, and the new migrate's DDL fails on its table locks — wait for it to end. The compose prefix here and in step 4 (`COMPOSE_ENV_FILES=…`) is the same single line as the `fd setup` start command and the watcher's container reason: with that file it layers the overlay, without it the value is empty and plain compose runs. Do not rewrite it per place.

## 4. Update

```
claude plugin marketplace update <marketplace>
claude plugin update flightdeck@<marketplace>

# Read EVERY repo mount BEFORE bringing it down — those values are recorded nowhere else
readarray -t R < <(docker inspect flightdeck --format '{{range .Mounts}}{{if eq .Source .Destination}}{{println .Source}}{{end}}{{end}}') && [ ${#R[@]} -ge 1 ] &&
if [ -e ~/.flightdeck/compose.env ]; then echo '선택 파일의 옛 자리 ~/.flightdeck/compose.env 는 서버가 쓸 수 있는 /data 안이라 더는 읽지 않는다 - 내용을 확인한 뒤 /etc/flightdeck/compose.env 로 옮기고 다시 돌린다: sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck && mv -n ~/.flightdeck/compose.env /etc/flightdeck/compose.env (README 「Postgres 로 띄우기」)' >&2; false; fi && if case "$(docker inspect flightdeck -f '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^FD_DB=//p')" in postgres://*) [ ! -e /etc/flightdeck/compose.env ];; *) [ -e /etc/flightdeck/compose.env ];; esac; then echo 'the running server FD_DB and the selection file /etc/flightdeck/compose.env disagree - not taking it down, stop and ask' >&2; false; fi && if [ -n "$(docker ps -q --filter label=com.docker.compose.service=db-backup --filter label=com.docker.compose.oneoff=True)" ]; then echo 'a hand-run dump (db-backup once) is running - wait for it to end, then run again' >&2; false; fi && cd "$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}')" && COMPOSE_PROJECT_NAME="$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project"}}')" COMPOSE_ENV_FILES="$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)" docker compose down &&
cd ~/.claude/plugins/cache/<marketplace>/flightdeck/<new version> &&
FD_TOKEN="$(cat ~/.flightdeck/token 2>/dev/null || sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' ~/.flightdeck/config.json)" FD_REPOS="${R[0]}" FD_REPOS2="${R[1]:-}" FD_REPOS3="${R[2]:-}" FD_REPOS4="${R[3]:-}" FD_UID=$(id -u) FD_GID=$(id -g) COMPOSE_ENV_FILES="$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)" docker compose up -d --build
```

**The four lines from reading the mounts to bringing it up are one `&&` chain.** If no mount can be read or a gate on the down line stops, it neither moves to the new directory nor brings anything up — then stop and ask. Do not split the lines and run them one by one (the `up` would run even after an earlier line stopped).
**Down the old directory first**, found from the running container's compose labels (project, directory) — the compose project name is the version directory, or the container name collides.
**Read every repo mount before that** — four slots `FD_REPOS`..`FD_REPOS4`, alive only at substitution time with
the mount as their one trace; drop one and it silently reverts (measured: 7 projects, **6 underivable**).

**Bring it up from the installed cache directory.** Bring it up from the dev repo and the installed build and
the running build diverge. The compose `migrate` step applies DB migrations before the server and backs up first; the server
starts only once it succeeds (otherwise see `logs migrate`). A Postgres ledger sits in fixed-name volumes and survives the move.

## 5. Look at the evidence that it updated

Check **both**: `fd status | head -1` prints `파생 git@` with a just-now stamp, **and** `claude plugin list` prints the new version. One alone mistakes a half-update for current.

## 6. Always pass on what the human must do

> **Claude Code has to be restarted — not one window, but every open session.**
> An MCP server reads its config once at startup and that is that, so a window you did not relaunch keeps
> using the old token and old tools. Until then use the CLI instead of MCP
> (`fd pick` · `fd note` · `fd finish` work right now).

Leave this out and the user hits "I updated it and it does not work" — the server is already new, only the screen is old, so the cause is invisible.

## Not doing

Manual build instead of a version bump (that server keeps running stale) · calling it current from `healthz` alone ·
omitting the restart notice · **deleting an old plugin cache other sessions reference** (their hooks and MCP break in place).
