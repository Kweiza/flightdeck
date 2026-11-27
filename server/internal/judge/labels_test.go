package judge

import (
	"reflect"
	"testing"
)

func TestApplyLabels(t *testing.T) {
	cases := []struct {
		name         string
		cur, add, rm []string
		want         []string
	}{
		{"빈 항목에 하나 더한다", nil, []string{"tickler"}, nil, []string{"tickler"}},
		{"기존 순서를 지키고 새것은 뒤에", []string{"b", "a"}, []string{"c"}, nil, []string{"b", "a", "c"}},
		{"이미 있는 것을 더해도 안 늘어난다", []string{"tickler"}, []string{"tickler"}, nil, []string{"tickler"}},
		{"없는 것을 빼도 오류가 아니다", []string{"a"}, nil, []string{"zzz"}, []string{"a"}},
		{"빼면 사라진다", []string{"a", "tickler", "b"}, nil, []string{"tickler"}, []string{"a", "b"}},
		{"더하기와 빼기가 함께 온다", []string{"a"}, []string{"b"}, []string{"a"}, []string{"b"}},
		{"같은 값을 더하고 빼면 빼기가 이긴다", []string{}, []string{"x"}, []string{"x"}, []string{}},
		{"add 안의 중복은 한 번만", nil, []string{"x", "x"}, nil, []string{"x"}},
		{"cur 의 중복은 정리된다", []string{"a", "a"}, nil, nil, []string{"a"}},
		{"공백은 다듬고 빈 값은 버린다", nil, []string{"  tickler  ", "", "   "}, nil, []string{"tickler"}},
		{"전부 비면 빈 슬라이스", []string{"a"}, nil, []string{"a"}, []string{}},
		// 빼기는 원장에 그대로 있는 값부터 지우고, 그런 값이 없을 때만 원장의 꼴(LabelKey)로 맞댄다: 원장의 `\x00` 표기 ·
		// 앞 판이 남긴 NUL · 받은 NUL 은 같은 꼴이다.
		{"받은 NUL 로 원장 표기를 뺀다", []string{`a\x00`, "b"}, nil, []string{"a\x00"}, []string{"b"}},
		{"원장 표기로 앞 판의 NUL 을 뺀다", []string{"a\x00", "b"}, nil, []string{`a\x00`}, []string{"b"}},
		{"함께 있던 둘 중 부른 NUL 만 빠진다", []string{"a\x00", `a\x00`, "b"}, nil, []string{"a\x00"}, []string{`a\x00`, "b"}},
		{"함께 있던 둘 중 부른 표기만 빠진다", []string{"a\x00", `a\x00`, "b"}, nil, []string{`a\x00`}, []string{"a\x00", "b"}},
		{"함께 있던 둘을 다 부르면 다 빠진다", []string{"a\x00", `a\x00`, "b"}, nil, []string{`a\x00`, "a\x00"}, []string{"b"}},
		{"함께 있던 둘은 다른 고침에도 남는다", []string{"a\x00", `a\x00`}, []string{"c"}, nil, []string{"a\x00", `a\x00`, "c"}},
		{"이관이 겹쳐 놓은 표기는 받은 NUL 로 함께 빠진다", []string{`a\x00`, `a\x00`, "b"}, nil, []string{"a\x00"}, []string{"b"}},
		{"원장 표기가 있으면 받은 NUL 을 더하지 않는다", []string{`a\x00`}, []string{"a\x00"}, nil, []string{`a\x00`}},
		{"빼기는 꼴로 더하기를 이긴다", nil, []string{"a\x00"}, []string{`a\x00`}, []string{}},
	}
	for _, c := range cases {
		got := ApplyLabels(c.cur, c.add, c.rm)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ApplyLabels(%v, %v, %v) = %v, 기대 %v", c.name, c.cur, c.add, c.rm, got, c.want)
		}
	}
}

// nil 이 아니라 빈 슬라이스를 내는 것이 계약이다 — store 가 JSON 으로 직렬화하는데
// nil 은 `null` 이 되고 빈 슬라이스는 `[]` 가 된다. 되읽기(scanItem)는 둘 다
// 받지만, 원장 diff 와 되쓰기 산출물에서 두 값이 다른 글자가 된다.
func TestApplyLabelsNeverReturnsNil(t *testing.T) {
	if got := ApplyLabels(nil, nil, nil); got == nil {
		t.Error("ApplyLabels 가 nil 을 냈다 — 빈 슬라이스여야 한다")
	}
}
