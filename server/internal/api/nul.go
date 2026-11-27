package api

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/kweiza/flightdeck/internal/nulesc"
	"github.com/kweiza/flightdeck/internal/store"
)

// 쓰기 길의 입력 규칙 - 요청이 서버에 들어오는 두 입구(URL · JSON 본문)에서 **거절만** 한다.
//
// ## 나눈 몫
//
// Postgres text 는 NUL(U+0000)과 깨진 UTF-8 을 못 담아(22021) 그 글이 든 쓰기 · 조회가 500 이 되고, SQLite 는 같은 글을 그대로
// 다룬다. 자유 글의 NUL 을 이관과 같은 `\x00` 표기로 옮기는 것은 입력이 아니라 원장에 담기는 글 하나의 일이다 - store 의 원장
// 칸 관문(store/cellgate.go)이 업무 쓰기 문장마다 이관 카탈로그의 칸 규칙(pg.Cell)을 건다. 그래야 여러 입력을 합친 칸 · 자른 칸이
// 이관과 같은 바이트가 되고, 요청을 안 푸는 쓰기(fd import)도 같은 규칙을 지난다. 입력은 받은 바이트 그대로 처리기로 간다 - 길이
// 재기 · 비교는 받은 원문으로 한다.
//
// 입구가 거절하는 것은 원장에 닿기 전에 흔적이 사라지거나 조회 열쇠로 쓰여 500 이 되는 글이다:
//
//   - URL: 경로 · 질의는 모두 이름(id · 프로젝트 · 검색어 같은 조회 열쇠)이다 - NUL 이나 깨진 UTF-8(`%FF`)이 있으면 400(JudgeURL).
//   - JSON 본문: 이름 칸(자유 글 표지 `nul:"text"` 가 없는 칸)의 NUL, 키의 U+0000, 짝 없는 서로게이트, 깨진 UTF-8 은 400 이다
//     (nulesc.Input - 풀기가 U+FFFD 로 바꿔 흔적을 지우는 것은 푼 바이트 그대로 잰다).
//   - MCP 도구 인자는 REST 로 넘기기 전에 mcpsrv 가 같은 nulesc.CheckDoc 으로 잰다(풀기가 지우는 것을 REST 가 다시 볼 수 없다).
//
// 원장 칸 관문이 거절한 글(식별자 칸의 NUL 처럼 입력 표지가 놓친 것)도 같은 400 으로 간다(ClassifyError 의 *store.CellRefusal).

// JudgeURL 은 URL 의 경로 · 질의(이름 · 값)에 NUL 이나 깨진 UTF-8 이 있는지 본다. 있으면 그 자리를 사람이 읽는 말과 그 갈래
// (nulesc.ErrNUL · nulesc.ErrUTF8)로 낸다. 순수 함수다.
//
// 경로는 푼 경로(u.Path - 처리기가 PathValue 로 읽는 것)를 잰다. 질의는 이름 차례로 잰다(어느 자리를 먼저 말할지가 요청마다
// 같게).
func JudgeURL(u *url.URL) (where string, bad error) {
	if err := nulesc.CheckName(u.Path); err != nil {
		return "경로", err
	}
	q := u.Query()
	for _, k := range slices.Sorted(maps.Keys(q)) {
		if err := nulesc.CheckName(k); err != nil {
			return "질의 이름", err
		}
		for _, v := range q[k] {
			if err := nulesc.CheckName(v); err != nil {
				return fmt.Sprintf("질의 %q", k), err
			}
		}
	}
	return "", nil
}

// withURLGate 는 URL 에 NUL · 깨진 UTF-8 이 든 요청을 처리기 앞에서 400 으로 거절한다. 화면(web)도 이 사슬 안의 처리기라 같이 걸린다.
//
// chain 에서 screenWrite 뒤 · idempotency 앞이다: 거절이 멱등 표에 자리를 잡지 않고, 화면 쓰기의 출처 대조(403)가 먼저다.
func (s *server) withURLGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if where, bad := JudgeURL(r.URL); bad != nil {
			s.writeError(w, r, classifyInput(bad, "URL 의 "+where))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkInput 은 푼 문서 doc 와 값 dst 에 입력 규칙을 건다(바꾸지 않는다). 거절이면 응답까지 내고 false 다.
func (s *server) checkInput(w http.ResponseWriter, r *http.Request, doc []byte, dst any) bool {
	if err := nulesc.Input(doc, dst); err != nil {
		s.fail(w, r, err)
		return false
	}
	return true
}

// classifyRefusal 은 입력 규칙(nulesc)이나 원장 칸 관문(store)이 거절한 글 하나를 응답으로 옮긴다(ClassifyError 가 부른다). 입력의
// 잘못이 아닌 것(원장 칸 관문의 깨진 JSON - 서버가 만든 값의 결함)이면 false 다.
func classifyRefusal(err error) (Classified, bool) {
	var nul *nulesc.Refusal
	if errors.As(err, &nul) {
		return classifyInput(nul.Err, fmt.Sprintf("요청 글 %q", nul.Pointer)), true
	}
	var cell *store.CellRefusal
	if errors.As(err, &cell) && cell.Unwrap() != nil {
		return classifyInput(cell.Unwrap(), fmt.Sprintf("%s(%s)", cell.Place(), cell.Where)), true
	}
	return Classified{}, false
}

// classifyInput 은 거절 갈래 kind(nulesc 의 갈래)와 그 자리 where 를 응답으로 옮긴다.
func classifyInput(kind error, where string) Classified {
	switch {
	case errors.Is(kind, nulesc.ErrSurrogate):
		return badRequest("lone_surrogate", where+" 에 짝 없는 서로게이트(홀로 선 \\ud800-\\udfff)가 있다",
			"그 글을 올바른 UTF-8 로 고쳐 다시 보내라 - 받으면 U+FFFD 로 바뀌어 다른 글과 같아진다.")
	case errors.Is(kind, nulesc.ErrUTF8):
		return badRequest("bad_utf8", where+" 이 올바른 UTF-8 이 아니다",
			"그 글을 UTF-8 로 고쳐 다시 보내라 - 깨진 바이트는 원장(Postgres text)이 담지 못하고, 받으면 U+FFFD 로 바뀌어 다른 글과 같아진다.")
	case errors.Is(kind, nulesc.ErrKeyNUL), errors.Is(kind, nulesc.ErrNUL):
		return badRequest("nul_refused", where+" 에 NUL(U+0000)이 있다",
			"그 자리(식별자 · 종류 · 경로 · 검색어 · JSON 키)에서 NUL 을 빼고 다시 보내라 - 자유 글(제목 · 본문 · 사유 · 꼬리표)의 "+
				"NUL 은 원장에 \\x00 표기로 담기지만, 이름을 바꾸면 원래 \\x00 네 글자인 다른 이름과 같아져 다른 것을 가리킨다.")
	}
	return badRequest("bad_json", where+" 를 JSON 으로 읽을 수 없다", "필드 이름과 타입을 확인해라.")
}
