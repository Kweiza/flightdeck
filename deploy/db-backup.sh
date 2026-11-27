#!/bin/sh
# flightdeck db-backup - Postgres 원장의 매일 덤프(pg_dump -Fc)와 보존 회전.
#
# compose.postgres.yaml 의 db-backup 서비스가 postgres 이미지 안에서 돌린다(서버와 같은 주판이라 pg_dump 가 서버보다 낮지 않다).
# 매시간 판단 원장(JSONL · journal.git, fd serve 안의 잡)과는 다른 겹이다: 그것은 여섯 표의 결정적 산출물이고, 이것은
# 원장 전체를 새 DB 로 되살리는 복원 수단이다.
#
#   sh db-backup.sh          끝없이 돈다: 때가 되면 덤프를 뜨고, CHECK 초마다 다시 본다(서비스의 기본).
#   sh db-backup.sh once     한 번만 본다: 때가 됐으면 뜨고 끝난다. 0 = 떴거나 아직 때가 아니다, 1 = 실패,
#                            3 = 다른 실행이 같은 자리에서 돌고 있어 아무것도 안 했다.
#   sh db-backup.sh check    가장 새 덤프가 STALE 분 안에 떠졌으면 0(컨테이너 헬스체크).
#   sh db-backup.sh config   읽은 설정을 한 줄로 내고 끝난다. 설정이 틀렸으면 다른 동작처럼 2 로 끝난다.
#
# 접속은 libpq 의 표준 환경 변수(PGHOST · PGPORT · PGDATABASE · PGUSER)로만 받는다. 비밀번호는 PGPASSFILE(pgpass 파일)로만
# 받는다 - 명령줄과 환경 변수에 비밀번호가 없다(ps 와 docker inspect 에 안 보인다). --no-password 라 파일이 없거나 틀리면
# 묻지 않고 실패한다.
#
# 설정(환경 변수):
#   FD_DB_BACKUP_DIR        덤프 자리의 뿌리(기본 /backups/daily). 판올림 전 덤프(/backups)와 다른 자리다 - 회전이 그것을 못
#                           지운다. 덤프는 그 아래 DB 마다의 하위 자리(dbkey)에 쌓인다.
#   FD_DB_BACKUP_KEEP       남길 덤프 수(기본 14, 재설계 스펙의 보존 규칙).
#   FD_DB_BACKUP_EVERY_MIN  덤프 간격(분, 기본 1440). 가장 새 덤프가 이보다 젊으면 안 뜬다 - 재시작이 덤프를 늘리지 않는다.
#   FD_DB_BACKUP_CHECK_SEC  다시 보는 간격(초, 기본 3600). 실패한 덤프는 이 간격 뒤에 다시 시도한다.
#   FD_DB_BACKUP_STALE_MIN  check 가 낡았다고 보는 나이(분). 기본은 EVERY + CHECK 두 번 + 기한이다(기본값으로 26시간 30분):
#                           잘 도는 동안 가장 새 덤프의 나이는 EVERY + CHECK 한 번 + 덤프 시간을 안 넘고, 남은 CHECK 한 번은
#                           한 번 실패한 재시도까지 건강하게 본다.
#   FD_DUMP_TIMEOUT         덤프 한 번의 기한(Go 시간 표기, 기본 30m, 1초 이상). fd migrate 의 판올림 전 덤프와 같은 설정
#                           하나다 - 같은 이름 · 같은 문법(time.ParseDuration) · 같은 범위이고, 시험이 둘을 맞댄다.
#
# ★ 회전은 덤프가 성공한 뒤에만 한다. 실패한 날에 지우면 접속이 끊긴 보름 뒤에는 덤프가 0개다.
# ★ 덤프는 실행마다 다른 임시 이름(mktemp)으로 뜨고, pg_restore --list 로 읽히는 것을 본 뒤에 이름을 바꾼다. 반쯤 쓴 파일이
#   덤프 이름을 갖지 않는다.
# ★ 한 DB 자리에서는 한 실행만 돈다. 정리 · 때 확인 · 덤프 · 회전을 그 자리의 잠금(flock) 아래에서 한다. 잠금이 없으면 정기
#   덤프가 도는 중에 시작한 수동 once 가 작성 중인 임시 파일을 지우거나, 같은 초에 시작한 둘이 상대의 미완성 파일을 덤프
#   이름으로 바꾼다. 잠금은 pg_dump 까지 물려받으므로, 셸이 죽어도 pg_dump 가 살아 있는 동안은 다른 실행이 못 잡는다.
# ★ 덤프 자리는 DB 마다 따로다(FD_DB_BACKUP_DIR/<dbkey>). PGDATABASE 를 A 에서 B 로 바꾸면 A 의 덤프가 B 의 때 확인에 안
#   쓰이고 B 의 회전이 A 의 덤프를 안 지운다.
# ★ 덤프를 고르는 이름 규칙은 하나다(dumps: fd-daily-<UTC>.dump). 때 확인 · 헬스체크 · 회전이 모두 그것만 본다. 다른 파일은
#   자리에 있어도 세지도 지우지도 않는다.
# ★ pg_dump 에 기한(timeout)과 잠금 대기 상한(--lock-wait-timeout)이 있다. pg_dump 는 끝날 때까지 REPEATABLE READ 스냅숏과
#   모든 표의 ACCESS SHARE 잠금을 쥔다 - 볼륨이 멈춰 덤프가 서면 그동안 fd migrate 의 DDL 이 lock_timeout 으로 실패하고
#   VACUUM 이 묶인다. 기한을 넘으면 pg_dump 를 끊고 실패로 친다(반쪽 파일을 안 남기고 회전하지 않는다). 서버 쪽 상한
#   (statement_timeout 등)으로는 못 묶는다 - pg_dump 가 접속하자마자 그것들을 0 으로 끈다.

set -u
umask 077

mode=${1:-loop}
root=${FD_DB_BACKUP_DIR:-/backups/daily}
keep=${FD_DB_BACKUP_KEEP:-14}
every=${FD_DB_BACKUP_EVERY_MIN:-1440}
interval=${FD_DB_BACKUP_CHECK_SEC:-3600}
stale=${FD_DB_BACKUP_STALE_MIN:-}
db=${PGDATABASE:-}

# lock_wait_ms 는 pg_dump 가 덤프 첫머리에 표마다 잡는 잠금을 기다리는 상한(밀리초)이다. 서버 연결의 lock_timeout
# (store.LockTimeout)과 같은 값이다 - 원장의 어느 연결도 잠금 하나를 그보다 오래 기다리지 않는다. 시험이 둘을 맞댄다.
lock_wait_ms=5000

log() { printf '%s fd-db-backup: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
bad() {
	log "$*"
	exit 2
}

# positive 는 셸이 그대로 읽는 양의 정수만 받는다: 숫자만, 앞자리 0 없이, 아홉 자리까지. 앞자리 0 은 셸 산술이 8진수로 읽어
# 014 가 12 가 되고 08 은 오류가 나며, 열 자리를 넘으면 셸 정수 비교가 오류로 끝나 조용히 지나간다.
positive() {
	case $2 in
	'' | *[!0-9]* | 0* | ??????????*) bad "$1 이 양의 정수가 아니다($2) - 숫자만, 앞자리 0 없이, 아홉 자리까지" ;;
	esac
}

# dump_deadline 은 FD_DUMP_TIMEOUT 값 하나를 「초 분」 한 줄로 옮긴다: 초는 밀리초까지(timeout 의 인자), 분은 올림(STALE 의
# 기본값). 비었으면(공백뿐이어도) 30m 이다. 못 읽거나 범위(1초 이상 · 밀리초 int32 이하) 밖이면 아무것도 안 내고 1 이다.
#
# ★ 문법은 Go 의 time.ParseDuration 그대로다: 부호 하나, 그 뒤 「수[.소수] 단위」의 되풀이, 단위는 ns · us · µs · μs · ms ·
#   s · m · h, 단위 없는 수는 0 하나뿐. 범위는 store.CheckDumpTimeout 과 같다. fd migrate 가 받는 값을 여기서 거절하거나 그
#   반대면 한 설정이 두 뜻이 된다 - 시험(TestDBBackupDeadlineReadsLikeFdMigrate)이 fd 의 판정과 맞댄다.
# ★ 나노초 정수로 센다. 범위 안의 값은 2^53 보다 작아 awk 의 실수가 정확히 담는다.
dump_deadline() {
	V=$1 LC_ALL=C awk '
	BEGIN {
		u["ns"] = 1; u["us"] = 1e3; u["µs"] = 1e3; u["μs"] = 1e3; u["ms"] = 1e6
		u["s"] = 1e9; u["m"] = 60e9; u["h"] = 3600e9
		s = ENVIRON["V"]
		gsub(/^[[:space:]]+|[[:space:]]+$/, "", s)
		if (s == "") s = "30m"
		neg = 0
		c = substr(s, 1, 1)
		if (c == "-" || c == "+") { neg = (c == "-"); s = substr(s, 2) }
		t = 0
		if (s == "0") s = ""
		else if (s == "") exit 1
		while (s != "") {
			match(s, /^[0-9]*/); ip = substr(s, 1, RLENGTH); s = substr(s, RLENGTH + 1)
			fp = ""
			if (substr(s, 1, 1) == ".") {
				s = substr(s, 2)
				match(s, /^[0-9]*/); fp = substr(s, 1, RLENGTH); s = substr(s, RLENGTH + 1)
			}
			if (ip == "" && fp == "") exit 1
			match(s, /^[^0-9.]*/); un = substr(s, 1, RLENGTH); s = substr(s, RLENGTH + 1)
			if (!(un in u)) exit 1
			v = (ip == "" ? 0 : ip + 0) * u[un]
			if (fp != "") v += int(("0." fp) * u[un])
			t += v
		}
		if (neg) t = -t
		if (t < 1e9 || t > 2147483647e6) exit 1
		ms = int(t / 1e6)
		sec = sprintf("%d.%03d", int(ms / 1000), ms % 1000)
		sub(/0+$/, "", sec); sub(/\.$/, "", sec)
		printf "%s %d\n", sec, int((ms + 59999) / 60000)
	}'
}

positive FD_DB_BACKUP_KEEP "$keep"
positive FD_DB_BACKUP_EVERY_MIN "$every"
positive FD_DB_BACKUP_CHECK_SEC "$interval"
[ -n "$db" ] || bad "PGDATABASE 가 비었다 - 덤프 자리를 DB 마다 가르는 이름이라 꼭 준다"
if ! deadline=$(dump_deadline "${FD_DUMP_TIMEOUT:-}"); then
	bad "FD_DUMP_TIMEOUT 을 읽지 못했다(${FD_DUMP_TIMEOUT:-}) - 45m · 2h 같은 Go 시간 표기로 1s 이상 596h31m23.647s 이하여야 한다(비우면 30m)"
fi
deadline_s=${deadline% *}
deadline_min=${deadline#* }
if [ -n "$stale" ]; then
	positive FD_DB_BACKUP_STALE_MIN "$stale"
else
	stale=$((every + 2 * ((interval + 59) / 60) + deadline_min))
fi

# dbkey 는 DB 이름을 디렉토리 이름 하나로 옮긴다: 소문자 · 숫자 · _ · - 는 그대로, 나머지 바이트는 %XX 다. 단사라 다른 DB
# 둘이 한 자리에 섞이지 않는다. 대문자도 옮겨 대소문자를 안 가리는 파일 시스템에서도 갈리고, 점도 옮겨 . · .. 가 안 생긴다.
dbkey() {
	printf '%s' "$1" | od -An -v -tx1 | LC_ALL=C awk '
	BEGIN { for (i = 1; i < 128; i++) { c = sprintf("%c", i); if (c ~ /^[a-z0-9_-]$/) plain[sprintf("%02x", i)] = c } }
	{ for (i = 1; i <= NF; i++) out = out (($i in plain) ? plain[$i] : "%" toupper($i)) }
	END { print out }'
}
dir="$root/$(dbkey "$db")"
lockfile="$dir/fd-daily.lock"

settings() {
	printf '자리 %s · 보존 %s · 간격 %s분 · 확인 %s초 · 기한 %s초 · 낡음 %s분\n' \
		"$dir" "$keep" "$every" "$interval" "$deadline_s" "$stale"
}

# dumps 는 자리의 덤프 이름을 새것부터 낸다. 이름의 UTC 시각이 사전순 = 시간순이다. 덤프를 고르는 규칙은 이 하나다.
dumps() {
	ls "$dir" 2>/dev/null | grep -E '^fd-daily-[0-9]{8}T[0-9]{6}Z\.dump$' | sort -r
}

# fresh 는 MIN 분보다 젊은 덤프가 있는가다. 덤프는 dumps 가 고른다 - 때 확인과 회전이 같은 규칙을 본다.
fresh() {
	for n in $(dumps); do
		[ -n "$(find "$dir/$n" -prune -type f -mmin "-$1" 2>/dev/null)" ] && return 0
	done
	return 1
}

child=
part=
stop() {
	[ -n "$child" ] && kill "$child" 2>/dev/null
	[ -n "$part" ] && rm -f "$part"
	log "멈춘다"
	exit 143
}
trap stop TERM INT

# run 은 명령을 자식으로 돌리고 끝을 기다린다. 기다리는 동안 TERM 을 받으면 stop 이 자식을 끊는다.
run() {
	"$@" &
	child=$!
	wait "$child"
	runrc=$?
	child=
	return "$runrc"
}

prune() {
	dumps | tail -n "+$((keep + 1))" | while read -r old; do
		rm -f "$dir/$old" && log "보존 수 $keep 을 넘은 덤프를 지웠다: $old"
	done
}

# discard 는 이번 실행의 임시 파일을 지운다.
discard() {
	rm -f "$part"
	part=
}

dump_once() {
	final="$dir/fd-daily-$(date -u +%Y%m%dT%H%M%SZ).dump"
	if [ -e "$final" ]; then
		log "같은 이름의 덤프가 이미 있다: $(basename "$final")"
		return 0
	fi
	if ! part=$(mktemp "$final.part.XXXXXX"); then
		part=
		log "임시 파일을 못 만든다($dir) - 앞 덤프는 그대로 두고 ${interval}초 뒤에 다시 본다"
		return 1
	fi
	# -k: 기한에 보낸 TERM 뒤 10초에도 살아 있으면 KILL 이다.
	run timeout -k 10 "${deadline_s}s" \
		pg_dump --lock-wait-timeout="$lock_wait_ms" --format=custom --no-password --file="$part"
	dumprc=$?
	if [ "$dumprc" -eq 124 ]; then
		discard
		log "덤프가 기한(FD_DUMP_TIMEOUT, ${deadline_s}초)을 넘어 끊었다 - 백업 자리와 DB 가 응답하는지 보고, 정상 덤프가" \
			"그보다 오래 걸리는 원장이면 FD_DUMP_TIMEOUT 을 늘린다. 앞 덤프는 그대로 두고 ${interval}초 뒤에 다시 본다"
		return 1
	fi
	if [ "$dumprc" -ne 0 ]; then
		discard
		log "덤프 실패(종료 코드 $dumprc) - 앞 덤프는 그대로 두고 ${interval}초 뒤에 다시 본다"
		return 1
	fi
	if ! run pg_restore --list "$part" >/dev/null; then
		discard
		log "뜬 덤프를 pg_restore 가 못 읽는다 - 버리고 앞 덤프는 그대로 둔다"
		return 1
	fi
	if ! mv "$part" "$final"; then
		discard
		log "뜬 덤프의 이름을 못 바꿨다 - 버리고 앞 덤프는 그대로 둔다"
		return 1
	fi
	part=
	log "덤프를 떴다: $(basename "$final") ($(wc -c <"$final" | tr -d ' ') 바이트)"
	prune
}

# ensure_dir 는 덤프 자리를 만들고 이 uid 가 쓸 수 있는지 잰다. 못 쓰면 호스트에서 고치는 법을 알린다.
#
# ★ 호스트 자리가 없으면 Linux 의 Docker 가 root 소유 0755 로 만든다. 그러면 이 컨테이너(uid FD_UID)는 하위 자리를 못 만들어
#   첫 덤프부터 실패한다 - 원인이 권한이라는 것을 문구가 말해야 한다.
ensure_dir() {
	if mkdir -p "$dir" && [ -w "$dir" ] && [ -x "$dir" ]; then
		return 0
	fi
	log "덤프 자리 $dir 에 쓸 수 없다 - 이 컨테이너는 uid $(id -u):$(id -g) 로 돈다. 호스트의" \
		"백업 자리(compose 의 FD_BACKUP_HOST - 서버 마운트 밖)를 그 uid 소유 · 권한 0700 으로 만든다:" \
		"sudo install -d -m 0700 -o $(id -u) -g $(id -g) <호스트 자리>. 자리가 없으면 Docker 가 root 소유 0755 로 만든다 -" \
		"이미 그렇게 생겼으면 sudo chown -R $(id -u):$(id -g) <호스트 자리> 와 sudo chmod 0700 <호스트 자리>"
	return 1
}

# tick 은 자리의 잠금을 쥐고 한 번 본다: 고아 정리 · 때 확인 · 덤프 · 회전. 0 = 떴거나 때가 아니다, 1 = 실패,
# 3 = 다른 실행이 잠금을 쥐고 있다(아무것도 안 했다).
tick() {
	ensure_dir || return 1
	# command: 열기가 실패해도 셸이 끝나지 않고 거짓을 낸다(exec 의 리다이렉션 오류는 셸을 끝낸다).
	if ! command exec 9>>"$lockfile"; then
		log "잠금 파일을 못 연다($lockfile)"
		return 1
	fi
	flock -n 9
	lockrc=$?
	if [ "$lockrc" -ne 0 ]; then
		exec 9>&-
		if [ "$lockrc" -eq 1 ]; then
			log "다른 실행이 같은 자리($dir)에서 덤프하고 있다 - 이번에는 아무것도 안 한다"
			return 3
		fi
		log "잠금을 못 잡았다(flock 종료 코드 $lockrc)"
		return 1
	fi
	# 잠금 아래에서는 남은 임시 파일이 모두 고아다: 임시 파일을 쓰는 실행은 끝날 때까지 잠금을 쥔다.
	rm -f "$dir"/fd-daily-*.dump.part.*
	tickrc=0
	fresh "$every" || dump_once || tickrc=$?
	exec 9>&-
	return "$tickrc"
}

case $mode in
config)
	settings
	;;
check)
	fresh "$stale"
	exit $?
	;;
once)
	tick
	exit $?
	;;
loop)
	ensure_dir || exit 1
	log "시작한다: $(settings)"
	while :; do
		tick || :
		run sleep "$interval"
	done
	;;
*)
	bad "모르는 동작: $mode (loop · once · check · config)"
	;;
esac
