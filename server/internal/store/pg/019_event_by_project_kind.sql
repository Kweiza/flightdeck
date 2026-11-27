-- 019 · event 를 (project, kind, id) 로 찾는다 (schema_version 18 → 19). 기준선 위 첫 증분이다.
--
-- ★ SQLite 증분 019(store/migrations/019_event_by_project_kind.sql)와 같은 뜻이고 같은 판 번호다. 까닭과 이 인덱스를
--   타는 문장 목록은 그 파일 머리말에 있다. Postgres 쪽 몫만 적는다:
--   - 서버는 pgx 의 문 캐시로 문장을 prepared 문으로 둔다. 다섯 번 값을 넣은 계획(custom) 뒤로 서버는 일반 계획
--     (generic)으로 바꿀 수 있고, 그 계획은 project 값을 모른다. (kind, at) 밖에 없던 때 그 계획은 기본키를 거꾸로
--     훑으며 거르는 쪽을 골라 LIMIT 을 못 채우는 프로젝트에서 표 전체를 읽었다. (project, kind, id) 가 있으면 일반
--     계획도 이 인덱스 범위 하나를 id 순으로 읽는다(시험이 force_generic_plan 으로 잠근다).
--   - plan_cache_mode 를 세션에 거는 길을 안 고른 까닭: 원인은 맞는 인덱스가 없는 것이다. 설정은 그 문장 말고 모든
--     문장의 계획 캐시를 끄고, 맞춤 계획도 (kind, at) 로 그 종류를 다 읽은 뒤 정렬한다 - 원장과 함께 자란다.
--
-- ★ 트랜잭션 안에서 돈다(이전 실행기의 단 하나가 한 트랜잭션이다). 그래서 CONCURRENTLY 가 아니다 - 만드는 동안
--   event 쓰기가 기다리는데, 판올림은 서버를 내린 채 돌고 실행기가 업무 잠금을 쥔다(store.go 의 applyStep).
-- ★ 소유 역할로 돈다(engine.stepRole). 인덱스의 주인은 표의 주인이라 권한을 따로 줄 것이 없다.

CREATE INDEX event_by_project_kind ON event(project, kind, id);
