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

	"github.com/jackc/pgx/v5"

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

// oddLedger 는 이름에 공백 · 홑따옴표 · 줄바꿈 · 겹따옴표가 든 빈 시험 DB 의 URL 과 그 이름이다. 시험이 끝나면 그 이름과
// 되돌리기가 남기는 <이름>_broken 을 지운다.
func oddLedger(t *testing.T) (dsn, name string) {
	t.Helper()
	u, err := url.Parse(storetest.NewBare(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	bare := strings.TrimPrefix(u.Path, "/")
	name = bare + " o'dd\n\"x\""
	admin := pgOpen(t, os.Getenv(storetest.EnvDSN))
	pgExec(t, admin, `ALTER DATABASE `+pgx.Identifier{bare}.Sanitize()+` RENAME TO `+pgx.Identifier{name}.Sanitize())
	t.Cleanup(func() {
		for _, n := range []string{name, name + brokenSuffix} {
			_, _ = admin.db.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+pgx.Identifier{n}.Sanitize()+` WITH (FORCE)`)
		}
	})
	u.Path, u.RawPath = "/"+name, ""
	return u.String(), name
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

// TestPGStepPanicClosesTheTransaction 은 증분 트랜잭션 안의 일이 패닉이어도 트랜잭션이 세션 해제보다 먼저 닫히는지 잰다.
// 안 닫히면 세션 해제(release)의 *sql.Conn Close 가 열린 트랜잭션을 영영 기다려 패닉이 밖으로 못 나가고 실행기가 선다.
func TestPGStepPanicClosesTheTransaction(t *testing.T) {
	ctx := context.Background()
	_, s := pgMigrated(t)
	sess, err := s.openMigrateSession(ctx)
	if err != nil {
		t.Fatalf("실행기 세션: %v", err)
	}
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		defer sess.release() // migrateTo 와 같은 자리
		_ = s.stepTx(ctx, sess, Migration{To: SchemaVersion + 1, Name: "패닉 증분"}, func(ctx context.Context, tx *txConn, _ Migration) error {
			if _, err := tx.ExecContext(ctx, `CREATE TABLE t_panic(x int)`); err != nil {
				return err
			}
			panic("시험 패닉")
		})
	}()
	select {
	case r := <-done:
		if r != "시험 패닉" {
			t.Fatalf("패닉이 그대로 안 나왔다: %v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("증분 패닉 뒤에 세션 해제가 열린 트랜잭션을 기다리며 섰다")
	}
	if pgTableExists(t, s, "t_panic") || pgVersion(t, s) != SchemaVersion {
		t.Fatal("패닉한 증분이 되감기지 않았다")
	}
	for _, k := range []int64{migrateLockKey, globalLockKey} {
		if n := advisoryHolders(t, s, k); n != 0 {
			t.Errorf("패닉 뒤에 잠금 %x 를 쥔 연결 %d개", k, n)
		}
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
	// 사람이 만든 객체 하나는 딸린 것(행 타입 · 배열 · 다중 범위 · 생성자 · 제약 인덱스 · 식별 시퀀스)과 함께 한 번만 센다.
	for name, prep := range map[string]string{
		"다른 스키마의 표":         `CREATE SCHEMA t_other; CREATE TABLE t_other.x(a int)`,
		"기본 키 · 식별 열이 있는 표": `CREATE SCHEMA t_other; CREATE TABLE t_other.x(a int GENERATED ALWAYS AS IDENTITY PRIMARY KEY)`,
		"다른 스키마의 뷰":         `CREATE SCHEMA t_other; CREATE VIEW t_other.v AS SELECT 1 AS a`,
		"다른 스키마의 시퀀스":       `CREATE SCHEMA t_other; CREATE SEQUENCE t_other.s`,
		// 표 없이 함수 · 타입만 있는 업무 DB 도 빈 DB 가 아니다.
		"함수만":           `CREATE SCHEMA t_other; CREATE FUNCTION t_other.f() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		"public 의 프로시저": `CREATE PROCEDURE public.p() LANGUAGE sql AS 'SELECT 1'`,
		"열거형만":          `CREATE SCHEMA t_other; CREATE TYPE t_other.e AS ENUM ('a')`,
		"복합 타입만":        `CREATE SCHEMA t_other; CREATE TYPE t_other.c AS (a int)`,
		"도메인만":          `CREATE SCHEMA t_other; CREATE DOMAIN t_other.d AS int`,
		"범위 타입만":        `CREATE SCHEMA t_other; CREATE TYPE t_other.r AS RANGE (subtype = int8)`,
	} {
		t.Run(name, func(t *testing.T) {
			dsn := storetest.NewBare(t, "")
			s := pgOpen(t, dsn)
			pgExec(t, s, prep)
			if err := Migrate(ctx, dsn, quietLog()); err == nil || !strings.Contains(err.Error(), "schema_version 표가 없는데 객체가 1개 있다") {
				t.Fatalf("남의 객체 하나가 있는 DB 를 빈 DB 로 봤거나 한 번으로 안 셌다: %v", err)
			}
			if pgTableExists(t, s, "machine") {
				t.Fatal("남의 객체가 있는 DB 위에 기준선을 깔았다")
			}
		})
	}
	// 확장이 깐 것(뷰 · 함수 · 타입과 그 배열)은 남의 데이터가 아니다. 준비(bootstrap.sql)의 pg_trgm 이 그 모양이다.
	for name, ext := range map[string]string{"확장이 깐 뷰 · 함수": "pg_buffercache", "확장이 깐 타입": "pg_trgm"} {
		t.Run(name, func(t *testing.T) {
			dsn := storetest.NewBare(t, "")
			s := pgOpen(t, dsn)
			pgExec(t, s, `CREATE EXTENSION `+ext)
			if plan, err := ProbeMigration(ctx, dsn); err != nil || plan.Action != MigrateApply {
				t.Fatalf("확장만 있는 DB 의 판 재기: %+v, %v", plan, err)
			}
		})
	}
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

// dsnDB 는 시험 접속 URL 의 DB 이름이다.
func dsnDB(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(u.Path, "/")
}

// onPath 는 가짜 실행 파일 bin 을 PATH 앞에 둔다.
func onPath(t *testing.T, bin string) {
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// blockingPGDump 는 PATH 앞에 가짜 pg_dump 를 둔다. 가짜는 뜨기 시작했다는 표시를 남기고, 풀릴 때까지 기다렸다가 머리에
// DB 이름 db 를 실은 사용자 정의 형식의 머리만 쓴다(원장에 붙지 않는다). started 는 뜨기 시작한 수, release 는 모두 풀어 준다.
func blockingPGDump(t *testing.T, db string) (started func() int, release func()) {
	t.Helper()
	rec := t.TempDir()
	bin := fakePGDump(t, rec, `: > "`+rec+`/started.$$"
i=0
while [ ! -e "`+rec+`/go" ]; do sleep 0.05; i=$((i+1)); [ "$i" -gt 1200 ] && exit 3; done
`+dumpHead(t, db))
	onPath(t, bin)
	started = func() int {
		m, _ := filepath.Glob(filepath.Join(rec, "started.*"))
		return len(m)
	}
	release = func() { _ = os.WriteFile(filepath.Join(rec, "go"), nil, 0o600) }
	t.Cleanup(release) // 중간에 실패한 시험이 가짜를 붙들어 두지 않게
	return started, release
}

// testStep 은 시험 실행기가 얹는 증분이다: 판 하나를 올리며 표 t_after 하나를 만든다.
var testStep = Migration{To: SchemaVersion + 1, Name: "시험 증분", SQL: `CREATE TABLE t_after(x int)`}

// upgradeRun 은 dsn 의 원장을 판 하나 올리는 실행기다(덤프 자리 dir, 증분 testStep). 운영과 같은 길(openMigrator)로 연다.
func upgradeRun(t *testing.T, dsn, dir string) (*Store, migrateRun) {
	t.Helper()
	m, run := pgMigrator(t, dsn, MigrateOptions{Target: SchemaVersion + 1, BackupDir: dir})
	run.steps = []Migration{testStep}
	return m, run
}

// upgradeRunWith 는 upgradeRun 이고 연결 상한을 lim 으로 줄인 원장으로 연다. 덤프는 운영과 같은 backupPG 다.
func upgradeRunWith(t *testing.T, dsn, dir string, lim pgLimits) (*Store, migrateRun) {
	t.Helper()
	tg := ParseTarget(dsn)
	m, err := openPGWith(tg, quietLog(), lim)
	if err != nil {
		t.Fatalf("openPGWith: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, migrateRun{target: SchemaVersion + 1, steps: []Migration{testStep},
		backup: func(ctx context.Context, sess *migrateSession, from int) (string, error) {
			return m.backupPG(ctx, tg, sess, dir, from)
		}}
}

// TestPGWriteWaitsForTheDump 는 덤프와 증분이 한 잠금 안에 있는지 잰다: 실행기가 판올림 전 덤프를 뜨는 동안 들어온 업무
// 쓰기는 덤프 · 증분 · 판 행이 다 커밋될 때까지 기다린다. 덤프 뒤 · 첫 증분 전에 쓰기가 끼면 덤프에도 없고 옛 판에서
// 커밋된 행이 생긴다 - 그 쓰기가 새 판을 보는 것이 안 끼었다는 증거다.
func TestPGWriteWaitsForTheDump(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	started, release := blockingPGDump(t, dsnDB(t, dsn))
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
	started, release := blockingPGDump(t, dsnDB(t, dsn))
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
	for _, bin := range []string{"pg_dump", "pg_restore", "psql", "sh"} {
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

// runScript 는 되돌리기가 낸 스크립트를 낸 그대로 sh 로 돌린다. 사람은 스크립트를 어느 디렉토리에서든 돌리므로 시험의
// 작업 디렉토리가 아닌 빈 자리에서 돌린다.
func runScript(t *testing.T, script string, env []string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// dbExists 는 클러스터에 그 이름의 DB 가 있는가다.
func dbExists(t *testing.T, s *Store, name string) bool {
	return pgCount(t, s, `SELECT count(*) FROM pg_database WHERE datname = ?`, name) == 1
}

// strayTargetEnv 는 사람의 셸에 남아 있을 수 있는, 대상을 바꾸는 libpq 변수다. 스크립트가 지우지 않으면 접속이 그것을
// 따라가 여기서는 실패한다(없는 DB · 주소가 아닌 hostaddr · 없는 서비스 · 대기 서버만 받기) - 실제로는 다른 서버의 같은
// 이름 DB 를 바꾼다.
var strayTargetEnv = []string{"PGDATABASE=t_no_such_db", "PGHOSTADDR=not-an-address", "PGSERVICE=fd-no-such-service",
	"PGTARGETSESSIONATTRS=standby", "PGLOADBALANCEHOSTS=bogus"}

// undecodableDB 는 클러스터 카탈로그에 UTF-8 로 못 읽는 이름의 DB 를 하나 둔다. LATIN1 DB 에서 지은 이름은 그 바이트(é 는
// 0xE9) 그대로 공유 카탈로그(pg_database)에 남는다 - UTF-8 관리 DB 에서 그 이름에 원장 식별(convert_to)을 돌리면 실패한다.
// 시험이 끝나면 지운다(그 이름은 같은 LATIN1 DB 에서만 적을 수 있다).
func undecodableDB(t *testing.T) {
	t.Helper()
	admin := pgOpen(t, os.Getenv(storetest.EnvDSN))
	latin := "t_latin1_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	pgExec(t, admin, `CREATE DATABASE `+latin+` ENCODING 'LATIN1' LOCALE_PROVIDER libc LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0`)
	t.Cleanup(func() {
		_, _ = admin.db.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+latin+` WITH (FORCE)`)
	})
	u, err := url.Parse(os.Getenv(storetest.EnvDSN))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + latin
	// 클라이언트 인코딩을 UTF-8 로 적어야 서버가 이름을 LATIN1 로 옮긴다. 안 적으면 DB 의 인코딩(LATIN1)을 따라 바이트가
	// 그대로 들어가 UTF-8 로 읽히는 이름이 된다.
	q := u.Query()
	q.Set("client_encoding", "UTF8")
	u.RawQuery = q.Encode()
	l := pgOpen(t, u.String())
	// _broken 으로 끝나게 둔다 - 복원 실패 갈래의 질의는 그 꼬리로 먼저 거르므로, 꼬리가 없으면 식별 식이 이 이름에 안 닿는다.
	bad := pgx.Identifier{"t_bad_é_" + latin + brokenSuffix}.Sanitize()
	pgExec(t, l, `CREATE DATABASE `+bad)
	t.Cleanup(func() { _, _ = l.db.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+bad+` WITH (FORCE)`) })
	var n int
	if err := admin.db.QueryRowContext(context.Background(), `SELECT count(*) FROM pg_catalog.pg_database, pg_catalog.pg_control_system()
		WHERE `+ledgerIDSQL("system_identifier::text", "datname::text")+` = ''`).Scan(&n); err == nil {
		t.Fatal("전제가 깨졌다: 카탈로그 전체에 원장 식별을 돌려도 실패하지 않는다(UTF-8 로 못 읽는 이름이 없다)")
	}
}

// TestPGDumpAndRestoreRehearsal 은 판올림 전 덤프를 실제 pg_dump 로 뜨고, `fd migrate --rollback` 이 낸 스크립트를 낸
// 그대로 돌려 되살린 원장으로 옛 판 바이너리가 다시 뜨는지 잰다. 접속은 사람이 둘 PGHOST · PGPORT · PGUSER 뿐이고,
// 셸에는 대상을 바꾸는 변수가 남아 있다(strayTargetEnv) - 스크립트가 그것을 지우고 명령마다 관리 DB 를 적어야 돈다.
func TestPGDumpAndRestoreRehearsal(t *testing.T) {
	needPGTools(t)
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	pgExec(t, s, `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m-before', 'h', 't', 't')`)
	undecodableDB(t)
	// 덤프 자리를 상대 경로로 준다. 스크립트는 다른 디렉토리에서 돈다(runScript) - 절대 경로가 실려야 덤프를 찾는다.
	base := t.TempDir()
	t.Chdir(base)
	const dir = "backups"
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
	if info, ok := parseDumpName(filepath.Base(out.Backup)); filepath.Dir(out.Backup) != filepath.Join(base, dir) || !ok ||
		info.Ledger != led.ID || info.From != SchemaVersion {
		t.Fatalf("되돌릴 덤프 %q (기대 %q 안의 이 원장 · 판 %d 덤프)", out.Backup, filepath.Join(base, dir), SchemaVersion)
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

	got, err := runScript(t, out.Script, clusterEnv(t, strayTargetEnv...))
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
	undecodableDB(t)
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

	got, err := runScript(t, out.Script, clusterEnv(t, strayTargetEnv...))
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
	if err := os.WriteFile(filepath.Join(dir, dumpName(led.ID, SchemaVersion, time.Now())), []byte(storetest.FakeDump(led.name, 16)), 0o600); err != nil {
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
	// 호스트 · 포트가 여럿이면 명령마다 다른 서버에 붙을 수 있다 - 아무것도 안 바꾸고 멈춘다.
	for _, multi := range []string{"PGHOST=h1,h2", "PGPORT=5432,5433"} {
		got, err := runScript(t, out.Script, clusterEnv(t, multi))
		if err == nil || !strings.Contains(got, "하나씩 적어라") {
			t.Fatalf("%s 로 돌았다: %v\n%s", multi, err, got)
		}
		if dbExists(t, s, name+"_broken") || pgVersion(t, s) != SchemaVersion+1 {
			t.Fatalf("%s 로 돈 스크립트가 원장을 바꿨다", multi)
		}
	}
}

// TestPGRollbackScriptRefusesAnotherLedger 는 스크립트가 붙은 클러스터에서 그 원장을 못 찾으면 아무것도 안 바꾸는지 잰다.
// 다른 서버에 붙은 자리를 식별이 다른 스크립트로 흉내 낸다 - 같은 이름의 DB 가 있어도(이 원장) 이름을 안 바꾼다. 후보를
// 좁히는 OID 가 다른 DB 를 가리켜도(다른 서버에서 우연히 같은 OID) 그 DB 의 식별이 다르므로 그것을 안 바꾼다.
func TestPGRollbackScriptRefusesAnotherLedger(t *testing.T) {
	needPGTools(t)
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	dir := filepath.Join(t.TempDir(), "backups")
	m, run := upgradeRun(t, dsn, dir)
	if err := m.migrateTo(ctx, run); err != nil {
		t.Fatalf("덤프를 뜨는 판올림: %v", err)
	}
	out, err := Rollback(ctx, dsn, dir, quietLog())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	led, err := readLedgerFacts(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(out.Backup)
	if err != nil {
		t.Fatal(err)
	}
	decoyDSN := storetest.NewBare(t, "")
	decoy, err := readLedgerFacts(ctx, pgOpen(t, decoyDSN).db)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	_ = s.Close()
	admin := pgOpen(t, os.Getenv(storetest.EnvDSN))
	name, decoyName := dsnDB(t, dsn), dsnDB(t, decoyDSN)

	cases := []struct {
		name string
		led  ledgerFacts
	}{{"다른 OID 의 DB", ledgerFacts{ID: led.ID, Admin: led.Admin, OID: decoy.OID}}}
	for _, other := range []string{ledgerA, ledgerB} {
		if other != led.ID {
			cases = append(cases, struct {
				name string
				led  ledgerFacts
			}{"다른 원장 " + other, ledgerFacts{ID: other, Admin: led.Admin, OID: led.OID}})
		}
	}
	for _, c := range cases {
		got, err := runScript(t, pgRollbackScript(out.Backup, sum, c.led), clusterEnv(t))
		if err == nil || !strings.Contains(got, "이 원장(식별 "+c.led.ID+")의 DB 가 없다") {
			t.Fatalf("%s 의 스크립트: %v\n%s", c.name, err, got)
		}
		for _, n := range []string{name, decoyName} {
			if dbExists(t, admin, n+brokenSuffix) || !dbExists(t, admin, n) {
				t.Fatalf("%s: 원장을 못 찾은 스크립트가 DB 이름을 바꿨다", c.name)
			}
		}
	}
}

// TestPGOddNameRehearsal 은 이름에 공백 · 따옴표 · 줄바꿈이 든 원장을 실제로 덤프 · 되돌리기 · 복원하는지 잰다. 이름은
// 덤프 머리와 카탈로그에서 바이트 그대로 오가야 한다 - pg_restore -l 은 줄바꿈을 공백으로 바꿔 다른 DB 를 고른다.
func TestPGOddNameRehearsal(t *testing.T) {
	needPGTools(t)
	ctx := context.Background()
	dsn, name := oddLedger(t)
	if err := Migrate(ctx, dsn, quietLog()); err != nil {
		t.Fatalf("빈 DB 이전: %v", err)
	}
	s := pgOpen(t, dsn)
	pgExec(t, s, `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m-odd', 'h', 't', 't')`)
	// 줄바꿈을 공백으로 바꾼 이름의 DB 가 따로 있다 - 스크립트가 그것을 건드리면 안 된다.
	admin := pgOpen(t, os.Getenv(storetest.EnvDSN))
	decoy := strings.ReplaceAll(name, "\n", " ")
	pgExec(t, admin, `CREATE DATABASE `+pgx.Identifier{decoy}.Sanitize())
	t.Cleanup(func() {
		_, _ = admin.db.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+pgx.Identifier{decoy}.Sanitize()+` WITH (FORCE)`)
	})
	dir := filepath.Join(t.TempDir(), "backups")
	m, run := upgradeRun(t, dsn, dir)
	if err := m.migrateTo(ctx, run); err != nil {
		t.Fatalf("덤프를 뜨는 판올림: %v", err)
	}
	out, err := Rollback(ctx, dsn, dir, quietLog())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if strings.Contains(out.Steps+out.Script, name) || strings.Contains(out.Steps+out.Script, decoy) {
		t.Fatalf("절차에 DB 이름이 실렸다:\n%s\n%s", out.Steps, out.Script)
	}
	_ = m.Close()
	_ = s.Close()
	got, err := runScript(t, out.Script, clusterEnv(t, strayTargetEnv...))
	if err != nil || !strings.Contains(got, "복원했다") {
		t.Fatalf("스크립트: %v\n%s", err, got)
	}
	if !dbExists(t, admin, name+brokenSuffix) || !dbExists(t, admin, decoy) || dbExists(t, admin, decoy+brokenSuffix) {
		t.Fatalf("이름 바꾸기가 다른 DB 를 건드렸다: 원장_broken %v · 미끼 %v · 미끼_broken %v",
			dbExists(t, admin, name+brokenSuffix), dbExists(t, admin, decoy), dbExists(t, admin, decoy+brokenSuffix))
	}
	r := pgOpen(t, dsn)
	if v := pgVersion(t, r); v != SchemaVersion || pgTableExists(t, r, "t_after") {
		t.Fatalf("판올림 전으로 안 돌아갔다: 판 %d, t_after %v", v, pgTableExists(t, r, "t_after"))
	}
	if n := pgCount(t, r, `SELECT count(*) FROM machine WHERE id = 'm-odd'`); n != 1 {
		t.Fatalf("판올림 전 행이 없다(%d)", n)
	}
}

// TestPGDumpSeesTheRunnersSnapshot 은 pg_dump 가 실행기 세션이 내보낸 스냅숏만 보는지 잰다. 내보낸 뒤에 커밋된 행은 덤프에
// 없다. 스냅숏이 없는 서버(다른 서버 · 지연 복제본 자리를 없는 식별로 흉내 낸다)와 다른 DB 의 스냅숏이면 pg_dump 가
// 실패하고 덤프에 이름이 안 붙는다.
func TestPGDumpSeesTheRunnersSnapshot(t *testing.T) {
	needPGTools(t)
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	pgExec(t, s, `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m-before', 'h', 't', 't')`)
	sess, err := s.openMigrateSession(ctx)
	if err != nil {
		t.Fatalf("실행기 세션: %v", err)
	}
	defer sess.release()
	snap, err := s.exportSnapshot(ctx, sess)
	if err != nil {
		t.Fatalf("스냅숏: %v", err)
	}
	defer func() { _ = snap.end() }()
	led, err := readLedgerFacts(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	if snap.id == "" || snap.ledger.ID != led.ID || snap.version != SchemaVersion {
		t.Fatalf("스냅숏 %+v (기대 식별 %s · 판 %d)", snap, led.ID, SchemaVersion)
	}
	// 전역 잠금을 안 잡는 쓰기(날 SQL)가 스냅숏 뒤에 커밋한다.
	pgExec(t, s, `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m-after', 'h', 't', 't')`)

	t.Run("내보낸 스냅숏", func(t *testing.T) {
		dir := t.TempDir()
		got, err := dumpPG(ctx, ParseTarget(dsn), realJob(dir, led, snap.id), quietLog())
		if err != nil {
			t.Fatalf("dumpPG: %v", err)
		}
		var data bytes.Buffer
		cmd := exec.Command("pg_restore", "--data-only", "--table=machine", "--file=-", got)
		cmd.Stdout = &data
		if err := cmd.Run(); err != nil {
			t.Fatalf("pg_restore: %v", err)
		}
		if !strings.Contains(data.String(), "m-before") || strings.Contains(data.String(), "m-after") {
			t.Fatalf("덤프가 스냅숏 밖을 봤다(m-before %v · m-after %v)", strings.Contains(data.String(), "m-before"), strings.Contains(data.String(), "m-after"))
		}
	})

	_, os2 := pgMigrated(t)
	osess, err := os2.openMigrateSession(ctx)
	if err != nil {
		t.Fatalf("다른 DB 의 실행기 세션: %v", err)
	}
	defer osess.release()
	osnap, err := os2.exportSnapshot(ctx, osess)
	if err != nil {
		t.Fatalf("다른 DB 의 스냅숏: %v", err)
	}
	defer func() { _ = osnap.end() }()
	for name, id := range map[string]string{"없는 스냅숏": "00000003-000000FF-1", "다른 DB 의 스냅숏": osnap.id} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := dumpPG(ctx, ParseTarget(dsn), realJob(dir, led, id), quietLog())
			if err == nil || !strings.Contains(err.Error(), "스냅숏을 가져오지 못했다") {
				t.Fatalf("%s 로 뜬 덤프: %v", name, err)
			}
			if names := dirNames(t, dir); len(names) != 0 {
				t.Fatalf("실패한 덤프가 남았다: %q", names)
			}
		})
	}
}

// realJob 은 실제 pg_dump 로 원장 led 의 지금 판을 snapshot 으로 뜨는 덤프 한 번이다(봉인은 늘 통과).
func realJob(dir string, led ledgerFacts, snapshot string) dumpJob {
	return dumpJob{dir: dir, bin: "pg_dump", ledger: led, from: SchemaVersion, snapshot: snapshot, lockWait: lockTimeout, seal: sealOK}
}

// TestPGDumpOutlivesTheTxLimits 는 덤프가 연결의 유휴 트랜잭션 상한(idle_in_transaction_session_timeout)과 서버 설정의
// transaction_timeout 보다 오래 걸려도 판을 올리는지 잰다. 스냅숏을 내보낸 트랜잭션은 pg_dump 를 기다리는 동안 문장 없이
// 열려 있다 - 상한이 그대로면 Postgres 가 실행기 연결을 끊어 두 잠금이 사라지고 봉인에서 실패한다. 두 상한을 1초로 줄이고
// pg_dump 자리의 가짜가 그보다 오래 걸린다. 상한은 그 트랜잭션 안에서만 풀려야 한다 - 풀로 돌아간 연결은 다시 상한을 쥔다.
func TestPGDumpOutlivesTheTxLimits(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	const limit = time.Second
	name := dsnDB(t, dsn)
	pgExec(t, s, `ALTER DATABASE `+pgx.Identifier{name}.Sanitize()+` SET transaction_timeout = `+millis(limit))
	onPath(t, fakePGDump(t, t.TempDir(), `sleep 2.5; `+dumpHead(t, name)))
	dir := t.TempDir()
	m, run := upgradeRunWith(t, dsn, dir, pgLimits{lock: lockTimeout, idleInTx: limit})
	m.db.raw.SetMaxOpenConns(1) // 실행기 세션의 연결이 풀로 돌아와 아래 되읽기가 받는다
	start := time.Now()
	if err := m.migrateTo(ctx, run); err != nil {
		t.Fatalf("상한보다 오래 걸린 덤프의 판올림: %v", err)
	}
	if d := time.Since(start); d < 2*limit {
		t.Fatalf("판올림이 %s 만에 끝났다 - 덤프가 상한보다 오래 걸리지 않아 상한을 재지 못했다", d)
	}
	if v := pgVersion(t, s); v != SchemaVersion+1 || !pgTableExists(t, s, "t_after") {
		t.Fatalf("판올림 뒤: 판 %d, t_after %v", v, pgTableExists(t, s, "t_after"))
	}
	if names := dirNames(t, dir); len(names) != 1 || !strings.HasSuffix(names[0], dumpExt) {
		t.Fatalf("백업 자리: %q", names)
	}
	for setting, want := range map[string]string{"idle_in_transaction_session_timeout": millis(limit), "transaction_timeout": millis(limit)} {
		var got string
		if err := m.db.QueryRowContext(ctx, `SELECT setting FROM pg_settings WHERE name = ?`, setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("판올림 뒤 실행기 연결의 %s = %s(기대 %s) - 상한을 트랜잭션 밖에서 풀었다", setting, got, want)
		}
	}
}

// TestPGUpgradeRefusesAnAliasedDump 는 덤프 머리의 DB 이름이 원장의 실제 이름과 다르면 판을 올리지 않는지 잰다. 연결
// 중개기(PgBouncer 등)의 DB 별칭으로 붙으면 머리에는 별칭이 실리고 되돌리기가 그 덤프를 거절한다 - 판올림 때 거절해야
// 복원하는 날에야 아는 일이 없다. 별칭으로 붙은 pg_dump 를 머리에 다른 이름을 싣는 가짜로 흉내 낸다.
func TestPGUpgradeRefusesAnAliasedDump(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	name := dsnDB(t, dsn)
	onPath(t, fakePGDump(t, t.TempDir(), dumpHead(t, name+"_alias")))
	dir := t.TempDir()
	m, run := upgradeRun(t, dsn, dir)
	err := m.migrateTo(ctx, run)
	if err == nil || !strings.Contains(err.Error(), "원래 이름으로 접속") || strings.Contains(err.Error(), name) {
		t.Fatalf("별칭으로 뜬 덤프의 판올림: %v", err)
	}
	if v := pgVersion(t, s); v != SchemaVersion || pgTableExists(t, s, "t_after") {
		t.Fatalf("거절한 판올림이 판을 올렸다: 판 %d, t_after %v", v, pgTableExists(t, s, "t_after"))
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("거절한 덤프가 남았다: %q", names)
	}
}

// TestPGRollbackScriptChecksTheDumpFirst 는 스크립트가 원장 이름을 바꾸기 전에 덤프를 재는지 잰다: 그 자리에 없거나, 스크립트를
// 낸 뒤 내용이 바뀌었거나(sha256), pg_restore 가 목록을 못 읽으면 아무것도 안 바꾸고 멈춘다. 이름부터 바꾸고 복원에서 알면
// 그사이 원장이 제 이름에 없다.
func TestPGRollbackScriptChecksTheDumpFirst(t *testing.T) {
	needPGTools(t)
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	name := dsnDB(t, dsn)
	dir := filepath.Join(t.TempDir(), "backups")
	m, run := upgradeRun(t, dsn, dir)
	if err := m.migrateTo(ctx, run); err != nil {
		t.Fatalf("덤프를 뜨는 판올림: %v", err)
	}
	out, err := Rollback(ctx, dsn, dir, quietLog())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// 머리만 있는 덤프: 되돌리기는 머리만 재므로 스크립트를 내지만 pg_restore 는 목록을 못 읽는다.
	headOnly := t.TempDir()
	if err := os.WriteFile(filepath.Join(headOnly, filepath.Base(out.Backup)), []byte(storetest.FakeDump(name, 16)), 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable, err := Rollback(ctx, dsn, headOnly, quietLog())
	if err != nil {
		t.Fatalf("머리만 있는 덤프의 Rollback: %v", err)
	}
	_ = m.Close()
	_ = s.Close()
	admin := pgOpen(t, os.Getenv(storetest.EnvDSN))
	t.Cleanup(func() {
		_, _ = admin.db.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+pgx.Identifier{name + brokenSuffix}.Sanitize()+` WITH (FORCE)`)
	})
	orig, err := os.ReadFile(out.Backup)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, script, want string
		spoil              func() error
	}{
		{"덤프가 없다", out.Script, "덤프가 그 자리에 없다", func() error { return os.Remove(out.Backup) }},
		{"덤프가 바뀌었다", out.Script, "sha256 이 스크립트를 낸 때와 다르다", func() error { return os.WriteFile(out.Backup, append(orig, 0), 0o600) }},
		{"pg_restore 가 못 읽는다", unreadable.Script, "pg_restore 가 덤프의 목록을 읽지 못한다", func() error { return nil }},
	} {
		if err := c.spoil(); err != nil {
			t.Fatal(err)
		}
		got, err := runScript(t, c.script, clusterEnv(t))
		if err == nil || !strings.Contains(got, c.want) {
			t.Errorf("%s: %v\n%s", c.name, err, got)
		}
		if dbExists(t, admin, name+brokenSuffix) || !dbExists(t, admin, name) {
			t.Fatalf("%s: 덤프를 재기 전에 원장 이름을 바꿨다", c.name)
		}
		if err := os.WriteFile(out.Backup, orig, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPGDumpGivesUpOnATableLock 은 원장 표에 긴 잠금을 쥔 연결이 있으면 덤프가 끝없이 서지 않고, 서버 연결의 lock_timeout 과
// 같은 상한(pg_dump --lock-wait-timeout)에서 실패해 판을 안 올리는지 잰다. 서 있는 동안 실행기는 두 잠금을 쥐고 업무 쓰기를
// 모두 돌려보낸다. 상한이 안 걸리면 잠금을 쥔 쪽이 놓을 때까지 선다 - 시험은 10초 뒤에 놓아 준다.
func TestPGDumpGivesUpOnATableLock(t *testing.T) {
	needPGTools(t)
	ctx := context.Background()
	dsn, s := pgMigrated(t)
	holder, err := s.db.raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `LOCK TABLE machine IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m, run := upgradeRunWith(t, dsn, dir, pgLimits{lock: 500 * time.Millisecond, idleInTx: idleInTxTimeout})
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- m.migrateTo(ctx, run) }()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(10 * time.Second):
		_ = holder.Rollback()
		runErr = <-done
		t.Errorf("덤프가 표 잠금을 10초 넘게 기다렸다(판올림: %v)", runErr)
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "표 잠금을 기다리다 상한(--lock-wait-timeout") {
		t.Fatalf("표 잠금에 선 덤프의 판올림: %v", runErr)
	}
	// 상한은 이 연결의 lock_timeout(500ms)에서 와야 한다 - 고정값(5초)을 받으면 여기서 걸린다.
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("덤프가 표 잠금을 %v 기다렸다 - 서버 연결의 lock_timeout 과 같은 값을 받지 않았다", took)
	}
	_ = holder.Rollback()
	if v := pgVersion(t, s); v != SchemaVersion || pgTableExists(t, s, "t_after") {
		t.Fatalf("실패한 덤프 뒤에 판이 올랐다: 판 %d, t_after %v", v, pgTableExists(t, s, "t_after"))
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("실패한 덤프가 남았다: %q", names)
	}
}
