package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// SQLite 원장을 빈 Postgres 원장으로 옮기고(`fd migrate import-sqlite`), 두 원장을 맞댄다(`fd migrate verify`).
// 무엇을 옮기고 무엇을 거절하고 행을 어떻게 재는지는 pg 의 import.go · verify.go(순수 규칙)이고, 여기는 두 원장을 열고
// 읽고 쓰는 실행이다.
//
// 이관 한 번의 길:
//
//	원본 열기(판 18 인가) → 대상 열기 → 실행기 세션(잠금 둘) → 대상 확인(판 18 · 업무 표 전부 0행 · 로케일)
//	→ 원본 스냅숏 하나에서: 1차 훑기(사전 검사 · 행 표지 · 행 수 · 발급 번호) → 걸린 것이 있으면 아무것도 안 쓰고 끝
//	→ 대상 트랜잭션 하나에서: 소유 역할 · FK 미루기 → 표마다 묶음 INSERT(2차 훑기) → setval → 전 칸 대조 → 커밋
//
// ★ 대조가 트랜잭션 **안**에 있다. 어긋나면 커밋하지 않는다 - 옮긴 원장이 원본과 다른 채로 남는 길이 없다. 커밋 뒤의
// 대조는 `fd migrate verify` 가 같은 함수(checkTarget)로 다시 한다.
//
// ★ 두 훑기가 원본의 한 스냅숏(ReadTx)을 본다. 1차가 잰 행과 2차가 옮긴 행이 같다(서버를 내린 사본이 전제지만, 붙은
// 연결이 있어도 같은 시점이다).
//
// ★ setval 은 트랜잭션이 아니다(되감겨도 남는다). 대상이 빈 원장이라 남는 번호는 해가 없고, 다시 돌리면 다시 맞춘다.

// ImportOptions 는 이관 한 번의 선택이다.
type ImportOptions struct {
	// DryRun 은 아무것도 쓰지 않는다: 원본의 판 · 모양 · 사전 검사와, 대상이 Postgres 면 그 판 · 빈 원장 · 로케일을 잰다.
	// 잠그지 않는다.
	DryRun bool
}

// FaultGroup 은 사전 검사에 걸린 한 갈래(표 · 칸 · 사유)다.
type FaultGroup struct {
	Table, Column string
	Fault         pg.Fault
	Count         int
	// Keys 는 걸린 행의 좌표다(pg.Table.KeyLabel). 앞쪽 pg.MaxListed 개만 싣는다.
	Keys []string
}

// TableRows 는 원본 표 하나의 행 수다.
type TableRows struct {
	Table string
	Rows  int
}

// ImportReport 는 이관 · 대조 한 번의 결과다. 오류로 끝나도 거기까지 잰 것을 싣는다.
type ImportReport struct {
	// From · To 는 표시값이다(Target.Shown) - 접속 URL 원문을 싣지 않는다.
	From, To string
	DryRun   bool
	// TargetChecked 는 대상 원장을 쟀는가다. 예행의 대상이 Postgres 접속 URL 이 아니면 거짓이다.
	TargetChecked bool
	Faults        []FaultGroup
	// Rows 는 원본 표마다 행 수다(pg.Tables 순서).
	Rows      []TableRows
	Tables    []pg.TableCheck
	Sequences []pg.SequenceCheck
	// Copied 는 대상에 넣은 행 수다(이관이 커밋했을 때만 0 이 아니다).
	Copied int
}

// Matched 는 대조가 돌았고 표 전부 · 순번 전부가 맞았는가다.
func (r ImportReport) Matched() bool {
	if len(r.Tables) == 0 {
		return false
	}
	for _, c := range r.Tables {
		if !c.OK() {
			return false
		}
	}
	for _, c := range r.Sequences {
		if !c.OK() {
			return false
		}
	}
	return true
}

// ImportSQLite 는 SQLite 원장 from 을 빈 Postgres 원장 to 로 옮긴다(opt.DryRun 이면 재기만 한다).
func ImportSQLite(ctx context.Context, from, to string, opt ImportOptions, log *slog.Logger) (ImportReport, error) {
	if log == nil {
		log = slog.Default()
	}
	tt := ParseTarget(to)
	rep := ImportReport{From: ParseTarget(from).Shown(), To: tt.Shown(), DryRun: opt.DryRun}
	src, err := openImportSource(ctx, from, log)
	if err != nil {
		return rep, err
	}
	defer src.Close()
	if opt.DryRun {
		return rep, dryRunImport(ctx, src, tt, log, &rep)
	}
	tgt, err := openImportTarget(tt, log)
	if err != nil {
		return rep, err
	}
	defer tgt.Close()
	rep.TargetChecked = true
	start := time.Now()
	if err := tgt.importInto(ctx, src, &rep); err != nil {
		return rep, err
	}
	log.Info("SQLite 이관 완료", "from", rep.From, "to", rep.To, "rows", rep.Copied,
		"duration", time.Since(start).Seconds())
	return rep, nil
}

// dryRunImport 는 쓰지 않는 이관이다. 원본의 사전 검사와 대상의 확인을 둘 다 잰 뒤 걸린 것을 함께 낸다.
func dryRunImport(ctx context.Context, src *Store, tt Target, log *slog.Logger, rep *ImportReport) error {
	srcErr := src.ReadTx(ctx, func(q Reader) error {
		sc, err := scanSource(ctx, q)
		if err != nil {
			return err
		}
		sc.report(rep)
		return sc.refusal()
	})
	if !tt.Postgres() {
		return srcErr
	}
	tgt, err := openImportTarget(tt, log)
	if err != nil {
		return errors.Join(srcErr, err)
	}
	defer tgt.Close()
	rep.TargetChecked = true
	return errors.Join(srcErr, targetReady(ctx, tgt.db))
}

// importInto 는 이 원장(대상)에 src 를 옮긴다. 실행기 세션이 판 확인부터 커밋까지 잠금 둘을 쥔다 - 다른 실행기와 업무
// 쓰기가 그 사이에 이 원장을 바꾸지 못하므로 확인한 「빈 원장」이 커밋 때도 그대로다.
func (s *Store) importInto(ctx context.Context, src *Store, rep *ImportReport) error {
	sess, err := s.openMigrateSession(ctx)
	if err != nil {
		return err
	}
	defer sess.release()
	if err := targetReady(ctx, sess); err != nil {
		return err
	}
	return src.ReadTx(ctx, func(q Reader) error {
		sc, err := scanSource(ctx, q)
		if err != nil {
			return err
		}
		sc.report(rep)
		if err := sc.refusal(); err != nil {
			return err
		}
		return s.sessionTx(ctx, sess, "SQLite 이관", func(ctx context.Context, tx *txConn) error {
			eng := tx.dialect()
			for _, stmt := range []string{eng.stepRole(), eng.deferForeignKeys()} {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("이관 트랜잭션 준비(%s) 실패: %w", stmt, err)
				}
			}
			n, err := copyTables(ctx, q, tx)
			if err != nil {
				return err
			}
			if err := setSequences(ctx, tx, sc.floors); err != nil {
				return err
			}
			if rep.Tables, rep.Sequences, err = checkTarget(ctx, tx, sc); err != nil {
				return err
			}
			if !rep.Matched() {
				return errors.New("옮긴 원장이 원본과 맞지 않는다 - 커밋하지 않는다(대조표를 보라)")
			}
			rep.Copied = n
			return nil
		})
	})
}

// VerifySQLite 는 SQLite 원장 from 과 Postgres 원장 to 를 맞댄다(표마다 행 수 · 전 칸 행 지문, identity 순번). 쓰지 않는다.
// 어긋나면 보고와 함께 오류를 낸다.
func VerifySQLite(ctx context.Context, from, to string, log *slog.Logger) (ImportReport, error) {
	if log == nil {
		log = slog.Default()
	}
	tt := ParseTarget(to)
	rep := ImportReport{From: ParseTarget(from).Shown(), To: tt.Shown(), TargetChecked: true}
	src, err := openImportSource(ctx, from, log)
	if err != nil {
		return rep, err
	}
	defer src.Close()
	tgt, err := openImportTarget(tt, log)
	if err != nil {
		return rep, err
	}
	defer tgt.Close()
	if err := targetVersion(ctx, tgt.db); err != nil {
		return rep, err
	}
	err = src.ReadTx(ctx, func(q Reader) error {
		sc, err := scanSource(ctx, q)
		if err != nil {
			return err
		}
		sc.report(&rep)
		return tgt.ReadTx(ctx, func(r Reader) error {
			rep.Tables, rep.Sequences, err = checkTarget(ctx, r, sc)
			return err
		})
	})
	if err != nil {
		return rep, err
	}
	if !rep.Matched() {
		return rep, errors.New("두 원장이 맞지 않는다(대조표를 보라)")
	}
	return rep, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 두 원장 열기
// ─────────────────────────────────────────────────────────────────────────────

// openImportSource 는 이관 원본을 읽으려고 연다. 판이 이관 판(pg.ImportVersion)이어야 한다. 파일을 바꾸지 않는다
// (openSQLiteReader).
//
// ★ 더 낮은 판은 이 명령이 올리지 않는다. SQLite 증분은 판 18 에서 멈췄고, SQLite 실행 경로가 빠진 뒤의 fd 는 그 증분을
// 갖지 않는다 - 그 판을 아는 fd 로 사본을 먼저 올리라고 안내한다.
func openImportSource(ctx context.Context, path string, log *slog.Logger) (*Store, error) {
	if err := sqliteOnly(path, "이관 원본(--from)"); err != nil {
		return nil, err
	}
	s, err := openSQLiteReader(ctx, path, log)
	if err != nil {
		return nil, err
	}
	hasTable, v, objects, err := readMigrationState(ctx, s.db)
	if err == nil {
		err = sourceRefusal(PlanMigration(hasTable, v, objects, pg.ImportVersion), v)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// sourceRefusal 은 원본의 판 plan(판 v)을 이관이 받지 않는 사유다. 받으면 nil 이다. 순수 함수다.
func sourceRefusal(plan MigrationPlan, v int) error {
	switch plan.Action {
	case MigrateNone:
		return nil
	case MigrateUpgrade:
		return fmt.Errorf("원본이 SQLite 판 %d 이다 - 이관은 마지막 SQLite 판(%d)만 읽는다. 먼저 이 판의 fd 로 원본 사본을 "+
			"판올림하라: `fd migrate --db <사본>` 을 돌린 뒤 다시 이관한다(이관은 증분을 스스로 적용하지 않는다)",
			v, pg.ImportVersion)
	case MigrateApply:
		return fmt.Errorf("원본에 flightdeck 원장이 없다(빈 DB 다) - 옮길 것이 없다: %s", plan.Reason)
	}
	return fmt.Errorf("원본을 이관하지 않는다: %s", plan.Reason)
}

// openImportTarget 은 이관 · 대조의 대상(Postgres 원장)을 판 재기 없이 연다. 판은 부르는 쪽이 잰다(targetReady ·
// targetVersion) - 이관은 기동과 다른 조건(빈 원장)을 본다.
func openImportTarget(t Target, log *slog.Logger) (*Store, error) {
	if !t.Postgres() {
		return nil, fmt.Errorf("대상은 Postgres 접속 URL 이어야 한다(--db 또는 FD_DB, 지금 %s) - SQLite 에서 SQLite 로는 옮기지 않는다",
			clip(t.Shown(), 200))
	}
	return openPG(t, log, lockTimeout)
}

// targetVersion 은 대상 원장이 이관 판(기준선)에 있는가다. 이관 카탈로그(pg.Tables)가 그 판의 모양이다.
func targetVersion(ctx context.Context, q dbtx) error {
	hasTable, v, objects, err := readMigrationState(ctx, q)
	if err != nil {
		return fmt.Errorf("대상의 판을 재지 못했다: %w", err)
	}
	plan := PlanMigration(hasTable, v, objects, pg.ImportVersion)
	switch plan.Action {
	case MigrateNone:
		return nil
	case MigrateApply:
		return fmt.Errorf("대상이 빈 DB 다 - 먼저 기준선을 깔아라: 초사용자나 소유 역할(%s)로 `fd migrate --db <대상> --to %d`",
			pg.OwnerRole, pg.ImportVersion)
	}
	return fmt.Errorf("대상이 이관 판(%d)이 아니다: %s", pg.ImportVersion, plan.Reason)
}

// targetReady 는 대상 원장이 이관을 받을 수 있는가다: 이관 판이고, 로케일 관문을 지나고, 업무 표가 전부 비었다.
//
// ★ 빈 원장에만 쓴다. 행이 있는 원장에 얹으면 무엇이 원본의 것인지 가를 수 없고, 추가 전용 표(judgment · event ·
// item_revision)는 잘못 얹은 행을 지울 수도 없다.
func targetReady(ctx context.Context, q dbtx) error {
	if err := targetVersion(ctx, q); err != nil {
		return err
	}
	if err := localeGate(ctx, q); err != nil {
		return err
	}
	var busy []string
	for _, t := range pg.Tables() {
		var has bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+t.Name+`)`).Scan(&has); err != nil {
			return fmt.Errorf("대상의 %s 를 재지 못했다: %w", t.Name, err)
		}
		if has {
			busy = append(busy, t.Name)
		}
	}
	if len(busy) > 0 {
		return fmt.Errorf("대상이 비어 있지 않다(행이 있는 표: %s) - 이관은 빈 원장에만 쓴다", strings.Join(busy, ", "))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 원본 훑기 - 사전 검사 · 행 표지 · 발급 번호
// ─────────────────────────────────────────────────────────────────────────────

// sourceScan 은 원본 1차 훑기의 결과다.
type sourceScan struct {
	faults []FaultGroup
	at     map[faultAt]int // faults 안의 자리
	rows   []TableRows
	marks  map[string][]pg.RowMark
	// floors 는 identity 가 있는 표마다 원본에서 발급된 가장 큰 번호다(pg.Sequence 의 floor).
	floors map[string]int64
}

type faultAt struct {
	table, column string
	fault         pg.Fault
}

func (sc *sourceScan) fault(table, column string, f pg.Fault, key string) {
	at := faultAt{table, column, f}
	i, ok := sc.at[at]
	if !ok {
		i = len(sc.faults)
		sc.at[at] = i
		sc.faults = append(sc.faults, FaultGroup{Table: table, Column: column, Fault: f})
	}
	g := &sc.faults[i]
	g.Count++
	if len(g.Keys) < pg.MaxListed {
		g.Keys = append(g.Keys, key)
	}
}

func (sc *sourceScan) report(rep *ImportReport) {
	rep.Faults, rep.Rows = sc.faults, sc.rows
}

// refusal 은 사전 검사에 걸린 것이 있으면 그 수를 담은 오류다.
func (sc *sourceScan) refusal() error {
	n := 0
	for _, g := range sc.faults {
		n += g.Count
	}
	if n == 0 {
		return nil
	}
	return fmt.Errorf("사전 검사에 %d 건(%d 갈래)이 걸렸다 - 아무것도 쓰지 않는다. 원본 사본에서 걸린 행을 고친 뒤 다시 돌려라",
		n, len(sc.faults))
}

// scanSource 는 원본을 한 번 훑는다: 모양을 재고, 표마다 행을 읽어 칸마다 사전 검사(pg.Cell)를 하고, 행 표지와 발급
// 번호를 모은다. 끝에 FK 를 잰다. q 는 원본의 읽기 트랜잭션이다.
func scanSource(ctx context.Context, q Reader) (*sourceScan, error) {
	if err := checkSourceShape(ctx, q); err != nil {
		return nil, err
	}
	seqs, err := sqliteSequences(ctx, q)
	if err != nil {
		return nil, err
	}
	sc := &sourceScan{at: map[faultAt]int{}, marks: map[string][]pg.RowMark{}, floors: map[string]int64{}}
	for _, t := range pg.Tables() {
		if err := sc.table(ctx, q, t, seqs[t.Name]); err != nil {
			return nil, err
		}
	}
	if err := foreignKeyFaults(ctx, q, sc); err != nil {
		return nil, err
	}
	return sc, nil
}

// table 은 원본 표 하나를 훑는다. seq 는 그 표의 sqlite_sequence 값이다(AUTOINCREMENT 가 아니면 0).
func (sc *sourceScan) table(ctx context.Context, q Reader, t pg.Table, seq int64) error {
	id, hasID := t.Identity()
	idAt := t.Index(id.Name)
	pks := map[string]bool{} // 지문 기본키(pg.Table.DigestPK) - 지문 칸이 있는 표만 찬다
	var floor int64
	var marks []pg.RowMark
	err := eachCells(ctx, q, t, true, func(vals []any, faults []pg.Fault) error {
		m := t.Mark(vals)
		for i, f := range faults {
			if f != "" {
				sc.fault(t.Name, t.Cols[i].Name, f, m.Key)
			}
		}
		if pk, ok := t.DigestPK(vals); ok {
			if pks[pk] {
				sc.fault(t.Name, t.DigestKey, pg.FaultDigest, m.Key)
			}
			pks[pk] = true
		}
		if hasID {
			if n, ok := vals[idAt].(int64); ok {
				floor = max(floor, n)
			}
		}
		marks = append(marks, m)
		return nil
	})
	if err != nil {
		return err
	}
	if hasID {
		if t.AutoIncrement {
			floor = max(floor, seq)
		}
		sc.floors[t.Name] = floor
	}
	sc.marks[t.Name] = marks
	sc.rows = append(sc.rows, TableRows{Table: t.Name, Rows: len(marks)})
	return nil
}

// checkSourceShape 는 원본의 표 · 칸이 이관 카탈로그(pg.Tables)와 같은지 잰다. 판 18 이면 같아야 한다 - 다르면 손으로
// 고친 원장이고, 모르는 칸을 버리거나 없는 칸을 지어내지 않으려고 거절한다.
func checkSourceShape(ctx context.Context, q Reader) error {
	names, err := strings1(ctx, q, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("원본의 표 목록을 읽지 못했다: %w", err)
	}
	want := map[string]pg.Table{}
	for _, t := range pg.Tables() {
		want[t.Name] = t
	}
	var bad []string
	for _, n := range names {
		if _, ok := want[n]; !ok && !pg.LeftBehind(n) {
			bad = append(bad, "모르는 표 "+n)
		}
	}
	for _, t := range pg.Tables() {
		cols, err := strings1(ctx, q, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, t.Name)
		if err != nil {
			return fmt.Errorf("원본 %s 의 칸을 읽지 못했다: %w", t.Name, err)
		}
		switch exp := t.SourceColumns(); {
		case len(cols) == 0:
			bad = append(bad, "없는 표 "+t.Name)
		case !slices.Equal(cols, exp):
			bad = append(bad, fmt.Sprintf("%s 의 칸 %v(기대 %v)", t.Name, cols, exp))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("원본의 모양이 SQLite 판 %d 과 다르다(%s) - 손으로 고친 원장은 옮기지 않는다",
			pg.ImportVersion, strings.Join(bad, "; "))
	}
	return nil
}

// sqliteSequences 는 sqlite_sequence 의 표 → 발급한 최고 번호다. AUTOINCREMENT 는 지워진 번호를 다시 내지 않으므로
// 이 값이 max(id) 보다 클 수 있다.
func sqliteSequences(ctx context.Context, q Reader) (map[string]int64, error) {
	out := map[string]int64{}
	err := eachRow(ctx, q, `SELECT name, seq FROM sqlite_sequence`, nil, "원본 sqlite_sequence", func(r *rowSet) error {
		var name string
		var seq sql.NullInt64
		if err := r.Scan(&name, &seq); err != nil {
			return err
		}
		out[name] = seq.Int64
		return nil
	})
	return out, err
}

// foreignKeyFaults 는 원본에서 부모가 없는 행을 사전 검사에 싣는다(PRAGMA foreign_key_check). 기준선은 커밋 때 그 행을
// 거절한다. 좌표는 rowid 다(그 표를 `WHERE rowid = N` 으로 찾는다).
func foreignKeyFaults(ctx context.Context, q Reader, sc *sourceScan) error {
	type miss struct {
		table, parent string
		rowid         sql.NullInt64
		fkid          int
	}
	var misses []miss
	err := eachRow(ctx, q, `SELECT "table", rowid, parent, fkid FROM pragma_foreign_key_check`, nil, "원본 FK 검사",
		func(r *rowSet) error {
			var m miss
			if err := r.Scan(&m.table, &m.rowid, &m.parent, &m.fkid); err != nil {
				return err
			}
			misses = append(misses, m)
			return nil
		})
	if err != nil {
		return err
	}
	// 칸 이름은 훑기가 끝난 뒤 읽는다 - 결과를 연 채로 같은 트랜잭션에 질의를 겹치지 않는다.
	for _, m := range misses {
		cols, err := strings1(ctx, q, `SELECT "from" FROM pragma_foreign_key_list(?) WHERE id = ? ORDER BY seq`, m.table, m.fkid)
		if err != nil {
			return fmt.Errorf("원본 %s 의 FK 칸: %w", m.table, err)
		}
		key := "rowid=" + strconv.FormatInt(m.rowid.Int64, 10) + " → " + m.parent
		sc.fault(m.table, strings.Join(cols, ","), pg.FaultForeignKey, key)
	}
	return nil
}

// strings1 은 첫 칸이 글인 질의의 값 목록이다.
func strings1(ctx context.Context, q Reader, query string, args ...any) ([]string, error) {
	var out []string
	err := eachRow(ctx, q, query, args, "목록", func(r *rowSet) error {
		var v string
		if err := r.Scan(&v); err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	return out, err
}

// eachCells 는 표 t 의 행을 차례로 읽어 칸마다 기준선 값으로 옮긴 것(pg.Cell)과 그 사전 검사 사유를 fn 에 준다. sqlite 면
// rowid 자리 칸을 rowid 로 읽는다. 두 원장이 이 하나로 읽는다 - 대조의 두 쪽이 같은 꼴을 잰다.
//
// ★ vals 는 행마다 새로 만든다. fn 이 그것을 붙들어도(묶음 INSERT) 다음 행이 덮지 않는다.
func eachCells(ctx context.Context, q Reader, t pg.Table, sqlite bool, fn func(vals []any, faults []pg.Fault) error) error {
	raw := make([]any, len(t.Cols))
	ptrs := make([]any, len(t.Cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	return eachRow(ctx, q, t.SelectSQL(sqlite), nil, t.Name, func(r *rowSet) error {
		if err := r.Scan(ptrs...); err != nil {
			return err
		}
		vals := make([]any, len(t.Cols))
		faults := make([]pg.Fault, len(t.Cols))
		for i, c := range t.Cols {
			vals[i], faults[i] = pg.Cell(c, raw[i])
		}
		return fn(vals, faults)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 옮기기 · 순번 · 대조
// ─────────────────────────────────────────────────────────────────────────────

// 묶음 INSERT 의 상한. 행 수와 값 바이트 중 먼저 닿는 쪽에서 끊는다. 자리표시 수는 Postgres 의 상한(65535) 아래로 맞춘다.
const (
	batchRows      = 500
	batchBytes     = 8 << 20
	maxPGPlaceArgs = 65535
)

// copyTables 는 원본 q 의 표 전부를 대상 트랜잭션 tx 에 FK 순서로 넣고 넣은 행 수를 낸다. 값은 원본 그대로다(identity
// 칸도 옛 번호 그대로 - pg.Table.InsertSQL 이 그 구절을 붙인다).
func copyTables(ctx context.Context, q Reader, tx *txConn) (int, error) {
	total := 0
	for _, t := range pg.Tables() {
		limit := min(batchRows, maxPGPlaceArgs/len(t.Cols))
		var args []any
		rows, size := 0, 0
		flush := func() error {
			if rows == 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, t.InsertSQL(rows), args...); err != nil {
				return fmt.Errorf("대상에 넣기 실패: %w", err)
			}
			total += rows
			args, rows, size = args[:0], 0, 0
			return nil
		}
		err := eachCells(ctx, q, t, true, func(vals []any, _ []pg.Fault) error {
			args = append(args, vals...)
			rows++
			for _, v := range vals {
				switch x := v.(type) {
				case string:
					size += len(x)
				case []byte:
					size += len(x)
				}
			}
			if rows >= limit || size >= batchBytes {
				return flush()
			}
			return nil
		})
		if err != nil {
			return total, err
		}
		if err := flush(); err != nil {
			return total, err
		}
	}
	return total, nil
}

// setSequences 는 identity 시퀀스마다 원본의 발급 번호 다음부터 내게 맞춘다(pg.Sequence). 발급된 적 없는 표도 맞춘다 -
// 대조(checkTarget)가 모든 identity 를 같은 규칙으로 잰다.
func setSequences(ctx context.Context, tx *txConn, floors map[string]int64) error {
	for _, t := range pg.Tables() {
		c, ok := t.Identity()
		if !ok {
			continue
		}
		v, called := pg.Sequence(floors[t.Name])
		if _, err := tx.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence(?, ?), ?, ?)`, t.Name, c.Name, v, called); err != nil {
			return fmt.Errorf("%s.%s 순번 맞추기 실패: %w", t.Name, c.Name, err)
		}
	}
	return nil
}

// checkTarget 은 대상 r 을 원본 훑기 sc 와 맞댄다: 표마다 행 지문의 다중집합(pg.Compare), identity 마다 다음 번호.
// 이관의 커밋 전 대조와 `fd migrate verify` 가 이 하나를 쓴다.
func checkTarget(ctx context.Context, r Reader, sc *sourceScan) ([]pg.TableCheck, []pg.SequenceCheck, error) {
	var tables []pg.TableCheck
	var seqs []pg.SequenceCheck
	for _, t := range pg.Tables() {
		var marks []pg.RowMark
		err := eachCells(ctx, r, t, false, func(vals []any, _ []pg.Fault) error {
			marks = append(marks, t.Mark(vals))
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("대상 %w", err)
		}
		tables = append(tables, pg.Compare(t.Name, sc.marks[t.Name], marks))
		c, ok := t.Identity()
		if !ok {
			continue
		}
		next, err := nextIdentity(ctx, r, t.Name, c.Name)
		if err != nil {
			return nil, nil, err
		}
		seqs = append(seqs, pg.SequenceCheck{Table: t.Name, Column: c.Name, Floor: sc.floors[t.Name], Next: next})
	}
	return tables, seqs, nil
}

// nextIdentity 는 대상 identity 시퀀스가 다음에 낼 번호다. 시퀀스 이름은 카탈로그가 준다(pg_get_serial_sequence 는
// 필요하면 따옴표를 붙여 낸다).
func nextIdentity(ctx context.Context, r Reader, table, col string) (int64, error) {
	var name sql.NullString
	if err := r.QueryRowContext(ctx, `SELECT pg_get_serial_sequence(?, ?)`, table, col).Scan(&name); err != nil {
		return 0, fmt.Errorf("%s.%s 의 시퀀스를 찾지 못했다: %w", table, col, err)
	}
	if !name.Valid {
		return 0, fmt.Errorf("%s.%s 에 identity 시퀀스가 없다", table, col)
	}
	var last int64
	var called bool
	if err := r.QueryRowContext(ctx, `SELECT last_value, is_called FROM `+name.String).Scan(&last, &called); err != nil {
		return 0, fmt.Errorf("%s.%s 의 순번을 읽지 못했다: %w", table, col, err)
	}
	return pg.NextValue(last, called), nil
}
