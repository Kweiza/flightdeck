-- 020 · 계정 · 기기 토큰 · 행위자 칸을 싣고 옛 행을 소유자 계정에 귀속한다 (schema_version 19 → 20)
--
-- ★ 무엇: 계정 표 넷(account · invite · web_session · device_code), machine 넓힘(계정 · 이름 · 토큰 지문 · 토큰 주소 · 철회),
--   사람 칸(session.account_id, item.created_by · created_session, judgment.author · via, item_revision.author). Postgres 증분
--   020(store/pg/020_accounts.sql)과 같은 뜻 · 같은 판 번호다.
--
-- ★ 칸은 NULL 을 받는다. 지금 판의 쓰기는 아직 행위자를 모른다(쓰기의 주체를 토큰에서 읽는 것은 다음 단계다). NOT NULL 은 모든
--   쓰기가 주체를 실은 뒤의 증분이 단다 - 지금 달면 그 전의 쓰기가 전부 거절된다.
--
-- ★ 토큰 · 암호 원문은 어디에도 없다. *_hash 칸은 sha256(16진) 또는 argon2id 문자열이고, token_prefix 는 화면에서 기기를 가리는
--   앞 8글자뿐이다.
--
-- ★ machine.last_seen_at 을 따로 두지 않는다. 지금 칸 last_seen 이 「이 기기를 마지막으로 본 때」이고, 토큰으로 들어온 요청도 그
--   기기를 본 것이다 - 칸이 둘이면 둘이 갈린다.
--
-- ★ 옛 행 귀속. 행이 하나라도 있는 원장이면 소유자 계정(id 'owner', 역할 admin, 암호 없음)을 만들고 옛 행 전부를 그 계정에
--   단다: 기기 · 세션의 계정, 항목의 작성자, 판단 · 개정의 작성자. 그 계정은 첫 관리자가 받는다(암호를 다는 쪽은 관리자 화면
--   단계다). 빈 원장에는 만들지 않는다 - 이관 · 역이관의 대상(빈 원장)이 원본의 계정 행을 그대로 받게.
--   - 판단의 via: 세션이 있으면 'session', 세션 없이 제목 머리가 `[fdboard]` 면 'web'(보드가 세션 없이 쓴 소유자 노트 -
--     board/board.py 의 is_owner_note 와 같은 규칙), 그 밖의 세션 없는 판단은 'cli'.
--   - 기기 이름은 hostname 에서 온다(토큰 없는 옛 기기).
--   - item.created_session 은 채우지 않는다. 어느 세션이 만들었는지는 event(item.add · item.followup_created)에 있다 - 옮겨
--     적으면 같은 사실이 두 벌이다.
--   - judgment 와 item_revision 은 추가 전용이다. 같은 트랜잭션 안에서 갱신 금지 트리거를 지우고 칸을 채운 뒤 같은 글로 다시
--     만든다(SQLite 에는 트리거 끄기가 없다 - 트랜잭션이 되감기면 지운 것도 되돌아온다). item_revision 의 트리거는 다시 만들 때
--     갱신 금지 칸에 author 를 더한다.
--   - 바꾼 행 수를 대조한다. 표마다 UPDATE 바로 뒤에 changes() 를 그 표의 행 수와 함께 적고, 둘이 다르면 CHECK 가 증분 전체를
--     되감는다(트리거의 RAISE(IGNORE) 같은 것이 행을 말없이 건너뛰면 여기서 선다). 대조 표는 이 트랜잭션에서 만든 TEMP 표이고
--     끝에서 지운다.
-- ★ 되돌리기: 표 넷과 칸을 더하기만 하고 기존 칸 값은 안 바꾼다. 귀속은 새 칸에만 쓰므로 그 칸을 비우면 이 증분 이전의 값과
--   같다(migrate_guard_test.go 의 destructiveExempt[20]). 판올림 전 VACUUM INTO 백업도 뜬다.

CREATE TABLE account (
  id            TEXT PRIMARY KEY,
  login         TEXT NOT NULL CHECK (login <> ''),
  display_name  TEXT NOT NULL,
  email         TEXT,
  role          TEXT NOT NULL CHECK (role IN ('member','admin')),
  password_hash TEXT,                     -- argon2id. NULL 은 「아직 암호가 없다」(옛 원장의 소유자 계정)
  created_at    TEXT NOT NULL,
  disabled_at   TEXT,
  UNIQUE (login)
);

CREATE TABLE invite (
  id         TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL,               -- 초대 링크 토큰의 sha256
  created_by TEXT NOT NULL REFERENCES account(id),
  role       TEXT NOT NULL CHECK (role IN ('member','admin')),
  email      TEXT,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at    TEXT,
  used_by    TEXT REFERENCES account(id),
  revoked_at TEXT,
  UNIQUE (token_hash),
  CHECK ((used_at IS NULL) = (used_by IS NULL)),
  CHECK (used_at IS NULL OR revoked_at IS NULL)
);

-- 웹 로그인 쿠키 하나. id_hash 는 쿠키 값의 sha256 이다.
CREATE TABLE web_session (
  id_hash      TEXT PRIMARY KEY,
  account_id   TEXT NOT NULL REFERENCES account(id),
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  user_agent   TEXT
);

ALTER TABLE machine ADD COLUMN account_id TEXT REFERENCES account(id);
ALTER TABLE machine ADD COLUMN name TEXT;
ALTER TABLE machine ADD COLUMN token_hash TEXT;      -- 기기 토큰의 sha256. NULL 은 「토큰 없는 기기」
ALTER TABLE machine ADD COLUMN token_prefix TEXT;
ALTER TABLE machine ADD COLUMN token_origin TEXT;    -- 토큰을 낸 서버 주소(scheme://host[:port])
ALTER TABLE machine ADD COLUMN issued_at TEXT;
ALTER TABLE machine ADD COLUMN revoked_at TEXT;
ALTER TABLE machine ADD COLUMN revoked_by TEXT REFERENCES account(id);

CREATE UNIQUE INDEX machine_token_hash ON machine(token_hash);

-- 기기 코드 흐름(`fd login`) 한 번. code_hash 는 CLI 가 폴링에 쓰는 코드의 sha256, user_code 는 사람이 승인 화면에 치는 코드다.
CREATE TABLE device_code (
  code_hash      TEXT PRIMARY KEY,
  user_code      TEXT NOT NULL,
  machine_hint   TEXT,                    -- 클라이언트가 보낸 옛 machine-id(같은 계정일 때만 그 행에 토큰을 단다). FK 가 없다
  requested_name TEXT NOT NULL,
  requested_ip   TEXT NOT NULL,
  requested_at   TEXT NOT NULL,
  invite_id      TEXT REFERENCES invite(id),   -- 초대 가입 흐름에서 만든 것만. 자동 승인은 이 초대에만 듣는다
  expires_at     TEXT NOT NULL,
  approved_by    TEXT REFERENCES account(id),
  approved_at    TEXT,
  issued_machine TEXT REFERENCES machine(id),
  UNIQUE (user_code),
  CHECK ((approved_by IS NULL) = (approved_at IS NULL)),
  CHECK (issued_machine IS NULL OR approved_by IS NOT NULL)
);

ALTER TABLE session ADD COLUMN account_id TEXT REFERENCES account(id);
ALTER TABLE item ADD COLUMN created_by TEXT REFERENCES account(id);
ALTER TABLE item ADD COLUMN created_session TEXT REFERENCES session(id);
ALTER TABLE judgment ADD COLUMN author TEXT REFERENCES account(id);
ALTER TABLE judgment ADD COLUMN via TEXT CHECK (via IN ('session','web','cli'));
ALTER TABLE item_revision ADD COLUMN author TEXT REFERENCES account(id);

-- ── 옛 행 귀속 ───────────────────────────────────────────────────────────────

INSERT INTO account(id, login, display_name, role, created_at)
SELECT 'owner', 'owner', 'owner', 'admin', strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000Z'
WHERE EXISTS (SELECT 1 FROM machine) OR EXISTS (SELECT 1 FROM session) OR EXISTS (SELECT 1 FROM item)
   OR EXISTS (SELECT 1 FROM judgment) OR EXISTS (SELECT 1 FROM item_revision);

CREATE TEMP TABLE attribution_count (
  tbl  TEXT PRIMARY KEY,
  want INTEGER NOT NULL,                  -- 표의 행 수(옛 행 전부가 귀속 대상이다)
  got  INTEGER NOT NULL,                  -- 바로 앞 UPDATE 가 바꾼 행 수
  CHECK (got = want)
);

UPDATE machine SET account_id = 'owner', name = hostname WHERE account_id IS NULL;
INSERT INTO attribution_count(tbl, want, got) VALUES ('machine', (SELECT count(*) FROM machine), changes());

UPDATE session SET account_id = 'owner' WHERE account_id IS NULL;
INSERT INTO attribution_count(tbl, want, got) VALUES ('session', (SELECT count(*) FROM session), changes());

UPDATE item SET created_by = 'owner' WHERE created_by IS NULL;
INSERT INTO attribution_count(tbl, want, got) VALUES ('item', (SELECT count(*) FROM item), changes());

DROP TRIGGER judgment_no_update;
UPDATE judgment SET author = 'owner',
  via = CASE WHEN session_id IS NOT NULL THEN 'session'
             WHEN substr(coalesce(title, ''), 1, 9) = '[fdboard]' THEN 'web'
             ELSE 'cli' END
WHERE author IS NULL;
INSERT INTO attribution_count(tbl, want, got) VALUES ('judgment', (SELECT count(*) FROM judgment), changes());
CREATE TRIGGER judgment_no_update BEFORE UPDATE ON judgment
BEGIN
  SELECT RAISE(ABORT, 'judgment 는 추가 전용이다 — 정정은 새 행 + supersedes 로 남겨라');
END;

DROP TRIGGER item_revision_no_update;
UPDATE item_revision SET author = 'owner' WHERE author IS NULL;
INSERT INTO attribution_count(tbl, want, got) VALUES ('item_revision', (SELECT count(*) FROM item_revision), changes());
CREATE TRIGGER item_revision_no_update
BEFORE UPDATE OF item_id, rev, at, session_id, title, body, paths, reason, author ON item_revision
BEGIN SELECT RAISE(ABORT, 'item_revision 은 추가 전용이다 — 개정은 새 rev 로 쌓아라'); END;

DROP TABLE temp.attribution_count;
