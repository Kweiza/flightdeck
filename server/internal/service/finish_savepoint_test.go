package service

import (
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store"
)

// 트랜잭션 안에서 DB 오류를 받고 이어 가는 자리를 실제 호출 경로(Finish · Land)로 두 엔진에서 잰다. Postgres 는 실패한
// 문장 뒤의 트랜잭션을 통째로 버려(25P02) 저장점 없이 오류를 삼키면 커밋이 안 되고, SQLite 는 지금 운영 길이라 동작이
// 그대로여야 한다. 저장층 단위 시험은 store/savepoint_test.go 에 있다.

// ledgerEngine 은 같은 시험을 돌 원장 하나다.
type ledgerEngine struct {
	name string
	open func(t *testing.T, opts ...Option) (*Service, *store.Store)
}

func bothLedgers() []ledgerEngine {
	return []ledgerEngine{
		{"sqlite", newSvc},
		{"postgres", newPGSvc},
	}
}

// newPGSvc 는 Postgres 시험 원장 위의 서비스다. opts 는 New 에 그대로 넘긴다.
func newPGSvc(t *testing.T, opts ...Option) (*Service, *store.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, _ := openPGLedger(t, log)
	return New(st, log, opts...), st
}

// rawExec 는 시험 준비 문장을 원장에 바로 친다. 자리표시가 엔진마다 달라 인자 없는 문장만 받는다.
func rawExec(t *testing.T, st *store.Store, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := st.DB().Exec(q); err != nil {
			t.Errorf("시험 준비 문장 실패: %v\n%s", err, q)
		}
	}
}

// armDuplicateFollowup 은 id 가 first-new 인 항목이 들어가는 순간 같은 트랜잭션 안에서 id 가 dup-next 인 항목을 만드는
// 트리거를 건다. 분류(트랜잭션 밖)와 존재 선검사를 다 지난 뒤에 뒤 후속의 등록이 중복을 만나는 갈래를 결정론적으로
// 고른다 - 운영에서는 전역 쓰기 잠금 때문에 거의 안 밟히지만, 밟히면 판단을 잃던 자리다.
func armDuplicateFollowup(t *testing.T, engine string, st *store.Store) {
	t.Helper()
	const insert = `INSERT INTO item(project, id, title, body, created_at)
		VALUES (NEW.project, 'dup-next', '먼저 생긴 항목', '본문', '2026-01-01T00:00:00.000000Z')`
	stmts := []string{`CREATE TRIGGER fd_test_dup_next AFTER INSERT ON item WHEN NEW.id = 'first-new' BEGIN ` +
		insert + `; END`}
	if engine == "postgres" {
		stmts = []string{
			`CREATE FUNCTION fd_test_dup_next() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				IF NEW.id = 'first-new' THEN ` + insert + `; END IF; RETURN NEW; END $$`,
			`CREATE TRIGGER fd_test_dup_next AFTER INSERT ON item FOR EACH ROW EXECUTE FUNCTION fd_test_dup_next()`,
		}
	}
	rawExec(t, st, stmts...)
}

// TestFinishKeepsTheJudgmentWhenAFollowupTurnsOutDuplicate 는 후속 등록이 트랜잭션 안에서 중복을 만나도 그 후속만
// 건너뛰고 판단 · 닫기 · 건너뜀 기록이 함께 커밋되는지, 그리고 **판단이 건너뛴 후속을 가리키지 않는지** 잰다.
// Postgres 에서 저장점 없던 앞 판은 중복 오류가 트랜잭션을 버려 커밋이 25P02 로 실패했고 판단까지 사라졌다. 판단을
// 후속보다 먼저 쓰던 판은 건너뛴 후속(먼저 있던 남의 항목)에도 링크를 남겼다.
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
				Followups: []FollowupInput{
					{ID: "first-new", Title: "앞 후속", Body: "앞 후속 본문"},
					{ID: "dup-next", Title: "뒤 후속", Body: "뒤 후속 본문"},
				},
			})
			if err != nil {
				t.Fatalf("중복 후속 하나가 마무리 전체를 죽였다: %v", err)
			}
			if !slices.Equal(res.SkippedFollowups, []string{"dup-next"}) {
				t.Fatalf("건너뛴 후속이 %v 다(기대 [dup-next])", res.SkippedFollowups)
			}
			if len(res.Followups) != 1 || res.Followups[0].ID != "first-new" {
				t.Fatalf("만든 후속이 %+v 다(기대 first-new 하나)", res.Followups)
			}
			if n := countRows(t, st, `SELECT count(*) FROM judgment WHERE title = '끝냈다'`); n != 1 {
				t.Fatalf("판단이 %d건이다(기대 1) - 중복 흡수가 판단을 잃었다", n)
			}
			// ★ 심장. 판단을 후속보다 먼저 쓰거나 링크를 「만들려 한 것」으로 짜면 여기가 1이 된다.
			if n := countRows(t, st,
				`SELECT count(*) FROM judgment_link WHERE target_kind = 'item' AND target_id = 'dup-next'`); n != 0 {
				t.Fatalf("건너뛴 후속에 판단 링크가 %d건 걸렸다 - 먼저 있던 남의 항목을 이 마무리의 후속으로 가리킨다", n)
			}
			if n := countRows(t, st,
				`SELECT count(*) FROM judgment_link WHERE target_kind = 'item' AND target_id = 'first-new'`); n != 1 {
				t.Fatalf("만든 후속의 판단 링크가 %d건이다(기대 1)", n)
			}
			if n := countRows(t, st, `SELECT count(*) FROM item WHERE id = 'batch7' AND state = 'done'`); n != 1 {
				t.Fatal("끝낸 항목이 닫히지 않았다")
			}
			if n := countRows(t, st, `SELECT count(*) FROM item WHERE id = 'dup-next' AND title = '먼저 생긴 항목'`); n != 1 {
				t.Fatal("먼저 생긴 항목이 후속으로 덮였거나 사라졌다")
			}
			if n := countRows(t, st, `SELECT count(*) FROM event WHERE kind = 'item.followup_skipped'
				AND payload LIKE '%dup-next%' AND payload LIKE '%같은 id 의 항목이 이미 있다%'`); n != 1 {
				t.Fatalf("중복으로 건너뛴 사유가 원장에 %d건이다(기대 1)", n)
			}
		})
	}
}

// TestFinishRaisesAFollowupReadFailure 는 트랜잭션 안의 후속 읽기(만들 것의 존재 선검사 · 이을 것의 조회)가 저장층
// 실패를 받으면 그 후속만 건너뛰지 않고 마무리 전체를 실패로 올리는지 잰다. 건너뛰면 「못 만들었다 · 못 이었다」가 원인
// 없이 추가 전용 원장에 굳는다 - 다시 보내면 풀리는 실패인데 영구 누락이 된다.
//
// ★ 실패는 스키마 어긋남(item_after 칸 이름 바꾸기)으로 낸다. 트랜잭션 안에서 item_after 를 읽는 것이 이 두 읽기뿐이라
// (후속 등록은 선행이 없으면 item_after 를 안 쓰고, 닫기 · 판단 · 반납은 그 표를 안 본다) 흡수하던 판은 실패 없이
// 커밋까지 간다. 창은 시계 후크로 벌린다(TestFinishSkipsALinkTargetThatClosedBetweenClassifyAndTx 와 같은 자리) -
// 분류는 이미 끝났으므로 late-next 는 「만들 것」으로, kept-axis 는 「이을 것」으로 정해진 뒤다.
func TestFinishRaisesAFollowupReadFailure(t *testing.T) {
	for _, e := range bothLedgers() {
		for _, c := range []struct {
			name     string
			followup FollowupInput
		}{
			{"만들 후속의 존재 선검사", FollowupInput{ID: "late-next", Title: "후속", Body: "후속 본문"}},
			{"이을 후속의 조회", FollowupInput{ID: "kept-axis"}},
		} {
			t.Run(e.name+"/"+c.name, func(t *testing.T) {
				var arm func()
				s, st := e.open(t, WithClock(func() time.Time {
					if arm != nil {
						f := arm
						arm = nil
						f()
					}
					return time.Now()
				}))
				repo, wt := newRepoWithWorktree(t, "feat")
				me := openSession(t, s, "p", repo, wt, "cc-1", "트랙2")
				addItem(t, s, "p", "batch7", nil, nil)
				claimed(t, s, "p", me.Session.ID, "batch7")
				addItemAs(t, s, "p", me.Session.ID, "kept-axis") // 분류 시점에 이을 자격이 있다

				arm = func() {
					rawExec(t, st,
						`INSERT INTO item(project, id, title, body, created_at)
							VALUES ('p', 'late-next', '나중에 생긴 항목', '본문', '2026-01-01T00:00:00.000000Z')`,
						`ALTER TABLE item_after RENAME COLUMN dep_sha TO dep_sha_gone`)
				}

				_, err := s.Finish(ctx(), FinishInput{
					Project: "p", SessionID: me.Session.ID, ItemID: "batch7",
					Outcome: model.ItemDone, Title: "끝냈다", Body: "판단 본문",
					Followups: []FollowupInput{c.followup},
				})
				if arm != nil {
					t.Fatal("시계 후크가 안 불렸다 - 창이 안 벌어졌으니 이 시험은 아무것도 안 잠근다")
				}
				if err == nil || !store.Unabsorbable(err) {
					t.Fatalf("저장층 실패를 받은 후속 읽기가 위로 안 올라왔다: %v", err)
				}
				if n := countRows(t, st, `SELECT count(*) FROM judgment`); n != 0 {
					t.Fatalf("판단이 %d건 커밋됐다 - 읽기 실패를 흡수하고 이어 갔다", n)
				}
				if n := countRows(t, st, `SELECT count(*) FROM event WHERE kind = 'item.followup_skipped'`); n != 0 {
					t.Fatalf("건너뜀이 %d건 남았다 - 저장층 실패가 후속의 사유로 굳었다", n)
				}
				if n := countRows(t, st, `SELECT count(*) FROM item WHERE id = 'batch7' AND state = 'done'`); n != 0 {
					t.Fatal("실패한 마무리가 항목을 닫았다")
				}
			})
		}
	}
}

// TestLandReentryAndWaitingKeepTheTx 는 이미 줄에 선 세션의 재진입과 남이 쥔 레인 앞에서의 대기가 각각 한 트랜잭션
// 안에서 커밋까지 가는지 잰다. 대기는 막힌 자원의 점유자를 내야 한다. Postgres 에서 앞 판은 첫 줄 서기부터
// LastInsertId(Postgres 드라이버가 지원하지 않는다)에서 죽어 재진입 · 대기까지 가지도 못했다. 그것을 넘었어도 막힌
// 삽입 뒤의 조회는 버려진 트랜잭션(25P02)에 닿는 자리였다.
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
