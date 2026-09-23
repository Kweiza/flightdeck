**English** | [한국어](codex.ko.md)

# Using it from codex

[← README](../README.md)

---

codex sessions can land on the same board. Overlap prescriptions seeing both harnesses at once is the whole point.

```bash
fd setup --install-codex
```

It installs three things — the fixed-path wrapper `~/.local/bin/fd-hook`, **`fd` itself
(`~/.local/bin/fd`)**, and `~/.codex/hooks.json`.
**It never overwrites an existing `hooks.json`.** If one is there, it prints what to add and stops, so you merge by hand.

## ★ And you must open the TUI once

```bash
codex        # clear the "Hooks need review" prompt
```

**Without this the hooks never run once.** codex **silently skips** untrusted hooks — if you only
ever use `codex exec`, you never get a chance to see the approval screen, and nothing anywhere
tells you why no card appears on the board. **Not one line about hooks reaches the log.**

Once trusted, the log tells you the opposite — `hook: SessionStart` … `hook: SessionStart Completed`.
The presence of that line *is* the trust signal.

## Why the hook command looks like that

Trust is pinned in `~/.codex/config.toml` like this:

```toml
[hooks.state."/Users/…/.codex/hooks.json:session_start:0:0"]
trusted_hash = "sha256:086dc4d6…"
```

**That hash covers the hook definition (the command string) only** — not the script's contents.
Change one character of the command and trust breaks; restore it and the hook runs again even if
you rewrote the script entirely.

That is why the hook command calls a **fixed path**, `~/.local/bin/fd-hook`. Put a versioned path
there — `${CLAUDE_PLUGIN_ROOT}/bin/fd` — and **every fd upgrade demands re-approval in the TUI**,
with the hooks silently dead until you do it. The wrapper picks the newest installed build from
inside, so **upgrading never changes the command string.**

> The wrapper considers **only official plugin installs.** It deliberately ignores repo checkouts —
> otherwise a stale build runs while pretending to be current and nobody notices.

## How to tell when it is broken

```bash
fd doctor        # the ■ codex section
```

It names four axes: hook file · **hook trust** · hook command (is it a fixed path) · hook wrapper.
With no trust, that row shows `✗` and says what to do. **Silence is not a pass** — codex's own
`codex doctor` says nothing about hooks at all (none of its 19 checks mention them), so this screen
is the only place the state is observable.

## Sandbox networking

codex's default sandbox cuts networking, and fd then **cannot reach the server at all**
(`connect: operation not permitted`).

```bash
codex -c sandbox_workspace_write.network_access=true
```

Or pin it in `~/.codex/config.toml`. This opens your sandbox policy — know what you are opening and why.

## What works from codex today, and what does not

| From codex | Today |
|---|---|
| Hooks — session card · footprints · overlap prescriptions · banner | ✅ works |
| Terminal `fd` | ✅ installed — **you still add it to PATH** (below) |
| Response tail (overlap/unacked) | ✅ on every write command |
| `pick --leave` · `finish --followups` · `land --resource` | ✅ present |
| Prescription syntax | ✅ codex cards get `fd …`, not MCP call syntax |
| The 10 MCP tools | ❌ **deliberately not built** (design ruling) |

### You add it to PATH — that is the one manual step

`fd setup --install-codex` installs `~/.local/bin/fd`, but **that directory is not on this machine's
clean login-shell PATH** (measured 2026-08-31: none of its 20 entries). So the installer prints this
line for you:

```bash
export PATH="$HOME/.local/bin:$PATH"   # put it in ~/.zshrc etc.
```

Before you do, the absolute path already works:

```bash
~/.local/bin/fd board
~/.local/bin/fd note --kind decision --title "…" --body -
```

> ⚠️ The name `fd` collides with `fd-find` (the find replacement). If that one comes first on your
> PATH, that one runs — `fd doctor`'s "codex 창의 fd" axis names **the exact path that wins**. Alias
> ours under another name, or put `~/.local/bin` first.

### The installed `fd` follows upgrades

It is the **same script** as the wrapper — it picks the highest installed version under
`~/.claude/plugins/cache/*/flightdeck/*/bin/fd` and execs it. So a plugin upgrade needs no
reinstall, and hook trust survives (the command string never changes).

### Why MCP is not built

codex gives MCP children only the core 13 variables (HOME, PATH, PWD, …) and **withholds the
session id.** So an MCP tool cannot tell which window it belongs to, and there is no way to recover
it — the parent codex process does not carry the session id in its environment either (measured),
and cwd is identical across two windows on the same repo.

**Attaching an identity-less MCP would make two windows share one card, and the ledger would lie.**

And crucially — **building MCP would not close any gap in the table above.** The missing `fd` is an
install problem; the tail and the handles are CLI-surface problems. MCP fixes neither, and would
introduce a new falsehood (identity-less cards) in exchange. So it is not built — the ruling and its
**four reversal conditions** live in DESIGN's "harness axis" section.
