**English** | [한국어](workspaces.ko.md)

# Several repositories — running from a root

[← README](../README.md)

---

When one product is split across several repositories, put them as folders under a root folder and
**start the harness once, from the root**, to coordinate all of them. For that, the root has to declare
what lives under it.

**1. Make the root a repository.** Ignore every member folder — an allowlist is the safe shape.

```gitignore
# .gitignore at the root — ignore everything, then bring back the root's own files
*
!.gitignore
!.flightdeck.yaml
!README.md
```

> **Never run `git clean -ffdx` at the root.** The command that deletes ignored files deletes the
> member repositories wholesale. The wider the ignore list, the more that accident costs.

**2. Write the roster and commit it.** It is only read once committed — the server reads the
**committed file**, not the working tree (so a half-written roster cannot change the exclusion scope).

```yaml
# .flightdeck.yaml at the root
workspace:
  members:
    - project: search-api             # when empty, the last segment of path is the id
      path: platform-search-api
    - path: platform-docs
```

**3. Open a session at the root and check.** The board banner shows `워크스페이스 루트 · 멤버 N건`
("workspace root · N members").

```bash
fd status                    # does the banner show the member count?
fd status --workspace        # one line per member (sessions · claims · queue · resources)
```

No banner means there is no roster (no file · no block). When the roster has a problem, **the banner
carries that line too**:

- **A member line with an unusable value is dropped on its own** — no path · absolute path · the root
  itself (`.`) · parent segments only (`..`) · beyond the root's sibling folders (`../../x`) · the
  root's id · an id that repeats an earlier line (the earlier line wins). The other members stand,
  and the banner adds `명부에서 뺀 줄 N건: path(reason)` ("N lines dropped from the roster").
- **When the file's shape cannot be read** (flow style · tab indentation) the roster is not changed at
  all. The cache stays as last read, and the banner adds `명부 캐시가 기본 브랜치보다 낡았다(…) — reason`
  ("the roster cache is older than the default branch", with both shas).

Both lines are re-read and repeated at every session open until the file is fixed.

A member may live outside the root — **only as far as the root's sibling folders** (`path: ../other-repo`,
or below it such as `../group/other-repo`). A line pointing higher (`../../x`) is dropped: a committed
roster line decides where the server runs git, so that place is kept next to the root. The path is
joined to the root path and the id is still its last segment. But a session started at the root only
leaves footprints inside the root tree, so **no overlap warning fires between root sessions and that member's sessions.** If a member
path is not readable by the server (outside the container's `FD_REPOS` mount — a member outside the root
needs its own mount), that member's branch stays `?(못 읽음)` ("? (unreadable)").

> **After changing the roster, compare the banner's member count with the file.** The two lines above
> (dropped entries, stale cache) report that mismatch, but until the server reads the new roster (it is not
> committed on the default branch yet, or no session has opened since) the banner still shows the old one.

**4. Hand off from umbrella to detail.**

```bash
fd add --id umbrella --title "umbrella" --body "..."                 # at the root
fd finish umbrella --body "..." --followups '[{"id":"detail","title":"detail","body":"...","project":"search-api"}]'
fd next --project search-api          # recommendation for that repository
fd pick detail --project search-api   # the worktree commands point at that repository
```

`fd move <id> --project <member>` moves an existing item too — and references that named the item as a
prerequisite, and the judgments linked to it, are **rewritten to the new project**, and its revision history moves with it (the relationships follow it).

**5. Exclusive resources are one per workspace.**

```bash
fd land --resource env:db --project search-api   # queues if a sibling holds it
fd land --project search-api                       # the landing lane is per repository — siblings do not collide
```

> **Starting codex from the root**: codex decides the project root by "the nearest `.git`", so started
> inside a member repository it does not read the root's `AGENTS.md`. Start it from the root, or pin the
> root with `project_root_markers`. (This sentence rests on the codex documentation, not on a measurement
> taken in this repository — keep it apart from the measured table in design §14.)
