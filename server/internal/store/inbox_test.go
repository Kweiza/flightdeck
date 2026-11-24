package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
)

// session_inbox: 보드가 세션에 초인종을 보낼 받는 곳(board/README 「세션에 알림」).

var inboxAt = time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)

func inboxSockets(t *testing.T, s *Store, project, machine string) map[string]string {
	t.Helper()
	got, err := s.ListInbox(context.Background(), project, machine)
	if err != nil {
		t.Fatalf("받는 곳 조회 실패: %v", err)
	}
	out := map[string]string{}
	for _, e := range got {
		out[e.CCSessionID] = e.Socket + "|" + e.Mode
	}
	return out
}

// 적은 것이 그 프로젝트 · 그 기계로만 읽히고, 고치면 한 행이 고쳐진다.
func TestInboxIsReadByProjectAndMachine(t *testing.T) {
	s := newStore(t)
	seed(t, s, "p1")
	seed(t, s, "p2")
	ctx := context.Background()
	if err := s.UpsertMachine(ctx, model.Machine{ID: "m2", Hostname: "other"}); err != nil {
		t.Fatal(err)
	}
	a := mustSession(t, s, "p1", "cc-a")
	b := mustSession(t, s, "p2", "cc-b")
	c, _, err := s.OpenSession(ctx, "p1", "m2", "/w/c", "cc-c", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct{ id, sock string }{{a.ID, "/s/a"}, {b.ID, "/s/b"}, {c.ID, "/s/c"}} {
		if _, err := s.PutInbox(ctx, x.id, x.sock, "default", inboxAt); err != nil {
			t.Fatalf("받는 곳 적기 실패: %v", err)
		}
	}
	if _, err := s.PutInbox(ctx, a.ID, "/s/a", "bypassPermissions", inboxAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, want := inboxSockets(t, s, "p1", "m1"), map[string]string{"cc-a": "/s/a|bypassPermissions"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("p1 · m1 의 받는 곳 = %v, 기대 %v (다른 프로젝트 · 다른 기계가 섞이면 안 된다)", got, want)
	}
	// 모드가 비면(모름) 있던 모드를 둔다.
	if _, err := s.PutInbox(ctx, a.ID, "/s/a", "", inboxAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := inboxSockets(t, s, "p1", "m1")["cc-a"]; got != "/s/a|bypassPermissions" {
		t.Fatalf("빈 모드가 있던 모드를 지웠다: %q", got)
	}
}

// 한 기계에서 같은 소켓은 한 세션의 것이다: 새 세션이 그 소켓을 적으면 옛 세션 행이 사라진다.
func TestInboxSocketBelongsToOneSession(t *testing.T) {
	s := newStore(t)
	seed(t, s, "p1")
	ctx := context.Background()
	old := mustSession(t, s, "p1", "cc-old")
	nu := mustSession(t, s, "p1", "cc-new")
	if _, err := s.PutInbox(ctx, old.ID, "/s/x", "default", inboxAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutInbox(ctx, nu.ID, "/s/x", "default", inboxAt); err != nil {
		t.Fatal(err)
	}
	if got, want := inboxSockets(t, s, "p1", "m1"), map[string]string{"cc-new": "/s/x|default"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("같은 소켓의 받는 곳 = %v, 기대 %v", got, want)
	}
}

// rekey(/clear) 뒤에는 카드의 **지금** cc 로 읽힌다: 받는 세션이 메시지의 id 로 자기 것인지 안다.
func TestInboxFollowsRekey(t *testing.T) {
	s := newStore(t)
	seed(t, s, "p1")
	ctx := context.Background()
	a := mustSession(t, s, "p1", "cc-a")
	if _, err := s.PutInbox(ctx, a.ID, "/s/a", "default", inboxAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rekey(ctx, a.ID, "cc-a2"); err != nil {
		t.Fatal(err)
	}
	if got, want := inboxSockets(t, s, "p1", "m1"), map[string]string{"cc-a2": "/s/a|default"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rekey 뒤 받는 곳 = %v, 기대 %v", got, want)
	}
}

// 닫힌 카드는 안 읽히고, 지우기는 소켓이 같을 때만 지운다.
func TestInboxSkipsDoneAndDropsOnlyTheSameSocket(t *testing.T) {
	s := newStore(t)
	seed(t, s, "p1")
	ctx := context.Background()
	a := mustSession(t, s, "p1", "cc-a")
	d := mustSession(t, s, "p1", "cc-d")
	for _, id := range []string{a.ID, d.ID} {
		if _, err := s.PutInbox(ctx, id, "/s/"+id, "", inboxAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetSessionState(ctx, d.ID, model.SessionDone, ""); err != nil {
		t.Fatal(err)
	}
	if got := inboxSockets(t, s, "p1", "m1"); len(got) != 1 || got["cc-a"] == "" {
		t.Fatalf("닫힌 카드가 읽혔다: %v", got)
	}
	if n, err := s.DropInbox(ctx, a.ID, "/s/other"); err != nil || n != 0 {
		t.Fatalf("다른 소켓으로 지우기 = (%d, %v), 0 행이어야 한다(그 사이 훅이 새 소켓을 적었을 수 있다)", n, err)
	}
	if n, err := s.DropInbox(ctx, a.ID, "/s/"+a.ID); err != nil || n != 1 {
		t.Fatalf("같은 소켓으로 지우기 = (%d, %v), 1 행이어야 한다", n, err)
	}
	if got := inboxSockets(t, s, "p1", "m1"); len(got) != 0 {
		t.Fatalf("지운 뒤에도 남았다: %v", got)
	}
}

// 없는 세션에는 못 적는다(받는 곳만 있는 고아 행이 생기지 않게).
func TestInboxRefusesUnknownSession(t *testing.T) {
	s := newStore(t)
	seed(t, s, "p1")
	if _, err := s.PutInbox(context.Background(), "nope", "/s/x", "", inboxAt); err == nil {
		t.Fatal("없는 세션에 받는 곳이 적혔다")
	}
}

// 옛 등록 파일의 cc 중 이 프로젝트 · 이 기계에서 그 폴더를 품은 닫히지 않은 카드만 답한다.
func TestLiveSessionsOfAnswersOnlyThisProjectAndMachine(t *testing.T) {
	s := newStore(t)
	seed(t, s, "p1")
	seed(t, s, "p2")
	ctx := context.Background()
	mustSession(t, s, "p1", "cc-a")
	mustSession(t, s, "p2", "cc-b")
	d := mustSession(t, s, "p1", "cc-d")
	if err := s.SetSessionState(ctx, d.ID, model.SessionDone, ""); err != nil {
		t.Fatal(err)
	}
	asks := []LegacyAsk{{"cc-a", "/w/cc-a"}, {"cc-b", "/w/cc-b"}, {"cc-d", "/w/cc-d"}, {"cc-x", "/w/cc-x"}}
	got, err := s.LiveSessionsOf(ctx, "p1", "m1", asks)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"cc-a"}) {
		t.Fatalf("p1 · m1 의 살아 있는 옛 등록 = %v, 기대 [cc-a]", got)
	}
	if got, _ := s.LiveSessionsOf(ctx, "p1", "m1", nil); len(got) != 0 {
		t.Fatalf("빈 물음에 %v", got)
	}
}

// 한 cc 가 두 프로젝트에 카드를 가졌다(조정 세션이 남의 프로젝트에도 카드를 연다). 그 세션을 띄운 폴더를 품은
// 카드의 프로젝트만 그 세션의 것이다: cc 만 보면 p2 의 초인종이 p1 에서 뜬 세션에 간다.
func TestLiveSessionsOfDoesNotCrossProjectsOnOneCC(t *testing.T) {
	s := newStore(t)
	seed(t, s, "p1")
	seed(t, s, "p2")
	ctx := context.Background()
	if _, _, err := s.OpenSession(ctx, "p1", "m1", "/r/home", "cc-coord", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenSession(ctx, "p2", "m1", "/r/other", "cc-coord", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		project, dir string
		want         []string
	}{
		{"p1", "/r/home", []string{"cc-coord"}},
		{"p1", "/r/home/sub", []string{"cc-coord"}}, // 저장소 안 하위 폴더에서 띄운 세션
		{"p2", "/r/home", []string{}},               // 남의 프로젝트 카드는 그 세션을 그 프로젝트 것으로 만들지 않는다
		{"p1", "/r/homework", []string{}},           // 접두가 경로 조각 단위다
		{"p1", "", []string{}},                      // 폴더 없는 물음(옛 보드)은 답하지 않는다
		{"p1", "rel/home", []string{}},
	} {
		got, err := s.LiveSessionsOf(ctx, c.project, "m1", []LegacyAsk{{"cc-coord", c.dir}})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s · %q = %v, 기대 %v", c.project, c.dir, got, c.want)
		}
	}
}
