-- 019 · event 를 (project, kind, id) 로 찾는다 (schema_version 18 → 19)
--
-- ★ 무엇을 위한 인덱스인가: 「이 프로젝트의 이 종류 이벤트를 id 순으로」 읽는 문장이다 - 종료 선언 조회
--   (closeDeclarationsByItem, pick 마다 돈다) · 재생산율(QueueReproduction 의 마무리 창과 추가 셈)이 그 꼴이고,
--   project + kind 로 거르는 다른 문장(AckReach 의 처방 셈 · ClaimHolderAt)도 앞 두 칸을 탄다. 문장 목록과
--   각 문장이 이 인덱스를 타는지는 event_project_kind_plan_test.go 가 두 엔진에서 잰다.
--
-- ★ 왜 지금인가: 그 전까지 event 인덱스는 (kind, at) · (session_id, at) 뿐이었다. SQLite 는 (kind, at) 로
--   그 종류를 다 읽고 project 로 거른 뒤 정렬했고, Postgres 는 문 캐시가 돌리는 일반 계획(generic plan)에서
--   기본키를 거꾸로 훑으며 거르는 쪽을 골랐다. 그 프로젝트의 행이 LIMIT 보다 적으면 표 전체를 다 본다 -
--   원장이 자랄수록 pick 이 느려진다. 이 인덱스가 있으면 두 엔진 모두 (project, kind) 범위 하나만 id 순으로 읽는다.
--
-- ★ id 를 셋째 칸에 적는 까닭: SQLite 에서 id 는 rowid 라 인덱스 끝에 어차피 붙지만, Postgres 는 적어야
--   id 순서를 인덱스에서 받는다. 두 엔진에 같은 정의를 둔다(pg/019_event_by_project_kind.sql).
--
-- ★ 순수 가산이다(CREATE INDEX 하나): migrate_guard_test.go 의 파괴 축에 안 걸린다.

CREATE INDEX event_by_project_kind ON event(project, kind, id);
