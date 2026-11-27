package service_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/api"
	"github.com/kweiza/flightdeck/internal/gitreader"
	"github.com/kweiza/flightdeck/internal/judge"
	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/service"
	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/storetest/freshstore"
)

// REST 왕복 지연의 기준 수치를 잰다: pick · note · finish, 그리고 랜딩 레인 경합.
//
// ★ 저장 엔진을 바꾸기 전후에 **같은 도구**로 잰다. 요청은 실물 HTTP(httptest)로 나가
// api 게이트 사슬(멱등 기록 포함) → service → store 를 다 탄다. 원장은 store 쪽 벤치와 같은
// freshstore.Open 이 연다 — 엔진을 바꿀 때 고칠 자리는 그 함수 하나다. api 를 불러야 해서 외부 시험
// 패키지다(service 안에서는 순환 import).
//
// ★ git 은 뺀다. 판정 자리(pick · finish)는 요청마다 git 을 지금 읽는다(derive_cache.go 머리말).
// 그 비용은 저장 엔진과 무관하고 호스트 부하로 크게 흔들려, 엔진이 바꾼 몫을 묻어 버린다.
// 그래서 git 자리에 움직이지 않는 저장소(stillGit)를 꽂아 프로세스를 하나도 안 띄운다.
// 이 수치는 「git 을 뺀 서버 몫」이다.
//
// ★ 벤치 함수라 기본 `go test` 에서는 안 돈다. 잴 때는 횟수를 박는다:
//
//	go test ./internal/service -run '^$' -bench . -benchtime 1000x -count 5
//
// 시간으로 두면 빠른 엔진일수록 표본과 원장이 커져 두 엔진이 다른 것을 잰다. 지연은 요청 종류마다
// p50 · p95 · p99(ms, 가까운 순위)로 낸다. 두 출력은 benchstat 으로 맞댄다.
//
// ★ 전 크기 원장 모양(BenchmarkRESTLifecycleFullLedger)은 두 엔진을 한 번에 잰다. Postgres 쪽은 시험 PG 가 있어야 한다
// (FD_TEST_PG - 없으면 실패한다):
//
//	FD_TEST_PG=… go test ./internal/service -run '^$' -bench FullLedger -benchtime 200x -count 3

const benchProject = "bench"

// restBench 는 실물 HTTP 서버 하나와 그 프로젝트의 워크트리들이다. 세션은 벤치가 연다.
type restBench struct {
	st        *store.Store
	url       string
	http      *http.Client
	repo      string
	worktrees []string
	keys      atomic.Int64 // Idempotency-Key 발번. 모든 쓰기에 필요하다
}

// newRESTBench 는 빈 원장(freshstore.Open) 위의 newRESTBenchOn 이다.
func newRESTBench(tb testing.TB, n int) *restBench {
	tb.Helper()
	return newRESTBenchOn(tb, n, freshstore.Open(tb))
}

// newRESTBenchOn 은 원장 st 위에 프로젝트 하나와 워크트리 n 개를 둔다. 클라이언트 k 가 워크트리 k 를 쓴다.
func newRESTBenchOn(tb testing.TB, n int, st *store.Store) *restBench {
	tb.Helper()
	base := tb.TempDir()
	repo := filepath.Join(base, "repo")
	worktrees := make([]string, n)
	for i := range worktrees {
		worktrees[i] = filepath.Join(base, fmt.Sprintf("wt-%02d", i))
	}
	// 디렉토리는 실제로 둔다: 사라진 워크트리 판정이 lstat 으로 본다.
	for _, d := range append([]string{repo}, worktrees...) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			tb.Fatalf("디렉토리 생성 실패: %v", err)
		}
	}
	git := newStillGit(repo, worktrees)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(st, log, service.WithGitFactory(func(string) service.GitReader { return git }))
	srv := httptest.NewServer(api.NewServer(svc, api.Options{Log: log}))
	tb.Cleanup(srv.Close) // 저장소 닫기보다 먼저 돈다(정리는 거꾸로 돈다)

	// 고루틴마다 연결 하나를 계속 쓴다. 기본 유휴 상한(호스트당 2)이면 동시 요청이 연결을
	// 새로 맺어 그 비용이 지연에 섞인다.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 64
	r := &restBench{st: st, url: srv.URL, http: &http.Client{Transport: tr}, repo: repo, worktrees: worktrees}
	tb.Cleanup(tr.CloseIdleConnections)
	return r
}

// openSession 은 워크트리 k 에 대화 cc 의 세션을 열고 id 를 낸다.
func (r *restBench) openSession(ctx context.Context, k int, cc string) (string, error) {
	var res struct{ Session struct{ ID string } }
	_, err := r.post(ctx, "/api/v1/sessions", map[string]any{
		"project": benchProject, "project_path": r.repo, "machine_id": "bench-m", "hostname": "bench",
		"worktree": r.worktrees[k], "cc_session_id": cc, "label": filepath.Base(r.worktrees[k]),
	}, &res)
	return res.Session.ID, err
}

// closeSession 은 세션을 끝낸다. 끝난 세션은 살아 있는 세션 목록(카드 파생)에서 빠진다.
func (r *restBench) closeSession(ctx context.Context, id string) error {
	_, err := r.send(ctx, http.MethodPatch, "/api/v1/sessions/"+id,
		map[string]any{"state": string(model.SessionDone)}, nil)
	return err
}

// openSessions 는 워크트리마다 오래 사는 세션 하나씩을 연다.
func (r *restBench) openSessions(tb testing.TB) []string {
	tb.Helper()
	ids := make([]string, len(r.worktrees))
	for k := range ids {
		id, err := r.openSession(context.Background(), k, fmt.Sprintf("cc-%02d", k))
		if err != nil {
			tb.Fatal(err)
		}
		ids[k] = id
	}
	return ids
}

func (r *restBench) post(ctx context.Context, path string, body, out any) (time.Duration, error) {
	return r.send(ctx, http.MethodPost, path, body, out)
}

// send 는 쓰기 하나를 보내고 왕복 시간을 낸다(본문을 다 읽을 때까지). 2xx 가 아니면 오류다,
// 실패한 요청의 지연은 표본이 아니다. 여러 고루틴에서 부르므로 Fatal 대신 오류를 돌려준다.
func (r *restBench) send(ctx context.Context, method, path string, body, out any) (time.Duration, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, r.url+path, bytes.NewReader(buf))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("bench-%d", r.keys.Add(1)))
	start := time.Now()
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	took := time.Since(start)
	if err != nil {
		return 0, fmt.Errorf("%s: 본문 읽기 실패: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("%s → %d: %.300s", path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return 0, fmt.Errorf("%s: 응답 해석 실패: %w", path, err)
		}
	}
	return took, nil
}

// clients 는 고루틴 n 개가 op 를 하나씩 집어 b.N 개를 다 할 때까지 돌린다. 고루틴 k 는 워크트리 k 를 쓴다.
//
// 하나가 실패하면 공유 ctx 를 취소해 모두 빠진다. 안 그러면 레인처럼 남을 기다리는 op 는 실패한
// 고루틴이 쥔 차례를 끝없이 기다린다. 취소된 뒤에 난 오류는 그 취소의 결과라 모으지 않는다.
func clients(b *testing.B, n int, op func(ctx context.Context, k int, i int64) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var next atomic.Int64
	var wg sync.WaitGroup
	errs := make([]error, n)
	for k := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := next.Add(1); i <= int64(b.N) && ctx.Err() == nil; i = next.Add(1) {
				if err := op(ctx, k, i); err != nil {
					if ctx.Err() == nil {
						errs[k] = err
					}
					cancel()
					return
				}
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// BenchmarkRESTLifecycle 은 op 하나가 항목 하나의 한살이다: add → pick → note → finish.
// pick · note · finish 의 왕복 지연을 잰다(add 와 세션 열고 닫기는 준비라 안 잰다). 클라이언트
// 하나가 혼자 할 때와 여덟이 함께 할 때를 따로 낸다, 여럿이면 쓰기 잠금 대기가 지연에 붙는다.
//
// 세션 모양이 둘이다.
//
//	fresh  : 한살이마다 세션을 새로 열고 끝나면 닫는다. 작업자 하나가 항목 하나를 맡는 평소 모양이다.
//	shared : 클라이언트마다 세션 하나가 한살이를 이어 간다. 오래 사는 세션에 판단이 쌓이는 모양이고,
//	         살아 있는 세션의 카드 파생이 그 세션의 판단을 전부 읽는 비용이 원장 크기와 함께 자란다.
func BenchmarkRESTLifecycle(b *testing.B) {
	for _, fresh := range []bool{true, false} {
		for _, n := range []int{1, 8} {
			shape := map[bool]string{true: "fresh", false: "shared"}[fresh]
			b.Run(fmt.Sprintf("session=%s/clients=%d", shape, n), func(b *testing.B) {
				benchLifecycle(b, newRESTBench(b, n), n, fresh)
			})
		}
	}
}

func benchLifecycle(b *testing.B, r *restBench, n int, fresh bool) {
	var shared []string
	if !fresh {
		shared = r.openSessions(b)
	}
	var lat latencies
	b.ResetTimer()
	err := clients(b, n, func(ctx context.Context, k int, i int64) error {
		if !fresh {
			return r.lifecycle(ctx, shared[k], i, &lat)
		}
		sess, err := r.openSession(ctx, k, fmt.Sprintf("cc-%06d", i))
		if err != nil {
			return err
		}
		if err := r.lifecycle(ctx, sess, i, &lat); err != nil {
			return err
		}
		return r.closeSession(ctx, sess)
	})
	b.StopTimer()
	if err != nil {
		b.Fatal(err)
	}
	r.mustAllDone(b)
	lat.report(b, "pick", "note", "finish")
}

// BenchmarkRESTLifecycleFullLedger 는 fresh/1 한살이를 **전 크기 원장 모양** 위에서 두 엔진으로 잰다.
//
// ★ 왜 따로 두나: 위 벤치는 op 마다 빈 원장에서 시작해 event 표가 작다. 원장 크기를 따라 자라는 비용(pick 마다 도는
// 종료 선언 조회 - event 를 project + kind 로 걸러 id 순으로 읽는다)은 거기서 안 보인다. Postgres 는 문 캐시의 일반 계획이
// 맞는 인덱스 없이 표 전체를 훑었고(증분 019 가 고쳤다), 그 퇴행은 작은 원장에서 0 에 가깝다.
// ★ 원장은 합성이다(seedFullLedger). 운영 원장의 값은 쓰지 않는다 - 모양(행 수 · 종류 비율 · 프로젝트 수)만 닮게 둔다.
func BenchmarkRESTLifecycleFullLedger(b *testing.B) {
	for _, e := range []freshstore.Engine{freshstore.SQLite, freshstore.Postgres} {
		b.Run("engine="+string(e), func(b *testing.B) {
			st, at := freshstore.OpenOn(b, e, nil)
			seedFullLedger(b, e, at)
			benchLifecycle(b, newRESTBenchOn(b, 1, st), 1, true)
		})
	}
}

// 전 크기 원장 모양: 이벤트 수십만 행이 남의 프로젝트 스물과 세션 수백에 걸쳐 있고, 대부분은 처방 · 박동이며 항목 마무리는
// 드물다(프로젝트마다 LIMIT 보다 훨씬 적다). 벤치 프로젝트는 비어서 시작한다 - 종료 선언 조회가 채울 것이 없는 쪽이다.
const (
	fullLedgerEvents   = 300_000
	fullLedgerProjects = 20
	fullLedgerSessions = 900
	fullLedgerBatch    = 1000
)

// fullLedgerKind 는 i 번째 합성 이벤트의 종류다. 비율만 닮게 둔다.
func fullLedgerKind(i int) string {
	switch {
	case i%160 == 0:
		return "item.finish"
	case i%150 == 0:
		return "item.add"
	case i%40 == 0:
		return "item.claim"
	case i%50 == 0:
		return "landing"
	case i%5 == 0:
		return "prescribe"
	}
	return "session.beat"
}

// seedFullLedger 는 엔진 e 의 원장 자리 at 에 합성 이벤트를 넣는다. 값은 이 파일의 상수뿐이라 문장에 그대로 싣는다
// (엔진마다 다른 자리표 문법을 피한다). Postgres 는 계획이 통계로 고르므로 넣은 뒤 ANALYZE 한다.
func seedFullLedger(tb testing.TB, e freshstore.Engine, at string) {
	tb.Helper()
	driver := map[freshstore.Engine]string{freshstore.SQLite: "sqlite", freshstore.Postgres: "pgx"}[e]
	db, err := sql.Open(driver, at)
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback()
	// 시각은 store 의 저장 표기(폭 고정, timeLayout)를 따른다 - 사전순이 시간순이어야 한다. 벤치가 쓰는 행보다 앞선다.
	base := time.Now().Add(-90 * 24 * time.Hour)
	var q strings.Builder
	for i := 1; i <= fullLedgerEvents; i++ {
		if q.Len() == 0 {
			q.WriteString(`INSERT INTO event(at, project, session_id, kind, payload) VALUES `)
		} else {
			q.WriteByte(',')
		}
		fmt.Fprintf(&q, `('%s', 'p%02d', 's%03d', '%s', '{"item":"s-%d","mode":"done"}')`,
			base.Add(time.Duration(i)*10*time.Second).UTC().Format("2006-01-02T15:04:05.000000Z"),
			i%fullLedgerProjects, i%fullLedgerSessions, fullLedgerKind(i), i)
		if i%fullLedgerBatch == 0 || i == fullLedgerEvents {
			if _, err := tx.ExecContext(ctx, q.String()); err != nil {
				tb.Fatalf("합성 이벤트 넣기: %v", err)
			}
			q.Reset()
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	if e == freshstore.Postgres {
		if _, err := db.ExecContext(ctx, `ANALYZE event`); err != nil {
			tb.Fatal(err)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM event`).Scan(&n); err != nil || n < fullLedgerEvents {
		tb.Fatalf("합성 원장의 이벤트가 %d 행이다(기대 %d 이상): %v", n, fullLedgerEvents, err)
	}
}

// mustAllDone 은 한살이 b.N 번이 원장에서도 다 끝났는지 본다. 2xx 인데 다른 갈래(재개 · 거절을
// 담은 응답)를 탔다면 잰 것이 pick · finish 가 아니다.
func (r *restBench) mustAllDone(b *testing.B) {
	b.Helper()
	items, err := r.st.ListItems(context.Background(), benchProject)
	if err != nil {
		b.Fatal(err)
	}
	var notDone int
	for _, it := range items {
		if it.State != model.ItemDone {
			notDone++
		}
	}
	if len(items) != b.N || notDone > 0 {
		b.Fatalf("항목 %d개(기대 %d) 중 done 이 아닌 것 %d", len(items), b.N, notDone)
	}
}

func (r *restBench) lifecycle(ctx context.Context, sess string, i int64, lat *latencies) error {
	id := fmt.Sprintf("b-%06d", i)
	if _, err := r.post(ctx, "/api/v1/items", map[string]any{
		"project": benchProject, "session_id": sess, "id": id, "title": id + " 제목",
		"body": "벤치 항목 본문", "paths": []string{"bench/" + id + "/"},
	}, nil); err != nil {
		return err
	}
	for _, step := range []struct {
		op, path string
		body     map[string]any
	}{
		{"pick", "/api/v1/items/" + id + "/claim", map[string]any{
			"project": benchProject, "session_id": sess}},
		{"note", "/api/v1/judgments", map[string]any{
			"project": benchProject, "session_id": sess, "kind": string(model.JudgmentDecision),
			"title": id + " 결정", "body": "벤치 판단 본문", "item_id": id}},
		{"finish", "/api/v1/items/" + id + "/finish", map[string]any{
			"project": benchProject, "session_id": sess, "outcome": string(model.ItemDone),
			"title": id + " 마무리", "body": "벤치 마무리 본문"}},
	} {
		d, err := r.post(ctx, step.path, step.body, nil)
		if err != nil {
			return err
		}
		lat.add(step.op, d)
	}
	return nil
}

// laneHold 는 차례를 받은 뒤 반납을 보내기까지 쥐는 시간이다(랜딩 일 흉내). 0 이면 보유 구간이
// 비어, 서버가 남이 쥔 레인에 차례를 줘도 구간이 겹칠 틈이 없어 안 보인다.
const laneHold = time.Millisecond

// laneWaitLimit 는 차례 한 번을 기다리는 상한이다. 레인이 안 풀리는 결함이면 벤치가 끝없이 도는
// 대신 여기서 실패한다.
const laneWaitLimit = 30 * time.Second

// BenchmarkRESTLane 은 세션 8 개가 레인 하나를 두고 다툰다. op 하나 = 랜딩 한 번
// (차례가 올 때까지 acquire → laneHold 만큼 쥐기 → report ok).
//
// 지연은 랜딩마다 한 표본씩 낸다: enqueue(첫 acquire 왕복) · turn(첫 acquire 를 보낸 때부터 차례를
// 받기까지) · report(반납 왕복). 기다리는 쪽의 폴링(1ms 쉬고 다시 acquire)은 표본에 안 넣는다,
// 넣으면 표본 수가 엔진 속도에 달린다. 실물 클라이언트는 초 단위로 기다리니 이 벤치가 훨씬 거세게 다툰다.
//
// 이중 보유는 랜딩마다 [차례 받음, 반납 보냄] 구간을 모아 겹침으로 본다. 반납이 커밋되기 전에는
// 아무도 차례를 못 받아야 하므로 정상이면 다음 차례는 앞 구간의 끝(반납 보냄)보다 뒤에 온다.
// 겹치면 서버가 남이 쥔 레인에 차례를 준 것이다. 끝나고 원장에 남은 점유도 0 이어야 한다.
func BenchmarkRESTLane(b *testing.B) {
	const n = 8
	r := newRESTBench(b, n)
	sessions := r.openSessions(b)
	var spans holdSpans
	var lat latencies
	b.ResetTimer()
	err := clients(b, n, func(ctx context.Context, k int, _ int64) error {
		return r.landOnce(ctx, sessions[k], &spans, &lat)
	})
	b.StopTimer()
	if err != nil {
		b.Fatal(err)
	}
	held, err := r.st.ListHeld(context.Background(), benchProject)
	if err != nil {
		b.Fatal(err)
	}
	doubles, count := spans.overlaps()
	b.ReportMetric(float64(doubles), "double-holds")
	lat.report(b, "enqueue", "turn", "report")
	if doubles > 0 || len(held) > 0 || count != b.N {
		b.Fatalf("랜딩 %d번(보유 구간 %d개) 중 이중 보유 %d · 끝난 뒤 남은 점유 %d",
			b.N, count, doubles, len(held))
	}
}

func (r *restBench) landOnce(ctx context.Context, sess string, spans *holdSpans, lat *latencies) error {
	acquire := map[string]any{"project": benchProject, "session_id": sess, "mode": api.LandModeAcquire}
	start := time.Now()
	for polls := 0; ; polls++ {
		var res struct {
			State string `json:"state"`
		}
		d, err := r.post(ctx, "/api/v1/landing", acquire, &res)
		if err != nil {
			return err
		}
		if polls == 0 {
			lat.add("enqueue", d)
		}
		if res.State == "turn" {
			break
		}
		if time.Since(start) > laneWaitLimit {
			return fmt.Errorf("세션 %s: %s 안에 차례가 안 왔다(폴링 %d번)", sess, laneWaitLimit, polls+1)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	from := time.Now()
	lat.add("turn", from.Sub(start))
	time.Sleep(laneHold)
	spans.add(from, time.Now())
	d, err := r.post(ctx, "/api/v1/landing", map[string]any{
		"project": benchProject, "session_id": sess, "mode": api.LandModeReport,
		"kind": string(model.LandingLeftOK), "detail": "벤치 랜딩"}, nil)
	if err != nil {
		return err
	}
	lat.add("report", d)
	return nil
}

// holdSpans 는 차례 보유 구간 [차례 받음, 반납 보냄] 을 모은다. 여러 고루틴이 함께 쓴다.
type holdSpans struct {
	mu    sync.Mutex
	spans [][2]time.Time
}

func (h *holdSpans) add(from, to time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.spans = append(h.spans, [2]time.Time{from, to})
}

// overlaps 는 구간을 시작 순으로 늘어놓아, 앞 구간들이 끝나기 전에 시작한 구간의 수와 구간 수를 낸다.
func (h *holdSpans) overlaps() (doubles, count int) {
	h.mu.Lock()
	s := slices.Clone(h.spans)
	h.mu.Unlock()
	slices.SortFunc(s, func(a, b [2]time.Time) int { return a[0].Compare(b[0]) })
	var end time.Time
	for i, sp := range s {
		if i > 0 && sp[0].Before(end) {
			doubles++
		}
		if sp[1].After(end) {
			end = sp[1]
		}
	}
	return doubles, len(s)
}

// latencies 는 요청 종류마다 왕복 시간을 모은다. 여러 고루틴이 함께 쓴다.
type latencies struct {
	mu sync.Mutex
	by map[string][]time.Duration
}

func (l *latencies) add(op string, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.by == nil {
		l.by = map[string][]time.Duration{}
	}
	l.by[op] = append(l.by[op], d)
}

// report 는 종류마다 p50 · p95 · p99 를 ms 로 낸다. 순위는 가까운 순위(nearest-rank)다:
// 표본을 정렬해 ⌈p·n⌉ 번째 값. 보간하지 않으니 낸 값은 언제나 실제로 잰 한 표본이다.
func (l *latencies) report(b *testing.B, ops ...string) {
	b.Helper()
	for _, op := range ops {
		ds := slices.Sorted(slices.Values(l.by[op]))
		if len(ds) == 0 {
			b.Fatalf("%s 표본이 없다", op)
		}
		for _, q := range []struct {
			name string
			p    float64
		}{{"p50", 0.50}, {"p95", 0.95}, {"p99", 0.99}} {
			i := max(int(float64(len(ds))*q.p+0.999999)-1, 0)
			b.ReportMetric(float64(ds[i])/float64(time.Millisecond), op+"-"+q.name+"-ms")
		}
	}
}

// stillGit 은 움직이지 않는 저장소다: 커밋 하나, 워크트리마다 제 브랜치, 미커밋 변경 없음,
// 워크스페이스 명부 파일 없음. git 을 띄우지 않고 곧바로 답한다.
type stillGit struct{ worktrees []gitreader.Worktree }

const stillSHA = "0123456789abcdef0123456789abcdef01234567"

func newStillGit(repo string, worktrees []string) stillGit {
	g := stillGit{worktrees: []gitreader.Worktree{{Path: repo, HEAD: stillSHA, Branch: "refs/heads/main"}}}
	for _, wt := range worktrees {
		g.worktrees = append(g.worktrees, gitreader.Worktree{
			Path: wt, HEAD: stillSHA, Branch: "refs/heads/" + filepath.Base(wt)})
	}
	return g
}

func (g stillGit) Refs(context.Context) ([]model.RefState, error) {
	out := make([]model.RefState, 0, len(g.worktrees))
	for _, w := range g.worktrees {
		out = append(out, model.RefState{Ref: w.Branch, SHA: stillSHA})
	}
	return out, nil
}

func (stillGit) Ref(_ context.Context, ref string) (model.RefState, error) {
	return model.RefState{Ref: ref, SHA: stillSHA}, nil
}

func (g stillGit) Worktrees(context.Context) ([]gitreader.Worktree, error) {
	return slices.Clone(g.worktrees), nil
}

func (stillGit) ChangedPaths(context.Context, string, string) ([]string, map[string]model.LineDelta, error) {
	return nil, nil, nil
}

func (stillGit) MergeBase(context.Context, string, string) (string, error) { return stillSHA, nil }

func (stillGit) UncommittedPaths(context.Context, string) ([]string, error) { return nil, nil }

func (stillGit) UncommittedDelta(context.Context, string) (map[string]model.LineDelta, error) {
	return nil, nil
}

func (stillGit) AheadBehind(context.Context, string, string) (int, int, error) { return 0, 0, nil }

func (stillGit) Ancestry(context.Context, string, string) (judge.AncestryResult, error) {
	return judge.AncestryYes, nil
}

func (stillGit) FileAt(_ context.Context, ref, path string) ([]byte, error) {
	return nil, fmt.Errorf("%s:%s: %w", ref, path, gitreader.ErrFileNotInRef)
}
