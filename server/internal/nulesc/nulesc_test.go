package nulesc_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/nulesc"
)

// Escape 는 되돌릴 수 있고(단사), 결과에 NUL 이 없고, NUL 도 「x00」도 없는 글은 그대로 둔다. 「x00」 앞 백슬래시 묶음의 홀짝으로
// 되돌리는 규칙(unescape - README 가 사람에게 적은 것)으로 짧은 글 전부를 맞댄다.
func TestEscapeIsReversible(t *testing.T) {
	alphabet := []byte{'\\', 'x', '0', 0, 'a'}
	seen := map[string]string{}
	var walk func(prefix []byte)
	n := 0
	walk = func(prefix []byte) {
		s := string(prefix)
		e := nulesc.Escape(s)
		n++
		switch {
		case strings.IndexByte(e, 0) >= 0:
			t.Fatalf("Escape(%q) = %q 에 NUL 이 남았다", s, e)
		case unescape(e) != s:
			t.Fatalf("Escape(%q) = %q 를 되돌리니 %q 다", s, e, unescape(e))
		case strings.IndexByte(s, 0) < 0 && !strings.Contains(s, "x00") && e != s:
			t.Fatalf("NUL 도 x00 도 없는 %q 를 %q 로 바꿨다", s, e)
		}
		if prev, ok := seen[e]; ok {
			t.Fatalf("%q 와 %q 가 같은 %q 가 된다", prev, s, e)
		}
		seen[e] = s
		if len(prefix) == 7 {
			return
		}
		for _, c := range alphabet {
			walk(append(prefix[:len(prefix):len(prefix)], c))
		}
	}
	walk(nil)
	if want := (78125*5 - 1) / 4; n != want { // 5^0 + … + 5^7
		t.Fatalf("잰 글 %d 개, 기대 %d 개", n, want)
	}
	for in, want := range map[string]string{
		"\x00":           `\x00`,
		`\` + "\x00":     `\\\x00`,
		`\x00` + "\x00":  `\\x00\x00`,
		`\\x00` + "\x00": `\\\\x00\x00`,
		"\x00" + `\x01`:  `\x00\x01`,
		`\n` + "\x00":    `\n\x00`,
	} {
		if got := nulesc.Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, 기대 %q", in, got, want)
		}
	}
}

// unescape 는 Escape 의 역이다(README 「SQLite 원장을 Postgres 로 옮기기」의 되돌리는 규칙): 「x00」 바로 앞의 백슬래시 묶음이
// 홀수 개(2k+1)면 k 개 백슬래시 + NUL, 짝수 개(2k)면 k 개 백슬래시 + 「x00」. 그 밖의 바이트는 그대로다.
func unescape(e string) string {
	var b strings.Builder
	for i := 0; i < len(e); {
		if e[i] != '\\' {
			b.WriteByte(e[i])
			i++
			continue
		}
		j := i
		for j < len(e) && e[j] == '\\' {
			j++
		}
		n := j - i
		if !strings.HasPrefix(e[j:], "x00") {
			b.WriteString(e[i:j])
			i = j
			continue
		}
		b.WriteString(strings.Repeat(`\`, n/2))
		if n%2 == 1 {
			b.WriteByte(0)
		} else {
			b.WriteString("x00")
		}
		i = j + 3
	}
	return b.String()
}

// EscapeJSON 은 U+0000 이 든 글 값 토큰만 원문 바이트에서 고친다: 같은 문서의 다른 글 · 공백 · 수 · 다른 이스케이프는 원래
// 바이트이고, 고친 자리를 JSON Pointer 로 낸다. 키의 U+0000 은 고치지 않고 거절하며(식별자다 - 바꾸면 옆 키와 같아질 수 있다),
// 짝 없는 서로게이트는 U+0000 이 없어도 거절한다. 거절은 그 글의 자리(Refusal.Pointer)를 댄다.
func TestEscapeJSONRewritesOnlyTheTokensWithNUL(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
		changes        []string
		err            error
		at             string // 거절한 글의 자리(거절이 아니면 빈 글)
	}{
		{"값", `{"a":"x\u0000"}`, `{"a":"x\\x00"}`, []string{"/a"}, nil, ""},
		{"문서 전체가 글", `"\u0000"`, `"\\x00"`, []string{""}, nil, ""},
		{"Pointer 이스케이프", `{"a/b":{"~":["\u0000"]}}`, `{"a/b":{"~":["\\x00"]}}`, []string{"/a~1b/~0/0"}, nil, ""},
		{"이스케이프된 백슬래시 뒤 u0000 은 글자다", `["\\u0000"]`, `["\\u0000"]`, nil, nil, ""},
		{"범위 밖 수 뒤", `[1e400,"\u0000"]`, `[1e400,"\\x00"]`, []string{"/1"}, nil, ""},
		// 디코드해 다시 쓰는 길이면 \u00e9 는 é, \/ 는 / 로 바뀐다. 고친 토큰 안의 그것들도 원래 바이트다.
		{"다른 이스케이프는 원래 바이트", `{ "h":"\u00e9\/", "n": "\/\u00e9\u0000", "f": 1.50 }`,
			`{ "h":"\u00e9\/", "n": "\/\u00e9\\x00", "f": 1.50 }`, []string{"/n"}, nil, ""},
		// 바꾸는 단위가 글 하나다: U+0000 이 없는 옆 글의 \x00 표기는 그대로다.
		{"옆 글의 \\x00 표기", `["\u0000","\\x00"]`, `["\\x00","\\x00"]`, []string{"/0"}, nil, ""},
		{"같은 글의 \\x00 표기", `["\\x00\u0000"]`, `["\\\\x00\\x00"]`, []string{"/0"}, nil, ""},
		{"\\u005c 도 백슬래시다", `["\u005c\u0000"]`, `["\u005c\u005c\\x00"]`, []string{"/0"}, nil, ""},
		{"\\u0078 도 x 다", `["\\\u00780\u0030\u0000"]`, `["\\\\\u00780\u0030\\x00"]`, []string{"/0"}, nil, ""},
		{"짝 맞은 서로게이트", `["\ud83d\ude00\u0000"]`, `["\ud83d\ude00\\x00"]`, []string{"/0"}, nil, ""},
		{"값이 여럿", `{"a":["\u0000"],"b":"\u0000"}`, `{"a":["\\x00"],"b":"\\x00"}`, []string{"/a/0", "/b"}, nil, ""},
		{"U+0000 없는 문서", `{"a":["\\x00","\u00e9"]}`, `{"a":["\\x00","\u00e9"]}`, nil, nil, ""},
		// 키는 바꾸지 않는다: 바꾸면 이 두 키가 하나(`\x00`)가 되어 jsonb 가 한 멤버를 버린다.
		{"키와 옆 키의 \\x00 표기", `{"\u0000":1,"\\x00":2}`, `{"\u0000":1,"\\x00":2}`, nil, nulesc.ErrKeyNUL, "/\x00"},
		{"키", `{"\u0000":1}`, `{"\u0000":1}`, nil, nulesc.ErrKeyNUL, "/\x00"},
		{"안쪽 키", `{"o":{"k\u0000":[1]}}`, `{"o":{"k\u0000":[1]}}`, nil, nulesc.ErrKeyNUL, "/o/k\x00"},
		// 값을 먼저 고쳤어도 뒤의 키가 거절이면 문서는 받은 그대로다.
		{"고친 값 뒤의 키", `{"a":"\u0000","b\u0000":1}`, `{"a":"\u0000","b\u0000":1}`, nil, nulesc.ErrKeyNUL, "/b\x00"},
		{"키 안의 Pointer 이스케이프", `[{"~/\u0000":1}]`, `[{"~/\u0000":1}]`, nil, nulesc.ErrKeyNUL, "/0/~0~1\x00"},
		{"홀로 선 높은 서로게이트", `["\ud800"]`, `["\ud800"]`, nil, nulesc.ErrSurrogate, "/0"},
		{"홀로 선 낮은 서로게이트", `["a\udc00b"]`, `["a\udc00b"]`, nil, nulesc.ErrSurrogate, "/0"},
		{"높은 뒤 높은", `["\ud800\ud800"]`, `["\ud800\ud800"]`, nil, nulesc.ErrSurrogate, "/0"},
		// 디코더는 둘 다 「U+FFFD + U+0000」으로 읽어 다른 두 키를 하나로 만든다. 서로게이트가 먼저다.
		{"U+FFFD 로 같아질 두 키", `{"\ud800\u0000":1,"\ufffd\u0000":2}`, `{"\ud800\u0000":1,"\ufffd\u0000":2}`, nil,
			nulesc.ErrSurrogate, "/" + "\ufffd" + "\x00"},
		{"깨진 문서", `[`, `[`, nil, nulesc.ErrSyntax, ""},
		{"날 NUL", "[\"\x00\"]", "[\"\x00\"]", nil, nulesc.ErrSyntax, ""},
	} {
		got, changes, err := nulesc.EscapeJSON(c.in)
		if got != c.want || !reflect.DeepEqual(changes, c.changes) || !errors.Is(err, c.err) || (err == nil) != (c.err == nil) {
			t.Errorf("%s: EscapeJSON(%s) = (%s, %q, %v), 기대 (%s, %q, %v)", c.name, c.in, got, changes, err, c.want, c.changes, c.err)
		}
		var r *nulesc.Refusal
		if refused := errors.As(err, &r); refused != (c.at != "") || (refused && r.Pointer != c.at) {
			t.Errorf("%s: 거절 자리 %+v, 기대 %q", c.name, r, c.at)
		}
	}
	// 원래 바이트를 둔다: 디코드한 값이 같은 두 문서(\u0041 · A)는 고친 뒤에도 다르다.
	a, _, _ := nulesc.EscapeJSON(`["\u0041\u0000"]`)
	b, _, _ := nulesc.EscapeJSON(`["A\u0000"]`)
	if a != `["\u0041\\x00"]` || b != `["A\\x00"]` {
		t.Fatalf("같은 값의 다른 이스케이프를 한 꼴로 썼다: %s · %s", a, b)
	}
}

// U+0000 이 든 글 토큰을 고쳐 디코드하면 원래 값에 Escape 를 건 값이다. U+0000 이 없는 토큰은 원래 바이트 그대로다. U+0000 이 든 서로 다른
// 토큰은 고친 뒤에도 다르다(단사). 백슬래시 · x · 0 · U+0000 을 날 바이트와 이스케이프로 섞은 짧은 토큰 전부를 잰다.
func TestEscapeJSONDecodesToEscape(t *testing.T) {
	units := []string{`\\`, `\u005c`, `x`, `\u0078`, `0`, `\u0030`, `\u0000`, `a`, `é`, `\ud83d\ude00`}
	seen := map[string]string{}
	n := 0
	var walk func(tok string, depth int)
	walk = func(tok string, depth int) {
		doc := `["` + tok + `"]`
		got, changes, err := nulesc.EscapeJSON(doc)
		if err != nil {
			t.Fatalf("EscapeJSON(%s): %v", doc, err)
		}
		var old, now []string
		if err := json.Unmarshal([]byte(doc), &old); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(got), &now); err != nil {
			t.Fatalf("EscapeJSON(%s) = %s 가 문서가 아니다: %v", doc, got, err)
		}
		n++
		hasNUL := strings.IndexByte(old[0], 0) >= 0
		switch {
		case hasNUL && now[0] != nulesc.Escape(old[0]):
			t.Fatalf("EscapeJSON(%s) = %s: 디코드 %q, 기대 %q", doc, got, now[0], nulesc.Escape(old[0]))
		case !hasNUL && (got != doc || changes != nil):
			t.Fatalf("U+0000 없는 %s 를 %s 로 바꿨다", doc, got)
		case hasNUL && len(changes) != 1:
			t.Fatalf("%s 의 고친 자리 %+v", doc, changes)
		}
		if hasNUL {
			if prev, ok := seen[got]; ok {
				t.Fatalf("%s 와 %s 가 같은 %s 가 된다", prev, doc, got)
			}
			seen[got] = doc
		}
		if depth == 4 {
			return
		}
		for _, u := range units {
			walk(tok+u, depth+1)
		}
	}
	walk("", 0)
	if want := 1 + 10 + 100 + 1000 + 10000; n != want {
		t.Fatalf("잰 토큰 %d 개, 기대 %d 개", n, want)
	}
}
