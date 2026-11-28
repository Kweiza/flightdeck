package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingTransport 는 보내려던 요청을 기록만 하고 200 을 낸다 - 망에 무엇이 나갔는지를 잰다.
type recordingTransport struct {
	mu   sync.Mutex
	seen []*http.Request
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req)
	r.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}, Request: req}, nil
}

func TestCredentialTransportRefusal(t *testing.T) {
	for _, c := range []struct {
		url           string
		token, escape bool
		refused       bool
	}{
		{"http://fd.example.internal:7420", false, false, false}, // 실을 비밀이 없다
		{"https://fd.example.internal:7420", true, false, false},
		{"http://127.0.0.1:7420", true, false, false},
		{"http://127.9.9.9:7420", true, false, false},
		{"http://localhost:7420", true, false, false},
		{"http://LOCALHOST.:7420", true, false, false},
		{"http://[::1]:7420", true, false, false},
		{"http://fd.example.internal:7420", true, false, true},
		{"http://10.0.0.5:7420", true, false, true},
		{"http://198.51.100.7:7420", true, false, true},
		{"http://localhost.example.internal:7420", true, false, true}, // 이름은 localhost 하나만 믿는다
		{"http://10.0.0.5:7420", true, true, false},                   // 명시 탈출구
		{"ftp://10.0.0.5", true, true, true},
		{"10.0.0.5:7420", true, false, true},
	} {
		why := CredentialTransportRefusal(c.url, c.token, c.escape)
		if (why != "") != c.refused {
			t.Errorf("%s (토큰 %v · 탈출구 %v): 거절 %v 여야 하는데 %q", c.url, c.token, c.escape, c.refused, why)
		}
	}
	if why := CredentialTransportRefusal("http://10.0.0.5:7420", true, false); !strings.Contains(why, InsecureHTTPEnv) ||
		!strings.Contains(why, "https") {
		t.Errorf("거절 사유가 처방(https · %s)을 안 말한다: %q", InsecureHTTPEnv, why)
	}
}

// TestClientNeverSendsATokenOverRemoteHTTP 는 막음 4 다: 원격 http 주소로는 토큰을 실은 요청이 아예 나가지 않는다(서버가 거절해도
// 이미 늦다). 거절은 미도달이 아니라 아웃박스에 안 쌓인다. 명시 탈출구를 주면 보낸다.
func TestClientNeverSendsATokenOverRemoteHTTP(t *testing.T) {
	h := newHarness(t)
	rt := &recordingTransport{}
	env := map[string]string{}
	for k, v := range h.env {
		env[k] = v
	}
	env["FD_URL"], env["FD_TOKEN"] = "http://fd.example.internal:7420", strings.Repeat("t", 32)
	cli := newClient(ResolveStateDir(envOf(env), ""), envOf(env), h.home, quietLogger())
	cli.HTTP.Transport = rt
	ctx := context.Background()
	_, err := cli.Healthz(ctx)
	var refused *TransportRefusal
	if !errors.As(err, &refused) || Unreachable(err, 0) {
		t.Fatalf("원격 http 의 토큰 요청이 전송 거절이 아니다: %v", err)
	}
	if _, err := cli.Write(ctx, "note", "/api/v1/judgments", map[string]any{"project": "p"}); !errors.As(err, &refused) {
		t.Fatalf("쓰기가 전송 거절이 아니다: %v", err)
	}
	if queued, err := cli.Outbox.List(); err != nil || len(queued) != 0 {
		t.Fatalf("전송 거절한 쓰기가 아웃박스에 %d건 쌓였다(%v) - 설정을 고치기 전에는 같은 거절만 재생된다", len(queued), err)
	}
	if len(rt.seen) != 0 {
		t.Fatalf("거절했는데 요청 %d건이 망에 나갔다", len(rt.seen))
	}

	env[InsecureHTTPEnv] = "1"
	cli = newClient(ResolveStateDir(envOf(env), ""), envOf(env), h.home, quietLogger())
	cli.HTTP.Transport = rt
	if _, err := cli.Healthz(ctx); err != nil && errors.As(err, &refused) {
		t.Fatalf("탈출구를 줬는데 거절했다: %v", err)
	}
	if len(rt.seen) != 1 || rt.seen[0].Header.Get("Authorization") == "" {
		t.Fatalf("탈출구를 줬는데 토큰 요청이 안 나갔다: %d", len(rt.seen))
	}
}

// TestProductionShapedClientsStillReachTheirServer 는 이 변경이 지금 배포를 안 끊는가다: 같은 머신의 서버를 루프백 http 로
// 가리키는 설정(config.json 의 url · 토큰, FD_URL 없음 - 훅 · MCP 가 받는 꼴)이 토큰을 실어 그대로 붙는다.
func TestProductionShapedClientsStillReachTheirServer(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t)
	env := map[string]string{}
	for k, v := range h.env {
		if k != "FD_URL" && k != "FD_TOKEN" {
			env[k] = v
		}
	}
	tok := strings.Repeat("k", 32)
	path, _ := ConfigPath(envOf(env), h.home)
	if err := SaveConfig(path, Config{URL: srv.URL, Token: tok}); err != nil {
		t.Fatal(err)
	}
	cli := newClient(ResolveStateDir(envOf(env), ""), envOf(env), h.home, quietLogger())
	if !strings.HasPrefix(cli.URL, "http://127.0.0.1:") {
		t.Fatalf("전제가 깨졌다 - 주소가 루프백 http 가 아니다: %s", cli.URL)
	}
	if _, err := cli.Healthz(context.Background()); err != nil {
		t.Fatalf("루프백 http 서버에 못 붙었다: %v", err)
	}
	if got != "Bearer "+tok {
		t.Fatalf("루프백 요청에 토큰이 안 실렸다")
	}
}

// TestCrossOriginRedirectIsNotFollowedWithTheToken 은 리다이렉트로 출처(체계 · 호스트 · 포트)가 바뀌면 따라가지 않는가다.
// net/http 는 호스트 이름이 바뀔 때만 Authorization 을 떼므로, 같은 호스트의 다른 포트 · https→http 강등에는 그대로 싣는다.
func TestCrossOriginRedirectIsNotFollowedWithTheToken(t *testing.T) {
	var other []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other = append(other, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(target.Close)
	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			http.Redirect(w, r, target.URL+"/healthz", http.StatusTemporaryRedirect) // 같은 127.0.0.1 · 다른 포트
		case "/same":
			http.Redirect(w, r, origin.URL+"/ok", http.StatusTemporaryRedirect)
		case "/ok":
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(origin.Close)
	h := newHarness(t)
	env := map[string]string{}
	for k, v := range h.env {
		env[k] = v
	}
	env["FD_URL"], env["FD_TOKEN"] = origin.URL, strings.Repeat("r", 32)
	cli := newClient(ResolveStateDir(envOf(env), ""), envOf(env), h.home, quietLogger())
	ctx := context.Background()
	_, err := cli.Healthz(ctx)
	var refused *TransportRefusal
	if !errors.As(err, &refused) || Unreachable(err, 0) {
		t.Fatalf("다른 출처로의 리다이렉트가 전송 거절이 아니다: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("다른 출처(포트)로 리다이렉트를 따라갔다(토큰 %d건)", len(other))
	}
	if _, err := cli.ReadFresh(ctx, "/same"); err != nil {
		t.Fatalf("같은 출처의 리다이렉트를 못 따라갔거나 토큰을 뗐다: %v", err)
	}
}

func TestSameOrigin(t *testing.T) {
	u := func(s string) *url.URL {
		p, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"https://h:443/a", "https://h/b", true},
		{"http://h/a", "http://H:80/b", true},
		{"https://h/a", "http://h/a", false}, // 강등
		{"http://h:7420/a", "http://h:7421/a", false},
		{"http://h/a", "http://g/a", false},
	} {
		if got := SameOrigin(u(c.a), u(c.b)); got != c.same {
			t.Errorf("SameOrigin(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// TestLocalServerURLFollowsTheServerConfig 는 기본 주소가 서버 설정(FD_ADDR · FD_TLS)을 따르는가다(중요 8).
func TestLocalServerURLFollowsTheServerConfig(t *testing.T) {
	for _, c := range []struct {
		env       map[string]string
		want, pin string
	}{
		{map[string]string{}, DefaultURL, ""},
		{map[string]string{"FD_ADDR": ":7420"}, "http://127.0.0.1:7420", ""},
		{map[string]string{"FD_ADDR": ":7999"}, "http://127.0.0.1:7999", ""},
		{map[string]string{"FD_ADDR": "0.0.0.0:7420"}, "http://127.0.0.1:7420", ""},
		{map[string]string{"FD_ADDR": "[::]:7420"}, "http://127.0.0.1:7420", ""},
		{map[string]string{"FD_ADDR": "127.0.0.1:7421"}, "http://127.0.0.1:7421", ""},
		{map[string]string{"FD_ADDR": ":7420", "FD_TLS": "files", "FD_TLS_CERT": "/certs/fd.pem"}, "https://127.0.0.1:7420", "/certs/fd.pem"},
		{map[string]string{"FD_TLS": "files", "FD_TLS_CERT": "/certs/fd.pem"}, "https://127.0.0.1:7420", "/certs/fd.pem"},
		{map[string]string{"FD_ADDR": ":7420", "FD_TLS": "proxy"}, "http://127.0.0.1:7420", ""}, // proxy 뒤의 서버 소켓은 평문이다
		{map[string]string{"FD_ADDR": "no-port"}, DefaultURL, ""},
	} {
		got, _, pin := LocalServerURL(envOf(c.env))
		if got != c.want || pin != c.pin {
			t.Errorf("%v: (%s, %q) 여야 하는데 (%s, %q)", c.env, c.want, c.pin, got, pin)
		}
	}
	// 사람이 적은 주소가 있으면 고정 신뢰를 안 쓴다.
	ep := ResolveEndpoint(envOf(map[string]string{"FD_STATE_DIR": t.TempDir(), "FD_TLS": "files", "FD_TLS_CERT": "/c.pem",
		"FD_URL": "https://fd.example.internal:7420"}), "")
	if ep.PinCert != "" || ep.URL != "https://fd.example.internal:7420" {
		t.Fatalf("FD_URL 이 있는데 고정 신뢰가 남았다: %+v", ep)
	}
}

// TestHealthProbeReachesAFilesModeServer 는 중요 8 의 재현이다: FD_TLS=files 로 띄운 서버와 같은 환경(FD_URL 없음)의 fd doctor 가
// 그 서버를 TLS 로 치고 그 인증서를 믿는다. 고정 http 주소로 치면 TLS 소켓에 평문을 보내 unhealthy 가 된다. 인증서의 이름은
// 루프백이 아니다(배포의 인증서는 망 이름으로 발급된다) - 이름 대신 그 인증서를 신뢰 앵커로 맞댄다.
func TestHealthProbeReachesAFilesModeServer(t *testing.T) {
	mine, mineDER := selfSignedCert(t, "fd.example.internal")
	_, otherDER := selfSignedCert(t, "fd.example.internal")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{mine}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	writePEM := func(name string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	u, _ := url.Parse(srv.URL)
	probe := func(cert string) error {
		env := map[string]string{"FD_STATE_DIR": t.TempDir(), "FD_ADDR": ":" + u.Port(), "FD_TLS": "files", "FD_TLS_CERT": cert}
		cli := newClient(ResolveStateDir(envOf(env), ""), envOf(env), "", quietLogger())
		if !strings.HasPrefix(cli.URL, "https://127.0.0.1:") {
			t.Fatalf("files 모드의 기본 주소가 %s 다", cli.URL)
		}
		_, err := cli.Healthz(context.Background())
		return err
	}
	if err := probe(writePEM("fd.pem", mineDER)); err != nil {
		t.Fatalf("files 모드 서버를 자기 인증서로 못 쳤다: %v", err)
	}
	// 다른 인증서를 믿게 두면 붙지 않는다(이름이 같아도 앵커가 다르다).
	if err := probe(writePEM("other.pem", otherDER)); err == nil {
		t.Fatal("다른 인증서를 믿는 설정으로 붙었다")
	}
}

// selfSignedCert 는 이름 하나로 자체 서명한 서버 인증서다.
func selfSignedCert(t *testing.T, name string) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, der
}

// proxyProbeEnv 는 TestLoopbackRequestsBypassTheEnvironmentProxy 가 자기 바이너리를 다시 띄울 때 아이 쪽에 거는 표지다.
const proxyProbeEnv = "FD_TEST_PROXY_PROBE"

// TestLoopbackRequestsBypassTheEnvironmentProxy 는 고침 2 의 막음 2 다: HTTP_PROXY 가 걸린 환경에서 `localhost.` 로 적은 주소에 토큰을
// 실은 요청이 프록시로 안 가고 이 머신의 서버에 곧장 닿는다. net/http 의 환경 프록시는 `localhost.` 를 루프백으로 못 알아봐 프록시로
// 보내므로, 고치기 전에는 평문 Bearer 가 프록시에 닿았다. 환경 프록시는 프로세스마다 한 번 읽혀 굳으므로(sync.Once) 환경을 건 새
// 프로세스(이 시험 바이너리)에서 잰다. 전제로 원격 주소의 요청은 그 프록시를 타는지(환경이 실제로 걸렸는지)를 함께 잰다.
func TestLoopbackRequestsBypassTheEnvironmentProxy(t *testing.T) {
	if os.Getenv(proxyProbeEnv) == "1" {
		probeThroughEnvironmentProxy(t)
		return
	}
	type seen struct{ host, auth string }
	var mu sync.Mutex
	var viaProxy, direct []seen
	record := func(to *[]seen) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*to = append(*to, seen{r.Host, r.Header.Get("Authorization")})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}
	proxy := httptest.NewServer(record(&viaProxy))
	t.Cleanup(proxy.Close)
	target := httptest.NewServer(record(&direct))
	t.Cleanup(target.Close)
	tu, _ := url.Parse(target.URL)

	env := []string{proxyProbeEnv + "=1", "HTTP_PROXY=" + proxy.URL, "FD_TEST_PROBE_TARGET=http://localhost.:" + tu.Port()}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "REQUEST_METHOD", proxyProbeEnv:
			continue // 아이의 프록시 환경은 이 시험이 정한 것 하나다
		}
		env = append(env, kv)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoopbackRequestsBypassTheEnvironmentProxy$", "-test.count=1")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("아이 프로세스가 실패했다: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range viaProxy {
		if strings.HasPrefix(s.host, "localhost") {
			t.Fatalf("localhost. 로 적은 루프백의 요청이 환경 프록시로 갔다(%s, 토큰 실림 %v) - 루프백은 프록시 없이 곧장 잇는다",
				s.host, s.auth != "")
		}
	}
	if len(viaProxy) != 1 || viaProxy[0].host != "fd.example.invalid:7420" {
		t.Fatalf("전제가 깨졌다 - 원격 주소의 요청이 환경 프록시를 안 탔다(프록시가 받은 것 %v)", viaProxy)
	}
	if len(direct) != 1 || direct[0].auth == "" {
		t.Fatalf("localhost. 의 토큰 요청이 이 머신의 서버에 곧장 닿지 않았다(서버가 받은 것 %d건)", len(direct))
	}
}

// probeThroughEnvironmentProxy 는 아이 쪽이다: 같은 클라이언트 조립(newClient)으로 루프백(`localhost.`) 하나 · 원격(탈출구를 준 평문)
// 하나를 친다. 어디에 닿았는지는 부모가 잰다.
func probeThroughEnvironmentProxy(t *testing.T) {
	hit := func(u string, extra map[string]string) {
		env := map[string]string{"FD_STATE_DIR": t.TempDir(), "FD_URL": u, "FD_TOKEN": strings.Repeat("p", 32)}
		for k, v := range extra {
			env[k] = v
		}
		cli := newClient(ResolveStateDir(envOf(env), ""), envOf(env), t.TempDir(), quietLogger())
		if _, err := cli.Healthz(context.Background()); err != nil {
			t.Fatalf("%s 를 못 쳤다: %v", u, err)
		}
	}
	hit(os.Getenv("FD_TEST_PROBE_TARGET"), nil)
	hit("http://fd.example.invalid:7420", map[string]string{InsecureHTTPEnv: "1"})
}

// TestLoopbackNameDialsOnlyLoopbackIPs 는 막음 2 의 둘째 절반이다: 이름으로 적은 루프백(`localhost` · `localhost.`)은 해석하지 않고
// 루프백 IP 로만 잇는다(127.0.0.1, 안 되면 ::1). IP 로 적은 주소와 다른 이름은 그대로 넘긴다.
func TestLoopbackNameDialsOnlyLoopbackIPs(t *testing.T) {
	for _, c := range []struct {
		addr string
		want []string
	}{
		{"localhost:7420", []string{"127.0.0.1:7420"}},
		{"LOCALHOST.:7420", []string{"127.0.0.1:7420"}},
		{"127.0.0.1:7420", []string{"127.0.0.1:7420"}},
		{"[::1]:7420", []string{"[::1]:7420"}},
		{"fd.example.internal:7420", []string{"fd.example.internal:7420"}},
	} {
		var got []string
		dial := loopbackDial(func(_ context.Context, _, addr string) (net.Conn, error) {
			got = append(got, addr)
			c1, c2 := net.Pipe()
			c2.Close()
			return c1, nil
		})
		if _, err := dial(context.Background(), "tcp", c.addr); err != nil || strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v 로 이어야 하는데 %v (%v)", c.addr, c.want, got, err)
		}
	}
	// 127.0.0.1 이 안 받으면 ::1 로 간다. 그 밖으로는 안 간다.
	var got []string
	dial := loopbackDial(func(_ context.Context, _, addr string) (net.Conn, error) {
		got = append(got, addr)
		return nil, errors.New("거절")
	})
	if _, err := dial(context.Background(), "tcp", "localhost:7420"); err == nil || strings.Join(got, ",") != "127.0.0.1:7420,[::1]:7420" {
		t.Fatalf("루프백 둘이 다 안 받으면 거기서 끝나야 하는데 %v (%v)", got, err)
	}
}

// TestUnreadablePinnedCertRefusesBeforeSending 은 고침 2 의 중요 5 다: FD_TLS=files 의 고정 인증서(FD_TLS_CERT)를 못 읽으면 기본
// 신뢰로 돌아가지 않고 보내기 전에 거절한다 - 고치기 전에는 기본 신뢰로 붙어, 그 주소에서 공인 인증서를 내는 다른 상대에도 토큰을
// 실었다. 거절은 전송 거절이라 미도달이 아니고(아웃박스에 안 쌓인다), 서버는 연결을 하나도 못 받는다.
func TestUnreadablePinnedCertRefusesBeforeSending(t *testing.T) {
	var mu sync.Mutex
	opened := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			mu.Lock()
			opened++
			mu.Unlock()
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	for _, cert := range []string{filepath.Join(t.TempDir(), "missing.pem"), writeFile(t, "not-a-cert.pem", "not pem")} {
		env := map[string]string{"FD_STATE_DIR": t.TempDir(), "FD_ADDR": ":" + u.Port(), "FD_TLS": "files", "FD_TLS_CERT": cert,
			"FD_TOKEN": strings.Repeat("c", 32)}
		cli := newClient(ResolveStateDir(envOf(env), ""), envOf(env), "", quietLogger())
		_, err := cli.Healthz(context.Background())
		var refused *TransportRefusal
		if !errors.As(err, &refused) || Unreachable(err, 0) || !strings.Contains(err.Error(), "FD_TLS_CERT") {
			t.Fatalf("%s: 고정 인증서를 못 읽었는데 전송 거절이 아니다: %v", filepath.Base(cert), err)
		}
		if !strings.Contains(cli.Endpoint.Warn, "FD_TLS_CERT") {
			t.Errorf("%s: fd doctor 가 찍을 사유가 없다: %q", filepath.Base(cert), cli.Endpoint.Warn)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if opened != 0 {
		t.Fatalf("고정 인증서를 못 읽었는데 서버에 연결 %d개가 열렸다 - 보내기 전에 거절해야 한다", opened)
	}
}

// writeFile 은 임시 디렉토리에 글 하나를 쓰고 그 경로를 낸다.
func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestQueuedEntriesSurviveTransportRefusal 는 고침 2 의 중요 3 이다: 올리기 전에 쌓인 판단이, 설정이 전송 거절을 내는 동안(원격 평문
// http + 토큰) 재생을 몇 번 돌아도 격리되지 않고 시도 횟수도 안 먹은 채 큐에 남는다. 고치기 전에는 거절이 「서버에 닿았는데 실패」로
// 세어져 다섯 번째 명령에서 「서버에 닿았는데 5번 연속 실패했다」는 거짓 사유로 격리됐다(격리는 되돌리는 길이 없다).
func TestQueuedEntriesSurviveTransportRefusal(t *testing.T) {
	h := newHarness(t)
	env := map[string]string{}
	for k, v := range h.env {
		env[k] = v
	}
	env["FD_URL"], env["FD_TOKEN"] = "http://fd.example.internal:7420", strings.Repeat("t", 32)
	cli := newClient(ResolveStateDir(envOf(env), ""), envOf(env), h.home, quietLogger())
	rt := &recordingTransport{}
	cli.HTTP.Transport = rt
	body, _ := json.Marshal(map[string]any{"project": "p", "kind": "decision", "title": "t", "body": "b"})
	if err := cli.Outbox.Append(OutboxEntry{Key: "s:1", At: time.Now().UTC(), Path: "/api/v1/judgments", Body: json.RawMessage(body)}); err != nil {
		t.Fatal(err)
	}
	var last ReplayResult
	for i := 0; i < maxReplayTries+1; i++ {
		last = cli.Flush(context.Background())
	}
	pending, err := cli.Outbox.List()
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := cli.Outbox.Rejected()
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 0 {
		t.Fatalf("서버에 닿지도 않은 줄이 격리됐다(%d건): %s", len(rejected), rejected[0].Reason)
	}
	if len(pending) != 1 || pending[0].Tries != 0 {
		t.Fatalf("전송 거절이 큐를 바꿨다(남은 %d건 · 시도 %v) - 거절은 횟수에 안 센다", len(pending), pending)
	}
	if last.Remaining != 1 || !strings.Contains(last.Detail, "전송 거절") {
		t.Fatalf("재생 결과가 거절을 사실대로 안 말한다: %+v", last)
	}
	if len(rt.seen) != 0 {
		t.Fatalf("거절했는데 요청 %d건이 망에 나갔다", len(rt.seen))
	}
}
