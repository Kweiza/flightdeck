#!/bin/sh
# flightdeck db-backup - Postgres 원장의 매일 덤프(pg_dump -Fc)와 보존 회전.
#
# compose 의 db-backup 서비스가 postgres 이미지 안에서 돌린다(서버와 같은 주판이라 pg_dump 가 서버보다 낮지 않다).
# 매시간 판단 원장(JSONL · journal.git, fd serve 안의 잡)과는 다른 겹이다: 그것은 여섯 표의 결정적 산출물이고, 이것은
# 원장 전체를 새 DB 로 되살리는 복원 수단이다.
#
#   sh db-backup.sh          끝없이 돈다: 때가 되면 덤프를 뜨고, CHECK 초마다 다시 본다(서비스의 기본).
#   sh db-backup.sh once     한 번만 본다: 때가 됐으면 뜨고 끝난다. 0 = 떴거나 아직 때가 아니다, 1 = 실패.
#   sh db-backup.sh check    가장 새 덤프가 STALE 분 안에 떠졌으면 0(컨테이너 헬스체크).
#
# 접속은 libpq 의 표준 환경 변수(PGHOST · PGPORT · PGDATABASE · PGUSER)로만 받는다. 비밀번호는 PGPASSFILE(pgpass 파일)로만
# 받는다 - 명령줄과 환경 변수에 비밀번호가 없다(ps 와 docker inspect 에 안 보인다). --no-password 라 파일이 없거나 틀리면
# 묻지 않고 실패한다.
#
# 설정(환경 변수):
#   FD_DB_BACKUP_DIR        덤프 자리(기본 /backups/daily). 판올림 전 덤프(/backups)와 다른 자리다 - 회전이 그것을 못 지운다.
#   FD_DB_BACKUP_KEEP       남길 덤프 수(기본 14, 재설계 스펙의 보존 규칙).
#   FD_DB_BACKUP_EVERY_MIN  덤프 간격(분, 기본 1440). 가장 새 덤프가 이보다 젊으면 안 뜬다 - 재시작이 덤프를 늘리지 않는다.
#   FD_DB_BACKUP_CHECK_SEC  다시 보는 간격(초, 기본 3600). 실패한 덤프는 이 간격 뒤에 다시 시도한다.
#   FD_DB_BACKUP_STALE_MIN  check 가 낡았다고 보는 나이(분, 기본 EVERY 의 두 배).
#
# ★ 회전은 덤프가 성공한 뒤에만 한다. 실패한 날에 지우면 접속이 끊긴 보름 뒤에는 덤프가 0개다.
# ★ 덤프는 .part 로 뜨고, pg_restore --list 로 읽히는 것을 본 뒤에 이름을 바꾼다. 반쯤 쓴 파일이 덤프 이름을 갖지 않는다.
# ★ 지우는 것은 이 스크립트가 짓는 이름(fd-daily-<UTC>.dump)뿐이다. 다른 파일은 자리에 있어도 세지도 지우지도 않는다.

set -u
umask 077

dir=${FD_DB_BACKUP_DIR:-/backups/daily}
keep=${FD_DB_BACKUP_KEEP:-14}
every=${FD_DB_BACKUP_EVERY_MIN:-1440}
interval=${FD_DB_BACKUP_CHECK_SEC:-3600}
stale=${FD_DB_BACKUP_STALE_MIN:-}

log() { printf '%s fd-db-backup: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

positive() {
	case $2 in
	'' | *[!0-9]*) log "$1 이 양의 정수가 아니다($2)"; exit 2 ;;
	esac
	if [ "$2" -lt 1 ]; then
		log "$1 이 양의 정수가 아니다($2)"
		exit 2
	fi
}
positive FD_DB_BACKUP_KEEP "$keep"
positive FD_DB_BACKUP_EVERY_MIN "$every"
positive FD_DB_BACKUP_CHECK_SEC "$interval"
[ -n "$stale" ] || stale=$((every * 2))
positive FD_DB_BACKUP_STALE_MIN "$stale"

# dumps 는 자리의 덤프 이름을 새것부터 낸다. 이름의 UTC 시각이 사전순 = 시간순이다.
dumps() {
	ls "$dir" 2>/dev/null | grep -E '^fd-daily-[0-9]{8}T[0-9]{6}Z\.dump$' | sort -r
}

# fresh 는 MIN 분보다 젊은 덤프가 있는가다.
fresh() {
	[ -n "$(find "$dir" -maxdepth 1 -type f -name 'fd-daily-*.dump' -mmin "-$1" 2>/dev/null | head -n 1)" ]
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
	rc=$?
	child=
	return "$rc"
}

prune() {
	dumps | tail -n "+$((keep + 1))" | while read -r old; do
		rm -f "$dir/$old" && log "보존 수 $keep 을 넘은 덤프를 지웠다: $old"
	done
}

dump_once() {
	final="$dir/fd-daily-$(date -u +%Y%m%dT%H%M%SZ).dump"
	if [ -e "$final" ]; then
		log "같은 이름의 덤프가 이미 있다: $(basename "$final")"
		return 0
	fi
	part="$final.part"
	if ! run pg_dump --format=custom --no-password --file="$part"; then
		rm -f "$part"
		part=
		log "덤프 실패 - 앞 덤프는 그대로 두고 ${interval}초 뒤에 다시 본다"
		return 1
	fi
	if ! run pg_restore --list "$part" >/dev/null; then
		rm -f "$part"
		part=
		log "뜬 덤프를 pg_restore 가 못 읽는다 - 버리고 앞 덤프는 그대로 둔다"
		return 1
	fi
	if ! mv "$part" "$final"; then
		rm -f "$part"
		part=
		log "뜬 덤프의 이름을 못 바꿨다 - 버리고 앞 덤프는 그대로 둔다"
		return 1
	fi
	part=
	log "덤프를 떴다: $(basename "$final") ($(wc -c <"$final" | tr -d ' ') 바이트)"
	prune
}

# tick 은 한 번 본다: 때가 됐으면 뜬다.
tick() {
	if fresh "$every"; then
		return 0
	fi
	dump_once
}

prepare() {
	if ! mkdir -p "$dir"; then
		log "덤프 자리를 못 만든다($dir) - 마운트한 호스트 자리의 주인이 이 컨테이너의 uid(FD_UID)인지 본다"
		exit 1
	fi
	# 앞 실행이 남긴 반쪽 파일. 이름이 덤프가 아니라 세지 않지만 자리를 먹는다.
	rm -f "$dir"/fd-daily-*.dump.part
}

case "${1:-loop}" in
check)
	fresh "$stale"
	exit $?
	;;
once)
	prepare
	tick
	exit $?
	;;
loop)
	prepare
	log "시작한다: 자리 $dir · 보존 $keep · 간격 ${every}분 · 확인 ${interval}초"
	while :; do
		tick || :
		run sleep "$interval"
	done
	;;
*)
	log "모르는 동작: $1 (loop · once · check)"
	exit 2
	;;
esac
