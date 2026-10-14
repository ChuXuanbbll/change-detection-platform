package queue

import (
	"context"
	"testing"
)

func TestFullQueueDoesNotPersist(t *testing.T) {
	q := New(1)
	defer q.Stop(context.Background())
	n := 0
	save := func() error { n++; return nil }
	if ok, _ := q.Submit("a", save); !ok {
		t.Fatal("first rejected")
	}
	if ok, _ := q.Submit("b", save); ok || n != 1 {
		t.Fatalf("accepted=%v persisted=%d", ok, n)
	}
}
func TestClosedQueueRejects(t *testing.T) {
	q := New(1)
	q.Stop(context.Background())
	q.Stop(context.Background())
	if q.TryEnqueue("a") {
		t.Fatal("closed queue accepted")
	}
}
