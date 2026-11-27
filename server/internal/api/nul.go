package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/kweiza/flightdeck/internal/nulesc"
)

// 쓰기 길의 NUL 규칙 — 요청이 서버에 들어오는 두 입구(URL · JSON 본문)에 이관과 같은 규칙(nulesc)을 건다.
//
// ## 왜 입구인가
//
// Postgres text 는 NUL(U+0000)을 못 담아(22021) 그 글이 든 쓰기가 500 이 되고, SQLite 는 같은 글을 그대로 저장한다 - 엔진마다
// 다른 값 · 다른 결과다. 처리기마다 · 저장소 문장마다 덧대지 않고 요청이 값으로 풀리는 자리 하나에서 고친다: MCP · CLI · 훅 ·
// 보드가 모두 이 REST 를 타므로(화면 폼은 web 이 같은 nulesc.Fields 를 건다) 두 엔진이 같은 값을 받는다. 저장소 문장(store 의
// exec)에서 고치지 않는 까닭: 거기에는 원장에서 읽어 옮겨 적는 값(개정 이력의 옛 제목 · 본문, 되돌리기 사본)도 지나가 입력과
// 원장 값을 가를 수 없고, 어느 칸이 이름인지도 모른다.
//
// ## 규칙
//
//   - JSON 본문: 자유 글 칸(`nul:"text"`)의 NUL 은 `\x00` 표기로 바꿔 받고(이관의 nulesc.Escape 와 같은 바이트), 이름 칸의 NUL ·
//     키의 U+0000 · 짝 없는 서로게이트는 400 이다(nulesc.Input).
//   - URL: 경로 · 질의는 모두 이름(id · 프로젝트 · 검색어 같은 조회 열쇠)이라 NUL 이 있으면 400 이다(JudgeURLNUL).

// JudgeURLNUL 은 URL 의 경로 · 질의(이름 · 값)에 NUL 이 있는지 본다. 있으면 그 자리를 사람이 읽는 말로 낸다. 순수 함수다.
//
// 경로는 푼 경로(u.Path - 처리기가 PathValue 로 읽는 것)를 잰다. 질의는 이름 차례로 잰다(어느 자리를 먼저 말할지가 요청마다
// 같게).
func JudgeURLNUL(u *url.URL) (where string, bad bool) {
	if strings.IndexByte(u.Path, 0) >= 0 {
		return "경로", true
	}
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if strings.IndexByte(k, 0) >= 0 {
			return "질의 이름", true
		}
		for _, v := range q[k] {
			if strings.IndexByte(v, 0) >= 0 {
				return fmt.Sprintf("질의 %q", k), true
			}
		}
	}
	return "", false
}

// withNULGate 는 URL 에 NUL 이 든 요청을 처리기 앞에서 400 으로 거절한다.
//
// chain 에서 screenWrite 뒤 · idempotency 앞이다: 거절이 멱등 표에 자리를 잡지 않고, 화면 쓰기의 출처 대조(403)가 먼저다.
func (s *server) withNULGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if where, bad := JudgeURLNUL(r.URL); bad {
			s.writeError(w, r, nulRefused("URL 의 "+where))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// acceptNUL 은 푼 문서 doc 와 값 dst 에 NUL 규칙을 건다. 거절이면 응답까지 내고 false 다.
//
// ★ 바꿔 받은 것은 응답에 표지를 달지 않고 서버 로그(Info - request_id · 경로 · 바꾼 칸의 자리, 글 내용은 안 싣는다)에 남긴다.
// 까닭: 바꾼 글은 저장된 그대로 이후의 조회 · 보드 · 이벤트에 `\x00` 네 글자로 보여 숨은 값이 없고(풀기가 짝 없는 서로게이트를
// U+FFFD 로 바꾸는 것과 달리 되돌릴 수 있다 - nulesc.Escape), 응답 본문에 표지 칸을 두면 처리기마다 결과 타입이 갈라지며
// (writeJSON 의 한 벌 규율), 응답 머리에 두면 멱등 재생(withIdempotency 는 본문만 담는다)이 그것을 잃는다. 어느 글이 바뀌었는지의
// 기록(경로 · 칸 자리)은 이관의 기록 파일 자리를 이 로그 줄이 맡는다.
func (s *server) acceptNUL(w http.ResponseWriter, r *http.Request, doc []byte, dst any) bool {
	rewritten, err := nulesc.Input(string(doc), dst)
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	if len(rewritten) > 0 {
		s.log.InfoContext(r.Context(), "요청 글의 NUL 을 \\x00 표기로 바꿔 받았다",
			"route", s.resolveRoute(r), "path", clip(r.URL.Path, 200),
			"request_id", infoFrom(r.Context()).id(), "fields", rewritten)
	}
	return true
}

// classifyNUL 은 NUL 규칙이 거절한 글 하나를 응답으로 옮긴다(ClassifyError 가 부른다).
func classifyNUL(r *nulesc.Refusal) Classified {
	switch {
	case errors.Is(r, nulesc.ErrSurrogate):
		return badRequest("lone_surrogate",
			fmt.Sprintf("요청 글 %q 에 짝 없는 서로게이트(홀로 선 \\ud800-\\udfff)가 있다", r.Pointer),
			"그 글을 올바른 UTF-8 로 고쳐 다시 보내라 - 받으면 U+FFFD 로 바뀌어 다른 글과 같아진다.")
	case errors.Is(r, nulesc.ErrKeyNUL), errors.Is(r, nulesc.ErrNUL):
		return nulRefused(fmt.Sprintf("요청 글 %q", r.Pointer))
	}
	return badRequest("bad_json", fmt.Sprintf("요청 글 %q 를 JSON 으로 읽을 수 없다", r.Pointer),
		"필드 이름과 타입을 확인해라.")
}

// nulRefused 는 이름 자리의 NUL 을 거절하는 응답이다. 자유 글의 NUL 은 거절이 아니라 바꿔 받으므로 여기 오는 것은 바꾸면 다른
// 것을 가리키게 되는 글뿐이다 - 그 까닭을 안내에 싣는다.
func nulRefused(where string) Classified {
	return badRequest("nul_refused", where+" 에 NUL(U+0000)이 있다",
		"그 자리(식별자 · 종류 · 경로 · 검색어 · JSON 키)에서 NUL 을 빼고 다시 보내라 - 자유 글(제목 · 본문 · 사유 · 꼬리표)의 "+
			"NUL 은 \\x00 표기로 바꿔 받지만, 이름을 바꾸면 원래 \\x00 네 글자인 다른 이름과 같아져 다른 것을 가리킨다.")
}
