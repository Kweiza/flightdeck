package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/kweiza/flightdeck/internal/auth"
)

// 계정 표면 — 부트스트랩 · 기기 철회 · 계정 끄기(설계 「REST `/api/v1/` — 정본」).
//
// ★ 철회 · 끄기는 **같은 자리에서** 그 주체의 열린 스트림을 끊는다. 철회는 다음 요청부터 듣는데(게이트가 캐시 없이 원장을
// 읽는다) 열린 SSE 에는 다음 요청이 없다. 끊기는 원장 쓰기가 커밋된 **뒤**다(Hub.Cut 의 ★).

// forbidden 은 주체가 그 일을 할 수 없다는 403 이다.
func forbidden(code, msg, guidance string) Classified {
	return Classified{Status: http.StatusForbidden, Code: code, Message: msg, Guidance: guidance}
}

// noLedger 는 원장 없는 조립(시험의 대조 조립)이 계정 쓰기를 받은 것이다.
var noLedger = Classified{Status: http.StatusServiceUnavailable, Code: "no_ledger", Message: "이 서버는 계정 원장이 없다"}

// principal 은 게이트가 실은 주체다. 게이트 뒤의 핸들러에서는 언제나 있다 - 없으면 조립 결함이라 무인증으로 접지 않는다.
func principal(r *http.Request) auth.Principal {
	p, _ := auth.FromContext(r.Context())
	return p
}

// ActorOf 는 쓰기 기록에 남길 행위자다. 순수 함수다. **인증 주체에서만 고른다** - 계정이 정해진 주체(기기 토큰 · 화면 세션)는 그
// 계정(과 기기)이고, 계정이 없는 주체는 그 종류(shared · anonymous · bootstrap)다. 요청 본문의 자기 신고는 이 칸에 안 온다:
// 공유 토큰 · 무인증의 본문 actor 를 행위자로 쓰면 누구든 아무 이름이나 적어 계정 행위처럼 보이는 감사 기록을 남긴다. 자기 신고는
// 버리지 않고 따로 싣는다(service.Actor.Claimed). 주체가 빈 것은 게이트를 안 지난 조립 결함이라 빈 행위자로 접지 않는다(unknown).
// 모든 쓰기의 행위자를 주체에서 채우는 일은 뒤 카드(행위자)의 몫이다.
func ActorOf(p auth.Principal) string {
	switch {
	case p.AccountID != "" && p.MachineID != "":
		return "account:" + p.AccountID + " machine:" + p.MachineID
	case p.AccountID != "":
		return "account:" + p.AccountID
	case p.Kind != "":
		return string(p.Kind)
	}
	return "unknown"
}

// BootstrapGrant 는 부트스트랩 응답이다. LoginCode 는 여기 한 번 보이고 서버 어디에도 원값으로 안 남는다.
type BootstrapGrant struct {
	AccountID string    `json:"account_id"`
	Login     string    `json:"login"`
	LoginCode string    `json:"login_code"`
	ExpiresAt time.Time `json:"expires_at"`
	Guidance  string    `json:"guidance"`
}

// JudgeBootstrapCaller 는 이 주체가 부트스트랩을 불러도 되는가다. 순수 함수다. FD_TOKEN 만 된다 - 계정 단계는 게이트가 관리자
// 0명일 때만 KindBootstrap 을 주고, 전환 단계의 FD_TOKEN(KindShared)은 원장이 관리자 수를 다시 잰다(store.BootstrapAdmin).
// 기기 · 화면 계정은 이미 계정이 있다는 뜻이라 부를 일이 없다.
func JudgeBootstrapCaller(p auth.Principal) bool {
	return p.Kind == auth.KindBootstrap || p.Kind == auth.KindShared
}

// handleBootstrap 은 첫 관리자를 만들고 그 계정의 일회용 로그인 코드를 낸다(POST /admin/bootstrap).
//
// ★ 멱등 표를 안 탄다(JudgeSecretResponse) - 응답이 비밀이다. 재시도는 「관리자가 이미 있다」 409 를 받는다. 그때 코드를
// 잃었으면 컨테이너 안의 fd admin recover 가 새로 낸다.
func (s *server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if !JudgeBootstrapCaller(principal(r)) {
		s.writeError(w, r, forbidden("bootstrap_forbidden", "부트스트랩은 FD_TOKEN 으로만 부른다",
			"Authorization: Bearer <FD_TOKEN> 을 실어라 - 관리자가 0명일 때만 통한다."))
		return
	}
	if s.st == nil {
		s.writeError(w, r, noLedger)
		return
	}
	var req struct {
		Login       string `json:"login"`
		DisplayName string `json:"display_name" nul:"text"`
		Email       string `json:"email"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	g, err := s.st.BootstrapAdmin(r.Context(), req.Login, req.DisplayName, req.Email, s.now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, r, http.StatusCreated, BootstrapGrant{
		AccountID: g.AccountID, Login: g.Login, LoginCode: g.Code, ExpiresAt: g.ExpiresAt,
		Guidance: "이 코드를 화면 /login 에 붙여 넣어라 - 한 번만 쓰고, 다시 보이지 않는다. " +
			"그 뒤 각 기기는 fd login 으로 기기 토큰을 받는다.",
	})
}

// JudgeRevokeDevice 는 이 주체가 그 계정의 기기를 철회해도 되는가다. 순수 함수다. 관리자는 누구의 기기든, 계정은 자기 기기만.
func JudgeRevokeDevice(p auth.Principal, owner string) bool {
	return p.Admin() || (p.AccountID != "" && p.AccountID == owner)
}

// RevokeResult 는 철회 · 끄기 응답이다.
type RevokeResult struct {
	ID string `json:"id"`
	// Changed 가 거짓이면 이미 그 상태였다(원장 쓰기 0).
	Changed bool `json:"changed"`
	// StreamsCut 은 이 요청이 끊은 열린 스트림 수다.
	StreamsCut int `json:"streams_cut"`
}

// handleRevokeDevice 는 기기 토큰을 철회하고 그 기기의 열린 스트림을 끊는다(POST /api/v1/devices/{id}/revoke).
func (s *server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if s.st == nil {
		s.writeError(w, r, noLedger)
		return
	}
	res, err := s.st.RevokeMachine(r.Context(), id, func(owner string) error {
		if !JudgeRevokeDevice(p, owner) {
			return errDenied
		}
		return nil
	}, p.AccountID, s.now())
	if errors.Is(err, errDenied) {
		s.writeError(w, r, forbidden("revoke_forbidden", "남의 기기는 관리자만 철회한다", "자기 기기는 그 기기의 계정으로 철회해라."))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, RevokeResult{ID: id, Changed: res.Changed, StreamsCut: s.hub.Cut(CutMachine(id))})
}

// errDenied 는 철회 판정(JudgeRevokeDevice)이 트랜잭션을 되감게 하는 표지다. 응답은 핸들러가 403 으로 고른다.
var errDenied = errors.New("주체가 이 기기를 철회할 수 없다")

// handleSetAccountDisabled 는 계정을 끄거나 켠다(PATCH /api/v1/admin/accounts/{id}, 관리자만). 끄면 그 계정의 열린 스트림을
// 모두 끊는다. 이 판이 받는 칸은 disabled 하나다 - 역할 바꾸기 · 암호 재설정 링크는 관리자 화면 단계의 몫이다.
func (s *server) handleSetAccountDisabled(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin() {
		s.writeError(w, r, forbidden("admin_only", "계정 끄기 · 켜기는 관리자만 한다", ""))
		return
	}
	if s.st == nil {
		s.writeError(w, r, noLedger)
		return
	}
	var req struct {
		Disabled *bool `json:"disabled"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	if req.Disabled == nil {
		s.writeError(w, r, badRequest("missing_disabled", "disabled 칸이 없다", `{"disabled": true} 로 끄고 false 로 켠다.`))
		return
	}
	id := r.PathValue("id")
	res, err := s.st.SetAccountDisabled(r.Context(), id, *req.Disabled, p.AccountID, s.now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cut := 0
	if *req.Disabled {
		cut = s.hub.Cut(CutAccount(id))
	}
	s.writeJSON(w, r, http.StatusOK, RevokeResult{ID: id, Changed: res.Changed, StreamsCut: cut})
}
