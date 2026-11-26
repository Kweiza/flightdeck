package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// compose 의 db-backup 서비스(Postgres 원장의 매일 덤프)와 그 스크립트(deploy/db-backup.sh)를 잰다. 스크립트는 진짜 sh 로
// 돌리고 pg_dump · pg_restore 만 가짜로 바꾼다 - 회전 · 실패 · 멈춤의 갈래가 덤프 내용과 무관하게 서는지 본다.
// 진짜 pg_dump 로 뜨고 새 DB 로 되살린 것은 시험이 아니라 시험용 compose 프로젝트의 실기동으로 쟀다.

// fakePGDump 는 받은 인자를 FAKE_LOG 에 한 줄로 남기고 FAKE_MODE 대로 --file 에 쓴다.
//   - ok: 덤프 머리(PGDMP)로 시작하는 파일 · slow: 반쯤 쓰고 오래 선다 · fail: 반쯤 쓰고 실패 · garbage: 머리 없는 파일로 성공
const fakePGDump = `#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_LOG"
[ -n "${PGPASSWORD:-}" ] && printf 'PGPASSWORD 가 환경에 있다\n' >>"$FAKE_LOG"
out=
for a in "$@"; do
	case $a in --file=*) out=${a#--file=} ;; esac
done
case $FAKE_MODE in
ok) printf 'PGDMP fake dump\n' >"$out" ;;
slow) printf 'PGDMP half' >"$out"; exec sleep 30 ;;
fail) printf 'PGDMP half' >"$out"; exit 1 ;;
garbage) printf 'not a dump' >"$out" ;;
esac
`

// fakePGRestore 는 `--list <파일>` 만 안다: 덤프 머리로 시작하면 읽힌다.
const fakePGRestore = `#!/bin/sh
[ "$1" = --list ] || exit 2
[ "$(head -c 5 "$2")" = PGDMP ]
`

var dailyDumpRe = regexp.MustCompile(`^fd-daily-\d{8}T\d{6}Z\.dump$`)

type dbBackupRig struct {
	t      *testing.T
	script string
	bin    string
	dir    string
	log    string
}

func newDBBackupRig(t *testing.T) *dbBackupRig {
	t.Helper()
	r := &dbBackupRig{t: t, script: filepath.Join(pluginRoot(t), "deploy", "db-backup.sh"), bin: t.TempDir()}
	r.dir = filepath.Join(t.TempDir(), "daily")
	r.log = filepath.Join(t.TempDir(), "pg_dump.log")
	if _, err := os.Stat(r.script); err != nil {
		t.Fatalf("db-backup 스크립트가 없다: %v", err)
	}
	for name, body := range map[string]string{"pg_dump": fakePGDump, "pg_restore": fakePGRestore} {
		if err := os.WriteFile(filepath.Join(r.bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

// put 은 자리에 파일 하나를 age 만큼 낡게 둔다.
func (r *dbBackupRig) put(name string, age time.Duration) {
	r.t.Helper()
	p := filepath.Join(r.dir, name)
	if err := os.WriteFile(p, []byte("PGDMP old"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(p, at, at); err != nil {
		r.t.Fatal(err)
	}
}

// seedOld 는 사흘 묵은 덤프 15개(11월 1일 ~ 15일)와 회전이 건드리면 안 되는 파일 셋을 둔다.
func (r *dbBackupRig) seedOld() {
	r.t.Helper()
	for d := 1; d <= 15; d++ {
		r.put("fd-daily-202611"+twoDigits(d)+"T000000Z.dump", 72*time.Hour)
	}
	for _, other := range []string{"fd-L1-from17-20261101T000000Z.dump", "notes.txt", "fd-daily-bad.dump"} {
		r.put(other, 90*24*time.Hour)
	}
}

func twoDigits(n int) string { return string([]byte{byte('0' + n/10), byte('0' + n%10)}) }

func (r *dbBackupRig) cmd(mode string, args ...string) *exec.Cmd {
	c := exec.Command("sh", append([]string{r.script}, args...)...)
	c.Env = []string{
		"PATH=" + r.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FD_DB_BACKUP_DIR=" + r.dir,
		"FAKE_MODE=" + mode,
		"FAKE_LOG=" + r.log,
	}
	return c
}

// run 은 스크립트를 한 번 돌려 종료 코드와 출력을 낸다.
func (r *dbBackupRig) run(mode string, args ...string) (int, string) {
	r.t.Helper()
	out, err := r.cmd(mode, args...).CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out)
	default:
		r.t.Fatalf("스크립트를 못 돌렸다: %v", err)
		return -1, ""
	}
}

func (r *dbBackupRig) names() []string {
	r.t.Helper()
	ents, err := os.ReadDir(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func (r *dbBackupRig) dailies() []string {
	var out []string
	for _, n := range r.names() {
		if dailyDumpRe.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

func (r *dbBackupRig) pgDumpCalls() []string {
	b, err := os.ReadFile(r.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// assertUntouched 는 회전이 남의 파일을 안 건드렸고 반쪽 파일이 안 남았는지 본다.
func (r *dbBackupRig) assertUntouched() {
	r.t.Helper()
	names := r.names()
	for _, other := range []string{"fd-L1-from17-20261101T000000Z.dump", "notes.txt", "fd-daily-bad.dump"} {
		if !slices.Contains(names, other) {
			r.t.Errorf("회전이 자기가 짓지 않은 파일 %s 를 지웠다", other)
		}
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".part") {
			r.t.Errorf("반쯤 쓴 파일 %s 가 남았다", n)
		}
	}
}

// TestDBBackupKeepsTheNewestFourteenAfterASuccessfulDump 는 덤프 하나가 성공하면 새것부터 14개만 남기는지 잰다(재설계 스펙의
// 보존 규칙). 지우는 것은 스크립트가 짓는 이름뿐이고, pg_dump 는 비밀번호 없이(파일로만) 불린다.
func TestDBBackupKeepsTheNewestFourteenAfterASuccessfulDump(t *testing.T) {
	r := newDBBackupRig(t)
	r.seedOld()
	rc, out := r.run("ok", "once")
	if rc != 0 {
		t.Fatalf("성공해야 할 회차가 rc=%d 로 끝났다:\n%s", rc, out)
	}
	got := r.dailies()
	if len(got) != 14 {
		t.Fatalf("덤프가 %d개 남았다 - 14개여야 한다: %v", len(got), got)
	}
	for _, gone := range []string{"fd-daily-20261101T000000Z.dump", "fd-daily-20261102T000000Z.dump"} {
		if slices.Contains(got, gone) {
			t.Errorf("가장 오래된 %s 가 남았다(새것부터 남겨야 한다): %v", gone, got)
		}
	}
	r.assertUntouched()

	calls := r.pgDumpCalls()
	if len(calls) != 1 {
		t.Fatalf("pg_dump 를 %d번 불렀다 - 한 번이어야 한다: %v", len(calls), calls)
	}
	argv := regexp.MustCompile(`^--format=custom --no-password --file=` + regexp.QuoteMeta(r.dir) +
		`/(fd-daily-\d{8}T\d{6}Z\.dump)\.part$`).FindStringSubmatch(calls[0])
	if argv == nil {
		t.Fatalf("pg_dump 의 인자가 %q 다 - 접속은 환경 변수(PGPASSFILE)로만, 쓰기는 .part 로만 해야 한다", calls[0])
	}
	newest := argv[1]
	if !slices.Contains(got, newest) {
		t.Fatalf("뜬 덤프 %s 가 이름을 못 얻었다: %v", newest, got)
	}
	st, err := os.Stat(filepath.Join(r.dir, newest))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("덤프의 권한이 %o 다 - 원장 전체라 0600 이어야 한다", perm)
	}
}

// TestDBBackupKeepsEverythingWhenTheDumpFails 는 실패한 덤프(접속 실패 · 읽히지 않는 덤프)가 앞 덤프를 하나도 안 지우고
// 반쪽 파일도 안 남기는지 잰다. 실패한 날에 회전하면 접속이 끊긴 보름 뒤에는 덤프가 0개다.
func TestDBBackupKeepsEverythingWhenTheDumpFails(t *testing.T) {
	for _, mode := range []string{"fail", "garbage"} {
		t.Run(mode, func(t *testing.T) {
			r := newDBBackupRig(t)
			r.seedOld()
			before := r.dailies()
			rc, out := r.run(mode, "once")
			if rc != 1 {
				t.Fatalf("실패한 회차가 rc=%d 로 끝났다 - 1 이어야 한다:\n%s", rc, out)
			}
			if after := r.dailies(); !slices.Equal(after, before) {
				t.Fatalf("실패한 회차가 덤프 목록을 바꿨다.\n  앞: %v\n  뒤: %v", before, after)
			}
			r.assertUntouched()
		})
	}
}

// TestDBBackupDumpsOnlyWhenDue 는 간격 안의 덤프가 있으면 안 뜨는지 잰다. 재시작 정책이 컨테이너를 되살릴 때마다 뜨면 회전이
// 보름치를 하루 만에 밀어낸다.
func TestDBBackupDumpsOnlyWhenDue(t *testing.T) {
	r := newDBBackupRig(t)
	r.seedOld()
	r.put("fd-daily-20261126T000000Z.dump", time.Hour)
	if rc, out := r.run("ok", "once"); rc != 0 {
		t.Fatalf("때가 아닌 회차가 rc=%d 로 끝났다:\n%s", rc, out)
	}
	if calls := r.pgDumpCalls(); len(calls) != 0 {
		t.Fatalf("간격 안의 덤프가 있는데 pg_dump 를 불렀다: %v", calls)
	}
	if got := r.dailies(); len(got) != 16 {
		t.Fatalf("안 뜬 회차가 회전했다: %d개 %v", len(got), got)
	}
}

// TestDBBackupCheckReportsAStaleDump 는 헬스체크(check)가 가장 새 덤프의 나이를 보는지 잰다(기본 이틀).
func TestDBBackupCheckReportsAStaleDump(t *testing.T) {
	r := newDBBackupRig(t)
	r.seedOld()
	if rc, _ := r.run("ok", "check"); rc == 0 {
		t.Fatal("사흘 묵은 덤프뿐인데 check 가 건강하다고 했다")
	}
	r.put("fd-daily-20261126T000000Z.dump", time.Hour)
	if rc, out := r.run("ok", "check"); rc != 0 {
		t.Fatalf("한 시간 된 덤프가 있는데 check 가 rc=%d:\n%s", rc, out)
	}
}

// TestDBBackupStopsMidDumpWithoutLeavingAPart 는 덤프하는 도중 멈추라는 신호(docker stop 의 TERM)를 받으면 pg_dump 를 끊고
// 반쪽 파일을 지우고 곧바로 끝나는지 잰다. 신호를 덤프 끝까지 미루면 stop 의 유예를 넘겨 SIGKILL 로 잘린다.
func TestDBBackupStopsMidDumpWithoutLeavingAPart(t *testing.T) {
	r := newDBBackupRig(t)
	c := r.cmd("slow")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var part bool
		for _, n := range r.names() {
			part = part || strings.HasSuffix(n, ".part")
		}
		if part {
			break
		}
		if time.Now().After(deadline) {
			_ = c.Process.Kill()
			t.Fatal("10초 안에 덤프가 시작되지 않았다")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 143 {
			t.Fatalf("TERM 을 받은 스크립트가 %v 로 끝났다 - 143 이어야 한다", err)
		}
	case <-time.After(5 * time.Second):
		_ = c.Process.Kill()
		t.Fatal("TERM 을 받고 5초 안에 안 끝났다 - pg_dump 를 안 끊었다")
	}
	for _, n := range r.names() {
		if strings.HasSuffix(n, ".part") {
			t.Fatalf("멈춘 뒤 반쪽 파일 %s 가 남았다", n)
		}
	}
}

// composeService 는 compose 파일에서 서비스 하나의 덩어리(두 칸 들여쓴 이름 줄부터 다음 같은 깊이 줄 앞까지)다.
func composeService(t *testing.T, file, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pluginRoot(t), file))
	if err != nil {
		t.Fatalf("%s 를 못 읽었다: %v", file, err)
	}
	lines := strings.Split(string(raw), "\n")
	start := slices.Index(lines, "  "+name+":")
	if start < 0 {
		t.Fatalf("%s 에 서비스 %s 가 없다", file, name)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if l != "" && !strings.HasPrefix(l, "   ") && !strings.HasPrefix(l, "  #") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// TestComposeDBBackupMatchesTheLedgerImage 는 compose 의 db-backup 서비스가 이 저장소의 약속대로 묶였는지 잰다: 기본 `up` 에
// 안 끼고(profile), 시험 DB 와 같은 주판의 postgres 이미지로, 스크립트를 읽기 전용으로 붙여, 비밀번호를 pgpass 파일로만
// 받고, 14개를 남긴다.
//
// ★ 이미지를 compose.test.yaml 의 pg-test 와 맞댄다. 원장 DB 의 주판을 올리는 사람이 시험 DB 만 올리면 pg_dump 가 더 높은
// 서버를 거절해 매일 덤프가 조용히 멈춘다 - 그 자리가 여기서 빨갛다.
func TestComposeDBBackupMatchesTheLedgerImage(t *testing.T) {
	svc := composeService(t, "compose.yaml", "db-backup")
	imageRe := regexp.MustCompile(`(?m)^\s+image:\s*(\S+)\s*$`)
	got := imageRe.FindStringSubmatch(svc)
	want := imageRe.FindStringSubmatch(composeService(t, "compose.test.yaml", "pg-test"))
	if got == nil || want == nil || got[1] != want[1] {
		t.Fatalf("db-backup 의 이미지(%v)가 시험 DB 의 이미지(%v)와 다르다 - 같은 주판이어야 한다", got, want)
	}
	if !strings.HasPrefix(got[1], "postgres:") {
		t.Fatalf("db-backup 의 이미지가 %s 다 - pg_dump 가 든 postgres 이미지여야 한다", got[1])
	}
	mustContain(t, "db-backup 서비스", svc,
		`profiles: ["postgres"]`,
		`entrypoint: ["sh", "/usr/local/bin/fd-db-backup"]`,
		"- ./deploy/db-backup.sh:/usr/local/bin/fd-db-backup:ro",
		":/backups\n",
		"PGPASSFILE: /run/secrets/fd_pgpass",
		"- fd_pgpass",
		"FD_DB_BACKUP_DIR: /backups/daily",
		`FD_DB_BACKUP_KEEP: "14"`,
		`user: "${FD_UID:-1000}:${FD_GID:-1000}"`,
		"restart: unless-stopped",
	)
	for _, leak := range []string{"PGPASSWORD", "POSTGRES_PASSWORD", "password="} {
		if strings.Contains(svc, leak) {
			t.Errorf("db-backup 서비스에 %q 가 있다 - 비밀번호는 pgpass 파일로만 넘긴다(환경 변수는 docker inspect 에 보인다)", leak)
		}
	}
	// 기본 `up` 이 안 띄우는 서비스라도 없는 서비스에 기대면 설정 전체가 선다 - db 가 생기기 전에는 기대지 않는다.
	if strings.Contains(svc, "depends_on") && !strings.Contains(composeTopLevel(t), "\n  db:\n") {
		t.Error("db-backup 이 없는 db 서비스에 기댄다 - 기본 `up` 까지 설정 오류로 선다")
	}
}

func composeTopLevel(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pluginRoot(t), "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
