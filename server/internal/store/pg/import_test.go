package pg_test

// 이관 카탈로그(pg.Tables)와 대조 규칙(verify.go)의 순수한 몫을 잰다. 두 원장을 실제로 옮기는 왕복 · 거절 시험은 store 의
// importsqlite_test.go 다.

import (
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/pg"
)

// 손으로 적은 이관 카탈로그가 두 엔진의 살아 있는 카탈로그와 같다: 표 목록(옮기지 않는 것을 뺀), 칸 이름 · 순서 · 타입 ·
// NOT NULL · 행 좌표 칸(SQLite 기본키) · identity · AUTOINCREMENT, 그리고 FK 순서(부모가 먼저).
func TestImportCatalogMatchesBothEngines(t *testing.T) {
	if pg.ImportVersion != store.SchemaVersion {
		t.Fatalf("이관 판 %d 이 SQLite 의 마지막 판 %d 과 다르다 - SQLite 판이 오르면 카탈로그를 그 판으로 다시 적어야 한다",
			pg.ImportVersion, store.SchemaVersion)
	}
	lite, pgdb := openSQLite(t), openPG(t)
	cat := map[string]pg.Table{}
	pos := map[string]int{}
	for i, tb := range pg.Tables() {
		if _, dup := cat[tb.Name]; dup {
			t.Fatalf("카탈로그에 %s 가 두 번 있다", tb.Name)
		}
		cat[tb.Name], pos[tb.Name] = tb, i
	}

	var liteTables []string
	for _, r := range strs(t, lite, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`) {
		if !pg.LeftBehind(r[0]) {
			liteTables = append(liteTables, r[0])
		}
	}
	var pgTables []string
	for _, r := range strs(t, pgdb, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`) {
		if !pg.LeftBehind(r[0]) {
			pgTables = append(pgTables, r[0])
		}
	}
	if len(liteTables) != len(cat) || len(pgTables) != len(cat) {
		t.Fatalf("표 수: 카탈로그 %d, SQLite %d %v, PG %d %v", len(cat), len(liteTables), liteTables, len(pgTables), pgTables)
	}
	for i := range liteTables {
		if _, ok := cat[liteTables[i]]; !ok {
			t.Errorf("SQLite 의 표 %s 가 카탈로그에 없다", liteTables[i])
		}
		if _, ok := cat[pgTables[i]]; !ok {
			t.Errorf("PG 의 표 %s 가 카탈로그에 없다", pgTables[i])
		}
	}

	kinds := map[string]pg.Kind{"INTEGER": pg.Int, "TEXT": pg.Text, "BLOB": pg.Bytes, "bigint": pg.Int, "text": pg.Text, "bytea": pg.Bytes}
	for _, tb := range pg.Tables() {
		// SQLite: 선언된 칸(rowid 자리는 빼고).
		ddl := strs(t, lite, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, tb.Name)
		if len(ddl) != 1 {
			t.Errorf("SQLite 에 %s 가 없다", tb.Name)
			continue
		}
		if got := strings.Contains(strings.ToUpper(ddl[0][0]), "AUTOINCREMENT"); got != tb.AutoIncrement {
			t.Errorf("%s AUTOINCREMENT: SQLite %v, 카탈로그 %v", tb.Name, got, tb.AutoIncrement)
		}
		var src []pg.Column
		for _, c := range tb.Cols {
			if !c.Rowid {
				src = append(src, c)
			}
		}
		cols := strs(t, lite, `SELECT name, type, "notnull", pk FROM pragma_table_info(?) ORDER BY cid`, tb.Name)
		if len(cols) != len(src) {
			t.Errorf("%s 칸 수: SQLite %d %v, 카탈로그 %d", tb.Name, len(cols), cols, len(src))
			continue
		}
		for i, c := range cols {
			w := src[i]
			inPK := c[3] != "0"
			if c[0] != w.Name || kinds[strings.ToUpper(c[1])] != w.Kind || (c[2] == "1" || inPK) != w.NotNull || inPK != w.Key {
				t.Errorf("%s.%s: SQLite (이름 %s 타입 %s notnull %s pk %s), 카탈로그 %+v", tb.Name, w.Name, c[0], c[1], c[2], c[3], w)
			}
		}
		// rowid 자리 칸은 SQLite 기본키가 없는 표의 행 좌표다.
		for _, c := range tb.Cols {
			if c.Rowid && (!c.Key || !c.Identity) {
				t.Errorf("%s.%s 는 rowid 자리라 행 좌표이고 identity 여야 한다: %+v", tb.Name, c.Name, c)
			}
		}

		// PG: 생성 칸(지문)을 뺀 칸 전부.
		var pgCols [][]string
		for _, c := range strs(t, pgdb, `SELECT column_name, data_type, is_nullable, coalesce(identity_generation, ''),
				coalesce(generation_expression, '') FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 ORDER BY ordinal_position`, tb.Name) {
			if c[4] != "" {
				if tb.DigestKey == "" || !strings.Contains(c[4], tb.DigestKey) {
					t.Errorf("%s 의 생성 칸 %s(%s)를 카탈로그가 모른다(DigestKey %q)", tb.Name, c[0], c[4], tb.DigestKey)
				}
				continue
			}
			pgCols = append(pgCols, c)
		}
		if len(pgCols) != len(tb.Cols) {
			t.Errorf("%s 칸 수: PG %d %v, 카탈로그 %d", tb.Name, len(pgCols), pgCols, len(tb.Cols))
			continue
		}
		for i, c := range pgCols {
			w := tb.Cols[i]
			if c[0] != w.Name || kinds[c[1]] != w.Kind || (c[2] == "NO") != w.NotNull || (c[3] == "ALWAYS") != w.Identity {
				t.Errorf("%s.%s: PG (이름 %s 타입 %s nullable %s identity %s), 카탈로그 %+v", tb.Name, w.Name, c[0], c[1], c[2], c[3], w)
			}
		}
		if tb.DigestKey != "" && tb.Index(tb.DigestKey) < 0 {
			t.Errorf("%s 의 DigestKey %q 가 칸에 없다", tb.Name, tb.DigestKey)
		}
		for _, c := range tb.Cols {
			if c.JSON && c.Kind != pg.Text {
				t.Errorf("%s.%s 는 JSON 칸인데 글이 아니다", tb.Name, c.Name)
			}
		}

		// FK 순서: 부모가 카탈로그에서 먼저다(자기 참조는 커밋까지 미룬 FK 가 받는다).
		for _, f := range strs(t, lite, `SELECT DISTINCT "table" FROM pragma_foreign_key_list(?)`, tb.Name) {
			parent := f[0]
			if parent != tb.Name && pos[parent] >= pos[tb.Name] {
				t.Errorf("%s 의 부모 %s 가 카탈로그에서 뒤에 있다(%d ≥ %d)", tb.Name, parent, pos[parent], pos[tb.Name])
			}
		}
	}
}

// Cell 은 Postgres 가 거절할 값을 사유와 함께 거르고, 바이트를 바꾸지 않는 저장 갈래는 받는다.
func TestCellFaults(t *testing.T) {
	text := pg.Column{Name: "t", Kind: pg.Text}
	notNull := pg.Column{Name: "n", Kind: pg.Text, NotNull: true}
	key := pg.Column{Name: "k", Kind: pg.Text, NotNull: true, Key: true}
	num := pg.Column{Name: "i", Kind: pg.Int}
	blob := pg.Column{Name: "b", Kind: pg.Bytes}
	doc := pg.Column{Name: "j", Kind: pg.Text, JSON: true}
	for _, c := range []struct {
		name  string
		col   pg.Column
		in    any
		want  any
		fault pg.Fault
	}{
		{"NULL 은 받는다", text, nil, nil, ""},
		{"빈 글은 NULL 이 아니다", notNull, "", "", ""},
		{"NOT NULL 칸의 NULL", notNull, nil, nil, pg.FaultNull},
		{"기본키의 NULL", key, nil, nil, pg.FaultKeyNull},
		{"글의 NUL", text, "a\x00b", "a\x00b", pg.FaultNUL},
		{"깨진 UTF-8", text, "\xff", "\xff", pg.FaultUTF8},
		{"글 칸의 BLOB 은 같은 바이트의 글이다", text, []byte("한글"), "한글", ""},
		{"글 칸의 BLOB 도 NUL 을 잰다", text, []byte{'a', 0}, "a\x00", pg.FaultNUL},
		{"글 칸의 정수", text, int64(1), int64(1), pg.FaultStorage},
		{"정수 칸의 글", num, "abc", "abc", pg.FaultStorage},
		{"정수 칸의 실수", num, 1.5, 1.5, pg.FaultStorage},
		{"정수는 받는다", num, int64(-7), int64(-7), ""},
		{"bytea 칸의 TEXT 는 같은 바이트다", blob, "x", []byte("x"), ""},
		{"bytea 는 NUL 을 받는다", blob, []byte{0, 1}, []byte{0, 1}, ""},
		{"깨진 JSON", doc, "[", "[", pg.FaultJSON},
		{"JSON 글 안의 \\u0000", doc, `{"a":"x\u0000"}`, `{"a":"x\u0000"}`, pg.FaultJSONNUL},
		{"JSON 키 안의 \\u0000", doc, `{"\u0000":1}`, `{"\u0000":1}`, pg.FaultJSONNUL},
		{"이스케이프된 백슬래시 뒤 u0000 은 글자다", doc, `["\\u0000"]`, `["\\u0000"]`, ""},
		{"JSON 은 받는다", doc, `{"a":[1,"b"]}`, `{"a":[1,"b"]}`, ""},
	} {
		got, fault := pg.Cell(c.col, c.in)
		if fault != c.fault || pg.RowHash([]any{got}) != pg.RowHash([]any{c.want}) {
			t.Errorf("%s: Cell(%v) = (%#v, %q), 기대 (%#v, %q)", c.name, c.in, got, fault, c.want, c.fault)
		}
	}
}

// 행 지문은 NULL 과 빈 글을 가르고, 칸 경계가 밀린 행을 가르고, 바이트 하나의 차이를 가른다.
func TestRowHashSeparates(t *testing.T) {
	for _, p := range [][2][]any{
		{{nil}, {""}},
		{{"ab", "c"}, {"a", "bc"}},
		{{nil, "a"}, {"a", nil}},
		{{"2026-01-01T00:00:00.000000Z"}, {"2026-01-01T00:00:00.000001Z"}},
		{{[]byte{}}, {nil}},
		{{int64(10)}, {int64(1)}},
	} {
		if pg.RowHash(p[0]) == pg.RowHash(p[1]) {
			t.Errorf("%#v 와 %#v 의 지문이 같다", p[0], p[1])
		}
	}
	if got, fault := pg.Cell(pg.Column{Name: "b", Kind: pg.Bytes, NotNull: true}, []byte(nil)); fault != "" || got.([]byte) == nil {
		t.Errorf("길이 0 BLOB(nil 조각)이 빈 bytea 가 아니다: (%#v, %q) - pgx 는 nil 조각을 NULL 로 보낸다", got, fault)
	}
	// 칸 타입은 표가 정하므로 같은 바이트의 글과 bytea 는 같은 값이다(Cell 이 그 둘을 서로 옮긴다).
	if pg.RowHash([]any{"x"}) != pg.RowHash([]any{[]byte("x")}) {
		t.Error("같은 바이트의 글과 bytea 의 지문이 다르다")
	}
}

// 대조는 행 순서를 묻지 않고, 중복 수까지 세며, 한쪽에만 있는 행을 양쪽으로 늘어놓는다.
func TestCompare(t *testing.T) {
	m := func(v, key string) pg.RowMark { return pg.RowMark{Hash: pg.RowHash([]any{v}), Key: key} }
	same := pg.Compare("t", []pg.RowMark{m("a", "1"), m("b", "2")}, []pg.RowMark{m("b", "2"), m("a", "1")})
	if !same.OK() || same.MissingSource+same.MissingTarget != 0 {
		t.Fatalf("순서만 다른 두 쪽이 어긋났다: %+v", same)
	}
	dup := pg.Compare("t", []pg.RowMark{m("a", "1"), m("a", "1")}, []pg.RowMark{m("a", "1"), m("b", "2")})
	if dup.OK() || dup.MissingTarget != 1 || dup.MissingSource != 1 ||
		len(dup.OnlySource) != 1 || dup.OnlySource[0] != "1" || len(dup.OnlyTarget) != 1 || dup.OnlyTarget[0] != "2" {
		t.Fatalf("중복과 바뀐 행을 못 갈랐다: %+v", dup)
	}
	var many []pg.RowMark
	for i := range pg.MaxListed + 5 {
		many = append(many, m(strings.Repeat("x", i+1), "k"))
	}
	cut := pg.Compare("t", many, nil)
	if cut.MissingTarget != pg.MaxListed+5 || len(cut.OnlySource) != pg.MaxListed {
		t.Fatalf("늘어놓기 상한: 수 %d · 목록 %d", cut.MissingTarget, len(cut.OnlySource))
	}
}

// 순번: 발급된 최고 번호가 있으면 그 다음부터, 없으면 1 부터 낸다. 대조는 다음 번호가 그 번호보다 커야 맞다.
func TestSequence(t *testing.T) {
	for _, c := range []struct {
		floor, next int64
	}{{0, 1}, {1, 2}, {41, 42}} {
		v, called := pg.Sequence(c.floor)
		if got := pg.NextValue(v, called); got != c.next {
			t.Errorf("floor %d: setval(%d, %v) 뒤 다음 번호 %d, 기대 %d", c.floor, v, called, got, c.next)
		}
		if !(pg.SequenceCheck{Floor: c.floor, Next: c.next}).OK() {
			t.Errorf("floor %d · 다음 %d 가 어긋남으로 나왔다", c.floor, c.next)
		}
	}
	if (pg.SequenceCheck{Floor: 3, Next: 3}).OK() {
		t.Error("발급된 번호를 다시 내는 순번이 맞다고 나왔다")
	}
}

// 지문 기본키는 지문 칸 밖의 기본키 칸과 지문으로 정해진다.
func TestDigestPK(t *testing.T) {
	var snap pg.Table
	for _, tb := range pg.Tables() {
		if tb.DigestKey != "" {
			snap = tb
		}
	}
	row := func(project, key, value string) []any {
		vals := make([]any, len(snap.Cols))
		vals[snap.Index("project")], vals[snap.Index("key")], vals[snap.Index("value")] = project, key, value
		return vals
	}
	a, okA := snap.DigestPK(row("p", "k", "1"))
	b, _ := snap.DigestPK(row("p", "k", "2"))
	c, _ := snap.DigestPK(row("q", "k", "1"))
	d, _ := snap.DigestPK(row("p", "k2", "1"))
	if !okA || a != b || a == c || a == d {
		t.Fatalf("지문 기본키: 같은 키 %v · 다른 프로젝트 %v · 다른 키 %v", a == b, a == c, a == d)
	}
	bad := row("p", "k", "1")
	bad[snap.Index("key")] = nil // 사전 검사가 이미 거른 값(기본키 NULL)
	if _, ok := snap.DigestPK(bad); ok {
		t.Fatal("지문 칸이 글이 아닌 행에서 지문 기본키가 나왔다")
	}
	if _, ok := pg.Tables()[0].DigestPK(make([]any, len(pg.Tables()[0].Cols))); ok {
		t.Fatalf("지문 칸이 없는 표 %s 에서 지문 기본키가 나왔다", pg.Tables()[0].Name)
	}
}

// INSERT 는 identity 가 있는 표에만 OVERRIDING SYSTEM VALUE 를 붙이고, 행마다 칸 수만큼 자리표시를 둔다.
func TestInsertSQL(t *testing.T) {
	for _, tb := range pg.Tables() {
		q := tb.InsertSQL(3)
		_, id := tb.Identity()
		if strings.Contains(q, "OVERRIDING SYSTEM VALUE") != id {
			t.Errorf("%s: identity %v 인데 %q", tb.Name, id, q)
		}
		if n := strings.Count(q, "?"); n != 3*len(tb.Cols) {
			t.Errorf("%s: 자리표시 %d, 기대 %d", tb.Name, n, 3*len(tb.Cols))
		}
	}
}
