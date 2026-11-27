**English** | [한국어](README.ko.md)

# flightdeck

**A coordination layer for parallel coding-agent sessions.** Claude Code and codex sessions register
with one self-hosted server and work while seeing what the others are doing.

Sessions have no way to talk to each other, so each one *guesses* what the others picked up — and when
the guess is wrong, it redoes work another session is already doing. flightdeck removes the guess: who is
active, which paths they touch and what they claimed are **derived from git and the database** and shown
to every session.

Open a new session and the board appears before you type anything:

```
보드 · my-app · 2026-08-13 06:40 UTC · 파생 git@06:40:08 최신
잡혀 있는 작업 0건 (선점 기준이다 — 세션의 생사가 아니다)

잡혀 있는 작업이 없다 — 아무 세션도 큐 항목을 쥐고 있지 않다. 서버 장애가 아니다.
창 밖 73건 (가장 오래된 신호 8일 10시간 전) — 창은 표시 구간이지 생존 판정이 아니다
큐 열림 4건
  · 19시간 22분·티클러(08-19 발화) · fd-folded-multi-turn-drain-unmeasured — 다턴 배수가 미실측이다
  · 18시간 41분·티클러(08-26 발화) · fd-lane-turn-remeasure-… — lane-turn 축을 재측한다
랜딩 레인 0건(질의는 돌았다)
```

The same board is also served as a read-only dashboard at <http://localhost:7420>:

![flightdeck dashboard](docs/images/dashboard.png)

<sub>Captured from a demo server loaded with synthetic data (`my-app`).</sub>

> **flightdeck speaks Korean by default. Set `FD_LANG=en` for English.** The board, the dashboard,
> pick/land replies and overlap prescriptions then come out in English; lines the translation table
> does not cover yet (`fd doctor`, the login page, rarer diagnostics) stay in Korean, whole. The docs
> below quote the default Korean output and explain it in English. In the board above: no items are
> claimed, 4 queue items are open, and the landing lane is empty.

## Highlights

- **Claims, not locks** — `pick` claims a queue item; its id becomes the branch and worktree name.
  The merge is the only thing held exclusively.
- **Overlap before the merge** — hooks report uncommitted edits as you go. When two sessions touch the
  same path, both hear about it at the end of the turn. Nothing is blocked.
- **A landing lane** — a serialized queue in front of the merge. `fd land` exits 1 unless it is your
  turn, so `fd land && git merge --ff-only "$BRANCH"` is a correct one-liner.
- **Judgments that travel with the item** — record why you did it, what you rejected and what you
  deliberately left undone; whoever picks the item next gets it in full.
- **Derived, not declared** — branch, HEAD, footprints and activity signals come from git and hooks.
  There is no write API for anything that can be derived.
- **Claude Code and codex on one board** — sessions from both harnesses get the same overlap checks.
- **Loud when the server is down** — every session gets a banner saying what still works and what does not.

## How it works

1. **Server** — one Go binary (SQLite) in Docker. It mounts your repositories read-only and reads
   branches, commits and uncommitted changes itself.
2. **Hooks** — register the session, send activity signals and edit footprints, and inject
   prescriptions such as overlaps when a turn ends.
3. **Tools** — agents call MCP tools; people call the `fd` command. Same operations either way.
4. **Dashboard** — one read-only page at <http://localhost:7420>.

One session's day looks like this:

```
session opens   the SessionStart hook injects the board
pick            recommends what to take (does not claim)
pick(item_id)   claim + item body + linked judgments + worktree commands
work            the PostToolUse hook reports a footprint for every edit
finish          judgment + followups + close + release, in one transaction
land            queue for the landing lane → merge on your turn → land(result: ok)
```

Overlap, the landing lane, item states and activity signals are covered in [Concepts](docs/concepts.md).

## Quick start

You need **Docker** (server machine), **Go 1.25+** (every machine that runs sessions — `bin/fd` builds
on first use) and git.

**1. Start the server** — once, on one machine.

```bash
git clone https://github.com/Kweiza/flightdeck.git
cd flightdeck
docker compose up -d
curl -s localhost:7420/healthz     # {"ok":true,"api_version":"1","db_ok":true,…}
```

**2. Install the plugin** — on every machine that runs sessions.

```
/plugin marketplace add Kweiza/flightdeck
/plugin install flightdeck@flightdeck
```

**3. Open a session.** If the board shows up first, you are done. Start work with `/fd-pickup`.

> The `/fd-setup` skill can do steps 1 and 2 for you — it asks whether this machine is the server or a
> client, and installs only what is missing, after your approval.

- **Server on another machine?** Give sessions `FD_URL=http://<server>:7420` (and `FD_TOKEN` if you set one).
- **Without Docker:** `cd server && go run ./cmd/fd serve --addr :7420 --db ~/.flightdeck/fd.db`.
  It points at the same database as the container, so **never run both.**
- **Already installed as `flightdeck@kweiza-cc-plugins`?** Keep it — it fetches this repository. Do not enable both.

## Usage

### Inside a session — 10 MCP tools

There are four to remember: claim (`pick`) · record (`note`) · close (`finish`) · queue (`land`).

| Tool | What it does |
|---|---|
| `board` | Who holds what right now |
| `pick` | No arguments: recommend only. `item_id`: claim. `leave`: release |
| `note` | Record a judgment — `decision` `handoff` `blocked` `ask` `rejected` `not-done` `verified` `draft` |
| `add` | Create a queue item. The id becomes the branch name |
| `finish` | Judgment + followups + close + release, in one transaction |
| `land` | Queue for the landing lane (or a named resource) / report and release |
| `alloc` | Atomic counter — for logical sequence numbers such as revision counts |
| `label` | Display-only tags |
| `amend` | Fix an item's title, body or paths. Old values stay in the revision history |
| `show` | Read one item — closed ones too |

### From the terminal — `fd`

```bash
fd status                         # server status banner + board
fd next                           # recommendation only
fd pick <item-id>                 # claim
fd finish <item-id> --outcome done --body "why · rejected · not done · verified"
fd land && git merge --ff-only "$BRANCH"
fd doctor                         # actually measure this machine and the server
```

### 4 skills

| Skill | When |
|---|---|
| `/fd-setup` | First time on a machine — decide server vs. client, install and start only what is missing |
| `/fd-pickup` | Starting a session — board → recommendation → claim → read the linked judgments |
| `/fd-handoff` | Wrapping up — judgment, followups, close and release in one call |
| `/fd-update` | Bring server, plugin and DB up to date |

Every parameter and the habits that go with them are in the [Tool reference](docs/reference.md).

### Owner kanban: `board/`

A web board where a person sees the queue as a kanban and answers on the spot (one Python stdlib file, a host process).
Columns come from labels (`status:running` · `status:owner` · `status:owner-done` · `status:owner-ask`); cards are drawn from
convention lines in the body (`미리보기:` · `그림:` · `커밋:` · `소유자:` · `선택:` …). Sessions receive the convention as one line
of the MCP instructions. Install, configuration and the full convention: [board/README.md](board/README.md).

## codex

```bash
fd setup --install-codex   # ~/.local/bin/fd-hook · ~/.local/bin/fd · ~/.codex/hooks.json
codex                      # open the TUI once and approve "Hooks need review"
```

- **Until approved, the hooks silently do not run.** Check with the codex section of `fd doctor`.
- codex's default sandbox cuts the network — `codex -c sandbox_workspace_write.network_access=true`.
- Put `~/.local/bin` on your PATH. codex has no MCP tools; use the `fd` command.

Why it is built this way, and the full table of what works, is in [Using it from codex](docs/codex.md).

## Configuration

The server reads environment variables through compose; sessions read `~/.flightdeck/config.json`
(written by `fd setup`) or environment variables. The Postgres ledger's variables (`FD_PG_*`, `FD_BACKUP_HOST`) are read only when the override `compose.postgres.yaml` is layered on, and go into the selection file `/etc/flightdeck/compose.env` (see "Postgres ledger" below).

| Variable | Side | Meaning |
|---|---|---|
| `FD_TOKEN` | server · session | Auth token. Without it the server runs unauthenticated, and `/healthz` says so |
| `FD_REPOS` | server | Where your repositories live (default `$HOME`). Everything derived comes from here — **keep it across upgrades** |
| `FD_REPOS2`–`FD_REPOS4` | server | Extra mounts when repositories live in several trees |
| `FD_UID` · `FD_GID` | server | Host user and group id (default 1000) |
| `FD_LEDGER_HOST` | server | Where the ledger backup goes (default `~/.flightdeck-ledger`) |
| `FD_BACKUP_DIR` | server | Where `fd migrate` writes the dump it takes before upgrading a Postgres ledger (`--backup-dir` wins; when both are empty, `/backups` if it exists). The Postgres layer of compose sets it to `/backups` (on the host, `FD_BACKUP_HOST`). **Give each deployment its own directory**: a test system cloned by physical replication shares the ledger identity (cluster identifier and database name) with its source, so in a shared directory a rollback could pick the other deployment's dump |
| `FD_DUMP_TIMEOUT` | server | Deadline for one dump of a Postgres ledger (Go duration, default `30m`, at least 1s). The dump `fd migrate` takes before upgrading and the daily dump of `db-backup` use the same value: for both it is "this ledger's normal dump time plus headroom". Business writes wait while the pre-upgrade dump runs, so if the dump stalls (say the backup volume stops responding) this is the longest they stay blocked. Past the deadline pg_dump is stopped, the server drops the migrator's connection and its locks, and the version is not raised. The daily dump stops pg_dump past the deadline and counts the day as failed (a stalled dump holding table locks makes the DDL of `fd migrate` fail). Raise it if a large ledger's normal dump takes longer |
| `FD_BACKUP_HOST` | server | The dump directory of the Postgres layer. **No default**: if it is empty, `up` with the Postgres layer stops. The dump `fd migrate` takes before upgrading goes right in it, the daily dumps of `db-backup` into its `daily/<db>/` (newest 14 kept). Dumps hold the whole ledger, so **create it beforehand outside the server's mounts (`FD_REPOS`, the whole home by default, and `~/.flightdeck`)** (for example `/var/backups/flightdeck`, owner `FD_UID`, mode `0700`). **Give each deployment its own**: two deployments with the same database name sharing one directory would judge freshness by each other's dumps and rotate each other's dumps away. Setup is in "Running on Postgres" below |
| `FD_PG_INSTANCE` | server | The deployment name of the Postgres layer (default `flightdeck`). The db container's name (`<name>-db`) and both volumes' names (`<name>-pgdata`, `<name>-pgrun`) all come from it. Production keeps the default; only rehearsals and tests give another value (the rehearsal in "Upgrade: SQLite ledger to Postgres" below) |
| `FD_PG_SERVER_PGPASS_HOST` · `FD_PG_BACKUP_PGPASS_HOST` | server | The pgpass files of the Postgres layer's two login accounts (server and daily dump). **No default**: if one is empty, `up` with the Postgres layer stops. Owner `FD_UID`, mode `0600`, line `db:5432:<db>:<account>:<password>`. They are the only place the passwords live (never in environment variables, command lines or URLs). **Keep them outside the server's mounts**: the server reads `FD_REPOS` (the whole home by default) (see "Running on Postgres" below) |
| `FD_PG_DATABASE` · `FD_PG_SERVER_USER` · `FD_PG_BACKUP_USER` | server | Database and login account names of the Postgres layer (default `flightdeck`, `fd_server` and `fd_backup`; lowercase letters, digits and `_` only). `db-roles` creates and checks the accounts: the server account is a member of `fd_app`, the dump account of `pg_read_all_data`, and neither has privileged attributes. Each database gets its own dump directory (`daily/<db>/`), so after switching databases the new one neither counts nor deletes the old one's dumps |
| `FD_URL` | session | Server address (default `http://127.0.0.1:7420`) |
| `FD_STATE_DIR` | session | Put the state files (outbox, caches, machine id) in one place |
| `FD_LANG` | server · session | Output language. `en` for English (default Korean). The server side covers the dashboard; the session side covers the CLI, hooks and MCP replies |

The traps behind each one are in [Running the server](docs/server.md).

`FD_LANG=en` translates **at the output boundary**: the board, the dashboard, add · next · pick · note ·
finish · show, land, the tail and Stop prescriptions are covered first. A line the table
(`server/internal/lang/catalog.go`) does not cover stays in Korean as a whole — no half-translated
lines. `fd doctor`, the login page and rarer diagnostics are still Korean.

## Postgres ledger

The default deployment keeps its ledger in one SQLite file (`~/.flightdeck/fd.db`). The Postgres ledger is on **only when you
choose it**: layering the override file `compose.postgres.yaml` over `compose.yaml` adds the ledger database (`db`,
Postgres 17), a one-shot for the login accounts (`db-roles`) and the daily dump (`db-backup`), and points `migrate` and the
server at that database. A plain `docker compose up` without the override is still SQLite in this version.

| Service | Connects as | Path · job |
|---|---|---|
| `migrate` | superuser `postgres` | The socket volume (trust). Raises the schema and runs the manual admin commands (import, export, restore, psql) |
| `db-roles` | superuser | The socket. Creates the two login accounts, checks them and sets their passwords (`deploy/db-roles.sh`). An account that does not match is left alone and the run stops |
| `flightdeck` | the server account (member of `fd_app`) | TCP `db:5432`, a pgpass file |
| `db-backup` | the dump account (member of `pg_read_all_data`) | TCP, a pgpass file |

The superuser has no password (it is initialised with a random value nobody knows). The access rules
(`deploy/pg_hba.conf`) reject the superuser over TCP, IPv4 and IPv6 alike, so only `migrate` and `db-roles`, which mount the
socket volume, can connect as it. Those rules are read from the repository's file at every start, so an existing volume
gets them too. `db` publishes no port.

### Running on Postgres

**Setup** - once, as the `FD_UID` user.

```bash
# 1. Two volumes. Fixed names outside any project, so the ledger stays the same when each version lives in another
#    directory, and `down -v` cannot remove them. Compose does not create them: if one is missing, `up` stops instead of
#    creating an empty ledger. The names come from the deployment name (FD_PG_INSTANCE, default flightdeck).
docker volume create flightdeck-pgdata && docker volume create flightdeck-pgrun

# 2. Places outside the server's mounts (see the star note): the selection file and pgpass, and the dumps. If the dump
#    directory is missing, Docker on Linux creates it root-owned 0755 and the first dump cannot write.
sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck /var/backups/flightdeck

# 3. A pgpass for each login account. Owner FD_UID, mode 0600, one line each.
(umask 077
 printf 'db:5432:flightdeck:fd_server:%s\n' "$(openssl rand -hex 24)" > /etc/flightdeck/server.pgpass
 printf 'db:5432:flightdeck:fd_backup:%s\n' "$(openssl rand -hex 24)" > /etc/flightdeck/backup.pgpass)

# 4. Write the choice down in the selection file. No passwords in it, only locations. Write it under its switched-off name
#    (.off): switching it on is one step of starting or upgrading below.
cat > /etc/flightdeck/compose.env.off <<'EOF'
COMPOSE_FILE=compose.yaml:compose.postgres.yaml
FD_PG_SERVER_PGPASS_HOST=/etc/flightdeck/server.pgpass
FD_PG_BACKUP_PGPASS_HOST=/etc/flightdeck/backup.pgpass
FD_BACKUP_HOST=/var/backups/flightdeck
EOF

# 5. Keep these in your shell config. fdc is the head that calls compose; fdrepos reads the running server's repository
#    mounts into FD_REPOS…FD_REPOS4.
fdc() {
  FD_TOKEN="${FD_TOKEN:-$(cat ~/.flightdeck/token 2>/dev/null || sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' ~/.flightdeck/config.json 2>/dev/null)}" \
  FD_UID="${FD_UID:-$(id -u)}" FD_GID="${FD_GID:-$(id -g)}" \
  COMPOSE_ENV_FILES="${FDC_ENV:-$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)}" docker compose "$@"
}
fdrepos() {
  set -- $(docker inspect flightdeck --format '{{range .Mounts}}{{if eq .Source .Destination}}{{println .Source}}{{end}}{{end}}') &&
    [ $# -ge 1 ] && export FD_REPOS="$1" FD_REPOS2="${2:-}" FD_REPOS3="${3:-}" FD_REPOS4="${4:-}" && echo "repository mounts: $*"
}
```

- When the selection file is switched on (`/etc/flightdeck/compose.env`), `fdc` hands it to compose as its env file
  (`COMPOSE_ENV_FILES`, the same as `--env-file`). That is the same test the start step of `fd setup` and the `fd-update`
  skill make. With `FDC_ENV` set it hands that file instead (the upgrade runs on the switched-off file before switching it
  on). `COMPOSE_FILE` layers the override; the paths are relative to the directory compose runs in, so run it from the root
  of the repository (or of the installed plugin). The token and user ids are filled in the way `fd-update` does; the
  repository mounts (`FD_REPOS` …) come from the shell as before (`fdrepos`). Shell values win over the selection file, and
  with it the directory's `.env` is not read.
- Passwords must be printable ASCII without spaces, backslashes or colons. The server (pgx) and the daily dump (libpq) read
  the same file and the two parse anything else differently. `db-roles` refuses a line that does not fit and names its line
  number (never the password).
- If you change the database or account names (`FD_PG_DATABASE`, `FD_PG_SERVER_USER`, `FD_PG_BACKUP_USER`, in the selection
  file), change the pgpass lines too.

★ **Keep the selection file, the pgpass files and the dumps outside the server's mounts.** The server runs as the same uid,
mounts `FD_REPOS` (the whole home by default) read-only and `~/.flightdeck` read-write. Inside the home, even at `0600`, the
server could read the dump account's password and the dumps of the whole ledger, and a selection file inside
`~/.flightdeck` is one the server could rewrite: the next update (run by a person, sometimes as root) would trust that
override and those secret locations. That is why compose has no default for the pgpass or dump locations: if one is empty,
`up` stops and names the variable. Do not widen `FD_REPOS` to `/`, `/etc` or `/var`. If a file is left at the selection
file's old place `~/.flightdeck/compose.env`, the start step of `fd setup` and `fd-update` print the command that moves it
and stop.

**Start** - for a new, empty ledger. Switch the selection file on, then start.

```bash
mv -n /etc/flightdeck/compose.env.off /etc/flightdeck/compose.env && [ ! -e /etc/flightdeck/compose.env.off ] &&
  fdc up -d --build && fdc ps -a     # db, flightdeck and db-backup healthy; migrate and db-roles Exited (0)
curl -s localhost:7420/healthz      # db_ok and db_locale_ok are true
```

On an empty ledger `migrate` lays the baseline first. **To bring a SQLite ledger along**, do not start this way; follow
"Upgrade: SQLite ledger to Postgres" below. If `~/.flightdeck/fd.db` is still there without the move marker, `migrate`
started this way refuses (a new ledger that skipped the move).

**Admin commands** run in the `migrate` service. The ledger (`/data`), the dump directory (`/backups`), psql, pg_dump and
pg_restore are all there, and it connects as the superuser over the socket. Use `--no-deps` while `db` is up.

```bash
fdc run --rm --no-deps migrate selfcheck                                                # version and planned steps
sql() { fdc run --rm --no-deps --entrypoint psql migrate -X -v ON_ERROR_STOP=1 "$@"; }    # superuser psql
srvsql() { fdc run --rm --no-deps -T --entrypoint sh flightdeck -c 'exec psql -X -v ON_ERROR_STOP=1 -d "$FD_DB" "$@"' sh "$@"; }  # the server account on the server's ledger (SQL on stdin)
nodump() { [ -z "$(docker ps -q --filter label=com.docker.compose.service=db-backup)" ] || { echo 'a dump is still running (a hand-run once too) - check again when it ends' >&2; false; }; }
```

- **Before running `fd migrate`, the export or a restore by hand, stop the daily dump and check that hand-run dumps have
  ended too**: `fdc stop db-backup && nodump`. pg_dump holds a lock on every table until it ends, and the DDL that raises
  the version fails when it cannot take that lock within lock_timeout (5s). When `fdc up` recreates migrate (new image or
  settings), compose stops the daily dump first and starts it again after migrate succeeds (`restart: true` in
  `depends_on`). `run` and `up --no-deps migrate` do not, and neither `stop` nor `restart: true` touches more than the
  service's own container: a dump started by hand with `fdc run --rm db-backup once` keeps running (`down` leaves it too).
  If `nodump` stops, wait for that dump to end and check again.
- **To change the passwords**, edit both pgpass files **in place** (replacing a file with a new one makes it disappear inside
  the containers that mount it), then `fdc up -d --force-recreate --no-deps db-roles flightdeck db-backup`. The server reads
  its file once at startup.

★ **One ledger, one override.** After committing, the import leaves a move marker next to the SQLite ledger
(`~/.flightdeck/fd.db.moved-to-postgres`). Starting the default compose without the selection file after the move runs the
default path's (SQLite) `migrate`, which sees the marker and neither opens nor creates the ledger, so the server stays down:
run `docker compose down`, check that the selection file is switched on, and start again with `fdc up -d`. The other way
round, `migrate` with the override refuses while an unmarked `fd.db` remains. Only "Going back within 72 hours" below
removes the marker.

### Upgrade: SQLite ledger to Postgres

This takes the ledger you built on SQLite into a Postgres ledger. The server is down during the move. For 72 hours afterwards
you can go back to SQLite, carrying along what was written in Postgres ("Going back within 72 hours" below).

**The ledger must be at version 18.** The import only takes the last SQLite version (18). An older ledger is raised first on
this version's default path: that is step 3.

**Go on to the next step only after a step prints its last line.** Each step is one `&&` chain, so if any command fails the
step's closing `echo` does not print. The next step's first check also re-tests what the previous step left; pasted all at
once, nothing after a failed step runs. **Run it from the root of the repository (or of this version's installed plugin).**

```bash
# 0. Finish setup steps 1-5 of "Running on Postgres" (volumes, places, pgpass, switched-off selection file, fdc, fdrepos).
#    Do not switch it on or start it.
# 1. Read the running server's compose project and directory and its repository mounts. The mounts (FD_REPOS and friends)
#    cannot be read once the container is down.
unset FDC_ENV; p=$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project"}}') &&
  w=$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}') &&
  [ -n "$p" ] && [ -d "$w" ] && fdrepos && echo "1 done: project $p in $w"
# 2. Take it down from the old version's directory and check that no container of that project is left (hand-run ones too).
[ -n "$p" ] && (cd "$w" && fdc -p "$p" down) && [ -z "$(docker ps -aq --filter label=com.docker.compose.project="$p")" ] &&
  ! docker inspect flightdeck >/dev/null 2>&1 && echo "2 done: it is down"
# 3. Build this version's image and raise the SQLite ledger to this version (nothing happens at 18). selfcheck must say ok.
! docker inspect flightdeck >/dev/null 2>&1 && fdc build &&
  fdc run --rm --no-deps migrate migrate --db /data/fd.db && fdc run --rm --no-deps migrate selfcheck --db /data/fd.db &&
  echo "3 done: the SQLite ledger is at this version"
# 4. Start the Postgres ledger from the switched-off selection file and lay only the baseline (stops at version 18). db
#    starts with it.
export FDC_ENV=/etc/flightdeck/compose.env.off
[ -f "$FDC_ENV" ] && fdc run --rm migrate migrate --to 18 && echo "4 done: baseline laid"
# 5. Dry run, then move. After committing, the move leaves a move marker (fd.db.moved-to-postgres) next to the source. No
#    marker, no further.
fdc run --rm --no-deps migrate migrate import-sqlite --from /data/fd.db --dry-run &&
  fdc run --rm --no-deps migrate migrate import-sqlite --from /data/fd.db &&
  [ -e ~/.flightdeck/fd.db.moved-to-postgres ] && echo "5 done: moved"
# 6. Create and check the login accounts (db-roles), then connect as the server account to the server's ledger: the moved
#    tables must be readable and TEMPORARY must be gone.
[ -e ~/.flightdeck/fd.db.moved-to-postgres ] && fdc up -d db-roles && [ "$(docker wait "$(fdc ps -aq db-roles)")" = 0 ] &&
  srvsql <<'SQL' && echo "6 done: the accounts are right"
SELECT current_user AS account, current_database() AS ledger, (SELECT count(*) FROM item) AS items, (SELECT count(*) FROM judgment) AS judgments;
SELECT 1 / (NOT has_database_privilege(current_database(), 'TEMPORARY'))::int AS no_temp_privilege;
SQL
# 7. Switch the selection file on only after the compare matches. Once on, later updates and restarts start on Postgres.
[ -e ~/.flightdeck/fd.db.moved-to-postgres ] && fdc run --rm --no-deps migrate migrate verify --from /data/fd.db &&
  mv -n /etc/flightdeck/compose.env.off /etc/flightdeck/compose.env && [ ! -e /etc/flightdeck/compose.env.off ] &&
  unset FDC_ENV && echo "7 done: it matches, selection file on"
# 8. Start on the Postgres ledger. It worked if the board shows the moved items.
[ -e /etc/flightdeck/compose.env ] && [ -z "${FDC_ENV:-}" ] && fdc up -d && fdc ps -a
curl -s localhost:7420/healthz      # db_ok and db_locale_ok are true
```

- If it stops **before step 5 leaves the marker** (a refusal or failure in 1-5), nothing is committed (the import is one
  transaction) and the SQLite ledger was only read. Fix the cause and run again from that step, or go back to SQLite:
  `fdc down; unset FDC_ENV; fdc up -d` (the selection file stays off).
- If it stops **after step 5 left the marker** (6-8), the ledger is already in Postgres. Fix the cause and run again from
  that step. To go back to SQLite, follow "Going back within 72 hours" below: there is no other way to remove the marker by
  hand.
- After the move the SQLite ledger (`fd.db` and its side files) may stay where it is: the marker blocks it on the SQLite
  side. To tidy up (optional), with the server down, move `fd.db`, `fd.db-wal` and `fd.db-shm` to other names in the same
  directory and make them read-only. **Do not move the marker**: without it a new SQLite ledger could appear in that place.
- **If you moved a copy** (a `VACUUM INTO` copy outside the ledger's place given to `--from`), the marker sits next to the
  copy, not in the ledger's place, and `migrate` with the override sees the `fd.db` in the ledger's place as a ledger that
  was never moved and refuses. Check that the original matches what was moved and leave the marker, or move the original
  out of the ledger's place:
  `fdc run --rm --no-deps migrate migrate verify --from /data/fd.db && touch ~/.flightdeck/fd.db.moved-to-postgres`
- **If both ledgers have already been written to separately** (a SQLite server wrote to the old ledger after the move, for
  example), take both servers down and **delete neither ledger**. `verify --from` lists the rows that differ by primary
  key; a person decides which side to keep. There is no merge command.
- The refusals and the comparison in detail are in "Moving a SQLite ledger to Postgres" below.

**Keep the rehearsal apart from production.** The server container name (`flightdeck`) and port 7420 are fixed, so it
cannot run next to the production server on the same Docker, and with the production volume names a rehearsal's `migrate`
connects to the production ledger as the superuser: rehearse on **another Docker environment** (another machine or a VM).
Even there, give values that differ from production: the project name (`fdc -p <rehearsal>`), the home (`HOME`: `/data` is
`~/.flightdeck`), the deployment name `FD_PG_INSTANCE` (the db container's and both volumes' names), `FD_BACKUP_HOST`, and the
selection file (`FDC_ENV=<rehearsal file>`). Create the volumes with
`docker volume create <deployment>-pgdata && docker volume create <deployment>-pgrun`, and empty them afterwards with
`fdc -p <rehearsal> down` and `docker volume rm <deployment>-pgdata <deployment>-pgrun`. ★ **Never remove
`flightdeck-pgdata` or `flightdeck-pgrun`**: that is the production ledger. Check the names once more with `docker volume ls`
before removing anything.

#### Going back within 72 hours

The 72 hours after the move are the rollback window. `export-sqlite` moves the Postgres ledger into a **new** SQLite file at
version 18, so the items, claims and judgments written in Postgres after the move come back too. During the window, do not
update to a flightdeck version that raises the Postgres schema past 18: `export-sqlite` only takes a version-18 ledger. As in
the upgrade, go on only after a step prints its last line.

```bash
# 1. Read the repository mounts, stop the server and the daily dump (leave db up), and check that hand-run dumps have ended
#    too. The export refuses while other connections are attached.
unset FDC_ENV; fdrepos && fdc stop flightdeck db-backup && nodump && echo "1 done: stopped"
# 2. Dry run, then move. No file, no further.
fdc run --rm --no-deps migrate migrate export-sqlite --to /data/fd-back.db --dry-run &&
  fdc run --rm --no-deps migrate migrate export-sqlite --to /data/fd-back.db &&
  [ -f ~/.flightdeck/fd-back.db ] && echo "2 done: written"
# 3. Take the Postgres layer down and switch the selection file off only after the compare matches. Left on, the next update
#    would layer Postgres again.
[ -f ~/.flightdeck/fd-back.db ] && fdc run --rm --no-deps migrate migrate verify --from /data/fd-back.db && fdc down &&
  mv -n /etc/flightdeck/compose.env /etc/flightdeck/compose.env.off && [ ! -e /etc/flightdeck/compose.env ] &&
  echo "3 done: it matches, selection file off"
# 4. Put the new file in the ledger's place. The ledger already there (the moved SQLite ledger and its side files) is not
#    deleted but moved to a side name; if any of them cannot be moved, it stops. Only then is the move marker removed: left
#    in place, the SQLite migrate will not open this ledger.
[ ! -e /etc/flightdeck/compose.env ] && (set -eu; cd ~/.flightdeck; [ -f fd-back.db ]; t=$(date -u +%Y%m%dT%H%M%SZ)
  for s in '' -wal -shm -journal; do if [ -e fd.db$s ]; then mv -n fd.db$s fd.db.pre-back-$t$s; fi; done
  for s in '' -wal -shm -journal; do [ ! -e fd.db$s ]; done
  for s in '' -wal -shm -journal; do if [ -e fd-back.db$s ]; then mv -n fd-back.db$s fd.db$s; fi; done
  for s in '' -wal -shm -journal; do [ ! -e fd-back.db$s ]; done
  [ -f fd.db ]; rm -f fd.db.moved-to-postgres; echo "4 done: the exported ledger is in place")
# 5. Start on the default path (SQLite).
[ -f ~/.flightdeck/fd.db ] && [ ! -e ~/.flightdeck/fd.db.moved-to-postgres ] && fdc up -d && fdc ps -a
```

- If verify does not match (step 3 stops), do not put the file in the ledger's place. Find and stop whatever is attached,
  move `fd-back.db` to a side name (the export does not overwrite an existing file), and start again from step 1.
- The Postgres ledger (volume `flightdeck-pgdata`) stays. A later move needs an empty target: the import refuses otherwise.

### Daily dumps of a Postgres ledger

The `db-backup` service writes a `pg_dump -Fc` once a day to `$FD_BACKUP_HOST/daily/<db>/fd-daily-<UTC>.dump` and keeps the
newest 14. It runs only with the Postgres layer (setup is in "Running on Postgres"). It rotates only after a dump succeeds,
and only one run works in a database's directory at a time. The container turns unhealthy when the newest dump is older
than the interval plus two checks plus the deadline (26h30m by default).

`fdc run --rm --no-deps db-backup config` prints the settings on one line. `once` takes one dump by hand (0 = dumped, or not
due yet; 1 = failed; 3 = another run is working in the same directory, nothing done).

**Restore** - in the `migrate` service (superuser over the socket; the dumps are visible under `/backups`). `sql`, `srvsql`
and `nodump` are the admin commands from "Running on Postgres". Go on only after a step prints its last line.

```bash
fdc stop flightdeck db-backup && nodump && echo "stopped"     # drop the connections to the database you are restoring
```

1. Read from the dump which database it restores and compare it with `FD_PG_DATABASE`. `--create` makes the database under
   **the name written in the dump**, not the current `FD_PG_DATABASE`; if they differ, the server does not see the restored
   database. **If they differ, stop**: a person decides which name to use (whether to change `FD_PG_DATABASE` and the pgpass
   lines to the dump's name).

   ```bash
   f='/backups/daily/<db dir>/fd-daily-<UTC>.dump'   # fill in the placeholders
   fdc run --rm --no-deps --entrypoint sh migrate -c 'd=$(pg_restore -l "$1" | sed -n "s/^; *dbname: //p")
     echo "dump database: $d, FD_PG_DATABASE: $PGDATABASE"; [ -n "$d" ] && [ "$d" = "$PGDATABASE" ]' sh "$f" && echo "1 done: same name"
   ```

   If a database by that name exists, do not drop it; **rename it and keep it** (`<db>` is the name read above):
   `sql -d postgres -c 'ALTER DATABASE "<db>" RENAME TO "<db>_before_restore"'`. Drop the old one after the check in step 4.
2. Roles are cluster-wide, so they are not in the dump. Restoring into the same volume (`flightdeck-pgdata`), they already
   exist. If the volume itself is gone, create a new one, run `fdc up -d db-roles` to create the group roles, the login
   accounts and an empty ledger, then begin with the rename in step 1.
3. Restore the whole database. `--create` also restores the database-level privileges (`TEMPORARY` revoked from PUBLIC);
   restoring into a database created beforehand loses that.

   ```bash
   fdc run --rm --no-deps --entrypoint pg_restore migrate -d postgres --create --exit-on-error "$f" && echo "3 done: restored"
   ```

4. Check the accounts again (`db-roles`), then connect **as the server account to the server's ledger** (`srvsql`: the
   server's own `FD_DB`) and check that it is the restored ledger. If the database the server connects to is the renamed old
   one (the same oid), the tables are not readable (the membership), there are no items, or `TEMPORARY` remains, it stops
   with an error and a nonzero exit (`ON_ERROR_STOP`; a deliberate division by zero). Check by eye on the first line that
   the last event is just before the time in the dump's name.

   ```bash
   fdc up -d db-roles && [ "$(docker wait "$(fdc ps -aq db-roles)")" = 0 ] && srvsql <<'SQL' && echo "4 done: it is the restored ledger"
   SELECT current_database() AS ledger, d.oid, o.oid AS old_oid, (SELECT count(*) FROM item) AS items,
          (SELECT count(*) FROM judgment) AS judgments, (SELECT max(at) FROM event) AS last_event
     FROM pg_database d LEFT JOIN pg_database o ON o.datname = d.datname || '_before_restore' WHERE d.datname = current_database();
   SELECT 1 / (d.oid IS DISTINCT FROM o.oid)::int AS new_database
     FROM pg_database d LEFT JOIN pg_database o ON o.datname = d.datname || '_before_restore' WHERE d.datname = current_database();
   SELECT 1 / (count(*) > 0)::int AS has_items FROM item;
   SELECT 1 / (NOT has_database_privilege(current_database(), 'TEMPORARY'))::int AS no_temp_privilege;
   SQL
   ```

5. Start again with `fdc up -d`. Drop the renamed old database once the check is done
   (`sql -d postgres -c 'DROP DATABASE "<db>_before_restore"'`).

### Moving a SQLite ledger to Postgres

In a compose deployment, move with "Upgrade: SQLite ledger to Postgres" above. This section covers what the commands
themselves do and refuse; `<URL>` below connects as the superuser or a member of `fd_owner` (in the compose `migrate` service
`FD_DB` is that URL, so leave out `--db`).

`fd migrate import-sqlite` moves a ledger at the last SQLite version (18) into an **empty** Postgres ledger in one
transaction. Inside that transaction it then compares the two ledgers and does not commit on any mismatch.
`fd migrate verify` runs only that comparison again (it writes nothing). The comparison covers, per table, the row count and
a fingerprint over every column, plus the identity sequences (the next number must exceed the highest number the source
ever issued); mismatched rows are listed by primary key. Exit codes: 0 = moved · matched, 1 = refused · mismatch · failure,
2 = usage.

```bash
# 1. Stop the server that writes the source. Pass the ledger file itself to --from, or, to keep a copy, take one with
#    VACUUM INTO. Do not copy it with cp (see below).
sqlite3 fd.db "VACUUM INTO 'fd-copy.db'"
# 2. Lay the baseline on the target database, stopping at the import version (18).
fd migrate --db <URL> --to 18
# 3. Dry run: runs the same transaction as the real import to the end and rolls it back instead of committing.
fd migrate import-sqlite --from fd-copy.db --db <URL> --dry-run
# 4. Move. On success it prints the comparison table and the number of rows moved.
fd migrate import-sqlite --from fd-copy.db --db <URL>
# 5. (Optional) compare again before starting the server on the new ledger. Once the server writes, they differ.
fd migrate verify --from fd-copy.db --db <URL>
```

- Do not take the copy with `cp fd.db`. The SQLite ledger runs in WAL mode, so a commit lives only in `fd.db-wal` until it
  is checkpointed into the main file, and a `cp` copy loses it. The import and the comparison then both read the same copy
  and both say it matches; nothing reports the loss. `VACUUM INTO` (or `sqlite3 fd.db ".backup fd-copy.db"`) takes a
  consistent copy that includes the -wal.
- The source is opened read-only. If the `-wal` next to it holds any bytes (the server still runs, or it stopped abnormally
  before a checkpoint), the import refuses. Stop the server and run it again; if the bytes remain, take a copy with the
  `VACUUM INTO` above and pass that.
- The account must be a superuser or a member of `fd_owner`. The import restarts each sequence above the source's highest
  number (a deleted top number is never issued again), which only the owner role may do. Connected as the app role, the
  dry run refuses just as the real import does.
- Run `fd migrate verify` with the same account. The app role has no privilege (SELECT) to read the identity sequences, so
  it cannot compare them.
- A source older than version 18 is refused. First upgrade the copy with this version of fd (`fd migrate --db <copy>`).
- A source whose tables or columns differ from version 18 (a hand-edited ledger) is refused.
- A target with even one row in a business table is refused, as is one whose identity sequence has ever issued a number
  (even after the rows were deleted), one not at version 18, or one that fails the locale gate.
- If any connection other than this run is attached to the target database (a server, psql, ...), the dry run and the real
  import both refuse and list each remaining connection's pid · role · application_name. Some writes of an attached server
  (observation records) do not take the migration lock and would interleave with the moving transaction.
- Pre-checks: NUL bytes and broken UTF-8 in text columns, broken JSON and `\u0000` in JSON columns, a storage type that does
  not match the column, NULL primary keys, colliding snapshot key digests, and foreign keys pointing at missing rows. If
  anything hits, nothing is written and each hit is listed by table · column · reason · primary key. Fix those rows in the
  copy and run it again. `fd migrate verify` does not report a match for a source that fails the pre-checks either.
- Refusals the pre-checks cannot see (a CHECK on the target, the index entry size limit, privileges) hit the dry run exactly
  as they hit the real import. The dry run also holds the migration locks as the real import does.

### Moving a Postgres ledger back to SQLite

In a compose deployment, go back with "Going back within 72 hours" above. This section covers what the command itself does
and refuses.

`fd migrate export-sqlite` is the reverse of the import. It moves a Postgres ledger at version 18 into a **new** SQLite file
(version 18) - the way back to the SQLite build, carrying the items, claims, events and judgments written on Postgres since
the import. It runs the same transaction and the same comparison as the import (row counts, fingerprints over every column,
sequences), writes into a temporary file next to the target, and gives it the `--to` name only when the comparison matches.
On a mismatch or failure (and always on a dry run) no file is left behind. Exit codes are the same as the import's.

```bash
# 1. Stop the server that writes the Postgres ledger. If any connection other than this run is attached, the export refuses
#    and lists each remaining connection's pid · role · application_name. The source is only read, in one snapshot
#    (REPEATABLE READ READ ONLY).
# 2. Dry run: runs the same path to the end in a temporary file (naming is tried with a throwaway hard link in the same
#    directory), then deletes that file.
fd migrate export-sqlite --db <URL> --to fd-back.db --dry-run
# 3. Move. On success it prints the comparison table and "moved N rows, the comparison matched -> fd-back.db".
fd migrate export-sqlite --db <URL> --to fd-back.db
# 4. Always compare again before starting a server on the new file. This is the import's verify, unchanged.
fd migrate verify --from fd-back.db --db <URL>
```

- Do not skip step 4. The comparison before commit matches the moved snapshot against itself, so a write that a server
  attached to the source committed after the connection check is neither moved nor noticed. verify reads the source again in
  a fresh snapshot and catches that mismatch. If it does not match, delete the exported file and start again from step 1
  with the server stopped.

- If a file already sits at `--to`, or a `-wal` · `-shm` · `-journal` sits next to it, the export refuses. It never
  overwrites, and a leftover `-wal` cannot be replayed into the new file.
- The source must be at version 18. A Postgres ledger upgraded past it has no place in the SQLite build and is refused. So is
  a ledger whose tables or columns differ from version 18 (a hand-edited ledger). The version and the shape are measured in
  the snapshot the export copies from. The account is the same as for the import (a superuser or a member of `fd_owner` - it
  reads the sequences).
- A source holding a value the import's pre-checks would refuse (broken JSON and the like) is refused, since the exported
  file could not be imported again.
- Sequences: the AUTOINCREMENT tables (pick_eval · resource_hold · event · landing_queue) get the highest number Postgres
  issued back into `sqlite_sequence`, so it is never issued again. item_after is a SQLite rowid table and cannot remember a
  deleted top number - its seq values (the order) move into rowid unchanged, and the next number is max(rowid)+1 as in the
  SQLite build.
- The judgment search index (judgment_fts) does not exist on Postgres, so it is not moved. It is rebuilt from the moved
  judgment rows and checked against them.
- The exported file has mode 0644, and "moved" is printed only after the directory holding the new name has been fsynced.
- Stopped with Ctrl-C or SIGTERM, the export deletes its temporary file and exits 1. Ended by SIGKILL or a power cut, it may
  leave `.<name>.export-<digits>` (with its `-wal` · `-shm` · `.link`) in the `--to` directory. While no export is running,
  find them with `ls -a <directory> | grep '^\.fd-back\.db\.export-'` and delete them. They never got the name, so they are
  not a ledger.

**Rehearsing the way back.** Before the real move, run the round trip once: a copy of the ledger → `import-sqlite` into a
test Postgres ledger → start a server on it and write a few things (an item, a claim, a judgment) → stop the server and
`export-sqlite` → check that `fd migrate verify --from <exported file> --db <URL>` matches, and that a SQLite-build server
started on the exported file shows those writes and finds the judgment by search.

## Several repositories

When one product spans several repositories, declare the members in a root `.flightdeck.yaml` and
start once from the root to coordinate them all. See [Several repositories](docs/workspaces.md).

## When the server is down

`SessionStart` injects a banner, so no agent acts as if coordination exists when it does not:

```
⚠ 조정 서버 미도달(http://localhost:7420, 마지막 접속 14:02 · 37분 전).
  되는 것: 코드 작성·커밋·조사 전부. 이미 선점한 항목의 작업.
  안 되는 것: 새 항목 선점 · 다른 세션의 현재 상태.
```

("Coordination server unreachable. Still works: writing code, committing, research, work on items you
already claimed. Does not: claiming new items, other sessions' current state.")

Reads return the last snapshot marked as stale; judgments and notes queue up and replay on reconnect.
Claims, counters and the landing lane are refused — allowing them offline would make exclusion a lie.

## Troubleshooting

Run `fd doctor` first. It names each platform axis and the server state one by one.

| Symptom | Where to look |
|---|---|
| The board is empty | If nobody holds an item, that is normal. Otherwise start with `FD_URL` in `fd doctor` |
| Hooks do nothing | Run `bin/fd` directly — without Go you get a notice |
| Tools do not show up | Is `type` in `.mcp.json` set to `stdio`? |
| `브랜치 ?(못 읽음)` ("branch ? (unreadable)") on the board | The repositories are not mounted into the server container (`FD_REPOS`) |
| No card for a codex session | Hooks are not approved yet — open the `codex` TUI once |

## Design principles

1. **No write API for anything derivable.** Enforced by absence, not by validation.
2. **No feature that adds concepts a session must memorize.** Add tools and the tail of the
   discipline — handoffs, followups — is the first thing to drop.
3. **REST is the consistency path; MCP is a thin shell over it.** Both call the same functions.

And there is **no liveness boolean.** flightdeck never answers "is that session alive?" with yes or no;
it shows the age of each signal instead, because that judgment was wrong twice in measurement. The
reasons, and the list of what was deliberately not built, are in [Concepts](docs/concepts.md) and the
design of record, [DESIGN.md](DESIGN.md) (Korean).

## In production

In the August 2026 ledger, **24 sessions on one machine signalled within the same 10-minute window**,
and 22 of them were touching the same two repositories. In the first ten days, 601 overlap
prescriptions went out and sessions queued for landing 157 times across the two repositories.

## Development

The repository root is the plugin root. The server, CLI, MCP server and hooks all come from one Go
module in `server/`.

```bash
cd server
gofmt -l .                                            # must print nothing
go vet ./... && GOOS=linux go vet ./... && GOOS=windows go vet ./...
# tests: start the PG test container, pass it as FD_TEST_PG, take it down afterwards (takes a few minutes)
(p=fdtest-$(basename "$(dirname "$PWD")"); trap 'docker compose -f ../compose.test.yaml -p "$p" down' EXIT; trap 'exit 143' HUP INT TERM; docker compose -f ../compose.test.yaml -p "$p" up -d --wait && FD_TEST_PG="postgres://postgres@$(docker compose -f ../compose.test.yaml -p "$p" port pg-test 5432)/postgres?sslmode=disable" go test ./...)
```

The tests run on Postgres. The test-only container is `compose.test.yaml` (Docker required), and the tests
take its address from `FD_TEST_PG`. Without `FD_TEST_PG` the tests **fail** rather than skip. The one line
above starts, runs and takes down, and it takes the container down even when a test fails or the shell is cut off.

A push that changes code bumps `version` in `.claude-plugin/plugin.json` too — otherwise
`/plugin update` never fetches the new code. Layout and the reasons behind each gate are in
[Development](docs/development.md).

## Origin

flightdeck grew inside the plugin collection [kweiza-cc-plugins](https://github.com/Kweiza/kweiza-cc-plugins)
as `plugins/flightdeck`, and was split out here on 2026-09-24 at 0.38.3. Earlier history, and the commit
shas that the docs and code comments cite, live in that repository.

## License

MIT — see [LICENSE](LICENSE).
