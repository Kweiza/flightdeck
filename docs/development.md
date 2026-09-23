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
| `server/` | One Go module — server, CLI, MCP and hooks are **one binary** |
| `compose.yaml` | Server container (one-shot schema apply + server) |
| `DESIGN.md` | Document of record for the design |
| `docs/*.md` · `docs/*.ko.md` | Topic docs (concepts · server · codex · several repositories · reference · field notes · development) |
| `docs/superpowers/` | Per-feature designs (`specs/`) and implementation plans (`plans/`) |

## Verification gates

```bash
cd server
gofmt -l .                        # must print nothing — check first that cwd is inside the module
go vet ./...
GOOS=linux go vet ./... && GOOS=windows go vet ./...
go test ./...                     # takes a few minutes
```

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
