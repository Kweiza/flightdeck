package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/auth"
	"github.com/kweiza/flightdeck/internal/store"
)

// serveUntilBind 는 runServe 를 이미 물린 포트로 부른다. 인증 설정의 기동 판정을 지나면 바인드에서 「서버를 띄우지 못했다」로
// 서므로, 그 로그 줄이 있으면 판정이 기동을 막지 않았다는 뜻이다(deploy_note_bind_test.go 와 같은 장치).
func serveUntilBind(t *testing.T, vars map[string]string) (code int, logs string) {
	t.Helper()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { busy.Close() })
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fd.db")
	if err := store.Migrate(context.Background(), dbPath, nil); err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		if k == "FD_LEDGER" {
			return filepath.Join(dir, "ledger"), true
		}
		v, ok := vars[k]
		return v, ok
	}
	var buf bytes.Buffer
	code = runServe([]string{"--addr", busy.Addr().String(), "--db", dbPath}, env, slog.New(slog.NewJSONHandler(&buf, nil)))
	return code, buf.String()
}

// TestServeRefusesAccountsWithNoWayToTheFirstAdmin 은 사소 11 이다: accounts 단계 · FD_TOKEN 빈 값 · 관리자 0명이면 첫 관리자를
// 만들 길이 없으므로 포트를 열기 전에 선다. FD_TOKEN 이 있거나 transition 이면 그대로 뜬다.
func TestServeRefusesAccountsWithNoWayToTheFirstAdmin(t *testing.T) {
	code, logs := serveUntilBind(t, map[string]string{"FD_AUTH": "accounts"})
	if code != 1 || !strings.Contains(logs, "첫 관리자를 만들 길이 없어") || strings.Contains(logs, "서버를 띄우지 못했다") {
		t.Fatalf("첫 관리자를 만들 길이 없는 서버가 섰어야 한다(%d): %s", code, logs)
	}
	for name, vars := range map[string]map[string]string{
		"accounts · 토큰 있음":   {"FD_AUTH": "accounts", "FD_TOKEN": "plain-shared-secret"},
		"transition · 토큰 없음": {},
	} {
		if _, logs := serveUntilBind(t, vars); strings.Contains(logs, "첫 관리자") || !strings.Contains(logs, "서버를 띄우지 못했다") {
			t.Errorf("%s: 기동 판정이 막았다: %s", name, logs)
		}
	}
}

// TestServeWarnsOnASharedTokenWithAReservedPrefix 는 중요 6 의 기동 쪽이다: 예약 접두로 시작하는 FD_TOKEN 은 거절하지 않고(그
// 배포가 판을 올리는 순간 서버째 못 뜬다) 경고만 하며, 경고에 값을 싣지 않는다.
func TestServeWarnsOnASharedTokenWithAReservedPrefix(t *testing.T) {
	tok, err := auth.NewToken(auth.DeviceToken)
	if err != nil {
		t.Fatal(err)
	}
	_, logs := serveUntilBind(t, map[string]string{"FD_TOKEN": tok})
	if !strings.Contains(logs, "예약 접두") || !strings.Contains(logs, "서버를 띄우지 못했다") {
		t.Fatalf("예약 접두 FD_TOKEN 이 경고 없이 떴거나 기동을 막았다: %s", logs)
	}
	if strings.Contains(logs, tok) || strings.Contains(logs, tok[4:]) {
		t.Fatal("경고에 FD_TOKEN 값이 실렸다")
	}
	if _, logs := serveUntilBind(t, map[string]string{"FD_TOKEN": "plain-shared-secret"}); strings.Contains(logs, "예약 접두") {
		t.Fatalf("예약 접두가 없는 FD_TOKEN 에 경고가 났다: %s", logs)
	}
}
