package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// 판올림 전 백업과 그것으로 덮는 되돌리기(SQLite)가 덜 뜬 사본 · 남의 원장의 사본으로 원장을 잃지 않는지 잠근다. 끊긴 뜨기는 프로세스를
// 죽이지 않고 그 흔적(덜 쓴 파일 · 옆 -journal · 숨은 임시 이름)을 직접 만들어 같은 갈래를 밟는다.

// upgradedLedger 는 판 SchemaVersion-1 에서 판올림한 SQLite 원장과 그때 뜬 백업을 낸다. 판올림 뒤에 event 한 줄(kind
// after.upgrade)을 더 쓴다 - 되돌리기가 원장 자리에서는 그것을 물리고 남긴 이름에는 그것을 두는지 잰다.
func upgradedLedger(t *testing.T) (path, backup string) {
	t.Helper()
	ctx := context.Background()
	path = filepath.Join(t.TempDir(), "fd.db")
	mustMigrateTo(t, path, SchemaVersion-1)
	mustMigrate(t, path)
	bs := backupsOf(t, path)
	if len(bs) != 1 {
		t.Fatalf("전제가 깨졌다 - 판올림이 백업을 %d개 떴다(1개여야 한다): %v", len(bs), bs)
	}
	s, err := OpenWithLogger(path, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO event(at, project, session_id, kind, payload) VALUES (?, 'p', 's', 'after.upgrade', '{}')`,
		fmtTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path, bs[0]
}

// sqliteLedgerFacts 는 SQLite 원장 path 의 판과 after.upgrade 이벤트 수다(원장 내보내기와 같은 접속 문자열 - 남은 -wal 까지 읽는다).
func sqliteLedgerFacts(t *testing.T, path string) (version, afterUpgrade int) {
	t.Helper()
	ctx := context.Background()
	r, err := openSQLiteReader(ctx, path, ledgerDSN(path), quietLog())
	if err != nil {
		t.Fatalf("%s 를 못 연다: %v", filepath.Base(path), err)
	}
	defer r.Close()
	rows, err := readVersionRows(ctx, r.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM event WHERE kind = 'after.upgrade'`).Scan(&afterUpgrade); err != nil {
		t.Fatal(err)
	}
	return topVersion(rows), afterUpgrade
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustWrite(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// globIn 은 dir 안에서 pattern 에 맞는 이름들이다.
func globIn(t *testing.T, dir, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 판올림 전 백업은 숨은 임시 이름에 뜨고, 다 뜬 그 판의 원장인지 잰 뒤에만 <db>.bak-* 이름을 받는다. 재기에서 걸리면 최종 이름도
// 임시 파일도 남기지 않는다(판도 안 올린다).
func TestUpgradeBackupIsSealedBeforeItGetsItsName(t *testing.T) {
	path, backup := upgradedLedger(t)
	dir := filepath.Dir(path)
	if left := globIn(t, dir, ".*partial*"); len(left) != 0 {
		t.Fatalf("다 뜬 뒤에도 임시 이름이 남았다: %v", left)
	}
	rows, err := checkLedgerCopy(context.Background(), backup)
	if err != nil {
		t.Fatalf("이름을 받은 백업이 다 뜬 원장이 아니다: %v", err)
	}
	if v := topVersion(rows); v != SchemaVersion-1 {
		t.Fatalf("백업의 판이 %d 다(판올림 전 %d 이어야 한다)", v, SchemaVersion-1)
	}

	// 재기에서 걸리는 갈래: 뜬 사본의 판이 원장이 말한 판과 다르다(from 을 어긋나게 넘긴다).
	s, err := OpenWithLogger(path, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := backupsOf(t, path)
	got, err := s.backup(context.Background(), SchemaVersion-1, time.Now())
	if err == nil || !strings.Contains(err.Error(), "백업을 뜬 사본의 판이") {
		t.Fatalf("판이 어긋난 사본에 이름을 붙였다(%q, %v)", got, err)
	}
	if after := backupsOf(t, path); len(after) != len(before) {
		t.Fatalf("재기에서 걸린 사본이 백업 이름을 받았다: %v → %v", before, after)
	}
	if left := globIn(t, dir, ".*partial*"); len(left) != 0 {
		t.Fatalf("재기에서 걸린 사본의 임시 파일이 남았다: %v", left)
	}
}

// 되돌리기는 가장 최근 <db>.bak-* 이 다 뜬 원장일 때만 덮는다. 덜 쓴 사본 · 옆 -journal 이 남은 사본 · 그 -journal 자체는 고르지 않고,
// 거절할 때 원장을 한 바이트도 안 바꾸고 남긴 이름도 만들지 않는다. 숨은 임시 이름(끊긴 판올림 전 백업)은 후보가 아니다.
func TestRollbackRefusesAnIncompleteNewestBackup(t *testing.T) {
	ctx := context.Background()
	path, good := upgradedLedger(t)
	dir := filepath.Dir(path)
	later := path + BackupSuffix(time.Now().Add(time.Hour))
	full := mustRead(t, good)

	for _, c := range []struct {
		name string
		lay  func()
		want string
	}{
		{"덜 쓴 사본", func() { mustWrite(t, later, full[:len(full)/2]) }, "다 뜬 원장이 아니다"},
		{"옆 -journal 이 남은 사본", func() {
			mustWrite(t, later, full)
			mustWrite(t, later+"-journal", []byte("끊긴 쓰기"))
		}, "뜨는 도중 끊긴 사본이다"},
		{"원장이 아닌 파일", func() { mustWrite(t, later, []byte("not a database")) }, "다 뜬 원장이 아니다"},
		// 열리고 판 기록도 읽히지만 인덱스 쪽이 깨진 사본 - integrity_check 만 잡는다.
		{"integrity_check 가 걸리는 사본", func() { mustWrite(t, later, brokenIndexCopy(t, good, full)) }, "integrity_check: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.lay()
			defer os.Remove(later)
			defer os.Remove(later + "-journal")
			ledger := mustRead(t, path)
			_, err := Rollback(ctx, path, "", quietLog())
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), filepath.Base(later)) {
				t.Fatalf("가장 최근 백업이 %s 인데 거절하지 않았다(또는 사유 · 이름이 없다): %v", c.name, err)
			}
			if !bytes.Equal(mustRead(t, path), ledger) {
				t.Fatal("거절했는데 원장이 바뀌었다")
			}
			if kept := globIn(t, dir, "fd.db.pre-rollback-*"); len(kept) != 0 {
				t.Fatalf("거절했는데 남긴 이름을 만들었다: %v", kept)
			}
		})
	}

	// 숨은 임시 이름은 그 갈래에서 지워지지 못한 채 남아도 후보가 아니다 - 다 뜬 백업으로 되돌린다.
	mustWrite(t, filepath.Join(dir, "."+filepath.Base(later)+".partial-123"), full[:100])
	out, err := Rollback(ctx, path, "", quietLog())
	if err != nil {
		t.Fatalf("다 뜬 백업으로 되돌리지 못했다: %v", err)
	}
	if out.Backup != good {
		t.Fatalf("고른 백업이 %s 다(%s 이어야 한다)", out.Backup, good)
	}
}

// brokenIndexCopy 는 사본 full(파일 src 의 바이트)에서 event_by_kind 인덱스의 뿌리 페이지 머리(페이지 종류)를 깨뜨린 바이트다. 표 ·
// 판 기록 페이지는 그대로라 열기와 판 기록 읽기는 지나고, integrity_check 만 걸린다.
func brokenIndexCopy(t *testing.T, src string, full []byte) []byte {
	t.Helper()
	ctx := context.Background()
	r, err := openSQLiteReader(ctx, src, importSourceDSN(src), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var root, size int
	if err := r.db.QueryRowContext(ctx, `SELECT rootpage FROM sqlite_master WHERE name = 'event_by_kind'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := r.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if root < 2 || root*size > len(full) {
		t.Fatalf("전제가 깨졌다 - 인덱스 뿌리 페이지 %d(페이지 %d 바이트, 파일 %d 바이트)", root, size, len(full))
	}
	out := bytes.Clone(full)
	out[(root-1)*size] = 0x01 // B-tree 페이지 종류(2 · 5 · 10 · 13)가 아니다
	return out
}

// 되돌리기는 이 바이너리가 여는 판의 사본만 고른다. 새 판의 fd 가 올리며 뜬 사본(판 기록은 이 원장의 앞부분이다)이 가장 최근이면,
// 그것으로 덮은 원장은 이 바이너리가 못 연다 - 거절하고 아무것도 안 바꾼다.
func TestRollbackRefusesABackupAboveThisBinary(t *testing.T) {
	ctx := context.Background()
	path, _ := upgradedLedger(t)
	s, err := openSQLiteReader(ctx, path, ledgerDSN(path), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	stamp := fmtTime(time.Now())
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version(version, applied_at) VALUES (?, ?)`, SchemaVersion+1, stamp); err != nil {
		t.Fatal(err)
	}
	newer := path + BackupSuffix(time.Now().Add(time.Hour))
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(newer, "'", "''")+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version(version, applied_at) VALUES (?, ?)`, SchemaVersion+2, stamp); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ledger := mustRead(t, path)

	_, err = Rollback(ctx, path, "", quietLog())
	want := fmt.Sprintf("그 사본은 판 %d 이다", SchemaVersion+1)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("이 바이너리 밖 판의 사본으로 덮으려 했다(기대 %q): %v", want, err)
	}
	if !bytes.Equal(mustRead(t, path), ledger) {
		t.Fatal("거절했는데 원장이 바뀌었다")
	}
}

// 되돌리기는 덮기 전의 원장을 <db>.pre-rollback-<시각> 으로 남긴다(옆 파일 포함). 원장 자리는 판올림 전 판이고 그 뒤의 기록이 없으며,
// 남긴 이름은 덮기 직전의 판과 기록을 그대로 연다.
func TestRollbackKeepsTheLedgerItOverwrites(t *testing.T) {
	path, good := upgradedLedger(t)
	if v, n := sqliteLedgerFacts(t, path); v != SchemaVersion || n != 1 {
		t.Fatalf("전제가 깨졌다 - 원장이 판 %d · 판올림 뒤 기록 %d 줄이다", v, n)
	}
	out, err := Rollback(context.Background(), path, "", quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if out.Backup != good || !strings.HasPrefix(filepath.Base(out.Kept), "fd.db.pre-rollback-") {
		t.Fatalf("결과가 어긋났다: %+v", out)
	}
	if v, n := sqliteLedgerFacts(t, path); v != SchemaVersion-1 || n != 0 {
		t.Fatalf("되돌린 원장이 판 %d · 판올림 뒤 기록 %d 줄이다(판 %d · 0 줄이어야 한다)", v, n, SchemaVersion-1)
	}
	if v, n := sqliteLedgerFacts(t, out.Kept); v != SchemaVersion || n != 1 {
		t.Fatalf("남긴 원장이 판 %d · 판올림 뒤 기록 %d 줄이다(덮기 직전 판 %d · 1 줄이어야 한다)", v, n, SchemaVersion)
	}
	for _, side := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + side); err == nil {
			t.Errorf("되돌린 원장 옆에 %s 가 남았다", side)
		}
	}
	if left := globIn(t, filepath.Dir(path), ".fd.db*"); len(left) != 0 {
		t.Errorf("임시 파일이 남았다: %v", left)
	}
}

// 원장을 다른 원장으로 바꿔 놓은 뒤(역이관이 낸 파일 · 손으로 되살린 원장) 남은 옛 원장의 백업으로는 덮지 않는다 - 맞대기를 마친
// 원장이 그 뒤의 기록 없이 옛 판으로 돌아간다. 판 기록이 지금 원장의 앞부분과 같을 때만 이 원장의 판올림 전 사본이다.
func TestRollbackRefusesABackupOfAnotherLedger(t *testing.T) {
	ctx := context.Background()
	path, _ := upgradedLedger(t)

	// 같은 자리에 다른 원장을 놓는다: 판은 같지만 판 기록(적용 시각)은 그 원장의 것이다.
	other := filepath.Join(t.TempDir(), "other.db")
	mustMigrate(t, other)
	for _, side := range []string{"-wal", "-shm"} {
		os.Remove(path + side)
	}
	mustWrite(t, path, mustRead(t, other))
	ledger := mustRead(t, path)

	_, err := Rollback(ctx, path, "", quietLog())
	if err == nil || !strings.Contains(err.Error(), "이 원장의 판올림 전 사본이 아니다") || !strings.Contains(err.Error(), "fd migrate --to") {
		t.Fatalf("다른 원장의 백업으로 덮으려 했다(또는 판만 내리는 길을 안 말한다): %v", err)
	}
	if !bytes.Equal(mustRead(t, path), ledger) {
		t.Fatal("거절했는데 원장이 바뀌었다")
	}
	if kept := globIn(t, filepath.Dir(path), "fd.db.pre-rollback-*"); len(kept) != 0 {
		t.Fatalf("거절했는데 남긴 이름을 만들었다: %v", kept)
	}
}

// 판 기록의 앞부분 판정(isPrefix)은 같은 판 · 다른 적용 시각을 다른 원장으로 읽는다. 순수 함수의 경계를 따로 잰다.
func TestVersionRowsPrefix(t *testing.T) {
	a := []versionRow{{1, "t1"}, {2, "t2"}}
	for _, c := range []struct {
		name string
		x, y []versionRow
		want bool
	}{
		{"같다", a, a, true},
		{"앞부분", a[:1], a, true},
		{"빈 사본", nil, a, true},
		{"더 길다", a, a[:1], false},
		{"시각이 다르다", []versionRow{{1, "t1"}, {2, "other"}}, a, false},
		{"판이 다르다", []versionRow{{1, "t1"}, {3, "t2"}}, a, false},
	} {
		if got := isPrefix(c.x, c.y); got != c.want {
			t.Errorf("%s: isPrefix = %v(기대 %v)", c.name, got, c.want)
		}
	}
}

// 72시간 되돌리기의 갈래를 실제로 밟는다: 판올림한 SQLite 원장(옆에 판올림 전 백업)을 옆 이름으로 치우고, 그 자리에 역이관이 낸
// 원장을 놓는다. 역이관은 이름을 붙이기 전에 그 자리의 옛 백업을 새 디렉토리로 치운다(quarantineBackups) - 되돌리기는 고를 백업이
// 없다. 옛 판의 역이관이나 손으로 다시 놓은 옛 백업이 남아도 그것은 그 원장의 사본이 아니다(판 기록이 다르다) - 덮으면 맞대기를
// 마친 원장이 Postgres 에서 쓴 기록 없이 옛 판으로 돌아간다.
func TestRollbackRefusesTheOldBackupBesideAnExportedLedger(t *testing.T) {
	ctx := context.Background()
	path, old := upgradedLedger(t)
	oldBytes := mustRead(t, old)
	for _, side := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(path+side, path+".pre-back"+side); err != nil && side == "" {
			t.Fatal(err)
		}
	}
	mustWrite(t, old+"-journal", []byte("옛 사본 옆 저널")) // 옆 파일도 사본과 함께 간다
	_, dsn, _ := importedPG(t)
	rep, err := exportLedger(t, ctx, dsn, path, TransferOptions{})
	if err != nil {
		t.Fatalf("역이관: %v", err)
	}
	if left := globIn(t, filepath.Dir(path), filepath.Base(path)+".bak-*"); len(left) != 0 {
		t.Fatalf("역이관이 낸 원장 옆에 옛 백업이 남았다: %v", left)
	}
	if !strings.HasPrefix(filepath.Base(rep.Quarantined), "fd.db.pre-export-") || !strings.HasSuffix(rep.Quarantined, ".bak") {
		t.Fatalf("옛 백업을 옮긴 디렉토리를 보고하지 않았다: %q", rep.Quarantined)
	}
	moved := filepath.Join(rep.Quarantined, filepath.Base(old))
	if !bytes.Equal(mustRead(t, moved), oldBytes) || !bytes.Equal(mustRead(t, moved+"-journal"), []byte("옛 사본 옆 저널")) {
		t.Fatal("옮긴 옛 백업(과 옆 파일)이 그대로가 아니다")
	}
	exported := mustRead(t, path)
	if _, err := Rollback(ctx, path, "", quietLog()); err == nil || !strings.Contains(err.Error(), "되돌릴 백업이 없다") {
		t.Fatalf("역이관이 낸 원장에 고를 백업이 있다: %v", err)
	}

	// 판 기록 재기는 그대로 다음 방어선이다: 옛 백업을 손으로 다시 놓아도 덮지 않는다.
	if err := os.Rename(moved, old); err != nil {
		t.Fatal(err)
	}
	_, err = Rollback(ctx, path, "", quietLog())
	if err == nil || !strings.Contains(err.Error(), "이 원장의 판올림 전 사본이 아니다") {
		t.Fatalf("역이관이 낸 원장을 옛 원장의 백업으로 덮으려 했다: %v", err)
	}
	if !bytes.Equal(mustRead(t, path), exported) {
		t.Fatal("거절했는데 역이관이 낸 원장이 바뀌었다")
	}
}

// 예행은 옛 백업을 옮기지 않는다 - 아무것도 남기지도 바꾸지도 않는다.
func TestExportDryRunLeavesTheOldBackups(t *testing.T) {
	ctx := context.Background()
	path, old := upgradedLedger(t)
	for _, side := range []string{"", "-wal", "-shm"} {
		os.Rename(path+side, path+".pre-back"+side)
	}
	_, dsn, _ := importedPG(t)
	rep, err := exportLedger(t, ctx, dsn, path, TransferOptions{DryRun: true})
	if err != nil {
		t.Fatalf("역이관 예행: %v", err)
	}
	if rep.Quarantined != "" || len(backupsOf(t, path)) != 1 || backupsOf(t, path)[0] != old {
		t.Fatalf("예행이 옛 백업을 옮겼다: %q %v", rep.Quarantined, backupsOf(t, path))
	}
	if left := globIn(t, filepath.Dir(path), "fd.db.pre-export-*"); len(left) != 0 {
		t.Fatalf("예행이 디렉토리를 만들었다: %v", left)
	}
}

// 옛 백업을 치우는 디렉토리는 늘 새로 만든다 - 같은 이름의 디렉토리가 있으면 번호를 붙이고, 있던 디렉토리에 섞지 않는다. 옮길 것이
// 없으면 아무것도 만들지 않는다.
func TestQuarantineBackupsUsesAFreshDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fd.db")
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	if got, err := quarantineBackups(path, "export", at); err != nil || got != "" {
		t.Fatalf("옮길 것이 없는데 %q, %v", got, err)
	}
	if left := globIn(t, dir, "*"); len(left) != 0 {
		t.Fatalf("옮길 것이 없는데 만들었다: %v", left)
	}
	taken := path + ".pre-export-20261127T120000Z.bak"
	if err := os.Mkdir(taken, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(taken, "먼저 있던 것"), []byte("x"))
	bak := path + BackupSuffix(at)
	mustWrite(t, bak, []byte("사본"))
	mustWrite(t, bak+"-wal", []byte("옆"))
	got, err := quarantineBackups(path, "export", at)
	if err != nil {
		t.Fatal(err)
	}
	if got != path+".pre-export-20261127T120000Z-001.bak" {
		t.Fatalf("있는 디렉토리를 다시 썼거나 이름이 다르다: %q", got)
	}
	if left := globIn(t, taken, "*"); len(left) != 1 {
		t.Fatalf("있던 디렉토리에 섞었다: %v", left)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(got, filepath.Base(bak))), []byte("사본")) ||
		!bytes.Equal(mustRead(t, filepath.Join(got, filepath.Base(bak)+"-wal")), []byte("옆")) {
		t.Fatal("사본과 옆 파일을 그대로 옮기지 않았다")
	}
	if len(backupsOf(t, path)) != 0 {
		t.Fatalf("원장 옆에 사본이 남았다: %v", backupsOf(t, path))
	}
}

// 이름 붙이기는 있는 파일을 갈지 않는다: 차 있으면 번호를 붙인 다음 빈 이름에 붙이고, 있던 파일은 한 바이트도 안 바뀐다.
func TestLinkFreshNeverReplacesAnExistingName(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "fd.db.bak-20261127T120000Z")
	mustWrite(t, name, []byte("앞 실행"))
	for i, want := range []string{name + "-001", name + "-002"} {
		tmp := filepath.Join(dir, fmt.Sprintf(".tmp-%d", i))
		mustWrite(t, tmp, []byte(fmt.Sprintf("뒤 실행 %d", i)))
		got, err := linkFresh(tmp, name)
		if err != nil || got != want {
			t.Fatalf("%d 번째: %q, %v(기대 %q)", i, got, err, want)
		}
		if !bytes.Equal(mustRead(t, got), []byte(fmt.Sprintf("뒤 실행 %d", i))) {
			t.Fatalf("%d 번째: 붙인 이름의 내용이 다르다", i)
		}
		if _, err := os.Lstat(tmp); err == nil {
			t.Fatalf("%d 번째: 임시 이름이 남았다", i)
		}
	}
	if !bytes.Equal(mustRead(t, name), []byte("앞 실행")) || !bytes.Equal(mustRead(t, name+"-001"), []byte("뒤 실행 0")) {
		t.Fatal("있던 이름을 갈았다")
	}
}

// 같은 초에 뜬 판올림 전 백업은 먼저 붙은 사본을 덮지 않는다 - 뒤 사본은 번호 붙은 이름을 받고, 그 번호가 사전순으로 뒤라 되돌리기는
// 뒤 사본을 가장 최근으로 고른다.
func TestUpgradeBackupInTheSameSecondKeepsTheEarlierOne(t *testing.T) {
	ctx := context.Background()
	path, _ := upgradedLedger(t)
	s, err := OpenWithLogger(path, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now().Add(time.Hour) // upgradedLedger 가 지금 뜬 백업보다 뒤 초다 - 같은 초의 두 사본이 가장 최근이어야 한다
	first := path + BackupSuffix(at)
	mustWrite(t, first, []byte("같은 초에 먼저 붙은 사본"))
	got, err := s.backup(ctx, SchemaVersion, at)
	if err != nil {
		t.Fatal(err)
	}
	if got != first+"-001" {
		t.Fatalf("같은 초의 두 번째 사본 이름이 %q 다(%q 이어야 한다)", got, first+"-001")
	}
	if !bytes.Equal(mustRead(t, first), []byte("같은 초에 먼저 붙은 사본")) {
		t.Fatal("같은 초에 먼저 붙은 사본을 덮었다")
	}
	if rows, err := checkLedgerCopy(ctx, got); err != nil || topVersion(rows) != SchemaVersion {
		t.Fatalf("번호 붙은 사본이 온전한 판 %d 원장이 아니다: %v", SchemaVersion, err)
	}
	all := backupsOf(t, path)
	sort.Strings(all)
	if all[len(all)-1] != got {
		t.Fatalf("번호 붙은 사본이 가장 최근으로 정렬되지 않는다: %v", all)
	}
	if left := globIn(t, filepath.Dir(path), ".*partial*"); len(left) != 0 {
		t.Fatalf("임시 이름이 남았다: %v", left)
	}
}

// 같은 초에 되돌리기를 두 번 돌아도 첫 번째가 남긴 원장(판올림 뒤의 기록이 남은 마지막 사본)을 잃지 않는다.
//
//   - 바로 다시 돌리면 거절한다: 가장 최근 백업의 판 기록이 지금 원장과 같다 - 무를 판올림이 없다.
//   - 다시 올린 뒤(새 백업) 같은 초에 돌리면 덮지만, 남기는 이름은 번호를 받는다 - 첫 번째가 남긴 원장은 그대로다.
func TestRollbackTwiceInTheSameSecondKeepsTheFirstKeptLedger(t *testing.T) {
	ctx := context.Background()
	path, good := upgradedLedger(t)
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	first, err := rollbackSQLite(ctx, path, at, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if first.Backup != good || first.Kept != path+".pre-rollback-20261127T120000Z" {
		t.Fatalf("첫 되돌리기: %+v", first)
	}
	keptBytes := mustRead(t, first.Kept)
	if v, n := sqliteLedgerFacts(t, first.Kept); v != SchemaVersion || n != 1 {
		t.Fatalf("첫 되돌리기가 남긴 원장이 판 %d · 판올림 뒤 기록 %d 줄이다", v, n)
	}

	ledger := mustRead(t, path)
	_, err = rollbackSQLite(ctx, path, at, quietLog())
	if err == nil || !strings.Contains(err.Error(), "판 기록이 지금 원장과 같다") {
		t.Fatalf("이미 그 백업으로 덮은 원장을 같은 백업으로 다시 덮으려 했다: %v", err)
	}
	if !bytes.Equal(mustRead(t, path), ledger) {
		t.Fatal("거절했는데 원장이 바뀌었다")
	}
	if kept := globIn(t, filepath.Dir(path), "fd.db.pre-rollback-*"); len(kept) != 1 {
		t.Fatalf("거절했는데 남긴 이름이 늘었다: %v", kept)
	}

	// 다시 올리고(새 백업) 같은 초에 되돌린다 - 남기는 이름이 첫 번째와 겹친다.
	mustMigrate(t, path)
	second, err := rollbackSQLite(ctx, path, at, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if second.Backup == good || second.Kept != first.Kept+"-001" {
		t.Fatalf("두 번째 되돌리기: %+v(백업은 새것, 남긴 이름은 %q 이어야 한다)", second, first.Kept+"-001")
	}
	if !bytes.Equal(mustRead(t, first.Kept), keptBytes) {
		t.Fatal("두 번째 되돌리기가 첫 번째가 남긴 원장을 덮었다")
	}
	if v, n := sqliteLedgerFacts(t, first.Kept); v != SchemaVersion || n != 1 {
		t.Fatalf("첫 번째가 남긴 원장이 판 %d · 판올림 뒤 기록 %d 줄이 됐다", v, n)
	}
	if v, _ := sqliteLedgerFacts(t, second.Kept); v != SchemaVersion {
		t.Fatalf("두 번째가 남긴 원장이 판 %d 다", v)
	}
}

// 남기는 이름이나 그 옆 이름에 이미 파일이 있으면(끊긴 앞 실행의 흔적) 덮지 않고 다음 번호에 원장과 옆 파일을 한 벌로 남긴다.
// 원장 옆에 없는 옆 파일이라도 그 이름이 차 있으면 넘어간다 - 남은 -wal 이 남긴 원장에 얹히지 않게. 옆 파일을 못 붙이면 이 실행이
// 붙인 것을 다 떼고 원장 옆 파일은 그대로 둔다 - -wal 만 떠나 원장이 체크포인트 안 된 커밋을 잃는 반쪽 상태를 만들지 않는다.
//
// ★ keepLedger 를 바로 부른다. rollbackSQLite 를 거치면 그 앞의 후보 재기가 원장을 열고 닫으며 SQLite 가 -wal · -shm 을
// 체크포인트하고 지워, 이 갈래에 옆 파일이 남지 않는다.
func TestKeepLedgerNeverReplacesASideFile(t *testing.T) {
	ledger := func(t *testing.T, sides ...string) (string, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "fd.db")
		mustWrite(t, path, []byte("원장"))
		for _, side := range sides {
			mustWrite(t, path+side, []byte("원장 옆 "+side))
		}
		return path, path + ".pre-rollback-20261127T120000Z"
	}
	absent := func(t *testing.T, name string) {
		t.Helper()
		if _, err := os.Lstat(name); err == nil {
			t.Fatalf("%s 가 남았다", filepath.Base(name))
		}
	}

	t.Run("옆 이름이 차 있으면 다음 번호에 한 벌로", func(t *testing.T) {
		path, name := ledger(t, "-wal", "-shm")
		mustWrite(t, name+"-shm", []byte("앞 실행의 흔적"))
		kept, err := keepLedger(path, name)
		if err != nil || kept != name+"-001" {
			t.Fatalf("남긴 이름 %q, %v(기대 %q)", kept, err, name+"-001")
		}
		if !bytes.Equal(mustRead(t, name+"-shm"), []byte("앞 실행의 흔적")) {
			t.Fatal("남기는 이름 옆에 있던 파일을 덮었다")
		}
		absent(t, name)
		for _, side := range []string{"", "-wal", "-shm"} {
			want := "원장 옆 " + side
			if side == "" {
				want = "원장"
			}
			if !bytes.Equal(mustRead(t, kept+side), []byte(want)) {
				t.Fatalf("남긴 원장%s 가 원장과 다르다", side)
			}
		}
		absent(t, path+"-wal")
		absent(t, path+"-shm")
		if !bytes.Equal(mustRead(t, path), []byte("원장")) {
			t.Fatal("원장 파일을 옮겼다 - 원장 자리는 덮기 전까지 그대로여야 한다")
		}
	})

	t.Run("원장 옆에 없는 옆 파일의 이름도 비어야", func(t *testing.T) {
		path, name := ledger(t)
		mustWrite(t, name+"-wal", []byte("앞 실행의 -wal"))
		kept, err := keepLedger(path, name)
		if err != nil || kept != name+"-001" {
			t.Fatalf("남긴 이름 %q, %v - 남은 -wal 옆에 붙이면 그 -wal 이 남긴 원장에 얹힌다", kept, err)
		}
		absent(t, name)
		absent(t, kept+"-wal")
	})

	t.Run("옆 파일을 못 붙이면 다 떼고 선다", func(t *testing.T) {
		path, name := ledger(t, "-shm")
		if err := os.Mkdir(path+"-wal", 0o755); err != nil { // 디렉토리에는 하드 링크를 못 건다
			t.Fatal(err)
		}
		kept, err := keepLedger(path, name)
		if err == nil || kept != "" || !strings.Contains(err.Error(), "원장 옆 파일은 그대로다") {
			t.Fatalf("옆 파일을 못 붙였는데 남겼다고 한다: %q, %v", kept, err)
		}
		if left := globIn(t, filepath.Dir(path), "*.pre-rollback-*"); len(left) != 0 {
			t.Fatalf("이 실행이 붙인 것을 떼지 않았다: %v", left)
		}
		if left := globIn(t, filepath.Dir(path), ".*stage*"); len(left) != 0 {
			t.Fatalf("임시 사본이 남았다: %v", left)
		}
		if !bytes.Equal(mustRead(t, path+"-shm"), []byte("원장 옆 -shm")) {
			t.Fatal("원장 옆 파일이 떠났거나 바뀌었다")
		}
	})
}
