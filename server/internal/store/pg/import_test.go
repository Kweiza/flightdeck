package pg_test

// 이관 카탈로그(pg.Tables)와 대조 규칙(verify.go)의 순수한 몫을 잰다. 두 원장을 실제로 옮기는 왕복 · 거절 시험은 store 의
// importsqlite_test.go 다.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/pg"
)

// 손으로 적은 이관 카탈로그가 두 엔진의 살아 있는 카탈로그와 같다: 표 목록(옮기지 않는 것을 뺀), 칸 이름 · 순서 · 타입 ·
// NOT NULL · 행 좌표 칸(SQLite 기본키) · identity · AUTOINCREMENT, 그리고 FK 순서(부모가 먼저). 옮기지 않는 표의 이름
// 목록(LeftBehindNames)도 이관 판 원본에 실제로 있는 표다.
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
	liteAll := map[string]bool{}
	for _, r := range strs(t, lite, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`) {
		liteAll[r[0]] = true
		if !pg.LeftBehind(r[0]) {
			liteTables = append(liteTables, r[0])
		}
	}
	// 이름 그대로 적은 목록에 낡은 이름이 없다 - 스키마가 FTS 선택지를 바꿔 그림자 표가 달라지면 여기서 빨갛다(남은 표는
	// 위 목록에 섞여 표 수가 어긋난다).
	for _, n := range pg.LeftBehindNames() {
		if !liteAll[n] {
			t.Errorf("옮기지 않는 표 %s 가 판 %d SQLite 원본에 없다 - 목록이 낡았다", n, pg.ImportVersion)
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

	known := map[string]pg.Kind{"INTEGER": pg.Int, "TEXT": pg.Text, "BLOB": pg.Bytes, "bigint": pg.Int, "text": pg.Text, "bytea": pg.Bytes}
	// 모르는 타입은 어느 Kind 와도 같지 않게 한다(맵의 0 값이 pg.Text 라 모르는 타입이 글 칸으로 지나가면 안 된다).
	kindOf := func(table, col, typ string) pg.Kind {
		k, ok := known[typ]
		if !ok {
			t.Errorf("%s.%s 의 타입 %q 를 이 시험이 모른다 - 이관 카탈로그의 Kind 로 옮길 수 없다", table, col, typ)
			return pg.Kind(-1)
		}
		return k
	}
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
			if c[0] != w.Name || kindOf(tb.Name, c[0], strings.ToUpper(c[1])) != w.Kind || (c[2] == "1" || inPK) != w.NotNull || inPK != w.Key {
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
			if c[0] != w.Name || kindOf(tb.Name, c[0], c[1]) != w.Kind || (c[2] == "NO") != w.NotNull || (c[3] == "ALWAYS") != w.Identity {
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
	capped := pg.Column{Name: "p", Kind: pg.Text, MaxBytes: 4}
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
		// float64 로 못 옮기는 수 뒤에 있어도 \u0000 을 잰다(수를 글 그대로 읽는다).
		{"범위 밖 수 뒤의 \\u0000", doc, `[1e400,"\u0000"]`, `[1e400,"\u0000"]`, pg.FaultJSONNUL},
		{"범위 밖 수는 받는다", doc, `{"n":1e400,"s":"\\u0000"}`, `{"n":1e400,"s":"\\u0000"}`, ""},
		{"JSON 은 받는다", doc, `{"a":[1,"b"]}`, `{"a":[1,"b"]}`, ""},
		// 상한은 Cell 이 재지 않는다 - 대조 · 역이관은 원문 그대로 옮기고 맞댄다(ImportCell 만 잰다).
		{"Cell 은 상한을 재지 않는다", capped, "abcde", "abcde", ""},
	} {
		got, fault := pg.Cell(c.col, c.in)
		if fault != c.fault || pg.RowHash([]any{got}) != pg.RowHash([]any{c.want}) {
			t.Errorf("%s: Cell(%v) = (%#v, %q), 기대 (%#v, %q)", c.name, c.in, got, fault, c.want, c.fault)
		}
	}
}

// 인덱스 키 상한(Column.MaxBytes)은 Postgres 로 옮겨 넣을 값의 규칙(ImportCell)에만 있다. 대조 · 역이관이 지나는 Cell · TargetCell 은
// 같은 값을 받는다. ImportCell 은 Cell 의 다른 검사를 그대로 한다.
func TestImportCellAloneCapsTheIndexKey(t *testing.T) {
	capped := pg.Column{Name: "p", Kind: pg.Text, MaxBytes: 4}
	for _, c := range []struct {
		name  string
		in    any
		fault pg.Fault
	}{
		{"상한까지는 받는다", "abcd", ""},
		{"상한을 넘는 글", "abcde", pg.FaultTooLong},
		{"상한은 글자가 아니라 바이트다", "가나", pg.FaultTooLong},
		{"상한 칸의 BLOB 도 바이트로 잰다", []byte("abcde"), pg.FaultTooLong},
		{"다른 검사가 먼저다", "a\x00bcde", pg.FaultNUL},
		{"NULL 은 받는다", nil, ""},
	} {
		got, fault := pg.ImportCell(capped, c.in)
		want, _ := pg.Cell(capped, c.in)
		if fault != c.fault || pg.RowHash([]any{got}) != pg.RowHash([]any{want}) {
			t.Errorf("%s: ImportCell(%v) = (%#v, %q), 기대 (%#v, %q)", c.name, c.in, got, fault, want, c.fault)
		}
		if c.fault == pg.FaultTooLong {
			if _, f := pg.Cell(capped, c.in); f != "" {
				t.Errorf("%s: Cell 이 상한을 쟀다(%q)", c.name, f)
			}
			if _, f := pg.TargetCell(capped, c.in); f != "" && c.name != "상한 칸의 BLOB 도 바이트로 잰다" {
				t.Errorf("%s: TargetCell 이 상한을 쟀다(%q)", c.name, f)
			}
		}
	}
	// 상한이 없는 칸은 Cell 과 같다.
	if _, f := pg.ImportCell(pg.Column{Name: "t", Kind: pg.Text}, strings.Repeat("a", 10000)); f != "" {
		t.Fatalf("상한이 없는 칸을 쟀다: %q", f)
	}
}

// 행 지문은 NULL 과 빈 글을 가르고, 칸 경계가 밀린 행을 가르고, 바이트 하나의 차이를 가르고, 바이트가 같은 다른 저장
// 타입(정수 7 · 글 '7' · BLOB X'37' · 실수 7)을 가른다.
func TestRowHashSeparates(t *testing.T) {
	for _, p := range [][2][]any{
		{{nil}, {""}},
		{{"ab", "c"}, {"a", "bc"}},
		{{nil, "a"}, {"a", nil}},
		{{"2026-01-01T00:00:00.000000Z"}, {"2026-01-01T00:00:00.000001Z"}},
		{{[]byte{}}, {nil}},
		{{int64(10)}, {int64(1)}},
		{{"7"}, {int64(7)}},
		{{[]byte("7")}, {int64(7)}},
		{{"7"}, {[]byte("7")}},
		{{7.0}, {int64(7)}},
		{{""}, {[]byte{}}},
	} {
		if pg.RowHash(p[0]) == pg.RowHash(p[1]) {
			t.Errorf("%#v 와 %#v 의 지문이 같다", p[0], p[1])
		}
	}
	if got, fault := pg.Cell(pg.Column{Name: "b", Kind: pg.Bytes, NotNull: true}, []byte(nil)); fault != "" || got.([]byte) == nil {
		t.Errorf("길이 0 BLOB(nil 조각)이 빈 bytea 가 아니다: (%#v, %q) - pgx 는 nil 조각을 NULL 로 보낸다", got, fault)
	}
	// 바이트가 그대로인 두 갈래는 Cell 이 칸 타입으로 옮긴 뒤 대상에서 읽은 값과 지문이 같다.
	for _, c := range []struct {
		col    pg.Column
		src    any
		target any
	}{
		{pg.Column{Name: "t", Kind: pg.Text}, []byte("x"), "x"},
		{pg.Column{Name: "b", Kind: pg.Bytes}, "x", []byte("x")},
	} {
		got, fault := pg.Cell(c.col, c.src)
		if fault != "" || pg.RowHash([]any{got}) != pg.RowHash([]any{c.target}) {
			t.Errorf("%s 칸의 %#v: Cell 뒤 (%#v, %q) 가 대상 값 %#v 와 지문이 다르다", c.col.Kind, c.src, got, fault, c.target)
		}
	}
}

// 사전 검사를 지난 값(Fault 가 빈)은 Cell 뒤 NULL 이거나 칸 타입의 꼴이고, 저장 타입이 칸과 달라 걸린 값은 원래 타입을
// 그대로 둔다. 행 지문의 표지가 이것에 기댄다 - 원본은 Cell 뒤의 타입, 대상은 칸 타입이라 지난 행은 표지가 같고 걸린 행은
// 다르다(RowHash 의 ★).
func TestCellKeepsTheColumnType(t *testing.T) {
	goType := map[pg.Kind]string{pg.Text: "string", pg.Int: "int64", pg.Bytes: "[]uint8"}
	stored := []any{nil, int64(7), 7.5, "7", []byte("7"), []byte(nil), "", "[]", "\xff"}
	typeName := func(v any) string {
		switch v.(type) {
		case nil:
			return "nil"
		case int64:
			return "int64"
		case float64:
			return "float64"
		case string:
			return "string"
		case []byte:
			return "[]uint8"
		}
		return "?"
	}
	for _, kind := range []pg.Kind{pg.Text, pg.Int, pg.Bytes} {
		for _, json := range []bool{false, true} {
			if json && kind != pg.Text {
				continue
			}
			col := pg.Column{Name: "c", Kind: kind, JSON: json}
			for _, v := range stored {
				got, fault := pg.Cell(col, v)
				switch {
				case fault == "" && got != nil && typeName(got) != goType[kind]:
					t.Errorf("%s 칸(JSON %v)이 %#v 를 받아 %s 로 냈다 - 칸 타입(%s)이 아니다", kind, json, v, typeName(got), goType[kind])
				case fault == pg.FaultStorage && typeName(got) != typeName(v):
					t.Errorf("%s 칸이 걸린 %#v 를 %s 로 바꿨다 - 원래 저장 타입을 둬야 한다", kind, v, typeName(got))
				case fault == "" && v == nil && got != nil:
					t.Errorf("%s 칸이 NULL 을 %#v 로 냈다", kind, got)
				}
			}
		}
	}
}

// TargetCell 은 Cell 이 받는 두 갈래(글 칸의 BLOB, bytea 칸의 TEXT)를 저장 갈래 오류로 걸고 값을 받은 그대로 둔다. 칸
// 타입 그대로 저장된 값과 NULL 은 Cell 과 같이 잰다.
func TestTargetCellRefusesTheOtherStorage(t *testing.T) {
	text, bytes := pg.Column{Name: "c", Kind: pg.Text}, pg.Column{Name: "c", Kind: pg.Bytes}
	for _, c := range []struct {
		col  pg.Column
		v    any
		want pg.Fault
	}{
		{text, []byte("x"), pg.FaultStorage},
		{bytes, "x", pg.FaultStorage},
		{text, "x", ""},
		{bytes, []byte("x"), ""},
		{pg.Column{Name: "c", Kind: pg.Int}, int64(1), ""},
		{text, nil, ""},
	} {
		got, fault := pg.TargetCell(c.col, c.v)
		if fault != c.want {
			t.Errorf("%s 칸의 %#v: 사유 %q, 기대 %q", c.col.Kind, c.v, fault, c.want)
		}
		if fault == pg.FaultStorage && fmt.Sprintf("%T", got) != fmt.Sprintf("%T", c.v) {
			t.Errorf("%s 칸이 걸린 %#v 를 %T 로 바꿨다", c.col.Kind, c.v, got)
		}
		if want, wantFault := pg.Cell(c.col, c.v); c.want == "" && (wantFault != "" || fmt.Sprint(got) != fmt.Sprint(want)) {
			t.Errorf("%s 칸의 %#v: Cell 과 다르다(%#v · %#v)", c.col.Kind, c.v, got, want)
		}
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
// RESTART WITH n 뒤의 시퀀스는 last_value = n · is_called = 거짓이다.
func TestSequence(t *testing.T) {
	for _, c := range []struct {
		floor, next int64
	}{{0, 1}, {1, 2}, {41, 42}} {
		restart := pg.Sequence(c.floor)
		if got := pg.NextValue(restart, false); got != c.next {
			t.Errorf("floor %d: RESTART WITH %d 뒤 다음 번호 %d, 기대 %d", c.floor, restart, got, c.next)
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

// INSERT 는 Postgres 쪽에서 identity 가 있는 표에만 OVERRIDING SYSTEM VALUE 를 붙이고(SQLite 쪽은 어느 표에도 붙이지
// 않는다), 행마다 칸 수만큼 자리표시를 둔다. 칸 이름은 같은 엔진의 SelectSQL 과 같다 - SQLite 쪽 item_after 는 seq 자리가
// rowid 다(역이관이 seq 를 rowid 로 되돌리는 축).
func TestInsertSQL(t *testing.T) {
	for _, tb := range pg.Tables() {
		for _, sqlite := range []bool{false, true} {
			q := tb.InsertSQL(3, sqlite)
			_, id := tb.Identity()
			if strings.Contains(q, "OVERRIDING SYSTEM VALUE") != (id && !sqlite) {
				t.Errorf("%s(sqlite %v): identity %v 인데 %q", tb.Name, sqlite, id, q)
			}
			if n := strings.Count(q, "?"); n != 3*len(tb.Cols) {
				t.Errorf("%s(sqlite %v): 자리표시 %d, 기대 %d", tb.Name, sqlite, n, 3*len(tb.Cols))
			}
			cols := strings.TrimPrefix(tb.SelectSQL(sqlite), "SELECT ")
			cols = strings.TrimSuffix(cols, " FROM "+tb.Name)
			if !strings.HasPrefix(q, "INSERT INTO "+tb.Name+" ("+cols+")") {
				t.Errorf("%s(sqlite %v): 넣는 칸이 읽는 칸(%s)과 다르다: %q", tb.Name, sqlite, cols, q)
			}
		}
	}
	after := pg.Tables()[slices.IndexFunc(pg.Tables(), func(tb pg.Table) bool { return tb.Name == "item_after" })]
	if q := after.InsertSQL(1, true); !strings.Contains(q, "(rowid, ") && !strings.Contains(q, ", rowid") {
		t.Errorf("SQLite 쪽 item_after INSERT 에 rowid 가 없다: %q", q)
	}
	if q := after.InsertSQL(1, false); strings.Contains(q, "rowid") {
		t.Errorf("Postgres 쪽 item_after INSERT 에 rowid 가 있다: %q", q)
	}
}

// InsertSQL 을 부르는(가리키는) 자리는 원장 옮기기(store 의 copyTables - 이관 · 역이관이 함께 쓴다) 하나다. 그 함수의
// Postgres 쪽 INSERT 는 identity 칸에 옛 번호를 넣는 구절(OVERRIDING SYSTEM VALUE)을 붙이는데, 그 구절이 이 패키지 안에
// 있어서 서버 코드의 OVERRIDING 을 훑는 시험(TestServerCodeNeverOverridesIdentity)이 앱 코드의 호출을 못 본다. 그래서
// 부르는 자리를 소스에서 잠근다.
// 함수 값으로 넘기는 자리(`f := t.InsertSQL`)와 패키지 수준 초기화도 센다.
func TestInsertSQLIsCalledOnlyByTheImport(t *testing.T) {
	root, err := filepath.Abs("../../..") // server 모듈 뿌리
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var got []string
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		for _, decl := range f.Decls {
			where := "(패키지 수준)"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				where = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "InsertSQL" {
					got = append(got, filepath.ToSlash(rel)+" "+where)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("훑은 Go 파일이 %d 개다 - 뿌리(%s)를 잘못 잡았다", scanned, root)
	}
	slices.Sort(got)
	if want := []string{"internal/store/importsqlite.go copyTables"}; !slices.Equal(got, want) {
		t.Errorf("InsertSQL(OVERRIDING SYSTEM VALUE 를 붙인 INSERT)을 부르는 자리가 다르다\n 얻음 %v\n 기대 %v", got, want)
	}
}

// 옮기지 않는 표는 이름이 정확히 맞아야 한다 - judgment_fts 로 시작하는 다른 표는 옮긴다(모르는 표면 이관이 거절한다).
func TestLeftBehindIsExact(t *testing.T) {
	for name, want := range map[string]bool{
		"schema_version": true, "sqlite_sequence": true, "sqlite_stat1": true,
		"judgment_fts": true, "judgment_fts_data": true, "judgment_fts_idx": true, "judgment_fts_docsize": true,
		"judgment_fts_config":  true,
		"judgment_fts_content": false, "judgment_fts2": false, "judgment_ftsx": false, "judgment": false, "schema_versions": false,
	} {
		if got := pg.LeftBehind(name); got != want {
			t.Errorf("LeftBehind(%q) = %v, 기대 %v", name, got, want)
		}
	}
}

// 「인덱스만 더한 증분」 판정(IndexOnly)은 `CREATE INDEX <이름> ON …` 문장만 받는다: 주석 줄 · 빈 줄은 건너뛰고, 이름을 적힌 순서로
// 낸다. 아래 판의 원장이 받던 행을 거절하는 것(UNIQUE), 트랜잭션 안에서 못 도는 것(CONCURRENTLY), 내림이 남의 인덱스를 지우게 되는 것
// (IF NOT EXISTS), 모르는 꼴(따옴표 이름 · 줄 끝 주석 · 다른 문장)은 「인덱스만」이 아니다.
func TestIndexOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		sql  string
		want []string
	}{
		{"하나", "-- 머리말\n\nCREATE INDEX a_by_x ON a(x);\n", []string{"a_by_x"}},
		{"둘", "CREATE INDEX a_by_x ON a(x);\ncreate index b_by_y on b (y, id);", []string{"a_by_x", "b_by_y"}},
		{"여러 줄", "CREATE INDEX a_by_x\n  ON a(x, y);", []string{"a_by_x"}},
		{"빈 증분", "-- 주석만\n", nil},
		{"UNIQUE", "CREATE UNIQUE INDEX a_by_x ON a(x);", nil},
		{"CONCURRENTLY", "CREATE INDEX CONCURRENTLY a_by_x ON a(x);", nil},
		{"IF NOT EXISTS", "CREATE INDEX IF NOT EXISTS a_by_x ON a(x);", nil},
		{"따옴표 이름", `CREATE INDEX "A" ON a(x);`, nil},
		{"줄 끝 주석", "CREATE INDEX a_by_x ON a(x); -- 끝", nil},
		{"다른 문장이 섞였다", "CREATE INDEX a_by_x ON a(x);\nALTER TABLE a ADD COLUMN z text;", nil},
		{"표 만들기", "CREATE TABLE a(x text);", nil},
		{"이름 없는 인덱스", "CREATE INDEX ON a(x);", nil},
	} {
		got, ok := pg.IndexOnly(c.sql)
		if wantOK := c.want != nil; ok != wantOK || !slices.Equal(got, c.want) {
			t.Errorf("%s: IndexOnly = %v, %v(기대 %v, %v)", c.name, got, ok, c.want, wantOK)
		}
	}
}

// 이관 판 범위는 ImportFloor 하나에서 온다: 이관 판에서 아래로 인덱스만 더한 증분을 거슬러 닿는 판이고 기준선 아래로는 안 간다. 지금
// 증분은 019(인덱스 하나)뿐이라 범위는 기준선(18)부터 19 다.
func TestImportFloorFollowsTheIndexOnlySteps(t *testing.T) {
	if got := pg.ImportFloor(); got != pg.BaselineVersion || pg.BaselineVersion != 18 || pg.ImportVersion != 19 {
		t.Fatalf("ImportFloor = %d(기준선 %d, 이관 판 %d) - 판 18-19 이어야 한다", got, pg.BaselineVersion, pg.ImportVersion)
	}
	for v, want := range map[int]bool{17: false, 18: true, 19: true, 20: false} {
		if got := pg.Transferable(v); got != want {
			t.Errorf("Transferable(%d) = %v(기대 %v)", v, got, want)
		}
	}
	for _, s := range pg.Steps {
		if _, ok := pg.IndexOnly(s.SQL); !ok {
			t.Errorf("증분 %d(%s)가 인덱스만 더한 것이 아니다 - 범위가 그 아래로 내려가지 않는다", s.To, s.Name)
		}
	}

	// 규칙 자체는 지금 목록과 떼어 잰다: 꼭대기에서 아래로 인덱스만 더한 증분만 거스르고, 표를 바꾼 증분 · 빈칸 · 기준선에서 선다.
	idx := func(to int) pg.Step { return pg.Step{To: to, SQL: "CREATE INDEX x" + fmt.Sprint(to) + " ON a(x);"} }
	tbl := func(to int) pg.Step {
		return pg.Step{To: to, SQL: "ALTER TABLE a ADD COLUMN y" + fmt.Sprint(to) + " text;"}
	}
	for _, c := range []struct {
		name      string
		steps     []pg.Step
		top, want int
	}{
		{"인덱스 하나", []pg.Step{idx(19)}, 19, 18},
		{"표를 바꾼 꼭대기", []pg.Step{tbl(19)}, 19, 19},
		{"인덱스 둘", []pg.Step{idx(19), idx(20)}, 20, 18},
		{"인덱스 위의 표", []pg.Step{idx(19), tbl(20)}, 20, 20},
		{"표 위의 인덱스", []pg.Step{tbl(19), idx(20)}, 20, 19},
		{"꼭대기가 목록보다 낮다", []pg.Step{idx(19), tbl(20)}, 19, 18},
		{"빈칸", []pg.Step{idx(19), idx(21)}, 21, 20},
		{"증분 없음", nil, 18, 18},
	} {
		if got := pg.FloorOf(c.steps, c.top, 18); got != c.want {
			t.Errorf("%s: 바닥 %d(기대 %d)", c.name, got, c.want)
		}
	}
}
