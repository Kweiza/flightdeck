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
(If the build is old enough to print `모르는 명령: selfcheck`, that build's `serve` stops at startup for the same reason.) **On a Postgres ledger** (the selection file `/etc/flightdeck/compose.env` exists — README "Running on Postgres") ask after the plugin update in step 4, from the new directory: `if [ -e ~/.flightdeck/compose.env ]; then echo '선택 파일의 옛 자리 ~/.flightdeck/compose.env 는 서버가 쓸 수 있는 /data 안이라 더는 읽지 않는다 - 내용을 확인한 뒤 /etc/flightdeck/compose.env 로 옮기고 다시 돌린다: sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck && mv -n ~/.flightdeck/compose.env /etc/flightdeck/compose.env (README 「Postgres 로 띄우기」)' >&2; false; elif [ -n "${FD_PG_INSTANCE+x}${FD_PG_DATABASE+x}" ]; then echo 'FD_PG_INSTANCE · FD_PG_DATABASE 는 셸에서 주지 않는다 - 어느 원장(볼륨 · DB)인지는 선택 파일 /etc/flightdeck/compose.env(꺼 두었으면 .off) 한 곳에 적는다. 셸 값은 선택 파일보다 먼저라 새 셸에서 빠지면 다른 원장으로 뜬다: 값을 선택 파일에 적고 unset FD_PG_INSTANCE FD_PG_DATABASE 뒤 다시 돌린다 (README 「Postgres 로 띄우기」)' >&2; false; fi && COMPOSE_ENV_FILES="$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)" docker compose run --rm --build --no-deps migrate selfcheck`. The down line in step 4 first checks three things and, if any fails, stops without taking anything down — **then do not bring it up; stop and ask**. (1) A file is left at the selection file's old place `~/.flightdeck/compose.env`, or the shell gives `FD_PG_INSTANCE` or `FD_PG_DATABASE` (even empty): the first is inside `/data`, which the server writes, so it is no longer read (the line prints the command that moves it); the second decides which ledger (volume, database) it is, so it lives only in the selection file — shell values win over the selection file, so a shell without the value would start on another ledger. (2) The running server's `FD_DB` (`docker inspect`) and the selection file disagree: `postgres://` without the file tries to start the default path (SQLite) — with the move marker (`~/.flightdeck/fd.db.moved-to-postgres`) migrate refuses and the server stays down, without it the old SQLite ledger gets the writes — and SQLite with the file tries to start on Postgres without a move. (3) A dump started by hand (`run … db-backup once`) is running: `down` does not stop it, and the new migrate's DDL fails on its table locks — wait for it to end. The compose prefix here and in step 4 (`COMPOSE_ENV_FILES=…`) is the same single line as the `fd setup` start command and the watcher's container reason: with that file it layers the overlay, without it the value is empty and plain compose runs. Do not rewrite it per place.

## 4. Update

```
claude plugin marketplace update <marketplace>
claude plugin update flightdeck@<marketplace>

fdp=$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project"}}') && fdw=$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}') && [ -n "$fdp" ] && [ -d "$fdw" ] &&
mounts=$(docker inspect flightdeck --format '{{range .Mounts}}{{if eq .Source .Destination}}{{println .Source}}{{end}}{{end}}') && nmounts=$(printf '%s\n' "$mounts" | grep -c .) && if [ "$nmounts" -gt 4 ]; then echo "도는 서버의 저장소 마운트가 $nmounts 개다 - 슬롯(FD_REPOS…FD_REPOS4)은 넷이라 다섯째부터 빠진다: 멈추고 묻는다" >&2; false; fi &&
export FD_REPOS="$(printf '%s\n' "$mounts" | sed -n 1p)" FD_REPOS2="$(printf '%s\n' "$mounts" | sed -n 2p)" FD_REPOS3="$(printf '%s\n' "$mounts" | sed -n 3p)" FD_REPOS4="$(printf '%s\n' "$mounts" | sed -n 4p)" &&
if [ -e ~/.flightdeck/compose.env ]; then echo '선택 파일의 옛 자리 ~/.flightdeck/compose.env 는 서버가 쓸 수 있는 /data 안이라 더는 읽지 않는다 - 내용을 확인한 뒤 /etc/flightdeck/compose.env 로 옮기고 다시 돌린다: sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck && mv -n ~/.flightdeck/compose.env /etc/flightdeck/compose.env (README 「Postgres 로 띄우기」)' >&2; false; elif [ -n "${FD_PG_INSTANCE+x}${FD_PG_DATABASE+x}" ]; then echo 'FD_PG_INSTANCE · FD_PG_DATABASE 는 셸에서 주지 않는다 - 어느 원장(볼륨 · DB)인지는 선택 파일 /etc/flightdeck/compose.env(꺼 두었으면 .off) 한 곳에 적는다. 셸 값은 선택 파일보다 먼저라 새 셸에서 빠지면 다른 원장으로 뜬다: 값을 선택 파일에 적고 unset FD_PG_INSTANCE FD_PG_DATABASE 뒤 다시 돌린다 (README 「Postgres 로 띄우기」)' >&2; false; fi && fdenv=$(docker inspect flightdeck -f '{{range .Config.Env}}{{println .}}{{end}}') && dumps=$(docker ps -q --filter label=com.docker.compose.service=db-backup --filter label=com.docker.compose.oneoff=True) && if case "$(printf '%s\n' "$fdenv" | sed -n 's/^FD_DB=//p')" in postgres://*) [ ! -e /etc/flightdeck/compose.env ];; *) [ -e /etc/flightdeck/compose.env ];; esac; then echo 'the running server FD_DB and the selection file /etc/flightdeck/compose.env disagree - not taking it down, stop and ask' >&2; false; fi && if [ -n "$dumps" ]; then echo 'a hand-run dump (db-backup once) is running - wait for it to end, then run again' >&2; false; fi && cd "$fdw" && COMPOSE_PROJECT_NAME="$fdp" COMPOSE_ENV_FILES="$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)" docker compose down &&
cd ~/.claude/plugins/cache/<marketplace>/flightdeck/<new version> &&
{ FD_TOKEN="$(cat ~/.flightdeck/token 2>/dev/null || sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' ~/.flightdeck/config.json)" FD_UID=$(id -u) FD_GID=$(id -g) COMPOSE_ENV_FILES="$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)" docker compose up -d --build || { COMPOSE_ENV_FILES="$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)" docker compose logs --no-log-prefix migrate; false; }; }
```

**The six lines from reading the server to bringing it up are one `&&` chain.** **Read EVERY repo mount BEFORE bringing it down** — those values are recorded nowhere else. The first line is the lookup line of `fdwhere` in README "Running on Postgres", the second and third those of `fdrepos`, word for word (a test compares them): they read the running server's compose labels (project, directory) and its repo mounts. Every docker query is a standalone assignment (`x=$(docker …)`), so a failing docker stops the chain there — `[ -n "$(docker …)" ]` and `cd "$(docker …)"` read the failure as an empty value and carry on. The mounts read **one output line as one path** (`sed -n 1p` …) into the slots in order — a path with a space (`/repos/team one`) is one path. The chain stops when none can be read (`grep -c .`, counting the lines, fails on 0) and, before anything goes down, when there are more than four (the fifth on would be dropped). No blank-splitting `set -- $(…)`, no `readarray`, no arrays: the first cuts a path with a space in two, zsh and macOS's bash 3.2 have no `readarray`, so the chain would stop on its first line (127), and zsh arrays count from 1. The values read stay in this shell through `export`, and the up line passes them on. If the server or its mounts cannot be read or a gate on the down line stops, it neither moves to the new directory nor brings anything up — then stop and ask. Do not split the lines and run them one by one (the `up` would run even after an earlier line stopped).
**Down the old directory first**, found from the running container's compose labels (project, directory) — the compose project name is the version directory, or the container name collides.
**Read every repo mount before that** — four slots `FD_REPOS`..`FD_REPOS4`, alive only at substitution time with the mount as their one trace; drop one and it silently reverts (measured: 7 projects, **6 underivable**).

**Bring it up from the installed cache directory.** Bring it up from the dev repo and the installed build and
the running build diverge. The compose `migrate` step applies DB migrations before the server and backs up first; the server
starts only once it succeeds. When the up fails the chain shows why with `logs migrate` — inside the rollback window (72 hours after the move) migrate stops on a migration that does more than add indexes and names when the window closes. Only if you will not go back to the older version, run `run --rm migrate migrate --allow-in-rollback-window` with the same compose head and run the up line again — otherwise stop and ask (README "Going back within 72 hours"). A Postgres ledger sits in fixed-name volumes and survives the move.

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
