// File overview: Search indexing helpers used during sync.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/backend/plugins"
	"rolltop/backend/search"
	"rolltop/backend/store"
)

// storeFetchedMessage always persists message and attachment metadata. Callers
// may defer the derived search document during generation recovery so attachment
// text extraction and Bleve writes run later in the attachment-index worker.
func (s *Service) storeFetchedMessage(ctx context.Context, userID int64, account store.MailAccount, mailbox store.Mailbox,
	item FetchedMessage, prepareSearchDocument bool,
) (store.MessageRecord, mailparse.ParsedMessage, *pendingFetchedSearchIndex, error) {
	generationRecoveryStartMessage(ctx, item.UID)
	generationRecoveryPhase(ctx, "mime-parse", "")
	parsed, err := mailparse.Parse(item.Raw)
	if err != nil {
		parsed = mailparse.ParsedMessage{
			Subject: fmt.Sprintf("Unparseable message UID %d", item.UID),
			Text:    fmt.Sprintf("rolltop stored the raw message, but could not parse its MIME body: %v. Download the raw .eml to inspect it.", err),
		}
	}
	generationRecoveryPhase(ctx, "plugin-security", "")
	securityState, securityHandled, err := s.detectMessageSecurity(ctx, userID, item.Raw, plugins.MessageBody{Purpose: "storage", Text: parsed.Text, HTML: parsed.HTML})
	if err != nil {
		return store.MessageRecord{}, parsed, nil, err
	}
	if securityHandled {
		parsed.IsEncrypted = securityState.Encrypted
		parsed.IsSigned = securityState.Signed
		if transform, err := s.transformMessageSecurityBody(ctx, userID, item.Raw, securityState, plugins.MessageBody{Purpose: "storage", Text: parsed.Text, HTML: parsed.HTML}); err != nil {
			return store.MessageRecord{}, parsed, nil, err
		} else if transform.Applied {
			parsed.Text = transform.Body.Text
			parsed.HTML = transform.Body.HTML
			if transform.DropAttachments {
				parsed.Files = nil
			}
		}
	}
	date := parsed.Date
	if date.IsZero() {
		date = item.Date
	}
	if date.IsZero() {
		date = item.InternalDate
	}
	if item.Oversized {
		// Only the leading header block exists locally. Keep the row listable
		// and searchable by subject/from, but never persist a partial body as
		// if it were the real message.
		parsed.Text = oversizedMessageBodyText(item.Size)
		parsed.HTML = ""
		parsed.Files = nil
	}
	fingerprint := store.MessageArrivalFingerprint(item.Raw, parsed.MessageID, item.InternalDate, item.Size)

	rawHash := sha256Hex(item.Raw)
	blobPath := ""
	blobRecordPath := remoteMessagePath(userID, account.ID, item.Mailbox, item.UID, rawHash)
	blobKind := "message-remote"
	blobSize := int64(0)
	if !item.Oversized && s.shouldRetainBlob(date) {
		generationRecoveryPhase(ctx, "blob-save", "")
		saved, err := s.Blobs.SaveRawMessage(userID, account.ID, item.Mailbox, item.UID, item.Raw)
		if err != nil {
			return store.MessageRecord{}, parsed, nil, err
		}
		blobPath = saved.Path
		blobRecordPath = saved.Path
		blobKind = "message"
		blobSize = saved.Size
		rawHash = saved.SHA256
	}
	generationRecoveryPhase(ctx, "sqlite-create-blob", "")
	blobRec, err := s.Store.CreateBlob(ctx, store.BlobRecord{
		UserID: userID,
		Kind:   blobKind,
		Path:   blobRecordPath,
		SHA256: rawHash,
		Size:   blobSize,
	})
	if err != nil {
		return store.MessageRecord{}, parsed, nil, err
	}

	languageCode := ""
	generationRecoveryPhase(ctx, "sqlite-plugin-enabled", plugins.LanguageSearch)
	if s.pluginEnabled(ctx, plugins.LanguageSearch) {
		generationRecoveryPhase(ctx, "language-detect", plugins.LanguageSearch)
		languageCode = detectLanguageCode(parsed.Subject, parsed.Text)
	}
	generationRecoveryPhase(ctx, "sqlite-create-message", "")
	msg, err := s.Store.CreateMessage(ctx, store.CreateMessage{
		UserID:           userID,
		AccountID:        account.ID,
		MailboxID:        mailbox.ID,
		BlobID:           blobRec.ID,
		MessageIDHeader:  parsed.MessageID,
		CanonicalSHA256:  fingerprint.CanonicalSHA256,
		MessageIDHash:    fingerprint.MessageIDHash,
		InReplyTo:        parsed.InReplyTo,
		ReferencesHeader: parsed.References,
		Subject:          parsed.Subject,
		LanguageCode:     languageCode,
		FromAddr:         parsed.From,
		ToAddr:           parsed.To,
		CCAddr:           parsed.CC,
		Date:             date,
		InternalDate:     item.InternalDate,
		UID:              item.UID,
		UIDValidity:      mailbox.UIDValidity,
		Size:             fingerprint.Size,
		BlobPath:         blobPath,
		BodyText:         store.MessageBodyPreview(parsed.Text, store.DefaultMessageBodyPreviewBytes),
		BodyHTML:         "",
		IsRead:           hasSeen(item.Flags),
		IsStarred:        hasFlagged(item.Flags),
		HasAttachments:   len(parsed.Files) > 0,
		IsEncrypted:      parsed.IsEncrypted,
		IsSigned:         parsed.IsSigned,
		ImportPending:    true,
	})
	if err != nil {
		_, cleanupErr := s.deleteUnreferencedBlob(ctx, userID, blobRec.ID, blobPath)
		return store.MessageRecord{}, parsed, nil, errors.Join(err, cleanupErr)
	}
	if msg.LanguageCode != languageCode {
		msg.LanguageCode = languageCode
		if err := s.Store.UpdateMessageLanguage(ctx, userID, msg.ID, languageCode); err != nil {
			return store.MessageRecord{}, parsed, nil, err
		}
	}
	generationRecoveryPhase(ctx, "sqlite-create-location", "")
	if err := s.Store.CreateLocation(ctx, userID, msg.ID, mailbox.ID, item.UID); err != nil {
		return store.MessageRecord{}, parsed, nil, err
	}
	generationRecoveryPhase(ctx, "plugin-incoming-message", "discover")
	if err := s.importIncomingMessageHooks(ctx, userID, item.Raw, parsed.From); err != nil {
		return store.MessageRecord{}, parsed, nil, err
	}
	searchVisible := mailbox.IncludeInSearch && s.Search != nil
	prepareSearchDocument = prepareSearchDocument && searchVisible
	attachmentDocs := make([]search.AttachmentDoc, 0, len(parsed.Files))
	visibleAttachmentCount := 0
	if len(parsed.Files) > 0 {
		generationRecoveryPhase(ctx, "sqlite-delete-attachments", "")
		if err := s.Store.DeleteAttachmentsForMessage(ctx, userID, msg.ID); err != nil {
			return store.MessageRecord{}, parsed, nil, err
		}
	}
	for _, file := range parsed.Files {
		generationRecoveryPhase(ctx, "sqlite-create-attachment", "")
		if _, err := s.Store.CreateAttachment(ctx, store.Attachment{
			UserID:      userID,
			MessageID:   msg.ID,
			BlobID:      blobRec.ID,
			Filename:    file.Filename,
			ContentType: file.ContentType,
			ContentID:   file.ContentID,
			IsInline:    file.IsInline,
			Size:        int64(len(file.Data)),
			BlobPath:    "",
		}); err != nil {
			return store.MessageRecord{}, parsed, nil, err
		}
		if !file.IsInline {
			visibleAttachmentCount++
			if prepareSearchDocument {
				generationRecoveryPhase(ctx, "search-extract-attachment", "attachment-text")
				attachmentDocs = append(attachmentDocs, search.AttachmentDoc{
					Filename:    file.Filename,
					ContentType: file.ContentType,
					Text:        file.SearchableText(),
				})
			}
		}
	}
	msg.HasAttachments = visibleAttachmentCount > 0
	generationRecoveryMessageStored(ctx, item.UID)
	if prepareSearchDocument {
		generationRecoveryPhase(ctx, "search-prepare", "")
		indexMsg := msg
		indexMsg.BodyText = parsed.Text
		indexMsg.BodyHTML = ""
		return msg, parsed, &pendingFetchedSearchIndex{
			Document: search.MessageIndexDocument{
				Message:     indexMsg,
				Attachments: attachmentDocs,
			},
			HasVisibleAttachments: visibleAttachmentCount > 0,
		}, nil
	}
	if searchVisible {
		// Keep attachment_indexed_at unset. IndexPendingAttachmentsForUser will
		// reparse the raw message and commit the complete document after recovery.
		return msg, parsed, nil, nil
	}
	generationRecoveryPhase(ctx, "sqlite-mark-attachment-indexed", "")
	if err := s.Store.MarkMessageAttachmentIndexed(ctx, userID, msg.ID, visibleAttachmentCount > 0); err != nil {
		return store.MessageRecord{}, parsed, nil, err
	}
	return msg, parsed, nil, nil
}

// IndexPendingAttachmentsForUser indexes attachment text from raw message bodies for pending messages.
func (s *Service) IndexPendingAttachmentsForUser(ctx context.Context, userID int64, limit int) (int, error) {
	if s.Search == nil {
		return 0, nil
	}
	ctx = search.WithBackgroundIndexing(ctx)
	// New mail is indexed synchronously during IMAP import. Do not turn pending
	// historical attachment metadata into a whole-mailbox background rebuild:
	// even local raw retention can make that consume the writer for hours.
	if !s.AllowBackgroundAttachmentHydration {
		if skipped, err := s.Store.SkipPendingHistoricalAttachmentIndexing(ctx, userID); err != nil {
			return 0, err
		} else if skipped > 0 {
			log.Printf("attachment index skipped historical messages user_id=%d count=%d", userID, skipped)
		}
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	cursor := s.attachmentIndexCursorForUser(userID)
	messages, wrapped, err := s.Store.ListMessagesNeedingAttachmentIndexAfter(ctx, userID, cursor, limit)
	if err != nil {
		return 0, err
	}
	// Maintenance is routinely preempted by foreground mailbox work. Checkpoint
	// smaller prefixes so a cancellation cannot repeatedly discard an entire
	// slow remote-hydration page.
	batch := newFetchedSearchIndexBatchWithSize(s, maintenanceSearchCheckpointSize)
	indexed := 0
	enriched := 0
	deferred := 0
	coolingDown := 0
	type remoteMailboxPage struct {
		mailbox  store.Mailbox
		messages []store.MessageRecord
	}
	remotePages := map[int64]*remoteMailboxPage{}
	remotePageOrder := make([]int64, 0)
	mailboxes := map[int64]store.Mailbox{}
	_, canBatchRemote := s.Fetcher.(UIDValidityMailboxFetcher)
	recordEnriched := func(msg store.MessageRecord, item *pendingFetchedSearchIndex) error {
		if err := batch.Add(ctx, item); err != nil {
			return err
		}
		s.clearAttachmentIndexRetry(userID, msg.ID)
		indexed++
		enriched++
		return nil
	}
	recordDeferred := func(msg store.MessageRecord, cause error) error {
		retryAt := s.deferAttachmentIndexRetry(userID, msg.ID, time.Now())
		log.Printf("attachment index message deferred user_id=%d account_id=%d mailbox_id=%d message_id=%d uid=%d stage=raw-fetch error_type=%T retry_in=%s",
			userID, msg.AccountID, msg.MailboxID, msg.ID, msg.UID, cause, time.Until(retryAt).Round(time.Second))
		if err := batch.Add(ctx, &pendingFetchedSearchIndex{
			Document:    search.MessageIndexDocument{Message: msg},
			KeepPending: true,
		}); err != nil {
			return err
		}
		indexed++
		deferred++
		return nil
	}
	for _, msg := range messages {
		if err := ctx.Err(); err != nil {
			return indexed, err
		}
		now := time.Now()
		if !s.attachmentIndexRetryReady(userID, msg.ID, now) {
			coolingDown++
			continue
		}
		mailbox, ok := mailboxes[msg.MailboxID]
		if !ok {
			mailbox, err = s.Store.GetMailboxForUser(ctx, userID, msg.MailboxID)
			if err != nil {
				return indexed, err
			}
			mailboxes[msg.MailboxID] = mailbox
		}
		raw, local, err := s.readLocalRawMessageForIndexRepair(userID, msg)
		if err != nil {
			return indexed, err
		}
		if !local {
			if s.AllowBackgroundAttachmentHydration {
				page := remotePages[msg.MailboxID]
				if page == nil {
					page = &remoteMailboxPage{mailbox: mailbox}
					remotePages[msg.MailboxID] = page
					remotePageOrder = append(remotePageOrder, msg.MailboxID)
				}
				if canBatchRemote && mailbox.IncludeInSearch {
					page.messages = append(page.messages, msg)
					continue
				}
				item, err := s.prepareAttachmentIndexMessage(ctx, msg)
				if err == nil {
					if err := recordEnriched(msg, item); err != nil {
						return indexed, err
					}
					continue
				}
				var deferredErr *attachmentIndexDeferredError
				if !errors.As(err, &deferredErr) {
					return indexed, err
				}
				if err := recordDeferred(msg, deferredErr.err); err != nil {
					return indexed, err
				}
				continue
			}
			// A blob path can outlive a deleted file. Commit the existing message
			// document as-is and complete the derived task rather than hydrating
			// old mail remotely in the background.
			if err := batch.Add(ctx, &pendingFetchedSearchIndex{
				Document:              search.MessageIndexDocument{Message: msg},
				HasVisibleAttachments: msg.HasAttachments,
			}); err != nil {
				return indexed, err
			}
			indexed++
			continue
		}
		item, err := s.prepareAttachmentIndexMessageFromRaw(ctx, msg, raw)
		if err != nil {
			return indexed, err
		}
		if err := recordEnriched(msg, item); err != nil {
			return indexed, err
		}
	}
	if s.AllowBackgroundAttachmentHydration {
		for _, mailboxID := range remotePageOrder {
			page := remotePages[mailboxID]
			_, err := s.repairMailboxSearchRemotePage(ctx, userID, page.mailbox, page.messages,
				func(msg store.MessageRecord, item *pendingFetchedSearchIndex) error { return recordEnriched(msg, item) },
				func(msg store.MessageRecord) error { return recordDeferred(msg, nil) })
			if err != nil {
				return indexed, err
			}
		}
	}
	if err := batch.Flush(ctx); err != nil {
		return indexed, err
	}
	if len(messages) > 0 {
		s.advanceAttachmentIndexCursor(userID, messages[len(messages)-1].ID)
	}
	if coolingDown > 0 {
		log.Printf("attachment index batch user_id=%d inspected=%d indexed=%d enriched=%d cooling_down=%d wrapped=%t",
			userID, len(messages), indexed, enriched, coolingDown, wrapped)
	}
	if enriched == 0 && deferred+coolingDown == len(messages) && len(messages) == limit &&
		(!wrapped || deferred > 0) {
		now := time.Now()
		continueAt := s.deferAttachmentIndexContinuation(userID, now)
		log.Printf("attachment index continuation scheduled user_id=%d after_message_id=%d resume_in=%s",
			userID, messages[len(messages)-1].ID, continueAt.Sub(now).Round(time.Millisecond))
		return 0, nil
	}
	// A wrapped page containing only cooling-down rows has completed a cursor
	// cycle. Stop here and let the earliest per-message retry wake the worker.
	if wrapped && enriched == 0 && len(messages) == limit {
		return 0, nil
	}
	return len(messages), nil
}

// IndexAttachmentsForMessage reparses one raw message, indexes its attachment text, and updates message metadata.
func (s *Service) IndexAttachmentsForMessage(ctx context.Context, msg store.MessageRecord) error {
	batch := newFetchedSearchIndexBatch(s)
	item, err := s.prepareAttachmentIndexMessage(ctx, msg)
	if err != nil {
		return err
	}
	if err := batch.Add(ctx, item); err != nil {
		return err
	}
	return batch.Flush(ctx)
}

// prepareAttachmentIndexMessage refreshes attachment rows and returns a pending
// Bleve document. The caller owns batching and the post-commit metadata mark.
func (s *Service) prepareAttachmentIndexMessage(ctx context.Context, msg store.MessageRecord) (*pendingFetchedSearchIndex, error) {
	if s.Search == nil {
		return nil, nil
	}
	mailbox, err := s.Store.GetMailboxForUser(ctx, msg.UserID, msg.MailboxID)
	if err != nil {
		return nil, err
	}
	if !mailbox.IncludeInSearch {
		if err := s.Search.DeleteMessage(ctx, msg.UserID, msg.ID); err != nil {
			return nil, err
		}
		return nil, s.Store.MarkMessageAttachmentIndexed(ctx, msg.UserID, msg.ID, msg.HasAttachments)
	}
	raw, err := s.FetchRawMessageForMessage(ctx, msg.UserID, msg)
	if err != nil {
		return nil, &attachmentIndexDeferredError{stage: "raw-fetch", err: err}
	}
	return s.prepareSearchVisibleAttachmentIndexMessageFromRaw(ctx, msg, raw)
}

func (s *Service) prepareAttachmentIndexMessageFromRaw(ctx context.Context, msg store.MessageRecord, raw []byte) (*pendingFetchedSearchIndex, error) {
	if s.Search == nil {
		return nil, nil
	}
	mailbox, err := s.Store.GetMailboxForUser(ctx, msg.UserID, msg.MailboxID)
	if err != nil {
		return nil, err
	}
	if !mailbox.IncludeInSearch {
		if err := s.Search.DeleteMessage(ctx, msg.UserID, msg.ID); err != nil {
			return nil, err
		}
		return nil, s.Store.MarkMessageAttachmentIndexed(ctx, msg.UserID, msg.ID, msg.HasAttachments)
	}
	return s.prepareSearchVisibleAttachmentIndexMessageFromRaw(ctx, msg, raw)
}

func (s *Service) prepareSearchVisibleAttachmentIndexMessageFromRaw(ctx context.Context, msg store.MessageRecord, raw []byte) (*pendingFetchedSearchIndex, error) {
	parsed, err := mailparse.Parse(raw)
	if err != nil {
		securityState, securityHandled, securityErr := s.detectMessageSecurity(ctx, msg.UserID, raw, plugins.MessageBody{Purpose: "storage", Text: msg.BodyText, HTML: msg.BodyHTML})
		if securityErr != nil {
			return nil, securityErr
		}
		if securityHandled {
			msg.IsEncrypted = securityState.Encrypted
			msg.IsSigned = securityState.Signed
			if err := s.Store.UpdateMessageSecurityState(ctx, msg.UserID, msg.ID, msg.IsEncrypted, msg.IsSigned); err != nil {
				return nil, err
			}
		}
		if msg.LanguageCode == "" && s.pluginEnabled(ctx, plugins.LanguageSearch) {
			msg.LanguageCode = detectLanguageCode(msg.Subject, msg.BodyText)
			if err := s.Store.UpdateMessageLanguage(ctx, msg.UserID, msg.ID, msg.LanguageCode); err != nil {
				return nil, err
			}
		}
		return &pendingFetchedSearchIndex{
			Document: search.MessageIndexDocument{Message: msg},
		}, nil
	}
	securityState, securityHandled, err := s.detectMessageSecurity(ctx, msg.UserID, raw, plugins.MessageBody{Purpose: "storage", Text: parsed.Text, HTML: parsed.HTML})
	if err != nil {
		return nil, err
	}
	if securityHandled {
		parsed.IsEncrypted = securityState.Encrypted
		parsed.IsSigned = securityState.Signed
		if transform, err := s.transformMessageSecurityBody(ctx, msg.UserID, raw, securityState, plugins.MessageBody{Purpose: "storage", Text: parsed.Text, HTML: parsed.HTML}); err != nil {
			return nil, err
		} else if transform.Applied {
			parsed.Text = transform.Body.Text
			parsed.HTML = transform.Body.HTML
			if transform.DropAttachments {
				parsed.Files = nil
			}
		}
	}
	msg.IsEncrypted = parsed.IsEncrypted
	msg.IsSigned = parsed.IsSigned
	if err := s.Store.UpdateMessageSecurityState(ctx, msg.UserID, msg.ID, msg.IsEncrypted, msg.IsSigned); err != nil {
		return nil, err
	}
	if len(parsed.Files) > 0 {
		if err := s.Store.DeleteAttachmentsForMessage(ctx, msg.UserID, msg.ID); err != nil {
			return nil, err
		}
	}
	attachmentDocs := make([]search.AttachmentDoc, 0, len(parsed.Files))
	visibleAttachmentCount := 0
	for _, file := range parsed.Files {
		if _, err := s.Store.CreateAttachment(ctx, store.Attachment{
			UserID:      msg.UserID,
			MessageID:   msg.ID,
			BlobID:      msg.BlobID,
			Filename:    file.Filename,
			ContentType: file.ContentType,
			ContentID:   file.ContentID,
			IsInline:    file.IsInline,
			Size:        int64(len(file.Data)),
			BlobPath:    "",
		}); err != nil {
			return nil, err
		}
		if !file.IsInline {
			visibleAttachmentCount++
			attachmentDocs = append(attachmentDocs, search.AttachmentDoc{
				Filename:    file.Filename,
				ContentType: file.ContentType,
				Text:        file.SearchableText(),
			})
		}
	}
	msg.HasAttachments = visibleAttachmentCount > 0
	msg.BodyText = parsed.Text
	msg.BodyHTML = ""
	if s.pluginEnabled(ctx, plugins.LanguageSearch) {
		msg.LanguageCode = detectLanguageCode(parsed.Subject, parsed.Text)
		if err := s.Store.UpdateMessageLanguage(ctx, msg.UserID, msg.ID, msg.LanguageCode); err != nil {
			return nil, err
		}
	} else {
		msg.LanguageCode = ""
	}
	return &pendingFetchedSearchIndex{
		Document: search.MessageIndexDocument{
			Message:     msg,
			Attachments: attachmentDocs,
		},
		HasVisibleAttachments: visibleAttachmentCount > 0,
	}, nil
}

// RepairMailboxSearchIndex indexes local mailbox messages that are missing from Bleve.
func (s *Service) RepairMailboxSearchIndex(ctx context.Context, userID int64, mailbox store.Mailbox, runID int64, progress *store.SyncProgress) (int, error) {
	if s.Search == nil {
		return 0, nil
	}
	if !mailbox.IncludeInSearch {
		_, err := s.Search.PurgeMailbox(ctx, userID, mailbox.ID)
		return 0, err
	}
	if _, err := s.Store.PreparePurgedMailboxSearchIndexRepair(ctx, userID, mailbox.ID); err != nil {
		return 0, err
	}
	indexedIDs, err := s.Search.MailboxMessageIDs(ctx, userID, mailbox.ID)
	if err != nil {
		return 0, err
	}
	missing, staleIDs, err := s.diffMailboxSearchDocuments(ctx, userID, mailbox.ID, indexedIDs)
	if err != nil {
		return 0, err
	}
	previousLatestFrom := ""
	previousLatestSubject := ""
	maintenanceWork := missing + len(staleIDs)
	if maintenanceWork > 0 {
		log.Printf("repair mailbox search index user_id=%d account_id=%d mailbox=%s missing=%d stale=%d",
			userID, mailbox.AccountID, mailbox.Name, missing, len(staleIDs))
	}
	if progress != nil && maintenanceWork > 0 {
		previousLatestFrom = progress.LatestNewFrom
		previousLatestSubject = progress.LatestNewSubject
		progress.MessagesTotal += maintenanceWork
		progress.CurrentMailbox = mailbox.Name
		progress.LatestNewFrom = "rolltop:maintenance"
		progress.LatestNewSubject = "Repairing full-text index"
		if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
			return 0, err
		}
	}
	if err := s.Search.DeleteMessagesWithProgress(ctx, userID, staleIDs, func(deleted int) error {
		if progress == nil || deleted <= 0 {
			return nil
		}
		progress.MessagesSeen += deleted
		return s.updateSyncProgress(ctx, userID, runID, *progress)
	}); err != nil {
		return 0, err
	}
	if missing == 0 {
		if progress != nil && maintenanceWork > 0 {
			progress.LatestNewFrom = previousLatestFrom
			progress.LatestNewSubject = previousLatestSubject
			if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
				return 0, err
			}
		}
		if err := s.Store.MarkMailboxSearchIndexActive(ctx, userID, mailbox.ID); err != nil {
			return 0, err
		}
		return 0, nil
	}

	indexed := 0
	deferred := 0
	var afterID int64
	// This is an explicit foreground rebuild under a dedicated mailbox
	// reservation. It is not the preemptible background attachment worker: use
	// materially larger Bleve batches and checkpoint SQLite/UI progress per page
	// rather than once per message.
	batch := newFetchedSearchIndexBatchWithSize(s, explicitSearchRepairBatchSize)
	pageIndexed := 0
	recordItem := func(msg store.MessageRecord, item *pendingFetchedSearchIndex) error {
		if err := batch.Add(ctx, item); err != nil {
			return err
		}
		indexed++
		indexedIDs[msg.ID] = true
		if progress != nil {
			progress.CurrentMailbox = mailbox.Name
			progress.CurrentUID = msg.UID
			progress.MessagesSeen++
			progress.MessagesStored++
			pageIndexed++
		}
		return nil
	}
	fallbackItem := func(msg store.MessageRecord) error {
		if indexedIDs[msg.ID] {
			return nil
		}
		s.deferAttachmentIndexRetry(userID, msg.ID, time.Now())
		if err := s.Store.MarkMessageAttachmentIndexPending(ctx, userID, msg.ID); err != nil {
			return err
		}
		item := &pendingFetchedSearchIndex{
			Document:    search.MessageIndexDocument{Message: msg},
			KeepPending: true,
		}
		if err := recordItem(msg, item); err != nil {
			return err
		}
		deferred++
		return nil
	}
	checkpointPage := func() error {
		if err := batch.Flush(ctx); err != nil {
			return err
		}
		if progress != nil && pageIndexed > 0 {
			if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
				return err
			}
			pageIndexed = 0
		}
		if indexed > 0 {
			log.Printf("repair mailbox search index checkpoint user_id=%d account_id=%d mailbox=%q indexed=%d bleve_batch_size=%d",
				userID, mailbox.AccountID, mailbox.Name, indexed, explicitSearchRepairBatchSize)
		}
		return nil
	}
	remoteHydrationUnavailable := false
	for {
		messages, err := s.Store.ListMessagesForMailboxIndex(ctx, userID, mailbox.ID, 500, afterID)
		if err != nil {
			return indexed, err
		}
		if len(messages) == 0 {
			if err := checkpointPage(); err != nil {
				return indexed, err
			}
			if deferred > 0 {
				log.Printf("repair mailbox search index complete user_id=%d account_id=%d mailbox=%q indexed=%d deferred=%d",
					userID, mailbox.AccountID, mailbox.Name, indexed, deferred)
			}
			if progress != nil {
				progress.LatestNewFrom = previousLatestFrom
				progress.LatestNewSubject = previousLatestSubject
				if err := s.updateSyncProgress(ctx, userID, runID, *progress); err != nil {
					return indexed, err
				}
			}
			if err := s.Store.MarkMailboxSearchIndexActive(ctx, userID, mailbox.ID); err != nil {
				return indexed, err
			}
			return indexed, nil
		}
		remoteMessages := make([]store.MessageRecord, 0, len(messages))
		for _, msg := range messages {
			afterID = msg.ID
			if indexedIDs[msg.ID] {
				continue
			}
			raw, local, err := s.readLocalRawMessageForIndexRepair(userID, msg)
			if err != nil {
				return indexed, err
			}
			if !local {
				remoteMessages = append(remoteMessages, msg)
				continue
			}
			item, err := s.prepareAttachmentIndexMessageFromRaw(ctx, msg, raw)
			if err != nil {
				return indexed, err
			}
			if err := recordItem(msg, item); err != nil {
				return indexed, err
			}
			s.clearAttachmentIndexRetry(userID, msg.ID)
		}
		if len(remoteMessages) == 0 {
			if err := checkpointPage(); err != nil {
				return indexed, err
			}
			continue
		}
		if remoteHydrationUnavailable {
			for _, msg := range remoteMessages {
				if err := fallbackItem(msg); err != nil {
					return indexed, err
				}
			}
			continue
		}
		breaker, err := s.repairMailboxSearchRemotePage(ctx, userID, mailbox, remoteMessages, recordItem, fallbackItem)
		if err != nil {
			return indexed, err
		}
		if breaker {
			remoteHydrationUnavailable = true
		}
		// Finish the partial batch at the page boundary before publishing the
		// matching progress snapshot. SQLite/UI updates happen once per 500-row
		// page while Bleve uses its responsive 25-document commit cadence.
		if err := checkpointPage(); err != nil {
			return indexed, err
		}
	}
}

func (s *Service) diffMailboxSearchDocuments(ctx context.Context, userID, mailboxID int64, indexedIDs map[int64]bool) (int, []int64, error) {
	missing := 0
	staleIDs := make(map[int64]struct{}, len(indexedIDs))
	for id := range indexedIDs {
		staleIDs[id] = struct{}{}
	}
	var afterID int64
	for {
		messages, err := s.Store.ListMessagesForMailboxIndex(ctx, userID, mailboxID, 500, afterID)
		if err != nil {
			return missing, nil, err
		}
		if len(messages) == 0 {
			stale := make([]int64, 0, len(staleIDs))
			for id := range staleIDs {
				stale = append(stale, id)
			}
			return missing, stale, nil
		}
		for _, msg := range messages {
			afterID = msg.ID
			delete(staleIDs, msg.ID)
			if !indexedIDs[msg.ID] {
				missing++
			}
		}
	}
}

// ReconcileMailboxSearchIndex adds or removes documents when a mailbox search-visibility setting changes.
func (s *Service) ReconcileMailboxSearchIndex(ctx context.Context, userID, mailboxID int64, include bool) error {
	if s.Search == nil {
		return nil
	}
	var batch *fetchedSearchIndexBatch
	if include {
		batch = newFetchedSearchIndexBatch(s)
	}
	var afterID int64
	for {
		messages, err := s.Store.ListMessagesForMailboxIndex(ctx, userID, mailboxID, 200, afterID)
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			if batch != nil {
				if err := batch.Flush(ctx); err != nil {
					return err
				}
				return s.Store.MarkMailboxSearchIndexActive(ctx, userID, mailboxID)
			}
			return nil
		}
		for _, msg := range messages {
			afterID = msg.ID
			if !include {
				if err := s.Search.DeleteMessage(ctx, msg.UserID, msg.ID); err != nil {
					return err
				}
				continue
			}
			item, err := s.prepareAttachmentIndexMessage(ctx, msg)
			if err != nil {
				return err
			}
			if err := batch.Add(ctx, item); err != nil {
				return err
			}
		}
	}
}

// oversizedMessageBodyText is the placeholder preview stored for messages whose
// remote size exceeded the mirror limit. Only their headers are kept locally.
func oversizedMessageBodyText(size int64) string {
	return fmt.Sprintf("[Message too large to mirror locally (%s). It is listed from headers only; read it in your mail provider's webmail.]", humanBytes(size))
}

func humanBytes(size int64) string {
	switch {
	case size <= 0:
		return "unknown size"
	case size < 1024:
		return fmt.Sprintf("%d B", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(size)/1024)
	case size < 1024*1024*1024:
		return fmt.Sprintf("%.1f MiB", float64(size)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GiB", float64(size)/(1024*1024*1024))
	}
}
