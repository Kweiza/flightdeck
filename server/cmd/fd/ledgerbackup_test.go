package main

import (
	"bytes"
	"cmp"
	"context"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/ledger"
	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// 산출물 자리는 DB 와 **다른 볼륨**이어야 한다(설계 §7).
func TestLedgerOutDir(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	cases := []struct {
		name      string
		vars      map[string]string
		home      string
		inDataDir bool
		want      string
	}{
		{"FD_LEDGER 가 이긴다", map[string]string{"FD_LEDGER": "/mnt/backup"}, "/home/u", true, "/mnt/backup"},
		{"컨테이너면 /ledger", nil, "/home/u", true, "/ledger"},
		{"호스트면 홈 아래", nil, "/home/u", false, "/home/u/.flightdeck-ledger"},
		{"공백은 없는 것으로 본다", map[string]string{"FD_LEDGER": "   "}, "/home/u", false, "/home/u/.flightdeck-ledger"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LedgerOutDir(env(c.vars), c.home, c.inDataDir); got != c.want {
				t.Errorf("자리가 %q 다 — %q 를 기대한다", got, c.want)
			}
		})
	}
	// ★ DB 자리와 같으면 분리가 통째로 무의미하다. 기본값끼리 겹치지 않는 것을 못박는다.
	db := DefaultDBPath(env(nil), "/home/u", true)
	out := LedgerOutDir(env(nil), "/home/u", true)
	if filepath.Dir(db) == out {
		t.Errorf("원장 자리가 DB 디렉토리와 같다(%s) — 볼륨 하나가 죽으면 둘 다 잃는다", out)
	}
}

// 안 바뀐 회차는 안 쓴다 — **매니페스트만 다른 것은 '바뀐 것'이 아니다.**
func TestLedgerDataUnchangedIgnoresManifest(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"judgments.jsonl":   []byte("{\"id\":\"a\"}\n"),
		"machines.jsonl":    []byte("{\"id\":\"m\"}\n"),
		ledger.ManifestName: []byte(`{"exported_at":"1"}`),
	}
	if ledgerDataUnchanged(files, dir) {
		t.Error("빈 자리를 '이미 최신'으로 봤다 — 첫 회차가 통째로 안 돈다")
	}
	if _, err := ledger.Write(files, dir); err != nil {
		t.Fatalf("Write 실패: %v", err)
	}

	// 매니페스트만 다른 회차 — exported_at 은 회차마다 새로 찍힌다.
	next := map[string][]byte{}
	for k, v := range files {
		next[k] = v
	}
	next[ledger.ManifestName] = []byte(`{"exported_at":"2"}`)
	if !ledgerDataUnchanged(next, dir) {
		t.Error("매니페스트만 다른데 '바뀌었다'로 봤다 — 안 바뀐 원장을 매시간 다시 쓰게 된다")
	}

	// 데이터가 다르면 바뀐 것이다.
	next["judgments.jsonl"] = []byte("{\"id\":\"b\"}\n")
	if ledgerDataUnchanged(next, dir) {
		t.Error("데이터가 다른데 '안 바뀌었다'로 봤다 — 새 판단이 백업에 영영 안 들어간다")
	}

	// 파일 하나가 사라지면 바뀐 것이다(반쯤 덮인 자리가 그 모양이다).
	next["judgments.jsonl"] = files["judgments.jsonl"]
	if err := os.Remove(filepath.Join(dir, "machines.jsonl")); err != nil {
		t.Fatalf("파일 제거 실패: %v", err)
	}
	if ledgerDataUnchanged(next, dir) {
		t.Error("파일이 빠진 자리를 '이미 최신'으로 봤다")
	}
}

// 한 회차가 실제로 원장을 낳고, 안 바뀌면 건너뛰고, 판단이 늘면 다시 쓴다.
func TestLedgerBackupOnceWritesThenSkipsThenWrites(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fd.db")
	// ★ 적용은 기동에서 분리돼 있다(설계 §7 ①) — 열기 전에 올린다.
	if err := store.Migrate(context.Background(), dbPath, nil); err != nil {
		t.Fatalf("DB 적용 실패: %v", err)
	}
	st, err := store.OpenWithLogger(dbPath, quietLog())
	if err != nil {
		t.Fatalf("DB 열기 실패: %v", err)
	}
	defer st.Close()
	if err := st.UpsertProject(ctx, model.Project{ID: "p", Path: "/repo/p"}); err != nil {
		t.Fatalf("프로젝트 등록 실패: %v", err)
	}
	if _, err := st.AddJudgment(ctx, model.Judgment{
		Project: "p", Kind: model.JudgmentDecision, Body: "첫 판단",
	}); err != nil {
		t.Fatalf("판단 저장 실패: %v", err)
	}

	out := filepath.Join(t.TempDir(), "ledger")

	_, wrote, err := ledgerBackupOnce(ctx, st, out, "2026-08-10T00:00:00.000000Z")
	if err != nil {
		t.Fatalf("첫 회차 실패: %v", err)
	}
	if !wrote {
		t.Fatal("첫 회차가 아무것도 안 썼다 — 빈 자리인데 '이미 최신'으로 봤다")
	}
	if !ledger.IsOurOutput(out) {
		t.Fatal("산출물이 우리 원장으로 안 보인다")
	}

	// 같은 DB · 다른 시각 — 데이터가 그대로이므로 건너뛴다.
	_, wrote, err = ledgerBackupOnce(ctx, st, out, "2026-08-10T01:00:00.000000Z")
	if err != nil {
		t.Fatalf("둘째 회차 실패: %v", err)
	}
	if wrote {
		t.Error("안 바뀐 회차가 다시 썼다 — exported_at 만 흔들려도 매시간 새 세대가 난다")
	}

	// 판단이 늘면 다시 쓴다.
	if _, err := st.AddJudgment(ctx, model.Judgment{
		Project: "p", Kind: model.JudgmentDecision, Body: "둘째 판단",
	}); err != nil {
		t.Fatalf("판단 저장 실패: %v", err)
	}
	_, wrote, err = ledgerBackupOnce(ctx, st, out, "2026-08-10T02:00:00.000000Z")
	if err != nil {
		t.Fatalf("셋째 회차 실패: %v", err)
	}
	if !wrote {
		t.Fatal("판단이 늘었는데 안 썼다 — 새 판단이 백업에 영영 안 들어간다")
	}
	d, _, err := ledger.Read(out)
	if err != nil {
		t.Fatalf("산출물 되읽기 실패: %v", err)
	}
	if len(d.Judgments) != 2 {
		t.Errorf("산출물의 판단이 %d건 — 2건이어야 한다", len(d.Judgments))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 잡의 관측이 /healthz 까지 실제로 도착한다
// ─────────────────────────────────────────────────────────────────────────────
//
// ★ 이 시험이 무는 것은 **조립·선 넘기기·화면을 잇는 변환**이다. 앞선 물결이 자동 갱신
// 축에서 정확히 이 자리를 잃었다(2026-08-07 실측): 셋은 각자 잠겨 있었는데 그것들을 잇는
// 변환만 아무 시험에도 안 걸려서, 판정은 살아 있고 값만 안 도착하는 상태가 조용히 났다.

// LastAt 은 "회차 없음"과 "1970년"을 안 접는다.
func TestLedgerBackupStatusOfDoesNotFoldNeverRanIntoEpoch(t *testing.T) {
	got := ledgerBackupStatusOf(ledgerBackupState{route: "/ledger"})
	if !got.Running {
		t.Error("잡이 있는데 Running=false 다 — '배선 안 됨'과 '아직 회차 없음'이 접힌다")
	}
	if got.LastAt != nil {
		t.Errorf("한 회차도 안 돌았는데 시각이 실렸다(%v) — 1970년에 백업한 서버로 보인다", *got.LastAt)
	}
	if got.Route != "/ledger" {
		t.Errorf("산출물 자리가 안 실렸다 — '돌긴 도는데 어디에 쌓이는지 모른다'가 된다: %+v", got)
	}

	at := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)
	got = ledgerBackupStatusOf(ledgerBackupState{lastAt: at, outcome: "failed", detail: "디스크가 찼다", route: "/ledger"})
	if got.LastAt == nil || !got.LastAt.Equal(at) {
		t.Errorf("회차 시각이 안 실렸다: %+v", got)
	}
	if got.Outcome != "failed" || got.Detail != "디스크가 찼다" {
		t.Errorf("결과와 사유가 안 실렸다: %+v", got)
	}
}

// 회차가 돌면 잡이 그것을 기억하고, 그 값이 API 모양까지 온다.
func TestLedgerBackupJobRemembersItsLastTick(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fd.db")
	// ★ 적용은 기동에서 분리돼 있다(설계 §7 ①) — 열기 전에 올린다.
	if err := store.Migrate(context.Background(), dbPath, nil); err != nil {
		t.Fatalf("DB 적용 실패: %v", err)
	}
	st, err := store.OpenWithLogger(dbPath, quietLog())
	if err != nil {
		t.Fatalf("DB 열기 실패: %v", err)
	}
	defer st.Close()
	if err := st.UpsertProject(ctx, model.Project{ID: "p", Path: "/repo/p"}); err != nil {
		t.Fatalf("프로젝트 등록 실패: %v", err)
	}

	out := filepath.Join(t.TempDir(), "ledger")
	j := newLedgerBackupJob(quietLog(), st, out, time.Hour)

	// 아직 안 돌았다.
	if s := ledgerBackupStatusOf(j.State()); s.LastAt != nil || s.Outcome != "" {
		t.Fatalf("한 회차도 안 돌았는데 상태가 차 있다: %+v", s)
	}

	at := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)
	j.tick(ctx, at)
	s := ledgerBackupStatusOf(j.State())
	if s.Outcome != "wrote" {
		t.Errorf("첫 회차가 %q 다 — 빈 자리라 wrote 여야 한다: %+v", s.Outcome, s)
	}
	if s.LastAt == nil || !s.LastAt.Equal(at) {
		t.Errorf("회차 시각이 안 남았다: %+v", s)
	}

	// 안 바뀐 회차 — unchanged 이고, 그것은 실패가 아니다.
	j.tick(ctx, at.Add(time.Hour))
	if s := ledgerBackupStatusOf(j.State()); s.Outcome != "unchanged" {
		t.Errorf("안 바뀐 회차가 %q 다: %+v", s.Outcome, s)
	}

	// 실패하는 회차 — 자리를 파일로 막으면 MkdirAll 이 죽는다.
	bad := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatalf("픽스처 실패: %v", err)
	}
	j2 := newLedgerBackupJob(quietLog(), st, bad, time.Hour)
	j2.tick(ctx, at)
	s2 := ledgerBackupStatusOf(j2.State())
	if s2.Outcome != "failed" {
		t.Errorf("쓰기가 실패했는데 %q 로 남았다 — 조용히 실패하는 자리가 그대로다: %+v", s2.Outcome, s2)
	}
	if strings.TrimSpace(s2.Detail) == "" {
		t.Error("실패인데 사유가 비었다 — 화면이 '무엇이 잘못됐나'를 못 말한다")
	}
}

// 배선이 실제로 걸린다 — 잡이 nil 이면 콜백을 안 달고, 있으면 그 값이 온다.
func TestServeAPIOptionsWiresLedgerBackup(t *testing.T) {
	if serveAPIOptions("tok", 60, quietLog(), false, nil, nil, false, nil).LedgerBackup != nil {
		t.Error("잡이 없는데 콜백이 달렸다 — api 가 '배선 안 됨'을 못 말하게 된다")
	}
	j := newLedgerBackupJob(quietLog(), nil, "/ledger", time.Hour)
	opt := serveAPIOptions("tok", 60, quietLog(), false, nil, j, false, nil)
	if opt.LedgerBackup == nil {
		t.Fatal("잡이 있는데 콜백이 안 달렸다 — /healthz 가 이 축을 영영 못 낸다")
	}
	if got := opt.LedgerBackup(); !got.Running || got.Route != "/ledger" {
		t.Errorf("콜백이 잡의 상태를 안 나른다: %+v", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 세대가 bare 레포에 쌓인다 — 지금 자리는 마지막 한 장만 산다
// ─────────────────────────────────────────────────────────────────────────────

func gitLog(t *testing.T, repo string) []string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+repo, "log", "--format=%s", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git log 실패: %v\n%s", err, out)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// 커밋은 **실제 변경에만** 쌓인다. 안 바뀐 회차는 트리가 같아 커밋이 안 생긴다.
func TestCommitLedgerGenerationOnlyOnRealChange(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), journalRepoName)
	at := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)

	src := t.TempDir()
	names := []string{"judgments.jsonl", ledger.ManifestName}
	put := func(judgments, manifest string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(src, "judgments.jsonl"), []byte(judgments), 0o600); err != nil {
			t.Fatalf("픽스처 실패: %v", err)
		}
		if err := os.WriteFile(filepath.Join(src, ledger.ManifestName), []byte(manifest), 0o600); err != nil {
			t.Fatalf("픽스처 실패: %v", err)
		}
	}
	put("{\"id\":\"a\"}\n", `{"exported_at":"1"}`)
	committed, err := commitLedgerGeneration(ctx, repo, src, names, at)
	if err != nil {
		t.Fatalf("첫 커밋 실패: %v", err)
	}
	if !committed {
		t.Fatal("첫 세대인데 커밋이 안 생겼다 — 역사가 통째로 안 쌓인다")
	}
	if got := gitLog(t, repo); len(got) != 1 {
		t.Fatalf("커밋이 %d개다: %v", len(got), got)
	}

	// 같은 내용 · 다른 시각 — 트리가 같으므로 안 쌓인다.
	committed, err = commitLedgerGeneration(ctx, repo, src, names, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("둘째 회차 실패: %v", err)
	}
	if committed {
		t.Error("안 바뀐 회차가 커밋을 만들었다 — 매시간 무의미한 커밋이 쌓인다")
	}
	if got := gitLog(t, repo); len(got) != 1 {
		t.Errorf("커밋이 %d개로 늘었다: %v", len(got), got)
	}

	// 내용이 바뀌면 쌓인다. 그리고 **앞 세대가 그대로 살아 있다.**
	put("{\"id\":\"a\"}\n{\"id\":\"b\"}\n", `{"exported_at":"2"}`)
	committed, err = commitLedgerGeneration(ctx, repo, src, names, at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("셋째 회차 실패: %v", err)
	}
	if !committed {
		t.Fatal("내용이 바뀌었는데 안 쌓였다")
	}
	got := gitLog(t, repo)
	if len(got) != 2 {
		t.Fatalf("커밋이 %d개다 — 2개여야 한다: %v", len(got), got)
	}
	// 앞 세대의 내용을 실제로 다시 꺼낼 수 있다 — 그것이 이 항목의 존재 이유다.
	old, err := exec.Command("git", "--git-dir="+repo, "show", "refs/heads/main~1:judgments.jsonl").Output()
	if err != nil {
		t.Fatalf("앞 세대를 못 꺼냈다: %v", err)
	}
	if string(old) != "{\"id\":\"a\"}\n" {
		t.Errorf("앞 세대의 내용이 다르다: %q", old)
	}
}

// 잡의 회차가 저널까지 민다 — 파일을 안 써도 저널은 시도한다.
func TestLedgerBackupJobPushesToJournalEvenWhenFilesUnchanged(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fd.db")
	// ★ 적용은 기동에서 분리돼 있다(설계 §7 ①) — 열기 전에 올린다.
	if err := store.Migrate(context.Background(), dbPath, nil); err != nil {
		t.Fatalf("DB 적용 실패: %v", err)
	}
	st, err := store.OpenWithLogger(dbPath, quietLog())
	if err != nil {
		t.Fatalf("DB 열기 실패: %v", err)
	}
	defer st.Close()
	if err := st.UpsertProject(ctx, model.Project{ID: "p", Path: "/repo/p"}); err != nil {
		t.Fatalf("프로젝트 등록 실패: %v", err)
	}

	out := filepath.Join(t.TempDir(), "ledger")
	at := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)

	// ① 먼저 파일만 만들어 둔다 — 이 기능이 붙기 전 판이 정확히 이 모양이다.
	if _, _, err := ledgerBackupOnce(ctx, st, out, "2026-08-10T04:00:00.000000Z"); err != nil {
		t.Fatalf("사전 회차 실패: %v", err)
	}
	repo := filepath.Join(out, journalRepoName)
	if _, err := os.Stat(repo); !os.IsNotExist(err) {
		t.Fatalf("사전 조건이 깨졌다 — 저널이 이미 있다")
	}

	// ② 잡의 회차 — 파일은 안 바뀌지만(unchanged) 저널은 첫 세대를 쌓아야 한다.
	j := newLedgerBackupJob(quietLog(), st, out, time.Hour)
	j.tick(ctx, at)
	s := ledgerBackupStatusOf(j.State())
	if s.Outcome != "unchanged" {
		t.Errorf("파일이 바뀌었다고 봤다: %+v", s)
	}
	if s.Journal != "committed" {
		t.Fatalf("파일이 이미 있는 판에서 저널이 안 쌓였다(%q) — 그 판은 영영 역사를 못 갖는다: %+v",
			s.Journal, s)
	}
	if got := gitLog(t, repo); len(got) != 1 {
		t.Errorf("커밋이 %d개다: %v", len(got), got)
	}

	// ③ 다시 돌면 저널도 unchanged 다.
	j.tick(ctx, at.Add(time.Hour))
	if s := ledgerBackupStatusOf(j.State()); s.Journal != "unchanged" {
		t.Errorf("안 바뀐 회차의 저널이 %q 다: %+v", s.Journal, s)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 두 엔진의 원장이 같은 바이트다
// ─────────────────────────────────────────────────────────────────────────────

// ledgerParityFixture 는 엔진 차이가 드러날 값만 모은 원장이다. 정렬 축(id · (judgment_id, target_kind, target_id) ·
// (project, key))마다 대소문자 · 밑줄 · 공백 · 발음 구별 기호 · 한글을 섞었다 - 언어 규칙 정렬(en_US 등)이면 그 순서가
// 코드포인트 순서와 갈린다. 글 칸에는 NULL 과 빈 글, 이모지 · 줄바꿈 · 탭 · 따옴표 · 역슬래시 · <>& 를 넣었다.
// NUL 은 없다(Postgres text 가 거절하고, 이관 사전 검사가 막는 값이다). 넣는 순서는 일부러 정렬 순서가 아니다.
func ledgerParityFixture() store.LedgerDump {
	s := func(v string) *string { return &v }
	const at = "2026-11-27T01:02:03.000004Z"
	return store.LedgerDump{
		Machines: []store.LedgerMachine{
			{ID: "m-lower", Hostname: "호스트", FirstSeen: at, LastSeen: at},
			{ID: "M-Upper", Hostname: "h\tq\"", FirstSeen: at, LastSeen: at},
		},
		Projects: []store.LedgerProject{
			{ID: "proj-가", Path: "/repo/가", RemoteURL: s(""), DefaultBranch: "main", Config: s(`{"a":"<b>&\"\\n","가":[1,2]}`),
				CreatedAt: at, PinnedAt: s(at)},
			{ID: "Proj", Path: "/repo/P", DefaultBranch: "trunk", ConfigFromSHA: s("abc"), CreatedAt: at, ArchivedAt: s(at)},
		},
		Sessions: []store.LedgerSession{
			{ID: "s_b", Project: "Proj", MachineID: "M-Upper", Worktree: "/w/b", CCSessionID: "cc", Label: s(""),
				State: "blocked", BlockedWhy: s("막힘 🙂\n둘째 줄"), OpenedAt: at, Harness: s("codex")},
			{ID: "S-a", Project: "proj-가", MachineID: "m-lower", Worktree: "/w/a", CCSessionID: "cc", State: "active", OpenedAt: at},
		},
		Judgments: []store.LedgerJudgment{
			{ID: "가", Project: s("proj-가"), SessionID: s("S-a"), At: at, Kind: "decision", Title: s(""), Body: "한글 본문 🙂"},
			{ID: "alpha", At: at, Kind: "now", Body: "프로젝트 없는 판단\r\n\t\"인용\" \\ <>&", Supersedes: s("Zeta")},
			{ID: "ä", Project: s("Proj"), At: at, Kind: "ask", Title: s("ä 제목"), Body: "u+2028   u+2029  "},
			{ID: "_x", Project: s("Proj"), SessionID: s("s_b"), At: at, Kind: "blocked", Body: "밑줄"},
			{ID: "Zeta", Project: s("Proj"), At: at, Kind: "handoff", Title: s("대문자"), Body: "Z"},
			{ID: "a b", Project: s("proj-가"), At: at, Kind: "verified", Body: "공백 id"},
		},
		Links: []store.LedgerLink{
			{JudgmentID: "가", TargetKind: "item", TargetID: "i-b"},
			{JudgmentID: "가", TargetKind: "item", TargetID: "I-a", TargetProject: s("Proj")},
			{JudgmentID: "가", TargetKind: "commit", TargetID: "abc"},
			{JudgmentID: "_x", TargetKind: "session", TargetID: "s_b", TargetProject: s("")},
			{JudgmentID: "Zeta", TargetKind: "job", TargetID: "j"},
		},
		Snapshots: []store.LedgerSnapshot{
			{Project: "proj-가", Key: "키", Value: "값", Method: "command", ComputedAt: at},
			{Project: "Proj", Key: "k", Value: "1", Method: "manual", Evidence: s("근거 \"따옴표\""), InputDigest: s(""), ComputedAt: at},
			{Project: "Proj", Key: "K", Value: "2", Method: "command", InputDigest: s("d"), ComputedAt: at},
			{Project: "Proj", Key: "_k", Value: "3", Method: "command", ComputedAt: at},
		},
	}
}

// sortedLedger 는 원장을 ReadLedger 의 ORDER BY 축으로, 바이트(코드포인트) 순서로 정렬한 사본이다. Go 의 문자열 비교가
// 바이트 비교이고, UTF-8 의 바이트 순서는 코드포인트 순서다.
func sortedLedger(d store.LedgerDump) store.LedgerDump {
	out := store.LedgerDump{
		Machines: slices.Clone(d.Machines), Projects: slices.Clone(d.Projects), Sessions: slices.Clone(d.Sessions),
		Judgments: slices.Clone(d.Judgments), Links: slices.Clone(d.Links), Snapshots: slices.Clone(d.Snapshots),
	}
	slices.SortFunc(out.Machines, func(a, b store.LedgerMachine) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Projects, func(a, b store.LedgerProject) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Sessions, func(a, b store.LedgerSession) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Judgments, func(a, b store.LedgerJudgment) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Links, func(a, b store.LedgerLink) int {
		return cmp.Or(strings.Compare(a.JudgmentID, b.JudgmentID), strings.Compare(a.TargetKind, b.TargetKind),
			strings.Compare(a.TargetID, b.TargetID))
	})
	slices.SortFunc(out.Snapshots, func(a, b store.LedgerSnapshot) int {
		return cmp.Or(strings.Compare(a.Project, b.Project), strings.Compare(a.Key, b.Key))
	})
	return out
}

// openLedgerParityStores 는 같은 판의 빈 원장 둘(SQLite 파일 · Postgres 시험 DB)을 이전 실행기로 깔고 서버처럼 연다.
func openLedgerParityStores(t *testing.T) map[string]*store.Store {
	t.Helper()
	ctx := context.Background()
	out := map[string]*store.Store{}
	for name, target := range map[string]string{
		"sqlite":   filepath.Join(t.TempDir(), "fd.db"),
		"postgres": storetest.NewBare(t, ""),
	} {
		if err := store.Migrate(ctx, target, quietLog()); err != nil {
			t.Fatalf("%s 원장 깔기: %v", name, err)
		}
		st, err := store.OpenWithLogger(target, quietLog())
		if err != nil {
			t.Fatalf("%s 원장 열기: %v", name, err)
		}
		t.Cleanup(func() { _ = st.Close() })
		out[name] = st
	}
	return out
}

// TestLedgerBackupBytesMatchAcrossEngines 는 매시간 원장 백업의 산출물이 두 엔진에서 같은 바이트인지 잰다(재설계 스펙의
// U-PG 성공 기준 「판단 원장 JSONL 내보내기 바이트가 SQLite 판과 같다」). 같은 원장을 두 엔진에 되쓰고 같은 시각으로 한
// 회차씩 돌려, 매니페스트까지 파일마다 바이트를 맞댄다.
//
// ★ 두 엔진이 같은 것을 똑같이 잃어도 서로는 같다. 그래서 산출물을 원본(정렬한 픽스처)을 직접 인코딩한 것과도 맞댄다.
// 정렬은 코드포인트 순서여야 한다 - Postgres 원장이 언어 규칙 정렬로 만들어지면 정렬 축에서 줄 순서가 갈린다(로케일 관문이
// 기동에서 막는 그 축이다).
func TestLedgerBackupBytesMatchAcrossEngines(t *testing.T) {
	ctx := context.Background()
	const at = "2026-11-27T05:00:00.000000Z"
	fixture := ledgerParityFixture()
	want, _, err := ledger.Encode(sortedLedger(fixture), store.SchemaVersion, at)
	if err != nil {
		t.Fatalf("원본 인코딩: %v", err)
	}

	got := map[string]map[string][]byte{}
	for name, st := range openLedgerParityStores(t) {
		if err := st.WriteLedger(ctx, fixture); err != nil {
			t.Fatalf("%s 되쓰기: %v", name, err)
		}
		dir := filepath.Join(t.TempDir(), name)
		if _, wrote, err := ledgerBackupOnce(ctx, st, dir, at); err != nil || !wrote {
			t.Fatalf("%s 회차: wrote=%v err=%v", name, wrote, err)
		}
		files := map[string][]byte{}
		for f := range want {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatalf("%s 산출물 %s 읽기: %v", name, f, err)
			}
			files[f] = b
		}
		got[name] = files
	}

	if len(want) < 8 {
		t.Fatalf("원장 파일이 %d개다 - 일곱 표 + 매니페스트여야 한다: %v", len(want), slices.Sorted(maps.Keys(want)))
	}
	for _, f := range slices.Sorted(maps.Keys(want)) {
		lite, pg := got["sqlite"][f], got["postgres"][f]
		if !bytes.Equal(lite, pg) {
			t.Errorf("%s 가 엔진마다 다르다.\n  sqlite:   %q\n  postgres: %q", f, lite, pg)
		}
		if !bytes.Equal(lite, want[f]) {
			t.Errorf("%s 가 원본을 인코딩한 것과 다르다(두 엔진이 같은 것을 잃었거나 정렬이 코드포인트 순서가 아니다).\n"+
				"  산출물: %q\n  원본:   %q", f, lite, want[f])
		}
	}
}
