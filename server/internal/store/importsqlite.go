package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// SQLite 원장을 빈 Postgres 원장으로 옮기고(`fd migrate import-sqlite`), 두 원장을 맞댄다(`fd migrate verify`). 거꾸로
// Postgres 원장을 같은 판의 새 SQLite 파일로 옮기는 역이관(`fd migrate export-sqlite`, exportsqlite.go)도 이 길을 그대로 탄다.
// 무엇을 옮기고 무엇을 거절하고 행을 어떻게 재는지는 pg 의 import.go · verify.go(순수 규칙)이고, 여기는 두 원장을 열고
// 읽고 쓰는 실행이다. 엔진마다 다른 몫(모양 검사 · 발급 번호 · 순번 맞추기 · 커밋 전 검사)은 ledgerSide 가 가른다.
//
// 옮기기 한 번의 길(transferInto - 이관 · 역이관이 같다):
//
//	원본 열기(SQLite: -wal 이 비었나 · 읽기 전용 · 이관 판 범위인가) → 대상 열기
//	→ 원본 스냅숏 하나에서: 판(Postgres) · 표 · 칸 모양, 붙은 다른 연결(Postgres), 1차 훑기(사전 검사 · 행 표지 · 행 수 ·
//	  발급 번호)
//	→ 실행기 세션(Postgres 대상이면 잠금 둘) → 대상 트랜잭션 하나에서: 판 · 붙은 다른 연결(Postgres) · 소유 역할 · FK 미루기 ·
//	  빈 원장(행 · 순번) · 로케일(사전 검사와 함께 낸다 - 걸린 것이 있으면 아무것도 안 쓰고 끝) → 표마다 묶음 INSERT(2차 훑기)
//	  → 순번 맞추기 → 미룬 FK 검사(SQLite 대상은 검색 색인 다시 짓기 · 재기도) → 전 칸 대조 → 커밋(예행은 되감기)
//
// ★ 대조가 트랜잭션 **안**에 있다. 어긋나면 커밋하지 않는다 - 옮긴 원장이 원본과 다른 채로 남는 길이 없다. 커밋 뒤의
// 대조는 `fd migrate verify` 가 같은 함수(checkTarget · verdict)로 다시 한다(SQLite 파일과 Postgres 원장을 맞대므로 역이관이
// 낸 파일에도 그대로 쓴다).
//
// ★ 예행(--dry-run)은 본 실행과 같은 길을 끝까지 돌고 커밋 대신 되감는다. 그래서 역할 권한 · 대상의 CHECK · 인덱스 항목
// 상한 · FK 처럼 사전 검사가 못 보는 거절도 예행이 본 실행과 똑같이 받는다.
//
// ★ 두 훑기가 원본의 한 스냅숏(ReadTx)을 본다. 1차가 잰 행과 2차가 옮긴 행이 같다. 판 · 모양도 그 스냅숏에서 잰다 - 연 뒤에
// 판이 오르거나 칸이 붙은 원장을 연 때의 판으로 옮기지 않는다.
//
// ★ 서버를 내린 원장이 전제고, Postgres 쪽은 그것을 잰다(soleClient): 이 실행 말고 붙은 연결이 있으면 거절한다. 원본에 붙은
// 서버가 스냅숏 뒤에 커밋한 것은 옮겨지지 않는데, 커밋 전 대조는 같은 스냅숏을 보므로 맞다고 한다. 대상에 붙은 서버는 실행기
// 잠금을 안 타는 쓰기(관측 기록)로 옮기는 트랜잭션에 끼어든다.
//
// ★ 순번은 `ALTER SEQUENCE … RESTART WITH` 로 맞춘다. setval 과 달리 트랜잭션에 묶여, 되감기면(예행 · 실패한 이관) 순번도
// 함께 되감긴다 - 실패한 뒤 다시 돌린 이관이 「번호를 낸 시퀀스」에 막히지 않는다.

// TransferOptions 는 이관 · 역이관 한 번의 선택이다.
type TransferOptions struct {
	// DryRun 은 본 실행과 같은 트랜잭션(역할 전환 · 빈 대상 검사 · INSERT · 대조)을 돈 뒤 커밋하지 않고 되감는다. 실행기
	// 잠금도 본 실행처럼 쥔다. 이관은 대상이 Postgres 접속 URL 이 아니면 원본만 잰다. 역이관은 임시 파일에 돌고 그 파일을
	// 지운다 - 아무 파일도 남지 않는다.
	DryRun bool
}

// CellGroup 은 칸 검사의 한 갈래(표 · 칸 · 사유)와 그 행들이다. 사유가 거절(pg.Fault)이면 사전 검사 · 대상 값 검사에 걸린
// 것이고(FaultGroup), 바꿔 넣은 규칙(pg.Rewrite)이면 원본에서 옮기며 바꾼 칸이다(RewriteGroup).
type CellGroup[R ~string] struct {
	Table, Column string
	Reason        R
	Count         int
	// Keys 는 그 행의 좌표다(pg.Table.KeyLabel). 앞쪽 pg.MaxListed 개만 싣는다.
	Keys []string
}

type (
	FaultGroup   = CellGroup[pg.Fault]
	RewriteGroup = CellGroup[pg.Rewrite]
)

// TableRows 는 원본 표 하나의 행 수다.
type TableRows struct {
	Table string
	Rows  int
}

// TransferReport 는 이관 · 역이관 · 대조 한 번의 결과다. 오류로 끝나도 거기까지 잰 것을 싣는다.
type TransferReport struct {
	// From · To 는 표시값이다(Target.Shown) - 접속 URL 원문을 싣지 않는다.
	From, To string
	DryRun   bool
	// TargetChecked 는 대상 원장을 쟀는가다. 예행의 대상이 Postgres 접속 URL 이 아니면 거짓이다.
	TargetChecked bool
	// Faults 는 원본의 사전 검사에 걸린 것이다.
	Faults []FaultGroup
	// TargetFaults 는 대상에서 읽은 값이 값 검사(pg.SettledCell - 원본의 검사에 바꿔 넣을 값 · 저장 갈래까지)에 걸린 것이다.
	// 대조가 대상 값을 읽을 때 잰다.
	TargetFaults []FaultGroup
	// Rewrites 는 원본 값을 정해진 규칙으로 바꿔 옮긴 칸이다(pg.Rewrite - SQLite 원본의 NUL). 이관은 그 값을 넣고, 대조는 같은
	// 규칙을 건 원본 값으로 맞댄다. RewrittenRows 는 그런 칸이 하나라도 있는 원본 행의 수다. 역이관에는 늘 비었다(SettledCell).
	Rewrites      []RewriteGroup
	RewrittenRows int
	// Rows 는 원본 표마다 행 수다(pg.Tables 순서).
	Rows      []TableRows
	Tables    []pg.TableCheck
	Sequences []pg.SequenceCheck
	// Copied 는 대상에 넣은 행 수다. 커밋한 뒤에만 채운다(예행 · 실패면 0 이다). 역이관은 새 파일을 낸 뒤에만 채운다.
	Copied int
	// Quarantined 는 SQLite 원장 자리의 판올림 전 사본(<원장>.bak-*)을 옮긴 디렉토리다(quarantineBackups). 옮길 것이 없었거나 예행이면
	// 비었다. 이관은 원본 옆, 역이관은 --to 옆의 사본을 옮긴다.
	Quarantined string
}

// Matched 는 대조가 돌았고 표 전부 · 순번 전부가 맞았는가다. 값 검사에 걸린 것까지 보는 판정은 verdict 다.
func (r TransferReport) Matched() bool {
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

// verdict 는 보고가 「두 원장이 같다」를 말하는가다(nil 이면 같다). 원본의 사전 검사, 대상 값의 검사, 대조 셋 중 걸린 것을
// 함께 낸다. 이관 · 역이관의 커밋 전 대조와 `fd migrate verify` 가 이 하나로 판정한다.
//
// ★ 사전 검사에 걸린 원본은 대조가 맞아도 같지 않다. 걸린 값은 이관이 받지 않는 값이다 - 대상과 바이트가 같아 보여도
// 그 행은 이관으로 생긴 것일 수 없다.
func (r TransferReport) verdict() error {
	var why []string
	if n := groupCount(r.Faults); n > 0 {
		why = append(why, fmt.Sprintf("원본 사전 검사에 %d 건(%d 갈래)이 걸렸다", n, len(r.Faults)))
	}
	if n := groupCount(r.TargetFaults); n > 0 {
		why = append(why, fmt.Sprintf("대상 값 검사에 %d 건(%d 갈래)이 걸렸다", n, len(r.TargetFaults)))
	}
	if !r.Matched() {
		why = append(why, "대조가 어긋났다")
	}
	if len(why) == 0 {
		return nil
	}
	return errors.New("두 원장이 맞지 않는다: " + strings.Join(why, " · ") + "(위 표를 보라)")
}

// ImportSQLite 는 SQLite 원장 from 을 빈 Postgres 원장 to 로 옮긴다(opt.DryRun 이면 같은 길을 돌고 되감는다). 커밋한 뒤 from
// 옆에 이관 표지(MovedMarker)를 남긴다 - 그 자리는 옮기기 전에 잡는다(reserveMovedMark, 예행도 잰다). 표지를 남긴 뒤 from 옆의
// 판올림 전 사본을 치운다(quarantineBackups) - 그 원장은 이제 원장이 아니고, 뒤에 그 자리에 놓일 원장(역이관이 낸 원장 · 되살린
// 원장)의 사본이 아니다. 못 치우면 옮긴 것과 표지는 그대로 두고 오류를 낸다.
func ImportSQLite(ctx context.Context, from, to string, opt TransferOptions, log *slog.Logger) (TransferReport, error) {
	if log == nil {
		log = slog.Default()
	}
	tt := ParseTarget(to)
	rep := TransferReport{From: ParseTarget(from).Shown(), To: tt.Shown(), DryRun: opt.DryRun}
	src, err := openImportSource(ctx, from, log)
	if err != nil {
		return rep, err
	}
	defer src.Close()
	mark, err := reserveMovedMark(from)
	if err != nil {
		return rep, err
	}
	defer mark.release(log)
	if opt.DryRun && !tt.Postgres() {
		return rep, src.ReadTx(ctx, func(q Reader) error {
			sc, err := scanLedger(ctx, q, sideOf(src), pgSide{}.intake())
			if err != nil {
				return err
			}
			sc.report(&rep)
			return sc.refusal()
		})
	}
	tgt, err := openImportTarget(tt, log)
	if err != nil {
		return rep, err
	}
	defer tgt.Close()
	rep.TargetChecked = true
	start := time.Now()
	if err := tgt.transferInto(ctx, src, "SQLite 이관", opt.DryRun, &rep); err != nil {
		return rep, err
	}
	if opt.DryRun {
		return rep, nil
	}
	if err := tgt.leaveMovedMark(ctx, mark, rep.Copied); err != nil {
		return rep, err
	}
	if rep.Quarantined, err = quarantineBackups(from, "import", time.Now()); err != nil {
		return rep, fmt.Errorf("옮겼고 이관 표지도 남겼다 - 원본 옆의 판올림 전 사본(%s.bak-*)을 치우지 못했다. 남은 것을 손으로 "+
			"%s.pre-import-<시각>.bak/ 같은 새 디렉토리로 옮긴다: %w", clip(from, 200), clip(from, 200), err)
	}
	log.Info("SQLite 이관 완료", "from", rep.From, "to", rep.To, "rows", rep.Copied, "rewritten_rows", rep.RewrittenRows,
		"marker", mark.path, "duration", time.Since(start).Seconds())
	return rep, nil
}

// errDryRun 은 예행의 body 가 커밋 대신 되감으려고 내는 표지다. transferInto 밖으로 나가지 않는다.
var errDryRun = errors.New("예행 - 커밋하지 않고 되감는다")

// transferInto 는 이 원장(대상)에 src 를 옮긴다(dryRun 이면 끝에 되감는다). what 은 오류 문구의 머리다. 이관(SQLite →
// Postgres)과 역이관(Postgres → SQLite)이 이 하나를 탄다 - 엔진마다 다른 몫은 두 원장의 ledgerSide 가 진다. 실행기 세션이
// 대상 확인부터 커밋까지 잠금 둘을 쥔다(Postgres 대상) - 다른 실행기와 업무 쓰기가 그 사이에 이 원장을 바꾸지 못하므로
// 확인한 「빈 원장」이 커밋 때도 그대로다. SQLite 대상(역이관이 만든 임시 파일)은 쓰기 트랜잭션(BEGIN IMMEDIATE)이 그 자리다.
//
// ★ 세션은 원본을 다 훑은 뒤에 연다. 전역 업무 잠금은 대상 트랜잭션 동안만 쥔다 - 잘못 고른 대상(서버가 쓰는 원장)이면
// 「비어 있지 않다」로 곧장 놓는다. 원본을 훑는 동안 그 서버의 쓰기를 세워 두지 않는다.
func (s *Store) transferInto(ctx context.Context, src *Store, what string, dryRun bool, rep *TransferReport) error {
	if dryRun {
		what += " 예행"
	}
	from, to := sideOf(src), sideOf(s)
	return src.ReadTx(ctx, func(q Reader) error {
		sc, err := scanLedger(ctx, q, from, to.intake())
		if err != nil {
			return err
		}
		sc.report(rep)
		sess, err := s.openMigrateSession(ctx)
		if err != nil {
			return errors.Join(err, sc.refusal())
		}
		defer sess.release()
		var copied int
		err = s.sessionTx(ctx, sess, what, func(ctx context.Context, tx *txConn) error {
			// 대상의 거절과 원본의 사전 검사를 함께 낸다 - 한 번 돌려 고칠 것을 다 본다.
			if err := errors.Join(prepareTarget(ctx, tx, to), sc.refusal()); err != nil {
				return err
			}
			n, err := copyTables(ctx, q, from, tx, to)
			if err != nil {
				return err
			}
			if err := to.align(ctx, tx, sc.floors); err != nil {
				return err
			}
			// 커밋 때 잴 것(미뤄 둔 FK)을 지금 앞당겨 잰다. 예행은 커밋하지 않으므로 이것이 없으면 본 실행이 커밋에서 받을
			// 거절을 못 본다.
			if err := to.settle(ctx, tx); err != nil {
				return err
			}
			if err := checkTarget(ctx, tx, to, sc, rep); err != nil {
				return err
			}
			if err := rep.verdict(); err != nil {
				return fmt.Errorf("%w - 커밋하지 않는다", err)
			}
			if dryRun {
				return errDryRun
			}
			copied = n
			return nil
		})
		switch {
		// 되감기까지 성공해야 예행이 끝난 것이다. 되감기가 실패하면 sessionTx 가 errDryRun 과 함께 errRollbackFailed 를
		// 싸서 내므로, 그때는 예행 성공으로 보지 않는다.
		case dryRun && errors.Is(err, errDryRun) && !errors.Is(err, errRollbackFailed):
			return nil
		case err != nil:
			return err
		}
		rep.Copied = copied // 커밋이 성공했다
		return nil
	})
}

// VerifySQLite 는 SQLite 원장 from 과 Postgres 원장 to 를 맞댄다(표마다 행 수 · 전 칸 행 지문, identity 순번). 쓰지 않는다.
// 원본이 사전 검사에 걸리거나, 대상 값이 검사에 걸리거나, 대조가 어긋나면 보고와 함께 오류를 낸다(verdict).
func VerifySQLite(ctx context.Context, from, to string, log *slog.Logger) (TransferReport, error) {
	if log == nil {
		log = slog.Default()
	}
	tt := ParseTarget(to)
	rep := TransferReport{From: ParseTarget(from).Shown(), To: tt.Shown(), TargetChecked: true}
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
		// 대조는 원문을 맞댄다 - 인덱스 키 상한(pg.ImportCell)은 옮겨 넣을 때만 잰다. 이미 두 원장에 있는 긴 값으로 대조가 막히면
		// 남는 처방이 감사 행을 자르는 것뿐이다.
		sc, err := scanLedger(ctx, q, sideOf(src), pg.Cell)
		if err != nil {
			return err
		}
		sc.report(&rep)
		return tgt.ReadTx(ctx, func(r Reader) error { return checkTarget(ctx, r, sideOf(tgt), sc, &rep) })
	})
	if err != nil {
		return rep, err
	}
	return rep, rep.verdict()
}

// ─────────────────────────────────────────────────────────────────────────────
// 두 원장 열기
// ─────────────────────────────────────────────────────────────────────────────

// openImportSource 는 이관 · 대조의 원본을 읽기 전용으로 연다(importSourceDSN). 옆의 -wal 이 비어 있어야 하고
// (sourceWALRefusal), 판이 이관 판 범위(transferPlan)여야 한다. 파일을 바꾸지 않는다.
//
// ★ 더 낮은 판은 이 명령이 올리지 않는다. 이관이 읽는 SQLite 판은 이관 판 범위이고, SQLite 실행 경로가 빠진 뒤의 fd 는 그
// 아래 증분을 갖지 않는다 - 그 판을 아는 fd 로 사본을 먼저 올리라고 안내한다.
func openImportSource(ctx context.Context, path string, log *slog.Logger) (*Store, error) {
	if err := sqliteOnly(path, "이관 원본(--from)"); err != nil {
		return nil, err
	}
	if err := sourceWALRefusal(path); err != nil {
		return nil, err
	}
	s, err := openSQLiteReader(ctx, path, importSourceDSN(path), log)
	if err != nil {
		return nil, err
	}
	hasTable, v, objects, err := readMigrationState(ctx, s.db)
	if err == nil {
		err = sourceRefusal(transferPlan(hasTable, v, objects), v)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// transferPlan 은 이관 · 역이관 · 대조가 원장의 판 상태(판 v)를 어떻게 보는가다. 순수 함수다. 판 범위(pg.Transferable)의 판이면 그
// 판을 기대 판으로 재고(받는다), 아니면 이관 판(pg.ImportVersion)으로 잰다 - 더 낮으면 MigrateUpgrade, 더 높으면 MigrateReject 다.
//
// ★ 판 범위 규칙은 pg.ImportFloor 하나이고, 이 함수가 그것을 세 문(이관 · 대조의 원본 openImportSource, 대상 targetVersion,
// 역이관 원본 pgSide.shape)에 똑같이 건다. 범위 안이면 원본과 대상의 판이 서로 달라도 받는다 - 범위 안 판끼리는 인덱스만 다르고
// 옮길 것(pg.Tables)이 같다. 모양은 판과 따로 잰다(checkShape).
func transferPlan(hasTable bool, v, objects int) MigrationPlan {
	want := pg.ImportVersion
	if pg.Transferable(v) {
		want = v
	}
	return PlanMigration(hasTable, v, objects, want)
}

// transferRange 는 이관 판 범위를 사람에게 적는 꼴이다(「18-19」, 범위가 한 판이면 「19」).
func transferRange() string {
	if f := pg.ImportFloor(); f < pg.ImportVersion {
		return fmt.Sprintf("%d-%d", f, pg.ImportVersion)
	}
	return fmt.Sprint(pg.ImportVersion)
}

// sourceRefusal 은 원본의 판 plan(판 v)을 이관이 받지 않는 사유다. 받으면 nil 이다. 순수 함수다.
func sourceRefusal(plan MigrationPlan, v int) error {
	switch plan.Action {
	case MigrateNone:
		return nil
	case MigrateUpgrade:
		return fmt.Errorf("원본이 SQLite 판 %d 이다 - 이관은 SQLite 판 %s 의 원장만 읽는다. 먼저 이 판의 fd 로 원본 사본을 "+
			"판올림하라: `fd migrate --db <사본>` 을 돌린 뒤 다시 이관한다(이관은 증분을 스스로 적용하지 않는다)",
			v, transferRange())
	case MigrateApply:
		return fmt.Errorf("원본에 flightdeck 원장이 없다(빈 DB 다) - 옮길 것이 없다: %s", plan.Reason)
	}
	return fmt.Errorf("원본을 이관하지 않는다: %s", plan.Reason)
}

// sourceWALRefusal 은 원본 path 옆의 -wal 에 바이트가 있으면 거절한다. 없거나 비었으면 nil 이다.
//
// ★ WAL 원장은 커밋을 먼저 -wal 에 쓰고 체크포인트 때 본 파일로 옮긴다. 비지 않은 -wal 은 원본에 쓰는 서버가 아직 돌거나
// (옮긴 뒤에도 쓰기가 이어져 이관이 그것을 잃는다) 비정상 종료로 체크포인트가 안 된 것이다. 어느 쪽이든 여기서 멈춘다.
// 거꾸로 `cp fd.db` 로 뜬 사본은 -wal 을 안 데려가 그 커밋을 잃는데, 이관과 대조가 둘 다 그 사본을 읽으므로 둘 다 맞다고
// 한다 - 이 검사도 그것은 못 본다. 그래서 처방이 VACUUM INTO 다(README 「SQLite 원장을 Postgres 로 옮기기」).
func sourceWALRefusal(path string) error {
	wal := path + "-wal"
	n, err := fileBytes(wal)
	switch {
	case err != nil:
		return fmt.Errorf("원본의 -wal 을 재지 못했다(path=%q): %w", clip(wal, 200), err)
	case n == 0:
		return nil
	}
	return fmt.Errorf("원본 옆 -wal 에 %d 바이트가 있다(path=%q) - 원본에 쓰는 서버가 아직 돌거나, 비정상 종료로 본 파일에 "+
		"안 옮긴 커밋이다. 서버를 내린 뒤 다시 돌리고, 그래도 남으면 `sqlite3 <원본> \"VACUUM INTO '<사본>'\"` 으로 뜬 사본을 "+
		"--from 에 준다(cp 로 뜬 사본은 -wal 의 커밋을 잃는다)", n, clip(wal, 200))
}

// fileBytes 는 파일 p 의 바이트 수다. 없으면 0 이다. 그 밖에 재지 못한 것(권한 · 고리 링크 · 입출력)은 오류다 - 없는 것으로
// 보지 않는다. 옆 파일(-wal)에 남은 커밋을 재는 자리(이관 원본 · 역이관 임시 파일)가 이 하나로 잰다.
func fileBytes(p string) (int64, error) {
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, nil
	case err != nil:
		return 0, err
	}
	return fi.Size(), nil
}

// openImportTarget 은 이관 · 대조의 대상(Postgres 원장)을 판 재기 없이 연다. 판은 부르는 쪽이 잰다(prepareTarget ·
// targetVersion) - 이관은 기동과 다른 조건(빈 원장)을 본다.
func openImportTarget(t Target, log *slog.Logger) (*Store, error) {
	if !t.Postgres() {
		return nil, fmt.Errorf("대상은 Postgres 접속 URL 이어야 한다(--db 또는 FD_DB, 지금 %s) - SQLite 에서 SQLite 로는 옮기지 않는다",
			clip(t.Shown(), 200))
	}
	return openPG(t, log, lockTimeout)
}

// targetVersion 은 대상 원장이 이관 판 범위(transferPlan)에 있는가다. 이관 카탈로그(pg.Tables)가 그 범위의 모양이다.
func targetVersion(ctx context.Context, q dbtx) error {
	hasTable, v, objects, err := readMigrationState(ctx, q)
	if err != nil {
		return fmt.Errorf("대상의 판을 재지 못했다: %w", err)
	}
	plan := transferPlan(hasTable, v, objects)
	switch plan.Action {
	case MigrateNone:
		return nil
	case MigrateApply:
		return fmt.Errorf("대상이 빈 DB 다 - 먼저 이관 판까지 깔아라: 초사용자나 소유 역할(%s)로 `fd migrate --db <대상> --to %d`",
			pg.OwnerRole, pg.ImportVersion)
	}
	return fmt.Errorf("대상이 이관 판 범위(%s)가 아니다: %s", transferRange(), plan.Reason)
}

// prepareTarget 은 옮기기 트랜잭션 tx 를 대상에 맞춘다: 판을 재고, 붙은 다른 연결이 없는지 재고(to.alone), 소유 역할로 바꿔
// 타고(Postgres), FK 검사를 미루고, 대상이 받을 수 있는지 잰다(targetReady). to 는 대상의 몫이다. 본 실행과 예행, 이관과
// 역이관이 이 하나를 지난다.
//
// ★ 판은 역할을 바꾸기 전에 잰다. 기준선을 안 깐 DB 에는 소유 역할이 없을 수 있고, 그때의 처방은 「이관 판까지 깔아라」다.
// ★ 붙은 연결도 역할을 바꾸기 전에 잰다. 초사용자로 붙었으면 그때는 남의 연결의 종류까지 보인다(soleClient).
// ★ 소유 역할이어야 하는 까닭: identity 시퀀스를 다시 시작하는 것(ALTER SEQUENCE)은 시퀀스의 주인만 하고, 순번을 읽는
// 권한(SELECT)도 앱 역할에는 없다. 앱 역할로 붙으면 여기서 거절한다 - 예행도 같다.
func prepareTarget(ctx context.Context, tx *txConn, to ledgerSide) error {
	if err := targetVersion(ctx, tx); err != nil {
		return err
	}
	if err := to.alone(ctx, tx, "대상"); err != nil {
		return err
	}
	eng := tx.dialect()
	if role := eng.stepRole(); role != "" {
		if _, err := tx.ExecContext(ctx, role); err != nil {
			return fmt.Errorf("소유 역할(%s)로 바꿔 타지 못했다 - 접속 계정이 초사용자나 그 역할의 멤버여야 한다: %w", pg.OwnerRole, err)
		}
	}
	if _, err := tx.ExecContext(ctx, eng.deferForeignKeys()); err != nil {
		return fmt.Errorf("FK 검사 미루기 실패: %w", err)
	}
	return targetReady(ctx, tx, to)
}

// targetReady 는 대상 원장이 옮기기를 받을 수 있는가다: 로케일 관문을 지나고, 업무 표가 전부 비었고, 순번이 번호를 낸
// 적이 없다(to.issued). q 는 소유 역할로 바꿔 탄 옮기기 트랜잭션이다(시퀀스를 읽는 권한).
//
// ★ 빈 원장에만 쓴다. 행이 있는 원장에 얹으면 무엇이 원본의 것인지 가를 수 없고, 추가 전용 표(judgment · event ·
// item_revision)는 잘못 얹은 행을 지울 수도 없다.
// ★ 행을 다 지운 원장도 시퀀스가 번호를 냈으면 빈 원장이 아니다. 이미 낸 번호는 원장 밖(받아 간 쪽)에 남아 있을 수
// 있는데, 이관이 순번을 원본의 번호로 되감으면 그 번호를 다시 낸다.
func targetReady(ctx context.Context, q dbtx, to ledgerSide) error {
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
	issued, err := to.issued(ctx, q)
	if err != nil {
		return err
	}
	if busy = append(busy, issued...); len(busy) > 0 {
		return fmt.Errorf("대상이 비어 있지 않다(행이 있는 표 · 번호를 낸 순번: %s) - 이관은 빈 원장에만 쓴다", strings.Join(busy, ", "))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 엔진마다 다른 몫
// ─────────────────────────────────────────────────────────────────────────────

// ledgerSide 는 옮기기의 한쪽 원장이 엔진마다 달리 하는 몫이다. 이관(SQLite → Postgres)과 역이관(Postgres → SQLite)이 같은
// 길(transferInto)을 타고 엔진에 따라 다른 것만 여기서 가른다 - 같은 엔진은 원본이든 대상이든 같은 몫을 쓴다. 원본으로는
// shape · alone · held · orphans 를, 대상으로는 alone · issued · align · settle · held · next · intake 를 부른다.
type ledgerSide interface {
	// sqlite 는 표를 읽고 넣는 문장의 갈래다(pg.Table.SelectSQL · InsertSQL).
	sqlite() bool
	// shape 는 원본의 판과 표 · 칸이 이관 카탈로그(pg.Tables)와 같은지 잰다. 원본 스냅숏 안에서 부른다.
	shape(ctx context.Context, q Reader) error
	// alone 은 이 원장에 이 실행 말고 붙은 연결이 없는지 잰다. what 은 문구의 머리(원본 · 대상)다.
	alone(ctx context.Context, q Reader, what string) error
	// held 는 identity 가 있는 표마다 원장이 행과 따로 기억하는 발급 번호다(pg.Floor 의 held).
	held(ctx context.Context, q Reader) (map[string]int64, error)
	// orphans 는 원본에서 부모가 없는 행을 사전 검사(ft)에 싣는다.
	orphans(ctx context.Context, q Reader, ft *tally[pg.Fault]) error
	// issued 는 대상에서 이미 번호를 낸 순번을 늘어놓는다(빈 원장 검사 targetReady 의 한 축). 비면 하나도 안 냈다.
	issued(ctx context.Context, q Reader) ([]string, error)
	// align 은 대상의 순번을 원본에서 발급된 번호(floors - pg.Floor) 다음으로 맞춘다.
	align(ctx context.Context, tx *txConn, floors map[string]int64) error
	// settle 은 커밋 때 대상이 할 검사를 앞당기고, 대상에만 있는 것(SQLite 의 검색 색인)을 짓는다.
	settle(ctx context.Context, tx *txConn) error
	// next 는 대상이 표 t 에서 다음에 낼 번호다. maxID · held 는 대상에서 잰 identity 최고값 · 발급 번호다.
	next(t pg.Table, maxID, held int64) int64
	// intake 는 이 원장으로 옮겨 넣을 원본 값에 칸마다 거는 규칙이다 - 사전 검사 · 행 표지 · 옮길 값이 이것 하나를 지난다.
	// 옮기는 길은 받는 엔진마다 하나라(이관 SQLite → Postgres, 역이관 Postgres → SQLite) 받는 쪽이 원본의 엔진도 정한다:
	// Postgres 는 SQLite 원본을 바꿔 넣고 인덱스 키 상한까지 잰다(pg.ImportCell), SQLite 는 Postgres 값을 그대로 받는다
	// (pg.SettledCell). 옮긴 대상의 값은 엔진과 무관하게 pg.SettledCell 로 잰다(checkTarget).
	intake() cellRule
}

// sideOf 는 원장 s 의 엔진 몫이다.
func sideOf(s *Store) ledgerSide {
	if s.db.eng == enginePG {
		return pgSide{}
	}
	return sqliteSide{}
}

// sqliteSide 는 SQLite 원장의 몫이다(이관의 원본, 역이관의 대상).
type sqliteSide struct{}

func (sqliteSide) sqlite() bool { return true }

// intake 는 pg.SettledCell 이다(역이관이 받는 Postgres 원본의 값). 역이관은 Postgres 값을 그대로 낸다 - 이관이 넣은 `\x00` 를
// NUL 로 되돌리지 않고, 바꿔 넣어야 할 값(JSON 안의 \u0000)이 든 원본은 바꾸지 않고 거절한다. 낸 파일이 Postgres 원장과
// 바이트로 맞아야 되돌리기의 대조가 그 파일을 그 원장의 사본이라고 말할 수 있다. 인덱스 키 상한은 재지 않는다 - 이미 원장에
// 있는 원문이다.
func (sqliteSide) intake() cellRule { return pg.SettledCell }

// shape 는 표 · 칸만 잰다. 판은 열 때 쟀다(openImportSource) - 원본은 -wal 이 빈 읽기 전용 파일이라 연 뒤에 판이 안 바뀐다.
func (sqliteSide) shape(ctx context.Context, q Reader) error { return checkShape(ctx, q, engineSQLite) }

// alone 은 잴 것이 없다. SQLite 에는 붙은 연결을 보는 자리가 없다 - 원본은 -wal 이 비었는지로 서버를 내렸는지 재고
// (sourceWALRefusal), 대상(역이관의 임시 파일)은 이 실행만 그 이름을 안다.
func (sqliteSide) alone(context.Context, Reader, string) error { return nil }

func (sqliteSide) held(ctx context.Context, q Reader) (map[string]int64, error) {
	return sqliteSequences(ctx, q)
}

func (sqliteSide) orphans(ctx context.Context, q Reader, ft *tally[pg.Fault]) error {
	return foreignKeyFaults(ctx, q, ft)
}

// issued 는 sqlite_sequence 에 번호가 남은 표다. AUTOINCREMENT 표는 행을 지워도 낸 번호가 거기 남는다. rowid 표
// (item_after)의 번호는 행에만 있어 빈 표 검사가 함께 잰다.
func (sqliteSide) issued(ctx context.Context, q Reader) ([]string, error) {
	seqs, err := sqliteSequences(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range pg.Tables() {
		if n := seqs[t.Name]; n > 0 {
			out = append(out, fmt.Sprintf("%s 순번(발급한 번호 %d)", t.Name, n))
		}
	}
	return out, nil
}

// align 은 AUTOINCREMENT 표마다 sqlite_sequence 를 원본에서 발급된 번호로 맞춘다 - 다음 번호가 pg.Sequence(floor) 다. 넣은
// 행이 이미 max(id) 까지 올려 두었지만, 원본에서 지워진 최고 번호(Postgres 시퀀스가 마지막에 낸 번호)는 행에 없어 여기서
// 되살린다. rowid 표(item_after)는 기억할 자리가 없다 - 다음 번호가 max(rowid)+1 이고 바닥도 그것이다(pg.Floor).
//
// ★ sqlite_sequence 에는 이름 유일 제약이 없다. 지우고 넣어야 표마다 한 행이고, 넣은 행이 없어 sqlite_sequence 행도 없는
// 표(UPDATE 가 놓친다)도 맞는다.
func (sqliteSide) align(ctx context.Context, tx *txConn, floors map[string]int64) error {
	for _, t := range pg.Tables() {
		if !t.AutoIncrement {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name = ?`, t.Name); err != nil {
			return fmt.Errorf("%s 순번 맞추기 실패: %w", t.Name, err)
		}
		if floor := floors[t.Name]; floor > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence(name, seq) VALUES (?, ?)`, t.Name, floor); err != nil {
				return fmt.Errorf("%s 순번 맞추기 실패: %w", t.Name, err)
			}
		}
	}
	return nil
}

// settle 은 미뤄 둔 FK 를 지금 재고(부모 없는 행이 있으면 거절 - pragma 는 위반을 오류로 내지 않고 행으로 낸다), 판단 검색
// 색인을 judgment 원문에서 다시 지어 잰다(pg.SearchRebuild · SearchCheck). Postgres 에는 그 색인이 없어 옮길 원본이 없다 -
// 새로 짓는 것이 옮기기다.
func (sqliteSide) settle(ctx context.Context, tx *txConn) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&n); err != nil {
		return fmt.Errorf("미뤄 둔 FK 를 재지 못했다: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("미뤄 둔 FK 검사에 %d 행이 걸렸다(부모가 없다)", n)
	}
	if _, err := tx.ExecContext(ctx, pg.SearchRebuild); err != nil {
		return fmt.Errorf("판단 검색 색인을 다시 짓지 못했다: %w", err)
	}
	if _, err := tx.ExecContext(ctx, pg.SearchCheck); err != nil {
		return fmt.Errorf("판단 검색 색인이 판단 행과 맞지 않는다: %w", err)
	}
	return nil
}

// next 는 SQLite 가 다음에 낼 번호다: AUTOINCREMENT 표는 greatest(max(id), sqlite_sequence.seq)+1, rowid 표는 max(rowid)+1.
func (sqliteSide) next(t pg.Table, maxID, held int64) int64 {
	return pg.Sequence(pg.Floor(t, maxID, held))
}

// pgSide 는 Postgres 원장의 몫이다(이관의 대상, 역이관의 원본).
type pgSide struct{}

func (pgSide) sqlite() bool { return false }

// intake 는 pg.ImportCell 이다(이관이 받는 SQLite 원본의 값). SQLite 는 Postgres 가 못 담는 값(NUL)을 담으므로 정해진 규칙으로
// 바꿔 옮기고 그 칸을 보고에 싣는다(pg.Cell). 인덱스 키 상한까지 잰다.
func (pgSide) intake() cellRule { return pg.ImportCell }

// shape 는 원본(역이관)의 판이 이관 판 범위(transferPlan)이고 표 · 칸이 그 범위의 모양인지 잰다. 원본은 살아 있는 서버의 원장이라 연 때가
// 아니라 옮기는 스냅숏에서 잰다 - 이관의 prepareTarget 이 대상의 판을 옮기기 트랜잭션 안에서 다시 재는 것과 같은 꼴이다.
func (pgSide) shape(ctx context.Context, q Reader) error {
	hasTable, v, objects, err := readMigrationState(ctx, onEngine{q, enginePG})
	if err != nil {
		return fmt.Errorf("원본의 판을 재지 못했다: %w", err)
	}
	if err := exportSourceRefusal(transferPlan(hasTable, v, objects)); err != nil {
		return err
	}
	return checkShape(ctx, q, enginePG)
}

func (pgSide) alone(ctx context.Context, q Reader, what string) error {
	return soleClient(ctx, q, what)
}

// held 는 identity 시퀀스마다 마지막에 낸 번호(다음 번호 - 1)다. 시퀀스를 읽는 권한이 있어야 한다(readIdentity).
func (pgSide) held(ctx context.Context, q Reader) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range pg.Tables() {
		c, ok := t.Identity()
		if !ok {
			continue
		}
		seq, err := readIdentity(ctx, q, t.Name, c.Name)
		if err != nil {
			return nil, err
		}
		out[t.Name] = pg.NextValue(seq.last, seq.called) - 1
	}
	return out, nil
}

// orphans 는 잴 것이 없다. 기준선이 FK 를 늘 재므로 부모 없는 행은 커밋되지 못했다.
func (pgSide) orphans(context.Context, Reader, *tally[pg.Fault]) error { return nil }

// issued 는 번호를 낸 identity 시퀀스다.
func (pgSide) issued(ctx context.Context, q Reader) ([]string, error) {
	var out []string
	for _, t := range pg.Tables() {
		c, ok := t.Identity()
		if !ok {
			continue
		}
		seq, err := readIdentity(ctx, q, t.Name, c.Name)
		if err != nil {
			return nil, err
		}
		// 새 기준선의 순번은 (시작값, 아직 안 냄)이다. 번호를 냈거나(called), 앞선 이관이 RESTART 로 옮겨 둔 순번(called 가
		// 거짓이어도 last ≠ 시작값)은 그 번호까지 이미 쓰인 원장의 흔적이다 - 다시 옮기면 순번을 되감아 낸 번호를 다시 낸다.
		if seq.called || seq.last != seq.start {
			out = append(out, fmt.Sprintf("%s.%s 순번(다음 번호 %d)", t.Name, c.Name, pg.NextValue(seq.last, seq.called)))
		}
	}
	return out, nil
}

// align 은 identity 시퀀스마다 다음 번호를 원본에서 발급된 번호 다음으로 다시 시작한다(pg.Sequence). 발급된 적 없는 표도
// 맞춘다 - 대조(checkTarget)가 모든 identity 를 같은 규칙으로 잰다.
//
// ★ setval 이 아니라 ALTER SEQUENCE 다. setval 은 트랜잭션 밖의 일이라 되감기지 않는다 - 예행이나 대조에 걸려 되감긴 이관이
// 순번만 바꿔 놓고, 다시 돌린 이관은 그 순번을 「번호를 낸 시퀀스」로 보고 거절한다. ALTER SEQUENCE 는 트랜잭션에 묶인다.
func (pgSide) align(ctx context.Context, tx *txConn, floors map[string]int64) error {
	for _, t := range pg.Tables() {
		c, ok := t.Identity()
		if !ok {
			continue
		}
		seq, err := readIdentity(ctx, tx, t.Name, c.Name)
		if err != nil {
			return err
		}
		// DDL 은 자리표시를 못 받는다. 이름은 카탈로그가 준 것이고(pg_get_serial_sequence 가 필요하면 따옴표를 붙인다)
		// 번호는 정수다.
		stmt := "ALTER SEQUENCE " + seq.name + " RESTART WITH " + strconv.FormatInt(pg.Sequence(floors[t.Name]), 10)
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s.%s 순번 맞추기 실패: %w", t.Name, c.Name, err)
		}
	}
	return nil
}

// settle 은 미뤄 둔 FK 를 지금 잰다(SET CONSTRAINTS ALL IMMEDIATE - 위반이면 이 문장이 실패한다).
func (pgSide) settle(ctx context.Context, tx *txConn) error {
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		return fmt.Errorf("미뤄 둔 FK 검사에 걸렸다: %w", err)
	}
	return nil
}

// next 는 시퀀스가 다음에 낼 번호다(held 는 마지막에 낸 번호). 행의 최고값은 보지 않는다 - identity 는 시퀀스로만 낸다.
func (pgSide) next(_ pg.Table, _, held int64) int64 { return held + 1 }

// soleClient 는 Postgres 원장 q 에 이 연결(pg_backend_pid) 말고 붙은 클라이언트가 없는지 잰다. 있으면 처방(서버를 내려라)과
// 남은 연결의 역할 · application_name 을 실은 거절이다. what 은 문구의 머리(원본 · 대상)다. 둘 다 종류가 가려진 연결에도
// 보인다 - fd 서버는 application_name 을 싣지 않으니 역할(앱 역할)로 알아본다.
//
// ★ 이 실행이 그 원장에 연 연결은 하나다. 원본은 스냅숏 하나(ReadTx), 대상은 실행기 세션의 연결 하나에서 옮기고, 그 앞의
// 질의(열기의 핑 · 연결 인자 되읽기)는 같은 연결을 풀에서 다시 꺼낸 것이다 - 질의를 겹쳐 열지 않는다.
// ★ 종류(backend_type)를 못 읽는 연결도 센다. pg_stat_activity 는 초사용자 · pg_read_all_stats · 그 연결의 역할(의 멤버)이
// 아니면 남의 연결의 종류를 NULL 로 가린다 - 서버(앱 역할)와 실행기(소유 역할의 멤버)는 역할이 달라 가려진다. 가려진 것
// 중 사용자가 있는 연결(usesysid)만 센다 - autovacuum 같은 서버 안 일꾼은 사용자가 없다.
func soleClient(ctx context.Context, q Reader, what string) error {
	var others []string
	err := eachRow(ctx, q, `SELECT pid, coalesce(usename::text, ''), coalesce(application_name, '') FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
		  AND (backend_type = 'client backend' OR (backend_type IS NULL AND usesysid IS NOT NULL))
		ORDER BY pid`, nil, what+"에 붙은 연결", func(r *rowSet) error {
		var pid int64
		var role, app string
		if err := r.Scan(&pid, &role, &app); err != nil {
			return err
		}
		if app == "" {
			app = "없음"
		}
		others = append(others, fmt.Sprintf("pid %d(역할 %s · application_name %s)", pid, clip(role, 64), clip(app, 64)))
		return nil
	})
	if err != nil {
		return err
	}
	if len(others) == 0 {
		return nil
	}
	n := len(others)
	if n > pg.MaxListed {
		others = append(others[:pg.MaxListed], fmt.Sprintf("외 %d 개", n-pg.MaxListed))
	}
	return fmt.Errorf("%s 원장에 이 실행 말고 붙은 연결이 %d 개 있다: %s - 그 원장을 쓰는 서버(fd serve)를 내리고 남은 연결을 "+
		"닫은 뒤 다시 돌려라. 붙은 채로 옮기면 원본은 스냅숏 뒤의 쓰기를 잃고(커밋 전 대조는 같은 스냅숏을 봐 맞다고 한다), 대상은 "+
		"옮기는 트랜잭션에 쓰기가 끼어든다", what, n, strings.Join(others, ", "))
}

// ─────────────────────────────────────────────────────────────────────────────
// 원본 훑기 - 사전 검사 · 행 표지 · 발급 번호
// ─────────────────────────────────────────────────────────────────────────────

// ledgerScan 은 원본 1차 훑기의 결과다.
type ledgerScan struct {
	cells cellTally
	rows  []TableRows
	marks map[string][]pg.RowMark
	// floors 는 identity 가 있는 표마다 원본에서 발급된 가장 큰 번호다(pg.Floor).
	floors map[string]int64
}

// cellTally 는 훑기 한 번이 칸 규칙(cellRule)에서 센 것이다: 걸린 칸(faults), 바꿔 넣은 칸(rewrites), 바꿔 넣은 칸이 하나라도
// 있는 행의 수(rewrittenRows). 원본의 사전 검사와 대조가 읽은 대상 값이 같은 셈을 쓴다.
type cellTally struct {
	faults        tally[pg.Fault]
	rewrites      tally[pg.Rewrite]
	rewrittenRows int
}

// row 는 표 t 의 행 하나에서 걸린 칸(faults)과 바꿔 넣은 칸(rewrites - 둘 다 칸 선언 순서)을 센다. key 는 그 행의 좌표다.
func (ct *cellTally) row(t pg.Table, faults []pg.Fault, rewrites []pg.Rewrite, key string) {
	rewritten := false
	for i, c := range t.Cols {
		if f := faults[i]; f != "" {
			ct.faults.add(t.Name, c.Name, f, key)
		}
		if r := rewrites[i]; r != "" {
			ct.rewrites.add(t.Name, c.Name, r, key)
			rewritten = true
		}
	}
	if rewritten {
		ct.rewrittenRows++
	}
}

// tally 는 칸을 갈래(표 · 칸 · 사유)마다 센다.
type tally[R ~string] struct {
	groups []CellGroup[R]
	at     map[tallyAt[R]]int // groups 안의 자리
}

type tallyAt[R ~string] struct {
	table, column string
	reason        R
}

func (tl *tally[R]) add(table, column string, r R, key string) {
	at := tallyAt[R]{table, column, r}
	i, ok := tl.at[at]
	if !ok {
		if tl.at == nil {
			tl.at = map[tallyAt[R]]int{}
		}
		i = len(tl.groups)
		tl.at[at] = i
		tl.groups = append(tl.groups, CellGroup[R]{Table: table, Column: column, Reason: r})
	}
	g := &tl.groups[i]
	g.Count++
	if len(g.Keys) < pg.MaxListed {
		g.Keys = append(g.Keys, key)
	}
}

// groupCount 는 갈래들에 든 칸의 수다.
func groupCount[R ~string](groups []CellGroup[R]) int {
	n := 0
	for _, g := range groups {
		n += g.Count
	}
	return n
}

func (sc *ledgerScan) report(rep *TransferReport) {
	rep.Faults, rep.Rewrites, rep.RewrittenRows, rep.Rows = sc.cells.faults.groups, sc.cells.rewrites.groups,
		sc.cells.rewrittenRows, sc.rows
}

// refusal 은 사전 검사에 걸린 것이 있으면 그 수와 다음 수를 담은 오류다.
//
// ★ 인덱스 키 상한(pg.FaultTooLong)에 걸린 행은 고치라고 하지 않는다. 그 칸은 추가 전용 감사 원장(event)의 것이라 행을 자르면
// 원본과 옮긴 원장이 달라진다 - 표 · 칸 · 기본키를 실어 이슈로 알려 달라고 한다.
func (sc *ledgerScan) refusal() error {
	n := groupCount(sc.cells.faults.groups)
	if n == 0 {
		return nil
	}
	tooLong, other := 0, 0
	for _, g := range sc.cells.faults.groups {
		if g.Reason == pg.FaultTooLong {
			tooLong += g.Count
		} else {
			other += g.Count
		}
	}
	var next []string
	if other > 0 {
		next = append(next, "원본 사본에서 걸린 행을 고친 뒤 다시 돌려라")
	}
	if tooLong > 0 {
		next = append(next, fmt.Sprintf("인덱스 키 상한을 넘는 %d 건은 행을 고치지 말고(추가 전용 감사 원장이다 - 자르면 원본과 "+
			"달라진다) 위 표 · 칸 · 행 좌표를 실어 이슈로 알려 달라", tooLong))
	}
	return fmt.Errorf("사전 검사에 %d 건(%d 갈래)이 걸렸다 - 아무것도 쓰지 않는다. %s", n, len(sc.cells.faults.groups),
		strings.Join(next, ". "))
}

// scanLedger 는 원본을 한 번 훑는다: 판 · 모양과 붙은 다른 연결을 재고, 표마다 행을 읽어 칸마다 규칙 cell(사전 검사 · 바꿔
// 넣기)을 걸고, 행 표지와 발급 번호를 모은다. 끝에 부모 없는 행을 잰다. q 는 원본의 읽기 트랜잭션, from 은 원본의 몫이다. cell 은
// 옮겨 넣을 쪽이 원본 값에 거는 규칙이다(ledgerSide.intake - 이관은 NUL 을 바꿔 넣고 인덱스 키 상한까지 잰다). 대조는 pg.Cell
// 이다 - 이미 두 원장에 있는 원문을 맞댄다.
func scanLedger(ctx context.Context, q Reader, from ledgerSide, cell cellRule) (*ledgerScan, error) {
	if err := from.shape(ctx, q); err != nil {
		return nil, err
	}
	if err := from.alone(ctx, q, "원본"); err != nil {
		return nil, err
	}
	held, err := from.held(ctx, q)
	if err != nil {
		return nil, err
	}
	sc := &ledgerScan{marks: map[string][]pg.RowMark{}, floors: map[string]int64{}}
	for _, t := range pg.Tables() {
		marks, maxID, err := scanTable(ctx, q, t, from.sqlite(), cell, &sc.cells)
		if err != nil {
			return nil, err
		}
		if _, ok := t.Identity(); ok {
			sc.floors[t.Name] = pg.Floor(t, maxID, held[t.Name])
		}
		sc.marks[t.Name] = marks
		sc.rows = append(sc.rows, TableRows{Table: t.Name, Rows: len(marks)})
	}
	if err := from.orphans(ctx, q, &sc.cells.faults); err != nil {
		return nil, err
	}
	return sc, nil
}

// scanTable 은 표 t 를 읽어 행 표지(칸 선언 순서 · cell 을 지난 값)와 identity 칸의 최고값을 내고, 칸 규칙에 걸리거나 바뀐
// 칸과 지문 기본키가 겹치는 행을 ct 에 센다. 원본 훑기(받는 쪽의 ledgerSide.intake, 대조는 pg.Cell)와 대상 대조(pg.SettledCell)가
// 이 하나로 읽는다 - 대조의 두 쪽이 같은 꼴을 잰다. 원본 쪽 값은 바꿔 넣은 값이라 행 표지 · 지문 기본키도 옮긴 값으로 잰다.
func scanTable(ctx context.Context, q Reader, t pg.Table, sqlite bool, cell cellRule, ct *cellTally) (marks []pg.RowMark, maxID int64, err error) {
	id, hasID := t.Identity()
	idAt := t.Index(id.Name)
	pks := map[string]bool{} // 지문 기본키(pg.Table.DigestPK) - 지문 칸이 있는 표만 찬다
	err = eachCells(ctx, q, t, sqlite, cell, func(vals []any, faults []pg.Fault, rewrites []pg.Rewrite) error {
		m := t.Mark(vals)
		ct.row(t, faults, rewrites, m.Key)
		if pk, ok := t.DigestPK(vals); ok {
			if pks[pk] {
				ct.faults.add(t.Name, t.DigestKey, pg.FaultDigest, m.Key)
			}
			pks[pk] = true
		}
		if hasID {
			if n, ok := vals[idAt].(int64); ok {
				maxID = max(maxID, n)
			}
		}
		marks = append(marks, m)
		return nil
	})
	return marks, maxID, err
}

// checkShape 는 원본(엔진 eng)의 표 · 칸이 이관 카탈로그(pg.Tables)와 같은지 잰다. 이관 판 범위면 같아야 한다 - 다르면 손으로
// 고친 원장이고, 모르는 칸을 버리거나 없는 칸을 지어내지 않으려고 거절한다. 카탈로그를 읽는 질의는 엔진마다 하나(tablesQuery ·
// columnsQuery)이고 맞대는 규칙은 한 벌이다.
func checkShape(ctx context.Context, q Reader, eng engine) error {
	names, err := strings1(ctx, q, tablesQuery(eng))
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
		cols, err := strings1(ctx, q, columnsQuery(eng), t.Name)
		if err != nil {
			return fmt.Errorf("원본 %s 의 칸을 읽지 못했다: %w", t.Name, err)
		}
		switch exp := t.DeclaredColumns(eng == engineSQLite); {
		case len(cols) == 0:
			bad = append(bad, "없는 표 "+t.Name)
		case !slices.Equal(cols, exp):
			bad = append(bad, fmt.Sprintf("%s 의 칸 %v(기대 %v)", t.Name, cols, exp))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("원본의 모양이 판 %s 과 다르다(%s) - 손으로 고친 원장은 옮기지 않는다",
			transferRange(), strings.Join(bad, "; "))
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

// foreignKeyFaults 는 SQLite 원본에서 부모가 없는 행을 사전 검사에 싣는다(PRAGMA foreign_key_check). 기준선은 커밋 때 그
// 행을 거절한다. 좌표는 rowid 다(그 표를 `WHERE rowid = N` 으로 찾는다).
func foreignKeyFaults(ctx context.Context, q Reader, ft *tally[pg.Fault]) error {
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
		ft.add(m.table, strings.Join(cols, ","), pg.FaultForeignKey, key)
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

// cellRule 은 읽은 칸 하나를 기준선 값으로 옮기고 거절 사유나 바꿔 넣은 갈래를 내는 규칙이다: 이관의 SQLite 원본은 pg.Cell
// (Postgres 로 옮겨 넣을 때는 키 상한까지 재는 pg.ImportCell - ledgerSide.intake), 이미 기준선 꼴이어야 하는 쪽(옮긴 대상,
// 역이관의 Postgres 원본)은 pg.SettledCell.
type cellRule func(pg.Column, any) (any, pg.Fault, pg.Rewrite)

// eachCells 는 표 t 의 행을 차례로 읽어 칸마다 기준선 값으로 옮긴 것(cell)과 그 거절 사유 · 바꿔 넣은 갈래를 fn 에 준다.
// sqlite 면 rowid 자리 칸을 rowid 로 읽는다. 두 원장이 이 하나로 읽는다 - 대조의 두 쪽이 같은 꼴을 잰다.
//
// ★ vals 는 행마다 새로 만든다. fn 이 그것을 붙들어도(묶음 INSERT) 다음 행이 덮지 않는다.
func eachCells(ctx context.Context, q Reader, t pg.Table, sqlite bool, cell cellRule,
	fn func(vals []any, faults []pg.Fault, rewrites []pg.Rewrite) error) error {
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
		rewrites := make([]pg.Rewrite, len(t.Cols))
		for i, c := range t.Cols {
			vals[i], faults[i], rewrites[i] = cell(c, raw[i])
		}
		return fn(vals, faults, rewrites)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 옮기기 · 순번 · 대조
// ─────────────────────────────────────────────────────────────────────────────

// 묶음 INSERT 의 상한. 행 수와 값 바이트 중 먼저 닿는 쪽에서 끊는다. 자리표시 수는 두 엔진 중 작은 상한(SQLite 의
// SQLITE_MAX_VARIABLE_NUMBER 32766, Postgres 65535) 아래로 맞춘다.
const (
	batchRows    = 500
	batchBytes   = 8 << 20
	maxPlaceArgs = 32766
)

// copyTables 는 원본 q(from)의 표 전부를 대상 트랜잭션 tx(to)에 FK 순서로 넣고 넣은 행 수를 낸다. 값은 받는 쪽의 규칙
// (to.intake - 1차 훑기와 같은 규칙)을 지난 값이다 - 원본 그대로이고, SQLite 원본의 NUL 만 정해진 규칙으로 바뀐다(pg.Rewrite).
// identity 칸도 옛 번호 그대로다(Postgres 대상이면 pg.Table.InsertSQL 이 그 구절을 붙이고, SQLite 대상이면 item_after.seq 가
// rowid 로 돌아간다). 이관과 역이관이 이 하나로 옮긴다.
func copyTables(ctx context.Context, q Reader, from ledgerSide, tx *txConn, to ledgerSide) (int, error) {
	total := 0
	for _, t := range pg.Tables() {
		limit := min(batchRows, maxPlaceArgs/len(t.Cols))
		var args []any
		rows, size := 0, 0
		flush := func() error {
			if rows == 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, t.InsertSQL(rows, to.sqlite()), args...); err != nil {
				return fmt.Errorf("대상에 넣기 실패: %w", err)
			}
			total += rows
			args, rows, size = args[:0], 0, 0
			return nil
		}
		err := eachCells(ctx, q, t, from.sqlite(), to.intake(), func(vals []any, _ []pg.Fault, _ []pg.Rewrite) error {
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

// checkTarget 은 대상 r(to)을 원본 훑기 sc 와 맞대 rep 에 싣는다: 표마다 행 지문의 다중집합(pg.Compare), identity 마다 다음
// 번호(to.next), 그리고 읽은 대상 값이 값 검사(pg.SettledCell - 바꿔 넣을 값이 없고 저장 갈래까지 칸 타입이어야 한다)에 걸린
// 것(rep.TargetFaults). 원본 쪽 표지는 원본의 규칙을 지난 값이라, NUL 을 바꿔 넣은 행도 옮긴 값끼리 맞댄다. 판정은
// rep.verdict 다. 이관 · 역이관의 커밋 전 대조와 `fd migrate verify` 가 이 하나를 쓴다.
func checkTarget(ctx context.Context, r Reader, to ledgerSide, sc *ledgerScan, rep *TransferReport) error {
	held, err := to.held(ctx, r)
	if err != nil {
		return err
	}
	var tables []pg.TableCheck
	var seqs []pg.SequenceCheck
	var cells cellTally
	for _, t := range pg.Tables() {
		marks, maxID, err := scanTable(ctx, r, t, to.sqlite(), pg.SettledCell, &cells)
		if err != nil {
			return fmt.Errorf("대상 %w", err)
		}
		tables = append(tables, pg.Compare(t.Name, sc.marks[t.Name], marks))
		if c, ok := t.Identity(); ok {
			seqs = append(seqs, pg.SequenceCheck{Table: t.Name, Column: c.Name, Floor: sc.floors[t.Name],
				Next: to.next(t, maxID, held[t.Name])})
		}
	}
	rep.Tables, rep.Sequences, rep.TargetFaults = tables, seqs, cells.faults.groups
	return nil
}

// identitySeq 는 대상 identity 칸 하나의 시퀀스다.
type identitySeq struct {
	// name 은 카탈로그가 준 시퀀스 이름이다(pg_get_serial_sequence - 필요하면 따옴표가 붙어 있어 문장에 그대로 넣는다).
	name string
	// last · called 는 시퀀스의 last_value · is_called 다. called 가 거짓이면 아직 번호를 낸 적이 없다(다음이 last 다).
	last   int64
	called bool
	// start 는 시퀀스의 시작값(pg_sequence.seqstart)이다. RESTART 는 이것을 안 바꾼다 - 새 기준선은 last = start 다.
	start int64
}

// readIdentity 는 대상 표 table 의 identity 칸 col 의 시퀀스를 읽는다. 시퀀스를 읽는 권한(SELECT)이 있어야 한다 - 소유
// 역할에는 있고 앱 역할에는 없다.
func readIdentity(ctx context.Context, r Reader, table, col string) (identitySeq, error) {
	var name sql.NullString
	if err := r.QueryRowContext(ctx, `SELECT pg_get_serial_sequence(?, ?)`, table, col).Scan(&name); err != nil {
		return identitySeq{}, fmt.Errorf("%s.%s 의 시퀀스를 찾지 못했다: %w", table, col, err)
	}
	if !name.Valid {
		return identitySeq{}, fmt.Errorf("%s.%s 에 identity 시퀀스가 없다", table, col)
	}
	seq := identitySeq{name: name.String}
	if err := r.QueryRowContext(ctx, `SELECT last_value, is_called FROM `+seq.name).Scan(&seq.last, &seq.called); err != nil {
		return identitySeq{}, fmt.Errorf("%s.%s 의 순번을 읽지 못했다(시퀀스를 읽는 권한은 소유 역할에 있다): %w", table, col, err)
	}
	if err := r.QueryRowContext(ctx, `SELECT seqstart FROM pg_sequence WHERE seqrelid = ?::regclass`, seq.name).Scan(&seq.start); err != nil {
		return identitySeq{}, fmt.Errorf("%s.%s 의 순번 시작값을 읽지 못했다: %w", table, col, err)
	}
	return seq, nil
}
