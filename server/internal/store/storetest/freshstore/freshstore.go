// Package freshstore 는 store 밖의 시험(외부 시험 패키지 · 다른 패키지)에 빈 원장 하나를 열어 준다.
//
// ★ 저장 엔진을 바꿀 때 시험 쪽에서 고칠 자리가 이 함수 하나가 되게 둔다. 지금은 SQLite 파일이다.
//
// ★ storetest 와 패키지를 가른 까닭: 이것은 store 를 import 해야 하는데, storetest 는 store 의 패키지 안
// 시험이 부르므로 store 를 import 하면 순환이다. 그래서 store 의 패키지 안 시험은 이것을 못 부른다.
package freshstore

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/kweiza/flightdeck/internal/store"
)

// Open 은 스키마를 다 올린 빈 원장을 연다. 시험이 끝나면 닫는다. 로그는 버린다.
func Open(tb testing.TB) *store.Store {
	tb.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(tb.TempDir(), "fd.db")
	if err := store.Migrate(context.Background(), path, log); err != nil {
		tb.Fatalf("DB 적용 실패: %v", err)
	}
	st, err := store.OpenWithLogger(path, log)
	if err != nil {
		tb.Fatalf("DB 열기 실패: %v", err)
	}
	tb.Cleanup(func() { st.Close() })
	return st
}
