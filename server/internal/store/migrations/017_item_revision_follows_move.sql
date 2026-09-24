-- 017_item_revision_follows_move.sql — 개정 이력이 항목을 따라 옮겨 간다.
--
-- 016 은 item_revision 의 UPDATE 를 통째로 막았다(「이력을 고칠 수 있으면 이력이 아니다」).
-- 그 결과 한 번이라도 amend 된 항목은 fd move 로 못 옮기게 됐다 — MoveItem 이 딸린 행의
-- project 를 바꿔야 하는데 이 표만 바꿀 수 없었고, 바꾸지 않으면 복합 FK
-- (project, item_id) → item(project, id) 가 커밋에서 787 로 거절한다.
-- 실측(2026-09-24): 저장소 분리 때 옮긴 항목 하나가 amend 뒤 되돌릴 수 없게 갇혔다.
--
-- ★ project 는 개정의 **내용**이 아니라 그 항목이 지금 어느 프로젝트에 있는가라는 **좌표**다.
--   claim·item_after 가 이동과 함께 좌표를 바꾸는 것과 같다. 그래서 막는 칼럼을 내용으로
--   좁힌다: rev · at · session_id · title · body · paths · reason, 그리고 item_id(항목 id 는
--   이동해도 안 바뀐다 — 바뀌면 다른 항목의 이력이 된다). DELETE 금지는 그대로다.
--
-- 파괴적 조작이 없다 — 트리거를 다시 만들 뿐 표·칼럼·행을 안 건드린다.

DROP TRIGGER item_revision_no_update;

CREATE TRIGGER item_revision_no_update
BEFORE UPDATE OF item_id, rev, at, session_id, title, body, paths, reason ON item_revision
BEGIN SELECT RAISE(ABORT, 'item_revision 은 추가 전용이다 — 개정은 새 rev 로 쌓아라'); END;
