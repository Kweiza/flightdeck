package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kweiza/flightdeck/internal/auth"
)

// SSE — 서버가 미는 변화 알림.
//
// ★ 이것은 **최적화이지 정본이 아니다**(설계 §13: monitors 가 안 되면 pull 폴백만으로 간다).
// 그래서 구독자가 느리면 이벤트를 **버리고 그 사실을 세는** 쪽을 택한다.
// 발행을 막으면 조정 경로(REST 쓰기)가 구독자 하나 때문에 멈추고,
// 그러면 최적화가 정본을 죽인다.

// Event 는 SSE 로 나가는 변화 하나다.
//
// 본문을 통째로 싣지 않는다 — 알림은 "무엇이 바뀌었으니 다시 읽어라"이고,
// 값의 정본은 REST 다. 여기에 본문을 실으면 두 벌이 되고 두 벌은 표류한다.
type Event struct {
	Kind      string         `json:"kind"`
	Project   string         `json:"project,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	At        time.Time      `json:"at"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// EncodeSSE 는 이벤트 하나를 SSE 한 덩어리로 만든다. 순수 함수다.
//
// ★ data 는 **JSON 한 줄**이다. JSON 인코딩이 개행을 \n 으로 escape 하므로
// 본문에 개행이 있어도 프레임이 쪼개지지 않는다 — 이 성질이 없으면
// 사용자가 친 판단 본문 한 줄이 프로토콜을 깨뜨린다.
// id 와 event 이름은 이 계층이 만드는 값이라 개행이 들어올 수 없지만,
// 그래도 걷어낸다(주입 방지의 자리는 소비 계층이다).
func EncodeSSE(id string, ev Event) ([]byte, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("이벤트 직렬화 실패: %w", err)
	}
	var b strings.Builder
	if id != "" {
		fmt.Fprintf(&b, "id: %s\n", oneLine(id))
	}
	// ★ `event:` 줄을 찍지 않는다. kind 는 data 안에 이미 있다.
	//
	// SSE 규약상 `event:` 가 있으면 브라우저의 `onmessage` 는 **한 번도 발화하지 않는다**
	// (그 프레임의 type 이 `message` 가 아니게 되므로). 앞선 판은 여기서 이름을 찍고
	// 화면은 `onmessage` 만 걸어, 스트림은 정상으로 열리는데 **화면이 영원히 정지**했다.
	// 그리고 스트림이 열려 있으니 메타 리프레시 폴백도 안 켜졌고,
	// "(파생: … N초 전)" 배지가 렌더 시각으로 굳어 **낡은 값을 신선하다고 우겼다.**
	//
	// 두 시험이 각자 자기 절반(발행 쪽은 프레임 문자열, 구독 쪽은 HTML 문자열)을 고정해
	// 둘 다 초록인 채로 제품이 죽어 있었다. 이름을 안 찍으면 그 이음매 자체가 사라진다 —
	// 새 kind 를 서버가 더해도 화면이 자동으로 받으므로 조용한 표류도 안 생긴다.
	// 구독자가 종류로 거를 일이 생기면 data 의 kind 를 보면 된다.
	fmt.Fprintf(&b, "data: %s\n\n", payload)
	return []byte(b.String()), nil
}

// oneLine 은 SSE 필드 값에서 개행·캐리지리턴을 걷어낸다.
func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// Sub 는 구독자 하나다.
type Sub struct {
	ch      chan []byte
	project string // 빈 문자열이면 전 프로젝트
	owner   StreamOwner
	// cut 은 서버가 이 스트림을 끊었다는 신호다(Hub.Cut 이 닫는다). ch 를 닫지 않고 따로 두는 이유: 핸들러가 「종료 · 철회」를
	// 가려 작별 줄을 고르고, ch 의 닫힘은 Unsubscribe 한 곳의 몫으로 남는다.
	cut chan struct{}
}

// StreamOwner 는 스트림을 연 주체다 - 열린 스트림을 (계정, 기기 | 화면 세션)으로 묶은 목록의 열쇠다(설계: 철회는 다음
// 요청부터 듣는데 열린 스트림에는 다음 요청이 없다). 빈 칸은 어떤 끊기와도 안 맞는다 - FD_TOKEN · 무인증 스트림은 계정 쓰기로
// 끊을 것이 없다(FD_TOKEN 을 바꾸고 서버를 다시 띄우면 끊긴다).
type StreamOwner struct {
	Account, Machine, WebSession string
}

// OwnerOf 는 주체의 스트림 열쇠다.
func OwnerOf(p auth.Principal) StreamOwner {
	return StreamOwner{Account: p.AccountID, Machine: p.MachineID, WebSession: p.WebSession}
}

// CutAccount · CutMachine · CutWebSession 은 Hub.Cut 의 고르기다. 빈 id 는 아무것도 안 고른다.
func CutAccount(id string) func(StreamOwner) bool {
	return func(o StreamOwner) bool { return id != "" && o.Account == id }
}

func CutMachine(id string) func(StreamOwner) bool {
	return func(o StreamOwner) bool { return id != "" && o.Machine == id }
}

func CutWebSession(idHash string) func(StreamOwner) bool {
	return func(o StreamOwner) bool { return idHash != "" && o.WebSession == idHash }
}

// Hub 는 구독자 집합이다.
type Hub struct {
	mu      sync.Mutex
	subs    map[*Sub]struct{}
	buf     int
	dropped uint64
}

// NewHub 는 구독자당 버퍼 크기를 정해 허브를 만든다.
func NewHub(buf int) *Hub {
	if buf <= 0 {
		buf = 32
	}
	return &Hub{subs: map[*Sub]struct{}{}, buf: buf}
}

// Subscribe 는 구독자를 등록한다. project 가 비면 전부 받는다. owner 는 철회 · 끄기가 이 스트림을 고르는 열쇠다.
func (h *Hub) Subscribe(project string, owner StreamOwner) *Sub {
	s := &Sub{ch: make(chan []byte, h.buf), project: strings.TrimSpace(project), owner: owner, cut: make(chan struct{})}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Cut 은 고른 주체의 열린 스트림을 끊고 그 수를 돌려준다. 철회 · 끄기 · 로그아웃 트랜잭션이 **커밋된 뒤** 부른다 - 앞에서
// 끊으면 롤백된 철회에도 스트림이 끊기고, 다시 붙은 스트림은 아직 철회 전 원장을 보고 통과한다.
//
// ★ 구독 집합에서 지우고 cut 만 닫는다(잠금 안). 지운 구독은 발행을 더 안 받고, 뒤의 Unsubscribe 는 할 일이 없다 - ch 를
// 함께 닫으면 핸들러의 select 가 「ch 닫힘」과 「끊김」 중 아무것이나 골라 작별 줄이 갈린다.
func (h *Hub) Cut(match func(StreamOwner) bool) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for s := range h.subs {
		if !match(s.owner) {
			continue
		}
		delete(h.subs, s)
		close(s.cut)
		n++
	}
	return n
}

// Cut 은 이 구독이 서버에게 끊겼다는 신호다.
func (s *Sub) Cut() <-chan struct{} { return s.cut }

// Unsubscribe 는 구독을 끊고 채널을 닫는다.
//
// ★ 채널을 닫는 것이 잠금 안에서 일어나야 한다 — 발행은 같은 잠금 아래에서만
// 집합의 채널에 쓰므로, 여기서 지운 뒤에 닫으면 "닫힌 채널에 쓰기" 가 원리적으로 불가능하다.
// 두 번 불러도 안전하다(defer 와 명시 호출이 겹칠 수 있다).
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	close(s.ch)
}

// Count 는 지금 붙어 있는 구독자 수다. /metrics 가 이 값을 그대로 낸다.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Dropped 는 버퍼가 차서 버린 이벤트 수다.
//
// **버린 것을 세지 않으면 "조용한 이벤트"가 된다** — 화면이 안 바뀌는데
// 아무도 그 이유를 모르는 상태가 정확히 그 모양이다.
func (h *Hub) Dropped() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

// Publish 는 이벤트를 구독자들에게 민다. 한 번만 직렬화한다.
//
// 직렬화가 실패하면 오류를 돌려준다 — 삼키면 "이벤트가 안 온다"의 원인이 사라진다.
func (h *Hub) Publish(id string, ev Event) error {
	frame, err := EncodeSSE(id, ev)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.project != "" && ev.Project != "" && s.project != ev.Project {
			continue
		}
		select {
		case s.ch <- frame:
		default:
			h.dropped++ // 느린 구독자가 발행을 막지 못하게 한다
		}
	}
	return nil
}
