package store

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// event_by_project_kind(project, kind, id) 의 키 칸 상한(pg.Column.MaxBytes)을 잠근다: 상한까지 채운 값은 Postgres 의 B-tree 가
// 받고, 넘는 값은 실제로 못 받으며(그래서 관문이 있어야 한다), Postgres 판올림은 넘는 행을 덤프 · 증분 전에 행 좌표와 함께 거절하되
// 행을 고치라고 하지 않고, 새 행은 두 엔진의 쓰기 관문이 같은 상한으로 막는다. 이관의 사전 검사(FaultTooLong)는 importsqlite_test.go,
// 상한을 두지 않는 역이관 · 대조는 exportsqlite_test.go 가 잰다.

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
// 행을 고치라고 하지 않는다(트리거를 끄고 감사 행을 자르는 처방은 원장을 SQLite 원본 · 덤프와 다르게 만든다) - 이슈로 알려 달라고 한다.
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
	before := pgString(t, s, `SELECT string_agg(md5(project || kind), ',' ORDER BY id) FROM event`)

	m, run := pgMigrator(t, dsn, MigrateOptions{BackupDir: t.TempDir()})
	defer m.Close()
	var dumped []int
	run.backup = func(_ context.Context, _ *migrateSession, from int) (string, error) {
		dumped = append(dumped, from)
		return "시험 덤프", nil
	}
	err := m.migrateTo(ctx, run)
	if err == nil {
		t.Fatal("상한을 넘는 키가 있는데 판을 올렸다")
	}
	msg := err.Error()
	for _, want := range []string{"인덱스 키 상한을 넘는", "덤프도 뜨지 않았다",
		"원장은 판 " + strconv.Itoa(pg.BaselineVersion) + " 그대로다",
		"event.project 행 " + strconv.FormatInt(longProject, 10) + " 이 2800 바이트(상한 2048)",
		"event.kind 행 " + strconv.FormatInt(longKind, 10) + " 이 513 바이트(상한 512)",
		"행을 고치지 않는다", "이슈로 알려 달라"} {
		if !strings.Contains(msg, want) {
			t.Errorf("거절에 %q 가 없다: %s", want, msg)
		}
	}
	for _, bad := range []string{"DISABLE TRIGGER", "UPDATE event", "left("} {
		if strings.Contains(msg, bad) {
			t.Errorf("거절이 감사 행을 고치는 처방(%q)을 냈다: %s", bad, msg)
		}
	}
	if len(dumped) != 0 {
		t.Errorf("거절 전에 덤프를 떴다: %v", dumped)
	}
	if v := pgVersion(t, s); v != pg.BaselineVersion {
		t.Fatalf("거절했는데 판이 %d 다", v)
	}
	if after := pgString(t, s, `SELECT string_agg(md5(project || kind), ',' ORDER BY id) FROM event`); after != before {
		t.Fatal("거절했는데 감사 행이 바뀌었다")
	}
}

// 새 행은 두 엔진에서 같은 상한으로 막는다(eventKeyRefusal): 이벤트 한 줄 넣기와 프로젝트 등록이 상한을 넘는 project · kind 를
// *KeyTooLongError 로 거절하고 아무것도 안 쓴다. 상한까지는 받는다.
func TestEventKeysOverTheCapAreRefusedOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		count := func() int {
			var n int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM event WHERE kind LIKE 'cap.%'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if err := s.TryLogEvent(ctx, "cap.fits", incompressible(7, pg.EventProjectMaxBytes), "", nil); err != nil {
			t.Fatalf("상한까지 채운 project 를 거절했다: %v", err)
		}
		for _, c := range []struct {
			name, kind, project, column string
		}{
			{"project", "cap.long", incompressible(8, pg.EventProjectMaxBytes+1), "project"},
			{"kind", "cap." + incompressible(9, pg.EventKindMaxBytes), "p", "kind"},
		} {
			err := s.TryLogEvent(ctx, c.kind, c.project, "", nil)
			var long *KeyTooLongError
			if !errors.As(err, &long) || long.Column != c.column {
				t.Fatalf("%s 가 상한을 넘는 이벤트: %v(KeyTooLongError %s 를 기대)", c.name, err, c.column)
			}
		}
		if n := count(); n != 1 {
			t.Fatalf("거절했는데 이벤트가 %d 줄이다(1 줄이어야 한다)", n)
		}
		err := s.UpsertProject(ctx, model.Project{ID: incompressible(10, pg.EventProjectMaxBytes+1), Path: "/r"})
		var long *KeyTooLongError
		if !errors.As(err, &long) || long.Column != "project" {
			t.Fatalf("상한을 넘는 프로젝트 id 를 등록했다: %v", err)
		}
		var projects int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM project`).Scan(&projects); err != nil || projects != 0 {
			t.Fatalf("거절했는데 프로젝트가 %d 개다: %v", projects, err)
		}
	})
}

// 관문은 Postgres 만 잰다 - SQLite 의 B-tree 는 상한이 없어 같은 행으로 판을 올린다.
func TestKeyLimitGateIsPostgresOnly(t *testing.T) {
	s := newStore(t)
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO event(at, project, kind, payload) VALUES ('a', ?, 'k', '{}')`,
		incompressible(6, 2800)); err != nil {
		t.Fatal(err)
	}
	if err := keyLimitGate(context.Background(), s.db, SchemaVersion); err != nil {
		t.Fatalf("SQLite 원장을 쟀다: %v", err)
	}
}
