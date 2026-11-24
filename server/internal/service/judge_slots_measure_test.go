package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/gitreader"
	"github.com/kweiza/flightdeck/internal/model"
)

// 화면 읽기를 몰아칠 때 판정 자리(sessionCards, pick · amend · finish 가 지난다)가 얼마나 기다리는지를
// 잰다. 시간을 재는 측정이라 기본으로는 안 돈다(시험 기계 부하로 흔들린다), 회귀는
// TestJudgeReadsDoNotQueueBehindScreen 이 시간 없이 잠근다.
//
//	FD_MEASURE_JUDGE=1 go test ./internal/service -run TestMeasureJudgeLatencyUnderScreenLoad -v
//
// git 한 번을 FD_MEASURE_DELAY(기본 100ms)만큼 늦춘다, 부하 높은 호스트에서 git 한 번이 0.1~1.8초였다
// (derive_cache.go 머리말). 판정 프로젝트 하나와 화면 프로젝트 셋(워크트리 18개씩, 실측 보드와 같은 수),
// 화면 쪽은 보드 6장이 쉬지 않고 부른다(새로 읽기 간격 0, 부를 때마다 뒤에서 새로 읽는다).
func TestMeasureJudgeLatencyUnderScreenLoad(t *testing.T) {
	if os.Getenv("FD_MEASURE_JUDGE") == "" {
		t.Skip("FD_MEASURE_JUDGE=1 일 때만 잰다")
	}
	delay := 100 * time.Millisecond
	if v := os.Getenv("FD_MEASURE_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
		delay = d
	}
	const nWT, screens, boards, samples = 18, 3, 6, 15

	s, _, _ := newCountingSvc(t, WithScreenSnapshots(0, time.Hour, 2*time.Second),
		WithGitFactory(func(repo string) GitReader { return slowGit{GitReader: gitreader.New(repo), d: delay} }))
	var self string
	mk := func(id string) {
		repo := newRepo(t)
		base := filepath.Dir(repo)
		for i := 0; i < nWT; i++ {
			br := fmt.Sprintf("feat-%02d", i)
			wt := filepath.Join(base, "wt-"+br)
			runGit(t, repo, "worktree", "add", "-q", "-b", br, wt)
			writeFile(t, wt, fmt.Sprintf("u%02d.go", i), "package x\n")
			self = openSession(t, s, id, repo, wt, fmt.Sprintf("cc-%s-%02d", id, i), br).Session.ID
		}
	}
	mk("judge")
	judgeSelf := self
	for i := 0; i < screens; i++ {
		mk(fmt.Sprintf("screen%d", i))
	}
	waitBehind(t, s)
	proj, err := s.st.GetProject(ctx(), "judge")
	if err != nil {
		t.Fatal(err)
	}
	judge := func() time.Duration {
		start := time.Now()
		if _, err := s.sessionCards(ctx(), proj, s.cut(s.now(), 0), judgeSelf, &derive{}); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	measure := func() []time.Duration {
		var out []time.Duration
		for i := 0; i < samples; i++ {
			out = append(out, judge())
			time.Sleep(50 * time.Millisecond)
		}
		return out
	}
	for i := 0; i < screens; i++ { // 화면 보관본을 한 번 채운다(첫 읽기는 재지 않는다)
		_, _ = s.Board(ctx(), fmt.Sprintf("screen%d", i), BoardOptions{})
	}
	s.dc.bg.Wait()
	judge()
	idle := measure()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var calls atomic.Int64
	for b := 0; b < boards; b++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := b; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = s.Board(context.Background(), fmt.Sprintf("screen%d", i%screens), BoardOptions{})
				calls.Add(1)
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	time.Sleep(2 * time.Second) // 뒤의 새로 읽기가 자리를 채울 때까지
	loaded := measure()
	close(stop)
	wg.Wait()
	s.dc.bg.Wait()

	t.Logf("MEASURE delay=%v worktrees=%d screen_projects=%d boards=%d board_calls=%d", delay, nWT, screens, boards, calls.Load())
	t.Logf("MEASURE idle   %s", summary(idle))
	t.Logf("MEASURE loaded %s", summary(loaded))
}

func summary(ds []time.Duration) string {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	q := func(p float64) time.Duration { return s[int(p*float64(len(s)-1))].Round(time.Millisecond) }
	return fmt.Sprintf("n=%d p50=%v p90=%v max=%v", len(s), q(0.5), q(0.9), s[len(s)-1].Round(time.Millisecond))
}

// slowGit 은 git 한 번을 d 만큼 늦춘다(부하 높은 호스트 흉내). 판정 · 화면 모두 같은 지연이다.
type slowGit struct {
	GitReader
	d time.Duration
}

func (g slowGit) wait(ctx context.Context) error {
	select {
	case <-time.After(g.d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g slowGit) Worktrees(ctx context.Context) ([]gitreader.Worktree, error) {
	if err := g.wait(ctx); err != nil {
		return nil, err
	}
	return g.GitReader.Worktrees(ctx)
}

func (g slowGit) Ref(ctx context.Context, ref string) (model.RefState, error) {
	if err := g.wait(ctx); err != nil {
		return model.RefState{}, err
	}
	return g.GitReader.Ref(ctx, ref)
}

func (g slowGit) MergeBase(ctx context.Context, a, b string) (string, error) {
	if err := g.wait(ctx); err != nil {
		return "", err
	}
	return g.GitReader.MergeBase(ctx, a, b)
}

func (g slowGit) ChangedPaths(ctx context.Context, base, head string) ([]string, map[string]model.LineDelta, error) {
	if err := g.wait(ctx); err != nil {
		return nil, nil, err
	}
	return g.GitReader.ChangedPaths(ctx, base, head)
}

func (g slowGit) AheadBehind(ctx context.Context, ref, base string) (int, int, error) {
	if err := g.wait(ctx); err != nil {
		return 0, 0, err
	}
	return g.GitReader.AheadBehind(ctx, ref, base)
}

func (g slowGit) UncommittedPaths(ctx context.Context, wt string) ([]string, error) {
	if err := g.wait(ctx); err != nil {
		return nil, err
	}
	return g.GitReader.UncommittedPaths(ctx, wt)
}

func (g slowGit) UncommittedDelta(ctx context.Context, wt string) (map[string]model.LineDelta, error) {
	if err := g.wait(ctx); err != nil {
		return nil, err
	}
	return g.GitReader.UncommittedDelta(ctx, wt)
}
