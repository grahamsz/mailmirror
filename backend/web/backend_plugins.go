// File overview: Host-side runtime backend plugin loading and API dispatch.

package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"

	"rolltop/backend/logging"
	"rolltop/backend/plugins"
	"rolltop/backend/search"
)

var errBackendPluginHostClosed = errors.New("backend plugin host is closed")

func (s *Server) Store() any {
	if s == nil {
		return nil
	}
	return s.store
}

func (s *Server) MasterKey() []byte {
	if s == nil {
		return nil
	}
	return s.masterKey
}

func (s *Server) PluginEnabled(ctx context.Context, pluginID string) bool {
	return s.pluginEnabled(ctx, pluginID)
}

// NotifyUserChanged invalidates per-user list state and wakes connected event
// streams after a plugin changes user-visible local metadata.
func (s *Server) NotifyUserChanged(userID int64) {
	if s != nil {
		s.notifyUserChanged(userID)
	}
}

var _ plugins.UserChangeHost = (*Server)(nil)

// QueueAccountMailboxSync lets an enabled backend plugin refetch a message it
// has just appended to one of the signed-in user's configured IMAP mailboxes.
// Resolve both records through tenant-scoped store methods before handing the
// work to the process-lifetime runner.
func (s *Server) QueueAccountMailboxSync(ctx context.Context, userID, accountID int64, mailboxName string) error {
	mailboxName = strings.TrimSpace(mailboxName)
	if s == nil || s.store == nil || s.syncRunner == nil {
		return errors.New("mailbox sync is not configured")
	}
	if userID <= 0 || accountID <= 0 || mailboxName == "" {
		return errors.New("user, destination account, and mailbox are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := s.store.GetMailAccountForUser(ctx, userID, accountID); err != nil {
		return fmt.Errorf("resolve destination IMAP account: %w", err)
	}
	mailbox, err := s.store.GetMailbox(ctx, userID, accountID, mailboxName)
	if err != nil {
		return fmt.Errorf("resolve destination IMAP mailbox: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.syncRunner.QueueAccountMailboxes(userID, accountID, []string{mailbox.Name}) {
		return errors.New("mailbox sync is stopping")
	}
	return nil
}

// MailboxGenerationRecoveryActive exposes only the tenant-scoped in-memory
// core sync gate. Plugins combine it with durable generation markers so a
// restart and the post-rebuild replay interval are both covered.
func (s *Server) MailboxGenerationRecoveryActive(userID int64) bool {
	return s != nil && s.syncRunner != nil && s.syncRunner.MailboxGenerationRecoveryActive(userID)
}

// BeginForegroundOperation coordinates a plugin-owned mail write with core
// sync recovery and derived indexing for the same tenant.
func (s *Server) BeginForegroundOperation(ctx context.Context, userID int64) (func(), error) {
	if s == nil || s.syncRunner == nil {
		return nil, errors.New("foreground mail scheduling is not configured")
	}
	return s.syncRunner.BeginForegroundOperation(ctx, userID)
}

func (s *Server) RequireAPIAuth(w http.ResponseWriter, r *http.Request) (plugins.CurrentUser, bool) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return plugins.CurrentUser{}, false
	}
	return plugins.CurrentUser{UserID: cu.User.ID}, true
}

var _ plugins.AccountMailboxSyncHost = (*Server)(nil)
var _ plugins.MailboxGenerationRecoveryHost = (*Server)(nil)
var _ plugins.ForegroundOperationHost = (*Server)(nil)

func (s *Server) LoginUserID(w http.ResponseWriter, r *http.Request, userID int64) error {
	return s.loginUser(w, r, userID)
}

func (s *Server) VerifyCSRF(w http.ResponseWriter, r *http.Request) bool {
	return s.verifyCSRF(w, r)
}

func (s *Server) DecodeJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	return decodeJSON(w, r, dest)
}

func (s *Server) WriteJSON(w http.ResponseWriter, value any) {
	writeJSON(w, value)
}

func (s *Server) WriteAPIError(w http.ResponseWriter, status int, message string) {
	writeAPIError(w, status, message)
}

func (s *Server) ServerError(w http.ResponseWriter, err error) {
	s.serverError(w, err)
}

func (s *Server) MatchMessageSearch(ctx context.Context, userID, messageID int64, query string) (plugins.SearchMatchResult, error) {
	if s == nil || s.search == nil {
		return plugins.SearchMatchResult{}, errors.New("search is not configured")
	}
	hit, ok, err := s.search.MatchMessageWithOptions(ctx, userID, messageID, query, search.SearchOptions{})
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

// SimilarMessages exposes the optional, read-only plugin similarity capability.
// The search service resolves all candidate ownership and read-state predicates
// through this server's tenant-aware store before querying Bleve.
func (s *Server) SimilarMessages(ctx context.Context, userID int64, request plugins.SimilarMessagesRequest) ([]plugins.SimilarMessageResult, error) {
	if s == nil || s.store == nil || s.search == nil {
		return nil, errors.New("message similarity is not configured")
	}
	return s.search.SimilarMessages(ctx, s.store, userID, request)
}

var _ plugins.MessageSimilarityHost = (*Server)(nil)

// FetchRawMessage exposes the syncer's non-caching raw-message path to backend
// plugins. Resolve the message here as well as in the syncer so a plugin cannot
// use a message ID to cross the caller's tenant boundary.
func (s *Server) FetchRawMessage(ctx context.Context, userID, messageID int64) ([]byte, error) {
	if s == nil || s.store == nil || s.syncer == nil {
		return nil, errors.New("raw message fetch is not configured")
	}
	if userID <= 0 || messageID <= 0 {
		return nil, errors.New("user and message are required")
	}
	message, err := s.store.GetMessageForUser(ctx, userID, messageID)
	if err != nil {
		return nil, err
	}
	return s.syncer.FetchRawMessageForMessage(ctx, userID, message)
}

var _ plugins.RawMessageFetchHost = (*Server)(nil)

func (s *Server) StarMessage(ctx context.Context, userID, messageID int64, starred bool) error {
	if s == nil || s.syncer == nil {
		return errors.New("sync service is not configured")
	}
	msg, err := s.syncer.SetStarredForMessage(ctx, userID, messageID, starred)
	if err != nil {
		return err
	}
	if err := s.syncer.SyncStarStateForMessage(ctx, userID, msg.ID); err != nil {
		return err
	}
	s.notifyUserChanged(userID)
	return nil
}

func (s *Server) MoveMessage(ctx context.Context, userID, messageID, destMailboxID int64) error {
	if s == nil || s.syncer == nil {
		return errors.New("sync service is not configured")
	}
	if err := s.syncer.MoveMessage(ctx, userID, messageID, destMailboxID); err != nil {
		return err
	}
	s.notifyUserChanged(userID)
	return nil
}

func (s *Server) ForwardMessage(ctx context.Context, userID, messageID int64, to string, headers []plugins.MailHeader) error {
	if s == nil || s.syncer == nil {
		return errors.New("sync service is not configured")
	}
	if s.syncer.Sender == nil {
		s.syncer.Sender = s.sender
	}
	if len(s.syncer.MasterKey) == 0 {
		s.syncer.MasterKey = s.masterKey
	}
	return s.syncer.ForwardMessage(ctx, userID, messageID, to, headers)
}

func (s *Server) RegisterProtectedAPI(pluginID string, route plugins.ProtectedAPIRoute) (plugins.ProtectedAPIRouteHandle, error) {
	handle, err := s.protectedAPIRouteRegistry().register(pluginID, route)
	if err != nil {
		return nil, err
	}
	logging.Debugf("plugin protected api registered plugin_id=%s path=%s prefix=%t", strings.TrimSpace(pluginID), cleanAPIPath(route.Path), route.Prefix)
	return handle, nil
}

func (s *Server) RegisterPublicAPI(pluginID string, route plugins.PublicAPIRoute) (plugins.ProtectedAPIRouteHandle, error) {
	handle, err := s.publicAPIRouteRegistry().register(pluginID, plugins.ProtectedAPIRoute{Path: route.Path, Prefix: route.Prefix, Handle: route.Handle})
	if err != nil {
		return nil, err
	}
	logging.Debugf("plugin public api registered plugin_id=%s path=%s prefix=%t", strings.TrimSpace(pluginID), cleanAPIPath(route.Path), route.Prefix)
	return handle, nil
}

func (s *Server) apiBackendPlugin(w http.ResponseWriter, r *http.Request, rest string) {
	cleanRest := cleanAPIPath(rest)
	pluginID, _, ok := strings.Cut(cleanRest, "/")
	if !ok || strings.TrimSpace(pluginID) == "" {
		http.NotFound(w, r)
		return
	}
	if !s.pluginEnabled(r.Context(), pluginID) {
		_ = s.stopBackendPlugin(pluginID)
		writeAPIError(w, http.StatusNotFound, "backend plugin is not enabled: "+pluginID)
		return
	}
	if _, ok, err := s.startBackendPlugin(r.Context(), pluginID); err != nil {
		s.serverError(w, err)
		return
	} else if !ok {
		writeAPIError(w, http.StatusNotFound, "backend plugin is not available: "+pluginID)
		return
	}
	if s.dispatchPublicAPIPath(w, r, "plugins/"+cleanRest) {
		return
	}
	if _, ok := s.requireAPIAuth(w, r); !ok {
		return
	}
	if s.dispatchProtectedAPIPath(w, r, "plugins/"+cleanRest) {
		return
	}
	writeAPIError(w, http.StatusNotFound, "backend plugin route not found: "+cleanRest)
}

func (s *Server) startBackendPlugin(ctx context.Context, pluginID string) (plugins.BackendPlugin, bool, error) {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" || s == nil {
		return nil, false, nil
	}
	if s.backendPluginsClosed() {
		return nil, false, errBackendPluginHostClosed
	}
	if !s.pluginEnabled(ctx, pluginID) {
		_ = s.stopBackendPlugin(pluginID)
		return nil, false, nil
	}
	s.backendLifecycleMu.Lock()
	defer s.backendLifecycleMu.Unlock()
	if s.backendLifecycleClosed {
		return nil, false, errBackendPluginHostClosed
	}
	if s.startedBackendPlugins == nil {
		s.startedBackendPlugins = map[string]plugins.BackendPlugin{}
	}
	if plugin := s.startedBackendPlugins[pluginID]; plugin != nil {
		return plugin, true, nil
	}
	plugin, ok, err := s.backendPlugin(pluginID)
	if err != nil {
		s.recordBackendPluginFailure(pluginID, err)
		return nil, false, err
	}
	if !ok || plugin == nil {
		return nil, false, nil
	}
	logging.Debugf("backend plugin starting plugin_id=%s", pluginID)
	if err := plugin.Start(s); err != nil {
		_ = plugin.Stop(s)
		unregistered := s.protectedAPIRouteRegistry().unregisterPlugin(pluginID)
		unregistered += s.publicAPIRouteRegistry().unregisterPlugin(pluginID)
		s.recordBackendPluginFailure(pluginID, err)
		// Start failures must reach the operator log even at the default log
		// level: the admin UI may still show the plugin as enabled.
		log.Printf("backend plugin start failed plugin_id=%s routes_unregistered=%d error=%v", pluginID, unregistered, err)
		return nil, true, err
	}
	s.startedBackendPlugins[pluginID] = plugin
	logging.Debugf("backend plugin started plugin_id=%s", pluginID)
	return plugin, true, nil
}

func (s *Server) stopBackendPlugin(pluginID string) error {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" || s == nil {
		return nil
	}
	s.backendLifecycleMu.Lock()
	defer s.backendLifecycleMu.Unlock()
	var plugin plugins.BackendPlugin
	if s.startedBackendPlugins != nil {
		plugin = s.startedBackendPlugins[pluginID]
		delete(s.startedBackendPlugins, pluginID)
	}
	return s.stopBackendPluginInstance(pluginID, plugin)
}

func (s *Server) stopBackendPluginInstance(pluginID string, plugin plugins.BackendPlugin) error {
	var stopErr error
	if plugin != nil {
		logging.Debugf("backend plugin stopping plugin_id=%s", pluginID)
		stopErr = plugin.Stop(s)
		if stopErr != nil {
			log.Printf("backend plugin stop failed plugin_id=%s error=%v", pluginID, stopErr)
		}
	}
	unregistered := s.protectedAPIRouteRegistry().unregisterPlugin(pluginID)
	unregistered += s.publicAPIRouteRegistry().unregisterPlugin(pluginID)
	if plugin != nil || unregistered > 0 {
		logging.Debugf("backend plugin stopped plugin_id=%s routes_unregistered=%d", pluginID, unregistered)
	}
	return stopErr
}

// Close stops every lifecycle-managed backend plugin before the application
// closes search indexes and user databases. It is idempotent and prevents an
// in-flight auto-start goroutine from installing a plugin after shutdown.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.backendLifecycleMu.Lock()
	defer s.backendLifecycleMu.Unlock()
	if s.backendLifecycleClosed {
		return nil
	}
	s.backendLifecycleClosed = true
	if s.ownedSyncRunnerCancel != nil {
		s.ownedSyncRunnerCancel()
		s.ownedSyncRunnerCancel = nil
	}
	started := s.startedBackendPlugins
	s.startedBackendPlugins = map[string]plugins.BackendPlugin{}

	ids := make([]string, 0, len(started))
	for pluginID := range started {
		ids = append(ids, pluginID)
	}
	sort.Strings(ids)
	var stopErrors []error
	for _, pluginID := range ids {
		if err := s.stopBackendPluginInstance(pluginID, started[pluginID]); err != nil {
			stopErrors = append(stopErrors, fmt.Errorf("stop backend plugin %s: %w", pluginID, err))
		}
	}
	return errors.Join(stopErrors...)
}

func (s *Server) backendPluginsClosed() bool {
	if s == nil {
		return true
	}
	s.backendLifecycleMu.Lock()
	defer s.backendLifecycleMu.Unlock()
	return s.backendLifecycleClosed
}

func (s *Server) dispatchProtectedAPIPath(w http.ResponseWriter, r *http.Request, apiPath string) bool {
	route, ok := s.protectedAPIRouteRegistry().match(apiPath)
	if !ok {
		return false
	}
	if route.pluginID != "" && !s.pluginEnabled(r.Context(), route.pluginID) {
		_ = s.stopBackendPlugin(route.pluginID)
		return false
	}
	if _, ok := s.requireAPIAuth(w, r); !ok {
		return true
	}
	route.handler(s, cleanAPIPath(apiPath), w, r)
	return true
}

func (s *Server) dispatchPublicAPIPath(w http.ResponseWriter, r *http.Request, apiPath string) bool {
	route, ok := s.publicAPIRouteRegistry().match(apiPath)
	if !ok {
		return false
	}
	if route.pluginID != "" && !s.pluginEnabled(r.Context(), route.pluginID) {
		_ = s.stopBackendPlugin(route.pluginID)
		return false
	}
	route.handler(s, cleanAPIPath(apiPath), w, r)
	return true
}

func (s *Server) backendPlugin(pluginID string) (plugins.BackendPlugin, bool, error) {
	if s == nil || s.backendPlugins == nil {
		return nil, false, nil
	}
	return s.backendPlugins.Plugin(pluginID)
}

func (s *Server) enabledBackendPlugins(ctx context.Context) ([]plugins.BackendPlugin, error) {
	if s == nil || s.backendPlugins == nil {
		return nil, nil
	}
	if s.backendPluginsClosed() {
		return nil, errBackendPluginHostClosed
	}
	ids := s.backendPlugins.PluginIDs()
	out := make([]plugins.BackendPlugin, 0, len(ids))
	for _, pluginID := range ids {
		if !s.pluginEnabled(ctx, pluginID) {
			_ = s.stopBackendPlugin(pluginID)
			continue
		}
		plugin, ok, err := s.startBackendPlugin(ctx, pluginID)
		if err != nil {
			log.Printf("backend plugin %s skipped after load/start failure: %v", pluginID, err)
			continue
		}
		if ok && plugin != nil {
			out = append(out, plugin)
		}
	}
	return out, nil
}

func (s *Server) authProviders(ctx context.Context) []apiAuthProvider {
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		log.Printf("auth providers: %v", err)
		return nil
	}
	var out []apiAuthProvider
	for _, backendPlugin := range backendPlugins {
		provider, ok := backendPlugin.(plugins.AuthProviderPlugin)
		if !ok {
			continue
		}
		for _, item := range provider.AuthProviders(ctx, s) {
			id := strings.TrimSpace(item.ID)
			name := strings.TrimSpace(item.Name)
			loginURL := strings.TrimSpace(item.LoginURL)
			if id == "" || name == "" || loginURL == "" {
				continue
			}
			out = append(out, apiAuthProvider{ID: id, Name: name, LoginURL: loginURL})
		}
	}
	return out
}

func (s *Server) recordBackendPluginFailure(pluginID string, err error) {
	if s == nil || s.backendPlugins == nil || err == nil {
		return
	}
	s.backendPlugins.SetFailure(pluginID, err)
}

func (s *Server) backendPluginFailure(pluginID string) string {
	if s == nil || s.backendPlugins == nil {
		return ""
	}
	return s.backendPlugins.Failure(pluginID)
}

func (s *Server) startAutoStartBackendPlugins(ctx context.Context) {
	if s == nil {
		return
	}
	for _, manifest := range s.pluginManifests {
		if !manifest.AutoStart || strings.TrimSpace(manifest.ID) == "" {
			continue
		}
		pluginID := manifest.ID
		if !s.pluginEnabled(ctx, pluginID) {
			continue
		}
		go func() {
			if _, _, err := s.startBackendPlugin(context.Background(), pluginID); err != nil {
				log.Printf("start backend plugin %s: %v", pluginID, err)
			}
		}()
	}
}

type protectedAPIRouteRegistry struct {
	mu     sync.RWMutex
	nextID uint64
	routes map[uint64]protectedAPIRouteEntry
}

type protectedAPIRouteEntry struct {
	id       uint64
	pluginID string
	path     string
	prefix   bool
	handler  plugins.ProtectedAPIHandler
}

type protectedAPIRouteHandle struct {
	registry *protectedAPIRouteRegistry
	id       uint64
	once     sync.Once
}

func newProtectedAPIRouteRegistry() *protectedAPIRouteRegistry {
	return &protectedAPIRouteRegistry{routes: map[uint64]protectedAPIRouteEntry{}}
}

func (s *Server) protectedAPIRouteRegistry() *protectedAPIRouteRegistry {
	if s.protectedAPIRoutes == nil {
		s.protectedAPIRoutes = newProtectedAPIRouteRegistry()
	}
	return s.protectedAPIRoutes
}

func (s *Server) publicAPIRouteRegistry() *protectedAPIRouteRegistry {
	if s.publicAPIRoutes == nil {
		s.publicAPIRoutes = newProtectedAPIRouteRegistry()
	}
	return s.publicAPIRoutes
}

func (r *protectedAPIRouteRegistry) register(pluginID string, route plugins.ProtectedAPIRoute) (plugins.ProtectedAPIRouteHandle, error) {
	pluginID = strings.TrimSpace(pluginID)
	path := cleanAPIPath(route.Path)
	if pluginID == "" {
		return nil, fmt.Errorf("protected API route has no plugin id")
	}
	if path == "" || strings.Contains(path, "..") {
		return nil, fmt.Errorf("protected API route %q is invalid", route.Path)
	}
	if route.Handle == nil {
		return nil, fmt.Errorf("protected API route %q has no handler", path)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.routes {
		if existing.pluginID == pluginID && existing.path == path && existing.prefix == route.Prefix {
			return nil, fmt.Errorf("protected API route %q is already registered by %s", path, pluginID)
		}
	}
	r.nextID++
	id := r.nextID
	r.routes[id] = protectedAPIRouteEntry{
		id:       id,
		pluginID: pluginID,
		path:     path,
		prefix:   route.Prefix,
		handler:  route.Handle,
	}
	return &protectedAPIRouteHandle{registry: r, id: id}, nil
}

func (r *protectedAPIRouteRegistry) match(apiPath string) (protectedAPIRouteEntry, bool) {
	path := cleanAPIPath(apiPath)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, route := range r.routes {
		if !route.prefix && route.path == path {
			return route, true
		}
	}
	var best protectedAPIRouteEntry
	for _, route := range r.routes {
		if !route.prefix || (path != route.path && !strings.HasPrefix(path, route.path+"/")) {
			continue
		}
		if best.path == "" || len(route.path) > len(best.path) {
			best = route
		}
	}
	if best.path == "" {
		return protectedAPIRouteEntry{}, false
	}
	return best, true
}

func (r *protectedAPIRouteRegistry) unregister(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, id)
}

func (r *protectedAPIRouteRegistry) unregisterPlugin(pluginID string) int {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := 0
	for id, route := range r.routes {
		if route.pluginID == pluginID {
			delete(r.routes, id)
			removed++
		}
	}
	return removed
}

func (h *protectedAPIRouteHandle) Unregister() {
	if h == nil || h.registry == nil {
		return
	}
	h.once.Do(func() {
		h.registry.unregister(h.id)
	})
}

func cleanAPIPath(value string) string {
	return strings.Trim(strings.TrimSpace(value), "/")
}
