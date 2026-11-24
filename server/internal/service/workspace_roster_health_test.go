package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commitRoster 는 루트의 명부를 고쳐 커밋하고 그 커밋의 sha 를 낸다.
func commitRoster(t *testing.T, root, yaml string) string {
	t.Helper()
	writeFile(t, root, ".flightdeck.yaml", yaml)
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-q", "-m", "명부를 고친다")
	return strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))
}

// 명부 한 줄의 결함이 **명부 전체를 얼리지 않는다.** 실배치에서 한 줄 때문에 명부 전체가
// 거절돼, 마지막으로 받아들인 명부가 몇 주 동안 남고 그 뒤에 올린 멤버들이 조용히 빠졌다.
func TestOneFaultyLineDoesNotFreezeTheRoster(t *testing.T) {
	svc, st := newSvc(t)
	root := wsRepo(t, twoMemberYAML, "member-a", "member-b", "member-c")
	openSession(t, svc, "repo", root, root, "cc-1", "")

	commitRoster(t, root, twoMemberYAML+"    - path: member-c\n    - path: /abs/elsewhere\n")
	res := openSession(t, svc, "repo", root, root, "cc-2", "")
	if got := strings.Join(res.Workspace.MemberIDs(), ","); got != "member-b,member-c,search-api" {
		t.Fatalf("멤버=%s — 새 멤버 member-c 가 들어와야 한다(결함 줄만 빠진다)", got)
	}
	if n := countRows(t, st, `SELECT count(*) FROM project_member`); n != 3 {
		t.Fatalf("캐시 %d건 — 3건이어야 한다", n)
	}
	if !strings.Contains(res.WorkspaceDetail, "뺀 줄 1건") || !strings.Contains(res.WorkspaceDetail, "/abs/elsewhere") {
		t.Errorf("사유가 뺀 줄을 안 말한다: %q", res.WorkspaceDetail)
	}
	// ★ 그리고 **다음 세션도 말한다.** sha 를 찍으면 다음 세션이 「이미 읽었다」로 접고,
	//   뺀 줄은 한 번 뜨고 영영 조용해진다 — 파싱 실패와 같은 규율이다.
	res2 := openSession(t, svc, "repo", root, root, "cc-3", "")
	if !strings.Contains(res2.WorkspaceDetail, "뺀 줄 1건") {
		t.Errorf("두 번째 세션에서 침묵했다: %q", res2.WorkspaceDetail)
	}
}

// 루트 밖 멤버(`../x`)를 받는다. 멤버 저장소를 워크스페이스 폴더 밖으로 옮긴 배치다.
func TestMemberOutsideTheRootIsAdopted(t *testing.T) {
	svc, _ := newSvc(t)
	root := wsRepo(t, "")
	sib := filepath.Join(filepath.Dir(root), "sib")
	if err := os.MkdirAll(sib, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, sib, "init", "-q", "-b", "main", ".")
	writeFile(t, sib, "README.md", "sib\n")
	runGit(t, sib, "add", "-A")
	runGit(t, sib, "commit", "-q", "-m", "init sib")
	commitRoster(t, root, "workspace:\n  members:\n    - path: ../sib\n")

	res := openSession(t, svc, "repo", root, root, "cc-out", "")
	if !res.Workspace.Knows("sib") {
		t.Fatalf("루트 밖 멤버가 안 섰다 — 사유: %q", res.WorkspaceDetail)
	}
	got, err := svc.MemberRepoPath(ctx(), res.Workspace, "sib")
	if err != nil || filepath.Clean(got) != sib {
		t.Fatalf("경로=%q err=%v — %q 여야 한다", got, err, sib)
	}
	// doctor 가 그 자리를 실제로 읽는다(서버가 읽을 수 있는 자리인지는 여기서 잰다).
	var seen bool
	for _, ax := range svc.Doctor(ctx()).Workspaces {
		if ax.Member == "sib" {
			seen = true
			if !ax.Readable {
				t.Errorf("루트 밖 멤버를 못 읽었다: %+v", ax)
			}
			if !strings.Contains(ax.Detail, "루트 밖") {
				t.Errorf("루트 밖이라는 사실을 안 말한다: %q", ax.Detail)
			}
		}
	}
	if !seen {
		t.Error("doctor 가 루트 밖 멤버를 안 쟀다")
	}
}

// 보드 배너와 doctor 가 **뺀 줄**을 말한다. 응답의 사유 칸만으로는 아무도 못 봤다.
func TestBoardAndDoctorSayWhatTheRosterDropped(t *testing.T) {
	svc, _ := newSvc(t)
	root := wsRepo(t, twoMemberYAML+"    - path: /abs/elsewhere\n", "member-a", "member-b")
	sess := openSession(t, svc, "repo", root, root, "cc-1", "")

	view, err := svc.Board(ctx(), "repo", BoardOptions{Self: sess.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if view.Roster == nil || len(view.Roster.Rejected) != 1 || view.Roster.Rejected[0].Path != "/abs/elsewhere" {
		t.Fatalf("보드가 뺀 줄을 안 싣는다: %+v", view.Roster)
	}
	if line := view.Roster.Line(); !strings.Contains(line, "뺀 줄 1건") || !strings.Contains(line, "/abs/elsewhere") {
		t.Errorf("배너 줄=%q", line)
	}
	rep := svc.Doctor(ctx())
	if len(rep.Rosters) != 1 || rep.Rosters[0].Root != "repo" || rep.Rosters[0].Detail != view.Roster.Line() {
		t.Fatalf("doctor 가 같은 줄을 안 낸다: %+v", rep.Rosters)
	}
}

// 명부를 통째로 못 읽으면 캐시는 그대로 두되 **낡았다고 말한다** — 몇 주 동안 배너가 옛
// 명부로 떠서 정상처럼 보였던 것이 이 결함의 본체다.
func TestBoardAndDoctorSayTheRosterCacheIsStale(t *testing.T) {
	svc, _ := newSvc(t)
	root := wsRepo(t, twoMemberYAML, "member-a", "member-b")
	sess := openSession(t, svc, "repo", root, root, "cc-1", "")
	cached := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))

	head := commitRoster(t, root, "workspace: {members: [{path: member-a}]}\n")
	openSession(t, svc, "repo", root, root, "cc-2", "")

	view, err := svc.Board(ctx(), "repo", BoardOptions{Self: sess.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !view.Workspace.Active() {
		t.Fatal("전제: 캐시의 옛 명부는 남아 있어야 한다")
	}
	h := view.Roster
	if h == nil || h.Failure == "" {
		t.Fatalf("보드가 낡음을 안 말한다: %+v", h)
	}
	if h.CacheSHA != cached || h.HeadSHA != head {
		t.Errorf("sha 캐시=%q 기본=%q — %q · %q 여야 한다", h.CacheSHA, h.HeadSHA, cached, head)
	}
	if line := h.Line(); !strings.Contains(line, "낡았다") || !strings.Contains(line, shortSHA(cached)) {
		t.Errorf("배너 줄=%q", line)
	}
	rep := svc.Doctor(ctx())
	if len(rep.Rosters) != 1 || rep.Rosters[0].Detail != h.Line() {
		t.Fatalf("doctor 가 같은 줄을 안 낸다: %+v", rep.Rosters)
	}
}

// 깨끗한 명부는 아무 말도 안 하고, **git 을 더 안 친다**(sha 가 같으면 파일을 안 읽는다).
func TestCleanRosterAddsNothingToBoardOrDoctor(t *testing.T) {
	svc, _ := newSvc(t)
	root := wsRepo(t, twoMemberYAML, "member-a", "member-b")
	sess := openSession(t, svc, "repo", root, root, "cc-1", "")

	view, err := svc.Board(ctx(), "repo", BoardOptions{Self: sess.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if view.Roster != nil {
		t.Fatalf("깨끗한 명부인데 실었다: %+v", view.Roster)
	}
	if rep := svc.Doctor(ctx()); len(rep.Rosters) != 0 {
		t.Fatalf("깨끗한 명부인데 doctor 가 실었다: %+v", rep.Rosters)
	}
}
