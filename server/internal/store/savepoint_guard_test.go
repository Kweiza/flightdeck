package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// 소스 잠금 - 트랜잭션 안에서 DB 오류를 받고 이어 가는 자리는 저장점 안에서만
// ─────────────────────────────────────────────────────────────────────────────
//
// Postgres 는 문장 하나가 실패하면 그 트랜잭션의 뒤 문장을 다 거절하고(25P02) 커밋도 롤백으로 끝난다. 그래서 store 의
// 트랜잭션 핸들(*Tx · *ObsTx · Reader · dbtx · *txConn - store 밖에서는 store.X 로 쓴 것만)을 쥔 함수에서 그 핸들로 낸
// 호출의 오류를 받고 이어 가는 꼴은 Postgres 에서 틀린다 - 오류를 Tx.Savepoint 안에서 받거나, ON CONFLICT DO NOTHING
// 으로 오류를 안 만들어야 한다(Tx 주석). 이 시험은 그 꼴 셋을 구문으로 찾는다(e = 트랜잭션 호출에서 받은 오류 변수,
// 저장점 호출에서 받은 것은 뺀다):
//
//	흡수: e 가 있을 때 들어가는 갈래(e != nil, errors.As(e, &DB 오류 타입), 분류기(e))에 오류를 내며 나가지 않는 길이 있다.
//	      마지막 값이 nil 인 return(return nil · return v, nil)도 오류를 버린다. 갈래를 그냥 빠져나가도 바로 다음 문장이
//	      그 e 를 내는 return 이면(if e != nil { 기록 }; return e) 전파라 안 친다
//	통과: if 의 Init 에서 받은 e 를 e == nil 로 보고, 오류일 때 오류를 내며 나가는 갈래가 사슬에 없다(return nil 로
//	      나가는 else if · else 는 오류를 버린 것이라 나가는 갈래로 안 친다)
//	이어 쓰기: e 가 있을 때 들어가는 갈래 안에서 다시 트랜잭션 호출을 한다(저장점 열기도 이미 버려진 트랜잭션에선 실패한다)
//
// ★ 문장 실패가 아닌 오류는 흡수해도 된다: 표지 오류(errors.Is(e, …), ErrNotFound 등은 행이 없다는 뜻이다)와
// 도메인 오류 타입(errors.As(e, &v) 의 v 가 DB 오류 타입이 아닌 것 - 예: 반납할 때 남이 쥐고 있었다는 ResourceHeldError).
// 그 둘만 거르는 갈래 안은 안 본다. 그 오류를 내는 쪽이 문장을 실패시키지 않는다는 것은 이 시험이 아니라 그쪽 시험이 진다.
//
// ★ 구문 판정이라 흐름은 못 본다: if 의 Init 밖에서 받은 e 를 e == nil 로만 보고 흘려보내는 꼴, 오류를 _ 로 버리는
// 꼴은 못 잡는다. 저장점 없이 오류를 받는 것이 맞는 자리는 까닭과 함께 아래 목록에 둔다.

// TestTxErrorsAreAbsorbedOnlyInsideSavepoints 는 server 의 시험 아닌 Go 소스 전부에서 위 세 꼴을 찾는다.
func TestTxErrorsAreAbsorbedOnlyInsideSavepoints(t *testing.T) {
	probeTxAbsorbScanner(t)

	fset, files := parseServerSource(t)
	hits, contexts := scanTxAbsorbs(fset, files)
	t.Logf("파일 %d개, 트랜잭션 문맥 %d개, 걸린 자리 %d개", len(files), len(contexts), len(hits))
	// 빈 탐지가 초록으로 숨지 않게 문맥 인식을 대표 자리로 잰다. 핸들 종류(받는 쪽 *Tx · 매개 dbtx · Reader · 클로저의
	// *store.Tx)마다 하나씩, 그리고 핸들이 아닌 같은 이름(io.Reader)은 문맥이 아니어야 한다.
	for at, want := range map[site]bool{
		{"internal/store/resource.go", "AcquireResource"}:   true,
		{"internal/store/landing.go", "EnqueueLanding"}:     true,
		{"internal/store/store.go", "Savepoint"}:            true,
		{"internal/store/resource.go", "heldBy"}:            true,
		{"internal/store/backup.go", "readLedgerJudgments"}: true,
		{"internal/service/finish.go", "Finish"}:            true,
		{"internal/service/session.go", "OpenSession"}:      true,
		{"cmd/fd/hook.go", "runHook"}:                       false,
		{"internal/mcpsrv/mcpsrv.go", "Serve"}:              false,
	} {
		if contexts[at] != want {
			t.Errorf("%s 의 트랜잭션 문맥 판정이 %v 다(기대 %v) - 문맥 인식이 죽었거나 좌표가 틀렸다", at, contexts[at], want)
		}
	}

	allowed := map[site]string{
		{"internal/store/store.go", "Savepoint"}: "저장점 자신 - 실패한 fn 뒤의 ROLLBACK TO 가 이 꼴의 정답이다",
	}
	seen := map[site]bool{}
	var bad []string
	for _, h := range hits {
		if _, ok := allowed[h.at]; ok {
			seen[h.at] = true
			continue
		}
		bad = append(bad, h.String(fset))
	}
	if len(bad) > 0 {
		t.Errorf("트랜잭션 안에서 DB 오류를 받고 이어 가는 자리가 저장점 밖에 있다 - Postgres 에서 그 트랜잭션은 25P02 로 버려진다.\n"+
			"Tx.Savepoint(또는 store.SavepointValue) 안에서 받거나 ON CONFLICT DO NOTHING 으로 오류를 안 만들어라(store.Tx 주석):\n  %s",
			strings.Join(bad, "\n  "))
	}
	for at, why := range allowed {
		if !seen[at] {
			t.Errorf("예외 목록이 낡았다: %s(%s)에 그 꼴이 더는 없다 - 목록에서 지워라", at, why)
		}
	}
}

// probeTxAbsorbScanner 는 탐지가 살아 있는지 합성 소스로 잰다. 세 꼴이 각각 잡히고, 같은 꼴을 저장점 안에서 받거나
// 오류를 내며 나가거나 바로 뒤에서 전파하거나 문장 실패가 아닌 오류만 흡수하는 것은 안 잡혀야 한다. 핸들은 store 의
// 것만 문맥이다 - store 안의 맨 이름, 밖의 store.X.
func probeTxAbsorbScanner(t *testing.T) {
	t.Helper()
	const storeSrc = `package store

func absorb(t *Tx) error {
	if err := t.AddItem(x); err != nil {
		log(err)
	}
	return nil
}

func writeAfter(t *Tx) error {
	_, err := t.tx.ExecContext(t.ctx, "INSERT")
	if err != nil {
		held, _ := heldBy(t.ctx, t.tx, p, r)
		return conflict(held)
	}
	return nil
}

func returnNil(t *Tx) error {
	if err := t.AddItem(x); err != nil {
		return nil
	}
	return nil
}

func returnValueNil(t *Tx) (Item, error) {
	it, err := t.GetItem(p, id)
	if err != nil {
		return Item{}, nil
	}
	return it, nil
}

func propagate(t *Tx) error {
	err := t.AddItem(x)
	if err != nil {
		log(err)
	}
	return err
}

func propagateReset(t *Tx) error {
	err := t.AddItem(x)
	if err != nil {
		err = nil
	}
	return err
}

func obs(o *ObsTx) error {
	if _, err := o.tx.ExecContext(o.ctx, "INSERT"); err != nil {
		log(err)
	}
	return nil
}

func ledger(ctx context.Context, q Reader) error {
	if _, err := q.QueryContext(ctx, "SELECT"); Unabsorbable(err) {
		log(err)
	}
	return nil
}

func fine(t *Tx) error {
	if err := t.Savepoint(func(sp *Tx) error { return sp.AddItem(x) }); err != nil {
		log(err)
	}
	it, err := SavepointValue(t, func(sp *Tx) (Item, error) { return sp.GetItem(p, id) })
	if err != nil {
		why = "못 읽었다"
	}
	if err := t.AddItem(x); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	for _, h := range holds {
		if err := t.ReleaseResource(h); err != nil {
			var held *ResourceHeldError
			if errors.Is(err, ErrNotFound) || errors.As(err, &held) {
				t.LogEvent("skipped")
				continue
			}
			return err
		}
	}
	if c, cerr := t.claimRow(p, id); cerr == nil {
		use(c)
	} else if !errors.Is(cerr, ErrNotFound) {
		return cerr
	}
	if _, err := t.GetItem(p, id); err == nil {
		return nil
	} else {
		return err
	}
}

func passElseIfNil(t *Tx) error {
	if c, cerr := t.claimRow(p, id); cerr == nil {
		use(c)
	} else if !errors.Is(cerr, ErrNotFound) {
		return nil
	}
	return nil
}

func passElseNil(t *Tx) (Item, error) {
	if c, cerr := t.claimRow(p, id); cerr == nil {
		use(c)
	} else if errors.Is(cerr, ErrNotFound) {
		skip()
	} else {
		return Item{}, nil
	}
	return Item{}, nil
}
`
	const serviceSrc = `package service

func passThrough(t *store.Tx) {
	if it, err := t.GetItem(p, id); err == nil {
		use(it)
	}
}

func closure(s *store.Store) error {
	return s.Tx(ctx, func(t *store.Tx) error {
		v, err := t.GetItem(p, id)
		switch {
		case err != nil:
			why = "못 읽었다"
		}
		return use(v)
	})
}

func duplicate(t *store.Tx) error {
	for _, it := range items {
		if err := t.AddItem(it); err != nil {
			var conflict *store.ConflictError
			if errors.As(err, &conflict) && conflict.Kind == store.ConflictDuplicate {
				continue
			}
			return err
		}
	}
	return nil
}

func readsInput(r io.Reader) error {
	if _, err := r.Read(b); err != nil {
		log(err)
	}
	return nil
}

func otherTx(t *Tx) error {
	if err := t.AddItem(x); err != nil {
		log(err)
	}
	return nil
}
`
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for name, src := range map[string]string{"store.go": storeSrc, "service.go": serviceSrc} {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
	}
	hits, contexts := scanTxAbsorbs(fset, files)
	got := map[string]bool{}
	for _, h := range hits {
		got[h.at.fn+" "+h.rule] = true
	}
	want := map[string]bool{
		"absorb " + ruleAbsorb: true, "writeAfter " + ruleWriteAfter: true, "returnNil " + ruleAbsorb: true,
		"returnValueNil " + ruleAbsorb: true, "propagateReset " + ruleAbsorb: true, "obs " + ruleAbsorb: true,
		"ledger " + ruleAbsorb:      true,
		"passElseIfNil " + rulePass: true, "passElseNil " + rulePass: true,
		"passThrough " + rulePass: true, "closure " + ruleAbsorb: true, "duplicate " + ruleAbsorb: true,
	}
	gotCtx := map[string]bool{}
	for at := range contexts {
		gotCtx[at.fn] = true
	}
	wantCtx := map[string]bool{
		"absorb": true, "writeAfter": true, "returnNil": true, "returnValueNil": true, "propagate": true,
		"propagateReset": true, "obs": true, "ledger": true, "fine": true, "passElseIfNil": true, "passElseNil": true,
		"passThrough": true, "closure": true, "duplicate": true,
	}
	if !maps.Equal(got, want) || !maps.Equal(gotCtx, wantCtx) {
		t.Fatalf("탐지가 합성 소스에서 어긋났다\n 걸림 얻음 %v\n 걸림 기대 %v\n 문맥 얻음 %v\n 문맥 기대 %v", got, want, gotCtx, wantCtx)
	}
}

// parseServerSource 는 server 모듈의 시험 아닌 Go 파일 전부다(키는 server 기준 상대 경로).
func parseServerSource(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "testdata" || (strings.HasPrefix(n, ".") && p != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = f
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files["internal/store/store.go"] == nil || len(files) < 100 {
		t.Fatalf("server 의 시험 아닌 파일이 %d개뿐이다 - 이 시험의 좌표(작업 디렉토리)가 틀렸다", len(files))
	}
	return fset, files
}

const (
	ruleAbsorb     = "흡수"
	rulePass       = "통과"
	ruleWriteAfter = "이어 쓰기"
)

type absorbHit struct {
	at   site
	rule string
	pos  token.Pos
}

func (h absorbHit) String(fset *token.FileSet) string {
	return h.rule + " " + h.at.String() + " (" + fset.Position(h.pos).String() + ")"
}

// scanTxAbsorbs 는 files 에서 세 꼴을 찾는다. 둘째 값은 트랜잭션 핸들을 쥔 함수(안의 함수 리터럴이 쥔 것 포함)의 자리다.
func scanTxAbsorbs(fset *token.FileSet, files map[string]*ast.File) (hits []absorbHit, contexts map[site]bool) {
	contexts = map[site]bool{}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		pkg := files[name].Name.Name
		for _, d := range files[name].Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			handles := map[string]bool{}
			addTxHandles(handles, fd.Recv, pkg)
			addTxHandles(handles, fd.Type.Params, pkg)
			sc := &txScan{at: site{name, fd.Name.Name}, pkg: pkg}
			sc.body(fd.Body, handles)
			hits = append(hits, sc.hits...)
			if sc.context {
				contexts[sc.at] = true
			}
		}
	}
	return hits, contexts
}

// addTxHandles 는 fields 중 트랜잭션 핸들 타입인 이름을 h 에 더한다. pkg 는 그 소스의 패키지 이름이다.
func addTxHandles(h map[string]bool, fields *ast.FieldList, pkg string) {
	if fields == nil {
		return
	}
	for _, f := range fields.List {
		if !isTxHandleType(f.Type, pkg) {
			continue
		}
		for _, n := range f.Names {
			h[n.Name] = true
		}
	}
}

// isTxHandleType 은 e 가 store 의 트랜잭션 핸들 타입인가다: store 안에서는 맨 이름, 밖에서는 store.X 다. 같은 이름의
// 남의 타입(io.Reader, 다른 패키지의 Tx)은 핸들이 아니다.
func isTxHandleType(e ast.Expr, pkg string) bool {
	star, ptr := e.(*ast.StarExpr)
	if ptr {
		e = star.X
	}
	var name string
	switch e := e.(type) {
	case *ast.Ident:
		if pkg != "store" {
			return false
		}
		name = e.Name
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); !ok || x.Name != "store" {
			return false
		}
		name = e.Sel.Name
	default:
		return false
	}
	if ptr {
		return name == "Tx" || name == "ObsTx" || name == "txConn"
	}
	return name == "Reader" || name == "dbtx"
}

// typeName 은 T · *T · pkg.T · *pkg.T 의 T 다.
func typeName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return typeName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// txScan 은 함수 하나(그 안의 함수 리터럴은 따로)를 훑는다.
type txScan struct {
	at      site
	pkg     string
	hits    []absorbHit
	context bool // 이 함수나 그 안의 함수 리터럴이 트랜잭션 핸들을 쥐었다
}

// errOrigin 은 오류 변수가 마지막으로 어디서 왔나다.
type errOrigin int

const (
	fromOther errOrigin = iota
	fromTx
	fromSavepoint
)

type fnScan struct {
	sc       *txScan
	handles  map[string]bool
	origin   map[string]errOrigin
	varTypes map[string]string     // 함수 안 var 선언의 타입 이름. 같은 이름이 다른 타입으로 둘이면 "" 다
	next     map[ast.Stmt]ast.Stmt // 문장 목록에서 바로 다음 문장. else if 는 사슬 머리의 다음을 물려받는다
}

func (sc *txScan) body(body *ast.BlockStmt, handles map[string]bool) {
	f := &fnScan{sc: sc, handles: handles, origin: map[string]errOrigin{}, varTypes: map[string]string{},
		next: map[ast.Stmt]ast.Stmt{}}
	if len(handles) > 0 {
		sc.context = true
	}
	link := func(list []ast.Stmt) {
		for i := 0; i+1 < len(list); i++ {
			f.next[list[i]] = list[i+1]
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.BlockStmt:
			link(n.List)
		case *ast.CaseClause:
			link(n.Body)
		case *ast.CommClause:
			link(n.Body)
		case *ast.IfStmt:
			if els, ok := n.Else.(*ast.IfStmt); ok {
				f.next[els] = f.next[n]
			}
		case *ast.ValueSpec:
			if n.Type == nil {
				return true
			}
			tn := typeName(n.Type)
			for _, id := range n.Names {
				if prev, ok := f.varTypes[id.Name]; ok && prev != tn {
					tn = ""
				}
				f.varTypes[id.Name] = tn
			}
		}
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			inner := maps.Clone(handles)
			addTxHandles(inner, n.Type.Params, sc.pkg)
			sc.body(n.Body, inner)
			return false
		case *ast.AssignStmt:
			f.assign(n)
		case *ast.IfStmt:
			if len(handles) > 0 {
				f.ifStmt(n)
			}
		case *ast.SwitchStmt:
			if len(handles) > 0 {
				f.switchStmt(n)
			}
		}
		return true
	})
}

func (f *fnScan) hit(rule string, pos token.Pos) {
	f.sc.hits = append(f.sc.hits, absorbHit{f.sc.at, rule, pos})
}

// assign 은 오류 변수의 출처를 적는다. 여러 값을 받는 호출은 관례대로 마지막 값만 오류로 본다.
func (f *fnScan) assign(a *ast.AssignStmt) {
	for i, l := range a.Lhs {
		id, ok := l.(*ast.Ident)
		if !ok || id.Name == "_" {
			continue
		}
		var rhs ast.Expr
		switch {
		case len(a.Rhs) == len(a.Lhs):
			rhs = a.Rhs[i]
		case len(a.Rhs) == 1 && i == len(a.Lhs)-1:
			rhs = a.Rhs[0]
		}
		o := fromOther
		if call, ok := ast.Unparen(rhs).(*ast.CallExpr); ok {
			switch {
			case isSavepointCall(call):
				o = fromSavepoint
			case f.isTxCall(call):
				o = fromTx
			}
		}
		if o == fromOther {
			delete(f.origin, id.Name)
		} else {
			f.origin[id.Name] = o
		}
	}
}

func isSavepointCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == "SavepointValue"
	case *ast.SelectorExpr:
		return fn.Sel.Name == "Savepoint" || fn.Sel.Name == "SavepointValue"
	}
	return false
}

// notStatements 는 핸들의 메서드 중 문장을 안 내는 것이다(LogEvent 는 커밋 뒤로 미뤄 쓴다, savepointBroke 는 오류를
// 만들고 트랜잭션에 표시만 한다).
var notStatements = map[string]bool{"LogEvent": true, "Ctx": true, "savepointBroke": true}

// isHandle 은 e 가 트랜잭션 핸들(이름 또는 <핸들>.tx)인가다.
func (f *fnScan) isHandle(e ast.Expr) bool {
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident:
		return f.handles[e.Name]
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		return ok && f.handles[x.Name] && e.Sel.Name == "tx"
	}
	return false
}

// isTxCall 은 call 이 트랜잭션 핸들로 문장을 낼 수 있는 호출인가다: 핸들의 메서드, 그 결과에 이어 붙인 메서드,
// 핸들을 인자로 받는 함수.
func (f *fnScan) isTxCall(call *ast.CallExpr) bool {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if notStatements[sel.Sel.Name] {
			return false
		}
		if f.isHandle(sel.X) {
			return true
		}
		if inner, ok := ast.Unparen(sel.X).(*ast.CallExpr); ok && f.isTxCall(inner) {
			return true
		}
	}
	for _, a := range call.Args {
		if f.isHandle(a) {
			return true
		}
	}
	return false
}

// txErr 는 e 가 트랜잭션 호출에서 받은 오류 변수인가다.
func (f *fnScan) txErr(e ast.Expr) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	return ok && f.origin[id.Name] == fromTx
}

// errClassifiers 는 오류가 있을 때만 참이 되는 값을 내는 store 의 분류기다.
var errClassifiers = map[string]bool{"JudgeError": true, "ConstraintCode": true, "driverCodeOf": true, "conflictOf": true,
	"Unabsorbable": true}

// dbErrorTypes 는 문장 실패를 나르는 오류 타입이다(errors.As 의 대상이 이것이면 그 갈래는 DB 오류를 받는다).
var dbErrorTypes = map[string]bool{"ConflictError": true, "TxFault": true, "PgError": true, "Error": true}

// errorsFunc 는 call 이 트랜잭션 오류를 첫 인자로 받는 errors.Is · errors.As 면 그 이름이다.
func (f *fnScan) errorsFunc(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 2 || !f.txErr(call.Args[0]) {
		return ""
	}
	if x, ok := sel.X.(*ast.Ident); ok && x.Name == "errors" {
		return sel.Sel.Name
	}
	return ""
}

// asTargetIsDB 는 errors.As 의 대상이 DB 오류 타입인가다. 타입을 못 정하면 그렇다고 본다.
func (f *fnScan) asTargetIsDB(target ast.Expr) bool {
	u, ok := ast.Unparen(target).(*ast.UnaryExpr)
	if !ok || u.Op != token.AND {
		return true
	}
	id, ok := u.X.(*ast.Ident)
	if !ok {
		return true
	}
	tn, ok := f.varTypes[id.Name]
	return !ok || tn == "" || dbErrorTypes[tn]
}

// present 는 cond 가 참이면 트랜잭션 오류(문장 실패일 수 있는 것)가 있는가다(부정 아래는 안 본다).
func (f *fnScan) present(cond ast.Expr) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.UnaryExpr:
			return n.Op != token.NOT
		case *ast.FuncLit:
			return false
		case *ast.BinaryExpr:
			if n.Op == token.NEQ && (f.txErr(n.X) && isNil(n.Y) || f.txErr(n.Y) && isNil(n.X)) {
				found = true
			}
		case *ast.CallExpr:
			if f.errorsFunc(n) == "As" && f.asTargetIsDB(n.Args[1]) {
				found = true
			}
			if len(n.Args) > 0 && f.txErr(n.Args[0]) && errClassifiers[typeName(n.Fun)] {
				found = true
			}
		}
		return !found
	})
	return found
}

// notStatementFailure 는 cond 가 문장 실패가 아닌 오류(표지 오류 · 도메인 오류 타입)만 고르는가다.
func (f *fnScan) notStatementFailure(cond ast.Expr) bool {
	switch c := ast.Unparen(cond).(type) {
	case *ast.BinaryExpr:
		return c.Op == token.LOR && f.notStatementFailure(c.X) && f.notStatementFailure(c.Y)
	case *ast.CallExpr:
		switch f.errorsFunc(c) {
		case "Is":
			return true
		case "As":
			return !f.asTargetIsDB(c.Args[1])
		}
	}
	return false
}

// coversFailures 는 cond 가 문장 실패일 수 있는 트랜잭션 오류를 전부 고르는가다(e != nil, !errors.Is(e, …) 와 그 &&).
func (f *fnScan) coversFailures(cond ast.Expr) bool {
	switch c := ast.Unparen(cond).(type) {
	case *ast.BinaryExpr:
		switch c.Op {
		case token.NEQ:
			return f.txErr(c.X) && isNil(c.Y) || f.txErr(c.Y) && isNil(c.X)
		case token.LAND:
			return f.coversFailures(c.X) && f.coversFailures(c.Y)
		}
	case *ast.UnaryExpr:
		return c.Op == token.NOT && f.notStatementFailure(c.X)
	}
	return false
}

// absent 는 cond 에 「트랜잭션 오류 == nil」이 있으면 그 변수 이름이다(부정 아래는 안 본다).
func (f *fnScan) absent(cond ast.Expr) string {
	name := ""
	ast.Inspect(cond, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.UnaryExpr:
			return n.Op != token.NOT
		case *ast.FuncLit:
			return false
		case *ast.BinaryExpr:
			if n.Op == token.EQL {
				switch {
				case f.txErr(n.X) && isNil(n.Y):
					name = ast.Unparen(n.X).(*ast.Ident).Name
				case f.txErr(n.Y) && isNil(n.X):
					name = ast.Unparen(n.Y).(*ast.Ident).Name
				}
			}
		}
		return name == ""
	})
	return name
}

func isNil(e ast.Expr) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	return ok && id.Name == "nil"
}

func (f *fnScan) ifStmt(n *ast.IfStmt) {
	if a, ok := n.Init.(*ast.AssignStmt); ok {
		f.assign(a)
	}
	if f.present(n.Cond) {
		f.errBranch(n.Body.List, n.Body.Pos(), f.next[n])
	}
	e := f.absent(n.Cond)
	if e == "" {
		return
	}
	if els, ok := n.Else.(*ast.BlockStmt); ok {
		f.errBranch(els.List, els.Pos(), f.next[n])
		return
	}
	// 오류일 때 나가는 갈래가 사슬에 없으면 오류가 이 if 를 지나 흘러간다. Init 밖에서 받은 오류는 뒤에서 볼 수도 있어
	// 흐름 없이는 판정 못 한다 - Init 에서 받은 것만 본다(그 변수는 이 if 사슬 밖에서 안 보인다).
	if initDefines(n.Init, e) && !f.chainExitsOnFailure(n) {
		f.hit(rulePass, n.Pos())
	}
}

func (f *fnScan) switchStmt(n *ast.SwitchStmt) {
	if a, ok := n.Init.(*ast.AssignStmt); ok {
		f.assign(a)
	}
	if n.Tag != nil {
		return
	}
	for _, s := range n.Body.List {
		cc := s.(*ast.CaseClause)
		for _, e := range cc.List {
			if f.present(e) {
				f.errBranch(cc.Body, cc.Pos(), f.next[n])
				break
			}
		}
	}
}

// errBranch 는 트랜잭션 오류가 있을 때 들어가는 갈래를 본다. next 는 그 if · switch 바로 다음 문장이다(없으면 nil).
func (f *fnScan) errBranch(stmts []ast.Stmt, pos token.Pos, next ast.Stmt) {
	switch {
	case f.returnsNil(stmts):
		f.hit(ruleAbsorb, pos)
	case f.exits(stmts), f.propagatesAfter(stmts, next):
	default:
		f.hit(ruleAbsorb, pos)
	}
	if f.callsTx(stmts) {
		f.hit(ruleWriteAfter, pos)
	}
}

// returnsNil 은 stmts 에 마지막 값이 nil 인 return 이 있는가다(오류를 버리고 나간다). 문장 실패가 아닌 오류만 고른 갈래
// 안은 안 본다.
func (f *fnScan) returnsNil(stmts []ast.Stmt) bool {
	found := false
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		if found {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.IfStmt:
			if f.notStatementFailure(n.Cond) {
				if n.Else != nil {
					ast.Inspect(n.Else, visit)
				}
				return false
			}
		case *ast.ReturnStmt:
			found = len(n.Results) > 0 && isNil(n.Results[len(n.Results)-1])
		}
		return !found
	}
	for _, s := range stmts {
		ast.Inspect(s, visit)
	}
	return found
}

// propagatesAfter 는 갈래를 그대로 빠져나가면 바로 다음 문장이 그 오류를 내는 return 인가다(if e != nil { 기록 };
// return e). 갈래 안에서 그 변수를 다시 쓰거나 갈래 밖으로 건너뛰면(continue 등) 전파가 아니다.
func (f *fnScan) propagatesAfter(stmts []ast.Stmt, next ast.Stmt) bool {
	ret, ok := next.(*ast.ReturnStmt)
	if !ok || len(ret.Results) == 0 || f.escapes(stmts) {
		return false
	}
	id, ok := ast.Unparen(ret.Results[len(ret.Results)-1]).(*ast.Ident)
	if !ok || !f.txErr(id) {
		return false
	}
	reassigned := false
	for _, s := range stmts {
		ast.Inspect(s, func(n ast.Node) bool {
			if a, ok := n.(*ast.AssignStmt); ok {
				for _, l := range a.Lhs {
					if lid, ok := l.(*ast.Ident); ok && lid.Name == id.Name {
						reassigned = true
					}
				}
			}
			return !reassigned
		})
	}
	return !reassigned
}

// callsTx 는 stmts 가 트랜잭션 호출을 하는가다. 문장 실패가 아닌 오류만 고른 갈래 안은 안 본다.
func (f *fnScan) callsTx(stmts []ast.Stmt) bool {
	found := false
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		if found {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.IfStmt:
			if f.notStatementFailure(n.Cond) {
				if n.Else != nil {
					ast.Inspect(n.Else, visit)
				}
				return false
			}
		case *ast.CallExpr:
			found = f.isTxCall(n)
		}
		return !found
	}
	for _, s := range stmts {
		ast.Inspect(s, visit)
	}
	return found
}

func initDefines(init ast.Stmt, name string) bool {
	a, ok := init.(*ast.AssignStmt)
	if !ok {
		return false
	}
	for _, l := range a.Lhs {
		if id, ok := l.(*ast.Ident); ok && id.Name == name {
			return true
		}
	}
	return false
}

// chainExitsOnFailure 는 if · else if 사슬에 문장 실패일 수 있는 오류를 전부 받아 오류를 내며 나가는 갈래가 있는가다.
// 그 갈래가 return nil 로 나가면 오류를 버린 것이라 나가는 갈래로 안 친다(errBranch 의 returnsNil 과 같은 판정).
func (f *fnScan) chainExitsOnFailure(n *ast.IfStmt) bool {
	for {
		switch els := n.Else.(type) {
		case *ast.IfStmt:
			if f.coversFailures(els.Cond) && f.raises(els.Body.List) {
				return true
			}
			n = els
		case *ast.BlockStmt:
			return f.raises(els.List)
		default:
			return false
		}
	}
}

// raises 는 stmts 가 모든 길에서 함수를 나가고, 오류를 버리는 return(마지막 값이 nil)이 없는가다.
func (f *fnScan) raises(stmts []ast.Stmt) bool {
	return f.exits(stmts) && !f.returnsNil(stmts)
}

// exits 는 stmts 가 모든 길에서 함수를 나가는가다(return · panic). 갈래 밖으로 가는 continue · break · goto 가
// 하나라도 있으면 나가지 않는 길이 있다. 문장 실패가 아닌 오류만 고른 갈래 안은 안 본다.
func (f *fnScan) exits(stmts []ast.Stmt) bool {
	return !f.escapes(stmts) && endsInExit(stmts)
}

func endsInExit(stmts []ast.Stmt) bool {
	if len(stmts) == 0 {
		return false
	}
	switch s := stmts[len(stmts)-1].(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.ExprStmt:
		call, ok := s.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := ast.Unparen(call.Fun).(*ast.Ident)
		return ok && id.Name == "panic"
	case *ast.BlockStmt:
		return endsInExit(s.List)
	case *ast.IfStmt:
		switch els := s.Else.(type) {
		case *ast.BlockStmt:
			return endsInExit(s.Body.List) && endsInExit(els.List)
		case *ast.IfStmt:
			return endsInExit(s.Body.List) && endsInExit([]ast.Stmt{els})
		}
	}
	return false
}

// escapes 는 stmts 안에 그 갈래 밖으로 가는 분기문(continue · break · goto · 이름표)이 있는가다.
func (f *fnScan) escapes(stmts []ast.Stmt) bool {
	var walk func(n ast.Node, inLoop, inBreakable bool) bool
	walk = func(n ast.Node, inLoop, inBreakable bool) bool {
		switch s := n.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			inLoop, inBreakable = true, true
		case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			inBreakable = true
		case *ast.IfStmt:
			if f.notStatementFailure(s.Cond) {
				return s.Else != nil && walk(s.Else, inLoop, inBreakable)
			}
		}
		found := false
		ast.Inspect(n, func(m ast.Node) bool {
			if found || m == nil {
				return false
			}
			if m == n {
				return true
			}
			switch m := m.(type) {
			case *ast.FuncLit:
				return false
			case *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.IfStmt:
				found = walk(m, inLoop, inBreakable)
				return false
			case *ast.BranchStmt:
				switch {
				case m.Label != nil || m.Tok == token.GOTO:
					found = true
				case m.Tok == token.CONTINUE:
					found = !inLoop
				case m.Tok == token.BREAK:
					found = !inBreakable
				}
			}
			return true
		})
		return found
	}
	for _, s := range stmts {
		if walk(s, false, false) {
			return true
		}
	}
	return false
}
