package store

import (
	"bufio"
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
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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
// ★ 덤프 한 번에는 기한이 있다(DumpTimeout). 덤프가 도는 동안 업무 쓰기는 전역 잠금에 서므로, 덤프가 멈추면(백업 볼륨이
// 응답을 멈추면 pg_dump 도, 그 파일을 읽는 실행기도 함께 선다) 기한이 곧 업무 쓰기가 막히는 최악의 길이다. 같은 기한이
// 스냅숏 트랜잭션의 서버 상한(snapshotTxLimits)과 pg_dump 자식의 문맥(backupPG)에 걸린다.
//
// ★ pg_dump 는 실행기 세션이 내보낸 스냅숏을 본다(dumpSnapshot, pg_dump --snapshot). 호스트 이름 하나가 주 서버와 지연
// 복제본으로 갈리면 세션은 주 서버를 잠그는데 pg_dump 는 복제본에 붙을 수 있다. 스냅숏은 내보낸 서버 · DB 에만 있으므로
// 그때 pg_dump 가 실패한다(닫힌 실패). 원장 식별과 판도 그 스냅숏에서 재므로 덤프 이름과 내용이 한 시점이다.
//
// ★ 덤프 이름이 원장을 가린다(ledgerIDSQL). 원장 여럿이 같은 백업 자리를 써도 되돌리기는 지금 붙은 원장의 덤프만
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

// DefaultDumpTimeout 은 판올림 전 덤프 한 번의 기한 기본값이다(MigrateOptions.DumpTimeout 이 0 일 때).
//
// ★ 30분인 까닭. 기한은 정상 덤프를 끊지 않을 만큼 길어야 하고, 멈춘 덤프가 업무 쓰기를 막는 최악의 길이라 끝이 있어야
// 한다. 원장은 항목 · 판단 · 사건의 글 행이라 오래 쓴 원장도 압축 덤프가 GB 를 넘기 어렵고, 그것을 느린 볼륨(초당 수 MB)에
// 써도 몇 분이다. 30분은 그 위로 몇 배를 남긴다. 판올림은 사람이 기동 전에 돌리는 정비라 그 안에 멈춘 덤프를 알아챌 수 있다.
// 정상 덤프가 더 걸리는 원장은 FD_DUMP_TIMEOUT 으로 늘린다.
const DefaultDumpTimeout = 30 * time.Minute

// 받는 덤프 기한의 범위다. 아래: 기한은 밀리초로 서버에 걸리고(snapshotTxLimits) 0 은 Postgres 에게 「상한 없음」이다 - 1초
// 아래는 받지 않는다. 위: 두 설정은 밀리초 int 라 그 최댓값(약 24일)을 넘으면 SET 이 실패한다.
const (
	minDumpTimeout = time.Second
	maxDumpTimeout = math.MaxInt32 * time.Millisecond
)

// dumpTimeout 은 이번 판올림의 덤프 기한이다: 0 이면 DefaultDumpTimeout, 범위 밖이면 거절한다.
func (o MigrateOptions) dumpTimeout() (time.Duration, error) {
	d := o.DumpTimeout
	if d == 0 {
		return DefaultDumpTimeout, nil
	}
	if d < minDumpTimeout || d > maxDumpTimeout {
		return 0, fmt.Errorf("덤프 기한(FD_DUMP_TIMEOUT)이 %s 다 - %s 이상 %s 이하여야 한다", d, minDumpTimeout, maxDumpTimeout)
	}
	return d, nil
}

// dumpDeadline 은 덤프 기한을 넘은 것이다. 덤프 창의 문맥(backupPG)이 이 원인으로 끝난다 - 부른 쪽의 취소와 가른다.
type dumpDeadline struct{ limit time.Duration }

func (e dumpDeadline) Error() string {
	return "덤프 기한(" + e.limit.String() + ", FD_DUMP_TIMEOUT)을 넘었다"
}

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

// ledgerIDSQL 은 원장 식별을 내는 SQL 식이다: 클러스터(pg_control_system 의 system_identifier)와 DB 이름을 NUL 하나로
// 이은 바이트의 sha256 16진 앞 16글자. sysid · name 은 그 둘을 내는 text 식이다. 순수 함수다.
//
// ★ 식별의 정의는 이것 하나다. 이전 실행기(readLedgerFacts)와 되돌리기 스크립트(pgRollbackScript)가 같은 식을 서버에서
// 돌린다 - 스크립트는 Go 없이 셸과 psql 로만 돌므로 정의가 서버 쪽에 있어야 한 벌이다. 이름은 DB 의 서버 인코딩 그대로의
// 바이트로 잰다(convert_to 의 목적지가 서버 인코딩이면 바꾸지 않는다). 원장 DB 와 관리 DB 의 인코딩이 달라도 같은 이름은
// 같은 바이트다.
//
// ★ 복원해도 그대로다 - 같은 클러스터에 같은 이름으로 되살리므로(pg_restore --create) 되살린 원장의 덤프도 같은 원장으로
// 묶인다. 다른 클러스터의 같은 이름 DB, 같은 클러스터의 다른 DB 는 다른 식별이다. DB 이름은 해시로만 싣는다(경계가 모호한
// 접속 URL 에서는 그 이름이 비밀번호 조각일 수 있다 - logPGServer 의 ★). system_identifier 가 섞여 이름을 거꾸로 못 찾는다.
//
// ★ 받아들인 예외: 운영 클러스터를 물리 복제해(pg_basebackup 등) 승격한 독립 시험계는 system_identifier 도 DB 이름도
// 같아 식별이 같다. 그래서 **배포마다 덤프 자리(--backup-dir · FD_BACKUP_DIR)를 따로 둬야 한다** - 같은 자리를 쓰면
// 운영의 되돌리기가 시험계의 덤프를 고를 수 있다. 식별로는 둘을 못 가른다(README 의 설정 표).
//
// ★ convert_to 는 이름 바이트가 서버 인코딩으로 맞는지 잰다. 공유 카탈로그(pg_database)에는 다른 인코딩의 DB 에서 지은
// 이름이 그 바이트 그대로 있어, 이 식을 카탈로그 전체에 돌리면 UTF-8 로 못 읽는 남의 이름 하나에 질의가 통째로 실패한다.
// 그래서 스크립트는 원장 DB 의 OID 로 후보를 먼저 좁히고 그 행에만 이 식을 돌린다(pgRollbackScript).
func ledgerIDSQL(sysid, name string) string {
	return `left(encode(sha256(convert_to(` + sysid + `, current_setting('server_encoding')) || decode('00', 'hex') || ` +
		`convert_to(` + name + `, current_setting('server_encoding'))), 'hex'), ` + strconv.Itoa(ledgerIDLen) + `)`
}

// adminDB 는 되돌리기 스크립트가 붙을 관리 DB 다. 대상과 달라야 한다 - 붙은 DB 는 이름을 바꿀 수도 지울 수도 없다. 순수 함수다.
func adminDB(database string) string {
	if database == "postgres" {
		return "template1"
	}
	return "postgres"
}

// ledgerFacts 는 지금 붙은 원장의 식별과 관리 DB 다. name 은 카탈로그의 DB 이름(current_database)이고 대조에만 쓴다 -
// 오류 · 출력 · 스크립트에 싣지 않는다(ledgerIDSQL 의 ★).
type ledgerFacts struct {
	ID    string
	Admin string
	// OID 는 원장 DB 의 pg_database.oid 다. 되돌리기 스크립트가 카탈로그에서 후보를 좁히는 열쇠다(pgRollbackScript). 복원하면
	// 새 DB 라 바뀐다 - 스크립트에 싣는 값은 복원 **전** 원장의 것이다.
	OID  int64
	name string
}

// readLedgerFacts 는 q 가 붙은 원장의 ledgerFacts 를 읽는다.
func readLedgerFacts(ctx context.Context, q Reader) (ledgerFacts, error) {
	var led ledgerFacts
	if err := q.QueryRowContext(ctx, `SELECT `+ledgerIDSQL("system_identifier::text", "current_database()::text")+
		`, current_database(), (SELECT oid::bigint FROM pg_catalog.pg_database WHERE datname = current_database())
		   FROM pg_catalog.pg_control_system()`).Scan(&led.ID, &led.name, &led.OID); err != nil {
		return ledgerFacts{}, fmt.Errorf("원장 식별(system_identifier)을 읽지 못했다: %w", err)
	}
	led.Admin = adminDB(led.name)
	return led, nil
}

// libpqTargetVars 는 libpq 가 어느 서버 · DB 에 누구로 붙을지를 정하는 환경 변수다.
//
// ★ 덤프와 되돌리기가 한 벌로 쓴다. 덤프(libpqEnv)는 물려받은 PG 변수를 다 지우고(이것들이 다 든다) 서버의 해석으로 다시
// 쓴다. 되돌리기 스크립트(pgRollbackScript)는 사람이 둔 PGHOST · PGPORT · PGUSER(scriptTarget) 말고는 이것을 첫머리에서
// 지운다 - PGHOST 가 맞아도 셸에 남은 PGHOSTADDR · PGSERVICE 가 다른 서버를 가리키면 그곳의 같은 이름 DB 를 바꾼다.
// 접속을 지키는 변수(PGPASSWORD · PGSSLMODE 등)는 대상을 바꾸지 않으므로 스크립트가 남긴다.
var libpqTargetVars = []string{"PGHOST", "PGHOSTADDR", "PGPORT", "PGDATABASE", "PGUSER", "PGSERVICE",
	"PGSERVICEFILE", "PGSYSCONFDIR", "PGTARGETSESSIONATTRS", "PGLOADBALANCEHOSTS"}

// scriptTarget 은 되돌리기 스크립트가 사람에게서 받는 대상 클러스터다(libpqTargetVars 의 부분).
var scriptTarget = []string{"PGHOST", "PGPORT", "PGUSER"}

// scriptClears 는 되돌리기 스크립트가 첫머리에서 지우는 변수다: libpqTargetVars 가운데 scriptTarget 이 아닌 것. 순수 함수다.
func scriptClears() []string {
	var out []string
	for _, v := range libpqTargetVars {
		if !slices.Contains(scriptTarget, v) {
			out = append(out, v)
		}
	}
	return out
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
// ★ 물려받은 환경에서 PG 로 시작하는 변수를 다 뺀다(대상을 정하는 libpqTargetVars 가 다 든다). pgx 는 해석할 때 이미 그것들(PGPASSWORD · PGSSLMODE · .pgpass 등)을
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
// --snapshot 은 실행기 세션이 내보낸 스냅숏이다(dumpSnapshot) - 다른 서버 · DB 에 붙으면 그것을 못 찾아 실패한다.
// 스냅숏 식별은 비밀이 아니다(트랜잭션 번호와 차례다).
// --lock-wait-timeout 은 pg_dump 가 덤프 첫머리에 표마다 잡는 잠금을 기다리는 상한이다. 없으면 원장 표에 긴 잠금을 쥔
// 연결 하나에 덤프가 덤프 기한(DumpTimeout)까지 서고, 실행기는 그동안 두 잠금을 쥔 채 업무 쓰기를 모두 lock_timeout 으로
// 돌려보낸다. 값은 서버 연결의 lock_timeout 과 같은 원천(pgLimits.lock)이다 - 업무 쓰기 하나가 잠금을 기다리는 상한과 같다.
func pgDumpArgs(bin, file, snapshot string, lockWait time.Duration) []string {
	return []string{bin, "--format=custom", "--no-password", "--snapshot=" + snapshot,
		"--lock-wait-timeout=" + millis(lockWait), "--file=" + file}
}

// pgDumpFault 는 pg_dump 의 실패를 고정 문구로 가른다. 순수 함수다. stderr 는 가르는 데만 쓰고 싣지 않는다.
func pgDumpFault(runErr error, stderr string) string {
	var ee *exec.ExitError
	var dl dumpDeadline
	code := "?"
	switch {
	case errors.Is(runErr, exec.ErrNotFound), errors.Is(runErr, fs.ErrNotExist):
		return "pg_dump 를 찾지 못했다 - PATH 에 서버와 같거나 높은 큰 판의 pg_dump 를 두거나 실행 파일 자리를 줘라"
	case errors.As(runErr, &dl):
		return "pg_dump 를 멈췄다: " + dl.Error() + " - 백업 자리가 응답하는지 확인하고, 정상 덤프가 그보다 오래 걸리는 " +
			"원장이면 FD_DUMP_TIMEOUT 을 늘려라"
	case errors.Is(runErr, context.DeadlineExceeded), errors.Is(runErr, context.Canceled):
		return "pg_dump 가 끝나기 전에 문맥이 끝났다"
	case errors.As(runErr, &ee):
		code = strconv.Itoa(ee.ExitCode())
	}
	l := strings.ToLower(pgDumpErrorLines(stderr))
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
	// --lock-wait-timeout 은 표 잠금을 잡는 동안 statement_timeout 으로 걸린다.
	case strings.Contains(l, "statement timeout"), strings.Contains(l, "lock timeout"):
		why = "표 잠금을 기다리다 상한(--lock-wait-timeout, 서버 연결의 lock_timeout 과 같은 값)을 넘었다 - 원장 표에 긴 잠금을 쥔 연결이 있다"
	// 권한 거절을 스냅숏보다 먼저 가른다 - 거절 문구가 인용하는 표 이름이 snapshot 일 수 있다(원장에 그 표가 있다).
	case strings.Contains(l, "permission denied"):
		why = "읽을 권한이 없는 객체가 있다(이전 실행기는 소유 역할이나 초사용자로 붙어야 한다)"
	// 스냅숏을 못 가져왔다: 없는 식별(다른 서버 · 복제본) · 다른 DB · 내보낸 트랜잭션이 끝났다(실행기 세션이 끊겼다).
	case strings.Contains(l, "snapshot"):
		why = "실행기가 내보낸 스냅숏을 가져오지 못했다(pg_dump 가 실행기와 다른 서버 · DB 에 붙었거나 실행기 세션이 끊겼다)"
	case strings.Contains(l, "could not connect"), strings.Contains(l, "connection refused"),
		strings.Contains(l, "could not translate host name"):
		why = "서버에 붙지 못했다"
	default:
		why = "분류하지 못한 실패"
	}
	return "pg_dump 가 실패했다(종료 코드 " + code + "): " + why +
		" - pg_dump 의 문구는 접속 세부를 인용해 싣지 않는다"
}

// pgDumpErrorLines 는 pg_dump stderr 에서 분류에 쓸 줄만 남긴다: detail · hint 줄을 뺀다. 순수 함수다.
//
// ★ detail 줄은 실패한 질의를 그대로 인용한다 - LOCK TABLE 은 원장의 표 목록 전부를 싣고, 원장에는 snapshot 이라는 표가
// 있다. 그 줄까지 보면 표를 잠그다 난 실패(잠금 표 한도 등)가 「스냅숏을 못 가져왔다」로 갈린다.
func pgDumpErrorLines(stderr string) string {
	var keep []string
	for _, ln := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(ln, "pg_dump: detail:") || strings.HasPrefix(ln, "pg_dump: hint:") {
			continue
		}
		keep = append(keep, ln)
	}
	return strings.Join(keep, "\n")
}

// dumpSnapshot 은 판올림 전 덤프가 볼 스냅숏이다: 실행기 세션 연결에서 연 REPEATABLE READ 읽기 전용 트랜잭션이 내보낸
// 식별(id)과, 그 스냅숏에서 잰 원장 식별 · 판이다. end 가 트랜잭션을 닫는다(여러 번 불러도 한 번만 닫는다).
type dumpSnapshot struct {
	id      string
	ledger  ledgerFacts
	version int
	tx      *txConn
	ended   bool
	endErr  error
}

func (d *dumpSnapshot) end() error {
	if !d.ended {
		d.ended = true
		d.endErr = d.tx.Rollback()
	}
	return d.endErr
}

// snapshotTxLimits 는 덤프 스냅숏 트랜잭션이 첫머리에서 거는 시간 상한이다: 유휴 상한과 transaction_timeout 을 덤프 기한
// (limit)으로 바꾼다. 순수 함수다.
//
// ★ pg_dump 를 기다리는 동안 이 트랜잭션은 문장 없이 열려 있다(idle in transaction). 연결 시작 인자의
// idle_in_transaction_session_timeout(idleInTxTimeout)이 그대로면 그 시간을 넘는 정상 덤프 도중에 Postgres 가 이 연결을
// 끊고, 세션 잠금 둘도 함께 사라진다 - 덤프를 다 떠도 봉인(sealDump)에서 실패해 판을 못 올린다. 서버 설정의
// transaction_timeout 도 같은 자리에서 끊는다. 그렇다고 둘을 풀면(0) 덤프가 멈춘 날 세션 잠금 둘이 끝없이 남고 업무 쓰기가
// 모두 lock_timeout 으로 돌아간다. 그래서 둘 다 덤프 기한으로 바꾼다: 실행기 프로세스나 그 파일 입출력이 서 있어도 기한이
// 지나면 **서버가 스스로** 이 연결을 끊고, 세션 잠금은 연결과 함께 풀린다.
//
// ★ 덤프 창의 문맥(backupPG)은 이 트랜잭션을 열기 전에 같은 기한으로 시작한다. 그래서 실행기가 살아 있으면 서버보다 먼저
// pg_dump 를 멈추고 스스로 닫는다. 서버의 끊기는 실행기가 서 있을 때의 안전망이다.
//
// ★ transaction_timeout 은 0 으로 한 번 끄고 다시 건다. 트랜잭션 안에서 값을 바꾸면 Postgres 는 시계가 꺼져 있을 때만 새
// 값으로 켜고, 이미 도는 시계(서버 · DB 설정이 있으면 BEGIN 에서 켜진다)는 그대로 둔다 - 기한만 쓰면 서버 설정의 시계가
// 이긴다. 끄고 켜면 시계가 이 자리에서 기한으로 다시 선다.
//
// ★ LOCAL 이다 - 이 트랜잭션이 닫히면(dumpSnapshot.end) 연결 시작 인자의 값으로 돌아간다. 실행기 세션 전체(SET)에서
// 바꾸지 않는다: 그 연결은 release 뒤 Store 의 풀로 돌아가 업무 트랜잭션을 받을 수 있고, 같은 연결의 증분 트랜잭션은 쉬지
// 않고 도는 우리 코드라 원래 안전망이 그대로 맞다. 오래 노는 것은 덤프를 기다리는 이 트랜잭션 하나다.
func snapshotTxLimits(limit time.Duration) []string {
	ms := millis(limit)
	return []string{
		`SET LOCAL idle_in_transaction_session_timeout = ` + ms,
		`SET LOCAL transaction_timeout = 0`,
		`SET LOCAL transaction_timeout = ` + ms,
	}
}

// exportSnapshot 은 실행기 세션 연결에서 스냅숏을 내보낸다(pg_export_snapshot). 첫 문장이 내보내기라 내보낸 스냅숏이 곧
// 이 트랜잭션의 스냅숏이고, 뒤의 원장 식별 · 판 읽기도 그것을 본다 - 덤프 내용과 덤프 이름(식별 · 판)이 한 시점이다.
//
// ★ 세션이 쥔 두 잠금(세션 단위 권고 잠금)은 이 트랜잭션과 따로 산다 - 닫아도(ROLLBACK) 안 풀린다. 다만 같은 연결이라
// 이 트랜잭션이 열린 동안 세션의 다른 질의 · 증분 트랜잭션은 그 안으로 섞인다. 그래서 봉인 전에 닫고(backupPG), 판올림 전
// 덤프를 나가기 전에 반드시 닫는다.
//
// ★ 이 트랜잭션은 시간 상한을 덤프 기한(limit)으로 바꾼다(snapshotTxLimits). 그 까닭과 자리는 그 값의 ★.
func (s *Store) exportSnapshot(ctx context.Context, sess *migrateSession, limit time.Duration) (*dumpSnapshot, error) {
	tx, err := s.begin(ctx, txSpec{kind: txRead, on: sess})
	if err != nil {
		return nil, fmt.Errorf("덤프 스냅숏 트랜잭션을 열지 못했다: %w", err)
	}
	snap := &dumpSnapshot{tx: tx}
	fail := func(err error) (*dumpSnapshot, error) {
		_ = snap.end()
		return nil, err
	}
	// SET 은 스냅숏을 잡지 않는다 - 아래 내보내기가 여전히 이 트랜잭션의 스냅숏을 처음 잡는 문장이다.
	for _, q := range snapshotTxLimits(limit) {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fail(fmt.Errorf("덤프 스냅숏 트랜잭션에 덤프 기한을 걸지 못했다: %w", err))
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT pg_export_snapshot()`).Scan(&snap.id); err != nil {
		return fail(fmt.Errorf("덤프 스냅숏을 내보내지 못했다: %w", err))
	}
	if snap.ledger, err = readLedgerFacts(ctx, tx); err != nil {
		return fail(err)
	}
	if _, snap.version, _, err = readMigrationState(ctx, tx); err != nil {
		return fail(err)
	}
	return snap, nil
}

// backupPG 는 이전 실행기의 판올림 전 덤프다(openMigrator 가 run.backup 으로 건다). 실행기 세션에서 스냅숏을 내보내고
// pg_dump 가 그것을 보게 하며, 다 뜬 뒤에는 스냅숏 트랜잭션을 닫고 같은 세션에서 잠금과 판을 다시 재야 이름을 붙인다(sealDump).
//
// 잠금 · 스냅숏 · 증분의 차례(한 연결):
//  1. 세션 잠금 둘(자기 · 전역 업무)을 잡고 판을 읽는다(migrateTo).
//  2. 덤프 창을 연다: 3~5 가 덤프 기한(limit)을 넘으면 문맥이 dumpDeadline 으로 끝난다.
//  3. RR 읽기 전용 트랜잭션을 열어 서버 상한을 덤프 기한으로 바꾸고(snapshotTxLimits) 스냅숏을 내보내고, 그 안에서 원장
//     식별과 판을 잰다. 판이 1의 것과 달라야 할 까닭이 없다(잠금 안이다) - 다르면 멈춘다.
//  4. pg_dump --snapshot 이 그 스냅숏을 가져와 뜬다. 내보낸 트랜잭션이 열려 있어야 가져온다. 기한이 지나면 자식을 죽인다.
//  5. 다 뜨면 머리의 DB 이름을 원장의 실제 이름과 맞추고(checkDumpDatabase) 트랜잭션을 닫은 뒤(봉인) 세션 잠금 둘과 판을
//     다시 잰다. 그 사이 기한이 지나 서버가 연결을 끊었으면 여기서 실패한다(닫힌 실패). 그 뒤 덮지 않고 게시한다.
//  6. 증분 트랜잭션은 이 함수가 돌아온 뒤 같은 연결에서 부른 쪽의 문맥으로 연다 - 3의 트랜잭션은 어느 길로 나가도 닫혀 있다.
func (s *Store) backupPG(ctx context.Context, t Target, sess *migrateSession, from int, dir string, limit time.Duration) (string, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, limit, dumpDeadline{limit})
	defer cancel()
	snap, err := s.exportSnapshot(ctx, sess, limit)
	if err != nil {
		return "", err
	}
	defer func() { _ = snap.end() }()
	if snap.version != from {
		return "", fmt.Errorf("덤프 스냅숏의 판(%d)이 실행기가 읽은 판(%d)과 다르다 - 판을 올리지 않는다", snap.version, from)
	}
	return dumpPG(ctx, t, dumpJob{dir: dir, bin: "pg_dump", ledger: snap.ledger, from: from, snapshot: snap.id,
		lockWait: s.pgLim.lock, seal: func(ctx context.Context) error {
			if err := snap.end(); err != nil {
				return fmt.Errorf("덤프 스냅숏 트랜잭션을 닫지 못했다(연결이 끊겼으면 잠금도 사라졌다): %w", err)
			}
			return sealDump(ctx, sess, from)
		}}, s.log)
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

// dumpJob 은 판올림 전 덤프 한 번의 입력이다(backupPG 가 채운다).
type dumpJob struct {
	dir      string        // 덤프 자리
	bin      string        // pg_dump 실행 파일(이름이면 PATH 에서 찾는다)
	ledger   ledgerFacts   // 스냅숏에서 잰 원장: 덤프 이름의 식별(ID)과 덤프 머리에 실려야 할 DB 이름(name)
	from     int           // 덤프를 뜨는 판
	snapshot string        // pg_dump 가 볼 스냅숏(dumpSnapshot)
	lockWait time.Duration // pg_dump 의 표 잠금 대기 상한(pgDumpArgs)
	// seal 이 nil 을 내야 최종 이름을 붙인다(sealDump).
	seal func(context.Context) error
}

// dumpPG 는 판올림 전 덤프를 뜬다. 뜬 파일 경로를 낸다.
//
// ★ 임시 이름에 뜨고 다 뜬 뒤에 최종 이름을 붙인다. pg_dump 가 도중에 죽으면 반쪽 파일이 남는데, 그것이 정식 이름이면
// 되돌리기(latestDump)가 그것을 최신으로 고른다. 머리도 잰다 - 0으로 끝나고 빈 파일을 낸 실행을 믿지 않고, 머리의 DB
// 이름이 원장의 실제 이름과 다른 덤프(checkDumpDatabase 의 ★)는 되돌리기가 거절하므로 지금 판올림을 거절한다. 마지막으로
// seal 을 잰다 - 실행기가 잠금을 잃은 동안 뜬 덤프는 이름이 붙지 않는다(닫힌 실패). 덤프 기한을 넘겨 서버가 연결을 끊은
// 것도 같은 길이고, 그때는 오류가 기한을 말한다(dumpDeadline).
//
// ★ 게시는 덮지 않는다(link, 있으면 실패). 봉인 직후 세션이 끊기고 같은 초에 시작한 다음 실행기가 먼저 게시했으면, 늦게
// 돌아온 이 게시가 rename 이었다면 새 덤프를 말없이 갈아 끼운다.
func dumpPG(ctx context.Context, t Target, j dumpJob, log *slog.Logger) (string, error) {
	dir := j.dir
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("판올림 전 덤프 자리가 없다(--backup-dir) - Postgres 원장은 덤프 없이 판을 올리지 않는다")
	}
	if j.snapshot == "" {
		return "", errors.New("실행기가 내보낸 스냅숏 없이 덤프를 뜨지 않는다 - pg_dump 가 실행기와 같은 서버를 본다는 보장이 없다")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("백업 자리를 만들지 못했다(%q): %w", clip(dir, 200), err)
	}
	dst := filepath.Join(dir, dumpName(j.ledger.ID, j.from, time.Now()))
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("같은 이름의 덤프가 이미 있다(%q) - 1초 안에 두 번 돌았다. 잠시 뒤 다시 돌려라", clip(dst, 200))
	}
	env, err := libpqEnv(t, os.Environ())
	if err != nil {
		return "", fmt.Errorf("%w - 덤프 없이 판을 올리지 않는다", err)
	}
	path, err := exec.LookPath(j.bin)
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
	args := pgDumpArgs(path, part, j.snapshot, j.lockWait)
	// 자식은 덤프 창의 문맥(backupPG 의 기한)에 묶인다. 문맥이 끝나면 죽이고, 그 자손이 stderr 를 쥐고 있어도 WaitDelay
	// 뒤에는 기다리지 않는다. 파일 입출력에 묶여 죽지도 못하는 자식은 이것으로 못 푼다 - 그때 잠금은 서버가 푼다(snapshotTxLimits).
	cmd := exec.CommandContext(ctx, path, args[1:]...)
	cmd.Env = env
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		_ = os.Remove(part)
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		return "", errors.New(pgDumpFault(err, stderr.String()))
	}
	if err := checkDumpDatabase(part, j.ledger.name); err != nil {
		_ = os.Remove(part)
		if errors.Is(err, errDumpNameDiffers) {
			return "", fmt.Errorf("%w - pg_dump 가 접속에 쓴 DB 이름이 원장의 원래 이름이 아니다(연결 중개기의 DB 별칭 등). "+
				"이 덤프는 되돌리기가 거절하므로 판을 올리지 않았다. 별칭이 아니라 원장 DB 의 원래 이름으로 접속해 다시 돌려라"+
				"(이름은 싣지 않는다)", err)
		}
		return "", err
	}
	if err := j.seal(ctx); err != nil {
		_ = os.Remove(part)
		var dl dumpDeadline
		if c := context.Cause(ctx); errors.As(c, &dl) {
			err = fmt.Errorf("%w(기한이 지나면 서버가 실행기 연결을 끊고 잠금을 푼다) - %w", dl, err)
		}
		return "", fmt.Errorf("덤프에 최종 이름을 붙이지 않는다 - %w. 판을 올리지 않았다. 다시 돌려라", err)
	}
	if err := os.Link(part, dst); err != nil {
		_ = os.Remove(part)
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("같은 이름의 덤프가 이미 있다(%q) - 다른 실행기가 먼저 게시했다. 덮지 않는다. 판을 올리지 않았다. 다시 돌려라", clip(dst, 200))
		}
		return "", fmt.Errorf("덤프에 최종 이름을 붙이지 못했다(%q): %w", clip(dst, 200), err)
	}
	if err := os.Remove(part); err != nil {
		// 같은 내용의 두 번째 이름일 뿐이다(.part 는 되돌리기가 안 고른다).
		log.Warn("덤프의 임시 이름을 지우지 못했다", "part", part, "error", err.Error())
	}
	log.Info("판올림 전 덤프 완료", "backup", dst, "from", j.from, "duration", time.Since(start).Seconds())
	return dst, nil
}

// errDumpNameDiffers 는 덤프 머리의 DB 이름이 원장의 실제 이름과 다른 것이다(checkDumpDatabase).
var errDumpNameDiffers = errors.New("덤프 머리에 실린 DB 이름이 원장의 실제 이름(current_database())과 다르다")

// checkDumpDatabase 는 path 가 pg_dump 사용자 정의 형식이고 그 머리에 실린 DB 이름이 want(원장의 current_database())와
// 바이트까지 같은지 잰다. 다르면 errDumpNameDiffers 다. 덤프 직후(dumpPG)와 되돌리기(rollbackPG)가 같은 대조를 쓴다.
//
// ★ 머리의 이름은 pg_dump 가 **접속에 쓴 이름**(libpq 의 PQdb)이고, pg_restore --create 가 만드는 DB 는 덤프의 DATABASE
// 항목, 곧 pg_dump 가 붙은 DB 의 **실제 이름**(current_database())이다. 연결 중개기(PgBouncer 등)의 DB 별칭으로 붙으면
// 머리에는 별칭이, DATABASE 항목에는 원래 이름이 실린다. 덤프 직후의 want 는 실행기 스냅숏에서 잰 current_database() 다 -
// pg_dump 는 그 스냅숏을 가져왔으므로(같은 DB 에서만 가져온다) 그것이 곧 DATABASE 항목의 이름이다. pg_restore -l 로
// DATABASE 항목을 읽지 않는 까닭은 dumpDatabase 의 ★.
func checkDumpDatabase(path, want string) error {
	got, err := dumpDatabase(path)
	if err != nil {
		return err
	}
	if got != want {
		return errDumpNameDiffers
	}
	return nil
}

// dumpDatabase 는 pg_dump 사용자 정의 형식 덤프의 머리에 실린 DB 이름(pg_dump 가 붙은 DB)을 바이트 그대로 읽는다.
//
// ★ pg_restore -l 을 쓰지 않는다. 그 목록은 사람이 읽는 표시라 DB 이름의 줄바꿈을 공백으로 바꾼다 - 그 줄을 이름으로 쓰면
// 공백이 든 다른 DB 를 고를 수 있다. 머리는 pg_dump 의 WriteHead 차례 그대로다: PGDMP, 판(큰 · 작은 · 고친) 바이트, int
// 크기, offset 크기, 형식(1 = 사용자 정의), 압축(1.15 부터 1바이트, 그 전은 int), 만든 시각 int 일곱, DB 이름 str. int 는
// 부호 바이트 하나와 int 크기만큼의 작은 끝 바이트, str 은 길이 int 와 그만큼의 바이트다.
func dumpDatabase(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("덤프를 다시 읽지 못했다(%q): %w", clip(path, 200), err)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	bad := func(why string) error {
		return fmt.Errorf("덤프가 pg_dump 사용자 정의 형식이 아니다(%q) - %s", clip(path, 200), why)
	}
	head := make([]byte, len(dumpMagic)+6)
	if _, err := io.ReadFull(r, head); err != nil || string(head[:len(dumpMagic)]) != dumpMagic {
		return "", bad("머리가 " + dumpMagic + " 가 아니다")
	}
	vmaj, vmin, intSize, format := head[5], head[6], int(head[8]), head[10]
	if vmaj != 1 || vmin < 10 || intSize < 1 || intSize > 8 || format != 1 {
		return "", bad(fmt.Sprintf("머리를 모른다(판 %d.%d, int %d바이트, 형식 %d)", vmaj, vmin, intSize, format))
	}
	readInt := func() (int64, error) {
		b := make([]byte, 1+intSize)
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, err
		}
		var v int64
		for i := intSize; i >= 1; i-- {
			v = v<<8 | int64(b[i])
		}
		if b[0] != 0 {
			v = -v
		}
		return v, nil
	}
	if vmin >= 15 {
		_, err = r.ReadByte()
	} else {
		_, err = readInt()
	}
	for i := 0; i < 7 && err == nil; i++ {
		_, err = readInt()
	}
	if err != nil {
		return "", bad("머리가 잘렸다")
	}
	n, err := readInt()
	if err != nil || n < 0 || n > 4096 {
		return "", bad("DB 이름을 읽지 못했다")
	}
	name := make([]byte, n)
	if _, err := io.ReadFull(r, name); err != nil {
		return "", bad("머리가 잘렸다")
	}
	return string(name), nil
}

// rollbackPG 는 지금 원장(t)의 덤프 중 가장 최근 것을 고르고 머리를 잰 뒤 복원 절차를 낸다. 원장에 붙어 식별 · 판 ·
// 이름 · OID 만 읽는다 - 바꾸지 않는다.
//
// ★ 덤프 뒤로 커밋된 판올림이 있어야 절차를 낸다(덤프를 뜬 판 < 지금 판). 덤프를 뜬 판올림이 실패해 되감겼으면 덤프와
// 지금 원장이 같은 판이고, 그 덤프로 되돌리면 덤프 뒤의 쓰기만 잃는다.
//
// ★ 덤프 머리에 실린 DB 이름이 이 원장 연결의 current_database() 와 바이트까지 같아야 한다(checkDumpDatabase). pg_restore
// --create 는 덤프의 DATABASE 항목 이름으로 DB 를 만든다 - 다르면 되살린 DB 가 원장 자리에 안 선다.
//
// ★ 덤프 자리는 절대 경로로 바꿔 싣는다. 스크립트는 사람이 다른 디렉토리에서 돌릴 수 있다.
func rollbackPG(ctx context.Context, t Target, dir string) (RollbackOutcome, error) {
	if strings.TrimSpace(dir) == "" {
		return RollbackOutcome{}, errors.New("덤프 자리(--backup-dir)가 없다 - Postgres 원장의 판올림 전 덤프를 어디서 찾을지 모른다")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return RollbackOutcome{}, fmt.Errorf("덤프 자리를 절대 경로로 바꾸지 못했다: %w", err)
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
	if err := checkDumpDatabase(dump, led.name); err != nil {
		if errors.Is(err, errDumpNameDiffers) {
			return RollbackOutcome{}, fmt.Errorf("가장 최근 덤프(%s): %w - 덤프 이름의 원장 식별과 내용이 어긋났다. "+
				"이 덤프로 복원하면 다른 이름의 DB 가 생긴다(이름은 싣지 않는다)", filepath.Base(dump), err)
		}
		return RollbackOutcome{}, fmt.Errorf("가장 최근 덤프가 깨졌다 - 더 옛 덤프로 조용히 넘어가지 않는다: %w", err)
	}
	if !has || info.From >= current {
		return RollbackOutcome{}, fmt.Errorf("되돌릴 판올림이 없다 - 이 원장의 가장 최근 덤프(%s)는 판 %d 에서 떴고 지금 원장은 판 %d 다. "+
			"그 덤프 뒤로 커밋된 판올림이 없다(판올림이 실패해 되감겼거나 덤프가 실패해 판을 올리지 않았다). "+
			"그 덤프로 되돌리면 덤프 뒤의 쓰기만 잃는다", filepath.Base(dump), info.From, current)
	}
	sum, err := fileSHA256(dump)
	if err != nil {
		return RollbackOutcome{}, err
	}
	return RollbackOutcome{Backup: dump, Steps: pgRollbackSteps(info, current), Script: pgRollbackScript(dump, sum, led)}, nil
}

// fileSHA256 은 파일 내용의 sha256 16진(소문자 64글자)이다.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("덤프를 다시 읽지 못했다(%q): %w", clip(path, 200), err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("덤프의 sha256 을 재지 못했다(%q): %w", clip(path, 200), err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// pgRollbackSteps 는 복원 스크립트(pgRollbackScript) 앞에 사람에게 내는 설명이다. 순수 함수다.
func pgRollbackSteps(info dumpInfo, current int) string {
	return fmt.Sprintf("판 %d 에서 뜬 덤프(%s)로 원장을 통째로 되돌린다. 지금 원장은 판 %d 이고, 덤프 뒤의 쓰기는 모두 사라진다. "+
		"서버와 원장에 붙은 모든 연결을 멈추고, 원장 클러스터에 초사용자로 붙는 접속을 PGHOST · PGPORT · PGUSER 로 둔 셸에서 "+
		"아래 스크립트를 그대로 돌려라(비밀번호는 PGPASSWORD 나 ~/.pgpass, TLS 는 PGSSLMODE · PGSSLROOTCERT). "+
		"셋 중 하나라도 비거나 호스트가 여럿이면 아무것도 바꾸지 않고 멈춘다. 대상을 바꾸는 다른 변수(PGHOSTADDR · PGSERVICE 등)는 "+
		"스크립트가 지운다. 덤프가 그 자리에 스크립트를 낸 때 그대로 있지 않거나(sha256) pg_restore 가 그것을 못 읽으면, "+
		"또 붙은 클러스터에 이 원장(식별 %s)이 없으면, 곧 다른 서버에 붙었으면 아무것도 바꾸지 않고 멈춘다. "+
		"망가진 DB 는 <원장 DB>%s 로 남고, 복원이 도중에 실패하면 반쪽 DB 를 지우고 그 이름을 되돌린다. "+
		"이 스크립트는 지금 원장 DB(OID)에 묶여 한 번만 돈다 - 복원 뒤에 다시 되돌리려면 `fd migrate --rollback` 으로 새로 받아라. "+
		"복원이 끝나면 판 %d 의 바이너리로 서버를 띄운다",
		info.From, info.Stamp, current, info.Ledger, brokenSuffix, info.From)
}

// brokenSuffix 는 되돌리기가 망가진 원장 DB 에 붙이는 꼬리다. maxRollbackName 은 그 꼬리를 붙여도 이름이 안 잘리는 DB
// 이름의 상한(바이트)이다 - Postgres 의 이름은 63바이트에서 말없이 잘린다(NAMEDATALEN).
const (
	brokenSuffix    = "_broken"
	maxRollbackName = 63 - len(brokenSuffix)
)

// pgRollbackScript 는 Postgres 원장을 판올림 전 덤프로 되돌리는 셸 스크립트다. 순수 함수다. dump 는 덤프의 절대 경로,
// sum 은 스크립트를 낼 때 잰 그 내용의 sha256(fileSHA256), led 는 복원 **전** 원장이다(식별 · 관리 DB · OID).
//
// ★ 제자리에 덮지 않고 새 DB 로 복원한다. 망가진 DB 는 이름을 바꿔 증거로 남기고, pg_restore --create 가 덤프에 든 DB
// 정의(로케일 · DB 단위 권한)로 원래 이름의 DB 를 새로 만든다 - 그래서 접속 URL(FD_DB)을 안 바꿔도 된다. 빈 DB 를 손으로
// 만들어 복원하면 로케일 관문과 준비(bootstrap.sql)의 DB 단위 권한(PUBLIC 의 TEMPORARY 회수)을 따로 맞춰야 한다.
//
// ★ 명령마다 대상 클러스터(-h · -p · -U)와 관리 DB(-d)를 적는다. 접속을 libpq 기본값에 맡기면 원장이 원격인데 로컬 기본
// 서버의 같은 이름 DB 를 바꾼다. 값은 환경 변수로만 받는다 - 출력에 비밀번호도 호스트도 없다. 비어 있거나 호스트가 여럿이면
// 멈춘다(명령마다 다른 서버에 붙을 수 있다). 그 밖에 대상을 바꾸는 libpq 변수는 첫머리에서 지운다(libpqTargetVars).
// 이름 바꾸기와 복원은 대상과 다른 관리 DB 에 붙어서 한다(붙은 DB 는 이름을 못 바꾼다).
//
// ★ 첫 ALTER 전에 덤프를 잰다: 그 경로에 파일이 있고, 내용의 sha256 이 스크립트를 낼 때와 같고, pg_restore -l 이 목록을
// 읽는다. 하나라도 아니면 아무것도 안 바꾸고 멈춘다 - 원장 이름부터 바꾸고 복원에서 덤프가 없다는 것을 알면 그사이 원장이
// 제 이름에 없다. sha256 도구는 있는 것(sha256sum · shasum · openssl)을 쓰고, 셋 다 없으면 값이 안 맞아 멈춘다.
//
// ★ 무엇을 바꾸기 전에 **같은 psql 연결에서** 원장을 대조한다. 카탈로그(pg_database)에서 복원 전 원장의 OID 로 후보를 먼저
// 좁히고(ledgerIDSQL 의 ★ - 남의 이름을 재지 않는다), 그 행의 식별(클러스터의 system_identifier 와 DB 이름)이 led.ID 일
// 때만 그 연결로 이름을 바꾼다. 다른 서버에 붙었으면 OID 가 우연히 같아도 식별이 달라 아무것도 안 바꾸고 멈춘다. 복원이
// 끝나면 원장은 새 OID 의 새 DB 이고 그 OID 는 <이름>_broken 에 남으므로, 같은 스크립트를 다시 돌리면 원장을 못 찾고
// 멈춘다. 실패 갈래도 같다 - 같은 OID 의 행이 <이름>_broken 이고 <이름>의 식별이 led.ID 일 때만 지우고 되돌린다.
//
// ★ DB 이름은 카탈로그에서 psql 변수로 받는다(\gset) - 출력에 싣지 않고(경계가 모호한 접속 URL 에서는 그 이름이 비밀번호
// 조각일 수 있다 - logPGServer 의 ★), 셸을 거치지 않으며, :"db" 로 인용되어 줄바꿈 · 따옴표 · 공백이 든 이름도 바이트 그대로
// 맞는다. 덤프 머리의 이름이 원장 이름과 같은지는 스크립트를 낼 때 잰다(rollbackPG).
//
// ★ 복원이 도중에 실패하면 반쪽 DB 를 지우고 _broken 을 원래 이름으로 되돌린다. 지우는 이름은 이름 바꾸기가 비운 이름이라
// 그 뒤에 그 이름으로 생긴 것은 pg_restore 가 만든 반쪽 DB 뿐이다.
func pgRollbackScript(dump, sum string, led ledgerFacts) string {
	conn := `-h "$PGHOST" -p "$PGPORT" -U "$PGUSER"`
	psql := `psql -X -q -v ON_ERROR_STOP=1 ` + conn + ` -d ` + led.Admin + ` -v ledger=` + led.ID +
		` -v ledger_oid=` + strconv.FormatInt(led.OID, 10)
	stop := func(why string) string { return `DO $$BEGIN RAISE EXCEPTION '` + why + `'; END$$;` }
	n := strconv.Itoa(len(brokenSuffix))
	// cand 는 복원 전 원장 DB 의 OID 로 좁힌 카탈로그 행(많아야 하나)이다. MATERIALIZED 라 바깥의 식별 식이 그 안으로
	// 내려가지 않는다 - 남의 이름에는 식별 식이 닿지 않는다.
	cand := `WITH cand AS MATERIALIZED (SELECT datname::text AS name FROM pg_catalog.pg_database WHERE oid = :'ledger_oid')`
	return strings.Join([]string{
		`#!/bin/sh`,
		`set -eu`,
		`unset ` + strings.Join(scriptClears(), " "),
		`: "${PGHOST:?원장 클러스터의 호스트}" "${PGPORT:?원장 클러스터의 포트}" "${PGUSER:?원장 클러스터의 초사용자}"`,
		`case "$PGHOST$PGPORT" in *,*) echo '원장 클러스터의 호스트 · 포트는 하나씩 적어라 - 여럿이면 명령마다 다른 서버에 붙을 수 있다' >&2; exit 1;; esac`,
		`dump=` + shellQuote(dump),
		`digest() { if command -v sha256sum >/dev/null 2>&1; then sha256sum; elif command -v shasum >/dev/null 2>&1; then shasum -a 256; else openssl dgst -sha256 -r; fi; }`,
		`[ -f "$dump" ] || { echo '덤프가 그 자리에 없다 - 아무것도 바꾸지 않았다' >&2; exit 1; }`,
		`[ "$(digest < "$dump" | cut -c1-64)" = '` + sum + `' ] || { echo '덤프의 sha256 이 스크립트를 낸 때와 다르다(내용이 바뀌었거나 sha256 도구가 없다) - 아무것도 바꾸지 않았다' >&2; exit 1; }`,
		`pg_restore -l "$dump" >/dev/null || { echo 'pg_restore 가 덤프의 목록을 읽지 못한다 - 아무것도 바꾸지 않았다' >&2; exit 1; }`,
		psql + ` <<'SQL'`,
		cand,
		`SELECT count(*) = 1 AS found, coalesce(bool_and(octet_length(name) <= ` + strconv.Itoa(maxRollbackName) + `), false) AS fits,`,
		`       min(name) AS db, min(name) || '` + brokenSuffix + `' AS broken`,
		`  FROM cand, pg_catalog.pg_control_system()`,
		` WHERE ` + ledgerIDSQL("system_identifier::text", "name") + ` = :'ledger' \gset`,
		`\if :found`,
		`\if :fits`,
		`ALTER DATABASE :"db" RENAME TO :"broken";`,
		`\else`,
		stop(`원장 DB 이름이 ` + strconv.Itoa(maxRollbackName) + `바이트를 넘어 ` + brokenSuffix + ` 를 붙이면 잘린다 - 아무것도 바꾸지 않았다. 이름을 손으로 바꿔 두고 복원하라`),
		`\endif`,
		`\else`,
		stop(`이 클러스터에 이 원장(식별 ` + led.ID + `)의 DB 가 없다 - 다른 서버에 붙었거나 이미 이름을 바꿨다. 아무것도 바꾸지 않았다`),
		`\endif`,
		`SQL`,
		`if ! pg_restore ` + conn + ` -d ` + led.Admin + ` --create --exit-on-error "$dump"; then`,
		`  ` + psql + ` <<'SQL'`,
		cand,
		`SELECT count(*) = 1 AS found, min(left(name, -` + n + `)) AS db, min(name) AS broken`,
		`  FROM cand, pg_catalog.pg_control_system()`,
		` WHERE right(name, ` + n + `) = '` + brokenSuffix + `'`,
		`   AND ` + ledgerIDSQL("system_identifier::text", "left(name, -"+n+")") + ` = :'ledger' \gset`,
		`\if :found`,
		`DROP DATABASE IF EXISTS :"db";`,
		`ALTER DATABASE :"broken" RENAME TO :"db";`,
		`\else`,
		stop(`이 클러스터에 이 원장(식별 ` + led.ID + `)의 ` + brokenSuffix + ` DB 가 없다 - 반쪽 DB 를 지우지 않았다. 원장 클러스터에서 손으로 확인하라`),
		`\endif`,
		`SQL`,
		`  echo '복원이 실패했다 - 반쪽 DB 를 지우고 원래 DB 를 제 이름으로 되돌렸다' >&2`,
		`  exit 1`,
		`fi`,
		`echo '복원했다 - 망가진 DB 는 <원장 DB>` + brokenSuffix + ` 로 남았다'`,
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
