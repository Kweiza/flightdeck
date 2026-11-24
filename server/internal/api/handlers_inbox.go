package api

import (
	"net/http"
	"strings"
)

// 세션이 받는 곳(board/README 「세션에 알림」). 훅이 PUT 으로 적고, 보드가 GET 으로 읽고 죽은 소켓을 DELETE 한다.
//
// ★ 이벤트(publish)를 내지 않는다. 훅이 프롬프트마다 적는 배달 주소라 SSE · 이벤트 표에 실으면 그대로 소음이다.
// ★ 응답 · 로그에 토큰이 없다: 토큰은 원장에 칸이 없고 요청에도 안 실린다(migrations/018).

type inboxRequest struct {
	Socket string `json:"socket"`
	Mode   string `json:"mode"`
}

// handlePutInbox 는 세션의 받는 곳을 적는다(있으면 고친다).
func (s *server) handlePutInbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	infoFrom(r.Context()).setSession(id)
	var req inboxRequest
	if !s.decode(w, r, &req) {
		return
	}
	out, err := s.svc.PutInbox(r.Context(), id, req.Socket, req.Mode)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"inbox": out})
}

// handleDropInbox 는 소켓이 같을 때만 받는 곳을 지운다(보드가 연결이 안 되는 소켓을 봤을 때).
func (s *server) handleDropInbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	infoFrom(r.Context()).setSession(id)
	var req inboxRequest
	if !s.decode(w, r, &req) {
		return
	}
	n, err := s.svc.DropInbox(r.Context(), id, req.Socket)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"dropped": n})
}

// handleInbox 는 한 프로젝트 · 한 기계의 받는 곳을 읽는다. legacy 는 쉼표로 이은 옛 등록 cc(옮겨 가는 동안만).
func (s *server) handleInbox(w http.ResponseWriter, r *http.Request) {
	project, ok := s.requireQuery(w, r, "project", "어느 프로젝트의 세션인지 적어라.")
	if !ok {
		return
	}
	machine, ok := s.requireQuery(w, r, "machine", "보드는 자기 기계의 세션에만 보낸다: 이 기계의 fd machine-id 를 적어라.")
	if !ok {
		return
	}
	var legacy []string
	for _, c := range strings.Split(r.URL.Query().Get("legacy"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			legacy = append(legacy, c)
		}
	}
	out, err := s.svc.Inbox(r.Context(), project, machine, legacy)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, out)
}
