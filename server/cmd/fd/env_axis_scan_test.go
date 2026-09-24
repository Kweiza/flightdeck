package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// envKeyShapeRe 는 **환경 키 모양**의 문자열이다 — 대문자 낱말을 밑줄로 이은 것.
//
// ★ 접두(FD_·CLAUDE_·CODEX_…)로 좁히지 않는다. 좁히면 새 접두의 키(예: 다른 도구가 넘기는
// 변수)가 이 관문을 조용히 지나간다. 넓게 잡고, 환경 읽기가 아닌 것은 notEnvAxes 에
// **사유와 함께** 올린다 — 검사받지 않은 채 들어오는 길을 막는 것이 이 관문의 일이다.
var envKeyShapeRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)+$`)

// bareEnvKeys 는 밑줄이 없는 환경 키 중 이 코드가 읽을 법한 것들이다. 모양만으로는 못 가른다.
var bareEnvKeys = map[string]bool{"HOME": true, "PATH": true, "USER": true, "TMPDIR": true, "SHELL": true, "PWD": true, "TERM": true}

// notEnvAxes 는 환경 키 모양이지만 **이 코드가 환경에서 읽지 않는** 문자열이다. 사유가 값이다.
var notEnvAxes = map[string]string{
	"GIT_DIR":          "gitreader 가 자식 git 의 환경에서 **걷어 내는** 키다(strippedGitEnv) — 읽지 않는다",
	"GIT_WORK_TREE":    "gitreader 가 자식 git 의 환경에서 **걷어 내는** 키다(strippedGitEnv) — 읽지 않는다",
	"GIT_INDEX_FILE":   "gitreader 가 자식 git 의 환경에서 **걷어 내는** 키다(strippedGitEnv) — 읽지 않는다",
	"GIT_AUTHOR_IDENT": "`git var` 의 인자다(identity_gate.go) — git 이 계산한 신원을 묻는 것이지 환경 변수가 아니다",
	"FD_EXPECT_IDENT":  "저장소 커밋 훅(.githooks/_identity.sh) **본문에서 찾는 글자**다(identity_gate.go) — 환경에서 읽지 않는다",
}

// 비시험 코드가 읽는 환경 키는 **전부** knownEnvAxes 에 있어야 한다. 그리고 그 반대도.
//
// ★ 이 시험이 없어서 난 일(2026-09-24 실측): knownEnvAxes 의 주석은 「코드가 새 환경 키를
// 읽기 시작하면 이 시험이 빨개진다」고 약속했는데, 그 자리의 시험(TestEnvAxisInventoryIsCurrent)은
// 하네스가 고정한 키 ⊆ 목록만 봤다. 그래서 FD_PLUGIN_ROOT 를 목록에서 빼도 초록이었고, 코드가
// 읽는 CODEX_SESSION_ID·CODEX_THREAD_ID·CODEX_SANDBOX·CODEX_SANDBOX_NETWORK_DISABLED·FD_LEDGER·
// PATH·USER 는 목록에 아예 없었다 — codex 하네스 축이 통째로 들어오는 동안 아무도 안 물었다.
//
// 훑는 것은 **문자열 리터럴**이다. 읽는 모양이 여럿이라(a.env("…") · get("…") · envOr(env, "…") ·
// 상수 EnvSessionID = "…" 를 거친 get(EnvSessionID) · doctor 의 관측 표) 호출 모양으로 가르면
// 한 모양을 빠뜨리는 순간 조용해진다. 리터럴은 상수 선언과 표 안에서도 한 번은 나타난다.
func TestEveryEnvKeyTheCodeReadsIsInventoried(t *testing.T) {
	serverDir := filepath.Join(pluginRoot(t), "server")
	found := map[string][]string{} // 키 → 나타난 자리
	files := 0
	err := filepath.WalkDir(serverDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("%s 를 못 읽었다: %v", p, perr)
		}
		files++
		rel, _ := filepath.Rel(serverDir, p)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !(envKeyShapeRe.MatchString(s) || bareEnvKeys[s]) {
				return true
			}
			found[s] = append(found[s], rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("server/ 를 못 훑었다: %v", err)
	}
	// ★ 훑기가 눈이 멀면 초록이다 — 하한을 건다. FD_STATE_DIR 는 상태 디렉토리의 첫 가지라 반드시 있다.
	if files < 50 || len(found["FD_STATE_DIR"]) == 0 {
		t.Fatalf("훑기가 눈이 멀었다 — 파일 %d개, FD_STATE_DIR 출현 %d건. 좌표(%s)부터 봐라",
			files, len(found["FD_STATE_DIR"]), serverDir)
	}

	known := map[string]bool{}
	for _, k := range knownEnvAxes {
		known[k] = true
	}
	var keys []string
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if known[k] {
			continue
		}
		if why, ok := notEnvAxes[k]; ok && strings.TrimSpace(why) != "" {
			continue
		}
		t.Errorf("코드가 %q 를 쓰는데(%s) knownEnvAxes 에 없다.\n"+
			"환경에서 읽는 키면 목록에 더하고 「하네스가 이 축도 고정하는가 / 고정하면 무엇이 안 보이게 되는가」를\n"+
			"한 번 물어라. 환경 읽기가 아니면 notEnvAxes 에 **사유와 함께** 올려라",
			k, strings.Join(found[k], ", "))
	}
	// 반대 방향: 목록에 있는데 코드에 없으면 목록이 낡았다.
	for _, k := range knownEnvAxes {
		if _, ok := found[k]; !ok {
			t.Errorf("knownEnvAxes 의 %q 를 비시험 코드 어디서도 안 쓴다 — 목록이 낡았다", k)
		}
	}
	// 예외 표도 낡으면 안 된다 — 코드에서 사라진 예외는 걷어라.
	for k := range notEnvAxes {
		if _, ok := found[k]; !ok {
			t.Errorf("notEnvAxes 의 %q 가 코드에 없다 — 사라진 예외는 걷어라", k)
		}
		if slices.Contains(knownEnvAxes, k) {
			t.Errorf("%q 가 knownEnvAxes 와 notEnvAxes 에 함께 있다 — 읽는지 안 읽는지 하나로 정해라", k)
		}
	}
}
