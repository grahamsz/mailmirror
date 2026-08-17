// File overview: HTTP server assembly. It wires store/search/sync dependencies, sessions, security headers, static assets, and route middleware into one handler.

package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"rolltop/backend/auth"
	"rolltop/backend/blob"
	"rolltop/backend/buildinfo"
	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/mailparse"
	"rolltop/backend/plugins"
	"rolltop/backend/search"
	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

const (
	sessionCookie = "mm_session"
	csrfCookie    = "mm_csrf"
)

const (
	mailboxStatusRefreshInterval       = 15 * time.Minute
	mailboxStatusRefreshFailureBackoff = 5 * time.Minute
)

// Options wires concrete dependencies and runtime values into a new HTTP Server.
type Options struct {
	Store        *store.Store
	Blobs        *blob.Store
	Search       *search.Service
	Syncer       *syncer.Service
	SyncRunner   *syncer.Runner
	Sender       mailSender
	MasterKey    []byte
	DataDir      string
	DatabasePath string
	IndexPath    string
	PluginDir    string
	SessionTTL   time.Duration
	CookieSecure bool
	WebhookToken string
	// DisableBackgroundWorkers is used by focused embeddings and tests that
	// explicitly drive scheduler behavior themselves.
	DisableBackgroundWorkers bool
}

// Server owns HTTP handlers, shared services, session settings, event fanout, and lightweight caches.
type Server struct {
	store                     *store.Store
	blobs                     *blob.Store
	search                    *search.Service
	syncer                    *syncer.Service
	syncRunner                *syncer.Runner
	ownedSyncRunnerCancel     context.CancelFunc
	sender                    mailSender
	masterKey                 []byte
	dataDir                   string
	databasePath              string
	indexPath                 string
	pluginDir                 string
	pluginManifests           []plugins.Manifest
	backendPlugins            *plugins.BackendManager
	protectedAPIRoutes        *protectedAPIRouteRegistry
	publicAPIRoutes           *protectedAPIRouteRegistry
	backendLifecycleMu        sync.Mutex
	backendLifecycleClosed    bool
	startedBackendPlugins     map[string]plugins.BackendPlugin
	sessionTTL                time.Duration
	cookieSecure              bool
	webhookToken              string
	events                    *eventHub
	statusMu                  sync.Mutex
	statusRefreshRunning      map[int64]bool
	statusRefreshBlockedUntil map[int64]time.Time
	deletingMu                sync.Mutex
	deletingIMAPAccounts      map[int64]map[int64]bool
	storageMu                 sync.Mutex
	storageCached             map[int64]storageStatsCacheEntry
	mailWarmMu                sync.Mutex
	mailWarmRunning           map[int64]bool
	mailListCache             *mailListCache
	attachmentRepairMu        sync.Mutex
	attachmentRepairRunning   map[int64]map[int64]bool
	senderContactIconMu       sync.Mutex
	senderContactIconCache    map[int64]map[string]senderContactIconCacheEntry
	composeIdentityMu         sync.Mutex
	composeIdentityCache      map[int64]composeIdentityCacheEntry
	webPushDeliveryLocks      sync.Map
	webPushScheduleMu         sync.Mutex
	webPushRunning            map[int64]bool
	webPushDirty              map[int64]bool
	webPushClient             *http.Client
	webPushSend               webPushSendFunc
	webPushRetryDelay         func(int) time.Duration
	snoozePushScheduleMu      sync.Mutex
	snoozePushRunning         map[int64]bool
	snoozePushDirty           map[int64]bool
	snoozeSchedulerWake       chan struct{}
	startedAt                 time.Time
}

type contextKey string

const userContextKey contextKey = "current-user"

// sessionErrorContextKey carries a session-lookup store failure through the
// request so that only routes which actually need the session answer 503;
// static assets, login, setup, and logout keep working during a store outage.
const sessionErrorContextKey contextKey = "session-lookup-error"

type currentUser struct {
	User         store.User
	SessionHash  string
	SessionToken string
}

type mailSender interface {
	Send(ctx context.Context, account store.MailAccount, msg smtpclient.Message) ([]byte, error)
}

type viewData struct {
	Mailboxes      []store.MailboxSummary
	LatestSyncRun  *store.SyncRun
	ActiveSyncRuns []store.SyncRun
	SyncRunning    bool
}

type threadMessageView struct {
	Message                  store.MessageRecord
	Attachments              []store.Attachment
	InlineAttachments        []store.Attachment
	HeaderDetails            []messageHeaderDetail
	SecurityIndicators       messageSecurityIndicators
	OneClickUnsub            bool
	OneClickSentAt           time.Time
	AttachmentMatches        []string
	AttachmentContentMatched bool
	AttachmentMatchTerms     []string
	SenderName               string
	SenderEmail              string
	SenderInitial            string
	RecipientLine            string
	Snippet                  string
	DisplayBodyHTML          string
	DisplayBodyText          string
	HasHiddenQuoted          bool
	HasDisplayBody           bool
	BodyPreviewOnly          bool
	HasRemoteImages          bool
	ImagesAllowed            bool
	ImageBlockRules          []string
	Expanded                 bool
	CanReplyAll              bool
}

type conversationView struct {
	Message                  store.MessageRecord
	MessageIDs               []int64
	MessageAccountIDs        []int64
	StarredMessageID         int64
	Participants             string
	RecipientParticipants    string
	Count                    int
	IsRead                   bool
	HasAttachments           bool
	AttachmentNames          []string
	AttachmentMatches        []string
	AttachmentContentMatched bool
	Snippet                  string
	MatchTerms               []string
	MatchFields              []string
	MatchQueryTerms          []string
	SnoozedUntil             time.Time
	CanReplyAll              bool
}

type composeForm struct {
	To                   string                      `json:"to"`
	Cc                   string                      `json:"cc"`
	Bcc                  string                      `json:"bcc"`
	Subject              string                      `json:"subject"`
	Body                 string                      `json:"body"`
	BodyHTML             string                      `json:"body_html"`
	DraftMessageID       int64                       `json:"draft_message_id"`
	InReplyToID          int64                       `json:"in_reply_to_id"`
	FromIdentityID       int64                       `json:"from_identity_id"`
	AvailableAttachments []composeExistingAttachment `json:"available_attachments,omitempty"`
	IncludeAttachmentIDs []int64                     `json:"include_attachment_ids,omitempty"`
	ForwardAttachmentID  int64                       `json:"forward_attachment_message_id,omitempty"`
	ForwardAttachment    *composeExistingAttachment  `json:"forward_attachment,omitempty"`
	Attachments          []composeAttachment         `json:"attachments,omitempty"`
	SecurityEncrypted    bool                        `json:"pgp_encrypted,omitempty"`
	SecuritySigned       bool                        `json:"pgp_signed,omitempty"`
	SecurityMIME         bool                        `json:"pgp_mime,omitempty"`
	SecuritySignature    string                      `json:"pgp_signature,omitempty"`
	AttachPublicKey      bool                        `json:"attach_public_key,omitempty"`
}

type composeExistingAttachment struct {
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"download_url"`
}

type composeAttachment struct {
	Field       string `json:"field"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ContentID   string `json:"content_id"`
	Inline      bool   `json:"inline"`
	Size        int64  `json:"size,omitempty"`
	Data        []byte `json:"-"`
}

type syncFolderView struct {
	Mailbox    store.MailboxSummary
	IsRunning  bool
	LastRun    *store.SyncRun
	CanSyncNow bool
}

// New wires the HTTP server dependency graph. It also installs default session
// TTL, sync runner, and SMTP sender so tests can pass only the collaborators they
// care about.
func New(opts Options) (*Server, error) {
	if opts.SessionTTL == 0 {
		opts.SessionTTL = 30 * 24 * time.Hour
	}
	var ownedSyncRunnerCancel context.CancelFunc
	if opts.SyncRunner == nil && opts.Syncer != nil {
		var runnerContext context.Context
		runnerContext, ownedSyncRunnerCancel = context.WithCancel(context.Background())
		opts.SyncRunner = syncer.NewRunnerWithContext(runnerContext, opts.Syncer)
	}
	if opts.Sender == nil && len(opts.MasterKey) == 32 {
		opts.Sender = &smtpclient.Sender{MasterKey: opts.MasterKey}
	}
	if strings.TrimSpace(opts.PluginDir) == "" {
		opts.PluginDir = "plugins"
	}
	pluginManifests, err := plugins.LoadManifests(opts.PluginDir)
	if err != nil {
		return nil, err
	}
	if opts.Store != nil {
		if err := opts.Store.SyncPluginDefinitions(context.Background(), plugins.DefinitionsFromManifests(pluginManifests)); err != nil {
			return nil, err
		}
	}
	backendPlugins := plugins.NewBackendManager(opts.PluginDir, pluginManifests)
	for _, manifest := range pluginManifests {
		if manifest.Backend == nil || manifest.Backend.Kind != "go-plugin" {
			continue
		}
		if _, _, err := backendPlugins.Plugin(manifest.ID); err != nil {
			log.Printf("backend plugin %s unavailable after load failure: %v", manifest.ID, err)
		}
	}
	events := newEventHub()
	srv := &Server{
		store:                 opts.Store,
		blobs:                 opts.Blobs,
		search:                opts.Search,
		syncer:                opts.Syncer,
		syncRunner:            opts.SyncRunner,
		ownedSyncRunnerCancel: ownedSyncRunnerCancel,
		sender:                opts.Sender,
		masterKey:             opts.MasterKey,
		dataDir:               opts.DataDir,
		databasePath:          opts.DatabasePath,
		indexPath:             opts.IndexPath,
		pluginDir:             opts.PluginDir,
		pluginManifests:       pluginManifests,
		backendPlugins:        backendPlugins,
		protectedAPIRoutes:    newProtectedAPIRouteRegistry(),
		publicAPIRoutes:       newProtectedAPIRouteRegistry(),
		startedBackendPlugins: map[string]plugins.BackendPlugin{},
		sessionTTL:            opts.SessionTTL,
		cookieSecure:          opts.CookieSecure,
		webhookToken:          strings.TrimSpace(opts.WebhookToken),
		events:                events,

		statusRefreshRunning:      map[int64]bool{},
		statusRefreshBlockedUntil: map[int64]time.Time{},
		deletingIMAPAccounts:      map[int64]map[int64]bool{},
		mailListCache:             newMailListCache(),
		mailWarmRunning:           map[int64]bool{},
		attachmentRepairRunning:   map[int64]map[int64]bool{},
		senderContactIconCache:    map[int64]map[string]senderContactIconCacheEntry{},
		composeIdentityCache:      map[int64]composeIdentityCacheEntry{},
		webPushRunning:            map[int64]bool{},
		webPushDirty:              map[int64]bool{},
		webPushClient:             newWebPushHTTPClient(),
		snoozePushRunning:         map[int64]bool{},
		snoozePushDirty:           map[int64]bool{},
		snoozeSchedulerWake:       make(chan struct{}, 1),
		startedAt:                 time.Now().UTC(),
	}
	if opts.Syncer != nil {
		opts.Syncer.Notify = srv.notifyUserChanged
		opts.Syncer.NotifyProgress = srv.notifySyncProgress
		opts.Syncer.NotifyRestoredState = srv.notifySnoozeStateChanged
	}
	if opts.SyncRunner != nil {
		if err := opts.SyncRunner.RecoverPendingBlobCleanups(); err != nil {
			if ownedSyncRunnerCancel != nil {
				ownedSyncRunnerCancel()
			}
			return nil, fmt.Errorf("recover pending blob cleanups: %w", err)
		}
		if err := opts.SyncRunner.RecoverPendingInboxArrivals(); err != nil {
			if ownedSyncRunnerCancel != nil {
				ownedSyncRunnerCancel()
			}
			return nil, fmt.Errorf("recover pending Inbox arrivals: %w", err)
		}
	}
	srv.startAutoStartBackendPlugins(context.Background())
	srv.warmAllMailFirstPages(context.Background())
	if !opts.DisableBackgroundWorkers {
		srv.resumeNewMailWebPushAsync()
		srv.resumeSnoozeReminderWebPushAsync()
		srv.startSnoozeScheduler()
	}
	return srv, nil
}

// Handler defines the browser/API/static route surface, then wraps it with session
// lookup and security headers. SPA routes intentionally point at handleApp so a
// hard reload keeps the client-side URL.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/api/", s.handleAPI)
	mux.HandleFunc("/assets/", s.handleFrontendAsset)
	mux.HandleFunc("/manifest.webmanifest", s.handleFrontendAsset)
	mux.HandleFunc("/sw.js", s.handleFrontendAsset)
	mux.HandleFunc("/offline.html", s.handleFrontendAsset)
	mux.HandleFunc("/icon.svg", s.handleFrontendAsset)
	mux.HandleFunc("/android/latest.json", s.handleAndroidLatest)
	mux.HandleFunc("/android/rolltop.apk", s.handleAndroidAPK)
	mux.HandleFunc("/android/", s.handleFrontendAsset)
	mux.HandleFunc("/setup", s.handleApp)
	mux.HandleFunc("/login", s.handleApp)
	mux.HandleFunc("/mail", s.handleApp)
	mux.HandleFunc("/mail/", s.handleApp)
	mux.HandleFunc("/snoozes", s.handleApp)
	mux.HandleFunc("/mailbox/", s.handleApp)
	mux.HandleFunc("/search", s.handleApp)
	mux.HandleFunc("/search/", s.handleApp)
	mux.HandleFunc("/compose", s.handleApp)
	mux.HandleFunc("/contacts", s.handleApp)
	mux.HandleFunc("/contacts/", s.handleContactOrApp)
	mux.HandleFunc("/webhooks/sync", s.handleSyncWebhook)
	mux.HandleFunc("/messages/", s.handleApp)
	mux.HandleFunc("/attachments/", s.handleAttachment)
	mux.HandleFunc("/blobs/", s.handleBlob)
	mux.HandleFunc("/remote-images/", s.handleRemoteImage)
	mux.HandleFunc("/brand-icons/", s.handleBrandIcon)
	mux.HandleFunc("/plugins/", s.handlePluginRoute)
	mux.HandleFunc("/sync-runs/", s.handleApp)
	mux.HandleFunc("/settings/account", s.handleApp)
	mux.HandleFunc("/settings/account/", s.handleApp)
	mux.HandleFunc("/admin/users", s.handleApp)
	return s.securityHeaders(s.withCurrentUser(mux))
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := buildinfo.Current()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval'; worker-src 'self' blob:; child-src 'self' blob:; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com data:; img-src 'self' data: blob: http: https: cid:; form-action 'self'; base-uri 'none'")
		w.Header().Set("Link", `<https://rolltop.app>; rel="home"`)
		w.Header().Set("X-rolltop-Version", info.Version)
		w.Header().Set("X-rolltop-Release", info.Label)
		if info.BuildDate != "" {
			w.Header().Set("X-rolltop-Build-Date", info.BuildDate)
		}
		if info.Commit != "" {
			w.Header().Set("X-rolltop-Commit", info.Commit)
		}
		next.ServeHTTP(w, r)
	})
}

// withCurrentUser resolves the signed session cookie into currentUser context.
// Normal browser/API routes derive user_id only here, never from request params.
func (s *Server) withCurrentUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err == nil && cookie.Value != "" {
			hash := mmcrypto.TokenHash(cookie.Value)
			sess, user, err := s.store.GetSessionUser(r.Context(), hash)
			switch {
			case err == nil:
				cu := currentUser{User: user, SessionHash: sess.TokenHash, SessionToken: cookie.Value}
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, cu))
			case store.IsNotFound(err):
				// Missing or expired session row: genuinely signed out.
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				// The client abandoned the request mid-lookup; stay anonymous
				// without logging so ordinary tab closes do not read like
				// store failures.
			default:
				// A store failure (busy database under heavy sync, disk
				// pressure) must not demote a valid session to anonymous:
				// that turns every route into 401 "login required" and forces
				// a spurious logout. Record it so session-dependent routes
				// answer 503 while assets, login, and logout still work.
				log.Printf("session lookup failed: %v", err)
				r = r.WithContext(context.WithValue(r.Context(), sessionErrorContextKey, err))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func current(r *http.Request) (currentUser, bool) {
	cu, ok := r.Context().Value(userContextKey).(currentUser)
	return cu, ok
}

// sessionLookupFailed reports whether this request carried a session cookie
// that could not be resolved because of a store failure (as opposed to a
// missing or expired session).
func sessionLookupFailed(r *http.Request) bool {
	_, failed := r.Context().Value(sessionErrorContextKey).(error)
	return failed
}

func sessionUnavailable(w http.ResponseWriter) {
	http.Error(w, "session lookup temporarily unavailable, retry shortly", http.StatusServiceUnavailable)
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if _, ok := s.requireAuth(w, r); !ok {
		return
	}
	http.Redirect(w, r, "/mail", http.StatusSeeOther)
}

func (s *Server) handleSyncWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.webhookToken == "" || !s.validWebhookToken(r) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if s.syncRunner == nil {
		http.Error(w, "sync is not configured", http.StatusServiceUnavailable)
		return
	}
	userIDs, err := s.store.ListUserIDsWithAccounts(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	type result struct {
		UserID  int64  `json:"user_id"`
		Started bool   `json:"started"`
		Folder  string `json:"folder"`
	}
	resp := struct {
		Results []result `json:"results"`
	}{Results: make([]result, 0, len(userIDs))}
	for _, userID := range userIDs {
		started := s.syncRunner.StartMailboxes(userID, []string{"INBOX"})
		resp.Results = append(resp.Results, result{UserID: userID, Started: started, Folder: "INBOX"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) validWebhookToken(r *http.Request) bool {
	got := strings.TrimSpace(r.Header.Get("X-Rolltop-Webhook-Token"))
	if got == "" {
		if authz := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			got = strings.TrimSpace(authz[len("bearer "):])
		}
	}
	if got == "" {
		got = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if got == "" || s.webhookToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.webhookToken)) == 1
}

// handleAttachment serves a tenant-scoped attachment. If no standalone attachment
// blob exists, it falls back to the parent raw message blob or IMAP hydration and
// extracts the matching MIME part just in time.
func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Cookie")
	pathValue := r.URL.Path
	inline := false
	switch {
	case strings.HasSuffix(pathValue, "/download"):
		pathValue = strings.TrimSuffix(pathValue, "/download")
	case strings.HasSuffix(pathValue, "/inline"):
		pathValue = strings.TrimSuffix(pathValue, "/inline")
		inline = true
	}
	id, ok := idFromPath(pathValue, "/attachments/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	att, err := s.store.GetAttachmentForUser(r.Context(), cu.User.ID, id)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	if strings.TrimSpace(att.BlobPath) != "" {
		file, err := s.blobs.OpenUserBlob(cu.User.ID, att.BlobPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", att.ContentType)
		w.Header().Set("Content-Disposition", attachmentContentDisposition(inline, att.Filename, att.ContentType))
		_, _ = io.Copy(w, file)
		return
	}

	msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, att.MessageID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	raw, err := s.rawMessageBytes(r.Context(), cu.User.ID, msg)
	if err != nil {
		http.Error(w, "attachment body is not available locally and could not be fetched from IMAP", http.StatusGone)
		return
	}
	parsed, err := mailparse.Parse(raw)
	if err != nil {
		http.Error(w, "attachment body could not be parsed from the message", http.StatusGone)
		return
	}
	file, ok := matchingAttachment(att, parsed.Files)
	if !ok {
		http.NotFound(w, r)
		return
	}
	contentType := file.ContentType
	if strings.TrimSpace(contentType) == "" {
		contentType = att.ContentType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", attachmentContentDisposition(inline, att.Filename, contentType))
	_, _ = w.Write(file.Data)
}

func attachmentContentDisposition(inline bool, filename, contentType string) string {
	disposition := "attachment"
	if inline && strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "image/") {
		disposition = "inline"
	}
	name := path.Base(strings.TrimSpace(filename))
	if name == "." || name == "/" || name == "" {
		name = "attachment"
	}
	return fmt.Sprintf("%s; filename=%q", disposition, name)
}

// handleBlob serves a raw blob record for the signed-in user. Message blobs can be
// regenerated from IMAP/local hydration when retention has pruned the file but the
// metadata still points at a fetchable message.
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Cookie")
	id, ok := idFromPath(r.URL.Path, "/blobs/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	blobRec, err := s.store.GetBlobForUser(r.Context(), cu.User.ID, id)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	file, err := s.blobs.OpenUserBlob(cu.User.ID, blobRec.Path)
	if err != nil {
		if blobRec.Kind != "message" && blobRec.Kind != "message-remote" {
			http.NotFound(w, r)
			return
		}
		msg, err := s.store.GetMessageByBlobIDForUser(r.Context(), cu.User.ID, blobRec.ID)
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, err)
			return
		}
		raw, err := s.rawMessageBytes(r.Context(), cu.User.ID, msg)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "message/rfc822")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(blobRec.Path)))
		_, _ = w.Write(raw)
		return
	}
	defer file.Close()
	if blobRec.Kind == "message" {
		w.Header().Set("Content-Type", "message/rfc822")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(blobRec.Path)))
	_, _ = io.Copy(w, file)
}

// matchingAttachment reconciles stored attachment metadata with freshly parsed MIME
// parts. Content-ID wins for inline media, then filename/size, then type/size.
func matchingAttachment(target store.Attachment, files []mailparse.Attachment) (mailparse.Attachment, bool) {
	targetName := strings.TrimSpace(target.Filename)
	targetCID := strings.Trim(target.ContentID, "<>")
	targetType := strings.TrimSpace(strings.ToLower(target.ContentType))
	for _, file := range files {
		if targetCID != "" && strings.EqualFold(strings.Trim(file.ContentID, "<>"), targetCID) {
			return file, true
		}
	}
	for _, file := range files {
		if targetName != "" && strings.EqualFold(strings.TrimSpace(file.Filename), targetName) {
			if target.Size <= 0 || int64(len(file.Data)) == target.Size {
				return file, true
			}
		}
	}
	for _, file := range files {
		if targetName != "" && strings.EqualFold(strings.TrimSpace(file.Filename), targetName) {
			return file, true
		}
		if targetType != "" && strings.EqualFold(strings.TrimSpace(file.ContentType), targetType) && target.Size > 0 && int64(len(file.Data)) == target.Size {
			return file, true
		}
	}
	return mailparse.Attachment{}, false
}

// loadMailboxChrome assembles the shared folder/sync state used by API presenters
// and the React chrome. It may kick off a cheap background STATUS refresh when remote
// counters are stale.
func (s *Server) loadMailboxChrome(ctx context.Context, userID int64, data *viewData) {
	if data == nil {
		return
	}
	if boxes, err := s.store.ListMailboxesForUser(ctx, userID); err == nil {
		boxes = s.filterDeletingMailboxes(userID, boxes)
		data.Mailboxes = boxes
		s.maybeRefreshMailboxStatuses(userID, boxes)
	}
	if runs, err := s.store.ListSyncRunsForUser(ctx, userID, 50); err == nil && len(runs) > 0 {
		latest := runs[0]
		data.LatestSyncRun = &latest
		for _, run := range runs {
			if strings.EqualFold(strings.TrimSpace(run.Status), "running") {
				data.ActiveSyncRuns = append(data.ActiveSyncRuns, run)
			}
		}
	}
	if s.syncRunner != nil {
		data.SyncRunning = s.syncRunner.IsRunning(userID)
	}
}

func (s *Server) maybeRefreshMailboxStatuses(userID int64, boxes []store.MailboxSummary) {
	if s.syncer == nil || s.syncer.Fetcher == nil || len(boxes) == 0 {
		return
	}
	cutoff := time.Now().Add(-mailboxStatusRefreshInterval)
	needsRefresh := false
	for _, box := range boxes {
		if box.StatusCheckedAt.IsZero() || box.StatusCheckedAt.Before(cutoff) {
			needsRefresh = true
			break
		}
	}
	if !needsRefresh {
		return
	}
	s.refreshMailboxStatusesAsync(userID)
}

// refreshMailboxStatusesAsync deduplicates and rate-limits background STATUS
// refreshes so opening settings or the sidebar does not start overlapping IMAP
// probes for the same user.
func (s *Server) refreshMailboxStatusesAsync(userID int64) {
	if s.syncer == nil || s.syncer.Fetcher == nil {
		return
	}
	if s.syncRunner != nil && s.syncRunner.IsRunning(userID) {
		return
	}
	now := time.Now()
	s.statusMu.Lock()
	if s.statusRefreshRunning[userID] {
		s.statusMu.Unlock()
		return
	}
	if until, ok := s.statusRefreshBlockedUntil[userID]; ok && now.Before(until) {
		s.statusMu.Unlock()
		return
	}
	s.statusRefreshRunning[userID] = true
	s.statusMu.Unlock()

	go func() {
		defer func() {
			s.statusMu.Lock()
			delete(s.statusRefreshRunning, userID)
			s.statusMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := s.syncer.DiscoverMailboxes(ctx, userID); err != nil {
			log.Printf("refresh mailbox statuses user_id=%d: %v", userID, err)
			s.statusMu.Lock()
			s.statusRefreshBlockedUntil[userID] = time.Now().Add(mailboxStatusRefreshFailureBackoff)
			s.statusMu.Unlock()
			return
		}
		s.statusMu.Lock()
		delete(s.statusRefreshBlockedUntil, userID)
		s.statusMu.Unlock()
	}()
}

// syncFolderViews joins mailbox summaries with complete per-folder sync history
// and search index counts for the settings folder-sync UI.
func (s *Server) syncFolderViews(ctx context.Context, userID int64) ([]syncFolderView, error) {
	return s.buildSyncFolderViews(ctx, userID, true, true)
}

// syncFolderProgressViews skips static effective-mode resolution and historical
// sync-run ranking because the polling DTO contains only changing counters and
// current runner state. The full account response still supplies both.
func (s *Server) syncFolderProgressViews(ctx context.Context, userID int64) ([]syncFolderView, error) {
	return s.buildSyncFolderViews(ctx, userID, false, false)
}

func (s *Server) buildSyncFolderViews(ctx context.Context, userID int64, includeCanSyncNow, includeLastRuns bool) ([]syncFolderView, error) {
	boxes, err := s.store.ListMailboxesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	boxes = s.filterDeletingMailboxes(userID, boxes)
	var runs []store.SyncRun
	if includeLastRuns {
		runs, err = s.store.ListLatestSyncRunsByMailboxForUser(ctx, userID)
		if err != nil {
			return nil, err
		}
	}
	var searchCounts map[int64]int
	if s.search != nil {
		searchCounts = map[int64]int{}
		needsSearchCounts := false
		for _, box := range boxes {
			if box.IncludeInSearch && box.SearchIndexKnown {
				needsSearchCounts = true
				break
			}
		}
		if needsSearchCounts {
			searchCounts, err = s.store.CountSearchIndexedMessagesByMailbox(ctx, userID)
			if err != nil {
				return nil, err
			}
		}
	}
	lastByFolder := map[string]store.SyncRun{}
	for _, run := range runs {
		name := strings.ToLower(strings.TrimSpace(run.CurrentMailbox))
		if name == "" {
			continue
		}
		key := fmt.Sprintf("%d:%s", run.AccountID, name)
		if _, ok := lastByFolder[key]; !ok {
			lastByFolder[key] = run
		}
	}
	out := make([]syncFolderView, 0, len(boxes))
	for _, box := range boxes {
		if searchCounts != nil {
			setMailboxSearchIndexStats(&box, searchCounts[box.ID], box.SearchIndexPurged)
		}
		canSyncNow := false
		if includeCanSyncNow {
			effectiveMode := box.SyncMode
			if effectiveMode == "inherit" {
				if mode, err := s.store.EffectiveMailboxSyncMode(ctx, userID, box.AccountID, box.Mailbox); err == nil {
					effectiveMode = mode
				}
			}
			canSyncNow = effectiveMode != "never"
		}
		view := syncFolderView{
			Mailbox:    box,
			CanSyncNow: canSyncNow,
		}
		if s.syncRunner != nil {
			view.IsRunning = s.syncRunner.IsAccountMailboxRunning(userID, box.AccountID, box.Name)
		}
		if run, ok := lastByFolder[fmt.Sprintf("%d:%s", box.AccountID, strings.ToLower(strings.TrimSpace(box.Name)))]; ok {
			r := run
			view.LastRun = &r
		}
		out = append(out, view)
	}
	return out, nil
}

func (s *Server) markDeletingIMAPAccount(userID, accountID int64) {
	s.deletingMu.Lock()
	defer s.deletingMu.Unlock()
	if s.deletingIMAPAccounts[userID] == nil {
		s.deletingIMAPAccounts[userID] = map[int64]bool{}
	}
	s.deletingIMAPAccounts[userID][accountID] = true
}

func (s *Server) clearDeletingIMAPAccount(userID, accountID int64) {
	s.deletingMu.Lock()
	defer s.deletingMu.Unlock()
	if s.deletingIMAPAccounts[userID] == nil {
		return
	}
	delete(s.deletingIMAPAccounts[userID], accountID)
	if len(s.deletingIMAPAccounts[userID]) == 0 {
		delete(s.deletingIMAPAccounts, userID)
	}
}

func (s *Server) imapAccountDeleting(userID, accountID int64) bool {
	s.deletingMu.Lock()
	defer s.deletingMu.Unlock()
	return s.deletingIMAPAccounts[userID] != nil && s.deletingIMAPAccounts[userID][accountID]
}

func (s *Server) filterDeletingAccounts(userID int64, accounts []store.MailAccount) []store.MailAccount {
	if len(accounts) == 0 {
		return accounts
	}
	out := accounts[:0]
	for _, account := range accounts {
		if !s.imapAccountDeleting(userID, account.ID) {
			out = append(out, account)
		}
	}
	return out
}

func (s *Server) filterDeletingMailboxes(userID int64, boxes []store.MailboxSummary) []store.MailboxSummary {
	if len(boxes) == 0 {
		return boxes
	}
	out := boxes[:0]
	for _, box := range boxes {
		if !s.imapAccountDeleting(userID, box.AccountID) {
			out = append(out, box)
		}
	}
	return out
}

func setMailboxSearchIndexStats(box *store.MailboxSummary, indexed int, purged bool) {
	if box == nil || !box.IncludeInSearch {
		return
	}
	if !box.SearchIndexKnown {
		return
	}
	// The marker is written only after a successful Bleve commit. Exact missing
	// and stale-document reconciliation remains in background index repair; this
	// settings counter deliberately avoids opening or enumerating the index.
	total := box.LocalMessageCount
	percent := boundedPercent(indexed, total)
	box.SearchIndexedCount = &indexed
	box.SearchIndexTotal = &total
	box.SearchIndexPercent = &percent
	box.SearchIndexPurged = purged
}

func boundedPercent(done, total int) int {
	if total <= 0 {
		return 0
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	return (done * 100) / total
}

func (s *Server) loginUser(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, err := auth.NewOpaqueToken()
	if err != nil {
		return err
	}
	expires := time.Now().UTC().Add(s.sessionTTL)
	if _, err := s.store.CreateSession(r.Context(), userID, mmcrypto.TokenHash(token), expires); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cookieSecure,
	})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.cookieSecure})
	return nil
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) (currentUser, bool) {
	// A resolved session already proves users exist; skip the CountUsers
	// round-trip that would otherwise run on every authenticated request.
	if cu, ok := current(r); ok {
		return cu, true
	}
	if sessionLookupFailed(r) {
		// The session cookie could not be checked against the store; a
		// redirect to /login here would force a spurious logout.
		sessionUnavailable(w)
		return currentUser{}, false
	}
	usersExist, err := s.usersExist(r.Context())
	if err != nil {
		s.serverError(w, err)
		return currentUser{}, false
	}
	if !usersExist {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return currentUser{}, false
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
	return currentUser{}, false
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (currentUser, bool) {
	cu, ok := s.requireAuth(w, r)
	if !ok {
		return currentUser{}, false
	}
	if !cu.User.IsAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return currentUser{}, false
	}
	return cu, true
}

// usersExist reports whether at least one local user account exists. Callers
// must treat an error as "unknown", never as "no users": claiming no users on
// a transient store failure sends signed-in browsers back to the first-run
// admin setup screen and would let /api/setup mint a fresh admin account.
func (s *Server) usersExist(ctx context.Context) (bool, error) {
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		return s.csrfForBase(cookie.Value)
	}
	cookie, err := r.Cookie(csrfCookie)
	base := ""
	if err == nil {
		base = cookie.Value
	}
	if base == "" {
		base, _ = auth.NewOpaqueToken()
		http.SetCookie(w, &http.Cookie{
			Name:     csrfCookie,
			Value:    base,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   s.cookieSecure,
		})
	}
	return s.csrfForBase(base)
}

func (s *Server) verifyCSRF(w http.ResponseWriter, r *http.Request) bool {
	base := ""
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		base = cookie.Value
	} else if cookie, err := r.Cookie(csrfCookie); err == nil {
		base = cookie.Value
	}
	if base == "" {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return false
	}
	expected := s.csrfForBase(base)
	got := r.FormValue("csrf_token")
	if got == "" {
		got = r.Header.Get("X-CSRF-Token")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) csrfForBase(base string) string {
	mac := hmac.New(sha256.New, s.masterKey)
	mac.Write([]byte("rolltop-csrf\x00"))
	mac.Write([]byte(base))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		http.Error(w, "request canceled", http.StatusRequestTimeout)
		return
	}
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, POST")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func idFromPath(value, prefix string) (int64, bool) {
	if !strings.HasPrefix(value, prefix) {
		return 0, false
	}
	rest := strings.Trim(strings.TrimPrefix(value, prefix), "/")
	if rest == "" || strings.Contains(rest, "/") {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func atoiDefault(value string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func pageFromRequest(r *http.Request) int {
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		return 1
	}
	return page
}
