package service

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// openPGLedger 는 판 행을 적은 새 시험 DB 를 store 로 연다. 둘째 값은 그 DB 의 접속 URL 이다.
//
// 판 행은 시험이 적는다 - 운영에서 그것을 적는 것은 Postgres 이전 실행기의 몫이다.
func openPGLedger(t *testing.T, log *slog.Logger) (*store.Store, string) {
	t.Helper()
	dsn := storetest.New(t)
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`INSERT INTO schema_version(version, applied_at) VALUES ($1, $2)`,
		store.SchemaVersion, time.Now().UTC().Format(time.RFC3339))
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenWithLogger(dsn, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dsn
}

// TestHealthDoesNotMeasureDiskForPostgres 는 Postgres 원장이면 디스크 여유를 재지 않고 「원격 DB, 재지 않음」으로
// 내는지 잰다. Path 는 파일 경로가 아닌 표시값이라 그 「상위 디렉토리」를 statfs 하면 현재 폴더를 잰다.
func TestHealthDoesNotMeasureDiskForPostgres(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, _ := openPGLedger(t, log)

	h := New(st, log).Health(context.Background())
	if !h.OK || !h.DBOK {
		t.Fatalf("살아 있는 원장인데 헬스가 %+v 다", h)
	}
	if !h.DBRemote {
		t.Fatal("Postgres 원장인데 DBRemote 가 거짓이다 - /healthz 가 로그에 전문이 있다고 잘못 안내한다")
	}
	if h.DiskKnown || h.DiskFreePct != -1 || h.DiskError != DiskRemote {
		t.Fatalf("Postgres 원장의 디스크 항목이 (%v, %v, %q)다 - 기대 (false, -1, %q)",
			h.DiskKnown, h.DiskFreePct, h.DiskError, DiskRemote)
	}
}

// TestHealthCarriesNoDSNPieceAfterReconnectFails 는 기동 **뒤** 연결 풀의 재접속이 실패할 때 Health 가 로그 ·
// DBError(대시보드의 헬스 줄)에 접속 문자열의 조각(사용자 · DB 이름)을 안 싣는지 잰다. 드라이버의 접속 실패 본문은
// 처음 받은 접속 문자열의 DB 이름을 다시 인용하고, 경계가 모호한 문자열이면 그 이름이 비밀번호 조각이다.
func TestHealthCarriesNoDSNPieceAfterReconnectFails(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	st, dsn := openPGLedger(t, log)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	// 로그는 비우지 않는다 - 열 때의 기동 진단 줄도 이름을 싣지 않아야 한다(store.logPGServer 의 ★).
	storetest.CutConnections(t, dsn)

	// 첫 확인은 풀에 남은 끊긴 연결을 꺼내 「끊겼다」로 끝날 수 있다(database/sql 의 Ping 은 그때 다시 붙지 않는다).
	// 재접속 거절(55000)에 닿을 때까지 몇 번 부르고, 그 사이의 DBError 도 전부 잰다.
	svc := New(st, log)
	var h Health
	for try := 0; try < 3 && !strings.Contains(h.DBError, "SQLSTATE 55000"); try++ {
		h = svc.Health(context.Background())
		if h.OK || h.DBOK {
			t.Fatalf("접속을 막았는데 헬스가 정상이다: %+v", h)
		}
		for what, text := range map[string]string{"DBError": h.DBError, "로그": logs.String()} {
			for _, piece := range []string{name, "user=", "failed to connect"} {
				if strings.Contains(text, piece) {
					t.Fatalf("%s 에 접속 문자열의 조각(%s)이 실렸다: %s", what, piece, text)
				}
			}
		}
	}
	if !strings.Contains(h.DBError, "SQLSTATE 55000") || !strings.Contains(logs.String(), "SQLSTATE 55000") {
		t.Fatalf("재접속 거절의 종류 · SQLSTATE 가 DBError · 로그에 없다: %q / %s", h.DBError, logs.String())
	}
}
