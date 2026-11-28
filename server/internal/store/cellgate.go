package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
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
// 그래서 store 의 업무 쓰기 트랜잭션(runTx - Tx · ObsTx)이 여는 txConn 이 문장마다 그 꼴(shapeOf)을 읽어 인자 하나하나에 그것이
// 닿는 자리의 규칙을 건다. 칸 값 자리(`칸 = ?` · VALUES 의 `?` 하나)의 글 인자에는 그 칸의 규칙이다: 이관 카탈로그(pg.Tables)에서
// 칸을 찾아 pg.Cell 을 그대로 부른다 - 이관과 쓰기가 같은 함수 · 같은 카탈로그를 지나므로 바이트가 같다는 것이 구성으로 선다(두
// 벌이면 한쪽만 고쳐지는 표류가 난다).
//
//   - 자유 글 칸의 NUL → `\x00` 표기(RewriteNUL), JSON 칸의 `\u0000` 글 토큰 → 그 토큰만 고친다(RewriteJSONNUL). 바꾼 칸은 커밋
//     뒤에 Info 로그 한 줄씩(표 · 칸 · 갈래 · JSON 자리 · 행 좌표, 글 내용은 안 싣는다) - 이관의 기록 파일 자리다. 원장에 닿은
//     것만 적는다: 영향 행이 0 인 문장(선점 없는 항목의 UPDATE claim), 행을 안 돌려준 INSERT … RETURNING(insertUnlessTaken 의
//     충돌), 실패한 문장, 저장점까지 되돌린 문장(Tx.Savepoint)의 바꿔 넣기는 버린다.
//   - 식별자 칸의 NUL · 깨진 UTF-8 · JSON 키의 U+0000 · 짝 없는 서로게이트 → *CellRefusal. 문장을 보내지 않고, 트랜잭션을
//     망가진 것으로 표시해(poison) 부른 쪽이 오류를 삼켜도 아무것도 커밋되지 않는다(Postgres 가 22021 로 트랜잭션을 버리는 것과
//     같은 결말이 두 엔진에서 난다).
//   - 칸에 담기지 않는 글 인자(조회 조건 · 식 · 조회 · 지우기 문장의 인자)는 이름 규칙이다(nulesc.CheckName - NUL · 깨진 UTF-8 은
//     *CellRefusal). 그 글은 원장의 값과 맞대는 열쇠라 바꿔 넣을 수 없고, Postgres 는 22021 로 떨군다. bytea 칸의 값은 재지 않는다.
//
// ★ 꼴은 닫힌 목록이다(fail-closed). 관문이 인자가 닿는 자리를 모르는 문장은 보내지 않고 오류다: 모르는 첫 낱말, 쓰기를 품은
// WITH, 카탈로그 밖 표(옮기지 않는 관리 표 pg.LeftBehind 는 칸 없이 받는다) · 칸, 따옴표 식별자, 위치 매개변수(`$1`), 자리표시
// 수와 다른 인자 수(이름 붙은 매개변수 `:이름`), sql.Named 인자, 드라이버 값으로 못 옮기는 인자. 그런 문장은 처음 도는 자리에서
// 늘 시끄럽게 걸린다(인자와 상관없이 같은 결과) - 칸에 안 묶인 채 NUL 을 Postgres 까지 보내는 것보다 낫다.
// ★ 조회 · 지우기 · 저장점 문장은 첫 낱말만 보고 지나간다(문장을 다 나누지 않는다 - 트랜잭션 안 조회마다 드는 몫이다).
//
// ★ 유지 보수 길(sessionTx 의 증분 · SQLite 이관, 역이관 · 덤프, 되읽기)은 관문을 안 지난다. 그 길의 값은 이미 원장에 있는
// 원문이거나 이관이 따로 잰다 - 거기에 바꿔 넣기를 걸면 되돌리기 사본이 원장과 갈린다(pg.SettledCell 의 ★).
// ★ 원장에서 읽어 다시 적는 값(개정 이력의 옛 제목 · 본문)도 관문을 지나지만 바뀌지 않는다: 원장의 글에는 NUL 이 없고(관문 ·
// 이관이 다 바꿨다) 규칙은 두 번 걸어도 같다(nulesc.Text). 단 원장에서 읽은 글을 받은 글과 한 칸에 이으면 안 된다 - 받은 글에
// NUL 이 있으면 칸 전체가 한 글로 바뀌며 읽은 글의 `\x00` 표기가 `\\x00` 로 한 번 더 옮겨진다(web 의 폐기 본문이 그래서 제목을
// 안 싣는다).

// CellRefusal 은 원장 칸 관문이 거절한 글 하나다. Fault 가 입력의 잘못이면(Unwrap 이 nulesc 의 거절 갈래) 400 으로 가고, 서버가
// 만든 값의 결함이면(깨진 JSON) 500 이다.
type CellRefusal struct {
	// Table · Column 은 글이 담기는 원장 칸이다. 칸에 담기지 않는 인자(Arg 가 찬)면 Table 은 쓰기 문장의 표(조회 · 지우기
	// 문장이면 빈 글), Column 은 그 인자를 맞대는 이름(`이름 = ?` 의 이름, 없으면 빈 글)이다.
	Table, Column string
	// Arg 는 칸에 담기지 않는 인자의 차례(1부터)다. 칸 값이면 0 이다.
	Arg int
	// Stmt 는 문장의 첫 낱말(INSERT · UPDATE · SELECT …)이다.
	Stmt  string
	Fault pg.Fault
	// Where 는 행 좌표다(표의 식별자 칸 · 조회 조건의 값, JSON 글 하나를 거절했으면 그 글의 자리를 덧붙인다).
	Where string
}

// Place 는 거절한 글의 자리를 사람이 읽는 말로 낸다: 「원장 칸 item.title」, 「UPDATE item 문장의 인자 3(id)」, 「SELECT 문장의
// 인자 1」. 오류 문구와 API 의 400 응답(api 의 classifyRefusal)이 이것 하나로 적는다.
func (e *CellRefusal) Place() string {
	if e.Arg == 0 {
		return "원장 칸 " + e.Table + "." + e.Column
	}
	p := strings.TrimSpace(e.Stmt+" "+e.Table) + " 문장의 인자 " + strconv.Itoa(e.Arg)
	if e.Column != "" {
		p += "(" + e.Column + ")"
	}
	return p
}

func (e *CellRefusal) Error() string {
	return fmt.Sprintf("%s 가 받을 수 없는 글이다(%s · %s)", e.Place(), e.Fault, e.Where)
}

// Unwrap 은 거절 갈래를 입력 규칙(nulesc)의 갈래로 옮긴다. 서버의 결함(깨진 JSON)이면 nil 이다.
func (e *CellRefusal) Unwrap() error {
	switch e.Fault {
	case pg.FaultIdentNUL, pg.FaultSecretNUL:
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

// cellGate 는 업무 쓰기 트랜잭션 하나의 관문 기록이다(runTx 가 txConn 에 단다). 원장에 닿은 바꿔 넣기만 모아 두었다가 커밋 뒤에
// 로그로 낸다 - 롤백된 쓰기 · 0 행 문장 · 되돌린 저장점을 「바꿔 넣었다」고 적지 않는다. nil 이면 관문이 없는 트랜잭션이다.
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

// keep 은 원장에 닿은 문장의 바꿔 넣기 pend 를 남긴다.
func (g *cellGate) keep(pend []cellRewrite) {
	if g != nil {
		g.rewrites = append(g.rewrites, pend...)
	}
}

// mark 는 지금까지 남긴 바꿔 넣기의 수다(저장점을 만든 자리 - Tx.Savepoint). undo 가 그 자리로 되돌린다.
func (g *cellGate) mark() int {
	if g == nil {
		return 0
	}
	return len(g.rewrites)
}

// undo 는 저장점까지 되돌린 문장들의 바꿔 넣기를 버린다(mark 가 낸 자리까지).
func (g *cellGate) undo(mark int) {
	if g != nil && mark <= len(g.rewrites) {
		g.rewrites = g.rewrites[:mark]
	}
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

// gateArgs 는 문장 q 의 인자 args 에 그것이 닿는 자리의 규칙을 건 인자와, 그 문장이 원장에 닿으면 남길 바꿔 넣기다. 바꿀 것이
// 없으면 args 그대로다(새로 만들지 않는다). 부른 쪽의 조각은 고치지 않는다. 꼴을 못 읽거나 인자를 자리에 못 묶으면 오류다.
func gateArgs(q string, args []any) ([]any, []cellRewrite, error) {
	sh, err := shapeOf(q)
	if err != nil {
		return nil, nil, err
	}
	if !sh.read && len(args) != len(sh.slots) {
		return nil, nil, fmt.Errorf("원장 칸 관문이 인자를 칸에 묶지 못한다 - 자리표시 `?` %d 개에 인자 %d 개다(이름 붙은 매개변수는 "+
			"못 쓴다): %s", len(sh.slots), len(args), clip(q, 200))
	}
	var out []any
	var pend []cellRewrite
	for i, a := range args {
		v, err := driverValue(a, sh.read)
		if err != nil {
			return nil, nil, fmt.Errorf("원장 칸 관문이 인자 %d 를 읽지 못한다(%w): %s", i+1, err, clip(q, 200))
		}
		var sl cellSlot
		if !sh.read {
			sl = sh.slots[i]
		}
		switch {
		case sl.col != nil && sl.col.Kind == pg.Bytes:
			continue // bytea 는 어떤 바이트든 담는다
		case sl.col != nil && sl.col.Kind == pg.Text:
			if !isText(v) {
				continue // 수 · 시각 · NULL - 드라이버가 글로 적는 값에는 NUL 이 없다
			}
			nv, fault, rw := pg.Cell(*sl.col, v)
			switch {
			case fault != "":
				return nil, nil, &CellRefusal{Table: sh.table, Column: sl.col.Name, Stmt: sh.stmt, Fault: fault,
					Where: pg.FaultLabel(sh.row(args), *sl.col, v, fault)}
			case rw != "":
				if out == nil {
					out = slices.Clone(args)
				}
				out[i] = nv
				pend = append(pend, cellRewrite{table: sh.table, column: sl.col.Name, rule: rw,
					pointers: pg.RewritePaths(*sl.col, v), row: sh.row(args)})
			}
		default:
			// 칸에 담기지 않는 글(조회 조건 · 식 · 조회 문장의 인자)이나 수 칸에 든 글 - 이름 규칙이다.
			s, ok := v.(string)
			if !ok {
				continue
			}
			kind := nulesc.CheckName(s)
			if kind == nil {
				continue
			}
			fault := pg.FaultIdentNUL
			if errors.Is(kind, nulesc.ErrUTF8) {
				fault = pg.FaultUTF8
			}
			ref := &CellRefusal{Table: sh.table, Column: sl.coord, Arg: i + 1, Stmt: sh.stmt, Fault: fault, Where: sh.row(args)}
			if sl.col != nil {
				ref.Column, ref.Arg = sl.col.Name, 0
			}
			return nil, nil, ref
		}
	}
	if out == nil {
		return args, pend, nil
	}
	return out, pend, nil
}

// driverValue 는 인자 v 를 드라이버가 받는 값으로 옮긴다(database/sql 의 기본 변환 - driver.Valuer · 포인터 · 이름 붙은 기본
// 타입을 푼다). 관문은 이 값으로 잰다 - 원장에 닿는 바이트가 이것이다. 이름 붙은 인자(sql.Named)는 칸에 쓰는 문장(write)이면
// 오류다(자리표시 차례로 칸에 묶는다).
func driverValue(v any, read bool) (driver.Value, error) {
	if n, ok := v.(sql.NamedArg); ok {
		if !read {
			return nil, fmt.Errorf("이름 붙은 인자 %q 는 칸에 묶지 못한다", n.Name)
		}
		v = n.Value
	}
	return driver.DefaultParameterConverter.ConvertValue(v)
}

// isText 는 드라이버 값 v 가 글 칸에 글로 담기는 값(글 · 바이트)인가다.
func isText(v driver.Value) bool {
	switch v.(type) {
	case string, []byte:
		return true
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// 문장 꼴
// ─────────────────────────────────────────────────────────────────────────────

// cellShape 는 문장 하나의 꼴이다: 첫 낱말, 그리고 쓰기 문장이면 그 표와 자리표시 차례마다 그 인자가 닿는 자리(칸 · 행 좌표
// 이름)다. 조회 · 지우기 · 저장점 문장(read)은 칸에 쓰는 인자가 없다 - 인자는 다 이름 규칙이다.
type cellShape struct {
	stmt  string
	read  bool
	table string
	slots []cellSlot
}

type cellSlot struct {
	col   *pg.Column // 인자가 그대로 담기는 칸(`칸 = ?` · VALUES 의 `?` 하나). 조회 조건 · 식 · 옮기지 않는 관리 표면 nil
	coord string     // 행 좌표 이름(식별자 칸 · 조회 조건 `이름 = ?`). 빈 것은 싣지 않는다
	// coordCol 은 좌표 이름이 가리키는 표의 칸이다(모르면 nil). 비밀 칸이면 값 대신 지문을 싣는다(pg.CoordLabel).
	coordCol *pg.Column
}

// row 는 인자 args 로 이 문장이 쓰는 행의 좌표를 적는다(「project="fd" id="x"」 - 이관의 행 좌표 pg.KeyLabel 과 같은 꼴). 글 내용이
// 아니라 이름이다 - 식별자 칸과 조회 조건만 싣는다. 비밀 칸(web_session.id_hash 등)은 지문이다.
func (sh *cellShape) row(args []any) string {
	var parts []string
	for i, sl := range sh.slots {
		if sl.coord == "" || i >= len(args) {
			continue
		}
		v := args[i]
		if dv, err := driverValue(v, true); err == nil {
			v = dv
		}
		parts = append(parts, sl.coord+"="+pg.CoordLabel(sl.coordCol, v))
	}
	return strings.Join(parts, " ")
}

// readVerbs 는 첫 낱말만 보고 지나가는 문장이다 - 칸에 값을 쓰지 않는다(조회 · 지우기 · 저장점 · 세션 설정). 인자는 다 이름
// 규칙이다. 문장을 다 나누지 않고 기억하지도 않는다(IN 목록처럼 길이가 갈리는 조회가 기억을 키우지 않는다).
var readVerbs = map[string]*cellShape{}

func init() {
	for _, v := range []string{"SELECT", "DELETE", "SAVEPOINT", "RELEASE", "ROLLBACK", "PRAGMA", "SET"} {
		readVerbs[v] = &cellShape{stmt: v, read: true}
	}
}

// shapes 는 나눠 읽은 문장 꼴의 기억이다(문장 글 → *shapeMemo): INSERT · UPDATE · WITH 의 꼴과 못 읽은 오류. store 의 그런
// 문장은 상수이거나 상수 조각을 이은 것이라 가짓수가 정해져 있다.
var shapes sync.Map

type shapeMemo struct {
	sh  *cellShape
	err error
}

// shapeOf 는 문장 q 의 꼴이다. 첫 낱말이 조회 · 지우기 · 저장점이면 그것만 보고(readVerbs), INSERT · UPDATE 는 칸마다 인자가 닿는
// 자리를 읽고, WITH 는 쓰기를 품지 않았을 때만 조회다. 그 밖은 오류다(파일 머리의 ★ fail-closed). 칸 값을 식으로 쓴 글 칸
// (`title = COALESCE(?, title)` 처럼 인자가 칸 값 전체가 아닌 자리)도 오류다 - 관문이 칸에 담길 바이트를 모른다.
func shapeOf(q string) (*cellShape, error) {
	verb := strings.ToUpper(firstWord(q))
	if sh, ok := readVerbs[verb]; ok {
		return sh, nil
	}
	if verb != "INSERT" && verb != "UPDATE" && verb != "WITH" {
		return nil, fmt.Errorf("원장 칸 관문이 모르는 문장이다(첫 낱말 %q) - 인자가 닿는 자리를 못 읽는다: %s", verb, clip(q, 200))
	}
	if m, ok := shapes.Load(q); ok {
		memo := m.(*shapeMemo)
		return memo.sh, memo.err
	}
	toks := slices.Collect(lexSQL(q))
	var sh *cellShape
	var err error
	switch verb {
	case "INSERT":
		sh, err = parseInsert(q, toks)
	case "UPDATE":
		sh, err = parseUpdate(q, toks)
	default:
		sh, err = parseWith(q, toks)
	}
	if err != nil {
		sh, err = nil, fmt.Errorf("원장 칸 관문이 쓰기 문장의 꼴을 못 읽는다(%w): %s", err, clip(q, 200))
	}
	shapes.Store(q, &shapeMemo{sh: sh, err: err})
	return sh, err
}

// firstWord 는 q 의 첫 낱말이다(앞의 공백 · 주석은 건너뛴다). 낱말로 시작하지 않으면 빈 글이다.
func firstWord(q string) string {
	for t := range lexSQL(q) {
		if t.kind == tokWord {
			return q[t.at:t.end]
		}
		break
	}
	return ""
}

// parseWith 는 `WITH …` 를 읽는다: 쓰기(INSERT · UPDATE · DELETE)를 품었으면 오류고, 아니면 조회다. 쓰기를 품은 WITH 의 칸은
// 읽지 않는다 - 그런 문장을 쓰려면 꼴 읽기를 먼저 넓힌다.
func parseWith(q string, toks []sqlTok) (*cellShape, error) {
	for _, t := range toks {
		if t.kind != tokWord {
			continue
		}
		if w := strings.ToUpper(q[t.at:t.end]); w == "INSERT" || w == "UPDATE" || w == "DELETE" {
			return nil, fmt.Errorf("WITH 가 쓰기(%s)를 품었다", w)
		}
	}
	return &cellShape{stmt: "WITH", read: true}, nil
}

// stmt 는 꼴을 읽는 동안의 자리다.
type stmt struct {
	q     string
	toks  []sqlTok
	i     int
	marks []int     // 낱말 차례 → 자리표시 차례(자리표시가 아니면 -1)
	tb    *pg.Table // 쓰기 대상 표. 옮기지 않는 관리 표(pg.LeftBehind)면 nil - 칸을 묶지 않는다
	sh    *cellShape
}

func newStmt(q string, toks []sqlTok, verb string) (*stmt, error) {
	st := &stmt{q: q, toks: toks, marks: make([]int, len(toks)), sh: &cellShape{stmt: verb}}
	n := 0
	for k, t := range toks {
		st.marks[k] = -1
		switch t.kind {
		case tokMark:
			st.marks[k] = n
			n++
		case tokPos:
			return nil, errors.New("위치 매개변수(`$n`)는 칸에 묶지 못한다 - 자리표시 `?` 로 써라")
		}
	}
	st.sh.slots = make([]cellSlot, n)
	return st, nil
}

// table 은 지금 자리의 이름을 쓰기 대상 표로 읽는다. 이름은 대소문자를 가리지 않는다(따옴표 없는 SQL 이름이다 - 따옴표 식별자는
// word 가 못 읽어 오류다). 카탈로그 밖이면 오류다 - 옮기지 않는 관리 표(pg.LeftBehind)만 칸 없이 받는다.
func (st *stmt) table() error {
	name, err := st.word()
	if err != nil {
		return err
	}
	name = strings.ToLower(name)
	st.sh.table = name
	if tb, ok := pg.TableOf(name); ok {
		st.tb = &tb
		return nil
	}
	if pg.LeftBehind(name) {
		return nil
	}
	return fmt.Errorf("표 %q 가 이관 카탈로그에 없다 - 칸 규칙을 모른다", name)
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

// value 는 칸 col 에 담기는 식(from..to)을 잰다: `?` 하나면 그 자리표시가 칸 값이다(칸에 묶는다). 식 안에 자리표시가 있는 글
// 칸이면 오류다 - 그 밖의 칸(수 · 바이트)의 식 안 자리표시는 칸에 안 묶인 인자로 남는다. 표에 없는 칸이면 오류다.
func (st *stmt) value(col string, from, to int) error {
	if st.tb == nil {
		return nil // 옮기지 않는 관리 표 - 칸 규칙이 없다
	}
	c := st.tb.Col(strings.ToLower(col))
	if c == nil {
		return fmt.Errorf("표 %s 에 칸 %q 가 없다", st.tb.Name, col)
	}
	if to-from == 1 && st.marks[from] >= 0 {
		st.sh.slots[st.marks[from]] = cellSlot{col: c, coord: coordOf(c), coordCol: c}
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
			sl := &st.sh.slots[st.marks[k]]
			sl.coord = st.text(k - 2)
			if st.tb != nil {
				sl.coordCol = st.tb.Col(strings.ToLower(sl.coord))
			}
		}
	}
}

// parseInsert 는 `INSERT INTO 표(칸, …) VALUES (식, …)[, (…)] [ON CONFLICT … [DO UPDATE SET 칸 = 식, …]] [RETURNING …]` 을 읽는다.
func parseInsert(q string, toks []sqlTok) (*cellShape, error) {
	st, err := newStmt(q, toks, "INSERT")
	if err != nil {
		return nil, err
	}
	st.i = 1
	if err := st.expect("INTO"); err != nil {
		return nil, err
	}
	if err := st.table(); err != nil {
		return nil, err
	}
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

// parseUpdate 는 `UPDATE 표 SET 칸 = 식, … [FROM …] [WHERE …] [RETURNING …]` 을 읽는다.
func parseUpdate(q string, toks []sqlTok) (*cellShape, error) {
	st, err := newStmt(q, toks, "UPDATE")
	if err != nil {
		return nil, err
	}
	st.i = 1
	if err := st.table(); err != nil {
		return nil, err
	}
	if err := st.expect("SET"); err != nil {
		return nil, err
	}
	if err := st.assignments("WHERE", "FROM", "RETURNING"); err != nil {
		return nil, err
	}
	st.conditions()
	return st.sh, nil
}
