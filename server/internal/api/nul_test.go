package api

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/nulesc"
	"github.com/kweiza/flightdeck/internal/store/pg"
	"github.com/kweiza/flightdeck/internal/store/storetest/freshstore"
)

// 쓰기 길의 NUL 규칙을 소비자 좌표계(HTTP 응답 · 원장에 담긴 날 값)로 잰다. 두 엔진 모두에서 돈다 - 고치기 전에는 Postgres 가
// NUL 이 든 글 · 이름 · 깨진 UTF-8 에 500(22021)을 내고 SQLite 는 그 글을 그대로 담았다.
//
// 바꿔 넣기는 store 의 원장 칸 관문 한 곳이 한다(입력은 거절만 한다 - nul.go). 그래서 이 시험의 정답은 언제나 이관이다: SQLite 판
// 서버가 같은 요청으로 그 칸에 담았을 원본을 pg.Cell 이 옮긴 값.

var nulEngines = []freshstore.Engine{freshstore.SQLite, freshstore.Postgres}

// importCell 은 이관이 원본 orig 를 칸 table.col 로 옮긴 값이다. 자유 글 칸이 아니거나 이관이 거절하면 시험을 세운다.
func importCell(t *testing.T, table, col string, orig any) string {
	t.Helper()
	tb, _ := pg.TableOf(table)
	c := tb.Col(col)
	if c == nil {
		t.Fatalf("이관 카탈로그에 %s.%s 가 없다", table, col)
	}
	if c.Identifier {
		t.Fatalf("%s.%s 는 이관 카탈로그의 식별자 칸이다 - 자유 글 시험 표에 둘 수 없다", table, col)
	}
	v, fault, _ := pg.Cell(*c, orig)
	if fault != "" {
		t.Fatalf("%s.%s: 이관이 원본 %q 를 거절한다(%s)", table, col, orig, fault)
	}
	s, _ := v.(string)
	return s
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

func (e *env) rawExec(q string) {
	e.t.Helper()
	if _, err := e.st.DB().Exec(q); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
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

// idOf 는 응답 본문의 절 sect 안 "ID" 다.
func idOf(t *testing.T, w *httptest.ResponseRecorder, sect string) string {
	t.Helper()
	m, _ := decodeBody(t, w)[sect].(map[string]any)
	id, _ := m["ID"].(string)
	if id == "" {
		t.Fatalf("응답의 %s 절에 id 가 없다: %s", sect, w.Body.String())
	}
	return id
}

// noNUL 은 원장 값 v 에 NUL 바이트도 JSON 의 U+0000 도 없는지 본다.
func noNUL(t *testing.T, what, v string) {
	t.Helper()
	if strings.IndexByte(v, 0) >= 0 || strings.Contains(v, `\u0000`) {
		t.Errorf("%s 에 NUL 이 남았다: %q", what, v)
	}
}

// 자유 글(글 칸 · JSON 칸의 값)의 NUL 은 두 엔진 모두 받아 같은 값으로 담는다. 그 값은 이관(pg.Cell)이 SQLite 판 서버가 같은
// 요청으로 담았을 원본(NUL 그대로의 글, json.Marshal 한 문서)을 옮긴 값과 바이트까지 같다. 원장에서 읽은 값을 다시 쓰는 엮인 칸
// (고친 항목의 이전 판)은 두 번 바뀌지 않는다. 이벤트 payload 에도 U+0000 이 남지 않는다.
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
			// 둘째 판의 이전 값은 원장에서 읽은 첫 고침이다 - 이미 원장의 꼴이라 다시 바뀌면 안 된다.
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items/n1/amend", with(map[string]any{
				"title": "새 제목", "reason": "둘째",
			})), "amend n1 둘째")
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items", with(map[string]any{"id": "n2", "title": "둘", "body": "b"})), "add n2")
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items/n2/label", with(map[string]any{"add": []string{"a\x00"}})), "label n2")
			jid := idOf(t, mustWrite(t, e.write(http.MethodPost, "/api/v1/judgments", with(map[string]any{
				"kind": "decision", "title": "판단\x00", "body": "판단 본문\x00",
			})), "note"), "judgment")
			mustWrite(t, e.write(http.MethodPatch, "/api/v1/sessions/"+sess, map[string]any{"state": "blocked", "why": "막힘\x00"}), "patch")

			// 마무리: 판단 · 폐기 사유 · 후속 항목(제목 · 본문 · 꼬리표)이 한 트랜잭션에 쓰인다.
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items", with(map[string]any{"id": "f1", "title": "끝낼 것", "body": "b"})), "add f1")
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items/f1/claim", with(nil)), "claim f1")
			fid := idOf(t, mustWrite(t, e.write(http.MethodPost, "/api/v1/items/f1/finish", with(map[string]any{
				"outcome": "dropped", "title": "폐기\x00", "body": "not-done: 폐기 본문\x00", "close_reason": "폐기 사유\x00",
				"followups": []map[string]any{{"id": "f1-next", "title": "후속\x00", "body": "후속 본문\x00", "labels": []string{"fl\x00"}}},
			})), "finish f1"), "judgment")

			// 수치 보관과 랜딩 이탈 사유.
			mustWrite(t, e.write(http.MethodPut, "/api/v1/snapshots/nul.key", map[string]any{
				"project": testProject, "value": "값\x00", "method": "manual", "evidence": "근거\x00",
			}), "snapshot")
			mustWrite(t, e.write(http.MethodPost, "/api/v1/landing", with(map[string]any{"mode": LandModeAcquire})), "land acquire")
			mustWrite(t, e.write(http.MethodPost, "/api/v1/landing", with(map[string]any{"mode": LandModeLeave, "detail": "이탈\x00"})), "land leave")

			cells := []struct {
				key, table, col, query string
				orig                   any // SQLite 판 서버가 이 칸에 담았을 값(이관의 원본)
			}{
				{"session.label", "session", "label", "SELECT label FROM session WHERE id = '" + sess + "'", "라벨\x00"},
				{"machine.hostname", "machine", "hostname", "SELECT hostname FROM machine WHERE id = 'm1'", "호스트\x00"},
				{"session.blocked_why", "session", "blocked_why", "SELECT blocked_why FROM session WHERE id = '" + sess + "'", "막힘\x00"},
				{"item.title", "item", "title", "SELECT title FROM item WHERE id = 'n1'", "새 제목"},
				{"item.body", "item", "body", "SELECT body FROM item WHERE id = 'n1'", "고친\x00본문"},
				{"item.labels", "item", "labels", "SELECT labels FROM item WHERE id = 'n1'", jsonText(t, []string{"l\x00"})},
				{"item.labels(label)", "item", "labels", "SELECT labels FROM item WHERE id = 'n2'", jsonText(t, []string{"a\x00"})},
				{"item_revision.body(1)", "item_revision", "body", "SELECT body FROM item_revision WHERE item_id = 'n1' AND rev = 1", `원래 \x00 네 글자와 ` + "\x00"},
				{"item_revision.reason(1)", "item_revision", "reason", "SELECT reason FROM item_revision WHERE item_id = 'n1' AND rev = 1", "사유\x00"},
				{"item_revision.body(2)", "item_revision", "body", "SELECT body FROM item_revision WHERE item_id = 'n1' AND rev = 2", "고친\x00본문"},
				{"item_revision.title(2)", "item_revision", "title", "SELECT title FROM item_revision WHERE item_id = 'n1' AND rev = 2", "제목\x00"},
				{"judgment.title", "judgment", "title", "SELECT title FROM judgment WHERE id = '" + jid + "'", "판단\x00"},
				{"judgment.body", "judgment", "body", "SELECT body FROM judgment WHERE id = '" + jid + "'", "판단 본문\x00"},
				{"finish judgment.title", "judgment", "title", "SELECT title FROM judgment WHERE id = '" + fid + "'", "폐기\x00"},
				{"finish judgment.body", "judgment", "body", "SELECT body FROM judgment WHERE id = '" + fid + "'", "not-done: 폐기 본문\x00"},
				{"item.close_reason", "item", "close_reason", "SELECT close_reason FROM item WHERE id = 'f1'", "폐기 사유\x00"},
				{"followup item.title", "item", "title", "SELECT title FROM item WHERE id = 'f1-next'", "후속\x00"},
				{"followup item.body", "item", "body", "SELECT body FROM item WHERE id = 'f1-next'", "후속 본문\x00"},
				{"followup item.labels", "item", "labels", "SELECT labels FROM item WHERE id = 'f1-next'", jsonText(t, []string{"fl\x00"})},
				{"snapshot.value", "snapshot", "value", "SELECT value FROM snapshot WHERE key = 'nul.key'", "값\x00"},
				{"snapshot.evidence", "snapshot", "evidence", "SELECT evidence FROM snapshot WHERE key = 'nul.key'", "근거\x00"},
				{"landing_queue.left_detail", "landing_queue", "left_detail", "SELECT left_detail FROM landing_queue WHERE session_id = '" + sess + "'", "이탈\x00"},
			}
			got := map[string]string{}
			for _, c := range cells {
				want := importCell(t, c.table, c.col, c.orig)
				v := e.rawCell(c.query)
				if v != want {
					t.Errorf("%s = %q, 이관이 옮긴 값 %q", c.key, v, want)
				}
				noNUL(t, c.key, v)
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
				noNUL(t, "이벤트 payload", p)
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

// 여러 입력을 이어 한 칸에 쓰는 자리(회수 · 반납 · 레인 회수의 판단 본문)는 **이은 글 하나**가 이관의 한
// 칸이다. 입력 하나(사유)만 바꾼 뒤 이으면, 다른 입력(행위자)에 원래 있던 `\x00` 네 글자가 그 칸을 되돌릴 때 NUL 로 읽혀 이관과
// 다른 바이트가 된다. 그래서 되돌린 글에 받은 사유(NUL 그대로)와 행위자(`\x00` 네 글자 그대로)가 함께 있어야 하고, 그 글을 이관이
// 옮긴 값이 원장 값이어야 한다.
func TestWriteNULComposedCellsAreOneImportCellOnBothEngines(t *testing.T) {
	const actor = `사람\x00`   // `\x00` 네 글자 - NUL 이 아니다
	const reason = "사유\x00끝" // NUL 이 든 사유
	for _, eng := range nulEngines {
		t.Run(string(eng), func(t *testing.T) {
			e := newEnvOn(t, eng, nil)
			holder := e.openSession("cc-holder")
			add := func(id string) {
				mustWrite(t, e.write(http.MethodPost, "/api/v1/items", map[string]any{
					"project": testProject, "session_id": holder, "id": id, "title": "t", "body": "b",
				}), "add "+id)
				mustWrite(t, e.write(http.MethodPost, "/api/v1/items/"+id+"/claim", map[string]any{
					"project": testProject, "session_id": holder,
				}), "claim "+id)
			}
			add("rc1")
			add("lv1")

			reclaim := decodeBody(t, mustWrite(t, e.write(http.MethodPost, "/api/v1/items/rc1/claim/release", map[string]any{
				"project": testProject, "actor": actor, "reason": reason,
			}), "reclaim"))
			leave := decodeBody(t, mustWrite(t, e.write(http.MethodPost, "/api/v1/claims/leave", map[string]any{
				"project": testProject, "session_id": holder, "item_id": "lv1", "reason": reason,
			}), "leave"))
			row := e.okBody(t, e.write(http.MethodPost, "/api/v1/landing", map[string]any{
				"project": testProject, "session_id": holder, "mode": LandModeAcquire,
			}), "land acquire")
			lane := decodeBody(t, mustWrite(t, e.write(http.MethodPost, "/api/v1/landing/rows/"+itoa(intOf(t, row, "row_id"))+"/release",
				map[string]any{"project": testProject, "actor": actor, "reason": reason}), "lane release"))

			composed := []struct {
				what, jid string
				actor     bool // 본문에 행위자가 실리는가
			}{
				{"회수 판단 본문", reclaim["judgment_id"].(string), true},
				{"반납 판단 본문", leave["judgment_id"].(string), false},
				{"레인 회수 판단 본문", lane["judgment_id"].(string), true},
			}
			for _, c := range composed {
				v := e.rawCell("SELECT body FROM judgment WHERE id = '" + c.jid + "'")
				noNUL(t, c.what, v)
				orig := nulesc.Unescape(v)
				if !strings.Contains(orig, reason) {
					t.Errorf("%s 를 되돌린 글에 받은 사유 %q 가 없다: %q", c.what, reason, orig)
				}
				if c.actor && !strings.Contains(orig, actor) {
					t.Errorf("%s 를 되돌린 글에 행위자 %q(`\\x00` 네 글자)가 그대로 없다 - 사유만 바꾼 뒤 이었다: %q", c.what, actor, orig)
				}
				if want := importCell(t, "judgment", "body", orig); v != want {
					t.Errorf("%s = %q, 이관이 이은 글을 옮긴 값 %q", c.what, v, want)
				}
			}

			// 사유가 그대로 담기는 칸.
			for _, c := range []struct{ what, table, col, query string }{
				{"회수 claim.force_reason", "claim", "force_reason", "SELECT force_reason FROM claim WHERE item_id = 'rc1'"},
				{"반납 claim.force_reason", "claim", "force_reason", "SELECT force_reason FROM claim WHERE item_id = 'lv1'"},
				{"레인 회수 landing_queue.left_detail", "landing_queue", "left_detail", "SELECT left_detail FROM landing_queue WHERE session_id = '" + holder + "'"},
				{"레인 회수 resource_hold.force_reason", "resource_hold", "force_reason", "SELECT force_reason FROM resource_hold WHERE session_id = '" + holder + "'"},
			} {
				if v, want := e.rawCell(c.query), importCell(t, c.table, c.col, reason); v != want {
					t.Errorf("%s = %q, 이관이 옮긴 값 %q", c.what, v, want)
				}
			}

			// 프로젝트 지우기: 항목 · 판단이 없는 프로젝트라야 지운다.
			e.openIn(t, "nulgone", "cc-gone")
			gone := e.okBody(t, e.write(http.MethodPost, "/api/v1/projects/nulgone/remove", map[string]any{
				"actor": actor, "reason": reason, "confirm": true,
			}), "project remove")
			if gone["removed"] != true {
				t.Fatalf("프로젝트를 안 지웠다: %v", gone)
			}
			p := e.rawCell("SELECT payload FROM event WHERE kind = 'project.removed'")
			noNUL(t, "project.removed payload", p)
			var doc map[string]any
			if err := json.Unmarshal([]byte(p), &doc); err != nil {
				t.Fatalf("project.removed payload: %v", err)
			}
			// 이 이벤트는 사유를 clip(제어 문자를 공백으로)해 싣는다 - NUL 이 칸에 닿지 않는 갈래다. 재는 것은 지우기가 두 엔진
			// 모두 끝까지 가고(500 이 아니다) 행위자의 `\x00` 네 글자가 그대로라는 것이다.
			if doc["actor"] != actor {
				t.Errorf("project.removed payload = %s, 행위자는 그대로 %q 여야 한다", p, actor)
			}
		})
	}
}

// 이름 자리(식별자 · 열거 · 항목 · 발자국 경로 · 선행 항목)의 NUL, JSON 키의 U+0000, 짝 없는 서로게이트, 깨진 UTF-8(본문 · URL),
// URL 의 NUL 은 두 엔진 모두 400 이고 아무것도 쓰지 않는다. 고치기 전에는 Postgres 가 URL 의 `%FF` 에 500(22021)을 냈다.
func TestWriteNULRefusesNamesOnBothEngines(t *testing.T) {
	for _, eng := range nulEngines {
		t.Run(string(eng), func(t *testing.T) {
			e := newEnvOn(t, eng, nil)
			sess := e.openSession("cc-nul")
			mustWrite(t, e.write(http.MethodPost, "/api/v1/items", map[string]any{
				"project": testProject, "session_id": sess, "id": "base", "title": "t", "body": "b",
			}), "add base")
			items, judgments, events := e.rawCount("item"), e.rawCount("judgment"), e.rawCount("event")
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
				{"본문 자유 글의 깨진 UTF-8", http.MethodPost, "/api/v1/judgments",
					`{"project":"cp","session_id":"` + sess + `","kind":"decision","body":"x` + "\xff" + `"}`, "bad_utf8"},
				{"본문 이름의 깨진 UTF-8", http.MethodPost, "/api/v1/items",
					`{"project":"cp","session_id":"` + sess + `","id":"i` + "\xc3" + `","title":"t","body":"b"}`, "bad_utf8"},
				{"URL 경로(쓰기)", http.MethodPost, "/api/v1/items/a%00b/amend",
					map[string]any{"project": testProject, "session_id": sess, "title": "t", "reason": "r"}, "nul_refused"},
				{"URL 경로(읽기)", http.MethodGet, "/api/v1/items/a%00b?project=" + testProject, nil, "nul_refused"},
				{"URL 질의", http.MethodGet, "/api/v1/judgments?q=%00", nil, "nul_refused"},
				{"URL 경로의 깨진 UTF-8(쓰기)", http.MethodPost, "/api/v1/items/a%FFb/amend",
					map[string]any{"project": testProject, "session_id": sess, "title": "t", "reason": "r"}, "bad_utf8"},
				{"URL 경로의 깨진 UTF-8(읽기)", http.MethodGet, "/api/v1/items/a%FFb?project=" + testProject, nil, "bad_utf8"},
				{"URL 질의의 깨진 UTF-8", http.MethodGet, "/api/v1/judgments?project=" + testProject + "&q=%FF", nil, "bad_utf8"},
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
			if n, m, v := e.rawCount("item"), e.rawCount("judgment"), e.rawCount("event"); n != items || m != judgments || v != events {
				t.Errorf("거절했는데 썼다: item %d→%d · judgment %d→%d · event %d→%d", items, n, judgments, m, events, v)
			}
		})
	}
}

// 꼬리표 rm 은 원장의 꼬리표와 **원장의 꼴**로 맞댄다. 고치기 전 SQLite 원장에 남은 실제 NUL 꼬리표(`"a\u0000"`)는 받은 rm 을
// 먼저 `\x00` 표기로 바꾼 탓에 못 지웠고, 같은 행에 있던 `\x00` 네 글자 꼬리표가 대신 지워졌다. 원장은 두 값을 가를 바이트가
// 없으므로(이관도 같은 값으로 옮긴다) 한 꼬리표로 보고 함께 지운다 - 어느 쪽으로 불러도, 두 엔진 모두.
func TestLabelRmMatchesTheLedgerFormOnBothEngines(t *testing.T) {
	legacy := `["a\u0000","a\\x00","keep"]` // 고치기 전 SQLite 서버가 담은 문서: NUL 꼬리표와 `\x00` 네 글자 꼬리표가 함께 있다
	for _, eng := range nulEngines {
		t.Run(string(eng), func(t *testing.T) {
			e := newEnvOn(t, eng, nil)
			sess := e.openSession("cc-label")
			seed := legacy
			if eng == freshstore.Postgres {
				seed = importCell(t, "item", "labels", legacy) // Postgres 원장에는 이관이 옮긴 값으로 있다
			}
			for _, rm := range []struct{ id, label string }{{"lr-nul", "a\x00"}, {"lr-lit", `a\x00`}} {
				mustWrite(t, e.write(http.MethodPost, "/api/v1/items", map[string]any{
					"project": testProject, "session_id": sess, "id": rm.id, "title": "t", "body": "b",
				}), "add "+rm.id)
				e.rawExec("UPDATE item SET labels = '" + seed + "' WHERE id = '" + rm.id + "'")
				w := mustWrite(t, e.write(http.MethodPost, "/api/v1/items/"+rm.id+"/label", map[string]any{
					"project": testProject, "session_id": sess, "rm": []string{rm.label},
				}), "label rm "+rm.id)
				if got := e.rawCell("SELECT labels FROM item WHERE id = '" + rm.id + "'"); got != `["keep"]` {
					t.Errorf("rm %q 뒤 꼬리표 = %s, 바라는 것 [\"keep\"]", rm.label, got)
				}
				if removed, _ := decodeBody(t, w)["removed"].([]any); len(removed) == 0 {
					t.Errorf("rm %q 의 응답이 지운 것이 없다고 한다: %s", rm.label, w.Body.String())
				}
			}
		})
	}
}

// 멱등 지문은 받은 원문의 지문이다: 같은 키 · 같은 원문은 재생(쓰기 한 번)이고, 원장에 담긴 표기(`\x00` 네 글자)로 같은 키를
// 다시 부르면 다른 요청이라 409 다. 지문이 바꾼 뒤의 값이면 둘이 같아져 남의 응답을 받는다.
func TestIdempotencyFingerprintsTheRawRequestOnBothEngines(t *testing.T) {
	for _, eng := range nulEngines {
		t.Run(string(eng), func(t *testing.T) {
			e := newEnvOn(t, eng, nil)
			sess := e.openSession("cc-idem")
			body := func(title string) map[string]any {
				return map[string]any{"project": testProject, "session_id": sess, "id": "idem1", "title": title, "body": "b"}
			}
			first := mustWrite(t, e.do(http.MethodPost, "/api/v1/items", body("제목\x00"), withKey("nul:1")), "첫 요청")
			again := mustWrite(t, e.do(http.MethodPost, "/api/v1/items", body("제목\x00"), withKey("nul:1")), "같은 요청")
			if again.Body.String() != first.Body.String() {
				t.Errorf("같은 키 · 같은 원문이 재생되지 않았다:\n%s\n%s", first.Body.String(), again.Body.String())
			}
			if n := e.rawCount("item"); n != 1 {
				t.Errorf("재생이 다시 썼다: item %d 행", n)
			}
			w := e.do(http.MethodPost, "/api/v1/items", body(`제목\x00`), withKey("nul:1"))
			if w.Code != http.StatusConflict || errorOf(t, w)["code"] != "idempotency_conflict" {
				t.Errorf("원장의 표기로 같은 키를 부르면 409 여야 한다: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// 원장 칸 관문이 바꿔 넣은 칸은 로그 한 줄로 남는다: 표 · 칸 · 규칙 · 행 좌표(식별자 칸의 값)를 싣고 글 내용은 안 싣는다.
func TestWriteNULLogsTheRewrittenCell(t *testing.T) {
	e := newEnv(t, nil)
	sess := e.openSession("cc-nul")
	mustWrite(t, e.write(http.MethodPost, "/api/v1/items", map[string]any{
		"project": testProject, "session_id": sess, "id": "n1", "title": "비밀\x00제목", "body": "b", "labels": []string{"ok", "l\x00"},
	}), "add")
	var hit []map[string]any
	for _, r := range e.logs.records(t) {
		if r["msg"] == "원장 칸의 NUL 을 \\x00 표기로 바꿔 넣었다" && r["table"] == "item" {
			hit = append(hit, r)
		}
	}
	cols := map[string]map[string]any{}
	for _, r := range hit {
		cols[r["column"].(string)] = r
	}
	if len(hit) != 2 || cols["title"] == nil || cols["labels"] == nil {
		t.Fatalf("item 칸 로그 줄 = %v, 바라는 것 title · labels 두 줄", hit)
	}
	for col, r := range cols {
		if row, _ := r["row"].(string); !strings.Contains(row, `id="n1"`) || !strings.Contains(row, `project="`+testProject+`"`) {
			t.Errorf("%s 로그 줄의 행 좌표 = %q, 항목 id 와 프로젝트가 있어야 한다", col, row)
		}
	}
	if !reflect.DeepEqual(cols["labels"]["json_pointers"], []any{"/1"}) || cols["title"]["rule"] == cols["labels"]["rule"] {
		t.Errorf("로그 줄 = %v", cols)
	}
	if strings.Contains(e.logs.String(), "비밀") {
		t.Errorf("로그에 글 내용이 실렸다")
	}
}

func TestJudgeURL(t *testing.T) {
	cases := []struct {
		raw, where string
		bad        error
	}{
		{"/api/v1/items/a?project=cp", "", nil},
		{"/api/v1/items/a%00b", "경로", nulesc.ErrNUL},
		{"/api/v1/items/a%FFb", "경로", nulesc.ErrUTF8},
		{"/api/v1/items/a?project=c%00p", `질의 "project"`, nulesc.ErrNUL},
		{"/api/v1/items/a?project=c%FFp", `질의 "project"`, nulesc.ErrUTF8},
		{"/api/v1/items/a?b=1&a=%00&c=%00", `질의 "a"`, nulesc.ErrNUL},
		{"/api/v1/items/a?x%00y=1", "질의 이름", nulesc.ErrNUL},
		{"/api/v1/items/a?x%C3=1", "질의 이름", nulesc.ErrUTF8},
		{"/api/v1/items/a?q=%5Cx00", "", nil},            // `\x00` 네 글자는 NUL 이 아니다
		{"/api/v1/items/%ED%95%9C?q=%EA%B0%80", "", nil}, // 올바른 UTF-8
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatal(err)
		}
		where, bad := JudgeURL(u)
		if where != c.where || (c.bad == nil) != (bad == nil) || (c.bad != nil && !errors.Is(bad, c.bad)) {
			t.Errorf("%s: (%q, %v), 바라는 것 (%q, %v)", c.raw, where, bad, c.where, c.bad)
		}
	}
}
