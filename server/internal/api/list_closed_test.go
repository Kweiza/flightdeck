package api

import (
	"net/http"
	"testing"
)

// TestListClosedItemsNewestFirst 는 닫힌 항목 목록이 열린 항목을 안 섞고, 최근에 닫힌 것을
// 먼저 내며, state 로 done·dropped 를 가르는지 본다. 모르는 state 는 거절이어야 한다 —
// 오타를 기본값으로 접으면 "끝난 것이 없다"와 구분이 안 된다.
func TestListClosedItemsNewestFirst(t *testing.T) {
	e := newEnv(t, nil)
	sess := e.openSession("cc-1")
	for _, id := range []string{"lc-open", "lc-done", "lc-drop"} {
		e.addItemIn(t, testProject, sess, id, nil)
	}
	finish := func(id, outcome string) {
		t.Helper()
		e.okBody(t, e.write(http.MethodPost, "/api/v1/items/"+id+"/claim", map[string]any{
			"project": testProject, "session_id": sess,
		}), "선점("+id+")")
		req := map[string]any{
			"project": testProject, "session_id": sess, "outcome": outcome,
			"title": id + " 마무리", "body": "닫는다. 기각한 것 없음, 안 한 것 없음",
		}
		if outcome == "dropped" {
			req["close_reason"] = "시험으로 버린다"
		}
		e.okBody(t, e.write(http.MethodPost, "/api/v1/items/"+id+"/finish", req), "마무리("+id+")")
	}
	finish("lc-done", "done")
	finish("lc-drop", "dropped")

	ids := func(body map[string]any) []string {
		var out []string
		list, _ := body["items"].([]any)
		for _, x := range list {
			m, _ := x.(map[string]any)
			out = append(out, m["ID"].(string))
		}
		return out
	}

	all := e.okBody(t, e.do(http.MethodGet, "/api/v1/items?project="+testProject, nil), "닫힌 목록")
	if got := ids(all); len(got) != 2 || got[0] != "lc-drop" || got[1] != "lc-done" {
		t.Fatalf("닫힌 목록이 %v 다 — 열린 lc-open 없이 최근에 닫힌 lc-drop 이 먼저여야 한다", got)
	}
	if all["state"] != "closed" {
		t.Errorf("state 가 %v 다 — 기본은 closed 여야 한다", all["state"])
	}

	done := e.okBody(t, e.do(http.MethodGet, "/api/v1/items?project="+testProject+"&state=done", nil), "done 목록")
	if got := ids(done); len(got) != 1 || got[0] != "lc-done" {
		t.Fatalf("done 목록이 %v 다", got)
	}

	one := e.okBody(t, e.do(http.MethodGet, "/api/v1/items?project="+testProject+"&limit=1", nil), "limit 1")
	if got := ids(one); len(got) != 1 || got[0] != "lc-drop" {
		t.Fatalf("limit=1 목록이 %v 다", got)
	}

	if w := e.do(http.MethodGet, "/api/v1/items?project="+testProject+"&state=open", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("state=open 이 %d 다 — 열린 목록은 dashboard.json 몫이라 거절이어야 한다", w.Code)
	}
	if w := e.do(http.MethodGet, "/api/v1/items", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("project 없는 요청이 %d 다", w.Code)
	}
}
