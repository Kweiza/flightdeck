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

	"github.com/kweiza/flightdeck/internal/store/storetest"
	"github.com/kweiza/flightdeck/internal/store/storetest/freshstore"
)

// TestHealthDoesNotMeasureDiskForPostgres 는 Postgres 원장이면 디스크 여유를 재지 않고 「원격 DB, 재지 않음」으로
// 내는지 잰다. Path 는 파일 경로가 아닌 표시값이라 그 「상위 디렉토리」를 statfs 하면 현재 폴더를 잰다.
func TestHealthDoesNotMeasureDiskForPostgres(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, _ := freshstore.OpenOn(t, freshstore.Postgres, log)

	h := New(st, log).Health(context.Background())
	if !h.OK || !h.DBOK || !h.DBLocaleOK {
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
	st, dsn := freshstore.OpenOn(t, freshstore.Postgres, log)
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
		if h.DBLocaleOK || h.DBLocaleError != DBLocaleUnmeasured {
			t.Fatalf("접속을 막았는데 로케일 축이 (%v, %q)다 - 못 쟀다고 해야 한다", h.DBLocaleOK, h.DBLocaleError)
		}
		for what, text := range map[string]string{"DBError": h.DBError, "DBLocaleError": h.DBLocaleError, "로그": logs.String()} {
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

// TestHealthTurnsOffWhenTheLocaleGateFails 는 기동 뒤에 원장 DB 가 로케일 관문을 못 지나게 되면 헬스가 그것을 잡는지
// 잰다(OK 거짓, db_locale_ok 거짓, 걸린 축). 기동 검사와 같은 판정이다 - 여기서는 pg_trgm 이 빠진 DB 로 만든다
// (로케일 자체는 만든 뒤에 못 바꾼다).
func TestHealthTurnsOffWhenTheLocaleGateFails(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	st, dsn := freshstore.OpenOn(t, freshstore.Postgres, log)
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`DROP EXTENSION pg_trgm CASCADE`)
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, log).Health(context.Background())
	if h.OK || !h.DBOK || h.DBLocaleOK {
		t.Fatalf("로케일 관문을 못 지나는 원장인데 헬스가 %+v 다", h)
	}
	if !strings.Contains(h.DBLocaleError, "pg_trgm") || !strings.Contains(logs.String(), "pg_trgm") {
		t.Fatalf("걸린 축이 헬스 · 로그에 없다: %q / %s", h.DBLocaleError, logs.String())
	}
}
