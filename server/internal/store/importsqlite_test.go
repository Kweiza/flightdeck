package store

// SQLite → Postgres 이관(ImportSQLite)과 대조(VerifySQLite)를 실물 두 원장으로 잰다: 표 전부를 채운 원본의 왕복,
// 사전 검사 갈래마다의 거절(아무것도 안 쓴다), 판 거절, 빈 원장이 아닌 대상의 거절, 지워진 최고 번호 뒤의 순번, 그리고
// 옮긴 뒤 바뀐 행 · 순번을 대조가 잡는지.

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

const importAt = "'2026-01-02T03:04:05.000006Z'"

// importSeed 는 이관 카탈로그의 표 전부에 행을 넣는다. 일부러 넣은 것:
//   - NULL 과 빈 글이 같은 칸에 함께(project.archived_at · session.label · ref_state.subject)
//   - 글의 따옴표 · 줄바꿈 · 한글, BLOB 의 0x00 과 빈 BLOB(idempotency.body)
//   - 지워진 최고 번호: pick_eval 은 3번을, resource_hold 는 2번을 지워 sqlite_sequence 가 max(id) 보다 크다
//   - item_after 의 rowid 틈(3 · 5 · 7)과 넣은 순서와 다른 rowid 순서
//   - 자기 참조(judgment.supersedes)
const importSeed = `
INSERT INTO project(id, path, remote_url, default_branch, config, config_from_sha, created_at, pinned_at, archived_at) VALUES
  ('p', '/r/p', 'git@example.invalid:p.git', 'main', NULL, NULL, ` + importAt + `, NULL, NULL),
  ('p2', '/r/p2', NULL, 'trunk', '{"k":1}', 'abc', ` + importAt + `, ` + importAt + `, '');
INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m', 'h', ` + importAt + `, ` + importAt + `);
INSERT INTO session(id, project, machine_id, worktree, cc_session_id, label, state, blocked_why, opened_at, harness) VALUES
  ('s1', 'p', 'm', '/w1', 'c1', NULL, 'active', NULL, ` + importAt + `, NULL),
  ('s2', 'p', 'm', '/w2', 'c2', '', 'blocked', '기다린다', ` + importAt + `, 'codex');
INSERT INTO session_workspace(session_id, project, path, is_primary) VALUES ('s1', 'p', '/w1', 1), ('s1', 'p2', '/w1/sub', 0);
INSERT INTO signal(session_id, kind, at) VALUES ('s1', 'prompt', ` + importAt + `), ('s2', 'tool', ` + importAt + `);
INSERT INTO footprint(session_id, path, origin, first_at, last_at) VALUES
  ('s1', 'a.go', 'observed', ` + importAt + `, ` + importAt + `), ('s1', 'a.go', 'declared', ` + importAt + `, ` + importAt + `);
INSERT INTO ref_state(project, ref, sha, subject, at) VALUES ('p', 'main', 'sha1', NULL, ` + importAt + `), ('p', 'dev', 'sha2', '', ` + importAt + `);
INSERT INTO change_set(project, base_sha, head_sha, paths, computed_at) VALUES ('p', 'b', 'h', '["a.go"]', ` + importAt + `);
INSERT INTO item(project, id, title, body, paths, labels, state, close_reason, landed_ref, created_at, closed_at) VALUES
  ('p', 'i1', '제목', '본문 "따옴표"
둘째 줄', '["a.go"]', '[]', 'open', NULL, NULL, ` + importAt + `, NULL),
  ('p', 'i2', 't2', '', '[]', '["x"]', 'dropped', '사유', NULL, ` + importAt + `, ` + importAt + `),
  ('p', 'i3', 't3', 'b', '[]', '[]', 'claimed', NULL, NULL, ` + importAt + `, NULL);
INSERT INTO item_after(rowid, project, item_id, dep_item, dep_job, dep_sha, dep_project) VALUES
  (7, 'p', 'i1', NULL, NULL, 'sha9', 'p2'), (3, 'p', 'i1', 'i2', NULL, NULL, NULL), (5, 'p', 'i3', NULL, 'j1', NULL, NULL);
INSERT INTO claim(project, item_id, session_id, at, released_at, force_reason) VALUES ('p', 'i3', 's1', ` + importAt + `, NULL, NULL);
INSERT INTO pick_eval(project, session_id, at, picked, rejected, picked_with) VALUES
  ('p', 's1', ` + importAt + `, 'i1', '[]', NULL),
  ('p', 's1', ` + importAt + `, NULL, '[{"item":"i2","reason_code":"x"}]', '["i3"]'),
  ('p', 's2', ` + importAt + `, NULL, '[]', NULL);
DELETE FROM pick_eval WHERE id = 3;
INSERT INTO judgment(id, project, session_id, at, kind, title, body, supersedes) VALUES
  ('j1', 'p', 's1', ` + importAt + `, 'decision', 't', '판단을 남긴다', NULL),
  ('j2', NULL, NULL, ` + importAt + `, 'now', NULL, '정정한다', 'j1');
INSERT INTO judgment_link(judgment_id, target_kind, target_id, target_project) VALUES ('j1', 'item', 'i1', NULL), ('j2', 'commit', 'sha1', 'p');
INSERT INTO snapshot(project, key, value, method, evidence, input_digest, computed_at) VALUES
  ('p', 'k1', 'v', 'command', NULL, NULL, ` + importAt + `), ('p', 'k2', 'v', 'manual', '근거', 'd', ` + importAt + `);
INSERT INTO counter(project, name, value) VALUES ('p', 'item', 7);
INSERT INTO resource_hold(project, resource, session_id, job_id, acquired_at, released_at, force_reason) VALUES
  ('p', 'env:db', 's1', NULL, ` + importAt + `, NULL, NULL), ('p', 'env:x', NULL, 'j1', ` + importAt + `, ` + importAt + `, 'f');
DELETE FROM resource_hold WHERE id = 2;
INSERT INTO job(id, project, kind, item_id, session_id, branch, recipe, state, fail_kind, queued_at, started_at, ended_at,
    beat_at, base_sha, head_sha, landed_sha, image_tag, ack_at, log_tail) VALUES
  ('j1', 'p', 'land', 'i1', 's1', 'i1', NULL, 'fail', 'verify', ` + importAt + `, ` + importAt + `, ` + importAt + `,
    NULL, 'b', 'h', NULL, NULL, NULL, '로그 꼬리');
INSERT INTO event(at, project, session_id, kind, payload) VALUES
  (` + importAt + `, 'p', 's1', 'k', '{}'), (` + importAt + `, NULL, NULL, 'k2', '{"a":[1,2]}');
INSERT INTO idempotency(key, fingerprint, status, ctype, body, at) VALUES
  ('key1', 'fp', 200, 'application/json', X'7B0022', ` + importAt + `), ('key2', 'fp2', 409, 'text/plain', X'', ` + importAt + `);
INSERT INTO landing_queue(project, session_id, enqueued_at, left_at, left_kind, left_detail) VALUES
  ('p', 's1', ` + importAt + `, NULL, NULL, NULL), ('p', 's2', ` + importAt + `, ` + importAt + `, 'fail', '까닭');
INSERT INTO landing_queue_resource(row_id, resource) VALUES (1, 'env:db');
INSERT INTO project_member(root_project, member_project, path, declared_at) VALUES ('p', 'p2', 'sub', ` + importAt + `);
INSERT INTO item_revision(project, item_id, rev, at, session_id, title, body, paths, reason) VALUES
  ('p', 'i1', 1, ` + importAt + `, 's1', 't', 'b', '[]', 'r'), ('p', 'i1', 2, ` + importAt + `, NULL, 't', 'b2', '["a"]', 'r2');
INSERT INTO session_inbox(session_id, machine_id, socket, mode, seen_at) VALUES ('s1', 'm', '/run/s.sock', '', ` + importAt + `);
`

// importSource 는 판 18 SQLite 원장 파일을 만들고 seed 를 넣는다. 파일 경로를 낸다.
func importSource(t *testing.T, seed string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fd.db")
	mustMigrate(t, path)
	if seed != "" {
		liteExec(t, path, seed)
	}
	return path
}

// liteExec 는 SQLite 원장 파일에 문장을 넣는다. FK 를 끈 연결이다 - 고장 난 원본(부모 없는 행)도 만들 수 있게.
func liteExec(t *testing.T, path, q string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("원본 준비 실패: %v\n%s", err, q)
	}
}

// importTarget 은 기준선 · 판 행이 있는 빈 Postgres 원장을 만든다. 접속 URL 과, 같은 원장을 연 Store(대상 재기용)를 낸다.
func importTarget(t *testing.T) (string, *Store) {
	t.Helper()
	dsn := storetest.New(t)
	s, err := openPG(ParseTarget(dsn), quietLog(), lockTimeout)
	if err != nil {
		t.Fatalf("openPG: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	stamp(t, s, pg.ImportVersion)
	return dsn, s
}

// targetRows 는 대상의 업무 표 전부의 행 수 합이다(아무것도 안 썼는가의 잣대).
func targetRows(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	for _, tb := range pg.Tables() {
		n += pgCount(t, s, `SELECT count(*) FROM `+tb.Name)
	}
	return n
}

// 표 전부를 채운 원본이 옮겨지고, 커밋 전 대조와 따로 돈 대조가 둘 다 맞으며, NULL · 빈 글 · BLOB · rowid 순서가 그대로다.
func TestImportSQLiteRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	// 묶음 INSERT 의 끊는 자리 둘을 지나게 한다: 행 수(batchRows 를 두 번 넘는 event)와 값 바이트(batchBytes 를 넘는 BLOB 하나
	// 뒤에 행이 더 있다).
	liteExec(t, src, `INSERT INTO event(at, kind, payload)
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1203)
		SELECT `+importAt+`, 'bulk', '{"i":' || i || '}' FROM n;
	INSERT INTO idempotency(key, fingerprint, status, ctype, body, at) VALUES
		('big', 'fp', 200, 'application/octet-stream', randomblob(9437184), `+importAt+`),
		('after-big', 'fp', 200, 'text/plain', X'00', `+importAt+`);`)
	dsn, dst := importTarget(t)

	rep, err := ImportSQLite(ctx, src, dsn, ImportOptions{}, quietLog())
	if err != nil {
		t.Fatalf("이관 실패: %v\n%+v", err, rep)
	}
	// 시드가 표 전부를 덮어야 왕복이 표 전부를 잰다.
	if len(rep.Rows) != len(pg.Tables()) {
		t.Fatalf("보고의 표 수 %d, 카탈로그 %d", len(rep.Rows), len(pg.Tables()))
	}
	total := 0
	for _, r := range rep.Rows {
		if r.Rows == 0 {
			t.Errorf("시드가 %s 를 안 채웠다", r.Table)
		}
		total += r.Rows
	}
	if !rep.Matched() || rep.Copied != total || int64(total) != targetRows(t, dst) {
		t.Fatalf("옮긴 행 %d · 원본 %d · 대상 %d · 대조 %v", rep.Copied, total, targetRows(t, dst), rep.Matched())
	}
	if strings.Contains(rep.To, "@") || rep.To != "postgres" {
		t.Fatalf("보고의 대상 표시값이 %q 다", rep.To)
	}

	again, err := VerifySQLite(ctx, src, dsn, quietLog())
	if err != nil || !again.Matched() {
		t.Fatalf("옮긴 뒤 대조: %v\n%+v", err, again.Tables)
	}

	for q, want := range map[string]int64{
		`SELECT count(*) FROM project WHERE id = 'p2' AND archived_at = ''`:                     1,
		`SELECT count(*) FROM project WHERE id = 'p' AND archived_at IS NULL`:                   1,
		`SELECT count(*) FROM session WHERE id = 's1' AND label IS NULL`:                        1,
		`SELECT count(*) FROM session WHERE id = 's2' AND label = ''`:                           1,
		`SELECT count(*) FROM idempotency WHERE key = 'key1' AND body = '\x7b0022'::bytea`:      1,
		`SELECT count(*) FROM idempotency WHERE key = 'key2' AND body = ''::bytea`:              1,
		`SELECT count(*) FROM item WHERE id = 'i1' AND body = E'본문 "따옴표"\n둘째 줄'`:                1,
		`SELECT count(*) FROM judgment WHERE id = 'j2' AND supersedes = 'j1'`:                   1,
		`SELECT count(*) FROM schema_version`:                                                   1,
		`SELECT count(*) FROM snapshot WHERE key_digest = fd_key_digest('k2')`:                  1,
		`SELECT count(*) FROM item_after WHERE seq IN (3, 5, 7)`:                                3,
		`SELECT count(*) FROM item_after WHERE seq = 3 AND dep_item = 'i2'`:                     1,
		`SELECT count(*) FROM pick_eval WHERE id IN (1, 2)`:                                     2,
		`SELECT count(*) FROM event WHERE kind = 'bulk'`:                                        1203,
		`SELECT count(*) FROM idempotency WHERE key = 'big' AND length(body) = 9437184`:         1,
		`SELECT count(*) FROM landing_queue_resource r JOIN landing_queue q ON q.id = r.row_id`: 1,
	} {
		if got := pgCount(t, dst, q); got != want {
			t.Errorf("%s = %d, 기대 %d", q, got, want)
		}
	}
}

// identity 다음 번호는 원본에서 발급된 최고 번호 뒤다 - 지워진 최고 번호(sqlite_sequence)도 다시 내지 않는다.
func TestImportSQLiteKeepsDeletedTopNumbers(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	dsn, dst := importTarget(t)
	rep, err := ImportSQLite(ctx, src, dsn, ImportOptions{}, quietLog())
	if err != nil {
		t.Fatalf("이관 실패: %v", err)
	}
	floors := map[string]int64{}
	for _, c := range rep.Sequences {
		floors[c.Table] = c.Floor
	}
	// pick_eval 은 1 · 2 가 남고 3 이 지워졌다. resource_hold 는 1 이 남고 2 가 지워졌다. item_after 는 rowid 최대가 7 이다.
	want := map[string]int64{"pick_eval": 3, "resource_hold": 2, "event": 2, "landing_queue": 2, "item_after": 7}
	for table, floor := range want {
		if floors[table] != floor {
			t.Errorf("%s 의 발급 최고 번호 %d, 기대 %d", table, floors[table], floor)
		}
	}
	for _, c := range []struct {
		insert string
		want   int64
	}{
		{`INSERT INTO pick_eval(project, session_id, at, rejected) VALUES ('p', 's1', 'x', '[]') RETURNING id`, 4},
		{`INSERT INTO resource_hold(project, resource, session_id, acquired_at) VALUES ('p', 'r', 's1', 'x') RETURNING id`, 3},
		{`INSERT INTO event(at, kind) VALUES ('x', 'k') RETURNING id`, 3},
		{`INSERT INTO landing_queue(project, session_id, enqueued_at) VALUES ('p', 's2', 'x') RETURNING id`, 3},
		{`INSERT INTO item_after(project, item_id, dep_sha) VALUES ('p', 'i1', 's') RETURNING seq`, 8},
	} {
		if got := pgCount(t, dst, c.insert); got != c.want {
			t.Errorf("%s → %d, 기대 %d", c.insert, got, c.want)
		}
	}
}

// 사전 검사의 갈래마다: 걸린 행을 표 · 칸 · 사유 · 행 좌표로 내고, 대상에 아무것도 쓰지 않는다. 예행도 같은 것을 낸다.
func TestImportSQLiteRefusesFaultsAndWritesNothing(t *testing.T) {
	for _, c := range []struct {
		name, break_          string
		table, column, keyHas string
		fault                 pg.Fault
	}{
		{"기본키 NULL", `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES (NULL, 'h', 'x', 'x')`,
			"machine", "id", "id=NULL", pg.FaultKeyNull},
		{"NUL 바이트", `UPDATE item SET body = CAST(X'610062' AS TEXT) WHERE id = 'i1'`,
			"item", "body", `id="i1"`, pg.FaultNUL},
		{"깨진 UTF-8", `UPDATE item SET title = CAST(X'FF' AS TEXT) WHERE id = 'i2'`,
			"item", "title", `id="i2"`, pg.FaultUTF8},
		{"정수 칸의 글", `UPDATE counter SET value = 'abc'`, "counter", "value", `name="item"`, pg.FaultStorage},
		{"정수 칸의 실수", `UPDATE counter SET value = 1.5`, "counter", "value", `name="item"`, pg.FaultStorage},
		{"깨진 JSON", `UPDATE item SET paths = '[' WHERE id = 'i3'`, "item", "paths", `id="i3"`, pg.FaultJSON},
		{"JSON 의 \\u0000", `UPDATE item SET labels = '["a\u0000"]' WHERE id = 'i1'`, "item", "labels", `id="i1"`, pg.FaultJSONNUL},
		{"부모 없는 행", `INSERT INTO claim(project, item_id, session_id, at) VALUES ('p', 'i2', 'nobody', 'x')`,
			"claim", "session_id", "→ session", pg.FaultForeignKey},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			src := importSource(t, importSeed)
			liteExec(t, src, c.break_)
			dsn, dst := importTarget(t)
			for _, dry := range []bool{true, false} {
				rep, err := ImportSQLite(ctx, src, dsn, ImportOptions{DryRun: dry}, quietLog())
				if err == nil || !strings.Contains(err.Error(), "사전 검사") {
					t.Fatalf("예행 %v: 걸린 원본을 받았다: %v", dry, err)
				}
				var hit *FaultGroup
				for i, g := range rep.Faults {
					if g.Table == c.table && g.Column == c.column && g.Fault == c.fault {
						hit = &rep.Faults[i]
					}
				}
				if hit == nil || hit.Count != 1 || len(hit.Keys) != 1 || !strings.Contains(hit.Keys[0], c.keyHas) {
					t.Fatalf("예행 %v: %s.%s %q 갈래가 없거나 좌표가 틀렸다: %+v", dry, c.table, c.column, c.fault, rep.Faults)
				}
				if len(rep.Faults) != 1 {
					t.Errorf("예행 %v: 걸린 갈래가 하나가 아니다: %+v", dry, rep.Faults)
				}
				if n := targetRows(t, dst); n != 0 {
					t.Fatalf("예행 %v: 거절했는데 대상에 %d 행이 있다", dry, n)
				}
			}
		})
	}
}

// 예행은 걸린 것이 없으면 성공하고 아무것도 쓰지 않는다. 대상이 Postgres 가 아니면 원본만 잰다.
func TestImportSQLiteDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	dsn, dst := importTarget(t)
	rep, err := ImportSQLite(ctx, src, dsn, ImportOptions{DryRun: true}, quietLog())
	if err != nil || !rep.TargetChecked || rep.Copied != 0 || len(rep.Rows) != len(pg.Tables()) {
		t.Fatalf("예행: %v %+v", err, rep)
	}
	if n := targetRows(t, dst); n != 0 {
		t.Fatalf("예행이 대상에 %d 행을 썼다", n)
	}
	other := filepath.Join(t.TempDir(), "not-there.db")
	rep, err = ImportSQLite(ctx, src, other, ImportOptions{DryRun: true}, quietLog())
	if err != nil || rep.TargetChecked {
		t.Fatalf("Postgres 아닌 대상의 예행: %v, 대상을 쟀다 %v", err, rep.TargetChecked)
	}
	if _, err := ImportSQLite(ctx, src, other, ImportOptions{}, quietLog()); err == nil || !strings.Contains(err.Error(), "Postgres 접속 URL") {
		t.Fatalf("Postgres 아닌 대상에 옮겼다: %v", err)
	}
}

// 대상이 빈 원장이 아니면(한 번 옮긴 뒤 다시) 거절하고, 판 행이 없는 빈 DB 면 기준선부터 깔라고 한다.
func TestImportSQLiteRefusesATargetThatIsNotEmpty(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	dsn, dst := importTarget(t)
	if _, err := ImportSQLite(ctx, src, dsn, ImportOptions{}, quietLog()); err != nil {
		t.Fatalf("첫 이관: %v", err)
	}
	before := targetRows(t, dst)
	for _, dry := range []bool{true, false} {
		_, err := ImportSQLite(ctx, src, dsn, ImportOptions{DryRun: dry}, quietLog())
		if err == nil || !strings.Contains(err.Error(), "비어 있지 않다") {
			t.Fatalf("예행 %v: 찬 대상에 다시 옮겼다: %v", dry, err)
		}
	}
	if after := targetRows(t, dst); after != before {
		t.Fatalf("거절했는데 대상 행 수가 %d → %d", before, after)
	}

	bare := storetest.NewBare(t, "")
	if _, err := ImportSQLite(ctx, src, bare, ImportOptions{}, quietLog()); err == nil || !strings.Contains(err.Error(), "--to 18") {
		t.Fatalf("빈 DB 에 옮겼거나 처방이 없다: %v", err)
	}
}

// 판 18 보다 낮은 원본은 먼저 올리라는 안내와 함께 거절한다(예행도). 이 명령은 옛 증분을 갖지 않는다.
func TestImportSQLiteRefusesAnOlderSource(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	mustMigrateTo(t, path, pg.ImportVersion-1)
	dsn, dst := importTarget(t)
	for _, dry := range []bool{true, false} {
		_, err := ImportSQLite(ctx, path, dsn, ImportOptions{DryRun: dry}, quietLog())
		if err == nil || !strings.Contains(err.Error(), "판 17") || !strings.Contains(err.Error(), "fd migrate --db") {
			t.Fatalf("예행 %v: 판 17 원본을 받았거나 안내가 없다: %v", dry, err)
		}
	}
	if _, err := VerifySQLite(ctx, path, dsn, quietLog()); err == nil || !strings.Contains(err.Error(), "판 17") {
		t.Fatalf("대조가 판 17 원본을 받았다: %v", err)
	}
	if n := targetRows(t, dst); n != 0 {
		t.Fatalf("거절했는데 대상에 %d 행", n)
	}
	// 원본을 바꾸지 않았다 - 여전히 17 이다.
	var v int
	liteQuery(t, path, `SELECT max(version) FROM schema_version`, &v)
	if v != pg.ImportVersion-1 {
		t.Fatalf("거절한 원본의 판이 %d 로 바뀌었다", v)
	}
}

func liteQuery(t *testing.T, path, q string, dest ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(q).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// 옮긴 뒤 대상의 한 칸 · 한 행 · 순번이 바뀌면 대조가 그 표와 행을 짚어 어긋남으로 낸다. NULL 을 빈 글로 바꾼 것도 잡는다.
func TestVerifySQLiteCatchesChangedRows(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	for _, c := range []struct {
		name, change, table, keyHas string
	}{
		{"글 한 칸", `UPDATE item SET title = '바뀜' WHERE id = 'i1'`, "item", `id="i1"`},
		{"NULL 을 빈 글로", `UPDATE session SET label = '' WHERE id = 's1'`, "session", `id="s1"`},
		{"빈 글을 NULL 로", `UPDATE ref_state SET subject = NULL WHERE ref = 'dev'`, "ref_state", `ref="dev"`},
		{"시각 한 자리", `UPDATE claim SET at = '2026-01-02T03:04:05.000007Z'`, "claim", `item_id="i3"`},
		{"행 하나 지움", `DELETE FROM signal WHERE kind = 'tool'`, "signal", `kind="tool"`},
		// identity 칸은 UPDATE 로 못 바꾼다 - 같은 내용을 다른 순번으로 다시 넣는다.
		{"순서 칸", `DELETE FROM item_after WHERE seq = 3;INSERT INTO item_after(project, item_id, dep_item, seq)
			OVERRIDING SYSTEM VALUE VALUES ('p', 'i1', 'i2', 4)`, "item_after", `seq=3`},
	} {
		t.Run(c.name, func(t *testing.T) {
			dsn, dst := importTarget(t)
			if _, err := ImportSQLite(ctx, src, dsn, ImportOptions{}, quietLog()); err != nil {
				t.Fatalf("이관: %v", err)
			}
			for _, q := range strings.Split(c.change, ";") {
				pgExec(t, dst, q)
			}
			rep, err := VerifySQLite(ctx, src, dsn, quietLog())
			if err == nil || rep.Matched() {
				t.Fatalf("바뀐 대상이 맞다고 나왔다: %v", err)
			}
			for _, tc := range rep.Tables {
				switch {
				case tc.Table == c.table && tc.OK():
					t.Fatalf("%s 가 맞다고 나왔다", c.table)
				case tc.Table == c.table && !slices.ContainsFunc(tc.OnlySource, func(k string) bool { return strings.Contains(k, c.keyHas) }):
					t.Fatalf("%s 의 어긋난 행 %v 에 %q 가 없다", c.table, tc.OnlySource, c.keyHas)
				case tc.Table != c.table && !tc.OK():
					t.Fatalf("바꾸지 않은 %s 가 어긋났다", tc.Table)
				}
			}
		})
	}
	t.Run("순번", func(t *testing.T) {
		dsn, dst := importTarget(t)
		if _, err := ImportSQLite(ctx, src, dsn, ImportOptions{}, quietLog()); err != nil {
			t.Fatalf("이관: %v", err)
		}
		pgExec(t, dst, `SELECT setval(pg_get_serial_sequence('pick_eval', 'id'), 2)`)
		rep, err := VerifySQLite(ctx, src, dsn, quietLog())
		if err == nil {
			t.Fatal("되감긴 순번이 맞다고 나왔다")
		}
		for _, c := range rep.Sequences {
			if (c.Table == "pick_eval") == c.OK() {
				t.Errorf("%s.%s 순번 판정 %v(다음 %d · 최고 %d)", c.Table, c.Column, c.OK(), c.Next, c.Floor)
			}
		}
	})
}
