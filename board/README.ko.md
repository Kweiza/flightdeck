[English](README.md) | **한국어**

# 소유자 칸반 보드

flightdeck 큐를 칸반으로 보는 사람(소유자)의 작업 데스크톱. 원본은 fd 하나이고 보드는 상태를 갖지 않는다.
소유자가 보드에서 한 일은 fd 의 꼬리표와 판단 노트로만 남는다. 세션(Claude Code · Codex)은 이 규약을
MCP 안내문 한 줄(`server/internal/mcpsrv/protocol.go` 의 `Instructions`)과 `fd-pickup` · `fd-handoff` 스킬로 받는다.
**이 파일이 규약의 정본이다** — 다른 곳에 옮겨 적지 말고 이 파일을 가리킨다.

[← README](../README.ko.md)

---

## 카드 본문 규약

작업자 · 조정자는 fd 항목 본문에 아래 줄을 쓴다. 본문 안 아무 곳의 줄을 접두어로 알아보고, 한 종류를 여러 줄 써도 된다.
읽는 곳은 `board.py` 의 `directives_of()` 한 곳이다.

    미리보기: https://...            웹 미리보기 URL -> 카드에 열기 버튼, 상세에서는 iframe(막히면 새 탭 버튼)
    그림: /절대/경로.png             로컬 이미지 -> 카드에 썸네일, 누르면 크게
    영상: /절대/경로.mp4|webm|mov    로컬 영상 -> 상세에서 <video controls>, 카드에 표시
    파일: /절대/경로.md|txt|json|log 로컬 글 파일 -> 상세에서 내용 보기(md 는 렌더, 긴 로그는 끝 200줄)
    링크: 이름 | https://...          바깥 링크(콘솔, PR, 스토어) -> 버튼
    커밋: <sha>                      GitHub 링크로(그 프로젝트 저장소의 origin 에서 주소를 만듦)
    소유자: 무엇을 · 어디서 · 버튼 이름   소유자가 할 일 한 줄 -> 카드 맨 위 강조
    선택: A안 설명 | B안 설명 | ...     소유자에게 묻는 질문의 보기 -> 상세에 버튼(결정 꼴이면 보기 카드마다)

**소유자에게 고르게 할 때**: `선택:` 줄만 두지 않는다. 본문에 이 네 덩어리를 다 쓴다.
1. 질문: 무엇을 정해야 하는지 한두 문장, 지금 정해야 하는 까닭.
2. 배경: 지금 상태 · 잰 숫자 · 비교 대상(원작 · 경쟁작 · 이전 판)은 어떻게 하는지 · 정하지 않으면 무엇이 막히는지.
3. 보기마다 한 단락: 무엇이 바뀌는지, 좋은 점, 나쁜 점 · 위험, 돈(수익)에 미치는 영향, 되돌릴 수 있는지, 드는 일.
4. 추천: 어느 보기를 왜, 판정자 두 의견(Claude · Codex)이 갈렸으면 둘 다.
그 다음에 `선택:` 줄(보기 이름은 3. 의 이름과 같게, 추천 보기 이름 끝에 「(추천)」).

본문은 이 꼴로 쓴다. 보드가 이 머리들을 알아보고 상세 맨 위에 「결정」 묶음(질문 판, 접는 배경, 보기마다 카드와 고르기 버튼,
추천 보기 테두리 · 「추천」 띠, 판정자 의견 나란히)으로, 목록 카드에는 질문 첫 줄과 「추천: 이름」 칩으로 보인다.

    ### 질문
    한두 문장. 지금 정해야 하는 까닭.
    ### 배경
    지금 상태 · 잰 숫자 · 비교 대상 대조 · 안 정하면 막히는 것 (목록 가능)
    ### 보기: A안 이름
    바뀌는 것 / 좋은 점 / 나쁜 점 · 위험 / 수익 영향 / 되돌릴 수 있는지 / 드는 일
    ### 보기: B안 이름 (추천)
    ...
    ### 추천
    어느 보기를 왜.
    Claude: (그 판정자가 고른 보기 이름과 까닭)
    Codex: (같은 꼴)
    선택: A안 이름 | B안 이름 (추천)

- 머리는 `###` 그대로. `### 질문` 과 `### 보기:` 하나 이상이 있어야 결정 카드로 본다(없으면 옛 꼴: 상세에 `선택:` 보기 버튼 줄).
  각 절은 다음 `##` · `###` 머리까지이고, 절 안의 규약 줄(`선택:` · `그림:` 등)은 절 글에서 빠진다(제 자리에 따로 보인다). 절 글은 마크다운.
- `### 보기:` 뒤 이름과 `선택:` 줄 보기는 끝의 「(추천)」을 뗀 채로 맞댄다. 맞는 것이 없는 보기는 버튼 없이 「고를 수 없음」.
  추천 표시는 머리나 `선택:` 줄 어느 쪽의 「(추천)」이든 된다.
- 판정자 줄은 `### 추천` 안에서 한 줄에 한 사람(`Claude:` · `Codex:` · `Fable:` · `Opus:` · `astra:`, 앞에 `- ` 가능).
  각 줄이 처음 이름 댄 보기가 다르거나, 추천 절에 「의견 갈림」 · 「갈렸」이 있으면 「의견 갈림」 표시.
- 고르기는 보기 카드의 버튼 → 카드 안 확인 줄 「고르기 확정」(브라우저 confirm 없음) → 기존 `/api/choose`.
  고른 뒤(가장 늦은 `소유자 선택:` 노트, 조정자가 다시 `status:owner` 로 묻는 중이 아니면) 그 보기에 「고름」, 버튼은 잠긴다.
  읽는 곳은 `board.py` 의 `decision_of()` · `chosen_of()` 한 곳이다.

- 로컬 경로는 허용 뿌리(`BOARD_FILE_ROOTS`, 기본 `~/Developments` · `/private/tmp/claude-<uid>`) 아래만 보인다(realpath 로 판정, 기호 링크로 밖에 나가면 403).
  아는 글 · 그림 · 영상 확장자(board.py TEXT_TYPES · MEDIA_TYPES)만 준다. 숨김 경로 조각(`.claude` · `.playwright-mcp` 만 예외, 그래서 `.certs` · `.env` · `.git` · `.ssh` 는 막힘)과 비밀 꼴 이름(조각 안 어디든 key · secret · token · credential · password · passwd · private · auth, 그래서 monkey.png 같은 오탐도 막힘, `*.pem` · `*.p12` · `*.jks`)은 403. 이 보드의 이름(127.0.0.1 · localhost · `BOARD_HOSTS`)이 아닌 Host 는 421.
- 미리보기 iframe: 보드는 스레드 Godot 를 돌리려고 교차 출처 격리(COOP same-origin, COEP credentialless)로 뜬다.
  그래서 미리보기 서버가 `Cross-Origin-Embedder-Policy` 와 `Cross-Origin-Resource-Policy: cross-origin` 을 함께 보내야
  iframe 이 열린다. 보드가 `GET /api/embed?url=` 로 먼저 머리를 보고, 못 열면 까닭과 「새 탭에서 열기」만 보인다.

## 무엇인가

`board.py` 하나(파이썬 3 표준 라이브러리만)가 정적 페이지(`index.html`)를 주고, fd REST API 를 대신 부르고
(토큰은 이 프로세스만 안다), 로컬 파일을 `GET /file?path=` 로 준다(영상은 Range 206, `tail=N` 은 끝 N 줄).
보드는 **컨테이너 안이 아니라 호스트 프로세스**다 — 작업자가 카드에 다는 그림 · 로그가 호스트 디스크에 있다.

- 주소: `http://127.0.0.1:7430/`(`?project=<id>` 프로젝트, `?item=<id>` 그 카드 상세를 열고 시작). 다른 기기에서 보려면
  tailnet 등으로 노출한다(예: `tailscale serve --bg --https=37430 http://127.0.0.1:7430`) — 보드는 그 설정을 건드리지 않는다.
- 상세는 데스크톱에서 옆 판, 폰(900px 이하)에서 전체 화면. 30초마다 새로 읽되 바뀐 것이 없으면 화면을 건드리지 않고,
  열린 상세 판은 소유자 동작 뒤에만 다시 그린다.
- 프로젝트 고르기: 위 칸. 처음 값은 `?project=` → 이 브라우저가 마지막에 본 것 → `BOARD_PROJECT` → 목록의 첫째.

## 설치 (macOS launchd)

    board/install.sh             # plist 를 쓰고 다시 올린 뒤 /healthz 200 이면 BOARD_READY 한 줄
    board/install.sh --print     # 쓸 plist 만 보기
    board/install.sh --uninstall

- 라벨 `com.<사용자>.flightdeck-board`, KeepAlive, 로그 `~/Library/Logs/flightdeck-board.log`.
  plist 는 `install.sh` 가 있는 체크아웃의 `board.py` 를 가리킨다 — 옮긴 뒤에는 새 자리에서 다시 부른다.
- 다시 부르면 지금 plist 의 설정 값을 이어받는다(안 준 변수만). 값을 바꾸려면 변수를 주고 다시 부른다.
- 코드만 고친 뒤: `launchctl kickstart -k gui/$(id -u)/com.$(id -un).flightdeck-board`.
- macOS 밖에서는 `python3 -I board/board.py` 를 그 OS 의 서비스로 띄운다(설정은 아래 환경 변수).

## 설정

모두 환경 변수다(`install.sh` 가 plist 에 적는다). 비우면 기본값.

| 변수 | 기본 | 뜻 |
| --- | --- | --- |
| `BOARD_PORT` · `BOARD_ADDR` | `7430` · `127.0.0.1` | 듣는 자리 |
| `BOARD_PROJECT` | (없음) | 처음 보일 프로젝트. 없으면 목록의 첫째 |
| `BOARD_HOSTS` | (없음) | 루프백 밖에서 부를 이름 `이름:포트`(쉼표 구분). `install.sh` 는 비면 tailscale 이름과 `BOARD_TS_PORT`(37430)로 채운다 |
| `BOARD_FILE_ROOTS` | `~/Developments:/private/tmp/claude-<uid>` | `/file` 허용 뿌리(콜론 구분) |
| `BOARD_LINKS` | `~/.config/flightdeck-board/links.json` | 위 링크 줄(기계별 파일, 저장소 밖). 꼴은 `links.example.json`: `[{"name", "url", "project"?}]`, `project` 가 있으면 그 프로젝트에서만. 없으면 링크 줄이 빈다. 요청마다 다시 읽음 |
| `FD_CONFIG` | `~/.flightdeck/config.json` | fd 주소 · 토큰을 읽는 파일(fd 클라이언트 설정과 같은 것, 요청마다 다시 읽음) |
| `FD_URL` · `FD_TOKEN` | (설정 파일) | 있으면 설정 파일보다 먼저 |
| `BOARD_TAILNET` | (install.sh: tailscale 이름의 접미사) | `/api/embed` 가 부를 tailnet 접미사(예: `<tailnet>.ts.net`) |
| `BOARD_PREVIEW_PORTS` | (없음) | `/api/embed` 가 부를 이 맥(localhost)의 포트(쉼표 구분) |
| `BOARD_AUTH` | `~/.config/flightdeck-board/auth.json` | 접근 비밀 · 소유자 로그인(아래 「접근」) |

커밋 링크(`커밋:` 줄, 닫힌 항목의 랜딩 sha)는 그 프로젝트 저장소의 `origin`(fd `/api/v1/projects` 의 path)에서 GitHub 주소를 만든다.

## 접근

보드는 fd 토큰을 쥐고 소유자 대신 fd 에 쓴다. 그래서 누가 부르는지를 둘로 가른다(`board.py` 의 `access_denied` 한 곳).

- **tailnet 으로 온 요청**(Host 가 `BOARD_HOSTS` 의 tailnet 이름): `tailscale serve` 가 붙이는 `Tailscale-User-Login` 이
  접근 설정 파일의 `owner_logins` 에 있어야 한다. 없거나 다르면 403. 이 헤더를 바깥에서 위조할 수 없는 것은 보드가
  루프백에만 묶여 serve 뒤에만 있을 때라, `BOARD_ADDR` 가 루프백이 아니면 보드는 뜨지 않는다(`check_bind`).
- **모든 요청의 `/api/*` · `/file`**: `/` 가 내려 주는 쿠키(`fdboard`, HttpOnly · SameSite=Strict, 값은 설치 때 만든 무작위 비밀)가
  있어야 한다. 다른 사이트의 페이지가 소유자 브라우저로 보드를 부르는 길을 막는다.
- 접근 설정 파일 `BOARD_AUTH`(기본 `~/.config/flightdeck-board/auth.json`, 0600): `{"secret": …, "owner_logins": […]}`.
  `install.sh` 가 없으면 만들고(로그인은 `BOARD_OWNER_LOGIN` 또는 이 기계 tailscale 사용자), 값을 화면 · 로그에 찍지 않는다.
  저장소 · 보고 · 카드에 그 값을 쓰지 않는다. 파일이 없으면 보드는 뜨지 않는다.
- **남는 위험**: 같은 맥의 같은 사용자 프로세스는 `/` 에서 쿠키를 받고 Host · 헤더도 흉내 낼 수 있다. 보드는 그 경계를 지키지 않는다
  (그 프로세스는 이미 `~/.flightdeck/config.json` 의 토큰을 읽을 수 있다).
- 미리보기 확인 `/api/embed` 는 같은 출처 요청만 받고, 이 기계 tailnet 접미사(`BOARD_TAILNET`) 아래 이름과 `BOARD_PREVIEW_PORTS` 의
  `localhost` · `127.0.0.1` 만 HEAD 로 부르며, 리다이렉트는 따라가지 않는다.

조정자 · 스크립트가 보드 API 를 부를 때는 쿠키를 먼저 받는다:

    curl -s -c /tmp/fdboard.jar -o /dev/null http://127.0.0.1:7430/
    curl -s -b /tmp/fdboard.jar 'http://127.0.0.1:7430/api/inbox?project=<프로젝트 id>'

## 시험

    python3 -I -B -m unittest discover -s board -p 'test_*.py'

`go test ./cmd/fd` 의 `TestBoardPythonSuite` 가 같은 것을 돌린다. 보드가 읽는 서버 응답 꼴(dashboard.json 의
`open_items` · `sessions[].view.Claims`, `items/{id}` 의 `item` · `judgments`, 닫힌 목록, projects)은
`go test ./internal/api` 의 `TestBoardReadsTheseFields` 가 잠근다 — 서버 쪽 이름을 바꾸면 거기서 빨개진다.

## 열과 상태를 바꾸는 법

열은 fd 의 상태와 꼬리표로 정해진다(`board.py` column_of 한 곳).

| 열 | 판정 |
| --- | --- |
| 끝남 | 닫힌 항목(done · dropped) |
| 진행 중 | 꼬리표 `status:running`, 또는 어느 세션이 선점 중 |
| 소유자 차례 | 꼬리표 `status:owner`(소유자가 할 일), `status:owner-done` · `status:owner-ask`(맨 위 「조정자에게 돌아간 것」) |
| 대기 | 그 밖의 열린 항목 |

작업자를 띄울 때는 `fd add -id <단위> -title ... -body - -label status:running`(이미 있으면 `fd label <단위> -add status:running`),
작업자가 직접 집으면 `fd pick <단위>` 만으로도 진행 중에 뜬다. 소유자에게 넘길 때는
`fd label <단위> -rm status:running -add status:owner` 하고 본문에 `소유자:` 줄(필요하면 `선택:` 줄)을 넣는다.
착륙하면 `fd pick <단위>` 뒤 `fd finish <단위> -outcome done -title ... -body -` 로 닫고 본문에 `커밋: <sha>` 줄을 넣는다.
버릴 때는 `-outcome dropped -close-reason "<까닭>"`. MCP 로는 같은 일을 `add(labels)` · `label(add, rm)` · `pick` · `finish` 로 한다.

## 소유자 동작이 fd 에 남기는 흔적

보드의 쓰기는 POST 만, 같은 출처(Sec-Fetch-Site same-origin 또는 Origin 이 보드 호스트)와 JSON 만 받는다.
판단 노트는 모두 kind `decision`, 제목 머리 `[fdboard]` 이고 그 항목에 링크된다(세션 없음).

| 동작 | 꼬리표 | 판단 노트(제목 / 본문) |
| --- | --- | --- |
| 댓글 | 그대로 | `[fdboard] 소유자 댓글` / 쓴 글 |
| 「했어」(status:owner 카드만) | `-status:owner +status:owner-done` | `[fdboard] 소유자: 했어` / `소유자: 했어` |
| 선택 버튼 | `-status:owner +status:owner-done` | `[fdboard] 소유자 선택: <보기>` / `소유자 선택: <보기>` |
| 상태 옮기기(끌어 놓기 · 상세 메뉴) | 진행 중 `+status:running`, 소유자 차례 `+status:owner`, 대기 = 넷 다 뗌(옮길 때 다른 status:* 는 뗌) | 없음 |
| 끝남으로 옮기기 | 막음(닫기는 조정자가 커밋과 함께), 안내 팝업만 | 없음 |
| 새 카드 | 새 항목 id `owner-<YYYYMMDD-HHMMSS>-<4자>`, `+status:owner-ask` | 없음(본문 끝에 「(소유자가 보드에서 맡긴 일)」) |

선점된 카드는 꼬리표를 바꿔도 진행 중에 남는다(선점이 열을 이긴다). 닫힌 항목은 꼬리표를 바꾸지 않는다(댓글은 됨).

## 조정자가 읽는 곳: `/api/inbox`

    curl -s -c /tmp/fdboard.jar -o /dev/null http://127.0.0.1:7430/
    curl -s -b /tmp/fdboard.jar 'http://127.0.0.1:7430/api/inbox?project=<프로젝트 id>'

- `items`: 열린 항목 중 `status:owner-done` · `status:owner-ask` 인 것(`kind`, 본문, 그 항목의 `[fdboard]` 노트 시간순).
- `recent_owner_notes`: 그 프로젝트의 최근 `[fdboard]` 노트 30개(최신 먼저, `item` = 항목 id). 댓글만 남긴 카드도 여기서 보인다.

조정자는 읽고 처리한 뒤 꼬리표를 정리한다: 확인 끝이면 닫기(`finish`), 일을 시작하면 `fd label <id> -rm status:owner-done -add status:running`,
소유자에게 되물을 것이면 `-rm status:owner-done -add status:owner`.
