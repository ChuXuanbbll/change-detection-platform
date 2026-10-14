package queue

import (
	"context"
	"fmt"
	"sync"
)

// Queue atomically admits persisted jobs and permits idle reconfiguration.
type Queue struct {
	mu                        sync.Mutex
	cond                      *sync.Cond
	pending                   []string
	capacity, workers, active int
	closed                    bool
	wg                        sync.WaitGroup
}

func New(buffer int) *Queue {
	if buffer <= 0 {
		buffer = 64
	}
	q := &Queue{capacity: buffer, workers: 1}
	q.cond = sync.NewCond(&q.mu)
	return q
}
func (q *Queue) Submit(id string, persist func() error) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || len(q.pending) >= q.capacity {
		return false, nil
	}
	if err := persist(); err != nil {
		return false, err
	}
	q.pending = append(q.pending, id)
	q.cond.Broadcast()
	return true, nil
}
func (q *Queue) TryEnqueue(id string) bool {
	ok, _ := q.Submit(id, func() error { return nil })
	return ok
}
func (q *Queue) Enqueue(id string) { q.TryEnqueue(id) }
func (q *Queue) Configure(workers, capacity int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if workers < 1 || workers > 8 || capacity < 1 || capacity > 1024 {
		return fmt.Errorf("invalid queue limits")
	}
	if (q.active > 0 || len(q.pending) > 0) && (workers != q.workers || capacity != q.capacity) {
		return fmt.Errorf("任务队列运行中，请结束后修改并发和容量")
	}
	q.workers, q.capacity = workers, capacity
	q.cond.Broadcast()
	return nil
}
func (q *Queue) Start(workers int, handler func(string)) {
	if workers > 0 {
		q.workers = workers
	}
	for i := 0; i < 8; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for {
				q.mu.Lock()
				for !q.closed && (len(q.pending) == 0 || q.active >= q.workers) {
					q.cond.Wait()
				}
				if q.closed {
					q.mu.Unlock()
					return
				}
				id := q.pending[0]
				q.pending = q.pending[1:]
				q.active++
				q.mu.Unlock()
				handler(id)
				q.mu.Lock()
				q.active--
				q.cond.Broadcast()
				q.mu.Unlock()
			}
		}()
	}
}
func (q *Queue) Stop(ctx context.Context) error {
	q.mu.Lock()
	q.closed = true
	q.pending = nil
	q.cond.Broadcast()
	q.mu.Unlock()
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue) Stats() map[string]int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return map[string]int{"workers": q.workers, "capacity": q.capacity, "active": q.active, "pending": len(q.pending)}
}
