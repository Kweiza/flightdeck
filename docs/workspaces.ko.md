[English](workspaces.md) | **한국어**

# 여러 저장소 — 루트에서 띄우기

[← README](../README.ko.md)

---

한 제품이 저장소 여럿으로 갈리면, 루트 폴더에 그것들을 폴더로 두고 **루트에서 하네스를
한 번만 띄워** 전부를 관장한다. 그러려면 루트가 자기 하위에 무엇이 있는지 선언해야 한다.

**① 루트 레포를 만든다.** 하위 저장소 폴더는 전부 무시한다 — 화이트리스트가 안전하다.

```gitignore
# 루트의 .gitignore — 전부 무시하고 루트 자신의 것만 되돌린다
*
!.gitignore
!.flightdeck.yaml
!README.md
```

> **`git clean -ffdx` 를 루트에서 돌리지 마라.** 무시된 것을 지우는 그 명령이 하위 저장소를
> 통째로 지운다. 무시 목록이 넓을수록 그 사고의 값이 크다.

**② 명부를 적고 커밋한다.** 커밋해야 읽힌다 — 서버는 작업 트리가 아니라 **커밋된 파일**을
읽는다(반쯤 쓴 명부가 배타 스코프를 바꾸는 것을 막는다).

```yaml
# 루트의 .flightdeck.yaml
workspace:
  members:
    - project: search-api             # 비면 path 의 마지막 마디가 id 다
      path: context-platform-search-api
    - path: context-platform-docs
```

**③ 루트에서 세션을 열고 확인한다.** 보드 배너가 「워크스페이스 루트 · 멤버 N건」을 낸다.

```bash
fd status                    # 배너에 멤버 수가 뜨는지
fd status --workspace        # 멤버별 한 줄 요약(세션·선점·큐·자원)
```

배너가 안 뜨면 명부를 못 읽은 것이고, **그 사유는 세션 응답이 말한다**(파일이 없다 ·
블록이 없다 · 파싱 실패 중 무엇인지). 멤버 경로가 서버에서 안 읽히면(컨테이너의 `FD_REPOS`
마운트 밖) 그 멤버의 브랜치가 「?(못 읽음)」으로 남는다.

**④ 총괄 → 세부로 넘긴다.**

```bash
fd add --id umbrella --title "총괄" --body "..."                 # 루트에
fd finish umbrella --body "..." --followups '[{"id":"detail","title":"세부","body":"...","project":"search-api"}]'
fd next --project search-api          # 그 레포의 추천
fd pick detail --project search-api   # 워크트리 명령이 그 레포 경로로 나온다
```

`fd move <id> --project <멤버>` 로 이미 만든 항목을 옮길 수도 있다 — 옮기면 그 항목을
선행으로 가리키던 참조를 **새 프로젝트로 다시 쓴다**(관계가 따라온다).

**⑤ 배타 자원은 워크스페이스에 하나다.**

```bash
fd land --resource env:dell --project search-api   # 형제가 쥐고 있으면 줄을 선다
fd land --project search-api                       # 랜딩 레인은 레포별 — 형제와 안 겹친다
```

> **codex 로 루트에서 띄울 때**: codex 의 프로젝트 루트 판정은 «가장 가까운 `.git`» 이라,
> 하위 레포 안에서 띄우면 루트의 `AGENTS.md` 를 안 읽는다. 루트에서 띄우거나
> `project_root_markers` 로 루트를 못박아라. (이 문장은 codex 문서를 근거로 적었고,
> 이 저장소가 직접 실측한 값은 아니다 — 설계 §14 의 실측 표와 구분한다.)
