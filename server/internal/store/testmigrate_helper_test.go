package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// mustMigrate 는 시험이 쓸 DB 를 이 바이너리의 스키마까지 올린다.
//
// ★ 이 헬퍼가 생긴 이유는 설계 §7 처방 ①(적용을 기동에서 분리)이다. 그 전까지 시험은
// Open() 한 번으로 빈 DB 를 만들고 스키마까지 세웠는데, 이제 Open 은 안 맞는 DB 를 **거절**한다.
//
// ★ **판정을 여기서 다시 하지 않는다.** Migrate 가 이미 멱등이라(MigrateNone 이면 아무것도
// 안 한다) 같은 경로에 두 번 불러도 안전하다. 시험마다 "올려야 하나" 를 재게 하면 그 판정이
// 시험 수만큼 복제되고, 복제된 판정은 본체가 바뀔 때 함께 안 바뀐다.
func mustMigrate(t *testing.T, path string) {
	t.Helper()
	if err := Migrate(context.Background(), path, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("DB 적용 실패(%s): %v", path, err)
	}
}

// mustMigrateTo 는 target 판까지만 올린다.
//
// ★ 중간 판을 시험하는 자리가 생겼다: 증분 011 이 item_dependents 를 걷은 뒤로, 010 이
// "값을 비운다"를 보려면 **10판에서 멈춰야** 한다. 최신까지 올리면 표 자체가 없어서
// 010 이 무엇을 했는지 볼 자리가 사라진다.
func mustMigrateTo(t *testing.T, path string, target int) {
	t.Helper()
	if err := MigrateTo(context.Background(), path, target, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("DB 적용 실패(%s, target=%d): %v", path, target, err)
	}
}

// ledgerAtVersion 은 eng("sqlite" · "postgres") 엔진의 빈 원장을 판 v 까지 올려 낸다: 원장 자리(SQLite 파일 경로 · Postgres 접속
// URL)와 그 원장을 연 Store. 증분 하나의 앞뒤를 재는 시험이 그 판에서 행을 심고 raiseLedger 로 올린다.
func ledgerAtVersion(t *testing.T, eng string, v int) (string, *Store) {
	t.Helper()
	ctx := context.Background()
	if eng == "postgres" {
		path := storetest.NewBare(t, "")
		if err := MigrateTo(ctx, path, v, quietLog()); err != nil {
			t.Fatalf("빈 Postgres 원장을 판 %d 로: %v", v, err)
		}
		return path, pgOpen(t, path)
	}
	path := filepath.Join(t.TempDir(), "fd.db")
	mustMigrateTo(t, path, v)
	s, err := openSQLiteReader(ctx, path, ledgerDSN(path), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return path, s
}

// raiseLedger 는 path 원장(두 엔진)을 판 target 까지 올린다. 판올림 전 백업은 막는다 - 이 시험들의 몫이 아니고, Postgres 의
// 덤프는 pg_dump 를 부른다. edit 가 nil 이 아니면 증분 목록을 바꿔 넣는다(증분 하나를 망가뜨려 보는 시험).
func raiseLedger(t *testing.T, path string, target int, edit func([]Migration) []Migration) error {
	t.Helper()
	m, run := pgMigrator(t, path, MigrateOptions{Target: target, BackupDir: t.TempDir()})
	run.backup = func(context.Context, *migrateSession, int) (string, error) { return "시험 덤프", nil }
	if edit != nil {
		run.steps = edit(run.steps)
	}
	return m.migrateTo(context.Background(), run)
}
