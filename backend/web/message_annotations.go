// File overview: Generic, tenant-scoped message annotations supplied by runtime backend plugins.

package web

import (
	"context"
	"strings"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

const (
	maxPluginAnnotationsPerMessage = 8
	maxPluginAnnotationMetadata    = 16
	maxPluginAnnotationTextBytes   = 512
)

func (s *Server) pluginMessageAnnotations(ctx context.Context, userID int64, messageIDs []int64, backendPlugins []plugins.BackendPlugin) map[int64][]apiMessageAnnotation {
	out := map[int64][]apiMessageAnnotation{}
	if s == nil || userID <= 0 || len(messageIDs) == 0 {
		return out
	}
	requested := make(map[int64]bool, len(messageIDs))
	owned := make([]int64, 0, len(messageIDs))
	for _, id := range messageIDs {
		if id <= 0 || requested[id] {
			continue
		}
		requested[id] = true
		owned = append(owned, id)
	}
	if len(owned) == 0 {
		return out
	}
	if backendPlugins == nil {
		backendPlugins, _ = s.enabledBackendPlugins(ctx)
	}
	for _, backendPlugin := range backendPlugins {
		provider, ok := backendPlugin.(plugins.MessageAnnotationProvider)
		if !ok {
			continue
		}
		provided, err := provider.MessageAnnotations(ctx, s, userID, owned)
		if err != nil {
			continue
		}
		for messageID, annotations := range provided {
			if !requested[messageID] {
				continue
			}
			for _, annotation := range annotations {
				if len(out[messageID]) >= maxPluginAnnotationsPerMessage {
					break
				}
				kind := cleanPluginAnnotationText(annotation.Kind)
				label := cleanPluginAnnotationText(annotation.Label)
				if kind == "" || label == "" {
					continue
				}
				out[messageID] = append(out[messageID], apiMessageAnnotation{
					PluginID: backendPlugin.ID(),
					Kind:     kind,
					Label:    label,
					Level:    cleanPluginAnnotationText(annotation.Level),
					Summary:  cleanPluginAnnotationText(annotation.Summary),
					Metadata: cleanPluginAnnotationMetadata(annotation.Metadata),
				})
			}
		}
	}
	return out
}

func (s *Server) apiConversationsWithAnnotations(ctx context.Context, userID int64, conversations []conversationView) []apiConversation {
	out := apiConversations(conversations)
	ids := make([]int64, 0, len(conversations))
	for _, conversation := range conversations {
		ids = append(ids, conversation.Message.ID)
	}
	annotations := s.pluginMessageAnnotations(ctx, userID, ids, nil)
	outboxStates, _ := s.store.OutboxMessageStatesForUser(ctx, userID, ids)
	for i := range out {
		out[i].Message.Annotations = annotations[out[i].Message.ID]
		applyOutboxMessageState(&out[i].Message, outboxStates[out[i].Message.ID])
	}
	return out
}

func applyOutboxMessageState(message *apiMessage, state store.OutboxMessageState) {
	if message == nil || state.OutboxID <= 0 {
		return
	}
	message.OutboxID = state.OutboxID
	message.DeliveryState = state.DeliveryState
	message.FilingState = state.FilingState
	message.DeliveryError = state.LastError
	message.NeedsAttention = state.Attention
}

func cleanPluginAnnotationText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxPluginAnnotationTextBytes {
		value = value[:maxPluginAnnotationTextBytes]
	}
	return value
}

func cleanPluginAnnotationMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]string, min(len(metadata), maxPluginAnnotationMetadata))
	for key, value := range metadata {
		if len(out) >= maxPluginAnnotationMetadata {
			break
		}
		key = cleanPluginAnnotationText(key)
		if key == "" {
			continue
		}
		out[key] = cleanPluginAnnotationText(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
