// File overview: Serialized sync runner that queues normal and priority sync jobs.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"rolltop/backend/store"
)

// Inbox polling is a freshness path, not an unbounded repair job. Each pass
// checkpoints incrementally and may resume on the next poll.
const liveInboxSyncTurnTimeout = 90 * time.Second

// Every ordinary folder sync is incremental and checkpoints its UID after each
// stored batch. Bound one folder turn so a silent IMAP command cannot keep an
// account-wide pass (and its visible sync run) alive forever; the next
// scheduled pass resumes from the durable UID checkpoint.
const ordinaryMailboxSyncTurnTimeout = 3 * time.Minute
const senderStatsRefreshTimeout = 2 * time.Minute

// Attachment indexing is derived, replayable work. Most stages honor cancellation,
// but MIME and external document parsing can be inside an uncancellable call when a
// mail sync asks the worker to yield. Do not let that background work starve the
// durable incremental sync indefinitely.
const attachmentIndexYieldTimeout = 15 * time.Second

type runnerMailboxCancellation struct {
	userID int64
	cancel context.CancelFunc
}

// Runner serializes sync work per user/mailbox and launches background indexing follow-ups.
type Runner struct {
	Service *Service
	ctx     context.Context

	mu                         sync.Mutex
	autoRunning                map[int64]bool
	autoPlanning               map[int64]bool
	autoCancels                map[int64]context.CancelFunc
	mailboxRunning             map[string]bool
	mailboxCancels             map[string]runnerMailboxCancellation
	runControls                map[int64]runnerSyncRunControl
	workActivities             map[string]runnerWorkActivity
	mailboxPending             map[string]bool
	accountMailboxPending      map[string]bool
	foregroundRunning          map[int64]int
	foregroundDone             map[int64]chan struct{}
	foregroundDeferredAuto     map[int64]bool
	foregroundDeferredBoxes    map[int64]map[string]string
	foregroundDeferredAccts    map[int64]map[string]deferredAccountMailbox
	autoDeferredAccts          map[int64]map[string]deferredAccountMailbox
	attachmentPending          map[int64]bool
	attachmentCancels          map[int64]context.CancelFunc
	attachmentDone             map[int64]chan struct{}
	attachmentResumeAfterStats map[int64]bool
	attachmentRetryTimers      map[int64]*time.Timer
	attachmentRetryEpoch       map[int64]uint64
	senderStatsPending         map[int64]bool
	senderStatsRunning         map[int64]bool
	senderStatsCancels         map[int64]context.CancelFunc
	senderStatsDone            map[int64]chan struct{}
	generationRecoveryUsers    map[int64]bool
	generationRecoveryRuns     map[int64]bool
	generationRecoveryReplay   map[int64]bool
	generationRecoveryEpoch    map[int64]uint64
	generationRecoveryAccounts map[int64]map[int64]bool
	generationRecoveryTargets  map[int64]map[string]bool
	generationRecoveryKnown    map[int64]bool
	generationRecoveryCursor   map[int64]int64
	generationRecoveryActive   map[int64]generationRecoveryActivity
	generationRecoveryQueues   map[int64][]generationRecoveryQueueTarget
	generationRecoveryQueueLog map[int64]generationRecoveryQueueLogState
	generationRecoveryAuto     map[int64]bool
	generationRecoveryBoxes    map[int64]map[string]string
	generationRecoveryAccts    map[int64]map[string]deferredAccountMailbox
	arrivalScheduler           *inboxArrivalScheduler
	rebuildRecoveryRunning     bool
	rebuildRecoveryInterval    time.Duration
	generationRecoveryTimeout  time.Duration
	liveInboxSyncTimeout       time.Duration
	senderStatsTimeout         time.Duration
	attachmentYieldTimeout     time.Duration
	rebuildRecoveryWake        chan struct{}
	rebuildRecoveryBeforeStop  func()
	queueRebuildMailbox        func(store.PendingMailboxGenerationRebuild)
	replayGenerationRecovery   func(generationRecoveryReplay)
	refreshSenderStatsForUser  func(context.Context, int64) error
	indexAttachmentsForUser    func(context.Context, int64, int) (int, error)
}

// NewRunner builds a process-lifetime scheduler using a background context. The
// main package uses NewRunnerWithContext so shutdown can interrupt running jobs.
func NewRunner(service *Service) *Runner {
	return NewRunnerWithContext(context.Background(), service)
}

// NewRunnerWithContext wires cancellation into all future jobs. When startup or
// shutdown cancels ctx, new sync jobs are refused and active jobs report
// interruption through syncAccount's deferred finish.
func NewRunnerWithContext(ctx context.Context, service *Service) *Runner {
	if ctx == nil {
		ctx = context.Background()
	}
	runner := &Runner{
		Service:                    service,
		ctx:                        ctx,
		autoRunning:                map[int64]bool{},
		autoPlanning:               map[int64]bool{},
		autoCancels:                map[int64]context.CancelFunc{},
		mailboxRunning:             map[string]bool{},
		mailboxCancels:             map[string]runnerMailboxCancellation{},
		runControls:                map[int64]runnerSyncRunControl{},
		workActivities:             map[string]runnerWorkActivity{},
		mailboxPending:             map[string]bool{},
		accountMailboxPending:      map[string]bool{},
		foregroundRunning:          map[int64]int{},
		foregroundDone:             map[int64]chan struct{}{},
		foregroundDeferredAuto:     map[int64]bool{},
		foregroundDeferredBoxes:    map[int64]map[string]string{},
		foregroundDeferredAccts:    map[int64]map[string]deferredAccountMailbox{},
		autoDeferredAccts:          map[int64]map[string]deferredAccountMailbox{},
		attachmentPending:          map[int64]bool{},
		attachmentCancels:          map[int64]context.CancelFunc{},
		attachmentDone:             map[int64]chan struct{}{},
		attachmentResumeAfterStats: map[int64]bool{},
		attachmentRetryTimers:      map[int64]*time.Timer{},
		attachmentRetryEpoch:       map[int64]uint64{},
		senderStatsPending:         map[int64]bool{},
		senderStatsRunning:         map[int64]bool{},
		senderStatsCancels:         map[int64]context.CancelFunc{},
		senderStatsDone:            map[int64]chan struct{}{},
		generationRecoveryUsers:    map[int64]bool{},
		generationRecoveryRuns:     map[int64]bool{},
		generationRecoveryReplay:   map[int64]bool{},
		generationRecoveryEpoch:    map[int64]uint64{},
		generationRecoveryAccounts: map[int64]map[int64]bool{},
		generationRecoveryTargets:  map[int64]map[string]bool{},
		generationRecoveryKnown:    map[int64]bool{},
		generationRecoveryCursor:   map[int64]int64{},
		generationRecoveryActive:   map[int64]generationRecoveryActivity{},
		generationRecoveryQueues:   map[int64][]generationRecoveryQueueTarget{},
		generationRecoveryQueueLog: map[int64]generationRecoveryQueueLogState{},
		generationRecoveryAuto:     map[int64]bool{},
		generationRecoveryBoxes:    map[int64]map[string]string{},
		generationRecoveryAccts:    map[int64]map[string]deferredAccountMailbox{},
		attachmentYieldTimeout:     attachmentIndexYieldTimeout,
		rebuildRecoveryWake:        make(chan struct{}, 1),
	}
	runner.arrivalScheduler = newInboxArrivalScheduler(ctx, runner.finalizePendingInboxArrivals, runner.notifyInboxArrivals)
	if service != nil {
		service.DeferMailboxGenerationRebuilds = true
		service.ScheduleInboxArrival = runner.ScheduleInboxArrival
		service.MailboxGenerationRecoveryStarted = runner.SignalMailboxGenerationRecovery
	}
	if done := ctx.Done(); done != nil {
		go func() {
			<-done
			runner.cancelAllAttachmentRetryTimers()
		}()
	}
	return runner
}

func (r *Runner) context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// lockAfterSenderStats waits for the tenant's derived sender-stat writer, then
// returns with r.mu held. Rechecking under the same mutex closes the admission
// race where a new refresh could otherwise start between waiting and reserving
// a mailbox writer.
func (r *Runner) lockAfterSenderStats(userID int64) bool {
	for r.context().Err() == nil {
		r.mu.Lock()
		if !r.senderStatsRunning[userID] {
			return true
		}
		done := r.senderStatsDone[userID]
		r.mu.Unlock()
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-r.context().Done():
			return false
		}
	}
	return false
}

// exclusiveWriterWaitTimeout bounds how long sync admission waits for a
// foreground operation to release its barrier. Without a cap, one leaked
// foreground barrier would block this user's Start forever — and because the
// scheduler starts users serially, every user behind them.
const exclusiveWriterWaitTimeout = 5 * time.Minute

func (r *Runner) lockAfterExclusiveWriters(userID int64) bool {
	started := time.Now()
	for r.context().Err() == nil {
		if !r.lockAfterSenderStats(userID) {
			return false
		}
		if r.foregroundRunning[userID] == 0 {
			return true
		}
		done := r.foregroundDone[userID]
		r.mu.Unlock()
		if done == nil {
			if time.Since(started) >= exclusiveWriterWaitTimeout {
				return false
			}
			continue
		}
		remaining := time.Until(started.Add(exclusiveWriterWaitTimeout))
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-done:
			timer.Stop()
		case <-r.context().Done():
			timer.Stop()
			return false
		case <-timer.C:
			return false
		}
	}
	return false
}

// Start begins an account-wide sync for one user if one is not already running.
// It plans folders first, then runs them serially as mailbox jobs so per-folder
// progress and priority reruns stay visible.
func (r *Runner) Start(userID int64) bool {
	ctx := r.context()
	if ctx.Err() != nil {
		return false
	}
	if !r.lockAfterExclusiveWriters(userID) {
		return false
	}
	if r.generationRecoveryGatedLocked(userID) {
		r.deferGenerationRecoveryAutoLocked(userID)
		r.mu.Unlock()
		return false
	}
	if r.autoRunning[userID] {
		r.mu.Unlock()
		return false
	}
	r.autoRunning[userID] = true
	r.autoPlanning[userID] = true
	ctx, cancel := context.WithCancel(ctx)
	r.autoCancels[userID] = cancel
	r.startWorkActivityLocked(runnerUserWorkActivityKey(runnerWorkAccountSync, userID), runnerWorkActivity{
		kind:   runnerWorkAccountSync,
		userID: userID,
	})
	r.pauseAttachmentIndexLocked(userID)
	r.mu.Unlock()

	go func() {
		planning := true
		releasePlanning := func() {
			if !planning {
				return
			}
			r.mu.Lock()
			delete(r.autoPlanning, userID)
			r.mu.Unlock()
			planning = false
		}
		defer func() {
			cancel()
			releasePlanning()
			r.mu.Lock()
			delete(r.autoRunning, userID)
			delete(r.autoCancels, userID)
			r.finishWorkActivityLocked(runnerUserWorkActivityKey(runnerWorkAccountSync, userID))
			deferredAccountMailboxes := r.takeAutoDeferredAccountMailboxesLocked(userID)
			r.mu.Unlock()
			for _, request := range deferredAccountMailboxes {
				r.QueueAccountMailboxes(userID, request.accountID, []string{request.mailbox})
			}
			r.refreshGenerationRecoveryGateForUser(r.context(), userID)
			r.RefreshSenderStats(userID)
			r.StartAttachmentIndex(userID)
		}()
		if err := r.waitForAttachmentIndex(ctx, userID); err != nil {
			return
		}
		mailboxes, err := r.Service.AutoMailboxNames(ctx, userID)
		if err != nil {
			log.Printf("plan sync user_id=%d: %v", userID, err)
			return
		}
		releasePlanning()
		// Account-wide sync is deliberately decomposed into mailbox jobs. That
		// keeps long archive backfills visible and allows foreground INBOX work
		// to proceed without waiting behind unrelated folders.
		for _, mailbox := range mailboxes {
			if ctx.Err() != nil {
				return
			}
			if !r.runMailboxes(userID, []string{mailbox}) {
				log.Printf("sync user_id=%d mailbox=%s skipped: already running", userID, mailbox)
			}
		}
	}()
	return true
}

// StartMailboxes schedules named folders across all accounts for a user. It is
// used after operations that should refresh source/destination mailboxes.
func (r *Runner) StartMailboxes(userID int64, mailboxes []string) bool {
	if r.context().Err() != nil {
		return false
	}
	mailboxes = uniqueMailboxes(mailboxes)
	if len(mailboxes) == 0 {
		return false
	}
	keys, ok := r.reserveMailboxes(userID, mailboxes)
	if !ok {
		return false
	}
	go func() {
		r.runReservedMailboxes(userID, mailboxes, keys)
		r.refreshGenerationRecoveryGateForUser(r.context(), userID)
		r.RefreshSenderStats(userID)
		r.StartAttachmentIndex(userID)
	}()
	return true
}

// StartAccountMailboxes reserves account-qualified folder keys so identical
// mailbox names on different IMAP servers do not block each other unnecessarily.
func (r *Runner) StartAccountMailboxes(userID, accountID int64, mailboxes []string) bool {
	if r.context().Err() != nil {
		return false
	}
	mailboxes = uniqueMailboxes(mailboxes)
	if len(mailboxes) == 0 || accountID <= 0 {
		return false
	}
	keys, ok := r.reserveAccountMailboxes(userID, accountID, mailboxes)
	if !ok {
		return false
	}
	go func() {
		r.runReservedAccountMailboxes(userID, accountID, mailboxes, keys)
		r.refreshGenerationRecoveryGateForUser(r.context(), userID)
		r.RefreshSenderStats(userID)
		r.StartAttachmentIndex(userID)
	}()
	return true
}

// QueueAccountMailboxes guarantees one account-qualified sync pass. If the
// destination is already reserved, the active job launches one follow-up pass
// after releasing it. Backend plugins use this after IMAP APPEND so the new
// remote message is mirrored without syncing same-named folders on every
// account.
func (r *Runner) QueueAccountMailboxes(userID, accountID int64, mailboxes []string) bool {
	if r.context().Err() != nil {
		return false
	}
	mailboxes = uniqueMailboxes(mailboxes)
	if len(mailboxes) == 0 || accountID <= 0 {
		return false
	}
	for _, mailbox := range mailboxes {
		names := []string{mailbox}
		keys, reserved := r.reserveOrQueueAccountMailboxes(userID, accountID, names)
		if !reserved {
			continue
		}
		go func(names []string, keys []string) {
			r.runReservedAccountMailboxes(userID, accountID, names, keys)
			r.refreshGenerationRecoveryGateForUser(r.context(), userID)
			r.RefreshSenderStats(userID)
			r.StartAttachmentIndex(userID)
		}(names, keys)
	}
	return true
}

// StartMailboxMaintenance reserves one account/mailbox and runs local maintenance in the background.
// Work on another mailbox of the same account may continue concurrently. A
// pending generation recovery blocks admission and cancels active maintenance.
func (r *Runner) StartMailboxMaintenance(userID int64, mailbox store.Mailbox, label string, fn func(context.Context, int64, *store.SyncProgress) error) (store.SyncRun, bool, error) {
	return r.startMailboxMaintenance(userID, mailbox, label, false, nil, fn)
}

// StartMailboxMaintenanceWithSetup reserves one account/mailbox, waits for any
// attachment-index worker to stop, and then applies setup synchronously while
// the reservation is held. This is for settings changes that must become
// visible before their asynchronous maintenance starts without leaving a race
// window for sync, purge, or rebuild work on the same mailbox.
func (r *Runner) StartMailboxMaintenanceWithSetup(userID int64, mailbox store.Mailbox, label string, setup func(context.Context) error, fn func(context.Context, int64, *store.SyncProgress) error) (store.SyncRun, bool, error) {
	if setup == nil {
		return store.SyncRun{}, false, fmt.Errorf("maintenance setup function is required")
	}
	return r.startMailboxMaintenance(userID, mailbox, label, false, setup, fn)
}

// StartMailboxMaintenanceToCompletion runs an explicit user-requested task
// under a mailbox reservation until it completes or the server stops. Unlike
// ordinary sync maintenance, mailbox-generation recovery cannot interrupt it
// midway and turn one requested rebuild into a sequence of fresh attempts.
func (r *Runner) StartMailboxMaintenanceToCompletion(userID int64, mailbox store.Mailbox, label string, fn func(context.Context, int64, *store.SyncProgress) error) (store.SyncRun, bool, error) {
	return r.StartMailboxMaintenanceWithSetup(userID, mailbox, label, func(context.Context) error { return nil }, fn)
}

// StartMailboxSearchRebuildToCompletion runs a full-text rebuild to completion
// like StartMailboxMaintenanceToCompletion, but marks the reservation as
// search-only. A rebuild re-derives search documents from durable SQLite rows
// and converges with concurrent inserts, so foreground operations such as
// sending do not have to wait behind it.
func (r *Runner) StartMailboxSearchRebuildToCompletion(userID int64, mailbox store.Mailbox, label string, fn func(context.Context, int64, *store.SyncProgress) error) (store.SyncRun, bool, error) {
	return r.startMailboxMaintenance(userID, mailbox, label, true, func(context.Context) error { return nil }, fn)
}

func (r *Runner) startMailboxMaintenance(userID int64, mailbox store.Mailbox, label string, searchOnly bool, setup func(context.Context) error, fn func(context.Context, int64, *store.SyncProgress) error) (store.SyncRun, bool, error) {
	ctx := r.context()
	if ctx.Err() != nil {
		return store.SyncRun{}, false, nil
	}
	if r.Service == nil || r.Service.Store == nil {
		return store.SyncRun{}, false, fmt.Errorf("sync service is not configured")
	}
	if fn == nil {
		return store.SyncRun{}, false, fmt.Errorf("maintenance function is required")
	}
	mailboxes := uniqueMailboxes([]string{mailbox.Name})
	if userID <= 0 || mailbox.AccountID <= 0 || len(mailboxes) == 0 {
		return store.SyncRun{}, false, fmt.Errorf("mailbox is required")
	}
	keys, ok := r.reserveAccountMailboxesForMaintenance(userID, mailbox.AccountID, mailboxes, searchOnly)
	if !ok {
		return store.SyncRun{}, false, nil
	}
	// A setup-backed maintenance task changes durable state before its
	// asynchronous body starts. Register its ordinary cancellation before that
	// mutation so a pending generation-recovery signal is observed. Once setup
	// commits, the body is deliberately handed to a root-scoped context: it must
	// finish the matching index transition instead of yielding halfway between
	// the SQLite setting and Bleve.
	var finishSetupContext func()
	if setup != nil {
		ctx, finishSetupContext = r.ordinaryMailboxContext(userID, keys, false)
		if err := ctx.Err(); err != nil {
			finishSetupContext()
			r.releaseMailboxMaintenanceReservation(userID, mailbox.AccountID, mailboxes, keys)
			r.StartAttachmentIndex(userID)
			return store.SyncRun{}, true, err
		}
	}
	finishSetup := func() {
		if finishSetupContext != nil {
			finishSetupContext()
			finishSetupContext = nil
		}
	}
	if err := r.waitForAttachmentIndex(ctx, userID); err != nil {
		finishSetup()
		r.releaseMailboxMaintenanceReservation(userID, mailbox.AccountID, mailboxes, keys)
		return store.SyncRun{}, true, err
	}
	run, err := r.Service.Store.CreateSyncRun(context.Background(), userID, mailbox.AccountID)
	if err != nil {
		finishSetup()
		r.releaseMailboxMaintenanceReservation(userID, mailbox.AccountID, mailboxes, keys)
		r.StartAttachmentIndex(userID)
		return store.SyncRun{}, true, err
	}
	progress := store.SyncProgress{MailboxesTotal: 1, CurrentMailbox: mailbox.Name, LatestNewFrom: "rolltop:maintenance", LatestNewSubject: label}
	if err := r.Service.Store.UpdateSyncRunProgress(context.Background(), userID, run.ID, progress); err != nil {
		finishSetup()
		r.releaseMailboxMaintenanceReservation(userID, mailbox.AccountID, mailboxes, keys)
		r.StartAttachmentIndex(userID)
		return store.SyncRun{}, true, err
	}
	if setup != nil {
		if err := setup(ctx); err != nil {
			finishErr := r.Service.Store.FinishSyncRun(context.Background(), userID, run.ID, "failed", progress, err.Error())
			finishSetup()
			r.releaseMailboxMaintenanceReservation(userID, mailbox.AccountID, mailboxes, keys)
			r.StartAttachmentIndex(userID)
			r.Service.notify(userID)
			return run, true, errors.Join(err, finishErr)
		}
		finishSetup()
	}
	r.Service.notify(userID)
	if setup != nil {
		go r.runReservedCommittedMailboxMaintenance(userID, mailbox.AccountID, mailboxes, keys, run.ID, progress, fn)
	} else {
		go r.runReservedMailboxMaintenance(userID, mailbox.AccountID, mailboxes, keys, run.ID, progress, fn)
	}
	return run, true, nil
}

// StartAccountMaintenance reserves all known folders for one account and runs a
// local maintenance task in the background. It is used for destructive local
// cache work such as deleting an IMAP account from Rolltop.
func (r *Runner) StartAccountMaintenance(userID int64, account store.MailAccount, mailboxes []store.Mailbox, label string, fn func(context.Context, int64, *store.SyncProgress) error) (store.SyncRun, bool, error) {
	return r.startAccountMaintenance(userID, account, mailboxes, label, false, false, fn)
}

// StartAccountMaintenanceToCompletion is the account-wide counterpart to
// StartMailboxMaintenanceToCompletion for an explicit user-requested task.
func (r *Runner) StartAccountMaintenanceToCompletion(userID int64, account store.MailAccount, mailboxes []store.Mailbox, label string, fn func(context.Context, int64, *store.SyncProgress) error) (store.SyncRun, bool, error) {
	return r.startAccountMaintenance(userID, account, mailboxes, label, true, false, fn)
}

// StartAccountSearchRebuildToCompletion is the account-wide counterpart to
// StartMailboxSearchRebuildToCompletion: the rebuild is search-only, so
// foreground operations proceed while it runs.
func (r *Runner) StartAccountSearchRebuildToCompletion(userID int64, account store.MailAccount, mailboxes []store.Mailbox, label string, fn func(context.Context, int64, *store.SyncProgress) error) (store.SyncRun, bool, error) {
	return r.startAccountMaintenance(userID, account, mailboxes, label, true, true, fn)
}

func (r *Runner) startAccountMaintenance(userID int64, account store.MailAccount, mailboxes []store.Mailbox, label string, runToCompletion, searchOnly bool, fn func(context.Context, int64, *store.SyncProgress) error) (store.SyncRun, bool, error) {
	ctx := r.context()
	if ctx.Err() != nil {
		return store.SyncRun{}, false, nil
	}
	if r.Service == nil || r.Service.Store == nil {
		return store.SyncRun{}, false, fmt.Errorf("sync service is not configured")
	}
	if fn == nil {
		return store.SyncRun{}, false, fmt.Errorf("maintenance function is required")
	}
	names := mailboxNamesForReservation(mailboxes)
	if len(names) == 0 {
		names = []string{"__account__"}
	}
	keys, ok := r.reserveAccountMailboxesForMaintenance(userID, account.ID, names, searchOnly)
	if !ok {
		return store.SyncRun{}, false, nil
	}
	if err := r.waitForAttachmentIndex(ctx, userID); err != nil {
		r.releaseMailboxMaintenanceReservation(userID, account.ID, names, keys)
		return store.SyncRun{}, true, err
	}
	run, err := r.Service.Store.CreateSyncRun(context.Background(), userID, account.ID)
	if err != nil {
		r.releaseMailboxMaintenanceReservation(userID, account.ID, names, keys)
		r.StartAttachmentIndex(userID)
		return store.SyncRun{}, true, err
	}
	progress := store.SyncProgress{
		MailboxesTotal:   len(mailboxes),
		CurrentMailbox:   accountMaintenanceLabel(account),
		LatestNewFrom:    "rolltop:maintenance",
		LatestNewSubject: label,
	}
	if err := r.Service.Store.UpdateSyncRunProgress(context.Background(), userID, run.ID, progress); err != nil {
		r.releaseMailboxMaintenanceReservation(userID, account.ID, names, keys)
		r.StartAttachmentIndex(userID)
		return store.SyncRun{}, true, err
	}
	r.Service.notify(userID)
	if runToCompletion {
		go r.runReservedCommittedMailboxMaintenance(userID, account.ID, names, keys, run.ID, progress, fn)
	} else {
		go r.runReservedMailboxMaintenance(userID, account.ID, names, keys, run.ID, progress, fn)
	}
	return run, true, nil
}

func (r *Runner) runReservedMailboxMaintenance(userID, accountID int64, mailboxes []string, keys []string, runID int64, progress store.SyncProgress, fn func(context.Context, int64, *store.SyncProgress) error) {
	ctx, finishContext := r.ordinaryMailboxContext(userID, keys, false)
	r.runReservedMailboxMaintenanceWithContext(ctx, finishContext, userID, accountID, mailboxes, keys, runID, progress, fn)
}

func (r *Runner) runReservedCommittedMailboxMaintenance(userID, accountID int64, mailboxes []string, keys []string, runID int64, progress store.SyncProgress, fn func(context.Context, int64, *store.SyncProgress) error) {
	ctx, finishContext := context.WithCancel(r.context())
	r.runReservedMailboxMaintenanceWithContext(ctx, finishContext, userID, accountID, mailboxes, keys, runID, progress, fn)
}

func (r *Runner) runReservedMailboxMaintenanceWithContext(ctx context.Context, finishContext func(), userID, accountID int64, mailboxes []string, keys []string, runID int64, progress store.SyncProgress, fn func(context.Context, int64, *store.SyncProgress) error) {
	status := "ok"
	errText := ""
	defer func() {
		if r.context().Err() != nil && status == "ok" && progress.MailboxesDone < progress.MailboxesTotal {
			status = "interrupted"
			errText = "Server stopped before this maintenance task finished."
		}
		if status == "ok" {
			progress.MailboxesDone = progress.MailboxesTotal
		}
		if err := r.Service.Store.FinishSyncRun(context.Background(), userID, runID, status, progress, errText); err != nil {
			log.Printf("finish mailbox maintenance user_id=%d run_id=%d: %v", userID, runID, err)
		}
		r.Service.notify(userID)
		r.releaseMailboxMaintenanceReservation(userID, accountID, mailboxes, keys)
		if status == "ok" {
			r.RefreshSenderStats(userID)
		}
		r.StartAttachmentIndex(userID)
	}()
	defer finishContext()
	r.setMailboxWorkActivitiesPhase(keys, "maintenance")
	if err := fn(ctx, runID, &progress); err != nil {
		if errors.Is(err, context.Canceled) && r.mailboxGenerationRecoveryPending(userID) {
			status = "interrupted"
			errText = "Mailbox maintenance yielded to mailbox generation recovery."
		} else {
			status = "failed"
			errText = err.Error()
			log.Printf("mailbox maintenance user_id=%d mailboxes=%s: %v", userID, strings.Join(mailboxes, ","), err)
		}
	}
}

func (r *Runner) releaseMailboxMaintenanceReservation(userID, accountID int64, mailboxes, keys []string) {
	reruns := r.releaseAccountMailboxReservations(userID, accountID, mailboxes, keys)
	r.refreshGenerationRecoveryGateForUser(r.context(), userID)
	r.startAccountMailboxReruns(userID, accountID, reruns)
}

func mailboxNamesForReservation(mailboxes []store.Mailbox) []string {
	names := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		names = append(names, mailbox.Name)
	}
	return uniqueMailboxes(names)
}

func accountMaintenanceLabel(account store.MailAccount) string {
	if strings.TrimSpace(account.Label) != "" {
		return account.Label
	}
	if strings.TrimSpace(account.Email) != "" {
		return account.Email
	}
	return account.Host
}

// StartPriorityMailboxes records a pending rerun when the folder is already busy.
// The active job will launch one follow-up pass after releasing its reservation.
func (r *Runner) StartPriorityMailboxes(userID int64, mailboxes []string) bool {
	if r.context().Err() != nil {
		return false
	}
	mailboxes = uniqueMailboxes(mailboxes)
	if len(mailboxes) == 0 {
		return false
	}
	started := false
	for _, mailbox := range mailboxes {
		names := []string{mailbox}
		keys, reserved := r.reserveOrQueueMailboxes(userID, names)
		if !reserved {
			continue
		}
		started = true
		go func(names []string, keys []string) {
			r.runReservedMailboxes(userID, names, keys)
			r.refreshGenerationRecoveryGateForUser(r.context(), userID)
			r.RefreshSenderStats(userID)
			r.StartAttachmentIndex(userID)
		}(names, keys)
	}
	return started
}

// RefreshSenderStats rebuilds the precomputed best-match sender boosts after
// sync or local mailbox maintenance has changed message/read-state data.
func (r *Runner) RefreshSenderStats(userID int64) {
	if r == nil || r.Service == nil || r.Service.Store == nil || userID <= 0 {
		return
	}
	r.mu.Lock()
	if r.generationRecoveryGatedLocked(userID) || r.ordinaryMailboxSyncRunningLocked(userID) ||
		r.attachmentDone[userID] != nil ||
		r.senderStatsRunning[userID] {
		r.senderStatsPending[userID] = true
		r.mu.Unlock()
		return
	}
	r.senderStatsRunning[userID] = true
	r.startWorkActivityLocked(runnerUserWorkActivityKey(runnerWorkSenderStats, userID), runnerWorkActivity{
		kind:   runnerWorkSenderStats,
		userID: userID,
	})
	done := make(chan struct{})
	r.senderStatsDone[userID] = done
	startEpoch := r.generationRecoveryEpoch[userID]
	timeout := r.senderStatsTimeout
	if timeout <= 0 {
		timeout = senderStatsRefreshTimeout
	}
	refreshCtx, cancel := context.WithTimeout(r.context(), timeout)
	r.senderStatsCancels[userID] = cancel
	delete(r.senderStatsPending, userID)
	r.mu.Unlock()

	err := r.refreshReadSenderStatsForUser(refreshCtx, userID)
	cancel()
	r.mu.Lock()
	delete(r.senderStatsRunning, userID)
	delete(r.senderStatsCancels, userID)
	r.finishWorkActivityLocked(runnerUserWorkActivityKey(runnerWorkSenderStats, userID))
	delete(r.senderStatsDone, userID)
	if err != nil || r.generationRecoveryGatedLocked(userID) || r.generationRecoveryEpoch[userID] != startEpoch {
		r.senderStatsPending[userID] = true
	}
	rerun := err == nil && r.senderStatsPending[userID] && !r.generationRecoveryGatedLocked(userID) &&
		!r.ordinaryMailboxSyncRunningLocked(userID)
	resumeAttachments := err == nil && !r.senderStatsPending[userID] && r.attachmentResumeAfterStats[userID] &&
		!r.generationRecoveryGatedLocked(userID) && !r.ordinaryMailboxSyncRunningLocked(userID)
	r.mu.Unlock()
	close(done)
	if err != nil {
		log.Printf("refresh sender stats user_id=%d: %v", userID, err)
	}
	if rerun && r.context().Err() == nil {
		go r.RefreshSenderStats(userID)
	} else if resumeAttachments && r.context().Err() == nil {
		r.StartAttachmentIndex(userID)
	}
}

func (r *Runner) refreshReadSenderStatsForUser(ctx context.Context, userID int64) error {
	if r.refreshSenderStatsForUser != nil {
		return r.refreshSenderStatsForUser(ctx, userID)
	}
	return r.Service.Store.RefreshReadSenderStatsForUser(ctx, userID)
}

func (r *Runner) indexPendingAttachmentsForUser(ctx context.Context, userID int64, limit int) (int, error) {
	if r.indexAttachmentsForUser != nil {
		return r.indexAttachmentsForUser(ctx, userID, limit)
	}
	return r.Service.IndexPendingAttachmentsForUser(ctx, userID, limit)
}

func (r *Runner) runMailboxes(userID int64, mailboxes []string) bool {
	if r.context().Err() != nil {
		return false
	}
	keys, ok := r.reserveMailboxes(userID, mailboxes)
	if !ok {
		return false
	}
	r.runReservedMailboxes(userID, mailboxes, keys)
	return true
}

// reserveMailboxes is the concurrency gate for user-level folder jobs. It claims
// a broad user/mailbox key and also checks account-qualified keys, because this
// job will sync the requested mailbox name across every account for the user.
func (r *Runner) reserveMailboxes(userID int64, mailboxes []string) ([]string, bool) {
	keys := mailboxKeys(userID, mailboxes)
	if !r.lockAfterExclusiveWriters(userID) {
		return nil, false
	}
	defer r.mu.Unlock()
	if r.generationRecoveryGatedLocked(userID) {
		r.deferGenerationRecoveryMailboxesLocked(userID, mailboxes)
		return nil, false
	}
	for _, mailbox := range mailboxes {
		if r.mailboxReservedByAnyAccountLocked(userID, mailbox) {
			return nil, false
		}
	}
	for _, key := range keys {
		r.mailboxRunning[key] = true
	}
	r.startMailboxWorkActivitiesLocked(userID, 0, mailboxes, keys, runnerWorkMailboxSync)
	r.pauseAttachmentIndexLocked(userID)
	return keys, true
}

func (r *Runner) reserveAccountMailboxes(userID, accountID int64, mailboxes []string) ([]string, bool) {
	keys := accountMailboxKeys(userID, accountID, mailboxes)
	if !r.lockAfterExclusiveWriters(userID) {
		return nil, false
	}
	defer r.mu.Unlock()
	// An account-wide pass already owns this tenant's sync plan. Let it finish
	// instead of starting a competing INBOX poll for another account: they share
	// the same per-user SQLite writer even though their IMAP servers differ.
	if r.autoRunning[userID] {
		return nil, false
	}
	if r.generationRecoveryAccountMailboxesGatedLocked(userID, accountID, mailboxes) {
		r.deferGenerationRecoveryAccountMailboxesLocked(userID, accountID, mailboxes)
		return nil, false
	}
	for _, mailbox := range mailboxes {
		if r.accountMailboxReservedLocked(userID, accountID, mailbox) {
			return nil, false
		}
	}
	for _, key := range keys {
		r.mailboxRunning[key] = true
	}
	r.startMailboxWorkActivitiesLocked(userID, accountID, mailboxes, keys, runnerWorkMailboxSync)
	r.clearDeferredGenerationRecoveryAccountMailboxesLocked(userID, accountID, mailboxes)
	r.pauseAttachmentIndexLocked(userID)
	return keys, true
}

func (r *Runner) reserveAccountMailboxesForMaintenance(userID, accountID int64, mailboxes []string, searchOnly bool) ([]string, bool) {
	keys := accountMailboxKeys(userID, accountID, mailboxes)
	if !r.lockAfterExclusiveWriters(userID) {
		return nil, false
	}
	defer r.mu.Unlock()
	if r.generationRecoveryGatedLocked(userID) {
		return nil, false
	}
	for _, mailbox := range mailboxes {
		if r.accountMailboxReservedLocked(userID, accountID, mailbox) {
			return nil, false
		}
	}
	for _, key := range keys {
		r.mailboxRunning[key] = true
	}
	kind := runnerWorkMailboxMaintenance
	if searchOnly {
		kind = runnerWorkMailboxSearchMaintenance
	}
	r.startMailboxWorkActivitiesLocked(userID, accountID, mailboxes, keys, kind)
	r.pauseAttachmentIndexLocked(userID)
	return keys, true
}

func (r *Runner) reserveOrQueueMailboxes(userID int64, mailboxes []string) ([]string, bool) {
	keys := mailboxKeys(userID, mailboxes)
	if !r.lockAfterSenderStats(userID) {
		return nil, false
	}
	defer r.mu.Unlock()
	if r.generationRecoveryGatedLocked(userID) {
		r.deferGenerationRecoveryMailboxesLocked(userID, mailboxes)
		return nil, false
	}
	if r.foregroundRunning[userID] > 0 {
		r.deferForegroundMailboxesLocked(userID, mailboxes)
		return nil, false
	}
	for _, mailbox := range mailboxes {
		if !r.mailboxReservedByAnyAccountLocked(userID, mailbox) {
			continue
		}
		for _, key := range keys {
			r.mailboxPending[key] = true
		}
		return nil, false
	}
	for _, key := range keys {
		r.mailboxRunning[key] = true
	}
	r.startMailboxWorkActivitiesLocked(userID, 0, mailboxes, keys, runnerWorkMailboxSync)
	r.pauseAttachmentIndexLocked(userID)
	return keys, true
}

func (r *Runner) reserveOrQueueAccountMailboxes(userID, accountID int64, mailboxes []string) ([]string, bool) {
	keys := accountMailboxKeys(userID, accountID, mailboxes)
	if !r.lockAfterSenderStats(userID) {
		return nil, false
	}
	defer r.mu.Unlock()
	if r.autoRunning[userID] {
		r.deferAutoAccountMailboxesLocked(userID, accountID, mailboxes)
		return nil, false
	}
	if r.generationRecoveryAccountMailboxesGatedLocked(userID, accountID, mailboxes) {
		r.deferGenerationRecoveryAccountMailboxesLocked(userID, accountID, mailboxes)
		return nil, false
	}
	if r.foregroundRunning[userID] > 0 {
		r.deferForegroundAccountMailboxesLocked(userID, accountID, mailboxes)
		return nil, false
	}
	for _, mailbox := range mailboxes {
		if !r.accountMailboxReservedLocked(userID, accountID, mailbox) {
			continue
		}
		if r.accountMailboxPending == nil {
			r.accountMailboxPending = map[string]bool{}
		}
		for _, key := range keys {
			r.accountMailboxPending[key] = true
		}
		return nil, false
	}
	for _, key := range keys {
		r.mailboxRunning[key] = true
	}
	r.startMailboxWorkActivitiesLocked(userID, accountID, mailboxes, keys, runnerWorkMailboxSync)
	r.clearDeferredGenerationRecoveryAccountMailboxesLocked(userID, accountID, mailboxes)
	r.pauseAttachmentIndexLocked(userID)
	return keys, true
}

func (r *Runner) markPending(userID int64, mailboxes []string) {
	keys := mailboxKeys(userID, mailboxes)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		r.mailboxPending[key] = true
	}
}

func (r *Runner) deferForegroundMailboxesLocked(userID int64, mailboxes []string) {
	if r.foregroundDeferredBoxes == nil {
		r.foregroundDeferredBoxes = map[int64]map[string]string{}
	}
	if r.foregroundDeferredBoxes[userID] == nil {
		r.foregroundDeferredBoxes[userID] = map[string]string{}
	}
	for _, mailbox := range uniqueMailboxes(mailboxes) {
		r.foregroundDeferredBoxes[userID][strings.ToLower(mailbox)] = mailbox
	}
}

func (r *Runner) deferForegroundAccountMailboxesLocked(userID, accountID int64, mailboxes []string) {
	if r.foregroundDeferredAccts == nil {
		r.foregroundDeferredAccts = map[int64]map[string]deferredAccountMailbox{}
	}
	if r.foregroundDeferredAccts[userID] == nil {
		r.foregroundDeferredAccts[userID] = map[string]deferredAccountMailbox{}
	}
	for _, mailbox := range uniqueMailboxes(mailboxes) {
		key := accountMailboxKey(userID, accountID, mailbox)
		r.foregroundDeferredAccts[userID][key] = deferredAccountMailbox{accountID: accountID, mailbox: mailbox}
	}
}

func (r *Runner) deferAutoAccountMailboxesLocked(userID, accountID int64, mailboxes []string) {
	if r.autoDeferredAccts == nil {
		r.autoDeferredAccts = map[int64]map[string]deferredAccountMailbox{}
	}
	if r.autoDeferredAccts[userID] == nil {
		r.autoDeferredAccts[userID] = map[string]deferredAccountMailbox{}
	}
	for _, mailbox := range uniqueMailboxes(mailboxes) {
		key := accountMailboxKey(userID, accountID, mailbox)
		r.autoDeferredAccts[userID][key] = deferredAccountMailbox{accountID: accountID, mailbox: mailbox}
	}
}

func (r *Runner) takeAutoDeferredAccountMailboxesLocked(userID int64) []deferredAccountMailbox {
	deferred := r.autoDeferredAccts[userID]
	out := make([]deferredAccountMailbox, 0, len(deferred))
	for _, request := range deferred {
		out = append(out, request)
	}
	delete(r.autoDeferredAccts, userID)
	return out
}

func (r *Runner) takeForegroundReplayLocked(userID int64) generationRecoveryReplay {
	replay := generationRecoveryReplay{userID: userID, auto: r.foregroundDeferredAuto[userID]}
	for _, mailbox := range r.foregroundDeferredBoxes[userID] {
		replay.mailboxes = append(replay.mailboxes, mailbox)
	}
	for _, request := range r.foregroundDeferredAccts[userID] {
		replay.accountMailboxes = append(replay.accountMailboxes, request)
	}
	delete(r.foregroundDeferredBoxes, userID)
	delete(r.foregroundDeferredAccts, userID)
	delete(r.foregroundDeferredAuto, userID)
	return r.coalesceGenerationRecoveryReplay(replay)
}

// runReservedMailboxes performs the already-reserved sync and then checks for
// priority reruns that arrived while it was busy.
func (r *Runner) runReservedMailboxes(userID int64, mailboxes []string, keys []string) error {
	defer func() {
		r.mu.Lock()
		var rerun []string
		accountReruns := map[int64][]string{}
		seen := map[string]bool{}
		for i, key := range keys {
			delete(r.mailboxRunning, key)
			r.finishWorkActivityLocked(runnerMailboxWorkActivityKey(key))
			if r.mailboxPending[key] {
				delete(r.mailboxPending, key)
				name := mailboxes[i]
				lower := strings.ToLower(strings.TrimSpace(name))
				if lower != "" && !seen[lower] {
					seen[lower] = true
					rerun = append(rerun, name)
				}
				r.takeAccountPendingForMailboxLocked(userID, name)
				continue
			}
			name := mailboxes[i]
			for accountID := range r.takeAccountPendingForMailboxLocked(userID, name) {
				accountReruns[accountID] = append(accountReruns[accountID], name)
			}
		}
		r.mu.Unlock()
		if len(rerun) > 0 && r.context().Err() == nil {
			r.StartPriorityMailboxes(userID, rerun)
		}
		for accountID, names := range accountReruns {
			if r.context().Err() != nil {
				break
			}
			r.QueueAccountMailboxes(userID, accountID, names)
		}
	}()
	ctx, finishContext := r.ordinaryMailboxContext(userID, keys, false)
	defer finishContext()
	timeout := ordinaryMailboxSyncTurnTimeout
	if generationRecoveryInboxBypassAllowed(mailboxes) {
		timeout = r.liveInboxSyncTimeout
		if timeout <= 0 {
			timeout = liveInboxSyncTurnTimeout
		}
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.setMailboxWorkActivitiesPhase(keys, "waiting_attachment")
	if err := r.waitForAttachmentIndex(ctx, userID); err != nil {
		return err
	}
	r.setMailboxWorkActivitiesPhase(keys, "syncing")
	diagnostics := newSyncRunDiagnostics()
	if _, err := r.Service.syncUserWithOptions(ctx, userID, mailboxes, syncAccountOptions{
		deferMaintenance: func() bool { return r.mailboxGenerationRecoveryPending(userID) },
		runDiagnostics:   diagnostics,
		onRunStarted:     func(runID int64) { r.registerSyncRunControl(userID, runID, keys, diagnostics) },
		onRunFinished:    r.unregisterSyncRunControl,
	}); err != nil {
		log.Printf("sync user_id=%d mailboxes=%s: %v", userID, strings.Join(mailboxes, ","), err)
		return err
	}
	return nil
}

func (r *Runner) takeAccountPendingForMailboxLocked(userID int64, mailbox string) map[int64]bool {
	out := map[int64]bool{}
	mailbox = strings.ToLower(strings.TrimSpace(mailbox))
	for key := range r.accountMailboxPending {
		pendingUserID, accountID, pendingMailbox, ok := parseAccountMailboxKey(key)
		if !ok || pendingUserID != userID || pendingMailbox != mailbox {
			continue
		}
		delete(r.accountMailboxPending, key)
		out[accountID] = true
	}
	return out
}

func (r *Runner) runReservedAccountMailboxes(userID, accountID int64, mailboxes []string, keys []string) error {
	defer func() {
		r.startAccountMailboxReruns(userID, accountID, r.releaseAccountMailboxReservations(userID, accountID, mailboxes, keys))
	}()
	ctx, finishContext := r.ordinaryMailboxContext(userID, keys,
		generationRecoveryInboxBypassAllowed(mailboxes))
	defer finishContext()
	timeout := ordinaryMailboxSyncTurnTimeout
	if generationRecoveryInboxBypassAllowed(mailboxes) {
		timeout = r.liveInboxSyncTimeout
		if timeout <= 0 {
			timeout = liveInboxSyncTurnTimeout
		}
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.setMailboxWorkActivitiesPhase(keys, "waiting_attachment")
	if err := r.waitForAttachmentIndex(ctx, userID); err != nil {
		return err
	}
	r.setMailboxWorkActivitiesPhase(keys, "syncing")
	r.mu.Lock()
	deferPendingFlags := r.generationRecoveryGatedLocked(userID) || r.generationRecoveryRuns[userID]
	r.mu.Unlock()
	diagnostics := newSyncRunDiagnostics()
	if _, err := r.Service.syncUserAccountMailboxes(ctx, userID, accountID, mailboxes,
		syncAccountOptions{
			deferPendingFlags: deferPendingFlags,
			deferMaintenance:  func() bool { return r.mailboxGenerationRecoveryPending(userID) },
			runDiagnostics:    diagnostics,
			onRunStarted:      func(runID int64) { r.registerSyncRunControl(userID, runID, keys, diagnostics) },
			onRunFinished:     r.unregisterSyncRunControl,
		}); err != nil {
		log.Printf("sync user_id=%d account_id=%d mailboxes=%s: %v", userID, accountID, strings.Join(mailboxes, ","), err)
		return err
	}
	return nil
}

type accountMailboxReruns struct {
	global  []string
	account []string
}

func (r *Runner) releaseAccountMailboxReservations(userID, accountID int64, mailboxes []string, keys []string) accountMailboxReruns {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		delete(r.mailboxRunning, key)
	}
	r.finishMailboxWorkActivitiesLocked(keys)
	var reruns accountMailboxReruns
	seenGlobal := map[string]bool{}
	seenAccount := map[string]bool{}
	for _, mailbox := range mailboxes {
		name := strings.TrimSpace(mailbox)
		lower := strings.ToLower(name)
		if lower == "" {
			continue
		}
		globalKey := mailboxKey(userID, name)
		accountKey := accountMailboxKey(userID, accountID, name)
		if r.mailboxPending[globalKey] {
			delete(r.mailboxPending, globalKey)
			delete(r.accountMailboxPending, accountKey)
			if !seenGlobal[lower] {
				seenGlobal[lower] = true
				reruns.global = append(reruns.global, name)
			}
			continue
		}
		if r.accountMailboxPending[accountKey] {
			delete(r.accountMailboxPending, accountKey)
			if !seenAccount[lower] {
				seenAccount[lower] = true
				reruns.account = append(reruns.account, name)
			}
		}
	}
	return reruns
}

func (r *Runner) startAccountMailboxReruns(userID, accountID int64, reruns accountMailboxReruns) {
	if r.context().Err() != nil {
		return
	}
	if len(reruns.global) > 0 {
		r.StartPriorityMailboxes(userID, reruns.global)
	}
	if len(reruns.account) > 0 {
		r.QueueAccountMailboxes(userID, accountID, reruns.account)
	}
}

const attachmentIndexBatchSize = 25

// BeginForegroundOperation asks resumable background writers to checkpoint
// while a direct user operation, such as send or move, changes remote and local
// mail state. The returned function is idempotent and replays interrupted work
// when the user is otherwise idle. Explicit maintenance is allowed to finish,
// except search-only rebuilds, which converge with concurrent foreground work
// and therefore do not block it.
func (r *Runner) BeginForegroundOperation(ctx context.Context, userID int64) (func(), error) {
	if r == nil || userID <= 0 {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var attachmentDone, senderStatsDone <-chan struct{}
	for {
		r.mu.Lock()
		if r.foregroundRunning[userID] == 0 {
			r.foregroundRunning[userID] = 1
			r.startWorkActivityLocked(runnerUserWorkActivityKey(runnerWorkForeground, userID), runnerWorkActivity{
				kind:   runnerWorkForeground,
				userID: userID,
			})
			if r.foregroundDone == nil {
				r.foregroundDone = map[int64]chan struct{}{}
			}
			r.foregroundDone[userID] = make(chan struct{})
			attachmentDone = r.attachmentDone[userID]
			senderStatsDone = r.senderStatsDone[userID]
			r.preemptResumableWorkForForegroundLocked(userID)
			r.mu.Unlock()
			break
		}
		done := r.foregroundDone[userID]
		r.mu.Unlock()
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return func() {}, ctx.Err()
		}
	}

	var once sync.Once
	finish := func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.foregroundRunning, userID)
			r.finishWorkActivityLocked(runnerUserWorkActivityKey(runnerWorkForeground, userID))
			done := r.foregroundDone[userID]
			delete(r.foregroundDone, userID)
			replay := r.takeForegroundReplayLocked(userID)
			// A foreground turn pauses derived workers but does not itself prove
			// that local mail changed. Resume work requested or interrupted while
			// the turn was held; the queued mailbox refresh will schedule fresh
			// derived maintenance after it mirrors an actual remote change.
			replay.senderStats = r.senderStatsPending[userID]
			replay.attachments = r.attachmentPending[userID]
			r.mu.Unlock()
			r.refreshGenerationRecoveryGateForUser(r.context(), userID)
			r.wakeMailboxGenerationRebuildRecovery()
			// Schedule plugin-requested destination refreshes before waking the
			// next foreground holder. The new holder will then wait for the
			// reserved mailbox job instead of repeatedly starving it.
			r.scheduleGenerationRecoveryWorkOutsideGate(replay)
			if done != nil {
				close(done)
			}
		})
	}
	if err := r.waitForForegroundYield(ctx, userID, attachmentDone, senderStatsDone); err != nil {
		// Return to the caller promptly, but retain the foreground barrier until
		// canceled workers have checkpointed and handed their replay state back.
		// Releasing it immediately can consume an incomplete replay and strand
		// the remainder when an acquisition timeout races worker cleanup. The
		// wait is bounded: if a wedged worker never yields, the barrier is
		// force-released so one stuck plugin cannot hold the tenant forever.
		go func() {
			if yieldErr := r.waitForForegroundYield(r.context(), userID, attachmentDone, senderStatsDone); yieldErr != nil {
				log.Printf("foreground barrier force-released user_id=%d error_type=%T", userID, yieldErr)
			}
			finish()
		}()
		return func() {}, err
	}
	return finish, nil
}

// foregroundYieldWaitTimeout bounds how long waitForForegroundYield polls for
// resumable workers to checkpoint. Without a cap a single hung worker (for
// example an in-process plugin ignoring its context) would keep the foreground
// barrier — and every future sync admission behind it — permanently.
const foregroundYieldWaitTimeout = 10 * time.Minute

func (r *Runner) waitForForegroundYield(ctx context.Context, userID int64, maintenanceDone ...<-chan struct{}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, done := range maintenanceDone {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	started := time.Now()
	for {
		r.mu.Lock()
		// A recovery turn can install its cancellation after reserving its
		// mailbox. Reissue preemption while waiting to close that small race.
		r.preemptResumableWorkForForegroundLocked(userID)
		mailboxWriterRunning := r.mailboxWriterRunningLocked(userID)
		r.mu.Unlock()
		if !mailboxWriterRunning {
			return ctx.Err()
		}
		if time.Since(started) >= foregroundYieldWaitTimeout {
			return fmt.Errorf("foreground yield still blocked after %s: %w", foregroundYieldWaitTimeout, context.DeadlineExceeded)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// StartAttachmentIndex runs after message sync so newly fetched raw .eml data can
// be mined for attachment text and then discarded according to retention rules.
// It yields to mailbox jobs and explicit user operations, then drains small
// batches while the user remains idle.
func (r *Runner) StartAttachmentIndex(userID int64) bool {
	if r == nil || r.Service == nil || r.Service.Store == nil || userID <= 0 || r.context().Err() != nil {
		return false
	}
	now := time.Now()
	r.Service.releaseDueAttachmentIndexWakes(userID, now)
	if _, blocked := r.Service.attachmentIndexContinuationBlocked(userID, now); blocked {
		r.mu.Lock()
		r.attachmentPending[userID] = true
		r.mu.Unlock()
		r.scheduleNextAttachmentIndexRetry(userID)
		return false
	}
	key := mailboxKey(userID, "__attachments__")
	r.mu.Lock()
	if r.senderStatsRunning[userID] {
		r.attachmentPending[userID] = true
		r.attachmentResumeAfterStats[userID] = true
		r.mu.Unlock()
		return false
	}
	if r.userForegroundRunningLocked(userID) || r.mailboxRunning[key] {
		r.attachmentPending[userID] = true
		r.mu.Unlock()
		return false
	}
	r.cancelAttachmentRetryTimerLocked(userID)
	ctx, cancel := context.WithCancel(r.context())
	done := make(chan struct{})
	r.mailboxRunning[key] = true
	r.startMailboxWorkActivitiesLocked(userID, 0, []string{"__attachments__"}, []string{key}, runnerWorkAttachmentIndex)
	r.attachmentCancels[userID] = cancel
	r.attachmentDone[userID] = done
	delete(r.attachmentPending, userID)
	delete(r.attachmentResumeAfterStats, userID)
	r.mu.Unlock()

	go func() {
		drainMore := false
		indexFailed := false
		defer func() {
			r.mu.Lock()
			delete(r.mailboxRunning, key)
			r.finishWorkActivityLocked(runnerMailboxWorkActivityKey(key))
			delete(r.attachmentCancels, userID)
			delete(r.attachmentDone, userID)
			if drainMore || indexFailed {
				r.attachmentPending[userID] = true
			}
			handoffSenderStats := r.senderStatsPending[userID]
			if handoffSenderStats && r.attachmentPending[userID] && !indexFailed {
				r.attachmentResumeAfterStats[userID] = true
			}
			restart := r.attachmentPending[userID] && !indexFailed && !handoffSenderStats &&
				!r.senderStatsRunning[userID] && !r.userForegroundRunningLocked(userID)
			r.mu.Unlock()
			close(done)
			r.refreshGenerationRecoveryGateForUser(r.context(), userID)
			r.wakeMailboxGenerationRebuildRecovery()
			if !indexFailed {
				r.scheduleNextAttachmentIndexRetry(userID)
			}
			if handoffSenderStats && r.context().Err() == nil {
				r.RefreshSenderStats(userID)
			} else if restart {
				r.StartAttachmentIndex(userID)
			}
		}()
		n, err := r.indexPendingAttachmentsForUser(ctx, userID, attachmentIndexBatchSize)
		if err != nil {
			indexFailed = true
			if ctx.Err() == nil {
				log.Printf("attachment index user_id=%d: %v", userID, err)
			}
			return
		}
		drainMore = n == attachmentIndexBatchSize
		if n > 0 {
			log.Printf("attachment index user_id=%d processed=%d", userID, n)
		}
	}()
	return true
}

func (r *Runner) pauseAttachmentIndexLocked(userID int64) {
	if cancel := r.attachmentCancels[userID]; cancel != nil {
		r.attachmentPending[userID] = true
		cancel()
	}
}

// waitForAttachmentIndex is called only after the caller has reserved ordinary
// mailbox work (or set autoRunning), so no replacement attachment worker can
// start between this snapshot and the wait. Cancellation can interrupt remote
// fetches, but a worker may still be finishing SQLite metadata or a Bleve batch.
func (r *Runner) waitForAttachmentIndex(ctx context.Context, userID int64) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = r.context()
	}
	r.mu.Lock()
	done := r.attachmentDone[userID]
	r.mu.Unlock()
	if done == nil {
		return ctx.Err()
	}
	timeout := r.attachmentYieldTimeout
	if timeout <= 0 {
		timeout = attachmentIndexYieldTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		// The worker has already received cancellation. Any later SQLite or
		// search-index operation receives that canceled context, while any
		// uncancellable parsing still in flight only computes derived data.
		log.Printf("attachment index user_id=%d did not yield after %s; continuing foreground sync", userID, timeout)
		return nil
	}
}

func (r *Runner) userForegroundRunningLocked(userID int64) bool {
	if r.generationRecoveryGatedLocked(userID) || r.autoRunning[userID] || r.foregroundRunning[userID] > 0 {
		return true
	}
	prefix := fmt.Sprintf("%d:", userID)
	attachmentKey := mailboxKey(userID, "__attachments__")
	for key := range r.mailboxRunning {
		if key != attachmentKey && strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// IsRunning reports foreground sync activity for the user, excluding the private
// attachment-index sentinel so the chrome does not look stuck after mail fetches.
func (r *Runner) IsRunning(userID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.autoRunning[userID] || r.foregroundRunning[userID] > 0 {
		return true
	}
	prefix := fmt.Sprintf("%d:", userID)
	for key := range r.mailboxRunning {
		if key == mailboxKey(userID, "__attachments__") {
			continue
		}
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// IsMailboxRunning reports whether any user-level or account-qualified mailbox
// sync reservation is active for this folder name.
func (r *Runner) IsMailboxRunning(userID int64, mailbox string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mailboxReservedByAnyAccountLocked(userID, mailbox)
}

// IsAccountMailboxRunning reports whether a broad user-level reservation or the
// exact account/mailbox reservation is active.
func (r *Runner) IsAccountMailboxRunning(userID, accountID int64, mailbox string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accountMailboxReservedLocked(userID, accountID, mailbox)
}

// AccountMailboxBlockReason describes why an account-qualified mailbox request
// was declined. Poll/IDLE logs use this after StartAccountMailboxes returns false
// so a tenant-wide recovery gate is not mislabeled as two stuck Inbox jobs.
func (r *Runner) AccountMailboxBlockReason(userID, accountID int64, mailbox string) string {
	if r == nil {
		return "sync runner is unavailable"
	}
	if err := r.context().Err(); err != nil {
		return "sync runner is stopping"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if activity, ok := r.generationRecoveryActive[userID]; ok {
		if activity.accountID == accountID &&
			strings.EqualFold(strings.TrimSpace(activity.mailbox), strings.TrimSpace(mailbox)) {
			elapsed := time.Since(activity.startedAt).Round(time.Second)
			if elapsed < 0 {
				elapsed = 0
			}
			reason := fmt.Sprintf("mailbox generation recovery active account_id=%d mailbox=%q elapsed=%s",
				activity.accountID, activity.mailbox, elapsed)
			if activity.diagnostics != nil {
				reason += " " + activity.diagnostics.snapshot().status(time.Now())
			}
			return reason
		}
		if !generationRecoveryInboxBypassAllowed([]string{mailbox}) {
			return "mailbox generation recovery active; unrelated folder sync is serialized"
		}
		if r.generationRecoveryOrdinaryWriterRunningLocked(userID) {
			return "mailbox generation recovery active; another live Inbox sync is already running"
		}
	}
	if r.generationRecoveryReplay[userID] {
		if !generationRecoveryInboxBypassAllowed([]string{mailbox}) {
			return "mailbox generation recovery replay active; unrelated folder sync is serialized"
		}
		if r.generationRecoveryOrdinaryWriterRunningLocked(userID) {
			return "mailbox generation recovery replay active; another mailbox sync is already running"
		}
	}
	if r.generationRecoveryUsers[userID] {
		if !r.generationRecoveryKnown[userID] {
			return "mailbox generation recovery pending account scan"
		}
		if targets, known := r.generationRecoveryTargets[userID]; known &&
			targets[accountMailboxKey(userID, accountID, mailbox)] {
			return "mailbox generation recovery pending for this folder"
		}
		if _, known := r.generationRecoveryTargets[userID]; !known && r.generationRecoveryAccounts[userID][accountID] {
			return "mailbox generation recovery pending for this account"
		}
		if !generationRecoveryInboxBypassAllowed([]string{mailbox}) {
			return "mailbox generation recovery pending; unrelated folder sync is serialized"
		}
		if r.generationRecoveryOrdinaryWriterRunningLocked(userID) {
			return "mailbox generation recovery pending; another live Inbox sync is already running"
		}
	}
	if r.accountMailboxReservedLocked(userID, accountID, mailbox) {
		return "mailbox sync already running"
	}
	if r.autoRunning[userID] {
		return "account-wide sync already running"
	}
	if r.foregroundRunning[userID] > 0 {
		return "foreground mail operation active"
	}
	return "sync request was not started"
}

func uniqueMailboxes(mailboxes []string) []string {
	out := make([]string, 0, len(mailboxes))
	seen := map[string]bool{}
	for _, mailbox := range mailboxes {
		name := strings.TrimSpace(mailbox)
		key := strings.ToLower(name)
		if name != "" && !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
	}
	return out
}

func (r *Runner) mailboxReservedByAnyAccountLocked(userID int64, mailbox string) bool {
	for key := range r.mailboxRunning {
		if reservationKeyMatchesMailbox(key, userID, mailbox) {
			return true
		}
	}
	return false
}

func (r *Runner) accountMailboxReservedLocked(userID, accountID int64, mailbox string) bool {
	return r.mailboxRunning[mailboxKey(userID, mailbox)] || r.mailboxRunning[accountMailboxKey(userID, accountID, mailbox)]
}

func reservationKeyMatchesMailbox(key string, userID int64, mailbox string) bool {
	mailbox = strings.ToLower(strings.TrimSpace(mailbox))
	if mailbox == "" {
		return false
	}
	if key == mailboxKey(userID, mailbox) {
		return true
	}
	prefix := fmt.Sprintf("%d:", userID)
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	rest := strings.TrimPrefix(key, prefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return false
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		return false
	}
	return parts[1] == mailbox
}

func mailboxKey(userID int64, mailbox string) string {
	return fmt.Sprintf("%d:%s", userID, strings.ToLower(strings.TrimSpace(mailbox)))
}

func mailboxKeys(userID int64, mailboxes []string) []string {
	keys := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		keys = append(keys, mailboxKey(userID, mailbox))
	}
	return keys
}

func accountMailboxKey(userID, accountID int64, mailbox string) string {
	return fmt.Sprintf("%d:%d:%s", userID, accountID, strings.ToLower(strings.TrimSpace(mailbox)))
}

func parseAccountMailboxKey(key string) (int64, int64, string, bool) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || strings.TrimSpace(parts[2]) == "" {
		return 0, 0, "", false
	}
	userID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || userID <= 0 {
		return 0, 0, "", false
	}
	accountID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || accountID <= 0 {
		return 0, 0, "", false
	}
	return userID, accountID, strings.ToLower(strings.TrimSpace(parts[2])), true
}

func accountMailboxKeys(userID, accountID int64, mailboxes []string) []string {
	keys := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		keys = append(keys, accountMailboxKey(userID, accountID, mailbox))
	}
	return keys
}
