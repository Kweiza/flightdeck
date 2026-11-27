// Package freshstore 는 store 밖의 시험(외부 시험 패키지 · 다른 패키지)에 빈 원장 하나를 열어 준다.
//
// ★ 저장 엔진을 바꿀 때 시험 쪽에서 고칠 자리가 이 파일 하나가 되게 둔다. 기본(Open)은 SQLite 파일이고, 두 엔진을 맞대는
// 시험 · 벤치는 OpenOn 으로 엔진을 고른다.
//
// ★ storetest 와 패키지를 가른 까닭: 이것은 store 를 import 해야 하는데, storetest 는 store 의 패키지 안
// 시험이 부르므로 store 를 import 하면 순환이다. 그래서 store 의 패키지 안 시험은 이것을 못 부른다.
package freshstore

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// Engine 은 OpenOn 이 여는 원장의 저장 엔진이다.
type Engine string

const (
	SQLite   Engine = "sqlite"
	Postgres Engine = "postgres"
)

// Open 은 스키마를 다 올린 빈 SQLite 원장을 연다. 시험이 끝나면 닫는다. 로그는 버린다.
func Open(tb testing.TB) *store.Store {
	tb.Helper()
	st, _ := OpenOn(tb, SQLite, nil)
	return st
}

// OpenOn 은 엔진 e 로 스키마를 다 올린 빈 원장을 열고, 그 원장의 자리(SQLite 파일 경로 · Postgres 접속 URL)를 함께 낸다.
// 시험이 끝나면 닫는다. log 가 nil 이면 버린다.
//
// Postgres 는 storetest.New 의 새 시험 DB(판 행이 없는 지금 판의 스키마)에 지금 판 행을 적고 연다 - 서버가 이관 실행기를
// 거친 원장만 열기 때문이다. FD_TEST_PG 가 없으면 storetest 가 실패한다(건너뛰지 않는다).
func OpenOn(tb testing.TB, e Engine, log *slog.Logger) (*store.Store, string) {
	tb.Helper()
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	var at string
	switch e {
	case SQLite:
		at = filepath.Join(tb.TempDir(), "fd.db")
		if err := store.Migrate(context.Background(), at, log); err != nil {
			tb.Fatalf("DB 적용 실패: %v", err)
		}
	case Postgres:
		at = storetest.New(tb)
		raw, err := sql.Open("pgx", at)
		if err != nil {
			tb.Fatal(err)
		}
		_, err = raw.Exec(`INSERT INTO schema_version(version, applied_at) VALUES ($1, $2)`,
			store.SchemaVersion, time.Now().UTC().Format(time.RFC3339))
		raw.Close()
		if err != nil {
			tb.Fatalf("판 행 적기 실패: %v", err)
		}
	default:
		tb.Fatalf("모르는 엔진 %q", e)
	}
	st, err := store.OpenWithLogger(at, log)
	if err != nil {
		tb.Fatalf("DB 열기 실패: %v", err)
	}
	tb.Cleanup(func() { st.Close() })
	return st, at
}
