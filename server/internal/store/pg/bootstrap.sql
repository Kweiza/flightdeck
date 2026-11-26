-- flightdeck Postgres 준비 — 기준선(001_baseline.sql)보다 먼저, 초사용자가 DB 하나에 한 번 돌린다.
--
-- ★ 왜 기준선과 가르나: 여기 것은 소유 역할이 스스로 못 한다. 역할은 클러스터 전역이라 CREATEROLE 이
--   있어야 만들고, 스키마 public 과 DB 의 권한은 그 주인이 준다. 기준선은 소유 역할로 돈다 — 표의 주인이
--   소유 역할이어야 앱 역할과 권한이 갈린다.
--
-- ★ 몇 번 돌려도 같다. 역할이 이미 있으면 그대로 두고(맨 끝에서 잰다), GRANT · REVOKE · 확장은 원래 멱등이다.

-- 역할 둘. 둘 다 접속하지 않는 묶음 역할이다 — 접속 계정은 배치(compose)가 만들어 이 둘에 넣는다.
--   fd_owner : 표 · 함수 · 트리거의 주인. 스키마를 바꾸는 이전 실행기가 이 역할로 돈다.
--   fd_app   : 서버. 행을 읽고 쓰기만 한다(DML). 표를 만들고 고치고 비우지 못한다.
-- 「있으면 건너뛴다」를 조회로 하지 않고 예외로 받는다. 이미 커밋된 역할이면 duplicate_object 가 나고,
-- 다른 DB 의 준비가 같은 역할을 만들고 아직 커밋하지 않았으면 그 커밋을 기다렸다가 pg_authid 의 유니크
-- 위반(unique_violation)이 난다. 둘 다 「이미 있다」다 — 두 DB 를 동시에 준비해도 둘 다 성공한다.
DO $$
BEGIN
  BEGIN
    CREATE ROLE fd_owner NOLOGIN;
  EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
  END;
  BEGIN
    CREATE ROLE fd_app NOLOGIN;
  EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
  END;
END $$;

-- 스키마 public 에 무엇을 만들 수 있는 것은 소유 역할뿐이다. PG15 부터 PUBLIC 의 CREATE 가 기본으로
-- 빠져 있지만, 앱 역할이 스키마를 못 바꾼다는 것이 이 판의 계약이라 기본값에 기대지 않고 적는다.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO fd_owner;
GRANT USAGE ON SCHEMA public TO fd_app;

-- ★ 임시 표를 못 만들게 한다. PUBLIC 은 DB 마다 TEMPORARY 를 기본으로 받는다. 표 이름을 찾을 때 pg_temp 가
--   search_path 맨 앞에 끼므로, 원장 표와 같은 이름의 임시 표 하나가 그 연결의 읽기 · 쓰기를 원장 대신
--   받는다. DB 단위 권한이라 DB 이름을 문장 안에서 읽는다(복제한 DB 는 이 권한을 물려받지 않는다).
DO $$
BEGIN
  EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
END $$;

-- 판단 전문 검색(pg_trgm 부분 일치)의 확장. 기준선의 judgment 식 인덱스가 이것의 연산자 클래스를 쓴다.
-- ★ DB 의 LC_CTYPE 이 `C` 면 한글 trigram 이 0개라 검색이 조용히 0건이 된다 — 로케일은 DB 를 만들 때
--   정한다(initdb 인자). 이 파일이 못 고치는 축이라 서버가 기동 때 따로 잰다.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ★ 이미 있던 앱 역할을 그대로 쓰기 전에 잰다. 앱 역할이 소유 역할로 바꿔 탈 수 있거나(구성원 자격) 권한
--   검사를 건너뛰는 속성이 있으면, 위 GRANT 와 기준선의 권한 나눔이 겉으로만 선다. 고쳐 쓰지 않고 멈춘다 —
--   누가 왜 그렇게 만들었는지는 사람이 봐야 한다. 재는 것:
--   - 속성: SUPERUSER · CREATEROLE · CREATEDB · REPLICATION · BYPASSRLS 가 없다.
--   - 구성원 자격: 앱 역할은 어느 역할의 구성원도 아니다(소유 역할도, pg_write_all_data 같은 미리 만든 역할도).
--   - 앱 역할을 쓰는 역할(접속 계정)이 소유 역할로 바꿔 타거나 그 권한을 물려받지 못한다. 구성원 사슬을 끝까지
--     따라간다 — 중간 그룹을 거쳐 앱 역할을 쓰는 계정도 앱이다. pg_has_role 을 모든 역할에 돌리지 않고 사슬을
--     따라가는 까닭: 초사용자는 pg_has_role 이 늘 참이라, 준비를 돌리는 초사용자 자신이 걸린다.
--   - 이 DB 에 스키마 · 임시 표를, 스키마 public 에 객체를 못 만든다(DB 주인이면 여기서 걸린다).
DO $$
DECLARE
  app  constant regrole := 'fd_app';
  bad  text[];
BEGIN
  SELECT array_remove(ARRAY[
           CASE WHEN rolsuper       THEN 'SUPERUSER'   END,
           CASE WHEN rolcreaterole  THEN 'CREATEROLE'  END,
           CASE WHEN rolcreatedb    THEN 'CREATEDB'    END,
           CASE WHEN rolreplication THEN 'REPLICATION' END,
           CASE WHEN rolbypassrls   THEN 'BYPASSRLS'   END], NULL)
    INTO bad FROM pg_roles WHERE oid = app;
  bad := bad || ARRAY(SELECT format('%s 의 구성원', m.roleid::regrole)
                        FROM pg_auth_members m WHERE m.member = app ORDER BY 1);
  bad := bad || ARRAY(WITH RECURSIVE users(oid) AS (
                          SELECT m.member FROM pg_auth_members m WHERE m.roleid = app
                          UNION
                          SELECT m.member FROM pg_auth_members m JOIN users u ON m.roleid = u.oid)
                      SELECT format('%s 가 fd_app 을 쓰면서 fd_owner 로도 바꿔 탄다', u.oid::regrole)
                        FROM users u
                       WHERE pg_has_role(u.oid, 'fd_owner', 'SET') OR pg_has_role(u.oid, 'fd_owner', 'USAGE')
                       ORDER BY 1);
  IF has_database_privilege(app, current_database(), 'CREATE') THEN bad := bad || 'DB 의 CREATE'::text; END IF;
  IF has_database_privilege(app, current_database(), 'TEMPORARY') THEN bad := bad || 'DB 의 TEMPORARY'::text; END IF;
  IF has_schema_privilege(app, 'public', 'CREATE') THEN bad := bad || '스키마 public 의 CREATE'::text; END IF;
  IF cardinality(bad) > 0 THEN
    RAISE EXCEPTION USING
      ERRCODE = 'object_not_in_prerequisite_state',
      MESSAGE = format('앱 역할 %s 가 행만 읽고 쓰는 역할이 아니다: %s', app, array_to_string(bad, ' · ')),
      HINT    = '역할을 고치거나 지운 뒤 다시 준비하라. 준비는 이미 있는 역할을 고쳐 쓰지 않는다.';
  END IF;
END $$;
