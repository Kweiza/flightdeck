// Package pg 는 Postgres 원장의 스키마다.
//
// 두 조각이다. 둘 다 SQL 파일이 정본이고 이 패키지는 그 바이트를 싣기만 한다.
//   - Bootstrap(bootstrap.sql): 초사용자가 DB 하나에 한 번 — 역할 둘, 스키마 public · DB 의 권한, pg_trgm,
//     그리고 이미 있던 앱 역할이 행만 읽고 쓰는 역할인지 재기(아니면 멈춘다).
//   - Baseline(001_baseline.sql): 소유 역할로 — 표 · 인덱스 · 추가 전용 트리거 · 앱 역할의 권한.
//     SQLite 판 18 과 같은 뜻이고 판 번호도 18 이다(BaselineVersion). 다음 증분은 19 부터 PG 에만 쓴다.
//
// ★ 판(schema_version 행)을 적는 것은 이 SQL 이 아니라 이전 실행기다 — SQLite 의 schema.sql 이 판 행을 안
// 적고 실행기가 적는 것과 같다.
//
// ★ 이관(SQLite → PG)이 지켜야 할 것. 저장 계층을 PG 로 옮기는 쪽도 여기를 본다. 이관의 순수 규칙은 import.go ·
// verify.go 이고(옮기는 표 목록 · 칸의 뜻 · 사전 검사 · 행 지문 · 순번), 실행은 store 의 ImportSQLite 다.
//   - identity 칼럼 다섯(pick_eval · resource_hold · event · landing_queue 의 id, item_after.seq)은
//     GENERATED ALWAYS 다. 옛 번호를 그대로 넣으려면 OwnerRole 로 `INSERT … OVERRIDING SYSTEM VALUE` 하고,
//     표마다 `setval(pg_get_serial_sequence('<표>', '<칼럼>'), n, true)` 로 번호를 맞춘다. n 은 원본에서 이미 발급된
//     가장 큰 번호다. AUTOINCREMENT 표는 greatest(max(<칼럼>), sqlite_sequence.seq) 다 — 지워진 최고 번호도 다시
//     쓰지 않으므로 max(<칼럼>) 만으로 맞추면 그 번호를 다시 낸다. item_after.seq 는 rowid 라 max(rowid) 다.
//     n 이 0 이면(발급된 번호가 없다) `setval(…, 1, false)` 로 새 시퀀스처럼 1 부터 낸다(Sequence).
//   - snapshot 의 기본키는 (project, key_digest) 다(key 의 sha256 계산 칼럼, fd_key_digest). upsert 의 충돌
//     대상도 그것이다. 지문이 같은 다른 키는 넣기 전에 23000(snapshot_key_digest_unique)으로 거절된다.
//   - 선점(claim)을 다시 쓰는 upsert 는 살아 있는 남의 행에서 23505(claim_one_live_holder)로 거절된다.
//   - 준비(Bootstrap)는 DB 의 TEMPORARY 를 PUBLIC 에서 거둔다. DB 단위 권한이라 템플릿으로 복제한 DB 는 물려받지 않는다.
//
// ★ 이 패키지는 store 를 import 하지 않는다. 시험 도우미 storetest 가 이것을 원판 내용으로 싣고, store 의
// 패키지 안 시험이 storetest 를 부른다 — store 를 import 하면 순환이다.
package pg

import _ "embed"

// Bootstrap 은 기준선보다 먼저 초사용자가 돌리는 준비 SQL 이다. 몇 번 돌려도 같다.
//
//go:embed bootstrap.sql
var Bootstrap string

// Baseline 은 SQLite 판 18 과 같은 뜻의 PG 스키마다. 준비를 마친 빈 DB 에서 OwnerRole 로 돈다.
//
//go:embed 001_baseline.sql
var Baseline string

// BaselineVersion 은 Baseline 이 만드는 판이다. 접어 넣은 SQLite 의 마지막 판과 같다.
const BaselineVersion = 18

// 역할 이름. SQL 파일이 이 이름을 그대로 쓴다.
const (
	// OwnerRole 은 표 · 함수 · 트리거의 주인이다. 스키마를 바꾸는 이전 실행기가 이 역할로 돈다.
	OwnerRole = "fd_owner"
	// AppRole 은 서버의 역할이다. 행을 읽고 쓰기만 한다.
	AppRole = "fd_app"
)

// Fresh 는 빈 DB 하나를 기준선까지 올리는 SQL 전부다 — 초사용자 연결 하나로 한 번에 돈다.
// 준비는 초사용자로, 기준선은 소유 역할로 돈다(그래야 표의 주인이 소유 역할이다).
// 시험 원판(storetest)이 이것을 그대로 싣는다.
func Fresh() string {
	return Bootstrap + "\nSET ROLE " + OwnerRole + ";\n" + Baseline + "\nRESET ROLE;\n"
}
