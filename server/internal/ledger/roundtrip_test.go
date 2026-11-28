package ledger_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/ledger"
	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store"
)

// ★ 이 시험이 "무손실"의 유일한 증명이다.
// DB → 원장 읽기 → JSONL → 파일 → 되읽기 → 빈 DB → 다시 원장 읽기 가 원본과 같아야 한다.
// 이것이 없으면 이 저장소가 만든 것은 "복원해 본 적 없는 백업"이다.
func TestLedgerSurvivesFullRoundTrip(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	srcPath := filepath.Join(t.TempDir(), "src.db")
	// ★ 적용은 기동에서 분리돼 있다(설계 §7 ①) — 열기 전에 올린다.
	if err := store.Migrate(context.Background(), srcPath, nil); err != nil {
		t.Fatalf("DB 적용 실패: %v", err)
	}
	src, err := store.OpenWithLogger(srcPath, quiet)
	if err != nil {
		t.Fatalf("원본 DB 열기 실패: %v", err)
	}
	seedLedgerFixture(t, src, srcPath)

	want, err := src.ReadLedger(ctx)
	if err != nil {
		t.Fatalf("원장 읽기 실패: %v", err)
	}
	// ★ machines·projects·sessions 도 함께 단정한다. judgments·links·snapshots 만 보면
	//   폐포 축(machine·project·session)이 통째로 비어도 가드를 통과한다 — 그 상태에서
	//   FK 오류나 DeepEqual 이 대신 잡아주긴 하지만, 그건 가드가 아니라 시험 본문이 우연히
	//   잡는 것이다. 가드의 목적은 "이 시험이 그 축을 보고 있다"를 실행 전에 못박는 것이다.
	if len(want.Machines) < 1 || len(want.Projects) < 1 || len(want.Sessions) < 1 ||
		len(want.Judgments) < 3 || len(want.Links) < 2 || len(want.Snapshots) < 1 {
		t.Fatalf("픽스처가 빈약하다 — 이 시험이 아무것도 안 본다: "+
			"machines=%d projects=%d sessions=%d judgments=%d links=%d snapshots=%d",
			len(want.Machines), len(want.Projects), len(want.Sessions),
			len(want.Judgments), len(want.Links), len(want.Snapshots))
	}
	// ★ 이게 핵심이다. 세션 표가 있어도 그것을 가리키는 판단이 하나도 없으면
	//   FK 폐포 축(judgment.session_id → session.id)을 이 시험이 안 본 것이다 —
	//   1차 시도가 BLOCKED 로 잡아낸 결함이 재발해도 위 개수 단정만으로는 초록이 나온다.
	if !anyJudgmentHasSession(want.Judgments) {
		t.Fatal("픽스처가 빈약하다 — 이 시험이 아무것도 안 본다: " +
			"SessionID 가 채워진 판단이 하나도 없다(세션 FK 폐포 축을 안 본다)")
	}
	if err := src.Close(); err != nil {
		t.Fatalf("원본 닫기 실패: %v", err)
	}

	// 내보낸다.
	files, m, err := ledger.Encode(want, store.SchemaVersion, "2026-08-06T00:00:00.000000Z")
	if err != nil {
		t.Fatalf("Encode 실패: %v", err)
	}
	// 비밀 칸(store.LedgerSecretColumns)의 값은 어느 파일에도 없다 - 원장은 지워지지 않는 기록 저장소에 매시간 쌓인다.
	for name, b := range files {
		for _, secret := range []string{fixturePasswordHash, fixtureTokenHash} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s 에 비밀 칸 값 %q 가 실렸다", name, secret)
			}
		}
	}
	dir := filepath.Join(t.TempDir(), "out")
	if _, err := ledger.Write(files, dir); err != nil {
		t.Fatalf("Write 실패: %v", err)
	}

	// 되읽는다.
	got, gotM, err := ledger.Read(dir)
	if err != nil {
		t.Fatalf("Read 실패: %v", err)
	}
	if gotM.SchemaVersion != m.SchemaVersion {
		t.Errorf("스키마 버전이 달라졌다: %d → %d", m.SchemaVersion, gotM.SchemaVersion)
	}

	// ★ 완전히 빈 DB 에 되쓴다. seed 를 부르지 않는다 — Task 10 이 폐포를 닫았으므로
	//   machine·project·session 까지 원장이 다 갖고 와야 한다. 미리 심어 줘야 통과한다면
	//   그것은 폐포가 안 닫힌 것이고, 이 시험의 존재 이유가 바로 그것을 잡는 것이다.
	dstPath := filepath.Join(t.TempDir(), "dst.db")
	// ★ 적용은 기동에서 분리돼 있다(설계 §7 ①) — 열기 전에 올린다.
	if err := store.Migrate(context.Background(), dstPath, nil); err != nil {
		t.Fatalf("DB 적용 실패: %v", err)
	}
	dst, err := store.OpenWithLogger(dstPath, quiet)
	if err != nil {
		t.Fatalf("복원 DB 열기 실패: %v", err)
	}
	defer dst.Close()

	if err := dst.WriteLedger(ctx, got); err != nil {
		t.Fatalf("빈 DB 되쓰기 실패 — 폐포가 안 닫혔다: %v", err)
	}

	final, err := dst.ReadLedger(ctx)
	if err != nil {
		t.Fatalf("복원본 읽기 실패: %v", err)
	}
	if !reflect.DeepEqual(want, final) {
		t.Fatalf("왕복에서 원장이 달라졌다:\n%s", diffLedgerDumps(t, want, final))
	}
}

// 복원한 DB 에서 전문검색이 실제로 동작하는지 본다.
// judgment_fts 는 내보내지 않는데, 그것을 손실 0이라고 주장하는 근거가
// "AFTER INSERT 트리거가 다시 채운다" 하나뿐이다 — 그 주장을 여기서 실측한다.
func TestRestoredDBHasWorkingFullTextSearch(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	dbp1 := filepath.Join(t.TempDir(), "src.db")
	// ★ 적용은 기동에서 분리돼 있다(설계 §7 ①) — 열기 전에 올린다.
	if err := store.Migrate(context.Background(), dbp1, nil); err != nil {
		t.Fatalf("DB 적용 실패: %v", err)
	}
	src, err := store.OpenWithLogger(dbp1, quiet)
	if err != nil {
		t.Fatalf("원본 열기 실패: %v", err)
	}
	seedLedgerFixture(t, src, dbp1)
	dump, err := src.ReadLedger(ctx)
	if err != nil {
		t.Fatalf("원장 읽기 실패: %v", err)
	}
	src.Close()

	dbp2 := filepath.Join(t.TempDir(), "dst.db")
	// ★ 적용은 기동에서 분리돼 있다(설계 §7 ①) — 열기 전에 올린다.
	if err := store.Migrate(context.Background(), dbp2, nil); err != nil {
		t.Fatalf("DB 적용 실패: %v", err)
	}
	dst, err := store.OpenWithLogger(dbp2, quiet)
	if err != nil {
		t.Fatalf("복원 DB 열기 실패: %v", err)
	}
	defer dst.Close()
	// 여기도 seed 를 안 부른다 — 폐포가 닫혔으므로 원장만으로 복원된다.
	if err := dst.WriteLedger(ctx, dump); err != nil {
		t.Fatalf("WriteLedger 실패: %v", err)
	}

	hits, err := dst.SearchJudgments(ctx, "p", "고유낱말", 10)
	if err != nil {
		t.Fatalf("복원본 검색 실패: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("복원 후 전문검색이 아무것도 못 찾는다 — judgment_fts 가 안 채워졌다")
	}
}

// anyJudgmentHasSession 은 SessionID 가 채워진(NULL 이 아닌) 판단이 하나라도 있는지 본다.
func anyJudgmentHasSession(js []store.LedgerJudgment) bool {
	for _, j := range js {
		if j.SessionID != nil {
			return true
		}
	}
	return false
}

// diffLedgerDumps 는 reflect.DeepEqual 이 false 일 때만 부른다(실패 경로 전용, 통과 경로는
// 안 돈다).
//
// ★ 왜 필요한가. `LedgerJudgment` 8필드 중 4개, `LedgerProject` 9필드 중 5개, `LedgerSnapshot`
// 7개 중 2개가 `*string` 이다. `%+v` 로 그대로 찍으면 리뷰가 실측한 대로 주소가 나온다
// (`Title:0x33fcac0a5400`) — 값이 다른데 값을 못 보여주면 다음 사람이 원인을 못 좇는다.
// 이미 import 한 `ledger.Encode` 를 그대로 쓴다 — 그것이 포인터를 JSON null/문자열로
// 이미 펴 두는 함수라 여기서 새로 펴는 코드를 안 써도 된다.
func diffLedgerDumps(t *testing.T, want, got store.LedgerDump) string {
	t.Helper()
	wantFiles, _, err := ledger.Encode(want, store.SchemaVersion, "diff")
	if err != nil {
		return fmt.Sprintf("(진단용 Encode 실패 — 원본: %v)", err)
	}
	gotFiles, _, err := ledger.Encode(got, store.SchemaVersion, "diff")
	if err != nil {
		return fmt.Sprintf("(진단용 Encode 실패 — 복원본: %v)", err)
	}

	names := make([]string, 0, len(wantFiles))
	for name := range wantFiles {
		names = append(names, name)
	}
	sort.Strings(names) // map 순회는 순서가 흔들린다. 출력이 실행마다 같아야 한다.

	var b strings.Builder
	for _, name := range names {
		wantLines := strings.Split(string(wantFiles[name]), "\n")
		gotLines := strings.Split(string(gotFiles[name]), "\n")
		if reflect.DeepEqual(wantLines, gotLines) {
			continue
		}
		fmt.Fprintf(&b, "파일 %s 가 다르다(원본 %d줄 · 복원 %d줄):\n", name, len(wantLines), len(gotLines))
		n := len(wantLines)
		if len(gotLines) > n {
			n = len(gotLines)
		}
		for i := 0; i < n; i++ {
			var wv, gv string
			if i < len(wantLines) {
				wv = wantLines[i]
			}
			if i < len(gotLines) {
				gv = gotLines[i]
			}
			if wv != gv {
				fmt.Fprintf(&b, "  [%d줄] 원본: %s\n  [%d줄] 복원: %s\n", i+1, wv, i+1, gv)
			}
		}
	}
	if b.Len() == 0 {
		// 인코딩된 바이트는 같은데 DeepEqual 은 false — nil 슬라이스 vs 빈 슬라이스 같은
		// Go 값 차이일 가능성이 크다(JSONL 로 펴면 둘 다 "행 0개"로 접혀 안 보인다).
		return fmt.Sprintf("(인코딩된 바이트는 같은데 DeepEqual 이 false 다 — "+
			"nil vs 빈 슬라이스 같은 Go 값 차이로 보인다)\n원본 %+v\n복원 %+v", want, got)
	}
	return b.String()
}

// seedLedgerRefs 는 원장 밖 표(project·session·machine)를 만든다.
func seedLedgerRefs(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	// ★ RemoteURL·Config·ConfigFromSHA 를 채운다. 비워 두면 이 셋의 NULL 왕복만
	//   검증되고 값이 있는 경로는 한 번도 안 지나간다 — LedgerProject 의 *string
	//   포인터 필드 중 값 있는 쪽이 시험되지 않은 채로 남는다.
	if err := s.UpsertProject(ctx, model.Project{
		ID: "p", Path: "/repo/p",
		RemoteURL:     "https://example.com/p.git",
		Config:        `{"lane":"main"}`,
		ConfigFromSHA: "deadbeefcafefeed",
	}); err != nil {
		t.Fatalf("프로젝트 등록 실패: %v", err)
	}
	if err := s.UpsertMachine(ctx, model.Machine{ID: "m1", Hostname: "dev"}); err != nil {
		t.Fatalf("머신 등록 실패: %v", err)
	}
	// label 을 채운다 — 마지막 인자가 그것이다. 비워 두면 LedgerSession.Label 의
	// 값 있는 경로가 시험되지 않는다. BlockedWhy 는 state='blocked' 여야 채워지는데
	// 그건 internal/store 의 TestWriteLedgerRestoresBlockedSession 이 이미 덮는다.
	if _, _, err := s.OpenSession(ctx, "p", "m1", "/w/cc1", "cc1", "메인 세션", time.Time{}); err != nil {
		t.Fatalf("세션 등록 실패: %v", err)
	}
}

// seedLedgerFixture 는 왕복이 실제로 무언가를 보게 하는 데이터를 넣는다 —
// NULL 과 값, supersedes, 여러 target_kind, 전문검색용 고유 낱말. path 는 s 가 연 원장 파일이다(seedIncrementColumns).
func seedLedgerFixture(t *testing.T, s *store.Store, path string) {
	t.Helper()
	ctx := context.Background()
	seedLedgerRefs(t, s)
	defer seedIncrementColumns(t, path)

	sess, _, err := s.OpenSession(ctx, "p", "m1", "/w/cc1", "cc1", "", time.Time{})
	if err != nil {
		t.Fatalf("세션 재개 실패: %v", err)
	}
	if err := s.AddItem(ctx, model.Item{
		Project: "p", ID: "i1", Title: "i1", Body: "본문", Paths: []string{"services/"},
	}); err != nil {
		t.Fatalf("항목 등록 실패: %v", err)
	}

	// ① 세션·제목이 있고 링크가 둘(종류가 다르다)
	first, err := s.AddJudgment(ctx, model.Judgment{
		Project: "p", SessionID: sess.ID, Kind: model.JudgmentDecision,
		Title: "결정", Body: "고유낱말 이 들어간 본문",
		Links: []model.JudgmentLink{
			{TargetKind: "item", TargetID: "i1"},
			{TargetKind: "commit", TargetID: "deadbeef"},
		},
	})
	if err != nil {
		t.Fatalf("판단① 저장 실패: %v", err)
	}

	// ② supersedes 가 걸린 정정
	if _, err := s.AddJudgment(ctx, model.Judgment{
		Project: "p", SessionID: sess.ID, Kind: model.JudgmentDecision,
		Title: "정정", Body: "앞 판단을 대체한다", Supersedes: first.ID,
	}); err != nil {
		t.Fatalf("판단② 저장 실패: %v", err)
	}

	// ③ project·session·title 이 전부 NULL 인 판단 — 포인터가 아니면 여기서 티가 난다
	if _, err := s.AddJudgment(ctx, model.Judgment{
		Kind: model.JudgmentDecision, Body: "좌표 없는 판단",
	}); err != nil {
		t.Fatalf("판단③ 저장 실패: %v", err)
	}

	// ★ 막힌 세션 하나 — session.blocked_why 축이다.
	//
	// nullable 전제 관문(TestRoundTripFixtureExercisesEveryNullableColumn)이 이 구멍을
	// 찾아냈다. 이것이 없으면 읽기가 blocked_why 를 버려도 왕복이 원리적으로 못 본다 —
	// want·final 이 둘 다 ReadLedger 산출물이라 양쪽에 똑같이 없기 때문이다.
	blocked, _, err := s.OpenSession(ctx, "p", "m1", "/w/cc2", "cc2", "막힌 세션", time.Time{})
	if err != nil {
		t.Fatalf("둘째 세션 열기 실패: %v", err)
	}
	if err := s.SetSessionState(ctx, blocked.ID, model.SessionBlocked, "왜 막혔는지"); err != nil {
		t.Fatalf("세션 막기 실패: %v", err)
	}

	// 스냅숏 둘 — evidence 가 있는 것과 없는 것
	if err := s.PutSnapshot(ctx, model.Snapshot{
		Project: "p", Key: "manual-key", Value: "12", Method: model.SnapshotManual,
		Evidence: "손으로 셌다", InputDigest: "abc",
	}); err != nil {
		t.Fatalf("스냅숏① 저장 실패: %v", err)
	}
	if err := s.PutSnapshot(ctx, model.Snapshot{
		Project: "p", Key: "cmd-key", Value: "7", Method: model.SnapshotCommand,
	}); err != nil {
		t.Fatalf("스냅숏② 저장 실패: %v", err)
	}
}

// 비밀 칸에 심는 값. 원장 파일 어디에도 나오면 안 된다(TestLedgerSurvivesFullRoundTrip).
const (
	fixturePasswordHash = "$argon2id$v=19$fixture-secret"
	fixtureTokenHash    = "fixture-token-digest"
)

// seedIncrementColumns 는 증분이 폐포 표에 더한 nullable 칸을 채운다: 계정 표와 증분 020 의 칸(기기 · 세션의 계정, 판단의
// 작성자 · 갈래), 그리고 그 전 증분의 칸(project.pinned_at · archived_at, session.harness, judgment_link.target_project). 020 의
// 칸을 쓰는 저장 계층이 아직 없어(store/account.go 머리말) 원장 파일에 SQL 로 바로 쓴다 - 옛 칸도 같은 자리에서 채워 이 관문의
// 입력을 한 곳에 둔다. 판단은 추가 전용이라 칸을 채운 새 행을 넣는다.
func seedIncrementColumns(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("원장 파일 열기 실패: %v", err)
	}
	defer db.Close()
	const at = "2026-08-06T00:00:05.000000Z"
	for _, q := range []string{
		`INSERT INTO account(id, login, display_name, email, role, password_hash, created_at, disabled_at) VALUES
		   ('a1', 'alice', '앨리스', 'a@example.invalid', 'admin', '` + fixturePasswordHash + `', '` + at + `', NULL),
		   ('a2', 'bob', '밥', NULL, 'member', NULL, '` + at + `', '` + at + `')`,
		`UPDATE machine SET account_id = 'a1', name = '개발 기기', token_hash = '` + fixtureTokenHash + `', token_prefix = 'fd_abcde',
		   token_origin = 'https://fd.example.invalid', issued_at = '` + at + `', revoked_at = '` + at + `', revoked_by = 'a2'
		 WHERE id = 'm1'`,
		`UPDATE session SET account_id = 'a1' WHERE cc_session_id = 'cc1'`,
		`INSERT INTO judgment(id, project, session_id, at, kind, title, body, supersedes, author, via) VALUES
		   ('01ZACCOUNT', 'p', NULL, '` + at + `', 'decision', '[fdboard] 노트', '보드가 남긴 노트', NULL, 'a1', 'web')`,
		`INSERT INTO judgment_link(judgment_id, target_kind, target_id, target_project) VALUES ('01ZACCOUNT', 'item', 'i1', 'p')`,
		`UPDATE project SET pinned_at = '` + at + `', archived_at = '` + at + `' WHERE id = 'p'`,
		`UPDATE session SET harness = 'codex' WHERE cc_session_id = 'cc1'`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("계정 칸 심기 실패(%s): %v", q, err)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 왕복 시험의 **입력**을 지킨다 — 읽기가 버린 값은 왕복이 원리적으로 못 본다
// ─────────────────────────────────────────────────────────────────────────────
//
// TestLedgerSurvivesFullRoundTrip 의 want·final 은 **둘 다 ReadLedger 산출물**이다.
// 그래서 읽기가 버린 컬럼은 양쪽에 똑같이 없고 reflect.DeepEqual 이 원리적으로 감지
// 못 한다. 리뷰가 실측으로 재현했다 — readLedgerJudgments 에서 `j.Title, j.Supersedes =
// nil, nil` 로 바꿔도 internal/store·internal/ledger 둘 다 초록이었다.
//
// 그래서 want 자체를 본다: **nullable 컬럼마다 값이 실린 행이 하나는 있어야 한다.**
// 읽기가 그 컬럼을 버리면 want 에 값이 없고 이 단정이 그 자리에서 죽는다.
// (anyJudgmentHasSession·픽스처 개수 단정이 이미 같은 모양이다 — 이것은 그 축의 완성이다.)
//
// ★ 컬럼 목록을 손으로 안 적는다. 스키마에서 nullable 을 뽑고 DTO 의 json 태그로 잇는다.
// 손으로 적으면 새 nullable 컬럼이 조용해지고, 실제로 이 축을 처방한 항목 본문이 "여섯"을
// 적었는데 실측하면 **열하나**였다.
//
// ★ 스키마는 증분까지 올린 원장의 카탈로그다(PRAGMA table_info). schema.sql 은 판 1 의 글이라 증분이 더한 표 · 칸
// (account, judgment.author 같은 것)이 거기 없다 - 글을 읽으면 증분이 더한 nullable 칸이 이 관문을 말없이 지나간다.
// 원장이 일부러 안 싣는 비밀 칸(store.LedgerSecretColumns)은 뺀다.
func TestRoundTripFixtureExercisesEveryNullableColumn(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	srcPath := filepath.Join(t.TempDir(), "src.db")
	// ★ 적용은 기동에서 분리돼 있다(설계 §7 ①) — 열기 전에 올린다.
	if err := store.Migrate(context.Background(), srcPath, nil); err != nil {
		t.Fatalf("DB 적용 실패: %v", err)
	}
	src, err := store.OpenWithLogger(srcPath, quiet)
	if err != nil {
		t.Fatalf("원본 DB 열기 실패: %v", err)
	}
	defer src.Close()
	seedLedgerFixture(t, src, srcPath)

	want, err := src.ReadLedger(ctx)
	if err != nil {
		t.Fatalf("원장 읽기 실패: %v", err)
	}

	rows := map[string]any{
		"account":       want.Accounts,
		"project":       want.Projects,
		"machine":       want.Machines,
		"session":       want.Sessions,
		"judgment":      want.Judgments,
		"snapshot":      want.Snapshots,
		"judgment_link": want.Links,
	}
	checked := 0
	for _, table := range store.LedgerTableNames() {
		slice, ok := rows[table]
		if !ok {
			t.Fatalf("폐포 표 %q 의 DTO 를 이 시험이 모른다 — 표가 늘었으면 여기도 이어라", table)
		}
		for _, col := range liveNullableColumns(t, srcPath, table) {
			checked++
			assertSomeRowCarries(t, table, col, slice)
		}
	}
	if checked == 0 {
		t.Fatal("nullable 컬럼을 하나도 못 뽑았다 — 이 관문이 아무것도 안 보면서 초록이 된다")
	}
	t.Logf("nullable 컬럼 %d개를 픽스처가 전부 채운다", checked)
}

// assertSomeRowCarries 는 그 컬럼에 값이 실린 행이 하나라도 있는지 본다.
func assertSomeRowCarries(t *testing.T, table, col string, slice any) {
	t.Helper()
	v := reflect.ValueOf(slice)
	if v.Kind() != reflect.Slice || v.Len() == 0 {
		t.Errorf("%s 의 행이 하나도 없다 — nullable 축을 볼 수가 없다", table)
		return
	}
	et := v.Type().Elem()
	idx := -1
	for i := 0; i < et.NumField(); i++ {
		if strings.Split(et.Field(i).Tag.Get("json"), ",")[0] == col {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Errorf("%s.%s 를 담는 DTO 필드가 없다 — 원장이 이 컬럼을 통째로 안 담는다", table, col)
		return
	}
	if et.Field(idx).Type.Kind() != reflect.Ptr {
		t.Errorf("%s.%s 의 DTO 필드가 포인터가 아니다 — NULL 과 빈 값이 한 값으로 접힌다", table, col)
		return
	}
	for i := 0; i < v.Len(); i++ {
		if !v.Index(i).Field(idx).IsNil() {
			return
		}
	}
	t.Errorf("%s.%s 에 값이 실린 행이 픽스처에 하나도 없다.\n"+
		"왕복 시험은 want·final 이 둘 다 ReadLedger 산출물이라 **읽기가 이 컬럼을 버려도** "+
		"양쪽에 똑같이 없어 DeepEqual 이 원리적으로 못 본다. 픽스처에 이 컬럼을 채운 행을 "+
		"넣거나, 읽기가 정말 이 컬럼을 버리고 있는지 확인해라.", table, col)
}

// liveNullableColumns 는 path 원장의 표 하나에서 NULL 이 될 수 있는 칸을 선언 순서로 낸다(기본키 · NOT NULL 은 빠진다).
// 원장이 일부러 안 싣는 비밀 칸도 뺀다.
func liveNullableColumns(t *testing.T, path, table string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("원장 파일 열기 실패: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name, "notnull", pk FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatalf("%s 칸 목록 조회 실패: %v", table, err)
	}
	defer rows.Close()
	secret := map[string]bool{}
	for _, c := range store.LedgerSecretColumns() {
		secret[c] = true
	}
	var out []string
	n := 0
	for rows.Next() {
		var name string
		var notNull, pk int
		if err := rows.Scan(&name, &notNull, &pk); err != nil {
			t.Fatalf("%s 칸 목록 해석 실패: %v", table, err)
		}
		n++
		if notNull == 0 && pk == 0 && !secret[table+"."+name] {
			out = append(out, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s 칸 목록 순회 실패: %v", table, err)
	}
	if n == 0 {
		t.Fatalf("%s 표가 원장에 없다 — 이 관문의 좌표가 틀렸다", table)
	}
	return out
}
