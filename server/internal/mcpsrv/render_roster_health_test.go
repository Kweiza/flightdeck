package mcpsrv

import (
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/judge"
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
