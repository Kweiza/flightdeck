package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kweiza/flightdeck/internal/model"
	sqlite3 "modernc.org/sqlite/lib"
)

// 제약 위반 → 타입 있는 오류.
//
// 이 파일의 단정은 두 층이다.
//   ① 순수 함수(JudgeConstraintCode) — 코드 하나하나의 판정과 **사유**
//   ② 실물 드라이버 — 우리가 기대는 것이 실제로 그렇게 나오는가
//
// ②가 없으면 ①은 우리가 지어낸 숫자표를 우리가 단정하는 것이 된다.
// 드라이버 판올림이 확장 결과 코드를 안 켜는 날 ①은 초록인 채로 전부 500 이 나간다.

// ─────────────────────────────────────────────────────────────────────────────
// ① 순수 함수
// ─────────────────────────────────────────────────────────────────────────────

func TestJudgeConstraintCode(t *testing.T) {
	cases := []struct {
		name         string
		code         int
		wantKind     ConflictKind
		wantInReason string
	}{
		{"PK 중복", sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, ConflictDuplicate, "이미 있다"},
		{"UNIQUE 중복", sqlite3.SQLITE_CONSTRAINT_UNIQUE, ConflictDuplicate, "이미 있다"},
		{"rowid 중복", sqlite3.SQLITE_CONSTRAINT_ROWID, ConflictDuplicate, "이미 있다"},
		{"FK", sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY, ConflictMissingRef, "등록돼 있지 않다"},

		// ★ 아래 셋은 **일부러 안 접는다.** 1차 방어를 지나 DB 까지 닿았다는 뜻이라
		//   호출자가 고칠 거리가 아니다. 4xx 로 접으면 서버 결함이 "사용자 잘못"으로
		//   분류돼 영영 안 고쳐진다.
		{"CHECK 는 안 접는다", sqlite3.SQLITE_CONSTRAINT_CHECK, "", "1차 방어"},
		{"NOT NULL 은 안 접는다", sqlite3.SQLITE_CONSTRAINT_NOTNULL, "", "1차 방어"},
		{"트리거 ABORT 는 안 접는다", sqlite3.SQLITE_CONSTRAINT_TRIGGER, "", "1차 방어"},

		{"확장 코드 없는 제약", sqlite3.SQLITE_CONSTRAINT, "", "종류를 못 가린다"},
		{"모르는 제약 축", sqlite3.SQLITE_CONSTRAINT_VTAB, "", "아는 축이 아닌"},
		{"제약이 아님", sqlite3.SQLITE_BUSY, "", "제약 위반이 아니다"},
		{"코드 없음", 0, "", "결과 코드가 없다"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := JudgeConstraintCode(c.code)
			if got.Kind != c.wantKind {
				t.Errorf("Kind = %q, want %q (사유: %s)", got.Kind, c.wantKind, got.Reason)
			}
			if !strings.Contains(got.Reason, c.wantInReason) {
				t.Errorf("사유에 %q 가 없다: %q", c.wantInReason, got.Reason)
			}
		})
	}
}

// TestJudgeBusyFamily 는 SQLite BUSY 를 확장 코드부터 가르는지 잰다. 확장 없는 BUSY 만 「busy_timeout 을 다
// 기다렸다」이고, BUSY_SNAPSHOT 은 기다린 것이 아니라 낡은 스냅숏이다(처방이 「트랜잭션 전체를 다시」다).
func TestJudgeBusyFamily(t *testing.T) {
	for _, c := range []struct {
		name      string
		code      int
		fault     FaultKind
		inReason  string
		notReason string
	}{
		{"잠금 못 얻음", sqlite3.SQLITE_BUSY, FaultLockUnavailable, "교착 회피 즉시 반환", "busy_timeout 을 넘었다"},
		{"낡은 스냅숏", sqlite3.SQLITE_BUSY_SNAPSHOT, FaultSerialization, "BUSY_SNAPSHOT", "넘었다"},
		{"복구 중", sqlite3.SQLITE_BUSY_RECOVERY, FaultLockUnavailable, "단정하지 않는다", "넘었다"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := JudgeConstraintCode(c.code)
			if v.Kind != "" || v.Fault != c.fault {
				t.Fatalf("(Kind, Fault) = (%q, %q), 기대 (\"\", %q): %s", v.Kind, v.Fault, c.fault, v.Reason)
			}
			if !strings.Contains(v.Reason, c.inReason) || (c.notReason != "" && strings.Contains(v.Reason, c.notReason)) {
				t.Fatalf("사유가 다르다(있어야 할 것 %q, 없어야 할 것 %q): %s", c.inReason, c.notReason, v.Reason)
			}
		})
	}
}

// 표 밖 케이스: 사유는 **어떤 코드에도** 비면 안 된다.
// 사유가 없으면 "접지 않기로 했다"와 "이 축을 아예 안 본다"가 구분되지 않는다.
func TestJudgeConstraintCodeAlwaysGivesReason(t *testing.T) {
	for code := 0; code <= 3200; code++ {
		v := JudgeConstraintCode(code)
		if strings.TrimSpace(v.Reason) == "" {
			t.Fatalf("코드 %d 의 사유가 비었다", code)
		}
		switch v.Kind {
		case "", ConflictDuplicate, ConflictMissingRef:
		default:
			t.Fatalf("코드 %d 가 모르는 종류를 냈다: %q", code, v.Kind)
		}
	}
}

// ConstraintCode 는 **감싼 오류 안에서도** 코드를 찾아야 한다.
// 저장 계층이 fmt.Errorf 로 문맥을 덧붙이므로 여기가 끊기면 전부 500 으로 돌아간다.
func TestConstraintCodeFindsWrappedDriverError(t *testing.T) {
	if got := ConstraintCode(errors.New("드라이버와 무관한 오류")); got != 0 {
		t.Fatalf("드라이버 오류가 아닌데 코드 %d 를 냈다", got)
	}
}

// JudgeSQLState 는 Postgres 의 SQLSTATE 를 SQLite 와 **같은 축**으로 판정한다(실물은 pg_tx_test.go).
func TestJudgeSQLState(t *testing.T) {
	for _, c := range []struct {
		name, state  string
		wantKind     ConflictKind
		wantFault    FaultKind
		wantInReason string
	}{
		{"PK · UNIQUE 중복", "23505", ConflictDuplicate, "", "이미 있다"},
		{"FK", "23503", ConflictMissingRef, "", "등록돼 있지 않다"},
		{"CHECK 는 안 접는다", "23514", "", "", "1차 방어"},
		{"NOT NULL 은 안 접는다", "23502", "", "", "1차 방어"},
		{"추가 전용 트리거는 안 접는다", "23000", "", "", "1차 방어"},
		{"모르는 제약 축", "23P01", "", "", "아는 축이 아닌"},
		{"잠금 대기 초과", "55P03", "", FaultLockUnavailable, "lock_timeout"},
		{"직렬화 실패", "40001", "", FaultSerialization, "트랜잭션 전체"},
		{"교착", "40P01", "", FaultDeadlock, "엇갈리면"},
		{"버려진 트랜잭션", "25P02", "", FaultAborted, "Savepoint"},
		{"제약이 아님", "42601", "", "", "제약 위반이 아니다"},
		{"코드 없음", "", "", "", "SQLSTATE 가 없다"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := JudgeSQLState(c.state)
			if got.Kind != c.wantKind || got.Fault != c.wantFault {
				t.Errorf("(Kind, Fault) = (%q, %q), want (%q, %q) (사유: %s)", got.Kind, got.Fault, c.wantKind, c.wantFault, got.Reason)
			}
			if !strings.Contains(got.Reason, c.wantInReason) {
				t.Errorf("사유에 %q 가 없다: %q", c.wantInReason, got.Reason)
			}
		})
	}
}

// asFault 는 트랜잭션 실패만 *TxFault 로 옮기고, 감싼 오류 안에서도 찾고, 두 번 감싸지 않는다.
func TestAsFaultWrapsOnlyTxFaults(t *testing.T) {
	if asFault(nil) != nil {
		t.Fatal("nil 을 오류로 바꿨다")
	}
	plain := errors.New("드라이버와 무관한 오류")
	if got := asFault(plain); got != plain {
		t.Fatalf("트랜잭션 실패가 아닌 오류를 바꿨다: %v", got)
	}
	dup := fmt.Errorf("쓰기: %w", &pgconn.PgError{Code: "23505", ConstraintName: "project_pkey"})
	if got := asFault(dup); got != dup {
		t.Fatalf("제약 위반을 트랜잭션 실패로 옮겼다: %v", got)
	}
	for _, c := range []struct {
		err  error
		kind FaultKind
		code string
	}{
		{fmt.Errorf("문맥: %w", &pgconn.PgError{Code: "55P03"}), FaultLockUnavailable, "55P03"},
		{fmt.Errorf("문맥: %w", &pgconn.PgError{Code: "40P01"}), FaultDeadlock, "40P01"},
		{fmt.Errorf("문맥: %w", &pgconn.PgError{Code: "40001"}), FaultSerialization, "40001"},
		{fmt.Errorf("커밋 실패: %w", pgx.ErrTxCommitRollback), FaultAborted, "25P02"},
	} {
		got := asFault(c.err)
		var f *TxFault
		if !errors.As(got, &f) || f.Kind != c.kind || f.Code != c.code || !errors.Is(got, c.err) {
			t.Fatalf("%v → %v(기대 TxFault %s/%s, 원문 사슬 유지)", c.err, got, c.kind, c.code)
		}
		if again := asFault(got); again != got {
			t.Fatalf("두 번 감쌌다: %v", again)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ② 실물 드라이버
// ─────────────────────────────────────────────────────────────────────────────

// 항목 id 중복은 **타입 있는 오류**로 올라온다.
// 이것이 없으면 표면이 500 을 내고, 500 은 멱등 표에 안 남아 재시도가 계속 하류로 들어간다.
func TestDuplicateItemIDIsTypedConflict(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seed(t, s, "p")

	it := model.Item{Project: "p", ID: "t9-dup", Title: "제목", Body: "본문"}
	// ── 대조 전제: 첫 등록이 정말 성공했는가 ──
	if err := s.AddItem(ctx, it); err != nil {
		t.Fatalf("전제가 깨졌다 — 첫 등록이 실패했다: %v", err)
	}

	err := s.AddItem(ctx, it)
	if err == nil {
		t.Fatal("같은 id 를 두 번 넣었는데 성공했다 — PK 가 안 물고 있다")
	}
	var c *ConflictError
	if !errors.As(err, &c) {
		t.Fatalf("제약 위반이 타입 있는 오류로 안 올라왔다: %T %v", err, err)
	}
	if c.Kind != ConflictDuplicate {
		t.Errorf("Kind = %q, want %q (사유: %s)", c.Kind, ConflictDuplicate, c.Reason)
	}
	if c.Target != TargetItem || c.ID != "t9-dup" || c.Project != "p" {
		t.Errorf("좌표가 안 실렸다: target=%q project=%q id=%q", c.Target, c.Project, c.ID)
	}
	if c.Code != sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY {
		t.Errorf("확장 결과 코드가 %d 다 — 실물 드라이버가 %d 를 낸다고 기대했다",
			c.Code, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
	}
	// 원문은 물려 있어야 한다(로그가 유일한 원천이다).
	if c.Unwrap() == nil {
		t.Error("드라이버 원문이 안 물려 있다 — 로그에 원인 전문을 실을 수 없다")
	}
}

// 등록 안 된 프로젝트로 판단을 쓰면 FK 위반이다. **중복과 처방이 정반대**라 축을 가른다.
func TestJudgmentToUnknownProjectIsMissingRef(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seed(t, s, "p")

	// ── 대조 전제: 등록된 프로젝트로는 통과하는가 ──
	if _, err := s.AddJudgment(ctx, model.Judgment{
		Project: "p", Kind: model.JudgmentDecision, Body: "대조"}); err != nil {
		t.Fatalf("전제가 깨졌다 — 등록된 프로젝트에도 판단이 안 들어간다: %v", err)
	}

	_, err := s.AddJudgment(ctx, model.Judgment{
		Project: "등록안된프로젝트", Kind: model.JudgmentDecision, Body: "본문"})
	if err == nil {
		t.Fatal("없는 프로젝트로 판단이 들어갔다 — FK 가 안 물고 있다")
	}
	var c *ConflictError
	if !errors.As(err, &c) {
		t.Fatalf("FK 위반이 타입 있는 오류로 안 올라왔다: %T %v", err, err)
	}
	if c.Kind != ConflictMissingRef {
		t.Errorf("Kind = %q, want %q (사유: %s)", c.Kind, ConflictMissingRef, c.Reason)
	}
	if !strings.Contains(c.RefHint, "등록안된프로젝트") {
		t.Errorf("무엇을 가리켰는지가 안 실렸다: %q", c.RefHint)
	}
}

// 등록 안 된 세션으로 선점해도 같은 형태다. 같은 결함이 여러 자리에 있었다.
func TestClaimWithUnknownSessionIsMissingRef(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seed(t, s, "p")
	if err := s.AddItem(ctx, model.Item{Project: "p", ID: "t9-c", Title: "t", Body: "b"}); err != nil {
		t.Fatalf("전제가 깨졌다 — 항목 등록 실패: %v", err)
	}

	_, err := s.ClaimItem(ctx, "p", "t9-c", "없는세션", time.Time{})
	if err == nil {
		t.Fatal("없는 세션이 선점에 성공했다 — FK 가 안 물고 있다")
	}
	var c *ConflictError
	if !errors.As(err, &c) {
		t.Fatalf("FK 위반이 타입 있는 오류로 안 올라왔다: %T %v", err, err)
	}
	if c.Kind != ConflictMissingRef || c.Target != TargetClaim {
		t.Errorf("판정이 다르다: kind=%q target=%q (사유: %s)", c.Kind, c.Target, c.Reason)
	}
}

// ★ CHECK 위반은 **접지 않는다.** 실물 드라이버가 내는 오류로 확인한다 —
// 순수 함수만 단정하면 "우리가 정한 숫자를 우리가 확인"하는 것이 된다.
func TestCheckViolationIsNotFoldedToConflict(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seed(t, s, "p")

	var raw error
	if err := s.Tx(ctx, func(t2 *Tx) error {
		// state='dropped' 인데 사유가 없다 → 스키마 CHECK 가 문다.
		// 정상 경로는 ValidateFinish 가 앞에서 막으므로 여기까지 오는 것은 서버 결함이다.
		_, raw = t2.tx.ExecContext(t2.ctx, `
			INSERT INTO item(project,id,title,body,paths,labels,state,close_reason,created_at)
			VALUES ('p','t9-chk','t','b','[]','[]','dropped',NULL,'2026-01-01T00:00:00.000000Z')`)
		return nil
	}); err != nil {
		t.Fatalf("트랜잭션 실패: %v", err)
	}
	// ── 대조 전제: CHECK 가 실제로 물었는가 ──
	if raw == nil {
		t.Fatal("전제가 깨졌다 — 사유 없는 dropped 가 들어갔다(CHECK 가 안 문다)")
	}
	if got := ConstraintCode(raw); got != sqlite3.SQLITE_CONSTRAINT_CHECK {
		t.Fatalf("전제가 깨졌다 — 코드가 %d 다(CHECK %d 를 기대했다)",
			got, sqlite3.SQLITE_CONSTRAINT_CHECK)
	}

	// ── 본 판정 ──
	if c := conflictOf(raw, TargetItem, "p", "t9-chk", ""); c != nil {
		t.Fatalf("CHECK 위반이 4xx 로 접혔다 — 서버 결함이 '사용자 잘못'으로 분류된다: %v", c)
	}
}

// writeErr 는 제약 위반이 아닌 오류를 **원문 그대로 감싸** 올려야 한다.
// 여기가 끊기면 무관한 하류 오류가 409 로 둔갑한다.
func TestWriteErrPassesThroughNonConstraintErrors(t *testing.T) {
	base := errors.New("디스크가 가득 찼다")
	got := writeErr(base, writeTarget{Target: TargetItem, Project: "p", ID: "i"}, "항목 등록 실패(%s)", "i")
	var c *ConflictError
	if errors.As(got, &c) {
		t.Fatalf("제약 위반이 아닌데 접혔다: %v", got)
	}
	if !errors.Is(got, base) {
		t.Fatalf("원문이 안 물려 있다: %v", got)
	}
	if !strings.Contains(got.Error(), "항목 등록 실패") {
		t.Fatalf("문맥이 안 붙었다: %v", got)
	}
	if writeErr(nil, writeTarget{Target: TargetItem}, "무시") != nil {
		t.Fatal("nil 오류를 감쌌다")
	}
}

// TestItemRevisionIsAppendOnly 는 개정 이력이 고쳐지거나 지워지지 않는지 본다.
//
// 이 표의 존재 이유가 「본문을 제자리에서 고치는 대신 옛 값이 사라지지 않는다」는
// 보장 하나다. 그 보장이 트리거 없이 주석으로만 있으면, 고치는 코드가 생기는 날
// 아무도 그것을 못 본다 — judgment 가 같은 이유로 같은 트리거를 갖는다.
func TestItemRevisionIsAppendOnly(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seed(t, s, "p1")
	mustItem(t, s, "p1", "i1")

	err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.tx.ExecContext(ctx,
			`INSERT INTO item_revision(project, item_id, rev, at, session_id, title, body, paths, reason)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			"p1", "i1", 1, "2026-09-16T00:00:00Z", nil, "옛 제목", "옛 본문", "[]", "오타")
		return err
	})
	if err != nil {
		t.Fatalf("개정 행을 못 넣었다: %v", err)
	}

	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{"UPDATE", `UPDATE item_revision SET body = ? WHERE project = ? AND item_id = ? AND rev = ?`,
			[]any{"덮어씀", "p1", "i1", 1}},
		{"DELETE", `DELETE FROM item_revision WHERE project = ? AND item_id = ? AND rev = ?`,
			[]any{"p1", "i1", 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Tx(ctx, func(tx *Tx) error {
				_, e := tx.tx.ExecContext(ctx, tc.sql, tc.args...)
				return e
			})
			if err == nil {
				t.Fatalf("%s 가 통과했다 — 개정 이력은 추가 전용이어야 한다", tc.name)
			}
			if !strings.Contains(err.Error(), "추가 전용") {
				t.Errorf("거절은 됐는데 사유가 이 트리거의 것이 아니다: %v\n"+
					"다른 제약이 잡은 것이면 이 시험은 재려던 것을 안 재고 있다", err)
			}
		})
	}
}

// TestItemRevisionRequiresReason 은 사유 없는 개정을 CHECK 가 막는지 본다.
func TestItemRevisionRequiresReason(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seed(t, s, "p1")
	mustItem(t, s, "p1", "i1")

	err := s.Tx(ctx, func(tx *Tx) error {
		_, e := tx.tx.ExecContext(ctx,
			`INSERT INTO item_revision(project, item_id, rev, at, session_id, title, body, paths, reason)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			"p1", "i1", 1, "2026-09-16T00:00:00Z", nil, "t", "b", "[]", "")
		return e
	})
	if err == nil {
		t.Fatal("빈 사유가 통과했다 — 되짚을 수 없는 개정을 남기게 된다")
	}
}
