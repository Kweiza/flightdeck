package pg

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/kweiza/flightdeck/internal/nulesc"
)

// SQLite 원장을 이 기준선으로 옮기는 규칙이다 - `fd migrate import-sqlite` · `fd migrate verify` 의 순수한 몫.
// 무엇을 어떤 순서로 옮기고, 어떤 값을 거절하고, 행을 어떻게 재는지(verify.go)만 정한다. 두 원장을 열고 읽고 쓰는 실행은
// store 의 importsqlite.go 다. 거꾸로 옮기는 역이관(`fd migrate export-sqlite`)도 같은 표 목록 · 값 검사 · 행 지문을 쓰고,
// 그쪽에만 있는 규칙은 export.go 다.
//
// ★ 표 목록(tables)은 이관 판(ImportVersion)의 표 · 칸을 손으로 적은 것이다. 이관은 그 목록이 그대로 맞는 판 범위
// (ImportFloor..ImportVersion)만 받는다 - 범위 안 판끼리는 인덱스만 다르다. 판 18 위의 증분(019 인덱스)은 표 · 칸을 안 바꿨으므로
// 목록은 기준선 18 의 것 그대로다. 표나 칸을 바꾸는 증분이 두 엔진에 함께 오르면 이 목록도 같이 고치고, 그 증분은 인덱스만
// 더한 것이 아니므로 범위의 아래 끝이 그 판으로 오른다. 손으로 적은 목록이 두 엔진의 살아 있는 카탈로그와 같은지는
// import_test.go 가 잰다(칼럼 이름 · 순서 · 타입 · NOT NULL · 기본키 · identity).
//
// ★ 옮기지 않는 것은 LeftBehind 다. 그 밖의 표는 전부 옮긴다 - idempotency 도다(재기동 너머로 판단 · 발번 · 항목
// 만들기 · 종료를 한 번만 처리하려는 기록이라, 버리면 전환 직전 응답을 잃은 기기의 아웃박스 재전송이 판단을 두 건
// 만든다). 만료된 행도 옮긴다 - 만료는 서버의 보존 기한(설정값)이 정하는 것이라 이관이 시계를 보고 거를 자리가 아니고,
// 서버가 다음 기록 때 그 기한으로 청소한다.

// ImportVersion 은 이관 · 역이관 · 대조가 아는 가장 높은 판이다. 두 엔진이 함께 오른 마지막 판이다(SQLite 의 마지막 판 = 기준선과
// 그 위 Steps 가 같은 뜻으로 닿는 판). 역이관이 내는 SQLite 파일이 이 판이고, 빈 Postgres 원장은 `fd migrate --to ImportVersion`
// 이 이 판까지만 깐다. 이 수가 store.SchemaVersion 과 같은지는 import_test.go 가 잰다. 받는 판은 이 하나가 아니라 범위다
// (Transferable).
const ImportVersion = 19

// ImportFloor 는 이관 · 역이관 · 대조가 받는 가장 낮은 판이다. ImportVersion 에서 아래로 인덱스만 더한 증분(IndexOnly)을 거슬러
// 닿는 판이고, 기준선(BaselineVersion) 아래로는 내려가지 않는다 - Postgres 원장에는 그 아래 판이 없다.
//
// ★ 판 범위 규칙은 이것 하나다. 인덱스만 더한 증분은 표 · 칸 · 행을 안 바꾸므로 그 아래 판의 원장도 옮길 것(Tables)이 같고, 같은
// 까닭으로 기록을 잃지 않고 내릴 수 있다(store 의 DownSteps). 그래서 원본과 대상의 판이 범위 안에서 서로 달라도 받는다: 판 18 에서
// 옮긴 SQLite 원장(표지가 있어 판을 못 올린다)을 판 19 로 오른 Postgres 원장과 맞대는 것이 그 갈래다(README 「켜기 전에 섰을
// 때」). 인덱스가 받는 값의 상한(MaxBytes)은 이관의 사전 검사(ImportCell)가 범위 전체에 똑같이 잰다 - 범위의 꼭대기가 그 인덱스를
// 갖기 때문이다.
func ImportFloor() int { return floorOf(Steps, ImportVersion, BaselineVersion) }

// floorOf 는 증분 목록 steps 위에서 판 top 부터 아래로 인덱스만 더한 증분을 거슬러 닿는 판이다(base 아래로는 안 간다). 순수
// 함수다 - ImportFloor 의 규칙을 지금 증분 목록과 떼어 잰다(import_test.go).
func floorOf(steps []Step, top, base int) int {
	v := top
	for i := len(steps) - 1; i >= 0 && v > base; i-- {
		if steps[i].To > v {
			continue
		}
		if steps[i].To < v {
			break // 판 v 에 이르는 증분이 없다 - 빈칸 아래로는 내려가지 않는다
		}
		if _, ok := IndexOnly(steps[i].SQL); !ok {
			break
		}
		v--
	}
	return v
}

// Transferable 은 판 v 의 원장을 이관 · 역이관 · 대조가 받는가다(ImportFloor 이상 ImportVersion 이하). 순수 함수다.
func Transferable(v int) bool { return v >= ImportFloor() && v <= ImportVersion }

// Kind 는 칸이 기준선에서 갖는 타입이다. SQLite 는 칸 타입을 강제하지 않으므로 읽은 값이 이 타입으로 옮겨지는지를
// 사전 검사가 잰다(Cell).
type Kind int

const (
	Text  Kind = iota // text (SQLite TEXT)
	Int               // bigint (SQLite INTEGER)
	Bytes             // bytea (SQLite BLOB)
)

func (k Kind) String() string {
	switch k {
	case Int:
		return "bigint"
	case Bytes:
		return "bytea"
	}
	return "text"
}

// Column 은 옮기는 칸 하나다.
type Column struct {
	Name string
	Kind Kind
	// NotNull 은 기준선의 NOT NULL 이다(기본키 칸 포함 - SQLite 는 정수 아닌 기본키에 NULL 을 받는다).
	NotNull bool
	// Key 는 행을 가리키는 칸이다: SQLite 판의 기본키, 기본키가 없는 item_after 는 seq(= rowid). 사전 검사에 걸린 행 ·
	// 대조에서 어긋난 행을 이 칸들의 값으로 적는다.
	Key bool
	// Identifier 는 식별자 칸이다: 기본키(key 가 함께 단다), 기준선의 UNIQUE 묶음, 그리고 다른 행을 이름으로 가리키는 칸(FK 가
	// 있든 없든 - project · *_id · dep_* 등). 이관은 이 칸의 NUL 을 바꿔 넣지 않고 거절한다(FaultIdentNUL) - 바꾼 값이 원래
	// `\x00` 네 글자를 담은 다른 이름과 같아지면 그 이름을 가리키던 행이 다른 행을 가리킨다. 바꿔 넣기는 자유 글에만이다.
	// 표시는 이 카탈로그 한 곳이고, 기준선의 기본키 · UNIQUE · FK 칸을 빠짐없이 덮는지는 import_test.go 가 Postgres 카탈로그로 잰다.
	Identifier bool
	// JSON 은 서버가 JSON 문서로 쓰는 칸이다(json.Marshal). Postgres 에서는 text 그대로지만 jsonb 로 읽힐 수 있는 칸이라
	// 깨진 문서 · 짝 없는 서로게이트 · 키 안의 \u0000 은 거절하고 글 값 안의 \u0000 은 그 글 토큰만 바꿔 넣는다(RewriteJSONNUL).
	JSON bool
	// Identity 는 기준선에서 GENERATED ALWAYS AS IDENTITY 다. 옛 번호를 그대로 넣으려면 소유 역할로 OVERRIDING SYSTEM
	// VALUE 를 붙이고, 넣은 뒤 같은 트랜잭션에서 `ALTER SEQUENCE … RESTART WITH` 로 번호를 맞춘다(Sequence).
	Identity bool
	// Rowid 는 SQLite 에서 이 칸 대신 rowid 를 읽는다는 뜻이다. item_after.seq 가 그것이다 - 그 표에는 기본키가 없어
	// SQLite 는 rowid 로 순서를 읽고, 기준선은 그 자리를 seq 로 둔다.
	Rowid bool
	// MaxBytes 는 글 칸 값의 바이트 상한이다(0 이면 없다). Postgres 의 B-tree 인덱스는 한 항목이 페이지의 약 1/3(2704 바이트)을
	// 넘으면 넣기를 거절하는데, SQLite 는 상한이 없다. 그 인덱스의 키 칸에만 둔다(event_by_project_kind 의 project · kind,
	// EventProjectMaxBytes). Postgres 가 **새로 받는** 값에만 잰다: 이관의 사전 검사(ImportCell)가 넘는 값을 FaultTooLong 으로
	// 거절하고, Postgres 판올림은 그 인덱스를 만들기 전에 같은 상한으로 잰다(store 의 keyLimitGate), 새 행은 store 의 쓰기 관문이
	// 막는다. 이미 원장에 있는 값을 맞대거나(대조) SQLite 로 내는(역이관) 길은 재지 않는다 - 원문 그대로 옮기고 맞댄다.
	MaxBytes int
}

// event_by_project_kind(project, kind, id) 의 키 칸 상한이다(Column.MaxBytes). 둘을 다 채워도 항목(머리 8 · 길이 머리 · 정렬 ·
// id 8 바이트)이 압축 없이 2704 바이트 안에 든다 - 압축이 안 되는 값으로 시험이 잰다(store 의 keylimit_test.go). 정상 원장의
// 값은 이보다 훨씬 작다(프로젝트는 저장소 디렉토리 이름, kind 는 서버의 상수다).
const (
	EventProjectMaxBytes = 2048
	EventKindMaxBytes    = 512
)

// Table 은 옮기는 표 하나다.
type Table struct {
	Name string
	Cols []Column
	// AutoIncrement 는 SQLite 에서 AUTOINCREMENT 표다 - 발급한 최고 번호가 지워진 뒤에도 sqlite_sequence 에 남는다.
	AutoIncrement bool
	// DigestKey 는 기준선의 기본키가 원문 대신 sha256 을 쓰는 칸이다(snapshot.key → key_digest, fd_key_digest). 기본키 겹침은
	// 그 지문으로 잰다(Uniques).
	DigestKey string
	// unique 는 기본키 밖에 기준선이 겹치지 못하게 하는 칸 묶음이다(UNIQUE 제약 · 부분 UNIQUE 인덱스). 기본키와 함께 Uniques 가 낸다.
	unique []Unique
}

// Unique 는 기준선이 두 행에 같은 값을 두지 못하게 하는 칸 묶음 하나다: 기본키, UNIQUE 제약, 부분 UNIQUE 인덱스. 이관의 사전
// 검사가 옮길 값으로 겹침을 먼저 잰다(FaultUnique). 기준선의 실제 제약 · 인덱스와 같은지는 import_test.go 가 Postgres 카탈로그로
// 잰다.
type Unique struct {
	// Name 은 기준선의 제약 · 인덱스 이름이다(기본키는 <표>_pkey).
	Name string
	// Cols 는 칸 이름이다(기준선의 선언 순서). 지문 칸(Table.DigestKey)은 기준선이 그 지문(key_digest)으로 잰다.
	Cols []string
	// WhereNull 은 부분 인덱스의 조건 칸이다: 그 칸이 NULL 인 행끼리만 겹치지 못한다. 비면 모든 행이다.
	WhereNull string
}

// Index 는 칸 name 의 자리다(선언 순서, 0부터). 없으면 -1 이다.
func (t Table) Index(name string) int {
	for i, c := range t.Cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// LeftBehind 는 SQLite 원본의 표 name 을 일부러 옮기지 않는가다: schema_version(원장마다 자기 이전 이력이다),
// sqlite_ 로 시작하는 엔진 표(이름 공간을 엔진이 쥔다 - sqlite_sequence 는 순번 맞추기가 흡수한다), FTS5 판단 색인과 그
// 그림자 표(leftBehind - 기준선은 trigram 인덱스로 대신한다). 그 밖의 원본 표는 전부 Tables 에 있어야 한다 - 모르는 표가
// 있으면 이관은 거절한다.
func LeftBehind(name string) bool {
	return strings.HasPrefix(name, "sqlite_") || leftBehind[name]
}

// leftBehind 는 옮기지 않는 표를 이름 그대로 적은 것이다. judgment_fts 는 외부 내용 FTS5 표(content='judgment')라 그림자
// 표가 넷이다(_content 가 없다). 접두어로 거르면 그 이름으로 시작하는 다른 표(손으로 만든 표 · 다음 판의 표)를 말없이 버린다.
// 이 목록이 이관 판 원본의 실제 표와 같은지는 import_test.go 가 잰다.
var leftBehind = map[string]bool{
	"schema_version":       true,
	"judgment_fts":         true,
	"judgment_fts_data":    true,
	"judgment_fts_idx":     true,
	"judgment_fts_docsize": true,
	"judgment_fts_config":  true,
}

// LeftBehindNames 는 이름 그대로 옮기지 않는 표 전부다(sqlite_ 접두어의 엔진 표는 빼고). 시험이 원본의 실제 표와 맞댄다.
func LeftBehindNames() []string {
	out := make([]string, 0, len(leftBehind))
	for n := range leftBehind {
		out = append(out, n)
	}
	return out
}

// Identity 는 표의 identity 칸이다. 없으면 false 다.
func (t Table) Identity() (Column, bool) {
	for _, c := range t.Cols {
		if c.Identity {
			return c, true
		}
	}
	return Column{}, false
}

// Col 은 표의 칸 name 이다. 없으면 nil 이다. 가리키는 것은 카탈로그의 칸이다 - 읽기만 한다.
func (t Table) Col(name string) *Column {
	for i := range t.Cols {
		if t.Cols[i].Name == name {
			return &t.Cols[i]
		}
	}
	return nil
}

// TableOf 는 이름이 name 인 옮기는 표다. 카탈로그 밖이면 false 다. store 의 원장 칸 관문이 쓰기 문장의 칸을 이것으로 찾는다.
func TableOf(name string) (Table, bool) {
	for _, t := range tables {
		if t.Name == name {
			return t, true
		}
	}
	return Table{}, false
}

// Tables 는 옮기는 표 전부다. FK 순서(부모가 먼저)이고 기준선의 선언 순서와 같다. 읽기만 한다.
//
// ★ 자기 참조(judgment.supersedes)는 순서로 못 푼다. 옮기기는 FK 검사를 커밋으로 미룬다(기준선의 FK 는 전부 DEFERRABLE 이다).
func Tables() []Table { return tables }

var tables = []Table{
	{Name: "project", Cols: []Column{key(txt("id")), nn(txt("path")), txt("remote_url"), nn(txt("default_branch")),
		txt("config"), txt("config_from_sha"), nn(txt("created_at")), txt("pinned_at"), txt("archived_at")}},
	{Name: "machine", Cols: []Column{key(txt("id")), nn(txt("hostname")), nn(txt("first_seen")), nn(txt("last_seen"))}},
	{Name: "session", Cols: []Column{key(txt("id")), ref(nn(txt("project"))), ref(nn(txt("machine_id"))),
		ref(nn(txt("worktree"))), ref(nn(txt("cc_session_id"))), txt("label"), nn(txt("state")), txt("blocked_why"),
		nn(txt("opened_at")), txt("harness")},
		unique: []Unique{{Name: "session_machine_id_worktree_cc_session_id_key", Cols: []string{"machine_id", "worktree", "cc_session_id"}}}},
	{Name: "session_workspace", Cols: []Column{key(txt("session_id")), ref(nn(txt("project"))), key(txt("path")), nn(num("is_primary"))}},
	{Name: "signal", Cols: []Column{key(txt("session_id")), key(txt("kind")), nn(txt("at"))}},
	{Name: "footprint", Cols: []Column{key(txt("session_id")), key(txt("path")), key(txt("origin")),
		nn(txt("first_at")), nn(txt("last_at"))}},
	{Name: "ref_state", Cols: []Column{key(txt("project")), key(txt("ref")), nn(txt("sha")), txt("subject"), nn(txt("at"))}},
	{Name: "change_set", Cols: []Column{key(txt("project")), key(txt("base_sha")), key(txt("head_sha")),
		doc(nn(txt("paths"))), nn(txt("computed_at"))}},
	{Name: "item", Cols: []Column{key(txt("project")), key(txt("id")), nn(txt("title")), nn(txt("body")),
		doc(nn(txt("paths"))), doc(nn(txt("labels"))), nn(txt("state")), txt("close_reason"), txt("landed_ref"),
		nn(txt("created_at")), txt("closed_at")}},
	{Name: "item_after", Cols: []Column{ref(nn(txt("project"))), ref(nn(txt("item_id"))), ref(txt("dep_item")),
		ref(txt("dep_job")), ref(txt("dep_sha")), ref(txt("dep_project")), rowid(ident(num("seq")))}},
	{Name: "claim", Cols: []Column{key(txt("project")), key(txt("item_id")), ref(nn(txt("session_id"))), nn(txt("at")),
		txt("released_at"), txt("force_reason")}},
	{Name: "pick_eval", AutoIncrement: true, Cols: []Column{ident(num("id")), ref(nn(txt("project"))),
		ref(nn(txt("session_id"))), nn(txt("at")), ref(txt("picked")), doc(nn(txt("rejected"))), doc(txt("picked_with"))}},
	{Name: "judgment", Cols: []Column{key(txt("id")), ref(txt("project")), ref(txt("session_id")), nn(txt("at")),
		nn(txt("kind")), txt("title"), nn(txt("body")), ref(txt("supersedes"))}},
	{Name: "judgment_link", Cols: []Column{key(txt("judgment_id")), key(txt("target_kind")), key(txt("target_id")),
		ref(txt("target_project"))}},
	{Name: "snapshot", DigestKey: "key", Cols: []Column{key(txt("project")), key(txt("key")), nn(txt("value")),
		nn(txt("method")), txt("evidence"), txt("input_digest"), nn(txt("computed_at"))}},
	{Name: "counter", Cols: []Column{key(txt("project")), key(txt("name")), nn(num("value"))}},
	{Name: "resource_hold", AutoIncrement: true, Cols: []Column{ident(num("id")), ref(nn(txt("project"))),
		ref(nn(txt("resource"))), ref(txt("session_id")), ref(txt("job_id")), nn(txt("acquired_at")), txt("released_at"),
		txt("force_reason")},
		unique: []Unique{{Name: "resource_one_holder", Cols: []string{"project", "resource"}, WhereNull: "released_at"}}},
	{Name: "job", Cols: []Column{key(txt("id")), ref(nn(txt("project"))), nn(txt("kind")), ref(txt("item_id")),
		ref(txt("session_id")), txt("branch"), txt("recipe"), nn(txt("state")), txt("fail_kind"), nn(txt("queued_at")), txt("started_at"),
		txt("ended_at"), txt("beat_at"), txt("base_sha"), txt("head_sha"), txt("landed_sha"), txt("image_tag"),
		txt("ack_at"), txt("log_tail")}},
	{Name: "event", AutoIncrement: true, Cols: []Column{ident(num("id")), nn(txt("at")),
		ref(capped(EventProjectMaxBytes, txt("project"))), ref(txt("session_id")),
		capped(EventKindMaxBytes, nn(txt("kind"))), doc(nn(txt("payload")))}},
	{Name: "idempotency", Cols: []Column{key(txt("key")), nn(txt("fingerprint")), nn(num("status")), nn(txt("ctype")),
		nn(blob("body")), nn(txt("at"))}},
	{Name: "landing_queue", AutoIncrement: true, Cols: []Column{ident(num("id")), ref(nn(txt("project"))),
		ref(nn(txt("session_id"))), nn(txt("enqueued_at")), txt("left_at"), txt("left_kind"), txt("left_detail")},
		unique: []Unique{{Name: "landing_queue_one_live_per_session", Cols: []string{"project", "session_id"}, WhereNull: "left_at"}}},
	{Name: "landing_queue_resource", Cols: []Column{key(num("row_id")), key(txt("resource"))}},
	{Name: "project_member", Cols: []Column{key(txt("root_project")), key(txt("member_project")), nn(txt("path")),
		nn(txt("declared_at"))}},
	{Name: "item_revision", Cols: []Column{key(txt("project")), key(txt("item_id")), key(num("rev")), nn(txt("at")),
		ref(txt("session_id")), nn(txt("title")), nn(txt("body")), doc(nn(txt("paths"))), nn(txt("reason"))}},
	{Name: "session_inbox", Cols: []Column{key(txt("session_id")), ref(nn(txt("machine_id"))), ref(nn(txt("socket"))),
		nn(txt("mode")), nn(txt("seen_at"))},
		unique: []Unique{{Name: "session_inbox_socket", Cols: []string{"machine_id", "socket"}}}},
}

func txt(name string) Column  { return Column{Name: name, Kind: Text} }
func num(name string) Column  { return Column{Name: name, Kind: Int} }
func blob(name string) Column { return Column{Name: name, Kind: Bytes} }
func nn(c Column) Column      { c.NotNull = true; return c }
func key(c Column) Column     { c.Key, c.NotNull = true, true; return ref(c) }
func ref(c Column) Column     { c.Identifier = true; return c }
func doc(c Column) Column     { c.JSON = true; return c }
func ident(c Column) Column   { c.Identity = true; return key(c) }
func rowid(c Column) Column   { c.Rowid = true; return c }

func capped(n int, c Column) Column { c.MaxBytes = n; return c }

// Fault 는 사전 검사가 칸 하나를 거절한 갈래다. 하나라도 걸리면 이관은 아무것도 안 쓰고, 걸린 행을 표 · 행 좌표 · 칸으로 낸다.
// 고치는 것은 원본 사본에서 사람이 한다. 이관이 스스로 바꿔 넣는 것은 되돌릴 수 있는 규칙이 정해진 갈래(Rewrite - 자유 글의
// NUL)뿐이고, 이관의 대조는 원본 값에 같은 규칙을 건 뒤 맞댄다 - 그 밖의 값을 짐작으로 고쳐 넣으면 전 칸 대조가 설 자리가 없다.
type Fault string

const (
	// FaultKeyNull: SQLite 는 정수 아닌 기본키에 NULL 을 받는다. 기준선의 기본키는 NOT NULL 이다.
	FaultKeyNull Fault = "기본키 NULL"
	// FaultNull: NOT NULL 칸의 NULL.
	FaultNull Fault = "NOT NULL 칸의 NULL"
	// FaultNUL: 변환 없이 맞대는 값(SettledCell - 옮긴 대상, 역이관의 Postgres 원본, 역이관이 낸 SQLite 파일)의 글 칸에 NUL
	// 바이트가 있다. Postgres 에서는 날 수 없고, 역이관이 낸 파일에 있으면 낸 뒤에 무엇이 넣은 것이다. 이관의 SQLite 원본에 든
	// NUL 은 자유 글이면 바꿔 넣고(RewriteNUL) 식별자 칸이면 FaultIdentNUL 이다.
	FaultNUL Fault = "NUL 바이트"
	// FaultIdentNUL: 이관의 SQLite 원본에서 식별자 칸(Column.Identifier - 기본키 · UNIQUE · 다른 행을 가리키는 칸)의 NUL
	// 바이트. 바꿔 넣지 않는다 - 바꾼 값은 원래 `\x00` 네 글자를 담은 다른 이름과 같아져, 그 이름 공간에서 두 값이 합쳐진다: FK
	// 없는 참조(item_after.dep_project)가 없던 프로젝트 대신 그 이름의 실제 프로젝트를 가리키고, 표 안의 겹침 재기
	// (FaultUnique)로는 그것이 안 보인다. 그래서 이 갈래의 NUL 은 원천에서 거절한다 - 식별자는 서버가 짓거나 사람이 짓는 짧은
	// 이름이라 NUL 이 든 값은 고장이다.
	FaultIdentNUL Fault = "식별자 칸의 NUL"
	// FaultUTF8: UTF8 DB 의 text 는 깨진 바이트열을 거절한다(SQLite 는 그대로 둔다).
	FaultUTF8 Fault = "깨진 UTF-8"
	// FaultStorage: 선언 타입으로 옮길 수 없는 값(정수 칸의 글, 글 칸의 실수 등). SQLite 는 칸 타입을 강제하지 않는다.
	FaultStorage Fault = "저장 갈래가 칸 타입과 다르다"
	// FaultJSON: JSON 칸이 JSON 문서가 아니다. 날 NUL 바이트가 든 JSON 칸도 여기다 - 글 안의 제어 문자는 이스케이프해야 하므로
	// 그 칸은 처음부터 문서가 아니었고, 그 바이트를 `\x00` 로 바꿔도 문서가 되지 않는다(`\x` 는 JSON 이스케이프가 아니다).
	FaultJSON Fault = "깨진 JSON"
	// FaultJSONSurrogate: JSON 문서의 글(키 포함)에 짝 없는 서로게이트 이스케이프(홀로 선 `\ud800`)가 있다. jsonb 는 그 문서를
	// 거절하고, 디코드하면 U+FFFD 로 바뀌어 다른 두 글이 같아진다. 서버(json.Marshal)는 그렇게 쓰지 않는다. 이관의 사전 검사
	// (Cell)에서만 난다 - SQLite 의 JSON 함수는 그 문서를 읽으므로 역이관 · 그 파일의 대조(SettledCell)는 원문 그대로 둔다.
	// 행 좌표에 그 글의 자리(JSON Pointer)를 싣는다(FaultLabel).
	FaultJSONSurrogate Fault = "JSON 안의 짝 없는 서로게이트"
	// FaultJSONKeyNUL: JSON 문서의 키(멤버 이름)에 든 U+0000(`\u0000`). 바꿔 넣지 않는다 - 키는 그 객체 안의 식별자라, 바꾼 키가
	// 원래 `\x00` 네 글자를 담은 옆 키와 같아지면(`{"\u0000":1,"\\x00":2}`) jsonb 가 한 멤버를 버린다. 행 좌표에 그 키의
	// 자리(JSON Pointer)를 싣는다(FaultLabel). 이관의 사전 검사(Cell)에서만 난다.
	FaultJSONKeyNUL Fault = "JSON 키 안의 U+0000"
	// FaultUnique: 기본키 · UNIQUE 칸 묶음(Table.Uniques)의 옮길 값이 앞의 행과 같다. 기준선은 뒤 행을 거절한다. SQLite 에서는
	// 달랐던 두 값이 옮기며 같아진 자리다 - 글 칸의 BLOB 이 같은 바이트의 글과 같아진다. NUL 로는 나지 않는다: 그 묶음의 칸은
	// 전부 식별자 칸이라 NUL 은 FaultIdentNUL 로 먼저 걸리고 바꿔 넣지 않는다. 칸 자리에 칸 묶음을, 좌표에 뒤 행의 옮길 값을 싣는다.
	FaultUnique Fault = "기본키 · UNIQUE 겹침"
	// FaultForeignKey: 부모가 없는 행. 기준선은 커밋 때 거절한다.
	FaultForeignKey Fault = "FK 부모가 없다"
	// FaultTooLong: 칸 상한(Column.MaxBytes)을 넘는 글. Postgres 의 B-tree 인덱스가 그 항목을 못 받는다(SQLite 는 받는다).
	// 이관의 사전 검사(ImportCell)에서만 난다.
	FaultTooLong Fault = "인덱스 키 상한을 넘는 글"
)

// Rewrite 는 이관이 SQLite 원본의 칸 하나를 Postgres 가 받는 꼴로 정해진 규칙에 따라 바꿔 넣은 갈래다(Cell). 거절이 아니라
// 보고다 - 이관 · 대조가 표 · 칸 · 규칙 · 행 좌표와 바꾼 행 수를 내고, 이관은 바꾼 칸 전부를 원래 바이트와 함께 기록 파일로
// 남긴다(조용히 바꾸지 않는다). 원본 파일은 바꾸지 않는다.
type Rewrite string

const (
	// RewriteNUL: 자유 글 칸(식별자 칸이 아닌 글 칸)의 NUL 바이트(0x00). Postgres text 는 NUL 을 못 담는다 - 칸 전체에
	// nulesc.Escape 를 걸어 `\x00` 네 글자로 옮긴다. 식별자 칸의 NUL 은 FaultIdentNUL 이다.
	RewriteNUL Rewrite = `NUL 바이트 → \x00`
	// RewriteJSONNUL: JSON 칸의 글 값(키 아님)에 든 U+0000(문서의 `\u0000`). U+0000 이 든 글 토큰에만 글 칸과 같은 규칙을 건다
	// (nulesc.EscapeJSON) - 그 토큰은 `\\x00` 로 쓰인다(디코드하면 `\x00` 네 글자). jsonb 는 `\u0000` 이 든 문서를 거절한다(22P05).
	// 키의 U+0000 은 FaultJSONKeyNUL 이다.
	RewriteJSONNUL Rewrite = `JSON 안의 \u0000 → \x00`
)

// Cell 은 SQLite 원본에서 읽은 값 v 를 칸 c 의 기준선 값으로 옮긴다. 옮긴 값은 nil · int64 · string · []byte 중 하나다.
// 옮길 수 없는 값이면 Fault 를 함께 낸다 - 그때도 값은 받은 그대로(글이면 그 바이트) 돌려줘 행 해시는 잴 수 있다. 정해진
// 규칙으로 바꿔 넣은 값이면 바꾼 값과 그 갈래(Rewrite)를 낸다. 셋째 값과 둘째 값은 함께 차지 않는다.
//
// ★ 이관의 원본 훑기 · 옮기기(그 위의 ImportCell), `fd migrate verify` 의 원본 쪽이 이것 하나로 읽는다 - 옮긴 값과 대조가
// 맞대는 원본 값이 같은 규칙을 지난다. 변환 없이 맞대는 쪽(옮긴 대상, 역이관의 원본, 역이관이 낸 파일)은 SettledCell 이다.
//
// ★ 값을 바꾸지 않고 옮기는 갈래 둘을 받는다: 글 칸에 든 BLOB(같은 바이트가 UTF-8 이면 글이다)과 bytea 칸에 든 TEXT.
// 바이트가 그대로이고, 옮긴 값의 타입이 칸 타입이라 대상에서 읽은 값과 행 해시의 표지까지 같다. 바이트가 바뀌어야 하는
// 갈래(정수 ↔ 글, 실수)는 거절하고 원래 타입을 그대로 둔다 - 그 값의 행 해시는 대상과 표지가 다르다.
// ★ 바이트를 바꿔 넣는 갈래는 자유 글의 NUL 하나다(Rewrite). Postgres 가 받을 길이 없고 되돌릴 수 있는 규칙(nulesc.Escape)이
// 있다. 식별자 자리(식별자 칸 · JSON 키)의 NUL 은 바꾸지 않고 거절한다(FaultIdentNUL · FaultJSONKeyNUL) - 바꾼 이름이 원래
// `\x00` 네 글자를 담은 다른 이름과 같아질 길을 원천에서 막는다.
//
// ★ 사전 검사를 지난 값(Fault 가 빈)의 타입은 언제나 nil 이거나 칸 타입의 꼴이다(Text → string, Int → int64, Bytes →
// []byte). 행 해시가 이것에 기댄다(RowHash 의 ★).
func Cell(c Column, v any) (any, Fault, Rewrite) { return convert(c, v, importText) }

// convert 는 읽은 값 v 를 칸 c 의 타입으로 옮기는 몫이다(Cell · SettledCell 이 같다). 글 칸에 든 글(BLOB 이면 같은 바이트의
// 글)은 규칙 text 가 잰다 - 이관의 원본(importText)과 변환 없이 맞대는 값(settledText)이 갈리는 자리는 그것 하나다.
func convert(c Column, v any, text func(Column, string) (string, Fault, Rewrite)) (any, Fault, Rewrite) {
	switch x := v.(type) {
	case nil:
		switch {
		case c.Key:
			return nil, FaultKeyNull, ""
		case c.NotNull:
			return nil, FaultNull, ""
		}
		return nil, "", ""
	case int64:
		if c.Kind != Int {
			return x, FaultStorage, ""
		}
		return x, "", ""
	case string:
		switch c.Kind {
		case Int:
			return x, FaultStorage, ""
		case Bytes:
			return []byte(x), "", ""
		}
		return text(c, x)
	case []byte:
		switch c.Kind {
		case Int:
			return x, FaultStorage, ""
		case Bytes:
			// ★ 길이 0 BLOB 은 빈 값이지 NULL 이 아니다. SQLite 드라이버는 그것을 nil 조각으로 주고, pgx 는 nil 조각을
			//   NULL 로 보낸다(NOT NULL 칸이면 23502). 비지 않은 조각으로 바꿔 빈 bytea 로 넣는다.
			if x == nil {
				x = []byte{}
			}
			return x, "", ""
		}
		return text(c, string(x))
	}
	return v, FaultStorage, ""
}

// SettledCell 은 변환 없이 맞대는 값 v 를 잰다: 옮긴 대상(이관 · 역이관의 커밋 전 대조, `fd migrate verify` 의 Postgres 쪽),
// 역이관의 원본(Postgres), 역이관이 낸 SQLite 파일(`fd migrate verify --exported`)이다. 값을 바꾸지 않는다 - 셋째 값은 늘 비었다.
// 거절은 어느 한쪽 엔진이 못 담거나 못 읽는 값뿐이다(settledText - 깨진 UTF-8, 깨진 JSON, 글 칸의 NUL 바이트)에 더해, 기본키 ·
// NOT NULL 칸의 NULL 과 저장 갈래가 칸 타입과 다른 값(FaultStorage, Text → 글 · Int → 정수 · Bytes → BLOB 이어야 한다)이다.
// 걸리면 값은 받은 그대로 돌려줘 행 해시의 표지도 원본과 갈린다.
//
// ★ 바꿔 넣기는 이관의 SQLite 원본만의 사정이다. 변환 없이 맞대는 쪽에 그 규칙을 걸면 두 쪽이 다른 값을 같다고 한다: 역이관이
// 낸 파일에 넣은 NUL 이 Postgres 의 `\x00` 네 글자와 같아진다. 그래서 두 쪽을 다 원문 그대로 맞댄다.
// ★ 이관이 jsonb 때문에 거는 규칙(JSON 글 · 키의 `\u0000`, 짝 없는 서로게이트)도 걸지 않는다. 두 엔진 다 글 칸이라 원문 그대로
// 담고 SQLite 의 JSON 함수도 그 문서를 읽는다 - Postgres 판 서버가 쓴 그런 값(쓰기 길에 원장 칸 관문이 서기 전 판이 쓴 것) 하나에
// 역이관(72시간 되돌리기)이 서면 안 된다.
// 그 파일을 다시 Postgres 로 옮길 때 이관의 사전 검사가 그 행을 바꿔 넣거나 좌표와 함께 거절한다(인덱스 키 상한과 같은 자리다).
// ★ Cell 이 받는 두 저장 갈래(글 칸의 BLOB, bytea 칸의 TEXT)도 원본의 사정이다 - SQLite 원본은 칸 타입을 강제하지 않아
// 그렇게 쌓였을 수 있고, 바이트가 그대로라 옮겨도 된다. 대상에 다른 갈래로 남았으면 넣은 뒤에 무엇이 바꾼 것이다. 바이트가
// 같아 지문으로는 안 갈리므로 여기서 잰다. SQLite 쪽의 자리다 - Postgres 는 엔진이 칸 타입을 강제해 늘 맞는다.
func SettledCell(c Column, v any) (any, Fault, Rewrite) {
	out, f, _ := convert(c, v, settledText)
	switch {
	case f == FaultNUL:
		return v, f, ""
	case f == "" && v != nil && !storedAs(c.Kind, v):
		return v, FaultStorage, ""
	}
	return out, f, ""
}

// ImportCell 은 SQLite 원본에서 읽어 Postgres 로 **옮겨 넣을** 값 v 를 잰다: Cell 의 검사 · 바꿔 넣기에 더해, 칸 상한
// (Column.MaxBytes)을 넘는 글이면 FaultTooLong 이다. 상한은 옮겨 넣을 바이트(바꿔 넣은 값)로 잰다. 이관의 사전 검사 · 옮기기가
// 이것을 쓴다.
//
// ★ 상한은 Cell 에 두지 않는다. Cell 은 대조(verify)도 지나고 역이관(Postgres → SQLite)은 그 위의 SettledCell 을 지나는데, 그
// 길의 값은 이미 원장에 있는 원문이다 - 상한에 걸려 그 길이 막히면 남는 처방이 감사 행을 자르는 것뿐이고, 그러면 두 원장이
// 달라진다. Postgres 의 인덱스가 이미 받은 값(짧은 kind 옆의 2048 바이트 넘는 project)도 칸별 상한에는 걸린다.
func ImportCell(c Column, v any) (any, Fault, Rewrite) {
	out, f, rw := Cell(c, v)
	if f != "" || c.MaxBytes == 0 {
		return out, f, rw
	}
	if s, ok := out.(string); ok && len(s) > c.MaxBytes {
		return out, FaultTooLong, ""
	}
	return out, "", rw
}

// storedAs 는 읽은 값 v 의 Go 타입이 칸 타입 k 의 저장 갈래인가다(드라이버가 저장 갈래마다 정한 타입으로 준다).
func storedAs(k Kind, v any) bool {
	switch v.(type) {
	case int64:
		return k == Int
	case string:
		return k == Text
	case []byte:
		return k == Bytes
	}
	return false
}

// importText 는 이관의 SQLite 원본 글 칸에 든 글 s 를 잰다: 깨진 UTF-8 은 거절하고, JSON 칸은 jsonCell 이 잰다. NUL 이 든 글은
// 식별자 칸(Column.Identifier)이면 거절하고(FaultIdentNUL) 자유 글 칸이면 nulesc.Escape 로 바꿔 넣는다(RewriteNUL). NUL 이 없는
// 글은 한 바이트도 안 바뀐다.
func importText(c Column, s string) (string, Fault, Rewrite) {
	switch {
	case !utf8.ValidString(s):
		return s, FaultUTF8, ""
	case c.JSON:
		return jsonCell(s)
	case strings.IndexByte(s, 0) < 0:
		return s, "", ""
	case c.Identifier:
		return s, FaultIdentNUL, ""
	}
	return nulesc.Escape(s), "", RewriteNUL
}

// settledText 는 변환 없이 맞대는 글 칸의 글 s 를 잰다(SettledCell). 어느 한쪽 엔진이 못 담거나 못 읽는 글만 거절한다: 깨진
// UTF-8(Postgres text 가 못 담는다), JSON 칸이 문서가 아닌 것(FaultJSON - 두 엔진의 JSON 함수가 다 못 읽는다, 날 NUL 이 든 JSON
// 도 여기다), 글 칸의 NUL 바이트(FaultNUL - Postgres text 가 못 담는다).
func settledText(c Column, s string) (string, Fault, Rewrite) {
	switch {
	case !utf8.ValidString(s):
		return s, FaultUTF8, ""
	case c.JSON && !json.Valid([]byte(s)):
		return s, FaultJSON, ""
	case strings.IndexByte(s, 0) >= 0:
		return s, FaultNUL, ""
	}
	return s, "", ""
}

// jsonCell 은 이관의 SQLite 원본 JSON 칸 하나를 잰다(글 바이트의 깨진 UTF-8 은 importText 가 먼저 걸렀다): 문서가 아니면
// FaultJSON, 짝 없는 서로게이트가 있으면 FaultJSONSurrogate, 키에 U+0000 이 있으면 FaultJSONKeyNUL, 글 값에 U+0000 이 있으면 그
// 글 토큰만 원문 바이트에서 고친 문서와 RewriteJSONNUL 이다(nulesc.EscapeJSON). 같은 문서의 다른 글 · 공백 · 수 · 순서 ·
// 이스케이프는 원래 바이트다.
//
// ★ U+0000 은 문서에 `\u0000` 으로만 쓰인다. 날 NUL 바이트가 든 칸은 처음부터 문서가 아니고(글 안의 제어 문자는 이스케이프해야
// 한다), 그 바이트를 `\x00` 로 바꿔도 문서가 되지 않는다(`\x` 는 JSON 이스케이프가 아니다) - FaultJSON 이다.
func jsonCell(s string) (string, Fault, Rewrite) {
	out, changes, err := nulesc.EscapeJSON(s)
	switch {
	case errors.Is(err, nulesc.ErrSurrogate):
		return s, FaultJSONSurrogate, ""
	case errors.Is(err, nulesc.ErrKeyNUL):
		return s, FaultJSONKeyNUL, ""
	case err != nil:
		return s, FaultJSON, ""
	case len(changes) > 0:
		return out, "", RewriteJSONNUL
	}
	return s, "", ""
}

// RewritePaths 는 Cell 이 RewriteJSONNUL 로 바꾼 JSON 칸의 원래 값 v(원본에서 읽은 그대로)에서 고친 글의 자리(RFC 6901 JSON
// Pointer)다. 글 칸의 RewriteNUL 은 칸 전체가 단위라 자리가 없다(nil). 이관이 바꾼 칸의 기록에 싣는다.
func RewritePaths(c Column, v any) []string {
	s, ok := docText(c, v)
	if !ok {
		return nil
	}
	_, changes, _ := nulesc.EscapeJSON(s)
	return changes
}

// FaultLabel 은 거절 f 에 걸린 칸 c(원래 값 v)의 좌표를 행 좌표 key 뒤에 잇는다. JSON 글 하나를 거절한 갈래(FaultJSONSurrogate ·
// FaultJSONKeyNUL)면 그 글의 자리(RFC 6901 JSON Pointer, 따옴표로 감싸 NUL 이 보인다)를 「JSON "…"」로 덧붙인다 - 큰 문서
// (event.payload)에서 고칠 글을 찾게. 칸 전체가 단위인 갈래면 key 그대로다. 칸 규칙에 걸린 칸을 세는 자리(store 의 cellTally)가
// 이것으로 좌표를 적는다.
func FaultLabel(key string, c Column, v any, f Fault) string {
	s, ok := docText(c, v)
	if !ok || (f != FaultJSONSurrogate && f != FaultJSONKeyNUL) {
		return key
	}
	var r *nulesc.Refusal
	if _, _, err := nulesc.EscapeJSON(s); !errors.As(err, &r) {
		return key
	}
	return key + " JSON " + LabelValue(r.Pointer)
}

// docText 는 JSON 칸 c 의 원래 값 v(글이나 BLOB)를 글로 낸다. JSON 칸이 아니거나 글 · BLOB 이 아니면 false 다.
func docText(c Column, v any) (string, bool) {
	if !c.JSON {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	}
	return "", false
}

// KeyDigest 는 snapshot 키의 지문이다. 기준선의 fd_key_digest(sha256(convert_to(key, 'UTF8')))와 같은 값이다.
func KeyDigest(key string) [32]byte { return sha256.Sum256([]byte(key)) }

// Uniques 는 표의 기본키 · UNIQUE 칸 묶음 전부다. 기본키(Key 칸, 선언 순서)가 먼저고 이름은 <표>_pkey 다.
func (t Table) Uniques() []Unique {
	pk := Unique{Name: t.Name + "_pkey"}
	for _, c := range t.Cols {
		if c.Key {
			pk.Cols = append(pk.Cols, c.Name)
		}
	}
	return append([]Unique{pk}, t.unique...)
}

// Mergeable 은 옮기며 SQLite 에서 달랐던 두 값이 같아질 수 있는 칸(글 · bytea)이 u 에 드는가다. 정수 칸만으로 된 묶음은 옮겨도
// 값이 그대로라(정수 칸의 다른 저장 갈래는 거절이다) SQLite 의 제약이 이미 지켰다 - 사전 검사가 그 묶음은 재지 않는다.
func (t Table) Mergeable(u Unique) bool {
	for _, n := range u.Cols {
		if t.Cols[t.Index(n)].Kind != Int {
			return true
		}
	}
	return false
}

// UniquePrint 는 행 vals(칸 선언 순서 · 옮길 값)의 칸 묶음 u 지문이다. 지문 칸(DigestKey)은 KeyDigest 로 잰다 - 기준선의 키가
// key_digest 다. 두 행의 지문이 같으면 기준선은 뒤 행을 거절한다. 그 묶음에 들지 않는 행이면 false 다: 칸 하나가 NULL 이거나
// (NULL 은 겹치지 않는다 - 기본키의 NULL 은 FaultKeyNull 이다), 부분 인덱스의 조건 칸(WhereNull)이 NULL 이 아니거나, 지문
// 칸이 글이 아니다(사전 검사에 이미 걸렸다).
func (t Table) UniquePrint(u Unique, vals []any) ([32]byte, bool) {
	if u.WhereNull != "" && vals[t.Index(u.WhereNull)] != nil {
		return [32]byte{}, false
	}
	key := make([]any, len(u.Cols))
	for i, n := range u.Cols {
		v := vals[t.Index(n)]
		if v == nil {
			return [32]byte{}, false
		}
		if n == t.DigestKey {
			s, ok := v.(string)
			if !ok {
				return [32]byte{}, false
			}
			d := KeyDigest(s)
			v = d[:]
		}
		key[i] = v
	}
	return RowHash(key), true
}

// SelectSQL 은 표의 칸을 선언 순서로 읽는 질의다. sqlite 면 Rowid 칸 자리에 rowid 를 읽는다.
func (t Table) SelectSQL(sqlite bool) string {
	return "SELECT " + strings.Join(t.columnNames(sqlite), ", ") + " FROM " + t.Name
}

// columnNames 는 표의 칸 이름을 선언 순서로 낸다. sqlite 면 Rowid 칸 자리가 rowid 다 - 읽기(SelectSQL)와 넣기(InsertSQL)가
// 같은 자리를 같은 이름으로 부른다.
func (t Table) columnNames(sqlite bool) []string {
	names := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		names[i] = c.Name
		if sqlite && c.Rowid {
			names[i] = "rowid"
		}
	}
	return names
}

// DeclaredColumns 는 원장에 선언돼 있어야 할 칸 이름이다(선언 순서). sqlite 면 rowid 자리를 뺀다 - SQLite 에서는 선언된 칸이
// 아니다. Postgres 는 칸 전부다(생성 칸인 지문은 카탈로그 밖이다).
func (t Table) DeclaredColumns(sqlite bool) []string {
	var out []string
	for _, c := range t.Cols {
		if !sqlite || !c.Rowid {
			out = append(out, c.Name)
		}
	}
	return out
}

// InsertSQL 은 표에 rows 행을 한 문장으로 넣는 INSERT 다(`?` 자리표시, 행마다 칸 수만큼). 칸 이름은 SelectSQL 과 같다.
//
//   - Postgres(sqlite 가 거짓): identity 칸이 있으면 OVERRIDING SYSTEM VALUE 를 붙인다 - GENERATED ALWAYS 칸에 옛 번호를
//     그대로 넣는 길은 그것뿐이다.
//   - SQLite: Rowid 칸 자리에 rowid 를 넣는다 - item_after 의 seq 값이 그 행의 rowid 로 돌아온다(순서의 축이 값 그대로다).
//     AUTOINCREMENT 표의 id 는 INTEGER PRIMARY KEY 라 넣은 값이 곧 rowid 다.
//
// ★ OVERRIDING 구절이 이 함수 안에 있어서 서버 코드의 OVERRIDING 을 훑는 시험(TestServerCodeNeverOverridesIdentity)이 이
// 함수를 부르는 자리를 못 본다. 부르는 자리는 원장 옮기기(store 의 copyTables - 이관과 역이관이 함께 쓴다) 하나여야 한다 -
// import_test.go 의 TestInsertSQLIsCalledOnlyByTheImport 가 소스에서 잠근다.
func (t Table) InsertSQL(rows int, sqlite bool) string {
	var b strings.Builder
	b.WriteString("INSERT INTO " + t.Name + " (" + strings.Join(t.columnNames(sqlite), ", ") + ")")
	if _, ok := t.Identity(); ok && !sqlite {
		b.WriteString(" OVERRIDING SYSTEM VALUE")
	}
	b.WriteString(" VALUES ")
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(t.Cols)), ", ") + ")"
	for i := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tuple)
	}
	return b.String()
}
