package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ─────────────────────────────────────────────────────────────────────────────
// 자리표시 감싸개 - 순수 함수
// ─────────────────────────────────────────────────────────────────────────────

func TestBindDollar(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"번호는 나온 순서", `SELECT a FROM t WHERE x = ? AND y = ?`, `SELECT a FROM t WHERE x = $1 AND y = $2`},
		{"IN 목록", `WHERE id IN (?,?,?)`, `WHERE id IN ($1,$2,$3)`},
		{"자리표시가 없으면 그대로", `SELECT 1`, `SELECT 1`},
		{"위치 매개변수는 건드리지 않는다", `SELECT $1`, `SELECT $1`},
		{"문자열 안의 ?", `WHERE note = 'why?' AND id = ?`, `WHERE note = 'why?' AND id = $1`},
		{"따옴표 두 개는 글자 하나", `SELECT 'it''s ?', ?`, `SELECT 'it''s ?', $1`},
		{"표준 문자열의 역슬래시는 글자다", `SELECT 'x\', ?`, `SELECT 'x\', $1`},
		{"E 문자열의 역슬래시는 따옴표를 품는다", `SELECT E'\'?', ?`, `SELECT E'\'?', $1`},
		{"이름 끝의 e 는 E 문자열이 아니다", `SELECT type'?\', ?`, `SELECT type'?\', $1`},
		{"큰따옴표 식별자", `SELECT "col?" FROM t WHERE a = ?`, `SELECT "col?" FROM t WHERE a = $1`},
		{"줄 주석", "-- 판단?\nSELECT ?", "-- 판단?\nSELECT $1"},
		{"겹친 블록 주석", `/* a ? /* b ? */ c ? */ SELECT ?`, `/* a ? /* b ? */ c ? */ SELECT $1`},
		{"달러 따옴표", `DO $$ SELECT '?' $$; SELECT ?`, `DO $$ SELECT '?' $$; SELECT $1`},
		{"꼬리표 달린 달러 따옴표", `SELECT $fn$ ? $fn$, ?`, `SELECT $fn$ ? $fn$, $1`},
		{"이름 속의 $ 는 따옴표가 아니다", `SELECT a$b, ?`, `SELECT a$b, $1`},
		{"뒤에 이름 글자가 붙으면 띄운다", `WHERE a=?AND b=?`, `WHERE a=$1 AND b=$2`},
		{"닫히지 않은 문자열은 끝까지 문자열", `SELECT '?`, `SELECT '?`},
		{"여러 문장", `UPDATE t SET a = ?; SELECT ?`, `UPDATE t SET a = $1; SELECT $2`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := bindDollar(c.in)
			if err != nil || got != c.want {
				t.Fatalf("bindDollar(%q)\n 얻음 %q (%v)\n 기대 %q", c.in, got, err, c.want)
			}
		})
	}
	// 열 자리를 넘는 번호(두 자리 수)도 그대로 센다.
	q := strings.Repeat("?,", 11) + "?"
	if got, err := bindDollar(q); err != nil || !strings.HasSuffix(got, ",$12") || !strings.HasPrefix(got, "$1,$2,") {
		t.Fatalf("12개 자리표시의 번호가 다르다: %q (%v)", got, err)
	}
}

// TestBindDollarRefusesMixedPlaceholders 는 `?` 와 `$n` 을 섞은 문장을 거절하는지 잰다. 바꾸면 `SELECT $1, ?` 의
// `?` 가 $1 이 되어 인자 하나가 두 자리에 조용히 들어간다. 따옴표 · 주석 · 달러 따옴표 안의 `$1` 은 섞은 것이 아니다.
func TestBindDollarRefusesMixedPlaceholders(t *testing.T) {
	for _, q := range []string{`SELECT $1, ?`, `WHERE a = ? AND b = $2`, `SELECT ?, ($1)`} {
		if got, err := bindDollar(q); err == nil {
			t.Errorf("섞은 문장을 받았다: %q → %q", q, got)
		}
	}
	for q, want := range map[string]string{
		`SELECT '$1', ?`:       `SELECT '$1', $1`,
		"-- $1\nSELECT ?":      "-- $1\nSELECT $1",
		`SELECT $f$ $1 $f$, ?`: `SELECT $f$ $1 $f$, $1`,
		`SELECT a$1, ?`:        `SELECT a$1, $1`,
		`SELECT $1, $2`:        `SELECT $1, $2`,
	} {
		if got, err := bindDollar(q); err != nil || got != want {
			t.Errorf("bindDollar(%q) = %q (%v), 기대 %q", q, got, err, want)
		}
	}
}

func TestBindIsIdentityOnSQLite(t *testing.T) {
	q := `SELECT ? FROM t WHERE a = '?'`
	if got, err := engineSQLite.bind(q); err != nil || got != q {
		t.Fatalf("SQLite 는 ? 그대로여야 한다: %q (%v)", got, err)
	}
	if got, err := enginePG.bind(q); err != nil || got != `SELECT $1 FROM t WHERE a = '?'` {
		t.Fatalf("Postgres 는 $n 으로 옮겨야 한다: %q (%v)", got, err)
	}
}

// TestBindRefusalReachesEveryCallShape 는 감싸개가 거절한 문장이 세 실행 꼴(Exec · Query · QueryRow) 모두에서
// 오류로 나오는지 잰다. QueryRow 는 *sql.Row 에 오류를 못 실어 row 로 감싼다 - 그 길이 끊기면 거절이 조용히 사라진다.
func TestBindRefusalReachesEveryCallShape(t *testing.T) {
	c := &conn{eng: enginePG} // raw 가 없다 - 거절이 드라이버에 닿기 전에 나와야 한다
	const q = `SELECT $1, ?`
	ctx := context.Background()
	if _, err := c.ExecContext(ctx, q); err == nil {
		t.Error("Exec 가 섞은 문장을 받았다")
	}
	if _, err := c.QueryContext(ctx, q); err == nil {
		t.Error("Query 가 섞은 문장을 받았다")
	}
	r := c.QueryRowContext(ctx, q)
	var x int
	if r.Scan(&x) == nil || r.Err() == nil {
		t.Error("QueryRow 의 Scan · Err 가 거절을 안 냈다")
	}
}

// TestParseTargetClassifiesTrimmedButOpensFilesAsGiven 은 분류는 앞뒤 공백을 뺀 값으로 하고, 여는 값은 Postgres 면
// 다듬은 것 · SQLite 면 받은 그대로인지 잰다. 공백 붙은 접속 URL 이 SQLite 로 갈리면 파일 API 에 닿아 원문이 오류에
// 실리고, SQLite 이름의 공백을 빼면 다른 파일을 연다.
func TestParseTargetClassifiesTrimmedButOpensFilesAsGiven(t *testing.T) {
	for _, c := range []struct {
		in   string
		eng  engine
		open string
	}{
		{"postgres://fd@db:5432/fd", enginePG, "postgres://fd@db:5432/fd"},
		{"postgresql://fd@db/fd", enginePG, "postgresql://fd@db/fd"},
		{"POSTGRES://fd@db/fd", enginePG, "postgres://fd@db/fd"},
		{"  postgres://fd:pw@db/fd\n", enginePG, "postgres://fd:pw@db/fd"},
		{"\tPostgreSQL://fd@db/fd ", enginePG, "postgresql://fd@db/fd"},
		{"/var/lib/flightdeck/fd.db", engineSQLite, "/var/lib/flightdeck/fd.db"},
		{"fd.db ", engineSQLite, "fd.db "},
		{" fd.db", engineSQLite, " fd.db"},
		{"file:fd.db?mode=ro", engineSQLite, "file:fd.db?mode=ro"},
		{"./postgres://-looking-file.db", engineSQLite, "./postgres://-looking-file.db"},
	} {
		got := ParseTarget(c.in)
		if got.eng != c.eng || got.open != c.open {
			t.Errorf("ParseTarget(%q) = (%s, %q), 기대 (%s, %q)", c.in, got.eng, got.open, c.eng, c.open)
		}
		if got.Postgres() != (c.eng == enginePG) {
			t.Errorf("ParseTarget(%q).Postgres() 가 엔진과 다르다", c.in)
		}
	}
}

// TestPostgresShownCarriesNoURLPiece 는 Postgres 원장의 표시값이 어떤 접속 문자열에서도 고정 문자열 하나인지 잰다.
//
// ★ 앞 판은 net/url 과 pgx 를 맞대 좌표(사용자 · 호스트 · 포트 · DB 이름)를 골라 실었고, 인코딩 안 한 `/ ? @` 가 든
// 비밀번호에서 세 번 연달아 틈이 났다. 그래서 「허용한 조각만」이 아니라 「조각이 하나도 없다」를 단정한다.
// 아래 입력은 그 틈들을 냈던 꼴과, pgx 해석이 파일을 읽게 하는 인자(passfile · sslkey)를 든 것이다.
func TestPostgresShownCarriesNoURLPiece(t *testing.T) {
	inputs := []string{
		"postgres://fd:hunter2@db:5432/fd?sslmode=disable",
		"postgres://fd@db:5432/fd?password=hunter2&sslpassword=hunter2&application_name=x",
		"POSTGRESQL://fd:hunter2@db/fd",
		"  postgres://fd:hunter2@db/fd ",
		"postgres:///fd?host=/run/pg&port=5433&user=fd&password=hunter2&x=hunter2",
		"postgres://fd:hunter2@db/fd?sslkey=k",
		"postgres://fd:hunter2@db/fd?host=other",
		"postgres://fd:%zz@db/fd",
		"postgres://fd:12/hunter2@db/fd",
		"postgres://fd:hun/ter2@db/fd",
		"postgres://fd:8642097531?hunter2@db/fd",
		"postgres://fd:8642097531@hunter2@db/fd",
		"postgres://fd:8642097531?x/hun@ter2@db/fd",
		"postgres://fd:8642097531@x/hunter2@db/fd",
		"postgres://fd:1/hunter2@db/fd?application_name=x@y",
		"postgres://fd@db/fd?passfile=/dev/stdin&sslcert=/dev/stdin&sslkey=/dev/stdin",
		"postgres://",
	}
	for _, in := range inputs {
		tg := ParseTarget(in)
		if !tg.Postgres() {
			t.Fatalf("시험 전제가 깨졌다: %q 를 Postgres 로 가르지 않았다", in)
		}
		if got := tg.Shown(); got != "postgres" {
			t.Errorf("ParseTarget(%q).Shown() = %q - 접속 문자열의 조각 없이 \"postgres\" 여야 한다", in, got)
		}
		if got := fmt.Sprint(tg); got != "postgres" {
			t.Errorf("Target 을 그대로 찍으면 %q 다(%q) - 표시값이어야 한다", got, in)
		}
	}
}

// TestShowSeparatesFilesFromURLs 는 파일 경로는 받은 그대로, 접속 URL 은 고정 표시값으로 내는지 잰다.
func TestShowSeparatesFilesFromURLs(t *testing.T) {
	for in, want := range map[string]string{
		"/data/fd.db": "/data/fd.db",
		"fd.db ":      "fd.db ",
		"postgres://fd:hunter2@db:5432/fd?sslpassword=hunter2": "postgres",
	} {
		if got := ParseTarget(in).Shown(); got != want {
			t.Errorf("ParseTarget(%q).Shown() = %q, 기대 %q", in, got, want)
		}
	}
}

// TestPostgresURLsNeverReachFileAPIs 는 원장 길이 접속 URL 을 파일 API 에 넘기지 않는지 잰다. 넘기면 「파일이 없다」
// 오류에 원문(비밀번호)이 실리고, 파일 API 가 그 이름의 파일을 만든다. 앞에 공백이 붙은 URL 도 같은 길이어야 한다
// (분류는 다듬은 값으로 한다).
//   - SQLite 파일만 다루는 길(OpenLedger)은 표시값으로 거절한다.
//   - Postgres 를 다루는 길(판 재기 · 판 적용 · 되돌리기)은 Postgres 로 가서 실패한다(닿지 않는 포트 · 덤프 자리 없음).
//     그 오류에도 원문 조각이 없다.
func TestPostgresURLsNeverReachFileAPIs(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, dsn := range []string{
		"postgres://fd:hunter2@127.0.0.1:1/fd?sslpassword=hunter2",
		" \tpostgres://fd:hunter2@127.0.0.1:1/fd?sslpassword=hunter2",
	} {
		ctx := context.Background()
		_, probeErr := ProbeMigration(ctx, dsn)
		_, rbErr := Rollback(ctx, dsn, "", nil)
		_, ledgerErr := OpenLedger(ctx, dsn, nil)
		for name, c := range map[string]struct {
			err  error
			want string
		}{
			"ProbeMigration": {probeErr, "Postgres 접속 실패(postgres)"},
			"MigrateTo":      {MigrateTo(ctx, dsn, SchemaVersion, nil), "Postgres 접속 실패(postgres)"},
			"Rollback":       {rbErr, "덤프 자리(--backup-dir)가 없다"},
			"OpenLedger":     {ledgerErr, "SQLite 파일만"},
		} {
			if c.err == nil || !strings.Contains(c.err.Error(), c.want) {
				t.Errorf("%s 가 접속 URL(%q)을 %q 로 안 막았다: %v", name, dsn[:3], c.want, c.err)
				continue
			}
			if strings.Contains(c.err.Error(), "hunter2") {
				t.Errorf("%s 의 거절에 비밀이 샜다: %v", name, c.err)
			}
		}
	}
	if left, err := os.ReadDir("."); err != nil || len(left) != 0 {
		t.Fatalf("접속 URL 이 파일 API 에 닿아 무언가를 남겼다: %v %v", left, err)
	}
}

func TestPGURLLowersOnlyTheScheme(t *testing.T) {
	for in, want := range map[string]string{
		"POSTGRES://Fd:PW@Db/Fd": "postgres://Fd:PW@Db/Fd",
		"PostgreSQL://fd@db/fd":  "postgresql://fd@db/fd",
		"postgres://fd@db/fd":    "postgres://fd@db/fd",
	} {
		if got := pgURL(in); got != want {
			t.Errorf("pgURL(%q) = %q, 기대 %q", in, got, want)
		}
	}
}

// TestConnFaultPassesOnlyServerErrors 는 store 경계의 통과 목록을 잠근다(connFault 의 ★): 서버가 돌려준 SQLSTATE 오류와
// database/sql 의 표지 그 자체만 그대로 지나고, 나머지 드라이버 · 네트워크 오류는 종류만 남는다. 판정에 쓰는 표지(문맥의 끝 ·
// 버려진 트랜잭션의 커밋)는 바뀐 뒤에도 읽혀야 하고, SQLite 의 오류는 손대지 않는다.
func TestConnFaultPassesOnlyServerErrors(t *testing.T) {
	const host, port = "10.1.2.3", "6543"
	netErr := &net.OpError{Op: "read", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP(host), Port: 6543}, Err: syscall.ECONNRESET}
	pgErr := &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: "duplicate key"}
	for _, c := range []struct {
		name string
		err  error
		pass bool
	}{
		{"서버 오류", pgErr, true},
		{"드라이버가 감싼 서버 오류", fmt.Errorf("prepare failed: %w", pgErr), true},
		{"빈 결과 표지", sql.ErrNoRows, true},
		{"끝난 트랜잭션 표지", sql.ErrTxDone, true},
		{"문맥의 끝", context.Canceled, true},
		{"네트워크 오류", netErr, false},
		{"드라이버가 감싼 네트워크 오류", fmt.Errorf("failed to receive message: %w", netErr), false},
		{"드라이버가 감싼 표지", fmt.Errorf("%s:%s: %w", host, port, sql.ErrNoRows), false},
		{"끊긴 연결", driver.ErrBadConn, false},
		{"드라이버 문구", errors.New("conn closed " + host + ":" + port), false},
	} {
		got := enginePG.connFault(c.err)
		if c.pass {
			if got != c.err {
				t.Errorf("%s: 그대로 지나야 하는데 바뀌었다: %v", c.name, got)
			}
			continue
		}
		var ce *connError
		if !errors.As(got, &ce) {
			t.Errorf("%s: 종류로 옮기지 않았다: %v", c.name, got)
			continue
		}
		if msg := got.Error(); strings.Contains(msg, host) || strings.Contains(msg, port) {
			t.Errorf("%s: 옮긴 오류에 원격 주소가 남았다: %s", c.name, msg)
		}
	}

	if got := enginePG.connFault(fmt.Errorf("timeout: %w", context.DeadlineExceeded)); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("옮긴 오류에서 문맥의 끝을 못 읽는다: %v", got)
	}
	var f *TxFault
	if got := asFault(enginePG.connFault(pgx.ErrTxCommitRollback)); !errors.As(got, &f) || f.Kind != FaultAborted {
		t.Errorf("버려진 트랜잭션의 커밋이 옮긴 뒤 TxFault(aborted)로 읽히지 않는다: %v", got)
	}
	if got := engineSQLite.connFault(netErr); got != netErr {
		t.Errorf("SQLite 의 오류를 바꿨다: %v", got)
	}
}

// TestLockSpaceFollowsTheEngine 은 대기 계측의 잠금 공간 라벨이 실제로 잡는 잠금을 따르는지 잰다.
// SQLite 의 ObsTx 는 BEGIN IMMEDIATE(전역)라 obs 로 적으면 전역 대기가 obs 칸에 숨는다.
func TestLockSpaceFollowsTheEngine(t *testing.T) {
	for _, c := range []struct {
		eng  engine
		kind txKind
		want LockSpace
	}{
		{enginePG, txWrite, LockGlobal},
		{enginePG, txObs, LockObs},
		{engineSQLite, txWrite, LockGlobal},
		{engineSQLite, txObs, LockGlobal},
	} {
		if got := c.eng.lockSpace(c.kind); got != c.want {
			t.Errorf("%s 의 종류 %d 가 %q 칸이다(기대 %q)", c.eng, c.kind, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 소스 잠금 - 무엇이 어느 자리에서만 일어나는가
// ─────────────────────────────────────────────────────────────────────────────

// storeSource 는 이 패키지의 시험 아닌 Go 파일들이다(하위 패키지 제외).
type storeSource struct {
	fset  *token.FileSet
	files map[string]*ast.File // 파일 이름 → 구문 나무
}

func parseStoreSource(t *testing.T) storeSource {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	src := storeSource{fset: token.NewFileSet(), files: map[string]*ast.File{}}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(src.fset, n, nil, 0)
		if err != nil {
			t.Fatalf("%s 해석 실패: %v", n, err)
		}
		src.files[n] = f
	}
	// 빈 목록이면 아래 잠금이 다 초록이 된다 - 수를 먼저 잰다.
	if len(src.files) < 20 {
		t.Fatalf("store 의 시험 아닌 파일이 %d개뿐이다 - 이 시험의 좌표(작업 디렉토리)가 틀렸다", len(src.files))
	}
	return src
}

// site 는 소스의 한 자리(파일, 감싼 함수)다.
type site struct{ file, fn string }

func (s site) String() string { return s.file + ":" + s.fn }

// each 는 모든 함수 선언 안의 노드마다 f 를 부른다. 함수 밖(전역 선언)은 fn 이 "" 이다.
func (src storeSource) each(f func(at site, n ast.Node)) {
	names := make([]string, 0, len(src.files))
	for n := range src.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, d := range src.files[name].Decls {
			fn := ""
			if fd, ok := d.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if n != nil {
					f(site{name, fn}, n)
				}
				return true
			})
		}
	}
}

// isDBField 는 e 가 `<x>.db`(Store 의 단발 핸들)인가다.
func isDBField(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "db"
}

// TestRawHandlesStayInEngine 은 날 database/sql 핸들이 engine.go 밖에서 안 쓰이는지 잠근다.
//
// ★ 이것이 「모든 실행 자리가 자리표시 감싸개를 탄다」의 근거다. store 의 질의는 s.db(conn) · t.tx(txConn) ·
// ReadTx 의 Reader 로만 나가고, 그 셋의 메서드가 감싸개다. 날 *sql.DB · *sql.Tx 에 손이 닿는 자리가 생기면
// 그 자리의 `?` 는 Postgres 에서 그대로 나간다. 남은 예외는 SQLite 전용 길이라 Postgres 로 옮긴 뒤 빠진다.
func TestRawHandlesStayInEngine(t *testing.T) {
	src := parseStoreSource(t)
	allowed := map[site]string{
		{"store.go", "DB"}:                "저수준 접근자 - 시험 전용(store 밖 소스는 TestNoRawDatabaseOutsideStore 가 막는다)",
		{"store.go", "openRaw"}:           "SQLite 파일 열기",
		{"probe.go", "probeMigrationVia"}: "SQLite 파일의 판 재기",
		{"backup.go", "openSQLiteReader"}: "SQLite 파일 읽기 전용 열기(원장 내보내기 · 이관 원본)",
	}
	raw := map[string]bool{"DB": true, "Tx": true, "Conn": true, "Stmt": true, "Open": true, "OpenDB": true, "TxOptions": true}
	var bad []string
	seen := map[site]bool{}
	src.each(func(at site, n ast.Node) {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return
		}
		isRawType := false
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "sql" && raw[sel.Sel.Name] {
			isRawType = true
		}
		if !isRawType && sel.Sel.Name != "raw" {
			return
		}
		if at.file == "engine.go" {
			return
		}
		if _, ok := allowed[at]; ok {
			seen[at] = true
			return
		}
		bad = append(bad, at.String()+" 의 "+src.fset.Position(sel.Pos()).String())
	})
	if len(bad) > 0 {
		t.Errorf("날 database/sql 핸들이 engine.go 밖에서 쓰였다 - 그 자리는 자리표시 감싸개를 안 지난다:\n  %s",
			strings.Join(bad, "\n  "))
	}
	for at, why := range allowed {
		if !seen[at] {
			t.Errorf("예외 목록이 낡았다: %s(%s)에 날 핸들이 더는 없다 - 목록에서 지워라", at, why)
		}
	}
}

// TestNoRawDatabaseOutsideStore 는 store 밖의 시험 아닌 소스가 DB 핸들을 직접 쥐지 않는지 잠근다: database/sql 을
// 가져오지 않고, 인자 없는 `.DB()` 를 부르지 않는다(Store.DB 는 *sql.DB 를 내므로 가져오기 없이도 질의할 수 있다).
//
// ★ store 밖의 질의는 자리표시 감싸개도, 엔진별 질의 조각도, 오류 번역도 안 지난다 - Postgres 에서 `?` 질의가 그대로
// 거절되고, 같은 질의가 store 와 두 벌이 된다. 읽을 것이 필요하면 store 에 접근자를 둔다.
func TestNoRawDatabaseOutsideStore(t *testing.T) {
	root, err := filepath.Abs("../..") // server 모듈 뿌리
	if err != nil {
		t.Fatal(err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files, bad []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// store 와 그 아래(pg · storetest)는 이 경계의 안쪽이다.
			if path == self || strings.HasPrefix(path, self+string(filepath.Separator)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files = append(files, rel)
		for _, im := range f.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); p == "database/sql" {
				bad = append(bad, rel+": database/sql 을 가져온다")
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 0 {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "DB" {
					bad = append(bad, fset.Position(call.Pos()).String()+": .DB() 를 부른다")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 빈 목록이면 아래가 초록이 된다 - 훑은 수를 먼저 잰다(web · api · service · cmd 가 다 들어야 한다).
	for _, want := range []string{"internal/web/", "internal/api/", "internal/service/", "cmd/fd/"} {
		if !slices.ContainsFunc(files, func(f string) bool { return strings.HasPrefix(filepath.ToSlash(f), want) }) {
			t.Fatalf("%s 의 소스를 하나도 못 훑었다(훑은 파일 %d개) - 이 시험의 좌표(작업 디렉토리)가 틀렸다", want, len(files))
		}
	}
	if len(bad) > 0 {
		t.Errorf("store 밖 소스가 DB 핸들을 직접 쥔다 - 질의는 store 의 접근자로 옮겨라:\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestTxKindsAreFour 는 트랜잭션을 여는 자리를 잠근다: BeginTx 는 begin 한 곳, begin 을 부르는 것은
// runTx(Tx · ObsTx) · ReadTx · sessionTx(증분 적용 · SQLite 이관) · exportSnapshot(판올림 전 덤프의 스냅숏) 넷뿐이다.
//
// ★ 직렬성 규칙(격리 수준 · 전역 잠금)은 begin 에만 산다. 다른 자리에서 트랜잭션을 열면 그 트랜잭션은
// 잠금도 격리 수준도 없이 돈다 - Postgres 에서 조용히 깨지는 바로 그 모양이다.
func TestTxKindsAreFour(t *testing.T) {
	src := parseStoreSource(t)
	calls := map[string]map[site]bool{"BeginTx": {}, "begin": {}, "runTx": {}}
	src.each(func(at site, n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return
		}
		if m, ok := calls[sel.Sel.Name]; ok {
			m[at] = true
		}
	})
	for name, want := range map[string][]site{
		"BeginTx": {{"engine.go", "begin"}},
		"begin":   {{"pgdump.go", "exportSnapshot"}, {"store.go", "ReadTx"}, {"store.go", "runTx"}, {"store.go", "sessionTx"}},
		"runTx":   {{"store.go", "ObsTx"}, {"store.go", "Tx"}},
	} {
		var got []site
		for at := range calls[name] {
			got = append(got, at)
		}
		sort.Slice(got, func(i, j int) bool { return got[i].String() < got[j].String() })
		if !slices.Equal(got, want) {
			t.Errorf("%s 를 부르는 자리가 다르다\n 얻음 %v\n 기대 %v", name, got, want)
		}
	}
}

// TestWritesOutsideTxAreListed 는 트랜잭션 밖 쓰기(s.db 로 곧장 쓰기)를 목록으로 잠근다.
//
// ★ 트랜잭션 밖 쓰기는 전역 잠금을 안 잡는다. 이벤트 추가가 여기 있으면 event.id 순서가 커밋 순서와
// 어긋난다(Postgres) - 그래서 이벤트 추가는 목록에 없고 자기 Tx 로 간다(event.go 의 TryLogEvent).
// 쓰기를 숨기는 길 셋을 다 본다: s.db.Exec*, s.db.Query* 로 INSERT/UPDATE/DELETE, 쓰는 도우미에 s.db 넘기기.
func TestWritesOutsideTxAreListed(t *testing.T) {
	src := parseStoreSource(t)
	allowed := map[site]string{
		{"store.go", "applyBase"}: "SQLite schema.sql 적용 - PRAGMA 가 있어 트랜잭션 밖이어야 한다",
		{"store.go", "backup"}:    "SQLite 판올림 전 VACUUM INTO",
	}

	// 쓰는 도우미에 s.db 를 넘기는 길. 지금 dbtx 도우미는 다 읽기라 실제 소스에선 0건이 정상이다 -
	// 그래서 탐지 자체를 합성 소스로 따로 잰다(빈 탐지가 초록으로 숨지 않게).
	writers := dbtxWriters(src.files)
	probe, err := parser.ParseFile(token.NewFileSet(), "probe.go", `package p
func put(ctx context.Context, q dbtx, id string) error { _, err := q.ExecContext(ctx, "x", id); return err }
func get(ctx context.Context, a int, q dbtx) error { return q.QueryRowContext(ctx, "x").Scan() }`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := dbtxWriters(map[string]*ast.File{"probe.go": probe}); len(got) != 1 || got["put"] != 1 {
		t.Fatalf("쓰는 도우미 탐지가 죽었다: 합성 소스에서 %v(기대 put 의 1번 자리)", got)
	}

	seen := map[site]bool{}
	var bad []string
	src.each(func(at site, n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		pos := src.fset.Position(call.Pos()).String()
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isDBField(sel.X) {
			switch sel.Sel.Name {
			case "ExecContext", "Exec":
				if _, ok := allowed[at]; ok {
					seen[at] = true
					return
				}
				bad = append(bad, at.String()+" 가 s.db 로 곧장 쓴다("+pos+")")
			case "QueryContext", "QueryRowContext", "Query", "QueryRow":
				if q := leadingSQL(call); looksLikeWrite(q) {
					bad = append(bad, at.String()+" 가 s.db 의 질의로 쓴다("+pos+"): "+clip(q, 60))
				}
			}
			return
		}
		if fn, ok := call.Fun.(*ast.Ident); ok {
			if i, ok := writers[fn.Name]; ok && i < len(call.Args) && isDBField(call.Args[i]) {
				bad = append(bad, at.String()+" 가 쓰는 도우미 "+fn.Name+" 에 s.db 를 넘긴다("+pos+")")
			}
		}
	})
	if len(bad) > 0 {
		t.Errorf("트랜잭션 밖 쓰기가 목록 밖에 있다 - 전역 잠금을 안 잡는다:\n  %s", strings.Join(bad, "\n  "))
	}
	for at, why := range allowed {
		if !seen[at] {
			t.Errorf("목록이 낡았다: %s(%s)에 트랜잭션 밖 쓰기가 더는 없다 - 목록에서 지워라", at, why)
		}
	}
}

// dbtxWriters 는 dbtx 매개변수로 ExecContext 하는 함수 이름 → 그 매개변수 자리다.
func dbtxWriters(files map[string]*ast.File) map[string]int {
	out := map[string]int{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Body == nil {
				continue
			}
			idx := 0
			for _, p := range fd.Type.Params.List {
				typ, _ := p.Type.(*ast.Ident)
				names := len(p.Names)
				for _, name := range p.Names {
					if typ != nil && typ.Name == "dbtx" && execsOn(fd.Body, name.Name) {
						out[fd.Name.Name] = idx
					}
					idx++
				}
				if names == 0 {
					idx++
				}
			}
		}
	}
	return out
}

// execsOn 은 body 안에 `<name>.ExecContext(` 호출이 있는가다.
func execsOn(body ast.Node, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ExecContext" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == name {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// leadingSQL 은 질의 호출의 SQL 인자가 문자열 글자(또는 글자로 시작하는 이어 붙이기)면 그 글자를 낸다.
func leadingSQL(call *ast.CallExpr) string {
	for _, a := range call.Args {
		for {
			b, ok := a.(*ast.BinaryExpr)
			if !ok {
				break
			}
			a = b.X
		}
		if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			s, err := strconv.Unquote(lit.Value)
			if err == nil {
				return s
			}
		}
	}
	return ""
}

func looksLikeWrite(q string) bool {
	u := strings.ToUpper(strings.TrimSpace(q))
	for _, w := range []string{"INSERT", "UPDATE", "DELETE", "WITH"} {
		if strings.HasPrefix(u, w) {
			return w != "WITH" || strings.Contains(u, "INSERT") || strings.Contains(u, "UPDATE") || strings.Contains(u, "DELETE")
		}
	}
	return strings.Contains(u, "RETURNING")
}

// TestStoreSQLHasNoJSONBQuestionOperators 는 jsonb 의 `?` · `?|` · `?&` 연산자가 store 의 SQL 에 없는지 잠근다.
//
// ★ 감싸개는 따옴표 밖의 `?` 를 전부 자리표시로 본다(bindDollar). jsonb 연산자는 자리표시와 글자가 같아
// 실행 때 못 가른다 - 그래서 소스에서 막는다. 자리표시 바로 뒤에 `|` · `&` 가 붙거나, 다음 글자가 문자열
// 따옴표나 또 다른 `?` 면(값 둘이 연산자 없이 붙을 수 없으니) 연산자 자리다. 키를 물을 일이 생기면
// `jsonb_exists(…)` 처럼 함수로 쓴다.
//
// ★ 훑는 곳은 둘이다: Go 소스의 문자열 조각과 store 의 .sql 파일(SQLite 스키마 · 증분, Postgres 기준선 · 준비).
// .sql 은 인자 없이 돌므로 따옴표 밖 `?` 는 연산자 꼴이든 아니든 하나도 있으면 안 된다(bindDollar 의 ★ 인자 수).
//
// ★ 한계: 문자열 조각을 하나씩 보므로 실행 중에 이어 붙여 만드는 SQL(`"payload ?" + col` 처럼 연산자와 피연산자가
// 다른 조각에 있는 것)은 못 본다.
func TestStoreSQLHasNoJSONBQuestionOperators(t *testing.T) {
	src := parseStoreSource(t)
	marks := 0
	var bad []string
	src.each(func(at site, n ast.Node) {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return
		}
		pos, _ := placeholders(s)
		for _, p := range pos {
			marks++
			if reason := questionOperatorAt(s, p); reason != "" {
				bad = append(bad, at.String()+" "+src.fset.Position(lit.Pos()).String()+": "+reason+" - "+clip(s, 80))
			}
		}
	})

	var sqlFiles []string
	for _, g := range []string{"*.sql", "migrations/*.sql", "pg/*.sql"} {
		m, err := filepath.Glob(g)
		if err != nil {
			t.Fatal(err)
		}
		sqlFiles = append(sqlFiles, m...)
	}
	// SQLite 증분 18 + SQLite 스키마 1 + Postgres 셋(준비 · 기준선 · 증분 019). 덜 세면 아래 단정이 그냥 초록이다.
	if len(sqlFiles) < 22 || !slices.Contains(sqlFiles, filepath.Join("pg", "001_baseline.sql")) {
		t.Fatalf("store 의 .sql 파일을 %d개만 찾았다(pg 기준선 포함 22개 이상이어야 한다): %v", len(sqlFiles), sqlFiles)
	}
	for _, f := range sqlFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		q := string(b)
		pos, _ := placeholders(q)
		for _, p := range pos {
			reason := questionOperatorAt(q, p)
			if reason == "" {
				reason = "인자 없이 도는 파일의 따옴표 밖 ?"
			}
			line := 1 + strings.Count(q[:p], "\n")
			bad = append(bad, f+":"+strconv.Itoa(line)+": "+reason)
		}
	}
	// 자리표시를 하나도 못 세면 아래 단정이 그냥 초록이다. 지금 store 의 SQL 은 자리표시가 수백 개다.
	if marks < 200 {
		t.Fatalf("store 의 SQL 에서 자리표시를 %d개만 셌다 - 이 시험의 탐지가 죽었다", marks)
	}
	if len(bad) > 0 {
		t.Errorf("jsonb 의 ? 연산자로 보이는 자리가 있다 - 감싸개가 자리표시로 바꿔 버린다:\n  %s", strings.Join(bad, "\n  "))
	}
	// 탐지 자체도 잰다(위가 늘 초록이면 이것이 빨개야 한다).
	for _, q := range []string{`payload::jsonb ?| array['a']`, `payload::jsonb ?& ?`, `payload::jsonb ? 'item'`, `payload::jsonb ? ?`} {
		hit := false
		pos, _ := placeholders(q)
		for _, p := range pos {
			if questionOperatorAt(q, p) != "" {
				hit = true
			}
		}
		if !hit {
			t.Errorf("탐지가 jsonb 연산자를 못 잡는다: %q", q)
		}
	}
	for _, q := range []string{`WHERE a = ? AND b IN (?, ?)`, `VALUES (?, ?)`, `(payload::jsonb ->> 'item') = ?`} {
		pos, _ := placeholders(q)
		for _, p := range pos {
			if r := questionOperatorAt(q, p); r != "" {
				t.Errorf("탐지가 자리표시를 연산자로 잘못 본다: %q (%s)", q, r)
			}
		}
	}
}

// questionOperatorAt 은 q[p] 의 `?` 가 jsonb 연산자 자리로 보이면 그 사유를 낸다.
func questionOperatorAt(q string, p int) string {
	if p+1 < len(q) && (q[p+1] == '|' || q[p+1] == '&') {
		return "?" + string(q[p+1]) + " 연산자"
	}
	rest := strings.TrimLeft(q[p+1:], " \t\r\n")
	if rest != "" && (rest[0] == '\'' || rest[0] == '?') {
		return "? 다음에 값이 곧장 온다(키 존재 연산자)"
	}
	return ""
}
