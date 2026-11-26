package storetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 도우미 자체를 잠근다: 독립 DB · 원판 내용 · 병렬 · 정리 · 프로세스 사이 원판 경합 · 내용이 바뀌면 새 원판 ·
// 만들다 멈춘 원판 · 원판 접속 거절 · 로케일 · FD_TEST_PG 없으면 실패.

// childEnv 는 이 시험 바이너리를 자식 프로세스로 다시 띄울 때 자식이 맡을 역할이다.
const childEnv = "STORETEST_CHILD"

func adminDB(t *testing.T) *sql.DB {
	t.Helper()
	_, admin, err := server()
	if err != nil {
		t.Fatal(err)
	}
	return admin
}

func open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func exec1(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func currentDB(t *testing.T, db *sql.DB) string {
	t.Helper()
	var name string
	if err := db.QueryRow("SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

// seedSQL 은 원판에 표 하나와 행 하나(v)를 넣는 내용이다.
func seedSQL(v string) []byte {
	return []byte("CREATE TABLE seed (v text); INSERT INTO seed VALUES ('" + v + "')")
}

// counted 는 내용을 SQL 로 그대로 넣되 불릴 때마다(원판을 실제로 만들 때마다) builds 를 센다.
func counted(builds *atomic.Int32) func(context.Context, *sql.DB, []byte) error {
	return func(ctx context.Context, db *sql.DB, content []byte) error {
		builds.Add(1)
		_, err := db.ExecContext(ctx, string(content))
		return err
	}
}

// freshTemplate 은 이 시험만의 원판을 둔다(접두가 난수라 다른 시험 · 앞선 실행과 안 겹친다).
// 시험이 끝나면 원판도 지운다.
func freshTemplate(t *testing.T, s schema) *template {
	t.Helper()
	tp := &template{prefix: "fd_tmpl_t" + strings.ToLower(rand.Text()[:8]), schema: s}
	cleanupTemplate(t, tp)
	return tp
}

func cleanupTemplate(t *testing.T, tp *template) {
	t.Helper()
	admin := adminDB(t)
	name, _ := tp.names()
	t.Cleanup(func() {
		var n int
		if err := admin.QueryRow("SELECT count(*) FROM pg_database WHERE datname = $1", name).Scan(&n); err != nil {
			t.Errorf("원판 %s 조회: %v", name, err)
			return
		}
		if n == 1 {
			if err := dropTemplate(context.Background(), admin, name); err != nil {
				t.Errorf("원판 %s 지우기: %v", name, err)
			}
		}
	})
}

func seedRows(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT v FROM seed ORDER BY v")
	if err != nil {
		t.Fatalf("원판의 seed 표가 복제본에 없다: %v", err)
	}
	defer rows.Close()
	var vs []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		vs = append(vs, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return vs
}

func TestNewGivesIsolatedDatabases(t *testing.T) {
	a, b := open(t, New(t)), open(t, New(t))
	na, nb := currentDB(t, a), currentDB(t, b)
	if na == nb || !strings.HasPrefix(na, "t_") || !strings.HasPrefix(nb, "t_") {
		t.Fatalf("시험 DB 둘이 따로가 아니다: %q, %q", na, nb)
	}
	exec1(t, a, "CREATE TABLE only_a (v int)")
	var reg sql.NullString
	if err := b.QueryRow("SELECT to_regclass('only_a')::text").Scan(&reg); err != nil {
		t.Fatal(err)
	}
	if reg.Valid {
		t.Fatalf("한 시험 DB 에 만든 표가 다른 시험 DB 에 보인다: %s", reg.String)
	}
}

// 원판에 넣은 내용이 복제본마다 오고, 한 복제본에 쓴 것은 원판에도 다른 복제본에도 안 간다.
func TestNewClonesTemplateContents(t *testing.T) {
	var builds atomic.Int32
	tp := freshTemplate(t, schema{content: seedSQL("from-template"), apply: counted(&builds)})
	a := open(t, tp.clone(t))
	if got := seedRows(t, a); len(got) != 1 || got[0] != "from-template" {
		t.Fatalf("복제본 seed = %v, 원판 내용 [from-template] 이어야 한다", got)
	}
	exec1(t, a, "INSERT INTO seed VALUES ('only-a')")
	b := open(t, tp.clone(t))
	if got := seedRows(t, b); len(got) != 1 {
		t.Fatalf("다른 복제본에 쓴 행이 새 복제본에 왔다: %v", got)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("원판을 %d 번 만들었다, 프로세스마다 한 번이어야 한다", n)
	}
}

// 병렬 시험이 한꺼번에 원판을 처음 부른다: 원판은 한 번만 만들어지고, DB 는 서로 다르며, 시험이 끝나면
// 다 지워진다.
func TestNewIsParallelSafeAndCleansUp(t *testing.T) {
	const n = 16
	var builds atomic.Int32
	tp := freshTemplate(t, schema{content: seedSQL("from-template"), apply: counted(&builds)})
	var mu sync.Mutex
	names := map[string]bool{}
	t.Run("group", func(t *testing.T) {
		for i := range n {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				db := open(t, tp.clone(t))
				if got := seedRows(t, db); len(got) != 1 {
					t.Errorf("복제본 seed = %v", got)
				}
				name := currentDB(t, db)
				mu.Lock()
				names[name] = true
				mu.Unlock()
			})
		}
	})
	if len(names) != n {
		t.Fatalf("병렬 시험 %d 개가 받은 DB 가 %d 개다 — 서로 달라야 한다", n, len(names))
	}
	if b := builds.Load(); b != 1 {
		t.Fatalf("원판을 %d 번 만들었다, 한 번이어야 한다", b)
	}
	list := make([]string, 0, n)
	for name := range names {
		list = append(list, name)
	}
	var left int
	if err := adminDB(t).QueryRow("SELECT count(*) FROM pg_database WHERE datname = ANY($1)", list).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("끝난 시험의 DB %d 개가 안 지워졌다", left)
	}
}

// builtMark 는 자식 프로세스가 원판을 실제로 만들 때 찍는 줄이다. 부모가 센다.
const builtMark = "STORETEST-TEMPLATE-BUILT"

// raceContent 는 경합 시험의 원판 내용이다. 부모와 자식이 같은 원판 이름을 내려면 같아야 한다.
var raceContent = seedSQL("from-template")

// 프로세스 여럿이 같은 원판을 동시에 처음 부른다(`go test ./...` 의 패키지들이 그렇다): 모두 성공하고
// 원판은 한 번만 만들어진다.
func TestTemplateBuildRaceAcrossProcesses(t *testing.T) {
	if os.Getenv(childEnv) == "race" {
		tp := &template{prefix: os.Getenv("STORETEST_PREFIX"), schema: schema{content: raceContent,
			apply: func(ctx context.Context, db *sql.DB, content []byte) error {
				fmt.Println(builtMark)
				// 다른 프로세스들이 잠금 앞에 와 설 틈을 벌린다.
				time.Sleep(300 * time.Millisecond)
				_, err := db.ExecContext(ctx, string(content))
				return err
			}}}
		if got := seedRows(t, open(t, tp.clone(t))); len(got) != 1 {
			t.Fatalf("복제본 seed = %v", got)
		}
		return
	}
	adminDB(t) // 부모도 시험 서버가 있어야 한다
	const n = 4
	prefix := "fd_tmpl_race" + strings.ToLower(rand.Text()[:8])
	cleanupTemplate(t, &template{prefix: prefix, schema: schema{content: raceContent}})
	outs := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestTemplateBuildRaceAcrossProcesses$", "-test.v")
			cmd.Env = append(os.Environ(), childEnv+"=race", "STORETEST_PREFIX="+prefix)
			outs[i], errs[i] = cmd.CombinedOutput()
		})
	}
	wg.Wait()
	built := 0
	for i := range n {
		if errs[i] != nil {
			t.Errorf("자식 %d 실패: %v\n%s", i, errs[i], outs[i])
		}
		built += strings.Count(string(outs[i]), builtMark+"\n")
	}
	if built != 1 {
		t.Fatalf("원판을 프로세스 %d 개가 합쳐 %d 번 만들었다, 한 번이어야 한다", n, built)
	}
}

// 내용이 바뀌면 원판 이름이 바뀐다. 그래서 옛 원판이 남아 있는 서버(오래 띄워 둔 시험 서버, 스키마가
// 다른 체크아웃)에서도 새 내용이 복제되고, 판을 손으로 올릴 일이 없다.
func TestChangedContentGetsNewTemplate(t *testing.T) {
	prefix := "fd_tmpl_c" + strings.ToLower(rand.Text()[:8])
	old := &template{prefix: prefix, schema: schema{content: seedSQL("old")}}
	cleanupTemplate(t, old)
	if got := seedRows(t, open(t, old.clone(t))); len(got) != 1 || got[0] != "old" {
		t.Fatalf("옛 원판 복제본 seed = %v", got)
	}
	cur := &template{prefix: prefix, schema: schema{content: seedSQL("new")}}
	cleanupTemplate(t, cur)
	on, _ := old.names()
	cn, _ := cur.names()
	if on == cn {
		t.Fatalf("내용이 다른 두 원판의 이름이 같다(%s) — 옛 원판이 새 스키마 자리에 복제된다", on)
	}
	if got := seedRows(t, open(t, cur.clone(t))); len(got) != 1 || got[0] != "new" {
		t.Fatalf("내용을 바꾼 원판의 복제본 seed = %v, [new] 이어야 한다", got)
	}
}

// 만들다 멈춘 원판(이름은 있고 원판 표시까지 갔으나 완성 표지가 없다)은 지우고 다시 만든다.
func TestUnfinishedTemplateIsRebuilt(t *testing.T) {
	var builds atomic.Int32
	tp := freshTemplate(t, schema{content: seedSQL("from-template"), apply: counted(&builds)})
	name, _ := tp.names()
	admin := adminDB(t)
	exec1(t, admin, "CREATE DATABASE "+ident(name)+" TEMPLATE template0")
	exec1(t, admin, "ALTER DATABASE "+ident(name)+" IS_TEMPLATE true")
	if got := seedRows(t, open(t, tp.clone(t))); len(got) != 1 {
		t.Fatalf("복제본 seed = %v", got)
	}
	if b := builds.Load(); b != 1 {
		t.Fatalf("만들다 멈춘 원판을 다시 만들지 않았다(만든 횟수 %d)", b)
	}
}

// 다 만든 원판은 접속을 거절한다. 원판에 연결이 남아 있으면 PG 가 복제를 거절하는데(55006), 이 거절로
// 그 자리 자체가 없어진다.
func TestTemplateRefusesConnections(t *testing.T) {
	tp := freshTemplate(t, schema{})
	tp.clone(t)
	name, _ := tp.names()
	base, _, err := server()
	if err != nil {
		t.Fatal(err)
	}
	err = open(t, dbURL(base, name)).Ping()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55000" {
		t.Fatalf("원판 %s 에 접속이 거절되지 않았다(55000 이어야 한다): %v", name, err)
	}
}

// 시험 서버의 로케일은 두 축 다 compose.test.yaml 이 못박은 값이어야 한다.
//   - 정렬: 기본 정렬 규칙이 builtin 제공자의 C.UTF-8(datlocale), 곧 코드포인트 순.
//   - 글자 가르기: PG17 의 pg_trgm 은 제공자가 아니라 libc LC_CTYPE(datctype)으로 글자를 가른다. 그것이
//     `C` 면 한글이 글자가 아니게 되어 show_trgm 이 0개, 한글 검색이 조용히 0건이다. initdb 는 --lc-* 를
//     안 받으면 이미지의 LANG 을 가져오므로, 그 값이 우연히 맞아 초록인 것을 막으려고 datctype 까지 본다.
func TestServerLocale(t *testing.T) {
	db := open(t, New(t))
	var provider, locale, ctype, collate string
	if err := db.QueryRow(`SELECT datlocprovider::text, coalesce(datlocale, ''), datctype, datcollate
		FROM pg_database WHERE datname = current_database()`).Scan(&provider, &locale, &ctype, &collate); err != nil {
		t.Fatal(err)
	}
	if provider != "b" || locale != "C.UTF-8" || ctype != "C.UTF-8" || collate != "C.UTF-8" {
		t.Errorf("시험 DB 로케일이 제공자 %s · 기본 %q · LC_CTYPE %q · LC_COLLATE %q 다, "+
			"builtin C.UTF-8 에 LC_CTYPE · LC_COLLATE 도 C.UTF-8 이어야 한다 — compose.test.yaml 의 POSTGRES_INITDB_ARGS",
			provider, locale, ctype, collate)
	}
	exec1(t, db, "CREATE EXTENSION IF NOT EXISTS pg_trgm")
	var n int
	var trgm string
	if err := db.QueryRow("SELECT cardinality(show_trgm('판단을')), show_trgm('판단을')::text").Scan(&n, &trgm); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatalf("show_trgm('판단을') = %s, 비었다 — 이 로케일에서는 한글 검색이 0건이 된다", trgm)
	}
}

// FD_TEST_PG 가 없으면 New 는 시험을 **실패**시킨다. 건너뛰면 아무것도 안 돈 패키지가 초록으로 남는다.
// 자식 프로세스로 돌려 실제 go test 의 판정(종료 코드 · FAIL/SKIP 줄)을 본다.
func TestNewFailsWithoutEnv(t *testing.T) {
	if os.Getenv(childEnv) == "noenv" {
		New(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNewFailsWithoutEnv$", "-test.v")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, EnvDSN+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, childEnv+"=noenv")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("%s 없이 돈 시험이 실패하지 않았다(%v):\n%s", EnvDSN, err, out)
	}
	s := string(out)
	if !strings.Contains(s, "--- FAIL: TestNewFailsWithoutEnv") || strings.Contains(s, "--- SKIP") ||
		!strings.Contains(s, EnvDSN+" 가 비었다") {
		t.Fatalf("%s 없이 돈 시험이 그 까닭으로 실패해야 한다(건너뛰기 아님):\n%s", EnvDSN, s)
	}
}

func TestAdminURLRejectsNonURL(t *testing.T) {
	for _, raw := range []string{"host=127.0.0.1 dbname=postgres", "mysql://x@h/db", "postgres:///postgres"} {
		if _, err := adminURL(raw); err == nil {
			t.Errorf("adminURL(%q) 가 받아들였다, URL 꼴만 받아야 한다", raw)
		}
	}
	u, err := adminURL("postgres://postgres@127.0.0.1:5999/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dbURL(u, "t_x"), "postgres://postgres@127.0.0.1:5999/t_x?sslmode=disable"; got != want {
		t.Fatalf("dbURL = %q, want %q", got, want)
	}
}
