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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// Postgres 판올림 전 덤프 · 되돌리기의 실행 경계 시험. 덤프는 pg_dump 자리에 가짜 실행 파일을 둔다. 되돌리기는 원장에
// 붙어 식별 · 판을 읽으므로 FD_TEST_PG 의 시험 DB 를 쓴다. 실제 pg_dump · pg_restore 로 뜨고 되살리는 시험은
// pg_migrate_test.go 의 TestPGDumpAndRestoreRehearsal 이다.

// cleanPGEnv 는 이 시험이 기대는 해석에 물려받은 PG 환경 변수가 끼지 않게 비운다(pgx 는 빈 값을 무시한다). HOME 도 빈
// 자리로 돌린다 - pgx 는 ~/.postgresql/root.crt 가 있으면 그것을 루트 인증서로 읽는다.
func cleanPGEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE",
		"PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGCHANNELBINDING", "PGREQUIREAUTH", "PGSSLNEGOTIATION",
		"PGSERVICE", "PGAPPNAME", "PGTARGETSESSIONATTRS", "PGCONNECT_TIMEOUT"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
}

const (
	ledgerA = "0123456789abcdef"
	ledgerB = "fedcba9876543210"
)

func TestDumpNameRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 27, 1, 2, 3, 0, time.FixedZone("KST", 9*3600))
	name := dumpName(ledgerA, 18, at)
	if name != "fd-"+ledgerA+"-from18-20261126T160203Z.dump" {
		t.Fatalf("dumpName = %q (UTC 로 찍어야 한다)", name)
	}
	if got, ok := parseDumpName(name); !ok || got != (dumpInfo{Ledger: ledgerA, From: 18, Stamp: "20261126T160203Z"}) {
		t.Fatalf("parseDumpName(%q) = %+v, %v", name, got, ok)
	}
	for _, bad := range []string{
		"fd-" + ledgerA + "-from18.dump",                         // 시각 없음 - 다시 돌리면 덮인다
		"fd-" + ledgerA + "-fromx-20261126T160203Z.dump",         // 판이 수가 아니다
		"fd-" + ledgerA + "-from018-20261126T160203Z.dump",       // 판 표기가 둘이 된다
		"fd-" + ledgerA + "-from-1-20261126T160203Z.dump",        // 음수
		"fd-" + ledgerA + "-from18-2026.dump",                    // 시각 폭이 다르다
		"fd-" + ledgerA + "-from18-20261326T160203Z.dump",        // 시각이 아니다
		"fd-" + ledgerA + "-from18-20261126T160203Z.dump.x.part", // 뜨는 중인 파일
		"fd-0123456789ABCDEF-from18-20261126T160203Z.dump",       // 식별은 소문자 16진
		"fd-0123456789abcde-from18-20261126T160203Z.dump",        // 식별 폭이 다르다
		"pre-19-20261126T160203Z.dump",                           // 원장 식별이 없는 옛 이름
		"fd.db.bak-20261126T160203Z",                             // SQLite 백업
	} {
		if got, ok := parseDumpName(bad); ok {
			t.Errorf("parseDumpName(%q) 가 덤프 이름으로 받았다: %+v", bad, got)
		}
	}
}

// TestDumpDatabaseReadsTheHeader 는 덤프 머리의 DB 이름을 바이트 그대로 읽는지(pg_restore -l 처럼 줄바꿈을 공백으로 안
// 바꾸는지), 모르는 머리 · 잘린 머리를 거절하는지 잰다.
func TestDumpDatabaseReadsTheHeader(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, strconv.Itoa(len(body))+".dump")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, db := range []string{"fd", "t_x y", "t_x\ny", `t_"x'y`, "원장"} {
		for _, vmin := range []byte{14, 16} {
			got, err := dumpDatabase(write(storetest.FakeDump(db, vmin)))
			if err != nil || got != db {
				t.Errorf("1.%d 머리의 이름 %q: %q, %v", vmin, db, got, err)
			}
		}
	}
	whole := storetest.FakeDump("fd", 16)
	neg := storetest.FakeDump("", 16)
	at := len(neg) - len("-rest") - len("17.2") - 5 - 5 // 이름 길이 int 자리(이름은 비었다)
	neg = neg[:at] + "\x01\x01\x00\x00\x00" + neg[at+5:]
	for name, body := range map[string]string{
		"이름 길이 음수": neg,
		"머리 아님":    "oops",
		"잘린 머리":    whole[:30],
		"다른 형식":    whole[:10] + "\x03" + whole[11:],
		"옛 판":      whole[:6] + "\x09" + whole[7:],
	} {
		if _, err := dumpDatabase(write(body)); err == nil || !strings.Contains(err.Error(), "사용자 정의 형식이 아니다") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestLedgerIDSeparatesClustersAndDatabases 는 원장 식별(ledgerIDSQL, 서버에서 돈다)이 클러스터와 DB 이름을 둘 다
// 가르는지, 경계를 옮긴 쌍을 가르는지, 이름을 싣지 않는지 잰다.
func TestLedgerIDSeparatesClustersAndDatabases(t *testing.T) {
	s := pgOpen(t, os.Getenv(storetest.EnvDSN))
	id := func(sysid, name string) string {
		t.Helper()
		var v string
		if err := s.db.QueryRowContext(context.Background(), `SELECT `+ledgerIDSQL("?::text", "?::text"), sysid, name).Scan(&v); err != nil {
			t.Fatalf("식별 재기: %v", err)
		}
		return v
	}
	a := id("7300000000000000001", "fd")
	for _, other := range []string{id("7300000000000000002", "fd"), id("7300000000000000001", "fd_test"),
		id("730000000000000000", "1fd"), id("7300000000000000001", "fd\n"), id("7300000000000000001", "fd ")} {
		if other == a {
			t.Fatalf("다른 원장이 같은 식별 %s 를 받았다", a)
		}
	}
	if a != id("7300000000000000001", "fd") || len(a) != ledgerIDLen || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("식별 %q", a)
	}
	if adminDB("postgres") != "template1" || adminDB("fd") != "postgres" {
		t.Fatalf("관리 DB: %q %q", adminDB("postgres"), adminDB("fd"))
	}
}

// TestLedgerIDSameFromAdminAndLedger 는 같은 원장의 식별이 원장 DB 안(이전 실행기 · 되돌리기, readLedgerFacts)과 관리 DB 의
// 카탈로그(되돌리기 스크립트)에서 같게 나오는지 잰다. 줄바꿈 · 따옴표 · 공백이 든 이름에서도 같아야 스크립트가 원장을 찾는다.
func TestLedgerIDSameFromAdminAndLedger(t *testing.T) {
	ctx := context.Background()
	dsn, name := oddLedger(t)
	led, err := readLedgerFacts(ctx, pgOpen(t, dsn).db)
	if err != nil {
		t.Fatal(err)
	}
	if led.name != name || led.Admin != "postgres" {
		t.Fatalf("카탈로그의 이름 %q · 관리 DB %q (기대 %q · postgres)", led.name, led.Admin, name)
	}
	admin := pgOpen(t, os.Getenv(storetest.EnvDSN))
	var got string
	if err := admin.db.QueryRowContext(ctx, `SELECT `+ledgerIDSQL("system_identifier::text", "datname::text")+
		` FROM pg_catalog.pg_database, pg_catalog.pg_control_system() WHERE datname = ?`, name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != led.ID {
		t.Fatalf("관리 DB 에서 잰 식별 %s, 원장 안에서 잰 식별 %s", got, led.ID)
	}
}

// TestLatestDumpPicksNewestOfThisLedger 는 되돌리기가 이 원장의 덤프만, 판이 아니라 뜬 시각으로 고르는지, 덤프가 아닌 것을
// 안 집는지 잰다.
func TestLatestDumpPicksNewestOfThisLedger(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"fd-" + ledgerA + "-from17-20261101T000000Z.dump",
		"fd-" + ledgerA + "-from18-20261102T000000Z.dump",
		"fd-" + ledgerA + "-from16-20261104T000000Z.dump",           // 판은 낮지만 이 원장에서 가장 나중에 떴다
		"fd-" + ledgerA + "-from18-20261105T000000Z.dump.1234.part", // 덜 뜬 것
		"fd-" + ledgerB + "-from18-20261108T000000Z.dump",           // 다른 원장 - 더 최근이어도 안 고른다
		"pre-18-20261109T000000Z.dump",                              // 식별 없는 옛 이름
		"notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(dumpMagic), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "fd-"+ledgerA+"-from18-20261109T000000Z.dump"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, info, err := latestDump(dir, ledgerA)
	if err != nil || filepath.Base(got) != "fd-"+ledgerA+"-from16-20261104T000000Z.dump" || info.From != 16 {
		t.Fatalf("latestDump = %q, %+v, %v (기대 from16-20261104)", got, info, err)
	}
	if got, _, err := latestDump(dir, ledgerB); err != nil || filepath.Base(got) != "fd-"+ledgerB+"-from18-20261108T000000Z.dump" {
		t.Fatalf("다른 원장: %q, %v", got, err)
	}
	_, _, err = latestDump(dir, "00000000000000ff")
	if err == nil || !strings.Contains(err.Error(), "되돌릴 덤프가 없다") || !strings.Contains(err.Error(), "다른 원장의 덤프 4개") {
		t.Fatalf("이 원장의 덤프가 없는 자리: %v", err)
	}
	empty := t.TempDir()
	if _, _, err := latestDump(filepath.Join(empty, "none"), ledgerA); err == nil || !strings.Contains(err.Error(), "백업 자리를 읽지 못했다") {
		t.Fatalf("없는 자리: %v", err)
	}
}

// TestPGTLSCarriesOnlyWhatLibpqCanMatch 는 pgx 가 푼 TLS 설정을 libpq 의 sslmode · sslrootcert 로 되옮기는 표다. 서버
// 인증서를 재는 모드는 서버와 같은 루트 인증서 파일을 찾을 때만 받는다. 같은 뜻으로 못 넘기는 것(시스템 인증서 저장소 ·
// 여러 호스트)은 거절해야 한다 - 약하게 넘기면 백업 접속만 검증 없이 붙는다.
func TestPGTLSCarriesOnlyWhatLibpqCanMatch(t *testing.T) {
	cleanPGEnv(t)
	root := writeTestRootCert(t)
	other := writeTestRootCert(t)
	const base = "postgres://u:pw@db.example:5432/ledger"
	for _, c := range []struct {
		name, url, env     string
		want, root, refuse string
	}{
		{"disable", base + "?sslmode=disable", "", "disable", "", ""},
		{"allow", base + "?sslmode=allow", "", "allow", "", ""},
		{"prefer", base + "?sslmode=prefer", "", "prefer", "", ""},
		{"기본값", base, "", "prefer", "", ""}, // libpq 와 pgx 의 기본값
		{"require", base + "?sslmode=require", "", "require", "", ""},
		{"verify-ca", base + "?sslmode=verify-ca&sslrootcert=" + root, "", "verify-ca", root, ""},
		{"verify-full", base + "?sslmode=verify-full&sslrootcert=" + root, "", "verify-full", root, ""},
		{"루트를 준 require", base + "?sslmode=require&sslrootcert=" + root, "", "verify-ca", root, ""}, // libpq 도 verify-ca 로 본다
		{"환경의 루트", base + "?sslmode=verify-full", root, "verify-full", root, ""},
		{"URL 이 환경을 이긴다", base + "?sslmode=verify-full&sslrootcert=" + root, other, "verify-full", root, ""},
		{"루트 없는 verify-ca", base + "?sslmode=verify-ca", "", "", "", "루트 인증서 파일 없이"},
		{"루트 없는 verify-full", base + "?sslmode=verify-full", "", "", "", "루트 인증서 파일 없이"},
		{"시스템 저장소", base + "?sslmode=verify-full&sslrootcert=system", "", "", "", "루트 인증서 파일을 찾지 못했다"},
		{"여러 호스트", "postgres://u:pw@db1.example:5432,db2.example:5432/ledger?sslmode=disable", "", "", "", "호스트가 여럿"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PGSSLROOTCERT", c.env)
			cfg, err := pgx.ParseConfig(c.url)
			if err != nil {
				t.Fatalf("해석 실패: %v", err)
			}
			var environ []string
			if c.env != "" {
				environ = []string{"PGSSLROOTCERT=" + c.env}
			}
			mode, got, err := pgTLS(&cfg.Config, sslRootCandidates(c.url, environ))
			switch {
			case c.refuse == "" && (err != nil || mode != c.want || got != c.root):
				t.Errorf("%q %q, %v (기대 %q %q)", mode, got, err, c.want, c.root)
			case c.refuse != "" && (err == nil || !strings.Contains(err.Error(), c.refuse)):
				t.Errorf("거절해야 하는데 %q %q, %v", mode, got, err)
			}
		})
	}

	// 루트 인증서가 서버가 읽은 것과 다르면(후보 파일이 그 사이 바뀌었다) 받지 않는다.
	cfg, err := pgx.ParseConfig(base + "?sslmode=verify-full&sslrootcert=" + root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := pgTLS(&cfg.Config, []string{other}); err == nil || !strings.Contains(err.Error(), "찾지 못했다") {
		t.Fatalf("다른 루트 인증서를 받았다: %v", err)
	}
}

// TestTargetSessionAttrsCarriesTheServersChoice 는 접속 URL 의 target_session_attrs 가 libpq 값으로 되옮겨지는지 잰다.
// 빠뜨리면 서버는 주 서버에 붙는데 덤프는 지연된 복제본에서 성공할 수 있다.
func TestTargetSessionAttrsCarriesTheServersChoice(t *testing.T) {
	cleanPGEnv(t)
	for _, v := range []string{"any", "read-write", "read-only", "primary", "standby", "prefer-standby"} {
		cfg, err := pgx.ParseConfig("postgres://u@db.example/ledger?sslmode=disable&target_session_attrs=" + v)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		got, err := targetSessionAttrs(&cfg.Config)
		want := v
		if v == "any" {
			want = ""
		}
		if err != nil || got != want {
			t.Errorf("%s: %q, %v", v, got, err)
		}
	}
	cfg, err := pgx.ParseConfig("postgres://u@db.example/ledger?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ValidateConnect = func(context.Context, *pgconn.PgConn) error { return nil }
	if _, err := targetSessionAttrs(&cfg.Config); err == nil {
		t.Fatal("모르는 접속 검증을 받았다")
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
// 빠지고, 비밀번호는 PGPASSWORD 하나에만 있고, 어느 서버를 받는가(target_session_attrs) · 얼마나 기다리는가
// (connect_timeout) · 접속을 지키는 설정(sslrootcert · channel_binding · require_auth)이 따라간다.
func TestLibpqEnvRebuildsTheConnection(t *testing.T) {
	cleanPGEnv(t)
	root := writeTestRootCert(t)
	const pw = "s3cret-pw"
	tg := ParseTarget("postgres://fduser:" + pw + "@db.example:6543/ledger?sslmode=verify-full&sslrootcert=" + root +
		"&channel_binding=require&require_auth=scram-sha-256&target_session_attrs=read-write&connect_timeout=7")
	inherited := []string{"PATH=/usr/bin", "HOME=/h", "LANG=C.UTF-8",
		"PGPASSWORD=inherited", "PGSSLMODE=disable", "PGOPTIONS=-c x=y", "PGCONNECT_TIMEOUT=999"}
	// 대상을 정하는 변수(되돌리기 스크립트가 지우는 것과 한 벌)는 하나도 물려주지 않는다.
	for _, v := range libpqTargetVars {
		inherited = append(inherited, v+"=inherited")
	}
	env, err := libpqEnv(tg, inherited)
	if err != nil {
		t.Fatalf("libpqEnv: %v", err)
	}
	want := []string{"PATH=/usr/bin", "HOME=/h", "LANG=C.UTF-8",
		"PGHOST=db.example", "PGPORT=6543", "PGUSER=fduser", "PGDATABASE=ledger", "PGSSLMODE=verify-full",
		"PGSSLROOTCERT=" + root, "PGPASSWORD=" + pw, "PGTARGETSESSIONATTRS=read-write", "PGCONNECT_TIMEOUT=7",
		"PGCHANNELBINDING=require", "PGREQUIREAUTH=scram-sha-256"}
	if !slices.Equal(env, want) {
		t.Fatalf("환경:\n got %q\nwant %q", env, want)
	}

	// 기본값(channel_binding=prefer · 빈 require_auth · any · 시간 제한 없음)은 싣지 않는다 - libpq 의 기본값과 같다.
	plain, err := libpqEnv(ParseTarget("postgres://fduser@db.example/ledger?sslmode=disable"), nil)
	if err != nil {
		t.Fatalf("libpqEnv(비밀번호 없음): %v", err)
	}
	for _, kv := range plain {
		for _, k := range []string{"PGPASSWORD=", "PGCHANNELBINDING=", "PGREQUIREAUTH=", "PGTARGETSESSIONATTRS=", "PGCONNECT_TIMEOUT=", "PGSSLROOTCERT="} {
			if strings.HasPrefix(kv, k) {
				t.Errorf("기본 접속에 %q 가 실렸다: %q", kv, plain)
			}
		}
	}

	// 거절 · 해석 실패는 표시값과 고정 문구만 싣는다.
	for raw, frag := range map[string]string{
		"postgres://fduser:" + pw + "@db.example/ledger?sslmode=verify-full&sslrootcert=system": "같은 뜻으로 넘길 수 없는 접속이다(postgres)",
		"postgres://fduser:" + pw + "@db.example:notaport/ledger":                               "해석하지 못했다(postgres)",
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
	got := pgDumpArgs("/usr/bin/pg_dump", "/b/x.dump.1.part", "00000003-0000001B-1", 1500*time.Millisecond)
	want := []string{"/usr/bin/pg_dump", "--format=custom", "--no-password", "--snapshot=00000003-0000001B-1",
		"--lock-wait-timeout=1500", "--file=/b/x.dump.1.part"}
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
		{"없는 스냅숏", exitErr, `pg_dump: error: query failed: ERROR:  snapshot "00000003-0000001B-1" does not exist` + "\n" +
			`pg_dump: detail: Query was: SET TRANSACTION SNAPSHOT '00000003-0000001B-1' at db.internal`, "스냅숏을 가져오지 못했다"},
		{"다른 DB 의 스냅숏", exitErr, `pg_dump: error: query failed: ERROR:  cannot import a snapshot from a different database`, "스냅숏을 가져오지 못했다"},
		// --lock-wait-timeout 을 넘었다(pg_dump 17 의 실제 문구).
		// detail 줄은 잠그려던 표를 다 싣는다 - 원장의 snapshot 표가 스냅숏 실패로 읽히면 안 된다.
		{"표 잠금 대기", exitErr, `pg_dump: error: query failed: ERROR:  canceling statement due to statement timeout` + "\n" +
			`pg_dump: detail: Query was: LOCK TABLE public.fd_secret_tbl, public.snapshot IN ACCESS SHARE MODE`, "표 잠금을 기다리다 상한(--lock-wait-timeout"},
		{"snapshot 표의 권한", exitErr, `pg_dump: error: query failed: ERROR:  permission denied for table snapshot` + "\n" +
			`pg_dump: detail: Query was: LOCK TABLE public.snapshot IN ACCESS SHARE MODE`, "읽을 권한이 없는 객체"},
		// 분류할 문구가 detail 줄에만 있으면 분류하지 않는다 - 잠그려던 표 목록의 snapshot 은 스냅숏 실패가 아니다.
		{"잠금 표 한도", exitErr, `pg_dump: error: query failed: ERROR:  out of shared memory` + "\n" +
			`pg_dump: hint: You might need to increase "max_locks_per_transaction".` + "\n" +
			`pg_dump: detail: Query was: LOCK TABLE public.fd_secret_tbl, public.snapshot IN ACCESS SHARE MODE`, "분류하지 못한 실패"},
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

func sealOK(context.Context) error { return nil }

// testSnap 은 가짜 pg_dump 에 넘기는 스냅숏 식별이다(가짜는 가져오지 않는다).
const testSnap = "00000003-0000001B-1"

// testJob 은 가짜 pg_dump(bin)로 dir 에 뜨는 덤프 한 번이다: 원장 ledgerA, 실제 이름 ledger(시험 접속 URL 의 DB), 판 18.
func testJob(dir, bin string) dumpJob {
	return dumpJob{dir: dir, bin: bin, ledger: ledgerFacts{ID: ledgerA, name: "ledger"}, from: 18, snapshot: testSnap,
		lockWait: lockTimeout, seal: sealOK}
}

// dumpHead 는 머리에 DB 이름 db 를 실은 가짜 덤프(storetest.FakeDump)를 파일로 두고, 가짜 pg_dump 가 그것을 출력 자리로
// 옮기는 셸 줄을 낸다.
func dumpHead(t *testing.T, db string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "head.dump")
	if err := os.WriteFile(p, []byte(storetest.FakeDump(db, 16)), 0o600); err != nil {
		t.Fatal(err)
	}
	return `cat '` + p + `' > "$out"`
}

// TestDumpPGRunsTheToolWithoutSecretsInArgv 는 덤프 한 번의 실행 경계를 잰다: 접속은 환경으로만 가고, 다 뜨고 봉인(seal)을
// 지난 뒤에만 정식 이름이 생기고, 실패는 고정 문구로 오고 반쪽 파일을 안 남긴다.
func TestDumpPGRunsTheToolWithoutSecretsInArgv(t *testing.T) {
	cleanPGEnv(t)
	t.Setenv("PGAPPNAME", "inherited-app") // 물려받은 PG 변수 - 도구에 안 가야 한다
	const pw = "s3cret-pw"
	tg := ParseTarget("postgres://fduser:" + pw + "@127.0.0.1:5432/ledger?sslmode=disable")
	ctx := context.Background()

	t.Run("성공", func(t *testing.T) {
		rec, dir := t.TempDir(), filepath.Join(t.TempDir(), "backups")
		bin := fakePGDump(t, rec, dumpHead(t, "ledger"))
		got, err := dumpPG(ctx, tg, testJob(dir, bin), quietLog())
		if err != nil {
			t.Fatalf("dumpPG: %v", err)
		}
		if filepath.Dir(got) != dir {
			t.Fatalf("덤프 자리 %q (기대 %q 안)", got, dir)
		}
		if info, ok := parseDumpName(filepath.Base(got)); !ok || info.Ledger != ledgerA || info.From != 18 {
			t.Fatalf("덤프 이름 %q", filepath.Base(got))
		}
		if names := dirNames(t, dir); len(names) != 1 || names[0] != filepath.Base(got) {
			t.Fatalf("백업 자리에 덤프 하나만 있어야 한다: %q", names)
		}
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("백업 자리 권한: %v, %v", fi.Mode(), err)
		}
		args, _ := os.ReadFile(filepath.Join(rec, "args"))
		lines := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
		if len(lines) != 5 || lines[0] != "--format=custom" || lines[1] != "--no-password" || lines[2] != "--snapshot="+testSnap ||
			lines[3] != "--lock-wait-timeout="+strconv.FormatInt(lockTimeout.Milliseconds(), 10) ||
			!strings.HasPrefix(lines[4], "--file="+got+".") || !strings.HasSuffix(lines[4], ".part") {
			t.Fatalf("argv:\n%s", args)
		}
		env, _ := os.ReadFile(filepath.Join(rec, "env"))
		wantEnv := "PGDATABASE=ledger\nPGHOST=127.0.0.1\nPGPASSWORD=" + pw + "\nPGPORT=5432\nPGSSLMODE=disable\nPGUSER=fduser\n"
		if string(env) != wantEnv {
			t.Fatalf("PG 환경:\n%s\n기대:\n%s", env, wantEnv)
		}
	})

	for _, c := range []struct {
		name, body, want string
		seal             func(context.Context) error
	}{
		{"인증 실패", `printf 'PGDMP-half' > "$out"; echo 'pg_dump: error: connection to server at "10.1.2.3", port 5432 failed: FATAL:  password authentication failed for user "fduser"' >&2; exit 1`,
			"pg_dump 가 실패했다(종료 코드 1): 인증에 실패했다", sealOK},
		{"형식이 아닌 출력", `printf 'oops' > "$out"`, "사용자 정의 형식이 아니다", sealOK},
		{"빈 출력", `: > "$out"`, "사용자 정의 형식이 아니다", sealOK},
		// 실행기가 잠금을 잃었다(연결이 끊겼다) - 다 뜬 덤프여도 이름을 안 붙인다.
		{"봉인 실패", dumpHead(t, "ledger"), "덤프에 최종 이름을 붙이지 않는다 - 이전 실행기가 잠금을 잃었다",
			func(context.Context) error { return errors.New("이전 실행기가 잠금을 잃었다") }},
		// 연결 중개기의 DB 별칭으로 붙었다: 머리에는 접속에 쓴 별칭이 실린다. 봉인까지 가지 않는다.
		{"머리의 DB 이름이 실제 이름과 다르다", dumpHead(t, "ledger_alias"), "DB 이름이 원장의 실제 이름(current_database())과 다르다",
			func(context.Context) error { t.Error("이름이 다른 덤프를 봉인하려 했다"); return nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec, dir := t.TempDir(), t.TempDir()
			bin := fakePGDump(t, rec, c.body)
			j := testJob(dir, bin)
			j.seal = c.seal
			_, err := dumpPG(ctx, tg, j, quietLog())
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

	// 봉인 직후 세션이 끊기고 같은 초에 시작한 다음 실행기가 먼저 게시했다 - 늦게 돌아온 게시는 덮지 않고 실패한다.
	t.Run("먼저 게시된 덤프는 덮지 않는다", func(t *testing.T) {
		rec, dir := t.TempDir(), t.TempDir()
		bin := fakePGDump(t, rec, `printf 'PGDMP-other' > "${out%.*.part}"; `+dumpHead(t, "ledger"))
		_, err := dumpPG(ctx, tg, testJob(dir, bin), quietLog())
		if err == nil || !strings.Contains(err.Error(), "다른 실행기가 먼저 게시했다. 덮지 않는다") {
			t.Fatalf("먼저 게시된 이름 위의 게시: %v", err)
		}
		names := dirNames(t, dir)
		if len(names) != 1 {
			t.Fatalf("백업 자리에 먼저 게시된 덤프 하나만 남아야 한다: %q", names)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, names[0])); string(b) != "PGDMP-other" {
			t.Fatalf("먼저 게시된 덤프가 덮였다: %q", b)
		}
	})

	t.Run("스냅숏 없이는 안 뜬다", func(t *testing.T) {
		rec, dir := t.TempDir(), t.TempDir()
		bin := fakePGDump(t, rec, dumpHead(t, "ledger"))
		j := testJob(dir, bin)
		j.snapshot = ""
		if _, err := dumpPG(ctx, tg, j, quietLog()); err == nil || !strings.Contains(err.Error(), "스냅숏 없이 덤프를 뜨지 않는다") {
			t.Fatalf("스냅숏 없는 덤프: %v", err)
		}
		if _, err := os.Stat(filepath.Join(rec, "args")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("거절했는데 pg_dump 가 돌았다: %v", err)
		}
	})

	t.Run("거절은 도구를 안 부른다", func(t *testing.T) {
		rec, dir := t.TempDir(), t.TempDir()
		bin := fakePGDump(t, rec, dumpHead(t, "ledger"))
		_, err := dumpPG(ctx, ParseTarget("postgres://fduser:"+pw+"@db.example/ledger?sslmode=verify-full"), testJob(dir, bin), quietLog())
		if err == nil || !strings.Contains(err.Error(), "덤프 없이 판을 올리지 않는다") || strings.Contains(err.Error(), pw) {
			t.Fatalf("루트 인증서 없는 verify-full 접속: %v", err)
		}
		if _, err := os.Stat(filepath.Join(rec, "args")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("거절했는데 pg_dump 가 돌았다: %v", err)
		}
		if names := dirNames(t, dir); len(names) != 0 {
			t.Fatalf("거절한 실행이 무엇을 남겼다: %q", names)
		}
	})

	t.Run("자리 없음 · 도구 없음", func(t *testing.T) {
		if _, err := dumpPG(ctx, tg, testJob(" ", "pg_dump"), quietLog()); err == nil || !strings.Contains(err.Error(), "덤프 자리가 없다(--backup-dir)") {
			t.Fatalf("빈 자리: %v", err)
		}
		dir := t.TempDir()
		for _, bin := range []string{"fd-no-such-pg_dump", filepath.Join(dir, "missing", "pg_dump")} {
			if _, err := dumpPG(ctx, tg, testJob(dir, bin), quietLog()); err == nil || !strings.Contains(err.Error(), "pg_dump 를 찾지 못했다") {
				t.Fatalf("%s: %v", bin, err)
			}
		}
		if names := dirNames(t, dir); len(names) != 0 {
			t.Fatalf("도구 없는 실행이 무엇을 남겼다: %q", names)
		}
	})
}

// TestRollbackPGPicksThisLedgersDump 는 `fd migrate --rollback` 의 Postgres 갈래를 잰다: 원장에 붙어 그 원장의 덤프 중
// 가장 최근 것만 고르고, 덤프 뒤로 커밋된 판올림이 없으면 절차를 안 내고, 가장 최근 덤프가 깨졌으면 옛 덤프로 조용히
// 넘어가지 않는다. 낸 스크립트는 대상 클러스터와 관리 DB 를 명령마다 적고, 접속 조각을 싣지 않는다.
func TestRollbackPGPicksThisLedgersDump(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	led, err := readLedgerFacts(ctx, s.db)
	if err != nil {
		t.Fatalf("원장 식별: %v", err)
	}
	if led.Admin != "postgres" {
		t.Fatalf("시험 DB 의 관리 DB %q", led.Admin)
	}
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
	const unreachable = "postgres://fduser:s3cret-pw@127.0.0.1:1/ledger?sslmode=disable&connect_timeout=2"
	if _, err := Rollback(ctx, unreachable, t.TempDir(), quietLog()); err == nil || !strings.Contains(err.Error(), "되돌릴 원장을 가리려면") ||
		strings.Contains(err.Error(), "s3cret-pw") {
		t.Fatalf("닿지 않는 원장: %v", err)
	}

	dir := t.TempDir()
	write(dir, "fd-"+led.ID+"-from"+strconv.Itoa(SchemaVersion-1)+"-20261101T000000Z.dump", storetest.FakeDump(led.name, 16))
	newest := write(dir, "fd-"+led.ID+"-from"+strconv.Itoa(SchemaVersion)+"-20261102T000000Z.dump", storetest.FakeDump(led.name, 16))
	write(dir, "fd-"+ledgerB+"-from"+strconv.Itoa(SchemaVersion)+"-20261109T000000Z.dump", storetest.FakeDump(led.name, 16)) // 다른 원장

	// 지금 판 = 덤프를 뜬 판: 덤프 뒤로 커밋된 판올림이 없다(판올림이 실패해 되감겼다).
	if _, err := Rollback(ctx, dsn, dir, quietLog()); err == nil || !strings.Contains(err.Error(), "되돌릴 판올림이 없다") {
		t.Fatalf("판올림이 커밋되지 않은 원장: %v", err)
	}

	stamp(t, s, SchemaVersion+1) // 덤프 뒤로 판올림이 커밋됐다
	out, err := Rollback(ctx, dsn, dir, quietLog())
	if err != nil || out.Backup != newest {
		t.Fatalf("Rollback = %+v, %v (기대 %q)", out, err, newest)
	}
	sum, err := fileSHA256(newest)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`pg_restore -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d postgres --create --exit-on-error "$dump"`,
		`psql -X -q -v ON_ERROR_STOP=1 -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d postgres -v ledger=` + led.ID +
			` -v ledger_oid=` + strconv.FormatInt(led.OID, 10) + ` <<'SQL'`,
		"unset " + strings.Join(scriptClears(), " ") + "\n",
		`"${PGHOST:?`, "dump='" + newest + "'", `ALTER DATABASE :"db" RENAME TO :"broken";`, `DROP DATABASE IF EXISTS :"db";`,
		`[ -f "$dump" ] ||`, `| cut -c1-64)" = '` + sum + `' ] ||`, `pg_restore -l "$dump" >/dev/null ||`,
		`FROM pg_catalog.pg_database WHERE oid = :'ledger_oid')`,
	} {
		if !strings.Contains(out.Script, frag) {
			t.Errorf("스크립트에 %q 가 없다:\n%s", frag, out.Script)
		}
	}
	if !strings.Contains(out.Steps, "판 "+strconv.Itoa(SchemaVersion)+" 에서 뜬 덤프") {
		t.Errorf("설명에 덤프를 뜬 판이 없다: %s", out.Steps)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{strings.TrimPrefix(u.Path, "/"), u.Host, u.User.Username() + "@"} {
		if strings.Contains(out.Steps+out.Script, leak) {
			t.Errorf("절차에 접속 조각 %q 가 샜다:\n%s\n%s", leak, out.Steps, out.Script)
		}
	}

	// 덤프 자리를 상대 경로로 받아도 스크립트에는 절대 경로가 실린다 - 사람은 스크립트를 다른 디렉토리에서 돌린다.
	t.Chdir(filepath.Dir(dir))
	rel, err := Rollback(ctx, dsn, filepath.Base(dir), quietLog())
	if err != nil || rel.Backup != newest || !strings.Contains(rel.Script, "dump='"+newest+"'") {
		t.Fatalf("상대 경로의 덤프 자리: %q, %v (기대 %q)", rel.Backup, err, newest)
	}

	// 덤프 머리의 DB 이름이 원장 이름과 바이트까지 같아야 한다 - 줄바꿈을 공백으로 바꾼 이름도 다른 이름이다.
	odd := write(dir, "fd-"+led.ID+"-from"+strconv.Itoa(SchemaVersion)+"-20261103T000000Z.dump", storetest.FakeDump(led.name+" ", 16))
	if _, err := Rollback(ctx, dsn, dir, quietLog()); err == nil || !strings.Contains(err.Error(), "DB 이름이 원장의 실제 이름(current_database())과 다르다") ||
		strings.Contains(err.Error(), led.name) {
		t.Fatalf("이름이 다른 덤프: %v", err)
	}
	if err := os.Remove(odd); err != nil {
		t.Fatal(err)
	}
	write(dir, "fd-"+led.ID+"-from"+strconv.Itoa(SchemaVersion)+"-20261104T000000Z.dump", storetest.FakeDump(led.name, 16)[:20])
	if _, err := Rollback(ctx, dsn, dir, quietLog()); err == nil || !strings.Contains(err.Error(), "가장 최근 덤프가 깨졌다") {
		t.Fatalf("깨진 최신 덤프: %v", err)
	}
	if !strings.Contains(pgFailureHint(""), "덤프를 뜨지 않았다") || !strings.Contains(pgFailureHint(newest), "fd migrate --rollback") {
		t.Fatalf("실패 안내: %q · %q", pgFailureHint(""), pgFailureHint(newest))
	}
}
