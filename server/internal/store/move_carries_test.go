package store

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
)

// 옮긴 항목은 개정 이력과 판단을 **데리고 가야** 한다.
//
// ★ 이 시험이 없어서 난 일(2026-09-24 실측): 저장소 분리 때 항목 하나를 kweiza-cc-plugins →
// flightdeck 로 옮겼더니 ① 걸려 있던 판단 7건이 show·pick 에서 사라졌고(연결 행은 그대로인데
// target_project 가 NULL 이라 판단의 프로젝트, 곧 옛 프로젝트로 해석됐다) ② 경로를 amend 한
// 뒤 되돌리려는 이동이 FK 787 로 500 이 됐다(딸린 표 목록에 item_revision 이 없었다).
// 둘 다 오류 없이 조용하거나, 원인을 말하지 않는 500 이었다.
func TestMoveItemCarriesRevisionsAndJudgmentLinks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seed(t, s, "from-proj")
	seed(t, s, "to-proj")
	seed(t, s, "other-proj")
	mustItem(t, s, "from-proj", "it-1")
	sess := mustSession(t, s, "from-proj", "cc-1")

	// 개정 이력 한 줄 — 이것이 있으면 이동이 FK 로 막혔다.
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, e := tx.AmendItem("from-proj", "it-1", AmendPatch{Title: strp("고친 제목"), Reason: "오타"}, sess.ID)
		return e
	}); err != nil {
		t.Fatalf("개정 실패: %v", err)
	}
	// 옛 프로젝트의 판단이 이 항목에 걸린다(target_project 는 비어 있다 = 판단의 프로젝트).
	carried, err := s.AddJudgment(ctx, model.Judgment{
		Project: "from-proj", SessionID: sess.ID, Kind: model.JudgmentDecision, Body: "따라가야 할 판단",
		Links: []model.JudgmentLink{{TargetKind: "item", TargetID: "it-1"}},
	})
	if err != nil {
		t.Fatalf("판단 저장 실패: %v", err)
	}
	// 같은 id 를 가진 **남의 항목**을 명시적으로 가리키는 링크 — 이것은 옮기면 안 된다.
	mustItem(t, s, "other-proj", "it-1")
	foreign, err := s.AddJudgment(ctx, model.Judgment{
		Project: "from-proj", SessionID: sess.ID, Kind: model.JudgmentDecision, Body: "남의 항목을 가리킨다",
		Links: []model.JudgmentLink{{TargetKind: "item", TargetID: "it-1", TargetProject: "other-proj"}},
	})
	if err != nil {
		t.Fatalf("판단 저장 실패: %v", err)
	}

	// ── 대조 전제: 옮기기 전에 정말 옛 자리에 보이는가.
	if n := countIn(t, s, "item_revision", "from-proj", "it-1"); n == 0 {
		t.Fatal("대조 전제가 깨졌다 — 개정 이력이 0건이라 이동을 볼 수 없다")
	}
	if js, _ := s.JudgmentsForItem(ctx, "from-proj", "it-1"); len(js) != 1 {
		t.Fatalf("대조 전제가 깨졌다 — 옛 자리에서 판단이 %d건 보인다(1건이어야 한다)", len(js))
	}

	if _, err := s.MoveItem(ctx, "from-proj", "it-1", "to-proj", "sess-x"); err != nil {
		t.Fatalf("개정 이력이 있는 항목을 못 옮겼다: %v", err)
	}

	// 개정 이력은 항목을 따라간다 — 옛 자리에 0, 새 자리에 그대로.
	if n := countIn(t, s, "item_revision", "from-proj", "it-1"); n != 0 {
		t.Errorf("item_revision 에 옛 프로젝트 행이 %d건 남았다", n)
	}
	if n := countIn(t, s, "item_revision", "to-proj", "it-1"); n != 1 {
		t.Errorf("item_revision 이 대상 프로젝트로 %d건 따라왔다(1건이어야 한다)", n)
	}
	// 판단은 새 자리에서 보이고 옛 자리에서는 안 보인다.
	got, err := s.JudgmentsForItem(ctx, "to-proj", "it-1")
	if err != nil {
		t.Fatalf("판단 조회 실패: %v", err)
	}
	if len(got) != 1 || got[0].ID != carried.ID {
		t.Errorf("옮긴 항목에서 판단이 안 보인다 — 받은 것 %d건. 판단은 파생 불가한 유일한 자산이다", len(got))
	}
	if js, _ := s.JudgmentsForItem(ctx, "from-proj", "it-1"); len(js) != 0 {
		t.Errorf("옛 자리에 판단이 %d건 남아 보인다 — 없는 항목에 걸린 판단이다", len(js))
	}
	// 남의 항목을 가리키던 링크는 그대로다.
	if js, _ := s.JudgmentsForItem(ctx, "other-proj", "it-1"); len(js) != 1 || js[0].ID != foreign.ID {
		t.Errorf("남의 항목(other-proj/it-1)을 가리키던 판단이 흔들렸다 — 받은 것 %d건", len(js))
	}
}

// itemKeyedNotMoved 는 (project, item_id) 를 들었지만 **일부러 안 옮기는** 표다. 사유가 값이다.
//
// 비어 있는 것이 정상이다. 여기에 오르는 표가 생기면 그 사유가 이동의 의미와 맞는지 다시 읽어라.
var itemKeyedNotMoved = map[string]string{}

// 이동 목록은 **스키마에서 뽑은 목록**과 맞아야 한다.
//
// ★ 앞선 관문(TestMoveItemCarriesEveryRowKeyedByProject)은 볼 표를 스스로 적어 두었다 —
// 그래서 증분 016 이 item_revision 을 더했을 때 이동 목록도 시험도 같이 낡았고 아무도 몰랐다.
// 이 시험은 목록을 적지 않는다. 적용이 끝난 스키마에서 `project`·`item_id` 칼럼을 함께 든 표를
// 전부 찾아, 이동 목록(itemKeyedTables)이나 안 옮기는 목록(사유와 함께) 중 한 곳에 있기를 요구한다.
func TestMoveItemListCoversEveryTableKeyedToItem(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	var found []string
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("표 목록 조회 실패: %v", err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("표 이름 해석 실패: %v", err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) == 0 {
		t.Fatal("표를 하나도 못 찾았다 — 훑기가 눈이 먼 것이지 통과가 아니다")
	}
	for _, tbl := range tables {
		cols := tableColumns(t, s.db, tbl)
		if cols["project"] && cols["item_id"] {
			found = append(found, tbl)
		}
	}
	sort.Strings(found)
	if len(found) == 0 {
		t.Fatal("(project, item_id) 를 든 표가 0개다 — claim 은 있어야 한다. 훑기가 틀렸다")
	}
	for _, tbl := range found {
		if slices.Contains(itemKeyedTables, tbl) {
			continue
		}
		if why, ok := itemKeyedNotMoved[tbl]; ok && strings.TrimSpace(why) != "" {
			continue
		}
		t.Errorf("표 %s 가 (project, item_id) 를 드는데 MoveItem 이 안 옮긴다.\n"+
			"옮겨야 하면 itemKeyedTables 에 더하고, 일부러 안 옮기면 itemKeyedNotMoved 에 **사유와 함께** 올려라", tbl)
	}
	// 반대 방향: 목록에 있는데 스키마에 없는 표는 걷어라(011 이 item_dependents 를 걷은 것과 같은 규율).
	for _, tbl := range itemKeyedTables {
		if !slices.Contains(found, tbl) {
			t.Errorf("itemKeyedTables 의 %s 가 스키마에 (project, item_id) 표로 없다 — 목록이 낡았다", tbl)
		}
	}
}

// 개정 이력은 여전히 **내용을 못 고치고 못 지운다.** 증분 017 이 연 것은 project 좌표 하나다.
func TestItemRevisionContentStaysAppendOnly(t *testing.T) {
	st, ctx, sess := amendFixture(t)
	if err := st.Tx(ctx, func(tx *Tx) error {
		_, e := tx.AmendItem("p1", "i1", AmendPatch{Title: strp("고친 제목"), Reason: "오타"}, sess)
		return e
	}); err != nil {
		t.Fatalf("개정 실패: %v", err)
	}
	for _, q := range []string{
		`UPDATE item_revision SET body = '바꿔치기' WHERE project = 'p1' AND item_id = 'i1'`,
		`UPDATE item_revision SET title = '바꿔치기' WHERE project = 'p1' AND item_id = 'i1'`,
		`UPDATE item_revision SET reason = '바꿔치기' WHERE project = 'p1' AND item_id = 'i1'`,
		`UPDATE item_revision SET rev = 99 WHERE project = 'p1' AND item_id = 'i1'`,
		`UPDATE item_revision SET item_id = 'i2' WHERE project = 'p1' AND item_id = 'i1'`,
		`DELETE FROM item_revision WHERE project = 'p1' AND item_id = 'i1'`,
	} {
		_, err := st.db.ExecContext(ctx, q)
		if err == nil || !strings.Contains(err.Error(), "추가 전용") {
			t.Errorf("개정 이력의 내용이 바뀌었다(또는 다른 이유로 거절됐다) — %s: %v", q, err)
		}
	}
}

// tableColumns 는 표의 칼럼 이름 집합이다.
func tableColumns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("%s 칼럼 조회 실패: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("%s 칼럼 해석 실패: %v", table, err)
		}
		out[n] = true
	}
	return out
}
