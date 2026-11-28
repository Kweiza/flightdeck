package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// 증분 020 의 옛 행 귀속을 두 엔진에서 잰다: 판 19 원장에 행을 심고 20 으로 올리면 소유자 계정이 생기고 행위자 칸이 모두
// 채워지며(판단의 갈래는 세션 · 보드 노트 · 그 밖으로 갈린다), 추가 전용 트리거는 다시 문다. 빈 원장에는 계정을 만들지 않는다.
// 바꾼 행 수가 표의 행 수와 다르면 증분 전체가 되감긴다.

// attributionSeed 는 판 19 모양의 행이다. 판단 넷이 갈래 셋을 모두 지난다: 세션이 있는 판단(session), 세션 없이 제목 머리가
// `[fdboard]` 인 판단(web), 세션 없이 제목이 없거나 `[fdboard]` 가 머리에 없는 판단(cli).
var attributionSeed = []string{
	`INSERT INTO project(id, path, default_branch, created_at) VALUES ('p', '/r/p', 'main', '2026-01-01T00:00:00.000000Z')`,
	`INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m', 'host-a', '2026-01-01T00:00:00.000000Z', '2026-01-01T00:00:00.000000Z')`,
	`INSERT INTO session(id, project, machine_id, worktree, cc_session_id, state, opened_at)
	 VALUES ('s1', 'p', 'm', '/w1', 'c1', 'active', '2026-01-01T00:00:00.000000Z')`,
	`INSERT INTO item(project, id, title, body, paths, labels, state, created_at) VALUES
	 ('p', 'i1', 't', 'b', '[]', '[]', 'open', '2026-01-01T00:00:00.000000Z'),
	 ('p', 'i2', 't', 'b', '[]', '[]', 'open', '2026-01-01T00:00:00.000000Z')`,
	`INSERT INTO judgment(id, project, session_id, at, kind, title, body) VALUES
	 ('j1', 'p', 's1', '2026-01-01T00:00:00.000000Z', 'decision', '[fdboard] 세션이 남긴 것', 'b'),
	 ('j2', 'p', NULL, '2026-01-01T00:00:00.000000Z', 'decision', '[fdboard] 소유자 노트', 'b'),
	 ('j3', NULL, NULL, '2026-01-01T00:00:00.000000Z', 'decision', NULL, 'b'),
	 ('j4', 'p', NULL, '2026-01-01T00:00:00.000000Z', 'decision', '머리가 아닌 [fdboard]', 'b')`,
	`INSERT INTO item_revision(project, item_id, rev, at, session_id, title, body, paths, reason) VALUES
	 ('p', 'i1', 1, '2026-01-01T00:00:00.000000Z', 's1', 't', 'b', '[]', 'r'),
	 ('p', 'i1', 2, '2026-01-01T00:00:00.000000Z', NULL, 't', 'b2', '[]', 'r2')`,
}

func seedAttribution(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range attributionSeed {
		if _, err := s.db.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("판 19 행 심기 실패(%s): %v", q, err)
		}
	}
}

func scanStrings(t *testing.T, s *Store, q string) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

func TestMigration020AttributesOldRowsToTheOwner(t *testing.T) {
	ctx := context.Background()
	for _, eng := range []string{"sqlite", "postgres"} {
		t.Run(eng, func(t *testing.T) {
			path, s := ledgerAtVersion(t, eng, 19)
			seedAttribution(t, s)
			if err := raiseLedger(t, path, 20, nil); err != nil {
				t.Fatalf("19→20: %v", err)
			}

			// 소유자 계정 하나: admin, 암호 · 메일 · 사용 중지 없음, 만든 때는 원장의 시각 꼴이다.
			var n int
			var login, display, role, created string
			var email, password, disabled *string
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM account`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("계정이 %d 개다(기대 1): %v", n, err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT login, display_name, email, role, password_hash, created_at, disabled_at
				FROM account WHERE id = ?`, OwnerAccountID).Scan(&login, &display, &email, &role, &password, &created, &disabled); err != nil {
				t.Fatalf("소유자 계정 읽기: %v", err)
			}
			if login != OwnerAccountID || display != OwnerAccountID || role != "admin" || email != nil || password != nil || disabled != nil {
				t.Errorf("소유자 계정이 %q · %q · %q · 메일 %v · 암호 %v · 사용 중지 %v 다", login, display, role, email, password, disabled)
			}
			if _, err := parseTime(created); err != nil || len(created) != len(timeLayout) {
				t.Errorf("소유자 계정의 만든 때 %q 가 원장의 시각 꼴(%s)이 아니다: %v", created, timeLayout, err)
			}

			// 행위자 칸: 모두 소유자다. 기기 이름은 hostname 이고, 토큰은 없다. 항목의 만든 세션은 채우지 않는다.
			for q, want := range map[string][]string{
				`SELECT account_id || '/' || name || '/' || coalesce(token_hash, '-') FROM machine`: {"owner/host-a/-"},
				`SELECT account_id FROM session`: {"owner"},
				`SELECT created_by || '/' || coalesce(created_session, '-') FROM item ORDER BY id`: {"owner/-", "owner/-"},
				`SELECT id || '/' || author || '/' || via FROM judgment ORDER BY id`: {
					"j1/owner/session", "j2/owner/web", "j3/owner/cli", "j4/owner/cli"},
				`SELECT author FROM item_revision ORDER BY rev`: {"owner", "owner"},
			} {
				if got := scanStrings(t, s, q); !slices.Equal(got, want) {
					t.Errorf("%s = %v, 기대 %v", q, got, want)
				}
			}

			a, err := readAttribution(ctx, s.db)
			if err != nil {
				t.Fatal(err)
			}
			if !a.Owner || a.Unattributed() != 0 || len(a.Columns) != len(actorColumns) ||
				!slices.Equal(a.Via, []ViaCount{{"cli", 2}, {"session", 1}, {"web", 1}}) {
				t.Errorf("귀속 축이 %+v 다", a)
			}

			// 끄고(지우고) 다시 켠(만든) 트리거가 문다. 개정의 갱신 금지 칸에 author 가 들었다.
			for _, q := range []string{
				`UPDATE judgment SET author = NULL WHERE id = 'j1'`,
				`UPDATE judgment SET body = 'x' WHERE id = 'j1'`,
				`UPDATE item_revision SET author = NULL WHERE rev = 1`,
				`UPDATE item_revision SET title = 'x' WHERE rev = 1`,
				`DELETE FROM judgment WHERE id = 'j3'`,
			} {
				if _, err := s.db.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "추가 전용") && !strings.Contains(err.Error(), "삭제할 수 없다") {
					t.Errorf("%s 가 추가 전용 트리거에 안 걸렸다: %v", q, err)
				}
			}
		})
	}
}

// 행이 하나도 없는 원장에는 소유자 계정을 만들지 않는다 - 이관 · 역이관의 빈 대상이 원본의 계정 행을 그대로 받는다.
func TestMigration020LeavesAnEmptyLedgerWithoutAccounts(t *testing.T) {
	ctx := context.Background()
	for _, eng := range []string{"sqlite", "postgres"} {
		t.Run(eng, func(t *testing.T) {
			path, s := ledgerAtVersion(t, eng, 19)
			if err := raiseLedger(t, path, 20, nil); err != nil {
				t.Fatalf("19→20: %v", err)
			}
			a, err := readAttribution(ctx, s.db)
			if err != nil {
				t.Fatal(err)
			}
			var n int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM account`).Scan(&n); err != nil || n != 0 || a.Owner {
				t.Fatalf("빈 원장에 계정이 %d 개 생겼다(소유자 %v): %v", n, a.Owner, err)
			}
		})
	}
}

// 바꾼 행 수가 표의 행 수와 다르면 증분 전체가 되감긴다: 판은 19 그대로이고 계정 표도 행위자 칸도 없으며, 꺼 둔(지운) 트리거가
// 제자리다. 한 표의 UPDATE 가 한 행을 건너뛰게 증분의 글을 바꿔 잰다 - 두 엔진의 020 이 같은 문장을 쓴다.
func TestMigration020RollsBackWhenACountDisagrees(t *testing.T) {
	ctx := context.Background()
	const full = "UPDATE item SET created_by = 'owner' WHERE created_by IS NULL"
	skipOne := func(steps []Migration) []Migration {
		out := slices.Clone(steps)
		for i := range out {
			if out[i].To != 20 {
				continue
			}
			if strings.Count(out[i].SQL, full) != 1 {
				t.Fatalf("증분 020 에 %q 가 한 번 있어야 한다 - 이 시험의 좌표가 틀렸다", full)
			}
			out[i].SQL = strings.Replace(out[i].SQL, full, full+" AND id <> 'i2'", 1)
		}
		return out
	}
	for _, eng := range []string{"sqlite", "postgres"} {
		t.Run(eng, func(t *testing.T) {
			path, s := ledgerAtVersion(t, eng, 19)
			seedAttribution(t, s)
			err := raiseLedger(t, path, 20, skipOne)
			why := "CHECK constraint failed"
			if eng == "postgres" {
				why = "옛 행 귀속: item 행 2개 중 1개만 바뀌었다"
			}
			if err == nil || !strings.Contains(err.Error(), why) {
				t.Fatalf("한 행을 건너뛴 귀속이 섰어야 한다(기대 %q): %v", why, err)
			}
			if v, _, _, _ := downFacts(t, s); v != 19 {
				t.Fatalf("되감기지 않았다: 판 %d", v)
			}
			cat := `SELECT count(*) FROM sqlite_master WHERE name = 'account'`
			if eng == "postgres" {
				cat = `SELECT count(*) FROM pg_class WHERE relname = 'account'`
			}
			var n int
			if err := s.db.QueryRowContext(ctx, cat).Scan(&n); err != nil || n != 0 {
				t.Fatalf("되감긴 증분의 account 표가 남았다(%d): %v", n, err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE judgment SET body = 'x' WHERE id = 'j1'`); err == nil ||
				!strings.Contains(err.Error(), "추가 전용") {
				t.Fatalf("되감긴 뒤 judgment 의 추가 전용 트리거가 안 문다: %v", err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE item_revision SET body = 'x' WHERE rev = 1`); err == nil ||
				!strings.Contains(err.Error(), "추가 전용") {
				t.Fatalf("되감긴 뒤 item_revision 의 추가 전용 트리거가 안 문다: %v", err)
			}
			// 고친 증분 없이 다시 올리면 선다.
			if err := raiseLedger(t, path, 20, nil); err != nil {
				t.Fatalf("되감긴 뒤 다시 올리기: %v", err)
			}
		})
	}
}
