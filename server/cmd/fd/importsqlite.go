package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/kweiza/flightdeck/internal/store"
)

// `fd migrate import-sqlite` · `fd migrate verify` - SQLite 원장을 빈 Postgres 원장으로 옮기고 두 원장을 맞댄다. 거꾸로 옮기는
// `fd migrate export-sqlite` 는 exportsqlite.go 이고 보고(printTransferReport)를 함께 쓴다.
//
// ★ 판정 · 옮기기 · 대조는 전부 store(ImportSQLite · VerifySQLite)와 그 순수 규칙(store/pg 의 import.go · verify.go)이
// 진다. 이 파일은 플래그를 읽고 결과를 사람에게 말하는 것뿐이다 - runMigrate 와 같은 규율이다.
//
// 종료 코드: 0 옮겼다(대조가 맞았다) · 1 거절 · 어긋남 · 실패 · 2 사용법.

// migrateSubcommands 는 `fd migrate` 아래 하위 동사다. 첫 인자가 이 이름이면 판올림 대신 그 동사를 돈다.
var migrateSubcommands = map[string]func(a *App, ctx context.Context, args []string, out io.Writer) int{
	"import-sqlite": (*App).runImportSQLite,
	"verify":        (*App).runVerifySQLite,
	"export-sqlite": (*App).runExportSQLite,
}

// importFlags 는 두 동사가 같이 받는 플래그다. 대상의 기본값은 판올림과 같은 규칙(resolveDBPath)이다.
type importFlags struct {
	from, to string
	dryRun   bool
}

// parseImportFlags 는 두 동사의 플래그를 읽는다. more 가 있으면 동사만의 플래그를 더 달고 쓰는 법에 붙일 꼬리를 낸다.
func (a *App) parseImportFlags(name string, args []string, more func(*flag.FlagSet) string) (importFlags, bool) {
	fs := newFlagSet("migrate " + name)
	from := fs.String("from", "", "옮길 SQLite 원장 파일(서버를 내린 원본이나 VACUUM INTO 로 뜬 사본. 읽기 전용으로 연다)")
	to := fs.String("db", "", "대상 Postgres 접속 URL(비면 "+dbEnv+")")
	dry := fs.Bool("dry-run", false, "원장에 아무것도 쓰지 않고 잰다(원본 디렉토리에 이관 표지를 남길 수 있는지도 잰다)")
	usage := ""
	if more != nil {
		usage = more(fs)
	}
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || strings.TrimSpace(*from) == "" {
		fmt.Fprintf(fs.Output(), "쓰는 법: fd migrate %s --from <fd.db> [--db <postgres URL>] [--dry-run]%s\n", name, usage)
		return importFlags{}, false
	}
	return importFlags{from: *from, to: resolveDBPath(a.env, *to), dryRun: *dry}, true
}

// runImportSQLite 는 `fd migrate import-sqlite` 다.
func (a *App) runImportSQLite(ctx context.Context, args []string, out io.Writer) int {
	f, ok := a.parseImportFlags("import-sqlite", args, nil)
	if !ok {
		return 2
	}
	rep, err := store.ImportSQLite(ctx, f.from, f.to, store.TransferOptions{DryRun: f.dryRun}, a.log)
	head := "fd migrate import-sqlite"
	if f.dryRun {
		head += " --dry-run"
	}
	printTransferReport(out, head, rep)
	switch {
	case err != nil:
		a.log.Error("SQLite 이관 실패", "from", rep.From, "to", rep.To, "dry_run", f.dryRun, "error", err.Error())
		fmt.Fprintf(out, "\n  거절 · 실패: %s\n", err)
		if rep.Copied > 0 { // 커밋은 됐다(표지 · 사본 치우기에서 섰다) - 바꾼 칸의 기록도 그 이관의 것이다
			printRewritten(out, rep)
		}
		return 1
	case f.dryRun && !rep.TargetChecked:
		fmt.Fprintf(out, "\n  원본은 옮길 수 있다. 대상은 재지 않았다(%s 는 Postgres 접속 URL 이 아니다)\n", rep.To)
	case f.dryRun:
		fmt.Fprintln(out, "\n  옮길 수 있다 - 본 실행과 같은 트랜잭션을 끝까지 돌고 되감았다(아무것도 쓰지 않았다)")
		printRewritten(out, rep)
	default:
		fmt.Fprintf(out, "\n  옮겼다: %d 행, 대조가 맞았다\n", rep.Copied)
		printRewritten(out, rep)
		fmt.Fprintf(out, "  이관 표지를 남겼다: %s - SQLite 판은 이 원장을 다시 열지 않는다(되돌리기는 README 「72시간 안에 되돌리기」)\n",
			store.MovedMarker(f.from))
	}
	return 0
}

// runVerifySQLite 는 `fd migrate verify` 다. 쓰지 않으므로 --dry-run 을 받아도 하는 일이 같다(두 동사의 플래그가 한 벌이다).
// --exported 는 이 동사만 받는다(store.VerifyOptions.Exported).
func (a *App) runVerifySQLite(ctx context.Context, args []string, out io.Writer) int {
	var exported *bool
	f, ok := a.parseImportFlags("verify", args, func(fs *flag.FlagSet) string {
		exported = fs.Bool("exported", false, "--from 이 export-sqlite 가 낸 파일이다 - 두 원장을 변환 없이 맞댄다"+
			"(이관 원본에 거는 NUL 바꿔 넣기를 걸지 않는다)")
		return " [--exported]"
	})
	if !ok {
		return 2
	}
	rep, err := store.VerifySQLite(ctx, f.from, f.to, store.VerifyOptions{Exported: *exported}, a.log)
	head := "fd migrate verify"
	if *exported {
		head += " --exported"
	}
	printTransferReport(out, head, rep)
	if err != nil {
		a.log.Error("SQLite 대조 실패", "from", rep.From, "to", rep.To, "error", err.Error())
		fmt.Fprintf(out, "\n  어긋남 · 실패: %s\n", err)
		return 1
	}
	fmt.Fprintln(out, "\n  두 원장이 맞다")
	printRewritten(out, rep)
	return 0
}

// printRewritten 은 판정 줄 아래에 원본에서 바꿔 넣은 행의 수와, 이관이 남긴 바꾼 칸의 기록 파일을 한 번 더 말한다 - 맞았다는
// 말이 바꾼 값을 덮지 않게.
func printRewritten(out io.Writer, rep store.TransferReport) {
	if rep.RewrittenRows > 0 {
		fmt.Fprintf(out, "  그중 %d 행은 원본의 NUL 을 %s 표기로 바꾼 값이다(위 「옮기며 바꾼 원본 칸」) - 원본 파일은 그대로다\n",
			rep.RewrittenRows, `\x00`)
	}
	if rep.RewriteRecord != "" {
		fmt.Fprintf(out, "  바꾼 칸 전부(행 키 · 칸 · JSON 자리 · 원래 바이트)를 %s 에 남겼다 - 그 칸의 원래 바이트는 이 파일이 정본이다(지우지 않는다)\n",
			rep.RewriteRecord)
	}
}

// printTransferReport 는 이관 · 역이관 · 대조 보고를 사람 말로 낸다. 접속 URL 원문은 보고에 없다(표시값뿐이다).
func printTransferReport(out io.Writer, head string, rep store.TransferReport) {
	fmt.Fprintf(out, "%s · %s → %s\n", head, rep.From, rep.To)
	printCells(out, "사전 검사에 걸린 원본 행(표 · 칸 · 사유 · 수 · 행 좌표)", rep.Faults)
	if len(rep.Rewrites) > 0 {
		printCells(out, fmt.Sprintf("옮기며 바꾼 원본 칸 - %d 행(표 · 칸 · 규칙 · 수 · 옮긴 행 좌표)", rep.RewrittenRows), rep.Rewrites)
	}
	printCells(out, "값 검사에 걸린 대상 행(표 · 칸 · 사유 · 수 · 행 좌표)", rep.TargetFaults)
	if len(rep.Tables) == 0 {
		if len(rep.Rows) > 0 {
			fmt.Fprintln(out, "\n  원본 행 수:")
			for _, r := range rep.Rows {
				fmt.Fprintf(out, "    %-24s %d\n", r.Table, r.Rows)
			}
		}
		printQuarantined(out, rep)
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
	printQuarantined(out, rep)
}

// printQuarantined 는 SQLite 원장 자리의 판올림 전 사본을 옮긴 디렉토리를 말한다(store.TransferReport.Quarantined). 대조표가 없는
// 갈래(오류)에서도 낸다 - 옮긴 뒤에 선 실행도 어디로 옮겼는지 말해야 한다.
func printQuarantined(out io.Writer, rep store.TransferReport) {
	if rep.Quarantined == "" {
		return
	}
	fmt.Fprintf(out, "\n  판올림 전 사본(.bak-*)을 %s 로 옮겼다 - 그 자리의 원장이 바뀌어 `fd migrate --rollback` 이 고를 자리에 두지 않는다(지우지 않았다)\n",
		rep.Quarantined)
}

// printCells 는 칸 규칙이 센 갈래(걸린 칸 · 바꿔 넣은 칸)를 늘어놓는다. 원본의 사전 검사 · 바꿔 넣기와 대조가 읽은 대상 값이
// 같은 꼴로 나온다.
func printCells[R ~string](out io.Writer, title string, groups []store.CellGroup[R]) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintf(out, "\n  %s:\n", title)
	for _, g := range groups {
		fmt.Fprintf(out, "    %s.%s  %s  %d건\n", g.Table, g.Column, g.Reason, g.Count)
		for _, k := range g.Keys {
			fmt.Fprintf(out, "      %s\n", k)
		}
		if more := g.Count - len(g.Keys); more > 0 {
			fmt.Fprintf(out, "      … 그 밖 %d건\n", more)
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
