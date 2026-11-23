package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
)

// 작업자 카드 훅 둘(hook_worker.go)의 동작을 실물 서버 위에서 잠근다.
// 단정의 좌표계는 **서버가 실제로 갖게 된 항목 · 꼬리표 · 판단**이다.

// fdRepo 는 .flightdeck/ 를 가진 git 저장소 하나를 만든다(훅이 일하는 조건).
func fdRepo(t *testing.T, withFlightdeck bool) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init 실패: %v %s", err, out)
	}
	if withFlightdeck {
		if err := os.MkdirAll(filepath.Join(dir, ".flightdeck"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func cardPayload(t *testing.T, cwd, toolUseID string, input map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id": "cc-session-uuid-1", "cwd": cwd, "hook_event_name": "PreToolUse",
		"tool_name": "Agent", "tool_use_id": toolUseID, "tool_input": input,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func closePayload(t *testing.T, cwd, agentID, msg string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id": "cc-session-uuid-1", "cwd": cwd, "hook_event_name": "SubagentStop",
		"agent_id": agentID, "last_assistant_message": msg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type preToolOut struct {
	HookSpecificOutput struct {
		HookEventName            string         `json:"hookEventName"`
		PermissionDecision       string         `json:"permissionDecision"`
		PermissionDecisionReason string         `json:"permissionDecisionReason"`
		UpdatedInput             map[string]any `json:"updatedInput"`
	} `json:"hookSpecificOutput"`
}

func parsePreTool(t *testing.T, out string) preToolOut {
	t.Helper()
	var o preToolOut
	if err := json.Unmarshal([]byte(out), &o); err != nil {
		t.Fatalf("훅 출력이 JSON 이 아니다: %v\n%s", err, out)
	}
	if o.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("hookEventName 이 %q 다", o.HookSpecificOutput.HookEventName)
	}
	return o
}

func (h *harness) addItem(id string) {
	h.t.Helper()
	if code, out := h.run("", "add", "--id", id, "--title", "제목", "--body", "본문"); code != 0 {
		h.t.Fatalf("전제 구성 실패 — add %s 가 %d 로 끝났다:\n%s", id, code, out)
	}
}

func (h *harness) item(id string) model.Item {
	h.t.Helper()
	it, err := h.st.GetItem(context.Background(), h.project, id)
	if err != nil {
		h.t.Fatalf("항목 %s 조회 실패: %v", id, err)
	}
	return it
}

func hasLabel(it model.Item, l string) bool {
	for _, x := range it.Labels {
		if x == l {
			return true
		}
	}
	return false
}

func (h *harness) closeLog() string {
	b, _ := os.ReadFile(filepath.Join(h.home, ".claude", "hooks", WorkerCloseLogName))
	return string(b)
}

func TestWorkerCardIDs(t *testing.T) {
	cases := map[string][]string{
		"fd:abc 하라":          {"abc"},
		"(fd:grp/sub-1).":    {"grp/sub-1"},
		"fd:a, fd:none":      {"a", "none"},
		"xfd:a":              nil,
		"카드fd:a":             nil, // 앞이 글자면 표식이 아니다
		"fd:-a":              nil,
		"끝 fd:abc._/-":       {"abc"},
		"줄\nfd:x2 그리고 fd:y3": {"x2", "y3"},
	}
	for in, want := range cases {
		got := WorkerCardIDs(in)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("WorkerCardIDs(%q) = %v, %v 여야 한다", in, got, want)
		}
	}
}

func TestWorkerCardDeniesWithoutRef(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	code, out := h.run(cardPayload(t, repo, "tu-1", map[string]any{"prompt": "카드 없는 지시", "subagent_type": "general-purpose"}), "hook", "worker-card")
	if code != 0 {
		t.Fatalf("훅 종료코드가 %d 다", code)
	}
	o := parsePreTool(t, out)
	if o.HookSpecificOutput.PermissionDecision != "deny" || o.HookSpecificOutput.PermissionDecisionReason != workerCardMissing {
		t.Fatalf("표식 없는 지시를 거절하지 않았다: %+v", o.HookSpecificOutput)
	}
}

func TestWorkerCardSkipsOutsideFlightdeckAndHelpers(t *testing.T) {
	h := newHarness(t)
	plain := fdRepo(t, false)
	if _, out := h.run(cardPayload(t, plain, "tu-1", map[string]any{"prompt": "x"}), "hook", "worker-card"); out != "" {
		t.Fatalf(".flightdeck 없는 저장소에서 말했다: %s", out)
	}
	repo := fdRepo(t, true)
	if _, out := h.run(cardPayload(t, repo, "tu-2", map[string]any{"prompt": "x", "subagent_type": "Explore"}), "hook", "worker-card"); out != "" {
		t.Fatalf("Explore 를 막았다: %s", out)
	}
	if _, out := h.run(cardPayload(t, repo, "tu-3", map[string]any{"prompt": "짧은 일 fd:none"}), "hook", "worker-card"); out != "" {
		t.Fatalf("fd:none 에 말했다: %s", out)
	}
}

func TestWorkerCardLabelsAndAppendsFooterOnce(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	h.addItem("wc-a")
	h.env["FD_PLUGIN_ROOT"] = "/plug"
	in := map[string]any{"prompt": "일 fd:wc-a 하라", "subagent_type": "general-purpose",
		"description": "d", "run_in_background": true}
	payload := cardPayload(t, repo, "tu-9", in)
	_, out := h.run(payload, "hook", "worker-card")
	o := parsePreTool(t, out)
	if o.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("허락하지 않았다: %+v", o.HookSpecificOutput)
	}
	got := o.HookSpecificOutput.UpdatedInput
	want := "일 fd:wc-a 하라" + WorkerCardFooter([]string{"wc-a"}, "/plug/board/README.ko.md")
	if got["prompt"] != want {
		t.Fatalf("꼬리글이 다르다:\n%v\n기대:\n%s", got["prompt"], want)
	}
	if got["description"] != "d" || got["run_in_background"] != true || got["subagent_type"] != "general-purpose" {
		t.Fatalf("다른 입력 필드가 바뀌었다: %v", got)
	}
	for _, s := range []string{"FD_FINISH wc-a", "FD_OWNER wc-a", "FD_BLOCKED wc-a", "/plug/board/README.ko.md", "FD_END"} {
		mustContain(t, "꼬리글", want, s)
	}
	if !hasLabel(h.item("wc-a"), "status:running") {
		t.Fatalf("status:running 이 안 달렸다: %v", h.item("wc-a").Labels)
	}
	// 같은 호출이 둘째 자리(사용자 설정)에서 또 와도 꼬리글을 두 번 붙이지 않는다
	if _, again := h.run(payload, "hook", "worker-card"); again != "" {
		t.Fatalf("같은 tool_use_id 에 두 번 일했다: %s", again)
	}
}

func TestWorkerCardSlashIDAndFinishAlreadyWritten(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	h.addItem("grp/sub")
	_, out := h.run(cardPayload(t, repo, "tu-1", map[string]any{"prompt": "fd:grp/sub\n끝에 FD_FINISH grp/sub 를 쓴다"}), "hook", "worker-card")
	if out != "" {
		t.Fatalf("FD_FINISH 를 이미 적은 지시에 꼬리글을 붙였다: %s", out)
	}
	if !hasLabel(h.item("grp/sub"), "status:running") {
		t.Fatalf("/ 가 든 id 에 꼬리표가 안 달렸다: %v", h.item("grp/sub").Labels)
	}
}

func TestWorkerCardDeniesUnknownItem(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	_, out := h.run(cardPayload(t, repo, "tu-1", map[string]any{"prompt": "fd:no-such-item"}), "hook", "worker-card")
	o := parsePreTool(t, out)
	if o.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("없는 항목인데 띄웠다: %+v", o.HookSpecificOutput)
	}
	mustContain(t, "거절 사유", o.HookSpecificOutput.PermissionDecisionReason, "fd label no-such-item 실패:", "fd add 로 먼저 만든다")
}

func TestWorkerCloseFinishesWithStringFollowups(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	h.addItem("wc-f")
	if code, out := h.run("", "label", "wc-f", "--add", "status:running"); code != 0 {
		t.Fatalf("전제: %s", out)
	}
	msg := "보고\nFD_FINISH wc-f\n무엇을 했나: 고침\n커밋: abc\nFD_FOLLOWUPS [\"Next thing to do\"]\nFD_END\n"
	payload := closePayload(t, repo, "agent-12345678xyz", msg)
	_, out := h.run(payload, "hook", "worker-close")
	var sm struct {
		SystemMessage string `json:"systemMessage"`
	}
	if err := json.Unmarshal([]byte(out), &sm); err != nil {
		t.Fatalf("systemMessage 가 아니다: %v\n%s", err, out)
	}
	if sm.SystemMessage != "fd 카드: wc-f: finish done 완료" {
		t.Fatalf("systemMessage 가 %q 다", sm.SystemMessage)
	}
	it := h.item("wc-f")
	if it.State != model.ItemDone || hasLabel(it, "status:running") {
		t.Fatalf("닫히지 않았거나 꼬리표가 남았다: %s %v", it.State, it.Labels)
	}
	fu := h.item("wc-f-fu1-next-thing-to-do")
	if fu.Title != "Next thing to do" {
		t.Fatalf("문자열 후속이 객체로 안 바뀌었다: %+v", fu)
	}
	mustContain(t, "로그", h.closeLog(), "agent=agent-12 wc-f: finish done 완료")
	// 같은 작업자의 같은 말이 둘째 자리에서 또 와도 두 번 닫지 않는다(둘째가 「실패」로 찍히지 않는다)
	if _, again := h.run(payload, "hook", "worker-close"); again != "" {
		t.Fatalf("같은 SubagentStop 에 두 번 일했다: %s", again)
	}
}

func TestWorkerCloseOwnerAndBlocked(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	h.addItem("wc-o")
	h.addItem("wc-b")
	for _, i := range []string{"wc-o", "wc-b"} {
		h.run("", "label", i, "--add", "status:running")
	}
	_, out := h.run(closePayload(t, repo, "ag", "FD_OWNER wc-o\nFD_BLOCKED wc-b 서버가 없다\n"), "hook", "worker-close")
	mustContain(t, "systemMessage", out, "wc-o: 소유자 차례로 돌림", "wc-b: 막힘 기록 완료")
	if o := h.item("wc-o"); !hasLabel(o, "status:owner") || hasLabel(o, "status:running") {
		t.Fatalf("소유자 차례로 안 갔다: %v", o.Labels)
	}
	if b := h.item("wc-b"); hasLabel(b, "status:running") {
		t.Fatalf("막힌 항목에 status:running 이 남았다: %v", b.Labels)
	}
	found := false
	for _, j := range h.judgments(model.JudgmentBlocked) {
		for _, l := range j.Links {
			if l.TargetID == "wc-b" && strings.Contains(j.Body, "서버가 없다") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("막힘 판단이 그 항목에 안 남았다")
	}
}

func TestWorkerCloseFailureIsLoud(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	_, out := h.run(closePayload(t, repo, "ag", "FD_FINISH ghost-item\n본문\nFD_END\nFD_FINISH empty-one\n\nFD_END"), "hook", "worker-close")
	mustContain(t, "systemMessage", out, "fd 카드: ghost-item: finish done 실패", "empty-one: FD_FINISH 본문이 비어 닫지 않음")
	mustContain(t, "로그", h.closeLog(), "ghost-item: finish done 실패")
	// 줄이 없는 말(중간에 멈춤 · 카드 없는 일)에는 아무것도 안 한다
	if _, quiet := h.run(closePayload(t, repo, "ag2", "그냥 보고"), "hook", "worker-close"); quiet != "" {
		t.Fatalf("줄 없는 말에 말했다: %s", quiet)
	}
}

func TestWorkerFollowups(t *testing.T) {
	got := WorkerFollowups("grp/x", `["한글 후속", {"id":"k","title":"t","body":"b"}]`)
	want := `[{"body":"한글 후속","id":"x-fu1-f1","title":"한글 후속"},{"id":"k","title":"t","body":"b"}]`
	if got != want {
		t.Fatalf("WorkerFollowups =\n%s\n기대\n%s", got, want)
	}
	if got := WorkerFollowups("a", "[깨짐"); got != "[깨짐" {
		t.Fatalf("깨진 JSON 은 그대로 넘겨야 한다(finish 가 거절해 항목이 열린 채 남는다): %s", got)
	}
}

// TestWorkerHooksAreWiredAsDesigned 는 hooks.json 의 작업자 훅 두 그룹을 명령 문자열째로 잠근다.
func TestWorkerHooksAreWiredAsDesigned(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pluginRoot(t), "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hf hooksFile
	if err := json.Unmarshal(raw, &hf); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		matcher, cmd string
		timeout      int
	}{
		"PreToolUse":   {"Agent", `"${CLAUDE_PLUGIN_ROOT}/bin/fd" hook worker-card --harness claude`, 15},
		"SubagentStop": {"", `"${CLAUDE_PLUGIN_ROOT}/bin/fd" hook worker-close --harness claude`, 120},
	}
	for ev, w := range want {
		g := hf.Hooks[ev]
		if len(g) != 1 || g[0].Matcher != w.matcher || len(g[0].Hooks) != 1 {
			t.Fatalf("%s 그룹 꼴이 다르다: %+v", ev, g)
		}
		hk := g[0].Hooks[0]
		if hk.Command != w.cmd || hk.Async || hk.Timeout != w.timeout || hk.StatusMessage == "" {
			t.Fatalf("%s 훅이 다르다: %+v (기대 %q, 동기, %d초, 상태 줄 있음)", ev, hk, w.cmd, w.timeout)
		}
	}
}

func TestWorkerCloseRetriesAfterFailureAndFlagsMisshapenLines(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	payload := closePayload(t, repo, "ag", "FD_FINISH wc-late\n본문\nFD_END\n")
	_, first := h.run(payload, "hook", "worker-close")
	mustContain(t, "첫 닫기", first, "wc-late: finish done 실패")
	// 실패한 보고는 표식을 지운다: 같은 보고를 다시 받으면 묻히지 않고 다시 해 보고 결과를 말한다
	// (같은 본문의 finish 는 서버 멱등 창 안에서 첫 응답이 재생될 수 있어, 성공까지는 단정하지 않는다)
	_, second := h.run(payload, "hook", "worker-close")
	mustContain(t, "다시 닫기", second, "wc-late: finish done 실패")
	// 낱말은 있는데 규약 꼴이 아니면 조용히 넘기지 않는다
	_, bad := h.run(closePayload(t, repo, "ag3", "끝.\n**FD_FINISH wc-late**\n본문\n"), "hook", "worker-close")
	mustContain(t, "꼴 아님", bad, workerCloseMisshapen)
}

func TestWorkerCloseBlockedOfflineIsNotReportedAsDone(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	h.addItem("wc-off")
	h.down()
	_, out := h.run(closePayload(t, repo, "ag", "FD_BLOCKED wc-off 이유\n"), "hook", "worker-close")
	mustContain(t, "서버 미도달", out, "wc-off: 막힘 기록 서버 미도달, 아웃박스에 쌓음", "꼬리표 실패")
	if strings.Contains(out, "막힘 기록 완료") {
		t.Fatalf("서버 미도달인데 완료로 찍었다: %s", out)
	}
}
