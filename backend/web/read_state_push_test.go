package web

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestReadStatePushQueueSingleDrainPerUser(t *testing.T) {
	q := newReadStatePushQueue()
	var mu sync.Mutex
	starts := 0
	for i := 0; i < 5; i++ {
		q.enqueue(7, []int64{int64(i)}, func(int64) {
			mu.Lock()
			starts++
			mu.Unlock()
		})
	}
	mu.Lock()
	if starts != 1 {
		mu.Unlock()
		t.Fatalf("drain goroutines started = %d, want 1", starts)
	}
	mu.Unlock()
	if _, ok := q.takePending(7); !ok {
		t.Fatal("pending IDs lost")
	}
}

func TestReadStatePushQueueStopsAfterConsecutiveErrors(t *testing.T) {
	q := newReadStatePushQueue()
	ctx := context.Background()
	calls := 0
	push := func(ctx context.Context, userID, messageID int64) error {
		calls++
		return errors.New("remote unavailable")
	}
	ids := make([]int64, 50)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	q.enqueue(9, ids, nil)
	done := make(chan struct{})
	go func() {
		q.drain(9, ctx, push, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not stop after repeated failures")
	}
	if calls > readStatePushMaxConsecutiveErrors {
		t.Fatalf("push calls = %d, want at most %d", calls, readStatePushMaxConsecutiveErrors)
	}
}

func TestReadStatePushQueueHonorsContextCancel(t *testing.T) {
	q := newReadStatePushQueue()
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan int, 100)
	push := func(ctx context.Context, userID, messageID int64) error {
		select {
		case calls <- 1:
		default:
		}
		cancel()
		return nil
	}
	ids := make([]int64, 30)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	q.enqueue(11, ids, nil)
	done := make(chan struct{})
	go func() {
		q.drain(11, ctx, push, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain ignored context cancellation")
	}
}

func TestReadStatePushQueueReportsIdleWhenEmpty(t *testing.T) {
	q := newReadStatePushQueue()
	idle := make(chan struct{}, 1)
	succeeded := map[int64]bool{}
	var mu sync.Mutex
	push := func(ctx context.Context, userID, messageID int64) error {
		mu.Lock()
		succeeded[messageID] = true
		mu.Unlock()
		return nil
	}
	q.enqueue(13, []int64{41, 42}, nil)
	q.mu.Lock()
	// Replace running flag so drain's takePending sees a worker; enqueue above
	// already marked it running.
	running := q.running[13]
	q.mu.Unlock()
	if !running {
		t.Fatal("enqueue did not mark user as running")
	}
	go func() {
		q.drain(13, context.Background(), push, func(int64) { idle <- struct{}{} })
	}()
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("onIdle never fired after successful drain")
	}
	mu.Lock()
	defer mu.Unlock()
	if !succeeded[41] || !succeeded[42] {
		t.Fatalf("processed = %v", succeeded)
	}
}
