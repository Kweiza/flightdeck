package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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
// 같은 트랜잭션이 이어 쓰고 커밋되는지 잰다. Postgres 에서 앞 판은 첫 획득부터 LastInsertId(Postgres 드라이버가
// 지원하지 않는다)에서 죽어 이 시험의 첫 단계도 못 지났다. 그것을 넘었어도 막힌 삽입 뒤의 점유자 조회는 버려진
// 트랜잭션(25P02)에 닿는 자리였다.
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
// 트랜잭션이 자원 획득과 커밋까지 이어 가는지 잰다. Postgres 에서 앞 판은 첫 줄 서기부터 LastInsertId(Postgres
// 드라이버가 지원하지 않는다)에서 죽었다. 그것을 넘었어도 막힌 삽입 뒤의 행 조회는 버려진 트랜잭션(25P02)에 닿는
// 자리였다.
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

// TestSavepointCleanupFailureStopsTheTx 는 저장점을 되돌리지 못하면 fn 의 오류(중복 · 없음) 대신 흡수할 수 없는
// *SavepointError 가 오고, 부른 쪽이 그것을 삼키고 이어 써도 아무것도 커밋되지 않는지 잰다. fn 안의 ROLLBACK 이 바깥
// 트랜잭션을 끝내 ROLLBACK TO 가 실패하는 길이다 - 그 연결에서 뒤 문장은 두 엔진 다 자동 커밋으로 돈다.
func TestSavepointCleanupFailureStopsTheTx(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		var spErr, afterErr error
		err := s.Tx(context.Background(), func(x *Tx) error {
			if err := insertMachine(x, "before"); err != nil {
				return err
			}
			spErr = x.Savepoint(func(sp *Tx) error {
				if _, err := sp.tx.raw.ExecContext(sp.ctx, "ROLLBACK"); err != nil {
					return fmt.Errorf("시험 준비(바깥 트랜잭션 끝내기) 실패: %w", err)
				}
				// 흡수하는 쪽이 찾는 두 뜻을 다 싣는다 - 정리 실패가 이것을 품으면 아래 단정이 잡는다.
				return errors.Join(&ConflictError{Kind: ConflictDuplicate, Target: TargetItem}, ErrNotFound)
			})
			// 부른 쪽이 흡수하고 이어 가는 꼴이다(Finish 의 후속 등록 · 읽기 자리).
			afterErr = insertMachine(x, "after")
			return nil
		})

		var se *SavepointError
		if !errors.As(spErr, &se) || se.Step != "되돌리기" {
			t.Fatalf("정리 실패가 *SavepointError(되돌리기)로 오지 않았다: %v", spErr)
		}
		var ce *ConflictError
		if errors.As(spErr, &ce) || errors.Is(spErr, ErrNotFound) {
			t.Fatalf("정리 실패가 fn 의 오류(중복 · 없음)로 읽힌다 - 부른 쪽이 흡수하고 이어 간다: %v", spErr)
		}
		if !Unabsorbable(spErr) {
			t.Fatalf("정리 실패를 흡수해도 된다고 판정했다: %v", spErr)
		}
		if !errors.Is(afterErr, se) {
			t.Fatalf("망가진 트랜잭션이 뒤 문장을 받았다: %v", afterErr)
		}
		if !errors.Is(err, se) {
			t.Fatalf("망가진 트랜잭션의 커밋이 그 까닭으로 거절되지 않았다: %v", err)
		}
		if got := machineIDs(t, s); len(got) != 0 {
			t.Fatalf("망가진 트랜잭션에서 쓴 것이 남았다: %v", got)
		}
	})
}

// TestBrokenTxFlushesItsEventsAfterRollback 은 저장점만 사라지고 바깥 트랜잭션은 열린 채 남은 정리 실패를 클로저가
// 삼켰을 때, 커밋 거절 뒤 예약 이벤트가 롤백 표시를 달고 원장에 남는지 잰다. 망가진 트랜잭션의 커밋은 드라이버에 안
// 가서 트랜잭션과 그 전역 잠금이 열린 채 남는다 - 롤백 전에 예약 이벤트를 흘리던 판은 이벤트 쓰기가 자기 잠금을 잠금
// 대기 상한까지 기다리다 기록을 잃었다(두 엔진 다).
//
// ★ 실패를 바깥 저장점 안의 안쪽 저장점에서 낸다. Postgres 는 최상위에서 문장이 실패하면 그 자리에서 트랜잭션의 잠금을
// 다 푼다(ROLLBACK 을 기다리는 버려진 블록만 남는다) - 그러면 잠금 대기가 안 생겨 이 갈래를 못 밟는다. 하위 트랜잭션
// (저장점) 안의 실패는 그 저장점의 것만 풀고 최상위의 전역 잠금은 그대로 쥔다. SQLite 는 어느 쪽이든 쥔다.
func TestBrokenTxFlushesItsEventsAfterRollback(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		var spErr error
		start := time.Now()
		err := s.Tx(context.Background(), func(x *Tx) error {
			x.LogEvent("test.broken_tx", "", "", map[string]any{"step": "예약"})
			if err := insertMachine(x, "before"); err != nil {
				return err
			}
			_ = x.Savepoint(func(outer *Tx) error {
				spErr = outer.Savepoint(func(in *Tx) error {
					// 안쪽 저장점만 먼저 풀어 뒤의 ROLLBACK TO 가 실패하게 한다. 바깥 트랜잭션과 그 잠금은 그대로 산다.
					if _, err := in.tx.raw.ExecContext(in.ctx, "RELEASE SAVEPOINT fd_sp_2"); err != nil {
						return fmt.Errorf("시험 준비(저장점 풀기) 실패: %w", err)
					}
					return ErrNotFound
				})
				return nil
			})
			return nil // 정리 실패를 삼키고 끝까지 가는 꼴이다
		})
		elapsed := time.Since(start)

		var se *SavepointError
		if !errors.As(spErr, &se) || se.Step != "되돌리기" {
			t.Fatalf("저장점만 사라진 정리 실패가 *SavepointError(되돌리기)로 오지 않았다 - 시험이 그 갈래를 못 밟았다: %v", spErr)
		}
		if !errors.Is(err, se) {
			t.Fatalf("망가진 트랜잭션의 커밋이 그 까닭으로 거절되지 않았다: %v", err)
		}
		if n := pgCount(t, s, `SELECT count(*) FROM event WHERE kind = 'test.broken_tx' AND payload LIKE ?`,
			`%"tx":"rolled_back"%`); n != 1 {
			t.Fatalf("커밋이 거절된 트랜잭션의 예약 이벤트가 %d건 남았다(기대 1, 롤백 표시) - 열린 트랜잭션의 잠금 뒤에서 흘렸다", n)
		}
		if got := machineIDs(t, s); len(got) != 0 {
			t.Fatalf("망가진 트랜잭션에서 쓴 것이 남았다: %v", got)
		}
		if elapsed >= lockTimeout {
			t.Fatalf("트랜잭션이 %s 걸렸다 - 예약 이벤트가 자기 잠금을 대기 상한(%s)까지 기다렸다", elapsed, lockTimeout)
		}
	})
}

// TestSavepointPanicDoesNotCommitItsWrites 는 fn 이 쓰고 패닉하면 그 쓰기를 되돌린 뒤 패닉을 올리는지 잰다. 부른 쪽이 같은
// 트랜잭션에서 recover 하고 커밋해도 패닉 전에 쓴 것은 안 남고, 트랜잭션은 그대로 이어 쓴다.
func TestSavepointPanicDoesNotCommitItsWrites(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		var recovered any
		err := s.Tx(context.Background(), func(x *Tx) error {
			if err := insertMachine(x, "before"); err != nil {
				return err
			}
			func() {
				defer func() { recovered = recover() }()
				_ = x.Savepoint(func(sp *Tx) error {
					if err := insertMachine(sp, "in-panic"); err != nil {
						return err
					}
					panic("시험 패닉")
				})
			}()
			return insertMachine(x, "after")
		})
		if recovered != "시험 패닉" {
			t.Fatalf("패닉이 그대로 올라오지 않았다: %v", recovered)
		}
		if err != nil {
			t.Fatalf("패닉을 되돌린 뒤 트랜잭션이 못 이어 갔다: %v", err)
		}
		if got := machineIDs(t, s); !slices.Equal(got, []string{"after", "before"}) {
			t.Fatalf("커밋된 행 %v(기대 [after before]) - 패닉 전에 쓴 것이 남았거나 이어 쓴 것이 사라졌다", got)
		}
	})
}

// TestUnabsorbableRaisesStoreFailuresOnly 는 흡수 금지 판정이 저장층 실패(드라이버 오류 · 트랜잭션 결함 · 문맥 끝 ·
// 저장점 정리 실패)는 다 잡고, 뜻으로 흡수하는 없음 · 값 해석 오류는 놓아 주는지 잰다. 드라이버 오류는 두 엔진에서
// 실제로 낸다.
func TestUnabsorbableRaisesStoreFailuresOnly(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"nil":       {nil, false},
		"없음":        {fmt.Errorf("감쌈: %w", ErrNotFound), false},
		"타입 있는 없음":  {&NotFoundError{Kind: NFItem, Project: "p", ID: "x"}, false},
		"값 해석":      {errors.New("paths 칸을 못 풀었다"), false},
		"잠금 대기 넘침":  {fmt.Errorf("감쌈: %w", &pgconn.PgError{Code: "55P03"}), true},
		"직렬화":       {&pgconn.PgError{Code: "40001"}, true},
		"트랜잭션 결함":   {&TxFault{Kind: FaultDeadlock}, true},
		"저장점 정리 실패": {&SavepointError{Name: "fd_sp_1", Step: "풀기"}, true},
		"취소":        {fmt.Errorf("감쌈: %w", context.Canceled), true},
		"시한":        {context.DeadlineExceeded, true},
		"접속 오류":     {&connError{fault: "끊겼다"}, true},
	} {
		if got := Unabsorbable(c.err); got != c.want {
			t.Errorf("%s: Unabsorbable(%v) = %v(기대 %v)", name, c.err, got, c.want)
		}
	}
	bothEngines(t, func(t *testing.T, s *Store) {
		_, schemaErr := s.db.ExecContext(context.Background(), `SELECT no_such_column FROM machine`)
		if schemaErr == nil || !Unabsorbable(schemaErr) {
			t.Fatalf("스키마 어긋남을 흡수해도 된다고 판정했다: %v", schemaErr)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, cancelErr := s.db.ExecContext(ctx, `SELECT 1`)
		if cancelErr == nil || !Unabsorbable(cancelErr) {
			t.Fatalf("취소된 문맥의 오류를 흡수해도 된다고 판정했다: %v", cancelErr)
		}
	})
}
