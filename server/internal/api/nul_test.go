package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest/freshstore"
)

// 쓰기 길의 NUL 규칙(nul.go)을 소비자 좌표계(HTTP 응답 · 원장에 담긴 날 값)로 잰다. 두 엔진 모두에서 돈다 - 고치기 전에는
// Postgres 가 NUL 이 든 글 · 이름에 500(22021)을 내고 SQLite 는 그 글을 그대로 담았다.

var nulEngines = []freshstore.Engine{freshstore.SQLite, freshstore.Postgres}

// catalogColumn 은 이관 카탈로그(pg.Tables)의 칸 하나다.
func catalogColumn(t *testing.T, table, name string) pg.Column {
	t.Helper()
	for _, tb := range pg.Tables() {
		if tb.Name != table {
			continue
		}
		for _, c := range tb.Cols {
			if c.Name == name {
				return c
			}
		}
	}
	t.Fatalf("이관 카탈로그에 %s.%s 가 없다", table, name)
	return pg.Column{}
}

// rawCell 은 원장 칸 하나를 날 글 그대로 읽는다. 문장에 자리표를 안 쓴다(엔진마다 꼴이 다르다) - 시험이 지었거나 서버가 지은 id
// 만 싣는다.
func (e *env) rawCell(q string) string {
	e.t.Helper()
	var s string
	if err := e.st.DB().QueryRow(q).Scan(&s); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return s
}

func (e *env) rawCount(table string) int64 {
	e.t.Helper()
	var n int64
	if err := e.st.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		e.t.Fatalf("%s 세기: %v", table, err)
	}
	return n
}

func mustWrite(t *testing.T, w *httptest.ResponseRecorder, what string) *httptest.ResponseRecorder {
	t.Helper()
	if w.Code/100 != 2 {
		t.Fatalf("%s: %d %s", what, w.Code, w.Body.String())
	}
	return w
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 자유 글(글 칸 · JSON 칸의 값)의 NUL 은 두 엔진 모두 받아 같은 값으로 담는다. 그 값은 이관(pg.Cell)이 SQLite 판 서버가 같은
// 요청으로 담았을 원본(NUL 그대로의 글, json.Marshal 한 문서)을 옮긴 값과 바이트까지 같다. 이벤트 payload 에도 U+0000 이 남지
// 않는다.
func TestWriteNULStoresTheImportValueOnBothEngines(t *testing.T) {
	stored := map[freshstore.Engine]map[string]string{}
	for _, eng := range nulEngines {
		t.Run(string(eng), func(t *testing.T) {
			e := newEnvOn(t, eng, nil)
			sess := e.openSessionWith("cc-nul", map[string]any{"label": "라벨\x00", "hostname": "호스트\x00"})
			with := func(kv map[string]any) map[string]any {
				out := map[string]any{"project": testProject, "session_id": sess}
				maps.Copy(out, kv)
				return out
			}
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items", with(map[string]any{
				"id": "n1", "title": "제목\x00", "body": `원래 \x00 네 글자와 ` + "\x00",
				"labels": []string{"l\x00"},
			})), "add n1")
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items/n1/amend", with(map[string]any{
				"body": "고친\x00본문", "reason": "사유\x00",
			})), "amend n1")
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items", with(map[string]any{"id": "n2", "title": "둘", "body": "b"})), "add n2")
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items/n2/label", with(map[string]any{"add": []string{"a\x00"}})), "label n2")
			w := mustWrite(t, e.write(http.MethodPost, "/api/v1/judgments", with(map[string]any{
				"kind": "decision", "title": "판단\x00", "body": "판단 본문\x00",
			})), "note")
			j, _ := decodeBody(t, w)["judgment"].(map[string]any)
			jid, _ := j["ID"].(string)
			if jid == "" {
				t.Fatalf("판단 응답에 id 가 없다: %s", w.Body.String())
			}
			mustWrite(t, e.write(http.MethodPatch, "/api/v1/sessions/"+sess, map[string]any{"state": "blocked", "why": "막힘\x00"}), "patch")

			cells := []struct {
				key, table, col, query string
				orig                   any // SQLite 판 서버가 이 칸에 담았을 값(이관의 원본)
			}{
				{"session.label", "session", "label", "SELECT label FROM session WHERE id = '" + sess + "'", "라벨\x00"},
				{"machine.hostname", "machine", "hostname", "SELECT hostname FROM machine WHERE id = 'm1'", "호스트\x00"},
				{"session.blocked_why", "session", "blocked_why", "SELECT blocked_why FROM session WHERE id = '" + sess + "'", "막힘\x00"},
				{"item.title", "item", "title", "SELECT title FROM item WHERE id = 'n1'", "제목\x00"},
				{"item.body", "item", "body", "SELECT body FROM item WHERE id = 'n1'", "고친\x00본문"},
				{"item.labels", "item", "labels", "SELECT labels FROM item WHERE id = 'n1'", jsonText(t, []string{"l\x00"})},
				{"item.labels(label)", "item", "labels", "SELECT labels FROM item WHERE id = 'n2'", jsonText(t, []string{"a\x00"})},
				{"item_revision.body", "item_revision", "body", "SELECT body FROM item_revision WHERE item_id = 'n1'", `원래 \x00 네 글자와 ` + "\x00"},
				{"item_revision.reason", "item_revision", "reason", "SELECT reason FROM item_revision WHERE item_id = 'n1'", "사유\x00"},
				{"judgment.title", "judgment", "title", "SELECT title FROM judgment WHERE id = '" + jid + "'", "판단\x00"},
				{"judgment.body", "judgment", "body", "SELECT body FROM judgment WHERE id = '" + jid + "'", "판단 본문\x00"},
			}
			got := map[string]string{}
			for _, c := range cells {
				col := catalogColumn(t, c.table, c.col)
				if col.Identifier {
					t.Fatalf("%s 는 이관 카탈로그의 식별자 칸이다 - 자유 글 시험 표에 둘 수 없다", c.key)
				}
				want, fault, _ := pg.Cell(col, c.orig)
				if fault != "" {
					t.Fatalf("%s: 이관이 원본 %q 를 거절한다(%s)", c.key, c.orig, fault)
				}
				v := e.rawCell(c.query)
				if v != want {
					t.Errorf("%s = %q, 이관이 옮긴 값 %q", c.key, v, want)
				}
				if strings.IndexByte(v, 0) >= 0 || strings.Contains(v, `\u0000`) {
					t.Errorf("%s 에 NUL 이 남았다: %q", c.key, v)
				}
				got[c.key] = v
			}

			rows, err := e.st.DB().Query("SELECT payload FROM event")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			escaped := 0
			for rows.Next() {
				var p string
				if err := rows.Scan(&p); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(p, `\u0000`) {
					t.Errorf("이벤트 payload 에 U+0000 이 남았다: %s", p)
				}
				if strings.Contains(p, `\\x00`) {
					escaped++
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if escaped == 0 {
				t.Errorf("바꾼 글이 실린 이벤트가 하나도 없다 - 이벤트가 입력 글을 안 싣는다면 이 단정이 낡았다")
			}
			stored[eng] = got
		})
	}
	if len(stored) == len(nulEngines) && !reflect.DeepEqual(stored[freshstore.SQLite], stored[freshstore.Postgres]) {
		t.Errorf("두 엔진이 다른 값을 담았다:\nSQLite   %q\nPostgres %q", stored[freshstore.SQLite], stored[freshstore.Postgres])
	}
}

// 이름 자리(식별자 · 열거 · 항목 · 발자국 경로 · 선행 항목)의 NUL, JSON 키의 U+0000, 짝 없는 서로게이트, URL 의 NUL 은 두 엔진 모두
// 400 이고 아무것도 쓰지 않는다.
func TestWriteNULRefusesNamesOnBothEngines(t *testing.T) {
	for _, eng := range nulEngines {
		t.Run(string(eng), func(t *testing.T) {
			e := newEnvOn(t, eng, nil)
			sess := e.openSession("cc-nul")
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items", map[string]any{
				"project": testProject, "session_id": sess, "id": "base", "title": "t", "body": "b",
			}), "add base")
			items, judgments := e.rawCount("item"), e.rawCount("judgment")
			cases := []struct {
				name, method, path string
				body               any
				code               string
			}{
				{"항목 id", http.MethodPost, "/api/v1/items",
					map[string]any{"project": testProject, "session_id": sess, "id": "i\x00d", "title": "t", "body": "b"}, "nul_refused"},
				{"선행 항목", http.MethodPost, "/api/v1/items",
					map[string]any{"project": testProject, "session_id": sess, "id": "n3", "title": "t", "body": "b",
						"after": []map[string]any{{"item": "x\x00"}}}, "nul_refused"},
				{"판단 종류", http.MethodPost, "/api/v1/judgments",
					map[string]any{"project": testProject, "session_id": sess, "kind": "deci\x00sion", "body": "x"}, "nul_refused"},
				{"프로젝트", http.MethodPost, "/api/v1/judgments",
					map[string]any{"project": "c\x00p", "session_id": sess, "kind": "decision", "body": "x"}, "nul_refused"},
				{"항목 경로", http.MethodPost, "/api/v1/items",
					map[string]any{"project": testProject, "session_id": sess, "id": "n4", "title": "t", "body": "b",
						"paths": []string{"internal/a\x00.go"}}, "nul_refused"},
				{"발자국 경로", http.MethodPost, "/api/v1/sessions/" + sess + "/signals",
					map[string]any{"kind": "edit", "paths": []string{"f\x00"}}, "nul_refused"},
				{"JSON 키", http.MethodPost, "/api/v1/judgments",
					`{"project":"cp","session_id":"` + sess + `","kind":"decision","body":"x","\u0000":1}`, "nul_refused"},
				{"짝 없는 서로게이트", http.MethodPost, "/api/v1/judgments",
					`{"project":"cp","session_id":"` + sess + `","kind":"decision","body":"x\ud800"}`, "lone_surrogate"},
				{"URL 경로(쓰기)", http.MethodPost, "/api/v1/items/a%00b/amend",
					map[string]any{"project": testProject, "session_id": sess, "title": "t", "reason": "r"}, "nul_refused"},
				{"URL 경로(읽기)", http.MethodGet, "/api/v1/items/a%00b?project=" + testProject, nil, "nul_refused"},
				{"URL 질의", http.MethodGet, "/api/v1/judgments?q=%00", nil, "nul_refused"},
			}
			for _, c := range cases {
				w := e.write(c.method, c.path, c.body)
				if w.Code != http.StatusBadRequest {
					t.Errorf("%s: %d %s, 바라는 것 400", c.name, w.Code, w.Body.String())
					continue
				}
				if got := errorOf(t, w)["code"]; got != c.code {
					t.Errorf("%s: code %v, 바라는 것 %s", c.name, got, c.code)
				}
			}
			if n, m := e.rawCount("item"), e.rawCount("judgment"); n != items || m != judgments {
				t.Errorf("거절했는데 썼다: item %d→%d · judgment %d→%d", items, n, judgments, m)
			}
		})
	}
}

// 바꿔 받은 쓰기는 로그 한 줄로 남는다: 바꾼 칸의 자리 · 경로 · request_id(응답과 잇는 열쇠)를 싣고 글 내용은 안 싣는다.
func TestWriteNULLogsWhatWasRewritten(t *testing.T) {
	e := newEnv(t, nil)
	sess := e.openSession("cc-nul")
	w := mustWrite(t, e.write(http.MethodPost, "/api/v1/items", map[string]any{
		"project": testProject, "session_id": sess, "id": "n1", "title": "비밀\x00제목", "body": "b", "labels": []string{"ok", "l\x00"},
	}), "add")
	var hit []map[string]any
	for _, r := range e.logs.records(t) {
		if r["msg"] == "요청 글의 NUL 을 \\x00 표기로 바꿔 받았다" {
			hit = append(hit, r)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("바꿔 받은 로그 줄 %d 개, 바라는 것 1", len(hit))
	}
	r := hit[0]
	if !reflect.DeepEqual(r["fields"], []any{"/title", "/labels/1"}) || r["path"] != "/api/v1/items" ||
		r["request_id"] != w.Header().Get("X-Request-Id") || r["request_id"] == "" {
		t.Errorf("로그 줄 = %v", r)
	}
	if strings.Contains(e.logs.String(), "비밀") {
		t.Errorf("로그에 글 내용이 실렸다")
	}
}

func TestJudgeURLNUL(t *testing.T) {
	cases := []struct {
		raw, where string
		bad        bool
	}{
		{"/api/v1/items/a?project=cp", "", false},
		{"/api/v1/items/a%00b", "경로", true},
		{"/api/v1/items/a?project=c%00p", `질의 "project"`, true},
		{"/api/v1/items/a?b=1&a=%00&c=%00", `질의 "a"`, true},
		{"/api/v1/items/a?x%00y=1", "질의 이름", true},
		{"/api/v1/items/a?q=%5Cx00", "", false}, // `\x00` 네 글자는 NUL 이 아니다
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatal(err)
		}
		if where, bad := JudgeURLNUL(u); where != c.where || bad != c.bad {
			t.Errorf("%s: (%q, %v), 바라는 것 (%q, %v)", c.raw, where, bad, c.where, c.bad)
		}
	}
}
