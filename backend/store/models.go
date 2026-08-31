// File overview: Shared store model types. These structs describe users, accounts, mailboxes, messages, attachments, contacts, identities, sync runs, and blob records exchanged across backend packages.

package store

import "time"

// User is the system-level local account record mirrored into each user database for joins/preferences.
type User struct {
	ID                     int64
	Email                  string
	Name                   string
	BackupEmail            string
	PasswordHash           string
	IsAdmin                bool
	DateLocale             string
	DateFormat             string
	Theme                  string
	SearchPreset           string
	SearchRecencyBias      string
	SearchFuzzy            string
	SearchSenderBoost      bool
	SearchSenderHistory    string
	SearchContactBoost     string
	SearchAttachmentWeight string
	SearchCompactSplitting bool
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Session is a hashed browser login token with an expiry time.
type Session struct {
	ID         int64
	UserID     int64
	TokenHash  string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// MailAccount is one IMAP server account plus cached SMTP defaults for a user.
type MailAccount struct {
	ID                    int64
	UserID                int64
	Email                 string
	Label                 string
	Host                  string
	Port                  int
	Username              string
	EncryptedPassword     string
	UseTLS                bool
	SMTPHost              string
	SMTPPort              int
	SMTPUsername          string
	EncryptedSMTPPassword string
	SMTPUseTLS            bool
	Mailbox               string
	SyncIntervalMinutes   int
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// SMTPAccount is one outgoing server that identities can use for composed mail.
type SMTPAccount struct {
	ID                int64
	UserID            int64
	Label             string
	Host              string
	Port              int
	Username          string
	EncryptedPassword string
	UseTLS            bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// MailIdentity links a Me contact email to an SMTP server, display name, and signature.
type MailIdentity struct {
	ID               int64
	UserID           int64
	ContactID        int64
	ContactEmailID   int64
	SMTPAccountID    int64
	IMAPAccountID    int64
	SentMailboxID    int64
	DraftsMailboxID  int64
	Email            string
	DisplayName      string
	Signature        string
	AutocryptEnabled bool
	IsPrimary        bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// IdentityPGPPrivateKey is passphrase-protected OpenPGP private key material for one outgoing identity.
type IdentityPGPPrivateKey struct {
	ID                    int64
	UserID                int64
	IdentityID            int64
	Label                 string
	Fingerprint           string
	KeyID                 string
	UserIDs               string
	PublicKeyArmored      string
	EncryptedPrivateKey   string
	PrivateKeyArmored     string
	PrivateKeyStorage     string
	RevocationCertificate string
	IsActiveSigning       bool
	IsActiveEncryption    bool
	IsDecryptOnly         bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// Mailbox is the local representation of one remote IMAP folder and its sync/search settings.
type Mailbox struct {
	ID                 int64
	UserID             int64
	AccountID          int64
	Name               string
	SyncMode           string
	Role               string
	Icon               string
	ShowInSidebar      bool
	ShowInAllMail      bool
	IncludeInSearch    bool
	UIDValidity        int64
	LastUID            uint32
	RemoteMessageCount int
	RemoteUnreadCount  int
	RemoteUIDNext      uint32
	StatusCheckedAt    time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// MailboxSummary is the mailbox plus derived counters used by chrome and settings UI.
type MailboxSummary struct {
	Mailbox
	AccountEmail       string
	AccountLabel       string
	MessageCount       int
	UnreadCount        int
	LocalMessageCount  int
	CachedMessageCount int
	LocalSyncPercent   int
	SyncPercent        int
	SearchIndexedCount *int
	SearchIndexTotal   *int
	SearchIndexPercent *int
	SearchIndexPurged  bool
	SearchIndexKnown   bool
}

// AccountPurgeEstimate is the local data footprint shown before deleting an
// IMAP account from Rolltop. It deliberately describes only local SQLite,
// blob, and search-index data; no remote IMAP message is deleted.
type AccountPurgeEstimate struct {
	Account      MailAccount
	MailboxCount int
	MessageCount int
	BlobCount    int
	BlobBytes    int64
}

// MailboxSettings is the editable subset of mailbox configuration saved from settings.
type MailboxSettings struct {
	SyncMode        string
	Role            string
	Icon            string
	ShowInSidebar   bool
	ShowInAllMail   bool
	IncludeInSearch bool
}

// MessageRecord is the canonical local metadata/body row for one mirrored message.
type MessageRecord struct {
	ID                  int64
	UserID              int64
	AccountID           int64
	MailboxID           int64
	BlobID              int64
	MessageIDHeader     string
	InReplyTo           string
	ReferencesHeader    string
	ThreadKey           string
	Subject             string
	LanguageCode        string
	FromAddr            string
	ToAddr              string
	CCAddr              string
	Date                time.Time
	InternalDate        time.Time
	UID                 uint32
	Size                int64
	BlobPath            string
	BodyText            string
	BodyHTML            string
	IsRead              bool
	ReadSyncPending     bool
	IsStarred           bool
	StarSyncPending     bool
	HasAttachments      bool
	IsEncrypted         bool
	IsSigned            bool
	AttachmentIndexedAt time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// OutboxJob is one durable, user-owned outbound message. SMTP delivery and
// remote Sent-folder filing are intentionally tracked independently so a
// filing failure can never cause an accepted message to be transmitted again.
type OutboxJob struct {
	ID                  int64
	UserID              int64
	SubmissionKey       string
	SMTPAccountID       int64
	IMAPAccountID       int64
	SentMailboxID       int64
	OptimisticMessageID int64
	EnvelopeFrom        string
	RecipientsJSON      string
	MessageIDHeader     string
	BlobID              int64
	BlobPath            string
	RawSHA256           string
	RawSize             int64
	DeliveryState       string
	FilingState         string
	AttemptCount        int
	FilingAttemptCount  int
	NextAttemptAt       time.Time
	LeaseOwner          string
	LeaseExpiresAt      time.Time
	AppendUIDValidity   uint32
	AppendUIDNext       uint32
	AppendedUID         uint32
	AppendedUIDValidity uint32
	LastErrorKind       string
	LastError           string
	AttentionAt         time.Time
	AcknowledgedAt      time.Time
	SMTPAcceptedAt      time.Time
	CompletedAt         time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// OutboxSummary is the compact durable state included in bootstrap and SSE
// responses. NeedsAttention is never inferred from a transient in-memory event.
type OutboxSummary struct {
	Active         int
	NeedsAttention int
	LatestID       int64
}

// OutboxMessageState decorates an optimistic Sent message without putting
// queue mechanics into the canonical message row API.
type OutboxMessageState struct {
	OutboxID      int64
	MessageID     int64
	DeliveryState string
	FilingState   string
	LastError     string
	Attention     bool
}

// MessageSimilarityCandidate is the minimal SQLite envelope needed to validate
// and hydrate a Bleve similarity hit without loading stored message bodies.
type MessageSimilarityCandidate struct {
	ID        int64
	UserID    int64
	ThreadKey string
	Date      time.Time
	FromAddr  string
	IsRead    bool
}

// SenderReadStat is a ranking hint derived from senders whose messages the user reads.
type SenderReadStat struct {
	Sender     string
	ReadCount  int
	TotalCount int
	Boost      float64
}

// Attachment is stored metadata for a MIME file part associated with a message.
type Attachment struct {
	ID          int64
	UserID      int64
	MessageID   int64
	BlobID      int64
	Filename    string
	ContentType string
	ContentID   string
	IsInline    bool
	Size        int64
	BlobPath    string
	CreatedAt   time.Time
}

// BlobRecord is SQLite metadata for a file in the user-scoped blob store.
type BlobRecord struct {
	ID        int64
	UserID    int64
	Kind      string
	Path      string
	SHA256    string
	Size      int64
	CreatedAt time.Time
}

// RemoteImageCache stores user-scoped metadata for warmed remote email images.
type RemoteImageCache struct {
	ID          int64
	UserID      int64
	URLHash     string
	URL         string
	BlobID      int64
	BlobPath    string
	ContentType string
	Size        int64
	Status      string
	Error       string
	FetchedAt   time.Time
	ExpiresAt   time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Contact is a user-owned address-book entry with nested email/phone/address/URL details.
type Contact struct {
	ID             int64
	UserID         int64
	NamePrefix     string
	GivenName      string
	AdditionalName string
	FamilyName     string
	NameSuffix     string
	DisplayName    string
	Nickname       string
	Organization   string
	Department     string
	JobTitle       string
	Birthday       string
	Notes          string
	Categories     string
	IsMe           bool
	IsPrimary      bool
	Emails         []ContactEmail
	Phones         []ContactPhone
	Addresses      []ContactAddress
	URLs           []ContactURL
	PGPKeys        []ContactPGPPublicKey
	Icon           *ContactIcon
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ContactEmail is one email address attached to a contact and optionally marked primary.
type ContactEmail struct {
	ID              int64
	UserID          int64
	ContactID       int64
	Label           string
	Email           string
	NormalizedEmail string
	IsPrimary       bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ContactPhone is one phone number attached to a contact.
type ContactPhone struct {
	ID        int64
	UserID    int64
	ContactID int64
	Label     string
	Number    string
	IsPrimary bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ContactAddress is one postal address attached to a contact.
type ContactAddress struct {
	ID         int64
	UserID     int64
	ContactID  int64
	Label      string
	Street     string
	Locality   string
	Region     string
	PostalCode string
	Country    string
	IsPrimary  bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ContactURL is one web URL attached to a contact.
type ContactURL struct {
	ID        int64
	UserID    int64
	ContactID int64
	Label     string
	URL       string
	IsPrimary bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ContactPGPPublicKey is one public key associated with a contact email address.
type ContactPGPPublicKey struct {
	ID               int64
	UserID           int64
	ContactID        int64
	Email            string
	NormalizedEmail  string
	Label            string
	Fingerprint      string
	KeyID            string
	UserIDs          string
	PublicKeyArmored string
	SourceKind       string
	SourceDetail     string
	IsPreferred      bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ContactIcon is the blob-backed icon metadata for a contact.
type ContactIcon struct {
	ID          int64
	UserID      int64
	ContactID   int64
	BlobID      int64
	ContentType string
	Filename    string
	Size        int64
	BlobPath    string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ContactAutocomplete is a flattened contact email choice for compose recipient search.
type ContactAutocomplete struct {
	ContactID int64
	Name      string
	Email     string
	Label     string
	IconURL   string
}

// SyncRun is the persisted progress/status row for a sync or maintenance operation.
type SyncRun struct {
	ID                 int64
	UserID             int64
	AccountID          int64
	Status             string
	StartedAt          time.Time
	FinishedAt         time.Time
	UpdatedAt          time.Time
	MessagesSeen       int
	MessagesStored     int
	MessagesSkipped    int
	NewMessages        int
	LatestNewFrom      string
	LatestNewSubject   string
	LatestNewMessageID int64
	MessagesTotal      int
	MailboxesDone      int
	MailboxesTotal     int
	CurrentMailbox     string
	CurrentUID         uint32
	Error              string
}

// WebPushSubscription stores one browser or native Web Push endpoint for a user.
type WebPushSubscription struct {
	ID                        int64
	UserID                    int64
	Endpoint                  string
	P256DH                    string
	Auth                      string
	UserAgent                 string
	LastNewMailEventID        int64
	LastSnoozeReminderEventID int64
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	LastSeenAt                time.Time
}

// MessageSnooze is local-only reminder state for one user-owned conversation.
type MessageSnooze struct {
	ID           int64
	UserID       int64
	MessageID    int64
	ThreadKey    string
	Generation   int64
	SnoozedUntil time.Time
	RemindedAt   time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SnoozedMessage pairs a snooze with the user-owned message that anchors it.
type SnoozedMessage struct {
	Snooze  MessageSnooze
	Message MessageRecord
}

// SnoozeReminderEvent is a minimal, durable envelope for reminder clients.
type SnoozeReminderEvent struct {
	ID               int64
	UserID           int64
	MessageID        int64
	SnoozeGeneration int64
	FromAddr         string
	Subject          string
	DueAt            time.Time
	CreatedAt        time.Time
}
