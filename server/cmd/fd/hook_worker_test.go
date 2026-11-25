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

// closePayload 는 SubagentStop 페이로드다. launchTU 는 그 작업자를 띄운 Agent 호출의 tool_use_id 로,
// 작업자 기록 옆 meta.json 의 toolUseId 로 적힌다(하네스가 남기는 꼴 그대로).
func closePayload(t *testing.T, cwd, agentID, launchTU, msg string) string {
	t.Helper()
	return closePayloadMeta(t, cwd, agentID, map[string]any{"agentType": "general-purpose", "toolUseId": launchTU, "spawnDepth": 1}, msg)
}

// closePayloadMeta 는 meta.json 을 그대로 적는다. meta 가 nil 이면 meta.json 을 남기지 않는다.
func closePayloadMeta(t *testing.T, cwd, agentID string, meta map[string]any, msg string) string {
	t.Helper()
	dir := t.TempDir()
	tr := filepath.Join(dir, "agent-"+agentID+".jsonl")
	if err := os.WriteFile(tr, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		mb, _ := json.Marshal(meta)
		if err := os.WriteFile(filepath.Join(dir, "agent-"+agentID+".meta.json"), mb, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(map[string]any{
		"session_id": "cc-session-uuid-1", "cwd": cwd, "hook_event_name": "SubagentStop",
		"agent_id": agentID, "agent_type": "general-purpose", "agent_transcript_path": tr, "last_assistant_message": msg,
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

// launch 는 worker-card 로 작업자 하나를 띄운다(꼬리표 + 이 띄우기의 카드 기록). 거절되면 시험 실패다.
// 띄운 호출의 tool_use_id("tu-" + 지시)를 낸다 - closePayload 에 넘겨 그 작업자의 닫기를 만든다.
func (h *harness) launch(repo, prompt string) string {
	h.t.Helper()
	tu := "tu-" + prompt
	_, out := h.run(cardPayload(h.t, repo, tu, map[string]any{"prompt": prompt}), "hook", "worker-card")
	if strings.Contains(out, `"deny"`) {
		h.t.Fatalf("전제: 작업자 띄우기가 거절됐다: %s", out)
	}
	return tu
}

// recordCard 는 꼬리표 없이 띄우기 기록만 남긴다(없는 항목의 finish 실패 길을 보려고).
func (h *harness) recordCard(tu string, ids ...string) {
	h.t.Helper()
	a := newApp(envOf(h.env), quietLogger(), "", nil)
	if err := a.recordWorkerLaunch("cc-session-uuid-1", tu, ids); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) addItem(id string) {
	h.t.Helper()
	if code, out := h.run("", "add", "--id", id, "--title", "제목", "--body", "본문"); code != 0 {
		h.t.Fatalf("전제 구성 실패 - add %s 가 %d 로 끝났다:\n%s", id, code, out)
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
	h.addItem("wc-sink")
	h.env["FD_FOLLOWUP_SINK"] = "wc-sink"
	tu := h.launch(repo, "fd:wc-f 하라")
	msg := "보고\nFD_FINISH wc-f\n무엇을 했나: 고침\n커밋: abc\n" +
		`FD_FOLLOWUPS ["Next thing to do", {"id":"wc-f-polish","title":"문구 다듬기","body":"잔손질"}, {"id":"wc-f-money","title":"이중 청구 막기","body":"b","blocks":"money"}]` +
		"\nFD_END\n"
	payload := closePayload(t, repo, "agent-12345678xyz", tu, msg)
	_, out := h.run(payload, "hook", "worker-close")
	var sm struct {
		SystemMessage string `json:"systemMessage"`
	}
	if err := json.Unmarshal([]byte(out), &sm); err != nil {
		t.Fatalf("systemMessage 가 아니다: %v\n%s", err, out)
	}
	if sm.SystemMessage != "fd 카드: wc-f: finish done 완료 | 후속 2건은 카드 대신 미룬 목록으로(wc-sink)" {
		t.Fatalf("systemMessage 가 %q 다", sm.SystemMessage)
	}
	it := h.item("wc-f")
	if it.State != model.ItemDone || hasLabel(it, "status:running") {
		t.Fatalf("닫히지 않았거나 꼬리표가 남았다: %s %v", it.State, it.Labels)
	}
	// blocks 까닭이 있는 후속만 카드가 된다
	if fu := h.item("wc-f-money"); fu.Title != "이중 청구 막기" {
		t.Fatalf("blocks 후속이 카드가 안 됐다: %+v", fu)
	}
	// 나머지는 미룬 목록 항목에 not-done 판단 하나로 모인다
	found := false
	for _, j := range h.judgments(model.JudgmentNotDone) {
		for _, l := range j.Links {
			if l.TargetID == "wc-sink" && strings.Contains(j.Body, "Next thing to do") && strings.Contains(j.Body, "문구 다듬기: 잔손질") && !strings.Contains(j.Body, "이중 청구") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("미룬 후속이 미룬 목록 항목에 안 모였다")
	}
	mustContain(t, "로그", h.closeLog(), "agent=agent-12 wc-f: finish done 완료")
	// 같은 작업자의 같은 말이 둘째 자리에서 또 와도 두 번 닫지 않는다(둘째가 「실패」로 찍히지 않는다)
	if _, again := h.run(payload, "hook", "worker-close"); again != "" {
		t.Fatalf("같은 SubagentStop 에 두 번 일했다: %s", again)
	}
}

func TestWorkerCloseDeferredWithoutSinkStaysInBody(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	h.addItem("wc-n")
	tu := h.launch(repo, "fd:wc-n 하라")
	msg := "FD_FINISH wc-n\n무엇을 했나: 고침\nFD_FOLLOWUPS [\"나중에 볼 것\"]\nFD_END\n"
	_, out := h.run(closePayload(t, repo, "ag-n", tu, msg), "hook", "worker-close")
	mustContain(t, "systemMessage", out, "wc-n: finish done 완료", "후속 1건은 카드 대신 미룬 목록으로(finish 본문에만")
	found := false
	for _, j := range h.judgments(model.JudgmentHandoff) {
		if strings.Contains(j.Body, "미룬 후속(카드 안 만듦, wc-n)") && strings.Contains(j.Body, "나중에 볼 것") {
			found = true
		}
	}
	if !found {
		t.Fatalf("미룬 후속이 finish 본문에 안 남았다")
	}
}

func TestWorkerCloseOwnerAndBlocked(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	h.addItem("wc-o")
	h.addItem("wc-b")
	tu := h.launch(repo, "fd:wc-o fd:wc-b")
	_, out := h.run(closePayload(t, repo, "ag", tu, "FD_OWNER wc-o\nFD_BLOCKED wc-b 서버가 없다\n"), "hook", "worker-close")
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
	h.recordCard("tu-g", "ghost-item")
	_, out := h.run(closePayload(t, repo, "ag", "tu-g", "FD_FINISH ghost-item\n본문\nFD_END\nFD_FINISH empty-one\n\nFD_END"), "hook", "worker-close")
	mustContain(t, "systemMessage", out, "fd 카드: ghost-item: finish done 실패", "empty-one: FD_FINISH 본문이 비어 닫지 않음")
	mustContain(t, "로그", h.closeLog(), "ghost-item: finish done 실패")
	// 줄이 없는 말(중간에 멈춤 · 카드 없는 일)에는 아무것도 안 한다
	if _, quiet := h.run(closePayload(t, repo, "ag2", "tu-none", "그냥 보고"), "hook", "worker-close"); quiet != "" {
		t.Fatalf("줄 없는 말에 말했다: %s", quiet)
	}
}

func TestSplitWorkerFollowups(t *testing.T) {
	cards, deferred := SplitWorkerFollowups(`["한글 후속", {"id":"k","title":"t","body":"b"}, {"id":"m","title":"돈","body":"b2","blocks":" Money "}]`)
	if want := `[{"body":"b2","id":"m","title":"돈"}]`; cards != want {
		t.Fatalf("cards =\n%s\n기대\n%s", cards, want)
	}
	if want := []string{"한글 후속", "t: b"}; strings.Join(deferred, "|") != strings.Join(want, "|") {
		t.Fatalf("deferred = %q", deferred)
	}
	if cards, deferred := SplitWorkerFollowups(`[{"id":"x","title":"x","blocks":"nice-to-have"}]`); cards != "" || len(deferred) != 1 {
		t.Fatalf("목록 밖 blocks 는 미뤄야 한다: %q %q", cards, deferred)
	}
	if got, d := SplitWorkerFollowups("[깨짐"); got != "[깨짐" || d != nil {
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
	h.recordCard("tu-l", "wc-late")
	payload := closePayload(t, repo, "ag", "tu-l", "FD_FINISH wc-late\n본문\nFD_END\n")
	_, first := h.run(payload, "hook", "worker-close")
	mustContain(t, "첫 닫기", first, "wc-late: finish done 실패")
	// 실패한 보고는 표식을 지운다: 같은 보고를 다시 받으면 묻히지 않고 다시 해 보고 결과를 말한다
	// (같은 본문의 finish 는 서버 멱등 창 안에서 첫 응답이 재생될 수 있어, 성공까지는 단정하지 않는다)
	_, second := h.run(payload, "hook", "worker-close")
	mustContain(t, "다시 닫기", second, "wc-late: finish done 실패")
	// 낱말은 있는데 규약 꼴이 아니면 조용히 넘기지 않는다
	_, bad := h.run(closePayload(t, repo, "ag3", "tu-l", "끝.\n**FD_FINISH wc-late**\n본문\n"), "hook", "worker-close")
	mustContain(t, "꼴 아님", bad, workerCloseMisshapen)
}

func TestWorkerCloseBlockedOfflineIsNotReportedAsDone(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	h.addItem("wc-off")
	tu := h.launch(repo, "fd:wc-off")
	h.down()
	_, out := h.run(closePayload(t, repo, "ag", tu, "FD_BLOCKED wc-off 이유\n"), "hook", "worker-close")
	mustContain(t, "서버 미도달", out, "wc-off: 막힘 기록 서버 미도달, 아웃박스에 쌓음", "꼬리표 실패")
	if strings.Contains(out, "막힘 기록 완료") {
		t.Fatalf("서버 미도달인데 완료로 찍었다: %s", out)
	}
}

// TestWorkerCloseOnlyClosesItsOwnCards 는 작업자의 말(모델 출력)이 닫을 수 있는 카드를 잠근다:
// 그 작업자를 띄운 호출에 이 세션의 worker-card 가 꼬리표를 단 카드만. 나머지는 거절하고 알린다.
func TestWorkerCloseOnlyClosesItsOwnCards(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	for _, i := range []string{"mine", "other", "unlaunched"} {
		h.addItem(i)
	}
	tu := h.launch(repo, "fd:mine")
	h.launch(repo, "fd:other") // 같은 세션의 다른 작업자 카드
	msg := "FD_FINISH other\n남의 카드\nFD_END\nFD_OWNER unlaunched\nFD_BLOCKED -x 플래그\nFD_FINISH a;b\n꼴 밖\nFD_END\nFD_FINISH mine\n내 카드\nFD_END\n"
	_, out := h.run(closePayload(t, repo, "ag", tu, msg), "hook", "worker-close")
	mustContain(t, "거절", out,
		"other: 이 작업자를 띄운 호출이 꼬리표를 단 카드가 아니라 거절함(",
		"unlaunched: 이 작업자를 띄운 호출이 꼬리표를 단 카드가 아니라 거절함(",
		"-x: 카드 id 꼴이 아니라 거절함",
		"a;b: 카드 id 꼴이 아니라 거절함",
		"mine: finish done 완료")
	if it := h.item("other"); it.State == model.ItemDone || !hasLabel(it, "status:running") {
		t.Fatalf("남의 카드가 바뀌었다: %s %v", it.State, it.Labels)
	}
	if it := h.item("unlaunched"); hasLabel(it, "status:owner") {
		t.Fatalf("띄우지 않은 카드가 소유자 차례로 갔다: %v", it.Labels)
	}
	if it := h.item("mine"); it.State != model.ItemDone {
		t.Fatalf("제 카드가 안 닫혔다: %s", it.State)
	}
	mustContain(t, "로그", h.closeLog(), "other: 이 작업자를 띄운 호출이")
	// 닫은 카드는 기록에서 빠진다: 같은 작업자의 다른 말로 다시 손대지 못한다
	_, again := h.run(closePayload(t, repo, "ag", tu, "FD_OWNER mine\n"), "hook", "worker-close")
	mustContain(t, "닫은 뒤", again, "mine: 이 작업자를 띄운 호출이 꼬리표를 단 카드가 아니라 거절함(")
	// 띄운 호출(meta.json)을 못 읽으면 아무것도 닫지 않고 알린다
	_, noMeta := h.run(closePayloadMeta(t, repo, "ag9", nil, "FD_OWNER other\n"), "hook", "worker-close")
	mustContain(t, "meta 없음", noMeta, "other: 이 작업자를 띄운 호출을 못 읽어 거절함(")
	_, noTU := h.run(closePayloadMeta(t, repo, "ag8", map[string]any{"spawnDepth": 1}, "FD_OWNER other\n"), "hook", "worker-close")
	mustContain(t, "toolUseId 없음", noTU, "other: 이 작업자를 띄운 호출을 못 읽어 거절함(meta.json 에 toolUseId 가 없다)")
}

func TestValidWorkerCardID(t *testing.T) {
	for id, want := range map[string]bool{"a": true, "grp/sub-1.x": true, "-x": false, "--outcome": false, "a;b": false, "a b": false, "": false, "/a": false} {
		if got := ValidWorkerCardID(id); got != want {
			t.Errorf("ValidWorkerCardID(%q) = %v", id, got)
		}
	}
}

// TestWorkerCardBindingEdges 는 묶음이 새는 자리를 잠근다: 작업자가 띄운 작업자, 카드 없이 띄우는 종류,
// 소유자 차례 뒤 이어 받은 닫기.
func TestWorkerCardBindingEdges(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	for _, i := range []string{"nested", "victim", "helper", "resume"} {
		h.addItem(i)
	}
	// 작업자가 띄운 작업자: 꼬리표도 꼬리글도 없이 지나간다(카드 표시가 없으면 여전히 거절)
	nested := func(prompt string) string {
		b, _ := json.Marshal(map[string]any{"session_id": "cc-session-uuid-1", "cwd": repo, "tool_use_id": "tu-n-" + prompt,
			"agent_id": "parent-agent", "tool_input": map[string]any{"prompt": prompt}})
		_, out := h.run(string(b), "hook", "worker-card")
		return out
	}
	if out := nested("fd:nested"); out != "" {
		t.Fatalf("중첩 띄우기에 손댔다: %s", out)
	}
	if hasLabel(h.item("nested"), "status:running") {
		t.Fatalf("중첩 띄우기가 꼬리표를 달았다")
	}
	if out := nested("표시 없음"); !strings.Contains(out, `"deny"`) {
		t.Fatalf("표시 없는 중첩 띄우기를 거절하지 않았다: %s", out)
	}
	// 작업자가 띄운 작업자의 말은, 조정 세션이 기록해 둔 남의 카드라도 닫지 못한다
	vtu := h.launch(repo, "fd:victim")
	_, out := h.run(closePayloadMeta(t, repo, "child", map[string]any{"toolUseId": vtu, "parentAgentId": "w1", "spawnDepth": 2},
		"FD_FINISH victim\n본문\nFD_END\n"), "hook", "worker-close")
	mustContain(t, "중첩", out, "victim: 작업자가 띄운 작업자라 거절함")
	if it := h.item("victim"); it.State == model.ItemDone {
		t.Fatalf("중첩 작업자가 남의 카드를 닫았다")
	}
	// 카드 없이 띄우는 종류(Explore 등)의 말은 기록이 있어도 못 닫는다
	htu := h.launch(repo, "fd:helper")
	pl := closePayload(t, repo, "ex", htu, "FD_FINISH helper\n본문\nFD_END\n")
	var m map[string]any
	_ = json.Unmarshal([]byte(pl), &m)
	m["agent_type"] = "Explore"
	pb, _ := json.Marshal(m)
	_, out = h.run(string(pb), "hook", "worker-close")
	mustContain(t, "도우미", out, "helper: 카드 없이 띄우는 종류(Explore)의 작업자라 거절함")
	// 소유자 차례로 돌린 뒤 이어 받은 작업자(같은 띄우기)는 그 카드를 닫을 수 있다
	rtu := h.launch(repo, "fd:resume")
	_, out = h.run(closePayload(t, repo, "ag-r", rtu, "FD_OWNER resume\n"), "hook", "worker-close")
	mustContain(t, "소유자", out, "resume: 소유자 차례로 돌림")
	_, out = h.run(closePayload(t, repo, "ag-r", rtu, "FD_FINISH resume\n이어서 끝냄\nFD_END\n"), "hook", "worker-close")
	mustContain(t, "이어 닫기", out, "resume: finish done 완료")
}

// TestWorkerHooksYieldToOldVersion 은 판이 섞인 세션을 잠근다: 새 판은 옛 판(0.45.0)의 표식 이름도
// 잡아 옛 판을 막고, 옛 판이 먼저 잡은 호출에는 손대지 않고 알린다.
func TestWorkerHooksYieldToOldVersion(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	h.addItem("mix")
	a := newApp(envOf(h.env), quietLogger(), "", nil)
	// 옛 판이 먼저 맡은 띄우기
	if ok, _ := a.workerOnce("card", "tu-old"); !ok {
		t.Fatal("전제: 옛 표식을 못 만들었다")
	}
	_, out := h.run(cardPayload(t, repo, "tu-old", map[string]any{"prompt": "fd:mix"}), "hook", "worker-card")
	mustContain(t, "띄우기", out, workerOldRan)
	if strings.Contains(out, "permissionDecision") || hasLabel(h.item("mix"), "status:running") {
		t.Fatalf("옛 판이 맡은 띄우기에 손댔다: %s", out)
	}
	// 새 판이 먼저 맡으면 옛 판의 표식도 잡혀 있다(옛 판은 그 표식을 보고 비킨다)
	tu := h.launch(repo, "fd:mix")
	if ok, _ := a.workerOnce("card", tu); ok {
		t.Fatal("새 판이 옛 판의 띄우기 표식을 잡지 않았다")
	}
	msg := "FD_OWNER mix\n"
	if ok, _ := a.workerOnce("close", "ag-old\x00"+msg); !ok {
		t.Fatal("전제: 옛 닫기 표식을 못 만들었다")
	}
	_, out = h.run(closePayload(t, repo, "ag-old", tu, msg), "hook", "worker-close")
	mustContain(t, "닫기", out, workerOldRan)
	if hasLabel(h.item("mix"), "status:owner") {
		t.Fatalf("옛 판이 맡은 닫기에 손댔다")
	}
	_, out = h.run(closePayload(t, repo, "ag-new", tu, msg), "hook", "worker-close")
	mustContain(t, "새 닫기", out, "mix: 소유자 차례로 돌림")
	if ok, _ := a.workerOnce("close", "ag-new\x00"+msg); ok {
		t.Fatal("새 판이 옛 판의 닫기 표식을 잡지 않았다")
	}
	// 새 판이 거절로 끝낸 보고: 제 표식만 지워 다시 받으면 다시 하지만(옛 판으로 오인하지 않음),
	// 늦게 온 옛 판은 옛 이름 표식을 보고 비킨다(거절된 카드를 묶음 없이 닫지 못한다)
	h.addItem("foreign")
	h.launch(repo, "fd:foreign")
	bad := "FD_OWNER foreign\n"
	_, out = h.run(closePayload(t, repo, "ag-bad", tu, bad), "hook", "worker-close")
	mustContain(t, "거절", out, "foreign: 이 작업자를 띄운 호출이 꼬리표를 단 카드가 아니라 거절함(")
	if ok, _ := a.workerOnce("close", "ag-bad\x00"+bad); ok {
		t.Fatal("거절로 끝난 뒤 옛 판이 같은 보고를 잡을 수 있다")
	}
	_, out = h.run(closePayload(t, repo, "ag-bad", tu, bad), "hook", "worker-close")
	if strings.Contains(out, workerOldRan) || !strings.Contains(out, "foreign: 이 작업자를 띄운 호출이") {
		t.Fatalf("다시 받은 보고를 제 것으로 보지 않았다: %s", out)
	}
	// 기록이 있는데 못 읽으면 「남의 카드」가 아니라 못 읽었다고 말한다
	rtu := h.launch(repo, "fd:mix 다시")
	rec := newApp(envOf(h.env), quietLogger(), "", nil).workerLaunchFile("cc-session-uuid-1", rtu)
	if err := os.Chmod(rec, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(rec, 0o600)
	if _, rerr := os.ReadFile(rec); rerr == nil {
		t.Skip("이 기계에서는 권한을 빼도 읽힌다(root)")
	}
	_, out = h.run(closePayload(t, repo, "ag-perm", rtu, "FD_OWNER mix\n"), "hook", "worker-close")
	mustContain(t, "못 읽음", out, "mix: 이 띄우기의 카드 기록을 못 읽어 거절함(")
}

func TestWorkerCardDeniesWhenRecordFails(t *testing.T) {
	h := newHarness(t)
	repo := fdRepo(t, true)
	h.addItem("rec")
	hooks := filepath.Join(h.home, ".claude", "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "fd-worker-cards"), []byte("막음"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out := h.run(cardPayload(t, repo, "tu-r", map[string]any{"prompt": "fd:rec"}), "hook", "worker-card")
	o := parsePreTool(t, out)
	if o.HookSpecificOutput.PermissionDecision != "deny" || !strings.Contains(o.HookSpecificOutput.PermissionDecisionReason, "카드 rec 를 이 띄우기의 기록에 못 적었다") {
		t.Fatalf("기록을 못 적었는데 띄웠다: %+v", o.HookSpecificOutput)
	}
	if hasLabel(h.item("rec"), "status:running") {
		t.Fatalf("띄우지 않았는데 status:running 이 남았다")
	}
	// 호출 id 가 없으면 띄우기에 묶을 수 없어 거절한다
	if err := os.Remove(filepath.Join(hooks, "fd-worker-cards")); err != nil {
		t.Fatal(err)
	}
	_, out = h.run(cardPayload(t, repo, "", map[string]any{"prompt": "fd:rec"}), "hook", "worker-card")
	if o := parsePreTool(t, out); o.HookSpecificOutput.PermissionDecision != "deny" || !strings.Contains(o.HookSpecificOutput.PermissionDecisionReason, "호출 id") {
		t.Fatalf("호출 id 없이 띄웠다: %+v", o.HookSpecificOutput)
	}
}
