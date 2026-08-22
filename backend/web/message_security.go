// File overview: Generic backend plugin hooks for displayed message security
// state and body transforms.

package web

import (
	"context"
	"errors"
	"log"
	"time"

	"rolltop/backend/plugins"
)

// Display-path hook budgets mirror the syncer guards: a hung in-process
// plugin degrades one render instead of hanging the HTTP request forever.
const (
	displaySecurityDetectTimeout    = 5 * time.Second
	displaySecurityTransformTimeout = 10 * time.Second
)

func (s *Server) hasMessageSecurityProvider(ctx context.Context) (bool, error) {
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		return false, err
	}
	for _, backendPlugin := range backendPlugins {
		if _, ok := backendPlugin.(plugins.MessageSecurityProvider); ok {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) detectMessageSecurity(ctx context.Context, userID int64, raw []byte, body plugins.MessageBody) (plugins.MessageSecurityState, bool, error) {
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		return plugins.MessageSecurityState{}, false, err
	}
	var out plugins.MessageSecurityState
	handled := false
	for _, backendPlugin := range backendPlugins {
		provider, ok := backendPlugin.(plugins.MessageSecurityProvider)
		if !ok {
			continue
		}
		state, stateErr := plugins.CallHook(displaySecurityDetectTimeout, func() (plugins.MessageSecurityState, error) {
			return provider.DetectMessageSecurity(ctx, s, userID, raw, body)
		})
		if errors.Is(stateErr, plugins.ErrUnsupported) {
			continue
		}
		if plugins.IsHookGuardFailure(stateErr) {
			log.Printf("display security detect skipped plugin_id=%s user_id=%d error_type=%T", backendPlugin.ID(), userID, stateErr)
			continue
		}
		if stateErr != nil {
			return out, handled, stateErr
		}
		handled = true
		out.Encrypted = out.Encrypted || state.Encrypted
		out.Signed = out.Signed || state.Signed
	}
	return out, handled, nil
}

func (s *Server) transformMessageSecurityBody(ctx context.Context, userID int64, raw []byte, state plugins.MessageSecurityState, body plugins.MessageBody) (plugins.MessageBodyTransform, error) {
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		return plugins.MessageBodyTransform{}, err
	}
	for _, backendPlugin := range backendPlugins {
		provider, ok := backendPlugin.(plugins.MessageSecurityProvider)
		if !ok {
			continue
		}
		transform, transformErr := plugins.CallHook(displaySecurityTransformTimeout, func() (plugins.MessageBodyTransform, error) {
			return provider.TransformMessageBody(ctx, s, userID, raw, state, body)
		})
		if errors.Is(transformErr, plugins.ErrUnsupported) {
			continue
		}
		if plugins.IsHookGuardFailure(transformErr) {
			log.Printf("display security transform skipped plugin_id=%s user_id=%d error_type=%T", backendPlugin.ID(), userID, transformErr)
			continue
		}
		if transformErr != nil {
			return plugins.MessageBodyTransform{}, transformErr
		}
		if transform.Applied {
			return transform, nil
		}
	}
	return plugins.MessageBodyTransform{}, nil
}
