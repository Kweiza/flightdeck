package pg_test

// 기준선을 PG 에서 잠근다: 추가 전용 트리거(UPDATE · DELETE · TRUNCATE) · FK 미루기와 이력 옮기기 · 살아 있는
// 선점 · identity 번호(서버 코드에 OVERRIDING 이 없는지까지) · snapshot 의 긴 키와 지문 겹침 · 앱 역할의 권한 ·
// 판단 검색 인덱스 · 준비(멱등 · 임시 표 · 앱 역할 검사 · 동시 준비). SQLite 와 맞대는 시험은 parity_test.go 다.
//
// ★ 외부 시험 패키지(pg_test)인 까닭: storetest 가 pg 를 import 한다. 패키지 안 시험이면 순환이다.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql 드라이버 "pgx"

	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// at 은 시험 행의 시각이다(저장 표기와 같은 고정 폭).
const at = "'2026-01-01T00:00:00.000000Z'"

// seedSQL 은 두 엔진에 똑같이 넣는 최소 원장이다: 프로젝트 둘 · 기계 하나 · 세션 둘 · 항목 둘, 그리고 추가 전용
// 표 셋에 한 행씩. 자리표시 없이 글자로만 적는다 — 두 엔진이 같은 문장을 받는다.
const seedSQL = `
INSERT INTO project(id, path, created_at) VALUES ('p', '/r/p', ` + at + `), ('p2', '/r/p2', ` + at + `);
INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m', 'h', ` + at + `, ` + at + `);
INSERT INTO session(id, project, machine_id, worktree, cc_session_id, opened_at)
  VALUES ('s1', 'p', 'm', '/w1', 'c1', ` + at + `), ('s2', 'p', 'm', '/w2', 'c2', ` + at + `);
INSERT INTO item(project, id, title, body, created_at)
  VALUES ('p', 'i1', 't', 'b', ` + at + `), ('p', 'i2', 't', 'b', ` + at + `);
INSERT INTO judgment(id, project, session_id, at, kind, title, body)
  VALUES ('j1', 'p', 's1', ` + at + `, 'decision', 't', '판단을 남긴다');
INSERT INTO event(at, project, session_id, kind) VALUES (` + at + `, 'p', 's1', 'k');
INSERT INTO item_revision(project, item_id, rev, at, session_id, title, body, paths, reason)
  VALUES ('p', 'i1', 1, ` + at + `, 's1', 't', 'b', '[]', 'r');
`

// longKey 는 B-tree 항목 상한(약 2.7KiB)을 넘는 키다. 압축으로 상한 밑까지 줄지 않게 지문을 이어 만든다.
var longKey = func() string {
	var b strings.Builder
	for i := range 200 {
		sum := sha256.Sum256([]byte{byte(i)})
		b.WriteString(hex.EncodeToString(sum[:]))
	}
	return b.String()
}()

// execer 는 *sql.DB · *sql.Conn · *sql.Tx 가 함께 갖는 것이다.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// openPG 는 기준선이 깔린 새 시험 DB 를 초사용자로 열고 seedSQL 을 넣는다.
func openPG(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", storetest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, seedSQL)
	return db
}

// as 는 role 로 바꾼 연결 하나를 낸다. 끝나면 역할을 되돌려 풀에 돌려준다 — 안 되돌리면 같은 풀의 다음
// 질의가 그 역할로 돈다.
func as(t *testing.T, db *sql.DB, role string) *sql.Conn {
	t.Helper()
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(context.Background(), "SET ROLE "+role); err != nil {
		c.Close()
		t.Fatalf("SET ROLE %s: %v", role, err)
	}
	t.Cleanup(func() {
		c.ExecContext(context.Background(), "RESET ROLE")
		c.Close()
	})
	return c
}

func mustExec(t *testing.T, db execer, q string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func count(t *testing.T, db execer, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// refused 는 q 가 SQLSTATE code 로 거절되는지 본다. constraint · msg 는 빈 값이면 안 본다.
func refused(t *testing.T, db execer, q, code, constraint, msg string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), q)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Errorf("%s: SQLSTATE %s 로 거절돼야 한다, 결과 %v", q, code, err)
		return
	}
	if pe.Code != code || (constraint != "" && pe.ConstraintName != constraint) || (msg != "" && pe.Message != msg) {
		t.Errorf("%s: 거절이 다르다\n  받음 %s · 제약 %q · %q\n  기대 %s · 제약 %q · %q",
			q, pe.Code, pe.ConstraintName, pe.Message, code, constraint, msg)
	}
}

// 추가 전용 표 셋은 UPDATE · DELETE · TRUNCATE 를 거절한다. 앱 역할에게도 주인에게도.
//   - UPDATE · DELETE: 행 트리거가 막는다(23000, 제약 자리에 트리거 이름, 문구는 SQLite 트리거와 같다).
//   - TRUNCATE: 앱 역할은 권한부터 없다(42501). 주인은 권한이 있어도 문 트리거가 막는다 — PG 의 TRUNCATE 는
//     행 트리거를 건너뛰므로 이 줄이 없으면 주인 한 문장에 표가 빈다.
func TestAppendOnlyRefusesUpdateDeleteTruncate(t *testing.T) {
	db := openPG(t)
	app, owner := as(t, db, pg.AppRole), as(t, db, pg.OwnerRole)
	for _, c := range []struct {
		table, update, updateMsg, deleteMsg, truncateMsg string
	}{
		{"judgment", "UPDATE judgment SET body = 'x'",
			"judgment 는 추가 전용이다 — 정정은 새 행 + supersedes 로 남겨라", "judgment 는 삭제할 수 없다",
			"judgment 는 비울 수 없다 — 추가 전용이다"},
		{"event", "UPDATE event SET kind = 'x'",
			"event 는 추가 전용이다", "event 는 삭제할 수 없다",
			"event 는 비울 수 없다 — 추가 전용이다"},
		{"item_revision", "UPDATE item_revision SET body = 'x'",
			"item_revision 은 추가 전용이다 — 개정은 새 rev 로 쌓아라",
			"item_revision 은 추가 전용이다 — 지우면 복구 경로가 0이 된다",
			"item_revision 은 비울 수 없다 — 추가 전용이다"},
	} {
		t.Run(c.table, func(t *testing.T) {
			for _, who := range []*sql.Conn{app, owner} {
				refused(t, who, c.update, "23000", c.table+"_no_update", c.updateMsg)
				refused(t, who, "DELETE FROM "+c.table, "23000", c.table+"_no_delete", c.deleteMsg)
			}
			// CASCADE 로 센다: FK 가 가리키는 표(judgment)는 CASCADE 없이는 트리거 앞에서 다른 까닭(0A000)으로
			// 거절된다 — 그것은 이 줄이 지키는 것이 아니다.
			refused(t, app, "TRUNCATE "+c.table+" CASCADE", "42501", "", "")
			refused(t, owner, "TRUNCATE "+c.table+" CASCADE", "23000", c.table+"_no_truncate", c.truncateMsg)
			if n := count(t, db, "SELECT count(*) FROM "+c.table); n != 1 {
				t.Errorf("%s 가 %d 행이다, 거절 뒤에도 1 행이어야 한다", c.table, n)
			}
		})
	}
	// 다른 표를 비우다 FK 로 끌려 들어와도 막힌다: project → item → item_revision, project → judgment.
	_, err := owner.ExecContext(context.Background(), "TRUNCATE project CASCADE")
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23000" || !strings.HasSuffix(pe.ConstraintName, "_no_truncate") {
		t.Errorf("TRUNCATE project CASCADE 가 추가 전용 표를 끌고 가 비웠다(또는 다른 까닭으로 거절): %v", err)
	}
}

// item_revision 은 내용 칼럼 여덟만 못 고친다(증분 017). project 는 항목을 옮길 때 함께 바뀌는 좌표다.
// 옮기기는 FK 를 커밋까지 미뤄야 선다 — 항목의 키와 딸린 행의 키가 한 문장에 함께 못 바뀐다.
func TestItemRevisionFollowsMoveUnderDeferredForeignKeys(t *testing.T) {
	db := openPG(t)
	app := as(t, db, pg.AppRole)
	for _, col := range []string{"item_id", "rev", "at", "session_id", "title", "body", "paths", "reason"} {
		refused(t, app, "UPDATE item_revision SET "+col+" = "+col, "23000", "item_revision_no_update", "")
	}
	mustExec(t, app, "INSERT INTO item_after(project, item_id, dep_sha) VALUES ('p', 'i1', 'abc')")
	mustExec(t, app, "INSERT INTO claim(project, item_id, session_id, at) VALUES ('p', 'i1', 's1', "+at+")")

	// 미루지 않으면 항목 키를 바꾸는 첫 문장이 딸린 행 때문에 거절된다 — 미루기가 하는 일이 그것이다.
	refused(t, app, "UPDATE item SET project = 'p2' WHERE project = 'p' AND id = 'i1'", "23503", "", "")

	tx, err := app.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		"SET CONSTRAINTS ALL DEFERRED",
		"UPDATE item SET project = 'p2' WHERE project = 'p' AND id = 'i1'",
		"UPDATE item_after SET project = 'p2' WHERE project = 'p' AND item_id = 'i1'",
		"UPDATE claim SET project = 'p2' WHERE project = 'p' AND item_id = 'i1'",
		"UPDATE item_revision SET project = 'p2' WHERE project = 'p' AND item_id = 'i1'",
	} {
		mustExec(t, tx, q)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("FK 를 미룬 옮기기가 커밋되지 않았다: %v", err)
	}
	if n := count(t, db, "SELECT count(*) FROM item_revision WHERE project = 'p2'"); n != 1 {
		t.Fatalf("옮긴 뒤 p2 의 개정이 %d 행이다, 1 이어야 한다", n)
	}
}

// 이력만 옮기는 것은 커밋에서 거절된다. 옮긴 곳에 마침 같은 id 의 항목이 있으면 FK 는 지나므로, 그대로 두면
// 남의 이력이 된다. 옛 자리에 항목이 남아 있는지를 커밋 때 본다(미룬 제약 트리거).
func TestItemRevisionProjectMovesOnlyWithTheItem(t *testing.T) {
	db := openPG(t)
	app := as(t, db, pg.AppRole)
	mustExec(t, app, "INSERT INTO item(project, id, title, body, created_at) VALUES ('p2', 'i1', 'other', 'b', "+at+")")

	tx, err := app.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// 문장은 지난다: 옮긴 곳에 항목이 있어 FK 가 서고, 이 검사는 커밋까지 미뤄져 있다.
	mustExec(t, tx, "UPDATE item_revision SET project = 'p2' WHERE project = 'p' AND item_id = 'i1'")
	err = tx.Commit()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23000" || pe.ConstraintName != "item_revision_moves_with_item" {
		t.Fatalf("항목은 두고 이력만 옮긴 커밋이 23000(item_revision_moves_with_item)으로 거절돼야 한다: %v", err)
	}
	if n := count(t, db, "SELECT count(*) FROM item_revision WHERE project = 'p' AND item_id = 'i1'"); n != 1 {
		t.Fatalf("거절 뒤 p/i1 의 개정이 %d 행이다, 1 이어야 한다", n)
	}
}

// claimUpsert 는 store/item.go 의 ClaimItem 이 쓰는 선점 문장 그대로다(자리표시만 글자로 채웠다).
// 반납된 선점 행이 키 자리에 남아 있어 단순 INSERT 가 아니라 upsert 다.
func claimUpsert(item, session string) string {
	return `INSERT INTO claim(project, item_id, session_id, at, released_at, force_reason)
		VALUES ('p', '` + item + `', '` + session + `', ` + at + `, NULL, NULL)
		ON CONFLICT(project, item_id) DO UPDATE SET
		  session_id = excluded.session_id, at = excluded.at, released_at = NULL, force_reason = NULL`
}

// forceRelease 는 store/item.go 의 ForceReleaseClaim 이 쓰는 회수 문장 그대로다.
const forceRelease = `UPDATE claim SET released_at = ` + at + `, force_reason = 'r'
	WHERE project = 'p' AND item_id = 'i1' AND released_at IS NULL`

// 살아 있는 남의 선점은 upsert 가 덮지 못한다. SQLite 는 BEGIN IMMEDIATE 가 「점유자 확인 → 쓰기」 사이를
// 닫아 코드의 판정이 지키지만, PG 의 READ COMMITTED 에서는 둘이 같이 「점유자 없음」을 보고 upsert 할 수
// 있다 — 그때 늦은 쪽이 받는 것이 이 거절이다. 회수(released_at 을 먼저 찍는다) 뒤에는 넘어간다.
func TestClaimUpsertCannotTakeALiveClaim(t *testing.T) {
	db := openPG(t)
	app := as(t, db, pg.AppRole)
	mustExec(t, app, claimUpsert("i1", "s1"))
	mustExec(t, app, claimUpsert("i1", "s1")) // 같은 세션이 다시 쥐는 것은 넘김이 아니다
	refused(t, app, claimUpsert("i1", "s2"), "23505", "claim_one_live_holder",
		"살아 있는 선점은 넘어가지 않는다(항목 p/i1 · 점유 세션 s1) — 회수(released_at)가 먼저다")
	refused(t, app, "UPDATE claim SET session_id = 's2' WHERE item_id = 'i1'", "23505", "claim_one_live_holder", "")
	if n := count(t, db, "SELECT count(*) FROM claim WHERE item_id = 'i1' AND session_id = 's1' AND released_at IS NULL"); n != 1 {
		t.Fatalf("거절 뒤 s1 의 살아 있는 선점이 %d 행이다, 1 이어야 한다", n)
	}
	mustExec(t, app, forceRelease)
	mustExec(t, app, claimUpsert("i1", "s2"))
	if n := count(t, db, "SELECT count(*) FROM claim WHERE item_id = 'i1' AND session_id = 's2' AND released_at IS NULL"); n != 1 {
		t.Fatalf("회수 뒤 s2 의 선점이 %d 행이다, 1 이어야 한다", n)
	}
}

// identity 번호는 시퀀스에서만 나온다. id 를 적어 넣으면 시퀀스가 안 올라 다음 자동 번호가 기본키 충돌로
// 죽으므로 ALWAYS 로 거절한다. 옛 번호를 옮기는 이관만 소유 역할로 OVERRIDING SYSTEM VALUE 를 쓰고 setval 로
// 번호를 맞춘다 — 그 길이 서는지도 본다.
func TestIdentityNumbersComeOnlyFromTheSequence(t *testing.T) {
	db := openPG(t)
	app, owner := as(t, db, pg.AppRole), as(t, db, pg.OwnerRole)
	for _, q := range []string{
		"INSERT INTO event(id, at, kind) VALUES (100, " + at + ", 'k')",
		"INSERT INTO pick_eval(id, project, session_id, at, rejected) VALUES (100, 'p', 's1', " + at + ", '[]')",
		"INSERT INTO resource_hold(id, project, resource, session_id, acquired_at) VALUES (100, 'p', 'r', 's1', " + at + ")",
		"INSERT INTO landing_queue(id, project, session_id, enqueued_at) VALUES (100, 'p', 's1', " + at + ")",
		"INSERT INTO item_after(seq, project, item_id, dep_sha) VALUES (100, 'p', 'i1', 'abc')",
	} {
		refused(t, app, q, "428C9", "", "")
	}
	mustExec(t, owner, "INSERT INTO event(id, at, kind) OVERRIDING SYSTEM VALUE VALUES (100, "+at+", 'old')")
	mustExec(t, owner, "SELECT setval(pg_get_serial_sequence('event', 'id'), (SELECT max(id) FROM event))")
	var id int
	if err := app.QueryRowContext(context.Background(),
		"INSERT INTO event(at, kind) VALUES ("+at+", 'new') RETURNING id").Scan(&id); err != nil || id != 101 {
		t.Fatalf("이관 뒤 다음 자동 번호가 %d(%v)다, 101 이어야 한다", id, err)
	}
}

// snapshot 의 키는 길이 제한이 없다(SQLite 가 받는다). PG 는 키의 지문을 기본키로 둔다 — B-tree 항목의
// 상한(약 2.7KiB)을 넘는 키도 들어가고, 같은 키는 한 행이며, upsert 의 충돌 대상은 (project, key_digest)다.
func TestSnapshotKeyHasNoLengthLimit(t *testing.T) {
	db := openPG(t)
	app := as(t, db, pg.AppRole)
	put := func(key, value string) string {
		return `INSERT INTO snapshot(project, key, value, method, computed_at)
			VALUES ('p', '` + key + `', '` + value + `', 'command', ` + at + `)
			ON CONFLICT(project, key_digest) DO UPDATE SET value = excluded.value`
	}
	mustExec(t, app, put(longKey, "1"))
	mustExec(t, app, put(longKey, "2"))
	mustExec(t, app, put(longKey+"x", "3"))
	if n := count(t, db, "SELECT count(*) FROM snapshot WHERE key = '"+longKey+"' AND value = '2'"); n != 1 {
		t.Errorf("같은 긴 키의 upsert 뒤 그 키의 행(값 2)이 %d 개다, 1 이어야 한다", n)
	}
	if n := count(t, db, "SELECT count(*) FROM snapshot"); n != 2 {
		t.Errorf("snapshot 이 %d 행이다, 키 둘이라 2 여야 한다", n)
	}
	refused(t, app, "UPDATE snapshot SET key_digest = 'x'", "428C9", "", "")
}

// 지문이 같은 다른 키는 남의 행을 덮지 못한다. 기본키가 지문이라 그대로 두면 upsert 가 충돌 갈래로 가서 남의
// 값을 말없이 덮는다. sha256 겹침은 만들 수 없으므로 **이 DB 에서만** 지문 함수를 상수로 바꿔 모든 키를
// 겹치게 한다(시험 전용 경로 — 계산 칼럼은 함수를 이름이 아니라 OID 로 물고 있어 본문만 바뀐다).
func TestSnapshotKeyDigestCollisionIsRefused(t *testing.T) {
	db := openPG(t)
	mustExec(t, db, `CREATE OR REPLACE FUNCTION fd_key_digest(key text) RETURNS bytea
		LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE RETURN '\x00'::bytea`)
	app := as(t, db, pg.AppRole)
	put := func(key, value string) string {
		return `INSERT INTO snapshot(project, key, value, method, computed_at)
			VALUES ('p', '` + key + `', '` + value + `', 'command', ` + at + `)
			ON CONFLICT(project, key_digest) DO UPDATE SET value = excluded.value`
	}
	const msg = "snapshot 키의 지문이 다른 키와 겹친다(프로젝트 p) — 다른 키의 행을 덮지 않는다"
	mustExec(t, app, put("k1", "1"))
	refused(t, app, put("k2", "2"), "23000", "snapshot_key_digest_unique", msg)
	refused(t, app, "INSERT INTO snapshot(project, key, value, method, computed_at) VALUES ('p', 'k2', '2', 'command', "+at+")",
		"23000", "snapshot_key_digest_unique", msg)
	mustExec(t, app, put("k1", "3")) // 같은 키는 겹침이 아니다 — 제 행을 고친다
	if n := count(t, db, "SELECT count(*) FROM snapshot WHERE key = 'k1' AND value = '3'"); n != 1 {
		t.Errorf("k1 의 행(값 3)이 %d 개다, 1 이어야 한다", n)
	}
	if n := count(t, db, "SELECT count(*) FROM snapshot"); n != 1 {
		t.Errorf("snapshot 이 %d 행이다, 겹친 키는 못 들어가 1 이어야 한다", n)
	}
}

// 앱 코드는 identity 번호를 손으로 정하지 않는다. GENERATED ALWAYS 는 실수만 막는다 — OVERRIDING SYSTEM VALUE
// 는 INSERT 권한만 보므로 앱 역할도 쓸 수 있다. 그래서 그 구절이 서버 코드(시험 · 이 스키마 패키지 밖)에 없다는
// 것을 소스에서 잰다. 옛 번호를 옮기는 이관의 그 구절은 이 패키지(import.go 의 InsertSQL)에만 있고, 이관은
// 소유 역할로만 돈다 — 그래서 이 패키지는 재지 않는다.
func TestServerCodeNeverOverridesIdentity(t *testing.T) {
	root, err := filepath.Abs("../../..") // server 모듈 뿌리
	if err != nil {
		t.Fatal(err)
	}
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	overriding := regexp.MustCompile(`(?i)\boverriding\b`)
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && path == here:
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if overriding.Match(src) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s 에 OVERRIDING 이 있다 — 앱 코드는 identity 번호를 손으로 정하지 않는다", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("훑은 Go 파일이 %d 개다 — 뿌리(%s)를 잘못 잡았다", scanned, root)
	}
}

// 앱 역할은 행을 읽고 쓰기만 한다. 주인은 소유 역할이다.
func TestAppRoleIsDMLOnly(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	// ① 표 권한이 정확히 이것이다. 표를 더하는 증분이 GRANT 를 빠뜨리면 그 표가 빈 집합으로 걸린다.
	privs := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"}
	rows, err := db.QueryContext(ctx, `SELECT c.relname, pg_get_userbyid(c.relowner) FROM pg_class c
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r' ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{}
	for rows.Next() {
		var name, owner string
		if err := rows.Scan(&name, &owner); err != nil {
			t.Fatal(err)
		}
		owners[name] = owner
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(owners) == 0 {
		t.Fatal("public 에 표가 없다 — 원판에 기준선이 안 깔렸다")
	}
	for table, owner := range owners {
		if owner != pg.OwnerRole {
			t.Errorf("표 %s 의 주인이 %s 다, %s 여야 한다", table, owner, pg.OwnerRole)
		}
		var got []string
		for _, p := range privs {
			var ok bool
			if err := db.QueryRowContext(ctx, "SELECT has_table_privilege($1, format('%I', $2::text), $3)",
				pg.AppRole, table, p).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				got = append(got, p)
			}
		}
		want := "SELECT,INSERT,UPDATE,DELETE"
		if table == "schema_version" {
			want = "SELECT" // 판을 적는 것은 이전 실행기다
		}
		if g := strings.Join(got, ","); g != want {
			t.Errorf("앱 역할의 %s 권한이 [%s] 다, [%s] 여야 한다", table, g, want)
		}
	}
	// 트리거 함수도 소유 역할의 것이다. pg_trgm 의 함수(확장의 몫)는 뺀다.
	var fns []string
	for _, f := range strs(t, db, `SELECT p.proname || ' ' || pg_get_userbyid(p.proowner) FROM pg_proc p
		WHERE p.pronamespace = 'public'::regnamespace AND NOT EXISTS (SELECT 1 FROM pg_depend d
		  WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
		ORDER BY 1`) {
		fns = append(fns, f[0])
	}
	if want := []string{"fd_append_only " + pg.OwnerRole, "fd_claim_one_live_holder " + pg.OwnerRole,
		"fd_item_revision_moves_with_item " + pg.OwnerRole, "fd_key_digest " + pg.OwnerRole,
		"fd_snapshot_key_digest_unique " + pg.OwnerRole}; !slices.Equal(fns, want) {
		t.Errorf("함수와 주인이 %q 다, %q 여야 한다", fns, want)
	}

	// ② 시퀀스 권한이 없어도 identity 칼럼은 번호를 받는다. 번호를 되감는 것(setval)은 못 한다.
	//    시퀀스는 identity 칼럼 다섯(pick_eval · resource_hold · event · landing_queue 의 id, item_after.seq)의 것이다.
	const seqs = `FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'S'`
	if n := count(t, db, "SELECT count(*) "+seqs); n != 5 {
		t.Errorf("시퀀스가 %d 개다, identity 칼럼 다섯의 것 5 개여야 한다", n)
	}
	if n := count(t, db, "SELECT count(*) "+seqs+` AND (has_sequence_privilege('`+pg.AppRole+`', c.oid, 'USAGE')
		OR has_sequence_privilege('`+pg.AppRole+`', c.oid, 'UPDATE'))`); n != 0 {
		t.Errorf("앱 역할이 시퀀스 %d 개에 USAGE · UPDATE 를 가졌다, 0 이어야 한다", n)
	}
	app := as(t, db, pg.AppRole)
	mustExec(t, app, "INSERT INTO event(at, kind) VALUES ("+at+", 'k2')")
	mustExec(t, app, "INSERT INTO pick_eval(project, session_id, at, rejected) VALUES ('p', 's1', "+at+", '[]')")
	refused(t, app, "SELECT setval(pg_get_serial_sequence('event', 'id'), 1)", "42501", "", "")

	// ③ 스키마를 못 바꾸고, 추가 전용을 우회할 손잡이도 없다.
	for _, q := range []string{
		"CREATE TABLE app_made (v int)",
		"ALTER TABLE judgment ADD COLUMN x int",
		"ALTER TABLE judgment DISABLE TRIGGER judgment_no_update",
		"DROP TRIGGER judgment_no_delete ON judgment",
		"SET session_replication_role = replica", // 트리거를 통째로 끄는 길
	} {
		refused(t, app, q, "42501", "", "")
	}

	// ④ 주인은 스키마를 바꾼다 — 이전 실행기의 자리다.
	owner := as(t, db, pg.OwnerRole)
	mustExec(t, owner, "CREATE TABLE owner_made (v int)")
	mustExec(t, owner, "ALTER TABLE judgment ADD COLUMN x int")
}

// FK 는 전부 DEFERRABLE INITIALLY IMMEDIATE 다. SQLite 의 PRAGMA defer_foreign_keys 가 FK 전부를 미루므로,
// 「SET CONSTRAINTS ALL DEFERRED 가 FK 전부를 미룬다」가 같은 뜻이다. 평소에는 지금처럼 문장마다 본다.
func TestEveryForeignKeyIsDeferrableInitiallyImmediate(t *testing.T) {
	db := openPG(t)
	if n := count(t, db, `SELECT count(*) FROM pg_constraint WHERE contype = 'f'
		AND connamespace = 'public'::regnamespace`); n == 0 {
		t.Fatal("FK 가 하나도 없다 — 기준선이 안 깔렸다")
	}
	rows, err := db.Query(`SELECT conrelid::regclass::text || '.' || conname FROM pg_constraint
		WHERE contype = 'f' AND connamespace = 'public'::regnamespace AND (NOT condeferrable OR condeferred)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		t.Errorf("FK %s 가 DEFERRABLE INITIALLY IMMEDIATE 가 아니다", name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// 판단 검색은 judgment_text_trgm 식 인덱스를 탄다. 검색이 쓸 꼴(식 그대로에 ILIKE)을 여기서 못박는다 —
// 식이 한 글자라도 다르면 인덱스를 안 타고 조용히 순차 훑기가 된다. 한글이 맞는지도 본다(로케일 축).
func TestJudgmentSearchUsesTheTrigramIndex(t *testing.T) {
	db := openPG(t)
	app := as(t, db, pg.AppRole)
	const where = ` FROM judgment WHERE (coalesce(title, '') || ' ' || body) ILIKE '%판단을%'`
	var id string
	if err := app.QueryRowContext(context.Background(), "SELECT id"+where).Scan(&id); err != nil || id != "j1" {
		t.Fatalf("한글 부분 일치가 j1 을 못 찾았다: %q, %v", id, err)
	}
	// 행이 적으면 계획기가 순차 훑기를 고른다. 그 선택지를 끄고 인덱스가 **쓰일 수 있는지**를 본다.
	mustExec(t, app, "SET enable_seqscan = off")
	t.Cleanup(func() { app.ExecContext(context.Background(), "RESET enable_seqscan") })
	rows, err := app.QueryContext(context.Background(), "EXPLAIN SELECT id"+where)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "judgment_text_trgm") {
		t.Fatalf("검색이 judgment_text_trgm 을 안 탄다:\n%s", plan.String())
	}
}

// 준비는 몇 번 돌려도 같다: 역할 · 권한 · 확장이 이미 있는 DB 에 다시 돌려도 성공한다.
func TestBootstrapIsRepeatable(t *testing.T) {
	db := openPG(t)
	mustExec(t, db, pg.Bootstrap)
	mustExec(t, db, pg.Bootstrap)
}

// 앱 역할은 임시 표를 못 만든다. 만들 수 있으면 원장 표와 같은 이름의 임시 표가 그 연결의 읽기 · 쓰기를
// 원장 대신 받는다(pg_temp 가 search_path 맨 앞에 낀다). 준비가 DB 의 TEMPORARY 를 PUBLIC 에서 거둔다.
// ★ 여느 시험 DB 그대로 잰다: DB 단위 권한은 템플릿 복제로 안 따라오므로 storetest 가 복제마다 준비를 다시
// 돌린다 — 그것을 빠뜨리면 다른 모든 시험이 앱이 임시 표를 만들 수 있는 DB 에서 돈다.
func TestAppRoleCannotShadowTheLedgerWithTempTables(t *testing.T) {
	db := openPG(t)
	app := as(t, db, pg.AppRole)
	refused(t, app, "CREATE TEMP TABLE judgment (id text)", "42501", "", "")
}

// roleWord 는 준비 SQL 안의 역할 이름이다. 역할은 클러스터 전역이고 다른 패키지의 시험이 같은 서버에서
// 동시에 도므로, 역할을 망가뜨려 보는 시험은 fd_owner · fd_app 을 건드리지 않고 제 이름으로 바꿔 돌린다.
var roleWord = regexp.MustCompile(`\b(fd_owner|fd_app)\b`)

// bootstrapFor 는 역할 이름만 owner · app 으로 바꾼 준비 SQL 이다.
func bootstrapFor(owner, app string) string {
	return roleWord.ReplaceAllStringFunc(pg.Bootstrap, func(w string) string {
		if w == pg.OwnerRole {
			return owner
		}
		return app
	})
}

// newRoles 는 이 시험만의 역할 이름 n 개를 짓는다. 끝나면 dbs 마다 그 역할의 권한을 거두고 역할을 지운다
// (DB 는 그 뒤에 지워진다 — 권한이 남은 역할은 지울 수 없다).
func newRoles(t *testing.T, dbs []*sql.DB, n int) []string {
	t.Helper()
	tag := strings.ToLower(rand.Text()[:10])
	var names []string
	for i := range n {
		names = append(names, fmt.Sprintf("fdt%d_%s", i, tag))
	}
	t.Cleanup(func() {
		ctx := context.Background()
		var have []string
		for _, r := range strs(t, dbs[0], "SELECT rolname FROM pg_roles WHERE rolname = ANY($1) ORDER BY 1", names) {
			have = append(have, r[0])
		}
		if len(have) == 0 {
			return
		}
		list := strings.Join(have, ", ")
		for _, db := range dbs {
			if _, err := db.ExecContext(ctx, "DROP OWNED BY "+list); err != nil {
				t.Errorf("DROP OWNED BY %s: %v", list, err)
			}
		}
		if _, err := dbs[0].ExecContext(ctx, "DROP ROLE "+list); err != nil {
			t.Errorf("DROP ROLE %s: %v", list, err)
		}
	})
	return names
}

// 준비는 이미 있던 앱 역할이 행만 읽고 쓰는 역할인지 재고, 아니면 멈춘다(고쳐 쓰지 않는다). 각 갈래는 앱 역할이
// 소유 역할의 권한을 얻거나 권한 검사를 건너뛰는 길 하나다.
func TestBootstrapRefusesAnAppRoleThatIsMoreThanDML(t *testing.T) {
	// cast 는 한 갈래의 역할 이름이다: 소유 · 앱 역할의 자리, 접속 계정, 중간 그룹.
	type cast struct{ owner, app, login, group, db string }
	for _, c := range []struct {
		name  string
		setup func(r cast) []string
		want  func(r cast) string
	}{
		{"소유 역할로 바꿔 탄다(INHERIT 없이 SET 만)", func(r cast) []string {
			return []string{"CREATE ROLE " + r.owner, "CREATE ROLE " + r.app,
				"GRANT " + r.owner + " TO " + r.app + " WITH INHERIT FALSE, SET TRUE"}
		}, func(r cast) string { return r.owner + " 의 구성원" }},
		{"미리 만든 역할의 구성원(pg_write_all_data)", func(r cast) []string {
			return []string{"CREATE ROLE " + r.app, "GRANT pg_write_all_data TO " + r.app}
		}, func(cast) string { return "pg_write_all_data 의 구성원" }},
		{"접속 계정이 소유 역할로도 바꿔 탄다", func(r cast) []string {
			return []string{"CREATE ROLE " + r.owner, "CREATE ROLE " + r.app, "CREATE ROLE " + r.login + " LOGIN",
				"GRANT " + r.app + " TO " + r.login, "GRANT " + r.owner + " TO " + r.login}
		}, func(r cast) string { return r.login + " 가 " }},
		{"중간 그룹을 거친 접속 계정이 소유 역할로도 바꿔 탄다", func(r cast) []string {
			return []string{"CREATE ROLE " + r.owner, "CREATE ROLE " + r.app, "CREATE ROLE " + r.group,
				"CREATE ROLE " + r.login + " LOGIN", "GRANT " + r.app + " TO " + r.group,
				"GRANT " + r.group + " TO " + r.login, "GRANT " + r.owner + " TO " + r.login + " WITH INHERIT FALSE, SET TRUE"}
		}, func(r cast) string { return r.login + " 가 " }},
		{"SUPERUSER", func(r cast) []string { return []string{"CREATE ROLE " + r.app + " SUPERUSER"} },
			func(cast) string { return "SUPERUSER" }},
		{"BYPASSRLS", func(r cast) []string { return []string{"CREATE ROLE " + r.app + " BYPASSRLS"} },
			func(cast) string { return "BYPASSRLS" }},
		{"CREATEROLE", func(r cast) []string { return []string{"CREATE ROLE " + r.app + " CREATEROLE"} },
			func(cast) string { return "CREATEROLE" }},
		{"DB 에 임시 표를 만든다", func(r cast) []string {
			return []string{"CREATE ROLE " + r.app, "GRANT TEMPORARY ON DATABASE " + r.db + " TO " + r.app}
		}, func(cast) string { return "DB 의 TEMPORARY" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := openPG(t)
			n := newRoles(t, []*sql.DB{db}, 4)
			r := cast{owner: n[0], app: n[1], login: n[2], group: n[3]}
			if err := db.QueryRow("SELECT current_database()").Scan(&r.db); err != nil {
				t.Fatal(err)
			}
			for _, q := range c.setup(r) {
				mustExec(t, db, q)
			}
			_, err := db.Exec(bootstrapFor(r.owner, r.app))
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "55000" || !strings.Contains(pe.Message, c.want(r)) {
				t.Fatalf("준비가 55000 「…%s…」 로 멈춰야 한다: %v", c.want(r), err)
			}
		})
	}
}

// 두 DB 를 동시에 준비해도 둘 다 성공한다. 앞 쪽이 역할을 만들고 아직 커밋하지 않았으면 뒤 쪽은 그 커밋을
// 기다렸다가 「이미 있다」를 duplicate_object 가 아니라 pg_authid 의 유니크 위반(23505)으로 받는다 — 그 순간을
// 잡으려고 뒤 쪽이 잠금을 기다리는 것을 확인한 뒤에 앞 쪽을 커밋한다.
func TestBootstrapRunsConcurrentlyOnTwoDatabases(t *testing.T) {
	ctx := context.Background()
	db1, db2 := openPG(t), openPG(t)
	r := newRoles(t, []*sql.DB{db1, db2}, 2)
	script := bootstrapFor(r[0], r[1])

	first, err := db1.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	if _, err := first.ExecContext(ctx, script); err != nil {
		t.Fatalf("앞 쪽 준비: %v", err)
	}
	second, err := db2.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var pid int
	if err := second.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := second.ExecContext(ctx, script)
		done <- err
	}()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if n := count(t, db1, fmt.Sprintf(
			"SELECT count(*) FROM pg_stat_activity WHERE pid = %d AND wait_event_type = 'Lock'", pid)); n == 1 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("뒤 쪽이 앞 쪽의 커밋을 기다리지 않고 끝났다(겹침을 못 만들었다): %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("뒤 쪽이 30초 안에 잠금 대기에 들어가지 않았다")
		}
	}
	if err := first.Commit(); err != nil {
		t.Fatalf("앞 쪽 커밋: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("앞 쪽을 기다린 뒤 쪽 준비가 실패했다: %v", err)
	}
}
