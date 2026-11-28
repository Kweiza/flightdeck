package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/kweiza/flightdeck/internal/auth"
	"github.com/kweiza/flightdeck/internal/judge"
)

// 인증 게이트의 표면 쪽 판정 — 경로의 뜻(화면인가 · 세션 이전인가 · 비밀을 내는가)과 쿠키 이름. 자격증명 → 주체의 판정은
// internal/auth 의 Authenticator 가 한다(설계 「REST `/api/v1/` — 정본」).
//
// ★ 이 파일의 판정은 전부 순수 함수다. 미들웨어 본문에 조건을 흩어 놓으면
// 시험이 그 조건의 **사본**을 단정하게 되고, 그러면 변이가 조용히 샌다.
//
// ★ 루프백 면제는 없다. 앞선 판은 루프백에서 온 요청을 토큰 없이 통과시켰는데, 같은 호스트의 리버스 프록시를 거친 요청 전부가
// 루프백으로 보여 그 면제를 받았고, 컨테이너에서는 반대로 아무도 못 받았다. 이제 루프백도 자격증명이 필요하다.

// authRequest 는 요청에서 판정에 필요한 사실을 뽑는다. 쿠키는 값만 읽는다 - 인정할지는 auth 가 경로 판정(ScreenPath)으로 정한다.
func authRequest(r *http.Request) auth.Request {
	return auth.Request{
		Authorization: r.Header.Get("Authorization"),
		SessionCookie: cookieValue(r, SessionCookieName),
		SharedCookie:  cookieValue(r, LoginCookieName),
		ScreenPath:    JudgeScreenPath(r.URL.Path),
		BootstrapPath: r.URL.Path == BootstrapPath,
	}
}

func cookieValue(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

// BootstrapPath 는 첫 관리자를 만드는 경로다 - 계정 단계에서 FD_TOKEN 이 통하는 유일한 자리다(auth.Request.BootstrapPath).
const BootstrapPath = "/admin/bootstrap"

// JudgeSecretResponse 는 이 경로의 응답이 비밀(로그인 코드 원값)을 싣는가다. 순수 함수다. 참이면 멱등 표에 안 들어간다 - 멱등
// 기록은 응답 본문을 원장(idempotency 표)에 남기므로, 거기 실리면 한 번만 보인다던 코드가 원장에 평문으로 산다.
func JudgeSecretResponse(path string) bool { return path == BootstrapPath }

// JudgeScreenPath 는 이 경로가 화면인가다 — **쿠키를 인정하는 유일한 조건**이다. 순수 함수다.
//
// ★ 이 함수가 /api/v1 에 대해 거짓을 내는 것이 화면 로그인 설계 전체의 안전을 지탱한다.
// REST 쓰기는 withScreenWrite 의 출처 대조를 **안 탄다**(JudgeScreenWrite 가 화면 경로만
// Screen=true 로 본다). 그래서 여기서 REST 를 참으로 내는 순간 REST 쓰기 전체의 CSRF 방어가
// 쿠키의 SameSite 하나로 줄어든다 — 그 방어는 브라우저 구현에 전적으로 기대는 것이다.
//
// ★ /events 가 참인 이유. 화면이 무는 짧은 별칭이고(api.go 의 그 라우트), EventSource 는
// 임의 헤더를 못 싣는다. 이 경로가 쿠키를 안 받으면 화면은 멀쩡히 뜨는데 영원히 안 갱신된다.
// /api/v1/events 는 REST 쪽이라 거짓이다.
func JudgeScreenPath(path string) bool {
	switch path {
	case "/", "/events":
		return true
	}
	return strings.HasPrefix(path, screenActionPrefix)
}

// screenActionPrefix 는 대시보드 쓰기 라우트의 뿌리다. web 패키지의 라우트와 같은 값이며,
// 접두어로 보는 이유는 api 가 web 패키지를 import 하지 않기 때문이다
// (api → web 은 의존 방향이 거꾸로다. Fallback 은 인터페이스로만 온다).
//
// ★ **상수가 하나다.** 앞선 판은 JudgeScreenPath 와 JudgeScreenWrite 가 같은 사실을
// 리터럴로 따로 적었다. 그 둘이 갈리면 "쿠키를 인정하는 경로"와 "출처를 대조하는 경로"가
// 어긋나고, 그 틈은 **쿠키는 통하는데 CSRF 대조는 안 하는 경로**라 정확히 이 설계가
// 막으려던 구멍이다. 한 자리에 두면 갈릴 수가 없다.
const screenActionPrefix = "/actions/"

// JudgeLoginScreen 은 이 401 에 HTML 폼을 낼 것인가다. 순수 함수다.
//
// ★ **메서드를 안 본다.** 걸러야 하는 것은 "쓰기"가 아니라 **HTML 을 못 읽는 소비자**이고,
// 그 축은 Accept 하나로 갈린다 — fd CLI 는 application/json, EventSource 는
// text/event-stream 을 보낸다. 메서드를 조건에 넣으면 쿠키 없이 화면 폼을 제출한 사람이
// 브라우저 앞에 앉아 JSON 401 을 들여다보게 된다. 그 사람은 폼을 읽을 수 있다.
//
// ★ */* 를 거짓으로 두는 이유. curl 의 기본값이라 참으로 두면 CLI 소비자가 HTML 을 받는다.
// 브라우저는 언제나 text/html 을 명시한다.
func JudgeLoginScreen(accept string) bool {
	return strings.Contains(strings.ToLower(accept), "text/html")
}

// LoginCookieName 은 옛 화면 로그인 쿠키(FD_TOKEN 을 담는다)의 이름이다. 전환 단계에서만 인정한다.
//
// ★ 이름을 내보내는 이유: 굽는 자리(handleLogin)와 읽는 자리(withAuth)와 지우는
// 자리(handleLogout)가 셋인데, 리터럴로 두면 오타 하나가 "로그인은 되는데 다음 요청에서
// 다시 폼"이라는 모양으로 나타난다 — 원인이 이름이라는 것이 그 증상에서 안 보인다.
const LoginCookieName = "fd_token"

// SessionCookieName 은 화면 세션 쿠키(auth.SessionToken)의 이름이다. 로그인 코드를 바꿔 굽는다(login.go).
const SessionCookieName = "fd_session"

// JudgePreSessionPath 는 이 경로가 **세션이 생기기 전**인가다. 순수 함수다.
//
// ★ 두 게이트가 이것을 함께 본다 — 인증(withAuth)과 멱등(withIdempotency). 그 둘이 같은
// 경로 집합을 면제하는 것은 우연이 아니라 **같은 한 가지 이유**다: 세션이 없으니 대조할
// 자격증명도 없고 `<session>:<seq>` 형식의 멱등 키도 만들 수 없다.
//
// ★ 로그인 경로가 인증 게이트 뒤에 있으면 **로그인하려면 이미 로그인돼 있어야 한다.**
//
// ★ **메서드를 안 본다.** GET /login 도 세션 이전이다.
//
// ★ /healthz 는 여기 **없다.** 그것은 읽기 하나이고 멱등과 무관하다 - 인증 게이트만 그 경로를 따로 앞에서 보낸다.
func JudgePreSessionPath(path string) bool {
	return path == "/login" || path == "/logout"
}

// JudgeLoginAction 은 이 경로에서 뜬 폼의 action 이 가리켜야 할 **상대경로**다. 순수 함수다.
//
// ★ 절대경로(`/login`)로 두면 리버스 프록시의 경로 접두 뒤에서 브라우저가 원점의 /login 을
// 찾아가고 프록시는 그 경로를 모른다. 그런데 상대경로는 **문서 URL 의 깊이**에 붙으므로
// 뿌리가 아닌 자리에서 뜬 폼은 /actions/login 같은 없는 곳으로 간다. 그 자리도 세션 이전
// 경로가 아니라 다시 401 이 나고, 사람은 토큰을 정확히 쳐도 같은 폼을 무한히 다시 본다.
// 401 폼은 POST /actions/* 에서도 뜨므로(JudgeLoginScreen 이 메서드를 안 본다) 이것은
// 가설이 아니라 그 설계가 명시로 든 시나리오다.
//
// ★ **깊이 셈은 여기 없다.** judge.RelativeTo 한 벌이 그것을 진다 — 같은 셈이 폼 action 과
// 리다이렉트 Location 양쪽에 필요한데, 두 벌이 되면 반드시 표류하고 그 표류의 증상이
// 바로 위 문단의 무한 폼이다. 이 함수가 남아 있는 이유는 이름이 의도를 말하고
// `"/login"` 이 한 자리에 있기 때문이다 — 없애면 그 리터럴이 호출자 둘로 흩어진다.
func JudgeLoginAction(path string) string {
	// 라우트 등록(api.go 의 "POST /login")의 경로다.
	return judge.RelativeTo(path, "/login")
}

// JudgeNextFrom 은 **401 을 맞은 요청 자체**를 로그인 뒤 돌아갈 자리로 쓸 수 있는지 본다.
// 순수 함수다.
//
// ★ 로그인 성공은 303 이고 303 은 **언제나 GET 으로 재생된다.** 그래서 GET 이 아닌 요청의
// URI 를 그대로 실으면 토큰이 맞아도 405/404 에 착지한다 — POST 전용인 /actions/* 가
// 정확히 그 자리다. 로그인은 됐는데 화면이 깨진 것처럼 보이고, 원인이 Next 라는 것이
// 그 증상에서 안 보인다. 그래서 GET 이 아니면 뿌리로 접는다.
//
// ★ HEAD 도 접는다. 303 이 재생하는 것은 GET 하나뿐이라 "같은 요청으로 되돌아간다"가
// 참인 메서드도 GET 하나다.
//
// ★ 값 자체의 안전(오픈 리다이렉트)은 여전히 JudgeNext 가 본다. 이 함수는 그 앞에
// **메서드 축 하나**를 더할 뿐이고, 두 축을 한 함수로 접지 않는 이유는 폼이 제출한 next
// 를 검증하는 자리(handleLogin)에는 메서드 축이 없기 때문이다 — 거기는 언제나 POST 다.
func JudgeNextFrom(method, requestURI string) string {
	if !strings.EqualFold(strings.TrimSpace(method), "GET") {
		return "/"
	}
	return JudgeNext(requestURI)
}

// JudgeNext 는 로그인 뒤 돌아갈 자리를 고른다. 순수 함수다.
//
// ★ **이 서버 안의 경로만 남긴다.** 스킴이 있거나 // 로 시작하면 브라우저가 다른 호스트로
// 나가고, 그 순간 이 로그인 폼이 오픈 리다이렉트가 된다 — 공격자가 만든 주소로 사람을
// 보내면서 출발지가 이 대시보드였다는 사실을 이용한다.
//
// ★ 못 읽은 것은 통과시키지 않는다. 파싱 실패도 "/" 로 접는다.
func JudgeNext(next string) string {
	next = strings.TrimSpace(next)
	// 역슬래시는 일부 브라우저가 / 로 정규화한다. \\evil.com 이 //evil.com 이 되는 길을 막는다.
	if next == "" || !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") || strings.Contains(next, `\`) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return u.RequestURI()
}
