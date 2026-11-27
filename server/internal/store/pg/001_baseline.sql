-- flightdeck Postgres 기준선 스키마 — SQLite 판 18 과 같은 뜻
--
-- ★ 무엇: SQLite 의 store/schema.sql(판 1) 위에 증분 002~018 을 다 얹은 모양을 이 한 파일로 접었다.
--   이 파일이 만드는 판은 18 이다(pg.BaselineVersion). 이 파일은 그 판에서 얼린다 - 그 위는 pg.Steps 의 증분(019 부터)이
--   얹고, 옮기는 동안은 SQLite 증분과 같은 뜻 · 같은 판 번호다. SQLite 쪽에 증분이 오르고 PG 짝이 없으면 store 의
--   시험(TestPGStepsFollowTheSQLiteVersion)이 그 자리에서 빨개진다.
--
-- ★ 어떻게 도나: 준비(bootstrap.sql, 초사용자)를 마친 빈 DB 에서 **소유 역할(fd_owner)로** 돈다.
--   표 · 함수 · 트리거의 주인이 소유 역할이어야 앱 역할(fd_app)이 맨 끝의 GRANT 만 받는다.
--
-- ★ 같은 뜻을 무엇으로 재나: 표 이름 · 칼럼(이름 · 순서 · 타입 · NOT NULL · 기본값) · 키 · FK · 인덱스 ·
--   CHECK 수 · 추가 전용 트리거를 SQLite 의 살아 있는 스키마와 맞대는 시험이 pg 패키지에 있다. 같은 거절을
--   두 엔진에 돌려 보는 시험도 함께 있다(부분 유니크 · CHECK · 추가 전용 · FK).
--
-- 계층이 곧 쓰기 권한이다. 제약 하나하나가 실제로 났던 사고를 막는다 — 제약을 지우려면 그 사고가 왜
-- 재발하지 않는지부터 답해야 한다. 각 사고의 긴 기록은 SQLite 의 schema.sql · 증분 머리말에 있다.
--
--   D 계층 — 파생.  서버(git 리더)와 러너만 쓴다. 사람용 쓰기 엔드포인트가 없다.
--   Q 계층 — 큐.    사람은 title·body·reason 만 쓴다. 나머지는 서버가 채운다.
--   J 계층 — 판단.  사람만 쓴다. 추가 전용(UPDATE·DELETE·TRUNCATE 를 트리거가 막는다).
--
-- 옮긴 규칙(SQLite → PG):
--   INTEGER                         → bigint. SQLite 정수는 64비트다.
--   INTEGER PRIMARY KEY AUTOINCREMENT → bigint GENERATED ALWAYS AS IDENTITY. ALWAYS 인 까닭: 손으로 넣은 id 는
--                                     시퀀스를 안 올려 다음 자동 번호가 기본키 충돌로 죽는다(SQLite 의
--                                     AUTOINCREMENT 는 넣은 값 뒤로 번호를 올린다). 그래서 id 를 적은 INSERT 를
--                                     거절한다. 옛 id 를 그대로 옮기는 이관만 소유 역할로 OVERRIDING SYSTEM
--                                     VALUE 를 붙여 넣고 setval 로 번호를 맞춘다. ★ 그 구절은 INSERT 권한만
--                                     보므로 앱 역할이 일부러 쓰면 못 막는다 — 막는 것은 실수다.
--   시각 TEXT                       → text. 고정 폭 UTC 문자열(2006-01-02T15:04:05.000000Z)이라 사전순이
--                                     곧 시간순이고, SQL 안에 시각 계산이 없다. timestamptz 는 이 판 밖이다.
--   JSON TEXT                       → text. jsonb 가 아니다 — 바이트가 그대로여야 판단 원장 내보내기와
--                                     멱등 재생이 같은 바이트를 낸다.
--   BLOB                            → bytea.
--   (a IS NOT NULL) + (b …) = 1     → num_nonnulls(a, b, …) = 1. PG 는 불리언을 더하지 못한다.
--   RAISE(ABORT) 트리거             → 함수 fd_append_only() 하나 + 행 트리거(UPDATE · DELETE) +
--                                     TRUNCATE 문 트리거. PG 의 TRUNCATE 는 행 트리거를 건너뛴다.
--   PRAGMA defer_foreign_keys       → FK 를 전부 DEFERRABLE INITIALLY IMMEDIATE 로 선언한다. 평소에는
--                                     지금처럼 문장마다 검사하고, 미루는 자리(항목 옮기기 · 원장 되쓰기)는
--                                     SET CONSTRAINTS ALL DEFERRED 로 커밋까지 미룬다. pragma 가 FK 전부를
--                                     미루므로 「전부 DEFERRABLE」이 같은 뜻이다.
--   item_after 의 rowid             → seq 칼럼(이 표의 행 정체). 이관이 rowid 값을 그대로 넣는다(위와 같은 길).
--   snapshot 의 키 (project, key)   → (project, key_digest). key 원문은 길이 제한이 없는데 B-tree 항목은
--                                     약 2.7KiB 를 못 넘는다. 키의 지문(sha256)을 키로 두고, 지문이 같은 다른
--                                     키는 넣기 전에 거절해 「키 하나에 행 하나」를 지킨다.
--   FTS5 judgment_fts + 넣기 트리거 → 없다. judgment 의 pg_trgm GIN 식 인덱스가 그 자리다.
--   item_dependents(011 이 걷음)    → 없다.
--   (SQLite 에 없음)                → 트리거 셋을 더한다. claim_one_live_holder: 살아 있는 선점을 upsert 가
--                                     덮지 못한다(SQLite 는 쓰기가 한 줄로 서서 코드의 판정이 지킨다).
--                                     item_revision_moves_with_item: 이력의 project 는 항목과 함께만 옮긴다.
--                                     snapshot_key_digest_unique: 지문이 같은 다른 키가 남의 행을 덮지 못한다.
--
-- ★ 하나 더 엄격해진 자리: SQLite 는 정수가 아닌 PRIMARY KEY 칼럼에 NULL 을 받는다(엔진의 옛 버릇).
--   PG 는 PRIMARY KEY 가 곧 NOT NULL 이다. 코드가 키에 NULL 을 넣는 길은 없다 — 이관의 사전 검사가 잰다.

CREATE TABLE schema_version (
  version    bigint NOT NULL,
  applied_at text   NOT NULL
);

-- ═══════════════════════════════════════════════════════════════════════════
-- 설정
-- ═══════════════════════════════════════════════════════════════════════════

CREATE TABLE project (
  id              text PRIMARY KEY,
  path            text NOT NULL,           -- 서버 머신에서의 절대경로
  remote_url      text,
  default_branch  text NOT NULL DEFAULT 'main',
  config          text,                    -- .flightdeck.yaml 의 캐시(JSON). 정본은 레포 안의 파일
  config_from_sha text,                    -- 그 캐시를 읽어 온 커밋
  created_at      text NOT NULL,
  pinned_at       text,                    -- 증분 007 · 표시 축. NULL 이 「아님」이다
  archived_at     text                     -- 증분 007
);

CREATE TABLE machine (
  id         text PRIMARY KEY,             -- 클라이언트가 생성해 로컬에 보관하는 안정 id
  hostname   text NOT NULL,
  first_seen text NOT NULL,
  last_seen  text NOT NULL
);

-- ═══════════════════════════════════════════════════════════════════════════
-- D 계층 · 파생
-- ═══════════════════════════════════════════════════════════════════════════

CREATE TABLE session (
  id            text PRIMARY KEY,                 -- 서버 발급 ULID
  project       text NOT NULL REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  machine_id    text NOT NULL REFERENCES machine(id) DEFERRABLE INITIALLY IMMEDIATE,
  worktree      text NOT NULL,
  cc_session_id text NOT NULL,
  label         text,                             -- 표시 전용. 어떤 필터의 축도 아니다
  state         text NOT NULL DEFAULT 'active'
                CHECK (state IN ('active','paused','blocked','done')),
  blocked_why   text,
  opened_at     text NOT NULL,
  harness       text,                             -- 증분 013

  -- ★ 3중키. 워크트리 경로는 규율상 재사용되고, 접두 일치가 없어야 조상 트리의 등록을 물려받지 않는다
  --   (마커를 물려받아 남의 절을 덮어쓴 사고, 원문 영구 소실).
  UNIQUE (machine_id, worktree, cc_session_id),

  -- 공허한 단정 금지: 막혔다고만 쓰고 왜인지 안 남기는 것을 막는다
  CHECK (state <> 'blocked' OR (blocked_why IS NOT NULL AND blocked_why <> ''))
);
-- ★ ended · pid 칼럼이 없다. 채울 수 없는 칼럼은 거짓으로 채워지고, pid 死로 산 세션을 죽었다고 판정한
--   사고가 있었다.

CREATE INDEX session_by_project ON session(project, state);

CREATE TABLE session_workspace (
  session_id text NOT NULL REFERENCES session(id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
  project    text NOT NULL REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  path       text NOT NULL,
  is_primary bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (session_id, path)
);

-- 생존 「사실」이지 판정이 아니다. ★ 종류별 현재값 한 행이지 이력이 아니다 — 간격 · 분포의 재료는
-- event(kind='session.beat') 뿐이다(store/signal_is_not_history_test.go).
CREATE TABLE signal (
  session_id text NOT NULL REFERENCES session(id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
  kind       text NOT NULL CHECK (kind IN ('prompt','tool','mcp','commit','push')),
  at         text NOT NULL,
  PRIMARY KEY (session_id, kind)
);

-- 미커밋 발자국. ★ origin 을 뭉개지 않는다 — 「선언했으나 안 건드림」과 「선언 없이 건드림」이 갈려야
-- 「겹침 없음」과 「이 축을 안 본다」가 갈린다.
CREATE TABLE footprint (
  session_id text NOT NULL REFERENCES session(id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
  path       text NOT NULL,
  origin     text NOT NULL CHECK (origin IN ('observed','declared','claimed')),
  first_at   text NOT NULL,
  last_at    text NOT NULL,
  PRIMARY KEY (session_id, path, origin)
);

CREATE INDEX footprint_by_path ON footprint(path);

CREATE TABLE ref_state (
  project text NOT NULL REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  ref     text NOT NULL,
  sha     text NOT NULL,
  subject text,
  at      text NOT NULL,                -- 이 값을 관측한 시각
  PRIMARY KEY (project, ref)
);

-- ★ 변경 시점에 계산해 불변으로 보관한다. 브랜치가 지워져도 이 행은 남는다.
CREATE TABLE change_set (
  project     text NOT NULL REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  base_sha    text NOT NULL,
  head_sha    text NOT NULL,
  paths       text NOT NULL,            -- JSON 배열
  computed_at text NOT NULL,
  PRIMARY KEY (project, base_sha, head_sha)
);

-- ═══════════════════════════════════════════════════════════════════════════
-- Q 계층 · 큐
-- ═══════════════════════════════════════════════════════════════════════════

CREATE TABLE item (
  project      text NOT NULL REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  id           text NOT NULL,           -- 사람이 정한다. 브랜치 이름으로도 쓰인다
  title        text NOT NULL,
  body         text NOT NULL,
  paths        text NOT NULL DEFAULT '[]',   -- JSON 배열
  labels       text NOT NULL DEFAULT '[]',   -- JSON 배열. ★ 표시 전용 — 어떤 배제 판정에도 안 쓴다
  state        text NOT NULL DEFAULT 'open'
               CHECK (state IN ('open','claimed','done','dropped')),
  close_reason text,
  landed_ref   text,                    -- ★ 러너가 실제로 fast-forward 한 sha 만 들어간다
  created_at   text NOT NULL,
  closed_at    text,
  PRIMARY KEY (project, id),

  -- 사유 없는 폐기는 나중에 되짚을 수 없다
  CHECK (state <> 'dropped' OR (close_reason IS NOT NULL AND close_reason <> ''))
);

CREATE INDEX item_by_state ON item(project, state, created_at);

CREATE TABLE item_after (
  project     text NOT NULL,
  item_id     text NOT NULL,
  dep_item    text,                     -- 미랜딩 선행: 항목 id
  dep_job     text,                     -- 진행 중 잡 (Tier B)
  dep_sha     text,                     -- 랜딩된 것: 커밋 sha
  dep_project text,                     -- 증분 015 · NULL = 같은 프로젝트. FK 가 없다(선행은 아직 없을 수 있다)
  seq         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,  -- SQLite rowid 의 자리. 순서의 축이다
  FOREIGN KEY (project, item_id) REFERENCES item(project, id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,

  -- ★ 브랜치 이름을 담을 칼럼이 없다 — 랜딩 뒤 브랜치가 지워지면 판정이 깨지므로 쓸 수 없게 만들어 막는다.
  CHECK (num_nonnulls(dep_item, dep_job, dep_sha) = 1)
);

CREATE INDEX item_after_by_item ON item_after(project, item_id);

-- ★ 만료 칼럼이 없다. 자동 반납 · 자동 회수가 없다 — 생존 판정의 정본이 없어 「만료 = 죽음」이 서지 않는다.
CREATE TABLE claim (
  project      text NOT NULL,
  item_id      text NOT NULL,
  session_id   text NOT NULL REFERENCES session(id) DEFERRABLE INITIALLY IMMEDIATE,
  at           text NOT NULL,
  released_at  text,
  force_reason text,                    -- 남의 선점을 뺏었다면 사유
  PRIMARY KEY (project, item_id),
  FOREIGN KEY (project, item_id) REFERENCES item(project, id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE
);

-- ★ 살아 있는 선점은 남에게 넘어가지 않는다 — 마지막 방어. 반납된 선점 행이 키 자리에 남아 있어 선점은
--   INSERT … ON CONFLICT DO UPDATE SET session_id = … 로 쓴다. 그 문장은 행이 살아 있어도 덮는다. SQLite 는
--   BEGIN IMMEDIATE 가 「점유자 확인 → 쓰기」 사이를 닫지만, PG 의 READ COMMITTED 에서는 두 연결이 같이
--   「점유자 없음」을 보고 둘 다 upsert 하면 늦은 쪽이 앞 쪽의 선점을 말없이 덮는다.
--   그래서 released_at 이 비어 있는 행의 session_id 는 못 바꾼다. 회수는 released_at 을 먼저 찍으므로 지난다.
--   SQLSTATE 는 23505: resource_one_holder · landing_queue_one_live_per_session 과 같은 「살아 있는 것은
--   하나」 축이다. 제약 이름 자리에 트리거 이름을 싣는다.
CREATE FUNCTION fd_claim_one_live_holder() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION USING
    ERRCODE    = 'unique_violation',
    MESSAGE    = format('살아 있는 선점은 넘어가지 않는다(항목 %s/%s · 점유 세션 %s) — 회수(released_at)가 먼저다',
                        OLD.project, OLD.item_id, OLD.session_id),
    TABLE      = TG_TABLE_NAME,
    CONSTRAINT = TG_NAME;
END
$$;

CREATE TRIGGER claim_one_live_holder BEFORE UPDATE ON claim
  FOR EACH ROW WHEN (OLD.released_at IS NULL AND NEW.session_id IS DISTINCT FROM OLD.session_id)
  EXECUTE FUNCTION fd_claim_one_live_holder();

-- 추천 1건과 탈락 사유 전부를 남긴다. 사유가 없으면 큐는 블랙박스가 된다.
CREATE TABLE pick_eval (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project     text NOT NULL,
  session_id  text NOT NULL,
  at          text NOT NULL,
  picked      text,                     -- 묶음 **선두**의 항목 id 또는 NULL(적격 0건)
  rejected    text NOT NULL,            -- JSON: [{item, reason_code, detail}]
  picked_with text                      -- 증분 004 · NULL 은 「단독이었다」
);

-- ═══════════════════════════════════════════════════════════════════════════
-- J 계층 · 판단 — 사람이 쓰는 유일한 것. 추가 전용.
-- ═══════════════════════════════════════════════════════════════════════════

CREATE TABLE judgment (
  id         text PRIMARY KEY,
  project    text REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  session_id text REFERENCES session(id) DEFERRABLE INITIALLY IMMEDIATE,
  at         text NOT NULL,
  kind       text NOT NULL CHECK (kind IN (
               'handoff','decision','blocked','ask','now','rejected','not-done','verified','draft')),
  title      text,
  body       text NOT NULL,
  supersedes text REFERENCES judgment(id) DEFERRABLE INITIALLY IMMEDIATE,
  CHECK (body <> '')
);

CREATE INDEX judgment_by_session ON judgment(session_id, at);
CREATE INDEX judgment_by_kind ON judgment(project, kind, at);

-- 전문 검색. FTS5 의 자리다. 검색은 이 식과 **글자 그대로 같은 식**에 ILIKE 를 걸어야 이 인덱스를 탄다.
-- 2글자 질의는 trigram 이 없어 순차 훑기가 된다(판단 수천 건에서는 문제가 아니다).
CREATE INDEX judgment_text_trgm ON judgment
  USING gin ((coalesce(title, '') || ' ' || body) gin_trgm_ops);

-- 판단 ↔ 항목·잡·커밋. 포인터가 FK 가 되면 끊긴 포인터가 원리적으로 사라진다.
CREATE TABLE judgment_link (
  judgment_id    text NOT NULL REFERENCES judgment(id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
  target_kind    text NOT NULL CHECK (target_kind IN ('item','job','commit','session')),
  target_id      text NOT NULL,
  target_project text,                  -- 증분 009 · NULL 은 judgment.project 로 읽는다
  PRIMARY KEY (judgment_id, target_kind, target_id)
);

CREATE INDEX judgment_link_by_target ON judgment_link(target_kind, target_id);

-- 키 원문의 지문(sha256). convert_to 는 STABLE 로 표시돼 있어(변환 함수를 카탈로그에서 찾는다) 계산 칼럼에
-- 바로 못 쓴다. DB 의 인코딩은 만든 뒤 안 바뀌므로 같은 원문은 늘 같은 바이트다 — 그래서 IMMUTABLE 로 감싼다.
-- 본문을 SQL 표준 꼴(RETURN)로 써서 이름을 만들 때 묶는다(부를 때의 search_path 를 안 탄다).
CREATE FUNCTION fd_key_digest(key text) RETURNS bytea
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN sha256(convert_to(key, 'UTF8'));

-- 재계산이 불가능한 수치. ★ 손으로 넣으려면 근거를 함께 넣어야 한다.
CREATE TABLE snapshot (
  project      text NOT NULL REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  key          text NOT NULL,
  value        text NOT NULL,
  method       text NOT NULL CHECK (method IN ('command','manual')),
  evidence     text,                    -- method='manual' 이면 필수
  input_digest text,
  computed_at  text NOT NULL,
  -- ★ 키의 지문. key 원문은 길이 제한이 없고(SQLite 가 받는다) B-tree 항목은 약 2.7KiB 를 못 넘어, 원문을
  --   키로 두면 긴 키가 PG 에서만 거절된다. 같은 원문이면 같은 지문이다. 지문이 같은 **다른** 원문은 아래
  --   트리거가 넣기 전에 거절한다 — 그래서 「프로젝트 안에서 키 하나에 행 하나」가 그대로다.
  --   계산 칼럼이라 누구도 손으로 적지 못한다.
  --   ★ upsert 의 충돌 대상은 (project, key_digest) 다 — (project, key) 에는 유니크 인덱스가 없다.
  key_digest   bytea GENERATED ALWAYS AS (fd_key_digest(key)) STORED,
  PRIMARY KEY (project, key_digest),
  CHECK (method <> 'manual' OR (evidence IS NOT NULL AND evidence <> ''))
);

-- ★ 지문이 겹쳐도 다른 키가 남의 행을 덮지 않는다. 기본키가 지문이라, 지문이 같은 다른 키의 upsert 는 충돌
--   갈래(DO UPDATE)로 가서 남의 값을 말없이 덮는다 — 그 갈래의 UPDATE 트리거는 들어오던 키를 못 본다.
--   BEFORE INSERT 는 upsert 의 충돌 판정보다 먼저 돌므로, 여기서 같은 지문 · 다른 원문의 행을 보면 거절한다.
--   sha256 이 겹칠 일은 없다고 봐도 되지만, 겹치면 조용히 덮지 않고 드러나게 한다(두 연결이 겹치는 두 키를
--   같은 순간 처음 넣는 경합은 못 본다 — 겹침 위에 경합이 하나 더 있어야 나는 자리다).
CREATE FUNCTION fd_snapshot_key_digest_unique() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM public.snapshot s
              WHERE s.project = NEW.project AND s.key_digest = public.fd_key_digest(NEW.key) AND s.key <> NEW.key) THEN
    RAISE EXCEPTION USING
      ERRCODE    = 'integrity_constraint_violation',
      MESSAGE    = format('snapshot 키의 지문이 다른 키와 겹친다(프로젝트 %s) — 다른 키의 행을 덮지 않는다', NEW.project),
      TABLE      = TG_TABLE_NAME,
      CONSTRAINT = TG_NAME;
  END IF;
  RETURN NEW;
END
$$;

CREATE TRIGGER snapshot_key_digest_unique BEFORE INSERT ON snapshot
  FOR EACH ROW EXECUTE FUNCTION fd_snapshot_key_digest_unique();

-- ═══════════════════════════════════════════════════════════════════════════
-- 자원 · 계측
-- ═══════════════════════════════════════════════════════════════════════════

-- 락이 원리적으로 못 지키는 논리 카운터(발번).
CREATE TABLE counter (
  project text NOT NULL REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  name    text NOT NULL,
  value   bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (project, name)
);

CREATE TABLE resource_hold (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project      text NOT NULL REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  resource     text NOT NULL,
  session_id   text REFERENCES session(id) DEFERRABLE INITIALLY IMMEDIATE,
  job_id       text,
  acquired_at  text NOT NULL,
  released_at  text,
  force_reason text,
  CHECK (num_nonnulls(session_id, job_id) = 1)
);

-- ★ 배타를 애플리케이션 판정이 아니라 DB 제약으로 만든다. 판정을 앱에서 빼면 우회할 코드도 없다.
CREATE UNIQUE INDEX resource_one_holder
  ON resource_hold(project, resource) WHERE released_at IS NULL;

-- Tier B. 지금은 안 쓰지만 item.landed_ref·item_after.dep_job 이 이 표를 가리키므로 함께 둔다.
CREATE TABLE job (
  id         text PRIMARY KEY,
  project    text NOT NULL REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  kind       text NOT NULL CHECK (kind IN ('land','exec')),
  item_id    text,
  session_id text REFERENCES session(id) DEFERRABLE INITIALLY IMMEDIATE,
  branch     text,
  recipe     text,
  state      text NOT NULL DEFAULT 'queued'
             CHECK (state IN ('queued','running','ok','fail','stalled','bypassed')),
  fail_kind  text CHECK (fail_kind IS NULL OR fail_kind IN (
               'conflict','verify','seed','raced','infra','timeout','admission','push')),
  queued_at  text NOT NULL,
  started_at text,
  ended_at   text,
  beat_at    text,
  base_sha   text,
  head_sha   text,
  landed_sha text,
  image_tag  text,
  ack_at     text,                      -- ★ NULL = 미확인. 결과의 주인은 세션이 아니라 항목이다
  log_tail   text
);

CREATE INDEX job_by_state ON job(project, state, queued_at);
CREATE INDEX job_unacked ON job(project, ack_at) WHERE ack_at IS NULL;

-- 추가 전용 감사·계측 원장. ★ id 순서를 삽입(커밋) 순서로 읽는 자리가 있다 — 넣는 쪽이 전역 잠금 안에서
-- 넣어야 그 순서가 선다(저장 계층의 몫).
CREATE TABLE event (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at         text NOT NULL,
  project    text,
  session_id text,
  kind       text NOT NULL,
  payload    text NOT NULL DEFAULT '{}'
);

CREATE INDEX event_by_kind ON event(kind, at);
CREATE INDEX event_by_session ON event(session_id, at);

-- 증분 002 · 멱등 기록. ★ 5xx 는 저장하지 않는다 — 일시 장애를 영구 응답으로 굳히지 않게 제약으로 둔다.
CREATE TABLE idempotency (
  key         text PRIMARY KEY,         -- Idempotency-Key 원문
  fingerprint text NOT NULL,            -- method+path+body 의 해시. 키 재사용 탐지의 유일한 축
  status      bigint NOT NULL,
  ctype       text NOT NULL,
  body        bytea NOT NULL,           -- 응답 본문 원문. 재생은 바이트가 같아야 한다
  at          text NOT NULL,            -- TTL 청소의 축
  CHECK (status >= 100 AND status < 500)
);

CREATE INDEX idempotency_by_at ON idempotency(at);

-- 증분 003 · 랜딩 순서 큐. ★ id 가 곧 순번이다. 「레인을 쥐었나」는 resource_hold 가 정본이다.
CREATE TABLE landing_queue (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,   -- 이것이 순번이다
  project     text NOT NULL REFERENCES project(id) DEFERRABLE INITIALLY IMMEDIATE,
  session_id  text NOT NULL REFERENCES session(id) DEFERRABLE INITIALLY IMMEDIATE,
  enqueued_at text NOT NULL,
  left_at     text,
  left_kind   text,        -- ok | fail | leave | finish | force
  left_detail text,

  -- 빠진 시각과 빠진 종류는 함께 있거나 함께 없다
  CHECK ((left_at IS NULL) = (left_kind IS NULL)),
  -- ★ 사유 없는 회수는 되짚을 수 없다. ok·finish 만 면제된다
  CHECK (left_kind IS NULL
         OR left_kind IN ('ok','finish')
         OR (left_detail IS NOT NULL AND left_detail <> '')),
  -- ★ 종류는 다섯뿐이다. Go 쪽 ValidateLandingLeave 가 1차 방어이고 이것이 최종 방어다
  CHECK (left_kind IS NULL
         OR left_kind IN ('ok','fail','leave','finish','force'))
);

-- ★ 한 세션은 살아 있는 줄 행을 하나만 가진다. 재진입이 줄을 두 자리 차지하면 순번이 거짓이 된다.
CREATE UNIQUE INDEX landing_queue_one_live_per_session
  ON landing_queue(project, session_id) WHERE left_at IS NULL;

CREATE INDEX landing_queue_waiting
  ON landing_queue(project, id) WHERE left_at IS NULL;

-- 증분 008 · 줄 행이 자원 집합을 갖는다. 취득이 all-or-nothing 이라 자원별 left_at 이 없다.
CREATE TABLE landing_queue_resource (
  row_id   bigint NOT NULL REFERENCES landing_queue(id) DEFERRABLE INITIALLY IMMEDIATE,
  resource text NOT NULL,
  PRIMARY KEY (row_id, resource),
  CHECK (resource <> '')
);

CREATE INDEX landing_queue_resource_by_name
  ON landing_queue_resource(resource, row_id);

-- 증분 014 · 루트가 멤버 프로젝트 명부를 갖는다. 파일이 정본이고 이 표는 그 캐시다.
CREATE TABLE project_member (
  root_project   text NOT NULL REFERENCES project(id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
  member_project text NOT NULL,
  path           text NOT NULL,         -- 루트 상대 경로, 슬래시 구분
  declared_at    text NOT NULL,
  PRIMARY KEY (root_project, member_project)
);

CREATE INDEX project_member_by_member ON project_member(member_project);

-- 증분 016 · 항목 개정 이력. 추가 전용(아래 트리거). title · body · paths 는 **고치기 직전의** 값이다.
CREATE TABLE item_revision (
  project    text NOT NULL,
  item_id    text NOT NULL,
  rev        bigint NOT NULL,           -- 1부터. 이 항목에서 몇 번째 개정인가
  at         text NOT NULL,
  session_id text REFERENCES session(id) DEFERRABLE INITIALLY IMMEDIATE,
  title      text NOT NULL,
  body       text NOT NULL,
  paths      text NOT NULL,             -- JSON 배열
  reason     text NOT NULL CHECK (reason <> ''),
  PRIMARY KEY (project, item_id, rev),
  FOREIGN KEY (project, item_id) REFERENCES item(project, id) DEFERRABLE INITIALLY IMMEDIATE
);

-- 증분 018 · 세션이 받는 곳(배달 주소). 소켓 경로는 재시작마다 바뀌어 session 칼럼으로 두지 않는다.
CREATE TABLE session_inbox (
  session_id text PRIMARY KEY REFERENCES session(id) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE,
  machine_id text NOT NULL REFERENCES machine(id) DEFERRABLE INITIALLY IMMEDIATE,
  socket     text NOT NULL CHECK (length(socket) > 0),
  mode       text NOT NULL,             -- 받는 세션의 권한 모드(빈 값 = 모름)
  seen_at    text NOT NULL
);

-- 「한 기계 · 한 소켓에 한 세션」을 코드만이 아니라 스키마도 지킨다.
CREATE UNIQUE INDEX session_inbox_socket ON session_inbox(machine_id, socket);

-- ═══════════════════════════════════════════════════════════════════════════
-- 추가 전용 — judgment · event · item_revision
-- ═══════════════════════════════════════════════════════════════════════════
--
-- ★ 규율이 아니라 물리로. 같은 파일을 두 세션이 써 앞 세션의 절이 통째로 덮였고 원문이 영구 소실됐다.
--   두 번 났다.
--
-- 거절 하나를 함수 하나가 낸다. 문구는 트리거마다 인자로 받는다(SQLite 트리거의 문구 그대로).
-- SQLSTATE 는 23000(integrity_constraint_violation): SQLite 의 트리거 ABORT 처럼 「불변식 위반」 축이고,
-- 유니크 · FK · CHECK(23505 · 23503 · 23514) 어느 것과도 안 겹친다. 제약 이름 자리에 트리거 이름을 싣는다.
-- ★ 막는 줄은 셋이다(UPDATE · DELETE 행 트리거, TRUNCATE 문 트리거). 앱 역할에게는 TRUNCATE 권한부터 없다
--   (맨 끝 GRANT). 트리거는 주인의 DML · TRUNCATE 에도 선다 — 그러나 **DDL 은 못 막는다**: 주인은 트리거를
--   끄거나 지울 수 있고(ALTER TABLE … DISABLE TRIGGER · DROP TRIGGER), ALTER COLUMN … TYPE … USING 으로
--   행을 다시 쓰는 것도 행 트리거를 안 탄다. 주인은 믿는 이전 실행기이고, 앱 역할은 그 어느 것도 못 한다.

CREATE FUNCTION fd_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION USING
    ERRCODE    = 'integrity_constraint_violation',
    MESSAGE    = TG_ARGV[0],
    TABLE      = TG_TABLE_NAME,
    CONSTRAINT = TG_NAME;
END
$$;

CREATE TRIGGER judgment_no_update BEFORE UPDATE ON judgment
  FOR EACH ROW EXECUTE FUNCTION fd_append_only('judgment 는 추가 전용이다 — 정정은 새 행 + supersedes 로 남겨라');
CREATE TRIGGER judgment_no_delete BEFORE DELETE ON judgment
  FOR EACH ROW EXECUTE FUNCTION fd_append_only('judgment 는 삭제할 수 없다');
CREATE TRIGGER judgment_no_truncate BEFORE TRUNCATE ON judgment
  FOR EACH STATEMENT EXECUTE FUNCTION fd_append_only('judgment 는 비울 수 없다 — 추가 전용이다');

CREATE TRIGGER event_no_update BEFORE UPDATE ON event
  FOR EACH ROW EXECUTE FUNCTION fd_append_only('event 는 추가 전용이다');
CREATE TRIGGER event_no_delete BEFORE DELETE ON event
  FOR EACH ROW EXECUTE FUNCTION fd_append_only('event 는 삭제할 수 없다');
CREATE TRIGGER event_no_truncate BEFORE TRUNCATE ON event
  FOR EACH STATEMENT EXECUTE FUNCTION fd_append_only('event 는 비울 수 없다 — 추가 전용이다');

-- ★ 증분 017: project 는 개정의 내용이 아니라 항목이 지금 어느 프로젝트에 있는가라는 좌표라, 항목을
--   옮길 때 함께 바뀐다. 그래서 갱신 금지를 내용 칼럼으로 좁힌다(item_id 는 옮겨도 안 바뀐다).
CREATE TRIGGER item_revision_no_update
  BEFORE UPDATE OF item_id, rev, at, session_id, title, body, paths, reason ON item_revision
  FOR EACH ROW EXECUTE FUNCTION fd_append_only('item_revision 은 추가 전용이다 — 개정은 새 rev 로 쌓아라');
CREATE TRIGGER item_revision_no_delete BEFORE DELETE ON item_revision
  FOR EACH ROW EXECUTE FUNCTION fd_append_only('item_revision 은 추가 전용이다 — 지우면 복구 경로가 0이 된다');
CREATE TRIGGER item_revision_no_truncate BEFORE TRUNCATE ON item_revision
  FOR EACH STATEMENT EXECUTE FUNCTION fd_append_only('item_revision 은 비울 수 없다 — 추가 전용이다');

-- ★ project 가 바뀌는 것은 항목이 옮겨 갈 때뿐이다. FK 는 옮긴 곳에 같은 id 의 항목이 **있는지**만 보므로,
--   항목은 그대로 두고 이력만 옮기면 옮긴 곳에 마침 같은 id 의 다른 항목이 있을 때 남의 이력이 된다.
--   그래서 커밋 때 옛 자리(OLD.project, item_id)에 항목이 없는지 본다 — 옮기기는 항목 행과 딸린 행을 한
--   트랜잭션에서 순서 없이 바꾸므로(FK 를 미룬다) 문장 끝이 아니라 커밋까지 미룬다.
--   표 이름을 스키마로 못박는다: 같은 이름의 임시 표가 가리지 못하게.
CREATE FUNCTION fd_item_revision_moves_with_item() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM public.item WHERE project = OLD.project AND id = OLD.item_id) THEN
    RAISE EXCEPTION USING
      ERRCODE    = 'integrity_constraint_violation',
      MESSAGE    = format('item_revision 의 project 는 항목을 옮길 때만 바뀐다 — 옛 자리 %s/%s 에 항목이 남아 있다',
                          OLD.project, OLD.item_id),
      TABLE      = TG_TABLE_NAME,
      CONSTRAINT = TG_NAME;
  END IF;
  RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER item_revision_moves_with_item AFTER UPDATE OF project ON item_revision
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW WHEN (OLD.project IS DISTINCT FROM NEW.project)
  EXECUTE FUNCTION fd_item_revision_moves_with_item();

-- ═══════════════════════════════════════════════════════════════════════════
-- 앱 역할의 권한 — 행을 읽고 쓰기만 한다
-- ═══════════════════════════════════════════════════════════════════════════
--
-- ★ 주지 않는 것: TRUNCATE · REFERENCES · TRIGGER(어느 표에도), 스키마 변경(주인이 아니고 스키마에 CREATE 가
--   없다), 시퀀스 권한(identity 칼럼의 번호는 INSERT 권한만으로 받는다 — setval 은 이관 몫이라 소유 역할이 한다).
-- ★ schema_version 은 읽기만 한다. 판을 적는 것은 이전 실행기(소유 역할)다.
-- ★ 표를 더하는 증분은 그 표의 GRANT 를 함께 싣는다 — 빠뜨리면 pg 패키지의 권한 시험이 빨개진다.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO fd_app;
REVOKE INSERT, UPDATE, DELETE ON schema_version FROM fd_app;
