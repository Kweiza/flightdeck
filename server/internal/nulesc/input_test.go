package nulesc_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/nulesc"
)

type inner struct {
	Name string `json:"name"`
	Note string `json:"note" nul:"text"`
}

type Embedded struct {
	Tag string `nul:"text"`
}

// hiddenEmbed 는 내보내지 않은 타입의 내장 칸이다 - encoding/json 은 그 안의 내보낸 칸(who)을 바깥 칸으로 채운다.
type hiddenEmbed struct {
	Who string `json:"who"`
}

type form struct {
	ID     string            `json:"id"`
	Title  string            `json:"title" nul:"text"`
	Body   *string           `json:"body" nul:"text"`
	Paths  *[]string         `json:"paths" nul:"text"`
	Labels []string          `json:"labels" nul:"text"`
	Refs   []string          `json:"refs"`
	Kids   []inner           `json:"kids"`
	Extra  json.RawMessage   `json:"extra" nul:"text"`
	Meta   json.RawMessage   `json:"meta"`
	Attrs  map[string]int    `json:"attrs"`
	Notes  map[string]string `json:"notes" nul:"text"`
	Any    any               `json:"any" nul:"text"`
	Bytes  []byte            `json:"bytes"`
	Skip   string            `json:"-" nul:"text"`
	Embedded
	hiddenEmbed
	hidden string
}

// 자유 글 칸(표지 · 포인터 · 조각 · 안쪽 구조체 · 펼친 내장 칸 · 맵 값 · 인터페이스 · RawMessage)의 NUL 은 받는다. 입력은 한 바이트도
// 바꾸지 않는다 - 원장의 꼴로 옮기는 것은 store 의 쓰기 관문이다.
func TestFieldsAcceptsTextWithoutTouching(t *testing.T) {
	body := "b\x00\\x00"
	paths := []string{"ok", "p\x00"}
	f := form{
		ID: "id-1", Title: "t\x00", Body: &body, Paths: &paths,
		Labels: []string{"l\x00", "plain"}, Refs: []string{"r"},
		Kids:  []inner{{Name: "n", Note: "k\x00"}},
		Extra: json.RawMessage(`{"a":"x\u0000","b":"y"}`),
		Meta:  json.RawMessage(`{"a":"plain"}`),
		Attrs: map[string]int{"k": 1}, Notes: map[string]string{"k": "v\x00"},
		Any:   map[string]any{"k": []any{"a\x00"}},
		Bytes: []byte{0, 0xff},
		Skip:  "s\x00", Embedded: Embedded{Tag: "g\x00"},
		hidden: "h\x00\xff",
	}
	before := deepCopy(t, f)
	if err := nulesc.Fields(&f); err != nil {
		t.Fatalf("Fields: %v", err)
	}
	if !reflect.DeepEqual(deepCopy(t, f), before) || *f.Body != "b\x00\\x00" || (*f.Paths)[1] != "p\x00" || f.hidden != "h\x00\xff" {
		t.Fatalf("Fields 가 입력을 바꿨다: %+v", f)
	}
}

// 이름 자리의 NUL · 맵 키의 NUL · RawMessage 의 짝 없는 서로게이트 · 깨진 UTF-8 은 *Refusal 이다. 내보내지 않은 타입의 내장 칸도
// 걷는다(encoding/json 이 그 안의 칸을 채우므로 건너뛰면 그 이름이 재지 않고 지나간다).
func TestFieldsRefuses(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*form)
		err     error
		pointer string
	}{
		{"식별자", func(f *form) { f.ID = "i\x00" }, nulesc.ErrNUL, "/id"},
		{"이름 조각", func(f *form) { f.Refs = []string{"a", "r\x00"} }, nulesc.ErrNUL, "/refs/1"},
		{"안쪽 이름", func(f *form) { f.Kids = []inner{{Name: "n\x00"}} }, nulesc.ErrNUL, "/kids/0/name"},
		{"내보내지 않은 내장 칸의 이름", func(f *form) { f.Who = "w\x00" }, nulesc.ErrNUL, "/who"},
		{"맵 키", func(f *form) { f.Attrs = map[string]int{"b": 1, "k\x00": 2} }, nulesc.ErrKeyNUL, "/attrs/k\x00"},
		{"자유 글 맵의 키", func(f *form) { f.Notes = map[string]string{"k\x00": "v"} }, nulesc.ErrKeyNUL, "/notes/k\x00"},
		{"이름 RawMessage 의 U+0000", func(f *form) { f.Meta = json.RawMessage(`{"a":["x","y\u0000"]}`) }, nulesc.ErrNUL, "/meta/a/1"},
		{"RawMessage 의 짝 없는 서로게이트", func(f *form) { f.Extra = json.RawMessage(`{"a":"\ud800"}`) }, nulesc.ErrSurrogate, "/extra/a"},
		{"RawMessage 키의 U+0000", func(f *form) { f.Extra = json.RawMessage(`{"a\u0000":"x"}`) }, nulesc.ErrKeyNUL, "/extra/a\x00"},
		{"자유 글의 깨진 UTF-8", func(f *form) { f.Labels = []string{"ok", "l\xff"} }, nulesc.ErrUTF8, "/labels/1"},
		{"이름의 깨진 UTF-8", func(f *form) { f.ID = "i\xc3" }, nulesc.ErrUTF8, "/id"},
		{"인터페이스 안의 깨진 UTF-8", func(f *form) { f.Any = []any{"a", "\xed\xa0\x80"} }, nulesc.ErrUTF8, "/any/1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := form{Title: "t\x00", Labels: []string{"l\x00"}}
			c.mutate(&f)
			err := nulesc.Fields(&f)
			var r *nulesc.Refusal
			if !errors.As(err, &r) || !errors.Is(err, c.err) {
				t.Fatalf("Fields = %v, 바라는 것 %v 거절", err, c.err)
			}
			if r.Pointer != c.pointer {
				t.Errorf("거절 자리 = %q, 바라는 것 %q", r.Pointer, c.pointer)
			}
		})
	}
}

// 구조체를 가리키는 포인터가 아니거나 모르는 표지 값이면 *Refusal 이 아닌 오류다(부르는 쪽의 결함 - 400 이 아니라 500 으로 간다).
func TestFieldsCallerDefects(t *testing.T) {
	type badTag struct {
		Title string `nul:"txt"`
	}
	for name, v := range map[string]any{
		"구조체 값":   form{},
		"nil 포인터": (*form)(nil),
		"글 포인터":   new(string),
		"모르는 표지":  &badTag{Title: "t"},
	} {
		err := nulesc.Fields(v)
		var r *nulesc.Refusal
		if err == nil || errors.As(err, &r) {
			t.Errorf("%s: err = %v, 바라는 것 부르는 쪽 결함 오류", name, err)
		}
	}
}

// Input 은 푼 값에 남지 않는 것(풀기가 U+FFFD 로 바꾼 짝 없는 서로게이트 · 깨진 UTF-8, 버린 모르는 키의 U+0000)을 원문에서 재
// 거절하고, 원문에 U+0000 단위가 있을 때만 Fields 다. 모르는 칸의 값 안 U+0000 은 저장되지 않으므로 거절하지 않는다.
func TestInputChecksTheRawDocumentFirst(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		err     error
		pointer string
	}{
		{"짝 없는 서로게이트(아는 칸)", `{"title":"a\udc00"}`, nulesc.ErrSurrogate, "/title"},
		{"짝 없는 서로게이트(모르는 칸 · 소문자 아님)", `{"zzz":"a\uD800"}`, nulesc.ErrSurrogate, "/zzz"},
		{"모르는 칸의 키 U+0000", `{"zzz":{"k\u0000":1}}`, nulesc.ErrKeyNUL, "/zzz/k\x00"},
		{"깨진 UTF-8", "{\"title\":\"a\xff\"}", nulesc.ErrUTF8, "바이트 11"},
		{"모르는 칸의 값 U+0000 은 받는다", `{"zzz":"v\u0000","title":"t\u0000"}`, nil, ""},
		{"짝 맞은 서로게이트는 받는다", `{"title":"\ud83d\ude00"}`, nil, ""},
		{"이름 칸", `{"id":"i\u0000"}`, nulesc.ErrNUL, "/id"},
		{"내보내지 않은 내장 칸의 이름", `{"who":"w\u0000"}`, nulesc.ErrNUL, "/who"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var f form
			_ = json.Unmarshal([]byte(c.doc), &f) // 깨진 UTF-8 도 풀기는 U+FFFD 로 지난다
			err := nulesc.Input([]byte(c.doc), &f)
			if c.err == nil {
				if err != nil {
					t.Fatalf("Input = %v, 바라는 것 받음", err)
				}
				return
			}
			var r *nulesc.Refusal
			if !errors.As(err, &r) || !errors.Is(err, c.err) || r.Pointer != c.pointer {
				t.Fatalf("Input = %v, 바라는 것 %v 거절(자리 %q)", err, c.err, c.pointer)
			}
		})
	}
}

// 보통 요청(U+0000 · 서로게이트 단위가 없는 문서)은 바이트를 훑기만 한다: 할당이 없고, 값을 걷지 않는다. Go 클라이언트가 `<>&` 를
// 보내는 꼴(`\u003c`)이 가득한 1 MiB 문서로 잰다 - 「`\u` 가 있다」가 문턱이면 이 문서가 느린 길을 탄다.
func TestInputFastPath(t *testing.T) {
	doc := bigDoc(1 << 20)
	var f form
	if err := json.Unmarshal(doc, &f); err != nil {
		t.Fatalf("풀기: %v", err)
	}
	if n := testing.AllocsPerRun(5, func() {
		if err := nulesc.Input(doc, &f); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("1 MiB 보통 문서의 Input 할당 = %v, 바라는 것 0", n)
	}
	// 원문에 U+0000 단위가 없으면 값을 걷지 않는다(이름 칸의 NUL 은 그 단위 없이는 풀기에서 못 나온다).
	f.ID = "i\x00"
	if err := nulesc.Input(doc, &f); err != nil {
		t.Fatalf("U+0000 단위 없는 문서에서 값을 걸었다: %v", err)
	}
}

func BenchmarkInput1MiB(b *testing.B) {
	doc := bigDoc(1 << 20)
	var f form
	if err := json.Unmarshal(doc, &f); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(doc)))
	b.ReportAllocs()
	for b.Loop() {
		if err := nulesc.Input(doc, &f); err != nil {
			b.Fatal(err)
		}
	}
}

// 원장의 꼴: NUL 이 없으면 그대로, 있으면 Escape. 비교는 두 쪽에 다 건다 - 바뀐 글과 처음부터 `\x00` 네 글자인 글은 같은 값이다.
func TestTextAndSame(t *testing.T) {
	for _, s := range []string{"", "plain", `a\x00`, `\\x00`} {
		if got := nulesc.Text(s); got != s {
			t.Errorf("Text(%q) = %q, NUL 없는 글은 그대로", s, got)
		}
	}
	if got := nulesc.Text("a\x00"); got != nulesc.Escape("a\x00") || nulesc.Text(got) != got {
		t.Errorf("Text(a NUL) = %q, 바라는 것 Escape 이고 두 번 걸어도 같다", got)
	}
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"a\x00", "a\x00", true},
		{"a\x00", `a\x00`, true},
		{"a\x00", "a", false},
		{`a\x00`, `a\\x00`, false},
		{"a\x00", `a\\x00`, false},
	} {
		if got := nulesc.Same(c.a, c.b); got != c.same {
			t.Errorf("Same(%q, %q) = %v, 바라는 것 %v", c.a, c.b, got, c.same)
		}
	}
}

// bigDoc 은 n 바이트쯤의 보통 요청 문서다: 자유 글이 `\u003c` · `\u0026` · `\n` 꼴과 한글로 가득하다.
func bigDoc(n int) []byte {
	var body strings.Builder
	for body.Len() < n {
		body.WriteString(`\u003ctag\u003e 가나다 \u0026amp; \"q\" \\ \n`)
	}
	var doc bytes.Buffer
	doc.WriteString(`{"id":"x","title":"t","body":"`)
	doc.WriteString(body.String())
	doc.WriteString(`","labels":["a\u003cb"]}`)
	return doc.Bytes()
}

func deepCopy(t *testing.T, f form) string {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
