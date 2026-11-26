package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
)

// project.go · move.go · amend.go · workspace.go · conversation.go · inbox.go · deploy.go · prescribe_reach.go 와
// 보드가 읽는 표(선점 · 큐 판정 · 잡 · 닫힌 항목)의 업무 SQL 이 두 엔진에서 같은 답을 내는지 잰다. 모든 단정을 SQLite
// 파일과 Postgres 시험 DB 에서 한 번씩 돈다(bothEngines).
//
// ★ Postgres 에서 실패하던 자리 셋(이동의 FK 검사 미루기 PRAGMA, 확인율 대화 키의 char(31), 대화 줄 이력의 EXISTS 를
// 정수로 읽기)과 원장 payload 판독 둘(대화의 done 항목, 마지막 배포 바이너리), 보드 목록의 같은 시각 끝맺음, 보드 목록이
// 자기가 안 찍는 칸의 깨진 값을 견디는 것은 아래 시험들이 따로 잠근다. 원장 되쓰기의 FK 미루기는 backup_test.go 가 잰다.
// 첫 시험은 범위 파일의 나머지 공개 질의를 실제 호출로 한 번씩 다 지나게 한다.

// TestProjectWorkspaceInboxDeploySQLRunsOnBothEngines 는 범위 파일의 공개 질의를 한 줄거리로 부른다.
func TestProjectWorkspaceInboxDeploySQLRunsOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		seed(t, s, "q")
		mustItem(t, s, "p", "it")
		a := mustSession(t, s, "p", "cc-a")

		// ── 프로젝트 · 기계
		if err := s.UpsertProject(ctx, model.Project{ID: "p", Path: "/repo/p2", DefaultBranch: "main"}); err != nil {
			t.Fatalf("프로젝트 다시 등록 실패: %v", err)
		}
		if p, err := s.GetProject(ctx, "p"); err != nil || p.Path != "/repo/p2" || p.DefaultBranch != "main" {
			t.Fatalf("다시 등록한 프로젝트가 안 읽힌다: %+v %v", p, err)
		}
		pinned := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := s.SetProjectView(ctx, "p", pinned, time.Time{}); err != nil {
			t.Fatalf("보기 표시 실패: %v", err)
		}
		if p, err := s.GetProject(ctx, "p"); err != nil || !p.PinnedAt.Equal(pinned) || !p.ArchivedAt.IsZero() {
			t.Fatalf("고정 표시가 안 읽힌다: pinned=%v archived=%v err=%v", p.PinnedAt, p.ArchivedAt, err)
		}
		ps, err := s.ListProjects(ctx)
		if err != nil {
			t.Fatalf("프로젝트 목록 실패: %v", err)
		}
		var ids []string
		for _, p := range ps {
			ids = append(ids, p.ID)
		}
		if !slices.Contains(ids, "p") || !slices.Contains(ids, "q") {
			t.Fatalf("프로젝트 목록 = %v (p · q 가 있어야 한다)", ids)
		}
		if err := s.UpsertMachine(ctx, model.Machine{ID: "m2", Hostname: "other"}); err != nil {
			t.Fatalf("기계 등록 실패: %v", err)
		}
		if m, err := s.GetMachine(ctx, "m2"); err != nil || m.Hostname != "other" {
			t.Fatalf("기계가 안 읽힌다: %+v %v", m, err)
		}

		// ── 참조 셈 · 지우기
		counts, err := s.ProjectRefCounts(ctx, "p")
		if err != nil || counts["item"] != 1 || counts["session"] != 1 {
			t.Fatalf("p 의 참조 셈 = %v %v (item 1 · session 1)", counts, err)
		}
		all, err := s.ProjectRefCountsAll(ctx)
		if err != nil || RefCountsOf(all, "p")["item"] != 1 || RefCountsOf(all, "q")["item"] != 0 {
			t.Fatalf("전체 참조 셈 = %v %v", all, err)
		}
		if err := s.UpsertProject(ctx, model.Project{ID: "gone", Path: "/repo/gone"}); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveProject(ctx, "gone"); err != nil {
			t.Fatalf("참조 없는 프로젝트를 못 지웠다: %v", err)
		}
		if _, err := s.GetProject(ctx, "gone"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("지운 프로젝트가 아직 읽힌다: %v", err)
		}
		var refused *RemovalRefusedError
		if err := s.RemoveProject(ctx, "p"); !errors.As(err, &refused) || refused.Counts["item"] != 1 {
			t.Fatalf("항목이 있는 프로젝트 지우기가 거절되지 않았다: %v", err)
		}

		// ── 작업공간 명부
		at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		if err := s.ReplaceWorkspaceMembers(ctx, "p", []model.WorkspaceMember{{Project: "q", Path: "/repo/q"}}, at); err != nil {
			t.Fatalf("명부 등재 실패: %v", err)
		}
		if ms, err := s.WorkspaceMembers(ctx, "p"); err != nil || len(ms) != 1 || ms[0].Project != "q" || ms[0].Path != "/repo/q" {
			t.Fatalf("명부 = %+v %v", ms, err)
		}
		if root, err := s.WorkspaceRootOf(ctx, "q"); err != nil || root != "p" {
			t.Fatalf("q 의 뿌리 = %q %v (p)", root, err)
		}
		if roots, err := s.WorkspaceRootsOf(ctx, "q"); err != nil || !slices.Equal(roots, []string{"p"}) {
			t.Fatalf("q 의 뿌리들 = %v %v ([p])", roots, err)
		}
		if err := s.SetWorkspaceSHA(ctx, "p", "abc123"); err != nil {
			t.Fatalf("명부 신선도 기록 실패: %v", err)
		}
		if p, err := s.GetProject(ctx, "p"); err != nil || p.ConfigFromSHA != "abc123" {
			t.Fatalf("명부 신선도 = %q %v", p.ConfigFromSHA, err)
		}
		if err := s.SetWorkspaceSHA(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("없는 프로젝트의 신선도 기록이 ErrNotFound 가 아니다: %v", err)
		}

		// ── 받은편지함 · 옛 등록 판정
		if _, err := s.PutInbox(ctx, a.ID, "/run/a.sock", "default", at); err != nil {
			t.Fatalf("받은편지함 등록 실패: %v", err)
		}
		if box, err := s.ListInbox(ctx, "p", "m1"); err != nil || len(box) != 1 || box[0].SessionID != a.ID || box[0].CCSessionID != "cc-a" {
			t.Fatalf("받은편지함 = %+v %v", box, err)
		}
		live, err := s.LiveSessionsOf(ctx, "p", "m1", []LegacyAsk{{CC: "cc-a", Dir: "/w/cc-a/sub"}, {CC: "cc-x", Dir: "/w/cc-x"}})
		if err != nil || !slices.Equal(live, []string{"cc-a"}) {
			t.Fatalf("옛 등록 판정 = %v %v ([cc-a])", live, err)
		}
		if n, err := s.DropInbox(ctx, a.ID, "/run/a.sock"); err != nil || n != 1 {
			t.Fatalf("받은편지함 지우기 = %d %v (1)", n, err)
		}

		// ── 개정
		if err := s.Tx(ctx, func(x *Tx) error {
			_, e := x.AmendItem("p", "it", AmendPatch{Title: strp("고친 제목"), Reason: "오타"}, a.ID)
			return e
		}); err != nil {
			t.Fatalf("개정 실패: %v", err)
		}
		if revs, err := s.ItemRevisions(ctx, "p", "it"); err != nil || len(revs) != 1 || revs[0].Rev != 1 || revs[0].Title != "it" {
			t.Fatalf("개정 이력 = %+v %v (rev 1 · 옛 제목 it)", revs, err)
		}

		// ── 배포 기록: 마지막 배포는 id 가 가장 큰 행이다.
		if _, ok, err := s.LastDeployAt(ctx); err != nil || ok {
			t.Fatalf("배포 전 마지막 배포 = %v %v (없음)", ok, err)
		}
		for i, c := range []struct {
			exe  string
			want bool
		}{{"/bin/a", true}, {"/bin/a", false}, {"/bin/b", true}, {"/bin/a", true}} {
			if got, err := s.NoteServerBuild(ctx, c.exe); err != nil || got != c.want {
				t.Fatalf("%d번째 NoteServerBuild(%s) = %v %v (%v)", i, c.exe, got, err, c.want)
			}
		}
		if _, ok, err := s.LastDeployAt(ctx); err != nil || !ok {
			t.Fatalf("배포 뒤 마지막 배포 = %v %v (있음)", ok, err)
		}
	})
}

// TestMoveItemDefersForeignKeysOnBothEngines 는 딸린 행이 FK 로 걸린 항목이 두 엔진에서 옮겨지는지 잰다.
//
// ★ Postgres 기준선은 FK 를 DEFERRABLE INITIALLY IMMEDIATE 로 선언한다. 항목 행을 먼저 옮기는 순간 선점 · 선행 · 개정
// 행이 옛 좌표를 가리키므로, 검사를 커밋까지 미루지 않으면 첫 UPDATE 가 막힌다(SQLite 의 PRAGMA 는 Postgres 에서
// 구문 오류였다).
func TestMoveItemDefersForeignKeysOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "from")
		seed(t, s, "to")
		mustItem(t, s, "from", "it")
		mustItem(t, s, "from", "next")
		a := mustSession(t, s, "from", "cc-a")

		// 반납된 선점(claim FK), 개정(item_revision FK), 이 항목을 선행으로 둔 다른 항목(item_after), 잡 행(FK 없음).
		if _, err := s.ClaimItem(ctx, "from", "it", a.ID, time.Time{}); err != nil {
			t.Fatalf("선점 실패: %v", err)
		}
		if err := s.ReleaseClaim(ctx, "from", "it", a.ID); err != nil {
			t.Fatalf("반납 실패: %v", err)
		}
		if err := s.Tx(ctx, func(x *Tx) error {
			_, e := x.AmendItem("from", "it", AmendPatch{Title: strp("새 제목"), Reason: "고침"}, a.ID)
			return e
		}); err != nil {
			t.Fatalf("개정 실패: %v", err)
		}
		pgExec(t, s, `INSERT INTO item_after(project, item_id, dep_item) VALUES (?, ?, ?)`, "from", "it", "next")
		pgExec(t, s, `INSERT INTO job(id, project, kind, item_id, state, queued_at) VALUES (?, ?, 'land', ?, 'ok', ?)`,
			"job-1", "from", "it", fmtTime(time.Now().UTC()))
		j, err := s.AddJudgment(ctx, model.Judgment{
			Project: "from", SessionID: a.ID, Kind: model.JudgmentDecision, Body: "따라가야 할 판단",
			Links: []model.JudgmentLink{{TargetKind: "item", TargetID: "it"}},
		})
		if err != nil {
			t.Fatalf("판단 저장 실패: %v", err)
		}

		if _, err := s.MoveItem(ctx, "from", "it", "to", a.ID); err != nil {
			t.Fatalf("딸린 행이 있는 항목을 못 옮겼다: %v", err)
		}

		for _, tbl := range itemKeyedTables {
			if n := countIn(t, s, tbl, "from", "it"); n != 0 {
				t.Errorf("%s 에 옛 프로젝트 행이 %d건 남았다", tbl, n)
			}
			if n := countIn(t, s, tbl, "to", "it"); n != 1 {
				t.Errorf("%s 가 대상 프로젝트로 %d건 따라왔다(1건)", tbl, n)
			}
		}
		if _, err := s.GetItem(ctx, "to", "it"); err != nil {
			t.Fatalf("옮긴 항목이 대상에서 안 읽힌다: %v", err)
		}
		if js, err := s.JudgmentsForItem(ctx, "to", "it"); err != nil || len(js) != 1 || js[0].ID != j.ID {
			t.Fatalf("옮긴 항목에서 판단이 안 보인다: %d건 %v", len(js), err)
		}
		if n := pgCount(t, s, `SELECT COUNT(*) FROM event WHERE kind = 'item.move' AND project = ?`, "to"); n != 1 {
			t.Fatalf("이동 원장이 대상 프로젝트에 %d건(1건)", n)
		}
	})
}

// TestConversationLifecycleOnBothEngines 는 대화 라이프사이클의 줄 이력 · done 항목 판독을 두 엔진에서 잰다.
//
// ★ 줄 이력(EXISTS)은 Postgres 에서 불리언이라 정수로 읽으면 Scan 이 실패했다. done 항목은 원장 payload 판독기
// 하나로 읽는다: 중복 키는 첫 값, 키는 글자 그대로, 결말 칸(tx)이 문자열이 아니면 「결말 없음」이라 센다.
// 구조체 풀기는 셋 다 거꾸로 읽었다(끝 값 · 대소문자 무시 · 형이 안 맞으면 행 전체를 버림).
func TestConversationLifecycleOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		a, _, err := s.OpenSession(ctx, "p", "m1", "/w/A", "cc-shared", "", time.Time{})
		if err != nil {
			t.Fatalf("카드 A 등록 실패: %v", err)
		}
		b, _, err := s.OpenSession(ctx, "p", "m1", "/w/B", "cc-shared", "", time.Time{})
		if err != nil {
			t.Fatalf("카드 B 등록 실패: %v", err)
		}

		got, err := s.ConversationLifecycle(ctx, "p", a.ID)
		if err != nil {
			t.Fatalf("줄 서기 전 조회 실패: %v", err)
		}
		if got.EverEnqueued {
			t.Fatal("줄에 선 적이 없는데 EverEnqueued 가 참이다")
		}
		if len(got.SessionIDs) != 2 || !slices.IsSorted(got.SessionIDs) {
			t.Fatalf("형제 카드 = %v (둘 · id 순)", got.SessionIDs)
		}

		if err := s.Tx(ctx, func(x *Tx) error {
			_, e := x.EnqueueLanding("p", b.ID, []string{"landing"}, time.Time{})
			return e
		}); err != nil {
			t.Fatalf("카드 B 줄서기 실패: %v", err)
		}

		// 원장 payload 는 표준 JSON 이면 무엇이든 올 수 있다 - 판독 규칙을 행마다 하나씩 건다.
		after := fmtTime(time.Now().UTC().Add(time.Minute))
		for _, payload := range []string{
			`{"item":"x1","mode":"done","tx":5}`,             // 결말 칸이 글이 아니다 → 「결말 없음」이라 센다
			`{"item":"x2","item":"x3","mode":"done"}`,        // 중복 키 → 첫 값 x2
			`{"Item":"x4","mode":"done"}`,                    // 키 대소문자가 다르다 → item 이 없다
			`{"item":"x5","mode":"done","tx":"rolled_back"}`, // 롤백된 시도
			`{"item":"x6","mode":"dropped"}`,                 // done 이 아니다
			`{"item":"x1","mode":"done"}`,                    // 이미 센 항목
		} {
			pgExec(t, s, `INSERT INTO event(at, project, session_id, kind, payload) VALUES (?, ?, ?, 'item.finish', ?)`,
				after, "p", a.ID, payload)
		}

		got, err = s.ConversationLifecycle(ctx, "p", b.ID)
		if err != nil {
			t.Fatalf("줄 서기 뒤 조회 실패: %v", err)
		}
		if !got.EverEnqueued {
			t.Fatal("B 가 줄에 섰는데 EverEnqueued 가 거짓이다")
		}
		if got.LaneRow == nil || got.LaneRow.SessionID != b.ID {
			t.Fatalf("B 의 줄 행이 안 보인다: %+v", got.LaneRow)
		}
		if want := []string{"x1", "x2"}; !slices.Equal(got.DoneItems, want) {
			t.Fatalf("done 항목 = %v (%v)", got.DoneItems, want)
		}
	})
}

// TestAckReachFoldsConversationsOnBothEngines 는 확인율 도달성이 대화 키(기계 · cc)로 카드를 접는지 두 엔진에서 잰다.
//
// ★ 대화 키는 두 칸을 구분자로 이어 만든다. Postgres 는 SQL 안의 char(31) 을 형 이름으로 읽어 질의 전체가 구문
// 오류였다. 발화는 카드 A 가, 판단은 같은 대화의 카드 B 가 남겨야 「판단에 닿는다」가 나온다 - 키가 접혀야 맞는다.
func TestAckReachFoldsConversationsOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		a, _, err := s.OpenSession(ctx, "p", "m1", "/w/A", "cc-shared", "", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		b, _, err := s.OpenSession(ctx, "p", "m1", "/w/B", "cc-shared", "", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		lone := mustSession(t, s, "p", "cc-lone")

		s.LogEvent(ctx, "prescribe", "p", a.ID, map[string]any{"key": "k"})
		s.LogEvent(ctx, "prescribe", "p", b.ID, map[string]any{"key": "k"})
		s.LogEvent(ctx, "prescribe_ack", "p", a.ID, map[string]any{"key": "k"})
		s.LogEvent(ctx, "prescribe", "p", lone.ID, map[string]any{"key": "k"})
		if _, err := s.AddJudgment(ctx, model.Judgment{
			Project: "p", SessionID: b.ID, Kind: model.JudgmentDecision, Body: "B 의 판단",
		}); err != nil {
			t.Fatalf("판단 저장 실패: %v", err)
		}

		all, recent, err := s.AckReach(ctx, "p", time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("확인율 도달성 조회 실패: %v", err)
		}
		want := AckCounts{Emitted: 2, Reachable: 1, Acked: 1}
		if all != want || recent != want {
			t.Fatalf("도달성 all=%+v recent=%+v (%+v)", all, recent, want)
		}
		if _, recent, err := s.AckReach(ctx, "p", time.Now().Add(time.Hour)); err != nil || recent != (AckCounts{}) {
			t.Fatalf("미래 기준의 최근 도달성 = %+v %v (0)", recent, err)
		}
	})
}

// TestNoteServerBuildReadsTheFirstExeOnBothEngines 는 마지막 배포의 바이너리를 원장 판독기로 읽는지 잰다.
//
// ★ 중복 키는 첫 값이다(json_extract 와 같다). 구조체 풀기는 끝 값을 읽어, 같은 바이너리를 새 배포로 또 적었다.
func TestNoteServerBuildReadsTheFirstExeOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		pgExec(t, s, `INSERT INTO event(at, kind, payload) VALUES (?, ?, ?)`,
			fmtTime(time.Now().UTC()), eventServerDeploy, `{"exe":"/bin/a","exe":"/bin/b"}`)
		if got, err := s.NoteServerBuild(ctx, "/bin/a"); err != nil || got {
			t.Fatalf("마지막 배포가 /bin/a 인데 새 배포로 적었다: %v %v", got, err)
		}
		pgExec(t, s, `INSERT INTO event(at, kind, payload) VALUES (?, ?, ?)`,
			fmtTime(time.Now().UTC()), eventServerDeploy, `{"Exe":"/bin/a"}`)
		if got, err := s.NoteServerBuild(ctx, "/bin/a"); err != nil || !got {
			t.Fatalf("마지막 배포 행에 exe 가 없는데(대소문자가 다르다) 같은 바이너리로 읽었다: %v %v", got, err)
		}
	})
}

// TestBoardListsBreakTiesOnBothEngines 는 보드가 읽는 세 목록(살아 있는 선점 · 큐 판정 · 미확인 잡)의 순서 · 거르기 ·
// 개수 제한을 두 엔진에서 잰다.
//
// ★ 같은 시각의 행은 끝맺음 칸이 순서를 정한다. 적지 않으면 순서를 엔진이 고르고, limit 이 자르는 행도 엔진마다
// 달라진다. 행은 기대 순서의 **반대**로 넣는다 - 끝맺음이 빠지면 넣은 순서가 그대로 드러난다.
// ★ limit 음수는 「전부」다. Postgres 는 음수 LIMIT 을 오류로 거절한다.
func TestBoardListsBreakTiesOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		seed(t, s, "q")
		a := mustSession(t, s, "p", "cc-a")
		t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		for _, id := range []string{"a", "b", "z", "r"} {
			mustItem(t, s, "p", id)
		}

		// ── 살아 있는 선점: 쥔 시각 순, 같은 시각은 항목 id 순. 반납된 것은 빠진다.
		for _, c := range []struct {
			item     string
			at       time.Time
			released any
		}{{"b", t0, nil}, {"a", t0, nil}, {"z", t0.Add(-time.Minute), nil}, {"r", t0.Add(-2 * time.Minute), fmtTime(t0)}} {
			pgExec(t, s, `INSERT INTO claim(project, item_id, session_id, at, released_at) VALUES (?, ?, ?, ?, ?)`,
				"p", c.item, a.ID, fmtTime(c.at), c.released)
		}
		claims, err := s.ListLiveClaims(ctx, "p")
		if err != nil {
			t.Fatalf("살아 있는 선점 조회 실패: %v", err)
		}
		var held []string
		for _, c := range claims {
			held = append(held, c.ItemID)
			if c.ReleasedAt != nil || c.SessionID != a.ID {
				t.Fatalf("살아 있는 선점 행이 이상하다: %+v", c)
			}
		}
		if want := []string{"z", "a", "b"}; !slices.Equal(held, want) {
			t.Fatalf("살아 있는 선점 = %v (%v)", held, want)
		}

		// ── 큐 판정: 최신순(id 내림차순), 다른 프로젝트는 빠진다.
		for i, e := range []model.PickEval{
			{Project: "p", SessionID: a.ID, At: t0, Picked: "a", Rejected: []model.Rejection{{Item: "b", Reason: "after", Detail: "d"}}},
			{Project: "p", SessionID: a.ID, At: t0, Picked: ""},
			{Project: "p", SessionID: a.ID, At: t0, Picked: "z", PickedWith: []string{"r"}},
			{Project: "q", SessionID: a.ID, At: t0, Picked: "q-only"},
		} {
			if err := s.RecordPickEval(ctx, e); err != nil {
				t.Fatalf("%d번째 큐 판정 기록 실패: %v", i, err)
			}
		}
		evals, err := s.ListPickEvals(ctx, "p", -1)
		if err != nil {
			t.Fatalf("큐 판정 조회 실패(전부): %v", err)
		}
		var picked []string
		for _, e := range evals {
			picked = append(picked, e.Picked)
		}
		if want := []string{"z", "", "a"}; !slices.Equal(picked, want) {
			t.Fatalf("큐 판정 = %q (%q)", picked, want)
		}
		// 묶음 구성원은 안 읽는다(ListPickEvals 의 ★) - 묶음으로 나간 판정도 PickedWith 가 비어 나온다.
		if len(evals[0].PickedWith) != 0 || evals[0].SessionID != a.ID ||
			len(evals[2].Rejected) != 1 || evals[2].Rejected[0].Reason != "after" || !evals[2].At.Equal(t0) {
			t.Fatalf("큐 판정 칸이 안 돌아왔다: %+v", evals)
		}
		if two, err := s.ListPickEvals(ctx, "p", 2); err != nil || len(two) != 2 || two[0].Picked != "z" || two[1].Picked != "" {
			t.Fatalf("큐 판정 둘 = %+v %v", two, err)
		}

		// ── 미확인 잡: 줄 선 시각의 최신순, 같은 시각은 id 내림차순. 확인했거나 안 끝난 잡 · 다른 프로젝트는 빠진다.
		ended := t0.Add(time.Hour)
		for _, j := range []struct {
			id, project, state string
			queued             time.Time
			failKind, endedAt  any
			ackAt              any
		}{
			{"j0", "p", "ok", t0.Add(-time.Minute), nil, fmtTime(ended), nil},
			{"j1", "p", "fail", t0, "verify", nil, nil},
			{"j2", "p", "stalled", t0, nil, nil, nil},
			{"j-acked", "p", "ok", t0.Add(time.Minute), nil, nil, fmtTime(ended)},
			{"j-running", "p", "running", t0.Add(time.Minute), nil, nil, nil},
			{"j-q", "q", "ok", t0.Add(time.Minute), nil, nil, nil},
		} {
			pgExec(t, s, `INSERT INTO job(id, project, kind, item_id, state, fail_kind, queued_at, ended_at, ack_at, log_tail)
				VALUES (?, ?, 'land', 'a', ?, ?, ?, ?, ?, ?)`,
				j.id, j.project, j.state, j.failKind, fmtTime(j.queued), j.endedAt, j.ackAt, "tail-"+j.id)
		}
		jobs, err := s.UnackedJobs(ctx, "p", -1)
		if err != nil {
			t.Fatalf("미확인 잡 조회 실패(전부): %v", err)
		}
		var jids []string
		for _, j := range jobs {
			jids = append(jids, j.ID)
		}
		if want := []string{"j2", "j1", "j0"}; !slices.Equal(jids, want) {
			t.Fatalf("미확인 잡 = %v (%v)", jids, want)
		}
		if j := jobs[1]; j.FailKind != "verify" || j.ItemID != "a" || j.LogTail != "tail-j1" || !j.EndedAt.IsZero() {
			t.Fatalf("j1 칸이 안 돌아왔다: %+v", j)
		}
		if !jobs[2].EndedAt.Equal(ended) {
			t.Fatalf("j0 끝난 시각 = %v (%v)", jobs[2].EndedAt, ended)
		}
		if one, err := s.UnackedJobs(ctx, "p", 1); err != nil || len(one) != 1 || one[0].ID != "j2" {
			t.Fatalf("미확인 잡 하나 = %+v %v (j2)", one, err)
		}
	})
}

// TestBoardReadersSkipColumnsTheyDoNotShowOnBothEngines 는 보드의 두 목록(큐 판정 · 닫힌 항목 요약)이 자기가 쓰지 않는
// 칸의 깨진 값 때문에 목록 전체를 잃지 않는지, 그리고 쓰는 칸의 깨진 값은 삼키지 않는지 두 엔진에서 잰다.
//
// ★ 깨진 값은 저장 API 를 거치지 않고 표에 직접 쓴다(RecordPickEval · AddItem 은 늘 바른 JSON 을 쓴다). 두 엔진의
// 스키마 모두 그 칸을 text 로 두어 막지 않는다.
func TestBoardReadersSkipColumnsTheyDoNotShowOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		a := mustSession(t, s, "p", "cc-a")
		t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

		// ── 큐 판정: 빈 rejected 는 「탈락 0줄」인 판정 1회다. picked_with 는 안 읽으므로 깨져도 상관없다.
		ins := `INSERT INTO pick_eval(project, session_id, at, picked, picked_with, rejected) VALUES (?, ?, ?, ?, ?, ?)`
		pgExec(t, s, ins, "p", a.ID, fmtTime(t0), "a", nil, `[{"item":"b","reason_code":"after","detail":"d"}]`)
		pgExec(t, s, ins, "p", a.ID, fmtTime(t0), nil, nil, "")
		pgExec(t, s, ins, "p", a.ID, fmtTime(t0), "z", `{깨짐`, `[]`)
		evals, err := s.ListPickEvals(ctx, "p", -1)
		if err != nil {
			t.Fatalf("빈 rejected · 깨진 picked_with 가 섞인 큐 판정을 못 읽었다: %v", err)
		}
		var picked []string
		for _, e := range evals {
			picked = append(picked, e.Picked)
		}
		if want := []string{"z", "", "a"}; !slices.Equal(picked, want) {
			t.Fatalf("큐 판정 = %q (%q) - 판정 행이 빠지면 분포의 판정 횟수가 줄어든다", picked, want)
		}
		if len(evals[0].Rejected) != 0 || len(evals[0].PickedWith) != 0 || len(evals[1].Rejected) != 0 ||
			len(evals[2].Rejected) != 1 || evals[2].Rejected[0].Item != "b" || evals[2].Rejected[0].Reason != "after" {
			t.Fatalf("큐 판정 칸 = %+v", evals)
		}
		// 비어 있지 않은데 깨진 사유는 오류다 - 그 행을 빼거나 0줄로 세면 분포가 조용히 짧아진다.
		pgExec(t, s, ins, "p", a.ID, fmtTime(t0), "y", nil, `[{"item":`)
		if got, err := s.ListPickEvals(ctx, "p", -1); err == nil || !strings.Contains(err.Error(), "탈락 사유 해석 실패") {
			t.Fatalf("깨진 탈락 사유를 오류 없이 넘겼다: %+v %v", got, err)
		}

		// ── 닫힌 항목 요약: 한 행의 labels · paths JSON 이 깨져도 나머지 이력이 같은 순서로 나온다.
		for i, it := range []model.Item{
			{ID: "c-old", State: model.ItemDone},
			{ID: "c-labels", State: model.ItemDropped, CloseReason: "접었다"},
			{ID: "c-paths", State: model.ItemDone},
			{ID: "c-new", State: model.ItemDone, LandedRef: "abc123"},
		} {
			closed := t0.Add(time.Duration(i) * time.Minute)
			it.Project, it.Title, it.Body, it.ClosedAt = "p", "제목 "+it.ID, "b", &closed
			if err := s.AddItem(ctx, it); err != nil {
				t.Fatalf("닫힌 항목 %s 등록 실패: %v", it.ID, err)
			}
		}
		mustItem(t, s, "p", "still-open")
		pgExec(t, s, `UPDATE item SET labels = ? WHERE project = ? AND id = ?`, `{깨짐`, "p", "c-labels")
		pgExec(t, s, `UPDATE item SET paths = ? WHERE project = ? AND id = ?`, `{깨짐`, "p", "c-paths")
		// 전제: 항목 전부를 읽는 ListClosed 는 이 행들에서 멈춘다 - 깨진 값이 실제로 해석 자리에 닿는다.
		if _, _, err := s.ListClosed(ctx, "p", "", 30); err == nil {
			t.Fatal("전제 실패 - 깨진 labels · paths 를 ListClosed 가 오류 없이 읽었다")
		}
		sums, err := s.ListClosedSummaries(ctx, "p", 30)
		if err != nil {
			t.Fatalf("깨진 labels · paths 행 하나에 랜딩 이력 전체를 잃었다: %v", err)
		}
		var ids []string
		for _, c := range sums {
			ids = append(ids, c.ID)
		}
		if want := []string{"c-new", "c-paths", "c-labels", "c-old"}; !slices.Equal(ids, want) {
			t.Fatalf("닫힌 항목 요약 = %v (%v)", ids, want)
		}
		if c := sums[2]; c.Title != "제목 c-labels" || c.State != model.ItemDropped || c.CloseReason != "접었다" ||
			c.ClosedAt == nil || !c.ClosedAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("c-labels 칸이 안 돌아왔다: %+v", c)
		}
		if sums[0].LandedRef != "abc123" || sums[3].LandedRef != "" {
			t.Fatalf("랜딩 sha 칸 = %q %q", sums[0].LandedRef, sums[3].LandedRef)
		}
		if two, err := s.ListClosedSummaries(ctx, "p", 2); err != nil || len(two) != 2 || two[0].ID != "c-new" || two[1].ID != "c-paths" {
			t.Fatalf("닫힌 항목 요약 둘 = %+v %v", two, err)
		}
		// 표가 실제로 찍는 칸(닫힌 시각)이 깨지면 여전히 오류다 - 지어내지 않는다.
		pgExec(t, s, `UPDATE item SET closed_at = ? WHERE project = ? AND id = ?`, "어제", "p", "c-old")
		if got, err := s.ListClosedSummaries(ctx, "p", 30); err == nil {
			t.Fatalf("해석 안 되는 닫힌 시각을 오류 없이 넘겼다: %+v", got)
		}
	})
}
