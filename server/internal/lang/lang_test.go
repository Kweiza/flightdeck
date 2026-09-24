package lang

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 표의 규칙 하나하나가 컴파일되고, 번역문이 **없는 캡처를 가리키지 않는다.**
//
// ★ 이 시험이 없어서 난 일(2026-09-25, 첫 빌드): 칸 종류가 없는 `{}` 를 읽다가
// compileRule 이 패닉했고, 그 패닉이 SessionStart 훅을 죽였다 — 훅은 fail-open 이
// 계약인데 번역이 그 아래에서 계약을 깼다. 지금은 Text 가 패닉을 삼키지만, 삼킨 규칙은
// 영영 영어를 못 낸다. 그래서 규칙은 여기서 전부 한 번씩 컴파일한다.
func TestEveryRuleCompilesAndReferencesOnlyItsCaptures(t *testing.T) {
	ref := regexp.MustCompile(`\{(\d+)\}`)
	check := func(table string, rows [][2]string) {
		seen := map[string]bool{}
		for i, p := range rows {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s[%d] %q 가 컴파일 중 패닉했다: %v", table, i, p[0], r)
					}
				}()
				r := compileRule(p[0], p[1])
				groups := r.re.NumSubexp()
				for _, m := range ref.FindAllStringSubmatch(p[1], -1) {
					n, _ := strconv.Atoi(m[1])
					if n < 1 || n > groups {
						t.Errorf("%s[%d] %q 의 번역이 {%d} 를 쓰는데 캡처는 %d개다", table, i, p[0], n, groups)
					}
				}
				if !HasHangul(p[0]) {
					t.Errorf("%s[%d] %q 에 한글이 없다 — 이 규칙은 한국어 출력을 못 만난다", table, i, p[0])
				}
				if HasHangul(p[1]) {
					t.Errorf("%s[%d] 번역 %q 에 한글이 남았다", table, i, p[1])
				}
			}()
			if seen[p[0]] {
				t.Errorf("%s[%d] %q 가 두 번 나온다 — 뒤의 것은 영영 안 맞는다", table, i, p[0])
			}
			seen[p[0]] = true
		}
	}
	check("catalog", catalog)
	check("jsCatalog", jsCatalog)
	if len(rules()) != len(catalog) || len(jsRules()) != len(jsCatalog) {
		t.Fatalf("컴파일된 규칙 수가 표와 다르다: %d/%d, %d/%d", len(rules()), len(catalog), len(jsRules()), len(jsCatalog))
	}
}

// 한국어(기본값)는 **한 바이트도** 안 바꾼다 — 기존 시험 수천 개가 그 문장을 잠그고 있다.
func TestKoreanIsIdentity(t *testing.T) {
	in := "잡혀 있는 작업 3건 (선점 기준이다 — 세션의 생사가 아니다)\n<b>보드</b>"
	for _, v := range []string{"", "ko", "KO", "ko-KR", "korean", "fr"} {
		l, _ := Parse(v)
		if got := l.Text(in); got != in {
			t.Errorf("FD_LANG=%q 가 글을 바꿨다: %q", v, got)
		}
		if got := string(l.HTML([]byte(in))); got != in {
			t.Errorf("FD_LANG=%q 가 HTML 을 바꿨다: %q", v, got)
		}
	}
	if _, ok := Parse("fr"); ok {
		t.Error("모르는 값(fr)을 아는 값으로 받았다 — 조용히 한국어로 떨어졌다는 사실을 호출자가 못 본다")
	}
	for _, v := range []string{"en", "EN", "en-US", "en_US.UTF-8", "english"} {
		if l, ok := Parse(v); !ok || l != English {
			t.Errorf("FD_LANG=%q 가 영어가 아니다: %q %v", v, l, ok)
		}
	}
}

func TestTextTranslatesBoardLines(t *testing.T) {
	cases := map[string]string{
		"잡혀 있는 작업 3건 (선점 기준이다 — 세션의 생사가 아니다)":                                       "Claimed work: 3 (counted by claims — not by whether a session is alive)",
		"land · 너는 2번째다 (줄 행 2 · 자원 landing)":                                       "land · you are #2 in line (row 2 · resource landing)",
		"land · 네 차례다 — landing 를 쥐었다 (줄 행 1)":                                      "land · your turn — you hold landing (row 1)",
		" 01M3ADY6… add-rate-limit · ● 활동 4초 전":                                     " 01M3ADY6… add-rate-limit · ● active 4s ago",
		"큐 열림 3건(최고령 1시간 5분)":                                                       "Queue: 3 open (oldest 1h 5m)",
		"   api · claude · add-rate-limit +0 · active · 경로 2: a.go, b.go · tool 8초": "   api · claude · add-rate-limit +0 · active · paths (2): a.go, b.go · tool 8s",
	}
	for in, want := range cases {
		if got := English.Text(in); got != want {
			t.Errorf("\n in: %q\ngot: %q\nwant %q", in, got, want)
		}
	}
	// 반쪽 줄을 내지 않는다 — 단어 규칙("경로"·"항목")만 맞는 문장은 줄째 원문이다.
	half := "경로 실재: 이 문장은 표에 없고 항목이라는 낱말만 있다"
	if got := English.Text(half); got != half {
		t.Errorf("반쪽 번역이 나왔다: %q", got)
	}
	// 여러 줄이면 줄마다 따로 판정한다.
	if got := English.Text("큐 열림 2건\n이 줄은 표에 없다"); got != "Queue: 2 open\n이 줄은 표에 없다" {
		t.Errorf("줄마다 따로 판정하지 않았다: %q", got)
	}
	// 표에 없는 문장은 한국어 그대로다 — 추측 번역을 내지 않는다.
	if got := English.Text("이런 문장은 표에 없다"); got != "이런 문장은 표에 없다" {
		t.Errorf("표에 없는 문장이 바뀌었다: %q", got)
	}
	// "$" 가 들어 있는 원문·번역이 치환 문법으로 새지 않는다.
	if got := English.Text("이 셸에서는: fd land --ok · fd land --fail \"<사유>\" · fd land --leave \"<사유>\""); strings.Contains(got, "${") || HasHangul(got) {
		t.Errorf("치환 문법이 샜거나 번역이 안 됐다: %q", got)
	}
}

func TestHTMLTranslatesTextAttributesAndScriptStringsOnly(t *testing.T) {
	doc := `<!doctype html><html lang="ko"><head><title>보드</title><style>.a::after{content:"회수"}</style></head>` +
		`<body><h2>⑥ 판단 검색</h2><input placeholder="판단 전문 검색(FTS5)" value="회수" data-x="회수">` +
		`<p>선점 없는 세션 2건은 안 낸다 — 겹침 처방은 그 세션들도 그대로 본다.</p><p>이 줄은 표에 없다 &amp; 그대로 남는다</p>` +
		`<script>// 주석은 그대로: 더보기
var s = "더보기 " + 3 + "건 (남은 " + 5 + "건)";</script></body></html>`
	got := string(English.HTML([]byte(doc)))
	for _, want := range []string{
		`<html lang="en">`,
		`<h2>⑥ Judgment search</h2>`,
		`placeholder="Full-text judgment search (FTS5)"`,
		`value="회수" data-x="회수"`, // 폼 값·data 는 안 건드린다
		`content:"회수"`,           // style 안은 안 건드린다
		`// 주석은 그대로: 더보기`,        // 스크립트 주석은 안 건드린다
		`"More " + 3 + " (" + 5 + " left)"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("결과에 %q 가 없다:\n%s", want, got)
		}
	}
	if strings.Contains(got, "선점 없는 세션") {
		t.Errorf("텍스트 노드가 안 옮겨졌다:\n%s", got)
	}
	// 표에 없는 줄은 원문 그대로, 이스케이프도 그대로다.
	if !strings.Contains(got, "<p>이 줄은 표에 없다 &amp; 그대로 남는다</p>") {
		t.Errorf("표에 없는 줄이 바뀌었다:\n%s", got)
	}
}

func TestLineWriterTranslatesCompleteLinesAndFlushesTheRest(t *testing.T) {
	var buf bytes.Buffer
	w := English.Writer(&buf)
	_, _ = w.Write([]byte("큐 열림 2건\n큐 열"))
	if got := buf.String(); got != "Queue: 2 open\n" {
		t.Fatalf("완성된 줄만 나가야 한다: %q", got)
	}
	_, _ = w.Write([]byte("림 5건"))
	_ = w.Flush()
	if got := buf.String(); got != "Queue: 2 open\nQueue: 5 open" {
		t.Fatalf("조각이 이어 붙어 번역돼야 한다: %q", got)
	}
	var kb bytes.Buffer
	kw := Korean.Writer(&kb)
	_, _ = kw.Write([]byte("큐 열림 2건"))
	if kb.String() != "큐 열림 2건" {
		t.Fatalf("한국어 writer 는 그대로 흘려야 한다: %q", kb.String())
	}
}
