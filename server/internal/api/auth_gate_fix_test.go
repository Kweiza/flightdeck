package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/auth"
)

// hookedGate 는 진짜 게이트 앞에 끼우는 판정기다. arm(n, after) 뒤 n 번째 판정이 끝난 직후(판정은 났고 그 결과를 쓰기 전)
// after 를 한 번 부른다 - 1 번째는 게이트와 핸들러 사이, /events 의 2 번째는 구독 뒤 다시 재기의 바로 뒤다. 시험이 틈을 정확히
// 그 자리에서 연다. after 안의 요청은 세지 않는다(부르기 전에 내린다).
type hookedGate struct {
	inner auth.Authenticator
	mu    sync.Mutex
	left  int
	after func()
}

func (h *hookedGate) Authenticate(ctx context.Context, req auth.Request) auth.Decision {
	d := h.inner.Authenticate(ctx, req)
	h.mu.Lock()
	fire := false
	if h.left > 0 {
		h.left--
		fire = h.left == 0
	}
	h.mu.Unlock()
	if fire {
		h.after()
	}
	return d
}

func (h *hookedGate) arm(nth int, after func()) {
	h.mu.Lock()
	h.left, h.after = nth, after
	h.mu.Unlock()
}

// revokeKimStream 은 관리자 · 회원 kim 과 둘의 기기 토큰을 만들고, 그 뒤 nth 번째 판정 바로 뒤에 kim 의 기기를 철회하게 건다.
// 철회가 끊은 스트림 수가 채널로 온다.
func revokeKimStream(t *testing.T, nth int) (e *env, ts *httptest.Server, adminTok, kimTok string, revoked chan int) {
	t.Helper()
	hg := &hookedGate{}
	e = newEnv(t, func(o *Options) {
		o.Token = sharedToken
		o.AuthMode = auth.ModeAccounts
		o.Heartbeat = time.Hour
		o.Authenticator = hg
	})
	hg.inner = auth.NewGate(auth.ModeAccounts, sharedToken, e.st, nil)
	ts = httptest.NewServer(e.h)
	t.Cleanup(ts.Close)

	g := e.bootstrapOn("admin")
	adminTok = e.device("m-admin", g.AccountID)
	e.member("kim")
	kimTok = e.device("m-kim", "kim")

	revoked = make(chan int, 1)
	hg.arm(nth, func() {
		w := e.write(http.MethodPost, "/api/v1/devices/m-kim/revoke", map[string]any{}, bearer(adminTok))
		if w.Code != http.StatusOK {
			t.Errorf("틈 안의 철회 %d %s", w.Code, w.Body.String())
		}
		revoked <- int(decodeBody(t, w)["streams_cut"].(float64))
	})
	return e, ts, adminTok, kimTok, revoked
}

// TestEventsRecheckCatchesARevokeBetweenGateAndSubscribe 는 막음 1 의 재현이다: /events 가 게이트를 지난 뒤 · 구독을 걸기 전에
// 그 기기의 철회가 커밋되고 Cut 이 돈다. Cut 은 아직 없는 구독을 못 고르므로, 구독 뒤에 다시 재지 않으면 이 스트림은 철회된
// 토큰으로 열린 채 남는다.
func TestEventsRecheckCatchesARevokeBetweenGateAndSubscribe(t *testing.T) {
	e, ts, adminTok, kimTok, revoked := revokeKimStream(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events?project="+testProject, nil)
	if err != nil {
		t.Fatal(err)
	}
	bearer(kimTok)(req)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if n := <-revoked; n != 0 {
		t.Fatalf("전제가 깨졌다 - 철회가 구독 뒤에 돌았다(끊은 스트림 %d)", n)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		first, _ := bufio.NewReader(resp.Body).ReadString('\n')
		t.Fatalf("게이트와 구독 사이에 철회된 토큰의 스트림이 %d 로 열렸다(첫 줄 %q) - 구독 뒤에 자격증명을 다시 재야 한다",
			resp.StatusCode, first)
	}
	waitFor(t, "거절된 구독의 해지", 2*time.Second, func() bool {
		return strings.Contains(e.do(http.MethodGet, "/metrics", nil, bearer(adminTok)).Body.String(), "\nflightdeck_sse_subscribers 0\n")
	})
}

// TestEventsSubscribeBeforeTheRecheck 는 다시 재기의 순서다: 구독을 건 **뒤에** 잰다. 재기가 통과한 바로 뒤에 철회가 커밋되면
// Cut 이 그 구독을 골라야 한다. 재기를 구독 앞에 두면 그 틈의 철회는 아직 없는 구독을 못 골라 스트림이 철회된 토큰으로 열린 채다.
func TestEventsSubscribeBeforeTheRecheck(t *testing.T) {
	_, ts, _, kimTok, revoked := revokeKimStream(t, 2)
	s := openStream(t, ts, "/events?project="+testProject, bearer(kimTok))
	if n := <-revoked; n != 1 {
		t.Fatalf("재기 뒤의 철회가 끊은 스트림이 %d 다 - 구독이 재기보다 먼저 걸려 있어야 한다", n)
	}
	s.cutWithin(t, "재기 뒤의 철회", time.Second)
}

// TestEventsCloseAtWebSessionExpiry 는 화면 세션 스트림이 세션 만료 시각에 스스로 닫히는가다. 열린 스트림에는 다음 요청이 없어
// 게이트가 만료를 다시 못 본다.
func TestEventsCloseAtWebSessionExpiry(t *testing.T) {
	var offset atomic.Int64
	clock := func() time.Time { return time.Now().UTC().Add(time.Duration(offset.Load())) }
	e := newEnv(t, func(o *Options) {
		o.Token = sharedToken
		o.AuthMode = auth.ModeAccounts
		o.Heartbeat = time.Hour
		o.Clock = clock
	})
	ts := httptest.NewServer(e.h)
	t.Cleanup(ts.Close)
	g := e.bootstrapOn("admin")
	cookie := e.exchange(g.LoginCode)
	// 만료 400ms 앞으로 시계를 민다 - 게이트는 아직 통과시키고, 스트림은 곧 닫혀야 한다.
	offset.Store(int64(auth.SessionTTL - 400*time.Millisecond))
	s := openStream(t, ts, "/events?project="+testProject, withCookie(SessionCookieName, cookie))
	select {
	case rest := <-s.done:
		if !strings.Contains(rest, ": bye 세션이 만료됐다") {
			t.Fatalf("스트림이 끝났지만 만료 사유가 없다: %q", rest)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("만료 시각이 지났는데 화면 세션 스트림이 열려 있다")
	}
}

// TestProjectRemoveIsAdminOnlyAndActorComesFromThePrincipal 은 막음 2 다: 회원의 기기 토큰으로는 프로젝트를 못 지우고(세기도
// 못 한다), 관리자가 지운 기록의 행위자는 본문이 아니라 인증 주체다.
func TestProjectRemoveIsAdminOnlyAndActorComesFromThePrincipal(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Token = sharedToken })
	g := e.bootstrapOn("admin")
	adminTok := e.device("m-admin", g.AccountID)
	e.member("kim")
	kimTok := e.device("m-kim", "kim")
	w := e.write(http.MethodPost, "/api/v1/sessions", map[string]any{
		"project": "gone", "project_path": e.repo, "machine_id": "m1", "hostname": "host1",
		"worktree": e.repo, "cc_session_id": "cc-gone",
	}, bearer(sharedToken))
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("세션 열기 %d %s", w.Code, w.Body.String())
	}
	body := map[string]any{"actor": "누군가-다른-사람", "reason": "시험 정리", "confirm": true}
	for _, confirm := range []bool{false, true} {
		body["confirm"] = confirm
		if w := e.write(http.MethodPost, "/api/v1/projects/gone/remove", body, bearer(kimTok)); w.Code != http.StatusForbidden ||
			errorOf(t, w)["code"] != "admin_only" {
			t.Fatalf("회원의 프로젝트 지우기(confirm=%v)가 %d 다: %s", confirm, w.Code, w.Body.String())
		}
	}
	if _, err := e.st.GetProject(context.Background(), "gone"); err != nil {
		t.Fatalf("거절했는데 프로젝트가 없어졌다: %v", err)
	}
	gone := e.okBody(t, e.write(http.MethodPost, "/api/v1/projects/gone/remove", body, bearer(adminTok)), "관리자의 지우기")
	if gone["removed"] != true {
		t.Fatalf("관리자가 못 지웠다: %v", gone)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(e.rawCell("SELECT payload FROM event WHERE kind = 'project.removed'")), &doc); err != nil {
		t.Fatal(err)
	}
	if want := "account:" + g.AccountID + " machine:m-admin"; doc["actor"] != want || doc["claimed_actor"] != "누군가-다른-사람" {
		t.Fatalf("project.removed 의 행위자가 %v · 자기 신고가 %v 다 - 행위자는 주체 %q, 본문은 claimed_actor 로 따로여야 한다",
			doc["actor"], doc["claimed_actor"], want)
	}
}

// TestProjectRemoveRecordsAccountlessPrincipalsByKind 는 고침 2 의 중요 6 이다: 계정이 없는 주체(공유 토큰 · 무인증)가 지운 기록의
// 행위자도 본문의 자기 신고가 아니라 그 주체의 종류(shared · anonymous)다. 고치기 전에는 본문 actor 가 행위자 칸에 그대로 들어가
// 누구든 남의 계정 이름(account:… 꼴까지)을 적어 계정 행위처럼 보이는 감사 기록을 남겼다. 자기 신고는 claimed_actor 로 따로 남는다.
func TestProjectRemoveRecordsAccountlessPrincipalsByKind(t *testing.T) {
	for _, c := range []struct {
		kind  string
		tune  func(*Options)
		creds []reqOpt
	}{
		{"shared", func(o *Options) { o.Token = sharedToken }, []reqOpt{bearer(sharedToken)}},
		{"anonymous", nil, nil},
	} {
		t.Run(c.kind, func(t *testing.T) {
			e := newEnv(t, c.tune)
			if w := e.write(http.MethodPost, "/api/v1/sessions", map[string]any{
				"project": "gone", "project_path": e.repo, "machine_id": "m1", "hostname": "host1",
				"worktree": e.repo, "cc_session_id": "cc-gone",
			}, c.creds...); w.Code != http.StatusCreated && w.Code != http.StatusOK {
				t.Fatalf("세션 열기 %d %s", w.Code, w.Body.String())
			}
			forged := "account:someone-else machine:m-x"
			gone := e.okBody(t, e.write(http.MethodPost, "/api/v1/projects/gone/remove",
				map[string]any{"actor": forged, "reason": "시험 정리", "confirm": true}, c.creds...), "지우기")
			if gone["removed"] != true {
				t.Fatalf("못 지웠다: %v", gone)
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(e.rawCell("SELECT payload FROM event WHERE kind = 'project.removed'")), &doc); err != nil {
				t.Fatal(err)
			}
			if doc["actor"] != c.kind || doc["claimed_actor"] != forged {
				t.Fatalf("행위자 %v · 자기 신고 %v 다 - 행위자는 주체의 종류 %q, 본문은 claimed_actor 여야 한다", doc["actor"], doc["claimed_actor"], c.kind)
			}
		})
	}
}

// TestActorOfComesOnlyFromThePrincipal 은 ActorOf 의 순수 계약이다: 행위자는 주체에서만 나오고 빈 글이 아니다.
func TestActorOfComesOnlyFromThePrincipal(t *testing.T) {
	for _, c := range []struct {
		p    auth.Principal
		want string
	}{
		{auth.Principal{Kind: auth.KindDevice, AccountID: "a1", MachineID: "m1", Role: auth.RoleAdmin}, "account:a1 machine:m1"},
		{auth.Principal{Kind: auth.KindWeb, AccountID: "a1", WebSession: "h", Role: auth.RoleMember}, "account:a1"},
		{auth.Principal{Kind: auth.KindShared, Role: auth.RoleAdmin}, "shared"},
		{auth.Principal{Kind: auth.KindAnonymous, Role: auth.RoleAdmin}, "anonymous"},
		{auth.Principal{Kind: auth.KindBootstrap}, "bootstrap"},
		{auth.Principal{}, "unknown"},
	} {
		if got := ActorOf(c.p); got != c.want {
			t.Errorf("ActorOf(%+v) = %q, %q 여야 한다", c.p, got, c.want)
		}
	}
}

// TestIdempotencyReplayIsScopedByPrincipal 은 막음 3 이다: 관리자와 회원이 같은 키 · 경로 · 본문을 보내도 서로의 응답을 재생받지
// 않는다. 두 순서를 다 잰다 - 관리자가 먼저면 회원이 인가를 안 거친 200 을, 회원이 먼저면 관리자가 403 을 재생받는다.
func TestIdempotencyReplayIsScopedByPrincipal(t *testing.T) {
	for _, order := range []string{"관리자 먼저", "회원 먼저"} {
		t.Run(order, func(t *testing.T) {
			e := newEnv(t, func(o *Options) { o.Token = sharedToken; o.AuthMode = auth.ModeAccounts })
			g := e.bootstrapOn("admin")
			adminTok := e.device("m-admin", g.AccountID)
			e.member("kim")
			e.member("lee")
			kimTok := e.device("m-kim", "kim")
			path, body, key := "/api/v1/admin/accounts/lee", map[string]any{"disabled": true}, withKey("same:1")
			admin := func() *httptest.ResponseRecorder { return e.do(http.MethodPatch, path, body, bearer(adminTok), key) }
			member := func() *httptest.ResponseRecorder { return e.do(http.MethodPatch, path, body, bearer(kimTok), key) }
			if order == "관리자 먼저" {
				if w := admin(); w.Code != http.StatusOK {
					t.Fatalf("관리자 %d %s", w.Code, w.Body.String())
				}
				if w := member(); w.Code != http.StatusForbidden || w.Header().Get("Idempotency-Replayed") != "" {
					t.Fatalf("회원이 같은 키로 관리자 응답을 받았다: %d replayed=%q %s", w.Code, w.Header().Get("Idempotency-Replayed"), w.Body.String())
				}
				return
			}
			if w := member(); w.Code != http.StatusForbidden {
				t.Fatalf("회원 %d %s", w.Code, w.Body.String())
			}
			if w := admin(); w.Code != http.StatusOK || w.Header().Get("Idempotency-Replayed") != "" {
				t.Fatalf("관리자가 같은 키로 회원의 403 을 재생받았다: %d %s", w.Code, w.Body.String())
			}
			// 같은 주체의 재시도는 그대로 재생된다(범위가 재생을 끄지 않는다).
			if w := admin(); w.Header().Get("Idempotency-Replayed") != "true" {
				t.Fatalf("같은 주체의 재시도가 재생되지 않았다: %d", w.Code)
			}
		})
	}
}

// TestPersistedIdempotencyIsScopedByPrincipalAcrossRestart 는 원장 쪽 멱등 표도 주체로 갈리는가다(재기동 뒤 메모리가 빈 자리).
// 공유 토큰(옛 판의 유일한 주체)의 키는 원문 그대로 남아 올리기 전의 기록이 올린 뒤에도 맞는다.
func TestPersistedIdempotencyIsScopedByPrincipalAcrossRestart(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Token = sharedToken })
	g := e.bootstrapOn("admin")
	adminTok := e.device("m-admin", g.AccountID)
	e.member("kim")
	kimTok := e.device("m-kim", "kim")
	if w := e.write(http.MethodPost, "/api/v1/sessions", map[string]any{
		"project": testProject, "project_path": e.repo, "machine_id": "m1", "hostname": "host1",
		"worktree": e.repo, "cc_session_id": "cc-idem",
	}, bearer(sharedToken)); w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("세션 열기 %d %s", w.Code, w.Body.String())
	}
	note := map[string]any{"project": testProject, "kind": "decision", "title": "같은 판단", "body": "같은 본문"}
	for _, tok := range []string{sharedToken, adminTok} {
		if w := e.do(http.MethodPost, "/api/v1/judgments", note, bearer(tok), withKey("j:1")); w.Code >= 300 {
			t.Fatalf("판단 %d %s", w.Code, w.Body.String())
		}
	}
	keys := map[string]bool{}
	rows, err := e.st.DB().Query(`SELECT key FROM idempotency`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys[k] = true
	}
	rows.Close()
	if !keys["j:1"] || !keys[ScopeIdempotencyKey(auth.Principal{Kind: auth.KindDevice, AccountID: g.AccountID, MachineID: "m-admin"}, "j:1")] {
		t.Fatalf("원장 멱등 표의 키가 주체로 안 갈렸다: %v", keys)
	}
	e.restart()
	if w := e.do(http.MethodPost, "/api/v1/judgments", note, bearer(kimTok), withKey("j:1")); w.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("재기동 뒤 회원이 남의 판단 응답을 재생받았다: %d %s", w.Code, w.Body.String())
	}
	if w := e.do(http.MethodPost, "/api/v1/judgments", note, bearer(sharedToken), withKey("j:1")); w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("재기동 뒤 공유 토큰의 같은 키가 재생되지 않았다: %d %s", w.Code, w.Body.String())
	}
}

// TestScopeIdempotencyKeyKeepsPrincipalsApart 는 범위 붙이기의 순수 계약이다: 주체가 다르면 키가 다르고, 빈 범위(공유 토큰 ·
// 무인증)의 원문은 어떤 범위 키와도 겹칠 수 없다(검사를 지난 원문에는 공백이 없다).
func TestScopeIdempotencyKeyKeepsPrincipalsApart(t *testing.T) {
	ps := []auth.Principal{
		{Kind: auth.KindDevice, AccountID: "a", MachineID: "m"},
		{Kind: auth.KindDevice, AccountID: "a", MachineID: "m2"},
		{Kind: auth.KindDevice, AccountID: "a m", MachineID: ""},
		{Kind: auth.KindWeb, AccountID: "a", WebSession: "m"},
		{Kind: auth.KindWeb, AccountID: "b", WebSession: "m"},
		{Kind: auth.KindBootstrap},
		{},
	}
	seen := map[string]auth.Principal{}
	for _, p := range ps {
		k := ScopeIdempotencyKey(p, "s:1")
		if prev, dup := seen[k]; dup {
			t.Fatalf("%+v 와 %+v 가 같은 키 %q 다", prev, p, k)
		}
		seen[k] = p
		if !strings.Contains(k, " ") {
			t.Fatalf("%+v 의 범위 키 %q 에 공백이 없다 - 원문 키와 겹칠 수 있다", p, k)
		}
		if v := JudgeIdempotencyKey(http.MethodPost, k); v.OK {
			t.Fatalf("범위 키 %q 가 원문 키 검사를 지난다 - 공유 토큰이 그 원문을 보내 남의 칸을 열 수 있다", k)
		}
	}
	for _, k := range []auth.Kind{auth.KindShared, auth.KindAnonymous} {
		if got := ScopeIdempotencyKey(auth.Principal{Kind: k, Role: auth.RoleAdmin}, "s:1"); got != "s:1" {
			t.Fatalf("%s 의 키가 %q 다 - 옛 판의 기록이 맞게 원문 그대로여야 한다", k, got)
		}
	}
}

// streamRest 는 /events 를 열고 첫 줄(: connected)을 읽은 뒤 after 를 부르고, 남은 글을 끝(EOF)까지 읽는다. d 안에 안 끝나면
// 그때까지 읽은 글과 거짓을 낸다 - 열린 채 남은 스트림이 무엇을 썼는지까지 보인다.
func streamRest(t *testing.T, ts *httptest.Server, path string, d time.Duration, after func(), opts ...reqOpt) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("구독이 %d 다", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	if first := readFrame(t, br); !strings.HasPrefix(first, ": connected") {
		t.Fatalf("구독 성립 줄이 %q 다", first)
	}
	if after != nil {
		after()
	}
	rest, err := io.ReadAll(br)
	return string(rest), err == nil
}

// testFrame 은 시험이 허브에 직접 미는 이벤트 하나다 - 구독자 버퍼에 프레임을 정확히 원하는 순간에 넣는다.
func testFrame(t *testing.T, h *Hub) {
	t.Helper()
	if err := h.Publish("t-1", Event{Kind: "test.frame", Project: testProject, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

// bothReadyRounds 는 「끝낼 신호와 프레임이 함께 준비된」 판을 몇 번 돌리는가다. select 는 준비된 갈래를 무작위로 고르므로, 쓰기 직전
// 확인이 없는 구현은 한 판마다 반의 확률로 프레임을 먼저 쓴다 - 24 판을 다 빠져나갈 확률은 2^-24 다.
const bothReadyRounds = 24

// TestEventsWriteNoFrameOnceRevokedEvenIfAFrameIsReady 는 고침 2 의 막음 1 이다: 구독 뒤 다시 재기가 통과한 바로 뒤(스트림이 루프에
// 들기 전)에 프레임 하나가 버퍼에 들고 그 기기가 철회된다. 철회와 프레임이 함께 준비된 채 루프에 들어가도 데이터는 0건이고 작별 줄로
// 끝나야 한다.
func TestEventsWriteNoFrameOnceRevokedEvenIfAFrameIsReady(t *testing.T) {
	hg := &hookedGate{}
	e := newEnv(t, func(o *Options) {
		o.Token = sharedToken
		o.AuthMode = auth.ModeAccounts
		o.Heartbeat = time.Hour
		o.Authenticator = hg
	})
	hg.inner = auth.NewGate(auth.ModeAccounts, sharedToken, e.st, nil)
	ts := httptest.NewServer(e.h)
	t.Cleanup(ts.Close)
	g := e.bootstrapOn("admin")
	adminTok := e.device("m-admin", g.AccountID)
	e.member("kim")
	for i := 0; i < bothReadyRounds; i++ {
		machine := "m-kim-" + itoa(i+1)
		tok := e.device(machine, "kim")
		cuts := make(chan int, 1)
		hg.arm(2, func() {
			testFrame(t, e.srv.hub)
			w := e.write(http.MethodPost, "/api/v1/devices/"+machine+"/revoke", map[string]any{}, bearer(adminTok))
			if w.Code != http.StatusOK {
				t.Errorf("틈 안의 철회 %d %s", w.Code, w.Body.String())
				cuts <- -1
				return
			}
			cuts <- int(decodeBody(t, w)["streams_cut"].(float64))
		})
		rest, ended := streamRest(t, ts, "/events?project="+testProject, 3*time.Second, nil, bearer(tok))
		if cut := <-cuts; cut != 1 {
			t.Fatalf("%d 판: 전제가 깨졌다 - 철회가 끊은 스트림이 %d 다(구독이 걸린 뒤여야 한다)", i+1, cut)
		}
		if !ended || strings.Contains(rest, "data:") || !strings.Contains(rest, ": bye 자격증명이 철회됐다") {
			t.Fatalf("%d 판: 철회와 프레임이 함께 준비됐는데 스트림이 %q 를 썼다(끝남 %v) - 철회된 뒤에는 데이터 0건이어야 한다",
				i+1, rest, ended)
		}
	}
}

// TestEventsWriteNoFrameAfterTheSessionExpires 는 만료 쪽이다: 화면 세션 스트림이 열린 뒤 시계가 만료 시각을 지나고, 만료
// 타이머가 울리기 전에 프레임이 온다. 쓰기 직전에 만료 **시각**을 재야 그 프레임을 안 낸다 - 타이머 채널만 보면 프레임이 먼저 나간다.
func TestEventsWriteNoFrameAfterTheSessionExpires(t *testing.T) {
	var offset atomic.Int64
	clock := func() time.Time { return time.Now().UTC().Add(time.Duration(offset.Load())) }
	e := newEnv(t, func(o *Options) {
		o.Token = sharedToken
		o.AuthMode = auth.ModeAccounts
		o.Heartbeat = time.Hour
		o.Clock = clock
	})
	ts := httptest.NewServer(e.h)
	t.Cleanup(ts.Close)
	g := e.bootstrapOn("admin")
	cookie := e.exchange(g.LoginCode)
	rest, ended := streamRest(t, ts, "/events?project="+testProject, 2*time.Second, func() {
		offset.Store(int64(auth.SessionTTL + time.Minute)) // 타이머는 실제 시각으로 14일 뒤에야 운다
		testFrame(t, e.srv.hub)
	}, withCookie(SessionCookieName, cookie))
	if !ended || strings.Contains(rest, "data:") || !strings.Contains(rest, ": bye 세션이 만료됐다") {
		t.Fatalf("만료 시각이 지난 뒤 온 프레임에 스트림이 %q 를 썼다(끝남 %v) - 데이터 0건 · 만료 작별이어야 한다", rest, ended)
	}
}

// TestEventsWriteNoFrameOnceDrainingEvenIfAFrameIsReady 는 종료 쪽이다: 다시 재기 바로 뒤에 프레임이 버퍼에 들고 서버가 종료
// 통지를 받는다. 종료 통지를 받은 뒤에는 버퍼에 남은 프레임을 안 쓴다(읽지 않는 피어면 그 쓰기에서 막혀 셧다운 유예를 다 쓴다).
// 종료는 서버 한 벌에 한 번이라 판마다 원장 없는 서버를 새로 세운다(이 경로는 원장을 안 쓴다).
func TestEventsWriteNoFrameOnceDrainingEvenIfAFrameIsReady(t *testing.T) {
	for i := 0; i < bothReadyRounds; i++ {
		hg := &hookedGate{inner: auth.NewGate(auth.ModeTransition, "", nil, nil)}
		srv := newServer(nil, Options{Heartbeat: time.Hour, Authenticator: hg})
		ts := httptest.NewServer(srv.chain(srv.routes()))
		hg.arm(2, func() {
			testFrame(t, srv.hub)
			srv.Drain()
		})
		rest, ended := streamRest(t, ts, "/events?project="+testProject, 3*time.Second, nil)
		ts.Close()
		if !ended || strings.Contains(rest, "data:") || !strings.Contains(rest, ": bye 서버가 종료한다") {
			t.Fatalf("%d 판: 종료 통지와 프레임이 함께 준비됐는데 스트림이 %q 를 썼다(끝남 %v)", i+1, rest, ended)
		}
	}
}

// TestHealthExemptionIsGETAndHEADOnly 는 고침 2 의 사소 7 이다: 헬스체크 면제(게이트 앞 통과 · 전송 표지 면제)는 GET · HEAD 의
// /healthz 하나다. 고치기 전에는 경로만 봐 POST · PUT · DELETE /healthz 가 두 관문을 다 건너 Fallback(화면)에 주체 없이 닿았다.
func TestHealthExemptionIsGETAndHEADOnly(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/healthz", true},
		{http.MethodHead, "/healthz", true},
		{http.MethodPost, "/healthz", false},
		{http.MethodDelete, "/healthz", false},
		{http.MethodGet, "/healthz/", false},
		{http.MethodGet, "/api/v1/healthz", false},
	} {
		if got := JudgeHealthProbe(c.method, c.path); got != c.want {
			t.Errorf("JudgeHealthProbe(%s %s) = %v", c.method, c.path, got)
		}
	}
	reached := ""
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = r.Method + " " + r.URL.Path
		w.WriteHeader(299)
	})
	pol, err := auth.ParseTLS("proxy", "", "", "10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		mode   auth.Mode
		tls    auth.TLSPolicy
		remote string
		want   int // 헬스체크가 아닌 메서드가 받아야 할 거절
	}{
		{"proxy · transition", auth.ModeTransition, pol, "10.1.2.3:1", http.StatusForbidden}, // 신뢰 프록시 · X-Forwarded-Proto 없음(평문)
		{"proxy · accounts", auth.ModeAccounts, pol, "10.1.2.3:1", http.StatusForbidden},
		{"전송 정책 없음", auth.ModeTransition, auth.TLSPolicy{}, "127.0.0.1:1", http.StatusUnauthorized}, // 자격증명 없음
	} {
		e := newEnv(t, func(o *Options) { o.Token, o.AuthMode, o.TLS, o.Fallback = sharedToken, c.mode, c.tls, fallback })
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			reached = ""
			if w := e.do(m, HealthPath, nil, withRemote(c.remote), withKey("k:1")); reached != "" || w.Code != c.want {
				t.Fatalf("%s: %s /healthz 가 %d 이고 Fallback 에 %q 로 닿았다 - 헬스체크 면제는 GET · HEAD 만이다", c.name, m, w.Code, reached)
			}
		}
		// 헬스체크 자체는 그대로 표지 없이 · 자격증명 없이 통한다(컨테이너 안 fd doctor 는 루프백 평문으로 친다).
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			if w := e.do(m, HealthPath, nil, withRemote("127.0.0.1:1")); w.Code != http.StatusOK || reached != "" {
				t.Fatalf("%s: 루프백 평문 %s /healthz 가 %d 다: %s", c.name, m, w.Code, w.Body.String())
			}
		}
	}
}
