package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 작업자 카드 훅 둘 - 조정 세션이 작업자(서브에이전트)를 띄우고 거둘 때 보드 카드를 같이 움직인다.
//
//	worker-card  (PreToolUse, matcher Agent)  지시(prompt)에 `fd:<항목 id>` 가 있어야 띄운다.
//	             있으면 그 항목에 status:running 을 달고, 지시 끝에 「끝낼 때 카드 닫는 줄」 규약을 붙인다.
//	             카드가 필요 없는 짧은 일은 `fd:none`.
//	worker-close (SubagentStop)               작업자의 마지막 말에서 FD_FINISH … FD_END · FD_OWNER ·
//	             FD_BLOCKED 줄을 읽어 finish · 꼬리표 · 막힘 기록을 한다. 그 작업자가 지시로 받은 카드이면서
//	             이 세션의 worker-card 가 기록한 카드만(작업자의 말은 모델 출력이다). 작업자가 띄운 작업자,
//	             SendMessage 로 새로 맡긴 카드는 기록이 없어 닫지 않는다(조정 세션이 손으로 닫는다).
//
// 둘 다 **.flightdeck/ 가 있는 저장소에서만** 일한다(주 저장소 기준, 워크트리 안에서 불려도).
// fd 호출은 CLI 와 같은 함수(runLabel · runPick · runFinish · runNote)를 프로세스 안에서 부른다 -
// 같은 판정을 두 벌 두지 않는다.
//
// ★ 막는 훅이다. 다른 훅과 달리 worker-card 는 `fd:` 표식이 없거나, 카드 기록을 못 적거나, 꼬리표를 못 달면
//   (기한 workerCardBudget 안에) **거절한다**(permissionDecision deny). 그 밖의 실패(git 을 못 읽음, 페이로드가 깨짐, 런처가 fd 를
//   못 띄움 · 빌드가 하네스 타임아웃을 넘김)는 다른 훅처럼 통과시킨다 - 옮기기 전 훅도 그 자리에서 통과했다.
//
// ★ 한 번만 일한다. 같은 훅이 두 자리(플러그인 hooks.json 과, 같은 `fd hook worker-*` 를 부르는 사용자 설정)에
//   걸린 동안은 같은 호출에 둘이 동시에 돈다. 그대로 두면 꼬리표 · 결과 줄이 두 번 나고 finish 가 두 번 간다.
//   그래서 호출마다 표식 파일 하나를 O_EXCL 로 만들고, 먼저 만든 쪽만 일한다(workerOnce). 표식을 아는 것은
//   이 Go 구현뿐이라, 두 자리 모두 이 하위 명령을 불러야 한 번이 된다.

// workerSkipTypes 는 카드 없이 띄우는 읽기 전용 · 도우미 에이전트다.
var workerSkipTypes = map[string]bool{
	"Explore": true, "Plan": true, "claude-code-guide": true, "web-fetch": true,
	"statusline-setup": true, "fork": true,
}

// workerFDRef 는 지시 안의 `fd:<id>` 다. id 에 `/` 를 허용한다(묶음 · 하위 항목).
var workerFDRef = regexp.MustCompile(`fd:([A-Za-z0-9][A-Za-z0-9._/-]*)`)

// workerIDShape 는 두 훅이 받는 카드 id 의 꼴이다. 첫 글자가 영숫자라 플래그(`-x`)로 읽힐 수 없다 -
// id 는 CLI 동사의 위치 인자로 넘어가므로, 이 검사를 지나지 않은 id 는 어디에도 넘기지 않는다.
var workerIDShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// ValidWorkerCardID 는 id 가 그 꼴인지다. 순수 함수다.
func ValidWorkerCardID(id string) bool { return workerIDShape.MatchString(id) }

// WorkerCardIDs 는 지시에서 카드 id 를 뽑는다. 순수 함수다.
//
// 앞 글자가 단어 글자(유니코드 글자 · 숫자 · _)면 표식이 아니다 - `xfd:a` 는 안 읽는다.
// 끝의 `._/-` 는 문장부호로 보고 뗀다(「fd:abc.」).
func WorkerCardIDs(prompt string) []string {
	var ids []string
	for _, m := range workerFDRef.FindAllStringSubmatchIndex(prompt, -1) {
		if m[0] > 0 {
			r, _ := utf8.DecodeLastRuneInString(prompt[:m[0]])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				continue
			}
		}
		if id := strings.TrimRight(prompt[m[2]:m[3]], "._/-"); ValidWorkerCardID(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// workerCardBudget · workerCloseBudget 은 hooks.json 의 타임아웃(15초 · 120초)보다 짧은 자기 기한이다.
// 하네스가 끊으면 거절도 결과 줄도 없이 지나가므로, 그 전에 멈춰 거절하거나 실패를 적는다.
const (
	workerCardBudget  = 12 * time.Second
	workerCloseBudget = 100 * time.Second
)

// workerCardMissing 은 `fd:` 표식이 없을 때의 거절 사유다.
const workerCardMissing = "작업자 지시에 보드 카드를 적어라: prompt 에 `fd:<항목 id>` (없으면 fd add 로 먼저 만든다). " +
	"카드가 필요 없는 일이면 `fd:none`. 이 훅이 그 항목에 status:running 을 달고, 끝날 때 닫는 줄을 지시에 붙인다."

// WorkerCardFooter 는 지시 끝에 붙는 「카드 닫는 줄」 규약이다. 순수 함수다.
// readme 는 줄 규약 정본(플러그인의 board/README.ko.md)의 자리다.
func WorkerCardFooter(ids []string, readme string) string {
	first := ids[0]
	return "\n\n## 보드 카드 닫기 (훅이 붙임, 카드 " + strings.Join(ids, ", ") + ")\n" +
		"fd 항목을 pick · finish 하지 않는다. 일이 **완전히 끝난 마지막 보고**의 끝에 아래 중 하나를 그대로 쓴다(중간 보고에는 쓰지 않는다). 조정 세션의 훅이 이 줄을 읽어 카드를 닫는다.\n" +
		"- 끝남: 줄 `FD_FINISH " + first + "` (버리면 `FD_FINISH " + first + " dropped`), 다음 줄부터 finish 본문(카드 본문 규약 줄: 무엇을 했나 · `커밋: <sha>` · `파일: <절대 경로>` 등, 정본 " + readme + "), 후속이 있으면 `FD_FOLLOWUPS [{\"id\":\"…\",\"title\":\"…\",\"body\":\"…\",\"blocks\":\"…\"}]` 한 줄(카드가 되는 것은 blocks 가 " + strings.Join(FollowupBlockKinds, "|") + " 인 것뿐, 그 밖 잔손질·확인은 blocks 없이 적으면 미룬 목록 한 줄로 간다), 마지막 줄 `FD_END`.\n" +
		"- 소유자에게 고르게 돌림(선택 카드를 다 썼을 때): 줄 `FD_OWNER " + first + "`.\n" +
		"- 막힘: 줄 `FD_BLOCKED " + first + " <사유 한 줄>`.\n"
}

// workerReadme 는 줄 규약 정본의 자리다. 런처가 FD_PLUGIN_ROOT 를 넣어 준다.
func (a *App) workerReadme() string {
	for _, k := range []string{"FD_PLUGIN_ROOT", "CLAUDE_PLUGIN_ROOT"} {
		if v, ok := a.env(k); ok && strings.TrimSpace(v) != "" {
			return filepath.Join(strings.TrimSpace(v), "board", "README.ko.md")
		}
	}
	return "flightdeck 플러그인의 board/README.ko.md"
}

// flightdeckRoot 는 cwd 가 속한 **주 저장소**이고, 그 저장소가 .flightdeck/ 를 가질 때만 답한다.
func flightdeckRoot(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		cwd = "."
	}
	out, err := gitOut(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	root := MainRepoRoot(out)
	if root == "" {
		return ""
	}
	if st, err := os.Stat(filepath.Join(root, ".flightdeck")); err != nil || !st.IsDir() {
		return ""
	}
	return root
}

// workerHooksDir 는 작업자 훅의 로그 · 표식 자리다: Claude Code 사용자 설정 폴더의 hooks/.
// CLAUDE_CONFIG_DIR 가 절대경로면 그것, 아니면 HOME/.claude. 둘 다 없으면 빈 값(쓰지 않는다).
func workerHooksDir(env func(string) (string, bool)) string {
	if v, ok := env("CLAUDE_CONFIG_DIR"); ok {
		if v = strings.TrimSpace(v); filepath.IsAbs(v) {
			return filepath.Join(v, "hooks")
		}
	}
	if h := homeDir(env); h != "" {
		return filepath.Join(h, ".claude", "hooks")
	}
	return ""
}

// WorkerCloseLogName 은 닫기 결과 로그의 파일 이름이다(workerHooksDir 아래).
const WorkerCloseLogName = "fd-worker-close.log"

// 표식 이름의 kind(card2 · close2)는 규약 세대다. 카드 기록으로 닫기를 묶기 전 판(card · close)과
// 같은 이름을 쓰면, 두 판이 두 자리에 걸린 세션에서 옛 판이 먼저 표식을 쥐고 묶음 없이 닫는다.
// workerOnceTTL 이 지난 표식은 지운다. 같은 호출의 두 벌은 몇 초 안에 함께 오므로 짧게 둔다 -
// 길면 정당한 재보고(닫기가 실패해 같은 작업자에게 같은 보고를 다시 받음)가 표식에 묻힌다.
const workerOnceTTL = 10 * time.Minute

// workerOnce 는 이 호출(kind + key)을 **처음 본 쪽만** ok 를 낸다. release 는 표식을 지운다 -
// 일이 실패했으면 불러서 다음 시도가 묻히지 않게 한다.
//
// 표식을 못 만드는 사정(자리가 없음 · 권한)이면 ok 다 - 두 번 일하는 것이 한 번도 안 하는 것보다 낫다.
// key 가 비면(페이로드에 호출 id 가 없음) 가르지 않고 ok 다.
func (a *App) workerOnce(kind, key string) (ok bool, release func()) {
	release = func() {}
	name := a.workerMarker(kind, key)
	if name == "" {
		return true, release
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, release
	}
	if err != nil {
		return true, release
	}
	f.Close()
	return true, func() { _ = os.Remove(name) }
}

// workerMarker 는 그 호출의 표식 자리다(지난 표식은 이때 치운다). 자리가 없거나 key 가 비면 "" 다.
func (a *App) workerMarker(kind, key string) string {
	dir := workerHooksDir(a.env)
	if dir == "" || key == "" {
		return ""
	}
	dir = filepath.Join(dir, "fd-worker-once")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	if ents, rerr := os.ReadDir(dir); rerr == nil {
		cut := time.Now().Add(-workerOnceTTL)
		for _, e := range ents {
			if info, ierr := e.Info(); ierr == nil && info.ModTime().Before(cut) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + key))
	return filepath.Join(dir, kind+"-"+hex.EncodeToString(sum[:16]))
}

// workerNewStamp 는 새 판이 옛 이름 표식에 적는 내용이다. 옛 판(0.45.0)의 표식은 빈 파일이다.
const workerNewStamp = "fd2\n"

// workerClaim 은 이 판의 표식(kind+"2")을 잡고, 이어 옛 판(0.45.0)의 표식(kind)도 잡는다.
// 표식 이름이 판마다 다르면 판이 섞인 세션(플러그인의 옛 판 + 설정의 새 판)에서 둘이 늘 함께 돈다 -
// 옛 판은 카드 묶음을 보지 않으므로, 새 판이 옛 이름도 먼저 잡아 옛 판을 막는다.
// 옛 판이 이미 잡았으면 oldRan 이다: 그 호출은 옛 판이 맡았고 이 판은 손대지 않는다.
//
// release 는 이 판의 표식만 지운다. 옛 이름 표식은 TTL 까지 남겨, 새 판이 거절 · 실패로 끝낸 호출을
// 늦게 온 옛 판이 묶음 없이 다시 하지 못하게 한다. 새 판이 다시 잡을 때는 제 내용(workerNewStamp)을 보고 제 것으로 안다.
func (a *App) workerClaim(kind, key string) (ok, oldRan bool, release func()) {
	ok, mine := a.workerOnce(kind+"2", key)
	if !ok {
		return false, false, func() {} // 같은 호출을 다른 자리의 이 판이 이미 맡았다
	}
	name := a.workerMarker(kind, key)
	if name == "" {
		return true, false, mine
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	switch {
	case err == nil:
		_, _ = f.WriteString(workerNewStamp)
		f.Close()
	case errors.Is(err, os.ErrExist):
		if b, rerr := os.ReadFile(name); rerr != nil || string(b) != workerNewStamp {
			return false, true, func() {} // 이 판 표식은 남긴다: 다른 자리의 이 판이 같은 알림을 또 내지 않게
		}
	}
	return true, false, mine
}

// workerOldRan 은 그때 알리는 말이다.
const workerOldRan = "옛 판(0.45.0) 작업자 카드 훅이 이 호출을 먼저 맡았다(카드 묶음을 보지 않음). 판이 섞인 세션이니 조정 세션을 다시 연다"

// workerCardsTTL 이 지난 띄우기 기록은 지운다. 작업자 하나가 이보다 오래 돌 일은 없다.
const workerCardsTTL = 14 * 24 * time.Hour

// workerLaunchFile 은 이 세션(조정 세션)의 띄우기 하나(Agent 호출의 tool_use_id)에 worker-card 가
// 꼬리표를 단 카드 기록이다. 띄우기 하나에 파일 하나(0600, 카드 id 한 줄씩)라 작업자 여럿이 동시에
// 떠도 서로 덮지 않고, 한 작업자는 제 띄우기의 카드만 닫는다.
func (a *App) workerLaunchFile(session, toolUseID string) string {
	dir := workerHooksDir(a.env)
	if dir == "" || strings.TrimSpace(session) == "" || strings.TrimSpace(toolUseID) == "" {
		return ""
	}
	s := sha256.Sum256([]byte(session))
	t := sha256.Sum256([]byte(toolUseID))
	return filepath.Join(dir, "fd-worker-cards", hex.EncodeToString(s[:16]), hex.EncodeToString(t[:16]))
}

// recordWorkerLaunch 는 이 띄우기가 그 카드들에 꼬리표를 달았다고 적는다. 못 적으면 오류다 -
// 적지 못한 카드는 worker-close 가 닫기를 거절하므로, 띄우기 전에 알려야 한다.
func (a *App) recordWorkerLaunch(session, toolUseID string, ids []string) error {
	f := a.workerLaunchFile(session, toolUseID)
	if f == "" {
		return fmt.Errorf("기록 자리가 없다(세션 id · 호출 id · 설정 폴더 가운데 없는 것이 있음)")
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	// 오래된 기록은 모든 세션 폴더에서 치운다(세션은 끝나도 폴더를 치우지 않는다)
	cut := time.Now().Add(-workerCardsTTL)
	if olds, gerr := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(f)), "*", "*")); gerr == nil {
		for _, o := range olds {
			if info, ierr := os.Stat(o); ierr == nil && info.ModTime().Before(cut) {
				_ = os.Remove(o)
			}
		}
	}
	return os.WriteFile(f, []byte(strings.Join(ids, "\n")+"\n"), 0o600)
}

// workerLaunchCards 는 그 띄우기의 기록에 남은 카드다. 기록이 없거나 낡았으면 빈 집합이고,
// 있는데 못 읽으면 오류다(거절 이유를 「남의 카드」로 잘못 말하지 않게).
func (a *App) workerLaunchCards(session, toolUseID string) (map[string]bool, error) {
	got := map[string]bool{}
	f := a.workerLaunchFile(session, toolUseID)
	if f == "" {
		return got, fmt.Errorf("기록 자리가 없다(세션 id · 설정 폴더가 없음)")
	}
	info, err := os.Stat(f)
	if errors.Is(err, os.ErrNotExist) {
		return got, nil
	}
	if err != nil {
		return got, err
	}
	if info.ModTime().Before(time.Now().Add(-workerCardsTTL)) {
		return got, nil
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return got, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			got[l] = true
		}
	}
	return got, nil
}

// forgetWorkerCard 는 닫힌 카드를 그 띄우기 기록에서 뺀다. 남은 카드가 없으면 기록을 지운다.
func (a *App) forgetWorkerCard(session, toolUseID, id string) {
	f := a.workerLaunchFile(session, toolUseID)
	if f == "" {
		return
	}
	left, err := a.workerLaunchCards(session, toolUseID)
	if err != nil {
		return
	}
	delete(left, id)
	if len(left) == 0 {
		_ = os.Remove(f)
		return
	}
	rest := make([]string, 0, len(left))
	for k := range left {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	_ = os.WriteFile(f, []byte(strings.Join(rest, "\n")+"\n"), 0o600)
}

// workerMeta 는 하네스가 작업자 기록(agent-<id>.jsonl) 옆에 남기는 agent-<id>.meta.json 의 축이다.
// toolUseId 는 그 작업자를 띄운 Agent 호출(PreToolUse 의 tool_use_id)이고, parentAgentId 는
// 작업자가 띄운 작업자에게만 있다.
type workerMeta struct {
	ToolUseID     string `json:"toolUseId"`
	ParentAgentID string `json:"parentAgentId"`
}

// workerLaunchOf 는 그 작업자를 띄운 호출을 읽는다. 못 읽으면 오류다 - 그때는 아무 카드도 닫지 않고 알린다.
func workerLaunchOf(transcript string) (workerMeta, error) {
	if !strings.HasSuffix(transcript, ".jsonl") {
		return workerMeta{}, fmt.Errorf("작업자 기록 경로(agent_transcript_path)가 없거나 .jsonl 이 아니다")
	}
	path := strings.TrimSuffix(transcript, ".jsonl") + ".meta.json"
	var m workerMeta
	var err error
	// 하네스는 작업자가 끝날 때 meta.json 을 다시 쓴다: 반쯤 쓰인 파일을 만나면 잠깐 뒤 다시 읽는다
	for try := 0; try < 4; try++ {
		if try > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		var b []byte
		if b, err = os.ReadFile(path); err != nil {
			continue
		}
		m = workerMeta{}
		if err = json.Unmarshal(b, &m); err != nil {
			continue
		}
		if strings.TrimSpace(m.ToolUseID) == "" {
			return m, fmt.Errorf("meta.json 에 toolUseId 가 없다")
		}
		return m, nil
	}
	return m, err
}

// workerPayload 는 두 훅이 읽는 페이로드 축이다. tool_input 은 **원문 그대로** 받아 돌려준다 -
// 맵으로 풀었다 다시 짜면 모르는 필드의 값 모양이 바뀐다.
type workerPayload struct {
	SessionID            string                     `json:"session_id"`
	CWD                  string                     `json:"cwd"`
	ToolUseID            string                     `json:"tool_use_id"`
	ToolInput            map[string]json.RawMessage `json:"tool_input"`
	AgentID              string                     `json:"agent_id"`
	AgentType            string                     `json:"agent_type"`
	AgentTranscriptPath  string                     `json:"agent_transcript_path"`
	LastAssistantMessage string                     `json:"last_assistant_message"`
}

// fdCall 은 CLI 동사 하나를 프로세스 안에서 부르고 (종료코드, 출력)을 낸다.
// body 가 있으면 `--body -` 의 stdin 으로 준다.
func (a *App) fdCall(ctx context.Context, run func(*App, context.Context, []string, io.Writer) int, body string, args ...string) (int, string) {
	var out bytes.Buffer
	prev := a.stdin
	a.stdin = strings.NewReader(body)
	defer func() { a.stdin = prev }()
	code := run(a, ctx, args, &out)
	return code, strings.TrimSpace(out.String())
}

// runeHead 는 앞 n 글자다(바이트가 아니라 글자로 자른다).
func runeHead(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// emitJSONLine 은 훅 출력 한 줄이다. `<` `>` `&` 를 \u 로 바꾸지 않는다 - 사람이 읽는 문구다.
func emitJSONLine(out io.Writer, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return
	}
	out.Write(b.Bytes())
}

func emitPreToolUse(out io.Writer, o map[string]any) {
	o["hookEventName"] = "PreToolUse"
	emitJSONLine(out, map[string]any{"hookSpecificOutput": o})
}

// hookWorkerCard 는 PreToolUse(Agent) 다.
func (a *App) hookWorkerCard(ctx context.Context, raw []byte, out io.Writer) {
	var p workerPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		a.log.Warn("작업자 카드 훅 페이로드 해석 실패", "error", err.Error())
		return
	}
	root := flightdeckRoot(p.CWD)
	if root == "" {
		return
	}
	var subType, prompt string
	_ = json.Unmarshal(p.ToolInput["subagent_type"], &subType)
	_ = json.Unmarshal(p.ToolInput["prompt"], &prompt)
	if workerSkipTypes[strings.TrimSpace(subType)] {
		return
	}
	// 호출 id 가 없으면 가르지 않는다: 지시 본문으로 가르면 같은 지시를 나중에 다시 띄울 때 꼬리표 없이 지나간다
	once, oldRan, release := a.workerClaim("card", p.ToolUseID)
	if oldRan {
		emitJSONLine(out, map[string]string{"systemMessage": "fd 카드: " + workerOldRan})
		return
	}
	if !once {
		return // 같은 호출을 다른 자리의 같은 훅이 이미 맡았다
	}
	deny := func(why string) {
		release()
		emitPreToolUse(out, map[string]any{"permissionDecision": "deny", "permissionDecisionReason": why})
	}
	// 하네스가 훅을 끊으면 거절 없이 통과한다(비차단 오류). 그 전에 스스로 멈춰 거절한다
	ctx, cancel := context.WithTimeout(ctx, workerCardBudget)
	defer cancel()
	ids := WorkerCardIDs(prompt)
	if len(ids) == 0 {
		deny(workerCardMissing)
		return
	}
	// 작업자가 띄운 작업자(agent_id 가 있는 호출)는 꼬리표도 꼬리글도 없이 지나간다: 카드는 모델 출력이
	// 고른 것이고, worker-close 는 그 작업자의 말로 아무 카드도 닫지 않는다(parentAgentId)
	if p.AgentID != "" {
		return
	}
	var cards []string
	for _, i := range ids {
		if i != "none" {
			cards = append(cards, i)
		}
	}
	if len(cards) == 0 {
		return
	}
	a.proj = resolveProject(a.env, root)
	sess := []string{}
	if s := strings.TrimSpace(p.SessionID); s != "" {
		sess = []string{"--cc-session", s}
	}
	// worker-close 는 이 띄우기의 기록에 있는 카드만 닫는다: 작업자의 말이 남의 카드를 닫지 못하게.
	// 꼬리표보다 먼저 적는다: 기록만 남는 것은 무해하고, 꼬리표만 남으면 작업자 없는 「진행 중」이 된다.
	if err := a.recordWorkerLaunch(p.SessionID, p.ToolUseID, cards); err != nil {
		deny(fmt.Sprintf("카드 %s 를 이 띄우기의 기록에 못 적었다(%v). 적지 못한 카드는 끝날 때 닫히지 않는다", strings.Join(cards, " "), err))
		return
	}
	for _, i := range cards {
		code, msg := a.fdCall(ctx, (*App).runLabel, "", append([]string{i, "--add", "status:running"}, sess...)...)
		if code != 0 {
			deny(fmt.Sprintf("fd label %s 실패: %s (항목이 없으면 fd add 로 먼저 만든다)", i, runeHead(msg, 300)))
			return
		}
	}
	// 지시문이 닫는 줄 규약을 이미 적었으면(손으로 쓴 지시문 · 이어 띄우기) 꼬리표만 달고 꼬리글은 다시 붙이지 않는다
	if strings.Contains(prompt, "FD_FINISH") {
		return
	}
	in := map[string]json.RawMessage{}
	for k, v := range p.ToolInput {
		in[k] = v
	}
	np, err := json.Marshal(prompt + WorkerCardFooter(cards, a.workerReadme()))
	if err != nil {
		return
	}
	in["prompt"] = np
	emitPreToolUse(out, map[string]any{"permissionDecision": "allow", "updatedInput": in})
}

var (
	workerFinishRe = regexp.MustCompile(`(?ms)^FD_FINISH[ \t]+(\S+)(?:[ \t]+(done|dropped))?[ \t]*\n(.*?)^FD_END[ \t]*$`)
	workerFollowRe = regexp.MustCompile(`(?m)^FD_FOLLOWUPS[ \t]+(\[.*\])[ \t]*$`)
	workerOwnerRe  = regexp.MustCompile(`(?m)^FD_OWNER[ \t]+(\S+)[ \t]*$`)
	workerBlockRe  = regexp.MustCompile(`(?m)^FD_BLOCKED[ \t]+(\S+)[ \t]*(.*)$`)
	workerSlugRe   = regexp.MustCompile(`[^a-z0-9]+`)
)

// FollowupBlockKinds 는 후속을 따로 카드로 열어도 되는 까닭이다. 후속 객체의 "blocks" 칸이 이 중 하나여야
// 카드가 된다: 돈이 잘못 빠짐 · 남의 데이터 · 개인정보 · 보안 · 자료 손실 · 배포에 꼭 필요한 설정 · 소유자 결정.
// 그 밖(잔손질 · 확인 · 드문 경우)은 카드를 만들지 않고 finish 본문과 미룬 목록 항목(FD_FOLLOWUP_SINK)에 한 줄로 남는다.
// 작업자마다 후속 카드를 1~5개씩 열어 큐가 실제 일보다 몇 배 불어나는 것을 막는다.
var FollowupBlockKinds = []string{"money", "data", "privacy", "security", "loss", "deploy", "owner"}

func followupBlocks(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, k := range FollowupBlockKinds {
		if v == k {
			return true
		}
	}
	return false
}

// SplitWorkerFollowups 는 FD_FOLLOWUPS 의 JSON 을 카드로 만들 것과 미룰 것으로 나눈다. 순수 함수다.
//
// 카드가 되는 것은 "blocks" 가 FollowupBlockKinds 중 하나인 객체뿐이다. 서버는 {id,title,body,...} 만 읽으므로
// "blocks" 칸은 떼고 넘긴다. 문자열 후속과 blocks 가 없거나 다른 객체는 「제목: 본문」 한 줄로 미룬다.
// JSON 이 아니면 그대로 돌려준다 - finish 가 그 자리에서 거절하고 항목은 열린 채로 남는다(결과 줄에 「실패」로 보인다).
func SplitWorkerFollowups(raw string) (cards string, deferred []string) {
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return raw, nil
	}
	keep := make([]map[string]any, 0, len(arr))
	for _, f := range arr {
		var s string
		if json.Unmarshal(f, &s) == nil {
			if s = strings.TrimSpace(s); s != "" {
				deferred = append(deferred, runeHead(s, 200))
			}
			continue
		}
		var o map[string]any
		if json.Unmarshal(f, &o) != nil {
			return raw, nil
		}
		blocks, _ := o["blocks"].(string)
		delete(o, "blocks")
		if followupBlocks(blocks) {
			keep = append(keep, o)
			continue
		}
		title, _ := o["title"].(string)
		body, _ := o["body"].(string)
		if strings.TrimSpace(title) == "" {
			title, _ = o["id"].(string)
		}
		line := strings.TrimSpace(title)
		if b := strings.TrimSpace(body); b != "" && b != line {
			line += ": " + b
		}
		if line != "" {
			deferred = append(deferred, runeHead(line, 200))
		}
	}
	if len(keep) == 0 {
		return "", deferred
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(keep) != nil {
		return raw, deferred
	}
	return strings.TrimSpace(b.String()), deferred
}

// deferredFollowupBody 는 미룬 후속을 본문에 붙일 꼴이다.
func deferredFollowupBody(item string, lines []string) string {
	return "미룬 후속(카드 안 만듦, " + item + "):\n- " + strings.Join(lines, "\n- ")
}

// workerConventionWord 는 닫는 줄의 낱말이다. 말에 이것이 있는데 규약 꼴로 맞은 줄이 없으면
// (FD_END 빠짐 · 굵게 · 목록 표시 · 들여쓰기) 아무것도 안 닫았다고 알린다.
var workerConventionWord = regexp.MustCompile(`FD_(FINISH|OWNER|BLOCKED)\b`)

// workerCloseMisshapen 은 그때의 결과 줄이다.
const workerCloseMisshapen = "FD_FINISH · FD_OWNER · FD_BLOCKED 가 보이지만 규약 꼴(줄 머리, FD_FINISH 는 FD_END 줄까지)이 아니라 아무것도 닫지 않음"

// hookWorkerClose 는 SubagentStop 이다. 결과는 로그와 systemMessage 로 조정 세션에 보인다.
//
// 결과는 하나 날 때마다 로그에 바로 쓴다 - 끝에서 한 번에 쓰면 기한에 끊긴 실행이 흔적 없이 사라진다.
// 실패가 하나라도 있으면 한 번만 표식을 지워, 같은 보고를 다시 받았을 때 묻히지 않게 한다.
func (a *App) hookWorkerClose(ctx context.Context, raw []byte, out io.Writer) {
	agent := ""
	logLine := func(line string) {
		line = a.now().Local().Format("2006-01-02T15:04:05") + " agent=" + runeHead(agent, 8) + " " + strings.ReplaceAll(line, "\n", " ")
		dir := workerHooksDir(a.env)
		if dir == "" {
			a.log.Error("작업자 닫기 로그 자리가 없다", "line", clip(line, 300))
			return
		}
		_ = os.MkdirAll(dir, 0o700)
		f, err := os.OpenFile(filepath.Join(dir, WorkerCloseLogName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			a.log.Error("작업자 닫기 로그를 못 썼다", "error", err.Error(), "line", clip(line, 300))
			return
		}
		defer f.Close()
		fmt.Fprintln(f, line)
	}
	var results []string
	failed := false
	add := func(r string, ok bool) {
		results = append(results, r)
		failed = failed || !ok
		logLine(r)
	}
	release := func() {}
	// 훅이 작업자 종료를 막지 않게, 그러나 조용히 묻히지 않게: 터져도 그때까지의 결과와 함께 알린다
	defer func() {
		if r := recover(); r != nil {
			add(fmt.Sprintf("error %v", r), false)
		}
		if len(results) == 0 {
			return
		}
		if failed {
			release()
		}
		emitJSONLine(out, map[string]string{"systemMessage": "fd 카드: " + strings.Join(results, " | ")})
	}()

	var p workerPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		add("error 페이로드 해석 실패: "+err.Error(), false)
		return
	}
	agent = p.AgentID
	msg := p.LastAssistantMessage
	if !strings.Contains(msg, "FD_") {
		return
	}
	root := flightdeckRoot(p.CWD)
	if root == "" {
		return
	}
	once, oldRan, rel := a.workerClaim("close", p.AgentID+"\x00"+msg)
	if oldRan {
		add(workerOldRan, false)
		return
	}
	if !once {
		return // 같은 보고를 다른 자리의 같은 훅이 이미 맡았다
	}
	release = rel
	ctx, cancel := context.WithTimeout(ctx, workerCloseBudget)
	defer cancel()
	a.proj = resolveProject(a.env, root)
	sess := []string{}
	if s := strings.TrimSpace(p.SessionID); s != "" {
		sess = []string{"--cc-session", s}
	}
	call := func(run func(*App, context.Context, []string, io.Writer) int, body string, args ...string) (int, string) {
		return a.fdCall(ctx, run, body, append(args, sess...)...)
	}
	// unlabel 은 status:running 을 뗀다. 못 뗐으면 결과 줄에 덧붙일 말을 낸다(옛 훅은 버렸다)
	unlabel := func(item string, more ...string) (string, bool) {
		code, o := call((*App).runLabel, "", append([]string{item, "--rm", "status:running"}, more...)...)
		if code != 0 {
			return " (꼬리표 실패 " + runeHead(o, 120) + ")", false
		}
		return "", true
	}
	matched := false
	// 닫아도 되는 카드: 이 작업자를 띄운 호출(meta.json 의 toolUseId)에 이 세션의 worker-card 가 꼬리표를 단 것.
	// 작업자의 말은 모델 출력이라, 그 밖의 id 는 거절한다. 작업자가 띄운 작업자의 말로는 아무것도 닫지 않는다.
	launch, launchErr := workerLaunchOf(p.AgentTranscriptPath)
	mine := map[string]bool{}
	var mineErr error
	if launchErr == nil {
		mine, mineErr = a.workerLaunchCards(p.SessionID, launch.ToolUseID)
	}
	allowed := func(item string) bool {
		shown := clip(item, 80) // 모델이 쓴 문자열이 결과 줄 · 로그에 그대로 들어가지 않게
		switch {
		case workerSkipTypes[strings.TrimSpace(p.AgentType)]:
			add(shown+": 카드 없이 띄우는 종류("+clip(p.AgentType, 40)+")의 작업자라 거절함", false)
		case !ValidWorkerCardID(item):
			add(shown+": 카드 id 꼴이 아니라 거절함", false)
		case launchErr != nil:
			add(shown+": 이 작업자를 띄운 호출을 못 읽어 거절함("+runeHead(launchErr.Error(), 160)+")", false)
		case launch.ParentAgentID != "":
			add(shown+": 작업자가 띄운 작업자라 거절함", false)
		case mineErr != nil:
			add(shown+": 이 띄우기의 카드 기록을 못 읽어 거절함("+runeHead(mineErr.Error(), 160)+")", false)
		case !mine[item]:
			add(shown+": 이 작업자를 띄운 호출이 꼬리표를 단 카드가 아니라 거절함(남의 카드, 기록 전 판이 띄운 작업자, 이미 닫은 카드일 수 있다)", false)
		default:
			return true
		}
		return false
	}

	for _, m := range workerFinishRe.FindAllStringSubmatch(msg, -1) {
		matched = true
		item, outcome, body := m[1], m[2], m[3]
		if outcome == "" {
			outcome = "done"
		}
		follow := ""
		var deferred []string
		if fm := workerFollowRe.FindStringSubmatchIndex(body); fm != nil {
			follow, deferred = SplitWorkerFollowups(body[fm[2]:fm[3]])
			body = body[:fm[0]] + body[fm[1]:]
		}
		body = strings.TrimSpace(body)
		if body == "" {
			add(clip(item, 80)+": FD_FINISH 본문이 비어 닫지 않음", false)
			continue
		}
		if len(deferred) > 0 {
			body += "\n\n" + deferredFollowupBody(item, deferred)
		}
		if !allowed(item) {
			continue
		}
		call((*App).runPick, "", item) // 이미 쥐었으면 거절돼도 괜찮다, 닫힘 여부는 finish 결과가 정한다
		// 꼬리표는 finish 앞에서 뗀다: 서버는 끝난 항목의 꼬리표를 고치지 않는다(400)
		labelNote, _ := unlabel(item)
		args := []string{item, "--body", "-", "--outcome", outcome}
		if outcome == "dropped" {
			args = append(args, "--close-reason", "작업자 보고")
		}
		if follow != "" {
			args = append(args, "--followups", follow)
		}
		code, o := call((*App).runFinish, body, args...)
		if code == 0 {
			a.forgetWorkerCard(p.SessionID, launch.ToolUseID, item)
			add(item+": finish "+outcome+" 완료", true)
			if len(deferred) > 0 {
				add(a.sinkDeferred(call, item, deferred))
			}
		} else {
			add(item+": finish "+outcome+" 실패 "+runeHead(o, 300)+labelNote, false)
		}
	}

	for _, m := range workerOwnerRe.FindAllStringSubmatch(msg, -1) {
		matched = true
		item := m[1]
		if !allowed(item) {
			continue
		}
		if note, ok := unlabel(item, "--add", "status:owner"); ok {
			add(item+": 소유자 차례로 돌림", true)
		} else {
			add(item+": 소유자 차례로 실패"+note, false)
		}
	}

	for _, m := range workerBlockRe.FindAllStringSubmatch(msg, -1) {
		matched = true
		item, why := m[1], strings.TrimSpace(m[2])
		if why == "" {
			why = "작업자가 막힘을 보고"
		}
		if !allowed(item) {
			continue
		}
		labelNote, labelOK := unlabel(item)
		code, o := call((*App).runNote, why, "--kind", "blocked", "--item", item, "--body", "-")
		switch {
		case code != 0:
			add(item+": 막힘 기록 실패 "+runeHead(o, 200)+labelNote, false)
		case strings.Contains(o, "아웃박스에 쌓았다"):
			// 서버 미도달: 판단은 재연결 때 재생되지만 지금은 서버에 없다
			add(item+": 막힘 기록 서버 미도달, 아웃박스에 쌓음"+labelNote, false)
		default:
			add(item+": 막힘 기록 완료"+labelNote, labelOK)
		}
	}

	if !matched && workerConventionWord.MatchString(msg) {
		add(workerCloseMisshapen, false)
	}
}

// sinkDeferred 는 미룬 후속을 FD_FOLLOWUP_SINK 항목에 not-done 판단 하나로 모은다. 결과 줄과 성공 여부를 낸다.
// 자리가 없으면 finish 본문에만 남았다고 알린다(실패가 아니다).
func (a *App) sinkDeferred(call func(func(*App, context.Context, []string, io.Writer) int, string, ...string) (int, string), item string, lines []string) (string, bool) {
	n := fmt.Sprintf("후속 %d건은 카드 대신 미룬 목록으로", len(lines))
	sink, _ := a.env("FD_FOLLOWUP_SINK")
	sink = strings.TrimSpace(sink)
	if sink == "" || !ValidWorkerCardID(sink) {
		return n + "(finish 본문에만, FD_FOLLOWUP_SINK 없음)", true
	}
	code, o := call((*App).runNote, deferredFollowupBody(item, lines), "--kind", "not-done", "--item", sink, "--body", "-")
	if code != 0 {
		return n + "(" + sink + " 에 적기 실패 " + runeHead(o, 160) + ", finish 본문에는 있음)", false
	}
	return n + "(" + sink + ")", true
}
