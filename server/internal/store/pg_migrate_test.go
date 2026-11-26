package store

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// Postgres 이전 실행기(fd migrate) 시험. 시험마다 FD_TEST_PG 서버에 새 DB 를 받는다 - 빈 DB(storetest.NewBare)에서
// 실행기가 기준선을 까는 길부터 잰다. 템플릿 복제(storetest.New)는 판 행이 없어 실행기가 남의 DB 로 거절한다.

// pgOpen 은 dsn 의 원장을 판 재기 없이 연다(판이 안 맞는 DB 를 들여다보는 자리). 시험이 끝나면 닫는다.
func pgOpen(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := openPG(ParseTarget(dsn), quietLog(), lockTimeout)
	if err != nil {
		t.Fatalf("openPG: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// pgMigrated 는 빈 DB 를 이전 실행기로 지금 판까지 올린 원장이다.
func pgMigrated(t *testing.T) (string, *Store) {
	t.Helper()
	dsn := storetest.NewBare(t, "")
	if err := Migrate(context.Background(), dsn, quietLog()); err != nil {
		t.Fatalf("빈 DB 이전: %v", err)
	}
	return dsn, pgOpen(t, dsn)
}

// pgMigrator 는 이전 실행기가 쓰는 원장과 실행 입력이다(MigrateWith 의 앞 절반). 시험이 증분 · 백업을 바꿔 넣는다.
func pgMigrator(t *testing.T, dsn string, opt MigrateOptions) (*Store, migrateRun) {
	t.Helper()
	m, run, err := openMigrator(dsn, opt, quietLog())
	if err != nil {
		t.Fatalf("openMigrator: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, run
}

// pgVersion 은 원장의 판이다(판 표가 없으면 -1).
func pgVersion(t *testing.T, s *Store) int {
	t.Helper()
	has, v, _, err := readMigrationState(context.Background(), s.db)
	if err != nil {
		t.Fatalf("판 읽기: %v", err)
	}
	if !has {
		return -1
	}
	return v
}

// pgEmpty 는 실행기가 아무것도 안 남겼는가다 - 판 표도 객체도 없다.
func pgEmpty(t *testing.T, s *Store) bool {
	t.Helper()
	has, _, objects, err := readMigrationState(context.Background(), s.db)
	if err != nil {
		t.Fatalf("판 읽기: %v", err)
	}
	return !has && objects == 0
}

func pgTableExists(t *testing.T, s *Store, name string) bool {
	return pgCount(t, s, `SELECT count(*) FROM pg_class WHERE relname = ? AND relkind = 'r'`, name) == 1
}

// notOwnedTables 는 이 DB 의 사용자 표 가운데 소유 역할의 것이 아닌 표 수다.
func notOwnedTables(t *testing.T, s *Store) int64 {
	return pgCount(t, s, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'S') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg_toast%' AND pg_get_userbyid(c.relowner) <> ?`, pg.OwnerRole)
}

// advisoryHolders 는 이 DB 에서 key 의 권고 잠금을 쥔 백엔드 수다. bigint 키는 pg_locks 에 위 32비트(classid) ·
// 아래 32비트(objid) · objsubid=1 로 갈려 찍힌다.
func advisoryHolders(t *testing.T, s *Store, key int64) int64 {
	return pgCount(t, s, `SELECT count(*) FROM pg_locks l JOIN pg_database d ON d.oid = l.database
		WHERE l.locktype = 'advisory' AND l.granted AND d.datname = current_database() AND l.objsubid = 1
		  AND l.classid::bigint = (?::bigint >> 32) AND l.objid::bigint = (?::bigint & 4294967295)`, key, key)
}

func failingBackup(t *testing.T) func(context.Context, int) (string, error) {
	return func(context.Context, int) (string, error) {
		t.Error("백업을 뜨면 안 되는 자리에서 떴다")
		return "", nil
	}
}

// TestPGMigrateLaysBaselineOnEmptyDB 는 빈 DB 에서 `fd migrate` 한 번이 기동 가능한 원장을 만드는지 잰다: 판은 지금 판,
// 표는 소유 역할의 것, 덤프는 안 뜬다(잃을 것이 없다). 다시 돌리면 아무것도 안 한다.
func TestPGMigrateLaysBaselineOnEmptyDB(t *testing.T) {
	ctx := context.Background()
	dsn := storetest.NewBare(t, "")
	m, run := pgMigrator(t, dsn, MigrateOptions{})
	run.backup = failingBackup(t)
	if err := m.migrateTo(ctx, run); err != nil {
		t.Fatalf("빈 DB 이전: %v", err)
	}
	s := pgOpen(t, dsn)
	if v := pgVersion(t, s); v != SchemaVersion {
		t.Fatalf("판 %d, 기대 %d", v, SchemaVersion)
	}
	if n := notOwnedTables(t, s); n != 0 {
		t.Fatalf("소유 역할(%s)의 것이 아닌 표 %d개", pg.OwnerRole, n)
	}
	if err := s.CheckLocale(ctx); err != nil {
		t.Fatalf("로케일 관문: %v", err)
	}
	opened, err := OpenWithLogger(dsn, quietLog())
	if err != nil {
		t.Fatalf("이전한 원장으로 기동하지 못했다: %v", err)
	}
	_ = opened.Close()
	if plan, err := ProbeMigration(ctx, dsn); err != nil || plan.Action != MigrateNone {
		t.Fatalf("판 재기: %+v, %v", plan, err)
	}

	// 다시 돌리면 판이 맞다 - 판 행이 늘지 않는다.
	if err := Migrate(ctx, dsn, quietLog()); err != nil {
		t.Fatalf("다시 돌리기: %v", err)
	}
	if n := pgCount(t, s, `SELECT count(*) FROM schema_version`); n != 1 {
		t.Fatalf("판 행 %d개(기대 1)", n)
	}
}

// TestPGLocaleGateRefusesEverywhere 는 로케일이 틀린 DB 를 네 자리(이전 실행기 · 기동 · 판 재기 · /healthz 의 CheckLocale)가
// 같은 판정으로 거절하는지 잰다. 빈 DB 면 기준선이 통째로 되감겨 아무것도 안 남아야 한다.
func TestPGLocaleGateRefusesEverywhere(t *testing.T) {
	ctx := context.Background()
	dsn := storetest.NewBare(t, "LOCALE_PROVIDER libc LOCALE 'C'")
	err := Migrate(ctx, dsn, quietLog())
	if !IsLocaleRefusal(err) {
		t.Fatalf("이전 실행기가 로케일로 거절하지 않았다: %v", err)
	}
	for _, frag := range []string{"datlocprovider=c", `datctype="C"`, "show_trgm"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("거절 사유에 %q 가 없다: %v", frag, err)
		}
	}
	s := pgOpen(t, dsn)
	if !pgEmpty(t, s) {
		t.Fatal("거절한 기준선이 남았다 - 준비 · 표 · 판 행이 한 트랜잭션으로 되감겨야 한다")
	}

	// 판이 맞게 손으로 깐 원장 - 판만 보면 뜰 수 있는 DB 다.
	pgExec(t, s, pg.Fresh())
	stamp(t, s, SchemaVersion)
	if _, err := OpenWithLogger(dsn, quietLog()); err == nil || !strings.Contains(err.Error(), "DB 로케일") {
		t.Fatalf("기동이 로케일로 거절하지 않았다: %v", err)
	}
	if plan, err := ProbeMigration(ctx, dsn); err != nil || plan.Action != MigrateReject || !strings.Contains(plan.Reason, "datlocprovider=c") {
		t.Fatalf("판 재기: %+v, %v", plan, err)
	}
	if err := Migrate(ctx, dsn, quietLog()); !IsLocaleRefusal(err) {
		t.Fatalf("판이 맞는 원장에서 이전 실행기가 로케일로 거절하지 않았다: %v", err)
	}
	// 판올림은 덤프 · 증분 전에 거절한다 - 거절될 DB 의 덤프를 뜨고 증분을 얹을 까닭이 없다.
	m, run := pgMigrator(t, dsn, MigrateOptions{Target: SchemaVersion + 1})
	run.steps = []Migration{{To: SchemaVersion + 1, Name: "시험 증분", SQL: `CREATE TABLE t_after(x int)`}}
	run.backup = failingBackup(t)
	if err := m.migrateTo(ctx, run); !IsLocaleRefusal(err) {
		t.Fatalf("판올림이 로케일로 거절하지 않았다: %v", err)
	}
	if v := pgVersion(t, s); v != SchemaVersion || pgTableExists(t, s, "t_after") {
		t.Fatalf("로케일로 거절한 판올림이 DB 를 바꿨다: 판 %d, t_after %v", v, pgTableExists(t, s, "t_after"))
	}
	if err := s.CheckLocale(ctx); !IsLocaleRefusal(err) {
		t.Fatalf("CheckLocale: %v", err)
	}
}

// TestPGMigrateSelfLockTurnsAwaySecondRunner 는 실행기끼리 자기 잠금으로 갈리는지 잰다: 쥔 동안 온 실행기는 기다리지 않고
// 곧장 거절되고, 놓은 뒤에는 돈다. 실행이 끝나면 잠금을 놓는다.
func TestPGMigrateSelfLockTurnsAwaySecondRunner(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	unlock, err := s.lockMigrate(ctx)
	if err != nil {
		t.Fatalf("자기 잠금: %v", err)
	}
	m, run := pgMigrator(t, dsn, MigrateOptions{})
	run.backup = failingBackup(t)
	start := time.Now()
	err = m.migrateTo(ctx, run)
	if err == nil || !strings.Contains(err.Error(), "다른 이전 실행기") {
		unlock()
		t.Fatalf("두 번째 실행기: %v", err)
	}
	if d := time.Since(start); d >= lockTimeout {
		t.Errorf("두 번째 실행기가 %s 기다렸다 - 기다리지 않고 거절해야 한다", d)
	}
	unlock()
	if err := m.migrateTo(ctx, run); err != nil {
		t.Fatalf("잠금을 놓은 뒤: %v", err)
	}
	if n := advisoryHolders(t, s, migrateLockKey); n != 0 {
		t.Fatalf("실행이 끝났는데 자기 잠금을 쥔 연결이 %d개", n)
	}
}

// TestPGStepRechecksVersionInsideTheLock 은 증분 트랜잭션이 잠금 안에서 판을 다시 재는지 잰다. 판을 잰 뒤에 다른
// 실행기가 같은 단을 얹었으면 그 단을 또 얹지 않는다. 기준선은 빈 DB 에만 깐다.
func TestPGStepRechecksVersionInsideTheLock(t *testing.T) {
	ctx := context.Background()
	_, s := pgMigrated(t)
	stamp(t, s, SchemaVersion+1)
	err := s.applyStep(ctx, Migration{To: SchemaVersion + 1, Name: "시험 증분", SQL: `CREATE TABLE t_twice(x int)`})
	if err == nil || !strings.Contains(err.Error(), "다른 실행기가 올렸다") {
		t.Fatalf("이미 얹힌 단: %v", err)
	}
	if pgTableExists(t, s, "t_twice") {
		t.Fatal("이미 얹힌 단을 또 얹었다")
	}
	if err := s.applyStep(ctx, pgBaseline); err == nil || !strings.Contains(err.Error(), "빈 DB 에만") {
		t.Fatalf("깔린 DB 위 기준선: %v", err)
	}
}

// TestPGUpgradeNeedsADumpDir 은 기존 데이터 위에 증분을 얹는 판올림이 덤프 자리 없이는 아무것도 안 바꾸는지 잰다.
func TestPGUpgradeNeedsADumpDir(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	m, run := pgMigrator(t, dsn, MigrateOptions{Target: SchemaVersion + 1})
	run.steps = []Migration{{To: SchemaVersion + 1, Name: "시험 증분", SQL: `CREATE TABLE t_after(x int)`}}
	err := m.migrateTo(ctx, run)
	if err == nil || !strings.Contains(err.Error(), "덤프 자리가 없다(--backup-dir)") {
		t.Fatalf("덤프 자리 없는 판올림: %v", err)
	}
	if v := pgVersion(t, s); v != SchemaVersion || pgTableExists(t, s, "t_after") {
		t.Fatalf("거절한 판올림이 DB 를 바꿨다: 판 %d, t_after %v", v, pgTableExists(t, s, "t_after"))
	}
}

// TestPGMigrateBelowBaselineLeavesEmptyDB 는 기준선(18판)보다 낮은 --to 를 빈 DB 에 줬을 때 기준선을 커밋하지 않고
// 거절하는지 잰다. 경로를 백업 · 적용 전에 고르지 않으면 기준선이 커밋된 채 「내려가는 이전은 없다」로 끝난다.
func TestPGMigrateBelowBaselineLeavesEmptyDB(t *testing.T) {
	ctx := context.Background()
	dsn := storetest.NewBare(t, "")
	if err := MigrateTo(ctx, dsn, 5, quietLog()); err == nil || !strings.Contains(err.Error(), "스키마 거절") {
		t.Fatalf("기준선 아래 --to: %v", err)
	}
	if !pgEmpty(t, pgOpen(t, dsn)) {
		t.Fatal("거절했는데 기준선이 남았다")
	}
}

// TestPGDumpAndRestoreRehearsal 은 판올림 전 덤프를 실제 pg_dump 로 뜨고, `fd migrate --rollback` 이 낸 절차 그대로
// 되살려 옛 판 바이너리가 다시 뜨는지 잰다. pg_dump · pg_restore 가 PATH 에 없으면 건너뛴다 - 실행 경계(argv · 환경 ·
// 실패 분류)는 pgdump_test.go 가 가짜 실행 파일로 잰다.
func TestPGDumpAndRestoreRehearsal(t *testing.T) {
	for _, bin := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s 가 없다 - 실제 덤프 · 복원 예행은 건너뛴다", bin)
		}
	}
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	pgExec(t, s, `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m-before', 'h', 't', 't')`)
	dir := filepath.Join(t.TempDir(), "backups")
	m, run := pgMigrator(t, dsn, MigrateOptions{Target: SchemaVersion + 1, BackupDir: dir})
	run.steps = []Migration{{To: SchemaVersion + 1, Name: "시험 증분", SQL: `CREATE TABLE t_after(x int)`}}
	if err := m.migrateTo(ctx, run); err != nil {
		t.Fatalf("덤프를 뜨는 판올림: %v", err)
	}
	if v := pgVersion(t, s); v != SchemaVersion+1 || !pgTableExists(t, s, "t_after") {
		t.Fatalf("판올림 뒤: 판 %d, t_after %v", v, pgTableExists(t, s, "t_after"))
	}

	out, err := Rollback(ctx, dsn, dir, quietLog())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if want := dumpPrefix + strconv.Itoa(SchemaVersion+1) + "-"; filepath.Dir(out.Backup) != dir || !strings.HasPrefix(filepath.Base(out.Backup), want) {
		t.Fatalf("되돌릴 덤프 %q (기대 %q 안의 %s*)", out.Backup, dir, want)
	}

	// 절차 (1) - 서버를 멈추고(이 시험의 연결을 다 닫고) 망가진 DB 의 이름을 바꾼다.
	_ = m.Close()
	_ = s.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	adminURL := os.Getenv(storetest.EnvDSN)
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	quote := func(n string) string { return `"` + strings.ReplaceAll(n, `"`, `""`) + `"` }
	if _, err := admin.ExecContext(ctx, "ALTER DATABASE "+quote(name)+" RENAME TO "+quote(name+"_broken")); err != nil {
		t.Fatalf("(1) 이름 바꾸기: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quote(name+"_broken")+" WITH (FORCE)")
	})

	// 절차 (2) - 덤프에 든 DB 정의로 원래 이름의 DB 를 새로 만들며 복원한다. 접속은 관리 DB(postgres) URL 로, 환경으로만 넘긴다.
	env, err := libpqEnv(ParseTarget(adminURL), os.Environ())
	if err != nil {
		t.Fatalf("관리 접속을 libpq 로 못 넘긴다: %v", err)
	}
	if !strings.Contains(out.Steps, "pg_restore --create --exit-on-error --dbname=postgres") {
		t.Fatalf("절차가 바뀌었다 - 이 예행도 맞춰라: %s", out.Steps)
	}
	cmd := exec.CommandContext(ctx, "pg_restore", "--create", "--exit-on-error", "--dbname=postgres", out.Backup)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("(2) pg_restore: %v\n%s", err, stderr.String())
	}

	// 절차 (3) - 덤프를 뜬 판(지금 판)의 바이너리가 다시 뜬다: 판 · 로케일 관문을 지난다.
	opened, err := OpenWithLogger(dsn, quietLog())
	if err != nil {
		t.Fatalf("(3) 되살린 원장으로 기동하지 못했다: %v", err)
	}
	_ = opened.Close()
	r := pgOpen(t, dsn)
	if v := pgVersion(t, r); v != SchemaVersion || pgTableExists(t, r, "t_after") {
		t.Fatalf("판올림 전으로 안 돌아갔다: 판 %d, t_after %v", v, pgTableExists(t, r, "t_after"))
	}
	if n := pgCount(t, r, `SELECT count(*) FROM machine WHERE id = 'm-before'`); n != 1 {
		t.Fatalf("판올림 전 행이 없다(%d)", n)
	}
	if n := notOwnedTables(t, r); n != 0 {
		t.Fatalf("되살린 표 %d개의 주인이 소유 역할이 아니다", n)
	}
	// 준비의 DB 단위 권한(PUBLIC 의 TEMPORARY 회수)도 덤프의 DB 정의로 돌아와야 한다.
	if n := pgCount(t, r, `SELECT count(*) WHERE has_database_privilege(?, current_database(), 'TEMPORARY')`, pg.AppRole); n != 0 {
		t.Fatalf("되살린 DB 에서 %s 이 임시 표를 만들 수 있다", pg.AppRole)
	}
}
