// Package nulesc 는 Postgres text 가 못 담는 NUL(U+0000)을 되돌릴 수 있는 `\x00` 표기로 옮기는 규칙이다: 글 하나에 거는 Escape 와,
// JSON 문서 안에서 U+0000 이 든 글 값 토큰만 원문 바이트에서 고치는 EscapeJSON.
//
// ★ 바꿔 넣기는 자유 글에만 건다. 식별자(이름으로 다른 것을 가리키는 글 - JSON 객체의 키가 그 하나다)의 NUL 은 바꾸지 않고
// 거절한다: 바꾼 이름이 원래 `\x00` 네 글자를 담은 다른 이름과 같아지면 이름 공간에서 두 값이 하나가 된다. 원장의 식별자 칸은
// 이관 카탈로그(store/pg 의 Column.Identifier)가 가르고, JSON 키는 EscapeJSON 이 ErrKeyNUL 로 거절한다.
//
// ★ 표준 라이브러리만 쓰는 순수 함수다. SQLite 원장을 Postgres 로 옮기는 이관(store/pg 의 Cell)이 쓰고, Postgres 원장에 새로 쓰는
// 길도 같은 규칙을 그대로 가져다 쓸 수 있게 이관 카탈로그(store/pg)와 떼어 둔다.
package nulesc

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// mark 는 Escape 가 NUL 자리에 쓰는 표기의 백슬래시 뒤 글자다. 표기는 `\` + mark - 바이트 이스케이프를 읽는 사람이 NUL 로
// 알아보는 꼴이다.
const mark = "x00"

// Escape 는 NUL 바이트가 든 글 s 를 NUL 없는 글로 옮긴다. NUL 은 `\x00` 네 글자가 되고, 원래 있던 백슬래시 묶음(연달은
// 백슬래시)은 바로 뒤가 NUL 이나 「x00」일 때만 두 배가 된다. 그 밖의 바이트는 그대로다. 글 하나 단위로 건다 - 이관은 글 칸이면
// NUL 이 든 칸에, JSON 칸이면 U+0000 이 든 글 토큰에만 건다(EscapeJSON). NUL 이 없는 글은 한 바이트도 안 바뀐다.
//
// ★ 되돌릴 수 있다(단사): 바뀐 글에서 「x00」 바로 앞의 백슬래시 묶음이 홀수 개(2k+1)면 원래 k 개 백슬래시 + NUL 이었고,
// 짝수 개(2k)면 k 개 백슬래시 + 글자 「x00」이었다. 그 밖의 백슬래시 묶음은 그대로다. 규칙은 README 「SQLite 원장을 Postgres 로
// 옮기기」가 사람에게 적고, nulesc_test.go 가 그 규칙으로 되돌려 맞댄다.
// ★ 되돌리는 단위는 바뀐 글이다. NUL 이 없어 그대로 둔 글에 원래 있던 `\x00` 네 글자는 바뀐 글의 그것과 저장값으로 갈리지 않는다 -
// 어느 글이 바뀌었는지는 이관이 남기는 기록 파일(<원본>.rewritten-<시각>.jsonl)이 정본이다.
// ★ NUL 만 `\x00` 로 바꾸면 원래 그 네 글자가 있던 글에서 어느 것이 NUL 이었는지 잃는다. NUL 을 인용하는 글은 `\x00` 표기도
// 함께 쓰기 쉬워 그런 글이 드물지 않다 - 그 글을 거절하면 바로 그런 글 때문에 손 SQL 단계가 다시 생긴다. 백슬래시를 전부 두 배로
// 하는 흔한 이스케이프도 되돌릴 수 있지만 NUL 과 무관한 표기(`\n` · `\x1f`)까지 바꾼다. 그래서 갈릴 자리의 백슬래시만 두 배다.
func Escape(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 3*strings.Count(s, "\x00"))
	for i := 0; i < len(s); {
		switch s[i] {
		case 0:
			b.WriteString(`\` + mark)
			i++
		case '\\':
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			n := j - i
			if j < len(s) && (s[j] == 0 || strings.HasPrefix(s[j:], mark)) {
				n *= 2
			}
			b.WriteString(strings.Repeat(`\`, n))
			i = j
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

var (
	// ErrSyntax: 받은 글이 JSON 문서가 아니다.
	ErrSyntax = errors.New("JSON 문서가 아니다")
	// ErrSurrogate: 글에 짝 없는 서로게이트 이스케이프(홀로 선 `\ud800` · `\udc00`)가 있다. jsonb 는 그 문서를 거절하고, 디코더는
	// 그것을 U+FFFD 로 바꿔 다른 두 글을 같게 만든다.
	ErrSurrogate = errors.New("짝 없는 서로게이트")
	// ErrKeyNUL: 객체의 키(멤버 이름)에 U+0000 이 있다. 키는 바꾸지 않는다 - 바꾼 키가 같은 객체에서 원래 `\x00` 네 글자를 담은
	// 옆 키와 같아지면(`{"\u0000":1,"\\x00":2}`) 두 멤버가 한 이름이 되고, jsonb 는 그중 하나를 버린다.
	ErrKeyNUL = errors.New("키 안의 U+0000")
)

// Refusal 은 EscapeJSON 이 문서를 거절한 글 하나의 자리다. 갈래(ErrSurrogate · ErrKeyNUL)는 errors.Is 로 잰다.
type Refusal struct {
	Err error
	// Pointer 는 그 글의 자리(RFC 6901 JSON Pointer - 원래 문서의 키 · 배열 번호)다. 글이 키면 그 키를 이름으로 가진 멤버의
	// 자리다(짝 없는 서로게이트 단위는 U+FFFD 로 읽는다).
	Pointer string
}

func (r *Refusal) Error() string { return fmt.Sprintf("%v(글 자리 %q)", r.Err, r.Pointer) }
func (r *Refusal) Unwrap() error { return r.Err }

// EscapeJSON 은 JSON 문서 doc 에서 U+0000 이 든 글 값 토큰만 고친 문서와 고친 글의 자리(RFC 6901 JSON Pointer - 원래 문서의 키 ·
// 배열 번호)를 낸다. 고친 토큰을 디코드하면 원래 값에 Escape 를 건 값이다. 문서를 디코드해 다시 쓰지 않는다 - 원문 바이트에서 그
// 토큰의 이스케이프 단위만 바꾼다:
//
//   - U+0000 단위(`\u0000`)는 `\\x00` 이 된다(디코드하면 `\x00` 네 글자).
//   - 백슬래시 단위(`\\` · `\u005c`)의 묶음은 바로 뒤 단위가 U+0000 이거나 「x00」 세 글자일 때 각 단위를 두 번 쓴다.
//   - 그 밖의 단위, U+0000 이 없는 토큰, 토큰 밖(공백 · 수 · 순서)은 원래 바이트다.
//
// U+0000 이 없는 문서는 받은 그대로 낸다(고친 자리가 없다). 문서가 아니면 ErrSyntax, 어느 글에든 짝 없는 서로게이트가 있거나
// 어느 키에든 U+0000 이 있으면 그 글의 자리를 담은 *Refusal(ErrSurrogate · ErrKeyNUL - 앞에서부터 처음 만난 것)이고, 그때도
// 문서는 받은 그대로다.
//
// ★ 고치는 단위가 글 토큰 하나다. 같은 문서의 다른 글은 Escape 를 타지 않는다 - U+0000 이 없는 문서에 든 같은 글과 같은 바이트로
// 남는다(감사 이벤트의 after 와 그 행의 칸이 옮긴 뒤에도 같은 값이다).
// ★ 디코드 · 재부호화를 하지 않는 까닭: 디코더는 짝 없는 서로게이트를 U+FFFD 로 바꿔 다른 두 키를 하나로 만들고, 다시 쓰면 같은
// 값의 다른 이스케이프(`\u0041` · `A`)가 한 꼴이 되어 원래 바이트를 잃는다.
func EscapeJSON(doc string) (string, []string, error) {
	if !json.Valid([]byte(doc)) {
		return doc, nil, ErrSyntax
	}
	if !strings.Contains(doc, `\u`) { // `\u` 단위가 없으면 U+0000 도 서로게이트도 없다
		return doc, nil, nil
	}
	w := &walker{s: doc}
	if err := w.value(); err != nil {
		return doc, nil, err
	}
	if len(w.changes) == 0 {
		return doc, nil, nil
	}
	w.out.WriteString(doc[w.at:])
	return w.out.String(), w.changes, nil
}

// walker 는 json.Valid 를 지난 문서를 앞에서부터 한 번 걷는다. 글 토큰마다 단위를 읽어 서로게이트 · 키의 U+0000 을 재고, U+0000 이
// 든 값 토큰은 고친 바이트를 out 에 쓴다. at 은 out 으로 아직 옮기지 않은 원문의 시작이고, path 는 지금 자리의 키 · 배열 번호다.
type walker struct {
	s       string
	i       int
	out     strings.Builder
	at      int
	path    []string
	changes []string
}

func (w *walker) space() {
	for w.i < len(w.s) && strings.IndexByte(" \t\n\r", w.s[w.i]) >= 0 {
		w.i++
	}
}

func (w *walker) value() error {
	w.space()
	switch w.s[w.i] {
	case '{':
		return w.object()
	case '[':
		return w.array()
	case '"':
		_, err := w.token(false)
		return err
	}
	for w.i < len(w.s) && strings.IndexByte(",]} \t\n\r", w.s[w.i]) < 0 { // 수 · true · false · null
		w.i++
	}
	return nil
}

func (w *walker) object() error {
	w.i++ // {
	w.space()
	if w.s[w.i] == '}' {
		w.i++
		return nil
	}
	for {
		w.space()
		name, err := w.token(true)
		if err != nil {
			return err
		}
		w.space()
		w.i++ // :
		w.path = append(w.path, name)
		err = w.value()
		w.path = w.path[:len(w.path)-1]
		if err != nil {
			return err
		}
		w.space()
		w.i++ // , 또는 }
		if w.s[w.i-1] == '}' {
			return nil
		}
	}
}

func (w *walker) array() error {
	w.i++ // [
	w.space()
	if w.s[w.i] == ']' {
		w.i++
		return nil
	}
	for n := 0; ; n++ {
		w.path = append(w.path, strconv.Itoa(n))
		err := w.value()
		w.path = w.path[:len(w.path)-1]
		if err != nil {
			return err
		}
		w.space()
		w.i++ // , 또는 ]
		if w.s[w.i-1] == ']' {
			return nil
		}
	}
}

// unit 은 글 토큰 안의 이스케이프 단위 하나다: 날 바이트 하나, `\X` 두 바이트, `\uXXXX` 여섯 바이트. r 은 디코드한 글자다 -
// `\uXXXX` 는 UTF-16 단위 하나, 날 바이트는 그 바이트 값이다(0x80 이상은 여러 바이트 글자의 한 조각이라 어떤 글자와도 같지 않다).
type unit struct {
	at, end int
	r       rune
}

func (u unit) raw() bool { return u.end-u.at == 1 }
func (u unit) u() bool   { return u.end-u.at == 6 }

// shortEscapes 는 `\X` 단위가 디코드하는 글자다(JSON 문법의 전부).
var shortEscapes = map[byte]rune{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}

// token 은 w.i 의 글 토큰을 읽고 디코드한 값을 낸다(짝 없는 서로게이트 단위는 U+FFFD 로 읽는다). 짝 없는 서로게이트가 있거나 key
// (그 토큰이 멤버 이름이다)에 U+0000 이 있으면 그 글의 자리를 담은 *Refusal 이다. 값에 U+0000 이 있으면 그 토큰을 고쳐 쓰고
// 자리를 changes 에 싣는다.
func (w *walker) token(key bool) (string, error) {
	start := w.i
	w.i++ // 여는 따옴표
	var units []unit
	for w.s[w.i] != '"' {
		u := unit{at: w.i}
		switch {
		case w.s[w.i] != '\\':
			u.r = rune(w.s[w.i])
			w.i++
		case w.s[w.i+1] == 'u':
			v, _ := strconv.ParseUint(w.s[w.i+2:w.i+6], 16, 16) // json.Valid 를 지난 문서라 16진 네 자리다
			u.r = rune(v)
			w.i += 6
		default:
			u.r = shortEscapes[w.s[w.i+1]]
			w.i += 2
		}
		u.end = w.i
		units = append(units, u)
	}
	w.i++ // 닫는 따옴표
	var dec strings.Builder
	lone, nul := false, false
	for k := 0; k < len(units); k++ {
		u := units[k]
		switch {
		case u.raw():
			dec.WriteByte(w.s[u.at])
		case u.u() && utf16.IsSurrogate(u.r):
			if u.r < 0xDC00 && k+1 < len(units) && units[k+1].u() && units[k+1].r >= 0xDC00 && units[k+1].r <= 0xDFFF {
				dec.WriteRune(utf16.DecodeRune(u.r, units[k+1].r))
				k++
				continue
			}
			lone = true
			dec.WriteRune(utf8.RuneError)
		default:
			nul = nul || u.r == 0
			dec.WriteRune(u.r)
		}
	}
	at := w.path
	if key {
		at = append(w.path[:len(w.path):len(w.path)], dec.String())
	}
	switch {
	case lone:
		return "", &Refusal{Err: ErrSurrogate, Pointer: pointer(at)}
	case nul && key:
		return "", &Refusal{Err: ErrKeyNUL, Pointer: pointer(at)}
	case nul:
		w.rewrite(start, units)
		w.changes = append(w.changes, pointer(at))
	}
	return dec.String(), nil
}

// rewrite 는 start 에서 여는 글 토큰(단위 units)을 고쳐 out 에 쓴다 - 디코드한 값이 원래 값에 Escape 를 건 값이 되게. 올바른
// 문서의 날 바이트는 NUL 도 백슬래시도 아니므로 U+0000 · 백슬래시 단위는 이스케이프다.
func (w *walker) rewrite(start int, units []unit) {
	w.out.WriteString(w.s[w.at : start+1])
	for k := 0; k < len(units); k++ {
		u := units[k]
		switch u.r {
		case 0:
			w.out.WriteString(`\\` + mark)
		case '\\':
			m := k
			for m < len(units) && units[m].r == '\\' {
				m++
			}
			times := 1
			if m < len(units) && (units[m].r == 0 || followsMark(units[m:])) {
				times = 2
			}
			for _, b := range units[k:m] {
				w.out.WriteString(strings.Repeat(w.s[b.at:b.end], times))
			}
			k = m - 1
		default:
			w.out.WriteString(w.s[u.at:u.end])
		}
	}
	w.out.WriteByte('"')
	w.at = w.i
}

// followsMark 는 단위 units 가 「x00」 세 글자로 시작하는가다(날 바이트든 이스케이프든 디코드한 글자로 잰다).
func followsMark(units []unit) bool {
	if len(units) < len(mark) {
		return false
	}
	for i := range len(mark) {
		if units[i].r != rune(mark[i]) {
			return false
		}
	}
	return true
}

var pointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

// pointer 는 path 의 RFC 6901 JSON Pointer 다(문서 전체는 빈 글).
func pointer(path []string) string {
	var b strings.Builder
	for _, p := range path {
		b.WriteByte('/')
		b.WriteString(pointerEscaper.Replace(p))
	}
	return b.String()
}
