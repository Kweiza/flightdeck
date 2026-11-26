package freshstore

import (
	"context"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
)

// Open 은 쓸 수 있는 빈 원장을 열고, 부를 때마다 서로 다른 원장이다.
func TestOpenGivesFreshWritableLedger(t *testing.T) {
	ctx := context.Background()
	a, b := Open(t), Open(t)
	if err := a.UpsertProject(ctx, model.Project{ID: "p", Path: "/repo/p"}); err != nil {
		t.Fatalf("새 원장에 못 쓴다: %v", err)
	}
	got, err := b.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("다른 Open 의 원장에 쓴 프로젝트가 보인다: %v", got)
	}
}
