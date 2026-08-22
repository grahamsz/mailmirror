// File overview: IMAP-backed message copy helpers for drag/drop copy operations.

package syncer

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"rolltop/backend/store"
)

// CopyMessages appends local or refetched raw messages to the destination IMAP
// mailbox, then mirrors the new server-assigned UID locally. Source messages are
// left untouched, and the destination may belong to a different IMAP account.
func (s *Service) CopyMessages(ctx context.Context, userID int64, messageIDs []int64, destMailboxID int64) (int, error) {
	ids := uniqueMessageIDs(messageIDs)
	if len(ids) == 0 {
		return 0, errors.New("no messages selected")
	}
	copied := 0
	for _, id := range ids {
		if err := s.CopyMessage(ctx, userID, id, destMailboxID); err != nil {
			return copied, err
		}
		copied++
	}
	return copied, nil
}

// StartCopyMessages runs a large copy as a background sync run so the HTTP
// request can return quickly while progress remains visible in the sidebar.
func (s *Service) StartCopyMessages(ctx context.Context, userID int64, messageIDs []int64, destMailboxID int64, onDone func()) (store.SyncRun, error) {
	if s.Fetcher == nil {
		return store.SyncRun{}, errors.New("sync fetcher is not configured")
	}
	ids := uniqueMessageIDs(messageIDs)
	if len(ids) == 0 {
		return store.SyncRun{}, errors.New("no messages selected")
	}
	dest, err := s.Store.GetMailboxForUser(ctx, userID, destMailboxID)
	if err != nil {
		return store.SyncRun{}, err
	}
	run, err := s.Store.CreateSyncRun(ctx, userID, dest.AccountID)
	if err != nil {
		return store.SyncRun{}, err
	}
	progress := store.SyncProgress{MessagesTotal: len(ids), MailboxesTotal: 1, CurrentMailbox: "Copying to " + dest.Name}
	if err := s.Store.UpdateSyncRunProgress(ctx, userID, run.ID, progress); err != nil {
		return store.SyncRun{}, err
	}
	s.notify(userID)
	go s.runCopyMessages(s.baseContextOrDefault(), userID, ids, destMailboxID, dest.Name, run.ID, progress, onDone)
	return run, nil
}

func (s *Service) runCopyMessages(ctx context.Context, userID int64, ids []int64, destMailboxID int64, destName string, runID int64, progress store.SyncProgress, onDone func()) {
	status := "ok"
	errText := ""
	defer func() {
		if ctx.Err() != nil && status == "ok" {
			status = "interrupted"
			errText = "Server stopped before this copy finished."
		}
		if status == "ok" {
			progress.MailboxesDone = 1
		}
		if err := s.Store.FinishSyncRun(context.Background(), userID, runID, status, progress, errText); err != nil {
			log.Printf("finish copy run user_id=%d run_id=%d: %v", userID, runID, err)
		}
		s.notify(userID)
		// Partial and failed copies still need their caller to release scheduler
		// reservations and refresh any destination UIDs that were appended before
		// the failure.
		if onDone != nil {
			onDone()
		}
	}()
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msg, err := s.Store.GetMessageForUser(ctx, userID, id)
		if err != nil {
			status = "failed"
			errText = err.Error()
			return
		}
		progress.CurrentMailbox = "Copying to " + destName
		progress.CurrentUID = msg.UID
		// A single copy can spend several minutes inside IMAP append and its
		// reconciliation commands. Touch the row first so the stale-run reaper
		// cannot mislabel live work between full progress writes.
		if err := s.Store.TouchSyncRun(ctx, userID, runID); err != nil && ctx.Err() == nil {
			log.Printf("touch copy run user_id=%d run_id=%d: %v", userID, runID, err)
		}
		if err := s.CopyMessage(ctx, userID, id, destMailboxID); err != nil {
			status = "failed"
			errText = err.Error()
			return
		}
		progress.MessagesSeen++
		progress.MessagesStored++
		if err := s.Store.UpdateSyncRunProgress(ctx, userID, runID, progress); err != nil {
			status = "failed"
			errText = err.Error()
			return
		}
		s.notify(userID)
	}
}

// CopyMessage writes one message to the destination IMAP mailbox and stores the
// resulting remote UID locally. It never deletes or alters the source message.
func (s *Service) CopyMessage(ctx context.Context, userID, messageID, destMailboxID int64) error {
	if s.Fetcher == nil {
		return errors.New("sync fetcher is not configured")
	}
	msg, err := s.Store.GetMessageForUser(ctx, userID, messageID)
	if err != nil {
		return err
	}
	dest, err := s.Store.GetMailboxForUser(ctx, userID, destMailboxID)
	if err != nil {
		return err
	}
	destAccount, err := s.Store.GetMailAccountForUser(ctx, userID, dest.AccountID)
	if err != nil {
		return err
	}
	raw, err := s.FetchRawMessageForMessage(ctx, userID, msg)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("source message has no raw body to copy")
	}
	date := msg.InternalDate
	if date.IsZero() {
		date = msg.Date
	}
	fingerprint := store.MessageArrivalFingerprint(raw, msg.MessageIDHeader, date, msg.Size)
	transfer, err := s.Store.StageMessageTransfer(ctx, userID, msg.ID, dest.ID, "copy", fingerprint.CanonicalSHA256)
	if err != nil {
		return err
	}
	if transfer.State == "consumed" {
		return nil
	}
	if transfer.State == "succeeded" {
		if mailboxReceivesNewMailNotifications(dest) {
			return nil
		}
		destinationUID, destinationUIDValidity, resolveErr := s.resolveSucceededCopyDestination(ctx,
			destAccount, dest, msg, raw, transfer)
		if resolveErr != nil {
			return resolveErr
		}
		return s.fetchAndCompleteSucceededCopy(ctx, userID, msg, destAccount, dest, raw, date,
			transfer.ID, destinationUID, destinationUIDValidity)
	}
	boundaryFetcher, hasBoundaryFetcher := s.Fetcher.(MailboxAppendBoundaryFetcher)
	matchFetcher, hasMatchFetcher := s.Fetcher.(ExactMessageMatchFetcher)
	if !hasBoundaryFetcher || !hasMatchFetcher {
		return errors.New("IMAP fetcher cannot safely reconcile interrupted copies")
	}
	if transfer.DestinationSnapshotUIDValidity == 0 && transfer.DispatchedAt.IsZero() {
		boundary, boundaryErr := boundaryFetcher.SnapshotMailboxAppendBoundary(ctx, destAccount, dest.Name)
		if boundaryErr != nil {
			return boundaryErr
		}
		transfer, err = s.Store.SetMessageTransferDestinationSnapshot(ctx, userID, transfer.ID,
			int64(boundary.UIDValidity), boundary.UIDNext)
		if err != nil {
			return err
		}
	}
	if transfer.DestinationSnapshotUIDValidity <= 0 || transfer.DestinationSnapshotUIDValidity > int64(^uint32(0)) ||
		transfer.DestinationSnapshotUIDNext == 0 {
		return errors.New("message copy is awaiting reconciliation without a safe destination boundary")
	}
	if !transfer.DispatchedAt.IsZero() {
		if !messageTransferCanReconcile(transfer) {
			return errors.New("message copy is already awaiting remote reconciliation")
		}
		matches, matchErr := matchFetcher.SnapshotExactMessageMatches(ctx, destAccount, dest.Name,
			msg.MessageIDHeader, raw, transfer.DestinationSnapshotUIDNext)
		if matchErr != nil {
			return errors.Join(errors.New("reconcile interrupted message copy"), matchErr)
		}
		if matches.UIDValidity != uint32(transfer.DestinationSnapshotUIDValidity) {
			return errors.New("message copy remains pending because the destination mailbox generation changed")
		}
		if matches.UIDNext == 0 {
			return errors.New("message copy remains pending because exact destination evidence has no UIDNEXT")
		}
		var before, after []uint32
		for _, uid := range matches.MatchingUIDs {
			if uid < transfer.DestinationSnapshotUIDNext {
				before = append(before, uid)
			} else {
				after = append(after, uid)
			}
		}
		if len(before) > 0 || len(after) > 1 {
			return errors.New("message copy remains pending because exact destination matches are ambiguous")
		}
		if len(after) == 1 {
			if err := s.Store.MarkMessageTransferSucceeded(ctx, userID, transfer.ID, after[0], int64(matches.UIDValidity)); err != nil {
				return err
			}
			return s.fetchAndCompleteSucceededCopy(ctx, userID, msg, destAccount, dest, raw, date,
				transfer.ID, after[0], matches.UIDValidity)
		}
		for _, uid := range matches.CandidateUIDs {
			if uid >= transfer.DestinationSnapshotUIDNext {
				return errors.New("message copy remains pending because a post-dispatch candidate is not an exact raw match")
			}
		}
		reopened, reopenErr := s.Store.ReopenMessageTransferDispatchAfterProof(ctx, userID, transfer.ID,
			messageTransferClaim(transfer), processMessageTransferOwner)
		if reopenErr != nil {
			return reopenErr
		}
		if !reopened {
			return errors.New("message copy is already awaiting remote reconciliation")
		}
	}
	claim, claimed, err := s.Store.ClaimMessageTransferDispatchForOwner(ctx, userID, transfer.ID, processMessageTransferOwner)
	if err != nil {
		return err
	}
	if !claimed {
		return errors.New("message copy is already awaiting remote reconciliation")
	}
	fetched, err := s.Fetcher.AppendMessage(ctx, destAccount, dest.Name, raw, msg.MessageIDHeader, date)
	if err != nil {
		var markErr error
		if IsAppendApplied(err) {
			markErr = s.Store.MarkMessageTransferSucceeded(ctx, userID, transfer.ID, 0, 0)
		} else if IsAppendOutcomeUnknown(err) {
			markErr = s.Store.FinishMessageTransferDispatch(ctx, userID, transfer.ID, claim)
		} else {
			markErr = s.Store.MarkMessageTransferFailed(ctx, userID, transfer.ID)
		}
		if markErr != nil {
			return errors.Join(err, markErr)
		}
		return err
	}
	var destinationUID uint32
	var destinationUIDValidity int64
	if fetched.AppendUIDAuthoritative {
		destinationUID = fetched.UID
		destinationUIDValidity = int64(fetched.UIDValidity)
	}
	if err := s.Store.MarkMessageTransferSucceeded(ctx, userID, transfer.ID, destinationUID, destinationUIDValidity); err != nil {
		finishErr := s.Store.FinishMessageTransferDispatch(context.WithoutCancel(ctx), userID, transfer.ID, claim)
		if errors.Is(finishErr, store.ErrNotFound) {
			finishErr = nil
		}
		return errors.Join(err, finishErr)
	}
	return s.completeCopiedMessageLocally(ctx, userID, msg, destAccount, dest, fetched, raw, date, transfer.ID)
}

func (s *Service) fetchAndCompleteSucceededCopy(ctx context.Context, userID int64, source store.MessageRecord, account store.MailAccount, destination store.Mailbox, raw []byte, date time.Time, transferID int64, destinationUID, destinationUIDValidity uint32) error {
	fetched, err := s.Fetcher.FetchMessage(ctx, account, destination.Name, destinationUID)
	if err != nil {
		return errors.Join(errors.New("complete local state for copied message"), err)
	}
	if fetched.UID == 0 {
		fetched.UID = destinationUID
	}
	if fetched.UIDValidity == 0 {
		fetched.UIDValidity = destinationUIDValidity
	}
	if fetched.UID != destinationUID || fetched.UIDValidity != destinationUIDValidity {
		return errors.New("copied message destination generation changed before local completion")
	}
	return s.completeCopiedMessageLocally(ctx, userID, source, account, destination, fetched, raw, date, transferID)
}

func (s *Service) resolveSucceededCopyDestination(ctx context.Context, account store.MailAccount, destination store.Mailbox, source store.MessageRecord, raw []byte, transfer store.MessageTransfer) (uint32, uint32, error) {
	if transfer.DestinationUID > 0 && transfer.DestinationUIDValidity > 0 &&
		transfer.DestinationUIDValidity <= int64(^uint32(0)) {
		return transfer.DestinationUID, uint32(transfer.DestinationUIDValidity), nil
	}
	if transfer.DestinationSnapshotUIDValidity <= 0 ||
		transfer.DestinationSnapshotUIDValidity > int64(^uint32(0)) ||
		transfer.DestinationSnapshotUIDNext == 0 {
		return 0, 0, errors.New("completed message copy has no safe destination boundary")
	}
	matchFetcher, ok := s.Fetcher.(ExactMessageMatchFetcher)
	if !ok {
		return 0, 0, errors.New("IMAP fetcher cannot complete a copied message locally")
	}
	matches, err := matchFetcher.SnapshotExactMessageMatches(ctx, account, destination.Name,
		source.MessageIDHeader, raw, transfer.DestinationSnapshotUIDNext)
	if err != nil {
		return 0, 0, errors.Join(errors.New("resolve completed message copy"), err)
	}
	if matches.UIDValidity != uint32(transfer.DestinationSnapshotUIDValidity) {
		return 0, 0, errors.New("completed message copy destination generation changed")
	}
	var before, after []uint32
	for _, uid := range matches.MatchingUIDs {
		if uid < transfer.DestinationSnapshotUIDNext {
			before = append(before, uid)
		} else {
			after = append(after, uid)
		}
	}
	if len(before) > 0 || len(after) != 1 {
		return 0, 0, errors.New("completed message copy exact destination matches are ambiguous")
	}
	if err := s.Store.MarkMessageTransferSucceeded(ctx, source.UserID, transfer.ID,
		after[0], int64(matches.UIDValidity)); err != nil {
		return 0, 0, err
	}
	return after[0], matches.UIDValidity, nil
}

func (s *Service) completeCopiedMessageLocally(ctx context.Context, userID int64, msg store.MessageRecord, destAccount store.MailAccount, dest store.Mailbox, fetched FetchedMessage, raw []byte, date time.Time, transferID int64) error {
	if fetched.UID == 0 {
		return errors.New("copied IMAP message is missing a UID")
	}
	if fetched.Mailbox == "" {
		fetched.Mailbox = dest.Name
	}
	fetched.Date = msg.Date
	if fetched.InternalDate.IsZero() {
		fetched.InternalDate = date
	}
	if fetched.Size == 0 {
		fetched.Size = int64(len(raw))
	}
	if len(fetched.Raw) == 0 {
		fetched.Raw = raw
	}
	if fetched.UIDValidity > 0 {
		// APPEND's post-write SELECT is authoritative. Clear stale rows before
		// storing its UID so UID reuse across mailbox generations cannot update
		// an old message in place.
		arrivalUIDFloor, err := ArrivalUIDFloorAfterConfirmedUID(fetched.UID)
		if err != nil {
			return err
		}
		if _, err := s.ResetMailboxGenerationIfNeeded(ctx, userID, destAccount, dest,
			fetched.UIDValidity, arrivalUIDFloor); err != nil {
			return err
		}
		refreshedDestination, err := s.Store.GetMailboxForUser(ctx, userID, dest.ID)
		if err != nil {
			return err
		}
		dest = refreshedDestination
	}
	if err := s.applyCopiedMessageFlags(ctx, destAccount, dest, fetched.UID, msg, &fetched); err != nil {
		return err
	}
	copied, parsed, pendingIndex, err := s.storeFetchedMessage(ctx, userID, destAccount, dest, fetched, true)
	if err != nil {
		return err
	}
	searchBatch := newFetchedSearchIndexBatch(s)
	if err := searchBatch.Add(ctx, pendingIndex); err != nil {
		return err
	}
	if err := searchBatch.Flush(ctx); err != nil {
		return err
	}
	// A copied message is a new local row and should receive the same advisory
	// post-index classification as a normally fetched message. Run it only after
	// essential copy state is durable; plugin discovery and classifier failures
	// remain best-effort and cannot turn a completed IMAP copy into a failure.
	if classifiers, _, hookErr := s.postStorePluginHooks(ctx); hookErr != nil {
		log.Printf("copied message classifiers unavailable user_id=%d message_id=%d error_type=%T", userID, copied.ID, hookErr)
	} else {
		s.classifyStoredMessage(ctx, classifiers, copied, parsed)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.Store.MarkMessagesImportCompleted(ctx, userID, []int64{copied.ID}); err != nil {
		return err
	}
	if mailboxReceivesNewMailNotifications(dest) {
		if err := s.recordInboxArrival(ctx, userID, 0, copied, fetched, nil); err != nil {
			return err
		}
	} else if err := s.Store.TerminalizeMessageTransferWithoutArrival(ctx, userID, transferID); err != nil {
		return err
	}
	s.notify(userID)
	return nil
}

func (s *Service) applyCopiedMessageFlags(ctx context.Context, account store.MailAccount, mailbox store.Mailbox, uid uint32, source store.MessageRecord, fetched *FetchedMessage) error {
	needsSeenUpdate := !source.IsRead && hasSeen(fetched.Flags)
	needsFlaggedUpdate := source.IsStarred && !hasFlagged(fetched.Flags)
	if !needsSeenUpdate && !needsFlaggedUpdate {
		return nil
	}
	if fetched.UIDValidity == 0 {
		return errors.New("copied message flag update is missing destination UIDVALIDITY")
	}
	flagFetcher, ok := s.Fetcher.(UIDValidityFlagFetcher)
	if !ok {
		return errors.New("IMAP fetcher cannot prove mailbox generation for copied message flags")
	}
	if needsSeenUpdate {
		applied, err := flagFetcher.SetSeenWithUIDValidity(ctx, account, mailbox.Name, uid, false, fetched.UIDValidity)
		if err != nil {
			return err
		}
		if !applied {
			return errors.New("copied message mailbox generation changed before Seen update")
		}
		fetched.Flags = withoutIMAPFlag(fetched.Flags, "\\Seen")
	}
	if needsFlaggedUpdate {
		applied, err := flagFetcher.SetFlaggedWithUIDValidity(ctx, account, mailbox.Name, uid, true, fetched.UIDValidity)
		if err != nil {
			return err
		}
		if !applied {
			return errors.New("copied message mailbox generation changed before Flagged update")
		}
		fetched.Flags = append(fetched.Flags, "\\Flagged")
	}
	return nil
}

func withoutIMAPFlag(flags []string, target string) []string {
	out := flags[:0]
	for _, flag := range flags {
		if !strings.EqualFold(flag, target) {
			out = append(out, flag)
		}
	}
	return out
}
