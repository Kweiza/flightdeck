package store_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store"
)

// 저장 엔진의 경합 기준 수치를 잰다: 동시 선점 · 발번 · 같은 행을 읽고 고치는 트랜잭션 동시.
//
// ★ 저장 엔진을 바꾸기 전후에 **같은 도구**로 잰다. 그래서 이 파일은 외부 시험 패키지이고
// store 의 공개 API 만 부른다(s.db 같은 엔진 속에 손이 안 닿는다). 엔진을 바꿀 때 고칠 자리는
// benchStore 하나다. 레인 경합은 레인 판정이 사는 service 쪽 벤치(BenchmarkRESTLane)가 REST 로 잰다.
//
// ★ 벤치 함수라 기본 `go test` 에서는 안 돈다. 잴 때는 횟수를 박는다:
//
//	go test ./internal/store -run '^$' -bench . -benchtime 1000x -count 5
//
// 시간(-benchtime 1s)으로 두면 빠른 엔진일수록 표본이 늘어 두 엔진이 서로 다른 크기의 원장을
// 재게 된다. 두 출력은 benchstat 으로 맞댄다.
//
// ★ 불변식(이중 선점 · 발번 중복과 빠짐 · 죽은 트랜잭션 · 잃어버린 갱신)이 하나라도 깨지면 벤치가 실패한다.
// 깨진 수는 0 이어도 지표로 낸다, 출력 한 줄이 「몇 번 다퉈 몇 건 깨졌나」의 기록이다.

const benchProject = "bench"

// benchStore 는 빈 원장 하나를 연다. **엔진을 바꿀 때 이 함수만 바꾼다.**
//
// ★ 짝이 있다: service 의 rest_bench_test.go 에 같은 몸통의 benchStore 가 있다(시험 패키지끼리는
// 함수를 나눠 쓸 수 없다). 한쪽만 바꾸면 두 벤치가 다른 엔진을 재므로, 그쪽의
// TestBenchStoreTwinsMatch 가 두 선언을 맞대 다르면 빨개진다. 바꿀 때는 둘을 함께 바꾼다.
func benchStore(tb testing.TB) *store.Store {
	tb.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(tb.TempDir(), "fd.db")
	if err := store.Migrate(context.Background(), path, log); err != nil {
		tb.Fatalf("DB 적용 실패: %v", err)
	}
	st, err := store.OpenWithLogger(path, log)
	if err != nil {
		tb.Fatalf("DB 열기 실패: %v", err)
	}
	tb.Cleanup(func() { st.Close() })
	return st
}

// benchFixture 는 프로젝트 하나와 세션 n 개를 연다. 세션 id 를 연 순서대로 낸다.
func benchFixture(tb testing.TB, n int) (*store.Store, []string) {
	tb.Helper()
	st := benchStore(tb)
	ctx := context.Background()
	if err := st.UpsertProject(ctx, model.Project{ID: benchProject, Path: "/repo/" + benchProject}); err != nil {
		tb.Fatalf("프로젝트 등록 실패: %v", err)
	}
	if err := st.UpsertMachine(ctx, model.Machine{ID: "m1", Hostname: "bench"}); err != nil {
		tb.Fatalf("머신 등록 실패: %v", err)
	}
	ids := make([]string, n)
	for i := range ids {
		cc := fmt.Sprintf("cc-%02d", i)
		sess, _, err := st.OpenSession(ctx, benchProject, "m1", "/w/"+cc, cc, "", time.Time{})
		if err != nil {
			tb.Fatalf("세션 등록 실패(%s): %v", cc, err)
		}
		ids[i] = sess.ID
	}
	return st, ids
}

func benchItem(tb testing.TB, st *store.Store, id string) {
	tb.Helper()
	if err := st.AddItem(context.Background(), model.Item{
		Project: benchProject, ID: id, Title: id, Body: "벤치 항목", Paths: []string{"bench/"},
	}); err != nil {
		tb.Fatalf("항목 등록 실패(%s): %v", id, err)
	}
}

// race 는 고루틴 n 개를 한꺼번에 풀어 f(0..n-1) 을 돌리고 다 끝나기를 기다린다.
// 출발선(start)이 없으면 먼저 뜬 고루틴이 끝난 뒤에 나중 것이 떠서 다툼이 안 생긴다.
func race(n int, f func(k int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for k := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			f(k)
		}()
	}
	close(start)
	wg.Wait()
}

// BenchmarkClaimRace 는 같은 항목을 두 세션이 동시에 선점한다. op 하나 = 새 항목 하나를 두고 한 판.
//
// 판마다 정확히 하나가 이기고, 진 쪽은 *ClaimHeldError 로 이긴 쪽을 들어야 한다. 끝나고 원장의
// 점유자가 이긴 쪽과 같은지 항목마다 다시 본다(응답만 맞고 원장이 다른 경우를 잡는다).
func BenchmarkClaimRace(b *testing.B) {
	st, sess := benchFixture(b, 2)
	ctx := context.Background()
	items := make([]string, b.N)
	for i := range items {
		items[i] = fmt.Sprintf("claim-%06d", i)
		benchItem(b, st, items[i])
	}
	winners := make([]string, b.N)
	var double, failed int
	var firstErr error
	b.ResetTimer()
	for i, item := range items {
		var errs [2]error
		race(2, func(k int) { _, errs[k] = st.ClaimItem(ctx, benchProject, item, sess[k], time.Time{}) })
		if errs[0] == nil && errs[1] == nil {
			double++
			continue
		}
		w := 0
		if errs[0] != nil {
			w = 1
		}
		var he *store.ClaimHeldError
		if lost := errs[1-w]; errs[w] != nil || !errors.As(lost, &he) || he.Holder != sess[w] {
			failed++
			if firstErr == nil {
				firstErr = errors.Join(errs[0], errs[1])
			}
			continue
		}
		winners[i] = sess[w]
	}
	b.StopTimer()

	var mismatch int
	for i, item := range items {
		c, err := st.GetClaim(ctx, benchProject, item)
		if err != nil || c.SessionID != winners[i] || c.ReleasedAt != nil {
			mismatch++
		}
	}
	b.ReportMetric(float64(double), "double-claims")
	if double+failed+mismatch > 0 {
		b.Fatalf("%d판 중 이중 선점 %d · 실패 %d · 원장 어긋남 %d. 첫 실패: %v",
			b.N, double, failed, mismatch, firstErr)
	}
}

// BenchmarkCounterAlloc 은 발번기 하나를 16 개가 쉬지 않고 함께 돌린다. op 하나 = 번호 하나.
//
// 받은 번호 전체가 1..N 을 겹침 없이, 빠짐 없이 채워야 한다. 겹침만 보면 「1,2,4,5…」처럼
// 건너뛴 것을 못 본다.
func BenchmarkCounterAlloc(b *testing.B) {
	const allocators = 16
	st, _ := benchFixture(b, 0)
	ctx := context.Background()
	got := make([][]int64, allocators)
	errs := make([]error, allocators)
	var next atomic.Int64
	b.ResetTimer()
	race(allocators, func(k int) {
		for next.Add(1) <= int64(b.N) {
			v, err := st.NextCounter(ctx, benchProject, "bench")
			if err != nil {
				errs[k] = err
				return
			}
			got[k] = append(got[k], v)
		}
	})
	b.StopTimer()

	seen := make(map[int64]bool, b.N)
	var dups, gaps int
	for _, vs := range got {
		for _, v := range vs {
			if seen[v] {
				dups++
			}
			seen[v] = true
		}
	}
	for want := int64(1); want <= int64(b.N); want++ {
		if !seen[want] {
			gaps++
		}
	}
	peek, perr := st.PeekCounter(ctx, benchProject, "bench")
	b.ReportMetric(float64(dups), "dups")
	b.ReportMetric(float64(gaps), "gaps")
	if err := errors.Join(append(errs, perr)...); err != nil || dups+gaps > 0 || peek != int64(b.N) {
		b.Fatalf("발번 %d건 중 중복 %d · 빠짐 %d · 마지막 값 %d. 실패: %v", b.N, dups, gaps, peek, err)
	}
}

// BenchmarkReadModifyWriteTx 는 같은 행을 읽고 그 값으로 고치는 트랜잭션을 24 개가 쉬지 않고 함께
// 연다. op 하나 = 트랜잭션 하나.
//
// 트랜잭션마다 항목 본문의 숫자를 읽어 1 을 더해 개정(AmendItem)으로 쓴다. 개정은 그 안에서 다시
// 「마지막 개정 번호 + 1」을 읽고 넣는다. 그래서 직렬성이 서지 않는 엔진 · 격리 수준에서는 둘이
// 같은 값을 읽고 같은 값을 써서 갱신을 잃거나(최종값 < N), 같은 개정 번호를 두 번 쓰려다
// 죽는다(실패한 트랜잭션). 쓰는 행이 겹치지 않으면 READ COMMITTED 엔진에서는 잠금 없이도 충돌이
// 원래 없어 이 축을 못 본다, 그래서 공통 행이다. 쓰기 잠금을 늦게 잡는 SQLite 설정은 이 모양에서
// 남의 커밋과 부딪혀 트랜잭션을 죽인다(store.Tx 머리말의 실측).
//
// 끝나고 셋을 본다: 최종 본문 = N, 개정 번호 = 1..N 빈칸 · 겹침 없음, 개정 k 의 「고치기 전 본문」
// = k-1(모든 트랜잭션이 바로 앞 트랜잭션의 결과를 읽었다는 사슬).
func BenchmarkReadModifyWriteTx(b *testing.B) {
	const writers = 24
	st, sess := benchFixture(b, writers)
	ctx := context.Background()
	if err := st.AddItem(ctx, model.Item{
		Project: benchProject, ID: "rmw", Title: "rmw", Body: "0", Paths: []string{"bench/"},
	}); err != nil {
		b.Fatal(err)
	}
	var next atomic.Int64
	failed := make([]int, writers)
	firstErr := make([]error, writers)
	b.ResetTimer()
	race(writers, func(k int) {
		for next.Add(1) <= int64(b.N) {
			err := st.Tx(ctx, func(t *store.Tx) error {
				it, err := t.GetItem(benchProject, "rmw")
				if err != nil {
					return err
				}
				n, err := strconv.Atoi(it.Body)
				if err != nil {
					return fmt.Errorf("본문이 숫자가 아니다(%q): %w", it.Body, err)
				}
				body := strconv.Itoa(n + 1)
				_, err = t.AmendItem(benchProject, "rmw", store.AmendPatch{Body: &body, Reason: "벤치 증가"}, sess[k])
				return err
			})
			if err != nil {
				failed[k]++
				if firstErr[k] == nil {
					firstErr[k] = err
				}
			}
		}
	})
	b.StopTimer()

	var nFailed int
	for _, f := range failed {
		nFailed += f
	}
	it, err := st.GetItem(ctx, benchProject, "rmw")
	if err != nil {
		b.Fatal(err)
	}
	final, _ := strconv.Atoi(it.Body)
	revs, err := st.ItemRevisions(ctx, benchProject, "rmw")
	if err != nil {
		b.Fatal(err)
	}
	broken := max(b.N-len(revs), 0)
	for i, r := range revs {
		if r.Rev != i+1 || r.Body != strconv.Itoa(i) {
			broken++
		}
	}
	lost := b.N - nFailed - final
	b.ReportMetric(float64(nFailed), "failed-tx")
	b.ReportMetric(float64(lost), "lost-updates")
	b.ReportMetric(float64(broken), "broken-revs")
	if nFailed+lost+broken > 0 || final != b.N {
		b.Fatalf("트랜잭션 %d개 중 실패 %d · 잃은 갱신 %d · 어긋난 개정 %d(개정 %d개, 최종값 %q). 첫 오류: %v",
			b.N, nFailed, lost, broken, len(revs), it.Body, errors.Join(firstErr...))
	}
}
