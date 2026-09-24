package service

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kweiza/flightdeck/internal/judge"
	"github.com/kweiza/flightdeck/internal/model"
)

// 묶인 outside 는 **칸 하나**로 나오지만 원장에는 **경로마다** 발화로 남는다 —
// 그래야 억제(다음 턴에 다시 안 뜬다)·확인(ack)·축별 발화 수가 묶기 전과 같은 단위로 선다.
// 판단 하나가 그 전부를 확인한다(세션이 조각마다 같은 답을 다시 쓰지 않게 하는 것이 묶음의 목적).
func TestGroupedOutsideRecordsEveryPathAndIsAckedAtOnce(t *testing.T) {
	svc, st := newSvc(t)
	sess := openSessionForPrescribeTest(t, svc)
	claimItemForPrescribeTest(t, svc, st, sess, "batch7", []string{"a.go"})
	for _, p := range []string{"b.go", "c.go", "d.go"} { // 셋 다 선언 밖
		touchPathForPrescribeTest(t, st, sess, p)
	}

	r, err := svc.Prescriptions(ctx(), sess)
	if err != nil {
		t.Fatalf("처방 실패: %v", err)
	}
	var grouped []judge.Prescription
	for _, p := range r.Shown {
		if strings.HasPrefix(p.Key, judge.PrescribeOutside+":") {
			grouped = append(grouped, p)
		}
	}
	if len(grouped) != 1 || len(grouped[0].Keys()) != 3 {
		t.Fatalf("표시된 outside 가 한 칸에 세 경로가 아니다: %+v", r.Shown)
	}

	// 원장 — 경로마다 prescribe 한 행.
	evs, err := st.ListSessionEvents(ctx(), sess, "prescribe", time.Time{})
	if err != nil {
		t.Fatalf("이벤트 조회 실패: %v", err)
	}
	var got []string
	for _, e := range evs {
		var a struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(e.Payload), &a); err != nil {
			t.Fatalf("payload 해석 실패: %v", err)
		}
		if strings.HasPrefix(a.Key, judge.PrescribeOutside+":") {
			got = append(got, a.Key)
		}
	}
	sort.Strings(got)
	if want := []string{"outside:b.go", "outside:c.go", "outside:d.go"}; !slices.Equal(got, want) {
		t.Fatalf("원장의 outside 발화가 %v 다 — 기대 %v. 경로마다 남아야 억제·계측이 묶기 전과 같다", got, want)
	}

	// 다음 턴 — 같은 경로는 다시 안 뜬다.
	again, err := svc.Prescriptions(ctx(), sess)
	if err != nil {
		t.Fatalf("둘째 처방 실패: %v", err)
	}
	for _, p := range again.All {
		if strings.HasPrefix(p.Key, judge.PrescribeOutside+":") {
			t.Fatalf("이미 낸 outside 가 다음 턴에 다시 떴다: %+v", p)
		}
	}

	// 판단 하나가 세 키를 전부 확인한다.
	if _, err := svc.Note(ctx(), NoteInput{
		Project: "p", SessionID: sess, Kind: model.JudgmentDecision,
		Title: "범위가 늘었다", Body: "b·c·d 도 같은 작업이다",
	}); err != nil {
		t.Fatalf("note 실패: %v", err)
	}
	acks, err := st.ListSessionEvents(ctx(), sess, "prescribe_ack", time.Time{})
	if err != nil || len(acks) != 1 {
		t.Fatalf("ack 이 1건이 아니다: %d (%v)", len(acks), err)
	}
	for _, k := range []string{"outside:b.go", "outside:c.go", "outside:d.go"} {
		if !strings.Contains(acks[0].Payload, k) {
			t.Errorf("ack 에 %s 가 없다 — 묶인 키가 확인에서 빠졌다: %s", k, acks[0].Payload)
		}
	}
}
