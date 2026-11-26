package main

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// `fd migrate import-sqlite` · `fd migrate verify` 의 표면: 예행은 아무것도 안 쓰고 0, 옮기기 0, 같은 대상에 다시 옮기기는
// 거절 1, 대조 0, 사용법 오류 2. 출력에 접속 URL 의 어떤 조각도 없다. 옮기기 · 거절의 뜻은 store 시험이 잰다.
func TestMigrateImportSQLiteSurface(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	src := filepath.Join(t.TempDir(), "fd.db")
	if err := store.Migrate(ctx, src, nil); err != nil {
		t.Fatalf("원본: %v", err)
	}
	s, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMachine(ctx, model.Machine{ID: "m1", Hostname: "host"}); err != nil {
		t.Fatalf("원본 행: %v", err)
	}
	s.Close()
	dsn := storetest.NewBare(t, "")
	if err := store.Migrate(ctx, dsn, nil); err != nil {
		t.Fatalf("대상 기준선: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")

	var outs []string
	step := func(want int, wantText string, args ...string) {
		t.Helper()
		rc, out := h.run("", append([]string{"migrate"}, args...)...)
		outs = append(outs, out)
		if rc != want || !strings.Contains(out, wantText) {
			t.Fatalf("fd migrate %v: rc=%d(기대 %d), %q 가 없다:\n%s", args, rc, want, wantText, out)
		}
	}
	step(0, "아무것도 쓰지 않았다", "import-sqlite", "--from", src, "--db", dsn, "--dry-run")
	step(1, "두 원장이 맞지 않는다", "verify", "--from", src, "--db", dsn) // 아직 안 옮겼다 - machine 이 어긋난다
	step(0, "옮겼다: 1 행", "import-sqlite", "--from", src, "--db", dsn)
	step(1, "비어 있지 않다", "import-sqlite", "--from", src, "--db", dsn)
	step(0, "두 원장이 맞다", "verify", "--from", src, "--db", dsn)
	step(2, "", "import-sqlite", "--db", dsn) // --from 없음
	step(2, "", "verify", "--from", src, "extra")
	for _, out := range outs {
		if strings.Contains(out, dsn) || strings.Contains(out, name) || strings.Contains(out, u.Host) {
			t.Fatalf("출력에 접속 URL 조각이 실렸다:\n%s", out)
		}
	}
}
