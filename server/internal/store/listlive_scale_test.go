package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
)

// ListLive 가 세션마다 묻던 세 축(신호 · 발자국 · 선점)을 한 번씩 묻게 된 뒤에도
// **세션마다 물은 것과 같은 답**을 내는지 본다. 세션 수가 많고, 축마다 비거나 여럿인 세션이
// 섞이고, 거름에 걸리는 세션(done · 다른 프로젝트 · 창 밖)이 하위 질의에 새지 않는지가 요점이다.
func TestListLiveBatchedAxesMatchPerSessionReads(t *testing.T) {
	s := newStore(t)
	seed(t, s, "p")
	seed(t, s, "q")
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)

	const n = 60
	var ids []string
	for i := 0; i < n; i++ {
		sess := mustSession(t, s, "p", fmt.Sprintf("cc-%02d", i))
		ids = append(ids, sess.ID)
		if i%3 != 0 { // 셋 중 하나는 신호가 없다
			if err := s.Beat(ctx, sess.ID, model.SignalPrompt, now.Add(-time.Duration(i)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		if i%4 == 0 {
			if err := s.Beat(ctx, sess.ID, model.SignalTool, now.Add(-time.Duration(i)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		for j := 0; j < i%3; j++ { // 0~2 개의 발자국, 같은 경로를 두 origin 으로도 남긴다
			p := fmt.Sprintf("dir%d/f%d.go", i%5, j)
			if err := s.Touch(ctx, sess.ID, p, model.OriginObserved, now); err != nil {
				t.Fatal(err)
			}
			if err := s.Touch(ctx, sess.ID, p, model.OriginDeclared, now); err != nil {
				t.Fatal(err)
			}
		}
		if i%7 == 0 {
			item := fmt.Sprintf("it-%02d", i)
			mustItem(t, s, "p", item)
			if _, err := s.ClaimItem(ctx, "p", item, sess.ID, time.Time{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 거름에 걸려야 하는 셋, 축 질의가 이 세션들의 행을 가져와도 결과에 새면 안 된다.
	done := mustSession(t, s, "p", "cc-done")
	if err := s.Touch(ctx, done.ID, "done.go", model.OriginObserved, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionState(ctx, done.ID, model.SessionDone, ""); err != nil {
		t.Fatal(err)
	}
	other := mustSession(t, s, "q", "cc-other")
	if err := s.Touch(ctx, other.ID, "other.go", model.OriginObserved, now); err != nil {
		t.Fatal(err)
	}

	for _, since := range []time.Time{{}, now.Add(-30 * time.Minute), old} {
		views, err := s.ListLive(ctx, "p", since)
		if err != nil {
			t.Fatalf("ListLive(since=%v) 실패: %v", since, err)
		}
		if since.IsZero() && len(views) != n {
			t.Fatalf("창 없이 세션 %d개를 기대했는데 %d개", n, len(views))
		}
		for _, v := range views {
			if v.Session.ID == done.ID || v.Session.ID == other.ID {
				t.Fatalf("거름에 걸려야 할 세션 %s 가 나왔다", v.Session.Label)
			}
			sig, err := s.Signals(ctx, v.Session.ID)
			if err != nil {
				t.Fatal(err)
			}
			paths, err := s.FootprintPaths(ctx, v.Session.ID)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := s.ClaimedItems(ctx, v.Session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(v.Signals, sig) {
				t.Errorf("세션 %s 신호: 묶음 %v · 하나씩 %v", v.Session.CCSessionID, v.Signals, sig)
			}
			if v.Signals == nil {
				t.Errorf("세션 %s 신호 맵이 nil, 신호 없는 세션도 빈 맵이어야 한다", v.Session.CCSessionID)
			}
			if !reflect.DeepEqual(v.Paths, paths) {
				t.Errorf("세션 %s 발자국: 묶음 %v · 하나씩 %v", v.Session.CCSessionID, v.Paths, paths)
			}
			if !reflect.DeepEqual(v.Claims, claims) {
				t.Errorf("세션 %s 선점: 묶음 %v · 하나씩 %v", v.Session.CCSessionID, v.Claims, claims)
			}
			if v.HasFootprint != (len(paths) > 0) {
				t.Errorf("세션 %s HasFootprint=%v 인데 발자국 %d개", v.Session.CCSessionID, v.HasFootprint, len(paths))
			}
		}
	}
	_ = ids
}

// ProjectRefCountsAll 이 프로젝트 하나씩 센 수와 같은지 본다, 목록(GET /projects)과
// 삭제 판정(RemoveProject)이 같은 수를 보는지가 요점이다. 행이 없는 프로젝트는 0 으로 채워진다.
func TestProjectRefCountsAllMatchesPerProject(t *testing.T) {
	s := newStore(t)
	seed(t, s, "p")
	seed(t, s, "q")
	seed(t, s, "empty")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		mustSession(t, s, "p", fmt.Sprintf("p-%d", i))
		mustItem(t, s, "p", fmt.Sprintf("p-it-%d", i))
	}
	qs := mustSession(t, s, "q", "q-0")
	mustItem(t, s, "q", "q-it")
	// 다른 프로젝트(p)의 판단이 q 의 세션을 가리킨다, judgment_foreign 의 한 건.
	if _, err := s.db.Exec(`INSERT INTO judgment(id, project, session_id, kind, title, body, at)
		VALUES (?, 'p', ?, 'decision', 't', 'b', ?)`, NewID(), qs.ID, fmtTime(time.Now().UTC())); err != nil {
		t.Fatalf("교차 판단 넣기 실패: %v", err)
	}

	all, err := s.ProjectRefCountsAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"p", "q", "empty", "never-registered"} {
		one, err := s.ProjectRefCounts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got := RefCountsOf(all, id); !reflect.DeepEqual(got, one) {
			t.Errorf("%s: 한 번에 %v · 하나씩 %v", id, got, one)
		}
	}
	if got := RefCountsOf(all, "q")["judgment_foreign"]; got != 1 {
		t.Errorf("q 의 judgment_foreign = %d, 1 을 기대", got)
	}
	if got := RefCountsOf(all, "p")["session"]; got != 5 {
		t.Errorf("p 의 session = %d, 5 를 기대", got)
	}
}
