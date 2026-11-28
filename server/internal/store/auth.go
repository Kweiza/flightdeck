package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kweiza/flightdeck/internal/auth"
)

// 인증 원장 - 자격증명 조회(auth.Directory)와 그것을 바꾸는 쓰기(부트스트랩 · 복구 · 로그인 코드 교환 · 로그아웃 · 기기 철회 ·
// 계정 끄기). 표는 증분 020 이 만들었다(account · web_session · machine 의 토큰 칸).
//
// ★ 원장에는 비밀의 sha256 만 들어간다(auth.Hash). 원값은 만든 함수가 돌려주는 값 한 번뿐이고, 이벤트 · 오류 · 로그에는 실리지
// 않는다 - 이 파일의 이벤트 payload 는 계정 id · 로그인 이름 · 기기 id 만 싣는다.
//
// ★ 일회용 로그인 코드(auth.LoginCode)는 web_session 의 행이다: 아직 브라우저에 건네지 않은 세션. 교환(ExchangeLoginCode)이 그
// 행을 지우고 새 세션 행을 만든다. 쿠키 판정은 `fds_` 모양만 받으므로 코드 행은 쿠키로 못 쓴다 - 지문은 원값의 sha256 이고
// 접두가 원값에 들어 있어 `fds_` 값의 지문이 코드 행과 같을 수 없다. 표를 새로 두지 않은 이유: 코드가 실은 사실(누구의,
// 언제까지)이 세션 행의 칸과 같고, 쓰이면 곧 세션이 된다. 행의 user_agent 칸이 「무엇이 냈나」(loginCodeAgent…)를 적는다.

// AccountRefusedError 는 계정 쓰기를 원장 사실 때문에 거절한 것이다(409). Code 는 기계가 읽는 사유, Reason 은 사람이 읽는 문장이다.
type AccountRefusedError struct {
	Code   string
	Reason string
}

func (e *AccountRefusedError) Error() string { return e.Reason }

func refused(code, reason string) error { return &AccountRefusedError{Code: code, Reason: reason} }

// 거절 사유(AccountRefusedError.Code).
const (
	RefuseBootstrapClosed = "bootstrap_closed"
	RefuseBadLogin        = "bad_login"
	RefuseLoginTaken      = "login_taken"
	RefuseNotAdmin        = "not_admin"
	RefuseLastAdmin       = "last_admin"
	RefuseNoDeviceToken   = "no_device_token"
)

// 로그인 코드 행의 user_agent - 무엇이 그 코드를 냈나.
const (
	loginCodeAgentBootstrap = "login-code: admin bootstrap"
	loginCodeAgentRecover   = "login-code: admin recover"
)

// userAgentMax 는 세션 행에 남기는 User-Agent 의 최대 글자 수다.
const userAgentMax = 200

// ─────────────────────────────────────────────────────────────────────────────
// 조회 - auth.Directory
// ─────────────────────────────────────────────────────────────────────────────

var _ auth.Directory = (*Store)(nil)

// DeviceCredential 은 기기 토큰 지문의 행이다. 철회 · 계정 끄기 여부를 함께 싣는다 - 판정은 auth 가 한다.
func (s *Store) DeviceCredential(ctx context.Context, tokenHash string) (auth.DeviceFact, error) {
	var f auth.DeviceFact
	var revoked, disabled sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT m.id, m.account_id, a.role, m.token_hash, m.revoked_at, a.disabled_at
		FROM machine m JOIN account a ON a.id = m.account_id
		WHERE m.token_hash = ?`, tokenHash).
		Scan(&f.MachineID, &f.AccountID, &f.Role, &f.TokenHash, &revoked, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return f, auth.ErrUnknown
	}
	if err != nil {
		return f, fmt.Errorf("기기 토큰 조회 실패: %w", err)
	}
	f.Revoked, f.Disabled = revoked.Valid, disabled.Valid
	return f, nil
}

// WebSessionCredential 은 세션 쿠키 지문의 행이다.
func (s *Store) WebSessionCredential(ctx context.Context, idHash string) (auth.SessionFact, error) {
	var f auth.SessionFact
	var exp string
	var disabled sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT w.id_hash, w.account_id, a.role, w.expires_at, a.disabled_at
		FROM web_session w JOIN account a ON a.id = w.account_id
		WHERE w.id_hash = ?`, idHash).
		Scan(&f.IDHash, &f.AccountID, &f.Role, &exp, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return f, auth.ErrUnknown
	}
	if err != nil {
		return f, fmt.Errorf("로그인 세션 조회 실패: %w", err)
	}
	if f.ExpiresAt, err = parseTime(exp); err != nil {
		return f, err
	}
	f.Disabled = disabled.Valid
	return f, nil
}

// AdminCount 는 관리자 계정 수다. **꺼진 관리자도 센다** - 꺼진 것만 남았다고 0 으로 세면 FD_TOKEN 을 쥔 누구나 그 틈에
// 새 관리자를 만든다. 관리자를 모두 잃은 경우의 길은 컨테이너 안의 RecoverAdmin 이다.
func (s *Store) AdminCount(ctx context.Context) (int, error) {
	return adminCount(ctx, s.db)
}

func adminCount(ctx context.Context, q Reader) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM account WHERE role = ?`, auth.RoleAdmin).Scan(&n); err != nil {
		return 0, fmt.Errorf("관리자 수 재기 실패: %w", err)
	}
	return n, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 로그인 코드 - 부트스트랩 · 복구가 내고, 화면 로그인이 바꾼다
// ─────────────────────────────────────────────────────────────────────────────

// LoginCodeGrant 는 새로 낸 로그인 코드다. Code 는 원값이고 여기 한 번뿐이다.
type LoginCodeGrant struct {
	AccountID, Login string
	Code             string
	ExpiresAt        time.Time
	// Reenabled 는 복구가 꺼진 계정을 다시 켰는가다.
	Reenabled bool
}

// insertLoginCode 는 계정 하나에 로그인 코드 행을 넣고 원값을 돌려준다.
func (t *Tx) insertLoginCode(accountID, agent string, now time.Time) (string, time.Time, error) {
	code, err := auth.NewToken(auth.LoginCode)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := now.Add(auth.LoginCodeTTL)
	if err := t.insertWebSession(auth.Hash(code), accountID, agent, now, exp); err != nil {
		return "", time.Time{}, fmt.Errorf("로그인 코드 기록 실패: %w", err)
	}
	return code, exp, nil
}

// insertWebSession 은 web_session 행 하나다 - 화면 세션과 로그인 코드가 같은 꼴로 넣는다.
func (t *Tx) insertWebSession(idHash, accountID, agent string, now, expires time.Time) error {
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO web_session(id_hash, account_id, created_at, expires_at, last_seen_at, user_agent)
		VALUES (?, ?, ?, ?, ?, ?)`,
		idHash, accountID, fmtTime(now), fmtTime(expires), fmtTime(now), nullStr(agent))
	return err
}

// BootstrapAdmin 은 관리자가 0명인 원장에 첫 관리자를 만들고 그 계정의 로그인 코드를 낸다. 관리자가 하나라도 있으면
// 거절한다(RefuseBootstrapClosed) - 세기와 넣기가 한 트랜잭션(전역 쓰기 잠금) 안이라 동시에 두 번 불러도 하나만 선다.
func (s *Store) BootstrapAdmin(ctx context.Context, login, displayName, email string, now time.Time) (LoginCodeGrant, error) {
	login = strings.TrimSpace(login)
	if err := auth.JudgeLogin(login); err != nil {
		return LoginCodeGrant{}, refused(RefuseBadLogin, err.Error())
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = login
	}
	now = atStamp(now)
	var g LoginCodeGrant
	err := s.Tx(ctx, func(t *Tx) error {
		n, err := adminCount(t.ctx, t.tx)
		if err != nil {
			return err
		}
		if n > 0 {
			return refused(RefuseBootstrapClosed, "관리자가 이미 있다 - 부트스트랩은 관리자가 0명일 때 한 번뿐이다. "+auth.RecoverHint)
		}
		var taken int
		if err := t.tx.QueryRowContext(t.ctx, `SELECT count(*) FROM account WHERE login = ?`, login).Scan(&taken); err != nil {
			return fmt.Errorf("로그인 이름 확인 실패: %w", err)
		}
		if taken > 0 {
			return refused(RefuseLoginTaken, fmt.Sprintf("로그인 이름 %q 는 이미 있다(관리자가 아닌 계정)", login))
		}
		id := NewID()
		if _, err := t.tx.ExecContext(t.ctx, `
			INSERT INTO account(id, login, display_name, email, role, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			id, login, displayName, nullStr(strings.TrimSpace(email)), auth.RoleAdmin, fmtTime(now)); err != nil {
			return fmt.Errorf("첫 관리자 기록 실패: %w", err)
		}
		code, exp, err := t.insertLoginCode(id, loginCodeAgentBootstrap, now)
		if err != nil {
			return err
		}
		g = LoginCodeGrant{AccountID: id, Login: login, Code: code, ExpiresAt: exp}
		t.LogEvent("account.bootstrap", "", "", map[string]any{"account_id": id, "login": login})
		return nil
	})
	return g, err
}

// RecoverAdmin 은 관리자 계정 하나에 로그인 코드를 새로 낸다. 꺼져 있으면 다시 켠다(그 사실을 돌려준다). 관리자가 아닌 계정은
// 거절한다 - 이 길은 「관리자를 모두 잃었을 때」의 것이고, 일반 계정의 암호 재설정은 관리자 화면의 일이다.
//
// ★ HTTP 에 이 길이 없다. 원장에 직접 붙는 사람(서버 컨테이너 안의 fd admin recover)만 부른다 - HTTP 로 열면 FD_TOKEN 하나로
// 관리자의 로그인 코드를 만들 수 있어 사실상 전권이 된다.
func (s *Store) RecoverAdmin(ctx context.Context, login string, now time.Time) (LoginCodeGrant, error) {
	login = strings.TrimSpace(login)
	now = atStamp(now)
	var g LoginCodeGrant
	err := s.Tx(ctx, func(t *Tx) error {
		var id, role string
		var disabled sql.NullString
		err := t.tx.QueryRowContext(t.ctx, `SELECT id, role, disabled_at FROM account WHERE login = ?`, login).
			Scan(&id, &role, &disabled)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound(NFAccount, "", login)
		}
		if err != nil {
			return fmt.Errorf("계정 조회 실패(login=%q): %w", clip(login, 64), err)
		}
		if role != auth.RoleAdmin {
			return refused(RefuseNotAdmin, fmt.Sprintf("%q 는 관리자가 아니다 - 복구는 관리자 계정만 한다", clip(login, 64)))
		}
		if disabled.Valid {
			if _, err := t.tx.ExecContext(t.ctx, `UPDATE account SET disabled_at = ? WHERE id = ?`, nil, id); err != nil {
				return fmt.Errorf("계정 다시 켜기 실패: %w", err)
			}
		}
		code, exp, err := t.insertLoginCode(id, loginCodeAgentRecover, now)
		if err != nil {
			return err
		}
		g = LoginCodeGrant{AccountID: id, Login: login, Code: code, ExpiresAt: exp, Reenabled: disabled.Valid}
		t.LogEvent("account.recover", "", "", map[string]any{"account_id": id, "login": login, "reenabled": disabled.Valid})
		return nil
	})
	return g, err
}

// WebLogin 은 로그인 코드를 바꿔 낸 화면 세션이다. Cookie 는 원값이고 여기 한 번뿐이다.
type WebLogin struct {
	AccountID string
	Cookie    string
	IDHash    string
	ExpiresAt time.Time
}

// ErrLoginCodeRefused 는 로그인 코드를 못 바꿨다는 것이다. 없음 · 만료 · 이미 씀 · 꺼진 계정을 가르지 않는다 - 가르면 코드
// 추측이 「있는 코드」를 알아낸다. 사람에게는 「새 코드를 받아라」 하나로 처방이 같다.
var ErrLoginCodeRefused = errors.New("로그인 코드가 맞지 않거나 만료됐다(한 번만 쓴다) - 관리자에게 새 코드를 받아라")

// ExchangeLoginCode 는 로그인 코드를 한 번 바꿔 새 화면 세션을 연다. 코드 행은 지운다(일회용). 지우기와 새 행 넣기가 한
// 트랜잭션이라 같은 코드로 동시에 두 번 불러도 하나만 선다.
func (s *Store) ExchangeLoginCode(ctx context.Context, code, userAgent string, now time.Time) (WebLogin, error) {
	if k, ok := auth.KindOf(code); !ok || k != auth.LoginCode {
		return WebLogin{}, ErrLoginCodeRefused
	}
	now = atStamp(now)
	var out WebLogin
	err := s.Tx(ctx, func(t *Tx) error {
		var account, exp string
		var disabled sql.NullString
		err := t.tx.QueryRowContext(t.ctx, `
			SELECT w.account_id, w.expires_at, a.disabled_at
			FROM web_session w JOIN account a ON a.id = w.account_id
			WHERE w.id_hash = ?`, auth.Hash(code)).Scan(&account, &exp, &disabled)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLoginCodeRefused
		}
		if err != nil {
			return fmt.Errorf("로그인 코드 조회 실패: %w", err)
		}
		expires, err := parseTime(exp)
		if err != nil {
			return err
		}
		if !now.Before(expires) || disabled.Valid {
			return ErrLoginCodeRefused
		}
		res, err := t.tx.ExecContext(t.ctx, `DELETE FROM web_session WHERE id_hash = ?`, auth.Hash(code))
		if err != nil {
			return fmt.Errorf("로그인 코드 지우기 실패: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return ErrLoginCodeRefused
		}
		cookie, err := auth.NewToken(auth.SessionToken)
		if err != nil {
			return err
		}
		out = WebLogin{AccountID: account, Cookie: cookie, IDHash: auth.Hash(cookie), ExpiresAt: now.Add(auth.SessionTTL)}
		if err := t.insertWebSession(out.IDHash, account, shownAgent(userAgent), now, out.ExpiresAt); err != nil {
			return fmt.Errorf("화면 세션 기록 실패: %w", err)
		}
		t.LogEvent("web_session.login", "", "", map[string]any{"account_id": account, "via": "login-code"})
		return nil
	})
	if err != nil {
		return WebLogin{}, err
	}
	return out, nil
}

// shownAgent 는 User-Agent 를 원장 칸에 맞게 다듬는다(깨진 UTF-8 · 제어 문자를 걷고 자른다).
func shownAgent(ua string) string {
	ua = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToValidUTF8(ua, ""))
	return clip(strings.TrimSpace(ua), userAgentMax)
}

// EndWebSession 은 화면 세션 하나를 지운다(로그아웃). 계정 id 와 지웠는가를 돌려준다 - 지운 뒤 그 세션의 열린 스트림을 끊는
// 것은 호출자(api)의 몫이다.
func (s *Store) EndWebSession(ctx context.Context, idHash string) (accountID string, ended bool, err error) {
	err = s.Tx(ctx, func(t *Tx) error {
		err := t.tx.QueryRowContext(t.ctx, `SELECT account_id FROM web_session WHERE id_hash = ?`, idHash).Scan(&accountID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("화면 세션 조회 실패: %w", err)
		}
		if _, err := t.tx.ExecContext(t.ctx, `DELETE FROM web_session WHERE id_hash = ?`, idHash); err != nil {
			return fmt.Errorf("화면 세션 지우기 실패: %w", err)
		}
		ended = true
		t.LogEvent("web_session.logout", "", "", map[string]any{"account_id": accountID})
		return nil
	})
	return accountID, ended && err == nil, err
}

// ─────────────────────────────────────────────────────────────────────────────
// 철회 · 끄기
// ─────────────────────────────────────────────────────────────────────────────

// Revocation 은 철회 · 끄기 한 번의 결과다. Changed 가 거짓이면 이미 그 상태였다(쓰기 0).
type Revocation struct {
	AccountID string
	Changed   bool
}

// RevokeMachine 은 기기 토큰을 철회한다. permit 은 그 기기의 계정 id 를 받아 이 주체가 철회해도 되는지 판정한다(트랜잭션 안,
// 읽은 값으로, 없는 기기면 빈 글). by 는 철회한 계정(없으면 빈 글 - FD_TOKEN 전환 단계)이다. 토큰이 없는 기기(훅이 등록한 옛 머신)는 거절한다.
func (s *Store) RevokeMachine(ctx context.Context, machineID string, permit func(accountID string) error, by string, now time.Time) (Revocation, error) {
	now = atStamp(now)
	var r Revocation
	err := s.Tx(ctx, func(t *Tx) error {
		var account, token, revoked sql.NullString
		err := t.tx.QueryRowContext(t.ctx, `SELECT account_id, token_hash, revoked_at FROM machine WHERE id = ?`, machineID).
			Scan(&account, &token, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			// ★ 없는 기기도 permit 을 먼저 지난다(주인 없음 = 빈 글). 404 를 먼저 내면 남의 기기를 철회할 수 없는 계정이
			// 기기 id 가 있는지 없는지를 캐 볼 수 있다.
			if err := permit(""); err != nil {
				return err
			}
			return notFound(NFMachine, "", machineID)
		}
		if err != nil {
			return fmt.Errorf("기기 조회 실패(id=%q): %w", clip(machineID, 64), err)
		}
		if err := permit(account.String); err != nil {
			return err
		}
		r.AccountID = account.String
		if !token.Valid {
			return refused(RefuseNoDeviceToken, "이 기기에는 기기 토큰이 없다 - 철회할 것이 없다(FD_TOKEN 으로 붙는 기기는 FD_TOKEN 을 바꿔 끊는다)")
		}
		if revoked.Valid {
			return nil
		}
		if _, err := t.tx.ExecContext(t.ctx, `UPDATE machine SET revoked_at = ?, revoked_by = ? WHERE id = ?`,
			fmtTime(now), nullStr(by), machineID); err != nil {
			return fmt.Errorf("기기 철회 실패: %w", err)
		}
		r.Changed = true
		t.LogEvent("machine.revoke", "", "", map[string]any{"machine_id": machineID, "account_id": account.String, "by": by})
		return nil
	})
	return r, err
}

// SetAccountDisabled 는 계정을 끄거나 켠다. 마지막으로 켜진 관리자를 끄는 것은 거절한다(RefuseLastAdmin) - 끄면 화면으로
// 관리할 사람이 없어진다. by 는 바꾼 계정이다.
func (s *Store) SetAccountDisabled(ctx context.Context, accountID string, disable bool, by string, now time.Time) (Revocation, error) {
	now = atStamp(now)
	r := Revocation{AccountID: accountID}
	err := s.Tx(ctx, func(t *Tx) error {
		var role string
		var disabled sql.NullString
		err := t.tx.QueryRowContext(t.ctx, `SELECT role, disabled_at FROM account WHERE id = ?`, accountID).Scan(&role, &disabled)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound(NFAccount, "", accountID)
		}
		if err != nil {
			return fmt.Errorf("계정 조회 실패(id=%q): %w", clip(accountID, 64), err)
		}
		if disabled.Valid == disable {
			return nil
		}
		var at any
		kind := "account.enable"
		if disable {
			if role == auth.RoleAdmin {
				var live int
				if err := t.tx.QueryRowContext(t.ctx,
					`SELECT count(*) FROM account WHERE role = ? AND disabled_at IS NULL`, auth.RoleAdmin).Scan(&live); err != nil {
					return fmt.Errorf("켜진 관리자 재기 실패: %w", err)
				}
				if live <= 1 {
					return refused(RefuseLastAdmin, "마지막으로 켜진 관리자는 못 끈다 - 다른 관리자를 먼저 세워라")
				}
			}
			at, kind = fmtTime(now), "account.disable"
		}
		if _, err := t.tx.ExecContext(t.ctx, `UPDATE account SET disabled_at = ? WHERE id = ?`, at, accountID); err != nil {
			return fmt.Errorf("계정 상태 바꾸기 실패: %w", err)
		}
		r.Changed = true
		t.LogEvent(kind, "", "", map[string]any{"account_id": accountID, "by": by})
		return nil
	})
	return r, err
}
