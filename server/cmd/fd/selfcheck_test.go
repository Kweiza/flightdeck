package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// sqlite 드라이버는 internal/store 가 blank import 로 등록한다(cmd/fd 가 그것을 쓴다).
// 여기서 다시 import 하지 않는다.
func selfcheckDB(t *testing.T, sqls ...string) string {
	t.Helper()
	return selfcheckDBAt(t, filepath.Join(t.TempDir(), "fd.db"), sqls...)
}

func selfcheckDBAt(t *testing.T, path string, sqls ...string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("열기 실패: %v", err)
	}
	defer db.Close()
	for _, q := range sqls {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("준비 실패(%q): %v", q, err)
		}
	}
	if len(sqls) == 0 {
		if _, err := db.Exec(`SELECT 1`); err != nil {
			t.Fatalf("빈 DB 실패: %v", err)
		}
	}
	return path
}

// migrated 는 path 의 원장을 이전 실행기로 target 판까지 올린다(0 이면 지금 판).
func migrated(t *testing.T, path string, target int) string {
	t.Helper()
	if target == 0 {
		target = store.SchemaVersion
	}
	if err := store.MigrateTo(context.Background(), path, target, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("이전 실행기: %v", err)
	}
	return path
}

func TestSelfcheckPassesOnCleanDB(t *testing.T) {
	var out bytes.Buffer
	code := runSelfcheck([]string{"--db", migrated(t, selfcheckDB(t), 0)}, noSelfcheckEnv, &out)
	if code != 0 {
		t.Fatalf("깨끗한 DB 인데 종료코드 %d — %s", code, out.String())
	}
	if !strings.HasPrefix(out.String(), "fd selfcheck ok build=") {
		t.Fatalf("계약된 첫 줄이 아니다: %q", out.String())
	}
}

// TestSelfcheckPassesOnlyWhatBootOpens 는 selfcheck 의 판정이 기동 검사와 같은지 두 엔진에서 잰다: 증분 계획이 none 일
// 때만 통과다. apply(빈 DB) · upgrade(낮은 판)를 통과로 내면 감시기가 새 판으로 넘어간 뒤 그 기동이 「fd migrate 를 먼저
// 돌려라」로 죽는다 - 기동은 증분을 적용하지 않는다.
func TestSelfcheckPassesOnlyWhatBootOpens(t *testing.T) {
	pgAt := func(t *testing.T, version int) string {
		dsn := storetest.NewBare(t, "")
		if version < 0 {
			return dsn
		}
		migrated(t, dsn, 0)
		if version != store.SchemaVersion {
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE schema_version SET version = $1`, version); err != nil {
				t.Fatal(err)
			}
		}
		return dsn
	}
	for _, c := range []struct {
		name string
		db   func(t *testing.T) string
		plan store.MigrationAction
	}{
		{"SQLite none", func(t *testing.T) string { return migrated(t, selfcheckDB(t), 0) }, store.MigrateNone},
		{"SQLite apply", func(t *testing.T) string { return selfcheckDB(t) }, store.MigrateApply},
		{"SQLite upgrade", func(t *testing.T) string { return migrated(t, selfcheckDB(t), store.SchemaVersion-1) }, store.MigrateUpgrade},
		{"Postgres none", func(t *testing.T) string { return pgAt(t, store.SchemaVersion) }, store.MigrateNone},
		{"Postgres apply", func(t *testing.T) string { return pgAt(t, -1) }, store.MigrateApply},
		{"Postgres upgrade", func(t *testing.T) string { return pgAt(t, store.SchemaVersion-1) }, store.MigrateUpgrade},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := c.db(t)
			if plan, err := store.ProbeMigration(context.Background(), path); err != nil || plan.Action != c.plan {
				t.Fatalf("준비한 원장의 계획 %+v, %v (기대 %s)", plan, err, c.plan)
			}
			env := func(k string) (string, bool) { return path, k == "FD_DB" }
			var out bytes.Buffer
			code := runSelfcheck(nil, env, &out)
			if c.plan == store.MigrateNone {
				if code != 0 || !strings.HasPrefix(out.String(), "fd selfcheck ok build=") {
					t.Fatalf("기동이 여는 원장인데 종료코드 %d - %s", code, out.String())
				}
				return
			}
			if code != 1 || !strings.Contains(out.String(), "("+string(c.plan)+")") || !strings.Contains(out.String(), "fd migrate") {
				t.Fatalf("기동이 거절할 원장(%s)인데 종료코드 %d - %s", c.plan, code, out.String())
			}
			if _, err := store.Open(path); err == nil {
				t.Fatalf("selfcheck 가 거절한 원장(%s)을 기동이 열었다 - 두 판정이 어긋난다", c.plan)
			}
		})
	}
}

// ★ 이 갈래가 이 명령의 존재 이유다.
func TestSelfcheckFailsWhenMigrationWouldBeRejected(t *testing.T) {
	path := selfcheckDB(t, `CREATE TABLE somebody_elses (a TEXT)`)
	var out bytes.Buffer
	code := runSelfcheck([]string{"--db", path}, noSelfcheckEnv, &out)
	if code == 0 {
		t.Fatalf("증분이 거절될 DB 인데 통과했다 — %s", out.String())
	}
	if !strings.Contains(out.String(), "schema_version") {
		t.Fatalf("사유가 원인을 안 나른다: %q", out.String())
	}
}

func TestSelfcheckFailsWhenDBMissing(t *testing.T) {
	var out bytes.Buffer
	if code := runSelfcheck([]string{"--db", filepath.Join(t.TempDir(), "nope.db")}, noSelfcheckEnv, &out); code == 0 {
		t.Fatalf("없는 DB 인데 통과했다 — %s", out.String())
	}
}

func TestSelfcheckRequiresDBFlag(t *testing.T) {
	var out bytes.Buffer
	if code := runSelfcheck(nil, noSelfcheckEnv, &out); code == 0 {
		t.Fatal("--db 없이 통과했다")
	}
	blank := func(k string) (string, bool) { return "  ", k == "FD_DB" }
	if code := runSelfcheck([]string{"--db", " "}, blank, &out); code != 2 {
		t.Fatalf("--db 도 FD_DB 도 공백인데 종료코드 %d", code)
	}
}

func noSelfcheckEnv(string) (string, bool) { return "", false }

// TestSelfcheckReadsFDDBWhenFlagIsEmpty 는 --db 가 비면 FD_DB 를 재는지 잰다. 감시기는 원장 자리를 늘 이 환경
// 변수로 넘기고, Postgres 접속 URL 은 argv 에 싣지 않는다(selfcheckArgs).
func TestSelfcheckReadsFDDBWhenFlagIsEmpty(t *testing.T) {
	path := migrated(t, selfcheckDB(t), 0)
	env := func(k string) (string, bool) { return path, k == "FD_DB" }
	var out bytes.Buffer
	if code := runSelfcheck(nil, env, &out); code != 0 {
		t.Fatalf("FD_DB 로 준 깨끗한 DB 인데 종료코드 %d - %s", code, out.String())
	}
}

// TestSelfcheckRefusesPostgresWithoutLeaking 은 닿지 않는 Postgres 원장을 받으면 원문을 싣지 않고 표시값과 함께
// 거절하는지 잰다. 이 출력은 감시기의 거절 사유로 로그 · 화면에 그대로 간다. 앞에 공백이 붙은 URL 도 Postgres 로
// 갈라야 한다 - SQLite 로 가르면 파일 API(os.Stat)에 닿아 「파일이 없다」 오류에 원문이 실린다.
func TestSelfcheckRefusesPostgresWithoutLeaking(t *testing.T) {
	const dsn = "postgres://fd:hunter2@127.0.0.1:1/fd?sslpassword=hunter2"
	for name, c := range map[string]struct {
		args []string
		env  string
	}{
		"FD_DB":    {nil, dsn},
		"--db":     {[]string{"--db", dsn}, ""},
		"앞 공백":     {[]string{"--db", "  " + dsn}, ""},
		"FD_DB 공백": {nil, "\t" + dsn + " "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			env := func(k string) (string, bool) { return c.env, k == "FD_DB" && c.env != "" }
			var out bytes.Buffer
			if code := runSelfcheck(c.args, env, &out); code != 1 {
				t.Fatalf("Postgres 원장인데 종료코드 %d - %s", code, out.String())
			}
			got := out.String()
			for _, piece := range []string{"hunter2", "127.0.0.1", "fd@"} {
				if strings.Contains(got, piece) {
					t.Fatalf("출력에 접속 문자열의 조각(%s)이 실렸다: %s", piece, got)
				}
			}
			if !strings.Contains(got, "읽지 못했다") || !strings.Contains(got, "접속 실패(postgres)") {
				t.Fatalf("Postgres 거절 사유를 표시값과 함께 안 냈다: %s", got)
			}
			if left, err := os.ReadDir("."); err != nil || len(left) != 0 {
				t.Fatalf("접속 URL 이 파일 API 에 닿아 무언가를 남겼다: %v %v", left, err)
			}
		})
	}
}

// TestSelfcheckKeepsFileNameAsGiven 은 SQLite 파일 경로를 다듬지 않고 받은 그대로 재는지 잰다. 이름 끝의 공백도
// 이름이다 - 다듬으면 없는 `fd.db` 를 재거나, 둘 다 있으면 다른 원장을 잰다.
func TestSelfcheckKeepsFileNameAsGiven(t *testing.T) {
	path := migrated(t, selfcheckDBAt(t, filepath.Join(t.TempDir(), "fd.db ")), 0)
	var out bytes.Buffer
	if code := runSelfcheck([]string{"--db", path}, noSelfcheckEnv, &out); code != 0 {
		t.Fatalf("이름 끝에 공백이 든 DB 를 못 쟀다(종료코드 %d) - %s", code, out.String())
	}
}

// main 의 서브명령 표에 실제로 걸렸는가 — 없으면 감시기가 부를 때 usage 만 나온다.
//
// run 의 시그니처는 main.go:55 다:
//
//	func run(args []string, env func(string) (string, bool), stdin io.Reader, stdout, stderr io.Writer) int
func TestSelfcheckIsWiredIntoMain(t *testing.T) {
	var out, errBuf bytes.Buffer
	noEnv := func(string) (string, bool) { return "", false }
	code := run([]string{"selfcheck", "--db", migrated(t, selfcheckDB(t), 0)},
		noEnv, strings.NewReader(""), &out, &errBuf)
	if code != 0 {
		t.Fatalf("main 경유 종료코드 %d — out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "fd selfcheck ok") {
		t.Fatalf("계약된 첫 줄이 stdout 으로 안 나왔다: %q", out.String())
	}
}

// TestSelfcheckPassesAMigratedPostgresLedger 는 이전 실행기가 올린 Postgres 원장을 selfcheck 가 기동과 같은 판 재기로
// 통과시키는지 잰다. 접속 URL 은 환경(FD_DB)으로만 받는다(selfcheckArgs) - 출력에 그 원문이 없어야 한다.
func TestSelfcheckPassesAMigratedPostgresLedger(t *testing.T) {
	dsn := storetest.NewBare(t, "")
	if err := store.Migrate(context.Background(), dsn, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("이전 실행기: %v", err)
	}
	env := func(k string) (string, bool) { return dsn, k == "FD_DB" }
	var out bytes.Buffer
	if code := runSelfcheck(nil, env, &out); code != 0 {
		t.Fatalf("올린 Postgres 원장인데 종료코드 %d - %s", code, out.String())
	}
	got := out.String()
	if !strings.HasPrefix(got, "fd selfcheck ok build=") || !strings.Contains(got, "증분 계획: none") {
		t.Fatalf("계약된 첫 줄 · 판정이 아니다: %s", got)
	}
	if strings.Contains(got, dsn) {
		t.Fatalf("출력에 접속 URL 원문이 실렸다: %s", got)
	}
}
