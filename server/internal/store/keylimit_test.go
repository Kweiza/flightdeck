package store

import (
	"context"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// event_by_project_kind(project, kind, id) 의 키 칸 상한(pg.Column.MaxBytes)을 잠근다: 상한까지 채운 값은 Postgres 의 B-tree 가
// 받고, 넘는 값은 실제로 못 받으며(그래서 관문이 있어야 한다), Postgres 판올림은 넘는 행을 덤프 · 증분 전에 행 좌표와 함께 거절하고,
// 거절이 낸 처방대로 고치면 오른다. 이관의 사전 검사(FaultTooLong)는 importsqlite_test.go 가 잰다.

// incompressible 은 n 바이트의 압축이 안 되는 UTF-8 글이다: 한글 음절을 무작위로 잇고 남는 바이트는 ASCII 로 채운다. Postgres 는
// 큰 인덱스 항목을 압축해 넣으려 하므로, 되풀이가 있는 값(strings.Repeat)은 상한을 재지 못한다.
func incompressible(seed uint64, n int) string {
	r := rand.New(rand.NewPCG(seed, seed+1))
	var b strings.Builder
	for b.Len()+3 <= n {
		b.WriteRune(rune(0xAC00 + r.IntN(11172)))
	}
	for b.Len() < n {
		b.WriteByte(byte('a' + r.IntN(26)))
	}
	return b.String()
}

// 두 칸을 상한까지 채운 행은 인덱스가 받는다. 상한보다 한참 넘는 project 는 인덱스가 거절한다 - 그 거절이 증분 019 를 막던 것이다.
func TestEventKeyCapsFitTheIndex(t *testing.T) {
	s := newPGStore(t, lockTimeout)
	project, kind := incompressible(1, pg.EventProjectMaxBytes), incompressible(2, pg.EventKindMaxBytes)
	if len(project) != pg.EventProjectMaxBytes || len(kind) != pg.EventKindMaxBytes {
		t.Fatalf("시험 값의 길이가 %d · %d 다", len(project), len(kind))
	}
	pgExec(t, s, `INSERT INTO event(at, project, kind, payload) VALUES ('a', ?, ?, '{}')`, project, kind)
	_, err := s.db.ExecContext(context.Background(), `INSERT INTO event(at, project, kind, payload) VALUES ('a', ?, 'k', '{}')`,
		incompressible(3, 2800))
	if err == nil || !strings.Contains(err.Error(), "index row") {
		t.Fatalf("상한을 한참 넘는 키를 인덱스가 받았다(이 상한이 재는 위험이 없다): %v", err)
	}
}

// pgAtBaseline 은 기준선(판 18)에서 멈춘 Postgres 원장이다 - 증분 019 의 인덱스가 없다. 낸 Store 는 쉬는 연결을 쥐지 않는다
// (importTarget 의 ★ - 이관 · 역이관이 그 원장에 붙은 다른 연결을 거절한다).
func pgAtBaseline(t *testing.T) (string, *Store) {
	t.Helper()
	dsn := storetest.NewBare(t, "")
	m, run := pgMigrator(t, dsn, MigrateOptions{Target: pg.BaselineVersion})
	run.backup = failingBackup(t)
	if err := m.migrateTo(context.Background(), run); err != nil {
		t.Fatalf("기준선까지: %v", err)
	}
	_ = m.Close()
	s := pgOpen(t, dsn)
	s.db.raw.SetMaxIdleConns(0)
	return dsn, s
}

// 판 18 원장에 상한을 넘는 키가 있으면 판올림은 덤프를 뜨기 전에 서고, 표 · 칸 · 행 id · 바이트 수를 댄다. 판도 인덱스도 그대로다.
// 거절이 낸 처방(트리거를 끄고 그 칸을 줄이고 켠다)대로 고치면 오른다.
func TestPGUpgradeRefusesAnOverLimitKeyBeforeTheDump(t *testing.T) {
	ctx := context.Background()
	dsn, s := pgAtBaseline(t)
	pgExec(t, s, `INSERT INTO event(at, project, kind, payload) VALUES ('a', 'p', 'item.finish', '{}')`)
	var longProject, longKind int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO event(at, project, kind, payload) VALUES ('b', ?, 'k', '{}') RETURNING id`,
		incompressible(4, 2800)).Scan(&longProject); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `INSERT INTO event(at, project, kind, payload) VALUES ('c', 'p', ?, '{}') RETURNING id`,
		incompressible(5, pg.EventKindMaxBytes+1)).Scan(&longKind); err != nil {
		t.Fatal(err)
	}

	upgrade := func() ([]int, error) {
		m, run := pgMigrator(t, dsn, MigrateOptions{BackupDir: t.TempDir()})
		defer m.Close()
		var dumped []int
		run.backup = func(_ context.Context, _ *migrateSession, from int) (string, error) {
			dumped = append(dumped, from)
			return "시험 덤프", nil
		}
		return dumped, m.migrateTo(ctx, run)
	}
	dumped, err := upgrade()
	if err == nil {
		t.Fatal("상한을 넘는 키가 있는데 판을 올렸다")
	}
	msg := err.Error()
	for _, want := range []string{"인덱스 키 상한을 넘는", "덤프도 뜨지 않았다",
		"event.project 행 " + strconv.FormatInt(longProject, 10) + " 이 2800 바이트(상한 2048)",
		"event.kind 행 " + strconv.FormatInt(longKind, 10) + " 이 513 바이트(상한 512)",
		"DISABLE TRIGGER event_no_update"} {
		if !strings.Contains(msg, want) {
			t.Errorf("거절에 %q 가 없다: %s", want, msg)
		}
	}
	if len(dumped) != 0 {
		t.Errorf("거절 전에 덤프를 떴다: %v", dumped)
	}
	if v := pgVersion(t, s); v != pg.BaselineVersion {
		t.Fatalf("거절했는데 판이 %d 다", v)
	}

	// 처방대로 고친다(시험 서버의 초사용자는 소유 역할의 일을 할 수 있다).
	pgExec(t, s, `ALTER TABLE event DISABLE TRIGGER event_no_update`)
	pgExec(t, s, `UPDATE event SET project = left(project, 600) WHERE id = ?`, longProject)
	pgExec(t, s, `UPDATE event SET kind = left(kind, 100) WHERE id = ?`, longKind)
	pgExec(t, s, `ALTER TABLE event ENABLE TRIGGER event_no_update`)
	dumped, err = upgrade()
	if err != nil {
		t.Fatalf("고친 뒤 판올림: %v", err)
	}
	if v := pgVersion(t, s); v != SchemaVersion || len(dumped) != 1 {
		t.Fatalf("고친 뒤 판 %d · 덤프 %v", v, dumped)
	}
}

// 관문은 Postgres 만 잰다 - SQLite 의 B-tree 는 상한이 없어 같은 행으로 판을 올린다.
func TestKeyLimitGateIsPostgresOnly(t *testing.T) {
	s := newStore(t)
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO event(at, project, kind, payload) VALUES ('a', ?, 'k', '{}')`,
		incompressible(6, 2800)); err != nil {
		t.Fatal(err)
	}
	if err := keyLimitGate(context.Background(), s.db); err != nil {
		t.Fatalf("SQLite 원장을 쟀다: %v", err)
	}
}
