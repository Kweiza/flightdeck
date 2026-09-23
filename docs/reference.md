**English** | [한국어](reference.ko.md)

# Tool reference

The full list of MCP tools, the `fd` terminal command and the skills, with the habits that go with them.

[← README](../README.md)

---

## Inside a session — 10 MCP tools

There are four to remember: **claim (`pick`) · record (`note`) · close (`finish`) · queue (`land`).**

| Tool | Parameters | When to call |
|---|---|---|
| `board` | `detail` | What work is held right now. Only cards holding a claim |
| `pick` | — | One recommendation + why + **every rejection reason**. Claims nothing |
| `pick` | `item_id` / `item_ids` | Claim + item body + linked judgments + branch/worktree commands |
| `pick` | `leave` | Release your claim. The item returns to `open` and **its id and history survive** |
| `note` | `kind` `body` `item_id` `supersedes` | Record a judgment. 8 kinds: `handoff` `decision` `blocked` `ask` `rejected` `not-done` `verified` `draft` |
| `add` | `id` `title` `body` `paths` `after` `labels` | A queue item. **The id becomes the branch name** |
| `finish` | `item_id` `outcome` `body` `followups` | Judgment + followups + close + release, one call, one transaction |
| `alloc` | `counter_name` | Atomic allocation (logical counters such as a revision number) |
| `land` | `resources` `result` `detail` `leave` | Join the landing queue / check your turn / report and release |
| `label` | `item_id` `add` `rm` | Display-only labels. Only `tickler` is exempt from the starvation axis |
| `amend` | `item_id` `title` `body` `paths` `reason` | Fixes an item's **title, body and paths** in place — those three axes and no others. Only what you pass changes; the old values survive in the revision history |
| `show` | `item_id` | Reads one item — its current body, its revision history and the judgments linked to it, in full. **Closed items too**: this is a read, not a claim. That is the whole point — the only reverse reader of `judgment_link` was `pick`, which only serves `state='open'`, so the 1,985 judgments hanging off closed items were unreachable (5 of them `ask`). Output is capped at 6,000 tokens; past that the oldest judgments come back as titles only **and the response says so** |

Three disciplines hide in that table.

1. **Followups ride in `finish`'s `followups`, not in `add`.** Call `add` beforehand and you can never
   buy back the link to the judgment — it must be in the same call to be attached.
2. **Release a claim with `pick(leave:)`.** Papering over it with `finish(dropped)` changes the id and
   severs the history.
3. **Judgments are never overwritten.** A correction is a new row via `note(supersedes: <judgment id>)`.

## From the terminal — `fd`

```bash
fd status                 # server status banner + board
fd next                   # recommendation only
fd pick <item-id> [<item-id>…]  # claim (with several, the first leads)
fd note --kind decision --body "why it was done this way"
fd finish <item-id> --outcome done --body "① why ② rejected ③ not done ④ only verified"
fd land                   # join the landing queue (--ok|--fail <reason>|--leave <reason> to report/leave)
fd lane wait              # wait for your turn within the turn
fd lane release --row <id> --reason "why it was cut"   # a human reclaims a stuck queue row
fd claim release --item <id> --reason "why it was cut" # a human reclaims a silent session's claim
fd doctor                 # actually measure this machine's platform axes and the server
```

## 4 skills — they exist so you do not memorize the order

| Skill | When | What it does |
|---|---|---|
| `/fd-setup` | First time on a machine | Measure state, decide server vs. client, install and start **only what is missing** |
| `/fd-pickup` | Starting a session | Board → recommendation → claim → **read the linked judgments** |
| `/fd-handoff` | Work is done | Judgment + followups + close item + release, in one call |
| `/fd-update` | The board looks stale | Bring server, plugin and DB up to date |

---

## What the plugin attaches

Enabling it attaches all of the following.

| What | When |
|---|---|
| `SessionStart` hook | Registers the session and injects the board summary, your claims, unacknowledged notes and a **server status banner** |
| `UserPromptSubmit` hook | `prompt` signal + unacknowledged notices |
| `PostToolUse`(Edit\|Write) hook | `tool` signal + **uncommitted footprints** — the only source for the path-overlap axis |
| `PreCompact` hook | Leaves the coordinates as a draft judgment just before compaction |
| `Stop` hook | Asks for prescriptions at end of turn and injects them as `additionalContext` |
| `SessionEnd`(clear) hook | Records, as an observation, that `/clear` ended that conversation |
| 10 MCP tools | `board` `pick` `note` `add` `finish` `alloc` `land` `label` `amend` `show` |
| 4 skills | `fd-pickup` · `fd-handoff` · `fd-setup` · `fd-update` |

**Every hook is fail-open.** `bin/fd` is a shell launcher; the first hook builds `server/` and caches
it under `~/.cache/flightdeck/bin` **per source tree** — so the location does not vary by channel
(hook, MCP, shell) and different source trees get **different files**. **Without Go it prints guidance
and the session continues unaffected.**
