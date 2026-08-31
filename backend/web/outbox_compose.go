// File overview: Compose validation, immutable MIME spooling, and optimistic Sent insertion.

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/backend/plugins"
	"rolltop/backend/search"
	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
)

func (s *Server) queueCompose(ctx context.Context, cu currentUser, form composeForm) (store.OutboxJob, store.MessageRecord, error) {
	if s.sender == nil {
		return store.OutboxJob{}, store.MessageRecord{}, errors.New("SMTP sending is not configured")
	}
	form.SubmissionKey = strings.TrimSpace(form.SubmissionKey)
	if form.SubmissionKey == "" {
		form.SubmissionKey = smtpclient.NewSubmissionID()
	}
	if len(form.SubmissionKey) > 200 {
		return store.OutboxJob{}, store.MessageRecord{}, errors.New("compose submission identifier is invalid")
	}
	if existing, err := s.store.GetOutboxJobBySubmission(ctx, cu.User.ID, form.SubmissionKey); err == nil {
		msg, msgErr := s.store.GetMessageForUser(ctx, cu.User.ID, existing.OptimisticMessageID)
		return existing, msg, msgErr
	} else if !store.IsNotFound(err) {
		return store.OutboxJob{}, store.MessageRecord{}, err
	}

	identity, err := s.selectedComposeIdentity(ctx, cu, form.FromIdentityID)
	if err != nil {
		return store.OutboxJob{}, store.MessageRecord{}, err
	}
	smtpAccount, err := s.smtpAccountForIdentity(ctx, cu.User.ID, identity)
	if err != nil {
		return store.OutboxJob{}, store.MessageRecord{}, err
	}
	imapAccount, sentMailbox, err := s.sentMailboxForIdentity(ctx, cu.User.ID, identity, smtpAccount)
	if err != nil {
		return store.OutboxJob{}, store.MessageRecord{}, err
	}
	attachments, err := s.composeMessageAttachments(ctx, cu.User.ID, form)
	if err != nil {
		return store.OutboxJob{}, store.MessageRecord{}, err
	}
	if form.SecurityEncrypted || form.SecuritySigned {
		if len(attachments) > 0 || form.AttachPublicKey {
			return store.OutboxJob{}, store.MessageRecord{}, errors.New("message security does not support attachments yet")
		}
	} else if form.AttachPublicKey {
		attachment, err := s.composePublicKeyAttachment(ctx, cu.User.ID, identity)
		if err != nil {
			return store.OutboxJob{}, store.MessageRecord{}, err
		}
		attachments = append(attachments, attachment)
	}
	bodyHTML, bodyText := form.BodyHTML, form.Body
	if !form.SecurityEncrypted && !form.SecuritySigned {
		bodyHTML, bodyText = appendIdentitySignature(form.BodyHTML, form.Body, identity.Signature)
	}
	msg := smtpclient.Message{
		From:        identity.Header,
		To:          []string{form.To},
		Cc:          []string{form.Cc},
		Bcc:         []string{form.Bcc},
		Subject:     form.Subject,
		BodyText:    bodyText,
		BodyHTML:    bodyHTML,
		MessageID:   smtpclient.NewMessageID(identity.Email),
		Date:        time.Now().UTC(),
		Attachments: attachments,
	}
	if err := s.applyPluginMIMEBodyOverride(ctx, cu.User.ID, &msg, identity, form); err != nil {
		return store.OutboxJob{}, store.MessageRecord{}, err
	}
	s.applyPluginMailHeaders(ctx, cu.User.ID, &msg, identity)
	if form.InReplyToID > 0 {
		reply, err := s.store.GetMessageForUser(ctx, cu.User.ID, form.InReplyToID)
		if err != nil && !store.IsNotFound(err) {
			return store.OutboxJob{}, store.MessageRecord{}, err
		}
		if err == nil {
			msg.InReplyTo = reply.MessageIDHeader
			msg.References = referencesForReply(reply)
		}
	}
	raw, recipients, err := smtpclient.BuildRaw(msg)
	if err != nil {
		return store.OutboxJob{}, store.MessageRecord{}, err
	}
	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return store.OutboxJob{}, store.MessageRecord{}, err
	}
	saved, err := s.blobs.SaveOutboxMessage(cu.User.ID, form.SubmissionKey, raw)
	if err != nil {
		return store.OutboxJob{}, store.MessageRecord{}, fmt.Errorf("save queued message: %w", err)
	}
	blobRec, err := s.store.CreateBlob(ctx, store.BlobRecord{
		UserID: cu.User.ID,
		Kind:   "outbox-message",
		Path:   saved.Path,
		SHA256: saved.SHA256,
		Size:   saved.Size,
	})
	if err != nil {
		_ = s.blobs.DeleteUserBlob(cu.User.ID, saved.Path)
		return store.OutboxJob{}, store.MessageRecord{}, err
	}
	parsed, parseErr := mailparse.Parse(raw)
	if parseErr != nil {
		parsed.Subject = form.Subject
		parsed.Text = bodyText
		parsed.HTML = bodyHTML
		parsed.MessageID = msg.MessageID
		parsed.InReplyTo = msg.InReplyTo
		parsed.References = msg.References
		parsed.From = msg.From
		parsed.To = form.To
		parsed.CC = form.Cc
	}
	fingerprint := store.MessageArrivalFingerprint(raw, msg.MessageID, msg.Date, int64(len(raw)))
	languageCode := ""
	if !form.SecurityEncrypted && s.pluginEnabled(ctx, plugins.LanguageSearch) {
		languageCode = detectLanguageCode(form.Subject, bodyText)
	}
	storedAttachments := make([]store.Attachment, 0, len(parsed.Files))
	attachmentDocs := make([]search.AttachmentDoc, 0, len(parsed.Files))
	visibleAttachments := 0
	for _, file := range parsed.Files {
		storedAttachments = append(storedAttachments, store.Attachment{
			UserID:      cu.User.ID,
			BlobID:      blobRec.ID,
			Filename:    file.Filename,
			ContentType: file.ContentType,
			ContentID:   file.ContentID,
			IsInline:    file.IsInline,
			Size:        int64(len(file.Data)),
		})
		if !file.IsInline {
			visibleAttachments++
			attachmentDocs = append(attachmentDocs, search.AttachmentDoc{
				Filename:    file.Filename,
				ContentType: file.ContentType,
				Text:        file.SearchableText(),
			})
		}
	}
	job, optimistic, created, err := s.store.EnqueueOutboxMessage(ctx, store.OutboxEnqueue{
		UserID:          cu.User.ID,
		SubmissionKey:   form.SubmissionKey,
		SMTPAccountID:   smtpAccount.ID,
		IMAPAccountID:   imapAccount.ID,
		SentMailboxID:   sentMailbox.ID,
		EnvelopeFrom:    identity.Email,
		RecipientsJSON:  string(recipientsJSON),
		MessageIDHeader: msg.MessageID,
		Blob:            blobRec,
		RawSHA256:       saved.SHA256,
		RawSize:         saved.Size,
		Message: store.CreateMessage{
			MessageIDHeader:  msg.MessageID,
			CanonicalSHA256:  fingerprint.CanonicalSHA256,
			MessageIDHash:    fingerprint.MessageIDHash,
			InReplyTo:        msg.InReplyTo,
			ReferencesHeader: msg.References,
			Subject:          form.Subject,
			LanguageCode:     languageCode,
			FromAddr:         msg.From,
			ToAddr:           form.To,
			CCAddr:           form.Cc,
			Date:             msg.Date,
			InternalDate:     msg.Date,
			Size:             int64(len(raw)),
			BodyText:         store.MessageBodyPreview(bodyText, store.DefaultMessageBodyPreviewBytes),
			BodyHTML:         "",
			IsRead:           true,
			HasAttachments:   visibleAttachments > 0,
			IsEncrypted:      form.SecurityEncrypted,
			IsSigned:         form.SecuritySigned,
		},
		Attachments: storedAttachments,
	})
	if err != nil {
		deleted, _ := s.store.DeleteBlobIfUnreferencedForUser(ctx, cu.User.ID, blobRec.ID)
		if deleted {
			_ = s.blobs.DeleteUserBlob(cu.User.ID, saved.Path)
		}
		return store.OutboxJob{}, store.MessageRecord{}, err
	}
	if !created && blobRec.ID != job.BlobID {
		deleted, _ := s.store.DeleteBlobIfUnreferencedForUser(ctx, cu.User.ID, blobRec.ID)
		if deleted {
			_ = s.blobs.DeleteUserBlob(cu.User.ID, saved.Path)
		}
	}
	if created && s.search != nil && sentMailbox.IncludeInSearch {
		indexMessage := optimistic
		indexMessage.BodyText = parsed.Text
		if err := s.search.IndexMessage(ctx, indexMessage, attachmentDocs); err != nil {
			log.Printf("index optimistic Sent message user_id=%d message_id=%d: %v",
				cu.User.ID, optimistic.ID, err)
		} else if err := s.store.MarkMessageAttachmentIndexed(ctx, cu.User.ID,
			optimistic.ID, visibleAttachments > 0); err != nil {
			log.Printf("mark optimistic Sent search commit user_id=%d message_id=%d: %v",
				cu.User.ID, optimistic.ID, err)
		}
	}
	return job, optimistic, nil
}
