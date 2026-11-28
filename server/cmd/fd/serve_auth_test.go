package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/auth"
)

// fd doctor 는 인증 단계와 전송 정책을 함께 찍는다. 「토큰 설정됨」만으로는 그 토큰이 어디서나 통하는지(전환 단계) 부트스트랩에만
// 통하는지(계정 단계) 모른다.
func TestRenderHealthShowsAuthModeAndTransport(t *testing.T) {
	var h healthzResponse
	h.OK, h.APIVersion, h.DBOK = true, "1", true
	h.Auth.TokenSet = true
	h.AuthMode, h.TLS = "accounts", "proxy"
	got := RenderHealth(h, true, "http://x:7420")
	for _, want := range []string{"단계 accounts", "전송 proxy", "토큰 설정 true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q 가 화면에 없다:\n%s", want, got)
		}
	}
	// 이 축을 모르는 옛 서버는 두 칸을 안 낸다 - 빈 글을 「전환 단계」로 읽으면 안 된다.
	var old healthzResponse
	old.OK, old.APIVersion, old.DBOK = true, "1", true
	if got := RenderHealth(old, true, "http://x:7420"); !strings.Contains(got, "단계 ? · 전송 ?") {
		t.Fatalf("옛 서버의 빈 칸을 「?」로 안 보인다:\n%s", got)
	}
}

// 사유 줄은 라벨과 **함께** 나와야 한다. 요약만 내면 "왜"가 사라진다.
func TestRenderHealthKeepsTheReasonLineBesideTheLabel(t *testing.T) {
	var h healthzResponse
	h.OK, h.APIVersion, h.DBOK = true, "1", true
	h.Auth.TokenSet = true
	h.Auth.Notice = "전환 단계다 — 기기마다 fd login 을 마치면 좁혀라"

	got := RenderHealth(h, true, "http://x:7420")
	if !strings.Contains(got, "fd login") {
		t.Fatalf("사유 줄이 화면에서 잘렸다:\n%s", got)
	}
}

// 클라이언트 미러가 새 축을 **읽어야** 화면이 그 값을 낼 수 있다 - 미러 타입은 조용히 낡는다.
func TestHealthzResponseReadsAuthModeAndTLS(t *testing.T) {
	raw := `{"ok":true,"api_version":"1","db_ok":true,"auth_mode":"accounts","tls":"files",
	         "auth":{"token_set":true,"notice":"x"}}`
	var h healthzResponse
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		t.Fatalf("역직렬화 실패: %v", err)
	}
	if h.AuthMode != "accounts" || h.TLS != "files" || !h.Auth.TokenSet {
		t.Fatalf("서버가 보낸 칸을 클라이언트가 버린다: %+v", h)
	}
}

// 인증 설정이 **api 표면까지 실제로 가는가.** 조립이 serve 본문 안에만 있으면 축 하나가 빠져도 아무 시험이 안 잡는다.
func TestServeAPIOptionsCarriesAuthConfig(t *testing.T) {
	tp := auth.TLSPolicy{Mode: auth.TLSProxy}
	opt := serveAPIOptions("tok", 60, quietLogger(), authConfig{Mode: auth.ModeAccounts, TLS: tp}, nil, nil, nil)
	if opt.AuthMode != auth.ModeAccounts || opt.TLS.Mode != auth.TLSProxy {
		t.Fatalf("인증 단계 · 전송 정책이 api 옵션까지 안 간다: mode=%q tls=%q", opt.AuthMode, opt.TLS.Mode)
	}
	if opt.Token != "tok" || opt.RatePerMinute != 60 {
		t.Fatalf("기존 축이 조립에서 빠졌다: token=%q rate=%d", opt.Token, opt.RatePerMinute)
	}
}

// TestReadAuthConfigDefaultsToTransition 은 이 판의 전환 길을 잠근다: FD_AUTH 를 안 적은 지금 배포는 전환 단계로 뜬다. 기본값이
// accounts 면 갈아 끼우는 순간 FD_TOKEN 하나로 붙던 기기 · 훅 · 보드가 전부 401 이다(그 왕복은 harness 의 FD_TOKEN 시험들이 본다).
func TestReadAuthConfigDefaultsToTransition(t *testing.T) {
	ac, err := readAuthConfig(envMap(nil))
	if err != nil {
		t.Fatalf("빈 환경이 거절됐다: %v", err)
	}
	if ac.Mode != auth.ModeTransition || ac.TLS.Mode != auth.TLSOff {
		t.Fatalf("기본값이 전환 단계 · 평문이 아니다: %+v", ac)
	}
	ac, err = readAuthConfig(envMap(map[string]string{"FD_AUTH": "accounts", "FD_TLS": "proxy", "FD_TRUSTED_PROXY": "172.16.0.0/12"}))
	if err != nil || ac.Mode != auth.ModeAccounts || ac.TLS.Mode != auth.TLSProxy || len(ac.TLS.Trusted) != 1 {
		t.Fatalf("좁힌 설정을 못 읽었다: %+v %v", ac, err)
	}
	// 오타는 기본값으로 접지 않는다 - 좁혔다고 믿는 서버가 FD_TOKEN 을 어디서나 받는다.
	for _, bad := range []map[string]string{{"FD_AUTH": "acounts"}, {"FD_TLS": "on"}, {"FD_TLS_CERT": "/x.pem"}} {
		if _, err := readAuthConfig(envMap(bad)); err == nil {
			t.Fatalf("%v 가 통과했다", bad)
		}
	}
}

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}
