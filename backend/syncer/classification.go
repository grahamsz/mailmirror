// File overview: Best-effort post-index message classification plugin dispatch.

package syncer

import (
	"context"
	"log"
	"strings"
	"unicode/utf8"

	"rolltop/backend/mailparse"
	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

const maxQueuedClassificationBodyBytes = 64 * 1024

type messageClassificationBatch struct {
	service *Service
	hooks   []plugins.MessageClassifier
	items   []plugins.MessageClassificationInput
}

func newMessageClassificationBatch(service *Service, hooks []plugins.MessageClassifier) *messageClassificationBatch {
	return &messageClassificationBatch{service: service, hooks: hooks}
}

func (b *messageClassificationBatch) Add(msg store.MessageRecord, parsed mailparse.ParsedMessage) {
	if b == nil || len(b.hooks) == 0 {
		return
	}
	b.items = append(b.items, messageClassificationInput(msg, parsed))
}

func (b *messageClassificationBatch) Flush(ctx context.Context) {
	if b == nil || len(b.items) == 0 {
		return
	}
	for _, input := range b.items {
		if ctx.Err() != nil {
			break
		}
		b.service.classifyMessageInput(ctx, b.hooks, input)
	}
	b.items = b.items[:0]
}

func (s *Service) postStorePluginHooks(ctx context.Context) ([]plugins.MessageClassifier, []plugins.StoredMessageHook, error) {
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		return nil, nil, err
	}
	classifiers := make([]plugins.MessageClassifier, 0, len(backendPlugins))
	stored := make([]plugins.StoredMessageHook, 0, len(backendPlugins))
	for _, backendPlugin := range backendPlugins {
		if hook, ok := backendPlugin.(plugins.MessageClassifier); ok {
			classifiers = append(classifiers, hook)
		}
		if hook, ok := backendPlugin.(plugins.StoredMessageHook); ok {
			stored = append(stored, hook)
		}
	}
	return classifiers, stored, nil
}

// classifyStoredMessage deliberately does not return plugin failures. The mail
// row and its search document are already committed; an experimental classifier
// must not turn an advisory failure into a failed incremental sync.
func (s *Service) classifyStoredMessage(ctx context.Context, hooks []plugins.MessageClassifier, msg store.MessageRecord, parsed mailparse.ParsedMessage) {
	if len(hooks) == 0 || ctx.Err() != nil {
		return
	}
	s.classifyMessageInput(ctx, hooks, messageClassificationInput(msg, parsed))
}

func messageClassificationInput(msg store.MessageRecord, parsed mailparse.ParsedMessage) plugins.MessageClassificationInput {
	attachments := make([]plugins.MessageClassificationAttachment, 0, len(parsed.Files))
	for _, attachment := range parsed.Files {
		attachments = append(attachments, plugins.MessageClassificationAttachment{
			Filename:    attachment.Filename,
			ContentType: attachment.ContentType,
			Size:        int64(len(attachment.Data)),
		})
	}
	bodyText := parsed.Text
	bodyTruncated := len(bodyText) > maxQueuedClassificationBodyBytes
	if msg.IsEncrypted {
		bodyText = ""
		bodyTruncated = false
	} else if bodyTruncated {
		bodyText = truncateClassificationBody(bodyText, maxQueuedClassificationBodyBytes)
	}
	return plugins.MessageClassificationInput{
		UserID:          msg.UserID,
		MessageID:       msg.ID,
		MessageIDHeader: msg.MessageIDHeader,
		AccountID:       msg.AccountID,
		MailboxID:       msg.MailboxID,
		Date:            msg.Date,
		From:            msg.FromAddr,
		To:              msg.ToAddr,
		CC:              msg.CCAddr,
		Subject:         msg.Subject,
		BodyText:        bodyText,
		BodyTruncated:   bodyTruncated,
		HasHTML:         strings.TrimSpace(parsed.HTML) != "",
		IsEncrypted:     msg.IsEncrypted,
		IsSigned:        msg.IsSigned,
		Attachments:     attachments,
	}
}

// truncateClassificationBody mirrors the model feature extractor's UTF-8-safe
// prefix semantics. Keeping the bounded sync payload byte-for-byte equivalent
// to training avoids changing features only for long messages.
func truncateClassificationBody(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (s *Service) classifyMessageInput(ctx context.Context, hooks []plugins.MessageClassifier, input plugins.MessageClassificationInput) {
	if len(hooks) == 0 || ctx.Err() != nil {
		return
	}
	host := syncPluginHost{s: s}
	for _, hook := range hooks {
		generationRecoveryPhase(ctx, "plugin-classification", hook.ID())
		if _, err := plugins.CallHook(messageClassifyHookTimeout, func() (struct{}, error) {
			return struct{}{}, hook.ClassifyMessage(ctx, host, input)
		}); err != nil {
			// Do not include the error string for plugin failures: plugin errors
			// can contain derived message evidence, which must not be written
			// to application logs. Guard failures carry only fixed wording.
			if plugins.IsHookGuardFailure(err) {
				log.Printf("message classifier skipped plugin_id=%s user_id=%d message_id=%d error_type=%T", hook.ID(), input.UserID, input.MessageID, err)
			} else {
				log.Printf("message classifier failed plugin_id=%s user_id=%d message_id=%d error_type=%T", hook.ID(), input.UserID, input.MessageID, err)
			}
		}
	}
}
