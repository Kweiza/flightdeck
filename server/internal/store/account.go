package store

import (
	"context"
	"fmt"
)

// 계정 · 행위자 칸의 저장 계층. 표와 옛 행 귀속은 증분 020(migrations/020_accounts.sql · pg/020_accounts.sql)이 만든다. 쓰기가
// 행위자를 토큰에서 읽어 이 칸을 채우는 것은 다음 단계다 - 그때까지 새 행의 행위자 칸은 빈다(귀속 축이 그 수를 센다).

// OwnerAccountID 는 증분 020 이 옛 행을 귀속한 소유자 계정의 id 다(로그인 이름 · 표시 이름도 같다). 역할은 admin 이고 암호가 없다
// - 첫 관리자가 이 계정을 받는다. 행이 하나도 없던 원장에는 이 계정이 없다(이관 · 역이관의 빈 대상이 원본의 계정 행을 그대로 받게).
const OwnerAccountID = "owner"

// ActorColumn 은 행위자 칸 하나의 귀속 상태다: 표의 행 수와 그 칸이 빈 행 수.
type ActorColumn struct {
	Table, Column string
	Rows, Null    int64
}

// Attribution 은 원장 한 벌의 소유자 귀속 축이다(이관 · 역이관 · 대조 보고의 한 축). 원본 훑기와 같은 읽기 트랜잭션에서 잰다.
//
// ★ 지금은 재기만 하고 판정에 넣지 않는다. 이 판의 쓰기는 아직 행위자를 모르므로(위 머리말) 증분 020 뒤에 쓴 행은 칸이 비어
// 있는 것이 맞다 - 빈 칸을 거절하면 이 판으로 쓴 원장을 옮기지 못한다. 쓰기가 행위자를 싣는 단계가 그 칸을 NOT NULL 로 닫고, 이
// 축은 그 증분 전의 원장이 몇 행을 못 채웠는지 말하는 자리로 남는다.
type Attribution struct {
	// Owner 는 소유자 계정(OwnerAccountID)이 있는가다.
	Owner bool
	// Columns 는 actorColumns 순서다.
	Columns []ActorColumn
	// Via 는 judgment.via 값마다 행 수다(값 순). 빈 칸은 값 "" 로 센다.
	Via []ViaCount
}

// ViaCount 는 judgment.via 값 하나의 행 수다.
type ViaCount struct {
	Via  string
	Rows int64
}

// Unattributed 는 행위자 칸이 빈 행의 합이다(칸마다 센 것을 더한다 - 한 행이 두 칸에서 비면 둘로 센다).
func (a Attribution) Unattributed() int64 {
	var n int64
	for _, c := range a.Columns {
		n += c.Null
	}
	return n
}

// actorColumns 는 옛 행 귀속(증분 020)이 채우는 칸이다. item.created_session 은 귀속이 채우지 않으므로 없다(020 머리말 - 어느
// 세션이 만들었는지는 event 에 있다). machine.name 은 행위자가 아니라 기기의 표시 이름이다.
var actorColumns = []struct{ table, column string }{
	{"machine", "account_id"},
	{"session", "account_id"},
	{"item", "created_by"},
	{"judgment", "author"},
	{"judgment", "via"},
	{"item_revision", "author"},
}

// readAttribution 은 q 가 보는 원장의 소유자 귀속 축이다. q 는 읽기 트랜잭션이다 - 표마다 따로 읽어도 한 스냅숏이다.
func readAttribution(ctx context.Context, q Reader) (Attribution, error) {
	var a Attribution
	var owners int64
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM account WHERE id = ?`, OwnerAccountID).Scan(&owners); err != nil {
		return a, fmt.Errorf("소유자 계정 재기 실패: %w", err)
	}
	a.Owner = owners > 0
	for _, c := range actorColumns {
		var rows, filled int64
		// 표 · 칸 이름은 위 상수다(입력이 아니다).
		if err := q.QueryRowContext(ctx, `SELECT count(*), count(`+c.column+`) FROM `+c.table).Scan(&rows, &filled); err != nil {
			return a, fmt.Errorf("행위자 칸 %s.%s 재기 실패: %w", c.table, c.column, err)
		}
		a.Columns = append(a.Columns, ActorColumn{Table: c.table, Column: c.column, Rows: rows, Null: rows - filled})
	}
	rows, err := q.QueryContext(ctx, `SELECT coalesce(via, ''), count(*) FROM judgment GROUP BY coalesce(via, '') ORDER BY 1`)
	if err != nil {
		return a, fmt.Errorf("judgment.via 재기 실패: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v ViaCount
		if err := rows.Scan(&v.Via, &v.Rows); err != nil {
			return a, fmt.Errorf("judgment.via 행 해석 실패: %w", err)
		}
		a.Via = append(a.Via, v)
	}
	if err := rows.Err(); err != nil {
		return a, fmt.Errorf("judgment.via 순회 실패: %w", err)
	}
	return a, nil
}
