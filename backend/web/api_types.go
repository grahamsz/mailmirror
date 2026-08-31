// File overview: API response DTOs. These types define the JSON shapes returned to the Vite frontend.

package web

type apiUser struct {
	ID                     int64  `json:"id"`
	Email                  string `json:"email"`
	Name                   string `json:"name"`
	BackupEmail            string `json:"backup_email,omitempty"`
	IsAdmin                bool   `json:"is_admin"`
	DateLocale             string `json:"date_locale"`
	DateFormat             string `json:"date_format"`
	Theme                  string `json:"theme"`
	SearchPreset           string `json:"search_preset"`
	SearchRecencyBias      string `json:"search_recency_bias"`
	SearchFuzzy            string `json:"search_fuzzy"`
	SearchSenderBoost      bool   `json:"search_sender_boost"`
	SearchSenderHistory    string `json:"search_sender_history"`
	SearchContactBoost     string `json:"search_contact_boost"`
	SearchAttachmentWeight string `json:"search_attachment_weight"`
	SearchCompactSplitting bool   `json:"search_compact_splitting"`
}

type apiSwipeArchiveMailbox struct {
	AccountID int64 `json:"account_id"`
	MailboxID int64 `json:"mailbox_id"`
}

type apiSwipePreferences struct {
	LeftAction        string                   `json:"left_action"`
	LeftSnoozePreset  string                   `json:"left_snooze_preset"`
	RightAction       string                   `json:"right_action"`
	RightSnoozePreset string                   `json:"right_snooze_preset"`
	ArchiveMailboxes  []apiSwipeArchiveMailbox `json:"archive_mailboxes"`
}

type apiAuthProvider struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	LoginURL string `json:"login_url"`
}

type apiMailbox struct {
	ID                 int64  `json:"id"`
	AccountID          int64  `json:"account_id"`
	AccountEmail       string `json:"account_email"`
	AccountLabel       string `json:"account_label"`
	Name               string `json:"name"`
	MessageCount       int    `json:"message_count"`
	UnreadCount        int    `json:"unread_count"`
	SyncMode           string `json:"sync_mode"`
	Role               string `json:"role"`
	Icon               string `json:"icon"`
	ShowInSidebar      bool   `json:"show_in_sidebar"`
	ShowInAllMail      bool   `json:"show_in_all_mail"`
	IncludeInSearch    bool   `json:"include_in_search"`
	LastUID            uint32 `json:"last_uid"`
	RemoteMessageCount int    `json:"remote_message_count"`
	RemoteUnreadCount  int    `json:"remote_unread_count"`
	RemoteUIDNext      uint32 `json:"remote_uid_next"`
	SyncPercent        int    `json:"sync_percent"`
	LocalMessageCount  int    `json:"local_message_count"`
	CachedMessageCount int    `json:"cached_message_count"`
	LocalSyncPercent   int    `json:"local_sync_percent"`
	SearchIndexedCount *int   `json:"search_indexed_count,omitempty"`
	SearchIndexTotal   *int   `json:"search_index_total,omitempty"`
	SearchIndexPercent *int   `json:"search_index_percent,omitempty"`
	SearchIndexPurged  bool   `json:"search_index_purged"`
	SearchIndexKnown   bool   `json:"search_index_state_known"`
}

type apiAccount struct {
	ID                  int64  `json:"id"`
	Email               string `json:"email"`
	Label               string `json:"label"`
	Host                string `json:"host"`
	Port                int    `json:"port"`
	Username            string `json:"username"`
	UseTLS              bool   `json:"use_tls"`
	SMTPHost            string `json:"smtp_host"`
	SMTPPort            int    `json:"smtp_port"`
	SMTPUsername        string `json:"smtp_username"`
	SMTPUseTLS          bool   `json:"smtp_use_tls"`
	SMTPSameAsIMAP      bool   `json:"smtp_same_as_imap"`
	Mailbox             string `json:"mailbox"`
	SyncIntervalMinutes int    `json:"sync_interval_minutes"`
}

type apiSMTPAccount struct {
	ID       int64  `json:"id"`
	Label    string `json:"label"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	UseTLS   bool   `json:"use_tls"`
}

type apiMailIdentity struct {
	ID               int64  `json:"id"`
	ContactID        int64  `json:"contact_id"`
	ContactEmailID   int64  `json:"contact_email_id"`
	SMTPAccountID    int64  `json:"smtp_account_id"`
	IMAPAccountID    int64  `json:"imap_account_id"`
	SentMailboxID    int64  `json:"sent_mailbox_id"`
	DraftsMailboxID  int64  `json:"drafts_mailbox_id"`
	Email            string `json:"email"`
	DisplayName      string `json:"display_name"`
	Signature        string `json:"signature"`
	AutocryptEnabled bool   `json:"autocrypt_enabled"`
	IsPrimary        bool   `json:"is_primary"`
}

type apiMessage struct {
	ID             int64                  `json:"id"`
	AccountID      int64                  `json:"account_id"`
	MailboxID      int64                  `json:"mailbox_id"`
	Subject        string                 `json:"subject"`
	FromAddr       string                 `json:"from_addr"`
	ToAddr         string                 `json:"to_addr"`
	CCAddr         string                 `json:"cc_addr"`
	Date           string                 `json:"date"`
	DateShort      string                 `json:"date_short"`
	IsRead         bool                   `json:"is_read"`
	IsStarred      bool                   `json:"is_starred"`
	HasAttachments bool                   `json:"has_attachments"`
	IsEncrypted    bool                   `json:"is_encrypted"`
	IsSigned       bool                   `json:"is_signed"`
	OutboxID       int64                  `json:"outbox_id,omitempty"`
	DeliveryState  string                 `json:"delivery_state,omitempty"`
	FilingState    string                 `json:"filing_state,omitempty"`
	DeliveryError  string                 `json:"delivery_error,omitempty"`
	NeedsAttention bool                   `json:"needs_attention,omitempty"`
	Snippet        string                 `json:"snippet"`
	Annotations    []apiMessageAnnotation `json:"annotations,omitempty"`
}

type apiMessageAnnotation struct {
	PluginID string            `json:"plugin_id"`
	Kind     string            `json:"kind"`
	Label    string            `json:"label"`
	Level    string            `json:"level"`
	Summary  string            `json:"summary"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type apiConversation struct {
	Message                  apiMessage `json:"message"`
	MessageIDs               []int64    `json:"message_ids,omitempty"`
	MessageAccountIDs        []int64    `json:"message_account_ids,omitempty"`
	StarredMessageID         int64      `json:"starred_message_id"`
	Participants             string     `json:"participants"`
	RecipientParticipants    string     `json:"recipient_participants"`
	Count                    int        `json:"count"`
	IsRead                   bool       `json:"is_read"`
	HasAttachments           bool       `json:"has_attachments"`
	AttachmentNames          []string   `json:"attachment_names,omitempty"`
	AttachmentMatches        []string   `json:"attachment_matches,omitempty"`
	AttachmentContentMatched bool       `json:"attachment_content_matched,omitempty"`
	Snippet                  string     `json:"snippet"`
	MatchTerms               []string   `json:"match_terms,omitempty"`
	MatchQueryTerms          []string   `json:"match_query_terms,omitempty"`
	SnoozedUntil             string     `json:"snoozed_until,omitempty"`
}

type apiAttachment struct {
	ID                    int64                 `json:"id"`
	Filename              string                `json:"filename"`
	ContentType           string                `json:"content_type"`
	Size                  int64                 `json:"size"`
	DownloadURL           string                `json:"download_url"`
	Matched               bool                  `json:"matched,omitempty"`
	ContentMatched        bool                  `json:"content_matched,omitempty"`
	MatchTerms            []string              `json:"match_terms,omitempty"`
	Actions               []apiAttachmentAction `json:"actions,omitempty"`
	PGPPublicKeyCandidate bool                  `json:"pgp_public_key_candidate,omitempty"`
	Preview               *apiAttachmentPreview `json:"preview,omitempty"`
}

type apiAttachmentAction struct {
	PluginID string            `json:"plugin_id"`
	Kind     string            `json:"kind"`
	Label    string            `json:"label"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type apiAttachmentPreview struct {
	Available bool   `json:"available"`
	Kind      string `json:"kind"`
	URL       string `json:"url"`
	Status    string `json:"status"`
	PluginID  string `json:"plugin_id"`
}

type apiSenderVisual struct {
	PluginID string `json:"plugin_id"`
	Kind     string `json:"kind"`
	URL      string `json:"url"`
}

type apiContact struct {
	ID             int64               `json:"id"`
	NamePrefix     string              `json:"name_prefix"`
	GivenName      string              `json:"given_name"`
	AdditionalName string              `json:"additional_name"`
	FamilyName     string              `json:"family_name"`
	NameSuffix     string              `json:"name_suffix"`
	DisplayName    string              `json:"display_name"`
	Nickname       string              `json:"nickname"`
	Organization   string              `json:"organization"`
	Department     string              `json:"department"`
	JobTitle       string              `json:"job_title"`
	Birthday       string              `json:"birthday"`
	Notes          string              `json:"notes"`
	Categories     string              `json:"categories"`
	IsMe           bool                `json:"is_me"`
	IsPrimary      bool                `json:"is_primary"`
	Emails         []apiContactEmail   `json:"emails"`
	Phones         []apiContactPhone   `json:"phones"`
	Addresses      []apiContactAddress `json:"addresses"`
	URLs           []apiContactURL     `json:"urls"`
	IconURL        string              `json:"icon_url"`
}

type apiContactEmail struct {
	ID        int64  `json:"id,omitempty"`
	Label     string `json:"label"`
	Email     string `json:"email"`
	IsPrimary bool   `json:"is_primary"`
}

type apiContactPhone struct {
	ID        int64  `json:"id,omitempty"`
	Label     string `json:"label"`
	Number    string `json:"number"`
	IsPrimary bool   `json:"is_primary"`
}

type apiContactAddress struct {
	ID         int64  `json:"id,omitempty"`
	Label      string `json:"label"`
	Street     string `json:"street"`
	Locality   string `json:"locality"`
	Region     string `json:"region"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"`
	IsPrimary  bool   `json:"is_primary"`
}

type apiContactURL struct {
	ID        int64  `json:"id,omitempty"`
	Label     string `json:"label"`
	URL       string `json:"url"`
	IsPrimary bool   `json:"is_primary"`
}

type apiContactAutocomplete struct {
	ContactID int64  `json:"contact_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Label     string `json:"label"`
	IconURL   string `json:"icon_url"`
}

type apiContactInteraction struct {
	MessageID      int64  `json:"message_id"`
	Subject        string `json:"subject"`
	FromAddr       string `json:"from_addr"`
	Date           string `json:"date"`
	Direction      string `json:"direction"`
	HasAttachments bool   `json:"has_attachments"`
}

type apiComposeIdentity struct {
	ID                     int64  `json:"id"`
	SecurityIdentityID     int64  `json:"pgp_identity_id"`
	Label                  string `json:"label"`
	Email                  string `json:"email"`
	Header                 string `json:"header"`
	Signature              string `json:"signature"`
	IconURL                string `json:"icon_url"`
	IsPrimary              bool   `json:"is_primary"`
	AutocryptEnabled       bool   `json:"autocrypt_enabled"`
	HasSecurityPrivateKey  bool   `json:"has_pgp_private_key"`
	SecurityPublicMaterial string `json:"pgp_public_key_armored,omitempty"`
}

type apiThreadMessage struct {
	Message            apiMessage                    `json:"message"`
	Attachments        []apiAttachment               `json:"attachments"`
	HeaderDetails      []apiHeaderItem               `json:"header_details"`
	SecurityIndicators *apiMessageSecurityIndicators `json:"security_indicators,omitempty"`
	OneClickUnsub      bool                          `json:"one_click_unsubscribe"`
	OneClickSentAt     string                        `json:"one_click_unsubscribe_sent_at"`
	SenderName         string                        `json:"sender_name"`
	SenderEmail        string                        `json:"sender_email"`
	SenderInitial      string                        `json:"sender_initial"`
	SenderVisual       *apiSenderVisual              `json:"sender_visual,omitempty"`
	RecipientLine      string                        `json:"recipient_line"`
	Snippet            string                        `json:"snippet"`
	BodyDoc            string                        `json:"body_doc"`
	FullBodyDoc        string                        `json:"full_body_doc"`
	HasHiddenQuoted    bool                          `json:"has_hidden_quoted"`
	HasDisplayBody     bool                          `json:"has_display_body"`
	BodyPreviewOnly    bool                          `json:"body_preview_only"`
	HasRemoteImages    bool                          `json:"has_remote_images"`
	ImagesAllowed      bool                          `json:"images_allowed"`
	Expanded           bool                          `json:"expanded"`
	ReplySubject       string                        `json:"reply_subject"`
	CanReplyAll        bool                          `json:"can_reply_all"`
}

type apiMessageSecurityIndicators struct {
	ReportedAuthentication *apiReportedAuthentication `json:"reported_authentication,omitempty"`
	Signals                []apiMessageSecuritySignal `json:"signals,omitempty"`
}

type apiReportedAuthentication struct {
	SPF   *apiReportedAuthenticationResult `json:"spf,omitempty"`
	DKIM  *apiReportedAuthenticationResult `json:"dkim,omitempty"`
	DMARC *apiReportedAuthenticationResult `json:"dmarc,omitempty"`
}

type apiReportedAuthenticationResult struct {
	Result string `json:"result"`
	Source string `json:"source"`
}

type apiMessageSecuritySignal struct {
	Kind        string `json:"kind"`
	DisplayHost string `json:"display_host,omitempty"`
	TargetHost  string `json:"target_host,omitempty"`
	Scheme      string `json:"scheme,omitempty"`
}

type apiHeaderItem struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type apiSyncRun struct {
	ID                 int64  `json:"id"`
	AccountID          int64  `json:"account_id"`
	Status             string `json:"status"`
	StartedAt          string `json:"started_at"`
	FinishedAt         string `json:"finished_at"`
	UpdatedAt          string `json:"updated_at"`
	MessagesSeen       int    `json:"messages_seen"`
	MessagesStored     int    `json:"messages_stored"`
	MessagesSkipped    int    `json:"messages_skipped"`
	NewMessages        int    `json:"new_messages"`
	LatestNewFrom      string `json:"latest_new_from"`
	LatestNewSubject   string `json:"latest_new_subject"`
	LatestNewMessageID int64  `json:"latest_new_message_id"`
	MessagesTotal      int    `json:"messages_total"`
	MailboxesDone      int    `json:"mailboxes_done"`
	MailboxesTotal     int    `json:"mailboxes_total"`
	CurrentMailbox     string `json:"current_mailbox"`
	CurrentUID         uint32 `json:"current_uid"`
	Error              string `json:"error"`
}

type apiSyncFolder struct {
	Mailbox    apiMailbox  `json:"mailbox"`
	IsRunning  bool        `json:"is_running"`
	LastRun    *apiSyncRun `json:"last_run"`
	CanSyncNow bool        `json:"can_sync_now"`
}

// apiFolderProgress is the changing subset of a settings folder row. Keeping
// this separate avoids polling credentials, identities, contacts, and every
// other account setting while sync/index work is running.
type apiFolderProgress struct {
	MailboxID          int64  `json:"mailbox_id"`
	MessageCount       int    `json:"message_count"`
	UnreadCount        int    `json:"unread_count"`
	LastUID            uint32 `json:"last_uid"`
	RemoteMessageCount int    `json:"remote_message_count"`
	RemoteUnreadCount  int    `json:"remote_unread_count"`
	RemoteUIDNext      uint32 `json:"remote_uid_next"`
	SyncPercent        int    `json:"sync_percent"`
	LocalMessageCount  int    `json:"local_message_count"`
	CachedMessageCount int    `json:"cached_message_count"`
	LocalSyncPercent   int    `json:"local_sync_percent"`
	SearchIndexedCount *int   `json:"search_indexed_count,omitempty"`
	SearchIndexTotal   *int   `json:"search_index_total,omitempty"`
	SearchIndexPercent *int   `json:"search_index_percent,omitempty"`
	SearchIndexPurged  bool   `json:"search_index_purged"`
	SearchIndexKnown   bool   `json:"search_index_state_known"`
	IsRunning          bool   `json:"is_running"`
}

type apiPluginSetting struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Enabled          bool   `json:"enabled"`
	EnabledByDefault bool   `json:"enabled_by_default"`
	Heavy            bool   `json:"heavy"`
	Experimental     bool   `json:"experimental"`
	BackendError     string `json:"backend_error,omitempty"`
}

type apiThemeDefinition struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	PluginID string `json:"plugin_id,omitempty"`
	CSSURL   string `json:"css_url,omitempty"`
}

type apiFrontendPlugin struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	ModuleURL string `json:"module_url"`
	CSSURL    string `json:"css_url,omitempty"`
}
