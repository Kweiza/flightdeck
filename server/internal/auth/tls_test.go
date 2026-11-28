package auth

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseTLSRefusesMismatchedCombinations(t *testing.T) {
	good := []struct {
		mode, cert, key, trusted string
		want                     TLSMode
	}{
		{"", "", "", "", TLSOff},
		{"off", "", "", "", TLSOff},
		{"files", "c.pem", "k.pem", "", TLSFiles},
		{"proxy", "", "", "10.0.0.0/8, 192.168.1.5", TLSProxy},
	}
	for _, c := range good {
		p, err := ParseTLS(c.mode, c.cert, c.key, c.trusted)
		if err != nil || p.Mode != c.want {
			t.Errorf("ParseTLS(%q,%q,%q,%q) = %+v, %v", c.mode, c.cert, c.key, c.trusted, p, err)
		}
	}
	// "TLS 를 켰다"고 믿는 평문 서버를 안 만든다.
	bad := [][4]string{
		{"", "c.pem", "", ""},      // 인증서만 적고 FD_TLS 를 빠뜨렸다
		{"", "", "", "10.0.0.0/8"}, // 신뢰 프록시만
		{"files", "c.pem", "", ""}, // 키 없음
		{"files", "c.pem", "k.pem", "10.0.0.0/8"},
		{"proxy", "", "", ""}, // 신뢰 프록시 없음
		{"proxy", "c.pem", "", "10.0.0.0/8"},
		{"proxy", "", "", "not-a-cidr"},
		{"on", "", "", ""},
	}
	for _, c := range bad {
		if p, err := ParseTLS(c[0], c[1], c[2], c[3]); err == nil {
			t.Errorf("ParseTLS(%q) 가 받았다: %+v", c, p)
		}
	}
	p, _ := ParseTLS("proxy", "", "", "192.168.1.5")
	if len(p.Trusted) != 1 || p.Trusted[0] != netip.MustParsePrefix("192.168.1.5/32") {
		t.Errorf("주소 하나가 /32 가 아니다: %v", p.Trusted)
	}
}

// TestJudgeBindKeepsTransitionDeploymentUp 은 전환 길이다: 지금 배포(컨테이너 안 :7420 평문)가 transition 에서는 경고만 받고
// 뜬다. accounts 에서만 기동을 거절한다.
func TestJudgeBindKeepsTransitionDeploymentUp(t *testing.T) {
	off := TLSPolicy{Mode: TLSOff}
	for _, addr := range []string{":7420", "0.0.0.0:7420", "10.1.2.3:7420"} {
		if v := JudgeBind(addr, off, ModeTransition); v.Refuse || v.Warn == "" {
			t.Errorf("transition %s: 경고만 해야 한다: %+v", addr, v)
		}
		if v := JudgeBind(addr, off, ModeAccounts); !v.Refuse {
			t.Errorf("accounts %s: 루프백 밖 평문을 거절해야 한다: %+v", addr, v)
		}
	}
	for _, addr := range []string{"127.0.0.1:7420", "localhost:7420", "[::1]:7420"} {
		if v := JudgeBind(addr, off, ModeAccounts); v.Refuse || v.Warn != "" {
			t.Errorf("루프백 %s: %+v", addr, v)
		}
	}
	for _, p := range []TLSPolicy{{Mode: TLSFiles}, {Mode: TLSProxy}} {
		if v := JudgeBind(":7420", p, ModeAccounts); v.Refuse || v.Warn != "" {
			t.Errorf("%s: %+v", p.Mode, v)
		}
	}
	if v := JudgeBind("no-port", off, ModeTransition); !v.Refuse {
		t.Errorf("못 읽는 주소: %+v", v)
	}
}

func TestJudgeTransportOnlyRefusesInProxyMode(t *testing.T) {
	p := TLSPolicy{Mode: TLSProxy, Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	for _, ra := range []string{"10.2.3.4:5555", "127.0.0.1:1", "[::1]:1", "[::ffff:10.0.0.1]:1"} {
		if v := JudgeTransport(ra, p); !v.OK {
			t.Errorf("%s: 받아야 한다: %+v", ra, v)
		}
	}
	for _, ra := range []string{"192.168.0.9:5555", "[2001:db8::1]:1", "garbage"} {
		if v := JudgeTransport(ra, p); v.OK || v.Reason == "" {
			t.Errorf("%s: 거절해야 한다: %+v", ra, v)
		}
	}
	for _, m := range []TLSMode{TLSOff, TLSFiles} {
		if v := JudgeTransport("192.168.0.9:5555", TLSPolicy{Mode: m}); !v.OK {
			t.Errorf("%s 는 전송 판정이 거절하지 않는다: %+v", m, v)
		}
	}
}

func TestCookieSecure(t *testing.T) {
	off := TLSPolicy{Mode: TLSOff}
	cases := []struct {
		p       TLSPolicy
		connTLS bool
		xfp     string
		want    bool
	}{
		{off, false, "", false}, // 평문 루프백 - Secure 면 브라우저가 쿠키를 저장조차 안 한다
		{off, true, "", true},
		{off, false, "https", true},
		{off, false, "HTTPS, http", true},
		{off, false, "http, https", false},
		{TLSPolicy{Mode: TLSFiles}, false, "", true},
		{TLSPolicy{Mode: TLSProxy}, false, "http", true},
	}
	for _, c := range cases {
		if got := CookieSecure(c.p, c.connTLS, c.xfp); got != c.want {
			t.Errorf("CookieSecure(%s, %v, %q) = %v", c.p.Mode, c.connTLS, c.xfp, got)
		}
	}
}

func TestServerTLSConfigReportsPathNotContent(t *testing.T) {
	if c, err := ServerTLSConfig(TLSPolicy{Mode: TLSOff}); c != nil || err != nil {
		t.Fatalf("off: %v %v", c, err)
	}
	_, err := ServerTLSConfig(TLSPolicy{Mode: TLSFiles, CertFile: t.TempDir() + "/none.pem", KeyFile: t.TempDir() + "/none.key"})
	if err == nil || !strings.Contains(err.Error(), "FD_TLS_CERT") {
		t.Fatalf("없는 인증서: %v", err)
	}
}
