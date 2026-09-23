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
      path: context-platform-search-api
    - path: context-platform-docs
```

**3. Open a session at the root and check.** The board banner shows `워크스페이스 루트 · 멤버 N건`
("workspace root · N members").

```bash
fd status                    # does the banner show the member count?
fd status --workspace        # one line per member (sessions · claims · queue · resources)
```

No banner means the roster was not read, and **the session response says why** (no file · no block ·
parse failure). If a member path is not readable by the server (outside the container's `FD_REPOS`
mount), that member's branch stays `?(못 읽음)` ("? (unreadable)").

**4. Hand off from umbrella to detail.**

```bash
fd add --id umbrella --title "umbrella" --body "..."                 # at the root
fd finish umbrella --body "..." --followups '[{"id":"detail","title":"detail","body":"...","project":"search-api"}]'
fd next --project search-api          # recommendation for that repository
fd pick detail --project search-api   # the worktree commands point at that repository
```

`fd move <id> --project <member>` moves an existing item too — and references that named the item as a
prerequisite are **rewritten to the new project** (the relationship follows it).

**5. Exclusive resources are one per workspace.**

```bash
fd land --resource env:dell --project search-api   # queues if a sibling holds it
fd land --project search-api                       # the landing lane is per repository — siblings do not collide
```

> **Starting codex from the root**: codex decides the project root by "the nearest `.git`", so started
> inside a member repository it does not read the root's `AGENTS.md`. Start it from the root, or pin the
> root with `project_root_markers`. (This sentence rests on the codex documentation, not on a measurement
> taken in this repository — keep it apart from the measured table in design §14.)
