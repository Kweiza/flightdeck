package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// job: 러너가 돌린 잡(랜딩 · 실행)의 결과 표. 지금(Tier A)은 러너가 없어 쓰는 자리가 없고 읽기만 한다.

// UnackedJob 은 아직 아무도 확인하지 않은 잡 결과 하나다.
type UnackedJob struct {
	ID       string
	Kind     string
	ItemID   string
	State    string
	FailKind string
	EndedAt  time.Time // 끝난 시각이 없으면 영값
	LogTail  string
}

// UnackedJobs 는 프로젝트의 미확인 결과(끝난 잡 중 ack_at 이 빈 것)를 **줄 선 시각의 최신순**으로 limit 개까지 낸다.
// limit 이 음수면 전부다(withLimit).
//
// ★ 표가 비어 있어도 질의는 한다(보드의 미확인 결과 패널) - 안 보면 "결과가 없다"와 "그 축을 안 본다"가 구분되지 않는다.
// ★ 같은 시각에 줄 선 잡은 id 로 끝을 맺는다. 적지 않으면 같은 시각 행의 순서를 엔진이 고르고, limit 이 자르는 행도
// 엔진마다 달라진다.
func (s *Store) UnackedJobs(ctx context.Context, project string, limit int) ([]UnackedJob, error) {
	q, args := withLimit(`
		SELECT id, kind, item_id, state, fail_kind, ended_at, log_tail
		FROM job WHERE project = ? AND ack_at IS NULL AND state IN ('ok','fail','stalled','bypassed')
		ORDER BY queued_at DESC, id DESC`, []any{project}, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("미확인 잡 조회 실패(project=%q): %w", clip(project, 64), err)
	}
	defer rows.Close()

	var out []UnackedJob
	for rows.Next() {
		var j UnackedJob
		var item, failKind, ended, tail sql.NullString
		if err := rows.Scan(&j.ID, &j.Kind, &item, &j.State, &failKind, &ended, &tail); err != nil {
			return nil, fmt.Errorf("미확인 잡 행 해석 실패(project=%q): %w", clip(project, 64), err)
		}
		j.ItemID, j.FailKind, j.LogTail = str(item), str(failKind), str(tail)
		if j.EndedAt, err = parseTime(str(ended)); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("미확인 잡 목록 순회 실패(project=%q): %w", clip(project, 64), err)
	}
	return out, nil
}
