// Package store 는 flightdeck 의 저장 계층이다(SQLite, 그리고 옮겨 가는 Postgres).
//
// 정본은 schema.sql(Postgres 는 pg/001_baseline.sql)이고 이 패키지는 그 위의 얇은 접근층이다.
// 엔진이 갈리는 자리는 engine.go(열기 · 트랜잭션 시작 · 자리표시)와 constraint.go(오류 번역)뿐이다. 스키마의 제약이
// 실제로 났던 사고를 막으므로, 여기서 **제약을 미리 흉내 내 판정하지 않는다** —
// 예를 들어 자원 배타는 부분 유니크 인덱스가 지키고, 이 코드는 그 위반을 받아
// 점유자를 담은 오류로 옮길 뿐이다. 앱에서 먼저 조회해 판정하면 그 사이에 남이 잡는다.
//
// 판정이 필요한 자리는 전부 **순수 함수**로 빼고 시험이 그 함수를 직접 부른다
// (PlanMigration·UpgradeSteps·JudgeClaim·JudgeConstraintCode·ValidateSnapshot·
// ValidateHolder·ValidateFinish·ValidateIdemRecord).
// 판정이 함수 본문에 흩어지면 시험이 그 로직의 사본을 단정하게 되고, 그러면 변이가 조용히 샌다.
// 그리고 다중 조건은 불리언이 아니라 **사유**를 돌려준다 — 사유가 없으면
// "조건 A 때문에 탈락"과 "이 축을 아예 안 본다"가 구분되지 않는다.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 순수 Go 드라이버. CGO_ENABLED=0 에서 돌아야 한다

	"github.com/kweiza/flightdeck/internal/store/pg"
)

//go:embed schema.sql
var schemaSQL string

//go:embed migrations/002_idempotency.sql
var migration002 string

//go:embed migrations/003_landing_queue.sql
var migration003 string

//go:embed migrations/004_pick_bundle.sql
var migrationPickBundle string

//go:embed migrations/005_footprint_absolute_backfill.sql
var migrationFootprintPurge string

//go:embed migrations/006_footprint_worktree_prefix_backfill.sql
var migrationWorktreePrefixPurge string

//go:embed migrations/007_project_view_axis.sql
var migrationProjectViewAxis string

//go:embed migrations/008_resource_queue.sql
var migrationResourceQueue string

//go:embed migrations/009_judgment_link_target_project.sql
var migrationLinkTargetProject string

//go:embed migrations/010_retire_item_dependents.sql
var migrationRetireItemDependents string

//go:embed migrations/011_drop_item_dependents.sql
var migrationDropItemDependents string

//go:embed migrations/012_recover_swallowed_judgment_links.sql
var migrationRecoverSwallowedLinks string

//go:embed migrations/013_session_harness.sql
var migrationSessionHarness string

//go:embed migrations/014_workspace_members.sql
var migrationWorkspaceMembers string

//go:embed migrations/015_item_after_dep_project.sql
var migrationItemAfterDepProject string

//go:embed migrations/016_item_revision.sql
var migrationItemRevision string

//go:embed migrations/017_item_revision_follows_move.sql
var migrationItemRevisionFollowsMove string

//go:embed migrations/018_session_inbox.sql
var migrationSessionInbox string

//go:embed migrations/019_event_by_project_kind.sql
var migrationEventByProjectKind string

//go:embed migrations/020_accounts.sql
var migrationAccounts string

// SchemaVersion 은 **이 바이너리가 아는** 스키마 버전이다. 두 엔진이 같은 수를 쓴다(SQLite 는 migrations, Postgres 는
// 기준선 위 pgMigrations 로 닿는다).
// DB 가 이보다 높으면 연다는 것 자체가 조용히 망가지는 경로이므로 거절한다.
const SchemaVersion = 20

// BaseSchemaVersion 은 schema.sql 하나가 만드는 버전이다.
//
// ★ 빈 DB 도 "schema.sql → 증분 전부"를 거쳐 올라간다. 신규 DB 를 위해 schema.sql 에
// 같은 표를 한 번 더 적으면 그 표의 정의가 두 벌이 되고, 두 벌은 반드시 표류한다 —
// 그때 신규 설치와 업그레이드가 **다른 모양의 DB** 를 갖게 되는데 그 차이는
// 문제가 터지기 전까지 아무 데도 안 보인다. 정의는 한 자리에만 둔다.
const BaseSchemaVersion = 1

// Migration 은 증분 하나다.
type Migration struct {
	To   int    // 적용 뒤의 버전
	Name string // 로그에 남길 이름
	SQL  string
	// base 는 빈 DB 에 까는 Postgres 기준선 단(pgBaseline)이다. 증분과 셋이 다르다: 앞선 판 대신 빈 DB 인지를 다시 재고,
	// 역할은 SQL 이 스스로 바꿔 타며(준비는 접속한 초사용자로 돌아야 한다), 판을 적기 전에 로케일 관문을 잰다(pg_trgm 이
	// 이 단 안에서 생긴다). stepBody 가 이것으로 가른다.
	base bool
}

// migrations 는 BaseSchemaVersion 위에 순서대로 얹는 증분 전부다.
//
// ★ 트랜잭션 안에서 돌 수 있어야 한다 — PRAGMA journal_mode 같은 문을 넣으면 안 된다.
// (schema.sql 은 그것 때문에 트랜잭션 밖에서 돈다.)
var migrations = []Migration{
	{To: 2, Name: "멱등 기록을 DB 로", SQL: migration002},
	{To: 3, Name: "랜딩 순서 큐", SQL: migration003},
	{To: 4, Name: "pick_eval 이 묶음을 담는다", SQL: migrationPickBundle},
	{To: 5, Name: "절대경로 발자국을 지운다", SQL: migrationFootprintPurge},
	{To: 6, Name: "워크트리 접두 발자국을 지운다", SQL: migrationWorktreePrefixPurge},
	{To: 7, Name: "프로젝트에 핀·보관 축", SQL: migrationProjectViewAxis},
	{To: 8, Name: "줄 행이 자원 집합을 갖는다", SQL: migrationResourceQueue},
	{To: 9, Name: "판단 링크가 대상 프로젝트를 싣는다", SQL: migrationLinkTargetProject},
	{To: 10, Name: "죽은 역인덱스의 값을 비운다", SQL: migrationRetireItemDependents},
	{To: 11, Name: "죽은 표 item_dependents 를 걷는다", SQL: migrationDropItemDependents},
	{To: 12, Name: "삼킴으로 죽은 판단 링크 21행을 되살린다", SQL: migrationRecoverSwallowedLinks},
	{To: 13, Name: "세션이 하네스를 싣는다", SQL: migrationSessionHarness},
	{To: 14, Name: "루트가 멤버 프로젝트 명부를 갖는다", SQL: migrationWorkspaceMembers},
	{To: 15, Name: "선행이 프로젝트를 넘는다", SQL: migrationItemAfterDepProject},
	{To: 16, Name: "항목 본문의 개정 이력 표를 만든다", SQL: migrationItemRevision},
	{To: 17, Name: "개정 이력이 항목을 따라 옮겨 간다", SQL: migrationItemRevisionFollowsMove},
	{To: 18, Name: "세션이 받는 곳(inbox 소켓)을 싣는다", SQL: migrationSessionInbox},
	{To: 19, Name: "event 를 (project, kind, id) 로 찾는다", SQL: migrationEventByProjectKind},
	{To: 20, Name: "계정 · 기기 토큰 · 행위자 칸, 옛 행을 소유자 계정에 귀속", SQL: migrationAccounts},
}

// pgBaseline 은 빈 Postgres DB 에 까는 기준선 단이다. 준비(초사용자) · 기준선(소유 역할) · 판 행이 한 트랜잭션이다 -
// 도중에 끊기면 통째로 되감겨 빈 DB 로 남는다. SQLite 처럼 schema.sql(1판) 위에 증분 17개를 다시 얹지 않는다:
// 기준선이 SQLite 18판을 접어 넣은 것이다(pg/schema.go).
var pgBaseline = Migration{To: pg.BaselineVersion, Name: "Postgres 기준선(준비 · 표 · 권한)", SQL: pg.Fresh(), base: true}

// pgMigrations 는 Postgres 기준선 위에 순서대로 얹는 증분이다. 목록의 정본은 pg.Steps 다 - 시험 원판(storetest)이 같은
// 목록을 싣는다(store 를 import 못 하므로 목록이 pg 에 산다). 한 단이 한 트랜잭션이고 소유 역할로 돈다(engine.stepRole).
// SQLite 에 오르는 증분은 같은 판 번호로 여기에도 실린다(pg/schema.go 머리말, schema_table_count_test.go 가 잰다).
var pgMigrations = pgStepMigrations(pg.Steps)

// pgStepMigrations 는 pg 의 증분 목록을 이전 실행기의 단으로 옮긴다. 순수 함수다.
func pgStepMigrations(steps []pg.Step) []Migration {
	out := make([]Migration, 0, len(steps))
	for _, st := range steps {
		out = append(out, Migration{To: st.To, Name: st.Name, SQL: st.SQL})
	}
	return out
}

// timeLayout 은 저장용 시각 표기다.
//
// time.RFC3339Nano 를 쓰면 안 된다 — 소수부의 뒤 0을 잘라 내므로 **길이가 흔들리고**,
// 그러면 사전순 정렬이 시간순과 어긋난다("…:05Z" 의 'Z'(0x5A) 가 "…:05.5Z" 의 '.'(0x2E)보다 크다).
// 스키마 주석이 "정렬이 사전순과 일치한다"를 전제하므로 폭 고정이 필수다.
const timeLayout = "2006-01-02T15:04:05.000000Z"

// ─────────────────────────────────────────────────────────────────────────────
// 오류
// ─────────────────────────────────────────────────────────────────────────────

// ErrNotFound 는 조회 대상이 없을 때의 표식이다. errors.Is 로 판별한다.
var ErrNotFound = errors.New("없다")

// ─────────────────────────────────────────────────────────────────────────────
// Store · Tx
// ─────────────────────────────────────────────────────────────────────────────

// Reader 는 읽기만 하는 면이다. ReadTx 의 클로저가 받는다 - 쓰기 문장(ExecContext)이 타입에 없다.
// 결과(*rowSet · *row)는 *sql.Rows · *sql.Row 처럼 읽는다. 드라이버 오류를 store 경계에서 옮기는 감싸개다(engine.go).
type Reader interface {
	QueryContext(ctx context.Context, q string, args ...any) (*rowSet, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *row
}

// catalogReader 는 엔진을 아는 읽기 면이다 - 판 재기(readMigrationState)처럼 엔진마다 문장이 다른 읽기가 받는다. dbtx 와
// 실행기 세션이 만족하고, ReadTx 의 Reader 는 onEngine 으로 엔진을 붙여 준다.
type catalogReader interface {
	Reader
	dialect() engine
}

// onEngine 은 Reader 에 그 원장의 엔진을 붙인다(catalogReader). 원장 옮기기가 원본 스냅숏(ReadTx) 안에서 판을 잴 때 쓴다.
type onEngine struct {
	Reader
	eng engine
}

func (r onEngine) dialect() engine { return r.eng }

// dbtx 는 단발 핸들(conn)과 트랜잭션(txConn)이 함께 만족하는 최소 면이다.
// 질의 구현을 한 벌로 두기 위한 것이다 — 두 벌이 되면 트랜잭션 경로와 단발 경로가 표류한다.
type dbtx interface {
	Reader
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	dialect() engine
}

// Store 는 열린 DB 하나다.
type Store struct {
	db *conn
	// path 는 SQLite 파일 경로다. Postgres 면 접속 세부 없는 표시값(Target.Shown)이다(로그 · 오류에 실린다).
	path  string
	log   *slog.Logger
	locks lockMeter
	// pgLim 은 Postgres 연결에 건 상한이다(openPGWith). SQLite 는 0 값이다.
	pgLim pgLimits

	// claimGap 은 시험의 끼움 자리다: ClaimItem 의 점유자 확인과 선점 쓰기 사이에 같은 트랜잭션으로 불린다. 운영은
	// nil 이다. 쓰기가 한 줄로 서는 동안은 그 틈에 남이 못 끼므로, 0행 경로(claimTakenMeanwhile)를 ClaimItem 호출로
	// 재현할 길이 이것뿐이다.
	claimGap func(*Tx) error
}

// Tx 는 진행 중인 쓰기 트랜잭션이다.
//
// ★ **직렬성: DB 전체에서 쓰기 트랜잭션 하나씩이다.** 두 엔진이 같은 뜻을 다른 수단으로 세운다(engine.go 의 begin).
//
//   - Postgres: READ COMMITTED 로 열고 첫 문장으로 전역 트랜잭션 advisory 잠금(globalLockKey)을 잡는다.
//     잠금은 커밋 · 롤백과 함께 풀린다. 잠금을 받은 뒤의 문장마다 새 스냅숏이라 앞 쓰기의 커밋이 보인다 -
//     그래서 「미선점 확인 → 삽입 사이에 창이 없다」 같은 클로저들의 정당화가 SQLite 때와 같이 선다.
//     잠금 대기 상한은 연결의 lock_timeout(lockTimeout)이다. 재시도는 없다.
//   - SQLite: DSN 의 `_txlock=immediate` 로 **BEGIN IMMEDIATE** 가 곧 전역 쓰기 잠금이다. 아래는 그 판의 근거다.
//
// ★ **오류의 뜻은 엔진마다 다르다.** SQLite 는 실패한 문장만 되돌리고 트랜잭션은 산다. Postgres 는 한 문장이
// 실패하면 트랜잭션 전체를 버리고(25P02) 뒤 문장과 커밋이 다 실패한다. 클로저 안에서 DB 오류를 받고 이어 가면
// Postgres 에서 그 트랜잭션은 커밋되지 않는다 - runTx 가 그것을 TxFault(FaultAborted)로 낸다.
// 오류를 받고 이어 가야 하는 자리는 둘 중 하나로 쓴다(savepoint_guard_test.go 가 소스로 잠근다):
//   - 오류 자체를 안 만든다: 흡수할 것이 그 문장의 유니크 충돌 하나뿐이면 `INSERT … ON CONFLICT DO NOTHING RETURNING`(insertUnlessTaken).
//   - 오류를 저장점 안에 가둔다: 흡수할 오류가 남의 메서드 안에서 나거나 종류를 가려 받아야 하면 Savepoint.
//
// 이 아래는 SQLite 판의 근거다.
//
// 무엇을 사는가를 정확히 적는다. **배타 자체는 이것과 무관하다** — 두 트랜잭션이 같은 것을
// 쓰려 하면 SQLite 가 어느 모드에서든 하나를 거절하므로, 이중 선점은 deferred 에서도 안 난다.
// immediate 가 사는 것은 **신뢰성**이다: deferred 는 읽기 스냅숏을 잡은 뒤 남이 커밋하면
// 쓰기 승격이 SQLITE_BUSY 로 즉시 실패하고(스냅숏 충돌이라 busy_timeout 이 안 듣는다),
// 그러면 정상 요청이 "database is locked" 로 사용자에게 올라간다.
// BEGIN 시점에 쓰기 잠금을 잡으면 그 승격 자체가 사라지고 대기가 busy_timeout 안에서 줄을 선다.
//
// 실측: 읽고 나서 쓰는 트랜잭션 24개를 동시에 돌리면 deferred 는 19~21개가 죽고 immediate 는
// 0개가 죽는다(TestTxSurvivesReadThenWriteContention 이 그 축을 동작으로 본다).
//
// 대가: 읽기 전용 작업도 이 함수를 거치면 쓰기 잠금을 잡아 서로 직렬화된다.
// 그래서 **읽기는 Tx 를 안 거치고** Store 의 조회 메서드가 s.db 로 바로 질의한다(WAL 이라 안 막힌다).
// 여러 질의가 한 시점을 봐야 하는 읽기는 ReadTx 다.
//
// 중첩 트랜잭션은 지원하지 않는다. 트랜잭션 안의 저장점(Savepoint)은 오류를 가두는 데만 쓴다.
type Tx struct {
	tx  *txConn
	ctx context.Context
	s   *Store

	// savepoints 는 이 트랜잭션이 연 저장점 수다. 저장점 이름의 번호가 된다(Savepoint).
	savepoints int

	// deferred 는 이 트랜잭션 안에서 남기려 한 계측 이벤트다.
	//
	// 트랜잭션 안에서 별도 커넥션으로 곧장 쓰면 **교착한다** — DSN 에 `_txlock=immediate` 가
	// 걸려 있어 BEGIN 시점부터 쓰기 잠금을 쥐므로, 같은 파일에 다른 커넥션으로 INSERT 하면
	// busy_timeout 을 통째로 기다린 뒤 SQLITE_BUSY 로 실패한다(실측 4.5초, 기록 0건).
	// 그러면 이벤트를 남길 가장 자연스러운 자리(원자화된 finish 같은 트랜잭션 안)에서
	// 계측이 **구조적으로 항상 0**이 되고, "0에 수렴하면 위험 신호"라는 §10 의 지표가
	// 거짓 양성이 된다.
	//
	// 그렇다고 같은 트랜잭션에 얹으면 롤백 때 함께 사라진다 — "무엇을 시도했다 실패했나"가
	// 감사 원장의 존재 이유이므로 그것도 안 된다.
	//
	// 그래서 버퍼에 모아 **커밋·롤백이 끝난 뒤** 별도 커넥션으로 흘린다. 두 성질을 다 지킨다.
	deferred []pendingEvent
}

type pendingEvent struct {
	kind      string
	project   string
	sessionID string
	payload   any
}

// LogEvent 는 트랜잭션이 끝난 뒤에 남길 계측 이벤트를 예약한다.
//
// **트랜잭션 안에서는 반드시 이것을 쓴다.** `Store.LogEvent` 를 그대로 부르면 교착한다 - 이벤트 추가도
// 같은 전역 잠금을 잡으므로(event.go), 자기가 쥔 잠금을 잠금 대기 상한까지 기다리다 죽는다(두 엔진 다).
// 롤백돼도 남는다 — 실패한 시도가 원장에서 사라지면 그 실패를 나중에 셀 수 없다.
func (t *Tx) LogEvent(kind, project, sessionID string, payload any) {
	t.deferred = append(t.deferred, pendingEvent{kind, project, sessionID, payload})
}

// Savepoint 는 fn 을 이 트랜잭션 안의 저장점에서 돌린다. fn 이 오류를 내면 저장점까지 되돌리고 그 오류를 그대로
// 낸다 - 트랜잭션은 살아 있어 뒤 문장과 커밋이 그대로 돈다. fn 이 성공하면 저장점을 풀고 쓴 것은 남긴다.
//
// ★ Tx 안에서 DB 오류를 받고 같은 Tx 로 이어 가는 자리는 반드시 이 안에서 부른다(Tx 주석의 「오류의 뜻」).
// Postgres 는 문장 하나가 실패하면 트랜잭션 전체를 버린다(25P02) - 저장점 없이 오류를 삼키면 앞서 쓴 것까지
// 커밋되지 않는다. SQLite 도 SAVEPOINT 를 같은 뜻으로 지원하므로 엔진 분기가 없다.
//
// ★ 되돌리기는 fn 전체다. fn 이 문장 둘을 쓰고 둘째에서 실패하면 첫째도 되돌아간다 - 반쯤 쓴 것을 남기지 않는다.
// 예약 이벤트(t.LogEvent)는 되돌리지 않는다 - 시도의 기록이라 트랜잭션 버퍼에 그대로 남는다.
//
// ★ 저장점을 만들거나 되돌리거나 풀지 못하면 fn 의 오류 대신 *SavepointError 를 내고 트랜잭션을 망가진 것으로
// 표시한다(그 뒤 문장과 커밋은 거절된다). 그 오류는 흡수하면 안 된다 - fn 의 오류(중복 · 없음)로 읽히지 않게
// 품지 않는다. fn 이 패닉해도 저장점까지 되돌린 뒤 패닉을 그대로 올린다 - 부른 쪽이 같은 트랜잭션에서 recover 하고
// 커밋하면 패닉 전에 쓴 것이 함께 커밋되기 때문이다.
//
// 흡수는 의도한 오류만 한다: 저장점 안에서 받은 오류라도 Unabsorbable 이면 위로 올린다.
//
// fn 은 같은 *Tx 를 받는다. 저장점은 중첩해도 된다(이름에 번호가 붙고, 되돌리기 · 풀기는 늘 가장 안쪽부터다).
func (t *Tx) Savepoint(fn func(*Tx) error) error {
	t.savepoints++
	name := "fd_sp_" + strconv.Itoa(t.savepoints)
	if _, err := t.tx.ExecContext(t.ctx, "SAVEPOINT "+name); err != nil {
		return t.savepointBroke(name, "만들기", nil, err)
	}
	mark := t.tx.cells.mark()
	returned := false
	defer func() {
		if !returned {
			_ = t.undoSavepoint(name, mark, errors.New("패닉"))
		}
	}()
	err := fn(t)
	returned = true
	if err != nil {
		if uerr := t.undoSavepoint(name, mark, err); uerr != nil {
			return uerr
		}
		return err
	}
	if _, rerr := t.tx.ExecContext(t.ctx, "RELEASE SAVEPOINT "+name); rerr != nil {
		return t.savepointBroke(name, "풀기", nil, rerr)
	}
	return nil
}

// SavepointValue 는 값을 돌려주는 fn 을 저장점 안에서 부르는 Savepoint 다(읽기를 저장점에 가둘 때의 꼴).
// fn 이 오류를 내면 값은 영값이다.
func SavepointValue[T any](t *Tx, fn func(*Tx) (T, error)) (T, error) {
	var v T
	err := t.Savepoint(func(sp *Tx) error {
		var err error
		v, err = fn(sp)
		return err
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}

// undoSavepoint 는 저장점까지 되돌리고 푼다. ROLLBACK TO 는 저장점을 남기므로, 안 풀면 같은 트랜잭션 안에 쌓인다. 저장점 안의
// 문장이 남긴 원장 칸 관문의 바꿔 넣기(mark 뒤의 것)도 버린다 - 원장에서 사라진 쓰기다.
func (t *Tx) undoSavepoint(name string, mark int, cause error) error {
	if _, err := t.tx.ExecContext(t.ctx, "ROLLBACK TO SAVEPOINT "+name); err != nil {
		return t.savepointBroke(name, "되돌리기", cause, err)
	}
	t.tx.cells.undo(mark)
	if _, err := t.tx.ExecContext(t.ctx, "RELEASE SAVEPOINT "+name); err != nil {
		return t.savepointBroke(name, "풀기", cause, err)
	}
	return nil
}

// savepointBroke 는 저장점 정리 실패를 *SavepointError 로 내고 트랜잭션을 망가진 것으로 표시한다.
func (t *Tx) savepointBroke(name, step string, cause, err error) error {
	e := &SavepointError{Name: name, Step: step, Err: err}
	if cause != nil {
		e.Cause = cause.Error()
	}
	t.tx.poison(e)
	return e
}

// SavepointError 는 저장점을 만들거나 되돌리거나 풀지 못한 것이다. 그 뒤로는 트랜잭션이 어디까지 살아 있는지 모른다 -
// SQLite 는 바깥 트랜잭션이 사라진 연결에서 뒤 문장을 자동 커밋으로 돌린다. 그래서 이것이 나면 그 트랜잭션은 문장 ·
// 커밋을 더 받지 않는다(txConn.broken).
//
// ★ fn 의 오류를 품지 않는다(Unwrap 은 실패한 저장점 문장의 오류다). 품으면 errors.Is · As 가 그것을 중복 · 없음으로
// 읽어 부른 쪽이 흡수하고 이어 간다. fn 의 오류는 문구(Cause)로만 싣는다.
type SavepointError struct {
	Name  string // 저장점 이름
	Step  string // 실패한 단계: 만들기 · 되돌리기 · 풀기
	Cause string // fn 이 낸 오류의 문구. fn 이 성공했거나 안 돌았으면 빈 값
	Err   error
}

func (e *SavepointError) Error() string {
	msg := fmt.Sprintf("저장점 %s %s 실패 - 이 트랜잭션은 더 쓸 수 없다: %v", e.Name, e.Step, e.Err)
	if e.Cause != "" {
		msg += " (저장점 안의 오류: " + e.Cause + ")"
	}
	return msg
}

func (e *SavepointError) Unwrap() error { return e.Err }

// insertUnlessTaken 은 `INSERT …` 한 문장에 `ON CONFLICT <liveKey> DO NOTHING RETURNING id` 를 붙여 돌린다.
// liveKey 의 부분 유니크 인덱스에 막혀 안 들어갔으면 inserted 가 거짓이고 오류는 없다 - 트랜잭션은 멀쩡하다(Tx 주석의
// 「오류의 뜻」). 다른 위반(PK 등 다른 유니크 · FK · CHECK · NOT NULL)은 그대로 오류로 온다.
//
// liveKey 는 「살아 있는 것은 하나」를 지키는 부분 유니크 인덱스의 정의 그대로다(칸과 WHERE). 두 엔진 모두 그 정의와
// 맞는 인덱스가 없으면 문장을 거절하므로(42P10 · SQLite 의 「ON CONFLICT clause does not match」) 어긋나면 시험이 잡는다.
func (t *Tx) insertUnlessTaken(insert, liveKey string, args ...any) (id int64, inserted bool, err error) {
	err = t.tx.QueryRowContext(t.ctx, unlessTaken(insert, liveKey), args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// unlessTaken 은 insertUnlessTaken 이 보내는 완성문이다. 이어 붙여 짓는 문장이라 원장 칸 관문의 꼴 시험이 이것으로 완성문을 잰다.
func unlessTaken(insert, liveKey string) string {
	return insert + `
		ON CONFLICT ` + liveKey + ` DO NOTHING
		RETURNING id`
}

// Open 은 DB 를 연다. **마이그레이션을 적용하지 않는다**(설계 §7 처방 ①).
//
// 스키마가 이 바이너리와 안 맞으면 거절하고 `fd migrate` 를 가리킨다. 적용은 그 명령의 일이다.
func Open(path string) (*Store, error) {
	return OpenWithLogger(path, slog.Default())
}

// OpenWithLogger 는 Open 에 로거를 주입한다.
//
// path 가 Postgres 접속 URL(postgres://…)이면 Postgres 원장에 붙은 뒤 판 재기(planOpen)를 지나야 연다 - 판이 이
// 바이너리와 같고 DB 로케일이 관문을 지나야 한다(기동 검사). 아니면 SQLite 파일이고, 붙기 전에 잰다(아래 ★).
// 가르는 것은 ParseTarget 이다.
func OpenWithLogger(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	t := ParseTarget(path)
	if t.Postgres() {
		s, err := openPG(t, log, lockTimeout)
		if err != nil {
			return nil, err
		}
		plan, err := planOpen(context.Background(), s.db)
		switch {
		case err != nil:
			err = fmt.Errorf("DB 상태를 재지 못했다(%s): %w - %s", s.path, err, migrateHint(t))
		case plan.Action != MigrateNone:
			err = openRefusal(plan, t)
		}
		if err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}
	// ★ **재기가 sql.Open 보다 앞에 선다.** sql.Open 은 없는 파일을 **만들기** 때문에,
	//   뒤에 두면 빈 DB 를 하나 만들어 놓고 "빈 DB 다" 라고 거절하게 되고 그 파일이 다음
	//   재기에는 "있는 DB" 로 보인다. ProbeMigration 머리말이 같은 함정을 적어 뒀다.
	//
	// ★ 여기서 적용하지 않는 근거는 store.go 마이그레이션 절에 있다 — 옛 판단("적용을 떼면
	//   fail-open 훅이 안 올라간 DB 를 만난다")의 전제가 전수 실측에서 거짓이었다.
	plan, perr := ProbeMigration(context.Background(), path)
	if perr != nil {
		return nil, fmt.Errorf("DB 상태를 재지 못했다(path=%q): %w — %s", clip(path, 200), perr, migrateHint(t))
	}
	if plan.Action != MigrateNone {
		return nil, openRefusal(plan, t)
	}

	return openRaw(path, log)
}

// openRaw 는 재기 없이 DB 를 연다. **Open 과 Migrate 가 나눠 쓰는 본체다.**
//
// ★ 이것이 따로 있어야 하는 이유: Migrate 는 정의상 "스키마가 안 맞는 DB" 를 열어야 하는데,
// Open 은 바로 그것을 거절한다. 한 함수로 두면 적용 경로가 자기 관문에 막힌다.
//
// ★ **패키지 밖으로 내보내지 않는다.** 이 문은 관문을 지나지 않으므로, 밖에서 부를 수 있으면
// 적용을 기동에서 분리한 의미가 없어진다 — 누구든 이것으로 열고 스키마를 만질 수 있다.
//
// SQLite 파일만 연다. Postgres 는 OpenWithLogger · MigrateWith 가 openPG 로 가른다.
//
// ★ 이관 표지가 있는 원장(Postgres 로 옮겼다)은 열지 않는다(movedRefusal). sql.Open 이 없는 파일을 **만들기** 때문에 이 문이
// 판올림(Migrate)이 빈 원장을 새로 만드는 유일한 자리다 - 덮개 파일을 빼고 띄운 compose 가 여기서 선다.
func openRaw(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := movedRefusal(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite 열기 실패(path=%q): %w", path, err)
	}
	// 동시 커넥션 상한. WAL 이라 읽기는 서로 막지 않고, 쓰기는 immediate 트랜잭션이
	// busy_timeout 안에서 줄을 선다. 무제한으로 두면 파일 핸들만 늘고 이득이 없다.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite 접속 실패(path=%q): %w", path, err)
	}

	s := &Store{db: &conn{raw: db, eng: engineSQLite}, path: path, log: log}

	// ★ DSN pragma 가 실제로 먹었는지 되읽어 확인한다.
	//   드라이버는 **모르는 pragma 이름을 조용히 무시한다**(실물로 확인: `_pragma=nonsense(1)` 이
	//   오류 없이 열린다). 그래서 "DSN 에 적었다"는 것은 "걸렸다"의 근거가 못 된다.
	//   foreign_keys 가 안 걸린 채로 도는 것은 FK 위반이 조용히 통과하는 최악의 경로다.
	if err := s.verifyPragmas(context.Background(), wantPragmas); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrateHint 는 적용이 기동에서 분리됐다는 사실과 **다음에 칠 것**을 낸다. 순수 함수다.
//
// ★ RollbackHint 와 같은 규율이다: 절차가 실패한 그 자리에 다음 수가 있어야 한다.
// 이 문구가 없으면 거절은 "안 뜬다" 로만 보이고, 그 상태에서 운영자가 할 수 있는 것은
// 같은 바이너리를 다시 띄우는 것뿐이다 — 그것이 §7 이 크래시루프라 부른 모양이다.
//
// Postgres 원장이면 `--db` 에 실을 것이 없다 - 표시값(pgShown)은 접속 URL 이 아니고, 원문은 비밀번호를 품는다. 접속 계정은
// 서버 계정(앱 역할)이 아니라 초사용자나 소유 역할이어야 한다 - 증분은 소유 역할로 돌고(engine.stepRole), 빈 DB 의 준비는
// 역할을 만든다.
func migrateHint(t Target) string {
	if t.Postgres() {
		return "적용은 기동에서 분리돼 있다(설계 §7 ①) - 같은 원장에 초사용자나 소유 역할(" + pg.OwnerRole +
			")로 접속해 `fd migrate` 를 먼저 돌려라"
	}
	return fmt.Sprintf("적용은 기동에서 분리돼 있다(설계 §7 ①) - `fd migrate --db %s` 를 먼저 돌려라", clip(t.Shown(), 200))
}

// openRefusal 은 Open 이 적용 없이 거절할 때의 사유다. 순수 함수다.
//
// ★ MigrateReject 를 가르는 이유: 그 갈래는 `fd migrate` 로 못 고친다(DB 가 이 바이너리보다
// 높거나 flightdeck 의 DB 가 아니다). 거기에 같은 처방을 붙이면 운영자가 고칠 수 없는 것을
// 고치려 들고, 그 시도가 다음 판올림 대상을 한 번 더 만진다.
func openRefusal(plan MigrationPlan, t Target) error {
	if plan.Action == MigrateNone {
		return nil
	}
	if plan.Action == MigrateReject {
		return fmt.Errorf("이 바이너리는 이 DB 를 아예 열지 않는다(스키마도 안 바꾼다): %s", plan.Reason)
	}
	return fmt.Errorf("스키마가 이 바이너리와 안 맞는다(%s): %s — %s", plan.Action, plan.Reason, migrateHint(t))
}

// OpenRefusal 은 기동(OpenWithLogger)이 계획 plan 의 원장 path 를 거절할 사유다. none 이면 nil 이다. fd selfcheck 가 기동과
// 같은 판정 · 같은 처방을 내려고 쓴다 - 거절 갈래(reject)와 `fd migrate` 로 고칠 갈래(apply · upgrade)의 문구가 한 벌이다.
func OpenRefusal(plan MigrationPlan, path string) error { return openRefusal(plan, ParseTarget(path)) }

// dsn 은 접속 문자열을 만든다.
//
// busy_timeout 은 잠금 대기 상한(lockTimeout, 스키마 주석 §2 의 `busy_timeout=5000`)이고,
// _txlock=immediate 는 Tx 주석의 실측 근거로 건다.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+busyTimeoutMS+")")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Set("_txlock", "immediate")
	return sqliteURI(path, q)
}

// sqliteURI 는 SQLite 파일 path 를 q 와 함께 여는 URI 접속 문자열(file:<경로>?<q>)이다. 기본 dsn · 원장 읽기(ledgerDSN) ·
// 이관 원본(importSourceDSN)이 이 하나로 짓는다.
//
// ★ 경로의 %, ?, # 를 인코딩한다. 드라이버는 file: 접속 문자열을 URI 로 열고(SQLITE_OPEN_URI), SQLite 는 URI 경로의 %XX 를
// 풀고 ? 와 # 에서 경로를 끊는다. 그대로 두면 `fd%20copy.db` 가 `fd copy.db` 를 열고(없으면 만든다), `a#b.db` 가 `a` 를
// 연다. 그 셋 말고는 SQLite 가 경로에서 손대는 글자가 없어 그대로 둔다(Windows 의 \ · 드라이브 글자도 그대로다).
func sqliteURI(path string, q url.Values) string {
	return "file:" + sqlitePath.Replace(path) + "?" + q.Encode()
}

var sqlitePath = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

// busyTimeoutMS 는 SQLite 의 busy_timeout pragma 값(밀리초)이다. Postgres 의 lock_timeout 과 같은 상한이다.
var busyTimeoutMS = millis(lockTimeout)

// wantPragmas 는 기본 DSN(dsn)이 요청한 pragma 와 기대값이다.
var wantPragmas = map[string]string{
	"busy_timeout": busyTimeoutMS,
	"foreign_keys": "1",
	"journal_mode": "wal",
}

// ledgerWantPragmas 는 원장 DSN(ledgerDSN)이 요청한 pragma 와 기대값이다.
//
// ★ journal_mode 가 없는 것이 요점이다. ledgerDSN 은 그것을 **일부러 요청하지 않는다**
// (파일을 바꿀 수 있는 유일한 pragma 라서다). 요청하지 않은 것을 되읽어 요구하면,
// 롤백저널 DB 마다 원인과 무관한 진단이 나온다 —
// "DSN pragma 가 실제로 걸리지 않았다: journal_mode=delete(기대 wal)".
// 그 문구는 DSN 문법을 의심하라고 말하는데 DSN 은 애초에 결백하다. 거짓 진단이다.
//
// 이 갈래가 없던 동안은 ProbeMigration 이 두 줄 위에서 파일을 WAL 로 바꿔 준 덕에
// 이 요구가 **우연히** 통과하고 있었다. 그 우연이 곧 결함이었다.
var ledgerWantPragmas = map[string]string{
	"busy_timeout": busyTimeoutMS,
	"foreign_keys": "1",
}

// CheckPragmas 는 되읽은 pragma 값들이 기본 DSN 의 기대와 맞는지 판정한다.
func CheckPragmas(got map[string]string) error { return CheckPragmasAgainst(wantPragmas, got) }

// CheckPragmasAgainst 는 되읽은 pragma 값들이 want 와 맞는지 판정한다.
//
// 불리언이 아니라 **어느 pragma 가 무슨 값이었는지**를 담은 오류를 돌려준다 —
// "안 걸렸다"만 알면 DSN 문법이 틀린 것인지 드라이버가 그 이름을 모르는 것인지 구분이 안 된다.
func CheckPragmasAgainst(want, got map[string]string) error {
	var bad []string
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s=<읽지 못함>(기대 %s)", name, w))
			continue
		}
		if !strings.EqualFold(g, w) {
			bad = append(bad, fmt.Sprintf("%s=%s(기대 %s)", name, g, w))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	// 정렬 없이 map 순회라 순서가 흔들리지만, 사유 전문이 목적이라 순서는 무의미하다.
	return fmt.Errorf("DSN pragma 가 실제로 걸리지 않았다: %s "+
		"— 드라이버가 모르는 pragma 이름을 조용히 무시하므로 DSN 문법을 확인하라", strings.Join(bad, ", "))
}

// readMigrationState 는 DB 의 스키마 상태를 읽는다. **순수 함수가 아니다** — 질의 셋을 던진다.
// 판정은 여기서 안 한다. 읽은 값을 PlanMigration(순수 함수)에 그대로 넘기는 것이 이 함수의 전부다.
//
// ★ Open 경로(마이그레이션 적용)와 검증 경로(ProbeMigration 읽기 전용)가 **같은 탐지를 쓴다**.
// 두 벌로 두면 한쪽만 고쳐져 실제 열기와 검증이 갈린다 — 낡은 탐지로 재기동을 승인하면
// 새 바이너리·새 DB 스키마가 실행 도중 맞지 않는 조용한 사고가 난다.
//
// ★ 엔진마다 다른 것은 카탈로그를 묻는 질의 둘뿐이다(engine.versionTableQuery · schemaObjectsQuery). 판정(PlanMigration)과
// 판 읽기는 한 벌이다 - 이전 실행기가 증분 트랜잭션 안에서 판을 다시 잴 때도 이것을 쓴다(stepReady).
func readMigrationState(ctx context.Context, db catalogReader) (hasTable bool, dbVersion, objects int, err error) {
	eng := db.dialect()
	// schema_version 테이블 존재 확인
	var n int
	if err := db.QueryRowContext(ctx, eng.versionTableQuery(), "schema_version").Scan(&n); err != nil {
		return false, 0, 0, fmt.Errorf("schema_version 테이블 존재 확인 실패: %w", err)
	}
	hasTable = n > 0

	// DB 버전 읽기
	if hasTable {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
			return false, 0, 0, fmt.Errorf("schema_version 읽기 실패: %w", err)
		}
		if v.Valid {
			dbVersion = int(v.Int64)
		}
	}

	// 객체 수 계산
	if err := db.QueryRowContext(ctx, eng.schemaObjectsQuery()).Scan(&objects); err != nil {
		return false, 0, 0, fmt.Errorf("스키마 객체 세기 실패: %w", err)
	}

	return hasTable, dbVersion, objects, nil
}

// planOpen 은 원장이 이 바이너리로 업무를 받아도 되는지 잰다 - 판(PlanMigration)과 DB 로케일(localeGate). 적용하지 않는다.
// 기동(OpenWithLogger)과 판 재기(ProbeMigration, fd selfcheck)가 두 엔진 모두 이 하나를 쓴다.
//
// ★ 로케일 거절은 MigrateReject 로 낸다. `fd migrate` 로 못 고치는 갈래(DB 를 새로 만들어 옮겨야 한다)라 openRefusal 이
// migrate 처방을 안 붙이고, selfcheck 가 「이 판으로 열면 거절된다」로 감시기의 재기동을 막는다. 읽기 실패는 오류다.
//
// ★ 빈 DB(MigrateApply)와 이미 거절인 판은 로케일을 안 잰다. 빈 DB 에는 아직 pg_trgm 이 없어 한글 trigram 을 잴 수
// 없다 - 이전 실행기가 기준선 단 안에서 잰다(stepBody). 그 갈래는 어차피 열리지 않는다.
func planOpen(ctx context.Context, q dbtx) (MigrationPlan, error) {
	hasTable, dbVersion, objects, err := readMigrationState(ctx, q)
	if err != nil {
		return MigrationPlan{}, err
	}
	plan := PlanMigration(hasTable, dbVersion, objects, SchemaVersion)
	if plan.Action == MigrateApply || plan.Action == MigrateReject {
		return plan, nil
	}
	// ★ 거절(*LocaleError)만 판정으로 바꾸고 읽기 실패는 그대로 낸다. 거절은 읽기가 다 성공한 뒤의 판정이라 q 가 트랜잭션이어도
	//   버려진 문장이 없다(savepoint_guard_test 의 도메인 오류 갈래).
	if err := localeGate(ctx, q); err != nil {
		var refusal *LocaleError
		if errors.As(err, &refusal) {
			return MigrationPlan{Action: MigrateReject, Reason: refusal.Error()}, nil
		}
		return MigrationPlan{}, err
	}
	return plan, nil
}

// verifyPragmas 는 want 에 적힌 pragma 만 되읽어 확인한다.
//
// ★ want 를 인자로 받는다. DSN 갈래마다 요청한 pragma 가 다르기 때문이다 —
// 요청하지 않은 pragma 를 확인하면 그 진단은 DSN 을 의심하라고 말하면서 거짓을 말한다
// (ledgerWantPragmas 주석 참고).
func (s *Store) verifyPragmas(ctx context.Context, want map[string]string) error {
	got := map[string]string{}
	for name := range want {
		var v string
		// pragma 이름은 상수 map 의 키라 주입 경로가 없다(사용자 입력이 닿지 않는다).
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
			return fmt.Errorf("PRAGMA %s 되읽기 실패: %w", name, err)
		}
		got[name] = v
	}
	return CheckPragmasAgainst(want, got)
}

// Close 는 DB 를 닫는다.
func (s *Store) Close() error { return s.db.Close() }

// Path 는 로그 · 출력에 실을 원장 자리다. SQLite 면 파일 경로, Postgres 면 접속 세부 없는 표시값(Target.Shown)이다.
func (s *Store) Path() string { return s.path }

// Postgres 는 이 원장이 Postgres 인가다. 참이면 Path 는 파일 경로가 아니다 - 파일 API(statfs · stat)에 넘기지 마라.
func (s *Store) Postgres() bool { return s.db.eng == enginePG }

// DB 는 저수준 핸들이다. 자리표시 감싸개를 안 지나므로 Postgres 에서는 `?` 질의가 안 돈다.
//
// ★ 시험 전용이다. store 밖 소스가 이것을 부르면 TestNoRawDatabaseOutsideStore 가 빨개진다 - 읽을 것이 필요하면
// store 에 접근자를 둔다. 접속 확인은 Ping 이다(날 핸들의 오류는 접속 문자열의 조각을 싣는다).
func (s *Store) DB() *sql.DB { return s.db.raw }

// ─────────────────────────────────────────────────────────────────────────────
// 트랜잭션 종류는 넷이다: Tx · ObsTx(runTx) · ReadTx · 실행기 세션 트랜잭션(sessionTx - 증분 적용 · SQLite 이관). 여는 자리는 engine.go 의
// begin 하나다(engine_test.go 가 소스로 잠근다).
// ─────────────────────────────────────────────────────────────────────────────

// Tx 는 fn 을 한 쓰기 트랜잭션 안에서 돌린다(전역 잠금). fn 이 오류를 내면 롤백하고 그 오류를 그대로 올린다.
func (s *Store) Tx(ctx context.Context, fn func(*Tx) error) error {
	t := &Tx{ctx: ctx, s: s}
	return s.runTx(ctx, txSpec{kind: txWrite}, func(tx *txConn) error {
		t.tx = tx
		return fn(t)
	}, t.flushDeferred)
}

// ObsTx 는 관측 적재 트랜잭션의 면이다. 클로저가 업무 *Tx 대신 이것을 받는다.
//
// ★ 타입으로 가르는 까닭: 관측 적재는 전역 잠금을 안 잡는다(Store.ObsTx). 업무 *Tx 를 넘기면 그 메서드
// (선점 · 세션 닫기처럼 업무 표를 쓰거나 이벤트를 곧장 넣는 것)를 관측 잠금만 쥔 채 부를 수 있고, 그러면 업무
// 표의 직렬성과 event.id 순서(= 커밋 순서)가 조용히 깨진다. ReadTx 가 Reader 를 받아 쓰기를 타입에서 빼는 것과
// 같은 방식이다. 관측 표에 쓰는 메서드는 관측 적재가 들어올 때 이 타입에만 붙인다 - 업무 메서드 · 이벤트 기록을
// 여기 붙이지 마라.
type ObsTx struct {
	tx  *txConn
	ctx context.Context
}

// Ctx 는 이 트랜잭션이 물고 있는 컨텍스트다.
func (o *ObsTx) Ctx() context.Context { return o.ctx }

// ObsTx 는 관측 적재 트랜잭션이다. 전역 잠금이 아니라 (기기, 저장소) 잠금을 잡는다 - 관측은 기기 × 저장소마다
// 잦게 오고, 관측 표 사이에는 선점 · 레인 같은 배타 불변식이 없어 업무 쓰기와 한 줄로 설 까닭이 없다.
// 같은 (기기, 저장소) 끼리만 한 줄로 선다. SQLite 에서는 Tx 와 같다(BEGIN IMMEDIATE 가 전역이다).
func (s *Store) ObsTx(ctx context.Context, device, repo string, fn func(*ObsTx) error) error {
	return s.runTx(ctx, txSpec{kind: txObs, device: device, repo: repo}, func(tx *txConn) error {
		return fn(&ObsTx{tx: tx, ctx: ctx})
	}, nil)
}

// runTx 는 쓰기 트랜잭션 하나를 끝까지 돌린다(Tx · ObsTx 의 본체). body 가 트랜잭션 핸들을 받고,
// after 는 커밋 · 롤백이 끝난 뒤 그 결말로 한 번 불린다(Tx 의 예약 이벤트 흘리기, 없으면 nil).
// 시작이 실패하면 body 도 after 도 안 불린다.
//
// 롤백 실패는 삼키지 않고 원래 오류에 합쳐 올린다 — 롤백이 실패하면 커넥션이 이상한 상태로
// 풀에 돌아가므로, 그 사실이 안 남으면 뒤따르는 무관한 실패의 원인을 영영 못 찾는다.
//
// 끝에서 오류를 한 번 옮긴다(asFault): 잠금 대기 초과 · 직렬화 실패 · 교착 · 버려진 트랜잭션은 *TxFault 로 나간다.
func (s *Store) runTx(ctx context.Context, sp txSpec, body func(*txConn) error, after func(committed bool)) error {
	tx, err := s.begin(ctx, sp)
	if err != nil {
		return asFault(fmt.Errorf("트랜잭션 시작 실패: %w", err))
	}
	// 업무 쓰기는 모두 여기를 지난다 - 원장 칸 관문(cellgate.go)을 단다. 바꿔 넣은 칸은 커밋된 뒤에만 로그로 낸다.
	tx.cells = &cellGate{}
	// ★ 안전망: body 가 패닉하면 아래 커밋 · 롤백 어느 쪽에도 닿지 않는다. 그러면 연결과 잠금(Postgres 의
	//   전역 · 관측 advisory 잠금, SQLite 의 쓰기 잠금)이 ctx 가 끝날 때까지 남고, 끝나지 않는 ctx 면 영영 남아
	//   뒤 쓰기가 전부 잠금 대기 상한으로 죽는다. 이미 끝난 트랜잭션의 Rollback 은 sql.ErrTxDone 만 낸다.
	//   예약 이벤트는 여기서 흘리지 않는다(flushDeferred 의 ★).
	defer func() { _ = tx.Rollback() }()
	if after == nil {
		after = func(bool) {}
	}
	// abort 는 실패한 트랜잭션을 끝낸다. **롤백이 먼저이고 예약 이벤트는 그 뒤다.** 예약 이벤트는 별도 연결로 같은
	// 전역 잠금을 잡고 쓰므로(Store.LogEvent), 이 트랜잭션이 열린 채 흘리면 자기 잠금을 잠금 대기 상한까지 기다리다
	// 기록을 잃는다. 커밋 거절 갈래가 그렇다 - 망가진 트랜잭션(txConn.broken)의 커밋은 드라이버에 안 가서 트랜잭션이
	// 열린 채 남는다. 롤백 뒤에 흘려야 실패한 시도가 원장에 남는다.
	abort := func(err error) error {
		rbErr := tx.Rollback()
		after(false)
		if rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("%w (롤백도 실패: %v)", err, rbErr)
		}
		return err
	}
	if err := body(tx); err != nil {
		return abort(asFault(err))
	}
	if err := tx.Commit(); err != nil {
		// ★ 커밋 실패도 롤백이다. 여기를 true 로 두면 아무것도 안 쓴 트랜잭션이 원장에서
		//   "썼다"고 말한다 — 롤백 갈래보다 되짚기 어려운 거짓이다(여기는 fn 이 오류를
		//   안 냈으므로 service 쪽 실패 로그조차 안 남는다). Postgres 에서 클로저가 문장 오류를
		//   삼키고 끝까지 가면 커밋이 ROLLBACK 으로 끝나 이 갈래로 온다(FaultAborted).
		return abort(asFault(fmt.Errorf("커밋 실패: %w", err)))
	}
	tx.cells.report(ctx, s.log)
	after(true)
	return nil
}

// ReadTx 는 fn 의 질의들을 한 스냅숏에서 돌린다. 잠그지 않는다 - 쓰기를 막지도, 쓰기에 막히지도 않는다.
//
// ★ 여러 질의가 같은 시점을 봐야 하는 읽기의 자리다. 단발 조회(s.db)는 문장마다 새 시점이라, 질의 사이에
// 닫힌 세션이 목록에는 있고 축에는 없는 카드가 나온다. Postgres 는 REPEATABLE READ READ ONLY 로 연다
// (읽기 전용 RR 은 직렬화 실패가 없다), SQLite 는 읽기 전용 BEGIN(WAL 스냅숏)이다.
//
// 읽기만 하므로 언제나 롤백으로 끝낸다. 커밋할 것이 없다.
func (s *Store) ReadTx(ctx context.Context, fn func(Reader) error) error {
	tx, err := s.begin(ctx, txSpec{kind: txRead})
	if err != nil {
		return asFault(fmt.Errorf("읽기 트랜잭션 시작 실패: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	return asFault(fn(tx))
}

// DeferredFlushBudget 는 flushDeferred 가 예약 이벤트 **전부**를 흘리는 데 쓰는 상한이다.
//
// 왜 상한이 필요한가. flushDeferred 는 트랜잭션의 ctx 에서 취소를 뗀 ctx 로 쓰는데,
// context.WithoutCancel 은 취소만이 아니라 **마감도 같이 버린다**(값만 남는다).
// 그러면 남는 대기가 둘 다 무제한이 된다: database/sql 의 커넥션 대기(MaxOpenConns=8 이
// 다 찼을 때, 자기 마감이 없는 요청은 자리가 날 때까지 영원히 선다)와 드라이버의 잠금
// 대기다. 거기 매달리면 계측 하나 때문에 상위 응답이 멈추는데, 그것은 LogEvent 가
// "계측이 기능을 죽이면 안 된다"로 세운 규율의 정반대다.
//
// **8초는 잰 값이 아니라 부등식 둘이 가둔 값이다. 양끝이 둘 다 이 저장소의 상수다.**
//
// busy_timeout(5s) < DeferredFlushBudget(8s) < api.ShutdownGrace(10s)
//
// 아래쪽 5초는 SQLite 가 잠금 하나를 기다리는 최댓값이다(dsn 의 busy_timeout).
// 예산이 그보다 작거나 같으면 정상적으로 줄 선 쓰기를 예산이 먼저 자른다 —
// 고치려던 유실을 다른 사유로 다시 만드는 것이다.
// 위쪽 10초는 종료 유예다(api.ShutdownGrace). 그 유예를 넘긴 인플라이트는 끊기고
// 곧 store 가 닫히므로, 예산이 그보다 크면 남는 시간에 할 수 있는 일이 "닫힌 DB 에
// 매달리기"뿐이다. 예산이 유예 안에서 끝나야 마지막 흘리기가 열린 DB 를 본다.
//
// 그 구간에서 8초를 고른 것은 아래로 3초, 위로 2초를 남기는 값이라서다.
// 두 부등식은 시험이 지킨다 — 아래쪽은 store(event_flush_detached_ctx_test.go), 위쪽은
// api(flush_budget_test.go). 자리가 갈리는 이유는 import 방향이다(api → store).
//
// ★ **공개인 이유가 그 위쪽 부등식이다.** ShutdownGrace 가 공개인 이유와 같다 —
// 여기서만 알면 그쪽이 같은 숫자를 다시 적게 되고, 두 벌은 반드시 표류한다.
const DeferredFlushBudget = 8 * time.Second

// flushCtx 는 예약 이벤트를 흘릴 ctx 를 만든다. 트랜잭션의 ctx 에서 취소와 마감만 떼고
// 값은 남긴 뒤 예산을 다시 건다.
//
// ★ 순수하게 판정 가능한 자리로 뺀다. 이 세 성질(값 보존 · 취소 비전파 · 마감 존재)을
// 동작으로만 재려면 store 안에 관측점이 없다 — 이 패키지는 ctx 값을 아무 데서도 안 읽고,
// 예산은 8초라 동작 시험이 그 시간을 통째로 쓴다. 판정을 순수 함수로 빼고 시험이 그
// 함수를 직접 부르는 것은 이 패키지의 규율이다(패키지 독 코멘트).
func flushCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), DeferredFlushBudget)
}

// flushDeferred 는 예약된 계측 이벤트를 별도 커넥션으로 흘린다.
// 트랜잭션이 끝난 뒤에만 불린다 — 그 전에 부르면 쓰기 잠금 때문에 교착한다.
//
// ★ **트랜잭션의 ctx 를 그대로 안 쓴다.** 그 ctx 는 요청의 것이고, 이 자리에 오는 가장 흔한
// 갈래가 바로 "그 요청이 끊겨서 트랜잭션이 죽었다"이다. 그대로 쓰면 취소된 ctx 로 INSERT 를
// 걸게 되어 database/sql 이 문을 보내기도 전에 되돌린다(실측: 롤백 갈래 기록 0건 ·
// `이벤트 기록 실패(…): context canceled` WARN 한 줄). 즉 **끊긴 시도일수록 원장에 안
// 남았다** — 남겨야 할 이유가 가장 큰 것이 정확히 그것이다. middleware.go 의 멱등 기록
// 저장이 같은 이유로 같은 관용구를 쓴다("끊는 클라이언트일수록 멱등 기억이 안 남는다 —
// 재시도하는 쪽이 정확히 그 클라이언트다").
//
// ★ 값은 남긴다. context.Background() 로 갈아타면 api 의 요청 상관 정보(reqInfo)가 끊겨,
// 이 층이 나중에 WarnContext 로 옮겨도 그 상관키가 안 붙는다.
//
// ★ 예산은 **한 번**이다(DeferredFlushBudget). 예약 이벤트마다 걸면 이벤트 수만큼 곱해져
// (Finish 는 후속마다 한 건을 예약할 수 있다) 상한이 사실상 사라진다.
//
// ★ 그래도 안 남는 갈래가 있다. BeginTx 가 실패하면 클로저를 아예 안 부르므로 예약 자체가
// 없고(TestTxBeginFailureLeavesNothingReserved), 쓰기가 실패하면 LogEvent 가 WARN 으로
// 삼키고, 프로세스가 여기 닿기 전에 죽으면 버퍼가 메모리째 사라진다. 클로저 안 패닉도
// 마찬가지다. runTx 의 안전망 defer 는 **롤백만** 한다. 그 목적은 잠금을 풀어 뒤 쓰기를 살리는
// 것이고, 되감기 도중에 별도 커넥션으로 잠금을 다시 잡으며 쓰는 일까지 얹지는 않는다.
// 그래서 이 수를 읽는 표면의 "하한이다" 단서는 이 수정 뒤에도 안 뗀다.
//
// ★ committed 를 받는다. 여기가 **트랜잭션의 결말을 아는 유일한 자리**다 — 호출자(service)는
// fn 이 오류를 냈는지만 알고 커밋 자체가 실패한 갈래는 모른다. 그 사실을 payload 에 안 찍으면
// 소비자가 항목 상태로 되추론해야 하는데, 그 되추론은 실측으로 죽었다
// (event.go 의 markTxOutcome · QueueReproduction · DESIGN §10).
func (t *Tx) flushDeferred(committed bool) {
	if len(t.deferred) == 0 {
		return
	}
	ctx, cancel := flushCtx(t.ctx)
	defer cancel()
	for _, e := range t.deferred {
		t.s.LogEvent(ctx, e.kind, e.project, e.sessionID, markTxOutcome(e.payload, committed))
	}
	t.deferred = nil
}

// Ctx 는 이 트랜잭션이 물고 있는 컨텍스트다.
func (t *Tx) Ctx() context.Context { return t.ctx }

// ─────────────────────────────────────────────────────────────────────────────
// 마이그레이션 — 판정은 순수 함수, 실행만 여기
// ─────────────────────────────────────────────────────────────────────────────
//
// ★ **설계 §7 과 맞춰진 자리다(2026-08-23).** 2026-08-03 부터 이 자리에는 "§7 과 어긋난다,
// 다음 세션이 '설계대로 돼 있다'고 믿지 않게 적는다"는 경고가 서 있었다. 그 경고가 무엇을
// 했는지 남긴다 — 없는 처방에 **미리 이름이 붙어 있던 덕에**, 지을 때가 왔을 때 무엇을
// 지어야 하는지를 다시 발견할 필요가 없었다.
//
// §7 의 "나쁜 스키마 변경으로 크래시루프" 행은 처방을 셋으로 적었고, 이제 **셋 다 있다**:
//
//	① 마이그레이션을 **별도 one-shot 으로 분리** — compose.yaml 의 migrate 서비스가
//	   `fd migrate` 를 돌고, 서버는 service_completed_successfully 로 그것이 성공으로
//	   끝난 뒤에만 뜬다. 이 파일의 Open 은 **어떤 적용도 하지 않는다.**
//	② 기동 전 DB 백업 — PlanMigration 이 판정하고 VACUUM INTO 로 뜬다.
//	③ 롤백 명령 — `fd migrate --rollback` 이 <db>.bak-* 중 최신(이 원장의 온전한 사본일 때만)으로 되돌린다.
//	   인덱스만 더한 판은 `fd migrate --to N` 이 기록을 잃지 않고 내린다(DownSteps).
//
// **적용을 기동에 남겼던 옛 판단과 그것이 뒤집힌 경위.** 근거는 한 문장이었다: "적용을 떼면
// 모든 명령(fail-open 훅 4종 포함)이 '스키마가 아직 안 올라간 DB' 를 만나는 새 경로가 생기고,
// 훅은 정의상 조용히 죽으므로 그 실패가 침묵한다."
//
// 그 전제가 전수 실측에서 **거짓**이었다(2026-08-23). cmd/ 에서 DB 를 여는 문은 셋뿐이고
// (serve · 이관용 openDB · OpenLedger), **훅 여섯은 하나도 DB 를 열지 않는다** — beatFromHook 은
// a.cli.Write 로 REST 를 치고 실패하면 아웃박스로 흐른다. failopen_test.go 의 "fail-open" 은
// **큐 잠금** 축이지 DB 스키마 축이 아니다. 그리고 셋 중 OpenLedger 는 이미 ProbeMigration →
// 거절 패턴이었다. 새로 발명한 것이 아니라 남은 둘에 같은 것을 두른 일이다.
//
// ★ **절반만 뗄 수는 없었다.** MigrateApply(빈 DB 신규 설치) 갈래도 증분 적용(applySteps)을 타므로
// (아래 그 자리의 주석), 업그레이드만 분리하면 크래시루프 경로가 절반 남는다. 그리고
// neverExempt 가 DROP TABLE 을 여는 근거가 "적용이 기동 밖에 있다" 인데, 신규 설치에서
// 여전히 안이면 그 조작이 기동 경로에서 돌아 근거 자체가 안 선다.
//
// ★ 그 위에서 증분 011 이 item_dependents 를 **DROP TABLE** 했다(항목
// fd-drop-item-dependents-needs-fd-migrate-first). 010 이 값을 비우고 011 이 구조를 걷는
// 두 단계였고, 011 이 가능해진 것은 이 절의 ①·③ 이 먼저 지어졌기 때문이다.
//
// ★ 그래서 파괴적 증분은 무조건 막는 대신 **근거를 요구하는 예외**로 통과시킨다.
// migrate_guard_test.go 의 destructiveExempt 가 증분 번호마다 (허용 조작, 사유) 를 담고,
// 사유가 비면 예외로 안 쳐 준다. 구조가 사라지는 조작(DROP TABLE·DROP COLUMN·RENAME)은
// neverExempt 라 그 예외로도 못 연다 — 그 셋을 "다른 시험이 본다" 고 적어 뒀던 것이
// 실측으로 거짓이었기 때문이다(지목된 두 시험이 DROP TABLE 을 둘 다 통과시켰다).
//
// **그 조건은 2026-08-23 에 충족됐고 처방이 이행됐다.** RollbackHint 는 이제 문구로 자리를
// 메우는 것이 아니라 **실제 명령을 가리킨다** — 절차가 실패한 그 자리에 수단이 있어야
// 한다는 규율은 그대로이고, 가리키는 곳만 바뀌었다.
//
// ★ **그 조건은 이제 시험이 지킨다** — TestBundledMigrationsAreAdditive(migrate_guard_test.go).
// 만료 조건이 문서와 주석에만 있으면 아무도 그 순간을 안 본다. 파괴적 증분이 들어오는 날
// 이 판단은 근거를 잃는데, 그 사실이 어디에도 안 뜨면 **만료된 판단 위에서 계속 돌게 된다** —
// 이 절이 없애려던 위험("설계대로 돼 있다고 믿는 것")이 정확히 그 모양이다.

// Migrate 는 DB 를 이 바이너리가 아는 스키마 버전까지 올린다. **`fd migrate` 의 본체다.**
//
// ★ 이 함수가 존재하는 것이 설계 §7 처방 ①(적용을 기동에서 분리)의 이행이다. 그 전까지
// 적용은 Open() 안에 있었고, 그래서 나쁜 증분은 "서버가 안 뜬다" 로 나타났으며 고칠 수단도
// 같은 바이너리를 다시 띄우는 것뿐이었다 — §7 이 크래시루프라 부른 모양이다.
//
// ★ **판정과 백업은 그대로 s.migrateTo 가 진다.** 여기서 다시 판정하면 판정이 두 벌이 되고,
// 두 벌은 반드시 표류한다. 이 함수가 더하는 것은 "누가 언제 부르는가" 하나다.
func Migrate(ctx context.Context, path string, log *slog.Logger) error {
	return MigrateWith(ctx, path, MigrateOptions{}, log)
}

// MigrateTo 는 target 까지만 올린다. `fd migrate --to N` 의 본체다.
func MigrateTo(ctx context.Context, path string, target int, log *slog.Logger) error {
	return MigrateWith(ctx, path, MigrateOptions{Target: target}, log)
}

// MigrateOptions 는 이전 실행기 한 번의 선택이다. 0 값은 「이 바이너리가 아는 최신 판까지, 덤프 자리 없음」이다.
type MigrateOptions struct {
	// Target 은 이 판까지만 올린다. 0 이면 SchemaVersion 이다.
	Target int
	// BackupDir 은 Postgres 원장의 판올림 전 덤프(pg_dump -Fc) 자리다. 기존 데이터 위에 증분을 얹는데 비어 있으면
	// 거절한다 - 덤프 없이 판을 올리지 않는다(dumpPG). SQLite 는 안 쓴다(사본이 원장 옆에 뜬다).
	BackupDir string
	// DumpTimeout 은 Postgres 판올림 전 덤프 한 번의 기한이다(FD_DUMP_TIMEOUT). 0 이면 DefaultDumpTimeout 이다. 같은 값이
	// 스냅숏 트랜잭션의 서버 상한과 pg_dump 자식의 문맥에 걸린다(snapshotTxLimits · backupPG). SQLite 는 안 쓴다.
	DumpTimeout time.Duration
	// Moved 는 이 Postgres 원장으로 옮긴 SQLite 원장의 자리다. 그 옆 이관 표지가 연 되돌리기 창(RollbackWindow) 안이면 인덱스만
	// 더한 것이 아닌 증분을 얹지 않는다(rollbackWindowGate). 비면 창을 재지 않는다. SQLite 는 안 쓴다.
	Moved string
	// AllowInRollbackWindow 는 그 창 안에서도 얹는다(`fd migrate --allow-in-rollback-window`) - 옛 판으로 되돌리지 않기로 한
	// 사람의 선택이다.
	AllowInRollbackWindow bool
}

// MigrateWith 는 원장을 opt 대로 올린다. 엔진은 path 로 가른다(ParseTarget).
func MigrateWith(ctx context.Context, path string, opt MigrateOptions, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	s, run, err := openMigrator(path, opt, log)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.migrateTo(ctx, run)
}

// openMigrator 는 이전 실행기가 쓸 원장(판 재기 없이 연다)과 그 엔진의 실행 입력을 낸다.
//
// ★ 두 엔진이 같은 실행기(migrateTo)를 탄다. 엔진마다 다른 것은 여기서 고르는 셋이다 - 여는 길(openRaw · openPG),
// 기준선 위 증분 목록(migrations · pgMigrations), 판올림 전 백업(원장 옆 VACUUM INTO 사본 · pg_dump 덤프). 기준선을
// 까는 길은 applyBase 가 가른다.
func openMigrator(path string, opt MigrateOptions, log *slog.Logger) (*Store, migrateRun, error) {
	run := migrateRun{target: opt.Target}
	if run.target == 0 {
		run.target = SchemaVersion
	}
	t := ParseTarget(path)
	if t.Postgres() {
		limit, err := opt.dumpTimeout()
		if err != nil {
			return nil, run, err
		}
		s, err := openPG(t, log, lockTimeout)
		if err != nil {
			return nil, run, err
		}
		run.steps = pgMigrations
		if opt.Moved != "" && !opt.AllowInRollbackWindow {
			run.window = func(steps []Migration) error { return rollbackWindowGate(opt.Moved, steps, time.Now()) }
		}
		run.backup = func(ctx context.Context, sess *migrateSession, from int) (string, error) {
			return s.backupPG(ctx, t, sess, from, opt.BackupDir, limit)
		}
		return s, run, nil
	}
	s, err := openRaw(path, log)
	if err != nil {
		return nil, run, err
	}
	run.steps = migrations
	run.backup = func(ctx context.Context, _ *migrateSession, from int) (string, error) {
		return s.backup(ctx, from, time.Now())
	}
	return s, run, nil
}

// RollbackHint 는 SQLite 판올림이 깨졌을 때 오류에 붙이는 다음 수다. 순수 함수다. dbPath 는 그 원장의 경로다 - 부르는 쪽이
// 절대 경로로 준다(rollbackHint).
//
// ★ 이 문구가 오류와 로그에 실린다. 앞선 판은 업그레이드 경로에서 백업 경로를
// **지역 변수로 버려서**, 정확히 §7 이 겨냥한 상황(다음 증분이 깨지는 날)에
// 오류가 "무엇을 얹다 실패했다"만 말하고 **어디로 되돌리는지는 말하지 못했다.**
// 적용 경로(MigrateApply)는 backup=%q 를 실었는데 업그레이드 경로는 안 실었다 —
// 같은 상황을 다루는 두 경로가 다르게 생겼으면 한쪽이 틀린 것이다.
//
// ★ 명령에 **대상 원장을 셸 인용해 싣는다**(`--db '<경로>'`). 앞선 판은 `fd migrate --rollback` 만 적어, `--db A` 로 올리다 선
// 사람이 그대로 돌리면 기본 원장(FD_DB · ~/.flightdeck/fd.db) B 를 되돌렸다 - B 옆에 온전한 백업이 있으면 모든 재기를 지나 B 를
// 덮는다. 손으로 파일을 옮기는 처방은 싣지 않는다: 그 처방은 옆 파일 하나를 못 옮겨도 다음 줄(cp -f)이 원장을 덮었다. 재기 · 지금
// 원장 남기기 · 옆 파일 옮기기는 명령이 한다(rollbackSQLite).
func RollbackHint(dbPath, backupPath string) string {
	cmd := "fd migrate --rollback --db " + shellQuote(dbPath)
	if strings.TrimSpace(backupPath) == "" {
		return "이번 적용은 백업을 뜨지 않았다(빈 DB 이거나 메모리 DB 다) — 이번 판올림이 남긴 되돌릴 파일이 없다. " +
			"그 원장 옆의 옛 백업(.bak-*)은 서버를 멈추고 `" + cmd + "` 가 재고 그중 최신을 쓴다."
	}
	return fmt.Sprintf("실패한 단은 트랜잭션째 되감겼다(원장은 마지막으로 커밋된 판에 있다). 판올림 전 백업은 %s 이다. 이미 커밋된 "+
		"단까지 물리려면 서버를 멈추고 `%s` 를 돈다 - 백업을 재고(다 뜬 이 원장의 사본인가, 그 뒤로 커밋된 판올림이 있는가) 지금 "+
		"원장을 옆 파일까지 <원장>.pre-rollback-<시각> 으로 남긴 뒤 덮는다. 커밋된 단이 없으면 거절한다(무를 것이 없다).",
		clip(backupPath, 200), cmd)
}

// MigrationAction 은 여는 시점에 무엇을 할지다.
type MigrationAction string

const (
	MigrateNone    MigrationAction = "none"    // 이미 맞다
	MigrateApply   MigrationAction = "apply"   // 스키마를 새로 적용한다(그 뒤 증분이 따라온다)
	MigrateUpgrade MigrationAction = "upgrade" // 기존 DB 에 증분을 얹는다
	MigrateReject  MigrationAction = "reject"  // 열지 않는다
)

// MigrationPlan 은 판정 결과다. Reason 은 **항상** 채운다 —
// 사유가 없으면 "조건 때문에 거절"과 "이 축을 안 본다"가 구분되지 않는다.
type MigrationPlan struct {
	Action MigrationAction
	Backup bool
	Reason string
}

// PlanMigration 은 DB 의 현재 상태만 보고 무엇을 할지 정한다. 순수 함수다.
//
//   - hasVersionTable: schema_version 테이블이 있는가
//   - dbVersion:       그 표의 MAX(version). 표가 없거나 행이 없으면 0
//   - objectCount:     sqlite_master 의 객체 수. 0 이면 완전히 빈 파일이다
//   - codeVersion:     이 바이너리가 아는 버전(SchemaVersion)
//
// 백업 판정을 여기에 둔 이유: "언제 백업하는가"가 조건 넷의 조합이라
// 본문에 흩어지면 시험이 그 조합의 사본을 단정하게 된다.
func PlanMigration(hasVersionTable bool, dbVersion, objectCount, codeVersion int) MigrationPlan {
	switch {
	case !hasVersionTable && objectCount == 0:
		return MigrationPlan{
			Action: MigrateApply, Backup: false,
			Reason: "빈 DB 다 — 스키마를 새로 적용한다(잃을 것이 없으므로 백업하지 않는다)",
		}

	case !hasVersionTable && objectCount > 0:
		// 여기서 스키마를 덮어 적용하면 남의 DB 를 망가뜨리거나, 중간에 끊긴
		// 마이그레이션 위에 또 얹게 된다. 둘 다 조용히 망가지는 경로다.
		return MigrationPlan{
			Action: MigrateReject, Backup: false,
			Reason: fmt.Sprintf("schema_version 표가 없는데 객체가 %d개 있다 "+
				"— flightdeck 의 DB 가 아니거나 마이그레이션이 중간에 끊긴 것이다. 손으로 확인하라", objectCount),
		}

	case dbVersion == 0 && objectCount <= 1:
		// 표는 있는데 행이 없고, 그 표 말고는 아무것도 없다 =
		// 앞선 적용이 schema_version 만 만들고 곧바로 끊겼다. 안전하게 다시 적용할 수 있다.
		// 파일이 이미 있으므로 백업은 뜬다(비용이 거의 없고, 여기서 안 뜨면 백업 경로가 통째로 죽는다).
		return MigrationPlan{
			Action: MigrateApply, Backup: true,
			Reason: "schema_version 표만 있고 기록된 버전이 없다 — 앞선 적용이 곧바로 끊긴 것으로 보고 다시 적용한다",
		}

	case dbVersion == 0:
		// ★ 객체는 있는데 버전 기록이 없다. **다시 적용하면 안 된다** —
		//   schema.sql 은 멱등이 아니다(IF NOT EXISTS 는 schema_version 하나뿐이라
		//   재적용이 "table already exists" 로 죽고, 그 시점의 DB 는 반쯤 적용된 채 남는다).
		//   조용히 시도해 실패하는 것보다 사유를 말하고 멈추는 쪽이 복구 가능하다.
		return MigrationPlan{
			Action: MigrateReject, Backup: false,
			Reason: fmt.Sprintf("스키마 객체가 %d개 있는데 schema_version 에 기록된 버전이 없다 "+
				"— 기준선(SQLite schema.sql · Postgres pg/001_baseline.sql)은 멱등이 아니라 재적용이 실패한다. "+
				"앞선 마이그레이션이 중간에 끊긴 것이므로 판올림 전 백업(SQLite <db>.bak-* · Postgres fd-<원장>-from<판>-<시각>.dump)에서 "+
				"되돌리거나 손으로 버전을 기록하라", objectCount),
		}

	case dbVersion > codeVersion:
		// ★ 구 바이너리가 신 DB 를 여는 것. 조용히 열면 모르는 컬럼·제약 위에서 돌게 된다.
		return MigrationPlan{
			Action: MigrateReject, Backup: false,
			Reason: fmt.Sprintf("DB 스키마 버전이 %d 인데 이 바이너리는 %d 까지만 안다 "+
				"— 구 바이너리로 신 DB 를 여는 것은 조용히 망가지는 경로라 거절한다. 바이너리를 올려라",
				dbVersion, codeVersion),
		}

	case dbVersion < codeVersion:
		// ★ 여기서 경로 유무를 판정하지 않는다 — 그것은 증분 목록을 봐야 알 수 있어
		//   이 함수가 순수하지 않게 된다. 실제 경로 해석은 UpgradeSteps 가 하고,
		//   빠진 단이 있으면 **거기서** 사유를 담아 거절한다.
		//   백업은 무조건 뜬다: 나쁜 마이그레이션은 1인 운영에서 복구 불가 사건이다.
		return MigrationPlan{
			Action: MigrateUpgrade, Backup: true,
			Reason: fmt.Sprintf("DB 스키마 버전이 %d 이고 이 바이너리는 %d 를 기대한다 "+
				"— %d→%d 증분을 얹는다(적용 전에 백업한다)", dbVersion, codeVersion, dbVersion, codeVersion),
		}

	default:
		return MigrationPlan{
			Action: MigrateNone, Backup: false,
			Reason: fmt.Sprintf("스키마 버전 %d 로 이미 맞다", dbVersion),
		}
	}
}

// UpgradeSteps 는 from 에서 to 까지 얹을 증분을 순서대로 고른다. 순수 함수다.
//
// ★ 한 단이라도 빠지면 **사유를 담아 거절한다.** 조용히 건너뛰면 "모르는 구 스키마 위에서
// 도는" 상태가 침묵으로 열리고, 그 침묵은 그 표를 처음 읽는 질의가 죽을 때까지 안 보인다.
// 사유에 무엇이 있는지까지 적는 이유: "1→3 경로가 없다"만으로는 2가 없는 것인지
// 3이 없는 것인지 운영자가 못 가린다.
func UpgradeSteps(from, to int, avail []Migration) ([]Migration, error) {
	if from > to {
		return nil, fmt.Errorf("내려가는 마이그레이션은 없다(%d→%d)", from, to)
	}
	have := map[int]Migration{}
	var haveList []int
	for _, m := range avail {
		have[m.To] = m
		haveList = append(haveList, m.To)
	}
	var out []Migration
	for v := from + 1; v <= to; v++ {
		m, ok := have[v]
		if !ok {
			return nil, fmt.Errorf("%d→%d 업그레이드 경로가 없다 — %d 로 올리는 증분이 없다(가진 증분: %v)",
				from, to, v, haveList)
		}
		out = append(out, m)
	}
	return out, nil
}

// migrateRun 은 이전 실행기 한 번의 입력이다. 엔진마다 다른 것을 여기서 받는다(openMigrator 가 채운다).
type migrateRun struct {
	target int
	// steps 는 기준선 위 증분이다(migrations · pgMigrations). 시험이 바꿔 넣는다.
	steps []Migration
	// backup 은 판올림 전 백업을 뜨고 그 자리를 낸다. sess 는 실행기 세션(잠금을 쥔 연결)이고 from 은 덤프를 뜨는 판이다 -
	// 덤프는 최종 이름을 붙이기 전에 그 세션에서 잠금을 다시 잰다(sealDump).
	backup func(ctx context.Context, sess *migrateSession, from int) (string, error)
	// window 는 얹을 증분을 되돌리기 창이 막는 사유다(rollbackWindowGate). nil 이면 재지 않는다(SQLite · 표지 자리를 모르는
	// 원장 · 플래그).
	window func(steps []Migration) error
}

// migrateTo 는 run.target 까지 올린다. 두 엔진이 같은 길을 탄다:
// 세션(잠금) → 판 재기 → 판정 → (경로 · 로케일 확인) → 백업 → 기준선 → 증분 → 끝.
//
// ★ target 을 받는 것이 `fd migrate --to N` 의 실질이다. 나쁜 증분이 N 단에 있으면
// N-1 까지만 올려 두고 옛 바이너리로 계속 돌 수 있다 — 그 선택지가 없으면 판올림은
// 전부 아니면 전무이고, 그것이 설계 §7 이 크래시루프라 부른 상태의 절반이다.
//
// ★ 판 재기부터 마지막 증분까지가 한 세션(openMigrateSession)이다. Postgres 는 그 세션이 판을 읽기 전에 자기 잠금과 전역
// 업무 잠금을 함께 잡고 끝까지 쥔다 - 실행기끼리는 자기 잠금이, 실행기와 업무 쓰기는 업무 잠금이 가른다. 그래서 읽은 판 ·
// 덤프 · 증분이 한 상태를 본다: 판을 읽은 뒤 남이 판을 올리지 못하고, 덤프 뒤 첫 증분 전에 업무 쓰기가 끼지 못한다.
func (s *Store) migrateTo(ctx context.Context, run migrateRun) error {
	sess, err := s.openMigrateSession(ctx)
	if err != nil {
		return err
	}
	defer sess.release()

	hasTable, dbVersion, objects, err := readMigrationState(ctx, sess)
	if err != nil {
		return err
	}
	// ★ 이 바이너리가 아는 판에서 더 낮은 --to 는 내림이다(migrateDown). 모르는 판(SchemaVersion 위)은 PlanMigration 이 거절한다 -
	//   그 판의 증분을 모르면 무엇을 지울지도 모른다.
	if hasTable && dbVersion > run.target && dbVersion <= SchemaVersion {
		return s.migrateDown(ctx, sess, dbVersion, run)
	}
	plan := PlanMigration(hasTable, dbVersion, objects, run.target)

	switch plan.Action {
	case MigrateNone:
		// 판이 맞아도 로케일이 틀리면 서버가 안 뜬다(OpenWithLogger). one-shot 이 성공으로 끝나고 서버만 죽는 것보다 여기서
		// 같은 판정으로 멈추는 편이 사유가 한 자리에 모인다.
		if err := localeGate(ctx, sess); err != nil {
			return err
		}
		s.log.Info("스키마 확인", "path", s.path, "version", dbVersion, "reason", plan.Reason)
		return nil
	case MigrateReject:
		// 열지 못한 사유는 원인 전문으로 남긴다. 이 오류가 곧 운영자가 볼 유일한 단서다.
		s.log.Error("스키마 버전 불일치로 DB 를 열지 않는다",
			"path", s.path, "db_version", dbVersion, "code_version", run.target, "reason", plan.Reason)
		return fmt.Errorf("스키마 거절: %s", plan.Reason)
	case MigrateApply, MigrateUpgrade:
	default:
		return fmt.Errorf("알 수 없는 마이그레이션 판정: %q (%s)", plan.Action, plan.Reason)
	}

	// ★ 얹을 증분을 백업 · 적용 **전에** 고른다. 빠진 단이 있으면 아무것도 안 바꾸고 거절한다. 뒤에서 알면 백업만 하나
	//   늘고, Postgres 는 기준선이 커밋된 채 남는다(기준선 18판보다 낮은 --to 가 그 모양이다).
	// ★ 신규 DB 도 증분을 그대로 탄다. 신규용으로 기준선에 같은 표를 또 적으면 정의가 두 벌이 되고, 신규 설치와
	//   업그레이드가 다른 모양의 DB 를 갖게 된다.
	from := dbVersion
	if plan.Action == MigrateApply {
		from = s.db.eng.baseVersion()
	}
	steps, err := UpgradeSteps(from, run.target, run.steps)
	if err != nil {
		s.log.Error("업그레이드 경로가 없어 DB 를 올리지 않는다",
			"path", s.path, "db_version", dbVersion, "code_version", run.target, "error", err.Error())
		return fmt.Errorf("스키마 거절: %w", err)
	}
	// ★ 되돌리기 창(run.window) · 로케일 · 인덱스 키 상한(keyLimitGate)은 백업 · 증분 전에 잰다 - 거절될 DB 의 덤프를 뜨고 증분을 얹을 까닭이 없다. 빈 DB 는
	//   로케일을 기준선 단 안에서 잰다(pg_trgm 이 거기서 생긴다, stepBody).
	if plan.Action == MigrateUpgrade {
		if run.window != nil {
			if err := run.window(steps); err != nil {
				s.log.Error("되돌리기 창 안이라 DB 를 올리지 않는다", "path", s.path, "db_version", dbVersion, "error", err.Error())
				return err
			}
		}
		if err := localeGate(ctx, sess); err != nil {
			return err
		}
		if err := keyLimitGate(ctx, sess, dbVersion); err != nil {
			return err
		}
	}

	start := time.Now()
	s.log.Info("마이그레이션 시작",
		"path", s.path, "from", dbVersion, "to", run.target, "backup", plan.Backup, "reason", plan.Reason)
	// ★ backupPath 를 실패 문구까지 끌고 간다. 앞선 판은 업그레이드 경로에서 이것을 지역 변수로 버려,
	//   정확히 설계 §7 이 겨냥한 상황에서 유일한 탈출구의 좌표가 사라졌다.
	var backupPath string
	if plan.Backup {
		// ★ 나쁜 마이그레이션은 1인 운영에서 복구 불가 사건이다. 적용 **전에** 뜬다.
		if backupPath, err = run.backup(ctx, sess, dbVersion); err != nil {
			return err
		}
		s.log.Info("마이그레이션 전 백업 완료", "path", s.path, "backup", backupPath)
	}
	if plan.Action == MigrateApply {
		if err := s.applyBase(ctx, sess); err != nil {
			return s.rollbackable(err, backupPath)
		}
	}
	if err := s.applySteps(ctx, sess, steps); err != nil {
		return s.rollbackable(err, backupPath)
	}
	s.log.Info("마이그레이션 완료",
		"path", s.path, "version", run.target, "duration", time.Since(start).Seconds())
	return nil
}

// DownSteps 는 from 판에서 to 판으로 내릴 증분을 내리는 순서(위 판부터)로 고른다. 순수 함수다. `fd migrate --to N`(N 이 지금
// 판보다 낮다)의 판정이다.
//
// ★ 인덱스만 더한 증분(pg.IndexOnly)만 내린다. 그 인덱스를 지우고 판 행을 지우면 표 · 칸 · 행이 그대로라 기록을 하나도 잃지
// 않는다 - 판올림 전 사본으로 덮는 `fd migrate --rollback` 이 그 사본 뒤의 기록을 잃는 것과 다르다. 내림 SQL 은 따로 적지 않고
// 올림 SQL 에서 읽는다(pg.IndexOnly 의 ★). 하나라도 다른 증분이 끼거나 증분 목록에 없는 판(기준선)에 닿으면 아무것도 안 바꾸고
// 사유를 담아 거절한다 - 일부만 내려 두는 일이 없다.
func DownSteps(from, to int, avail []Migration) ([]Migration, error) {
	if to >= from {
		return nil, fmt.Errorf("내림은 더 낮은 판으로만 간다(%d→%d)", from, to)
	}
	have := map[int]Migration{}
	for _, m := range avail {
		have[m.To] = m
	}
	var out []Migration
	for v := from; v > to; v-- {
		m, ok := have[v]
		if !ok {
			return nil, fmt.Errorf("%d→%d 로 내릴 길이 없다 - 판 %d 은 증분이 아니다(기준선은 내리지 않는다)", from, to, v)
		}
		if _, ok := pg.IndexOnly(m.SQL); !ok {
			return nil, fmt.Errorf("%d→%d 로 내리지 않는다 - 증분 %d(%s)는 인덱스만 더한 것이 아니라 내리면 기록이나 모양을 잃는다. "+
				"그 판은 판올림 전 사본으로만 되돌린다: `fd migrate --rollback`(그 사본 뒤에 쓴 기록을 잃는다)", from, to, v, m.Name)
		}
		out = append(out, m)
	}
	return out, nil
}

// migrateDown 은 from 판의 원장을 run.target 판으로 내린다(DownSteps). 증분 하나가 트랜잭션 하나다 - 중간에 끊겨도 내린 단까지는
// 판 기록과 인덱스가 맞고, 다시 돌리면 남은 단부터 내린다.
//
// ★ 판올림 전 백업을 뜨지 않는다. 지우는 것이 인덱스뿐이라 잃을 것이 없고, 다시 올리면(판올림) 같은 인덱스를 다시 만든다.
func (s *Store) migrateDown(ctx context.Context, sess *migrateSession, from int, run migrateRun) error {
	steps, err := DownSteps(from, run.target, run.steps)
	if err != nil {
		s.log.Error("내릴 길이 없어 DB 를 내리지 않는다",
			"path", s.path, "db_version", from, "target", run.target, "error", err.Error())
		return fmt.Errorf("스키마 거절: %w", err)
	}
	s.log.Info("내림 시작(인덱스만 지운다)", "path", s.path, "from", from, "to", run.target)
	for _, m := range steps {
		if err := s.sessionTx(ctx, sess, fmt.Sprintf("증분 %d 내림(%s)", m.To, m.Name), func(ctx context.Context, tx *txConn) error {
			return downBody(ctx, tx, m)
		}); err != nil {
			return err
		}
		s.log.Info("증분 내림 완료", "path", s.path, "version", m.To-1, "reason", m.Name)
	}
	return nil
}

// downBody 는 내림 트랜잭션 안의 일이다: 판을 다시 재고 → (소유 역할로) 그 증분이 더한 인덱스를 거꾸로 지우고 → 판 행을 지운다.
func downBody(ctx context.Context, tx *txConn, m Migration) error {
	_, v, _, err := readMigrationState(ctx, tx)
	if err != nil {
		return err
	}
	if v != m.To {
		return fmt.Errorf("이 증분은 판 %d 에서 내리는데 DB 가 %d판이다 - 판을 잰 뒤에 다른 실행기가 바꿨다. 다시 돌려라", m.To, v)
	}
	if role := tx.dialect().stepRole(); role != "" {
		if _, err := tx.ExecContext(ctx, role); err != nil {
			return fmt.Errorf("소유 역할로 바꾸지 못했다: %w", err)
		}
	}
	names, _ := pg.IndexOnly(m.SQL)
	for i := len(names) - 1; i >= 0; i-- {
		if _, err := tx.ExecContext(ctx, "DROP INDEX "+names[i]); err != nil {
			return fmt.Errorf("인덱스 %s 를 지우지 못했다: %w", names[i], err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM schema_version WHERE version = ?`, m.To); err != nil {
		return fmt.Errorf("판 기록을 지우지 못했다: %w", err)
	}
	return nil
}

// applyBase 는 빈 DB 에 기준선을 깐다. 엔진마다 길이 다르다.
//
//   - SQLite: schema.sql 을 **트랜잭션 밖에서** 돌리고(그 안의 PRAGMA journal_mode 가 트랜잭션 안에서 못 돈다) 1판을 적는다.
//     중간에 끊기면 남는 것은 백업뿐이고, 그 상태는 PlanMigration 이 다음 실행에서 dbVersion==0 으로 잡아 다시 적용한다.
//   - Postgres: 준비 · 기준선 · 판 행이 한 트랜잭션(pgBaseline 단)이다. 끊기면 통째로 되감겨 빈 DB 로 남는다.
func (s *Store) applyBase(ctx context.Context, sess *migrateSession) error {
	if s.db.eng == enginePG {
		return s.applySteps(ctx, sess, []Migration{pgBaseline})
	}
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("스키마 적용 실패(path=%q): %w", s.path, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO schema_version(version, applied_at) VALUES (?, ?)`,
		BaseSchemaVersion, fmtTime(time.Now()),
	); err != nil {
		return fmt.Errorf("schema_version 기록 실패(스키마는 적용됨, path=%q): %w", s.path, err)
	}
	return nil
}

// applySteps 는 고른 증분을 차례로 얹는다. **한 단이 한 트랜잭션이다** — 중간에 끊겨도 얹힌 단까지는 버전 기록과
// 실제 스키마가 일치하고, 그러면 다음 실행이 남은 단부터 이어서 얹는다. 트랜잭션은 실행기 세션(sess) 위에서 연다.
func (s *Store) applySteps(ctx context.Context, sess *migrateSession, steps []Migration) error {
	for _, m := range steps {
		if err := s.applyStep(ctx, sess, m); err != nil {
			return err
		}
	}
	return nil
}

// applyStep 은 증분 하나를 한 트랜잭션으로 얹는다.
//
// ★ **증분 트랜잭션은 업무 쓰기와 같은 전역 잠금을 잡는다**(txWrite). 잠금이 다르면 DDL 도중에 업무 쓰기가
// 끼어 반쯤 바뀐 스키마 위에서 커밋할 수 있다. 같은 잠금이면 적용이 도는 동안 들어온 쓰기는 적용이 끝날
// 때까지 기다린다(pg_tx_test.go 의 DDL 중 쓰기 시험). SQLite 는 BEGIN IMMEDIATE 라 원래 그렇다. Postgres 는 실행기
// 세션이 그 잠금을 이미 쥐고 있고, 트랜잭션을 그 세션에서 열므로 곧장 받는다(migrateSession 의 ★).
func (s *Store) applyStep(ctx context.Context, sess *migrateSession, m Migration) error {
	return s.stepTx(ctx, sess, m, stepBody)
}

// stepTx 는 증분 트랜잭션 하나다: 실행기 세션 트랜잭션(sessionTx) 안에서 body 를 돌리고, 커밋하면 적용 로그를 남긴다.
// 안의 일(stepBody)과 가른 것은 수명 규칙(어느 길로 나가도 닫는다)을 따로 재려고다.
func (s *Store) stepTx(ctx context.Context, sess *migrateSession, m Migration, body func(context.Context, *txConn, Migration) error) error {
	start := time.Now()
	err := s.sessionTx(ctx, sess, fmt.Sprintf("증분 %d(%s)", m.To, m.Name), func(ctx context.Context, tx *txConn) error {
		return body(ctx, tx, m)
	})
	if err != nil {
		return err
	}
	s.log.Info("증분 적용 완료",
		"path", s.path, "version", m.To, "reason", m.Name,
		"duration", time.Since(start).Seconds())
	return nil
}

// sessionTx 는 실행기 세션(sess) 위 쓰기 트랜잭션 하나의 수명이다: 열고 → body → 커밋, 실패하면 되감는다. 증분 적용
// (stepTx)과 원장 옮기기(transferInto - 이관 · 역이관)가 쓴다 - 둘 다 실행기가 쥔 두 세션 잠금과 같은 연결에서 돌아야 한다(txSpec.on).
// what 은 오류 문구의 머리다. 끝에서 오류를 한 번 옮긴다(asFault) - 잠금 대기 초과(55P03)도 Tx 와 같은 *TxFault 로 나간다.
//
// ★ 열자마자 되감기를 미뤄 둔다(defer). body 가 패닉이면 명시 롤백을 건너뛰는데, 그러면 트랜잭션이 열린 채로 세션 해제
// (migrateSession.release)까지 올라가고 *sql.Conn 의 Close 가 열린 트랜잭션이 끝나기를 영영 기다린다 - 패닉이 밖으로
// 나가지도 못하고 실행기가 선다. defer 는 세션 해제보다 먼저 돈다(안쪽 함수의 defer 가 먼저다). 커밋한 뒤의 되감기는
// sql.ErrTxDone 으로 아무것도 안 한다.
// errRollbackFailed 는 sessionTx 의 되감기가 실패했다는 표지다. body 오류(예: 예행의 errDryRun)와 함께 싸여 나간다.
var errRollbackFailed = errors.New("롤백도 실패")

func (s *Store) sessionTx(ctx context.Context, sess *migrateSession, what string, body func(context.Context, *txConn) error) (err error) {
	defer func() { err = asFault(err) }()
	tx, err := s.begin(ctx, txSpec{kind: txWrite, on: sess})
	if err != nil {
		return fmt.Errorf("%s 트랜잭션 시작 실패(path=%q): %w", what, s.path, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := body(ctx, tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("%s 실패(path=%q): %w (%w: %v)", what, s.path, err, errRollbackFailed, rbErr)
		}
		return fmt.Errorf("%s 실패(path=%q): %w", what, s.path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s 커밋 실패(path=%q): %w", what, s.path, err)
	}
	return nil
}

// stepBody 는 증분 트랜잭션 안의 일이다: 판을 다시 재고 → (소유 역할로) SQL 을 돌리고 → 판을 적는다.
//
// ★ 기준선 단(m.base)은 역할을 SQL 이 스스로 바꿔 탄다 - 준비(pg/bootstrap.sql)는 역할을 만들고 스키마 권한을 주므로
// 접속한 초사용자로 돌아야 한다. 판 행은 두 갈래 모두 소유 역할로 적는다(앱 역할은 그 표를 읽기만 한다).
// 기준선 단은 판을 적기 전에 로케일을 잰다. 거절이면 준비 · 표가 함께 되감겨 빈 DB 로 남는다.
func stepBody(ctx context.Context, tx *txConn, m Migration) error {
	if err := stepReady(ctx, tx, m); err != nil {
		return err
	}
	role := tx.dialect().stepRole()
	if role != "" && !m.base {
		if _, err := tx.ExecContext(ctx, role); err != nil {
			return fmt.Errorf("소유 역할로 바꾸지 못했다: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("적용 실패: %w", err)
	}
	if m.base {
		if err := localeGate(ctx, tx); err != nil {
			return err
		}
		if role != "" {
			if _, err := tx.ExecContext(ctx, role); err != nil {
				return fmt.Errorf("소유 역할로 바꾸지 못했다: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_version(version, applied_at) VALUES (?, ?)`, m.To, fmtTime(time.Now())); err != nil {
		return fmt.Errorf("버전 기록 실패: %w", err)
	}
	return nil
}

// stepReady 는 증분을 얹기 직전, 잠금 안에서 판을 다시 잰다.
//
// ★ 판정은 트랜잭션 밖에서 읽은 판으로 했다. 그 사이에 다른 실행기가 같은 단을 얹었으면(자기 잠금이 없는 SQLite 판을 두
// 프로세스가 돌렸다) 여기서 멈춘다. Postgres 는 실행기 세션이 두 잠금을 쥔 채 판을 읽으므로 그런 일이 없지만, 같은 판정을
// 한 벌로 두려고 두 엔진이 함께 탄다. 이것이 없으면 같은 증분이 두 번 얹혀 DDL 이 「이미 있다」로 죽거나, 멱등인 증분이면
// 판 행이 두 번 적힌다.
func stepReady(ctx context.Context, tx *txConn, m Migration) error {
	hasTable, v, objects, err := readMigrationState(ctx, tx)
	if err != nil {
		return err
	}
	switch {
	case m.base && (hasTable || objects > 0):
		return fmt.Errorf("기준선은 빈 DB 에만 깐다 - 판 표 %t · 객체 %d개가 있다"+
			"(판을 잰 뒤에 다른 실행기가 깔았거나, 손으로 만든 것이 있다)", hasTable, objects)
	case !m.base && v != m.To-1:
		return fmt.Errorf("이 증분은 %d판 위에 얹는데 DB 가 %d판이다 - 판을 잰 뒤에 다른 실행기가 올렸다. 다시 돌려라",
			m.To-1, v)
	}
	return nil
}

// rollbackable 은 마이그레이션 실패에 **되돌리는 절차**를 붙인다.
//
// 두 경로(신규 적용·증분 업그레이드)가 같은 문구를 쓰게 하려고 헬퍼로 뺐다 —
// 두 벌로 두면 한쪽만 고쳐지고, 그 비대칭이 다시 "어디로 되돌리나"를 지운다.
func (s *Store) rollbackable(err error, backupPath string) error {
	hint := s.rollbackHint(backupPath)
	s.log.Error("마이그레이션 실패 — 되돌리는 절차를 함께 낸다",
		"path", s.path, "reason", hint, "error", err.Error())
	return fmt.Errorf("%w — %s", err, hint)
}

// rollbackHint 는 이 원장의 엔진에 맞는 되돌리기 절차다. Postgres 는 실패한 단이 트랜잭션째 되감긴다는 것부터 말한다 -
// 덤프 복원은 끝까지 커밋된 판올림을 통째로 물릴 때의 수단이다.
func (s *Store) rollbackHint(backupPath string) string {
	if s.db.eng == enginePG {
		return "실패한 단은 트랜잭션째 되감겼다(DB 는 마지막으로 커밋된 판에 있다). " + pgFailureHint(backupPath)
	}
	path := s.path
	if abs, err := filepath.Abs(path); err == nil {
		path = abs // 안내의 명령은 어느 디렉토리에서 돌려도 이 원장을 가리켜야 한다
	}
	return RollbackHint(path, backupPath)
}

// BackupSuffix 는 백업 파일 이름의 꼬리를 만든다. 사전순 = 시간순이다(같은 초에 겹치면 linkFresh 가 번호를 붙인다).
func BackupSuffix(at time.Time) string {
	return ".bak-" + fileStamp(at)
}

// backup 은 마이그레이션 직전 스냅숏을 뜨고 붙인 이름을 낸다. from 은 뜨는 원장의 판이다 - 사본이 그 판인지 잰다. at 은 이름의
// 시각이다(<db>.bak-<at>).
//
// 파일을 그냥 복사하지 않고 VACUUM INTO 를 쓴다 — WAL 모드에서는 .db 파일만 베끼면
// -wal 에 남은 커밋이 빠져 **조용히 낡은 사본**이 된다. VACUUM INTO 는 그 시점의
// 일관된 DB 하나를 만든다.
//
// ★ **최종 이름(<db>.bak-*)에 바로 쓰지 않는다.** 그 이름이 곧 `fd migrate --rollback` 의 후보라, 뜨는 도중 프로세스 · 전원이
// 끊기면 덜 쓴 사본(과 그 옆 -journal)이 가장 최근 백업으로 골라진다. 같은 디렉토리의 숨은 임시 이름(.<db>.bak-*.partial-*)에
// 뜨고, 다 뜬 원장인지(checkLedgerCopy - integrity_check · 판) 잰 뒤 굳히고(fsync) 이름을 붙이고 디렉토리를 굳힌다. 끊기면 숨은
// 이름만 남는다 - 후보가 아니다(rollbackCandidate). 이 실행이 실패로 끝나면 그 자리에서 지운다.
// ★ 이름은 **있는 파일을 갈지 않는 길**로 붙인다(linkFresh). 같은 초에 두 실행이 같은 이름을 고르면 앞선 판은 rename 으로 먼저
// 붙은 사본을 덮었다 - 이제 뒤 실행이 번호 붙은 이름(<db>.bak-<시각>-001)을 받는다.
func (s *Store) backup(ctx context.Context, from int, at time.Time) (string, error) {
	if s.path == "" || strings.Contains(s.path, ":memory:") {
		return "", nil // 메모리 DB 는 뜰 것이 없다. 무해한 무시라 여기 적는다
	}
	name := s.path + BackupSuffix(at)
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("백업 디렉토리 생성 실패(dir=%q): %w", dir, err)
	}
	// VACUUM INTO 는 대상이 없거나 빈 파일이어야 한다. 겹치지 않는 빈 파일을 먼저 잡는다(모드는 ledgerFileMode - 주인만 읽는다).
	f, err := createExclusive(dir, "."+filepath.Base(name)+".partial-", ledgerFileMode)
	if err != nil {
		return "", fmt.Errorf("마이그레이션 전 백업의 임시 파일을 만들지 못했다(dir=%q): %w", dir, err)
	}
	tmp := f.Name()
	if err := f.Close(); err != nil {
		removeLedgerFiles(tmp, s.log)
		return "", fmt.Errorf("마이그레이션 전 백업의 임시 파일을 닫지 못했다(%q): %w", tmp, err)
	}
	dst, err := s.sealBackup(ctx, tmp, name, from)
	if err != nil {
		removeLedgerFiles(tmp, s.log)
		return "", err
	}
	return dst, nil
}

// sealBackup 은 임시 이름 tmp 에 원장을 뜨고, 다 뜬 from 판 원장인지 잰 뒤 굳혀 name 에(차 있으면 번호를 붙여 - linkFresh) 이름을
// 붙이고 그 이름을 낸다(backup 의 ★).
func (s *Store) sealBackup(ctx context.Context, tmp, name string, from int) (string, error) {
	// VACUUM INTO 는 파라미터 바인딩을 안 받는다. 경로는 사용자 입력이 아니라
	// 서버 설정에서 온 값이고, 작은따옴표만 이스케이프하면 리터럴로 안전하다.
	q := "VACUUM INTO '" + strings.ReplaceAll(tmp, "'", "''") + "'"
	if _, err := s.db.ExecContext(ctx, q); err != nil {
		return "", fmt.Errorf("마이그레이션 전 백업 실패(dst=%q): %w", name, err)
	}
	rows, err := checkLedgerCopy(ctx, tmp)
	if err != nil {
		return "", fmt.Errorf("마이그레이션 전 백업을 뜬 사본이 온전하지 않다(dst=%q) - 판을 올리지 않는다: %w", name, err)
	}
	if v := topVersion(rows); v != from {
		return "", fmt.Errorf("마이그레이션 전 백업을 뜬 사본의 판이 %d 인데 원장은 %d판이다(dst=%q) - 판을 올리지 않는다", v, from, name)
	}
	if err := syncFile(tmp); err != nil {
		return "", fmt.Errorf("마이그레이션 전 백업을 굳히지 못했다(dst=%q): %w", name, err)
	}
	dst, err := linkFresh(tmp, name)
	if err != nil {
		return "", fmt.Errorf("마이그레이션 전 백업에 이름을 붙이지 못했다 - 판을 올리지 않는다: %w", err)
	}
	return dst, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 잡동사니
// ─────────────────────────────────────────────────────────────────────────────

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// nowStamp 는 **저장 표기와 정확히 같은 해상도**의 지금 시각이다.
//
// 함수가 시각을 스스로 만들고 그것을 구조체에 담아 돌려줄 때 반드시 이걸 쓴다.
// time.Now() 를 그대로 담으면 반환값(로컬 시간대·나노초)과 다시 읽은 값
// (UTC·마이크로초)이 달라서, "방금 만든 것"과 "다시 읽은 것"의 비교가 조용히 틀어진다.
// 재개 판정이 정확히 그 비교다.
func nowStamp() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// atStamp 는 **받은** 시각을 같은 해상도로 맞춘다. 영값이면 지금이다.
//
// nowStamp 의 짝이고, 이유도 같다. 시각을 주입받는 쓰기가 그 값을 구조체에 담아
// 돌려줄 때 반드시 이걸 거친다 — 서비스의 시계는 `time.Now().UTC()` 라 나노초를 담고
// 있는데 행은 마이크로초로 저장되므로(timeLayout), 받은 값을 그대로 돌려주면
// "방금 만든 것"과 "다시 읽은 것"이 미세하게 갈린다. 그 어긋남은 두 값을 비교하는
// 자리에서만 드러나고 그때는 이미 원인에서 멀다.
//
// ★ Beat·Touch 는 아직 안 거친다. 그 둘은 받은 시각을 구조체로 돌려주지 않아 같은
// 비교가 생기지 않는다 — 돌려주게 되는 날 이 함수를 거쳐야 한다.
func atStamp(at time.Time) time.Time {
	if at.IsZero() {
		return nowStamp()
	}
	return at.UTC().Truncate(time.Microsecond)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("시각 해석 실패(%q): %w", clip(s, 64), err)
	}
	return t.UTC(), nil
}

func parseNullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// withLimit 은 목록 질의 끝에 LIMIT 을 붙인 질의와 인자를 낸다. limit 이 음수면 안 붙인다 - 「전부」다.
//
// ★ 음수 LIMIT 의 뜻이 엔진마다 다르다. SQLite 는 「상한 없음」이고 Postgres 는 오류로 거절한다. 절을 빼서 두 엔진에
// 같은 뜻을 세운다. 0 은 두 엔진 모두 빈 목록이다. args 는 고치지 않는다(사본에 덧붙인다).
func withLimit(q string, args []any, limit int) (string, []any) {
	if limit < 0 {
		return q, args
	}
	return q + ` LIMIT ?`, append(slices.Clip(args), limit)
}

// nullStr 은 빈 문자열을 NULL 로 바꾼다.
// 빈 문자열을 그대로 넣으면 FK 가 "" 라는 없는 키를 찾다 실패하고,
// 그 실패는 "값을 안 준 것"과 구분되지 않는다.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func str(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}

// clip 은 오류·로그에 실을 외부 문자열을 자르고 제어문자를 걷어낸다.
// 로그 주입과 무한장 오류 메시지를 막는다.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
