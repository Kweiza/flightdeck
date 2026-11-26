package store

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Postgres 판올림 전 덤프 · 되돌리기의 실행 경계 시험. DB 에 붙지 않는다 - pg_dump 자리에 가짜 실행 파일을 둔다.
// 실제 pg_dump · pg_restore 로 뜨고 되살리는 시험은 pg_migrate_test.go 의 TestPGDumpAndRestoreRehearsal 이다.

// cleanPGEnv 는 이 시험이 기대는 해석에 물려받은 PG 환경 변수가 끼지 않게 비운다(pgx 는 빈 값을 무시한다).
func cleanPGEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE",
		"PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGCHANNELBINDING", "PGREQUIREAUTH", "PGSSLNEGOTIATION",
		"PGSERVICE", "PGAPPNAME"} {
		t.Setenv(k, "")
	}
}

func TestDumpNameAndStamp(t *testing.T) {
	at := time.Date(2026, 11, 27, 1, 2, 3, 0, time.FixedZone("KST", 9*3600))
	name := dumpName(19, at)
	if name != "pre-19-20261126T160203Z.dump" {
		t.Fatalf("dumpName = %q (UTC 로 찍어야 한다)", name)
	}
	if st, ok := dumpStamp(name); !ok || st != "20261126T160203Z" {
		t.Fatalf("dumpStamp(%q) = %q, %v", name, st, ok)
	}
	for _, bad := range []string{
		"pre-19.dump",                       // 시각 없음 - 다시 돌리면 덮인다
		"pre-x-20261126T160203Z.dump",       // 판이 수가 아니다
		"pre-19-2026.dump",                  // 시각 폭이 다르다
		"pre-19-20261326T160203Z.dump",      // 시각이 아니다
		"pre-19-20261126T160203Z.dump.part", // 뜨는 중인 파일
		"fd.db.bak-20261126T160203Z",        // SQLite 백업
		"pre--20261126T160203Z.dump",
	} {
		if _, ok := dumpStamp(bad); ok {
			t.Errorf("dumpStamp(%q) 가 덤프 이름으로 받았다", bad)
		}
	}
}

// TestLatestDumpPicksNewestByTime 은 되돌리기가 판이 아니라 뜬 시각으로 고르는지, 덤프가 아닌 것을 안 집는지 잰다.
func TestLatestDumpPicksNewestByTime(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"pre-18-20261101T000000Z.dump",
		"pre-19-20261102T000000Z.dump",
		"pre-17-20261104T000000Z.dump",      // 판은 낮지만 가장 나중에 떴다
		"pre-19-20261105T000000Z.dump.part", // 덜 뜬 것
		"notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(dumpMagic), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "pre-19-20261109T000000Z.dump"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := latestDump(dir)
	if err != nil || filepath.Base(got) != "pre-17-20261104T000000Z.dump" {
		t.Fatalf("latestDump = %q, %v (기대 pre-17-20261104T000000Z.dump)", got, err)
	}

	empty := t.TempDir()
	if _, err := latestDump(empty); err == nil || !strings.Contains(err.Error(), "되돌릴 덤프가 없다") {
		t.Fatalf("덤프 없는 자리: %v", err)
	}
	if _, err := latestDump(filepath.Join(empty, "none")); err == nil || !strings.Contains(err.Error(), "백업 자리를 읽지 못했다") {
		t.Fatalf("없는 자리: %v", err)
	}
}

// TestPGSSLModeCarriesOnlyWhatLibpqCanMatch 는 pgx 가 푼 TLS 설정을 libpq 의 sslmode 로 되옮기는 표다. 같은 뜻으로
// 못 넘기는 것(서버 인증서를 재는 모드 · 여러 호스트)은 거절해야 한다 - 약하게 넘기면 백업 접속만 검증 없이 붙는다.
func TestPGSSLModeCarriesOnlyWhatLibpqCanMatch(t *testing.T) {
	cleanPGEnv(t)
	root := writeTestRootCert(t)
	const base = "postgres://u:pw@db.example:5432/ledger"
	for _, c := range []struct {
		url, want, refuse string
	}{
		{base + "?sslmode=disable", "disable", ""},
		{base + "?sslmode=allow", "allow", ""},
		{base + "?sslmode=prefer", "prefer", ""},
		{base, "prefer", ""}, // libpq 와 pgx 의 기본값
		{base + "?sslmode=require", "require", ""},
		{base + "?sslmode=verify-ca", "", "서버 인증서"},
		{base + "?sslmode=verify-full", "", "서버 인증서"},
		{base + "?sslmode=verify-full&sslrootcert=system", "", "서버 인증서"},
		{base + "?sslmode=require&sslrootcert=" + root, "", "서버 인증서"}, // libpq 는 이것을 verify-ca 로 본다
		{"postgres://u:pw@db1.example:5432,db2.example:5432/ledger?sslmode=disable", "", "호스트가 여럿"},
	} {
		cfg, err := pgx.ParseConfig(c.url)
		if err != nil {
			t.Fatalf("%s: 해석 실패: %v", c.url, err)
		}
		got, err := pgSSLMode(&cfg.Config)
		switch {
		case c.refuse == "" && (err != nil || got != c.want):
			t.Errorf("%s: %q, %v (기대 %q)", c.url, got, err, c.want)
		case c.refuse != "" && (err == nil || !strings.Contains(err.Error(), c.refuse)):
			t.Errorf("%s: 거절해야 하는데 %q, %v", c.url, got, err)
		}
	}
}

// writeTestRootCert 는 sslrootcert 로 줄 자체 서명 인증서 파일을 만든다(pgx 는 해석할 때 그 파일을 읽는다).
func writeTestRootCert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "root.crt")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLibpqEnvRebuildsTheConnection 은 libpq 도구에 넘길 환경이 서버의 해석에서만 나오는지 잰다. 물려받은 PG 변수는
// 빠지고, 비밀번호는 PGPASSWORD 하나에만 있고, 접속을 지키는 설정(channel_binding · require_auth)이 따라간다.
func TestLibpqEnvRebuildsTheConnection(t *testing.T) {
	cleanPGEnv(t)
	const pw = "s3cret-pw"
	tg := ParseTarget("postgres://fduser:" + pw + "@db.example:6543/ledger?sslmode=require" +
		"&channel_binding=require&require_auth=scram-sha-256")
	inherited := []string{"PATH=/usr/bin", "HOME=/h", "LANG=C.UTF-8",
		"PGPASSWORD=inherited", "PGHOST=elsewhere", "PGSERVICE=svc", "PGSSLMODE=disable", "PGOPTIONS=-c x=y"}
	env, err := libpqEnv(tg, inherited)
	if err != nil {
		t.Fatalf("libpqEnv: %v", err)
	}
	want := []string{"PATH=/usr/bin", "HOME=/h", "LANG=C.UTF-8",
		"PGHOST=db.example", "PGPORT=6543", "PGUSER=fduser", "PGDATABASE=ledger", "PGSSLMODE=require",
		"PGPASSWORD=" + pw, "PGCHANNELBINDING=require", "PGREQUIREAUTH=scram-sha-256"}
	if !slices.Equal(env, want) {
		t.Fatalf("환경:\n got %q\nwant %q", env, want)
	}

	// 기본값(channel_binding=prefer · 빈 require_auth)은 싣지 않는다 - libpq 의 기본값과 같다.
	plain, err := libpqEnv(ParseTarget("postgres://fduser@db.example/ledger?sslmode=disable"), nil)
	if err != nil {
		t.Fatalf("libpqEnv(비밀번호 없음): %v", err)
	}
	for _, kv := range plain {
		if strings.HasPrefix(kv, "PGPASSWORD=") || strings.HasPrefix(kv, "PGCHANNELBINDING=") || strings.HasPrefix(kv, "PGREQUIREAUTH=") {
			t.Errorf("기본 접속에 %q 가 실렸다: %q", kv, plain)
		}
	}

	// 거절 · 해석 실패는 표시값과 고정 문구만 싣는다.
	for raw, frag := range map[string]string{
		"postgres://fduser:" + pw + "@db.example/ledger?sslmode=verify-full": "같은 뜻으로 넘길 수 없는 접속이다(postgres)",
		"postgres://fduser:" + pw + "@db.example:notaport/ledger":            "해석하지 못했다(postgres)",
	} {
		_, err := libpqEnv(ParseTarget(raw), nil)
		if err == nil || !strings.Contains(err.Error(), frag) {
			t.Errorf("거절 문구에 %q 가 없다: %v", frag, err)
			continue
		}
		for _, leak := range []string{pw, "fduser", "db.example", "ledger"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("거절 문구에 접속 조각 %q 가 샜다: %v", leak, err)
			}
		}
	}
}

func TestPGDumpArgsCarryNoConnection(t *testing.T) {
	got := pgDumpArgs("pg_dump", "/b/pre-19-20261126T160203Z.dump.part")
	want := []string{"pg_dump", "--format=custom", "--no-password", "--file=/b/pre-19-20261126T160203Z.dump.part"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %q, 기대 %q", got, want)
	}
}

// TestPGDumpFaultNeverEchoesStderr 는 pg_dump 실패를 고정 문구로 가르는 표다. stderr 의 호스트 · 사용자 · 주소가 실리면 안 된다.
func TestPGDumpFaultNeverEchoesStderr(t *testing.T) {
	exitErr := errors.New("exit status ?") // 종료 코드를 모르는 실패
	for _, c := range []struct {
		name   string
		err    error
		stderr string
		want   string
	}{
		{"없는 실행 파일", exec.ErrNotFound, "", "pg_dump 를 찾지 못했다"},
		{"없는 경로", &os.PathError{Op: "stat", Path: "/x/pg_dump", Err: os.ErrNotExist}, "", "pg_dump 를 찾지 못했다"},
		{"문맥 끝", context.DeadlineExceeded, "", "문맥이 끝났다"},
		{"큰 판 낮음", exitErr, "pg_dump: error: aborting because of server version mismatch\npg_dump: detail: server version: 17.2; pg_dump version: 16.4", "큰 판이 서버보다 낮다"},
		{"인증", exitErr, `pg_dump: error: connection to server at "10.1.2.3", port 5432 failed: FATAL:  password authentication failed for user "fduser"`, "인증에 실패했다"},
		{"권한", exitErr, `pg_dump: error: query failed: ERROR:  permission denied for table fd_secret_tbl`, "읽을 권한이 없는 객체"},
		{"접속", exitErr, `pg_dump: error: connection to server at "db.internal" (10.1.2.3), port 5432 failed: Connection refused`, "서버에 붙지 못했다"},
		{"출력", exitErr, `pg_dump: error: could not open output file "/backups/x": Permission denied`, "덤프 파일을 쓰지 못했다"},
		{"모름", exitErr, `pg_dump: error: something odd at db.internal`, "분류하지 못한 실패"},
	} {
		got := pgDumpFault(c.err, c.stderr)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q 에 %q 가 없다", c.name, got, c.want)
		}
		for _, leak := range []string{"10.1.2.3", "fduser", "db.internal", "fd_secret_tbl", "/backups/x"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: stderr 조각 %q 가 샜다: %q", c.name, leak, got)
			}
		}
	}
}

// fakePGDump 는 pg_dump 자리에 둘 셸 스크립트다. 받은 argv 와 PG 환경을 rec 에 적고, body 를 돈다.
func fakePGDump(t *testing.T, rec, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("가짜 pg_dump 는 sh 스크립트다")
	}
	p := filepath.Join(t.TempDir(), "pg_dump")
	script := "#!/bin/sh\n" +
		`printf '%s\n' "$@" > "` + rec + `/args"` + "\n" +
		`env | grep '^PG' | sort > "` + rec + `/env"` + "\n" +
		`for a in "$@"; do case "$a" in --file=*) out="${a#--file=}";; esac; done` + "\n" +
		body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// TestDumpPGRunsTheToolWithoutSecretsInArgv 는 덤프 한 번의 실행 경계를 잰다: 접속은 환경으로만 가고, 다 뜬 뒤에만 정식
// 이름이 생기고, 실패는 고정 문구로 오고 반쪽 파일을 안 남긴다.
func TestDumpPGRunsTheToolWithoutSecretsInArgv(t *testing.T) {
	cleanPGEnv(t)
	t.Setenv("PGAPPNAME", "inherited-app") // 물려받은 PG 변수 - 도구에 안 가야 한다
	const pw = "s3cret-pw"
	tg := ParseTarget("postgres://fduser:" + pw + "@127.0.0.1:5432/ledger?sslmode=disable")
	ctx := context.Background()

	t.Run("성공", func(t *testing.T) {
		rec, dir := t.TempDir(), filepath.Join(t.TempDir(), "backups")
		bin := fakePGDump(t, rec, `printf 'PGDMP-fake' > "$out"`)
		got, err := dumpPG(ctx, tg, dir, bin, 19, quietLog())
		if err != nil {
			t.Fatalf("dumpPG: %v", err)
		}
		if filepath.Dir(got) != dir {
			t.Fatalf("덤프 자리 %q (기대 %q 안)", got, dir)
		}
		if _, ok := dumpStamp(filepath.Base(got)); !ok || !strings.HasPrefix(filepath.Base(got), "pre-19-") {
			t.Fatalf("덤프 이름 %q", filepath.Base(got))
		}
		if names := dirNames(t, dir); len(names) != 1 || names[0] != filepath.Base(got) {
			t.Fatalf("백업 자리에 덤프 하나만 있어야 한다: %q", names)
		}
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("백업 자리 권한: %v, %v", fi.Mode(), err)
		}
		args, _ := os.ReadFile(filepath.Join(rec, "args"))
		wantArgs := "--format=custom\n--no-password\n--file=" + got + ".part\n"
		if string(args) != wantArgs {
			t.Fatalf("argv:\n%s\n기대:\n%s", args, wantArgs)
		}
		env, _ := os.ReadFile(filepath.Join(rec, "env"))
		wantEnv := "PGDATABASE=ledger\nPGHOST=127.0.0.1\nPGPASSWORD=" + pw + "\nPGPORT=5432\nPGSSLMODE=disable\nPGUSER=fduser\n"
		if string(env) != wantEnv {
			t.Fatalf("PG 환경:\n%s\n기대:\n%s", env, wantEnv)
		}
	})

	for _, c := range []struct {
		name, body, want string
	}{
		{"인증 실패", `printf 'PGDMP-half' > "$out"; echo 'pg_dump: error: connection to server at "10.1.2.3", port 5432 failed: FATAL:  password authentication failed for user "fduser"' >&2; exit 1`,
			"pg_dump 가 실패했다(종료 코드 1): 인증에 실패했다"},
		{"형식이 아닌 출력", `printf 'oops' > "$out"`, "사용자 정의 형식이 아니다"},
		{"빈 출력", `: > "$out"`, "사용자 정의 형식이 아니다"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec, dir := t.TempDir(), t.TempDir()
			bin := fakePGDump(t, rec, c.body)
			_, err := dumpPG(ctx, tg, dir, bin, 19, quietLog())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("오류에 %q 가 없다: %v", c.want, err)
			}
			for _, leak := range []string{pw, "fduser", "10.1.2.3"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("오류에 %q 가 샜다: %v", leak, err)
				}
			}
			if names := dirNames(t, dir); len(names) != 0 {
				t.Fatalf("실패한 덤프가 남았다: %q", names)
			}
		})
	}

	t.Run("거절은 도구를 안 부른다", func(t *testing.T) {
		rec, dir := t.TempDir(), t.TempDir()
		bin := fakePGDump(t, rec, `printf 'PGDMP' > "$out"`)
		_, err := dumpPG(ctx, ParseTarget("postgres://fduser:"+pw+"@db.example/ledger?sslmode=verify-full"), dir, bin, 19, quietLog())
		if err == nil || !strings.Contains(err.Error(), "덤프 없이 판을 올리지 않는다") || strings.Contains(err.Error(), pw) {
			t.Fatalf("verify-full 접속: %v", err)
		}
		if _, err := os.Stat(filepath.Join(rec, "args")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("거절했는데 pg_dump 가 돌았다: %v", err)
		}
	})

	t.Run("자리 없음 · 도구 없음", func(t *testing.T) {
		if _, err := dumpPG(ctx, tg, " ", "pg_dump", 19, quietLog()); err == nil || !strings.Contains(err.Error(), "덤프 자리가 없다(--backup-dir)") {
			t.Fatalf("빈 자리: %v", err)
		}
		dir := t.TempDir()
		for _, bin := range []string{"fd-no-such-pg_dump", filepath.Join(dir, "missing", "pg_dump")} {
			if _, err := dumpPG(ctx, tg, dir, bin, 19, quietLog()); err == nil || !strings.Contains(err.Error(), "pg_dump 를 찾지 못했다") {
				t.Fatalf("%s: %v", bin, err)
			}
		}
		if names := dirNames(t, dir); len(names) != 0 {
			t.Fatalf("도구 없는 실행이 무엇을 남겼다: %q", names)
		}
	})
}

// TestRollbackPGNamesTheNewestDump 는 `fd migrate --rollback` 의 Postgres 갈래가 가장 최근 덤프를 고르고, 그것이 깨졌으면
// 옛 덤프로 조용히 넘어가지 않는지 잰다. DB 에 붙지 않는다(접속 URL 은 닿지 않는 주소다).
func TestRollbackPGNamesTheNewestDump(t *testing.T) {
	ctx := context.Background()
	const dsn = "postgres://fduser:s3cret-pw@127.0.0.1:1/ledger?sslmode=disable"
	write := func(dir, name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, err := Rollback(ctx, dsn, "", quietLog()); err == nil || !strings.Contains(err.Error(), "덤프 자리(--backup-dir)가 없다") {
		t.Fatalf("빈 자리: %v", err)
	}
	if _, err := Rollback(ctx, dsn, t.TempDir(), quietLog()); err == nil || !strings.Contains(err.Error(), "되돌릴 덤프가 없다") {
		t.Fatalf("덤프 없음: %v", err)
	}

	dir := t.TempDir()
	write(dir, "pre-19-20261101T000000Z.dump", dumpMagic+"-old")
	newest := write(dir, "pre-19-20261102T000000Z.dump", dumpMagic+"-new")
	out, err := Rollback(ctx, dsn, dir, quietLog())
	if err != nil || out.Backup != newest {
		t.Fatalf("Rollback = %+v, %v (기대 %q)", out, err, newest)
	}
	for _, frag := range []string{"pg_restore --create --exit-on-error --dbname=postgres", `"` + newest + `"`, "RENAME TO"} {
		if !strings.Contains(out.Steps, frag) {
			t.Errorf("절차에 %q 가 없다: %s", frag, out.Steps)
		}
	}
	if strings.Contains(out.Steps, "s3cret-pw") || strings.Contains(out.Steps, "ledger") {
		t.Errorf("절차에 접속 조각이 샜다: %s", out.Steps)
	}

	write(dir, "pre-19-20261103T000000Z.dump", "truncated")
	if _, err := Rollback(ctx, dsn, dir, quietLog()); err == nil || !strings.Contains(err.Error(), "가장 최근 덤프가 깨졌다") {
		t.Fatalf("깨진 최신 덤프: %v", err)
	}
	if !strings.Contains(pgRollbackHint(""), "덤프를 뜨지 않았다") {
		t.Fatalf("덤프 없는 실패의 절차: %q", pgRollbackHint(""))
	}
}
