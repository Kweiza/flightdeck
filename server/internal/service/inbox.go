package service

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kweiza/flightdeck/internal/model"
	"github.com/kweiza/flightdeck/internal/store"
)

// 세션이 받는 곳(board/README 「세션에 알림」). 훅이 적고 보드가 읽는다.
//
// ★ 여기서 거르는 것은 **꼴**뿐이다. 받는 곳이 살아 있는지는 보드가 소켓에 붙어 본다.

// InboxModeRE 는 권한 모드 이름의 꼴이다(default · acceptEdits · bypassPermissions · plan …, 빈 값 = 모름).
// 보드는 이 값으로 봉투의 갈래만 고르고 글에 싣지 않지만, 아무 글이나 원장에 들이지 않는다.
var InboxModeRE = regexp.MustCompile(`^[A-Za-z]{0,40}$`)

// inboxSocketMax 는 소켓 경로의 길이 상한이다. 유닉스 소켓 경로는 OS 상한이 100 바이트 안팎이라
// 넉넉히 잡은 꼴 검사일 뿐이다(긴 값은 연결이 안 될 뿐이지만, 원장에 아무 길이나 들이지 않는다).
const inboxSocketMax = 512

// InboxListing 은 보드가 한 번에 읽는 것이다.
//   - Inbox: 받는 곳이 원장에 있는 세션.
//   - Legacy: 물은 옛 등록(cc · 띄운 폴더) 중 이 프로젝트 · 이 기계에서 그 폴더를 품은 닫히지 않은 카드가 있는 cc
//     (옮겨 가는 동안만, store.LiveSessionsOf).
type InboxListing struct {
	Inbox  []model.SessionInbox `json:"inbox"`
	Legacy []string             `json:"legacy"`
}

// legacyAskMax 는 한 번에 물을 수 있는 옛 등록 수다(보드 폴더의 파일 수, 대개 한 자리).
const legacyAskMax = 200

func inboxRefused(reason string) error {
	return &RefusedError{What: "session inbox", Reason: reason}
}

func checkInbox(socket, mode string) error {
	switch {
	case socket == "" || !filepath.IsAbs(socket) || len(socket) > inboxSocketMax:
		return inboxRefused("socket 은 절대 경로여야 한다(빈 값 · 상대 경로 · 지나친 길이 거절)")
	case strings.ContainsFunc(socket, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return inboxRefused("socket 에 제어 문자가 있다")
	case !InboxModeRE.MatchString(mode):
		return inboxRefused("mode 는 영문 낱말 하나여야 한다(권한 모드 이름)")
	}
	return nil
}

// PutInbox 는 세션의 받는 곳을 적는다. 같은 기계 · 같은 소켓의 다른 세션 행은 지워진다(store.PutInbox).
func (s *Service) PutInbox(ctx context.Context, sessionID, socket, mode string) (model.SessionInbox, error) {
	if err := checkInbox(socket, mode); err != nil {
		return model.SessionInbox{}, err
	}
	out, err := s.st.PutInbox(ctx, sessionID, socket, mode, s.now())
	if err != nil {
		s.logFail(ctx, "session.inbox", "", sessionID, err, failAbout{})
		return model.SessionInbox{}, err
	}
	return out, nil
}

// DropInbox 는 소켓이 같을 때만 받는 곳을 지운다(보드가 죽은 소켓을 봤을 때). 지운 행 수를 낸다.
func (s *Service) DropInbox(ctx context.Context, sessionID, socket string) (int64, error) {
	if socket == "" {
		return 0, inboxRefused("socket 이 비었다: 지울 받는 곳을 소켓으로 지목한다")
	}
	return s.st.DropInbox(ctx, sessionID, socket)
}

// Inbox 는 한 프로젝트 · 한 기계의 받는 곳과, 물은 옛 등록 중 그 프로젝트 · 기계의 것이다.
func (s *Service) Inbox(ctx context.Context, project, machineID string, legacy []store.LegacyAsk) (InboxListing, error) {
	if project == "" || machineID == "" {
		return InboxListing{}, inboxRefused("project 와 machine 이 둘 다 필요하다: 보드는 자기 기계의 세션에만 보낸다")
	}
	if len(legacy) > legacyAskMax {
		// 거절하면 원장 쪽 초인종까지 막힌다. 옛 길만 비우고 원장의 받는 곳은 답한다.
		legacy = nil
	}
	inbox, err := s.st.ListInbox(ctx, project, machineID)
	if err != nil {
		return InboxListing{}, err
	}
	live, err := s.st.LiveSessionsOf(ctx, project, machineID, legacy)
	if err != nil {
		return InboxListing{}, err
	}
	return InboxListing{Inbox: inbox, Legacy: live}, nil
}
