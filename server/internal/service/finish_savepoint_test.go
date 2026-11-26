package service

import (
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store"
)

// 트랜잭션 안에서 DB 오류를 받고 이어 가는 자리를 실제 호출 경로(Finish · Land)로 두 엔진에서 잰다. Postgres 는 실패한
// 문장 뒤의 트랜잭션을 통째로 버려(25P02) 저장점 없이 오류를 삼키면 커밋이 안 되고, SQLite 는 지금 운영 길이라 동작이
// 그대로여야 한다. 저장층 단위 시험은 store/savepoint_test.go 에 있다.

// ledgerEngine 은 같은 시험을 돌 원장 하나다.
type ledgerEngine struct {
	name string
	open func(t *testing.T) (*Service, *store.Store)
}

func bothLedgers() []ledgerEngine {
	return []ledgerEngine{
		{"sqlite", newSvc},
		{"postgres", newPGSvc},
	}
}

// newPGSvc 는 Postgres 시험 원장 위의 서비스다.
//
// ★ 임시 받침: item.go 의 afterOf 가 아직 SQLite 의 rowid 로 정렬해 Postgres 에서 42703 으로 죽는다(Postgres 판의
// item_after 는 같은 자리를 seq 칸으로 둔다). 업무 SQL 이식은 이 시험의 몫이 아니라서, 시험 DB 에서만 칸 이름을
// 맞춰 항목 경로를 지나게 한다. afterOf 가 이식되면 이 받침을 지운다.
func newPGSvc(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, _ := openPGLedger(t, log)
	if _, err := st.DB().Exec(`ALTER TABLE item_after RENAME COLUMN seq TO rowid`); err != nil {
		t.Fatalf("시험 받침 실패: %v", err)
	}
	return New(st, log), st
}

// armDuplicateFollowup 은 판단이 들어가는 순간 같은 트랜잭션 안에서 id 가 dup-next 인 항목을 만드는 트리거를 건다.
// 분류(트랜잭션 밖)와 존재 선검사(판단보다 앞)를 다 지난 뒤에 후속 등록이 중복을 만나는 갈래를 결정론적으로 고른다 -
// 운영에서는 전역 쓰기 잠금 때문에 거의 안 밟히지만, 밟히면 판단을 잃던 자리다.
func armDuplicateFollowup(t *testing.T, engine string, st *store.Store) {
	t.Helper()
	const insert = `INSERT INTO item(project, id, title, body, created_at)
		VALUES (NEW.project, 'dup-next', '먼저 생긴 항목', '본문', '2026-01-01T00:00:00.000000Z')`
	stmts := []string{`CREATE TRIGGER fd_test_dup_next AFTER INSERT ON judgment BEGIN ` + insert + `; END`}
	if engine == "postgres" {
		stmts = []string{
			`CREATE FUNCTION fd_test_dup_next() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN ` + insert + `; RETURN NEW; END $$`,
			`CREATE TRIGGER fd_test_dup_next AFTER INSERT ON judgment FOR EACH ROW EXECUTE FUNCTION fd_test_dup_next()`,
		}
	}
	for _, q := range stmts {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatalf("트리거 걸기 실패: %v\n%s", err, q)
		}
	}
}

// TestFinishKeepsTheJudgmentWhenAFollowupTurnsOutDuplicate 는 후속 등록이 트랜잭션 안에서 중복을 만나도 그 후속만
// 건너뛰고 판단 · 닫기 · 건너뜀 기록이 함께 커밋되는지 잰다. Postgres 에서 저장점 없던 앞 판은 중복 오류가 트랜잭션을
// 버려 커밋이 25P02 로 실패했고 판단까지 사라졌다.
func TestFinishKeepsTheJudgmentWhenAFollowupTurnsOutDuplicate(t *testing.T) {
	for _, e := range bothLedgers() {
		t.Run(e.name, func(t *testing.T) {
			s, st := e.open(t)
			repo, wt := newRepoWithWorktree(t, "feat")
			a := openSession(t, s, "p", repo, wt, "cc-a", "트랙A")
			addItem(t, s, "p", "batch7", nil, nil)
			claimed(t, s, "p", a.Session.ID, "batch7")
			armDuplicateFollowup(t, e.name, st)

			res, err := s.Finish(ctx(), FinishInput{
				Project: "p", SessionID: a.Session.ID, ItemID: "batch7",
				Outcome: model.ItemDone, Title: "끝냈다", Body: "판단 본문",
				Followups: []FollowupInput{{ID: "dup-next", Title: "후속", Body: "후속 본문"}},
			})
			if err != nil {
				t.Fatalf("중복 후속 하나가 마무리 전체를 죽였다: %v", err)
			}
			if !slices.Equal(res.SkippedFollowups, []string{"dup-next"}) {
				t.Fatalf("건너뛴 후속이 %v 다(기대 [dup-next])", res.SkippedFollowups)
			}
			if n := countRows(t, st, `SELECT count(*) FROM judgment WHERE title = '끝냈다'`); n != 1 {
				t.Fatalf("판단이 %d건이다(기대 1) - 중복 흡수가 판단을 잃었다", n)
			}
			if n := countRows(t, st, `SELECT count(*) FROM item WHERE id = 'batch7' AND state = 'done'`); n != 1 {
				t.Fatal("끝낸 항목이 닫히지 않았다")
			}
			if n := countRows(t, st, `SELECT count(*) FROM item WHERE id = 'dup-next' AND title = '먼저 생긴 항목'`); n != 1 {
				t.Fatal("먼저 생긴 항목이 후속으로 덮였거나 사라졌다")
			}
			if n := countRows(t, st,
				`SELECT count(*) FROM event WHERE kind = 'item.followup_skipped' AND payload LIKE '%dup-next%'`); n != 1 {
				t.Fatalf("건너뜀 기록이 %d건이다(기대 1)", n)
			}
		})
	}
}

// TestLandReentryAndWaitingKeepTheTx 는 이미 줄에 선 세션의 재진입과 남이 쥔 레인 앞에서의 대기가 각각 한 트랜잭션
// 안에서 커밋까지 가는지 잰다. 대기는 막힌 자원의 점유자를 내야 한다. Postgres 에서 앞 판은 막힌 삽입 뒤 조회가
// 25P02 로 죽어 재진입도 대기도 일반 오류로 끝났다.
func TestLandReentryAndWaitingKeepTheTx(t *testing.T) {
	for _, e := range bothLedgers() {
		t.Run(e.name, func(t *testing.T) {
			s, st := e.open(t)
			me, other := twoSessions(t, s)

			first, err := s.Land(ctx(), LandInput{Project: "p", SessionID: me})
			if err != nil || first.State != "turn" {
				t.Fatalf("빈 레인인데 %+v, %v", first, err)
			}
			again, err := s.Land(ctx(), LandInput{Project: "p", SessionID: me})
			if err != nil {
				t.Fatalf("재진입이 실패했다: %v", err)
			}
			if again.State != "turn" || again.RowID != first.RowID {
				t.Fatalf("재진입이 %s · 행 %d 를 냈다(기대 turn · 행 %d)", again.State, again.RowID, first.RowID)
			}

			wait, err := s.Land(ctx(), LandInput{Project: "p", SessionID: other})
			if err != nil {
				t.Fatalf("남이 쥔 레인 앞에서 실패했다: %v", err)
			}
			if wait.State != "waiting" || wait.Holder == nil || wait.Holder.SessionID != me {
				t.Fatalf("대기 응답 %+v(기대 waiting · 점유자 %s)", wait, me)
			}
			if n := liveQueue(t, st); n != 2 {
				t.Fatalf("살아 있는 줄 행이 %d개다(기대 2 - 둘 다 줄에 남는다)", n)
			}
		})
	}
}
