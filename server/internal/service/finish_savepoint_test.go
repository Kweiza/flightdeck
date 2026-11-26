package service

import (
	"errors"
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

// armJudgmentFailure 는 판단이 들어가는 문장을 실패시키는 트리거를 건다. 돌려준 함수가 걷는다 - 원인이 사라진 뒤의
// 재시도를 재려는 것이다.
func armJudgmentFailure(t *testing.T, engine string, st *store.Store) (disarm func()) {
	t.Helper()
	if engine == "postgres" {
		rawExec(t, st,
			`CREATE FUNCTION fd_test_fail_judgment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				RAISE EXCEPTION 'fd test: 판단 저장 실패'; END $$`,
			`CREATE TRIGGER fd_test_fail_judgment BEFORE INSERT ON judgment FOR EACH ROW EXECUTE FUNCTION fd_test_fail_judgment()`)
		return func() { rawExec(t, st, `DROP TRIGGER fd_test_fail_judgment ON judgment`) }
	}
	rawExec(t, st, `CREATE TRIGGER fd_test_fail_judgment BEFORE INSERT ON judgment
		BEGIN SELECT RAISE(ABORT, 'fd test: 판단 저장 실패'); END`)
	return func() { rawExec(t, st, `DROP TRIGGER fd_test_fail_judgment`) }
}

// TestFinishDoesNotAdoptAnItemWhoseCreationRolledBack 은 이 세션의 생성이 롤백으로 사라진 id 를 남이 만든 뒤, 그 남의
// 항목이 이 세션의 후속 자격(이을 수 있는 것 · 관문이 보는 것)에 안 드는지 실제 경로로 잰다. 생성 이벤트는 롤백 뒤에도
// 원장에 남으므로, 결말 표시를 안 보던 판은 남의 항목에 이 마무리의 판단을 걸었다.
//
//	마무리 재시도: 후속을 만든 뒤 판단 저장이 실패해 롤백된다(item.followup_created 가 롤백 표시로 남는다) → 남이 같은
//	               id 를 만든다 → 같은 마무리를 다시 보낸다.
//	등록 실패:     남이 먼저 만든 id 를 이 세션이 add 하다 중복으로 롤백된다(item.add 가 롤백 표시로 남는다) → 그 id 를
//	               후속으로 싣는다.
//	표시 없는 옛 실패: 표시가 생기기 전 코드도 생성 이벤트를 삽입 전에 예약하고 롤백 뒤에 흘렸다. 그 행은 결말 표시 없이
//	               남는다 → 선점이 업그레이드를 넘어 이어지면 그 id 의 남의 항목을 후속으로 싣는다. 표시를 안 붙이는
//	               Store.LogEvent 로 그 행을 그대로 심는다(두 kind 각각).
func TestFinishDoesNotAdoptAnItemWhoseCreationRolledBack(t *testing.T) {
	const (
		rolledBack = `AND payload LIKE '%"tx":"rolled_back"%'`
		unmarked   = `AND payload NOT LIKE '%"tx":%'`
	)
	// plantUnmarked 는 남이 gone-x 를 만든 뒤, 이 세션이 그 id 를 만들려다 실패한 옛 행(결말 표시 없음)을 심는다.
	plantUnmarked := func(kind string) func(t *testing.T, s *Service, st *store.Store, in FinishInput, other string) {
		return func(t *testing.T, s *Service, st *store.Store, in FinishInput, other string) {
			addItemAs(t, s, "p", other, "gone-x")
			st.LogEvent(ctx(), kind, "p", in.SessionID, map[string]any{"item": "gone-x"})
		}
	}
	for _, e := range bothLedgers() {
		for _, c := range []struct {
			name  string
			kind  string
			mark  string // 전제: 이 세션의 gone-x 생성 이벤트가 어떤 결말 표시로 남았나
			spoil func(t *testing.T, s *Service, st *store.Store, in FinishInput, other string)
		}{
			{"마무리 재시도", followupCreatedEvent, rolledBack, func(t *testing.T, s *Service, st *store.Store, in FinishInput, other string) {
				disarm := armJudgmentFailure(t, e.name, st)
				if _, err := s.Finish(ctx(), in); err == nil {
					t.Fatal("판단 저장이 실패했는데 마무리가 성공했다 - 시험 준비가 안 걸렸다")
				}
				disarm()
				if n := countRows(t, st, `SELECT count(*) FROM item WHERE id = 'gone-x'`); n != 0 {
					t.Fatalf("롤백된 마무리의 후속이 %d건 남았다", n)
				}
				addItemAs(t, s, "p", other, "gone-x")
			}},
			{"등록 실패", "item.add", rolledBack, func(t *testing.T, s *Service, st *store.Store, in FinishInput, other string) {
				addItemAs(t, s, "p", other, "gone-x")
				if _, err := s.AddItem(ctx(), AddItemInput{Project: "p", SessionID: in.SessionID, ID: "gone-x",
					Title: "내 항목", Body: "본문"}); err == nil {
					t.Fatal("남이 먼저 만든 id 를 다시 만들었다 - 시험 준비가 안 걸렸다")
				}
			}},
			{"표시 없는 옛 add", "item.add", unmarked, plantUnmarked("item.add")},
			{"표시 없는 옛 후속 생성", followupCreatedEvent, unmarked, plantUnmarked(followupCreatedEvent)},
		} {
			t.Run(e.name+"/"+c.name, func(t *testing.T) {
				s, st := e.open(t)
				repo, wt := newRepoWithWorktree(t, "feat")
				me := openSession(t, s, "p", repo, wt, "cc-a", "트랙A")
				dirB := tmpBase(t)
				other := openSession(t, s, "p", dirB, dirB, "cc-b", "트랙B")
				addItem(t, s, "p", "batch7", nil, nil)
				claimed(t, s, "p", me.Session.ID, "batch7")

				in := FinishInput{
					Project: "p", SessionID: me.Session.ID, ItemID: "batch7",
					Outcome: model.ItemDone, Title: "끝냈다", Body: "판단 본문",
					Followups: []FollowupInput{{ID: "gone-x", Title: "후속", Body: "후속 본문"}},
				}
				c.spoil(t, s, st, in, other.Session.ID)
				// 전제: 이 세션의 생성 이벤트가 그 결말 표시로 남았고(롤백 · 표시 없음), 지금 그 id 는 남의 열린 항목이다.
				if n := countRows(t, st, `SELECT count(*) FROM event WHERE kind = '`+c.kind+`'
					AND session_id = '`+me.Session.ID+`' AND payload LIKE '%gone-x%' `+c.mark); n != 1 {
					t.Fatalf("이 세션의 실패한 생성 이벤트(%s, %s)가 %d건이다(기대 1) - 시험이 그 갈래를 못 밟았다", c.kind, c.mark, n)
				}
				if n := countRows(t, st, `SELECT count(*) FROM item WHERE id = 'gone-x' AND state = 'open'`); n != 1 {
					t.Fatalf("남의 열린 gone-x 가 %d건이다(기대 1)", n)
				}

				if got := s.followupCandidates(ctx(), FinishInput{Project: "p", SessionID: me.Session.ID, ItemID: "batch7"}); slices.Contains(got, "gone-x") {
					t.Errorf("관문이 남의 항목을 이 세션이 바닥에 떨어뜨린 후속으로 본다: %v", got)
				}
				_, err := s.Finish(ctx(), in)
				var ref *RefusedError
				if !errors.As(err, &ref) {
					t.Fatalf("이 세션의 생성이 실패한 id 의 남의 항목을 잇는 마무리가 거절되지 않았다: %v", err)
				}
				if n := countRows(t, st, `SELECT count(*) FROM judgment_link WHERE target_kind = 'item' AND target_id = 'gone-x'`); n != 0 {
					t.Fatalf("남의 항목에 이 마무리의 판단 링크가 %d건 걸렸다", n)
				}
			})
		}
	}
}

// TestFinishSkipsOnlyItemDuplicates 는 후속 등록의 중복 흡수가 item 표의 중복만 건너뛰는지 두 엔진에서 잰다. 선행
// 행(item_after)의 키 충돌은 「같은 id 의 항목이 이미 있다」가 아니다 - 건너뛰면 거짓 사유가 추가 전용 원장에 굳고
// 마무리가 커밋된다. item 표의 중복을 건너뛰는 쪽은 TestFinishKeepsTheJudgmentWhenAFollowupTurnsOutDuplicate 가 잰다.
//
// ★ 운영 스키마의 item_after 에는 유니크 키가 없다. 시험이 (project, item_id) 유니크 인덱스를 걸어 선행 둘째 행이
// 중복이 되게 한다 - 두 엔진 모두 같은 문장이다.
func TestFinishSkipsOnlyItemDuplicates(t *testing.T) {
	for _, e := range bothLedgers() {
		t.Run(e.name, func(t *testing.T) {
			s, st := e.open(t)
			repo, wt := newRepoWithWorktree(t, "feat")
			me := openSession(t, s, "p", repo, wt, "cc-a", "트랙A")
			addItem(t, s, "p", "batch7", nil, nil)
			claimed(t, s, "p", me.Session.ID, "batch7")
			rawExec(t, st, `CREATE UNIQUE INDEX fd_test_one_after ON item_after(project, item_id)`)

			_, err := s.Finish(ctx(), FinishInput{
				Project: "p", SessionID: me.Session.ID, ItemID: "batch7",
				Outcome: model.ItemDone, Title: "끝냈다", Body: "판단 본문",
				Followups: []FollowupInput{{ID: "two-deps", Title: "후속", Body: "후속 본문",
					After: []model.After{{Item: "batch7"}, {SHA: "0123456789abcdef0123456789abcdef01234567"}}}},
			})
			var conflict *store.ConflictError
			if !errors.As(err, &conflict) || conflict.Kind != store.ConflictDuplicate || conflict.Target != store.TargetItemAfter {
				t.Fatalf("선행 행의 키 충돌이 그대로 안 올라왔다: %v", err)
			}
			if n := countRows(t, st, `SELECT count(*) FROM judgment`); n != 0 {
				t.Fatalf("판단이 %d건 커밋됐다 - 선행 행의 충돌을 항목 중복으로 흡수했다", n)
			}
			if n := countRows(t, st, `SELECT count(*) FROM event WHERE kind = 'item.followup_skipped'`); n != 0 {
				t.Fatalf("건너뜀이 %d건 남았다 - 선행 행의 충돌이 「같은 id 의 항목이 이미 있다」로 굳었다", n)
			}
			if n := countRows(t, st, `SELECT count(*) FROM item WHERE id = 'batch7' AND state = 'done'`); n != 0 {
				t.Fatal("실패한 마무리가 항목을 닫았다")
			}
		})
	}
}
