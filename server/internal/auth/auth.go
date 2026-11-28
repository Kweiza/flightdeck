// Package auth 는 요청 → 주체의 판정이다(설계 「REST `/api/v1/` — 정본」 · 계정 단계).
//
// 자격증명은 넷이다: 기기 토큰(`fdd_`, Authorization 헤더), 화면 세션 쿠키(`fds_`), 일회용 로그인 코드(`fdl_`, 로그인 폼에서
// 세션 쿠키로 바꾼다), 공유 토큰(FD_TOKEN). 접두가 곧 종류다 - 한 종류의 값을 다른 자리에 내밀면 원장을 찾기 전에 거절한다.
//
// ★ 전환은 두 단계다(Mode). transition(기본)은 지금 배포를 그대로 받는다 - FD_TOKEN 이 헤더 · 옛 쿠키로 어디서나 통하고,
// 기기 토큰 · 세션 쿠키도 함께 받는다. 각 기기가 fd login 으로 옮긴 뒤 FD_AUTH=accounts 로 좁히면 FD_TOKEN 은 관리자가 0명일
// 때의 /admin/bootstrap 에만 통한다. 기본값을 accounts 로 두면 갱신하는 순간 FD_TOKEN 하나로 붙던 기기 · 훅 · 보드가 전부
// 401 이다.
//
// ★ 루프백 면제는 없다(두 단계 모두). 토큰을 켠 서버는 루프백에서도 자격증명을 요구한다. FD_TOKEN 이 빈 서버가 무인증으로 도는
// 것은 transition 에만 남은 옛 동작이고, accounts 에는 그 갈래가 없다.
//
// 판정은 순수 함수 셋(pick · judgeDevice · judgeSession)이고, FD_TOKEN 갈래(shared)와 원장 조회(Directory)는 Gate 가 그 사이에 한다.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Mode 는 인증 단계다(FD_AUTH).
type Mode string

const (
	// ModeTransition 은 기본값이다: FD_TOKEN 과 계정 자격증명을 함께 받는다.
	ModeTransition Mode = "transition"
	// ModeAccounts 는 계정 자격증명만 받는다. FD_TOKEN 은 부트스트랩 전용이다.
	ModeAccounts Mode = "accounts"
)

// ParseMode 는 FD_AUTH 값을 읽는다. 비면 transition 이고, 모르는 값은 오류다 - 오타를 기본값으로 접으면 좁혔다고 믿는
// 서버가 FD_TOKEN 을 어디서나 받는다.
func ParseMode(v string) (Mode, error) {
	switch m := Mode(strings.TrimSpace(v)); m {
	case "":
		return ModeTransition, nil
	case ModeTransition, ModeAccounts:
		return m, nil
	}
	return "", fmt.Errorf("FD_AUTH=%q 를 모른다 - transition(기본) 또는 accounts 다", clip(v, 40))
}

// ─────────────────────────────────────────────────────────────────────────────
// 토큰
// ─────────────────────────────────────────────────────────────────────────────

// TokenKind 는 비밀 값의 종류이고 그 값의 접두다.
type TokenKind string

const (
	// DeviceToken 은 기기 토큰이다(Authorization: Bearer). 원장에는 sha256 과 앞 8글자만(machine.token_hash · token_prefix).
	DeviceToken TokenKind = "fdd_"
	// SessionToken 은 화면 세션 쿠키다. 원장에는 sha256(web_session.id_hash).
	SessionToken TokenKind = "fds_"
	// LoginCode 는 일회용 로그인 코드다(부트스트랩 · 복구가 낸다). 로그인 폼에 붙여 넣으면 세션 쿠키로 한 번 바뀌고 사라진다.
	// 원장에서는 세션 쿠키와 같은 표(web_session)의 행이다 - 아직 브라우저에 안 건넨 세션이다. 쿠키로는 못 쓴다(접두가 다르다).
	LoginCode TokenKind = "fdl_"
)

// tokenBody 는 접두 뒤 글자 수다: 32바이트 난수를 base62 로 적은 길이(⌈256 / log2 62⌉ = 43). 앞을 0 으로 채워 늘 같다.
const tokenBody = 43

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// NewToken 은 그 종류의 새 비밀 값이다. 엔트로피가 256비트라 원장에는 느린 해시가 아니라 sha256 이면 된다.
func NewToken(k TokenKind) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("난수를 못 읽었다: %w", err)
	}
	n := new(big.Int).SetBytes(b[:])
	body := make([]byte, tokenBody)
	sixtyTwo := big.NewInt(62)
	mod := new(big.Int)
	for i := tokenBody - 1; i >= 0; i-- {
		n.DivMod(n, sixtyTwo, mod)
		body[i] = base62[mod.Int64()]
	}
	return string(k) + string(body), nil
}

// KindOf 는 값이 어느 종류의 **모양**인가다. 접두 + base62 43글자가 아니면 거짓이다 - 원장 조회 전에 거른다.
func KindOf(v string) (TokenKind, bool) {
	for _, k := range []TokenKind{DeviceToken, SessionToken, LoginCode} {
		body, ok := strings.CutPrefix(v, string(k))
		if !ok {
			continue
		}
		if len(body) != tokenBody {
			return "", false
		}
		for i := 0; i < len(body); i++ {
			if strings.IndexByte(base62, body[i]) < 0 {
				return "", false
			}
		}
		return k, true
	}
	return "", false
}

// Hash 는 원장에 담는 지문이다(sha256 16진).
func Hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// Prefix 는 화면에서 기기를 가리는 앞 8글자다(접두 4 + 본문 4). 이것만으로는 토큰을 못 쓴다.
func Prefix(v string) string {
	if len(v) <= 8 {
		return v
	}
	return v[:8]
}

// sameSecret 은 두 비밀을 상수 시간으로 맞댄다. 양쪽을 sha256 으로 먼저 접어 같은 길이(32바이트)로 맞댄다 -
// subtle.ConstantTimeCompare 는 길이가 다르면 곧장 거짓을 내므로, 원값을 그대로 주면 FD_TOKEN 처럼 형식이 안 정한 비밀의
// 길이가 응답 시간으로 샌다.
func sameSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// SharedTokenMatches 는 받은 값이 서버의 FD_TOKEN 인가다. 서버에 토큰이 없으면 언제나 거짓이다.
func SharedTokenMatches(serverToken, got string) bool {
	return serverToken != "" && sameSecret(got, serverToken)
}

// SharedSecret 은 SecretOf 가 FD_TOKEN 과 정확히 같은 값에 내는 종류다. 접두가 아니다 - NewToken · KindOf 의 종류가 아니다.
const SharedSecret TokenKind = "FD_TOKEN"

// SecretOf 는 내민 비밀 값의 종류다. 순수 함수다. 어느 꼴도 아니면 빈 글이다. sharedToken 은 서버의 FD_TOKEN 이다(비면 없다).
//
// ★ **FD_TOKEN 과 정확히 같으면 꼴과 무관하게 SharedSecret 이다.** 접두(fdd_ · fds_ · fdl_)는 이 판이 새로 예약한 것이라 그보다
// 먼저 정한 FD_TOKEN 이 우연히 그 꼴일 수 있다. 꼴을 먼저 보면 그 배포는 갱신하는 순간 그 입구에서 401 이다. 자격증명이 들어오는
// 두 입구 - 헤더(pick)와 화면 로그인 폼(api 의 handleLogin) - 가 이 한 자리로 가른다. 따로 가르면 한쪽만 꼴을 먼저 본다(헤더는
// 통하는데 같은 FD_TOKEN 을 폼에 넣으면 로그인 코드 교환으로 빠져 401 이던 결함이 그 모양이다).
func SecretOf(sharedToken, v string) TokenKind {
	if SharedTokenMatches(sharedToken, v) {
		return SharedSecret
	}
	k, _ := KindOf(v)
	return k
}

// SharedTokenCollides 는 FD_TOKEN 이 이 판이 예약한 접두(fdd_ · fds_ · fdl_)로 시작하는가다. 순수 함수다. 겹쳐도 통한다(pick 의
// ②가 정확히 같은 값을 먼저 FD_TOKEN 으로 읽는다) - 기동이 경고만 한다. 거절하면 그 FD_TOKEN 으로 붙던 배포가 갱신하는 순간
// 서버째 못 뜬다.
func SharedTokenCollides(token string) bool {
	for _, k := range []TokenKind{DeviceToken, SessionToken, LoginCode} {
		if strings.HasPrefix(token, string(k)) {
			return true
		}
	}
	return false
}

// JudgeFirstAdmin 은 이 설정으로 첫 관리자를 만들 길이 있는가다. 순수 함수다. 참이면 기동을 거절한다.
//
// accounts 단계에서 관리자가 0명이면 첫 관리자는 FD_TOKEN 으로 여는 POST /admin/bootstrap 으로만 생긴다(fd admin recover 는
// 이미 있는 관리자의 코드를 낼 뿐이다). 그때 FD_TOKEN 까지 비면 그 서버는 누구도 로그인할 수 없는 채로 뜬다 - 아무 요청에도
// 401 만 내는 서버를 띄우지 않는다. transition 은 토큰이 비면 옛 동작(무인증 전권)이라 막지 않는다.
func JudgeFirstAdmin(m Mode, tokenSet bool, admins int) (refuse bool, reason string) {
	if m != ModeAccounts || tokenSet || admins > 0 {
		return false, ""
	}
	return true, "FD_AUTH=accounts 인데 관리자가 0명이고 FD_TOKEN 이 비었다 - 첫 관리자를 만들 길(FD_TOKEN 으로 여는 " +
		"POST /admin/bootstrap)이 없다. FD_TOKEN 을 주고 띄워 fd admin bootstrap 으로 첫 관리자를 만들어라"
}

// ─────────────────────────────────────────────────────────────────────────────
// 주체
// ─────────────────────────────────────────────────────────────────────────────

// Kind 는 무엇이 이 요청을 인증했는가다.
type Kind string

const (
	// KindDevice 는 기기 토큰이다 - 계정과 기기가 정해진다.
	KindDevice Kind = "device"
	// KindWeb 은 화면 세션 쿠키다 - 계정이 정해지고 기기는 없다.
	KindWeb Kind = "web"
	// KindShared 는 transition 단계의 FD_TOKEN 이다 - 누구인지 모른다. 지금까지처럼 전권이다(Admin).
	KindShared Kind = "shared"
	// KindBootstrap 은 accounts 단계에서 관리자 0명일 때 /admin/bootstrap 에 온 FD_TOKEN 이다. 첫 관리자를 만드는 일만 한다.
	KindBootstrap Kind = "bootstrap"
	// KindAnonymous 는 FD_TOKEN 이 빈 transition 서버의 무인증 통과다(옛 동작). accounts 에는 없다. 옛 동작 그대로 전권이다
	// (Admin) - 무인증이 항목 · 판단 · 선점 쓰기를 다 받는 서버에서 프로젝트 지우기 · 계정 끄기만 막으면 경계가 아니라 막다른
	// 길이다(그 서버에서는 부트스트랩도 못 해 관리자가 될 길이 없다). 경계가 필요하면 FD_TOKEN 을 켜거나 accounts 로 좁힌다.
	KindAnonymous Kind = "anonymous"
)

// 계정 역할(account.role).
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Principal 은 요청 하나의 주체다. 쓰기의 행위자는 이것에서 온다(요청 본문의 칸에서가 아니다).
type Principal struct {
	Kind      Kind
	AccountID string // 기기 · 세션이면 그 계정. 공유 토큰 · 부트스트랩 · 무인증이면 빈 글
	Role      string // 계정의 역할. 공유 토큰 · 무인증은 admin(전권), 부트스트랩은 빈 글
	MachineID string // 기기 토큰이면 그 기기
	// WebSession 은 세션 쿠키의 원장 지문(web_session.id_hash)이다. 원값이 아니다.
	WebSession string
	// ExpiresAt 은 이 자격증명이 스스로 끝나는 시각이다(화면 세션의 expires_at). 영값이면 끝이 없다(기기 토큰 · FD_TOKEN).
	// 열린 스트림은 다음 요청이 없어 게이트가 만료를 다시 못 보므로, 스트림이 이 시각에 스스로 닫는다.
	ExpiresAt time.Time
}

// Admin 은 관리자 일을 해도 되는가다.
func (p Principal) Admin() bool { return p.Role == RoleAdmin }

type ctxKey struct{}

// WithPrincipal 은 ctx 에 주체를 싣는다(인증 게이트가 한 번).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext 는 게이트가 실은 주체다. 게이트를 안 지난 ctx 면 거짓이다.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// ─────────────────────────────────────────────────────────────────────────────
// 원장 조회
// ─────────────────────────────────────────────────────────────────────────────

// ErrUnknown 은 원장에 그 자격증명이 없다는 것이다(지문이 맞는 행이 0).
var ErrUnknown = errors.New("원장에 없는 자격증명이다")

// DeviceFact 는 기기 토큰 지문으로 찾은 행이다.
type DeviceFact struct {
	MachineID, AccountID, Role string
	TokenHash                  string // 찾은 행의 지문(상수 시간으로 다시 맞댄다)
	Revoked, Disabled          bool   // 기기가 철회됐다 · 계정이 꺼졌다
}

// SessionFact 는 세션 쿠키 지문으로 찾은 행이다.
type SessionFact struct {
	IDHash, AccountID, Role string
	ExpiresAt               time.Time
	Disabled                bool
}

// Directory 는 판정이 묻는 원장이다(store 가 만족한다). 캐시하지 않는다 - 철회는 다음 요청부터 듣는다.
type Directory interface {
	DeviceCredential(ctx context.Context, tokenHash string) (DeviceFact, error)
	WebSessionCredential(ctx context.Context, idHash string) (SessionFact, error)
	AdminCount(ctx context.Context) (int, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// 판정
// ─────────────────────────────────────────────────────────────────────────────

// Request 는 판정에 필요한 요청 사실이다. 경로의 뜻(화면인가 · 부트스트랩인가)은 표면이 판정해 싣는다.
type Request struct {
	Authorization string // 헤더 원문
	SessionCookie string // 화면 세션 쿠키 값
	SharedCookie  string // 옛 토큰 쿠키(FD_TOKEN 을 담은 fd_token) 값
	// ScreenPath 는 쿠키를 인정하는 경로인가다. 거짓이면 쿠키를 안 본다(REST 는 헤더 전용).
	ScreenPath bool
	// BootstrapPath 는 /admin/bootstrap 인가다 - accounts 단계의 FD_TOKEN 이 통하는 유일한 자리다.
	BootstrapPath bool
}

// Decision 은 판정 한 번이다. 사유는 늘 채운다 - "헤더가 없다"와 "철회됐다"와 "부트스트랩 전용이다"는 처방이 다르다.
//
// ★ 사유에 자격증명 값을 싣지 않는다(접두조차). 사유는 401 본문으로 그대로 나간다.
type Decision struct {
	OK        bool
	Principal Principal
	Reason    string
	// Unavailable 은 원장을 못 읽어 판정하지 못했다는 것이다(503). 401 로 접으면 DB 장애가 "토큰이 틀렸다"로 읽힌다.
	Unavailable bool
}

// Authenticator 는 요청 → 주체다(확장 자리 - 나중 OIDC · SSO 는 이것을 바꿔 끼운다). fd serve 조립이 주입한다.
type Authenticator interface {
	Authenticate(ctx context.Context, req Request) Decision
}

// Gate 는 지금 구현이다: 기기 토큰 + 세션 쿠키 + FD_TOKEN.
type Gate struct {
	mode  Mode
	token string
	dir   Directory
	now   func() time.Time
}

// NewGate 는 판정기를 만든다. dir 이 nil 이면 계정 자격증명은 모두 "원장이 없다"로 거절한다(원장 없는 시험 조립).
func NewGate(mode Mode, token string, dir Directory, now func() time.Time) *Gate {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Gate{mode: mode, token: token, dir: dir, now: now}
}

// credential 은 요청이 내민 자격증명 하나다.
type credential struct {
	kind  credKind
	value string
}

type credKind int

const (
	credNone credKind = iota
	credDevice
	credSession
	credShared // 헤더의 기기 토큰 아닌 값 · 옛 토큰 쿠키
)

// pick 은 요청이 내민 자격증명 하나를 고른다. 순수 함수다. 못 고르면 거절 판정을 함께 낸다. sharedToken 은 서버의 FD_TOKEN 이다
// (비면 없다).
//
// 순서가 계약이다.
//  1. **헤더가 있으면 헤더만 본다.** 쿠키는 안 본다 - 무엇이 인증했는지가 요청마다 달라지면 안 된다.
//  2. **헤더 값이 FD_TOKEN 과 정확히 같으면 접두와 무관하게 FD_TOKEN 이다**(SecretOf - 로그인 폼과 같은 한 자리). 접두를 먼저
//     보면 그 배포의 훅 · MCP 가 갱신하는 순간 전부 401 이다. 기동이 그 FD_TOKEN 을 경고한다(fd serve).
//  3. 헤더의 세션 쿠키 · 로그인 코드는 거절이다. 코드는 로그인 폼에서 한 번 바꾸는 값이고, 헤더로 받으면 일회용이 아니게 된다.
//  4. 쿠키는 화면 경로에서만 본다. 세션 쿠키가 옛 토큰 쿠키보다 먼저다(둘 다 있으면 로그인한 계정이 이긴다).
func pick(req Request, sharedToken string) (credential, *Decision) {
	if h := strings.TrimSpace(req.Authorization); h != "" {
		parts := strings.Fields(h)
		if len(parts) != 2 {
			return credential{}, &Decision{Reason: "Authorization 헤더가 'Bearer <token>' 형식이 아니다"}
		}
		if !strings.EqualFold(parts[0], "bearer") {
			return credential{}, &Decision{Reason: "Authorization 인증 방식이 Bearer 가 아니다"}
		}
		v := parts[1]
		switch SecretOf(sharedToken, v) {
		case SharedSecret:
			return credential{credShared, v}, nil
		case DeviceToken:
			return credential{credDevice, v}, nil
		case SessionToken, LoginCode:
			return credential{}, &Decision{Reason: "이 값은 기기 토큰이 아니다 - 화면 쿠키 · 로그인 코드는 헤더로 못 쓴다"}
		}
		if strings.HasPrefix(v, string(DeviceToken)) {
			return credential{}, &Decision{Reason: "기기 토큰의 모양이 아니다 - 잘렸거나 다른 값이 섞였다"}
		}
		return credential{credShared, v}, nil
	}
	session := strings.TrimSpace(req.SessionCookie)
	shared := strings.TrimSpace(req.SharedCookie)
	if req.ScreenPath {
		if session != "" {
			if k, ok := KindOf(session); !ok || k != SessionToken {
				return credential{}, &Decision{Reason: "화면 세션 쿠키의 모양이 아니다 - 다시 로그인해라"}
			}
			return credential{credSession, session}, nil
		}
		if shared != "" {
			return credential{credShared, shared}, nil
		}
	} else if session != "" || shared != "" {
		// ★ 쿠키를 들고 REST 를 친 경우다. "헤더가 없다"로 접으면 브라우저에서 REST 를 부르는 도구를 만든 사람이 원인을 못 찾는다.
		return credential{}, &Decision{Reason: "쿠키가 있지만 이 경로는 화면이 아니다 - /api/v1 은 Authorization 헤더만 받는다"}
	}
	return credential{kind: credNone}, nil
}

// Authenticate 는 요청 하나를 판정한다.
func (g *Gate) Authenticate(ctx context.Context, req Request) Decision {
	c, refused := pick(req, g.token)
	if refused != nil {
		return *refused
	}
	switch c.kind {
	case credDevice:
		return g.device(ctx, c.value)
	case credSession:
		return g.session(ctx, c.value)
	case credShared:
		return g.shared(ctx, c.value, req.BootstrapPath)
	}
	if g.mode == ModeTransition && g.token == "" {
		return Decision{OK: true, Principal: anonymous,
			Reason: "서버에 토큰이 설정되지 않았다 - 전환 단계라 무인증으로 통과했다"}
	}
	return Decision{Reason: "자격증명이 없다 - Authorization: Bearer <기기 토큰> 을 실어라(루프백도 같다)"}
}

// anonymous 는 토큰 없는 전환 서버의 무인증 주체다(KindAnonymous 의 ★ - 옛 동작 그대로 전권).
var anonymous = Principal{Kind: KindAnonymous, Role: RoleAdmin}

// shared 는 FD_TOKEN 갈래다.
func (g *Gate) shared(ctx context.Context, v string, bootstrapPath bool) Decision {
	if g.token == "" {
		if g.mode == ModeTransition {
			// 옛 동작: 토큰이 없는 서버는 무엇을 내밀든 무인증으로 통과한다(클라이언트에 토큰이 남은 채 서버만 끈 배포).
			return Decision{OK: true, Principal: anonymous,
				Reason: "서버에 토큰이 설정되지 않아 대조할 기준이 없다 - 전환 단계라 무인증으로 통과했다"}
		}
		return Decision{Reason: "이 값은 기기 토큰이 아니고 이 서버에는 FD_TOKEN 이 없다 - fd login 으로 기기 토큰을 받아라"}
	}
	if !sameSecret(v, g.token) {
		return Decision{Reason: "토큰이 일치하지 않는다"}
	}
	if g.mode == ModeTransition {
		return Decision{OK: true, Principal: Principal{Kind: KindShared, Role: RoleAdmin},
			Reason: "FD_TOKEN 이 일치한다(전환 단계 - 기기마다 fd login 을 마치면 FD_AUTH=accounts 로 좁혀라)"}
	}
	if !bootstrapPath {
		return Decision{Reason: "FD_TOKEN 은 부트스트랩 전용이다(FD_AUTH=accounts) - 기기 토큰(fd login)이나 화면 로그인을 써라"}
	}
	if g.dir == nil {
		return Decision{Unavailable: true, Reason: "계정 원장이 없어 부트스트랩 여부를 못 쟀다"}
	}
	n, err := g.dir.AdminCount(ctx)
	if err != nil {
		return Decision{Unavailable: true, Reason: "관리자 수를 못 읽었다 - 원장에 붙지 못했다"}
	}
	if n > 0 {
		return Decision{Reason: "관리자가 이미 있다 - FD_TOKEN 은 첫 관리자를 만들 때만 통한다. " + RecoverHint}
	}
	return Decision{OK: true, Principal: Principal{Kind: KindBootstrap},
		Reason: "관리자가 없다 - FD_TOKEN 으로 첫 관리자를 만든다"}
}

// device 는 기기 토큰 갈래다.
func (g *Gate) device(ctx context.Context, v string) Decision {
	if g.dir == nil {
		return Decision{Reason: "이 서버는 계정 원장이 없어 기기 토큰을 대조할 수 없다"}
	}
	h := Hash(v)
	f, err := g.dir.DeviceCredential(ctx, h)
	return judgeDevice(h, f, err)
}

// judgeDevice 는 조회 결과를 판정으로 옮긴다. 순수 함수다.
func judgeDevice(want string, f DeviceFact, err error) Decision {
	switch {
	case errors.Is(err, ErrUnknown):
		return Decision{Reason: "이 기기 토큰을 서버가 모른다 - 다른 서버의 토큰이거나 지워진 기기다. fd login 으로 다시 받아라"}
	case err != nil:
		return Decision{Unavailable: true, Reason: "기기 토큰을 못 대조했다 - 원장에 붙지 못했다"}
	case !sameSecret(f.TokenHash, want):
		return Decision{Reason: "이 기기 토큰을 서버가 모른다 - 다른 서버의 토큰이거나 지워진 기기다. fd login 으로 다시 받아라"}
	case f.Revoked:
		return Decision{Reason: "철회된 기기 토큰이다 - fd login 으로 새로 받아라"}
	case f.Disabled:
		return Decision{Reason: "이 기기의 계정이 꺼져 있다 - 관리자에게 물어라"}
	}
	return Decision{OK: true, Reason: "기기 토큰이 일치한다",
		Principal: Principal{Kind: KindDevice, AccountID: f.AccountID, Role: f.Role, MachineID: f.MachineID}}
}

// session 은 화면 세션 쿠키 갈래다.
func (g *Gate) session(ctx context.Context, v string) Decision {
	if g.dir == nil {
		return Decision{Reason: "이 서버는 계정 원장이 없어 로그인 쿠키를 대조할 수 없다 - 다시 로그인해라"}
	}
	h := Hash(v)
	f, err := g.dir.WebSessionCredential(ctx, h)
	return judgeSession(h, f, err, g.now())
}

// judgeSession 은 조회 결과를 판정으로 옮긴다. 순수 함수다.
func judgeSession(want string, f SessionFact, err error, now time.Time) Decision {
	switch {
	case errors.Is(err, ErrUnknown):
		return Decision{Reason: "로그인이 끝났다(로그아웃했거나 지워졌다) - 다시 로그인해라"}
	case err != nil:
		return Decision{Unavailable: true, Reason: "로그인 쿠키를 못 대조했다 - 원장에 붙지 못했다"}
	case !sameSecret(f.IDHash, want):
		return Decision{Reason: "로그인이 끝났다(로그아웃했거나 지워졌다) - 다시 로그인해라"}
	case !now.Before(f.ExpiresAt):
		return Decision{Reason: "로그인이 만료됐다 - 다시 로그인해라"}
	case f.Disabled:
		return Decision{Reason: "이 계정이 꺼져 있다 - 관리자에게 물어라"}
	}
	return Decision{OK: true, Reason: "로그인 쿠키가 일치한다",
		Principal: Principal{Kind: KindWeb, AccountID: f.AccountID, Role: f.Role, WebSession: f.IDHash, ExpiresAt: f.ExpiresAt}}
}

func clip(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// RecoverHint 는 관리자가 이미 있는 원장에서 로그인 코드를 얻는 길이다. 부트스트랩 거절 두 자리(게이트 · 원장)가 같은 말을 한다.
// 옛 원장을 이어받은 서버는 판올림(020)이 만든 소유자 계정(login owner, 관리자)이 이미 있어 부트스트랩이 처음부터 닫혀 있다.
const RecoverHint = "관리자 로그인 코드는 서버 컨테이너 안의 fd admin recover <login> 이 낸다(옛 원장을 이어받았으면 login 은 owner 다)"

// SessionTTL 은 화면 세션의 수명이다(설계: 웹 세션 14일).
const SessionTTL = 14 * 24 * time.Hour

// LoginCodeTTL 은 일회용 로그인 코드의 수명이다. 코드는 터미널 · 응답 본문에 한 번 보이는 값이라 짧게 둔다.
const LoginCodeTTL = time.Hour

// loginMax 는 로그인 이름의 최대 길이다.
const loginMax = 64

// JudgeLogin 은 새 계정의 로그인 이름이 쓸 만한가다. 순수 함수다. 소문자 · 숫자 · `.` `_` `-` 만, 첫 글자는 소문자나 숫자다 -
// 화면 · 명령줄 · 로그에 그대로 실리는 이름이라 공백 · 대소문자 겹침 · 제어 문자를 처음부터 막는다.
func JudgeLogin(login string) error {
	if login == "" {
		return errors.New("로그인 이름이 비었다")
	}
	if len(login) > loginMax {
		return fmt.Errorf("로그인 이름이 %d 자를 넘는다", loginMax)
	}
	for i := 0; i < len(login); i++ {
		c := login[i]
		alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if i == 0 && !alnum {
			return errors.New("로그인 이름은 소문자나 숫자로 시작한다")
		}
		if !alnum && c != '.' && c != '_' && c != '-' {
			return fmt.Errorf("로그인 이름에 쓸 수 없는 글자가 있다(%d 번째) - 소문자 · 숫자 · . _ - 만 쓴다", i+1)
		}
	}
	return nil
}
