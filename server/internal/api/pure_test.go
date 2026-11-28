package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/auth"
	"github.com/kweiza/flightdeck/internal/buildinfo"
	"github.com/kweiza/flightdeck/internal/nulesc"
	"github.com/kweiza/flightdeck/internal/service"
	"github.com/kweiza/flightdeck/internal/store"
)

// 순수 함수 시험 — 표와 **표 밖 케이스**를 함께 둔다.
// 표만 두면 시험이 구현자가 상상한 입력만 보게 되고, 그 밖의 입력이 정확히
// 결함이 사는 자리다.

func TestRoutePattern(t *testing.T) {
	cases := []struct{ pattern, method, want string }{
		{"POST /api/v1/sessions", "POST", "POST /api/v1/sessions"},
		{"GET /api/v1/items/next", "GET", "GET /api/v1/items/next"},
		{"", "GET", "GET unmatched"},
		{"/", "DELETE", "DELETE unmatched"},
		{"", "", "unmatched"},
		// 표 밖: 못 맞춘 요청의 경로가 라벨에 새면 카디널리티가 요청 수만큼 는다.
		{"  ", "get", "GET unmatched"},
	}
	for _, c := range cases {
		if got := RoutePattern(c.pattern, c.method); got != c.want {
			t.Errorf("RoutePattern(%q,%q)=%q, 기대 %q", c.pattern, c.method, got, c.want)
		}
	}
}

func TestJudgeIdempotencyKey(t *testing.T) {
	cases := []struct {
		name, method, key string
		wantOK            bool
	}{
		{"GET 은 불요", http.MethodGet, "", true},
		{"HEAD 도 불요", http.MethodHead, "", true},
		{"POST 에 키 있음", http.MethodPost, "s1:1", true},
		{"POST 에 키 없음", http.MethodPost, "", false},
		{"PUT 에 키 없음", http.MethodPut, "  ", false},
		{"PATCH 에 키 없음", http.MethodPatch, "", false},
		{"DELETE 에 키 없음", http.MethodDelete, "", false},
		// 표 밖
		{"키에 공백", http.MethodPost, "s1 1", false},
		{"키에 제어문자", http.MethodPost, "s1\n1", false},
		{"키가 너무 김", http.MethodPost, strings.Repeat("k", 201), false},
		{"소문자 메서드", "post", "", false},
		{"키 앞뒤 공백은 허용", http.MethodPost, "  s1:1  ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := JudgeIdempotencyKey(c.method, c.key)
			if got.OK != c.wantOK {
				t.Fatalf("OK=%v, 기대 %v (사유 %q)", got.OK, c.wantOK, got.Reason)
			}
			if got.Reason == "" {
				t.Fatal("사유가 비었다")
			}
		})
	}
}

// 키는 원장의 이름 칸에 담기므로 이름 자리의 규칙으로 잰다: 깨진 UTF-8 · NUL 은 거절이고 그 갈래(Bad)를 함께 낸다.
// 깨진 바이트는 제어문자가 아니라(U+FFFD 로 읽힌다) 문자 검사만으로는 지나간다.
func TestJudgeIdempotencyKeyRefusesWhatTheLedgerCannotHold(t *testing.T) {
	for _, c := range []struct {
		name, key string
		want      error
	}{
		{"깨진 UTF-8", "s1:\xff", nulesc.ErrUTF8},
		{"자른 다바이트 글자", "s1:\xc3", nulesc.ErrUTF8},
		{"NUL", "s1:\x00", nulesc.ErrNUL},
	} {
		v := JudgeIdempotencyKey(http.MethodPost, c.key)
		if v.OK || !errors.Is(v.Bad, c.want) || v.Reason == "" {
			t.Errorf("%s: %+v, 바라는 것 거절 · 갈래 %v", c.name, v, c.want)
		}
	}
	if v := JudgeIdempotencyKey(http.MethodPost, "s1:한글"); !v.OK || v.Bad != nil {
		t.Errorf("올바른 UTF-8 키를 거절했다: %+v", v)
	}
}

func TestJudgeIdemMatch(t *testing.T) {
	m := JudgeIdemMatch("", "abc")
	if m.Replay || m.Conflict || m.Reason == "" {
		t.Fatalf("처음 보는 키는 새 처리여야 한다: %+v", m)
	}
	m = JudgeIdemMatch("abc", "abc")
	if !m.Replay || m.Conflict {
		t.Fatalf("같은 지문은 재생이어야 한다: %+v", m)
	}
	m = JudgeIdemMatch("abc", "xyz")
	if m.Replay || !m.Conflict {
		t.Fatalf("다른 지문은 충돌이어야 한다: %+v", m)
	}
}

func TestFingerprintSeparatesFields(t *testing.T) {
	// 표 밖: 구분자 없이 이어 붙이면 (method+path) 의 경계가 옮겨진 조합이 같은 지문이 된다.
	a := Fingerprint("POST", "/a/b", []byte("x"))
	b := Fingerprint("POST", "/a", []byte("/bx"))
	if a == b {
		t.Fatal("경로와 본문의 경계가 옮겨진 두 요청이 같은 지문이다")
	}
	if Fingerprint("post", "/a", nil) != Fingerprint("POST", "/a", nil) {
		t.Fatal("메서드 대소문자는 같은 요청이어야 한다")
	}
	if Fingerprint("POST", "/a", []byte("1")) == Fingerprint("POST", "/a", []byte("2")) {
		t.Fatal("본문이 다르면 지문이 달라야 한다")
	}
}

func TestClassifyError(t *testing.T) {
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		msgHas     string
		internal   bool
	}{
		{"거절", &service.RefusedError{What: "finish", Reason: "본문이 비었다", Guidance: "넷을 적어라"},
			http.StatusBadRequest, "refused", "본문이 비었다", false},
		{"남이 쥔 항목", &store.ClaimHeldError{ItemID: "t5-x", Holder: "S1", At: at, Reason: "이미 남이 잡았다"},
			http.StatusConflict, "claim_held", "S1", false},
		{"선점 거절", &store.ClaimRefusedError{ItemID: "t5-x", Reason: "이미 끝난 항목이다"},
			http.StatusConflict, "claim_refused", "이미 끝난 항목이다", false},
		{"종료된 항목", &store.ItemClosedError{ItemID: "t5-x", State: "dropped", Want: "open"},
			http.StatusConflict, "item_closed", "dropped", false},
		{"자원 점유", &store.ResourceHeldError{Resource: "staging", Holder: store.Holder{SessionID: "S2"}, AcquiredAt: at},
			http.StatusConflict, "resource_held", "staging", false},
		{"없다", fmt.Errorf("항목 조회: %w", store.ErrNotFound),
			http.StatusNotFound, "not_found", "없다", false},
		{"모르는 오류", errors.New("SQL logic error near \"SELECT\": /data/fd.db"),
			http.StatusInternalServerError, "internal", "서버 내부 오류", true},
		// 표 밖: nil 을 넣어도 죽지 않는다(호출부가 실수해도 500 을 흘리지 않는다).
		{"nil", nil, http.StatusOK, "ok", "오류가 없다", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyError(c.err)
			if got.Status != c.wantStatus || got.Code != c.wantCode {
				t.Fatalf("status=%d code=%q, 기대 %d %q", got.Status, got.Code, c.wantStatus, c.wantCode)
			}
			if !strings.Contains(got.Message, c.msgHas) {
				t.Fatalf("문구에 %q 가 없다: %q", c.msgHas, got.Message)
			}
			if got.Internal != c.internal {
				t.Fatalf("Internal=%v, 기대 %v", got.Internal, c.internal)
			}
		})
	}
}

// ★ 이 시험이 이 파일의 핵심이다: **모르는 오류의 문구는 입력과 무관해야 한다.**
// err.Error() 를 한 글자라도 옮기면 여기서 걸린다.
func TestClassifyErrorNeverEchoesUnknownCause(t *testing.T) {
	secret := "/home/user/.flightdeck/fd.db"
	c := ClassifyError(fmt.Errorf("unable to open database file: %s (sqlite3.Error)", secret))
	if strings.Contains(c.Message+c.Guidance, secret) {
		t.Fatalf("응답 문구에 하류 문자열이 새어 나왔다: %q", c.Message+c.Guidance)
	}
	if strings.Contains(strings.ToLower(c.Message+c.Guidance), "sqlite") {
		t.Fatalf("응답 문구에 드라이버 이름이 새어 나왔다: %q", c.Message)
	}
}

func TestRenderMetrics(t *testing.T) {
	out := RenderMetrics(MetricsSnapshot{
		Requests: map[RequestKey]uint64{
			{Route: "POST /api/v1/items", Status: 201}:     2,
			{Route: "GET /api/v1/items/next", Status: 200}: 1,
		},
		DurationSum:   map[string]float64{"POST /api/v1/items": 0.5},
		DurationCount: map[string]uint64{"POST /api/v1/items": 2},
		Unauthorized:  3,
		RateLimited:   4,
		SSESubs:       2,
		DB: store.DBStats{
			Locks: map[store.LockSpace]store.LockWait{
				// 칸은 누적이 아닌 건수로 오고 문서는 누적으로 쓴다: 0.001 칸 1, 0.2 칸 2, 5초 넘음 1.
				store.LockGlobal: {Buckets: lockBuckets(map[int]uint64{0: 1, 6: 2}), Count: 4, Sum: 6.5, NotAcquired: 3},
			},
			PoolWaits: 7, PoolWaitSeconds: 1.25,
		},
	})
	for _, want := range []string{
		`flightdeck_requests_total{route="POST /api/v1/items",status="201"} 2`,
		`flightdeck_requests_total{route="GET /api/v1/items/next",status="200"} 1`,
		`flightdeck_request_duration_seconds_count{route="POST /api/v1/items"} 2`,
		"flightdeck_unauthorized_total 3",
		"flightdeck_rate_limited_total 4",
		"flightdeck_sse_subscribers 2",
		"# TYPE flightdeck_requests_total counter",
		"# TYPE flightdeck_tx_lock_wait_seconds histogram",
		`flightdeck_tx_lock_wait_seconds_bucket{lock="global",le="0.001"} 1`,
		`flightdeck_tx_lock_wait_seconds_bucket{lock="global",le="0.1"} 1`,
		`flightdeck_tx_lock_wait_seconds_bucket{lock="global",le="0.2"} 3`,
		`flightdeck_tx_lock_wait_seconds_bucket{lock="global",le="5"} 3`,
		`flightdeck_tx_lock_wait_seconds_bucket{lock="global",le="+Inf"} 4`,
		`flightdeck_tx_lock_wait_seconds_sum{lock="global"} 6.500000`,
		`flightdeck_tx_lock_wait_seconds_count{lock="global"} 4`,
		// 안 쓴 공간도 0 으로 나온다(0 과 「안 잰다」를 가른다)
		`flightdeck_tx_lock_wait_seconds_bucket{lock="obs",le="+Inf"} 0`,
		"# HELP flightdeck_tx_lock_not_acquired_total 잠금을 못 얻고 실패한",
		`flightdeck_tx_lock_not_acquired_total{lock="global"} 3`,
		`flightdeck_tx_lock_not_acquired_total{lock="obs"} 0`,
		"flightdeck_db_pool_waits_total 7",
		"flightdeck_db_pool_wait_seconds_total 1.250000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("지표 문서에 %q 가 없다:\n%s", want, out)
		}
	}
	// 표 밖: 같은 스냅숏은 항상 같은 문서여야 한다(맵 순회 순서가 새면 안 된다).
	for i := 0; i < 20; i++ {
		if again := RenderMetrics(MetricsSnapshot{
			Requests: map[RequestKey]uint64{
				{Route: "b", Status: 200}: 1, {Route: "a", Status: 500}: 1, {Route: "a", Status: 200}: 1,
			},
			DurationSum: map[string]float64{}, DurationCount: map[string]uint64{},
		}); again != RenderMetrics(MetricsSnapshot{
			Requests: map[RequestKey]uint64{
				{Route: "a", Status: 200}: 1, {Route: "a", Status: 500}: 1, {Route: "b", Status: 200}: 1,
			},
			DurationSum: map[string]float64{}, DurationCount: map[string]uint64{},
		}) {
			t.Fatal("같은 상태에서 서로 다른 지표 문서가 나왔다 — 순서가 고정되지 않았다")
		}
	}
}

// lockBuckets 는 칸 번호 → 건수로 store.LockWait 의 칸 배열을 만든다.
func lockBuckets(at map[int]uint64) (out [len(store.LockWaitBuckets)]uint64) {
	for i, n := range at {
		out[i] = n
	}
	return out
}

func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel(`a"b\c` + "\n"); got != `a\"b\\c\n` {
		t.Fatalf("이스케이프가 다르다: %q", got)
	}
}

func TestEncodeSSE(t *testing.T) {
	frame, err := EncodeSSE("01ABC", Event{
		Kind: "judgment.note", Project: "cp", SessionID: "S1",
		At:     time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC),
		Detail: map[string]any{"bytes": 12},
	})
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	s := string(frame)
	if !strings.HasPrefix(s, "id: 01ABC\ndata: {") {
		t.Fatalf("프레임 머리가 다르다:\n%s", s)
	}
	if !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("프레임이 빈 줄로 안 끝난다:\n%q", s)
	}
	// ★ 표 밖: 본문에 개행이 있어도 프레임이 쪼개지면 안 된다.
	frame, err = EncodeSSE("id\n주입", Event{Kind: "x\ny", At: time.Now(),
		Detail: map[string]any{"body": "첫 줄\n\ndata: 가짜\n\n"}})
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	if n := strings.Count(strings.TrimSuffix(string(frame), "\n\n"), "\n\n"); n != 0 {
		t.Fatalf("프레임 안에 빈 줄이 생겼다(주입 통로다):\n%q", string(frame))
	}
	if lines := strings.Split(strings.TrimSuffix(string(frame), "\n\n"), "\n"); len(lines) != 2 {
		// id·data 둘뿐이다 — event 줄을 안 찍는다(브라우저 onmessage 가 발화해야 하므로)
		t.Fatalf("프레임 줄 수가 2가 아니다: %q", string(frame))
	}
}

func TestValidateWorkspacePath(t *testing.T) {
	if err := ValidateWorkspacePath("/abs/path"); err != nil {
		t.Fatalf("절대경로가 거절됐다: %v", err)
	}
	for _, bad := range []string{"", "  ", "rel/path", "./x"} {
		if err := ValidateWorkspacePath(bad); err == nil {
			t.Fatalf("%q 가 통과했다 — 상대경로는 서버와 세션이 서로 다른 곳을 가리킨다", bad)
		}
	}
}

func TestAuthNoticeAndHealthzScrub(t *testing.T) {
	if !strings.Contains(AuthNotice(false, auth.ModeTransition, auth.TLSFiles), "무인증") {
		t.Fatal("토큰 미설정을 알리지 않는다 — 조용한 무인증은 안 된다")
	}
	if n := AuthNotice(true, auth.ModeTransition, auth.TLSFiles); !strings.Contains(n, "FD_AUTH=accounts") {
		t.Fatalf("전환 단계가 다음 걸음(좁히기)을 안 준다: %s", n)
	}
	// 계정 단계는 토큰이 있든 없든 무인증 문구가 없다 - 그 단계에는 무인증 갈래가 없다.
	if n := AuthNotice(false, auth.ModeAccounts, auth.TLSFiles); strings.Contains(n, "무인증") || !strings.Contains(n, "/admin/bootstrap") {
		t.Fatalf("계정 단계 문구가 틀렸다: %s", n)
	}
	if n := AuthNotice(true, auth.ModeAccounts, auth.TLSOff); !strings.Contains(n, "평문") {
		t.Fatalf("평문 전송을 안 알린다: %s", n)
	}
	if n := AuthNotice(true, auth.ModeAccounts, auth.TLSProxy); strings.Contains(n, "평문") {
		t.Fatalf("TLS 를 켰는데 평문이라 한다: %s", n)
	}
	// 401 처방은 어느 단계든 루프백도 자격증명이 필요하다고 말한다(면제가 없어졌다).
	for _, m := range []auth.Mode{auth.ModeTransition, auth.ModeAccounts} {
		if g := UnauthorizedGuidance(m); !strings.Contains(g, "루프백도 같다") || !strings.Contains(g, "Bearer") {
			t.Fatalf("%s 의 401 처방이 틀렸다: %s", m, g)
		}
	}
	if strings.Contains(UnauthorizedGuidance(auth.ModeAccounts), "둘 다 통한다") {
		t.Fatal("계정 단계 처방이 FD_TOKEN 도 통한다고 말한다")
	}

	body := HealthzOf(service.Health{
		OK: false, APIVersion: "1", DBOK: false,
		DBPath:    "/home/user/.flightdeck/fd.db",
		DBError:   "unable to open database file /home/user/.flightdeck/fd.db",
		DiskError: "statfs /home/user/.flightdeck: no such file",
	}, true, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{}, LedgerBackupStatus{})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	if strings.Contains(string(raw), "/home/user") {
		t.Fatalf("/healthz 본문에 서버 파일 경로가 새어 나왔다: %s", raw)
	}
	if !strings.Contains(string(raw), "db_ok") {
		t.Fatalf("db_ok 축이 없다: %s", raw)
	}
	if body.DBError == "" {
		t.Fatal("DB 가 죽었는데 그 사실이 응답에 없다 — 침묵하면 배너가 정상으로 읽힌다")
	}
}

// TestHealthzKeepsRemoteDiskReason 은 Postgres 원장이라 디스크를 재지 않은 사유(service.DiskRemote)를 /healthz 가
// 「측정 실패 · 로그를 보라」로 바꾸지 않고 그대로 내는지 잰다. 그 갈래는 정상이라 서버 로그에 남는 것이 없다.
func TestHealthzKeepsRemoteDiskReason(t *testing.T) {
	body := HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true, DiskFreePct: -1, DiskError: service.DiskRemote},
		true, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{}, LedgerBackupStatus{})
	if body.DiskKnown || body.DiskError != service.DiskRemote {
		t.Fatalf("원격 DB 의 디스크 항목이 (%v, %q)다 - 기대 (false, %q)", body.DiskKnown, body.DiskError, service.DiskRemote)
	}
}

// TestHealthzCarriesTheLocaleAxis 는 /healthz 가 로케일 관문의 결과(db_locale_ok)를 늘 싣고, 거절 사유는 고정 문구로
// 바꾸며, 못 잰 사유는 그대로 내는지 잰다. 거짓일 때도 키가 있어야 한다 - 빠지면 감시가 「모름」과 「틀렸다」를 못 가른다.
func TestHealthzCarriesTheLocaleAxis(t *testing.T) {
	ok := HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true, DBLocaleOK: true},
		true, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{}, LedgerBackupStatus{})
	if !ok.DBLocaleOK || ok.DBLocaleError != "" {
		t.Fatalf("관문을 지난 원장: (%v, %q)", ok.DBLocaleOK, ok.DBLocaleError)
	}
	refused := HealthzOf(service.Health{APIVersion: "1", DBOK: true,
		DBLocaleError: "DB 로케일이 이 서버가 기대는 값이 아니다 - 정렬 제공자가 builtin 이 아니다(datlocprovider=c)"},
		true, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{}, LedgerBackupStatus{})
	if refused.DBLocaleOK || !strings.Contains(refused.DBLocaleError, "서버 로그") || strings.Contains(refused.DBLocaleError, "datlocprovider") {
		t.Fatalf("거절한 원장: (%v, %q)", refused.DBLocaleOK, refused.DBLocaleError)
	}
	raw, err := json.Marshal(refused)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"db_locale_ok":false`) {
		t.Fatalf("거짓인 db_locale_ok 가 본문에서 빠졌다: %s", raw)
	}
	down := HealthzOf(service.Health{APIVersion: "1", DBLocaleError: service.DBLocaleUnmeasured},
		true, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{}, LedgerBackupStatus{})
	if down.DBLocaleOK || down.DBLocaleError != service.DBLocaleUnmeasured {
		t.Fatalf("못 잰 원장: (%v, %q)", down.DBLocaleOK, down.DBLocaleError)
	}
}

// TestHealthzDBErrorPointsAtWhatTheLogHas 는 접속 실패 안내가 서버 로그에 실제로 남는 것을 가리키는지 잰다. Postgres 면
// store 가 드라이버 문구를 걷어 로그에도 종류 · SQLSTATE 만 남는다 - 「원인 전문은 로그에」라고 하면 없는 것을 찾으러 간다.
// SQLite 는 로그에 전문(파일 경로)이 있다.
func TestHealthzDBErrorPointsAtWhatTheLogHas(t *testing.T) {
	h := service.Health{APIVersion: "1", DBRemote: true,
		DBError: "Postgres 와 주고받지 못했다(postgres): 서버가 거절했다(SQLSTATE 55000)"}
	pg := HealthzOf(h, true, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{}, LedgerBackupStatus{}).DBError
	if pg != dbErrorRemote || strings.Contains(pg, "전문") || !strings.Contains(pg, "SQLSTATE") {
		t.Fatalf("Postgres 접속 실패 안내가 로그에 남는 것(종류 · SQLSTATE)과 다르다: %q", pg)
	}
	h.DBRemote = false
	if lite := HealthzOf(h, true, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{}, LedgerBackupStatus{}).DBError; !strings.Contains(lite, "원인 전문은 서버 로그에") {
		t.Fatalf("SQLite 접속 실패 안내가 바뀌었다: %q", lite)
	}
}

// ★ 문자열 포함만 보면 태그 오타(예: outcome → outcomeX)가 안 잡힌다 — 그 오타가 나도
// 값은 여전히 응답 어딘가에 있으므로 strings.Contains(raw, "refused") 는 그대로 통과한다.
// 그래서 **키:값 쌍**으로 단언한다. `"outcome":"refused"` 는 태그가 정확히 outcome 일 때만 나온다.
func TestHealthzCarriesSelfUpdateRefusal(t *testing.T) {
	at := time.Date(2026, 8, 5, 0, 31, 2, 0, time.UTC)
	body := HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true},
		false, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{
			Watching: true, LastAt: &at,
			From: "07e5df4", To: "1d044b2",
			Outcome: "refused", Detail: "selfcheck exit 1 — 증분 계획이 거절된다",
		}, LedgerBackupStatus{})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	for _, want := range []string{
		`"self_update":`,
		`"watching":true`,
		`"outcome":"refused"`,
		`"from":"07e5df4"`,
		`"to":"1d044b2"`,
		`"detail":`,
		`"last_at":`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("%s 가 응답에 없다: %s", want, raw)
		}
	}
}

// ★ **보고는 있는데 못 재는** 상태가 선을 넘어가야 한다. 이것이 안 실리면 클라이언트는
// watching=true 만 보고 "따라가는 중"이라 찍는다 — 지워진 바이너리를 감시하는 서버가
// 화면에서는 정상으로 보인다. 여기서도 키:값 쌍으로 단언한다(태그 오타를 잡으려면 그래야 한다).
func TestHealthzCarriesTheStalledWatcher(t *testing.T) {
	body := HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true},
		false, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{
			Watching: true,
			Stalled:  "실행 파일을 못 쟀다: no such file or directory",
		}, LedgerBackupStatus{})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	if !strings.Contains(string(raw), `"stalled":"실행 파일을 못 쟀다`) {
		t.Fatalf("막힌 사실이 선을 안 넘었다: %s", raw)
	}
	// 아무 일도 없을 때는 안 나가야 한다 — 빈 축이 매번 실리면 읽는 쪽이 그 키를 무시하게 된다.
	quiet, err := json.Marshal(HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true},
		false, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{Watching: true}, LedgerBackupStatus{}))
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	if strings.Contains(string(quiet), `"stalled"`) {
		t.Fatalf("막히지 않았는데 stalled 가 실렸다: %s", quiet)
	}
}

// ★ **못 덮는 갈래도 선을 넘어야 한다.** 이것이 안 실리면 클라이언트는 watching=true 만
// 보고 "따라가는 중"이라 찍는데, 그 서버는 플러그인 버전이 올라도 영영 안 바뀐다 —
// 침묵이 아니라 **틀린 안심**이다. Stalled 와 **다른 키**인 것이 계약이다: 접으면
// "지금 못 잰다"(회복된다)와 "영영 안 바뀔 자리를 잰다"(회복이 없다)가 같은 값이 된다.
func TestHealthzCarriesTheUncoveredBranch(t *testing.T) {
	body := HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true},
		false, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{
			Watching:  true,
			Uncovered: "이 실행 파일 이름에는 소스 트리가 박혀 있다(런처 bin/fd)",
		}, LedgerBackupStatus{})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	if !strings.Contains(string(raw), `"uncovered":"이 실행 파일 이름에는`) {
		t.Fatalf("못 덮는 갈래가 선을 안 넘었다: %s", raw)
	}
	// 두 축이 한 키로 접히지 않았는가 — stalled 는 안 찼으니 안 나가야 한다.
	if strings.Contains(string(raw), `"stalled"`) {
		t.Fatalf("못 덮는 갈래를 stalled 로 접었다: %s", raw)
	}
	// 덮는 배치에서는 안 나가야 한다. 빈 축이 매번 실리면 읽는 쪽이 그 키를 무시하게 된다.
	quiet, err := json.Marshal(HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true},
		false, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{Watching: true}, LedgerBackupStatus{}))
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	if strings.Contains(string(quiet), `"uncovered"`) {
		t.Fatalf("다 덮는데 uncovered 가 실렸다: %s", quiet)
	}
}

// ★ 안 보고 있다는 사실이 '아직 갱신이 없었다'로 접히면 안 된다.
// json.Marshal 을 거쳐 **실제 바이트**로 확인한다 — 구조체 필드만 보면 태그 오타를
// 원리적으로 못 잡는다(Go 필드 값은 태그와 무관하게 그대로 있으므로).
func TestHealthzSaysWhenItIsNotWatching(t *testing.T) {
	body := HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true},
		false, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{
			Watching: false, Reason: "이 플랫폼은 자기 재기동을 지원하지 않는다",
		}, LedgerBackupStatus{})
	if body.SelfUpdate.Watching {
		t.Fatal("watching 이 참이다")
	}
	if strings.TrimSpace(body.SelfUpdate.Reason) == "" {
		t.Fatal("왜 안 보는지가 비었다")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	for _, want := range []string{`"watching":false`, `"reason":`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("%s 가 응답에 없다: %s", want, raw)
		}
	}
}

// ★ 시도가 없었으면 last_at 은 응답에서 아예 **빠져야** 한다(omitempty). 제로 시각을
// null 로 찍으면 "시도가 있었는데 시각을 모른다"로 읽힐 수 있다 — 부재와 null 은 다른 말이다.
//
// ★ 이 시험이 재는 것은 **omitempty 하나**다. 앞선 판의 주석은 "serve.go 의 변환이
// 깨지면 이 시험이 잡는다"고 적었는데 거짓이었다 — 여기서는 nil 을 직접 넣으므로 상류가
// 항상 &at 를 채우게 바뀌어도 이 시험은 초록이다(2026-08-07 실측: 그 자리는 아무 시험에도
// 안 걸렸다). 그 변환을 재는 것은 cmd/fd 의 selfUpdateStatusOf 갈래이고, 이 선의 두
// 조각은 **각자 자기 것만** 잰다.
func TestHealthzOmitsLastAtWhenNoAttemptEver(t *testing.T) {
	body := HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true},
		false, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{
			Watching: true, // 감시 중이지만 아직 교체를 한 번도 안 봤다 — LastAt 이 nil
		}, LedgerBackupStatus{})
	if body.SelfUpdate.LastAt != nil {
		t.Fatalf("시도가 없었는데 LastAt 이 채워졌다: %v", body.SelfUpdate.LastAt)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	if strings.Contains(string(raw), `"last_at"`) {
		t.Fatalf("last_at 이 omitempty 로 안 빠졌다: %s", raw)
	}
}

func TestRefill(t *testing.T) {
	base := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if got := Refill(0, base, base.Add(10*time.Second), 1, 60); got != 10 {
		t.Fatalf("10초에 10토큰이어야 한다: %v", got)
	}
	if got := Refill(0, base, base.Add(time.Hour), 1, 60); got != 60 {
		t.Fatalf("상한을 넘었다: %v", got)
	}
	// 표 밖: 시계가 뒤로 갔을 때 토큰이 줄면 안 된다.
	if got := Refill(5, base, base.Add(-time.Hour), 1, 60); got != 5 {
		t.Fatalf("시계 역행에서 토큰이 변했다: %v", got)
	}
}

func TestClip(t *testing.T) {
	if got := clip("가나다라", 2); got != "가나…" {
		t.Fatalf("룬 단위로 안 잘린다: %q", got)
	}
	if got := clip("a\nb\x00c", 10); got != "a b c" {
		t.Fatalf("제어문자가 안 걷혔다: %q", got)
	}
}

// 판단 원장 백업 축이 본문까지 실려 나간다.
//
// ★ 순수 함수(HealthzOf)를 통과시키는 것이 요점이다. 본문 뒤에서 필드를 채우면 그 변환이
// 어느 시험에도 안 걸리고, 앞선 물결이 자동 갱신 축에서 정확히 그렇게 값을 잃었다.
func TestHealthzCarriesLedgerBackup(t *testing.T) {
	at := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)
	body := HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true},
		true, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{},
		LedgerBackupStatus{Running: true, LastAt: &at, Outcome: "failed",
			Detail: "디스크가 찼다", Route: "/ledger"})
	if !body.LedgerBackup.Running || body.LedgerBackup.Outcome != "failed" {
		t.Fatalf("축이 본문에 안 실렸다: %+v", body.LedgerBackup)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	for _, want := range []string{`"ledger_backup"`, `"outcome":"failed"`, `"route":"/ledger"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s 가 wire 에 없다:\n%s", want, raw)
		}
	}
	// 회차가 없으면 시각 키가 통째로 빠진다 — 제로값이 1970년으로 나가면 안 된다.
	quiet, err := json.Marshal(HealthzOf(service.Health{OK: true, APIVersion: "1", DBOK: true},
		true, auth.ModeTransition, auth.TLSOff, buildinfo.Coord{}, SelfUpdateStatus{},
		LedgerBackupStatus{Running: true}))
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	if strings.Contains(string(quiet), `"last_at"`) {
		t.Errorf("회차가 없는데 last_at 이 실렸다:\n%s", quiet)
	}
}

func TestJudgeScreenPath(t *testing.T) {
	// ★ 이 표가 이 설계 전체의 안전을 지탱한다. /api/v1 이 참이 되는 순간
	// REST 쓰기의 CSRF 방어가 쿠키의 SameSite 하나로 줄어든다.
	cases := map[string]bool{
		"/":                     true,
		"/events":               true,
		"/actions/reclaim":      true,
		"/actions/drop":         true,
		"/actions/lane-release": true,
		"/api/v1/items/next":    false,
		"/api/v1/events":        false, // REST 쪽 별칭이다 — 화면이 무는 것은 /events 다
		"/healthz":              false,
		"/metrics":              false,
		"/login":                false,
		"/actions":              false, // 접두는 슬래시까지다
		"":                      false,
	}
	for path, want := range cases {
		if got := JudgeScreenPath(path); got != want {
			t.Errorf("JudgeScreenPath(%q) = %v, 기대 %v", path, got, want)
		}
	}
}

func TestJudgeLoginScreen(t *testing.T) {
	cases := map[string]bool{
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8": true,
		"text/html":         true,
		"TEXT/HTML":         true, // 대소문자 무시
		"application/json":  false,
		"text/event-stream": false, // EventSource 는 폼을 못 읽는다
		"*/*":               false, // curl 기본값 — 사람이 아니다
		"":                  false,
	}
	for accept, want := range cases {
		if got := JudgeLoginScreen(accept); got != want {
			t.Errorf("JudgeLoginScreen(%q) = %v, 기대 %v", accept, got, want)
		}
	}
}

func TestJudgeNext(t *testing.T) {
	cases := map[string]string{
		"/":                   "/",
		"/?project=Kweiza":    "/?project=Kweiza",
		"/?q=a%20b":           "/?q=a%20b",
		"":                    "/",
		"   ":                 "/",
		"//evil.com":          "/", // 프로토콜 상대 URL — 다른 호스트로 나간다
		"///evil.com":         "/",
		"http://evil.com":     "/",
		"https://evil.com/x":  "/",
		"javascript:alert(1)": "/",
		"relative":            "/", // 슬래시로 안 시작하면 거절한다
		"/\\evil.com":         "/", // 일부 브라우저가 \ 를 / 로 정규화한다
	}
	for next, want := range cases {
		if got := JudgeNext(next); got != want {
			t.Errorf("JudgeNext(%q) = %q, 기대 %q", next, got, want)
		}
	}
}

// TestJudgeLoginAction 은 폼 action 의 깊이 셈을 잠근다.
//
// ★ 기대값을 손으로 적는 것으로는 부족하다 — 표가 틀리면 코드와 함께 틀린다. 그래서
// 각 줄을 **문서 URL 에 실제로 해석해서** /login 에 닿는지 함께 본다(RFC 3986 그대로인
// url.ResolveReference). 브라우저가 하는 계산이 그것이다.
func TestJudgeLoginAction(t *testing.T) {
	cases := map[string]string{
		"/":                     "./login",
		"/login":                "./login", // 재시도 폼 — 제출이 실패한 자리도 뿌리 깊이다
		"/events":               "./login",
		"/actions/reclaim":      "../login",
		"/actions/lane-release": "../login",
		"/actions/":             "../login", // 뒤 슬래시면 마디가 하나 더다
		"/api/v1/items/next":    "../../../login",
		"/a//b":                 "../../login", // 빈 마디도 해석이 한 마디로 센다
		"":                      "./login",     // 못 읽은 것은 뿌리로 접는다
		"*":                     "./login",     // OPTIONS * — 슬래시가 없다
	}
	for path, want := range cases {
		got := JudgeLoginAction(path)
		if got != want {
			t.Errorf("JudgeLoginAction(%q) = %q, 기대 %q", path, got, want)
			continue
		}
		// 그리고 그 값이 실제로 /login 으로 풀리는지 본다.
		if path == "" || path == "*" {
			continue // 문서 URL 로 성립하지 않는 자리다 — 위 기대값만 잠근다
		}
		base, err := url.Parse("http://fd.example" + path)
		if err != nil {
			t.Fatalf("문서 URL %q 파싱 실패: %v", path, err)
		}
		ref, err := url.Parse(got)
		if err != nil {
			t.Fatalf("action %q 파싱 실패: %v", got, err)
		}
		if p := base.ResolveReference(ref).Path; p != "/login" {
			t.Errorf("문서 %q 에서 action %q 가 %q 로 풀린다 — /login 이어야 한다", path, got, p)
		}
	}
}

// TestJudgeNextFrom 은 303 이 재생할 수 있는 요청만 돌아갈 자리로 남는지 본다.
func TestJudgeNextFrom(t *testing.T) {
	cases := []struct {
		method, uri, want string
	}{
		{"GET", "/?project=Kweiza", "/?project=Kweiza"},
		{"get", "/", "/"},                         // 메서드는 대소문자를 안 가린다
		{"GET", "//evil.com", "/"},                // 오픈 리다이렉트 축은 JudgeNext 가 계속 본다
		{"POST", "/actions/reclaim?key=abc", "/"}, // 303 은 GET 으로 재생된다 — 405 에 착지한다
		{"DELETE", "/api/v1/items/x", "/"},
		{"HEAD", "/", "/"}, // 303 이 재생하는 것은 GET 하나뿐이다
		{"", "/x", "/"},
	}
	for _, c := range cases {
		if got := JudgeNextFrom(c.method, c.uri); got != c.want {
			t.Errorf("JudgeNextFrom(%q, %q) = %q, 기대 %q", c.method, c.uri, got, c.want)
		}
	}
}

func TestJudgePreSessionPath(t *testing.T) {
	cases := map[string]bool{
		"/login":  true,
		"/logout": true,
		// /healthz 는 여기 없다 — 읽기 하나라 멱등과 무관하고, 인증 게이트만 따로 앞에서 보낸다.
		"/healthz":           false,
		"/":                  false,
		"/api/v1/items/next": false,
		"/login/x":           false,
	}
	for path, want := range cases {
		if got := JudgePreSessionPath(path); got != want {
			t.Errorf("JudgePreSessionPath(%q) = %v, 기대 %v", path, got, want)
		}
	}
}
