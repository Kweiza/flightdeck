package judge

import (
	"strings"
	"testing"
)

// 블록이 없으면 **아무 일도 안 일어난다** — 단일 레포 프로젝트 전건이 지금과 같이 돈다.
//
// Declared 가 false 인 것과 멤버가 0건인 것을 가르는 이유가 이 시험의 전부다:
// 그 둘이 접히면 «명부를 비웠다»가 «명부가 없다»와 같아져 캐시에 남은 옛 멤버를
// 영영 못 걷는다.
func TestParseWorkspaceUndeclaredIsNotAnError(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"빈 파일", ""},
		{"다른 키만", "schema: 1\nname: solo\ndefault_branch: main\n"},
		{"주석만", "# workspace: 이건 주석이다\n"},
		{"들여쓴 workspace 는 최상위가 아니다", "verify:\n  workspace:\n    members:\n      - path: x\n"},
	}
	for _, c := range cases {
		ws, err := ParseWorkspace(c.src)
		if err != nil {
			t.Errorf("%s: 오류가 나면 안 된다: %v", c.name, err)
		}
		if ws.Declared {
			t.Errorf("%s: Declared=true 였다 — 블록이 없으면 선언도 없다", c.name)
		}
		if len(ws.Members) != 0 {
			t.Errorf("%s: 멤버 %d건이 나왔다", c.name, len(ws.Members))
		}
	}
}

// 정상 명부를 읽는다. project 가 비면 path 의 마지막 마디가 id 다.
func TestParseWorkspaceReadsBlockMembers(t *testing.T) {
	src := `schema: 1
name: cp-root
workspace:
  members:
    - project: search-api
      path: platform-search-api
    - path: platform-docs        # project 생략 → basename
    - path: "nested/inner"
labels:
  values: [a, b]
`
	ws, err := ParseWorkspace(src)
	if err != nil {
		t.Fatalf("파싱 실패: %v", err)
	}
	if !ws.Declared {
		t.Fatal("Declared=false 다")
	}
	if len(ws.Members) != 3 {
		t.Fatalf("멤버 %d건 — 3건이어야 한다: %+v", len(ws.Members), ws.Members)
	}
	want := []struct{ id, path string }{
		{"search-api", "platform-search-api"},
		{"platform-docs", "platform-docs"},
		{"inner", "nested/inner"},
	}
	for i, w := range want {
		if got := MemberProjectID(ws.Members[i]); got != w.id {
			t.Errorf("멤버 %d: id=%q, 기대 %q", i, got, w.id)
		}
		if ws.Members[i].Path != w.path {
			t.Errorf("멤버 %d: path=%q, 기대 %q", i, ws.Members[i].Path, w.path)
		}
	}
	// ★ 그 뒤의 최상위 키(labels)가 명부에 안 새어 든다 — 블록 경계를 들여쓰기로 읽는다.
}

// `workspace:` 는 있는데 `members:` 가 없으면 **선언은 있었다**고 본다.
func TestParseWorkspaceDeclaredWithoutMembers(t *testing.T) {
	ws, err := ParseWorkspace("workspace:\n  # 아직 아무도 안 넣었다\n")
	if err != nil {
		t.Fatalf("오류: %v", err)
	}
	if !ws.Declared || len(ws.Members) != 0 {
		t.Fatalf("Declared=%v 멤버=%d — 선언 있음·멤버 0건이어야 한다", ws.Declared, len(ws.Members))
	}
}

// 못 읽는 꼴은 **거절한다**. 조용히 0건으로 접으면 사람은 "등록했는데 안 붙는다"만 본다.
func TestParseWorkspaceRefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantSub string
	}{
		{
			"플로우 스타일(최상위)",
			"workspace: {members: [{path: a}]}\n",
			"블록 스타일만 읽는다",
		},
		{
			"플로우 스타일(members)",
			"workspace:\n  members: [{path: a}]\n",
			"블록 시퀀스만 읽는다",
		},
		{
			"값이 플로우",
			"workspace:\n  members:\n    - path: [a, b]\n",
			"플로우 스타일",
		},
		{
			"탭 들여쓰기",
			"workspace:\n  members:\n    - path:\tx\n",
			"탭",
		},
		{
			"시퀀스가 아닌 줄",
			"workspace:\n  members:\n    path: a\n",
			"시퀀스가 아닌 줄",
		},
	}
	for _, c := range cases {
		_, err := ParseWorkspace(c.src)
		if err == nil {
			t.Errorf("%s: 거절해야 하는데 통과했다", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%s: 사유에 %q 가 없다 — 실제: %v", c.name, c.wantSub, err)
		}
	}
}

// 멤버 한 줄의 결함은 **그 줄만** 뺀다. 명부 전체를 거절하면 캐시가 마지막으로 받아들인
// 명부에 얼어붙고, 그 뒤에 올린 멤버들이 아무 표시 없이 빠진다(실배치에서 몇 주를 그렇게 갔다).
//
// ★ 뺀 줄은 **사유와 함께** 남는다(Rejected). 0건으로 접지 않는다는 이 파서의 규율은 그대로다 —
// 거절하는 단위가 파일에서 줄로 좁아졌을 뿐이다.
func TestParseWorkspaceRejectsOnlyTheFaultyMember(t *testing.T) {
	cases := []struct {
		name    string
		bad     string
		wantSub string
	}{
		{"절대경로", "    - path: /Users/u/cp\n", "절대경로"},
		{"루트 자신", "    - path: .\n", "루트 자신"},
		{"루트의 부모 자신", "    - path: ../\n", "루트 자신이거나 부모 마디뿐"},
		{"부모 마디뿐", "    - path: ../..\n", "루트 자신이거나 부모 마디뿐"},
		{"path 없음", "    - project: only-a-name\n", "path 가 없다"},
		{"id 를 못 짓는다", "    - path: \"...\"\n", "프로젝트 id 가 비었다"},
	}
	for _, c := range cases {
		src := "workspace:\n  members:\n    - path: good-a\n" + c.bad + "    - path: good-b\n"
		ws, err := ParseWorkspace(src)
		if err != nil {
			t.Errorf("%s: 한 줄 결함이 명부 전체를 거절했다: %v", c.name, err)
			continue
		}
		if len(ws.Members) != 2 || ws.Members[0].Path != "good-a" || ws.Members[1].Path != "good-b" {
			t.Errorf("%s: 성한 줄이 안 남았다: %+v", c.name, ws.Members)
		}
		if len(ws.Rejected) != 1 {
			t.Errorf("%s: 뺀 줄이 %d건 — 1건이어야 한다: %+v", c.name, len(ws.Rejected), ws.Rejected)
			continue
		}
		if !strings.Contains(ws.Rejected[0].Reason, c.wantSub) {
			t.Errorf("%s: 사유에 %q 가 없다 — 실제: %q", c.name, c.wantSub, ws.Rejected[0].Reason)
		}
	}
}

// 루트 밖 멤버(`../x`)는 **받는다.** 멤버 저장소를 워크스페이스 폴더 밖으로 옮긴 배치가
// 실재하고, 그 멤버도 루트 경로와 합쳐 읽히는 자리다(service.MemberRepoPath).
// id 는 여전히 마지막 마디다 — 그 레포에서 띄운 세션이 스스로 등록하는 이름과 같아야 한다.
func TestParseWorkspaceAcceptsMembersOutsideTheRoot(t *testing.T) {
	ws, err := ParseWorkspace("workspace:\n  members:\n    - path: ../sibling\n    - path: ../../far/away\n")
	if err != nil {
		t.Fatalf("오류: %v", err)
	}
	if len(ws.Rejected) != 0 {
		t.Fatalf("루트 밖 멤버를 뺐다: %+v", ws.Rejected)
	}
	if len(ws.Members) != 2 {
		t.Fatalf("멤버=%+v", ws.Members)
	}
	if got := MemberProjectID(ws.Members[0]); got != "sibling" {
		t.Errorf("id=%q — sibling 이어야 한다", got)
	}
	if got := MemberProjectID(ws.Members[1]); got != "away" {
		t.Errorf("id=%q — away 여야 한다", got)
	}
}

// 채택은 id 축의 결함(루트 자신 · 겹침)도 **그 줄만** 뺀다. 겹치면 먼저 적힌 줄이 이긴다 —
// 뒤 줄을 이기게 하면 명부 끝에 한 줄을 더한 것이 이미 쓰던 멤버의 경로를 조용히 바꾼다.
func TestAdoptRosterDropsSelfAndDuplicateIDs(t *testing.T) {
	ws := Workspace{Declared: true, Members: []WorkspaceMember{
		{Path: "a"},
		{Path: "nested/root"},           // 마지막 마디가 루트 id 와 같다
		{Project: "a", Path: "other/a"}, // id 가 앞 줄과 겹친다
		{Project: "b", Path: "../b"},
	}, Rejected: []RejectedMember{{Path: "/abs", Reason: "파서가 뺀 줄"}}}

	got, rejected := AdoptRoster("root", ws)
	if len(got) != 2 || got[0] != (WorkspaceMember{Project: "a", Path: "a"}) ||
		got[1] != (WorkspaceMember{Project: "b", Path: "../b"}) {
		t.Fatalf("채택=%+v — a·b 둘이어야 한다(id 가 채워져서)", got)
	}
	if len(rejected) != 3 {
		t.Fatalf("뺀 줄=%+v — 파서가 뺀 1 + 루트 자신 1 + 겹침 1", rejected)
	}
	if rejected[0].Path != "/abs" {
		t.Errorf("파서가 뺀 줄이 먼저 와야 한다: %+v", rejected)
	}
	if !strings.Contains(rejected[1].Reason, "루트 자신과 같은") || rejected[1].Path != "nested/root" {
		t.Errorf("루트 자신 사유: %+v", rejected[1])
	}
	if !strings.Contains(rejected[2].Reason, "두 번") || rejected[2].Path != "other/a" {
		t.Errorf("겹침 사유: %+v", rejected[2])
	}
}

// 값 안의 `#` 은 주석이 아니다. 잘라내면 경로가 조용히 바뀐다.
//
// ★ **인용 없는 꼴이 이 시험의 본체다.** 처음엔 `"a#b"` 만 재고 통과라고 적었는데,
// 변이(주석 판정에서 공백 조건을 뗀 것)를 넣어도 초록이었다 — 큰따옴표가 먼저 막아서
// 정작 재려던 줄에 닿지 않았다. YAML 규격에서 `#` 이 주석이 되는 것은 **공백 뒤**일
// 때뿐이고, 인용 없는 `a#b` 가 그 규칙이 걸리는 유일한 자리다.
func TestParseWorkspaceHashInsideValueIsNotAComment(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"인용 없음", "workspace:\n  members:\n    - path: a#b\n", "a#b"},
		{"큰따옴표", "workspace:\n  members:\n    - path: \"a#b\"\n", "a#b"},
		{"공백 뒤 #는 주석", "workspace:\n  members:\n    - path: ab # 여긴 주석\n", "ab"},
	}
	for _, c := range cases {
		ws, err := ParseWorkspace(c.src)
		if err != nil {
			t.Errorf("%s: 오류: %v", c.name, err)
			continue
		}
		if len(ws.Members) != 1 || ws.Members[0].Path != c.want {
			t.Errorf("%s: path=%+v — %q 여야 한다", c.name, ws.Members, c.want)
		}
	}
}

// 문서 구분자 뒤는 **둘째 문서**라 안 읽는다. 진짜 YAML 도구가 첫 문서만 보는 것과 맞춘다.
//
// ★ 첫 문서에 블록이 **없는** 꼴로 잰다. 양쪽 문서에 다 두면 «첫 키에서 멈춘다»는
// 다른 규칙이 먼저 걸려서, 구분자를 아예 무시하도록 변이를 넣어도 초록이 나온다
// (실측 — 그래서 이 시험이 이 꼴로 다시 쓰였다).
func TestParseWorkspaceStopsAtDocumentSeparator(t *testing.T) {
	src := "schema: 1\n---\nworkspace:\n  members:\n    - path: second\n"
	ws, err := ParseWorkspace(src)
	if err != nil {
		t.Fatalf("오류: %v", err)
	}
	if ws.Declared || len(ws.Members) != 0 {
		t.Fatalf("Declared=%v 멤버=%+v — 둘째 문서는 안 읽어야 한다", ws.Declared, ws.Members)
	}
}

// 모르는 키는 건너뛴다 — 명부가 나중에 칸을 얻어도 옛 서버가 그 파일을 읽어야 한다.
func TestParseWorkspaceSkipsUnknownMemberKeys(t *testing.T) {
	ws, err := ParseWorkspace("workspace:\n  members:\n    - path: a\n      branch: main\n      note: 나중에 생긴 칸\n")
	if err != nil {
		t.Fatalf("오류: %v", err)
	}
	if len(ws.Members) != 1 || ws.Members[0].Path != "a" {
		t.Fatalf("멤버=%+v", ws.Members)
	}
}

// 프로젝트 id 규칙은 **클라이언트와 같은 문자열**을 내야 한다 — 다르면 같은 레포가
// 프로젝트 둘로 갈린다. cmd/fd 의 동명 함수가 이것을 부른다(그 위임이 이 대칭의 자리다).
func TestProjectIDFromPathStripsBoundaryCharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/b/platform-search-api", "platform-search-api"},
		{"nested/inner", "inner"},
		{"a b", "a-b"},
		{"-lead-", "lead"},
		{"", ""},
		{"/", ""},
		{"...", ""},
		// 슬래시 구분으로 읽는다 — 서버가 윈도우에서 돌아도 커밋된 값의 뜻은 안 바뀐다.
		{"a\\b", "b"},
	}
	for _, c := range cases {
		if got := ProjectIDFromPath(c.in); got != c.want {
			t.Errorf("ProjectIDFromPath(%q) = %q, 기대 %q", c.in, got, c.want)
		}
	}
}
