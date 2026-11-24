package mcpsrv

import (
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/judge"
	"github.com/kweiza/flightdeck/internal/lang"
	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/service"
)

// 워크스페이스 배너가 **명부의 건강**을 함께 말한다(service.RosterHealth). 명부를 통째로 못
// 읽으면 배너가 옛 캐시로 떠서 정상처럼 보였고, 첫 명부부터 못 읽었으면 배너 자체가 없었다.
func TestWorkspaceBannerCarriesRosterHealth(t *testing.T) {
	now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	health := &service.RosterHealth{
		CacheSHA: "aaaaaaaaaaaaaaaa", HeadSHA: "bbbbbbbbbbbbbbbb", Failure: "꼴을 못 읽었다",
		Rejected: []judge.RejectedMember{{Path: "/abs/x", Reason: "절대경로다"}},
	}
	active := judge.Roster{Root: "root", Members: map[string]string{"a": "a"}}
	cases := []struct {
		name   string
		roster judge.Roster
	}{
		{"옛 캐시로 선 배너", active},
		{"명부가 하나도 안 선 루트", judge.Roster{}},
	}
	for _, c := range cases {
		v := service.BoardView{At: now, Window: time.Hour, Project: model.Project{ID: "root"},
			Workspace: c.roster, Roster: health}
		got := RenderBoard(v, BoardRenderOptions{Now: now})
		if !strings.Contains(got, health.Line()) {
			t.Errorf("%s: 배너가 명부 건강을 안 싣는다:\n%s", c.name, got)
		}
	}
	// 깨끗하면 아무것도 안 붙는다 — 워크스페이스가 아니면 배너 자체가 없다.
	v := service.BoardView{At: now, Window: time.Hour, Project: model.Project{ID: "solo"}}
	if got := RenderBoard(v, BoardRenderOptions{Now: now}); strings.Contains(got, "명부") {
		t.Errorf("깨끗한데 명부 줄이 붙었다:\n%s", got)
	}
}

// 배너의 명부 줄(머리 「워크스페이스 명부:」 · 옛 캐시 아래의 「  ! 」)이 FD_LANG=en 에서 한글 0자다.
// 사유는 판정이 실제로 내는 문장이다(judge.ParseWorkspace 가 뺀 줄).
func TestWorkspaceBannerRosterLineIsEnglish(t *testing.T) {
	now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	ws, err := judge.ParseWorkspace("workspace:\n  members:\n    - path: /abs/x\n    - path: ../../far\n")
	if err != nil || len(ws.Rejected) != 2 {
		t.Fatalf("전제: 뺀 줄 2건이어야 한다 — %+v %v", ws.Rejected, err)
	}
	health := &service.RosterHealth{Rejected: ws.Rejected}
	for _, roster := range []judge.Roster{{Root: "root", Members: map[string]string{"a": "a"}}, {}} {
		v := service.BoardView{At: now, Window: time.Hour, Project: model.Project{ID: "root"},
			Workspace: roster, Roster: health}
		var hit int
		for _, ln := range strings.Split(RenderBoard(v, BoardRenderOptions{Now: now}), "\n") {
			if !strings.Contains(ln, health.Line()) {
				continue
			}
			hit++
			if got := lang.English.Text(ln); lang.HasHangul(got) {
				t.Errorf("배너의 명부 줄에 한글이 남았다:\n  ko: %s\n  en: %s", ln, got)
			}
		}
		if hit != 1 {
			t.Errorf("명부 줄이 %d번 나왔다 — 한 번이어야 한다", hit)
		}
	}
}
