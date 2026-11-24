package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
)

// SummarizeOutside 가 앞 판의 보드 셈(ListLive(since=0) 에서 ListLive(cut) 을 뺀 것)과 같은 값을
// 내는지 본다. 건수 · 숨은 세션만의 가장 오래된 마지막 신호 · 선점을 든 숨은 세션(ListLive 와 같은
// 적재)이 요점이다. 끝난 세션 · 다른 프로젝트 · 푼 선점은 거름에 걸려야 한다.
func TestSummarizeOutsideMatchesListLiveDifference(t *testing.T) {
	s := newStore(t)
	seed(t, s, "p")
	seed(t, s, "q")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	const n = 40
	for i := 0; i < n; i++ {
		sess := mustSession(t, s, "p", fmt.Sprintf("cc-%02d", i))
		// 여는 시각을 i 시간 전으로, 창 밖 세션을 만들려면 열림도 창 밖이어야 한다.
		if _, err := s.db.Exec(`UPDATE session SET opened_at = ? WHERE id = ?`,
			fmtTime(now.Add(-time.Duration(i)*time.Hour)), sess.ID); err != nil {
			t.Fatal(err)
		}
		if i%3 != 0 { // 셋 중 하나는 신호가 없다
			if err := s.Beat(ctx, sess.ID, model.SignalPrompt, now.Add(-time.Duration(i)*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if i%4 == 0 { // 옛 신호 하나 더, 보이는 세션이면 Oldest 에 섞이면 안 된다
			if err := s.Beat(ctx, sess.ID, model.SignalCommit, now.Add(-time.Duration(i+30)*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if i%2 == 0 {
			if err := s.Touch(ctx, sess.ID, fmt.Sprintf("d/f%d.go", i), model.OriginObserved, now); err != nil {
				t.Fatal(err)
			}
		}
		if i%5 == 0 {
			item := fmt.Sprintf("it-%02d", i)
			mustItem(t, s, "p", item)
			if _, err := s.ClaimItem(ctx, "p", item, sess.ID, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if i%10 == 0 { // 둘째 선점은 풀어 둔다, 푼 선점은 세지 않는다
				item2 := item + "-b"
				mustItem(t, s, "p", item2)
				if _, err := s.ClaimItem(ctx, "p", item2, sess.ID, time.Time{}); err != nil {
					t.Fatal(err)
				}
				if err := s.ReleaseClaim(ctx, "p", item2, sess.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// 오래전에 열렸고 신호로만 창을 가르는 셋. 위 세션들은 신호가 열림보다 늦지 않아 거름의
	// 신호 갈래(EXISTS)가 판정을 가르지 않는다.
	//   recent: 열림 -50h, prompt -1h. 창 안(cut -90분)이고 신호 갈래로만 든다.
	//   stale:  열림 -50h, prompt -3h, commit -500h. cut -90분에서 창 밖, 마지막 신호는 -3h 라
	//           Oldest 가 -500h 로 나오면 세션별 MAX 가 아니라 신호 전체의 MIN 을 잰 것이다.
	//   stale 은 선점도 든다, Claimed 의 신호 두 종류 · 발자국까지 비교되게.
	for _, c := range []struct {
		cc   string
		sigs map[model.SignalKind]time.Duration
	}{
		{"cc-recent", map[model.SignalKind]time.Duration{model.SignalPrompt: time.Hour}},
		{"cc-stale", map[model.SignalKind]time.Duration{model.SignalPrompt: 3 * time.Hour, model.SignalCommit: 500 * time.Hour}},
	} {
		sess := mustSession(t, s, "p", c.cc)
		if _, err := s.db.Exec(`UPDATE session SET opened_at = ? WHERE id = ?`,
			fmtTime(now.Add(-50*time.Hour)), sess.ID); err != nil {
			t.Fatal(err)
		}
		for k, ago := range c.sigs {
			if err := s.Beat(ctx, sess.ID, k, now.Add(-ago)); err != nil {
				t.Fatal(err)
			}
		}
		if c.cc == "cc-stale" {
			mustItem(t, s, "p", "it-stale")
			if _, err := s.ClaimItem(ctx, "p", "it-stale", sess.ID, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if err := s.Touch(ctx, sess.ID, "stale.go", model.OriginObserved, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	const extra = 2

	// 거름에 걸려야 하는 둘: 끝난 옛 선점자, 다른 프로젝트의 옛 세션.
	done := mustSession(t, s, "p", "cc-done")
	mustItem(t, s, "p", "it-done")
	if _, err := s.ClaimItem(ctx, "p", "it-done", done.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	other := mustSession(t, s, "q", "cc-other")
	for _, id := range []string{done.ID, other.ID} {
		if _, err := s.db.Exec(`UPDATE session SET opened_at = ? WHERE id = ?`,
			fmtTime(now.Add(-100*time.Hour)), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetSessionState(ctx, done.ID, model.SessionDone, ""); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListLive(ctx, "p", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []time.Time{now.Add(-90 * time.Minute), now.Add(-10 * time.Hour), now.Add(-200 * time.Hour), now.Add(time.Hour)} {
		live, err := s.ListLive(ctx, "p", cut)
		if err != nil {
			t.Fatal(err)
		}
		shown := map[string]bool{}
		for _, v := range live {
			shown[v.Session.ID] = true
		}
		var want OutsideSummary
		for _, v := range all {
			if shown[v.Session.ID] {
				continue
			}
			want.Count++
			var last time.Time
			for _, at := range v.Signals {
				if at.After(last) {
					last = at
				}
			}
			if !last.IsZero() && (want.Oldest.IsZero() || last.Before(want.Oldest)) {
				want.Oldest = last
			}
			if len(v.Claims) > 0 {
				want.Claimed = append(want.Claimed, v)
			}
		}

		got, err := s.SummarizeOutside(ctx, "p", cut)
		if err != nil {
			t.Fatalf("SummarizeOutside(cut=%v) 실패: %v", cut, err)
		}
		if got.Count != want.Count {
			t.Errorf("cut=%v 건수 %d, ListLive 차 %d", cut, got.Count, want.Count)
		}
		if !got.Oldest.Equal(want.Oldest) {
			t.Errorf("cut=%v Oldest %v, ListLive 차 %v", cut, got.Oldest, want.Oldest)
		}
		if !reflect.DeepEqual(got.Claimed, want.Claimed) {
			t.Errorf("cut=%v 선점 세션\n 얻음 %+v\n 기대 %+v", cut, got.Claimed, want.Claimed)
		}
		for _, v := range got.Claimed {
			if v.Session.ID == done.ID || v.Session.ID == other.ID {
				t.Errorf("거름에 걸려야 할 세션 %s 가 나왔다", v.Session.CCSessionID)
			}
		}
	}
	// 시험이 빈 비교로 지나가지 않게 기대값을 숫자로도 박는다.
	//   cut -90분: recent 는 창 안, stale 은 창 밖이고 그 Oldest 후보는 -3h(-500h 가 아니다).
	got, err := s.SummarizeOutside(ctx, "p", now.Add(-90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var stale *model.SessionView
	for i, v := range got.Claimed {
		if v.Session.CCSessionID == "cc-recent" {
			t.Fatal("신호로 창 안에 든 세션이 창 밖 선점에 나왔다")
		}
		if v.Session.CCSessionID == "cc-stale" {
			stale = &got.Claimed[i]
		}
	}
	if stale == nil || len(stale.Signals) != 2 || !stale.HasFootprint || !reflect.DeepEqual(stale.Claims, []string{"it-stale"}) {
		t.Fatalf("창 밖 선점자 stale 의 적재가 틀렸다: %+v", stale)
	}
	if got.Oldest.Before(now.Add(-200 * time.Hour)) {
		t.Fatalf("Oldest %v 가 stale 의 옛 commit(-500h)이다, 세션별 마지막 신호가 아니다", got.Oldest)
	}
	//   cut 미래: 모든 세션이 창 밖이다.
	got, err = s.SummarizeOutside(ctx, "p", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != n+extra || len(got.Claimed) != n/5+1 || got.Oldest.IsZero() {
		t.Fatalf("미래 cut 에서 건수 %d(기대 %d) · 선점 세션 %d(기대 %d) · Oldest %v",
			got.Count, n+extra, len(got.Claimed), n/5+1, got.Oldest)
	}
}
