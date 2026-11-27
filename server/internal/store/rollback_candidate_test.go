package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	got, err := s.backup(context.Background(), SchemaVersion-1)
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
// 원장을 놓는다. 남은 백업은 그 원장의 사본이 아니다 - 덮으면 맞대기를 마친 원장이 Postgres 에서 쓴 기록 없이 옛 판으로 돌아간다.
func TestRollbackRefusesTheOldBackupBesideAnExportedLedger(t *testing.T) {
	ctx := context.Background()
	path, _ := upgradedLedger(t)
	for _, side := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(path+side, path+".pre-back"+side); err != nil && side == "" {
			t.Fatal(err)
		}
	}
	_, dsn, _ := importedPG(t)
	if _, err := exportLedger(t, ctx, dsn, path, TransferOptions{}); err != nil {
		t.Fatalf("역이관: %v", err)
	}
	if len(backupsOf(t, path)) != 1 {
		t.Fatal("전제가 깨졌다 - 옛 원장의 백업이 원장 옆에 없다")
	}
	exported := mustRead(t, path)
	_, err := Rollback(ctx, path, "", quietLog())
	if err == nil || !strings.Contains(err.Error(), "이 원장의 판올림 전 사본이 아니다") {
		t.Fatalf("역이관이 낸 원장을 옛 원장의 백업으로 덮으려 했다: %v", err)
	}
	if !bytes.Equal(mustRead(t, path), exported) {
		t.Fatal("거절했는데 역이관이 낸 원장이 바뀌었다")
	}
}
