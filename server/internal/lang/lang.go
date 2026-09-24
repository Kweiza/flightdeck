// Package lang 은 출력 언어 축이다. 기본은 한국어이고, FD_LANG=en 이면 영어로 낸다.
//
// ★ 번역은 **출력 경계**에서 한다 — CLI stdout, 훅의 additionalContext·block reason,
// MCP 도구 결과, 대시보드 HTML. 문장을 만드는 수백 곳을 고치지 않는다. 그 자리들은
// 한국어 문장 하나하나에 판정의 근거를 주석으로 달고 있고, 시험이 그 문장을 그대로
// 잠근다. 언어를 호출부마다 끼워 넣으면 그 둘을 한꺼번에 흔든다.
//
// ★ 표(catalog.go)에 없는 문장은 **한국어 그대로** 나간다 — 줄 단위로 전부 옮겨지지
// 않으면 그 줄은 원문이다(translate). 빈칸이나 추측 번역을 내지 않는다. 표가 덮는 표면은
// 시험(cmd/fd 의 TestEnglishSurfacesHaveNoHangul)이 대표 출력에서 한글 0자로 잰다.
//
// ★ 규칙은 한 줄 안에서만 맞는다(캡처가 줄바꿈을 안 넘는다). CLI 는 줄 단위로 흘려
// 보내므로(Writer) 여러 줄에 걸친 규칙은 스트림에서 절대 안 맞는다 — 그래서 아예 안 쓴다.
package lang

import (
	"bytes"
	"html"
	"io"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

// Lang 은 출력 언어다. 빈 값은 한국어다.
type Lang string

const (
	Korean  Lang = "ko"
	English Lang = "en"
)

// EnvKey 는 언어를 고르는 환경 키다. 서버(대시보드)와 클라이언트(CLI·훅·MCP)가 각자 읽는다.
const EnvKey = "FD_LANG"

// Parse 는 FD_LANG 값을 읽는다. 모르는 값이면 한국어와 false 다 — 조용히 영어로 넘기지 않는다.
func Parse(v string) (Lang, bool) {
	s := strings.ToLower(strings.TrimSpace(v))
	if i := strings.IndexAny(s, "-_."); i > 0 {
		s = s[:i] // en-US, en_US.UTF-8 → en
	}
	switch s {
	case "", "ko", "kr", "korean":
		return Korean, true
	case "en", "english":
		return English, true
	}
	return Korean, false
}

// FromEnv 는 환경에서 언어를 읽는다. 없거나 모르는 값이면 한국어다.
func FromEnv(get func(string) (string, bool)) Lang {
	if get == nil {
		return Korean
	}
	v, _ := get(EnvKey)
	l, _ := Parse(v)
	return l
}

// IsEnglish 는 번역을 해야 하는가다.
func (l Lang) IsEnglish() bool { return l == English }

// Text 는 사람이 읽을 글을 이 언어로 옮긴다. 한국어면 그대로 돌려준다.
//
// ★ **번역은 출력을 막지 못한다.** 훅은 어떤 실패에도 세션을 막지 않는다(fail-open)는
// 계약이 있고, 번역은 그 계약보다 아래다. 규칙 하나가 잘못돼 패닉이 나면 원문 한국어를
// 그대로 낸다 — 영어가 안 나오는 것은 불편이고, 훅이 죽는 것은 조율이 끊기는 것이다.
func (l Lang) Text(s string) (out string) {
	if !l.IsEnglish() || !HasHangul(s) {
		return s
	}
	defer func() {
		if recover() != nil {
			out = s
		}
	}()
	return translate(s)
}

// HasHangul 은 한글 음절·자모가 하나라도 있는가다. 시험이 "번역이 덮었다"를 재는 잣대다.
func HasHangul(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// ── 규칙 ────────────────────────────────────────────────────────────────

type rule struct {
	src    string // 표에 적힌 원문 패턴 — 시험이 쓴다
	re     *regexp.Regexp
	repl   string
	anchor string // 가장 긴 고정 조각. 이 조각이 없으면 정규식을 돌리지 않는다
}

var (
	compileOnce sync.Once
	compiled    []rule
	compiledJS  []rule
)

// 패턴 문법(catalog.go 의 원문 쪽):
//
//	{}  한 줄 안의 아무 글(최소 일치)
//	{n} 숫자
//	{w} 공백 없는 한 덩어리
//	{a} 공백 없는 ASCII 한 덩어리(한글이 안 섞인다)
//	{$} 줄 끝까지
//
// 번역 쪽의 {1}, {2} … 는 원문의 캡처를 차례로 가리킨다.
var holeRe = regexp.MustCompile(`\{(n|w|a|\$)?\}`)

func compileRule(src, dst string) rule {
	var b strings.Builder
	var fixed []string
	last := 0
	for _, m := range holeRe.FindAllStringSubmatchIndex(src, -1) {
		lit := src[last:m[0]]
		fixed = append(fixed, lit)
		b.WriteString(regexp.QuoteMeta(lit))
		kind := ""
		if m[2] >= 0 {
			kind = src[m[2]:m[3]]
		}
		switch kind {
		case "n":
			b.WriteString(`(\d+)`)
		case "w":
			b.WriteString(`(\S+)`)
		case "a":
			b.WriteString(`([!-~]+)`)
		case "$":
			b.WriteString(`([^\n]*)`)
		default:
			b.WriteString(`([^\n]+?)`)
		}
		last = m[1]
	}
	fixed = append(fixed, src[last:])
	b.WriteString(regexp.QuoteMeta(src[last:]))
	anchor := ""
	for _, f := range fixed {
		if len(f) > len(anchor) {
			anchor = f
		}
	}
	repl := regexp.MustCompile(`\{(\d+)\}`).ReplaceAllString(strings.ReplaceAll(dst, "$", "$$"), "$${$1}")
	return rule{src: src, re: regexp.MustCompile(b.String()), repl: repl, anchor: anchor}
}

func compileAll() {
	compileOnce.Do(func() {
		for _, p := range catalog {
			compiled = append(compiled, compileRule(p[0], p[1]))
		}
		for _, p := range jsCatalog {
			compiledJS = append(compiledJS, compileRule(p[0], p[1]))
		}
	})
}

func rules() []rule { compileAll(); return compiled }

func jsRules() []rule { compileAll(); return compiledJS }

// translate 는 **줄마다 전부 아니면 원문**이다. 규칙을 다 돌고도 한글이 남은 줄은
// 원래 한국어 줄로 되돌린다.
//
// ★ 이것이 없어서 난 일(2026-09-25, 표면 시험): 표에 없는 문장 안에서 단어 규칙("경로",
// "항목")만 맞아 "Paths 실재: … 지금 이 Item은 …" 같은 **반쪽 줄**이 나왔다. 반쪽 줄은
// 한국어 원문보다 나쁘다 — 영어 사용자도 못 읽고 한국어 사용자도 못 읽는다. 그래서 단어
// 규칙(표 머리·버튼)은 줄 전체가 영어가 될 때만 효력이 있다.
//
// 대가: 사용자가 한국어로 쓴 제목·판단이 든 줄은 틀(chrome)까지 한국어로 남는다.
func translate(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if !HasHangul(ln) {
			continue
		}
		if t := apply(rules(), ln); !HasHangul(t) {
			lines[i] = t
		}
	}
	return strings.Join(lines, "\n")
}

func apply(rs []rule, s string) string {
	for _, r := range rs {
		if r.anchor != "" && !strings.Contains(s, r.anchor) {
			continue
		}
		s = r.re.ReplaceAllString(s, r.repl)
		if !HasHangul(s) {
			break
		}
	}
	return s
}

// ── 출력 경계 ───────────────────────────────────────────────────────────

// Writer 는 완성된 줄마다 번역해서 w 로 보낸다. 한국어면 w 그대로다.
// 마지막 줄바꿈 없는 조각은 Flush 가 내보낸다.
func (l Lang) Writer(w io.Writer) *LineWriter {
	return &LineWriter{l: l, w: w}
}

// LineWriter 는 줄 단위 번역 writer 다.
type LineWriter struct {
	l   Lang
	w   io.Writer
	buf []byte
}

func (lw *LineWriter) Write(p []byte) (int, error) {
	if !lw.l.IsEnglish() {
		return lw.w.Write(p)
	}
	lw.buf = append(lw.buf, p...)
	if i := bytes.LastIndexByte(lw.buf, '\n'); i >= 0 {
		done := string(lw.buf[:i+1])
		lw.buf = append(lw.buf[:0], lw.buf[i+1:]...)
		if _, err := io.WriteString(lw.w, lw.l.Text(done)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush 는 남은 조각을 내보낸다.
func (lw *LineWriter) Flush() error {
	if len(lw.buf) == 0 {
		return nil
	}
	s := string(lw.buf)
	lw.buf = lw.buf[:0]
	_, err := io.WriteString(lw.w, lw.l.Text(s))
	return err
}

// jsStringRe 는 스크립트 안의 큰따옴표 문자열 리터럴이다. 주석과 코드는 안 건드린다 —
// 화면에 뜨는 글은 리터럴뿐이다.
var jsStringRe = regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"`)

// 번역하는 속성. 화면에 보이는 것만 — 값(value)·data-* 는 폼과 스크립트가 읽는 것이라 안 건드린다.
var visibleAttrRe = regexp.MustCompile(`(\s(?:placeholder|title|aria-label|alt)=")([^"]*)(")`)

// HTML 은 렌더된 문서의 텍스트 노드와 보이는 속성을 옮긴다. <script>·<style> 안은 건드리지 않는다.
// 한국어면 그대로다.
func (l Lang) HTML(doc []byte) (out []byte) {
	if !l.IsEnglish() {
		return doc
	}
	defer func() { // Text 와 같은 방벽 — 화면은 한국어로라도 나가야 한다
		if recover() != nil {
			out = doc
		}
	}()
	s := string(doc)
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			b.WriteString(l.htmlText(s[i:]))
			break
		}
		b.WriteString(l.htmlText(s[i : i+lt]))
		i += lt
		end := tagEnd(s, i)
		tag := s[i:end]
		name := tagName(tag)
		if name == "html" {
			tag = strings.Replace(tag, `lang="ko"`, `lang="en"`, 1)
		}
		b.WriteString(visibleAttrRe.ReplaceAllStringFunc(tag, func(m string) string {
			p := visibleAttrRe.FindStringSubmatch(m)
			return p[1] + html.EscapeString(l.Text(html.UnescapeString(p[2]))) + p[3]
		}))
		i = end
		if name == "script" || name == "style" {
			closeTag := "</" + name
			j := strings.Index(strings.ToLower(s[i:]), closeTag)
			if j < 0 {
				b.WriteString(s[i:])
				break
			}
			body := s[i : i+j]
			if name == "script" {
				body = jsStringRe.ReplaceAllStringFunc(body, func(lit string) string {
					if !HasHangul(lit) {
						return lit
					}
					return apply(jsRules(), lit)
				})
			}
			b.WriteString(body)
			i += j
		}
	}
	return []byte(b.String())
}

func (l Lang) htmlText(t string) string {
	if !HasHangul(t) {
		return t
	}
	return html.EscapeString(l.Text(html.UnescapeString(t)))
}

// tagEnd 는 s[i]=='<' 인 태그의 끝(> 다음) 위치다. 따옴표 안의 > 는 건너뛴다.
func tagEnd(s string, i int) int {
	if strings.HasPrefix(s[i:], "<!--") {
		if j := strings.Index(s[i:], "-->"); j >= 0 {
			return i + j + 3
		}
		return len(s)
	}
	var q byte
	for j := i + 1; j < len(s); j++ {
		c := s[j]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '>':
			return j + 1
		}
	}
	return len(s)
}

func tagName(tag string) string {
	t := strings.TrimPrefix(tag, "<")
	if strings.HasPrefix(t, "/") || strings.HasPrefix(t, "!") {
		return ""
	}
	j := strings.IndexAny(t, " \t\n>/")
	if j < 0 {
		j = len(t)
	}
	return strings.ToLower(t[:j])
}
