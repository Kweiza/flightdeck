package service

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// TestHealthDoesNotMeasureDiskForPostgres 는 Postgres 원장이면 디스크 여유를 재지 않고 「원격 DB, 재지 않음」으로
// 내는지 잰다. Path 는 가린 접속 URL 이라 그 「상위 디렉토리」를 statfs 하면 현재 폴더나 없는 경로를 잰다.
func TestHealthDoesNotMeasureDiskForPostgres(t *testing.T) {
	dsn := storetest.New(t)
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// 판 행은 시험이 적는다 - 운영에서 그것을 적는 것은 Postgres 이전 실행기의 몫이다.
	_, err = raw.Exec(`INSERT INTO schema_version(version, applied_at) VALUES ($1, $2)`,
		store.SchemaVersion, time.Now().UTC().Format(time.RFC3339))
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenWithLogger(dsn, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := New(st, log).Health(context.Background())
	if !h.OK || !h.DBOK {
		t.Fatalf("살아 있는 원장인데 헬스가 %+v 다", h)
	}
	if h.DiskKnown || h.DiskFreePct != -1 || h.DiskError != DiskRemote {
		t.Fatalf("Postgres 원장의 디스크 항목이 (%v, %v, %q)다 - 기대 (false, -1, %q)",
			h.DiskKnown, h.DiskFreePct, h.DiskError, DiskRemote)
	}
}
