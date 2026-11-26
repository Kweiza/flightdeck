package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ─────────────────────────────────────────────────────────────────────────────
// Postgres 판올림 전 덤프(pg_dump -Fc)와 되돌리기 절차
// ─────────────────────────────────────────────────────────────────────────────
//
// SQLite 판은 판올림 전에 원장 옆에 VACUUM INTO 사본(<db>.bak-*)을 뜨고 `fd migrate --rollback` 이 그것을 덮는다.
// Postgres 판은 같은 자리에서 `pg_dump -Fc` 로 백업 자리에 fd-<원장>-from<판>-<시각>.dump 를 뜬다. 되돌리기는 살아 있는
// DB 를 덮지 않는다 - 그 덤프를 **새 DB** 로 복원하는 스크립트를 낸다(pgRollbackScript). 증분은 트랜잭션째 되감기므로
// 실패한 단이 반쯤 남는 일은 없고, 덤프가 필요한 것은 끝까지 커밋된 판올림을 통째로 물리는 날이다.
//
// ★ 덤프는 이전 실행기 세션이 두 잠금(자기 잠금 · 전역 업무 잠금)을 쥔 동안 뜬다(migrateSession). 판을 읽은 뒤부터 마지막
// 증분까지 남의 판올림도 업무 쓰기도 끼지 못하므로, 덤프는 읽은 판의 상태 그대로다. pg_dump 는 다른 프로세스라 세션이
// 끊긴 것을 모른다 - 그래서 최종 이름을 붙이기 전에 세션에서 잠금과 판을 다시 잰다(sealDump). 끊겼으면 이름을 안 붙이고
// 실패한다.
//
// ★ 덤프 이름이 원장을 가린다(ledgerID). 운영 DB 와 시험 DB 가 같은 백업 자리를 써도 되돌리기는 지금 붙은 원장의 덤프만
// 고르고, 그 덤프를 뜬 판보다 지금 판이 높을 때만(덤프 뒤로 커밋된 판올림이 있을 때만) 절차를 낸다.
//
// ★ pg_dump 에 접속 정보를 argv 로 넘기지 않는다. argv 는 같은 기계의 다른 사용자에게 `ps` 로 보인다. 연결 정보는
// 서버가 실제로 붙는 해석(pgx.ParseConfig - openPG 와 같은 해석기)에서 꺼내 libpq 환경 변수로만 넘긴다.
// ★ pg_dump 의 오류 문구는 싣지 않는다. 호스트 · 사용자 · DB 이름을 인용하고, 경계가 모호한 접속 URL 에서는 그 자리에
// 비밀번호 조각이 든다(pgFault 의 ★). 알려진 원인만 고정 문구로 가른다(pgDumpFault).

// dumpPrefix · dumpExt 는 덤프 파일 이름의 앞뒤다: fd-<원장>-from<판>-<시각>.dump. <원장>은 ledgerID, <판>은 덤프를 뜬
// 판(판올림의 출발 판), <시각>은 BackupSuffix 와 같은 폭 고정 UTC 라 사전순이 곧 시간순이다.
const (
	dumpPrefix = "fd-"
	dumpFrom   = "-from"
	dumpExt    = ".dump"
	// dumpMagic 은 pg_dump 사용자 정의 형식(-Fc) 파일의 머리 다섯 바이트다.
	dumpMagic   = "PGDMP"
	stampLayout = "20060102T150405Z"
	ledgerIDLen = 16
)

// dumpInfo 는 덤프 파일 이름이 담은 것이다.
type dumpInfo struct {
	Ledger string // ledgerID
	From   int    // 덤프를 뜬 판
	Stamp  string // UTC 시각(stampLayout)
}

// dumpName 은 덤프 파일 이름이다. 순수 함수다.
func dumpName(ledger string, from int, at time.Time) string {
	return dumpPrefix + ledger + dumpFrom + strconv.Itoa(from) + "-" + at.UTC().Format(stampLayout) + dumpExt
}

// parseDumpName 은 덤프 파일 이름을 푼다. 덤프 이름이 아니면 거짓이다. 순수 함수다.
func parseDumpName(name string) (dumpInfo, bool) {
	if !strings.HasPrefix(name, dumpPrefix) || !strings.HasSuffix(name, dumpExt) {
		return dumpInfo{}, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, dumpPrefix), dumpExt)
	ledger, rest, ok := strings.Cut(body, dumpFrom)
	if !ok || len(ledger) != ledgerIDLen || strings.Trim(ledger, "0123456789abcdef") != "" {
		return dumpInfo{}, false
	}
	from, stamp, ok := strings.Cut(rest, "-")
	if !ok || len(stamp) != len(stampLayout) {
		return dumpInfo{}, false
	}
	v, err := strconv.Atoi(from)
	if err != nil || v < 0 || strconv.Itoa(v) != from {
		return dumpInfo{}, false
	}
	if _, err := time.Parse(stampLayout, stamp); err != nil {
		return dumpInfo{}, false
	}
	return dumpInfo{Ledger: ledger, From: v, Stamp: stamp}, true
}

// ledgerID 는 원장 식별이다: 클러스터(pg_control_system 의 system_identifier)와 DB 이름의 해시 앞 16글자. 순수 함수다.
//
// ★ 복원해도 그대로다 - 같은 클러스터에 같은 이름으로 되살리므로(pg_restore --create) 되살린 원장의 덤프도 같은 원장으로
// 묶인다. 다른 클러스터의 같은 이름 DB, 같은 클러스터의 다른 DB 는 다른 식별이다. DB 이름은 해시로만 싣는다(경계가 모호한
// 접속 URL 에서는 그 이름이 비밀번호 조각일 수 있다 - logPGServer 의 ★). system_identifier 가 섞여 이름을 거꾸로 못 찾는다.
func ledgerID(systemID, database string) string {
	h := sha256.Sum256([]byte(systemID + "\x00" + database))
	return hex.EncodeToString(h[:])[:ledgerIDLen]
}

// adminDB 는 되돌리기 스크립트가 붙을 관리 DB 다. 대상과 달라야 한다 - 붙은 DB 는 이름을 바꿀 수도 지울 수도 없다. 순수 함수다.
func adminDB(database string) string {
	if database == "postgres" {
		return "template1"
	}
	return "postgres"
}

// ledgerFacts 는 지금 붙은 원장의 식별과 관리 DB 다. DB 이름 자체는 담지 않는다(ledgerID 의 ★).
type ledgerFacts struct {
	ID    string
	Admin string
}

// readLedgerFacts 는 q 가 붙은 원장의 ledgerFacts 를 읽는다.
func readLedgerFacts(ctx context.Context, q Reader) (ledgerFacts, error) {
	var sysid, db string
	if err := q.QueryRowContext(ctx,
		`SELECT system_identifier::text, current_database() FROM pg_catalog.pg_control_system()`).Scan(&sysid, &db); err != nil {
		return ledgerFacts{}, fmt.Errorf("원장 식별(system_identifier)을 읽지 못했다: %w", err)
	}
	return ledgerFacts{ID: ledgerID(sysid, db), Admin: adminDB(db)}, nil
}

// latestDump 는 dir 의 덤프 가운데 ledger 원장의 것 중 가장 최근에 뜬 것이다. 판이 아니라 시각으로 고른다 - 판올림이
// 실패해 같은 판을 다시 돌렸으면 나중 덤프가 그 판의 가장 최근 상태다. 다른 원장의 덤프는 아무리 최근이어도 안 고른다.
func latestDump(dir, ledger string) (string, dumpInfo, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", dumpInfo{}, fmt.Errorf("백업 자리를 읽지 못했다(%q): %w", clip(dir, 200), err)
	}
	type cand struct {
		name string
		info dumpInfo
	}
	var cs []cand
	others := 0
	for _, e := range ents {
		info, ok := parseDumpName(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if info.Ledger != ledger {
			others++
			continue
		}
		cs = append(cs, cand{e.Name(), info})
	}
	if len(cs) == 0 {
		return "", dumpInfo{}, fmt.Errorf("되돌릴 덤프가 없다 - %q 에 이 원장(식별 %s)의 %s%s%s<판>-<시각>%s 이 하나도 없다"+
			"(다른 원장의 덤프 %d개는 고르지 않는다). 덤프는 판올림이 기존 데이터 위에 증분을 얹을 때만 뜬다"+
			"(빈 DB 에 기준선을 깐 경우는 안 뜬다)", clip(dir, 200), ledger, dumpPrefix, ledger, dumpFrom, dumpExt, others)
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].info.Stamp != cs[j].info.Stamp {
			return cs[i].info.Stamp < cs[j].info.Stamp
		}
		return cs[i].name < cs[j].name
	})
	last := cs[len(cs)-1]
	return filepath.Join(dir, last.name), last.info, nil
}

// pgTLS 는 pgx 가 해석한 접속 설정의 TLS 를 libpq 의 sslmode 와, 서버 인증서를 재는 접속이면 그 루트 인증서 파일로
// 되옮긴다. roots 는 루트 인증서 파일 후보다(libpqEnv 가 고른다).
//
// ★ 되옮길 수 있는 것만 받는다. pgx 는 sslmode 를 TLS 설정과 대체 접속(Fallbacks)으로 풀어 두고 루트 인증서는 파일
// 경로 대신 읽은 인증서 묶음(RootCAs)만 남긴다. 약하게 넘기면 백업 접속만 조용히 검증 없이 붙는다. 그래서 재는 모드는
// 후보 파일 가운데 **서버가 쓰는 것과 같은 인증서 묶음**을 담은 것을 찾아 PGSSLROOTCERT 로 넘기고, 못 찾으면 거절한다:
//   - verify-ca(sslrootcert 를 준 require 포함): libpq 도 그 파일로 사슬만 잰다. 같은 뜻이다.
//   - verify-full: libpq 도 그 파일로 사슬을, PGHOST 로 호스트 이름을 잰다. 다른 점은 하나 - libpq 는 인증서에 DNS 이름
//     SAN 이 없으면 CN 으로 호스트를 재고 Go 는 SAN 만 본다. 둘 다 호스트를 재는 검증이라 약해지는 쪽이 아니다.
//   - 루트 인증서 없이 재는 접속(sslrootcert=system, 또는 아무 루트도 없는 verify-*)은 받지 않는다. pgx 는 Go 의 시스템
//     인증서 저장소를 쓰고 libpq 는 OpenSSL 의 저장소(16 미만은 system 을 모른다)나 ~/.postgresql/root.crt 를 써서, 같은
//     묶음으로 잰다는 보장이 없다.
//
// 클라이언트 인증서(sslcert · sslkey)는 재지 않는다 - pgx 가 해석 결과에 드러내지 않는다(접속 때 붙이는 비공개 칸이다).
// 넘기지 못해도 약해지는 쪽이 아니다: 인증서는 서버가 이쪽을 재는 자격이라, 서버가 그것을 요구하면 pg_dump 가 인증에
// 실패하고(pgDumpFault 의 「인증에 실패했다」) 판을 올리지 않는다. 기본 자리(~/.postgresql/postgresql.crt)는 libpq 도
// 같은 자리를 읽는다.
func pgTLS(cfg *pgconn.Config, roots []string) (mode, rootcert string, err error) {
	for _, fb := range cfg.Fallbacks {
		if fb.Host != cfg.Host || fb.Port != cfg.Port {
			return "", "", errors.New("호스트가 여럿인 접속이다")
		}
	}
	var tlsFallback, plainFallback bool
	for _, fb := range cfg.Fallbacks {
		if fb.TLSConfig == nil {
			plainFallback = true
		} else {
			tlsFallback = true
		}
	}
	if cfg.TLSConfig == nil {
		if tlsFallback {
			return "allow", "", nil
		}
		return "disable", "", nil
	}
	tc := cfg.TLSConfig
	switch {
	case tc.VerifyConnection != nil:
		return "", "", errors.New("알 수 없는 TLS 검증이 걸린 접속이다")
	case tc.InsecureSkipVerify && tc.VerifyPeerCertificate == nil:
		if plainFallback {
			return "prefer", "", nil
		}
		return "require", "", nil
	case plainFallback || tlsFallback:
		return "", "", errors.New("서버 인증서를 재면서 대체 접속을 가진 접속이다")
	case tc.InsecureSkipVerify:
		mode = "verify-ca"
	case tc.VerifyPeerCertificate == nil && tc.ServerName == cfg.Host:
		mode = "verify-full"
	default:
		return "", "", errors.New("알 수 없는 TLS 검증이 걸린 접속이다")
	}
	if tc.RootCAs == nil {
		return "", "", errors.New("루트 인증서 파일 없이 서버 인증서를 재는 접속이다(시스템 인증서 저장소) - libpq 는 같은 저장소를 쓴다는 보장이 없다")
	}
	for _, p := range roots {
		if p == "" || p == "system" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		pem, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(pem) && pool.Equal(tc.RootCAs) {
			return mode, abs, nil
		}
	}
	return "", "", errors.New("서버 인증서를 재는 접속인데 그 루트 인증서 파일을 찾지 못했다" +
		"(sslrootcert=system 이거나 서비스 파일에서 왔다) - 접속 URL 에 sslrootcert=<파일> 을 적어라")
}

// sslRootCandidates 는 pgx 가 루트 인증서를 읽었을 수 있는 자리다: 접속 URL 의 sslrootcert, 물려받은 PGSSLROOTCERT,
// ~/.postgresql/root.crt. 후보일 뿐이다 - pgTLS 가 인증서 묶음을 대조해 서버와 같은 것만 받는다.
func sslRootCandidates(open string, environ []string) []string {
	var out []string
	if u, err := url.Parse(pgURL(open)); err == nil {
		if v := u.Query().Get("sslrootcert"); v != "" {
			out = append(out, v)
		}
	}
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "PGSSLROOTCERT="); ok && v != "" {
			out = append(out, v)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".postgresql", "root.crt"))
	}
	return out
}

// targetSessionAttrs 는 pgx 가 푼 접속 검증(ValidateConnect)을 libpq 의 target_session_attrs 값으로 되옮긴다. 빈 값은
// any(검증 없음)다. pgx 는 값을 문자열로 남기지 않고 검증 함수로만 남기므로 함수를 대조한다. 모르는 검증이면 거절한다 -
// 빠뜨리면 서버는 주 서버에 붙는데 덤프는 지연된 복제본에서 성공할 수 있다.
func targetSessionAttrs(cfg *pgconn.Config) (string, error) {
	if cfg.ValidateConnect == nil {
		return "", nil
	}
	got := reflect.ValueOf(cfg.ValidateConnect).Pointer()
	for name, f := range map[string]pgconn.ValidateConnectFunc{
		"read-write":     pgconn.ValidateConnectTargetSessionAttrsReadWrite,
		"read-only":      pgconn.ValidateConnectTargetSessionAttrsReadOnly,
		"primary":        pgconn.ValidateConnectTargetSessionAttrsPrimary,
		"standby":        pgconn.ValidateConnectTargetSessionAttrsStandby,
		"prefer-standby": pgconn.ValidateConnectTargetSessionAttrsPreferStandby,
	} {
		if reflect.ValueOf(f).Pointer() == got {
			return name, nil
		}
	}
	return "", errors.New("알 수 없는 접속 검증(target_session_attrs)이 걸린 접속이다")
}

// libpqEnv 는 서버가 붙는 접속(t)을 libpq 도구(pg_dump · pg_restore)에 넘길 환경이다. environ 은 물려받을 환경(os.Environ)이다.
//
// ★ 접속 URL 원문을 쓰는 열기 경계의 하나다(Target 의 ★). 원문은 openPG 와 같은 해석기(pgx.ParseConfig)로 해석하고
// 어디에도 싣지 않는다(루트 인증서 후보를 고를 때만 다시 읽는다). 해석 실패 · 되옮길 수 없는 설정의 오류도 표시값과
// 고정 문구만 싣는다.
//
// ★ 물려받은 환경에서 PG 로 시작하는 변수를 다 뺀다. pgx 는 해석할 때 이미 그것들(PGPASSWORD · PGSSLMODE · .pgpass 등)을
// 반영했으므로, 남겨 두면 libpq 가 같은 변수를 다시 읽어 서버와 다른 곳에 붙을 수 있다. 접속은 아래에서 다시 쓴 것만으로
// 정한다 - 어디에(호스트 · 포트 · DB) 누구로(사용자 · 비밀번호) 붙는가, 어느 서버를 받는가(target_session_attrs), 얼마나
// 기다리는가(connect_timeout), 그리고 접속을 지키는 설정(sslmode · sslrootcert · channel_binding · require_auth ·
// sslnegotiation)이다. 지키는 설정을 빠뜨리면 백업 접속만 비밀번호를 약한 방식으로 내줄 수 있다. 그 설정을 모르는 옛
// libpq 는 변수를 무시한다.
func libpqEnv(t Target, environ []string) ([]string, error) {
	shown := t.Shown()
	cfg, err := pgx.ParseConfig(t.open)
	if err != nil {
		return nil, fmt.Errorf("libpq 도구에 넘길 접속 정보를 해석하지 못했다(%s)", shown)
	}
	refuse := func(err error) error {
		return fmt.Errorf("libpq 도구에 같은 뜻으로 넘길 수 없는 접속이다(%s): %s", shown, err)
	}
	mode, root, err := pgTLS(&cfg.Config, sslRootCandidates(t.open, environ))
	if err != nil {
		return nil, refuse(err)
	}
	tsa, err := targetSessionAttrs(&cfg.Config)
	if err != nil {
		return nil, refuse(err)
	}
	var env []string
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "PG") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"PGHOST="+cfg.Host,
		"PGPORT="+strconv.Itoa(int(cfg.Port)),
		"PGUSER="+cfg.User,
		"PGDATABASE="+cfg.Database,
		"PGSSLMODE="+mode,
	)
	if root != "" {
		env = append(env, "PGSSLROOTCERT="+root)
	}
	if cfg.Password != "" {
		env = append(env, "PGPASSWORD="+cfg.Password)
	}
	if tsa != "" {
		env = append(env, "PGTARGETSESSIONATTRS="+tsa)
	}
	if s := int64(cfg.ConnectTimeout / time.Second); s > 0 {
		env = append(env, "PGCONNECT_TIMEOUT="+strconv.FormatInt(s, 10))
	}
	if cfg.ChannelBinding != "" && cfg.ChannelBinding != "prefer" {
		env = append(env, "PGCHANNELBINDING="+cfg.ChannelBinding)
	}
	if cfg.RequireAuth != "" {
		env = append(env, "PGREQUIREAUTH="+cfg.RequireAuth)
	}
	if cfg.SSLNegotiation != "" {
		env = append(env, "PGSSLNEGOTIATION="+cfg.SSLNegotiation)
	}
	return env, nil
}

// pgDumpArgs 는 pg_dump 한 번의 argv 다. 순수 함수다. 접속 정보는 하나도 없다(libpqEnv 로 간다).
// --no-password 는 비밀번호가 모자랄 때 프롬프트로 멈추지 않고 실패하게 한다(one-shot 컨테이너에는 터미널이 없다).
func pgDumpArgs(bin, file string) []string {
	return []string{bin, "--format=custom", "--no-password", "--file=" + file}
}

// pgDumpFault 는 pg_dump 의 실패를 고정 문구로 가른다. 순수 함수다. stderr 는 가르는 데만 쓰고 싣지 않는다.
func pgDumpFault(runErr error, stderr string) string {
	var ee *exec.ExitError
	code := "?"
	switch {
	case errors.Is(runErr, exec.ErrNotFound), errors.Is(runErr, fs.ErrNotExist):
		return "pg_dump 를 찾지 못했다 - PATH 에 서버와 같거나 높은 큰 판의 pg_dump 를 두거나 실행 파일 자리를 줘라"
	case errors.Is(runErr, context.DeadlineExceeded), errors.Is(runErr, context.Canceled):
		return "pg_dump 가 끝나기 전에 문맥이 끝났다"
	case errors.As(runErr, &ee):
		code = strconv.Itoa(ee.ExitCode())
	}
	l := strings.ToLower(stderr)
	var why string
	switch {
	case strings.Contains(l, "server version mismatch"), strings.Contains(l, "aborting because of server version"):
		why = "pg_dump 의 큰 판이 서버보다 낮다"
	case strings.Contains(l, "password authentication failed"), strings.Contains(l, "no password supplied"),
		strings.Contains(l, "authentication failed"):
		why = "인증에 실패했다"
	// 출력 파일 실패를 권한 거절보다 먼저 가른다 - 백업 자리에 못 쓰는 것도 「Permission denied」로 끝난다.
	case strings.Contains(l, "could not open output file"), strings.Contains(l, "no space left"):
		why = "덤프 파일을 쓰지 못했다(백업 자리의 권한 · 여유를 확인하라)"
	case strings.Contains(l, "permission denied"):
		why = "읽을 권한이 없는 객체가 있다(이전 실행기는 소유 역할이나 초사용자로 붙어야 한다)"
	case strings.Contains(l, "could not connect"), strings.Contains(l, "connection refused"),
		strings.Contains(l, "could not translate host name"):
		why = "서버에 붙지 못했다"
	default:
		why = "분류하지 못한 실패"
	}
	return "pg_dump 가 실패했다(종료 코드 " + code + "): " + why +
		" - pg_dump 의 문구는 접속 세부를 인용해 싣지 않는다"
}

// backupPG 는 이전 실행기의 판올림 전 덤프다(openMigrator 가 run.backup 으로 건다). 세션에서 원장 식별을 읽어 이름을 짓고,
// 다 뜬 뒤에는 같은 세션에서 잠금과 판을 다시 재야 이름을 붙인다(sealDump).
func backupPG(ctx context.Context, t Target, sess *migrateSession, dir string, from int, log *slog.Logger) (string, error) {
	led, err := readLedgerFacts(ctx, sess)
	if err != nil {
		return "", err
	}
	return dumpPG(ctx, t, dir, "pg_dump", led.ID, from, func(ctx context.Context) error {
		return sealDump(ctx, sess, from)
	}, log)
}

// sealDump 는 덤프에 최종 이름을 붙여도 되는가를 잰다: 실행기 세션이 두 잠금을 아직 쥐고 있고(held), 판이 덤프를 시작할
// 때(from)와 같다. 세션이 끊겼다면 그 사이 남이 판을 올렸을 수 있고, 그러면 덤프는 from 판의 상태가 아니다.
func sealDump(ctx context.Context, sess *migrateSession, from int) error {
	if err := sess.held(ctx); err != nil {
		return err
	}
	_, v, _, err := readMigrationState(ctx, sess)
	if err != nil {
		return fmt.Errorf("덤프 뒤에 판을 다시 읽지 못했다: %w", err)
	}
	if v != from {
		return fmt.Errorf("덤프를 뜨는 동안 판이 %d 에서 %d 로 바뀌었다", from, v)
	}
	return nil
}

// dumpPG 는 판올림 전 덤프를 뜬다. 뜬 파일 경로를 낸다. ledger 는 원장 식별(ledgerID), from 은 덤프를 뜨는 판이고, bin 은
// pg_dump 실행 파일(이름이면 PATH 에서 찾는다)이다. seal 이 nil 을 내야 최종 이름을 붙인다.
//
// ★ 임시 이름에 뜨고 다 뜬 뒤에 이름을 바꾼다. pg_dump 가 도중에 죽으면 반쪽 파일이 남는데, 그것이 정식 이름이면
// 되돌리기(latestDump)가 그것을 최신으로 고른다. 머리(PGDMP)도 잰다 - 0으로 끝나고 빈 파일을 낸 실행을 믿지 않는다.
// 마지막으로 seal 을 잰다 - 실행기가 잠금을 잃은 동안 뜬 덤프는 이름이 붙지 않는다(닫힌 실패).
func dumpPG(ctx context.Context, t Target, dir, bin, ledger string, from int, seal func(context.Context) error, log *slog.Logger) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("판올림 전 덤프 자리가 없다(--backup-dir) - Postgres 원장은 덤프 없이 판을 올리지 않는다")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("백업 자리를 만들지 못했다(%q): %w", clip(dir, 200), err)
	}
	dst := filepath.Join(dir, dumpName(ledger, from, time.Now()))
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("같은 이름의 덤프가 이미 있다(%q) - 1초 안에 두 번 돌았다. 잠시 뒤 다시 돌려라", clip(dst, 200))
	}
	env, err := libpqEnv(t, os.Environ())
	if err != nil {
		return "", fmt.Errorf("%w - 덤프 없이 판을 올리지 않는다", err)
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", errors.New(pgDumpFault(err, ""))
	}
	// 임시 이름은 실행마다 다르다. 잠금을 잃은 실행기의 pg_dump 가 아직 도는 동안 다음 실행기가 같은 초에 떠도 서로의
	// 반쪽 파일을 덮거나 지우지 않는다.
	f, err := os.CreateTemp(dir, filepath.Base(dst)+".*.part")
	if err != nil {
		return "", fmt.Errorf("백업 자리에 덤프를 쓰지 못한다(%q): %w", clip(dir, 200), err)
	}
	part := f.Name()
	_ = f.Close()
	args := pgDumpArgs(path, part)
	cmd := exec.CommandContext(ctx, path, args[1:]...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		_ = os.Remove(part)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", errors.New(pgDumpFault(err, stderr.String()))
	}
	if err := checkDumpHead(part); err != nil {
		_ = os.Remove(part)
		return "", err
	}
	if err := seal(ctx); err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("덤프에 최종 이름을 붙이지 않는다 - %w. 판을 올리지 않았다. 다시 돌려라", err)
	}
	if err := os.Rename(part, dst); err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("덤프 이름을 바꾸지 못했다(%q): %w", clip(dst, 200), err)
	}
	log.Info("판올림 전 덤프 완료", "backup", dst, "from", from, "duration", time.Since(start).Seconds())
	return dst, nil
}

// checkDumpHead 는 파일이 pg_dump 사용자 정의 형식으로 시작하는지 잰다.
func checkDumpHead(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("덤프를 다시 읽지 못했다(%q): %w", clip(path, 200), err)
	}
	defer f.Close()
	head := make([]byte, len(dumpMagic))
	if _, err := io.ReadFull(f, head); err != nil || string(head) != dumpMagic {
		return fmt.Errorf("덤프가 pg_dump 사용자 정의 형식이 아니다(%q) - 머리가 %s 가 아니다", clip(path, 200), dumpMagic)
	}
	return nil
}

// rollbackPG 는 지금 원장(t)의 덤프 중 가장 최근 것을 고르고 머리를 잰 뒤 복원 절차를 낸다. 원장에 붙어 식별 · 판만
// 읽는다 - 바꾸지 않는다.
//
// ★ 덤프 뒤로 커밋된 판올림이 있어야 절차를 낸다(덤프를 뜬 판 < 지금 판). 덤프를 뜬 판올림이 실패해 되감겼으면 덤프와
// 지금 원장이 같은 판이고, 그 덤프로 되돌리면 덤프 뒤의 쓰기만 잃는다.
func rollbackPG(ctx context.Context, t Target, dir string) (RollbackOutcome, error) {
	if strings.TrimSpace(dir) == "" {
		return RollbackOutcome{}, errors.New("덤프 자리(--backup-dir)가 없다 - Postgres 원장의 판올림 전 덤프를 어디서 찾을지 모른다")
	}
	s, err := openPG(t, slog.New(slog.DiscardHandler), lockTimeout)
	if err != nil {
		return RollbackOutcome{}, fmt.Errorf("되돌릴 원장을 가리려면 그 원장에 붙어야 한다: %w", err)
	}
	defer s.Close()
	led, err := readLedgerFacts(ctx, s.db)
	if err != nil {
		return RollbackOutcome{}, err
	}
	has, current, _, err := readMigrationState(ctx, s.db)
	if err != nil {
		return RollbackOutcome{}, err
	}
	dump, info, err := latestDump(dir, led.ID)
	if err != nil {
		return RollbackOutcome{}, err
	}
	if err := checkDumpHead(dump); err != nil {
		return RollbackOutcome{}, fmt.Errorf("가장 최근 덤프가 깨졌다 - 더 옛 덤프로 조용히 넘어가지 않는다: %w", err)
	}
	if !has || info.From >= current {
		return RollbackOutcome{}, fmt.Errorf("되돌릴 판올림이 없다 - 이 원장의 가장 최근 덤프(%s)는 판 %d 에서 떴고 지금 원장은 판 %d 다. "+
			"그 덤프 뒤로 커밋된 판올림이 없다(판올림이 실패해 되감겼거나 덤프가 실패해 판을 올리지 않았다). "+
			"그 덤프로 되돌리면 덤프 뒤의 쓰기만 잃는다", filepath.Base(dump), info.From, current)
	}
	return RollbackOutcome{Backup: dump, Steps: pgRollbackSteps(info, current), Script: pgRollbackScript(dump, led.Admin)}, nil
}

// pgRollbackSteps 는 복원 스크립트(pgRollbackScript) 앞에 사람에게 내는 설명이다. 순수 함수다.
func pgRollbackSteps(info dumpInfo, current int) string {
	return fmt.Sprintf("판 %d 에서 뜬 덤프(%s)로 원장을 통째로 되돌린다. 지금 원장은 판 %d 이고, 덤프 뒤의 쓰기는 모두 사라진다. "+
		"서버와 원장에 붙은 모든 연결을 멈추고, 원장 클러스터에 초사용자로 붙는 접속을 PGHOST · PGPORT · PGUSER 로 둔 셸에서 "+
		"아래 스크립트를 그대로 돌려라(비밀번호는 PGPASSWORD 나 ~/.pgpass, TLS 는 PGSSLMODE · PGSSLROOTCERT). "+
		"셋 중 하나라도 비면 아무것도 바꾸지 않고 멈춘다. 망가진 DB 는 <원장 DB>_broken 으로 남고, 복원이 도중에 실패하면 "+
		"반쪽 DB 를 지우고 그 이름을 되돌린다. 복원이 끝나면 판 %d 의 바이너리로 서버를 띄운다",
		info.From, info.Stamp, current, info.From)
}

// pgRollbackScript 는 Postgres 원장을 판올림 전 덤프로 되돌리는 셸 스크립트다. 순수 함수다. admin 은 붙을 관리 DB 다(adminDB).
//
// ★ 제자리에 덮지 않고 새 DB 로 복원한다. 망가진 DB 는 이름을 바꿔 증거로 남기고, pg_restore --create 가 덤프에 든 DB
// 정의(로케일 · DB 단위 권한)로 원래 이름의 DB 를 새로 만든다 - 그래서 접속 URL(FD_DB)을 안 바꿔도 된다. 빈 DB 를 손으로
// 만들어 복원하면 로케일 관문과 준비(bootstrap.sql)의 DB 단위 권한(PUBLIC 의 TEMPORARY 회수)을 따로 맞춰야 한다.
//
// ★ 명령마다 대상 클러스터(-h · -p · -U)와 관리 DB(-d)를 적는다. 접속을 libpq 기본값에 맡기면 원장이 원격인데 로컬 기본
// 서버의 같은 이름 DB 를 바꾼다. 값은 환경 변수로만 받는다 - 출력에 비밀번호도 호스트도 없다. 비어 있으면 첫 줄이 멈춘다.
// 이름 바꾸기와 복원은 대상과 다른 관리 DB 에 붙어서 한다(붙은 DB 는 이름을 못 바꾼다).
//
// ★ DB 이름은 덤프 머리에서 읽는다(pg_restore -l). 출력에 싣지 않고(경계가 모호한 접속 URL 에서는 그 이름이 비밀번호
// 조각일 수 있다 - logPGServer 의 ★), 사람이 옮겨 적다 틀릴 자리도 없다. 덤프는 되돌리기가 식별로 고른 이 원장의 것이라
// 그 이름이 곧 원장 이름이다. 이름은 psql 변수(:"db")로 문장에 들어가 따옴표 · 공백이 든 이름도 그대로 맞는다.
//
// ★ 복원이 도중에 실패하면 반쪽 DB 를 지우고 _broken 을 원래 이름으로 되돌린다. 지우는 이름은 이름 바꾸기가 비운 이름이라
// 그 뒤에 그 이름으로 생긴 것은 pg_restore 가 만든 반쪽 DB 뿐이다.
func pgRollbackScript(dump, admin string) string {
	conn := `-h "$PGHOST" -p "$PGPORT" -U "$PGUSER"`
	psql := `psql -X -q -v ON_ERROR_STOP=1 ` + conn + ` -d ` + admin + ` -v db="$db" -v broken="${db}_broken"`
	return strings.Join([]string{
		`#!/bin/sh`,
		`set -eu`,
		`: "${PGHOST:?원장 클러스터의 호스트}" "${PGPORT:?원장 클러스터의 포트}" "${PGUSER:?원장 클러스터의 초사용자}"`,
		`dump=` + shellQuote(dump),
		`db=$(pg_restore -l "$dump" | sed -n 's/^;     dbname: //p')`,
		`: "${db:?덤프 머리에서 DB 이름을 읽지 못했다}"`,
		psql + ` <<'SQL'`,
		`ALTER DATABASE :"db" RENAME TO :"broken";`,
		`SQL`,
		`if ! pg_restore ` + conn + ` -d ` + admin + ` --create --exit-on-error "$dump"; then`,
		`  ` + psql + ` <<'SQL'`,
		`DROP DATABASE IF EXISTS :"db";`,
		`ALTER DATABASE :"broken" RENAME TO :"db";`,
		`SQL`,
		`  echo '복원이 실패했다 - 반쪽 DB 를 지우고 원래 DB 를 제 이름으로 되돌렸다' >&2`,
		`  exit 1`,
		`fi`,
		`echo "복원했다 - 망가진 DB 는 ${db}_broken 으로 남았다"`,
	}, "\n")
}

// shellQuote 는 s 를 POSIX 셸의 홑따옴표 낱말로 싼다. 순수 함수다.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pgFailureHint 는 Postgres 판올림이 실패했을 때 오류에 붙이는 다음 수다. 순수 함수다. 복원 절차 자체는 되돌리기
// (`fd migrate --rollback`)가 원장 식별과 판을 재고 낸다 - 여기서 따로 지으면 절차가 두 벌이 된다.
func pgFailureHint(dump string) string {
	if strings.TrimSpace(dump) == "" {
		return "이번 적용은 덤프를 뜨지 않았다(빈 DB 에 기준선을 깔았다) - 되돌릴 덤프가 없다."
	}
	return fmt.Sprintf("판올림 전 덤프는 %s 이다. 이미 커밋된 단까지 통째로 물리려면 같은 접속 URL 과 덤프 자리로 "+
		"`fd migrate --rollback` 을 돌려 복원 절차를 받아라.", clip(dump, 200))
}
