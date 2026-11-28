package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

// 자격증명의 전송 - 토큰이 평문으로 망을 건너지 않게 하는 클라이언트 쪽 절반이다. 서버 쪽 절반은 auth.JudgeTransport ·
// auth.JudgeBind 다. 서버가 평문 요청을 거절해도 그 요청의 Authorization 은 이미 망을 건넜으므로, 거절은 **보내기 전에**
// 클라이언트가 한다.

// TransportRefusal 은 클라이언트가 보내기 전에(또는 리다이렉트를 따라가기 전에) 거절한 것이다. **미도달이 아니다** -
// 아웃박스에 쌓지도 캐시로 답하지도 않는다. 설정을 고치기 전에는 몇 번을 다시 해도 같은 거절이라, 쌓으면 같은 거절만 재생된다.
type TransportRefusal struct{ Reason string }

func (e *TransportRefusal) Error() string { return "자격증명 전송 거절: " + e.Reason }

// InsecureHTTPEnv 는 비루프백 평문 http 로도 토큰을 보내게 하는 명시 탈출구다(값 1). 사설망 너머의 서버처럼 망 자체를 믿는
// 사람이 적는다. 기본은 거절이다.
const InsecureHTTPEnv = "FD_INSECURE_HTTP"

// IsLoopbackHost 는 주소의 호스트 칸이 이 머신 자신인가다. 순수 함수다. 이름은 localhost 하나만 믿는다 - 다른 이름은
// 해석해 봐야 알고, 해석 결과는 이 판정이 볼 수 없는 자리(DNS · hosts)가 정한다.
func IsLoopbackHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(h, "[]"))
	return err == nil && a.Unmap().IsLoopback()
}

// CredentialTransportRefusal 은 이 주소로 토큰을 실어 보내면 안 되는 사유다. 빈 글이면 보낸다. 순수 함수다.
//
// 축: 토큰이 있고 · 주소가 https 가 아니고 · 호스트가 루프백이 아니면 거절이다. 토큰이 없으면 실을 비밀이 없다. 루프백 http 는
// 망을 안 건넌다(컨테이너 안의 헬스체크 · 같은 머신의 서버). 탈출구(allowInsecure)는 사유를 바꾸지 않고 판정만 넘긴다.
func CredentialTransportRefusal(rawURL string, tokenSet, allowInsecure bool) string {
	if !tokenSet {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return fmt.Sprintf("서버 주소 %q 를 읽지 못했다 - https://<호스트>:<포트> 꼴로 적어라", clip(rawURL, 120))
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return ""
	case "http":
		if IsLoopbackHost(u.Hostname()) || allowInsecure {
			return ""
		}
		return fmt.Sprintf("토큰을 평문 http 로 원격(%s)에 보내지 않는다 - 서버를 https 로 열고(FD_TLS=files · proxy) 주소를 "+
			"https:// 로 바꿔라. 망 자체를 믿는다면 %s=1 로 이 검사를 끈다", clip(u.Host, 120), InsecureHTTPEnv)
	}
	return fmt.Sprintf("모르는 주소 체계 %q - http(루프백) · https 만 쓴다", clip(u.Scheme, 20))
}

// SameOrigin 은 두 주소가 같은 출처(체계 · 호스트 · 포트)인가다. 순수 함수다. 포트를 빼면 체계의 기본 포트다.
func SameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) &&
		originPort(a) == originPort(b)
}

func originPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// checkRedirect 는 출처가 바뀌는 리다이렉트를 따라가지 않는다. net/http 의 기본은 호스트 이름이 바뀔 때만 Authorization 을
// 떼고 포트 변경 · https→http 강등에는 그대로 싣는다. 이 API 에는 출처를 넘는 리다이렉트를 낼 이유가 없으므로, 헤더를 떼고
// 따라가는 대신(본문은 그대로 넘어간다) 거절한다.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("리다이렉트가 10번을 넘었다")
	}
	if from := via[0].URL; !SameOrigin(from, req.URL) {
		return &TransportRefusal{Reason: fmt.Sprintf("서버가 다른 출처로 리다이렉트했다(%s://%s → %s://%s) - 자격증명을 싣고 "+
			"따라가지 않는다. 서버 주소를 리다이렉트 대상으로 고쳐라",
			from.Scheme, clip(from.Host, 120), req.URL.Scheme, clip(req.URL.Host, 120))}
	}
	return nil
}

// LocalServerURL 은 이 머신에서 도는 서버를 가리키는 기본 주소다. 순수 함수다(env 를 받는다).
//
// ★ 주소는 서버 설정(FD_ADDR · FD_TLS)에서 고른다. 서버와 같은 환경을 받는 곳(컨테이너의 헬스체크 fd doctor)이 서버가 실제로
// 듣는 체계 · 포트를 치게 한다 - 따로 적어 둔 고정 주소(이미지의 FD_URL)는 FD_TLS=files 로 띄운 서버를 평문으로 쳐서
// unhealthy 를 낸다. FD_ADDR 이 모든 주소(빈 호스트 · 0.0.0.0 · ::)면 루프백을 친다.
//
// FD_TLS=files 면 pinCert 에 서버 인증서 경로를 낸다. 루프백 주소는 인증서 이름과 안 맞으므로 이름 대신 그 인증서 자체를
// 믿는다(pinnedTLS).
func LocalServerURL(get func(string) (string, bool)) (u, source, pinCert string) {
	addr := envGet(get, "FD_ADDR")
	scheme := "http"
	if strings.EqualFold(envGet(get, "FD_TLS"), "files") {
		scheme, pinCert = "https", envGet(get, "FD_TLS_CERT")
	}
	if addr == "" && scheme == "http" {
		return DefaultURL, "기본값 " + DefaultURL, ""
	}
	host, port, err := net.SplitHostPort(firstNonEmpty(addr, ":7420"))
	if err != nil {
		return DefaultURL, "기본값 " + DefaultURL + " (FD_ADDR " + clip(addr, 60) + " 을 읽지 못했다)", ""
	}
	if a, perr := netip.ParseAddr(host); host == "" || (perr == nil && a.IsUnspecified()) {
		host = "127.0.0.1"
	}
	u = scheme + "://" + net.JoinHostPort(host, port)
	return u, "이 머신의 서버 설정(FD_ADDR · FD_TLS)에서 고른 " + u, pinCert
}

func envGet(get func(string) (string, bool), k string) string {
	v, _ := get(k)
	return strings.TrimSpace(v)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// pinnedTLS 는 인증서 파일의 첫 인증서를 신뢰 앵커로 삼고 그 인증서의 이름으로 검증하는 설정이다. 루프백으로 치는 자기 서버의
// 인증서는 주소(127.0.0.1)와 이름이 안 맞고 자체 서명일 수도 있다 - 그래서 신뢰 앵커와 맞댈 이름만 그 인증서에서 고르고,
// 체인 · 이름 · 기한 · 용도 검증은 그대로 돈다. 다른 인증서를 내는 상대는 거절된다.
func pinnedTLS(certFile string) (*tls.Config, error) {
	raw, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("FD_TLS_CERT 를 읽지 못했다: %w", err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("FD_TLS_CERT 에 PEM 인증서가 없다")
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("FD_TLS_CERT 의 인증서를 해석하지 못했다: %w", err)
	}
	name := ""
	switch {
	case len(leaf.DNSNames) > 0:
		name = leaf.DNSNames[0]
	case len(leaf.IPAddresses) > 0:
		name = leaf.IPAddresses[0].String()
	default:
		return nil, errors.New("FD_TLS_CERT 의 인증서에 이름(SAN)이 없다 - 이름 없는 인증서는 TLS 검증을 못 지난다")
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: name}, nil
}

// newHTTPClient 는 클라이언트의 HTTP 한 벌이다. 리다이렉트 판정은 늘 걸고, pinCert 가 있으면 그 인증서만 믿는다. 인증서를 못
// 읽으면 고정 없이 만들고 사유를 낸다 - 그때 TLS 검증은 기본 신뢰로 돌아 실패가 미도달로 보이므로 fd doctor 가 사유를 찍는다.
func newHTTPClient(timeout time.Duration, pinCert string) (*http.Client, string) {
	c := &http.Client{Timeout: timeout, CheckRedirect: checkRedirect}
	if pinCert == "" {
		return c, ""
	}
	cfg, err := pinnedTLS(pinCert)
	if err != nil {
		return c, err.Error()
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg
	c.Transport = tr
	return c, ""
}
