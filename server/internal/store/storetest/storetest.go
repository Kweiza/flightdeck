// Package storetest 는 시험마다 독립된 Postgres DB 를 하나씩 준다.
//
//	dsn := storetest.New(t) // 이 시험만의 새 DB 를 가리키는 접속 URL. 시험이 끝나면 DB 가 지워진다
//
// ★ 시험 서버는 환경 변수 FD_TEST_PG(관리 DB 의 URL, 예: postgres://postgres@127.0.0.1:<포트>/postgres)로
// 받는다. **없으면 시험이 실패한다 — 건너뛰지 않는다.** 건너뛰면 아무것도 안 돈 패키지가 초록으로 남는다.
// 띄우는 법은 저장소 루트 compose.test.yaml 의 머리말이다.
//
// ★ 시험마다 스키마를 처음부터 올리지 않는다. 판이 맞는 원판(템플릿) DB 를 프로세스마다 한 번 확인하고
// (없으면 만든다), 시험마다 `CREATE DATABASE t_<난수> TEMPLATE <원판>` 으로 복제한다. 「판이 맞는다」는
// 원판 이름이 원판에 넣는 내용 바이트의 해시에서 나온다는 뜻이다 — 손으로 적는 판 문자열은 없다.
//
// ★ 병렬 안전. 겹치는 자리가 셋이다.
//   - 한 프로세스 안의 병렬 시험: 원판 확인은 sync.Once 로 한 번만 한다.
//   - 프로세스 사이(`go test ./...` 는 패키지마다 따로 띄운다): 원판 만들기를 관리 DB 의 advisory 잠금
//     안에서 한다. 잠금을 기다린 쪽은 완성 표지가 붙은 원판을 보고 그대로 쓴다.
//   - 원판에 붙은 연결: PG 는 원판에 다른 연결이 있으면 복제를 거절한다(55006). 그래서 다 만든 원판은
//     ALLOW_CONNECTIONS false 로 접속 자체를 막는다. 만들 때 쓴 연결이 아직 닫히는 중이면 PG 가 몇 초
//     기다려 준다. 재시도를 두지 않는 까닭이다 — 그래도 거절이 나면 누가 원판에 붙었는지 드러나야 한다.
//
// ★ 이 패키지는 store 를 import 하지 않는다. store 의 패키지 안 시험이 이것을 부르므로, import 하면
// 순환이다. 그래서 *store.Store 가 아니라 접속 URL 을 돌려준다.
package storetest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql 드라이버 "pgx"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// EnvDSN 은 시험 서버의 관리 DB URL 을 주는 환경 변수다.
const EnvDSN = "FD_TEST_PG"

// schema 는 원판에 넣을 내용이다.
//
// ★ 원판 이름은 content 의 해시에서 나온다. 그래서 스키마가 바뀌면 이름이 바뀌고, 오래 띄워 둔 시험
// 서버도 옛 원판을 복제하지 않는다. 판 문자열을 따로 두지 않는 까닭이다: 손으로 적는 판은 올리기를
// 잊는 순간 옛 원판이 조용히 쓰인다.
type schema struct {
	// content 는 원판에 넣는 것의 바이트 **전부**다 — 스키마 SQL, 파일이 여럿이면 순서대로 이은 것.
	content []byte
	// apply 는 갓 만든 빈 원판에 content 를 넣는다. nil 이면 content 를 SQL 로 그대로 돌린다.
	// **content 밖의 것을 넣으면 안 된다**(그 몫은 해시에 안 잡혀 원판 이름이 안 바뀐다). 그래서 content 를
	// 인자로 받는다: 실행기가 내용을 따로 읽지 말고 이것을 넣게 하려는 꼴이다.
	apply func(ctx context.Context, db *sql.DB, content []byte) error
}

// fill 은 갓 만든 빈 원판에 내용을 넣는다.
func (s schema) fill(ctx context.Context, db *sql.DB) error {
	if s.apply != nil {
		return s.apply(ctx, db, s.content)
	}
	if len(s.content) == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, string(s.content))
	return err
}

// fd 는 New 가 복제하는 원판이다. 내용은 PG 기준선 전부(준비 + 소유 역할로 기준선)다.
//
// ★ 준비가 역할 둘(클러스터 전역)을 만든다. 원판 만들기는 프로세스 사이에서 한 줄로 서므로(buildLock)
// 겹치지 않고, 준비 자체도 이미 있는 역할을 그대로 둔다. 판 행은 비어 있다 — 그것은 이전 실행기의 몫이다.
// 스키마 패키지는 store 를 import 하지 않는다(위 순환과 같은 까닭).
//
// ★ 준비는 복제한 DB 마다 다시 돈다(everyDB). 준비는 DB 하나에 한 번 도는 것이고, 그 안의 DB 단위 권한
// (PUBLIC 의 TEMPORARY 회수)은 템플릿 복제로 안 따라온다 — 빠뜨리면 시험 DB 에서만 앱 역할이 임시 표로 원장을
// 가릴 수 있다. 준비는 몇 번 돌려도 같다.
var fd = &template{prefix: "fd_tmpl", schema: schema{content: []byte(pg.Fresh())}, everyDB: pg.Bootstrap}

// New 는 이 시험만의 새 DB 를 만들어 그 접속 URL 을 돌려준다. 시험이 끝나면 DB 를 지운다.
// FD_TEST_PG 가 없거나 서버에 못 붙으면 시험을 실패시킨다.
func New(tb testing.TB) string {
	tb.Helper()
	return fd.clone(tb)
}

// CutConnections 는 New 가 준 DB 가 새 연결을 받지 않게 하고(ALLOW_CONNECTIONS false) 지금 붙은 연결을 모두 끊는다.
// 서버가 다시 떴거나 연결이 끊긴 뒤 연결 풀의 재접속이 실패하는 자리를 흉내 낸다. 끊긴 연결이 다 빠질 때까지 기다린다 -
// 안 기다리면 아직 안 죽은 연결이 다음 질의를 받아 재접속 길을 안 탄다. 지우는 것은 New 의 정리가 한다
// (DROP DATABASE 는 그 DB 에 붙지 않는다).
func CutConnections(tb testing.TB, dsn string) {
	tb.Helper()
	_, admin, err := server()
	if err != nil {
		tb.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		tb.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := admin.ExecContext(ctx, "ALTER DATABASE "+ident(name)+" WITH ALLOW_CONNECTIONS false"); err != nil {
		tb.Fatalf("시험 DB 접속 막기 실패(%s): %v", name, err)
	}
	for {
		var left int
		if err := admin.QueryRowContext(ctx, `WITH k AS (SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1)
			SELECT count(*) FROM k`, name).Scan(&left); err != nil {
			tb.Fatalf("시험 DB 연결 끊기 실패(%s): %v", name, err)
		}
		if left == 0 {
			return
		}
		select {
		case <-ctx.Done():
			tb.Fatalf("시험 DB 연결이 %s 안에 안 빠졌다(%s, 남은 %d)", opTimeout, name, left)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// opTimeout 은 DB 하나를 만들거나 지우는 한 번의 상한이다. 평소에는 1초도 안 걸린다 — 이것은 서버가
// 응답하지 않을 때 시험이 영영 서지 않게 하는 상한일 뿐이다.
const opTimeout = time.Minute

// buildLock 은 원판 만들기를 프로세스 사이에서 한 줄로 세우는 advisory 잠금 키다("fdtstmpl").
// 잠금은 관리 DB 안에서만 뜻이 있다.
const buildLock int64 = 0x6664_7473_746d_706c

// template 은 원판 하나와 그것을 이 프로세스에서 확인했는지의 기억이다.
type template struct {
	prefix string
	schema schema
	// everyDB 는 복제한 DB 마다 관리 사용자로 한 번 돌리는 SQL 이다. 템플릿 복제가 옮기지 않는 것
	// (DB 단위 권한 · ALTER DATABASE … SET)을 다시 적용하는 자리다. 원판 이름의 해시에는 안 들어간다 —
	// 원판에 넣는 것이 아니다.
	everyDB string

	once sync.Once
	name string
	err  error
}

// names 는 원판의 DB 이름과 완성 표지를 낸다. 둘 다 내용의 해시다: 내용이 다른 원판은 이름부터 달라
// 같은 서버에 함께 산다(스키마가 다른 체크아웃 둘이 한 서버를 써도 서로의 원판을 지우지 않는다).
func (tp *template) names() (name, mark string) {
	sum := sha256.Sum256(tp.schema.content)
	mark = hex.EncodeToString(sum[:])
	return tp.prefix + "_" + mark[:12], mark
}

// clone 은 원판을 복제한 새 DB 의 URL 을 낸다. 시험이 끝나면 지운다.
func (tp *template) clone(tb testing.TB) string {
	tb.Helper()
	base, admin, err := server()
	if err != nil {
		tb.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	tmpl, err := tp.ensure(ctx, base, admin)
	if err != nil {
		tb.Fatalf("시험 원판 준비 실패: %v", err)
	}
	name := "t_" + strings.ToLower(rand.Text())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+ident(name)+" TEMPLATE "+ident(tmpl)); err != nil {
		tb.Fatalf("시험 DB 만들기 실패(%s ← %s): %v", name, tmpl, err)
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		// FORCE: 시험이 닫지 않고 남긴 연결이 있어도 지운다. 버리는 DB 다.
		if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+ident(name)+" WITH (FORCE)"); err != nil {
			tb.Errorf("시험 DB 지우기 실패(%s): %v", name, err)
		}
	})
	if tp.everyDB != "" {
		if err := atDB(ctx, dbURL(base, name), func(db *sql.DB) error {
			_, err := db.ExecContext(ctx, tp.everyDB)
			return err
		}); err != nil {
			tb.Fatalf("시험 DB 다시 준비 실패(%s): %v", name, err)
		}
	}
	return dbURL(base, name)
}

// ensure 는 원판을 이 프로세스에서 한 번만 확인한다. 실패도 기억한다: 원판이 없으면 모든 시험이 같은
// 까닭으로 실패하는 것이 맞다.
func (tp *template) ensure(ctx context.Context, base *url.URL, admin *sql.DB) (string, error) {
	tp.once.Do(func() { tp.name, tp.err = tp.build(ctx, base, admin) })
	return tp.name, tp.err
}

// build 는 원판이 완성돼 있으면 그대로 쓰고, 없거나 만들다 멈춘 것이면 (다시) 만든다.
func (tp *template) build(ctx context.Context, base *url.URL, admin *sql.DB) (string, error) {
	name, mark := tp.names()

	// ★ 트랜잭션 잠금이다: 커밋 · 롤백 · 연결 끊김 어느 쪽으로 끝나도 풀린다. 세션 잠금이면 풀기를 놓친
	// 연결이 풀로 돌아가 다른 프로세스를 영영 세운다. CREATE DATABASE 는 트랜잭션 안에서 못 돌므로
	// DB 를 만들고 고치는 문장은 잠금을 쥔 연결 밖, 풀의 다른 연결로 보낸다.
	lock, err := admin.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("관리 DB 연결: %w", err)
	}
	defer lock.Rollback()
	if _, err := lock.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", buildLock); err != nil {
		return "", fmt.Errorf("원판 잠금: %w", err)
	}

	var got sql.NullString
	err = admin.QueryRowContext(ctx,
		"SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1", name).Scan(&got)
	switch {
	case err == nil && got.String == mark:
		return name, nil // 다른 프로세스나 앞선 실행이 다 만들어 뒀다
	case err == nil:
		// 만들다 멈춘 원판이다. 완성 표지가 마지막 단계라, 표지가 없으면 그 앞 어디선가 멈췄다.
		if err := dropTemplate(ctx, admin, name); err != nil {
			return "", fmt.Errorf("만들다 멈춘 원판 %s 지우기: %w", name, err)
		}
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("원판 %s 조회: %w", name, err)
	}

	// template0 에서 만든다: template1 은 누가 무엇을 넣어 뒀을 수 있다. 로케일은 클러스터의 것(initdb)을
	// 그대로 받는다 — 운영 DB 가 만들어지는 길과 같게 두고, 로케일은 시험이 따로 잠근다.
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+ident(name)+" TEMPLATE template0"); err != nil {
		return "", fmt.Errorf("원판 %s 만들기: %w", name, err)
	}
	if err := fillAt(ctx, dbURL(base, name), tp.schema); err != nil {
		return "", fmt.Errorf("원판 %s 에 내용 넣기: %w", name, err)
	}
	// 봉인: 접속을 막고(복제가 「원판에 다른 연결이 있다」로 거절되는 자리를 없앤다) 원판으로 표시한 뒤,
	// 마지막에 완성 표지를 단다.
	if _, err := admin.ExecContext(ctx,
		"ALTER DATABASE "+ident(name)+" WITH ALLOW_CONNECTIONS false IS_TEMPLATE true"); err != nil {
		return "", fmt.Errorf("원판 %s 봉인: %w", name, err)
	}
	// COMMENT 는 자리표시를 못 받는다. mark 는 16진 글자뿐이라 그대로 넣어도 된다.
	if _, err := admin.ExecContext(ctx, "COMMENT ON DATABASE "+ident(name)+" IS '"+mark+"'"); err != nil {
		return "", fmt.Errorf("원판 %s 완성 표지: %w", name, err)
	}
	return name, nil
}

// fillAt 은 dsn 의 DB 에 내용을 넣는다.
func fillAt(ctx context.Context, dsn string, s schema) error {
	return atDB(ctx, dsn, func(db *sql.DB) error { return s.fill(ctx, db) })
}

// atDB 는 dsn 의 DB 에 붙어 f 를 돌리고 연결을 다 닫는다. 원판에 남은 연결은 복제를 막는다.
func atDB(ctx context.Context, dsn string, f func(*sql.DB) error) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	err = f(db)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	return err
}

// dropTemplate 은 원판을 지운다. 원판 표시가 있으면 DROP 이 거절하므로 먼저 푼다.
func dropTemplate(ctx context.Context, admin *sql.DB, name string) error {
	if _, err := admin.ExecContext(ctx, "ALTER DATABASE "+ident(name)+" IS_TEMPLATE false"); err != nil {
		return err
	}
	_, err := admin.ExecContext(ctx, "DROP DATABASE "+ident(name)+" WITH (FORCE)")
	return err
}

// srv 는 이 프로세스가 쓰는 시험 서버다. 관리 DB 연결 풀은 프로세스가 끝날 때까지 연다.
var srv struct {
	once  sync.Once
	base  *url.URL
	admin *sql.DB
	err   error
}

func server() (*url.URL, *sql.DB, error) {
	srv.once.Do(func() {
		if srv.base, srv.err = adminURL(os.Getenv(EnvDSN)); srv.err == nil {
			srv.admin, srv.err = sql.Open("pgx", srv.base.String())
		}
	})
	return srv.base, srv.admin, srv.err
}

// adminURL 은 FD_TEST_PG 값을 읽는다. 시험 DB 마다 이 URL 의 DB 자리만 바꿔 끼우므로 URL 꼴만 받는다.
func adminURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s 가 비었다. 이 시험은 Postgres 에서 돈다(건너뛰지 않는다) — "+
			"저장소 루트 compose.test.yaml 머리말의 명령으로 시험 서버를 띄우고 그 주소를 줘라", EnvDSN)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return nil, fmt.Errorf("%s 는 postgres://<사용자>@<호스트>:<포트>/<관리 DB> 꼴의 URL 이어야 한다", EnvDSN)
	}
	return u, nil
}

// dbURL 은 관리 DB URL 에서 DB 자리만 name 으로 바꾼다. 사용자 · 주소 · 질의 인자는 그대로 간다.
func dbURL(base *url.URL, name string) string {
	u := *base
	u.Path, u.RawPath = "/"+name, ""
	return u.String()
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }
