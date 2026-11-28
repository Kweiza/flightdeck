package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// ─────────────────────────────────────────────────────────────────────────────
// `fd migrate` — 설계 §7 처방 ①·③(적용 분리 · 롤백 명령)
// ─────────────────────────────────────────────────────────────────────────────
//
// ★ 이 명령이 없던 동안 §7 의 세 처방 중 ②(백업)만 구현돼 있었다. 적용이 store.Open() 안에
// 있어서 나쁜 증분은 "서버가 안 뜬다" 로 나타났고, 고칠 수단도 같은 바이너리를 다시 띄우는
// 것뿐이었다 — 그 모양을 §7 이 크래시루프라 부른다.

// 빈 자리에 DB 를 세우고 이 바이너리의 스키마까지 올린다.
//
// ★ 판정을 출력으로 검사하지 않고 **Open 이 열리는지**로 검사한다. Open 은 스키마가 안 맞으면
// 거절하므로, 열렸다는 사실 자체가 "이 바이너리가 아는 버전까지 갔다" 의 증거다. 출력 문구를
// 세면 문구를 바꾸는 순간 시험이 빨개지지만 그것은 행동이 아니다.
func TestMigrateBuildsANewDatabaseTheServerCanOpen(t *testing.T) {
	h := newHarness(t)
	fresh := filepath.Join(t.TempDir(), "new.db")

	rc, out := h.run("", "migrate", "--db", fresh)
	if rc != 0 {
		t.Fatalf("적용이 rc=%d 로 끝났다: %s", rc, out)
	}

	s, err := store.Open(fresh)
	if err != nil {
		t.Fatalf("적용했다는데 서버가 그 DB 를 못 연다: %v\n출력: %s", err, out)
	}
	s.Close()
}

// 두 번 불러도 안전하다 — 두 번째는 **아무것도 안 한다**.
//
// ★ 멱등이 깨지면 컨테이너의 one-shot 적용 서비스가 재시작될 때마다 증분을 다시 얹으려 든다.
func TestMigrateIsIdempotent(t *testing.T) {
	h := newHarness(t)
	fresh := filepath.Join(t.TempDir(), "new.db")

	if rc, out := h.run("", "migrate", "--db", fresh); rc != 0 {
		t.Fatalf("첫 적용 실패(rc=%d): %s", rc, out)
	}
	rc, out := h.run("", "migrate", "--db", fresh)
	if rc != 0 {
		t.Fatalf("두 번째 적용이 rc=%d 로 끝났다: %s", rc, out)
	}
	// 이미 맞다는 사실을 **말해야 한다.** 침묵하면 운영자가 두 번 돌았는지 아닌지 모른다.
	if !strings.Contains(out, "이미") {
		t.Errorf("두 번째 적용이 '이미 맞다'를 안 말한다: %s", out)
	}
	s, err := store.Open(fresh)
	if err != nil {
		t.Fatalf("두 번 적용한 DB 를 못 연다: %v", err)
	}
	s.Close()
}

// --to N 은 **그 버전에서 멈춘다.**
//
// ★ 이것이 §7 ①의 실질이다. 나쁜 증분이 N 단에 있으면 N-1 까지만 올려 놓고 서버를 옛
// 바이너리로 계속 돌릴 수 있다. 그 선택지가 없으면 판올림은 전부 아니면 전무다.
func TestMigrateToStopsAtTheRequestedVersion(t *testing.T) {
	h := newHarness(t)
	fresh := filepath.Join(t.TempDir(), "new.db")
	target := store.SchemaVersion - 1

	rc, out := h.run("", "migrate", "--db", fresh, "--to", itoa(int64(target)))
	if rc != 0 {
		t.Fatalf("--to 적용이 rc=%d 로 끝났다: %s", rc, out)
	}

	// ★ 서버는 이 DB 를 **거절해야 한다** — 아직 이 바이너리의 버전이 아니다.
	//   여기서 열리면 --to 가 무시되고 끝까지 올라간 것이다.
	if s, err := store.Open(fresh); err == nil {
		s.Close()
		t.Fatalf("--to %d 로 멈췄다는데 서버가 열었다 — 끝까지 올라갔다", target)
	}

	// 그리고 이어서 끝까지 올릴 수 있어야 한다.
	if rc, out := h.run("", "migrate", "--db", fresh); rc != 0 {
		t.Fatalf("이어 올리기가 rc=%d 로 끝났다: %s", rc, out)
	}
	s, err := store.Open(fresh)
	if err != nil {
		t.Fatalf("이어 올린 DB 를 못 연다: %v", err)
	}
	s.Close()
}

// 지금보다 낮은 --to 는 인덱스만 더한 판을 내린다(기록을 잃지 않는다): 19→18 은 되고, 서버는 그 원장을 (판이 낮아) 거절하고, 다시
// 올리면 연다. 표를 바꾼 판(18→17, 20→19)은 거절하고 판올림 전 사본으로 되돌리는 길을 말한다.
//
// ★ 판 번호가 리터럴이다(SchemaVersion-1 이 아니다). 내릴 수 있는 증분은 019 하나이고(store.TestDownStepsLowerOnlyIndexOnlySteps),
// 그 사실은 뒤에 증분이 더 붙어도 안 변한다 - 020 이 붙은 뒤로 지금 판에서 한 판 내리기는 거절이다.
func TestMigrateToLowersAnIndexOnlyStep(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(t.TempDir(), "fd.db")
	if rc, out := h.run("", "migrate", "--db", path, "--to", "19"); rc != 0 {
		t.Fatalf("판 19 적용 실패(rc=%d): %s", rc, out)
	}
	rc, out := h.run("", "migrate", "--db", path, "--to", "18")
	if rc != 0 || !strings.Contains(out, "맞췄다: 스키마 18판") {
		t.Fatalf("내림이 rc=%d 로 끝났거나 판을 안 말한다: %s", rc, out)
	}
	if s, err := store.Open(path); err == nil {
		s.Close()
		t.Fatal("내렸다는데 서버가 그대로 연다")
	}
	refused := func(to string) {
		t.Helper()
		rc, out := h.run("", "migrate", "--db", path, "--to", to)
		if rc == 0 || !strings.Contains(out, "인덱스만 더한 것이 아니라") || !strings.Contains(out, "fd migrate --rollback") {
			t.Fatalf("--to %s: 표를 바꾼 판을 내렸거나 사유가 없다(rc=%d): %s", to, rc, out)
		}
	}
	refused("17")
	if rc, out := h.run("", "migrate", "--db", path); rc != 0 {
		t.Fatalf("다시 올리기 실패(rc=%d): %s", rc, out)
	}
	refused("19")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("다시 올린 원장을 못 연다(거절한 내림이 원장을 바꿨나): %v", err)
	}
	s.Close()
}

// --rollback 은 판올림 전 백업으로 되돌린다.
//
// ★ §7 이 "롤백 명령" 자리를 비워 두고 문구(RollbackHint)로 메워 온 자리다. 문구는 사람이
// 손으로 복사하는 절차를 낼 뿐이고, 그 손 복사에는 -wal·-shm 을 함께 지우는 단계가 있어서
// 빠뜨리면 되돌린 파일 위에 옛 저널이 얹힌다.
func TestMigrateRollbackRestoresThePreUpgradeBackup(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(t.TempDir(), "fd.db")

	// v1 상태를 만든 뒤 판올림한다 — 그 자리에서 백업이 뜬다.
	if rc, out := h.run("", "migrate", "--db", path, "--to", "1"); rc != 0 {
		t.Fatalf("기반 적용 실패(rc=%d): %s", rc, out)
	}
	if rc, out := h.run("", "migrate", "--db", path); rc != 0 {
		t.Fatalf("판올림 실패(rc=%d): %s", rc, out)
	}
	if s, err := store.Open(path); err != nil {
		t.Fatalf("판올림한 DB 를 못 연다: %v", err)
	} else {
		s.Close()
	}

	rc, out := h.run("", "migrate", "--db", path, "--rollback")
	if rc != 0 {
		t.Fatalf("되돌리기가 rc=%d 로 끝났다: %s", rc, out)
	}
	// 되돌렸으면 서버는 다시 거절해야 한다 — 판올림 전 상태이기 때문이다.
	if s, err := store.Open(path); err == nil {
		s.Close()
		t.Fatal("되돌렸다는데 서버가 그대로 연다 — 아무것도 안 되돌아갔다")
	}
	// 어느 파일에서 되돌렸는지, 덮기 전의 원장을 어디 남겼는지 말해야 한다.
	if !strings.Contains(out, ".bak-") || !strings.Contains(out, "fd.db.pre-rollback-") {
		t.Errorf("되돌린 백업 파일 · 남긴 원장을 안 말한다: %s", out)
	}
}

// 되돌릴 백업이 없으면 **거절한다** — 조용히 성공하면 되돌아간 줄 안다.
func TestMigrateRollbackRefusesWhenThereIsNoBackup(t *testing.T) {
	h := newHarness(t)
	fresh := filepath.Join(t.TempDir(), "new.db")
	if rc, out := h.run("", "migrate", "--db", fresh); rc != 0 {
		t.Fatalf("적용 실패(rc=%d): %s", rc, out)
	}
	rc, out := h.run("", "migrate", "--db", fresh, "--rollback")
	if rc == 0 {
		t.Fatalf("백업이 없는데 되돌리기가 성공했다: %s", out)
	}
	if !strings.Contains(out, "백업") {
		t.Errorf("거절 사유가 백업을 안 말한다: %s", out)
	}
}

// --to 와 --rollback 을 함께 주면 아무것도 하지 않는다(runExport 의 형식 규율과 같다).
func TestMigrateRefusesToAndRollbackTogether(t *testing.T) {
	h := newHarness(t)
	fresh := filepath.Join(t.TempDir(), "new.db")
	rc, out := h.run("", "migrate", "--db", fresh, "--to", "2", "--rollback")
	if rc == 0 {
		t.Fatalf("--to 와 --rollback 을 함께 받았다: %s", out)
	}
	_ = context.Background()
}

// 되돌리기는 **-wal·-shm 을 함께 지운다.**
//
// ★ 이 시험이 따로 있어야 하는 이유는 변이로 확인했다(2026-08-23): Rollback 에서 저널을
// 지우는 루프를 통째로 들어내도 위 TestMigrateRollbackRestoresThePreUpgradeBackup 은 **초록이었다.**
// 그 시험은 "Open 이 거절하는가"만 보는데, 파일 내용이 백업으로 바뀌면 저널이 남아 있어도
// 그 단정은 만족되기 때문이다.
//
// 그런데 이 단계가 되돌리기에서 **가장 빠뜨리기 쉬운 줄**이다 — RollbackHint 가 손 절차로
// 낼 때부터 "옛 -wal 을 남기면 반쯤 적용된 상태가 되살아난다"고 경고해 왔고, 명령을 지은
// 이유의 절반이 사람이 그 단계를 빠뜨린다는 것이었다. 지우는 코드가 있는데 그것을 재는
// 시험이 없으면, 그 코드는 있다는 사실 말고 아무것도 보장하지 않는다.
func TestMigrateRollbackRemovesTheStaleJournal(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(t.TempDir(), "fd.db")

	if rc, out := h.run("", "migrate", "--db", path, "--to", "1"); rc != 0 {
		t.Fatalf("기반 적용 실패(rc=%d): %s", rc, out)
	}
	if rc, out := h.run("", "migrate", "--db", path); rc != 0 {
		t.Fatalf("판올림 실패(rc=%d): %s", rc, out)
	}

	// 판올림 뒤 저널이 남아 있는 상태를 만든다. 실제로는 서버가 죽으면서 남긴다.
	for _, side := range []string{path + "-wal", path + "-shm"} {
		if err := os.WriteFile(side, []byte("옛 저널"), 0o644); err != nil {
			t.Fatalf("전제 구성 실패(%s): %v", side, err)
		}
	}
	// ── 대조가 성립했는지 먼저 단정한다 ──
	for _, side := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(side); err != nil {
			t.Fatalf("전제가 깨졌다 — %s 를 못 만들었다: %v", side, err)
		}
	}

	if rc, out := h.run("", "migrate", "--db", path, "--rollback"); rc != 0 {
		t.Fatalf("되돌리기 실패(rc=%d): %s", rc, out)
	}

	for _, side := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(side); err == nil {
			t.Errorf("되돌린 뒤에도 %s 가 남아 있다 — 이 저널이 백업 위에 얹히면 "+
				"되돌리기가 무효가 되고 '반쯤 적용된' 상태가 되살아난다", filepath.Base(side))
		}
	}
}

// --db 로 준 원장의 판올림이 커밋된 단 뒤에서 서면, 실패 안내가 내는 되돌리기 명령은 **그 원장**을 되돌린다 - 이 환경의 기본 원장
// (FD_DB)이 아니다. 안내의 명령을 셸이 읽는 그대로 갈라(공백 · 작은따옴표가 든 경로) 기본 원장이 따로 있는 환경에서 돌린다.
//
// ★ 맨 위 증분(020)이 만들 표 이름을 먼저 차지해 둔다 - 그 아래 단(019)은 커밋되고 020 이 선다(각 단은 자기 트랜잭션이다).
func TestFailedUpgradeHintRollsBackTheLedgerItNamed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("안내의 명령을 POSIX 셸로 가른다")
	}
	h := newHarness(t)
	version := func(path string) int {
		t.Helper()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var v int
		if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
			t.Fatalf("%s 의 판을 못 읽었다: %v", path, err)
		}
		return v
	}

	// 기본 원장 B: 판올림 전 사본이 있어 --db 없는 되돌리기가 실제로 덮을 수 있는 원장이다.
	b := filepath.Join(t.TempDir(), "fd.db")
	for _, args := range [][]string{{"--to", "1"}, {}} {
		if rc, out := h.run("", append([]string{"migrate", "--db", b}, args...)...); rc != 0 {
			t.Fatalf("기본 원장 준비 실패(rc=%d): %s", rc, out)
		}
	}
	bBytes, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	env := maps.Clone(h.env)
	env[dbEnv] = b

	dir := filepath.Join(t.TempDir(), "a b'c")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "fd.db")
	if rc, out := h.runEnv(env, "", "migrate", "--db", a, "--to", itoa(store.SchemaVersion-2)); rc != 0 {
		t.Fatalf("대상 원장 준비 실패(rc=%d): %s", rc, out)
	}
	selfcheckDBAt(t, a, `CREATE TABLE account(x INTEGER)`)

	rc, out := h.runEnv(env, "", "migrate", "--db", a)
	if rc != 1 {
		t.Fatalf("막힌 판올림이 rc=%d 로 끝났다: %s", rc, out)
	}
	if v := version(a); v != store.SchemaVersion-1 {
		t.Fatalf("전제가 깨졌다 - 커밋된 단 뒤에서 서야 한다(판 %d, 기대 %d): %s", v, store.SchemaVersion-1, out)
	}
	m := regexp.MustCompile("`(fd migrate --rollback [^`]*)`").FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("실패 안내에 되돌리기 명령이 없다: %s", out)
	}
	words, err := exec.Command("sh", "-c", "for w in "+m[1]+"; do printf '%s\\0' \"$w\"; done").Output()
	if err != nil {
		t.Fatalf("안내의 명령을 셸이 못 읽는다(%s): %v", m[1], err)
	}
	argv := strings.Split(strings.TrimSuffix(string(words), "\x00"), "\x00")
	if argv[0] != "fd" || !slices.Contains(argv, a) {
		t.Fatalf("안내의 명령이 막힌 원장 %q 를 한 낱말로 대지 않는다: %q", a, argv)
	}

	if rc, out := h.runEnv(env, "", argv[1:]...); rc != 0 {
		t.Fatalf("안내의 명령이 rc=%d 로 끝났다: %s", rc, out)
	}
	if v := version(a); v != store.SchemaVersion-2 {
		t.Fatalf("막힌 원장이 판 %d 다 - 판올림 전 판 %d 로 돌아가야 한다", v, store.SchemaVersion-2)
	}
	kept, err := filepath.Glob(a + ".pre-rollback-*")
	if err != nil || len(kept) != 1 || version(kept[0]) != store.SchemaVersion-1 {
		t.Fatalf("덮기 전의 원장을 남기지 않았다: %v %v", kept, err)
	}
	if got, err := os.ReadFile(b); err != nil || !bytes.Equal(got, bBytes) {
		t.Fatalf("안내의 명령이 기본 원장을 건드렸다: %v", err)
	}
	if other, _ := filepath.Glob(b + ".pre-rollback-*"); len(other) != 0 {
		t.Fatalf("안내의 명령이 기본 원장을 되돌렸다: %v", other)
	}
}

// Postgres 원장도 같은 명령이 빈 DB 에 기준선을 깔고, 서버가 그것을 연다. 두 번째는 아무것도 안 한다. 접속 URL 의
// 어떤 조각도 출력에 안 실린다(표시값은 postgres 다).
func TestMigrateLaysAPostgresLedgerTheServerCanOpen(t *testing.T) {
	h := newHarness(t)
	dsn := storetest.NewBare(t, "")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")

	rc, first := h.run("", "migrate", "--db", dsn)
	if rc != 0 {
		t.Fatalf("Postgres 적용이 rc=%d 로 끝났다: %s", rc, first)
	}
	s, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("적용했다는데 서버가 그 원장을 못 연다: %v\n출력: %s", err, first)
	}
	s.Close()
	rc, second := h.run("", "migrate", "--db", dsn)
	if rc != 0 || !strings.Contains(second, "이미") {
		t.Fatalf("두 번째 적용(rc=%d)이 '이미 맞다'를 안 말한다: %s", rc, second)
	}
	for _, out := range []string{first, second} {
		if strings.Contains(out, dsn) || strings.Contains(out, name) {
			t.Fatalf("출력에 접속 URL 조각이 실렸다: %s", out)
		}
	}
}

// Postgres 의 --rollback 은 원장에 붙어 그 원장의 가장 최근 덤프를 고르고 복원 스크립트를 내며, 되돌렸다고 말하지 않는다.
// 덤프 자리는 --backup-dir, 없으면 FD_BACKUP_DIR 이다. 출력에 접속 URL 의 조각이 없다.
func TestMigrateRollbackNamesThePostgresDump(t *testing.T) {
	h := newHarness(t)
	dsn := storetest.NewBare(t, "")
	if err := store.Migrate(context.Background(), dsn, nil); err != nil {
		t.Fatalf("이전 실행기: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	rc, out := h.run("", "migrate", "--db", dsn, "--rollback", "--backup-dir", dir)
	if rc == 0 || !strings.Contains(out, "되돌릴 덤프가 없다") {
		t.Fatalf("덤프 없는 되돌리기(rc=%d): %s", rc, out)
	}
	id := regexp.MustCompile(`식별 ([0-9a-f]{16})`).FindStringSubmatch(out)
	if id == nil {
		t.Fatalf("덤프 없는 되돌리기가 원장 식별을 안 말한다: %s", out)
	}
	dump := filepath.Join(dir, fmt.Sprintf("fd-%s-from%d-20261126T160203Z.dump", id[1], store.SchemaVersion))
	if err := os.WriteFile(dump, []byte(storetest.FakeDump(strings.TrimPrefix(u.Path, "/"), 16)), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE schema_version SET version = version + 1`); err != nil { // 덤프 뒤로 판올림이 커밋됐다
		t.Fatal(err)
	}
	env := map[string]string{backupDirEnv: dir}
	for k, v := range h.env {
		env[k] = v
	}
	for _, args := range [][]string{
		{"migrate", "--db", dsn, "--rollback", "--backup-dir", dir},
		{"migrate", "--db", dsn, "--rollback"}, // FD_BACKUP_DIR
	} {
		rc, out := h.runEnv(env, "", args...)
		if rc != 0 {
			t.Fatalf("%q: rc=%d: %s", args, rc, out)
		}
		for _, frag := range []string{"덤프: " + dump, "아직 되돌리지 않았다", "\n#!/bin/sh\n", `pg_restore -h "$PGHOST" -p "$PGPORT" -U "$PGUSER"`} {
			if !strings.Contains(out, frag) {
				t.Errorf("%q: 출력에 %q 가 없다: %s", args, frag, out)
			}
		}
		if strings.Contains(out, "에서 되돌렸다") || strings.Contains(out, strings.TrimPrefix(u.Path, "/")) || strings.Contains(out, u.Host) {
			t.Errorf("%q: 되돌렸다고 말하거나 접속 조각을 실었다: %s", args, out)
		}
	}
}

func TestResolveBackupDir(t *testing.T) {
	fallback := ""
	if fi, err := os.Stat(defaultBackupDir); err == nil && fi.IsDir() {
		fallback = defaultBackupDir
	}
	for _, c := range []struct {
		env        map[string]string
		flag, want string
	}{
		{map[string]string{backupDirEnv: "/from-env"}, "/from-flag", "/from-flag"},
		{map[string]string{backupDirEnv: "/from-env"}, " ", "/from-env"},
		{map[string]string{backupDirEnv: "  "}, "", fallback},
		{nil, "", fallback},
	} {
		if got := resolveBackupDir(envOf(c.env), c.flag); got != c.want {
			t.Errorf("resolveBackupDir(%v, %q) = %q, 기대 %q", c.env, c.flag, got, c.want)
		}
	}
}

// FD_DUMP_TIMEOUT 은 Go 시간 표기의 양수만 받는다. 비면 0(store 의 기본값)이다. 못 읽는 값 · 0 이하는 거절한다 - 기본값으로
// 물러서면 늘렸다고 믿은 기한이 기본값에서 끊기고, 0 은 「기한 없음」이 아니다. 범위(1초 이상 · 밀리초 int 최댓값 이하)도 store 와
// 같은 판정으로 여기서 거절한다.
func TestResolveDumpTimeout(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want time.Duration
		ok   bool
	}{
		{nil, 0, true},
		{map[string]string{dumpTimeoutEnv: "  "}, 0, true},
		{map[string]string{dumpTimeoutEnv: "45m"}, 45 * time.Minute, true},
		{map[string]string{dumpTimeoutEnv: " 2h "}, 2 * time.Hour, true},
		{map[string]string{dumpTimeoutEnv: "30"}, 0, false},
		{map[string]string{dumpTimeoutEnv: "0"}, 0, false},
		{map[string]string{dumpTimeoutEnv: "-5m"}, 0, false},
		{map[string]string{dumpTimeoutEnv: "1s"}, time.Second, true},
		{map[string]string{dumpTimeoutEnv: "30ms"}, 0, false},
		{map[string]string{dumpTimeoutEnv: "999ms"}, 0, false},
		{map[string]string{dumpTimeoutEnv: "600h"}, 0, false},
	} {
		got, err := resolveDumpTimeout(envOf(c.env))
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("resolveDumpTimeout(%v) = %s, %v (기대 %s, 받음 %v)", c.env, got, err, c.want, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), dumpTimeoutEnv) {
			t.Errorf("resolveDumpTimeout(%v) 의 거절이 변수 이름을 안 말한다: %v", c.env, err)
		}
	}
}

// 못 읽는 FD_DUMP_TIMEOUT 이면 `fd migrate` 는 원장을 건드리지 않고 거절한다 - 판이 이미 맞는 날에도 그렇다.
func TestMigrateRefusesAnUnreadableDumpTimeout(t *testing.T) {
	h := newHarness(t)
	fresh := filepath.Join(t.TempDir(), "new.db")
	env := map[string]string{dumpTimeoutEnv: "half an hour"}
	for k, v := range h.env {
		env[k] = v
	}
	rc, out := h.runEnv(env, "", "migrate", "--db", fresh)
	if rc != 2 || !strings.Contains(out, dumpTimeoutEnv) {
		t.Fatalf("못 읽는 덤프 기한(rc=%d): %s", rc, out)
	}
	if _, err := os.Stat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("거절했는데 원장을 만들었다: %v", err)
	}
}

// 범위 밖 FD_DUMP_TIMEOUT 도 판이 이미 맞는 날에 거절한다(종료 코드 2). 판이 맞으면 실행기를 열지 않고 끝나므로, 범위를
// 실행기에만 맡기면 그날은 「이미 맞다」로 지나고 다음 판올림 날에야 실패한다.
func TestMigrateRefusesAnOutOfRangeDumpTimeoutWhenUpToDate(t *testing.T) {
	h := newHarness(t)
	fresh := filepath.Join(t.TempDir(), "new.db")
	if rc, out := h.run("", "migrate", "--db", fresh); rc != 0 {
		t.Fatalf("첫 적용 실패(rc=%d): %s", rc, out)
	}
	env := map[string]string{dumpTimeoutEnv: "30ms"}
	for k, v := range h.env {
		env[k] = v
	}
	rc, out := h.runEnv(env, "", "migrate", "--db", fresh)
	if rc != 2 || !strings.Contains(out, dumpTimeoutEnv) || !strings.Contains(out, "30ms") || strings.Contains(out, "이미 맞다") {
		t.Fatalf("판이 맞는 날의 범위 밖 덤프 기한(rc=%d): %s", rc, out)
	}
}

// Postgres 판올림(--db 없이 FD_DB)은 이 환경의 SQLite 원장 자리(가짜 홈의 ~/.flightdeck/fd.db)에 이관 표지 없는 원장이 남아
// 있으면 기준선을 깔지도 판을 올리지도 않는다 - 이관 판까지만 까는 --to pg.ImportVersion 은 받는다. 표지가 있거나 원장이 없으면 받고, --db 로 준
// 원장은 재지 않는다. 거꾸로 표지가 있으면 SQLite 판올림(compose.yaml 의 `migrate --db /data/fd.db`)이 선다.
func TestMigratePostgresRefusesBesideAnUnmovedSQLiteLedger(t *testing.T) {
	if dataVolume() {
		t.Skip("/data 가 있는 기계다 - 이 환경의 SQLite 원장 자리가 가짜 홈 밖이다")
	}
	ctx := context.Background()
	h := newHarness(t)
	dsn := storetest.NewBare(t, "")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	env := maps.Clone(h.env)
	env[dbEnv] = dsn
	lite := filepath.Join(h.home, ".flightdeck", "fd.db")
	if got := sqliteLedgerPath(envOf(env)); got != lite {
		t.Fatalf("이 환경의 SQLite 원장 자리가 %q 다 - 가짜 홈의 %q 여야 한다", got, lite)
	}
	if err := ensureDBDir(lite); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, lite, quietLogger()); err != nil {
		t.Fatal(err)
	}
	var outs []string
	step := func(want int, wantText string, args ...string) {
		t.Helper()
		rc, out := h.runEnv(env, "", append([]string{"migrate"}, args...)...)
		outs = append(outs, out)
		if rc != want || !strings.Contains(out, wantText) {
			t.Fatalf("fd migrate %v: rc=%d(기대 %d), %q 가 없다:\n%s", args, rc, want, wantText, out)
		}
	}
	plan := func() store.MigrationAction {
		t.Helper()
		p, err := store.ProbeMigration(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		return p.Action
	}

	step(1, "이관을 거치지 않은 새 원장")
	if got := plan(); got != store.MigrateApply {
		t.Fatalf("거절했는데 빈 클러스터에 무엇을 깔았다: %s", got)
	}
	step(0, fmt.Sprintf("올렸다: 스키마 %d판", pg.ImportVersion), "--to", fmt.Sprint(pg.ImportVersion))
	step(1, "이관을 거치지 않은 새 원장")    // 기준선을 깐 뒤 이관 전에 띄운 up
	step(0, "이미 맞다", "--db", dsn) // --db 로 준 원장은 재지 않는다

	if err := os.WriteFile(store.MovedMarker(lite), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	step(0, "이미 맞다")
	step(1, "Postgres 로 옮겼다", "--db", lite) // SQLite 쪽은 표지를 보고 선다

	if err := os.Remove(store.MovedMarker(lite)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(lite, lite+".aside"); err != nil {
		t.Fatal(err)
	}
	step(0, "이미 맞다") // SQLite 원장이 없다
	for _, out := range outs {
		if strings.Contains(out, dsn) || strings.Contains(out, u.Host) {
			t.Fatalf("출력에 접속 URL 조각이 실렸다:\n%s", out)
		}
	}
}

// Postgres 판올림은 되돌리기 창 안이면 인덱스만 더한 것이 아닌 증분을 얹지 않고, 닫히는 때와 넘는 플래그를 댄다. 길과 상관없다:
// 기본 경로(FD_DB)로 부르든 --db 로 원장을 주든(FD_DB 없이) 같은 원장이면 같이 선다 - 그 원장의 판은 따로 잰다. 창의 정본은 원장에
// 이관이 남긴 기록이고(깨진 표지의 오래된 고친 때로 닫히지 않는다), 기록이 없는 원장(옛 판이 옮겼다 - 운영이 그 경우다)은 이 환경의
// 이관 표지로 잰다. 둘 다 없으면 창 밖으로 본다. 창 밖 · 플래그면 올리고, 인덱스만 더한 증분은 창 안에서도 얹는다.
func TestMigratePostgresHoldsInTheRollbackWindow(t *testing.T) {
	if dataVolume() {
		t.Skip("/data 가 있는 기계다 - 이 환경의 SQLite 원장 자리가 가짜 홈 밖이다")
	}
	ctx := context.Background()
	h := newHarness(t)
	lite := sqliteLedgerPath(envOf(h.env))
	if err := ensureDBDir(lite); err != nil {
		t.Fatal(err)
	}
	mark := store.MovedMarker(lite)
	index, other := pgSteps(t)
	for _, c := range []struct {
		name     string
		from, to int           // to 가 0 이면 최신(--to 없음)
		record   time.Duration // 원장의 이관 기록이 이만큼 앞이다(0 이면 없다)
		markAgo  time.Duration // 표지(식별 없는 내용)가 이만큼 앞이다(0 이면 없다, 음수면 깨진 내용 · 그만큼 앞의 고친 때)
		args     []string
		refuse   bool
	}{
		{"기록이 창 안", other.To - 1, 0, time.Hour, 0, nil, true},
		{"기록이 창 안 · 깨진 표지의 고친 때는 창 밖", other.To - 1, 0, time.Hour, -(store.RollbackWindow + 30*time.Hour), nil, true},
		{"기록 없이 표지가 창 안", other.To - 1, 0, 0, time.Hour, nil, true},
		{"기록이 창 안 · 플래그", other.To - 1, 0, time.Hour, 0, []string{"--allow-in-rollback-window"}, false},
		{"기록이 창 밖(73시간)", other.To - 1, 0, store.RollbackWindow + time.Hour, 0, nil, false},
		{"기록도 표지도 없다", other.To - 1, 0, 0, 0, nil, false},
		{"기록이 창 안 · 인덱스만 더한 증분", index.To - 1, index.To, time.Hour, 0, nil, false},
	} {
		for _, viaDB := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/--db=%v", c.name, viaDB), func(t *testing.T) {
				_ = os.Remove(mark)
				dsn := storetest.NewBare(t, "")
				if err := store.MigrateTo(ctx, dsn, c.from, quietLogger()); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("pgx", dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if c.record != 0 {
					if _, err := db.Exec(store.MovedRecordSQL(time.Now().Add(-c.record))); err != nil {
						t.Fatal(err)
					}
				}
				switch {
				case c.markAgo > 0:
					note := fmt.Sprintf(`{"moved_at": %q}`, time.Now().UTC().Add(-c.markAgo).Format(time.RFC3339))
					if err := os.WriteFile(mark, []byte(note), 0o644); err != nil {
						t.Fatal(err)
					}
				case c.markAgo < 0:
					at := time.Now().Add(c.markAgo)
					if err := os.WriteFile(mark, []byte(`{"moved_at":`), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(mark, at, at); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() { _ = os.Remove(mark) })
				env := maps.Clone(h.env)
				env[backupDirEnv] = fakeDumpOnPath(t, dsn)
				args := append([]string{"migrate"}, c.args...)
				if viaDB {
					delete(env, dbEnv)
					args = append(args, "--db", dsn)
				} else {
					env[dbEnv] = dsn
				}
				if c.to != 0 {
					args = append(args, "--to", fmt.Sprint(c.to))
				}
				rc, out := h.runEnv(env, "", args...)
				want := c.to
				if want == 0 {
					want = store.SchemaVersion
				}
				if c.refuse {
					want = c.from
					if rc != 1 || !strings.Contains(out, "되돌리기 창 안이다") || !strings.Contains(out, "(UTC)에 닫힌다") ||
						!strings.Contains(out, "--allow-in-rollback-window") {
						t.Fatalf("창 안의 판올림이 닫히는 때 · 플래그를 대며 서지 않았다(rc=%d):\n%s", rc, out)
					}
				} else if rc != 0 {
					t.Fatalf("올려야 하는데 섰다(rc=%d):\n%s", rc, out)
				}
				var v int
				if err := db.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&v); err != nil || v != want {
					t.Fatalf("원장의 판이 %d 다(%v) - %d 여야 한다:\n%s", v, err, want, out)
				}
			})
		}
	}
}

// fakeDumpOnPath 는 PATH 앞에 원장 dsn 의 이름을 머리에 실은 가짜 pg_dump 를 두고 덤프 자리를 낸다 - 창을 지난 판올림이
// 덤프에서 서지 않고 판까지 오르는지 재려는 것이다(덤프의 실행 경계는 store 의 시험이 잰다).
func fakeDumpOnPath(t *testing.T, dsn string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("가짜 pg_dump 는 sh 스크립트다")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	head := filepath.Join(bin, "head.dump")
	if err := os.WriteFile(head, []byte(storetest.FakeDump(strings.TrimPrefix(u.Path, "/"), 16)), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --file=*) out=\"${a#--file=}\";; esac; done\ncat '" + head + "' > \"$out\"\n"
	if err := os.WriteFile(filepath.Join(bin, "pg_dump"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return t.TempDir()
}

// pgSteps 는 Postgres 증분 가운데 인덱스만 더한 것과 아닌 것 하나씩이다(시험이 그 판으로 창의 두 갈래를 잰다).
func pgSteps(t *testing.T) (index, other pg.Step) {
	t.Helper()
	var hasIndex, hasOther bool
	for _, st := range pg.Steps {
		if _, ok := pg.IndexOnly(st.SQL); ok && !hasIndex {
			index, hasIndex = st, true
		} else if !ok && !hasOther {
			other, hasOther = st, true
		}
	}
	if !hasIndex || !hasOther {
		t.Fatalf("인덱스만 더한 증분(%v)과 아닌 증분(%v)이 다 있어야 한다", hasIndex, hasOther)
	}
	return index, other
}
