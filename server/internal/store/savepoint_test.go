package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
)

// Tx 안에서 DB 오류를 받고 이어 가는 두 꼴(Tx.Savepoint · insertUnlessTaken)의 시험. 두 엔진에서 같은 단정을 돈다 -
// SQLite 는 지금 운영 길이라 동작이 그대로여야 하고, Postgres 는 실패한 문장 뒤의 트랜잭션을 통째로 버리는(25P02)
// 엔진이라 이 꼴이 없으면 커밋이 안 된다. 저장점 없이 삼킨 트랜잭션이 버려지는 것은 pg_tx_test.go 의
// TestPGSwallowedErrorAbortsTheTx 가 잰다(이 파일 단정들의 대조군이다).

// bothEngines 는 run 을 SQLite 파일과 Postgres 시험 DB 에서 한 번씩 돈다.
func bothEngines(t *testing.T, run func(t *testing.T, s *Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { run(t, newStore(t)) })
	t.Run("postgres", func(t *testing.T) { run(t, newPGStore(t, lockTimeout)) })
}

func machineIDs(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT id FROM machine ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSavepointTrapsTheErrorAndKeepsTheTx 는 저장점이 오류를 그 안에 가두는지 잰다: 실패한 저장점은 자기 안에서 쓴 것만
// 되돌리고 오류를 그대로 내며, 트랜잭션은 살아 뒤 문장과 커밋이 돈다. 성공한 저장점 · 중첩도 함께 본다.
func TestSavepointTrapsTheErrorAndKeepsTheTx(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		err := s.Tx(context.Background(), func(x *Tx) error {
			if err := insertMachine(x, "before"); err != nil {
				return err
			}
			if err := x.Savepoint(func(sp *Tx) error { return insertMachine(sp, "kept") }); err != nil {
				return fmt.Errorf("성공할 저장점이 실패했다: %w", err)
			}
			// 둘째 문장이 실패하면 첫째(inside)도 되돌아간다 - 반쯤 쓴 것을 남기지 않는다.
			spErr := x.Savepoint(func(sp *Tx) error {
				if err := insertMachine(sp, "inside"); err != nil {
					return err
				}
				return insertMachine(sp, "before")
			})
			if v := JudgeError(spErr); v.Kind != ConflictDuplicate {
				return fmt.Errorf("저장점이 안의 중복 위반을 그대로 내지 않았다(판정 %+v): %v", v, spErr)
			}
			// 중첩: 안쪽이 실패해도 바깥 저장점과 그 안의 앞뒤 쓰기는 산다.
			if err := x.Savepoint(func(outer *Tx) error {
				if err := insertMachine(outer, "outer"); err != nil {
					return err
				}
				if err := outer.Savepoint(func(in *Tx) error { return insertMachine(in, "kept") }); err == nil {
					return errors.New("중복인데 안쪽 저장점이 성공했다")
				}
				return insertMachine(outer, "outer-after")
			}); err != nil {
				return fmt.Errorf("바깥 저장점이 실패했다: %w", err)
			}
			return insertMachine(x, "after")
		})
		if err != nil {
			t.Fatalf("저장점 안에 가둔 오류가 트랜잭션을 죽였다: %v", err)
		}
		want := []string{"after", "before", "kept", "outer", "outer-after"}
		if got := machineIDs(t, s); !slices.Equal(got, want) {
			t.Fatalf("커밋된 행 %v(기대 %v)", got, want)
		}
	})
}

// TestAcquireResourceNamesTheHolder 는 이미 쥔 자원을 잡으면 그 점유자를 담은 ResourceHeldError 가 오고, 그 뒤에도
// 같은 트랜잭션이 이어 쓰고 커밋되는지 잰다. Postgres 에서 앞 판은 막힌 삽입 뒤의 점유자 조회가 25P02 로 죽어
// 일반 오류가 나갔고, 첫 획득도 LastInsertId 가 없어 실패했다.
func TestAcquireResourceNamesTheHolder(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		a, b := mustSession(t, s, "p", "cc-a"), mustSession(t, s, "p", "cc-b")

		at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		first, err := s.AcquireResource(ctx, "p", "landing", Holder{SessionID: a.ID}, at)
		if err != nil {
			t.Fatalf("빈 자원 획득 실패: %v", err)
		}
		if id := pgCount(t, s, `SELECT id FROM resource_hold WHERE project = 'p' AND resource = 'landing'`); first.ID != id {
			t.Fatalf("돌려준 점유 id %d 가 행의 id %d 와 다르다", first.ID, id)
		}

		_, err = s.AcquireResource(ctx, "p", "landing", Holder{SessionID: b.ID}, time.Time{})
		var held *ResourceHeldError
		if !errors.As(err, &held) {
			t.Fatalf("쥔 자원을 다시 잡았는데 ResourceHeldError 가 아니다: %v", err)
		}
		if held.Holder.SessionID != a.ID || !held.AcquiredAt.Equal(at) {
			t.Fatalf("점유자 %s · 획득 %s(기대 %s · %s)", held.Holder, held.AcquiredAt, a.ID, at)
		}

		// 막힌 획득 뒤에도 트랜잭션이 산다 - 다른 자원을 잡고 커밋한다.
		err = s.Tx(ctx, func(x *Tx) error {
			if _, err := x.AcquireResource("p", "landing", Holder{SessionID: b.ID}, time.Time{}); !errors.As(err, &held) {
				return fmt.Errorf("트랜잭션 안에서도 ResourceHeldError 여야 한다: %v", err)
			}
			_, err := x.AcquireResource("p", "r2", Holder{SessionID: b.ID}, time.Time{})
			return err
		})
		if err != nil {
			t.Fatalf("막힌 획득 뒤의 쓰기 · 커밋이 실패했다: %v", err)
		}
		if h, err := s.HeldBy(ctx, "p", "r2"); err != nil || h.SessionID != b.ID {
			t.Fatalf("막힌 획득 뒤에 잡은 자원이 커밋되지 않았다: %+v, %v", h, err)
		}

		// 유니크 충돌이 아닌 위반은 막힘이 아니라 제약 위반이다.
		_, err = s.AcquireResource(ctx, "없음", "landing", Holder{SessionID: a.ID}, time.Time{})
		var ce *ConflictError
		if !errors.As(err, &ce) || ce.Kind != ConflictMissingRef || ce.Target != TargetResourceHold {
			t.Fatalf("등록 안 된 프로젝트로 잡았는데 resource_hold 의 FK 위반이 아니다: %v", err)
		}
	})
}

// TestEnqueueLandingReentersAndTheTxGoesOn 은 이미 줄에 선 세션이 다시 서면 그 행(자원 집합까지)이 그대로 오고, 같은
// 트랜잭션이 자원 획득과 커밋까지 이어 가는지 잰다. Postgres 에서 앞 판은 막힌 삽입 뒤의 행 조회가 25P02 로 죽었다.
func TestEnqueueLandingReentersAndTheTxGoesOn(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		a := mustSession(t, s, "p", "cc-a")

		var first model.LandingRow
		if err := s.Tx(ctx, func(x *Tx) error {
			var err error
			first, err = x.EnqueueLanding("p", a.ID, []string{"r2", "landing"}, time.Time{})
			return err
		}); err != nil {
			t.Fatalf("첫 줄 서기 실패: %v", err)
		}
		if !slices.Equal(first.Resources, []string{"landing", "r2"}) || first.ID == 0 {
			t.Fatalf("첫 줄 행 %+v", first)
		}

		err := s.Tx(ctx, func(x *Tx) error {
			again, err := x.EnqueueLanding("p", a.ID, []string{"landing"}, time.Time{})
			if err != nil {
				return fmt.Errorf("재진입: %w", err)
			}
			if again.ID != first.ID || !slices.Equal(again.Resources, first.Resources) {
				return fmt.Errorf("재진입이 다른 행을 냈다: %d %v(기대 %d %v)", again.ID, again.Resources, first.ID, first.Resources)
			}
			_, err = x.AcquireResource("p", "landing", Holder{SessionID: a.ID}, time.Time{})
			return err
		})
		if err != nil {
			t.Fatalf("재진입 뒤의 쓰기 · 커밋이 실패했다: %v", err)
		}
		if h, err := s.HeldBy(ctx, "p", "landing"); err != nil || h.SessionID != a.ID {
			t.Fatalf("재진입 뒤에 잡은 레인이 커밋되지 않았다: %+v, %v", h, err)
		}
		if n := pgCount(t, s, `SELECT count(*) FROM landing_queue WHERE session_id = ? AND left_at IS NULL`, a.ID); n != 1 {
			t.Fatalf("살아 있는 줄 행이 %d개다(기대 1)", n)
		}

		err = s.Tx(ctx, func(x *Tx) error {
			_, err := x.EnqueueLanding("p", "없는-세션", []string{"landing"}, time.Time{})
			return err
		})
		var ce *ConflictError
		if !errors.As(err, &ce) || ce.Kind != ConflictMissingRef || ce.Target != TargetLandingQueue {
			t.Fatalf("등록 안 된 세션으로 섰는데 landing_queue 의 FK 위반이 아니다: %v", err)
		}
	})
}
