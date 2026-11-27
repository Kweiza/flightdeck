package judge

import (
	"strings"

	"github.com/kweiza/flightdeck/internal/nulesc"
)

// ApplyLabels 는 꼬리표 집합에 더하기와 빼기를 적용한다. 순수 함수다.
//
// ★ 계약 셋이 있고 셋 다 이유가 있다.
//
//  1. **기존 순서를 지킨다.** 정렬하지 않는다 — labels 는 JSON 배열로 저장되고
//     되쓰기 산출물(legacy/export.go)이 원본과 diff 로 대조된다. 정렬을 걸면 판이
//     바뀔 때 무관한 항목들의 줄이 통째로 흔들리고, 그 산출물의 존재 이유가 무너진다.
//     새로 더한 것은 **뒤에** 붙는다.
//
//  2. **빼기가 더하기를 이긴다.** 같은 값이 add·rm 에 함께 오면 결과에서 빠진다.
//     반대로 정하면 "지워라"가 조용히 무시되는데, 이 함수의 소비자는 사람이 방금
//     친 명령이라 무시된 의도가 화면에 안 나타난다.
//
//  3. **nil 을 안 낸다.** 빈 슬라이스다 — nil 은 JSON 에서 `null` 이 되고 빈
//     슬라이스는 `[]` 가 된다. 두 값은 되읽기에서 같아 보이지만 원장에 남는
//     글자가 다르다.
//
// 공백만 있는 값과 빈 문자열은 버린다. 판정의 정본은 IsTickler 이고 그것은 정확
// 일치만 보므로(tickler.go), 앞뒤 공백이 붙은 채 저장되면 사람 눈에는 같은
// 꼬리표인데 판정에서 조용히 빠진다.
//
// ★ 빼기는 원장의 값과 **바이트까지 같은 값부터** 지운다. 고치기 전 SQLite 원장의 한 행에는 실제 NUL 꼬리표와 `\x00` 네
// 글자 꼬리표가 따로 있을 수 있다 - 하나를 불렀는데 둘 다 지우면 부르지 않은 꼬리표가 조용히 사라진다. 같은 값이 원장에
// 없을 때만 원장의 꼴(LabelKey)로 맞댄다: cur 는 원장에서 읽은 값(Postgres 원장이면 NUL 이 `\x00` 표기로 담겼다)이고 rm 은
// 받은 원문이라, 받은 NUL 로 원장의 `\x00` 표기를 지우려면 꼴을 맞춰야 한다. cur 의 겹침도 바이트로 잰다(다른 바이트는 그
// 원장이 가르던 다른 꼬리표다). 더하기는 꼴로 잰다 - 원장에 같은 꼴이 있는 값을 더하면 원장에서 둘이 같은 값이 된다. 빼기가
// 이기는 것도 꼴로 잰다. 결과에는 받은 값을 그대로 싣는다 - 원장의 꼴로 옮기는 것은 store 의 원장 칸 관문이다.
func ApplyLabels(cur, add, rm []string) []string {
	have := make(map[string]bool, len(cur))
	for _, l := range cur {
		have[strings.TrimSpace(l)] = true
	}
	exact := make(map[string]bool, len(rm)) // 원장에 그대로 있는 값 - 그 값만 뺀다
	keyed := make(map[string]bool, len(rm)) // 원장에 그대로는 없는 값의 꼴 - 그 꼴인 것을 뺀다
	beats := make(map[string]bool, len(rm)) // 더하기를 이기는 꼴
	for _, l := range rm {
		l = strings.TrimSpace(l)
		k := LabelKey(l)
		if k == "" {
			continue
		}
		beats[k] = true
		if have[l] {
			exact[l] = true
		} else {
			keyed[k] = true
		}
	}

	out := make([]string, 0, len(cur)+len(add))
	seen := make(map[string]bool, len(cur))             // 남긴 cur 의 값
	seenKey := make(map[string]bool, len(cur)+len(add)) // 남긴 값의 꼴
	for _, l := range cur {
		l = strings.TrimSpace(l)
		k := LabelKey(l)
		if k == "" || exact[l] || keyed[k] || seen[l] {
			continue
		}
		seen[l], seenKey[k] = true, true
		out = append(out, l)
	}
	for _, l := range add {
		l = strings.TrimSpace(l)
		k := LabelKey(l)
		if k == "" || beats[k] || seenKey[k] {
			continue
		}
		seenKey[k] = true
		out = append(out, l)
	}
	return out
}

// LabelKey 는 꼬리표 l 을 맞댈 열쇠다: 앞뒤 공백을 걷고 원장의 꼴(nulesc.Text)로 옮긴 값. 빈 값이면 버릴 꼬리표다.
func LabelKey(l string) string { return nulesc.Text(strings.TrimSpace(l)) }
