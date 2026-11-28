package api

import (
	"io"
	"net/http"
	"strings"
	"time"

	"context"
	"errors"
	"github.com/kweiza/flightdeck/internal/auth"
	"github.com/kweiza/flightdeck/internal/buildinfo"
	"github.com/kweiza/flightdeck/internal/service"
	"github.com/kweiza/flightdeck/internal/store"
)

// 화면·알림·진단 표면.

// AuthStatus 는 /healthz 가 알리는 인증 상태다.
//
// ★ **조용한 무인증은 안 된다.** 토큰이 설정되지 않은 전환 단계 서버는 자격증명 없는 요청을 통과시킨다 - 그 사실이 여기
// 없으면 운영자는 서버가 열려 있다는 것을 알 방법이 없다. 루프백 면제는 없어졌다(두 단계 모두) - 그래서 루프백 설정 · 도달
// 관측 칸도 없다.
type AuthStatus struct {
	TokenSet bool   `json:"token_set"`
	Notice   string `json:"notice"`
}

// HealthzBody 는 /healthz 응답이다.
//
// ★ service.Health 를 그대로 내지 않는 **유일한** 자리다. 그 타입은 db_path 와
// 하류 오류 전문을 들고 있는데 이 표면은 인증 앞에 있다(배너 훅이 토큰 오설정
// 상황에서도 물어야 하므로). 서버의 파일 경로가 무인증으로 새는 것은
// "오류 응답에 내부 이름을 넣지 마라"와 같은 규율이고, 그래서 여기서 걷어낸다.
type HealthzBody struct {
	OK         bool   `json:"ok"`
	APIVersion string `json:"api_version"`
	DBOK       bool   `json:"db_ok"`
	DBError    string `json:"db_error,omitempty"`
	// DBLocaleOK 는 원장 DB 의 로케일이 기동 검사와 같은 관문을 지나는가다(service.Health 의 같은 칸).
	DBLocaleOK    bool      `json:"db_locale_ok"`
	DBLocaleError string    `json:"db_locale_error,omitempty"`
	DiskFreePct   float64   `json:"disk_free_pct"`
	DiskKnown     bool      `json:"disk_known"`
	DiskError     string    `json:"disk_error,omitempty"`
	At            time.Time `json:"at"`
	// AuthMode 는 인증 단계다(FD_AUTH: transition | accounts).
	AuthMode string `json:"auth_mode"`
	// TLS 는 전송 정책이다(FD_TLS: off | files | proxy).
	TLS  string     `json:"tls"`
	Auth AuthStatus `json:"auth"`
	// Build 는 **응답하는 이 프로세스**의 빌드 좌표다. 파일의 것이 아니다.
	//
	// ★ api_version 으로는 판 나이를 못 나른다 — 그 값은 계약이 깨질 때만 오르므로
	// 판이 수십 커밋 벌어져도 "1" == "1" 이다. 그 구간에서 응답의 축 하나가 조용히
	// 사라지는 것이 실제로 관측됐다(fd-binary-vintage-has-no-signal).
	//
	// 좌표 누출이 아니다 — sha 는 이 저장소를 읽을 수 있는 사람에게만 의미가 있고,
	// db_path 처럼 서버의 파일 배치를 알리지 않는다.
	Build buildinfo.Coord `json:"build"`
	// SelfUpdate 는 이 서버가 자기 판을 따라가고 있는가다.
	SelfUpdate SelfUpdateStatus `json:"self_update"`
	// LedgerBackup 은 매시간 판단 원장 백업이 실제로 돌고 있는가다.
	LedgerBackup LedgerBackupStatus `json:"ledger_backup"`
}

// LedgerBackupStatus 는 판단 원장 주기 백업의 상태다.
//
// ★ **Running 이 먼저다.** SelfUpdateStatus.Watching·buildinfo.Coord.Known 과 같은 규율이다 —
// 잡이 안 떴는데 나머지가 비어 있으면 "아직 회차가 없었다"로 읽히고, 그것은 "안 돌고 있다"와
// 전혀 다르다. 이 축이 존재하는 이유가 정확히 그 둘을 가르는 것이다.
//
// ★ 이 축이 왜 필요했나. 잡을 세우면서 위험이 **옮겨갔다**: "손으로 부르는 것을 아무도
// 안 부른다" 에서 "도는 줄 알았는데 조용히 실패한다" 로. 로그만 남기면 `docker logs` 를
// 뒤지기 전까지 아무도 모르고, 그동안 설계 §7 은 "매시간 자동 실행: 있음"이라고 말한다.
//
// ★ **성공도 여기 남는다.** 자동 갱신과 다른 점이다 — 그쪽은 성공하면 프로세스가 갈아치워져
// 새 프로세스가 그 사실을 모르지만, 이 잡은 성공해도 같은 프로세스가 계속 돈다.
// 그래서 "마지막 회차가 언제 · 무엇을 했나"가 이 축의 본체다.
type LedgerBackupStatus struct {
	Running bool   `json:"running"`
	Reason  string `json:"reason,omitempty"`
	// LastAt 은 마지막 회차 시각이다. **nil 은 "아직 한 회차도 안 돌았다"** 이고,
	// 제로값과 접으면 1970년에 백업한 서버처럼 보인다(selfUpdateStatusOf 가 같은 갈래를 가른다).
	LastAt *time.Time `json:"last_at,omitempty"`
	// Outcome 은 wrote | unchanged | failed 다.
	//
	// ★ unchanged 를 failed 와 안 접는다. 안 바뀐 회차는 **정상이고 흔하다**(원장이
	// 안 바뀌면 안 쓴다) — 그것을 실패로 접으면 화면이 정상 운영을 사고로 보고한다.
	Outcome string `json:"outcome,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// Route 는 산출물 자리다. 이 값이 없으면 "돌긴 도는데 어디에 쌓이는지 모른다"가 된다.
	Route string `json:"route,omitempty"`
	// Journal 은 세대가 bare 레포에 쌓였는가다: committed | unchanged | failed: <사유>.
	//
	// ★ **백업 결과와 따로 낸다.** 저널이 실패해도 JSONL 은 착지했고 그것이 복원의 정본이다 —
	// 한 축으로 접으면 "역사는 못 쌓았지만 원장은 성했다"가 "백업이 실패했다"로 읽힌다.
	Journal string `json:"journal,omitempty"`
}

// SelfUpdateStatus 는 서버의 자동 갱신 축이다.
//
// ★ **Watching 이 먼저다.** 감시기가 안 떴는데 나머지가 비어 있으면
// "아직 갱신이 없었다"로 읽힌다 — 그것은 "안 보고 있다"와 전혀 다르다.
// buildinfo.Coord 의 Known 이 같은 규율이다.
//
// **성공은 여기 안 남는다.** 성공하면 프로세스가 갈아치워져 새 프로세스는 그 사실을 모른다.
type SelfUpdateStatus struct {
	Watching bool       `json:"watching"`
	Reason   string     `json:"reason,omitempty"`
	LastAt   *time.Time `json:"last_at,omitempty"`
	From     string     `json:"from,omitempty"`
	To       string     `json:"to,omitempty"`
	Outcome  string     `json:"outcome,omitempty"` // refused | failed
	Detail   string     `json:"detail,omitempty"`

	// ★ Stalled 는 **보고는 있는데 지금 실행 파일을 못 잰다**는 사실이다.
	// Watching=true 인데 이것이 차 있으면 "따라가는 중"이 아니라 "눈이 멀었다"다 —
	// 그 둘을 안 가르면 지워진 바이너리를 감시하는 서버가 화면에서는 정상으로 보인다.
	Stalled string `json:"stalled,omitempty"`

	// ★ Uncovered 는 보고 있는데도 **구조적으로 못 덮는 갈래**다 — 감시하는 자리의 이름에
	// 소스 트리가 박혀 있어, 플러그인 버전이 오르는 갱신은 다른 이름의 파일을 짓고 이 자리는
	// 아무도 안 덮는다. 그 서버는 영원히 "그대로다"를 보며 옛 코드로 산다.
	//
	// **Stalled 와 한 필드로 접지 않는다.** Stalled 는 회복되는 고장이고 이것은 회복이 없는
	// 성질이다 — 처방이 "왜 못 재는지 고쳐라" 대 "이 갱신은 사람이 재기동해야 한다"로 갈린다.
	// 판정의 주인은 cmd/fd 의 selfUpdateStatus.Uncovered 이고 이 표면은 그것을 옮기기만 한다.
	Uncovered string `json:"uncovered,omitempty"`
}

// AuthNotice 는 지금 인증 상태를 사람이 읽을 한 줄로 만든다. 순수 함수다.
//
// ★ 전환 단계는 다음 걸음을 같이 준다(FD_AUTH=accounts 로 좁히기). 그 걸음을 안 적으면 「FD_TOKEN 이 어디서나 통한다」가 정상
// 상태로 읽혀 좁히는 날이 안 온다.
func AuthNotice(tokenSet bool, mode auth.Mode, tls auth.TLSMode) string {
	var n string
	switch {
	case mode == auth.ModeAccounts:
		n = "계정 단계다(FD_AUTH=accounts) - 기기 토큰 · 화면 로그인만 받는다. FD_TOKEN 은 관리자가 0명일 때의 /admin/bootstrap 에만 통한다"
	case !tokenSet:
		n = "토큰이 설정되지 않았다 - 전환 단계라 자격증명이 없는 요청을 무인증으로 통과시킨다"
	default:
		n = "전환 단계다(FD_AUTH=transition) - FD_TOKEN 과 기기 토큰 · 화면 로그인을 함께 받는다. " +
			"기기마다 fd login 을 마치면 FD_AUTH=accounts 로 좁혀라"
	}
	if tls == auth.TLSOff {
		n += ". 전송은 평문이다(FD_TLS 꺼짐) - 루프백 밖에 묶었다면 토큰이 망에 평문으로 실린다"
	}
	return n
}

// UnauthorizedGuidance 는 401 을 받은 사람에게 **무엇을 하면 되는지**를 낸다. 순수 함수다. 루프백 면제는 없다 - 어느
// 단계든 루프백도 자격증명이 필요하다는 것을 함께 적는다(옛 판의 면제를 믿고 토큰 없이 치는 도구가 원인을 찾게).
func UnauthorizedGuidance(mode auth.Mode) string {
	if mode == auth.ModeAccounts {
		return "Authorization: Bearer <기기 토큰> 을 실어라(fd login 이 받는다). 화면은 로그인 코드로 로그인한다. " +
			"FD_TOKEN 은 부트스트랩 전용이다. 루프백도 같다."
	}
	return "Authorization: Bearer <기기 토큰>(fd login) 이나 FD_TOKEN 을 실어라 - 전환 단계라 둘 다 통한다. 루프백도 같다."
}

// dbErrorRemote 는 Postgres 원장에 붙지 못했을 때 /healthz 가 내는 안내다. 서버 로그에 실제로 남는 것(종류 · SQLSTATE)을
// 가리킨다(HealthzOf 의 ★).
const dbErrorRemote = "DB 에 접속할 수 없다 - 서버 로그에 종류 · SQLSTATE 가 있다(드라이버 문구는 접속 문자열의 조각을 품어 남기지 않는다)"

// HealthzOf 는 내부 상태를 무인증으로 내보내도 되는 형태로 옮긴다. 순수 함수다.
//
// 오류 **전문**을 고정 문구로 갈아 끼운다. 그 문자열에는 DB 파일 경로와
// 드라이버 이름이 섞여 들어오고, 그것을 그대로 내면 걷어낸 db_path 가
// 다른 필드로 되살아난다 — 실제로 그 모양의 누출이 흔하다.
// ★ build 를 **인자로 받는다.** service.Health 에 필드를 더하지 않는 이유가 둘이다 —
// 빌드 좌표는 DB·디스크 상태가 아니라 프로세스의 성질이고, 인자로 받으면 이 함수가
// tokenSet·mode 와 같은 모양으로 순수하게 남아 시험이 값을 직접 준다.
// su 도 같은 이유로 인자다 — 핸들러가 body 에 따로 얹으면 순수 함수가 불완전한 body 를
// 만들고 그 시험은 통과한다. 실제 응답과 갈리는 그 모양을 이 저장소가 반복해서 문제 삼았다.
func HealthzOf(h service.Health, tokenSet bool, mode auth.Mode, tls auth.TLSMode, build buildinfo.Coord,
	su SelfUpdateStatus, lb LedgerBackupStatus) HealthzBody {
	b := HealthzBody{
		OK: h.OK, APIVersion: h.APIVersion, DBOK: h.DBOK, DBLocaleOK: h.DBLocaleOK, Build: build,
		SelfUpdate: su, LedgerBackup: lb,
		DiskFreePct: h.DiskFreePct, DiskKnown: h.DiskKnown, At: h.At,
		AuthMode: string(mode), TLS: string(tls),
		Auth: AuthStatus{TokenSet: tokenSet, Notice: AuthNotice(tokenSet, mode, tls)},
	}
	// ★ Postgres 면 서버 로그에도 원인 전문이 없다 - store 가 드라이버 문구(접속 문자열의 조각 · 원격 주소를 품는다)를
	//   걷고 종류 · SQLSTATE 만 남긴다. 「전문은 로그에」라고 하면 없는 것을 찾으러 간다.
	switch {
	case h.DBError == "":
	case h.DBRemote:
		b.DBError = dbErrorRemote
	default:
		b.DBError = "DB 에 접속할 수 없다 — 원인 전문은 서버 로그에 있다"
	}
	// ★ 로케일 사유는 고정 문구로 낸다. 거절 사유 자체(제공자 · 로케일 이름)는 비밀이 아니지만, 못 잰 갈래는 하류 오류
	//   본문이라 db_error 와 같은 규율을 따른다. 걸린 축 전부는 서버 로그와 인증 뒤 헬스(fd doctor)에 있다.
	switch {
	case h.DBLocaleError == "":
	case h.DBLocaleError == service.DBLocaleUnmeasured:
		b.DBLocaleError = h.DBLocaleError
	default:
		b.DBLocaleError = "DB 로케일이 관문을 못 지났거나 재지 못했다 - 걸린 축은 서버 로그에 있다"
	}
	// ★ 원격 DB 라 재지 않은 것은 측정 실패가 아니다 - 그 갈래는 서버 로그도 안 남기므로 「로그를 보라」는 헛걸음이다.
	//   그 사유는 고정 문구라 경로가 없고, 그대로 낸다.
	switch h.DiskError {
	case "":
	case service.DiskRemote:
		b.DiskError = h.DiskError
	default:
		b.DiskError = "디스크 여유를 재지 못했다 — 원인 전문은 서버 로그에 있다"
	}
	return b
}

// handleHealthz 는 서버 상태와 **인증 설정**을 알린다. 인증 게이트 앞에 있다.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	h := s.svc.Health(r.Context())
	// buildinfo.Self 는 파일이 아니라 **이 프로세스**를 읽는다. 실측에서 설치 파일은 이미
	// 최신으로 교체돼 있는데 프로세스는 지워진 아이노드의 옛 코드를 돌고 있었다 —
	// 파일을 재는 진단은 그 상황에서 "최신"이라 답한다.
	//
	// s.opt.SelfUpdate 가 nil 인 것은 정상 갈래다(시험 조립·다른 진입점이 안 줄 수 있다).
	// 그때 빈 구조체를 내면 "아직 갱신이 없었다"로 읽히는데 그것은 "배선이 안 됐다"와
	// 전혀 다르다 — 그래서 사유를 채운 값으로 대신한다.
	su := SelfUpdateStatus{Watching: false, Reason: "이 서버는 자동 갱신 축을 배선하지 않았다"}
	if s.opt.SelfUpdate != nil {
		su = s.opt.SelfUpdate()
	}
	// 같은 규율 — nil 이면 빈 구조체가 아니라 **사유를 채운 값**을 낸다.
	// 빈 값은 "아직 회차가 없었다"로 읽히는데 그것은 "배선이 안 됐다"와 전혀 다르다.
	lb := LedgerBackupStatus{Running: false, Reason: "이 서버는 판단 원장 백업 축을 배선하지 않았다"}
	if s.opt.LedgerBackup != nil {
		lb = s.opt.LedgerBackup()
	}
	body := HealthzOf(h, s.opt.Token != "", s.opt.AuthMode, s.opt.TLS.Mode, buildinfo.Self(), su, lb)
	status := http.StatusOK
	if !body.OK {
		status = http.StatusServiceUnavailable
	}
	s.writeJSON(w, r, status, body)
}

// handleMetrics 는 프로메테우스 텍스트 포맷을 직접 쓴다(의존성 0).
func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap := s.met.snapshot(s.hub.Count(), s.hub.Dropped())
	// 파생 비용은 service 계층이 센다(비싼 일이 거기 있으므로). 표면은 옮기기만 한다.
	// svc 가 nil 인 조립이 시험에 있으므로 그 축은 0 으로 남긴다 — 0 과 "안 잰다"를
	// 여기서 가르지 않는 이유는, 그 조립에는 파생을 돌릴 경로 자체가 없어서다.
	if s.svc != nil {
		d := s.svc.DeriveStats()
		snap.DeriveRuns, snap.DeriveCards, snap.DeriveSeconds = d.Runs, d.Cards, d.Seconds
		if st := s.svc.Store(); st != nil {
			snap.DB = st.DBStats()
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, RenderMetrics(snap)); err != nil {
		s.log.WarnContext(r.Context(), "지표 응답 쓰기 실패", "error", err.Error())
	}
}

// handleDashboard 는 화면 한 장분의 값을 낸다.
//
// 읽기 전용이다. 파생물에 손을 대는 표면을 열면 대시보드가 다시 손 기재 저장소가 되고,
// 그 순간 이 제품이 없애려던 병목 1위가 부활한다(설계 §6).
func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	project, ok := s.requireQuery(w, r, "project", "어느 프로젝트의 화면인지 없이는 좌표가 없다.")
	if !ok {
		return
	}
	window, err := queryDuration(r, "window", 0)
	if err != nil {
		s.writeError(w, r, badRequest("bad_window", "window 가 구간 표기가 아니다", "예: window=8h"))
		return
	}
	queue, err := queryBool(r, "queue", true)
	if err != nil {
		s.writeError(w, r, badRequest("bad_queue", "queue 가 불리언이 아니다", "예: queue=false"))
		return
	}
	notes, err := queryBool(r, "notes", true)
	if err != nil {
		s.writeError(w, r, badRequest("bad_notes", "notes 가 불리언이 아니다", "예: notes=false"))
		return
	}
	limit, err := queryInt(r, "note_limit", 0)
	if err != nil {
		s.writeError(w, r, badRequest("bad_note_limit", "note_limit 이 정수가 아니다", "예: note_limit=20"))
		return
	}
	// ★ 기본값 false 다. 이 축을 켜면 응답이 멤버 수만큼 길어지고, 그 화면은 루트
	//   세션이 **일부러** 물었을 때만 뜻이 있다(service.BoardOptions.Workspace).
	workspace, err := queryBool(r, "workspace", false)
	if err != nil {
		s.writeError(w, r, badRequest("bad_workspace", "workspace 가 불리언이 아니다", "예: workspace=true"))
		return
	}
	self := strings.TrimSpace(r.URL.Query().Get("self"))
	infoFrom(r.Context()).setSession(self)

	view, err := s.svc.Board(r.Context(), project, service.BoardOptions{
		Window: window, Self: self, IncludeQueue: queue, IncludeNotes: notes, NoteLimit: limit,
		Workspace: workspace,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, view)
}

// handleNotices 는 **응답 꼬리에 실을 알림만** 낸다(최근 ask·blocked).
//
// ★ 이 표면이 왜 따로 있나. MCP 도구는 응답마다 꼬리에 미확인 알림을 붙이는데(설계 §6),
// 그 값을 가져올 표면이 dashboard.json 뿐이었다. 그래서 **도구 호출 1회마다**
// 세션 카드 파생(git worktree list · ChangedPaths · UncommittedPaths · UncommittedDelta)이
// 통째로 한 번 더 돌았다.
// 지금은 8~60ms 라 무해하지만, 세션·워크트리가 늘면 모든 도구 응답 지연에
// 저장소 전수 훑기가 얹힌다.
//
// dashboard.json 에 "파생을 건너뛰는 인자"를 더하는 쪽도 정당했지만 이쪽을 골랐다.
// 이유는 **비대칭이 이미 결함을 가리키고 있어서**다: service 계층에는 이 축만 여는
// RecentNotes 가 진작 있었고(그 함수 주석이 바로 이 비용을 이유로 든다),
// 없는 것은 그것을 밖으로 내는 REST 표면 하나뿐이었다. 인자를 더하면
// "무엇을 부르는가"가 여전히 화면 표면이라 /metrics 의 라우트 라벨에서 두 트래픽이
// 계속 한 덩어리로 남는다 — 표면을 가르면 그 라벨이 그대로 계측 축이 된다.
func (s *server) handleNotices(w http.ResponseWriter, r *http.Request) {
	project, ok := s.requireQuery(w, r, "project", "알림은 프로젝트 안에서만 좌표가 있다.")
	if !ok {
		return
	}
	limit, err := queryInt(r, "limit", 0)
	if err != nil {
		s.writeError(w, r, badRequest("bad_limit", "limit 이 정수가 아니다", "예: limit=20"))
		return
	}
	// **거르지 않은 것을 낸다.** "내가 쓴 것은 알림이 아니다"는 표시 계층의 판정이고
	// (mcpsrv.FilterNotes), 그 축을 여기서 접으면 같은 목록을 다른 '나'로 다시 볼 수 없다.
	// 그래서 self·session_id 인자를 두지 않는다 — 결과를 안 바꾸는 인자는 만들지 않는다.
	notes, err := s.svc.RecentNotes(r.Context(), project, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"notes": notes})
}

// handleEvents 는 SSE 구독이다.
//
// ★ 하트비트 주석 줄을 주기적으로 보낸다. 프록시와 로드밸런서는 조용한 연결을
// 유휴로 보고 끊는데, 그러면 "이벤트가 안 온다"와 "연결이 끊겼다"가 구분되지 않는다.
//
// ★ 클라이언트가 끊으면 이 핸들러가 **반드시 반환한다**. 반환 경로가 하나뿐이도록
// 구독 해제를 defer 에 두고, 쓰기 실패도 전부 반환으로 접는다 —
// 여기서 새는 고루틴은 세션 수만큼 누적되고 그 사실은 어디에도 안 남는다.
//
// ★ 자격증명은 **구독을 건 뒤에 한 번 더** 잰다. 철회 · 끄기 · 로그아웃은 커밋한 뒤 Hub.Cut 으로 열린 스트림을 고르는데, 게이트의
// 판정과 Subscribe 사이에 그 커밋 · Cut 이 끼면 Cut 은 아직 없는 구독을 못 고르고 이 스트림은 철회된 자격증명으로 계속 받는다.
// 구독 뒤에 다시 재면 철회 커밋이 재기 전이면 여기서 거절되고, 잰 뒤면 Cut 이 이미 걸린 구독을 고른다 - 틈이 없다.
// 화면 세션은 만료 시각에 스스로 닫는다(Principal.ExpiresAt) - 열린 스트림에는 게이트가 만료를 다시 볼 다음 요청이 없다.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))

	// 주체는 게이트가 실었다. 철회 · 끄기 · 로그아웃이 이 열쇠로 이 스트림을 고른다(Hub.Cut).
	who, _ := auth.FromContext(r.Context())
	subscriber := s.hub.Subscribe(project, OwnerOf(who))
	defer s.hub.Unsubscribe(subscriber)
	d := s.gate.Authenticate(r.Context(), authRequest(r))
	if !d.OK {
		s.refuseAuth(w, r, d)
		return
	}
	var expired <-chan time.Time
	if !d.Principal.ExpiresAt.IsZero() {
		t := time.NewTimer(d.Principal.ExpiresAt.Sub(s.now()))
		defer t.Stop()
		expired = t.C
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx 가 버퍼링하면 하트비트가 죽는다
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)

	// 첫 줄을 즉시 보낸다 — 구독이 성립했다는 사실 자체가 소비자에게 필요한 신호다.
	//
	// ★ **무엇을 구독했는지 함께 말한다.** 범위를 안 말하면 뒤이은 침묵이
	// "아무 일도 없다"인지 "필터가 아무것도 안 맞춘다"인지 구분되지 않는다 —
	// 프로젝트 이름 오타 하나로 영원히 조용한 스트림을 정상으로 읽게 된다.
	// 알려진 프로젝트인지도 함께 본다: 모르는 이름이면 그 사실이 첫 줄에 뜬다.
	if _, err := io.WriteString(w, ": connected "+s.subscriptionScope(r.Context(), project)+"\n\n"); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		s.log.WarnContext(r.Context(), "SSE 플러시 실패 — 구독을 끊는다", "error", err.Error())
		return
	}

	beat := time.NewTicker(s.opt.Heartbeat)
	defer beat.Stop()

	for {
		// ★ 종료 통지를 **먼저** 본다. select 는 준비된 갈래 중 무작위로 고르므로,
		// 구독자 버퍼에 프레임이 남아 있으면 통지를 받고도 최대 버퍼 길이(기본 32)만큼
		// 더 쓴다 — 그러면 종료 시간이 버퍼 길이만큼 흔들리고, 읽지 않는 피어에 쓰면
		// 그 자리에서 막힌다.
		select {
		case <-s.draining:
			s.byeAndReturn(w, rc)
			return
		default:
		}

		select {
		case <-s.draining:
			// 서버가 종료 중이다 — 클라이언트 절단과 **다른 사실**이고, 그래서
			// 다른 채널로 온다(api.go 의 draining 주석).
			s.byeAndReturn(w, rc)
			return
		case <-r.Context().Done():
			return // 클라이언트가 끊었다
		case <-subscriber.Cut():
			// 이 주체의 자격증명이 철회됐다(기기 철회 · 계정 끄기 · 로그아웃). 다시 붙으면 게이트가 401 을 낸다.
			if _, err := io.WriteString(w, ": bye 자격증명이 철회됐다 - 다시 로그인해라\n\n"); err == nil {
				_ = rc.Flush()
			}
			return
		case <-expired:
			// 화면 세션이 만료됐다. 다시 붙으면 게이트가 401(로그인 폼)을 낸다.
			if _, err := io.WriteString(w, ": bye 세션이 만료됐다 - 다시 로그인해라\n\n"); err == nil {
				_ = rc.Flush()
			}
			return
		case <-beat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case frame, ok := <-subscriber.ch:
			if !ok {
				// ★ **도달 불가다.** 이 채널을 닫는 자리는 Hub.Unsubscribe 하나뿐이고 그
				// 유일한 프로덕션 호출자는 이 핸들러 자신의 defer — 즉 루프가 반환한 뒤에 돈다.
				// 방어로만 남긴다(없으면 닫힌 채널에서 빈 프레임을 무한히 쓴다).
				// **서버 종료는 위 s.draining 이 말한다.** 두 사실을 한 채널로 접지 않는다 —
				// 접으면 !ok 하나가 "허브가 닫혔다"와 "내 구독이 해지됐다" 둘의 신호가 된다.
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// byeAndReturn 은 종료 사유를 와이어에 한 줄 남기고 나간다.
//
// ★ **이 줄이 닿는 곳을 정확히 적는다 — 대시보드가 아니다.** SSE 규약상 `:` 로 시작하는
// 줄은 파서가 버리므로 `EventSource` 의 어떤 핸들러에도 안 간다. 유일한 프로덕션 소비자인
// 대시보드가 보는 것은 이 줄이 있든 없든 EOF → `onerror` 하나이고, 그쪽 판별력은 안 바뀐다.
// 이 줄을 읽는 것은 **`curl -N` 으로 스트림을 보는 운영자와 시험**이다. 그 둘에게는 값이
// 있다: 조용한 EOF 는 "서버가 내려갔다"와 "이벤트가 안 온다"를 안 가르는데 이 줄은 가른다.
// 와이어 계약은 안 바뀐다(주석 줄이라 소비자가 안전하게 무시하고 재연결한다).
//
// 오류는 삼킨다. 이미 나가는 길이고, 여기서 할 수 있는 다른 일이 없다.
func (s *server) byeAndReturn(w io.Writer, rc *http.ResponseController) {
	if _, err := io.WriteString(w, ": bye 서버가 종료한다 — 재연결하면 이어진다\n\n"); err != nil {
		return
	}
	_ = rc.Flush()
}

// handleUnmatched 는 라우트를 못 맞춘 요청에 JSON 으로 답한다.
//
// 기본 404 는 평문이라 클라이언트의 오류 파싱 경로가 두 벌이 된다.
// 그리고 **경로를 응답에 되비추지 않는다** — 되비추면 그것이 반사형 노출 통로가 된다.
func (s *server) handleUnmatched(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, r, Classified{
		Status: http.StatusNotFound, Code: "no_route",
		Message:  "그런 표면이 없다",
		Guidance: "정본 표면은 /api/v1/ 아래에 있다. 목록은 설계 §6 의 REST 표다.",
	})
}

// subscriptionScope 는 이 구독이 무엇을 받는지 한 줄로 말한다.
//
// 침묵과 부재를 가르기 위한 것이다 — 스트림이 조용할 때 그것이 "일이 없다"인지
// "이 필터에 맞는 것이 애초에 없다"인지는 이 줄이 없으면 답할 수 없다.
func (s *server) subscriptionScope(ctx context.Context, project string) string {
	if project == "" {
		return "— 전 프로젝트(필터 없음)"
	}
	if s.st == nil {
		return "— 프로젝트 " + clip(project, 64)
	}
	if _, err := s.st.GetProject(ctx, project); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "— 프로젝트 " + clip(project, 64) +
				" (★ 등록되지 않은 이름이다 — 이 구독은 아무것도 받지 못한다. 오타이거나 아직 안 만든 프로젝트다)"
		}
		return "— 프로젝트 " + clip(project, 64) + " (등록 여부를 확인하지 못했다)"
	}
	return "— 프로젝트 " + clip(project, 64)
}
