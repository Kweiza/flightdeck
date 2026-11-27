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

// 「이 프로젝트의 이 종류 이벤트를 id 순으로」 읽는 문장(event.go 의 closeDeclarationsSQL 묶음)이 두 엔진에서
// event_by_project_kind(project, kind, id) 를 타는지 잠근다(증분 019).
//
// ★ Postgres 는 일반 계획(generic plan)을 강제해서 잰다. 서버는 pgx 의 문 캐시로 이 문장을 prepared 문으로 두고, 서버가
// 여섯 번째 실행부터 일반 계획으로 바꿀 수 있다 - 그 계획은 project 값을 모른다. 인덱스가 없던 때의 일반 계획은 기본키를
// 거꾸로 훑으며 거르는 쪽이었고, 그 프로젝트의 행이 LIMIT 보다 적으면 표 전체를 읽었다. 맞춤 계획(custom)만 재면 그
// 갈래를 못 본다 - 값을 넣은 계획은 project 의 빈도를 알고 다른 인덱스를 고른다.
// ★ plan_cache_mode 는 이 시험의 연결 하나에만 건다(SET, 세션 단위). 서버의 연결 인자(pgSessionParams)에는 없다.

// projectKindStatement 는 잴 문장 하나다. pgArgs 는 EXECUTE 에 넣는 SQL 상수 목록, liteArgs 는 같은 값의 SQLite 인자다.
type projectKindStatement struct {
	name     string
	sql      string
	pgArgs   string
	liteArgs []any
	ordered  bool // ORDER BY id … LIMIT 이다 - 인덱스가 순서를 줘야 LIMIT 에서 멈춘다(정렬 단계가 없어야 한다)
}

// projectKindStatements 는 event 를 project + kind 로 걸러 id 순으로 읽는 문장 전부다. 목록이 소스와 같은지는
// TestProjectKindStatementsAreListed 가 잰다.
var projectKindStatements = []projectKindStatement{
	{"CloseDeclarationsByItem", closeDeclarationsSQL, "'p03', 5000", []any{"p03", 5000}, true},
	{"QueueReproduction 마무리 창", reproFinishesSQL, "'p03', 20", []any{"p03", 20}, true},
	{"QueueReproduction 추가", reproAddsSQL, "'p03', 1", []any{"p03", 1}, false},
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

func TestProjectKindStatementsUseTheIndex(t *testing.T) {
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
		for i, st := range projectKindStatements {
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
				var used, generic, sorted bool
				var other []string
				plans[0].Plan.walk(func(n planNode) {
					switch {
					case n.Type == "Seq Scan" && n.Relation == "event":
						other = append(other, "Seq Scan")
					case n.Index == "event_by_project_kind":
						used = true
						// 일반 계획이면 값 대신 매개변수가 남는다 - 맞춤 계획을 잰 것이 아님을 여기서 본다.
						generic = strings.Contains(n.IndexCond, "$1")
						if !strings.Contains(n.IndexCond, "project") || !strings.Contains(n.IndexCond, "kind") {
							other = append(other, "거름이 project · kind 둘 다가 아니다: "+n.IndexCond)
						}
					case n.Index != "":
						other = append(other, n.Type+" "+n.Index)
					case n.Type == "Sort" || n.Type == "Incremental Sort":
						sorted = true
					}
				})
				switch {
				case !used || len(other) > 0:
					t.Fatalf("일반 계획이 event_by_project_kind 만 타지 않는다(탄 것 %v, 그 밖 %v):\n%s", used, other, raw)
				case !generic:
					t.Fatalf("잰 계획이 일반 계획이 아니다(Index Cond 에 $1 이 없다) - plan_cache_mode 가 안 걸렸다:\n%s", raw)
				case st.ordered && sorted:
					t.Fatalf("id 순서를 인덱스에서 안 받고 다시 정렬한다 - LIMIT 에서 못 멈춘다:\n%s", raw)
				}
			})
		}
	})

	t.Run("sqlite", func(t *testing.T) {
		s := newStore(t)
		for _, st := range projectKindStatements {
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
				plan := strings.Join(details, " | ")
				if !strings.Contains(plan, "USING INDEX event_by_project_kind (project=? AND kind=?") ||
					strings.Contains(plan, "TEMP B-TREE") || len(details) != 1 {
					t.Fatalf("event_by_project_kind 범위 하나로 읽지 않는다: %s", plan)
				}
			})
		}
	})
}

// projectKindShape 는 「event 를 project 로 거르고 kind 로 거르고 id 순으로 LIMIT 까지」인 문장 조각을 알아본다.
var projectKindShape = regexp.MustCompile(`(?is)\bFROM\s+event\b.*\bproject\s*=\s*\?.*\bkind\s*(=|IN\b).*\bORDER\s+BY\s+id\b.*\bLIMIT\b`)

// TestProjectKindStatementsAreListed 는 store 의 Go 소스에서 그 꼴의 문장이 projectKindStatements 의 상수뿐인지 잰다.
// 같은 꼴을 새로 쓰고 목록에 안 더하면 그 문장의 계획을 아무도 안 잰다.
//
// ★ 한계: 문자열 조각을 하나씩 본다. 실행 중에 이어 붙이는 문장(IN 목록 뒤에 거름이 오는 것)은 조각마다 보므로 꼴이 한
// 조각에 다 있어야 잡힌다.
func TestProjectKindStatementsAreListed(t *testing.T) {
	listed := map[string]bool{}
	for _, st := range projectKindStatements {
		listed[st.sql] = true
	}
	src := parseStoreSource(t)
	var found int
	var bad []string
	src.each(func(at site, n ast.Node) {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		q, err := strconv.Unquote(lit.Value)
		if err != nil || !projectKindShape.MatchString(q) {
			return
		}
		found++
		if !listed[q] {
			bad = append(bad, at.String()+" "+src.fset.Position(lit.Pos()).String()+": "+clip(strings.Join(strings.Fields(q), " "), 120))
		}
	})
	// 지금 그 꼴은 둘이다(종료 선언 · 재생산율 마무리 창). 덜 찾으면 꼴 정규식이 낡은 것이다.
	if found < 2 {
		t.Fatalf("그 꼴의 문장을 %d개만 찾았다 - 정규식이 지금 문장을 못 알아본다", found)
	}
	if len(bad) > 0 {
		t.Fatalf("event 를 project + kind 로 걸러 id 순 LIMIT 으로 읽는 문장이 목록 밖에 있다 - event.go 의 closeDeclarationsSQL "+
			"묶음에 두고 projectKindStatements 에 더하라:\n%s", strings.Join(bad, "\n"))
	}
}
