package store

import (
	"bytes"
	"context"
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

func failingBackup(t *testing.T) func(context.Context, *migrateSession, int) (string, error) {
	return func(context.Context, *migrateSession, int) (string, error) {
		t.Error("백업을 뜨면 안 되는 자리에서 떴다")
		return "", nil
	}
}

// TestMigrateHintNamesWhoMayRunIt 은 기동 거절의 처방(OpenRefusal)이 Postgres 원장에는 누구로 붙어 `fd migrate` 를 돌릴지
// 말하는지 잰다. 서버 계정(앱 역할)으로 돌리면 준비 · 증분이 권한으로 실패한다. 접속 URL 의 조각은 싣지 않는다.
func TestMigrateHintNamesWhoMayRunIt(t *testing.T) {
	const dsn = "postgres://fd_app_user:s3cret@db.example/ledger"
	for _, a := range []MigrationAction{MigrateApply, MigrateUpgrade} {
		err := OpenRefusal(MigrationPlan{Action: a, Reason: "시험 사유"}, dsn)
		if err == nil || !strings.Contains(err.Error(), "초사용자나 소유 역할("+pg.OwnerRole+")로 접속해 `fd migrate`") {
			t.Fatalf("%s 처방: %v", a, err)
		}
		for _, leak := range []string{"s3cret", "fd_app_user", "db.example", "ledger"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s 처방에 접속 조각 %q 가 실렸다: %v", a, leak, err)
			}
		}
	}
	for _, tg := range []Target{ParseTarget(dsn), ParseTarget("/srv/fd.db")} {
		if h := migrateHint(tg); strings.Contains(h, "—") {
			t.Errorf("처방에 긴 대시: %q", h)
		}
	}
	if err := OpenRefusal(MigrationPlan{Action: MigrateNone}, dsn); err != nil {
		t.Fatalf("none 인데 거절: %v", err)
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
// 곧장 거절되고, 놓은 뒤에는 돈다. 실행이 끝나면 두 잠금을 다 놓는다.
func TestPGMigrateSelfLockTurnsAwaySecondRunner(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	sess, err := s.openMigrateSession(ctx)
	if err != nil {
		t.Fatalf("실행기 세션: %v", err)
	}
	unlock := sess.release
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
	for _, k := range []int64{migrateLockKey, globalLockKey} {
		if n := advisoryHolders(t, s, k); n != 0 {
			t.Fatalf("실행이 끝났는데 잠금 %x 를 쥔 연결이 %d개", k, n)
		}
	}
}

// TestPGStepRechecksVersionInsideTheLock 은 증분 트랜잭션이 잠금 안에서 판을 다시 재는지 잰다. 판을 잰 뒤에 다른
// 실행기가 같은 단을 얹었으면 그 단을 또 얹지 않는다. 기준선은 빈 DB 에만 깐다.
func TestPGStepRechecksVersionInsideTheLock(t *testing.T) {
	ctx := context.Background()
	_, s := pgMigrated(t)
	stamp(t, s, SchemaVersion+1)
	sess, err := s.openMigrateSession(ctx)
	if err != nil {
		t.Fatalf("실행기 세션: %v", err)
	}
	defer sess.release()
	err = s.applyStep(ctx, sess, Migration{To: SchemaVersion + 1, Name: "시험 증분", SQL: `CREATE TABLE t_twice(x int)`})
	if err == nil || !strings.Contains(err.Error(), "다른 실행기가 올렸다") {
		t.Fatalf("이미 얹힌 단: %v", err)
	}
	if pgTableExists(t, s, "t_twice") {
		t.Fatal("이미 얹힌 단을 또 얹었다")
	}
	if err := s.applyStep(ctx, sess, pgBaseline); err == nil || !strings.Contains(err.Error(), "빈 DB 에만") {
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

// TestPGEmptyMeansEverySchema 는 「빈 DB」를 public 만이 아니라 모든 사용자 스키마로 재는지 잰다. 다른 스키마에 객체가
// 있는 DB 를 빈 DB 로 보면 남의 DB 위에 기준선을 깐다. 확장이 깐 객체는 남의 데이터가 아니라 세지 않는다.
func TestPGEmptyMeansEverySchema(t *testing.T) {
	ctx := context.Background()
	for name, prep := range map[string]string{
		"다른 스키마의 표":   `CREATE SCHEMA t_other; CREATE TABLE t_other.x(a int)`,
		"다른 스키마의 뷰":   `CREATE SCHEMA t_other; CREATE VIEW t_other.v AS SELECT 1 AS a`,
		"다른 스키마의 시퀀스": `CREATE SCHEMA t_other; CREATE SEQUENCE t_other.s`,
	} {
		t.Run(name, func(t *testing.T) {
			dsn := storetest.NewBare(t, "")
			s := pgOpen(t, dsn)
			pgExec(t, s, prep)
			if err := Migrate(ctx, dsn, quietLog()); err == nil || !strings.Contains(err.Error(), "schema_version 표가 없는데 객체가") {
				t.Fatalf("남의 객체가 있는 DB 를 빈 DB 로 봤다: %v", err)
			}
			if pgTableExists(t, s, "machine") {
				t.Fatal("남의 객체가 있는 DB 위에 기준선을 깔았다")
			}
		})
	}
	t.Run("확장이 깐 뷰", func(t *testing.T) {
		dsn := storetest.NewBare(t, "")
		s := pgOpen(t, dsn)
		pgExec(t, s, `CREATE EXTENSION pg_buffercache`)
		if plan, err := ProbeMigration(ctx, dsn); err != nil || plan.Action != MigrateApply {
			t.Fatalf("확장만 있는 DB 의 판 재기: %+v, %v", plan, err)
		}
	})
}

// TestPGServerAccountCanJudgeTheLedger 는 서버 계정(앱 역할의 구성원 - 초사용자도 소유 역할도 아니다)으로 붙어도 기동의 판
// 재기(planOpen)와 /healthz 의 로케일 재기(CheckLocale)가 이전 실행기와 같은 답을 내는지 잰다. 카탈로그를 못 읽어 빈 DB ·
// 읽기 실패로 보이면 운영 서버만 뜨지 않는다.
func TestPGServerAccountCanJudgeTheLedger(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	role := "t_srv_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	pgExec(t, s, `CREATE ROLE `+role+` LOGIN IN ROLE `+pg.AppRole)
	t.Cleanup(func() { _, _ = s.db.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+role) })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.User(role)
	app := u.String()

	if plan, err := ProbeMigration(ctx, app); err != nil || plan.Action != MigrateNone {
		t.Fatalf("서버 계정의 판 재기: %+v, %v", plan, err)
	}
	opened, err := OpenWithLogger(app, quietLog())
	if err != nil {
		t.Fatalf("서버 계정으로 기동하지 못했다: %v", err)
	}
	defer opened.Close()
	if err := opened.CheckLocale(ctx); err != nil {
		t.Fatalf("서버 계정의 로케일 재기: %v", err)
	}
	var who string
	if err := opened.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&who); err != nil || who != role {
		t.Fatalf("서버 계정으로 붙지 않았다: %q, %v", who, err)
	}
}

// blockingPGDump 는 PATH 앞에 가짜 pg_dump 를 둔다. 가짜는 뜨기 시작했다는 표시를 남기고, 풀릴 때까지 기다렸다가 사용자
// 정의 형식의 머리만 쓴다(원장에 붙지 않는다). started 는 뜨기 시작한 수, release 는 모두 풀어 준다.
func blockingPGDump(t *testing.T) (started func() int, release func()) {
	t.Helper()
	rec := t.TempDir()
	bin := fakePGDump(t, rec, `: > "`+rec+`/started.$$"
i=0
while [ ! -e "`+rec+`/go" ]; do sleep 0.05; i=$((i+1)); [ "$i" -gt 1200 ] && exit 3; done
printf 'PGDMP-fake' > "$out"`)
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	started = func() int {
		m, _ := filepath.Glob(filepath.Join(rec, "started.*"))
		return len(m)
	}
	release = func() { _ = os.WriteFile(filepath.Join(rec, "go"), nil, 0o600) }
	t.Cleanup(release) // 중간에 실패한 시험이 가짜를 붙들어 두지 않게
	return started, release
}

// upgradeRun 은 dsn 의 원장을 판 하나 올리는 실행기다(덤프 자리 dir). 증분은 표 t_after 하나를 만든다.
func upgradeRun(t *testing.T, dsn, dir string) (*Store, migrateRun) {
	t.Helper()
	m, run := pgMigrator(t, dsn, MigrateOptions{Target: SchemaVersion + 1, BackupDir: dir})
	run.steps = []Migration{{To: SchemaVersion + 1, Name: "시험 증분", SQL: `CREATE TABLE t_after(x int)`}}
	return m, run
}

// TestPGWriteWaitsForTheDump 는 덤프와 증분이 한 잠금 안에 있는지 잰다: 실행기가 판올림 전 덤프를 뜨는 동안 들어온 업무
// 쓰기는 덤프 · 증분 · 판 행이 다 커밋될 때까지 기다린다. 덤프 뒤 · 첫 증분 전에 쓰기가 끼면 덤프에도 없고 옛 판에서
// 커밋된 행이 생긴다 - 그 쓰기가 새 판을 보는 것이 안 끼었다는 증거다.
func TestPGWriteWaitsForTheDump(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	started, release := blockingPGDump(t)
	dir := t.TempDir()
	m, run := upgradeRun(t, dsn, dir)
	aDone := make(chan error, 1)
	go func() { aDone <- m.migrateTo(ctx, run) }()
	waitUntil(t, "덤프가 뜬다", func() bool { return started() == 1 })
	for _, k := range []int64{migrateLockKey, globalLockKey} {
		if n := advisoryHolders(t, s, k); n != 1 {
			t.Errorf("덤프 도중 잠금 %x 를 쥔 연결 %d개(기대 1 - 실행기 세션)", k, n)
		}
	}

	w, err := openPG(ParseTarget(dsn), quietLog(), time.Minute)
	if err != nil {
		t.Fatalf("업무 쓰기 연결: %v", err)
	}
	defer w.Close()
	var version int64
	wDone := make(chan error, 1)
	go func() {
		wDone <- w.Tx(ctx, func(x *Tx) error {
			if err := x.tx.QueryRowContext(x.ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil {
				return err
			}
			return insertMachine(x, "m-during-dump")
		})
	}()
	waitUntil(t, "업무 쓰기가 전역 잠금을 기다린다", func() bool { return lockWaiters(t, s) == 1 })
	select {
	case err := <-wDone:
		t.Fatalf("덤프를 뜨는 동안 업무 쓰기가 끝났다: %v", err)
	default:
	}
	release()
	if err := <-aDone; err != nil {
		t.Fatalf("판올림: %v", err)
	}
	if err := <-wDone; err != nil {
		t.Fatalf("기다린 업무 쓰기: %v", err)
	}
	if version != int64(SchemaVersion+1) {
		t.Fatalf("덤프 도중에 들어온 쓰기가 판 %d 에서 커밋됐다(기대 %d) - 덤프와 증분 사이에 끼었다", version, SchemaVersion+1)
	}
	if names := dirNames(t, dir); len(names) != 1 || !strings.HasSuffix(names[0], dumpExt) {
		t.Fatalf("백업 자리: %q", names)
	}
}

// TestPGLostRunnerFailsClosed 는 덤프 도중 실행기 세션이 끊긴 자리를 잰다(pg_terminate_backend). 잠금은 세션과 함께
// 사라지므로 다음 실행기가 곧장 돈다 - 끊긴 실행기의 pg_dump 는 그것을 모른 채 계속 돈다. 끊긴 실행기는 봉인(sealDump)에서
// 실패해 덤프에 최종 이름을 안 붙이고(닫힌 실패) 증분도 안 얹는다. 판은 다음 실행기가 한 번만 올린다.
func TestPGLostRunnerFailsClosed(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	started, release := blockingPGDump(t)
	dir := t.TempDir()
	runner := func() <-chan error {
		m, run := upgradeRun(t, dsn, dir)
		done := make(chan error, 1)
		go func() { done <- m.migrateTo(ctx, run) }()
		return done
	}

	aDone := runner()
	waitUntil(t, "A 의 덤프가 뜬다", func() bool { return started() == 1 })
	pid := pgCount(t, s, `SELECT l.pid::bigint FROM pg_locks l JOIN pg_database d ON d.oid = l.database
		WHERE l.locktype = 'advisory' AND l.granted AND d.datname = current_database() AND l.objsubid = 1
		  AND l.classid::bigint = (?::bigint >> 32) AND l.objid::bigint = (?::bigint & 4294967295)`, migrateLockKey, migrateLockKey)
	if n := pgCount(t, s, `SELECT count(*) WHERE pg_terminate_backend(?::int)`, pid); n != 1 {
		t.Fatalf("실행기 세션(pid %d)을 끊지 못했다", pid)
	}
	waitUntil(t, "A 의 잠금이 세션과 함께 사라진다", func() bool {
		return advisoryHolders(t, s, migrateLockKey) == 0 && advisoryHolders(t, s, globalLockKey) == 0
	})

	bDone := runner()
	waitUntil(t, "B 의 덤프가 A 의 덤프와 함께 뜬다", func() bool { return started() == 2 })
	release()
	aErr, bErr := <-aDone, <-bDone
	if aErr == nil || !strings.Contains(aErr.Error(), "덤프에 최종 이름을 붙이지 않는다") {
		t.Fatalf("잠금을 잃은 실행기: %v", aErr)
	}
	if bErr != nil {
		t.Fatalf("다음 실행기: %v", bErr)
	}
	if v := pgVersion(t, s); v != SchemaVersion+1 || !pgTableExists(t, s, "t_after") {
		t.Fatalf("판올림 뒤: 판 %d, t_after %v", v, pgTableExists(t, s, "t_after"))
	}
	if n := pgCount(t, s, `SELECT count(*) FROM schema_version WHERE version = ?`, SchemaVersion+1); n != 1 {
		t.Fatalf("판 %d 행 %d개(기대 1)", SchemaVersion+1, n)
	}
	if names := dirNames(t, dir); len(names) != 1 || !strings.HasSuffix(names[0], dumpExt) {
		t.Fatalf("백업 자리에 다음 실행기의 덤프 하나만 있어야 한다: %q", names)
	}
}

// needPGTools 는 실제 덤프 · 복원 예행에 필요한 도구가 PATH 에 없으면 건너뛴다. 실행 경계(argv · 환경 · 실패 분류)는
// pgdump_test.go 가 가짜 실행 파일로 잰다.
func needPGTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"pg_dump", "pg_restore", "psql", "sh", "sed"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s 가 없다 - 실제 덤프 · 복원 예행은 건너뛴다", bin)
		}
	}
}

// clusterEnv 는 사람이 되돌리기 스크립트 앞에 둘 환경이다: 원장 클러스터(FD_TEST_PG)의 PGHOST · PGPORT · PGUSER(와 있으면
// PGPASSWORD). 그 밖의 PG 변수는 물려주지 않는다. extra 가 덧붙는다.
func clusterEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	u, err := url.Parse(os.Getenv(storetest.EnvDSN))
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "PGHOST=" + u.Hostname(), "PGPORT=" + u.Port(),
		"PGUSER=" + u.User.Username()}
	if pw, ok := u.User.Password(); ok {
		env = append(env, "PGPASSWORD="+pw)
	}
	return append(env, extra...)
}

// runScript 는 되돌리기가 낸 스크립트를 낸 그대로 sh 로 돌린다.
func runScript(t *testing.T, script string, env []string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// dbExists 는 클러스터에 그 이름의 DB 가 있는가다.
func dbExists(t *testing.T, s *Store, name string) bool {
	return pgCount(t, s, `SELECT count(*) FROM pg_database WHERE datname = ?`, name) == 1
}

// TestPGDumpAndRestoreRehearsal 은 판올림 전 덤프를 실제 pg_dump 로 뜨고, `fd migrate --rollback` 이 낸 스크립트를 낸
// 그대로 돌려 되살린 원장으로 옛 판 바이너리가 다시 뜨는지 잰다. 접속은 사람이 둘 PGHOST · PGPORT · PGUSER 뿐이고,
// PGDATABASE 는 없는 DB 로 둔다 - 관리 DB 를 안 적은 명령이 있으면 거기서 실패한다.
func TestPGDumpAndRestoreRehearsal(t *testing.T) {
	needPGTools(t)
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	pgExec(t, s, `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m-before', 'h', 't', 't')`)
	dir := filepath.Join(t.TempDir(), "backups")
	m, run := upgradeRun(t, dsn, dir)
	if err := m.migrateTo(ctx, run); err != nil {
		t.Fatalf("덤프를 뜨는 판올림: %v", err)
	}
	if v := pgVersion(t, s); v != SchemaVersion+1 || !pgTableExists(t, s, "t_after") {
		t.Fatalf("판올림 뒤: 판 %d, t_after %v", v, pgTableExists(t, s, "t_after"))
	}
	led, err := readLedgerFacts(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Rollback(ctx, dsn, dir, quietLog())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if info, ok := parseDumpName(filepath.Base(out.Backup)); filepath.Dir(out.Backup) != dir || !ok || info.Ledger != led.ID || info.From != SchemaVersion {
		t.Fatalf("되돌릴 덤프 %q (기대 %q 안의 이 원장 · 판 %d 덤프)", out.Backup, dir, SchemaVersion)
	}

	// 서버를 멈춘다(이 시험의 연결을 다 닫는다). 관리 연결은 클러스터의 관리 DB 에 붙는다.
	_ = m.Close()
	_ = s.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	admin := pgOpen(t, os.Getenv(storetest.EnvDSN))
	t.Cleanup(func() {
		_, _ = admin.db.ExecContext(context.Background(), `DROP DATABASE IF EXISTS "`+name+`_broken" WITH (FORCE)`)
	})

	got, err := runScript(t, out.Script, clusterEnv(t, "PGDATABASE=t_no_such_db"))
	if err != nil || !strings.Contains(got, "복원했다") {
		t.Fatalf("스크립트: %v\n%s", err, got)
	}
	if !dbExists(t, admin, name+"_broken") {
		t.Fatal("망가진 DB 가 _broken 으로 남지 않았다")
	}

	// 덤프를 뜬 판(지금 판)의 바이너리가 다시 뜬다: 판 · 로케일 관문을 지난다.
	opened, err := OpenWithLogger(dsn, quietLog())
	if err != nil {
		t.Fatalf("되살린 원장으로 기동하지 못했다: %v", err)
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
	if l, err := readLedgerFacts(ctx, r.db); err != nil || l.ID != led.ID {
		t.Fatalf("되살린 원장의 식별이 바뀌었다: %+v, %v (기대 %s) - 다음 되돌리기가 이 원장의 덤프를 못 찾는다", l, err, led.ID)
	}
}

// TestPGRollbackScriptPutsTheLedgerBackOnFailure 는 복원이 도중에 실패하면 스크립트가 반쪽 DB 를 지우고 망가진 원장을 제
// 이름으로 되돌리는지 잰다. 덤프 뒤에 지운 역할에 준 권한이 덤프에 들어 있어, pg_restore 는 DB 를 만든 뒤 권한 단계에서
// 실패한다(--exit-on-error).
func TestPGRollbackScriptPutsTheLedgerBackOnFailure(t *testing.T) {
	needPGTools(t)
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	role := "t_gone_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	pgExec(t, s, `CREATE ROLE `+role)
	t.Cleanup(func() { _, _ = s.db.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+role) })
	pgExec(t, s, `GRANT SELECT ON machine TO `+role)
	dir := filepath.Join(t.TempDir(), "backups")
	m, run := upgradeRun(t, dsn, dir)
	if err := m.migrateTo(ctx, run); err != nil {
		t.Fatalf("덤프를 뜨는 판올림: %v", err)
	}
	pgExec(t, s, `REVOKE SELECT ON machine FROM `+role)
	pgExec(t, s, `DROP ROLE `+role)
	out, err := Rollback(ctx, dsn, dir, quietLog())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	_ = m.Close()
	_ = s.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	admin := pgOpen(t, os.Getenv(storetest.EnvDSN))

	got, err := runScript(t, out.Script, clusterEnv(t, "PGDATABASE=t_no_such_db"))
	if err == nil || !strings.Contains(got, "복원이 실패했다") {
		t.Fatalf("실패해야 할 복원: %v\n%s", err, got)
	}
	if dbExists(t, admin, name+"_broken") {
		t.Fatal("실패한 복원 뒤에 _broken 이 남았다 - 원장이 제 이름으로 안 돌아왔다")
	}
	r := pgOpen(t, dsn)
	if v := pgVersion(t, r); v != SchemaVersion+1 || !pgTableExists(t, r, "t_after") {
		t.Fatalf("제 이름으로 돌아온 DB 가 원래 원장이 아니다: 판 %d, t_after %v", v, pgTableExists(t, r, "t_after"))
	}
}

// TestPGRollbackScriptStopsWithoutCluster 는 사람이 대상 클러스터를 안 적었으면 스크립트가 아무것도 안 바꾸고 첫 줄에서
// 멈추는지 잰다. libpq 기본값(로컬 소켓의 기본 서버)으로 흘러가면 원장이 원격인데 엉뚱한 서버의 같은 이름 DB 를 바꾼다.
func TestPGRollbackScriptStopsWithoutCluster(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh 가 없다")
	}
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	led, err := readLedgerFacts(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dumpName(led.ID, SchemaVersion, time.Now())), []byte(dumpMagic+"-x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp(t, s, SchemaVersion+1)
	out, err := Rollback(ctx, dsn, dir, quietLog())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	for _, drop := range []string{"PGHOST=", "PGPORT=", "PGUSER="} {
		var env []string
		for _, kv := range clusterEnv(t) {
			if !strings.HasPrefix(kv, drop) {
				env = append(env, kv)
			}
		}
		got, err := runScript(t, out.Script, env)
		if err == nil || !strings.Contains(got, "원장 클러스터의") {
			t.Fatalf("%s 없이 돌았다: %v\n%s", drop, err, got)
		}
		if dbExists(t, s, name+"_broken") || pgVersion(t, s) != SchemaVersion+1 {
			t.Fatalf("%s 없이 돈 스크립트가 원장을 바꿨다", drop)
		}
	}
}
