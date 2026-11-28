package main

import (
	"html"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/lang"
	"github.com/kweiza/flightdeck/internal/web"
)

// FD_LANG=en 의 표면이 한글 0자인가. 범위는 처음 만나는 표면이다 — 보드,
// add·next·pick·note·finish·show, land 세 갈래, pick(leave), 꼬리, doctor, 대시보드 한 장,
// 토큰 로그인 화면(첫 방문과 거절 사유).
//
// ★ 번역은 출력 경계의 표(internal/lang/catalog.go)가 한다. 문장을 만드는 자리가 바뀌면
// 표가 조용히 빗나가 그 문장만 한국어로 돌아온다 — 그 회귀를 잡는 것이 이 시험이다.
// 빨개지면 남은 줄이 그대로 찍힌다. 그 줄을 표에 넣어라(표 밖의 표면을 넓힐 때도 여기에 명령을 더한다).
//
// ★ 기본값(한국어)이 한 글자도 안 바뀐다는 것은 이 모듈의 나머지 시험 전부가 잰다 —
// 그 시험들은 FD_LANG 없이 돌고 한국어 문장을 그대로 단정한다.
func TestEnglishSurfacesHaveNoHangul(t *testing.T) {
	h := newHarness(t)
	en := func(cc string, args ...string) string {
		t.Helper()
		e := map[string]string{}
		for k, v := range h.env {
			e[k] = v
		}
		e["CLAUDE_CODE_SESSION_ID"] = cc
		e[lang.EnvKey] = "en"
		_, out := h.runEnv(e, "", args...)
		return out
	}
	var outs []string
	add := func(label, out string) { outs = append(outs, "── "+label+"\n"+out) }

	add("status(빈 보드)", en("cc-a", "status"))
	add("add", en("cc-a", "add", "--id", "fix-login-redirect", "--title", "Stop the redirect loop", "--body", "Validate next.", "--path", "src/auth/login.go"))
	en("cc-a", "add", "--id", "add-rate-limit", "--title", "Rate limit the API", "--body", "Token bucket.", "--path", "src/api/ratelimit.go")
	en("cc-a", "add", "--id", "api-error-codes", "--title", "Document error codes", "--body", "List them.", "--path", "docs/api.md")
	add("next", en("cc-a", "next"))
	add("pick", en("cc-a", "pick", "fix-login-redirect"))
	add("pick(둘째 세션)", en("cc-b", "pick", "add-rate-limit"))
	add("note", en("cc-b", "note", "--kind", "decision", "--item", "add-rate-limit", "--body", "Per API key, not per IP."))
	add("status(선점 둘)", en("cc-a", "status"))
	add("show", en("cc-a", "show", "fix-login-redirect"))
	add("finish", en("cc-a", "finish", "fix-login-redirect", "--outcome", "done", "--body", "Verified: go vet clean."))
	en("cc-b", "finish", "add-rate-limit", "--outcome", "done", "--body", "Verified.")
	add("land(차례)", en("cc-a", "land"))
	add("land(대기)", en("cc-b", "land"))
	add("land(보고)", en("cc-a", "land", "--ok"))
	add("land(둘째 차례)", en("cc-b", "land"))
	en("cc-b", "land", "--ok")
	en("cc-c", "pick", "api-error-codes")
	add("pick(leave)", en("cc-c", "pick", "--leave", "Waiting on the API owners."))
	add("status(끝)", en("cc-a", "status"))
	add("doctor", en("cc-a", "doctor"))

	var left []string
	for _, o := range outs {
		for _, line := range strings.Split(o, "\n") {
			if lang.HasHangul(line) && !strings.HasPrefix(line, "── ") {
				left = append(left, line)
			}
		}
	}
	// 대시보드 한 장. 서버 쪽 언어는 서버 프로세스의 FD_LANG 이다(web.WithLang).
	rec := httptest.NewRecorder()
	web.New(h.svc, web.WithLang(lang.English)).ServeHTTP(rec, httptest.NewRequest("GET", "/?project="+h.project, nil))
	for _, line := range visibleLines(rec.Body.String()) {
		if lang.HasHangul(line) {
			left = append(left, "dashboard: "+line)
		}
	}
	// 토큰 로그인 화면. 대시보드와 같은 언어 옵션(web.WithLang)을 받는다 — 운영 배선은 serve.go 의
	// serveAPIOptions 가 대시보드와 같은 옵션 한 벌을 넘긴다(TestServeAPIOptionsLoginScreenFollowsLang).
	// 사유는 api 가 거절할 때 싣는 문장 전부다(api/login.go · api/auth.go).
	var loginBodies []string
	for _, why := range append([]string{""}, loginReasonsForLangTest...) {
		lrec := httptest.NewRecorder()
		web.LoginScreen(web.WithLang(lang.English))(lrec, httptest.NewRequest("GET", "/", nil),
			web.LoginView{Error: why, Next: "/", Action: "./login"})
		loginBodies = append(loginBodies, lrec.Body.String())
		for _, line := range visibleLines(lrec.Body.String()) {
			if lang.HasHangul(line) {
				left = append(left, "login: "+line)
			}
		}
	}
	if len(left) > 0 {
		t.Fatalf("FD_LANG=en 인데 한글이 남은 줄 %d개 — 표(internal/lang/catalog.go)에 넣어라:\n  %s\n\n전체 출력:\n%s",
			len(left), strings.Join(left, "\n  "), strings.Join(outs, "\n"))
	}
	// 번역이 **실제로 돌았다**는 대조. 한글이 0인 이유가 "출력이 비었다"면 이 시험은 아무것도 안 잰다.
	all := strings.Join(outs, "\n")
	for _, want := range []string{"Claimed work:", "your turn", "you are #2 in line", "released the lane", "pick · claimed", "── tail ──"} {
		if !strings.Contains(all, want) {
			t.Errorf("영어 출력에 %q 가 없다 — 번역이 안 돌았거나 명령이 실패했다:\n%s", want, all)
		}
	}
	if body := rec.Body.String(); !strings.Contains(body, `<html lang="en">`) || !strings.Contains(body, "Now — claimed work") {
		t.Errorf("대시보드가 영어로 안 나왔다(상태 %d)", rec.Code)
	}
	if !strings.Contains(all, "■ this machine") {
		t.Errorf("doctor 가 영어로 안 나왔다:\n%s", all)
	}
	if b := loginBodies[0]; !strings.Contains(b, `<html lang="en">`) || !strings.Contains(b, "Sign in") {
		t.Errorf("로그인 화면이 영어로 안 나왔다:\n%s", b)
	}
}

// loginReasonsForLangTest 는 로그인 화면에 사유로 실릴 수 있는 문장이다 — 거절하는 갈래만
// (api/login.go 의 loginRefused 호출, internal/auth 에서 OK=false 인 사유, store.ErrLoginCodeRefused). 통과 사유는
// 폼에 안 뜬다. 사유를 더하거나 고치면 여기와 표(internal/lang/catalog.go)를 함께 고친다.
var loginReasonsForLangTest = []string{
	// api/login.go
	"폼을 읽을 수 없다",
	"로그인 코드를 적어라",
	"로그인 코드가 아니다 - 관리자에게 로그인 코드를 받아라. FD_TOKEN 은 화면 로그인에 못 쓴다(부트스트랩 전용)",
	"토큰이나 로그인 코드를 적어라",
	"토큰이 일치하지 않는다",
	"이 서버는 계정 원장이 없어 로그인 코드를 못 바꾼다",
	"로그인 코드가 맞지 않거나 만료됐다(한 번만 쓴다) - 관리자에게 새 코드를 받아라",
	// internal/auth
	"Authorization 헤더가 'Bearer <token>' 형식이 아니다",
	"Authorization 인증 방식이 Bearer 가 아니다",
	"이 값은 기기 토큰이 아니다 - 화면 쿠키 · 로그인 코드는 헤더로 못 쓴다",
	"기기 토큰의 모양이 아니다 - 잘렸거나 다른 값이 섞였다",
	"화면 세션 쿠키의 모양이 아니다 - 다시 로그인해라",
	"쿠키가 있지만 이 경로는 화면이 아니다 - /api/v1 은 Authorization 헤더만 받는다",
	"자격증명이 없다 - Authorization: Bearer <기기 토큰> 을 실어라(루프백도 같다)",
	"이 값은 기기 토큰이 아니고 이 서버에는 FD_TOKEN 이 없다 - fd login 으로 기기 토큰을 받아라",
	"FD_TOKEN 은 부트스트랩 전용이다(FD_AUTH=accounts) - 기기 토큰(fd login)이나 화면 로그인을 써라",
	"계정 원장이 없어 부트스트랩 여부를 못 쟀다",
	"관리자 수를 못 읽었다 - 원장에 붙지 못했다",
	"관리자가 이미 있다 - FD_TOKEN 은 첫 관리자를 만들 때만 통한다. 관리자 로그인 코드는 서버 컨테이너 안의 fd admin recover <login> 이 낸다(옛 원장을 이어받았으면 login 은 owner 다)",
	"이 서버는 계정 원장이 없어 기기 토큰을 대조할 수 없다",
	"이 기기 토큰을 서버가 모른다 - 다른 서버의 토큰이거나 지워진 기기다. fd login 으로 다시 받아라",
	"기기 토큰을 못 대조했다 - 원장에 붙지 못했다",
	"철회된 기기 토큰이다 - fd login 으로 새로 받아라",
	"이 기기의 계정이 꺼져 있다 - 관리자에게 물어라",
	"이 서버는 계정 원장이 없어 로그인 쿠키를 대조할 수 없다 - 다시 로그인해라",
	"로그인이 끝났다(로그아웃했거나 지워졌다) - 다시 로그인해라",
	"로그인 쿠키를 못 대조했다 - 원장에 붙지 못했다",
	"로그인이 만료됐다 - 다시 로그인해라",
	"이 계정이 꺼져 있다 - 관리자에게 물어라",
}

var (
	scriptStyleRe = regexp.MustCompile(`(?s)<(script|style)\b.*?</(script|style)>`)
	tagRe         = regexp.MustCompile(`(?s)<[^>]*>`)
	visibleAttr   = regexp.MustCompile(`\s(?:placeholder|title|aria-label|alt)="([^"]*)"`)
)

// visibleLines 는 사람이 보는 글 — 텍스트 노드와 보이는 속성 — 을 줄로 낸다.
func visibleLines(doc string) []string {
	doc = scriptStyleRe.ReplaceAllString(doc, "")
	var out []string
	for _, m := range visibleAttr.FindAllStringSubmatch(doc, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	for _, l := range strings.Split(html.UnescapeString(tagRe.ReplaceAllString(doc, "\n")), "\n") {
		if s := strings.TrimSpace(l); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// MCP 도구 결과도 같은 표로 옮겨진다 — 운영과 같은 배선(newApp → newMCPBackend → mcpsrv.New)이다.
// 세션이 에이전트라 사람보다 이 표면을 더 많이 읽는다.
func TestEnglishMCPToolResults(t *testing.T) {
	h := newHarness(t)
	h.env[lang.EnvKey] = "en"
	r := newMCPRig(t, h, "cc-mcp-en")
	frames := mcpServe(t, r, mcpCall("board", map[string]any{}), mcpCall("land", map[string]any{}))
	if len(frames) != 2 {
		t.Fatalf("응답 프레임이 %d개다 — 둘이어야 한다", len(frames))
	}
	for i, want := range []string{"Claimed work:", "your turn"} {
		text, _ := mcpText(t, frames[i])
		if !strings.Contains(text, want) {
			t.Errorf("도구 %d 결과에 %q 가 없다:\n%s", i, want, text)
		}
		for _, line := range strings.Split(text, "\n") {
			if lang.HasHangul(line) {
				t.Errorf("도구 %d 결과에 한글이 남았다: %q", i, line)
			}
		}
	}
}
