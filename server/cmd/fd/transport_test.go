package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
