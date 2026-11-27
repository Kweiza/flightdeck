#!/bin/sh
# flightdeck db-roles - Postgres 원장의 접속 계정 둘을 맞춘다.
#
# compose.postgres.yaml 의 db-roles one-shot 이 postgres 이미지 안에서 `fd migrate` 뒤, 서버와 매일 덤프 앞에 돌린다. 초사용자로
# 소켓에 붙는다(PGHOST · PGPORT · PGUSER · PGDATABASE). 묶음 역할 fd_owner · fd_app 은 `fd migrate` 의 준비
# (server/internal/store/pg/bootstrap.sql)가 만들고, 이 스크립트는 그 위에 로그인 계정만 얹는다:
#
#   서버 계정(FD_PG_SERVER_USER, 기본 fd_server)   fd_app 의 구성원. 행을 읽고 쓰기만 한다.
#   덤프 계정(FD_PG_BACKUP_USER, 기본 fd_backup)   pg_read_all_data 의 구성원. 읽기만 한다.
#
#   sh db-roles.sh                         계정 둘을 맞춘다. 0 = 맞췄다, 1 = 거절 · 실패, 2 = 설정이 틀렸다.
#   sh db-roles.sh password <파일> <계정>   pgpass 파일에서 그 계정의 비밀번호를 낸다(아래 psql 이 부른다).
#
# 맞추는 것(한 트랜잭션):
#   - 없으면 만든다: LOGIN, 특권 속성 없이(SUPERUSER · CREATEDB · CREATEROLE · REPLICATION · BYPASSRLS 가 없다).
#   - 구성원 자격을 준다: 서버 계정 → fd_app, 덤프 계정 → pg_read_all_data.
#   - 잰다. 어긋나면 **고치지 않고 멈춘다** - 이미 있던 계정이 다르게 생겼으면 누가 왜 그렇게 만들었는지 사람이 봐야 한다:
#     특권 속성 · NOLOGIN, 위 하나 밖의 구성원 자격, 서버 계정이 fd_owner 로 바꿔 타거나 그 권한을 물려받는 것, 덤프 계정이
#     fd_app · fd_owner · pg_write_all_data 에 닿는 것, 두 계정을 구성원으로 둔 역할, 두 계정이 DB 의 주인인 것.
#   - 비밀번호를 pgpass 파일의 값으로 둔다(실행마다). 비밀번호를 바꾸려면 파일을 고치고 `up` 한다.
#
# ★ 비밀번호는 명령줄 · 환경 변수에 싣지 않는다(ps 와 docker inspect 에 보인다). psql 이 이 스크립트의 password 동작을
#   역따옴표로 불러 파이프로 받고, `ALTER ROLE … PASSWORD` 의 문자열로만 서버에 간다. 서버의 log_statement 가 ddl 이나 all
#   이면 그 문장이 로그에 남는다 - 그 설정을 켠 클러스터에서는 돌리지 마라.
#
# ★ pgpass 파일은 **libpq 와 pgx 가 똑같이 읽는 꼴만** 받는다. 같은 파일을 매일 덤프(libpq)와 서버(pgx)가 읽는데 두 구현의
#   해석이 다르다: pgx 는 줄 앞뒤 공백을 지우고 `\\` · `\:` 만 풀며 칸이 정확히 다섯이어야 하고, libpq 는 공백을 안 지우고
#   모든 역빗금을 풀며 비밀번호를 풀리지 않은 콜론에서 끊는다. 그래서 주석(`#` 로 시작)과 빈 줄 밖의 모든 줄이
#   「공백 · 역빗금 없이, 콜론 넷으로 다섯 칸, 비밀번호는 비지 않은 인쇄 가능한 ASCII」여야 한다. 어긋나면 줄 번호를 대고
#   멈춘다(비밀번호는 안 낸다). ASCII 밖의 비밀번호를 안 받는 까닭: SCRAM 의 SASLprep 정규화가 구현마다 다를 수 있다.
#   골라 쓰는 줄은 두 라이브러리와 같다 - 호스트 db · 포트 5432 · PGDATABASE · 계정에 맞는 첫 줄(`*` 는 아무 값).

set -u
LC_ALL=C
export LC_ALL

die() {
	printf 'fd-db-roles: %s\n' "$*" >&2
	exit 1
}
bad() {
	printf 'fd-db-roles: 설정이 틀렸다: %s\n' "$*" >&2
	exit 2
}

# pgpass_password 는 pgpass 파일 하나를 위의 꼴로 재고, 맞으면 「호스트 db · 포트 5432 · DB · 계정」에 맞는 첫 줄의 비밀번호를
# 낸다. 어긋난 줄이 하나라도 있으면(고른 줄이 아니어도) 파일 이름 · 줄 번호 · 사유를 stderr 에 내고 1 로 끝난다 - 읽는 쪽마다
# 다른 줄을 고를 수 있는 파일이기 때문이다. 맞는 줄이 없으면 3 이다.
pgpass_password() { # <파일> <DB> <계정>
	awk -v host=db -v port=5432 -v db="$2" -v user="$3" '
		function bad(why) { printf "fd-db-roles: %s:%d: %s\n", FILENAME, FNR, why > "/dev/stderr"; err = 1 }
		function hit(field, v) { return field == "*" || field == v }
		/^#/ || /^$/ { next }
		/[[:space:]]/ { bad("공백이 있다(libpq 는 그대로, pgx 는 지우고 읽는다)"); next }
		/\\/ { bad("역빗금이 있다(libpq 와 pgx 가 다르게 푼다)"); next }
		{
			n = split($0, f, ":")
			if (n != 5) { bad("칸이 다섯이 아니다(호스트:포트:DB:계정:비밀번호)"); next }
			if (f[5] == "") { bad("비밀번호가 비었다"); next }
			if (f[5] !~ /^[!-~]+$/) { bad("비밀번호에 인쇄 가능한 ASCII 밖의 바이트가 있다"); next }
			if (pw == "" && hit(f[1], host) && hit(f[2], port) && hit(f[3], db) && hit(f[4], user)) pw = f[5]
		}
		END {
			if (err) exit 1
			if (pw == "") exit 3
			print pw
		}' "$1"
}

# pgpass_check 는 파일을 읽을 수 있고, 남에게 열려 있지 않고(libpq 는 group · other 권한이 있으면 파일을 무시한다), 위의 꼴이며,
# 그 계정의 줄이 있는지 잰다. 비밀번호는 안 낸다.
pgpass_check() { # <변수 이름> <파일> <계정>
	[ -n "$2" ] || bad "$1 이 비었다"
	[ -f "$2" ] && [ -r "$2" ] || bad "$1($2)을 못 읽는다 - 호스트 파일의 주인이 FD_UID($(id -u))여야 한다"
	# 권한 글자(-rw-------)의 group · other 여섯 칸이 다 `-` 여야 한다. stat 의 인자는 GNU 와 BSD 가 달라 ls 로 읽는다.
	perm=$(ls -lnL -- "$2") || bad "$1($2)의 권한을 못 읽었다"
	case $perm in
	????------*) ;;
	*) bad "$1($2)의 권한이 ${perm%% *} 다 - 0600 이어야 한다(libpq 는 남에게 열린 pgpass 를 무시한다)" ;;
	esac
	pgpass_password "$2" "$PGDATABASE" "$3" >/dev/null
	case $? in
	0) ;;
	3) bad "$1($2)에 db:5432:$PGDATABASE:$3 에 맞는 줄이 없다" ;;
	*) bad "$1($2)의 꼴이 틀렸다(위 줄들)" ;;
	esac
}

# ident 는 따옴표 없이 쓸 수 있는 Postgres 이름만 받는다(소문자 · 숫자 · _, 글자나 _ 로 시작, 63바이트까지). URL · pgpass ·
# SQL 에 그대로 실려도 뜻이 안 바뀐다.
ident() { # <변수 이름> <값>
	case $2 in
	'' | [!a-z_]* | *[!a-z0-9_]*) bad "$1($2)는 소문자 · 숫자 · _ 만, 글자나 _ 로 시작해야 한다" ;;
	esac
	[ ${#2} -le 63 ] || bad "$1($2)가 63바이트를 넘는다"
}

if [ "${1:-}" = password ]; then
	[ $# -eq 3 ] || bad "쓰는 법: db-roles.sh password <파일> <계정>"
	pgpass_password "$2" "${PGDATABASE:-}" "$3"
	rc=$?
	[ $rc -eq 3 ] && printf 'fd-db-roles: %s 에 %s 의 줄이 없다\n' "$2" "$3" >&2
	exit $rc
fi
[ $# -eq 0 ] || bad "모르는 동작이다: $1"

: "${PGHOST:?}" "${PGPORT:?}" "${PGUSER:?}"
PGDATABASE=${PGDATABASE:-}
srv=${FD_PG_SERVER_USER:-}
bak=${FD_PG_BACKUP_USER:-}
srv_file=${FD_PG_SERVER_PGPASS:-}
bak_file=${FD_PG_BACKUP_PGPASS:-}

ident PGDATABASE "$PGDATABASE"
ident FD_PG_SERVER_USER "$srv"
ident FD_PG_BACKUP_USER "$bak"
[ "$srv" != "$bak" ] || bad "서버 계정과 덤프 계정이 같다($srv) - 서버가 덤프 계정의 권한을, 덤프가 쓰기 권한을 얻는다"
for u in "$srv" "$bak"; do
	case $u in
	postgres | public | none | fd_owner | fd_app | pg_*) bad "$u 는 계정 이름으로 못 쓴다(초사용자 · 예약 · 묶음 역할)" ;;
	esac
	[ "$u" != "$PGUSER" ] || bad "$u 는 이 스크립트가 붙는 초사용자다"
done
pgpass_check FD_PG_SERVER_PGPASS "$srv_file" "$srv"
pgpass_check FD_PG_BACKUP_PGPASS "$bak_file" "$bak"

# ★ 여기 문서(SQL)는 따옴표 친 heredoc 이다 - 셸이 아무것도 펼치지 않는다. 값은 psql 변수로만 들어가고(:'x' 는 문자열,
#   :"x" 는 이름으로 psql 이 따옴표를 친다), DO 블록 안에는 psql 변수가 안 펼쳐지므로 SET LOCAL 로 넘긴다.
# ★ 역따옴표 명령이 실패하면 psql 은 변수를 비운 채 다음 줄로 간다(ON_ERROR_STOP 이 거기 안 걸린다). 그래서 위
#   pgpass_check 가 같은 함수로 먼저 재고, 아래 DO 블록이 비밀번호 길이를 한 번 더 잰다.
if ! psql -X -q -v ON_ERROR_STOP=1 --no-password \
	-v self="$0" -v srv="$srv" -v bak="$bak" -v srv_file="$srv_file" -v bak_file="$bak_file" <<'SQL'
\set srv_pw `sh :'self' password :'srv_file' :'srv'`
\set bak_pw `sh :'self' password :'bak_file' :'bak'`
BEGIN;
SET LOCAL client_min_messages = warning;
SET LOCAL fd.srv = :'srv';
SET LOCAL fd.bak = :'bak';
SELECT set_config('fd.pw_len', least(length(:'srv_pw'), length(:'bak_pw'))::text, true) \gset fd_ignore_

-- 없으면 만든다. 「있으면 건너뛴다」를 조회로 하지 않고 예외로 받는다(bootstrap.sql 과 같은 까닭: 다른 실행이 같은 역할을
-- 만들고 아직 커밋하지 않았으면 그 커밋을 기다렸다가 unique_violation 이 난다).
DO $$
DECLARE
  r text;
BEGIN
  IF current_setting('fd.pw_len')::int = 0 THEN
    RAISE EXCEPTION 'pgpass 파일에서 비밀번호를 못 읽었다';
  END IF;
  FOREACH r IN ARRAY ARRAY[current_setting('fd.srv'), current_setting('fd.bak')] LOOP
    BEGIN
      EXECUTE format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', r);
    EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
    END;
  END LOOP;
END $$;

GRANT fd_app TO :"srv";
GRANT pg_read_all_data TO :"bak";

DO $$
DECLARE
  srv  constant regrole := current_setting('fd.srv')::regrole;
  bak  constant regrole := current_setting('fd.bak')::regrole;
  want constant regrole[] := ARRAY['fd_app'::regrole, 'pg_read_all_data'::regrole];
  bad  text[] := '{}';
  who  regrole;
  i    int;
BEGIN
  FOR i IN 1..2 LOOP
    who := (ARRAY[srv, bak])[i];
    bad := bad || ARRAY(
      SELECT format('%s: %s', who, a) FROM pg_roles r,
             unnest(ARRAY[CASE WHEN NOT rolcanlogin  THEN 'NOLOGIN'     END,
                          CASE WHEN rolsuper       THEN 'SUPERUSER'   END,
                          CASE WHEN rolcreaterole  THEN 'CREATEROLE'  END,
                          CASE WHEN rolcreatedb    THEN 'CREATEDB'    END,
                          CASE WHEN rolreplication THEN 'REPLICATION' END,
                          CASE WHEN rolbypassrls   THEN 'BYPASSRLS'   END]) a
       WHERE r.oid = who AND a IS NOT NULL);
    bad := bad || ARRAY(
      SELECT format('%s: %s 의 구성원이다', who, m.roleid::regrole)
        FROM pg_auth_members m WHERE m.member = who AND m.roleid <> want[i] ORDER BY 1);
    bad := bad || ARRAY(
      SELECT format('%s 를 %s 가 구성원으로 둔다', who, m.member::regrole)
        FROM pg_auth_members m WHERE m.roleid = who ORDER BY 1);
    IF (SELECT datdba FROM pg_database WHERE datname = current_database()) = who THEN
      bad := bad || format('%s: DB 의 주인이다', who);
    END IF;
  END LOOP;
  IF pg_has_role(srv, 'fd_owner', 'USAGE') OR pg_has_role(srv, 'fd_owner', 'SET') THEN
    bad := bad || format('%s: fd_owner 로 바꿔 타거나 그 권한을 물려받는다', srv);
  END IF;
  FOREACH who IN ARRAY ARRAY['fd_app'::regrole, 'fd_owner'::regrole, 'pg_write_all_data'::regrole] LOOP
    IF pg_has_role(bak, who, 'USAGE') OR pg_has_role(bak, who, 'SET') THEN
      bad := bad || format('%s: %s 에 닿는다', bak, who);
    END IF;
  END LOOP;
  IF cardinality(bad) > 0 THEN
    RAISE EXCEPTION '접속 계정이 이 배포의 약속과 다르다 - 고치지 않고 멈춘다: %', array_to_string(bad, '; ');
  END IF;
END $$;

ALTER ROLE :"srv" PASSWORD :'srv_pw';
ALTER ROLE :"bak" PASSWORD :'bak_pw';
COMMIT;
SQL
then
	die "계정을 못 맞췄다(위 psql 의 줄이 사유다). 아무것도 바뀌지 않았다"
fi
printf 'fd-db-roles: 맞췄다: %s(fd_app) · %s(pg_read_all_data), 비밀번호는 pgpass 파일의 값이다\n' "$srv" "$bak"
