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
(written by `fd setup`) or environment variables.

| Variable | Side | Meaning |
|---|---|---|
| `FD_TOKEN` | server · session | Auth token. Without it the server runs unauthenticated, and `/healthz` says so |
| `FD_REPOS` | server | Where your repositories live (default `$HOME`). Everything derived comes from here — **keep it across upgrades** |
| `FD_REPOS2`–`FD_REPOS4` | server | Extra mounts when repositories live in several trees |
| `FD_UID` · `FD_GID` | server | Host user and group id (default 1000) |
| `FD_LEDGER_HOST` | server | Where the ledger backup goes (default `~/.flightdeck-ledger`) |
| `FD_BACKUP_DIR` | server | Where `fd migrate` writes the dump it takes before upgrading a Postgres ledger (`--backup-dir` wins; when both are empty, `/backups` if it exists). **Give each deployment its own directory**: a test system cloned by physical replication shares the ledger identity (cluster identifier and database name) with its source, so in a shared directory a rollback could pick the other deployment's dump |
| `FD_DUMP_TIMEOUT` | server | Deadline for one dump of a Postgres ledger (Go duration, default `30m`, at least 1s). The dump `fd migrate` takes before upgrading and the daily dump of `db-backup` use the same value: for both it is "this ledger's normal dump time plus headroom". Business writes wait while the pre-upgrade dump runs, so if the dump stalls (say the backup volume stops responding) this is the longest they stay blocked. Past the deadline pg_dump is stopped, the server drops the migrator's connection and its locks, and the version is not raised. The daily dump stops pg_dump past the deadline and counts the day as failed (a stalled dump holding table locks makes the DDL of `fd migrate` fail). Raise it if a large ledger's normal dump takes longer |
| `FD_BACKUP_HOST` | server | Where the daily dumps of a Postgres ledger go (default `~/.flightdeck-backups`). The `db-backup` service writes one a day into its `daily/<db>/` and keeps the newest 14. **Create it beforehand** (owner `FD_UID`, mode `0700`) and **give each deployment its own**: two deployments with the same database name sharing one directory would judge freshness by each other's dumps and rotate each other's dumps away. Setup and restore are in "Daily dumps of a Postgres ledger" below |
| `FD_PGPASS_HOST` | server | The pgpass file `db-backup` reads (default `~/.flightdeck-pgpass`, owner `FD_UID`, mode `0600`, line `db:5432:<db>:<role>:<password>`). It is the only place the password lives; keep it out of environment variables and command lines. libpq ignores the file if its mode is wider. **Keep it outside the server's mounts**: the default sits inside the default `FD_REPOS` (the whole home), so the server can read it (see "Daily dumps of a Postgres ledger" below) |
| `FD_PG_DATABASE` · `FD_PG_BACKUP_USER` | server | Database and role `db-backup` connects as (default `flightdeck` and `fd_backup`). The role needs `pg_read_all_data`. Each database gets its own dump directory (`daily/<db>/`; bytes other than lowercase letters, digits, `_` and `-` become `%XX`), so after switching databases the new one neither counts nor deletes the old one's dumps |
| `FD_URL` | session | Server address (default `http://127.0.0.1:7420`) |
| `FD_STATE_DIR` | session | Put the state files (outbox, caches, machine id) in one place |
| `FD_LANG` | server · session | Output language. `en` for English (default Korean). The server side covers the dashboard; the session side covers the CLI, hooks and MCP replies |

The traps behind each one are in [Running the server](docs/server.md).

`FD_LANG=en` translates **at the output boundary**: the board, the dashboard, add · next · pick · note ·
finish · show, land, the tail and Stop prescriptions are covered first. A line the table
(`server/internal/lang/catalog.go`) does not cover stays in Korean as a whole — no half-translated
lines. `fd doctor`, the login page and rarer diagnostics are still Korean.

### Daily dumps of a Postgres ledger

The compose `db-backup` service (profile `postgres`) writes a `pg_dump -Fc` once a day to
`$FD_BACKUP_HOST/daily/<db>/fd-daily-<UTC>.dump` and keeps the newest 14. Nothing runs before the move to Postgres.
It rotates only after a dump succeeds, and only one run works in a database's directory at a time. The container turns
unhealthy when the newest dump is older than the interval + two checks + the deadline (26h30m by default).

**Setup**: once, before starting the service. Run it as the `FD_UID` user.

```bash
# 1. The dump directory. If it is missing, Docker on Linux creates it root-owned 0755 and the first dump cannot write.
install -d -m 0700 "${FD_BACKUP_HOST:-$HOME/.flightdeck-backups}"
#    For a directory of another uid: sudo install -d -m 0700 -o "$FD_UID" -g "$FD_GID" <dir>

# 2. The pgpass. Owner FD_UID, mode 0600, outside the server's mounts (see the star note below).
sudo install -d -m 0700 -o "${FD_UID:-1000}" -g "${FD_GID:-1000}" /etc/flightdeck
install -m 0600 /dev/null /etc/flightdeck/pgpass
"${EDITOR:-vi}" /etc/flightdeck/pgpass        # one line: db:5432:flightdeck:fd_backup:<password>
export FD_PGPASS_HOST=/etc/flightdeck/pgpass  # in the shell that runs compose, or in .env
```

The backup role only reads (once, as a superuser):

```sql
CREATE ROLE fd_backup LOGIN PASSWORD '<password>';
GRANT pg_read_all_data TO fd_backup;
```

`docker compose --profile postgres run --rm db-backup config` prints the settings on one line. `once` takes one dump by
hand (0 = dumped or not due yet, 1 = failed, 3 = another run is working in the same directory, so nothing was done).

★ **Keep the pgpass outside the server's mounts.** The server (the `flightdeck` service) runs as the same uid and mounts
`FD_REPOS` (the whole home by default) read-only. The default `~/.flightdeck-pgpass` is inside it, so even at `0600`
the server can read the backup password. Put it outside the home as above, or narrow `FD_REPOS` to where the repositories live.

**Restore**: as a superuser (it creates roles and a database). The commands below use `psql` and `pg_restore` from the
`db-backup` service image (the dumps are visible under `/backups`); when asked for a password, give that account's.
If the database is on another host, add `-h <host>`.

```bash
docker compose stop flightdeck db-backup      # drop the connections to the database being restored
sql() { docker compose --profile postgres run --rm --entrypoint psql db-backup -U postgres -d postgres -v ON_ERROR_STOP=1 "$@"; }
```

`sql` with no arguments opens an interactive superuser psql.

1. Read the database name from the dump. `--create` creates the database under **the name in the dump**, not the current `FD_PG_DATABASE`.

   ```bash
   docker compose --profile postgres run --rm --entrypoint pg_restore db-backup -l /backups/daily/<db dir>/fd-daily-<UTC>.dump | grep '^; *dbname:'
   ```

   If a database of that name already exists, do not drop it, **rename it and keep it** (`<db>` is the name read above):
   `sql -c 'ALTER DATABASE "<db>" RENAME TO "<db>_before_restore"'`. Drop the old one only after the check in step 4.
2. Create the roles and grant the memberships (in `sql`). Roles are cluster-wide, so they are not in the dump. Skip roles
   that already exist (`CREATE` fails on them). Without the memberships the restore succeeds but the server cannot read
   the tables (`permission denied`).

   ```sql
   CREATE ROLE fd_owner NOLOGIN;
   CREATE ROLE fd_app NOLOGIN;
   CREATE ROLE <app account> LOGIN PASSWORD '<password>';  GRANT fd_app TO <app account>;
   CREATE ROLE fd_backup LOGIN PASSWORD '<password>';  GRANT pg_read_all_data TO fd_backup;
   -- If the migrator uses its own account, GRANT fd_owner TO <that account>. The original database's owner role must exist too.
   ```

3. Restore the whole database. `--create` also restores the database-level privilege (`TEMPORARY` revoked from PUBLIC);
   restoring into a database you created beforehand drops it.

   ```bash
   docker compose --profile postgres run --rm --entrypoint pg_restore db-backup \
     -U postgres -d postgres --create --exit-on-error /backups/daily/<db dir>/fd-daily-<UTC>.dump
   ```

4. Connect **as the real app account** to check. The tables must be readable (memberships) and the `TEMPORARY` privilege
   must be gone. If either is off, psql stops with an error and a nonzero exit code (`ON_ERROR_STOP`; a remaining privilege
   deliberately triggers a division by zero).

   ```bash
   docker compose --profile postgres run --rm --entrypoint psql db-backup -U <app account> -d <db> -v ON_ERROR_STOP=1 \
     -c 'SELECT count(*) FROM judgment' \
     -c "SELECT 1 / (NOT has_database_privilege(current_database(), 'TEMPORARY'))::int"
   ```

5. Start again with `docker compose --profile postgres up -d`. Drop the renamed old database once the check is done.

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
