package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// touchMarker 는 SQLite 원장 path 의 이관 표지를 빈 파일로 남긴다(손으로 `touch` 한 표지와 같다).
func touchMarker(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(MovedMarker(path), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// sqliteGateRefusals 는 SQLite 원장 path 를 여는 · 만드는 · 되돌리는 문 전부를 두드려 각 문의 오류를 낸다(거절이면 비지 않은
// 글). 판 재기(ProbeMigration)는 거절 계획의 사유를 낸다.
func sqliteGateRefusals(t *testing.T, path string) map[string]string {
	t.Helper()
	ctx := context.Background()
	got := map[string]string{}
	note := func(name string, err error) {
		if err != nil {
			got[name] = err.Error()
		} else {
			got[name] = ""
		}
	}
	plan, err := ProbeMigration(ctx, path)
	switch {
	case err != nil:
		got["ProbeMigration"] = "오류: " + err.Error()
	case plan.Action == MigrateReject:
		got["ProbeMigration"] = plan.Reason
	default:
		got["ProbeMigration"] = ""
	}
	s, err := OpenWithLogger(path, quietLog())
	if err == nil {
		s.Close()
	}
	note("OpenWithLogger", err)
	note("Migrate", Migrate(ctx, path, quietLog()))
	_, err = Rollback(ctx, path, "", quietLog())
	note("Rollback", err)
	l, err := OpenLedger(ctx, path, quietLog())
	if err == nil {
		l.Close()
	}
	note("OpenLedger", err)
	return got
}

// 이관 표지가 있으면 SQLite 원장을 여는 · 새로 만드는 · 되돌리는 문이 전부 같은 처방으로 선다 - 원장 파일이 있든 없든. 없는
// 자리에는 파일을 만들지 않고, 있는 원장은 바이트 하나 안 바뀐다. 표지를 지우면(되돌리기의 rm) 같은 문이 다시 열린다.
func TestMovedMarkerClosesEverySQLiteDoor(t *testing.T) {
	for _, withFile := range []bool{true, false} {
		name := "원장 없음"
		if withFile {
			name = "원장 있음"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fd.db")
			var before []byte
			if withFile {
				mustMigrate(t, path)
				// 되돌리기가 「백업이 없다」 로 서지 않게 백업을 하나 둔다 - 서는 까닭이 표지여야 한다.
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".bak-20260101T000000Z", raw, 0o644); err != nil {
					t.Fatal(err)
				}
				before = raw
			}
			touchMarker(t, path)
			names := dirNames(t, dir)

			for door, why := range sqliteGateRefusals(t, path) {
				if !strings.Contains(why, "Postgres 로 옮겼다") || !strings.Contains(why, "rm") ||
					!strings.Contains(why, "72시간 안에 되돌리기") {
					t.Errorf("%s 가 표지를 보고 처방과 함께 서지 않았다: %q", door, why)
				}
			}
			if got := dirNames(t, dir); !slices.Equal(got, names) {
				t.Fatalf("표지가 있는데 자리에 파일이 생기거나 사라졌다: %v → %v", names, got)
			}
			if withFile {
				if raw, _ := os.ReadFile(path); string(raw) != string(before) {
					t.Fatal("표지가 있는데 원장 파일이 바뀌었다")
				}
			}

			if err := os.Remove(MovedMarker(path)); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(context.Background(), path, quietLog()); err != nil {
				t.Fatalf("표지를 지웠는데 판올림이 선다: %v", err)
			}
			s, err := Open(path)
			if err != nil {
				t.Fatalf("표지를 지웠는데 기동이 선다: %v", err)
			}
			s.Close()
		})
	}
}

// 표지는 있는가만 본다 - 빈 파일 · 못 읽는 파일 · 디렉토리도 표지다. 표지 자리를 재지 못하면(이름이 길어 ENAMETOOLONG) 옮겼는지
// 모르므로 연다고 보지 않는다. 내용을 읽으면 옮긴 때 · 원장 · 행 수가 거절 문구에 붙는다.
func TestMovedMarkerIsPresenceOnlyAndFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		make func(t *testing.T, mark string)
		want string
	}{
		{"빈 파일", func(t *testing.T, mark string) { must(t, os.WriteFile(mark, nil, 0o644)) }, "Postgres 로 옮겼다"},
		{"못 읽는 파일", func(t *testing.T, mark string) { must(t, os.WriteFile(mark, []byte("{}"), 0)) }, "Postgres 로 옮겼다"},
		{"디렉토리", func(t *testing.T, mark string) { must(t, os.Mkdir(mark, 0o755)) }, "Postgres 로 옮겼다"},
		{"내용", func(t *testing.T, mark string) {
			raw, _ := json.Marshal(movedNote{MovedAt: "2026-11-27T01:02:03Z", Ledger: "0123456789abcdef", Rows: 42})
			must(t, os.WriteFile(mark, raw, 0o644))
		}, "Postgres 로 옮겼다(이관 표지 "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fd.db")
			c.make(t, MovedMarker(path))
			err := movedRefusal(path)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("표지(%s)를 보고 서지 않았다: %v", c.name, err)
			}
			if c.name == "내용" && !strings.Contains(err.Error(), "2026-11-27T01:02:03Z 에 Postgres 원장 0123456789abcdef 로 42 행") {
				t.Fatalf("표지 내용이 거절 문구에 안 붙었다: %v", err)
			}
		})
	}

	// 원장 이름(옆 파일 -wal · -shm 까지)은 되지만 표지 이름은 NAME_MAX(255)를 넘는다 - 표지 자리를 재지 못한다.
	// 원장은 짧은 이름으로 만들어 옮긴다 - 그 이름의 판올림은 바로 이 표지 재기에 선다.
	short := filepath.Join(t.TempDir(), "fd.db")
	mustMigrate(t, short)
	long := filepath.Join(filepath.Dir(short), strings.Repeat("l", 236)+".db")
	must(t, os.Rename(short, long))
	if err := movedRefusal(long); err == nil || !strings.Contains(err.Error(), "재지 못했다") {
		t.Fatalf("재지 못한 표지를 없는 것으로 봤다: %v", err)
	}
	if plan, err := ProbeMigration(context.Background(), long); err != nil || plan.Action != MigrateReject {
		t.Fatalf("재지 못한 표지에 판 재기가 거절을 안 냈다: %v %+v", err, plan)
	}
	if err := UnmovedSQLiteRefusal(long, 0); err == nil || !strings.Contains(err.Error(), "재지 못했다") {
		t.Fatalf("재지 못한 표지를 Postgres 쪽이 옮긴 것으로 봤다: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Postgres 판올림 관문의 갈래 전부: 원장이 없거나 표지가 있으면 받고, 표지 없는 원장이 남았으면 이관 판까지만 까는 단계(--to pg.ImportVersion)
// 말고는 거절하며, 원장 자리를 재지 못하면 거절한다.
func TestUnmovedSQLiteRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fd.db")
	if err := UnmovedSQLiteRefusal(path, 0); err != nil {
		t.Fatalf("SQLite 원장이 없는데 거절했다: %v", err)
	}
	mustMigrate(t, path)
	for _, to := range []int{0, pg.ImportVersion - 1, pg.ImportVersion + 1} {
		err := UnmovedSQLiteRefusal(path, to)
		if err == nil || !strings.Contains(err.Error(), "이관을 거치지 않은 새 원장") ||
			!strings.Contains(err.Error(), "업그레이드: SQLite 원장을 Postgres 로") || !strings.Contains(err.Error(), "touch") {
			t.Fatalf("--to %d: 표지 없는 원장을 두고 처방과 함께 서지 않았다: %v", to, err)
		}
	}
	if err := UnmovedSQLiteRefusal(path, pg.ImportVersion); err != nil {
		t.Fatalf("이관 판까지만 까는 단계(--to %d)를 거절했다: %v", pg.ImportVersion, err)
	}
	touchMarker(t, path)
	if err := UnmovedSQLiteRefusal(path, 0); err != nil {
		t.Fatalf("표지가 있는데 거절했다: %v", err)
	}

	// 원장 자리 자체를 재지 못한다(부모가 파일이라 ENOTDIR).
	file := filepath.Join(dir, "plain")
	must(t, os.WriteFile(file, nil, 0o644))
	if err := UnmovedSQLiteRefusal(filepath.Join(file, "fd.db"), 0); err == nil || !strings.Contains(err.Error(), "재지 못했다") {
		t.Fatalf("재지 못한 원장 자리를 없는 것으로 봤다: %v", err)
	}
}

// 이관은 커밋한 뒤에만 원본 옆에 표지를 남긴다: 예행은 아무것도 안 남기고, 본 실행은 표지(내용 · 모드)만 남기며 임시 파일을
// 안 남긴다. 그 뒤 SQLite 쪽 문은 서고, 읽기만 하는 맞대기는 그대로 돌며, 같은 원본을 다시 옮기지 않는다.
func TestImportSQLiteLeavesTheMovedMarker(t *testing.T) {
	ctx := context.Background()
	src := importSource(t, importSeed)
	dir := filepath.Dir(src)
	dsn, tgt := importTarget(t)
	before := dirNames(t, dir)

	if _, err := importLedger(t, ctx, src, dsn, TransferOptions{DryRun: true}); err != nil {
		t.Fatalf("예행: %v", err)
	}
	if _, ok := ledgerMovedAt(t, tgt); ok {
		t.Fatal("예행이 대상 원장에 이관 기록을 남겼다")
	}
	// 읽기 전용으로 열어도 SQLite 가 빈 옆 파일(-wal · -shm)을 만든다 - 그것 말고는 그대로여야 한다.
	if got := slices.DeleteFunc(dirNames(t, dir), func(n string) bool { return slices.Contains(pg.Sidecars(src), filepath.Join(dir, n)) }); !slices.Equal(got, before) {
		t.Fatalf("예행이 원본 디렉토리에 무엇을 남겼다: %v → %v", before, got)
	}

	start := time.Now().UTC().Truncate(time.Second)
	rep, err := importLedger(t, ctx, src, dsn, TransferOptions{})
	if err != nil {
		t.Fatalf("이관: %v", err)
	}
	mark := MovedMarker(src)
	fi, err := os.Stat(mark)
	if err != nil {
		t.Fatalf("이관이 표지를 안 남겼다: %v", err)
	}
	if fi.Mode().Perm()&0o044 == 0 {
		t.Errorf("표지 모드 %v - 서버의 다른 uid 가 못 읽는다", fi.Mode().Perm())
	}
	for _, n := range dirNames(t, dir) {
		if strings.HasPrefix(n, ".") {
			t.Errorf("이관이 임시 파일 %s 를 남겼다", n)
		}
	}
	raw, err := os.ReadFile(mark)
	if err != nil {
		t.Fatal(err)
	}
	var note movedNote
	if err := json.Unmarshal(raw, &note); err != nil {
		t.Fatalf("표지 내용이 JSON 이 아니다: %v\n%s", err, raw)
	}
	at, err := time.Parse(time.RFC3339, note.MovedAt)
	if err != nil || at.Before(start) || at.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("표지의 옮긴 때 %q 가 이관 시각이 아니다(%v)", note.MovedAt, err)
	}
	// 대상 원장의 이관 기록(되돌리기 창의 정본)이 표지의 옮긴 때와 같다.
	if rec, ok := ledgerMovedAt(t, tgt); !ok || !rec.Equal(at) {
		t.Errorf("대상 원장의 이관 기록 %v(%v)이 표지의 옮긴 때 %v 와 다르다", rec, ok, at)
	}
	if note.Schema != pg.ImportVersion || note.Rows != rep.Copied || rep.Copied == 0 {
		t.Errorf("표지의 판 %d · 행 %d, 옮긴 행 %d", note.Schema, note.Rows, rep.Copied)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(note.Ledger) {
		t.Errorf("표지의 원장 식별 %q 가 덤프 이름의 원장 식별 꼴이 아니다", note.Ledger)
	}
	if note.Note == "" || !strings.Contains(note.Undo, "rm") {
		t.Errorf("표지가 무엇인지 · 지우는 길을 안 적었다: %+v", note)
	}

	for door, why := range sqliteGateRefusals(t, src) {
		if !strings.Contains(why, "Postgres 로 옮겼다") || !strings.Contains(why, note.MovedAt) {
			t.Errorf("옮긴 원본을 %s 가 표지 내용과 함께 거절하지 않았다: %q", door, why)
		}
	}
	if again, err := VerifySQLite(ctx, src, dsn, VerifyOptions{}, quietLog()); err != nil || !again.Matched() {
		t.Fatalf("옮긴 원본과의 맞대기가 섰다: %v", err)
	}
	fresh, dst := importTarget(t)
	for _, dry := range []bool{true, false} {
		_, err := importLedger(t, ctx, src, fresh, TransferOptions{DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), "다시 옮기지 않는다") {
			t.Fatalf("예행 %v: 옮긴 원본을 다시 옮겼다: %v", dry, err)
		}
	}
	if n := targetRows(t, dst); n != 0 {
		t.Fatalf("거절했는데 새 대상에 %d 행", n)
	}
	if _, ok := ledgerMovedAt(t, dst); ok {
		t.Fatal("거절했는데 새 대상에 이관 기록이 남았다")
	}
}

// 원본 디렉토리에 표지를 남길 수 없으면 옮기기 전에(예행도) 거절하고 대상에 아무것도 쓰지 않는다.
func TestImportSQLiteRefusesWhenTheMarkerCannotBeLeft(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 는 디렉토리 권한을 넘는다")
	}
	ctx := context.Background()
	src := importSource(t, importSeed)
	dir := filepath.Dir(src)
	dsn, dst := importTarget(t)
	// 옆 파일이 미리 있으면 SQLite 는 읽기 전용 디렉토리에서도 원본을 연다(옆 파일에 쓴다) - 거절이 원본 열기가 아니라 표지
	// 자리에서 나야 한다.
	for _, p := range pg.Sidecars(src)[:2] {
		must(t, os.WriteFile(p, nil, 0o644))
	}
	must(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	for _, dry := range []bool{true, false} {
		_, err := importLedger(t, ctx, src, dsn, TransferOptions{DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), "이관 표지를 남길 수 없다") {
			t.Fatalf("예행 %v: 표지를 못 남기는 자리에서 옮겼다: %v", dry, err)
		}
	}
	if n := targetRows(t, dst); n != 0 {
		t.Fatalf("거절했는데 대상에 %d 행", n)
	}
	if _, err := os.Lstat(MovedMarker(src)); err == nil {
		t.Fatal("거절했는데 표지가 생겼다")
	}
}

// 커밋한 뒤 표지를 못 남기면 실패로 내되 커밋했다고 말하고 손으로 남기는 명령(touch)을 낸다.
func TestLeaveMovedMarkSaysTheCommitHappened(t *testing.T) {
	src := importSource(t, "")
	_, dst := importTarget(t)
	mark, err := reserveMovedMark(src)
	if err != nil {
		t.Fatal(err)
	}
	defer mark.release(quietLog())
	must(t, os.Remove(mark.tmp)) // 잡아 둔 자리가 사라졌다
	err = dst.leaveMovedMark(context.Background(), mark, time.Now(), 7, "")
	if err == nil || !strings.Contains(err.Error(), "7 행, 커밋했다") || !strings.Contains(err.Error(), "touch "+shellQuote(MovedMarker(src))) {
		t.Fatalf("표지를 못 남긴 실패가 커밋 · 손 처방을 안 말한다: %v", err)
	}
	if _, err := os.Lstat(MovedMarker(src)); err == nil {
		t.Fatal("실패했다면서 표지가 생겼다")
	}
}

// movedNoteAt 는 SQLite 원장 path 옆에 at 에 원장 ledger(빈 값이면 식별 없이)로 옮겼다고 적은 표지를 남긴다(이관이 남기는 꼴).
func movedNoteAt(t *testing.T, path string, at time.Time, ledger string) {
	t.Helper()
	raw, err := json.Marshal(movedNote{MovedAt: at.UTC().Format(time.RFC3339), Ledger: ledger, Schema: pg.ImportVersion})
	must(t, err)
	must(t, os.WriteFile(MovedMarker(path), raw, 0o644))
}

// ledgerMovedAt 은 Postgres 원장 s 의 이관 기록이다(없으면 false).
func ledgerMovedAt(t *testing.T, s *Store) (time.Time, bool) {
	t.Helper()
	at, ok, err := readMovedRecord(context.Background(), s.db)
	must(t, err)
	return at, ok
}

// stepsOf 는 Postgres 증분 가운데 인덱스만 더한 것(index)과 아닌 것(other) 하나씩이다.
func stepsOf(t *testing.T) (index, other Migration) {
	t.Helper()
	var hasIndex, hasOther bool
	for _, m := range pgMigrations {
		if _, ok := pg.IndexOnly(m.SQL); ok && !hasIndex {
			index, hasIndex = m, true
		} else if !ok && !hasOther {
			other, hasOther = m, true
		}
	}
	if !hasIndex || !hasOther {
		t.Fatalf("인덱스만 더한 증분(%v)과 아닌 증분(%v)이 다 있어야 한다", hasIndex, hasOther)
	}
	return index, other
}

// 되돌리기 창의 판정: 창을 연 때부터 RollbackWindow 동안 인덱스만 더한 것이 아닌 증분을 거절하고, 닫히는 때(그 순간 포함)부터는
// 받는다. 인덱스만 더한 증분은 창 안에서도 받는다(내림으로 옛 판에 닿는다). 거절 문구는 닫히는 때(UTC)와 넘는 플래그를 댄다.
func TestRollbackWindowRefusal(t *testing.T) {
	index, other := stepsOf(t)
	opened := time.Date(2026, 11, 27, 1, 2, 3, 0, time.FixedZone("KST", 9*3600))
	for _, c := range []struct {
		name   string
		now    time.Time
		steps  []Migration
		refuse bool
	}{
		{"창을 연 때", opened, []Migration{other}, true},
		{"닫히기 직전", opened.Add(RollbackWindow - time.Second), []Migration{index, other}, true},
		{"닫히는 때", opened.Add(RollbackWindow), []Migration{other}, false},
		{"창 밖", opened.Add(RollbackWindow + time.Hour), []Migration{other}, false},
		{"창 안의 인덱스만 더한 증분", opened.Add(time.Hour), []Migration{index}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := rollbackWindowRefusal("이관 표지 /d/fd.db.moved-to-postgres", opened, c.now, c.steps)
			if (err != nil) != c.refuse {
				t.Fatalf("거절 %v 여야 한다: %v", c.refuse, err)
			}
			if err == nil {
				return
			}
			for _, want := range []string{"2026-11-26T16:02:03Z 에 열렸고(이관 표지 /d/fd.db.moved-to-postgres)",
				"2026-11-29T16:02:03Z(UTC)에 닫힌다", "판 " + fmt.Sprint(other.To) + " 의 증분",
				"--allow-in-rollback-window", "README 「72시간 안에 되돌리기」"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("거절 문구에 %q 가 없다: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "판 "+fmt.Sprint(index.To)+" ") {
				t.Errorf("인덱스만 더한 증분까지 막는 판으로 댔다: %v", err)
			}
		})
	}
}

// 표지가 연 창은 표지 내용의 옮긴 때 · 원장 식별이다. 못 읽으면(손으로 touch 한 빈 표지 · 깨진 내용) 표지 파일의 고친 때로 재고
// 식별은 비운다. 표지가 없으면 창이 없다.
func TestMovedAtReadsTheNoteThenTheFileTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fd.db")
	if _, _, ok, err := movedAt(path); ok || err != nil {
		t.Fatalf("표지가 없는데 창이 열렸다: %v %v", ok, err)
	}
	at := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	movedNoteAt(t, path, at, "0123456789abcdef")
	if got, led, ok, err := movedAt(path); !ok || err != nil || !got.Equal(at) || led != "0123456789abcdef" {
		t.Fatalf("표지 내용의 옮긴 때 · 식별을 안 읽었다: %v %q %v %v", got, led, ok, err)
	}
	mtime := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	for _, body := range []string{"", `{"moved_at":"어제"}`} {
		must(t, os.WriteFile(MovedMarker(path), []byte(body), 0o644))
		must(t, os.Chtimes(MovedMarker(path), mtime, mtime))
		if got, led, ok, err := movedAt(path); !ok || err != nil || !got.Equal(mtime) || led != "" {
			t.Fatalf("내용 %q 의 표지가 파일의 고친 때로 안 쟀다: %v %q %v %v", body, got, led, ok, err)
		}
	}
}

// 이관 기록은 판 표의 주석이다: 다는 문장이 단 때를 읽고, 머리가 다른 주석 · 주석 없음은 기록이 아니며, 머리는 맞는데 때가 깨졌으면
// 오류다(정본이 깨졌다 - 창 밖으로 보지 않는다).
func TestMovedRecordRoundTrips(t *testing.T) {
	_, s := ledgerAtVersion(t, "postgres", pg.ImportVersion)
	if _, ok := ledgerMovedAt(t, s); ok {
		t.Fatal("기록을 안 단 원장에 기록이 있다")
	}
	pgExec(t, s, `COMMENT ON TABLE schema_version IS '사람이 단 설명'`)
	if _, ok := ledgerMovedAt(t, s); ok {
		t.Fatal("머리가 다른 주석을 기록으로 읽었다")
	}
	at := time.Date(2026, 11, 27, 1, 2, 3, 0, time.FixedZone("KST", 9*3600))
	pgExec(t, s, MovedRecordSQL(at))
	if got, ok := ledgerMovedAt(t, s); !ok || !got.Equal(at) {
		t.Fatalf("기록 %v %v, 기대 %v", got, ok, at)
	}
	pgExec(t, s, `COMMENT ON TABLE schema_version IS '`+movedRecordPrefix+`어제'`)
	if _, _, err := readMovedRecord(context.Background(), s.db); err == nil || !strings.Contains(err.Error(), "옮긴 때를 읽지 못했다") {
		t.Fatalf("깨진 기록이 오류가 아니다: %v", err)
	}
}

// Postgres 판올림이 되돌리기 창 안이면 인덱스만 더한 것이 아닌 증분을 얹지 않고 원장을 그 판에 둔다(덤프도 안 뜬다 - 판은 따로
// 잰다). 창의 정본은 원장의 이관 기록이다: 이 환경의 표지 자리(Moved)를 안 줘도(--db) 재고, 깨진 표지의 오래된 고친 때나 다른 때를
// 적은 표지로 닫히지 않는다. 기록이 없는 원장(옛 판이 옮겼다)만 표지로 잰다 - 표지가 적은 원장 식별이 이 원장일 때 · 식별이 없을
// 때만이다. 둘 다 없거나 창 밖이거나 플래그를 주면 올린다. 인덱스만 더한 증분은 창 안에서도 얹는다.
func TestPostgresUpgradeHoldsInTheRollbackWindow(t *testing.T) {
	ctx := context.Background()
	index, other := stepsOf(t)
	// 표지의 꼴: 없음 · 식별 없는 내용 · 이 원장 식별 · 다른 원장 식별 · 깨진 내용(고친 때만 남는다).
	const (
		noMark = iota
		markBare
		markSame
		markOther
		markBroken
	)
	for _, c := range []struct {
		name     string
		from, to int
		record   time.Duration // 원장의 이관 기록이 이만큼 앞이다(0 이면 기록이 없다)
		mark     int
		markAgo  time.Duration
		moved    bool // 이 환경의 표지 자리를 준다(--db 로 열어도 준다 - false 는 표지 자리를 모르는 실행기다)
		allow    bool
		refuse   bool
	}{
		{"기록 · 창 안 · 표지 자리 없음", other.To - 1, other.To, time.Hour, noMark, 0, false, false, true},
		{"기록 · 창 안 · 표지 없음", other.To - 1, other.To, time.Hour, noMark, 0, true, false, true},
		{"기록 · 창 안 · 깨진 표지의 고친 때는 창 밖", other.To - 1, other.To, time.Hour, markBroken, RollbackWindow + 30*time.Hour, true, false, true},
		{"기록 · 창 안 · 표지는 창 밖", other.To - 1, other.To, time.Hour, markBare, RollbackWindow + time.Hour, true, false, true},
		{"기록 · 창 밖 · 표지는 창 안", other.To - 1, other.To, RollbackWindow + time.Hour, markBare, time.Hour, true, false, false},
		{"기록 · 창 밖", other.To - 1, other.To, RollbackWindow + time.Hour, noMark, 0, false, false, false},
		{"기록 · 창 안 · 플래그", other.To - 1, other.To, time.Hour, noMark, 0, true, true, false},
		{"기록 · 창 안 · 인덱스만 더한 증분", index.To - 1, index.To, time.Hour, noMark, 0, true, false, false},
		{"기록 없음 · 표지 창 안", other.To - 1, other.To, 0, markBare, time.Hour, true, false, true},
		{"기록 없음 · 이 원장의 표지 창 안", other.To - 1, other.To, 0, markSame, time.Hour, true, false, true},
		{"기록 없음 · 다른 원장의 표지 창 안", other.To - 1, other.To, 0, markOther, time.Hour, true, false, false},
		{"기록 없음 · 깨진 표지의 고친 때 창 안", other.To - 1, other.To, 0, markBroken, time.Hour, true, false, true},
		{"기록 없음 · 표지 창 밖", other.To - 1, other.To, 0, markBare, RollbackWindow + time.Hour, true, false, false},
		{"기록 없음 · 표지 창 안 · 플래그", other.To - 1, other.To, 0, markBare, time.Hour, true, true, false},
		{"기록 없음 · 표지 없음", other.To - 1, other.To, 0, noMark, 0, true, false, false},
		{"기록 없음 · 표지 창 안 · 표지 자리 없음", other.To - 1, other.To, 0, markBare, time.Hour, false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, s := ledgerAtVersion(t, "postgres", c.from)
			if c.record != 0 {
				pgExec(t, s, MovedRecordSQL(time.Now().Add(-c.record)))
			}
			lite := filepath.Join(t.TempDir(), "fd.db")
			at := time.Now().Add(-c.markAgo)
			switch c.mark {
			case markBare:
				movedNoteAt(t, lite, at, "")
			case markSame, markOther:
				led, err := readLedgerFacts(ctx, s.db)
				must(t, err)
				if c.mark == markOther {
					led.ID = "fedcba9876543210"
				}
				movedNoteAt(t, lite, at, led.ID)
			case markBroken:
				must(t, os.WriteFile(MovedMarker(lite), []byte(`{"moved_at":`), 0o644))
				must(t, os.Chtimes(MovedMarker(lite), at, at))
			}
			opt := MigrateOptions{Target: c.to, BackupDir: t.TempDir(), AllowInRollbackWindow: c.allow}
			if c.moved {
				opt.Moved = lite
			}
			m, run := pgMigrator(t, path, opt)
			dumped := false
			run.backup = func(context.Context, *migrateSession, int) (string, error) {
				dumped = true
				return "시험 덤프", nil
			}
			err := m.migrateTo(ctx, run)
			if (err != nil) != c.refuse {
				t.Fatalf("거절 %v 여야 한다: %v", c.refuse, err)
			}
			want := c.to
			if c.refuse {
				want = c.from
				if !strings.Contains(err.Error(), "되돌리기 창 안이다") || dumped {
					t.Fatalf("창의 거절이 아니거나 거절 전에 덤프를 떴다(%v): %v", dumped, err)
				}
				src := "이 원장에 이관이 남긴 기록"
				if c.record == 0 {
					src = "이관 표지 " + MovedMarker(lite)
				}
				if !strings.Contains(err.Error(), src) {
					t.Fatalf("창을 연 출처(%s)를 안 댔다: %v", src, err)
				}
			}
			if v := pgVersion(t, s); v != want {
				t.Fatalf("판이 %d 다 - %d 여야 한다", v, want)
			}
		})
	}
}

// 원장의 이관 기록이 깨졌으면(머리는 맞는데 때를 못 읽는다) 창 안인지 모르므로 올리지 않는다 - 표지로 물러서지 않는다. 플래그는 넘는다.
func TestPostgresUpgradeHoldsOnABrokenMovedRecord(t *testing.T) {
	_, other := stepsOf(t)
	for _, allow := range []bool{false, true} {
		path, s := ledgerAtVersion(t, "postgres", other.To-1)
		pgExec(t, s, `COMMENT ON TABLE schema_version IS '`+movedRecordPrefix+`어제'`)
		m, run := pgMigrator(t, path, MigrateOptions{Target: other.To, BackupDir: t.TempDir(), AllowInRollbackWindow: allow})
		run.backup = func(context.Context, *migrateSession, int) (string, error) { return "시험 덤프", nil }
		err := m.migrateTo(context.Background(), run)
		if allow {
			if err != nil || pgVersion(t, s) != other.To {
				t.Fatalf("플래그로도 안 올렸다: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "되돌리기 창 안인지 모르므로") || pgVersion(t, s) != other.To-1 {
			t.Fatalf("깨진 기록 위에서 올렸거나 다른 오류다(판 %d): %v", pgVersion(t, s), err)
		}
	}
}
