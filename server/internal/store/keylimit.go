package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// keyLimitGate 는 Postgres 원장을 올리기 전에(판올림 전 덤프 · 증분보다 먼저) 인덱스 키 상한(pg.Column.MaxBytes)을 넘는 행을
// 찾아, 있으면 표 · 칸 · 행의 기본키 · 바이트 수를 대고 거절한다. at 은 원장의 지금 판이다(거절은 그 판을 그대로 둔다). SQLite 는
// B-tree 항목에 상한이 없어 재지 않는다.
//
// ★ 왜 증분 전에 따로 재나: 증분 019 의 event_by_project_kind(project, kind, id) 는 넘는 행 하나로 CREATE INDEX 가 실패한다. 단은
// 트랜잭션째 되감겨 판 18 로 남지만, 그 오류는 행을 대지 않고(「index row size … exceeds btree version 4 maximum」) 이 바이너리의
// 서버는 판 19 를 기다려 뜨지 않는다. 덤프를 뜬 뒤에야 알게 되는 것도 헛일이다. 상한은 이관의 사전 검사와 같은 값이다(FaultTooLong -
// 한 자리, pg.Tables).
// ★ **행을 고치라고 하지 않는다.** event 는 추가 전용 감사 원장이다 - 행을 자르면 SQLite 원본 · 덤프와 달라지고, 트리거를 끄라는
// 처방은 그 원장의 불변식을 사람 손에 맡긴다. 원장은 지금 판 그대로 남으니 좌표와 함께 이슈로 알려 달라고 한다.
// ★ 상한을 넘는 값은 서버가 받은 요청의 프로젝트 값을 그대로 남긴 실패 기록에서나 나온다. 정상 원장의 project 는 저장소 디렉토리
// 이름이라 상한보다 훨씬 작다. 새 행은 쓰기 관문이 같은 상한으로 막는다(eventKeyRefusal). 상한까지 채운 값을 그 인덱스가 받는지는
// keylimit_test.go 가 압축이 안 되는 값으로 잰다.
func keyLimitGate(ctx context.Context, q dbtx, at int) error {
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
	return fmt.Errorf("스키마 거절: 인덱스 키 상한을 넘는 값이 있어 판을 올리지 않는다(덤프도 뜨지 않았다 - 원장은 판 %d 그대로다): %s - "+
		"이 판의 인덱스가 그 행을 못 받을 수 있다. 행을 고치지 않는다(event 는 추가 전용 감사 원장이다 - 자르면 SQLite 원본 · 덤프와 "+
		"달라진다). 위 좌표를 실어 이슈로 알려 달라",
		at, strings.Join(bad, "; "))
}

// KeyTooLongError 는 event 의 인덱스 키 칸(project · kind)에 상한보다 긴 값을 쓰려 한 것이다(eventKeyRefusal). 쓰기 전에 거절한
// 입력 오류라 API 는 400 이다(api.ClassifyError).
type KeyTooLongError struct {
	// Column 은 걸린 칸이다(project · kind).
	Column string
	// Bytes 는 받은 값의 바이트 수, Max 는 상한이다.
	Bytes, Max int
}

func (e *KeyTooLongError) Error() string {
	return fmt.Sprintf("%s 가 %d 바이트다 - %d 바이트를 넘는 %s 는 받지 않는다(Postgres 인덱스의 키 상한이고, 두 엔진이 같이 잰다)",
		e.Column, e.Bytes, e.Max, e.Column)
}

// eventKeyRefusal 은 새로 쓸 값의 project · kind 가 event 인덱스 키 상한(pg.EventProjectMaxBytes · EventKindMaxBytes) 안인지 잰다.
// 넘으면 *KeyTooLongError 다. 두 엔진이 이 하나를 지난다: event 에 한 줄을 넣는 자리(Tx.insertEvent)와 프로젝트 id 를 등록하는
// 자리(Tx.UpsertProject - 그 id 가 뒤의 이벤트 project 가 된다). kind 가 비면 project 만 잰다.
//
// ★ 왜 엔진과 상관없이 막나: SQLite 는 받지만 받은 행은 Postgres 로 옮길 때(이관의 사전 검사) 걸리고, 판 18 의 Postgres 원장은
// 받아 두었다가 판올림(keyLimitGate)을 세운다. 둘 다 행을 고칠 수 없는 감사 원장이라 들어오는 자리에서 막는다. 판 19 의 Postgres
// 원장은 인덱스가 거절하지만 그 오류는 계측 실패(LogEvent 의 WARN)로만 남았다.
func eventKeyRefusal(project, kind string) error {
	if len(project) > pg.EventProjectMaxBytes {
		return &KeyTooLongError{Column: "project", Bytes: len(project), Max: pg.EventProjectMaxBytes}
	}
	if len(kind) > pg.EventKindMaxBytes {
		return &KeyTooLongError{Column: "kind", Bytes: len(kind), Max: pg.EventKindMaxBytes}
	}
	return nil
}
