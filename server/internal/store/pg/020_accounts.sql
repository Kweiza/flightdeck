-- 020 · 계정 · 기기 토큰 · 행위자 칸을 싣고 옛 행을 소유자 계정에 귀속한다 (schema_version 19 → 20)
--
-- ★ SQLite 증분 020(store/migrations/020_accounts.sql)과 같은 뜻이고 같은 판 번호다. 표 · 칸의 뜻, 귀속 규칙(소유자 계정을
--   언제 만드는가, judgment.via 를 어떻게 가르는가, 무엇을 안 채우는가)은 그 파일 머리말에 있다. Postgres 쪽 몫만 적는다:
--   - 소유 역할로 한 트랜잭션 안에서 돈다(engine.stepRole). 표의 주인이라 트리거를 끄고 켤 수 있고, 앱 역할은 그 어느 것도
--     못 한다(기준선의 「추가 전용」 절).
--   - 추가 전용 트리거 judgment_no_update · item_revision_no_update 를 귀속하는 동안만 끄고(ALTER TABLE … DISABLE TRIGGER)
--     같은 블록 끝에서 켠다. 블록 안 어디서 오류가 나도 트랜잭션이 통째로 되감겨 꺼진 채 남지 않는다.
--   - 바꾼 행 수를 대조한다. 표마다 UPDATE 앞에서 행 수를 재고 GET DIAGNOSTICS 의 ROW_COUNT 와 다르면 증분 전체를 되감는다.
--   - item_revision_no_update 는 갱신 금지 칸에 author 를 더해 다시 만든다(귀속보다 먼저 - 귀속은 그것을 끈 채 돈다).
--   - FK 는 기준선처럼 DEFERRABLE INITIALLY IMMEDIATE 다(옮기기 · 이관이 한 트랜잭션에서 순서 없이 쓸 때 미룰 수 있게).
--   - 새 표 넷의 앱 역할 권한을 여기서 준다(기준선 끝 GRANT 는 그때 있던 표에만 들었다). 함수는 더하지 않는다.

CREATE TABLE account (
  id            text PRIMARY KEY,
  login         text NOT NULL CHECK (login <> ''),
  display_name  text NOT NULL,
  email         text,
  role          text NOT NULL CHECK (role IN ('member','admin')),
  password_hash text,
  created_at    text NOT NULL,
  disabled_at   text,
  UNIQUE (login)
);

CREATE TABLE invite (
  id         text PRIMARY KEY,
  token_hash text NOT NULL,
  created_by text NOT NULL REFERENCES account(id) DEFERRABLE INITIALLY IMMEDIATE,
  role       text NOT NULL CHECK (role IN ('member','admin')),
  email      text,
  created_at text NOT NULL,
  expires_at text NOT NULL,
  used_at    text,
  used_by    text REFERENCES account(id) DEFERRABLE INITIALLY IMMEDIATE,
  revoked_at text,
  UNIQUE (token_hash),
  CHECK ((used_at IS NULL) = (used_by IS NULL)),
  CHECK (used_at IS NULL OR revoked_at IS NULL)
);

CREATE TABLE web_session (
  id_hash      text PRIMARY KEY,
  account_id   text NOT NULL REFERENCES account(id) DEFERRABLE INITIALLY IMMEDIATE,
  created_at   text NOT NULL,
  expires_at   text NOT NULL,
  last_seen_at text NOT NULL,
  user_agent   text
);

ALTER TABLE machine ADD COLUMN account_id text REFERENCES account(id) DEFERRABLE INITIALLY IMMEDIATE;
ALTER TABLE machine ADD COLUMN name text;
ALTER TABLE machine ADD COLUMN token_hash text;
ALTER TABLE machine ADD COLUMN token_prefix text;
ALTER TABLE machine ADD COLUMN token_origin text;
ALTER TABLE machine ADD COLUMN issued_at text;
ALTER TABLE machine ADD COLUMN revoked_at text;
ALTER TABLE machine ADD COLUMN revoked_by text REFERENCES account(id) DEFERRABLE INITIALLY IMMEDIATE;

CREATE UNIQUE INDEX machine_token_hash ON machine(token_hash);

CREATE TABLE device_code (
  code_hash      text PRIMARY KEY,
  user_code      text NOT NULL,
  machine_hint   text,
  requested_name text NOT NULL,
  requested_ip   text NOT NULL,
  requested_at   text NOT NULL,
  invite_id      text REFERENCES invite(id) DEFERRABLE INITIALLY IMMEDIATE,
  expires_at     text NOT NULL,
  approved_by    text REFERENCES account(id) DEFERRABLE INITIALLY IMMEDIATE,
  approved_at    text,
  issued_machine text REFERENCES machine(id) DEFERRABLE INITIALLY IMMEDIATE,
  UNIQUE (user_code),
  CHECK ((approved_by IS NULL) = (approved_at IS NULL)),
  CHECK (issued_machine IS NULL OR approved_by IS NOT NULL)
);

ALTER TABLE session ADD COLUMN account_id text REFERENCES account(id) DEFERRABLE INITIALLY IMMEDIATE;
ALTER TABLE item ADD COLUMN created_by text REFERENCES account(id) DEFERRABLE INITIALLY IMMEDIATE;
ALTER TABLE item ADD COLUMN created_session text REFERENCES session(id) DEFERRABLE INITIALLY IMMEDIATE;
ALTER TABLE judgment ADD COLUMN author text REFERENCES account(id) DEFERRABLE INITIALLY IMMEDIATE;
ALTER TABLE judgment ADD COLUMN via text CHECK (via IN ('session','web','cli'));
ALTER TABLE item_revision ADD COLUMN author text REFERENCES account(id) DEFERRABLE INITIALLY IMMEDIATE;

DROP TRIGGER item_revision_no_update ON item_revision;
CREATE TRIGGER item_revision_no_update
  BEFORE UPDATE OF item_id, rev, at, session_id, title, body, paths, reason, author ON item_revision
  FOR EACH ROW EXECUTE FUNCTION fd_append_only('item_revision 은 추가 전용이다 — 개정은 새 rev 로 쌓아라');

GRANT SELECT, INSERT, UPDATE, DELETE ON account, invite, web_session, device_code TO fd_app;

-- ── 옛 행 귀속 ───────────────────────────────────────────────────────────────

DO $attribution$
DECLARE
  want bigint;
  got  bigint;
BEGIN
  IF EXISTS (SELECT 1 FROM machine) OR EXISTS (SELECT 1 FROM session) OR EXISTS (SELECT 1 FROM item)
     OR EXISTS (SELECT 1 FROM judgment) OR EXISTS (SELECT 1 FROM item_revision) THEN
    INSERT INTO account(id, login, display_name, role, created_at)
    VALUES ('owner', 'owner', 'owner', 'admin', to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'));
  END IF;

  ALTER TABLE judgment DISABLE TRIGGER judgment_no_update;
  ALTER TABLE item_revision DISABLE TRIGGER item_revision_no_update;

  SELECT count(*) INTO want FROM machine;
  UPDATE machine SET account_id = 'owner', name = hostname WHERE account_id IS NULL;
  GET DIAGNOSTICS got = ROW_COUNT;
  IF got <> want THEN
    RAISE EXCEPTION '옛 행 귀속: machine 행 %개 중 %개만 바뀌었다', want, got;
  END IF;

  SELECT count(*) INTO want FROM session;
  UPDATE session SET account_id = 'owner' WHERE account_id IS NULL;
  GET DIAGNOSTICS got = ROW_COUNT;
  IF got <> want THEN
    RAISE EXCEPTION '옛 행 귀속: session 행 %개 중 %개만 바뀌었다', want, got;
  END IF;

  SELECT count(*) INTO want FROM item;
  UPDATE item SET created_by = 'owner' WHERE created_by IS NULL;
  GET DIAGNOSTICS got = ROW_COUNT;
  IF got <> want THEN
    RAISE EXCEPTION '옛 행 귀속: item 행 %개 중 %개만 바뀌었다', want, got;
  END IF;

  SELECT count(*) INTO want FROM judgment;
  UPDATE judgment SET author = 'owner',
    via = CASE WHEN session_id IS NOT NULL THEN 'session'
               WHEN substr(coalesce(title, ''), 1, 9) = '[fdboard]' THEN 'web'
               ELSE 'cli' END
  WHERE author IS NULL;
  GET DIAGNOSTICS got = ROW_COUNT;
  IF got <> want THEN
    RAISE EXCEPTION '옛 행 귀속: judgment 행 %개 중 %개만 바뀌었다', want, got;
  END IF;

  SELECT count(*) INTO want FROM item_revision;
  UPDATE item_revision SET author = 'owner' WHERE author IS NULL;
  GET DIAGNOSTICS got = ROW_COUNT;
  IF got <> want THEN
    RAISE EXCEPTION '옛 행 귀속: item_revision 행 %개 중 %개만 바뀌었다', want, got;
  END IF;

  ALTER TABLE judgment ENABLE TRIGGER judgment_no_update;
  ALTER TABLE item_revision ENABLE TRIGGER item_revision_no_update;
END
$attribution$;
