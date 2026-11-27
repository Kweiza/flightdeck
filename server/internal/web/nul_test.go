package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/nulesc"
)

// 대시보드 폼도 api 의 요청 풀기와 같은 쓰기 길 NUL 규칙(nulesc.Fields)을 탄다: 사유의 NUL 은 `\x00` 표기로 바꿔 받고, 이름 칸
// (프로젝트 · 항목 · 축)의 NUL 은 400 이다 - 그때 아무것도 안 바뀐다.
func TestFormsTakeTheWritePathNULRule(t *testing.T) {
	f := newFixture(t).withRepo("feat")
	f.openSession("cc-1", "트랙2")
	f.addItem("t5-n", "NUL 사유", nil, nil)
	ctx := context.Background()

	for name, form := range map[string]struct {
		path string
		v    url.Values
	}{
		"항목":      {"/actions/drop", url.Values{"project": {testProject}, "item": {"t5-n\x00"}, "reason": {"사유가 충분히 길다"}}},
		"프로젝트":    {"/actions/drop", url.Values{"project": {testProject + "\x00"}, "item": {"t5-n"}, "reason": {"사유가 충분히 길다"}}},
		"표시 축 대상": {"/actions/project-view", url.Values{"project": {testProject}, "axis": {"pin:" + testProject + "\x00"}}},
	} {
		rec := f.post(form.path, form.v)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "NUL") {
			t.Errorf("%s 칸의 NUL: %d %s, 바라는 것 400 · NUL 거절", name, rec.Code, rec.Body.String())
		}
	}
	if it, err := f.st.GetItem(ctx, testProject, "t5-n"); err != nil || it.State != "open" {
		t.Fatalf("거절했는데 항목이 바뀌었다: %+v %v", it, err)
	}

	reason := "사유 안의\x00NUL 바이트"
	if rec := f.post("/actions/drop", url.Values{"project": {testProject}, "item": {"t5-n"}, "reason": {reason}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("사유의 NUL: %d %s, 바라는 것 303", rec.Code, rec.Body.String())
	}
	it, err := f.st.GetItem(ctx, testProject, "t5-n")
	if err != nil {
		t.Fatal(err)
	}
	if it.CloseReason != nulesc.Escape(reason) {
		t.Errorf("폐기 사유 = %q, 바라는 것 %q(이관 · api 와 같은 표기)", it.CloseReason, nulesc.Escape(reason))
	}
}
