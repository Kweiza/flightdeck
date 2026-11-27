<!-- 이 파일은 사람이 읽는 참조본이다. Claude Code 는 SKILL.md 만 읽는다 —
     이 파일을 고쳐도 스킬 동작은 안 바뀐다.
     한글이 정본이다: 여기를 먼저 고치고 SKILL.md 를 맞춰라. -->
---
name: fd-update
description: 서버·플러그인·DB 를 최신으로 올린다. "갱신" · "업데이트" · "최신인지 확인" · "새 버전 나왔다" · 보드가 낡았거나 브랜치를 못 읽을 때 쓴다.
---

# 갱신

낡음은 세 축이다 — **플러그인 · 서버 · DB**. 셋은 따로 낡고, 하나만 고치면 나머지는 그대로다.

## 1. 무엇이 낡았는지 잰다

```
fd status | head -1
fd doctor
claude plugin list
```

`fd status` **첫 줄이 판정이다.** `파생 git@…` 이어야 서버가 git 을 읽고 있다.
`파생 db@…` 나 `브랜치 ?(못 읽음)` 이면 **그 서버의 겹침·브랜치·발자국은 전부 못 믿는다** —
갱신 대상이지 참고 자료가 아니다.

**`/healthz` 의 `ok=true` 를 최신 판정으로 쓰지 마라.** 몇 달 낡은 서버도, git 을 못 읽는
서버도 똑같이 `ok` 를 낸다. 그것은 "떠 있다"만 말한다.

## 2. 순서가 있다

**푸시 → 마켓플레이스 → 플러그인 → 컨테이너.** 마켓플레이스가 git 원격이라,
`plugin.json` 버전을 올려 **푸시하지 않으면 갱신이 아예 오지 않는다.** 순서를 건너뛰면
옛 판을 다시 설치하고는 최신이라고 믿게 된다.

## 3. 바꾸기 전에 새 판에게 묻는다

```
fd selfcheck --db ~/.flightdeck/fd.db
```

**재기동해도 되는가**에만 답한다 — 그 바이너리가 실행되는가, DB 증분 계획이 거절이 아닌가.
**되돌릴 때도 이걸 먼저 물어라.** DB 버전이 그 판이 아는 것보다 높으면 거절이 나온다.
(`모르는 명령: selfcheck` 가 나올 만큼 옛 판이면, 그 판의 `serve` 가 기동에서 같은 이유로 멈춘다.)
**Postgres 원장이면**(선택 파일 `/etc/flightdeck/compose.env` 이 있다 — README 「Postgres 로 띄우기」) 4 의 플러그인 갱신 뒤 새 자리에서 `if [ -e ~/.flightdeck/compose.env ]; then echo '선택 파일의 옛 자리 ~/.flightdeck/compose.env 는 서버가 쓸 수 있는 /data 안이라 더는 읽지 않는다 - 내용을 확인한 뒤 /etc/flightdeck/compose.env 로 옮기고 다시 돌린다: sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck && mv -n ~/.flightdeck/compose.env /etc/flightdeck/compose.env (README 「Postgres 로 띄우기」)' >&2; false; elif [ -n "${FD_PG_INSTANCE+x}${FD_PG_DATABASE+x}" ]; then echo 'FD_PG_INSTANCE · FD_PG_DATABASE 는 셸에서 주지 않는다 - 어느 원장(볼륨 · DB)인지는 선택 파일 /etc/flightdeck/compose.env(꺼 두었으면 .off) 한 곳에 적는다. 셸 값은 선택 파일보다 먼저라 새 셸에서 빠지면 다른 원장으로 뜬다: 값을 선택 파일에 적고 unset FD_PG_INSTANCE FD_PG_DATABASE 뒤 다시 돌린다 (README 「Postgres 로 띄우기」)' >&2; false; fi && COMPOSE_ENV_FILES="$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)" docker compose run --rm --build --no-deps migrate selfcheck` 로 묻는다. 4 의 내리기 줄은 셋을 먼저 재고 하나라도 걸리면 내리지 않고 선다 — **서면 올리지 말고 멈추고 묻는다**. ① 선택 파일의 옛 자리 `~/.flightdeck/compose.env` 에 파일이 남았거나, 셸이 `FD_PG_INSTANCE` · `FD_PG_DATABASE` 를 준다(빈 값도): 앞은 서버가 쓰는 `/data` 안이라 더는 안 읽고(줄이 옮기는 명령을 낸다), 뒤는 어느 원장(볼륨 · DB)인지를 정하는 값이라 선택 파일에만 둔다 — 셸 값이 선택 파일보다 먼저라, 그 값이 빠진 셸에서는 다른 원장으로 뜬다. ② 도는 서버의 `FD_DB`(`docker inspect`)와 선택 파일이 어긋난다: `postgres://` 인데 파일이 없으면 기본 경로(SQLite)로 뜨려 들고(이관 표지 `~/.flightdeck/fd.db.moved-to-postgres` 가 있으면 migrate 가 거절해 서버가 안 뜨고, 없으면 옛 SQLite 원장에 쓴다), SQLite 인데 파일이 있으면 이관 없이 Postgres 로 뜨려 든다. ③ 손으로 돌린 덤프(`run … db-backup once`)가 돈다: `down` 이 그것을 안 멈추고, 그 표 잠금에 새 판 migrate 의 DDL 이 실패한다 — 끝나기를 기다린다. 이 절과 아래 4 의 compose 머리(`COMPOSE_ENV_FILES=…`)는 `fd setup` 의 기동 명령 · 감시기의 컨테이너 사유와 같은 한 줄이다 — 그 파일이 있으면 덮개를 얹고, 없으면 빈 값이라 기본 compose 다. 따로 고쳐 쓰지 않는다.

## 4. 갱신한다

```
claude plugin marketplace update <마켓플레이스>
claude plugin update flightdeck@<마켓플레이스>

fdp=$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project"}}') && fdw=$(docker inspect flightdeck -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}') && [ -n "$fdp" ] && [ -d "$fdw" ] &&
mounts=$(docker inspect flightdeck --format '{{range .Mounts}}{{if eq .Source .Destination}}{{println .Source}}{{end}}{{end}}') && nmounts=$(printf '%s\n' "$mounts" | grep -c .) && if [ "$nmounts" -gt 4 ]; then echo "도는 서버의 저장소 마운트가 $nmounts 개다 - 슬롯(FD_REPOS…FD_REPOS4)은 넷이라 다섯째부터 빠진다: 멈추고 묻는다" >&2; false; fi &&
export FD_REPOS="$(printf '%s\n' "$mounts" | sed -n 1p)" FD_REPOS2="$(printf '%s\n' "$mounts" | sed -n 2p)" FD_REPOS3="$(printf '%s\n' "$mounts" | sed -n 3p)" FD_REPOS4="$(printf '%s\n' "$mounts" | sed -n 4p)" &&
if [ -e ~/.flightdeck/compose.env ]; then echo '선택 파일의 옛 자리 ~/.flightdeck/compose.env 는 서버가 쓸 수 있는 /data 안이라 더는 읽지 않는다 - 내용을 확인한 뒤 /etc/flightdeck/compose.env 로 옮기고 다시 돌린다: sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck && mv -n ~/.flightdeck/compose.env /etc/flightdeck/compose.env (README 「Postgres 로 띄우기」)' >&2; false; elif [ -n "${FD_PG_INSTANCE+x}${FD_PG_DATABASE+x}" ]; then echo 'FD_PG_INSTANCE · FD_PG_DATABASE 는 셸에서 주지 않는다 - 어느 원장(볼륨 · DB)인지는 선택 파일 /etc/flightdeck/compose.env(꺼 두었으면 .off) 한 곳에 적는다. 셸 값은 선택 파일보다 먼저라 새 셸에서 빠지면 다른 원장으로 뜬다: 값을 선택 파일에 적고 unset FD_PG_INSTANCE FD_PG_DATABASE 뒤 다시 돌린다 (README 「Postgres 로 띄우기」)' >&2; false; fi && fdenv=$(docker inspect flightdeck -f '{{range .Config.Env}}{{println .}}{{end}}') && dumps=$(docker ps -q --filter label=com.docker.compose.service=db-backup --filter label=com.docker.compose.oneoff=True) && if case "$(printf '%s\n' "$fdenv" | sed -n 's/^FD_DB=//p')" in postgres://*) [ ! -e /etc/flightdeck/compose.env ];; *) [ -e /etc/flightdeck/compose.env ];; esac; then echo '도는 서버의 FD_DB 와 선택 파일 /etc/flightdeck/compose.env 이 어긋난다 - 내리지 않는다, 멈추고 묻는다' >&2; false; fi && if [ -n "$dumps" ]; then echo '손으로 돌린 덤프(db-backup once)가 돈다 - 끝나기를 기다렸다가 다시 돌린다' >&2; false; fi && cd "$fdw" && COMPOSE_PROJECT_NAME="$fdp" COMPOSE_ENV_FILES="$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)" docker compose down &&
cd ~/.claude/plugins/cache/<마켓플레이스>/flightdeck/<새 버전> &&
FD_TOKEN="$(cat ~/.flightdeck/token 2>/dev/null || sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' ~/.flightdeck/config.json)" FD_UID=$(id -u) FD_GID=$(id -g) COMPOSE_ENV_FILES="$([ ! -e /etc/flightdeck/compose.env ] || echo /etc/flightdeck/compose.env)" docker compose up -d --build
```

**서버 읽기부터 올리기까지 여섯 줄이 `&&` 한 사슬이다.** **내리기 전에 저장소 마운트를 전부 읽는다** — 이 값들은 어디에도 안 남는다. 첫 줄은 README 「Postgres 로 띄우기」의 `fdwhere`, 둘째 · 셋째 줄은 `fdrepos` 의 조회 줄과 같은 글자다(시험이 맞댄다): 도는 서버의 compose 라벨(프로젝트 · 자리)과 저장소 마운트를 읽는다. docker 조회는 모두 따로 선 대입(`x=$(docker …)`)이라 docker 가 실패하면 그 자리에서 선다 — `[ -n "$(docker …)" ]` · `cd "$(docker …)"` 는 실패를 빈 값으로 읽고 지나간다. 마운트는 docker 가 낸 **한 줄을 경로 하나로** 읽어(`sed -n 1p` …) 차례로 슬롯에 넣는다 — 공백이 든 경로(`/repos/team one`)도 한 경로다. 하나도 못 읽으면(줄을 세는 `grep -c .` 가 0 으로 실패한다) 서고, 넷을 넘으면 다섯째부터 빠지므로 내리기 **전에** 선다. 공백으로 가르는 `set -- $(…)` · `readarray` · 배열은 쓰지 않는다: 앞은 공백 경로를 둘로 갈라 없는 경로를 붙이고, zsh 와 macOS 의 bash 3.2 에는 `readarray` 가 없어 사슬이 첫 줄에서 서고(127), zsh 의 배열은 1 부터 센다. 읽은 값은 `export` 로 이 셸에 남고 올리기가 그대로 넘긴다. 서버 · 마운트를 못 읽거나 내리기 줄의 관문이 서면 새 자리로 옮기지도
올리지도 않는다 — 서면 멈추고 묻는다. 줄을 나눠 따로 돌리지 않는다(앞 줄이 서도 뒷줄의 `up` 이 돈다).
**옛 자리에서 먼저 내린다** — 도는 컨테이너의 compose 라벨(프로젝트 · 자리)로 그 자리에서. compose 프로젝트 이름이 **버전 디렉토리**라,
새 자리에서 바로 올리면 컨테이너 이름이 충돌해 멈춘다. 토큰 파일이 없는 머신도 있어 위 명령이 `config.json` 으로 흘러간다.

**저장소 마운트는 슬롯 넷이다(`FD_REPOS`~`FD_REPOS4`). 하나라도 빠뜨리면 조용히 기본값(홈)으로 되돌아간다.**
빈 슬롯은 첫 슬롯으로 접히도록 설계돼 있으니 그대로 넘겨도 된다. 그 값은 compose 치환 시점에만 쓰이고
컨테이너 env 에도 안 남아 흔적이 **마운트뿐**이라, 내린 뒤에는 못 읽는다. 좁혀 둔 머신에서 이걸
빠뜨리면 갱신마다 풀린다 — 실측(NAS): 등록 7건 중 **6건이 파생 불능**이었고 `healthz` 는 ok 였다.

**설치된 캐시 자리에서 띄운다.** 개발 저장소에서 띄우면 설치된 판과 도는 판이 갈라진다.
DB 증분은 compose 의 `migrate` 단계가 서버보다 먼저 얹고 얹기 전에 백업을 뜬다(서버는 그것이 성공한 뒤에만 뜨고, 안 뜨면 사유는 `logs migrate`). Postgres 원장은 고정 이름 볼륨이라 자리가 바뀌어도 그대로다.

## 5. 갱신됐다는 증거를 본다

`fd status | head -1` 이 `파생 git@<방금 시각>` 을 내는지, `claude plugin list` 가 새 버전을
내는지 **둘 다** 본다. 하나만 보면 반쪽 갱신을 최신으로 착각한다.

## 6. 사람이 할 일을 반드시 전한다

> **Claude Code 를 다시 시작해야 한다 — 창 하나가 아니라 열려 있는 세션마다.**
> MCP 서버는 기동 시 설정을 한 번 읽고 끝이라, 안 띄운 창은 옛 토큰·옛 도구를 계속 쓴다.
> 그때까지는 MCP 대신 CLI 를 써라(`fd pick` · `fd note` · `fd finish` 는 지금 된다).

이 말을 빼면 사용자는 "갱신했는데 안 된다"를 겪는다. 서버는 이미 새 판인데 화면만 옛 판이라
원인이 안 보인다.

## 안 하는 것

버전을 안 올리고 수동 빌드로 우회하기(그 서버는 낡은 채로 계속 돈다) · `healthz` 만 보고
최신이라 말하기 · 재시작 안내 빼먹기 · **다른 세션이 참조 중인 옛 플러그인 캐시 지우기**
(그 세션들의 훅·MCP 가 그 자리에서 깨진다).
