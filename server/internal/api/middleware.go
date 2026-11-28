package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/kweiza/flightdeck/internal/auth"
	"github.com/kweiza/flightdeck/internal/judge"
	"github.com/kweiza/flightdeck/internal/store"
)

// 게이트와 관측.
//
// ★ 이 파일의 규율 하나: **401·429 는 액세스 로그에 줄을 내지 않는다.**
// 인증 실패와 한도 초과는 정의상 반복해서 오는 요청이라 건별로 남기면
// 초과 트래픽이 그대로 로그 증폭이 된다. 그 축의 정본은 /metrics 의
// flightdeck_unauthorized_total · flightdeck_rate_limited_total 이다.

type ctxKey int

const infoKey ctxKey = 1

// reqInfo 는 요청 1건의 상관 정보다. 액세스 로그와 오류 본문이 같은 값을 쓴다.
//
// SessionID 는 핸들러가 본문을 읽은 **뒤에** 채운다 — 미들웨어는 본문의 스키마를
// 모르기 때문이다. 이 값이 없으면 "어느 세션이 이 500 을 맞았나"에 답할 자리가 없다.
type reqInfo struct {
	mu        sync.Mutex
	requestID string
	sessionID string
}

func (i *reqInfo) setSession(id string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	i.sessionID = clip(id, 64)
	i.mu.Unlock()
}

// id 는 요청 상관키다. 사슬 밖(nil)이면 빈 값이다.
func (i *reqInfo) id() string {
	if i == nil {
		return ""
	}
	return i.requestID
}

func (i *reqInfo) session() string {
	if i == nil {
		return ""
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.sessionID
}

func infoFrom(ctx context.Context) *reqInfo {
	v, _ := ctx.Value(infoKey).(*reqInfo)
	return v
}

// RoutePattern 은 로그·지표에 실을 라우트 이름을 만든다. 순수 함수다.
//
// ★ **실제 경로를 절대 쓰지 않는다.** 경로에는 항목 id·세션 id·스냅숏 키가 들어 있어
// 라벨에 넣으면 시계열이 요청 수만큼 늘고, 동시에 인증 없이 열려 있는 /metrics 로
// 도메인 좌표가 새어 나간다. 못 맞춘 요청은 경로 대신 고정 문자열로 접는다.
func RoutePattern(pattern, method string) string {
	p := strings.TrimSpace(pattern)
	if p == "" || p == "/" {
		m := strings.ToUpper(strings.TrimSpace(method))
		if m == "" {
			return "unmatched"
		}
		return m + " unmatched"
	}
	return p
}

// withRequestID 는 상관키를 만들고 응답 헤더에 되돌려 준다.
//
// 클라이언트가 준 X-Request-Id 가 있으면 **그것을 잇는다** — 소비자 신고와
// 서버 로그를 잇는 열쇠는 한쪽이 끊으면 그 자리에서 죽는다.
func (s *server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := clip(strings.TrimSpace(r.Header.Get("X-Request-Id")), 64)
		if id == "" {
			id = store.NewID()
		}
		info := &reqInfo{requestID: id}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), infoKey, info)))
	})
}

// withRateLimit 은 원격 주소당 한도를 건다. **한도는 기본으로 꺼져 있다.**
func (s *server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.lim.allow(hostOf(r.RemoteAddr)) {
			next.ServeHTTP(w, r)
			return
		}
		s.met.incRateLimited()
		// 로그 줄 없음 — 초과 트래픽이 그대로 로그 증폭이 되기 때문이다.
		s.writeError(w, r, Classified{
			Status: http.StatusTooManyRequests, Code: "rate_limited",
			Message:  "요청 한도를 넘었다",
			Guidance: "잠시 뒤에 다시 불러라 — 이 축은 /metrics 의 flightdeck_rate_limited_total 로만 센다.",
		})
	})
}

// withTransport 는 전송 정책(FD_TLS=proxy)을 건다: 신뢰 프록시 밖에서 곧장 온 연결은 TLS 를 건너뛴 평문이라 자격증명을 보기
// 전에 거절한다. 판정은 auth.JudgeTransport 다. /healthz 도 예외가 아니다 - 컨테이너 헬스체크는 안에서 루프백으로 친다.
func (s *server) withTransport(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := auth.JudgeTransport(r.RemoteAddr, s.opt.TLS); !v.OK {
			s.writeError(w, r, Classified{
				Status: http.StatusForbidden, Code: "transport_refused",
				Message:  v.Reason,
				Guidance: "이 서버는 TLS 프록시 뒤에서만 받는다(FD_TLS=proxy) - 프록시의 https 주소로 붙어라.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAuth 는 인증 게이트다. 판정은 Authenticator(auth.Gate)가 하고, 통과한 주체를 ctx 에 싣는다(auth.FromContext).
func (s *server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /healthz 는 게이트 앞에 둔다. 배너 훅이 "서버가 살아 있나 · 인증이 어떤 단계인가"를
		// 묻는 유일한 창인데, 그것을 인증 뒤에 두면 토큰 오설정 시 아무것도 알 수 없다.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		// 로그인·로그아웃은 게이트 앞이다. 판정은 핸들러가 스스로 한다(login.go).
		if JudgePreSessionPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		d := s.gate.Authenticate(r.Context(), authRequest(r))
		if d.Unavailable {
			// ★ 401 로 접지 않는다. 원장 장애가 "토큰이 틀렸다"로 읽히면 사람은 멀쩡한 토큰을 갈아 치운다.
			s.log.WarnContext(r.Context(), "인증 판정 실패 - 원장에 붙지 못했다", "reason", d.Reason)
			s.writeError(w, r, Classified{
				Status: http.StatusServiceUnavailable, Code: "auth_unavailable",
				Message:  d.Reason,
				Guidance: "잠시 뒤에 다시 불러라 - 원장 상태는 /healthz 의 db_ok 가 낸다.",
			})
			return
		}
		if !d.OK {
			s.met.incUnauthorized()
			// 로그 줄 없음(위 규율). 사유는 응답에만 싣는다. 사유에는 자격증명 값이 없다(auth.Decision).
			//
			// ★ HTML 을 읽는 소비자에게는 폼을 낸다. **상태코드는 401 그대로다** —
			// 리다이렉트로 덮으면 인증 실패가 지표에서 사라지고, /metrics 의
			// unauthorized 카운터가 "아무도 막히지 않았다"고 거짓말한다.
			// ★ Action 은 **여기서** 계산한다. 이 401 은 뿌리가 아닌 경로에서도 뜨는데
			// (JudgeLoginScreen 이 메서드도 경로도 안 본다) 폼 action 은 상대경로라,
			// 문서 URL 의 깊이만큼 거슬러 올라가지 않으면 /actions/login 처럼 없는 자리를
			// 가리키고 그 자리도 다시 401 이다 — 무한 폼이다.
			//
			// ★ Next 는 JudgeNext 가 아니라 JudgeNextFrom 이다. 이 요청이 GET 이 아니면
			// 그 URI 로 되돌아갈 수 없다 — 로그인 성공은 303 이고 303 은 GET 으로 재생된다.
			if s.opt.LoginScreen != nil && JudgeLoginScreen(r.Header.Get("Accept")) {
				s.opt.LoginScreen(w, r, LoginView{
					Error:  d.Reason,
					Next:   JudgeNextFrom(r.Method, r.URL.RequestURI()),
					Action: JudgeLoginAction(r.URL.Path),
				})
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="flightdeck"`)
			s.writeError(w, r, Classified{
				Status: http.StatusUnauthorized, Code: "unauthorized",
				Message:  d.Reason,
				Guidance: UnauthorizedGuidance(s.opt.AuthMode),
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), d.Principal)))
	})
}

// statusRecorder 는 상태코드와 바이트 수를 본다.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (rec *statusRecorder) WriteHeader(code int) {
	if rec.wrote {
		return
	}
	rec.status, rec.wrote = code, true
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if !rec.wrote {
		rec.WriteHeader(http.StatusOK)
	}
	return rec.ResponseWriter.Write(b)
}

// Unwrap 은 http.NewResponseController 가 Flush 를 찾아가게 한다.
// 없으면 SSE 가 이 래퍼 뒤에서 버퍼링돼 하트비트가 안 나간다.
func (rec *statusRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// withAccessLog 는 **게이트를 통과한 요청 1건당 1줄**을 남기고 지표를 센다.
//
// 원격 주소·포트·UA 는 **일부러 안 싣는다.** 대가는 커넥션(4-tuple)과 이을 열쇠가
// 로그에 없다는 것이다 — 미매칭 404 가 브라우저인지 탐침인지 못 가른 조사가 실제로
// 있었다(drain-ms 측정, 판단 01KZA7Y76G). 그 수요를 원장에서 재집계했더니(2026-08-07)
// 그 한 번뿐이고 이후 0건이라, 없는 수요에 로그 부피·사생활 축을 지불하지 않는다.
// 두 번째 수요가 오면 원격 포트부터 검토하라 — IP 는 이미 게이트 판정에 쓰여
// 카디널리티가 낮고, 포트가 4-tuple 을 완성한다(fd-access-log-cannot-join-connections).
func (s *server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			route := s.resolveRoute(r)
			dur := s.now().Sub(start).Seconds()
			s.met.observe(route, rec.status, dur)
			info := infoFrom(r.Context())
			s.log.InfoContext(r.Context(), "request served",
				"route", route,
				"status", rec.status,
				"duration", dur,
				"request_id", info.id(),
				"session_id", info.session(),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

// bodyRecorder 는 응답 본문을 함께 모은다(멱등 재생용).
type bodyRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	wrote  bool
}

func (rec *bodyRecorder) WriteHeader(code int) {
	if rec.wrote {
		return
	}
	rec.status, rec.wrote = code, true
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *bodyRecorder) Write(b []byte) (int, error) {
	if !rec.wrote {
		rec.WriteHeader(http.StatusOK)
	}
	rec.body.Write(b)
	return rec.ResponseWriter.Write(b)
}

func (rec *bodyRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// withIdempotency 는 쓰기 재시도를 같은 결과로 접는다.
//
// 읽기는 그대로 지나간다. 5xx 는 저장하지 않는다 — 일시 장애를 영구 응답으로
// 굳히면 하류가 복구된 뒤에도 같은 실패만 돌려주게 된다.
func (s *server) withIdempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWrite(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		// ★ 세션 이전 경로는 멱등 표에 **아예 안 들어간다.** 키 요구만 면제하면 key 가 ""
		// 인 채로 begin() 에 들어가고, 그러면 서로 무관한 로그인 시도들이 s.entries[""]
		// 한 슬롯을 공유한다 — 틀린 토큰의 401 이 캐시된 뒤 맞는 토큰의 재시도가
		// fingerprint 불일치로 409 가 된다. 폼에 키를 심는 우회를 기각한 것과 같은 실패다.
		//
		// ★ 비밀을 내는 경로(부트스트랩)도 안 들어간다. 멱등 기록은 응답 본문을 원장에 남긴다 - 한 번만 보인다던 로그인 코드가
		// 거기 평문으로 산다(JudgeSecretResponse). 그 경로는 본문 상한을 손으로 건다.
		if JudgePreSessionPath(r.URL.Path) || JudgeSecretResponse(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		v := JudgeIdempotencyKey(r.Method, key)
		if v.Bad != nil {
			s.writeError(w, r, classifyInput(v.Bad, "Idempotency-Key 헤더"))
			return
		}
		if !v.OK {
			s.writeError(w, r, badRequest("idempotency_key_required", v.Reason,
				"Idempotency-Key: <session>:<seq> 를 실어라 — 훅은 타임아웃으로 끊고 다시 부르는 것이 정상 동작이라, "+
					"이 키가 없으면 같은 신호·항목이 조용히 두 번 들어간다."))
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opt.MaxBodyBytes))
		if err != nil {
			s.writeError(w, r, Classified{
				Status: http.StatusRequestEntityTooLarge, Code: "body_too_large",
				Message: "요청 본문이 상한을 넘었다",
			})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		fp := Fingerprint(r.Method, r.URL.Path, body)
		// 이 라우트가 재기동을 넘겨야 하는가. 판정은 순수 함수가 하고 사유를 함께 낸다.
		pv := JudgePersistIdempotency(r.Method, r.URL.Path)
		entry, replay, m := s.idem.begin(r.Context(), key, fp, pv.Persist)
		if m != nil && m.Conflict {
			s.met.incConflict()
			s.writeError(w, r, Classified{
				Status: http.StatusConflict, Code: "idempotency_conflict",
				Message:  m.Reason,
				Guidance: "키는 요청 1건에 하나다 — <session>:<seq> 의 seq 를 올려라.",
			})
			return
		}
		if entry == nil {
			// 앞선 같은 키의 요청을 기다리다 끊겼다. 재시도가 맞는 상황이라 503 으로 낸다.
			s.writeError(w, r, Classified{
				Status: http.StatusServiceUnavailable, Code: "idempotency_wait_canceled",
				Message: "같은 키의 앞선 요청을 기다리다 취소됐다 — 다시 불러라",
			})
			return
		}
		if replay {
			s.met.incReplay()
			ct := entry.ctype
			if ct == "" {
				ct = "application/json; charset=utf-8"
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(entry.status)
			if _, err := w.Write(entry.body); err != nil {
				s.log.WarnContext(r.Context(), "멱등 재생 응답 쓰기 실패", "error", err.Error())
			}
			return
		}

		rec := &bodyRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			// ★ 요청 컨텍스트의 취소를 떼고 저장한다. 클라이언트가 응답을 받자마자 끊으면
			//   그 컨텍스트가 죽는데, 그때 저장을 건너뛰면 **끊는 클라이언트일수록**
			//   멱등 기억이 안 남는다 — 재시도하는 쪽이 정확히 그 클라이언트다.
			s.idem.finish(context.WithoutCancel(r.Context()), key, entry,
				rec.status, rec.Header().Get("Content-Type"), rec.body.Bytes(), pv.Persist)
		}()
		next.ServeHTTP(rec, r)
	})
}

// withRecover 는 핸들러 패닉을 500 으로 옮기고 스택을 남긴다.
//
// 다시 던지지 않는다 — 다시 던지면 net/http 가 커넥션을 닫아 버려
// 소비자가 상태코드조차 못 받고, 그러면 "무엇이 터졌나"를 물을 좌표가 사라진다.
// 대신 이 줄이 유일한 원천이 되므로 **스택을 반드시 싣는다**.
func (s *server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rv := recover()
			if rv == nil {
				return
			}
			s.met.incPanic()
			info := infoFrom(r.Context())
			s.log.ErrorContext(r.Context(), "request panicked",
				"route", s.resolveRoute(r),
				"request_id", info.id(),
				"session_id", info.session(),
				"error", clip(toString(rv), 600),
				"stack", clip(string(debug.Stack()), 4000),
			)
			s.writeError(w, r, Classified{
				Status: http.StatusInternalServerError, Code: "panic",
				Message:  "서버 내부 오류다. 원인 전문은 서버 로그에 있다",
				Guidance: "이 응답의 request_id 로 로그를 찾아라.",
				Internal: true,
			})
		}()
		next.ServeHTTP(w, r)
	})
}

// withRelativeLocation 은 Location 헤더의 **경로만 있는 절대경로**를 상대 참조로 바꾼다.
//
// ★ 이 자리가 필요한 이유는 net/http.ServeMux 자신이다. 그 mux 는 경로를 정규화해야 할 때
// (`//` · `/a//b` · `/a/../` 같은 요청) RedirectHandler 로 리다이렉트를 내는데, 그 Location 이
// 경로만 있는 절대경로다 — 접두를 벗기는 프록시 뒤에서 접두 **밖**으로 떨어진다.
// 저장소 코드가 아니라 표준 라이브러리 안이라 `grep 'http.Redirect('` 로는 안 보인다.
//
// ★ **상태 코드를 여기 적지 않는다.** 앞선 판은 `307` 이라 적었는데 Go 1.25.6 은 `301` 을
// 낸다(2026-08-31 확인). 그 값은 stdlib 의 것이고 이 미들웨어는 **상태와 무관하게** Location 만
// 본다 — 적어 두면 낡고, 낡은 주석은 시험이 그 값을 못박는 근거가 된다(실제로 그렇게 됐다).
//
// ★ **이미 상대인 값은 안 건드린다.** 이 서버의 핸들러(seeOther)는 `./` · `../` 를 내고
// 그것들은 `/` 로 시작하지 않는다. 그 값을 다시 상대화하면 RelativeTo 의 to 방어(`..` 포함)에
// 걸려 `./` 로 접히고, 되돌아갈 자리를 잃는다.
func (s *server) withRelativeLocation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &relativeLocationRecorder{ResponseWriter: w, from: r.URL.Path}
		next.ServeHTTP(rw, r)
	})
}

// relativeLocationRecorder 는 WriteHeader 직전에 Location 을 상대화한다. statusRecorder 와
// 같은 모양이다 — WriteHeader 를 가로채고, Write 의 암묵 200 을 같은 자리에서 처리하고,
// Unwrap 을 낸다.
//
// ★ **Unwrap 이 없으면 SSE 가 죽는다.** /events 는 http.NewResponseController(w) 로 Flush 를
// 찾는데(handlers_meta.go), 이 래퍼가 Unwrap 을 안 주면 그 컨트롤러가 Flush 를 못 찾아
// 하트비트가 안 나간다 — statusRecorder 의 Unwrap 주석이 그 이유를 이미 적어 뒀다.
//
// ★ **statusRecorder 를 고쳐서 겸용하지 않는다.** 그것은 "상태코드와 바이트 수를 본다"는
// 단일 책임이다. 여기에 Location 을 얹으면 책임이 둘이 되고, 한쪽만 필요한 자리(액세스
// 로그)가 다른 쪽(Location 재작성)의 존재를 몰라도 되는 이유가 사라진다.
type relativeLocationRecorder struct {
	http.ResponseWriter
	from  string // 이 요청의 원 경로 — judge.RelativeTo 의 from(mux 가 정규화하기 전 값)
	wrote bool
}

func (rw *relativeLocationRecorder) WriteHeader(code int) {
	if rw.wrote {
		return
	}
	rw.wrote = true
	// ★ "/" 로 시작하는 값만 상대화한다. RelativeTo 자신이 "//" 스킴 상대와 ".." 를
	// 이미 "./" 로 접으므로, 여기서 가를 것은 "이미 상대인가" 하나뿐이다.
	if loc := rw.Header().Get("Location"); strings.HasPrefix(loc, "/") {
		rw.Header().Set("Location", judge.RelativeTo(rw.from, loc))
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *relativeLocationRecorder) Write(b []byte) (int, error) {
	if !rw.wrote {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// Unwrap 은 http.NewResponseController 가 Flush 를 찾아가게 한다.
// 없으면 SSE 가 이 래퍼 뒤에서 버퍼링돼 하트비트가 안 나간다.
func (rw *relativeLocationRecorder) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case error:
		return t.Error()
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "직렬화할 수 없는 패닉 값"
		}
		return string(b)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 한도 — 원격 주소당 토큰 버킷
// ─────────────────────────────────────────────────────────────────────────────

// Refill 은 경과 시간만큼 토큰을 채운다. 순수 함수다.
// 상한(burst)을 넘지 않는다 — 안 막으면 오래 쉰 클라이언트가 무한 버스트를 얻는다.
func Refill(tokens float64, last, now time.Time, ratePerSec, burst float64) float64 {
	if now.After(last) {
		tokens += now.Sub(last).Seconds() * ratePerSec
	}
	if tokens > burst {
		tokens = burst
	}
	return tokens
}

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // 초당
	burst   float64
	now     func() time.Time
}

func newLimiter(perMinute, burst int, now func() time.Time) *limiter {
	l := &limiter{buckets: map[string]*bucket{}, now: now}
	if perMinute > 0 {
		l.rate = float64(perMinute) / 60.0
		l.burst = float64(perMinute)
		if burst > 0 {
			l.burst = float64(burst)
		}
	}
	return l
}

// allow 는 요청 1건을 통과시킬지 본다. rate 가 0 이면 **한도가 없다**.
func (l *limiter) allow(key string) bool {
	if l.rate <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = Refill(b.tokens, b.last, now, l.rate, l.burst)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	if len(l.buckets) > 10000 {
		l.sweepLocked(now)
	}
	return true
}

// sweepLocked 는 다 찬 버킷을 걷어낸다(가진 상태가 없는 것과 같다).
func (l *limiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if Refill(b.tokens, b.last, now, l.rate, l.burst) >= l.burst {
			delete(l.buckets, k)
		}
	}
}

func hostOf(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}
