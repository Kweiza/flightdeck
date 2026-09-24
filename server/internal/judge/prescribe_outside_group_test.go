package judge

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// 한 턴의 outside 는 **처방 하나로 묶인다** — 칸 하나, 문구 하나, 키는 경로마다.
//
// ★ 이 시험이 없어서 난 일(2026-09-24 실측): 선점 경로 밖 편집이 경로마다 처방 한 줄씩 나와
// 한 턴에 52개까지 접혔다. 상한이 3이라 같은 지시(「범위가 왜 늘었는지 남겨라」)가 경로만
// 바꿔 약 18턴을 이어 갔고, 세션은 둘째 이후 outside 턴에도 9번 중 8번 판단을 다시 썼다 —
// 같은 원인(범위가 넓어졌다)에 대한 같은 답을 조각마다 되풀이한 것이다.
// 묶어도 잃는 것이 없게 한다: 억제·확인(ack)·축별 발화 수는 **키 단위**로 그대로다(Keys()).
func outsideOnly(ps []Prescription) []Prescription {
	var out []Prescription
	for _, p := range ps {
		if strings.HasPrefix(p.Key, PrescribeOutside+":") {
			out = append(out, p)
		}
	}
	return out
}

func outsideInput(paths ...string) PrescribeInput {
	return PrescribeInput{
		Now: pt0, SessionID: "me",
		Claims:       []ClaimView{{ItemID: "fd-x", Paths: []string{"internal/judge"}}},
		TurnPaths:    append([]string{"internal/judge/prescribe.go"}, paths...),
		LastJudgment: pt0, NewPaths: len(paths) + 1,
	}
}

func TestOutsideGroupsTheTurnIntoOnePrescription(t *testing.T) {
	ps := outsideOnly(Prescribe(outsideInput("cmd/fd/hook.go", "docs/x.md", "bin/fd")))
	if len(ps) != 1 {
		t.Fatalf("outside 가 %d개로 나왔다 — 한 턴의 outside 는 한 처방이어야 한다: %v", len(ps), keys(ps))
	}
	p := ps[0]
	if p.Key != "outside:cmd/fd/hook.go" {
		t.Errorf("대표 키가 %q 다 — 턴의 첫 경로여야 한다", p.Key)
	}
	wantAll := []string{"outside:cmd/fd/hook.go", "outside:docs/x.md", "outside:bin/fd"}
	if got := p.Keys(); !slices.Equal(got, wantAll) {
		t.Errorf("Keys() = %v, 기대 %v — 억제·확인·계측은 경로마다 따로 남아야 한다", got, wantAll)
	}
	for _, path := range []string{"cmd/fd/hook.go", "docs/x.md", "bin/fd"} {
		if !strings.Contains(p.Text, path) {
			t.Errorf("문구에 %q 가 없다 — 묶었으면 무엇을 묶었는지 보여야 한다:\n%s", path, p.Text)
		}
	}
	if !strings.Contains(p.Reason, "3") {
		t.Errorf("사유에 경로 수(3)가 없다: %s", p.Reason)
	}
}

// 경로가 하나면 지금과 같다 — 묶음은 둘 이상일 때만의 모양이다.
func TestOutsideSinglePathStaysAsBefore(t *testing.T) {
	ps := outsideOnly(Prescribe(outsideInput("cmd/fd/hook.go")))
	if len(ps) != 1 {
		t.Fatalf("outside 가 %d개다: %v", len(ps), keys(ps))
	}
	if len(ps[0].Also) != 0 {
		t.Errorf("경로가 하나인데 Also 가 있다: %v", ps[0].Also)
	}
	if want := fmt.Sprintf(syntaxFor("").AddWithPath, "cmd/fd/hook.go"); !strings.Contains(ps[0].Text, want) {
		t.Errorf("단일 경로 문구가 바뀌었다 — %q 가 없다:\n%s", want, ps[0].Text)
	}
}

// 이미 발화한 경로는 묶음에서 빠진다 — 경로마다 한 번이라는 억제 규칙은 그대로다.
func TestOutsideGroupSkipsAlreadyEmittedPaths(t *testing.T) {
	in := outsideInput("cmd/fd/hook.go", "docs/x.md", "bin/fd")
	in.Emitted = map[string]time.Time{"outside:docs/x.md": pt0.Add(-time.Minute)}
	ps := outsideOnly(Prescribe(in))
	if len(ps) != 1 {
		t.Fatalf("outside 가 %d개다: %v", len(ps), keys(ps))
	}
	if got, want := ps[0].Keys(), []string{"outside:cmd/fd/hook.go", "outside:bin/fd"}; !slices.Equal(got, want) {
		t.Errorf("Keys() = %v, 기대 %v — 발화한 경로가 다시 묶였다", got, want)
	}
	if strings.Contains(ps[0].Text, "docs/x.md") {
		t.Errorf("이미 발화한 경로가 문구에 다시 나왔다:\n%s", ps[0].Text)
	}
}

// 많으면 목록은 줄이되 수와 키는 전부 싣는다.
func TestOutsideGroupClipsTheListButKeepsEveryKey(t *testing.T) {
	var paths []string
	for i := range 8 {
		paths = append(paths, fmt.Sprintf("data/f%d.json", i))
	}
	ps := outsideOnly(Prescribe(outsideInput(paths...)))
	if len(ps) != 1 {
		t.Fatalf("outside 가 %d개다", len(ps))
	}
	if n := len(ps[0].Keys()); n != 8 {
		t.Errorf("키가 %d개다 — 8개 전부 발화로 남아야 다음 턴에 다시 안 뜬다", n)
	}
	if !strings.Contains(ps[0].Reason, "8") {
		t.Errorf("사유에 경로 수(8)가 없다: %s", ps[0].Reason)
	}
	if !strings.Contains(ps[0].Text, "외 3개") {
		t.Errorf("목록을 줄였다는 표시(외 3개)가 없다:\n%s", ps[0].Text)
	}
	if strings.Contains(ps[0].Text, "data/f7.json") {
		t.Errorf("줄인 목록에 여섯째 이후가 나왔다:\n%s", ps[0].Text)
	}
}

// 묶음은 **칸 하나**다 — 52 경로 폭주가 뒤 축을 접어 내지 않는다.
func TestOutsideGroupTakesOneSlot(t *testing.T) {
	var paths []string
	for i := range 52 {
		paths = append(paths, fmt.Sprintf("scripts/sim/f%02d.gd", i))
	}
	in := outsideInput(paths...)
	in.NewPaths = SilentNewPaths + 1 // silent 도 같이 뜨게 한다
	shown, folded := FoldPrescriptions(Prescribe(in))
	if folded != 0 {
		t.Fatalf("52 경로 턴이 %d개를 접었다 — 묶음은 한 칸이어야 한다: %v", folded, keys(shown))
	}
	if n := len(outsideOnly(shown)); n != 1 {
		t.Errorf("표시된 outside 가 %d개다", n)
	}
}
