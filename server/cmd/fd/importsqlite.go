package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/kweiza/flightdeck/internal/store"
)

// `fd migrate import-sqlite` · `fd migrate verify` - SQLite 원장을 빈 Postgres 원장으로 옮기고 두 원장을 맞댄다.
//
// ★ 판정 · 옮기기 · 대조는 전부 store(ImportSQLite · VerifySQLite)와 그 순수 규칙(store/pg 의 import.go · verify.go)이
// 진다. 이 파일은 플래그를 읽고 결과를 사람에게 말하는 것뿐이다 - runMigrate 와 같은 규율이다.
//
// 종료 코드: 0 옮겼다(대조가 맞았다) · 1 거절 · 어긋남 · 실패 · 2 사용법.

// migrateSubcommands 는 `fd migrate` 아래 하위 동사다. 첫 인자가 이 이름이면 판올림 대신 그 동사를 돈다.
var migrateSubcommands = map[string]func(a *App, ctx context.Context, args []string, out io.Writer) int{
	"import-sqlite": (*App).runImportSQLite,
	"verify":        (*App).runVerifySQLite,
}

// importFlags 는 두 동사가 같이 받는 플래그다. 대상의 기본값은 판올림과 같은 규칙(resolveDBPath)이다.
type importFlags struct {
	from, to string
	dryRun   bool
}

func (a *App) parseImportFlags(name string, args []string) (importFlags, bool) {
	fs := newFlagSet("migrate " + name)
	from := fs.String("from", "", "옮길 SQLite 원장 파일(서버를 내린 뒤 뜬 사본)")
	to := fs.String("db", "", "대상 Postgres 접속 URL(비면 "+dbEnv+")")
	dry := fs.Bool("dry-run", false, "아무것도 쓰지 않고 잰다")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || strings.TrimSpace(*from) == "" {
		fmt.Fprintf(fs.Output(), "쓰는 법: fd migrate %s --from <fd.db> [--db <postgres URL>] [--dry-run]\n", name)
		return importFlags{}, false
	}
	return importFlags{from: *from, to: resolveDBPath(a.env, *to), dryRun: *dry}, true
}

// runImportSQLite 는 `fd migrate import-sqlite` 다.
func (a *App) runImportSQLite(ctx context.Context, args []string, out io.Writer) int {
	f, ok := a.parseImportFlags("import-sqlite", args)
	if !ok {
		return 2
	}
	rep, err := store.ImportSQLite(ctx, f.from, f.to, store.ImportOptions{DryRun: f.dryRun}, a.log)
	head := "fd migrate import-sqlite"
	if f.dryRun {
		head += " --dry-run"
	}
	printImportReport(out, head, rep)
	switch {
	case err != nil:
		a.log.Error("SQLite 이관 실패", "from", rep.From, "to", rep.To, "dry_run", f.dryRun, "error", err.Error())
		fmt.Fprintf(out, "\n  거절 · 실패: %s\n", err)
		return 1
	case f.dryRun && !rep.TargetChecked:
		fmt.Fprintf(out, "\n  원본은 옮길 수 있다. 대상은 재지 않았다(%s 는 Postgres 접속 URL 이 아니다)\n", rep.To)
	case f.dryRun:
		fmt.Fprintln(out, "\n  옮길 수 있다 - 아무것도 쓰지 않았다")
	default:
		fmt.Fprintf(out, "\n  옮겼다: %d 행, 대조가 맞았다\n", rep.Copied)
	}
	return 0
}

// runVerifySQLite 는 `fd migrate verify` 다. 쓰지 않으므로 --dry-run 을 받아도 하는 일이 같다(두 동사의 플래그가 한 벌이다).
func (a *App) runVerifySQLite(ctx context.Context, args []string, out io.Writer) int {
	f, ok := a.parseImportFlags("verify", args)
	if !ok {
		return 2
	}
	rep, err := store.VerifySQLite(ctx, f.from, f.to, a.log)
	printImportReport(out, "fd migrate verify", rep)
	if err != nil {
		a.log.Error("SQLite 대조 실패", "from", rep.From, "to", rep.To, "error", err.Error())
		fmt.Fprintf(out, "\n  어긋남 · 실패: %s\n", err)
		return 1
	}
	fmt.Fprintln(out, "\n  두 원장이 맞다")
	return 0
}

// printImportReport 는 이관 · 대조 보고를 사람 말로 낸다. 접속 URL 원문은 보고에 없다(표시값뿐이다).
func printImportReport(out io.Writer, head string, rep store.ImportReport) {
	fmt.Fprintf(out, "%s · %s → %s\n", head, rep.From, rep.To)
	if len(rep.Faults) > 0 {
		fmt.Fprintln(out, "\n  사전 검사에 걸린 행(표 · 칸 · 사유 · 수 · 행 좌표):")
		for _, g := range rep.Faults {
			fmt.Fprintf(out, "    %s.%s  %s  %d건\n", g.Table, g.Column, g.Fault, g.Count)
			for _, k := range g.Keys {
				fmt.Fprintf(out, "      %s\n", k)
			}
			if more := g.Count - len(g.Keys); more > 0 {
				fmt.Fprintf(out, "      … 그 밖 %d건\n", more)
			}
		}
	}
	if len(rep.Tables) == 0 {
		if len(rep.Rows) > 0 {
			fmt.Fprintln(out, "\n  원본 행 수:")
			for _, r := range rep.Rows {
				fmt.Fprintf(out, "    %-24s %d\n", r.Table, r.Rows)
			}
		}
		return
	}
	fmt.Fprintln(out, "\n  대조표(표 · 원본 행 · 대상 행 · 전 칸 지문):")
	for _, c := range rep.Tables {
		mark := "맞다"
		if !c.OK() {
			mark = "어긋남"
		}
		fmt.Fprintf(out, "    %-24s %8d %8d  %s  %s\n", c.Table, c.Source, c.Target, hex.EncodeToString(c.SourceDigest[:6]), mark)
		if !c.OK() {
			listKeys(out, "원본에만", c.OnlySource, c.MissingTarget)
			listKeys(out, "대상에만", c.OnlyTarget, c.MissingSource)
		}
	}
	if len(rep.Sequences) > 0 {
		fmt.Fprintln(out, "\n  순번(identity 다음 값 > 원본에서 발급된 최고 번호):")
		for _, c := range rep.Sequences {
			mark := "맞다"
			if !c.OK() {
				mark = "어긋남"
			}
			fmt.Fprintf(out, "    %-30s 다음 %d > %d  %s\n", c.Table+"."+c.Column, c.Next, c.Floor, mark)
		}
	}
}

func listKeys(out io.Writer, what string, keys []string, total int) {
	if total == 0 {
		return
	}
	fmt.Fprintf(out, "      %s %d행:\n", what, total)
	for _, k := range keys {
		fmt.Fprintf(out, "        %s\n", k)
	}
	if more := total - len(keys); more > 0 {
		fmt.Fprintf(out, "        … 그 밖 %d행\n", more)
	}
}
