package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// `fd migrate import-sqlite` · `fd migrate verify` · `fd migrate export-sqlite` 의 표면: 예행은 아무것도 안 쓰고 0, 옮기기 0,
// 옮긴 원본을 다시 옮기기는 거절 1(이관 표지), 대조 0, 사용법 오류 2. 역이관은 새 파일을 내고 0(예행은 파일을 안 남긴다), 있는 파일에는
// 거절 1, 낸 파일과 Postgres 원장의 변환 없는 대조(--exported) 0. 원본의 NUL 을 바꿔 옮긴 칸은 예행 · 이관 · 대조가 칸 · 규칙 ·
// 행 수로 말하고(판정 줄 아래에도 한 번 더), 이관은 바꾼 칸의 기록 파일을 말한다. 바꿀 것 없는 역이관 · 그 파일의 대조는 말하지
// 않는다. 출력에 접속 URL 의 어떤 조각도 없다. 옮기기 · 거절의 뜻은 store 시험이 잰다.
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
	// NUL 이 든 원본 칸은 앞 판 서버가 쓴 것이다 - 지금 서버는 원장 칸 관문이 그 NUL 을 \x00 표기로 옮겨 쓴다.
	if _, err := s.DB().ExecContext(ctx, `UPDATE machine SET hostname = 'ho' || char(0) || 'st' WHERE id = 'm1'`); err != nil {
		t.Fatalf("원본 행의 NUL: %v", err)
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
		// 앞 명령이 닫은 연결이 서버에서 빠지기를 기다린다 - 이관 · 역이관은 원장에 붙은 다른 연결을 거절한다.
		storetest.SettleClients(t, dsn, 0)
		rc, out := h.run("", append([]string{"migrate"}, args...)...)
		outs = append(outs, out)
		if rc != want || !strings.Contains(out, wantText) {
			t.Fatalf("fd migrate %v: rc=%d(기대 %d), %q 가 없다:\n%s", args, rc, want, wantText, out)
		}
	}
	// 원본의 NUL 을 바꿔 옮겼다는 말(표 · 칸 · 규칙 · 행 좌표, 판정 줄 아래의 행 수)이 있는가.
	rewrote := func(out string) bool {
		return strings.Contains(out, "옮기며 바꾼 원본 칸 - 1 행") && strings.Contains(out, "machine.hostname  NUL 바이트 → \\x00  1건") &&
			strings.Contains(out, `id="m1"`) && strings.Contains(out, "그중 1 행은 원본의 NUL 을 \\x00 표기로 바꾼 값이다")
	}
	last := func() string { return outs[len(outs)-1] }
	step(0, "아무것도 쓰지 않았다", "import-sqlite", "--from", src, "--db", dsn, "--dry-run")
	if !rewrote(last()) {
		t.Fatalf("예행이 바꿔 옮길 칸을 말하지 않는다:\n%s", last())
	}
	step(1, "두 원장이 맞지 않는다", "verify", "--from", src, "--db", dsn) // 아직 안 옮겼다 - machine 이 어긋난다
	step(0, "옮겼다: 1 행", "import-sqlite", "--from", src, "--db", dsn)
	if !strings.Contains(last(), "이관 표지를 남겼다: "+store.MovedMarker(src)) || !rewrote(last()) ||
		!strings.Contains(last(), "바꾼 칸 전부(행 키 · 칸 · JSON 자리 · 원래 바이트)를 "+src+".rewritten-") {
		t.Fatalf("옮긴 뒤 이관 표지나 바꿔 옮긴 칸 · 그 기록 파일을 말하지 않는다:\n%s", last())
	}
	// 원본의 소유자 귀속을 대조표 위에 적는다(판정에는 안 든다): 이 판의 서버가 쓴 기기는 계정 칸이 빈다.
	if !strings.Contains(last(), "소유자 계정 없다 · 빈 행위자 칸 1") || !regexp.MustCompile(`machine\.account_id +빈 칸 1 / 1 행`).MatchString(last()) {
		t.Fatalf("옮긴 보고가 원본의 소유자 귀속을 적지 않는다:\n%s", last())
	}
	step(1, "다시 옮기지 않는다", "import-sqlite", "--from", src, "--db", dsn)
	step(0, "두 원장이 맞다", "verify", "--from", src, "--db", dsn)
	if !rewrote(last()) {
		t.Fatalf("대조가 바꿔 옮긴 칸을 말하지 않는다:\n%s", last())
	}
	step(2, "", "import-sqlite", "--db", dsn) // --from 없음
	step(2, "", "verify", "--from", src, "extra")

	back := filepath.Join(t.TempDir(), "back.db")
	step(0, "아무 파일도 남기지 않았다", "export-sqlite", "--to", back, "--db", dsn, "--dry-run")
	if entries, err := os.ReadDir(filepath.Dir(back)); err != nil || len(entries) != 0 {
		t.Fatalf("역이관 예행이 파일을 남겼다: %v %v", entries, err)
	}
	step(0, "옮겼다: 1 행", "export-sqlite", "--to", back, "--db", dsn)
	exported := last()
	if !strings.Contains(exported, "fd migrate verify --from "+back+" --exported") {
		t.Fatalf("역이관이 낸 파일을 변환 없이 맞대라고(--exported) 말하지 않는다:\n%s", exported)
	}
	step(1, "이미 있다", "export-sqlite", "--to", back, "--db", dsn)
	step(0, "두 원장이 맞다", "verify", "--from", back, "--db", dsn, "--exported")
	for _, out := range []string{exported, last()} {
		if strings.Contains(out, "바꾼") {
			t.Fatalf("바꿀 것 없는 역이관 · 대조가 바꾼 칸을 말한다:\n%s", out)
		}
	}
	step(1, "Postgres 접속 URL", "export-sqlite", "--to", filepath.Join(t.TempDir(), "x.db"), "--db", src)
	step(2, "", "export-sqlite", "--db", dsn) // --to 없음
	for _, out := range outs {
		if strings.Contains(out, dsn) || strings.Contains(out, name) || strings.Contains(out, u.Host) {
			t.Fatalf("출력에 접속 URL 조각이 실렸다:\n%s", out)
		}
	}
}
