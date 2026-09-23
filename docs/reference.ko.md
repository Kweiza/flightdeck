[English](reference.md) | **한국어**

# 도구 레퍼런스

MCP 도구, 터미널 `fd`, 스킬의 전체 목록과 쓰는 규율.

[← README](../README.ko.md)

---

## 세션 안에서 — MCP 도구 10개

외워야 할 것은 넷이다: **집고(`pick`) · 남기고(`note`) · 끝내고(`finish`) · 줄 선다(`land`).**

| 도구 | 파라미터 | 언제 부르나 |
|---|---|---|
| `board` | `detail` | 지금 어느 작업이 잡혀 있나. 선점을 든 카드만 낸다 |
| `pick` | — | 추천 1건 + 왜 + **탈락 사유 전부**. 선점하지 않는다 |
| `pick` | `item_id` / `item_ids` | 선점 + 항목 본문 + 연결된 판단 + 브랜치·워크트리 명령 |
| `pick` | `leave` | 내 선점을 놓는다. 항목은 `open` 으로 돌아가고 **id·이력이 산다** |
| `note` | `kind` `body` `item_id` `supersedes` | 판단을 남긴다. 종류 8: `handoff` `decision` `blocked` `ask` `rejected` `not-done` `verified` `draft` |
| `add` | `id` `title` `body` `paths` `after` `labels` | 큐 항목. **id 가 그대로 브랜치 이름**이 된다 |
| `finish` | `item_id` `outcome` `body` `followups` | 판단+후속+종료+반납을 한 호출·한 트랜잭션으로 |
| `alloc` | `counter_name` | 원자 발번(개정 차수 같은 논리 카운터) |
| `land` | `resources` `result` `detail` `leave` | 랜딩 줄에 선다 / 내 차례를 본다 / 보고하고 반납한다 |
| `label` | `item_id` `add` `rm` | 표시 전용 꼬리표. `tickler` 만 굶김 축에서 빠진다 |
| `amend` | `item_id` `title` `body` `paths` `reason` | 항목의 **제목·본문·경로**를 제자리에서 고친다 — 고치는 축은 그 셋뿐이다. 준 것만 바뀌고 옛 값은 개정 이력에 남는다 |
| `show` | `item_id` | 항목 하나를 읽는다 — 지금 본문 + 개정 이력 + 걸린 판단 전문. **닫힌 항목도 된다**: 선점이 아니라 읽기다. 그것이 이 동사의 전부다 — `judgment_link` 를 역방향으로 읽던 것이 `pick` 하나였고 그쪽은 `state='open'` 만 줘서, 닫힌 항목에 걸린 판단 1,985건이 도달 불가였다(그중 `ask` 5건). 출력은 6,000토큰 상한이고 넘치면 오래된 판단부터 제목만 내며 **그 사실을 말한다** |

세 가지 규율이 이 표에 숨어 있다.

1. **후속은 `add` 가 아니라 `finish` 의 `followups` 에 싣는다.** 미리 `add` 하면 판단과의
   연결을 영영 못 산다 — 같은 호출에 넣어야 판단에 이어진다.
2. **선점을 놓을 때는 `pick(leave:)` 다.** `finish(dropped)` 로 때우면 id 가 바뀌어 이력이 끊긴다.
3. **판단은 덮어쓰지 않는다.** 정정은 `note(supersedes: <판단 id>)` 로 새 행을 얹는다.

## 터미널에서 — `fd`

```bash
fd status                 # 서버 상태 배너 + 보드
fd next                   # 추천만
fd pick <item-id> [<item-id>…]  # 선점(여럿이면 첫째가 선두)
fd note --kind decision --body "왜 그렇게 했나"
fd finish <item-id> --outcome done --body "① 왜 ② 기각 ③ 안 한 것 ④ 확인만 한 것"
fd land                   # 랜딩 줄에 선다(--ok|--fail <사유>|--leave <사유> 로 보고·이탈)
fd lane wait              # 내 차례를 턴 안에서 기다린다
fd lane release --row <id> --reason "왜 끊었나"   # 물린 줄 행을 사람이 회수한다
fd claim release --item <id> --reason "왜 끊었나" # 무신호 세션의 선점을 사람이 회수한다
fd doctor                 # 이 머신의 플랫폼 축과 서버 상태를 실제로 잰다
```

## 스킬 4개 — 순서를 외우지 않으려고 있다

| 스킬 | 언제 | 무엇을 하나 |
|---|---|---|
| `/fd-setup` | 머신을 처음 켤 때 | 상태를 재고 서버/클라이언트를 정한 뒤 **필요한 것만** 설치·기동 |
| `/fd-pickup` | 세션을 시작할 때 | 보드 확인 → 추천 → 선점 → **연결된 판단 읽기** 순서로 |
| `/fd-handoff` | 작업이 끝났을 때 | 판단 저장 + 후속 등록 + 항목 종료 + 자원 반납을 한 호출로 |
| `/fd-update` | 보드가 낡았을 때 | 서버·플러그인·DB 를 최신으로 |

---

## 플러그인이 붙이는 것

켜면 다음이 자동으로 붙는다.

| 무엇 | 언제 |
|---|---|
| `SessionStart` 훅 | 세션을 등록하고 보드 요약·내 선점·미확인·**서버 상태 배너**를 주입한다 |
| `UserPromptSubmit` 훅 | `prompt` 신호 + 미확인 알림 |
| `PostToolUse`(Edit\|Write) 훅 | `tool` 신호 + **미커밋 발자국** — 경로 겹침 축의 유일한 원천 |
| `PreCompact` 훅 | 압축 직전 좌표를 초안 판단으로 남긴다 |
| `Stop` 훅 | 턴이 끝날 때 처방을 물어 `additionalContext` 로 주입한다 |
| `SessionEnd`(clear) 훅 | `/clear` 로 대화가 떠난 것을 관측으로 남긴다 |
| MCP 도구 10개 | `board` `pick` `note` `add` `finish` `alloc` `land` `label` `amend` `show` |
| 스킬 4개 | `fd-pickup` · `fd-handoff` · `fd-setup` · `fd-update` |

**훅은 전부 fail-open 이다.** `bin/fd` 는 셸 런처고, 첫 훅이 `server/` 를 빌드해
`~/.cache/flightdeck/bin` 에 **소스 트리별로** 캐시한다 — 자리가 채널(훅·MCP·셸)을 안 타고,
서로 다른 소스 트리는 **서로 다른 파일**을 갖는다. **Go 가 없으면 안내만 내고 세션은 그대로
진행된다.**
