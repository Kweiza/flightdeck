**English** | [한국어](server.ko.md)

# Running the server

Starting and connecting the server, the environment variables and their traps, what still works when the server is down, and troubleshooting.

[← README](../README.md)

---

## Start the server (once, on one machine)

```bash
git clone https://github.com/Kweiza/flightdeck.git
cd flightdeck
FD_TOKEN="$(cat ~/.flightdeck/token 2>/dev/null)" docker compose up -d
curl -s localhost:7420/healthz
```

`{"ok":true,"api_version":"1","db_ok":true,…}` means you are done.
The screen is at <http://localhost:7420> — one read-only page.

Three things come from the environment:

- **`FD_TOKEN`** — omit it and the server comes up with auth disabled. `/healthz` says so, but it
  **only says so; it does not block.** If you are moving a server that already used a token, you must
  supply the same value. Note that with the container, requests from the host are **not loopback**
  either (they arrive from the bridge gateway), so the token exemption does not apply and the client
  needs the same value via `fd setup --token …`.
- **`FD_UID`/`FD_GID`** — the `~/.flightdeck` volume and the repositories belong to the host user.
  If yours is not the default 1000 (`id -u`), then without these the DB opens but cannot be written,
  and git becomes suspicious about ownership.
- **`FD_REPOS`** — where the repositories live (**default `$HOME`**). **Everything derived comes from
  here** — branch, sha, uncommitted footprints, path overlap. Without it the server looks healthy while
  writing only `브랜치 ?(못 읽음)` ("branch ? (unreadable)") on the board. The path must be **identical**
  on host and container, because a worktree's `.git` points at the main repository by absolute path.
  **Do not lose this value when you upgrade** — compose only reads it at substitution time and it does
  not survive into the container's env, so the mount is its only trace. Leave it out of `up` and it
  silently falls back to the default (measured: 6 of 7 registrations then could not be derived while
  `healthz` stayed ok). Read the old value with `docker inspect` **before** taking the container down.
- **`FD_REPOS2`/`FD_REPOS3`/`FD_REPOS4`** — extra slots (max four) when repositories live in several trees.
  Mount just the roots you need instead of the whole home, and `.ssh`/`.claude` stay invisible. Unused slots
  fold onto the first — compose merges duplicate mounts of the same path. Colon-separated paths do **not**
  work (`FD_REPOS=/a:/b`): one entry is one path. Need a fifth? Add a slot — passing one that does not exist
  binds nothing, silently.

If you enabled it as a plugin, start it from the **installed cache directory**, not from the repo —
that is the copy `/plugin` fetched:

```bash
cd ~/.claude/plugins/cache/<marketplace>/flightdeck/<version>
```

To run without Docker:

```bash
cd server && go run ./cmd/fd serve --addr :7420 --db ~/.flightdeck/fd.db
```

> **This replaces the container — do not run both.** compose mounts `~/.flightdeck:/data`, so the
> `--db` above is **the very database the container holds**. On the same port you stop at bind (and
> the ledger is untouched), but change only the port and that throwaway binary **records itself as a
> deployment** in the ledger — and the container's next restart adds another. To experiment while the
> container runs, move `--db` somewhere else too.

---

## If the server is on another machine, say where

```bash
export FD_URL=http://<server-host>:7420
export FD_TOKEN=<same token as the server>   # only if you gave the server FD_TOKEN
```

Without a token, auth is off. **`/healthz` announces that** — it is never left open quietly.

---

## When the server dies (L1)

`SessionStart` **explicitly injects** a banner. Left silent, an agent proceeds believing coordination
exists.

```
⚠ 조정 서버 미도달(http://localhost:7420, 마지막 접속 14:02 · 37분 전).
  되는 것: 코드 작성·커밋·조사 전부. 이미 선점한 항목의 작업.
  안 되는 것: 새 항목 선점 · 다른 세션의 현재 상태.
  아래는 14:02 시점의 스냅숏이다. 그 뒤 남이 무엇을 집었는지는 알 수 없다.
```

*"Coordination server unreachable (last contact 14:02, 37 minutes ago). Works: writing code,
committing, investigating — all of it; and work on items you already claimed. Does not work: claiming
new items; the current state of other sessions. What follows is a snapshot as of 14:02. What others
picked up since then is unknowable."*

- **Reads** — the last successful response is served with a staleness banner. It never goes silent.
- **Judgments and notes** — queued in an outbox and **replayed idempotently** on reconnect. Exit 0.
- **Claims** — refused. Only the server can guarantee exclusion; allowing offline acquisition would make that exclusion a lie.
- **Allocation** — refused. Issued offline, two sessions would use the same number.
- **The four landing-lane operations** (`land` acquire, report, leave, `lane release`) — all refused,
  and **for three different reasons.** Acquisition: the document of record for exclusion is a server-side
  DB constraint, so manufacturing "my turn" here would let two sessions land simultaneously.
  Report and leave: by replay time someone else may hold the lane, so replaying would **release
  someone else's hold.** Reclaim: it is a human decision made by looking at what is stuck right now,
  and the lane at replay time is not the lane that decision saw.
- **Claim reclaim** (`fd claim release`) — refused, for those reasons plus one more: by replay time
  that session may have come back and be working (this is the axis where two measured liveness
  misjudgments rejected automatic reclaiming).

<details>
<summary><b>Why state is split across three locations</b> (deep dive)</summary>

**State is not one place but three.** Two axes divide them — ① can it be regenerated ② if copies
diverge, is each one still correct. Only things answering "yes" to both may live in a
channel-dependent location.

| What | Where | Why |
|---|---|---|
| **Response cache** | `${CLAUDE_PLUGIN_DATA}` | Regenerable, and if copies diverge **each is still correct** — values carry a timestamp and say "stale: 37 minutes ago" themselves. `${CLAUDE_PLUGIN_ROOT}` changes path on every update, so nothing lives there |
| **Binary cache** | `~/.cache/flightdeck/bin/fd-<source tree>` | Regenerable, but once exec'd it **will not tell you which build it is** — if two copies differ, one is old code pretending to be current. So it lives in a channel-independent fixed location with the source encoded in the name |
| **Judgments and identity** (outbox, `machine-id`, `config.json`, beacons) | `~/.flightdeck` | Either not regenerable, or required to be identical on the same machine. Put it in a divergent location and judgments queued from the shell can **never** be sent by the hook or MCP |

Set `FD_STATE_DIR` and all three move under it — an axis chosen by **a human**, not by the channel,
so it does not diverge per process.

**Old binaries in old locations are neither moved nor deleted.** Copying carries the mtime along and
suppresses a rebuild that is needed, and a rebuild takes under a second anyway. Instead
**`fd doctor` says something**:

```
! 옛 바이너리 자리 /home/you/.local/state/flightdeck/bin — fd 1개 · 22.1MB(아무도 안 쓴다. 지우려면 사람이 지운다)
```

*"! Old binary location … — 1 fd · 22.1 MB (nobody uses it; a human deletes it if it should go)."*

**doctor only speaks. A human deletes:**

```sh
rm -f ~/.local/state/flightdeck/bin/fd ~/.claude/plugins/data/*/flightdeck/bin/fd
```

That diagnostic line sees **only the locations this channel can compute** — a user shell usually has
no `${CLAUDE_PLUGIN_DATA}`, so only **one of the two lines** appears (doctor states that fact on the
next line itself). The `rm` above is therefore broader than what doctor reported. Do not put
`${CLAUDE_PLUGIN_DATA}` in a command literally — unset, it expands to `/flightdeck/bin`. It is safe
even if a process still holds that path (unlink does not touch the inode). Conversely, **if you revert
this axis** the GC goes with it, so run `rm -rf ~/.cache/flightdeck/bin` once so the copies left in
the new location find an owner.

</details>

---

## When something breaks

```bash
fd doctor
```

It reports platform axes **one at a time, by name**. A `✗` on `CLAUDE_CODE_SESSION_ID` means the
source of session identity is severed, and at that point this tool refuses rather than inventing a
session. Fold an absence into a default and that fact becomes invisible forever.

| Symptom | Where to look |
|---|---|
| Board section ① is empty | **First: if nobody is holding an item, that is normal.** ① lists only cards holding a claim — the screen itself says "this is not a server failure" along with how many it folded. If it still looks wrong: `FD_URL` in `fd doctor`, the server log's startup line, whether other sessions point at the same server |
| Hooks do nothing | Run `bin/fd` directly. Without Go it prints guidance |
| The port will not open | The server log's "failed to start" line carries the remedy with it |
| Tools do not appear | Is `type` in `.mcp.json` set to `stdio`? Without it the whole server is skipped |
| Board shows `브랜치 ?(못 읽음)` | The repositories are not mounted into the server container (`FD_REPOS`). `파생 git@` in `fd status` is the diagnostic |
