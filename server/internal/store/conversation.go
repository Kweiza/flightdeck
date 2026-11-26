package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
)

// 대화 단위 라이프사이클 관측.
//
// ★ 대화를 접는 규율은 prescribe_reach.go 의 AckReach 를 그대로 따른다 — (machine_id,
// cc_session_id) 가 대화 키이고, cc 가 비면 자기 카드 하나로 접는다(빈 값끼리는 같은
// 대화로 안 본다. 그 근거도 AckReach 와 같다: OpenSession 이 3중키 전부를 요구해 세션
// 표에 빈 cc 행이 여럿 있을 수 있고, 그 행들을 서로 묶으면 남의 카드가 형제로 잡힌다).
// 다만 저기는 **전 세션**을 한 질의로 접어 세지만, 여기는 **대화 하나**만 보므로 형제
// 목록 질의로 편다 — 목적이 다르다(저쪽은 도달성 분모, 이쪽은 지금 이 대화의 상태).

// ConvLifecycle 은 대화 하나(machine_id, cc_session_id)가 지금 어디에 있는지의 관측이다.
// 판정(judgeLifecycleGate)은 이 값을 순수하게 해석할 뿐 여기서 하지 않는다.
type ConvLifecycle struct {
	SessionIDs   []string          // 이 대화를 이루는 카드(세션) id 전부
	EarliestOpen time.Time         // 대화 카드들의 가장 이른 opened_at — 관측 구간의 하한
	LiveClaims   []string          // 살아 있는 선점 중 항목이 아직 done/dropped 가 아닌 것의 item id
	LaneRow      *model.LandingRow // 대화의 살아 있는 줄 행(자원 포함). 없으면 nil
	HeldRes      []string          // 대화가 쥔 자원 이름들
	DoneItems    []string          // EarliestOpen 이후 이 대화가 done 으로 닫은 항목(롤백 제외)
	EverEnqueued bool              // EarliestOpen 이후 줄에 선 적(닫힌 행 포함)이 있나
}

// doneFinishItem 은 item.finish 원장 payload 가 「이 항목을 done 으로 닫았다」인지 읽고, 그렇다면 그 항목 id 다.
//
// ★ 키 이름은 finish.go 가 실제로 쓰는 것(item·mode)과 markTxOutcome 이 얹는 결말 표시(tx) 그대로다.
// ★ 판독은 원장 payload 판독기 한 벌(payloadTopString · PayloadRolledBack)로 한다 - 표준 JSON 만 받고, 중복 키는 첫 값,
// 키는 글자 그대로 견준다. 구조체 풀기로 따로 읽으면 중복 키의 끝 값을 쓰고 키의 대소문자를 안 가려, 같은 행을
// 다른 판정(CloseDeclarationsByItem · 후속 자격)과 다르게 읽는다. 결말 칸의 타입이 틀린 행도 행째 버리지 않는다 -
// 롤백이 확인된 행만 뺀다(QueueReproduction 의 「결말 표시가 없는 행은 센다」와 같은 규율).
// ★ payload 를 못 읽은 행(깨진 JSON)은 안 센다 - 항목 id 를 모른다.
func doneFinishItem(payload string) (string, bool) {
	item, ok, err := payloadTopString(payload, "item")
	if err != nil || !ok || item == "" {
		return "", false
	}
	if mode, ok, _ := payloadTopString(payload, "mode"); !ok || mode != string(model.ItemDone) {
		return "", false
	}
	if PayloadRolledBack(payload) {
		return "", false
	}
	return item, true
}

// ConversationLifecycle 은 세션 하나가 속한 대화 전체의 라이프사이클 관측을 모은다.
//
// ★ **관측만 한다 — 판정은 안 한다.** "지금 lane-wait 인가 finish 인가"는
// service.judgeLifecycleGate(순수 함수)의 몫이다. 여기서 판정까지 하면 표 시험으로
// 잠글 수 있는 로직이 DB 왕복과 뒤섞여, 판정 순서를 고치는 변경마다 SQLite 픽스처가 든다.
func (s *Store) ConversationLifecycle(ctx context.Context, project, sessionID string) (ConvLifecycle, error) {
	var out ConvLifecycle
	if project == "" || sessionID == "" {
		return out, fmt.Errorf("대화 라이프사이클 좌표가 비었다(project=%q session=%q)",
			clip(project, 64), clip(sessionID, 64))
	}

	// ① 형제 카드: 같은 (machine_id, cc_session_id). cc 가 비면 자기 하나다(위 파일 주석).
	//
	// ★ 순서를 적는다. UNION 의 결과 순서는 엔진이 고른다(SQLite 는 중복 제거를 정렬로 해서 id 순, Postgres 는 해시로
	//   하면 아무 순서) - 적지 않으면 SessionIDs 가 엔진마다 다른 순서로 나간다.
	sibRows, err := s.db.QueryContext(ctx, `
		SELECT s2.id, s2.opened_at FROM session s1
		JOIN session s2 ON s2.machine_id = s1.machine_id
		 AND s2.cc_session_id = s1.cc_session_id AND s1.cc_session_id <> ''
		WHERE s1.id = ?
		UNION
		SELECT id, opened_at FROM session WHERE id = ?
		ORDER BY id`, sessionID, sessionID)
	if err != nil {
		return out, fmt.Errorf("대화 형제 카드 조회 실패(project=%q session=%q): %w",
			clip(project, 64), clip(sessionID, 64), err)
	}
	for sibRows.Next() {
		var id, opened string
		if err := sibRows.Scan(&id, &opened); err != nil {
			sibRows.Close()
			return out, fmt.Errorf("대화 형제 카드 행 해석 실패: %w", err)
		}
		t, err := parseTime(opened)
		if err != nil {
			sibRows.Close()
			return out, err
		}
		out.SessionIDs = append(out.SessionIDs, id)
		if out.EarliestOpen.IsZero() || t.Before(out.EarliestOpen) {
			out.EarliestOpen = t
		}
	}
	if err := sibRows.Err(); err != nil {
		sibRows.Close()
		return out, fmt.Errorf("대화 형제 카드 순회 실패: %w", err)
	}
	sibRows.Close()
	if len(out.SessionIDs) == 0 {
		// 세션 자신도 안 잡혔다 — 존재하지 않는 세션 id. UNION 의 두 번째 가지가 그 카드
		// 자신을 무조건 내므로, 이 자리에 오는 유일한 길은 sessionID 가 session 표에 없는 것이다.
		return out, notFound(NFSession, project, sessionID)
	}

	// 이하 전부 SessionIDs 의 IN (…) 자리표를 만들어 돈다(attachResources 의 marks 방식).
	marks := make([]string, len(out.SessionIDs))
	sidArgs := make([]any, len(out.SessionIDs))
	for i, id := range out.SessionIDs {
		marks[i], sidArgs[i] = "?", id
	}
	inClause := strings.Join(marks, ",")
	sinceStr := fmtTime(out.EarliestOpen)

	// ② 살아 있는 선점 + 항목이 여전히 열려 있는 것.
	claimRows, err := s.db.QueryContext(ctx, `
		SELECT c.item_id FROM claim c
		JOIN item i ON i.project = c.project AND i.id = c.item_id
		WHERE c.project = ? AND c.released_at IS NULL AND c.session_id IN (`+inClause+`)
		  AND i.state NOT IN ('done','dropped')
		ORDER BY c.item_id`, append([]any{project}, sidArgs...)...)
	if err != nil {
		return out, fmt.Errorf("대화 살아 있는 선점 조회 실패(project=%q session=%q): %w",
			clip(project, 64), clip(sessionID, 64), err)
	}
	for claimRows.Next() {
		var id string
		if err := claimRows.Scan(&id); err != nil {
			claimRows.Close()
			return out, fmt.Errorf("대화 살아 있는 선점 행 해석 실패: %w", err)
		}
		out.LiveClaims = append(out.LiveClaims, id)
	}
	if err := claimRows.Err(); err != nil {
		claimRows.Close()
		return out, fmt.Errorf("대화 살아 있는 선점 순회 실패: %w", err)
	}
	claimRows.Close()

	// ③ 줄 행: 대화의 살아 있는 랜딩 줄 행 하나(있다면). 취득이 all-or-nothing 이라
	// 한 세션은 살아 있는 행을 하나만 갖지만(landing_queue_one_live_per_session), 대화
	// 전체로는 형제 카드가 각자 다른 순간에 섰다 닫혔다 할 수 있으므로 여기서는 LIMIT 1 로 뽑는다.
	laneRow := s.db.QueryRowContext(ctx, `
		SELECT `+landingCols+` FROM landing_queue
		WHERE project = ? AND left_at IS NULL AND session_id IN (`+inClause+`)
		ORDER BY id LIMIT 1`, append([]any{project}, sidArgs...)...)
	r, lerr := scanLandingRow(laneRow)
	switch {
	case errors.Is(lerr, sql.ErrNoRows):
		// 줄에 안 섰다 — LaneRow 는 nil 그대로.
	case lerr != nil:
		return out, fmt.Errorf("대화 살아 있는 줄 행 조회 실패(project=%q session=%q): %w",
			clip(project, 64), clip(sessionID, 64), lerr)
	default:
		wrapped := []model.LandingRow{r}
		if err := attachResources(ctx, s.db, wrapped); err != nil {
			return out, err
		}
		out.LaneRow = &wrapped[0]
	}

	// EverEnqueued: EarliestOpen 이후 줄에 선 적(닫힌 행 포함)이 있나.
	//
	// ★ bool 로 읽는다. EXISTS 는 SQLite 에서 정수 0 · 1 이고 Postgres 에서 boolean 이다. database/sql 은 정수 0 · 1 을
	//   bool 로 옮기지만 boolean 을 정수로는 못 옮긴다(Postgres 에서 Scan 오류).
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM landing_queue
		  WHERE project = ? AND session_id IN (`+inClause+`) AND enqueued_at >= ?)`,
		append(append([]any{project}, sidArgs...), sinceStr)...).Scan(&out.EverEnqueued); err != nil {
		return out, fmt.Errorf("대화 줄 이력 조회 실패(project=%q session=%q): %w",
			clip(project, 64), clip(sessionID, 64), err)
	}

	// ④ 쥔 자원.
	heldRows, err := s.db.QueryContext(ctx, `
		SELECT resource FROM resource_hold
		WHERE project = ? AND released_at IS NULL AND session_id IN (`+inClause+`)
		ORDER BY resource`, append([]any{project}, sidArgs...)...)
	if err != nil {
		return out, fmt.Errorf("대화 쥔 자원 조회 실패(project=%q session=%q): %w",
			clip(project, 64), clip(sessionID, 64), err)
	}
	for heldRows.Next() {
		var r string
		if err := heldRows.Scan(&r); err != nil {
			heldRows.Close()
			return out, fmt.Errorf("대화 쥔 자원 행 해석 실패: %w", err)
		}
		out.HeldRes = append(out.HeldRes, r)
	}
	if err := heldRows.Err(); err != nil {
		heldRows.Close()
		return out, fmt.Errorf("대화 쥔 자원 순회 실패: %w", err)
	}
	heldRows.Close()

	// ⑤ done 항목: EarliestOpen 이후 이 대화가 done 으로 닫은 항목(롤백 제외). 판독은 doneFinishItem 이다.
	evRows, err := s.db.QueryContext(ctx, `
		SELECT payload FROM event
		WHERE project = ? AND kind = 'item.finish' AND session_id IN (`+inClause+`) AND at >= ?
		ORDER BY id`, append(append([]any{project}, sidArgs...), sinceStr)...)
	if err != nil {
		return out, fmt.Errorf("대화 종료 선언 조회 실패(project=%q session=%q): %w",
			clip(project, 64), clip(sessionID, 64), err)
	}
	seen := map[string]bool{}
	for evRows.Next() {
		var payload string
		if err := evRows.Scan(&payload); err != nil {
			evRows.Close()
			return out, fmt.Errorf("대화 종료 선언 행 해석 실패: %w", err)
		}
		item, ok := doneFinishItem(payload)
		if ok && !seen[item] {
			seen[item] = true
			out.DoneItems = append(out.DoneItems, item)
		}
	}
	if err := evRows.Err(); err != nil {
		evRows.Close()
		return out, fmt.Errorf("대화 종료 선언 순회 실패: %w", err)
	}
	evRows.Close()

	return out, nil
}
