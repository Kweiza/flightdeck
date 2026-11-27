package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// 이관 표지: SQLite 원장 옆의 <원장>.moved-to-postgres 는 「이 원장은 Postgres 로 옮겼다」는 뜻이다. 이관(ImportSQLite)이
// 커밋한 뒤 원본 옆에 남기고, 두 엔진의 관문이 이것으로 한 원장이 두 갈래로 쓰이는 길(갈린 원장)을 막는다.
//
//   - SQLite 쪽(fd migrate · fd serve · fd selfcheck · fd migrate --rollback)은 표지가 있으면 원장 파일이 있든 없든 열지도 새로
//     만들지도 않는다(movedRefusal). 덮개 파일을 빼고 띄운 compose 가 SQLite 원장을 새로 만들어 쓰기 시작하는 길이다.
//   - Postgres 판올림(fd migrate)은 이 환경의 SQLite 원장 자리에 표지 없는 원장이 남아 있으면 거절한다(UnmovedSQLiteRefusal).
//     덮개 파일을 얹고 이관 없이 띄운 compose 가 빈 Postgres 원장을 만들어 쓰기 시작하는 길이다.
//
// ★ 판정은 표지가 **있는가**뿐이다. 내용(movedNote)은 사람과 거절 문구를 위한 것이라 못 읽어도 표지다 - 손으로 `touch` 한 빈
// 파일도 표지다(이관이 커밋 뒤 표지를 못 남겼을 때의 처방).
// ★ 표지 자리를 재지 못한 것(없음 말고 - 권한 · 입출력)은 두 관문 다 거절이다. 옮겼는지 모르는 채로 어느 쪽 원장도 열지 않는다.
// ★ 지우는 길은 README 절차의 손 `rm` 뿐이고, 그 절차는 둘이다: 「72시간 안에 되돌리기」(`fd migrate export-sqlite` 가 낸 파일을
// 원장 자리에 둔 뒤), 그리고 선택 파일을 켜기 전에 선 업그레이드를 거둘 때(「켜기 전에 섰을 때」 - 양쪽 서버를 내린 채 Postgres
// 원장이 이 SQLite 원장과 맞을 때만. 서버가 Postgres 에 쓴 적이 없으니 이 원장이 곧 Postgres 의 것이다). 표지를 넘는 플래그는 두지
// 않는다 - 관문을 넘는 길이 코드에 하나 더 있으면 그 길은 절차의 대조를 건너뛴다.
// ★ 읽기만 하는 길(이관 원본 · `fd migrate verify` 의 원본)은 표지를 보지 않는다. 옮긴 뒤의 맞대기가 바로 그 원장을 읽는다.

// movedSuffix 는 표지 이름의 꼬리다.
const movedSuffix = ".moved-to-postgres"

// MovedMarker 는 SQLite 원장 path 의 이관 표지 자리다. 순수 함수다.
func MovedMarker(path string) string { return path + movedSuffix }

// movedNote 는 표지 파일의 내용(JSON 한 덩이)이다. 판정에는 쓰지 않는다(머리말의 ★).
type movedNote struct {
	Note    string `json:"note"`
	MovedAt string `json:"moved_at"`
	// Ledger 는 옮겨 간 Postgres 원장의 식별이다(ledgerIDSQL - 판올림 전 덤프 이름의 <원장>과 같은 값). 못 읽었으면 비었다.
	Ledger string `json:"ledger,omitempty"`
	Schema int    `json:"schema"`
	Rows   int    `json:"rows"`
	Undo   string `json:"undo"`
}

// movedNoteLimit 는 거절 문구에 실으려고 표지에서 읽는 바이트 상한이다. 이관이 쓰는 내용은 그보다 작다.
const movedNoteLimit = 4 << 10

// movedMarkMode 는 표지 파일을 만들 때 주는 모드다(umask 가 거른다). 서버가 다른 uid 여도 읽는다.
const movedMarkMode fs.FileMode = 0o644

// statMoved 는 SQLite 원장 path 의 표지를 잰다. 있으면 그 정보, 없으면 nil 이고, 재지 못한 것은 오류다.
func statMoved(path string) (fs.FileInfo, error) {
	fi, err := os.Lstat(MovedMarker(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return fi, err
}

// movedRefusal 은 SQLite 원장 path 를 표지 때문에 열지도 만들지도 않는 사유다. 표지가 없으면 nil 이다.
func movedRefusal(path string) error {
	mark := MovedMarker(path)
	fi, err := statMoved(path)
	switch {
	case err != nil:
		return fmt.Errorf("SQLite 원장 %s 의 이관 표지(%s)를 재지 못했다 - Postgres 로 옮긴 원장인지 모르므로 열지도 새로 "+
			"만들지도 않는다: %w", clip(path, 200), clip(mark, 200), err)
	case fi == nil:
		return nil
	}
	return fmt.Errorf("SQLite 원장 %s 은 Postgres 로 옮겼다(이관 표지 %s%s) - 이 원장을 열지도 새로 만들지도 않는다. 옮긴 원장은 "+
		"Postgres 덮개 파일을 얹어 띄운다(README 「Postgres 로 띄우기」). 72시간 안에 SQLite 판으로 되돌릴 때만 "+
		"`fd migrate export-sqlite` 가 낸 파일을 이 자리에 두고 이 표지를 지운(rm) 뒤 띄운다(README 「72시간 안에 되돌리기」 - "+
		"선택 파일을 켜기 전에 선 업그레이드라면 「켜기 전에 섰을 때」)",
		clip(path, 200), clip(mark, 200), describeMoved(mark, fi))
}

// describeMoved 는 표지 내용에서 옮긴 때 · 원장 · 행 수를 읽어 거절 문구에 붙일 꼬리를 낸다. 못 읽으면 빈 값이다 - 내용은
// 판정이 아니다. 보통 파일만 연다(FIFO 를 열면 쓰는 쪽이 올 때까지 선다).
func describeMoved(mark string, fi fs.FileInfo) string {
	if !fi.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(mark)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, movedNoteLimit))
	var n movedNote
	if err != nil || json.Unmarshal(raw, &n) != nil || n.MovedAt == "" {
		return ""
	}
	s := " · " + clip(n.MovedAt, 40) + " 에 "
	if n.Ledger != "" {
		s += "Postgres 원장 " + clip(n.Ledger, 40) + " 로 "
	}
	return s + strconv.Itoa(n.Rows) + " 행"
}

// UnmovedSQLiteRefusal 은 Postgres 원장을 판올림(fd migrate)하려는데 이 환경의 SQLite 원장 자리 sqlite 에 표지 없는 원장이
// 남아 있어 거절하는 사유다. 받으면 nil 이다. to 는 판올림의 목표 판(--to, 0 이면 최신)이다.
//
// ★ 판(빈 클러스터의 기준선 · 그 위 증분)과 상관없이 잰다. 덮개 파일만 얹고 이관 없이 띄운 compose 가 빈 클러스터에 기준선을
// 까는 날이 본체지만, 기준선을 깐 뒤 이관 전에 띄운 `up` 도, 사본을 옮겨(--from 사본) 원본에 표지가 없는 것도 같은 갈림이다 -
// 그 SQLite 원장이 다시 열리면 두 원장에 따로 쓰인다.
// ★ 이관 판까지만 까는 단계(--to pg.ImportVersion - 기준선과 그 위 증분)는 받는다. 이관이 그 위에서만 돌고(README 「업그레이드: SQLite 원장을 Postgres
// 로」), 그 단계는 서버를 띄우지 않는다.
func UnmovedSQLiteRefusal(sqlite string, to int) error {
	if to == pg.ImportVersion {
		return nil
	}
	_, err := os.Lstat(sqlite)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("SQLite 원장 자리(%s)를 재지 못했다 - 이관을 거치지 않은 원장이 남았는지 모르므로 Postgres 원장을 "+
			"올리지 않는다: %w", clip(sqlite, 200), err)
	}
	mark := MovedMarker(sqlite)
	fi, err := statMoved(sqlite)
	switch {
	case err != nil:
		return fmt.Errorf("SQLite 원장 %s 의 이관 표지(%s)를 재지 못했다 - 옮긴 원장인지 모르므로 Postgres 원장을 올리지 "+
			"않는다: %w", clip(sqlite, 200), clip(mark, 200), err)
	case fi != nil:
		return nil
	}
	return fmt.Errorf("SQLite 원장 %s 이 이관 표지(%s) 없이 남아 있다 - 이관을 거치지 않은 새 원장이다. 이대로 Postgres 원장을 "+
		"올려 띄우면 두 원장에 따로 쓰이므로 올리지 않는다. SQLite 원장을 들고 옮기려면 README 「업그레이드: SQLite 원장을 "+
		"Postgres 로」를 따른다(이관 판까지만 까는 `fd migrate --to %d` 는 받는다). 그 원장을 버리고 Postgres 로 새로 시작하려면 그 "+
		"파일을 원장 자리 밖으로 옮긴다. 이미 옮겼는데 표지만 없으면(사본을 옮겼거나 이관이 표지를 못 남겼다) "+
		"`fd migrate verify --from %s` 로 맞는지 본 뒤 표지를 남긴다(touch). 두 원장에 이미 따로 쓰였으면 두 서버를 다 내리고 "+
		"두 원장을 지우지 말고 맞대 본다",
		clip(sqlite, 200), clip(mark, 200), pg.ImportVersion, clip(sqlite, 200))
}

// movedMark 는 이관이 원본 옆에 표지를 남기려고 미리 잡은 자리다(reserveMovedMark).
type movedMark struct {
	path string // 표지 자리
	tmp  string // 같은 디렉토리의 임시 파일 - publish 가 내용을 쓰고 표지 이름으로 바꾼다
}

// reserveMovedMark 는 SQLite 원본 from 옆에 표지를 남길 수 있는지 커밋 **전에** 재고 그 자리를 잡는다. 표지가 이미 있으면
// (옮긴 원본이다) 거절하고, 원본 디렉토리에 임시 파일을 못 만들면 거절한다 - 옮긴 뒤 표지를 못 남기면 그 원장에 SQLite 쪽 관문이
// 서지 않는다. 예행도 잰다(본 실행이 받을 거절을 예행이 먼저 받는다). 잡은 자리는 release 로 놓는다.
func reserveMovedMark(from string) (*movedMark, error) {
	if err := movedRefusal(from); err != nil {
		return nil, fmt.Errorf("다시 옮기지 않는다: %w", err)
	}
	f, err := createExclusive(filepath.Dir(from), "."+filepath.Base(from)+movedSuffix+"-", movedMarkMode)
	if err != nil {
		return nil, fmt.Errorf("원본 디렉토리에 이관 표지를 남길 수 없다(%s 에 쓸 수 있어야 한다) - 옮긴 뒤 표지가 없으면 SQLite "+
			"판이 옛 원장을 다시 열 수 있으므로 옮기지 않는다: %w", clip(filepath.Dir(from), 200), err)
	}
	m := &movedMark{path: MovedMarker(from), tmp: f.Name()}
	if err := f.Close(); err != nil {
		m.release(slog.Default())
		return nil, fmt.Errorf("이관 표지의 임시 파일을 닫지 못했다 - 옮기지 않는다: %w", err)
	}
	return m, nil
}

// publish 는 커밋한 이관의 표지를 남긴다: 잡아 둔 임시 파일에 note 를 쓰고 굳힌 뒤 표지 이름으로 바꾸고 디렉토리를 굳힌다
// (syncDir). 디렉토리를 굳히기 전에는 전원이 나가면 이름이 사라질 수 있다.
func (m *movedMark) publish(note movedNote) error {
	raw, err := json.MarshalIndent(note, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(m.tmp, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(m.tmp, m.path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(m.path))
}

// release 는 잡아 둔 임시 파일을 지운다. 표지 이름으로 바꾼 뒤면 지울 것이 없다.
func (m *movedMark) release(log *slog.Logger) {
	if err := os.Remove(m.tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("이관 표지의 임시 파일을 지우지 못했다", "path", m.tmp, "error", err.Error())
	}
}

// leaveMovedMark 는 이 Postgres 원장으로 rows 행을 옮겨 커밋한 이관의 표지를 mark 자리에 남긴다.
//
// ★ 커밋한 뒤에만 부른다. 못 남기면 실패로 내되 커밋은 이미 됐다고 말하고 손으로 남기는 길(touch)을 같이 낸다 - 그대로 두면
// SQLite 쪽 관문이 이 원장에 안 서고, Postgres 판올림은 표지 없는 원장을 보고 선다(UnmovedSQLiteRefusal).
func (s *Store) leaveMovedMark(ctx context.Context, mark *movedMark, rows int) error {
	if err := mark.publish(s.movedNoteFor(ctx, rows)); err != nil {
		return fmt.Errorf("옮겼다(%d 행, 커밋했다) - 그런데 원본 옆에 이관 표지를 남기지 못했다: %w. 서버를 띄우기 전에 손으로 "+
			"남긴다: touch %s", rows, err, shellQuote(mark.path))
	}
	return nil
}

// movedNoteFor 는 Postgres 원장 s 로 rows 행을 옮긴 이관의 표지 내용이다. 원장 식별은 못 읽으면 비운다 - 커밋은 이미 끝났고
// 표지는 내용 없이도 표지다(머리말의 ★). 취소된 ctx 여도 표지는 남겨야 하므로 식별 읽기만 ctx 를 탄다.
func (s *Store) movedNoteFor(ctx context.Context, rows int) movedNote {
	n := movedNote{
		Note: "이 SQLite 원장은 Postgres 원장으로 옮겼다. 이 파일이 있는 동안 SQLite 판(fd migrate · fd serve)은 이 원장을 " +
			"열지도 새로 만들지도 않는다.",
		MovedAt: time.Now().UTC().Format(time.RFC3339),
		Schema:  pg.ImportVersion,
		Rows:    rows,
		Undo: "72시간 안에 되돌리려면 README 「72시간 안에 되돌리기」: fd migrate export-sqlite 가 낸 파일을 이 원장 자리에 두고 " +
			"이 파일을 지운다(rm). 선택 파일을 켜기 전에 선 업그레이드를 거두려면 README 「켜기 전에 섰을 때」.",
	}
	if err := s.ReadTx(ctx, func(q Reader) error {
		led, err := readLedgerFacts(ctx, q)
		n.Ledger = led.ID
		return err
	}); err != nil {
		n.Ledger = ""
	}
	return n
}
