-- 018 · 세션이 받는 곳(inbox 소켓)을 원장에 싣는다 (schema_version 17 → 18)
--
-- ★ 무엇을 위한 표인가: 보드가 소유자 동작을 사람이 연 Claude Code 세션에 초인종 한 줄로 알린다
--   (board/README 「세션에 알림」). 지금까지 그 세션 목록은 보드 쪽 등록 폴더에 따로 살았고,
--   어느 세션이 어느 프로젝트인지를 폴더 접두로 다시 판정했다. 세션 · 프로젝트는 이 원장이 이미
--   git-common-dir 로 정해 두었으므로, 받는 곳만 여기 실으면 둘째 장부와 둘째 판정이 사라진다.
--
-- ★ session 표의 칼럼이 아니라 따로 둔다. 받는 곳은 세션의 좌표도 속성도 아니고 **배달 주소**다:
--   소켓 경로는 프로세스 id 로 지어져 재시작마다 바뀌고 다른 세션에 다시 쓰인다. session 칼럼으로
--   두면 판단 원장 백업(ledgerTables 의 session 칼럼 목록)에 실려 이 기계의 일시 경로가 원장 파일로
--   나간다. 이 표는 그 폐포 밖이다(signal 과 같다).
--
-- ★ 토큰은 칼럼이 없다. CLAUDE_CODE_MESSAGING_TOKEN 은 적지도 쓰지도 않는다(board/README).
--
-- ★ machine_id 를 session 에서 다시 적는 까닭: 한 기계 안에서 소켓은 한 살아 있는 세션의 것이다.
--   같은 (machine_id, socket) 의 옛 행은 쓰기 때 지운다(소켓 경로 재사용 · /clear 뒤 새 카드).
--   그 판정을 색인 하나로 하려고 둔다. session.machine_id 와 어긋날 길은 없다(쓰기가 거기서 읽는다).
--
-- ★ 순수 가산이다(CREATE TABLE · CREATE INDEX 뿐): migrate_guard_test.go 의 파괴 축에 안 걸린다.

CREATE TABLE session_inbox (
  session_id TEXT PRIMARY KEY REFERENCES session(id) ON DELETE CASCADE,  -- signal · footprint 와 같다: 카드와 함께 간다
  machine_id TEXT NOT NULL REFERENCES machine(id),
  socket     TEXT NOT NULL CHECK (length(socket) > 0),
  mode       TEXT NOT NULL,                  -- 받는 세션의 권한 모드(빈 값 = 모름)
  seen_at    TEXT NOT NULL                   -- 마지막으로 훅이 적은 때(사람이 볼 기록, 판정에는 안 씀)
);

-- UNIQUE: 「한 기계 · 한 소켓에 한 세션」을 코드(PutInbox)만이 아니라 스키마도 지킨다.
CREATE UNIQUE INDEX session_inbox_socket ON session_inbox(machine_id, socket);
