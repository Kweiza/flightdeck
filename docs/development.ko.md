[English](development.md) | **한국어**

# 개발

저장소 구성, 검증 관문, 판올림 규칙.

[← README](../README.ko.md)

---

저장소 루트가 곧 플러그인 루트다(`${CLAUDE_PLUGIN_ROOT}` 가 가리키는 자리).

| 자리 | 무엇 |
|---|---|
| `.claude-plugin/` | `plugin.json`(판) · `marketplace.json`(이 저장소를 마켓플레이스로 만든다) |
| `bin/fd` | 셸 런처. 첫 호출에 `server/` 를 빌드해 `~/.cache/flightdeck/bin` 에 캐시한다 |
| `hooks/hooks.json` · `.mcp.json` | Claude Code 훅 배선과 MCP 서버(`fd mcp`) |
| `skills/` | 스킬 4개 |
| `board/` | 소유자 칸반 보드(`board.py` · `index.html` · `install.sh`). 시험 `test_board.py` 는 `go test ./cmd/fd` 가 돌린다 |
| `server/` | Go 모듈 하나 — 서버 · CLI · MCP · 훅이 **한 바이너리**다 |
| `compose.yaml` | 서버 컨테이너(스키마 적용 one-shot + 서버). 원장은 SQLite 파일 |
| `compose.postgres.yaml` · `deploy/` | Postgres 원장 덮개 파일(원장 DB · 접속 계정 · 매일 덤프)과 그 스크립트 · 접속 규칙(`db-roles.sh` · `db-backup.sh` · `pg_hba.conf`)을 구운 이미지(`Dockerfile`). 시험 `deploypg_test.go` 가 렌더한 compose 설정으로, `deployhba_test.go` 가 그 이미지로 띄운 실제 Postgres 로, `readmeproc_test.go` 가 README 의 이관 · 되돌리기 절차를 대역 docker 로 잰다 |
| `compose.test.yaml` | 시험 전용 Postgres(`pg-test`). 시험이 `FD_TEST_PG` 로 받는다 |
| `DESIGN.md` | 설계 정본 |
| `docs/*.md` · `docs/*.ko.md` | 주제별 문서(개념 · 서버 운영 · codex · 여러 저장소 · 레퍼런스 · 개발) |

## 검증 관문

```bash
cd server
gofmt -l .                        # 빈 출력이어야 한다 — 먼저 cwd 가 모듈 안인지 봐라
go vet ./...
GOOS=linux go vet ./... && GOOS=windows go vet ./...
# 시험: PG 시험 컨테이너를 띄워 FD_TEST_PG 로 주고, 끝나면 내린다(수 분 걸린다)
(p=fdtest-$(basename "$(dirname "$PWD")"); trap 'docker compose -f ../compose.test.yaml -p "$p" down' EXIT; trap 'exit 143' HUP INT TERM; docker compose -f ../compose.test.yaml -p "$p" up -d --wait && FD_TEST_PG="postgres://postgres@$(docker compose -f ../compose.test.yaml -p "$p" port pg-test 5432)/postgres?sslmode=disable" go test ./...)
```

- **시험은 Postgres 에서 돈다.** `compose.test.yaml` 의 `pg-test`(Docker 필요)를 띄우고 그 주소를 `FD_TEST_PG` 로
  준다. 시험 도우미(`server/internal/store/storetest`)가 시험마다 원판 DB 를 복제한 새 DB 를 만들고 끝나면 지운다.
- **`FD_TEST_PG` 가 없으면 실패한다 — 건너뛰지 않는다.** 건너뛰면 아무것도 안 돈 패키지가 초록으로 남는다.
- **시험 컨테이너는 운영과 아무것도 나누지 않는다.** 모든 compose 명령에 `-f ../compose.test.yaml` 을 적는다
  (빠뜨리면 운영 `compose.yaml` 을 집어 내리기가 운영 서버를 내린다). 프로젝트 이름은 체크아웃마다 다르고
  (`fdtest-<디렉토리>`), 호스트 포트는 루프백에 도커가 고른다 — 체크아웃 둘이 동시에 관문을 돌려도 서로를 내리지 않는다.
- **내리기는 서브셸의 EXIT 트랩이 진다.** 시험이 실패해도, 띄우기가 실패해도, 셸이 HUP · INT · TERM 을 받아도
  컨테이너를 내린다. 종료 코드는 `go test` 의 것이다.

- **교차 검증은 `go build` 가 아니라 `go vet` 으로 한다.** `go build` 는 `_test.go` 를 건너뛰어
  시험 코드가 다른 OS 에서 깨져도 조용하다.
- **무출력은 통과가 아니다.** cwd 가 모듈 밖이면 `gofmt -l` 은 빈 디렉토리를 재고 아무 말 없이 끝난다.

## 판올림

코드가 바뀐 push 는 `.claude-plugin/plugin.json` 의 `version` 을 **같은 push 에서** 올린다.
마켓플레이스가 git 저장소라 판이 그대로면 `/plugin update` 가 새 코드를 **아예 안 가져온다.**
크기는 새 동사·도구·스키마면 minor, 표시·문구·버그면 patch 다.
돌고 있는 서버까지 바꾸려면 그다음 순서는 `/fd-update` 스킬이 안다(push → 마켓플레이스 → 플러그인 → 컨테이너).

## 문서

- 설계 정본은 [`DESIGN.md`](../DESIGN.md) 다. 여기 없는 것은 만들지 않는다.
- **한국어가 정본이다.** `README.ko.md`·`docs/*.ko.md` 를 먼저 고치고 영문(`README.md`·`docs/*.md`)을
  맞춘다. 이 저장소의 커밋·판단·코드 주석이 전부 한글이라 그 방향이 현실과 맞는다.
- GitHub 은 언어별 README 를 자동으로 안 고른다(`README.md` 만 렌더링한다). 그래서 문서마다 맨
  윗줄의 언어 링크가 그 역할을 한다.
- README 의 「MCP 도구 N개」·「스킬 N개」(영문 「N MCP tools」·「N skills」)는 시험이 실재하는 수와
  대조한다(`server/cmd/fd/plugin_test.go`). 도구나 스킬이 늘면 README 의 그 수부터 틀린다.

## 이 저장소에 두지 않는 것

이 저장소는 제품이지, 제품을 만든 기록이 아니다. 커밋 · 문서 · 코드 주석 · 시험 값 · 커밋 메시지 어디에도
다음을 넣지 않는다.

- 기기 · 사람 정보: 홈 디렉토리 경로, 계정 · 호스트 이름, tailnet 이름, IP 주소, 세션 id, 사람 이름 · 메일.
- 다른 저장소 · 프로젝트의 이름과 거기서 잰 숫자. 설계 문서에 잰 숫자는 남겨도 되지만 어디서 쟀는지는 남기지 않는다.
- 개발 자료: 기능별 계획 · 스펙, 인계 문서, 실측 기록, 리뷰 메모. git 밖, 체크아웃 옆에 둔다.

시험 값은 중립 값(`/home/u/proj`, `example.com`, `proj-a`)을 쓴다.
