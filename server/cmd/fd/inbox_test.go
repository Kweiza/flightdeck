package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
)

// 받는 곳(보드 초인종의 배달 주소)을 훅이 원장에 적는다: board/README 「세션에 알림」.

const inboxTestToken = "tok-must-never-leave-the-session"

// inboxEnv 는 사람이 연 세션 · 보드를 깐 기계의 훅 환경이다.
func inboxEnv(h *harness, extra map[string]string) map[string]string {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Join(h.home, boardConfigDir), 0o700); err != nil {
		h.t.Fatal(err)
	}
	e := map[string]string{}
	for k, v := range h.env {
		e[k] = v
	}
	e["CLAUDE_CODE_SESSION_ATTENDED"] = "1"
	e[EnvMessagingSocket] = "/tmp/fd-inbox-test/claude-1.sock"
	e["CLAUDE_CODE_MESSAGING_TOKEN"] = inboxTestToken
	e["CLAUDE_PROJECT_DIR"] = filepath.Join(h.home, "proj")
	for k, v := range extra {
		if v == "" {
			delete(e, k)
		} else {
			e[k] = v
		}
	}
	return e
}

func (h *harness) inboxRows() []model.SessionInbox {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.state, "machine-id"))
	if err != nil {
		h.t.Fatalf("machine-id 를 못 읽었다: %v", err)
	}
	rows, err := h.st.ListInbox(context.Background(), h.project, strings.TrimSpace(string(raw)))
	if err != nil {
		h.t.Fatalf("받는 곳 조회 실패: %v", err)
	}
	return rows
}

// 세션 시작이 소켓과 짐작한 모드를 적고, 프롬프트가 페이로드의 모드로 고친다. 토큰은 원장 어디에도 없다.
func TestHooksPutTheInboxIntoTheLedger(t *testing.T) {
	h := newHarness(t)
	env := inboxEnv(h, nil)
	startup := `{"session_id":"cc-session-uuid-1","cwd":"/tmp","hook_event_name":"SessionStart","source":"startup"}`
	if code, out := h.runEnv(env, startup, "hook", "session-start", "--harness", "claude"); code != 0 {
		t.Fatalf("session-start 종료코드 %d: %s", code, out)
	}
	rows := h.inboxRows()
	if len(rows) != 1 || rows[0].Socket != "/tmp/fd-inbox-test/claude-1.sock" || rows[0].Mode != "default" ||
		rows[0].CCSessionID != "cc-session-uuid-1" {
		t.Fatalf("세션 시작 뒤 받는 곳 = %+v, 소켓 · default · cc-session-uuid-1 하나여야 한다", rows)
	}

	prompt := `{"session_id":"cc-session-uuid-1","cwd":"/tmp","hook_event_name":"UserPromptSubmit","prompt":"x","permission_mode":"bypassPermissions"}`
	if code, out := h.runEnv(env, prompt, "hook", "user-prompt", "--harness", "claude"); code != 0 {
		t.Fatalf("user-prompt 종료코드 %d: %s", code, out)
	}
	if rows := h.inboxRows(); len(rows) != 1 || rows[0].Mode != "bypassPermissions" {
		t.Fatalf("프롬프트 뒤 받는 곳 = %+v, 모드가 bypassPermissions 로 고쳐져야 한다", rows)
	}
	// 이어지는 SessionStart(clear · compact …)는 프롬프트가 고친 모드를 설정 파일 짐작(default)으로 덮지 않는다.
	if code, out := h.runEnv(env, sessionStartPayload("cc-session-uuid-1", "/tmp"), "hook", "session-start", "--harness", "claude"); code != 0 {
		t.Fatalf("session-start(clear) 종료코드 %d: %s", code, out)
	}
	if rows := h.inboxRows(); len(rows) != 1 || rows[0].Mode != "bypassPermissions" {
		t.Fatalf("clear 뒤 받는 곳 = %+v, 모드가 bypassPermissions 로 남아야 한다", rows)
	}

	// 토큰은 읽지도 보내지도 않는다: 원장 파일(본문 · WAL) 어디에도 그 글자가 없어야 한다.
	for _, p := range []string{h.db, h.db + "-wal"} {
		raw, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), inboxTestToken) {
			t.Fatalf("%s 에 메시징 토큰이 들어갔다", filepath.Base(p))
		}
	}
	keys := map[string]any{}
	raw, _ := json.Marshal(inboxReq{})
	_ = json.Unmarshal(raw, &keys)
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "mode,socket" {
		t.Fatalf("받는 곳 요청의 칸이 %v 다: socket · mode 둘뿐이어야 한다(토큰 칸 금지)", names)
	}
}

// 사람이 안 연 세션(-p · SDK 작업자), 보드를 안 깐 기계, 소켓이 없는 세션, Claude Code 가 아닌 하네스는 적지 않는다.
// codex · 미상은 부모 Claude Code 의 소켓 · ATTENDED 를 물려받은 환경 그대로 돈다(inbox.go 머리말).
func TestHooksSkipTheInboxWhenNobodyCanBeTold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   map[string]string
		board   bool
		harness string
	}{
		{"작업자", map[string]string{"CLAUDE_CODE_SESSION_ATTENDED": "0"}, true, "claude"},
		{"소켓 없음", map[string]string{EnvMessagingSocket: ""}, true, "claude"},
		{"보드 없음", nil, false, "claude"},
		{"codex 가 물려받은 소켓", nil, true, "codex"},
		{"하네스 미상", nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			env := inboxEnv(h, tc.extra)
			if !tc.board {
				if err := os.RemoveAll(filepath.Join(h.home, boardConfigDir)); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"hook", "session-start"}
			if tc.harness != "" {
				args = append(args, "--harness", tc.harness)
			}
			if code, out := h.runEnv(env, sessionStartPayload("cc-session-uuid-1", "/tmp"), args...); code != 0 {
				t.Fatalf("종료코드 %d: %s", code, out)
			}
			if rows := h.inboxRows(); len(rows) != 0 {
				t.Fatalf("받는 곳이 적혔다: %+v", rows)
			}
		})
	}
}

func TestModeFromArgsAndSettings(t *testing.T) {
	for args, want := range map[string]string{
		"claude --dangerously-skip-permissions":                           "bypassPermissions",
		"claude --permission-mode plan":                                   "plan",
		"claude --permission-mode=acceptEdits -c":                         "acceptEdits",
		"claude --resume abc":                                             "",
		"claude --permission-mode default --dangerously-skip-permissions": "bypassPermissions",
		"claude --permission-mode":                                        "",
	} {
		if got := modeFromArgs(args); got != want {
			t.Errorf("modeFromArgs(%q) = %q, 기대 %q", args, got, want)
		}
	}
	files := map[string]string{
		"/p/.claude/settings.json": `{"permissions":{"defaultMode":"acceptEdits"}}`,
		"/h/.claude/settings.json": `{"permissions":{"defaultMode":"bypassPermissions"}}`,
	}
	read := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	}
	if got := modeFromSettings(read, "/p", "/h"); got != "acceptEdits" {
		t.Errorf("프로젝트 설정이 이겨야 한다: %q", got)
	}
	if got := modeFromSettings(read, "/q", "/h"); got != "bypassPermissions" {
		t.Errorf("사용자 설정으로 떨어져야 한다: %q", got)
	}
	if got := modeFromSettings(read, "/q", "/nohome"); got != "default" {
		t.Errorf("설정이 없으면 default: %q", got)
	}
}
