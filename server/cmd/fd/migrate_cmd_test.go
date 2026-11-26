package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/store"
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
	// 어느 파일에서 되돌렸는지 말해야 한다.
	if !strings.Contains(out, ".bak-") {
		t.Errorf("되돌린 백업 파일을 안 말한다: %s", out)
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
	if err := os.WriteFile(dump, []byte("PGDMP-test"), 0o600); err != nil {
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
