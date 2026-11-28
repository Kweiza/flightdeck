package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/auth"
	"github.com/kweiza/flightdeck/internal/store"
)

// 계정 단계의 HTTP 표면 시험 - 전환 길 · 부트스트랩 · 로그인 코드 · 철회 · 끄기 · 스트림 끊기 · 전송 정책 · 비밀 가리기.

const sharedToken = "s3cret-shared-token"

func withCookie(name, value string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: value}) }
}

// bootstrapOn 은 FD_TOKEN 으로 첫 관리자를 만든다.
func (e *env) bootstrapOn(login string, opts ...reqOpt) BootstrapGrant {
	e.t.Helper()
	w := e.do(http.MethodPost, BootstrapPath, map[string]any{"login": login}, append([]reqOpt{bearer(sharedToken)}, opts...)...)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("부트스트랩 %d %s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		e.t.Fatalf("비밀 응답의 Cache-Control 이 %q 다", cc)
	}
	var g BootstrapGrant
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil {
		e.t.Fatal(err)
	}
	return g
}

// exchange 는 로그인 코드를 화면 세션 쿠키로 바꾼다.
func (e *env) exchange(code string) string {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, loginPost(e.t, "/login", url.Values{"token": {code}, "next": {"/"}}))
	if rec.Code != http.StatusSeeOther {
		e.t.Fatalf("로그인 코드 교환 %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != int(auth.SessionTTL/time.Second) {
				e.t.Fatalf("세션 쿠키 속성 %+v", c)
			}
			return c.Value
		}
	}
	e.t.Fatalf("세션 쿠키를 안 구웠다: %+v", rec.Result().Cookies())
	return ""
}

// device 는 계정에 딸린 기기 하나를 원장에 넣고 그 기기 토큰을 낸다(기기 코드 흐름은 다음 단계 - 여기서는 행을 직접 단다).
func (e *env) device(machineID, accountID string) string {
	e.t.Helper()
	tok, err := auth.NewToken(auth.DeviceToken)
	if err != nil {
		e.t.Fatal(err)
	}
	at := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	if _, err := e.st.DB().Exec(`INSERT INTO machine(id, hostname, first_seen, last_seen, account_id, name, token_hash, token_prefix)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, machineID, machineID, at, at, accountID, machineID, auth.Hash(tok), auth.Prefix(tok)); err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *env) member(id string) {
	e.t.Helper()
	if _, err := e.st.DB().Exec(`INSERT INTO account(id, login, display_name, role, created_at) VALUES (?, ?, ?, 'member', ?)`,
		id, id, id, time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")); err != nil {
		e.t.Fatal(err)
	}
}

// dash 는 인증만 재는 읽기 하나다(프로젝트 없이도 200).
const dash = "/api/v1/projects"

// TestTransitionDefaultKeepsTheSharedTokenDeployment 는 이 판의 전환 길이다. 기본값(FD_AUTH 미설정)에서 지금 배포 - FD_TOKEN
// 하나로 붙는 기기 · 훅 · MCP(헤더)와 보드 화면(옛 fd_token 쿠키) - 가 하나도 401 로 떨어지지 않는다.
func TestTransitionDefaultKeepsTheSharedTokenDeployment(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Token = sharedToken })
	if w := e.do(http.MethodGet, dash, nil, bearer(sharedToken)); w.Code != http.StatusOK {
		t.Fatalf("기본 단계에서 FD_TOKEN 헤더가 %d 다: %s", w.Code, w.Body.String())
	}
	w := e.write(http.MethodPost, "/api/v1/sessions", map[string]any{
		"project": testProject, "project_path": e.repo, "machine_id": "m1", "hostname": "host1",
		"worktree": e.repo, "cc_session_id": "cc-transition",
	}, bearer(sharedToken))
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("기본 단계에서 FD_TOKEN 쓰기(세션 열기)가 %d 다: %s", w.Code, w.Body.String())
	}
	if w := e.do(http.MethodGet, "/", nil, withCookie(LoginCookieName, sharedToken)); w.Code == http.StatusUnauthorized {
		t.Fatalf("기본 단계에서 옛 화면 쿠키가 401 이다: %s", w.Body.String())
	}
	if w := e.do(http.MethodGet, "/healthz", nil); !strings.Contains(w.Body.String(), `"auth_mode":"transition"`) {
		t.Fatalf("/healthz 가 단계를 안 말한다: %s", w.Body.String())
	}
	// 토큰 없는 서버는 옛날 그대로 무인증으로 통한다.
	open := newEnv(t, nil)
	if w := open.do(http.MethodGet, dash, nil); w.Code != http.StatusOK {
		t.Fatalf("토큰 없는 기본 서버가 %d 다", w.Code)
	}
}

// TestAccountsModeConfinesSharedTokenToBootstrap 은 좁힌 단계다: FD_TOKEN 은 관리자 0명일 때 부트스트랩 한 곳에서만 통하고,
// 그 뒤로는 어디서도 401 이다.
func TestAccountsModeConfinesSharedTokenToBootstrap(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Token = sharedToken; o.AuthMode = auth.ModeAccounts })
	for _, p := range []string{dash, "/api/v1/items?project=" + testProject + "&state=closed"} {
		if w := e.do(http.MethodGet, p, nil, bearer(sharedToken)); w.Code != http.StatusUnauthorized {
			t.Fatalf("accounts 단계에서 FD_TOKEN 이 %s 에 %d 다", p, w.Code)
		}
	}
	if w := e.do(http.MethodGet, "/", nil, withCookie(LoginCookieName, sharedToken)); w.Code != http.StatusUnauthorized {
		t.Fatalf("accounts 단계에서 옛 쿠키가 %d 다", w.Code)
	}
	g := e.bootstrapOn("admin")
	if k, ok := auth.KindOf(g.LoginCode); !ok || k != auth.LoginCode {
		t.Fatalf("로그인 코드 모양 %q", g.LoginCode)
	}
	w := e.do(http.MethodPost, BootstrapPath, map[string]any{"login": "again"}, bearer(sharedToken))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "fd admin recover") {
		t.Fatalf("관리자가 생긴 뒤 부트스트랩이 %d 다: %s", w.Code, w.Body.String())
	}
	if w := e.do(http.MethodGet, dash, nil, bearer(sharedToken)); w.Code != http.StatusUnauthorized {
		t.Fatalf("부트스트랩 뒤 FD_TOKEN 이 %d 다", w.Code)
	}
	// 로그인 코드로 바꾼 화면 세션은 화면에서만 통한다.
	cookie := e.exchange(g.LoginCode)
	if w := e.do(http.MethodGet, "/", nil, withCookie(SessionCookieName, cookie)); w.Code == http.StatusUnauthorized {
		t.Fatalf("화면 세션이 화면에서 401 이다: %s", w.Body.String())
	}
	if w := e.do(http.MethodGet, dash, nil, withCookie(SessionCookieName, cookie)); w.Code != http.StatusUnauthorized {
		t.Fatalf("화면 세션 쿠키가 REST 에서 %d 다", w.Code)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, loginPost(t, "/login", url.Values{"token": {g.LoginCode}}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("쓴 로그인 코드가 또 통했다: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, loginPost(t, "/login", url.Values{"token": {sharedToken}}))
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("accounts 단계에서 FD_TOKEN 으로 화면 로그인이 됐다: %d %+v", rec.Code, rec.Result().Cookies())
	}
}

// TestBootstrapInTransitionIsOnceAndNotMemoized 는 전환 단계의 부트스트랩이다 - 원장이 관리자 수를 다시 잰다(409). 응답이
// 비밀이라 멱등 표에 안 들어간다.
func TestBootstrapInTransitionIsOnceAndNotMemoized(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Token = sharedToken })
	w := e.do(http.MethodPost, BootstrapPath, map[string]any{"login": "admin"}, bearer(sharedToken), withKey("boot:1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("부트스트랩 %d %s", w.Code, w.Body.String())
	}
	var n int
	if err := e.st.DB().QueryRow(`SELECT count(*) FROM idempotency WHERE key = ?`, "boot:1").Scan(&n); err != nil || n != 0 {
		t.Fatalf("부트스트랩 응답이 멱등 표에 들어갔다(%d, %v)", n, err)
	}
	w = e.do(http.MethodPost, BootstrapPath, map[string]any{"login": "admin"}, bearer(sharedToken), withKey("boot:1"))
	if w.Code != http.StatusConflict || errorOf(t, w)["code"] != store.RefuseBootstrapClosed {
		t.Fatalf("두 번째 부트스트랩이 %d 다(같은 키로 첫 응답을 재생하면 비밀이 두 번 나간다): %s", w.Code, w.Body.String())
	}
	if w := e.do(http.MethodPost, BootstrapPath, map[string]any{"login": "x"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("무자격 부트스트랩이 %d 다", w.Code)
	}
}

// TestRevokeAndDisableTakeEffectOnTheNextRequest 는 게이트가 캐시 없이 원장을 읽는가다.
func TestRevokeAndDisableTakeEffectOnTheNextRequest(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Token = sharedToken; o.AuthMode = auth.ModeAccounts })
	g := e.bootstrapOn("admin")
	adminTok := e.device("m-admin", g.AccountID)
	e.member("kim")
	kimTok := e.device("m-kim", "kim")
	kimTok2 := e.device("m-kim2", "kim")

	for _, tok := range []string{adminTok, kimTok, kimTok2} {
		if w := e.do(http.MethodGet, dash, nil, bearer(tok)); w.Code != http.StatusOK {
			t.Fatalf("기기 토큰이 %d 다: %s", w.Code, w.Body.String())
		}
	}
	// 일반 계정은 남의 기기를 철회 못 하고, 계정 끄기를 못 한다.
	for _, id := range []string{"m-admin", "m-ghost"} {
		if w := e.write(http.MethodPost, "/api/v1/devices/"+id+"/revoke", map[string]any{}, bearer(kimTok)); w.Code != http.StatusForbidden {
			t.Fatalf("남의(또는 없는) 기기 %s 철회가 %d 다", id, w.Code)
		}
	}
	if w := e.write(http.MethodPost, "/api/v1/devices/m-ghost/revoke", map[string]any{}, bearer(adminTok)); w.Code != http.StatusNotFound {
		t.Fatalf("관리자의 없는 기기 철회가 %d 다", w.Code)
	}
	if w := e.write(http.MethodPatch, "/api/v1/admin/accounts/"+g.AccountID, map[string]any{"disabled": true}, bearer(kimTok)); w.Code != http.StatusForbidden {
		t.Fatalf("일반 계정의 계정 끄기가 %d 다", w.Code)
	}
	// 자기 기기는 자기가 철회한다 - 다음 요청부터 401.
	w := e.write(http.MethodPost, "/api/v1/devices/m-kim/revoke", map[string]any{}, bearer(kimTok))
	if w.Code != http.StatusOK || decodeBody(t, w)["changed"] != true {
		t.Fatalf("자기 기기 철회 %d %s", w.Code, w.Body.String())
	}
	if w := e.do(http.MethodGet, dash, nil, bearer(kimTok)); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "철회된") {
		t.Fatalf("철회한 기기 토큰이 %d 다: %s", w.Code, w.Body.String())
	}
	// 관리자가 계정을 끄면 그 계정의 다른 기기도 다음 요청부터 401.
	w = e.write(http.MethodPatch, "/api/v1/admin/accounts/kim", map[string]any{"disabled": true}, bearer(adminTok))
	if w.Code != http.StatusOK {
		t.Fatalf("계정 끄기 %d %s", w.Code, w.Body.String())
	}
	if w := e.do(http.MethodGet, dash, nil, bearer(kimTok2)); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "꺼져 있다") {
		t.Fatalf("꺼진 계정의 기기가 %d 다: %s", w.Code, w.Body.String())
	}
	if w := e.write(http.MethodPatch, "/api/v1/admin/accounts/"+g.AccountID, map[string]any{"disabled": true}, bearer(adminTok)); w.Code != http.StatusConflict || errorOf(t, w)["code"] != store.RefuseLastAdmin {
		t.Fatalf("마지막 관리자 끄기가 %d 다: %s", w.Code, w.Body.String())
	}
	if w := e.write(http.MethodPatch, "/api/v1/admin/accounts/kim", map[string]any{"disabled": false}, bearer(adminTok)); w.Code != http.StatusOK {
		t.Fatalf("계정 켜기 %d", w.Code)
	}
	if w := e.do(http.MethodGet, dash, nil, bearer(kimTok2)); w.Code != http.StatusOK {
		t.Fatalf("다시 켠 계정의 기기가 %d 다", w.Code)
	}
}

// stream 은 열린 SSE 하나다. 끝날 때까지 읽은 글을 done 으로 낸다.
type stream struct {
	done chan string
}

func openStream(t *testing.T, ts *httptest.Server, path string, opts ...reqOpt) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("구독이 %d 다", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	if first := readFrame(t, br); !strings.HasPrefix(first, ": connected") {
		t.Fatalf("구독 성립 줄이 %q 다", first)
	}
	s := &stream{done: make(chan string, 1)}
	go func() {
		rest, _ := io.ReadAll(br)
		resp.Body.Close()
		s.done <- string(rest)
	}()
	return s
}

// cutWithin 은 스트림이 d 안에 bye 를 받고 끝났는가다.
func (s *stream) cutWithin(t *testing.T, what string, d time.Duration) {
	t.Helper()
	select {
	case rest := <-s.done:
		if !strings.Contains(rest, ": bye 자격증명이 철회됐다") {
			t.Fatalf("%s: 스트림이 끝났지만 철회 사유가 없다: %q", what, rest)
		}
	case <-time.After(d):
		t.Fatalf("%s: %v 안에 열린 스트림이 안 끊겼다", what, d)
	}
}

func (s *stream) stillOpen(t *testing.T, what string) {
	t.Helper()
	select {
	case rest := <-s.done:
		t.Fatalf("%s: 남의 스트림이 끊겼다: %q", what, rest)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestRevokeDisableLogoutCutOpenStreams 는 열린 /events 가 철회 · 끄기 · 로그아웃의 같은 자리에서 1초 안에 끊기는가다. 열린
// 스트림에는 다음 요청이 없어 게이트가 다시 못 본다.
func TestRevokeDisableLogoutCutOpenStreams(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Token = sharedToken; o.AuthMode = auth.ModeAccounts; o.Heartbeat = time.Hour })
	ts := httptest.NewServer(e.h)
	t.Cleanup(ts.Close)

	g := e.bootstrapOn("admin")
	adminTok := e.device("m-admin", g.AccountID)
	e.member("kim")
	kimTok := e.device("m-kim", "kim")
	kimTok2 := e.device("m-kim2", "kim")
	cookie := e.exchange(g.LoginCode)

	events := "/api/v1/events?project=" + testProject
	admin := openStream(t, ts, events, bearer(adminTok))
	kim := openStream(t, ts, events, bearer(kimTok))
	kim2 := openStream(t, ts, events, bearer(kimTok2))
	screen := openStream(t, ts, "/events?project="+testProject, withCookie(SessionCookieName, cookie))
	waitFor(t, "구독자 넷", 2*time.Second, func() bool {
		return strings.Contains(e.do(http.MethodGet, "/metrics", nil, bearer(adminTok)).Body.String(), "\nflightdeck_sse_subscribers 4\n")
	})

	w := e.write(http.MethodPost, "/api/v1/devices/m-kim/revoke", map[string]any{}, bearer(adminTok))
	if w.Code != http.StatusOK || decodeBody(t, w)["streams_cut"] != float64(1) {
		t.Fatalf("철회 %d %s", w.Code, w.Body.String())
	}
	kim.cutWithin(t, "기기 철회", time.Second)
	kim2.stillOpen(t, "기기 철회")

	if w := e.write(http.MethodPatch, "/api/v1/admin/accounts/kim", map[string]any{"disabled": true}, bearer(adminTok)); w.Code != http.StatusOK {
		t.Fatalf("끄기 %d %s", w.Code, w.Body.String())
	}
	kim2.cutWithin(t, "계정 끄기", time.Second)
	admin.stillOpen(t, "계정 끄기")

	req := loginPost(t, "/logout", url.Values{})
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("로그아웃 %d %s", rec.Code, rec.Body.String())
	}
	screen.cutWithin(t, "로그아웃", time.Second)
	admin.stillOpen(t, "로그아웃")
	if w := e.do(http.MethodGet, "/", nil, withCookie(SessionCookieName, cookie)); w.Code != http.StatusUnauthorized {
		t.Fatalf("로그아웃한 세션 쿠키가 %d 다", w.Code)
	}
}

// TestProxyTransportRefusesDirectConnections 는 FD_TLS=proxy 가 신뢰 프록시 밖에서 곧장 온 연결을 게이트 앞에서 거절하는가다.
func TestProxyTransportRefusesDirectConnections(t *testing.T) {
	pol := auth.TLSPolicy{Mode: auth.TLSProxy, Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	e := newEnv(t, func(o *Options) { o.Token = sharedToken; o.TLS = pol })
	w := e.do(http.MethodGet, dash, nil, bearer(sharedToken), withRemote("192.0.2.7:1234"))
	if w.Code != http.StatusForbidden || errorOf(t, w)["code"] != "transport_refused" {
		t.Fatalf("프록시를 건너뛴 연결이 %d 다: %s", w.Code, w.Body.String())
	}
	for _, ra := range []string{"10.9.8.7:1234", "127.0.0.1:1"} {
		if w := e.do(http.MethodGet, dash, nil, bearer(sharedToken), withRemote(ra)); w.Code != http.StatusOK {
			t.Fatalf("%s 가 %d 다", ra, w.Code)
		}
	}
	if w := e.do(http.MethodGet, "/healthz", nil, withRemote("10.9.8.7:1")); !strings.Contains(w.Body.String(), `"tls":"proxy"`) {
		t.Fatalf("/healthz 가 전송 정책을 안 말한다: %s", w.Body.String())
	}
	// proxy 뒤의 세션 쿠키는 언제나 Secure 다.
	g := e.bootstrapOn("admin", withRemote("10.9.8.7:1"))
	rec := httptest.NewRecorder()
	req := loginPost(t, "/login", url.Values{"token": {g.LoginCode}})
	req.RemoteAddr = "10.9.8.7:1"
	e.h.ServeHTTP(rec, req)
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			found = true
			if !c.Secure {
				t.Fatalf("proxy 뒤 세션 쿠키에 Secure 가 없다: %+v", c)
			}
		}
	}
	if !found {
		t.Fatalf("세션 쿠키를 안 구웠다(%d): %+v", rec.Code, rec.Result().Cookies())
	}
}

// TestSecretsNeverReachLogsErrorsOrMetrics 는 토큰 · 쿠키 · 코드 원값이 로그 · 오류 본문 · /metrics 어디에도 안 실리는가다.
func TestSecretsNeverReachLogsErrorsOrMetrics(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Token = sharedToken; o.AuthMode = auth.ModeAccounts })
	g := e.bootstrapOn("admin")
	tok := e.device("m-admin", g.AccountID)
	cookie := e.exchange(g.LoginCode)
	bad, _ := auth.NewToken(auth.DeviceToken)
	badCode, _ := auth.NewToken(auth.LoginCode)

	var bodies []string
	for _, w := range []*httptest.ResponseRecorder{
		e.do(http.MethodGet, dash, nil, bearer(tok)),
		e.do(http.MethodGet, dash, nil, bearer(bad)),
		e.do(http.MethodGet, dash, nil, bearer(sharedToken)),
		e.do(http.MethodGet, dash, nil, bearer(cookie)),
		e.do(http.MethodGet, dash, nil, withCookie(SessionCookieName, cookie)),
		e.do(http.MethodGet, "/", nil, withCookie(SessionCookieName, bad)),
		e.do(http.MethodGet, "/", nil, withCookie(SessionCookieName, cookie)),
		e.do(http.MethodPost, BootstrapPath, map[string]any{"login": "x"}, bearer(sharedToken)),
		e.do(http.MethodGet, "/metrics", nil),
		e.do(http.MethodGet, "/healthz", nil),
	} {
		bodies = append(bodies, w.Body.String())
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, loginPost(t, "/login", url.Values{"token": {badCode}}))
	bodies = append(bodies, rec.Body.String())

	haystacks := map[string]string{"로그": e.logs.String()}
	for i, b := range bodies {
		haystacks["응답 "+itoa(i)] = b
	}
	for where, h := range haystacks {
		for name, secret := range map[string]string{
			"FD_TOKEN": sharedToken, "기기 토큰": tok, "모르는 기기 토큰": bad, "세션 쿠키": cookie,
			"로그인 코드": g.LoginCode, "모르는 로그인 코드": badCode,
		} {
			if strings.Contains(h, secret) {
				t.Errorf("%s 에 %s 원값이 실렸다", where, name)
			}
		}
	}
}
