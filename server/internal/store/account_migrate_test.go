package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// 증분 020 의 옛 행 귀속을 두 엔진에서 잰다: 판 19 원장에 행을 심고 20 으로 올리면 소유자 계정이 생기고 행위자 칸이 모두
// 채워지며(판단의 갈래는 세션 · 보드 노트 · 그 밖으로 갈린다), 추가 전용 트리거는 다시 문다. 빈 원장에는 계정을 만들지 않는다.
// 바꾼 행 수가 표의 행 수와 다르면 증분 전체가 되감긴다.

// attributionSeed 는 판 19 모양의 행이다. 판단 넷이 갈래 셋을 모두 지난다: 세션이 있는 판단(session), 세션 없이 제목 머리가
// `[fdboard]` 인 판단(web), 세션 없이 제목이 없거나 `[fdboard]` 가 머리에 없는 판단(cli). 가장 이른 시각은 첫 표가 아닌 판단
// j3 의 것이다(attributionSeedEarliest) - 소유자 계정의 만든 때가 그것이어야 한다.
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
	 ('j3', NULL, NULL, '2025-12-31T23:59:59.000001Z', 'decision', NULL, 'b'),
	 ('j4', 'p', NULL, '2026-01-01T00:00:00.000000Z', 'decision', '머리가 아닌 [fdboard]', 'b')`,
	`INSERT INTO item_revision(project, item_id, rev, at, session_id, title, body, paths, reason) VALUES
	 ('p', 'i1', 1, '2026-01-01T00:00:00.000000Z', 's1', 't', 'b', '[]', 'r'),
	 ('p', 'i1', 2, '2026-01-01T00:00:00.000000Z', NULL, 't', 'b2', '[]', 'r2')`,
}

const attributionSeedEarliest = "2025-12-31T23:59:59.000001Z"

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
			// 만든 때는 옛 행 시각의 최솟값이다 - 지금 시각이면 같은 원장을 따로 올린 둘이 대조에서 갈린다.
			if created != attributionSeedEarliest {
				t.Errorf("소유자 계정의 만든 때가 %q 다(기대: 옛 행 시각의 최솟값 %q)", created, attributionSeedEarliest)
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
// 제자리다. 마지막 귀속 UPDATE(item_revision, 그 표의 트리거를 끄거나 지운 뒤다)가 한 행을 건너뛰게 증분의 글을 바꿔 잰다 - 두
// 엔진의 020 이 같은 문장을 쓴다. 앞 자리를 바꾸면 트리거를 끄기 전에 서서 「꺼 둔 것이 돌아온다」를 못 잰다.
func TestMigration020RollsBackWhenACountDisagrees(t *testing.T) {
	ctx := context.Background()
	const full = "UPDATE item_revision SET author = 'owner' WHERE author IS NULL"
	skipOne := func(steps []Migration) []Migration {
		out := slices.Clone(steps)
		for i := range out {
			if out[i].To != 20 {
				continue
			}
			if strings.Count(out[i].SQL, full) != 1 {
				t.Fatalf("증분 020 에 %q 가 한 번 있어야 한다 - 이 시험의 좌표가 틀렸다", full)
			}
			out[i].SQL = strings.Replace(out[i].SQL, full, full+" AND rev <> 2", 1)
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
				why = "옛 행 귀속: item_revision 행 2개 중 1개만 바뀌었다"
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

// ownerRoots 는 판 19 의 뿌리 표다(다른 표를 NOT NULL FK 로 안 가리킨다). 증분 020 은 이 여섯 가운데 하나에라도 행이 있으면
// 소유자 계정을 만들고, 만든 때를 그 시각 칸의 최솟값으로 한다. 행마다 시각을 달리 심어 어느 표의 값이 골렸는지 보인다.
var ownerRoots = []struct{ table, insert, at string }{
	{"project", `INSERT INTO project(id, path, default_branch, created_at) VALUES ('p', '/r/p', 'main', '%s')`, "2026-02-01T00:00:00.000000Z"},
	{"machine", `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m', 'h', '%s', '2026-09-09T00:00:00.000000Z')`, "2026-02-02T00:00:00.000000Z"},
	{"judgment", `INSERT INTO judgment(id, project, session_id, at, kind, title, body) VALUES ('j', NULL, NULL, '%s', 'decision', NULL, 'b')`, "2026-02-03T00:00:00.000000Z"},
	{"pick_eval", `INSERT INTO pick_eval(project, session_id, at, rejected) VALUES ('p', 's', '%s', '[]')`, "2026-02-04T00:00:00.000000Z"},
	{"event", `INSERT INTO event(at, kind) VALUES ('%s', 'project.add')`, "2026-02-05T00:00:00.000000Z"},
	{"idempotency", `INSERT INTO idempotency(key, fingerprint, status, ctype, body, at) VALUES ('k', 'f', 200, 'application/json', '', '%s')`, "2026-02-06T00:00:00.000000Z"},
}

// 업무 행이 한 표에만 있어도(project 한 행, event 한 행 …) 소유자 계정이 생기고, 만든 때는 그 행의 시각이다. 완전히 빈 원장과는
// TestMigration020LeavesAnEmptyLedgerWithoutAccounts 가 가른다.
func TestMigration020CreatesTheOwnerForAnyBusinessRow(t *testing.T) {
	ctx := context.Background()
	for _, eng := range []string{"sqlite", "postgres"} {
		for _, r := range ownerRoots {
			t.Run(eng+"/"+r.table, func(t *testing.T) {
				path, s := ledgerAtVersion(t, eng, 19)
				if _, err := s.db.ExecContext(ctx, fmt.Sprintf(r.insert, r.at)); err != nil {
					t.Fatalf("%s 한 행 심기: %v", r.table, err)
				}
				if err := raiseLedger(t, path, 20, nil); err != nil {
					t.Fatalf("19→20: %v", err)
				}
				if got := scanStrings(t, s, `SELECT id || '/' || created_at FROM account`); !slices.Equal(got, []string{OwnerAccountID + "/" + r.at}) {
					t.Errorf("%s 한 행뿐인 원장의 계정이 %v 다(기대: 소유자 하나, 만든 때 %s)", r.table, got, r.at)
				}
			})
		}
	}
}

// 소유자 계정의 만든 때는 시각 칸의 저장형과 상관없이 가장 이른 시각이다. SQLite 원장에는 앞 판이 쓴 BLOB 시각이 TEXT 시각과 섞여
// 있을 수 있고, 저장형을 통일하지 않으면 SQLite 의 min 은 저장형 순(TEXT < BLOB)으로 먼저 갈라 2월 TEXT 를 1월 BLOB 보다 앞에 둔다 -
// 표 사이(판단의 1월 BLOB · 프로젝트의 2월 TEXT)에서도 한 표 안(사건 두 행)에서도. Postgres 에는 저장형이 없고 이관이 그 BLOB 을
// 글로 옮기므로, 같은 시각을 글로 심은 Postgres 원장과 같은 값이어야 한다.
func TestMigration020OwnerTimeIgnoresStorageClass(t *testing.T) {
	ctx := context.Background()
	const jan, feb = "2026-01-05T00:00:00.000000Z", "2026-02-01T00:00:00.000000Z"
	for _, eng := range []string{"sqlite", "postgres"} {
		blob := func(v string) string {
			if eng == "sqlite" {
				return "CAST('" + v + "' AS BLOB)"
			}
			return "'" + v + "'" // 이관이 옮긴 꼴(글)
		}
		for _, c := range []struct {
			name string
			seed []string
		}{
			{"표 사이", []string{
				`INSERT INTO project(id, path, default_branch, created_at) VALUES ('p', '/r/p', 'main', '` + feb + `')`,
				`INSERT INTO judgment(id, project, session_id, at, kind, title, body) VALUES ('j', NULL, NULL, ` + blob(jan) + `, 'decision', NULL, 'b')`,
			}},
			{"한 표 안", []string{
				`INSERT INTO event(at, kind) VALUES (` + blob(jan) + `, 'project.add')`,
				`INSERT INTO event(at, kind) VALUES ('` + feb + `', 'project.add')`,
			}},
		} {
			t.Run(eng+"/"+c.name, func(t *testing.T) {
				path, s := ledgerAtVersion(t, eng, 19)
				for _, q := range c.seed {
					if _, err := s.db.ExecContext(ctx, q); err != nil {
						t.Fatalf("심기(%s): %v", q, err)
					}
				}
				if eng == "sqlite" {
					if got := scanStrings(t, s, `SELECT typeof(at) FROM (SELECT at FROM judgment UNION ALL SELECT at FROM event) ORDER BY 1`); !slices.Contains(got, "blob") {
						t.Fatalf("BLOB 시각이 안 심겼다: %v", got)
					}
				}
				if err := raiseLedger(t, path, 20, nil); err != nil {
					t.Fatalf("19→20: %v", err)
				}
				if got := scanStrings(t, s, `SELECT id || '/' || created_at FROM account`); !slices.Equal(got, []string{OwnerAccountID + "/" + jan}) {
					t.Errorf("소유자 계정이 %v 다(기대: 만든 때 %s - 가장 이른 시각)", got, jan)
				}
				if eng == "sqlite" {
					if got := scanStrings(t, s, `SELECT typeof(created_at) FROM account`); !slices.Equal(got, []string{"text"}) {
						t.Errorf("소유자 계정의 만든 때 저장형이 %v 다(기대 text)", got)
					}
				}
			})
		}
	}
}

// ownerRoots 밖의 판 19 표는 모두 NOT NULL FK 칸을 가진다 - 그 행이 있으면 FK 가 가리키는 행도 있으므로, 뿌리 여섯만 보아도
// 「업무 행이 하나라도 있다」를 놓치지 않는다. 판 19 에 뿌리 표가 늘면 여기서 선다(020 의 조건에도 그 표를 더해야 한다).
func TestMigration020OwnerRootsHoldEveryOtherTable(t *testing.T) {
	ctx := context.Background()
	_, s := ledgerAtVersion(t, "sqlite", 19)
	roots := map[string]bool{"schema_version": true}
	for _, r := range ownerRoots {
		roots[r.table] = true
	}
	tables := scanStrings(t, s, `SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name NOT LIKE 'judgment\_fts%' ESCAPE '\' ORDER BY name`)
	if len(tables) < len(roots) {
		t.Fatalf("판 19 의 표가 %d 개뿐이다: %v", len(tables), tables)
	}
	for _, table := range tables {
		var held int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_list(?) f
			JOIN pragma_table_info(?) c ON c.name = f."from" WHERE c."notnull" = 1`, table, table).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if roots[table] == (held > 0) {
			t.Errorf("%s: 뿌리 표 %v 인데 NOT NULL FK 칸이 %d 개다", table, roots[table], held)
		}
	}
}

// 새 표 넷의 기본키는 두 엔진 모두 NULL 을 거절한다. SQLite 는 INTEGER 가 아닌 PRIMARY KEY 에 NULL 을 받으므로 020 이 NOT NULL 을
// 적어야 한다 - 카탈로그 대조(키 칸 = NOT NULL)는 PG 의 기본키가 품은 NOT NULL 만 보아 이것을 못 잡는다.
func TestMigration020KeysRefuseNull(t *testing.T) {
	ctx := context.Background()
	const at = "2026-01-01T00:00:00.000000Z"
	for _, eng := range []string{"sqlite", "postgres"} {
		t.Run(eng, func(t *testing.T) {
			_, s := ledgerAtVersion(t, eng, 20)
			if _, err := s.db.ExecContext(ctx, `INSERT INTO account(id, login, display_name, role, created_at)
				VALUES ('a', 'a', 'a', 'admin', '`+at+`')`); err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				`INSERT INTO account(login, display_name, role, created_at) VALUES ('b', 'b', 'member', '` + at + `')`,
				`INSERT INTO invite(token_hash, created_by, role, created_at, expires_at) VALUES ('t', 'a', 'member', '` + at + `', '` + at + `')`,
				`INSERT INTO web_session(account_id, created_at, expires_at, last_seen_at) VALUES ('a', '` + at + `', '` + at + `', '` + at + `')`,
				`INSERT INTO device_code(user_code, requested_name, requested_ip, requested_at, expires_at)
				 VALUES ('u', 'n', '127.0.0.1', '` + at + `', '` + at + `')`,
			} {
				_, err := s.db.ExecContext(ctx, q)
				if c := driverCodeOf(err); c.state != "23502" && c.sqlite != sqlite3.SQLITE_CONSTRAINT_NOTNULL {
					t.Errorf("키를 뺀 %s 가 NOT NULL 로 거절되지 않았다(%v): %v", q, c, err)
				}
			}
		})
	}
}
