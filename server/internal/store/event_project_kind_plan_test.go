package store

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// event 를 project + kind 로 거르는 문장이 두 엔진에서 어느 인덱스 범위를 타는지 잠근다(증분 019 의 event_by_project_kind
// (project, kind, id) 와 그 대상이 아닌 문장).
//
// ★ Postgres 는 일반 계획(generic plan)을 강제해서 잰다. 서버는 pgx 의 문 캐시로 이 문장을 prepared 문으로 두고, 서버가
// 여섯 번째 실행부터 일반 계획으로 바꿀 수 있다 - 그 계획은 project 값을 모른다. 인덱스가 없던 때의 일반 계획은 기본키를
// 거꾸로 훑으며 거르는 쪽이었고, 그 프로젝트의 행이 LIMIT 보다 적으면 표 전체를 읽었다. 맞춤 계획(custom)만 재면 그
// 갈래를 못 본다 - 값을 넣은 계획은 project 의 빈도를 알고 다른 인덱스를 고른다.
// ★ plan_cache_mode 는 이 시험의 연결 하나에만 건다(SET, 세션 단위). 서버의 연결 인자(pgSessionParams)에는 없다.
// ★ SQLite 는 통계 없이 잰다(빈 원장). 서버는 ANALYZE 를 돌리지 않으므로 운영 원장의 계획도 같은 기본 추정으로 선다.

// eventPlan 은 문장이 event 를 읽는 모양으로 지켜야 할 것이다.
type eventPlan int

const (
	// planProjectKind: event 를 event_by_project_kind 의 (project, kind) 범위로만 읽는다.
	planProjectKind eventPlan = iota
	// planProjectKindOrdered: 위에 더해 id 순서를 인덱스에서 받는다(정렬 단계가 없다) - ORDER BY id … LIMIT 이 LIMIT 에서 멈춘다.
	planProjectKindOrdered
	// planProjectKindFromID: 위에 더해 셋째 키(id)의 하한이 범위 조건이다 - 범위를 다 읽고 id 로 사후에 거르지 않는다.
	planProjectKindFromID
	// planWindow: 시각 창(at 하한)으로 자른 인덱스 범위를 탄다 - 프로젝트의 그 종류 이력 전부를 읽고 거르지 않는다.
	planWindow
	// planProjectKindOrWindow: event 를 읽는 범위마다 event_by_project_kind 의 (project, kind) 범위이거나 시각 창 범위다. 「전체」
	// 셈과 창 셈을 한 문장에 둔 집계(AckReach)다 - SQLite 는 CTE 를 셈마다 펼치고 창 셈에 at 하한을 밀어 넣어 창 범위를 고른다.
	planProjectKindOrWindow
)

// eventPlanStatement 는 잴 문장 하나다. pgArgs 는 EXECUTE 에 넣는 SQL 상수 목록, liteArgs 는 같은 값의 SQLite 인자다. alias 는
// 문장이 event 에 붙인 별칭이다(없으면 event) - SQLite 계획 줄이 그 이름으로 event 를 부른다. built 는 실행 중에 잇는 문장이다 -
// 소스에 그 글 그대로의 조각이 없고, 만드는 함수가 eventPlanSites 에 있다.
type eventPlanStatement struct {
	name     string
	sql      string
	pgArgs   string
	liteArgs []any
	alias    string
	want     eventPlan
	built    bool
}

const planWindowStart = "'2026-01-01T00:00:00.000000Z'"

// eventPlanStatements 는 event 를 project + kind 로 거르는 문장 전부다. 목록이 소스와 같은지는 TestEventPlanStatementsAreListed
// 가 잰다. 실행 중에 잇는 문장(convFinishesSQL)은 IN 목록의 길이마다 계획이 달라(SQLite) 길이 둘을 잰다.
var eventPlanStatements = []eventPlanStatement{
	{"CloseDeclarationsByItem", closeDeclarationsSQL, "'p03', 5000", []any{"p03", 5000}, "event", planProjectKindOrdered, false},
	{"QueueReproduction 마무리 창", reproFinishesSQL, "'p03', 20", []any{"p03", 20}, "event", planProjectKindOrdered, false},
	{"QueueReproduction 추가", reproAddsSQL, "'p03', 1", []any{"p03", 1}, "event", planProjectKindFromID, false},
	{"AckReach", ackReachSQL, "chr(31), 'p03'" + strings.Repeat(", "+planWindowStart, 4),
		[]any{convKeySep, "p03", "2026-01-01T00:00:00.000000Z", "2026-01-01T00:00:00.000000Z", "2026-01-01T00:00:00.000000Z",
			"2026-01-01T00:00:00.000000Z"}, "e", planProjectKindOrWindow, false},
	{"ConversationLifecycle 카드 하나", convFinishesSQL("?"), "'p03', 's3', " + planWindowStart,
		[]any{"p03", "s3", "2026-01-01T00:00:00.000000Z"}, "event", planWindow, true},
	{"ConversationLifecycle 카드 셋", convFinishesSQL("?,?,?"), "'p03', 's3', 's23', 's43', " + planWindowStart,
		[]any{"p03", "s3", "s23", "s43", "2026-01-01T00:00:00.000000Z"}, "event", planWindow, true},
}

// seedProjectKindEvents 는 프로젝트 스물에 걸친 합성 이벤트를 넣는다: 대부분 세션 박동 · 처방이고 마무리 · 추가는 드물다.
// 한 프로젝트의 마무리가 LIMIT(5000)보다 훨씬 적다 - 인덱스가 없을 때 일반 계획이 표 전체를 읽는 모양이다.
// Postgres 는 통계가 계획을 고르므로 넣은 뒤 ANALYZE 한다. 값은 합성이다(운영 원장과 무관하다).
const seedProjectKindEventsPG = `
	INSERT INTO event(at, project, session_id, kind, payload)
	SELECT to_char(timestamp '2026-01-01' + i * interval '10 second', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
	       'p' || lpad((i % 20)::text, 2, '0'), 's' || (i % 900),
	       CASE WHEN i % 167 = 0 THEN 'item.finish' WHEN i % 151 = 0 THEN 'item.add'
	            WHEN i % 7 = 0 THEN 'prescribe' ELSE 'session.beat' END,
	       '{"item":"i' || i || '","mode":"done"}'
	FROM generate_series(1, 60000) AS i;
	ANALYZE event;`

// planNode 는 EXPLAIN (FORMAT JSON) 의 계획 마디 하나다(쓰는 칸만).
type planNode struct {
	Type      string     `json:"Node Type"`
	Relation  string     `json:"Relation Name"`
	Index     string     `json:"Index Name"`
	IndexCond string     `json:"Index Cond"`
	Plans     []planNode `json:"Plans"`
}

func (n planNode) walk(f func(planNode)) {
	f(n)
	for _, c := range n.Plans {
		c.walk(f)
	}
}

// pgPlanFault 는 Postgres 계획 하나가 want 를 어기는 까닭이다. 지키면 "" 이다. 순수 함수다 - 판정 자체를 합성 계획으로 따로 잰다
// (TestEventPlanJudgesCatchTheBadShapes).
//
// ★ event 를 읽는 마디만 본다(Relation 이 event 이거나 인덱스 이름이 event_ 로 시작한다). 다른 표(session · judgment)의 마디는
// 이 증분의 몫이 아니다.
func pgPlanFault(root planNode, want eventPlan) string {
	var idx []planNode
	var bad []string
	sorted := false
	root.walk(func(n planNode) {
		switch {
		case n.Type == "Seq Scan" && n.Relation == "event":
			bad = append(bad, "event 를 Seq Scan 으로 읽는다")
		case strings.HasPrefix(n.Index, "event_"):
			idx = append(idx, n)
		case n.Type == "Sort" || n.Type == "Incremental Sort":
			sorted = true
		}
	})
	if len(idx) == 0 {
		bad = append(bad, "event 를 인덱스로 읽는 마디가 없다")
	}
	// 일반 계획이면 값 대신 매개변수가 남는다 - 맞춤 계획을 잰 것이 아님을 여기서 본다.
	generic := false
	for _, n := range idx {
		generic = generic || strings.Contains(n.IndexCond, "$")
	}
	if len(idx) > 0 && !generic {
		bad = append(bad, "잰 계획이 일반 계획이 아니다(Index Cond 에 $n 이 없다) - plan_cache_mode 가 안 걸렸다")
	}
	switch want {
	case planProjectKind, planProjectKindOrdered, planProjectKindFromID:
		for _, n := range idx {
			if n.Index != "event_by_project_kind" {
				bad = append(bad, n.Type+" "+n.Index+" 로도 읽는다")
			} else if !pgProjectKindRange(n) {
				bad = append(bad, "범위 조건이 project · kind 둘 다가 아니다: "+n.IndexCond)
			} else if want == planProjectKindFromID && !strings.Contains(n.IndexCond, "(id >= $") {
				bad = append(bad, "id 하한이 범위 조건에 없다(범위를 다 읽고 거른다): "+n.IndexCond)
			}
		}
		if want != planProjectKind && sorted {
			bad = append(bad, "id 순서를 인덱스에서 안 받고 다시 정렬한다 - LIMIT 에서 못 멈춘다")
		}
	case planWindow:
		windowed := false
		for _, n := range idx {
			windowed = windowed || pgWindowRange(n)
		}
		if !windowed {
			bad = append(bad, "at 하한으로 자른 인덱스 범위가 없다 - 그 프로젝트의 그 종류 이력을 다 읽고 거른다")
		}
	case planProjectKindOrWindow:
		for _, n := range idx {
			if !pgProjectKindRange(n) && !pgWindowRange(n) {
				bad = append(bad, n.Type+" "+n.Index+" 가 (project, kind) 범위도 창 범위도 아니다: "+n.IndexCond)
			}
		}
	}
	return strings.Join(bad, "; ")
}

// pgProjectKindRange 는 마디가 event_by_project_kind 의 (project, kind) 범위인가다.
func pgProjectKindRange(n planNode) bool {
	return n.Index == "event_by_project_kind" && strings.Contains(n.IndexCond, "(project = ") && strings.Contains(n.IndexCond, "(kind = ")
}

// pgWindowRange 는 마디의 범위 조건에 at 하한(매개변수)이 있는가다.
func pgWindowRange(n planNode) bool { return strings.Contains(n.IndexCond, "(at >= $") }

// liteWindowIndex 는 SQLite 계획 줄이 시각 창(at 하한)으로 자른 인덱스 범위인지 알아본다.
var liteWindowIndex = regexp.MustCompile(`USING INDEX event_by_(session|kind) \([^)]*\bat>\?\)`)

// liteProjectKindIndex 는 SQLite 계획 줄이 event_by_project_kind 의 (project, kind) 범위인지 알아본다.
const liteProjectKindIndex = "USING INDEX event_by_project_kind (project=? AND kind=?"

// litePlanFault 는 SQLite 계획(EXPLAIN QUERY PLAN 의 줄들)이 want 를 어기는 까닭이다. 지키면 "" 이다. 순수 함수다.
// alias 로 event 를 읽는 줄(SCAN <alias> · SEARCH <alias>)만 본다.
func litePlanFault(lines []string, alias string, want eventPlan) string {
	var ev, bad []string
	sorted := false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "SCAN "+alias+" ") || l == "SCAN "+alias:
			bad = append(bad, "event 를 훑는다: "+l)
		case strings.HasPrefix(l, "SEARCH "+alias+" "):
			ev = append(ev, l)
		case strings.Contains(l, "TEMP B-TREE FOR ORDER BY"):
			sorted = true
		}
	}
	if len(ev) == 0 {
		bad = append(bad, "event 를 인덱스 범위로 읽는 줄이 없다")
	}
	for _, l := range ev {
		switch want {
		case planProjectKind, planProjectKindOrdered, planProjectKindFromID:
			if !strings.Contains(l, liteProjectKindIndex) {
				bad = append(bad, "event_by_project_kind 의 (project, kind) 범위가 아니다: "+l)
			} else if want == planProjectKindFromID && !strings.Contains(l, "kind=? AND id>?)") {
				bad = append(bad, "id 하한이 범위 조건에 없다(범위를 다 읽고 거른다): "+l)
			}
		case planWindow:
			if !liteWindowIndex.MatchString(l) {
				bad = append(bad, "at 하한으로 자른 인덱스 범위가 아니다: "+l)
			}
		case planProjectKindOrWindow:
			if !strings.Contains(l, liteProjectKindIndex) && !liteWindowIndex.MatchString(l) {
				bad = append(bad, "(project, kind) 범위도 창 범위도 아니다: "+l)
			}
		}
	}
	if (want == planProjectKindOrdered || want == planProjectKindFromID) && sorted {
		bad = append(bad, "id 순서를 인덱스에서 안 받고 다시 정렬한다")
	}
	return strings.Join(bad, "; ")
}

func TestEventPlanStatementsHoldTheirPlan(t *testing.T) {
	t.Run("postgres 일반 계획", func(t *testing.T) {
		ctx := context.Background()
		s := newPGStore(t, lockTimeout)
		pgExec(t, s, seedProjectKindEventsPG)
		c, err := s.db.raw.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		simple := pgx.QueryExecModeSimpleProtocol // PREPARE · EXECUTE 를 서버의 SQL 로 보낸다(드라이버의 prepared 문이 아니다)
		if _, err := c.ExecContext(ctx, `SET plan_cache_mode = force_generic_plan`, simple); err != nil {
			t.Fatal(err)
		}
		for i, st := range eventPlanStatements {
			t.Run(st.name, func(t *testing.T) {
				bound, err := enginePG.bind(st.sql)
				if err != nil {
					t.Fatal(err)
				}
				name := "pk" + strconv.Itoa(i)
				if _, err := c.ExecContext(ctx, "PREPARE "+name+" AS "+bound, simple); err != nil {
					t.Fatalf("PREPARE: %v", err)
				}
				var raw string
				if err := c.QueryRowContext(ctx, "EXPLAIN (FORMAT JSON) EXECUTE "+name+"("+st.pgArgs+")", simple).Scan(&raw); err != nil {
					t.Fatalf("EXPLAIN: %v", err)
				}
				var plans []struct {
					Plan planNode `json:"Plan"`
				}
				if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
					t.Fatalf("계획을 못 읽었다(%v): %s", err, raw)
				}
				if why := pgPlanFault(plans[0].Plan, st.want); why != "" {
					t.Fatalf("일반 계획이 어긋났다 - %s:\n%s", why, raw)
				}
			})
		}
	})

	t.Run("sqlite", func(t *testing.T) {
		s := newStore(t)
		for _, st := range eventPlanStatements {
			t.Run(st.name, func(t *testing.T) {
				rows, err := s.db.raw.Query("EXPLAIN QUERY PLAN "+st.sql, st.liteArgs...)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var details []string
				for rows.Next() {
					var id, parent, unused int
					var d string
					if err := rows.Scan(&id, &parent, &unused, &d); err != nil {
						t.Fatal(err)
					}
					details = append(details, d)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if why := litePlanFault(details, st.alias, st.want); why != "" {
					t.Fatalf("계획이 어긋났다 - %s: %s", why, strings.Join(details, " | "))
				}
			})
		}
	})
}

// TestEventPlanJudgesCatchTheBadShapes 는 두 판정이 어긋난 계획을 실제로 잡는지 합성 계획으로 잰다 - 판정이 무르면 위 시험이
// 초록으로 숨는다. 모양은 리뷰가 짚은 갈래다: id 하한이 사후 거름으로 밀린 계획, 창 없이 프로젝트 이력을 다 읽는 계획.
func TestEventPlanJudgesCatchTheBadShapes(t *testing.T) {
	pk := func(cond string) planNode {
		return planNode{Type: "Bitmap Index Scan", Index: "event_by_project_kind", IndexCond: cond}
	}
	heap := func(kids ...planNode) planNode {
		return planNode{Type: "Bitmap Heap Scan", Relation: "event", Plans: kids}
	}
	good := map[eventPlan]planNode{
		planProjectKindFromID: heap(pk("((project = $1) AND (kind = 'item.add'::text) AND (id >= $2))")),
		planWindow: {Type: "Sort", Plans: []planNode{heap(planNode{Type: "BitmapAnd", Plans: []planNode{
			pk("((project = $1) AND (kind = 'item.finish'::text))"),
			{Type: "Bitmap Index Scan", Index: "event_by_session", IndexCond: "((session_id = $2) AND (at >= $3))"},
		}})}},
	}
	for want, plan := range good {
		if why := pgPlanFault(plan, want); why != "" {
			t.Errorf("맞는 합성 계획(%d)을 거절했다: %s", want, why)
		}
	}
	for _, c := range []struct {
		name string
		plan planNode
		want eventPlan
	}{
		{"id 하한이 사후 거름", heap(pk("((project = $1) AND (kind = 'item.add'::text))")), planProjectKindFromID},
		{"창 없이 프로젝트 이력", heap(pk("((project = $1) AND (kind = 'item.finish'::text))")), planWindow},
		{"표 훑기", planNode{Type: "Seq Scan", Relation: "event"}, planWindow},
		{"다른 인덱스", heap(planNode{Type: "Bitmap Index Scan", Index: "event_by_kind", IndexCond: "(kind = $1)"}), planProjectKind},
		{"창 없는 다른 인덱스", heap(planNode{Type: "Bitmap Index Scan", Index: "event_by_kind", IndexCond: "(kind = $1)"}),
			planProjectKindOrWindow},
		{"다시 정렬", planNode{Type: "Sort", Plans: []planNode{heap(pk("((project = $1) AND (kind = 'x'::text))"))}}, planProjectKindOrdered},
		{"맞춤 계획", heap(pk("((project = 'p03'::text) AND (kind = 'item.add'::text) AND (id >= '1'::bigint))")), planProjectKindFromID},
	} {
		if pgPlanFault(c.plan, c.want) == "" {
			t.Errorf("Postgres 판정이 %s 계획을 통과시켰다", c.name)
		}
	}

	if why := litePlanFault([]string{"SEARCH event USING INDEX event_by_project_kind (project=? AND kind=? AND id>?)"}, "event",
		planProjectKindFromID); why != "" {
		t.Errorf("맞는 SQLite 계획을 거절했다: %s", why)
	}
	for _, c := range []struct {
		name  string
		lines []string
		alias string
		want  eventPlan
	}{
		{"id 하한이 사후 거름", []string{"SEARCH event USING INDEX event_by_project_kind (project=? AND kind=?)"}, "event",
			planProjectKindFromID},
		{"창 없이 프로젝트 이력", []string{"SEARCH event USING INDEX event_by_project_kind (project=? AND kind=?)",
			"USE TEMP B-TREE FOR ORDER BY"}, "event", planWindow},
		{"표 훑기", []string{"SCAN e"}, "e", planProjectKind},
		{"다시 정렬", []string{"SEARCH event USING INDEX event_by_project_kind (project=? AND kind=?)", "USE TEMP B-TREE FOR ORDER BY"},
			"event", planProjectKindOrdered},
		{"다른 인덱스", []string{"SEARCH e USING INDEX event_by_kind (kind=?)"}, "e", planProjectKind},
		{"창 없는 다른 인덱스", []string{"SEARCH e USING INDEX event_by_project_kind (project=? AND kind=?)",
			"SEARCH e USING INDEX event_by_kind (kind=?)"}, "e", planProjectKindOrWindow},
	} {
		if litePlanFault(c.lines, c.alias, c.want) == "" {
			t.Errorf("SQLite 판정이 %s 계획을 통과시켰다", c.name)
		}
	}
}

// eventProjectKindShape 는 「event 를 읽으며 project 를 자리표로 거르고 kind 로 거른다」인 문장 조각을 알아본다. 순서 · LIMIT 은 안
// 본다 - LIMIT 없는 문장(재생산율의 추가 셈 · 집계)도 같은 범위를 탄다.
var eventProjectKindShape = []*regexp.Regexp{
	regexp.MustCompile(`(?is)\bFROM\s+event\b`),
	regexp.MustCompile(`(?is)\bproject\s*=\s*\?`),
	regexp.MustCompile(`(?is)\bkind\s*(=|IN\b)`),
}

// eventPlanSites 는 그 꼴의 문장 조각이 상수가 아닌 자리에 있어도 되는 함수와 그 까닭이다.
var eventPlanSites = map[site]string{
	{"conversation.go", "convFinishesSQL"}: "IN 목록을 실행 중에 잇는다 - eventPlanStatements 가 이 함수로 길이 둘을 잰다",
	{"item.go", "ClaimHolderAt"}: "재지 않는다: 진단용이라 업무 경로의 호출자가 없다. (project = ? OR project IS NULL) 이라 " +
		"SQLite 는 event_by_kind(kind, at) 로 그 종류를 다 읽는다(Postgres 는 event_by_project_kind 둘을 BitmapOr) - 문장을 " +
		"둘로 나눠야 범위가 서는데 진단 하나에 SQL 두 벌을 둘 까닭이 없다",
}

// TestEventPlanStatementsAreListed 는 store 의 Go 소스에서 그 꼴의 문장 조각이 eventPlanStatements 의 상수이거나 eventPlanSites 의
// 함수 안에만 있는지 잰다. 같은 꼴을 새로 쓰고 목록에 안 더하면 그 문장의 계획을 아무도 안 잰다.
//
// ★ 문자열 조각을 하나씩 본다. 실행 중에 이어 붙이는 문장(IN 목록 뒤에 거름이 오는 것)은 조각마다 보므로 꼴이 한 조각에
// 다 있어야 잡힌다 - 그래서 그 문장은 만드는 함수(convFinishesSQL)로 목록에 둔다.
func TestEventPlanStatementsAreListed(t *testing.T) {
	listed := map[string]bool{}
	for _, st := range eventPlanStatements {
		listed[st.sql] = true
	}
	src := parseStoreSource(t)
	foundSQL := map[string]bool{}
	seenSite := map[site]bool{}
	var bad []string
	src.each(func(at site, n ast.Node) {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		q, err := strconv.Unquote(lit.Value)
		if err != nil {
			return
		}
		for _, re := range eventProjectKindShape {
			if !re.MatchString(q) {
				return
			}
		}
		switch _, bySite := eventPlanSites[at]; {
		case listed[q]:
			foundSQL[q] = true
		case bySite:
			seenSite[at] = true
		default:
			bad = append(bad, at.String()+" "+src.fset.Position(lit.Pos()).String()+": "+clip(strings.Join(strings.Fields(q), " "), 120))
		}
	})
	if len(bad) > 0 {
		t.Errorf("event 를 project + kind 로 거르는 문장이 목록 밖에 있다 - 상수로 두고 eventPlanStatements 에 더하거나(지킬 계획과 "+
			"함께) 까닭과 함께 eventPlanSites 에 둬라:\n%s", strings.Join(bad, "\n"))
	}
	// 목록이 낡지 않았는지: 상수 문장은 그 글 그대로 소스에 있어야 하고, 자리는 그 함수 안에 그 꼴이 있어야 한다.
	for _, st := range eventPlanStatements {
		if !st.built && !foundSQL[st.sql] {
			t.Errorf("목록의 %s 문장이 소스에서 그 꼴로 안 보인다 - 정규식이 낡았거나 문장이 바뀌었다", st.name)
		}
	}
	for at, why := range eventPlanSites {
		if !seenSite[at] {
			t.Errorf("자리 목록이 낡았다: %s(%s)에 그 꼴의 조각이 없다", at, clip(why, 40))
		}
	}
}
