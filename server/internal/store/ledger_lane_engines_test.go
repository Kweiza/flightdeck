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

// TestIdemSweepSurvivesNullKey 는 NULL 키 행이 있어도 초과분 청소가 돌고, 그 행도 같이 걷히는지 잰다.
//
// ★ SQLite 의 `key TEXT PRIMARY KEY` 는 NULL 을 받는다(Postgres 는 거절한다). 남길 목록에 NULL 이 들면 `NOT IN` 이
// 모든 행에서 NULL 이 되어 청소가 조용히 0건이었다. NULL 행을 가장 새 시각에 두어야 남길 목록에 든다 - 그래야 이
// 시험이 그 결함에 닿는다.
func TestIdemSweepSurvivesNullKey(t *testing.T) {
	bothEngines(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		put := func(key string, m, max int) {
			t.Helper()
			r := IdemRecord{Key: key, Fingerprint: "f", Status: 200, ContentType: "x", Body: []byte(key), At: t0.Add(time.Duration(m) * time.Minute)}
			if err := s.PutIdemRecord(ctx, r, 0, max); err != nil {
				t.Fatalf("PutIdemRecord(%s): %v", key, err)
			}
		}
		for i, k := range []string{"a", "b", "c"} {
			put(k, i, 0) // 아직 청소하지 않는다
		}
		_, err := s.db.ExecContext(ctx, `INSERT INTO idempotency(key, fingerprint, status, ctype, body, at) VALUES (NULL, 'f', 200, 'x', ?, ?)`,
			[]byte("n"), fmtTime(t0.Add(time.Hour)))
		if s.Postgres() {
			if err == nil {
				t.Fatal("Postgres 의 기본키가 NULL 을 받았다")
			}
			return
		}
		if err != nil {
			t.Fatalf("전제: SQLite 의 TEXT PRIMARY KEY 는 NULL 을 받는다: %v", err)
		}
		put("d", 10, 2)
		var nulls int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM idempotency WHERE key IS NULL`).Scan(&nulls); err != nil {
			t.Fatal(err)
		}
		n, err := s.CountIdemRecords(ctx)
		if err != nil || nulls != 0 || n != 2 {
			t.Fatalf("청소 뒤 %d행(NULL 키 %d행), 기대 c · d 두 행: %v", n, nulls, err)
		}
		for _, k := range []string{"c", "d"} {
			if _, err := s.GetIdemRecord(ctx, k); err != nil {
				t.Fatalf("최신 %s 가 남아야 한다: %v", k, err)
			}
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

// TestSearchTermsSplitsLikeUnicode61 은 조각 나누기의 대표 사례를 범주별로 고정한다. 판정 하나하나는 SQLite 에 물어
// 얻은 값이다(코드포인트 전부의 대조는 unicode61_test.go). 「지금 범주」와 「unicode61 의 판정」이 갈리는 글자를 일부러
// 넣었다 - unicode61 의 표는 Unicode 6.1 에 멈춰 있고, 그 뒤에 생긴 글자는 미지정이라 낱말 글자다.
func TestSearchTermsSplitsLikeUnicode61(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want []string
		why  string
	}{
		{"fd-update", []string{"fd", "update"}, "Pd 가름표"},
		{"  fd   update ", []string{"fd", "update"}, "Zs 가름표"},
		{"sync_mode", []string{"sync", "mode"}, "Pc 가름표"},
		{`"unbalanced`, []string{"unbalanced"}, "Po 가름표"},
		{"a%b_c\\d", []string{"a", "b", "c", "d"}, "LIKE 특수 글자는 다 가름표"},
		{"판단을, 남긴다.", []string{"판단을", "남긴다"}, "Lo 낱말"},
		{"v1.2·β３", []string{"v1", "2", "β３"}, "Nd 낱말(전각 숫자도)"},
		{"x\ue000y", []string{"x\ue000y"}, "Co 낱말"},
		{"🦊", []string{"🦊"}, "So 인데 낱말: Unicode 9.0 글자라 6.1 표에서 미지정"},
		{"a©b", []string{"a", "b"}, "So 가름표: 6.1 표에 있는 기호"},
		{"₺100", []string{"₺100"}, "Sc 인데 낱말: U+20BA 는 6.2 글자"},
		{"$100+1", []string{"100", "1"}, "Sc · Sm 가름표"},
		{"x\u0378y", []string{"x\u0378y"}, "Cn(지금도 미지정)은 낱말"},
		{"x\ufffey", []string{"x", "y"}, "비문자 U+FFFE 는 가름표"},
		{"x\u19b0y", []string{"x", "y"}, "Lo 인데 가름표: 6.1 에서는 Mc 였다"},
		{"x\u0305y", []string{"x", "y"}, "Mn 가름표"},
		{"e\u0301", []string{"e"}, "unicode61 이 떼어 내는 결합 기호는 가름표로 둔다(SearchTerms 의 ★)"},
		{"x\u07fdy", []string{"x\u07fdy"}, "Mn 인데 낱말: 6.1 이후 글자"},
		{"x\u00ady", []string{"x", "y"}, "Cf(소프트 하이픈) 가름표"},
		{"x\u0605y", []string{"x\u0605y"}, "Cf 인데 낱말: 6.1 이후 글자"},
		{"x\u2e40y", []string{"x\u2e40y"}, "Pd 인데 낱말: 6.1 이후 글자"},
		{"x\ufffdy", []string{"x", "y"}, "U+FFFD 는 가름표"},
		{"x\x00y", []string{"x", "y"}, "NUL 은 가름표"},
		{"x\xffy\xc3", []string{"x", "y"}, "깨진 UTF-8 바이트는 가름표(조각이 Postgres 에서 22021 이 되지 않게)"},
		{"--- * ( )", nil, "가름표뿐"},
		{"", nil, "빈 글"},
	} {
		if got := SearchTerms(c.raw); !slices.Equal(got, c.want) {
			t.Errorf("SearchTerms(%q) = %q, 기대 %q(%s)", c.raw, got, c.want, c.why)
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
// `fd update` · `fd, update` 를 못 맞힌다. 받아들인 예외(글자 접기 둘)는 상위집합 고리에 넣지 않고 아래 표에
// 「SQLite 만 맞힌다」로 고정한다. 조각 하나만 품은 판단(d10 · d11)은 조각을 AND 로 거는지 본다.
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
		{"d10", "", "fd 하나만 적었다"},
		{"d11", "", "update 하나만 적었다"},
		{"d12", "", "🦊"},
		{"d13", "", "λογος"},
		{"d14", "", "ſun"},
		{"d15", "", "ª 서수 표시"},
		{"f1", "", "ẛqz1"}, {"f2", "", "µqz2"}, {"f3", "", "ϐqz3"}, {"f4", "", "ϵqz4"}, {"f5", "", "ϑqz5"},
		{"f6", "", "ϰqz6"}, {"f7", "", "ϖqz7"}, {"f8", "", "ϱqz8"}, {"f9", "", "ϕqz9"},
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
			t.Fatalf("SearchJudgments(%q, %s): %v", clip(q, 40), s.db.eng, err)
		}
		ids := strings.Split(judgmentIDs(js), ",")
		if len(js) == 0 {
			ids = nil
		}
		slices.Sort(ids)
		return ids
	}
	for _, q := range []string{"fd-update", "fd update", "FD", "update", "판단", "판단을", "sync_mode", "ωmega", "école",
		`"unbalanced`, "-not", "*", "(", "OR OR", "a AND", "없는낱말", "---", "🦊", "판단을\x00남긴다\xff",
		"ª", "\xaa", "\x00\xaa", "ª\xaa서수"} {
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
		{"fd update", []string{"d1", "d2", "d3", "d4", "d6"}, []string{"d1", "d2", "d3", "d4", "d6"}, "조각은 AND 다(d10 · d11 은 하나만 품는다)"},
		{"🦊", []string{"d12"}, []string{"d12"}, "6.1 이후 이모지도 두 판에서 낱말이다"},
		{"판단을\x00남긴다\xff", []string{"d5"}, []string{"d5"}, "NUL · 깨진 바이트가 든 검색도 두 판에서 돈다"},
		{"ª", []string{"d15"}, []string{"d15"}, "대조: 온전한 ª(U+00AA) 는 두 판에서 낱말이다"},
		{"\xaa", nil, nil, "깨진 바이트 0xAA 는 두 판에서 가름표다(SQLite 판이 U+00AA 로 읽어 ª 를 찾으면 안 된다)"},
		{"\x00\xaa", nil, nil, "NUL 뒤의 깨진 바이트도 두 판에서 가름표다"},
		{"ª\xaa서수", []string{"d15"}, []string{"d15"}, "깨진 바이트가 구절 가운데에 있어도 두 판이 같은 낱말을 본다"},
		{"판단", nil, []string{"d5"}, "낱말 단위와 부분 일치"},
		{"ωmega", []string{"d8"}, []string{"d8"}, "한글 밖 대소문자 접기"},
		{"cafe", []string{"d9"}, nil, "받아들인 예외: 발음 구별 기호 접기는 FTS5 에만 있다"},
		{"ΛΟΓΟΣ", []string{"d13"}, nil, "받아들인 예외: 대소문자 특수 접기(ς→σ)는 FTS5 에만 있다"},
		{"sun", []string{"d14"}, nil, "받아들인 예외: 대소문자 특수 접기(ſ→s)는 FTS5 에만 있다"},
		{"sqz1", []string{"f1"}, nil, "받아들인 예외: 대소문자 특수 접기(ẛ→s)"},
		{"μqz2", []string{"f2"}, nil, "받아들인 예외: 대소문자 특수 접기(µ→μ)"},
		{"βqz3", []string{"f3"}, nil, "받아들인 예외: 대소문자 특수 접기(ϐ→β)"},
		{"εqz4", []string{"f4"}, nil, "받아들인 예외: 대소문자 특수 접기(ϵ→ε)"},
		{"θqz5", []string{"f5"}, nil, "받아들인 예외: 대소문자 특수 접기(ϑ→θ)"},
		{"κqz6", []string{"f6"}, nil, "받아들인 예외: 대소문자 특수 접기(ϰ→κ)"},
		{"πqz7", []string{"f7"}, nil, "받아들인 예외: 대소문자 특수 접기(ϖ→π)"},
		{"ρqz8", []string{"f8"}, nil, "받아들인 예외: 대소문자 특수 접기(ϱ→ρ)"},
		{"φqz9", []string{"f9"}, nil, "받아들인 예외: 대소문자 특수 접기(ϕ→φ)"},
		{"---", nil, nil, "가름표만 친 검색은 맞을 판단이 없다(조각 없는 검색이 전부를 내면 안 된다)"},
	} {
		if l, p := search(lite, c.q), search(pg, c.q); !slices.Equal(l, c.lite) || !slices.Equal(p, c.pg) {
			t.Errorf("%q(%s): SQLite %v(기대 %v), Postgres %v(기대 %v)", clip(c.q, 40), c.why, l, c.lite, p, c.pg)
		}
	}
}

// TestSearchJudgmentsHasNoTermLimit 는 검색어의 조각 수에 상한이 없는지 두 엔진에서 잰다. 앞 판은 조각 33개부터
// 두 엔진 모두 입력 오류로 거절해, 예전 SQLite 판이 찾던 검색(`w0` 부터 `w32` 까지)이 깨졌다.
//
// 33개 · 100개 검색이 두 엔진에서 같은 판단을 찾는지 본다(빈칸이 없어도 조각으로 센다). Postgres 판은 앞 32개 조각만
// 인덱스 조건이고 나머지는 배열 하나로 넘긴 행 거르기라(trgmSearch 의 ★), 빠진 조각이 그 거르기 쪽에 있는 판단(head ·
// most)이 안 맞는지(AND), 대문자로 적은 판단(upper)이 맞는지(대소문자 무시) 본다. 낱말 길이를 같게 둬 부분 일치가
// 다른 낱말 안에서 맞는 일이 없게 한다(w005 는 w050 안에 없다).
func TestSearchJudgmentsHasNoTermLimit(t *testing.T) {
	ctx := context.Background()
	words := make([]string, 100)
	for i := range words {
		words[i] = fmt.Sprintf("w%03d", i)
	}
	without := func(i int) []string { return slices.Delete(slices.Clone(words), i, i+1) }
	upper := slices.Clone(words)
	upper[90] = "W090"
	lite, pg := newStore(t), newPGStore(t, lockTimeout)
	for _, s := range []*Store{lite, pg} {
		seed(t, s, "p")
		for _, d := range []struct {
			id    string
			words []string
		}{
			{"all", words},
			{"head", words[:32]},  // w032 부터 없다
			{"most", without(90)}, // w090 만 없다
			{"upper", upper},
		} {
			if _, err := s.AddJudgment(ctx, model.Judgment{ID: d.id, Project: "p", Kind: model.JudgmentDecision,
				Body: strings.Join(d.words, " ")}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct {
		q, want string
	}{
		{strings.Join(words[:32], " "), "all,head,most,upper"},
		{strings.Join(words[:33], " "), "all,most,upper"},
		{strings.Join(words[:33], "-"), "all,most,upper"},
		{strings.Join(words, " "), "all,upper"},
		// 검색어만 대문자(W090, 배열 거르기 쪽): 무늬도 소문자로 접어야 소문자 본문(all)이 맞는다.
		{strings.Join(upper, " "), "all,upper"},
		{strings.Join(words, " ") + " 없는낱말", ""},
	} {
		for _, s := range []*Store{lite, pg} {
			js, err := s.SearchJudgments(ctx, "", c.q, 0)
			ids := strings.Split(judgmentIDs(js), ",")
			slices.Sort(ids)
			if got := strings.Join(ids, ","); err != nil || got != c.want {
				t.Errorf("조각 %d개 검색(%s): %q %v, 기대 %q", len(SearchTerms(c.q)), s.db.eng, got, err, c.want)
			}
		}
	}
}

// TestTrgmSearchBindCountIsBounded 는 Postgres 판 검색의 바인드 수가 조각 수와 상관없이 indexedTerms + 5 개를 넘지
// 않는지 잰다. 조각마다 바인드를 붙이면 조각이 수만 개인 검색어가 문장 하나의 바인드 한도(65,535)를 넘어 질의를 못
// 짓는다. 한도를 넘는 조각 수로 Postgres 에서 실제로 돌려, 드라이버가 []string 을 text[] 로 넘기는지와 질의가 메모리를
// 조각 수의 제곱으로 쓰지 않는지(그 꼴은 이 조각 수에서 Postgres 백엔드가 메모리 부족으로 죽는다)까지 본다. 조각은
// 모두 같은 낱말이다 - 서로 다른 낱말 7만 개를 품은 본문은 부분 일치에 행마다 수십 초가 걸린다. SQLite 판은 돌리지
// 않는다(FTS5 가 구절 7만 개에 십여 초를 쓴다). 두 엔진의 같은 결과는 TestSearchJudgmentsHasNoTermLimit 가 잰다.
func TestTrgmSearchBindCountIsBounded(t *testing.T) {
	ctx := context.Background()
	// 질의 짓기는 상한과 무관하게 조각 수만 개에서도 바인드가 묶이는지 본다(순수).
	const n = 70000
	many := make([]string, n)
	for i := range many {
		many[i] = fmt.Sprintf("t%05x", i)
	}
	sq := trgmSearch("", strings.Join(many, " "), 10)
	if pats, ok := sq.args[2+indexedTerms].([]string); len(sq.args) != indexedTerms+5 || !ok || len(pats) != n-indexedTerms {
		t.Fatalf("바인드 %d개(기대 %d개), 무늬 배열 %T", len(sq.args), indexedTerms+5, sq.args[2+indexedTerms])
	}
	// 같은 조각의 되풀이는 일치 조건에서 한 번으로 준다. 긴 낱말 하나가 인덱스 조건 자리를 다 차지하지 않는다.
	rep := strings.TrimSpace(strings.Repeat("longword ", 40)) + " ab"
	if sq := trgmSearch("", rep, 10); len(sq.args) != 2+2+2 {
		t.Fatalf("되풀이 조각 검색의 바인드 %d개(기대 6개: project 둘 · 조각 둘 · 유사도 · limit): %v", len(sq.args), sq.args)
	}
	// 실제 Postgres 에서는 원문 바이트 상한(MaxSearchQueryBytes) 안에서 조각을 가장 많이 넣은 검색이 돈다.
	var parts []string
	for i := 0; len(strings.Join(parts, " "))+6 <= MaxSearchQueryBytes; i++ {
		parts = append(parts, fmt.Sprintf("t%04x", i))
	}
	raw := strings.Join(parts, " ")
	if len(parts) <= indexedTerms {
		t.Fatalf("조각 %d개라 배열 거르기에 닿지 않는다", len(parts))
	}
	s := newPGStore(t, lockTimeout)
	seed(t, s, "p")
	for _, d := range []struct{ id, body string }{{"hit", raw}, {"miss", strings.Join(parts[:len(parts)-1], " ")}} {
		if _, err := s.AddJudgment(ctx, model.Judgment{ID: d.id, Project: "p", Kind: model.JudgmentDecision, Body: d.body}); err != nil {
			t.Fatal(err)
		}
	}
	if js, err := s.SearchJudgments(ctx, "", raw, 0); err != nil || judgmentIDs(js) != "hit" {
		t.Fatalf("조각 %d개(%d 바이트) 검색: %q %v", len(parts), len(raw), judgmentIDs(js), err)
	}
	// 상한을 넘으면 두 엔진 모두 질의 전에 입력 오류다.
	var tooLong *SearchQueryTooLongError
	for _, st := range []*Store{newStore(t), s} {
		if _, err := st.SearchJudgments(ctx, "", strings.Repeat("a", MaxSearchQueryBytes+1), 0); !errors.As(err, &tooLong) {
			t.Fatalf("상한을 넘는 검색어: %v(SearchQueryTooLongError 를 기대)", err)
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

// TestTrgmSearchRanksByRawQuery 는 Postgres 판의 순위가 조각을 이은 글이 아니라 원문과의 유사도인지 잰다. `xⓐy` 의
// 조각은 x · y 라 둘 다 맞지만, 원문과 똑같은 판단이 먼저다. 조각을 빈칸으로 이어 유사도에 넘기면 `x y` 판단이 먼저
// 온다. 똑같은 판단을 더 오래된 쪽에 둬 최신 먼저로 우연히 서지 않게 한다. NUL · 깨진 바이트가 든 원문도 순위를 짓는다
// (그대로 넘기면 Postgres 가 22021 로 질의를 거절한다).
func TestTrgmSearchRanksByRawQuery(t *testing.T) {
	ctx := context.Background()
	s := newPGStore(t, lockTimeout)
	seed(t, s, "p")
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for i, d := range []struct{ id, body string }{
		{"exact", "xⓐy"},
		{"split", "x y"},
	} {
		if _, err := s.AddJudgment(ctx, model.Judgment{ID: d.id, Project: "p", Kind: model.JudgmentDecision, Body: d.body,
			At: t0.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"xⓐy", "xⓐy\x00", "xⓐy\xff"} {
		if js, err := s.SearchJudgments(ctx, "p", q, 0); err != nil || judgmentIDs(js) != "exact,split" {
			t.Errorf("%q 순위: %s %v(기대 exact,split)", q, judgmentIDs(js), err)
		}
	}
}

// TestTrgmSearchUsesTheIndex 는 Postgres 판 검색이 기준선의 식 인덱스(judgment_text_trgm)를 탈 수 있는지 실행 계획으로 잰다.
//
// ★ 식이 인덱스와 한 글자라도 다르면 검색 결과는 같은데 매번 표 전체를 훑는다 - 결과 시험으로는 안 보인다. 순차 훑기를
// 끈 연결에서 계획에 인덱스 이름이 없으면 그 식으로는 인덱스를 못 탄다는 뜻이다. 프로젝트를 비워(전 프로젝트 검색) 거르는
// 조건을 본문 일치 하나로 둔다 - 프로젝트를 주면 빈 표에서는 플래너가 프로젝트 인덱스를 골라 이 잣대가 안 선다.
//
// ★ 조각이 인덱스 조건이 되는 수도 센다(trgmSearch 의 ★). 조각은 indexedTerms 를 넘게 두고, 짧은 조각(trigram 이
// 없다)을 앞에 둔다: 긴 조각 indexedTerms 개가 모두 인덱스 조건의 ILIKE(`~~*`)이고, 짧은 조각은 행 거르기의 LIKE ALL
// 이고, 행 거르기에 ILIKE 가 없어야 한다. `ILIKE ALL` 꼴은 인덱스를 아예 못 타고, 조각 일치가 행 거르기로 밀리면 인덱스로
// 좁히지 못한다.
func TestTrgmSearchUsesTheIndex(t *testing.T) {
	ctx := context.Background()
	s := newPGStore(t, lockTimeout)
	var short, long []string
	for i := range 8 {
		short = append(short, fmt.Sprintf("x%d", i))
	}
	for i := range indexedTerms {
		long = append(long, fmt.Sprintf("판단%02d", i))
	}
	sq := trgmSearch("", strings.Join(append(slices.Clone(short), long...), " "), 10)
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
	all := strings.Join(plan, "\n")
	if !strings.Contains(all, "judgment_text_trgm") {
		t.Fatalf("검색이 식 인덱스 judgment_text_trgm 을 못 탄다:\n%s", all)
	}
	var cond, filter string
	for _, line := range plan {
		switch l := strings.TrimSpace(line); {
		case strings.HasPrefix(l, "Index Cond:"):
			cond += l
		case strings.HasPrefix(l, "Filter:"):
			filter += l
		}
	}
	if n := strings.Count(cond, "~~*"); n != indexedTerms || strings.Contains(filter, "~~*") {
		t.Fatalf("인덱스 조건의 조각 일치 %d개(기대 %d개), 행 거르기의 ILIKE %v:\n%s",
			n, indexedTerms, strings.Contains(filter, "~~*"), all)
	}
	for _, w := range long {
		if !strings.Contains(cond, likeContains(w)) {
			t.Fatalf("긴 조각 %s 가 인덱스 조건에 없다:\n%s", w, all)
		}
	}
	if !strings.Contains(filter, "~~ ALL") {
		t.Fatalf("인덱스 조건에 못 든 짧은 조각이 행 거르기(LIKE ALL)에 없다:\n%s", all)
	}
}
