package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

var loginCodeRe = regexp.MustCompile(`fdl_[0-9A-Za-z]{43}`)

// TestAdminBootstrapThenRecover 는 관리자 손잡이 둘이다: bootstrap 은 서버를 거쳐 FD_TOKEN 으로 한 번만 서고, recover 는 원장을
// 직접 열어(서버 컨테이너 안의 길) 그 관리자의 새 로그인 코드를 낸다. 코드는 stdout 에 한 번만 보이고 로그에는 안 실린다.
func TestAdminBootstrapThenRecover(t *testing.T) {
	h := newHarnessAuth(t, "harness-token")
	h.env["FD_TOKEN"] = "harness-token"

	code, out := h.run("", "admin", "bootstrap", "--login", "admin")
	if code != 0 {
		t.Fatalf("bootstrap 종료코드 %d:\n%s", code, out)
	}
	first := loginCodeRe.FindString(out)
	if first == "" {
		t.Fatalf("bootstrap 이 로그인 코드를 안 보였다:\n%s", out)
	}
	if code, out := h.run("", "admin", "bootstrap", "--login", "admin2"); code != 1 || !strings.Contains(out, "fd admin recover") {
		t.Fatalf("두 번째 bootstrap 이 %d 다(복구 길을 말해야 한다):\n%s", code, out)
	}
	noToken := map[string]string{}
	for k, v := range h.env {
		noToken[k] = v
	}
	delete(noToken, "FD_TOKEN")
	if code, _ := h.runEnv(noToken, "", "admin", "bootstrap", "--login", "x"); code != 2 {
		t.Fatalf("FD_TOKEN 없는 bootstrap 이 %d 다", code)
	}

	var out2, errb bytes.Buffer
	rc := run([]string{"admin", "recover", "admin", "--db", h.db}, envOf(h.env), strings.NewReader(""), &out2, &errb)
	if rc != 0 {
		t.Fatalf("recover 종료코드 %d:\n%s\n%s", rc, out2.String(), errb.String())
	}
	second := loginCodeRe.FindString(out2.String())
	if second == "" || second == first {
		t.Fatalf("recover 가 새 로그인 코드를 안 냈다:\n%s", out2.String())
	}
	if strings.Contains(errb.String(), second) {
		t.Fatalf("로그(stderr)에 로그인 코드 원값이 실렸다")
	}
	if _, err := h.st.ExchangeLoginCode(context.Background(), second, "", time.Now()); err != nil {
		t.Fatalf("recover 가 낸 코드가 안 바뀐다: %v", err)
	}
	if code, out := h.run("", "admin", "recover", "nobody", "--db", h.db); code != 1 || !strings.Contains(out, "계정") {
		t.Fatalf("없는 계정 recover 가 %d 다:\n%s", code, out)
	}
	if code, _ := h.run("", "admin", "recover"); code != 2 {
		t.Fatalf("로그인 이름 없는 recover 가 %d 다", code)
	}
}
