package main

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
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

func TestSelfcheckPassesOnCleanDB(t *testing.T) {
	var out bytes.Buffer
	code := runSelfcheck([]string{"--db", selfcheckDB(t)}, noSelfcheckEnv, &out)
	if code != 0 {
		t.Fatalf("깨끗한 DB 인데 종료코드 %d — %s", code, out.String())
	}
	if !strings.HasPrefix(out.String(), "fd selfcheck ok build=") {
		t.Fatalf("계약된 첫 줄이 아니다: %q", out.String())
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

// TestSelfcheckReadsFDDBWhenFlagIsEmpty 는 --db 가 비면 FD_DB 를 재는지 잰다. 감시기는 원장 자리를 argv 가 아니라
// 이 환경 변수로 넘긴다(verifyWithSelfcheck).
func TestSelfcheckReadsFDDBWhenFlagIsEmpty(t *testing.T) {
	path := selfcheckDB(t)
	env := func(k string) (string, bool) { return path, k == "FD_DB" }
	var out bytes.Buffer
	if code := runSelfcheck(nil, env, &out); code != 0 {
		t.Fatalf("FD_DB 로 준 깨끗한 DB 인데 종료코드 %d - %s", code, out.String())
	}
}

// TestSelfcheckRefusesPostgresWithoutLeaking 은 Postgres 원장을 받으면 원문을 싣지 않고 「판 재기는 아직 없다」로
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
			if strings.Contains(got, "hunter2") {
				t.Fatalf("출력에 비밀이 샜다: %s", got)
			}
			if !strings.Contains(got, "판 재기는 아직 없다") || !strings.Contains(got, "postgres://fd@127.0.0.1:1/fd") {
				t.Fatalf("Postgres 거절 사유를 표시값과 함께 안 냈다: %s", got)
			}
		})
	}
}

// TestSelfcheckKeepsFileNameAsGiven 은 SQLite 파일 경로를 다듬지 않고 받은 그대로 재는지 잰다. 이름 끝의 공백도
// 이름이다 - 다듬으면 없는 `fd.db` 를 재거나, 둘 다 있으면 다른 원장을 잰다.
func TestSelfcheckKeepsFileNameAsGiven(t *testing.T) {
	path := selfcheckDBAt(t, filepath.Join(t.TempDir(), "fd.db "))
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
	code := run([]string{"selfcheck", "--db", selfcheckDB(t)},
		noEnv, strings.NewReader(""), &out, &errBuf)
	if code != 0 {
		t.Fatalf("main 경유 종료코드 %d — out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "fd selfcheck ok") {
		t.Fatalf("계약된 첫 줄이 stdout 으로 안 나왔다: %q", out.String())
	}
}
