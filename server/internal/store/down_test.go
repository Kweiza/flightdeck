package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// `fd migrate --to N`(N 이 지금 판보다 낮다)의 내림을 잠근다: 인덱스만 더한 증분만 내리고(DownSteps), 내리면 그 인덱스와 판 행만
// 사라지고 행은 그대로이며, 다시 올리면 같은 인덱스가 선다. 그 아래로는 거절하고 아무것도 안 바꾼다.

// 내릴 수 있는 증분은 인덱스만 더한 것뿐이다. SQLite 증분 가운데 지금은 019 하나이고, 두 엔진의 019 가 같은 인덱스를 더한다.
func TestDownStepsLowerOnlyIndexOnlySteps(t *testing.T) {
	var downable []int
	for _, m := range migrations {
		if _, ok := pg.IndexOnly(m.SQL); ok {
			downable = append(downable, m.To)
		}
	}
	if !slices.Equal(downable, []int{19}) {
		t.Fatalf("인덱스만 더한 SQLite 증분이 %v 다(기대 [19]) - 내림 · 이관 판 범위가 바뀐다", downable)
	}
	lite, _ := pg.IndexOnly(migrations[len(migrations)-1].SQL)
	pgSide, _ := pg.IndexOnly(pg.Steps[len(pg.Steps)-1].SQL)
	if !slices.Equal(lite, pgSide) || !slices.Equal(lite, []string{"event_by_project_kind"}) {
		t.Fatalf("두 엔진의 019 가 더하는 인덱스가 다르다: SQLite %v · Postgres %v", lite, pgSide)
	}

	got, err := DownSteps(19, 18, migrations)
	if err != nil || len(got) != 1 || got[0].To != 19 {
		t.Fatalf("19→18: %v, %v", got, err)
	}
	for _, c := range []struct {
		from, to int
		want     string
	}{
		{19, 17, "증분 18"},
		{19, 19, "더 낮은 판으로만"},
		{18, 19, "더 낮은 판으로만"},
		{2, 1, "증분 2"},
	} {
		if _, err := DownSteps(c.from, c.to, migrations); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d→%d 를 받았거나 사유가 다르다(기대 %q): %v", c.from, c.to, c.want, err)
		}
	}
	if _, err := DownSteps(19, 18, nil); err == nil || !strings.Contains(err.Error(), "증분이 아니다") {
		t.Errorf("증분 목록에 없는 판을 내렸다: %v", err)
	}
}

// downFacts 는 원장의 판 · 판 19 행 수 · event_by_project_kind 의 있음 · event 행 수다. 엔진마다 카탈로그 질의가 다르다.
func downFacts(t *testing.T, s *Store) (version int, row19 int, index bool, events int) {
	t.Helper()
	ctx := context.Background()
	q := `SELECT max(version), (SELECT count(*) FROM schema_version WHERE version = 19), (SELECT count(*) FROM event) FROM schema_version`
	if err := s.db.QueryRowContext(ctx, q).Scan(&version, &row19, &events); err != nil {
		t.Fatal(err)
	}
	cat := `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'event_by_project_kind'`
	if s.db.eng == enginePG {
		cat = `SELECT count(*) FROM pg_class WHERE relkind = 'i' AND relname = 'event_by_project_kind'`
	}
	var n int
	if err := s.db.QueryRowContext(ctx, cat).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return version, row19, n == 1, events
}

// 두 엔진에서: 판 19 원장을 18 로 내리면 그 인덱스와 판 19 행만 사라지고 행은 그대로다. 더 내리기(17)는 거절하고 아무것도 안
// 바꾼다. 다시 올리면 같은 인덱스가 선다.
func TestMigrateDownIsLossless(t *testing.T) {
	ctx := context.Background()
	for _, eng := range []string{"sqlite", "postgres"} {
		t.Run(eng, func(t *testing.T) {
			var path string
			var s *Store
			if eng == "sqlite" {
				path = filepath.Join(t.TempDir(), "fd.db")
				mustMigrate(t, path)
				var err error
				if s, err = openSQLiteReader(ctx, path, ledgerDSN(path), quietLog()); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
			} else {
				path, s = pgMigrated(t)
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO event(at, project, kind, payload) VALUES (?, 'p', 'item.finish', '{}')`,
				fmtTime(time.Now())); err != nil {
				t.Fatal(err)
			}
			if v, r, idx, n := downFacts(t, s); v != 19 || r != 1 || !idx || n != 1 {
				t.Fatalf("전제가 깨졌다: 판 %d · 판 19 행 %d · 인덱스 %v · 이벤트 %d", v, r, idx, n)
			}

			if err := MigrateTo(ctx, path, 18, quietLog()); err != nil {
				t.Fatalf("19→18 내림: %v", err)
			}
			if v, r, idx, n := downFacts(t, s); v != 18 || r != 0 || idx || n != 1 {
				t.Fatalf("내린 뒤: 판 %d · 판 19 행 %d · 인덱스 %v · 이벤트 %d(기대 18 · 0 · false · 1)", v, r, idx, n)
			}
			if eng == "sqlite" {
				if bs := backupsOf(t, path); len(bs) != 0 {
					t.Errorf("내림이 판올림 전 백업을 떴다: %v", bs)
				}
			}

			// SQLite 의 증분 018 은 표를 바꾼 증분이고, Postgres 의 판 18 은 기준선이다 - 사유가 엔진마다 다르다.
			why := "인덱스만 더한 것이 아니라"
			if eng == "postgres" {
				why = "기준선은 내리지 않는다"
			}
			if err := MigrateTo(ctx, path, 17, quietLog()); err == nil || !strings.Contains(err.Error(), why) {
				t.Fatalf("18→17 을 받았거나 사유가 다르다(기대 %q): %v", why, err)
			}
			if v, _, _, n := downFacts(t, s); v != 18 || n != 1 {
				t.Fatalf("거절했는데 원장이 바뀌었다: 판 %d · 이벤트 %d", v, n)
			}

			// 다시 올린다. Postgres 는 판올림 전 덤프를 뜨므로 시험의 덤프 자리를 준다(덤프는 이 시험의 몫이 아니라 막는다).
			if eng == "sqlite" {
				mustMigrate(t, path)
			} else {
				m, run := pgMigrator(t, path, MigrateOptions{BackupDir: t.TempDir()})
				run.backup = func(context.Context, *migrateSession, int) (string, error) { return "시험 덤프", nil }
				if err := m.migrateTo(ctx, run); err != nil {
					t.Fatalf("18→19 다시 올리기: %v", err)
				}
			}
			if v, r, idx, n := downFacts(t, s); v != 19 || r != 1 || !idx || n != 1 {
				t.Fatalf("다시 올린 뒤: 판 %d · 판 19 행 %d · 인덱스 %v · 이벤트 %d", v, r, idx, n)
			}
		})
	}
}
