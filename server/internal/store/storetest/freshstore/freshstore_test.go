package freshstore

import (
	"context"
	"testing"

	"github.com/kweiza/flightdeck/internal/model"
)

// 두 엔진 모두 쓸 수 있는 빈 원장을 열고, 부를 때마다 서로 다른 원장이다.
func TestOpenGivesFreshWritableLedger(t *testing.T) {
	for _, e := range []Engine{SQLite, Postgres} {
		t.Run(string(e), func(t *testing.T) {
			ctx := context.Background()
			a, _ := OpenOn(t, e, nil)
			b, _ := OpenOn(t, e, nil)
			if err := a.UpsertProject(ctx, model.Project{ID: "p", Path: "/repo/p"}); err != nil {
				t.Fatalf("새 원장에 못 쓴다: %v", err)
			}
			got, err := b.ListProjects(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("다른 OpenOn 의 원장에 쓴 프로젝트가 보인다: %v", got)
			}
		})
	}
}
