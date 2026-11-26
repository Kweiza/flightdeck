package store

import (
	"context"
	"slices"
	"testing"
)

// 살아 있는 DB 의 카탈로그를 읽는 시험 도우미. 표 목록 · 칸 목록을 스키마와 맞대는 시험(이동 목록 · 프로젝트 참조 목록 ·
// 원장 칸 목록 · 표 수)이 모두 이것으로 읽는다. 엔진 차이는 아래 두 질의의 갈래 한 곳뿐이다. 시험마다 sqlite_master ·
// PRAGMA · information_schema 를 따로 적으면 한 엔진의 시험만 남고, 다른 엔진에서는 맞대기가 아예 안 돈다.
//
// ★ 빈 결과는 실패로 본다. 좌표가 틀린 훑기(엉뚱한 스키마 · 바뀐 표 이름)는 0행을 내고, 0행 위의 맞대기는
// 「빠진 것이 없다」로 초록이 된다.

// tablesQuery 는 업무 표 이름을 이름순으로 내는 질의다.
//
//   - SQLite: sqlite_master 의 표(FTS5 가상 표와 그 그림자 표 포함). 엔진 자신의 표(sqlite_ 로 시작)는 뺀다.
//   - Postgres: 이름 없는 질의가 닿는 스키마(current_schema)의 표. 시험 DB 마다 스키마 이름을 적지 않는다.
func tablesQuery(e engine) string {
	if e == enginePG {
		return `SELECT tablename::text FROM pg_catalog.pg_tables WHERE schemaname = current_schema() ORDER BY tablename`
	}
	return `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`
}

// columnsQuery 는 표 하나(인자)의 칸 이름을 선언 순서로 내는 질의다. 없는 표면 0행이다.
//
// ★ 계산 칼럼(GENERATED)은 두 엔진 모두 뺀다. SQLite 의 pragma_table_info 가 원래 그것을 안 보이고(table_xinfo 만
// 보인다), Postgres 의 계산 칼럼(snapshot.key_digest)은 누구도 손으로 못 적는 파생값이라 원장이 실을 것도 아니다.
// 한쪽만 빼면 같은 맞대기가 엔진마다 다른 것을 잰다.
func columnsQuery(e engine) string {
	if e == enginePG {
		return `SELECT a.attname::text FROM pg_catalog.pg_attribute a
		         WHERE a.attrelid = to_regclass(?) AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
		         ORDER BY a.attnum`
	}
	return `SELECT name FROM pragma_table_info(?) ORDER BY cid`
}

// liveTables 는 s 의 업무 표 이름을 이름순으로 낸다(tablesQuery).
func liveTables(t *testing.T, s *Store) []string {
	t.Helper()
	out := catalogNames(t, s, "표 목록", tablesQuery(s.db.dialect()))
	if len(out) == 0 {
		t.Fatal("표를 하나도 못 찾았다 - 훑기의 좌표가 틀린 것이지 통과가 아니다")
	}
	return out
}

// liveColumns 는 표 하나의 칸 이름을 선언 순서로 낸다. 없는 표면 시험을 실패시킨다.
func liveColumns(t *testing.T, s *Store, table string) []string {
	t.Helper()
	out := catalogNames(t, s, table+" 의 칸 목록", columnsQuery(s.db.dialect()), table)
	if len(out) == 0 {
		t.Fatalf("%s 표가 스키마에 없다 - 맞대기의 좌표가 틀렸다(표 이름이 바뀌었나)", table)
	}
	return out
}

// liveColumnSet 은 liveColumns 를 집합으로 낸다.
func liveColumnSet(t *testing.T, s *Store, table string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, c := range liveColumns(t, s, table) {
		out[c] = true
	}
	return out
}

func catalogNames(t *testing.T, s *Store, what, q string, args ...any) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("%s 조회 실패: %v", what, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("%s 해석 실패: %v", what, err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s 순회 실패: %v", what, err)
	}
	return out
}

// TestLiveColumnsReadDeclarationOrderOnBothEngines 는 도우미 자신을 잰다: 두 엔진에서 칸 목록이 선언 순서이고, 없는 표는
// 0행이다(그래서 liveColumns 가 그것을 실패로 바꿀 수 있다). 표 목록 쪽은 표 수 시험 둘(schema_table_count_test.go)이
// 선언과 맞대어 잰다.
func TestLiveColumnsReadDeclarationOrderOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		want := []string{"id", "hostname", "first_seen", "last_seen"}
		if got := liveColumns(t, s, "machine"); !slices.Equal(got, want) {
			t.Fatalf("machine 의 칸이 %v - 선언 순서 %v 여야 한다", got, want)
		}
		if n := catalogNames(t, s, "없는 표의 칸 목록", columnsQuery(s.db.dialect()), "no_such_table"); len(n) != 0 {
			t.Fatalf("없는 표에서 칸 %v 가 나왔다", n)
		}
	})
}
