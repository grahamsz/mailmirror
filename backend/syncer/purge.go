// File overview: Local folder maintenance actions that purge search documents or local mirror references without touching IMAP.

package syncer

import (
	"context"
	"strings"
	"time"

	"rolltop/backend/store"
)

// PurgeMailboxSearchIndex clears Bleve documents for one local mailbox only.
func (s *Service) PurgeMailboxSearchIndex(ctx context.Context, userID, mailboxID int64) (int, error) {
	return s.PurgeMailboxSearchIndexWithProgress(ctx, userID, mailboxID, 0, nil)
}

// PurgeMailboxSearchIndexWithProgress clears Bleve documents for one local mailbox only and updates a sync-run progress row when provided.
func (s *Service) PurgeMailboxSearchIndexWithProgress(ctx context.Context, userID, mailboxID int64, runID int64, progress *store.SyncProgress) (int, error) {
	if s.Search == nil {
		return 0, nil
	}
	mailbox, err := s.Store.GetMailboxForUser(ctx, userID, mailboxID)
	if err != nil {
		return 0, err
	}
	total, err := s.Search.CountMailboxMessages(ctx, userID, mailboxID)
	if err != nil {
		return 0, err
	}
	if progress != nil {
		progress.MessagesTotal += total
		progress.CurrentMailbox = mailbox.Name
		progress.LatestNewSubject = "Purging full-text index"
		if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
			return 0, err
		}
	}
	// Persist conservative intent before deleting Bleve documents. If deletion
	// is interrupted, progress remains under-counted until an explicit rebuild
	// repairs the mailbox instead of claiming stale coverage.
	if err := s.Store.MarkMailboxSearchIndexPurged(ctx, userID, mailboxID); err != nil {
		return 0, err
	}
	s.notify(userID)
	deleted, err := s.Search.PurgeMailboxWithProgress(ctx, userID, mailboxID, func(n int) error {
		if progress == nil {
			return nil
		}
		progress.MessagesSeen += n
		return s.updateSyncProgress(ctx, userID, runID, *progress)
	})
	if err != nil {
		return deleted, err
	}
	return deleted, nil
}

// PurgeMailboxLocalReferences removes local message/blob rows and search documents
// for one folder, then resets the local UID checkpoint. It never deletes remote IMAP mail.
func (s *Service) PurgeMailboxLocalReferences(ctx context.Context, userID, mailboxID int64) (int, error) {
	return s.PurgeMailboxLocalReferencesWithProgress(ctx, userID, mailboxID, 0, nil)
}

// PurgeMailboxLocalReferencesWithProgress removes local message/blob rows and search documents while reporting background progress.
func (s *Service) PurgeMailboxLocalReferencesWithProgress(ctx context.Context, userID, mailboxID int64, runID int64, progress *store.SyncProgress) (int, error) {
	mailbox, err := s.Store.GetMailboxForUser(ctx, userID, mailboxID)
	if err != nil {
		return 0, err
	}
	localTotal, err := s.Store.CountMessagesForMailbox(ctx, userID, mailboxID)
	if err != nil {
		return 0, err
	}
	searchTotal := 0
	if s.Search != nil {
		searchTotal, err = s.Search.CountMailboxMessages(ctx, userID, mailboxID)
		if err != nil {
			return 0, err
		}
	}
	if progress != nil {
		progress.MessagesTotal += localTotal + searchTotal
		progress.CurrentMailbox = mailbox.Name
		progress.LatestNewSubject = "Purging local references and full-text index"
		if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
			return 0, err
		}
	}
	if err := s.Store.ResetMailboxLastUID(ctx, userID, mailbox.ID); err != nil {
		return 0, err
	}
	purged := 0
	const purgeBatchSize = 10
	for {
		messages, err := s.Store.PurgeMailboxMessageBatch(ctx, userID, mailbox.AccountID, mailbox.ID, purgeBatchSize)
		if err != nil {
			return purged, err
		}
		if len(messages) == 0 {
			break
		}
		purged += len(messages)
		if progress != nil {
			progress.MessagesSeen += len(messages)
			if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
				return purged, err
			}
		}
		s.notify(userID)
		if err := s.cleanupPurgedMessageBlobs(ctx, userID, messages); err != nil {
			return purged, err
		}
		if len(messages) == purgeBatchSize {
			if err := pauseBetweenPurgeBatches(ctx); err != nil {
				return purged, err
			}
		}
	}
	if s.Search != nil {
		if _, err := s.Search.PurgeMailboxWithProgress(ctx, userID, mailboxID, func(n int) error {
			if progress == nil {
				return nil
			}
			progress.MessagesSeen += n
			return s.updateSyncProgress(ctx, userID, runID, *progress)
		}); err != nil {
			return purged, err
		}
	}
	s.notify(userID)
	return purged, nil
}

// PurgeAccountLocalDataWithProgress removes every local message/blob/search
// reference for one IMAP account. It does not touch remote IMAP messages and it
// leaves deleting the account row to the caller so HTTP can decide how to report
// completion.
func (s *Service) PurgeAccountLocalDataWithProgress(ctx context.Context, userID int64, account store.MailAccount, mailboxes []store.Mailbox, runID int64, progress *store.SyncProgress) (int, error) {
	estimate, err := s.Store.AccountPurgeEstimate(ctx, userID, account.ID)
	if err != nil {
		return 0, err
	}
	outboxBlobs, outboxMessageIDs, err := s.Store.PurgeOutboxJobsForIMAPAccount(ctx, userID, account.ID)
	if err != nil {
		return 0, err
	}
	if err := s.cleanupPurgedBlobRecords(ctx, userID, outboxBlobs); err != nil {
		return 0, err
	}
	searchTotal := 0
	if s.Search != nil {
		for _, mailbox := range mailboxes {
			count, err := s.Search.CountMailboxMessages(ctx, userID, mailbox.ID)
			if err != nil {
				return 0, err
			}
			searchTotal += count
		}
	}
	if progress != nil {
		progress.MailboxesTotal = len(mailboxes)
		progress.MessagesTotal = estimate.MessageCount + searchTotal
		progress.CurrentMailbox = accountPurgeLabel(account)
		progress.LatestNewFrom = "rolltop:maintenance"
		progress.LatestNewSubject = "Deleting local IMAP account data"
		progress.MessagesSeen += len(outboxMessageIDs)
		if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
			return 0, err
		}
	}

	purged := len(outboxMessageIDs)
	const purgeBatchSize = 100
	for {
		refs, n, err := s.Store.PurgeAccountMessageBatch(ctx, userID, account.ID, purgeBatchSize)
		if err != nil {
			return purged, err
		}
		if n == 0 {
			break
		}
		purged += n
		if err := s.cleanupPurgedBlobRecords(ctx, userID, refs); err != nil {
			return purged, err
		}
		if progress != nil {
			progress.MessagesSeen += n
			if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
				return purged, err
			}
		}
		s.notify(userID)
		if n == purgeBatchSize {
			if err := pauseBetweenPurgeBatches(ctx); err != nil {
				return purged, err
			}
		}
	}

	if s.Search != nil {
		for _, mailbox := range mailboxes {
			if progress != nil {
				progress.CurrentMailbox = mailbox.Name
				if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
					return purged, err
				}
			}
			if _, err := s.Search.PurgeMailboxWithProgress(ctx, userID, mailbox.ID, func(n int) error {
				if progress == nil {
					return nil
				}
				progress.MessagesSeen += n
				return s.updateSyncProgress(ctx, userID, runID, *progress)
			}); err != nil {
				return purged, err
			}
			if progress != nil {
				progress.MailboxesDone++
			}
		}
	}
	s.notify(userID)
	return purged, nil
}

func pauseBetweenPurgeBatches(ctx context.Context) error {
	const delay = 150 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) cleanupPurgedMessageBlobs(ctx context.Context, userID int64, messages []store.MessageRecord) error {
	if len(messages) == 0 {
		return nil
	}
	for _, msg := range messages {
		if _, err := s.deleteUnreferencedBlob(ctx, userID, msg.BlobID, msg.BlobPath); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) cleanupPurgedBlobRecords(ctx context.Context, userID int64, refs []store.BlobRecord) error {
	if len(refs) == 0 {
		return nil
	}
	for _, ref := range refs {
		if _, err := s.deleteUnreferencedBlob(ctx, userID, ref.ID, ref.Path); err != nil {
			return err
		}
	}
	return nil
}

func accountPurgeLabel(account store.MailAccount) string {
	if strings.TrimSpace(account.Label) != "" {
		return account.Label
	}
	if strings.TrimSpace(account.Email) != "" {
		return account.Email
	}
	return account.Host
}
