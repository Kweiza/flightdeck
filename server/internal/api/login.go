package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kweiza/flightdeck/internal/auth"
	"github.com/kweiza/flightdeck/internal/judge"
	"github.com/kweiza/flightdeck/internal/store"
)

// 화면 로그인 — 브라우저가 자격증명을 내놓는 유일한 길이다(설계 「REST `/api/v1/` — 정본」).
//
// ★ 이 파일이 존재하는 이유. 게이트는 Authorization 헤더를 읽는데 브라우저는 최초
// 요청에 임의 헤더를 못 싣는다. 응답이 WWW-Authenticate: Bearer 라 로그인 팝업도 안 뜬다
// (브라우저는 Basic 에만 띄운다). 그래서 폼으로 받은 값을 쿠키로 바꾼다.
//
// 폼이 받는 값은 둘이다.
//   - 로그인 코드(`fdl_`, 부트스트랩 · fd admin recover 가 낸다): 한 번 바꿔 화면 세션 쿠키(fd_session, 14일)를 굽는다.
//   - FD_TOKEN: **전환 단계에서만.** 옛 쿠키(fd_token)를 굽는다. 계정 단계에서는 거절한다 - FD_TOKEN 은 부트스트랩 전용이다.
//
// ★ 화면을 게이트 사슬 밖으로 빼는 처방은 기각이다. 그 사슬은 무인증 POST 로 항목이
// 실제 폐기된 사고의 처방이고 여전히 옳다(api.go 의 Fallback 주석).

// loginCookieMaxAge 는 옛 로그인 쿠키(FD_TOKEN)의 수명(초)이다 — 10년. 전환 단계의 옛 동작 그대로다 - 그 쿠키는 FD_TOKEN 을
// 바꾸면 죽고, 계정 단계에서는 인정되지 않는다.
const loginCookieMaxAge = 10 * 365 * 24 * 60 * 60

// LoginCookie 는 쿠키 한 벌을 만든다. 순수 함수다.
//
// ★ 속성이 넷 다 필요하다. Path=/ 가 아니면 /events 와 /actions 중 한쪽이 안 닿고,
// HttpOnly 가 없으면 XSS 하나로 토큰이 새고, SameSite=Strict 가 없으면 크로스사이트
// 요청에 쿠키가 실린다. 시험이 넷을 개별로 단정하는 이유가 이것이다 — 하나가 빠져도
// 로그인은 멀쩡히 되므로 왕복 시험만으로는 안 잡힌다.
func LoginCookie(name, value string, maxAge int, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
		Secure:   secure,
	}
}

// seeOther 는 303 과 함께 **상대 참조**를 Location 에 싣는다.
//
// ★ **http.Redirect 를 안 쓴다.** 그 함수는 상대 URL 을 받아도 요청 경로 기준으로
// 절대화해서 내보낸다(net/http server.go 의 olddir+url → path.Clean). 그래서 `./` 를
// 줘도 `/` 가 나가고, 접두를 벗기는 리버스 프록시 뒤에서 그 값은 접두 **밖**으로
// 떨어진다 — 로그인은 성공했는데 화면을 못 보는 그 결함이다.
//
// ★ **to 는 절대경로로 준다.** 상대화는 여기서 한 번만 한다. 호출자가 `../` 를 손으로
// 박으면 새 라우트가 깊이를 바꿀 때 조용히 틀리고, 그 값이 http.Redirect 를 지나며
// 절대화되면 틀린 사실조차 안 보인다(web/actions.go 가 정확히 그 모양이었다).
//
// ★ 본문을 안 쓴다. http.Redirect 는 GET 에 <a href> 한 줄을 붙이는데, 303 을 못 따르는
// 옛 클라이언트를 위한 것이고 이 화면의 대상이 아니다.
func (s *server) seeOther(w http.ResponseWriter, r *http.Request, to string) {
	w.Header().Set("Location", judge.RelativeTo(r.URL.Path, to))
	w.WriteHeader(http.StatusSeeOther)
}

// handleLogin 은 폼 값을 받아 쿠키를 굽는다(이 파일 머리말의 두 갈래).
//
// GET 은 "/" 로 보낸다 — 사용자가 주소창에 칠 수 있는 URL 이라, 없으면 화면의
// 404("대시보드는 / 한 장이다")가 나와 혼란스럽다. 인증이 안 됐으면 거기서 폼을 만난다.
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.seeOther(w, r, "/")
		return
	}
	if s.refusedCrossSite(w, r, "로그인", "대시보드 화면의 폼에서 제출해라.") {
		return
	}
	// ★ 본문 상한을 **손으로** 건다. 이 경로는 세션 이전이라 withIdempotency 를 통째로
	// 건너뛰는데(멱등 키를 만들 세션이 없다) MaxBodyBytes 를 거는 자리가 거기뿐이다.
	// 안 걸면 ParseForm 의 기본값 10MiB 가 서고, 그러면 **아무나 칠 수 있는 무인증
	// 표면 하나가 인증을 통과한 REST 쓰기 전부의 10배**를 받는다 — 방향이 거꾸로다.
	r.Body = http.MaxBytesReader(w, r.Body, s.opt.MaxBodyBytes)
	if err := r.ParseForm(); err != nil {
		// 상한 초과도 여기로 온다. 사유를 나누지 않는 이유: 이 응답을 받는 것은 브라우저이고
		// 처방이 "다시 제출해라"로 같다. 크기 축은 /metrics 의 unauthorized 로 센다.
		s.loginRefused(w, r, "폼을 읽을 수 없다", "/")
		return
	}
	next := JudgeNext(r.PostFormValue("next"))
	got := strings.TrimSpace(r.PostFormValue("token"))
	// ★ FD_TOKEN 과 정확히 같은 값을 꼴보다 먼저 가른다(auth.SecretOf - 헤더의 pick 과 같은 한 자리). 예약 접두(fdl_)로 시작하는
	// 옛 FD_TOKEN 을 꼴로 먼저 보면 로그인 코드 교환으로 빠져 401 이다 - 헤더로는 통하는 같은 값이 폼에서만 막힌다.
	switch k := auth.SecretOf(s.opt.Token, got); {
	case k == auth.SharedSecret && s.opt.AuthMode != auth.ModeAccounts:
		s.grantLoginCookie(w, r, LoginCookieName, s.opt.Token, loginCookieMaxAge)
		s.seeOther(w, r, next)
		return
	case k == auth.LoginCode:
		s.exchangeLoginCode(w, r, got, next)
		return
	}
	if s.opt.AuthMode == auth.ModeAccounts {
		if got == "" {
			s.loginRefused(w, r, "로그인 코드를 적어라", next)
			return
		}
		s.loginRefused(w, r, "로그인 코드가 아니다 - 관리자에게 로그인 코드를 받아라. FD_TOKEN 은 화면 로그인에 못 쓴다(부트스트랩 전용)", next)
		return
	}
	// ★ 토큰이 없는 전환 단계 서버는 쿠키를 굽지 않는다. 구우면 나중에 토큰을 켰을 때 그 쿠키가
	// **틀린 자격증명**이 되어, 브라우저가 폼이 아니라 거절을 만난다.
	if s.opt.Token == "" {
		s.seeOther(w, r, next)
		return
	}
	if got == "" {
		s.loginRefused(w, r, "토큰이나 로그인 코드를 적어라", next)
		return
	}
	s.loginRefused(w, r, "토큰이 일치하지 않는다", next)
}

// grantLoginCookie 는 로그인 성공이 굽는 쿠키 한 벌이다: 고른 쿠키를 굽고 다른 하나를 지운다. 두 로그인 갈래(FD_TOKEN · 로그인
// 코드)가 이 한 자리를 쓴다.
//
// ★ 둘 다 남기지 않는다. auth.pick 은 세션 쿠키를 옛 쿠키보다 먼저 집으므로, 낡은(철회 · 만료된) 세션 쿠키가 남은 브라우저는
// FD_TOKEN 으로 로그인해도 그 세션 쿠키에 가려 303 뒤 다시 401 이다. 반대로 세션 로그인 뒤 남은 옛 쿠키는 FD_TOKEN 원값을
// 브라우저에 둔 채다.
func (s *server) grantLoginCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	other := SessionCookieName
	if name == SessionCookieName {
		other = LoginCookieName
	}
	s.putCookie(w, r, name, value, maxAge)
	s.putCookie(w, r, other, "", -1)
}

// exchangeLoginCode 는 로그인 코드를 화면 세션으로 바꾼다. 옛 쿠키는 지운다(grantLoginCookie).
func (s *server) exchangeLoginCode(w http.ResponseWriter, r *http.Request, code, next string) {
	if s.st == nil {
		s.loginRefused(w, r, "이 서버는 계정 원장이 없어 로그인 코드를 못 바꾼다", next)
		return
	}
	got, err := s.st.ExchangeLoginCode(r.Context(), code, r.UserAgent(), s.now())
	if errors.Is(err, store.ErrLoginCodeRefused) {
		s.loginRefused(w, r, store.ErrLoginCodeRefused.Error(), next)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.grantLoginCookie(w, r, SessionCookieName, got.Cookie, int(auth.SessionTTL/time.Second))
	s.seeOther(w, r, next)
}

// refusedCrossSite 는 출처를 대조하고, 아니면 403 을 내고 **참**을 돌려준다.
//
// ★ 출처 대조를 **직접** 부르는 이유. withScreenWrite 는 게이트 사슬에서 withAuth
// 안쪽이라 이 두 경로(게이트 **앞**에서 갈라진다)에는 안 걸린다.
//
// ★ 헬퍼로 접은 이유. 로그인과 로그아웃이 문구 둘만 다른 같은 블록을 각자 갖고 있었다.
// 사본이 둘이면 한쪽만 고쳐지고, CSRF 방어가 한쪽만 열린 상태는 "잠겨 있다"고 믿게
// 만들어서 안 잠근 것보다 나쁘다(api.go 의 Fallback 주석이 세운 그 규율).
func (s *server) refusedCrossSite(w http.ResponseWriter, r *http.Request, what, guidance string) bool {
	v := JudgeScreenOrigin(r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"), r.Host)
	if v.OK {
		return false
	}
	s.writeError(w, r, Classified{
		Status: http.StatusForbidden, Code: "cross_site_write_refused",
		Message:  what + "의 출처가 이 화면이 아니다 — " + v.Reason,
		Guidance: guidance,
	})
	return true
}

// putCookie 는 이 요청의 전송에 맞는 쿠키를 굽는다. 지우는 것도 같은 자리다
// (값 없음 · MaxAge<0). Secure 판정이 두 자리에 살면 한쪽만 고쳐지고, 그러면
// 로그아웃이 로그인과 **다른 속성**의 쿠키를 내 브라우저가 원본을 안 지운다.
//
// ★ 쿠키를 싣는 응답은 캐시에 남기지 않는다(Cache-Control: no-store). 공유 캐시 · 프록시가 Set-Cookie 를 실은 303 을 담아 두면
// 다른 사람이 같은 응답(자격증명)을 받거나, 지우는 응답 대신 옛 응답이 재생된다. 부트스트랩 응답과 같은 규율이다.
func (s *server) putCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, LoginCookie(name, value, maxAge,
		auth.CookieSecure(s.opt.TLS, r.TLS != nil, r.Header.Get("X-Forwarded-Proto"))))
}

// handleLogout 은 화면 세션을 끝내고 쿠키를 지운다.
//
// ★ 세션 쿠키면 원장의 행을 지우고(다음 요청부터 401), 커밋 뒤 그 세션의 열린 스트림(/events)을 끊는다. 옛 쿠키(FD_TOKEN)는
// 이 브라우저의 쿠키만 지운다 - FD_TOKEN 자체는 유효한 채로 남는다(바꾸는 길은 FD_TOKEN 을 바꾸고 서버를 다시 띄우는 것).
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.refusedCrossSite(w, r, "로그아웃", "대시보드 화면의 버튼을 눌러라.") {
		return
	}
	if v := cookieValue(r, SessionCookieName); v != "" && s.st != nil {
		if k, ok := auth.KindOf(v); ok && k == auth.SessionToken {
			h := auth.Hash(v)
			if _, _, err := s.st.EndWebSession(r.Context(), h); err != nil {
				// 원장이 세션을 못 지웠는데 쿠키만 지우면 「로그아웃했다」고 믿게 된다 - 그 세션 값은 아직 통한다.
				s.fail(w, r, err)
				return
			}
			s.hub.Cut(CutWebSession(h))
		}
	}
	// MaxAge<0 이 삭제다. 값도 비운다 — 둘 중 하나만 하면 브라우저에 따라 남는다.
	s.putCookie(w, r, SessionCookieName, "", -1)
	s.putCookie(w, r, LoginCookieName, "", -1)
	s.seeOther(w, r, "/")
}

// loginRefused 는 거절을 폼으로 되돌린다.
//
// ★ 상태코드는 401 이다. 200 으로 내면 "로그인 실패"가 성공과 같은 코드로 보이고,
// /metrics 의 unauthorized 축과 어긋난다.
//
// ★ **시도한 토큰 값을 안 싣는다.** LoginView 에 그 필드가 없는 것이 그 규율의 자리다.
//
// ★ Action 을 여기서도 계산한다. 이 자리의 r.URL.Path 는 폼이 제출된 경로이므로 언제나
// 라우트 그대로인 `/login` 이고(다른 자리로 간 제출은 이 핸들러에 아예 안 닿는다 —
// 세션 이전 경로가 아니라 withAuth 가 먼저 401 을 낸다), JudgeLoginAction 이 거기서
// 깊이 0 을 내 `./login` 을 돌려준다. 즉 재시도 폼도 같은 자리를 가리킨다.
// 상수로 박지 않는 이유는 같은 계산이 두 벌이 되지 않게 하기 위해서다.
func (s *server) loginRefused(w http.ResponseWriter, r *http.Request, why, next string) {
	s.met.incUnauthorized()
	if s.opt.LoginScreen != nil && JudgeLoginScreen(r.Header.Get("Accept")) {
		s.opt.LoginScreen(w, r, LoginView{Error: why, Next: next, Action: JudgeLoginAction(r.URL.Path)})
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="flightdeck"`)
	s.writeError(w, r, Classified{
		Status: http.StatusUnauthorized, Code: "unauthorized",
		Message:  why,
		Guidance: UnauthorizedGuidance(s.opt.AuthMode),
	})
}
