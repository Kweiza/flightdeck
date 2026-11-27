package legacy

import (
	"context"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest/freshstore"
)

// `fd import --apply` 는 REST 를 안 거치고 원장에 직접 쓴다. 그래도 원장 칸 규칙은 같다(store 의 원장 칸 관문): 자유 글의 NUL 은
// 이관과 같은 `\x00` 표기로 담고, 이름(항목 id)의 NUL · 깨진 UTF-8 은 그 레코드의 거절이다. 계획(예행)이 그 거절을 미리 내므로
// 예행이 「넣는다」고 한 것을 적용이 그대로 넣는다 - 두 엔진 모두. 고치기 전에는 Postgres 에서 NUL 한 칸이 가져오기 전체를
// 롤백했다(22021).
func TestImportApplyTakesTheLedgerCellRuleOnBothEngines(t *testing.T) {
	sc := scanFixture(t)
	planned := map[string]bool{}
	for _, it := range PlanImport(sc, PlanOptions{Project: "cp"}).Items {
		planned[it.Item.ID] = true
	}
	var at []int // 손대지 않으면 계획에 드는 큐 항목
	for i, it := range sc.Items {
		if planned[it.ID] {
			at = append(at, i)
		}
	}
	if len(at) < 3 {
		t.Fatalf("시험 원본에서 계획에 드는 큐 항목이 %d건이다 - 셋이 있어야 한다", len(at))
	}
	nulBody, badID, badTitle := sc.Items[at[0]].ID, sc.Items[at[1]].ID, sc.Items[at[2]].ID
	sc.Items[at[0]].Body += "\n끝에 NUL\x00 하나"
	sc.Items[at[1]].ID = badID + "\x00"
	sc.Items[at[2]].Title = "깨진 제목\xff"
	p := PlanImport(sc, PlanOptions{Project: "cp"})

	rejected := map[string]string{}
	for _, r := range p.Rejects {
		if r.Fatal && (r.Code == "bad_name" || r.Code == "bad_utf8") {
			rejected[r.Field] = r.Code
		}
	}
	if rejected["id"] != "bad_name" || rejected["title"] != "bad_utf8" {
		t.Fatalf("이름의 NUL · 깨진 UTF-8 을 거절하지 않았다: %+v", p.Rejects)
	}
	var body string
	for _, it := range p.Items {
		switch it.Item.ID {
		case nulBody:
			body = it.Item.Body
		case badID + "\x00", badTitle:
			t.Errorf("거절한 항목 %q 를 넣으려 한다", it.Item.ID)
		}
	}
	if !strings.Contains(body, "\x00") {
		t.Fatalf("자유 글에 NUL 이 든 항목 %q 를 계획에서 뺐다", nulBody)
	}
	if !strings.Contains(strings.Join(p.Notes, "\n"), "자유 글 1칸에 NUL") {
		t.Errorf("바꿔 넣을 칸을 예행이 말하지 않는다: %q", p.Notes)
	}

	tb, _ := pg.TableOf("item")
	want, fault, _ := pg.Cell(*tb.Col("body"), body)
	if fault != "" {
		t.Fatal(fault)
	}
	for _, eng := range []freshstore.Engine{freshstore.SQLite, freshstore.Postgres} {
		t.Run(string(eng), func(t *testing.T) {
			st, _ := freshstore.OpenOn(t, eng, nil)
			got, err := Apply(context.Background(), st, p, fxCode)
			if err != nil {
				t.Fatalf("적용: %v", err)
			}
			if got.Items != len(p.Items)+len(p.Issues)+len(p.Blockers) {
				t.Errorf("넣은 항목 %d건, 계획은 %d건", got.Items, len(p.Items)+len(p.Issues)+len(p.Blockers))
			}
			var stored string
			if err := st.DB().QueryRow("SELECT body FROM item WHERE id = '" + nulBody + "'").Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != want {
				t.Errorf("넣은 본문 %q, 이관이 옮긴 값 %q", stored, want)
			}
		})
	}
}
