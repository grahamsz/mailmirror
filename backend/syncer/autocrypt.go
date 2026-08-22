// File overview: Backend plugin hook dispatch during message sync.

package syncer

import (
	"context"
	"errors"
	"log"

	"rolltop/backend/plugins"
	"rolltop/backend/search"
	"rolltop/backend/store"
)

func (s *Service) importIncomingMessageHooks(ctx context.Context, userID int64, raw []byte, parsedFrom string) error {
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		return err
	}
	host := syncPluginHost{s: s}
	for _, backendPlugin := range backendPlugins {
		hook, ok := backendPlugin.(plugins.IncomingMessageHook)
		if !ok {
			continue
		}
		generationRecoveryPhase(ctx, "plugin-incoming-message", backendPlugin.ID())
		if _, err := plugins.CallHook(messageImportHookTimeout, func() (struct{}, error) {
			return struct{}{}, hook.ImportIncomingMessage(ctx, host, userID, raw, parsedFrom)
		}); err != nil {
			if plugins.IsHookGuardFailure(err) {
				log.Printf("incoming message hook skipped plugin_id=%s user_id=%d error_type=%T", backendPlugin.ID(), userID, err)
				continue
			}
			return err
		}
	}
	return nil
}

func (s *Service) importStoredMessageHooks(ctx context.Context, hooks []plugins.StoredMessageHook, msg store.MessageRecord, mailbox store.Mailbox) error {
	if len(hooks) == 0 {
		return nil
	}
	host := syncPluginHost{s: s}
	info := plugins.StoredMessageContext{
		UserID:      msg.UserID,
		MessageID:   msg.ID,
		AccountID:   msg.AccountID,
		MailboxID:   msg.MailboxID,
		MailboxName: mailbox.Name,
		UID:         msg.UID,
		Date:        msg.Date,
		From:        msg.FromAddr,
		To:          msg.ToAddr,
		CC:          msg.CCAddr,
		Subject:     msg.Subject,
		IsRead:      msg.IsRead,
		IsStarred:   msg.IsStarred,
	}
	for _, hook := range hooks {
		generationRecoveryPhase(ctx, "plugin-stored-message", hook.ID())
		if _, err := plugins.CallHook(messageImportHookTimeout, func() (struct{}, error) {
			return struct{}{}, hook.ImportStoredMessage(ctx, host, info)
		}); err != nil {
			if plugins.IsHookGuardFailure(err) {
				log.Printf("stored message hook skipped plugin_id=%s user_id=%d message_id=%d error_type=%T", hook.ID(), info.UserID, info.MessageID, err)
				continue
			}
			if !errors.Is(err, plugins.ErrUnsupported) {
				return err
			}
		}
	}
	return nil
}

func (s *Service) storedMessageHooks(ctx context.Context) ([]plugins.StoredMessageHook, error) {
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		return nil, err
	}
	hooks := make([]plugins.StoredMessageHook, 0, len(backendPlugins))
	for _, backendPlugin := range backendPlugins {
		hook, ok := backendPlugin.(plugins.StoredMessageHook)
		if ok {
			hooks = append(hooks, hook)
		}
	}
	return hooks, nil
}

type syncPluginHost struct {
	s *Service
}

func (h syncPluginHost) Store() any {
	return h.s.Store
}

func (h syncPluginHost) MasterKey() []byte {
	return h.s.MasterKey
}

func (h syncPluginHost) PluginEnabled(ctx context.Context, pluginID string) bool {
	return h.s.pluginEnabled(ctx, pluginID)
}

func (h syncPluginHost) MatchMessageSearch(ctx context.Context, userID, messageID int64, query string) (plugins.SearchMatchResult, error) {
	if h.s == nil || h.s.Search == nil {
		return plugins.SearchMatchResult{}, errors.New("search is not configured")
	}
	hit, ok, err := h.s.Search.MatchMessageWithOptions(ctx, userID, messageID, query, search.SearchOptions{})
	if err != nil {
		return plugins.SearchMatchResult{}, err
	}
	return plugins.SearchMatchResult{
		Matched:    ok,
		Score:      hit.Score,
		Terms:      hit.Terms,
		QueryTerms: hit.QueryTerms,
		Fields:     hit.Fields,
	}, nil
}

func (h syncPluginHost) SimilarMessages(ctx context.Context, userID int64, request plugins.SimilarMessagesRequest) ([]plugins.SimilarMessageResult, error) {
	if h.s == nil || h.s.Store == nil || h.s.Search == nil {
		return nil, errors.New("message similarity is not configured")
	}
	return h.s.Search.SimilarMessages(ctx, h.s.Store, userID, request)
}

func (h syncPluginHost) StarMessage(ctx context.Context, userID, messageID int64, starred bool) error {
	if h.s == nil {
		return errors.New("sync service is not configured")
	}
	msg, err := h.s.SetStarredForMessage(ctx, userID, messageID, starred)
	if err != nil {
		return err
	}
	return h.s.SyncStarStateForMessage(ctx, userID, msg.ID)
}

func (h syncPluginHost) MoveMessage(ctx context.Context, userID, messageID, destMailboxID int64) error {
	if h.s == nil {
		return errors.New("sync service is not configured")
	}
	return h.s.MoveMessage(ctx, userID, messageID, destMailboxID)
}

func (h syncPluginHost) ForwardMessage(ctx context.Context, userID, messageID int64, to string, headers []plugins.MailHeader) error {
	if h.s == nil {
		return errors.New("sync service is not configured")
	}
	return h.s.ForwardMessage(ctx, userID, messageID, to, headers)
}

var _ plugins.BackendHost = syncPluginHost{}
var _ plugins.StoredMessageHost = syncPluginHost{}
var _ plugins.MessageSimilarityHost = syncPluginHost{}
var _ plugins.MessageClassificationHost = syncPluginHost{}
