// File overview: Bounded, generation-safe IMAP hydration for mailbox search repair.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"rolltop/backend/store"
)

var (
	errSearchRepairRemoteBatchIncomplete = errors.New("mailbox search repair remote batch was incomplete")
	errSearchRepairRemoteGeneration      = errors.New("mailbox search repair remote generation did not match")
)

// A full-text rebuild may hydrate old remote-only message bodies, but one IMAP
// page must never hold its explicit rebuild forever. On deadline the caller
// commits fallback documents and records those messages as eligible for a
// later explicit enrichment instead.
const searchRepairRemotePageTimeout = 45 * time.Second

type searchRepairRecordFunc func(store.MessageRecord, *pendingFetchedSearchIndex) error
type searchRepairFallbackFunc func(store.MessageRecord) error

// repairMailboxSearchRemotePage hydrates one SQLite page using one production
// IMAP session. It returns breaker=true only when the remote batch was unavailable;
// callers then use local previews for remote-only rows on all later pages.
func (s *Service) repairMailboxSearchRemotePage(
	ctx context.Context,
	userID int64,
	requestedMailbox store.Mailbox,
	messages []store.MessageRecord,
	record searchRepairRecordFunc,
	fallback searchRepairFallbackFunc,
) (breaker bool, err error) {
	if len(messages) == 0 {
		return false, nil
	}
	mailbox, err := s.Store.GetMailboxForUser(ctx, userID, requestedMailbox.ID)
	if err != nil {
		return false, err
	}
	if mailbox.AccountID != requestedMailbox.AccountID || mailbox.UserID != userID {
		return false, store.ErrNotFound
	}
	account, err := s.Store.GetMailAccountForUser(ctx, userID, mailbox.AccountID)
	if err != nil {
		return false, err
	}
	if account.UserID != userID || account.ID != mailbox.AccountID {
		return false, store.ErrNotFound
	}

	messageIDs := make([]int64, 0, len(messages))
	for _, msg := range messages {
		messageIDs = append(messageIDs, msg.ID)
	}
	uidValidities, err := s.Store.MessageUIDValiditiesForUser(ctx, userID, messageIDs)
	if err != nil {
		return false, err
	}

	expectedUIDValidity := uint32(0)
	if mailbox.UIDValidity > 0 && mailbox.UIDValidity <= int64(^uint32(0)) {
		expectedUIDValidity = uint32(mailbox.UIDValidity)
	}
	byUID := make(map[uint32]store.MessageRecord, len(messages))
	uids := make([]uint32, 0, len(messages))
	for _, msg := range messages {
		storedUIDValidity, ok := uidValidities[msg.ID]
		if !ok {
			return false, store.ErrNotFound
		}
		if expectedUIDValidity == 0 || storedUIDValidity != int64(expectedUIDValidity) {
			if err := fallback(msg); err != nil {
				return false, err
			}
			continue
		}
		if s.MaxMessageBytes > 0 && msg.Size > s.MaxMessageBytes {
			// Oversized messages were never mirrored in full; remote hydration
			// would be refused, so publish the header-derived fallback now.
			if err := fallback(msg); err != nil {
				return false, err
			}
			continue
		}
		if msg.UserID != userID || msg.AccountID != account.ID || msg.MailboxID != mailbox.ID || msg.UID == 0 {
			return false, store.ErrNotFound
		}
		if _, exists := byUID[msg.UID]; exists {
			return false, fmt.Errorf("duplicate local message UID in mailbox search repair")
		}
		byUID[msg.UID] = msg
		uids = append(uids, msg.UID)
	}
	if len(uids) == 0 {
		return expectedUIDValidity == 0, nil
	}

	processed := make(map[uint32]bool, len(uids))
	fetchCtx, cancelFetch := context.WithTimeout(ctx, searchRepairRemotePageTimeout)
	defer cancelFetch()
	var callbackErr error
	handle := func(item FetchedMessage) error {
		msg, ok := byUID[item.UID]
		if !ok || processed[item.UID] || len(item.Raw) == 0 || item.UIDValidity != expectedUIDValidity {
			return errSearchRepairRemoteGeneration
		}
		retained, err := s.retainFetchedRawMessageForIndexRepair(ctx, userID, msg, account, mailbox, item.Raw)
		if err != nil {
			callbackErr = err
			return err
		}
		itemToIndex, err := s.prepareAttachmentIndexMessageFromRaw(ctx, retained, item.Raw)
		if err != nil {
			callbackErr = err
			return err
		}
		if err := record(retained, itemToIndex); err != nil {
			callbackErr = err
			return err
		}
		s.clearAttachmentIndexRetry(userID, msg.ID)
		processed[item.UID] = true
		return nil
	}

	var fetchErr error
	switch fetcher := s.Fetcher.(type) {
	case UIDValidityMailboxFetcher:
		fetchErr = s.fetchUIDsForGeneration(fetchCtx, account, mailbox.Name, uids, expectedUIDValidity, handle)
	case uidBatchFetcher:
		fetchErr = fetcher.FetchUIDs(fetchCtx, account, mailbox.Name, uids, handle)
	case nil:
		fetchErr = errors.New("mailbox search repair fetcher is not configured")
	default:
		failed := 0
		var firstFetchErr error
		for _, uid := range uids {
			item, singleErr := s.Fetcher.FetchMessage(fetchCtx, account, mailbox.Name, uid)
			if singleErr == nil {
				singleErr = handle(item)
			}
			if callbackErr != nil {
				return false, callbackErr
			}
			if err := fetchCtx.Err(); err != nil {
				return false, err
			}
			if singleErr != nil {
				failed++
				if firstFetchErr == nil {
					firstFetchErr = singleErr
				}
				if err := fallback(byUID[uid]); err != nil {
					return false, err
				}
			}
		}
		if failed > 0 {
			log.Printf("repair mailbox search index legacy remote page deferred user_id=%d account_id=%d mailbox=%q requested=%d enriched=%d deferred=%d error_type=%T",
				userID, account.ID, mailbox.Name, len(uids), len(processed), failed, firstFetchErr)
		}
		return false, nil
	}

	if callbackErr != nil {
		return false, callbackErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	unresolved := make([]store.MessageRecord, 0, len(uids)-len(processed))
	for _, uid := range uids {
		if !processed[uid] {
			unresolved = append(unresolved, byUID[uid])
		}
	}
	if len(unresolved) == 0 {
		return false, nil
	}
	if fetchErr == nil {
		fetchErr = errSearchRepairRemoteBatchIncomplete
	}
	for _, msg := range unresolved {
		if err := fallback(msg); err != nil {
			return false, err
		}
	}
	log.Printf("repair mailbox search index remote page deferred user_id=%d account_id=%d mailbox=%q requested=%d enriched=%d deferred=%d error_type=%T breaker=true",
		userID, account.ID, mailbox.Name, len(uids), len(processed), len(unresolved), fetchErr)
	return true, nil
}
