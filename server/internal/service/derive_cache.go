package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/kweiza/flightdeck/internal/gitreader"
	"github.com/kweiza/flightdeck/internal/model"
)

// 세션 카드 파생의 비용을 카드 수에서 떼어 낸다.
//
// ★ **왜 이 파일이 있나(2026-11-23 실측).** 한 프로젝트 보드(창 안 카드 31장, 워크트리 18,
// 브랜치 17)의 `GET /api/v1/dashboard.json` 이 26~31초였다. 요청 하나가 git 프로세스를 154번
// **차례로** 띄웠다, 카드마다 merge-base · diff --numstat · rev-list · status · diff HEAD 다섯.
// 벽시계 표본 208개 중 205개가 그 다섯 안이었다(status 38% · diff HEAD 21% · merge-base 17% ·
// rev-list 12% · diff 11%). 호스트 부하가 높으면 git 한 번이 0.1~1.8초라 합이 카드 수에 정비례했고,
// 보드(board.py, 10초 상한)는 502 를 냈다. 같은 브랜치·같은 워크트리를 쥔 카드가 여럿이면
// 같은 git 을 그 수만큼 다시 돌렸고, 다음 요청도 처음부터 다시 돌렸다.
//
// 그래서 축을 성질대로 가른다.
//
//	① 커밋 쌍의 순수 결과(갈래 지점 · 변경 경로 · ahead), 입력이 sha 둘이면 답이 영원히 같다.
//	   sha 쌍으로 기억하고 다시 안 묻는다. 무효화가 필요 없다(키가 곧 내용이다).
//	② 워크트리의 미커밋 상태(status · diff HEAD), 파일이 바뀌면 답이 바뀌고, 그것을 싸게 알려 줄
//	   신호가 없다(읽기 전용 바인드 마운트라 감시도 못 건다). 워크트리 목록 · 기본 브랜치 tip 도
//	   같은 성질이다. 그래서 **관측 시각과 함께** 보관하고,
//	   화면(Board)만 그 관측을 쓰면서 뒤에서 새로 읽는다. 판정 자리(pick · amend · finish 가 지나는
//	   sessionCards)는 언제나 지금 읽는다, 그 읽기가 보관본도 새로 고친다.
//	③ 같은 워크트리 · 같은 커밋 쌍은 요청 안에서도, 겹친 요청 사이에서도 한 번만 읽는다(도는
//	   읽기에 올라탄다). 판정 자리만은 화면이 연 도는 읽기에 올라타지 않는다(branchFactsOf).
//	   서로 다른 것은 함께 읽는다(서버 전체 git 동시 실행 수 상한, 화면은 deriveParallel ·
//	   판정 자리는 judgeParallel, 둘은 따로 줄 선다).
//
// ★ 보관본을 쓴 화면은 **그 관측 시각을 말한다**(derive.observed → Freshness.ObservedAt).
// 지금 읽은 척하지 않는다, 설계 §6 의 「(파생: git@14:31, 12초 전)」이 그 시각을 그대로 낸다.

// deriveParallel 은 서버 전체에서 **화면(Board)** 카드 파생 git 이 동시에 몇 개까지 뜨는지다.
// 뒤의 새로 읽기도 이 자리를 쓴다.
//
// ★ 요청마다가 아니라 **서버 전체**다. 요청마다 두면 보드 여러 장이 겹칠 때 프로세스 수가
// 그 곱으로 는다. 8 은 화면만 놓고 컨테이너 CPU(12) 아래로 둔 값이다, git status 는 대개
// 디스크를 기다린다(판정 자리와 겹친 순간은 judgeParallel 참조).
const deriveParallel = 8

// judgeParallel 은 판정 자리(readNow: pick · amend · finish 가 지나는 sessionCards)의 git 자리다.
// 화면 자리와 **따로** 둔다.
//
// ★ 한 자리로 줄 서던 앞 판은 화면 읽기를 몰아칠 때(보드 여러 장 · 뒤의 새로 읽기) 판정이 그
// 뒤에 섰다. 실측(judge_slots_measure_test.go, git 한 번 100ms · 워크트리 18 · 화면 프로젝트 셋 ·
// 보드 6장): 판정 sessionCards p50 이 부하 없이 1.05초, 부하에서 6.09초였다. 자리를 가른 뒤 부하에서
// 1.06초(부하 없음 1.11초). 판정은 드물고(사람 · 세션의 동작 하나에
// 한 번) 늘 지금 읽어야 하므로 화면에 밀리면 안 된다.
// 크기는 화면과 같은 8 이다. 판정 읽기는 드물어 둘이 함께 꽉 차는 때가 짧고, 판정은 워크트리
// 수만큼 읽어 자리를 줄이면 그만큼 느려진다(같은 측정에서 4 자리는 p50 1.52~1.55초). 둘이 겹친
// 순간 카드 파생 git 의 상한은 16 이다. 카드 파생 밖의 git(pick 의 Ancestry, finish 의
// worktreeFacts, session · workspace 의 Ref 등)은 어느 자리에도 서지 않는다, 이 상한 밖이다.
const judgeParallel = 8

// pureMemoLimit 은 커밋 쌍 기억의 상한이다. 넘으면 끝난 쌍을 통째로 비운다(다음 요청이 다시 채운다).
// 살아 있는 브랜치 수의 몇 배면 충분하다, 브랜치가 앞으로 가면 옛 쌍은 다시 안 쓰인다.
const pureMemoLimit = 4096

// memoIdle 보다 오래 아무도 안 쓴 보관본(커밋 쌍 · 워크트리)은 버린다(evict). 사라진 워크트리와
// 지나간 커밋 쌍이 서버 수명 내내 쌓이지 않게 하는 유일한 장치다.
const memoIdle = time.Hour

// snapshotStaleAfter 보다 오래된 관측을 쓴 화면은 신선도를 「낡음」으로 낸다. 보드가 몇 초마다
// 부르는 동안 보관본은 ScreenRefreshAfter 와 git 한 번의 시간 안에 있으므로, 이것을 넘었다는 것은
// 뒤의 새로 읽기가 못 따라오고 있다는 뜻이다.
const snapshotStaleAfter = 30 * time.Second

// 화면 보관본의 기본값(서버가 켠다, cmd/fd 의 serve). 시험과 다른 진입점은 New 의 기본(끔)을 쓴다.
const (
	// ScreenRefreshAfter 보다 오래된 보관본을 화면이 쓰면 뒤에서 새로 읽는다.
	ScreenRefreshAfter = 5 * time.Second
	// ScreenWait 는 화면이 보관본 없는 읽기(새 워크트리 · 새 커밋 쌍 · 기동 직후)를 기다리는 상한이다.
	// 넘으면 그 카드를 「관측 중」으로 고백하고 읽기는 뒤에서 마저 끝나 다음 조회에 실린다, 그래서
	// 화면 한 장의 시간이 처음 보는 워크트리 수에도 비례하지 않는다.
	ScreenWait = 2 * time.Second
	// ScreenMaxAge 보다 오래된 보관본은 화면도 안 쓰고 새로 읽는다(ScreenWait 까지 기다린다).
	// 보드 · 훅 · 다른 세션이 몇 초마다 부르는 동안은 보관본이 늘 몇 초 안이라, 이 상한은 한참
	// 아무도 안 본 뒤의 첫 요청만 가른다. 그 요청은 한 시간 넘은 관측을 내지 않는다.
	ScreenMaxAge = time.Hour
)

// WithScreenSnapshots 는 화면(Board)이 워크트리 미커밋 보관본을 쓰게 한다.
//
// refreshAfter 보다 오래된 보관본은 쓰면서 뒤에서 새로 읽고, maxAge 보다 오래되면 안 쓴다.
// 보관본 없는 읽기는 wait 까지만 기다린다. maxAge 가 0 이하면 끈다(기본), 화면도 판정 자리처럼
// 언제나 지금 읽고 끝까지 기다린다.
func WithScreenSnapshots(refreshAfter, maxAge, wait time.Duration) Option {
	return func(s *Service) {
		if maxAge <= 0 {
			return
		}
		s.dc.refreshAfter, s.dc.maxAge, s.dc.wait = max(refreshAfter, 0), maxAge, max(wait, 0)
	}
}

// readMode 는 파생을 누가 쓰는지다. 보관본을 써도 되는지가 그것으로 갈린다.
type readMode int

const (
	// readNow 는 판정 자리다(pick · amend · finish 가 지나는 sessionCards). 워크트리는 언제나
	// 지금 읽고 끝까지 기다린다, 요청이 끊기면 읽기도 멈춘다.
	readNow readMode = iota
	// readScreen 은 화면이다(Board). 보관본을 쓰고, 보관본 없는 읽기는 wait 까지만 기다린다.
	readScreen
)

// deriveCache 는 Service 하나가 쥐는 파생 보관소다. 0값으로는 못 쓴다, newDeriveCache.
type deriveCache struct {
	screenSem chan struct{} // 화면 git 동시 실행 상한(서버 전체, deriveParallel)
	judgeSem  chan struct{} // 판정 자리 git 동시 실행 상한(서버 전체, judgeParallel)

	refreshAfter time.Duration
	maxAge       time.Duration // 0 이면 화면 보관본을 안 쓴다
	wait         time.Duration // 화면이 보관본 없는 읽기를 기다리는 상한(maxAge>0 일 때만 쓴다)

	mu    sync.Mutex
	pure  map[pureKey]*pureEntry
	wt    cells[wtSnap]   // 워크트리 경로(Clean) → 미커밋 관측
	lists cells[listSnap] // 저장소 경로(Clean) → 워크트리 목록
	refs  cells[refSnap]  // 저장소 경로 + ref → tip

	// bg 는 뒤에서 도는 읽기의 수다. 시험이 그것이 끝나기를 기다릴 때만 쓴다.
	bg sync.WaitGroup
}

func newDeriveCache() *deriveCache {
	return &deriveCache{
		screenSem: make(chan struct{}, deriveParallel),
		judgeSem:  make(chan struct{}, judgeParallel),
		pure:      map[pureKey]*pureEntry{},
		wt:        cells[wtSnap]{},
		lists:     cells[listSnap]{},
		refs:      cells[refSnap]{},
	}
}

func (c *deriveCache) screenSnapshots(mode readMode) bool { return mode == readScreen && c.maxAge > 0 }

// slots 는 mode 가 줄 서는 git 실행 자리다. 판정 자리는 화면 자리와 따로 선다(judgeParallel).
func (c *deriveCache) slots(mode readMode) chan struct{} {
	if mode == readNow {
		return c.judgeSem
	}
	return c.screenSem
}

// acquire 는 mode 의 git 실행 자리 하나를 잡는다. ctx 가 먼저 끝나면 false.
func (c *deriveCache) acquire(ctx context.Context, mode readMode) bool {
	select {
	case c.slots(mode) <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *deriveCache) release(mode readMode) { <-c.slots(mode) }

// evict 는 memoIdle 넘게 아무도 안 쓴 보관본을 버린다. 도는 읽기는 안 건드린다.
//
// ★ 워크트리 목록으로 거르지 않는다. 카드는 등록된 워크트리의 하위 디렉토리를 쥘 수 있어
// (예: <wt>/server) 목록에 없는 경로가 곧 사라진 워크트리가 아니고, 사라진 워크트리의 카드는
// 파생 앞에서 닫혀 그 보관본을 다시 묻지 않는다. 쓰이지 않는 것은 시간으로 빠진다.
func (c *deriveCache) evict(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.pure {
		if settled(e.done) && t.Sub(e.used) > memoIdle {
			delete(c.pure, k)
		}
	}
	c.wt.evict(t)
	c.lists.evict(t)
	c.refs.evict(t)
}

func settled(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// errStillReading 은 화면의 기다림 상한(ScreenWait) 안에 못 끝난 읽기다. 읽기는 뒤에서 이어진다.
var errStillReading = errors.New("관측 중, 첫 읽기가 뒤에서 이어진다(다음 조회에 실린다)")

// branchKey 는 카드의 브랜치 하나다(이름과 그 tip). 같은 키의 카드는 커밋 축을 함께 쓴다.
type branchKey struct{ name, sha string }

// readCardFacts 는 카드들이 쓸 커밋 축(브랜치마다)과 미커밋 축(워크트리마다)을 **함께** 읽는다.
//
// 판정 자리(readNow)와 보관본이 꺼진 화면은 다 끝날 때까지 기다리고 요청의 ctx 를 쓴다.
// 보관본이 켜진 화면은 wait 까지만 기다린다. 그 안에 못 끝난 것은 errStillReading 으로
// 고백하고, 읽기는 요청의 ctx 와 떨어져 끝까지 돌아 보관소에 들어간다(커밋 쌍은 기억과
// 변경집합에, 워크트리는 보관본에), 다음 조회가 그것을 쓴다.
func (s *Service) readCardFacts(ctx context.Context, g GitReader, proj model.Project, mainSHA string,
	bks []branchKey, wts []string, mode readMode) (map[branchKey]branchFacts, map[string]wtSnap) {

	readCtx, bounded := ctx, s.dc.screenSnapshots(mode)
	if bounded {
		readCtx = context.WithoutCancel(ctx)
	}
	type got struct {
		i    int
		wt   bool
		f    branchFacts
		snap wtSnap
	}
	total := len(bks) + len(wts)
	ch := make(chan got, total) // 넉넉히, 늦게 끝난 읽기가 아무도 안 받는 채널에 막히지 않게
	for i, bk := range bks {
		s.dc.bg.Add(1)
		go func() {
			defer s.dc.bg.Done()
			// 변경집합 보관은 읽은 그 자리에서 한다. 화면이 기다림을 넘겨도 뒤에서 끝난 쌍이
			// change_set 에 들어가야 한다(조립 단계에서 하면 늦게 끝난 쌍은 영영 빠진다).
			remember := func(f branchFacts) { s.rememberChangeSet(readCtx, proj.ID, f.fork, bk.sha, f.paths) }
			ch <- got{i: i, f: s.dc.branchFactsOf(readCtx, g, mode, proj.Path, proj.DefaultBranch, mainSHA, bk.name, bk.sha, s.now(), remember)}
		}()
	}
	for i, wt := range wts {
		s.dc.bg.Add(1)
		go func() {
			defer s.dc.bg.Done()
			ch <- got{i: i, wt: true, snap: s.dc.worktreeState(readCtx, g, wt, mode, s.now)}
		}()
	}
	facts := make(map[branchKey]branchFacts, len(bks))
	states := make(map[string]wtSnap, len(wts))
	for _, bk := range bks {
		facts[bk] = branchFacts{forkErr: errStillReading, pathsErr: errStillReading, aheadErr: errStillReading}
	}
	for _, wt := range wts {
		states[wt] = wtSnap{pathsErr: errStillReading, deltaErr: errStillReading}
	}
	put := func(r got) {
		if r.wt {
			states[wts[r.i]] = r.snap
		} else {
			facts[bks[r.i]] = r.f
		}
	}
	var timeout <-chan time.Time
	if bounded {
		t := time.NewTimer(s.dc.wait)
		defer t.Stop()
		timeout = t.C
	}
	for n := 0; n < total; n++ {
		select {
		case r := <-ch:
			put(r)
		case <-timeout:
			// 상한과 같은 순간에 끝난 것은 마저 싣는다(기다리지는 않는다).
			for ; n < total; n++ {
				select {
				case r := <-ch:
					put(r)
					continue
				default:
				}
				break
			}
			if n < total {
				s.log.InfoContext(ctx, "화면 파생이 기다림 상한을 넘었다, 남은 읽기는 뒤에서 잇는다",
					"project", proj.ID, "wait", s.dc.wait.String(), "left", fmt.Sprintf("%d/%d", total-n, total))
			}
			return facts, states
		}
	}
	return facts, states
}

// ─────────────────────────────────────────────────────────────────────────────
// ① 커밋 쌍의 순수 결과
// ─────────────────────────────────────────────────────────────────────────────

type pureKey struct{ repo, base, head string }

// pureEntry 는 커밋 쌍 하나의 기억이다. done 이 닫히기 전에는 읽는 중이다(올라탈 수 있다).
type pureEntry struct {
	done chan struct{}
	f    branchFacts // done 이 닫힌 뒤에만 읽는다
	used time.Time   // mu 아래에서만 쓴다
	// judge 는 판정 자리가 연 칸인지다(판정 자리에서 읽는다). 판정은 이런 칸에만 올라탄다.
	judge bool
}

// branchFacts 는 브랜치 하나의 커밋 축 셋이다. 축마다 실패가 따로 산다(호출부가 축마다 고백한다).
type branchFacts struct {
	fork     string
	forkErr  error
	paths    []string
	delta    map[string]model.LineDelta
	pathsErr error
	ahead    int
	aheadErr error
}

func (f branchFacts) ok() bool { return f.forkErr == nil && f.pathsErr == nil && f.aheadErr == nil }

func failedFacts(err error) branchFacts {
	return branchFacts{forkErr: err, pathsErr: err, aheadErr: err}
}

// branchFactsOf 는 기본 브랜치 tip(mainSHA)과 브랜치 tip(headSHA) 쌍의 커밋 축 셋을 낸다.
//
// ★ git 에는 **sha 를 넘긴다**(이름이 아니라). 이름을 넘기고 sha 로 기억하면, 워크트리 목록을
// 읽은 뒤 브랜치가 움직인 순간 새 tip 의 답이 옛 tip 의 키로 들어간다. sha 를 넘기면 키와
// 답이 언제나 같은 커밋이고, 카드의 BranchSHA 와 변경집합의 head 도 같은 커밋을 가리킨다.
// sha 를 하나라도 모르면 이름으로 읽고 기억하지 않는다(그때는 키가 없다, 변경집합도 안 남긴다).
//
// 같은 쌍을 누가 읽는 중이면 그 읽기에 올라탄다(겹친 보드가 같은 git 을 다시 띄우지 않게).
// 셋이 다 성공했을 때만 기억하고, 그때 한 번 remember 를 부른다. 실패는 일시적일 수 있어
// 기억하지 않는다, 올라탔던 쪽은 다시 시도한다.
//
// ★ 판정 자리(readNow)는 **화면이 연 도는 읽기에 올라타지 않는다**. 그 읽기는 화면 자리
// (deriveParallel)가 비기를 기다리는 중일 수 있고, 올라타면 판정이 다시 화면 줄 뒤에 선다.
// 판정은 자기 자리로 따로 읽는다(쌍의 답은 같으니 결과는 같다). 기억과 remember 는 칸을 쥔 읽기가
// 한다, 판정의 따로 읽기는 둘 다 안 한다. 판정이 연 칸에는 판정도 올라탄다(같은 판정 자리다).
func (c *deriveCache) branchFactsOf(ctx context.Context, g GitReader, mode readMode, repo, defBranch, mainSHA, branch, headSHA string,
	t time.Time, remember func(branchFacts)) branchFacts {

	if mainSHA == "" || headSHA == "" {
		return c.readFacts(ctx, g, mode, defBranch, branch)
	}
	key := pureKey{repo: filepath.Clean(repo), base: mainSHA, head: headSHA}
	for {
		c.mu.Lock()
		if e := c.pure[key]; e != nil {
			e.used = t
			c.mu.Unlock()
			if mode == readNow && !e.judge && !settled(e.done) {
				return c.readFacts(ctx, g, mode, mainSHA, headSHA)
			}
			select {
			case <-e.done:
			case <-ctx.Done():
				return failedFacts(ctx.Err())
			}
			if e.f.ok() {
				return e.f
			}
			continue // 앞선 읽기가 실패해 기억에서 빠졌다, 다시 시도한다
		}
		if len(c.pure) >= pureMemoLimit {
			for k, e := range c.pure {
				if settled(e.done) {
					delete(c.pure, k)
				}
			}
		}
		e := &pureEntry{done: make(chan struct{}), used: t, judge: mode == readNow}
		c.pure[key] = e
		c.mu.Unlock()

		f := c.readFacts(ctx, g, mode, mainSHA, headSHA)
		if f.ok() && remember != nil {
			remember(f)
		}
		c.mu.Lock()
		e.f = f
		if !f.ok() && c.pure[key] == e {
			delete(c.pure, key)
		}
		close(e.done)
		c.mu.Unlock()
		return f
	}
}

// readFacts 는 커밋 축 셋을 mode 의 git 자리에서 지금 읽는다.
func (c *deriveCache) readFacts(ctx context.Context, g GitReader, mode readMode, base, head string) branchFacts {
	if !c.acquire(ctx, mode) {
		return failedFacts(ctx.Err())
	}
	defer c.release(mode)
	var f branchFacts
	if f.fork, f.forkErr = g.MergeBase(ctx, base, head); f.forkErr == nil {
		f.paths, f.delta, f.pathsErr = g.ChangedPaths(ctx, f.fork, head)
	}
	f.ahead, _, f.aheadErr = g.AheadBehind(ctx, head, base)
	return f
}

// ─────────────────────────────────────────────────────────────────────────────
// ② 관측 보관본: 워크트리 목록 · 기본 브랜치 tip · 워크트리 미커밋
// ─────────────────────────────────────────────────────────────────────────────

// snapshot 은 보관본 한 칸에 드는 관측 한 번이다.
type snapshot interface {
	observedAt() time.Time // 읽기를 **시작한** 시각, 이 관측은 적어도 그때만큼 새롭다
	ok() bool              // 실패 없이 읽었나(실패한 새로 읽기가 성한 관측을 안 덮게)
}

// cell 은 키 하나의 보관본이다. 필드는 mu 아래에서만 읽고 쓴다.
type cell[T snapshot] struct {
	val      T
	have     bool
	inflight chan struct{} // 화면 쪽 읽기(첫 읽기 · 뒤의 새로 읽기)가 도는 중이면 닫히기를 기다릴 채널
	used     time.Time
}

// cells 는 한 종류의 보관본이다(키 → 칸).
type cells[T snapshot] map[string]*cell[T]

// at 은 키의 칸을 낸다(없으면 만든다). mu 를 쥐고 부른다.
func (m cells[T]) at(key string, t time.Time) *cell[T] {
	e := m[key]
	if e == nil {
		e = &cell[T]{}
		m[key] = e
	}
	e.used = t
	return e
}

// evict 는 memoIdle 넘게 안 쓴 칸을 버린다(도는 읽기는 안 건드린다). mu 를 쥐고 부른다.
func (m cells[T]) evict(t time.Time) {
	for k, e := range m {
		if e.inflight == nil && t.Sub(e.used) > memoIdle {
			delete(m, k)
		}
	}
}

// store 는 관측을 보관한다. mu 를 쥐고 부른다.
//
//   - 더 새 관측이 이미 있으면 안 덮는다(뒤늦게 끝난 읽기가 새 값을 지우지 않게).
//   - 요청이 끊겨 난 실패는 안 넣는다, 그것은 저장소의 사실이 아니다.
//   - 뒤의 새로 읽기(keepGood)가 실패하면 성한 보관본을 안 덮는다. 부하로 git 한 번이 시간을
//     넘긴 것이 화면의 경로를 지우면 안 된다. 성한 보관본은 늙어 가다(낡음) maxAge 에서 빠진다.
func (e *cell[T]) store(ctx context.Context, v T, keepGood bool) {
	if ctx.Err() != nil {
		return
	}
	if e.have && e.val.observedAt().After(v.observedAt()) {
		return
	}
	if keepGood && e.have && e.val.ok() && !v.ok() {
		return
	}
	e.val, e.have = v, true
}

// observe 는 키 하나의 관측을 낸다. read 는 지금 읽는다(git 자리는 read 가 mode 대로 잡는다,
// 뒤의 새로 읽기는 화면만 띄우므로 화면 자리에 선다).
//
// 판정 자리(readNow)나 보관본이 꺼진 화면은 **언제나 지금 읽는다**, 도는 읽기에 올라타지도
// 않는다(그 읽기는 이 요청보다 먼저 시작했을 수 있다). 그 읽기도 보관본을 고친다.
// 화면은 maxAge 안의 보관본을 쓰고 refreshAfter 보다 오래됐으면 뒤에서 하나 새로 읽는다.
// 쓸 보관본이 없으면 도는 읽기에 올라타거나, 없으면 자기가 읽는다(같은 키의 첫 읽기는
// 겹친 보드 사이에서도 하나다).
func observe[T snapshot](c *deriveCache, m cells[T], key string, ctx context.Context, mode readMode,
	now func() time.Time, read func(context.Context) T) T {

	if !c.screenSnapshots(mode) {
		v := read(ctx)
		if c.maxAge > 0 { // 화면이 보관본을 쓸 때만 넣는다(꺼져 있으면 아무도 안 꺼낸다)
			c.mu.Lock()
			m.at(key, now()).store(ctx, v, false)
			c.mu.Unlock()
		}
		return v
	}
	for {
		t := now()
		c.mu.Lock()
		e := m.at(key, t)
		if e.have && t.Sub(e.val.observedAt()) <= c.maxAge {
			v := e.val
			if t.Sub(v.observedAt()) >= c.refreshAfter && e.inflight == nil {
				e.inflight = make(chan struct{})
				c.bg.Add(1)
				go func() {
					defer c.bg.Done()
					// 요청의 ctx 를 안 쓴다. 응답이 나가면 그 ctx 는 끝나고, 그러면 읽기가 매번
					// 취소돼 보관본이 영영 안 새로워진다. 상한은 git 한 번의 시간 상한(gitreader)이 건다.
					readInto(c, e, context.Background(), read, true)
				}()
			}
			c.mu.Unlock()
			return v
		}
		if ch := e.inflight; ch != nil {
			c.mu.Unlock()
			select {
			case <-ch:
				continue // 그 읽기가 넣은 보관본을 다시 본다
			case <-ctx.Done():
				return read(ctx) // 끊긴 ctx 로 읽으면 곧바로 그 실패가 난다
			}
		}
		e.inflight = make(chan struct{})
		c.mu.Unlock()
		return readInto(c, e, ctx, read, false)
	}
}

// readInto 는 e.inflight 를 쥔 쪽이 읽어 보관하고 inflight 를 푼다.
func readInto[T snapshot](c *deriveCache, e *cell[T], ctx context.Context, read func(context.Context) T, background bool) T {
	v := read(ctx)
	c.mu.Lock()
	e.store(ctx, v, background)
	close(e.inflight)
	e.inflight = nil
	c.mu.Unlock()
	return v
}

// withSlot 은 mode 의 git 실행 자리를 잡고 fn 을 부른다. 못 잡으면(ctx 끝남) failed 를 낸다.
func withSlot[T any](c *deriveCache, ctx context.Context, mode readMode, failed func(error) T, fn func() T) T {
	if !c.acquire(ctx, mode) {
		return failed(ctx.Err())
	}
	defer c.release(mode)
	return fn()
}

// wtSnap 은 워크트리 하나의 미커밋 관측 한 번이다. 두 축의 실패가 따로 산다.
type wtSnap struct {
	paths    []string
	pathsErr error
	delta    map[string]model.LineDelta
	deltaErr error
	at       time.Time
}

func (sn wtSnap) observedAt() time.Time { return sn.at }
func (sn wtSnap) ok() bool              { return sn.pathsErr == nil && sn.deltaErr == nil }

// worktreeState 는 워크트리 하나의 미커밋 관측을 낸다(observe 의 규칙대로).
func (c *deriveCache) worktreeState(ctx context.Context, g GitReader, wt string, mode readMode, now func() time.Time) wtSnap {
	wt = filepath.Clean(wt)
	return observe(c, c.wt, wt, ctx, mode, now, func(ctx context.Context) wtSnap {
		at := now()
		return withSlot(c, ctx, mode, func(err error) wtSnap { return wtSnap{at: at, pathsErr: err, deltaErr: err} }, func() wtSnap {
			sn := wtSnap{at: at}
			sn.paths, sn.pathsErr = g.UncommittedPaths(ctx, wt)
			sn.delta, sn.deltaErr = g.UncommittedDelta(ctx, wt)
			return sn
		})
	})
}

// listSnap 은 저장소 하나의 `git worktree list` 한 번이다.
type listSnap struct {
	wts []gitreader.Worktree
	err error
	at  time.Time
}

func (l listSnap) observedAt() time.Time { return l.at }
func (l listSnap) ok() bool              { return l.err == nil }

// worktreeList 는 저장소의 워크트리 목록을 낸다(observe 의 규칙대로).
//
// ★ 화면이 보관본 목록을 써도 사라진 워크트리의 카드 닫기는 안전하다. 닫기는 목록에 없다는 것에
// 더해 그 디렉토리가 **지금** 없어야 한다(gone_worktree.go, lstat). 보관본보다 뒤에 생긴
// 워크트리는 디렉토리가 있어 안 닫히고, 지워진 워크트리는 다음 새로 읽기 뒤에 닫힌다.
func (c *deriveCache) worktreeList(ctx context.Context, g GitReader, repo string, mode readMode, now func() time.Time) listSnap {
	return observe(c, c.lists, filepath.Clean(repo), ctx, mode, now, func(ctx context.Context) listSnap {
		at := now()
		return withSlot(c, ctx, mode, func(err error) listSnap { return listSnap{err: err, at: at} }, func() listSnap {
			wts, err := g.Worktrees(ctx)
			return listSnap{wts: wts, err: err, at: at}
		})
	})
}

// refSnap 은 ref 하나의 tip 관측이다.
type refSnap struct {
	ref model.RefState
	err error
	at  time.Time
}

func (r refSnap) observedAt() time.Time { return r.at }
func (r refSnap) ok() bool              { return r.err == nil }

// refState 는 저장소 repo 의 ref 하나의 tip 을 낸다(observe 의 규칙대로). fresh 는 git 에서
// 새로 읽은 성한 관측마다 한 번 불린다(ref_state 보관, 보관본에서 꺼낸 값은 이미 보관됐다).
func (c *deriveCache) refState(ctx context.Context, g GitReader, repo, ref string, mode readMode, now func() time.Time,
	fresh func(context.Context, model.RefState)) refSnap {

	return observe(c, c.refs, filepath.Clean(repo)+"\x00"+ref, ctx, mode, now, func(ctx context.Context) refSnap {
		at := now()
		r := withSlot(c, ctx, mode, func(err error) refSnap { return refSnap{err: err, at: at} }, func() refSnap {
			r, err := g.Ref(ctx, ref)
			return refSnap{ref: r, err: err, at: at}
		})
		if r.ok() {
			fresh(ctx, r.ref)
		}
		return r
	})
}

// hasKey 는 맵에 키가 있는지다(값이 0값이어도).
func hasKey[K comparable, V any](m map[K]V, k K) bool {
	_, ok := m[k]
	return ok
}
