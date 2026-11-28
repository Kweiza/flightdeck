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
-- ★ 새 표의 기본키에 NOT NULL 을 적는다. SQLite 는 INTEGER 가 아닌 PRIMARY KEY 에 NULL 을 받는다(옛 판과의 호환으로 남긴
--   동작) - 적지 않으면 키를 뺀 INSERT 가 몇 번이고 들어간다. Postgres 는 기본키가 NOT NULL 을 품는다.
--
-- ★ 옛 행 귀속. 업무 행이 하나라도 있는 원장이면 소유자 계정(id 'owner', 역할 admin, 암호 없음)을 만들고 옛 행 전부를 그 계정에
--   단다: 기기 · 세션의 계정, 항목의 작성자, 판단 · 개정의 작성자. 빈 원장(schema_version 밖에 행이 하나도 없다)에는 만들지
--   않는다 - 이관 · 역이관의 대상(빈 원장)이 원본의 계정 행을 그대로 받게.
--   - 「업무 행이 있다」는 뿌리 표 여섯(project · machine · judgment · pick_eval · event · idempotency) 가운데 하나에 행이 있다는
--     뜻이다. 판 19 의 나머지 표는 모두 NOT NULL FK 로 이 여섯 가운데 하나에 매달려 있어 따로 안 본다
--     (TestMigration020OwnerRootsHoldEveryOtherTable).
--   - 계정의 created_at 은 그 여섯 표의 시각 칸(project.created_at · machine.first_seen · judgment.at · pick_eval.at · event.at ·
--     idempotency.at) 가운데 가장 이른 값이다. 지금 시각을 쓰면 같은 판 19 원장을 따로 올린 둘이 대조에서 갈린다. 바이트 순으로
--     고른다(Postgres 는 COLLATE "C") - 두 엔진이 같은 값을 고르게. 그래서 SQLite 는 min 전에 값을 글(CAST … AS TEXT)로 통일한다:
--     저장형이 섞이면 SQLite 의 min 은 저장형 순(INTEGER · REAL < TEXT < BLOB)으로 먼저 갈라, 1월 BLOB 과 2월 TEXT 에서 2월을
--     고른다. 이관은 그 BLOB 을 Postgres 의 글로 옮기므로 Postgres 는 1월을 고른다. 여섯 표가 모두 비면 그 값이 NULL 이고, 그것이
--     곧 「계정을 안 만든다」다(조건과 값이 한 식이다).
--   - 암호 없는 계정은 로그인할 수 없다. 암호를 다는 길은 관리자가 내는 재설정 링크뿐이고(그 화면은 관리자 화면 단계다),
--     「암호가 없다」를 「먼저 온 사람이 받는다」로 읽는 자리는 없다 - 원장 백업 되쓰기는
--     password_hash 를 빼고 돌아오므로 복원된 관리자도 암호 없는 계정이다. 기기 토큰의 유무도 token_hash 로만 가른다
--     (token_prefix · issued_at 은 표시 칸이고, 백업 되쓰기는 token_hash 만 빼고 그 둘을 싣는다).
--   - 판단의 via: 세션이 있으면 'session', 세션 없이 제목 머리가 `[fdboard]` 면 'web'(보드가 세션 없이 쓴 소유자 노트 -
--     board/board.py 의 is_owner_note 와 같은 규칙), 그 밖의 세션 없는 판단은 'cli'.
--   - 기기 이름은 hostname 에서 온다(토큰 없는 옛 기기).
--   - item.created_session 은 채우지 않는다. 어느 세션이 만들었는지는 event(item.add · item.followup_created)에 있다 - 옮겨
--     적으면 같은 사실이 두 벌이다.
--   - judgment 와 item_revision 은 추가 전용이다. 같은 트랜잭션 안에서 갱신 금지 트리거를 지우고 칸을 채운 뒤 같은 글로 다시
--     만든다(SQLite 에는 트리거 끄기가 없다 - 트랜잭션이 되감기면 지운 것도 되돌아온다). item_revision 의 트리거는 다시 만들 때
--     갱신 금지 칸에 author 를 더한다.
--   - 갱신 금지는 판단 · 개정의 작성자 칸(judgment.author · via, item_revision.author)에만 듣는다 - 추가 전용 표의 칸이라 그 표의
--     트리거가 덮는다. machine.account_id · session.account_id · item.created_by · created_session 은 갱신 금지가 아니다: 그
--     표들은 추가 전용이 아니고(상태 · 옮기기 · 지우기를 앱이 쓴다), 그 칸을 언제 누가 쓰는지는 쓰기가 주체를 토큰에서 읽는
--     단계가 정한다.
--   - 바꾼 행 수를 대조한다. 표마다 UPDATE 바로 뒤에 changes() 를 그 표의 행 수와 함께 적고, 둘이 다르면 CHECK 가 증분 전체를
--     되감는다(트리거의 RAISE(IGNORE) 같은 것이 행을 말없이 건너뛰면 여기서 선다). 대조 표는 이 트랜잭션에서 만든 TEMP 표이고
--     끝에서 지운다.
-- ★ 되돌리기: 표 넷과 칸을 더하기만 하고 기존 칸 값은 안 바꾼다. 귀속은 새 칸에만 쓰므로 그 칸을 비우면 이 증분 이전의 값과
--   같다(migrate_guard_test.go 의 destructiveExempt[20]). 판올림 전 VACUUM INTO 백업도 뜬다.

CREATE TABLE account (
  id            TEXT NOT NULL PRIMARY KEY,
  login         TEXT NOT NULL CHECK (login <> ''),
  display_name  TEXT NOT NULL,
  email         TEXT,
  role          TEXT NOT NULL CHECK (role IN ('member','admin')),
  password_hash TEXT,                     -- argon2id. NULL 은 「로그인할 수 없다」(재설정 링크로만 단다, 머리말)
  created_at    TEXT NOT NULL,
  disabled_at   TEXT,
  UNIQUE (login)
);

CREATE TABLE invite (
  id         TEXT NOT NULL PRIMARY KEY,
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
  id_hash      TEXT NOT NULL PRIMARY KEY,
  account_id   TEXT NOT NULL REFERENCES account(id),
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  user_agent   TEXT
);

ALTER TABLE machine ADD COLUMN account_id TEXT REFERENCES account(id);
ALTER TABLE machine ADD COLUMN name TEXT;
ALTER TABLE machine ADD COLUMN token_hash TEXT;      -- 기기 토큰의 sha256. NULL 은 「토큰 없는 기기」(유무는 이 칸으로만 가른다)
ALTER TABLE machine ADD COLUMN token_prefix TEXT;
ALTER TABLE machine ADD COLUMN token_origin TEXT;    -- 토큰을 낸 서버 주소(scheme://host[:port])
ALTER TABLE machine ADD COLUMN issued_at TEXT;
ALTER TABLE machine ADD COLUMN revoked_at TEXT;
ALTER TABLE machine ADD COLUMN revoked_by TEXT REFERENCES account(id);

CREATE UNIQUE INDEX machine_token_hash ON machine(token_hash);

-- 기기 코드 흐름(`fd login`) 한 번. code_hash 는 CLI 가 폴링에 쓰는 코드의 sha256, user_code 는 사람이 승인 화면에 치는 코드다.
CREATE TABLE device_code (
  code_hash      TEXT NOT NULL PRIMARY KEY,
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
SELECT 'owner', 'owner', 'owner', 'admin', first FROM (
  SELECT min(at) AS first FROM (
              SELECT min(CAST(created_at AS TEXT)) AS at FROM project
    UNION ALL SELECT min(CAST(first_seen AS TEXT)) FROM machine
    UNION ALL SELECT min(CAST(at AS TEXT)) FROM judgment
    UNION ALL SELECT min(CAST(at AS TEXT)) FROM pick_eval
    UNION ALL SELECT min(CAST(at AS TEXT)) FROM event
    UNION ALL SELECT min(CAST(at AS TEXT)) FROM idempotency))
WHERE first IS NOT NULL;

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
