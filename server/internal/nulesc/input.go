package nulesc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 서버의 쓰기 길이 받는 입력에 거는 규칙 - **거절만 한다.** 받은 글은 한 바이트도 바꾸지 않는다: 자유 글의 NUL 을 `\x00` 로
// 옮기는 것(Escape)은 입력이 아니라 원장에 담기는 글 하나(store 의 쓰기 관문이 이관 카탈로그로 거는 pg.Cell)의 일이다. 그래야
// 여러 입력을 합쳐 한 칸에 쓰는 자리(회수 판단 본문 = 행위자 + 사유)와 입력을 자르거나 재서 쓰는 자리(이벤트의 clip · 길이)가
// 이관이 SQLite 원본의 그 칸 하나에 건 것과 같은 바이트가 되고, 길이 · 비교는 받은 원문으로 잰다.
//
// 여기서 거절하는 것은 원장에 닿기 전에 흔적이 사라지거나 다른 것을 가리키게 되는 글이다: 이름 자리의 NUL(바꾸면 원래 `\x00`
// 네 글자인 다른 이름이 된다), JSON 키의 U+0000, 짝 없는 서로게이트와 깨진 UTF-8(풀기가 U+FFFD 로 바꿔 다른 글과 같아진다).

var (
	// ErrNUL: 자유 글 표지(`nul:"text"`)가 없는 자리의 NUL. 그 자리는 이름이다 - 식별자(이관 카탈로그의 식별자 칸에 들어가는
	// 항목 · 세션 · 프로젝트 id, 자원 이름), 열거(종류 · 상태), 서버가 좌표로 쓰는 경로(항목 · 발자국 · 작업 경로)와 git 이름처럼
	// 값이 곧 다른 것을 가리키는 글. 받으면 원장의 규칙이 원래 `\x00` 네 글자를 담은 다른 이름과 같게 만들거나(이관이 식별자
	// 칸 · JSON 키의 NUL 을 거절하는 것과 같은 까닭이다), 조회 열쇠면 Postgres 가 22021 로 떨군다.
	ErrNUL = errors.New("이름 자리의 NUL")
	// ErrUTF8: 깨진 UTF-8 바이트열. Postgres text 는 못 담고(22021), 이관은 그 칸을 거절하며(pg.FaultUTF8), JSON 풀기는 U+FFFD 로
	// 바꿔 다른 글과 같게 만든다.
	ErrUTF8 = errors.New("깨진 UTF-8")
)

// Text 는 자유 글 s 가 원장에 담기는 꼴이다: NUL 이 있으면 Escape, 없으면 s 그대로(한 바이트도 안 바뀐다). 이관이 자유 글 칸
// 하나 · JSON 글 토큰 하나에 거는 규칙이 이것이다(store/pg 의 Cell, EscapeJSON). 두 번 걸어도 같다 - 바뀐 글에는 NUL 이 없다.
//
// 원장은 이 꼴만 담으므로, 받은 글을 원장의 값과 맞댈 때는 두 쪽에 다 건다(Same) - 받은 원문과 원장 값을 그대로 맞대면 NUL 이 든
// 글은 자기 자신과 다르다.
func Text(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return Escape(s)
}

// Same 은 두 자유 글이 원장에서 같은 값인가다(Text 를 건 꼴이 같다). 바뀐 글과 처음부터 `\x00` 네 글자를 담은 글은 원장에서
// 같은 값이므로 같다고 본다 - Postgres 원장은 둘을 가를 바이트가 없고, 이관도 둘을 같은 값으로 옮긴다.
func Same(a, b string) bool { return a == b || Text(a) == Text(b) }

// tagKey 는 자유 글 표지의 구조체 표지 이름이고 tagText 는 그 값이다: `nul:"text"`.
const (
	tagKey  = "nul"
	tagText = "text"
)

var rawMessage = reflect.TypeFor[json.RawMessage]()

// Input 은 서버가 받은 JSON 문서 doc 와 그것을 풀어 낸 값 v(구조체를 가리키는 포인터)를 잰다. 요청 본문을 푸는 자리(api 의
// decode)가 부른다: 먼저 원문을 CheckDoc 으로 재고, 원문에 U+0000 단위(`\u0000`)가 있을 때만 v 에 Fields 를 건다 - JSON 글은
// 제어 문자를 날로 못 담으므로 그 단위가 없으면 풀린 값에 NUL 이 없다. 거절이면 *Refusal 이다. v 는 바꾸지 않는다.
//
// ★ 보통 요청은 바이트를 한 번 훑고 끝난다(할당 없음 - input_test.go 의 벤치 · 할당 시험이 1 MiB 로 잰다). Go 클라이언트는 `<>&`
// 를 `\u003c` 꼴로 보내므로 「`\u` 가 있다」를 느린 길의 문턱으로 쓰면 CLI · MCP 의 요청 대부분이 느린 길을 탄다 - 문턱은 U+0000
// · 서로게이트 단위다.
func Input(doc []byte, v any) error {
	nul, err := checkDoc(doc)
	if err != nil || !nul {
		return err
	}
	return Fields(v)
}

// CheckDoc 은 받은 JSON 문서 doc 의 원문을 잰다 - 풀기(encoding/json)가 흔적을 지우는 것들이다: 깨진 UTF-8 과 짝 없는
// 서로게이트(U+FFFD 로 바뀐다), 키의 U+0000(모르는 키는 버려지고 아는 키는 다른 이름이 된다). 거절이면 그 글의 자리를 담은
// *Refusal(ErrUTF8 · ErrSurrogate · ErrKeyNUL)이다. 값 안의 U+0000 은 거절하지 않는다 - 그 글이 자유 글인지는 문서가 아니라
// 그것을 받는 칸이 안다(Fields).
//
// 요청 본문(api 의 decode, Input 을 거쳐)과 MCP 도구 인자(mcpsrv 의 callTool - 풀기 전에, REST 로 넘기기 전에)가 이것 하나로 잰다.
func CheckDoc(doc []byte) error {
	_, err := checkDoc(doc)
	return err
}

// checkDoc 은 CheckDoc 이고, 원문에 U+0000 단위가 있는가(풀린 값에 NUL 이 있을 수 있는가)를 함께 낸다.
func checkDoc(doc []byte) (nul bool, err error) {
	if !utf8.Valid(doc) {
		return false, &Refusal{Err: ErrUTF8, Pointer: badUTF8At(doc)}
	}
	nul, sur := suspects(doc)
	if !nul && !sur {
		return false, nil
	}
	if _, _, err := EscapeJSON(string(doc)); err != nil {
		return nul, err
	}
	return nul, nil
}

// suspects 는 JSON 문서 doc 에 U+0000 단위(`\u0000`)와 서로게이트 단위(`\uD800`-`\uDFFF`, 대소문자 무관)가 있을 수 있는가다. 틀릴
// 때는 늘 있다는 쪽이다(역슬래시 묶음 뒤의 글자 `u0000` 도 있다고 본다) - 없다고 하면 정말 없다. 할당하지 않는다.
//
// 받은 문서(checkDoc - 바이트)와 원장에 담을 JSON 칸(EscapeJSON - 글)이 이 한 벌을 문턱으로 쓴다: 둘 다 그 단위가 없으면 문서를
// 걷지 않는다. Go 의 json.Marshal 은 `<>&` 를 `\u003c` 꼴로 쓰므로 「`\u` 가 있다」를 문턱으로 쓰면 보통 문서 대부분이 느린 길을 탄다.
func suspects[T string | []byte](doc T) (nul, sur bool) {
	for i := 0; ; {
		j := indexEscape(doc[i:])
		if j < 0 || nul && sur {
			return nul, sur
		}
		k := i + j + 2
		if k+4 <= len(doc) {
			h0, h1 := doc[k], doc[k+1]
			nul = nul || h0 == '0' && h1 == '0' && doc[k+2] == '0' && doc[k+3] == '0'
			sur = sur || (h0 == 'd' || h0 == 'D') && strings.IndexByte("89abcdefABCDEF", h1) >= 0
		}
		i = k
	}
}

// indexEscape 는 s 에서 처음 나온 `\u` 의 자리다(없으면 -1). 바이트 · 글 어느 쪽이든 옮겨 담지 않는다.
func indexEscape[T string | []byte](s T) int {
	if b, ok := any(s).([]byte); ok {
		return bytes.Index(b, []byte(`\u`))
	}
	return strings.Index(string(s), `\u`)
}

// badUTF8At 은 doc 에서 처음 깨진 UTF-8 이 나온 바이트 자리를 사람이 읽는 말로 낸다(JSON Pointer 를 잴 수 없는 원문이라).
func badUTF8At(doc []byte) string {
	for i := 0; i < len(doc); {
		r, n := utf8.DecodeRune(doc[i:])
		if r == utf8.RuneError && n == 1 {
			return "바이트 " + strconv.Itoa(i)
		}
		i += n
	}
	return ""
}

// Fields 는 입력 값 v(구조체를 가리키는 포인터)의 글을 잰다. 칸마다 표지로 가른다:
//
//   - `nul:"text"` 가 붙은 칸은 자유 글이다(제목 · 본문 · 사유 · 꼬리표처럼 사람이 쓰는 글). 그 칸에서 조각 · 포인터 · 맵 값 ·
//     인터페이스로 닿는 글의 NUL 은 받는다 - 원장에 담길 때 store 의 쓰기 관문이 그 칸의 규칙으로 바꾼다. json.RawMessage
//     칸이면 그 문서의 짝 없는 서로게이트 · 키의 U+0000 만 거절한다.
//   - 표지가 없는 칸의 글은 이름이다. NUL 이 있으면 그 자리의 *Refusal(ErrNUL)이다(json.RawMessage 칸이면 U+0000 이 든 글).
//   - 맵 키의 NUL 은 표지와 상관없이 *Refusal(ErrKeyNUL)이다 - 키는 이름이다.
//   - 깨진 UTF-8 은 어느 칸이든 *Refusal(ErrUTF8)이다(화면 폼은 JSON 풀기를 안 거쳐 깨진 바이트가 그대로 온다).
//
// 처음 만난 거절의 자리(RFC 6901 JSON Pointer - 칸 이름은 json 표지의 이름, 없으면 Go 이름. 표지 이름 없는 내장 칸은
// encoding/json 처럼 바깥 칸에 펼친다)를 낸다. v 는 바꾸지 않는다. 구조체를 가리키는 포인터가 아니거나 모르는 표지 값이면
// *Refusal 이 아닌 오류다(부르는 쪽의 결함이다).
//
// ★ 표지가 없으면 거절이 기본이다. 자유 글 칸에 표지를 빠뜨리면 NUL 이 든 입력이 400 으로 시끄럽게 걸리지만, 이름 칸을 자유
// 글로 잘못 두면 그 이름이 원장에서 다른 이름과 같아진다 - 틀릴 때 시끄러운 쪽을 기본으로 둔다.
// ★ 내보내지 않은 타입의 내장 칸도 걷는다. encoding/json 은 그 안의 내보낸 칸을 바깥 칸으로 채우므로, 건너뛰면 그 칸의 이름이
// 재지 않고 지나간다. 바이트 조각은 글이 아니라 재지 않는다.
func Fields(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("nulesc.Fields: 구조체를 가리키는 포인터가 아니다(%T)", v)
	}
	return walk(rv.Elem(), nil, false)
}

func walk(v reflect.Value, path []string, text bool) error {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return walk(v.Elem(), path, text)
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			name, inline := fieldName(f)
			if !f.IsExported() && !(inline && embedsStruct(f.Type)) {
				continue // encoding/json 이 안 채우는 칸
			}
			isText, err := textTag(f)
			if err != nil {
				return err
			}
			at := path
			if !inline {
				at = with(path, name)
			}
			if err := walk(v.Field(i), at, isText); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		switch {
		case v.Type() == rawMessage:
			return doc(v, path, text)
		case v.Type().Elem().Kind() == reflect.Uint8:
			return nil
		}
		for i := range v.Len() {
			if err := walk(v.Index(i), with(path, strconv.Itoa(i)), text); err != nil {
				return err
			}
		}
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(keyText(a), keyText(b)) })
		for _, k := range keys {
			at := with(path, keyText(k))
			if k.Kind() == reflect.String {
				if err := refuse(check(k.String(), ErrKeyNUL), at); err != nil {
					return err
				}
			}
			if err := walk(v.MapIndex(k), at, text); err != nil {
				return err
			}
		}
	case reflect.String:
		if text {
			return refuse(check(v.String(), nil), path)
		}
		return refuse(check(v.String(), ErrNUL), path)
	}
	return nil
}

// CheckName 은 이름 자리의 글 s 하나를 잰다: 깨진 UTF-8 이면 ErrUTF8, NUL 이 있으면 ErrNUL, 아니면 nil. URL 의 경로 · 질의처럼
// 구조체로 풀리지 않는 이름이 쓴다(api 의 JudgeURL).
func CheckName(s string) error { return check(s, ErrNUL) }

// check 는 글 s 를 잰다: 깨진 UTF-8 은 ErrUTF8 이고, nul 이 차 있으면(이름 자리) NUL 이 든 글은 그 갈래다.
func check(s string, nul error) error {
	switch {
	case !utf8.ValidString(s):
		return ErrUTF8
	case nul != nil && strings.IndexByte(s, 0) >= 0:
		return nul
	}
	return nil
}

// refuse 는 거절 갈래 err 를 자리 path 의 *Refusal 로 싼다(nil 이면 nil).
func refuse(err error, path []string) error {
	if err == nil {
		return nil
	}
	return &Refusal{Err: err, Pointer: pointer(path)}
}

// doc 는 json.RawMessage 칸 하나를 잰다: 짝 없는 서로게이트 · 키의 U+0000 · 깨진 UTF-8 · 깨진 문서는 표지와 상관없이 거절이고,
// 이름 칸이면 U+0000 이 든 글도 거절한다(자리는 칸 자리 뒤에 문서 안 자리를 잇는다).
func doc(v reflect.Value, path []string, text bool) error {
	if v.Len() == 0 {
		return nil
	}
	raw := v.Bytes()
	if !utf8.Valid(raw) {
		return &Refusal{Err: ErrUTF8, Pointer: pointer(path)}
	}
	_, changes, err := EscapeJSON(string(raw))
	var r *Refusal
	switch {
	case errors.As(err, &r):
		return &Refusal{Err: r.Err, Pointer: pointer(path) + r.Pointer}
	case err != nil:
		return &Refusal{Err: err, Pointer: pointer(path)}
	case len(changes) > 0 && !text:
		return &Refusal{Err: ErrNUL, Pointer: pointer(path) + changes[0]}
	}
	return nil
}

// textTag 는 칸 f 가 자유 글 표지(`nul:"text"`)를 달았는가다. 그 밖의 표지 값은 오류다 - 오타 난 표지가 이름 칸으로 조용히 접히면
// 자유 글이 거절된다.
func textTag(f reflect.StructField) (bool, error) {
	switch tag, ok := f.Tag.Lookup(tagKey); {
	case !ok:
		return false, nil
	case tag == tagText:
		return true, nil
	default:
		return false, fmt.Errorf("nulesc: 칸 %s 의 표지 %s:%q 를 모른다(자유 글이면 %s:%q)", f.Name, tagKey, tag, tagKey, tagText)
	}
}

// fieldName 은 칸 f 의 자리 이름이다: json 표지의 이름, 없으면 Go 이름. 표지 이름이 없는 내장 칸은 encoding/json 처럼 바깥 칸에
// 펼친다(inline).
func fieldName(f reflect.StructField) (name string, inline bool) {
	name, _, _ = strings.Cut(f.Tag.Get("json"), ",")
	switch {
	case name == "-":
		return f.Name, false
	case name != "":
		return name, false
	case f.Anonymous:
		return "", true
	}
	return f.Name, false
}

// embedsStruct 는 내장 칸의 타입 t 가 구조체(나 그 포인터)인가다 - encoding/json 이 내보내지 않은 타입이어도 그 안의 내보낸 칸을
// 펼쳐 채우는 갈래다.
func embedsStruct(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// keyText 는 맵 키 k 의 자리 이름이다(글 키는 그 글, 그 밖은 fmt 의 꼴).
func keyText(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	return fmt.Sprint(k)
}

// with 는 path 뒤에 조각 하나를 이은 새 자리다(path 의 바탕 배열을 나누지 않는다).
func with(path []string, part string) []string {
	return append(path[:len(path):len(path)], part)
}
