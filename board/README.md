**English** | [한국어](README.ko.md)

# Owner kanban board

A work desktop for the person (the owner) who watches the flightdeck queue as a kanban. fd is the only source of
truth and the board holds no state: whatever the owner does on the board is left only as fd labels and judgment notes.
Sessions (Claude Code · Codex) receive this convention as one line of the MCP instructions (`Instructions` in
`server/internal/mcpsrv/protocol.go`) and through the `fd-pickup` · `fd-handoff` skills.
**The Korean file [README.ko.md](README.ko.md) is canonical** — point to it, do not copy it elsewhere.
The line prefixes stay Korean (`미리보기:` …) because the board parses them literally.

[← README](../README.md)

---

## Card body convention

Workers and coordinators write these lines in an fd item body. A line anywhere in the body is recognised by its
prefix, and one kind may appear several times. They are read in one place, `directives_of()` in `board.py`.

    미리보기: https://...             web preview URL -> open button on the card, iframe in the detail (new-tab button if blocked)
    그림: /absolute/path.png          local image -> thumbnail on the card, click to enlarge
    영상: /absolute/path.mp4|webm|mov local video -> <video controls> in the detail, marker on the card
    파일: /absolute/path.md|txt|json|log  local text file -> shown in the detail (md rendered, long logs last 200 lines)
    링크: name | https://...          outside link (console, PR, store) -> button
    커밋: <sha>                       GitHub link (built from that project's repository origin)
    소유자: what · where · button     the owner's step, one line -> highlighted at the top of the card
    선택: option A | option B | ...   options of a question to the owner -> buttons in the detail

**When the owner has to choose**, do not leave just a `선택:` line. Write all four parts:
1. Question: what has to be decided, in one or two sentences, and why now.
2. Background: the current state · measured numbers · what the comparison points do · what is blocked until it is decided.
3. One paragraph per option: what changes, upsides, downsides and risks, revenue impact, whether it can be undone, the work it takes.
4. Recommendation: which option and why; if the two judges (Claude · Codex) disagreed, both.
Then the `선택:` line (option names equal to those in 3., the recommended one ending in 「(추천)」).

The board recognises these headings and shows a "decision" block at the top of the detail (question panel, folded
background, a card and a choose button per option, recommended border and ribbon, judges side by side), and on the
list card the first line of the question and a "추천: name" chip.

    ### 질문
    One or two sentences. Why now.
    ### 배경
    Current state · numbers · comparison · what is blocked (lists allowed)
    ### 보기: Option A
    What changes / upsides / downsides · risk / revenue / reversible / work
    ### 보기: Option B (추천)
    ...
    ### 추천
    Which option and why.
    Claude: (the option that judge picked, and why)
    Codex: (same form)
    선택: Option A | Option B (추천)

- Headings are literally `###`. A card is a decision card only with `### 질문` and at least one `### 보기:`
  (otherwise the old form: a row of `선택:` buttons). Each section runs until the next `##` · `###` heading;
  convention lines inside it are taken out of the section text (they show in their own place). Section text is Markdown.
- Names after `### 보기:` and in the `선택:` line are matched with a trailing 「(추천)」 removed. An option with no match has no button.
  The recommendation mark may sit on either side.
- Judge lines are one per person inside `### 추천` (`Claude:` · `Codex:` · `Fable:` · `Opus:` · `astra:`, optional `- `).
  If they name different options first, or the section says 「의견 갈림」 · 「갈렸」, it is marked as a split.
- Choosing: the option's button → an in-card confirm row → `/api/choose`. Afterwards (the latest `소유자 선택:` note,
  unless the coordinator is asking again with `status:owner`) the option shows 「고름」 and the buttons lock.
  Read in one place: `decision_of()` · `chosen_of()` in `board.py`.

- Local paths are served only under the allowed roots (`BOARD_FILE_ROOTS`, default `~/Developments` · `/private/tmp/claude-<uid>`),
  judged by realpath; a symlink that leaves them is 403. Only known text · image · video extensions (`TEXT_TYPES` · `MEDIA_TYPES`).
  Hidden path segments (except `.claude` · `.playwright-mcp`, so `.certs` · `.env` · `.git` · `.ssh` are refused) and
  secret-looking names (key · secret · token · credential · password · passwd · private · auth anywhere in a segment,
  false positives accepted; `*.pem` · `*.p12` · `*.jks`) are 403. A Host that is not one of the board's names
  (127.0.0.1 · localhost · `BOARD_HOSTS`) is 421.
- Preview iframes: the board runs cross-origin isolated (COOP same-origin, COEP credentialless) so threaded Godot works,
  so a preview server must send `Cross-Origin-Embedder-Policy` and `Cross-Origin-Resource-Policy: cross-origin`.
  The board checks the headers first with `GET /api/embed?url=`; if it cannot embed, it shows why and a new-tab button.

## What it is

One `board.py` (Python 3 standard library only) serves a static page (`index.html`), proxies the fd REST API
(only this process knows the token), and serves local files at `GET /file?path=` (Range 206 for video, `tail=N` for the last N lines).
The board is **a host process, not inside the container** — the images and logs workers attach live on the host disk.

- Address: `http://127.0.0.1:7430/` (`?project=<id>`, `?item=<id>` opens that card). To reach it from other devices expose it,
  e.g. `tailscale serve --bg --https=37430 http://127.0.0.1:7430` — the board never touches that configuration.
- Initial project: `?project=` → the last one this browser saw → `BOARD_PROJECT` → the first in the list.

## Install (macOS launchd)

    board/install.sh             # write the plist, reload, print one BOARD_READY line once /healthz is 200
    board/install.sh --print     # only print the plist
    board/install.sh --uninstall

- Label `com.<user>.flightdeck-board`, KeepAlive, log `~/Library/Logs/flightdeck-board.log`. The plist points at the
  `board.py` of the checkout that ran `install.sh` — after moving, run it again from the new place.
- Re-running inherits the current plist's values for variables you do not pass.
- After a code-only change: `launchctl kickstart -k gui/$(id -u)/com.$(id -un).flightdeck-board`.
- Outside macOS, run `python3 -I board/board.py` as that OS's service with the variables below.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `BOARD_PORT` · `BOARD_ADDR` | `7430` · `127.0.0.1` | where it listens |
| `BOARD_PROJECT` | (none) | first project shown; otherwise the first in the list |
| `BOARD_HOSTS` | (none) | `name:port` names allowed beyond loopback (comma-separated). `install.sh` fills it from the tailscale name and `BOARD_TS_PORT` (37430) |
| `BOARD_FILE_ROOTS` | `~/Developments:/private/tmp/claude-<uid>` | `/file` roots (colon-separated) |
| `BOARD_LINKS` | `~/.config/flightdeck-board/links.json` | top link row, a per-machine file outside the repository; shape in `links.example.json` (`project` limits an entry to that project). Missing means no links |
| `FD_CONFIG` | `~/.flightdeck/config.json` | where the fd URL and token are read (the fd client config, re-read per request) |
| `FD_URL` · `FD_TOKEN` | (config file) | take precedence over the file |
| `BOARD_TAILNET` | (install.sh: suffix of the tailscale name) | tailnet suffix `/api/embed` may call (e.g. `<tailnet>.ts.net`) |
| `BOARD_PREVIEW_PORTS` | (none) | localhost ports `/api/embed` may call (comma-separated) |
| `BOARD_AUTH` | `~/.config/flightdeck-board/auth.json` | access secret · owner logins (see Access) |

## Access

The board holds the fd token and writes to fd on the owner's behalf, so it sorts callers in two (one place, `access_denied` in `board.py`).

- **Requests over the tailnet** (Host is a tailnet name in `BOARD_HOSTS`): the `Tailscale-User-Login` header that `tailscale serve`
  adds must be in `owner_logins` of the access file, otherwise 403. That header cannot be forged from outside only while the board
  is bound to loopback behind serve, so the board refuses to start when `BOARD_ADDR` is not loopback (`check_bind`).
- **`/api/*` · `/file` for every request**: the cookie `/` hands out (`fdboard`, HttpOnly · SameSite=Strict, a random secret made at install)
  is required. It stops another site's page from driving the board through the owner's browser.
- Access file `BOARD_AUTH` (default `~/.config/flightdeck-board/auth.json`, 0600): `{"secret": …, "owner_logins": […]}`. `install.sh`
  creates it when missing (login from `BOARD_OWNER_LOGIN` or this machine's tailscale user) and never prints the values. Do not put them
  in the repository, reports or cards. Without the file the board does not start.
- **Remaining risk**: a process of the same user on the same Mac can fetch the cookie from `/` and imitate Host and headers. The board does
  not defend that boundary (such a process can already read the token in `~/.flightdeck/config.json`).
- The preview check `/api/embed` accepts same-origin requests only, HEADs only names under this machine's tailnet suffix (`BOARD_TAILNET`)
  and `localhost` · `127.0.0.1` on `BOARD_PREVIEW_PORTS`, and does not follow redirects.

Coordinators and scripts calling the board API fetch the cookie first:

    curl -s -c /tmp/fdboard.jar -o /dev/null http://127.0.0.1:7430/
    curl -s -b /tmp/fdboard.jar 'http://127.0.0.1:7430/api/inbox?project=<project id>'

## Tests

    python3 -I -B -m unittest discover -s board -p 'test_*.py'

`TestBoardPythonSuite` in `go test ./cmd/fd` runs the same. `TestBoardReadsTheseFields` in `go test ./internal/api`
locks the server response fields the board reads — rename one on the server and it turns red there.

## Columns and how to move a card

Decided in one place, `column_of` in `board.py`.

| Column | Rule |
| --- | --- |
| Done | closed items (done · dropped) |
| Running | label `status:running`, or claimed by any session |
| Owner's turn | label `status:owner` (owner to do), `status:owner-done` · `status:owner-ask` (back to the coordinator, shown on top) |
| Waiting | every other open item |

Starting a worker: `fd add --id <unit> --title ... --body - --label status:running` (or `fd label <unit> --add status:running`);
a worker that picks the item shows as running anyway. Handing to the owner: `fd label <unit> --rm status:running --add status:owner`
plus a `소유자:` line (and `선택:` if needed). After landing: `fd pick <unit>` then `fd finish <unit> --outcome done ...` with a `커밋: <sha>` line.
Over MCP the same is `add(labels)` · `label(add, rm)` · `pick` · `finish`.

## What owner actions leave in fd

Writes are POST only, same-origin (Sec-Fetch-Site same-origin, or an Origin that is a board host) and JSON only.
Every note is kind `decision`, titled with the `[fdboard]` prefix, linked to the item (no session).

| Action | Labels | Judgment note (title / body) |
| --- | --- | --- |
| Comment | unchanged | `[fdboard] 소유자 댓글` / the text |
| 「했어」 (status:owner cards only) | `-status:owner +status:owner-done` | `[fdboard] 소유자: 했어` / `소유자: 했어` |
| Choose button | `-status:owner +status:owner-done` | `[fdboard] 소유자 선택: <option>` / `소유자 선택: <option>` |
| Move (drag · detail menu) | running `+status:running`, owner `+status:owner`, waiting = all four removed | none |
| Move to done | refused (closing is the coordinator's, with the commit); a popup explains | none |
| New card | id `owner-<YYYYMMDD-HHMMSS>-<4 chars>`, `+status:owner-ask` | none |

A claimed card stays in Running whatever its labels. Closed items do not change labels (comments still work).

## Where the coordinator reads: `/api/inbox`

    curl -s -c /tmp/fdboard.jar -o /dev/null http://127.0.0.1:7430/
    curl -s -b /tmp/fdboard.jar 'http://127.0.0.1:7430/api/inbox?project=<project id>'

- `items`: open items labelled `status:owner-done` · `status:owner-ask` (`kind`, body, that item's `[fdboard]` notes in time order).
- `recent_owner_notes`: the project's latest 30 `[fdboard]` notes (newest first, `item` = item id).

After handling, tidy the labels: close with `finish` when confirmed; `--rm status:owner-done --add status:running` when starting work;
`--rm status:owner-done --add status:owner` to ask the owner again.
