// File overview: Wire-level TypeScript shapes returned by the Go API. These mirror JSON payloads,
// so field names intentionally stay snake_case instead of being adapted per component.

/** User mirrors the authenticated local account and display preferences returned by the API. */
export type User = {
  id: number;
  email: string;
  name: string;
  backup_email?: string;
  is_admin: boolean;
  date_locale: string;
  date_format: "mdy" | "dmy" | "ymd" | "locale" | string;
  theme: "classic" | "classic_dark" | "matrix" | string;
  search_preset: "strict" | "balanced" | "forgiving" | string;
  search_recency_bias: "none" | "light" | "normal" | "strong" | string;
  search_fuzzy: "off" | "balanced" | "forgiving" | string;
  search_sender_boost: boolean;
  search_sender_history: "none" | "light" | "normal" | "strong" | string;
  search_contact_boost: "none" | "light" | "normal" | "strong" | string;
  search_attachment_weight: "off" | "light" | "normal" | "strong" | string;
  search_compact_splitting: boolean;
};

export type SwipeAction = "trash" | "archive" | "snooze" | "mark_read" | "mark_unread";

export type SwipeSnoozePreset = "later_today" | "tomorrow" | "next_week";

export type SwipeArchiveMailbox = {
  account_id: number;
  mailbox_id: number;
};

export type SwipePreferences = {
  left_action: SwipeAction;
  left_snooze_preset: SwipeSnoozePreset;
  right_action: SwipeAction;
  right_snooze_preset: SwipeSnoozePreset;
  archive_mailboxes: SwipeArchiveMailbox[];
};

/** Mailbox mirrors a folder summary row including sync, visibility, and indexing counters. */
export type Mailbox = {
  id: number;
  account_id: number;
  account_email: string;
  account_label: string;
  name: string;
  message_count: number;
  unread_count: number;
  sync_mode: "auto" | "manual" | "never" | string;
  role: "inbox" | "sent" | "drafts" | "trash" | "junk" | "" | string;
  icon: string;
  show_in_sidebar: boolean;
  show_in_all_mail: boolean;
  include_in_search: boolean;
  last_uid: number;
  remote_message_count: number;
  remote_unread_count: number;
  remote_uid_next: number;
  sync_percent: number;
  local_message_count?: number;
  cached_message_count?: number;
  local_sync_percent?: number;
  search_indexed_count?: number;
  search_index_total?: number;
  search_index_percent?: number;
  search_index_purged?: boolean;
  search_index_state_known?: boolean;
};

/** Message is the API's compact message record used in lists and thread cards. */
export type Message = {
  id: number;
  account_id: number;
  mailbox_id: number;
  subject: string;
  from_addr: string;
  to_addr: string;
  cc_addr: string;
  date: string;
  date_short: string;
  is_read: boolean;
  is_starred: boolean;
  has_attachments: boolean;
  is_encrypted: boolean;
  is_signed: boolean;
  outbox_id?: number;
  delivery_state?: string;
  filing_state?: string;
  delivery_error?: string;
  needs_attention?: boolean;
  snippet: string;
  annotations?: MessageAnnotation[];
};

/** OutboxSummary is durable send state kept in bootstrap and chrome events. */
export type OutboxSummary = {
  active: number;
  needs_attention: number;
  latest_id: number;
};

/** OutboxJob describes one queued send and its independent SMTP/Sent-copy states. */
export type OutboxJob = {
  id: number;
  message_id: number;
  subject: string;
  delivery_state: string;
  filing_state: string;
  attempt_count: number;
  filing_attempt_count: number;
  next_attempt_at: string;
  last_error: string;
  needs_attention: boolean;
  can_cancel: boolean;
  can_retry: boolean;
  retry_may_duplicate: boolean;
  smtp_accepted_at: string;
  completed_at: string;
  created_at: string;
  updated_at: string;
  raw_size: number;
  appended_uid: number;
  appended_uid_validity: number;
};

export type OutboxResponse = {
  jobs: OutboxJob[];
  summary: OutboxSummary;
};

/** MessageAnnotation is compact, non-sensitive metadata supplied by an enabled backend plugin. */
export type MessageAnnotation = {
  plugin_id: string;
  kind: string;
  label: string;
  level: string;
  summary: string;
  metadata?: Record<string, string>;
};

/** Conversation is a list/search row grouped around the latest visible thread message. */
export type Conversation = {
  message: Message;
  message_ids?: number[];
  message_account_ids?: number[];
  starred_message_id: number;
  participants: string;
  recipient_participants: string;
  count: number;
  is_read: boolean;
  has_attachments: boolean;
  attachment_names?: string[];
  attachment_matches?: string[];
  attachment_content_matched?: boolean;
  snippet: string;
  match_terms?: string[];
  match_query_terms?: string[];
  snoozed_until?: string;
};

/** MailListResponse is one paged conversation list returned by /api/mail. */
export type MailListResponse = {
  conversations: Conversation[];
  page: number;
  has_prev: boolean;
  has_next: boolean;
};

export type MessageSnooze = {
  id: number;
  message_id: number;
  snoozed_until: string;
  created_at: string;
  updated_at: string;
};

/** Attachment is message attachment metadata plus optional preview/search match details. */
export type Attachment = {
  id: number;
  filename: string;
  content_type: string;
  size: number;
  download_url: string;
  matched?: boolean;
  content_matched?: boolean;
  match_terms?: string[];
  actions?: AttachmentAction[];
  pgp_public_key_candidate?: boolean;
  preview?: AttachmentPreview;
};

/** AttachmentAction is a plugin-provided operation available for an attachment. */
export type AttachmentAction = {
  plugin_id: string;
  kind: string;
  label: string;
  metadata?: Record<string, string>;
};

/** AttachmentPreview describes a plugin-provided in-browser preview option. */
export type AttachmentPreview = {
  available: boolean;
  kind: string;
  url: string;
  status: string;
  plugin_id: string;
};

/** HeaderDetail is one expanded message header row shown in thread details. */
export type HeaderDetail = {
  label: string;
  value: string;
};

/** AuthenticationResult is a receiver-reported header result, not a Rolltop verification. */
export type AuthenticationResult = {
  result: string;
  source: "authentication-results" | "received-spf";
};

export type MessageSecuritySignal = {
  kind: "sender_display_address_mismatch" | "reply_to_domain_mismatch" | "link_destination_mismatch" | "risky_link_scheme";
  display_host?: string;
  target_host?: string;
  scheme?: string;
};

export type MessageSecurityIndicators = {
  reported_authentication?: {
    spf?: AuthenticationResult;
    dkim?: AuthenticationResult;
    dmarc?: AuthenticationResult;
  };
  signals?: MessageSecuritySignal[];
};

/** ThreadMessage is the render-ready payload for one message inside a conversation. */
export type ThreadMessage = {
  message: Message;
  attachments: Attachment[];
  header_details: HeaderDetail[];
  security_indicators?: MessageSecurityIndicators;
  one_click_unsubscribe: boolean;
  one_click_unsubscribe_sent_at: string;
  sender_name: string;
  sender_email: string;
  sender_initial: string;
  sender_visual?: SenderVisual;
  recipient_line: string;
  snippet: string;
  body_doc: string;
  full_body_doc: string;
  has_hidden_quoted: boolean;
  has_display_body: boolean;
  body_preview_only: boolean;
  has_remote_images: boolean;
  images_allowed: boolean;
  expanded: boolean;
  reply_subject: string;
  can_reply_all: boolean;
};


/** MessageOriginalSource is the raw RFC822 source fetched on demand for View Original. */
export type MessageOriginalSource = {
  filename: string;
  source: string;
};

/** SearchExplanation describes one on-demand Bleve scoring explanation for a message. */
export type SearchExplanation = {
  matched: boolean;
  query: string;
  reason?: string;
  score?: number;
  message_id?: number;
  requested_message_id?: number;
  terms?: string[];
  query_terms?: string[];
  fields?: string[];
  field_matches?: SearchFieldMatch[];
  term_contributions?: SearchTermContribution[];
  boosts?: SearchBoost[];
  raw?: ScoreExplanationNode;
};

export type SearchFieldMatch = {
  field: string;
  terms: string[];
};

export type SearchTermContribution = {
  field: string;
  section: string;
  term: string;
  query_term: string;
  score: number;
  term_frequency?: number;
  field_norm?: number;
  idf?: number;
  query_weight?: number;
  boost?: number;
  query_norm?: number;
};

export type SearchBoost = {
  kind: string;
  label: string;
  description: string;
  value?: string;
  boost?: number;
};

export type ScoreExplanationNode = {
  value?: number;
  message: string;
  children?: ScoreExplanationNode[];
};

/** SenderVisual identifies a plugin-provided sender avatar or brand image. */
export type SenderVisual = {
  plugin_id: string;
  kind: string;
  url: string;
};

export type ThemeDefinition = {
  id: string;
  name: string;
  plugin_id?: string;
  css_url?: string;
};

export type FrontendPluginDefinition = {
  id: string;
  name: string;
  version?: string;
  module_url: string;
  css_url?: string;
};

/** ContactEmail is one editable email row on a contact. */
export type ContactEmail = {
  id?: number;
  label: string;
  email: string;
  is_primary: boolean;
};

export type ContactPGPKey = {
  id?: number;
  contact_id?: number;
  email: string;
  label: string;
  fingerprint: string;
  key_id: string;
  user_ids: string;
  public_key_armored: string;
  source_kind?: string;
  source_detail?: string;
  is_preferred: boolean;
};

/** ContactPhone is one editable phone row on a contact. */
export type ContactPhone = {
  id?: number;
  label: string;
  number: string;
  is_primary: boolean;
};

/** ContactAddress is one editable postal address row on a contact. */
export type ContactAddress = {
  id?: number;
  label: string;
  street: string;
  locality: string;
  region: string;
  postal_code: string;
  country: string;
  is_primary: boolean;
};

/** ContactURL is one editable URL row on a contact. */
export type ContactURL = {
  id?: number;
  label: string;
  url: string;
  is_primary: boolean;
};

/** Contact is the API address-book shape including nested detail rows and icon URL. */
export type Contact = {
  id: number;
  name_prefix: string;
  given_name: string;
  additional_name: string;
  family_name: string;
  name_suffix: string;
  display_name: string;
  nickname: string;
  organization: string;
  department: string;
  job_title: string;
  birthday: string;
  notes: string;
  categories: string;
  is_me: boolean;
  is_primary: boolean;
  emails: ContactEmail[];
  phones: ContactPhone[];
  addresses: ContactAddress[];
  urls: ContactURL[];
  pgp_keys?: ContactPGPKey[];
  icon_url: string;
};

/** ContactInteraction is compact recent-mail context shown on a contact profile. */
export type ContactInteraction = {
  message_id: number;
  subject: string;
  from_addr: string;
  date: string;
  direction: "received" | "sent" | string;
  has_attachments: boolean;
};

/** ContactAutocomplete is a flattened recipient suggestion for compose. */
export type ContactAutocomplete = {
  contact_id: number;
  name: string;
  email: string;
  label: string;
  icon_url: string;
};

/** ComposeIdentity is a selectable From identity returned for compose/reply forms. */
export type ComposeIdentity = {
  id: number;
  pgp_identity_id: number;
  label: string;
  email: string;
  header: string;
  signature: string;
  icon_url: string;
  is_primary: boolean;
  autocrypt_enabled: boolean;
  has_pgp_private_key?: boolean;
  pgp_public_key_armored?: string;
};

/** PluginSetting is the admin-visible enablement state for one plugin. */
export type PluginSetting = {
  id: string;
  name: string;
  description: string;
  enabled: boolean;
  enabled_by_default: boolean;
  heavy: boolean;
  experimental?: boolean;
  backend_error?: string;
};

/** SyncRun is the API progress/status shape for sync and maintenance jobs. */
export type SyncRun = {
  id: number;
  account_id: number;
  status: string;
  started_at: string;
  finished_at: string;
  updated_at: string;
  messages_seen: number;
  messages_stored: number;
  messages_skipped: number;
  new_messages: number;
  latest_new_from: string;
  latest_new_subject: string;
  latest_new_message_id: number;
  messages_total: number;
  mailboxes_done: number;
  mailboxes_total: number;
  current_mailbox: string;
  current_uid: number;
  error: string;
};

export type SyncRunLiveDetail = {
  active: boolean;
  cancellable: boolean;
  phase: string;
  detail: string;
  phase_started_at: string;
};

/** SyncFolder combines mailbox settings with current/last sync run information. */
export type SyncFolder = {
  mailbox: Mailbox;
  is_running: boolean;
  last_run: SyncRun | null;
  can_sync_now: boolean;
};

/** FolderProgress is the small, frequently refreshed subset of a settings row. */
export type FolderProgress = {
  mailbox_id: number;
  message_count: number;
  unread_count: number;
  last_uid: number;
  remote_message_count: number;
  remote_unread_count: number;
  remote_uid_next: number;
  sync_percent: number;
  local_message_count: number;
  cached_message_count: number;
  local_sync_percent: number;
  search_indexed_count?: number;
  search_index_total?: number;
  search_index_percent?: number;
  search_index_purged: boolean;
  search_index_state_known: boolean;
  is_running: boolean;
};

/** StorageStats is a flexible storage usage payload keyed by backend stat names. */
export type StorageStats = Record<string, unknown>;

/** Bootstrap is the first API payload that establishes auth, chrome, CSRF, and plugin state. */
export type Bootstrap = {
  users_exist: boolean;
  csrf: string;
  user: User | null;
  mailboxes: Mailbox[];
  latest_sync_run?: SyncRun | null;
  active_sync_runs?: SyncRun[];
  sync_running?: boolean;
  mail_generation?: number;
  swipe_preferences?: SwipePreferences;
  account_needs_password?: boolean;
  account_notice?: string;
  enabled_plugins?: string[];
  auth_providers?: AuthProvider[];
  available_themes?: ThemeDefinition[];
  frontend_plugins?: FrontendPluginDefinition[];
  server_started_at?: string;
  server_uptime_seconds?: number;
  build_version?: string;
  build_date?: string;
  build_label?: string;
  build_commit?: string;
  public_site_url?: string;
  outbox?: OutboxSummary;
};

export type AuthProvider = {
  id: string;
  name: string;
  login_url: string;
};

/** ChromeEvent is the SSE payload used to refresh folders and sync status. */
export type ChromeEvent = {
  mailboxes: Mailbox[];
  latest_sync_run: SyncRun | null;
  active_sync_runs: SyncRun[];
  sync_running: boolean;
  mail_generation: number;
  swipe_preferences?: SwipePreferences;
  server_started_at?: string;
  server_uptime_seconds?: number;
  build_version?: string;
  build_date?: string;
  build_label?: string;
  build_commit?: string;
  public_site_url?: string;
  outbox?: OutboxSummary;
};

/** ComposeExistingAttachment is an already-stored attachment that compose can reuse without a new upload. */
export type ComposeExistingAttachment = Pick<Attachment, "id" | "filename" | "content_type" | "size" | "download_url">;

/** ComposeAttachmentUpload couples a File with metadata sent in multipart compose requests. */
export type ComposeAttachmentUpload = {
  field: string;
  filename: string;
  content_type: string;
  content_id: string;
  inline: boolean;
  size: number;
  file: File;
};

/** ComposeForm is the editable compose/reply/forward payload exchanged with the API. */
export type IdentityPGPPrivateKey = {
  id?: number;
  identity_id: number;
  label: string;
  fingerprint: string;
  key_id: string;
  user_ids: string;
  public_key_armored: string;
  private_key_armored?: string;
  private_key_storage?: "server" | "browser" | string;
  revocation_certificate?: string;
  is_active_signing: boolean;
  is_active_encryption: boolean;
  is_decrypt_only: boolean;
  created_at?: string;
  updated_at?: string;
};

export type ComposeForm = {
  submission_key?: string;
  to: string;
  cc: string;
  bcc: string;
  subject: string;
  body: string;
  body_html: string;
  draft_message_id: number;
  in_reply_to_id: number;
  from_identity_id: number;
  available_attachments?: ComposeExistingAttachment[];
  include_attachment_ids?: number[];
  forward_attachment_message_id?: number;
  forward_attachment?: ComposeExistingAttachment;
  pgp_encrypted?: boolean;
  pgp_signed?: boolean;
  pgp_mime?: boolean;
  pgp_signature?: string;
  attach_public_key?: boolean;
};

/** Account is the IMAP account settings shape used by the settings page. */
export type Account = {
  id: number;
  email: string;
  label: string;
  host: string;
  port: number;
  username: string;
  use_tls: boolean;
  smtp_host: string;
  smtp_port: number;
  smtp_username: string;
  smtp_use_tls: boolean;
  smtp_same_as_imap: boolean;
  mailbox: string;
  sync_interval_minutes: number;
};

export type AccountPurgeEstimate = {
  account_id: number;
  account_name: string;
  account_email: string;
  mailbox_count: number;
  message_count: number;
  blob_count: number;
  blob_bytes: number;
  search_index_count: number;
};

/** SMTPAccount is the outgoing server settings shape used by the settings page. */
export type SMTPAccount = {
  id: number;
  label: string;
  host: string;
  port: number;
  username: string;
  use_tls: boolean;
};

/** MailIdentity is the settings shape for a Me-contact-backed outgoing identity. */
export type MailIdentity = {
  id: number;
  contact_id: number;
  contact_email_id: number;
  smtp_account_id: number;
  imap_account_id: number;
  sent_mailbox_id: number;
  drafts_mailbox_id: number;
  email: string;
  display_name: string;
  signature: string;
  autocrypt_enabled: boolean;
  is_primary: boolean;
};
