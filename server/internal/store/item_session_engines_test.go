package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
)

// item.go · session.go 의 업무 SQL 이 두 엔진에서 같은 답을 내는지 잰다. 모든 단정을 SQLite 파일과 Postgres 시험 DB
// 에서 한 번씩 돈다(bothEngines) - SQLite 는 지금 운영 길이라 답이 그대로여야 하고, Postgres 는 옮겨 갈 길이다.
//
// ★ 방언이 갈리던 자리 넷은 따로 잠근다(아래 시험들). 첫 시험은 두 파일의 나머지 실행 자리를 실제 호출로 한 번씩
// 다 지나게 해, 「읽어 보니 두 엔진에 같은 꼴이다」를 실행으로 굳힌다.

// TestItemSessionSQLRunsOnBothEngines 는 item.go · session.go 의 공개 질의를 한 줄거리로 다 부른다.
func TestItemSessionSQLRunsOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
		seed(t, s, "p")
		seed(t, s, "q")

		// ── 세션
		a, created, err := s.OpenSessionAs(ctx, "p", "m1", "/w/a", "cc-1", "첫", "claude", at(0))
		if err != nil || !created {
			t.Fatalf("세션 열기: created=%v err=%v", created, err)
		}
		again, created, err := s.OpenSessionAs(ctx, "p", "m1", "/w/a", "cc-1", "둘", "", at(1))
		if err != nil || created || again.ID != a.ID || again.Label != "둘" || again.Harness != "claude" {
			t.Fatalf("같은 3중키 다시 열기: %+v created=%v err=%v", again, created, err)
		}
		b := openAt(t, s, "p", "/w/b", "cc-1", at(2))
		c := openAt(t, s, "q", "/w/c", "cc-1", at(3))
		if got, err := s.FindSession(ctx, "m1", "/w/b", "cc-1"); err != nil || got.ID != b.ID {
			t.Fatalf("FindSession: %+v %v", got, err)
		}
		if got, err := s.GetSession(ctx, a.ID); err != nil || got.Label != "둘" || !got.OpenedAt.Equal(at(0)) {
			t.Fatalf("GetSession: %+v %v", got, err)
		}
		if err := s.Tx(ctx, func(x *Tx) error {
			if got, err := x.FindSession("m1", "/w/a", "cc-1"); err != nil || got.ID != a.ID {
				return fmt.Errorf("Tx.FindSession: %+v %v", got, err)
			}
			if got, err := x.GetSession(c.ID); err != nil || got.Project != "q" {
				return fmt.Errorf("Tx.GetSession: %+v %v", got, err)
			}
			div, err := x.DivergentSessions("cc-1", "p", "m1")
			if err != nil || len(div) != 1 || div[0].ID != c.ID {
				return fmt.Errorf("DivergentSessions: %+v %v", div, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSessionState(ctx, b.ID, model.SessionBlocked, "기다림"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetSession(ctx, b.ID); got.State != model.SessionBlocked || got.BlockedWhy != "기다림" {
			t.Fatalf("막힘 상태: %+v", got)
		}
		if err := s.SetSessionState(ctx, b.ID, model.SessionActive, ""); err != nil {
			t.Fatal(err)
		}
		d := openAt(t, s, "q", "/w/d", "cc-d", at(4))
		if got, err := s.Rekey(ctx, d.ID, "cc-e"); err != nil || got.CCSessionID != "cc-e" {
			t.Fatalf("Rekey: %+v %v", got, err)
		}

		// ── 워크스페이스 · 신호 · 발자국
		for _, w := range []model.Workspace{
			{SessionID: a.ID, Project: "p", Path: "/w/a", IsPrimary: true},
			{SessionID: a.ID, Project: "p", Path: "/w/z"},
			{SessionID: a.ID, Project: "q", Path: "/w/z"}, // 같은 경로를 다시 - 덮는다
		} {
			if err := s.AddWorkspace(ctx, w); err != nil {
				t.Fatal(err)
			}
		}
		ws, err := s.ListWorkspaces(ctx, a.ID)
		if err != nil || !reflect.DeepEqual(ws, []model.Workspace{
			{SessionID: a.ID, Project: "p", Path: "/w/a", IsPrimary: true},
			{SessionID: a.ID, Project: "q", Path: "/w/z"},
		}) {
			t.Fatalf("ListWorkspaces: %+v %v", ws, err)
		}
		for _, bt := range []struct {
			kind model.SignalKind
			m    int
		}{{model.SignalPrompt, 10}, {model.SignalPrompt, 9}, {model.SignalTool, 8}} {
			if err := s.Beat(ctx, a.ID, bt.kind, at(bt.m)); err != nil {
				t.Fatal(err)
			}
		}
		sig, err := s.Signals(ctx, a.ID)
		if err != nil || len(sig) != 2 || !sig[model.SignalPrompt].Equal(at(10)) || !sig[model.SignalTool].Equal(at(8)) {
			t.Fatalf("Signals(뒤로 안 간다): %v %v", sig, err)
		}
		if last, ok, err := s.LastSignal(ctx, a.ID); err != nil || !ok || !last.Equal(at(10)) {
			t.Fatalf("LastSignal: %v %v %v", last, ok, err)
		}
		if _, ok, err := s.LastSignal(ctx, c.ID); err != nil || ok {
			t.Fatalf("신호 없는 세션의 LastSignal: ok=%v err=%v", ok, err)
		}
		for _, f := range []struct {
			path   string
			origin model.FootprintOrigin
			m      int
		}{{"x/b.go", model.OriginObserved, 5}, {"x/b.go", model.OriginObserved, 6}, {"x/a.go", model.OriginClaimed, 5}, {"x/b.go", model.OriginClaimed, 5}} {
			if err := s.Touch(ctx, a.ID, f.path, f.origin, at(f.m)); err != nil {
				t.Fatal(err)
			}
		}
		fps, err := s.Footprints(ctx, a.ID)
		if err != nil || len(fps) != 3 || fps[0].Path != "x/a.go" || fps[1].Origin != model.OriginClaimed ||
			fps[2].Origin != model.OriginObserved || !fps[2].FirstAt.Equal(at(5)) || !fps[2].LastAt.Equal(at(6)) {
			t.Fatalf("Footprints: %+v %v", fps, err)
		}
		if paths, err := s.FootprintPaths(ctx, a.ID); err != nil || !slices.Equal(paths, []string{"x/a.go", "x/b.go"}) {
			t.Fatalf("FootprintPaths: %v %v", paths, err)
		}

		// ── ref 상태 · 변경집합
		for _, r := range []model.RefState{
			{Project: "p", Ref: "refs/heads/main", SHA: "sha-1", Subject: "제목", At: at(1)},
			{Project: "p", Ref: "refs/heads/main", SHA: "sha-2", At: at(2)},
		} {
			if err := s.UpsertRefState(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		if r, err := s.GetRefState(ctx, "p", "refs/heads/main"); err != nil || r.SHA != "sha-2" || r.Subject != "" || !r.At.Equal(at(2)) {
			t.Fatalf("GetRefState: %+v %v", r, err)
		}
		if err := s.UpsertChangeSet(ctx, model.ChangeSet{Project: "p", BaseSHA: "b0", HeadSHA: "h0", Paths: []string{"a", "b"}, ComputedAt: at(1)}); err != nil {
			t.Fatal(err)
		}
		if cs, err := s.GetChangeSet(ctx, "p", "b0", "h0"); err != nil || !slices.Equal(cs.Paths, []string{"a", "b"}) {
			t.Fatalf("GetChangeSet: %+v %v", cs, err)
		}

		// ── 항목 · 선행
		for i, it := range []model.Item{
			{Project: "p", ID: "dep"},
			{Project: "p", ID: "waiter", After: []model.After{{Item: "dep"}, {SHA: "abc123"}, {Job: "job-1"}}},
			{Project: "q", ID: "dep"},
			{Project: "q", ID: "far", After: []model.After{{Project: "p", Item: "dep"}}},
		} {
			it.Title, it.Body, it.CreatedAt = it.ID, "본문", at(i)
			if err := s.AddItem(ctx, it); err != nil {
				t.Fatal(err)
			}
		}
		if it, err := s.GetItem(ctx, "p", "waiter"); err != nil || !reflect.DeepEqual(it.After,
			[]model.After{{Item: "dep"}, {SHA: "abc123"}, {Job: "job-1"}}) {
			t.Fatalf("GetItem 의 선행(넣은 순서): %+v %v", it.After, err)
		}
		if ps, err := s.ItemProjects(ctx, "dep"); err != nil || !slices.Equal(ps, []string{"p", "q"}) {
			t.Fatalf("ItemProjects: %v %v", ps, err)
		}
		if n, err := s.Dependents(ctx, "p", "dep"); err != nil || n != 2 {
			t.Fatalf("Dependents: %d %v", n, err)
		}
		if names, err := s.DependentItems(ctx, "p", "dep"); err != nil || !slices.Equal(names, []string{"q/far", "waiter"}) {
			t.Fatalf("DependentItems: %v %v", names, err)
		}
		if open, err := s.ListOpen(ctx, "p"); err != nil || len(open) != 2 || open[0].ID != "dep" || len(open[1].After) != 3 {
			t.Fatalf("ListOpen: %+v %v", open, err)
		}
		if n, err := s.CountOpen(ctx, "p"); err != nil || n != 2 {
			t.Fatalf("CountOpen: %d %v", n, err)
		}
		if all, err := s.ListItems(ctx, "q"); err != nil || len(all) != 2 || all[1].After[0].Project != "p" {
			t.Fatalf("ListItems: %+v %v", all, err)
		}
		if err := s.RemoveAfter(ctx, "p", "waiter", model.After{SHA: "abc123"}, a.ID); err != nil {
			t.Fatalf("RemoveAfter(sha): %v", err)
		}
		if err := s.RemoveAfter(ctx, "p", "waiter", model.After{SHA: "abc123"}, a.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("이미 끊은 선행을 다시 끊으면 없음이어야 한다: %v", err)
		}
		if err := s.Tx(ctx, func(x *Tx) error {
			if err := x.RemoveAfter("p", "waiter", model.After{Job: "job-1"}, a.ID); err != nil {
				return err
			}
			return x.AddAfter("p", "waiter", model.After{SHA: "def456"})
		}); err != nil {
			t.Fatal(err)
		}
		if it, _ := s.GetItem(ctx, "p", "waiter"); !reflect.DeepEqual(it.After, []model.After{{Item: "dep"}, {SHA: "def456"}}) {
			t.Fatalf("끊고 단 뒤의 선행: %+v", it.After)
		}

		// ── 선점
		if cl, err := s.ClaimItem(ctx, "p", "waiter", a.ID, at(20)); err != nil || !cl.At.Equal(at(20)) {
			t.Fatalf("ClaimItem: %+v %v", cl, err)
		}
		var held *ClaimHeldError
		if _, err := s.ClaimItem(ctx, "p", "waiter", b.ID, at(21)); !errors.As(err, &held) || held.Holder != a.ID {
			t.Fatalf("남의 선점: %v", err)
		}
		if cl, err := s.ClaimItem(ctx, "p", "waiter", a.ID, at(22)); err != nil || !cl.At.Equal(at(20)) {
			t.Fatalf("이어 쥐기는 처음 시각을 낸다: %+v %v", cl, err)
		}
		if _, err := s.ClaimItem(ctx, "q", "far", a.ID, at(21)); err != nil {
			t.Fatal(err)
		}
		if cl, err := s.GetClaim(ctx, "p", "waiter"); err != nil || cl.SessionID != a.ID || cl.ReleasedAt != nil {
			t.Fatalf("GetClaim: %+v %v", cl, err)
		}
		if err := s.Tx(ctx, func(x *Tx) error {
			cl, err := x.LiveClaim("p", "waiter")
			if err != nil || cl.SessionID != a.ID {
				return fmt.Errorf("LiveClaim: %+v %v", cl, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if ids, err := s.ClaimedItems(ctx, a.ID); err != nil || !slices.Equal(ids, []string{"far", "waiter"}) {
			t.Fatalf("ClaimedItems: %v %v", ids, err)
		}
		if refs, err := s.ClaimedRefs(ctx, a.ID); err != nil || len(refs) != 2 || refs[0].Project != "q" || refs[1].ItemID != "waiter" {
			t.Fatalf("ClaimedRefs: %+v %v", refs, err)
		}
		if names, err := s.ClaimedItemsLabeled(ctx, a.ID); err != nil || !slices.Equal(names, []string{"q/far", "waiter"}) {
			t.Fatalf("ClaimedItemsLabeled: %v %v", names, err)
		}
		if ids, err := s.SiblingClaimedItems(ctx, "p", "m1", "cc-1", b.ID); err != nil || !slices.Equal(ids, []string{"waiter"}) {
			t.Fatalf("SiblingClaimedItems: %v %v", ids, err)
		}
		if err := s.ReleaseClaim(ctx, "p", "waiter", a.ID); err != nil {
			t.Fatal(err)
		}
		if ids, err := s.ReleasedItems(ctx, a.ID, at(0)); err != nil || !slices.Equal(ids, []string{"waiter"}) {
			t.Fatalf("ReleasedItems: %v %v", ids, err)
		}
		if ids, err := s.SiblingReleasedItems(ctx, "p", "m1", "cc-1", b.ID, at(0)); err != nil || !slices.Equal(ids, []string{"waiter"}) {
			t.Fatalf("SiblingReleasedItems: %v %v", ids, err)
		}
		// 반납된 행 위의 재선점 - upsert 가 덮는 길이다.
		if _, err := s.ClaimItem(ctx, "p", "waiter", b.ID, at(23)); err != nil {
			t.Fatalf("반납된 항목 다시 선점: %v", err)
		}
		if err := s.ForceReleaseClaim(ctx, "p", "waiter", "회수"); err != nil {
			t.Fatal(err)
		}
		if cl, _ := s.GetClaim(ctx, "p", "waiter"); cl.SessionID != b.ID || cl.ForceReason != "회수" || cl.ReleasedAt == nil {
			t.Fatalf("강제 반납 뒤 선점 행: %+v", cl)
		}
		if _, err := s.ClaimItem(ctx, "p", "waiter", a.ID, at(24)); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishItem(ctx, "p", "waiter", a.ID, model.ItemDone, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.SetLandedRef(ctx, "p", "waiter", "sha-l"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetLabels(ctx, "p", "dep", []string{"x", "y"}, a.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.SetItemState(ctx, "p", "dep", model.ItemDropped, "버림"); err != nil {
			t.Fatal(err)
		}
		var closedErr *ItemClosedError
		if err := s.SetItemState(ctx, "p", "dep", model.ItemOpen, ""); !errors.As(err, &closedErr) {
			t.Fatalf("닫힌 항목을 다시 여는 것은 거절이어야 한다: %v", err)
		}
		if it, _ := s.GetItem(ctx, "p", "waiter"); it.State != model.ItemDone || it.LandedRef != "sha-l" || it.ClosedAt == nil {
			t.Fatalf("끝낸 항목: %+v", it)
		}
		if it, _ := s.GetItem(ctx, "p", "dep"); !slices.Equal(it.Labels, []string{"x", "y"}) || it.CloseReason != "버림" {
			t.Fatalf("버린 항목: %+v", it)
		}
		if got, total, err := s.ListClosed(ctx, "p", model.ItemDone, 10); err != nil || total != 1 || len(got) != 1 || got[0].ID != "waiter" {
			t.Fatalf("ListClosed(done): %+v %d %v", got, total, err)
		}

		// ── 선택 기록
		if err := s.RecordPickEval(ctx, model.PickEval{Project: "p", SessionID: a.ID, At: at(30), Picked: "dep",
			PickedWith: []string{"w2"}, Rejected: []model.Rejection{{Item: "waiter", Reason: "claimed"}}}); err != nil {
			t.Fatal(err)
		}
		var picked, with, rejected string
		if err := s.db.QueryRowContext(ctx, `SELECT picked, picked_with, rejected FROM pick_eval`).Scan(&picked, &with, &rejected); err != nil ||
			picked != "dep" || with != `["w2"]` || rejected != `[{"item":"waiter","reason_code":"claimed","detail":""}]` {
			t.Fatalf("pick_eval 행: %q %q %q %v", picked, with, rejected, err)
		}

		// ── 생존 창
		live, err := s.ListLive(ctx, "p", at(2))
		if err != nil || len(live) != 2 || live[0].Session.ID != b.ID || live[1].Session.ID != a.ID ||
			!slices.Equal(live[1].Paths, []string{"x/a.go", "x/b.go"}) || !slices.Equal(live[1].Claims, []string{"far"}) ||
			!live[1].Signals[model.SignalPrompt].Equal(at(10)) || !live[1].HasFootprint || live[0].HasFootprint {
			t.Fatalf("ListLive: %+v %v", live, err)
		}
		if live, err := s.ListLive(ctx, "p", at(11)); err != nil || len(live) != 0 {
			t.Fatalf("창 밖만 있을 때 ListLive: %+v %v", live, err)
		}
		out, err := s.SummarizeOutside(ctx, "p", at(11))
		if err != nil || out.Count != 2 || !out.Oldest.Equal(at(10)) || len(out.Claimed) != 1 ||
			out.Claimed[0].Session.ID != a.ID || !slices.Equal(out.Claimed[0].Claims, []string{"far"}) {
			t.Fatalf("SummarizeOutside: %+v %v", out, err)
		}
		if ss, err := s.ListSessions(ctx, "p"); err != nil || len(ss) != 2 || ss[0].ID != a.ID || ss[1].ID != b.ID {
			t.Fatalf("ListSessions: %+v %v", ss, err)
		}
		if last, err := s.LastSessionAt(ctx, "p"); err != nil || !last.Equal(at(2)) {
			t.Fatalf("LastSessionAt: %v %v", last, err)
		}
		if last, err := s.LastSessionAt(ctx, "없음"); err != nil || !last.IsZero() {
			t.Fatalf("세션 없는 프로젝트의 LastSessionAt: %v %v", last, err)
		}

		// ── 워크트리 없는 세션 닫기: 선점을 쥔 a 는 안 닫히고, 빈손인 b 는 한 번만 닫힌다
		for _, cs := range []struct {
			id   string
			want bool
		}{{a.ID, false}, {b.ID, true}, {b.ID, false}} {
			var closed bool
			if err := s.Tx(ctx, func(x *Tx) error {
				var err error
				closed, err = x.CloseSessionWhoseWorktreeIsGone(cs.id, "p", map[string]any{"why": "gone"})
				return err
			}); err != nil || closed != cs.want {
				t.Fatalf("CloseSessionWhoseWorktreeIsGone(%s): %v %v (기대 %v)", cs.id, closed, err, cs.want)
			}
		}

		if err := s.DeleteItem(ctx, "q", "dep"); err != nil {
			t.Fatal(err)
		}
		if ps, err := s.ItemProjects(ctx, "dep"); err != nil || !slices.Equal(ps, []string{"p"}) {
			t.Fatalf("지운 뒤 ItemProjects: %v %v", ps, err)
		}
	})
}

func openAt(t *testing.T, s *Store, project, worktree, cc string, at time.Time) model.Session {
	t.Helper()
	sess, _, err := s.OpenSession(context.Background(), project, "m1", worktree, cc, "", at)
	if err != nil {
		t.Fatalf("세션 열기(%s %s): %v", project, worktree, err)
	}
	return sess
}

// TestListClosedPutsUndatedRowsLast 는 닫힌 시각이 없는 행이 두 엔진 모두에서 「최근에 닫힌 순」 목록의 끝에 서는지
// 잰다. Postgres 는 NULL 을 DESC 의 처음에 두므로, 정렬이 그것을 적지 않으면 limit 1 이 시각 없는 행을 낸다.
func TestListClosedPutsUndatedRowsLast(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		closed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
		for _, it := range []model.Item{
			{Project: "p", ID: "a-undated", Title: "t", Body: "b", State: model.ItemDone},
			{Project: "p", ID: "b-dated", Title: "t", Body: "b", State: model.ItemDone, ClosedAt: &closed},
		} {
			if err := s.AddItem(ctx, it); err != nil {
				t.Fatal(err)
			}
		}
		got, total, err := s.ListClosed(ctx, "p", "", 1)
		if err != nil || total != 2 || len(got) != 1 || got[0].ID != "b-dated" {
			t.Fatalf("limit 1 은 닫힌 시각이 있는 행이어야 한다: %+v total=%d err=%v", got, total, err)
		}
		got, _, err = s.ListClosed(ctx, "p", "", 10)
		if err != nil || len(got) != 2 || got[1].ID != "a-undated" || got[1].ClosedAt != nil {
			t.Fatalf("시각 없는 행은 끝이어야 한다: %+v err=%v", got, err)
		}
	})
}

// TestWriteClaimLeavesALiveClaimAlone 은 선점 쓰기가 살아 있는 남의 선점을 덮지 않고, 트랜잭션을 버리지도 않는지 잰다.
//
// ★ 정상 길(ClaimItem)은 쓰기 전에 점유자를 확인하고 쓰기가 한 줄로 서므로 이 자리에 못 닿는다. 그래서 확인을 건너뛴
// 쓰기(writeClaim)를 직접 불러 직렬성이 깨진 자리를 흉내 낸다. 조건 없는 upsert 였다면 SQLite 는 남의 선점을 말없이
// 덮고, Postgres 는 트리거 claim_one_live_holder 의 23505 로 트랜잭션을 버린다(그 뒤의 점유자 읽기가 25P02 로 죽는다).
func TestWriteClaimLeavesALiveClaimAlone(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		mustItem(t, s, "p", "it")
		a := mustSession(t, s, "p", "cc-a")
		b := mustSession(t, s, "p", "cc-b")
		if _, err := s.ClaimItem(ctx, "p", "it", a.ID, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := s.Tx(ctx, func(x *Tx) error {
			written, err := x.writeClaim("p", "it", b.ID, time.Now())
			if err != nil {
				return fmt.Errorf("살아 있는 선점 위의 쓰기가 오류를 냈다: %w", err)
			}
			if written {
				return errors.New("살아 있는 남의 선점을 덮었다")
			}
			// 트랜잭션이 살아 있어 같은 자리에서 점유자를 읽고 판정한다.
			var held *ClaimHeldError
			if _, err := x.claimTakenMeanwhile("p", "it", b.ID, model.ItemClaimed); !errors.As(err, &held) || held.Holder != a.ID {
				return fmt.Errorf("남이 끼어든 자리는 점유자를 담은 ClaimHeldError 여야 한다: %v", err)
			}
			if cl, err := x.claimTakenMeanwhile("p", "it", a.ID, model.ItemClaimed); err != nil || cl.SessionID != a.ID {
				return fmt.Errorf("자기 선점이 끼어든 자리는 이어 쥐기여야 한다: %+v %v", cl, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if cl, err := s.GetClaim(ctx, "p", "it"); err != nil || cl.SessionID != a.ID || cl.ReleasedAt != nil {
			t.Fatalf("선점이 그대로여야 한다: %+v %v", cl, err)
		}

		// 대조: 반납된 행은 덮는다(재선점 길). 이것이 없으면 위 단정은 「아무것도 안 쓴다」로도 초록이다.
		if err := s.ReleaseClaim(ctx, "p", "it", a.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.Tx(ctx, func(x *Tx) error {
			written, err := x.writeClaim("p", "it", b.ID, time.Now())
			if err != nil || !written {
				return fmt.Errorf("반납된 행 위의 쓰기: written=%v err=%v", written, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if cl, err := s.GetClaim(ctx, "p", "it"); err != nil || cl.SessionID != b.ID || cl.ReleasedAt != nil {
			t.Fatalf("반납된 행은 새 점유자로 덮여야 한다: %+v %v", cl, err)
		}
	})
}

// TestClaimHolderAtReadsTheItemFromThePayload 는 원장 payload 의 item 을 두 엔진이 같이 읽는지 잰다.
//
// ★ payload 의 다른 칸에 \u0000 이스케이프가 든 행을 둔다. Postgres 는 json · jsonb 어느 쪽으로 읽어도 그것을
// 거절해(22P05) 질의 전체가 죽는다 - SQL 이 payload 를 해석하지 않고 Go 가 읽는 까닭이다(payloadItem).
func TestClaimHolderAtReadsTheItemFromThePayload(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		x := mustSession(t, s, "p", "cc-x")
		y := mustSession(t, s, "p", "cc-y")
		t1 := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
		insertEventAt(t, s, fmtTime(t1), "p", x.ID, `{"item":"fd-x","why":"a\u0000b"}`, "item.claim")
		insertEventAt(t, s, fmtTime(t1), "p", y.ID, `{"item":"fd-y"}`, "item.claim")
		insertEventAt(t, s, fmtTime(t1.Add(time.Minute)), "p", x.ID, `{"item":"fd-x"}`, "item.finish")

		for _, c := range []struct {
			item string
			at   time.Time
			want string
		}{
			{"fd-x", t1.Add(time.Second), x.ID},
			{"fd-x", t1.Add(2 * time.Minute), ""},
			{"fd-y", t1.Add(2 * time.Minute), y.ID},
			{"fd-z", t1.Add(2 * time.Minute), ""},
		} {
			got, err := s.ClaimHolderAt(ctx, "p", c.item, c.at)
			if err != nil || got != c.want {
				t.Fatalf("ClaimHolderAt(%s, %s) = %q, %v (기대 %q)", c.item, fmtTime(c.at), got, err, c.want)
			}
		}
	})
}

// TestPayloadItemReadsLikeJSONExtract 는 payloadItem 이 SQLite 의 `json_extract(payload, '$.item') = ?` 와 같은
// 자리에서 맞고 · 빠지고 · 죽는지 잰다. 같은 판을 SQLite 에 직접 물어 견준다 - 옮기기 전 식이 기준이다.
func TestPayloadItemReadsLikeJSONExtract(t *testing.T) {
	s := newStore(t)
	for _, payload := range []string{
		`{"item":"fd-x"}`,
		`{"item":"fd-x","why":"a\u0000b"}`,
		`{"Item":"fd-x"}`,
		`{"item":null}`,
		`{"item":3}`,
		`{"item":["fd-x"]}`,
		`{"other":"fd-x"}`,
		`{}`,
		`["fd-x"]`,
		`"fd-x"`,
		`3`,
		`null`,
		`{"item":"fd-x"}`,
		`not json`,
		`{"item":"fd-x"`,
	} {
		var want sql.NullString
		sqlErr := s.db.QueryRowContext(context.Background(),
			`SELECT CASE WHEN json_extract(?, '$.item') = 'fd-x' THEN 'hit' ELSE 'miss' END`, payload).Scan(&want)
		item, ok, err := payloadItem(payload)
		got := "miss"
		if ok && item == "fd-x" {
			got = "hit"
		}
		if (sqlErr != nil) != (err != nil) {
			t.Fatalf("payload %s: json_extract 오류 %v, payloadItem 오류 %v - 죽는 자리가 달라졌다", payload, sqlErr, err)
		}
		if sqlErr == nil && want.String != got {
			t.Fatalf("payload %s: json_extract %q, payloadItem %q", payload, want.String, got)
		}
	}
}
