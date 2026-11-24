package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
)

// 세션이 받는 곳: 보드 초인종의 배달 주소(board/README 「세션에 알림」).
//
// SessionStart · UserPromptSubmit 훅이 세션을 연 김에 그 세션의 cross-session messaging 소켓과 권한 모드를
// 원장에 적는다(PUT /api/v1/sessions/{id}/inbox). 보드는 그 프로젝트 · 그 기계의 것을 읽어 보낸다.
// 예전에는 보드 폴더에 따로 적고(board/session_inbox.py hook) 폴더 접두로 프로젝트를 다시 판정했다.
// 세션의 프로젝트는 이 원장이 git-common-dir 로 이미 정해 두었으므로 그 둘째 장부 · 둘째 판정을 없앴다.
//
// ★ CLAUDE_CODE_MESSAGING_TOKEN 은 읽지도 보내지도 않는다. 그 토큰은 「이 세션의 자식 프로세스」를 증명하는
//   값이고 macOS · Linux 에서는 연결에 없어도 되며, 자식이 아닌 보드가 내밀어도 쓸모가 없다(실측 2.1.330).
//   보내지 않으면 원장 · 로그 · 응답 어디에도 샐 것이 없다.
// ★ 사람이 연 세션만 적는다. -p · SDK 작업자는 보드 알림을 받을 사람이 없다(attended).
// ★ Claude Code 하네스로 선언된 훅만 적는다. 소켓 환경 변수는 그 Claude Code 프로세스의 것인데 자식 프로세스에
//   그대로 물려간다: 조정 세션이 띄운 codex 의 훅이 부모 소켓(과 ATTENDED=1)을 보고 자기 카드의 받는 곳으로 적자,
//   보드가 부모 소켓에 codex 세션 id 를 실어 보내 부모는 버렸고(id 불일치), 같은 소켓의 맞는 등록까지 밀려났다.
//   미상(빈 값)도 적지 않는다: 하네스는 환경으로 못 가른다(app.go 의 harness).
// ★ 보드를 깔지 않은 기계(보드 설정 폴더가 없음)에서는 아무것도 안 보낸다: 그 기계의 받는 곳은 읽을 보드가 없다.
// ★ 실패는 훅을 막지 않는다(hook.go 머리말). 옛 서버(018 이전)는 이 경로를 몰라 404 · 405 를 내고,
//   그때는 다음 프롬프트가 다시 적으므로 서버가 올라간 뒤 저절로 채워진다.

// EnvMessagingSocket 은 Claude Code 가 훅 환경에 주는 그 세션의 inbox 소켓이다.
const EnvMessagingSocket = "CLAUDE_CODE_MESSAGING_SOCKET"

// boardConfigDir 는 보드 설정 폴더다(홈 아래). board/README 「설정」의 BOARD_AUTH · BOARD_WATCH 가 사는 자리.
const boardConfigDir = ".config/flightdeck-board"

// sessionAttended 는 사람이 연 세션인가다. -p · SDK 작업자는 CLAUDE_CODE_SESSION_ATTENDED=0, 터미널 세션은 1
// (없으면 entrypoint 가 cli 인지로 본다).
func sessionAttended(get func(string) (string, bool)) bool {
	if v, ok := get("CLAUDE_CODE_SESSION_ATTENDED"); ok {
		return strings.TrimSpace(v) == "1"
	}
	v, _ := get("CLAUDE_CODE_ENTRYPOINT")
	return strings.TrimSpace(v) == "cli"
}

// modeFromArgs 는 claude 명령줄에서 권한 모드다. 못 정하면 "". 순수 함수다.
func modeFromArgs(args string) string {
	words := strings.Fields(args)
	for _, w := range words { // 권한 확인을 건너뛰는 플래그는 자리와 상관없이 이긴다(claude 가 그렇게 푼다)
		if w == "--dangerously-skip-permissions" {
			return "bypassPermissions"
		}
	}
	for i, w := range words {
		switch {
		case w == "--permission-mode" && i+1 < len(words):
			return words[i+1]
		case strings.HasPrefix(w, "--permission-mode="):
			return strings.TrimPrefix(w, "--permission-mode=")
		}
	}
	return ""
}

// modeFromSettings 는 설정 파일의 permissions.defaultMode 다, 이기는 순서(local > project > user).
// 없으면 "default". read 는 파일 읽기(시험이 바꾼다).
func modeFromSettings(read func(string) ([]byte, error), projectDir, home string) string {
	var paths []string
	if projectDir != "" {
		paths = append(paths, filepath.Join(projectDir, ".claude", "settings.local.json"),
			filepath.Join(projectDir, ".claude", "settings.json"))
	}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".claude", "settings.json"))
	}
	for _, p := range paths {
		raw, err := read(p)
		if err != nil {
			continue
		}
		var cfg struct {
			Permissions struct {
				DefaultMode string `json:"defaultMode"`
			} `json:"permissions"`
		}
		if json.Unmarshal(raw, &cfg) == nil && cfg.Permissions.DefaultMode != "" {
			return cfg.Permissions.DefaultMode
		}
	}
	return "default"
}

// inboxMode 는 받는 세션의 권한 모드다. UserPromptSubmit 페이로드는 permission_mode 를 싣고(shift+tab 으로 바꾼
// 것까지), SessionStart 페이로드는 안 싣는다: 새로 뜬 세션(startup)이면 명령줄 다음 설정 파일로 짐작하고 첫 프롬프트가
// 바로잡는다. 이어지는 세션(resume · clear · compact · fork)이면 ""(모름)을 보내 프롬프트가 고쳐 둔 값을 짐작으로 덮지 않는다.
func (a *App) inboxMode(ctx context.Context, p HookPayload) string {
	if m := strings.TrimSpace(p.PermissionMode); m != "" {
		return m
	}
	if p.HookEventName == "SessionStart" && p.Source != "" && p.Source != "startup" {
		return ""
	}
	if pid, _ := a.env("CLAUDE_PID"); strings.TrimSpace(pid) != "" {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second) // 훅 기한 안에서
		out, err := exec.CommandContext(ctx, "ps", "-o", "args=", "-p", strings.TrimSpace(pid)).Output()
		cancel()
		if err == nil {
			if m := modeFromArgs(string(out)); m != "" {
				return m
			}
		}
	}
	project, _ := a.env("CLAUDE_PROJECT_DIR")
	if strings.TrimSpace(project) == "" {
		project = p.CWD
	}
	return modeFromSettings(os.ReadFile, strings.TrimSpace(project), homeDir(a.env))
}

// inboxSkip 은 이 세션의 받는 곳을 적지 않을 까닭이다(적을 것이면 ""). 소켓도 함께 낸다.
func (a *App) inboxSkip() (socket, why string) {
	home := homeDir(a.env)
	if home == "" {
		return "", "no_home"
	}
	if st, err := os.Stat(filepath.Join(home, boardConfigDir)); err != nil || !st.IsDir() {
		return "", "no_board"
	}
	if a.harness != model.HarnessClaude {
		return "", "not_claude"
	}
	if !sessionAttended(a.env) {
		return "", "unattended"
	}
	sock, _ := a.env(EnvMessagingSocket)
	if sock = strings.TrimSpace(sock); sock == "" {
		return "", "no_socket"
	}
	return sock, ""
}

type inboxReq struct {
	Socket string `json:"socket"`
	Mode   string `json:"mode"`
}

// tellInbox 는 그 카드의 받는 곳을 원장에 적는다. 실패는 로그로만 남긴다(훅을 막지 않는다).
//
// ★ 아웃박스에 싣지 않는다(a.cli.Write 가 아니라 do). 서버가 닿지 않을 때 줄 세워 뒀다가 나중에 재생하면
// 그 사이 끝난 세션의 소켓이 되살아난다. 다음 프롬프트가 지금 값을 다시 적는 것이 맞다.
func (a *App) tellInbox(ctx context.Context, sessionID string, p HookPayload) {
	sock, why := a.inboxSkip()
	if why != "" {
		a.log.Debug("받는 곳을 적지 않는다", "why", why)
		return
	}
	if sessionID == "" {
		return
	}
	_, _, err := a.cli.do(ctx, http.MethodPut, "/api/v1/sessions/"+urlPath(sessionID)+"/inbox",
		inboxReq{Socket: sock, Mode: a.inboxMode(ctx, p)}, FreshKey(a.cli.Session))
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusMethodNotAllowed) {
			// 옛 서버(018 이전)이거나 그 카드가 사라졌다. 프롬프트마다 경고를 쌓지 않는다.
			a.log.Debug("받는 곳 적기: 서버가 이 경로를 모른다", "status", ae.Status)
		} else {
			a.log.Warn("받는 곳 적기 실패", "error", err.Error())
		}
	}
}
