package auth

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// 전송 정책(FD_TLS) - 토큰 · 쿠키가 평문으로 망을 건너지 않게 한다.
//
//   - files: 서버가 직접 TLS 로 듣는다(FD_TLS_CERT · FD_TLS_KEY).
//   - proxy: TLS 는 앞의 리버스 프록시가 끝낸다. 서버는 FD_TRUSTED_PROXY 의 CIDR(과 루프백)에서 온 연결만 받는다 - 프록시를
//     건너뛰고 곧장 온 평문 요청을 막는 것이 이 모드의 몫이다.
//   - off(빈 값): 평문이다. 루프백에 묶을 때만 맞다.
//
// ★ 루프백 밖 평문 bind 는 accounts 단계에서 기동을 거절하고, transition 단계에서는 경고만 한다. 지금 배포(컨테이너 안
// :7420 평문, 호스트 앞에서 TLS)가 갱신하는 순간 못 뜨면 전환 길 자체가 끊긴다 - 좁히는 날(FD_AUTH=accounts)에 함께 정한다.

// TLSMode 는 FD_TLS 값이다.
type TLSMode string

const (
	TLSOff   TLSMode = "off"
	TLSFiles TLSMode = "files"
	TLSProxy TLSMode = "proxy"
)

// TLSPolicy 는 전송 정책 한 벌이다.
type TLSPolicy struct {
	Mode              TLSMode
	CertFile, KeyFile string
	Trusted           []netip.Prefix // proxy 에서만
}

// ParseTLS 는 FD_TLS · FD_TLS_CERT · FD_TLS_KEY · FD_TRUSTED_PROXY 를 읽는다. 모드와 맞지 않는 값이 함께 오면 오류다 -
// FD_TLS_CERT 만 적고 FD_TLS 를 빠뜨린 서버가 평문으로 뜨면서 "TLS 를 켰다"고 믿게 하지 않는다.
func ParseTLS(mode, cert, key, trusted string) (TLSPolicy, error) {
	p := TLSPolicy{CertFile: strings.TrimSpace(cert), KeyFile: strings.TrimSpace(key)}
	trusted = strings.TrimSpace(trusted)
	switch m := TLSMode(strings.TrimSpace(mode)); m {
	case "", TLSOff:
		p.Mode = TLSOff
		if p.CertFile != "" || p.KeyFile != "" || trusted != "" {
			return TLSPolicy{}, fmt.Errorf("FD_TLS 가 꺼져 있는데 FD_TLS_CERT · FD_TLS_KEY · FD_TRUSTED_PROXY 가 있다 - FD_TLS=files 나 proxy 를 켜라")
		}
	case TLSFiles:
		p.Mode = TLSFiles
		if p.CertFile == "" || p.KeyFile == "" {
			return TLSPolicy{}, fmt.Errorf("FD_TLS=files 는 FD_TLS_CERT 와 FD_TLS_KEY 가 둘 다 있어야 한다")
		}
		if trusted != "" {
			return TLSPolicy{}, fmt.Errorf("FD_TRUSTED_PROXY 는 FD_TLS=proxy 에서만 쓴다")
		}
	case TLSProxy:
		p.Mode = TLSProxy
		if p.CertFile != "" || p.KeyFile != "" {
			return TLSPolicy{}, fmt.Errorf("FD_TLS=proxy 는 인증서를 안 읽는다 - FD_TLS_CERT · FD_TLS_KEY 를 지우거나 FD_TLS=files 를 써라")
		}
		prefixes, err := ParseTrustedProxies(trusted)
		if err != nil {
			return TLSPolicy{}, err
		}
		if len(prefixes) == 0 {
			return TLSPolicy{}, fmt.Errorf("FD_TLS=proxy 는 FD_TRUSTED_PROXY(프록시의 CIDR, 쉼표로 여럿)가 있어야 한다")
		}
		p.Trusted = prefixes
	default:
		return TLSPolicy{}, fmt.Errorf("FD_TLS=%q 를 모른다 - files · proxy 또는 빈 값(평문, 루프백 전용)이다", clip(mode, 40))
	}
	return p, nil
}

// ParseTrustedProxies 는 쉼표 · 공백으로 가른 CIDR 목록을 읽는다. 주소 하나는 그 주소만의 범위(/32 · /128)다.
func ParseTrustedProxies(v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("FD_TRUSTED_PROXY 의 %q 를 CIDR 로 못 읽었다", clip(f, 60))
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// BindVerdict 는 듣는 주소 판정이다.
type BindVerdict struct {
	Refuse bool
	// Warn 은 뜨되 알려야 할 사정이다(transition 단계의 루프백 밖 평문).
	Warn   string
	Reason string
}

// JudgeBind 는 이 주소로 들어도 되는가다. 순수 함수다.
func JudgeBind(addr string, p TLSPolicy, m Mode) BindVerdict {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return BindVerdict{Refuse: true, Reason: fmt.Sprintf("듣는 주소 %q 를 못 읽었다 - host:port 꼴이다", clip(addr, 80))}
	}
	if p.Mode == TLSFiles || p.Mode == TLSProxy {
		return BindVerdict{Reason: "FD_TLS=" + string(p.Mode)}
	}
	if IsLoopbackHost(host) {
		return BindVerdict{Reason: "루프백에만 묶는다(평문)"}
	}
	why := "루프백 밖(" + shownHost(host) + ")에 평문 HTTP 로 듣는다 - 토큰 · 쿠키가 망에 평문으로 실린다. " +
		"FD_TLS=files(FD_TLS_CERT · FD_TLS_KEY)나 FD_TLS=proxy(FD_TRUSTED_PROXY)를 켜거나 --addr 를 127.0.0.1 로 묶어라"
	if m == ModeAccounts {
		return BindVerdict{Refuse: true, Reason: why}
	}
	return BindVerdict{Warn: why + " (FD_AUTH=accounts 에서는 기동을 거절한다)", Reason: "전환 단계라 경고만 한다"}
}

func shownHost(h string) string {
	if h == "" {
		return "모든 인터페이스"
	}
	return h
}

// IsLoopbackHost 는 듣는 주소의 host 가 루프백인가다. 빈 host(모든 인터페이스)는 아니다.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && a.Unmap().IsLoopback()
}

// TransportVerdict 는 연결 하나가 이 전송 정책을 지켰는가다.
type TransportVerdict struct {
	OK     bool
	Reason string
}

// JudgeTransport 는 원격 주소(RemoteAddr)가 이 정책으로 받을 연결인가다. 순수 함수다.
//
// proxy 에서만 거절이 난다: 루프백도 신뢰 프록시도 아닌 곳에서 곧장 온 연결은 TLS 를 건너뛴 평문이다. files 는 듣는 소켓이
// TLS 라 평문이 여기까지 못 오고, off 는 bind 판정이 이미 정했다. 못 읽은 주소는 거절한다(통과시키지 않는다).
func JudgeTransport(remoteAddr string, p TLSPolicy) TransportVerdict {
	if p.Mode != TLSProxy {
		return TransportVerdict{OK: true}
	}
	a, ok := remoteIP(remoteAddr)
	if !ok {
		return TransportVerdict{Reason: "원격 주소를 못 읽었다 - FD_TLS=proxy 는 신뢰 프록시에서 온 연결만 받는다"}
	}
	if a.IsLoopback() {
		return TransportVerdict{OK: true}
	}
	for _, t := range p.Trusted {
		if t.Contains(a) {
			return TransportVerdict{OK: true}
		}
	}
	return TransportVerdict{Reason: "신뢰 프록시(FD_TRUSTED_PROXY) 밖에서 곧장 온 연결이다 - TLS 프록시의 주소로 붙어라"}
}

// remoteIP 는 RemoteAddr(host:port)의 IP 다.
func remoteIP(remoteAddr string) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err == nil {
		return ap.Addr().Unmap(), true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// CookieSecure 는 세션 쿠키에 Secure 를 다는가다. files · proxy 는 언제나 단다(브라우저 앞이 TLS 다). off 는 이 연결이 TLS
// 뒤라고 말할 때만 단다 - 평문 루프백에서 Secure 를 달면 브라우저가 쿠키를 저장조차 안 해 로그인이 원리적으로 안 된다.
func CookieSecure(p TLSPolicy, connTLS bool, forwardedProto string) bool {
	if p.Mode == TLSFiles || p.Mode == TLSProxy || connTLS {
		return true
	}
	// 프록시 사슬은 "https, http" 처럼 쉼표로 온다. 첫째가 클라이언트에 가장 가깝다.
	first, _, _ := strings.Cut(forwardedProto, ",")
	return strings.EqualFold(strings.TrimSpace(first), "https")
}

// ServerTLSConfig 는 files 모드의 듣는 소켓 설정이다. 다른 모드는 nil 이다(평문 소켓). 인증서는 기동 때 한 번 읽는다 - 못 읽으면
// 포트를 열기 전에 기동을 멈춘다. 오류에 인증서 경로는 싣지만 내용은 안 싣는다.
func ServerTLSConfig(p TLSPolicy) (*tls.Config, error) {
	if p.Mode != TLSFiles {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(p.CertFile, p.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("FD_TLS_CERT · FD_TLS_KEY 를 읽지 못했다: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}
