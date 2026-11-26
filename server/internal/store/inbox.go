package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
)

// session_inbox: 세션이 받는 곳(Claude Code cross-session messaging 의 소켓)과 권한 모드.
//
// 보드가 소유자 동작을 세션에 초인종 한 줄로 알릴 때 이 표를 읽는다(board/README 「세션에 알림」).
// 세션 · 프로젝트 · 기계는 session 표가 정본이다: 이 표는 배달 주소만 갖고, 어느 프로젝트의 어느
// 기계 세션인지는 늘 session 과 이어 읽는다(둘째 판정을 두지 않는다).
//
// ★ 판정 축이 아니다. 「살아 있나」는 보드가 소켓에 붙어 보고 안다: 여기서 나이로 거르지 않는다
//   (signal 표 머리말의 규율과 같다: 나이를 불리언으로 만들지 않는다).

// PutInbox 는 세션의 받는 곳을 적는다(있으면 고친다). mode 가 비면 있던 모드를 둔다(모르는 것으로 아는 것을 지우지 않는다).
//
// 한 기계에서 소켓 하나는 한 살아 있는 세션의 것이다. 같은 기계 · 같은 소켓의 **다른 세션** 행은
// 낡았다(소켓 경로는 프로세스 id 로 지어져 다시 쓰이고, rekey 가 실패한 /clear 는 새 카드를 연다).
// 그래서 같은 트랜잭션에서 지운다: 남겨 두면 보드가 한 소켓에 두 세션 id 로 두 번 보낸다.
func (t *Tx) PutInbox(sessionID, socket, mode string, at time.Time) (model.SessionInbox, error) {
	sess, err := t.GetSession(sessionID)
	if err != nil {
		return model.SessionInbox{}, err
	}
	if strings.TrimSpace(socket) == "" {
		return model.SessionInbox{}, fmt.Errorf("세션 받는 곳의 소켓이 비었다(session_id=%q)", clip(sessionID, 64))
	}
	if _, err := t.tx.ExecContext(t.ctx,
		`DELETE FROM session_inbox WHERE machine_id = ? AND socket = ? AND session_id <> ?`,
		sess.MachineID, socket, sess.ID); err != nil {
		return model.SessionInbox{}, fmt.Errorf("같은 소켓의 옛 받는 곳 지우기 실패(session_id=%q): %w", clip(sess.ID, 64), err)
	}
	seen := atStamp(at)
	if _, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO session_inbox(session_id, machine_id, socket, mode, seen_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET machine_id = excluded.machine_id, socket = excluded.socket,
		  mode = COALESCE(NULLIF(excluded.mode, ''), session_inbox.mode), seen_at = excluded.seen_at`,
		sess.ID, sess.MachineID, socket, mode, fmtTime(seen)); err != nil {
		return model.SessionInbox{}, writeErr(err, writeTarget{
			Target: TargetSession, Project: sess.Project, ID: sess.ID,
			RefHint: fmt.Sprintf("프로젝트 %s · 머신 %s", clip(sess.Project, 64), clip(sess.MachineID, 64)),
		}, "세션 받는 곳 적기 실패(session_id=%q)", clip(sess.ID, 64))
	}
	return model.SessionInbox{SessionID: sess.ID, CCSessionID: sess.CCSessionID, Socket: socket, Mode: mode, SeenAt: seen}, nil
}

// PutInbox 는 단발 트랜잭션이다.
func (s *Store) PutInbox(ctx context.Context, sessionID, socket, mode string, at time.Time) (model.SessionInbox, error) {
	var out model.SessionInbox
	err := s.Tx(ctx, func(t *Tx) error {
		var e error
		out, e = t.PutInbox(sessionID, socket, mode, at)
		return e
	})
	return out, err
}

// DropInbox 는 세션의 받는 곳을 지운다. **소켓이 같을 때만** 지운다: 보드가 죽은 소켓을 본 사이에
// 훅이 새 소켓을 적었으면 그 새 값은 산다. 지운 행 수(0 또는 1)를 낸다.
//
// ★ 단발 트랜잭션(전역 잠금)으로 지운다. 트랜잭션 밖에서 곧장 지우면, 같은 세션의 받는 곳을 적는 쓰기가
// 전역 잠금을 쥐고 아직 그 행을 안 만진 사이에 삭제가 먼저 커밋된다 - 쓰기를 한 줄로 세운 규칙의 구멍이다.
func (s *Store) DropInbox(ctx context.Context, sessionID, socket string) (int64, error) {
	var n int64
	err := s.Tx(ctx, func(t *Tx) error {
		res, err := t.tx.ExecContext(t.ctx, `DELETE FROM session_inbox WHERE session_id = ? AND socket = ?`, sessionID, socket)
		if err != nil {
			return fmt.Errorf("세션 받는 곳 지우기 실패(session_id=%q): %w", clip(sessionID, 64), err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// ListInbox 는 한 프로젝트 · 한 기계에서 받는 곳이 적힌 세션이다(닫힌 카드 done 은 뺀다).
// cc_session_id 는 session 의 **지금** 값이다: rekey 가 옮긴 뒤에도 받는 세션이 자기 id 로 알아본다.
func (s *Store) ListInbox(ctx context.Context, project, machineID string) ([]model.SessionInbox, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.session_id, s.cc_session_id, i.socket, i.mode, i.seen_at
		  FROM session_inbox i JOIN session s ON s.id = i.session_id
		 WHERE s.project = ? AND s.machine_id = ? AND s.state <> ?
		 ORDER BY i.session_id`, project, machineID, string(model.SessionDone))
	if err != nil {
		return nil, fmt.Errorf("세션 받는 곳 조회 실패(project=%q): %w", clip(project, 64), err)
	}
	defer rows.Close()
	out := []model.SessionInbox{}
	for rows.Next() {
		var e model.SessionInbox
		var seen string
		if err := rows.Scan(&e.SessionID, &e.CCSessionID, &e.Socket, &e.Mode, &seen); err != nil {
			return nil, fmt.Errorf("세션 받는 곳 해석 실패: %w", err)
		}
		if e.SeenAt, err = parseTime(seen); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LegacyAsk 는 옛 등록 파일 하나의 물음이다: 그 세션의 cc 와 그 세션을 띄운 폴더(등록 파일의 project_dir).
type LegacyAsk struct {
	CC  string
	Dir string
}

// LiveSessionsOf 는 물은 옛 등록 중 그 프로젝트 · 그 기계에서 **그 폴더를 품은** 닫히지 않은 카드가 있는 cc 다.
//
// ★ 옮겨 가는 동안만 쓰는 길이다(board/README 「세션에 알림」의 「옛 등록 파일」). 옛 플러그인으로 뜬
// 세션은 받는 곳을 보드 폴더에만 적고 여기에는 안 적는다. 보드가 그 파일의 cc 와 폴더를 물으면 원장이
// 어느 것이 이 프로젝트 · 이 기계 것인지 답한다.
//
// ★ cc 만으로 답하지 않는다. 한 Claude Code 세션은 여러 프로젝트에 카드를 연다(조정 세션이 남의 프로젝트에
// MCP 로 쓰거나 작업자가 다른 저장소에서 훅을 쏜다). cc 만 보면 그 세션의 소켓이 카드를 가진 모든
// 프로젝트의 초인종을 받는다: 한 조정 세션이 남의 프로젝트 알림을 받고, 그 프로젝트 세션은 못 받았다.
// 소켓은 프로세스 하나의 것이고 그 프로세스가 뜬 폴더가 그 세션의 프로젝트다.
func (s *Store) LiveSessionsOf(ctx context.Context, project, machineID string, asks []LegacyAsk) ([]string, error) {
	out := []string{}
	dirs := map[string][]string{}
	for _, a := range asks {
		if a.CC != "" && filepath.IsAbs(a.Dir) {
			dirs[a.CC] = append(dirs[a.CC], filepath.Clean(a.Dir))
		}
	}
	if len(dirs) == 0 {
		return out, nil
	}
	args := []any{project, machineID, string(model.SessionDone)}
	for c := range dirs {
		args = append(args, c)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(dirs)), ",")
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT cc_session_id, worktree FROM session
		 WHERE project = ? AND machine_id = ? AND state <> ? AND cc_session_id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("옛 등록 세션 조회 실패(project=%q): %w", clip(project, 64), err)
	}
	defer rows.Close()
	hit := map[string]bool{}
	for rows.Next() {
		var c, wt string
		if err := rows.Scan(&c, &wt); err != nil {
			return nil, fmt.Errorf("옛 등록 세션 해석 실패: %w", err)
		}
		for _, d := range dirs[c] {
			if dirWithin(d, wt) {
				hit[c] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for c := range hit {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// dirWithin 은 dir 가 root 와 같거나 그 아래인가다(둘 다 절대 경로, 경로 조각 단위). 순수 함수다.
func dirWithin(dir, root string) bool {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return false
	}
	return dir == root || strings.HasPrefix(dir, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}
