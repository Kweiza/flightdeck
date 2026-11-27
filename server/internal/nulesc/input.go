package nulesc

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// 서버의 쓰기 길이 받는 입력에 거는 규칙 — 이관(store/pg 의 Cell)이 SQLite 원본의 칸에 거는 것과 같은 함수를, 칸 대신 입력 값의
// 자리에 건다. 자유 글의 NUL 은 Escape 로 바꿔 받고, 이름 자리의 NUL · JSON 키의 U+0000 · 짝 없는 서로게이트는 거절한다. 그래서
// 두 엔진이 같은 값을 저장하고(SQLite 도 NUL 을 받지 않는다), 쓰기가 저장한 값은 그 원본을 이관이 옮겨 낸 값과 같다.

// ErrNUL: 자유 글 표지(`nul:"text"`)가 없는 자리의 NUL. 그 자리는 이름이다 - 식별자(이관 카탈로그의 식별자 칸에 들어가는 항목 ·
// 세션 · 프로젝트 id, 자원 이름), 열거(종류 · 상태), 서버가 좌표로 쓰는 경로(항목 · 발자국 · 작업 경로)와 git 이름처럼 값이 곧
// 다른 것을 가리키는 글. 바꿔 받으면 원래 `\x00` 네 글자를 담은 다른 이름과 같아져 다른 행을 가리키거나(이관이 식별자 칸 · JSON
// 키의 NUL 을 거절하는 것과 같은 까닭이다) 가리키는 것이 없다(경로의 `\x00` 는 POSIX 좌표가 아니라 서비스가 다시 거절한다).
var ErrNUL = errors.New("이름 자리의 NUL")

// tagKey 는 자유 글 표지의 구조체 표지 이름이고 tagText 는 그 값이다: `nul:"text"`.
const (
	tagKey  = "nul"
	tagText = "text"
)

var rawMessage = reflect.TypeFor[json.RawMessage]()

// Input 은 서버가 받은 JSON 문서 doc 와 그것을 풀어 낸 값 v(구조체를 가리키는 포인터)에 규칙을 건다. 요청 본문을 푸는 자리
// (api 의 decode)가 부른다:
//
//   - doc 의 글에 짝 없는 서로게이트가 있거나 키에 U+0000 이 있으면 그 자리의 *Refusal(ErrSurrogate · ErrKeyNUL)이다. 풀기
//     (encoding/json)는 짝 없는 서로게이트를 U+FFFD 로 바꾸고 모르는 키를 버려 v 에 흔적을 안 남기므로 풀기 전 원문에서 잰다.
//   - 그다음 v 에 Fields 를 건다.
//
// 바꾼 글의 자리를 낸다. 거절이면 v 를 하나도 안 바꾼다. doc 가 JSON 문서가 아니면 ErrSyntax 다(풀기를 지난 문서라 나지 않는다).
func Input(doc string, v any) ([]string, error) {
	if _, _, err := EscapeJSON(doc); err != nil {
		return nil, err
	}
	return Fields(v)
}

// Fields 는 입력 값 v(구조체를 가리키는 포인터)의 글에 규칙을 건다. 칸마다 표지로 가른다:
//
//   - `nul:"text"` 가 붙은 칸은 자유 글이다(제목 · 본문 · 사유 · 꼬리표처럼 사람이 쓰는 글). 그 칸에서 조각 · 포인터로 닿는 글에
//     NUL 이 있으면 Escape 로 제자리에서 바꾼다. json.RawMessage 칸은 EscapeJSON 이다(U+0000 이 든 글 토큰만).
//   - 표지가 없는 칸의 글은 이름이다. NUL 이 있으면 그 자리의 *Refusal(ErrNUL)이다(json.RawMessage 칸이면 U+0000 이 든 글).
//   - 맵 키의 NUL 은 표지와 상관없이 *Refusal(ErrKeyNUL)이다 - 키는 이름이다.
//
// 바꾼 글의 자리(RFC 6901 JSON Pointer - 칸 이름은 json 표지의 이름, 없으면 Go 이름. 표지 이름 없는 내장 칸은 encoding/json
// 처럼 바깥 칸에 펼친다)를 걸은 차례로 낸다. 거절이면 v 를 하나도 안 바꾸고 처음 만난 자리만 낸다. 구조체를 가리키는 포인터가
// 아니거나 모르는 표지 값이면 *Refusal 이 아닌 오류다(부르는 쪽의 결함이다).
//
// ★ 표지가 없으면 거절이 기본이다. 자유 글 칸에 표지를 빠뜨리면 NUL 이 든 입력이 400 으로 시끄럽게 걸리지만, 이름 칸을 자유
// 글로 잘못 두면 바꾼 이름이 다른 행을 조용히 가리킨다 - 틀릴 때 시끄러운 쪽을 기본으로 둔다.
// ★ 제자리에서 바꿀 수 없는 자리(맵의 값, 인터페이스 안)는 표지와 상관없이 이름으로 본다(거절). 바이트 조각은 글이 아니라 재지
// 않는다.
func Fields(v any) ([]string, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("nulesc.Fields: 구조체를 가리키는 포인터가 아니다(%T)", v)
	}
	w := &fieldWalker{}
	if err := w.walk(rv.Elem(), nil, false); err != nil {
		return nil, err
	}
	for _, s := range w.sets {
		s.v.Set(s.to)
	}
	return w.at, nil
}

// fieldWalker 는 Fields 가 값을 한 번 걷는 상태다. 바꿀 자리(sets)는 걷기를 다 마친 뒤에야 바꾼다 - 거절이면 하나도 안 바꾼다.
type fieldWalker struct {
	sets []fieldSet
	at   []string
}

type fieldSet struct{ v, to reflect.Value }

func (w *fieldWalker) walk(v reflect.Value, path []string, text bool) error {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return w.walk(v.Elem(), path, text)
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			isText, err := textTag(f)
			if err != nil {
				return err
			}
			at := path
			if name, inline := fieldName(f); !inline {
				at = with(path, name)
			}
			if err := w.walk(v.Field(i), at, isText); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		switch {
		case v.Type() == rawMessage:
			return w.doc(v, path, text)
		case v.Type().Elem().Kind() == reflect.Uint8:
			return nil
		}
		for i := range v.Len() {
			if err := w.walk(v.Index(i), with(path, strconv.Itoa(i)), text); err != nil {
				return err
			}
		}
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return cmp.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
		for _, k := range keys {
			at := with(path, fmt.Sprint(k))
			if k.Kind() == reflect.String && strings.IndexByte(k.String(), 0) >= 0 {
				return &Refusal{Err: ErrKeyNUL, Pointer: pointer(at)}
			}
			if err := w.walk(v.MapIndex(k), at, text); err != nil {
				return err
			}
		}
	case reflect.String:
		s := v.String()
		if strings.IndexByte(s, 0) < 0 {
			return nil
		}
		if !text || !v.CanSet() {
			return &Refusal{Err: ErrNUL, Pointer: pointer(path)}
		}
		w.sets = append(w.sets, fieldSet{v: v, to: reflect.ValueOf(Escape(s)).Convert(v.Type())})
		w.at = append(w.at, pointer(path))
	}
	return nil
}

// doc 는 json.RawMessage 칸 하나를 잰다: 자유 글 칸이면 EscapeJSON 으로 바꾸고, 이름 칸이면 U+0000 이 든 글을 거절한다. 짝 없는
// 서로게이트 · 키의 U+0000 · 깨진 문서는 표지와 상관없이 거절이다(자리는 칸 자리 뒤에 문서 안 자리를 잇는다).
func (w *fieldWalker) doc(v reflect.Value, path []string, text bool) error {
	if v.Len() == 0 {
		return nil
	}
	out, changes, err := EscapeJSON(string(v.Bytes()))
	var r *Refusal
	switch {
	case errors.As(err, &r):
		return &Refusal{Err: r.Err, Pointer: pointer(path) + r.Pointer}
	case err != nil:
		return &Refusal{Err: err, Pointer: pointer(path)}
	case len(changes) == 0:
		return nil
	case !text || !v.CanSet():
		return &Refusal{Err: ErrNUL, Pointer: pointer(path) + changes[0]}
	}
	w.sets = append(w.sets, fieldSet{v: v, to: reflect.ValueOf(json.RawMessage(out)).Convert(v.Type())})
	for _, c := range changes {
		w.at = append(w.at, pointer(path)+c)
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

// with 는 path 뒤에 조각 하나를 이은 새 자리다(path 의 바탕 배열을 나누지 않는다).
func with(path []string, part string) []string {
	return append(path[:len(path):len(path)], part)
}
