package pg_test

// PG 사다리(기준선 + 그 위 증분, storetest 의 템플릿)가 SQLite 의 마지막 판과 **같은 뜻**인지를 두 축으로 잰다.
//   - 모양: 표 · 칼럼(이름 · 순서 · 타입 · NOT NULL · 기본값 · identity · 계산식) · 기본키 · FK(ON UPDATE ·
//     ON DELETE · 미루기) · 인덱스 · CHECK 수 · 트리거를 두 엔진의 카탈로그에서 읽어 맞댄다. 일부러 다른 것은
//     pgOnly · pgOnlyTriggers 에 까닭과 함께 적는다.
//   - 거절: 같은 문장을 두 엔진에 돌려 같은 축(성공 · 유니크 · FK · CHECK · 추가 전용 · NOT NULL)으로 끝나는지 본다.
//     CHECK 는 수만으로는 내용이 바뀐 것을 못 잡으므로 CHECK 마다 허용 값 전부와 경계 양쪽을 민다.
//
// ★ 이 파일은 옮기는 동안만 산다. SQLite 실행 경로를 걷는 작업이 이 파일도 걷는다 — 그 뒤로는 기준선이 정본이다.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/kweiza/flightdeck/internal/store"
)

// openSQLite 는 마지막 판까지 올린 새 SQLite 원장을 FK 를 켜고 열고 seedSQL 을 넣는다.
func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fd.db")
	if err := store.Migrate(context.Background(), path, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("SQLite 올리기: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	// 드라이버는 모르는 pragma 를 조용히 버린다. FK 가 꺼진 채로 돌면 FK 갈래가 거짓 초록이 된다.
	if n := count(t, db, "PRAGMA foreign_keys"); n != 1 {
		t.Fatalf("SQLite 의 foreign_keys 가 %d 다, 1 이어야 한다", n)
	}
	mustExec(t, db, seedSQL)
	return db
}

// ── 거절 ────────────────────────────────────────────────────────────────────

// 거절 축. 두 엔진의 오류 코드를 이 이름으로 접는다.
const (
	okay       = ""
	unique     = "유니크"
	foreignKey = "FK"
	check      = "CHECK"
	appendOnly = "추가 전용"
	notNull    = "NOT NULL"
)

func sqliteKind(err error) string {
	if err == nil {
		return okay
	}
	switch store.ConstraintCode(err) {
	case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_UNIQUE:
		return unique
	case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
		return foreignKey
	case sqlite3.SQLITE_CONSTRAINT_CHECK:
		return check
	case sqlite3.SQLITE_CONSTRAINT_TRIGGER:
		return appendOnly
	case sqlite3.SQLITE_CONSTRAINT_NOTNULL:
		return notNull
	}
	return "모르는 오류: " + err.Error()
}

func pgKind(err error) string {
	if err == nil {
		return okay
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505":
			return unique
		case "23503":
			return foreignKey
		case "23514":
			return check
		case "23000":
			return appendOnly
		case "23502":
			return notNull
		}
	}
	return "모르는 오류: " + err.Error()
}

// step 은 문장 하나와 기대다. count 가 있으면 그 질의의 수가 n 이어야 한다.
type step struct {
	sql   string
	want  string
	count string
	n     int
}

// each 는 tmpl 의 %[1]s 자리에 vals 를 하나씩 넣은 걸음들이다. 모두 want 로 끝나야 한다.
func each(want, tmpl string, vals ...string) []step {
	var out []step
	for _, v := range vals {
		out = append(out, step{sql: fmt.Sprintf(tmpl, v), want: want})
	}
	return out
}

func pass(q string) step           { return step{sql: q, want: okay} }
func reject(kind, q string) step   { return step{sql: q, want: kind} }
func counted(q string, n int) step { return step{count: q, n: n} }

// run 은 steps 를 차례로 돌려 기대와 다른 걸음을 모두 적는다.
func run(t *testing.T, db *sql.DB, engine string, kind func(error) string, steps []step) {
	t.Helper()
	for i, s := range steps {
		if s.count != "" {
			if got := count(t, db, s.count); got != s.n {
				t.Errorf("%s %d번 %s = %d, 기대 %d", engine, i, s.count, got, s.n)
			}
			continue
		}
		_, err := db.ExecContext(context.Background(), s.sql)
		if got := kind(err); got != s.want {
			t.Errorf("%s %d번 %s\n  결과 %q, 기대 %q", engine, i, s.sql, got, s.want)
		}
	}
}

// 같은 문장이 두 엔진에서 같은 축으로 끝난다. 부분 유니크 인덱스가 막는 이중 보유 · 이중 줄 서기가 핵심이다.
func TestBothEnginesRefuseTheSameWrites(t *testing.T) {
	for _, sc := range []struct {
		name  string
		steps []step
	}{
		// 실제 선점 문장(upsert)으로 잰다. 살아 있는 남의 선점을 덮는 upsert 는 PG 만 거절한다 — SQLite 는 쓰기가
		// 한 줄이라 코드의 판정이 지킨다. 그 갈래는 TestClaimUpsertCannotTakeALiveClaim 이다.
		{"선점은 항목마다 한 행(ClaimItem 의 upsert)", []step{
			pass(claimUpsert("i1", "s1")),
			pass(claimUpsert("i1", "s1")),
			reject(unique, "INSERT INTO claim(project, item_id, session_id, at) VALUES ('p', 'i1', 's2', "+at+")"),
			pass(forceRelease),
			pass(claimUpsert("i1", "s2")),
			pass(claimUpsert("i2", "s1")),
			counted("SELECT count(*) FROM claim WHERE item_id = 'i1'", 1),
			counted("SELECT count(*) FROM claim WHERE item_id = 'i1' AND session_id = 's2' AND released_at IS NULL", 1),
		}},
		{"살아 있는 보유는 자원마다 하나(resource_one_holder)", []step{
			pass("INSERT INTO resource_hold(project, resource, session_id, acquired_at) VALUES ('p', 'landing', 's1', " + at + ")"),
			reject(unique, "INSERT INTO resource_hold(project, resource, session_id, acquired_at) VALUES ('p', 'landing', 's2', "+at+")"),
			reject(unique, "INSERT INTO resource_hold(project, resource, job_id, acquired_at) VALUES ('p', 'landing', 'jb', "+at+")"),
			pass("INSERT INTO resource_hold(project, resource, session_id, acquired_at) VALUES ('p', 'x', 's2', " + at + ")"),
			pass("INSERT INTO resource_hold(project, resource, session_id, acquired_at) VALUES ('p2', 'landing', 's2', " + at + ")"),
			pass("UPDATE resource_hold SET released_at = " + at + " WHERE project = 'p' AND resource = 'landing'"),
			pass("INSERT INTO resource_hold(project, resource, session_id, acquired_at) VALUES ('p', 'landing', 's2', " + at + ")"),
			reject(unique, "INSERT INTO resource_hold(project, resource, session_id, acquired_at) VALUES ('p', 'landing', 's1', "+at+")"),
			counted("SELECT count(*) FROM resource_hold WHERE project = 'p' AND resource = 'landing' AND released_at IS NULL", 1),
		}},
		{"세션은 살아 있는 줄 행 하나(landing_queue_one_live_per_session)", []step{
			pass("INSERT INTO landing_queue(project, session_id, enqueued_at) VALUES ('p', 's1', " + at + ")"),
			reject(unique, "INSERT INTO landing_queue(project, session_id, enqueued_at) VALUES ('p', 's1', "+at+")"),
			pass("INSERT INTO landing_queue(project, session_id, enqueued_at) VALUES ('p', 's2', " + at + ")"),
			pass("UPDATE landing_queue SET left_at = " + at + ", left_kind = 'ok' WHERE session_id = 's1'"),
			pass("INSERT INTO landing_queue(project, session_id, enqueued_at) VALUES ('p', 's1', " + at + ")"),
			counted("SELECT count(*) FROM landing_queue WHERE session_id = 's1' AND left_at IS NULL", 1),
		}},
		{"그 밖의 유니크(세션 3중키 · 받는 곳 소켓)", []step{
			reject(unique, "INSERT INTO session(id, project, machine_id, worktree, cc_session_id, opened_at) VALUES ('s3', 'p', 'm', '/w1', 'c1', "+at+")"),
			pass("INSERT INTO session_inbox(session_id, machine_id, socket, mode, seen_at) VALUES ('s1', 'm', '/sock', '', " + at + ")"),
			reject(unique, "INSERT INTO session_inbox(session_id, machine_id, socket, mode, seen_at) VALUES ('s2', 'm', '/sock', '', "+at+")"),
		}},
		{"셋 · 둘 중 정확히 하나(num_nonnulls)", []step{
			pass("INSERT INTO item_after(project, item_id, dep_item) VALUES ('p', 'i1', 'i2')"),
			pass("INSERT INTO item_after(project, item_id, dep_job) VALUES ('p', 'i1', 'jb')"),
			reject(check, "INSERT INTO item_after(project, item_id) VALUES ('p', 'i1')"),
			reject(check, "INSERT INTO item_after(project, item_id, dep_item, dep_sha) VALUES ('p', 'i1', 'i2', 'abc')"),
			reject(check, "INSERT INTO item_after(project, item_id, dep_item, dep_job, dep_sha) VALUES ('p', 'i1', 'i2', 'jb', 'abc')"),
			pass("INSERT INTO resource_hold(project, resource, job_id, acquired_at) VALUES ('p', 'r1', 'jb', " + at + ")"),
			reject(check, "INSERT INTO resource_hold(project, resource, acquired_at) VALUES ('p', 'r2', "+at+")"),
			reject(check, "INSERT INTO resource_hold(project, resource, session_id, job_id, acquired_at) VALUES ('p', 'r3', 's1', 'jb', "+at+")"),
		}},
		{"줄 행이 빠지는 법(landing_queue CHECK 셋 · 줄 행의 자원)", slices.Concat(
			[]step{
				reject(check, "INSERT INTO landing_queue(project, session_id, enqueued_at, left_at) VALUES ('p', 's1', "+at+", "+at+")"),
				reject(check, "INSERT INTO landing_queue(project, session_id, enqueued_at, left_kind) VALUES ('p', 's1', "+at+", 'ok')"),
				reject(check, "INSERT INTO landing_queue(project, session_id, enqueued_at, left_at, left_kind, left_detail) VALUES ('p', 's1', "+at+", "+at+", 'bogus', 'd')"),
			},
			// ok · finish 는 사유가 없어도 되고, 나머지 셋은 빈 사유도 안 된다.
			each(okay, "INSERT INTO landing_queue(project, session_id, enqueued_at, left_at, left_kind) VALUES ('p', 's1', "+at+", "+at+", '%[1]s')", "ok", "finish"),
			each(check, "INSERT INTO landing_queue(project, session_id, enqueued_at, left_at, left_kind) VALUES ('p', 's1', "+at+", "+at+", '%[1]s')", "fail", "leave", "force"),
			each(check, "INSERT INTO landing_queue(project, session_id, enqueued_at, left_at, left_kind, left_detail) VALUES ('p', 's1', "+at+", "+at+", '%[1]s', '')", "fail", "leave", "force"),
			each(okay, "INSERT INTO landing_queue(project, session_id, enqueued_at, left_at, left_kind, left_detail) VALUES ('p', 's1', "+at+", "+at+", '%[1]s', 'd')", "fail", "leave", "force"),
			[]step{
				reject(check, "INSERT INTO landing_queue_resource(row_id, resource) VALUES ((SELECT max(id) FROM landing_queue), '')"),
				pass("INSERT INTO landing_queue_resource(row_id, resource) VALUES ((SELECT max(id) FROM landing_queue), 'landing')"),
			},
		)},
		// CHECK 하나마다 경계 양쪽을 민다: 목록 CHECK 는 허용 값 **전부**가 지나고 목록 밖 하나가 걸린다(값 하나를
		// 빠뜨린 옮김이 여기서 빨개진다). 「빈 값 금지」는 NULL · '' 가 걸리고 값이 지난다. 범위는 양 끝을 민다.
		{"CHECK 의 경계", slices.Concat(
			each(okay, "UPDATE session SET state = '%[1]s' WHERE id = 's2'", "active", "paused", "done"),
			[]step{
				reject(check, "UPDATE session SET state = 'gone' WHERE id = 's2'"),
				reject(check, "UPDATE session SET state = 'blocked' WHERE id = 's1'"),
				reject(check, "UPDATE session SET state = 'blocked', blocked_why = '' WHERE id = 's1'"),
				pass("UPDATE session SET state = 'blocked', blocked_why = 'w' WHERE id = 's1'"),
			},
			each(okay, "INSERT INTO signal(session_id, kind, at) VALUES ('s1', '%[1]s', "+at+")", "prompt", "tool", "mcp", "commit", "push"),
			each(check, "INSERT INTO signal(session_id, kind, at) VALUES ('s2', '%[1]s', "+at+")", "bogus"),
			each(okay, "INSERT INTO footprint(session_id, path, origin, first_at, last_at) VALUES ('s1', 'a', '%[1]s', "+at+", "+at+")", "observed", "declared", "claimed"),
			each(check, "INSERT INTO footprint(session_id, path, origin, first_at, last_at) VALUES ('s1', 'a', '%[1]s', "+at+", "+at+")", "bogus"),
			each(okay, "UPDATE item SET state = '%[1]s' WHERE id = 'i2'", "open", "claimed", "done"),
			[]step{
				reject(check, "UPDATE item SET state = 'gone' WHERE id = 'i2'"),
				reject(check, "UPDATE item SET state = 'dropped' WHERE id = 'i1'"),
				reject(check, "UPDATE item SET state = 'dropped', close_reason = '' WHERE id = 'i1'"),
				pass("UPDATE item SET state = 'dropped', close_reason = 'r' WHERE id = 'i1'"),
			},
			each(okay, "INSERT INTO judgment(id, at, kind, body) VALUES ('j-%[1]s', "+at+", '%[1]s', 'b')",
				"handoff", "decision", "blocked", "ask", "now", "rejected", "not-done", "verified", "draft"),
			[]step{
				reject(check, "INSERT INTO judgment(id, at, kind, body) VALUES ('j2', "+at+", 'bogus', 'b')"),
				reject(check, "INSERT INTO judgment(id, at, kind, body) VALUES ('j3', "+at+", 'decision', '')"),
			},
			each(okay, "INSERT INTO judgment_link(judgment_id, target_kind, target_id) VALUES ('j1', '%[1]s', 'x')", "item", "job", "commit", "session"),
			each(check, "INSERT INTO judgment_link(judgment_id, target_kind, target_id) VALUES ('j1', '%[1]s', 'x')", "bogus"),
			[]step{
				pass("INSERT INTO snapshot(project, key, value, method, computed_at) VALUES ('p', 'k1', 'v', 'command', " + at + ")"),
				pass("INSERT INTO snapshot(project, key, value, method, evidence, computed_at) VALUES ('p', 'k2', 'v', 'manual', 'e', " + at + ")"),
				reject(check, "INSERT INTO snapshot(project, key, value, method, computed_at) VALUES ('p', 'k3', 'v', 'manual', "+at+")"),
				reject(check, "INSERT INTO snapshot(project, key, value, method, evidence, computed_at) VALUES ('p', 'k3', 'v', 'manual', '', "+at+")"),
				reject(check, "INSERT INTO snapshot(project, key, value, method, computed_at) VALUES ('p', 'k3', 'v', 'bogus', "+at+")"),
			},
			each(okay, "INSERT INTO job(id, project, kind, queued_at) VALUES ('k-%[1]s', 'p', '%[1]s', "+at+")", "land", "exec"),
			each(check, "INSERT INTO job(id, project, kind, queued_at) VALUES ('k-%[1]s', 'p', '%[1]s', "+at+")", "bogus"),
			each(okay, "INSERT INTO job(id, project, kind, state, queued_at) VALUES ('s-%[1]s', 'p', 'land', '%[1]s', "+at+")",
				"queued", "running", "ok", "fail", "stalled", "bypassed"),
			each(check, "INSERT INTO job(id, project, kind, state, queued_at) VALUES ('s-%[1]s', 'p', 'land', '%[1]s', "+at+")", "bogus"),
			each(okay, "INSERT INTO job(id, project, kind, fail_kind, queued_at) VALUES ('f-%[1]s', 'p', 'land', '%[1]s', "+at+")",
				"conflict", "verify", "seed", "raced", "infra", "timeout", "admission", "push"),
			each(check, "INSERT INTO job(id, project, kind, fail_kind, queued_at) VALUES ('f-%[1]s', 'p', 'land', '%[1]s', "+at+")", "bogus"),
			each(okay, "INSERT INTO idempotency(key, fingerprint, status, ctype, body, at) VALUES ('k%[1]s', 'f', %[1]s, 'c', 'x', "+at+")", "100", "200", "499"),
			each(check, "INSERT INTO idempotency(key, fingerprint, status, ctype, body, at) VALUES ('k%[1]s', 'f', %[1]s, 'c', 'x', "+at+")", "99", "500", "503"),
			[]step{
				reject(check, "INSERT INTO item_revision(project, item_id, rev, at, title, body, paths, reason) VALUES ('p', 'i1', 2, "+at+", 't', 'b', '[]', '')"),
				pass("INSERT INTO item_revision(project, item_id, rev, at, title, body, paths, reason) VALUES ('p', 'i1', 2, " + at + ", 't', 'b', '[]', 'r')"),
				reject(check, "INSERT INTO session_inbox(session_id, machine_id, socket, mode, seen_at) VALUES ('s1', 'm', '', '', "+at+")"),
				pass("INSERT INTO session_inbox(session_id, machine_id, socket, mode, seen_at) VALUES ('s1', 'm', 's', '', " + at + ")"),
			},
		)},
		// 키에 길이 제한이 없다. PG 는 키의 지문을 기본키로 두어 B-tree 항목 상한을 비켜 간다.
		{"snapshot 의 긴 키", []step{
			pass("INSERT INTO snapshot(project, key, value, method, computed_at) VALUES ('p', '" + longKey + "', 'v', 'command', " + at + ")"),
			reject(unique, "INSERT INTO snapshot(project, key, value, method, computed_at) VALUES ('p', '"+longKey+"', 'v', 'command', "+at+")"),
			pass("INSERT INTO snapshot(project, key, value, method, computed_at) VALUES ('p', '" + longKey + "x', 'v', 'command', " + at + ")"),
			pass("INSERT INTO snapshot(project, key, value, method, computed_at) VALUES ('p2', '" + longKey + "', 'v', 'command', " + at + ")"),
			counted("SELECT count(*) FROM snapshot", 3),
		}},
		{"추가 전용", []step{
			reject(appendOnly, "UPDATE judgment SET body = 'x'"),
			reject(appendOnly, "DELETE FROM judgment"),
			reject(appendOnly, "UPDATE event SET kind = 'x'"),
			reject(appendOnly, "DELETE FROM event"),
			reject(appendOnly, "UPDATE item_revision SET title = 'x'"),
			reject(appendOnly, "DELETE FROM item_revision"),
			// project 는 좌표라 트리거를 지나고, 옮긴 곳에 항목이 없어 FK 에 걸린다(증분 017).
			reject(foreignKey, "UPDATE item_revision SET project = 'p2'"),
			// 행위자 칸도 내용이다(증분 020): 판단은 칸을 가리지 않고, 개정은 갱신 금지 칸에 author 가 들었다.
			reject(appendOnly, "UPDATE judgment SET author = NULL"),
			reject(appendOnly, "UPDATE judgment SET via = 'cli'"),
			reject(appendOnly, "UPDATE item_revision SET author = NULL"),
			counted("SELECT count(*) FROM judgment", 1),
			counted("SELECT count(*) FROM event", 1),
			counted("SELECT count(*) FROM item_revision", 1),
		}},
		{"계정 · 초대 · 기기 코드 · 기기 토큰(증분 020)", slices.Concat(
			[]step{
				pass("INSERT INTO account(id, login, display_name, role, created_at) VALUES ('a1', 'alice', '앨리스', 'admin', " + at + ")"),
				reject(unique, "INSERT INTO account(id, login, display_name, role, created_at) VALUES ('a2', 'alice', 'b', 'member', "+at+")"),
				reject(check, "INSERT INTO account(id, login, display_name, role, created_at) VALUES ('a2', '', 'b', 'member', "+at+")"),
				reject(notNull, "INSERT INTO account(id, display_name, role, created_at) VALUES ('a2', 'b', 'member', "+at+")"),
				pass("INSERT INTO account(id, login, display_name, role, created_at) VALUES ('a2', 'bob', 'b', 'member', " + at + ")"),
				reject(foreignKey, "INSERT INTO web_session(id_hash, account_id, created_at, expires_at, last_seen_at) VALUES ('w1', 'nope', "+at+", "+at+", "+at+")"),
				pass("INSERT INTO web_session(id_hash, account_id, created_at, expires_at, last_seen_at) VALUES ('w1', 'a1', " + at + ", " + at + ", " + at + ")"),
			},
			each(check, "INSERT INTO account(id, login, display_name, role, created_at) VALUES ('a9', 'x', 'x', '%[1]s', "+at+")", "owner", "Admin", ""),
			[]step{
				pass("INSERT INTO invite(id, token_hash, created_by, role, created_at, expires_at) VALUES ('v1', 'th1', 'a1', 'member', " + at + ", " + at + ")"),
				reject(unique, "INSERT INTO invite(id, token_hash, created_by, role, created_at, expires_at) VALUES ('v2', 'th1', 'a1', 'member', "+at+", "+at+")"),
				reject(check, "INSERT INTO invite(id, token_hash, created_by, role, created_at, expires_at) VALUES ('v2', 'th2', 'a1', 'owner', "+at+", "+at+")"),
				reject(foreignKey, "INSERT INTO invite(id, token_hash, created_by, role, created_at, expires_at) VALUES ('v2', 'th2', 'nope', 'member', "+at+", "+at+")"),
				// 쓴 때와 쓴 계정은 함께 있거나 함께 없다. 쓴 초대는 철회하지 않는다.
				reject(check, "UPDATE invite SET used_at = "+at+" WHERE id = 'v1'"),
				reject(check, "UPDATE invite SET used_by = 'a2' WHERE id = 'v1'"),
				pass("UPDATE invite SET used_at = " + at + ", used_by = 'a2' WHERE id = 'v1'"),
				reject(check, "UPDATE invite SET revoked_at = "+at+" WHERE id = 'v1'"),
				pass("INSERT INTO invite(id, token_hash, created_by, role, created_at, expires_at, revoked_at) VALUES ('v2', 'th2', 'a1', 'admin', " + at + ", " + at + ", " + at + ")"),

				pass("INSERT INTO device_code(code_hash, user_code, requested_name, requested_ip, requested_at, expires_at) VALUES ('d1', 'ABCD-EFGH', 'h', '192.0.2.1', " + at + ", " + at + ")"),
				reject(unique, "INSERT INTO device_code(code_hash, user_code, requested_name, requested_ip, requested_at, expires_at) VALUES ('d2', 'ABCD-EFGH', 'h', '192.0.2.1', "+at+", "+at+")"),
				// 승인한 계정과 승인한 때는 함께 있거나 함께 없다. 낸 기기는 승인 뒤에만 있다.
				reject(check, "UPDATE device_code SET approved_by = 'a1' WHERE code_hash = 'd1'"),
				reject(check, "UPDATE device_code SET approved_at = "+at+" WHERE code_hash = 'd1'"),
				reject(check, "UPDATE device_code SET issued_machine = 'm' WHERE code_hash = 'd1'"),
				reject(foreignKey, "UPDATE device_code SET invite_id = 'nope' WHERE code_hash = 'd1'"),
				pass("UPDATE device_code SET approved_by = 'a1', approved_at = " + at + ", issued_machine = 'm', invite_id = 'v1' WHERE code_hash = 'd1'"),
				// machine_hint 는 FK 가 없다 - 클라이언트가 보낸 옛 id 라 없는 기기를 가리킬 수 있다.
				pass("INSERT INTO device_code(code_hash, user_code, machine_hint, requested_name, requested_ip, requested_at, expires_at) VALUES ('d2', 'IJKL-MNOP', 'nope', 'h', '192.0.2.2', " + at + ", " + at + ")"),

				// 토큰 지문은 기기마다 하나이고, 토큰 없는 기기(NULL)는 몇이든 된다.
				pass("UPDATE machine SET account_id = 'a1', token_hash = 'tk1' WHERE id = 'm'"),
				reject(unique, "INSERT INTO machine(id, hostname, first_seen, last_seen, token_hash) VALUES ('m2', 'h', "+at+", "+at+", 'tk1')"),
				pass("INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m2', 'h', " + at + ", " + at + ")"),
				pass("INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m3', 'h', " + at + ", " + at + ")"),
				reject(foreignKey, "UPDATE machine SET revoked_by = 'nope' WHERE id = 'm2'"),
				reject(foreignKey, "UPDATE session SET account_id = 'nope' WHERE id = 's1'"),
				reject(foreignKey, "UPDATE item SET created_session = 'nope' WHERE id = 'i1'"),
				pass("UPDATE item SET created_by = 'a1', created_session = 's1' WHERE id = 'i1'"),
				// 계정을 가리키는 행이 있으면 계정을 못 지운다(CASCADE 가 없다).
				reject(foreignKey, "DELETE FROM account WHERE id = 'a1'"),
			},
			each(okay, "INSERT INTO judgment(id, at, kind, body, author, via) VALUES ('jv%[1]s', "+at+", 'decision', 'b', 'a1', '%[1]s')", "session", "web", "cli"),
			each(check, "INSERT INTO judgment(id, at, kind, body, via) VALUES ('jx', "+at+", 'decision', 'b', '%[1]s')", "mail", "Web", ""),
			[]step{
				reject(foreignKey, "INSERT INTO judgment(id, at, kind, body, author) VALUES ('jy', "+at+", 'decision', 'b', 'nope')"),
				counted("SELECT count(*) FROM account", 2),
				counted("SELECT count(*) FROM judgment WHERE author = 'a1'", 3),
			},
		)},
		{"FK 와 함께 지우기", []step{
			reject(foreignKey, "INSERT INTO session(id, project, machine_id, worktree, cc_session_id, opened_at) VALUES ('s3', 'nope', 'm', '/w3', 'c3', "+at+")"),
			reject(foreignKey, "INSERT INTO judgment(id, at, kind, body, supersedes) VALUES ('j2', "+at+", 'decision', 'b', 'nope')"),
			reject(foreignKey, "INSERT INTO landing_queue_resource(row_id, resource) VALUES (999, 'landing')"),
			pass("INSERT INTO signal(session_id, kind, at) VALUES ('s2', 'tool', " + at + ")"),
			pass("INSERT INTO footprint(session_id, path, origin, first_at, last_at) VALUES ('s2', 'a', 'observed', " + at + ", " + at + ")"),
			pass("INSERT INTO session_workspace(session_id, project, path, is_primary) VALUES ('s2', 'p', '/w2', 1)"),
			pass("DELETE FROM session WHERE id = 's2'"),
			counted("SELECT count(*) FROM signal", 0),
			counted("SELECT count(*) FROM footprint", 0),
			counted("SELECT count(*) FROM session_workspace", 0),
			reject(foreignKey, "DELETE FROM session WHERE id = 's1'"), // 판단이 CASCADE 없이 가리킨다
			reject(foreignKey, "DELETE FROM item WHERE id = 'i1'"),    // 개정 이력이 CASCADE 없이 가리킨다
			pass("INSERT INTO item_after(project, item_id, dep_sha) VALUES ('p', 'i2', 'abc')"),
			pass("DELETE FROM item WHERE id = 'i2'"),
			counted("SELECT count(*) FROM item_after", 0),
		}},
		{"NOT NULL", []step{
			reject(notNull, "INSERT INTO project(id, created_at) VALUES ('p3', "+at+")"),
			reject(notNull, "INSERT INTO event(at) VALUES ("+at+")"),
			pass("INSERT INTO event(at, kind) VALUES (" + at + ", 'k')"),
			counted("SELECT count(*) FROM event WHERE payload = '{}'", 2), // 씨앗 한 행 + 방금 한 행
		}},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			run(t, openSQLite(t), "SQLite", sqliteKind, sc.steps)
			run(t, openPG(t), "PG", pgKind, sc.steps)
		})
	}
}

// ── 모양 ────────────────────────────────────────────────────────────────────

// shape 은 표 하나의 모양을 두 엔진에 공통인 말로 적은 것이다. 문자열로 접어 두면 어긋난 자리가 읽힌다.
type shape struct {
	Cols    []string // 이름 타입 [NOT NULL] [DEFAULT x] [IDENTITY 세대] [GENERATED 식], 순서대로
	PK      string
	FKs     []string // (칼럼) → 표(칼럼) ON UPDATE x ON DELETE y [DEFERRABLE], 정렬
	Indexes []string // [이름] (칼럼) [UNIQUE] [WHERE 술어], 정렬. 기본키 인덱스는 PK 가 맡는다
	Checks  int
}

// pgOnly 는 기준선이 SQLite 판 18 에 일부러 더한 것이다. 뜻을 바꾸지 않는 옮김의 몫이라 기대에 얹는다.
var pgOnly = map[string]func(*shape){
	// rowid 의 자리 — 이 표에는 기본키가 없어 SQLite 는 rowid 로 순서를 읽는다.
	"item_after": func(s *shape) {
		s.Cols = append(s.Cols, "seq bigint NOT NULL IDENTITY ALWAYS")
		s.PK = "seq"
	},
	// 키 원문은 B-tree 항목 상한(약 2.7KiB)을 넘을 수 있다. 키의 지문(sha256)이 키다 — 지문이 같은 다른 키는
	// snapshot_key_digest_unique 가 거절하므로 「키 하나에 행 하나」는 그대로다.
	"snapshot": func(s *shape) {
		s.Cols = append(s.Cols, "key_digest bytea NOT NULL GENERATED fd_key_digest(key)")
		s.PK = "project,key_digest"
	},
	// FTS5 judgment_fts 의 자리.
	"judgment": func(s *shape) {
		s.Indexes = append(s.Indexes, "judgment_text_trgm (expr) gin")
		sort.Strings(s.Indexes)
	},
}

// pgOnlyTriggers 는 기준선이 SQLite 판 18 에 일부러 더한 트리거다.
var pgOnlyTriggers = []string{
	// 살아 있는 남의 선점을 upsert 가 덮지 못한다. SQLite 는 쓰기가 한 줄로 서서 코드의 판정이 지킨다.
	"claim.claim_one_live_holder 행",
	// 이력의 project 는 항목과 함께만 옮긴다(커밋 때 옛 자리에 항목이 없어야 한다).
	"item_revision.item_revision_moves_with_item 행",
	// 지문이 같은 다른 키가 upsert 의 충돌 갈래로 가서 남의 행을 덮지 못한다(SQLite 는 키 원문이 기본키다).
	"snapshot.snapshot_key_digest_unique 행",
}

// sqliteOnly 는 SQLite 에만 있고 기준선이 일부러 안 옮긴 것이다(FTS5 와 그 그림자 표 · 넣기 트리거,
// AUTOINCREMENT 의 sqlite_sequence).
func sqliteOnly(name string) bool {
	return strings.HasPrefix(name, "sqlite_") || strings.HasPrefix(name, "judgment_fts")
}

// SQLite 의 살아 있는 스키마(마지막 판)를 PG 말로 옮긴 기대가, 사다리를 다 깐 PG 의 카탈로그와 같다.
func TestBaselineHasTheSameShapeAsSQLite(t *testing.T) {
	lite, pgdb := openSQLite(t), openPG(t)
	want, wantTrig := sqliteShapes(t, lite)
	got, gotTrig := pgShapes(t, pgdb)

	tables := slices.Sorted(maps.Keys(want))
	if g := slices.Sorted(maps.Keys(got)); !slices.Equal(tables, g) {
		t.Fatalf("표 목록이 다르다\n  SQLite %v\n  PG     %v", tables, g)
	}
	for _, table := range tables {
		w := want[table]
		if patch, ok := pgOnly[table]; ok {
			patch(&w)
		}
		g := got[table]
		if !slices.Equal(w.Cols, g.Cols) {
			t.Errorf("%s 칼럼\n  SQLite %q\n  PG     %q", table, w.Cols, g.Cols)
		}
		if w.PK != g.PK {
			t.Errorf("%s 기본키: SQLite %q, PG %q", table, w.PK, g.PK)
		}
		if !slices.Equal(w.FKs, g.FKs) {
			t.Errorf("%s FK\n  SQLite %q\n  PG     %q", table, w.FKs, g.FKs)
		}
		if !slices.Equal(w.Indexes, g.Indexes) {
			t.Errorf("%s 인덱스\n  SQLite %q\n  PG     %q", table, w.Indexes, g.Indexes)
		}
		if w.Checks != g.Checks {
			t.Errorf("%s CHECK 수: SQLite %d, PG %d", table, w.Checks, g.Checks)
		}
	}
	// 트리거: SQLite 의 추가 전용 행 트리거가 PG 에 같은 이름으로 있고, PG 는 그 표마다 TRUNCATE 문 트리거를 더한다.
	// 그 밖에 PG 가 더한 것은 pgOnlyTriggers 다.
	wantPG := map[string]bool{}
	for _, name := range pgOnlyTriggers {
		wantPG[name] = true
	}
	for name, table := range wantTrig {
		wantPG[table+"."+name+" 행"] = true
		wantPG[table+"."+table+"_no_truncate 문"] = true
	}
	if w, g := slices.Sorted(maps.Keys(wantPG)), slices.Sorted(maps.Keys(gotTrig)); !slices.Equal(w, g) {
		t.Errorf("트리거\n  SQLite 에서 기대 %q\n  PG              %q", w, g)
	}
}

var (
	sqlComment = regexp.MustCompile(`--[^\n]*`)
	checkWord  = regexp.MustCompile(`(?i)\bCHECK\s*\(`)
	whereTail  = regexp.MustCompile(`(?is)\bWHERE\b(.*)$`)
	castTail   = regexp.MustCompile(`::[a-z ]+$`)
)

// predicate 은 부분 인덱스 술어를 두 엔진에 공통인 꼴로 접는다: 소문자, 괄호 없이, 공백 하나.
func predicate(s string) string {
	s = strings.NewReplacer("(", "", ")", "").Replace(strings.ToLower(s))
	return strings.Join(strings.Fields(s), " ")
}

func index(name, cols string, unique bool, pred string) string {
	s := name + " (" + cols + ")"
	if unique {
		s += " UNIQUE"
	}
	if pred != "" {
		s += " WHERE " + predicate(pred)
	}
	return strings.TrimSpace(s)
}

func strs(t *testing.T, db *sql.DB, q string, args ...any) [][]string {
	t.Helper()
	r, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer r.Close()
	cols, _ := r.Columns()
	var out [][]string
	for r.Next() {
		row := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		vals := make([]string, len(cols))
		for i, v := range row {
			vals[i] = v.String
		}
		out = append(out, vals)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// sqliteShapes 는 SQLite 의 카탈로그를 PG 말로 옮긴 기대다. 둘째 값은 추가 전용 트리거(이름 → 표)다.
func sqliteShapes(t *testing.T, db *sql.DB) (map[string]shape, map[string]string) {
	t.Helper()
	types := map[string]string{"INTEGER": "bigint", "TEXT": "text", "BLOB": "bytea"}
	out := map[string]shape{}
	for _, tr := range strs(t, db, `SELECT name, sql FROM sqlite_master WHERE type = 'table' ORDER BY name`) {
		table, ddl := tr[0], tr[1]
		if sqliteOnly(table) {
			continue
		}
		var s shape
		autoinc := strings.Contains(strings.ToUpper(ddl), "AUTOINCREMENT")
		for _, c := range strs(t, db, `SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?) ORDER BY cid`, table) {
			name, typ, nn, dflt, inPK := c[0], c[1], c[2] == "1", c[3], c[4] != "0"
			pgType, known := types[strings.ToUpper(typ)]
			if !known {
				t.Errorf("%s.%s 의 타입 %q 를 옮기는 규칙이 없다", table, name, typ)
			}
			col := name + " " + pgType
			if nn || inPK { // PG 의 기본키는 곧 NOT NULL 이다(SQLite 는 정수 아닌 키에 NULL 을 받는다)
				col += " NOT NULL"
			}
			if dflt != "" {
				col += " DEFAULT " + dflt
			}
			if autoinc && inPK { // 손으로 넣은 번호가 시퀀스를 앞지르지 못하게 ALWAYS 다(SQLite 보다 엄격하다)
				col += " IDENTITY ALWAYS"
			}
			s.Cols = append(s.Cols, col)
		}
		// pragma 의 pk 는 기본키 안의 자리(1부터)다. 그 순서로 줄 세운다.
		var pk []string
		for _, c := range strs(t, db, `SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table) {
			pk = append(pk, c[0])
		}
		s.PK = strings.Join(pk, ",")
		// FK 하나가 칼럼 여럿이면 pragma 가 같은 id 로 여러 줄을 낸다(seq 순).
		type fkRows struct {
			from, to                  []string
			table, onUpdate, onDelete string
		}
		var fks []*fkRows
		byID := map[string]*fkRows{}
		for _, f := range strs(t, db, `SELECT id, "table", "from", "to", on_update, on_delete FROM pragma_foreign_key_list(?) ORDER BY id, seq`, table) {
			fk, seen := byID[f[0]]
			if !seen {
				fk = &fkRows{table: f[1], onUpdate: f[4], onDelete: f[5]}
				byID[f[0]] = fk
				fks = append(fks, fk)
			}
			fk.from, fk.to = append(fk.from, f[2]), append(fk.to, f[3])
		}
		for _, f := range fks {
			s.FKs = append(s.FKs, fmt.Sprintf("(%s) → %s(%s) ON UPDATE %s ON DELETE %s DEFERRABLE",
				strings.Join(f.from, ","), f.table, strings.Join(f.to, ","), f.onUpdate, f.onDelete))
		}
		sort.Strings(s.FKs)
		for _, ix := range strs(t, db, `SELECT name, "unique", origin, partial FROM pragma_index_list(?)`, table) {
			name, uniq, origin, partial := ix[0], ix[1] == "1", ix[2], ix[3] == "1"
			if origin == "pk" {
				continue
			}
			var cols []string
			for _, c := range strs(t, db, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name) {
				cols = append(cols, c[0])
			}
			var pred string
			if partial {
				src := strs(t, db, `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, name)
				if m := whereTail.FindStringSubmatch(src[0][0]); m != nil {
					pred = m[1]
				}
			}
			if origin == "u" { // UNIQUE 제약이 만든 인덱스 — 이름은 엔진이 짓는다
				name = ""
			}
			s.Indexes = append(s.Indexes, index(name, strings.Join(cols, ","), uniq, pred))
		}
		sort.Strings(s.Indexes)
		s.Checks = len(checkWord.FindAllString(sqlComment.ReplaceAllString(ddl, ""), -1))
		out[table] = s
	}
	trig := map[string]string{}
	for _, tr := range strs(t, db, `SELECT name, tbl_name FROM sqlite_master WHERE type = 'trigger'`) {
		if !sqliteOnly(tr[0]) {
			trig[tr[0]] = tr[1]
		}
	}
	return out, trig
}

// pgShapes 는 기준선을 깐 PG 의 카탈로그다. 둘째 값은 트리거(「표.이름 행|문」)다.
func pgShapes(t *testing.T, db *sql.DB) (map[string]shape, map[string]bool) {
	t.Helper()
	// attnames 는 칼럼 번호 배열을 이름 목록으로 바꾼다(순서 보존).
	const attnames = `(SELECT string_agg(a.attname, ',' ORDER BY k.ord)
		FROM unnest(%s) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = %s AND a.attnum = k.attnum)`
	out := map[string]shape{}
	for _, tr := range strs(t, db, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`) {
		table := tr[0]
		rel := `format('%I', $1::text)::regclass`
		var s shape
		for _, c := range strs(t, db, `SELECT column_name, data_type, is_nullable, column_default, identity_generation,
				generation_expression
			FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1
			ORDER BY ordinal_position`, table) {
			col := c[0] + " " + c[1]
			if c[2] == "NO" {
				col += " NOT NULL"
			}
			if c[3] != "" {
				col += " DEFAULT " + castTail.ReplaceAllString(c[3], "")
			}
			if c[4] != "" {
				col += " IDENTITY " + c[4]
			}
			if c[5] != "" {
				col += " GENERATED " + c[5]
			}
			s.Cols = append(s.Cols, col)
		}
		for _, ix := range strs(t, db, `SELECT c.relname, i.indisunique, i.indisprimary,
				EXISTS (SELECT 1 FROM pg_constraint x WHERE x.conindid = i.indexrelid AND x.contype = 'u'),
				i.indexprs IS NOT NULL, am.amname, coalesce(pg_get_expr(i.indpred, i.indrelid), ''),
				`+fmt.Sprintf(attnames, "i.indkey::int2[]", "i.indrelid")+`
			FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_am am ON am.oid = c.relam
			WHERE i.indrelid = `+rel, table) {
			name, uniq, primary, constraint, expr, am, pred, cols := ix[0], ix[1] == "true", ix[2] == "true",
				ix[3] == "true", ix[4] == "true", ix[5], ix[6], ix[7]
			switch {
			case primary:
				s.PK = cols
			case expr:
				s.Indexes = append(s.Indexes, name+" (expr) "+am)
			default:
				if constraint {
					name = ""
				}
				s.Indexes = append(s.Indexes, index(name, cols, uniq, pred))
			}
		}
		sort.Strings(s.Indexes)
		action := map[string]string{"a": "NO ACTION", "c": "CASCADE", "r": "RESTRICT", "n": "SET NULL", "d": "SET DEFAULT"}
		for _, f := range strs(t, db, `SELECT `+fmt.Sprintf(attnames, "x.conkey", "x.conrelid")+`,
				x.confrelid::regclass::text, `+fmt.Sprintf(attnames, "x.confkey", "x.confrelid")+`,
				x.confupdtype::text, x.confdeltype::text, x.condeferrable AND NOT x.condeferred
			FROM pg_constraint x WHERE x.contype = 'f' AND x.conrelid = `+rel, table) {
			fk := fmt.Sprintf("(%s) → %s(%s) ON UPDATE %s ON DELETE %s", f[0], f[1], f[2], action[f[3]], action[f[4]])
			if f[5] == "true" {
				fk += " DEFERRABLE"
			}
			s.FKs = append(s.FKs, fk)
		}
		sort.Strings(s.FKs)
		s.Checks = count(t, db, `SELECT count(*) FROM pg_constraint WHERE contype = 'c' AND conrelid = format('%I', '`+table+`')::regclass`)
		out[table] = s
	}
	trig := map[string]bool{}
	for _, tr := range strs(t, db, `SELECT c.relname, t.tgname, (t.tgtype & 1) = 1 FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace`) {
		level := " 문"
		if tr[2] == "true" {
			level = " 행"
		}
		trig[tr[0]+"."+tr[1]+level] = true
	}
	return out, trig
}
