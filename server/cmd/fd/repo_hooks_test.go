package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 훅 관문의 **플랫폼 축**이다 — 훅 JSON 을 설계가 아니라 플랫폼 실측과 대조한다.
//
// `plugin_test.go` 의 `TestHooksJSONIsWiredAsDesigned` 는 배선을 DESIGN 과 대조한다. 이 파일은
// 그 옆에서 **플랫폼이 실제로 받는 값인가**를 문다 — 경로가 틀려도, 이벤트 이름에 오타가
// 나도, matcher 가 플랫폼이 안 쏘는 값이어도 조용히 지나가던 자리다.
//
// ★ 이 파일은 flightdeck 이 여러 플러그인을 싣던 마켓플레이스 저장소 안에 있던 시절, 옆
// 플러그인의 훅까지 보려고 태어났다. flightdeck 이 단독 저장소로 나오면서 훑기는 루트
// 플러그인 하나로 좁혀졌고(`repoPlugin` 이 그 전제를 문다), 플랫폼 축은 그대로 남았다.
//
// ★ **훅이 죽는 모양은 오류가 아니라 부재다.** `plugin_test.go` 머리말이 그 이유를 적어 뒀다:
// "훅 경로가 틀리면 세션이 그냥 아무것도 안 한다." 보드에 카드가 안 뜨는 것으로만 나타나고,
// 화면에 아무 오류도 안 뜬다.
//
// ★ **잠글 것의 근거는 어디서 오나.** 배선의 값(matcher·async)은 `plugin_test.go` 가
// DESIGN 에서 끌어와 잠근다. 이 파일이 무는 것은 근거가 **DESIGN 밖 둘 중 하나**에서 오는 축이다:
//
//	① 플랫폼 실측 — 설치본 2.1.240 바이너리를 뜯어 얻은 이벤트 이름 31종과 matcher 열거값
//	② 실측한 예산 — 입력 경로 훅의 콜드 스타트(`promptPathHookMinTimeout` 주석)
//
// 그 둘 중 어느 것도 안 대는 축(예: "timeout 은 정확히 N 초여야 한다")은 여기 없다.
// 값이 아니라 **존재**를 무는 이유가 그것이다(`TestEveryHookHasATimeout` 주석 참고).

// repoHookEvents 는 **플러그인별 훅 이벤트 집합**이다. 표와 디스크를 양방향으로 문다.
//
// ★ 표가 필요한 이유는 `repoSkillCounts` 와 같다: 훑기가 glob 뿐이면 "아무것도 못 찾았다"와
// "전부 봤다"가 화면에서 같다. 이벤트가 늘거나 훅 파일이 사라지면 이 표가 먼저 빨개져서,
// **검사받지 않은 채 들어오는 길**이 없다.
var repoHookEvents = map[string][]string{
	"flightdeck": {
		"PostToolUse", "PreCompact", "SessionEnd", "SessionStart", "Stop", "UserPromptSubmit",
	},
}

// platformHookEvents 는 설치본이 **아는 훅 이벤트 이름 전부**다(2.1.240 실측, 31종).
//
// 뽑은 자리: 번들의 zod 스키마 `hook_event_name:Ht("<이름>")` 전수.
// DESIGN §6 이 "훅 이벤트 31종에도 프로세스 종료를 알리는 것이 없다"고 적은 그 31 과 같은 수다.
//
// ★ 이름에 오타가 나면 **아무 일도 안 일어난다** — 설정은 그대로 실리고 그 훅만 영원히 안 돈다.
// 그것이 이 표가 무는 것이다. 플랫폼이 이벤트를 늘리면 여기가 낡는데, 그때 할 일은 표를
// 지우는 것이 아니라 **새 이름을 재고 적는 것**이다.
var platformHookEvents = map[string]bool{
	"ConfigChange": true, "CwdChanged": true, "DirectoryAdded": true, "Elicitation": true,
	"ElicitationResult": true, "FileChanged": true, "InstructionsLoaded": true,
	"MessageDisplay": true, "Notification": true, "PermissionDenied": true,
	"PermissionRequest": true, "PostCompact": true, "PostToolBatch": true, "PostToolUse": true,
	"PostToolUseFailure": true, "PreCompact": true, "PreToolUse": true, "SessionEnd": true,
	"SessionStart": true, "Setup": true, "Stop": true, "StopFailure": true,
	"SubagentStart": true, "SubagentStop": true, "TaskCompleted": true, "TaskCreated": true,
	"TeammateIdle": true, "UserPromptExpansion": true, "UserPromptSubmit": true,
	"WorktreeCreate": true, "WorktreeRemove": true,
}

// matcherSpec 은 matcher 가 **닫힌 열거값**인 이벤트 하나의 계약이다(2.1.240 실측).
//
// 뽑은 자리: 이벤트 메타의 `matcherMetadata:{fieldToMatch:"<필드>",values:[…]}`.
// zod 스키마 쪽 `source:Dr([…])` 와 교차로 맞췄다 — 두 자리가 같은 다섯을 낸다.
type matcherSpec struct {
	values []string
	// complete 는 이 레포의 훅이 그 열거값을 **전부** 받아야 하는가다.
	// 부분집합 검사만으로는 **빠진 값**을 못 잡는다 — 오타는 잡히는데 누락은 조용하다.
	complete bool
	// why 는 complete 가 그 값인 근거다. 실패 메시지에 실린다 — 다음 사람이
	// 이 단정을 만났을 때 고칠지 말지를 근거로 정하도록.
	why string
}

// platformMatcherValues 는 matcher 를 잠그는 이벤트들이다.
//
// ★ **여기 없는 이벤트의 matcher 는 안 문다.** `PostToolUse` 의 matcher 는 도구 **이름**이라
// 열거가 아니고(`values:[]` 로 비어 온다), `UserPromptSubmit`·`Stop` 은 matcher 자체가 없다.
// 모르는 것을 아는 척 잠그면 그 관문이 다음 사람을 틀린 데로 보낸다.
//
// ★ `SessionStart` 에 **`fork` 가 있다**. DESIGN §6 의 표는 2.1.221·2.1.222 실측 기준이고
// 그때는 이 값이 없었다 — 플랫폼이 움직인 자리다. 세션 전환 사유 여덟
// (`clear`·`resume`·`fork`·`remote_attach`·`cd`·`spare_claim`·`hydrate`·`startup_custom_id`)
// 중 훅으로 오는 것이 이 다섯이고, `/fork` 는 `/clear` 와 같은 계열의 **같은 창 안 전환**이다.
var platformMatcherValues = map[string]matcherSpec{
	"SessionStart": {
		values:   []string{"clear", "compact", "fork", "resume", "startup"},
		complete: true,
		why: "이 레포의 SessionStart 훅은 **갈래를 안 가린다** — flightdeck 은 페이로드의 " +
			"`source` 를 한 번도 안 읽는다(cmd/fd/hook.go 의 HookPayload 는 그 필드를 파싱만 한다). " +
			"그래서 빠진 값은 의도가 아니라 표류이고, 그 갈래로 시작한 세션은 카드가 없다(보드의 거짓)",
	},
	"SessionEnd": {
		values: []string{"clear", "logout", "other", "prompt_input_exit", "resume"},
		// ★ 전부가 **아니다**. flightdeck 은 `clear` 하나만 받아야 하고 그것이 DESIGN §6 의
		// 핵심 단정이다(넷은 아무도 안 쏘고, `resume` 은 `/fork` 와 사유를 공유한다).
		// 그 폭은 plugin_test.go 의 TestHooksJSONIsWiredAsDesigned 가 따로 붙들고 있다.
		complete: false,
		why:      "SessionEnd 는 좁아야 한다 — 폭을 넓히는 것을 DESIGN §6 이 금지한다",
	},
	"PreCompact": {
		values:   []string{"auto", "manual"},
		complete: false,
		why:      "PreCompact 는 matcher 없이(= 전부) 걸려 있어 완전성을 따로 안 문다",
	},
}

// hookRef 는 훑어 낸 훅 파일 하나다.
type hookRef struct {
	plugin string
	path   string
	root   string // 그 플러그인의 루트(= ${CLAUDE_PLUGIN_ROOT} 가 가리키는 자리)
}

// hookRefs 는 루트 플러그인의 `hooks/hooks.json` 을 낸다.
//
// 표본을 자르지 않는다 — 이 레포에는 `head` 파이프가 목록을 말없이 잘라 "전수"가 거짓이 된
// 선례가 있다(`94fc82e`). 하나도 못 찾으면 초록으로 지나가지 않고 그 자리에서 죽는다.
func hookRefs(t *testing.T, root string) []hookRef {
	t.Helper()
	name := repoPlugin(t, root)
	hp := filepath.Join(root, "hooks", "hooks.json")
	if _, err := os.Stat(hp); err != nil {
		t.Fatalf("hooks.json 을 못 찾았다(레포 루트 %s) — 훑기가 눈이 먼 것이지 통과가 아니다: %v", root, err)
	}
	return []hookRef{{plugin: name, path: hp, root: root}}
}

// readHooksFile 은 hooks.json 을 **모르는 키를 거절하며** 읽는다.
//
// ★ `DisallowUnknownFields` 는 일부러다. 훅 항목 스키마에는 이 구조체가 안 든 필드가 더 있다
// (2.1.240 실측: `args`·`if`·`shell`·`statusMessage`·`once`·`asyncRewake`·`rewakeMessage`·
// `rewakeSummary`). 그것들을 쓰기 시작하는 날 이 관문이 먼저 빨개지는 것이 옳다 —
// **검사받지 않은 축이 조용히 들어오는 길**을 막는 것이 이 파일의 일이기 때문이다.
// 그날 할 일은 이 단정을 지우는 것이 아니라 `hooksFile`(plugin_test.go)에 그 필드를 더하고
// 그것이 무엇을 보장해야 하는지 여기 적는 것이다.
func readHooksFile(t *testing.T, h hookRef) hooksFile {
	t.Helper()
	raw, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatalf("%s 의 hooks.json 을 못 읽었다: %v", h.plugin, err)
	}
	var hf hooksFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&hf); err != nil {
		t.Fatalf("%s 의 hooks.json 이 유효한 JSON 이 아니거나 모르는 키가 있다: %v\n"+
			"깨진 hooks.json 은 그 플러그인의 훅을 통째로 지운다 — 화면에 오류가 안 뜨는 부류다", h.plugin, err)
	}
	if len(hf.Hooks) == 0 {
		t.Fatalf("%s 의 hooks.json 에 훅이 하나도 없다 — 파일만 있고 배선이 없는 것은 통과가 아니다", h.plugin)
	}
	return hf
}

// pluginRootRefRe 는 command 에서 `${CLAUDE_PLUGIN_ROOT}` 뒤의 경로를 뽑는다.
var pluginRootRefRe = regexp.MustCompile(`\$\{CLAUDE_PLUGIN_ROOT\}(/[^"'\s]+)`)

// TestEveryHooksJSONInTheRepoIsInScope 는 표와 디스크를 양방향으로 문다.
func TestEveryHooksJSONInTheRepoIsInScope(t *testing.T) {
	root := repoRootFromCmdFd(t)

	onDisk := map[string]bool{repoPlugin(t, root): true}
	for name := range onDisk {
		if _, ok := repoHookEvents[name]; !ok {
			t.Fatalf("플러그인 %s 가 훅 표에 없다 — 표에 없는 플러그인의 훅은\n"+
				"이 파일의 어느 관문도 보지 않는다. 이름을 적기 전에 그 훅들을 먼저 읽어라", name)
		}
	}
	for name := range repoHookEvents {
		if !onDisk[name] {
			t.Fatalf("표의 %s 가 이 저장소의 플러그인이 아니다 — 죽은 이름이 표에 남으면\n"+
				"그 표를 근거로 센 것이 전부 틀린다", name)
		}
	}

	// 훅 파일이 **있는** 플러그인의 이벤트 집합이 표와 같아야 한다.
	got := map[string][]string{}
	for _, h := range hookRefs(t, root) {
		hf := readHooksFile(t, h)
		evs := keysOf(hf.Hooks)
		sort.Strings(evs)
		got[h.plugin] = evs
	}
	for name, want := range repoHookEvents {
		have, ok := got[name]
		if len(want) == 0 {
			if ok {
				t.Fatalf("표는 %s 에 훅이 없다는데 hooks.json 이 %v 를 싣고 있다 —\n"+
					"훅이 생겼으면 그것이 이 파일의 관문들을 지나는지부터 보고 표를 고쳐라", name, have)
			}
			continue
		}
		if !ok {
			t.Fatalf("표는 %s 가 훅 %v 를 갖는다는데 hooks.json 이 없다", name, want)
		}
		if strings.Join(have, ",") != strings.Join(want, ",") {
			t.Fatalf("플러그인 %s 의 훅 이벤트가 %v 인데 표는 %v 라 한다 —\n"+
				"수를 고치기 전에 늘어난 쪽이 이 파일의 관문들을 지나는지부터 봐라", name, have, want)
		}
	}

	// 이벤트 이름이 **플랫폼이 아는 것**이어야 한다. 오타는 조용히 안 걸린다.
	for _, h := range hookRefs(t, root) {
		hf := readHooksFile(t, h)
		for ev := range hf.Hooks {
			if !platformHookEvents[ev] {
				t.Fatalf("%s 가 플랫폼이 모르는 훅 이벤트 %q 를 쓴다 —\n"+
					"모르는 이름은 오류를 안 내고 그 훅만 영원히 안 돈다(2.1.240 실측 31종)", h.plugin, ev)
			}
		}
	}
}

// TestEveryHookCommandPointsAtSomethingReal 은 훅이 **실재하는 것**을 부르는지 본다.
//
// ★ 이 항목의 뿌리다. 경로가 틀리면 세션이 그냥 아무것도 안 하고, 보드에 카드가 안 뜨는
// 것으로만 나타난다 — 오류가 아니라 부재로.
func TestEveryHookCommandPointsAtSomethingReal(t *testing.T) {
	root := repoRootFromCmdFd(t)
	canLint := canLintShell(t)
	seen := 0
	for _, h := range hookRefs(t, root) {
		hf := readHooksFile(t, h)
		for ev, groups := range hf.Hooks {
			for gi, g := range groups {
				if len(g.Hooks) == 0 {
					t.Fatalf("%s 의 %s 그룹 %d 에 훅이 하나도 없다", h.plugin, ev, gi)
				}
				for hi, hk := range g.Hooks {
					where := h.plugin + "/" + ev
					// 훅 타입은 셋뿐이다(2.1.240 실측: command·prompt·mcp_tool).
					// 이 레포는 command 만 쓴다 — 다른 것을 쓰는 날 그 계약을 여기 적어라.
					if hk.Type != "command" {
						t.Fatalf("%s 의 훅 %d 의 type 이 %q 다 — 이 레포는 command 만 쓴다", where, hi, hk.Type)
					}
					// ★ 절대경로여야 한다. 훅 실행 환경이 Bash 도구와 같다는 보장이 없다(DESIGN §13).
					m := pluginRootRefRe.FindStringSubmatch(hk.Command)
					if m == nil {
						t.Fatalf("%s 의 명령이 ${CLAUDE_PLUGIN_ROOT} 절대경로를 안 쓴다: %q\n"+
							"플러그인 경로에는 **버전이 들어간다** — 갱신되면 하드코딩한 경로가 조용히 죽는다",
							where, hk.Command)
					}
					target := filepath.Join(h.root, filepath.FromSlash(strings.TrimPrefix(m[1], "/")))
					st, err := os.Stat(target)
					if err != nil {
						t.Fatalf("%s 의 명령이 가리키는 %s 가 없다: %v\n"+
							"이름이 밀리면 훅은 오류가 아니라 **부재**로 죽는다 — 아무도 못 본다", where, m[1], err)
					}
					if st.IsDir() {
						t.Fatalf("%s 의 명령이 디렉토리 %s 를 가리킨다", where, m[1])
					}
					seen++
					if canLint && (strings.HasSuffix(target, ".sh") || isShellScript(t, target)) {
						lintShell(t, target, where+" 가 부르는 "+m[1])
					}
				}
			}
		}
	}
	if seen == 0 {
		t.Fatalf("훅 명령을 하나도 안 봤다 — 훑기가 눈이 먼 것이지 통과가 아니다")
	}
}

// canLintShell 은 이 머신에서 셸 문법을 잴 수 있는지 본다.
//
// ★ 못 재면 **밝히며** 건너뛴다. 조용히 공허해지는 것이 이 레포가 두 번 밟은 자리다
// (plugin_test.go 의 로케일 프로브와 같은 규율) — 도구가 없는 머신에서 관문이 아무것도
// 안 물면서 초록인 것은 통과가 아니다.
func canLintShell(t *testing.T) bool {
	t.Helper()
	if err := exec.Command("/bin/bash", "-c", "exit 0").Run(); err != nil {
		t.Logf("이 머신에서 /bin/bash 를 못 돌린다(%v) — 셸 문법 축(bash -n)만 건너뛴다. 나머지는 그대로 잰다", err)
		return false
	}
	return true
}

// lintShell 은 셸 스크립트가 파싱되는지 본다.
//
// ★ 셸 문법 오류는 컴파일러가 안 본다 — 그 스크립트가 도는 그 순간에만 드러나고,
// 훅은 **오류가 아니라 부재**로 죽는 자리다.
func lintShell(t *testing.T, path, where string) {
	t.Helper()
	if out, err := exec.Command("/bin/bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("%s 가 bash 문법을 안 지킨다: %v\n%s", where, err, out)
	}
}

// isShellScript 는 첫 줄의 shebang 으로 셸 스크립트인지 본다(확장자가 없는 런처용).
func isShellScript(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 64)
	n, _ := f.Read(buf)
	head := string(buf[:n])
	return strings.HasPrefix(head, "#!") && (strings.Contains(head, "bash") || strings.Contains(head, "/sh"))
}

// TestEveryHookMatcherIsAValueThePlatformSends 는 matcher 가 **오는 값**인지 본다.
//
// ★ 안 오는 값을 적으면 "잡고 있다"는 착각만 생기고, 빠뜨린 값은 그 갈래에서 훅이 통째로
// 안 도는 것으로 나타난다. 둘 다 조용하다.
func TestEveryHookMatcherIsAValueThePlatformSends(t *testing.T) {
	root := repoRootFromCmdFd(t)
	checked := 0
	for _, h := range hookRefs(t, root) {
		hf := readHooksFile(t, h)
		for ev, groups := range hf.Hooks {
			spec, ok := platformMatcherValues[ev]
			if !ok {
				continue // 열거가 아닌 matcher 는 안 문다(위 표 주석 참고)
			}
			set := map[string]bool{}
			for _, v := range spec.values {
				set[v] = true
			}
			got := map[string]bool{}
			wideOpen := false
			for _, g := range groups {
				if g.Matcher == "" {
					wideOpen = true // 빈 matcher 는 "전부"다
					continue
				}
				for _, part := range strings.Split(g.Matcher, "|") {
					part = strings.TrimSpace(part)
					if !set[part] {
						t.Fatalf("%s 의 %s matcher 가 플랫폼이 안 쏘는 값 %q 를 쓴다 — 아는 값은 %v 다\n"+
							"안 오는 값은 오류를 안 낸다: 그 갈래가 조용히 영원히 안 돌 뿐이다",
							h.plugin, ev, part, spec.values)
					}
					got[part] = true
					checked++
				}
			}
			// ★★ 누락을 문다. 오타는 위에서 잡히지만 **빠진 값은 조용하다** —
			// 그 갈래에서 훅이 통째로 안 도는 것으로만 나타난다.
			if !spec.complete || wideOpen {
				continue
			}
			var missing []string
			for _, v := range spec.values {
				if !got[v] {
					missing = append(missing, v)
				}
			}
			if len(missing) > 0 {
				t.Fatalf("%s 의 %s matcher 가 %v 를 빠뜨렸다 — 플랫폼이 쏘는 값은 %v 다.\n"+
					"%s.\n"+
					"일부러 좁히는 것이면 이 표의 complete 를 끄고 그 근거를 적어라 — 지금은 근거가 반대다",
					h.plugin, ev, missing, spec.values, spec.why)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("matcher 를 하나도 안 봤다 — 훑기가 눈이 먼 것이지 통과가 아니다")
	}
}

// promptPathHookMinTimeout 은 flightdeck 의 **사람 입력 경로 훅**이 가져야 하는 최소 예산이다.
//
// ★★ **여기는 값을 문다** — 바로 아래 `TestEveryHookHasATimeout` 이 값을 안 무는 것과
// 일부러 다르다. 그 시험이 값을 피한 이유는 "근거가 안 따라와서"인데, 이 축은 근거가
// 따라온다. 2026-09-01 실측(이 머신, `fd hook user-prompt`):
//
//	단독 p50            0.56s   (12회 · 0.52~0.68)
//	동시 8개 경합       0.98s   (8회 · 0.83~0.98)
//	서버 완전 미도달    1.18s   (재시도 없이 아웃박스로 빠지는 경로)
//	**콜드 스타트       3.36s**  ← 이 상수의 유일한 이유
//
// 앞의 셋은 2초 안에 드는데 **콜드 스타트만 2초를 넘는다.** 바이너리가 페이지 캐시에서
// 밀려난 뒤의 첫 호출이 그렇고, 그 조건은 **긴 에이전트가 끝난 직후**에 정확히 성립한다 —
// 사용자가 `UserPromptSubmit hook timed out after 2s — output discarded` 를 본 자리가
// 거기다. 그때 버려지는 것은 **미확인 알림과 처방 전부**이고, 훅은 fail-open 이라 사람도
// 모델도 무엇을 못 받았는지 모른다. 확인율 지표가 그 소실만큼 아래로 편향된다.
//
// ★ **타임아웃은 상한이지 대기 시간이 아니다.** "올리면 매 프롬프트가 그만큼 멈춘다"는
// 걱정은 틀렸다 — 훅이 0.56초에 끝나면 0.56초만 쓴다. 올려서 늘어나는 것은 서버가 정말
// 죽었을 때의 최악값뿐이고, 그것도 실측 1.18초다.
//
// ★ **실측이 있는 이벤트에만 건다.** 근거가 안 따라오는 수를 관문에 들이면 그것은 관문이
// 아니라 장식이다(아래 시험의 머리말과 같은 규율).
const promptPathHookMinTimeout = 5

// promptPathHooks 는 그 하한이 걸리는 이벤트다 — **사람의 턴 경계에 있는 것**만이다.
// PostToolUse·PreCompact 는 async 라 사람을 안 막고, SessionStart 는 이미 10초다.
var promptPathHooks = map[string]bool{"UserPromptSubmit": true, "Stop": true}

// TestFlightdeckPromptPathHooksCoverColdStart 는 그 하한을 잠근다.
//
// ★ 이 시험이 없으면 누군가 2초로 되돌려도 **아무 화면도 안 붉는다** — 소실이 조용하기
// 때문에 되돌린 사실조차 관측되지 않는다. 그것이 이 항목이 처음 열린 이유다.
func TestFlightdeckPromptPathHooksCoverColdStart(t *testing.T) {
	root := repoRootFromCmdFd(t)
	seen := 0
	for _, h := range hookRefs(t, root) {
		if h.plugin != "flightdeck" {
			continue
		}
		hf := readHooksFile(t, h)
		for ev, groups := range hf.Hooks {
			if !promptPathHooks[ev] {
				continue
			}
			for _, g := range groups {
				for _, hk := range g.Hooks {
					if hk.Timeout < promptPathHookMinTimeout {
						t.Errorf("%s 의 %s 예산이 %d초다 — 콜드 스타트 실측 3.36초를 못 덮는다.\n"+
							"그 초과분은 조용히 버려진다(output discarded) — 미확인 알림과 처방이 통째로 사라지고 "+
							"사람도 모델도 무엇을 못 받았는지 모른다. 최소 %d초여야 한다",
							h.plugin, ev, hk.Timeout, promptPathHookMinTimeout)
					}
					seen++
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("입력 경로 훅을 하나도 안 봤다 — 훑기가 눈이 먼 것이지 통과가 아니다")
	}
}

// TestEveryHookHasATimeout 은 훅마다 **예산이 적혀 있는지** 본다.
//
// ★★ **값이 아니라 존재를 문다.** 훅 예산(2s·3s·10s)은 DESIGN 에서 끌어오지만, "정확히 N 초"를
// 잠글 근거까지 따라오지는 않는다 — 그래서 여기서 잠그지 않는다. 근거가 안 따라오는 수를
// 관문에 들이면 그것은 관문이 아니라 장식이다. 이 파일이 값을 무는 곳은 실측이 따로 따라오는
// 자리 하나뿐이다(`promptPathHookMinTimeout`).
//
// ★ **그런데 존재는 근거가 따라온다.** `timeout` 은 플랫폼 스키마에서 optional 이고
// (2.1.240 실측: `timeout:Xe().positive().optional()`), 안 적으면 **플랫폼 기본값**이 쓰인다.
// 그 기본값이 몇 초인지는 **이 레포가 모른다** — 2.1.240 바이너리에서 command 훅의 기본
// 예산을 못 떴다(Bun 런타임 코드에 묻혀 있다). 모르는 예산에 세션 시작을 맡기지 않는다.
// SessionStart 훅은 **사람이 첫 글자를 치기 전에** 끝나야 하고, 사용자 머신에서는 다른
// 플러그인의 훅과 그 자리에서 **함께** 돈다 — 합이 곧 세션이 안 뜨는 시간이다.
func TestEveryHookHasATimeout(t *testing.T) {
	root := repoRootFromCmdFd(t)
	seen := 0
	for _, h := range hookRefs(t, root) {
		hf := readHooksFile(t, h)
		for ev, groups := range hf.Hooks {
			for _, g := range groups {
				for _, hk := range g.Hooks {
					if hk.Timeout <= 0 {
						t.Fatalf("%s 의 %s 에 타임아웃이 없다 — 훅이 안 끊기면 세션이 멈춘다.\n"+
							"플랫폼 기본값이 몇 초인지 이 레포는 재지 못했다(2.1.240 에서 못 떴다) — "+
							"모르는 예산에 세션 시작을 맡기지 마라", h.plugin, ev)
					}
					seen++
				}
			}
		}
	}
	if seen == 0 {
		t.Fatalf("훅을 하나도 안 봤다 — 훑기가 눈이 먼 것이지 통과가 아니다")
	}
}

// platformFileWritingTools 는 플랫폼이 **파일을 편집하는 도구**로 치는 것 전부다(2.1.240 실측).
//
// 뽑은 자리 셋이 서로 맞물린다:
//
//	① 판별 함수 자체 — `S3S=["Edit","Write","NotebookEdit"]` 과 `function Usl(e){return S3S.includes(e)}`.
//	   그 옆이 권한 판정에서 `getPath` 로 편집 대상 경로를 뽑는 코드다.
//	② 지표 설명 — `claude_code.code_edit_tool.decision` 이 "for Edit, Write, and NotebookEdit tools".
//	③ 내장 도구 목록 — `BUILTIN_TOOL_NAMES` 22종. 그 중 파일을 쓰는 것은 위 셋뿐이다.
//
// ★ `MultiEdit` 은 **여기 없다.** 도구 이름 상수 정의가 0건이고 `BUILTIN_TOOL_NAMES` 에도 없다 —
// 유일한 등장이 권한 규칙 문자열을 `Edit` 로 접는 비교 하나다(레거시 호환 문자열이다).
// 넣으면 이 관문이 빨개지는 것이 옳다: 없는 도구를 잡는 척하는 matcher 는 잡는 것도 없이
// 다음 사람에게 "덮여 있다"고 말한다.
//
// ★★ **이 표는 표류하는 값이다.** 플랫폼이 파일 쓰는 도구를 늘리는 날 여기가 낡는데, 그때
// 할 일은 표를 지우는 것이 아니라 **새 이름을 재고 적는 것**이다. 이 관문이 태어난 경로가
// 정확히 그것이다 — `NotebookEdit` 이 생겼는데 matcher 가 안 따라왔고, 아무도 그것을 못 봤다.
var platformFileWritingTools = []string{"Edit", "NotebookEdit", "Write"}

// TestPostToolUseCatchesEveryFileWritingTool 은 **미커밋 발자국의 유일한 원천**에 구멍이 없는지 본다.
//
// ★★ `PostToolUse` 의 matcher 는 도구 **이름**이라 닫힌 열거가 아니다(실측에서 `values:[]` 로
// 비어 온다) — 그래서 위 `platformMatcherValues` 는 이 이벤트를 일부러 안 문다. 대신
// **저장소가 표를 든다.** 여기서 무는 것은 "플랫폼이 쏘는 값인가"가 아니라
// **"이 훅이 하는 일에 필요한 값 전부인가"** 다. 근거가 다르므로 관문도 따로 산다.
//
// ★ 양방향이다. 빠지면 그 도구로 고친 파일이 화면에서 통째로 사라지고(설계 §6: 착수 직후
// 구간은 브랜치 diff 가 정의상 비어 있어 이 훅 말고는 원천이 없다), 더하면 발자국을 안 남기는
// 도구에 훅이 돌면서 훅 호출만 늘고 "잡고 있다"는 착각이 생긴다.
func TestPostToolUseCatchesEveryFileWritingTool(t *testing.T) {
	root := repoRootFromCmdFd(t)
	want := map[string]bool{}
	for _, tool := range platformFileWritingTools {
		want[tool] = true
	}
	seen := 0
	for _, h := range hookRefs(t, root) {
		hf := readHooksFile(t, h)
		groups, ok := hf.Hooks["PostToolUse"]
		if !ok {
			continue
		}
		got := map[string]bool{}
		for _, g := range groups {
			// ★ 빈 matcher 는 "전 도구"다. 그것은 이 계약이 아니다 — Read·Grep·Bash 까지
			// 매 호출마다 훅 프로세스가 뜨는데, 그 예산은 이 훅이 **편집마다** 도는 것만으로
			// 이미 횟수 쪽에서 걸리는 자리다(hook.go 의 hookBudget 주석). 표류에 강하다는
			// 이유로 비우려면 그 예산을 먼저 재고 설계 문서에 적어라 — 지금 근거는 반대다.
			if strings.TrimSpace(g.Matcher) == "" {
				t.Fatalf("%s 의 PostToolUse matcher 가 비었다 — 그것은 **전 도구**다.\n"+
					"이 훅은 파일을 쓰는 도구 %v 만 받아야 한다: 나머지는 발자국을 안 남기는데\n"+
					"훅 프로세스만 매 호출 뜬다", h.plugin, platformFileWritingTools)
			}
			for _, part := range strings.Split(g.Matcher, "|") {
				part = strings.TrimSpace(part)
				if !want[part] {
					t.Fatalf("%s 의 PostToolUse matcher 가 %q 를 쓴다 — 파일을 쓰는 도구는 %v 다.\n"+
						"파일을 안 쓰는 도구를 받으면 경로 없는 발자국이 쌓이고, **없는 도구**를 적으면\n"+
						"(MultiEdit 이 그렇다) 잡는 것도 없이 덮여 있다고 말한다",
						h.plugin, part, platformFileWritingTools)
				}
				got[part] = true
				seen++
			}
		}
		var missing []string
		for _, tool := range platformFileWritingTools {
			if !got[tool] {
				missing = append(missing, tool)
			}
		}
		if len(missing) > 0 {
			t.Fatalf("%s 의 PostToolUse matcher 가 %v 를 빠뜨렸다 — 파일을 쓰는 도구는 %v 다.\n"+
				"빠진 도구로 고친 파일은 **발자국이 안 남는다**: 이 훅이 미커밋 발자국의 유일한\n"+
				"원천이라(설계 §6) 그 세션의 겹침 판정과 발자국이 통째로 조용해진다.\n"+
				"경로 추출(cmd/fd/hook.go 의 EditedPaths)은 이미 그 도구들의 키를 전부 본다 —\n"+
				"여기만 안 따라오면 그 코드가 **불리지 않아** 죽는다", h.plugin, missing, platformFileWritingTools)
		}
	}
	if seen == 0 {
		t.Fatalf("PostToolUse matcher 를 하나도 안 봤다 — 훑기가 눈이 먼 것이지 통과가 아니다")
	}
}
