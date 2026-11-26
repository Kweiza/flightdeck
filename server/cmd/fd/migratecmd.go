package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kweiza/flightdeck/internal/store"
)

// backupDirEnv 는 Postgres 원장의 판올림 전 덤프 자리를 주는 환경 변수다(--backup-dir 가 비었을 때).
const backupDirEnv = "FD_BACKUP_DIR"

// defaultBackupDir 는 배치(compose)가 덤프 볼륨을 붙이는 자리다. 있을 때만 쓴다.
const defaultBackupDir = "/backups"

// resolveBackupDir 는 덤프 자리를 고른다: 플래그 → FD_BACKUP_DIR → 있으면 /backups. 다 없으면 빈 값이고, 그러면
// 덤프가 필요한 판올림은 거절된다(임시 디렉토리로 물러서지 않는다 - 컨테이너가 다시 뜨면 사라지는 덤프는 백업이 아니다).
func resolveBackupDir(env func(string) (string, bool), flag string) string {
	if strings.TrimSpace(flag) != "" {
		return flag
	}
	if v, ok := env(backupDirEnv); ok && strings.TrimSpace(v) != "" {
		return v
	}
	if fi, err := os.Stat(defaultBackupDir); err == nil && fi.IsDir() {
		return defaultBackupDir
	}
	return ""
}

// `fd migrate` — 설계 §7 처방 ①·③(적용을 기동에서 분리 · 롤백 명령).
//
// ★ 이 명령이 생기기 전까지 §7 의 처방 셋 중 ②(기동 전 백업)만 구현돼 있었다. 적용이
// store.Open() 안에 있었고 그것이 `fd serve` 기동 경로라, 나쁜 증분은 "서버가 안 뜬다" 로
// 나타났으며 고칠 수단도 같은 바이너리를 다시 띄우는 것뿐이었다.
//
// ★ **판정도 백업도 여기서 다시 하지 않는다.** 전부 store 의 PlanMigration·migrateTo 가 진다.
// 이 파일이 더하는 것은 "누가 언제 부르는가" 와 그 결과를 사람에게 말하는 것뿐이다 —
// 판정을 여기 복제하면 두 벌이 되고, 두 벌은 반드시 표류한다.
func (a *App) runMigrate(ctx context.Context, args []string, out io.Writer) int {
	fs := newFlagSet("migrate")
	dbPath := fs.String("db", "", "원장: SQLite 파일 경로 또는 Postgres 접속 URL(비면 자동)")
	to := fs.Int("to", 0, "이 버전까지만 올린다(0 이면 이 바이너리가 아는 최신)")
	rollback := fs.Bool("rollback", false, "판올림 전 백업 중 최신 것으로 되돌린다(Postgres 는 복원 절차를 낸다)")
	backupDir := fs.String("backup-dir", "", "Postgres 판올림 전 덤프 자리(비면 "+backupDirEnv+", 그것도 비면 "+defaultBackupDir+" 가 있을 때 그곳)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// ★ 둘을 함께 받지 않는다. 어느 쪽인지 알 수 없으므로 아무것도 하지 않는다
	//   (runExport 의 --to-legacy/--judgments 와 같은 규율).
	if *rollback && *to != 0 {
		fmt.Fprintln(out, "--to 와 --rollback 을 함께 줬다 — 어느 쪽인지 알 수 없으므로 아무것도 하지 않는다")
		return 2
	}

	path := resolveDBPath(a.env, *dbPath)
	shown := store.ParseTarget(path).Shown() // 로그 · 출력에는 이 표시값만 싣는다
	if err := ensureDBDir(path); err != nil {
		fmt.Fprintf(out, "DB 디렉토리를 만들지 못했다(%s): %s\n", shown, err)
		return 1
	}

	dumps := resolveBackupDir(a.env, *backupDir)

	if *rollback {
		got, err := store.Rollback(ctx, path, dumps, a.log)
		if err != nil {
			a.log.Error("되돌리기 실패", "path", shown, "error", err.Error())
			fmt.Fprintf(out, "되돌리기 실패: %s\n", err)
			return 1
		}
		if got.Steps == "" {
			fmt.Fprintf(out, "fd migrate --rollback · %s\n\n  %s 에서 되돌렸다\n", shown, got.Backup)
			return 0
		}
		// ★ Postgres 는 살아 있는 DB 를 덮지 않는다. 덤프를 고르고 머리를 잰 것까지가 이 명령이고, 복원은 사람이 서버를
		//   멈추고 스크립트를 돈다 - 그래서 「되돌렸다」라고 쓰지 않는다. 스크립트는 출력의 마지막이다(#!/bin/sh 부터 끝까지
		//   파일로 옮겨 그대로 돌린다).
		fmt.Fprintf(out, "fd migrate --rollback · %s\n\n  덤프: %s\n  아직 되돌리지 않았다. %s\n\n%s\n", shown, got.Backup, got.Steps, got.Script)
		return 0
	}

	target := store.SchemaVersion
	if *to != 0 {
		target = *to
	}

	// ★ 이미 맞으면 **그렇다고 말하고 아무것도 안 한다.** 침묵하면 운영자가 두 번 돌았는지
	//   아닌지 모르고, 그 모호함이 컨테이너의 one-shot 단계에서 재시작마다 되풀이된다.
	if plan, err := store.ProbeMigration(ctx, path); err == nil &&
		plan.Action == store.MigrateNone && target == store.SchemaVersion {
		fmt.Fprintf(out, "fd migrate · %s\n\n  이미 맞다: 스키마 %d판\n", shown, store.SchemaVersion)
		return 0
	}

	if err := store.MigrateWith(ctx, path, store.MigrateOptions{Target: target, BackupDir: dumps}, a.log); err != nil {
		a.log.Error("적용 실패", "path", shown, "target", target, "error", err.Error())
		fmt.Fprintf(out, "적용 실패: %s\n", err)
		return 1
	}
	fmt.Fprintf(out, "fd migrate · %s\n\n  올렸다: 스키마 %d판\n", shown, target)
	return 0
}
