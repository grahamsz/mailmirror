// File overview: Supervised background queue that pushes local read-state
// changes to IMAP. Replaces the earlier fire-and-forget goroutine that walked
// up to 1000 messages on context.Background() with ignored errors: work now
// runs on the server's shutdown-aware context, is deduplicated per user, and
// stops early when the remote server keeps failing.

package web

import (
	"context"
	"log"
	"sync"
	"time"
)

const (
	// readStatePushIdleDelay paces sequential IMAP sessions so a large bulk
	// selection cannot hammer the remote server with back-to-back logins.
	readStatePushIdleDelay = 100 * time.Millisecond
	// readStatePushMaxRuntime bounds one drain cycle. The local read flag is
	// already committed before this queue starts, so abandoning the remainder
	// only defers the remote \Seen push until the next change or sync.
	readStatePushMaxRuntime = 10 * time.Minute
	// readStatePushMaxConsecutiveErrors stops a drain cycle against a dead or
	// half-open IMAP server instead of grinding through every remaining ID at
	// one timeout each.
	readStatePushMaxConsecutiveErrors = 5
	// readStatePushMaxPending bounds queued IDs per user; older overflow is
	// dropped because the push is best-effort and self-healing on later syncs.
	readStatePushMaxPending = 5000
)

type readStatePushQueue struct {
	mu      sync.Mutex
	pending map[int64][]int64
	running map[int64]bool
}

func newReadStatePushQueue() *readStatePushQueue {
	return &readStatePushQueue{
		pending: map[int64][]int64{},
		running: map[int64]bool{},
	}
}

// enqueue records IDs and starts a single drain goroutine per user. Concurrent
// callers while a drain is active only append to the pending list.
func (q *readStatePushQueue) enqueue(userID int64, ids []int64, start func(userID int64)) {
	if q == nil || userID <= 0 || len(ids) == 0 {
		return
	}
	q.mu.Lock()
	merged := append(q.pending[userID], ids...)
	if len(merged) > readStatePushMaxPending {
		merged = merged[len(merged)-readStatePushMaxPending:]
	}
	q.pending[userID] = merged
	alreadyRunning := q.running[userID]
	if !alreadyRunning {
		q.running[userID] = true
	}
	q.mu.Unlock()
	if !alreadyRunning && start != nil {
		start(userID)
	}
}

// pendingSnapshot drains the pending list under lock. A nil result with
// running=false means the queue is empty and the worker should exit.
func (q *readStatePushQueue) takePending(userID int64) ([]int64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := q.pending[userID]
	q.pending[userID] = nil
	if len(ids) == 0 {
		q.running[userID] = false
		return nil, false
	}
	return ids, true
}

// drain processes pending IDs until the queue is empty, the context is
// canceled, the runtime budget is exhausted, or the remote server fails
// repeatedly. It never returns an error: the local read state is already
// durable and the remote push retries naturally on the next change.
func (q *readStatePushQueue) drain(
	userID int64,
	ctx context.Context,
	push func(ctx context.Context, userID, messageID int64) error,
	onIdle func(userID int64),
) {
	if q == nil || push == nil {
		return
	}
	deadline := time.Now().Add(readStatePushMaxRuntime)
	for {
		if ctx.Err() != nil {
			q.mu.Lock()
			q.pending[userID] = nil
			q.running[userID] = false
			q.mu.Unlock()
			return
		}
		ids, continueQueue := q.takePending(userID)
		if !continueQueue {
			if onIdle != nil {
				onIdle(userID)
			}
			return
		}
		consecutiveErrors := 0
		for _, messageID := range ids {
			if ctx.Err() != nil {
				break
			}
			if time.Now().After(deadline) {
				log.Printf("read-state push budget exhausted user_id=%d remaining=%d", userID, len(ids))
				break
			}
			if err := push(ctx, userID, messageID); err != nil {
				consecutiveErrors++
				// Do not log error strings: they can carry message-derived detail.
				log.Printf("read-state push failed user_id=%d message_id=%d consecutive=%d error_type=%T", userID, messageID, consecutiveErrors, err)
				if consecutiveErrors >= readStatePushMaxConsecutiveErrors {
					log.Printf("read-state push stopping after repeated failures user_id=%d remaining=%d", userID, len(ids))
					break
				}
			} else {
				consecutiveErrors = 0
			}
			select {
			case <-ctx.Done():
			case <-time.After(readStatePushIdleDelay):
			}
		}
	}
}

// queueReadStatePush schedules IMAP \Seen pushes on the server's background
// context. It replaces direct `go ... context.Background()` loops.
func (s *Server) queueReadStatePush(userID int64, ids []int64) {
	if s == nil || s.syncer == nil || len(ids) == 0 {
		return
	}
	q := s.readStatePushes
	if q == nil {
		return
	}
	q.enqueue(userID, ids, func(queueUserID int64) {
		go q.drain(queueUserID, s.backgroundContext(), s.pushReadStateForUser, s.notifyUserChanged)
	})
}

func (s *Server) backgroundContext() context.Context {
	if s == nil || s.backgroundCtx == nil {
		return context.Background()
	}
	return s.backgroundCtx
}

func (s *Server) pushReadStateForUser(ctx context.Context, userID, messageID int64) error {
	return s.syncer.SyncReadStateForMessage(ctx, userID, messageID)
}
