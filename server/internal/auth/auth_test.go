package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeDir 는 원장 대역이다. 지문 → 행.
type fakeDir struct {
	devices  map[string]DeviceFact
	sessions map[string]SessionFact
	admins   int
	err      error // 셋 모두 이 오류를 낸다(원장 장애)
	asked    int   // 원장을 몇 번 물었나 - "모양에서 거른다"를 잰다
}

func (d *fakeDir) DeviceCredential(_ context.Context, h string) (DeviceFact, error) {
	d.asked++
	if d.err != nil {
		return DeviceFact{}, d.err
	}
	f, ok := d.devices[h]
	if !ok {
		return DeviceFact{}, ErrUnknown
	}
	return f, nil
}

func (d *fakeDir) WebSessionCredential(_ context.Context, h string) (SessionFact, error) {
	d.asked++
	if d.err != nil {
		return SessionFact{}, d.err
	}
	f, ok := d.sessions[h]
	if !ok {
		return SessionFact{}, ErrUnknown
	}
	return f, nil
}

func (d *fakeDir) AdminCount(context.Context) (int, error) {
	d.asked++
	return d.admins, d.err
}

func mustToken(t *testing.T, k TokenKind) string {
	t.Helper()
	v, err := NewToken(k)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func TestParseModeDefaultsToTransitionAndRefusesTypos(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeTransition, "  ": ModeTransition, "transition": ModeTransition, "accounts": ModeAccounts} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v - %q 여야 한다", in, got, err, want)
		}
	}
	// 오타를 기본값으로 접으면 좁혔다고 믿는 서버가 FD_TOKEN 을 어디서나 받는다.
	for _, in := range []string{"account", "Accounts", "strict", "off"} {
		if _, err := ParseMode(in); err == nil {
			t.Errorf("ParseMode(%q) 가 오류 없이 받았다", in)
		}
	}
}

func TestNewTokenShapeAndKindOf(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range []TokenKind{DeviceToken, SessionToken, LoginCode} {
		for i := 0; i < 50; i++ {
			v := mustToken(t, k)
			if len(v) != len(k)+tokenBody {
				t.Fatalf("%s 길이 %d", k, len(v))
			}
			got, ok := KindOf(v)
			if !ok || got != k {
				t.Fatalf("KindOf(새 %s 토큰) = %q, %v", k, got, ok)
			}
			if seen[v] {
				t.Fatalf("같은 토큰이 두 번 나왔다")
			}
			seen[v] = true
		}
	}
	dev := mustToken(t, DeviceToken)
	for _, bad := range []string{"", "fdd_", dev[:len(dev)-1], dev + "x", "fdd_" + strings.Repeat("!", tokenBody), "fdx_" + dev[4:], "plain-shared-token"} {
		if k, ok := KindOf(bad); ok {
			t.Errorf("KindOf(%q) = %q 로 받았다", bad, k)
		}
	}
}

func TestHashIsStableAndNotTheSecret(t *testing.T) {
	v := mustToken(t, DeviceToken)
	if Hash(v) != Hash(v) || len(Hash(v)) != 64 || strings.Contains(Hash(v), v[4:]) {
		t.Fatalf("Hash 가 안정적인 sha256 16진이 아니다: %q", Hash(v))
	}
	if Prefix(v) != v[:8] {
		t.Fatalf("Prefix = %q", Prefix(v))
	}
}

// TestPickPrecedence 는 자격증명 고르기의 계약이다: 헤더가 있으면 헤더만, 헤더의 세션 · 코드는 거절, 쿠키는 화면에서만, 세션 쿠키가
// 옛 쿠키보다 먼저다.
func TestPickPrecedence(t *testing.T) {
	dev, ses, code := mustToken(t, DeviceToken), mustToken(t, SessionToken), mustToken(t, LoginCode)
	cases := []struct {
		name    string
		req     Request
		kind    credKind
		value   string
		refusal string
	}{
		{"헤더 기기 토큰", Request{Authorization: "Bearer " + dev}, credDevice, dev, ""},
		{"헤더가 쿠키를 이긴다", Request{Authorization: "Bearer " + dev, SessionCookie: ses, ScreenPath: true}, credDevice, dev, ""},
		{"bearer 대소문자", Request{Authorization: "bearer " + dev}, credDevice, dev, ""},
		{"헤더 공유 토큰", Request{Authorization: "Bearer shared"}, credShared, "shared", ""},
		{"헤더 세션 쿠키 값", Request{Authorization: "Bearer " + ses}, 0, "", "화면 쿠키 · 로그인 코드는 헤더로 못 쓴다"},
		{"헤더 로그인 코드", Request{Authorization: "Bearer " + code}, 0, "", "화면 쿠키 · 로그인 코드는 헤더로 못 쓴다"},
		{"잘린 기기 토큰", Request{Authorization: "Bearer " + dev[:20]}, 0, "", "기기 토큰의 모양이 아니다"},
		{"Bearer 아님", Request{Authorization: "Basic abc"}, 0, "", "Bearer 가 아니다"},
		{"헤더 꼴이 아님", Request{Authorization: "Bearer a b"}, 0, "", "형식이 아니다"},
		{"화면 세션 쿠키", Request{SessionCookie: ses, ScreenPath: true}, credSession, ses, ""},
		{"세션 쿠키가 옛 쿠키를 이긴다", Request{SessionCookie: ses, SharedCookie: "shared", ScreenPath: true}, credSession, ses, ""},
		{"화면 옛 쿠키", Request{SharedCookie: "shared", ScreenPath: true}, credShared, "shared", ""},
		{"화면 세션 쿠키 모양 틀림", Request{SessionCookie: code, ScreenPath: true}, 0, "", "화면 세션 쿠키의 모양이 아니다"},
		{"REST 의 쿠키", Request{SessionCookie: ses}, 0, "", "이 경로는 화면이 아니다"},
		{"REST 의 옛 쿠키", Request{SharedCookie: "shared"}, 0, "", "이 경로는 화면이 아니다"},
		{"아무것도 없음", Request{}, credNone, "", ""},
	}
	for _, c := range cases {
		got, refused := pick(c.req, "shared")
		if c.refusal != "" {
			if refused == nil || !strings.Contains(refused.Reason, c.refusal) {
				t.Errorf("%s: 거절 %q 여야 하는데 %+v / %+v", c.name, c.refusal, got, refused)
			}
			continue
		}
		if refused != nil || got.kind != c.kind || got.value != c.value {
			t.Errorf("%s: (%v, %q) 여야 하는데 (%v, %q) / %+v", c.name, c.kind, c.value, got.kind, got.value, refused)
		}
	}
}

// TestPickReadsTheExactSharedTokenFirst 는 예약 접두와 겹치는 옛 FD_TOKEN 의 계약이다: 헤더 값이 서버의 FD_TOKEN 과 정확히 같으면
// 꼴(fdd_ · fds_ · fdl_)을 보기 전에 공유 토큰이다. 아니면 그런 FD_TOKEN 으로 붙던 훅 · MCP 가 판을 올린 순간 401 이다.
func TestPickReadsTheExactSharedTokenFirst(t *testing.T) {
	for _, k := range []TokenKind{DeviceToken, SessionToken, LoginCode} {
		v := mustToken(t, k)
		got, refused := pick(Request{Authorization: "Bearer " + v}, v)
		if refused != nil || got.kind != credShared || got.value != v {
			t.Errorf("%s 꼴 FD_TOKEN: 공유 토큰이어야 하는데 (%v, %q) / %+v", k, got.kind, got.value, refused)
		}
		// 같은 꼴의 다른 값은 그대로 그 꼴로 읽는다(먼저 보는 것은 정확히 같은 값 하나다).
		other := mustToken(t, k)
		if got, refused := pick(Request{Authorization: "Bearer " + other}, v); got.kind == credShared && refused == nil {
			t.Errorf("%s 꼴의 다른 값이 공유 토큰으로 읽혔다", k)
		}
		if !SharedTokenCollides(v) {
			t.Errorf("SharedTokenCollides(%s 꼴) 가 거짓이다", k)
		}
	}
	// 서버에 토큰이 없으면 빈 헤더 값과 맞대지 않는다.
	if got, _ := pick(Request{Authorization: "Bearer "}, ""); got.kind == credShared {
		t.Errorf("빈 FD_TOKEN 이 빈 헤더와 맞았다")
	}
	if SharedTokenCollides("plain-shared-secret") {
		t.Errorf("예약 접두가 없는 값이 겹친다고 나왔다")
	}
	ctx := context.Background()
	dev := mustToken(t, DeviceToken)
	for _, mode := range []Mode{ModeTransition} {
		d := NewGate(mode, dev, &fakeDir{}, nil).Authenticate(ctx, Request{Authorization: "Bearer " + dev})
		if !d.OK || d.Principal.Kind != KindShared {
			t.Errorf("%s: fdd_ 꼴 FD_TOKEN 이 통해야 한다: %+v", mode, d)
		}
	}
}

// TestSameSecretAgreesWithEquality 는 지문으로 맞댄 결과가 원값의 같음과 맞는가다(길이가 다른 쌍 · 빈 값 포함). 지문 위에서
// 맞대는 것 자체(길이가 시간으로 새지 않는 것)는 결과로 못 가르고 시간은 재지 않는다 - 원값을 곧장 맞대는 변이도 이 시험을
// 지난다. 그 축은 sameSecret 의 주석과 리뷰가 지킨다.
func TestSameSecretAgreesWithEquality(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"abc", "abc", true}, {"abc", "abcd", false}, {"", "", true}, {"", "x", false}, {"long-secret-value", "long-secret-valuf", false}} {
		if got := sameSecret(c.a, c.b); got != c.want {
			t.Errorf("sameSecret(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// TestJudgeFirstAdmin 은 첫 관리자를 만들 길이 없는 설정의 기동 거절이다.
func TestJudgeFirstAdmin(t *testing.T) {
	if refuse, why := JudgeFirstAdmin(ModeAccounts, false, 0); !refuse || !strings.Contains(why, "FD_TOKEN") {
		t.Errorf("accounts · 토큰 없음 · 관리자 0: 거절해야 한다: %v %q", refuse, why)
	}
	for _, c := range []struct {
		m      Mode
		token  bool
		admins int
	}{{ModeAccounts, true, 0}, {ModeAccounts, false, 1}, {ModeTransition, false, 0}, {ModeTransition, true, 0}} {
		if refuse, _ := JudgeFirstAdmin(c.m, c.token, c.admins); refuse {
			t.Errorf("%+v: 거절하면 안 된다", c)
		}
	}
}

// TestTransitionDefaultKeepsSharedTokenDeployment 는 전환 길의 핵심이다: 기본 단계(transition)에서 지금 배포(FD_TOKEN 하나로
// 기기 · 훅 · MCP · 화면이 붙는다)가 그대로 통하고, 토큰 없는 서버는 옛날처럼 무인증으로 통한다.
func TestTransitionDefaultKeepsSharedTokenDeployment(t *testing.T) {
	mode, _ := ParseMode("")
	ctx := context.Background()
	g := NewGate(mode, "shared-secret", &fakeDir{}, nil)
	for name, req := range map[string]Request{
		"헤더":       {Authorization: "Bearer shared-secret"},
		"화면 옛 쿠키":  {SharedCookie: "shared-secret", ScreenPath: true},
		"부트스트랩 경로": {Authorization: "Bearer shared-secret", BootstrapPath: true},
	} {
		d := g.Authenticate(ctx, req)
		if !d.OK || d.Principal.Kind != KindShared || !d.Principal.Admin() {
			t.Errorf("%s: 기본 단계에서 FD_TOKEN 이 안 통했다 - 갱신하는 순간 운영이 401 이다: %+v", name, d)
		}
	}
	if d := g.Authenticate(ctx, Request{Authorization: "Bearer wrong"}); d.OK {
		t.Errorf("틀린 토큰이 통했다: %+v", d)
	}
	if d := g.Authenticate(ctx, Request{}); d.OK || !strings.Contains(d.Reason, "루프백도 같다") {
		t.Errorf("토큰 켠 서버의 무자격 요청이 통했거나 사유가 다르다: %+v", d)
	}
	open := NewGate(mode, "", &fakeDir{}, nil)
	for name, req := range map[string]Request{"무자격": {}, "남은 토큰": {Authorization: "Bearer leftover"}} {
		if d := open.Authenticate(ctx, req); !d.OK || d.Principal.Kind != KindAnonymous {
			t.Errorf("%s: 토큰 없는 전환 단계 서버가 옛날처럼 통과시키지 않았다: %+v", name, d)
		}
	}
}

func TestAccountsModeConfinesSharedTokenToBootstrap(t *testing.T) {
	ctx := context.Background()
	dir := &fakeDir{}
	g := NewGate(ModeAccounts, "shared-secret", dir, nil)
	if d := g.Authenticate(ctx, Request{Authorization: "Bearer shared-secret"}); d.OK || !strings.Contains(d.Reason, "부트스트랩 전용") {
		t.Errorf("accounts 단계의 FD_TOKEN 이 부트스트랩 밖에서 통했다: %+v", d)
	}
	if d := g.Authenticate(ctx, Request{SharedCookie: "shared-secret", ScreenPath: true}); d.OK {
		t.Errorf("accounts 단계의 옛 쿠키가 통했다: %+v", d)
	}
	boot := Request{Authorization: "Bearer shared-secret", BootstrapPath: true}
	if d := g.Authenticate(ctx, boot); !d.OK || d.Principal.Kind != KindBootstrap || d.Principal.Admin() {
		t.Errorf("관리자 0명의 부트스트랩이 안 통했거나 관리자 주체가 됐다: %+v", d)
	}
	dir.admins = 1
	if d := g.Authenticate(ctx, boot); d.OK || !strings.Contains(d.Reason, "관리자가 이미 있다") {
		t.Errorf("관리자가 생긴 뒤 FD_TOKEN 이 부트스트랩에서 통했다: %+v", d)
	}
	dir.admins, dir.err = 0, errors.New("db down")
	if d := g.Authenticate(ctx, boot); d.OK || !d.Unavailable {
		t.Errorf("원장 장애가 503 이 아니다: %+v", d)
	}
	if d := NewGate(ModeAccounts, "", dir, nil).Authenticate(ctx, Request{}); d.OK {
		t.Errorf("accounts 단계에 무인증 갈래가 남았다: %+v", d)
	}
	if d := NewGate(ModeAccounts, "", dir, nil).Authenticate(ctx, Request{Authorization: "Bearer x"}); d.OK {
		t.Errorf("accounts 단계 · 토큰 없는 서버가 아무 값이나 받았다: %+v", d)
	}
}

func TestDeviceAndSessionCredentials(t *testing.T) {
	ctx := context.Background()
	dev, ses := mustToken(t, DeviceToken), mustToken(t, SessionToken)
	dir := &fakeDir{
		devices:  map[string]DeviceFact{Hash(dev): {MachineID: "m1", AccountID: "a1", Role: RoleMember, TokenHash: Hash(dev)}},
		sessions: map[string]SessionFact{Hash(ses): {IDHash: Hash(ses), AccountID: "a1", Role: RoleAdmin, ExpiresAt: t0.Add(time.Hour)}},
	}
	now := t0
	for _, mode := range []Mode{ModeTransition, ModeAccounts} {
		g := NewGate(mode, "shared-secret", dir, func() time.Time { return now })
		d := g.Authenticate(ctx, Request{Authorization: "Bearer " + dev})
		if !d.OK || d.Principal != (Principal{Kind: KindDevice, AccountID: "a1", Role: RoleMember, MachineID: "m1"}) {
			t.Errorf("%s: 기기 토큰 %+v", mode, d)
		}
		d = g.Authenticate(ctx, Request{SessionCookie: ses, ScreenPath: true})
		if !d.OK || d.Principal != (Principal{Kind: KindWeb, AccountID: "a1", Role: RoleAdmin, WebSession: Hash(ses), ExpiresAt: t0.Add(time.Hour)}) {
			t.Errorf("%s: 세션 쿠키 %+v", mode, d)
		}
	}
	g := NewGate(ModeAccounts, "", dir, func() time.Time { return now })
	unknown := mustToken(t, DeviceToken)
	if d := g.Authenticate(ctx, Request{Authorization: "Bearer " + unknown}); d.OK || d.Unavailable {
		t.Errorf("모르는 기기 토큰: %+v", d)
	}
	asked := dir.asked
	g.Authenticate(ctx, Request{Authorization: "Bearer fdd_short"})
	if dir.asked != asked {
		t.Errorf("모양이 틀린 기기 토큰으로 원장을 물었다")
	}
	now = t0.Add(time.Hour)
	if d := g.Authenticate(ctx, Request{SessionCookie: ses, ScreenPath: true}); d.OK || !strings.Contains(d.Reason, "만료") {
		t.Errorf("만료 시각의 세션이 통했다: %+v", d)
	}
	if d := NewGate(ModeAccounts, "", nil, nil).Authenticate(ctx, Request{Authorization: "Bearer " + dev}); d.OK {
		t.Errorf("원장 없는 게이트가 기기 토큰을 받았다: %+v", d)
	}
}

func TestJudgeDeviceAndSessionRefusals(t *testing.T) {
	h := Hash("x")
	ok := DeviceFact{MachineID: "m", AccountID: "a", Role: RoleMember, TokenHash: h}
	cases := []struct {
		name string
		f    DeviceFact
		err  error
		want string
		unav bool
	}{
		{"모름", DeviceFact{}, ErrUnknown, "서버가 모른다", false},
		{"장애", DeviceFact{}, errors.New("boom"), "원장에 붙지 못했다", true},
		{"지문 다름", DeviceFact{TokenHash: Hash("y")}, nil, "서버가 모른다", false},
		{"철회", func() DeviceFact { f := ok; f.Revoked = true; return f }(), nil, "철회된", false},
		{"계정 꺼짐", func() DeviceFact { f := ok; f.Disabled = true; return f }(), nil, "꺼져 있다", false},
	}
	for _, c := range cases {
		d := judgeDevice(h, c.f, c.err)
		if d.OK || d.Unavailable != c.unav || !strings.Contains(d.Reason, c.want) {
			t.Errorf("기기 %s: %+v", c.name, d)
		}
	}
	if d := judgeDevice(h, ok, nil); !d.OK {
		t.Errorf("맞는 기기: %+v", d)
	}
	sf := SessionFact{IDHash: h, AccountID: "a", Role: RoleMember, ExpiresAt: t0.Add(time.Minute)}
	if d := judgeSession(h, sf, nil, t0); !d.OK {
		t.Errorf("맞는 세션: %+v", d)
	}
	off := sf
	off.Disabled = true
	if d := judgeSession(h, off, nil, t0); d.OK || !strings.Contains(d.Reason, "꺼져 있다") {
		t.Errorf("꺼진 계정의 세션: %+v", d)
	}
	if d := judgeSession(h, sf, errors.New("boom"), t0); d.OK || !d.Unavailable {
		t.Errorf("세션 원장 장애: %+v", d)
	}
}

// TestReasonsNeverCarryTheCredential 은 사유에 자격증명 값이 안 실리는가다 - 사유는 401 본문 · 로그로 그대로 나간다.
func TestReasonsNeverCarryTheCredential(t *testing.T) {
	ctx := context.Background()
	dev, ses, code := mustToken(t, DeviceToken), mustToken(t, SessionToken), mustToken(t, LoginCode)
	secrets := []string{dev, ses, code, "shared-secret-value", "wrong-secret-value"}
	reqs := []Request{
		{Authorization: "Bearer " + dev}, {Authorization: "Bearer " + ses}, {Authorization: "Bearer " + code},
		{Authorization: "Bearer " + dev[:30]}, {Authorization: "Bearer wrong-secret-value"},
		{SessionCookie: ses, ScreenPath: true}, {SessionCookie: code, ScreenPath: true}, {SessionCookie: ses},
		{SharedCookie: "wrong-secret-value", ScreenPath: true}, {Authorization: "Bearer shared-secret-value"},
		{Authorization: "Bearer shared-secret-value", BootstrapPath: true},
	}
	for _, mode := range []Mode{ModeTransition, ModeAccounts} {
		for _, dir := range []Directory{nil, &fakeDir{}, &fakeDir{err: errors.New("db down"), admins: 1}} {
			g := NewGate(mode, "shared-secret-value", dir, nil)
			for _, r := range reqs {
				d := g.Authenticate(ctx, r)
				for _, s := range secrets {
					if strings.Contains(d.Reason, s) || strings.Contains(d.Reason, s[:12]) {
						t.Errorf("%s: 사유에 자격증명이 실렸다: %q", mode, d.Reason)
					}
				}
			}
		}
	}
}

func TestJudgeLogin(t *testing.T) {
	for _, ok := range []string{"admin", "kim.j", "a_b-c", "0day"} {
		if err := JudgeLogin(ok); err != nil {
			t.Errorf("JudgeLogin(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Admin", "-x", ".x", "a b", "a/b", "가나", strings.Repeat("a", loginMax+1), "a\x00"} {
		if err := JudgeLogin(bad); err == nil {
			t.Errorf("JudgeLogin(%q) 가 받았다", bad)
		}
	}
}
