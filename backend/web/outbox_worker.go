// File overview: Native durable SMTP outbox worker and crash-safe Sent filing.

package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

const (
	outboxScanInterval   = 15 * time.Second
	outboxLeaseDuration  = 5 * time.Minute
	outboxLeaseHeartbeat = time.Minute
	outboxMaxAttempts    = 10
	outboxHistoryTTL     = 30 * 24 * time.Hour
	outboxPruneInterval  = 6 * time.Hour
)

func (s *Server) startOutboxWorker() {
	if s == nil || s.store == nil || s.blobs == nil || s.sender == nil {
		return
	}
	if _, ok := s.sender.(rawMailSender); !ok {
		log.Printf("outbox disabled: configured SMTP sender cannot stream immutable messages")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.outboxCancel = cancel
	s.outboxWorkerID = fmt.Sprintf("rolltop-%d-%d", time.Now().UTC().UnixNano(), rand.Int63())
	s.outboxWG.Add(1)
	go func() {
		defer s.outboxWG.Done()
		s.recoverOutboxJobs(ctx)
		s.runOutboxWorker(ctx)
	}()
}

func (s *Server) wakeOutboxWorker() {
	if s == nil || s.outboxWake == nil {
		return
	}
	select {
	case s.outboxWake <- struct{}{}:
	default:
	}
}

func (s *Server) recoverOutboxJobs(ctx context.Context) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		log.Printf("recover outbox jobs: %v", err)
		return
	}
	for _, user := range users {
		recovered, err := s.store.RecoverInterruptedOutboxJobs(ctx, user.ID)
		if err != nil {
			log.Printf("recover outbox jobs user_id=%d: %v", user.ID, err)
			continue
		}
		if recovered > 0 {
			log.Printf("recovered outbox jobs user_id=%d count=%d", user.ID, recovered)
			s.notifyOutboxChanged(user.ID)
		}
		s.pruneCompletedOutboxHistory(ctx, user.ID)
	}
}

func (s *Server) runOutboxWorker(ctx context.Context) {
	ticker := time.NewTicker(outboxScanInterval)
	defer ticker.Stop()
	pruneTicker := time.NewTicker(outboxPruneInterval)
	defer pruneTicker.Stop()
	for {
		if s.processOneOutboxJob(ctx) {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.outboxWake:
		case <-ticker.C:
		case <-pruneTicker.C:
			s.pruneAllCompletedOutboxHistory(ctx)
		}
	}
}

func (s *Server) pruneAllCompletedOutboxHistory(ctx context.Context) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("scan users for completed outbox history: %v", err)
		}
		return
	}
	for _, user := range users {
		s.pruneCompletedOutboxHistory(ctx, user.ID)
	}
}

func (s *Server) processOneOutboxJob(ctx context.Context) bool {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("scan outbox users: %v", err)
		}
		return false
	}
	for _, user := range users {
		if ctx.Err() != nil {
			return false
		}
		if recovered, recoverErr := s.store.RecoverExpiredOutboxJobs(ctx, user.ID, time.Now().UTC()); recoverErr != nil {
			log.Printf("recover expired outbox jobs user_id=%d: %v", user.ID, recoverErr)
		} else if recovered > 0 {
			log.Printf("recovered expired outbox jobs user_id=%d count=%d", user.ID, recovered)
			s.notifyOutboxChanged(user.ID)
		}
		job, claimed, err := s.store.ClaimNextOutboxJob(ctx, user.ID, s.outboxWorkerID,
			time.Now().UTC(), outboxLeaseDuration)
		if err != nil {
			log.Printf("claim outbox job user_id=%d: %v", user.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		s.processClaimedOutboxJob(ctx, job)
		return true
	}
	return false
}

func (s *Server) pruneCompletedOutboxHistory(ctx context.Context, userID int64) {
	blobs, err := s.store.PurgeCompletedOutboxHistory(ctx, userID, time.Now().UTC().Add(-outboxHistoryTTL))
	if err != nil {
		log.Printf("prune completed outbox history user_id=%d: %v", userID, err)
		return
	}
	for _, blobRecord := range blobs {
		deleted, cleanupErr := s.store.DeleteBlobIfUnreferencedForUser(ctx, userID, blobRecord.ID)
		if cleanupErr != nil {
			log.Printf("cleanup completed outbox blob user_id=%d blob_id=%d: %v", userID, blobRecord.ID, cleanupErr)
			continue
		}
		if deleted && s.blobs != nil {
			_ = s.blobs.DeleteUserBlob(userID, blobRecord.Path)
		}
	}
}

func (s *Server) processClaimedOutboxJob(ctx context.Context, job store.OutboxJob) {
	if job.DeliveryState == store.OutboxDeliveryClaimed {
		s.deliverClaimedOutboxJob(ctx, job)
		return
	}
	if job.DeliveryState == store.OutboxDeliveryAccepted {
		s.fileClaimedOutboxJob(ctx, job)
	}
}

func (s *Server) deliverClaimedOutboxJob(ctx context.Context, job store.OutboxJob) {
	started := time.Now().UTC()
	attempt := job.AttemptCount + 1
	if err := s.store.MarkOutboxSMTPInFlight(ctx, job.UserID, job.ID, s.outboxWorkerID); err != nil {
		log.Printf("start outbox SMTP user_id=%d outbox_id=%d: %v", job.UserID, job.ID, err)
		return
	}
	ctx, stopLease := s.maintainOutboxLease(ctx, job)
	defer stopLease()
	s.notifyOutboxChanged(job.UserID)

	account, err := s.store.GetSMTPAccountForUser(ctx, job.UserID, job.SMTPAccountID)
	if err != nil {
		s.finishOutboxDeliveryFailure(ctx, job, attempt, started, "smtp_account",
			"The outgoing mail account is no longer available.", false, false)
		return
	}
	var recipients []string
	if err := json.Unmarshal([]byte(job.RecipientsJSON), &recipients); err != nil || len(recipients) == 0 {
		s.finishOutboxDeliveryFailure(ctx, job, attempt, started, "invalid_envelope",
			"The queued recipient envelope is invalid.", false, false)
		return
	}
	file, err := s.blobs.OpenUserBlob(job.UserID, job.BlobPath)
	if err != nil {
		s.finishOutboxDeliveryFailure(ctx, job, attempt, started, "spool_missing",
			"The queued message body is unavailable.", false, false)
		return
	}
	defer file.Close()
	if err := verifyOutboxSpool(file, job.RawSize, job.RawSHA256); err != nil {
		s.finishOutboxDeliveryFailure(ctx, job, attempt, started, "spool_corrupt",
			"The queued message failed its integrity check and was not sent.", false, false)
		return
	}
	envelope := store.MailAccount{
		UserID:                job.UserID,
		Email:                 job.EnvelopeFrom,
		SMTPHost:              account.Host,
		SMTPPort:              account.Port,
		SMTPUsername:          account.Username,
		EncryptedSMTPPassword: account.EncryptedPassword,
		SMTPUseTLS:            account.UseTLS,
	}
	err = s.sender.(rawMailSender).SendRawReader(ctx, envelope, recipients, file)
	if err != nil {
		var deliveryErr *smtpclient.DeliveryError
		if errors.As(err, &deliveryErr) {
			kind := string(deliveryErr.Phase)
			if deliveryErr.Outcome == smtpclient.DeliveryUnknown {
				s.finishOutboxDeliveryFailure(ctx, job, attempt, started, kind,
					"Rolltop lost the SMTP response after transmission began. Delivery may have succeeded.",
					false, true)
				return
			}
			s.finishOutboxDeliveryFailure(ctx, job, attempt, started, kind,
				safeOutboxSMTPError(deliveryErr), deliveryErr.Temporary || ctx.Err() != nil, false)
			return
		}
		s.finishOutboxDeliveryFailure(ctx, job, attempt, started, "smtp",
			"SMTP delivery failed before Rolltop could confirm acceptance.", true, false)
		return
	}
	if err := s.store.MarkOutboxSMTPAccepted(ctx, job.UserID, job.ID, s.outboxWorkerID); err != nil {
		// SMTP has accepted the message. A failure to commit at this exact
		// boundary is fundamentally ambiguous, so do not resend here.
		log.Printf("record accepted outbox SMTP user_id=%d outbox_id=%d: %v", job.UserID, job.ID, err)
		if unknownErr := s.store.FailOutboxDelivery(context.WithoutCancel(ctx), job.UserID, job.ID,
			s.outboxWorkerID, store.OutboxDeliveryUnknown, "accept_commit",
			"SMTP accepted the message, but Rolltop could not durably record the acknowledgement. Delivery may have succeeded."); unknownErr != nil {
			log.Printf("record uncertain outbox SMTP user_id=%d outbox_id=%d: %v", job.UserID, job.ID, unknownErr)
		} else {
			s.notifyOutboxChanged(job.UserID)
		}
		return
	}
	_ = s.store.RecordOutboxAttempt(context.WithoutCancel(ctx), job.UserID, job.ID,
		attempt, "smtp", "accepted", "", "", started)
	log.Printf("outbox SMTP accepted user_id=%d outbox_id=%d attempt=%d bytes=%d duration=%s",
		job.UserID, job.ID, attempt, job.RawSize, time.Since(started).Round(time.Millisecond))
	s.notifyOutboxChanged(job.UserID)
	s.wakeOutboxWorker()
}

func verifyOutboxSpool(file *os.File, expectedSize int64, expectedSHA256 string) error {
	if file == nil || expectedSize <= 0 || strings.TrimSpace(expectedSHA256) == "" {
		return errors.New("queued message integrity metadata is incomplete")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != expectedSize {
		return fmt.Errorf("queued message size is %d, expected %d", info.Size(), expectedSize)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedSHA256) {
		return errors.New("queued message checksum does not match")
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

func (s *Server) maintainOutboxLease(parent context.Context, job store.OutboxJob) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(outboxLeaseHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := s.store.RenewOutboxLease(ctx, job.UserID, job.ID, s.outboxWorkerID,
					time.Now().UTC().Add(outboxLeaseDuration))
				if errors.Is(err, store.ErrNotFound) {
					log.Printf("outbox lease lost user_id=%d outbox_id=%d", job.UserID, job.ID)
					cancel()
					return
				}
				if err != nil && ctx.Err() == nil {
					log.Printf("renew outbox lease user_id=%d outbox_id=%d: %v", job.UserID, job.ID, err)
				}
			}
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

func (s *Server) finishOutboxDeliveryFailure(ctx context.Context, job store.OutboxJob, attempt int,
	started time.Time, kind, message string, temporary, unknown bool,
) {
	outcome := "failed"
	var err error
	switch {
	case unknown:
		outcome = "unknown"
		err = s.store.FailOutboxDelivery(context.WithoutCancel(ctx), job.UserID, job.ID,
			s.outboxWorkerID, store.OutboxDeliveryUnknown, kind, message)
	case temporary && attempt < outboxMaxAttempts:
		outcome = "retry_wait"
		err = s.store.RetryOutboxDelivery(context.WithoutCancel(ctx), job.UserID, job.ID,
			s.outboxWorkerID, kind, message, time.Now().UTC().Add(outboxRetryDelay(attempt)))
	default:
		err = s.store.FailOutboxDelivery(context.WithoutCancel(ctx), job.UserID, job.ID,
			s.outboxWorkerID, store.OutboxDeliveryFailed, kind, message)
	}
	if err != nil {
		log.Printf("finish outbox SMTP failure user_id=%d outbox_id=%d: %v", job.UserID, job.ID, err)
		return
	}
	_ = s.store.RecordOutboxAttempt(context.WithoutCancel(ctx), job.UserID, job.ID,
		attempt, "smtp", outcome, kind, message, started)
	log.Printf("outbox SMTP user_id=%d outbox_id=%d attempt=%d outcome=%s error_kind=%s",
		job.UserID, job.ID, attempt, outcome, kind)
	s.notifyOutboxChanged(job.UserID)
}

func safeOutboxSMTPError(err *smtpclient.DeliveryError) string {
	if err == nil {
		return "SMTP delivery failed."
	}
	switch err.Phase {
	case smtpclient.DeliveryPhaseAuthenticate:
		return "The SMTP server rejected the saved login. Check the outgoing mail settings."
	case smtpclient.DeliveryPhaseEnvelope:
		return "The SMTP server rejected the sender or a recipient."
	case smtpclient.DeliveryPhaseTLS:
		return "Rolltop could not establish a secure connection to the SMTP server."
	case smtpclient.DeliveryPhaseConnect:
		return "Rolltop could not connect to the SMTP server."
	default:
		return "The SMTP server did not accept the message."
	}
}

func outboxRetryDelay(attempt int) time.Duration {
	switch attempt {
	case 0, 1:
		return 30 * time.Second
	case 2:
		return 2 * time.Minute
	case 3:
		return 5 * time.Minute
	case 4:
		return 15 * time.Minute
	case 5:
		return 30 * time.Minute
	default:
		return time.Hour
	}
}

func (s *Server) fileClaimedOutboxJob(ctx context.Context, job store.OutboxJob) {
	ctx, stopLease := s.maintainOutboxLease(ctx, job)
	defer stopLease()
	if s.syncer == nil || s.syncer.Fetcher == nil {
		s.retryOutboxFiling(ctx, job, store.OutboxFilingAttention, "imap_unavailable",
			"Message delivered, but IMAP is unavailable for saving the Sent copy.", true)
		return
	}
	account, err := s.store.GetMailAccountForUser(ctx, job.UserID, job.IMAPAccountID)
	if err != nil {
		s.retryOutboxFiling(ctx, job, store.OutboxFilingAttention, "imap_account",
			"Message delivered, but its IMAP account is no longer available.", true)
		return
	}
	mailbox, err := s.store.GetMailboxForUser(ctx, job.UserID, job.SentMailboxID)
	if err != nil || mailbox.AccountID != account.ID {
		s.retryOutboxFiling(ctx, job, store.OutboxFilingAttention, "sent_mailbox",
			"Message delivered, but its configured Sent folder is no longer available.", true)
		return
	}
	file, err := s.blobs.OpenUserBlob(job.UserID, job.BlobPath)
	if err != nil {
		s.retryOutboxFiling(ctx, job, store.OutboxFilingAttention, "spool_missing",
			"Message delivered, but its local Sent payload is unavailable.", true)
		return
	}
	if err := verifyOutboxSpool(file, job.RawSize, job.RawSHA256); err != nil {
		_ = file.Close()
		s.retryOutboxFiling(ctx, job, store.OutboxFilingAttention, "spool_corrupt",
			"Message delivered, but its local Sent payload failed its integrity check.", true)
		return
	}
	raw, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil {
		s.retryOutboxFiling(ctx, job, store.OutboxFilingRetryWait, "spool_read",
			"Message delivered; Rolltop will retry reading its local Sent payload.", false)
		return
	}
	finishForeground, err := s.beginComposeForegroundOperationWithin(ctx, job.UserID, composeForegroundReservationWait)
	if err != nil {
		s.retryOutboxFiling(ctx, job, store.OutboxFilingRetryWait, "imap_busy",
			"Message delivered; Rolltop will retry saving it to Sent.", false)
		return
	}
	defer finishForeground()

	var fetched syncer.FetchedMessage
	if job.FilingState == store.OutboxFilingReconciling ||
		(job.AppendUIDValidity > 0 && job.AppendUIDNext > 0) {
		fetched, err = s.reconcileOutboxSentAppend(ctx, job, account, mailbox, raw)
		if err != nil {
			s.handleOutboxFilingError(ctx, job, err)
			return
		}
		if fetched.UID == 0 {
			if err := s.store.MarkOutboxFilingAppending(ctx, job.UserID, job.ID, s.outboxWorkerID); err != nil {
				log.Printf("mark outbox filing append user_id=%d outbox_id=%d: %v", job.UserID, job.ID, err)
				return
			}
		}
	}
	if fetched.UID == 0 {
		if job.AppendUIDValidity == 0 || job.AppendUIDNext == 0 {
			matcher, ok := s.syncer.Fetcher.(syncer.ExactMessageMatchFetcher)
			if !ok {
				s.retryOutboxFiling(ctx, job, store.OutboxFilingAttention, "append_reconciliation",
					"Message delivered, but this IMAP server cannot safely reconcile an interrupted Sent copy.", true)
				return
			}
			snapshot, snapshotErr := matcher.SnapshotExactMessageMatches(ctx, account, mailbox.Name,
				job.MessageIDHeader, raw, 1)
			if snapshotErr != nil {
				s.retryOutboxFiling(ctx, job, store.OutboxFilingRetryWait, "append_boundary",
					"Message delivered; Rolltop will retry preparing its Sent copy.", false)
				return
			}
			if len(snapshot.CandidateUIDs) > 0 {
				if len(snapshot.CandidateUIDs) != 1 || len(snapshot.MatchingUIDs) != 1 {
					s.retryOutboxFiling(ctx, job, store.OutboxFilingAttention, "sent_match",
						"Message delivered, but existing Sent-folder copies with the same Message-ID are ambiguous.", true)
					return
				}
				fetched, err = s.syncer.Fetcher.FetchMessage(ctx, account, mailbox.Name, snapshot.MatchingUIDs[0])
				if err != nil {
					s.retryOutboxFiling(ctx, job, store.OutboxFilingRetryWait, "sent_match_fetch",
						"Message delivered; Rolltop will retry confirming the matching Sent copy.", false)
					return
				}
				if fetched.UIDValidity == 0 {
					fetched.UIDValidity = snapshot.UIDValidity
				}
			}
			if fetched.UID != 0 {
				if err := s.finishOutboxSentFiling(ctx, job, account, mailbox, fetched); err != nil {
					s.retryOutboxFiling(ctx, job, store.OutboxFilingReconciling, "sent_local",
						"Message delivered and appears to be in Sent; Rolltop will retry confirming its local copy.", false)
					log.Printf("complete existing outbox Sent filing user_id=%d outbox_id=%d: %v", job.UserID, job.ID, err)
				}
				return
			}
			if err := s.store.SetOutboxAppendBoundary(ctx, job.UserID, job.ID, s.outboxWorkerID,
				snapshot.UIDValidity, snapshot.UIDNext); err != nil {
				log.Printf("store outbox append boundary user_id=%d outbox_id=%d: %v", job.UserID, job.ID, err)
				return
			}
			job.AppendUIDValidity = snapshot.UIDValidity
			job.AppendUIDNext = snapshot.UIDNext
		}
		fetched, err = s.syncer.Fetcher.AppendMessage(ctx, account, mailbox.Name, raw,
			job.MessageIDHeader, job.CreatedAt)
		if err != nil {
			s.handleOutboxFilingError(ctx, job, err)
			return
		}
	}
	if fetched.UID == 0 || fetched.UIDValidity == 0 {
		s.handleOutboxFilingError(ctx, job,
			syncer.AppendApplied(errors.New("IMAP did not confirm the Sent UID")))
		return
	}
	if err := s.finishOutboxSentFiling(ctx, job, account, mailbox, fetched); err != nil {
		s.retryOutboxFiling(ctx, job, store.OutboxFilingReconciling, "sent_local",
			"Message delivered and appears to be in Sent; Rolltop will retry confirming its local copy.", false)
		log.Printf("complete outbox Sent filing user_id=%d outbox_id=%d: %v", job.UserID, job.ID, err)
	}
}

func (s *Server) reconcileOutboxSentAppend(ctx context.Context, job store.OutboxJob,
	account store.MailAccount, mailbox store.Mailbox, raw []byte,
) (syncer.FetchedMessage, error) {
	if job.AppendUIDValidity == 0 || job.AppendUIDNext == 0 {
		return syncer.FetchedMessage{}, errors.New("Sent append reconciliation has no durable boundary")
	}
	matcher, ok := s.syncer.Fetcher.(syncer.ExactMessageMatchFetcher)
	if !ok {
		return syncer.FetchedMessage{}, errors.New("IMAP server cannot reconcile the interrupted Sent append")
	}
	snapshot, err := matcher.SnapshotExactMessageMatches(ctx, account, mailbox.Name,
		job.MessageIDHeader, raw, job.AppendUIDNext)
	if err != nil {
		return syncer.FetchedMessage{}, err
	}
	if snapshot.UIDValidity != job.AppendUIDValidity {
		return syncer.FetchedMessage{}, errors.New("Sent mailbox generation changed during append reconciliation")
	}
	before := 0
	var after []uint32
	for _, uid := range snapshot.MatchingUIDs {
		if uid < job.AppendUIDNext {
			before++
		} else {
			after = append(after, uid)
		}
	}
	if before > 0 || len(after) > 1 {
		return syncer.FetchedMessage{}, errors.New("exact Sent matches are ambiguous")
	}
	if len(after) == 1 {
		fetched, err := s.syncer.Fetcher.FetchMessage(ctx, account, mailbox.Name, after[0])
		if err != nil {
			return syncer.FetchedMessage{}, err
		}
		if fetched.UIDValidity == 0 {
			fetched.UIDValidity = snapshot.UIDValidity
		}
		return fetched, nil
	}
	for _, uid := range snapshot.CandidateUIDs {
		if uid >= job.AppendUIDNext {
			return syncer.FetchedMessage{}, errors.New("a post-append Message-ID candidate did not match the queued message")
		}
	}
	return syncer.FetchedMessage{}, nil
}

func (s *Server) handleOutboxFilingError(ctx context.Context, job store.OutboxJob, err error) {
	switch {
	case syncer.IsAppendOutcomeUnknown(err), syncer.IsAppendApplied(err):
		s.retryOutboxFiling(ctx, job, store.OutboxFilingReconciling, "append_unknown",
			"Message delivered; Rolltop is confirming whether the Sent copy was stored.", false)
	default:
		s.retryOutboxFiling(ctx, job, store.OutboxFilingRetryWait, "append",
			"Message delivered; Rolltop will retry saving its Sent copy.", false)
	}
}

func (s *Server) retryOutboxFiling(ctx context.Context, job store.OutboxJob, state, kind, message string, attention bool) {
	if !attention && job.FilingAttemptCount+1 >= outboxMaxAttempts {
		state = store.OutboxFilingAttention
		attention = true
		message = "Message delivered, but Rolltop stopped retrying its Sent copy after repeated failures. Retry when IMAP is healthy."
	}
	delay := outboxRetryDelay(job.FilingAttemptCount + 1)
	if attention {
		delay = 24 * time.Hour
	}
	if err := s.store.RetryOutboxFiling(context.WithoutCancel(ctx), job.UserID, job.ID,
		s.outboxWorkerID, state, kind, message, time.Now().UTC().Add(delay), attention); err != nil {
		log.Printf("retry outbox filing user_id=%d outbox_id=%d: %v", job.UserID, job.ID, err)
		return
	}
	s.notifyOutboxChanged(job.UserID)
}

func (s *Server) finishOutboxSentFiling(ctx context.Context, job store.OutboxJob,
	account store.MailAccount, mailbox store.Mailbox, fetched syncer.FetchedMessage,
) error {
	arrivalFloor, err := syncer.ArrivalUIDFloorAfterConfirmedUID(fetched.UID)
	if err != nil {
		return err
	}
	if _, err := s.syncer.ResetMailboxGenerationIfNeeded(ctx, job.UserID, account, mailbox,
		fetched.UIDValidity, arrivalFloor); err != nil {
		return err
	}
	displaced, err := s.store.PromoteOutboxMessage(ctx, job.UserID, job.ID,
		fetched.UID, fetched.UIDValidity)
	if err != nil {
		return err
	}
	if displaced != nil {
		if s.search != nil {
			_ = s.search.DeleteMessage(ctx, displaced.UserID, displaced.ID)
		}
		deleted, cleanupErr := s.store.DeleteBlobIfUnreferencedForUser(ctx, job.UserID, displaced.BlobID)
		if cleanupErr == nil && deleted && s.blobs != nil && displaced.BlobPath != "" {
			_ = s.blobs.DeleteUserBlob(job.UserID, displaced.BlobPath)
		}
	}
	if err := s.store.CompleteOutboxFiling(ctx, job.UserID, job.ID, s.outboxWorkerID,
		fetched.UID, fetched.UIDValidity); err != nil {
		return err
	}
	log.Printf("outbox Sent filing complete user_id=%d outbox_id=%d uid=%d",
		job.UserID, job.ID, fetched.UID)
	s.notifyOutboxChanged(job.UserID)
	return nil
}

func (s *Server) notifyOutboxChanged(userID int64) {
	s.noteMailListChanged(userID)
	if s.events != nil {
		s.events.Notify(userID)
	}
}
