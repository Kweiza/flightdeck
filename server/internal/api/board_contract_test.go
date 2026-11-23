package api

import (
	"net/http"
	"testing"
)

// TestBoardReadsTheseFields 는 칸반 보드(board/board.py)가 읽는 응답 꼴을 잠근다.
//
// 보드는 이 저장소의 REST 소비자인데 파이썬이라 Go 컴파일러가 안 본다. 실제로 두 번 깨졌다:
// 세션 view 의 Claims 가 {ItemID} 객체에서 id 문자열로 바뀌자 보드가 AttributeError 로 502 를 냈고,
// 선점된 항목이 dashboard.json 의 open_items 에서 빠지자 진행 중 열이 비었다. 여기 적힌 키를
// 바꾸거나 지우면 이 시험이 빨개진다. 그때 board.py 의 같은 이름을 함께 고쳐라.
func TestBoardReadsTheseFields(t *testing.T) {
	e := newEnv(t, nil)
	sess := e.openSession("cc-board")
	e.addItemIn(t, testProject, sess, "bd-open", map[string]any{"labels": []string{"status:owner"}})
	e.addItemIn(t, testProject, sess, "bd-claimed", nil)
	e.addItemIn(t, testProject, sess, "bd-closed", nil)

	has := func(what string, m map[string]any, keys ...string) {
		t.Helper()
		for _, k := range keys {
			if _, ok := m[k]; !ok {
				t.Fatalf("%s 에 보드가 읽는 키 %q 가 없다: %v", what, k, m)
			}
		}
	}
	obj := func(what string, v any) map[string]any {
		t.Helper()
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s 가 객체가 아니다: %#v", what, v)
		}
		return m
	}
	list := func(what string, v any) []any {
		t.Helper()
		l, ok := v.([]any)
		if !ok {
			t.Fatalf("%s 가 목록이 아니다: %#v", what, v)
		}
		return l
	}

	// 선점: 보드의 claimed_ids 는 sessions[].view.Claims 의 원소가 id 문자열이거나 {ItemID, ReleasedAt} 이길 바란다.
	e.okBody(t, e.write(http.MethodPost, "/api/v1/items/bd-claimed/claim", map[string]any{
		"project": testProject, "session_id": sess,
	}), "선점")
	finishOne := func(id string) {
		t.Helper()
		e.okBody(t, e.write(http.MethodPost, "/api/v1/items/"+id+"/claim", map[string]any{
			"project": testProject, "session_id": sess,
		}), "선점("+id+")")
		e.okBody(t, e.write(http.MethodPost, "/api/v1/items/"+id+"/finish", map[string]any{
			"project": testProject, "session_id": sess, "outcome": "done", "title": "닫음",
			"body": "닫는다. 기각한 것 없음, 안 한 것 없음",
		}), "마무리("+id+")")
	}
	finishOne("bd-closed")

	dash := e.okBody(t, e.do(http.MethodGet, "/api/v1/dashboard.json?project="+testProject, nil), "dashboard.json")
	has("dashboard.json", dash, "at", "open_items", "sessions")
	var openIDs []string
	for _, x := range list("open_items", dash["open_items"]) {
		it := obj("open_items[]", x)
		has("open_items[]", it, "ID", "Title", "Body", "Labels", "State", "CreatedAt")
		openIDs = append(openIDs, it["ID"].(string))
	}
	if len(openIDs) == 0 {
		t.Fatalf("open_items 가 비었다: bd-open 이 있어야 한다")
	}
	claimed := map[string]bool{}
	for _, x := range list("sessions", dash["sessions"]) {
		view := obj("sessions[].view", obj("sessions[]", x)["view"])
		cl, _ := view["Claims"].([]any)
		for _, c := range cl {
			switch v := c.(type) {
			case string:
				claimed[v] = true
			case map[string]any:
				if id, _ := v["ItemID"].(string); id != "" && v["ReleasedAt"] == nil {
					claimed[id] = true
				}
			default:
				t.Fatalf("Claims 원소가 문자열도 {ItemID} 도 아니다: %#v, board.py claimed_ids 를 함께 고쳐라", c)
			}
		}
	}
	if !claimed["bd-claimed"] {
		t.Fatalf("sessions[].view.Claims 에서 bd-claimed 선점을 못 읽었다: %v", dash["sessions"])
	}

	// 소유자 동작: 세션 없는 판단 노트([fdboard]) · 꼬리표 바꾸기.
	w := e.write(http.MethodPost, "/api/v1/judgments", map[string]any{
		"project": testProject, "kind": "decision", "item_id": "bd-open",
		"title": "[fdboard] 소유자 선택: A", "body": "소유자 선택: A",
	})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("보드의 세션 없는 판단 노트가 %d 다: %s", w.Code, w.Body.String())
	}
	e.okBody(t, e.write(http.MethodPost, "/api/v1/items/bd-open/label", map[string]any{
		"project": testProject, "add": []string{"status:owner-done"}, "rm": []string{"status:owner"},
	}), "꼬리표")

	detail := e.okBody(t, e.do(http.MethodGet, "/api/v1/items/bd-open?project="+testProject, nil), "items/{id}")
	item := obj("item", detail["item"])
	has("item", item, "ID", "Title", "Body", "Labels", "State", "CreatedAt")
	if labels := list("item.Labels", item["Labels"]); len(labels) != 1 || labels[0] != "status:owner-done" {
		t.Fatalf("꼬리표가 %v 다: status:owner-done 하나여야 한다", labels)
	}
	js := list("judgments", detail["judgments"])
	if len(js) == 0 {
		t.Fatal("items/{id} 의 judgments 에 보드 노트가 없다")
	}
	j := obj("judgments[]", js[0])
	has("judgments[]", j, "Title", "Body", "At", "Links")
	link := obj("judgments[].Links[]", list("Links", j["Links"])[0])
	has("judgments[].Links[]", link, "TargetKind", "TargetID")

	found := e.okBody(t, e.do(http.MethodGet, "/api/v1/judgments?q=fdboard&limit=200&project="+testProject, nil), "judgments?q")
	if len(list("judgments?q", found["judgments"])) == 0 {
		t.Fatal("judgments?q=fdboard 가 보드 노트를 못 찾았다(/api/inbox 가 이 낱말로 찾는다)")
	}

	closed := e.okBody(t, e.do(http.MethodGet, "/api/v1/items?project="+testProject+"&state=closed&limit=30", nil), "닫힌 목록")
	cl := list("items", closed["items"])
	if len(cl) != 1 {
		t.Fatalf("닫힌 목록이 %d건이다: bd-closed 하나여야 한다", len(cl))
	}
	has("닫힌 items[]", obj("items[]", cl[0]), "ID", "Title", "Body", "Labels", "State", "ClosedAt")

	projects := e.okBody(t, e.do(http.MethodGet, "/api/v1/projects", nil), "projects")
	ps := list("projects", projects["projects"])
	if len(ps) == 0 {
		t.Fatal("projects 가 비었다")
	}
	has("projects[]", obj("projects[]", ps[0]), "id", "path", "items")
}
