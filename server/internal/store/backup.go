package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// 판단 원장 내보내기 — `fd export --judgments` 의 저장 계층.
//
// ★ 이름을 "백업"이라고 하지 않는다. 이 패키지에서 backup·BackupSuffix·<db>.bak-* 는
// 이미 마이그레이션 직전 VACUUM INTO 로 뜨는 **DB 파일 사본**을 뜻한다. 두 개념이
// 같은 낱말을 쓰면 오류 문구와 로그에서 섞인다.
//
// ★ model 을 거치지 않는다. nullStr/str 이 NULL 과 빈 문자열을 접기 때문이다.
// 그 접힘이 왕복으로 닫히는지는 "빈 문자열이 저장된 행이 원리적으로 없다"는 별도 논증에
// 의존하는데, 원문 DTO 로 가면 그 논증 자체가 필요 없다. 포인터가 nil 이면 NULL 이다.

// LedgerJudgment 는 judgment 표 한 행의 원문이다.
type LedgerJudgment struct {
	ID         string  `json:"id"`
	Project    *string `json:"project"`
	SessionID  *string `json:"session_id"`
	At         string  `json:"at"` // DB 원문 문자열(폭 고정 마이크로초). time.Time 으로 접지 않는다
	Kind       string  `json:"kind"`
	Title      *string `json:"title"`
	Body       string  `json:"body"`
	Supersedes *string `json:"supersedes"`
	// Author · Via 는 증분 020 의 행위자 칸이다. 그 증분 이전 행은 소유자 계정으로 귀속됐고, 지금 판의 쓰기는 아직 비워 둔다.
	Author *string `json:"author"`
	Via    *string `json:"via"`
}

// LedgerLink 는 judgment_link 표 한 행이다.
type LedgerLink struct {
	JudgmentID string `json:"judgment_id"`
	TargetKind string `json:"target_kind"`
	TargetID   string `json:"target_id"`
	// ★ NULL 은 "판단 자신의 프로젝트"다 — 증분 009 이전의 링크가 전부 그 모양이라
	//   *string 이어야 한다. string 으로 받으면 NULL 과 명시된 빈 값이 같아지고,
	//   왕복 복원이 옛 행에 빈 문자열을 써 넣어 **어느 프로젝트와도 안 맞는 링크**로
	//   바꿔 놓는다(교차 링크가 복구 경로에서 다시 죽는 자리).
	TargetProject *string `json:"target_project"`
}

// LedgerSnapshot 은 snapshot 표 한 행의 원문이다.
type LedgerSnapshot struct {
	Project     string  `json:"project"`
	Key         string  `json:"key"`
	Value       string  `json:"value"`
	Method      string  `json:"method"`
	Evidence    *string `json:"evidence"`
	InputDigest *string `json:"input_digest"`
	ComputedAt  string  `json:"computed_at"`
}

// LedgerMachine 은 machine 표 한 행의 원문이다. 기기 토큰의 지문(token_hash)은 싣지 않는다(ledgerSecretCols).
type LedgerMachine struct {
	ID          string  `json:"id"`
	Hostname    string  `json:"hostname"`
	FirstSeen   string  `json:"first_seen"`
	LastSeen    string  `json:"last_seen"`
	AccountID   *string `json:"account_id"`
	Name        *string `json:"name"`
	TokenPrefix *string `json:"token_prefix"`
	TokenOrigin *string `json:"token_origin"`
	IssuedAt    *string `json:"issued_at"`
	RevokedAt   *string `json:"revoked_at"`
	RevokedBy   *string `json:"revoked_by"`
}

// LedgerAccount 는 account 표 한 행의 원문이다. 증분 020 부터 폐포에 든다 - judgment.author · session.account_id ·
// machine.account_id 가 이것을 가리킨다. 암호 지문(password_hash)은 싣지 않는다(ledgerSecretCols).
type LedgerAccount struct {
	ID          string  `json:"id"`
	Login       string  `json:"login"`
	DisplayName string  `json:"display_name"`
	Email       *string `json:"email"`
	Role        string  `json:"role"`
	CreatedAt   string  `json:"created_at"`
	DisabledAt  *string `json:"disabled_at"`
}

// LedgerProject 는 project 표 한 행의 원문이다.
//
// ★ PinnedAt·ArchivedAt 도 *string 이다(원장은 값 원문을 그대로 나르지, 파싱해서 time.Time
// 으로 되살리지 않는다 — RemoteURL·Config·ConfigFromSHA 와 같은 이유). 증분 007 이 더한
// 두 컬럼이 nullable TEXT 라 이 표의 NULL 가능 필드가 셋에서 다섯으로 늘었다.
type LedgerProject struct {
	ID            string  `json:"id"`
	Path          string  `json:"path"`
	RemoteURL     *string `json:"remote_url"`
	DefaultBranch string  `json:"default_branch"`
	Config        *string `json:"config"`
	ConfigFromSHA *string `json:"config_from_sha"`
	CreatedAt     string  `json:"created_at"`
	PinnedAt      *string `json:"pinned_at"`
	ArchivedAt    *string `json:"archived_at"`
}

// LedgerSession 은 session 표 한 행의 원문이다.
//
// ★ 이 표가 원장에 있는 이유. session.id 는 서버 발급 ULID 라 같은 3중키로 다시 열어도
// 새 값이 나온다 — project·machine 처럼 "이름을 다시 부르면 같은 것"이 아니다.
// 판단의 85%(실측 973/1141)가 session_id 를 갖고, 복원이 한 트랜잭션이라 그 FK 가 하나만
// 깨져도 판단 전체가 롤백된다.
type LedgerSession struct {
	ID          string  `json:"id"`
	Project     string  `json:"project"`
	MachineID   string  `json:"machine_id"`
	Worktree    string  `json:"worktree"`
	CCSessionID string  `json:"cc_session_id"`
	Label       *string `json:"label"`
	State       string  `json:"state"`
	BlockedWhy  *string `json:"blocked_why"`
	OpenedAt    string  `json:"opened_at"`
	// Harness 는 013 이 더한 축이다. 포인터인 이유는 Label·BlockedWhy 와 같다 —
	// NULL(「미상」)과 빈 문자열을 원장이 구별해야 한다.
	Harness *string `json:"harness"`
	// AccountID 는 020 이 더한 축(세션을 연 계정)이다.
	AccountID *string `json:"account_id"`
}

// LedgerDump 는 한 순간의 FK 폐포 전량이다.
//
// 일곱 표다. 앞 넷(account·machine·project·session)이 뒤 셋의 FK 대상이고, account·project 는
// 아무것도 참조하지 않는 leaf 라 폐포가 여기서 닫힌다(machine 은 account 만 가리킨다).
type LedgerDump struct {
	Accounts  []LedgerAccount
	Machines  []LedgerMachine
	Projects  []LedgerProject
	Sessions  []LedgerSession
	Judgments []LedgerJudgment
	Links     []LedgerLink
	Snapshots []LedgerSnapshot
}

// linkCols 는 judgment_link 표의 컬럼이다. 다른 다섯과 달리 이 표에는 조회 접근자가 없어
// 상수가 없었고, 읽기와 쓰기가 각자 리터럴을 들고 있었다.
const linkCols = `judgment_id, target_kind, target_id, target_project`

// 원장이 담는 칸 목록 가운데 업무 조회 상수와 다른 넷이다. 증분 020 의 칸(계정 · 토큰 · 행위자)을 업무 조회(model)는 아직 안
// 읽으므로 그 상수에 붙이지 않고, 원장만 그 뒤에 잇는다 - 표마다 목록은 여전히 하나다(ledgerTables).
const (
	accountLedgerCols  = `id, login, display_name, email, role, created_at, disabled_at`
	machineLedgerCols  = machineCols + `, account_id, name, token_prefix, token_origin, issued_at, revoked_at, revoked_by`
	sessionLedgerCols  = sessionCols + `, account_id`
	judgmentLedgerCols = judgmentCols + `, author, via`
)

// ledgerSecretCols 는 원장이 일부러 안 담는 비밀 칸이다(표 → 칸). 원장은 매시간 기록 저장소(journal.git)에 쌓이고 그 이력은
// 지우지 않으므로, 여기 실린 비밀은 바꾼 뒤에도 영구히 남는다 - 암호 지문(argon2id)은 약한 암호면 오프라인으로 풀린다. 되쓴
// 계정은 암호 없이(옛 원장의 소유자 계정과 같은 상태), 기기는 토큰 없이 돌아오고 다시 로그인한다. 빠뜨린 칸이 이것뿐인지는
// TestLedgerColumnListsMatchTheSchema 가 잰다.
var ledgerSecretCols = map[string][]string{
	"account": {"password_hash"},
	"machine": {"token_hash"},
}

// LedgerSecretColumns 는 원장이 안 담는 비밀 칸(「표.칸」, 정렬)이다. 손실 목록(ledger.Losses)이 이것으로 적는다 - 손으로 한 벌 더
// 적으면 칸이 늘 때 그 설명이 조용히 거짓이 된다(LedgerTableNames 와 같은 까닭).
func LedgerSecretColumns() []string {
	var out []string
	for table, cols := range ledgerSecretCols {
		for _, c := range cols {
			out = append(out, table+"."+c)
		}
	}
	sort.Strings(out)
	return out
}

// ledgerTables 는 판단 원장이 담는 FK 폐포 일곱 표다 — **표마다 컬럼 목록이 하나다.**
//
// 순서는 되쓰기 순서다: account 가 machine·session 보다, machine·project·session 이 판단보다
// 먼저 들어가야 FK 가 닫힌다(WriteLedger 의 그 주석 참고).
//
// ★ 왜 한 자리에 모으나. 예전에는 읽기가 상수를, 쓰기가 인라인 리터럴을 써서 같은 표에
// 목록이 **둘**이었다. 스키마에 컬럼이 하나 들어오면 백업이 그것을 버리고(읽기 목록에
// 없다) 복원도 버리는데(쓰기 목록에 없다), 왕복 시험은 want·final 이 둘 다 ReadLedger
// 산출물이라 원리적으로 못 본다. 목록이 둘이면 관문도 한쪽만 재게 되고, **재지 않은
// 쪽이 정확히 그 조용한 손실 자리**가 된다.
var ledgerTables = []struct {
	name string
	cols string
}{
	{"account", accountLedgerCols},
	{"machine", machineLedgerCols},
	{"project", projectCols},
	{"session", sessionLedgerCols},
	{"judgment", judgmentLedgerCols},
	{"judgment_link", linkCols},
	{"snapshot", snapshotCols},
}

// LedgerTableNames 는 판단 원장이 담는 폐포 표 이름들이다(선언 순서 = 되쓰기 순서).
//
// ★ 왜 내보내나. **복원 결과를 설명하는 쪽**(ledger.Losses)이 "무엇이 복원되는가"를 손으로
// 적으면 폐포가 바뀔 때 그 설명이 조용히 거짓이 된다. 실제로 그랬다 — session 이 폐포에
// 들어온 뒤에도 손실 목록은 세션을 가리키는 링크를 손실로 부르고 있었고, 그 링크가
// 실측 34%였다.
func LedgerTableNames() []string {
	out := make([]string, 0, len(ledgerTables))
	for _, e := range ledgerTables {
		out = append(out, e.name)
	}
	return out
}

// ledgerColNames 는 컬럼 목록 문자열을 이름 슬라이스로 가른다. 순수 함수다.
func ledgerColNames(cols string) []string {
	var out []string
	for _, c := range strings.Split(cols, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// ledgerInsert 는 표 하나의 되쓰기 문장이다. 순수 함수다.
//
// ★ `?` 를 손으로 안 적는다. 컬럼을 더한 사람이 목록·자리표·인자 셋을 다 고쳐야 하는데
// 자리표를 빠뜨리면 런타임에서야 죽는다 — 상수에서 세면 그 자리가 원리적으로 사라진다.
func ledgerInsert(name, cols string) string {
	n := len(ledgerColNames(cols))
	return `INSERT INTO ` + name + `(` + cols + `) VALUES (` +
		strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + `)`
}

// ReadLedger 는 일곱 표(FK 폐포 전량)를 **한 트랜잭션 안에서** 읽는다.
//
// ★ 왜 트랜잭션인가. 표를 따로 읽으면 그 사이 서버가 커밋한 판단의 **링크만** 산출물에
// 들어간다(judgment 를 읽은 뒤 link 를 읽으므로). 그 링크가 가리키는 판단이 없으니
// 되읽기가 FK 로 죽는다. 트랜잭션 원자성은 DB 상태를 보장하지 서로 다른 시점의 두 읽기를
// 보장하지 않는다 — 동시 세션이 스물이 넘는 이 저장소에서 실제로 열리는 창이다.
//
// ★ 왜 Store.Tx 를 안 쓰는가. 그 함수는 쓰기 트랜잭션이라 전역 쓰기 잠금을 잡는다(Tx 주석이
// 그 대가를 적어 뒀다). 전량 읽기가 도는 동안 서버의 모든 쓰기가 잠금 대기 상한 안에서 줄을 선다.
// 여기서는 읽기 트랜잭션(ReadTx)을 연다 - 잠그지 않고 한 스냅숏을 본다. 엔진 차이는 ReadTx 안에만 있다: SQLite 는
// WAL 의 첫 읽기 스냅숏이고, Postgres 는 REPEATABLE READ READ ONLY 라 일곱 질의가 첫 질의의 스냅숏을 함께 본다(READ
// COMMITTED 면 질의마다 새 스냅숏이라 위의 창이 그대로 열린다). Postgres 에서도 쓰기를 세우지 않는다 - 전역 잠금은
// 쓰기 트랜잭션만 잡는다. 그 창이 닫힌 것은 backup_test.go 의 TestPGReadLedgerReadsOneSnapshot 이 잰다.
//
// ★ 상한이 없다. ListJudgmentsByKind 는 limit<=0 을 50 으로 바꾸고, legacy 되쓰기는
// 100000 이라는 수를 손으로 넣는다 — 상한에 걸려 조용히 잘리면 산출물이 원본보다
// 적어지고 그 차이는 세어 보기 전에는 안 보인다. 원장은 전량이 목적이라 WHERE 절도 없다.
func (s *Store) ReadLedger(ctx context.Context) (LedgerDump, error) {
	var d LedgerDump
	err := s.ReadTx(ctx, func(tx Reader) error {
		var err error
		d, err = readLedger(ctx, tx)
		return err
	})
	return d, err
}

// readLedger 는 ReadLedger 의 본체다. 읽기 트랜잭션 하나(ReadTx) 안에서 돈다.
func readLedger(ctx context.Context, tx Reader) (LedgerDump, error) {
	var d LedgerDump
	var err error
	if d.Accounts, err = readLedgerAccounts(ctx, tx); err != nil {
		return d, err
	}
	if d.Machines, err = readLedgerMachines(ctx, tx); err != nil {
		return d, err
	}
	if d.Projects, err = readLedgerProjects(ctx, tx); err != nil {
		return d, err
	}
	if d.Sessions, err = readLedgerSessions(ctx, tx); err != nil {
		return d, err
	}
	if d.Judgments, err = readLedgerJudgments(ctx, tx); err != nil {
		return d, err
	}
	if d.Links, err = readLedgerLinks(ctx, tx); err != nil {
		return d, err
	}
	if d.Snapshots, err = readLedgerSnapshots(ctx, tx); err != nil {
		return d, err
	}
	return d, nil
}

// readLedgerJudgments 는 판단 전량을 id 순으로 읽는다.
// id 는 ULID 라 정렬이 곧 생성순이고, 같은 DB 면 같은 바이트가 나오는 근거가 이것이다.
func readLedgerJudgments(ctx context.Context, q Reader) ([]LedgerJudgment, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+judgmentLedgerCols+` FROM judgment ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("원장 판단 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []LedgerJudgment
	for rows.Next() {
		var j LedgerJudgment
		var project, session, title, supersedes, author, via sql.NullString
		if err := rows.Scan(&j.ID, &project, &session, &j.At, &j.Kind, &title, &j.Body, &supersedes,
			&author, &via); err != nil {
			return nil, fmt.Errorf("원장 판단 행 해석 실패: %w", err)
		}
		j.Project, j.SessionID = ptrOf(project), ptrOf(session)
		j.Title, j.Supersedes = ptrOf(title), ptrOf(supersedes)
		j.Author, j.Via = ptrOf(author), ptrOf(via)
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("원장 판단 순회 실패: %w", err)
	}
	return out, nil
}

// readLedgerLinks 는 링크 전량을 읽는다.
// 정렬 셋은 PK 와 같은 순서다 — 재현성을 위해 완전 순서여야 한다.
func readLedgerLinks(ctx context.Context, q Reader) ([]LedgerLink, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+linkCols+` FROM judgment_link
		ORDER BY judgment_id, target_kind, target_id`)
	if err != nil {
		return nil, fmt.Errorf("원장 링크 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []LedgerLink
	for rows.Next() {
		var l LedgerLink
		if err := rows.Scan(&l.JudgmentID, &l.TargetKind, &l.TargetID, &l.TargetProject); err != nil {
			return nil, fmt.Errorf("원장 링크 행 해석 실패: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("원장 링크 순회 실패: %w", err)
	}
	return out, nil
}

// readLedgerSnapshots 는 스냅숏 전량을 읽는다. PK 와 같은 (project, key) 순이다.
func readLedgerSnapshots(ctx context.Context, q Reader) ([]LedgerSnapshot, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+snapshotCols+` FROM snapshot ORDER BY project, key`)
	if err != nil {
		return nil, fmt.Errorf("원장 스냅숏 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []LedgerSnapshot
	for rows.Next() {
		var sn LedgerSnapshot
		var evidence, digest sql.NullString
		if err := rows.Scan(&sn.Project, &sn.Key, &sn.Value, &sn.Method,
			&evidence, &digest, &sn.ComputedAt); err != nil {
			return nil, fmt.Errorf("원장 스냅숏 행 해석 실패: %w", err)
		}
		sn.Evidence, sn.InputDigest = ptrOf(evidence), ptrOf(digest)
		out = append(out, sn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("원장 스냅숏 순회 실패: %w", err)
	}
	return out, nil
}

// readLedgerAccounts 는 계정 전량을 id 순으로 읽는다.
func readLedgerAccounts(ctx context.Context, q Reader) ([]LedgerAccount, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+accountLedgerCols+` FROM account ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("원장 계정 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []LedgerAccount
	for rows.Next() {
		var a LedgerAccount
		var email, disabled sql.NullString
		if err := rows.Scan(&a.ID, &a.Login, &a.DisplayName, &email, &a.Role, &a.CreatedAt, &disabled); err != nil {
			return nil, fmt.Errorf("원장 계정 행 해석 실패: %w", err)
		}
		a.Email, a.DisabledAt = ptrOf(email), ptrOf(disabled)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("원장 계정 순회 실패: %w", err)
	}
	return out, nil
}

// readLedgerMachines 는 머신 전량을 id 순으로 읽는다.
func readLedgerMachines(ctx context.Context, q Reader) ([]LedgerMachine, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+machineLedgerCols+` FROM machine ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("원장 머신 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []LedgerMachine
	for rows.Next() {
		var m LedgerMachine
		var account, name, prefix, origin, issued, revoked, revokedBy sql.NullString
		if err := rows.Scan(&m.ID, &m.Hostname, &m.FirstSeen, &m.LastSeen,
			&account, &name, &prefix, &origin, &issued, &revoked, &revokedBy); err != nil {
			return nil, fmt.Errorf("원장 머신 행 해석 실패: %w", err)
		}
		m.AccountID, m.Name, m.TokenPrefix, m.TokenOrigin = ptrOf(account), ptrOf(name), ptrOf(prefix), ptrOf(origin)
		m.IssuedAt, m.RevokedAt, m.RevokedBy = ptrOf(issued), ptrOf(revoked), ptrOf(revokedBy)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("원장 머신 순회 실패: %w", err)
	}
	return out, nil
}

// readLedgerProjects 는 프로젝트 전량을 id 순으로 읽는다.
func readLedgerProjects(ctx context.Context, q Reader) ([]LedgerProject, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+projectCols+` FROM project ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("원장 프로젝트 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []LedgerProject
	for rows.Next() {
		var p LedgerProject
		var remote, config, fromSHA, pinned, archived sql.NullString
		if err := rows.Scan(&p.ID, &p.Path, &remote, &p.DefaultBranch,
			&config, &fromSHA, &p.CreatedAt, &pinned, &archived); err != nil {
			return nil, fmt.Errorf("원장 프로젝트 행 해석 실패: %w", err)
		}
		p.RemoteURL, p.Config, p.ConfigFromSHA = ptrOf(remote), ptrOf(config), ptrOf(fromSHA)
		p.PinnedAt, p.ArchivedAt = ptrOf(pinned), ptrOf(archived)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("원장 프로젝트 순회 실패: %w", err)
	}
	return out, nil
}

// readLedgerSessions 는 세션 전량을 id 순으로 읽는다.
func readLedgerSessions(ctx context.Context, q Reader) ([]LedgerSession, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+sessionLedgerCols+` FROM session ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("원장 세션 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []LedgerSession
	for rows.Next() {
		var x LedgerSession
		var label, blockedWhy, harness, account sql.NullString
		if err := rows.Scan(&x.ID, &x.Project, &x.MachineID, &x.Worktree, &x.CCSessionID,
			&label, &x.State, &blockedWhy, &x.OpenedAt, &harness, &account); err != nil {
			return nil, fmt.Errorf("원장 세션 행 해석 실패: %w", err)
		}
		x.Label, x.BlockedWhy, x.Harness = ptrOf(label), ptrOf(blockedWhy), ptrOf(harness)
		x.AccountID = ptrOf(account)
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("원장 세션 순회 실패: %w", err)
	}
	return out, nil
}

// ptrOf 는 NULL 을 nil 로, 값을 포인터로 낸다.
// str() 과 다르다 — str 은 NULL 을 "" 로 접어 둘을 구분 불가능하게 만든다.
func ptrOf(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

// ledgerDSN 은 원장 읽기 전용 접속 문자열이다. 기본 dsn() 과 두 곳이 다르다.
//
//	_txlock=deferred      기본은 immediate 다. 읽기 스냅숏만 잡고 서버 쓰기를 안 막는다.
//	                      deferred 의 알려진 위험(읽기 뒤 쓰기 승격이 SQLITE_BUSY 로 즉시
//	                      실패하고 busy_timeout 이 안 듣는다)은 원장이 쓰기를 하지 않으므로
//	                      발생할 자리가 없다.
//	journal_mode 를 안 건다  그것이 이 DSN 에서 파일을 바꿀 수 있는 유일한 pragma 다.
//	                      대상은 롤백저널일 수 있다 — VACUUM INTO 로 뜬 <db>.bak-* 이 그 모드이고,
//	                      그것이 바로 이 명령이 건져야 할 물건이다. 걸면 그 헤더를 영구히 고친다.
//	                      되읽기 확인은 ledgerWantPragmas 로 한다(journal_mode 가 빠진 판) —
//	                      요청하지도 않은 pragma 를 요구하면 롤백저널 DB 마다 거짓 진단이 난다.
func ledgerDSN(path string) string { return sqliteURI(path, ledgerQuery()) }

func ledgerQuery() url.Values {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+busyTimeoutMS+")")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "deferred")
	return q
}

// importSourceDSN 은 SQLite 이관 · 대조의 원본(openImportSource)을 여는 접속 문자열이다. ledgerDSN 에 mode=ro 를 더한다 -
// 원본은 옮길 것일 뿐이라 이관이 그 파일에 쓸 자리가 없다. 읽기 전용 연결은 없는 파일을 만들지 않고, 끝낼 때 -wal 을
// 본 파일로 옮기지(체크포인트) 않는다.
//
// ★ OpenLedger 가 mode=ro 를 안 쓰는 까닭(서버가 죽은 채 남은 -wal)은 이관에 없다. 이관은 비지 않은 -wal 이 옆에 있는
// 원본을 열기 전에 거절한다(sourceWALRefusal).
func importSourceDSN(path string) string {
	q := ledgerQuery()
	q.Set("mode", "ro")
	return sqliteURI(path, q)
}

// WriteLedger 는 원장을 DB 에 되쓴다(`fd import --judgments --apply`). 업무 쓰기 트랜잭션(Tx)이라 원장 칸 관문을 지난다 -
// 자유 글의 NUL 은 `\x00` 표기로 담기고 이름 칸의 NUL 은 전체를 거절한다(예행은 ScreenLedger 가 같은 판정을 DB 없이 낸다).
//
// ★ AddJudgment 를 쓰지 않는다. 그것은 빈 ID 에 새 ULID 를, 빈 At 에 지금 시각을 채운다 —
// 복원은 원문을 그대로 되살려야 하므로 raw INSERT 로 간다. 트리거와 CHECK 는 그대로 걸린다
// (그것이 안전핀이다).
//
// ★ 정책은 "빈 표 전제"다. judgment 는 judgment_no_update·judgment_no_delete 트리거로
// UPDATE·DELETE 가 물리적으로 금지돼 있어, 잘못 넣은 행을 고치거나 지울 수 없다.
// 그래서 중복 id 를 건너뛰지 않고 거절한다 — 조용히 넘어가면 무엇이 반쯤 들어갔는지 모른다.
//
// ★ 폐포를 통째로 되쓴다. account·machine·project·session 이 판단보다 먼저 들어가고, 그 넷은
// 아무것도 참조하지 않거나 서로만 참조하므로 여기서 닫힌다. 빈 DB 에 되쓰면 미리 심어 둘
// 것이 하나도 없다 — 그것이 이 함수가 증명하는 "무손실"의 실제 의미다.
func (s *Store) WriteLedger(ctx context.Context, d LedgerDump) error {
	return s.Tx(ctx, func(t *Tx) error {
		// ★ supersedes 는 judgment 를 자기참조하고, 원장의 id 순서가 그 참조 순서와 같다는
		//   보장이 없다. FK 검사를 커밋 시점으로 미뤄 순서 제약을 없앤다(move.go 와 같은 수단).
		//   미루기는 **이 트랜잭션에만** 걸리고 커밋 때 전부 검사된다(deferForeignKeys).
		if _, err := t.tx.ExecContext(t.ctx, t.tx.dialect().deferForeignKeys()); err != nil {
			return fmt.Errorf("FK 검사 미루기 실패: %w", err)
		}
		for _, r := range ledgerRows(d) {
			if _, err := t.tx.ExecContext(t.ctx, r.q, r.args...); err != nil {
				return fmt.Errorf("%s: %w", r.what, err)
			}
		}
		return nil
	})
}

// ScreenLedger 는 WriteLedger 가 원장 칸 관문(cellgate.go)에서 겪을 것을 DB 없이 잰다 - `fd import --judgments` 의 예행이
// 적용과 같은 판정을 내도록. 관문이 거절할 글이 있으면 그 첫 거절(적용이면 트랜잭션 전체가 되돌아간다)이고, 없으면 `\x00`
// 표기로 바꿔 넣을 칸 수다. 순수 함수다.
func ScreenLedger(d LedgerDump) (rewrites int, err error) {
	for _, r := range ledgerRows(d) {
		_, pend, err := gateArgs(r.q, r.args)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", r.what, err)
		}
		rewrites += len(pend) // 되쓰기는 행마다 한 행을 넣거나 트랜잭션 전체가 되돌아간다 - 문장마다 원장에 닿는다
	}
	return rewrites, nil
}

// ledgerRow 는 되쓰기 문장 하나와 그 인자, 실패 문구의 머리다.
type ledgerRow struct {
	q    string
	args []any
	what string
}

// ledgerRows 는 원장 d 를 되쓰는 문장들이다(폐포 순서: 계정 · 머신 · 프로젝트 · 세션 · 판단 · 링크 · 스냅숏). 순수 함수다 -
// WriteLedger 가 실행하고 ScreenLedger 가 DB 없이 잰다(한 벌이라 예행과 적용이 갈릴 자리가 없다).
func ledgerRows(d LedgerDump) []ledgerRow {
	out := make([]ledgerRow, 0, len(d.Accounts)+len(d.Machines)+len(d.Projects)+len(d.Sessions)+len(d.Judgments)+
		len(d.Links)+len(d.Snapshots))
	accountStmt := ledgerInsert("account", accountLedgerCols)
	for _, a := range d.Accounts {
		out = append(out, ledgerRow{accountStmt, []any{a.ID, a.Login, a.DisplayName, a.Email, a.Role, a.CreatedAt, a.DisabledAt},
			fmt.Sprintf("원장 계정 되쓰기 실패(id=%q)", clip(a.ID, 64))})
	}
	machineStmt := ledgerInsert("machine", machineLedgerCols)
	for _, m := range d.Machines {
		out = append(out, ledgerRow{machineStmt, []any{m.ID, m.Hostname, m.FirstSeen, m.LastSeen,
			m.AccountID, m.Name, m.TokenPrefix, m.TokenOrigin, m.IssuedAt, m.RevokedAt, m.RevokedBy},
			fmt.Sprintf("원장 머신 되쓰기 실패(id=%q)", clip(m.ID, 64))})
	}
	projectStmt := ledgerInsert("project", projectCols)
	for _, p := range d.Projects {
		out = append(out, ledgerRow{projectStmt, []any{p.ID, p.Path, p.RemoteURL, p.DefaultBranch,
			p.Config, p.ConfigFromSHA, p.CreatedAt, p.PinnedAt, p.ArchivedAt},
			fmt.Sprintf("원장 프로젝트 되쓰기 실패(id=%q)", clip(p.ID, 64))})
	}
	sessionStmt := ledgerInsert("session", sessionLedgerCols)
	for _, x := range d.Sessions {
		out = append(out, ledgerRow{sessionStmt, []any{x.ID, x.Project, x.MachineID, x.Worktree, x.CCSessionID,
			x.Label, x.State, x.BlockedWhy, x.OpenedAt, x.Harness, x.AccountID},
			fmt.Sprintf("원장 세션 되쓰기 실패(id=%q project=%q)", clip(x.ID, 64), clip(x.Project, 64))})
	}
	judgmentStmt := ledgerInsert("judgment", judgmentLedgerCols)
	for _, j := range d.Judgments {
		out = append(out, ledgerRow{judgmentStmt, []any{j.ID, j.Project, j.SessionID, j.At, j.Kind, j.Title, j.Body, j.Supersedes,
			j.Author, j.Via},
			fmt.Sprintf("원장 판단 되쓰기 실패(id=%q kind=%q)", clip(j.ID, 64), clip(j.Kind, 32))})
	}
	linkStmt := ledgerInsert("judgment_link", linkCols)
	for _, l := range d.Links {
		out = append(out, ledgerRow{linkStmt, []any{l.JudgmentID, l.TargetKind, l.TargetID, l.TargetProject},
			fmt.Sprintf("원장 링크 되쓰기 실패(judgment=%q target=%s/%s)",
				clip(l.JudgmentID, 64), clip(l.TargetKind, 32), clip(l.TargetID, 64))})
	}
	snapshotStmt := ledgerInsert("snapshot", snapshotCols)
	for _, sn := range d.Snapshots {
		out = append(out, ledgerRow{snapshotStmt, []any{sn.Project, sn.Key, sn.Value, sn.Method,
			sn.Evidence, sn.InputDigest, sn.ComputedAt},
			fmt.Sprintf("원장 스냅숏 되쓰기 실패(project=%q key=%q)", clip(sn.Project, 64), clip(sn.Key, 64))})
	}
	return out
}

// ledgerOpenRefusal 은 원장 열기를 거절하는 문구다. 순수 함수다.
//
// ★ 왜 갈래를 가르나. 예전에는 non-None 넷을 한 문장으로 뭉쳤다:
//
//	"이 바이너리로 열면 DB 가 바뀐다(%s) — … (먼저 fd serve 를 이 바이너리로 올려 스키마를 맞춰라)"
//
// 그런데 non-None 중 **셋이 MigrateReject** 이고, 그것은 정의상 "열어도 DB 를 안 바꾼다 —
// 아예 안 연다"다. 앞 절이 거짓이고, 뒤 절은 어느 갈래도 fd serve 재기동으로 안 풀린다
// (남의 DB거나 · 끊긴 마이그레이션이라 백업에서 되돌려야 하거나 · 바이너리가 낡았거나).
//
// 특히 dbVersion > codeVersion 에서는 한 문장이 **정반대를 말했다**. Reason 은 "바이너리를
// 올려라"인데 꼬리는 "이 바이너리로 serve 를 올려라"이고, 그 처방을 따르면 selfcheck 가
// 같은 판정으로 기동도 거절한다. 이 저장소에서 실제로 났던 상황이다(수동 빌드 서버가
// 19시간·115커밋만큼 낡은 채로 돌았다) — 하필 판단 원장을 뜨는 것이 가장 절실한
// 상황에서만 보이는 문구였다.
//
// ★ MigrateReject 에는 처방을 **일부러 안 붙인다.** Reason 이 갈래마다 다른 처방을 이미
// 담고 있어서(손으로 확인하라 · 백업에서 되돌려라 · 바이너리를 올려라), 고정 꼬리를 붙이면
// 그중 하나와 반드시 어긋난다. 갈래를 가르는 선례는 selfcheck.go 가 이미 쓴다.
func ledgerOpenRefusal(plan MigrationPlan) error {
	// Reason 은 PlanMigration 이 항상 채운다 — 어느 갈래든 새 사유를 지어내지 않는다.
	if plan.Action == MigrateReject {
		return fmt.Errorf("이 바이너리는 이 DB 를 아예 열지 않는다(스키마도 안 바꾼다) "+
			"— 원장 내보내기를 거절한다: %s", plan.Reason)
	}
	return fmt.Errorf("이 바이너리로 열면 DB 가 바뀐다(%s) — 원장 내보내기를 거절한다: %s "+
		"(먼저 fd serve 를 이 바이너리로 올려 스키마를 맞춰라)", plan.Action, plan.Reason)
}

// OpenLedger 는 원장을 읽기 위해 DB 를 연다. **스키마를 바꾸지 않는다.**
//
// ★ store.Open 을 쓰지 않는 이유. 그것은 verifyPragmas 다음에 반드시 s.migrate 를 돌고,
// 판정에 따라 증분을 적용하며 그 앞에서 VACUUM INTO 백업을 뜬다. 즉 낡은 DB 를 만나면
// **백업하기 전에 스키마를 바꾼다.** 백업이 그 계기가 되면 안 된다.
//
// ★ mode=ro 를 쓰지 않는 이유. 서버가 죽은 채 -wal 이 남아 있으면 읽기 전용 연결은
// WAL 복구를 못 해 **열기 자체가 실패한다.** 원장 내보내기는 정확히 그 상황에서 돌아야 한다.
// "이행이 필요하면 거절"이 mode=ro 보다 강하다 — 애초에 바꿀 수 있는 상태로 안 들어간다.
//
// 없는 파일에는 오류를 낸다. sql.Open 은 파일을 **만들기** 때문에, 부재를 확인 안 하고 열면
// 내보내기가 빈 DB 를 하나 만들어 놓고 "0건 내보냈다"고 말한다(ProbeMigration 과 같은 판단).
func OpenLedger(ctx context.Context, path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := sqliteOnly(path, "원장 내보내기(fd export --judgments)"); err != nil {
		return nil, err
	}
	// ★ 이 재기는 대상 파일을 한 바이트도 안 바꾼다 — "거절한다"고 인쇄하는 실행조차
	//   아카이브를 변조하던 것을 probe.go 가 DSN 에서 막는다(그 함수의 주석 참고).
	plan, err := ProbeMigration(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("원장을 읽기 전에 DB 상태를 재지 못했다(path=%q): %w", clip(path, 200), err)
	}
	if plan.Action != MigrateNone {
		return nil, ledgerOpenRefusal(plan)
	}
	return openSQLiteReader(ctx, path, ledgerDSN(path), log)
}

// openSQLiteReader 는 SQLite 파일 path 를 접속 문자열 connStr(ledgerDSN · importSourceDSN)로 읽으려고 연다. 판을 재지도
// 스키마를 바꾸지도 않는다 - 판은 부르는 쪽이 잰다(원장 내보내기는 이 바이너리의 판, 이관은 이관 판). 원장 내보내기
// (OpenLedger)와 SQLite 이관의 원본(openImportSource)이 나눠 쓴다.
//
// 없는 파일에는 오류를 낸다. sql.Open 은 파일을 **만들기** 때문이다(OpenLedger 의 ★).
func openSQLiteReader(ctx context.Context, path, connStr string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("DB 파일이 없다(path=%q): %w", clip(path, 200), err)
	}
	db, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("읽기용 sqlite 열기 실패(path=%q): %w", clip(path, 200), err)
	}
	// 읽기 한 갈래뿐이라 커넥션을 늘릴 이유가 없다. 트랜잭션이 커넥션 하나에 묶인다.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("읽기용 sqlite 접속 실패(path=%q): %w", clip(path, 200), err)
	}

	s := &Store{db: &conn{raw: db, eng: engineSQLite}, path: path, log: log}
	// DSN pragma 가 실제로 걸렸는지 되읽어 확인한다 — 드라이버는 모르는 pragma 를 조용히 무시한다.
	// ledgerDSN 이 요청한 것만 본다. journal_mode 는 요청하지 않았으므로 요구하지도 않는다.
	if err := s.verifyPragmas(ctx, ledgerWantPragmas); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// RollbackOutcome 은 되돌리기 한 번의 결과다.
type RollbackOutcome struct {
	// Backup 은 고른 백업이다(SQLite 는 원장 옆 사본, Postgres 는 판올림 전 덤프).
	Backup string
	// Kept 는 덮기 전의 원장을 남긴 이름이다(SQLite, <db>.pre-rollback-<시각> 과 그 옆 파일). 되돌리기는 지금 원장을 지우지 않는다.
	Kept string
	// Steps 는 사람이 돌 절차의 설명이다. 비어 있으면 이 명령이 원장을 이미 갈았다(SQLite). Postgres 는 살아 있는 DB 를
	// 덮지 않고, 덤프를 새 DB 로 복원하는 절차를 낸다(pgRollbackSteps).
	Steps string
	// Script 는 Postgres 의 복원 셸 스크립트다(pgRollbackScript). 그대로 돌리는 것이 절차다 - 접속은 환경 변수로만 받고
	// 출력에 비밀번호도 호스트도 없다.
	Script string
}

// Rollback 은 판올림 전 백업 중 **가장 최근 것**으로 되돌린다. `fd migrate --rollback` 의 본체다. 엔진은 path 로 가른다.
//
//   - SQLite: <db>.bak-* 중 최신이 이 원장의 온전한 사본일 때만 원장 위에 덮는다(rollbackSQLite).
//   - Postgres: 원장에 붙어 식별과 판을 읽고, backupDir 의 덤프 중 그 원장의 최신(fd-<원장>-from<판>-<시각>.dump)을 고른다.
//     머리를 재고, 덤프 뒤로 커밋된 판올림이 있을 때만 복원 절차를 낸다(rollbackPG). 원장은 바꾸지 않는다.
func Rollback(ctx context.Context, path, backupDir string, log *slog.Logger) (RollbackOutcome, error) {
	if log == nil {
		log = slog.Default()
	}
	if ParseTarget(path).Postgres() {
		return rollbackPG(ctx, ParseTarget(path), backupDir)
	}
	return rollbackSQLite(ctx, path, time.Now(), log)
}

// rollbackSQLite 는 <db>.bak-* 중 가장 최근 것으로 DB 를 되돌린다. 고른 백업과 지금 원장을 남긴 이름을 낸다. at 은 남기는 이름의
// 시각이다(<db>.pre-rollback-<at>).
//
// ★ 덮기 전에 후보를 잰다(rollbackCandidate) - 다 뜬 사본이고(integrity_check · 옆 저널 없음) **이 원장의** 판올림 전 사본이고
// 그 뒤로 이 원장에 커밋된 판올림이 있어야 한다. 덜 뜬 사본이나 남의 원장의 사본으로 덮으면 되돌리기가 원장을 잃는 자리가 된다.
// ★ **지금 원장을 지우지 않는다.** 덮기 전에 원장 파일을 <db>.pre-rollback-<시각> 으로 베끼고 -wal · -shm · -journal 을 그
// 이름 옆으로 옮긴다(옆 파일까지 한 벌이라 그 이름으로 열면 덮기 직전 상태다 - keepLedger). 앞선 판은 -wal 을 지웠다 - 체크포인트
// 안 된 커밋이 거기 있으면 되돌리기와 함께 사라졌다. 옆 파일은 원장 자리에서는 치워야 한다: 백업은 VACUUM INTO 로 뜬 일관 사본이라
// WAL 이 없는데, 옛 -wal 을 남기면 되돌린 파일 위에 반쯤 적용된 상태가 되살아난다.
// ★ 남기는 이름은 **있는 파일을 갈지 않는다**(linkFresh). 같은 초에 두 번 돌면 두 번째가 같은 이름을 고르는데, 앞선 판은 그
// 이름을 rename 으로 덮어 첫 번째가 남긴 원장(판올림 뒤의 기록이 남은 마지막 사본)을 지웠다.
// ★ 순서: 후보를 원장 옆 임시 파일로 베껴 굳히고 → 지금 원장을 베껴 남기고 → 옆 파일을 남긴 이름으로 옮기고 → 임시 파일을 원장
// 이름으로 바꾼다. 어디서 끊겨도 원장 자리에 파일이 없는 순간이 없고(이름 바꾸기 하나로 갈린다), 지금 원장은 남긴 이름에 있다.
//
// ★ **서버가 멈춰 있어야 한다.** 이 함수는 그것을 강제하지 못한다 — 파일을 쥔 프로세스를
// 이 자리에서 알 방법이 없다. 도는 서버 밑에서 파일을 갈면 그 프로세스는 옛 페이지 캐시
// 위에서 계속 돌고, 그 상태는 어디에도 안 나타난다.
func rollbackSQLite(ctx context.Context, path string, at time.Time, log *slog.Logger) (RollbackOutcome, error) {
	var out RollbackOutcome
	// 옮긴 원장(이관 표지)의 판을 되돌리면 SQLite 원장이 다시 살아난다 - 표지는 README 의 두 절차만 손으로 지운다(moved.go 머리말).
	if err := movedRefusal(path); err != nil {
		return out, err
	}
	src, err := rollbackCandidate(ctx, path)
	if err != nil {
		return out, err
	}
	staged, err := stageCopy(src, path)
	if err != nil {
		return out, fmt.Errorf("백업을 원장 옆에 베끼지 못했다(%q): %w", clip(src, 200), err)
	}
	defer os.Remove(staged) // 이름을 바꾼 뒤면 지울 것이 없다
	kept, err := keepLedger(path, path+".pre-rollback-"+fileStamp(at))
	if err != nil {
		return out, fmt.Errorf("덮기 전에 지금 원장을 남기지 못했다 - 덮지 않는다: %w", err)
	}
	if err := os.Rename(staged, path); err != nil {
		return out, fmt.Errorf("백업을 되돌리지 못했다(%q → %q): %w - 지금 원장은 %q 에 있다(옆 파일 포함)",
			clip(src, 200), clip(path, 200), err, clip(kept, 200))
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return out, fmt.Errorf("되돌린 원장의 디렉토리를 굳히지 못했다(%q): %w", clip(path, 200), err)
	}
	log.Info("백업으로 되돌렸다", "path", path, "backup", src, "kept", kept)
	return RollbackOutcome{Backup: src, Kept: kept}, nil
}

// rollbackCandidate 는 되돌릴 백업을 고른다: <db>.bak-* 중 가장 최근의 보통 파일이고, 다 뜬 원장이고(checkLedgerCopy), 이 바이너리가
// 여는 판이고, **이 원장의** 판올림 전 사본이고, 그 뒤로 이 원장에 커밋된 판올림이 있어야 한다. 하나라도 아니면 고르지 않고 사유를
// 낸다 - 그보다 옛 사본으로 내려가 고르지 않는다(그 사이의 기록을 잃는다).
//
// ★ 옆 파일(-journal · -wal · -shm)은 후보가 아니다. 뜨는 도중 끊긴 옛 판의 사본은 <db>.bak-<시각>-journal 을 남기는데, 이름이
// 사본보다 뒤에 정렬돼 「가장 최근」으로 골라졌다.
// ★ 「이 원장의 사본」은 판 기록(schema_version 의 판 · 적용 시각)으로 잰다: 사본의 판 기록이 지금 원장의 판 기록 앞부분과 같아야
// 한다. 판올림은 원장의 옛 판 행을 고치지 않고 위에 더하기만 하므로 이 원장을 올리기 전에 뜬 사본이면 늘 같다. 원장을 바꿔 놓은 뒤
// (역이관이 낸 파일을 둔 72시간 되돌리기 · 손으로 되살리기) 남은 옛 원장의 사본은 판 행을 새로 적은 원장과 어긋난다 - 그 사본으로
// 덮으면 맞대기를 마친 원장이 그 뒤의 기록 없이 옛 판으로 돌아간다. 판만 내리려면 `fd migrate --to N` 이다(DownSteps). 이 재기는
// 판 기록이 같은 복제 원장(같은 판 · 같은 적용 시각, 다른 기록)의 사본은 가르지 못한다 - 그래서 원장을 바꾸는 자리가 옛 후보를
// 치운다(quarantineBackups).
// ★ **판 기록이 지금 원장과 같은 사본은 고르지 않는다.** 되돌리기는 판올림을 무르는 명령인데, 같으면 그 사본 뒤로 이 원장에 커밋된
// 판올림이 없다: 판올림이 첫 단에서 실패해 되감겼거나(무를 것이 없다), 앞선 되돌리기가 이미 그 사본으로 덮었다. 뒤 갈래에서 덮으면
// 첫 되돌리기 뒤에 쓴 기록만 잃는다 - 같은 초에 두 번 돈 되돌리기가 판 19 원장의 마지막 사본을 판 18 로 덮은 길이 이것이었다.
func rollbackCandidate(ctx context.Context, path string) (string, error) {
	found, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		return "", fmt.Errorf("백업을 찾지 못했다(path=%q): %w", clip(path, 200), err)
	}
	var backups []string
	for _, b := range found {
		if fi, err := os.Lstat(b); err == nil && fi.Mode().IsRegular() && !isSidecar(b) {
			backups = append(backups, b)
		}
	}
	if len(backups) == 0 {
		return "", fmt.Errorf("되돌릴 백업이 없다 — %s.bak-* 이 하나도 없다. "+
			"백업은 판올림이 기존 데이터 위에 증분을 얹을 때만 뜬다(빈 DB 를 새로 세운 경우는 안 뜬다)",
			clip(path, 200))
	}
	// 파일명의 스탬프는 폭이 고정된 UTC 라 사전순이 곧 시간순이다(timeLayout 과 같은 규율).
	sort.Strings(backups)
	src := backups[len(backups)-1]
	refuse := func(why string, args ...any) error {
		return fmt.Errorf("가장 최근 백업 %s 으로 되돌리지 않는다 - "+why+". 그보다 옛 백업으로 되돌리려면(그 사이의 기록을 잃는다) "+
			"이 파일을 다른 이름으로 옮긴 뒤 다시 돈다", append([]any{clip(src, 200)}, args...)...)
	}
	rows, err := checkLedgerCopy(ctx, src)
	if err != nil {
		return "", refuse("다 뜬 원장이 아니다(%v)", err)
	}
	if v := topVersion(rows); v < BaseSchemaVersion || v > SchemaVersion {
		return "", refuse("그 사본은 판 %d 이다(이 바이너리는 %d-%d 판을 연다)", v, BaseSchemaVersion, SchemaVersion)
	}
	cur, err := ledgerVersionRows(ctx, path)
	if err != nil {
		return "", refuse("지금 원장의 판 기록을 읽지 못해 이 원장의 사본인지 모른다(%v)", err)
	}
	if !isPrefix(rows, cur) {
		return "", refuse("이 원장의 판올림 전 사본이 아니다(판 기록이 지금 원장과 다르다 - 원장을 바꿔 놓은 뒤 남은 옛 원장의 사본이다: " +
			"역이관 · 되살리기). 덮으면 그 뒤의 기록을 잃는다. 판만 내리려면 `fd migrate --to <판>` 이 인덱스만 더한 판을 기록을 잃지 " +
			"않고 내린다(README 「72시간 안에 되돌리기」)")
	}
	if len(rows) == len(cur) {
		return "", refuse("그 사본은 판 기록이 지금 원장과 같다(판 %d) - 그 뒤로 이 원장에 커밋된 판올림이 없어 무를 것이 없다(판올림이 "+
			"첫 단에서 되감겼거나 앞선 되돌리기가 이미 그 사본으로 덮었다). 덮으면 그 사본 뒤에 쓴 기록만 잃는다", topVersion(cur))
	}
	return src, nil
}

// isSidecar 는 p 가 SQLite 원장 옆 파일(-wal · -shm · -journal, pg.Sidecars)의 이름인가다.
func isSidecar(p string) bool {
	for _, side := range pg.Sidecars("") {
		if strings.HasSuffix(p, side) {
			return true
		}
	}
	return false
}

// versionRow 는 schema_version 의 한 행이다. 판올림 전 사본이 어느 원장의 것인지 이 행들로 잰다(rollbackCandidate).
type versionRow struct {
	Version   int
	AppliedAt string
}

// topVersion 은 판 기록의 가장 높은 판이다. 비었으면 0 이다.
func topVersion(rows []versionRow) int {
	if len(rows) == 0 {
		return 0
	}
	return rows[len(rows)-1].Version
}

// isPrefix 는 판 기록 a 가 b 의 앞부분과 같은가다(같아도 된다).
func isPrefix(a, b []versionRow) bool {
	return len(a) <= len(b) && slices.Equal(a, b[:len(a)])
}

// readVersionRows 는 schema_version 의 행 전부를 판 순서로 읽는다. 표가 없으면 오류다.
func readVersionRows(ctx context.Context, q Reader) ([]versionRow, error) {
	var out []versionRow
	err := eachRow(ctx, q, `SELECT version, applied_at FROM schema_version ORDER BY version`, nil, "schema_version",
		func(r *rowSet) error {
			var v versionRow
			if err := r.Scan(&v.Version, &v.AppliedAt); err != nil {
				return err
			}
			out = append(out, v)
			return nil
		})
	return out, err
}

// checkLedgerCopy 는 판올림 전 사본 path 가 다 뜬 원장인지 재고 그 판 기록을 낸다: 옆 파일(-journal · -wal)이 없고, integrity_check
// 가 ok 이고, 판 표를 읽는다. 사본을 뜰 때(backup)와 그것으로 덮기 전(rollbackCandidate)이 이 하나로 잰다. 파일을 바꾸지 않는다
// (읽기 전용 연결, importSourceDSN).
//
// ★ 옆 저널이 있으면 끝나지 않은 쓰기다 - 그 사본을 여는 연결이 저널을 되감아 다른 파일이 된다. 읽기 전용 연결은 되감지 못해 열기가
// 실패하지만, 사유를 먼저 말한다.
func checkLedgerCopy(ctx context.Context, path string) ([]versionRow, error) {
	for _, side := range pg.Sidecars(path) {
		if _, err := os.Lstat(side); err == nil {
			return nil, fmt.Errorf("옆에 %s 이 있다 - 뜨는 도중 끊긴 사본이다", clip(filepath.Base(side), 200))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("옆 파일 %s 을 재지 못했다: %w", clip(side, 200), err)
		}
	}
	r, err := openSQLiteReader(ctx, path, importSourceDSN(path), nil)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var verdict string
	if err := r.db.QueryRowContext(ctx, `PRAGMA integrity_check(1)`).Scan(&verdict); err != nil {
		return nil, fmt.Errorf("integrity_check 를 돌리지 못했다: %w", err)
	}
	if verdict != "ok" {
		return nil, fmt.Errorf("integrity_check: %s", clip(verdict, 200))
	}
	return readVersionRows(ctx, r.db)
}

// ledgerVersionRows 는 지금 원장 path 의 판 기록을 읽는다. 기록을 바꾸지 않는다 - 원장 내보내기 · 판 재기와 같은 접속 문자열(ledgerDSN)이고,
// 남은 -wal 의 커밋까지 읽는다.
func ledgerVersionRows(ctx context.Context, path string) ([]versionRow, error) {
	r, err := openSQLiteReader(ctx, path, ledgerDSN(path), nil)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return readVersionRows(ctx, r.db)
}

// stageCopy 는 src 를 name 옆 숨은 임시 파일(.<name 의 끝 이름>.stage-*)로 베껴 굳히고 그 이름을 낸다. 부르는 쪽이 이름을
// 붙이거나 지운다.
//
// ★ 최종 이름에 바로 쓰지 않는다 - 베끼는 도중 끊기면 원본도 사본도 아닌 파일이 그 이름으로 남는다. 되돌리기는 이미 무언가
// 잘못된 상황에서 부르는 명령이라, 그 자리에서 한 번 더 잃으면 남는 것이 없다.
func stageCopy(src, name string) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".stage-*")
	if err != nil {
		return "", err
	}
	if err := copyInto(tmp, src); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// keepLedger 는 덮기 전의 원장 path 를 name(<db>.pre-rollback-<시각>) 자리에 남기고 남긴 이름을 낸다: 원장 파일을 숨은 임시
// 파일로 베껴 굳힌 뒤, 원장 옆 파일(-wal · -shm · -journal)과 한 벌로 있는 이름을 갈지 않는 길로 붙이고(linkFresh - name 이나 그
// 옆 이름이 차 있으면 번호를 붙인다), 다 붙은 뒤에야 원장 옆 파일을 뗀다. 원장 파일은 옮기지 않고 베낀다 - 원장 자리에 파일이
// 없는 순간을 만들지 않는다(rollbackSQLite 의 ★).
//
// ★ 옆 파일은 모두 붙거나 하나도 안 붙는다(linkSet). 못 붙이면 이 실행이 붙인 것을 다 떼고 원장 옆 파일은 그대로 둔다 - -wal 만
// 떠나 원장이 체크포인트 안 된 커밋을 잃는 반쪽 상태도, 옆 파일 없는 남긴 원장도 만들지 않는다.
func keepLedger(path, name string) (string, error) {
	tmp, err := stageCopy(path, name)
	if err != nil {
		return "", fmt.Errorf("원장 %q 을 베끼지 못했다: %w", clip(path, 200), err)
	}
	sides := pg.Sidecars(path)
	kept, err := linkFresh(tmp, name, sides...)
	if kept == "" {
		os.Remove(tmp)
		return "", fmt.Errorf("지금 원장을 남길 이름을 붙이지 못했다(원장 옆 파일은 그대로다): %w", err)
	}
	if err != nil {
		return kept, err
	}
	for i, keptSide := range pg.Sidecars(kept) {
		if _, err := os.Lstat(keptSide); err != nil {
			continue // 원장 옆에 없던 옆 파일이다 - 붙인 이름 옆 자리는 비어 있었다(linkSet)
		}
		if err := os.Remove(sides[i]); err != nil {
			return kept, fmt.Errorf("옆 파일 %q 을 원장 옆에서 떼지 못했다(같은 파일이 %q 에도 있다): %w",
				clip(sides[i], 200), clip(keptSide, 200), err)
		}
	}
	return kept, syncDir(filepath.Dir(path))
}

// maxNameNumber 는 freshName 이 이름 뒤에 붙여 보는 번호의 끝이다(-001 … -999).
const maxNameNumber = 999

// freshName 은 base + suffix 가 차 있으면 base-001 + suffix, base-002 + suffix … 를 차례로 create 에 넘겨, 처음으로 새로 만든 이름을
// 낸다. create 는 그 이름이 이미 있으면 fs.ErrExist 로 실패하는 연산이어야 한다(link(2) · mkdir(2) - **있는 것을 갈지 않는다**).
// 번호는 폭이 고정돼 사전순이 곧 붙인 순서다(rollbackCandidate 의 정렬).
func freshName(base, suffix string, create func(string) error) (string, error) {
	for i := 0; i <= maxNameNumber; i++ {
		name := base + suffix
		if i > 0 {
			name = fmt.Sprintf("%s-%03d%s", base, i, suffix)
		}
		err := create(name)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("%q 을 만들지 못했다: %w", clip(name, 200), err)
		}
		return name, nil
	}
	return "", fmt.Errorf("%q 와 그 뒤 번호 %d 개가 다 차 있다", clip(base+suffix, 200), maxNameNumber)
}

// linkFresh 는 다 쓰고 굳힌 파일 tmp 에 이름 name 을 붙이되 **이미 있는 이름은 갈지 않는다**(freshName - 하드 링크는 대상이
// 있으면 실패한다, os.Rename 은 말없이 덮는다). sides 는 tmp 와 한 벌로 그 이름 옆에 붙일 옆 파일이다(pg.Sidecars 순서) - 붙일
// 이름과 그 옆 이름이 모두 빈 첫 번호에 한 벌로 붙는다(linkSet). 붙인 이름을 내고, tmp 를 떼고, 그 디렉토리를 굳힌다. 이름을
// 붙이기 전에 실패하면 빈 이름과 오류를 내고 tmp 는 부르는 쪽이 지운다. 붙인 뒤에 실패하면 붙인 이름과 오류를 낸다(그 이름의
// 파일은 다 쓴 것이다).
//
// ★ 같은 초에 두 실행이 같은 이름을 고르는 자리가 둘이다: 판올림 전 사본(<db>.bak-<초>, sealBackup)과 되돌리기가 남기는 지금
// 원장(<db>.pre-rollback-<초>, keepLedger). 앞선 판은 rename 으로 이름을 붙여 뒤 실행이 앞 실행의 파일을 덮었다.
func linkFresh(tmp, name string, sides ...string) (string, error) {
	to, err := freshName(name, "", func(n string) error { return linkSet(tmp, sides, n) })
	if err != nil {
		return "", err
	}
	if err := os.Remove(tmp); err != nil {
		return to, fmt.Errorf("%q 에 이름을 붙였는데 임시 이름 %q 을 떼지 못했다: %w", clip(to, 200), clip(tmp, 200), err)
	}
	if err := syncDir(filepath.Dir(to)); err != nil {
		return to, fmt.Errorf("%q 에 붙인 이름을 굳히지 못했다(디렉토리 fsync): %w", clip(to, 200), err)
	}
	return to, nil
}

// linkSet 은 tmp 를 n 에, sides[i] 를 n 의 옆 이름(pg.Sidecars(n)[i])에 하드 링크로 붙인다 - 모두 붙거나 하나도 안 붙는다. n 이나
// 그 옆 이름이 하나라도 차 있으면 아무것도 붙이지 않고 fs.ErrExist 다(freshName 이 다음 번호로 간다). 옆 이름은 원본 옆 파일이
// 없어도 비어 있어야 한다 - 끊긴 앞 실행이 남긴 -wal 이 새로 붙인 파일에 얹혀 되살아나지 않게. 없는 원본 옆 파일은 건너뛴다.
func linkSet(tmp string, sides []string, n string) error {
	dsts := pg.Sidecars(n)
	for _, d := range append([]string{n}, dsts...) {
		if _, err := os.Lstat(d); err == nil {
			return fs.ErrExist
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Link(tmp, n); err != nil {
		return err
	}
	made := []string{n}
	for i, side := range sides {
		err := os.Link(side, dsts[i])
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			for _, m := range made {
				os.Remove(m)
			}
			return fmt.Errorf("옆 파일 %q 을 %q 옆에 붙이지 못했다: %w", clip(side, 200), clip(n, 200), err)
		}
		made = append(made, dsts[i])
	}
	return nil
}

// fileStamp 는 원장 옆에 남기는 이름의 시각 꼴이다(UTC, 폭이 고정돼 사전순이 곧 시간순이다). 판올림 전 사본(BackupSuffix) ·
// 되돌리기가 남긴 원장 · 치운 사본의 디렉토리가 이 하나를 쓴다.
func fileStamp(at time.Time) string { return at.UTC().Format("20060102T150405Z") }

// quarantineBackups 는 원장 path 옆의 판올림 전 사본(<path>.bak-* - 그 옆 파일까지)을 새 디렉토리
// <path>.pre-<why>-<시각>.bak/ 로 옮기고 그 디렉토리를 낸다. 옮길 것이 없으면 빈 이름이다. 원장 자리의 원장이 바뀌는(또는 그
// 자리를 떠나는) 자리가 이 하나를 부른다: 역이관이 낸 원장에 이름을 붙이기 전(ExportSQLite), 이관이 표지를 남긴 뒤(ImportSQLite).
// 손으로 하는 교체는 README 의 절차가 같은 꼴의 디렉토리로 옮긴다(「72시간 안에 되돌리기」 · 「Postgres 원장을 SQLite 로 되돌리기」).
//
// ★ 왜 판 기록 재기(rollbackCandidate 의 isPrefix)만으로 모자라나: 원장을 복제해 두 곳에 다른 기록을 쌓으면 두 원장의 판과
// 적용 시각이 같다 - 한쪽의 사본이 다른 쪽의 앞부분 재기를 지난다. 그 사본으로 덮으면 이 원장의 기록이 남의 기록으로 갈린다.
// 원장이 바뀌는 순간에 옛 후보를 치우면 그 뒤에 남는 후보는 바뀐 원장이 뜬 사본뿐이다.
// ★ 지우지 않는다. 디렉토리는 새로 만든다(있으면 번호를 붙인다 - 있는 디렉토리에 섞지 않는다). 새 디렉토리라 옮긴 이름이 겹치지
// 않는다. 하나라도 못 옮기면 거기서 서고 오류가 남은 것을 댄다.
func quarantineBackups(path, why string, at time.Time) (string, error) {
	found, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		return "", fmt.Errorf("판올림 전 사본을 찾지 못했다(path=%q): %w", clip(path, 200), err)
	}
	if len(found) == 0 {
		return "", nil
	}
	dir, err := freshName(path+".pre-"+why+"-"+fileStamp(at), ".bak", func(n string) error { return os.Mkdir(n, 0o755) })
	if err != nil {
		return "", fmt.Errorf("판올림 전 사본을 치울 디렉토리를 만들지 못했다: %w", err)
	}
	for _, f := range found {
		if err := os.Rename(f, filepath.Join(dir, filepath.Base(f))); err != nil {
			return dir, fmt.Errorf("판올림 전 사본 %q 을 %q 로 옮기지 못했다(앞의 것은 옮겼다): %w", clip(f, 200), clip(dir, 200), err)
		}
	}
	if err := syncDir(dir); err != nil {
		return dir, fmt.Errorf("판올림 전 사본을 옮긴 디렉토리 %q 를 굳히지 못했다: %w", clip(dir, 200), err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return dir, fmt.Errorf("판올림 전 사본을 옮긴 뒤 %q 의 디렉토리를 굳히지 못했다: %w", clip(path, 200), err)
	}
	return dir, nil
}

// copyInto 는 src 의 바이트를 열린 파일 dst 에 쓰고 굳힌 뒤 닫는다. 실패해도 dst 를 닫는다.
func copyInto(dst *os.File, src string) error {
	in, err := os.Open(src)
	if err != nil {
		dst.Close()
		return err
	}
	defer in.Close()
	if _, err := io.Copy(dst, in); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// syncFile 은 파일 p 를 굳힌다(fsync).
func syncFile(p string) error {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
