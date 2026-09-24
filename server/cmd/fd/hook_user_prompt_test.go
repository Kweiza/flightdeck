package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// UPS 훅은 **미확인 알림만** 필요하다 — 보드 전체를 부르면 안 된다.
//
// ★ 이 시험이 없어서 난 일(2026-09-24 실측, 대화 기록 전수): flightdeck UserPromptSubmit
// 첨부 433건이 hook_cancelled 였고 그중 413건이 제한 시간(5초) 초과였다. 원장과 대조하면
// 419건은 prompt 신호가 **이미 남은 뒤**였다(취소 시각보다 중앙 4.83초 앞) — 늦은 것은 신호가
// 아니라 그 뒤의 보드 조회(dashboard.json)였다. 보드는 서버가 세션 카드 파생(git worktree list ·
// 세션마다 미커밋 경로)을 통째로 돌리는 자리라 큰 저장소에서 느렸고, 훅이 죽으면 같은 턴의
// 다른 플러그인 훅까지 묶음째 취소되고 사람은 5초를 기다렸다.
// MCP 꼬리는 같은 문제로 이미 꼬리 전용 표면(/api/v1/notices — 파생을 안 돈다)으로 옮겨 가 있었다
// (mcpBackend.RecentNotes). UPS 훅만 남아 있었다.
func TestHookUserPromptReadsNoticesNotTheBoard(t *testing.T) {
	notes := `{"notes":[` +
		`{"ID":"J2","SessionID":"S2","Kind":"ask","Title":"OTHER-ASK","Body":"b","At":"2026-09-24T00:00:00Z"},` +
		`{"ID":"J1","SessionID":"S1","Kind":"blocked","Title":"OWN-BLOCKED","Body":"b","At":"2026-09-24T00:00:01Z"}]}`
	srv, hits := countingServer(t, map[string]string{
		"/api/v1/notices":        notes,
		"/api/v1/dashboard.json": `{"asks":[{"ID":"J9","SessionID":"S2","Kind":"ask","Title":"BOARD-ASK"}]}`,
	})

	out := runHookForTest(t, srv.URL, "user-prompt", `{"session_id":"cc-1","cwd":"."}`)

	if n := hits("/api/v1/dashboard.json"); n != 0 {
		t.Fatalf("UPS 훅이 보드 전체를 %d번 불렀다 — 서버가 세션 카드 파생(git)을 돌고, 큰 저장소에서 "+
			"그것이 5초 제한을 넘겨 훅이 묶음째 취소된다(2026-09-24 실측 413건)", n)
	}
	if n := hits("/api/v1/notices"); n != 1 {
		t.Fatalf("미확인을 꼬리 전용 표면(/api/v1/notices)으로 %d번 읽었다 — 1번이어야 한다", n)
	}
	if !strings.Contains(out, "OTHER-ASK") {
		t.Fatalf("다른 세션의 ask 가 안 나왔다: %q", out)
	}
	// 자기 세션이 남긴 것은 자기에게 「미확인」이 아니다 — MCP 꼬리(다른 세션이 남긴 ask·blocked)와 같은 판정.
	if strings.Contains(out, "OWN-BLOCKED") {
		t.Fatalf("자기 세션이 남긴 blocked 가 미확인으로 나왔다: %q", out)
	}
}

// 미확인이 없으면 아무것도 안 낸다 — 매 프롬프트마다 도는 자리라 빈 머리글도 컨텍스트다.
func TestHookUserPromptStaysSilentWithoutNotices(t *testing.T) {
	srv, _ := countingServer(t, map[string]string{"/api/v1/notices": `{"notes":[]}`})
	if out := runHookForTest(t, srv.URL, "user-prompt", `{"session_id":"cc-1","cwd":"."}`); strings.TrimSpace(out) != "" {
		t.Fatalf("미확인이 없는데 뭔가 냈다: %q", out)
	}
}

// 훅 안의 예산은 하네스 제한보다 짧아야 한다 — 같으면 하네스가 먼저 죽이고 묶음째 취소된다.
func TestUserPromptBudgetFitsTheHookTimeout(t *testing.T) {
	root := repoRootFromCmdFd(t)
	seen := 0
	for _, h := range hookRefs(t, root) {
		for _, g := range readHooksFile(t, h).Hooks["UserPromptSubmit"] {
			for _, hk := range g.Hooks {
				seen++
				limit := time.Duration(hk.Timeout) * time.Second
				if userPromptBudget+time.Second > limit {
					t.Errorf("user-prompt 예산 %s 이 하네스 제한 %s 에 1초 여유를 못 남긴다 — "+
						"기동·소스 훑기 몫을 빼면 하네스가 먼저 죽인다", userPromptBudget, limit)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("UserPromptSubmit 훅을 하나도 못 봤다 — 훑기가 눈이 먼 것이지 통과가 아니다")
	}
}

// 서버가 느리면 **하네스보다 먼저** 조용히 물러난다.
func TestHookUserPromptGivesUpBeforeTheHarnessDoes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"session":{"id":"S1"},"created":false}`)
	})
	mux.HandleFunc("/api/v1/notices", func(w http.ResponseWriter, r *http.Request) {
		select { // 하네스 제한(5초)보다 오래 걸리는 서버
		case <-time.After(6 * time.Second):
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `{"notes":[{"ID":"J2","SessionID":"S2","Kind":"ask","Title":"LATE"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	start := time.Now()
	out := runHookForTest(t, srv.URL, "user-prompt", `{"session_id":"cc-1","cwd":"."}`)
	if d := time.Since(start); d >= 5*time.Second {
		t.Fatalf("훅이 %s 걸렸다 — 하네스 제한(5초)에 먼저 닿으면 같은 턴의 UPS 훅이 묶음째 취소된다", d)
	}
	if strings.Contains(out, "LATE") {
		t.Fatalf("예산을 넘긴 응답이 실렸다: %q", out)
	}
}
