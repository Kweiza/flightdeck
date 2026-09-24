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

> **flightdeck speaks Korean.** The board, prescriptions, refusals and `fd doctor` are all in Korean.
> The docs quote that output verbatim and explain it in English, so what you read is what you will see.
> In the board above: no items are claimed, 4 queue items are open, and the landing lane is empty.

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
| `FD_URL` | session | Server address (default `http://127.0.0.1:7420`) |
| `FD_STATE_DIR` | session | Put the state files (outbox, caches, machine id) in one place |

The traps behind each one are in [Running the server](docs/server.md).

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
prescriptions went out and sessions queued for landing 157 times across the two repositories. The
numbers and their limits are in [Field notes](docs/field-notes.md).

## Development

The repository root is the plugin root. The server, CLI, MCP server and hooks all come from one Go
module in `server/`.

```bash
cd server
gofmt -l .                                            # must print nothing
go vet ./... && GOOS=linux go vet ./... && GOOS=windows go vet ./...
go test ./...                                         # takes a few minutes
```

A push that changes code bumps `version` in `.claude-plugin/plugin.json` too — otherwise
`/plugin update` never fetches the new code. Layout and the reasons behind each gate are in
[Development](docs/development.md).

## Origin

flightdeck grew inside the plugin collection [kweiza-cc-plugins](https://github.com/Kweiza/kweiza-cc-plugins)
as `plugins/flightdeck`, and was split out here on 2026-09-24 at 0.38.3. Earlier history, and the commit
shas that the docs and code comments cite, live in that repository.

## License

MIT — see [LICENSE](LICENSE).
