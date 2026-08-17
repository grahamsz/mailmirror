// File overview: Runtime-loaded Go plugin ABI. Backend plugin binaries are
// built separately with -buildmode=plugin, then loaded from manifest metadata
// only when an enabled plugin route or hook needs them.

package plugins

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	goplugin "plugin"
	"strings"
	"sync"
	"time"

	"rolltop/backend/logging"
)

// CurrentUser is the authenticated user shape exposed to backend plugins.
type CurrentUser struct {
	UserID int64
}

// BackendHost is the non-HTTP service surface available to runtime plugins.
type BackendHost interface {
	Store() any
	MasterKey() []byte
	PluginEnabled(context.Context, string) bool
}

// AccountMailboxSyncHost is an optional host capability for plugins that write
// a message to a configured IMAP destination. Implementations must validate the
// account and mailbox against userID before queuing the incremental refetch.
type AccountMailboxSyncHost interface {
	BackendHost
	QueueAccountMailboxSync(context.Context, int64, int64, string) error
}

// MailboxGenerationRecoveryHost is an optional, process-local scheduling gate
// for plugins that mirror mail into Rolltop-managed IMAP destinations. The
// durable store marker remains the startup fallback; this capability covers
// the interval after the marker clears while deferred core work is replayed.
type MailboxGenerationRecoveryHost interface {
	BackendHost
	MailboxGenerationRecoveryActive(int64) bool
}

// ForegroundOperationHost is an optional scheduling capability for plugins
// that perform a long-lived, tenant-scoped mail write. The host pauses derived
// maintenance and serializes mailbox-generation recovery until the returned
// release function is called.
type ForegroundOperationHost interface {
	BackendHost
	BeginForegroundOperation(context.Context, int64) (func(), error)
}

// StoredMessageContext is the host-owned subset of a newly mirrored message
// passed to plugins that need to react after a message row exists.
type StoredMessageContext struct {
	UserID      int64
	MessageID   int64
	AccountID   int64
	MailboxID   int64
	MailboxName string
	UID         uint32
	Date        time.Time
	From        string
	To          string
	CC          string
	Subject     string
	IsRead      bool
	IsStarred   bool
}

// SearchMatchResult mirrors the search service's per-message match metadata in
// a plugin-neutral shape.
type SearchMatchResult struct {
	Matched    bool
	Score      float64
	Terms      []string
	QueryTerms []string
	Fields     []string
}

const (
	// Similarity searches are deliberately bounded because stored-message hooks
	// run inline with sync and candidate doc-ID queries grow with every ID.
	MaxSimilarityExplicitCandidates   = 2000
	MaxSimilarityRecentReadCandidates = 5000
	MaxSimilarityTerms                = 48
	MaxSimilarityResults              = 12

	SimilarityFieldSubject    = "subject"
	SimilarityFieldFromDomain = "from_domain"
	SimilarityFieldBody       = "body"
)

// SimilarityTerm is one weighted piece of evidence used to find messages with
// related subject, sender-domain, or body text. Text is analyzed by the same
// Bleve field analyzer used while indexing mail.
type SimilarityTerm struct {
	Field  string
	Text   string
	Weight float64
}

// RecentReadCandidates asks the host to resolve candidates from authoritative
// SQLite read state. Since is clamped to the last 90 days and Limit to
// MaxSimilarityRecentReadCandidates.
type RecentReadCandidates struct {
	Since time.Time
	Limit int
}

// SimilarMessagesRequest selects exactly one candidate source: caller-provided
// IDs or recent read mail. The host ownership-checks every candidate, always
// excludes CurrentMessageID and its thread, and then applies ExcludeMessageIDs.
type SimilarMessagesRequest struct {
	CandidateMessageIDs []int64
	RecentRead          *RecentReadCandidates
	CurrentMessageID    int64
	ExcludeMessageIDs   []int64
	Terms               []SimilarityTerm
	Limit               int
}

// SimilarMessageResult is a tenant-owned SQLite envelope paired with Bleve's
// sparse similarity score and concrete matched-term coverage.
type SimilarMessageResult struct {
	MessageID            int64
	Score                float64
	MatchedTerms         []string
	MatchedTermCount     int
	MatchedFields        []string
	WeightedTermCoverage float64
	Date                 time.Time
	From                 string
	ThreadKey            string
}

// MessageSimilarityHost is an optional, read-only capability for plugins that
// personalize a decision from tenant-owned similar mail.
type MessageSimilarityHost interface {
	BackendHost
	SimilarMessages(context.Context, int64, SimilarMessagesRequest) ([]SimilarMessageResult, error)
}

// RawMessageFetchHost is an optional, read-only capability for plugins that
// need a tenant-owned message's complete RFC822 source. Implementations must
// resolve both IDs through the user scope and must not persist or cache bytes
// fetched from the remote mailbox as a side effect of this call.
type RawMessageFetchHost interface {
	BackendHost
	FetchRawMessage(context.Context, int64, int64) ([]byte, error)
}

// MessageClassificationAttachment is bounded MIME metadata supplied to a
// classifier without persisting a second attachment body.
type MessageClassificationAttachment struct {
	Filename    string
	ContentType string
	Size        int64
}

// MessageClassificationInput is the in-memory, tenant-scoped payload exposed
// after a newly stored message has committed to the search index.
type MessageClassificationInput struct {
	UserID          int64
	MessageID       int64
	MessageIDHeader string
	AccountID       int64
	MailboxID       int64
	Date            time.Time
	From            string
	To              string
	CC              string
	Subject         string
	BodyText        string
	BodyTruncated   bool
	HasHTML         bool
	IsEncrypted     bool
	IsSigned        bool
	Attachments     []MessageClassificationAttachment
}

// MessageClassificationHost is the read-only host surface supplied to
// post-index classifiers.
type MessageClassificationHost interface {
	BackendHost
	MessageSimilarityHost
}

// MessageClassifier classifies a message after its Bleve batch commits. Errors
// are reported by the host but must not roll back already mirrored mail.
type MessageClassifier interface {
	BackendPlugin
	ClassifyMessage(context.Context, MessageClassificationHost, MessageClassificationInput) error
}

// MessageAnnotation is a generic, batch-hydrated decoration for message lists
// and detail views. Metadata must not contain raw message content or secrets.
type MessageAnnotation struct {
	PluginID string
	Kind     string
	Label    string
	Level    string
	Summary  string
	Metadata map[string]string
}

// MessageAnnotationProvider returns annotations only for the supplied
// tenant-owned message IDs. The host batches calls to avoid browser N+1 work.
type MessageAnnotationProvider interface {
	BackendPlugin
	MessageAnnotations(context.Context, BackendHost, int64, []int64) (map[int64][]MessageAnnotation, error)
}

// UserChangeHost is an optional web-host capability for plugin writes that
// invalidate message-list caches and should wake user event streams.
type UserChangeHost interface {
	BackendHost
	NotifyUserChanged(int64)
}

// StoredMessageHost exposes existing mail operations that stored-message hooks
// may apply after they have made a user-scoped decision.
type StoredMessageHost interface {
	BackendHost
	MatchMessageSearch(context.Context, int64, int64, string) (SearchMatchResult, error)
	StarMessage(context.Context, int64, int64, bool) error
	MoveMessage(context.Context, int64, int64, int64) error
	ForwardMessage(context.Context, int64, int64, string, []MailHeader) error
}

// APIHost adds the HTTP helpers needed by plugin-owned API routes.
type APIHost interface {
	BackendHost
	RequireAPIAuth(http.ResponseWriter, *http.Request) (CurrentUser, bool)
	LoginUserID(http.ResponseWriter, *http.Request, int64) error
	VerifyCSRF(http.ResponseWriter, *http.Request) bool
	DecodeJSON(http.ResponseWriter, *http.Request, any) bool
	WriteJSON(http.ResponseWriter, any)
	WriteAPIError(http.ResponseWriter, int, string)
	ServerError(http.ResponseWriter, error)
}

// ProtectedAPIHandler is a plugin-owned API route handler. The host has already
// matched an /api path for an enabled plugin and required a logged-in user;
// handlers still choose their own method and CSRF checks.
type ProtectedAPIHandler func(APIHost, string, http.ResponseWriter, *http.Request)

// ProtectedAPIRoute describes one protected /api route owned by a backend
// plugin. Path is relative to /api, for example "plugins/example/settings".
// Prefix matches both Path itself and slash-delimited descendants of Path.
type ProtectedAPIRoute struct {
	Path   string
	Prefix bool
	Handle ProtectedAPIHandler
}

type PublicAPIRoute struct {
	Path   string
	Prefix bool
	Handle ProtectedAPIHandler
}

// ProtectedAPIRouteHandle removes a route previously registered by a plugin.
type ProtectedAPIRouteHandle interface {
	Unregister()
}

// BackendStartHost is the host surface available while a backend plugin starts
// and stops.
type BackendStartHost interface {
	BackendHost
	RegisterProtectedAPI(string, ProtectedAPIRoute) (ProtectedAPIRouteHandle, error)
	RegisterPublicAPI(string, PublicAPIRoute) (ProtectedAPIRouteHandle, error)
}

// BackendPlugin is the minimal ABI every Go backend plugin exports.
type BackendPlugin interface {
	ID() string
	Start(BackendStartHost) error
	Stop(BackendStartHost) error
}

type AuthProvider struct {
	ID       string
	Name     string
	LoginURL string
}

type AuthProviderPlugin interface {
	BackendPlugin
	AuthProviders(context.Context, BackendHost) []AuthProvider
}

// NoopBackendPlugin satisfies the backend ABI for plugins that only need to
// register hooks and do not own lifecycle-managed routes or resources.
type NoopBackendPlugin struct {
	PluginID string
}

func (p NoopBackendPlugin) ID() string { return strings.TrimSpace(p.PluginID) }

func (p NoopBackendPlugin) Start(BackendStartHost) error { return nil }

func (p NoopBackendPlugin) Stop(BackendStartHost) error { return nil }

// ErrUnsupported lets a plugin decline a generic hook request without making the
// host treat that plugin as broken.
var ErrUnsupported = errors.New("plugin hook unsupported")

// MailIdentityContext is the host-provided subset of an outgoing identity that
// compose/security plugins may inspect.
type MailIdentityContext struct {
	ID                int64
	Email             string
	HeaderDisplayName string
	Preferences       map[string]string
}

// IdentitySecurityInfo is public metadata for an identity-owned security
// material, such as a public key and whether matching secret material exists.
type IdentitySecurityInfo struct {
	IdentityID     int64
	PublicMaterial string
	HasSecret      bool
	Metadata       map[string]string
}

// IdentitySecurityProvider augments compose identities with plugin-owned
// security metadata.
type IdentitySecurityProvider interface {
	BackendPlugin
	ComposeIdentitySecurity(context.Context, BackendHost, int64, MailIdentityContext) (IdentitySecurityInfo, error)
}

// IdentityAttachmentProvider creates plugin-owned compose attachments for an
// outgoing identity. Purpose names are host-defined, for example "public-key".
type IdentityAttachmentProvider interface {
	BackendPlugin
	ComposeIdentityAttachment(context.Context, BackendHost, int64, MailIdentityContext, string) (Attachment, error)
}

// MailHeader is an outbound RFC822 header requested by a plugin.
type MailHeader struct {
	Name  string
	Value string
}

// OutboundMailHeaderProvider lets plugins add RFC822 headers to composed mail.
type OutboundMailHeaderProvider interface {
	BackendPlugin
	OutboundMailHeaders(context.Context, BackendHost, int64, MailIdentityContext) ([]MailHeader, error)
}

// ComposeMessageBodyContext is the host-provided body state for an outgoing
// message before it is serialized by the SMTP layer.
type ComposeMessageBodyContext struct {
	MessageID string
	BodyText  string
	BodyHTML  string
	Metadata  map[string]string
}

// MIMEBodyOverride lets a plugin provide a fully prepared root MIME body for an
// outgoing message.
type MIMEBodyOverride struct {
	ContentType string
	Body        string
}

// ComposeMIMEBodyProvider lets plugins replace normal body serialization for
// plugin-owned MIME modes.
type ComposeMIMEBodyProvider interface {
	BackendPlugin
	ComposeMIMEBodyOverride(context.Context, BackendHost, int64, MailIdentityContext, ComposeMessageBodyContext) (*MIMEBodyOverride, error)
}

// MessageSecurityState is the protocol-neutral security metadata stored on a
// message row. The core owns the booleans; plugins own protocol detection.
type MessageSecurityState struct {
	Encrypted bool
	Signed    bool
}

// MessageBody is parsed renderable/indexable message content before or after a
// security plugin transforms it.
type MessageBody struct {
	Purpose string
	Text    string
	HTML    string
}

// MessageBodyTransform is returned by plugins that need to replace parsed body
// content or suppress attachment indexing for protected messages.
type MessageBodyTransform struct {
	Applied         bool
	Body            MessageBody
	DropAttachments bool
}

// MessageSecurityProvider lets plugins detect protected messages and adjust
// parsed/display body content without hardcoding a protocol in the host.
type MessageSecurityProvider interface {
	BackendPlugin
	DetectMessageSecurity(context.Context, BackendHost, int64, []byte, MessageBody) (MessageSecurityState, error)
	TransformMessageBody(context.Context, BackendHost, int64, []byte, MessageSecurityState, MessageBody) (MessageBodyTransform, error)
}

// IncomingMessageHook receives raw messages during import so plugins can index
// or discover metadata without the host knowing the protocol.
type IncomingMessageHook interface {
	BackendPlugin
	ImportIncomingMessage(context.Context, BackendHost, int64, []byte, string) error
}

// StoredMessageHook receives a full stored-message context after a message row
// and mailbox location have been created. Plugins should keep their own state
// user-scoped and return ErrUnsupported when no work is needed.
type StoredMessageHook interface {
	BackendPlugin
	ImportStoredMessage(context.Context, StoredMessageHost, StoredMessageContext) error
}

// MessageMoveContext is a bounded snapshot of a mirrored message immediately
// after its existing same-account IMAP move succeeds and before the host removes
// the old local row, search document, and blob. BodyPreview is empty for encrypted
// messages and must remain bounded by the host.
type MessageMoveContext struct {
	UserID                 int64
	MessageID              int64
	MessageIDHeader        string
	ThreadKey              string
	AccountID              int64
	SourceMailboxID        int64
	SourceMailboxName      string
	SourceMailboxRole      string
	DestinationMailboxID   int64
	DestinationMailboxName string
	DestinationMailboxRole string
	UID                    uint32
	Date                   time.Time
	InternalDate           time.Time
	From                   string
	To                     string
	CC                     string
	Subject                string
	BodyPreview            string
	BodyPreviewTruncated   bool
	HasHTML                bool
	IsRead                 bool
	IsStarred              bool
	HasAttachments         bool
	IsEncrypted            bool
	IsSigned               bool
}

// MessageMoveObserver receives advisory observations of successful moves that
// the core already performed. It cannot initiate a move through this interface.
// The host treats errors as best-effort failures and continues local cleanup.
type MessageMoveObserver interface {
	BackendPlugin
	ObserveMessageMove(context.Context, BackendHost, MessageMoveContext) error
}

// AttachmentInfo is the host-provided subset of a stored attachment used by
// plugin action providers.
type AttachmentInfo struct {
	ID          int64
	Filename    string
	ContentType string
	Size        int64
	Inline      bool
}

// AttachmentAction is a plugin-owned action hint for one displayed attachment.
type AttachmentAction struct {
	Kind     string
	Label    string
	Metadata map[string]string
}

// AttachmentActionProvider lets plugins expose generic actions for attachments
// without hardcoded filename/content-type checks in the host.
type AttachmentActionProvider interface {
	BackendPlugin
	AttachmentActions(context.Context, BackendHost, AttachmentInfo) []AttachmentAction
}

// Attachment is the plugin-neutral shape converted to smtpclient.Attachment by
// the web host.
type Attachment struct {
	Filename    string
	ContentType string
	Inline      bool
	Data        []byte
}

// BackendManager lazily loads backend plugin shared objects from manifests.
type BackendManager struct {
	root      string
	manifests []Manifest

	mu       sync.Mutex
	loaded   map[string]BackendPlugin
	failures map[string]string
}

// NewBackendManager creates a loader for runtime Go plugin binaries.
func NewBackendManager(root string, manifests []Manifest) *BackendManager {
	return &BackendManager{
		root:      strings.TrimSpace(root),
		manifests: manifests,
		loaded:    map[string]BackendPlugin{},
		failures:  map[string]string{},
	}
}

// Plugin loads and returns one backend plugin when its manifest declares a
// Go plugin binary.
func (m *BackendManager) Plugin(id string) (BackendPlugin, bool, error) {
	id = strings.TrimSpace(id)
	if m == nil || id == "" {
		return nil, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if plugin := m.loaded[id]; plugin != nil {
		return plugin, true, nil
	}
	if failure := strings.TrimSpace(m.failures[id]); failure != "" {
		return nil, true, fmt.Errorf("%s", failure)
	}
	manifest, ok := m.manifest(id)
	if !ok || manifest.Backend == nil {
		return nil, false, nil
	}
	if manifest.Backend.Kind != "go-plugin" {
		return nil, false, nil
	}
	if strings.TrimSpace(manifest.Backend.Binary) == "" {
		return nil, true, fmt.Errorf("backend plugin %s has no binary", id)
	}
	binary := filepath.Join(manifest.Dir, filepath.FromSlash(manifest.Backend.Binary))
	logging.Debugf("backend plugin module loading plugin_id=%s binary=%s", id, binary)
	opened, err := goplugin.Open(binary)
	if err != nil {
		m.failures[id] = err.Error()
		return nil, true, err
	}
	symbol, err := opened.Lookup("RolltopPlugin")
	if err != nil {
		m.failures[id] = err.Error()
		return nil, true, err
	}
	factory, ok := symbol.(func() BackendPlugin)
	if !ok {
		err := fmt.Errorf("backend plugin %s RolltopPlugin has incompatible type", id)
		m.failures[id] = err.Error()
		return nil, true, err
	}
	instance := factory()
	if instance == nil || instance.ID() != id {
		err := fmt.Errorf("backend plugin %s returned id %q", id, pluginID(instance))
		m.failures[id] = err.Error()
		return nil, true, err
	}
	delete(m.failures, id)
	m.loaded[id] = instance
	logging.Debugf("backend plugin module loaded plugin_id=%s hooks=%s", id, strings.Join(backendHookNames(instance), ","))
	return instance, true, nil
}

// PluginIDs returns the manifest order for plugins that declare a backend
// binary. The host uses this to discover generic hook implementers.
func (m *BackendManager) PluginIDs() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.manifests))
	for _, manifest := range m.manifests {
		if manifest.Backend != nil {
			out = append(out, manifest.ID)
		}
	}
	return out
}

// SetFailure records an operational plugin failure that should be shown to
// admins and retried after process restart.
func (m *BackendManager) SetFailure(id string, err error) {
	id = strings.TrimSpace(id)
	if m == nil || id == "" || err == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failures == nil {
		m.failures = map[string]string{}
	}
	m.failures[id] = err.Error()
}

// Failure returns the last load/start failure recorded for one backend plugin.
func (m *BackendManager) Failure(id string) string {
	id = strings.TrimSpace(id)
	if m == nil || id == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.TrimSpace(m.failures[id])
}

func (m *BackendManager) manifest(id string) (Manifest, bool) {
	for _, manifest := range m.manifests {
		if manifest.ID == id {
			return manifest, true
		}
	}
	return Manifest{}, false
}

func pluginID(plugin BackendPlugin) string {
	if plugin == nil {
		return ""
	}
	return plugin.ID()
}

func backendHookNames(plugin BackendPlugin) []string {
	if plugin == nil {
		return nil
	}
	names := []string{"lifecycle"}
	if _, ok := plugin.(IdentitySecurityProvider); ok {
		names = append(names, "identity-security")
	}
	if _, ok := plugin.(IdentityAttachmentProvider); ok {
		names = append(names, "identity-attachment")
	}
	if _, ok := plugin.(OutboundMailHeaderProvider); ok {
		names = append(names, "outbound-mail-headers")
	}
	if _, ok := plugin.(ComposeMIMEBodyProvider); ok {
		names = append(names, "compose-mime-body")
	}
	if _, ok := plugin.(MessageSecurityProvider); ok {
		names = append(names, "message-security")
	}
	if _, ok := plugin.(IncomingMessageHook); ok {
		names = append(names, "incoming-message")
	}
	if _, ok := plugin.(StoredMessageHook); ok {
		names = append(names, "stored-message")
	}
	if _, ok := plugin.(MessageMoveObserver); ok {
		names = append(names, "message-move-observer")
	}
	if _, ok := plugin.(MessageClassifier); ok {
		names = append(names, "message-classifier")
	}
	if _, ok := plugin.(MessageAnnotationProvider); ok {
		names = append(names, "message-annotations")
	}
	if _, ok := plugin.(AttachmentActionProvider); ok {
		names = append(names, "attachment-actions")
	}
	return names
}
