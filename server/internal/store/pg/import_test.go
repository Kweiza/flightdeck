package pg_test

// 이관 카탈로그(pg.Tables)와 대조 규칙(verify.go)의 순수한 몫을 잰다. 두 원장을 실제로 옮기는 왕복 · 거절 시험은 store 의
// importsqlite_test.go 다.

import (
	"crypto/sha256"
	"encoding/hex"
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

// Cell 은 Postgres 가 거절할 값을 사유와 함께 거르고, 바이트를 바꾸지 않는 저장 갈래는 받고, 자유 글의 NUL 은 정해진 규칙으로
// 바꿔 넣으며 그 갈래를 낸다. 식별자 자리(식별자 칸 · JSON 키)의 NUL 은 바꾸지 않고 거절한다.
func TestCellFaults(t *testing.T) {
	text := pg.Column{Name: "t", Kind: pg.Text}
	notNull := pg.Column{Name: "n", Kind: pg.Text, NotNull: true}
	key := pg.Column{Name: "k", Kind: pg.Text, NotNull: true, Key: true, Identifier: true}
	ident := pg.Column{Name: "r", Kind: pg.Text, Identifier: true}
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
		rw    pg.Rewrite
	}{
		{"NULL 은 받는다", text, nil, nil, "", ""},
		{"빈 글은 NULL 이 아니다", notNull, "", "", "", ""},
		{"NOT NULL 칸의 NULL", notNull, nil, nil, pg.FaultNull, ""},
		{"기본키의 NULL", key, nil, nil, pg.FaultKeyNull, ""},
		{"글의 NUL 은 \\x00 이 된다", text, "a\x00b", `a\x00b`, "", pg.RewriteNUL},
		{"기본키의 NUL 은 거절한다", key, "k\x00", "k\x00", pg.FaultIdentNUL, ""},
		{"참조 칸의 NUL 도 거절한다", ident, "p\x00", "p\x00", pg.FaultIdentNUL, ""},
		{"식별자 칸의 BLOB 에 든 NUL 도 거절한다", ident, []byte{'p', 0}, "p\x00", pg.FaultIdentNUL, ""},
		{"식별자 칸의 \\x00 표기는 그대로다", ident, `p\x00`, `p\x00`, "", ""},
		{"NUL 이 든 칸의 \\x00 표기는 백슬래시가 두 배다", text, `\x00` + "\x00", `\\x00\x00`, "", pg.RewriteNUL},
		{"NUL 이 없는 칸의 \\x00 표기는 그대로다", text, `a\x00\\`, `a\x00\\`, "", ""},
		{"깨진 UTF-8", text, "\xff", "\xff", pg.FaultUTF8, ""},
		{"깨진 UTF-8 은 NUL 보다 먼저 걸린다", text, "\xff\x00", "\xff\x00", pg.FaultUTF8, ""},
		{"글 칸의 BLOB 은 같은 바이트의 글이다", text, []byte("한글"), "한글", "", ""},
		{"글 칸의 BLOB 의 NUL 도 바꿔 넣는다", text, []byte{'a', 0}, `a\x00`, "", pg.RewriteNUL},
		{"글 칸의 정수", text, int64(1), int64(1), pg.FaultStorage, ""},
		{"정수 칸의 글", num, "abc", "abc", pg.FaultStorage, ""},
		{"정수 칸의 실수", num, 1.5, 1.5, pg.FaultStorage, ""},
		{"정수는 받는다", num, int64(-7), int64(-7), "", ""},
		{"bytea 칸의 TEXT 는 같은 바이트다", blob, "x", []byte("x"), "", ""},
		{"bytea 는 NUL 을 받는다", blob, []byte{0, 1}, []byte{0, 1}, "", ""},
		{"깨진 JSON", doc, "[", "[", pg.FaultJSON, ""},
		{"날 NUL 이 든 JSON 은 문서가 아니다", doc, "[\"\x00\"]", "[\"\x00\"]", pg.FaultJSON, ""},
		{"JSON 글 안의 \\u0000", doc, `{"a":"x\u0000"}`, `{"a":"x\\x00"}`, "", pg.RewriteJSONNUL},
		{"JSON 키 안의 \\u0000 은 거절한다", doc, `{"\u0000":1}`, `{"\u0000":1}`, pg.FaultJSONKeyNUL, ""},
		// 키를 바꿔 넣으면 옆 키와 같아진다.
		{"바꾼 키가 옆 키와 같아질 문서", doc, `{"\u0000":1,"\\x00":2}`, `{"\u0000":1,"\\x00":2}`, pg.FaultJSONKeyNUL, ""},
		{"글 값의 \\u0000 뒤에 있어도 키는 거절이다", doc, `{"a":"\u0000","b\u0000":1}`, `{"a":"\u0000","b\u0000":1}`, pg.FaultJSONKeyNUL, ""},
		{"이스케이프된 백슬래시 뒤 u0000 은 글자다", doc, `["\\u0000"]`, `["\\u0000"]`, "", ""},
		// float64 로 못 옮기는 수 뒤에 있어도 \u0000 을 잰다(수를 글 그대로 읽는다).
		{"범위 밖 수 뒤의 \\u0000", doc, `[1e400,"\u0000"]`, `[1e400,"\\x00"]`, "", pg.RewriteJSONNUL},
		{"범위 밖 수는 받는다", doc, `{"n":1e400,"s":"\\u0000"}`, `{"n":1e400,"s":"\\u0000"}`, "", ""},
		// 문서를 디코드해 다시 쓰지 않는다: 다른 글 · 고친 글 안의 다른 이스케이프(다시 쓰면 é · / 가 된다) · 공백 · 수는 원래 바이트다.
		{"고친 글 밖은 원래 바이트다", doc, `{ "h":"\u00e9\/", "n": "\/\u00e9\u0000", "f": 1.50 }`, `{ "h":"\u00e9\/", "n": "\/\u00e9\\x00", "f": 1.50 }`, "", pg.RewriteJSONNUL},
		// 바꾸는 단위가 글 하나다: U+0000 이 없는 옆 글의 \x00 표기는 U+0000 이 없는 문서에서처럼 그대로다.
		{"옆 글의 \\x00 표기는 그대로다", doc, `["\u0000","\\x00"]`, `["\\x00","\\x00"]`, "", pg.RewriteJSONNUL},
		{"같은 글의 \\x00 표기는 백슬래시가 두 배다", doc, `["\\x00\u0000"]`, `["\\\\x00\\x00"]`, "", pg.RewriteJSONNUL},
		{"짝 없는 서로게이트", doc, `["\ud800"]`, `["\ud800"]`, pg.FaultJSONSurrogate, ""},
		// 디코드하면 둘 다 「U+FFFD + U+0000」이라 다른 두 키가 하나가 된다.
		{"U+FFFD 로 같아질 두 키", doc, `{"\ud800\u0000":1,"\ufffd\u0000":2}`, `{"\ud800\u0000":1,"\ufffd\u0000":2}`, pg.FaultJSONSurrogate, ""},
		{"U+0000 없는 문서의 \\x00 표기는 그대로다", doc, `["\\x00"]`, `["\\x00"]`, "", ""},
		{"JSON 은 받는다", doc, `{"a":[1,"b"]}`, `{"a":[1,"b"]}`, "", ""},
		// 상한은 Cell 이 재지 않는다 - 대조 · 역이관은 원문 그대로 옮기고 맞댄다(ImportCell 만 잰다).
		{"Cell 은 상한을 재지 않는다", capped, "abcde", "abcde", "", ""},
	} {
		got, fault, rw := pg.Cell(c.col, c.in)
		if fault != c.fault || rw != c.rw || pg.RowHash([]any{got}) != pg.RowHash([]any{c.want}) {
			t.Errorf("%s: Cell(%q) = (%#v, %q, %q), 기대 (%#v, %q, %q)", c.name, c.in, got, fault, rw, c.want, c.fault, c.rw)
		}
	}
}

// 인덱스 키 상한(Column.MaxBytes)은 Postgres 로 옮겨 넣을 값의 규칙(ImportCell)에만 있다. 대조 · 역이관이 지나는 Cell · SettledCell
// 은 같은 값을 받는다. ImportCell 은 Cell 의 다른 검사 · 바꿔 넣기를 그대로 하고, 상한은 바꿔 넣은 값(옮겨 넣을 바이트)으로 잰다.
func TestImportCellAloneCapsTheIndexKey(t *testing.T) {
	capped := pg.Column{Name: "p", Kind: pg.Text, MaxBytes: 4}
	for _, c := range []struct {
		name  string
		in    any
		fault pg.Fault
		rw    pg.Rewrite
	}{
		{"상한까지는 받는다", "abcd", "", ""},
		{"상한을 넘는 글", "abcde", pg.FaultTooLong, ""},
		{"상한은 글자가 아니라 바이트다", "가나", pg.FaultTooLong, ""},
		{"상한 칸의 BLOB 도 바이트로 잰다", []byte("abcde"), pg.FaultTooLong, ""},
		{"다른 검사가 먼저다", "\xffbcde", pg.FaultUTF8, ""},
		{"바꿔 넣은 값이 상한 안이면 받는다", "\x00", "", pg.RewriteNUL},
		{"바꿔 넣은 값으로 잰다", "a\x00", pg.FaultTooLong, ""},
		{"NULL 은 받는다", nil, "", ""},
	} {
		got, fault, rw := pg.ImportCell(capped, c.in)
		want, _, _ := pg.Cell(capped, c.in)
		if fault != c.fault || rw != c.rw || pg.RowHash([]any{got}) != pg.RowHash([]any{want}) {
			t.Errorf("%s: ImportCell(%q) = (%#v, %q, %q), 기대 (%#v, %q, %q)", c.name, c.in, got, fault, rw, want, c.fault, c.rw)
		}
	}
	for _, in := range []string{"abcde", "가나"} {
		if _, f, _ := pg.Cell(capped, in); f != "" {
			t.Errorf("Cell(%q) 이 상한을 쟀다(%q)", in, f)
		}
		if _, f, _ := pg.SettledCell(capped, in); f != "" {
			t.Errorf("SettledCell(%q) 이 상한을 쟀다(%q)", in, f)
		}
	}
	// 상한이 없는 칸은 Cell 과 같다.
	if _, f, _ := pg.ImportCell(pg.Column{Name: "t", Kind: pg.Text}, strings.Repeat("a", 10000)); f != "" {
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
	if got, fault, _ := pg.Cell(pg.Column{Name: "b", Kind: pg.Bytes, NotNull: true}, []byte(nil)); fault != "" || got.([]byte) == nil {
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
		got, fault, _ := pg.Cell(c.col, c.src)
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
				got, fault, _ := pg.Cell(col, v)
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

// SettledCell 은 값을 바꾸지 않는다(변환 없이 맞댄다): Cell 이 받는 두 갈래(글 칸의 BLOB, bytea 칸의 TEXT)는 저장 갈래 오류,
// 글 칸의 NUL 은 식별자 칸이든 아니든 FaultNUL, 깨진 JSON 은 FaultJSON 으로 건다. 이관이 jsonb 때문에 거는 JSON 규칙(글 · 키의
// \u0000, 짝 없는 서로게이트)은 걸지 않는다 - 두 엔진이 다 글 칸에 담고 SQLite 의 JSON 함수도 읽는 원문이다.
func TestSettledCellComparesVerbatim(t *testing.T) {
	text, bytes := pg.Column{Name: "c", Kind: pg.Text}, pg.Column{Name: "c", Kind: pg.Bytes}
	doc := pg.Column{Name: "c", Kind: pg.Text, JSON: true}
	for _, c := range []struct {
		col  pg.Column
		v    any
		want pg.Fault
	}{
		{text, []byte("x"), pg.FaultStorage},
		{bytes, "x", pg.FaultStorage},
		{text, "a\x00b", pg.FaultNUL},
		{text, []byte("a\x00b"), pg.FaultNUL},
		{doc, `["\u0000"]`, ""},
		{doc, `["\ud800"]`, ""},
		{doc, `{"\u0000":1}`, ""},
		{doc, "[", pg.FaultJSON},
		{pg.Column{Name: "c", Kind: pg.Text, Identifier: true}, "a\x00", pg.FaultNUL},
		{doc, "[\"\x00\"]", pg.FaultJSON},
		{text, `a\x00b`, ""},
		{doc, `["\\x00"]`, ""},
		{text, "x", ""},
		{bytes, []byte("x"), ""},
		{bytes, []byte{0}, ""},
		{pg.Column{Name: "c", Kind: pg.Int}, int64(1), ""},
		{text, nil, ""},
	} {
		got, fault, rw := pg.SettledCell(c.col, c.v)
		if fault != c.want || rw != "" {
			t.Errorf("%s 칸의 %q: (%q, %q), 기대 (%q, \"\")", c.col.Kind, c.v, fault, rw, c.want)
		}
		if pg.RowHash([]any{got}) != pg.RowHash([]any{c.v}) {
			t.Errorf("%s 칸이 %q 를 %#v 로 바꿨다", c.col.Kind, c.v, got)
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

// 칸 묶음의 지문은 그 묶음의 옮길 값으로 정해진다: 지문 칸(snapshot.key)은 KeyDigest 로, 부분 인덱스는 조건 칸이 NULL 인 행만,
// NULL 이 든 행은 묶음 밖이다. 정수 칸만으로 된 묶음은 사전 검사가 재지 않는다(Mergeable).
func TestUniquePrint(t *testing.T) {
	table := func(name string) pg.Table {
		for _, tb := range pg.Tables() {
			if tb.Name == name {
				return tb
			}
		}
		t.Fatalf("표 %s 가 없다", name)
		return pg.Table{}
	}
	row := func(tb pg.Table, kv ...any) []any {
		vals := make([]any, len(tb.Cols))
		for i := 0; i < len(kv); i += 2 {
			vals[tb.Index(kv[i].(string))] = kv[i+1]
		}
		return vals
	}
	snap := table("snapshot")
	pk := snap.Uniques()[0]
	if pk.Name != "snapshot_pkey" || !slices.Equal(pk.Cols, []string{"project", "key"}) {
		t.Fatalf("snapshot 기본키 %+v", pk)
	}
	fp := func(tb pg.Table, u pg.Unique, vals []any) string {
		h, ok := tb.UniquePrint(u, vals)
		if !ok {
			return "밖"
		}
		return fmt.Sprintf("%x", h[:4])
	}
	a := fp(snap, pk, row(snap, "project", "p", "key", "k", "value", "1"))
	if a != fp(snap, pk, row(snap, "project", "p", "key", "k", "value", "2")) ||
		a == fp(snap, pk, row(snap, "project", "q", "key", "k", "value", "1")) ||
		a == fp(snap, pk, row(snap, "project", "p", "key", "k2", "value", "1")) {
		t.Fatal("snapshot 기본키 지문이 기본키 칸만으로 정해지지 않는다")
	}
	d := pg.KeyDigest("k")
	if a == fp(snap, pk, row(snap, "project", "p", "key", d[:])) {
		t.Fatal("지문 칸을 지문으로 재지 않았다")
	}
	if fp(snap, pk, row(snap, "project", "p")) != "밖" || fp(snap, pk, row(snap, "project", "p", "key", []byte("k"))) != "밖" {
		t.Fatal("지문 칸이 글이 아닌 행이 묶음에 들었다")
	}
	hold := table("resource_hold")
	one := hold.Uniques()[1]
	if one.Name != "resource_one_holder" || one.WhereNull != "released_at" {
		t.Fatalf("resource_hold 의 묶음 %+v", hold.Uniques())
	}
	live := row(hold, "project", "p", "resource", "env:db")
	if fp(hold, one, live) == "밖" || fp(hold, one, row(hold, "project", "p", "resource", "env:db", "released_at", "t")) != "밖" {
		t.Fatal("부분 인덱스의 조건 칸을 재지 않았다")
	}
	if hold.Mergeable(hold.Uniques()[0]) || !hold.Mergeable(one) {
		t.Fatal("정수 칸만으로 된 기본키와 글 칸 묶음을 못 갈랐다")
	}
}

// 손으로 적은 기본키 · UNIQUE 칸 묶음(Table.Uniques)이 기준선의 유일 인덱스 전부와 같다: 이름 · 칸(지문 칸은 key_digest) · 부분
// 인덱스 조건. 사전 검사의 겹침 재기(FaultUnique)가 기준선이 거절할 겹침을 빠짐없이 본다.
func TestUniquesMatchTheBaseline(t *testing.T) {
	db := openPG(t)
	n := 0
	for _, tb := range pg.Tables() {
		var want, got []string
		for _, u := range tb.Uniques() {
			cols := slices.Clone(u.Cols)
			for i, c := range cols {
				if c == tb.DigestKey {
					cols[i] = c + "_digest"
				}
			}
			pred := ""
			if u.WhereNull != "" {
				pred = "(" + u.WhereNull + " IS NULL)"
			}
			want = append(want, u.Name+" ("+strings.Join(cols, ",")+") "+pred)
		}
		for _, r := range strs(t, db, `SELECT c.relname, coalesce(pg_get_expr(i.indpred, i.indrelid), ''),
				(SELECT string_agg(a.attname, ',' ORDER BY k.ord) FROM unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
				 JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum)
			FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			WHERE i.indisunique AND i.indrelid = format('%I', $1::text)::regclass`, tb.Name) {
			got = append(got, r[0]+" ("+r[2]+") "+r[1])
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			t.Errorf("%s 의 유일 묶음: 카탈로그 %q, 기준선 %q", tb.Name, want, got)
		}
		n += len(got)
	}
	// 표마다 기본키 하나 + 기준선의 UNIQUE 제약 하나 · 부분 · UNIQUE 인덱스 셋 + 증분 020 의 UNIQUE 제약 셋 · UNIQUE 인덱스 하나
	if n != len(pg.Tables())+8 {
		t.Fatalf("기준선의 유일 인덱스 %d 개, 기대 %d 개", n, len(pg.Tables())+8)
	}
}

// 식별자 칸(Column.Identifier) 표시가 기준선과 맞는다: Postgres 의 유일 인덱스 · FK 에 드는 글 칸은 다 식별자 칸이고(지문 칸은
// key_digest 대신 key), FK 없이 다른 행을 이름으로 가리키는 칸(손 목록)만 더 있다. 이름이 식별자 꼴(id · project · *_id ·
// *_project · dep_*)인 글 칸도 다 식별자 칸이다. 그래서 바꿔 넣은 NUL 이 기본키 · UNIQUE 겹침(FaultUnique)이나 다른 행을 가리키는
// 값을 만들 길이 없다.
func TestIdentifierColumnsMatchTheBaseline(t *testing.T) {
	db := openPG(t)
	// FK 가 없는 참조 칸: 다른 행(항목 · 작업 · 커밋 · 세션 · 프로젝트)을 이름으로 가리킨다.
	byName := []string{"item_after.dep_item", "item_after.dep_job", "item_after.dep_sha", "item_after.dep_project",
		"pick_eval.project", "pick_eval.session_id", "pick_eval.picked", "judgment_link.target_project",
		"resource_hold.job_id", "job.item_id", "event.project", "event.session_id",
		// 증분 020: 기기 코드 흐름에서 클라이언트가 보낸 옛 machine-id. 없는 기기일 수 있어 FK 가 없다.
		"device_code.machine_hint"}
	inBaseline := map[string]bool{}
	for _, tb := range pg.Tables() {
		for _, r := range strs(t, db, `SELECT a.attname FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid
				AND a.attnum = ANY(i.indkey::int2[]) WHERE i.indisunique AND i.indrelid = format('%I', $1::text)::regclass
			UNION SELECT a.attname FROM pg_constraint c JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
				WHERE c.contype = 'f' AND c.conrelid = format('%I', $1::text)::regclass`, tb.Name) {
			name := r[0]
			if tb.DigestKey != "" && name == tb.DigestKey+"_digest" {
				name = tb.DigestKey
			}
			inBaseline[tb.Name+"."+name] = true
		}
	}
	identName := func(n string) bool {
		return n == "id" || n == "project" || strings.HasSuffix(n, "_id") || strings.HasSuffix(n, "_project") ||
			strings.HasPrefix(n, "dep_")
	}
	var extra []string
	for _, tb := range pg.Tables() {
		for _, u := range tb.Uniques() {
			for _, n := range u.Cols {
				if !tb.Cols[tb.Index(n)].Identifier {
					t.Errorf("%s 의 유일 묶음 %s 의 칸 %s 가 식별자 칸이 아니다", tb.Name, u.Name, n)
				}
			}
		}
		for _, c := range tb.Cols {
			at := tb.Name + "." + c.Name
			if c.Kind != pg.Text {
				continue
			}
			switch {
			case inBaseline[at] && !c.Identifier:
				t.Errorf("%s 는 기준선의 유일 인덱스 · FK 칸인데 식별자 칸이 아니다", at)
			case identName(c.Name) && !c.Identifier:
				t.Errorf("%s 는 식별자 이름인데 식별자 칸이 아니다", at)
			case c.Identifier && !inBaseline[at]:
				extra = append(extra, at)
			}
		}
	}
	if len(inBaseline) == 0 {
		t.Fatal("기준선에서 유일 인덱스 · FK 칸을 하나도 못 읽었다")
	}
	slices.Sort(extra)
	slices.Sort(byName)
	if !slices.Equal(extra, byName) {
		t.Errorf("기준선 밖의 식별자 칸 %q, 기대(FK 없는 참조 칸) %q", extra, byName)
	}
}

// 칸 하나를 통째로 거절하는 갈래는 행 좌표 그대로고, JSON 글 하나를 거절한 갈래(짝 없는 서로게이트 · 키의 U+0000)는 그 글의
// 자리를 덧붙인다 - 큰 문서에서 고칠 글을 찾게.
func TestFaultLabelPointsAtTheRefusedString(t *testing.T) {
	text := pg.Column{Name: "c", Kind: pg.Text}
	doc := pg.Column{Name: "c", Kind: pg.Text, JSON: true}
	for _, c := range []struct {
		col  pg.Column
		v    any
		f    pg.Fault
		want string
	}{
		{doc, `{"a":{"b\u0000":1}}`, pg.FaultJSONKeyNUL, `id=1 JSON "/a/b\x00"`},
		{doc, []byte(`[0,{"x":"\udc00"}]`), pg.FaultJSONSurrogate, `id=1 JSON "/1/x"`},
		{doc, `{"a/b":{"~\u0000":1}}`, pg.FaultJSONKeyNUL, `id=1 JSON "/a~1b/~0\x00"`},
		{doc, "[", pg.FaultJSON, `id=1`},
		{text, "a\x00", pg.FaultIdentNUL, `id=1`},
		{text, "\xff", pg.FaultUTF8, `id=1`},
	} {
		if got := pg.FaultLabel("id=1", c.col, c.v, c.f); got != c.want {
			t.Errorf("FaultLabel(%q, %s) = %q, 기대 %q", c.v, c.f, got, c.want)
		}
	}
}

// jsonb 는 이관이 바꿔 넣는 JSON(\u0000)과 거절하는 JSON(짝 없는 서로게이트)을 둘 다 거절하고, 바꿔 넣은 문서 · 짝 맞은
// 서로게이트는 받는다. JSON 칸은 Postgres 에서 text 지만 jsonb 로 읽는 질의가 있어, 그 하나가 남의 행에 든 것이라도 질의가 통째로
// 죽는다 - 그래서 Cell 이 jsonb 가 거절할 문서를 그대로 옮기지 않는다.
func TestJSONBRefusesWhatImportRewritesOrRefuses(t *testing.T) {
	db := openPG(t)
	doc := pg.Column{Name: "j", Kind: pg.Text, JSON: true}
	for in, jsonb := range map[string]bool{
		`["\u0000"]`: false, `{"\u0000":1}`: false, `["\ud800"]`: false, `["\udc00"]`: false,
		`["\\x00"]`: true, `["\ud83d\ude00"]`: true, `["\\u0000"]`: true,
	} {
		var out string
		err := db.QueryRow(`SELECT ($1::text)::jsonb::text`, in).Scan(&out)
		if (err == nil) != jsonb {
			t.Errorf("jsonb(%s): 받음 %v(%v), 기대 %v", in, err == nil, err, jsonb)
		}
		got, fault, rw := pg.Cell(doc, in)
		if jsonb != (fault == "" && rw == "") {
			t.Errorf("Cell(%s) = (%s, %q, %q) - jsonb 가 거절할 문서를 그대로 옮기거나 받는 문서를 건드렸다", in, got, fault, rw)
		}
		if rw != "" {
			if err := db.QueryRow(`SELECT ($1::text)::jsonb::text`, got).Scan(&out); err != nil {
				t.Errorf("바꿔 넣은 %s 를 jsonb 가 거절한다: %v", got, err)
			}
		}
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
// 꼭대기 증분 020 이 표 · 칸을 더했으므로(계정 표 넷 · 행위자 칸) 범위는 판 20 하나다 - 그 아래 019 가 인덱스만 더한 것이어도
// 020 에서 선다.
func TestImportFloorFollowsTheIndexOnlySteps(t *testing.T) {
	if got := pg.ImportFloor(); got != 20 || pg.BaselineVersion != 18 || pg.ImportVersion != 20 {
		t.Fatalf("ImportFloor = %d(기준선 %d, 이관 판 %d) - 판 20 하나여야 한다", got, pg.BaselineVersion, pg.ImportVersion)
	}
	for v, want := range map[int]bool{18: false, 19: false, 20: true, 21: false} {
		if got := pg.Transferable(v); got != want {
			t.Errorf("Transferable(%d) = %v(기대 %v)", v, got, want)
		}
	}
	for _, s := range pg.Steps {
		_, ok := pg.IndexOnly(s.SQL)
		if want := s.To == 19; ok != want {
			t.Errorf("증분 %d(%s)의 「인덱스만」 판정이 %v 다(기대 %v)", s.To, s.Name, ok, want)
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

// 비밀 칸 표시(Column.Secret)는 이름이 `_hash` 로 끝나는 칸 전부이고 그것뿐이다 - 암호 지문 · 토큰 · 쿠키 · 기기 코드의 sha256.
// 새 해시 칸이 표시 없이 들어오면 그 원값이 행 좌표 · 오류 · 바꾼 칸 기록으로 나간다.
func TestSecretColumnsAreTheHashColumns(t *testing.T) {
	var marked, hashes []string
	for _, tb := range pg.Tables() {
		for _, c := range tb.Cols {
			if c.Secret {
				marked = append(marked, tb.Name+"."+c.Name)
			}
			if strings.HasSuffix(c.Name, "_hash") {
				hashes = append(hashes, tb.Name+"."+c.Name)
			}
		}
	}
	want := []string{"account.password_hash", "invite.token_hash", "machine.token_hash", "web_session.id_hash", "device_code.code_hash"}
	if !slices.Equal(marked, hashes) || len(marked) != len(want) {
		t.Fatalf("비밀 칸 %v, `_hash` 칸 %v (기대 %v)", marked, hashes, want)
	}
	for _, w := range want {
		if !slices.Contains(marked, w) {
			t.Errorf("%s 가 비밀 칸이 아니다", w)
		}
	}
}

// 행 좌표는 비밀 키 칸을 원값 대신 지문으로 적고, 그 밖의 칸은 원값 그대로다. 지문은 같은 값이면 같고, 원값의 어떤 조각도 싣지
// 않는다. 대조의 행 지문(Mark 의 Hash)은 원값으로 잰다 - 키가 같고 내용이 다른 두 행은 지문이 다르다.
func TestRowLabelsFingerprintSecretKeys(t *testing.T) {
	var ws pg.Table
	for _, tb := range pg.Tables() {
		if tb.Name == "web_session" {
			ws = tb
		}
	}
	const raw = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	row := []any{raw, "a1", "t", "t", "t", nil}
	m := ws.Mark(row)
	if want := "id_hash=" + pg.SecretMark(raw); m.Key != want || strings.Contains(m.Key, raw[:12]) {
		t.Fatalf("web_session 행 좌표 %q, 기대 %q", m.Key, want)
	}
	if got := pg.SecretMark(raw); !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+12 || got != pg.SecretMark([]byte(raw)) {
		t.Fatalf("지문 꼴 %q", got)
	}
	sum := sha256.Sum256([]byte(raw))
	if got := pg.SecretDigest(raw); got != "sha256:"+hex.EncodeToString(sum[:]) || got != pg.SecretDigest([]byte(raw)) ||
		!strings.HasPrefix(got, pg.SecretMark(raw)) {
		t.Fatalf("전체 지문 꼴 %q - 「sha256:」 뒤 sha256 전부여야 하고 12자 지문이 그 앞부분이어야 한다", got)
	}
	other := slices.Clone(row)
	other[5] = "ua"
	if o := ws.Mark(other); o.Key != m.Key || o.Hash == m.Hash {
		t.Fatalf("키가 같고 내용이 다른 행: 좌표 %q · %q, 행 지문이 같다 %v", m.Key, o.Key, o.Hash == m.Hash)
	}
	if got := pg.CoordLabel(nil, raw); got != pg.LabelValue(raw) || !strings.Contains(got, raw) {
		t.Fatalf("칸을 모르는 좌표는 원값 꼴이어야 한다: %q", got)
	}
	if got := pg.CoordLabel(&ws.Cols[1], "a1"); got != `"a1"` {
		t.Fatalf("비밀 아닌 칸의 좌표 %q", got)
	}
}

// 비밀 칸의 NUL 은 바꿔 넣지 않고 거절한다 - 바꾸면 바꾼 칸 기록이 그 원래 바이트를 담는다. 비밀이 아닌 자유 글(user_agent)은
// 바꿔 넣는다.
func TestSecretColumnsRefuseNUL(t *testing.T) {
	for _, tb := range pg.Tables() {
		for _, c := range tb.Cols {
			_, fault, rw := pg.Cell(c, "a\x00b")
			switch {
			case c.Secret && (fault != pg.FaultSecretNUL || rw != ""):
				t.Errorf("비밀 칸 %s.%s 의 NUL: 거절 %q · 바꿈 %q", tb.Name, c.Name, fault, rw)
			case tb.Name == "web_session" && c.Name == "user_agent" && rw != pg.RewriteNUL:
				t.Errorf("web_session.user_agent 의 NUL 을 바꿔 넣지 않았다: 거절 %q · 바꿈 %q", fault, rw)
			}
		}
	}
}
