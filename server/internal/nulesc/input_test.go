package nulesc_test

import (
	"encoding/json"
	"errors"
	"reflect"
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

type form struct {
	ID     string          `json:"id"`
	Title  string          `json:"title" nul:"text"`
	Body   *string         `json:"body" nul:"text"`
	Paths  *[]string       `json:"paths" nul:"text"`
	Labels []string        `json:"labels" nul:"text"`
	Refs   []string        `json:"refs"`
	Kids   []inner         `json:"kids"`
	Extra  json.RawMessage `json:"extra" nul:"text"`
	Meta   json.RawMessage `json:"meta"`
	Attrs  map[string]int  `json:"attrs"`
	Any    any             `json:"any" nul:"text"`
	Bytes  []byte          `json:"bytes"`
	Skip   string          `json:"-" nul:"text"`
	Embedded
	hidden string
}

// 자유 글 칸(표지 · 포인터 · 조각 · 안쪽 구조체 · 펼친 내장 칸 · RawMessage)의 NUL 은 Escape 와 같은 바이트로 제자리에서 바뀌고,
// 바뀐 자리를 걸은 차례로 낸다. 이름 칸 · 바꿀 수 없는 칸은 손대지 않는다.
func TestFieldsEscapesTextInPlace(t *testing.T) {
	body := "b\x00\\x00"
	paths := []string{"ok", "p\x00"}
	f := form{
		ID: "id-1", Title: "t\x00", Body: &body, Paths: &paths,
		Labels: []string{"l\x00", "plain"}, Refs: []string{"r"},
		Kids:  []inner{{Name: "n", Note: "k\x00"}},
		Extra: json.RawMessage(`{"a":"x\u0000","b":"y"}`),
		Meta:  json.RawMessage(`{"a":"plain"}`),
		Attrs: map[string]int{"k": 1},
		Bytes: []byte{0},
		Skip:  "s\x00", Embedded: Embedded{Tag: "g\x00"},
		hidden: "h\x00",
	}
	at, err := nulesc.Fields(&f)
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	want := []string{"/title", "/body", "/paths/1", "/labels/0", "/kids/0/note", "/extra/a", "/Skip", "/Tag"}
	if !reflect.DeepEqual(at, want) {
		t.Fatalf("바꾼 자리 = %q, 바라는 것 %q", at, want)
	}
	checks := []struct{ got, want string }{
		{f.Title, nulesc.Escape("t\x00")},
		{*f.Body, nulesc.Escape("b\x00\\x00")},
		{(*f.Paths)[0], "ok"},
		{(*f.Paths)[1], nulesc.Escape("p\x00")},
		{f.Labels[0], nulesc.Escape("l\x00")},
		{f.Labels[1], "plain"},
		{f.Kids[0].Note, nulesc.Escape("k\x00")},
		{f.Skip, nulesc.Escape("s\x00")},
		{f.Tag, nulesc.Escape("g\x00")},
		{f.hidden, "h\x00"},
		{string(f.Meta), `{"a":"plain"}`},
	}
	for i, c := range checks {
		if c.got != c.want {
			t.Errorf("자리 %d: %q, 바라는 것 %q", i, c.got, c.want)
		}
	}
	wantExtra, _, _ := nulesc.EscapeJSON(`{"a":"x\u0000","b":"y"}`)
	if string(f.Extra) != wantExtra {
		t.Errorf("Extra = %s, 바라는 것 %s(EscapeJSON)", f.Extra, wantExtra)
	}
	if string(f.Bytes) != "\x00" {
		t.Errorf("바이트 조각을 바꿨다: %q", f.Bytes)
	}
}

// 이름 자리의 NUL · 맵 키의 NUL · 인터페이스 안 · 맵 값 · RawMessage 의 짝 없는 서로게이트는 *Refusal 이고, 그때 v 의 어느 칸도
// 바뀌지 않는다(앞에서 이미 바꿀 자리로 잡은 자유 글 칸도).
func TestFieldsRefusesNamesWithoutTouching(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*form)
		err     error
		pointer string
	}{
		{"식별자", func(f *form) { f.ID = "i\x00" }, nulesc.ErrNUL, "/id"},
		{"이름 조각", func(f *form) { f.Refs = []string{"a", "r\x00"} }, nulesc.ErrNUL, "/refs/1"},
		{"안쪽 이름", func(f *form) { f.Kids = []inner{{Name: "n\x00"}} }, nulesc.ErrNUL, "/kids/0/name"},
		{"맵 키", func(f *form) { f.Attrs = map[string]int{"b": 1, "k\x00": 2} }, nulesc.ErrKeyNUL, "/attrs/k\x00"},
		{"표지 단 인터페이스 안", func(f *form) { f.Any = "a\x00" }, nulesc.ErrNUL, "/any"},
		{"표지 단 인터페이스 안의 맵 값", func(f *form) { f.Any = map[string]string{"k": "v\x00"} }, nulesc.ErrNUL, "/any/k"},
		{"이름 RawMessage 의 U+0000", func(f *form) { f.Meta = json.RawMessage(`{"a":["x","y\u0000"]}`) }, nulesc.ErrNUL, "/meta/a/1"},
		{"RawMessage 의 짝 없는 서로게이트", func(f *form) { f.Extra = json.RawMessage(`{"a":"\ud800"}`) }, nulesc.ErrSurrogate, "/extra/a"},
		{"RawMessage 키의 U+0000", func(f *form) { f.Extra = json.RawMessage(`{"a\u0000":"x"}`) }, nulesc.ErrKeyNUL, "/extra/a\x00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := form{Title: "t\x00", Labels: []string{"l\x00"}}
			c.mutate(&f)
			at, err := nulesc.Fields(&f)
			var r *nulesc.Refusal
			if !errors.As(err, &r) || !errors.Is(err, c.err) {
				t.Fatalf("Fields = (%q, %v), 바라는 것 %v 거절", at, err, c.err)
			}
			if r.Pointer != c.pointer {
				t.Errorf("거절 자리 = %q, 바라는 것 %q", r.Pointer, c.pointer)
			}
			if f.Title != "t\x00" || f.Labels[0] != "l\x00" {
				t.Errorf("거절했는데 자유 글을 바꿨다: title %q · labels %q", f.Title, f.Labels)
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
		_, err := nulesc.Fields(v)
		var r *nulesc.Refusal
		if err == nil || errors.As(err, &r) {
			t.Errorf("%s: err = %v, 바라는 것 부르는 쪽 결함 오류", name, err)
		}
	}
}

// Input 은 푼 값에 남지 않는 것(풀기가 U+FFFD 로 바꾼 짝 없는 서로게이트, 버린 모르는 키의 U+0000)을 원문에서 재 거절하고, 그다음
// Fields 다. 모르는 칸의 값 안 U+0000 은 저장되지 않으므로 거절하지 않는다.
func TestInputChecksTheRawDocumentFirst(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		err     error
		pointer string
		title   string
	}{
		{"짝 없는 서로게이트(아는 칸)", `{"title":"a\udc00"}`, nulesc.ErrSurrogate, "/title", ""},
		{"짝 없는 서로게이트(모르는 칸)", `{"zzz":"a\ud800"}`, nulesc.ErrSurrogate, "/zzz", ""},
		{"모르는 칸의 키 U+0000", `{"zzz":{"k\u0000":1}}`, nulesc.ErrKeyNUL, "/zzz/k\x00", ""},
		{"모르는 칸의 값 U+0000 은 받는다", `{"zzz":"v\u0000","title":"t\u0000"}`, nil, "", nulesc.Escape("t\x00")},
		{"이름 칸", `{"id":"i\u0000"}`, nulesc.ErrNUL, "/id", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var f form
			if err := json.Unmarshal([]byte(c.doc), &f); err != nil {
				t.Fatalf("풀기: %v", err)
			}
			_, err := nulesc.Input(c.doc, &f)
			if c.err == nil {
				if err != nil || f.Title != c.title {
					t.Fatalf("Input = %v, title %q, 바라는 것 받음 · %q", err, f.Title, c.title)
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
