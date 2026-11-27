// Package pg 는 Postgres 원장의 스키마다.
//
// 두 조각이다. 둘 다 SQL 파일이 정본이고 이 패키지는 그 바이트를 싣기만 한다.
//   - Bootstrap(bootstrap.sql): 초사용자가 DB 하나에 한 번 — 역할 둘, 스키마 public · DB 의 권한, pg_trgm,
//     그리고 이미 있던 앱 역할이 행만 읽고 쓰는 역할인지 재기(아니면 멈춘다).
//   - Baseline(001_baseline.sql): 소유 역할로 — 표 · 인덱스 · 추가 전용 트리거 · 앱 역할의 권한.
//     SQLite 판 18 과 같은 뜻이고 판 번호도 18 이다(BaselineVersion).
//   - Steps(019_*.sql …): 기준선 위 증분. 소유 역할로 하나씩 돈다. SQLite 원장이 운영 원장인 동안은 SQLite 에 오르는
//     증분을 같은 판 번호 · 같은 뜻으로 여기에도 싣는다(019 가 그 첫째다) - 두 판이 갈리면 store 의 시험이 빨갛다.
//     인덱스만 더한 증분(IndexOnly)은 이관 판 범위(ImportFloor)와 기록을 잃지 않는 내림의 바탕이다.
//
// ★ 판(schema_version 행)을 적는 것은 이 SQL 이 아니라 이전 실행기다 — SQLite 의 schema.sql 이 판 행을 안
// 적고 실행기가 적는 것과 같다.
//
// ★ 이관(SQLite → PG)이 지켜야 할 것. 저장 계층을 PG 로 옮기는 쪽도 여기를 본다. 이관의 순수 규칙은 import.go ·
// verify.go 이고(옮기는 표 목록 · 칸의 뜻 · 사전 검사 · 행 지문 · 순번), 실행은 store 의 ImportSQLite 다. 거꾸로
// 옮기는 역이관(PG → SQLite, store 의 ExportSQLite)도 같은 규칙을 쓰고 그쪽에만 있는 것은 export.go 다.
//   - identity 칼럼 다섯(pick_eval · resource_hold · event · landing_queue 의 id, item_after.seq)은
//     GENERATED ALWAYS 다. 옛 번호를 그대로 넣으려면 OwnerRole 로 `INSERT … OVERRIDING SYSTEM VALUE` 하고,
//     같은 트랜잭션에서 표마다 `ALTER SEQUENCE <pg_get_serial_sequence('<표>', '<칼럼>')> RESTART WITH n+1` 로 번호를
//     맞춘다. n 은 원본에서 이미 발급된 가장 큰 번호다. AUTOINCREMENT 표는 greatest(max(<칼럼>), sqlite_sequence.seq)
//     다 - 지워진 최고 번호도 다시 쓰지 않으므로 max(<칼럼>) 만으로 맞추면 그 번호를 다시 낸다. item_after.seq 는
//     rowid 라 max(rowid) 다. n 이 0 이면(발급된 번호가 없다) 새 시퀀스처럼 1 부터 낸다(Sequence). setval 은 쓰지
//     않는다(트랜잭션에 안 묶여 되감긴 이관이 순번만 바꿔 놓는다).
//   - snapshot 의 기본키는 (project, key_digest) 다(key 의 sha256 계산 칼럼, fd_key_digest). upsert 의 충돌
//     대상도 그것이다. 지문이 같은 다른 키는 넣기 전에 23000(snapshot_key_digest_unique)으로 거절된다.
//   - 선점(claim)을 다시 쓰는 upsert 는 살아 있는 남의 행에서 23505(claim_one_live_holder)로 거절된다.
//   - 준비(Bootstrap)는 DB 의 TEMPORARY 를 PUBLIC 에서 거둔다. DB 단위 권한이라 템플릿으로 복제한 DB 는 물려받지 않는다.
//
// ★ 이 패키지는 store 를 import 하지 않는다. 시험 도우미 storetest 가 이것을 원판 내용으로 싣고, store 의
// 패키지 안 시험이 storetest 를 부른다 — store 를 import 하면 순환이다.
package pg

import (
	_ "embed"
	"regexp"
	"strings"
)

// Bootstrap 은 기준선보다 먼저 초사용자가 돌리는 준비 SQL 이다. 몇 번 돌려도 같다.
//
//go:embed bootstrap.sql
var Bootstrap string

// Baseline 은 SQLite 판 18 과 같은 뜻의 PG 스키마다. 준비를 마친 빈 DB 에서 OwnerRole 로 돈다.
//
//go:embed 001_baseline.sql
var Baseline string

// BaselineVersion 은 Baseline 이 만드는 판이다. 기준선이 접어 넣은 SQLite 판(18)과 같다.
const BaselineVersion = 18

//go:embed 019_event_by_project_kind.sql
var stepEventByProjectKind string

// Step 은 기준선 위 증분 하나다. 이전 실행기(store 의 pgMigrations)와 시험 원판(Latest)이 같은 목록을 싣는다.
type Step struct {
	To   int    // 적용 뒤의 판
	Name string // 로그에 남길 이름
	SQL  string // 소유 역할로 한 트랜잭션 안에서 돈다. 판 행은 적지 않는다(실행기의 몫)
}

// Steps 는 기준선 위에 순서대로 얹는 증분 전부다. 판 번호는 BaselineVersion 바로 위부터 빈칸 없이 오른다.
var Steps = []Step{
	{To: 19, Name: "event 를 (project, kind, id) 로 찾는다", SQL: stepEventByProjectKind},
}

// IndexOnly 는 증분 SQL 이 인덱스를 더하기만 하는가와, 그렇다면 더한 인덱스의 이름(적힌 순서)이다. 순수 함수다. 두 엔진의 증분에
// 같은 규칙을 쓴다(store 는 SQLite 증분에도 이것을 부른다).
//
// ★ 이것이 판 범위 규칙의 바탕이다. 인덱스만 더한 증분은 표 · 칸 · 행을 바꾸지 않으므로 (1) 그 아래 판의 원장도 이관 · 역이관 ·
// 대조가 같은 모양으로 받고(ImportFloor), (2) 그 인덱스를 지우고 판 행을 지우면 기록을 잃지 않고 아래 판으로 내린다(store 의
// DownSteps). 내림 SQL 을 손으로 따로 적지 않는다 - 올림에서 읽어 내므로 두 벌이 없다.
// ★ 받는 문장은 `CREATE INDEX <이름> ON …` 꼴뿐이다. 문장마다 그 꼴이어야 하고, 줄 전체가 `--` 주석인 줄과 빈 줄은 건너뛴다(줄
// 끝 주석이 붙은 문장은 안 받는다 - 모르는 꼴은 「인덱스만」이 아니다). UNIQUE 는 안 받는다: 아래 판이 받던 행을 거절하므로
// 옮길 것이 같지 않다. CONCURRENTLY 는 트랜잭션 안에서 못 돈다. IF NOT EXISTS 는 이미 있던 같은 이름의 인덱스를 내림이 지우게
// 된다. 이름은 따옴표 없는 식별자만 받는다.
func IndexOnly(sql string) ([]string, bool) {
	var body []string
	for _, line := range strings.Split(sql, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "--") {
			body = append(body, line)
		}
	}
	var names []string
	for _, st := range strings.Split(strings.Join(body, "\n"), ";") {
		st = strings.TrimSpace(st)
		if st == "" {
			continue
		}
		m := createIndex.FindStringSubmatch(st)
		if m == nil {
			return nil, false
		}
		names = append(names, m[1])
	}
	return names, len(names) > 0
}

// createIndex 는 IndexOnly 가 받는 문장 하나의 꼴이다. 이름 뒤에 곧바로 ON 이 와야 한다 - CONCURRENTLY · IF NOT EXISTS 는 그
// 자리에서 어긋난다.
var createIndex = regexp.MustCompile(`(?is)^CREATE\s+INDEX\s+([a-z_][a-z0-9_]*)\s+ON\s+\S`)

// 역할 이름. SQL 파일이 이 이름을 그대로 쓴다.
const (
	// OwnerRole 은 표 · 함수 · 트리거의 주인이다. 스키마를 바꾸는 이전 실행기가 이 역할로 돈다.
	OwnerRole = "fd_owner"
	// AppRole 은 서버의 역할이다. 행을 읽고 쓰기만 한다.
	AppRole = "fd_app"
)

// Fresh 는 빈 DB 하나를 기준선까지 올리는 SQL 전부다 — 초사용자 연결 하나로 한 번에 돈다.
// 준비는 초사용자로, 기준선은 소유 역할로 돈다(그래야 표의 주인이 소유 역할이다). 이전 실행기의 기준선 단이 이것이다.
func Fresh() string {
	return prepared(Baseline)
}

// Latest 는 빈 DB 하나를 기준선과 그 위 증분 전부(Steps)까지 올리는 SQL 이다 — Fresh 처럼 초사용자 연결 하나로 돌고,
// 증분도 소유 역할로 돈다. 판 행은 적지 않는다. 시험 원판(storetest)이 이것을 그대로 싣는다: 원판이 기준선에서 멈추면
// 지금 판(SchemaVersion)을 적은 시험 DB 가 증분이 만든 것 없이 돈다.
func Latest() string {
	parts := []string{Baseline}
	for _, s := range Steps {
		parts = append(parts, s.SQL)
	}
	return prepared(parts...)
}

// prepared 는 준비(초사용자) 뒤에 parts 를 소유 역할로 차례로 돌리는 SQL 하나로 잇는다.
func prepared(parts ...string) string {
	return Bootstrap + "\nSET ROLE " + OwnerRole + ";\n" + strings.Join(parts, "\n") + "\nRESET ROLE;\n"
}
