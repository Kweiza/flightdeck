package store

// SQLite → Postgres 이관(ImportSQLite)과 대조(VerifySQLite)를 실물 두 원장으로 잰다: 표 전부를 채운 원본의 왕복,
// 사전 검사 갈래마다의 거절(아무것도 안 쓴다), NUL 을 바꿔 옮기는 규칙과 그 보고 · 역이관 왕복, 판 거절, 빈 원장이 아닌 대상(행 · 번호를 낸 순번)의 거절, 지워진 최고 번호
// 뒤의 순번, 예행이 본 실행과 같은 거절을 받고 아무것도 안 남기는지, 대조가 어긋나면 커밋하지 않는지, 비지 않은 -wal 과
// 경로 글자, 그리고 옮긴 뒤 바뀐 행 · 순번 · 이관이 받지 않을 원본을 대조가 잡는지.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/nulesc"
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

// importSource 는 이관 판 SQLite 원장 파일을 만들고 seed 를 넣는다. 파일 경로를 낸다.
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
	db, err := sql.Open("sqlite", sqliteURI(path, url.Values{"_pragma": {"foreign_keys(0)"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("원본 준비 실패: %v\n%s", err, q)
	}
}

// importTarget 은 기준선 · 판 행이 있는 빈 Postgres 원장을 만든다. 접속 URL 과, 같은 원장을 연 Store(대상 재기용)를 낸다.
//
// ★ 그 Store 는 연결을 쥐고 있지 않는다(SetMaxIdleConns(0) - 질의마다 열고 닫는다). 이관 · 역이관은 그 원장에 붙은 다른
// 연결을 거절한다(soleClient) - 재기용 Store 가 쉬는 연결을 쥐면 옮기기마다 그 Store 를 닫아야 한다.
func importTarget(t *testing.T) (string, *Store) {
	t.Helper()
	dsn := storetest.New(t)
	s, err := openPG(ParseTarget(dsn), quietLog(), lockTimeout)
	if err != nil {
		t.Fatalf("openPG: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.db.raw.SetMaxIdleConns(0)
	stamp(t, s, pg.ImportVersion)
	return dsn, s
}

// importLedger 는 ImportSQLite 다. 대상이 Postgres 면 앞 단계(채우기 · 앞 실행 · 재기)의 연결이 서버에서 다 빠지기를 먼저
// 기다린다 - 이관은 대상에 붙은 다른 연결을 거절한다(soleClient). 닫은 연결의 백엔드는 조금 늦게 빠진다.
func importLedger(t *testing.T, ctx context.Context, from, to string, opt TransferOptions) (TransferReport, error) {
	t.Helper()
	settleLedger(t, to)
	return ImportSQLite(ctx, from, to, opt, quietLog())
}

// exportLedger 는 ExportSQLite 다. 원본에 대해 importLedger 와 같이 기다린다.
func exportLedger(t *testing.T, ctx context.Context, from, to string, opt TransferOptions) (TransferReport, error) {
	t.Helper()
	settleLedger(t, from)
	return ExportSQLite(ctx, from, to, opt, quietLog())
}

// settleLedger 는 dsn 이 Postgres 원장이면 거기 붙은 클라이언트 연결이 다 빠지기를 기다린다(storetest.SettleClients).
func settleLedger(t *testing.T, dsn string) {
	t.Helper()
	if ParseTarget(dsn).Postgres() {
		storetest.SettleClients(t, dsn, 0)
	}
}

// identityStates 는 대상 identity 시퀀스마다 「last_value/is_called」다(순번을 건드렸는가의 잣대). 새 원장은 전부 1/false 다.
func identityStates(t *testing.T, s *Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tb := range pg.Tables() {
		c, ok := tb.Identity()
		if !ok {
			continue
		}
		seq, err := readIdentity(context.Background(), s.db, tb.Name, c.Name)
		if err != nil {
			t.Fatal(err)
		}
		out[tb.Name] = fmt.Sprintf("%d/%v", seq.last, seq.called)
	}
	return out
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

	rep, err := importLedger(t, ctx, src, dsn, TransferOptions{})
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

	again, err := VerifySQLite(ctx, src, dsn, VerifyOptions{}, quietLog())
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
	rep, err := importLedger(t, ctx, src, dsn, TransferOptions{})
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
		{"깨진 UTF-8", `UPDATE item SET title = CAST(X'FF' AS TEXT) WHERE id = 'i2'`,
			"item", "title", `id="i2"`, pg.FaultUTF8},
		{"정수 칸의 글", `UPDATE counter SET value = 'abc'`, "counter", "value", `name="item"`, pg.FaultStorage},
		{"정수 칸의 실수", `UPDATE counter SET value = 1.5`, "counter", "value", `name="item"`, pg.FaultStorage},
		{"깨진 JSON", `UPDATE item SET paths = '[' WHERE id = 'i3'`, "item", "paths", `id="i3"`, pg.FaultJSON},
		// 글 안의 날 NUL 은 JSON 문서가 아니다(제어 문자는 이스케이프해야 한다) - 바꿔 넣는 갈래(\u0000)와 다르다.
		{"JSON 칸의 날 NUL", `UPDATE item SET labels = CAST(X'5B226100225D' AS TEXT) WHERE id = 'i1'`, "item", "labels", `id="i1"`, pg.FaultJSON},
		{"부모 없는 행", `INSERT INTO claim(project, item_id, session_id, at) VALUES ('p', 'i2', 'nobody', 'x')`,
			"claim", "session_id", "→ session", pg.FaultForeignKey},
		// 짝 없는 서러게이트는 jsonb 가 받지 않는다. 디코드해 다시 쓰면 U+FFFD 가 되어 다른 두 키가 하나가 된다 - 거절한다.
		{"JSON 의 짝 없는 서러게이트", `UPDATE item SET labels = '{"\ud800\u0000":1,"\ufffd\u0000":2}' WHERE id = 'i1'`,
			"item", "labels", `id="i1"`, pg.FaultJSONSurrogate},
		// 옮기며 같아지는 기본키 · UNIQUE 칸: NUL 을 바꿔 넣은 id 가 원래 `\x00` 네 글자를 담은 id 와, 글 칸의 BLOB 가 같은 바이트의
		// 글과 같아진다(대상이 커밋 때 거절할 것을 좌표와 함께 먼저 낸다). 부분 인덱스는 그 조건 안의 행끼리만 잰다 - 놓은 점유는
		// 살아 있는 점유와 겹쳐도 된다.
		{"NUL 을 바꿔 넣어 겹치는 기본키", `INSERT INTO judgment(id, project, session_id, at, kind, title, body) VALUES
				('a\x00', 'p', 's1', 'x', 'decision', 't', 'b'), (CAST(X'6100' AS TEXT), 'p', 's1', 'x', 'decision', 't', 'b')`,
			"judgment", "id", `id="a\\x00"`, pg.FaultUnique},
		{"BLOB 로 겹치는 지문 키", `INSERT INTO snapshot(project, key, value, method, computed_at) VALUES ('p', X'6B31', 'v', 'command', 'x')`,
			"snapshot", "project,key", `key="k1"`, pg.FaultUnique},
		{"BLOB 로 겹치는 살아 있는 점유", `INSERT INTO resource_hold(project, resource, session_id, acquired_at, released_at) VALUES
				('p', X'656E763A6462', 's1', 'x', NULL), ('p', X'656E763A6462', 's1', 'x', 'x')`,
			"resource_hold", "project,resource", "id=3", pg.FaultUnique},
		// 글자 수(700)는 상한 아래지만 바이트(2100)는 넘는다 - 인덱스는 바이트로 받는다.
		{"인덱스 키 상한을 넘는 글", `INSERT INTO event(at, project, kind, payload) VALUES ('x', '` + strings.Repeat("가", 700) + `', 'k3', '{}')`,
			"event", "project", "id=3", pg.FaultTooLong},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			src := importSource(t, importSeed)
			liteExec(t, src, c.break_)
			dsn, dst := importTarget(t)
			for _, dry := range []bool{true, false} {
				rep, err := importLedger(t, ctx, src, dsn, TransferOptions{DryRun: dry})
				if err == nil || !strings.Contains(err.Error(), "사전 검사") {
					t.Fatalf("예행 %v: 걸린 원본을 받았다: %v", dry, err)
				}
				// 인덱스 키 상한에 걸린 감사 행은 고치라고 하지 않는다 - 자르면 원본과 옮긴 원장이 달라진다.
				fixIt, report := strings.Contains(err.Error(), "고친 뒤"), strings.Contains(err.Error(), "이슈로 알려 달라")
				if tooLong := c.fault == pg.FaultTooLong; fixIt == tooLong || report != tooLong {
					t.Fatalf("예행 %v: 다음 수가 갈래(%s)와 다르다: %v", dry, c.fault, err)
				}
				var hit *FaultGroup
				for i, g := range rep.Faults {
					if g.Table == c.table && g.Column == c.column && g.Reason == c.fault {
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

// NUL 이 든 원본은 거절하지 않고 정해진 규칙(nulesc)으로 바꿔 옮긴다: 글 칸의 NUL 바이트(판단 본문 - 같은 칸에 원래 있던 \x00
// 표기와 함께, 항목 본문)와 JSON 칸의 \u0000(항목 라벨 - 같은 행, 이벤트 내용 - U+0000 이 든 글만, 같은 문서의 \x00 표기는
// 그대로). 예행 · 이관 · 대조가 바꾼 칸을 표 · 칸 · 규칙 · 행 좌표와 바꾼 행 수로 내고(조용히 바꾸지 않는다), 이관은 바꾼 칸
// 전부를 원래 바이트와 함께 기록 파일로 남기고 이관 표지가 그 파일을 댄다. 대조는 바꾼 값으로 맞대 맞다. 옮긴 JSON 은 jsonb 로
// 읽힌다. 역이관은 Postgres 값을 그대로 낸다 - 낸 파일에는 NUL 이 없고, 그 파일과 Postgres 의 변환 없는 대조가 맞다.
func TestImportSQLiteRewritesNULAndReportsIt(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	blobText := func(s string) string { return fmt.Sprintf("CAST(X'%x' AS TEXT)", s) }
	liteExec(t, src, `INSERT INTO judgment(id, project, session_id, at, kind, title, body) VALUES
			('j3', 'p', 's1', `+importAt+`, 'decision', 't', `+blobText(`표기 \x00 와 날 `+"\x00"+` 끝`)+`);
		UPDATE item SET body = `+blobText("a\x00b")+`, labels = '["l\u0000"]' WHERE id = 'i1';
		INSERT INTO event(at, project, session_id, kind, payload) VALUES
			(`+importAt+`, 'p', 's1', 'nul', '{"m":"x\u0000y","n":"\\x00"}');`)
	dsn, dst := importTarget(t)

	type cell struct {
		table, column string
		rule          pg.Rewrite
	}
	want := map[cell]string{ // 바꾼 칸마다 행 좌표에 든 글
		{"item", "body", pg.RewriteNUL}:         `id="i1"`,
		{"item", "labels", pg.RewriteJSONNUL}:   `id="i1"`,
		{"judgment", "body", pg.RewriteNUL}:     `id="j3"`,
		{"event", "payload", pg.RewriteJSONNUL}: `id=`,
	}
	checkRewrites := func(what string, rep TransferReport) {
		t.Helper()
		got := map[cell]string{}
		for _, g := range rep.Rewrites {
			if g.Count != 1 || len(g.Keys) != 1 {
				t.Errorf("%s: %s.%s 갈래의 수 %d · 좌표 %v", what, g.Table, g.Column, g.Count, g.Keys)
				continue
			}
			got[cell{g.Table, g.Column, g.Reason}] = g.Keys[0]
		}
		if len(got) != len(want) {
			t.Errorf("%s: 바꾼 칸 갈래 %+v, 기대 %d 갈래", what, rep.Rewrites, len(want))
		}
		for c, keyHas := range want {
			if k, ok := got[c]; !ok || !strings.Contains(k, keyHas) {
				t.Errorf("%s: %s.%s %q 갈래가 없거나 좌표 %q 에 %q 가 없다", what, c.table, c.column, c.rule, k, keyHas)
			}
		}
		// 항목 i1 은 칸 둘이 바뀌었어도 한 행이다.
		if rep.RewrittenRows != 3 {
			t.Errorf("%s: 바꾼 행 %d, 기대 3", what, rep.RewrittenRows)
		}
		if len(rep.Faults) != 0 || len(rep.TargetFaults) != 0 {
			t.Errorf("%s: 걸린 갈래 %+v · %+v", what, rep.Faults, rep.TargetFaults)
		}
	}

	rep, err := importLedger(t, ctx, src, dsn, TransferOptions{DryRun: true})
	if err != nil || !rep.Matched() {
		t.Fatalf("예행: %v\n%+v", err, rep)
	}
	checkRewrites("예행", rep)
	if n := targetRows(t, dst); n != 0 {
		t.Fatalf("예행이 대상에 %d 행을 썼다", n)
	}
	if left, _ := filepath.Glob(src + rewrittenSuffix + "*"); rep.RewriteRecord != "" || len(left) != 0 {
		t.Fatalf("예행이 바꾼 칸의 기록을 남겼다: %q %v", rep.RewriteRecord, left)
	}
	rep, err = importLedger(t, ctx, src, dsn, TransferOptions{})
	if err != nil || !rep.Matched() || rep.Copied == 0 {
		t.Fatalf("이관: %v\n%+v", err, rep)
	}
	checkRewrites("이관", rep)

	// 바꾼 칸의 기록: 칸마다 한 줄 - 행의 전체 키 · 칸 · 규칙 · JSON 자리 · 원래 바이트. 이관 표지가 그 파일을 댄다.
	if !strings.HasPrefix(rep.RewriteRecord, src+rewrittenSuffix+"-") || !strings.HasSuffix(rep.RewriteRecord, ".jsonl") {
		t.Fatalf("기록 파일 이름 %q", rep.RewriteRecord)
	}
	type recordWant struct {
		key      string
		original string
		paths    []nulesc.Change
	}
	wantRecord := map[cell]recordWant{
		{"item", "body", pg.RewriteNUL}:         {`{"id":"i1","project":"p"}`, "a\x00b", nil},
		{"item", "labels", pg.RewriteJSONNUL}:   {`{"id":"i1","project":"p"}`, `["l\u0000"]`, []nulesc.Change{{Pointer: "/0"}}},
		{"judgment", "body", pg.RewriteNUL}:     {`{"id":"j3"}`, `표기 \x00 와 날 ` + "\x00" + ` 끝`, nil},
		{"event", "payload", pg.RewriteJSONNUL}: {`{"id":3}`, `{"m":"x\u0000y","n":"\\x00"}`, []nulesc.Change{{Pointer: "/m"}}},
	}
	record := readRecord(t, rep.RewriteRecord)
	if len(record) != len(wantRecord) {
		t.Fatalf("기록 %d 줄, 기대 %d 줄: %+v", len(record), len(wantRecord), record)
	}
	for _, r := range record {
		w, ok := wantRecord[cell{r.Table, r.Column, r.Rule}]
		key, _ := json.Marshal(r.Key)
		if !ok || string(key) != w.key || string(r.Original) != w.original || !reflect.DeepEqual(r.Paths, w.paths) {
			t.Errorf("기록 줄 %+v(키 %s · 원래 %q), 기대 %+v", r, key, r.Original, w)
		}
	}
	if note, err := os.ReadFile(MovedMarker(src)); err != nil || !strings.Contains(string(note), `"rewritten": "`+rep.RewriteRecord+`"`) {
		t.Fatalf("이관 표지가 기록 파일을 대지 않는다: %v\n%s", err, note)
	}

	// 옮긴 값: NUL 은 \x00 네 글자, 같은 칸에 원래 있던 \x00 표기는 백슬래시가 두 배다. JSON 은 U+0000 이 든 글에만 같은 규칙을
	// 건다 - 같은 문서의 다른 글(n)은 원래 바이트다.
	moved := map[string]string{
		`SELECT body FROM judgment WHERE id = 'j3'`:                   `표기 \\x00 와 날 \x00 끝`,
		`SELECT body FROM item WHERE id = 'i1'`:                       `a\x00b`,
		`SELECT labels FROM item WHERE id = 'i1'`:                     `["l\\x00"]`,
		`SELECT payload FROM event WHERE kind = 'nul'`:                `{"m":"x\\x00y","n":"\\x00"}`,
		`SELECT (payload::jsonb)->>'m' FROM event WHERE kind = 'nul'`: `x\x00y`,
		`SELECT (labels::jsonb)->>0 FROM item WHERE id = 'i1'`:        `l\x00`,
	}
	for q, v := range moved {
		if got := pgString(t, dst, q); got != v {
			t.Errorf("%s = %q, 기대 %q", q, got, v)
		}
	}

	again, err := VerifySQLite(ctx, src, dsn, VerifyOptions{}, quietLog())
	if err != nil || !again.Matched() {
		t.Fatalf("옮긴 뒤 대조: %v\n%+v", err, again.Tables)
	}
	checkRewrites("대조", again)

	back := exportBack(t)
	out, err := exportLedger(t, ctx, dsn, back, TransferOptions{})
	if err != nil || len(out.Rewrites) != 0 || out.RewrittenRows != 0 {
		t.Fatalf("역이관: %v, 바꾼 칸 %+v · %d 행", err, out.Rewrites, out.RewrittenRows)
	}
	for q, v := range map[string]string{
		`SELECT body FROM judgment WHERE id = 'j3'`:    moved[`SELECT body FROM judgment WHERE id = 'j3'`],
		`SELECT body FROM item WHERE id = 'i1'`:        moved[`SELECT body FROM item WHERE id = 'i1'`],
		`SELECT payload FROM event WHERE kind = 'nul'`: moved[`SELECT payload FROM event WHERE kind = 'nul'`],
	} {
		var got string
		liteQuery(t, back, q, &got)
		if got != v {
			t.Errorf("역이관 %s = %q, 기대 %q(Postgres 값 그대로)", q, got, v)
		}
	}
	var nul int64
	liteQuery(t, back, `SELECT (SELECT count(*) FROM judgment WHERE instr(CAST(body AS BLOB), X'00') > 0)
		+ (SELECT count(*) FROM item WHERE instr(CAST(body AS BLOB), X'00') > 0)`, &nul)
	if nul != 0 {
		t.Fatalf("역이관이 NUL 을 되살렸다: %d 행", nul)
	}
	settleLedger(t, dsn)
	if rep, err := VerifySQLite(ctx, back, dsn, VerifyOptions{Exported: true}, quietLog()); err != nil || !rep.Matched() || len(rep.Rewrites) != 0 {
		t.Fatalf("역이관 파일의 대조: %v, 바꾼 칸 %+v", err, rep.Rewrites)
	}
}

// readRecord 는 바꾼 칸의 기록 파일 path 를 줄마다 읽는다. 줄마다 모르는 키 없는 JSON 하나여야 한다.
func readRecord(t *testing.T, path string) []rewrittenCell {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("기록 파일: %v", err)
	}
	var out []rewrittenCell
	for _, line := range bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n")) {
		var c rewrittenCell
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			t.Fatalf("기록 줄 %q: %v", line, err)
		}
		out = append(out, c)
	}
	return out
}

// 바꾼 칸의 기록에는 상한이 없다: 화면 보고(TransferReport.Rewrites)는 갈래마다 행 좌표를 pg.MaxListed 개만, 그것도 줄여서 싣지만
// 기록 파일은 바꾼 칸 전부를 행의 전체 키(자르지 않은 긴 id)와 원래 바이트로 남긴다.
func TestImportSQLiteRecordsEveryRewrittenCell(t *testing.T) {
	ctx := context.Background()
	n := pg.MaxListed + 5
	long := strings.Repeat("긴", 200)
	src := importSource(t, importSeed)
	liteExec(t, src, fmt.Sprintf(`INSERT INTO judgment(id, project, session_id, at, kind, title, body)
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
		SELECT '%s-' || i, 'p', 's1', %s, 'decision', 't', CAST(X'6E00' AS TEXT) || i FROM n`, n, long, importAt))
	dsn, _ := importTarget(t)
	rep, err := importLedger(t, ctx, src, dsn, TransferOptions{})
	if err != nil || len(rep.Rewrites) != 1 {
		t.Fatalf("이관: %v, 바꾼 칸 %+v", err, rep.Rewrites)
	}
	if g := rep.Rewrites[0]; g.Count != n || len(g.Keys) != pg.MaxListed || strings.Contains(g.Keys[0], long) {
		t.Fatalf("화면 보고 %s.%s: 수 %d · 좌표 %d 개(기대 %d · %d 개, 줄인 좌표)", g.Table, g.Column, g.Count, len(g.Keys), n, pg.MaxListed)
	}
	want := map[string]string{}
	for i := 1; i <= n; i++ {
		want[fmt.Sprintf("%s-%d", long, i)] = fmt.Sprintf("n\x00%d", i)
	}
	record := readRecord(t, rep.RewriteRecord)
	if len(record) != n {
		t.Fatalf("기록 %d 줄, 기대 %d 줄", len(record), n)
	}
	for _, r := range record {
		id, _ := r.Key["id"].(string)
		if orig, ok := want[id]; !ok || r.Table != "judgment" || r.Column != "body" || string(r.Original) != orig || len(r.Key) != 1 {
			t.Fatalf("기록 줄 %s.%s 키 %d 바이트 · 원래 %q", r.Table, r.Column, len(id), r.Original)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("기록에 없는 행 %d 개", len(want))
	}
}

// 바꾼 칸의 기록을 못 남기면 커밋하지 않는다: 대상은 빈 채이고 이관 표지도, 기록 파일(그 임시 파일까지)도 남지 않는다. 기록을
// 남길 수 있게 되면 같은 원본을 다시 옮긴다.
func TestImportSQLiteDoesNotCommitWithoutTheRecord(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	liteExec(t, src, `UPDATE item SET body = CAST(X'610062' AS TEXT) WHERE id = 'i1'`)
	dsn, dst := importTarget(t)
	linkRecord = func(string, string) error { return errors.New("넣은 링크 실패") }
	t.Cleanup(func() { linkRecord = os.Link })
	rep, err := importLedger(t, ctx, src, dsn, TransferOptions{})
	if err == nil || !strings.Contains(err.Error(), "바꾼 칸의 기록을 남기지 못했다 - 커밋하지 않는다") ||
		!strings.Contains(err.Error(), "넣은 링크 실패") || rep.Copied != 0 || rep.RewriteRecord != "" {
		t.Fatalf("기록 없이 커밋했거나 다른 오류다: %v (옮긴 행 %d · 기록 %q)", err, rep.Copied, rep.RewriteRecord)
	}
	if n := targetRows(t, dst); n != 0 {
		t.Fatalf("기록을 못 남겼는데 대상에 %d 행이 있다", n)
	}
	for _, name := range dirNames(t, filepath.Dir(src)) {
		if strings.Contains(name, rewrittenSuffix) || name == filepath.Base(MovedMarker(src)) {
			t.Fatalf("거절 뒤 %s 가 남았다", name)
		}
	}
	linkRecord = os.Link
	if rep, err := importLedger(t, ctx, src, dsn, TransferOptions{}); err != nil || rep.RewriteRecord == "" || rep.Copied == 0 {
		t.Fatalf("다시 옮기기: %v, 기록 %q", err, rep.RewriteRecord)
	}
}

// 바꾸기는 JSON 글 하나 단위다: 항목 라벨을 바꾼 이벤트(item.label)의 after 와 그 항목의 labels 는 원장에서 같은 글이고, 이관
// 뒤에도 같다 - 같은 이벤트의 before 에만 U+0000 이 있어도 after 의 `\x00` 네 글자는 그대로 옮긴다. 원장은 앱이 쓴 것이다.
func TestImportSQLiteKeepsTheLabelEventInStepWithTheItem(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seed(t, s, "p")
	sess := mustSession(t, s, "p", "cc")
	if err := s.AddItem(ctx, model.Item{Project: "p", ID: "it", Title: "제목", Labels: []string{"a\x00"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLabels(ctx, "p", "it", []string{`\x00z`}, sess.ID); err != nil {
		t.Fatal(err)
	}
	src := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dsn, dst := importTarget(t)
	rep, err := importLedger(t, ctx, src, dsn, TransferOptions{})
	if err != nil {
		t.Fatalf("이관: %v", err)
	}
	after := pgString(t, dst, `SELECT ((payload::jsonb)->'after')::text FROM event WHERE kind = 'item.label'`)
	labels := pgString(t, dst, `SELECT (labels::jsonb)::text FROM item WHERE id = 'it'`)
	if after != labels || labels != `["\\x00z"]` {
		t.Fatalf("옮긴 이벤트 after %s · 항목 labels %s, 기대 둘 다 [\"\\\\x00z\"]", after, labels)
	}
	if before := pgString(t, dst, `SELECT (payload::jsonb)->'before'->>0 FROM event WHERE kind = 'item.label'`); before != `a\x00` {
		t.Fatalf("옮긴 이벤트 before %q", before)
	}
	// 바꾼 칸은 그 이벤트 하나, 그 안의 글 하나다(항목의 labels 에는 이제 U+0000 이 없다).
	paths := map[string][]nulesc.Change{}
	for _, r := range readRecord(t, rep.RewriteRecord) {
		paths[r.Table+"."+r.Column] = append(paths[r.Table+"."+r.Column], r.Paths...)
	}
	if !reflect.DeepEqual(paths["event.payload"], []nulesc.Change{{Pointer: "/before/0"}}) || len(paths) != 1 {
		t.Fatalf("바꾼 칸의 기록 %+v", paths)
	}
}

// 예행은 걸린 것이 없으면 성공하고(대조까지 돈다) 아무것도 남기지 않는다 - 행도 순번도. 그래서 예행 뒤의 본 실행이 같은
// 대상을 빈 원장으로 받는다. 대상이 Postgres 가 아니면 원본만 잰다.
func TestImportSQLiteDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	dsn, dst := importTarget(t)
	fresh := identityStates(t, dst)
	rep, err := importLedger(t, ctx, src, dsn, TransferOptions{DryRun: true})
	if err != nil || !rep.TargetChecked || rep.Copied != 0 || len(rep.Rows) != len(pg.Tables()) || !rep.Matched() {
		t.Fatalf("예행: %v %+v", err, rep)
	}
	if n := targetRows(t, dst); n != 0 {
		t.Fatalf("예행이 대상에 %d 행을 썼다", n)
	}
	if got := identityStates(t, dst); !maps.Equal(got, fresh) {
		t.Fatalf("예행이 순번을 바꿔 놓았다: %v → %v", fresh, got)
	}
	if rep, err := importLedger(t, ctx, src, dsn, TransferOptions{}); err != nil || rep.Copied == 0 {
		t.Fatalf("예행 뒤 본 실행: %v (옮긴 행 %d)", err, rep.Copied)
	}
	// 옮긴 원본은 표지가 있어 다시 옮기지 않는다(TestImportSQLiteLeavesTheMovedMarker) - 옮기지 않은 원본으로 잰다.
	src = importSource(t, importSeed)
	other := filepath.Join(t.TempDir(), "not-there.db")
	rep, err = importLedger(t, ctx, src, other, TransferOptions{DryRun: true})
	if err != nil || rep.TargetChecked {
		t.Fatalf("Postgres 아닌 대상의 예행: %v, 대상을 쟀다 %v", err, rep.TargetChecked)
	}
	if _, err := importLedger(t, ctx, src, other, TransferOptions{}); err == nil || !strings.Contains(err.Error(), "Postgres 접속 URL") {
		t.Fatalf("Postgres 아닌 대상에 옮겼다: %v", err)
	}
}

// 대상이 빈 원장이 아니면(한 번 옮긴 뒤 다시) 거절하고, 판 행이 없는 빈 DB 면 기준선부터 깔라고 한다.
func TestImportSQLiteRefusesATargetThatIsNotEmpty(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	dsn, dst := importTarget(t)
	if _, err := importLedger(t, ctx, src, dsn, TransferOptions{}); err != nil {
		t.Fatalf("첫 이관: %v", err)
	}
	before := targetRows(t, dst)
	// 옮긴 원본은 표지가 있어 다시 옮기지 않는다 - 대상 쪽 거절을 재려고 옮기지 않은 같은 내용의 원본을 쓴다.
	src = importSource(t, importSeed)
	for _, dry := range []bool{true, false} {
		_, err := importLedger(t, ctx, src, dsn, TransferOptions{DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), "비어 있지 않다") {
			t.Fatalf("예행 %v: 찬 대상에 다시 옮겼다: %v", dry, err)
		}
	}
	if after := targetRows(t, dst); after != before {
		t.Fatalf("거절했는데 대상 행 수가 %d → %d", before, after)
	}

	// 행을 다 지웠어도 identity 시퀀스가 번호를 냈으면 빈 원장이 아니다. 순번을 원본의 번호로 되감으면 이미 낸 번호를 다시 낸다.
	dsn2, dst2 := importTarget(t)
	pgExec(t, dst2, `SELECT nextval(pg_get_serial_sequence('event', 'id'))`)
	for _, dry := range []bool{true, false} {
		_, err := importLedger(t, ctx, src, dsn2, TransferOptions{DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), "비어 있지 않다") || !strings.Contains(err.Error(), "event.id 순번") {
			t.Fatalf("예행 %v: 번호를 낸 순번이 있는 대상에 옮겼다: %v", dry, err)
		}
	}
	if n := targetRows(t, dst2); n != 0 {
		t.Fatalf("거절했는데 대상에 %d 행", n)
	}

	// 번호를 내지 않았어도(is_called 거짓) 순번이 시작값에서 옮겨져 있으면 빈 원장이 아니다. 앞선 이관이 지워진 최고 번호를
	// 지키려고 RESTART 로 옮겨 두고 행만 사라진 꼴이다 - 다시 옮기면 순번을 1 로 되감는다.
	dsn3, dst3 := importTarget(t)
	pgExec(t, dst3, `SELECT setval(pg_get_serial_sequence('event', 'id'), 101, false)`)
	for _, dry := range []bool{true, false} {
		_, err := importLedger(t, ctx, src, dsn3, TransferOptions{DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), "비어 있지 않다") || !strings.Contains(err.Error(), "event.id 순번(다음 번호 101)") {
			t.Fatalf("예행 %v: 시작값에서 옮겨진 순번이 있는 대상에 옮겼다: %v", dry, err)
		}
	}
	if n := targetRows(t, dst3); n != 0 {
		t.Fatalf("거절했는데 대상에 %d 행", n)
	}

	bare := storetest.NewBare(t, "")
	if _, err := importLedger(t, ctx, src, bare, TransferOptions{}); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("--to %d", pg.ImportVersion)) {
		t.Fatalf("빈 DB 에 옮겼거나 처방이 없다: %v", err)
	}
}

// 이관 판보다 낮은 원본은 먼저 올리라는 안내와 함께 거절한다(예행도). 이 명령은 옛 증분을 갖지 않는다.
func TestImportSQLiteRefusesAnOlderSource(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	below := pg.ImportFloor() - 1
	mustMigrateTo(t, path, below)
	dsn, dst := importTarget(t)
	older := fmt.Sprintf("판 %d", below)
	for _, dry := range []bool{true, false} {
		_, err := importLedger(t, ctx, path, dsn, TransferOptions{DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), older) || !strings.Contains(err.Error(), "fd migrate --db") {
			t.Fatalf("예행 %v: %s 원본을 받았거나 안내가 없다: %v", dry, older, err)
		}
	}
	if _, err := VerifySQLite(ctx, path, dsn, VerifyOptions{}, quietLog()); err == nil || !strings.Contains(err.Error(), older) {
		t.Fatalf("대조가 %s 원본을 받았다: %v", older, err)
	}
	if n := targetRows(t, dst); n != 0 {
		t.Fatalf("거절했는데 대상에 %d 행", n)
	}
	// 원본을 바꾸지 않았다 - 여전히 이관 판 아래다.
	var v int
	liteQuery(t, path, `SELECT max(version) FROM schema_version`, &v)
	if v != below {
		t.Fatalf("거절한 원본의 판이 %d 로 바뀌었다", v)
	}
}

// 이관 판 범위(pg.ImportFloor-ImportVersion) 안이면 원본과 대상의 판이 서로 달라도 옮기고 맞댄다. 「켜기 전에 섰을 때」에서 이어 가는
// 갈래가 이것이다: 판 18 에서 옮긴 SQLite 원장(표지가 있어 판을 못 올린다)을 이 판의 실행기가 19 로 올린 Postgres 원장과 맞댄다.
// 반대(판 19 원본 · 판 18 대상)와 역이관(판 18 원본)도 받는다.
func TestTransferAcceptsTheVersionRange(t *testing.T) {
	ctx := context.Background()
	floor := pg.ImportFloor()
	if floor >= pg.ImportVersion {
		t.Skip("이관 판 범위가 한 판이다 - 서로 다른 판이 없다")
	}
	at := func(v int) string {
		path := filepath.Join(t.TempDir(), "fd.db")
		mustMigrateTo(t, path, v)
		liteExec(t, path, importSeed)
		return path
	}

	t.Run("판 18 원본 → 판 19 대상", func(t *testing.T) {
		src := at(floor)
		dsn, dst := importTarget(t)
		for _, dry := range []bool{true, false} {
			if _, err := importLedger(t, ctx, src, dsn, TransferOptions{DryRun: dry}); err != nil {
				t.Fatalf("예행 %v: 범위 안의 원본을 거절했다: %v", dry, err)
			}
		}
		settleLedger(t, dsn)
		if _, err := VerifySQLite(ctx, src, dsn, VerifyOptions{}, quietLog()); err != nil {
			t.Fatalf("범위 안의 원본을 대조가 거절했다: %v", err)
		}
		if n := targetRows(t, dst); n == 0 {
			t.Fatal("옮긴 행이 없다")
		}
	})

	t.Run("판 19 원본 → 판 18 대상 → 역이관", func(t *testing.T) {
		src := at(pg.ImportVersion)
		dsn, _ := pgAtBaseline(t)
		if _, err := importLedger(t, ctx, src, dsn, TransferOptions{}); err != nil {
			t.Fatalf("범위 안의 대상을 거절했다: %v", err)
		}
		back := exportBack(t)
		if _, err := exportLedger(t, ctx, dsn, back, TransferOptions{}); err != nil {
			t.Fatalf("판 18 원장의 역이관을 거절했다: %v", err)
		}
		var v int
		liteQuery(t, back, `SELECT max(version) FROM schema_version`, &v)
		if v != pg.ImportVersion {
			t.Fatalf("역이관이 낸 원장의 판이 %d 다(이관 판 %d 이어야 한다)", v, pg.ImportVersion)
		}
	})
}

// transferPlan 은 판 범위 규칙을 세 문에 똑같이 건다: 범위 안이면 받고(MigrateNone), 아래면 올리라 하고, 위면 거절한다.
func TestTransferPlanFollowsTheRange(t *testing.T) {
	for v, want := range map[int]MigrationAction{
		pg.ImportFloor() - 1: MigrateUpgrade,
		pg.ImportFloor():     MigrateNone,
		pg.ImportVersion:     MigrateNone,
		pg.ImportVersion + 1: MigrateReject,
	} {
		if got := transferPlan(true, v, 30).Action; got != want {
			t.Errorf("판 %d: %s(기대 %s)", v, got, want)
		}
	}
	if got := transferRange(); got != fmt.Sprintf("%d-%d", pg.ImportFloor(), pg.ImportVersion) {
		t.Errorf("범위 표기가 %q 다", got)
	}
}

func liteQuery(t *testing.T, path, q string, dest ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteURI(path, nil))
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
	// 갈래마다 옮기지 않은 원본을 새로 만든다 - 옮긴 원본은 표지가 있어 다시 옮기지 않는다.
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
			src := importSource(t, importSeed)
			dsn, dst := importTarget(t)
			if _, err := importLedger(t, ctx, src, dsn, TransferOptions{}); err != nil {
				t.Fatalf("이관: %v", err)
			}
			for _, q := range strings.Split(c.change, ";") {
				pgExec(t, dst, q)
			}
			rep, err := VerifySQLite(ctx, src, dsn, VerifyOptions{}, quietLog())
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
	// 대상 값이 값 검사에 걸리면 어긋남과 따로 그 칸 · 사유 · 행을 적는다(대상에서 읽은 값을 버리지 않는다). 짝 없는 서러게이트는
	// jsonb 가 거절하는 JSON 이다.
	t.Run("대상 값 검사", func(t *testing.T) {
		src := importSource(t, importSeed)
		dsn, dst := importTarget(t)
		if _, err := importLedger(t, ctx, src, dsn, TransferOptions{}); err != nil {
			t.Fatalf("이관: %v", err)
		}
		pgExec(t, dst, `UPDATE item SET labels = '["\ud800"]' WHERE id = 'i1'`)
		rep, err := VerifySQLite(ctx, src, dsn, VerifyOptions{}, quietLog())
		if err == nil || !strings.Contains(err.Error(), "대상 값 검사") {
			t.Fatalf("대상 값 검사에 걸린 칸을 판정에 넣지 않았다: %v", err)
		}
		if len(rep.TargetFaults) != 1 {
			t.Fatalf("대상 값 검사 갈래: %+v", rep.TargetFaults)
		}
		g := rep.TargetFaults[0]
		if g.Table != "item" || g.Column != "labels" || g.Reason != pg.FaultJSONSurrogate || g.Count != 1 ||
			len(g.Keys) != 1 || !strings.Contains(g.Keys[0], `id="i1"`) {
			t.Fatalf("대상 값 검사 갈래: %+v", g)
		}
	})
	t.Run("순번", func(t *testing.T) {
		src := importSource(t, importSeed)
		dsn, dst := importTarget(t)
		if _, err := importLedger(t, ctx, src, dsn, TransferOptions{}); err != nil {
			t.Fatalf("이관: %v", err)
		}
		pgExec(t, dst, `SELECT setval(pg_get_serial_sequence('pick_eval', 'id'), 2)`)
		rep, err := VerifySQLite(ctx, src, dsn, VerifyOptions{}, quietLog())
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

// 예행은 본 실행과 같은 트랜잭션을 돈다: 사전 검사가 못 보는 거절(역할 권한 · 대상의 CHECK · 커밋 때 잴 미룬 FK)을 예행도
// 본 실행과 똑같이 받고, 어느 쪽도 행 · 순번을 남기지 않는다.
func TestImportSQLiteDryRunMatchesTheRealRun(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, seed, errHas string
		// login 이 있으면 대상에 그 역할의 구성원으로 붙는다. target 은 이관 전에 대상에 거는 문장이다.
		login, target string
	}{
		// 앱 역할은 소유 역할로 바꿔 타지 못한다(시퀀스를 다시 시작하는 것은 주인의 일이다).
		{"앱 역할", importSeed, "소유 역할", pg.AppRole, ""},
		// 길이 0 BLOB 은 SQLite 의 CHECK (body <> '') 를 지난다(BLOB 은 어느 글보다 크다). Cell 은 그것을 빈 글로 옮기고
		// 사전 검사도 받지만, 기준선의 같은 CHECK 는 빈 글을 거절한다.
		{"대상의 CHECK", importSeed + `INSERT INTO judgment(id, project, session_id, at, kind, title, body) VALUES
			('j3', NULL, NULL, ` + importAt + `, 'now', NULL, X'');`, "judgment_body_check", "", ""},
		// 카탈로그가 모르는 미룰 수 있는 FK 는 사전 검사가 못 보고 커밋 때 걸린다. 예행은 커밋하지 않으므로 그 검사를 앞당겨야
		// 본 실행과 같이 거절한다(session.label '' 은 machine 에 없다).
		{"커밋 때 잴 FK", importSeed, "t_label_fk", "", `ALTER TABLE session ADD CONSTRAINT t_label_fk
			FOREIGN KEY (label) REFERENCES machine(id) DEFERRABLE`},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := importSource(t, c.seed)
			dsn, dst := importTarget(t)
			if c.target != "" {
				pgExec(t, dst, c.target)
			}
			to := dsn
			if c.login != "" {
				to, _ = loginAs(t, dst, dsn, c.login)
			}
			fresh := identityStates(t, dst)
			for _, dry := range []bool{true, false} {
				rep, err := importLedger(t, ctx, src, to, TransferOptions{DryRun: dry})
				if err == nil || !strings.Contains(err.Error(), c.errHas) || rep.Copied != 0 {
					t.Fatalf("예행 %v: %q 로 거절하지 않았다: %v (옮긴 행 %d)", dry, c.errHas, err, rep.Copied)
				}
			}
			if n := targetRows(t, dst); n != 0 {
				t.Fatalf("거절했는데 대상에 %d 행", n)
			}
			if got := identityStates(t, dst); !maps.Equal(got, fresh) {
				t.Fatalf("거절했는데 순번이 바뀌었다: %v → %v", fresh, got)
			}
		})
	}
}

// 옮긴 값이 대상에서 달라지면(값을 바꾸는 BEFORE INSERT 트리거) 커밋 전 대조가 그 표를 짚고, 이관은 커밋하지 않는다 - 행도
// 순번도 남지 않는다. 예행도 같은 자리에서 멈춘다.
func TestImportSQLiteDoesNotCommitAMismatch(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	dsn, dst := importTarget(t)
	pgExec(t, dst, `CREATE FUNCTION t_bend_title() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN NEW.title := NEW.title || '!'; RETURN NEW; END $$`)
	pgExec(t, dst, `CREATE TRIGGER t_bend_title BEFORE INSERT ON item FOR EACH ROW EXECUTE FUNCTION t_bend_title()`)
	fresh := identityStates(t, dst)
	for _, dry := range []bool{true, false} {
		rep, err := importLedger(t, ctx, src, dsn, TransferOptions{DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), "커밋하지 않는다") || rep.Copied != 0 {
			t.Fatalf("예행 %v: 어긋난 이관이 끝났다: %v (옮긴 행 %d)", dry, err, rep.Copied)
		}
		for _, tc := range rep.Tables {
			if (tc.Table == "item") == tc.OK() {
				t.Errorf("예행 %v: %s 의 대조가 %v 다", dry, tc.Table, tc.OK())
			}
		}
	}
	if n := targetRows(t, dst); n != 0 {
		t.Fatalf("커밋하지 않았는데 대상에 %d 행", n)
	}
	if got := identityStates(t, dst); !maps.Equal(got, fresh) {
		t.Fatalf("커밋하지 않았는데 순번이 바뀌었다: %v → %v", fresh, got)
	}
}

// 대조는 이관이 받지 않을 원본(사전 검사에 걸린 원본)을 맞다고 하지 않는다. 정수 칸의 BLOB X'37' 은 대상의 정수 7 과 바이트가
// 같다 - 그래도 사전 검사에 걸렸다고 거절하고, 행 지문의 저장 타입 표지가 달라 그 표가 어긋난다.
func TestVerifySQLiteRefusesASourceTheImportWouldRefuse(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	dsn, _ := importTarget(t)
	if _, err := importLedger(t, ctx, src, dsn, TransferOptions{}); err != nil {
		t.Fatalf("이관: %v", err)
	}
	liteExec(t, src, `UPDATE counter SET value = X'37'`)
	var typ string
	liteQuery(t, src, `SELECT typeof(value) FROM counter`, &typ)
	if typ != "blob" {
		t.Fatalf("시험 전제: counter.value 가 %s 다(blob 이어야 한다)", typ)
	}
	rep, err := VerifySQLite(ctx, src, dsn, VerifyOptions{}, quietLog())
	if err == nil || !strings.Contains(err.Error(), "사전 검사") {
		t.Fatalf("사전 검사에 걸린 원본을 맞다고 했다: %v", err)
	}
	if len(rep.Faults) != 1 || rep.Faults[0].Table != "counter" || rep.Faults[0].Reason != pg.FaultStorage {
		t.Fatalf("걸린 갈래: %+v", rep.Faults)
	}
	for _, tc := range rep.Tables {
		if (tc.Table == "counter") == tc.OK() {
			t.Errorf("%s 의 대조가 %v 다 - 바이트가 같은 다른 저장 타입을 갈라야 한다", tc.Table, tc.OK())
		}
	}
}

// 원본 옆 -wal 에 커밋이 남아 있으면(원본에 쓰는 연결이 아직 있다) 예행 · 이관 · 대조가 다 거절하고 처방(VACUUM INTO)을 낸다.
// 그 처방으로 뜬 사본은 -wal 에만 있던 커밋까지 옮긴다 - `cp` 로 뜬 사본이 말없이 잃는 그것이다.
func TestImportSQLiteRefusesAPendingWAL(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	w, err := sql.Open("sqlite", sqliteURI(src, url.Values{"_pragma": {"journal_mode(WAL)"}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if _, err := w.Exec(`INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m-wal', 'h', 'x', 'x')`); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(src + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("시험 전제: -wal 이 비었다(%v)", err)
	}
	dsn, dst := importTarget(t)
	for name, run := range map[string]func() error{
		"예행": func() error {
			_, err := importLedger(t, ctx, src, dsn, TransferOptions{DryRun: true})
			return err
		},
		"이관": func() error { _, err := importLedger(t, ctx, src, dsn, TransferOptions{}); return err },
		"대조": func() error { _, err := VerifySQLite(ctx, src, dsn, VerifyOptions{}, quietLog()); return err },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "-wal") || !strings.Contains(err.Error(), "VACUUM INTO") {
			t.Fatalf("%s: 비지 않은 -wal 옆의 원본을 받았거나 처방이 없다: %v", name, err)
		}
	}
	if n := targetRows(t, dst); n != 0 {
		t.Fatalf("거절했는데 대상에 %d 행", n)
	}
	copied := filepath.Join(t.TempDir(), "copy.db")
	if _, err := w.Exec(`VACUUM INTO '` + copied + `'`); err != nil {
		t.Fatal(err)
	}
	if _, err := importLedger(t, ctx, copied, dsn, TransferOptions{}); err != nil {
		t.Fatalf("VACUUM INTO 사본의 이관: %v", err)
	}
	if n := pgCount(t, dst, `SELECT count(*) FROM machine WHERE id = 'm-wal'`); n != 1 {
		t.Fatalf("-wal 에만 있던 행이 %d 개 옮겨졌다", n)
	}
}

// 원본 경로는 글자 그대로다: `fd%20copy#1.db` 는 `fd copy#1.db` 도 `fd copy` 도 아니다(SQLite URI 는 경로의 %XX 를 풀고 #
// 에서 끊는다). 없는 원본은 거절하고 만들지 않는다. 원본은 읽기 전용으로 연다.
func TestImportSQLiteOpensTheNamedFileOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	name := "fd%20copy#1.db"
	src := filepath.Join(dir, name)
	mustMigrate(t, src)
	liteExec(t, src, importSeed)
	dsn, dst := importTarget(t)
	if _, err := importLedger(t, ctx, src, dsn, TransferOptions{}); err != nil {
		t.Fatalf("이관: %v", err)
	}
	if n := targetRows(t, dst); n == 0 {
		t.Fatal("원본 행이 하나도 안 옮겨졌다 - 다른 파일을 읽었다")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), name) {
			t.Errorf("원본 디렉토리에 다른 이름의 파일 %q 가 생겼다", e.Name())
		}
	}
	missing := filepath.Join(dir, "none.db")
	if _, err := VerifySQLite(ctx, missing, dsn, VerifyOptions{}, quietLog()); err == nil || !strings.Contains(err.Error(), "없다") {
		t.Fatalf("없는 원본을 받았다: %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("없는 원본을 만들었다: %v", err)
	}
	q, err := url.ParseQuery(importSourceDSN(src)[strings.IndexByte(importSourceDSN(src), '?')+1:])
	if err != nil || q.Get("mode") != "ro" {
		t.Fatalf("이관 원본이 읽기 전용이 아니다: %q", importSourceDSN(src))
	}
	if got, want := sqliteURI("/a/b%c?d#e.db", nil), "file:/a/b%25c%3fd%23e.db?"; got != want {
		t.Fatalf("sqliteURI = %q, 기대 %q", got, want)
	}
}

// 이관은 표지를 남긴 뒤 원본 옆의 판올림 전 사본(과 그 옆 파일)을 새 디렉토리로 치운다 - 그 원장은 이제 원장이 아니고, 뒤에 그
// 자리에 놓일 원장의 사본이 아니다. 예행은 옮기지 않는다.
func TestImportSQLiteQuarantinesTheSourceBackups(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	bak := src + ".bak-20260101T000000Z"
	mustWrite(t, bak, []byte("옛 판올림 전 사본"))
	mustWrite(t, bak+"-journal", []byte("옆"))
	dsn, _ := importTarget(t)

	rep, err := importLedger(t, ctx, src, dsn, TransferOptions{DryRun: true})
	if left := globIn(t, filepath.Dir(src), "fd.db.bak-*"); err != nil || rep.Quarantined != "" || len(left) != 2 {
		t.Fatalf("예행이 사본을 옮겼다: %v %q %v", err, rep.Quarantined, left)
	}
	rep, err = importLedger(t, ctx, src, dsn, TransferOptions{})
	if err != nil {
		t.Fatalf("이관: %v", err)
	}
	if _, err := os.Lstat(MovedMarker(src)); err != nil {
		t.Fatalf("이관 표지가 없다: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(rep.Quarantined), "fd.db.pre-import-") {
		t.Fatalf("사본을 옮긴 디렉토리를 보고하지 않았다: %q", rep.Quarantined)
	}
	if left := globIn(t, filepath.Dir(src), "fd.db.bak-*"); len(left) != 0 {
		t.Fatalf("원본 옆에 사본이 남았다: %v", left)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(rep.Quarantined, filepath.Base(bak))), []byte("옛 판올림 전 사본")) ||
		!bytes.Equal(mustRead(t, filepath.Join(rep.Quarantined, filepath.Base(bak)+"-journal")), []byte("옆")) {
		t.Fatal("옮긴 사본 · 옆 파일이 그대로가 아니다")
	}
}
