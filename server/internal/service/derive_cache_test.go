package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/gitreader"
	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store"
)

// 카드 파생의 비용이 카드 수에 비례하지 않는다는 것을 **git 호출 수**로 잠근다(derive_cache.go 머리말).
// 시간으로 재지 않는다, 시험 기계의 부하에 따라 흔들리는 수는 회귀를 못 가른다.

// derivCountingReader 는 실물 리더를 감싸 메서드별 호출 수를 세고, 시험이 원하면 메서드를
// 붙잡거나(gate, 느린 저장소 흉내) 실패시킨다(fail). 수는 gitCounter 한 자리에 모인다.
type derivCountingReader struct {
	GitReader
	g *gitCounter
}

func (c derivCountingReader) Worktrees(ctx context.Context) ([]gitreader.Worktree, error) {
	if err := c.g.hit("Worktrees"); err != nil {
		return nil, err
	}
	return c.GitReader.Worktrees(ctx)
}

func (c derivCountingReader) Ref(ctx context.Context, ref string) (model.RefState, error) {
	if err := c.g.hit("Ref"); err != nil {
		return model.RefState{}, err
	}
	return c.GitReader.Ref(ctx, ref)
}

func (c derivCountingReader) MergeBase(ctx context.Context, a, b string) (string, error) {
	if err := c.g.hit("MergeBase"); err != nil {
		return "", err
	}
	return c.GitReader.MergeBase(ctx, a, b)
}

func (c derivCountingReader) ChangedPaths(ctx context.Context, base, head string) ([]string, map[string]model.LineDelta, error) {
	if err := c.g.hit("ChangedPaths"); err != nil {
		return nil, nil, err
	}
	return c.GitReader.ChangedPaths(ctx, base, head)
}

func (c derivCountingReader) AheadBehind(ctx context.Context, ref, base string) (int, int, error) {
	if err := c.g.hit("AheadBehind"); err != nil {
		return 0, 0, err
	}
	return c.GitReader.AheadBehind(ctx, ref, base)
}

func (c derivCountingReader) UncommittedPaths(ctx context.Context, wt string) ([]string, error) {
	if err := c.g.hit("UncommittedPaths"); err != nil {
		return nil, err
	}
	return c.GitReader.UncommittedPaths(ctx, wt)
}

func (c derivCountingReader) UncommittedDelta(ctx context.Context, wt string) (map[string]model.LineDelta, error) {
	if err := c.g.hit("UncommittedDelta"); err != nil {
		return nil, err
	}
	return c.GitReader.UncommittedDelta(ctx, wt)
}

// gitCounter 는 Service 가 만드는 모든 리더의 호출 수를 한 자리에 모은다.
type gitCounter struct {
	mu   sync.Mutex
	n    map[string]int
	gate map[string]chan struct{} // 메서드 → 닫힐 때까지 붙잡는 문
	fail map[string]error         // 메서드 → 낼 실패
}

func (g *gitCounter) factory(repo string) GitReader {
	return derivCountingReader{GitReader: gitreader.New(repo), g: g}
}

// hit 는 호출 하나를 세고, 문이 걸려 있으면 열릴 때까지 기다리고, 실패가 걸려 있으면 낸다.
func (g *gitCounter) hit(m string) error {
	g.mu.Lock()
	g.n[m]++
	gate, err := g.gate[m], g.fail[m]
	g.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return err
}

// hold 는 메서드 m 을 붙잡는 문을 건다. 돌려준 함수가 연다(여러 번 불러도 된다). 시험이
// 도중에 멈춰도 뒤에서 붙잡힌 읽기가 정리(bg.Wait)를 막지 않게 정리 때도 연다.
func (g *gitCounter) hold(t *testing.T, m string) (open func()) {
	ch := make(chan struct{})
	var once sync.Once
	open = func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.gate, m)
			g.mu.Unlock()
			close(ch)
		})
	}
	g.mu.Lock()
	g.gate[m] = ch
	g.mu.Unlock()
	t.Cleanup(open)
	return open
}

func (g *gitCounter) failWith(m string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		delete(g.fail, m)
	} else {
		g.fail[m] = err
	}
}

// take 는 지금까지의 수를 내고 0 으로 되돌린다.
func (g *gitCounter) take() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]int{}
	for k, v := range g.n {
		out[k] = v
		delete(g.n, k)
	}
	return out
}

// newCountingSvc 는 git 호출을 세는 Service 를 만든다. opts 는 뒤에 더한다.
//
// 뒤에서 도는 읽기는 시험이 끝날 때 waitBehind 로 기다린다. 여기서 t.Cleanup 으로 걸면 그 뒤에
// 만든 저장소(t.TempDir)가 먼저 지워져(정리는 거꾸로 돈다) 도는 git 과 경합한다.
func newCountingSvc(t *testing.T, opts ...Option) (*Service, *store.Store, *gitCounter) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dbp := filepath.Join(t.TempDir(), "fd.db")
	if err := store.Migrate(context.Background(), dbp, nil); err != nil {
		t.Fatalf("DB 적용 실패: %v", err)
	}
	st, err := store.OpenWithLogger(dbp, log)
	if err != nil {
		t.Fatalf("DB 열기 실패: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	gc := &gitCounter{n: map[string]int{}, gate: map[string]chan struct{}{}, fail: map[string]error{}}
	svc := New(st, log, append([]Option{WithGitFactory(gc.factory)}, opts...)...)
	return svc, st, gc
}

// waitBehind 는 시험이 끝날 때 뒤에서 도는 읽기가 끝나기를 기다린다. 저장소를 만든 **뒤에**
// 부른다, 정리는 거꾸로 돌아 그래야 저장소 삭제보다 먼저 기다린다(DB 닫기보다도 먼저다).
func waitBehind(t *testing.T, s *Service) { t.Cleanup(s.dc.bg.Wait) }

// cardPaths 는 세션 id → 경로 목록이다(비교용).
func cardPaths(cards []SessionCard) map[string][]string {
	out := map[string][]string{}
	for _, c := range cards {
		p := append([]string(nil), c.View.Paths...)
		sort.Strings(p)
		out[c.View.Session.ID] = p
	}
	return out
}

// 워크트리 nWT 개에 카드를 둘씩(같은 워크트리를 쥔 두 대화) 올리고, 브랜치마다 커밋 하나와
// 미커밋 파일 하나를 둔다. 그리고 보드를 두 번, 판정 자리(sessionCards)를 한 번 부른다.
//
//	첫 보드  , 커밋 쌍 셋(MergeBase · ChangedPaths · AheadBehind)은 브랜치마다 한 번,
//	            미커밋 둘은 워크트리마다 한 번. 카드 수(2×nWT)가 아니다.
//	둘째 보드, 카드마다 드는 git 이 **0** 이다(목록 · 기본 브랜치 tip 둘만 남는다).
//	판정 자리, 커밋 쌍은 기억에서, 미커밋은 워크트리마다 지금 읽는다.
//
// 워크트리 수를 두 벌(3 · 12)로 돌려 둘째 보드의 수가 **같다**는 것까지 본다, 그것이
// 「카드 · 워크트리가 열 배가 돼도」의 단정이다.
func TestBoardDeriveCostDoesNotGrowWithCards(t *testing.T) {
	var warm []map[string]int
	for _, nWT := range []int{3, 12} {
		t.Run(fmt.Sprintf("worktrees=%d", nWT), func(t *testing.T) {
			s, _, gc := newCountingSvc(t, WithScreenSnapshots(time.Hour, 2*time.Hour, time.Minute))
			repo := newRepo(t)
			waitBehind(t, s)
			base := filepath.Dir(repo)
			var self string
			for i := 0; i < nWT; i++ {
				br := fmt.Sprintf("feat-%02d", i)
				wt := filepath.Join(base, "wt-"+br)
				runGit(t, repo, "worktree", "add", "-q", "-b", br, wt)
				writeFile(t, wt, fmt.Sprintf("src/c%02d.go", i), "package x\n")
				runGit(t, wt, "add", "-A")
				runGit(t, wt, "commit", "-q", "-m", "c")
				writeFile(t, wt, fmt.Sprintf("src/u%02d.go", i), "package x\n")
				for k := 0; k < 2; k++ {
					res := openSession(t, s, "p", repo, wt, fmt.Sprintf("cc-%02d-%d", i, k), br)
					self = res.Session.ID
				}
			}
			gc.take() // 세션 열기가 부른 것은 세지 않는다

			first, err := s.Board(ctx(), "p", BoardOptions{Self: self})
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Sessions) != 2*nWT {
				t.Fatalf("카드 %d장, 기대 %d장", len(first.Sessions), 2*nWT)
			}
			if len(first.Failures) != 0 {
				t.Fatalf("파생 실패: %+v", first.Failures)
			}
			got := gc.take()
			for m, want := range map[string]int{
				"MergeBase": nWT, "ChangedPaths": nWT, "AheadBehind": nWT,
				"UncommittedPaths": nWT, "UncommittedDelta": nWT, "Worktrees": 1, "Ref": 1,
			} {
				if got[m] != want {
					t.Errorf("첫 보드 %s %d번, 기대 %d번(카드 %d장 · 워크트리 %d)", m, got[m], want, 2*nWT, nWT)
				}
			}
			for _, c := range first.Sessions {
				if len(c.View.Paths) != 2 || !c.AheadKnown || c.View.AheadMain != 1 {
					t.Fatalf("카드 %s 파생이 틀렸다: paths=%v ahead=%d(%v)", c.View.Session.Label,
						c.View.Paths, c.View.AheadMain, c.AheadKnown)
				}
			}

			second, err := s.Board(ctx(), "p", BoardOptions{Self: self})
			if err != nil {
				t.Fatal(err)
			}
			got = gc.take()
			warm = append(warm, got)
			if want := map[string]int{}; !equalCounts(got, want) {
				t.Errorf("둘째 보드 git %v, 기대 %v(목록 · tip 도 보관본), 카드마다 git 이 다시 돈다", got, want)
			}
			if a, b := cardPaths(first.Sessions), cardPaths(second.Sessions); fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("둘째 보드의 경로가 첫 보드와 다르다:\n첫 %v\n둘 %v", a, b)
			}

			proj, err := s.st.GetProject(ctx(), "p")
			if err != nil {
				t.Fatal(err)
			}
			d := &derive{}
			if _, err := s.sessionCards(ctx(), proj, s.cut(s.now(), 0), self, d); err != nil {
				t.Fatal(err)
			}
			got = gc.take()
			for m, want := range map[string]int{
				"MergeBase": 0, "ChangedPaths": 0, "AheadBehind": 0,
				"UncommittedPaths": nWT, "UncommittedDelta": nWT,
			} {
				if got[m] != want {
					t.Errorf("판정 자리 %s %d번, 기대 %d번", m, got[m], want)
				}
			}
		})
	}
	if len(warm) == 2 && !equalCounts(warm[0], warm[1]) {
		t.Errorf("워크트리 3 → 12 에서 둘째 보드의 git 수가 달라졌다: %v → %v", warm[0], warm[1])
	}
}

func equalCounts(a, b map[string]int) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

// 화면 보관본의 세 갈래를 시계로 잠근다.
//
//	refreshAfter 안  , 보관본을 쓴다. 관측 시각이 그 보관본의 것이다(지금이라고 안 한다).
//	refreshAfter 넘음, 보관본을 쓰면서 뒤에서 새로 읽고, 다음 보드가 그 값을 쓴다.
//	maxAge 넘음      , 보관본을 안 쓰고 그 자리에서 읽는다.
//
// 판정 자리(sessionCards)는 보관본과 무관하게 언제나 지금 읽는다.
func TestScreenSnapshotServesObservedStateAndRefreshesBehind(t *testing.T) {
	var mu sync.Mutex
	t0 := time.Now().UTC()
	now := t0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	set := func(d time.Duration) { mu.Lock(); now = t0.Add(d); mu.Unlock() }

	s, _, _ := newCountingSvc(t, WithClock(clock), WithScreenSnapshots(5*time.Second, time.Minute, time.Minute))
	repo, wt := newRepoWithWorktree(t, "feat")
	waitBehind(t, s)
	sess := openSession(t, s, "p", repo, wt, "cc-1", "feat")
	board := func() BoardView {
		t.Helper()
		v, err := s.Board(ctx(), "p", BoardOptions{Self: sess.Session.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Sessions) != 1 {
			t.Fatalf("카드 %d장", len(v.Sessions))
		}
		return v
	}
	has := func(v BoardView, p string) bool { return contains(v.Sessions[0].View.Paths, p) }

	if v := board(); has(v, "a.txt") || !v.Freshness.ObservedAt.Equal(t0) {
		t.Fatalf("첫 보드: paths=%v observed=%v", v.Sessions[0].View.Paths, v.Freshness.ObservedAt)
	}
	writeFile(t, wt, "a.txt", "a\n")

	set(2 * time.Second)
	if v := board(); has(v, "a.txt") || !v.Freshness.ObservedAt.Equal(t0) {
		t.Fatalf("refreshAfter 안: 보관본(t0)을 써야 하고 관측 시각도 t0 이어야 한다: paths=%v observed=%v",
			v.Sessions[0].View.Paths, v.Freshness.ObservedAt)
	}

	set(10 * time.Second)
	if v := board(); has(v, "a.txt") {
		t.Fatalf("refreshAfter 넘음: 이 응답은 아직 보관본이어야 한다(새로 읽기는 뒤에서 돈다): %v", v.Sessions[0].View.Paths)
	}
	s.dc.bg.Wait()
	set(11 * time.Second)
	if v := board(); !has(v, "a.txt") || !v.Freshness.ObservedAt.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("뒤에서 새로 읽은 값을 다음 보드가 써야 한다: paths=%v observed=%v",
			v.Sessions[0].View.Paths, v.Freshness.ObservedAt)
	}

	// 판정 자리는 지금 읽는다, 그리고 그 읽기가 보관본을 새로 고친다.
	writeFile(t, wt, "b.txt", "b\n")
	proj, err := s.st.GetProject(ctx(), "p")
	if err != nil {
		t.Fatal(err)
	}
	cards, err := s.sessionCards(ctx(), proj, s.cut(clock(), 0), sess.Session.ID, &derive{})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(cards[0].View.Paths, "b.txt") {
		t.Fatalf("판정 자리가 보관본을 썼다: %v", cards[0].View.Paths)
	}
	if v := board(); !has(v, "b.txt") {
		t.Fatalf("판정 자리의 읽기가 보관본을 안 고쳤다: %v", v.Sessions[0].View.Paths)
	}

	// maxAge 를 넘으면 화면도 그 자리에서 읽는다.
	writeFile(t, wt, "c.txt", "c\n")
	set(5 * time.Minute)
	if v := board(); !has(v, "c.txt") || !v.Freshness.ObservedAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("maxAge 넘음: 지금 읽어야 한다: paths=%v observed=%v", v.Sessions[0].View.Paths, v.Freshness.ObservedAt)
	}
}

// 보관본을 켜지 않은 Service(New 의 기본)는 화면도 언제나 지금 읽는다, 다른 진입점과 시험이
// 기대는 성질이다.
func TestBoardWithoutScreenSnapshotsReadsNow(t *testing.T) {
	s, _, gc := newCountingSvc(t)
	repo, wt := newRepoWithWorktree(t, "feat")
	waitBehind(t, s)
	sess := openSession(t, s, "p", repo, wt, "cc-1", "feat")
	if _, err := s.Board(ctx(), "p", BoardOptions{Self: sess.Session.ID}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, wt, "a.txt", "a\n")
	gc.take()
	v, err := s.Board(ctx(), "p", BoardOptions{Self: sess.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(v.Sessions[0].View.Paths, "a.txt") {
		t.Fatalf("기본 Service 의 보드가 보관본을 썼다: %v", v.Sessions[0].View.Paths)
	}
	if got := gc.take(); got["UncommittedPaths"] != 1 || got["UncommittedDelta"] != 1 {
		t.Fatalf("기본 Service 의 보드가 미커밋을 지금 안 읽었다: %v", got)
	}
}

// newSlowScreen 은 화면 보관본이 켜지고 기다림 상한이 짧은 Service 와, 미커밋 경로 읽기를 붙잡은
// 저장소 하나 · 세션 하나를 만든다. open 이 붙잡힌 읽기를 놓는다.
func newSlowScreen(t *testing.T, held string) (s *Service, gc *gitCounter, repo, wt string, sess model.Session, open func()) {
	t.Helper()
	s, _, gc = newCountingSvc(t, WithScreenSnapshots(time.Hour, 2*time.Hour, 50*time.Millisecond))
	repo, wt = newRepoWithWorktree(t, "feat")
	waitBehind(t, s)
	writeFile(t, wt, "a.txt", "a\n")
	sess = openSession(t, s, "p", repo, wt, "cc-1", "feat").Session
	gc.take()
	open = gc.hold(t, held)
	return s, gc, repo, wt, sess, open
}

func boardOf(t *testing.T, s *Service, self string) BoardView {
	t.Helper()
	v, err := s.Board(ctx(), "p", BoardOptions{Self: self})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Sessions) != 1 {
		t.Fatalf("카드 %d장", len(v.Sessions))
	}
	return v
}

// 화면은 보관본 없는 읽기를 ScreenWait 까지만 기다린다. 넘으면 그 카드를 「관측 중」으로 고백하고
// (조용히 비우지 않고, 「못 읽었다」 · 「안 막는다」 라고도 안 한다), 읽기는 요청과 떨어져 끝까지
// 돌아 다음 보드가 그 값을 쓴다.
func TestScreenWaitIsBoundedAndSaysWhatIsStillReading(t *testing.T) {
	s, _, _, _, sess, open := newSlowScreen(t, "UncommittedPaths")

	start := time.Now()
	v := boardOf(t, s, sess.ID)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("보드가 붙잡힌 읽기를 기다렸다(%v)", took)
	}
	var axis string
	for _, f := range v.Failures {
		if f.Axis == "uncommitted:"+sess.ID {
			axis = f.Detail
		}
	}
	if axis != errStillReading.Error() || !v.Freshness.Stale {
		t.Fatalf("못 끝난 읽기를 「관측 중」으로 고백해야 한다: axis=%q stale=%v failures=%+v",
			axis, v.Freshness.Stale, v.Failures)
	}
	c := v.Sessions[0]
	if !c.Observing || c.DeriveError != errStillReading.Error() {
		t.Fatalf("카드는 축마다 「못 읽었다」가 아니라 「관측 중」 한 번이어야 한다: observing=%v derive_error=%q",
			c.Observing, c.DeriveError)
	}

	open()
	s.dc.bg.Wait()
	v = boardOf(t, s, sess.ID)
	if len(v.Failures) != 0 || !contains(v.Sessions[0].View.Paths, "a.txt") || v.Sessions[0].Observing {
		t.Fatalf("뒤에서 끝난 읽기를 다음 보드가 써야 한다: paths=%v failures=%+v",
			v.Sessions[0].View.Paths, v.Failures)
	}
}

// 판정 자리는 같은 느린 읽기라도 기다린다, 답을 비우지 않는다.
func TestDecisionPathWaitsForASlowRead(t *testing.T) {
	s, _, _, _, sess, open := newSlowScreen(t, "UncommittedPaths")
	proj, err := s.st.GetProject(ctx(), "p")
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, open)
	d := &derive{}
	cards, err := s.sessionCards(ctx(), proj, s.cut(s.now(), 0), sess.ID, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.failures) != 0 || len(cards) != 1 || !contains(cards[0].View.Paths, "a.txt") {
		t.Fatalf("판정 자리가 느린 읽기를 안 기다렸다: failures=%+v cards=%d", d.failures, len(cards))
	}
}

// 화면이 기다림을 넘긴 커밋 쌍도 뒤에서 끝나면 변경집합(change_set)에 들어간다. 조립 단계에서
// 보관하던 앞 판은 늦게 끝난 쌍을 영영 안 남겼다(그 뒤로는 기억에서 꺼내 다시 안 읽으므로).
func TestLateFinishedPairIsRemembered(t *testing.T) {
	s, _, repo, wt, sess, open := newSlowScreen(t, "MergeBase")
	forkSHA := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	writeFile(t, wt, "branch/only.py", "print(1)\n")
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-q", "-m", "b")
	headSHA := strings.TrimSpace(runGit(t, wt, "rev-parse", "HEAD"))

	if v := boardOf(t, s, sess.ID); !v.Sessions[0].Observing {
		t.Fatalf("붙잡힌 갈래 지점 읽기가 「관측 중」이 아니다: %+v", v.Failures)
	}
	open()
	s.dc.bg.Wait()
	if _, err := s.st.GetChangeSet(ctx(), "p", forkSHA, headSHA); err != nil {
		t.Fatalf("뒤에서 끝난 커밋 쌍이 change_set 에 없다: %v", err)
	}
	if v := boardOf(t, s, sess.ID); !contains(v.Sessions[0].View.Paths, "branch/only.py") {
		t.Fatalf("다음 보드가 기억한 쌍을 안 썼다: %v", v.Sessions[0].View.Paths)
	}
}

// 첫 읽기가 기다림을 넘는 동안 보드가 여러 번 와도 같은 워크트리 · 같은 커밋 쌍은 한 번만 읽는다
// (도는 읽기에 올라탄다). 앞 판은 보드마다 새 git 을 띄워, 느린 기계에서 밀린 읽기가 쌓였다.
func TestInflightFirstReadsAreShared(t *testing.T) {
	s, gc, _, _, sess, open := newSlowScreen(t, "UncommittedPaths")
	closeMB := gc.hold(t, "MergeBase")
	for i := 0; i < 4; i++ {
		boardOf(t, s, sess.ID)
	}
	open()
	closeMB()
	s.dc.bg.Wait()
	got := gc.take()
	if got["UncommittedPaths"] != 1 || got["MergeBase"] != 1 {
		t.Fatalf("보드 4장이 붙잡힌 첫 읽기를 따로 띄웠다: %v", got)
	}
}

// 등록된 워크트리의 하위 디렉토리를 쥔 카드(예: <wt>/server)도 보관본을 쓴다. 워크트리 목록으로
// 보관본을 거르던 앞 판은 그 경로를 목록에 없다고 매번 지웠다.
func TestSubdirCardSnapshotIsKept(t *testing.T) {
	s, _, gc := newCountingSvc(t, WithScreenSnapshots(time.Hour, 2*time.Hour, time.Minute))
	repo, wt := newRepoWithWorktree(t, "feat")
	waitBehind(t, s)
	sub := filepath.Join(wt, "server")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	sess := openSession(t, s, "p", repo, sub, "cc-1", "x")
	gc.take()
	for i := 0; i < 3; i++ {
		boardOf(t, s, sess.Session.ID)
	}
	if got := gc.take(); got["UncommittedPaths"] != 1 {
		t.Fatalf("하위 디렉토리 카드를 보드 3장이 %d번 읽었다(보관본이 매번 지워진다)", got["UncommittedPaths"])
	}
}

// 기억의 키가 sha 쌍이라는 것을 잠근다. 같은 브랜치 이름이 앞으로 가거나(새 커밋) 기본 브랜치가
// 그 브랜치를 빨아들이면(랜딩) 화면은 목록 · tip 의 새로 읽기 다음 보드에서 새 답을 낸다
// (옛 쌍의 기억을 새 tip 에 쓰지 않는다). 판정 자리는 그 자리에서 본다.
func TestMemoFollowsBranchMoveAndLanding(t *testing.T) {
	var mu sync.Mutex
	t0 := time.Now().UTC()
	now := t0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	step := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	s, _, _ := newCountingSvc(t, WithClock(clock), WithScreenSnapshots(5*time.Second, time.Hour, time.Minute))
	repo, wt := newRepoWithWorktree(t, "feat")
	waitBehind(t, s)
	commit := func(name string) {
		writeFile(t, wt, name, "1\n")
		runGit(t, wt, "add", "-A")
		runGit(t, wt, "commit", "-q", "-m", name)
	}
	commit("a.py")
	sess := openSession(t, s, "p", repo, wt, "cc-1", "x")
	// screen 은 refreshAfter 를 넘긴 뒤 한 장(뒤에서 새로 읽음) + 한 장(그 값을 씀)이다.
	screen := func() SessionCard {
		t.Helper()
		step(10 * time.Second)
		boardOf(t, s, sess.Session.ID)
		s.dc.bg.Wait()
		step(time.Second)
		return boardOf(t, s, sess.Session.ID).Sessions[0]
	}
	decision := func() SessionCard {
		t.Helper()
		proj, err := s.st.GetProject(ctx(), "p")
		if err != nil {
			t.Fatal(err)
		}
		cards, err := s.sessionCards(ctx(), proj, s.cut(clock(), 0), sess.Session.ID, &derive{})
		if err != nil {
			t.Fatal(err)
		}
		return cards[0]
	}
	if c := screen(); c.View.AheadMain != 1 {
		t.Fatalf("ahead=%d", c.View.AheadMain)
	}
	commit("b.py")
	if c := decision(); !contains(c.View.Paths, "b.py") || c.View.AheadMain != 2 {
		t.Fatalf("판정 자리가 브랜치가 앞으로 간 것을 못 봤다: paths=%v ahead=%d", c.View.Paths, c.View.AheadMain)
	}
	if c := screen(); !contains(c.View.Paths, "b.py") || c.View.AheadMain != 2 {
		t.Fatalf("화면이 브랜치가 앞으로 간 것을 못 봤다: paths=%v ahead=%d", c.View.Paths, c.View.AheadMain)
	}
	runGit(t, repo, "merge", "-q", "--ff-only", "feat")
	if c := decision(); c.View.AheadMain != 0 || contains(c.View.Paths, "a.py") {
		t.Fatalf("판정 자리가 랜딩을 못 봤다: paths=%v ahead=%d", c.View.Paths, c.View.AheadMain)
	}
	if c := screen(); c.View.AheadMain != 0 || contains(c.View.Paths, "a.py") {
		t.Fatalf("화면이 랜딩을 못 봤다: paths=%v ahead=%d", c.View.Paths, c.View.AheadMain)
	}
}

// 요청 ctx 가 응답 뒤 끊겨도(HTTP 처럼) 뒤에서 이어진 첫 읽기는 보관본에 들어간다.
func TestCancelledRequestStillFillsSnapshot(t *testing.T) {
	s, gc, _, _, sess, open := newSlowScreen(t, "UncommittedPaths")
	rctx, cancel := context.WithCancel(context.Background())
	if _, err := s.Board(rctx, "p", BoardOptions{Self: sess.ID}); err != nil {
		t.Fatal(err)
	}
	cancel()
	open()
	s.dc.bg.Wait()
	gc.take()
	v := boardOf(t, s, sess.ID)
	if got := gc.take(); got["UncommittedPaths"] != 0 || !contains(v.Sessions[0].View.Paths, "a.txt") {
		t.Fatalf("끊긴 요청 뒤 보관본이 안 찼다: reads=%v paths=%v failures=%v", got, v.Sessions[0].View.Paths, v.Failures)
	}
}

// 뒤의 새로 읽기가 실패해도 성한 보관본을 안 덮는다(부하로 git 한 번이 시간을 넘긴 것이 화면의
// 경로를 지우면 안 된다). 대신 관측이 늙어 가고, snapshotStaleAfter 를 넘으면 「낡음」이 된다.
// 관측 시각은 여러 워크트리 중 **가장 오래된** 것이다.
func TestFailedRefreshKeepsGoodSnapshotAndAges(t *testing.T) {
	var mu sync.Mutex
	t0 := time.Now().UTC()
	now := t0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	set := func(d time.Duration) { mu.Lock(); now = t0.Add(d); mu.Unlock() }

	s, _, gc := newCountingSvc(t, WithClock(clock), WithScreenSnapshots(5*time.Second, time.Hour, time.Minute))
	repo := newRepo(t)
	waitBehind(t, s)
	base := filepath.Dir(repo)
	var ids []string
	for _, br := range []string{"f1", "f2"} {
		wt := filepath.Join(base, "wt-"+br)
		runGit(t, repo, "worktree", "add", "-q", "-b", br, wt)
		writeFile(t, wt, br+".txt", "x\n")
		ids = append(ids, openSession(t, s, "p", repo, wt, "cc-"+br, br).Session.ID)
	}
	board := func() BoardView {
		t.Helper()
		v, err := s.Board(ctx(), "p", BoardOptions{Self: ids[0]})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := board(); len(v.Failures) != 0 || v.Freshness.Stale {
		t.Fatalf("첫 보드: %+v", v.Failures)
	}

	// 10초 뒤 화면이 두 워크트리를 뒤에서 새로 읽게 하되, 그 읽기를 실패시킨다.
	gc.failWith("UncommittedPaths", errors.New("시간 초과 흉내"))
	set(10 * time.Second)
	if v := board(); len(v.Failures) != 0 {
		t.Fatalf("이 응답은 보관본이어야 한다: %+v", v.Failures)
	}
	s.dc.bg.Wait()
	set(40 * time.Second)
	v := board()
	if len(v.Failures) != 0 {
		t.Fatalf("실패한 새로 읽기가 성한 보관본을 덮었다: %+v", v.Failures)
	}
	for _, c := range v.Sessions {
		if len(c.View.Paths) != 1 {
			t.Fatalf("실패한 새로 읽기가 경로를 지웠다: %s %v", c.View.Session.Label, c.View.Paths)
		}
	}
	if !v.Freshness.ObservedAt.Equal(t0) || !v.Freshness.Stale {
		t.Fatalf("40초 된 관측은 그 시각과 「낡음」으로 나야 한다: observed=%v stale=%v",
			v.Freshness.ObservedAt, v.Freshness.Stale)
	}
	s.dc.bg.Wait()

	// 실패가 풀리면 뒤의 새로 읽기가 다시 채운다. 관측 시각은 둘 중 오래된 쪽이다.
	gc.failWith("UncommittedPaths", nil)
	set(50 * time.Second)
	board()
	s.dc.bg.Wait()
	set(51 * time.Second)
	if v := board(); !v.Freshness.ObservedAt.Equal(t0.Add(50*time.Second)) || v.Freshness.Stale {
		t.Fatalf("새로 읽은 뒤: observed=%v stale=%v", v.Freshness.ObservedAt, v.Freshness.Stale)
	}
}

// 화면이 보관본 워크트리 목록을 써도 카드 닫기는 안전하다. 보관본보다 뒤에 생긴 워크트리의 카드는
// (디렉토리가 있으니) 안 닫히고, 지워진 워크트리의 카드는 뒤의 새로 읽기 다음 보드에서 닫힌다.
func TestStaleWorktreeListNeverClosesALiveCard(t *testing.T) {
	var mu sync.Mutex
	t0 := time.Now().UTC()
	now := t0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	set := func(d time.Duration) { mu.Lock(); now = t0.Add(d); mu.Unlock() }

	s, _, _ := newCountingSvc(t, WithClock(clock), WithScreenSnapshots(5*time.Second, time.Hour, time.Minute))
	repo, wt := newRepoWithWorktree(t, "feat")
	waitBehind(t, s)
	first := openSession(t, s, "p", repo, wt, "cc-1", "feat").Session.ID
	board := func() BoardView {
		t.Helper()
		v, err := s.Board(ctx(), "p", BoardOptions{Self: first})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	board() // 목록 보관본(워크트리 feat 만)

	wt2 := filepath.Join(filepath.Dir(repo), "wt-late")
	runGit(t, repo, "worktree", "add", "-q", "-b", "late", wt2)
	late := openSession(t, s, "p", repo, wt2, "cc-2", "late").Session.ID
	set(time.Second) // refreshAfter 안: 보관본 목록에는 wt-late 가 없다
	v := board()
	if len(v.ClosedGoneWorktree) != 0 {
		t.Fatalf("보관본 목록에 없다고 산 카드를 닫았다: %+v", v.ClosedGoneWorktree)
	}
	found := false
	for _, c := range v.Sessions {
		found = found || c.View.Session.ID == late
	}
	if !found {
		t.Fatalf("뒤에 생긴 워크트리의 카드가 빠졌다")
	}
}
