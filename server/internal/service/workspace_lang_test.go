package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/gitreader"
	"github.com/kweiza/flightdeck/internal/lang"
	"github.com/kweiza/flightdeck/internal/model"
)

// rosterFile 은 명부 파일 읽기의 답만 정한다(raw 또는 err). 다른 git 은 안 부른다.
type rosterFile struct {
	GitReader
	raw []byte
	err error
}

func (f rosterFile) FileAt(context.Context, string, string) ([]byte, error) { return f.raw, f.err }

// 명부 건강의 줄(RosterHealth.Line)이 FD_LANG=en 에서 **한글 0자**다 — 뺀 줄의 사유 전부와
// 꼴을 못 읽은 사유 전부를 실제 판정(readRoster)으로 만들어 잰다. 표(internal/lang/catalog.go)는
// 줄마다 전부 아니면 원문이라, 사유 하나만 빠져도 그 줄 전체가 한국어로 돌아온다.
func TestRosterHealthLineIsEnglish(t *testing.T) {
	svc, _ := newSvc(t)
	proj := model.Project{ID: "cp-root", Path: "/r", DefaultBranch: "main"}
	read := func(raw string, err error) rosterRead {
		return svc.readRoster(ctx(), proj, rosterFile{raw: []byte(raw), err: err})
	}

	// 뺀 줄 — 파서가 빼는 값 전부와 채택이 빼는 id 축 둘.
	bad := read("workspace:\n  members:\n"+
		"    - path: /abs/x\n"+
		"    - path: .\n"+
		"    - path: ../\n"+
		"    - path: ../..\n"+
		"    - path: ../../far\n"+
		"    - project: only-a-name\n"+
		"    - path: \"...\"\n"+
		"    - path: cp-root\n"+
		"    - path: good\n"+
		"    - path: other/good\n", nil)
	if len(bad.rejected) != 9 {
		t.Fatalf("전제: 뺀 줄 9건이어야 한다 — %d건: %+v", len(bad.rejected), bad.rejected)
	}
	lines := []string{RosterHealth{Rejected: bad.rejected}.Line()}

	// 꼴을 못 읽은 사유 — 파서가 파일을 통째로 거절하는 꼴 전부와 크기 상한.
	for _, src := range []string{
		"workspace: x\n",
		"workspace:\n  members: x\n",
		"workspace:\n  members:\n    stray\n",
		"workspace:\n  members:\n    - nokey\n",
		"workspace:\n  members:\n    - path:\ta\n",
		"workspace:\n  members:\n    - path: [a]\n",
	} {
		rr := read(src, nil)
		if rr.malformed == "" {
			t.Fatalf("전제: %q 는 꼴 실패여야 한다: %+v", src, rr)
		}
		lines = append(lines, RosterHealth{CacheSHA: "aaaaaaaaaaaa", HeadSHA: "bbbbbbbbbbbb", Failure: rr.malformed}.Line())
	}
	big := read("", fmt.Errorf("main:.flightdeck.yaml: %w", gitreader.ErrFileTooLarge))
	if big.malformed == "" {
		t.Fatalf("전제: 크기 상한은 꼴 실패여야 한다: %+v", big)
	}
	lines = append(lines, RosterHealth{CacheSHA: "", HeadSHA: "bbbbbbbbbbbb", Failure: big.malformed,
		Rejected: bad.rejected[:1]}.Line())

	for _, ln := range lines {
		if got := lang.English.Text(ln); lang.HasHangul(got) {
			t.Errorf("한글이 남았다 — 표에 넣어라:\n  ko: %s\n  en: %s", ln, got)
		}
	}
}

// doctor 의 멤버 축(workspaceAxes)이 루트 밖 멤버를 말하는 줄도 영어로 옮겨진다 — 읽히는 자리와
// 못 읽는 자리 둘 다.
func TestOutsideRootMemberAxisIsEnglish(t *testing.T) {
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
	commitRoster(t, root, "workspace:\n  members:\n    - path: ../sib\n    - path: ../gone\n")
	openSession(t, svc, "repo", root, root, "cc-out", "")

	seen := map[string]bool{}
	for _, ax := range svc.Doctor(ctx()).Workspaces {
		if !strings.Contains(ax.Detail, "루트 밖") {
			continue
		}
		seen[ax.Member] = true
		if got := lang.English.Text(ax.Detail); lang.HasHangul(got) {
			t.Errorf("%s: 한글이 남았다 — 표에 넣어라:\n  ko: %s\n  en: %s", ax.Member, ax.Detail, got)
		}
	}
	if !seen["sib"] || !seen["gone"] {
		t.Fatalf("전제: 루트 밖 멤버 둘(읽힘 · 못 읽음)을 재야 한다 — %v", seen)
	}
}
