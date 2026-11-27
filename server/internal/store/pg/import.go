package pg

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
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
	// JSON 은 서버가 JSON 문서로 쓰는 칸이다(json.Marshal). Postgres 에서는 text 그대로지만 jsonb 로 읽힐 수 있는 칸이라
	// 깨진 문서와 \u0000 을 거절한다.
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
	// DigestKey 는 기준선의 기본키가 원문 대신 sha256 을 쓰는 칸이다(snapshot.key, fd_key_digest). 다른 Key 칸과 그 지문이
	// 같은 다른 행은 기준선이 넣기 전에 거절하므로 사전 검사가 먼저 센다(FaultDigest).
	DigestKey string
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

// Tables 는 옮기는 표 전부다. FK 순서(부모가 먼저)이고 기준선의 선언 순서와 같다. 읽기만 한다.
//
// ★ 자기 참조(judgment.supersedes)는 순서로 못 푼다. 옮기기는 FK 검사를 커밋으로 미룬다(기준선의 FK 는 전부 DEFERRABLE 이다).
func Tables() []Table { return tables }

var tables = []Table{
	{Name: "project", Cols: []Column{key(txt("id")), nn(txt("path")), txt("remote_url"), nn(txt("default_branch")),
		txt("config"), txt("config_from_sha"), nn(txt("created_at")), txt("pinned_at"), txt("archived_at")}},
	{Name: "machine", Cols: []Column{key(txt("id")), nn(txt("hostname")), nn(txt("first_seen")), nn(txt("last_seen"))}},
	{Name: "session", Cols: []Column{key(txt("id")), nn(txt("project")), nn(txt("machine_id")), nn(txt("worktree")),
		nn(txt("cc_session_id")), txt("label"), nn(txt("state")), txt("blocked_why"), nn(txt("opened_at")), txt("harness")}},
	{Name: "session_workspace", Cols: []Column{key(txt("session_id")), nn(txt("project")), key(txt("path")), nn(num("is_primary"))}},
	{Name: "signal", Cols: []Column{key(txt("session_id")), key(txt("kind")), nn(txt("at"))}},
	{Name: "footprint", Cols: []Column{key(txt("session_id")), key(txt("path")), key(txt("origin")),
		nn(txt("first_at")), nn(txt("last_at"))}},
	{Name: "ref_state", Cols: []Column{key(txt("project")), key(txt("ref")), nn(txt("sha")), txt("subject"), nn(txt("at"))}},
	{Name: "change_set", Cols: []Column{key(txt("project")), key(txt("base_sha")), key(txt("head_sha")),
		doc(nn(txt("paths"))), nn(txt("computed_at"))}},
	{Name: "item", Cols: []Column{key(txt("project")), key(txt("id")), nn(txt("title")), nn(txt("body")),
		doc(nn(txt("paths"))), doc(nn(txt("labels"))), nn(txt("state")), txt("close_reason"), txt("landed_ref"),
		nn(txt("created_at")), txt("closed_at")}},
	{Name: "item_after", Cols: []Column{nn(txt("project")), nn(txt("item_id")), txt("dep_item"), txt("dep_job"),
		txt("dep_sha"), txt("dep_project"), rowid(ident(num("seq")))}},
	{Name: "claim", Cols: []Column{key(txt("project")), key(txt("item_id")), nn(txt("session_id")), nn(txt("at")),
		txt("released_at"), txt("force_reason")}},
	{Name: "pick_eval", AutoIncrement: true, Cols: []Column{ident(num("id")), nn(txt("project")), nn(txt("session_id")),
		nn(txt("at")), txt("picked"), doc(nn(txt("rejected"))), doc(txt("picked_with"))}},
	{Name: "judgment", Cols: []Column{key(txt("id")), txt("project"), txt("session_id"), nn(txt("at")), nn(txt("kind")),
		txt("title"), nn(txt("body")), txt("supersedes")}},
	{Name: "judgment_link", Cols: []Column{key(txt("judgment_id")), key(txt("target_kind")), key(txt("target_id")),
		txt("target_project")}},
	{Name: "snapshot", DigestKey: "key", Cols: []Column{key(txt("project")), key(txt("key")), nn(txt("value")),
		nn(txt("method")), txt("evidence"), txt("input_digest"), nn(txt("computed_at"))}},
	{Name: "counter", Cols: []Column{key(txt("project")), key(txt("name")), nn(num("value"))}},
	{Name: "resource_hold", AutoIncrement: true, Cols: []Column{ident(num("id")), nn(txt("project")), nn(txt("resource")),
		txt("session_id"), txt("job_id"), nn(txt("acquired_at")), txt("released_at"), txt("force_reason")}},
	{Name: "job", Cols: []Column{key(txt("id")), nn(txt("project")), nn(txt("kind")), txt("item_id"), txt("session_id"),
		txt("branch"), txt("recipe"), nn(txt("state")), txt("fail_kind"), nn(txt("queued_at")), txt("started_at"),
		txt("ended_at"), txt("beat_at"), txt("base_sha"), txt("head_sha"), txt("landed_sha"), txt("image_tag"),
		txt("ack_at"), txt("log_tail")}},
	{Name: "event", AutoIncrement: true, Cols: []Column{ident(num("id")), nn(txt("at")),
		capped(EventProjectMaxBytes, txt("project")), txt("session_id"), capped(EventKindMaxBytes, nn(txt("kind"))),
		doc(nn(txt("payload")))}},
	{Name: "idempotency", Cols: []Column{key(txt("key")), nn(txt("fingerprint")), nn(num("status")), nn(txt("ctype")),
		nn(blob("body")), nn(txt("at"))}},
	{Name: "landing_queue", AutoIncrement: true, Cols: []Column{ident(num("id")), nn(txt("project")), nn(txt("session_id")),
		nn(txt("enqueued_at")), txt("left_at"), txt("left_kind"), txt("left_detail")}},
	{Name: "landing_queue_resource", Cols: []Column{key(num("row_id")), key(txt("resource"))}},
	{Name: "project_member", Cols: []Column{key(txt("root_project")), key(txt("member_project")), nn(txt("path")),
		nn(txt("declared_at"))}},
	{Name: "item_revision", Cols: []Column{key(txt("project")), key(txt("item_id")), key(num("rev")), nn(txt("at")),
		txt("session_id"), nn(txt("title")), nn(txt("body")), doc(nn(txt("paths"))), nn(txt("reason"))}},
	{Name: "session_inbox", Cols: []Column{key(txt("session_id")), nn(txt("machine_id")), nn(txt("socket")),
		nn(txt("mode")), nn(txt("seen_at"))}},
}

func txt(name string) Column  { return Column{Name: name, Kind: Text} }
func num(name string) Column  { return Column{Name: name, Kind: Int} }
func blob(name string) Column { return Column{Name: name, Kind: Bytes} }
func nn(c Column) Column      { c.NotNull = true; return c }
func key(c Column) Column     { c.Key, c.NotNull = true, true; return c }
func doc(c Column) Column     { c.JSON = true; return c }
func ident(c Column) Column   { c.Identity = true; return key(c) }
func rowid(c Column) Column   { c.Rowid = true; return c }

func capped(n int, c Column) Column { c.MaxBytes = n; return c }

// Fault 는 사전 검사가 칸 하나를 거절한 갈래다. 하나라도 걸리면 이관은 아무것도 안 쓰고, 걸린 행을 표 · 행 좌표 · 칸으로 낸다.
// 고치는 것은 원본 사본에서 사람이 한다 - 이관이 값을 바꿔 넣으면 전 칸 대조가 설 자리가 없다.
type Fault string

const (
	// FaultKeyNull: SQLite 는 정수 아닌 기본키에 NULL 을 받는다. 기준선의 기본키는 NOT NULL 이다.
	FaultKeyNull Fault = "기본키 NULL"
	// FaultNull: NOT NULL 칸의 NULL.
	FaultNull Fault = "NOT NULL 칸의 NULL"
	// FaultNUL: SQLite 는 글 칸의 0x00 을 보존하지만 Postgres text 는 못 받는다.
	FaultNUL Fault = "NUL 바이트"
	// FaultUTF8: UTF8 DB 의 text 는 깨진 바이트열을 거절한다(SQLite 는 그대로 둔다).
	FaultUTF8 Fault = "깨진 UTF-8"
	// FaultStorage: 선언 타입으로 옮길 수 없는 값(정수 칸의 글, 글 칸의 실수 등). SQLite 는 칸 타입을 강제하지 않는다.
	FaultStorage Fault = "저장 갈래가 칸 타입과 다르다"
	// FaultJSON: JSON 칸이 JSON 문서가 아니다.
	FaultJSON Fault = "깨진 JSON"
	// FaultJSONNUL: JSON 문서 안 글에 U+0000(\u0000)이 있다. jsonb 는 그 문서를 거절하고(22P05), jsonb 로 읽는 질의는 남의
	// 행에 든 것이라도 통째로 죽는다.
	FaultJSONNUL Fault = `JSON 안의 \u0000`
	// FaultDigest: 기준선의 기본키는 snapshot 키의 sha256 이다. 같은 프로젝트에서 지문이 같은 다른 키는 들어가지 못한다.
	FaultDigest Fault = "snapshot 키 지문 겹침"
	// FaultForeignKey: 부모가 없는 행. 기준선은 커밋 때 거절한다.
	FaultForeignKey Fault = "FK 부모가 없다"
	// FaultTooLong: 칸 상한(Column.MaxBytes)을 넘는 글. Postgres 의 B-tree 인덱스가 그 항목을 못 받는다(SQLite 는 받는다).
	// 이관의 사전 검사(ImportCell)에서만 난다.
	FaultTooLong Fault = "인덱스 키 상한을 넘는 글"
)

// Cell 은 한 원장에서 읽은 값 v 를 칸 c 의 기준선 값으로 옮긴다. 옮긴 값은 nil · int64 · string · []byte 중 하나다.
// 옮길 수 없는 값이면 Fault 를 함께 낸다 - 그때도 값은 받은 그대로(글이면 그 바이트) 돌려줘 행 해시는 잴 수 있다.
//
// ★ Postgres 에서 읽은 값도 이것을 지난다. 이미 그 꼴이라 그대로 나온다 - 그래서 두 엔진의 행 해시(RowHash)가 같은 값을 잰다.
//
// ★ 값을 바꾸지 않고 옮기는 갈래 둘을 받는다: 글 칸에 든 BLOB(같은 바이트가 UTF-8 이면 글이다)과 bytea 칸에 든 TEXT.
// 바이트가 그대로이고, 옮긴 값의 타입이 칸 타입이라 대상에서 읽은 값과 행 해시의 표지까지 같다. 바이트가 바뀌어야 하는
// 갈래(정수 ↔ 글, 실수)는 거절하고 원래 타입을 그대로 둔다 - 그 값의 행 해시는 대상과 표지가 다르다.
//
// ★ 사전 검사를 지난 값(Fault 가 빈)의 타입은 언제나 nil 이거나 칸 타입의 꼴이다(Text → string, Int → int64, Bytes →
// []byte). 행 해시가 이것에 기댄다(RowHash 의 ★).
func Cell(c Column, v any) (any, Fault) {
	switch x := v.(type) {
	case nil:
		switch {
		case c.Key:
			return nil, FaultKeyNull
		case c.NotNull:
			return nil, FaultNull
		}
		return nil, ""
	case int64:
		if c.Kind != Int {
			return x, FaultStorage
		}
		return x, ""
	case string:
		switch c.Kind {
		case Int:
			return x, FaultStorage
		case Bytes:
			return []byte(x), ""
		}
		return x, textFault(c, x)
	case []byte:
		switch c.Kind {
		case Int:
			return x, FaultStorage
		case Bytes:
			// ★ 길이 0 BLOB 은 빈 값이지 NULL 이 아니다. SQLite 드라이버는 그것을 nil 조각으로 주고, pgx 는 nil 조각을
			//   NULL 로 보낸다(NOT NULL 칸이면 23502). 비지 않은 조각으로 바꿔 빈 bytea 로 넣는다.
			if x == nil {
				x = []byte{}
			}
			return x, ""
		}
		s := string(x)
		return s, textFault(c, s)
	}
	return v, FaultStorage
}

// TargetCell 은 옮긴 대상에서 읽은 값 v 를 잰다: Cell 의 검사에 더해, 저장 갈래가 칸 타입 그대로여야 한다(Text → 글,
// Int → 정수, Bytes → BLOB). 걸리면 값은 받은 그대로 돌려줘 행 해시의 표지도 원본과 갈린다.
//
// ★ Cell 이 받는 두 갈래(글 칸의 BLOB, bytea 칸의 TEXT)는 원본의 사정이다 - SQLite 원본은 칸 타입을 강제하지 않아 그렇게
// 쌓였을 수 있고, 바이트가 그대로라 옮겨도 된다. 대상은 옮기기가 칸 타입의 값으로 넣은 원장이라 다른 갈래로 남았으면 넣은
// 뒤에 무엇이 바꾼 것이다. 바이트가 같아 지문으로는 안 갈리므로 여기서 잰다. SQLite 대상(역이관)의 자리다 - Postgres 대상은
// 엔진이 칸 타입을 강제해 늘 맞는다.
func TargetCell(c Column, v any) (any, Fault) {
	out, f := Cell(c, v)
	if f == "" && v != nil && !storedAs(c.Kind, v) {
		return v, FaultStorage
	}
	return out, f
}

// ImportCell 은 SQLite 원본에서 읽어 Postgres 로 **옮겨 넣을** 값 v 를 잰다: Cell 의 검사에 더해, 칸 상한(Column.MaxBytes)을
// 넘는 글이면 FaultTooLong 이다. 이관의 사전 검사가 이것을 쓴다.
//
// ★ 상한은 Cell 에 두지 않는다. Cell 은 대조(verify)와 역이관(Postgres → SQLite)도 지나는데, 그 길의 값은 이미 원장에 있는
// 원문이다 - 상한에 걸려 그 길이 막히면 남는 처방이 감사 행을 자르는 것뿐이고, 그러면 두 원장이 달라진다. Postgres 의 인덱스가
// 이미 받은 값(짧은 kind 옆의 2048 바이트 넘는 project)도 칸별 상한에는 걸린다.
func ImportCell(c Column, v any) (any, Fault) {
	out, f := Cell(c, v)
	if f != "" || c.MaxBytes == 0 {
		return out, f
	}
	if s, ok := out.(string); ok && len(s) > c.MaxBytes {
		return out, FaultTooLong
	}
	return out, ""
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

func textFault(c Column, s string) Fault {
	switch {
	case strings.IndexByte(s, 0) >= 0:
		return FaultNUL
	case !utf8.ValidString(s):
		return FaultUTF8
	case c.JSON:
		return jsonFault(s)
	}
	return ""
}

// jsonFault 는 JSON 칸 하나를 잰다. 글 바이트의 NUL · 깨진 UTF-8 은 textFault 가 먼저 걸렀다.
//
// ★ \u0000 은 디코드해서 잰다. 글자 `\u0000` 이 보여도 앞 백슬래시가 이스케이프된 것(`\\u0000`)이면 글은 백슬래시와
// 「u0000」이고 jsonb 가 받는다. 그 글자가 없으면 디코드한 글에도 U+0000 이 없다(빠른 길).
//
// ★ 수는 글 그대로 받는다(UseNumber). float64 로 옮기면 범위 밖 수(1e400)에서 토큰이 오류로 끝나 그 뒤의 글을 못 잰다.
// 끝은 io.EOF 하나다 - 그 밖의 오류는 json.Valid 를 지난 문서에서 날 자리가 없지만, 나면 끝으로 읽지 않고 깨진 문서로 낸다.
func jsonFault(s string) Fault {
	if !json.Valid([]byte(s)) {
		return FaultJSON
	}
	if !strings.Contains(s, `\u0000`) {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		switch {
		case errors.Is(err, io.EOF):
			return ""
		case err != nil:
			return FaultJSON
		}
		if str, ok := tok.(string); ok && strings.IndexByte(str, 0) >= 0 { // 키도 글 토큰으로 온다
			return FaultJSONNUL
		}
	}
}

// KeyDigest 는 snapshot 키의 지문이다. 기준선의 fd_key_digest(sha256(convert_to(key, 'UTF8')))와 같은 값이다.
func KeyDigest(key string) [32]byte { return sha256.Sum256([]byte(key)) }

// DigestPK 는 지문 기본키 표(DigestKey 가 있는 표)에서 행 vals 의 기준선 기본키다: 다른 Key 칸의 값과 DigestKey 칸의
// KeyDigest 를 이은 것(RowHash 로 접어 칸 경계가 모호하지 않다). 두 행의 값이 같으면 기준선은 뒤 행을 거절한다.
// 지문 칸이 없는 표이거나 그 값이 글이 아니면(사전 검사에 이미 걸렸다) false 다.
func (t Table) DigestPK(vals []any) (string, bool) {
	if t.DigestKey == "" {
		return "", false
	}
	var scope []any
	var key string
	ok := false
	for i, c := range t.Cols {
		switch {
		case c.Name == t.DigestKey:
			key, ok = vals[i].(string)
		case c.Key:
			scope = append(scope, vals[i])
		}
	}
	if !ok {
		return "", false
	}
	d := KeyDigest(key)
	h := RowHash(append(scope, d[:]))
	return string(h[:]), true
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
