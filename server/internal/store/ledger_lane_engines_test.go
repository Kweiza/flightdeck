package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/model"
)

// judgment.go · landing.go · resource.go · event.go · idempotency.go 의 업무 SQL 이 두 엔진에서 같은 답을 내는지 잰다.
// 모든 단정을 SQLite 파일과 Postgres 시험 DB 에서 한 번씩 돈다(bothEngines).
//
// ★ Postgres 에서 실패하던 자리 넷(판단 검색의 FTS5, snapshot upsert 의 충돌 대상, 발번 upsert 의 모호한 칸 이름,
// 멱등 초과분 청소의 `LIMIT -1`)은 아래 시험들이 따로 잠근다. 첫 시험은 다섯 파일의 나머지 실행 자리를 실제 호출로
// 한 번씩 다 지나게 해, 「읽어 보니 두 엔진에 같은 꼴이다」를 실행으로 굳힌다.

// TestLedgerLaneSQLRunsOnBothEngines 는 다섯 파일의 공개 질의를 한 줄거리로 다 부른다.
func TestLedgerLaneSQLRunsOnBothEngines(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
		seed(t, s, "p")
		seed(t, s, "q")
		mustItem(t, s, "p", "it")
		a := mustSession(t, s, "p", "cc-a")
		b := mustSession(t, s, "p", "cc-b")

		// ── 판단 · 링크 · 정정
		j1, err := s.AddJudgment(ctx, model.Judgment{
			Project: "p", SessionID: a.ID, Kind: model.JudgmentDecision, Title: "첫 판단", Body: "레인 줄 서기를 고른다", At: at(0),
			Links: []model.JudgmentLink{{TargetKind: "item", TargetID: "it"}, {TargetKind: "commit", TargetID: "abc"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		j2, err := s.AddJudgment(ctx, model.Judgment{
			Project: "p", SessionID: a.ID, Kind: model.JudgmentDecision, Body: "정정한다", Supersedes: j1.ID, At: at(1),
			Links: []model.JudgmentLink{{TargetKind: "item", TargetID: "it"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddJudgment(ctx, model.Judgment{Project: "p", SessionID: b.ID, Kind: model.JudgmentHandoff, Body: "넘김", At: at(2)}); err != nil {
			t.Fatal(err)
		}
		var ce *ConflictError
		if _, err := s.AddJudgment(ctx, model.Judgment{Project: "없는", Kind: model.JudgmentNow, Body: "b"}); !errors.As(err, &ce) || ce.Kind != ConflictMissingRef {
			t.Fatalf("등록 안 된 프로젝트의 판단은 참조 없음 충돌이어야 한다: %v", err)
		}
		got, err := s.GetJudgment(ctx, j1.ID)
		if err != nil || got.Title != "첫 판단" || !got.At.Equal(at(0)) || !reflectLinks(got.Links, "commit/abc", "item/it") {
			t.Fatalf("GetJudgment: %+v %v", got, err)
		}
		if _, err := s.GetJudgment(ctx, "없음"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("없는 판단: %v", err)
		}
		if js, err := s.ListJudgmentsBySession(ctx, a.ID); err != nil || judgmentIDs(js) != j1.ID+","+j2.ID {
			t.Fatalf("ListJudgmentsBySession: %v %v", judgmentIDs(js), err)
		}
		if js, err := s.ListJudgmentsByKind(ctx, "p", model.JudgmentDecision, 0); err != nil || judgmentIDs(js) != j2.ID+","+j1.ID {
			t.Fatalf("ListJudgmentsByKind(최신 먼저): %v %v", judgmentIDs(js), err)
		}
		if sup, err := s.SupersededBy(ctx, "p"); err != nil || !maps.Equal(sup, map[string]string{j1.ID: j2.ID}) {
			t.Fatalf("SupersededBy: %v %v", sup, err)
		}
		links, err := s.JudgmentLinksForItems(ctx, "p", []string{"it", "없음"})
		wantLinks := []string{j1.ID, j2.ID}
		slices.Sort(wantLinks)
		if err != nil || len(links) != 1 || !slices.Equal(links["it"], wantLinks) {
			t.Fatalf("JudgmentLinksForItems: %v %v", links, err)
		}
		if links, err := s.JudgmentLinksForItems(ctx, "p", nil); err != nil || len(links) != 0 {
			t.Fatalf("빈 입력의 JudgmentLinksForItems: %v %v", links, err)
		}
		if js, err := s.JudgmentsForItem(ctx, "p", "it"); err != nil || judgmentIDs(js) != j2.ID+","+j1.ID || len(js[1].Links) != 2 {
			t.Fatalf("JudgmentsForItem: %v %v", judgmentIDs(js), err)
		}
		if js, err := s.SearchJudgments(ctx, "", "레인 줄", 0); err != nil || judgmentIDs(js) != j1.ID {
			t.Fatalf("SearchJudgments: %v %v", judgmentIDs(js), err)
		}

		// ── 스냅숏
		for _, sn := range []model.Snapshot{
			{Project: "p", Key: "k1", Value: "1", Method: model.SnapshotCommand, ComputedAt: at(0)},
			{Project: "p", Key: "k1", Value: "2", Method: model.SnapshotManual, Evidence: "손으로 셌다", ComputedAt: at(1)},
			{Project: "p", Key: "k0", Value: "0", Method: model.SnapshotCommand, InputDigest: "d", ComputedAt: at(2)},
		} {
			if err := s.PutSnapshot(ctx, sn); err != nil {
				t.Fatalf("PutSnapshot(%s=%s): %v", sn.Key, sn.Value, err)
			}
		}
		if sn, err := s.GetSnapshot(ctx, "p", "k1"); err != nil || sn.Value != "2" || sn.Method != model.SnapshotManual ||
			sn.Evidence != "손으로 셌다" || !sn.ComputedAt.Equal(at(1)) {
			t.Fatalf("GetSnapshot(덮인 값): %+v %v", sn, err)
		}
		if _, err := s.GetSnapshot(ctx, "p", "없음"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("없는 스냅숏: %v", err)
		}
		if sns, err := s.ListSnapshots(ctx, "p"); err != nil || len(sns) != 2 || sns[0].Key != "k0" || sns[0].InputDigest != "d" || sns[1].Value != "2" {
			t.Fatalf("ListSnapshots: %+v %v", sns, err)
		}

		// ── 랜딩 줄
		var r1, r2 model.LandingRow
		if err := s.Tx(ctx, func(x *Tx) error {
			var err error
			if r1, err = x.EnqueueLanding("p", a.ID, []string{"path:x", "landing"}, at(0)); err != nil {
				return err
			}
			again, err := x.EnqueueLanding("p", a.ID, []string{"landing"}, at(5))
			if err != nil || again.ID != r1.ID || !slices.Equal(again.Resources, []string{"landing", "path:x"}) || !again.EnqueuedAt.Equal(at(0)) {
				return fmt.Errorf("다시 줄 서기는 살아 있는 행을 그대로 내야 한다: %+v %v", again, err)
			}
			if r2, err = x.EnqueueLanding("p", b.ID, []string{"landing"}, at(1)); err != nil {
				return err
			}
			if q, err := x.ListLandingQueue("p"); err != nil || len(q) != 2 || q[0].ID != r1.ID {
				return fmt.Errorf("Tx.ListLandingQueue: %+v %v", q, err)
			}
			if f, err := x.FrontLandingRowFor("p", "landing"); err != nil || f.ID != r1.ID {
				return fmt.Errorf("Tx.FrontLandingRowFor: %+v %v", f, err)
			}
			if l, err := x.LiveLandingRow("p", b.ID); err != nil || l.ID != r2.ID {
				return fmt.Errorf("Tx.LiveLandingRow: %+v %v", l, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if r1.ID <= 0 || r2.ID <= r1.ID {
			t.Fatalf("순번은 RETURNING 으로 받은 늘어나는 id 다: %d %d", r1.ID, r2.ID)
		}
		if q, err := s.ListLandingQueue(ctx, "p"); err != nil || len(q) != 2 || q[0].ID != r1.ID || q[1].ID != r2.ID ||
			!slices.Equal(q[0].Resources, []string{"landing", "path:x"}) || !q[1].EnqueuedAt.Equal(at(1)) {
			t.Fatalf("ListLandingQueue: %+v %v", q, err)
		}
		if f, err := s.FrontLandingRowFor(ctx, "p", "path:x"); err != nil || f.ID != r1.ID {
			t.Fatalf("FrontLandingRowFor: %+v %v", f, err)
		}
		if _, err := s.FrontLandingRowFor(ctx, "p", "없음"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("줄 없는 자원의 맨 앞: %v", err)
		}
		if err := s.CloseLandingRow(ctx, "p", r1.ID, model.LandingLeftOK, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.CloseLandingRow(ctx, "p", r1.ID, model.LandingLeftOK, ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("닫힌 행을 다시 닫기: %v", err)
		}
		if _, err := s.LiveLandingRow(ctx, "p", a.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("닫힌 뒤 LiveLandingRow: %v", err)
		}
		if err := s.Tx(ctx, func(x *Tx) error {
			last, err := x.LastLandingRow("p", a.ID)
			if err != nil || last.ID != r1.ID || last.LeftAt == nil || last.LeftKind != model.LandingLeftOK || len(last.Resources) != 2 {
				return fmt.Errorf("LastLandingRow: %+v %v", last, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for range 2 { // 둘째는 살아 있는 행이 없어 무동작이다
			if err := s.CloseLandingRowBySession(ctx, "p", b.ID, model.LandingLeftLeave, "비킨다"); err != nil {
				t.Fatal(err)
			}
		}
		if q, err := s.ListLandingQueue(ctx, "p"); err != nil || len(q) != 0 {
			t.Fatalf("다 닫은 뒤 줄: %+v %v", q, err)
		}

		// ── 자원 · 발번
		if h, err := s.AcquireResource(ctx, "p", "landing", Holder{SessionID: a.ID}, at(0)); err != nil || h.ID <= 0 {
			t.Fatalf("AcquireResource: %+v %v", h, err)
		}
		var held *ResourceHeldError
		if _, err := s.AcquireResource(ctx, "p", "landing", Holder{SessionID: b.ID}, at(0)); !errors.As(err, &held) || held.Holder.SessionID != a.ID {
			t.Fatalf("쥐인 자원 다시 잡기: %v", err)
		}
		// 같은 시각이면 자원 이름순이다 - 바이트 순(대문자가 소문자 앞)이 두 엔진에서 같은지도 본다.
		for _, r := range []string{"alpha", "Zeta"} {
			if _, err := s.AcquireResource(ctx, "p", r, Holder{JobID: "job-" + r}, at(0)); err != nil {
				t.Fatal(err)
			}
		}
		if hs, err := s.ListHeld(ctx, "p"); err != nil || holdNames(hs) != "Zeta,alpha,landing" || hs[0].JobID != "job-Zeta" {
			t.Fatalf("ListHeld: %v %v", holdNames(hs), err)
		}
		if h, err := s.HeldBy(ctx, "p", "landing"); err != nil || h.SessionID != a.ID || !h.AcquiredAt.Equal(at(0)) {
			t.Fatalf("HeldBy: %+v %v", h, err)
		}
		if err := s.ReleaseResource(ctx, "p", "landing", Holder{SessionID: b.ID}); !errors.As(err, &held) {
			t.Fatalf("남의 점유 반납: %v", err)
		}
		if err := s.ReleaseResource(ctx, "p", "landing", Holder{SessionID: a.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.HeldBy(ctx, "p", "landing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("반납 뒤 HeldBy: %v", err)
		}
		if err := s.ForceReleaseResource(ctx, "p", "alpha", "멈춘 잡"); err != nil {
			t.Fatal(err)
		}
		if err := s.ForceReleaseResource(ctx, "p", "alpha", "멈춘 잡"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("빈 자원 강제 반납: %v", err)
		}
		for want := int64(1); want <= 2; want++ {
			if v, err := s.NextCounter(ctx, "p", "n"); err != nil || v != want {
				t.Fatalf("NextCounter: %d %v (기대 %d)", v, err, want)
			}
		}
		if v, err := s.PeekCounter(ctx, "p", "n"); err != nil || v != 2 {
			t.Fatalf("PeekCounter: %d %v", v, err)
		}
		if v, err := s.PeekCounter(ctx, "p", "없음"); err != nil || v != 0 {
			t.Fatalf("없는 카운터: %d %v", v, err)
		}

		// ── 이벤트
		for _, e := range []struct {
			kind, project, session string
			payload                map[string]any
		}{
			{"item.finish", "p", a.ID, map[string]any{"item": "it", "mode": "done", "count": 2, TxOutcomeKey: TxCommitted}},
			{"item.add", "p", a.ID, map[string]any{TxOutcomeKey: TxCommitted}},
			{"item.finish", "p", b.ID, map[string]any{"item": "it", "mode": "dropped", "count": 5, TxOutcomeKey: TxRolledBack}},
			{"probe.other", "q", "", nil},
		} {
			if err := s.TryLogEvent(ctx, e.kind, e.project, e.session, e.payload); err != nil {
				t.Fatal(err)
			}
		}
		all, err := s.ListEvents(ctx, "", time.Time{}, 1000)
		if err != nil || len(all) < 4 || all[0].Kind != "probe.other" || all[0].Project != "q" || all[0].SessionID != "" {
			t.Fatalf("ListEvents(전 종류, 최신 먼저): %+v %v", all, err)
		}
		if n, err := s.CountEvents(ctx, "", time.Time{}); err != nil || n != len(all) {
			t.Fatalf("CountEvents(전 종류): %d %v (목록 %d)", n, err, len(all))
		}
		if n, err := s.CountEvents(ctx, "item.finish", time.Time{}); err != nil || n != 2 {
			t.Fatalf("CountEvents(종류): %d %v", n, err)
		}
		if fin, err := s.ListEvents(ctx, "item.finish", time.Time{}, 0); err != nil || len(fin) != 2 || fin[0].SessionID != b.ID || fin[0].ID <= fin[1].ID {
			t.Fatalf("ListEvents(종류): %+v %v", fin, err)
		}
		if n, err := s.CountEvents(ctx, "", time.Now().Add(time.Hour)); err != nil || n != 0 {
			t.Fatalf("CountEvents(앞으로의 시각부터): %d %v", n, err)
		}
		if evs, err := s.ListSessionEvents(ctx, a.ID, "item.add", time.Time{}); err != nil || len(evs) != 1 {
			t.Fatalf("ListSessionEvents(종류): %+v %v", evs, err)
		}
		if evs, err := s.ListSessionEvents(ctx, a.ID, "", time.Time{}); err != nil || len(evs) < 2 ||
			!slices.IsSortedFunc(evs, func(x, y model.Event) int { return int(x.ID - y.ID) }) {
			t.Fatalf("ListSessionEvents(오래된 먼저): %+v %v", evs, err)
		}
		if r, err := s.QueueReproduction(ctx, "p", 10); err != nil || r != (Reproduction{Finishes: 1, Followups: 2, Adds: 1}) {
			t.Fatalf("QueueReproduction(롤백 마무리는 안 센다): %+v %v", r, err)
		}
		decl, err := s.CloseDeclarationsByItem(ctx, "p")
		if d := decl["it"]; err != nil || len(decl) != 1 || d.Done != 1 || d.Dropped != 1 || d.LastMode != "dropped" || d.LastSession != b.ID {
			t.Fatalf("CloseDeclarationsByItem: %+v %v", decl, err)
		}

		// ── 멱등 기록
		blob := []byte{0x00, 0x01, 0x7f, 0x80, 0xff, 0x00}
		for _, r := range []IdemRecord{
			{Key: "k1", Fingerprint: "f0", Status: 200, ContentType: "text/plain", Body: []byte("옛"), At: at(0)},
			{Key: "k1", Fingerprint: "f1", Status: 201, ContentType: "application/octet-stream", Body: blob, At: at(1)},
			{Key: "k2", Fingerprint: "f2", Status: 204, ContentType: "text/plain", Body: []byte{}, At: at(2)},
		} {
			if err := s.PutIdemRecord(ctx, r, 0, 0); err != nil {
				t.Fatalf("PutIdemRecord(%s): %v", r.Key, err)
			}
		}
		if r, err := s.GetIdemRecord(ctx, "k1"); err != nil || r.Fingerprint != "f1" || r.Status != 201 ||
			r.ContentType != "application/octet-stream" || !bytes.Equal(r.Body, blob) || !r.At.Equal(at(1)) {
			t.Fatalf("GetIdemRecord(덮인 값 · 바이트 그대로): %+v %v", r, err)
		}
		if r, err := s.GetIdemRecord(ctx, "k2"); err != nil || len(r.Body) != 0 {
			t.Fatalf("빈 본문: %+v %v", r, err)
		}
		if _, err := s.GetIdemRecord(ctx, "없음"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("없는 멱등 기록: %v", err)
		}
		if n, err := s.CountIdemRecords(ctx); err != nil || n != 2 {
			t.Fatalf("CountIdemRecords: %d %v", n, err)
		}
	})
}

func judgmentIDs(js []model.Judgment) string {
	ids := make([]string, len(js))
	for i, j := range js {
		ids[i] = j.ID
	}
	return strings.Join(ids, ",")
}

func reflectLinks(ls []model.JudgmentLink, want ...string) bool {
	got := make([]string, len(ls))
	for i, l := range ls {
		got[i] = l.TargetKind + "/" + l.TargetID
	}
	return slices.Equal(got, want)
}

func holdNames(hs []model.ResourceHold) string {
	names := make([]string, len(hs))
	for i, h := range hs {
		names[i] = h.Resource
	}
	return strings.Join(names, ",")
}

// TestPutSnapshotKeepsOneRowPerKey 는 같은 키를 다시 쓰면 그 행을 덮고, 긴 키도 두 엔진에서 같은지 잰다.
//
// ★ Postgres 판의 기본키는 키 원문이 아니라 지문이다(snapshotKey). 충돌 대상을 (project, key) 로 두면 Postgres 는
// 「맞는 유니크 제약이 없다」(42P10)로 모든 쓰기를 거절했다. 긴 키(B-tree 항목 상한을 넘는 길이)는 지문을 둔 까닭이다.
func TestPutSnapshotKeepsOneRowPerKey(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		seed(t, s, "q")
		long := strings.Repeat("긴키", 3000) // 18KB
		for i, sn := range []model.Snapshot{
			{Project: "p", Key: long, Value: "1", Method: model.SnapshotCommand},
			{Project: "p", Key: long, Value: "2", Method: model.SnapshotCommand},
			{Project: "q", Key: long, Value: "q", Method: model.SnapshotCommand}, // 다른 프로젝트는 다른 행이다
			{Project: "p", Key: long + "!", Value: "3", Method: model.SnapshotCommand},
		} {
			if err := s.PutSnapshot(ctx, sn); err != nil {
				t.Fatalf("PutSnapshot[%d]: %v", i, err)
			}
		}
		sns, err := s.ListSnapshots(ctx, "p")
		if err != nil || len(sns) != 2 || sns[0].Key != long || sns[0].Value != "2" || sns[1].Value != "3" {
			t.Fatalf("키 하나에 행 하나여야 한다: %d행 %v", len(sns), err)
		}
		if sn, err := s.GetSnapshot(ctx, "q", long); err != nil || sn.Value != "q" {
			t.Fatalf("다른 프로젝트의 같은 키: %+v %v", sn.Value, err)
		}
	})
}

// TestIdemSweepKeepsTheNewest 는 초과분 청소가 최신 max 개(시각 내림차순, 같으면 키 내림차순)만 남기는지 잰다.
//
// ★ 앞 판은 `LIMIT -1 OFFSET ?`(SQLite 의 「상한 없음」)로 앞 max 개를 건너뛴 나머지를 지웠고, Postgres 는 음수
// LIMIT 을 거절해(2201W) 멱등 기록이 하나도 저장되지 않았다(청소가 저장과 한 트랜잭션이다).
func TestIdemSweepKeepsTheNewest(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		put := func(key string, m, max int, ttl time.Duration) {
			t.Helper()
			r := IdemRecord{Key: key, Fingerprint: "f", Status: 200, ContentType: "x", Body: []byte(key), At: t0.Add(time.Duration(m) * time.Minute)}
			if err := s.PutIdemRecord(ctx, r, ttl, max); err != nil {
				t.Fatalf("PutIdemRecord(%s): %v", key, err)
			}
		}
		keys := func() string {
			t.Helper()
			var out []string
			for _, k := range []string{"a", "b", "c", "d", "e", "v", "w", "x", "y", "z"} {
				if _, err := s.GetIdemRecord(ctx, k); err == nil {
					out = append(out, k)
				} else if !errors.Is(err, ErrNotFound) {
					t.Fatal(err)
				}
			}
			return strings.Join(out, ",")
		}
		for i, k := range []string{"a", "b", "c", "d", "e"} {
			put(k, i, 0, 0) // max 0 은 개수 제한이 없다
		}
		if got := keys(); got != "a,b,c,d,e" {
			t.Fatalf("상한 없이 다 남아야 한다: %s", got)
		}
		put("x", 10, 3, 0)
		if got := keys(); got != "d,e,x" {
			t.Fatalf("최신 셋만 남아야 한다: %s", got)
		}
		// 같은 시각이면 키가 큰 쪽이 남는다. 방금 넣은 v 도 그 규칙으로 걷힌다.
		put("y", 10, 3, 0)
		put("w", 10, 3, 0)
		put("v", 10, 3, 0)
		if got := keys(); got != "w,x,y" {
			t.Fatalf("같은 시각은 키 내림차순으로 남아야 한다: %s", got)
		}
		put("z", 20, 0, 5*time.Minute) // ttl 이 시각 축으로 걷는다
		if got := keys(); got != "z" {
			t.Fatalf("만료 청소: %s", got)
		}
	})
}

// TestNextCounterHandsOutEachNumberOnce 는 동시 발번이 두 엔진에서 1 부터 빠짐 · 겹침 없이 나오는지 잰다.
//
// ★ Postgres 는 upsert 의 DO UPDATE 안에서 맨 칸 이름 value 를 excluded 와 모호하다(42702)며 거절해 발번이 한 번도
// 안 됐다. 동시성 자체(전역 잠금)는 pg_tx_test.go 가 날 SQL 로 재고, 여기는 그 잠금 아래에서 이 문장이 도는지 본다.
func TestNextCounterHandsOutEachNumberOnce(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s, "p")
		const workers, each = 8, 25
		var mu sync.Mutex
		var got []int64
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range each {
					v, err := s.NextCounter(ctx, "p", "alloc")
					if err != nil {
						t.Errorf("NextCounter: %v", err)
						return
					}
					mu.Lock()
					got = append(got, v)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		slices.Sort(got)
		for i, v := range got {
			if v != int64(i+1) {
				t.Fatalf("발번이 1..%d 가 아니다(자리 %d 에 %d)", workers*each, i, v)
			}
		}
		if len(got) != workers*each {
			t.Fatalf("발번 %d건(기대 %d)", len(got), workers*each)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 판단 검색 - SQLite 판은 FTS5, Postgres 판은 pg_trgm 부분 일치
// ─────────────────────────────────────────────────────────────────────────────

// TestSearchTermsSplitsLikeUnicode61 은 조각 나누기가 unicode61 의 가름표 규칙(글자 · 숫자 · Co 가 아니면 가름표)인지 잰다.
func TestSearchTermsSplitsLikeUnicode61(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want []string
	}{
		{"fd-update", []string{"fd", "update"}},
		{"  fd   update ", []string{"fd", "update"}},
		{"sync_mode", []string{"sync", "mode"}},
		{`"unbalanced`, []string{"unbalanced"}},
		{"a%b_c\\d", []string{"a", "b", "c", "d"}},
		{"판단을, 남긴다.", []string{"판단을", "남긴다"}},
		{"v1.2·β３", []string{"v1", "2", "β３"}}, // 전각 숫자도 숫자다
		{"x\ue000y", []string{"x\ue000y"}},     // 사용자 정의 글자(Co)는 낱말 글자다
		{"e\u0301", []string{"e"}},             // 결합 기호(Mn)는 가름표다(unicode61 과 일부러 다른 자리)
		{"--- * ( )", nil},
		{"", nil},
	} {
		if got := SearchTerms(c.raw); !slices.Equal(got, c.want) {
			t.Errorf("SearchTerms(%q) = %q, 기대 %q", c.raw, got, c.want)
		}
	}
}

// TestLikeContainsMatchesLiterally 는 LIKE 무늬가 특수 글자를 글자 그대로 찾는지 Postgres 에서 직접 잰다.
func TestLikeContainsMatchesLiterally(t *testing.T) {
	if got := likeContains(`a%b_c\`); got != `%a\%b\_c\\%` {
		t.Fatalf("likeContains = %q", got)
	}
	s := newPGStore(t, lockTimeout)
	for _, c := range []struct {
		text, piece string
		want        bool
	}{
		{"50% 끝", "%", true}, {"50 끝", "%", false},
		{"a_b", "_", true}, {"ab", "_", false},
		{`c:\x`, `\`, true}, {"cx", `\`, false},
		{"ABC 판단을", "abc", true}, {"ÉCOLE", "école", true},
	} {
		var ok bool
		if err := s.db.QueryRow(`SELECT ? ILIKE ?`, c.text, likeContains(c.piece)).Scan(&ok); err != nil || ok != c.want {
			t.Errorf("%q ILIKE likeContains(%q) = %v(%v), 기대 %v", c.text, c.piece, ok, err, c.want)
		}
	}
}

// TestSearchJudgmentsPGCoversFTS 는 같은 판단을 두 엔진에 넣고 검색한 결과에서 Postgres 판이 SQLite 판의 상위집합인지 잰다
// (상한 없이 - 순위가 엔진마다 달라 잘린 목록끼리는 상위집합이 안 선다).
//
// ★ 이것이 가름표 나누기(SearchTerms)의 까닭이다. 공백 토큰을 통째로 ILIKE 에 걸면 `fd-update` 가 FTS5 는 맞히는
// `fd update` · `fd, update` 를 못 맞힌다.
func TestSearchJudgmentsPGCoversFTS(t *testing.T) {
	ctx := context.Background()
	lite, pg := newStore(t), newPGStore(t, lockTimeout)
	docs := []struct{ id, title, body string }{
		{"d1", "배포 절차", "fd-update 로 판을 올린다"},
		{"d2", "", "fd update 를 먼저 돌린다"},
		{"d3", "", "fd, update 순서다"},
		{"d4", "", "FD_Update 꼴로 적었다"},
		{"d5", "", "판단을 남긴다"},
		{"d6", "", "update 하고 fd 를 거꾸로"},
		{"d7", "sync_mode", "계약 개정"},
		{"d8", "", "Ωmega ÉCOLE"},
		{"d9", "", "café crème"},
	}
	for _, s := range []*Store{lite, pg} {
		seed(t, s, "p")
		for _, d := range docs {
			if _, err := s.AddJudgment(ctx, model.Judgment{ID: d.id, Project: "p", Kind: model.JudgmentDecision, Title: d.title, Body: d.body}); err != nil {
				t.Fatal(err)
			}
		}
	}
	search := func(s *Store, q string) []string {
		t.Helper()
		js, err := s.SearchJudgments(ctx, "", q, 1000)
		if err != nil {
			t.Fatalf("SearchJudgments(%q, %s): %v", q, s.db.eng, err)
		}
		ids := strings.Split(judgmentIDs(js), ",")
		if len(js) == 0 {
			ids = nil
		}
		slices.Sort(ids)
		return ids
	}
	for _, q := range []string{"fd-update", "fd update", "FD", "update", "판단", "판단을", "sync_mode", "ωmega", "école",
		`"unbalanced`, "-not", "*", "(", "OR OR", "a AND", "없는낱말", "---"} {
		l, p := search(lite, q), search(pg, q)
		for _, id := range l {
			if !slices.Contains(p, id) {
				t.Errorf("%q: SQLite 판이 맞힌 %s 를 Postgres 판이 못 맞혔다(SQLite %v, Postgres %v)", q, id, l, p)
			}
		}
	}
	// 상위집합이 빈 집합끼리 서는 것이 아님을 수로 본다.
	for _, c := range []struct {
		q        string
		lite, pg []string
		why      string
	}{
		{"fd-update", []string{"d1", "d2", "d3", "d4"}, []string{"d1", "d2", "d3", "d4", "d6"}, "구절은 순서를 보고 조각은 안 본다"},
		{"판단", nil, []string{"d5"}, "낱말 단위와 부분 일치"},
		{"ωmega", []string{"d8"}, []string{"d8"}, "한글 밖 대소문자 접기"},
		{"cafe", []string{"d9"}, nil, "받아들인 예외: 발음 구별 기호 접기는 FTS5 에만 있다"},
		{"---", nil, nil, "가름표만 친 검색은 맞을 판단이 없다(조각 없는 검색이 전부를 내면 안 된다)"},
	} {
		if l, p := search(lite, c.q), search(pg, c.q); !slices.Equal(l, c.lite) || !slices.Equal(p, c.pg) {
			t.Errorf("%q(%s): SQLite %v(기대 %v), Postgres %v(기대 %v)", c.q, c.why, l, c.lite, p, c.pg)
		}
	}
}

// TestTrgmSearchRanksBySimilarityThenNewest 는 Postgres 판의 순위를 잰다: 원문과 비슷한 판단이 먼저, 같으면 최신 먼저.
func TestTrgmSearchRanksBySimilarityThenNewest(t *testing.T) {
	ctx := context.Background()
	s := newPGStore(t, lockTimeout)
	seed(t, s, "p")
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// id 순서를 시각 순서와 거꾸로 둔다 - 최신 먼저가 id 로 우연히 서지 않게.
	for i, d := range []struct{ id, body string }{
		{"z-old", "레인 회수"},
		{"m-long", "레인 회수 뒤에 다른 낱말들이 길게 이어지는 판단 본문"},
		{"a-new", "레인 회수"},
	} {
		if _, err := s.AddJudgment(ctx, model.Judgment{ID: d.id, Project: "p", Kind: model.JudgmentDecision, Body: d.body,
			At: t0.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	js, err := s.SearchJudgments(ctx, "p", "레인 회수", 0)
	if err != nil || judgmentIDs(js) != "a-new,z-old,m-long" {
		t.Fatalf("순위: %s %v", judgmentIDs(js), err)
	}
	if js, err := s.SearchJudgments(ctx, "q", "레인 회수", 0); err != nil || len(js) != 0 {
		t.Fatalf("다른 프로젝트로 거르기: %s %v", judgmentIDs(js), err)
	}
}

// TestTrgmSearchUsesTheIndex 는 Postgres 판 검색이 기준선의 식 인덱스(judgment_text_trgm)를 탈 수 있는지 실행 계획으로 잰다.
//
// ★ 식이 인덱스와 한 글자라도 다르면 검색 결과는 같은데 매번 표 전체를 훑는다 - 결과 시험으로는 안 보인다. 순차 훑기를
// 끈 연결에서 계획에 인덱스 이름이 없으면 그 식으로는 인덱스를 못 탄다는 뜻이다. 프로젝트를 비워(전 프로젝트 검색) 거르는
// 조건을 본문 일치 하나로 둔다 - 프로젝트를 주면 빈 표에서는 플래너가 프로젝트 인덱스를 골라 이 잣대가 안 선다.
func TestTrgmSearchUsesTheIndex(t *testing.T) {
	ctx := context.Background()
	s := newPGStore(t, lockTimeout)
	sq := trgmSearch("", "판단을 남긴다", 10)
	q, err := bindDollar(sq.sql)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := c.QueryContext(ctx, `EXPLAIN `+q, sq.args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "judgment_text_trgm") {
		t.Fatalf("검색이 식 인덱스 judgment_text_trgm 을 못 탄다:\n%s", strings.Join(plan, "\n"))
	}
}
