package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/kweiza/flightdeck/internal/store"
)

// `fd migrate export-sqlite` - Postgres 원장을 같은 판의 새 SQLite 파일로 옮긴다(이관의 역 - 되돌리기 창 안에서 SQLite 판으로
// 돌아가는 길). 판정 · 옮기기 · 대조는 store.ExportSQLite 이고, 보고는 이관과 같은 꼴(printTransferReport)이다.
//
// 종료 코드: 0 옮겼다(대조가 맞았다) · 1 거절 · 어긋남 · 실패 · 2 사용법.
//
// ★ SIGINT · SIGTERM 은 ctx 취소로 받는다(serve 와 같은 signal.NotifyContext). 그대로 죽으면 대상 디렉토리에 임시 파일
// 「.<이름>.export-*」 이 남는다 - 취소를 받으면 store.ExportSQLite 가 옮기기를 멈추고 그 파일을 지운 뒤 1 로 끝난다.

// runExportSQLite 는 `fd migrate export-sqlite` 다. 원본의 기본값은 판올림 · 이관과 같은 규칙(resolveDBPath)이다.
func (a *App) runExportSQLite(ctx context.Context, args []string, out io.Writer) int {
	fs := newFlagSet("migrate export-sqlite")
	to := fs.String("to", "", "새로 만들 SQLite 원장 파일(이미 있거나 옆에 -wal · -shm · -journal 이 있으면 거절한다)")
	from := fs.String("db", "", "옮길 Postgres 원장 접속 URL(비면 "+dbEnv+")")
	dry := fs.Bool("dry-run", false, "본 실행과 같은 길을 임시 파일에 돌고(이름 붙이기는 일회용 하드 링크로 재 본다) 지운다(아무 파일도 남기지 않는다)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || strings.TrimSpace(*to) == "" {
		fmt.Fprintln(fs.Output(), "쓰는 법: fd migrate export-sqlite --to <새 fd.db> [--db <postgres URL>] [--dry-run]")
		return 2
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, err := store.ExportSQLite(ctx, resolveDBPath(a.env, *from), *to, store.TransferOptions{DryRun: *dry}, a.log)
	head := "fd migrate export-sqlite"
	if *dry {
		head += " --dry-run"
	}
	printTransferReport(out, head, rep)
	switch {
	case err != nil:
		a.log.Error("SQLite 역이관 실패", "from", rep.From, "to", rep.To, "dry_run", *dry, "error", err.Error())
		fmt.Fprintf(out, "\n  거절 · 실패: %s\n", err)
		return 1
	case *dry:
		fmt.Fprintln(out, "\n  옮길 수 있다 - 본 실행과 같은 트랜잭션을 임시 파일에 끝까지 돌고 되감았고, 대상 디렉토리에 하드 링크를 붙였다 지운 뒤 그 파일을 지웠다(아무 파일도 남기지 않았다)")
	default:
		fmt.Fprintf(out, "\n  옮겼다: %d 행, 대조가 맞았다 → %s\n  다음: 서버를 띄우기 전에 `fd migrate verify --from %s`(--db 는 이번과 같게)로 원본과 새 스냅숏에서 다시 맞댄다\n", rep.Copied, rep.To, rep.To)
	}
	return 0
}
