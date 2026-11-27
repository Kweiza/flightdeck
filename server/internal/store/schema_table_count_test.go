package store

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// declaredTableRe 는 **사람이 선언한** 표를 찾는다.
//
// ★ `IF NOT EXISTS` 를 반드시 건너뛴다. `schema_version` 하나가 그 형태인데,
// `^CREATE TABLE [a-z_]` 같은 순진한 패턴은 그 한 줄을 놓쳐 21 을 20 으로 센다.
// 이 함정으로 세 세션이 각자 다른 수(21·23·28)를 보고했다 — 패턴에 못박아 둔다.
var declaredTableRe = regexp.MustCompile(
	`(?im)^\s*CREATE\s+(VIRTUAL\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)

// declaredTables 는 SQLite 의 schema.sql 과 증분 전부가 선언하는 표 이름을 정렬해 낸다.
//
// 여기서 세는 것은 **정의가 이 레포에 있는 표**뿐이다. 살아 있는 DB 의 `sqlite_master`
// 는 이보다 큰 수를 내는데(아래 시험의 주석을 보라) 그 차이는 엔진이 만든 것이라
// 우리가 세는 대상이 아니다.
func declaredTables(t *testing.T) []string {
	t.Helper()
	sources := map[string]string{"schema.sql": schemaSQL}
	for _, m := range migrations {
		sources[m.Name] = m.SQL
	}
	return declaredIn(t, sources)
}

// pgDeclaredTables 는 PG 사다리(준비 + 001_baseline.sql + 기준선 위 증분)가 선언하는 표 이름을 정렬해 낸다.
func pgDeclaredTables(t *testing.T) []string {
	t.Helper()
	sources := map[string]string{"pg/bootstrap.sql": pg.Bootstrap, "pg/001_baseline.sql": pg.Baseline}
	for _, st := range pg.Steps {
		sources["pg 증분 "+st.Name] = st.SQL
	}
	return declaredIn(t, sources)
}

// declaredIn 은 SQL 묶음(이름 → 본문)이 선언하는 표 이름을 정렬해 낸다.
func declaredIn(t *testing.T, sources map[string]string) []string {
	t.Helper()
	seen := make(map[string]string, 32)
	var names []string
	for src, sql := range sources {
		if strings.TrimSpace(sql) == "" {
			t.Fatalf("%s 의 SQL 이 비었다 — //go:embed 가 안 걸렸을 수 있다", src)
		}
		for _, m := range declaredTableRe.FindAllStringSubmatch(sql, -1) {
			name := m[2]
			// ★ 같은 표를 두 자리에서 선언하면 신규 설치와 업그레이드가 다른 모양의
			// DB 를 갖는다(BaseSchemaVersion 주석의 그 경로다). 수를 세기 전에 막는다.
			if prev, dup := seen[name]; dup {
				t.Fatalf("표 %q 가 %s 와 %s 두 자리에서 선언됐다 — 정의는 한 자리에만 둔다", name, prev, src)
			}
			seen[name] = src
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// wantTables 는 데이터 모델의 표 이름 전부다 — **PG 기준선이 정본이다.**
//
// ★ 재설계 스펙의 「스키마」 절이 정한 줄기다: 선언 28(SQLite) → PG 기준선 26 → 단위마다 늘어난다.
// 표를 더하거나 지우는 단위는 **이 목록을 고친다.** SQLite 쪽 목록은 이것에 sqliteOnlyTables 를 더한 것으로
// 얼어 있다(옮기는 동안만 산다).
var wantTables = []string{
	"change_set",
	"claim",
	"counter",
	"event",
	"footprint",
	"idempotency", // SQLite 증분 002
	"item",
	"item_after",
	"item_revision", // SQLite 증분 016
	"job",
	"judgment",
	"judgment_link",
	"landing_queue",          // SQLite 증분 003 — 랜딩 순서 큐
	"landing_queue_resource", // SQLite 증분 008 — 줄 행이 자원 집합을 갖는다
	"machine",
	"pick_eval",
	"project",
	"project_member", // SQLite 증분 014 — 루트가 멤버 프로젝트 명부를 갖는다
	"ref_state",
	"resource_hold",
	"schema_version", // SQLite 에서는 CREATE TABLE IF NOT EXISTS — 순진한 패턴이 놓치는 그 한 줄
	"session",
	"session_inbox", // SQLite 증분 018, 세션이 받는 곳(board/README 「세션에 알림」)
	"session_workspace",
	"signal",
	"snapshot",
}

// sqliteOnlyTables 는 SQLite 가 선언했고 PG 기준선이 일부러 안 옮긴 표와 그 까닭이다.
var sqliteOnlyTables = map[string]string{
	"judgment_fts":    "CREATE VIRTUAL TABLE(FTS5) — PG 는 judgment 의 pg_trgm 식 인덱스가 그 자리다",
	"item_dependents": "증분 011 이 걷은 죽은 표 — 선언만 schema.sql 에 남아 있다",
}

// TestDeclaredTablesMatchDesign 은 설계가 못박은 표 목록을 스키마 쪽에서 지킨다.
//
// ★ 왜 문서가 아니라 여기서 지키나 — `TestToolTableIsTen`(mcpsrv/protocol_test.go) 와 같은
// 규율이다. 표를 **더하는 사람**의 빨간불이 여기서 나야 그 사람이 DESIGN 을 같이 고친다.
// 문서를 읽는 시험은 표를 안 더한 사람까지 세우고, 문서 편집이 도는 동안 계속 빨갛다.
//
// 이름 목록을 통째로 못박는다(수만 세지 않는다). 표 하나를 지우고 하나를 더하면
// 수는 그대로인데 데이터 모델은 달라지고, 그 변경이 리뷰에서 안 보이면 안 된다.
//
// 두 엔진을 함께 본다: PG 기준선의 선언 = wantTables(26), SQLite 의 선언 = wantTables + sqliteOnlyTables(28).
// DESIGN 「데이터 모델」 절의 「테이블 N개」는 PG 기준선의 26 이다(SQLite 의 28 은 옮기는 동안만 사는 사본의 수다).
//
// SQLite 의 살아 있는 DB `sqlite_master` 는 28 보다 **4 큰** 수를 낸다(2026-09-16 운영 원장
// mode=ro 사본 실측: 판 16 에서 31). 축이 **둘** 어긋나기 때문이다:
//
//	⊕ SQLite 가 다섯을 더 만든다 — FTS5 가 judgment_fts 뒤에 자동 생성하는 그림자 표
//	  4개(_config·_data·_docsize·_idx)와, AUTOINCREMENT 이 있으면 생기는 sqlite_sequence 1개.
//	⊖ 증분 011 이 item_dependents 를 DROP 해서 하나가 빠진다. 그런데도 SQLite 목록이 그
//	  이름을 계속 세는 이유는 **선언이 schema.sql 에 남아 있어서**다(정의를 두 자리에
//	  두지 않는다는 규율). 세는 축이 "선언"이지 "살아 있는 DB"가 아니라는 뜻이다.
//
// **그 다섯은 우리가 정한 값이 아니다** — fts5 옵션이나 SQLite 판이 바뀌면 조용히 달라진다.
func TestDeclaredTablesMatchDesign(t *testing.T) {
	sqliteWant := append([]string{}, wantTables...)
	for name := range sqliteOnlyTables {
		sqliteWant = append(sqliteWant, name)
	}
	sort.Strings(sqliteWant)

	for _, c := range []struct {
		engine    string
		got, want []string
	}{
		{"PG", pgDeclaredTables(t), wantTables},
		{"SQLite", declaredTables(t), sqliteWant},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: 선언한 표가 %d개다 — 이 시험은 %d개를 안다.\n"+
				"표를 더했거나 지웠으면 **wantTables 와 DESIGN 「데이터 모델」 절의 '테이블 N개' 도 같이 고쳐라**"+
				"(그 숫자가 곧 이 레포의 계약이고, 틀리면 아무도 계약을 검산할 수 없다).\n"+
				"실측: %v\n기대: %v", c.engine, len(c.got), len(c.want), c.got, c.want)
		}
	}
}

// TestPGStepsFollowTheSQLiteVersion 은 PG 사다리(기준선 + pg.Steps)가 지금 판까지 빈칸 없이 닿고, 기준선 위의 SQLite
// 증분마다 같은 판 번호의 PG 증분이 있는지 본다.
//
// ★ 옮기는 동안 SQLite 에 증분이 오르면 PG 는 그 증분을 모른 채 같은 판 번호를 말하게 된다 - 판 재기 · 이관 · 역이관의
// 「같은 판」 관문이 모양이 다른 두 원장을 같다고 받는다. 그 자리에서 빨갛게 한다. 두 증분이 **같은 뜻**인지(표 · 칸 ·
// 인덱스 …)는 pg 패키지의 TestBaselineHasTheSameShapeAsSQLite 가 두 엔진의 카탈로그를 맞대 잰다 - 여기는 판 번호의 축이다.
func TestPGStepsFollowTheSQLiteVersion(t *testing.T) {
	if _, err := UpgradeSteps(pg.BaselineVersion, SchemaVersion, pgMigrations); err != nil {
		t.Fatalf("PG 기준선(%d)에서 지금 판(%d)으로 가는 PG 증분이 없다 - SQLite 증분의 뜻을 pg.Steps 에도 실어라: %v",
			pg.BaselineVersion, SchemaVersion, err)
	}
	pgHas := map[int]bool{}
	for _, m := range pgMigrations {
		if m.To <= pg.BaselineVersion || m.To > SchemaVersion || pgHas[m.To] {
			t.Errorf("PG 증분 %d(%s)이 기준선 %d 위 지금 판 %d 안의 한 칸이 아니다(겹치거나 넘었다)",
				m.To, m.Name, pg.BaselineVersion, SchemaVersion)
		}
		pgHas[m.To] = true
	}
	for _, m := range migrations {
		if m.To > pg.BaselineVersion && !pgHas[m.To] {
			t.Errorf("SQLite 증분 %d(%s)에 짝인 PG 증분이 없다", m.To, m.Name)
		}
	}
}

// TestSchemaVersionTableIsCounted 는 위 목록이 `IF NOT EXISTS` 한 줄을 계속 세는지 본다.
//
// 이것만 따로 세우는 이유: 표 수가 틀렸던 세 번의 보고가 **전부** 이 한 줄에서 갈렸다.
// 목록 시험이 어떤 이유로 느슨해져도 이 갈래만은 남는다.
func TestSchemaVersionTableIsCounted(t *testing.T) {
	if !strings.Contains(schemaSQL, "CREATE TABLE IF NOT EXISTS schema_version") {
		t.Skip("schema_version 의 선언 형태가 바뀌었다 — 이 시험이 지키던 함정이 사라졌다")
	}
	if !slices.Contains(declaredTables(t), "schema_version") {
		t.Fatal("schema_version 이 안 세졌다 — `IF NOT EXISTS` 를 건너뛰는 패턴이 깨졌다. " +
			"이 상태로 센 수는 실제보다 1 작다")
	}
}

// migrationsDropTables 는 증분이 **걷어낸** 표다.
//
// ★ 이 목록이 필요한 이유는 declaredTables 가 `CREATE TABLE` **텍스트만** 세기 때문이다.
// DROP 은 안 본다 — 그래서 증분이 표를 걷으면 선언 목록과 실제 DB 가 갈리는데, 그 어긋남을
// 잡는 시험이 하나도 없었다. 그 구멍은 추측이 아니라 실측이다: migrate_guard_test.go 의
// neverExempt 주석이 "증분에 DROP TABLE 을 넣어 돌려 보니 지목된 두 시험이 둘 다 통과했다"고
// 적어 뒀고, 그중 하나가 바로 위 TestDeclaredTablesMatchDesign 이다.
//
// ★ 표를 걷는 증분을 실으면 **여기에 그 이름을 더해야 한다.** 안 더하면 아래 시험이 빨개진다.
var migrationsDropTables = []string{
	"item_dependents", // 011 · 죽은 표를 걷었다
}

// TestLiveSchemaAfterAllMigrationsMatchesTheDeclaredList 는 **실제로 올린 DB** 를 센다.
//
// ★ 위 TestDeclaredTablesMatchDesign 과 짝이다. 저쪽은 "사람이 선언한 것"을 세고 여기는
// "실제로 남은 것"을 센다. 둘이 갈리는 유일한 이유가 DROP 이고, 그 차이를 migrationsDropTables
// 가 명시한다. 한쪽만 있으면 DROP 이 조용히 지나가거나(저쪽만) 선언의 계약이 사라진다(여기만).
func TestLiveSchemaAfterAllMigrationsMatchesTheDeclaredList(t *testing.T) {
	// ★ 그림자 표는 우리가 정한 값이 아니다 — FTS5 가 judgment_fts 뒤에 만드는 넷이다(sqlite_sequence 는 liveTables 가
	//   이미 뺀다). fts5 옵션이나 SQLite 판이 바뀌면 조용히 달라지므로 세는 대상에서 뺀다(위 시험의 머리말과 같은 규율).
	var live []string
	for _, n := range liveTables(t, newStore(t)) {
		if !strings.HasPrefix(n, "judgment_fts_") {
			live = append(live, n)
		}
	}

	dropped := make(map[string]bool, len(migrationsDropTables))
	for _, n := range migrationsDropTables {
		dropped[n] = true
	}
	var want []string
	for _, n := range declaredTables(t) {
		if !dropped[n] {
			want = append(want, n)
		}
	}
	sort.Strings(want)

	if len(live) != len(want) {
		t.Fatalf("올린 DB 의 표가 %d개인데 선언−걷어낸 것은 %d개다.\n"+
			"표를 걷는 증분을 실었으면 migrationsDropTables 에 그 이름을 더하고 **DESIGN 「데이터 모델」 절도 같이 고쳐라**.\n"+
			"실측(DB): %v\n기대(선언−DROP): %v", len(live), len(want), live, want)
	}
	for i := range want {
		if live[i] != want[i] {
			t.Fatalf("정렬 %d번이 DB 에서는 %q, 선언−DROP 에서는 %q다.\n"+
				"실측(DB): %v\n기대(선언−DROP): %v", i, live[i], want[i], live, want)
		}
	}

	// ★ 걷었다고 적은 것이 **실제로 없는지**도 본다. 이것이 없으면 목록에 이름만 올려 두고
	//   실제로는 안 걷어도 위 대조가 통과한다(수도 목록도 맞는다).
	for _, n := range migrationsDropTables {
		for _, l := range live {
			if l == n {
				t.Errorf("migrationsDropTables 에 %q 가 있는데 DB 에 그대로 있다 — 증분이 안 걷었다", n)
			}
		}
	}
}

// TestPGLiveSchemaMatchesTheDeclaredList 는 기준선을 **실제로 깐** PG DB 를 센다.
//
// ★ 위 SQLite 짝과 같은 까닭이다: 선언 목록은 SQL 글자를 세고 여기는 DB 에 남은 것을 센다. PG 는 그림자 표를
// 만들지 않으므로(pg_trgm 도 표를 안 만든다) 걸러 낼 것 없이 둘이 같아야 한다.
func TestPGLiveSchemaMatchesTheDeclaredList(t *testing.T) {
	live := liveTables(t, newPGStore(t, lockTimeout))
	if !slices.Equal(live, wantTables) {
		t.Fatalf("기준선을 깐 PG DB 의 표가 %d개, 선언은 %d개다.\n실측(DB): %v\n기대(선언): %v",
			len(live), len(wantTables), live, wantTables)
	}
}
