package judge

import (
	"fmt"
	"path"
	"strings"

	"github.com/kweiza/flightdeck/internal/model"
)

// 워크스페이스 — 루트 레포 하나가 하위 git 레포 여럿을 «멤버»로 갖는 배치.
//
// ★ 이 낱말은 이 파일 전까지 **다른 뜻**이었다. judge/prescribe.go 의 `WorkspaceClaims`
// 는 «세션의 워크트리»를 가리키고, 그 이름은 그대로 둔다 — 고치면 처방 문면과 REST
// 필드 이름이 함께 바뀌어 옛 응답을 읽는 쪽이 조용히 깨진다. 두 뜻이 한 패키지에 있는
// 것을 감추지 않고 여기 적는다: **이 파일의 Workspace 는 레포 묶음이고, prescribe 의
// WorkspaceClaims 는 트리 하나다.**
//
// 정본은 루트 레포에 커밋된 `.flightdeck.yaml` 의 `workspace:` 블록이고, 서버 DB 는
// 캐시다(설계 §8 의 "대상 ref 의 파일에서 읽는다"가 그대로 적용된다 — 로컬 사본을 믿지
// 않는다). 관계는 **한 단계**다: 멤버가 다시 워크스페이스를 갖는 것은 안 읽는다.

// WorkspaceMember 는 멤버 레포 하나의 선언이다 — 정의는 model 에 있다.
//
// ★ 별칭인 이유: 이 값은 judge(파싱) → store(캐시) → service(절대경로 해석) 세 계층을
// 그대로 지나가는데, 계층마다 제 타입을 두면 변환이 세 벌 생기고 그 변환은 필드를
// 하나 빠뜨려도 컴파일을 통과한다. 이름을 여기 남기는 것은 이 패키지의 함수 시그니처가
// 읽히게 하기 위해서다(정의를 옮긴 사실은 이 주석이 못박는다).
//
// Path 는 **루트 상대** 경로다. 절대경로는 memberPathOK 가 뺀다 — 정본이 커밋된
// 파일이라 그 값은 이 머신 밖에서도 읽히고, 절대경로는 남의 머신에서 뜻이 없다.
// 루트 밖(`../x`)은 받는다(memberPathOK 의 사유).
type WorkspaceMember = model.WorkspaceMember

// RejectedMember 는 명부에서 **뺀 줄** 하나와 그 사유다.
//
// ★ 왜 줄 단위인가. 한 줄의 결함으로 명부 전체를 거절하면 캐시가 마지막으로 받아들인
// 명부에 얼어붙는다 — 그 뒤에 올린 멤버들이 아무 표시 없이 빠지고, 배너는 옛 명부로
// 계속 떠서 정상처럼 보인다(실배치에서 몇 주를 그렇게 갔다). 그래서 못 받는 줄만 빼고,
// 뺀 사실은 이 값으로 화면까지 나른다. **조용히 버리지 않는다는 규율은 그대로다** —
// 거절하는 단위가 파일에서 줄로 좁아졌을 뿐이다.
//
// Path·Project 는 파일에 적힌 그대로다(비어 있을 수 있다 — 그것이 사유일 때).
type RejectedMember struct {
	Path    string `json:"path,omitempty"`
	Project string `json:"project,omitempty"`
	Reason  string `json:"reason"`
}

// Workspace 는 루트 레포가 선언한 멤버 명부다.
//
// ★ **빈 명부와 «블록이 없다»를 가른다.** Declared 가 false 면 파일에 `workspace:` 가
// 아예 없었다는 뜻이고, 그때는 **아무것도 바뀌지 않아야 한다**(fd-ws-registration 의
// 수용 조건 — 단일 레포 프로젝트 전건이 지금과 같이 돈다). 블록은 있는데 멤버가 0건인
// 것은 다른 사실이다: 사람이 명부를 비운 것이고, 그러면 캐시에 남은 옛 멤버를 걷어야 한다.
//
// Rejected 는 읽었지만 못 받은 멤버 줄이다(RejectedMember 의 사유). Members 와 함께 온다 —
// 한 줄의 결함이 성한 줄을 끌고 가지 않는다.
type Workspace struct {
	Declared bool
	Members  []WorkspaceMember
	Rejected []RejectedMember
}

// MemberProjectID 는 멤버 하나의 프로젝트 id 다. 순수 함수다.
//
// 선언이 이기고, 비면 경로의 마지막 마디다 — 그 규칙은 클라이언트가 자기 cwd 에서
// 프로젝트를 푸는 규칙(ProjectIDFromPath)과 **같아야 한다**. 다르면 루트가 명부에 적은
// 이름과 그 레포에서 띄운 세션이 스스로 등록하는 이름이 갈리고, 그러면 같은 레포가
// 프로젝트 두 개가 된다 — 이 항목이 없애려는 결함 자체가 다른 자리에서 되살아난다.
func MemberProjectID(m WorkspaceMember) string {
	if id := strings.TrimSpace(m.Project); id != "" {
		return id
	}
	return ProjectIDFromPath(m.Path)
}

// ProjectIDFromPath 는 경로에서 기본 프로젝트 id 를 만든다. 순수 함수다.
//
// ★ **정본은 여기다.** cmd/fd 의 동명 함수가 이것을 부른다 — 클라이언트가 자기 cwd 에서
// 푸는 이름과 서버가 명부에서 푸는 이름이 **같은 문자열**이어야 하기 때문이다(위
// MemberProjectID 주석의 사유). 두 벌로 두면 한쪽만 고쳐지고, 그 어긋남은 프로젝트가
// 조용히 둘로 갈리는 것으로만 나타난다.
//
// 항목 id 와 달리 이 값은 셸·git ref 로 나가지 않지만, URL 질의 문자열과 파일 이름으로는
// 나가므로 경계 문자를 걷어낸다.
//
// ★ `path` 를 쓰고 `path/filepath` 를 안 쓴다. 명부의 경로는 **커밋된 파일**에서 오므로
// 슬래시 구분이고, 서버가 어느 OS 에서 도는지와 무관하다. filepath 를 쓰면 윈도우에서
// 도는 서버가 `a/b` 를 한 마디로 읽어 id 가 `a-b` 가 된다.
func ProjectIDFromPath(p string) string {
	base := path.Base(path.Clean(strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))))
	if base == "." || base == "/" || base == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// ─────────────────────────────────────────────────────────────────────────────
// 파싱 — `.flightdeck.yaml` 의 workspace 블록 하나만 읽는다
// ─────────────────────────────────────────────────────────────────────────────

// ParseWorkspace 는 `.flightdeck.yaml` 본문에서 `workspace:` 블록을 읽는다. 순수 함수다.
//
// ★ **YAML 을 파싱하지 않는다 — 이 블록만 읽는다.** 왜 라이브러리를 안 쓰나:
// 이 모듈의 의존은 sqlite 드라이버 하나뿐이고(go.mod), 서버는 그 상태로 정적 바이너리
// 하나로 나간다. 명부 한 블록을 위해 YAML 전체를 들이면 **그 파서가 읽는 모든 것**이
// 이 도구의 입력 표면이 된다 — 앵커·별칭·태그·병합 키까지. 읽는 것을 좁히면 표면도 좁다.
//
// ★ **그래서 읽는 꼴을 못박고, 못 읽는 꼴은 거절한다.** 조용히 0건으로 접지 않는다 —
// 그것이 이 저장소가 반복해서 당한 실패다("관문의 무출력은 통과가 아니다").
// 거절의 단위는 둘이다: 파일의 **꼴**을 못 읽으면 오류(줄 경계부터 못 믿는다), 꼴은 읽었는데
// **멤버 한 줄의 값**이 못 쓰는 것이면 그 줄만 Rejected 로 뺀다(RejectedMember 의 사유). 읽는 꼴:
//
//	workspace:
//	  members:
//	    - project: search-api      # 비면 path 의 마지막 마디
//	      path: platform-search-api
//	    - path: platform-docs
//
// 오류로 거절하는 꼴과 사유:
//   - 플로우 스타일(`workspace: {members: [...]}`) — 읽는 척하고 0건을 내면 사람은
//     "등록했는데 안 붙는다"만 본다. 못 읽는다고 **말한다**.
//   - 탭 들여쓰기 — YAML 규격이 금지하고, 여기서 관대하면 진짜 YAML 도구와 이 파서가
//     같은 파일을 다르게 읽는다.
//
// 줄만 빼는 값: path 없음·절대경로·루트 자신·루트의 형제 폴더 밖(memberPathOK 의 사유), id 를
// 못 짓는 경로.
//
// ★ 다른 최상위 키는 **건너뛴다.** 이 파일에는 이미 labels·verify·secrets·recipes 가
// 살고(설계 §8), 그것들은 이 함수의 관심 밖이다. 모르는 키를 만나 죽으면 명부 하나가
// 파일 전체의 문법 관문이 된다.
func ParseWorkspace(src string) (Workspace, error) {
	lines := splitYAMLLines(src)

	// ① `workspace:` 최상위 키를 찾는다.
	start := -1
	for i, ln := range lines {
		if ln.indent != 0 || ln.blank {
			continue
		}
		key, rest, ok := splitYAMLKey(ln.text)
		if !ok || key != "workspace" {
			continue
		}
		if v := strings.TrimSpace(rest); v != "" {
			return Workspace{}, fmt.Errorf(
				"workspace: 뒤에 값 %q 가 붙어 있다 — 이 파서는 블록 스타일만 읽는다"+
					"(다음 줄부터 `  members:` 로 들여써라)", clipWS(v, 60))
		}
		start = i
		break
	}
	if start < 0 {
		// 블록이 없다. **오류가 아니다** — 단일 레포 프로젝트의 정상 상태다.
		return Workspace{}, nil
	}

	ws := Workspace{Declared: true}

	// ② workspace 블록 안에서 `members:` 를 찾는다.
	body := blockAfter(lines, start)
	membersAt := -1
	memberIndent := 0
	for i, ln := range body {
		if ln.blank {
			continue
		}
		key, rest, ok := splitYAMLKey(ln.text)
		if !ok {
			continue
		}
		if key != "members" {
			continue
		}
		if v := strings.TrimSpace(rest); v != "" {
			return Workspace{}, fmt.Errorf(
				"members: 뒤에 값 %q 가 붙어 있다 — 이 파서는 블록 시퀀스만 읽는다"+
					"(다음 줄부터 `    - path: …` 로 써라)", clipWS(v, 60))
		}
		membersAt, memberIndent = i, ln.indent
		break
	}
	if membersAt < 0 {
		// `workspace:` 는 있는데 `members:` 가 없다. 선언은 있었다고 본다 —
		// 그래야 캐시에 남은 옛 멤버가 걷힌다(Workspace.Declared 주석).
		return ws, nil
	}

	// ③ 시퀀스 항목을 읽는다. `- ` 로 시작하는 줄이 새 멤버다.
	var cur *WorkspaceMember
	flush := func() error {
		if cur == nil {
			return nil
		}
		m := *cur
		cur = nil
		reason := ""
		if err := memberPathOK(m.Path); err != nil {
			reason = err.Error()
		} else if MemberProjectID(m) == "" {
			reason = fmt.Sprintf("멤버의 프로젝트 id 가 비었다(path=%q) — path 의 마지막 마디가 "+
				"경계 문자뿐이면 id 를 지을 수 없다. project 를 명시해라", clipWS(m.Path, 60))
		}
		if reason != "" {
			ws.Rejected = append(ws.Rejected, RejectedMember{Path: m.Path, Project: m.Project, Reason: reason})
			return nil
		}
		ws.Members = append(ws.Members, m)
		return nil
	}
	for _, ln := range body[membersAt+1:] {
		if ln.blank {
			continue
		}
		if ln.indent <= memberIndent {
			break // members 블록이 끝났다
		}
		t := ln.text
		if strings.HasPrefix(t, "- ") || t == "-" {
			if err := flush(); err != nil {
				return Workspace{}, err
			}
			cur = &WorkspaceMember{}
			t = strings.TrimSpace(strings.TrimPrefix(t, "-"))
			if t == "" {
				continue
			}
		} else if cur == nil {
			return Workspace{}, fmt.Errorf("members 아래에 시퀀스가 아닌 줄이 있다: %q", clipWS(t, 60))
		}
		key, rest, ok := splitYAMLKey(t)
		if !ok {
			return Workspace{}, fmt.Errorf("멤버 줄을 `키: 값` 으로 못 읽었다: %q", clipWS(t, 60))
		}
		v, err := scalar(rest)
		if err != nil {
			return Workspace{}, err
		}
		switch key {
		case "project":
			cur.Project = v
		case "path":
			cur.Path = v
		default:
			// 모르는 키는 건너뛴다 — 최상위와 같은 이유다. 명부가 나중에 칸을 하나 더
			// 갖게 되면 옛 서버가 그 파일을 여전히 읽어야 한다.
		}
	}
	if err := flush(); err != nil {
		return Workspace{}, err
	}
	return ws, nil
}

// memberPathOK 는 멤버 경로가 쓸 수 있는 값인지 본다. 순수 함수다.
//
// ★ 왜 절대경로를 막나. 정본이 **커밋된 파일**이라 이 값은 다른 머신·컨테이너에서도
// 읽힌다. `/Users/u/...` 는 그 어느 곳에서도 안 맞고, 안 맞는 경로는 「멤버가
// 실재하지 않는다」로만 나타나 사람이 원인을 못 짚는다.
//
// ★ 루트 자신(`.`)과 부모 마디뿐인 경로(`..`·`../..`)는 막는다. 자기를 멤버로 두면 자원
// 정규화가 자기 참조가 되고, 부모 마디뿐인 경로는 루트를 품은 폴더라 마지막 마디로 id 를
// 지으면 그 이름이 멤버 레포가 아니라 그 폴더의 것이 된다.
//
// ★ **루트 밖은 루트의 형제까지만 받는다**(2026-11-24) — `../x` 와 그 아래(`../x/y`), 곧 정리한
// 경로가 루트의 부모 폴더 아래인 것. 그보다 위(`../../x`·`../../../../etc/x`)는 그 줄을 뺀다.
// 상한이 없으면 커밋된 명부 한 줄로 서버가 디스크 아무 자리에서 git 을 돈다(doctor 의 멤버
// 축 · 보드의 멤버 요약이 그 경로를 연다) — 명부를 쓸 수 있는 사람과 서버 디스크를 읽을 수
// 있는 사람은 같은 사람이 아니다.
//
// ★ 루트의 형제를 받는 이유(2026-11-24). 예전에는 막았다 — «루트가 관장한다»는 전제를
// 깨고 발자국 귀속이 루트 트리 밖 경로를 멤버 상대로 접으려다 엉뚱한 자리를 가리킨다는
// 사유였다. 그 거절이 명부 전체를 얼렸고(RejectedMember), 그 줄은 사람이 멤버 저장소를
// 워크스페이스 폴더 밖으로 옮기며 **일부러** 쓴 것이었다. 지키던 것은 이렇게 남는다:
//   - 경로 귀속(Roster.AttributePath)은 루트 상대 `..` 를 원래 귀속하지 않고, 형제 겹침의
//     좌표 옮김(Roster.PathAsSeenFrom)은 루트 밖 멤버와 루트 사이를 «겹칠 수 없음»으로 낸다 —
//     루트 세션의 발자국은 루트 트리 안 경로만 남기 때문이다(service.RelPathWithin).
//     잃는 것은 그 둘 사이의 겹침 경고이고, 엉뚱한 귀속은 안 생긴다.
//   - id 는 여전히 마지막 마디다(MemberProjectID) — 그 레포에서 띄운 세션이 등록하는 이름과 같다.
//   - 실재하는 저장소인지 · 서버가 그 자리를 읽는지(컨테이너면 마운트 안인지)는 doctor 가 잰다.
func memberPathOK(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return fmt.Errorf("멤버에 path 가 없다 — 루트 상대 경로가 명부의 필수 칸이다")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, ":\\") {
		return fmt.Errorf("멤버 path 가 절대경로다(%q) — 커밋되는 값이라 루트 상대여야 한다", clipWS(p, 80))
	}
	c := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	ups := 0
	for c == ".." || strings.HasPrefix(c, "../") {
		c = strings.TrimPrefix(strings.TrimPrefix(c, ".."), "/")
		ups++
	}
	if c == "" || c == "." {
		return fmt.Errorf("멤버 path 가 루트 자신이거나 부모 마디뿐이다(%q) — 멤버 레포 폴더를 가리켜라", clipWS(p, 80))
	}
	if ups > 1 {
		return fmt.Errorf("멤버 path 가 루트의 형제 폴더 밖이다(%q) — 루트 밖 멤버는 루트의 형제 폴더까지만"+
			"(`../<이름>` 과 그 아래) 받는다", clipWS(p, 80))
	}
	return nil
}

// OutsideRoot 는 루트 상대 멤버 경로가 루트 트리 밖(`../…`)인가다. 순수 함수다.
//
// 그런 멤버는 루트 경로와 합쳐 읽히지만(service.MemberRepoPath) 루트 세션의 발자국 좌표
// (루트 트리 안 상대 경로)로는 닿지 않는다 — 좌표 옮김과 doctor 의 마운트 안내가 이것을 본다.
func OutsideRoot(memberPath string) bool {
	c := path.Clean(strings.ReplaceAll(strings.TrimSpace(memberPath), "\\", "/"))
	return c == ".." || strings.HasPrefix(c, "../")
}

// AdoptRoster 는 파싱한 명부에서 **채택할 멤버**와 **뺀 줄**을 가른다. 순수 함수다.
//
// 파서가 본 것은 한 줄씩의 값이고, 여기서 보는 것은 줄 **사이**의 관계(id 축)다:
//   - 루트 자신과 같은 id — 자원 정규화가 자기 참조가 되고 발자국 귀속이 루트 경로를 다시
//     루트로 접는다. 경로 검증이 `.` 는 막지만 basename 이 겹치는 다른 경로는 못 막는다.
//   - 두 번 나온 id — **먼저 적힌 줄이 이긴다.** 뒤 줄을 이기게 하면 명부 끝에 한 줄을 더한
//     것이 이미 쓰던 멤버의 경로를 조용히 바꾼다.
//
// 채택한 멤버는 Project 가 채워져 나온다(빈 값은 store 가 거절한다). 뺀 줄은 파서가 뺀 것
// 다음에 이 함수가 뺀 것이 파일 순서대로 온다.
func AdoptRoster(root string, ws Workspace) ([]WorkspaceMember, []RejectedMember) {
	members := make([]WorkspaceMember, 0, len(ws.Members))
	rejected := append([]RejectedMember(nil), ws.Rejected...)
	seen := make(map[string]bool, len(ws.Members))
	for _, m := range ws.Members {
		id := MemberProjectID(m)
		switch {
		case id == root:
			rejected = append(rejected, RejectedMember{Path: m.Path, Project: m.Project,
				Reason: fmt.Sprintf("멤버 %q 가 루트 자신과 같은 프로젝트 id 다", clipWS(id, 64))})
		case seen[id]:
			rejected = append(rejected, RejectedMember{Path: m.Path, Project: m.Project,
				Reason: fmt.Sprintf("멤버 프로젝트 id %q 가 두 번 나온다 — 먼저 적힌 줄을 쓴다", clipWS(id, 64))})
		default:
			seen[id] = true
			members = append(members, WorkspaceMember{Project: id, Path: m.Path})
		}
	}
	return members, rejected
}

// yamlLine 은 들여쓰기를 뗀 한 줄이다.
type yamlLine struct {
	indent int
	text   string
	blank  bool
}

// splitYAMLLines 는 주석·빈 줄을 걷고 들여쓰기를 센다.
//
// ★ 문서 구분자(`---`)를 만나면 **거기서 끝낸다.** 여러 문서가 든 파일에서 둘째
// 문서의 `workspace:` 를 읽으면, 진짜 YAML 도구가 첫 문서만 보는 것과 갈린다.
//
// ★ 주석은 `#` 이 **줄 첫 글자이거나 공백 뒤**일 때만 주석이다. `path: a#b` 의 `#` 은
// 값의 일부다(YAML 규격이 그렇다) — 여기서 잘라내면 경로가 조용히 바뀐다.
func splitYAMLLines(src string) []yamlLine {
	var out []yamlLine
	for _, raw := range strings.Split(src, "\n") {
		raw = strings.TrimRight(raw, "\r")
		trimmed := strings.TrimLeft(raw, " ")
		if trimmed == "---" || trimmed == "..." {
			if len(out) > 0 {
				break // 첫 문서가 끝났다
			}
			continue // 파일 첫머리의 시작 표식이다
		}
		indent := len(raw) - len(trimmed)
		if i := commentAt(trimmed); i >= 0 {
			trimmed = strings.TrimRight(trimmed[:i], " ")
		}
		if trimmed == "" {
			out = append(out, yamlLine{blank: true})
			continue
		}
		out = append(out, yamlLine{indent: indent, text: trimmed})
	}
	return out
}

// commentAt 은 주석이 시작되는 자리다. 없으면 -1. 인용부호 안의 `#` 은 주석이 아니다.
func commentAt(s string) int {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || s[i-1] == ' '):
			return i
		}
	}
	return -1
}

// blockAfter 는 start 줄보다 깊게 들여쓴 연속 구간이다.
func blockAfter(lines []yamlLine, start int) []yamlLine {
	base := lines[start].indent
	var out []yamlLine
	for _, ln := range lines[start+1:] {
		if ln.blank {
			out = append(out, ln)
			continue
		}
		if ln.indent <= base {
			break
		}
		out = append(out, ln)
	}
	return out
}

// splitYAMLKey 는 `키: 나머지` 를 가른다. 키에 콜론이 없으면 ok=false.
func splitYAMLKey(s string) (key, rest string, ok bool) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(s[:i])
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	return key, s[i+1:], true
}

// scalar 는 값 한 칸을 읽는다. 인용부호를 벗기고, 탭 들여쓰기를 거절한다.
//
// ★ 이스케이프를 **안 푼다.** 멤버 경로에 `\n` 을 쓸 이유가 없고, 푸는 순간 어느
// 규격을 따르는지가 또 하나의 답할 질문이 된다. 큰따옴표 안의 역슬래시는 글자 그대로다.
func scalar(rest string) (string, error) {
	if strings.Contains(rest, "\t") {
		return "", fmt.Errorf("값에 탭이 있다(%q) — YAML 은 탭 들여쓰기를 금지한다", clipWS(rest, 60))
	}
	v := strings.TrimSpace(rest)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1], nil
		}
	}
	if strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") {
		return "", fmt.Errorf("플로우 스타일(%q)은 이 파서가 안 읽는다 — 블록으로 써라", clipWS(v, 60))
	}
	return v, nil
}

// clipWS 는 오류 문면에 실을 외부 문자열을 자른다(judge 는 service 의 clip 을 못 쓴다).
func clipWS(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
