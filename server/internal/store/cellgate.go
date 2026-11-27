package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/kweiza/flightdeck/internal/nulesc"
	"github.com/kweiza/flightdeck/internal/store/pg"
)

// 원장 칸 관문 - 서버가 원장에 쓰는 글 하나하나에 이관과 같은 칸 규칙(pg.Cell)을 거는 단 한 자리다.
//
// ## 왜 여기인가
//
// Postgres text 는 NUL 을 못 담는다(22021). 이관은 SQLite 원본의 칸 하나를 단위로 자유 글의 NUL 을 `\x00` 표기로 바꾸고 식별자
// 칸의 NUL 은 거절한다(pg.Cell). 서버의 쓰기가 같은 원장에 같은 바이트를 담으려면 그 규칙이 같은 단위 - **원장에 담기는 글
// 하나** - 에 걸려야 한다. 요청을 푸는 자리(입력 칸 하나)에 걸면 여러 입력을 합쳐 한 칸에 쓰는 자리(회수 판단 본문 = 행위자 +
// 사유)는 조각마다 바꾼 뒤 합친 값이 되고(이관은 합친 칸 전체에 한 번 건다 - 두 값이 갈린다), 입력을 자르거나 재는 자리(이벤트의
// clip · 길이)는 바꾼 글을 잰다. 요청을 안 푸는 쓰기(fd import --apply · --judgments)는 아예 그 자리를 안 지난다.
//
// 그래서 store 의 업무 쓰기 트랜잭션(runTx - Tx · ObsTx)이 여는 txConn 이 INSERT · UPDATE 문장마다 그 꼴(shapeOf)을 읽어 칸 값
// 자리의 글 인자에 그 칸의 규칙을 건다: 이관 카탈로그(pg.Tables)에서 칸을 찾아 pg.Cell 을 그대로 부른다 - 이관과 쓰기가 같은
// 함수 · 같은 카탈로그를 지나므로 바이트가 같다는 것이 구성으로 선다(두 벌이면 한쪽만 고쳐지는 표류가 난다).
//
//   - 자유 글 칸의 NUL → `\x00` 표기(RewriteNUL), JSON 칸의 `\u0000` 글 토큰 → 그 토큰만 고친다(RewriteJSONNUL). 바꾼 칸은 커밋
//     뒤에 Info 로그 한 줄씩(표 · 칸 · 갈래 · JSON 자리 · 행 좌표, 글 내용은 안 싣는다) - 이관의 기록 파일 자리다.
//   - 식별자 칸의 NUL · 깨진 UTF-8 · JSON 키의 U+0000 · 짝 없는 서로게이트 → *CellRefusal. 문장을 보내지 않고, 트랜잭션을
//     망가진 것으로 표시해(poison) 부른 쪽이 오류를 삼켜도 아무것도 커밋되지 않는다(Postgres 가 22021 로 트랜잭션을 버리는 것과
//     같은 결말이 두 엔진에서 난다).
//
// ★ 유지 보수 길(sessionTx 의 증분 · SQLite 이관, 역이관 · 덤프, 되읽기)은 관문을 안 지난다. 그 길의 값은 이미 원장에 있는
// 원문이거나 이관이 따로 잰다 - 거기에 바꿔 넣기를 걸면 되돌리기 사본이 원장과 갈린다(pg.SettledCell 의 ★).
// ★ 원장에서 읽어 다시 적는 값(개정 이력의 옛 제목 · 본문)도 관문을 지나지만 바뀌지 않는다: 원장의 글에는 NUL 이 없고(관문 ·
// 이관이 다 바꿨다) 규칙은 두 번 걸어도 같다(nulesc.Text).

// CellRefusal 은 원장 칸 관문이 칸 규칙으로 거절한 글 하나다. Fault 가 입력의 잘못이면(Unwrap 이 nulesc 의 거절 갈래) 400 으로
// 가고, 서버가 만든 값의 결함이면(깨진 JSON) 500 이다.
type CellRefusal struct {
	Table, Column string
	Fault         pg.Fault
	// Where 는 행 좌표다(표의 식별자 칸 · 조회 조건의 값, JSON 글 하나를 거절했으면 그 글의 자리를 덧붙인다).
	Where string
}

func (e *CellRefusal) Error() string {
	return fmt.Sprintf("원장 칸 %s.%s 가 받을 수 없는 글이다(%s · %s)", e.Table, e.Column, e.Fault, e.Where)
}

// Unwrap 은 거절 갈래를 입력 규칙(nulesc)의 갈래로 옮긴다. 서버의 결함(깨진 JSON)이면 nil 이다.
func (e *CellRefusal) Unwrap() error {
	switch e.Fault {
	case pg.FaultIdentNUL:
		return nulesc.ErrNUL
	case pg.FaultUTF8:
		return nulesc.ErrUTF8
	case pg.FaultJSONSurrogate:
		return nulesc.ErrSurrogate
	case pg.FaultJSONKeyNUL:
		return nulesc.ErrKeyNUL
	}
	return nil
}

// cellGate 는 업무 쓰기 트랜잭션 하나의 관문 상태다(runTx 가 txConn 에 단다). 바꾼 칸을 모아 두었다가 커밋 뒤에 로그로 낸다 -
// 롤백된 쓰기를 「바꿔 넣었다」고 적지 않는다.
type cellGate struct {
	rewrites []cellRewrite
}

// cellRewrite 는 관문이 바꿔 넣은 칸 하나다.
type cellRewrite struct {
	table, column string
	rule          pg.Rewrite
	pointers      []string // JSON 칸이면 고친 글의 자리
	row           string
}

// args 는 문장 q 의 인자 args 에 칸 규칙을 건 인자다. 바꿀 것이 없으면 args 그대로다(새로 만들지 않는다). g 가 nil 이면(관문이
// 없는 트랜잭션) 그대로 낸다.
func (g *cellGate) args(q string, args []any) ([]any, error) {
	if g == nil {
		return args, nil
	}
	sh, err := shapeOf(q)
	if err != nil || sh == nil {
		return args, err
	}
	var out []any
	for i, sl := range sh.slots {
		if sl.col == nil || i >= len(args) {
			continue
		}
		s, ok := textArg(args[i])
		if !ok {
			continue
		}
		v, fault, rw := pg.Cell(*sl.col, s)
		switch {
		case fault != "":
			return nil, &CellRefusal{Table: sh.table, Column: sl.col.Name, Fault: fault,
				Where: pg.FaultLabel(sh.row(args), *sl.col, s, fault)}
		case rw != "":
			if out == nil {
				out = slices.Clone(args)
			}
			out[i] = v
			g.rewrites = append(g.rewrites, cellRewrite{table: sh.table, column: sl.col.Name, rule: rw,
				pointers: pg.RewritePaths(*sl.col, s), row: sh.row(args)})
		}
	}
	if out == nil {
		return args, nil
	}
	return out, nil
}

// report 는 커밋된 트랜잭션이 바꿔 넣은 칸을 한 줄씩 남긴다.
func (g *cellGate) report(ctx context.Context, log *slog.Logger) {
	if g == nil {
		return
	}
	for _, r := range g.rewrites {
		log.InfoContext(ctx, "원장 칸의 NUL 을 \\x00 표기로 바꿔 넣었다",
			"table", r.table, "column", r.column, "rule", string(r.rule), "row", r.row, "json_pointers", r.pointers)
	}
}

// textArg 는 인자 v 가 글이면 그 글이다: string(과 그것을 바탕으로 둔 이름 붙은 타입), 그 포인터(nil 이 아닌), sql.NullString
// (Valid). 그 밖(수 · 바이트 · NULL)은 글이 아니다.
func textArg(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case *string:
		if x != nil {
			return *x, true
		}
		return "", false
	case sql.NullString:
		return x.String, x.Valid
	case nil, []byte, int, int64, bool:
		return "", false
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.String {
		return rv.String(), true
	}
	return "", false
}

// ─────────────────────────────────────────────────────────────────────────────
// 문장 꼴
// ─────────────────────────────────────────────────────────────────────────────

// cellShape 는 쓰기 문장 하나의 꼴이다: 자리표시 차례마다 그 인자가 그대로 담기는 원장 칸(없으면 nil)과, 행 좌표로 로그에 실을
// 이름이다.
type cellShape struct {
	table string
	slots []cellSlot
}

type cellSlot struct {
	col   *pg.Column // 인자가 그대로 담기는 글 칸(`칸 = ?` · VALUES 의 `?` 하나). 조회 조건 · 글 칸이 아님 · 카탈로그 밖이면 nil
	coord string     // 행 좌표 이름(식별자 칸 · 조회 조건 `이름 = ?`). 빈 것은 싣지 않는다
}

// row 는 인자 args 로 이 문장이 쓰는 행의 좌표를 적는다(「project="fd" id="x"」 - 이관의 행 좌표 pg.KeyLabel 과 같은 꼴). 글 내용이
// 아니라 이름이다 - 식별자 칸과 조회 조건만 싣는다.
func (sh *cellShape) row(args []any) string {
	var parts []string
	for i, sl := range sh.slots {
		if sl.coord == "" || i >= len(args) {
			continue
		}
		v := args[i]
		if s, ok := textArg(v); ok {
			v = s
		}
		parts = append(parts, sl.coord+"="+pg.LabelValue(v))
	}
	return strings.Join(parts, " ")
}

// shapes 는 읽은 문장 꼴의 기억이다(문장 글 → *shapeMemo). store 의 쓰기 문장은 상수이거나 상수 조각을 이은 것이라 가짓수가
// 정해져 있다 - INSERT · UPDATE 만 담는다(IN 목록처럼 길이가 갈리는 조회는 안 담는다).
var shapes sync.Map

type shapeMemo struct {
	sh  *cellShape
	err error
}

// shapeOf 는 문장 q 의 꼴이다. INSERT · UPDATE 가 아니거나 표가 이관 카탈로그 밖이면 nil 이다(관문이 볼 칸이 없다). 칸 값을
// 식으로 쓴 글 칸(`title = COALESCE(?, title)` 처럼 인자가 칸 값 전체가 아닌 자리)은 오류다 - 관문이 칸에 담길 바이트를 모른다.
// 그런 문장은 처음 도는 자리에서 늘 시끄럽게 걸린다(인자와 상관없이 같은 결과).
func shapeOf(q string) (*cellShape, error) {
	if m, ok := shapes.Load(q); ok {
		memo := m.(*shapeMemo)
		return memo.sh, memo.err
	}
	toks := slices.Collect(lexSQL(q))
	if len(toks) == 0 || toks[0].kind != tokWord {
		return nil, nil
	}
	var sh *cellShape
	var err error
	switch strings.ToUpper(q[toks[0].at:toks[0].end]) {
	case "INSERT":
		sh, err = parseInsert(q, toks)
	case "UPDATE":
		sh, err = parseUpdate(q, toks)
	default:
		return nil, nil
	}
	if err != nil {
		err = fmt.Errorf("원장 칸 관문이 쓰기 문장의 꼴을 못 읽는다(%w): %s", err, clip(q, 200))
	}
	shapes.Store(q, &shapeMemo{sh: sh, err: err})
	return sh, err
}

// stmt 는 꼴을 읽는 동안의 자리다.
type stmt struct {
	q     string
	toks  []sqlTok
	i     int
	marks []int // 낱말 차례 → 자리표시 차례(자리표시가 아니면 -1)
	tb    pg.Table
	sh    *cellShape
}

func newStmt(q string, toks []sqlTok, tb pg.Table) *stmt {
	st := &stmt{q: q, toks: toks, marks: make([]int, len(toks)), tb: tb, sh: &cellShape{table: tb.Name}}
	n := 0
	for k, t := range toks {
		st.marks[k] = -1
		if t.kind == tokMark {
			st.marks[k] = n
			n++
		}
	}
	st.sh.slots = make([]cellSlot, n)
	return st
}

func (st *stmt) text(k int) string { return st.q[st.toks[k].at:st.toks[k].end] }

// is 는 k 번째 낱말이 글자 w(키워드면 대소문자 무관)인가다.
func (st *stmt) is(k int, w string) bool {
	return k < len(st.toks) && strings.EqualFold(st.text(k), w)
}

// expect 는 지금 자리부터 낱말 ws 가 차례로 오는지 보고 그 뒤로 간다.
func (st *stmt) expect(ws ...string) error {
	for _, w := range ws {
		if !st.is(st.i, w) {
			return fmt.Errorf("%q 자리에 %q 가 와야 한다", st.at(), w)
		}
		st.i++
	}
	return nil
}

func (st *stmt) at() string {
	if st.i >= len(st.toks) {
		return "<끝>"
	}
	return st.text(st.i)
}

// word 는 지금 자리의 이름 하나를 읽는다.
func (st *stmt) word() (string, error) {
	if st.i >= len(st.toks) || st.toks[st.i].kind != tokWord {
		return "", fmt.Errorf("%q 자리에 이름이 와야 한다", st.at())
	}
	st.i++
	return st.text(st.i - 1), nil
}

// expr 는 지금 자리부터 바깥 깊이의 쉼표 · 닫는 괄호 · 멈춤 낱말(stops) 앞까지를 식 하나로 읽고, 그 낱말 범위를 낸다.
func (st *stmt) expr(stops ...string) (from, to int) {
	from, depth := st.i, 0
	for ; st.i < len(st.toks); st.i++ {
		t := st.text(st.i)
		switch {
		case t == "(":
			depth++
		case t == ")" && depth == 0, t == "," && depth == 0:
			return from, st.i
		case t == ")":
			depth--
		case depth == 0 && st.toks[st.i].kind == tokWord && slices.ContainsFunc(stops, func(s string) bool { return strings.EqualFold(t, s) }):
			return from, st.i
		}
	}
	return from, st.i
}

// value 는 칸 col 에 담기는 식(from..to)을 잰다: `?` 하나면 그 자리표시가 칸 값이다. 식 안에 자리표시가 있는 글 칸이면 오류다.
// 규칙을 거는 칸은 글 칸뿐이다(수 · 바이트 칸은 NUL 이 문제가 아니고, 이관도 그 칸에는 글 규칙을 안 건다) - 그 밖의 칸은 행
// 좌표만 단다.
func (st *stmt) value(col string, from, to int) error {
	c := st.tb.Col(col)
	if c == nil {
		return nil // 카탈로그 밖 칸(이관이 옮기지 않는다) - 볼 규칙이 없다
	}
	if to-from == 1 && st.marks[from] >= 0 {
		sl := cellSlot{coord: coordOf(c)}
		if c.Kind == pg.Text {
			sl.col = c
		}
		st.sh.slots[st.marks[from]] = sl
		return nil
	}
	for k := from; k < to; k++ {
		if st.marks[k] >= 0 && c.Kind == pg.Text {
			return fmt.Errorf("글 칸 %s 의 값이 자리표시 하나가 아니라 식이다", col)
		}
	}
	return nil
}

// coordOf 는 칸 c 가 행 좌표면(식별자 칸) 그 이름이다.
func coordOf(c *pg.Column) string {
	if c.Identifier {
		return c.Name
	}
	return ""
}

// assignments 는 `칸 = 식, …` 을 멈춤 낱말 앞까지 읽는다(UPDATE … SET, ON CONFLICT … DO UPDATE SET).
func (st *stmt) assignments(stops ...string) error {
	for {
		col, err := st.word()
		if err != nil {
			return err
		}
		if err := st.expect("="); err != nil {
			return err
		}
		from, to := st.expr(stops...)
		if err := st.value(col, from, to); err != nil {
			return err
		}
		if !st.is(st.i, ",") {
			return nil
		}
		st.i++
	}
}

// conditions 는 지금 자리부터 끝까지 남은 자리표시 가운데 `이름 = ?` 꼴의 것에 행 좌표 이름을 단다(조회 조건).
func (st *stmt) conditions() {
	for k := st.i; k < len(st.toks); k++ {
		if st.marks[k] >= 0 && k >= 2 && st.text(k-1) == "=" && st.toks[k-2].kind == tokWord {
			st.sh.slots[st.marks[k]].coord = st.text(k - 2)
		}
	}
}

// parseInsert 는 `INSERT INTO 표(칸, …) VALUES (식, …)[, (…)] [ON CONFLICT … [DO UPDATE SET 칸 = 식, …]] [RETURNING …]` 을 읽는다.
func parseInsert(q string, toks []sqlTok) (*cellShape, error) {
	if len(toks) < 3 || !strings.EqualFold(q[toks[1].at:toks[1].end], "INTO") || toks[2].kind != tokWord {
		return nil, errors.New("INSERT INTO 표 꼴이 아니다")
	}
	tb, ok := pg.TableOf(q[toks[2].at:toks[2].end])
	if !ok {
		return nil, nil
	}
	st := newStmt(q, toks, tb)
	st.i = 3
	if err := st.expect("("); err != nil {
		return nil, err
	}
	var cols []string
	for {
		c, err := st.word()
		if err != nil {
			return nil, err
		}
		cols = append(cols, c)
		if !st.is(st.i, ",") {
			break
		}
		st.i++
	}
	if err := st.expect(")", "VALUES"); err != nil {
		return nil, err
	}
	for {
		if err := st.expect("("); err != nil {
			return nil, err
		}
		for k, c := range cols {
			if k > 0 {
				if err := st.expect(","); err != nil {
					return nil, err
				}
			}
			from, to := st.expr()
			if err := st.value(c, from, to); err != nil {
				return nil, err
			}
		}
		if err := st.expect(")"); err != nil {
			return nil, err
		}
		if !st.is(st.i, ",") {
			break
		}
		st.i++
	}
	for st.i < len(st.toks) && !(st.is(st.i, "DO") && st.is(st.i+1, "UPDATE") && st.is(st.i+2, "SET")) {
		st.i++
	}
	if st.i < len(st.toks) {
		st.i += 3
		if err := st.assignments("WHERE", "RETURNING"); err != nil {
			return nil, err
		}
	}
	return st.sh, nil
}

// parseUpdate 는 `UPDATE 표 SET 칸 = 식, … [WHERE …] [RETURNING …]` 을 읽는다.
func parseUpdate(q string, toks []sqlTok) (*cellShape, error) {
	if len(toks) < 2 || toks[1].kind != tokWord {
		return nil, errors.New("UPDATE 표 꼴이 아니다")
	}
	tb, ok := pg.TableOf(q[toks[1].at:toks[1].end])
	if !ok {
		return nil, nil
	}
	st := newStmt(q, toks, tb)
	st.i = 2
	if err := st.expect("SET"); err != nil {
		return nil, err
	}
	if err := st.assignments("WHERE", "FROM", "RETURNING"); err != nil {
		return nil, err
	}
	st.conditions()
	return st.sh, nil
}
