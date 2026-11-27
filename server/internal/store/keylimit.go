package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// keyLimitGate 는 Postgres 원장을 올리기 전에(판올림 전 덤프 · 증분보다 먼저) 인덱스 키 상한(pg.Column.MaxBytes)을 넘는 행을
// 찾아, 있으면 표 · 칸 · 행의 기본키 · 바이트 수를 대고 거절한다. SQLite 는 B-tree 항목에 상한이 없어 재지 않는다.
//
// ★ 왜 증분 전에 따로 재나: 증분 019 의 event_by_project_kind(project, kind, id) 는 넘는 행 하나로 CREATE INDEX 가 실패한다. 단은
// 트랜잭션째 되감겨 판 18 로 남지만, 그 오류는 행을 대지 않고(「index row size … exceeds btree version 4 maximum」) 이 바이너리의
// 서버는 판 19 를 기다려 뜨지 않는다. 덤프를 뜬 뒤에야 알게 되는 것도 헛일이다. 상한은 이관의 사전 검사와 같은 값이다(FaultTooLong -
// 한 자리, pg.Tables).
// ★ 상한을 넘는 값은 서버가 받은 요청의 프로젝트 값을 그대로 남긴 실패 기록에서나 나온다. 정상 원장의 project 는 저장소 디렉토리
// 이름이라 상한보다 훨씬 작다. 상한까지 채운 값을 그 인덱스가 받는지는 keylimit_test.go 가 압축이 안 되는 값으로 잰다.
func keyLimitGate(ctx context.Context, q dbtx) error {
	if q.dialect() != enginePG {
		return nil
	}
	var bad []string
	for _, t := range pg.Tables() {
		var keys []string
		for _, c := range t.Cols {
			if c.Key {
				keys = append(keys, c.Name+"::text")
			}
		}
		for _, c := range t.Cols {
			if c.MaxBytes == 0 {
				continue
			}
			query := fmt.Sprintf(`SELECT concat_ws(' · ', %s), octet_length(%s) FROM %s WHERE octet_length(%s) > ? ORDER BY 1 LIMIT 5`,
				strings.Join(keys, ", "), c.Name, t.Name, c.Name)
			err := eachRow(ctx, q, query, []any{c.MaxBytes}, t.Name+"."+c.Name+" 의 키 상한 재기", func(r *rowSet) error {
				var key string
				var n int
				if err := r.Scan(&key, &n); err != nil {
					return err
				}
				bad = append(bad, fmt.Sprintf("%s.%s 행 %s 이 %d 바이트(상한 %d)", t.Name, c.Name, key, n, c.MaxBytes))
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("스키마 거절: 인덱스 키 상한을 넘는 값이 있어 판을 올리지 않는다(덤프도 뜨지 않았다): %s - 이 판의 인덱스가 그 "+
		"행을 못 받는다. event 는 추가 전용이라 서버로는 못 고친다: 서버를 내린 채 소유 역할(%s)로 한 트랜잭션 안에서 "+
		"`ALTER TABLE event DISABLE TRIGGER event_no_update` → 그 행의 칸을 상한 안으로 줄이고(UPDATE event SET <칸> = left(<칸>, <글자 수>) "+
		"WHERE id IN (…)) → "+
		"`ALTER TABLE event ENABLE TRIGGER event_no_update` 를 돌린 뒤 다시 올린다",
		strings.Join(bad, "; "), pg.OwnerRole)
}
