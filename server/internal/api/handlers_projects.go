package api

import "net/http"

// handleListProjects 는 프로젝트 요약 전부를 낸다.
//
// ★ 읽기 전용이라 화면 쓰기 사슬을 안 탄다. 이 경로가 내는 것은 등록된 프로젝트와 그 실적뿐이고,
// 그것을 읽을 자격은 이미 게이트가 판정했다.
func (s *server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.ListProjectSummaries(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"projects": list})
}

// handleRemoveProject 는 프로젝트를 지운다. **되돌릴 수 없다.** 사유가 필수고, confirm 이
// 없으면 세기만 한다 — 그 두 안전판은 service.RemoveProject 가 지킨다(여기서 다시 판정하지
// 않는다. 두 벌이 되면 반드시 표류한다).
//
// ★ 관리자만 한다. 프로젝트는 모든 계정이 함께 보는 원장 행이라 한 회원의 기기 토큰이 남의 판단 · 항목째 지우게 두지 않는다.
// 행위자는 본문이 아니라 인증 주체에서 고른다(ActorOf) - 본문 칸은 계정이 없는 옛 주체의 자기 신고로만 남는다.
func (s *server) handleRemoveProject(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin() {
		s.writeError(w, r, forbidden("admin_only", "프로젝트 지우기는 관리자만 한다", "관리자 계정으로 다시 해라."))
		return
	}
	var req struct {
		Actor   string `json:"actor" nul:"text"`
		Reason  string `json:"reason" nul:"text"`
		Confirm bool   `json:"confirm"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	res, err := s.svc.RemoveProject(r.Context(), r.PathValue("id"), ActorOf(p, req.Actor), req.Reason, req.Confirm)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, res)
}
