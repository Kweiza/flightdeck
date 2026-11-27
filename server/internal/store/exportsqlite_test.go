package store

// Postgres → SQLite 역이관(ExportSQLite)을 실물 두 원장으로 잰다: 시험 SQLite → 이관 → Postgres 에서 쓰기 → 역이관의 왕복
// (되돌리기 창에 Postgres 에 쌓인 항목 · 선점 · 이벤트 · 판단이 살아남고, 원본의 값 · rowid 순서 · 지워진 최고 번호가 그대로인지),
// 순번 되살리기, 검색 색인, 대상 자리의 거절(있는 파일 · 옆 파일), 예행이 파일을 안 남기는지, 대조가 어긋나면 커밋하지 않는지,
// 옮길 수 없는 원본의 거절(파일을 안 남긴다).

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// exportBack 은 빈 디렉토리 안의 아직 없는 SQLite 대상 경로다.
func exportBack(t *testing.T) string { return filepath.Join(t.TempDir(), "back.db") }

// importedPG 는 importSeed 원본을 이관한 Postgres 원장이다. 원본 경로, 접속 URL, 그 원장을 연 Store(시험의 재기 · 손질)를 낸다.
func importedPG(t *testing.T) (string, string, *Store) {
	t.Helper()
	src := importSource(t, importSeed)
	dsn, dst := importTarget(t)
	if _, err := ImportSQLite(context.Background(), src, dsn, TransferOptions{}, quietLog()); err != nil {
		t.Fatalf("이관: %v", err)
	}
	return src, dsn, dst
}

// liteScan 은 SQLite 원장 path 를 이관 원본처럼 훑는다(읽기 전용).
func liteScan(t *testing.T, path string) *ledgerScan {
	t.Helper()
	ctx := context.Background()
	s, err := openImportSource(ctx, path, quietLog())
	if err != nil {
		t.Fatalf("%s 열기: %v", filepath.Base(path), err)
	}
	defer s.Close()
	var sc *ledgerScan
	if err := s.ReadTx(ctx, func(q Reader) error {
		sc, err = scanLedger(ctx, q, sqliteSide{})
		return err
	}); err != nil {
		t.Fatalf("%s 훑기: %v", filepath.Base(path), err)
	}
	return sc
}

func pgString(t *testing.T, s *Store, q string) string {
	t.Helper()
	var v string
	if err := s.db.QueryRowContext(context.Background(), q).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// 되돌리기 예행: 시험 SQLite → 이관 → Postgres 에서 업무 쓰기 → 역이관. 낸 파일은 Postgres 원장과 맞고(커밋 전 대조와 따로 돈
// 대조), 이 판의 fd 로 열려 Postgres 에서 쓴 항목 · 선점 · 개정 · 판단(검색 포함) · 이벤트가 업무 API 로 보인다. 원본의 행은
// Postgres 에서 바꾼 것 말고 전부 그대로다 - NULL 과 빈 글, BLOB, item_after 의 rowid 순서, 지워진 최고 번호까지.
func TestExportSQLiteRoundTripKeepsThePostgresWrites(t *testing.T) {
	ctx := context.Background()
	src, dsn, dst := importedPG(t)
	app, err := OpenWithLogger(dsn, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	at := time.Date(2026, 1, 3, 4, 5, 6, 0, time.UTC)
	if err := app.AddItem(ctx, model.Item{Project: "p", ID: "pg-item", Title: "Postgres 에서 연 항목", Body: "처음 본문",
		Paths: []string{"b.go"}, State: model.ItemOpen,
		After: []model.After{{Item: "i1"}, {SHA: "sha7"}, {Job: "j1"}}}); err != nil {
		t.Fatalf("항목: %v", err)
	}
	if _, err := app.ClaimItem(ctx, "p", "pg-item", "s2", at); err != nil {
		t.Fatalf("선점: %v", err)
	}
	body := "고친 본문"
	if err := app.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AmendItem("p", "pg-item", AmendPatch{Body: &body, Reason: "정정"}, "s2")
		return err
	}); err != nil {
		t.Fatalf("개정: %v", err)
	}
	j, err := app.AddJudgment(ctx, model.Judgment{Project: "p", SessionID: "s2", At: at, Kind: model.JudgmentDecision,
		Title: "되돌리기", Body: "되돌림검색어 가 살아남는다", Links: []model.JudgmentLink{{TargetKind: "item", TargetID: "pg-item"}}})
	if err != nil {
		t.Fatalf("판단: %v", err)
	}
	if err := app.FinishItem(ctx, "p", "i3", "s1", model.ItemDropped, "되돌린다"); err != nil {
		t.Fatalf("마감: %v", err)
	}
	if n, err := app.NextCounter(ctx, "p", "item"); err != nil || n != 8 {
		t.Fatalf("번호: %d %v", n, err)
	}
	app.LogEvent(ctx, "export.test", "p", "s2", map[string]string{"k": "v"})
	if err := app.RecordPickEval(ctx, model.PickEval{Project: "p", SessionID: "s2", At: at, Picked: "pg-item"}); err != nil {
		t.Fatalf("고르기 기록: %v", err)
	}
	if err := app.RecordPickEval(ctx, model.PickEval{Project: "p", SessionID: "s2", At: at}); err != nil {
		t.Fatalf("고르기 기록: %v", err)
	}
	if _, err := app.AcquireResource(ctx, "p", "env:pg", Holder{SessionID: "s2"}, at); err != nil {
		t.Fatalf("자원: %v", err)
	}
	if err := app.PutIdemRecord(ctx, IdemRecord{Key: "pg-key", Fingerprint: "fp", Status: 200, ContentType: "text/plain",
		Body: []byte{0, 1}, At: at}, 0, 0); err != nil {
		t.Fatalf("멱등 기록: %v", err)
	}
	// Postgres 에서 낸 pick_eval 의 맨 끝 번호를 지운다 - 시퀀스는 그 번호를 기억하고 행에는 없다. 낸 SQLite 도 그 번호를 다시
	// 내지 않아야 한다(sqlite_sequence).
	pickTop := pgCount(t, dst, `SELECT max(id) FROM pick_eval`)
	pgExec(t, dst, `DELETE FROM pick_eval WHERE id = ?`, pickTop)
	wantAfter := pgString(t, dst, `SELECT string_agg(seq || ':' || item_id || ':' || coalesce(dep_item, dep_job, dep_sha), ',' ORDER BY seq)
		FROM item_after`)
	pgItem, err := app.GetItem(ctx, "p", "pg-item")
	if err != nil {
		t.Fatal(err)
	}

	back := exportBack(t)
	rep, err := ExportSQLite(ctx, dsn, back, TransferOptions{}, quietLog())
	if err != nil {
		t.Fatalf("역이관 실패: %v\n%+v", err, rep)
	}
	total := 0
	for _, r := range rep.Rows {
		total += r.Rows
	}
	if !rep.Matched() || rep.Copied != total || int64(total) != targetRows(t, dst) || rep.From != "postgres" {
		t.Fatalf("옮긴 행 %d · 원본 %d · Postgres %d · 대조 %v · 원본 표시 %q", rep.Copied, total, targetRows(t, dst),
			rep.Matched(), rep.From)
	}
	if got := dirNames(t, filepath.Dir(back)); !slices.Equal(got, []string{"back.db"}) {
		t.Fatalf("역이관 뒤 디렉토리에 %v 가 있다 - 새 원장 하나만 있어야 한다", got)
	}
	if again, err := VerifySQLite(ctx, back, dsn, quietLog()); err != nil || !again.Matched() {
		t.Fatalf("낸 파일과 Postgres 원장의 대조: %v\n%+v", err, again.Tables)
	}

	// 원본의 행은 Postgres 에서 바꾼 것(i3 마감 · 그 선점 반납 · 번호) 말고 그대로다 - 전 칸 지문으로 잰다.
	orig, out := liteScan(t, src), liteScan(t, back)
	changed := map[string][]string{"item": {`project="p" id="i3"`}, "claim": {`project="p" item_id="i3"`},
		"counter": {`project="p" name="item"`}}
	for _, tb := range pg.Tables() {
		c := pg.Compare(tb.Name, orig.marks[tb.Name], out.marks[tb.Name])
		if !slices.Equal(c.OnlySource, changed[tb.Name]) {
			t.Errorf("%s: 원본에만 있는 행 %v, 기대 %v", tb.Name, c.OnlySource, changed[tb.Name])
		}
	}
	for q, want := range map[string]int64{
		`SELECT count(*) FROM project WHERE id = 'p2' AND archived_at = ''`:                                       1,
		`SELECT count(*) FROM project WHERE id = 'p' AND archived_at IS NULL`:                                     1,
		`SELECT count(*) FROM session WHERE id = 's1' AND label IS NULL`:                                          1,
		`SELECT count(*) FROM idempotency WHERE key = 'key1' AND body = X'7B0022'`:                                1,
		`SELECT count(*) FROM idempotency WHERE key = 'key2' AND typeof(body) = 'blob' AND body = X''`:            1,
		`SELECT count(*) FROM idempotency WHERE key = 'pg-key' AND body = X'0001'`:                                1,
		`SELECT count(*) FROM item_after WHERE rowid = 3 AND dep_item = 'i2'`:                                     1,
		`SELECT count(*) FROM counter WHERE project = 'p' AND name = 'item' AND value = 8`:                        1,
		`SELECT count(*) FROM item WHERE id = 'i3' AND state = 'dropped' AND close_reason = '되돌린다'`:               1,
		`SELECT count(*) FROM event WHERE kind = 'export.test'`:                                                   1,
		`SELECT count(*) FROM resource_hold WHERE resource = 'env:pg' AND released_at IS NULL`:                    1,
		`SELECT count(*) FROM schema_version WHERE version = 18`:                                                  1,
		`SELECT count(*) FROM sqlite_sequence WHERE name = 'pick_eval' AND seq > (SELECT max(id) FROM pick_eval)`: 1,
	} {
		var got int64
		liteQuery(t, back, q, &got)
		if got != want {
			t.Errorf("%s = %d, 기대 %d", q, got, want)
		}
	}
	// item_after 의 순서 축: 낸 파일의 rowid 가 Postgres 의 seq 그대로다.
	var gotAfter string
	liteQuery(t, back, `SELECT group_concat(r, ',') FROM (SELECT rowid || ':' || item_id || ':' ||
		coalesce(dep_item, dep_job, dep_sha) AS r FROM item_after ORDER BY rowid)`, &gotAfter)
	if gotAfter != wantAfter {
		t.Fatalf("item_after 의 rowid 순서가 seq 와 다르다:\n 낸 파일 %s\n Postgres %s", gotAfter, wantAfter)
	}

	lite, err := OpenWithLogger(back, quietLog())
	if err != nil {
		t.Fatalf("낸 파일이 이 판의 fd 로 안 열린다: %v", err)
	}
	defer lite.Close()
	it, err := lite.GetItem(ctx, "p", "pg-item")
	if err != nil || it.Body != body || it.State != model.ItemClaimed || !slices.Equal(it.After, pgItem.After) {
		t.Fatalf("Postgres 에서 쓴 항목: %v\n 낸 파일 %+v\n Postgres %+v", err, it, pgItem)
	}
	claims, err := lite.ListLiveClaims(ctx, "p")
	if err != nil || !slices.ContainsFunc(claims, func(c model.Claim) bool { return c.ItemID == "pg-item" && c.SessionID == "s2" }) {
		t.Fatalf("Postgres 에서 잡은 선점: %v %+v", err, claims)
	}
	if revs, err := lite.ItemRevisions(ctx, "p", "pg-item"); err != nil ||
		!slices.ContainsFunc(revs, func(r model.ItemRevision) bool { return r.Reason == "정정" }) {
		t.Fatalf("Postgres 에서 쓴 개정: %v %+v", err, revs)
	}
	if js, err := lite.JudgmentsForItem(ctx, "p", "pg-item"); err != nil ||
		!slices.ContainsFunc(js, func(x model.Judgment) bool { return x.ID == j.ID }) {
		t.Fatalf("Postgres 에서 쓴 판단: %v %+v", err, js)
	}
	for word, id := range map[string]string{"되돌림검색어": j.ID, "남긴다": "j1"} {
		found, err := lite.SearchJudgments(ctx, "", word, 10)
		if err != nil || !slices.ContainsFunc(found, func(x model.Judgment) bool { return x.ID == id }) {
			t.Fatalf("낸 파일의 판단 검색 %q 이 %s 를 못 찾는다: %v %+v", word, id, err, found)
		}
	}

	// 낸 파일이 다음에 낼 번호는 Postgres 가 다음에 낼 번호와 같다(AUTOINCREMENT 표 - 지워진 최고 번호 뒤). rowid 표(item_after)는
	// max(rowid)+1 이다(pg.Floor).
	pgNext := map[string]int64{}
	for _, tb := range pg.Tables() {
		if c, ok := tb.Identity(); ok {
			seq, err := readIdentity(ctx, dst.db, tb.Name, c.Name)
			if err != nil {
				t.Fatal(err)
			}
			pgNext[tb.Name] = pg.NextValue(seq.last, seq.called)
		}
	}
	var maxSeq int64
	liteQuery(t, back, `SELECT max(rowid) FROM item_after`, &maxSeq)
	for _, c := range []struct {
		table, insert string
		want          int64
	}{
		{"pick_eval", `INSERT INTO pick_eval(project, session_id, at, rejected) VALUES ('p', 's1', 'x', '[]') RETURNING id`, pgNext["pick_eval"]},
		{"resource_hold", `INSERT INTO resource_hold(project, resource, session_id, acquired_at) VALUES ('p', 'r', 's1', 'x') RETURNING id`, pgNext["resource_hold"]},
		{"event", `INSERT INTO event(at, kind) VALUES ('x', 'k') RETURNING id`, pgNext["event"]},
		{"landing_queue", `INSERT INTO landing_queue(project, session_id, enqueued_at) VALUES ('p', 's2', 'x') RETURNING id`, pgNext["landing_queue"]},
		{"item_after", `INSERT INTO item_after(project, item_id, dep_sha) VALUES ('p', 'i1', 's') RETURNING rowid`, maxSeq + 1},
	} {
		var got int64
		liteQuery(t, back, c.insert, &got)
		if got != c.want {
			t.Errorf("%s 의 다음 번호 %d, 기대 %d", c.table, got, c.want)
		}
	}
	if pgNext["pick_eval"] != pickTop+1 {
		t.Fatalf("시험 전제: Postgres pick_eval 의 다음 번호 %d 는 지운 최고 번호 %d 뒤여야 한다", pgNext["pick_eval"], pickTop)
	}
}

// 예행은 본 실행과 같은 길(대조까지)을 임시 파일에 돌고 그 파일을 지운다 - 대상 디렉토리가 빈 채로 남는다. 원본은 읽기만 한다
// (행도 순번도 그대로). 예행 뒤의 본 실행은 그 자리에 새 원장을 낸다.
func TestExportSQLiteDryRunLeavesNoFile(t *testing.T) {
	ctx := context.Background()
	_, dsn, dst := importedPG(t)
	rows, seqs := targetRows(t, dst), identityStates(t, dst)
	back := exportBack(t)
	rep, err := ExportSQLite(ctx, dsn, back, TransferOptions{DryRun: true}, quietLog())
	if err != nil || !rep.TargetChecked || rep.Copied != 0 || !rep.Matched() || len(rep.Rows) != len(pg.Tables()) {
		t.Fatalf("예행: %v (대상을 쟀다 %v · 옮긴 행 %d · 표 %d · 대조 %v)", err, rep.TargetChecked, rep.Copied, len(rep.Rows),
			rep.Matched())
	}
	if got := dirNames(t, filepath.Dir(back)); len(got) != 0 {
		t.Fatalf("예행이 %v 를 남겼다", got)
	}
	if targetRows(t, dst) != rows || !maps.Equal(identityStates(t, dst), seqs) {
		t.Fatalf("역이관 예행이 원본을 바꿨다")
	}
	if rep, err := ExportSQLite(ctx, dsn, back, TransferOptions{}, quietLog()); err != nil || rep.Copied == 0 {
		t.Fatalf("예행 뒤 본 실행: %v (옮긴 행 %d)", err, rep.Copied)
	}
	if targetRows(t, dst) != rows || !maps.Equal(identityStates(t, dst), seqs) {
		t.Fatalf("역이관이 원본을 바꿨다")
	}
}

// 대상 자리에 파일이나 옆 파일(-wal · -shm · -journal)이 있으면 예행 · 본 실행 모두 거절하고 그 파일을 건드리지 않는다. 대상이
// Postgres 접속 URL 이어도 거절한다.
func TestExportSQLiteRefusesAnExistingTarget(t *testing.T) {
	ctx := context.Background()
	_, dsn, _ := importedPG(t)
	for _, name := range []string{"back.db", "back.db-wal", "back.db-shm", "back.db-journal"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("옛 원장"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, dry := range []bool{true, false} {
			_, err := ExportSQLite(ctx, dsn, filepath.Join(dir, "back.db"), TransferOptions{DryRun: dry}, quietLog())
			if err == nil || !strings.Contains(err.Error(), "이미 있다") {
				t.Fatalf("%s 가 있는데 예행 %v 가 거절하지 않았다: %v", name, dry, err)
			}
		}
		if got := dirNames(t, dir); !slices.Equal(got, []string{name}) {
			t.Fatalf("%s 거절 뒤 디렉토리: %v", name, got)
		}
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != "옛 원장" {
			t.Fatalf("%s 가 바뀌었다: %q %v", name, b, err)
		}
	}
	if _, err := ExportSQLite(ctx, dsn, dsn, TransferOptions{}, quietLog()); err == nil || !strings.Contains(err.Error(), "SQLite 파일만") {
		t.Fatalf("Postgres 대상에 역이관했다: %v", err)
	}
}

// 옮길 수 없는 원본은 파일을 남기지 않고 거절한다: Postgres 가 아닌 원본, 빈 DB, 판 18 이 아닌 원장, 그리고 이관의 사전 검사가
// 받지 않을 값(깨진 JSON)이 든 원장 - 낸 파일을 다시 이관할 수 없게 되므로 역이관도 받지 않는다.
func TestExportSQLiteRefusesASourceItCannotMove(t *testing.T) {
	ctx := context.Background()
	newer, newerStore := importTarget(t)
	stamp(t, newerStore, pg.ImportVersion+1)
	_, broken, brokenStore := importedPG(t)
	pgExec(t, brokenStore, `UPDATE item SET paths = '[깨진' WHERE id = 'i1'`)
	for _, c := range []struct {
		name, from, want string
	}{
		{"SQLite 원본", importSource(t, importSeed), "Postgres 접속 URL"},
		{"빈 DB", storetest.NewBare(t, ""), "빈 DB"},
		{"판 19", newer, "판 18 이 아니다"},
		{"깨진 JSON", broken, "사전 검사"},
	} {
		for _, dry := range []bool{true, false} {
			back := exportBack(t)
			rep, err := ExportSQLite(ctx, c.from, back, TransferOptions{DryRun: dry}, quietLog())
			if err == nil || !strings.Contains(err.Error(), c.want) || rep.Copied != 0 {
				t.Fatalf("%s(예행 %v): %v, 옮긴 행 %d - %q 로 거절해야 한다", c.name, dry, err, rep.Copied, c.want)
			}
			if got := dirNames(t, filepath.Dir(back)); len(got) != 0 {
				t.Fatalf("%s(예행 %v) 거절 뒤 %v 가 남았다", c.name, dry, got)
			}
			if c.name == "깨진 JSON" && (len(rep.Faults) != 1 || rep.Faults[0].Table != "item" || rep.Faults[0].Column != "paths") {
				t.Fatalf("걸린 갈래: %+v", rep.Faults)
			}
		}
	}
}

// 커밋 전 대조가 어긋나면 커밋하지 않는다 - 임시 원장이 비어 있는 채로(되감겼다) 오류가 나고, ExportSQLite 는 그 오류에서 이름을
// 붙이지 않고 임시 파일을 지운다. 대상 SQLite 에 행을 비트는 트리거를 걸어 어긋남을 만든다.
func TestExportSQLiteDoesNotCommitAMismatch(t *testing.T) {
	ctx := context.Background()
	_, dsn, _ := importedPG(t)
	src, err := openExportSource(ctx, ParseTarget(dsn), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, dry := range []bool{true, false} {
		tmp, err := newExportFile(ctx, exportBack(t), quietLog())
		if err != nil {
			t.Fatal(err)
		}
		liteExec(t, tmp, `CREATE TRIGGER t_bend_title AFTER INSERT ON item BEGIN
			UPDATE item SET title = title || '!' WHERE rowid = new.rowid; END`)
		var rep TransferReport
		err = exportInto(ctx, src, tmp, dry, &rep, quietLog())
		if err == nil || !strings.Contains(err.Error(), "커밋하지 않는다") {
			t.Fatalf("예행 %v: 어긋난 역이관이 끝났다: %v", dry, err)
		}
		for _, tc := range rep.Tables {
			if (tc.Table == "item") == tc.OK() {
				t.Errorf("예행 %v: %s 의 대조가 %v 다", dry, tc.Table, tc.OK())
			}
		}
		var n int64
		liteQuery(t, tmp, `SELECT (SELECT count(*) FROM item) + (SELECT count(*) FROM event) + (SELECT count(*) FROM sqlite_sequence)`, &n)
		if n != 0 {
			t.Fatalf("예행 %v: 커밋하지 않았는데 임시 원장에 %d 행", dry, n)
		}
	}
}
