package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/gitreader"
	"github.com/kweiza/flightdeck/internal/judge"
	"github.com/kweiza/flightdeck/internal/model"
)

// 카드 파생 밖의 git(세션 열기 · pick 의 선행 sha · finish 의 선행 후보 · 진단)도 git 자리에 선다는 것을
// **동시에 뜬 git 수**로 잠근다. 시간으로 재지 않는다(git 한 번을 늦춰 겹치게만 한다).

// peakGit 은 카드 파생 밖에서 부르는 메서드가 동시에 몇 개 돌았는지 잰다. 한 번을 d 만큼 늦춰
// 겹친 요청이 실제로 겹치게 한다.
type peakGit struct {
	GitReader
	p *peakCounter
}

type peakCounter struct {
	d          time.Duration
	now, peak  atomic.Int64
	sawOutside atomic.Int64
}

func (c *peakCounter) enter() func() {
	c.sawOutside.Add(1)
	n := c.now.Add(1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(c.d)
	return func() { c.now.Add(-1) }
}

func (g peakGit) Refs(ctx context.Context) ([]model.RefState, error) {
	defer g.p.enter()()
	return g.GitReader.Refs(ctx)
}

func (g peakGit) Ref(ctx context.Context, ref string) (model.RefState, error) {
	defer g.p.enter()()
	return g.GitReader.Ref(ctx, ref)
}

func (g peakGit) Worktrees(ctx context.Context) ([]gitreader.Worktree, error) {
	defer g.p.enter()()
	return g.GitReader.Worktrees(ctx)
}

func (g peakGit) Ancestry(ctx context.Context, sha, tip string) (judge.AncestryResult, error) {
	defer g.p.enter()()
	return g.GitReader.Ancestry(ctx, sha, tip)
}

func (g peakGit) FileAt(ctx context.Context, ref, path string) ([]byte, error) {
	defer g.p.enter()()
	return g.GitReader.FileAt(ctx, ref, path)
}

// fillSlots 는 sem 을 다 쥔다. 돌려준 함수가 놓는다(정리 때도 놓는다).
func fillSlots(t *testing.T, sem chan struct{}) {
	t.Helper()
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			for i := 0; i < cap(sem); i++ {
				<-sem
			}
		})
	})
}

// TestOutsideDeriveGitIsBounded 는 카드 파생 밖 git 의 동시 실행 수가 제 자리의 상한을 안 넘고,
// 다른 자리가 꽉 차도 그 뒤에 서지 않는다는 것을 잠근다.
//
//   - 세션 열기(훅마다 온다: UserPromptSubmit · Edit/Write 뒤 비동기 · Stop · PreCompact) · 진단은
//     세션 자리(sessionParallel). 판정 · 화면 자리가 꽉 차도 돈다.
//   - pick 의 선행 sha · finish 의 선행 후보는 판정 자리(judgeParallel). 화면 · 세션 자리가 꽉 차도 돈다.
//
// 요청 하나 안에서는 이 git 들이 차례로 돈다(갈래가 없다). 그래서 상한이 없으면 동시 수 = 겹친 요청 수다,
// 여기서는 자리 수의 세 배를 겹친다.
func TestOutsideDeriveGitIsBounded(t *testing.T) {
	pc := &peakCounter{d: 20 * time.Millisecond}
	s, _, _ := newCountingSvc(t, WithGitFactory(func(repo string) GitReader {
		return peakGit{GitReader: gitreader.New(repo), p: pc}
	}))
	repo, wt := newRepoWithWorktree(t, "feat")
	waitBehind(t, s)
	sess := openSession(t, s, "p", repo, wt, "cc-0", "feat").Session
	proj, err := s.st.GetProject(ctx(), "p")
	if err != nil {
		t.Fatal(err)
	}
	mainSHA := strings.TrimSpace(runGit(t, repo, "rev-parse", proj.DefaultBranch))
	// finish 의 선행 후보가 조상 판정까지 가게, 브랜치를 기본 브랜치 tip 에 둔다(조상이다).
	runGit(t, wt, "reset", "-q", "--hard", mainSHA)

	paths := []struct {
		name  string
		lane  func() chan struct{} // 이 길이 서는 자리
		other func() []chan struct{}
		call  func(i int, c context.Context) []DerivedFailure
	}{
		{
			name:  "세션 열기",
			lane:  func() chan struct{} { return s.dc.sessionSem },
			other: func() []chan struct{} { return []chan struct{}{s.dc.judgeSem, s.dc.screenSem} },
			call: func(i int, c context.Context) []DerivedFailure {
				res, err := s.OpenSession(c, OpenSessionInput{
					Project: "p", ProjectPath: repo, MachineID: "m1", Hostname: "testhost",
					Worktree: wt, CCSessionID: fmt.Sprintf("cc-%d", i+1), Label: "feat",
				})
				if err != nil {
					return []DerivedFailure{{Axis: "open", Detail: err.Error()}}
				}
				return res.Failures
			},
		},
		{
			name:  "진단",
			lane:  func() chan struct{} { return s.dc.sessionSem },
			other: func() []chan struct{} { return []chan struct{}{s.dc.judgeSem, s.dc.screenSem} },
			call: func(_ int, c context.Context) []DerivedFailure {
				for _, a := range s.Doctor(c).Projects {
					if !a.Readable || a.Worktrees == 0 {
						return []DerivedFailure{{Axis: "doctor", Detail: a.Detail}}
					}
				}
				return nil
			},
		},
		{
			name:  "pick 선행 sha",
			lane:  func() chan struct{} { return s.dc.judgeSem },
			other: func() []chan struct{} { return []chan struct{}{s.dc.sessionSem, s.dc.screenSem} },
			call: func(_ int, c context.Context) []DerivedFailure {
				d := &derive{}
				f := s.afterFacts(c, proj, []judge.Candidate{{Item: model.Item{ID: "x",
					After: []model.After{{SHA: mainSHA}}}}}, d)
				if f.SHAAncestry[mainSHA] != judge.AncestryYes {
					d.note("ancestry", "조상 판정이 비었다")
				}
				return d.failures
			},
		},
		{
			name:  "finish 선행 후보",
			lane:  func() chan struct{} { return s.dc.judgeSem },
			other: func() []chan struct{} { return []chan struct{}{s.dc.sessionSem, s.dc.screenSem} },
			call: func(_ int, c context.Context) []DerivedFailure {
				d := &derive{}
				if got := s.afterCandidate(c, "p", sess.ID, 1, d); got != mainSHA {
					d.note("after-candidate", "후보가 안 나왔다: "+got)
				}
				return d.failures
			},
		},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			lane := p.lane()
			n := 3 * cap(lane)
			pc.peak.Store(0)
			var wg sync.WaitGroup
			fails := make(chan []DerivedFailure, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					fails <- p.call(i, context.Background())
				}()
			}
			wg.Wait()
			close(fails)
			for f := range fails {
				if len(f) != 0 {
					t.Fatalf("겹친 요청이 실패했다: %+v", f)
				}
			}
			if got := pc.peak.Load(); got > int64(cap(lane)) || got == 0 {
				t.Fatalf("동시에 뜬 git %d 개, 자리 상한 %d 를 넘었다(또는 하나도 안 떴다)", got, cap(lane))
			}

			// 다른 자리가 꽉 차도 이 길은 그 뒤에 서지 않는다.
			t.Run("다른 자리 꽉 참", func(t *testing.T) {
				for _, sem := range p.other() {
					fillSlots(t, sem)
				}
				c, cancel := context.WithTimeout(ctx(), 10*time.Second)
				defer cancel()
				before := pc.sawOutside.Load()
				if f := p.call(n, c); len(f) != 0 {
					t.Fatalf("다른 자리 뒤에 섰다: %+v", f)
				}
				if pc.sawOutside.Load() == before {
					t.Fatal("git 을 하나도 안 불렀다(시험이 길을 못 탔다)")
				}
			})
		})
	}
}
