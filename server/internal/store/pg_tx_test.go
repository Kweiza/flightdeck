package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// Postgres 길의 트랜잭션 규칙 시험. 시험마다 FD_TEST_PG 서버에 기준선이 깔린 새 DB(템플릿 복제)를 받는다.
//
// ★ 업무 SQL 은 아직 SQLite 꼴이라 여기서 부르지 않는다. 기준선 표에 최소 문장만 직접 써서 규칙
// (격리 수준 · 전역 잠금 · 관측 잠금 · lock_timeout · 오류 옮기기) 자체를 잰다.

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newPGStore 는 기준선 위에 판 행(SchemaVersion)을 적은 새 DB 의 Store 다. 잠금 대기 상한은 wait 다.
//
// 판 행은 시험이 적는다 - 운영에서 그것을 적는 것은 Postgres 이전 실행기(migrateTo)다. 열기 뒤에 기동과 같은 판 재기
// (planOpen: 판 + DB 로케일)를 지나야 한다 - 시험 서버의 로케일이 관문을 지난다는 것을 시험마다 잰다.
func newPGStore(t *testing.T, wait time.Duration) *Store {
	t.Helper()
	dsn := storetest.New(t)
	s, err := openPG(ParseTarget(dsn), quietLog(), wait)
	if err != nil {
		t.Fatalf("openPG: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	stamp(t, s, SchemaVersion)
	if plan, err := planOpen(context.Background(), s.db); err != nil || plan.Action != MigrateNone {
		t.Fatalf("판 재기: %+v, %v", plan, err)
	}
	return s
}

func stamp(t *testing.T, s *Store, version int) {
	t.Helper()
	pgExec(t, s, `INSERT INTO schema_version(version, applied_at) VALUES (?, ?)`, version, fmtTime(time.Now()))
}

// pgExec 는 시험 준비 문장 하나를 단발로 쓴다(감싸개를 지나므로 `?` 를 쓴다).
func pgExec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("준비 문장 실패: %v\n%s", err, q)
	}
}

func pgCount(t *testing.T, s *Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("세기 실패: %v\n%s", err, q)
	}
	return n
}

// lockWaiters 는 이 DB 에서 권고 잠금을 기다리는 백엔드 수다.
func lockWaiters(t *testing.T, s *Store) int64 {
	return pgCount(t, s, `SELECT count(*) FROM pg_locks l JOIN pg_database d ON d.oid = l.database
		WHERE l.locktype = 'advisory' AND NOT l.granted AND d.datname = current_database()`)
}

// waitUntil 은 cond 가 참이 될 때까지 기다린다. 10초 안에 안 되면 실패다.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("10초 안에 오지 않았다: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// holding 은 쥐고 있는 쓰기 트랜잭션 하나다. release 를 부르면 커밋하고, 그 결과가 done 으로 온다.
type holding struct {
	release func()
	done    <-chan error
}

// hold 는 open 으로 트랜잭션을 열어 body 를 돌리고 쥔 채로 선다. 쥔 것이 확인돼야 돌아온다.
// 시험이 중간에 실패해도 정리 때 놓는다(놓지 않으면 Store 닫기가 그 연결을 영영 기다린다).
// T 는 클로저가 받는 면이다(업무 *Tx 또는 관측 *ObsTx).
func hold[T any](t *testing.T, open func(context.Context, func(T) error) error, body func(T) error) holding {
	t.Helper()
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	rel := func() { once.Do(func() { close(release) }) }
	t.Cleanup(rel)
	go func() {
		done <- open(context.Background(), func(x T) error {
			if body != nil {
				if err := body(x); err != nil {
					return err
				}
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("쥐기 전에 끝났다: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("10초 안에 트랜잭션을 쥐지 못했다")
	}
	return holding{release: rel, done: done}
}

func (h holding) finish(t *testing.T) {
	t.Helper()
	h.release()
	if err := <-h.done; err != nil {
		t.Fatalf("쥐고 있던 트랜잭션이 실패했다: %v", err)
	}
}

func noop(*Tx) error { return nil }

func obsNoop(*ObsTx) error { return nil }

// finishedOrWaiting 은 done 이 먼저 오면 그 결과를, 아니면 이 DB 에서 권고 잠금을 기다리는 백엔드가 생길 때까지
// 기다린다. 「잠금을 쥔 동안 끝나지 않는다」를 재는 자리가 쓴다 - 끝났으면 (true, 결과) 다.
func finishedOrWaiting(t *testing.T, what string, s *Store, done <-chan error) (bool, error) {
	t.Helper()
	var finished bool
	var got error
	waitUntil(t, what, func() bool {
		select {
		case got = <-done:
			finished = true
			return true
		default:
		}
		return lockWaiters(t, s) >= 1
	})
	return finished, got
}

func insertMachine(x *Tx, id string) error {
	_, err := x.tx.ExecContext(x.ctx,
		`INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES (?, 'h', 't', 't')`, id)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// 전역 잠금 · READ COMMITTED
// ─────────────────────────────────────────────────────────────────────────────

// TestPGTxSerializesReadThenWrite 는 「읽고 그 값으로 쓰기」가 겹쳐도 갱신을 잃지 않는지 잰다.
//
// ★ READ COMMITTED 는 행 잠금 없이 읽으므로, 전역 잠금이 없으면 둘이 같은 값을 읽고 둘 다 +1 을 적는다.
// 오류도 없이 조용히 잃는다 - 그래서 실패 수가 아니라 최종 값을 단정한다.
func TestPGTxSerializesReadThenWrite(t *testing.T) {
	s := newPGStore(t, lockTimeout)
	pgExec(t, s, `INSERT INTO project(id, path, created_at) VALUES ('p', '/p', 't')`)
	pgExec(t, s, `INSERT INTO counter(project, name, value) VALUES ('p', 'n', 0)`)

	const n = 24
	var wg sync.WaitGroup
	var failed atomic.Int64
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Tx(context.Background(), func(x *Tx) error {
				var v int64
				if err := x.tx.QueryRowContext(x.ctx,
					`SELECT value FROM counter WHERE project = ? AND name = ?`, "p", "n").Scan(&v); err != nil {
					return err
				}
				time.Sleep(3 * time.Millisecond) // 읽기와 쓰기 사이를 벌린다 - 잠금이 없으면 여기서 겹친다
				_, err := x.tx.ExecContext(x.ctx,
					`UPDATE counter SET value = ? WHERE project = ? AND name = ?`, v+1, "p", "n")
				return err
			})
			if err != nil {
				failed.Add(1)
				t.Errorf("경합 쓰기 실패: %v", err)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 {
		t.Fatalf("경합 쓰기 %d건이 실패했다 - 전역 잠금은 기다리게 해야지 실패시키면 안 된다", failed.Load())
	}
	if got := pgCount(t, s, `SELECT value FROM counter WHERE project = 'p' AND name = 'n'`); got != n {
		t.Fatalf("갱신을 잃었다: %d(기대 %d) - 읽고 쓰기가 전역 잠금 아래에서 한 줄로 서지 않았다", got, n)
	}
	if w := s.DBStats().Locks[LockGlobal]; w.Count != n || w.NotAcquired != 0 {
		t.Fatalf("잠금 대기 계측이 다르다: %+v(기대 Count %d, NotAcquired 0)", w, n)
	}
}

// TestPGTxReadsWhatTheLockHolderCommitted 는 잠금을 기다린 쪽이 앞사람이 커밋한 것을 보는지 잰다.
//
// ★ REPEATABLE READ 면 스냅숏이 첫 문장 - 잠금 문장 - 에서 찍혀, 기다리는 동안 앞사람이 커밋한 것을
// 못 본다. 잠금을 받고 나서 낡은 값으로 판정하는 것이다. 그래서 쓰기 트랜잭션은 READ COMMITTED 다.
func TestPGTxReadsWhatTheLockHolderCommitted(t *testing.T) {
	s := newPGStore(t, lockTimeout)
	a := hold(t, s.Tx, func(x *Tx) error { return insertMachine(x, "m1") })

	var seen int64
	bDone := make(chan error, 1)
	go func() {
		bDone <- s.Tx(context.Background(), func(x *Tx) error {
			return x.tx.QueryRowContext(x.ctx, `SELECT count(*) FROM machine`).Scan(&seen)
		})
	}()
	waitUntil(t, "뒷사람이 전역 잠금 앞에 선다", func() bool { return lockWaiters(t, s) == 1 })
	a.finish(t)
	if err := <-bDone; err != nil {
		t.Fatalf("뒷사람 트랜잭션: %v", err)
	}
	if seen != 1 {
		t.Fatalf("잠금을 받은 뒤의 읽기가 machine %d행을 봤다(기대 1) - 스냅숏이 잠금을 받기 전에 찍혔다", seen)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ReadTx - 한 스냅숏 · 읽기 전용
// ─────────────────────────────────────────────────────────────────────────────

func TestPGReadTxSeesOneSnapshot(t *testing.T) {
	s := newPGStore(t, lockTimeout)
	ctx := context.Background()
	pgExec(t, s, `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m1', 'h', 't', 't')`)

	var before, after int64
	err := s.ReadTx(ctx, func(q Reader) error {
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM machine`).Scan(&before); err != nil {
			return err
		}
		// 두 질의 사이에 다른 연결이 쓰고 커밋한다. ReadTx 는 잠그지 않으므로 막히지 않아야 한다.
		if err := s.Tx(ctx, func(x *Tx) error { return insertMachine(x, "m2") }); err != nil {
			return err
		}
		return q.QueryRowContext(ctx, `SELECT count(*) FROM machine`).Scan(&after)
	})
	if err != nil {
		t.Fatalf("ReadTx: %v", err)
	}
	if got := pgCount(t, s, `SELECT count(*) FROM machine`); got != 2 {
		t.Fatalf("사이 쓰기가 커밋되지 않았다(machine %d행) - 이 시험의 전제가 깨졌다", got)
	}
	if before != 1 || after != before {
		t.Fatalf("ReadTx 의 두 질의가 다른 시점을 봤다: %d → %d - 한 스냅숏이 아니다", before, after)
	}

	// 읽기 전용이다: 쓰기 면으로 꺼내 써도 DB 가 거절한다.
	err = s.ReadTx(ctx, func(q Reader) error {
		w, ok := q.(dbtx)
		if !ok {
			t.Fatal("ReadTx 의 Reader 가 쓰기 면을 안 가졌다 - 아래 단정이 무의미하다")
		}
		_, err := w.ExecContext(ctx, `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m3', 'h', 't', 't')`)
		return err
	})
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "25006" {
		t.Fatalf("ReadTx 안의 쓰기가 read_only_sql_transaction(25006)으로 거절되지 않았다: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 이벤트 - id 순서가 커밋 순서다
// ─────────────────────────────────────────────────────────────────────────────

// TestPGEventIDsFollowCommitOrder 는 동시 이벤트 기록 중에 본 max(id) 아래에 나중에 끼어드는 행이 없는지 잰다.
//
// ★ id(IDENTITY)는 INSERT 때 받고 보이는 것은 커밋 때다. 잠금 없이 넣으면 id 를 먼저 받은 쪽이 늦게 커밋할
// 수 있고, 그 사이 읽은 쪽은 「max(id) 까지 다 봤다」고 믿지만 그 아래 행이 나중에 나타난다.
// 각 관측(한 문장 = 한 스냅숏)의 count 가, 끝난 뒤 센 count(id ≤ 그때 max) 와 같아야 한다.
func TestPGEventIDsFollowCommitOrder(t *testing.T) {
	s := newPGStore(t, lockTimeout)
	ctx := context.Background()
	type look struct{ max, count int64 }
	var looks []look
	// 관측은 자기 연결 하나로 한다 - 풀을 쓰는 쪽과 다투면 관측이 몇 번 못 돈다.
	reader, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	stop, readerDone := make(chan struct{}), make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				readerDone <- nil
				return
			default:
			}
			var l look
			if err := reader.QueryRowContext(ctx, `SELECT coalesce(max(id), 0), count(*) FROM event`).Scan(&l.max, &l.count); err != nil {
				readerDone <- err
				return
			}
			looks = append(looks, l)
		}
	}()

	const writers, each = 16, 16
	var wg sync.WaitGroup
	var failed atomic.Int64
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if err := s.TryLogEvent(ctx, "probe.order", "", "", map[string]int{"w": w, "i": i}); err != nil {
					failed.Add(1)
					t.Errorf("이벤트 기록: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	if err := <-readerDone; err != nil {
		t.Fatalf("관측 질의: %v", err)
	}
	if failed.Load() != 0 {
		t.Fatalf("이벤트 기록 %d건 실패", failed.Load())
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id FROM event ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != writers*each {
		t.Fatalf("이벤트가 %d행이다(기대 %d)", len(ids), writers*each)
	}

	mid := 0 // 쓰는 도중의 관측 수 - 0 이나 끝 값만 봤다면 아무것도 안 잰 것이다
	var late []string
	for _, l := range looks {
		if l.count > 0 && l.count < int64(len(ids)) {
			mid++
		}
		final := int64(sort.Search(len(ids), func(i int) bool { return ids[i] > l.max }))
		if final != l.count {
			late = append(late, fmt.Sprintf("max(id)=%d 때 %d행 → 나중에 %d행", l.max, l.count, final))
		}
	}
	t.Logf("관측 %d번(쓰는 도중 %d번)", len(looks), mid)
	// 어긋남이 먼저다 - 잠금이 빠지면 쓰기가 빨라져 관측 수가 줄므로, 수 단정이 앞이면 진짜 사유를 가린다.
	if len(late) > 0 {
		t.Fatalf("본 max(id) 아래에 나중 행이 끼어들었다(관측 %d번 중 %d번) - id 순서가 커밋 순서가 아니다:\n  %s",
			len(looks), len(late), strings.Join(late[:min(len(late), 5)], "\n  "))
	}
	if mid < 20 {
		t.Fatalf("쓰는 도중의 관측이 %d번뿐이다(전체 %d번) - 이 시험이 아무것도 안 쟀다", mid, len(looks))
	}
}

// TestPGEventInsertWaitsForBusinessTx 는 트랜잭션 밖 이벤트 추가(TryLogEvent)가 업무 Tx 와 **같은** 잠금에서
// 기다리는지 잰다.
//
// ★ 업무 Tx 도 이벤트를 곧장 넣는 자리가 있다(세션 닫기처럼). 그 Tx 가 낮은 id 를 받은 채 커밋을 미루는 동안
// TryLogEvent 가 먼저 커밋하면, 읽는 쪽이 본 max(id) 아래에 업무 Tx 의 행이 나중에 나타난다. 위 시험은
// TryLogEvent 끼리만 겨뤄서, 이벤트 추가를 이벤트 전용 잠금으로 바꿔도 초록이다 - 그 빈칸을 이것이 잰다.
func TestPGEventInsertWaitsForBusinessTx(t *testing.T) {
	s := newPGStore(t, lockTimeout)
	ctx := context.Background()
	var heldID int64
	a := hold(t, s.Tx, func(x *Tx) error {
		return x.tx.QueryRowContext(x.ctx,
			`INSERT INTO event(at, kind, payload) VALUES (?, 'probe.held', '{}') RETURNING id`, fmtTime(time.Now())).Scan(&heldID)
	})
	done := make(chan error, 1)
	go func() { done <- s.TryLogEvent(ctx, "probe.after", "", "", nil) }()
	if finished, err := finishedOrWaiting(t, "이벤트 추가가 끝나거나 잠금을 기다린다", s, done); finished {
		t.Fatalf("업무 Tx 가 event id %d 를 받은 채 커밋을 미루는 동안 이벤트 추가가 끝났다(%v) - 같은 잠금에 서지 않는다",
			heldID, err)
	}
	a.finish(t)
	if err := <-done; err != nil {
		t.Fatalf("이벤트 추가: %v", err)
	}
	if after := pgCount(t, s, `SELECT id FROM event WHERE kind = 'probe.after'`); after <= heldID {
		t.Fatalf("나중에 커밋한 이벤트의 id(%d)가 앞 커밋의 id(%d) 이하다", after, heldID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 트랜잭션 밖이던 쓰기 · 패닉 - 전역 잠금을 비켜 가지 않는다
// ─────────────────────────────────────────────────────────────────────────────

// TestPGDropInboxWaitsForTheGlobalLock 은 받는 곳 지우기가 전역 잠금을 쥔 쓰기가 끝날 때까지 기다리는지 잰다.
// 트랜잭션 밖에서 곧장 지우면, 같은 행을 적으려고 잠금을 쥔 쓰기가 그 행을 만지기 전에 삭제가 먼저 커밋된다.
func TestPGDropInboxWaitsForTheGlobalLock(t *testing.T) {
	s := newPGStore(t, lockTimeout)
	ctx := context.Background()
	a := hold(t, s.Tx, nil)
	done := make(chan error, 1)
	go func() {
		_, err := s.DropInbox(ctx, "s1", "/s/1")
		done <- err
	}()
	if finished, err := finishedOrWaiting(t, "지우기가 끝나거나 잠금을 기다린다", s, done); finished {
		t.Fatalf("전역 잠금을 쥔 쓰기가 도는 동안 DropInbox 가 끝났다(%v) - 쓰기 줄을 비켜 간다", err)
	}
	a.finish(t)
	if err := <-done; err != nil {
		t.Fatalf("DropInbox: %v", err)
	}
}

// TestPGPanicInTxReleasesTheLock 은 클로저가 패닉한 트랜잭션이 연결과 전역 잠금을 남기지 않는지 잰다.
// 커밋도 롤백도 못 하고 빠지면, 끝나지 않는 ctx 에서는 잠금이 영영 남아 뒤 쓰기가 전부 lock_timeout 으로 죽는다.
func TestPGPanicInTxReleasesTheLock(t *testing.T) {
	s := newPGStore(t, 300*time.Millisecond)
	// 재는 동안에는 끝나지 않는 ctx 다. 정리 때만 취소한다 - 안전망을 뺀 변이에서도 Store 닫기가 서지 않게.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("패닉이 올라오지 않았다 - 안전망이 패닉을 삼키면 안 된다")
			}
		}()
		_ = s.Tx(ctx, func(x *Tx) error {
			if err := insertMachine(x, "panicked"); err != nil {
				return err
			}
			panic("클로저 패닉")
		})
	}()
	if err := s.Tx(ctx, noop); err != nil {
		t.Fatalf("패닉 뒤 다음 쓰기가 잠금을 못 얻었다: %v", err)
	}
	if got := pgCount(t, s, `SELECT count(*) FROM machine WHERE id = 'panicked'`); got != 0 {
		t.Fatalf("패닉한 트랜잭션의 쓰기 %d행이 남았다", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 증분 적용 - 업무 쓰기와 같은 잠금
// ─────────────────────────────────────────────────────────────────────────────

// TestPGWriteWaitsForSchemaStep 은 이전 실행기가 증분(DDL)을 얹는 동안 두 잠금을 함께 쥐고, 그동안 들어온 업무 쓰기가
// 증분이 커밋될 때까지 기다리는지 잰다.
//
// ★ 실행기의 길(migrateTo) 그대로 돌린다. 실행기 세션이 판을 읽기 전부터 끝까지 자기 잠금(migrateLockKey, 실행기끼리)과
// 업무 쓰기의 전역 잠금(globalLockKey)을 함께 쥐고, 증분 트랜잭션은 같은 세션에서 전역 잠금을 다시 잡는다(같은 세션이라
// 안 막힌다). 키가 하나만 있으면 DDL 도중에 업무 쓰기가 끼어 반쯤 바뀐 스키마 위에서 커밋한다 - 그 쓰기가 판 19 · 새 열을
// 보는 것이 그것이 안 끼었다는 증거다. 덤프를 뜨는 동안의 쓰기는 TestPGWriteWaitsForTheDump 가 잰다.
func TestPGWriteWaitsForSchemaStep(t *testing.T) {
	dsn, s := pgMigrated(t)
	ctx := context.Background()
	m, run := pgMigrator(t, dsn, MigrateOptions{Target: SchemaVersion + 1})
	run.steps = []Migration{{To: SchemaVersion + 1, Name: "시험 증분",
		SQL: `CREATE TABLE t_step_tbl(x int); ALTER TABLE machine ADD COLUMN t_probe text; SELECT pg_sleep(0.4)`}}
	run.backup = func(context.Context, *migrateSession, int) (string, error) { return "시험 덤프 자리", nil }
	aDone := make(chan error, 1)
	go func() { aDone <- m.migrateTo(ctx, run) }()
	waitUntil(t, "증분이 돈다", func() bool {
		return pgCount(t, s, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND state = 'active' AND pid <> pg_backend_pid()
			  AND query LIKE '%ADD COLUMN t_probe%'`) == 1
	})
	if n := advisoryHolders(t, s, migrateLockKey); n != 1 {
		t.Errorf("증분 도중 실행기 자기 잠금을 쥔 연결 %d개(기대 1)", n)
	}
	if n := pgCount(t, s, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
		  AND l.classid::bigint = (?::bigint >> 32) AND l.objid::bigint = (?::bigint & 4294967295)
		  AND a.pid <> pg_backend_pid() AND a.query LIKE '%ADD COLUMN t_probe%'`, globalLockKey, globalLockKey); n != 1 {
		t.Errorf("DDL 을 도는 증분 트랜잭션이 전역 업무 잠금을 쥐지 않았다(%d)", n)
	}

	var version, cols int64
	err := s.Tx(ctx, func(x *Tx) error {
		if err := x.tx.QueryRowContext(x.ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil {
			return err
		}
		if err := x.tx.QueryRowContext(x.ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'machine' AND column_name = 't_probe'`).Scan(&cols); err != nil {
			return err
		}
		_, err := x.tx.ExecContext(x.ctx,
			`INSERT INTO machine(id, hostname, first_seen, last_seen, t_probe) VALUES ('m1', 'h', 't', 't', 'after')`)
		return err
	})
	if aErr := <-aDone; aErr != nil {
		t.Fatalf("증분 적용: %v", aErr)
	}
	if err != nil {
		t.Fatalf("증분 도중에 들어온 쓰기가 실패했다: %v", err)
	}
	if version != int64(SchemaVersion+1) || cols != 1 {
		t.Fatalf("증분 도중에 들어온 쓰기가 적용 커밋 전에 돌았다: 판 %d(기대 %d), 새 열 %d개(기대 1)",
			version, SchemaVersion+1, cols)
	}
	if n := notOwnedTables(t, s); n != 0 {
		t.Fatalf("증분이 만든 표의 주인이 소유 역할이 아니다(%d개)", n)
	}
	if n := advisoryHolders(t, s, migrateLockKey); n != 0 {
		t.Fatalf("실행이 끝났는데 자기 잠금이 남았다(%d)", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 실패의 옮기기 - lock_timeout · 버려진 트랜잭션 · 제약
// ─────────────────────────────────────────────────────────────────────────────

// TestPGLockTimeoutIsATxFault 는 전역 잠금의 대기 초과가 여는 자리와 무관하게 같은 TxFault 로 오는지 잰다.
// 이전 실행기 세션(openMigrateSession)이 asFault 를 안 지나면 같은 55P03 이 거기서만 드라이버 오류로 나간다.
func TestPGLockTimeoutIsATxFault(t *testing.T) {
	const wait = 200 * time.Millisecond
	s := newPGStore(t, wait)
	a := hold(t, s.Tx, nil)

	ctx := context.Background()
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"업무 쓰기", func() error { return s.Tx(ctx, noop) }},
		{"이전 실행기", func() error {
			sess, err := s.openMigrateSession(ctx)
			if err == nil {
				sess.release()
			}
			return err
		}},
	} {
		start := time.Now()
		err := c.run()
		took := time.Since(start)
		var f *TxFault
		if !errors.As(err, &f) || f.Kind != FaultLockUnavailable || f.Code != "55P03" {
			t.Fatalf("%s: 잠금 대기 초과가 TxFault(lock_not_available, 55P03)로 오지 않았다: %v", c.name, err)
		}
		if took < wait {
			t.Fatalf("%s: lock_timeout(%s) 전에 포기했다: %s", c.name, wait, took)
		}
	}
	a.finish(t)
	if n := advisoryHolders(t, s, migrateLockKey); n != 0 {
		t.Fatalf("전역 잠금을 못 잡은 실행기가 자기 잠금을 놓지 않았다(%d)", n)
	}

	w := s.DBStats().Locks[LockGlobal]
	if w.NotAcquired != 2 || w.Count != 1 {
		t.Fatalf("잠금 계측: %+v(기대 NotAcquired 2 - 못 얻은 둘, Count 1 - 쥔 것)", w)
	}
}

// TestPGSwallowedErrorAbortsTheTx 는 문장 오류를 삼키고 이어 간 트랜잭션이 통째로 버려지고, 그것이
// TxFault(aborted)로 드러나는지 잰다. 이어 간 문장에서 나든(25P02), 끝까지 가서 커밋이 ROLLBACK 이 되든.
func TestPGSwallowedErrorAbortsTheTx(t *testing.T) {
	s := newPGStore(t, lockTimeout)
	ctx := context.Background()
	pgExec(t, s, `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('dup', 'h', 't', 't')`)

	for _, c := range []struct {
		name string
		tail func(x *Tx) error
	}{
		{"삼키고 이어 간 문장", func(x *Tx) error { return insertMachine(x, "after") }},
		{"삼키고 커밋", noop},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := s.Tx(ctx, func(x *Tx) error {
				if err := insertMachine(x, "before"); err != nil {
					return err
				}
				if err := insertMachine(x, "dup"); err == nil {
					t.Fatal("중복이 거절되지 않았다 - 이 시험의 전제가 깨졌다")
				}
				return c.tail(x)
			})
			var f *TxFault
			if !errors.As(err, &f) || f.Kind != FaultAborted || f.Code != "25P02" {
				t.Fatalf("버려진 트랜잭션이 TxFault(aborted, 25P02)로 오지 않았다: %v", err)
			}
			if got := pgCount(t, s, `SELECT count(*) FROM machine WHERE id IN ('before', 'after')`); got != 0 {
				t.Fatalf("버려진 트랜잭션의 앞 쓰기 %d행이 남았다", got)
			}
		})
	}
}

// TestPGConstraintErrorsAreJudged 는 실물 Postgres 의 제약 오류가 SQLite 와 같은 축으로 판정되는지 잰다.
// 순수 표(JudgeSQLState)만 있으면 우리가 지어낸 코드표를 우리가 단정하는 것이 된다.
func TestPGConstraintErrorsAreJudged(t *testing.T) {
	s := newPGStore(t, lockTimeout)
	ctx := context.Background()
	pgExec(t, s, `INSERT INTO project(id, path, created_at) VALUES ('p', '/p', 't')`)
	pgExec(t, s, `INSERT INTO event(at, kind) VALUES ('t', 'seed')`)

	for _, c := range []struct {
		name, q, state, constraint string
		kind                       ConflictKind
	}{
		{"PK 중복", `INSERT INTO project(id, path, created_at) VALUES ('p', '/p', 't')`, "23505", "project_pkey", ConflictDuplicate},
		{"FK", `INSERT INTO counter(project, name) VALUES ('없음', 'n')`, "23503", "counter_project_fkey", ConflictMissingRef},
		{"CHECK 는 안 접는다", `INSERT INTO signal(session_id, kind, at) VALUES ('없음', 'bogus', 't')`, "23514", "signal_kind_check", ""},
		{"NOT NULL 은 안 접는다", `INSERT INTO machine(id, hostname, first_seen, last_seen) VALUES ('m', NULL, 't', 't')`, "23502", "", ""},
		{"추가 전용 트리거는 안 접는다", `UPDATE event SET kind = 'x'`, "23000", "event_no_update", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := s.Tx(ctx, func(x *Tx) error {
				_, err := x.tx.ExecContext(x.ctx, c.q)
				return err
			})
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != c.state {
				t.Fatalf("SQLSTATE %s 를 기대했다: %v", c.state, err)
			}
			if c.constraint != "" && pe.ConstraintName != c.constraint {
				t.Fatalf("제약 이름 %q(기대 %q)", pe.ConstraintName, c.constraint)
			}
			var f *TxFault
			if errors.As(err, &f) {
				t.Fatalf("제약 위반이 트랜잭션 실패로 옮겨졌다: %v", f)
			}
			if v := JudgeError(err); v.Kind != c.kind || v.Fault != "" {
				t.Fatalf("판정 %+v(기대 Kind %q)", v, c.kind)
			}
			ce := conflictOf(err, TargetItem, "p", "x", "")
			if c.kind == "" {
				if ce != nil {
					t.Fatalf("접지 않아야 할 위반을 접었다: %v", ce)
				}
				return
			}
			if ce == nil || ce.Kind != c.kind || ce.State != c.state || ce.Constraint != c.constraint || ce.Code != 0 {
				t.Fatalf("ConflictError 가 다르다: %+v", ce)
			}
			if want := "code=" + c.state + "/" + c.constraint; !strings.Contains(ce.Error(), want) {
				t.Fatalf("오류 문구에 %q 가 없다: %s", want, ce.Error())
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ObsTx - (기기, 저장소) 잠금
// ─────────────────────────────────────────────────────────────────────────────

func TestPGObsTxLocksPerDeviceRepo(t *testing.T) {
	const wait = 300 * time.Millisecond
	s := newPGStore(t, wait)
	ctx := context.Background()
	obs := func(device, repo string) func(context.Context, func(*ObsTx) error) error {
		return func(ctx context.Context, fn func(*ObsTx) error) error { return s.ObsTx(ctx, device, repo, fn) }
	}

	a := hold(t, obs("d1", "r1"), nil)
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"같은 기기 다른 저장소", func() error { return s.ObsTx(ctx, "d1", "r2", obsNoop) }},
		{"다른 기기 같은 저장소", func() error { return s.ObsTx(ctx, "d2", "r1", obsNoop) }},
		{"업무 쓰기", func() error { return s.Tx(ctx, noop) }},
	} {
		if err := c.run(); err != nil {
			t.Fatalf("%s 가 (d1, r1) 관측 잠금에 막혔다: %v", c.name, err)
		}
	}
	var f *TxFault
	if err := s.ObsTx(ctx, "d1", "r1", obsNoop); !errors.As(err, &f) || f.Kind != FaultLockUnavailable {
		t.Fatalf("같은 (기기, 저장소)가 한 줄로 서지 않았다: %v", err)
	}
	a.finish(t)

	g := hold(t, s.Tx, nil)
	if err := s.ObsTx(ctx, "d1", "r1", obsNoop); err != nil {
		t.Fatalf("관측이 전역 잠금에 막혔다: %v", err)
	}
	g.finish(t)

	locks := s.DBStats().Locks
	if locks[LockObs].NotAcquired != 1 || locks[LockGlobal].NotAcquired != 0 {
		t.Fatalf("잠금 공간별 계측이 섞였다: %+v", locks)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 열기 - 판이 안 맞으면 뜨지 않는다
// ─────────────────────────────────────────────────────────────────────────────

// TestPGOpenAcceptsUpperCaseScheme 은 판별(ParseTarget)이 Postgres 로 가른 대문자 스킴을 여는 경계도 여는지 잰다.
// pgx 는 소문자 접두만 URL 로 읽어, 스킴을 안 맞추면 「Postgres 로 갈랐는데 못 연다」가 된다.
func TestPGOpenAcceptsUpperCaseScheme(t *testing.T) {
	dsn := storetest.New(t)
	i := strings.Index(dsn, "://")
	up := strings.ToUpper(dsn[:i]) + dsn[i:]
	if up == dsn || !ParseTarget(up).Postgres() {
		t.Fatalf("전제가 깨졌다: 대문자 스킴 %q 가 Postgres 로 갈리지 않는다", up)
	}
	raw, err := openPG(ParseTarget(up), quietLog(), lockTimeout)
	if err != nil {
		t.Fatalf("대문자 스킴을 못 열었다: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	stamp(t, raw, SchemaVersion)
	s, err := OpenWithLogger(up, quietLog())
	if err != nil {
		t.Fatalf("OpenWithLogger 가 대문자 스킴을 못 열었다: %v", err)
	}
	_ = s.Close()
}

// TestPGSessionLimitsAreSet 은 연결마다 거는 상한 둘이 실제로 걸렸는지 잰다. idle_in_transaction_session_timeout 이
// 빠지면 샌 트랜잭션이 전역 잠금을 영영 쥔다 - 걸려 있는지는 이 값으로만 보인다.
func TestPGSessionLimitsAreSet(t *testing.T) {
	const wait = 300 * time.Millisecond
	s := newPGStore(t, wait)
	if idleInTxTimeout <= lockTimeout {
		t.Fatalf("idle_in_transaction 상한(%s)이 lock_timeout(%s)보다 넉넉하지 않다", idleInTxTimeout, lockTimeout)
	}
	for name, want := range map[string]time.Duration{
		"lock_timeout":                        wait,
		"idle_in_transaction_session_timeout": idleInTxTimeout,
	} {
		var got string
		if err := s.db.QueryRowContext(context.Background(), `SELECT setting FROM pg_settings WHERE name = ?`, name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != strconv.FormatInt(want.Milliseconds(), 10) {
			t.Errorf("%s = %sms, 기대 %s", name, got, want)
		}
	}
}

func TestPGOpenRefusesUnstampedOrForeignVersion(t *testing.T) {
	dsn := storetest.New(t)
	raw, err := openPG(ParseTarget(dsn), quietLog(), lockTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	for _, c := range []struct {
		name  string
		stamp int // 0 이면 판 행을 안 적는다
		want  string
	}{
		{"판 행이 없다", 0, "기록된 버전이 없다"},
		{"다른 판", SchemaVersion - 1, "기대한다"},
	} {
		if c.stamp != 0 {
			stamp(t, raw, c.stamp)
		}
		s, err := OpenWithLogger(dsn, quietLog())
		if err == nil {
			_ = s.Close()
			t.Fatalf("%s: 열었다", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 거절 사유에 %q 가 없다: %v", c.name, c.want, err)
		}
	}
	stamp(t, raw, SchemaVersion)
	s, err := OpenWithLogger(dsn, quietLog())
	if err != nil {
		t.Fatalf("맞는 판을 못 열었다: %v", err)
	}
	if got := s.DBStats().Engine; got != enginePG.String() {
		t.Fatalf("엔진 %q", got)
	}
	_ = s.Close()
}

// TestPGOpenLogsServerCoordsWithoutNames 는 열기가 붙은 **뒤에** 서버가 말한 포트 · 판을 기동 로그에 한 줄 남기고, 이름
// (DB · 사용자)은 어디에도 안 싣는지 잰다. 붙었다는 것은 그 이름이 맞았다는 뜻일 뿐이다 - 잘못 읽힌 비밀번호 조각이 실제
// DB 이름과 같으면 그 조각으로 붙고, 서버가 current_database() 로 그 조각을 돌려준다(logPGServer 의 ★).
func TestPGOpenLogsServerCoordsWithoutNames(t *testing.T) {
	dsn := storetest.New(t)
	var logs bytes.Buffer
	s, err := openPG(ParseTarget(dsn), slog.New(slog.NewJSONHandler(&logs, nil)), lockTimeout)
	if err != nil {
		t.Fatalf("openPG: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(raw), &rec) == nil && rec["msg"] == "Postgres 원장에 붙었다" {
			line = rec
		}
	}
	if line == nil {
		t.Fatalf("기동 진단 줄이 없다: %s", logs.String())
	}
	// 포트는 서버 쪽에서 본 값이라 접속 URL 의 포트와 다를 수 있다(컨테이너의 포트 넘기기). 찼는지만 본다.
	if line["port"] == "" || line["server_version"] == "" {
		t.Fatalf("서버가 말한 포트 · 판이 비었다: %v", line)
	}
	for k := range line {
		if strings.Contains(k, "user") || strings.Contains(k, "database") {
			t.Fatalf("이름 칸(%s)을 실었다: %v", k, line)
		}
	}
	if name := strings.TrimPrefix(u.Path, "/"); strings.Contains(logs.String(), name) {
		t.Fatalf("기동 로그에 DB 이름(%s)이 실렸다: %s", name, logs.String())
	}
}

// TestPGReconnectFailureCarriesNoDSNPiece 는 열기 **뒤** 연결 풀이 다시 붙다 실패한 오류가 접속 문자열의 조각
// (사용자 · DB 이름)을 싣지 않는지 잰다. 드라이버의 ConnectError 는 `user=… database=…` 를 본문에 넣고, 그 DB 이름은
// 처음 받은 접속 문자열에서 읽은 값이다 - 경계가 모호한 문자열이면 비밀번호 조각이다. 풀에서 연결을 받는 자리
// (Ping · 단발 질의 · 트랜잭션 시작)가 다 store 경계에서 종류 · SQLSTATE 로 옮겨야 한다.
func TestPGReconnectFailureCarriesNoDSNPiece(t *testing.T) {
	dsn := storetest.New(t)
	s, err := openPG(ParseTarget(dsn), quietLog(), lockTimeout)
	if err != nil {
		t.Fatalf("openPG: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	storetest.CutConnections(t, dsn)

	ctx := context.Background()
	var one int
	for _, c := range []struct {
		where string
		run   func() error
	}{
		{"Ping", func() error { return s.Ping(ctx) }},
		{"단발 질의", func() error { return s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one) }},
		{"단발 실행", func() error { _, err := s.db.ExecContext(ctx, `SELECT 1`); return err }},
		{"여러 행 질의", func() error {
			rows, err := s.db.QueryContext(ctx, `SELECT 1`)
			if err == nil {
				rows.Close()
			}
			return err
		}},
		{"트랜잭션 시작", func() error { return s.ReadTx(ctx, func(Reader) error { return nil }) }},
	} {
		// database/sql 의 Ping 은 풀에 남은 끊긴 연결을 꺼내면 다시 붙지 않고 「끊겼다」만 낸다(그 연결은 버린다).
		// 그래서 재접속 거절(55000)에 닿을 때까지 몇 번 부르고, 그 사이의 오류도 전부 잰다.
		var msg string
		for try := 0; try < 3 && !strings.Contains(msg, "SQLSTATE 55000"); try++ {
			err := c.run()
			if err == nil {
				t.Fatalf("%s: 접속을 막았는데 성공했다", c.where)
			}
			msg = err.Error()
			for _, piece := range []string{name, "user=", "failed to connect"} {
				if strings.Contains(msg, piece) {
					t.Fatalf("%s: 재접속 오류에 접속 문자열의 조각(%s)이 실렸다: %s", c.where, piece, msg)
				}
			}
		}
		if !strings.Contains(msg, "SQLSTATE 55000") {
			t.Fatalf("%s: 재접속 거절을 종류 · SQLSTATE 로 옮기지 않았다: %s", c.where, msg)
		}
	}
}

// rstRelay 는 dsn 의 서버 앞에 TCP 중계를 세우고, 중계를 거치는 접속 URL 과 끊기(cut)를 낸다. cut 은 지금 중계 중인
// 연결을 모두 RST 로 끊는다 - 서버가 마지막 말 없이 사라지는 자리(망 단절 · 서버 기계의 급사)다. 중계는 계속 들으므로
// 연결 풀은 다시 붙는다.
func rstRelay(t *testing.T, dsn string) (string, func()) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	upstream := u.Host
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var live []net.Conn
	cut := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range live {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // 닫을 때 FIN 이 아니라 RST 를 보낸다
			}
			_ = c.Close()
		}
		live = nil
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = c.Close()
				continue
			}
			mu.Lock()
			live = append(live, c, up)
			mu.Unlock()
			go func() { _, _ = io.Copy(up, c) }()
			go func() { _, _ = io.Copy(c, up) }()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		cut()
	})
	u.Host = ln.Addr().String()
	return u.String(), cut
}

// TestPGBrokenConnCarriesNoRemoteAddr 는 붙은 연결이 도중에 끊겼을 때 store 가 내는 오류에 원격 주소 · 포트가 없는지
// 잰다. 드라이버는 그 자리의 네트워크 오류에 `tcp <내 주소>-><원격 주소>` 를 싣고, 그 본문이 문장 · 순회(rows.Err) ·
// 커밋의 오류를 타고 API 오류 로그까지 간다. 재접속 실패(ConnectError)만 가리던 앞 판이 놓친 자리다.
//
// ★ 끊는 길이 둘이다. pg_terminate_backend 로 자기 연결을 끊으면 서버가 마지막 말(FATAL 57P01)을 먼저 보내고 드라이버는
// 그것을 PgError 로 낸다 - 서버가 돌려준 SQLSTATE 라 그대로 지나야 하고, 그 뒤 문장은 「끊긴 연결」 종류로 와야 한다.
// 주소를 싣는 네트워크 오류는 서버가 마지막 말 없이 사라질 때 난다 - 중계(rstRelay)를 RST 로 끊어 그 자리를 만든다.
func TestPGBrokenConnCarriesNoRemoteAddr(t *testing.T) {
	dsn, cut := rstRelay(t, storetest.New(t))
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var one int
	for _, c := range []struct {
		name string
		run  func(t *testing.T, s *Store) error
	}{
		{"자기 연결 끊기(pg_terminate_backend)", func(t *testing.T, s *Store) error {
			return s.ReadTx(ctx, func(r Reader) error {
				var ok bool
				err := r.QueryRowContext(ctx, `SELECT pg_terminate_backend(pg_backend_pid())`).Scan(&ok)
				var pe *pgconn.PgError
				if !errors.As(err, &pe) || pe.Code != "57P01" {
					t.Fatalf("서버가 돌려준 FATAL 57P01 이 PgError 로 지나지 않았다: %v", err)
				}
				return r.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
			})
		}},
		{"트랜잭션 안의 문장", func(t *testing.T, s *Store) error {
			return s.ReadTx(ctx, func(r Reader) error {
				if err := r.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
					t.Fatalf("끊기 전 질의: %v", err)
				}
				cut()
				return r.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
			})
		}},
		{"도중의 순회(rows.Err)", func(t *testing.T, s *Store) error {
			// 첫 행(서버의 보내기 버퍼보다 커서 곧장 나간다)을 보낸 뒤 서버가 둘째 행에서 쉰다 - 그 사이에 끊는다.
			// 쉬지 않고 흘리면 받는 쪽 창이 찬 채로 RST 를 맞아 커널이 그것을 버리고, 다음 탐침까지 십수 초를 선다.
			// 쉬는 백엔드는 시험 DB 를 지울 때 FORCE 가 끝낸다.
			return s.ReadTx(ctx, func(r Reader) error {
				rows, err := r.QueryContext(ctx, `SELECT g, repeat('x', 20000),
					CASE WHEN g = 2 THEN (SELECT 0 FROM pg_sleep(30)) ELSE 0 END FROM generate_series(1, 2) g`)
				if err != nil {
					t.Fatalf("끊기 전 질의: %v", err)
				}
				defer rows.Close()
				if !rows.Next() {
					t.Fatalf("첫 행이 없다: %v", rows.Err())
				}
				cut()
				if rows.Next() {
					t.Fatal("끊은 뒤 둘째 행을 받았다")
				}
				return rows.Err()
			})
		}},
		{"커밋", func(t *testing.T, s *Store) error {
			return s.Tx(ctx, func(x *Tx) error {
				if err := insertMachine(x, "m"); err != nil {
					t.Fatalf("끊기 전 쓰기: %v", err)
				}
				cut()
				return nil
			})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := openPG(ParseTarget(dsn), quietLog(), lockTimeout)
			if err != nil {
				t.Fatalf("openPG: %v", err)
			}
			defer s.Close()
			err = c.run(t, s)
			if err == nil {
				t.Fatal("끊었는데 성공했다")
			}
			msg := err.Error()
			for _, piece := range []string{u.Host, ":" + u.Port(), "tcp ", "->"} {
				if strings.Contains(msg, piece) {
					t.Fatalf("오류에 원격 주소의 조각(%s)이 실렸다: %s", piece, msg)
				}
			}
			if !strings.Contains(msg, "Postgres 와 주고받지 못했다") {
				t.Fatalf("드라이버 오류를 종류로 옮기지 않았다: %s", msg)
			}
		})
	}
}

// TestPGRowKeepsDatabaseSQLResults 는 드라이버 오류를 옮기는 감싸개가 database/sql 이 스스로 낸 결과는 그대로 두는지
// 잰다. sql.ErrNoRows 는 부르는 쪽이 견주는 표지이고, 값 변환 오류(NULL 을 string 으로)는 드라이버 본문이 아니라 우리 쪽
// 결함이라 가리면 고칠 거리가 사라진다. *sql.Row 를 감싸던 앞 판은 둘을 드라이버 오류와 한 반환값으로 받았다(row 의 ★).
func TestPGRowKeepsDatabaseSQLResults(t *testing.T) {
	s := newPGStore(t, lockTimeout)
	ctx := context.Background()
	check := func(t *testing.T, r Reader) {
		t.Helper()
		var n int
		if err := r.QueryRowContext(ctx, `SELECT 1 WHERE false`).Scan(&n); err != sql.ErrNoRows {
			t.Fatalf("빈 결과가 sql.ErrNoRows 그 자체로 오지 않았다: %v", err)
		}
		var str string
		if err := r.QueryRowContext(ctx, `SELECT NULL::text`).Scan(&str); err == nil || !strings.Contains(err.Error(), "converting NULL to string") {
			t.Fatalf("한 행 질의의 값 변환 오류가 본문을 잃었다: %v", err)
		}
		rows, err := r.QueryContext(ctx, `SELECT NULL::text`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("행이 없다: %v", rows.Err())
		}
		if err := rows.Scan(&str); err == nil || !strings.Contains(err.Error(), "converting NULL to string") {
			t.Fatalf("여러 행 질의의 값 변환 오류가 본문을 잃었다: %v", err)
		}
	}
	t.Run("단발", func(t *testing.T) { check(t, s.db) })
	t.Run("트랜잭션", func(t *testing.T) {
		if err := s.ReadTx(ctx, func(r Reader) error { check(t, r); return nil }); err != nil {
			t.Fatal(err)
		}
	})
}
