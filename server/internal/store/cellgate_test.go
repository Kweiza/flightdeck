package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/nulesc"
	"github.com/kweiza/flightdeck/internal/store/pg"
)

// 원장 칸 관문(cellgate.go)을 문장 하나 · 인자 하나 단위로 잰다. 쓰기 길 전체(두 엔진 · HTTP)는 api 의 nul_test.go 가 잰다.

// slotsOf 는 꼴의 자리표시마다 「인자가 담기는 칸 이름 / 행 좌표」다.
func slotsOf(sh *cellShape) []string {
	var out []string
	for _, sl := range sh.slots {
		col := ""
		if sl.col != nil {
			col = sl.col.Name
		}
		out = append(out, col+"/"+sl.coord)
	}
	return out
}

func TestShapeOfReadsWhereEachArgumentLands(t *testing.T) {
	cases := []struct {
		name, q string
		table   string
		slots   []string // nil 이면 칸에 쓰지 않는 문장(read)이다
	}{
		{"INSERT · 충돌 갱신", `INSERT INTO item(project, id, title, labels, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(project, id) DO UPDATE SET title = excluded.title, body = ?, state = 'open' WHERE item.state = ?`,
			"item", []string{"project/project", "id/id", "title/", "labels/", "created_at/", "body/", "/"}},
		{"INSERT · 여러 행", `INSERT INTO item_after(project, item_id, dep_item, dep_job, dep_sha, dep_project) VALUES (?, ?, ?, NULL, NULL, ?), (?, ?, ?, NULL, NULL, ?)`,
			"item_after", []string{"project/project", "item_id/item_id", "dep_item/dep_item", "dep_project/dep_project", "project/project", "item_id/item_id", "dep_item/dep_item", "dep_project/dep_project"}},
		{"UPDATE · 조회 조건은 좌표", `UPDATE item SET title = ?, close_reason = NULL, closed_at = ? WHERE project = ? AND id = ? AND state IN ('open', 'claimed')`,
			"item", []string{"title/", "closed_at/", "/project", "/id"}},
		{"글 안의 ? 는 자리표시가 아니다", `UPDATE item SET title = '?', body = ? WHERE project = ? AND id = ?`,
			"item", []string{"body/", "/project", "/id"}},
		{"수 칸의 식은 칸에 안 묶는다", `UPDATE counter SET value = value + ? WHERE project = ? AND name = ?`,
			"counter", []string{"/", "/project", "/name"}},
		{"수 · 바이트 칸도 칸에 묶는다", `INSERT INTO idempotency(key, fingerprint, status, ctype, body, at) VALUES (?, ?, ?, ?, ?, ?)`,
			"idempotency", []string{"key/key", "fingerprint/", "status/", "ctype/", "body/", "at/"}},
		// 따옴표 없는 SQL 이름은 대소문자를 안 가린다 - 카탈로그의 칸으로 묶는다.
		{"대소문자 변형", `insert into ITEM(Project, ID, Title) values (?, ?, ?)`, "item", []string{"project/project", "id/id", "title/"}},
		{"UPDATE 대소문자 변형", `update Claim set Force_Reason = ? where Project = ? and ITEM_ID = ?`,
			"claim", []string{"force_reason/", "/Project", "/ITEM_ID"}},
		// 옮기지 않는 관리 표는 칸 규칙이 없다 - 인자는 다 칸에 안 묶인 이름이다.
		{"관리 표", `INSERT INTO sqlite_sequence(name, seq) VALUES (?, ?)`, "sqlite_sequence", []string{"/", "/"}},
		{"조회", `SELECT title FROM item WHERE id = ?`, "", nil},
		{"주석 뒤의 조회", "-- 머리\n /* 주석 */ select title FROM item WHERE id = ?", "", nil},
		{"지우기", `DELETE FROM claim WHERE project = ? AND item_id = ?`, "", nil},
		{"저장점", `SAVEPOINT fd_sp_1`, "", nil},
		{"쓰기 없는 WITH", `WITH x AS (SELECT id FROM item WHERE project = ?) SELECT id FROM x`, "", nil},
	}
	for _, c := range cases {
		sh, err := shapeOf(c.q)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if c.slots == nil {
			if !sh.read || sh.slots != nil {
				t.Errorf("%s: 꼴 %+v, 바라는 것 칸에 쓰지 않는 문장", c.name, sh)
			}
			continue
		}
		if sh.read || sh.table != c.table || !reflect.DeepEqual(slotsOf(sh), c.slots) {
			t.Errorf("%s: 꼴 %s %v, 바라는 것 %s %v", c.name, sh.table, slotsOf(sh), c.table, c.slots)
		}
	}
}

// 글 칸의 값이 자리표시 하나가 아니라 식이면(관문이 칸에 담길 바이트를 모른다) 인자와 상관없이 늘 오류다.
func TestShapeOfRefusesAnExpressionIntoATextCell(t *testing.T) {
	for _, q := range []string{
		`UPDATE item SET title = COALESCE(?, title) WHERE project = ? AND id = ?`,
		`INSERT INTO item(project, id, title) VALUES (?, ?, ? || '끝')`,
		`INSERT INTO claim(project, item_id, session_id) VALUES (?, ?, ?) ON CONFLICT(project, item_id) DO UPDATE SET force_reason = 'r:' || ?`,
	} {
		if _, err := shapeOf(q); err == nil || !strings.Contains(err.Error(), "식이다") {
			t.Errorf("%s: %v, 바라는 것 「식이다」 오류", q, err)
		}
	}
}

// 관문은 인자가 닿는 자리를 모르는 문장을 보내지 않는다(fail-closed) - 칸에 안 묶인 채 NUL 을 Postgres 까지 보내는 대신 처음 도는
// 자리에서 늘 오류다. 꼴 오류는 입력의 잘못이 아니다(*CellRefusal 이 아니다 - 500).
func TestShapeOfFailsClosed(t *testing.T) {
	for _, c := range []struct{ name, q, says string }{
		{"쓰기를 품은 WITH", `WITH x AS (SELECT 1) UPDATE item SET title = ? WHERE project = ? AND id = ?`, "WITH 가 쓰기"},
		{"WITH 안의 INSERT", `WITH x AS (INSERT INTO judgment(id, at, kind, body) VALUES (?, ?, ?, ?) RETURNING id) SELECT id FROM x`, "WITH 가 쓰기"},
		{"위치 매개변수", `UPDATE item SET title = $1 WHERE project = $2 AND id = $3`, "위치 매개변수"},
		{"카탈로그 밖 표", `INSERT INTO nosuch(a) VALUES (?)`, "카탈로그에 없다"},
		{"카탈로그 밖 칸(UPDATE)", `UPDATE item SET titel = ? WHERE project = ? AND id = ?`, "칸 \"titel\" 가 없다"},
		{"카탈로그 밖 칸(INSERT)", `INSERT INTO item(project, id, titel) VALUES (?, ?, ?)`, "칸 \"titel\" 가 없다"},
		{"카탈로그 밖 칸(충돌 갱신)", `INSERT INTO item(project, id) VALUES (?, ?) ON CONFLICT(project, id) DO UPDATE SET bdy = ?`, "칸 \"bdy\" 가 없다"},
		{"따옴표 식별자", `UPDATE "item" SET title = ? WHERE id = ?`, "이름이 와야 한다"},
		{"스키마 붙은 표", `UPDATE public.item SET title = ? WHERE id = ?`, "카탈로그에 없다"},
		{"INSERT … SELECT", `INSERT INTO item(project, id, title) SELECT ?, ?, ?`, "VALUES"},
		{"INSERT OR IGNORE", `INSERT OR IGNORE INTO item(project, id, title) VALUES (?, ?, ?)`, "INTO"},
		{"모르는 첫 낱말", `REPLACE INTO item(project, id, title) VALUES (?, ?, ?)`, "모르는 문장"},
		{"낱말로 시작하지 않는 문장", `(SELECT 1)`, "모르는 문장"},
	} {
		_, err := shapeOf(c.q)
		var ref *CellRefusal
		if err == nil || !strings.Contains(err.Error(), c.says) || errors.As(err, &ref) {
			t.Errorf("%s: %v, 바라는 것 「%s」 오류", c.name, err, c.says)
		}
		// 두 번째도 같은 오류다(기억한 꼴).
		if _, again := shapeOf(c.q); (again == nil) != (err == nil) {
			t.Errorf("%s: 다시 읽은 꼴이 갈렸다: %v · %v", c.name, err, again)
		}
	}
	// 인자 꼴: 자리표시 수와 다른 인자(이름 붙은 매개변수 `:이름` - 자리표시가 없다), sql.Named 인자, 드라이버 값으로 못 옮기는 인자.
	for _, c := range []struct {
		name, q string
		args    []any
		says    string
	}{
		{"이름 붙은 매개변수", `UPDATE item SET title = :t WHERE project = :p AND id = :i`, []any{"t\x00", "p", "i"}, "자리표시 `?` 0 개에 인자 3 개"},
		{"자리표시보다 많은 인자", `UPDATE item SET title = ? WHERE project = ? AND id = :i`, []any{"t\x00", "p", "i"}, "자리표시 `?` 2 개에 인자 3 개"},
		{"쓰기의 sql.Named", `UPDATE item SET title = ? WHERE project = ? AND id = ?`, []any{sql.Named("t", "t\x00"), "p", "i"}, "이름 붙은 인자"},
		{"못 옮기는 인자", `UPDATE item SET title = ? WHERE project = ? AND id = ?`, []any{[]string{"t"}, "p", "i"}, "인자 1 를 읽지 못한다"},
	} {
		_, _, err := gateArgs(c.q, c.args)
		var ref *CellRefusal
		if err == nil || !strings.Contains(err.Error(), c.says) || errors.As(err, &ref) {
			t.Errorf("%s: %v, 바라는 것 「%s」 오류", c.name, err, c.says)
		}
	}
}

type labelText string

func TestCellGateAppliesTheImportRule(t *testing.T) {
	const upd = `UPDATE item SET title = ?, labels = ? WHERE project = ? AND id = ?`
	title := "제목\x00"
	args := []any{&title, `["l\u0000","\\x00"]`, "p", "i1"}
	orig := append([]any(nil), args...)
	out, pend, err := gateArgs(upd, args)
	if err != nil {
		t.Fatal(err)
	}
	if out[0] != nulesc.Escape(title) || out[1] != `["l\\x00","\\x00"]` || out[2] != "p" || out[3] != "i1" {
		t.Errorf("바꾼 인자 %q", out)
	}
	if !reflect.DeepEqual(args, orig) {
		t.Errorf("부른 쪽의 인자를 고쳤다: %q", args)
	}
	want := []cellRewrite{
		{table: "item", column: "title", rule: pg.RewriteNUL, row: `project="p" id="i1"`},
		{table: "item", column: "labels", rule: pg.RewriteJSONNUL, pointers: []string{"/0"}, row: `project="p" id="i1"`},
	}
	if !reflect.DeepEqual(pend, want) {
		t.Errorf("바꾼 칸 %+v, 바라는 것 %+v", pend, want)
	}

	// 이름 붙은 글 타입 · sql.NullString · 글 칸에 든 바이트도 글이다(드라이버가 받는 값으로 잰다).
	for _, v := range []any{labelText("a\x00"), sql.NullString{String: "a\x00", Valid: true}, []byte("a\x00")} {
		out, _, err := gateArgs(`UPDATE item SET body = ? WHERE project = ? AND id = ?`, []any{v, "p", "i1"})
		if err != nil || out[0] != `a\x00` {
			t.Errorf("%T: %q %v", v, out, err)
		}
	}

	// 바꿀 것이 없으면 받은 조각 그대로다(새로 안 만든다).
	clean := []any{"t", `["a"]`, "p", "i1"}
	if out, pend, err := gateArgs(upd, clean); err != nil || &out[0] != &clean[0] || pend != nil {
		t.Errorf("바꿀 것 없는 인자를 새로 만들었다: %v %v", pend, err)
	}
	// 바이트 칸은 어떤 바이트든 담는다 - 재지 않는다.
	if _, _, err := gateArgs(`INSERT INTO idempotency(key, fingerprint, status, ctype, body, at) VALUES (?, ?, ?, ?, ?, ?)`,
		[]any{"k", "f", 200, "application/json", []byte("\x00\xff"), "t"}); err != nil {
		t.Errorf("바이트 칸의 NUL: %v", err)
	}
}

func TestCellGateRefusesWhatTheImportRefuses(t *testing.T) {
	cases := []struct {
		name, q string
		args    []any
		place   string
		kind    error
		where   string
	}{
		{"식별자 칸의 NUL", `INSERT INTO item(project, id, title) VALUES (?, ?, ?)`, []any{"p", "i\x00", "t"}, "원장 칸 item.id", nulesc.ErrNUL, `id="i\x00"`},
		{"글 칸의 깨진 UTF-8", `UPDATE item SET body = ? WHERE project = ? AND id = ?`, []any{"a\xff", "p", "i1"}, "원장 칸 item.body", nulesc.ErrUTF8, `id="i1"`},
		{"JSON 칸의 짝 없는 서로게이트", `UPDATE item SET labels = ? WHERE project = ? AND id = ?`, []any{`["\ud800"]`, "p", "i1"}, "원장 칸 item.labels", nulesc.ErrSurrogate, `id="i1" JSON "/0"`},
		{"JSON 키의 U+0000", `INSERT INTO event(at, project, session_id, kind, payload) VALUES (?, ?, ?, ?, ?)`,
			[]any{"x", "p", nil, "k", `{"\u0000":1}`}, "원장 칸 event.payload", nulesc.ErrKeyNUL, `JSON "/\x00"`},
		// 칸에 담기지 않는 글은 이름 규칙이다: 쓰기 문장의 조회 조건, 조회 · 지우기 문장의 인자(sql.Named 도 값을 잰다), 수 칸에 든 글.
		{"조회 조건의 NUL", `UPDATE item SET title = ? WHERE project = ? AND id = ?`, []any{"t", "p", "i\x00"}, "UPDATE item 문장의 인자 3(id)", nulesc.ErrNUL, `id="i\x00"`},
		{"충돌 갱신 조건의 깨진 UTF-8", `INSERT INTO item(project, id, title) VALUES (?, ?, ?) ON CONFLICT(project, id) DO UPDATE SET title = excluded.title WHERE item.state = ?`,
			[]any{"p", "i", "t", "o\xff"}, "INSERT item 문장의 인자 4", nulesc.ErrUTF8, `project="p" id="i"`},
		{"조회의 NUL", `SELECT id FROM item WHERE project = ? AND title = ?`, []any{"p", "a\x00"}, "SELECT 문장의 인자 2", nulesc.ErrNUL, ""},
		{"조회의 sql.Named", `SELECT id FROM item WHERE title = @t`, []any{sql.Named("t", "a\x00")}, "SELECT 문장의 인자 1", nulesc.ErrNUL, ""},
		{"지우기의 깨진 UTF-8", `DELETE FROM claim WHERE project = ? AND item_id = ?`, []any{"p\xff", "i"}, "DELETE 문장의 인자 1", nulesc.ErrUTF8, ""},
		{"수 칸의 글", `UPDATE counter SET value = ? WHERE project = ? AND name = ?`, []any{"7\x00", "p", "n"}, "원장 칸 counter.value", nulesc.ErrNUL, ""},
	}
	for _, c := range cases {
		_, pend, err := gateArgs(c.q, c.args)
		var ref *CellRefusal
		if !errors.As(err, &ref) || ref.Place() != c.place || !errors.Is(err, c.kind) || !strings.Contains(ref.Where, c.where) {
			t.Errorf("%s: %v (%+v), 바라는 것 %s 의 %v · 자리 %q", c.name, err, ref, c.place, c.kind, c.where)
		}
		if pend != nil {
			t.Errorf("%s: 거절했는데 바꾼 칸을 냈다 %+v", c.name, pend)
		}
	}
	// 수 칸의 맑은 글은 지나간다(드라이버가 옮긴다).
	if _, _, err := gateArgs(`UPDATE counter SET value = ? WHERE project = ? AND name = ?`, []any{"7", "p", "n"}); err != nil {
		t.Errorf("수 칸의 글 인자: %v", err)
	}
}

// 바꿔 넣기는 원장에 닿은 문장의 것만 로그로 낸다: 영향 행이 0 인 UPDATE(선점 없는 항목의 강제 반납), 행을 안 돌려준
// INSERT … RETURNING(insertUnlessTaken 의 충돌), 실패한 문장, 저장점까지 되돌린 문장의 바꿔 넣기는 버린다. 두 엔진 모두.
func TestCellGateLogsOnlyWhatReachedTheLedger(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		mustItem(t, s, "p", "i1")
		var logged bytes.Buffer
		s.log = slog.New(slog.NewTextHandler(&logged, nil))
		lines := func() int { return strings.Count(logged.String(), "원장 칸의 NUL 을") }
		ins := `INSERT INTO item(project, id, title, body, paths, labels, created_at) VALUES (?, ?, ?, ?, '[]', '[]', ?)`
		at := fmtTime(nowStamp())
		for _, c := range []struct {
			name string
			body func(t *Tx) error
			want int // 이 트랜잭션이 남길 줄
		}{
			{"0 행 UPDATE", func(t *Tx) error {
				if err := t.ForceReleaseClaim("p", "i1", "사유\x00"); !errors.Is(err, ErrNotFound) {
					return fmt.Errorf("선점 없는 항목의 강제 반납: %v", err)
				}
				return nil
			}, 0},
			{"충돌로 행을 안 돌려준 INSERT … RETURNING", func(t *Tx) error {
				var id string
				err := t.tx.QueryRowContext(t.ctx, ins+` ON CONFLICT(project, id) DO NOTHING RETURNING id`, "p", "i1", "제목\x00", "b", at).Scan(&id)
				if !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("충돌한 넣기: %v", err)
				}
				return nil
			}, 0},
			{"충돌로 0 행인 INSERT", func(t *Tx) error {
				_, err := t.tx.ExecContext(t.ctx, ins+` ON CONFLICT(project, id) DO NOTHING`, "p", "i1", "제목\x00", "b", at)
				return err
			}, 0},
			{"저장점까지 되돌린 문장", func(t *Tx) error {
				err := t.Savepoint(func(sp *Tx) error {
					if _, err := sp.tx.ExecContext(sp.ctx, `UPDATE item SET title = ? WHERE project = ? AND id = ?`, "제목\x00", "p", "i1"); err != nil {
						return err
					}
					return errors.New("되돌린다")
				})
				if err == nil || err.Error() != "되돌린다" {
					return fmt.Errorf("저장점: %v", err)
				}
				return nil
			}, 0},
			{"저장점 안에서 실패한 문장", func(t *Tx) error {
				err := t.Savepoint(func(sp *Tx) error {
					_, err := sp.tx.ExecContext(sp.ctx, ins, "p", "i1", "제목\x00", "b", at) // 기본키 겹침
					return err
				})
				if err == nil {
					return errors.New("겹친 넣기가 성공했다")
				}
				return nil
			}, 0},
			{"행을 돌려준 INSERT … RETURNING · 풀린 저장점 · 바꾼 UPDATE", func(t *Tx) error {
				var id string
				if err := t.tx.QueryRowContext(t.ctx, ins+` ON CONFLICT(project, id) DO NOTHING RETURNING id`, "p", "i2", "제목\x00", "b", at).Scan(&id); err != nil {
					return err
				}
				if err := t.Savepoint(func(sp *Tx) error {
					_, err := sp.tx.ExecContext(sp.ctx, `UPDATE item SET body = ? WHERE project = ? AND id = ?`, "본문\x00", "p", "i1")
					return err
				}); err != nil {
					return err
				}
				_, err := t.tx.ExecContext(t.ctx, `UPDATE item SET title = ? WHERE project = ? AND id = ?`, "제목\x00", "p", "i1")
				return err
			}, 3},
		} {
			before := lines()
			if err := s.Tx(ctx, c.body); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if got := lines() - before; got != c.want {
				t.Errorf("%s: 로그 %d 줄, 바라는 것 %d\n%s", c.name, got, c.want, logged.String())
			}
		}
	})
	// 실패한 문장 하나: SQLite 는 문장이 실패해도 트랜잭션이 살아 커밋된다 - 그 문장의 바꿔 넣기를 남기면 커밋 뒤 로그에 실린다.
	s := newStore(t)
	seed(t, s, "p")
	mustItem(t, s, "p", "i1")
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	if err := s.Tx(context.Background(), func(t *Tx) error {
		_, err := t.tx.ExecContext(t.ctx, `INSERT INTO item(project, id, title, body, paths, labels, created_at) VALUES (?, ?, ?, ?, '[]', '[]', ?)`,
			"p", "i1", "제목\x00", "b", fmtTime(nowStamp()))
		if err == nil {
			return errors.New("겹친 넣기가 성공했다")
		}
		return nil // 오류를 삼키고 커밋한다
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), "원장 칸의 NUL 을") {
		t.Errorf("실패한 문장의 바꿔 넣기를 적었다:\n%s", logged.String())
	}
}

// 관문이 거절하면 트랜잭션이 망가진다: 부른 쪽이 그 오류를 삼키고 끝까지 가도 앞서 쓴 행까지 아무것도 커밋되지 않는다(Postgres 가
// 22021 로 트랜잭션을 버리는 것과 같은 결말이 SQLite 에서도 난다). 바꿔 넣은 칸은 커밋된 뒤에만 로그로 낸다.
func TestCellRefusalPoisonsTheTransaction(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seed(t, s, "p")
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	ins := `INSERT INTO item(project, id, title, body, created_at) VALUES (?, ?, ?, ?, ?)`
	err := s.Tx(ctx, func(t *Tx) error {
		if _, err := t.tx.ExecContext(t.ctx, ins, "p", "ok", "제목\x00", "b", fmtTime(nowStamp())); err != nil {
			return err
		}
		_, _ = t.tx.ExecContext(t.ctx, ins, "p", "bad\x00", "t", "b", fmtTime(nowStamp())) // 오류를 삼킨다
		return nil
	})
	var ref *CellRefusal
	if !errors.As(err, &ref) || ref.Column != "id" {
		t.Fatalf("삼킨 거절이 커밋됐다: %v", err)
	}
	if _, gerr := s.GetItem(ctx, "p", "ok"); !errors.Is(gerr, ErrNotFound) {
		t.Errorf("망가진 트랜잭션의 앞 행이 남았다: %v", gerr)
	}
	if strings.Contains(logged.String(), "원장 칸의 NUL 을") {
		t.Errorf("롤백된 쓰기를 바꿔 넣었다고 적었다")
	}
	if err := s.Tx(ctx, func(t *Tx) error {
		_, err := t.tx.ExecContext(t.ctx, ins, "p", "ok", "제목\x00", "b", fmtTime(nowStamp()))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "원장 칸의 NUL 을") || !strings.Contains(logged.String(), `project=\"p\" id=\"ok\"`) ||
		strings.Contains(logged.String(), "제목") {
		t.Errorf("커밋된 바꿔 넣기의 로그 줄(행 좌표 · 내용 없음):\n%s", logged.String())
	}
}

// --judgments 의 원장 되쓰기(WriteLedger)와 그 예행(ScreenLedger)도 같은 관문을 지난다: 자유 글의 NUL 은 이관과 같은 값으로
// 담고, 식별자 칸의 NUL 은 아무것도 안 쓰고 거절한다. 예행이 거절하는 덤프는 적용도 거절한다.
func TestWriteLedgerTakesTheCellGate(t *testing.T) {
	ctx := context.Background()
	at := fmtTime(nowStamp())
	dump := func(id string) LedgerDump {
		p, title := "p", "판단\x00"
		return LedgerDump{
			Projects:  []LedgerProject{{ID: p, Path: "/r/p", DefaultBranch: "main", CreatedAt: at}},
			Judgments: []LedgerJudgment{{ID: id, Project: &p, At: at, Kind: string(model.JudgmentDecision), Title: &title, Body: "본문\x00"}},
		}
	}
	if n, err := ScreenLedger(dump("j1")); err != nil || n != 2 {
		t.Fatalf("예행: 바꿀 칸 %d · %v, 바라는 것 2 · nil", n, err)
	}
	s := newStore(t)
	if err := s.WriteLedger(ctx, dump("j1")); err != nil {
		t.Fatalf("되쓰기: %v", err)
	}
	var title, body string
	if err := s.DB().QueryRow(`SELECT title, body FROM judgment WHERE id = 'j1'`).Scan(&title, &body); err != nil {
		t.Fatal(err)
	}
	if title != `판단\x00` || body != `본문\x00` {
		t.Errorf("되쓴 판단 %q · %q, 바라는 것 이관의 값", title, body)
	}

	_, serr := ScreenLedger(dump("j\x00"))
	werr := newStore(t).WriteLedger(ctx, dump("j\x00"))
	for what, err := range map[string]error{"예행": serr, "되쓰기": werr} {
		var ref *CellRefusal
		if !errors.As(err, &ref) || ref.Table != "judgment" || ref.Column != "id" {
			t.Errorf("%s: 식별자 칸의 NUL 을 %v 로 냈다, 바라는 것 judgment.id 거절", what, err)
		}
	}
}

// store 의 쓰기 문장은 관문이 꼴을 읽을 수 있어야 한다 - 못 읽는 문장은 처음 도는 자리에서 늘 오류다(TestShapeOfRefuses…). 시험이
// 안 닿는 문장도 여기서 잰다: 이 패키지의 시험 아닌 파일에서 INSERT · UPDATE 로 시작하는 글 상수 전부.
func TestStoreWriteStatementsHaveReadableShapes(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fs := token.NewFileSet()
	seen := 0
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fs, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(nd ast.Node) bool {
			bl, ok := nd.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			q, err := strconv.Unquote(bl.Value)
			if err != nil {
				return true
			}
			head := strings.ToUpper(strings.TrimSpace(q))
			if !strings.HasPrefix(head, "INSERT ") && !strings.HasPrefix(head, "UPDATE ") {
				return true
			}
			// 상수 조각(문장 앞머리만 있고 나머지를 이어 붙이는 것) · 서식 틀(`UPDATE %s SET …`)은 완성문을 짓는 시험이 잰다
			// (TestDynamicWriteStatementsHaveReadableShapes).
			if !strings.Contains(head, "?") || strings.Contains(q, "%s") {
				return true
			}
			seen++
			if sh, err := shapeOf(q); err != nil || sh.read {
				t.Errorf("%s: 꼴 %+v · %v, 바라는 것 카탈로그 표에 쓰는 꼴", fs.Position(bl.Pos()), sh, err)
			}
			return true
		})
	}
	if seen < 40 {
		t.Errorf("잰 쓰기 문장이 %d 개다 - 훑기가 문장을 놓친다", seen)
	}
}

// 이어 붙여 짓는 쓰기 문장(되쓰기의 ledgerInsert, 항목 옮기기의 딸린 표, insertUnlessTaken 의 충돌 꼬리, 엔진마다 다른 FK 미루기)은
// 소스의 글 상수로는 꼴을 못 잰다 - 완성문을 지어 관문에 넣는다: 꼴을 읽고, 표가 카탈로그에 있고, 자리표시가 부르는 자리의 인자
// 수와 같고, 칸에 묶인 자리가 그 표의 카탈로그 칸이다.
func TestDynamicWriteStatementsHaveReadableShapes(t *testing.T) {
	p, s := "p", "s"
	dump := LedgerDump{
		Accounts:  []LedgerAccount{{ID: "a"}},
		Machines:  []LedgerMachine{{ID: "m"}},
		Projects:  []LedgerProject{{ID: p}},
		Sessions:  []LedgerSession{{ID: s}},
		Judgments: []LedgerJudgment{{ID: "j", Project: &p}},
		Links:     []LedgerLink{{JudgmentID: "j"}},
		Snapshots: []LedgerSnapshot{{Project: p, Key: "k"}},
	}
	type built struct {
		what, q string
		args    int
	}
	var all []built
	tables := map[string]bool{}
	for _, r := range ledgerRows(dump) {
		all = append(all, built{r.what, r.q, len(r.args)})
	}
	for _, tbl := range itemKeyedTables {
		all = append(all, built{"항목 옮기기 " + tbl, itemKeyedMove(tbl), 3})
	}
	all = append(all, built{"랜딩 줄 서기", unlessTaken(landingEnqueue, landingOneLivePerSession), 3},
		built{"자원 획득", unlessTaken(resourceAcquire, resourceOneHolder), 5})
	for _, b := range all {
		sh, err := shapeOf(b.q)
		if err != nil {
			t.Errorf("%s: %v", b.what, err)
			continue
		}
		tb, ok := pg.TableOf(sh.table)
		if sh.read || !ok || len(sh.slots) != b.args {
			t.Errorf("%s: 꼴 %+v(카탈로그 %v), 바라는 것 카탈로그 표에 쓰는 자리표시 %d 개", b.what, sh, ok, b.args)
			continue
		}
		tables[sh.table] = true
		for k, sl := range sh.slots {
			if sl.col != nil && tb.Col(sl.col.Name) != sl.col {
				t.Errorf("%s: 자리 %d 가 표 %s 의 칸이 아닌 %s 에 묶였다", b.what, k+1, sh.table, sl.col.Name)
			}
		}
	}
	// 되쓰기 일곱 표 · 옮기기 넷 · 줄 서기 · 자원 - 지은 문장이 빠짐없이 닿았다.
	if len(tables) != 13 {
		t.Errorf("잰 표 %d 개(%v), 바라는 것 13", len(tables), tables)
	}
	for _, e := range []engine{engineSQLite, enginePG} {
		if sh, err := shapeOf(e.deferForeignKeys()); err != nil || !sh.read {
			t.Errorf("FK 미루기(%v): %+v %v", e, sh, err)
		}
	}
}

// 트랜잭션 안의 조회는 첫 낱말만 보고 지나간다: 문장을 나누지도 기억하지도 않는다(IN 목록처럼 길이가 갈리는 조회가 기억을 키우지
// 않는다). 인자는 이름 규칙으로 잰다. WITH 는 다 나눠 읽되 그 결과(조회 · 오류)를 기억한다.
func TestCellGateReadsAtTheFirstWord(t *testing.T) {
	q := `SELECT id FROM item WHERE project = ? AND id IN (?, ?, ?)`
	args := []any{"p", "a", "b", "c"}
	if n := testing.AllocsPerRun(10, func() {
		if _, _, err := gateArgs(q, args); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Errorf("조회 하나의 관문 할당 %v, 바라는 것 0(문장을 나눴다)", n)
	}
	if _, ok := shapes.Load(q); ok {
		t.Errorf("조회의 꼴을 기억했다")
	}
	for _, w := range []string{
		`WITH x AS (SELECT id FROM item WHERE project = ?) SELECT id FROM x`,
		`WITH x AS (SELECT 1) UPDATE item SET title = ? WHERE project = ? AND id = ?`,
	} {
		_, _, _ = gateArgs(w, []any{"p"})
		if _, ok := shapes.Load(w); !ok {
			t.Errorf("WITH 의 꼴을 안 기억했다: %s", w)
		}
	}
}

// 관문이 트랜잭션 안 문장 하나에 쓰는 몫이다. 조회는 첫 낱말만 본다(문장을 나누지 않는다). JSON 칸은 U+0000 · 서로게이트 단위가
// 없으면 문서를 걷지 않는다 - json.Marshal 이 쓰는 `\u003c` 꼴이 가득한 1 MiB 문서로 잰다.
func BenchmarkCellGate(b *testing.B) {
	payload := `[` + strings.Repeat(`{"t":"a\u003cb\u003e","n":1},`, 1<<20/24) + `1]`
	for _, c := range []struct {
		name, q string
		args    []any
	}{
		{"조회", `SELECT q.id, q.project, q.session_id, q.enqueued_at, q.left_at, q.left_kind, q.left_detail
			FROM landing_queue q JOIN session s ON s.id = q.session_id
			WHERE q.project = ? AND q.session_id = ? AND q.left_at IS NULL ORDER BY q.id LIMIT 1`, []any{"fd", "s1"}},
		{"JSON 칸 1MiB", `INSERT INTO event(at, project, session_id, kind, payload) VALUES (?, ?, ?, ?, ?)`, []any{"t", "fd", "s", "k", payload}},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := gateArgs(c.q, c.args); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
