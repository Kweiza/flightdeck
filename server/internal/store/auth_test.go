package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/auth"
)

// 계정 판정 길(store/auth.go)의 시험. 두 엔진에서 같은 단정을 돈다 - 운영 원장이 Postgres 다.

var authT0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func refusalCode(err error) string {
	var re *AccountRefusedError
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

func addAccount(t *testing.T, s *Store, id, role string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO account(id, login, display_name, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, id, id, role, fmtTime(authT0)); err != nil {
		t.Fatal(err)
	}
}

// addDevice 는 계정에 딸린 기기 하나를 넣고 그 기기 토큰 원값을 돌려준다. token=false 면 토큰 없는 옛 머신이다.
func addDevice(t *testing.T, s *Store, id, account string, token bool) string {
	t.Helper()
	var raw string
	var hash any
	if token {
		v, err := auth.NewToken(auth.DeviceToken)
		if err != nil {
			t.Fatal(err)
		}
		raw, hash = v, auth.Hash(v)
	}
	if _, err := s.db.Exec(`INSERT INTO machine(id, hostname, first_seen, last_seen, account_id, name, token_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, id, fmtTime(authT0), fmtTime(authT0), account, id, hash); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBootstrapAdminOpensOnceAndOnlyWithNoAdmins(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		if n, err := s.AdminCount(ctx); err != nil || n != 0 {
			t.Fatalf("빈 원장 관리자 수 = %d, %v", n, err)
		}
		if _, err := s.BootstrapAdmin(ctx, "Bad Login", "", "", authT0); refusalCode(err) != RefuseBadLogin {
			t.Fatalf("나쁜 로그인 이름: %v", err)
		}
		g, err := s.BootstrapAdmin(ctx, "admin", "", "a@example.invalid", authT0)
		if err != nil {
			t.Fatal(err)
		}
		if k, ok := auth.KindOf(g.Code); !ok || k != auth.LoginCode || !g.ExpiresAt.Equal(authT0.Add(auth.LoginCodeTTL)) {
			t.Fatalf("로그인 코드 %+v", g)
		}
		if n, _ := s.AdminCount(ctx); n != 1 {
			t.Fatalf("부트스트랩 뒤 관리자 수 = %d", n)
		}
		if _, err := s.BootstrapAdmin(ctx, "second", "", "", authT0); refusalCode(err) != RefuseBootstrapClosed {
			t.Fatalf("관리자가 생긴 뒤 부트스트랩이 또 섰다: %v", err)
		}
		// 꺼진 관리자도 센다 - 관리자를 끄고 부트스트랩을 다시 열어 FD_TOKEN 으로 새 관리자를 세우는 길을 안 만든다.
		addAccount(t, s, "other-admin", auth.RoleAdmin)
		if _, err := s.SetAccountDisabled(ctx, g.AccountID, true, "", authT0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetAccountDisabled(ctx, "other-admin", true, "", authT0); refusalCode(err) != RefuseLastAdmin {
			t.Fatalf("마지막 관리자 끄기: %v", err)
		}
		if n, _ := s.AdminCount(ctx); n != 2 {
			t.Fatalf("꺼진 관리자를 안 셌다: %d", n)
		}
		// 원장에는 코드의 지문만 남는다.
		assertNoRawSecret(t, s, g.Code)
	})
}

func TestBootstrapAdminRaceStandsOne(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		const n = 6
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = s.BootstrapAdmin(ctx, "admin"+string(rune('a'+i)), "", "", authT0)
			}(i)
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case refusalCode(err) != RefuseBootstrapClosed:
				t.Errorf("동시 부트스트랩의 거절이 bootstrap_closed 가 아니다: %v", err)
			}
		}
		if ok != 1 {
			t.Fatalf("동시 부트스트랩 %d 건 중 %d 건이 섰다 - 하나여야 한다", n, ok)
		}
		if c, _ := s.AdminCount(ctx); c != 1 {
			t.Fatalf("관리자 수 = %d", c)
		}
	})
}

func TestLoginCodeIsOneTimeAndBecomesASession(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		g, err := s.BootstrapAdmin(ctx, "admin", "", "", authT0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ExchangeLoginCode(ctx, "not-a-code", "ua", authT0); !errors.Is(err, ErrLoginCodeRefused) {
			t.Fatalf("모양이 틀린 코드: %v", err)
		}
		// 로그인 코드는 그 자체로 화면 세션이 아니다(같은 표의 행이지만 쿠키 자리에서는 접두가 거른다 - auth.pick).
		w, err := s.ExchangeLoginCode(ctx, g.Code, "Mozilla/5.0\x00\x1b[31m", authT0.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if k, ok := auth.KindOf(w.Cookie); !ok || k != auth.SessionToken || w.IDHash != auth.Hash(w.Cookie) || w.AccountID != g.AccountID {
			t.Fatalf("화면 세션 %+v", w)
		}
		f, err := s.WebSessionCredential(ctx, w.IDHash)
		if err != nil || f.Role != auth.RoleAdmin || f.AccountID != g.AccountID || !f.ExpiresAt.Equal(authT0.Add(time.Minute+auth.SessionTTL)) {
			t.Fatalf("세션 조회 %+v, %v", f, err)
		}
		if _, err := s.ExchangeLoginCode(ctx, g.Code, "ua", authT0.Add(2*time.Minute)); !errors.Is(err, ErrLoginCodeRefused) {
			t.Fatalf("같은 코드가 두 번 통했다: %v", err)
		}
		if _, err := s.WebSessionCredential(ctx, auth.Hash(g.Code)); !errors.Is(err, auth.ErrUnknown) {
			t.Fatalf("쓴 코드 행이 남았다: %v", err)
		}
		assertNoRawSecret(t, s, g.Code, w.Cookie)

		// 만료된 코드 · 꺼진 계정의 코드는 안 바뀐다.
		r, err := s.RecoverAdmin(ctx, "admin", authT0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ExchangeLoginCode(ctx, r.Code, "ua", authT0.Add(auth.LoginCodeTTL)); !errors.Is(err, ErrLoginCodeRefused) {
			t.Fatalf("만료 시각의 코드가 통했다: %v", err)
		}

		// 로그아웃은 그 세션 행만 지운다.
		acct, ended, err := s.EndWebSession(ctx, w.IDHash)
		if err != nil || !ended || acct != g.AccountID {
			t.Fatalf("로그아웃 %q %v %v", acct, ended, err)
		}
		if _, ended, _ := s.EndWebSession(ctx, w.IDHash); ended {
			t.Fatalf("지운 세션을 또 지웠다고 했다")
		}
		if _, err := s.WebSessionCredential(ctx, w.IDHash); !errors.Is(err, auth.ErrUnknown) {
			t.Fatalf("로그아웃한 세션이 남았다: %v", err)
		}
	})
}

func TestRecoverAdmin(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		var nf *NotFoundError
		if _, err := s.RecoverAdmin(ctx, "ghost", authT0); !errors.As(err, &nf) || nf.Kind != NFAccount {
			t.Fatalf("없는 계정: %v", err)
		}
		addAccount(t, s, "member", auth.RoleMember)
		if _, err := s.RecoverAdmin(ctx, "member", authT0); refusalCode(err) != RefuseNotAdmin {
			t.Fatalf("일반 계정 복구: %v", err)
		}
		addAccount(t, s, "boss", auth.RoleAdmin)
		addAccount(t, s, "boss2", auth.RoleAdmin)
		if _, err := s.SetAccountDisabled(ctx, "boss", true, "boss2", authT0); err != nil {
			t.Fatal(err)
		}
		g, err := s.RecoverAdmin(ctx, "boss", authT0)
		if err != nil || !g.Reenabled || g.AccountID != "boss" {
			t.Fatalf("꺼진 관리자 복구 %+v, %v", g, err)
		}
		w, err := s.ExchangeLoginCode(ctx, g.Code, "", authT0.Add(time.Minute))
		if err != nil {
			t.Fatalf("복구 코드가 안 바뀐다: %v", err)
		}
		if f, err := s.WebSessionCredential(ctx, w.IDHash); err != nil || f.Disabled {
			t.Fatalf("복구한 계정이 아직 꺼져 있다: %+v %v", f, err)
		}
	})
}

func TestRevokeMachineAndDisableReachTheCredential(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		addAccount(t, s, "boss", auth.RoleAdmin)
		addAccount(t, s, "kim", auth.RoleMember)
		tok := addDevice(t, s, "m-kim", "kim", true)
		addDevice(t, s, "m-old", "kim", false)

		f, err := s.DeviceCredential(ctx, auth.Hash(tok))
		if err != nil || f.MachineID != "m-kim" || f.AccountID != "kim" || f.Role != auth.RoleMember || f.Revoked || f.Disabled {
			t.Fatalf("기기 조회 %+v, %v", f, err)
		}
		if _, err := s.DeviceCredential(ctx, auth.Hash("fdd_nope")); !errors.Is(err, auth.ErrUnknown) {
			t.Fatalf("모르는 지문: %v", err)
		}

		// permit 이 거절하면 아무것도 안 바뀐다(트랜잭션이 되감긴다).
		denied := errors.New("denied")
		var gotOwner string
		if _, err := s.RevokeMachine(ctx, "m-kim", func(owner string) error { gotOwner = owner; return denied }, "x", authT0); !errors.Is(err, denied) || gotOwner != "kim" {
			t.Fatalf("거절된 철회: %v (owner %q)", err, gotOwner)
		}
		if f, _ := s.DeviceCredential(ctx, auth.Hash(tok)); f.Revoked {
			t.Fatalf("거절된 철회가 기록됐다")
		}
		if _, err := s.RevokeMachine(ctx, "m-ghost", func(owner string) error { gotOwner = owner; return denied }, "x", authT0); !errors.Is(err, denied) || gotOwner != "" {
			t.Fatalf("없는 기기가 permit 보다 먼저 404 를 냈다(기기 id 를 캐 볼 수 있다): %v (owner %q)", err, gotOwner)
		}
		allow := func(string) error { return nil }
		var nf *NotFoundError
		if _, err := s.RevokeMachine(ctx, "m-ghost", allow, "boss", authT0); !errors.As(err, &nf) || nf.Kind != NFMachine {
			t.Fatalf("없는 기기: %v", err)
		}
		if _, err := s.RevokeMachine(ctx, "m-old", allow, "boss", authT0); refusalCode(err) != RefuseNoDeviceToken {
			t.Fatalf("토큰 없는 기기: %v", err)
		}
		r, err := s.RevokeMachine(ctx, "m-kim", allow, "boss", authT0)
		if err != nil || !r.Changed || r.AccountID != "kim" {
			t.Fatalf("철회 %+v, %v", r, err)
		}
		if f, _ := s.DeviceCredential(ctx, auth.Hash(tok)); !f.Revoked {
			t.Fatalf("철회가 다음 조회에 안 보인다")
		}
		if r, err := s.RevokeMachine(ctx, "m-kim", allow, "boss", authT0); err != nil || r.Changed {
			t.Fatalf("두 번째 철회는 쓰기 0 이어야 한다: %+v, %v", r, err)
		}

		// 계정 끄기 · 켜기.
		tok2 := addDevice(t, s, "m-kim2", "kim", true)
		if r, err := s.SetAccountDisabled(ctx, "kim", true, "boss", authT0); err != nil || !r.Changed {
			t.Fatalf("끄기 %+v, %v", r, err)
		}
		if f, _ := s.DeviceCredential(ctx, auth.Hash(tok2)); !f.Disabled {
			t.Fatalf("계정 끄기가 기기 조회에 안 보인다")
		}
		if r, _ := s.SetAccountDisabled(ctx, "kim", true, "boss", authT0); r.Changed {
			t.Fatalf("이미 꺼진 계정을 또 껐다고 했다")
		}
		if _, err := s.SetAccountDisabled(ctx, "boss", true, "boss", authT0); refusalCode(err) != RefuseLastAdmin {
			t.Fatalf("마지막 관리자 끄기: %v", err)
		}
		if r, err := s.SetAccountDisabled(ctx, "kim", false, "boss", authT0); err != nil || !r.Changed {
			t.Fatalf("켜기 %+v, %v", r, err)
		}
		if f, _ := s.DeviceCredential(ctx, auth.Hash(tok2)); f.Disabled {
			t.Fatalf("켜기가 안 보인다")
		}
		if _, err := s.SetAccountDisabled(ctx, "ghost", true, "boss", authT0); !errors.As(err, &nf) || nf.Kind != NFAccount {
			t.Fatalf("없는 계정 끄기: %v", err)
		}
		assertNoRawSecret(t, s, tok, tok2)
	})
}

// assertNoRawSecret 은 원장의 비밀 칸 어디에도 원값이 없는가다(지문만 남는다). 계정 표와 이벤트 기록까지 본다.
func assertNoRawSecret(t *testing.T, s *Store, secrets ...string) {
	t.Helper()
	for _, q := range []string{
		`SELECT id_hash || ' ' || coalesce(user_agent, '') FROM web_session`,
		`SELECT id || ' ' || coalesce(token_hash, '') FROM machine`,
		`SELECT coalesce(payload, '') FROM event`,
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			for _, sec := range secrets {
				if sec != "" && strings.Contains(v, sec[4:]) {
					t.Errorf("원장에 비밀 원값이 남았다(%s)", q)
				}
			}
		}
		rows.Close()
	}
}
