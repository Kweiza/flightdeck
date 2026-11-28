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
docker compose up -d --build
curl -s localhost:7420/healthz
```

`/healthz` printing `{"ok":true,"api_version":"1","db_ok":true,…}` means you are done. `--build` builds the image from the
version you fetched - the image name is a fixed name that does not carry the version (`flightdeck:local`), so without it the
server starts from an old image built earlier.

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
| `FD_PG_INSTANCE` | server | The deployment name of the Postgres layer (default `flightdeck`). The db container's name (`<name>-db`) and both volumes' names (`<name>-pgdata`, `<name>-pgrun`) all come from it. **Write it only in the selection file**: if the shell gives it (even empty), the gate of `fdc`, of the start step of `fd setup` and of `fd-update` stops before calling compose, because shell values win over the selection file and a new shell without the value would pick another volume (an empty ledger). Production keeps the default; only rehearsals and tests write another value in that environment's selection file (the rehearsal in "Upgrade: SQLite ledger to Postgres" below) |
| `FD_PG_SERVER_PGPASS_HOST` · `FD_PG_BACKUP_PGPASS_HOST` | server | The pgpass files of the Postgres layer's two login accounts (server and daily dump). **No default**: if one is empty, `up` with the Postgres layer stops. Owner `FD_UID`, mode `0600`, line `db:5432:<db>:<account>:<password>`. They are the only place the passwords live (never in environment variables, command lines or URLs). **Keep them outside the server's mounts**: the server reads `FD_REPOS` (the whole home by default) (see "Running on Postgres" below) |
| `FD_PG_DATABASE` · `FD_PG_SERVER_USER` · `FD_PG_BACKUP_USER` | server | Database and login account names of the Postgres layer (default `flightdeck`, `fd_server` and `fd_backup`; lowercase letters, digits and `_` only). To change them, write them in the selection file: `FD_PG_DATABASE` decides which ledger it is, so like `FD_PG_INSTANCE` the gate stops if the shell gives it. `db-roles` creates and checks the accounts: the server account is a member of `fd_app`, the dump account of `pg_read_all_data`, and neither has privileged attributes. Each database gets its own dump directory (`daily/<db>/`), so after switching databases the new one neither counts nor deletes the old one's dumps |
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
socket volume, can connect as it. Those rules are read from a file baked into the image at every start, so an existing
volume gets them too. `db` publishes no port.

★ **Nothing is mounted from a version directory.** `db`, `db-roles` and `db-backup` run from one image that has the access
rules, the account script and the dump script baked in (`deploy/Dockerfile`, `flightdeck-pg:local`). Bound from the installed
plugin (`…/flightdeck/<version>`), they would break once the cleanup after an update removes that version directory: when a
reboot or a crash makes Docker start `db` again, the source is gone and the ledger database does not come up. The image sets
the file modes too (0644), so `db`'s postgres user reads the rules even from a checkout made under umask 077. After changing
these files, rebuild with `--build` (`/fd-update` starts that way).

### Running on Postgres

The command blocks in this section and in the three procedures below (upgrade, stopped before switching on, going back
within 72 hours) are meant to be pasted into a terminal as they are. The notes inside a block are `: '…'`, not `#`: an
interactive zsh (the default shell on macOS) does not read `#` as a comment (`interactivecomments` is off), so it runs a
`#` line as a command, and a `set -e` block stops on the first one. A block takes its docker queries (`docker ps`, `docker
inspect`) as standalone assignments (`x=$(docker …)`): `[ -n "$(docker …)" ]` and `set -- $(docker …)` read the empty output
of a failed docker as "none" or "stopped" and carry on, while an assignment ends with docker's failure and the block stops
there. Every `up` builds and starts with `--build`: the image names are fixed names that do not carry the version
(`flightdeck:local`, `flightdeck-pg:local`), so when an image built in another version's directory is left, `up -d` starts
from it.

**Setup** - once, as the `FD_UID` user.

```bash
: '1. Two volumes. Fixed names outside any project, so the ledger stays the same when each version lives in another
   directory, and `down -v` cannot remove them. Compose does not create them: if one is missing, `up` stops instead of
   creating an empty ledger. The names come from the deployment name of step 4 (FD_PG_INSTANCE).'
docker volume create flightdeck-pgdata && docker volume create flightdeck-pgrun

: '2. Places outside the mounts of the server (see the star note): the selection file and pgpass, and the dumps. If the
   dump directory is missing, Docker on Linux creates it root-owned 0755 and the first dump cannot write.'
sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck /var/backups/flightdeck

: '3. A pgpass for each login account. Owner FD_UID, mode 0600, one line each.'
(umask 077
 printf 'db:5432:flightdeck:fd_server:%s\n' "$(openssl rand -hex 24)" > /etc/flightdeck/server.pgpass
 printf 'db:5432:flightdeck:fd_backup:%s\n' "$(openssl rand -hex 24)" > /etc/flightdeck/backup.pgpass)

: '4. Write the choice down in the selection file. No passwords in it, only locations and names. Which ledger it is (the
   deployment name FD_PG_INSTANCE - the names of the db container and both volumes - and, if you change it, the database
   name FD_PG_DATABASE) lives only in this file. Write it under its switched-off name (.off): switching it on is one step of
   starting or upgrading below.'
cat > /etc/flightdeck/compose.env.off <<'EOF'
COMPOSE_FILE=compose.yaml:compose.postgres.yaml
FD_PG_INSTANCE=flightdeck
FD_PG_SERVER_PGPASS_HOST=/etc/flightdeck/server.pgpass
FD_PG_BACKUP_PGPASS_HOST=/etc/flightdeck/backup.pgpass
FD_BACKUP_HOST=/var/backups/flightdeck
EOF

: '5. Keep these in your shell config. fdc is the head that calls compose; fdrepos reads the repository mounts of the
   running server into FD_REPOS…FD_REPOS4, and fdwhere also reads its compose project (p) and directory (w) into this
   shell. The last line of each is what to give again in a new shell.'
fdc() {
  if [ -e ~/.flightdeck/compose.env ]; then echo '선택 파일의 옛 자리 ~/.flightdeck/compose.env 는 서버가 쓸 수 있는 /data 안이라 더는 읽지 않는다 - 내용을 확인한 뒤 /etc/flightdeck/compose.env 로 옮기고 다시 돌린다: sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck && mv -n ~/.flightdeck/compose.env /etc/flightdeck/compose.env (README 「Postgres 로 띄우기」)' >&2; false; elif [ -n "${FD_PG_INSTANCE+x}${FD_PG_DATABASE+x}" ]; then echo 'FD_PG_INSTANCE · FD_PG_DATABASE 는 셸에서 주지 않는다 - 어느 원장(볼륨 · DB)인지는 선택 파일 /etc/flightdeck/compose.env(꺼 두었으면 .off) 한 곳에 적는다. 셸 값은 선택 파일보다 먼저라 새 셸에서 빠지면 다른 원장으로 뜬다: 값을 선택 파일에 적고 unset FD_PG_INSTANCE FD_PG_DATABASE 뒤 다시 돌린다 (README 「Postgres 로 띄우기」)' >&2; false; fi || return
  FD_TOKEN="${FD_TOKEN:-$(cat ~/.flightdeck/token 2>/dev/null || sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' ~/.flightdeck/config.json 2>/dev/null)}" \
  FD_UID="${FD_UID:-$(id -u)}" FD_GID="${FD_GID:-$(id -g)}" \
  COMPOSE_ENV_FILES="${FDC_ENV:-$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)}" docker compose "$@"
}
fdrepos() {
  unset FD_REPOS FD_REPOS2 FD_REPOS3 FD_REPOS4
  mounts=$(docker inspect flightdeck --format '{{range .Mounts}}{{if eq .Source .Destination}}{{println .Source}}{{end}}{{end}}') && nmounts=$(printf '%s\n' "$mounts" | grep -c .) && if [ "$nmounts" -gt 4 ]; then echo "도는 서버의 저장소 마운트가 $nmounts 개다 - 슬롯(FD_REPOS…FD_REPOS4)은 넷이라 다섯째부터 빠진다: 멈추고 묻는다" >&2; false; fi &&
    export FD_REPOS="$(printf '%s\n' "$mounts" | sed -n 1p)" FD_REPOS2="$(printf '%s\n' "$mounts" | sed -n 2p)" FD_REPOS3="$(printf '%s\n' "$mounts" | sed -n 3p)" FD_REPOS4="$(printf '%s\n' "$mounts" | sed -n 4p)" &&
    echo "repository mounts (give again in a new shell): export FD_REPOS='$FD_REPOS' FD_REPOS2='$FD_REPOS2' FD_REPOS3='$FD_REPOS3' FD_REPOS4='$FD_REPOS4'"
}
fdwhere() {
  unset p w FD_REPOS FD_REPOS2 FD_REPOS3 FD_REPOS4
  fdp=$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project"}}') && fdw=$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}') && [ -n "$fdp" ] && [ -d "$fdw" ] &&
    fdrepos && p=$fdp w=$fdw && echo "running server (give again in a new shell): p='$p' w='$w'"
}
```

- When the selection file is switched on (`/etc/flightdeck/compose.env`), `fdc` hands it to compose as its env file
  (`COMPOSE_ENV_FILES`, the same as `--env-file`). That is the same test the start step of `fd setup` and the `fd-update`
  skill make. With `FDC_ENV` set it hands that file instead (the upgrade runs on the switched-off file before switching it
  on). `COMPOSE_FILE` layers the override; the paths are relative to the directory compose runs in, so run it from the root
  of the repository (or of the installed plugin). The token and user ids are filled in the way `fd-update` does; the
  repository mounts (`FD_REPOS` …) come from the shell as before (`fdrepos`). Shell values win over the selection file, and
  with it the directory's `.env` is not read.
- The first line of `fdc` is the same gate as the start step of `fd setup` and `fd-update` (the same text, in Korean). In
  either of two cases it prints what to do and does not call compose: a file is left at the selection file's old place (see
  the star note below), or the shell gives `FD_PG_INSTANCE` or `FD_PG_DATABASE` (even empty). Those two decide which ledger
  (volume, database) it is, and shell values win over the selection file: moving with values given in one shell and
  carrying on in a new shell without them would pick another volume (an empty ledger). So they live only in the selection
  file (setup step 4). The gate is followed by `|| return`, not `&&`, and the body ends in the one compose line - zsh does
  not stop the `set -e` block that called a function whose body ends in `if … fi && …` when that function fails (bash does).
- `fdrepos` and `fdwhere` read only while the server runs. They first clear this shell's values (`FD_REPOS` …, `p`, `w`), and
  when they cannot read (the server is down) they fail and leave them empty: the next block never inherits values read
  earlier from another server or at another time. `fdwhere` sets the three values only after **every** lookup (project,
  directory, mounts) has succeeded, all at once - when any one fails, all three stay empty. Once the server is down, give
  again what their last line printed.
- The mounts read **one line of docker's output as one path** and fill the slots in order - a path with a space
  (`/repos/team one`) is one path too. `set -- $(…)`, which splits on blanks, would cut that path in two and mount two paths
  that do not exist. More than four lines (there are four slots) would drop the fifth on silently, so the read stops
  instead (the message is in Korean, like the gate). The update chain of `fd-update` uses the lookup lines of these two
  functions word for word (a test compares them).
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
file's old place `~/.flightdeck/compose.env`, the start step of `fd setup`, `fd-update` and `fdc` print the command that
moves it and stop.

**Start** - for a new, empty ledger. Switch the selection file on, then start.

```bash
mv -n /etc/flightdeck/compose.env.off /etc/flightdeck/compose.env && [ ! -e /etc/flightdeck/compose.env.off ] &&
  fdc up -d --build && fdc ps -a
curl -s localhost:7420/healthz
```

It worked when `ps -a` shows `db`, `flightdeck` and `db-backup` healthy and `migrate` and `db-roles` `Exited (0)`, and
`/healthz` has `db_ok` and `db_locale_ok` true. On an empty ledger `migrate` lays the baseline first. **To bring a SQLite
ledger along**, do not start this way; follow "Upgrade: SQLite ledger to Postgres" below. If `~/.flightdeck/fd.db` is still
there without the move marker, `migrate` started this way refuses (a new ledger that skipped the move).

**Admin commands** run in the `migrate` service. The ledger (`/data`), the dump directory (`/backups`), psql, pg_dump and
pg_restore are all there, and it connects as the superuser over the socket. Use `--no-deps` while `db` is up. The version
and planned steps are `fdc run --rm --no-deps migrate selfcheck`. Keep the three below in your shell config too: `sql` is
superuser psql, `srvsql` is psql as the server account on the server's ledger (the server's own `FD_DB`; SQL on stdin), and
`nodump` checks that no `db-backup` container is running (when `docker ps` fails it stops instead of reading "none running").

```bash
sql() { fdc run --rm --no-deps --entrypoint psql migrate -X -v ON_ERROR_STOP=1 "$@"; }
srvsql() { fdc run --rm --no-deps -T --entrypoint sh flightdeck -c 'exec psql -X -v ON_ERROR_STOP=1 -d "$FD_DB" "$@"' sh "$@"; }
nodump() { if ! dumps=$(docker ps --filter label=com.docker.compose.service=db-backup --format '{{.Names}}(once={{.Label "com.docker.compose.oneoff"}})'); then echo 'docker ps failed - cannot tell whether a db-backup container is running: check Docker, then check again' >&2; return 1; fi; if [ -n "$dumps" ]; then echo "db-backup containers are running: $dumps - once=True is a dump started by hand (wait for it to end, then check again); once=False is the service container (stop db-backup in that project, then check again)" >&2; return 1; fi; }
```

- **Before running `fd migrate`, the export or a restore by hand, stop the daily dump and check that hand-run dumps have
  ended too**: `fdc stop db-backup && nodump`. pg_dump holds a lock on every table until it ends, and the DDL that raises
  the version fails when it cannot take that lock within lock_timeout (5s). When `fdc up` recreates migrate (new image or
  settings), compose stops the daily dump first and starts it again after migrate succeeds (`restart: true` in
  `depends_on`). `run` and `up --no-deps migrate` do not, and neither `stop` nor `restart: true` touches more than the
  service's own container: a dump started by hand with `fdc run --rm db-backup once` keeps running (`down` leaves it too).
  If `nodump` stops, look at the containers it names: wait for a dump started by hand to end, and stop a service container
  in its own project (which may live in another version's directory).
- **To change the passwords**, edit both pgpass files **in place** (replacing a file with a new one makes it disappear inside
  the containers that mount it), then `fdc up -d --build --force-recreate --no-deps db-roles flightdeck db-backup`. The server reads
  its file once at startup.

★ **One ledger, one override.** After committing, the import leaves a move marker next to the SQLite ledger
(`~/.flightdeck/fd.db.moved-to-postgres`). Starting the default compose without the selection file after the move runs the
default path's (SQLite) `migrate`, which sees the marker and neither opens nor creates the ledger, so the server stays down:
run `docker compose down`, check that the selection file is switched on, and start again with `fdc up -d --build`. The other way
round, `migrate` with the override refuses while an unmarked `fd.db` remains. Only two procedures remove the marker, by hand:
"Stopped before switching on" below (with both servers down and after the compare) and "Going back within 72 hours".

### Upgrade: SQLite ledger to Postgres

This takes the ledger you built on SQLite into a Postgres ledger. The server is down during the move. For 72 hours afterwards
you can go back to SQLite, carrying along what was written in Postgres ("Going back within 72 hours" below).

**The ledger must be at the import version (20).** The import only takes versions whose tables and columns match the last
SQLite version (20) - 20 added the account tables and actor columns to 19 and attributed the old rows to an owner account (it
does more than add an index, so the range is one version). An older ledger is raised first on this version's default path:
that is step 3 (step 3 always raises it to 20).

**Step 0 is setup, step 1 reads values into this shell, steps 2-8 are one block.** Step 0 is setup steps 1-5 of "Running on
Postgres" (volumes, places, pgpass, switched-off selection file, shell functions): do not switch it on or start it. Step 1
reads the running server's compose project and directory and its repository mounts into this shell. They cannot be read
once the server is down, so its last two lines are what to give again in a new shell ("Running it again" below). The block
is a subshell under `set -eu`, so the first command that fails stops the whole block where it is and no later step runs:
nothing moves while the old server is still up, and the selection file is not switched on after the account check failed.
The last "n done" line shows where it stopped. Fix the cause and paste the same block again: it skips what is done (2-5 once
the marker is there, 2-6 once the selection file is on). **Run it from the root of the repository (or of this version's
installed plugin).**

```bash
fdwhere
```

```bash
( set -eu
  on=/etc/flightdeck/compose.env; off=$on.off; mark=$HOME/.flightdeck/fd.db.moved-to-postgres
  : "${FD_REPOS:?run step 1 first (in a new shell, give its last two lines again)}"
  unset FDC_ENV
  if [ -e "$on" ] && [ ! -e "$mark" ]; then
    echo "the selection file ($on) is on but there is no move marker - before the move, rename it to $off and run again" >&2; exit 1
  fi
  if [ ! -e "$on" ]; then
    if [ ! -e "$mark" ]; then
      : "${p:?run step 1 first}" "${w:?run step 1 first}"
      : '2. Take it down from the directory of the old version and check that no container of that project is left (hand-run
         ones too) and that no project has a server container (flightdeck).'
      (cd "$w" && fdc -p "$p" down)
      left=$(docker ps -aq --filter label=com.docker.compose.project="$p")
      if [ -n "$left" ]; then echo "containers of the old version's project ($p) are left - take them down there, then run again" >&2; exit 1; fi
      names=$(docker ps -a --format '{{.Names}}')
      if printf '%s\n' "$names" | grep -qx flightdeck; then echo 'the server container (flightdeck) is still there - take it down in its project, then run again' >&2; exit 1; fi
      echo "2 done: it is down"
      : '3. Build the image of this version and raise the SQLite ledger to this version (nothing happens at 20). selfcheck
         must say ok.'
      fdc build
      fdc run --rm --no-deps migrate migrate --db /data/fd.db
      fdc run --rm --no-deps migrate selfcheck --db /data/fd.db
      echo "3 done: the SQLite ledger is at this version"
      : '4. Build the image of the Postgres layer from the switched-off selection file and lay only up to the import version
         (the baseline and the steps above it - stops at version 20). db starts with it.'
      export FDC_ENV="$off"; [ -f "$FDC_ENV" ]
      fdc build
      fdc run --rm migrate migrate --to 20
      echo "4 done: laid up to the import version"
      : '5. Dry run, then move. After committing, the move leaves a move marker (fd.db.moved-to-postgres) next to the
         source. No marker, no further.'
      if ! fdc run --rm --no-deps migrate migrate import-sqlite --from /data/fd.db --dry-run; then
        echo 'the dry run of step 5 stopped - if it says "대상이 비어 있지 않다" (the target is not empty), an earlier run may have committed (and been cut off before the marker): do not move again, measure with A of "Stopped before switching on"' >&2; exit 1
      fi
      if ! fdc run --rm --no-deps migrate migrate import-sqlite --from /data/fd.db; then
        echo "the move of step 5 stopped - if the line above says it committed (커밋했다), first leave the marker on this machine, then paste the block again: touch '$mark' (the /data/… that line names is the place inside the container - it is ~/.flightdeck on this machine). If it was cut off without that line, measure with A of \"Stopped before switching on\" before pasting again. Otherwise fix the cause and paste again" >&2; exit 1
      fi
      [ -e "$mark" ]
      echo "5 done: moved"
    fi
    : '6. On the switched-off selection file, create and check the login accounts (db-roles), then connect as the server
       account to the ledger of the server: the moved tables must be readable and TEMPORARY must be gone. Before that, read the
       versions of both the SQLite ledger and the Postgres ledger and compare them (verify) - both must be at the import version
       (20) and match. If either is not (a move that stopped on an older version), stop without changing the accounts - once
       the migrate that runs before db-roles raises a lower Postgres ledger and the accounts change, 7 cannot compare it with the
       SQLite ledger the marker holds back.'
    export FDC_ENV="$off"; [ -f "$FDC_ENV" ]
    fdc build
    if ! fdc run --rm migrate migrate verify --from /data/fd.db; then
      echo '6: the two ledgers do not match at the import version (20), or could not be compared (line above) - stopping without creating the accounts. If the line above names the version of the source or the target, the move stopped on an older version: do not go on with this version, follow "A move that stopped on an older version" under "Stopped before switching on". Otherwise measure with A in the same section' >&2; exit 1
    fi
    fdc up -d --build db-roles
    [ "$(docker wait "$(fdc ps -aq db-roles)")" = 0 ]
    srvsql <<'SQL'
SELECT current_user AS account, current_database() AS ledger, (SELECT count(*) FROM item) AS items, (SELECT count(*) FROM judgment) AS judgments;
SELECT 1 / (NOT has_database_privilege(current_database(), 'TEMPORARY'))::int AS no_temp_privilege;
SQL
    echo "6 done: the accounts are right"
  fi
  : '7. Switch on and start only after comparing the Postgres ledger the selection file picks with the SQLite ledger -
     carrying on in a new shell compares again before starting. A server container that was only created and never ran
     (the up of step 8 stopped before starting the server) never wrote to the ledger, so it is compared. If the server here
     has already run (step 8 started it, then stopped), it is a ledger the server wrote to, so it is not compared with
     SQLite; instead the settings that created that server and db (the config-hash label of compose) must equal the
     settings rendered now from the selection file and the repository mounts - only then does the up of step 8 start the
     same volume and the same database without creating them again. Once on, later updates and restarts start on Postgres.'
  srv=$(fdc ps -aq flightdeck); st=
  if [ -n "$srv" ]; then st=$(docker inspect -f '{{.State.Status}}' "$srv"); fi
  if [ -z "$srv" ] || [ "$st" = created ]; then
    fdc run --rm migrate migrate verify --from /data/fd.db
  else
    for s in db flightdeck; do
      c=$(fdc ps -aq "$s")
      if [ -z "$c" ]; then echo "7: the server has run but there is no $s container here - cannot measure the ledger the server wrote to: stop and ask" >&2; exit 1; fi
      l=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.config-hash"}}' "$c"); h=$(fdc config --hash "$s")
      if [ "$h" != "$s $l" ]; then echo "7: the settings that created the $s container that ran here differ from the settings rendered now from the selection file, the repository mounts and the token - step 8 could create it again on another ledger (volume, database) or other mounts, so it does not start: give the selection file from when that server started and the last two lines of step 1 as they were, then paste again (see what differs with docker inspect $c and fdc config $s)" >&2; exit 1; fi
    done
    echo "7: the server here has already run - it wrote to this ledger, so no compare with SQLite, and the settings that created that server and db are the same as now"
  fi
  if [ ! -e "$on" ]; then mv -n "$off" "$on"; [ -e "$on" ]; [ ! -e "$off" ]; fi
  unset FDC_ENV
  echo "7 done: selection file on"
  : '8. Start on the Postgres ledger. It worked if the board shows the moved items.'
  fdc up -d --build
  fdc ps -a
  echo "8 done: running on the Postgres ledger"
)
curl -s localhost:7420/healthz
```

`/healthz` must have `db_ok` and `db_locale_ok` true.

- **NUL becomes `\x00` in free text only; a NUL in an identifier or a JSON key is refused.** SQLite holds NUL bytes (0x00)
  in text columns; Postgres cannot. Step 5's dry run and move and step 7's comparison do not refuse a NUL in free text (text
  columns such as bodies and titles, and string values in JSON columns): they move it written as the four characters `\x00`,
  list the changed columns under "옮기며 바꾼 원본 칸" ("source columns changed in the move") by table · column · rule · row
  key, and repeat the number of changed rows under the verdict line. Every changed column is kept with its original bytes in
  `~/.flightdeck/fd.db.rewritten-<time>.jsonl` - that file is the authority on those original bytes, so do not delete it.
  A NUL in free text needs no hand-editing step. A NUL in an identifier column (primary key · UNIQUE · a column that points
  at another row) or in a JSON key is not changed: step 5's dry run refuses it in the pre-checks - fix that row in the copy
  and run it again. The SQLite ledger file is left as it is. The rule and how to reverse it are in "Moving a SQLite ledger to
  Postgres" below.
- If it stops **before step 5 leaves the marker** (a refusal or failure in 2-5), nothing is committed (the import is one
  transaction), **with two exceptions**. If step 5's move stops with "옮겼다(N 행, 커밋했다) - 그런데 원본 옆에 이관 표지를
  남기지 못했다" ("moved (N rows, committed) - but could not leave the move marker next to the source"), it did commit. If
  step 5's move was cut off **without a result** (no error line and no "5 done": the process, the shell or the machine died
  right after the commit), you do not know whether it committed. No marker does not mean nothing was committed. Either way
  the SQLite ledger was only read. If it said it committed, first leave the marker **on this machine** with `touch
  ~/.flightdeck/fd.db.moved-to-postgres`, then paste the block again - the `touch /data/…` the move's line names is the place
  inside the container (`/data` is `~/.flightdeck` on this machine), not a place on this machine. When it stops, the block
  names that command again with this machine's place. If you do not know the result, measure the Postgres ledger's rows with A of "Stopped before
  switching on" before pasting again - pasting again by mistake does not move twice: the import only takes an empty
  ledger, so step 5's dry run stops with "대상이 비어 있지 않다" (the target is not empty) and the block points to A. For any
  other cause, fix it and paste the block again. To go back to SQLite, measure first with "Stopped before switching on" and
  then choose.
- If it stops **after step 5 left the marker but before step 7 switched the selection file on** (6 or 7), the ledger is
  already in Postgres and no server has written to it yet. Fix the cause and paste the block again: it skips 2-5 and starts
  at 6. To go back to SQLite, see "Stopped before switching on".
- If it stops **after step 7 switched the selection file on** (8), pasting the block again runs step 7's compare and step 8.
  A server container being there is not enough to skip the compare: if it was only created and never ran (step 8's up
  stopped at db or migrate), it is compared; if it already ran, instead of the SQLite ledger the settings that created that
  server and db (compose's config-hash label) are compared with the settings rendered now (`fdc config --hash`), and step 8
  runs only when they are the same - if the selection file meanwhile points to another deployment name or database, step
  8's up would create them again on another volume or database. To go back to SQLite, follow "Going back within 72 hours". After
  "8 done" do not use this block again: once the server is removed with `fdc down`, the ledger the server wrote to no longer
  matches the moved SQLite ledger and step 7 stops. To start again, `fdc up -d --build`.
- **Which ledger it is lives only in the selection file.** The deployment name (`FD_PG_INSTANCE`: the names of the db
  container and both volumes) and the database (`FD_PG_DATABASE`) go in the selection file of setup step 4, never in the
  shell: if the shell gives them, the gate of `fdc` stops before calling compose. Shell values win over the selection file,
  so running 4-6 with values given in one shell and carrying on with 8 in a new shell without them would pick another
  volume (an empty ledger). Even in a new shell, step 7 compares the ledger that file picks with the SQLite ledger again
  before starting.
- **Running it again (in a new shell).** `fdc`, `fdrepos`, `fdwhere` and `srvsql` come from your shell config. Do not set
  `FDC_ENV`: the block picks the file from the selection file's state (the switched-off file before switching on, the
  switched-on file after). Once the server is down step 1 cannot read anything, so give step 1's last two lines again as they
  were: `export FD_REPOS=… FD_REPOS2=… FD_REPOS3=… FD_REPOS4=…` and `p='…' w='…'` (once the marker is there, the repository
  mounts are enough). Running step 1 again cannot read them and clears them: give the two lines. Then paste the 2-8 block.
  Without the repository mounts the block stops; step 8 starts the server with them, so give the values step 1 read.
- After the move the SQLite ledger (`fd.db` and its side files) may stay where it is: the marker blocks it on the SQLite
  side. To tidy up (optional), with the server down, move `fd.db`, `fd.db-wal` and `fd.db-shm` to other names in the same
  directory and make them read-only. **Do not move the marker**: without it a new SQLite ledger could appear in that place.
- **If you moved a copy** (a `VACUUM INTO` copy outside the ledger's place given to `--from`), the move marker sits next to
  the copy. **Always leave the marker in the ledger's place too, even if you move the original out of it.** Without a marker
  there, `migrate` with the override sees an `fd.db` left there as a ledger that was never moved and refuses, and once the
  original is gone, `migrate` started without the override creates a new, empty SQLite ledger in that place. Leave the marker
  after checking that the original matches what was moved; moving the original out (optional) comes after that:
  `( set -eu; unset FDC_ENV; [ -e /etc/flightdeck/compose.env ] || export FDC_ENV=/etc/flightdeck/compose.env.off; fdc run --rm migrate migrate verify --from /data/fd.db; touch ~/.flightdeck/fd.db.moved-to-postgres )`
- **If both ledgers have already been written to separately** (a SQLite server wrote to the old ledger after the move, for
  example), take both servers down and **delete neither ledger**. `verify --from` lists the rows that differ by primary
  key; a person decides which side to keep. There is no merge command.
- The refusals and the comparison in detail are in "Moving a SQLite ledger to Postgres" below.

**Keep the rehearsal apart from production.** The server container name (`flightdeck`) and port 7420 are fixed, so it
cannot run next to the production server on the same Docker, and with the production volume names a rehearsal's `migrate`
connects to the production ledger as the superuser: rehearse on **another Docker environment** (another machine or a VM).
Even there, give values that differ from production: the project name (`fdc -p <rehearsal>`), the home (`HOME`: `/data` is
`~/.flightdeck`), and, written in that environment's selection file, the deployment name `FD_PG_INSTANCE` (the db
container's and both volumes' names) and `FD_BACKUP_HOST` (`fdc` stops if the shell gives them). Create the volumes with
`docker volume create <deployment>-pgdata && docker volume create <deployment>-pgrun`, and empty them afterwards with
`fdc -p <rehearsal> down` and `docker volume rm <deployment>-pgdata <deployment>-pgrun`. ★ **Never remove
`flightdeck-pgdata` or `flightdeck-pgrun`**: that is the production ledger. Check the names once more with `docker volume ls`
before removing anything.

#### Stopped before switching on

For when the upgrade stopped before step 7 switched the selection file on, and you are deciding whether to go on or back to
SQLite (if step 2 stopped before taking the old server down, nothing changed and this is not needed). While the selection
file is off the server never starts on the Postgres layer, so no server has written to the Postgres ledger. **Do not choose
from the marker alone**: the import writes the marker **after** it commits, so it may have committed without one, and going
back to SQLite and writing there would split the two ledgers (a new move would also stop with "not empty"). So measure with
both servers down (A), then choose from the table. Run it where the upgrade ran (the root of the repository or of this
version's installed plugin).

**If it stopped before "4 done"** (the last "n done" line is "2 done" or "3 done"), step 5's move never ran: the Postgres
ledger has no ledger rows, so choose without measuring A (without step 4's image and baseline, A only ends in "not
measured"). To go on, fix the cause and paste the upgrade block again. To go back to the SQLite path of this version, run B.
**If it stopped at step 3** ("2 done" is the last line: this version's image, the SQLite version raise or selfcheck stopped),
even the SQLite path of this version may not start, so start the old version again: `(cd "$w" && fdc -p "$p" up -d --build)`
(with the `p`, `w` and repository mounts step 1 read; in a new shell, give step 1's last two lines again). Keep `--build`:
the image step 3 built from this version holds the same fixed name (`flightdeck:local`), so without it even the old
directory starts this version's image. `--build` builds it again from the old version's source. If the old version
stops with "the database version is higher than this version knows", part of step 3's raise landed: fix the cause and paste
the upgrade block again (step 3 carries the raise on).

```bash
( set -eu
  : 'A. Measure; this changes nothing. Check that the selection file is off and that neither server runs, read the marker,
     and compare the Postgres ledger with the SQLite ledger. In the comparison table, the target rows are the rows of the
     Postgres ledger.'
  on=/etc/flightdeck/compose.env
  unset FDC_ENV
  if [ -e "$on" ]; then echo 'the selection file is on - this is not the branch for it ("Going back within 72 hours")' >&2; exit 1; fi
  export FDC_ENV="$on.off"; [ -f "$FDC_ENV" ]
  up=$(docker ps -q --filter label=com.docker.compose.service=flightdeck)
  if [ -n "$up" ]; then echo 'a server is running - take it down in its project, then measure again' >&2; exit 1; fi
  if [ -e "$HOME/.flightdeck/fd.db.moved-to-postgres" ]; then echo 'A: the move marker is there'; else echo 'A: there is no move marker'; fi
  if fdc run --rm migrate migrate verify --from /data/fd.db; then echo 'A done: match'; else echo 'A done: no match or not measured - read the output above'; fi
)
```

| What A shows | Meaning | Choose |
|---|---|---|
| marker · match | moved, and nothing wrote to either side since | to go on, paste the upgrade's 2-8 block again (it starts at 6); to go back, B |
| no marker · match | moved (committed) but the marker was not left | to go on, `touch ~/.flightdeck/fd.db.moved-to-postgres`, then paste the block again; to go back, B |
| no marker · no match, and every target row count in the table is 0 | stopped before committing: the Postgres ledger has only the baseline | fix the cause and paste the block again, or to go back, B |
| no marker · no table, "대상이 빈 DB 다" (the target is an empty database) | stopped before the baseline: the Postgres ledger is empty | fix the cause and paste the block again, or to go back, B |
| any other run without a table | not measured (connection, image, version, side files) | fix the cause above and run A again. If the upgrade stopped before "4 done", choose as above without measuring A |
| anything else (marker but no match; rows in the target but no match) | something wrote to one side separately | **Stop.** Delete neither ledger. Look at the rows that differ (primary keys) in the table; a person decides |

**A move that stopped on an older version (import version 19 or lower) does not go on with this version.** The comparison,
the import and the export only take a ledger at the import version (20). Raising that Postgres ledger leaves the SQLite ledger
at the older version behind the marker, so step 7 refuses it with "raise it first", and from there the older version cannot
take over either (it refuses a version 20 ledger). So at step 6, before the accounts change (db-roles), this version's block
reads the versions of both the SQLite ledger and the Postgres ledger and compares them (verify), and if either is not at the
import version it stops without raising anything and points here. If the Postgres ledger is on the older version, finish with
the older version's block in its directory, or go back with B, and then update to this version. If only the Postgres ledger
has been raised to this version (the marker held the SQLite ledger back on the older version), no version's block can go on -
**stop.** Delete neither ledger; a person decides.

Run B only when A's table says "to go back, B", or when the upgrade stopped before "4 done". It takes this directory's
Postgres layer (db and the one-shots) down, removes the marker (no server ever wrote to the Postgres ledger, and it matches
the SQLite ledger or is empty), and starts on this version's default path. The selection file stays off. If the import left
the marker, it also moved the ledger's pre-upgrade copies into `fd.db.pre-import-<time>.bak/` - since you go back to the same
ledger, move them back next to it by hand if you need one for `fd migrate --rollback`.

```bash
( set -eu
  : 'B. Go back to SQLite.'
  on=/etc/flightdeck/compose.env
  unset FDC_ENV
  if [ -e "$on" ]; then echo 'the selection file is on - this is not the branch for it ("Going back within 72 hours")' >&2; exit 1; fi
  : "${FD_REPOS:?give again the repository mount line from the upgrade step 1}"
  export FDC_ENV="$on.off"; [ -f "$FDC_ENV" ]
  fdc down
  unset FDC_ENV
  rm -f "$HOME/.flightdeck/fd.db.moved-to-postgres"
  fdc up -d --build
  fdc ps -a
  echo "B done: running on the SQLite ledger"
)
```

- The Postgres ledger (the volume) stays. If a moved copy is left there (the table's first two rows), a later move refuses it
  as "not empty": the import only takes an empty target.

#### Going back within 72 hours

The 72 hours after the move are the rollback window. `export-sqlite` moves the Postgres ledger (import version 20)
into a **new** SQLite file at the last SQLite version (20), so the items, claims and judgments written in Postgres after the
move come back too. During the window, do not update to a flightdeck version that raises the Postgres schema past the last
SQLite version: `export-sqlite` only takes a ledger at its own import version. **`migrate` holds upgrades inside the
window:** for 72 hours from the move time it does not apply a migration that does more than add indexes, and stops naming
when the window closes (UTC) - the server does not start either. The move time comes from the record the import leaves in the
Postgres ledger when it commits, whichever way the ledger is opened (`--db`, `FD_DB` or the container). A ledger without that
record (moved by an older version) is measured by the move time in this environment's move marker (`fd.db.moved-to-postgres`;
only for that ledger when the marker names a ledger id), and with neither record nor marker the window counts as closed. Bringing this version up inside the window
after a move on an older version (import version 19 or lower) is that case - migration 020 does more than add indexes, so once
applied, going down (`--to`) is refused, the older version's `export-sqlite` refuses 20, and `--rollback`, which restores the
pre-upgrade dump, loses the writes after it. That ledger stays at version 19, so go back with the older version's copy of this
section in its directory. If you will not go back to the older version, raise it with
`fdc run --rm migrate migrate --allow-in-rollback-window` and then bring it up - after that, go back with this version's
`export-sqlite`. Before the dump, that upgrade looks for rows over the index key limit and, if
there are any, stops with their coordinates and leaves the ledger at its current version (the same limit as the pre-checks in
"Moving a SQLite ledger to Postgres" below). Do not fix those rows - it is an append-only audit ledger, and cutting a value
makes it differ from the SQLite source and the dumps. Report the coordinates in an issue.
**Only a deployment whose selection file is on** goes back this way; an upgrade that stopped before switching on is
"Stopped before switching on" above. As in the upgrade, step 1 reads values into this shell (`fdwhere`; once step 5 has
taken the server down they cannot be read, so its last two lines are what to give again in a new shell) and steps 2-7 are
one block (it stops whole at the first failure). Every compose command in the block runs in the directory and project step
1 read: a `stop` or `down` run from another version's directory looks at another project and stops nothing.

**Measure every time before putting the file in place.** Step 4 checks that no server runs and compares the exported
`fd-back.db` with the Postgres ledger **as it is now**, without conversion (`--exported`). Pasting the block again to carry on after step 5 does not skip it
either (it starts `db` again from the switched-off selection file to compare): the compare is the gate, not the fact that the
file exists. A file that matches is the same as the ledger now, so it is not stale. When the block starts fresh (the
selection file is on) and an `fd-back.db` from an earlier run is there, it does not start: only the file this run writes
goes into place.

```bash
fdwhere
```

```bash
( set -eu
  on=/etc/flightdeck/compose.env; off=$on.off; d=$HOME/.flightdeck
  : "${p:?run step 1 first}" "${w:?run step 1 first}" "${FD_REPOS:?run step 1 first}"
  unset FDC_ENV
  cd "$w"
  if [ -e "$on" ]; then
    : 'Only the file this run writes goes into the place of the ledger: if an earlier run left one, do not start.'
    for s in '' -wal -shm -journal; do
      if [ -e "$d/fd-back.db$s" ]; then echo "$d/fd-back.db$s from an earlier run is still there - move it to a side name, then run again" >&2; exit 1; fi
    done
    : '2. Stop the server and the daily dump (leave db up), and check that hand-run dumps have ended too. The export
       refuses while other connections are attached.'
    fdc -p "$p" stop flightdeck db-backup
    nodump
    echo "2 done: stopped"
    : '3. Dry run, then write.'
    fdc -p "$p" run --rm --no-deps migrate migrate export-sqlite --to /data/fd-back.db --dry-run
    fdc -p "$p" run --rm --no-deps migrate migrate export-sqlite --to /data/fd-back.db
    [ -f "$d/fd-back.db" ]
    echo "3 done: written"
  elif [ -f "$off" ] && [ -f "$d/fd-back.db" ]; then
    echo "carrying on after step 5: the selection file is off and $d/fd-back.db is there - comparing again before putting it in place"
    export FDC_ENV="$off"
  else
    echo "the selection file is off and there is no exported ledger ($d/fd-back.db) - for an upgrade that stopped before switching on, see \"Stopped before switching on\"; if this block stopped after step 6, run only step 7 again: (cd \"$w\" && fdc -p \"$p\" up -d --build)" >&2
    exit 1
  fi
  : '4. Measure every time before putting it in place: no server runs, and the exported file matches the Postgres ledger
     as it is now. Only a match goes on.'
  up=$(docker ps -q --filter label=com.docker.compose.service=flightdeck)
  if [ -n "$up" ]; then
    echo 'a server is running - it can write to the ledger even after the compare: take it down in its project, then run again' >&2; exit 1
  fi
  fdc -p "$p" run --rm migrate migrate verify --from /data/fd-back.db --exported
  echo "4 done: the exported file matches the ledger now"
  : '5. Switch the selection file off and take the Postgres layer down. Left on, the next update or restart would layer
     Postgres again.'
  if [ -e "$on" ]; then mv -n "$on" "$off"; [ ! -e "$on" ]; fi
  export FDC_ENV="$off"
  fdc -p "$p" down
  unset FDC_ENV
  echo "5 done: selection file off, layer down"
  : '6. Put the new file in the place of the ledger. The ledger already there (the moved SQLite ledger and its side files)
     is not deleted but moved to a side name. Its pre-upgrade copies (fd.db.bak-*) go to a side directory
     (fd.db.pre-back-<time>.bak): they are not copies of the exported ledger, so they must not sit where fd migrate
     --rollback picks. If any of them cannot be moved, it stops. Only then is the move marker removed: left in place, the
     SQLite migrate will not open this ledger.'
  cd "$d"; t=$(date -u +%Y%m%dT%H%M%SZ)
  for s in '' -wal -shm -journal; do if [ -e "fd.db$s" ]; then mv -n "fd.db$s" "fd.db.pre-back-$t$s"; fi; done
  for s in '' -wal -shm -journal; do [ ! -e "fd.db$s" ]; done
  old=$(find . -maxdepth 1 -name 'fd.db.bak-*')
  if [ -n "$old" ]; then
    mkdir "fd.db.pre-back-$t.bak"
    find . -maxdepth 1 -name 'fd.db.bak-*' -exec mv -n {} "fd.db.pre-back-$t.bak/" \;
    left=$(find . -maxdepth 1 -name 'fd.db.bak-*'); [ -z "$left" ]
  fi
  for s in '' -wal -shm -journal; do if [ -e "fd-back.db$s" ]; then mv -n "fd-back.db$s" "fd.db$s"; fi; done
  for s in '' -wal -shm -journal; do [ ! -e "fd-back.db$s" ]; done
  [ -f fd.db ]; rm -f fd.db.moved-to-postgres
  echo "6 done: the exported ledger is in place"
  : '7. Start on the default path (SQLite).'
  cd "$w"
  fdc -p "$p" up -d --build
  fdc -p "$p" ps -a
  echo "7 done: running on the SQLite ledger"
)
```

- If it stops **before "4 done"** (stopping, the export or the compare), the only change is that the server and the daily
  dump are stopped; the selection file is still on. If an `fd-back.db` was written, the block stops on it when pasted
  again: move it to a side name (the export does not overwrite an existing file, and this block only puts in place the file
  this run wrote). Find and stop whatever is attached or running and paste the block again. To give up the rollback, start
  again on the Postgres ledger as it is with `(cd "$w" && fdc -p "$p" up -d --build)`.
- If it stops **after step 5 switched the selection file off** (step 5's down, 6 or 7), pasting the block again runs step 4
  again before putting anything in place: it checks that no server runs, starts `db` again from the switched-off selection
  file and compares the remaining `fd-back.db` with the Postgres ledger now. If the ledger was written in between, or the
  file is from another time, step 4 stops and nothing is put in place. Step 6 only moves the old ledger to a side name
  (`fd.db.pre-back-<time>`) and its pre-upgrade copies to a side directory (`fd.db.pre-back-<time>.bak/`), never deletes
  them. In a new shell, give step 1's last two lines again (with the server down,
  step 1 reads nothing). If step 6 put the file in place and step 7 stopped, the block says so and stops: run only step 7
  again. To give up the rollback when step 4 stops, switch the selection file on again
  (`mv -n /etc/flightdeck/compose.env.off /etc/flightdeck/compose.env`), start the Postgres ledger again with
  `(cd "$w" && fdc -p "$p" up -d --build)`, then move the remaining `fd-back.db` to a side name.
- **If there is no server container** and step 1 cannot read anything (the upgrade's step 8 stopped before creating the
  server), give `w=$PWD` from where the upgrade ran and that directory's project
  `p=$(docker inspect "$(fdc ps -aq db)" -f '{{index .Config.Labels "com.docker.compose.project"}}')`, and give the repository
  mount line from the upgrade's step 1 again.
- The Postgres ledger (volume `flightdeck-pgdata`) stays. A later move needs an empty target: the import refuses otherwise.

**Going back to SQLite is not going back to an older version's image.** Step 7 starts this version's image, and the ledger
put in place is at version 20 (the export always writes the last SQLite version). flightdeck at version 19 or lower refuses
that ledger with "the database version is higher than this version knows", and version 20 is not lowered (`--to N` below) - the
ledger put in place only runs on this version's image or a newer one.

`--to N` (N below the current version) only lowers steps that just add indexes: it drops that index and the version row and
keeps every record (version 19 → 18 is such a step). Below a step that changed tables or columns it refuses and changes
nothing (version 20 → 19 is such a step - step 020 added the account tables and actor columns and wrote the owner account into
the old rows). Do not lower with `fd migrate --rollback`: that command **overwrites** the ledger with a pre-upgrade copy and loses
what was written after it. The rollback checks the copy before overwriting: it refuses a copy that is not complete (a
`-journal` left next to it, a failing integrity_check) or that is not this ledger's pre-upgrade copy (its version history
differs from the ledger's - an old copy left behind after the ledger was replaced by an export or a restore). It also
refuses a copy whose version history is the **same** as the ledger's: no upgrade was committed after that copy (the upgrade
rolled back at its first step, or an earlier rollback already overwrote the ledger with it), so overwriting would only lose
what was written since. When it does overwrite, it keeps the current ledger as `fd.db.pre-rollback-<time>` (with its side
files). Neither the pre-upgrade copy nor the kept ledger ever replaces an existing name - if the name is taken within the same
second, it gets a number from `-001` on (a numbered name sorts later, so the rollback picks it as the newest). When an upgrade
stops, the failure hint names that ledger, shell-quoted, as `fd migrate --rollback --db '<ledger>'` -
without `--db` it would roll back this environment's default ledger.

**When you put a ledger file in place by hand** (restoring a copy, or cloning a ledger from another place or machine), first
move the `fd.db.bak-*` files there (with their side files) into a new directory (such as `fd.db.pre-restore-<time>.bak/`). Do
not delete them. A ledger cloned at the same version has the same version history (versions and apply times), so the version
history check cannot tell another ledger's copy apart - left in place, `fd migrate --rollback` could overwrite with it. The
import and the export move them on their own (the two sections below).

### Daily dumps of a Postgres ledger

The `db-backup` service writes a `pg_dump -Fc` once a day to `$FD_BACKUP_HOST/daily/<db>/fd-daily-<UTC>.dump` and keeps the
newest 14. It runs only with the Postgres layer (setup is in "Running on Postgres"). It rotates only after a dump succeeds,
and only one run works in a database's directory at a time. The container turns unhealthy when the newest dump is older
than the interval plus two checks plus the deadline (26h30m by default).

`fdc run --rm --no-deps db-backup config` prints the settings on one line. `once` takes one dump by hand (0 = dumped, or not
due yet; 1 = failed; 3 = another run is working in the same directory, nothing done).

**Restore** - in the `migrate` service (superuser over the socket; the dumps are visible under `/backups`). `sql`, `srvsql`
and `nodump` are the admin commands from "Running on Postgres". Go on only after a step prints its last line. First stop the server and the daily dump, which drops the
connections to the database you are restoring.

```bash
fdc stop flightdeck db-backup && nodump && echo "stopped"
```

1. Read from the dump which database it restores and compare it with `FD_PG_DATABASE`. `--create` makes the database under
   **the name written in the dump**, not the current `FD_PG_DATABASE`; if they differ, the server does not see the restored
   database. **If they differ, stop**: a person decides which name to use (whether to change `FD_PG_DATABASE` and the pgpass
   lines to the dump's name). Fill in the placeholders of `f` below with the dump to restore.

   ```bash
   f='/backups/daily/<db dir>/fd-daily-<UTC>.dump'
   fdc run --rm --no-deps --entrypoint sh migrate -c 'd=$(pg_restore -l "$1" | sed -n "s/^; *dbname: //p")
     echo "dump database: $d, FD_PG_DATABASE: $PGDATABASE"; [ -n "$d" ] && [ "$d" = "$PGDATABASE" ]' sh "$f" && echo "1 done: same name"
   ```

   If a database by that name exists, do not drop it; **rename it and keep it** (`<db>` is the name read above):
   `sql -d postgres -c 'ALTER DATABASE "<db>" RENAME TO "<db>_before_restore"'`. Drop the old one after the check in step 4.
2. Roles are cluster-wide, so they are not in the dump. Restoring into the same volume (`flightdeck-pgdata`), they already
   exist. If the volume itself is gone, create a new one, run `fdc up -d --build db-roles` to create the group roles, the login
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
   fdc up -d --build db-roles && [ "$(docker wait "$(fdc ps -aq db-roles)")" = 0 ] && srvsql <<'SQL' && echo "4 done: it is the restored ledger"
   SELECT current_database() AS ledger, d.oid, o.oid AS old_oid, (SELECT count(*) FROM item) AS items,
          (SELECT count(*) FROM judgment) AS judgments, (SELECT max(at) FROM event) AS last_event
     FROM pg_database d LEFT JOIN pg_database o ON o.datname = d.datname || '_before_restore' WHERE d.datname = current_database();
   SELECT 1 / (d.oid IS DISTINCT FROM o.oid)::int AS new_database
     FROM pg_database d LEFT JOIN pg_database o ON o.datname = d.datname || '_before_restore' WHERE d.datname = current_database();
   SELECT 1 / (count(*) > 0)::int AS has_items FROM item;
   SELECT 1 / (NOT has_database_privilege(current_database(), 'TEMPORARY'))::int AS no_temp_privilege;
   SQL
   ```

5. Start again with `fdc up -d --build`. Drop the renamed old database once the check is done
   (`sql -d postgres -c 'DROP DATABASE "<db>_before_restore"'`).

### Moving a SQLite ledger to Postgres

In a compose deployment, move with "Upgrade: SQLite ledger to Postgres" above. This section covers what the commands
themselves do and refuse; `<URL>` below connects as the superuser or a member of `fd_owner` (in the compose `migrate` service
`FD_DB` is that URL, so leave out `--db`).

`fd migrate import-sqlite` moves a ledger at the import version (20 - the version whose tables and columns match the last
SQLite version) into an **empty** Postgres ledger in one
transaction. Inside that transaction it then compares the two ledgers and does not commit on any mismatch.
`fd migrate verify` runs only that comparison again (it writes nothing). The comparison covers, per table, the row count and
a fingerprint over every column, plus the identity sequences (the next number must exceed the highest number the source
ever issued); mismatched rows are listed by primary key. Exit codes: 0 = moved · matched, 1 = refused · mismatch · failure,
2 = usage.

```bash
: '1. Stop the server that writes the source. Pass the ledger file itself to --from, or, to keep a copy, take one with
   VACUUM INTO. Do not copy it with cp (see below).'
sqlite3 fd.db "VACUUM INTO 'fd-copy.db'"
: '2. Lay the target database up to the import version (the baseline and the steps above it), stopping at 20.'
fd migrate --db <URL> --to 20
: '3. Dry run: runs the same transaction as the real import to the end and rolls it back instead of committing.'
fd migrate import-sqlite --from fd-copy.db --db <URL> --dry-run
: '4. Move. On success it prints the comparison table and the number of rows moved.'
fd migrate import-sqlite --from fd-copy.db --db <URL>
: '5. (Optional) compare again before starting the server on the new ledger. Once the server writes, they differ.'
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
- A source older than version 20 is refused. First upgrade the copy with this version of fd (`fd migrate --db <copy>`). The
  range comes from one place in the code (the steps that only add indexes), and the import, the comparison and the export
  all use it. It is one version (20) now - step 020 below it does more than add an index.
- Above the comparison table the report prints the source's owner attribution: whether the owner account (`owner`) exists,
  and per actor column (the account of machines and sessions, the author of items, judgments and revisions, the channel of
  judgments) how many rows are empty. It is not part of the verdict - this version's writes do not fill the actor columns
  yet, so rows written after the upgrade leave them empty.
- A source whose tables or columns differ from the import version (a hand-edited ledger) is refused.
- A target with even one row in a business table is refused, as is one whose identity sequence has ever issued a number
  (even after the rows were deleted), one not at the import version (20), or one that fails the locale gate.
- If any connection other than this run is attached to the target database (a server, psql, ...), the dry run and the real
  import both refuse and list each remaining connection's pid · role · application_name. Some writes of an attached server
  (observation records) do not take the migration lock and would interleave with the moving transaction.
- Pre-checks: broken UTF-8 in text columns, broken JSON in JSON columns (a JSON column with a raw NUL byte inside a string
  is not a JSON document either and lands here) and JSON with a lone surrogate, a NUL in an identifier column and U+0000 in
  a JSON key (below), a storage type that does not match the column, NULL primary keys, primary key · UNIQUE columns that collide once moved (below), foreign keys pointing at missing rows, and text over the limit of an index key column
  (event's project 2048 bytes, kind 512 bytes - the Postgres B-tree cannot take that row). If anything hits, nothing is
  written and each hit is listed by table · column · reason · primary key. Fix those rows in the copy and run it again. Rows
  over the key limit are the exception: do not fix them - they belong to the append-only audit ledger (event), and cutting a
  value makes it differ from the source. Report the coordinates in an issue. `fd migrate verify` does not report a match for
  a source that fails the pre-checks either (except the key limit - the comparison checks the original values already in
  both ledgers).
- New values cannot pass the same limit: the server refuses a project over 2048 bytes or a kind over 512 bytes with a 400
  (`key_too_long`) on both engines.
- **NUL becomes `\x00` in free text only; a NUL in an identifier or a JSON key is refused.** Postgres text cannot hold a
  NUL byte (0x00), and jsonb refuses a document with U+0000 (`\u0000`) in a string. Free text (text columns that are not
  identifier columns, and string values in JSON columns) is not refused; it is moved with the rule below. The rule works on
  one string at a time and can be reversed.
  - Identifier places are not changed; the pre-checks refuse them: a NUL in an identifier column (primary key and UNIQUE
    columns, and columns that name another row whether or not a foreign key backs them - project, `*_id`, `dep_*` and so
    on), and U+0000 in a JSON key (the key's JSON Pointer follows the row key). A changed name could equal another name
    that always held the four characters `\x00`: primary key · UNIQUE would collide, a row pointing at that name would point
    at another row (nothing catches that at commit for a reference column without a foreign key), or two keys of one
    document would become one. Identifiers are short names the server or a person picks, so fix them in the copy.
  - Text columns: in a value that holds a NUL, each NUL becomes the four characters `\x00`, and a run of backslashes directly
    followed by a NUL or by `x00` is doubled - a `\x00` notation already in the same value becomes `\\x00` and stays
    apart from the NUL positions. Every other byte (including other notations such as `\n` or `\x1f`) and every value
    without a NUL is left byte for byte.
  - JSON columns: the document is not decoded and written again. Only the string values that hold U+0000 are
    fixed, in place within their original bytes - `\u0000` becomes `\\x00` (which decodes to the four characters `\x00`),
    and within that string a run of backslashes (`\\` or `\u005c`) directly followed by U+0000 or by `x00` is doubled.
    Strings without U+0000 keep their original bytes even in the same document (their `\x00` notations too), as do numbers,
    whitespace, order and other escapes (`\u00e9`, `\/`).
  - JSON with a lone surrogate (an unpaired `\ud800`-`\udfff`) is refused by the pre-checks (the string's JSON Pointer
    follows the row key). jsonb does not take it, and decoding and writing it again turns it into U+FFFD, merging two
    different keys into one.
  - Reversing: in a changed string (the whole value for a text column), a run of backslashes directly before `x00` that is
    odd (2k+1) was k backslashes + NUL, and one that is even (2k) was k backslashes + `x00`. Everything else is unchanged.
    The record file below says which columns and strings were changed - the values in the moved ledger alone cannot tell a
    changed column from one that always held the four characters `\x00`.
  - **The record of changed columns.** Before it commits, the move writes `<source>.rewritten-<time>.jsonl` next to the
    source: one JSON line per changed column with `table`, `key` (the row's primary key columns → moved values, never cut;
    the secret key columns `web_session.id_hash` and `device_code.code_hash` carry `sha256:` and the value's whole sha256
    (64 characters) instead of the value, and the row is the one where `encode(sha256(convert_to(id_hash, 'UTF8')), 'hex')`
    in Postgres equals what follows - the row coordinates on screen and in errors carry only the first 12 of them),
    `column`, `rule` (the rule in the screen report), `paths` (for a JSON column, where the fixed string values are - a list
    of RFC 6901 JSON Pointers with the original document's keys; absent for text columns) and `original` (the column's bytes as read from the source, base64). There is no limit on the number of lines.
    **That file is the authority on the original bytes of those columns** - do not delete it; keep it with the source copy.
    If the file cannot be written, the move does not commit (nothing is moved). The moved marker
    (`<source>.moved-to-postgres`) names it under `rewritten`, and so does the line below "옮겼다". A dry run and the
    comparison write no record.
  - Nothing is changed silently: the dry run, the move and `fd migrate verify` list the changed columns under "옮기며 바꾼
    원본 칸" by table · column · rule · count · row key (the first 20 per group, shortened - the record file has them all)
    and repeat the number of changed rows under the verdict line. The comparison applies the same rule to the source values,
    so changed rows match. Target values are compared without conversion; broken UTF-8, a NUL in a text column, a storage
    type that does not match the column, or broken JSON in the target is flagged by the target value check.
  - If primary key · UNIQUE columns become equal to another row's once moved (one key sits in a text column as a BLOB and
    another is text with the same bytes), the pre-checks refuse the colliding row by table · column set · row key - before
    the target would refuse it at commit. A partial index (one live hold, and so on) is checked only among the rows inside
    its condition. A NUL cannot cause a collision - those columns are identifier columns, so their NUL is refused above.
- **Text the server writes after the switch follows the same rule.** On both engines, a NUL in free text bound for the
  ledger (titles, bodies, reasons, labels: text people write; for text the server joins, such as a judgment body made of the
  actor and the reason, the whole joined text) becomes `\x00` just before it is stored, byte for byte what moving a SQLite
  ledger holding the same text produces. It does not matter where the write comes from (MCP · CLI · hooks · dashboard
  forms, and `fd import --apply` · `--judgments`, which do not go through REST). A NUL in a name place (ids, kinds, states,
  paths, JSON keys, the URL, the `Idempotency-Key` header), a lone surrogate and broken UTF-8 are refused with 400
  (`nul_refused` · `lone_surrogate` · `bad_utf8`) and nothing is written. From the dry run on, `fd import` lists such
  records as refused and `--judgments` refuses the whole rewrite (it checks each line of the ledger files before decoding
  it). Length limits and responses use the text as sent. Removing a label removes the value that is in the ledger as it
  is first: when an item that an earlier server wrote to a SQLite ledger holds both a NUL label and a label made of the four characters
  `\x00`, only the one named goes. Each rewritten cell that reached the ledger (not a zero-row update, not a savepoint that
  was rolled back) leaves one server log line after the commit with the table, the column and the row coordinates (not
  the text).
- After moving and leaving the moved marker, the import moves the source's pre-upgrade copies (`<source>.bak-*`, with their
  side files) into a new directory `<source>.pre-import-<time>.bak/` (it does not delete them). That file is no longer the
  ledger, and those copies are not copies of whatever ledger is put there later. A dry run, a refusal, or a failure moves
  nothing.
- Refusals the pre-checks cannot see (a CHECK on the target, the index entry size of columns outside those limits, privileges) hit the dry run exactly
  as they hit the real import. The dry run also holds the migration locks as the real import does.

### Moving a Postgres ledger back to SQLite

In a compose deployment, go back with "Going back within 72 hours" above ("Stopped before switching on" if the selection
file was never switched on). This section covers what the command itself does and refuses.

`fd migrate export-sqlite` is the reverse of the import. It moves a Postgres ledger at the import version (20) into a **new** SQLite file
(version 20) - the way back to the SQLite build, carrying the items, claims, events and judgments written on Postgres since
the import. It runs the same transaction and the same comparison as the import (row counts, fingerprints over every column,
sequences), writes into a temporary file next to the target, and gives it the `--to` name only when the comparison matches.
On a mismatch or failure (and always on a dry run) no file is left behind. Exit codes are the same as the import's.

```bash
: '1. Stop the server that writes the Postgres ledger. If any connection other than this run is attached, the export
   refuses and lists pid · role · application_name of each remaining connection. The source is only read, in one
   snapshot (REPEATABLE READ READ ONLY).'
: '2. Dry run: runs the same path to the end in a temporary file (naming is tried with a throwaway hard link in the same
   directory), then deletes that file.'
fd migrate export-sqlite --db <URL> --to fd-back.db --dry-run
: '3. Move. On success it prints the comparison table and "moved N rows, the comparison matched -> fd-back.db".'
fd migrate export-sqlite --db <URL> --to fd-back.db
: '4. Always compare again before starting a server on the new file. --exported compares both ledgers without conversion.'
fd migrate verify --from fd-back.db --db <URL> --exported
```

- Do not skip step 4. The comparison before commit matches the moved snapshot against itself, so a write that a server
  attached to the source committed after the connection check is neither moved nor noticed. verify reads the source again in
  a fresh snapshot and catches that mismatch. If it does not match, delete the exported file and start again from step 1
  with the server stopped.
- Do not drop step 4's `--exported` either. Without it the exported file is measured like an import source and the NUL rule
  is applied to it, so a NUL put into that file after the export would equal the four characters `\x00` in Postgres.
  `--exported` flags that NUL in the value check (a NUL in a text column).

- If a file already sits at `--to`, or a `-wal` · `-shm` · `-journal` sits next to it, the export refuses. It never
  overwrites, and a leftover `-wal` cannot be replayed into the new file.
- The source must be at the import version (20). A Postgres ledger upgraded past it has no place in the SQLite
  build and is refused. So is a ledger whose tables or columns differ from the import version (a hand-edited ledger). The version and the shape are measured in
  the snapshot the export copies from. The account is the same as for the import (a superuser or a member of `fd_owner` - it
  reads the sequences).
- A source holding a value SQLite could not read (broken JSON) is refused. The import-only rules (JSON with a lone surrogate
  or with U+0000 in a key, the index key limit) are not checked: both engines keep these columns as text and SQLite's JSON
  functions read those documents, so the original values already in the Postgres ledger are exported as they are and
  `fd migrate verify --exported` compares those originals - the way back does not stop on one such value. Importing that
  file again stops at the import's pre-checks, which name the row.
- Postgres values are written as they are. The `\x00` the import wrote in place of a NUL is not turned back into a NUL (the
  import's record file is the authority on the original bytes) - the exported file holds no NUL, and its comparison with the
  Postgres ledger (step 4) matches without conversion. A `\u0000` in a JSON string (written after the import by a server from
  before the write path took the NUL rule above - JSON columns are text, not jsonb, so Postgres holds it) is not refused either and is written as it is. The way back does
  not stop on that value. Importing that file again changes a `\u0000` in a string value by the rule above and records it,
  and refuses one in a key in the pre-checks.
- If pre-upgrade copies (`<to>.bak-*`, with their side files) sit next to `--to`, they are moved into a new directory
  `<to>.pre-export-<time>.bak/` before the name is given (not deleted) - they are not copies of the new ledger, so they are
  kept out of the place where `fd migrate --rollback` picks. A dry run moves nothing.
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
`export-sqlite` → check that `fd migrate verify --from <exported file> --db <URL> --exported` matches, and that a SQLite-build server
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
