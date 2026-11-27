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

// 폐기 판단 본문은 원장에서 읽은 글(항목 제목)을 받은 사유와 잇지 않는다. 제목은 원장의 꼴(NUL 이 `\x00` 표기)로 읽히므로,
// 사유에 NUL 이 있어 관문이 본문을 한 글로 바꾸면 제목의 `\x00` 가 `\\x00` 로 한 번 더 옮겨져 이관이 SQLite 판 본문을 옮긴
// 값과 갈렸다. 본문은 받은 글과 서버가 지은 글로만 짓고, 그 한 글이 관문을 한 번 지난다 - 두 엔진 모두.
func TestDropJudgmentBodyIsOneImportCellOnBothEngines(t *testing.T) {
	for _, eng := range []freshstore.Engine{freshstore.SQLite, freshstore.Postgres} {
		t.Run(string(eng), func(t *testing.T) {
			f := newFixture(t, withEngine(eng)).withRepo("feat")
			f.openSession("cc-1", "트랙2")
			f.addItem("t5-d", "제목\x00끝", nil, nil)
			const reason = "사유\x00가 충분히 길다"
			if rec := f.post("/actions/drop", url.Values{"project": {testProject}, "item": {"t5-d"}, "reason": {reason}}); rec.Code != http.StatusSeeOther {
				t.Fatalf("폐기: %d %s, 바라는 것 303", rec.Code, rec.Body.String())
			}
			var body string
			if err := f.st.DB().QueryRow(`SELECT body FROM judgment WHERE title = '항목 폐기: t5-d'`).Scan(&body); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(body, "제목") || strings.Contains(body, `\\x00`) {
				t.Errorf("폐기 본문에 원장에서 읽은 제목이 다시 실렸다(그 `\\x00` 가 두 번 옮겨진다): %q", body)
			}
			orig := nulesc.Unescape(body)
			if !strings.Contains(orig, "\n사유: "+reason+"\n") {
				t.Errorf("폐기 본문을 되돌린 글에 받은 사유 %q 가 없다: %q", reason, orig)
			}
			tb, _ := pg.TableOf("judgment")
			if want, fault, _ := pg.Cell(*tb.Col("body"), orig); fault != "" || body != want {
				t.Errorf("폐기 본문 = %q, 이관이 되돌린 글을 옮긴 값 %q(%s)", body, want, fault)
			}
		})
	}
}
