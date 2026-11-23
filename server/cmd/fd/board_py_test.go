package main

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 보드(board/board.py)는 Go 가 아니라 파이썬 한 파일이라 go test 가 안 본다. 그래서 그 시험
// (board/test_board.py: 카드 본문 규약 · 결정 카드 네 부분 · 선점 꼴 둘 · 파일 창구의 거부)을
// 여기서 돌려 **같은 관문**에 물린다. 보드가 읽는 서버 응답 꼴은 api 패키지의
// TestBoardReadsTheseFields 가 따로 잠근다.
//
// ★ 종료코드가 아니라 unittest 의 마지막 「OK」 줄과 돈 시험 수를 단언한다 — 발견이 0건이면
// unittest 도 「OK」로 끝나므로 수가 0 이 아닌지를 함께 본다.
func TestBoardPythonSuite(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 가 없다 — 보드 시험을 못 돌린다(보드는 python3 로만 돈다)")
	}
	root := pluginRoot(t)
	cmd := exec.Command(py, "-I", "-B", "-m", "unittest", "discover", "-s", filepath.Join(root, "board"), "-p", "test_*.py")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		t.Fatalf("보드 파이썬 시험이 실패했다(%v):\n%s", err, text)
	}
	m := regexp.MustCompile(`(?m)^Ran (\d+) tests? in`).FindStringSubmatch(text)
	if m == nil || m[1] == "0" {
		t.Fatalf("보드 시험이 하나도 안 돌았다:\n%s", text)
	}
	if !strings.Contains(text, "\nOK") {
		t.Fatalf("보드 시험 출력에 OK 줄이 없다:\n%s", text)
	}
}
