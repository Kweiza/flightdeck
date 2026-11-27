[English](README.md) | **한국어**

# flightdeck

**병렬 에이전트 세션을 위한 조정 계층.** 자체 호스팅 서버 하나에 Claude Code 와 codex 세션이
붙어, 서로가 무엇을 하고 있는지 보면서 일한다.

세션끼리는 대화할 수단이 없다. 그래서 각 세션은 "저쪽이 뭘 집었나"를 추측하고, 추측이 틀리면
남이 하던 일을 통째로 다시 한다. flightdeck 은 그 추측을 없앤다 — 누가 살아 있나 · 어느 경로를
만지나 · 무엇을 집었나를 **git 과 데이터베이스에서 파생해** 모든 세션에 보여 준다.

새 세션을 열면 아무것도 안 쳐도 보드가 먼저 뜬다.

```
보드 · my-app · 2026-08-13 06:40 UTC · 파생 git@06:40:08 최신
잡혀 있는 작업 0건 (선점 기준이다 — 세션의 생사가 아니다)

잡혀 있는 작업이 없다 — 아무 세션도 큐 항목을 쥐고 있지 않다. 서버 장애가 아니다.
창 밖 73건 (가장 오래된 신호 8일 10시간 전) — 창은 표시 구간이지 생존 판정이 아니다
큐 열림 4건
  · 19시간 22분·티클러(08-19 발화) · fd-folded-multi-turn-drain-unmeasured — 다턴 배수가 미실측이다
  · 18시간 41분·티클러(08-26 발화) · fd-lane-turn-remeasure-… — lane-turn 축을 재측한다
랜딩 레인 0건(질의는 돌았다)
```

같은 보드가 <http://localhost:7420> 에 읽기 전용 대시보드로도 뜬다.

![flightdeck 대시보드](docs/images/dashboard.png)

<sub>데모 서버에 합성 데이터(`my-app`)를 넣고 찍은 화면이다.</sub>

## 특징

- **락 대신 선점** — `pick` 이 큐 항목을 집으면 그 id 가 그대로 브랜치·워크트리 이름이 된다.
  배타로 쥐는 것은 머지 하나뿐이다.
- **머지 전에 보이는 겹침** — 훅이 편집마다 미커밋 발자국을 보고한다. 두 세션이 같은 경로를
  만지면 턴이 끝날 때 양쪽에 알린다. 막지는 않는다.
- **랜딩 레인** — 머지 앞의 직렬 줄. 차례가 아니면 `fd land` 가 1로 끝나므로
  `fd land && git merge --ff-only "$BRANCH"` 한 줄이 그대로 옳다.
- **항목을 따라다니는 판단** — 왜 그렇게 했나, 무엇을 기각했나, 무엇을 일부러 안 했나를 남기면
  그 항목을 다음에 집는 세션이 전문으로 받는다.
- **적지 않고 파생한다** — 브랜치·HEAD·발자국·활동 신호는 git 과 훅에서 온다. 파생할 수 있는
  사실에는 쓰기 API 가 아예 없다.
- **Claude Code 와 codex 를 한 보드에** — 두 하네스의 세션이 같은 겹침 판정을 받는다.
- **서버가 죽어도 조용하지 않다** — 모든 세션에 배너를 띄우고 되는 것과 안 되는 것을 나눠 말한다.

## 동작 방식

1. **서버** — Go 바이너리 하나(SQLite)를 Docker 로 띄운다. 저장소를 읽기 전용으로 마운트해
   브랜치·커밋·미커밋 변경을 직접 읽는다.
2. **훅** — 세션을 등록하고, 활동 신호와 편집 발자국을 보내고, 턴이 끝날 때 겹침 같은 처방을
   세션에 주입한다.
3. **도구** — 에이전트는 MCP 도구로, 사람은 터미널 `fd` 로 같은 동작을 부른다.
4. **대시보드** — <http://localhost:7420> 에 읽기 전용 보드 한 장.

한 세션의 하루는 이렇게 흐른다.

```
세션 시작      SessionStart 훅이 보드를 주입한다
pick           무엇을 집을지 추천만 한다 (선점하지 않는다)
pick(item_id)  선점 + 항목 본문 + 연결된 판단 + 워크트리 명령
작업           PostToolUse 훅이 편집마다 발자국을 보고한다
finish         판단 + 후속 등록 + 종료 + 반납을 한 트랜잭션으로
land           랜딩 줄에 선다 → 차례가 오면 머지 → land(result: ok)
```

겹침·랜딩 레인·항목 상태·활동 신호의 자세한 모양은 [개념과 동작](docs/concepts.ko.md)에 있다.

## 빠른 시작

필요한 것: **Docker**(서버 머신) · **Go 1.25 이상**(세션을 돌리는 머신 — `bin/fd` 가 첫 호출에
빌드한다) · git.

**1. 서버를 띄운다** — 한 머신에서 한 번.

```bash
git clone https://github.com/Kweiza/flightdeck.git
cd flightdeck
docker compose up -d --build
curl -s localhost:7420/healthz
```

`/healthz` 가 `{"ok":true,"api_version":"1","db_ok":true,…}` 를 내면 된 것이다. `--build` 는 받은 판으로 이미지를 짓는다 -
이미지 이름이 판을 안 타는 고정 이름(`flightdeck:local`)이라 빼면 앞서 지은 옛 이미지로 뜬다.

**2. 플러그인을 설치한다** — 세션을 돌리는 머신마다.

```
/plugin marketplace add Kweiza/flightdeck
/plugin install flightdeck@flightdeck
```

**3. 세션을 연다.** 보드가 먼저 뜨면 된 것이다. 일은 `/fd-pickup` 으로 시작한다.

> 1·2 단계는 `/fd-setup` 스킬이 대신 해 준다 — 이 머신이 서버인지 클라이언트인지 묻고,
> 없는 것만 승인받아 설치한다.

- **서버가 다른 머신이면** 세션 쪽에 `FD_URL=http://<서버>:7420` 을 준다(토큰을 쓰면 `FD_TOKEN` 도).
- **도커 없이** 돌리려면 `cd server && go run ./cmd/fd serve --addr :7420 --db ~/.flightdeck/fd.db`.
  컨테이너와 같은 DB 를 가리키므로 **둘을 같이 돌리지 마라.**
- **이미 `flightdeck@kweiza-cc-plugins` 로 깔았다면** 그대로 둬라 — 같은 저장소를 받는다. 둘을 함께 켜지는 마라.

## 쓰는 법

### 세션 안에서 — MCP 도구 10개

외워야 할 것은 넷이다: 집고(`pick`) · 남기고(`note`) · 끝내고(`finish`) · 줄 선다(`land`).

| 도구 | 하는 일 |
|---|---|
| `board` | 지금 누가 무엇을 쥐고 있나 |
| `pick` | 인자 없으면 추천만 한다. `item_id` 를 주면 선점, `leave` 를 주면 반납 |
| `note` | 판단을 남긴다 — `decision` `handoff` `blocked` `ask` `rejected` `not-done` `verified` `draft` |
| `add` | 큐 항목을 만든다. id 가 그대로 브랜치 이름이 된다 |
| `finish` | 판단 + 후속 등록 + 종료 + 반납을 한 트랜잭션으로 |
| `land` | 랜딩 레인(또는 이름 붙인 자원)의 줄에 선다 / 보고하고 반납한다 |
| `alloc` | 원자 발번 — 개정 차수 같은 논리 카운터 |
| `label` | 표시용 꼬리표 |
| `amend` | 항목의 제목·본문·경로를 고친다. 옛 값은 개정 이력에 남는다 |
| `show` | 항목 하나를 읽는다 — 닫힌 항목도 된다 |

### 터미널에서 — `fd`

```bash
fd status                         # 서버 상태 배너 + 보드
fd next                           # 추천만
fd pick <item-id>                 # 선점
fd finish <item-id> --outcome done --body "왜 · 기각한 것 · 안 한 것 · 확인한 것"
fd land && git merge --ff-only "$BRANCH"
fd doctor                         # 이 머신과 서버의 상태를 실제로 잰다
```

### 스킬 4개

| 스킬 | 언제 |
|---|---|
| `/fd-setup` | 머신을 처음 켤 때 — 서버/클라이언트를 정하고 필요한 것만 설치·기동 |
| `/fd-pickup` | 세션을 시작할 때 — 보드 → 추천 → 선점 → 연결된 판단 읽기 |
| `/fd-handoff` | 작업을 마칠 때 — 판단·후속·종료·반납을 한 호출로 |
| `/fd-update` | 서버·플러그인·DB 를 최신으로 |

파라미터 전체와 쓰는 규율은 [도구 레퍼런스](docs/reference.ko.md)에 있다.

### 소유자 칸반: `board/`

사람이 큐를 칸반으로 보고 그 자리에서 답하는 웹 보드(파이썬 표준 라이브러리 한 파일, 호스트 프로세스).
열은 꼬리표(`status:running` · `status:owner` · `status:owner-done` · `status:owner-ask`)로 갈리고, 카드는 본문의
규약 줄(`미리보기:` · `그림:` · `커밋:` · `소유자:` · `선택:` …)로 그려진다. 세션은 그 규약을 MCP 안내문 한 줄로 받는다.
설치 · 설정 · 규약 전문은 [board/README.ko.md](board/README.ko.md).

## codex

```bash
fd setup --install-codex   # ~/.local/bin/fd-hook · ~/.local/bin/fd · ~/.codex/hooks.json
codex                      # 한 번은 TUI 로 띄워 "Hooks need review" 를 승인한다
```

- **승인 전에는 훅이 아무 말 없이 안 돈다.** 확인은 `fd doctor` 의 codex 절로 한다.
- codex 기본 샌드박스는 네트워크를 끊는다 — `codex -c sandbox_workspace_write.network_access=true`.
- `~/.local/bin` 을 PATH 에 넣는다. codex 에는 MCP 도구가 없고 터미널 `fd` 를 쓴다.

왜 그렇게 생겼는지와 되는 것·안 되는 것의 전체 표는 [codex 에서 쓰기](docs/codex.ko.md)에 있다.

## 설정

서버는 compose 가 읽는 환경 변수로, 세션은 `fd setup` 이 쓰는 `~/.flightdeck/config.json` 이나
환경 변수로 설정한다. Postgres 원장의 변수(`FD_PG_*` · `FD_BACKUP_HOST`)는 덮개 파일 `compose.postgres.yaml` 을 얹을 때만 읽히고, 선택 파일 `/etc/flightdeck/compose.env` 에 적는다(아래 「Postgres 원장」).

| 변수 | 쪽 | 뜻 |
|---|---|---|
| `FD_TOKEN` | 서버 · 세션 | 인증 토큰. 안 주면 인증 없이 뜨고 `/healthz` 가 그 사실을 알린다 |
| `FD_REPOS` | 서버 | 저장소가 있는 자리(기본 `$HOME`). 파생의 전부가 여기서 나온다 — **갱신할 때 잃지 마라** |
| `FD_REPOS2`–`FD_REPOS4` | 서버 | 저장소가 여러 트리에 흩어져 있을 때 추가 마운트 |
| `FD_UID` · `FD_GID` | 서버 | 호스트 사용자 id(기본 1000) |
| `FD_LEDGER_HOST` | 서버 | 원장 백업 자리(기본 `~/.flightdeck-ledger`) |
| `FD_BACKUP_DIR` | 서버 | `fd migrate` 가 Postgres 원장의 판을 올리기 전에 뜨는 덤프 자리(`--backup-dir` 가 먼저고, 둘 다 비면 `/backups` 가 있을 때 그곳). Postgres 겹의 compose 는 `/backups`(호스트 쪽은 `FD_BACKUP_HOST`)로 준다. **배포마다 따로 둔다**: 물리 복제로 떠 온 시험 시스템은 원래 시스템과 원장 식별(클러스터 식별자 · DB 이름)이 같아서, 한 자리를 나눠 쓰면 되돌리기가 다른 배포의 덤프를 고를 수 있다 |
| `FD_DUMP_TIMEOUT` | 서버 | Postgres 원장의 덤프 한 번의 기한(Go 시간 표기, 기본 `30m`, 1초 이상). `fd migrate` 가 판을 올리기 전에 뜨는 덤프와 `db-backup` 의 매일 덤프가 같은 값을 쓴다 - 둘 다 「이 원장의 정상 덤프 시간 + 여유」다. 판올림 전 덤프 동안에는 업무 쓰기가 기다리므로, 덤프가 멈추면(백업 볼륨이 응답을 멈추면) 이 기한이 쓰기가 막히는 가장 긴 시간이다. 기한이 지나면 pg_dump 를 멈추고 서버가 실행기 연결을 끊어 잠금을 풀며, 판은 올리지 않는다. 매일 덤프는 기한이 지나면 pg_dump 를 끊고 그날을 실패로 친다(멈춘 덤프가 표 잠금을 쥔 채 서 있으면 `fd migrate` 의 DDL 이 실패한다). 큰 원장에서 정상 덤프가 더 걸리면 늘린다 |
| `FD_BACKUP_HOST` | 서버 | Postgres 겹의 덤프 자리. **기본값이 없다** - 비면 Postgres 겹의 `up` 이 선다. `fd migrate` 의 판올림 전 덤프가 바로 아래에, `db-backup` 의 매일 덤프가 `daily/<DB>/` 에 쌓인다(새것 14개). 덤프는 원장 전체라 **서버 마운트(`FD_REPOS` - 기본 홈 전체 - 와 `~/.flightdeck`) 밖에 미리 만든다**(예 `/var/backups/flightdeck`, 주인 `FD_UID`, 권한 `0700`). **배포마다 따로 둔다**: 같은 DB 이름을 쓰는 두 배포가 한 자리를 나누면 서로의 덤프로 때를 재고 서로의 덤프를 회전한다. 준비는 아래 「Postgres 로 띄우기」 |
| `FD_PG_INSTANCE` | 서버 | Postgres 겹의 배포 이름(기본 `flightdeck`). db 컨테이너(`<이름>-db`)와 두 볼륨(`<이름>-pgdata` · `<이름>-pgrun`)의 이름이 여기서 함께 나온다. **선택 파일에만 적는다** - 셸에서 주면(빈 값도) `fdc` · `fd setup` 의 기동 · `fd-update` 의 관문이 compose 를 부르기 전에 선다. 셸 값은 선택 파일보다 먼저라 그 값이 빠진 새 셸에서 다른 볼륨(빈 원장)을 고르기 때문이다. 운영은 기본값 그대로 두고, 예행 · 시험만 그 환경의 선택 파일에 다른 값을 적는다(아래 「업그레이드: SQLite 원장을 Postgres 로」의 예행) |
| `FD_PG_SERVER_PGPASS_HOST` · `FD_PG_BACKUP_PGPASS_HOST` | 서버 | Postgres 겹의 접속 계정 둘(서버 · 매일 덤프)의 pgpass 파일. **기본값이 없다** - 비면 Postgres 겹의 `up` 이 선다. 주인 `FD_UID`, 권한 `0600`, 줄 `db:5432:<DB>:<계정>:<비밀번호>`. 비밀번호가 들어가는 유일한 자리다(환경 변수 · 명령줄 · URL 에 두지 않는다). **서버 마운트 밖에 둔다** - 서버는 `FD_REPOS`(기본 홈 전체)를 읽는다(아래 「Postgres 로 띄우기」) |
| `FD_PG_DATABASE` · `FD_PG_SERVER_USER` · `FD_PG_BACKUP_USER` | 서버 | Postgres 겹의 DB 와 접속 계정 이름(기본 `flightdeck` · `fd_server` · `fd_backup`, 소문자 · 숫자 · `_` 만). 바꾸면 선택 파일에 적는다 - `FD_PG_DATABASE` 는 어느 원장인지를 정해 `FD_PG_INSTANCE` 처럼 셸에서 주면 관문이 선다. 계정은 `db-roles` 가 만들고 잰다 - 서버 계정은 `fd_app`, 덤프 계정은 `pg_read_all_data` 의 구성원이고 특권 속성이 없다. 덤프 자리는 DB 마다 따로라(`daily/<DB>/`) DB 를 바꿔도 옛 DB 의 덤프를 새 DB 가 세거나 지우지 않는다 |
| `FD_URL` | 세션 | 서버 주소(기본 `http://127.0.0.1:7420`) |
| `FD_STATE_DIR` | 세션 | 상태 파일(아웃박스·캐시·머신 id)을 한 자리로 모은다 |
| `FD_LANG` | 서버 · 세션 | 출력 언어. `en` 이면 영어로 낸다(기본 한국어). 서버 쪽은 대시보드, 세션 쪽은 CLI·훅·MCP 결과다 |

각 변수의 함정과 이유는 [서버 운영](docs/server.ko.md)에 있다.

`FD_LANG=en` 은 **출력 경계에서** 옮긴다 — 보드, 대시보드, add·next·pick·note·finish·show, land,
꼬리, Stop 처방이 1차 범위다. 번역표(`server/internal/lang/catalog.go`)에 없는 줄은 **줄째 한국어로**
남는다(반쪽 번역을 안 낸다). `fd doctor`·로그인 화면·드문 진단 문구는 아직 한국어다.

## Postgres 원장

기본 배포의 원장은 SQLite 파일 하나(`~/.flightdeck/fd.db`)다. Postgres 원장은 **고를 때만** 켜진다: 덮개 파일
`compose.postgres.yaml` 을 `compose.yaml` 위에 얹으면 원장 DB(`db`, Postgres 17) · 접속 계정 one-shot(`db-roles`) · 매일
덤프(`db-backup`)가 붙고, `migrate` 와 서버가 그 DB 를 본다. 얹지 않은 `docker compose up` 은 이 판에서도 그대로 SQLite 다.

| 서비스 | 붙는 계정 | 길 · 하는 일 |
|---|---|---|
| `migrate` | 초사용자 `postgres` | 소켓 볼륨(trust). 스키마를 올리고, 손으로 하는 관리 명령(옮기기 · 되돌리기 · 되살리기 · psql)을 맡는다 |
| `db-roles` | 초사용자 | 소켓. 접속 계정 둘을 만들고 재고 비밀번호를 둔다(`deploy/db-roles.sh`). 어긋난 계정은 고치지 않고 멈춘다 |
| `flightdeck` | 서버 계정(`fd_app` 의 구성원) | TCP `db:5432`, pgpass 파일 |
| `db-backup` | 덤프 계정(`pg_read_all_data` 의 구성원) | TCP, pgpass 파일 |

초사용자는 비밀번호가 없다(아무도 모르는 난수로 초기화한다). 접속 규칙(`deploy/pg_hba.conf`)이 초사용자의 TCP 접속을 IPv4 ·
IPv6 모두 거절하므로 소켓 볼륨을 붙인 `migrate` · `db-roles` 만 붙는다. 그 규칙은 기동마다 이미지에 구운 파일에서 읽는다 - 이미
있는 볼륨도 같다. `db` 는 포트를 내지 않는다.

★ **판 디렉토리를 붙이지 않는다.** `db` · `db-roles` · `db-backup` 은 접속 규칙 · 계정 스크립트 · 덤프 스크립트를 구운 이미지
하나(`deploy/Dockerfile`, `flightdeck-pg:local`)로 뜬다. 플러그인 설치본(`…/flightdeck/<버전>`)에서 bind 하면, 갱신 뒤 정리가 그
판 디렉토리를 지운 다음 재부팅 · 크래시로 Docker 가 `db` 를 다시 띄울 때 원천이 없어 원장 DB 가 안 뜬다. 파일 모드도 이미지가
정한다(0644) - 체크아웃의 umask 가 077 이어도 `db` 의 postgres 사용자가 규칙을 읽는다. 이 파일들을 고쳤으면 `--build` 로 다시
짓는다(`/fd-update` 가 그렇게 띄운다).

### Postgres 로 띄우기

이 절과 아래 세 절차(업그레이드 · 켜기 전에 섰을 때 · 72시간 되돌리기)의 명령 덩이는 터미널에 그대로 붙여 넣는 꼴이다. 덩이 안의
설명 줄은 `#` 가 아니라 `: '…'` 다 - 대화형 zsh(macOS 의 기본 셸)는 `#` 를 주석으로 읽지 않아(`interactivecomments` 가 꺼져
있다) `#` 줄을 명령으로 돌리고, `set -e` 덩이는 그 첫 줄에서 선다. 덩이의 docker 조회(`docker ps` · `docker inspect`)는 따로 선
대입(`x=$(docker …)`)으로 받는다 - `[ -n "$(docker …)" ]` · `set -- $(docker …)` 는 docker 가 실패해도 빈 출력을 「없다 · 섰다」로
읽고 지나가지만, 대입은 docker 의 실패로 끝나 덩이가 그 자리에서 선다. `up` 은 모두 `--build` 로 짓고 띄운다 - 이미지 이름이
판을 안 타는 고정 이름(`flightdeck:local` · `flightdeck-pg:local`)이라, 다른 판의 자리에서 지은 이미지가 남아 있으면 `up -d` 는
그것으로 뜬다.

**준비** - 한 번. `FD_UID` 인 사용자로 돌린다.

```bash
: '1. 볼륨 둘. 프로젝트 밖의 고정 이름이라 판마다 디렉토리가 바뀌어도 같은 원장이고, `down -v` 가 못 지운다. compose 가
   만들지 않는다 - 없으면 빈 원장을 새로 만드는 대신 `up` 이 선다. 이름은 4 의 배포 이름(FD_PG_INSTANCE)에서 나온다.'
docker volume create flightdeck-pgdata && docker volume create flightdeck-pgrun

: '2. 서버 마운트 밖의 자리(아래 ★): 선택 파일 · pgpass 와 덤프. 덤프 자리가 없으면 Linux 의 Docker 가 root 소유 0755 로
   만들어 첫 덤프부터 못 쓴다.'
sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck /var/backups/flightdeck

: '3. 접속 계정 둘의 pgpass. 주인 FD_UID, 권한 0600, 한 줄씩.'
(umask 077
 printf 'db:5432:flightdeck:fd_server:%s\n' "$(openssl rand -hex 24)" > /etc/flightdeck/server.pgpass
 printf 'db:5432:flightdeck:fd_backup:%s\n' "$(openssl rand -hex 24)" > /etc/flightdeck/backup.pgpass)

: '4. 고른 것을 선택 파일에 적는다. 비밀번호는 없고 자리 · 이름만 있다. 어느 원장인지(배포 이름 FD_PG_INSTANCE - db 컨테이너 ·
   두 볼륨의 이름, 바꾸면 DB 이름 FD_PG_DATABASE)는 이 파일에만 둔다. 꺼 둔 이름(.off)으로 쓴다 - 켜는 것은 아래 띄우기 ·
   업그레이드의 한 단계다.'
cat > /etc/flightdeck/compose.env.off <<'EOF'
COMPOSE_FILE=compose.yaml:compose.postgres.yaml
FD_PG_INSTANCE=flightdeck
FD_PG_SERVER_PGPASS_HOST=/etc/flightdeck/server.pgpass
FD_PG_BACKUP_PGPASS_HOST=/etc/flightdeck/backup.pgpass
FD_BACKUP_HOST=/var/backups/flightdeck
EOF

: '5. 셸 설정에 둔다. fdc 는 compose 를 부르는 머리, fdrepos 는 도는 서버의 저장소 마운트를 FD_REPOS…FD_REPOS4 로, fdwhere 는
   거기에 더해 도는 서버의 compose 프로젝트(p) · 자리(w)를 이 셸에 읽는다. 둘 다 끝 줄이 새 셸에서 다시 줄 값이다.'
fdc() {
  if [ -e ~/.flightdeck/compose.env ]; then echo '선택 파일의 옛 자리 ~/.flightdeck/compose.env 는 서버가 쓸 수 있는 /data 안이라 더는 읽지 않는다 - 내용을 확인한 뒤 /etc/flightdeck/compose.env 로 옮기고 다시 돌린다: sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck && mv -n ~/.flightdeck/compose.env /etc/flightdeck/compose.env (README 「Postgres 로 띄우기」)' >&2; false; elif [ -n "${FD_PG_INSTANCE+x}${FD_PG_DATABASE+x}" ]; then echo 'FD_PG_INSTANCE · FD_PG_DATABASE 는 셸에서 주지 않는다 - 어느 원장(볼륨 · DB)인지는 선택 파일 /etc/flightdeck/compose.env(꺼 두었으면 .off) 한 곳에 적는다. 셸 값은 선택 파일보다 먼저라 새 셸에서 빠지면 다른 원장으로 뜬다: 값을 선택 파일에 적고 unset FD_PG_INSTANCE FD_PG_DATABASE 뒤 다시 돌린다 (README 「Postgres 로 띄우기」)' >&2; false; fi || return
  FD_TOKEN="${FD_TOKEN:-$(cat ~/.flightdeck/token 2>/dev/null || sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' ~/.flightdeck/config.json 2>/dev/null)}" \
  FD_UID="${FD_UID:-$(id -u)}" FD_GID="${FD_GID:-$(id -g)}" \
  COMPOSE_ENV_FILES="${FDC_ENV:-$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)}" docker compose "$@"
}
fdrepos() {
  unset FD_REPOS FD_REPOS2 FD_REPOS3 FD_REPOS4
  mounts=$(docker inspect flightdeck --format '{{range .Mounts}}{{if eq .Source .Destination}}{{println .Source}}{{end}}{{end}}') && nmounts=$(printf '%s\n' "$mounts" | grep -c .) && if [ "$nmounts" -gt 4 ]; then echo "도는 서버의 저장소 마운트가 $nmounts 개다 - 슬롯(FD_REPOS…FD_REPOS4)은 넷이라 다섯째부터 빠진다: 멈추고 묻는다" >&2; false; fi &&
    export FD_REPOS="$(printf '%s\n' "$mounts" | sed -n 1p)" FD_REPOS2="$(printf '%s\n' "$mounts" | sed -n 2p)" FD_REPOS3="$(printf '%s\n' "$mounts" | sed -n 3p)" FD_REPOS4="$(printf '%s\n' "$mounts" | sed -n 4p)" &&
    echo "저장소 마운트(새 셸에서 다시 줄 값): export FD_REPOS='$FD_REPOS' FD_REPOS2='$FD_REPOS2' FD_REPOS3='$FD_REPOS3' FD_REPOS4='$FD_REPOS4'"
}
fdwhere() {
  unset p w FD_REPOS FD_REPOS2 FD_REPOS3 FD_REPOS4
  fdp=$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project"}}') && fdw=$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}') && [ -n "$fdp" ] && [ -d "$fdw" ] &&
    fdrepos && p=$fdp w=$fdw && echo "도는 서버(새 셸에서 다시 줄 값): p='$p' w='$w'"
}
```

- `fdc` 는 선택 파일이 켜져 있으면(`/etc/flightdeck/compose.env`) 그것을 compose 의 env 파일로 준다(`COMPOSE_ENV_FILES`,
  `--env-file` 과 같다). `fd setup` 의 기동 · `fd-update` 스킬과 같은 판정이다. `FDC_ENV` 를 주면 그 파일을 대신 준다(업그레이드가
  켜기 전에 꺼 둔 파일로 돌린다). `COMPOSE_FILE` 이 덮개 파일을 얹는다 - 경로는 compose 를 돌리는 디렉토리 기준이라
  저장소(또는 플러그인 설치본)의 루트에서 돌린다. 토큰 · 사용자 id 는 `fd-update` 와 같게 채우고, 저장소 마운트
  (`FD_REPOS` …)는 지금처럼 셸에서 준다(`fdrepos`). 셸 값이 선택 파일보다 먼저고, 선택 파일을 주면 그 디렉토리의 `.env` 는
  안 읽는다.
- `fdc` 의 첫 줄은 `fd setup` 의 기동 · `fd-update` 와 같은 관문이다(글자까지 같다). 둘 가운데 하나면 처방을 내고 compose 를
  부르지 않는다: 선택 파일의 옛 자리에 파일이 남아 있다(아래 ★), 또는 셸이 `FD_PG_INSTANCE` · `FD_PG_DATABASE` 를 준다(빈
  값도). 그 둘은 어느 원장(볼륨 · DB)인지를 정하는데 셸 값이 선택 파일보다 먼저라, 한 셸에서 준 값으로 옮기고 그 값이 빠진 새
  셸에서 이어 띄우면 다른 볼륨(빈 원장)을 고른다 - 그래서 선택 파일 한 곳에만 둔다(준비 4). 관문 뒤는 `&&` 가 아니라
  `|| return` 이고 몸의 끝은 compose 한 줄이다 - zsh 는 몸이 `if … fi && …` 로 끝난 함수가 실패해도 그 함수를 부른 `set -e`
  덩이를 세우지 않는다(bash 는 세운다).
- `fdrepos` · `fdwhere` 는 서버가 돌 때만 읽는다. 시작에서 이 셸의 값(`FD_REPOS` … · `p` · `w`)부터 지우고, 못 읽으면(서버가
  내려갔다) 빈 채로 실패한다 - 앞서 다른 서버 · 다른 때에 읽은 값을 다음 덩이가 이어받지 않는다. `fdwhere` 는 조회(프로젝트 ·
  자리 · 마운트)가 **모두** 성공한 뒤에야 세 값을 한꺼번에 둔다 - 하나라도 못 읽으면 셋 다 빈 채다. 서버를 내린 뒤에는 끝 줄이 낸
  값을 다시 준다.
- 마운트는 docker 가 낸 출력 **한 줄을 경로 하나로** 읽어 차례대로 슬롯에 넣는다 - 공백이 든 경로(`/repos/team one`)도 한
  경로다. 공백으로 가르는 `set -- $(…)` 는 그 경로를 둘로 갈라 없는 경로 둘을 붙인다. 줄이 넷을 넘으면(슬롯은 넷이다) 다섯째부터
  조용히 빠지므로 읽기가 선다. `fd-update` 의 갱신 사슬이 이 두 함수의 조회 줄을 글자 그대로 쓴다(시험이 맞댄다).
- 비밀번호는 공백 · 역빗금 · 콜론이 없는 인쇄 가능한 ASCII 여야 한다. 같은 파일을 서버(pgx)와 매일 덤프(libpq)가 읽는데
  두 구현이 그 밖의 글자를 다르게 푼다. `db-roles` 가 어긋난 줄을 줄 번호와 함께 거절한다(비밀번호는 안 낸다).
- DB · 계정 이름을 바꾸면(`FD_PG_DATABASE` · `FD_PG_SERVER_USER` · `FD_PG_BACKUP_USER`, 선택 파일에) pgpass 의 줄도 같이
  바꾼다.

★ **선택 파일 · pgpass · 덤프를 서버 마운트 밖에 둔다.** 서버는 같은 uid 로 `FD_REPOS`(기본 홈 전체)를 읽기 전용으로,
`~/.flightdeck` 를 읽고 쓰게 붙인다. 홈 안에 두면 `0600` 이어도 서버가 덤프 계정의 비밀번호와 원장 전체의 덤프를 읽고,
`~/.flightdeck` 안의 선택 파일은 서버가 고쳐 쓸 수 있다 - 다음 갱신(사람이, 때로 root 로 돌린다)이 그 덮개 · 시크릿 자리를
믿는다. 그래서 compose 는 pgpass · 덤프 자리에 기본값을 두지 않는다(비면 `up` 이 그 변수 이름을 대고 선다). `FD_REPOS` 를
`/` · `/etc` · `/var` 로 넓히지 않는다. 선택 파일의 옛 자리 `~/.flightdeck/compose.env` 에 파일이 남아 있으면 `fd setup` 의
기동 · `fd-update` · `fdc` 가 옮기는 명령을 내고 멈춘다.

**띄우기** - 빈 원장으로 새로 시작할 때. 선택 파일을 켜고 띄운다.

```bash
mv -n /etc/flightdeck/compose.env.off /etc/flightdeck/compose.env && [ ! -e /etc/flightdeck/compose.env.off ] &&
  fdc up -d --build && fdc ps -a
curl -s localhost:7420/healthz
```

`ps -a` 에서 `db` · `flightdeck` · `db-backup` 은 healthy, `migrate` · `db-roles` 는 `Exited (0)` 이고, `/healthz` 의 `db_ok` ·
`db_locale_ok` 가 true 면 된 것이다. 빈 원장이면 `migrate` 가 기준선부터 깐다. **SQLite 원장을 들고 옮기려면** 이렇게 띄우지
말고 아래 「업그레이드: SQLite 원장을 Postgres 로」를 따른다 - `~/.flightdeck/fd.db` 가 이관 표지 없이 남아 있으면 이렇게 띄운
`migrate` 가 거절한다(이관을 거치지 않은 새 원장).

**관리 명령**은 `migrate` 서비스로 한다. 원장(`/data`) · 덤프 자리(`/backups`) · psql · pg_dump · pg_restore 가 그 안에 있고,
초사용자로 소켓에 붙는다. `--no-deps` 는 `db` 가 떠 있을 때 쓴다. 판 · 증분 계획은 `fdc run --rm --no-deps migrate selfcheck`
다. 아래 셋도 셸 설정에 둔다: `sql` 은 초사용자 psql, `srvsql` 은 서버 계정으로 서버의 원장(서버의 `FD_DB` 그대로)에 붙는
psql(SQL 은 표준 입력), `nodump` 는 `db-backup` 컨테이너가 하나도 안 도는지 잰다(`docker ps` 가 실패하면 「안 돈다」로 읽지
않고 선다).

```bash
sql() { fdc run --rm --no-deps --entrypoint psql migrate -X -v ON_ERROR_STOP=1 "$@"; }
srvsql() { fdc run --rm --no-deps -T --entrypoint sh flightdeck -c 'exec psql -X -v ON_ERROR_STOP=1 -d "$FD_DB" "$@"' sh "$@"; }
nodump() { if ! dumps=$(docker ps --filter label=com.docker.compose.service=db-backup --format '{{.Names}}(once={{.Label "com.docker.compose.oneoff"}})'); then echo 'docker ps 가 실패했다 - db-backup 컨테이너가 도는지 모른다: Docker 를 확인하고 다시 잰다' >&2; return 1; fi; if [ -n "$dumps" ]; then echo "db-backup 컨테이너가 돈다: $dumps - once=True 는 손으로 돌린 덤프다(끝나기를 기다렸다가 다시 잰다). once=False 는 서비스 컨테이너다(그 프로젝트에서 stop db-backup 한 뒤 다시 잰다)" >&2; return 1; fi; }
```

- **손으로 `fd migrate` · 역이관 · 되살리기를 돌리기 전에는 매일 덤프를 멈추고 손으로 돌린 덤프까지 끝났는지 본다**:
  `fdc stop db-backup && nodump`. pg_dump 는 끝날 때까지 모든 표의 잠금을 쥐고, 판을 올리는 DDL 은 그 잠금을
  lock_timeout(5초) 안에 못 얻으면 실패한다. `fdc up` 이 migrate 를 다시 만들 때(새 이미지 · 설정)는 compose 가 매일 덤프를
  먼저 멈췄다가 migrate 가 성공한 뒤 다시 띄운다(`depends_on` 의 `restart: true`). `run` 과 `up --no-deps migrate` 에는 그것이
  없고, `stop` 도 `restart: true` 도 서비스의 컨테이너만 멈춘다 - `fdc run --rm db-backup once` 로 손으로 돌린 덤프는 그대로
  돈다(`down` 도 남긴다). `nodump` 가 서면 그 줄이 대는 컨테이너를 본다: 손으로 돌린 덤프는 끝나기를 기다리고, 서비스
  컨테이너면 그 프로젝트(다른 판의 자리일 수 있다)에서 멈춘다.
- **비밀번호를 바꾸려면** 두 pgpass 파일을 **제자리에서** 고치고(새 파일로 바꿔 끼우면 붙어 있는 컨테이너에서 파일이
  사라진다) `fdc up -d --build --force-recreate --no-deps db-roles flightdeck db-backup` 한다. 서버는 기동 때 한 번 읽는다.

★ **원장 하나에 덮개 하나.** 이관은 커밋한 뒤 SQLite 원장 옆에 이관 표지(`~/.flightdeck/fd.db.moved-to-postgres`)를 남긴다.
옮긴 뒤 선택 파일 없이 기본 compose 로 띄우면 SQLite 쪽 `migrate` 가 그 표지를 보고 원장을 열지도 새로 만들지도 않아 서버가
안 뜬다 - `docker compose down` 뒤 선택 파일이 켜져 있는지 보고 `fdc up -d --build` 로 다시 띄운다. 거꾸로 덮개를 얹은 `migrate` 는
표지 없는 `fd.db` 가 남아 있으면 거절한다. 표지는 손으로 두 절차만 지운다: 아래 「켜기 전에 섰을 때」(양쪽 서버를 내린 채
맞댄 뒤)와 「72시간 안에 되돌리기」.

### 업그레이드: SQLite 원장을 Postgres 로

SQLite 판에서 쌓은 원장을 들고 Postgres 원장으로 넘어가는 절차다. 옮기는 동안 서버는 내려가 있다. 옮긴 뒤 72시간은
Postgres 에 쌓인 것까지 들고 SQLite 로 되돌릴 수 있다(아래 「72시간 안에 되돌리기」).

**원장이 이관 판 범위(18-19)여야 한다.** 이관은 표 · 칸이 마지막 SQLite 판(19)과 같은 판만 받는다 - 19 는 18 에 인덱스만
더했다. 더 옛 판이면 이 판의 기본 경로로 먼저 판을 올린다 - 아래 3 이 그것이다(3 은 늘 19 로 올린다).

**0 은 준비, 1 은 이 셸에 값을 읽고, 2-8 은 덩이 하나다.** 0 은 「Postgres 로 띄우기」의 준비 1-5(볼륨 · 자리 · pgpass · 꺼 둔
선택 파일 · 셸 함수)다 - 켜지도 띄우지도 않는다. 1 은 도는 서버의 compose 프로젝트 · 자리와 저장소 마운트를 이 셸에 읽는다.
서버를 내리면 못 읽으므로 1 의 끝 두 줄이 새 셸에서 다시 줄 값이다(아래 「다시 돌 때」). 덩이는 `set -eu` 를 건 부분 셸이라
어느 명령이든 실패하면 그 자리에서 덩이째 서고 뒤 단계는 돌지 않는다 - 옛 서버를 못 내렸는데 옮기거나, 계정 검사가 섰는데
선택 파일을 켜는 일이 없다. 마지막으로 나온 「n 끝」 줄이 선 자리다. 사유를 고친 뒤 같은 덩이를 다시 붙여 넣으면 된 단계를
건너뛴다(표지가 남았으면 2-5, 선택 파일이 켜졌으면 2-6). **저장소(또는 이 판의 플러그인 설치본) 루트에서 돌린다.**

```bash
fdwhere
```

```bash
( set -eu
  on=/etc/flightdeck/compose.env; off=$on.off; mark=$HOME/.flightdeck/fd.db.moved-to-postgres
  : "${FD_REPOS:?1 을 먼저 돌린다(새 셸이면 1 의 끝 두 줄을 다시 준다)}"
  unset FDC_ENV
  if [ -e "$on" ] && [ ! -e "$mark" ]; then
    echo "선택 파일($on)이 켜져 있는데 이관 표지가 없다 - 옮기기 전이면 꺼 둔 이름($off)으로 돌리고 다시 돈다" >&2; exit 1
  fi
  if [ ! -e "$on" ]; then
    if [ ! -e "$mark" ]; then
      : "${p:?1 을 먼저 돌린다}" "${w:?1 을 먼저 돌린다}"
      : '2. 옛 판의 자리에서 내리고, 그 프로젝트의 컨테이너가 하나도 안 남았는지(손으로 돌린 run 컨테이너도), 어느 프로젝트에도
         서버 컨테이너(flightdeck)가 없는지 본다.'
      (cd "$w" && fdc -p "$p" down)
      left=$(docker ps -aq --filter label=com.docker.compose.project="$p")
      if [ -n "$left" ]; then echo "옛 판의 프로젝트($p)에 컨테이너가 남았다 - 그 자리에서 내린 뒤 다시 돈다" >&2; exit 1; fi
      names=$(docker ps -a --format '{{.Names}}')
      if printf '%s\n' "$names" | grep -qx flightdeck; then echo '서버 컨테이너(flightdeck)가 남았다 - 그 프로젝트에서 내린 뒤 다시 돈다' >&2; exit 1; fi
      echo "2 끝: 내렸다"
      : '3. 이 판의 이미지를 짓고 SQLite 원장을 이 판으로 올린다(이미 19 면 아무것도 안 한다). selfcheck 가 ok 를 내야 한다.'
      fdc build
      fdc run --rm --no-deps migrate migrate --db /data/fd.db
      fdc run --rm --no-deps migrate selfcheck --db /data/fd.db
      echo "3 끝: SQLite 원장이 이 판이다"
      : '4. 꺼 둔 선택 파일로 Postgres 겹의 이미지를 짓고 이관 판까지만 깐다(기준선과 그 위 증분 - 판 19 에서 멈춘다). db 가 같이 뜬다.'
      export FDC_ENV="$off"; [ -f "$FDC_ENV" ]
      fdc build
      fdc run --rm migrate migrate --to 19
      echo "4 끝: 이관 판까지 깔았다"
      : '5. 예행 → 옮기기. 옮기기가 커밋한 뒤 원본 옆에 이관 표지(fd.db.moved-to-postgres)를 남긴다. 표지가 없으면 선다.'
      if ! fdc run --rm --no-deps migrate migrate import-sqlite --from /data/fd.db --dry-run; then
        echo '5 의 예행이 섰다 - 「대상이 비어 있지 않다」면 앞 실행이 커밋했을 수 있다(표지 없이 끊겼다): 다시 옮기지 말고 「켜기 전에 섰을 때」의 가 로 잰다' >&2; exit 1
      fi
      if ! fdc run --rm --no-deps migrate migrate import-sqlite --from /data/fd.db; then
        echo "5 의 옮기기가 섰다 - 위 줄이 「커밋했다」고 했으면 이 기계에서 표지부터 남기고 덩이를 다시 붙여 넣는다: touch '$mark' (위 줄이 대는 /data/… 는 컨테이너 안 자리다 - 이 기계의 ~/.flightdeck 다). 그 줄 없이 끊겼으면 다시 붙여 넣기 전에 「켜기 전에 섰을 때」의 가 로 잰다. 그 밖이면 사유를 고치고 다시 붙여 넣는다" >&2; exit 1
      fi
      [ -e "$mark" ]
      echo "5 끝: 옮겼다"
    fi
    : '6. 꺼 둔 선택 파일로 접속 계정을 만들고 재고(db-roles), 서버 계정으로 서버의 원장에 붙어 본다 - 옮긴 표가 읽히고
       TEMPORARY 권한이 없어야 한다.'
    export FDC_ENV="$off"; [ -f "$FDC_ENV" ]
    fdc up -d --build db-roles
    [ "$(docker wait "$(fdc ps -aq db-roles)")" = 0 ]
    srvsql <<'SQL'
SELECT current_user AS 계정, current_database() AS 원장, (SELECT count(*) FROM item) AS 항목, (SELECT count(*) FROM judgment) AS 판단;
SELECT 1 / (NOT has_database_privilege(current_database(), 'TEMPORARY'))::int AS temp_권한_없음;
SQL
    echo "6 끝: 계정이 맞다"
  fi
  : '7. 선택 파일이 고르는 Postgres 원장을 SQLite 원장과 맞댄 뒤에만 켜고 띄운다 - 새 셸에서 이어도 띄우기 전에 다시 맞댄다.
     만들기만 하고 돌지 못한 서버 컨테이너(8 의 up 이 서버를 띄우기 전에 섰다)는 원장에 쓴 적이 없으니 맞댄다. 이 자리의 서버가
     이미 돌았으면(8 이 띄운 뒤 섰다) 서버가 쓴 원장이라 SQLite 와 맞대지 않고, 그 서버와 db 를 만든 설정(compose 의 config-hash
     라벨)이 지금 선택 파일 · 저장소 마운트로 렌더한 설정과 같은지 본다 - 같아야 8 의 up 이 그 둘을 다시 만들지 않고 같은 볼륨 ·
     같은 DB 로 띄운다. 켠 선택 파일은 이 뒤의 갱신 · 재기동이 Postgres 로 띄운다.'
  srv=$(fdc ps -aq flightdeck); st=
  if [ -n "$srv" ]; then st=$(docker inspect -f '{{.State.Status}}' "$srv"); fi
  if [ -z "$srv" ] || [ "$st" = created ]; then
    fdc run --rm migrate migrate verify --from /data/fd.db
  else
    for s in db flightdeck; do
      c=$(fdc ps -aq "$s")
      if [ -z "$c" ]; then echo "7: 서버가 돌았는데 이 자리에 $s 컨테이너가 없다 - 서버가 쓴 원장을 재지 못한다: 멈추고 묻는다" >&2; exit 1; fi
      l=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.config-hash"}}' "$c"); h=$(fdc config --hash "$s")
      if [ "$h" != "$s $l" ]; then echo "7: 이 자리에서 돈 $s 컨테이너를 만든 설정이 지금 선택 파일 · 저장소 마운트 · 토큰으로 렌더한 설정과 다르다 - 8 이 다른 원장(볼륨 · DB)이나 다른 마운트로 다시 만들 수 있어 띄우지 않는다: 그 서버를 띄운 때의 선택 파일과 1 의 끝 두 줄을 그대로 주고 다시 붙여 넣는다(무엇이 다른지는 docker inspect $c 와 fdc config $s 로 본다)" >&2; exit 1; fi
    done
    echo "7: 이 자리의 서버가 이미 돌았다 - 서버가 쓴 원장이라 맞대지 않고, 그 서버 · db 를 만든 설정이 지금과 같다"
  fi
  if [ ! -e "$on" ]; then mv -n "$off" "$on"; [ -e "$on" ]; [ ! -e "$off" ]; fi
  unset FDC_ENV
  echo "7 끝: 선택 파일이 켜졌다"
  : '8. Postgres 원장으로 띄운다. 보드에 옮긴 항목이 보이면 된 것이다.'
  fdc up -d --build
  fdc ps -a
  echo "8 끝: Postgres 원장으로 띄웠다"
)
curl -s localhost:7420/healthz
```

`/healthz` 의 `db_ok` · `db_locale_ok` 가 true 여야 한다.

- **NUL 은 `\x00` 로 옮겨진다.** SQLite 는 글 칸에 NUL 바이트(0x00)를 담지만 Postgres 는 못 담는다. 5 의 예행 · 옮기기와
  7 의 맞대기는 그런 칸을 거절하지 않고 `\x00` 네 글자로 바꿔 옮기며, 바꾼 칸을 「옮기며 바꾼 원본 칸」에 표 · 칸 · 규칙 ·
  행 좌표로 적고 판정 줄 아래에 바꾼 행 수를 한 번 더 낸다. 바꾼 칸 전부는 원래 바이트와 함께
  `~/.flightdeck/fd.db.rewritten-<시각>.jsonl` 에 남는다 - 그 칸의 원래 바이트는 이 파일이 정본이니 지우지 않는다. 손으로
  고칠 단계는 없다. SQLite 원장 파일은 그대로다. 규칙과 되돌리는 법은 아래 「SQLite 원장을 Postgres 로 옮기기」.
- **5 의 표지가 남기 전에** 서면(2-5 의 거절 · 실패) 아무것도 커밋되지 않았다(이관은 트랜잭션 하나) - **둘만 빼고**. 5 의
  옮기기가 「옮겼다(N 행, 커밋했다) - 그런데 원본 옆에 이관 표지를 남기지 못했다」로 섰으면 커밋은 됐다. 5 의 옮기기가 **결과
  없이** 끊겼으면(오류 줄도 「5 끝」도 없다 - 커밋 직후 프로세스 · 셸 · 기계가 죽었다) 커밋됐는지 모른다. 표지가 없다고 커밋
  전이 아니다. 어느 쪽이든 SQLite 원장은 읽기만 했다. 커밋했다고 했으면 **이 기계에서** `touch ~/.flightdeck/fd.db.moved-to-postgres`
  로 표지부터 남기고 덩이를 다시 붙여 넣는다 - 옮기기 줄이 대는 `touch /data/…` 는 컨테이너 안 자리라(`/data` 가 이 기계의
  `~/.flightdeck` 다) 이 기계에서는 없는 자리다. 덩이도 설 때 이 기계의 자리로 그 명령을 다시 댄다. 결과를 모르면 다시 붙여
  넣기 전에 「켜기 전에 섰을 때」의 가 로 Postgres 원장의 행을 잰다 - 잊고 다시 붙여
  넣어도 두 번 옮기지는 않는다: 이관은 빈 원장만 받아 5 의 예행이 「대상이 비어 있지 않다」로 서고, 덩이가 가 를 댄다. 그
  밖의 사유면 고치고 덩이를 다시 붙여 넣는다. SQLite 판으로 돌아가려면 「켜기 전에 섰을 때」로 잰 뒤 고른다.
- **5 의 표지가 남은 뒤, 7 이 선택 파일을 켜기 전에** 서면(6 · 7) 원장은 이미 Postgres 에 있고 서버는 아직 거기 쓰지 않았다.
  사유를 고치고 덩이를 다시 붙여 넣는다 - 2-5 를 건너뛰고 6 부터 돈다. SQLite 판으로 돌아가려면 「켜기 전에 섰을 때」.
- **7 이 선택 파일을 켠 뒤에** 서면(8) 덩이를 다시 붙여 넣으면 7 의 맞대기와 8 이 돈다. 이 자리의 서버 컨테이너가 있다는 것만으로
  맞대기를 건너뛰지 않는다: 만들기만 하고 돌지 못했으면(8 의 up 이 db · migrate 에서 섰다) 맞대고, 이미 돌았으면 SQLite 원장 대신
  그 서버 · db 를 만든 설정(compose 의 config-hash 라벨)을 지금 렌더한 설정(`fdc config --hash`)과 맞대 같을 때만 8 을 돈다 -
  그 사이 선택 파일이 다른 배포 이름 · DB 를 가리키면 8 의 up 이 다른 볼륨 · DB 로 다시 만들기 때문이다.
  SQLite 판으로 돌아가려면 「72시간 안에 되돌리기」. 「8 끝」 뒤에는 이 덩이를 다시 쓰지 않는다 - 서버를 `fdc down` 으로 지운
  뒤면 서버가 쓴 원장이 옮긴 SQLite 원장과 어긋나 7 이 선다. 다시 띄우기는 `fdc up -d --build` 다.
- **어느 원장인지는 선택 파일에만 있다.** 배포 이름(`FD_PG_INSTANCE` - db 컨테이너 · 두 볼륨의 이름)과 DB(`FD_PG_DATABASE`)는
  준비 4 의 선택 파일에 적고 셸에서 주지 않는다 - 셸이 주면 `fdc` 의 관문이 compose 를 부르기 전에 선다. 셸 값은 선택 파일보다
  먼저라, 한 셸에서 준 값으로 4-6 을 돌고 그 값이 빠진 새 셸에서 8 을 이으면 다른 볼륨(빈 원장)을 고른다. 7 은 새 셸에서
  이어도 띄우기 전에 그 파일이 고르는 원장을 SQLite 원장과 다시 맞댄다.
- **다시 돌 때(새 셸).** `fdc` · `fdrepos` · `fdwhere` · `srvsql` 은 셸 설정에서 온다. `FDC_ENV` 는 주지 않는다 - 덩이가 선택
  파일의 상태로 정한다(켜기 전이면 꺼 둔 파일, 켠 뒤면 켠 파일). 서버를 내린 뒤에는 1 이 못 읽으므로 1 이 낸 끝 두 줄을 그대로
  다시 준다: `export FD_REPOS=… FD_REPOS2=… FD_REPOS3=… FD_REPOS4=…` 와 `p='…' w='…'`(표지가 남은 뒤면 저장소 마운트만 있으면
  된다). 1 을 다시 돌리면 못 읽고 그 값들을 비운다 - 끝 두 줄을 준다. 그 뒤 2-8 덩이를 붙여 넣는다. 저장소 마운트가 없으면
  덩이가 선다 - 8 이 그 마운트로 서버를 띄우므로 1 이 읽은 값 그대로 준다.
- 옮긴 뒤 SQLite 원장(`fd.db` 와 옆 파일)은 그 자리에 두어도 된다 - 표지가 SQLite 쪽에서 그 원장을 막는다. 치우려면
  (선택) 서버를 내린 채 `fd.db` · `fd.db-wal` · `fd.db-shm` 을 같은 디렉토리의 다른 이름으로 옮기고 읽기 전용으로 둔다. **표지는
  옮기지 않는다** - 표지가 없으면 그 자리에 새 SQLite 원장이 생길 수 있다.
- **사본을 옮겼으면**(`--from` 에 원장 자리 밖의 `VACUUM INTO` 사본) 이관 표지는 사본 옆에 남는다. **원장 자리에도 표지를 꼭
  남긴다 - 원본을 원장 자리 밖으로 옮기더라도.** 원장 자리에 표지가 없으면 덮개를 얹은 `migrate` 는 거기 남은 `fd.db` 를 이관
  안 된 원장으로 보고 거절하고, 원본을 치웠으면 덮개 없이 띄운 `migrate` 가 그 자리에 빈 SQLite 원장을 새로 만든다. 원본이
  옮긴 것과 같은지 맞댄 뒤에 남기고, 원본을 치우는 것(선택)은 그 뒤다:
  `( set -eu; unset FDC_ENV; [ -e /etc/flightdeck/compose.env ] || export FDC_ENV=/etc/flightdeck/compose.env.off; fdc run --rm migrate migrate verify --from /data/fd.db; touch ~/.flightdeck/fd.db.moved-to-postgres )`
- **두 원장에 이미 따로 쓰였으면**(옮긴 뒤 SQLite 판 서버가 옛 원장에 썼다 등) 두 서버를 다 내리고 **어느 원장도 지우지
  않는다.** `verify --from` 으로 어긋난 행을 기본키로 보고 어느 쪽을 살릴지 사람이 정한다. 합치는 명령은 없다.
- 거절 갈래와 대조의 자세한 모양은 아래 「SQLite 원장을 Postgres 로 옮기기」에 있다.

**예행은 운영과 겹치지 않게 한다.** 서버 컨테이너 이름(`flightdeck`)과 포트 7420 이 고정이라 같은 Docker 에서는 운영 서버와
같이 못 뜨고, 운영과 같은 볼륨 이름을 쓰면 예행의 `migrate` 가 운영 원장에 초사용자로 붙는다 - **다른 Docker 환경**(다른 기계 ·
VM)에서 한다. 거기서도 운영과 다른 값을 준다: 프로젝트 이름(`fdc -p <예행>`), 홈(`HOME` - `/data` 가 `~/.flightdeck` 다), 그리고
그 환경의 선택 파일에 적는 배포 이름 `FD_PG_INSTANCE`(db 컨테이너 · 두 볼륨의 이름)와 `FD_BACKUP_HOST`(셸에서 주면 `fdc` 가
선다). 볼륨은 `docker volume create <배포 이름>-pgdata && docker volume create <배포 이름>-pgrun` 으로 만들고, 끝나면
`fdc -p <예행> down` 뒤 `docker volume rm <배포 이름>-pgdata <배포 이름>-pgrun` 으로 비운다. ★ **`flightdeck-pgdata` ·
`flightdeck-pgrun` 은 지우지 않는다** - 운영 원장이다. 지우기 전에 `docker volume ls` 로 이름을 한 번 더 본다.

#### 켜기 전에 섰을 때

업그레이드가 7 에서 선택 파일을 켜기 전에 섰고, 이어 갈지 SQLite 판으로 돌아갈지 정할 때다(2 가 옛 서버를 내리기 전에 섰으면
바뀐 것이 없다 - 이 절이 필요 없다). 선택 파일이 꺼져 있는 동안 서버는 Postgres 겹으로 뜨지 않으므로 서버가 Postgres 원장에 쓴
적은 없다. **표지만 보고 고르지 않는다** - 이관은 커밋한 **뒤에** 표지를 쓰므로 표지가 없어도 커밋됐을 수 있고, 그때 SQLite
판으로 돌아가 쓰기 시작하면 두 원장이 갈라진다(다시 옮기기도 「비어 있지 않다」로 선다). 그래서 양쪽 서버를 내린 채 재고(가),
표를 보고 고른다. 업그레이드와 같은 자리(저장소 또는 이 판의 설치본 루트)에서 돌린다.

**「4 끝」 전에 섰으면**(나온 마지막 「n 끝」 줄이 「2 끝」 이나 「3 끝」 이다) 5 의 옮기기는 돌지 않았다 - Postgres 원장에 원장
행이 없으니 가 를 재지 않고 고른다(4 의 이미지 · 기준선이 없으면 가 는 「못 쟀다」로만 끝난다). 이어 가려면 사유를 고쳐 업그레이드
덩이를 다시 붙여 넣는다. 이 판의 SQLite 판으로 돌아가려면 나 를 돈다. **3 에서 섰으면**(「2 끝」 이 마지막이다 - 이 판의 이미지 ·
SQLite 판올림 · selfcheck 가 섰다) 이 판으로는 SQLite 판도 못 뜰 수 있다 - 옛 판으로 다시 띄운다: `(cd "$w" && fdc -p "$p" up -d --build)`
(1 이 읽은 `p` · `w` · 저장소 마운트로 - 새 셸이면 1 의 끝 두 줄을 다시 준다). `--build` 를 빼지 않는다 - 3 이 이 판으로 지은
이미지가 같은 고정 이름(`flightdeck:local`)을 차지해, 빼면 옛 자리에서도 이 판의 이미지로 뜬다. `--build` 가 옛 판의 소스로 다시 짓는다. 옛 판이 「DB 판이 그 판이 아는 것보다 높다」로
서면 3 의 판올림이 일부 얹혔다 - 사유를 고쳐 업그레이드 덩이를 다시 붙여 넣는다(3 이 이어 올린다).

```bash
( set -eu
  : '가. 잰다 - 아무것도 고치지 않는다. 선택 파일이 꺼져 있고 어느 쪽 서버도 안 도는지 보고, 표지를 읽고, Postgres 원장을
     SQLite 원장과 맞댄다. 대조표의 「대상 행」이 Postgres 원장의 행이다.'
  on=/etc/flightdeck/compose.env
  unset FDC_ENV
  if [ -e "$on" ]; then echo '선택 파일이 켜져 있다 - 이 갈래가 아니다(「72시간 안에 되돌리기」)' >&2; exit 1; fi
  export FDC_ENV="$on.off"; [ -f "$FDC_ENV" ]
  up=$(docker ps -q --filter label=com.docker.compose.service=flightdeck)
  if [ -n "$up" ]; then echo '서버가 돈다 - 그 프로젝트에서 내린 뒤 다시 잰다' >&2; exit 1; fi
  if [ -e "$HOME/.flightdeck/fd.db.moved-to-postgres" ]; then echo '가: 이관 표지가 있다'; else echo '가: 이관 표지가 없다'; fi
  if fdc run --rm migrate migrate verify --from /data/fd.db; then echo '가 끝: 맞다'; else echo '가 끝: 맞지 않거나 못 쟀다 - 위 출력을 본다'; fi
)
```

| 가 의 결과 | 뜻 | 고른다 |
|---|---|---|
| 표지가 있다 · 맞다 | 옮겼고, 그 뒤 어느 쪽에도 안 썼다 | 이어 가려면 업그레이드의 2-8 덩이를 다시 붙여 넣는다(6 부터 돈다). 돌아가려면 나 |
| 표지가 없다 · 맞다 | 옮겼는데(커밋) 표지만 못 남겼다 | 이어 가려면 `touch ~/.flightdeck/fd.db.moved-to-postgres` 뒤 덩이를 다시 붙여 넣는다. 돌아가려면 나 |
| 표지가 없다 · 어긋남이고 대조표의 대상 행이 모두 0 | 커밋 전에 섰다 - Postgres 원장은 기준선뿐이다 | 사유를 고쳐 덩이를 다시 붙여 넣거나, 돌아가려면 나 |
| 표지가 없다 · 대조표 없이 「대상이 빈 DB 다」 | 기준선 전에 섰다 - Postgres 원장이 비었다 | 사유를 고쳐 덩이를 다시 붙여 넣거나, 돌아가려면 나 |
| 그 밖에 대조표가 없다 | 못 쟀다(접속 · 이미지 · 판 · 옆 파일) | 위 사유를 고치고 가 를 다시 돈다. 업그레이드가 「4 끝」 전에 섰으면 가 를 재지 않고 위대로 고른다 |
| 그 밖(표지가 있는데 어긋난다 · 대상에 행이 있는데 어긋난다) | 어느 쪽엔가 따로 쓰였다 | **멈춘다.** 어느 원장도 지우지 않는다. 대조표의 어긋난 행(기본키)을 보고 사람이 정한다 |

**옛 판(18)으로 옮기다 섰어도 이 판으로 이어 간다.** 대조와 이관 · 역이관은 이관 판 범위(18-19)의 원장을 받는다 - 표 · 칸이
같은 판이다. 이어 가는 덩이의 6 이 Postgres 원장을 19 로 올려도, 7 은 판 18 그대로인 SQLite 원장과 그 원장을 맞댄다(SQLite
원장은 표지가 막아 올리지 않는다).

나 는 가 의 표가 「돌아가려면 나」라 한 때와, 업그레이드가 「4 끝」 전에 섰을 때만 돈다. 이 자리의 Postgres 겹(`db` 와 one-shot)을
내리고, 표지를 지우고(서버가 쓴 적 없는 Postgres 원장이 SQLite 원장과 같거나 비었다), 이 판의 기본 경로로 띄운다. 선택 파일은
꺼진 채 둔다. 이관이 표지를 남겼으면 원장 옆 판올림 전 사본도 `fd.db.pre-import-<시각>.bak/` 로 옮겨 두었다 - 같은 원장으로
돌아가니 `fd migrate --rollback` 에 쓸 사본이 필요하면 그 디렉토리에서 원장 옆으로 손으로 옮겨 놓는다.

```bash
( set -eu
  : '나. SQLite 판으로 돌아간다.'
  on=/etc/flightdeck/compose.env
  unset FDC_ENV
  if [ -e "$on" ]; then echo '선택 파일이 켜져 있다 - 이 갈래가 아니다(「72시간 안에 되돌리기」)' >&2; exit 1; fi
  : "${FD_REPOS:?업그레이드의 1 이 낸 저장소 마운트 줄을 다시 준다}"
  export FDC_ENV="$on.off"; [ -f "$FDC_ENV" ]
  fdc down
  unset FDC_ENV
  rm -f "$HOME/.flightdeck/fd.db.moved-to-postgres"
  fdc up -d --build
  fdc ps -a
  echo "나 끝: SQLite 원장으로 띄웠다"
)
```

- Postgres 원장(볼륨)은 그대로 남는다. 옮긴 사본이 남았으면(표의 첫째 · 둘째 줄) 다시 옮길 때 이관이 그 원장을 「비어 있지
  않다」로 거절한다 - 이관은 빈 대상만 받는다.

#### 72시간 안에 되돌리기

옮긴 뒤 72시간을 되돌리기 창으로 둔다. `export-sqlite` 가 Postgres 원장(이관 판 범위 18-19)을 마지막 SQLite 판(19)의 **새**
SQLite 파일로 옮기므로, 옮긴 뒤 Postgres 에 쌓인 항목 · 선점 · 판단도 같이 돌아온다. 이 창 동안은 Postgres 스키마를 SQLite 의
마지막 판 위로 올리는 flightdeck 판으로 갱신하지 않는다 - `export-sqlite` 는 자기 이관 판 범위의 원장만 받는다. 판 18 에서
옮긴 뒤 창 안에 이 판으로 올렸으면 Postgres 원장도 19 로 오르고, 되돌리기는 이 판의 `export-sqlite` 로 한다(옛 판의 것은 19 를
거절한다). 그 판올림은 덤프를 뜨기 전에 인덱스 키 상한을 넘는 행을 찾고, 있으면 행 좌표를 대고 원장을 지금 판 그대로 둔 채
선다(아래 「SQLite 원장을 Postgres 로 옮기기」의 사전 검사와 같은 상한). 그 행은 고치지 않는다 - 추가 전용 감사 원장이라 자르면
SQLite 원본 · 덤프와 달라진다. 좌표를 실어 이슈로 알려 달라. **선택 파일을 켠
배포만** 이 길로 되돌린다 - 켜기 전에 선 업그레이드는 위 「켜기 전에 섰을 때」다. 업그레이드처럼 1 은 이 셸에 값을 읽고(`fdwhere` - 5 가 서버를 내린 뒤에는 못 읽으므로
끝 두 줄이 새 셸에서 다시 줄 값이다) 2-7 은 덩이 하나다(첫 실패에서 덩이째 선다). 덩이의 compose 명령은 모두 1 이 읽은 자리 ·
프로젝트로 돈다 - 다른 판의 디렉토리에서 돌린 `stop` · `down` 은 다른 프로젝트를 재고 아무것도 안 멈춘다.

**놓기 전에 매번 잰다.** 4 는 서버가 안 도는지 보고 낸 `fd-back.db` 를 **지금** Postgres 원장과 변환 없이(`--exported`) 맞댄다. 다시 붙여 넣어 5 뒤에서
이을 때도 건너뛰지 않는다(꺼 둔 선택 파일로 `db` 를 다시 띄워 잰다) - 파일이 있다는 것이 아니라 대조가 관문이다. 맞은 파일은
지금 원장과 같으니 낡지 않았다. 덩이를 처음 시작할 때(선택 파일이 켜져 있다) 앞 실행이 남긴 `fd-back.db` 가 있으면 시작하지
않는다 - 이번 실행이 낸 파일만 놓는다.

```bash
fdwhere
```

```bash
( set -eu
  on=/etc/flightdeck/compose.env; off=$on.off; d=$HOME/.flightdeck
  : "${p:?1 을 먼저 돌린다}" "${w:?1 을 먼저 돌린다}" "${FD_REPOS:?1 을 먼저 돌린다}"
  unset FDC_ENV
  cd "$w"
  if [ -e "$on" ]; then
    : '이번 실행이 낸 파일만 원장 자리에 둔다 - 앞 실행이 남긴 파일이 있으면 시작하지 않는다.'
    for s in '' -wal -shm -journal; do
      if [ -e "$d/fd-back.db$s" ]; then echo "앞 실행이 낸 $d/fd-back.db$s 가 남았다 - 옆 이름으로 옮긴 뒤 다시 돈다" >&2; exit 1; fi
    done
    : '2. 서버와 매일 덤프를 멈추고(db 는 둔다) 손으로 돌린 덤프까지 끝났는지 본다. 붙은 연결이 있으면 역이관이 거절한다.'
    fdc -p "$p" stop flightdeck db-backup
    nodump
    echo "2 끝: 멈췄다"
    : '3. 예행 → 옮기기.'
    fdc -p "$p" run --rm --no-deps migrate migrate export-sqlite --to /data/fd-back.db --dry-run
    fdc -p "$p" run --rm --no-deps migrate migrate export-sqlite --to /data/fd-back.db
    [ -f "$d/fd-back.db" ]
    echo "3 끝: 냈다"
  elif [ -f "$off" ] && [ -f "$d/fd-back.db" ]; then
    echo "5 뒤에서 잇는다: 선택 파일이 꺼졌고 $d/fd-back.db 가 남았다 - 놓기 전에 다시 잰다"
    export FDC_ENV="$off"
  else
    echo "선택 파일이 꺼져 있고 낸 원장($d/fd-back.db)도 없다 - 켜기 전에 선 업그레이드면 「켜기 전에 섰을 때」, 이 덩이가 6 뒤에 섰으면 7 만 다시 돈다: (cd \"$w\" && fdc -p \"$p\" up -d --build)" >&2
    exit 1
  fi
  : '4. 놓기 전에 매번 잰다: 서버가 안 돌고, 낸 파일이 지금 Postgres 원장과 맞는다. 맞아야 다음으로 간다.'
  up=$(docker ps -q --filter label=com.docker.compose.service=flightdeck)
  if [ -n "$up" ]; then
    echo '서버가 돈다 - 낸 파일을 맞댄 뒤에도 원장에 쓸 수 있다: 그 프로젝트에서 내린 뒤 다시 돈다' >&2; exit 1
  fi
  fdc -p "$p" run --rm migrate migrate verify --from /data/fd-back.db --exported
  echo "4 끝: 낸 파일이 지금 원장과 맞다"
  : '5. 선택 파일을 끄고 Postgres 겹을 내린다. 켜 두면 다음 갱신 · 재기동이 다시 Postgres 겹을 얹는다.'
  if [ -e "$on" ]; then mv -n "$on" "$off"; [ ! -e "$on" ]; fi
  export FDC_ENV="$off"
  fdc -p "$p" down
  unset FDC_ENV
  echo "5 끝: 선택 파일을 끄고 겹을 내렸다"
  : '6. 낸 파일을 원장 자리에 둔다. 그 자리의 원장(옮긴 SQLite 원장과 옆 파일)은 지우지 않고 옆 이름으로 옮긴다. 그 원장의
     판올림 전 사본(fd.db.bak-*)도 옆 디렉토리(fd.db.pre-back-<시각>.bak)로 옮긴다 - 낸 원장의 사본이 아니라 fd migrate --rollback
     이 고를 자리에 두지 않는다. 하나라도 못 옮기면 선다. 그 뒤에야 이관 표지를 지운다 - 남기면 SQLite 쪽 migrate 가 이 원장을
     열지 않는다.'
  cd "$d"; t=$(date -u +%Y%m%dT%H%M%SZ)
  for s in '' -wal -shm -journal; do if [ -e "fd.db$s" ]; then mv -n "fd.db$s" "fd.db.pre-back-$t$s"; fi; done
  for s in '' -wal -shm -journal; do [ ! -e "fd.db$s" ]; done
  old=$(find . -maxdepth 1 -name 'fd.db.bak-*')
  if [ -n "$old" ]; then
    mkdir "fd.db.pre-back-$t.bak"
    find . -maxdepth 1 -name 'fd.db.bak-*' -exec mv -n {} "fd.db.pre-back-$t.bak/" \;
    left=$(find . -maxdepth 1 -name 'fd.db.bak-*'); [ -z "$left" ]
  fi
  for s in '' -wal -shm -journal; do if [ -e "fd-back.db$s" ]; then mv -n "fd-back.db$s" "fd.db$s"; fi; done
  for s in '' -wal -shm -journal; do [ ! -e "fd-back.db$s" ]; done
  [ -f fd.db ]; rm -f fd.db.moved-to-postgres
  echo "6 끝: 낸 원장을 놓았다"
  : '7. 기본 경로(SQLite)로 띄운다.'
  cd "$w"
  fdc -p "$p" up -d --build
  fdc -p "$p" ps -a
  echo "7 끝: SQLite 원장으로 띄웠다"
)
```

- **4 끝 전에** 서면(멈추기 · 내기 · 맞대기) 바뀐 것은 서버와 매일 덤프가 멈춘 것뿐이고 선택 파일은 켜진 채다. 낸 `fd-back.db`
  가 남았으면 다시 붙여 넣은 덩이가 그것을 보고 선다 - 옆 이름으로 옮긴다(역이관은 있는 파일을 덮지 않고, 이 덩이는 이번 실행이
  낸 파일만 놓는다). 붙은 연결 · 도는 서버를 찾아 내리고 덩이를 다시 붙여 넣는다. 되돌리기를 그만두려면
  `(cd "$w" && fdc -p "$p" up -d --build)` 로 Postgres 원장 그대로 다시 띄운다.
- **5 가 선택 파일을 끈 뒤에** 서면(5 의 내리기 · 6 · 7) 다시 붙여 넣은 덩이는 놓기 전에 4 를 다시 돈다: 서버가 안 도는지 보고,
  꺼 둔 선택 파일로 `db` 를 다시 띄워 남은 `fd-back.db` 를 지금 Postgres 원장과 맞댄다. 그 사이 원장에 쓰였거나 다른 때의 파일이면
  4 가 서고 아무것도 놓지 않는다. 6 은 옛 원장을 옆 이름(`fd.db.pre-back-<시각>`)으로, 그 판올림 전 사본을 옆 디렉토리
  (`fd.db.pre-back-<시각>.bak/`)로 옮길 뿐 지우지 않는다. 새 셸이면 1 의 끝
  두 줄을 다시 준다(서버가 내려가 1 은 못 읽는다). 6 이 낸 파일을 놓은 뒤 7 에서 섰으면 덩이가 그렇게 대고 서니 7 만 다시 돈다.
  4 가 서서 되돌리기를 그만두려면 선택 파일을 다시 켜고(`mv -n /etc/flightdeck/compose.env.off /etc/flightdeck/compose.env`)
  `(cd "$w" && fdc -p "$p" up -d --build)` 로 Postgres 원장을 다시 띄운 뒤, 남은 `fd-back.db` 를 옆 이름으로 옮긴다.
- **서버 컨테이너가 없어** 1 이 못 읽으면(업그레이드의 8 이 서버를 만들기 전에 섰다) 업그레이드를 돌린 자리에서 `w=$PWD` 와 그
  자리의 프로젝트 `p=$(docker inspect "$(fdc ps -aq db)" -f '{{index .Config.Labels "com.docker.compose.project"}}')` 를 주고,
  저장소 마운트는 업그레이드의 1 이 낸 줄을 다시 준다.
- Postgres 원장(볼륨 `flightdeck-pgdata`)은 그대로 남는다. 다시 옮길 때 이관은 빈 대상만 받는다.

**SQLite 로 돌아가기는 판 18 이미지로 돌아가기가 아니다.** 7 은 이 판의 이미지로 띄우고, 놓은 원장은 판 19 다(역이관은 늘
마지막 SQLite 판으로 낸다). 판 18 의 flightdeck 은 그 원장을 「DB 판이 그 판이 아는 것보다 높다」로 거절한다. 판 18 이미지로
띄우려면 서버를 내린 채 **이 판의 이미지로** 판을 내린 뒤 옛 판의 자리에서 띄운다:

```bash
(cd "$w" && fdc -p "$p" run --rm --no-deps migrate migrate --db /data/fd.db --to 18)
```

`--to N`(N 이 지금 판보다 낮다)은 인덱스만 더한 증분만 내린다 - 그 인덱스를 지우고 판 행을 지울 뿐 기록은 그대로다(판 19 →
18 이 그렇다). 표 · 칸을 바꾼 증분 아래로는 거절하고 아무것도 안 바꾼다. 내리는 데에 `fd migrate --rollback` 을 쓰지 않는다 -
그것은 판올림 전 사본으로 원장을 **덮는** 명령이라 그 뒤의 기록을 잃는다. 되돌리기는 덮기 전에 사본을 잰다: 다 뜬 원장이 아니거나
(옆에 `-journal` 이 남은 사본, integrity_check 실패) 이 원장의 판올림 전 사본이 아니면(판 기록이 지금 원장과 다르다 - 역이관 ·
되살리기로 원장을 바꿔 놓은 뒤 남은 옛 사본) 거절한다. 판 기록이 지금 원장과 **같은** 사본도 거절한다 - 그 사본 뒤로 커밋된
판올림이 없다(판올림이 첫 단에서 되감겼거나 앞선 되돌리기가 이미 그 사본으로 덮었다). 덮으면 그 사본 뒤에 쓴 기록만 잃는다.
덮을 때는 지금 원장을 `fd.db.pre-rollback-<시각>`(옆 파일 포함)으로 남긴다. 판올림 전 사본도 남기는 원장도 있는 이름을 덮지
않는다 - 같은 초에 이미 있으면 `-001` 부터 번호를 붙인다(번호 붙은 이름이 사전순으로 뒤라 되돌리기는 그것을 가장 최근으로
고른다). 판올림이 서면 실패 안내가 그 원장을 셸 인용해 `fd migrate --rollback --db '<원장>'` 으로 낸다 -
`--db` 를 빼면 이 환경의 기본 원장을 되돌린다.

**원장 파일을 손으로 바꿔 놓을 때**(사본을 되살리거나, 다른 자리 · 기계의 원장을 복제해 놓을 때) 먼저 그 자리의
`fd.db.bak-*`(옆 파일 포함)를 새 디렉토리(`fd.db.pre-restore-<시각>.bak/` 처럼)로 옮긴다. 지우지 않는다. 같은 판에서 복제한
원장은 판 기록(판 · 적용 시각)까지 같아서, 판 기록 대조로는 남의 원장의 사본을 가르지 못한다 - 그 자리에 남기면 `fd migrate
--rollback` 이 그것으로 덮을 수 있다. 이관과 역이관은 스스로 옮긴다(아래 두 절).

### Postgres 원장의 매일 덤프

`db-backup` 서비스가 `pg_dump -Fc` 를 하루 한 번 `$FD_BACKUP_HOST/daily/<DB>/fd-daily-<UTC>.dump` 로 뜨고 새것 14개만
남긴다. Postgres 겹을 얹을 때만 뜬다(준비는 「Postgres 로 띄우기」). 회전은 덤프가 성공한 뒤에만 하고, 한 DB 자리에서는 한
실행만 돈다. 가장 새 덤프가 간격 + 확인 두 번 + 기한(기본 26시간 30분)보다 낡으면 컨테이너가 unhealthy 다.

설정은 `fdc run --rm --no-deps db-backup config` 가 한 줄로 낸다. 손으로 한 번 뜨려면 `once` 다(0 = 떴거나 아직 때가
아니다, 1 = 실패, 3 = 다른 실행이 같은 자리에서 돌고 있어 아무것도 안 했다).

**되살리기** - `migrate` 서비스로 한다(초사용자 · 소켓, 덤프가 `/backups` 에 보인다). `sql` · `srvsql` · `nodump` 는 위
「Postgres 로 띄우기」의 관리 명령이다. 단계마다 끝 줄이 나와야 다음으로 간다. 먼저 서버와 매일 덤프를 멈춰 되살릴 DB 에
붙은 연결을 끊는다.

```bash
fdc stop flightdeck db-backup && nodump && echo "내렸다"
```

1. 되살릴 DB 이름을 덤프에서 읽어 `FD_PG_DATABASE` 와 맞댄다. `--create` 는 지금 `FD_PG_DATABASE` 가 아니라 **덤프에 적힌
   이름**으로 DB 를 만든다 - 다르면 서버가 되살린 DB 를 안 본다. **다르면 멈춘다**: 어느 이름을 쓸지(`FD_PG_DATABASE` 와
   pgpass 의 줄을 덤프의 이름으로 바꿀지) 사람이 정한다. 아래 `f` 에 되살릴 덤프의 자리를 채운다.

   ```bash
   f='/backups/daily/<DB 자리>/fd-daily-<UTC>.dump'
   fdc run --rm --no-deps --entrypoint sh migrate -c 'd=$(pg_restore -l "$1" | sed -n "s/^; *dbname: //p")
     echo "덤프의 DB: $d · FD_PG_DATABASE: $PGDATABASE"; [ -n "$d" ] && [ "$d" = "$PGDATABASE" ]' sh "$f" && echo "1 끝: 이름이 같다"
   ```

   그 이름의 DB 가 이미 있으면 지우지 말고 **이름을 바꿔 남긴다**(`<DB>` 는 위에서 읽은 이름):
   `sql -d postgres -c 'ALTER DATABASE "<DB>" RENAME TO "<DB>_before_restore"'`. 옛 DB 는 4 의 확인이 끝난 뒤에 지운다.
2. 역할은 클러스터 전역이라 덤프에 없다. 같은 볼륨(`flightdeck-pgdata`)에 되살리면 이미 있다. 볼륨째 잃었으면 볼륨을 새로
   만들고 `fdc up -d --build db-roles` 로 묶음 역할 · 접속 계정과 빈 원장을 먼저 만든 뒤 1 의 이름 바꾸기부터 한다.
3. DB 째 되살린다. `--create` 가 DB 단위 권한(PUBLIC 의 `TEMPORARY` 회수)까지 되살린다 - 미리 만든 DB 에 부으면 그것이 빠진다.

   ```bash
   fdc run --rm --no-deps --entrypoint pg_restore migrate -d postgres --create --exit-on-error "$f" && echo "3 끝: 되살렸다"
   ```

4. 계정을 다시 재고(`db-roles`) **서버 계정으로 서버의 원장에** 붙어(`srvsql` - 서버의 `FD_DB` 그대로) 되살린 원장인지 본다.
   서버가 붙는 DB 가 이름을 바꿔 남긴 옛 DB 와 같은 DB(같은 oid)거나, 표가 안 읽히거나(멤버십), 항목이 0 건이거나,
   `TEMPORARY` 권한이 남아 있으면 오류로 멈추고 종료 코드가 0 이 아니다(`ON_ERROR_STOP`, 0 으로 나누기 오류를 일부러 낸다).
   마지막 이벤트 시각이 덤프 이름의 시각 바로 전인지는 첫 줄에서 눈으로 본다.

   ```bash
   fdc up -d --build db-roles && [ "$(docker wait "$(fdc ps -aq db-roles)")" = 0 ] && srvsql <<'SQL' && echo "4 끝: 되살린 원장이다"
   SELECT current_database() AS 원장, d.oid, o.oid AS 옛_oid, (SELECT count(*) FROM item) AS 항목,
          (SELECT count(*) FROM judgment) AS 판단, (SELECT max(at) FROM event) AS 마지막_이벤트
     FROM pg_database d LEFT JOIN pg_database o ON o.datname = d.datname || '_before_restore' WHERE d.datname = current_database();
   SELECT 1 / (d.oid IS DISTINCT FROM o.oid)::int AS 새_DB
     FROM pg_database d LEFT JOIN pg_database o ON o.datname = d.datname || '_before_restore' WHERE d.datname = current_database();
   SELECT 1 / (count(*) > 0)::int AS 항목_있음 FROM item;
   SELECT 1 / (NOT has_database_privilege(current_database(), 'TEMPORARY'))::int AS temp_권한_없음;
   SQL
   ```

5. `fdc up -d --build` 로 다시 띄운다. 이름을 바꿔 남긴 옛 DB 는 확인이 끝나면 지운다(`sql -d postgres -c 'DROP DATABASE "<DB>_before_restore"'`).

### SQLite 원장을 Postgres 로 옮기기

compose 배포는 위 「업그레이드: SQLite 원장을 Postgres 로」의 절차로 옮긴다. 이 절은 명령 자체의 동작과 거절 갈래이고,
아래 `<URL>` 은 초사용자나 `fd_owner` 의 구성원으로 붙는 접속 URL 이다(compose 의 `migrate` 서비스에서는 `FD_DB` 가 그것이라 `--db` 를 뺀다).

`fd migrate import-sqlite` 는 이관 판 범위(18-19 - 표 · 칸이 마지막 SQLite 판과 같은 판)의 원장을 **빈** Postgres 원장으로 트랜잭션 하나에 옮긴다. 옮긴 뒤 같은
트랜잭션 안에서 두 원장을 맞대고, 어긋나면 커밋하지 않는다. `fd migrate verify` 는 그 대조만 다시 한다(쓰지 않는다).
대조는 표마다 행 수 · 전 칸 지문과 identity 순번(다음 번호가 원본에서 발급된 최고 번호보다 크다)이고, 어긋난 행은
기본키로 적는다. 종료 코드는 0 = 옮겼다 · 맞다, 1 = 거절 · 어긋남 · 실패, 2 = 사용법이다.

```bash
: '1. 원본에 쓰는 서버를 내린다. 원장 파일을 그대로 --from 에 주거나, 사본을 남기려면 VACUUM INTO 로 뜬다.
   cp 로 복사하지 않는다(아래).'
sqlite3 fd.db "VACUUM INTO 'fd-copy.db'"
: '2. 대상 DB 에 이관 판까지 깐다(기준선과 그 위 증분). 판 19 에서 멈춘다.'
fd migrate --db <URL> --to 19
: '3. 예행: 본 실행과 같은 트랜잭션을 끝까지 돌고 커밋 대신 되감는다. 아무것도 남지 않는다.'
fd migrate import-sqlite --from fd-copy.db --db <URL> --dry-run
: '4. 옮긴다. 성공하면 대조표와 함께 「옮겼다: N 행, 대조가 맞았다」를 낸다.'
fd migrate import-sqlite --from fd-copy.db --db <URL>
: '5. (선택) 서버를 새 원장으로 띄우기 전에 다시 맞댄다. 서버가 쓰기 시작하면 어긋난다.'
fd migrate verify --from fd-copy.db --db <URL>
```

- 사본은 `cp fd.db` 로 뜨지 않는다. SQLite 원장은 WAL 이라 커밋이 본 파일에 옮겨지기 전까지 `fd.db-wal` 에만 있고, `cp` 사본은
  그 커밋을 잃는다. 이관과 대조가 둘 다 같은 사본을 읽으므로 둘 다 맞다고 한다 - 잃은 것을 아무도 말하지 않는다.
  `VACUUM INTO`(또는 `sqlite3 fd.db ".backup fd-copy.db"`)는 -wal 까지 읽은 일관 사본을 뜬다.
- 원본은 읽기 전용으로 연다. 원본 옆 `-wal` 에 바이트가 있으면(서버가 아직 돌거나, 비정상 종료로 체크포인트가 안 됐다)
  거절한다. 서버를 내린 뒤 다시 돌리고, 그래도 남으면 위 `VACUUM INTO` 로 사본을 떠 그것을 준다.
- 접속 계정은 초사용자나 `fd_owner` 의 멤버여야 한다. 시퀀스를 원본의 최고 번호 위로 다시 시작하는데(지워진 최고 번호도
  다시 내지 않는다), 이것은 소유 역할만 한다. 앱 역할로 붙으면 예행도 본 실행과 같이 거절한다.
- `fd migrate verify` 도 같은 계정으로 돌린다. 앱 역할은 identity 시퀀스를 읽을 권한(SELECT)이 없어 순번을 대조하지 못한다.
- 원본이 판 18 보다 옛 판이면 거절한다. 먼저 이 판의 fd 로 사본을 판올림한다(`fd migrate --db <사본>`). 판 범위는 코드 한 곳
  (인덱스만 더한 증분)에서 나오고, 이관 · 대조 · 역이관이 같은 범위를 쓴다.
- 원본의 표 · 칸이 이관 판과 다르면(손으로 고친 원장) 거절한다.
- 대상의 업무 표에 한 행이라도 있거나, identity 시퀀스가 한 번이라도 번호를 냈으면(행을 지웠어도) 거절한다. 판이 이관 판
  범위(18-19)가 아니거나 로케일 관문을 못 지나도 거절한다.
- 대상 DB 에 이 실행 말고 붙은 연결(서버 · psql 등)이 있으면 예행 · 본 실행 모두 거절하고, 남은 연결의 pid · 역할 ·
  application_name 을 적는다. 붙은 서버의 쓰기 일부(관측 기록)는 실행기 잠금을 타지 않아 옮기는 트랜잭션에 끼어든다.
- 사전 검사: text 칸의 깨진 UTF-8, JSON 칸의 깨진 JSON(글 안에 날 NUL 바이트가 든 칸도 JSON 문서가 아니라 여기다)과 짝 없는
  서러게이트가 든 JSON, 칸과 다른 저장 타입, 기본키 NULL, 옮기며 겹치는 기본키 · UNIQUE 칸(아래), FK 가 가리키는 행이 없는 것, 인덱스 키 칸(event 의 project 2048 바이트 ·
  kind 512 바이트)의 상한을 넘는 글(Postgres 의 B-tree 가 그 행을 못 받는다). 하나라도 걸리면 아무것도 쓰지 않고 표 · 칸 ·
  사유 · 행의 기본키를 적는다. 사본에서 그 행을 고친 뒤 다시 돌린다. 키 상한에 걸린 행만은 고치지 않는다 - 추가 전용 감사
  원장(event)이라 자르면 원본과 달라진다. 좌표를 실어 이슈로 알려 달라. `fd migrate verify` 도 사전 검사에 걸린 원본은 맞다고
  하지 않는다(키 상한은 빼고 - 대조는 두 원장에 이미 있는 원문을 맞댄다).
- 새로 쓰는 값은 같은 상한을 넘지 못한다: 서버가 project 2048 바이트 · kind 512 바이트를 넘는 값을 두 엔진 모두 400
  (`key_too_long`)으로 거절한다.
- **NUL 은 `\x00` 로 옮겨진다.** Postgres text 는 NUL 바이트(0x00)를 못 담고, jsonb 는 글에 U+0000(`\u0000`)이 든 문서를
  거절한다. 그런 칸은 거절하지 않고 아래 규칙으로 바꿔 옮긴다. 규칙은 글 하나 단위이고 되돌릴 수 있다.
  - 글 칸: NUL 이 든 칸에서 NUL 은 `\x00` 네 글자가 된다. 바로 뒤가 NUL 이나 `x00` 인 백슬래시 묶음(연달은 백슬래시)은 두 배가
    된다 - 같은 칸에 원래 있던 `\x00` 표기는 `\\x00` 이 되어 NUL 자리와 갈린다. 그 밖의 바이트(`\n` · `\x1f` 같은 다른
    표기도)와 NUL 이 없는 칸은 한 바이트도 안 바뀐다.
  - JSON 칸: 문서를 디코드해 다시 쓰지 않는다. U+0000 이 든 글(키 포함)만 그 글의 원래 바이트 구간에서 고친다 - `\u0000` 은
    `\\x00` 이 되고(디코드하면 `\x00` 네 글자다), 그 글 안에서 바로 뒤가 U+0000 이나 `x00` 인 백슬래시 묶음(`\\` · `\u005c`)은
    두 배가 된다. U+0000 이 없는 글은 같은 문서 안이어도 원래 바이트이고(그 글의 `\x00` 표기도 그대로다), 수 · 공백 · 순서와
    다른 이스케이프(`\u00e9` · `\/`)도 원래 바이트다.
  - 짝 없는 서러게이트(짝이 없는 `\ud800`-`\udfff`)가 든 JSON 은 사전 검사가 거절한다. jsonb 가 받지 않고, 디코드해 다시 쓰면
    U+FFFD 가 되어 다른 두 키가 하나가 된다.
  - 되돌리기: 바꾼 글(글 칸은 칸 전체)에서 `x00` 바로 앞 백슬래시 묶음이 홀수 개(2k+1)면 원래 백슬래시 k 개 + NUL, 짝수
    개(2k)면 백슬래시 k 개 + `x00` 이었다. 그 밖은 그대로다. 어느 칸 · 글을 바꿨는지는 아래 기록 파일이 댄다 - 옮긴 원장의 값만으로는
    바꾼 칸과 처음부터 `\x00` 네 글자를 담은 칸이 갈리지 않는다.
  - **바꾼 칸의 기록.** 옮기기는 커밋 **전에** 원본 옆에 `<원본>.rewritten-<시각>.jsonl` 을 남긴다. 바꾼 칸마다 JSON 한 줄이다:
    `table`, `key`(그 행의 기본키 칸 → 옮긴 값, 자르지 않는다), `column`, `rule`(화면 보고의 규칙), `paths`(JSON 칸에서 고친
    글의 자리 - RFC 6901 JSON Pointer, 원래 문서의 키. 키 이름을 고쳤으면 `"key": true`. 글 칸은 없다), `original`(원본에서 읽은
    그 칸의 바이트, base64). 줄 수에 상한이 없다. **그 칸의 원래 바이트는 이 파일이 정본이다** - 지우지 말고 원본 사본과 함께
    둔다. 이 파일을 못 남기면 커밋하지 않는다(아무것도 옮기지 않는다). 이관 표지(`<원본>.moved-to-postgres`)의 `rewritten` 이
    그 파일을 대고, 「옮겼다」 아래에도 그 이름을 낸다. 예행 · 대조는 남기지 않는다.
  - 조용히 바꾸지 않는다: 예행 · 옮기기 · `fd migrate verify` 가 「옮기며 바꾼 원본 칸」에 표 · 칸 · 규칙 · 수 · 행 좌표(갈래마다
    앞 20 개, 줄인 좌표 - 전부는 기록 파일이다)를 적고, 판정 줄 아래에 바꾼 행 수를 한 번 더 낸다. 대조는 원본 값에 같은 규칙을
    건 값으로 맞대므로 바꾼 행도 맞다. 대상 값은 변환 없이 맞대고, 글 칸의 NUL · 칸과 다른 저장 타입 · 깨진 JSON(짝 없는
    서러게이트 포함)은 대상 값 검사에 건다.
  - 기본키 · UNIQUE 칸이 옮기며 다른 행과 같아지면(NUL 이 든 키와 그 자리에 `\x00` 을 쓴 키, 글 칸에 BLOB 로 든 키와 같은
    바이트의 글 키) 사전 검사가 겹친 행을 표 · 칸 묶음 · 행 좌표로 거절한다 - 대상이 커밋 때 받을 거절을 먼저 낸다. 부분
    인덱스(살아 있는 점유 하나 등)는 그 조건 안의 행끼리만 잰다.
- 옮기고 이관 표지를 남긴 뒤, 원본 옆의 판올림 전 사본(`<원본>.bak-*`, 옆 파일 포함)을 새 디렉토리
  `<원본>.pre-import-<시각>.bak/` 로 옮긴다(지우지 않는다). 그 파일은 이제 원장이 아니고, 그 사본은 뒤에 그 자리에 놓일 원장의
  사본이 아니다. 예행 · 거절 · 실패에서는 옮기지 않는다.
- 사전 검사가 못 보는 거절(대상의 CHECK, 위 상한 밖 칸의 인덱스 항목 크기, 권한)은 예행이 본 실행과 똑같이 받는다. 예행은 실행기 잠금도
  본 실행처럼 쥔다.

### Postgres 원장을 SQLite 로 되돌리기

compose 배포는 위 「72시간 안에 되돌리기」의 절차로 되돌린다(선택 파일을 켜기 전이면 「켜기 전에 섰을 때」). 이 절은 명령
자체의 동작과 거절 갈래다.

`fd migrate export-sqlite` 는 이관의 역이다. 이관 판 범위(18-19)의 Postgres 원장을 **새** SQLite 파일(판 19)로 옮긴다 - 이관한 뒤 Postgres
에 쌓인 항목 · 선점 · 이벤트 · 판단을 들고 SQLite 판으로 돌아가는 길이다. 이관과 같은 트랜잭션 · 같은 대조(행 수 · 전 칸 지문 ·
순번)를 돌고, 대상 옆 임시 파일에 쓴 뒤 대조가 맞아야 `--to` 이름을 붙인다. 어긋나거나 실패하면(예행은 늘) 아무 파일도 남지
않는다. 종료 코드는 이관과 같다.

```bash
: '1. Postgres 원장에 쓰는 서버를 내린다. 이 실행 말고 붙은 연결이 있으면 거절하고 남은 연결의 pid · 역할 ·
   application_name 을 적는다. 원본은 스냅숏 하나(REPEATABLE READ READ ONLY)로 읽기만 한다.'
: '2. 예행: 본 실행과 같은 길을 임시 파일에 끝까지 돌고(이름 붙이기는 같은 디렉토리의 일회용 하드 링크로 재 본다) 그 파일을 지운다.'
fd migrate export-sqlite --db <URL> --to fd-back.db --dry-run
: '3. 옮긴다. 성공하면 대조표와 함께 「옮겼다: N 행, 대조가 맞았다 → fd-back.db」를 낸다.'
fd migrate export-sqlite --db <URL> --to fd-back.db
: '4. 낸 파일로 서버를 띄우기 전에 꼭 다시 맞댄다. --exported 는 두 원장을 변환 없이 맞댄다.'
fd migrate verify --from fd-back.db --db <URL> --exported
```

- 4 는 빼지 않는다. 커밋 전 대조는 옮긴 스냅숏끼리 맞대므로, 연결을 잰 뒤 원본에 붙은 서버가 커밋한 쓰기는 옮기지도 않고
  맞다고 한다. verify 는 원본을 새 스냅숏으로 다시 읽어 그 어긋남을 잡는다. 어긋나면 낸 파일을 지우고 서버를 내린 채 1 부터
  다시 돌린다.
- 4 의 `--exported` 도 빼지 않는다. 빼면 낸 파일을 이관 원본처럼 재어 NUL 바꿔 넣기를 걸므로, 낸 뒤 그 파일에 들어간 NUL 을
  Postgres 의 `\x00` 네 글자와 같다고 한다. `--exported` 는 그 NUL 을 값 검사(글 칸의 NUL)에 건다.

- `--to` 자리에 파일이 있거나 그 옆에 `-wal` · `-shm` · `-journal` 이 있으면 거절한다. 덮지 않고, 남은 `-wal` 이 새 파일에
  되살아나지 않게 한다.
- 원본은 이관 판 범위(18-19)여야 한다. 그 뒤 판으로 올린 Postgres 원장은 SQLite 판에 자리가 없어 거절한다. 표 · 칸이 이관 판과 다른
  원장(손으로 고친 원장)도 거절한다. 판과 모양은 옮기는 스냅숏 안에서 잰다. 접속 계정은 이관과 같다(초사용자나 `fd_owner` 의
  멤버 - 시퀀스를 읽는다).
- 이관의 사전 검사가 받지 않을 값(깨진 JSON, 짝 없는 서러게이트가 든 JSON 등)이 원본에 있으면 거절한다 - 낸 파일을 다시 이관할 수 없게 되기 때문이다.
  인덱스 키 상한은 재지 않는다: 이미 Postgres 원장에 있는 원문을 그대로 내고, `fd migrate verify` 도 그 원문을 맞댄다. 그 파일을
  다시 이관하면 이관의 사전 검사가 그 행을 좌표와 함께 거절한다.
- Postgres 값을 그대로 낸다. 이관이 NUL 자리에 쓴 `\x00` 은 NUL 로 되돌리지 않는다(원래 바이트는 이관의 기록 파일이 정본이다) -
  낸 파일에는 NUL 이 없고, 그 파일과 Postgres 원장의 대조(4)는 변환 없이 맞는다. JSON 글의 `\u0000`(이관 뒤 서버가 쓴 값 -
  JSON 칸은 jsonb 가 아니라 글 칸이라 Postgres 가 담는다)도 거절하지 않고 그대로 낸다. 그래서 되돌리기가 그 값에 서지 않고, 그
  파일을 다시 이관하면 그 칸은 위 규칙으로 바뀌고 기록 파일에 남는다.
- `--to` 옆에 판올림 전 사본(`<to>.bak-*`, 옆 파일 포함)이 있으면 이름을 붙이기 전에 새 디렉토리 `<to>.pre-export-<시각>.bak/`
  로 옮긴다(지우지 않는다) - 새 원장의 사본이 아니므로 `fd migrate --rollback` 이 고를 자리에 두지 않는다. 예행은 옮기지 않는다.
- 순번: AUTOINCREMENT 표(pick_eval · resource_hold · event · landing_queue)는 Postgres 가 낸 최고 번호를 `sqlite_sequence` 에
  되살려 그 번호를 다시 내지 않는다. item_after 는 SQLite rowid 표라 지워진 최고 번호를 기억하지 못한다 - seq 값(순서)은 rowid 로
  그대로 옮고, 다음 번호는 SQLite 판 그대로 max(rowid)+1 이다.
- 판단 검색 색인(judgment_fts)은 Postgres 에 없어 옮기지 않는다. 옮긴 판단 행에서 다시 짓고 판단 행과 맞는지 잰다.
- 낸 파일의 모드는 0644 이고, 이름을 붙인 디렉토리를 fsync 한 뒤에야 「옮겼다」를 낸다.
- Ctrl-C · SIGTERM 으로 멈추면 임시 파일을 지우고 1 로 끝난다. SIGKILL · 전원 차단으로 끝나면 `--to` 디렉토리에
  `.<이름>.export-<숫자>`(와 그 옆 `-wal` · `-shm` · `.link`)가 남을 수 있다. 역이관이 돌지 않을 때
  `ls -a <디렉토리> | grep '^\.fd-back\.db\.export-'` 로 알아보고 지운다. 그 파일에는 이름이 붙지 않았으니 원장이 아니다.

**되돌리기 예행.** 실제로 옮기기 전에 왕복을 한 번 돌려 둔다: 원장 사본 → `import-sqlite` 로 시험용 Postgres 원장 → 그 원장으로
서버를 띄워 몇 가지를 쓴다(항목 · 선점 · 판단) → 서버를 내리고 `export-sqlite` → `fd migrate verify --from <낸 파일> --db <URL> --exported`
이 맞는지, 낸 파일로 띄운 SQLite 판 서버에서 그 쓰기가 보이고 판단 검색이 되는지 본다.

## 여러 저장소

한 제품이 저장소 여럿으로 갈리면 루트 폴더에 `.flightdeck.yaml` 로 멤버를 선언하고 루트에서 한 번만
띄워 전부를 관장한다. 절차는 [여러 저장소](docs/workspaces.ko.md)에 있다.

## 서버가 죽으면

`SessionStart` 가 배너를 주입한다 — 조정 기구가 있는 줄 알고 움직이는 에이전트가 없도록.

```
⚠ 조정 서버 미도달(http://localhost:7420, 마지막 접속 14:02 · 37분 전).
  되는 것: 코드 작성·커밋·조사 전부. 이미 선점한 항목의 작업.
  안 되는 것: 새 항목 선점 · 다른 세션의 현재 상태.
```

읽기는 마지막 스냅숏을 낡음 표시와 함께 내고, 판단·노트는 쌓였다가 재연결 때 재생된다. 선점·발번·
랜딩 레인은 거절된다 — 오프라인에서 허용하면 배타가 거짓이 된다.

## 문제 해결

먼저 `fd doctor` 를 돌려라. 플랫폼 축과 서버 상태를 하나씩 이름으로 낸다.

| 증상 | 볼 곳 |
|---|---|
| 보드가 비어 있다 | 아무도 항목을 안 쥐었으면 그것이 정상이다. 이상하면 `fd doctor` 의 `FD_URL` 부터 |
| 훅이 아무것도 안 한다 | `bin/fd` 를 직접 돌려 본다 — Go 가 없으면 안내가 나온다 |
| 도구가 안 보인다 | `.mcp.json` 의 `type` 이 `stdio` 인가 |
| 보드에 `브랜치 ?(못 읽음)` | 서버 컨테이너에 저장소가 안 마운트됐다(`FD_REPOS`) |
| codex 에서 카드가 안 뜬다 | 훅 승인 전이다 — `codex` TUI 를 한 번 띄운다 |

## 설계 원칙

1. **파생 가능한 사실에는 쓰기 API 를 만들지 않는다.** 검사가 아니라 부재로 강제한다.
2. **세션이 외워야 할 개념 수를 늘리지 않는다.** 도구가 늘면 핸드오프·후속 등록 같은 뒤쪽 규율이 먼저 빠진다.
3. **정합성은 REST, MCP 는 그 위의 얇은 껍데기.** 둘 다 같은 함수를 부른다.

그리고 **생존 판정 불리언이 없다.** "저 세션 살아 있나"에 예/아니오로 답하지 않고 신호별 나이만 낸다 —
그 판정이 실측에서 두 번 틀렸기 때문이다. 이유와 안 만든 것의 목록은 [개념과 동작](docs/concepts.ko.md)과
설계 정본 [DESIGN.md](DESIGN.md)에 있다.

## 실제 운영

2026년 8월 운영 원장에서, 한 머신의 **세션 24개가 10분 창 안에 동시에** 신호를 냈고 그중 22개가
같은 두 저장소를 만지고 있었다. 첫 열흘 동안 겹침 처방이 601건 나갔고 랜딩 줄서기는 두 저장소 합쳐
157회였다.

## 개발

저장소 루트가 곧 플러그인 루트다. 서버·CLI·MCP·훅은 `server/` 의 Go 모듈 하나에서 나온다.

```bash
cd server
gofmt -l .                                            # 빈 출력
go vet ./... && GOOS=linux go vet ./... && GOOS=windows go vet ./...
# 시험: PG 시험 컨테이너를 띄워 FD_TEST_PG 로 주고, 끝나면 내린다(수 분 걸린다)
(p=fdtest-$(basename "$(dirname "$PWD")"); trap 'docker compose -f ../compose.test.yaml -p "$p" down' EXIT; trap 'exit 143' HUP INT TERM; docker compose -f ../compose.test.yaml -p "$p" up -d --wait && FD_TEST_PG="postgres://postgres@$(docker compose -f ../compose.test.yaml -p "$p" port pg-test 5432)/postgres?sslmode=disable" go test ./...)
```

시험은 Postgres 에서 돈다. 시험 전용 컨테이너는 `compose.test.yaml` 이고(Docker 가 필요하다), 시험은 그 주소를
`FD_TEST_PG` 로 받는다. `FD_TEST_PG` 가 없으면 시험은 건너뛰지 않고 **실패**한다. 위 한 줄이 띄우기 · 돌리기 ·
내리기를 다 하고, 시험이 실패하거나 셸이 끊겨도 컨테이너를 내린다.

코드가 바뀐 push 는 `.claude-plugin/plugin.json` 의 `version` 을 같이 올린다 — 안 올리면
`/plugin update` 가 새 코드를 안 가져온다. 구성과 관문의 자세한 이유는 [개발](docs/development.ko.md)에 있다.

## 출처

flightdeck 은 플러그인 모음 [kweiza-cc-plugins](https://github.com/Kweiza/kweiza-cc-plugins) 의
`plugins/flightdeck` 에서 자랐고 2026-09-24 에 0.38.3 을 기준으로 여기로 분리됐다. 그전의 이력과,
문서·코드 주석이 인용하는 커밋 sha 는 그 저장소에 있다.

## 라이선스

MIT — [LICENSE](LICENSE) 를 보라.
