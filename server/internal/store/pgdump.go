package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
// Postgres 판은 같은 자리에서 `pg_dump -Fc` 로 백업 자리에 pre-<판>-<시각>.dump 를 뜬다. 되돌리기는 살아 있는 DB 를
// 덮지 않는다 - 그 덤프를 **새 DB** 로 복원하는 절차를 낸다(pgRollbackHint). 증분은 트랜잭션째 되감기므로 실패한 단이
// 반쯤 남는 일은 없고, 덤프가 필요한 것은 끝까지 커밋된 판올림을 통째로 물리는 날이다.
//
// ★ pg_dump 에 접속 정보를 argv 로 넘기지 않는다. argv 는 같은 기계의 다른 사용자에게 `ps` 로 보인다. 연결 정보는
// 서버가 실제로 붙는 해석(pgx.ParseConfig - openPG 와 같은 해석기)에서 꺼내 libpq 환경 변수로만 넘긴다.
// ★ pg_dump 의 오류 문구는 싣지 않는다. 호스트 · 사용자 · DB 이름을 인용하고, 경계가 모호한 접속 URL 에서는 그 자리에
// 비밀번호 조각이 든다(pgFault 의 ★). 알려진 원인만 고정 문구로 가른다(pgDumpFault).

// dumpPrefix · dumpExt 는 덤프 파일 이름의 앞뒤다: pre-<판>-<시각>.dump. <판>은 이번에 올릴 판, <시각>은 BackupSuffix 와
// 같은 폭 고정 UTC 라 같은 판 안에서는 사전순이 곧 시간순이다.
const (
	dumpPrefix = "pre-"
	dumpExt    = ".dump"
	// dumpMagic 은 pg_dump 사용자 정의 형식(-Fc) 파일의 머리 다섯 바이트다.
	dumpMagic = "PGDMP"
	stampLen  = len("20060102T150405Z")
)

// dumpName 은 덤프 파일 이름이다. 순수 함수다.
func dumpName(to int, at time.Time) string {
	return dumpPrefix + strconv.Itoa(to) + "-" + at.UTC().Format("20060102T150405Z") + dumpExt
}

// dumpStamp 는 덤프 파일 이름에서 시각 꼬리를 꺼낸다. 덤프 이름이 아니면 거짓이다. 순수 함수다.
func dumpStamp(name string) (string, bool) {
	if !strings.HasPrefix(name, dumpPrefix) || !strings.HasSuffix(name, dumpExt) {
		return "", false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, dumpPrefix), dumpExt)
	i := strings.LastIndexByte(body, '-')
	if i < 1 || len(body)-i-1 != stampLen {
		return "", false
	}
	if _, err := strconv.Atoi(body[:i]); err != nil {
		return "", false
	}
	if _, err := time.Parse("20060102T150405Z", body[i+1:]); err != nil {
		return "", false
	}
	return body[i+1:], true
}

// latestDump 는 dir 의 덤프 가운데 가장 최근에 뜬 것이다. 판이 아니라 시각으로 고른다 - 판올림이 실패해 같은 판을 다시
// 돌렸으면 나중 덤프가 그 판의 가장 최근 상태다.
func latestDump(dir string) (string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("백업 자리를 읽지 못했다(%q): %w", clip(dir, 200), err)
	}
	type cand struct{ name, stamp string }
	var cs []cand
	for _, e := range ents {
		if st, ok := dumpStamp(e.Name()); ok && e.Type().IsRegular() {
			cs = append(cs, cand{e.Name(), st})
		}
	}
	if len(cs) == 0 {
		return "", fmt.Errorf("되돌릴 덤프가 없다 - %q 에 %s<판>-<시각>%s 이 하나도 없다. "+
			"덤프는 판올림이 기존 데이터 위에 증분을 얹을 때만 뜬다(빈 DB 에 기준선을 깐 경우는 안 뜬다)",
			clip(dir, 200), dumpPrefix, dumpExt)
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].stamp != cs[j].stamp {
			return cs[i].stamp < cs[j].stamp
		}
		return cs[i].name < cs[j].name
	})
	return filepath.Join(dir, cs[len(cs)-1].name), nil
}

// pgSSLMode 는 pgx 가 해석한 접속 설정의 TLS 를 libpq 의 sslmode 로 되옮긴다. 순수 함수다.
//
// ★ 되옮길 수 있는 것만 받는다. pgx 는 sslmode 를 TLS 설정과 대체 접속(Fallbacks)으로 풀어 두는데, 서버 인증서를 재는
// 모드(verify-ca · verify-full · 루트 인증서를 준 require)는 루트 인증서 파일 경로를 잃어 libpq 에 같은 뜻으로 못 넘긴다.
// 약하게 넘기면 백업 접속만 조용히 검증 없이 붙는다. 그래서 거절하고, 덤프 없이 판을 올리지도 않는다.
//
// 클라이언트 인증서(sslcert · sslkey)는 재지 않는다 - pgx 가 해석 결과에 드러내지 않는다(접속 때 붙이는 비공개 칸이다).
// 넘기지 못해도 약해지는 쪽이 아니다: 인증서는 서버가 이쪽을 재는 자격이라, 서버가 그것을 요구하면 pg_dump 가 인증에
// 실패하고(pgDumpFault 의 「인증에 실패했다」) 판을 올리지 않는다. 기본 자리(~/.postgresql/postgresql.crt)는 libpq 도
// 같은 자리를 읽는다.
func pgSSLMode(cfg *pgconn.Config) (string, error) {
	for _, fb := range cfg.Fallbacks {
		if fb.Host != cfg.Host || fb.Port != cfg.Port {
			return "", errors.New("호스트가 여럿인 접속이다")
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
			return "allow", nil
		}
		return "disable", nil
	}
	tc := cfg.TLSConfig
	if !tc.InsecureSkipVerify || tc.VerifyPeerCertificate != nil || tc.VerifyConnection != nil {
		return "", errors.New("서버 인증서를 재는 TLS 접속이다(sslmode=verify-ca · verify-full 또는 sslrootcert)")
	}
	if plainFallback {
		return "prefer", nil
	}
	return "require", nil
}

// libpqEnv 는 서버가 붙는 접속(t)을 libpq 도구(pg_dump · pg_restore)에 넘길 환경이다. environ 은 물려받을 환경(os.Environ)이다.
//
// ★ 접속 URL 원문을 쓰는 열기 경계의 하나다(Target 의 ★). 원문은 openPG 와 같은 해석기(pgx.ParseConfig)로 해석만 하고
// 어디에도 싣지 않는다. 해석 실패 · 되옮길 수 없는 설정의 오류도 표시값과 고정 문구만 싣는다.
//
// ★ 물려받은 환경에서 PG 로 시작하는 변수를 다 뺀다. pgx 는 해석할 때 이미 그것들(PGPASSWORD · PGSSLMODE · .pgpass 등)을
// 반영했으므로, 남겨 두면 libpq 가 같은 변수를 다시 읽어 서버와 다른 곳에 붙을 수 있다. 접속은 아래에서 다시 쓴 것만으로
// 정한다 - 어디에(호스트 · 포트 · DB) 누구로(사용자 · 비밀번호) 붙는가, 그리고 접속을 지키는 설정(sslmode ·
// channel_binding · require_auth · sslnegotiation)이다. 지키는 설정을 빠뜨리면 백업 접속만 비밀번호를 약한 방식으로
// 내줄 수 있다. 그 설정을 모르는 옛 libpq 는 변수를 무시한다.
func libpqEnv(t Target, environ []string) ([]string, error) {
	shown := t.Shown()
	cfg, err := pgx.ParseConfig(t.open)
	if err != nil {
		return nil, fmt.Errorf("libpq 도구에 넘길 접속 정보를 해석하지 못했다(%s)", shown)
	}
	mode, err := pgSSLMode(&cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("libpq 도구에 같은 뜻으로 넘길 수 없는 접속이다(%s): %s", shown, err)
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
	if cfg.Password != "" {
		env = append(env, "PGPASSWORD="+cfg.Password)
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

// dumpPG 는 판올림 전 덤프를 뜬다. 뜬 파일 경로를 낸다. to 는 이번에 올릴 판이고, bin 은 pg_dump 실행 파일(이름이면
// PATH 에서 찾는다)이다.
//
// ★ 임시 이름에 뜨고 다 뜬 뒤에 이름을 바꾼다. pg_dump 가 도중에 죽으면 반쪽 파일이 남는데, 그것이 정식 이름이면
// 되돌리기(latestDump)가 그것을 최신으로 고른다. 머리(PGDMP)도 잰다 - 0으로 끝나고 빈 파일을 낸 실행을 믿지 않는다.
func dumpPG(ctx context.Context, t Target, dir, bin string, to int, log *slog.Logger) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("판올림 전 덤프 자리가 없다(--backup-dir) - Postgres 원장은 덤프 없이 판을 올리지 않는다")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("백업 자리를 만들지 못했다(%q): %w", clip(dir, 200), err)
	}
	dst := filepath.Join(dir, dumpName(to, time.Now()))
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("같은 이름의 덤프가 이미 있다(%q) - 1초 안에 두 번 돌았다. 잠시 뒤 다시 돌려라", clip(dst, 200))
	}
	part := dst + ".part"
	env, err := libpqEnv(t, os.Environ())
	if err != nil {
		return "", fmt.Errorf("%w - 덤프 없이 판을 올리지 않는다", err)
	}
	args := pgDumpArgs(bin, part)
	path, err := exec.LookPath(args[0])
	if err != nil {
		return "", errors.New(pgDumpFault(err, ""))
	}
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
	if err := os.Rename(part, dst); err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("덤프 이름을 바꾸지 못했다(%q): %w", clip(dst, 200), err)
	}
	log.Info("판올림 전 덤프 완료", "backup", dst, "duration", time.Since(start).Seconds())
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

// rollbackPG 는 dir 의 덤프 중 가장 최근 것을 고르고 머리를 잰 뒤 복원 절차를 낸다. DB 에 붙지 않는다.
func rollbackPG(dir string) (RollbackOutcome, error) {
	if strings.TrimSpace(dir) == "" {
		return RollbackOutcome{}, errors.New("덤프 자리(--backup-dir)가 없다 - Postgres 원장의 판올림 전 덤프를 어디서 찾을지 모른다")
	}
	dump, err := latestDump(dir)
	if err != nil {
		return RollbackOutcome{}, err
	}
	if err := checkDumpHead(dump); err != nil {
		return RollbackOutcome{}, fmt.Errorf("가장 최근 덤프가 깨졌다 - 더 옛 덤프로 조용히 넘어가지 않는다: %w", err)
	}
	return RollbackOutcome{Backup: dump, Steps: pgRollbackHint(dump)}, nil
}

// pgRollbackHint 는 Postgres 원장을 판올림 전 덤프로 되돌리는 절차다. 순수 함수다.
//
// ★ 제자리에 덮지 않고 새 DB 로 복원한다. 망가진 DB 는 이름을 바꿔 증거로 남기고, pg_restore --create 가 덤프에 든 DB
// 정의(로케일 · DB 단위 권한)로 원래 이름의 DB 를 새로 만든다 - 그래서 접속 URL(FD_DB)을 안 바꿔도 된다. 빈 DB 를 손으로
// 만들어 복원하면 로케일 관문과 준비(bootstrap.sql)의 DB 단위 권한(PUBLIC 의 TEMPORARY 회수)을 따로 맞춰야 한다.
//
// ★ DB 이름은 싣지 않고 자리표시로 둔다. 서버에게 물어 채울 수 있지만, 경계가 모호한 접속 URL 에서는 그 이름이 비밀번호
// 조각일 수 있다(logPGServer 의 ★).
func pgRollbackHint(dump string) string {
	if strings.TrimSpace(dump) == "" {
		return "이번 적용은 덤프를 뜨지 않았다(빈 DB 에 기준선을 깔았다) - 되돌릴 덤프가 없다."
	}
	return fmt.Sprintf("판올림 전으로 통째로 되돌리려면 서버를 멈추고 초사용자로: "+
		"(1) psql -d postgres -c 'ALTER DATABASE \"<원장 DB>\" RENAME TO \"<원장 DB>_broken\"' "+
		"(2) pg_restore --create --exit-on-error --dbname=postgres %q "+
		"(3) 덤프를 뜬 판의 바이너리로 서버를 띄운다. 이 절차는 `fd migrate --rollback` 이 덤프를 골라 그대로 낸다.",
		clip(dump, 200))
}
