package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// Postgres 원장을 같은 판(pg.ImportVersion)의 새 SQLite 파일로 옮긴다(`fd migrate export-sqlite`) - 이관의 역이다. 이관 뒤
// 되돌리기 창 안에 Postgres 에 쌓인 항목 · 선점 · 이벤트 · 판단을 들고 SQLite 판으로 돌아가는 길이다. 옮기기 · 대조 · 판정은
// 이관과 같은 길(transferInto)이고, 여기는 새 파일을 만들고 이름을 붙이는 것뿐이다.
//
// 역이관 한 번의 길:
//
//	대상 자리 재기(파일 · 옆 파일이 없어야) → 원본 열기(Postgres) → 대상 디렉토리에 임시 파일(0644) · 이관 판 스키마
//	→ transferInto(원본 스냅숏 하나에서 이관 판 · 모양 · 붙은 다른 연결을 재고 훑기 · 대상 트랜잭션 하나 · 커밋 전 대조)
//	→ 임시 파일 닫기(옆 파일의 커밋이 본 파일로 옮았나) → 그 자리의 옛 판올림 전 사본 치우기(quarantineBackups, 본 실행만)
//	→ 새 이름 붙이기 · 디렉토리 fsync(예행은 일회용 이름에 붙였다 지운다)
//
// ★ 커밋 전 대조가 맞은 임시 파일만 이름을 얻는다. 어긋나거나 실패하거나 예행이면 임시 파일과 그 옆 파일을 지운다 - 대상
// 자리에는 아무것도 남지 않는다. 신호(SIGINT · SIGTERM)도 ctx 취소로 받아 같은 길로 지운다 - SIGKILL · 전원 차단만
// 「.<이름>.export-*」 를 남긴다(README 의 「SQLite 판으로 돌아가기」).
// ★ 이름은 하드 링크로 붙인다(os.Link). rename 은 그 사이에 생긴 같은 이름의 파일을 말없이 덮는다 - 링크는 이미 있으면
// 실패한다. 임시 파일이 대상과 같은 디렉토리에 있어 같은 파일 시스템이다. 예행도 같은 디렉토리에서 링크를 붙여 본다 - 링크를
// 안 받는 파일 시스템이면 예행이 먼저 거절한다.
// ★ 원본은 읽기만 한다(ReadTx - REPEATABLE READ READ ONLY 스냅숏 하나). 서버를 내린 원본이 전제고 그것을 잰다 - 이 실행
// 말고 붙은 연결이 있으면 거절한다(soleClient). 잰 뒤에 붙은 서버가 쓴 것은 스냅숏에 없어 옮기지 않고 커밋 전 대조도 못
// 본다 - 그래서 역이관 뒤의 `fd migrate verify`(새 스냅숏으로 다시 맞댄다)가 절차의 한 단계다.

// ExportSQLite 는 Postgres 원장 from 을 새 SQLite 파일 to 로 옮긴다(opt.DryRun 이면 같은 길을 임시 파일에 돌고 지운다).
func ExportSQLite(ctx context.Context, from, to string, opt TransferOptions, log *slog.Logger) (TransferReport, error) {
	if log == nil {
		log = slog.Default()
	}
	ft := ParseTarget(from)
	rep := TransferReport{From: ft.Shown(), To: ParseTarget(to).Shown(), DryRun: opt.DryRun}
	if err := exportTargetRefusal(to); err != nil {
		return rep, err
	}
	src, err := openExportSource(ft, log)
	if err != nil {
		return rep, err
	}
	defer src.Close()
	tmp, err := newExportFile(ctx, to, log)
	if err != nil {
		return rep, err
	}
	// 이름을 붙인 뒤에도 지운다 - 그때 tmp 는 같은 파일의 두 번째 이름일 뿐이다.
	defer removeLedgerFiles(tmp, log)
	rep.TargetChecked = true
	start := time.Now()
	if err := exportInto(ctx, src, tmp, opt.DryRun, &rep, log); err != nil {
		rep.Copied = 0
		return rep, err
	}
	// 취소(신호)가 왔으면 이름을 붙이지 않는다 - 임시 DB 를 닫는 사이에 온 취소는 exportInto 가 못 볼 수 있다.
	if err := ctx.Err(); err != nil {
		rep.Copied = 0
		return rep, fmt.Errorf("역이관이 취소됐다 - 파일을 내지 않는다: %w", err)
	}
	if !opt.DryRun {
		// 이름을 붙이기 전에 그 자리의 옛 판올림 전 사본을 치운다(quarantineBackups) - 새 원장이 이름을 얻는 순간 남의 원장의
		// 사본이 `fd migrate --rollback` 의 후보로 그 옆에 있지 않게 한다. 못 치우면 이름을 붙이지 않는다.
		if rep.Quarantined, err = quarantineBackups(to, "export", time.Now()); err != nil {
			rep.Copied = 0
			return rep, fmt.Errorf("새 원장을 내지 않았다 - %s 옆의 옛 판올림 전 사본(.bak-*)을 치우지 못했다: %w", clip(to, 200), err)
		}
	}
	if err := nameExport(tmp, to, opt.DryRun); err != nil {
		rep.Copied = 0
		return rep, err
	}
	if opt.DryRun {
		return rep, nil
	}
	log.Info("SQLite 역이관 완료", "from", rep.From, "to", rep.To, "rows", rep.Copied,
		"duration", time.Since(start).Seconds())
	return rep, nil
}

// exportTargetRefusal 은 역이관의 대상 자리 to 가 새 원장을 받을 수 없는 사유다. 받으면 nil 이다.
//
// ★ 파일만이 아니라 옆 파일(pg.Sidecars)도 없어야 한다. 남은 -wal 은 새 파일을 처음 여는 연결이 그 파일의 것으로 알고
// 되살린다 - 옮긴 원장 위에 옛 원장의 쪽들이 덮인다.
func exportTargetRefusal(to string) error {
	if strings.TrimSpace(to) == "" {
		return errors.New("역이관 대상(--to)이 비었다 - 새로 만들 SQLite 원장 파일 경로를 준다")
	}
	if err := sqliteOnly(to, "역이관 대상(--to)"); err != nil {
		return err
	}
	for _, p := range append([]string{to}, pg.Sidecars(to)...) {
		_, err := os.Lstat(p)
		switch {
		case err == nil:
			return fmt.Errorf("%s 가 이미 있다 - 역이관은 새 파일에만 쓴다(있는 원장을 덮지 않고, 옆에 남은 -wal 을 새 원장에 "+
				"되살리지 않는다). 다른 이름을 주거나 그 파일을 옮겨 둔 뒤 다시 돌려라", clip(p, 200))
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("역이관 대상 자리를 재지 못했다(path=%q): %w", clip(p, 200), err)
		}
	}
	return nil
}

// openExportSource 는 역이관의 원본(Postgres 원장)을 연다. 판 · 모양은 여기서 재지 않는다 - 원본은 살아 있는 원장이라 옮기는
// 스냅숏 안에서 잰다(pgSide.shape).
func openExportSource(t Target, log *slog.Logger) (*Store, error) {
	if !t.Postgres() {
		return nil, fmt.Errorf("역이관 원본은 Postgres 접속 URL 이어야 한다(--db 또는 FD_DB, 지금 %s)", clip(t.Shown(), 200))
	}
	return openPG(t, log, lockTimeout)
}

// exportSourceRefusal 은 원본의 판 plan(transferPlan)을 역이관이 받지 않는 사유다. 받으면 nil 이다. 순수 함수다. 판이 이관 판
// 범위여야 한다 - SQLite 판에는 그 뒤 판의 자리가 없다. 범위 안 어느 판이든 낸 파일은 이관 판(pg.ImportVersion)이다.
func exportSourceRefusal(plan MigrationPlan) error {
	switch plan.Action {
	case MigrateNone:
		return nil
	case MigrateApply:
		return fmt.Errorf("원본에 flightdeck 원장이 없다(빈 DB 다) - 옮길 것이 없다: %s", plan.Reason)
	}
	return fmt.Errorf("원본이 판 %s 이 아니다(%s) - 역이관은 SQLite 판과 표 · 칸이 같은 판(%s)의 원장만 옮긴다",
		transferRange(), plan.Reason, transferRange())
}

// newExportFile 은 to 와 같은 디렉토리에 임시 SQLite 파일을 만들어 판 pg.ImportVersion 스키마를 깐다. 실패하면 남기지 않는다.
//
// ★ 모드는 ledgerFileMode(0600)를 만들 때 주고 umask 는 더 좁힐 수만 있다(createExclusive). 만든 뒤 Chmod 하지 않는다 -
// umask(예: 077)를 무시하고 넓히게 된다. 스키마를 깔기 전에 정해진다 - SQLite 는 옆 파일(-wal · -shm)을 본 파일의 모드로 만든다.
func newExportFile(ctx context.Context, to string, log *slog.Logger) (string, error) {
	f, err := createExclusive(filepath.Dir(to), "."+filepath.Base(to)+".export-", ledgerFileMode)
	if err != nil {
		return "", fmt.Errorf("역이관 임시 파일을 만들지 못했다(대상 디렉토리가 있고 쓸 수 있어야 한다): %w", err)
	}
	tmp := f.Name()
	if err := f.Close(); err != nil {
		removeLedgerFiles(tmp, log)
		return "", fmt.Errorf("역이관 임시 파일을 닫지 못했다: %w", err)
	}
	if err := MigrateTo(ctx, tmp, pg.ImportVersion, log); err != nil {
		removeLedgerFiles(tmp, log)
		return "", fmt.Errorf("역이관 임시 파일에 판 %d 스키마를 깔지 못했다: %w", pg.ImportVersion, err)
	}
	return tmp, nil
}

// ledgerFileMode 는 이 패키지가 내는 원장 파일(역이관이 낸 원장 · 판올림 전 사본 · 바꾼 칸 기록)을 만들 때 주는 모드다. 주인만
// 읽고 쓴다(0600): 판 20 원장은 암호 지문(account.password_hash)과 기기 토큰 지문(machine.token_hash)을 담고, 바꾼 칸 기록은
// 원본 바이트를 담는다. umask 022 에 0644 로 만들면 같은 기계의 누구나 읽는다. 그 파일을 여는 서버 · `fd migrate` 는 그것을 낸
// 것과 같은 uid(compose 의 FD_UID)로 돈다 - 다른 uid 의 서버는 0644 여도 원장에 못 써서 어차피 못 연다.
const ledgerFileMode fs.FileMode = 0o600

// createExclusive 는 dir 안에 prefix 로 시작하는 겹치지 않는 이름의 새 파일을 mode 로 만든다(O_EXCL - 있는 파일은 안 연다).
// os.CreateTemp 와 같지만 모드를 받는다 - 프로세스 umask 가 그 모드를 거른다.
func createExclusive(dir, prefix string, mode fs.FileMode) (*os.File, error) {
	var b [8]byte
	for range 10000 {
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(dir, prefix+hex.EncodeToString(b[:])), os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, fmt.Errorf("%s 에 겹치지 않는 임시 이름을 못 찾았다", dir)
}

// exportInto 는 임시 파일 tmp 를 열어 src 를 옮기고(transferInto) 닫는다. 본 실행이면 닫은 뒤 커밋이 본 파일에 다 옮았는지
// 잰다(exportSettled).
func exportInto(ctx context.Context, src *Store, tmp string, dryRun bool, rep *TransferReport, log *slog.Logger) error {
	tgt, err := openRaw(tmp, log)
	if err != nil {
		return err
	}
	err = tgt.transferInto(ctx, src, "SQLite 역이관", dryRun, nil, rep)
	if cerr := tgt.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("역이관 임시 파일을 닫지 못했다: %w", cerr)
	}
	if err != nil || dryRun {
		return err
	}
	return exportSettled(tmp)
}

// exportSettled 는 닫은 임시 파일 tmp 의 커밋이 본 파일에 다 옮았는지 잰다 - 이름은 본 파일에만 붙고 옆 파일(-wal)에 남은
// 커밋은 따라가지 않는다. 옆 파일을 재지 못한 것(없음 말고 - 권한 · 고리 링크 · 입출력)도 거절이다. 남은 것이 없다고 보지
// 않는다.
func exportSettled(tmp string) error {
	for _, p := range pg.Sidecars(tmp) {
		n, err := fileBytes(p)
		switch {
		case err != nil:
			return fmt.Errorf("역이관 임시 파일 옆 %s 를 재지 못했다 - 커밋이 본 파일에 다 옮았는지 모른다. 새 원장을 내지 "+
				"않는다: %w", filepath.Base(p), err)
		case n > 0:
			return fmt.Errorf("역이관 임시 파일을 닫았는데 %s 에 %d 바이트가 남았다 - 커밋이 본 파일에 다 옮지 않았다. 새 원장을 "+
				"내지 않는다", filepath.Base(p), n)
		}
	}
	return nil
}

// nameExport 는 대조를 지난 임시 파일 tmp 에 이름을 붙인다. 본 실행은 to 에 붙이고(publishExport), 예행은 같은 디렉토리의
// 일회용 이름에 붙였다가 바로 지운다 - 본 실행과 같은 호출(os.Link)로 그 디렉토리가 하드 링크를 받는지 잰다.
//
// ★ 일회용 이름은 tmp 에 꼬리를 붙인 것이다. tmp 가 os.CreateTemp 의 유일한 이름이라 남과 겹치지 않고, 강제 종료로 남아도
// 「.<이름>.export-*」 꼴이라 임시 파일과 함께 알아보고 지운다.
func nameExport(tmp, to string, dryRun bool) error {
	if !dryRun {
		return publishExport(tmp, to)
	}
	probe := tmp + ".link"
	if err := os.Link(tmp, probe); err != nil {
		return fmt.Errorf("대상 디렉토리에서 하드 링크를 붙이지 못했다 - 본 실행은 옮긴 원장에 이 길로 이름을 붙인다(대상 "+
			"디렉토리가 하드 링크를 받는 파일 시스템이어야 한다): %w", err)
	}
	if err := os.Remove(probe); err != nil {
		return fmt.Errorf("예행이 붙여 본 링크를 지우지 못했다(path=%q) - 손으로 지운다: %w", clip(probe, 200), err)
	}
	return nil
}

// publishExport 는 대조를 지난 임시 파일 tmp 에 이름 to 를 붙이고 그 디렉토리를 디스크에 굳힌다(syncDir). 그 사이에 대상
// 자리에 무엇이 생겼으면 거절한다.
//
// ★ 굳히기까지 지나야 낸 것이다. 링크는 디렉토리 항목 하나라 디렉토리를 fsync 하기 전에는 전원이 나가면 이름째 사라질 수
// 있다 - 「옮겼다」 고 말한 뒤 원장이 없어진다. 굳히지 못하면 붙인 이름을 지우고 실패로 낸다.
// ★ 파일 내용은 닫을 때 굳었다. SQLite 는 마지막 연결을 닫으며 -wal 을 본 파일로 옮기고 본 파일을 fsync 한다(exportSettled 가
// 옆 파일이 비었는지 잰다).
func publishExport(tmp, to string) error {
	if err := exportTargetRefusal(to); err != nil {
		return err
	}
	if err := os.Link(tmp, to); err != nil {
		return fmt.Errorf("옮긴 원장에 이름을 붙이지 못했다(path=%q) - 새 원장을 내지 않았다: %w", clip(to, 200), err)
	}
	if err := syncDir(filepath.Dir(to)); err != nil {
		err = fmt.Errorf("옮긴 원장에 붙인 이름을 디스크에 굳히지 못했다(디렉토리 fsync, path=%q) - 새 원장을 내지 않았다: %w",
			clip(to, 200), err)
		if rerr := os.Remove(to); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = fmt.Errorf("%w (붙인 이름도 지우지 못했다 - 손으로 지운다: %v)", err, rerr)
		}
		return err
	}
	return nil
}

// removeLedgerFiles 는 SQLite 원장 파일 path 와 그 옆 파일을 지운다. 없는 것은 넘어가고, 못 지운 것은 로그에 남긴다.
func removeLedgerFiles(path string, log *slog.Logger) {
	for _, p := range append([]string{path}, pg.Sidecars(path)...) {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warn("역이관 임시 파일을 지우지 못했다", "path", p, "error", err.Error())
		}
	}
}
