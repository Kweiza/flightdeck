package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/nulesc"
	"github.com/kweiza/flightdeck/internal/store/pg"
)

// 원장 칸 관문(cellgate.go)을 문장 하나 · 인자 하나 단위로 잰다. 쓰기 길 전체(두 엔진 · HTTP)는 api 의 nul_test.go 가 잰다.

// slotsOf 는 꼴의 자리표시마다 「칸 이름(규칙을 거는 글 칸이면) / 행 좌표」다.
func slotsOf(sh *cellShape) []string {
	var out []string
	for _, sl := range sh.slots {
		col := ""
		if sl.col != nil {
			col = sl.col.Name
		}
		out = append(out, col+"/"+sl.coord)
	}
	return out
}

func TestShapeOfReadsWhereEachArgumentLands(t *testing.T) {
	cases := []struct {
		name, q string
		table   string
		slots   []string // nil 이면 꼴이 없다(관문이 볼 칸이 없다)
	}{
		{"INSERT · 충돌 갱신", `INSERT INTO item(project, id, title, labels, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(project, id) DO UPDATE SET title = excluded.title, body = ?, state = 'open' WHERE item.state = ?`,
			"item", []string{"project/project", "id/id", "title/", "labels/", "created_at/", "body/", "/"}},
		{"INSERT · 여러 행", `INSERT INTO item_after(project, item_id, dep_item, dep_job, dep_sha, dep_project) VALUES (?, ?, ?, NULL, NULL, ?), (?, ?, ?, NULL, NULL, ?)`,
			"item_after", []string{"project/project", "item_id/item_id", "dep_item/dep_item", "dep_project/dep_project", "project/project", "item_id/item_id", "dep_item/dep_item", "dep_project/dep_project"}},
		{"UPDATE · 조회 조건은 좌표", `UPDATE item SET title = ?, close_reason = NULL, closed_at = ? WHERE project = ? AND id = ? AND state IN ('open', 'claimed')`,
			"item", []string{"title/", "closed_at/", "/project", "/id"}},
		{"글 안의 ? 는 자리표시가 아니다", `UPDATE item SET title = '?', body = ? WHERE project = ? AND id = ?`,
			"item", []string{"body/", "/project", "/id"}},
		{"수 칸의 식은 읽는다", `UPDATE counter SET value = value + ? WHERE project = ? AND name = ?`,
			"counter", []string{"/", "/project", "/name"}},
		{"수 칸에는 규칙을 안 건다", `INSERT INTO idempotency(key, fingerprint, status, ctype, body, at) VALUES (?, ?, ?, ?, ?, ?)`,
			"idempotency", []string{"key/key", "fingerprint/", "/", "ctype/", "/", "at/"}},
		{"조회", `SELECT title FROM item WHERE id = ?`, "", nil},
		{"카탈로그 밖 표", `INSERT INTO sqlite_sequence(name, seq) VALUES (?, ?)`, "", nil},
	}
	for _, c := range cases {
		sh, err := shapeOf(c.q)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if c.slots == nil {
			if sh != nil {
				t.Errorf("%s: 꼴 %+v, 바라는 것 없음", c.name, slotsOf(sh))
			}
			continue
		}
		if sh == nil || sh.table != c.table || !reflect.DeepEqual(slotsOf(sh), c.slots) {
			var got []string
			if sh != nil {
				got = slotsOf(sh)
			}
			t.Errorf("%s: 꼴 %v, 바라는 것 %s %v", c.name, got, c.table, c.slots)
		}
	}
}

// 글 칸의 값이 자리표시 하나가 아니라 식이면(관문이 칸에 담길 바이트를 모른다) 인자와 상관없이 늘 오류다.
func TestShapeOfRefusesAnExpressionIntoATextCell(t *testing.T) {
	for _, q := range []string{
		`UPDATE item SET title = COALESCE(?, title) WHERE project = ? AND id = ?`,
		`INSERT INTO item(project, id, title) VALUES (?, ?, ? || '끝')`,
		`INSERT INTO claim(project, item_id, session_id) VALUES (?, ?, ?) ON CONFLICT(project, item_id) DO UPDATE SET force_reason = 'r:' || ?`,
	} {
		if _, err := shapeOf(q); err == nil || !strings.Contains(err.Error(), "식이다") {
			t.Errorf("%s: %v, 바라는 것 「식이다」 오류", q, err)
		}
	}
}

type labelText string

func TestCellGateAppliesTheImportRule(t *testing.T) {
	const upd = `UPDATE item SET title = ?, labels = ? WHERE project = ? AND id = ?`
	g := &cellGate{}
	title := "제목\x00"
	args := []any{&title, `["l\u0000","\\x00"]`, "p", "i1"}
	orig := append([]any(nil), args...)
	out, err := g.args(upd, args)
	if err != nil {
		t.Fatal(err)
	}
	if out[0] != nulesc.Escape(title) || out[1] != `["l\\x00","\\x00"]` || out[2] != "p" || out[3] != "i1" {
		t.Errorf("바꾼 인자 %q", out)
	}
	if !reflect.DeepEqual(args, orig) {
		t.Errorf("부른 쪽의 인자를 고쳤다: %q", args)
	}
	want := []cellRewrite{
		{table: "item", column: "title", rule: pg.RewriteNUL, row: `project="p" id="i1"`},
		{table: "item", column: "labels", rule: pg.RewriteJSONNUL, pointers: []string{"/0"}, row: `project="p" id="i1"`},
	}
	if !reflect.DeepEqual(g.rewrites, want) {
		t.Errorf("바꾼 칸 %+v, 바라는 것 %+v", g.rewrites, want)
	}

	// 이름 붙은 글 타입 · sql.NullString 도 글이다.
	for _, v := range []any{labelText("a\x00"), sql.NullString{String: "a\x00", Valid: true}} {
		out, err := (&cellGate{}).args(`UPDATE item SET body = ? WHERE project = ? AND id = ?`, []any{v, "p", "i1"})
		if err != nil || out[0] != `a\x00` {
			t.Errorf("%T: %q %v", v, out, err)
		}
	}

	// 바꿀 것이 없으면 받은 조각 그대로다(새로 안 만든다). 관문이 없는 트랜잭션도 그대로다.
	clean := []any{"t", `["a"]`, "p", "i1"}
	if out, err := g.args(upd, clean); err != nil || &out[0] != &clean[0] {
		t.Errorf("바꿀 것 없는 인자를 새로 만들었다: %v", err)
	}
	var none *cellGate
	if out, err := none.args(upd, args); err != nil || &out[0] != &args[0] {
		t.Errorf("관문 없는 트랜잭션이 인자를 건드렸다: %v", err)
	}
	// 수 칸에 글 인자가 와도 거절하지 않는다 - 규칙은 글 칸에만 건다.
	if _, err := (&cellGate{}).args(`UPDATE counter SET value = ? WHERE project = ? AND name = ?`, []any{"7", "p", "n"}); err != nil {
		t.Errorf("수 칸의 글 인자: %v", err)
	}
}

func TestCellGateRefusesWhatTheImportRefuses(t *testing.T) {
	cases := []struct {
		name, q string
		args    []any
		col     string
		kind    error
		where   string
	}{
		{"식별자 칸의 NUL", `INSERT INTO item(project, id, title) VALUES (?, ?, ?)`, []any{"p", "i\x00", "t"}, "id", nulesc.ErrNUL, `id="i\x00"`},
		{"글 칸의 깨진 UTF-8", `UPDATE item SET body = ? WHERE project = ? AND id = ?`, []any{"a\xff", "p", "i1"}, "body", nulesc.ErrUTF8, `id="i1"`},
		{"JSON 칸의 짝 없는 서로게이트", `UPDATE item SET labels = ? WHERE project = ? AND id = ?`, []any{`["\ud800"]`, "p", "i1"}, "labels", nulesc.ErrSurrogate, `id="i1" JSON "/0"`},
		{"JSON 키의 U+0000", `INSERT INTO event(at, project, session_id, kind, payload) VALUES (?, ?, ?, ?, ?)`,
			[]any{"x", "p", nil, "k", `{"\u0000":1}`}, "payload", nulesc.ErrKeyNUL, `JSON "/\x00"`},
	}
	for _, c := range cases {
		g := &cellGate{}
		_, err := g.args(c.q, c.args)
		var ref *CellRefusal
		if !errors.As(err, &ref) || ref.Column != c.col || !errors.Is(err, c.kind) || !strings.Contains(ref.Where, c.where) {
			t.Errorf("%s: %v (%+v), 바라는 것 %s 칸의 %v · 자리 %q", c.name, err, ref, c.col, c.kind, c.where)
		}
		if len(g.rewrites) != 0 {
			t.Errorf("%s: 거절했는데 바꾼 칸을 적었다 %+v", c.name, g.rewrites)
		}
	}
}

// 관문이 거절하면 트랜잭션이 망가진다: 부른 쪽이 그 오류를 삼키고 끝까지 가도 앞서 쓴 행까지 아무것도 커밋되지 않는다(Postgres 가
// 22021 로 트랜잭션을 버리는 것과 같은 결말이 SQLite 에서도 난다). 바꿔 넣은 칸은 커밋된 뒤에만 로그로 낸다.
func TestCellRefusalPoisonsTheTransaction(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seed(t, s, "p")
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	ins := `INSERT INTO item(project, id, title, body, created_at) VALUES (?, ?, ?, ?, ?)`
	err := s.Tx(ctx, func(t *Tx) error {
		if _, err := t.tx.ExecContext(t.ctx, ins, "p", "ok", "제목\x00", "b", fmtTime(nowStamp())); err != nil {
			return err
		}
		_, _ = t.tx.ExecContext(t.ctx, ins, "p", "bad\x00", "t", "b", fmtTime(nowStamp())) // 오류를 삼킨다
		return nil
	})
	var ref *CellRefusal
	if !errors.As(err, &ref) || ref.Column != "id" {
		t.Fatalf("삼킨 거절이 커밋됐다: %v", err)
	}
	if _, gerr := s.GetItem(ctx, "p", "ok"); !errors.Is(gerr, ErrNotFound) {
		t.Errorf("망가진 트랜잭션의 앞 행이 남았다: %v", gerr)
	}
	if strings.Contains(logged.String(), "원장 칸의 NUL 을") {
		t.Errorf("롤백된 쓰기를 바꿔 넣었다고 적었다")
	}
	if err := s.Tx(ctx, func(t *Tx) error {
		_, err := t.tx.ExecContext(t.ctx, ins, "p", "ok", "제목\x00", "b", fmtTime(nowStamp()))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "원장 칸의 NUL 을") || !strings.Contains(logged.String(), `project=\"p\" id=\"ok\"`) ||
		strings.Contains(logged.String(), "제목") {
		t.Errorf("커밋된 바꿔 넣기의 로그 줄(행 좌표 · 내용 없음):\n%s", logged.String())
	}
}

// --judgments 의 원장 되쓰기(WriteLedger)와 그 예행(ScreenLedger)도 같은 관문을 지난다: 자유 글의 NUL 은 이관과 같은 값으로
// 담고, 식별자 칸의 NUL 은 아무것도 안 쓰고 거절한다. 예행이 거절하는 덤프는 적용도 거절한다.
func TestWriteLedgerTakesTheCellGate(t *testing.T) {
	ctx := context.Background()
	at := fmtTime(nowStamp())
	dump := func(id string) LedgerDump {
		p, title := "p", "판단\x00"
		return LedgerDump{
			Projects:  []LedgerProject{{ID: p, Path: "/r/p", DefaultBranch: "main", CreatedAt: at}},
			Judgments: []LedgerJudgment{{ID: id, Project: &p, At: at, Kind: string(model.JudgmentDecision), Title: &title, Body: "본문\x00"}},
		}
	}
	if n, err := ScreenLedger(dump("j1")); err != nil || n != 2 {
		t.Fatalf("예행: 바꿀 칸 %d · %v, 바라는 것 2 · nil", n, err)
	}
	s := newStore(t)
	if err := s.WriteLedger(ctx, dump("j1")); err != nil {
		t.Fatalf("되쓰기: %v", err)
	}
	var title, body string
	if err := s.DB().QueryRow(`SELECT title, body FROM judgment WHERE id = 'j1'`).Scan(&title, &body); err != nil {
		t.Fatal(err)
	}
	if title != `판단\x00` || body != `본문\x00` {
		t.Errorf("되쓴 판단 %q · %q, 바라는 것 이관의 값", title, body)
	}

	_, serr := ScreenLedger(dump("j\x00"))
	werr := newStore(t).WriteLedger(ctx, dump("j\x00"))
	for what, err := range map[string]error{"예행": serr, "되쓰기": werr} {
		var ref *CellRefusal
		if !errors.As(err, &ref) || ref.Table != "judgment" || ref.Column != "id" {
			t.Errorf("%s: 식별자 칸의 NUL 을 %v 로 냈다, 바라는 것 judgment.id 거절", what, err)
		}
	}
}

// store 의 쓰기 문장은 관문이 꼴을 읽을 수 있어야 한다 - 못 읽는 문장은 처음 도는 자리에서 늘 오류다(TestShapeOfRefuses…). 시험이
// 안 닿는 문장도 여기서 잰다: 이 패키지의 시험 아닌 파일에서 INSERT · UPDATE 로 시작하는 글 상수 전부.
func TestStoreWriteStatementsHaveReadableShapes(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fs := token.NewFileSet()
	seen := 0
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fs, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(nd ast.Node) bool {
			bl, ok := nd.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			q, err := strconv.Unquote(bl.Value)
			if err != nil {
				return true
			}
			head := strings.ToUpper(strings.TrimSpace(q))
			if !strings.HasPrefix(head, "INSERT ") && !strings.HasPrefix(head, "UPDATE ") {
				return true
			}
			// 상수 조각(문장 앞머리만 있고 나머지를 이어 붙이는 것) · 서식 틀(`UPDATE %s SET …`)은 이은 문장이 도는 시험이 잰다.
			if !strings.Contains(head, "?") || strings.Contains(q, "%s") {
				return true
			}
			seen++
			if _, err := shapeOf(q); err != nil {
				t.Errorf("%s: %v", fs.Position(bl.Pos()), err)
			}
			return true
		})
	}
	if seen < 40 {
		t.Errorf("잰 쓰기 문장이 %d 개다 - 훑기가 문장을 놓친다", seen)
	}
}
