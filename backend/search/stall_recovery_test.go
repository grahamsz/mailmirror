package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rolltop/backend/store"
)

func TestActiveWriterStallSurvivesCallerCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	base, err := svc.indexForUser(17)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingBatchIndex{
		delegatedBleveIndex: base,
		started:             make(chan struct{}),
		release:             make(chan struct{}),
		finished:            make(chan struct{}),
	}
	svc.mu.Lock()
	svc.indexes[17] = blocking
	svc.mu.Unlock()
	t.Cleanup(func() {
		blocking.unblock()
		_ = svc.Close()
	})

	svc.activeStallAfter = 20 * time.Millisecond
	logs := &capturedBleveLogs{}
	svc.bleveErrorLog = logs.Printf
	stalledUsers := make(chan int64, 2)
	svc.SetActiveWriterStallHandler(func(userID int64) {
		stalledUsers <- userID
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- svc.IndexMessage(ctx, store.MessageRecord{
			ID: 912, UserID: 17, AccountID: 4, MailboxID: 34,
			Subject: "private stalled subject", BodyText: "private stalled body", Date: time.Now(),
		}, nil)
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not start")
	}
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("IndexMessage error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller remained blocked after cancellation")
	}

	select {
	case userID := <-stalledUsers:
		if userID != 17 {
			t.Fatalf("stalled user = %d, want 17", userID)
		}
	case <-time.After(time.Second):
		t.Fatal("active writer watchdog did not signal")
	}
	required, err := svc.SearchIndexRecoveryRequired(17)
	if err != nil {
		t.Fatal(err)
	}
	if !required {
		t.Fatal("active writer watchdog did not persist a recovery marker")
	}
	if writer := svc.writerForUser(17); writer.TryLock() {
		writer.Unlock()
		t.Fatal("watchdog released the writer while Bleve Batch remained active")
	}
	svc.writeCoordinator.mu.Lock()
	coordinatorActive := svc.writeCoordinator.activeUsers[17]
	svc.writeCoordinator.mu.Unlock()
	if coordinatorActive != 1 {
		t.Fatalf("coordinator active leases for user 17 = %d, want 1 until Bleve returns", coordinatorActive)
	}

	output := logs.String()
	for _, want := range []string{
		`bleve active writer stalled operation="index-batch"`,
		`user_id=17 account_id=4 mailbox_id=34 documents=1`,
		`first_document_id=912 last_document_id=912 document_ids=[912]`,
		`marker_written=true marker_error_type=<nil>`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stall diagnostics missing %q: %q", want, output)
		}
	}
	if strings.Contains(output, "batch_bytes=0") {
		t.Fatalf("stall diagnostics omitted projected batch size: %q", output)
	}
	for _, private := range []string{"private stalled subject", "private stalled body"} {
		if strings.Contains(output, private) {
			t.Fatalf("stall diagnostics exposed indexed content %q: %q", private, output)
		}
	}

	time.Sleep(3 * svc.activeStallAfter)
	select {
	case userID := <-stalledUsers:
		t.Fatalf("watchdog signaled more than once for user %d", userID)
	default:
	}
	blocking.unblock()
	select {
	case <-blocking.finished:
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not finish after release")
	}
	deadline := time.Now().Add(time.Second)
	for {
		svc.writeCoordinator.mu.Lock()
		coordinatorActive = svc.writeCoordinator.activeUsers[17]
		svc.writeCoordinator.mu.Unlock()
		if coordinatorActive == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("coordinator lease remained active after Bleve returned")
		}
		time.Sleep(time.Millisecond)
	}
	required, err = svc.SearchIndexRecoveryRequired(17)
	if err != nil {
		t.Fatal(err)
	}
	if !required {
		t.Fatal("recovery marker was cleared when the stalled operation returned")
	}
}

func TestActiveWriterWatchdogIgnoresCompletedOperation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	base, err := svc.indexForUser(23)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingBatchIndex{
		delegatedBleveIndex: base,
		started:             make(chan struct{}),
		release:             make(chan struct{}),
		finished:            make(chan struct{}),
	}
	svc.mu.Lock()
	svc.indexes[23] = blocking
	svc.mu.Unlock()
	t.Cleanup(func() {
		blocking.unblock()
		_ = svc.Close()
	})

	svc.activeStallAfter = 100 * time.Millisecond
	logs := &capturedBleveLogs{}
	svc.bleveErrorLog = logs.Printf
	stalledUsers := make(chan int64, 1)
	svc.SetActiveWriterStallHandler(func(userID int64) { stalledUsers <- userID })
	result := make(chan error, 1)
	go func() {
		result <- svc.IndexMessage(context.Background(), store.MessageRecord{
			ID: 1, UserID: 23, AccountID: 2, MailboxID: 3, Subject: "fast batch", Date: time.Now(),
		}, nil)
	}()
	select {
	case <-blocking.started:
		blocking.unblock()
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not start")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("completed Bleve batch did not return")
	}
	time.Sleep(2 * svc.activeStallAfter)
	select {
	case userID := <-stalledUsers:
		t.Fatalf("completed operation triggered watchdog for user %d", userID)
	default:
	}
	if required, err := svc.SearchIndexRecoveryRequired(23); err != nil {
		t.Fatal(err)
	} else if required {
		t.Fatal("completed operation left a recovery marker")
	}
	if strings.Contains(logs.String(), "active writer stalled") {
		t.Fatalf("completed operation produced stall diagnostics: %q", logs.String())
	}
}

func TestActiveWriterStallSignalsBeforeBlockedDiagnostics(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	base, err := svc.indexForUser(29)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingBatchIndex{
		delegatedBleveIndex: base,
		started:             make(chan struct{}),
		release:             make(chan struct{}),
		finished:            make(chan struct{}),
	}
	svc.mu.Lock()
	svc.indexes[29] = blocking
	svc.mu.Unlock()
	t.Cleanup(func() {
		blocking.unblock()
		_ = svc.Close()
	})

	svc.activeStallAfter = 20 * time.Millisecond
	diagnosticStarted := make(chan struct{})
	releaseDiagnostic := make(chan struct{})
	var diagnosticOnce sync.Once
	var releaseDiagnosticOnce sync.Once
	releaseBlockedDiagnostic := func() { releaseDiagnosticOnce.Do(func() { close(releaseDiagnostic) }) }
	svc.bleveErrorLog = func(string, ...any) {
		diagnosticOnce.Do(func() {
			close(diagnosticStarted)
			<-releaseDiagnostic
		})
	}
	stalledUsers := make(chan int64, 1)
	svc.SetActiveWriterStallHandler(func(userID int64) { stalledUsers <- userID })
	t.Cleanup(releaseBlockedDiagnostic)
	result := make(chan error, 1)
	go func() {
		result <- svc.IndexMessage(context.Background(), store.MessageRecord{
			ID: 1, UserID: 29, AccountID: 2, MailboxID: 3, Subject: "blocked diagnostics", Date: time.Now(),
		}, nil)
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not start")
	}

	select {
	case userID := <-stalledUsers:
		if userID != 29 {
			t.Fatalf("stalled user = %d, want 29", userID)
		}
	case <-time.After(time.Second):
		t.Fatal("restart signal was blocked by diagnostics")
	}
	select {
	case <-diagnosticStarted:
	case <-time.After(time.Second):
		t.Fatal("stall diagnostics did not start after restart signal")
	}
	if required, markerErr := svc.SearchIndexRecoveryRequired(29); markerErr != nil || !required {
		t.Fatalf("recovery marker required=%t err=%v, want true, nil", required, markerErr)
	}

	releaseBlockedDiagnostic()
	blocking.unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Bleve batch did not finish")
	}
}

func TestSearchIndexRecoveryMarkerRoundTripWithMalformedIndex(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	userDir := filepath.Join(root, "31")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "bleve"), []byte("not an index directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.MarkSearchIndexRecoveryRequired(31); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkSearchIndexRecoveryRequired(31); err != nil {
		t.Fatalf("idempotent marker write: %v", err)
	}
	if required, err := svc.SearchIndexRecoveryRequired(31); err != nil {
		t.Fatal(err)
	} else if !required {
		t.Fatal("recovery marker was not found")
	}
	if err := svc.ClearSearchIndexRecoveryRequired(31); err != nil {
		t.Fatal(err)
	}
	if required, err := svc.SearchIndexRecoveryRequired(31); err != nil {
		t.Fatal(err)
	} else if required {
		t.Fatal("recovery marker remained after clear")
	}
}

func TestSearchIndexRecoveryMarkerIsRestoredWhenClearCannotBePersisted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	svc, err := OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.MarkSearchIndexRecoveryRequired(37); err != nil {
		t.Fatal(err)
	}

	persistErr := errors.New("directory sync failed")
	syncCalls := 0
	err = svc.clearSearchIndexRecoveryRequiredWithSync(37, func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return persistErr
		}
		return nil
	})
	if !errors.Is(err, persistErr) || !strings.Contains(err.Error(), "marker restored for retry") {
		t.Fatalf("clear error = %v, want persisted-clear failure with restored marker", err)
	}
	if syncCalls != 2 {
		t.Fatalf("directory sync calls = %d, want clear and restored-marker sync", syncCalls)
	}
	if required, requiredErr := svc.SearchIndexRecoveryRequired(37); requiredErr != nil || !required {
		t.Fatalf("recovery marker required=%t err=%v, want true, nil", required, requiredErr)
	}
	if err := svc.ClearSearchIndexRecoveryRequired(37); err != nil {
		t.Fatal(err)
	}
}

func TestFilterBleveBatchStackReturnsOnlyTargetFrames(t *testing.T) {
	stack := []byte(`goroutine 7 [select]:
rolltop/backend/search.privateWorker("private message body")
	/home/rolltop/private.go:12 +0x111

goroutine 19 [semacquire]:
github.com/blevesearch/bleve/v2/index/scorch.(*Scorch).Batch(0xc000, 0xdeadbeef)
	/home/gxs/go/pkg/mod/github.com/blevesearch/bleve/v2/index/scorch/scorch.go:201 +0x123
rolltop/backend/search.(*Service).commitBatch.func1()
	/home/rolltop/backend/search/search.go:850 +0x99
`)
	filtered := filterBleveBatchStack(stack)
	for _, want := range []string{
		"github.com/blevesearch/bleve/v2/index/scorch.(*Scorch).Batch",
		"/index/scorch/scorch.go:201",
		"rolltop/backend/search.(*Service).commitBatch.func1",
	} {
		if !strings.Contains(filtered, want) {
			t.Fatalf("filtered stack missing %q: %q", want, filtered)
		}
	}
	for _, unwanted := range []string{"goroutine", "private message body", "0xdeadbeef", "+0x123"} {
		if strings.Contains(filtered, unwanted) {
			t.Fatalf("filtered stack retained %q: %q", unwanted, filtered)
		}
	}
	if len(filtered) > maxBleveStackBytes {
		t.Fatalf("filtered stack length = %d, want <= %d", len(filtered), maxBleveStackBytes)
	}
}
