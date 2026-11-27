package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/store"
)

// compose 의 db-backup 서비스(Postgres 원장의 매일 덤프)와 그 스크립트(deploy/db-backup.sh)를 잰다. 스크립트는 진짜 sh 로
// 돌리고 pg_dump · pg_restore 만 가짜로 바꾼다 - 회전 · 실패 · 기한 · 멈춤 · 잠금의 갈래가 덤프 내용과 무관하게 서는지 본다.
// 진짜 pg_dump 로 뜨고 새 DB 로 되살린 것은 시험이 아니라 시험용 compose 프로젝트의 실기동으로 쟀다.
//
// ★ flock · timeout 은 운영 이미지(postgres, Debian)에 있는 util-linux · coreutils 의 것이다. 이 머신에 없으면(macOS) 같은
// 꼴만 아는 대역을 PATH 에 둔다 - 잠금은 대역도 진짜 flock(2) 이고, 기한은 대역도 진짜 시계다.

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

// shimFlock 은 util-linux 의 `flock -n <fd>` 꼴만 안다. 잠금은 셸이 연 fd 의 열린 파일 설명에 걸리므로 대역이 끝나도 셸이
// 그 fd 를 닫을 때까지(물려받은 자식까지) 남는다 - util-linux 의 것과 같다.
const shimFlock = `#!/usr/bin/perl
use strict; use warnings; use Fcntl qw(:flock);
@ARGV == 2 && $ARGV[0] eq '-n' && $ARGV[1] =~ /^\d+$/ or die "flock 대역: -n <fd> 꼴만 안다\n";
open(my $fh, '>>&=', $ARGV[1]) or die "flock 대역: fd $ARGV[1] 를 못 연다: $!\n";
exit(flock($fh, LOCK_EX | LOCK_NB) ? 0 : 1);
`

// shimTimeout 은 coreutils 의 `timeout -k <K> <D>[s] <명령…>` 꼴만 안다: 기한에 명령에 TERM 을 보내고 124 로 끝난다.
// 받은 TERM 은 명령에 넘긴다. 그 밖에는 명령의 종료 코드다.
const shimTimeout = `#!/usr/bin/perl
use strict; use warnings; use Time::HiRes qw(alarm);
my ($k, undef, $d, @cmd) = @ARGV;
defined $k && $k eq '-k' && @cmd or die "timeout 대역: -k <K> <D> <명령…> 꼴만 안다\n";
$d =~ s/s$//;
my $pid = fork() // die "timeout 대역: fork: $!\n";
if ($pid == 0) { exec { $cmd[0] } @cmd or exit 127 }
my $timed = 0;
$SIG{TERM} = sub { kill 'TERM', $pid };
$SIG{ALRM} = sub { $timed = 1; kill 'TERM', $pid };
alarm $d;
waitpid($pid, 0);
exit 124 if $timed;
exit(($? & 127) ? 128 + ($? & 127) : $? >> 8);
`

var (
	dailyDumpRe = regexp.MustCompile(`^fd-daily-\d{8}T\d{6}Z\.dump$`)
	deadlineRe  = regexp.MustCompile(`기한 ([0-9.]+)초 · 낡음 ([0-9]+)분`)
)

// 시험의 DB 이름은 덤프 자리 이름(dbkey)이 그대로인 글자만 쓴다 - 자리를 Go 에서 다시 옮기지 않는다. 옮김 자체는
// TestDBBackupNamesEachDatabaseDirectoryApart 가 잰다.
const (
	rigDB   = "flightdeck"
	otherDB = "fd_next"
)

type dbBackupRig struct {
	t      *testing.T
	script string
	bin    string
	root   string // FD_DB_BACKUP_DIR
	db     string // PGDATABASE
	log    string
	env    []string // 시험마다 더하는 환경
}

func newDBBackupRig(t *testing.T) *dbBackupRig {
	t.Helper()
	r := &dbBackupRig{t: t, script: filepath.Join(pluginRoot(t), "deploy", "db-backup.sh"), bin: t.TempDir(), db: rigDB}
	r.root = filepath.Join(t.TempDir(), "daily")
	r.log = filepath.Join(t.TempDir(), "pg_dump.log")
	if _, err := os.Stat(r.script); err != nil {
		t.Fatalf("db-backup 스크립트가 없다: %v", err)
	}
	tools := map[string]string{"pg_dump": fakePGDump, "pg_restore": fakePGRestore}
	for name, shim := range map[string]string{"flock": shimFlock, "timeout": shimTimeout} {
		if _, err := exec.LookPath(name); err == nil {
			continue
		}
		if _, err := exec.LookPath("perl"); err != nil {
			t.Fatalf("이 머신에 %s 도 그 대역을 돌릴 perl 도 없다", name)
		}
		tools[name] = shim
	}
	for name, body := range tools {
		if err := os.WriteFile(filepath.Join(r.bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(r.dirOf(r.db), 0o700); err != nil {
		t.Fatal(err)
	}
	return r
}

// dirOf 는 DB 하나의 덤프 자리다.
func (r *dbBackupRig) dirOf(db string) string { return filepath.Join(r.root, db) }

// put 은 지금 DB 의 자리에 파일 하나를 age 만큼 낡게 둔다.
func (r *dbBackupRig) put(name string, age time.Duration) {
	r.t.Helper()
	r.putIn(r.db, name, age)
}

func (r *dbBackupRig) putIn(db, name string, age time.Duration) {
	r.t.Helper()
	if err := os.MkdirAll(r.dirOf(db), 0o700); err != nil {
		r.t.Fatal(err)
	}
	p := filepath.Join(r.dirOf(db), name)
	if err := os.WriteFile(p, []byte("PGDMP old"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(p, at, at); err != nil {
		r.t.Fatal(err)
	}
}

// strangers 는 자리에 있어도 덤프로 세지도 지우지도 않는 이름들이다. 덤프 이름과 앞이나 뒤가 닮았다.
var strangers = []string{"fd-L1-from17-20261101T000000Z.dump", "notes.txt", "fd-daily-bad.dump",
	"fd-daily-2026111T000000Z.dump", "fd-daily-20261101T000000Z.dump.bak"}

// seedOld 는 사흘 묵은 덤프 15개(11월 1일 ~ 15일)와 회전이 건드리면 안 되는 파일들을 둔다.
func (r *dbBackupRig) seedOld() {
	r.t.Helper()
	for d := 1; d <= 15; d++ {
		r.put("fd-daily-202611"+twoDigits(d)+"T000000Z.dump", 72*time.Hour)
	}
	for _, other := range strangers {
		r.put(other, 90*24*time.Hour)
	}
}

func twoDigits(n int) string { return string([]byte{byte('0' + n/10), byte('0' + n%10)}) }

func (r *dbBackupRig) cmd(mode string, args ...string) *exec.Cmd {
	c := exec.Command("sh", append([]string{r.script}, args...)...)
	c.Env = append([]string{
		"PATH=" + r.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FD_DB_BACKUP_DIR=" + r.root,
		"PGDATABASE=" + r.db,
		"FAKE_MODE=" + mode,
		"FAKE_LOG=" + r.log,
	}, r.env...)
	return c
}

// run 은 스크립트를 한 번 돌려 종료 코드와 출력을 낸다. 20초 안에 안 끝나면 실패다(멈춘 pg_dump 를 안 끊은 것이다).
func (r *dbBackupRig) run(mode string, args ...string) (int, string) {
	r.t.Helper()
	c := r.cmd(mode, args...)
	var out strings.Builder
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		r.t.Fatalf("스크립트를 못 돌렸다: %v", err)
	}
	return r.wait(c, &out, 20*time.Second)
}

func (r *dbBackupRig) wait(c *exec.Cmd, out *strings.Builder, within time.Duration) (int, string) {
	r.t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0, out.String()
		case errors.As(err, &ee):
			return ee.ExitCode(), out.String()
		default:
			r.t.Fatalf("스크립트를 못 기다렸다: %v", err)
			return -1, ""
		}
	case <-time.After(within):
		_ = c.Process.Kill()
		<-done
		r.t.Fatalf("스크립트가 %s 안에 안 끝났다:\n%s", within, out.String())
		return -1, ""
	}
}

func (r *dbBackupRig) names() []string {
	r.t.Helper()
	return r.namesIn(r.db)
}

func (r *dbBackupRig) namesIn(db string) []string {
	r.t.Helper()
	ents, err := os.ReadDir(r.dirOf(db))
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func (r *dbBackupRig) dailies() []string { return r.dailiesIn(r.db) }

func (r *dbBackupRig) dailiesIn(db string) []string {
	var out []string
	for _, n := range r.namesIn(db) {
		if dailyDumpRe.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

func (r *dbBackupRig) parts() []string {
	var out []string
	for _, n := range r.names() {
		if strings.Contains(n, ".part") {
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

// startSlow 는 멈춰 선 덤프 하나를 띄우고 그 임시 파일이 생길 때까지 기다린다. 끝은 부른 쪽이 TERM 으로 낸다.
func (r *dbBackupRig) startSlow(args ...string) (*exec.Cmd, *strings.Builder, string) {
	r.t.Helper()
	c := r.cmd("slow", args...)
	var out strings.Builder
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = c.Process.Kill() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if p := r.parts(); len(p) == 1 {
			b, _ := os.ReadFile(filepath.Join(r.dirOf(r.db), p[0]))
			if strings.HasPrefix(string(b), "PGDMP") {
				return c, &out, p[0]
			}
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("10초 안에 덤프가 시작되지 않았다:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertUntouched 는 회전이 남의 파일을 안 건드렸고 반쪽 파일이 안 남았는지 본다.
func (r *dbBackupRig) assertUntouched() {
	r.t.Helper()
	names := r.names()
	for _, other := range strangers {
		if !slices.Contains(names, other) {
			r.t.Errorf("회전이 자기가 짓지 않은 파일 %s 를 지웠다", other)
		}
	}
	if p := r.parts(); len(p) > 0 {
		r.t.Errorf("반쯤 쓴 파일이 남았다: %v", p)
	}
}

// TestDBBackupKeepsTheNewestFourteenAfterASuccessfulDump 는 덤프 하나가 성공하면 새것부터 14개만 남기는지 잰다(재설계 스펙의
// 보존 규칙). 지우는 것은 스크립트가 짓는 이름뿐이고, pg_dump 는 비밀번호 없이(파일로만), 실행마다 다른 임시 이름으로,
// 서버 연결과 같은 잠금 대기 상한으로 불린다.
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
	lockWait := strconv.FormatInt(store.LockTimeout.Milliseconds(), 10)
	argv := regexp.MustCompile(`^--lock-wait-timeout=` + lockWait + ` --format=custom --no-password --file=` +
		regexp.QuoteMeta(r.dirOf(r.db)) + `/(fd-daily-\d{8}T\d{6}Z\.dump)\.part\.[A-Za-z0-9]{6}$`).FindStringSubmatch(calls[0])
	if argv == nil {
		t.Fatalf("pg_dump 의 인자가 %q 다 - 잠금 대기는 서버의 lock_timeout(%sms)과 같고, 접속은 환경 변수(PGPASSFILE)로만, "+
			"쓰기는 DB 자리 안의 실행마다 다른 임시 이름으로만 해야 한다", calls[0], lockWait)
	}
	newest := argv[1]
	if !slices.Contains(got, newest) {
		t.Fatalf("뜬 덤프 %s 가 이름을 못 얻었다: %v", newest, got)
	}
	st, err := os.Stat(filepath.Join(r.dirOf(r.db), newest))
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

// TestDBBackupStopsADumpPastTheDeadline 는 멈춰 선 pg_dump(볼륨이 응답을 멈춘 날)를 기한(FD_DUMP_TIMEOUT)에 끊고, 실패로 쳐서
// 반쪽 파일을 안 남기고 회전하지 않는지 잰다. 기한이 없으면 pg_dump 가 스냅숏과 모든 표의 잠금을 쥔 채 서서 fd migrate 의
// DDL 이 그동안 실패한다.
func TestDBBackupStopsADumpPastTheDeadline(t *testing.T) {
	r := newDBBackupRig(t)
	r.seedOld()
	r.env = append(r.env, "FD_DUMP_TIMEOUT=1s")
	before := r.dailies()
	began := time.Now()
	rc, out := r.run("slow", "once")
	if took := time.Since(began); took > 15*time.Second {
		t.Fatalf("기한 1초인 덤프가 %s 걸렸다 - 기한이 pg_dump 를 안 끊었다", took)
	}
	if rc != 1 {
		t.Fatalf("기한을 넘은 회차가 rc=%d 로 끝났다 - 실패(1)여야 한다:\n%s", rc, out)
	}
	mustContain(t, "기한을 넘은 회차의 출력", out, "기한(FD_DUMP_TIMEOUT, 1초)을 넘어 끊었다")
	if after := r.dailies(); !slices.Equal(after, before) {
		t.Fatalf("기한을 넘은 회차가 덤프 목록을 바꿨다.\n  앞: %v\n  뒤: %v", before, after)
	}
	r.assertUntouched()
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

// TestDBBackupCountsOnlyItsOwnDumpNames 는 때 확인 · 헬스체크가 회전과 같은 이름 규칙(fd-daily-<UTC>.dump) 하나만 보는지
// 잰다. 덤프 이름과 닮은 갓 생긴 파일(fd-daily-bad.dump 따위)을 덤프로 세면, 회전은 안 세는 파일 때문에 덤프가 안 떠지고
// 헬스체크는 건강하다고 한다.
func TestDBBackupCountsOnlyItsOwnDumpNames(t *testing.T) {
	r := newDBBackupRig(t)
	r.seedOld()
	for _, other := range strangers {
		r.put(other, time.Hour)
	}
	if rc, _ := r.run("ok", "check"); rc == 0 {
		t.Fatal("갓 생긴 것은 이름 규칙 밖의 파일뿐인데 check 가 건강하다고 했다")
	}
	if rc, out := r.run("ok", "once"); rc != 0 {
		t.Fatalf("때가 된 회차가 rc=%d 로 끝났다:\n%s", rc, out)
	}
	if calls := r.pgDumpCalls(); len(calls) != 1 {
		t.Fatalf("이름 규칙 밖의 파일이 때 확인에 셌다 - pg_dump 를 %d번 불렀다(한 번이어야 한다)", len(calls))
	}
	r.assertUntouched()
}

// TestDBBackupCheckReportsAStaleDump 는 헬스체크(check)가 가장 새 덤프의 나이를 보는지 잰다. 기본 한도는 간격 + 확인 두 번 +
// 기한(26시간 30분)이다 - 잘 도는 동안의 최대 나이(25시간 30분) 위로 한 번 실패한 재시도만큼이다. 이틀을 기다리지 않는다.
func TestDBBackupCheckReportsAStaleDump(t *testing.T) {
	r := newDBBackupRig(t)
	r.seedOld()
	if rc, _ := r.run("ok", "check"); rc == 0 {
		t.Fatal("사흘 묵은 덤프뿐인데 check 가 건강하다고 했다")
	}
	r.put("fd-daily-20261124T000000Z.dump", 27*time.Hour)
	if rc, _ := r.run("ok", "check"); rc == 0 {
		t.Fatal("가장 새 덤프가 27시간 묵었는데 check 가 건강하다고 했다 - 한도가 26시간 30분이어야 한다")
	}
	r.put("fd-daily-20261125T000000Z.dump", 25*time.Hour)
	if rc, out := r.run("ok", "check"); rc != 0 {
		t.Fatalf("25시간 된 덤프(잘 도는 동안의 나이)가 있는데 check 가 rc=%d:\n%s", rc, out)
	}
}

// TestDBBackupRunsOneAtATime 는 한 DB 자리에서 한 실행만 도는지 잰다. 정기 덤프가 도는 중에 수동 once 가 시작되면, 잠금 없이는
// 그 정리가 작성 중인 임시 파일을 지우고 제 덤프와 회전까지 돈다. 잠금을 못 잡은 실행은 아무것도 안 하고 3 으로 끝난다.
func TestDBBackupRunsOneAtATime(t *testing.T) {
	r := newDBBackupRig(t)
	r.seedOld()
	before := r.dailies()
	first, firstOut, part := r.startSlow("once")

	rc, out := r.run("ok", "once")
	if rc != 3 {
		t.Fatalf("다른 실행이 덤프하는 중에 시작한 once 가 rc=%d 로 끝났다 - 3 이어야 한다:\n%s", rc, out)
	}
	mustContain(t, "밀려난 once 의 출력", out, "다른 실행이 같은 자리")
	if !slices.Contains(r.names(), part) {
		t.Fatalf("밀려난 once 가 작성 중인 임시 파일 %s 를 지웠다: %v", part, r.names())
	}
	if after := r.dailies(); !slices.Equal(after, before) {
		t.Fatalf("밀려난 once 가 덤프 목록을 바꿨다.\n  앞: %v\n  뒤: %v", before, after)
	}
	if calls := r.pgDumpCalls(); len(calls) != 1 {
		t.Fatalf("pg_dump 를 %d번 불렀다 - 앞 실행의 한 번뿐이어야 한다: %v", len(calls), calls)
	}

	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if rc, out := r.wait(first, firstOut, 5*time.Second); rc != 143 {
		t.Fatalf("TERM 을 받은 앞 실행이 rc=%d 로 끝났다:\n%s", rc, out)
	}
	if rc, out := r.run("ok", "once"); rc != 0 {
		t.Fatalf("앞 실행이 끝난 뒤의 once 가 rc=%d - 잠금이 안 풀렸다:\n%s", rc, out)
	}
	r.assertUntouched()
}

// TestDBBackupRemovesAnOrphanPartUnderTheLock 는 죽은 실행(SIGKILL · 컨테이너 강제 종료)이 남긴 임시 파일을 다음 실행이
// 잠금 아래에서 지우는지 잰다. 임시 파일을 쓰는 실행은 끝날 때까지 잠금을 쥐므로, 잠금을 잡은 뒤에 남은 것은 고아다.
func TestDBBackupRemovesAnOrphanPartUnderTheLock(t *testing.T) {
	r := newDBBackupRig(t)
	r.seedOld()
	r.put("fd-daily-20261126T000000Z.dump", time.Hour) // 때가 아니다 - 정리만 잰다
	r.put("fd-daily-20261120T000000Z.dump.part.Ab12Cd", 5*time.Minute)
	if rc, out := r.run("ok", "once"); rc != 0 {
		t.Fatalf("rc=%d:\n%s", rc, out)
	}
	r.assertUntouched()
}

// TestDBBackupKeepsEachDatabaseApart 는 DB 마다 덤프 자리가 따로인지 잰다. PGDATABASE 를 A 에서 B 로 바꾸면 A 의 덤프가 B 의
// 때 확인 · 헬스체크에 안 쓰이고(B 의 첫 덤프를 건너뛰지 않는다), B 의 회전이 A 의 덤프를 안 지운다.
func TestDBBackupKeepsEachDatabaseApart(t *testing.T) {
	r := newDBBackupRig(t)
	r.seedOld()
	r.put("fd-daily-20261126T000000Z.dump", time.Hour)
	before := r.dailiesIn(rigDB)

	r.db = otherDB
	r.env = append(r.env, "FD_DB_BACKUP_KEEP=1")
	if rc, _ := r.run("ok", "check"); rc == 0 {
		t.Fatalf("%s 에는 덤프가 없는데 %s 의 덤프로 check 가 건강하다고 했다", otherDB, rigDB)
	}
	if rc, out := r.run("ok", "once"); rc != 0 {
		t.Fatalf("rc=%d:\n%s", rc, out)
	}
	if calls := r.pgDumpCalls(); len(calls) != 1 {
		t.Fatalf("%s 의 첫 덤프를 %s 의 덤프 때문에 건너뛰었다(pg_dump %d번)", otherDB, rigDB, len(calls))
	}
	if got := r.dailiesIn(otherDB); len(got) != 1 {
		t.Fatalf("%s 자리에 덤프가 %d개다 - 하나여야 한다: %v", otherDB, len(got), got)
	}
	if after := r.dailiesIn(rigDB); !slices.Equal(after, before) {
		t.Fatalf("%s 의 회전이 %s 의 덤프를 건드렸다.\n  앞: %v\n  뒤: %v", otherDB, rigDB, before, after)
	}
}

// TestDBBackupNamesEachDatabaseDirectoryApart 는 DB 이름을 자리 이름 하나로 옮기는 규칙(dbkey)을 잰다: 다른 이름은 다른 자리
// (대소문자만 달라도 - 대소문자를 안 가리는 파일 시스템에서도), 경로를 벗어나는 이름(. · .. · /)은 없다.
func TestDBBackupNamesEachDatabaseDirectoryApart(t *testing.T) {
	r := newDBBackupRig(t)
	for db, want := range map[string]string{
		"flightdeck": "flightdeck",
		"fd_next-2":  "fd_next-2",
		"Flightdeck": "%46lightdeck",
		"..":         "%2E%2E",
		"a/b":        "a%2Fb",
		"a%2fb":      "a%252fb",
		"원장":         "%EC%9B%90%EC%9E%A5",
	} {
		r.db = db
		rc, out := r.run("ok", "config")
		if rc != 0 {
			t.Fatalf("DB %q 의 config 가 rc=%d:\n%s", db, rc, out)
		}
		mustContain(t, "DB "+strconv.Quote(db)+" 의 설정", out, "자리 "+filepath.Join(r.root, want)+" · ")
	}
	r.db = ""
	if rc, out := r.run("ok", "config"); rc != 2 || !strings.Contains(out, "PGDATABASE") {
		t.Fatalf("PGDATABASE 가 빈 설정이 rc=%d 로 끝났다 - 자리를 못 가르므로 2 여야 한다:\n%s", rc, out)
	}
}

// TestDBBackupDeadlineReadsLikeFdMigrate 는 FD_DUMP_TIMEOUT 하나를 두 자리(fd migrate 의 판올림 전 덤프 · db-backup 의 매일
// 덤프)가 같은 뜻으로 읽는지 잰다: fd 가 받는 값은 스크립트도 같은 길이로 받고, fd 가 거절하는 값은 스크립트도 거절한다(2).
// 그리고 헬스체크의 기본 한도가 그 기한을 품는다(간격 + 확인 두 번 + 기한).
func TestDBBackupDeadlineReadsLikeFdMigrate(t *testing.T) {
	r := newDBBackupRig(t)
	values := []string{"", "  ", "30m", " 45m ", "2h", "1h30m", "1.5h", ".5m", "5.m", "1.s", "+2m", "90s", "1s",
		"1500ms", "1000ms", "999ms", "1000000000ns", "999999999ns", "1000000us", "1000000µs", "1000000μs",
		"596h31m23s647ms", "596h31m23s648ms", "1h0.5s", "0", "+0", "-0", "-1s", "1", "1 h", "1h 30m", "abc", ".", "s",
		"1..5s", "1e3s", "30M", "9999999999999999999h"}
	for _, v := range values {
		d, err := resolveDumpTimeout(func(k string) (string, bool) { return v, k == dumpTimeoutEnv })
		r.env = []string{"FD_DUMP_TIMEOUT=" + v}
		rc, out := r.run("ok", "config")
		if err != nil {
			if rc != 2 || !strings.Contains(out, "FD_DUMP_TIMEOUT") {
				t.Errorf("fd 가 거절하는 FD_DUMP_TIMEOUT=%q(%v)를 스크립트가 rc=%d 로 받았다:\n%s", v, err, rc, out)
			}
			continue
		}
		if d == 0 {
			d = store.DefaultDumpTimeout
		}
		m := deadlineRe.FindStringSubmatch(out)
		if rc != 0 || m == nil {
			t.Errorf("fd 가 받는 FD_DUMP_TIMEOUT=%q(%s)를 스크립트가 rc=%d 로 거절했다:\n%s", v, d, rc, out)
			continue
		}
		if want := strconv.FormatFloat(float64(d.Milliseconds())/1000, 'f', -1, 64); m[1] != want {
			t.Errorf("FD_DUMP_TIMEOUT=%q: 스크립트의 기한이 %s초다 - fd 는 %s초(%s)로 읽는다", v, m[1], want, d)
		}
		ceilMin := (d.Milliseconds() + 59_999) / 60_000
		if want := strconv.FormatInt(1440+2*60+ceilMin, 10); m[2] != want {
			t.Errorf("FD_DUMP_TIMEOUT=%q: 헬스체크 기본 한도가 %s분이다 - 간격 + 확인 두 번 + 기한 = %s분이어야 한다", v, m[2], want)
		}
	}
	r.env = nil
	if rc, out := r.run("ok", "config"); rc != 0 || !strings.Contains(out, "기한 1800초") {
		t.Fatalf("FD_DUMP_TIMEOUT 이 없으면 기본 30m 이어야 한다(rc=%d):\n%s", rc, out)
	}
}

// TestDBBackupRejectsNumbersTheShellMisreads 는 정수 설정이 셸이 그대로 읽는 값만 받는지 잰다. 앞자리 0 은 셸 산술이 8진수로
// 읽어(014 는 12, 08 은 오류) 보존 수가 조용히 어긋나거나 회전이 멈춘다. 열 자리를 넘으면 셸 정수 비교가 오류로 지나간다.
func TestDBBackupRejectsNumbersTheShellMisreads(t *testing.T) {
	r := newDBBackupRig(t)
	for _, name := range []string{"FD_DB_BACKUP_KEEP", "FD_DB_BACKUP_EVERY_MIN", "FD_DB_BACKUP_CHECK_SEC", "FD_DB_BACKUP_STALE_MIN"} {
		for _, v := range []string{"014", "08", "0", "00", "-1", "1e3", " 14", "1.5", "1234567890"} {
			r.env = []string{name + "=" + v}
			if rc, out := r.run("ok", "config"); rc != 2 || !strings.Contains(out, name) {
				t.Errorf("%s=%q 를 rc=%d 로 받았다 - 거절(2)해야 한다:\n%s", name, v, rc, out)
			}
		}
		for _, v := range []string{"14", "1", "999999999"} {
			r.env = []string{name + "=" + v}
			if rc, out := r.run("ok", "config"); rc != 0 {
				t.Errorf("%s=%q 를 rc=%d 로 거절했다:\n%s", name, v, rc, out)
			}
		}
	}
}

// TestDBBackupSaysHowToFixAnUnwritableDir 는 덤프 자리에 못 쓸 때 첫 덤프에서 원인(권한)과 호스트에서 고치는 법을 말하는지
// 잰다. 호스트 자리가 없으면 Linux 의 Docker 가 root 소유 0755 로 만들어, 이 uid 로 도는 서비스가 첫 덤프부터 실패한다.
func TestDBBackupSaysHowToFixAnUnwritableDir(t *testing.T) {
	fix := "sudo install -d -m 0700 -o " + strconv.Itoa(os.Getuid()) + " -g " + strconv.Itoa(os.Getgid()) + " <호스트 자리>"
	check := func(t *testing.T, r *dbBackupRig) {
		t.Helper()
		for _, mode := range []string{"once", "loop"} {
			rc, out := r.run("ok", mode)
			if rc != 1 {
				t.Fatalf("못 쓰는 자리에서 %s 가 rc=%d 로 끝났다 - 1 이어야 한다:\n%s", mode, rc, out)
			}
			mustContain(t, mode+" 의 출력", out, "에 쓸 수 없다", "FD_BACKUP_HOST", fix)
		}
		if calls := r.pgDumpCalls(); len(calls) != 0 {
			t.Fatalf("못 쓰는 자리에서 pg_dump 를 불렀다: %v", calls)
		}
	}
	t.Run("뿌리가 파일", func(t *testing.T) {
		r := newDBBackupRig(t)
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		r.root = filepath.Join(file, "daily")
		check(t, r)
	})
	t.Run("읽기만 되는 뿌리", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 는 권한 검사를 건너뛰어 이 갈래를 못 만든다 - 「뿌리가 파일」 갈래가 같은 문구를 잰다")
		}
		r := newDBBackupRig(t)
		r.root = filepath.Join(t.TempDir(), "owned-by-root")
		if err := os.Mkdir(r.root, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(r.root, 0o755) })
		check(t, r)
	})
}

// TestDBBackupStopsMidDumpWithoutLeavingAPart 는 덤프하는 도중 멈추라는 신호(docker stop 의 TERM)를 받으면 pg_dump 를 끊고
// 반쪽 파일을 지우고 곧바로 끝나는지 잰다. 신호를 덤프 끝까지 미루면 stop 의 유예를 넘겨 SIGKILL 로 잘린다.
func TestDBBackupStopsMidDumpWithoutLeavingAPart(t *testing.T) {
	r := newDBBackupRig(t)
	c, out, _ := r.startSlow()
	if err := c.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if rc, got := r.wait(c, out, 5*time.Second); rc != 143 {
		t.Fatalf("TERM 을 받은 스크립트가 rc=%d 로 끝났다 - 143 이어야 한다:\n%s", rc, got)
	}
	if p := r.parts(); len(p) > 0 {
		t.Fatalf("멈춘 뒤 반쪽 파일이 남았다: %v", p)
	}
}
