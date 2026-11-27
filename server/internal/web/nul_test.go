package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/nulesc"
	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest/freshstore"
)

// 대시보드 폼도 api 의 요청 풀기와 같은 입력 규칙(nulesc.Fields)을 탄다: 이름 칸(프로젝트 · 항목 · 축)의 NUL 과 어느 칸이든 깨진
// UTF-8 은 400 이고 그때 아무것도 안 바뀐다. 사유의 NUL 은 받은 그대로 판정하고(길이도 받은 원문으로 잰다), 원장에 담길 때 store 의
// 원장 칸 관문이 이관과 같은 `\x00` 표기로 옮긴다 - 두 엔진 모두.
func TestFormsTakeTheWritePathInputRuleOnBothEngines(t *testing.T) {
	for _, eng := range []freshstore.Engine{freshstore.SQLite, freshstore.Postgres} {
		t.Run(string(eng), func(t *testing.T) {
			f := newFixture(t, withEngine(eng)).withRepo("feat")
			f.openSession("cc-1", "트랙2")
			f.addItem("t5-n", "NUL 사유", nil, nil)
			ctx := context.Background()

			for name, form := range map[string]struct {
				path, want string
				v          url.Values
			}{
				"항목":           {"/actions/drop", "NUL", url.Values{"project": {testProject}, "item": {"t5-n\x00"}, "reason": {"사유가 충분히 길다"}}},
				"프로젝트":         {"/actions/drop", "NUL", url.Values{"project": {testProject + "\x00"}, "item": {"t5-n"}, "reason": {"사유가 충분히 길다"}}},
				"표시 축 대상":      {"/actions/project-view", "NUL", url.Values{"project": {testProject}, "axis": {"pin:" + testProject + "\x00"}}},
				"사유의 깨진 UTF-8": {"/actions/drop", "UTF-8", url.Values{"project": {testProject}, "item": {"t5-n"}, "reason": {"사유가 충분히 길다\xff"}}},
				"항목의 깨진 UTF-8": {"/actions/drop", "UTF-8", url.Values{"project": {testProject}, "item": {"t5-n\xc3"}, "reason": {"사유가 충분히 길다"}}},
				// 받은 원문은 두 글자다(최소 4자에 못 미친다) - `\x00` 표기로 옮긴 뒤(다섯 글자)를 재면 받는다.
				"짧은 사유": {"/actions/drop", "너무 짧다", url.Values{"project": {testProject}, "item": {"t5-n"}, "reason": {"가\x00"}}},
			} {
				rec := f.post(form.path, form.v)
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), form.want) {
					t.Errorf("%s: %d %s, 바라는 것 400 · %q", name, rec.Code, rec.Body.String(), form.want)
				}
			}
			if it, err := f.st.GetItem(ctx, testProject, "t5-n"); err != nil || it.State != "open" {
				t.Fatalf("거절했는데 항목이 바뀌었다: %+v %v", it, err)
			}

			// 받은 원문으로 상한(2000자)에 딱 맞는 사유 - 옮긴 뒤(2003자)를 재면 거절한다.
			reason := strings.Repeat("가", reasonMax-1) + "\x00"
			if rec := f.post("/actions/drop", url.Values{"project": {testProject}, "item": {"t5-n"}, "reason": {reason}}); rec.Code != http.StatusSeeOther {
				t.Fatalf("사유의 NUL: %d %s, 바라는 것 303", rec.Code, rec.Body.String())
			}
			it, err := f.st.GetItem(ctx, testProject, "t5-n")
			if err != nil {
				t.Fatal(err)
			}
			tb, _ := pg.TableOf("item")
			want, fault, _ := pg.Cell(*tb.Col("close_reason"), reason)
			if fault != "" || it.CloseReason != want || it.CloseReason != nulesc.Escape(reason) {
				t.Errorf("폐기 사유 = %q, 바라는 것 이관이 옮긴 값 %q(%s)", it.CloseReason, want, fault)
			}
		})
	}
}
