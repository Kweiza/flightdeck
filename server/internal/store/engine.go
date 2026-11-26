package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// ─────────────────────────────────────────────────────────────────────────────
// 저장 엔진
// ─────────────────────────────────────────────────────────────────────────────
//
// 엔진이 갈리는 자리는 이 파일(열기 · 트랜잭션 시작 · 자리표시)과 constraint.go(오류 번역) 둘뿐이다.
// 질의를 쓰는 자리(item.go 등)는 엔진을 모른다 - `?` 자리표시로 쓰고 conn · txConn 을 지난다.
// 날 *sql.DB · *sql.Tx 는 이 파일 밖으로 안 나간다(DB() 만 예외다, 시험 · 진단 전용).
// engine_test.go 가 그 경계를 소스로 잠근다.
//
// ★ 두 엔진이 함께 있는 것은 옮기는 동안뿐이다. Postgres 로 옮긴 뒤 SQLite 실행 경로는 빠진다.

type engine int

const (
	engineSQLite engine = iota
	enginePG
)

func (e engine) String() string {
	if e == enginePG {
		return "postgres"
	}
	return "sqlite"
}

// engineOf 는 열기 인자로 엔진을 가린다. 순수 함수다.
// postgres:// · postgresql:// 로 시작하면 Postgres 접속 URL 이고, 아니면 SQLite 파일 경로다.
func engineOf(target string) engine {
	t := strings.ToLower(target)
	if strings.HasPrefix(t, "postgres://") || strings.HasPrefix(t, "postgresql://") {
		return enginePG
	}
	return engineSQLite
}

// lockTimeout 은 잠금 하나를 기다리는 상한이다. 두 엔진이 같은 값을 쓴다 -
// SQLite 는 busy_timeout pragma 로, Postgres 는 연결마다 lock_timeout 으로 건다.
// DeferredFlushBudget 의 아래쪽 부등식이 이 값이다.
const lockTimeout = 5 * time.Second

// ─────────────────────────────────────────────────────────────────────────────
// 열기
// ─────────────────────────────────────────────────────────────────────────────

// openPG 는 Postgres 원장에 붙는다. 판은 재지 않는다(OpenWithLogger 가 checkPGVersion 으로 잰다).
//
// ★ lock_timeout 을 트랜잭션마다 `SET LOCAL` 하지 않고 연결 시작 인자로 건다. SQLite 의 busy_timeout 이
// DSN 에 있던 것과 같은 자리이고, 쓰기 트랜잭션마다 왕복 하나를 던다. 걸렸는지는 되읽어 잰다
// (verifyPragmas 와 같은 까닭: 적었다는 것은 걸렸다의 근거가 못 된다 - 사이의 연결 중개기가 버릴 수 있다).
func openPG(dsn string, log *slog.Logger, wait time.Duration) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	shown := redactDSN(dsn)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("Postgres 접속 문자열 해석 실패(%s): %w", shown, err)
	}
	ms := strconv.FormatInt(wait.Milliseconds(), 10)
	cfg.RuntimeParams["lock_timeout"] = ms
	db := stdlib.OpenDB(*cfg)
	// 동시 연결 상한. 쓰기는 전역 잠금에서 한 줄로 서므로 늘려도 처리량이 안 는다. 커밋 뒤 이벤트 흘리기가
	// 연결 하나를 더 쓰므로 둘보다 작으면 안 된다(SQLite 와 같은 값).
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("Postgres 접속 실패(%s): %w", shown, err)
	}
	s := &Store{db: &conn{raw: db, eng: enginePG}, path: shown, log: log}
	var got string
	if err := s.db.QueryRowContext(ctx, `SELECT setting FROM pg_settings WHERE name = 'lock_timeout'`).Scan(&got); err != nil {
		db.Close()
		return nil, fmt.Errorf("lock_timeout 되읽기 실패(%s): %w", shown, err)
	}
	if got != ms {
		db.Close()
		return nil, fmt.Errorf("연결 인자 lock_timeout 이 실제로 걸리지 않았다(%s): %sms(기대 %sms) "+
			"- 연결 중개기가 시작 인자를 버리는지 확인하라", shown, got, ms)
	}
	return s, nil
}

// checkPGVersion 은 Postgres 원장의 판이 이 바이너리의 판과 같은지만 본다.
//
// ★ 최소한이다. 판 행을 적는 것(기준선을 깐 뒤)과 어긋난 판을 올리는 것, 로케일 재기는 Postgres 이전
// 실행기의 몫이다. 여기는 「판이 안 맞으면 뜨지 않는다」는 SQLite 쪽 규율만 Postgres 에도 세운다.
func (s *Store) checkPGVersion(ctx context.Context) error {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT max(version) FROM schema_version`).Scan(&v); err != nil {
		return fmt.Errorf("Postgres 원장의 판을 읽지 못했다(%s): %w - 기준선이 안 깔린 DB 다", s.path, err)
	}
	if !v.Valid {
		return fmt.Errorf("Postgres 원장에 판 행이 없다(%s) - 기준선을 깐 뒤 판(%d)을 적는 것은 이전 실행기의 몫이다",
			s.path, SchemaVersion)
	}
	if int(v.Int64) != SchemaVersion {
		return fmt.Errorf("Postgres 원장의 판이 %d 인데 이 바이너리는 %d 를 기대한다(%s) - 판을 맞추는 것은 이전 실행기의 몫이다",
			v.Int64, SchemaVersion, s.path)
	}
	return nil
}

// redactDSN 은 로그 · 오류에 실을 접속 URL 이다. 비밀번호를 가린다(사용자 정보와 질의 인자 둘 다). 순수 함수다.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "postgres://<해석 못 한 접속 문자열>"
	}
	if q := u.Query(); q.Has("password") {
		q.Set("password", "xxxxx")
		u.RawQuery = q.Encode()
	}
	return u.Redacted()
}

// ─────────────────────────────────────────────────────────────────────────────
// 감싸개 - 모든 실행 자리가 지난다
// ─────────────────────────────────────────────────────────────────────────────

// conn 은 store 의 단발 질의가 지나는 DB 핸들이다(Store.db).
type conn struct {
	raw *sql.DB
	eng engine
}

func (c *conn) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return c.raw.ExecContext(ctx, c.eng.bind(q), args...)
}

func (c *conn) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return c.raw.QueryContext(ctx, c.eng.bind(q), args...)
}

func (c *conn) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return c.raw.QueryRowContext(ctx, c.eng.bind(q), args...)
}

// Exec · Query · QueryRow 는 시험이 쓰는 꼴(ctx 없음)이다. 같은 감싸개를 지난다.
func (c *conn) Exec(q string, args ...any) (sql.Result, error) {
	return c.ExecContext(context.Background(), q, args...)
}

func (c *conn) Query(q string, args ...any) (*sql.Rows, error) {
	return c.QueryContext(context.Background(), q, args...)
}

func (c *conn) QueryRow(q string, args ...any) *sql.Row {
	return c.QueryRowContext(context.Background(), q, args...)
}

func (c *conn) Close() error { return c.raw.Close() }

// txConn 은 진행 중인 트랜잭션의 핸들이다. begin 만 만든다.
type txConn struct {
	raw *sql.Tx
	eng engine
}

func (t *txConn) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.raw.ExecContext(ctx, t.eng.bind(q), args...)
}

func (t *txConn) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.raw.QueryContext(ctx, t.eng.bind(q), args...)
}

func (t *txConn) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.raw.QueryRowContext(ctx, t.eng.bind(q), args...)
}

func (t *txConn) Commit() error   { return t.raw.Commit() }
func (t *txConn) Rollback() error { return t.raw.Rollback() }

// bind 는 질의를 엔진의 자리표시로 옮긴다. SQLite 는 `?` 그대로다.
func (e engine) bind(q string) string {
	if e != enginePG {
		return q
	}
	return bindDollar(q)
}

// bindDollar 는 자리표시 `?` 를 나온 순서대로 $1, $2 … 로 바꾼다. 순수 함수다.
//
// ★ 소스를 일괄로 안 바꾸고 실행 때 바꾸는 까닭: SQL 을 실행 중 조립하는 자리(IN 목록, 조건에 따라 붙는
// 거름)는 갈래마다 자리표시 수가 달라 번호를 소스에 못 박는다. 실행 직전의 완성된 문장만 번호가 정해진다.
//
// ★ `?` 는 낱말을 스스로 끊지만 `$n` 은 아니다 - `$1AND` 를 PG 는 「매개변수 뒤의 군더더기」로 거절한다.
// 그래서 뒤에 이름 글자가 붙어 있으면 빈칸을 하나 넣는다.
//
// ★ jsonb 의 `?` · `?|` · `?&` 연산자는 자리표시와 못 가른다. store 의 SQL 은 그것을 쓰지 않는다
// (engine_test.go 가 소스의 SQL 조각을 훑어 막는다).
func bindDollar(q string) string {
	pos := placeholders(q)
	if len(pos) == 0 {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 3*len(pos))
	last := 0
	for k, p := range pos {
		b.WriteString(q[last:p])
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(k + 1))
		if p+1 < len(q) && isIdentByte(q[p+1]) {
			b.WriteByte(' ')
		}
		last = p + 1
	}
	b.WriteString(q[last:])
	return b.String()
}

// placeholders 는 q 에서 자리표시 `?` 의 바이트 위치를 낸다. 순수 함수다.
//
// 자리표시가 아닌 `?`: 작은따옴표 문자열(E'…' 의 역슬래시 이스케이프 포함) · 큰따옴표 식별자 ·
// 달러 따옴표($$…$$, $tag$…$tag$) · 줄 주석(--) · 블록 주석(/* */, PG 처럼 겹칠 수 있다) 안의 것.
// 닫히지 않은 따옴표 · 주석은 끝까지 그 안으로 본다 - 문장 오류는 DB 가 낸다.
func placeholders(q string) []int {
	var out []int
	n := len(q)
	for i := 0; i < n; {
		c := q[i]
		switch {
		case c == '\'':
			esc := i > 0 && (q[i-1] == 'E' || q[i-1] == 'e') && (i < 2 || !isIdentByte(q[i-2]))
			i = skipQuoted(q, i, '\'', esc)
		case c == '"':
			i = skipQuoted(q, i, '"', false)
		case c == '-' && i+1 < n && q[i+1] == '-':
			if j := strings.IndexByte(q[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = n
			}
		case c == '/' && i+1 < n && q[i+1] == '*':
			i = skipBlockComment(q, i)
		case c == '$' && (i == 0 || !isIdentByte(q[i-1])):
			tag, ok := dollarTag(q, i)
			if !ok {
				i++
				continue
			}
			if j := strings.Index(q[i+len(tag):], tag); j >= 0 {
				i += 2*len(tag) + j
			} else {
				i = n
			}
		case c == '?':
			out = append(out, i)
			i++
		default:
			i++
		}
	}
	return out
}

// skipQuoted 는 q[i] 의 따옴표로 연 것이 닫힌 바로 뒤 위치를 낸다. 따옴표 두 개는 그 글자 하나다.
// backslash 면 역슬래시가 다음 글자를 품는다(E'…').
func skipQuoted(q string, i int, quote byte, backslash bool) int {
	n := len(q)
	for j := i + 1; j < n; j++ {
		switch {
		case backslash && q[j] == '\\':
			j++
		case q[j] == quote:
			if j+1 < n && q[j+1] == quote {
				j++
				continue
			}
			return j + 1
		}
	}
	return n
}

// skipBlockComment 는 q[i] 에서 연 블록 주석이 닫힌 바로 뒤 위치를 낸다. 겹친 주석을 센다.
func skipBlockComment(q string, i int) int {
	n, depth := len(q), 0
	for j := i; j < n; {
		switch {
		case j+1 < n && q[j] == '/' && q[j+1] == '*':
			depth++
			j += 2
		case j+1 < n && q[j] == '*' && q[j+1] == '/':
			depth--
			j += 2
			if depth == 0 {
				return j
			}
		default:
			j++
		}
	}
	return n
}

// dollarTag 는 q[i] 가 달러 따옴표의 여는 꼬리표($$ 또는 $이름$)면 그 꼬리표를 낸다.
// `$1` 같은 위치 매개변수는 꼬리표가 아니다(이름은 숫자로 시작하지 못한다).
func dollarTag(q string, i int) (string, bool) {
	j := i + 1
	if j < len(q) && q[j] == '$' {
		return "$$", true
	}
	if j >= len(q) || (q[j] >= '0' && q[j] <= '9') || !isIdentByte(q[j]) {
		return "", false
	}
	for j < len(q) && isIdentByte(q[j]) {
		j++
	}
	if j < len(q) && q[j] == '$' {
		return q[i : j+1], true
	}
	return "", false
}

// isIdentByte 는 이름(식별자 · 키워드)을 이루는 바이트인가다. UTF-8 의 여러 바이트 글자도 이름 글자로 본다.
func isIdentByte(c byte) bool {
	return c == '_' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// ─────────────────────────────────────────────────────────────────────────────
// 트랜잭션 시작 - 직렬성 규칙이 사는 단 한 자리
// ─────────────────────────────────────────────────────────────────────────────

// txKind 는 트랜잭션의 종류다. 증분 적용은 쓰기(txWrite)와 같은 잠금을 잡는다.
type txKind int

const (
	// txWrite 는 업무 쓰기(Tx) · 이벤트 추가 · 증분 적용이다. 전역 잠금 하나에서 한 줄로 선다.
	txWrite txKind = iota
	// txObs 는 관측 적재(ObsTx)다. (기기, 저장소) 잠금만 잡는다 - 전역 잠금과 서로 안 막는다.
	txObs
	// txRead 는 여러 질의 읽기(ReadTx)다. 잠그지 않고 한 스냅숏을 본다.
	txRead
)

// txSpec 은 트랜잭션 하나의 종류와 잠금 좌표다.
type txSpec struct {
	kind         txKind
	device, repo string // txObs 의 잠금 좌표
}

// 잠금 키. 전역 키는 bigint 한 개 공간, 관측 키는 int4 두 개 공간이다 - PG 는 두 공간을 겹치지 않게 둔다.
const (
	// globalLockKey 는 업무 쓰기 전부가 트랜잭션 첫 문장으로 잡는 advisory 잠금 키다("fd:txlck").
	globalLockKey int64 = 0x6664_3a74_786c_636b
	// obsLockSpace 는 관측 적재 잠금의 첫 키다("fdob"). 둘째 키가 (기기, 저장소) 의 해시다.
	obsLockSpace int32 = 0x6664_6f62
)

// txOptions 는 종류별 격리 수준이다.
//
// ★ Postgres 의 쓰기는 READ COMMITTED 를 **명시해서** 연다. 잠금을 받은 뒤의 문장마다 새 스냅숏을 봐야
// 앞 쓰기의 커밋이 보인다. REPEATABLE READ 면 스냅숏이 잠금 문장 시작에 잡혀, 잠금을 기다린 뒤에도 옛 상태를
// 읽고 이중 취득이 **조용히** 난다(pg_tx_test.go 의 잠금 다음 읽기 시험이 그것을 빨갛게 잡는다).
//
// ★ Postgres 의 읽기는 REPEATABLE READ READ ONLY 다. 여러 질의가 한 스냅숏을 봐야 질의 사이에 닫힌 세션이
// 목록에는 있고 축에는 없는 카드가 안 나온다. 읽기 전용 RR 은 직렬화 실패가 없다.
//
// ★ SQLite 는 DSN 의 _txlock=immediate 로 쓰기가 BEGIN IMMEDIATE 다(그것이 곧 전역 쓰기 잠금이다).
// 읽기 전용이면 드라이버가 그냥 BEGIN(deferred)을 내고, WAL 에서 첫 읽기에 스냅숏이 잡힌다.
func (e engine) txOptions(k txKind) *sql.TxOptions {
	switch {
	case e == enginePG && k == txRead:
		return &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	case e == enginePG:
		return &sql.TxOptions{Isolation: sql.LevelReadCommitted}
	case k == txRead:
		return &sql.TxOptions{ReadOnly: true}
	default:
		return nil
	}
}

// lockStmt 는 트랜잭션이 첫 문장으로 잡는 잠금이다. 빈 문자열이면 따로 잡지 않는다.
// 트랜잭션 잠금(xact)이라 커밋 · 롤백 · 연결 끊김 어느 쪽으로 끝나도 풀린다.
func (e engine) lockStmt(sp txSpec) (string, []any) {
	if e != enginePG {
		return "", nil
	}
	switch sp.kind {
	case txWrite:
		return `SELECT pg_advisory_xact_lock(?)`, []any{globalLockKey}
	case txObs:
		// 해시가 겹치면 서로 다른 (기기, 저장소) 가 한 줄로 설 뿐이다 - 틀리지 않고 느려질 뿐이다.
		return `SELECT pg_advisory_xact_lock(?, hashtext(?::text || '|' || ?::text))`,
			[]any{obsLockSpace, sp.device, sp.repo}
	}
	return "", nil
}

// begin 은 store 의 트랜잭션이 열리는 단 한 자리다. 종류가 격리 수준과 잠금을 정한다.
// 부르는 자리는 runTx(Tx · ObsTx) · ReadTx · applySteps 셋뿐이다(engine_test.go 가 잠근다).
//
// 쓰기 종류는 잠금을 받기까지 기다린 시간을 잰다(/metrics). Postgres 는 잠금 문장만 잰다 - 연결 풀 대기는
// DBStats 의 풀 축이 따로 센다. SQLite 는 BEGIN IMMEDIATE 가 곧 잠금이라 BeginTx 전체를 잰다(풀 대기가 섞인다).
func (s *Store) begin(ctx context.Context, sp txSpec) (*txConn, error) {
	eng := s.db.eng
	start := time.Now()
	raw, err := s.db.raw.BeginTx(ctx, eng.txOptions(sp.kind))
	if err == nil {
		if q, args := eng.lockStmt(sp); q != "" {
			start = time.Now()
			if _, err = raw.ExecContext(ctx, eng.bind(q), args...); err != nil {
				_ = raw.Rollback()
			}
		}
	}
	if sp.kind != txRead {
		s.locks.observe(sp.space(), time.Since(start), err)
	}
	if err != nil {
		return nil, err
	}
	return &txConn{raw: raw, eng: eng}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 잠금 대기 계측
// ─────────────────────────────────────────────────────────────────────────────

// LockSpace 는 잠금 공간이다. /metrics 의 라벨 값이다.
type LockSpace string

const (
	LockGlobal LockSpace = "global" // 업무 쓰기 · 이벤트 추가 · 증분 적용
	LockObs    LockSpace = "obs"    // 관측 적재
)

// LockSpaces 는 잠금 공간 전부다. 표면이 둘 다 늘 내도록(0 과 「안 잰다」를 안 섞게) 목록으로 둔다.
func LockSpaces() []LockSpace { return []LockSpace{LockGlobal, LockObs} }

func (sp txSpec) space() LockSpace {
	if sp.kind == txObs {
		return LockObs
	}
	return LockGlobal
}

// LockWaitBuckets 는 잠금 대기 히스토그램의 칸 상한(초)이다. 0.2 칸은 「대기 p95 가 200 ms 를 넘으면
// 잠금을 좁히는 것을 검토한다」는 문턱을 그대로 읽게 하려고 둔다. 마지막 칸은 lockTimeout 이다.
var LockWaitBuckets = [...]float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.5, 1, 2.5, 5}

// LockWait 는 잠금 공간 하나의 대기 기록이다. 잠금을 받은 것만 칸에 들고, 대기 상한을 넘은 것은 Timeouts 다.
type LockWait struct {
	// Buckets[i] 는 대기가 (LockWaitBuckets[i-1], LockWaitBuckets[i]] 에 든 건수다(누적이 아니다).
	// 마지막 칸을 넘은 것은 어느 칸에도 없고 Count 에만 있다.
	Buckets  [len(LockWaitBuckets)]uint64
	Count    uint64
	Sum      float64
	Timeouts uint64
}

// DBStats 는 /metrics 에 실을 저장 계층 계측이다.
type DBStats struct {
	Engine string
	Locks  map[LockSpace]LockWait
	// PoolWaits · PoolWaitSeconds 는 연결 풀에서 연결을 기다린 횟수와 시간 합이다(database/sql 이 센다).
	PoolWaits       int64
	PoolWaitSeconds float64
}

// lockMeter 는 잠금 대기 누산기다. 영값을 그대로 쓴다.
type lockMeter struct {
	mu sync.Mutex
	m  map[LockSpace]*LockWait
}

func (l *lockMeter) observe(space LockSpace, d time.Duration, err error) {
	timeout := err != nil && JudgeError(err).Fault == FaultLockTimeout
	if err != nil && !timeout {
		return // 잠금과 무관한 실패(취소 등)는 대기 축이 아니다
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[LockSpace]*LockWait{}
	}
	w := l.m[space]
	if w == nil {
		w = &LockWait{}
		l.m[space] = w
	}
	if timeout {
		w.Timeouts++
		return
	}
	sec := d.Seconds()
	w.Count++
	w.Sum += sec
	for i, le := range LockWaitBuckets {
		if sec <= le {
			w.Buckets[i]++
			break
		}
	}
}

// DBStats 는 지금까지의 계측 사본이다.
func (s *Store) DBStats() DBStats {
	out := DBStats{Engine: s.db.eng.String(), Locks: map[LockSpace]LockWait{}}
	s.locks.mu.Lock()
	for _, sp := range LockSpaces() {
		if w := s.locks.m[sp]; w != nil {
			out.Locks[sp] = *w
		} else {
			out.Locks[sp] = LockWait{}
		}
	}
	s.locks.mu.Unlock()
	st := s.db.raw.Stats()
	out.PoolWaits, out.PoolWaitSeconds = st.WaitCount, st.WaitDuration.Seconds()
	return out
}
