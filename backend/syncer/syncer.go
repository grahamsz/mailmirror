// File overview: Core IMAP sync service. It coordinates mailbox discovery, message fetch, blob storage, parsing, database writes, and search indexing.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/plugins"
	"rolltop/backend/search"
	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
)

// MailboxInfo is the mailbox discovery record returned by a Fetcher. Attributes
// preserve IMAP LIST special-use metadata so discovery can distinguish folders
// such as Junk even when the remote name is provider-specific.
type MailboxInfo struct {
	Name       string
	Attributes []string
}

const (
	// MaxTrainingCandidateCount bounds one read-only IMAP training scan. Callers
	// can make additional explicit requests, but a single operation cannot make
	// an unbounded mailbox download.
	MaxTrainingCandidateCount = 5000
	// MaxTrainingCandidateBodyBytes is the protocol-level BODY.PEEK partial size
	// used for personal spam-training candidates.
	MaxTrainingCandidateBodyBytes = 512 * 1024
)

// TrainingCandidateQuery describes a bounded, read-only mailbox sample. IMAP
// evaluates date criteria at day precision; callers that need exact cutoffs
// should also check the returned InternalDate.
type TrainingCandidateQuery struct {
	Since    time.Time
	Before   time.Time
	SeenOnly bool
	Limit    int
}

// TrainingCandidateMetadata is the header-level information used to preview
// and filter a training sample before any raw message content is downloaded.
type TrainingCandidateMetadata struct {
	Mailbox      string
	UID          uint32
	Date         time.Time
	InternalDate time.Time
	Size         int64
	Flags        []string
	Subject      string
	From         []string
	To           []string
	MessageID    string
}

// TrainingCandidateSearch reports the full number of remote matches and the
// newest bounded metadata sample selected by the query limit.
type TrainingCandidateSearch struct {
	Matched    int
	Candidates []TrainingCandidateMetadata
}

// TrainingCandidate is an ephemeral IMAP message sample. Raw is never larger
// than MaxTrainingCandidateBodyBytes and is not persisted by the fetcher.
type TrainingCandidate struct {
	FetchedMessage
	Truncated bool
}

// TrainingCandidateFetcher is an optional read-only capability used by
// personal classifiers. It intentionally remains separate from Fetcher so
// ordinary synchronization implementations and test doubles need not support
// training scans.
type TrainingCandidateFetcher interface {
	SearchTrainingCandidates(ctx context.Context, account store.MailAccount, mailbox string, query TrainingCandidateQuery) (TrainingCandidateSearch, error)
	FetchTrainingCandidates(ctx context.Context, account store.MailAccount, mailbox string, uids []uint32, handle func(TrainingCandidate) error) error
}

// MailboxStatus mirrors IMAP STATUS counters used for planning and UI progress.
type MailboxStatus struct {
	Messages    uint32
	Unseen      uint32
	UIDNext     uint32
	UIDValidity uint32
}

// MailboxPlan combines a mailbox name, remote status, last local UID, and estimated pending work.
type MailboxPlan struct {
	Name    string
	Status  MailboxStatus
	LastUID uint32
	Pending int
}

// FetchedMessage is a raw message body plus IMAP metadata streamed from Fetcher to Service.
type FetchedMessage struct {
	Mailbox     string
	UID         uint32
	UIDValidity uint32
	// AppendUIDAuthoritative is true only when UID and UIDValidity came from
	// the server's UIDPLUS APPENDUID response for this append operation.
	AppendUIDAuthoritative bool
	Date                   time.Time
	InternalDate           time.Time
	Size                   int64
	Flags                  []string
	Raw                    []byte
	// Oversized marks messages whose remote RFC822 size exceeded the mirror
	// limit. Raw holds only the leading header block; callers must persist
	// metadata without a local raw blob and must not attempt full-body
	// hydration for them.
	Oversized bool
}

// Fetcher is the narrow IMAP boundary used by sync. Keeping this interface here
// lets tests substitute fake servers while the real imapclient package handles
// protocol details and password decryption.
type Fetcher interface {
	ListMailboxes(ctx context.Context, account store.MailAccount) ([]MailboxInfo, error)
	MailboxStatus(ctx context.Context, account store.MailAccount, mailbox string) (MailboxStatus, error)
	UIDs(ctx context.Context, account store.MailAccount, mailbox string) ([]uint32, error)
	FetchMailbox(ctx context.Context, account store.MailAccount, mailbox string, afterUID uint32, handle func(FetchedMessage) error) error
	FetchMessage(ctx context.Context, account store.MailAccount, mailbox string, uid uint32) (FetchedMessage, error)
	AppendMessage(ctx context.Context, account store.MailAccount, mailbox string, raw []byte, messageID string, date time.Time) (FetchedMessage, error)
	SetSeen(ctx context.Context, account store.MailAccount, mailbox string, uid uint32, seen bool) error
	SeenUIDs(ctx context.Context, account store.MailAccount, mailbox string) ([]uint32, error)
	SetFlagged(ctx context.Context, account store.MailAccount, mailbox string, uid uint32, flagged bool) error
	FlaggedUIDs(ctx context.Context, account store.MailAccount, mailbox string) ([]uint32, error)
	MoveMessage(ctx context.Context, account store.MailAccount, sourceMailbox string, destMailbox string, uid uint32) error
}

// MailSender is the SMTP boundary used by filter-style background actions.
type MailSender interface {
	Send(ctx context.Context, account store.MailAccount, msg smtpclient.Message) ([]byte, error)
}

// Service is the sync orchestrator. It owns no goroutine scheduling itself; the
// Runner decides when work starts, then Service performs one account/mailbox sync
// against Store, Blob, Search, and Fetcher dependencies.
type Service struct {
	Store   *store.Store
	Blobs   *blob.Store
	Search  *search.Service
	Fetcher Fetcher
	Sender  MailSender

	BlobRetention                    time.Duration
	Notify                           func(userID int64)
	NotifyProgress                   func(userID int64)
	ScheduleInboxArrival             func(userID, accountID int64, due time.Time)
	NotifyRestoredState              func(userID int64)
	MailboxGenerationRecoveryStarted func(userID int64)
	// DeferMailboxGenerationRebuilds makes ordinary Runner-scheduled syncs
	// yield newly discovered generation markers to the serialized recovery
	// worker. Direct Service callers retain the synchronous behavior used by
	// maintenance tools and focused tests.
	DeferMailboxGenerationRebuilds bool
	// AllowBackgroundAttachmentHydration preserves the legacy behavior of
	// fetching uncached historical raw messages solely to enrich attachment text.
	// It is disabled in production: explicit full-text rebuilds are the opt-in
	// path for that potentially large remote operation.
	AllowBackgroundAttachmentHydration bool
	PluginDir                          string
	MasterKey                          []byte
	// BaseContext is the process-lifetime context used by detached background
	// work (large copies/moves) so shutdown cancellation reaches them. When
	// unset these operations fall back to context.Background().
	BaseContext context.Context
	// MaxMessageBytes mirrors the fetcher's mirror limit so derived workers
	// (search-index repair hydration) can skip oversized rows locally instead
	// of attempting remote hydration that is guaranteed to be refused.
	MaxMessageBytes int64

	pluginOnce     sync.Once
	pluginLoadErr  error
	backendPlugins *plugins.BackendManager

	attachmentIndexMu         sync.Mutex
	attachmentIndexCursor     map[int64]int64
	attachmentIndexRetryAfter map[attachmentIndexRetryKey]time.Time
	attachmentIndexLastPrune  time.Time
	attachmentIndexContinueAt map[int64]time.Time
	// attachmentIndexContinuationDelay is overridden only by focused tests.
	attachmentIndexContinuationDelay time.Duration
}

const (
	inlineMetadataSyncLimit               = 10000
	mailboxGenerationBlobCleanupBatchSize = 25
)

// baseContextOrDefault is the context detached background operations run under.
func (s *Service) baseContextOrDefault() context.Context {
	if s != nil && s.BaseContext != nil {
		return s.BaseContext
	}
	return context.Background()
}

func (s *Service) initBackendPlugins() error {
	if s == nil {
		return nil
	}
	s.pluginOnce.Do(func() {
		root := strings.TrimSpace(s.PluginDir)
		if root == "" {
			root = "plugins"
		}
		manifests, err := plugins.LoadManifests(filepath.Clean(root))
		if err != nil {
			s.pluginLoadErr = err
			return
		}
		s.backendPlugins = plugins.NewBackendManager(root, manifests)
	})
	return s.pluginLoadErr
}

func (s *Service) backendPlugin(pluginID string) (plugins.BackendPlugin, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	if err := s.initBackendPlugins(); err != nil {
		return nil, false, err
	}
	if s.backendPlugins == nil {
		return nil, false, nil
	}
	return s.backendPlugins.Plugin(pluginID)
}

func (s *Service) enabledBackendPlugins(ctx context.Context) ([]plugins.BackendPlugin, error) {
	if s == nil {
		return nil, nil
	}
	generationRecoveryPhase(ctx, "plugin-discovery", "load-manifests")
	if err := s.initBackendPlugins(); err != nil {
		return nil, err
	}
	if s.backendPlugins == nil {
		return nil, nil
	}
	ids := s.backendPlugins.PluginIDs()
	out := make([]plugins.BackendPlugin, 0, len(ids))
	for _, pluginID := range ids {
		generationRecoveryPhase(ctx, "plugin-discovery", pluginID)
		if !s.pluginEnabled(ctx, pluginID) {
			continue
		}
		plugin, ok, err := s.backendPlugin(pluginID)
		if err != nil {
			log.Printf("backend plugin %s skipped during sync after load failure: %v", pluginID, err)
			continue
		}
		if ok && plugin != nil {
			out = append(out, plugin)
		}
	}
	return out, nil
}

// SyncUser syncs every configured account for a user using each account's auto
// mailbox plan. Runner normally decomposes this into mailbox jobs, but tests and
// direct callers can still use this whole-account entrypoint.
func (s *Service) SyncUser(ctx context.Context, userID int64) (store.SyncRun, error) {
	return s.syncUser(ctx, userID, nil)
}

// SyncUserMailboxes applies the same requested folder names to every account on
// the user. It is used for global priority jobs like refreshing INBOX after a
// webhook.
func (s *Service) SyncUserMailboxes(ctx context.Context, userID int64, mailboxNames []string) (store.SyncRun, error) {
	return s.syncUser(ctx, userID, mailboxNames)
}

// SyncUserAccountMailboxes targets one IMAP server, which avoids ambiguous folder
// names when multiple accounts contain the same mailbox path.
func (s *Service) SyncUserAccountMailboxes(ctx context.Context, userID, accountID int64, mailboxNames []string) (store.SyncRun, error) {
	return s.syncUserAccountMailboxes(ctx, userID, accountID, mailboxNames, syncAccountOptions{})
}

func (s *Service) syncUserAccountMailboxes(ctx context.Context, userID, accountID int64,
	mailboxNames []string, options syncAccountOptions,
) (store.SyncRun, error) {
	if s.Fetcher == nil {
		return store.SyncRun{}, errors.New("sync fetcher is not configured")
	}
	account, err := s.Store.GetMailAccountForUser(ctx, userID, accountID)
	if err != nil {
		return store.SyncRun{}, err
	}
	return s.syncAccount(ctx, userID, account, mailboxNames, options)
}

// RecoverUserAccountMailboxGeneration runs one scheduler-bounded generation
// recovery turn. Pending flag uploads are tenant-wide work and are deliberately
// deferred until recovery releases its gate; otherwise a single mailbox turn can
// perform hundreds of unrelated IMAP commands before fetching its first body.
func (s *Service) RecoverUserAccountMailboxGeneration(ctx context.Context, userID, accountID int64, mailboxName string) (store.SyncRun, error) {
	if s.Fetcher == nil {
		return store.SyncRun{}, errors.New("sync fetcher is not configured")
	}
	account, err := s.Store.GetMailAccountForUser(ctx, userID, accountID)
	if err != nil {
		return store.SyncRun{}, err
	}
	return s.syncAccount(ctx, userID, account, []string{mailboxName}, syncAccountOptions{generationRecovery: true})
}

// DiscoverMailboxes refreshes local mailbox rows and remote STATUS counters
// without fetching message bodies. The UI uses this for folder lists and stale
// sync clues even when a full sync is not running.
func (s *Service) DiscoverMailboxes(ctx context.Context, userID int64) (int, error) {
	if s.Fetcher == nil {
		return 0, errors.New("sync fetcher is not configured")
	}
	accounts, err := s.Store.ListMailAccountsForUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	defer s.notify(userID)
	count := 0
	for _, account := range accounts {
		configured := strings.TrimSpace(account.Mailbox)
		if configured == "" {
			configured = store.DefaultMailboxPattern
		}
		infos, err := s.configuredMailboxes(ctx, account, configured)
		if err != nil {
			return count, err
		}
		for _, info := range infos {
			mb, err := s.Store.GetOrCreateMailboxWithRole(ctx, userID, account.ID, info.Name, mailboxSpecialUseRole(info.Attributes))
			if err != nil {
				return count, err
			}
			if status, err := s.Fetcher.MailboxStatus(ctx, account, mb.Name); err == nil {
				s.recordMailboxStatus(ctx, userID, mb, status)
			} else {
				log.Printf("refresh mailbox status user_id=%d account_id=%d mailbox=%s: %v", userID, account.ID, mb.Name, err)
			}
			count++
		}
	}
	return count, nil
}

// AutoMailboxNames returns the effective set of folders that an account-wide sync
// should visit. It respects per-folder sync modes and prioritizes inbox-like
// names so fresh mail is mirrored before archival folders.
func (s *Service) AutoMailboxNames(ctx context.Context, userID int64) ([]string, error) {
	accounts, err := s.Store.ListMailAccountsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, account := range accounts {
		names, err := s.mailboxesToSync(ctx, account, nil)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			key := strings.ToLower(strings.TrimSpace(name))
			if key != "" && !seen[key] {
				seen[key] = true
				out = append(out, name)
			}
		}
	}
	return prioritizeInbox(out), nil
}

// syncUser is the common multi-account loop. It returns the first run record so
// callers have something stable to display even when the user owns several IMAP
// accounts.
func (s *Service) syncUser(ctx context.Context, userID int64, requestedMailboxes []string) (store.SyncRun, error) {
	return s.syncUserWithOptions(ctx, userID, requestedMailboxes, syncAccountOptions{})
}

func (s *Service) syncUserWithOptions(ctx context.Context, userID int64, requestedMailboxes []string,
	options syncAccountOptions,
) (store.SyncRun, error) {
	if s.Fetcher == nil {
		return store.SyncRun{}, errors.New("sync fetcher is not configured")
	}
	if !options.deferOrdinaryMaintenanceNow() {
		completedCleanups, failedCleanups, cleanupErr := s.drainPendingBlobCleanupsForUser(ctx, userID, genericBlobCleanupOpportunisticLimit)
		if cleanupErr != nil {
			log.Printf("generic blob cleanup user_id=%d: %v", userID, cleanupErr)
		} else if failedCleanups > 0 {
			log.Printf("generic blob cleanup user_id=%d completed=%d failed=%d", userID, completedCleanups, failedCleanups)
		}
	}
	accounts, err := s.Store.ListMailAccountsForUser(ctx, userID)
	if err != nil {
		return store.SyncRun{}, err
	}
	if len(accounts) == 0 {
		return store.SyncRun{}, store.ErrNotFound
	}
	var first store.SyncRun
	for _, account := range accounts {
		run, err := s.syncAccount(ctx, userID, account, requestedMailboxes, options)
		if first.ID == 0 {
			first = run
		}
		if err != nil {
			return first, err
		}
	}
	return first, nil
}

// syncAccount is the main incremental sync pipeline:
// 1. create a sync_runs row for UI progress,
// 2. plan folders from IMAP STATUS and last stored UID,
// 3. normally push pending local Seen/Flagged changes (recovery defers them),
// 4. fetch only UIDs newer than each mailbox's last UID,
// 5. store blobs/message rows/attachments/search documents, and
// 6. reconcile lightweight metadata for small folders.
// The defer marks interrupted runs when the process context is cancelled.
type syncAccountOptions struct {
	generationRecovery bool
	deferPendingFlags  bool
	deferMaintenance   func() bool
	runDiagnostics     *syncRunDiagnostics
	onRunStarted       func(int64)
	onRunFinished      func(int64)
}

func (o syncAccountOptions) deferOrdinaryMaintenanceNow() bool {
	return !o.generationRecovery && o.deferMaintenance != nil && o.deferMaintenance()
}

func (s *Service) syncAccount(ctx context.Context, userID int64, account store.MailAccount, requestedMailboxes []string, options syncAccountOptions) (store.SyncRun, error) {
	ctx = withSyncRunDiagnostics(ctx, options.runDiagnostics)
	generationRecoveryPhase(ctx, "sqlite-create-sync-run", "")
	run, err := s.Store.CreateSyncRun(ctx, userID, account.ID)
	if err != nil {
		return store.SyncRun{}, err
	}
	if options.onRunStarted != nil {
		options.onRunStarted(run.ID)
	}

	progress := store.SyncProgress{}
	if len(requestedMailboxes) == 1 {
		progress.CurrentMailbox = strings.TrimSpace(requestedMailboxes[0])
		if progress.CurrentMailbox != "" {
			progress.MailboxesTotal = 1
		}
	}
	status := "ok"
	errText := ""
	defer func() {
		if options.onRunFinished != nil {
			defer options.onRunFinished(run.ID)
		}
		if ctx.Err() != nil && (status != "ok" || progress.MailboxesDone < progress.MailboxesTotal) {
			status = "interrupted"
			errText = "Server stopped before this sync finished."
		}
		if err := s.Store.FinishSyncRun(context.Background(), userID, run.ID, status, progress, errText); err != nil {
			log.Printf("finish sync run user_id=%d run_id=%d: %v", userID, run.ID, err)
		}
		s.notify(userID)
	}()
	if progress.CurrentMailbox != "" {
		if err := s.updateSyncProgress(ctx, userID, run.ID, progress); err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
	}

	generationRecoveryPhase(ctx, "mailbox-selection", "")
	mailboxNames, err := s.mailboxesToSync(ctx, account, requestedMailboxes)
	if err != nil {
		status = "failed"
		errText = err.Error()
		return run, err
	}
	generationRecoveryPhase(ctx, "sqlite-last-uids", "")
	lastUIDs, err := s.Store.LastUIDs(ctx, userID, account.ID)
	if err != nil {
		status = "failed"
		errText = err.Error()
		return run, err
	}
	generationRecoveryPhase(ctx, "imap-mailbox-status", "")
	plan, planErr := s.planMailboxes(ctx, account, mailboxNames, lastUIDs)
	requestedSet := requestedMailboxSet(requestedMailboxes)
	progress.MailboxesTotal = len(plan)
	for _, item := range plan {
		progress.MessagesTotal += item.Pending
	}
	s.updateSyncProgress(ctx, userID, run.ID, progress)

	deferOrdinaryMaintenance := options.deferOrdinaryMaintenanceNow()
	if options.generationRecovery {
		log.Printf("recover mailbox generation phase user_id=%d account_id=%d phase=defer-pending-flags", userID, account.ID)
	} else if options.deferPendingFlags || deferOrdinaryMaintenance {
		log.Printf("sync user_id=%d account_id=%d phase=defer-pending-flags reason=mailbox-generation-recovery", userID, account.ID)
	} else {
		if err := s.PushPendingReadState(ctx, userID, 500); err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
		if err := s.PushPendingStarState(ctx, userID, 500); err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
	}
	if planErr != nil {
		status = "failed"
		errText = planErr.Error()
		return run, planErr
	}

	for _, planned := range plan {
		mailboxName := planned.Name
		mailboxLastUIDAtStart := planned.LastUID
		generationRecoveryCheckpoint(ctx, mailboxLastUIDAtStart)
		progress.CurrentMailbox = mailboxName
		progress.CurrentUID = mailboxLastUIDAtStart
		s.updateSyncProgress(ctx, userID, run.ID, progress)

		generationRecoveryPhase(ctx, "sqlite-get-mailbox", "")
		mailbox, err := s.Store.GetOrCreateMailbox(ctx, userID, account.ID, mailboxName)
		if err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
		generationRecoveryPhase(ctx, "sqlite-generation-state", "")
		generationReset, err := s.ResetMailboxGenerationIfNeeded(ctx, userID, account, mailbox,
			planned.Status.UIDValidity, planned.Status.UIDNext)
		if err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
		if generationReset {
			previousPending := planned.Pending
			planned.LastUID = 0
			planned.Pending = 0
			if planned.Status.UIDNext > 0 {
				planned.Pending = int(planned.Status.UIDNext - 1)
			}
			progress.MessagesTotal += planned.Pending - previousPending
			if progress.MessagesTotal < progress.MessagesSeen {
				progress.MessagesTotal = progress.MessagesSeen
			}
			mailboxLastUIDAtStart = 0
			lastUIDs[mailboxName] = 0
			mailbox.LastUID = 0
			log.Printf("reset mailbox generation user_id=%d account_id=%d mailbox=%s uidvalidity=%d", userID, account.ID, mailboxName, planned.Status.UIDValidity)
		}
		generationRebuildPending := false
		generationArrivalUIDFloor := uint32(0)
		if planned.Status.UIDValidity > 0 {
			generationRebuildPending, err = s.Store.MailboxGenerationRebuildPending(ctx, userID, account.ID, mailbox.ID, planned.Status.UIDValidity)
			if err != nil {
				status = "failed"
				errText = err.Error()
				return run, err
			}
			if generationRebuildPending {
				if planned.Status.UIDNext > 0 {
					generationArrivalUIDFloor, err = s.Store.InitializeMailboxGenerationRebuildArrivalUIDFloor(ctx,
						userID, account.ID, mailbox.ID, planned.Status.UIDValidity, planned.Status.UIDNext)
				} else {
					generationArrivalUIDFloor, err = s.Store.MailboxGenerationRebuildArrivalUIDFloor(ctx,
						userID, account.ID, mailbox.ID, planned.Status.UIDValidity)
				}
				if err != nil {
					status = "failed"
					errText = err.Error()
					return run, err
				}
			}
		}
		if generationRebuildPending && s.DeferMailboxGenerationRebuilds && !options.generationRecovery {
			// An ordinary Inbox bypass may discover a second UIDVALIDITY change
			// while another mailbox is recovering. Persist the STATUS snapshot,
			// then leave all message fetches to the per-tenant recovery worker so
			// this sync cannot start another untracked rebuild inline.
			if err := s.Store.UpdateMailboxRemoteStatusForGeneration(ctx, userID, account.ID, mailbox.ID,
				int(planned.Status.Messages), int(planned.Status.Unseen), planned.Status.UIDNext,
				planned.Status.UIDValidity); err != nil {
				status = "failed"
				errText = err.Error()
				return run, err
			}
			progress.CurrentMailbox = mailboxName
			progress.CurrentUID = mailboxLastUIDAtStart
			if err := s.updateSyncProgress(ctx, userID, run.ID, progress); err != nil {
				status = "failed"
				errText = err.Error()
				return run, err
			}
			log.Printf("queue mailbox generation recovery user_id=%d account_id=%d mailbox=%q reason=serialized-recovery",
				userID, account.ID, mailboxName)
			// ResetMailboxGenerationIfNeeded already signals for a fresh marker.
			// A marker restored from disk still needs to wake the worker.
			if !generationReset && s.MailboxGenerationRecoveryStarted != nil {
				s.MailboxGenerationRecoveryStarted(userID)
			}
			return run, nil
		}
		rebuildInProgress := generationReset || generationRebuildPending
		isPostRebuildArrival := func(item FetchedMessage) bool {
			return rebuildInProgress && generationArrivalUIDFloor > 0 && item.UID >= generationArrivalUIDFloor
		}
		shouldNotifyFetchedItem := func(item FetchedMessage) bool {
			if isPostRebuildArrival(item) {
				return mailboxReceivesNewMailNotifications(mailbox)
			}
			return !rebuildInProgress && shouldNotifyNewMail(mailbox, mailboxLastUIDAtStart, item)
		}
		shouldCancelSnoozeForFetchedItem := func(item FetchedMessage, notifyNewMail bool) bool {
			if notifyNewMail {
				return false
			}
			if isPostRebuildArrival(item) {
				return true
			}
			return !rebuildInProgress && shouldCancelSnoozeForNewMessage(mailbox, mailboxLastUIDAtStart, item)
		}
		if planned.Status.UIDValidity == 0 {
			status = "failed"
			errText = "mailbox sync requires a known UIDVALIDITY"
			return run, errors.New(errText)
		}
		if err := s.Store.UpdateMailboxRemoteStatusForGeneration(ctx, userID, account.ID, mailbox.ID,
			int(planned.Status.Messages), int(planned.Status.Unseen), planned.Status.UIDNext, planned.Status.UIDValidity); err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
		if planned.Status.UIDValidity > 0 {
			mailbox.UIDValidity = int64(planned.Status.UIDValidity)
		}
		if generationRebuildPending && generationArrivalUIDFloor > 0 {
			if err := s.replayStoredGenerationRebuildArrivals(ctx, userID, account, mailbox,
				planned.Status.UIDValidity, generationArrivalUIDFloor, run.ID, &progress); err != nil {
				status = "failed"
				errText = err.Error()
				return run, err
			}
		}
		repairRequested := requestedSet[strings.ToLower(mailboxName)] && !rebuildInProgress
		if repairRequested && options.deferOrdinaryMaintenanceNow() {
			log.Printf("defer incomplete mailbox repair user_id=%d account_id=%d mailbox=%q reason=mailbox-generation-recovery",
				userID, account.ID, mailboxName)
			repairRequested = false
		}
		repairedPlan, repaired, err := s.repairRequestedIncompleteMailbox(ctx, userID, account, mailbox, planned,
			repairRequested, run.ID, &progress)
		if err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
		if repaired {
			planned = repairedPlan
			mailboxLastUIDAtStart = planned.LastUID
			lastUIDs[mailboxName] = planned.LastUID
			progress.CurrentUID = planned.LastUID
			if err := s.updateSyncProgress(ctx, userID, run.ID, progress); err != nil {
				status = "failed"
				errText = err.Error()
				return run, err
			}
		}
		// Consume an explicit purge before fetching the incremental delta. Newly
		// fetched documents then retain their post-commit markers while the repair
		// pass below restores only missing historical documents.
		if s.Search != nil && mailbox.IncludeInSearch && !generationRebuildPending && !options.deferOrdinaryMaintenanceNow() {
			if _, err := s.Store.PreparePurgedMailboxSearchIndexRepair(ctx, userID, mailbox.ID); err != nil {
				status = "failed"
				errText = err.Error()
				return run, err
			}
		}
		generationRecoveryPhase(ctx, "plugin-hook-discovery", "")
		classifiers, storedHooks, err := s.postStorePluginHooks(ctx)
		if err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
		searchBatch := newFetchedSearchIndexBatch(s)
		classificationBatch := newMessageClassificationBatch(s, classifiers)
		completionBatch := newMessageImportCompletionBatch(s, userID)
		if rebuildInProgress {
			if _, ok := s.Fetcher.(UIDValidityMailboxFetcher); !ok {
				status = "failed"
				errText = "mailbox generation rebuild requires a generation-bound fetcher"
				return run, errors.New(errText)
			}
		}
		prepareFetchedItem := func(item FetchedMessage) FetchedMessage {
			if item.Mailbox == "" {
				item.Mailbox = mailboxName
			}
			if item.InternalDate.IsZero() {
				item.InternalDate = time.Now().UTC()
			}
			if item.Size == 0 {
				item.Size = int64(len(item.Raw))
			}
			return item
		}
		storeFetchedItem := func(item FetchedMessage, runStoredHooks bool) (store.MessageRecord, bool, error) {
			indexFetched := !rebuildInProgress || generationReset || options.generationRecovery
			msg, parsed, pendingIndex, err := s.storeFetchedMessage(ctx, userID, account, mailbox, item, indexFetched)
			if err != nil {
				return store.MessageRecord{}, false, err
			}
			completionBatch.Add(msg.ID)
			// A UIDVALIDITY recovery starts from a clean mailbox document scope, so
			// each recovered message can be indexed as it is stored. This avoids a
			// later whole-mailbox audit while never exposing stale documents from
			// the previous generation.
			if indexFetched {
				if err := searchBatch.Add(ctx, pendingIndex); err != nil {
					return store.MessageRecord{}, false, err
				}
				classificationBatch.Add(msg, parsed)
				if searchBatch.Empty() {
					classificationBatch.Flush(ctx)
					if err := ctx.Err(); err != nil {
						return store.MessageRecord{}, false, err
					}
					if len(storedHooks) == 0 {
						if err := completionBatch.Flush(ctx); err != nil {
							return store.MessageRecord{}, false, err
						}
					}
				}
			} else {
				generationRecoveryPhase(ctx, "search-deferred", "generation-rebuild")
			}
			if runStoredHooks && len(storedHooks) > 0 {
				if !rebuildInProgress {
					if err := searchBatch.Flush(ctx); err != nil {
						return store.MessageRecord{}, false, err
					}
					classificationBatch.Flush(ctx)
					if err := ctx.Err(); err != nil {
						return store.MessageRecord{}, false, err
					}
				}
				if err := s.importStoredMessageHooks(ctx, storedHooks, msg, mailbox); err != nil {
					return store.MessageRecord{}, false, err
				}
			}
			if rebuildInProgress || (runStoredHooks && len(storedHooks) > 0) {
				if err := completionBatch.Flush(ctx); err != nil {
					return store.MessageRecord{}, false, err
				}
			}
			s.warmRemoteImagesForStoredMessage(msg, mailbox, parsed.HTML)
			return msg, completionBatch.Empty(), nil
		}
		prewarmedUIDs := map[uint32]bool{}
		prewarmHandle := func(item FetchedMessage) error {
			item = prepareFetchedItem(item)
			generationRecoveryPhase(ctx, "sqlite-message-exists", "")
			exists, err := s.Store.MessageExistsByUIDForGeneration(ctx, userID, account.ID, mailbox.ID,
				item.UID, item.UIDValidity)
			if err != nil {
				return err
			}
			if exists {
				return nil
			}
			msg, _, err := storeFetchedItem(item, isPostRebuildArrival(item))
			if err != nil {
				return err
			}
			prewarmedUIDs[item.UID] = true
			progress.MessagesSeen++
			progress.MessagesStored++
			if progress.MessagesTotal < progress.MessagesSeen {
				progress.MessagesTotal = progress.MessagesSeen
			}
			notifyNewMail := shouldNotifyFetchedItem(item)
			if shouldCancelSnoozeForFetchedItem(item, notifyNewMail) {
				if _, err := s.Store.CancelSnoozeForNewMessage(ctx, userID, msg); err != nil {
					return err
				}
			}
			if notifyNewMail {
				if err := s.recordInboxArrival(ctx, userID, run.ID, msg, item, &progress); err != nil {
					return err
				}
			}
			return s.updateSyncProgress(ctx, userID, run.ID, progress)
		}
		generationRecoverySnapshot := MailboxUIDSnapshot{}
		if generationRebuildPending {
			log.Printf("recover mailbox generation phase user_id=%d account_id=%d mailbox=%q phase=snapshot-and-newest-page after_uid=%d",
				userID, account.ID, mailbox.Name, mailboxLastUIDAtStart)
			var prewarmFatalErr, prewarmErr error
			seedRecoveryProgress := func(snapshot MailboxUIDSnapshot) error {
				completedBefore := mailboxGenerationSnapshotCompletedBefore(snapshot, mailboxLastUIDAtStart)
				// The planner estimates only the remaining UID range. Replace that
				// mailbox contribution with the immutable snapshot total, then seed
				// seen from the durable checkpoint so each bounded turn reports
				// cumulative recovery progress instead of restarting at zero.
				progress.MessagesTotal += len(snapshot.UIDs) - planned.Pending
				progress.MessagesSeen += completedBefore
				if progress.MessagesTotal < progress.MessagesSeen {
					progress.MessagesTotal = progress.MessagesSeen
				}
				return s.updateSyncProgress(ctx, userID, run.ID, progress)
			}
			generationRecoverySnapshot, prewarmFatalErr, prewarmErr = s.prewarmPendingMailboxGeneration(ctx,
				userID, account, mailbox, planned.Status.UIDValidity, prewarmHandle, seedRecoveryProgress)
			log.Printf("recover mailbox generation phase complete user_id=%d account_id=%d mailbox=%q phase=snapshot-and-newest-page snapshot_uids=%d prewarmed=%d",
				userID, account.ID, mailbox.Name, len(generationRecoverySnapshot.UIDs), len(prewarmedUIDs))
			if prewarmFatalErr != nil {
				status = "failed"
				errText = prewarmFatalErr.Error()
				return run, prewarmFatalErr
			}
			if prewarmErr != nil && (ctx.Err() != nil || errors.Is(prewarmErr, context.Canceled)) {
				status = "failed"
				errText = prewarmErr.Error()
				return run, prewarmErr
			}
			if prewarmErr != nil && generationRecoverySnapshot.UIDNext == 0 {
				// Production recovery needs the immutable UID snapshot to stop after a
				// bounded turn. Retrying is preferable to falling back to an unbounded
				// mailbox fetch that can starve every other account for this tenant.
				if _, snapshotCapable := s.Fetcher.(MailboxUIDSnapshotFetcher); snapshotCapable {
					status = "failed"
					errText = prewarmErr.Error()
					return run, prewarmErr
				}
			}
			if prewarmErr != nil {
				// A newest-page preview must never become a new prerequisite for
				// completing a recovery after its immutable snapshot was captured.
				log.Printf("prewarm mailbox generation user_id=%d account_id=%d mailbox=%s: %v",
					userID, account.ID, mailbox.Name, prewarmErr)
			}
			if err := searchBatch.Flush(ctx); err != nil {
				status = "failed"
				errText = err.Error()
				return run, err
			}
			classificationBatch.Flush(ctx)
			if len(prewarmedUIDs) > 0 {
				// Invalidate the saved first page once after the bounded preview is
				// durable. Per-message progress updates use the lightweight event path.
				s.notify(userID)
			}
		}
		var pendingImportUID uint32
		handleFetchedItem := func(item FetchedMessage) error {
			item = prepareFetchedItem(item)
			prewarmed := prewarmedUIDs[item.UID]
			if !prewarmed {
				progress.MessagesSeen++
			}
			progress.CurrentMailbox = item.Mailbox
			progress.CurrentUID = item.UID

			if item.UID <= lastUIDs[mailboxName] {
				if !prewarmed {
					progress.MessagesSkipped++
				}
				return s.updateSyncProgress(ctx, userID, run.ID, progress)
			}
			generationRecoveryPhase(ctx, "sqlite-message-exists", "")
			exists, err := s.Store.MessageExistsByUIDForGeneration(ctx, userID, account.ID, mailbox.ID,
				item.UID, item.UIDValidity)
			if err != nil {
				return err
			}
			if exists {
				notifyNewMail := shouldNotifyFetchedItem(item)
				cancelSnooze := shouldCancelSnoozeForFetchedItem(item, notifyNewMail)
				if notifyNewMail || cancelSnooze {
					msg, err := s.Store.GetMessageByUID(ctx, userID, account.ID, mailbox.ID, item.UID)
					if err != nil {
						return err
					}
					storedUIDValidity, err := s.Store.GetMessageUIDValidityForUser(ctx, userID, msg.ID)
					if err != nil {
						return err
					}
					if storedUIDValidity != int64(item.UIDValidity) {
						return store.ErrMailboxGenerationChanged
					}
					if cancelSnooze {
						if _, err := s.Store.CancelSnoozeForNewMessage(ctx, userID, msg); err != nil {
							return err
						}
					}
					if notifyNewMail {
						if err := s.recordInboxArrival(ctx, userID, run.ID, msg, item, &progress); err != nil {
							return err
						}
					}
				}
				if !prewarmed {
					progress.MessagesSkipped++
				}
				if pendingImportUID == 0 && item.UID > lastUIDs[mailboxName] {
					generationRecoveryPhase(ctx, "sqlite-checkpoint", "")
					if err := s.Store.UpdateMailboxLastUIDForGeneration(ctx, userID, account.ID, mailbox.ID,
						item.UID, item.UIDValidity); err != nil {
						return err
					}
					lastUIDs[mailboxName] = item.UID
					generationRecoveryCheckpoint(ctx, item.UID)
				}
				return s.updateSyncProgress(ctx, userID, run.ID, progress)
			}
			if prewarmed {
				// A concurrent local purge removed a preview row. Count and store the
				// normal ascending fetch as fresh work instead of losing progress.
				delete(prewarmedUIDs, item.UID)
				progress.MessagesSeen++
			}
			msg, importComplete, err := storeFetchedItem(item, !rebuildInProgress || isPostRebuildArrival(item))
			if err != nil {
				return err
			}
			progress.MessagesStored++
			notifyNewMail := shouldNotifyFetchedItem(item)
			if shouldCancelSnoozeForFetchedItem(item, notifyNewMail) {
				if _, err := s.Store.CancelSnoozeForNewMessage(ctx, userID, msg); err != nil {
					return err
				}
			}
			if notifyNewMail {
				if err := s.recordInboxArrival(ctx, userID, run.ID, msg, item, &progress); err != nil {
					return err
				}
			}
			if !importComplete {
				pendingImportUID = item.UID
			} else if item.UID > lastUIDs[mailboxName] {
				generationRecoveryPhase(ctx, "sqlite-checkpoint", "")
				if err := s.Store.UpdateMailboxLastUIDForGeneration(ctx, userID, account.ID, mailbox.ID,
					item.UID, item.UIDValidity); err != nil {
					return err
				}
				lastUIDs[mailboxName] = item.UID
				pendingImportUID = 0
				generationRecoveryCheckpoint(ctx, item.UID)
			}
			return s.updateSyncProgress(ctx, userID, run.ID, progress)
		}
		generationSnapshotRecovery := generationRebuildPending &&
			generationRecoverySnapshot.UIDValidity == planned.Status.UIDValidity &&
			generationRecoverySnapshot.UIDNext > 0
		generationRecoveryComplete := true
		if generationSnapshotRecovery {
			log.Printf("recover mailbox generation batch user_id=%d account_id=%d mailbox=%q after_uid=%d snapshot_uids=%d limit=%d",
				userID, account.ID, mailbox.Name, mailboxLastUIDAtStart, len(generationRecoverySnapshot.UIDs),
				mailboxGenerationRecoveryBatchSize)
			refreshNewest := func(final bool) error {
				_, fatalErr, fetchErr := s.prewarmPendingMailboxGeneration(ctx, userID, account, mailbox,
					planned.Status.UIDValidity, prewarmHandle, nil)
				if fatalErr != nil {
					return fatalErr
				}
				if fetchErr == nil {
					return nil
				}
				if final || ctx.Err() != nil || errors.Is(fetchErr, context.Canceled) {
					return fetchErr
				}
				log.Printf("refresh prewarm mailbox generation user_id=%d account_id=%d mailbox=%s: %v",
					userID, account.ID, mailbox.Name, fetchErr)
				return nil
			}
			generationRecoveryComplete, err = s.fetchMailboxGenerationSnapshotBatch(ctx, account, mailbox,
				mailboxLastUIDAtStart, planned.Status.UIDValidity, generationRecoverySnapshot,
				handleFetchedItem, refreshNewest)
			if err == nil {
				log.Printf("recover mailbox generation batch complete user_id=%d account_id=%d mailbox=%q checkpoint_uid=%d final=%t",
					userID, account.ID, mailbox.Name, lastUIDs[mailboxName], generationRecoveryComplete)
			}
		} else {
			err = s.fetchMailboxForGeneration(ctx, account, mailboxName, mailboxLastUIDAtStart,
				planned.Status.UIDValidity, handleFetchedItem)
		}
		if err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
		if err := searchBatch.Flush(ctx); err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
		classificationBatch.Flush(ctx)
		if err := ctx.Err(); err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
		if err := completionBatch.Flush(ctx); err != nil {
			status = "failed"
			errText = err.Error()
			return run, err
		}
		if pendingImportUID > lastUIDs[mailboxName] {
			generationRecoveryPhase(ctx, "sqlite-checkpoint", "")
			if err := s.Store.UpdateMailboxLastUIDForGeneration(ctx, userID, account.ID, mailbox.ID,
				pendingImportUID, planned.Status.UIDValidity); err != nil {
				status = "failed"
				errText = err.Error()
				return run, err
			}
			lastUIDs[mailboxName] = pendingImportUID
			generationRecoveryCheckpoint(ctx, pendingImportUID)
			pendingImportUID = 0
		}
		if generationRebuildPending && !generationRecoveryComplete {
			// The checkpoint and index batch are durable. End this scheduler turn
			// without finalizing the marker so another account can run before the
			// next bounded history batch.
			progress.CurrentMailbox = mailboxName
			progress.CurrentUID = lastUIDs[mailboxName]
			if err := s.updateSyncProgress(ctx, userID, run.ID, progress); err != nil {
				status = "failed"
				errText = err.Error()
				return run, err
			}
			continue
		}
		// Ordinary IMAP sync only indexes the messages it fetches. An exact
		// Bleve/SQLite audit can require a remote raw-message fetch for old mail,
		// so it is deliberately an explicit folder or account reindex task rather
		// than surprise background work after an upgrade or restart.
		if options.deferOrdinaryMaintenanceNow() {
			log.Printf("defer mailbox metadata reconciliation user_id=%d account_id=%d mailbox=%q reason=mailbox-generation-recovery",
				userID, account.ID, mailboxName)
		} else if s.shouldSyncInlineMetadata(planned) {
			if err := s.syncMailboxReadFlags(ctx, userID, account, mailbox); err != nil {
				log.Printf("sync seen flags user_id=%d mailbox=%s: %v", userID, mailboxName, err)
			}
			if err := s.syncMailboxStarFlags(ctx, userID, account, mailbox); err != nil {
				log.Printf("sync flagged flags user_id=%d mailbox=%s: %v", userID, mailboxName, err)
			}
			if err := s.reconcileMailboxUIDs(ctx, userID, account, mailbox); err != nil {
				log.Printf("reconcile mailbox user_id=%d mailbox=%s: %v", userID, mailboxName, err)
			}
		} else {
			log.Printf("defer large-folder metadata reconciliation user_id=%d mailbox=%s messages=%d threshold=%d", userID, mailboxName, planned.Status.Messages, inlineMetadataSyncLimit)
		}
		if planned.Status.UIDValidity > 0 {
			if err := s.Store.FinalizeMailboxGenerationRebuild(ctx, userID, account.ID, mailbox.ID, planned.Status.UIDValidity); err != nil {
				status = "failed"
				errText = err.Error()
				return run, err
			}
			if generationRebuildPending && s.ScheduleInboxArrival != nil {
				restoredArrivalDue, dueErr := s.Store.NextPendingInboxArrivalDue(ctx, userID, account.ID)
				if dueErr != nil && !store.IsNotFound(dueErr) {
					status = "failed"
					errText = dueErr.Error()
					return run, dueErr
				}
				if dueErr == nil {
					s.ScheduleInboxArrival(userID, account.ID, restoredArrivalDue)
				}
			}
			if generationRebuildPending && s.NotifyRestoredState != nil {
				s.NotifyRestoredState(userID)
			}
		}
		progress.MailboxesDone++
		progress.CurrentMailbox = mailboxName
		progress.CurrentUID = lastUIDs[mailboxName]
		s.updateSyncProgress(ctx, userID, run.ID, progress)
		// Blob cleanup is durable derived maintenance. Drain only a small batch
		// after current mail and its checkpoint are visible; a large generation
		// reset must not spend minutes deleting old cache entries before the
		// newest page can render.
		if !options.deferOrdinaryMaintenanceNow() {
			if err := s.cleanupMailboxGenerationBlobs(ctx, userID, account.ID, mailbox.ID,
				mailboxGenerationBlobCleanupBatchSize); err != nil && ctx.Err() == nil {
				log.Printf("defer mailbox generation blob cleanup user_id=%d account_id=%d mailbox=%s: %v",
					userID, account.ID, mailboxName, err)
			}
		}
	}
	return run, nil
}

func requestedMailboxSet(names []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			out[name] = true
		}
	}
	return out
}

type uidBatchFetcher interface {
	FetchUIDs(ctx context.Context, account store.MailAccount, mailbox string, uids []uint32, handle func(FetchedMessage) error) error
}

// repairRequestedIncompleteMailbox handles the explicit "Sync now" repair case.
// If local rows are missing but the UID checkpoint says there are no newer
// messages, a normal incremental sync cannot fill the gap. Instead of resetting
// the checkpoint and downloading every body again, this compares the remote UID
// list to local UIDs and fetches only the missing bodies.
func (s *Service) repairRequestedIncompleteMailbox(ctx context.Context, userID int64, account store.MailAccount, mailbox store.Mailbox, plan MailboxPlan, requested bool, runID int64, progress *store.SyncProgress) (MailboxPlan, bool, error) {
	if !requested || plan.Status.Messages == 0 {
		return plan, false, nil
	}
	localUIDs, err := s.Store.MessageUIDsForMailbox(ctx, userID, account.ID, mailbox.ID)
	if err != nil {
		return plan, false, err
	}
	remoteCount := int(plan.Status.Messages)
	if remoteCount > 0 && len(localUIDs) >= remoteCount && plan.Pending == 0 {
		return plan, false, nil
	}
	var remoteUIDs []uint32
	var snapshotUIDValidity uint32
	expectedUIDValidity := plan.Status.UIDValidity
	if expectedUIDValidity == 0 && mailbox.UIDValidity > 0 && mailbox.UIDValidity <= int64(^uint32(0)) {
		expectedUIDValidity = uint32(mailbox.UIDValidity)
	}
	if snapshotFetcher, ok := s.Fetcher.(MailboxUIDSnapshotFetcher); ok {
		snapshot, err := snapshotFetcher.SnapshotMailboxUIDs(ctx, account, mailbox.Name)
		if err != nil {
			return plan, false, err
		}
		if snapshot.UIDValidity == 0 || snapshot.UIDNext == 0 {
			return plan, false, errors.New("mailbox repair snapshot is missing UIDVALIDITY or UIDNEXT")
		}
		if plan.Status.UIDValidity == 0 || snapshot.UIDValidity != plan.Status.UIDValidity || mailbox.UIDValidity <= 0 || int64(snapshot.UIDValidity) != mailbox.UIDValidity {
			return plan, false, errors.New("mailbox generation changed while preparing sparse repair")
		}
		for _, uid := range snapshot.UIDs {
			if uid == 0 || uid >= snapshot.UIDNext {
				return plan, false, errors.New("mailbox repair snapshot contains a UID outside its UIDNEXT boundary")
			}
		}
		remoteUIDs = snapshot.UIDs
		snapshotUIDValidity = snapshot.UIDValidity
		expectedUIDValidity = snapshot.UIDValidity
	} else {
		// Legacy fake/plugin fetchers do not expose a same-session generation
		// snapshot. Production imapclient.Fetcher always uses the branch above.
		var err error
		remoteUIDs, err = s.Fetcher.UIDs(ctx, account, mailbox.Name)
		if err != nil {
			return plan, false, err
		}
	}
	missing := missingRemoteUIDs(remoteUIDs, localUIDs)
	highestRemoteUID := maxUID(remoteUIDs)
	if progress != nil {
		progress.MessagesTotal += len(missing) - plan.Pending
		if progress.MessagesTotal < progress.MessagesSeen {
			progress.MessagesTotal = progress.MessagesSeen
		}
		progress.CurrentMailbox = mailbox.Name
		progress.CurrentUID = plan.LastUID
		if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
			return plan, false, err
		}
	}
	if len(missing) == 0 {
		if highestRemoteUID > plan.LastUID {
			if err := s.Store.UpdateMailboxLastUIDForGeneration(ctx, userID, account.ID, mailbox.ID,
				highestRemoteUID, expectedUIDValidity); err != nil {
				return plan, false, err
			}
			plan.LastUID = highestRemoteUID
		}
		plan.Pending = 0
		return plan, true, nil
	}
	log.Printf("repair incomplete mailbox user_id=%d account_id=%d mailbox=%s local=%d remote=%d missing=%d last_uid=%d uidnext=%d", userID, account.ID, mailbox.Name, len(localUIDs), len(remoteUIDs), len(missing), plan.LastUID, plan.Status.UIDNext)

	classifiers, storedHooks, err := s.postStorePluginHooks(ctx)
	if err != nil {
		return plan, false, err
	}
	searchBatch := newFetchedSearchIndexBatch(s)
	classificationBatch := newMessageClassificationBatch(s, classifiers)
	completionBatch := newMessageImportCompletionBatch(s, userID)
	handle := func(item FetchedMessage) error {
		if item.Mailbox == "" {
			item.Mailbox = mailbox.Name
		}
		if item.InternalDate.IsZero() {
			item.InternalDate = time.Now().UTC()
		}
		if item.Size == 0 {
			item.Size = int64(len(item.Raw))
		}
		if progress != nil {
			progress.MessagesSeen++
			progress.CurrentMailbox = item.Mailbox
			progress.CurrentUID = item.UID
		}
		exists, err := s.Store.MessageExistsByUIDForGeneration(ctx, userID, account.ID, mailbox.ID,
			item.UID, expectedUIDValidity)
		if err != nil {
			return err
		}
		if exists {
			if progress != nil {
				if shouldNotifyNewMail(mailbox, plan.LastUID, item) {
					msg, err := s.Store.GetMessageByUID(ctx, userID, account.ID, mailbox.ID, item.UID)
					if err != nil {
						return err
					}
					if err := s.recordInboxArrival(ctx, userID, runID, msg, item, progress); err != nil {
						return err
					}
				}
				progress.MessagesSkipped++
				return s.updateSyncProgress(ctx, userID, runID, *progress)
			}
			return nil
		}
		msg, parsed, pendingIndex, err := s.storeFetchedMessage(ctx, userID, account, mailbox, item, true)
		if err != nil {
			return err
		}
		completionBatch.Add(msg.ID)
		if err := searchBatch.Add(ctx, pendingIndex); err != nil {
			return err
		}
		classificationBatch.Add(msg, parsed)
		if searchBatch.Empty() {
			classificationBatch.Flush(ctx)
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(storedHooks) == 0 {
				if err := completionBatch.Flush(ctx); err != nil {
					return err
				}
			}
		}
		if len(storedHooks) > 0 {
			if err := searchBatch.Flush(ctx); err != nil {
				return err
			}
			classificationBatch.Flush(ctx)
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.importStoredMessageHooks(ctx, storedHooks, msg, mailbox); err != nil {
				return err
			}
			if err := completionBatch.Flush(ctx); err != nil {
				return err
			}
		}
		s.warmRemoteImagesForStoredMessage(msg, mailbox, parsed.HTML)
		notifyNewMail := shouldNotifyNewMail(mailbox, plan.LastUID, item)
		if !notifyNewMail && shouldCancelSnoozeForNewMessage(mailbox, plan.LastUID, item) {
			if _, err := s.Store.CancelSnoozeForNewMessage(ctx, userID, msg); err != nil {
				return err
			}
		}
		if progress != nil {
			progress.MessagesStored++
			if notifyNewMail {
				if err := s.recordInboxArrival(ctx, userID, runID, msg, item, progress); err != nil {
					return err
				}
			}
			if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
				return err
			}
		}
		return nil
	}
	if snapshotUIDValidity > 0 {
		if err := s.fetchUIDsForGeneration(ctx, account, mailbox.Name, missing, snapshotUIDValidity, handle); err != nil {
			return plan, false, err
		}
	} else if batchFetcher, ok := s.Fetcher.(uidBatchFetcher); ok {
		if err := batchFetcher.FetchUIDs(ctx, account, mailbox.Name, missing, handle); err != nil {
			return plan, false, err
		}
	} else {
		for _, uid := range missing {
			item, err := s.Fetcher.FetchMessage(ctx, account, mailbox.Name, uid)
			if err != nil {
				return plan, false, err
			}
			if err := handle(item); err != nil {
				return plan, false, err
			}
		}
	}
	if err := searchBatch.Flush(ctx); err != nil {
		return plan, false, err
	}
	classificationBatch.Flush(ctx)
	if err := ctx.Err(); err != nil {
		return plan, false, err
	}
	if err := completionBatch.Flush(ctx); err != nil {
		return plan, false, err
	}
	if highestRemoteUID > plan.LastUID {
		if err := s.Store.UpdateMailboxLastUIDForGeneration(ctx, userID, account.ID, mailbox.ID,
			highestRemoteUID, expectedUIDValidity); err != nil {
			return plan, false, err
		}
		plan.LastUID = highestRemoteUID
	}
	plan.Pending = 0
	return plan, true, nil
}

func missingRemoteUIDs(remoteUIDs, localUIDs []uint32) []uint32 {
	local := make(map[uint32]bool, len(localUIDs))
	for _, uid := range localUIDs {
		if uid != 0 {
			local[uid] = true
		}
	}
	seen := map[uint32]bool{}
	out := make([]uint32, 0)
	for _, uid := range remoteUIDs {
		if uid == 0 || local[uid] || seen[uid] {
			continue
		}
		seen[uid] = true
		out = append(out, uid)
	}
	return out
}

func maxUID(uids []uint32) uint32 {
	var max uint32
	for _, uid := range uids {
		if uid > max {
			max = uid
		}
	}
	return max
}

// planMailboxes makes progress meaningful before the first message arrives.
// IMAP STATUS is cheap compared with fetching bodies, and UIDNEXT lets us
// estimate remaining work per folder without mutating the remote mailbox.
func (s *Service) planMailboxes(ctx context.Context, account store.MailAccount, names []string, lastUIDs map[string]uint32) ([]MailboxPlan, error) {
	plans := make([]MailboxPlan, 0, len(names))
	for _, name := range names {
		status, err := s.Fetcher.MailboxStatus(ctx, account, name)
		if err != nil {
			plans = append(plans, MailboxPlan{Name: name, LastUID: lastUIDs[name]})
			return plans, fmt.Errorf("read IMAP status for mailbox %q: %w", name, err)
		}
		pending := 0
		if status.UIDNext > 0 {
			highest := status.UIDNext - 1
			if highest > lastUIDs[name] {
				pending = int(highest - lastUIDs[name])
			}
		}
		plans = append(plans, MailboxPlan{Name: name, Status: status, LastUID: lastUIDs[name], Pending: pending})
	}
	return plans, nil
}

// updateSyncProgress persists a progress snapshot and immediately notifies the
// event hub, which is what drives the sidebar and settings sync indicators.
func (s *Service) updateSyncProgress(ctx context.Context, userID, runID int64, progress store.SyncProgress) error {
	generationRecoveryPhase(ctx, "sqlite-sync-progress", "")
	if err := s.Store.UpdateSyncRunProgress(ctx, userID, runID, progress); err != nil {
		return err
	}
	s.notifyProgress(userID)
	return nil
}

// recordMailboxStatus keeps remote counters separate from local/indexed counters
// so the UI can tell "known remotely" from "mirrored locally".
func (s *Service) recordMailboxStatus(ctx context.Context, userID int64, mailbox store.Mailbox, status MailboxStatus) {
	if status.UIDNext == 0 && status.Messages == 0 && status.Unseen == 0 && status.UIDValidity == 0 {
		return
	}
	if status.UIDValidity == 0 {
		return
	}
	var err error
	switch {
	case mailbox.UIDValidity == 0:
		err = s.Store.InitializeMailboxRemoteStatus(ctx, userID, mailbox.AccountID, mailbox.ID,
			int(status.Messages), int(status.Unseen), status.UIDNext, status.UIDValidity)
	case mailbox.UIDValidity == int64(status.UIDValidity):
		err = s.Store.UpdateMailboxRemoteStatusForGeneration(ctx, userID, mailbox.AccountID, mailbox.ID,
			int(status.Messages), int(status.Unseen), status.UIDNext, status.UIDValidity)
	default:
		log.Printf("skip mailbox status from unproven generation user_id=%d account_id=%d mailbox=%s cached_uidvalidity=%d remote_uidvalidity=%d",
			userID, mailbox.AccountID, mailbox.Name, mailbox.UIDValidity, status.UIDValidity)
		return
	}
	if errors.Is(err, store.ErrMailboxGenerationChanged) {
		log.Printf("skip mailbox status after concurrent generation change user_id=%d account_id=%d mailbox=%s remote_uidvalidity=%d",
			userID, mailbox.AccountID, mailbox.Name, status.UIDValidity)
		return
	}
	if err != nil {
		log.Printf("store mailbox status user_id=%d mailbox=%s: %v", userID, mailbox.Name, err)
	}
}

// shouldSyncInlineMetadata avoids expensive full-mailbox flag and UID searches on
// very large folders; those folders still get new-message fetches incrementally.
func (s *Service) shouldSyncInlineMetadata(plan MailboxPlan) bool {
	return plan.Status.Messages == 0 || plan.Status.Messages <= inlineMetadataSyncLimit
}
func (s *Service) notify(userID int64) {
	if s.Notify != nil {
		s.Notify(userID)
	}
}

func (s *Service) notifyProgress(userID int64) {
	if s.NotifyProgress != nil {
		s.NotifyProgress(userID)
		return
	}
	s.notify(userID)
}
