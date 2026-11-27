**English** | [한국어](development.ko.md)

# Development

Repository layout, verification gates, and version bumps.

[← README](../README.md)

---

The repository root is the plugin root (what `${CLAUDE_PLUGIN_ROOT}` points at).

| Path | What |
|---|---|
| `.claude-plugin/` | `plugin.json` (the version) · `marketplace.json` (makes this repository a marketplace) |
| `bin/fd` | Shell launcher. Builds `server/` on first use and caches it under `~/.cache/flightdeck/bin` |
| `hooks/hooks.json` · `.mcp.json` | Claude Code hook wiring and the MCP server (`fd mcp`) |
| `skills/` | The 4 skills |
| `board/` | Owner kanban board (`board.py` · `index.html` · `install.sh`). Its tests `test_board.py` run under `go test ./cmd/fd` |
| `server/` | One Go module — server, CLI, MCP and hooks are **one binary** |
| `compose.yaml` | Server container (one-shot schema apply + server). The ledger is a SQLite file |
| `compose.postgres.yaml` · `deploy/` | The Postgres ledger override (ledger database, login accounts, daily dump) and its scripts and access rules (`db-roles.sh`, `db-backup.sh`, `pg_hba.conf`). `deploypg_test.go` checks it against the rendered compose config, `deployhba_test.go` against a real Postgres started from that config |
| `compose.test.yaml` | Test-only Postgres (`pg-test`). The tests take it from `FD_TEST_PG` |
| `DESIGN.md` | Document of record for the design |
| `docs/*.md` · `docs/*.ko.md` | Topic docs (concepts · server · codex · several repositories · reference · development) |

## Verification gates

```bash
cd server
gofmt -l .                        # must print nothing — check first that cwd is inside the module
go vet ./...
GOOS=linux go vet ./... && GOOS=windows go vet ./...
# tests: start the PG test container, pass it as FD_TEST_PG, take it down afterwards (takes a few minutes)
(p=fdtest-$(basename "$(dirname "$PWD")"); trap 'docker compose -f ../compose.test.yaml -p "$p" down' EXIT; trap 'exit 143' HUP INT TERM; docker compose -f ../compose.test.yaml -p "$p" up -d --wait && FD_TEST_PG="postgres://postgres@$(docker compose -f ../compose.test.yaml -p "$p" port pg-test 5432)/postgres?sslmode=disable" go test ./...)
```

- **The tests run on Postgres.** Start `pg-test` from `compose.test.yaml` (Docker required) and pass its address
  as `FD_TEST_PG`. The test helper (`server/internal/store/storetest`) gives each test a new database cloned
  from a template database and drops it when the test ends.
- **Without `FD_TEST_PG` the tests fail — they do not skip.** A skip would leave a package that ran nothing green.
- **The test container shares nothing with production.** Every compose command names `-f ../compose.test.yaml`
  (leave it out and compose picks the production `compose.yaml`, so the take-down would stop the production server).
  The project name differs per checkout (`fdtest-<directory>`) and the host port is picked by Docker on loopback,
  so two checkouts running the gates at once never take each other down.
- **The take-down rides the subshell's EXIT trap.** It runs when a test fails, when the start fails, and when the
  shell gets HUP · INT · TERM. The exit code is that of `go test`.

- **Cross-check with `go vet`, not `go build`.** `go build` skips `_test.go`, so test code broken on
  another OS stays silent.
- **Silence is not a pass.** With cwd outside the module, `gofmt -l` inspects an empty directory and
  exits without a word.

## Version bumps

A push that changes code bumps `version` in `.claude-plugin/plugin.json` **in the same push**. The
marketplace is a git repository, so an unchanged version means `/plugin update` **never fetches the
new code at all.** New verbs, tools or schema are minor; display, wording and bug fixes are patch.
To update a running server afterwards, the `/fd-update` skill knows the order (push → marketplace →
plugin → container).

## Documentation

- The design of record is [`DESIGN.md`](../DESIGN.md) (Korean). Nothing exists here that is not in it.
- **Korean is the source of truth.** Edit `README.ko.md` and `docs/*.ko.md` first, then bring the English
  (`README.md`, `docs/*.md`) in line. Commits, judgments and code comments here are all Korean, so that
  direction matches reality. If the two disagree, the Korean one is right.
- GitHub does not pick a README by language — it renders only `README.md` — so the language link on
  the first line of every document does that job.
- The "N MCP tools" and "N skills" in the READMEs (Korean: 「MCP 도구 N개」, 「스킬 N개」) are checked
  against the real counts by a test (`server/cmd/fd/plugin_test.go`). Add a tool or skill and those
  numbers are the first thing to go wrong.

## What stays out of this repository

This repository is the product, not the record of building it. Keep these out of commits, docs, code
comments, test values and commit messages:

- Machine and person details: home-directory paths, account and host names, tailnet names, IP
  addresses, session ids, names or e-mail addresses of people.
- Other repositories' and projects' names, and numbers measured on them. A design note may keep a
  measured number, but not where it was measured.
- Development material: per-feature plans and specs, handoffs, measurement logs, review notes. Keep
  them next to your checkout, outside git.

Test values use neutral placeholders (`/home/u/proj`, `example.com`, `proj-a`).
