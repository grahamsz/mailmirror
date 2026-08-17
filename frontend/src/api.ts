// File overview: Typed browser API client. It centralizes JSON parsing, CSRF-bearing writes,
// ETag-aware GET caching, multipart compose uploads, and endpoint shapes used by views.

import type {
  Account,
  AccountPurgeEstimate,
  Bootstrap,
  Contact,
  ContactAutocomplete,
  ComposeAttachmentUpload,
  ComposeForm,
  ComposeIdentity,
  Conversation,
  FolderProgress,
  MailListResponse,
  Mailbox,
  MailIdentity,
  MessageSnooze,
  MessageOriginalSource,
  PluginSetting,
  SMTPAccount,
  SearchExplanation,
  StorageStats,
  SwipePreferences,
  SyncFolder,
  SyncRun,
  SyncRunLiveDetail,
  ThreadMessage,
  User
} from "./types";
import { clearMailSnapshots, clearOtherMailSnapshots, loadMailSnapshot, saveMailSnapshot } from "./lib/mailSnapshot";
import { clearOtherCollapsedAccounts } from "./lib/sidebarLocal";

/** Error thrown for non-2xx API responses after the JSON error payload is decoded. */
export class ApiError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

// All API helpers flow through parse so callers see typed payloads on success
// and a consistent ApiError on backend validation/session failures.
async function parse<T>(res: Response): Promise<T> {
  const text = await res.text();
  let data: Record<string, unknown> = {};
  if (text) {
    try {
      data = JSON.parse(text);
    } catch (err) {
      if (!res.ok) {
        throw new ApiError(res.status, text || res.statusText);
      }
      throw err;
    }
  }
  if (!res.ok) {
    throw new ApiError(res.status, typeof data.error === "string" ? data.error : res.statusText);
  }
  return data as T;
}

type SnoozeListResponse = MailListResponse & { snoozes: MessageSnooze[] };

const getCache = new Map<string, { etag: string; data: unknown }>();
const getInflight = new Map<string, Promise<unknown>>();
const mailCacheEpochs = new Map<number, number>();
type MutationRequestOptions = { keepalive?: boolean };

async function fetchGET(url: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(url, init);
  } catch (err) {
    await new Promise((resolve) => window.setTimeout(resolve, 250));
    try {
      return await fetch(url, init);
    } catch {
      throw err;
    }
  }
}

/**
 * GET JSON with lightweight ETag revalidation. Callers can supply an explicit
 * scope key when the same URL may return data for different authenticated users.
 */
export async function getJSON<T>(url: string, cacheKey = url): Promise<T> {
  const inflight = getInflight.get(cacheKey);
  if (inflight) return inflight as Promise<T>;

  const request = (async () => {
    const headers: Record<string, string> = { Accept: "application/json" };
    const cached = getCache.get(cacheKey);
    if (cached?.etag) headers["If-None-Match"] = cached.etag;
    const res = await fetchGET(url, { headers });
    if (res.status === 304 && cached) return cached.data as T;
    const data = await parse<T>(res);
    const etag = res.headers.get("ETag") || "";
    if (etag) getCache.set(cacheKey, { etag, data });
    else getCache.delete(cacheKey);
    return data;
  })();

  getInflight.set(cacheKey, request);
  try {
    return await request;
  } finally {
    if (getInflight.get(cacheKey) === request) getInflight.delete(cacheKey);
  }
}

/** POST JSON to a mutating endpoint with the current CSRF token. */
export async function postJSON<T>(url: string, csrf: string, body: unknown = {}, options: MutationRequestOptions = {}): Promise<T> {
  return parse<T>(
    await fetch(url, {
      method: "POST",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-CSRF-Token": csrf
      },
      body: JSON.stringify(body),
      ...(options.keepalive ? { keepalive: true } : {})
    })
  );
}

/** PUT JSON to a mutating endpoint with the current CSRF token. */
export async function putJSON<T>(url: string, csrf: string, body: unknown = {}, options: MutationRequestOptions = {}): Promise<T> {
  return parse<T>(
    await fetch(url, {
      method: "PUT",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-CSRF-Token": csrf
      },
      body: JSON.stringify(body),
      ...(options.keepalive ? { keepalive: true } : {})
    })
  );
}

/** DELETE JSON from a mutating endpoint with the current CSRF token. */
export async function deleteJSON<T>(url: string, csrf: string): Promise<T> {
  return parse<T>(
    await fetch(url, {
      method: "DELETE",
      headers: {
        Accept: "application/json",
        "X-CSRF-Token": csrf
      }
    })
  );
}

/** DELETE JSON with a request body for endpoints keyed by payload rather than path. */
export async function deleteJSONBody<T>(url: string, csrf: string, body: unknown = {}): Promise<T> {
  return parse<T>(
    await fetch(url, {
      method: "DELETE",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-CSRF-Token": csrf
      },
      body: JSON.stringify(body)
    })
  );
}

/** POST multipart form data without forcing a Content-Type boundary. */
export async function postForm<T>(url: string, csrf: string, body: FormData): Promise<T> {
  return parse<T>(
    await fetch(url, {
      method: "POST",
      headers: {
        Accept: "application/json",
        "X-CSRF-Token": csrf
      },
      body
    })
  );
}

function composeSendPayload(form: ComposeForm): ComposeForm {
  const { available_attachments: _availableAttachments, forward_attachment: _forwardAttachment, ...payload } = form;
  return payload;
}

function mailListURL(mailboxID: string | null, page: number) {
  const q = new URLSearchParams({ page: String(page) });
  if (mailboxID) q.set("mailbox", mailboxID);
  return `/api/mail?${q}`;
}

function mailCacheEpoch(userID: number) {
  const current = mailCacheEpochs.get(userID);
  if (current !== undefined) return current;
  mailCacheEpochs.set(userID, 0);
  return 0;
}

function mailListCacheKey(userID: number, mailboxID: string | null, page: number, epoch = mailCacheEpoch(userID)) {
  return `user:${userID}:mail-epoch:${epoch}:${mailListURL(mailboxID, page)}`;
}

function searchListURL(query: string, page: number) {
  const q = new URLSearchParams({ q: query, page: String(page) });
  return `/api/search?${q}`;
}

function messageURL(id: string | number, showImages: boolean, highlightQuery = "") {
  const params = new URLSearchParams();
  if (showImages) params.set("images", "1");
  if (highlightQuery.trim()) params.set("q", highlightQuery.trim());
  const q = params.toString() ? `?${params}` : "";
  return `/api/messages/${id}${q}`;
}

function prefetchJSON<T>(url: string, cacheKey = url) {
  void getJSON<T>(url, cacheKey).catch(() => undefined);
}

function cachedJSON<T>(cacheKey: string): T | null {
  const cached = getCache.get(cacheKey);
  return cached ? cached.data as T : null;
}

function cachedMail(userID: number, mailboxID: string | null, page: number): MailListResponse | null {
  const key = mailListCacheKey(userID, mailboxID, page);
  const cached = cachedJSON<MailListResponse>(key);
  return cached || loadMailSnapshot(userID, mailboxID, page);
}

async function loadMail(userID: number, mailboxID: string | null, page: number): Promise<MailListResponse> {
  const url = mailListURL(mailboxID, page);
  const epoch = mailCacheEpoch(userID);
  const key = mailListCacheKey(userID, mailboxID, page, epoch);
  const data = await getJSON<MailListResponse>(url, key);
  if (mailCacheEpoch(userID) !== epoch) {
    getCache.delete(key);
    return data;
  }
  saveMailSnapshot(userID, mailboxID, page, data);
  return data;
}

function prefetchMail(userID: number, mailboxID: string | null, page: number) {
  const url = mailListURL(mailboxID, page);
  void loadMail(userID, mailboxID, page).catch(() => undefined);
}

function clearMailCache(userID: number) {
  mailCacheEpochs.set(userID, mailCacheEpoch(userID) + 1);
  const prefix = `user:${userID}:mail-epoch:`;
  for (const key of getCache.keys()) {
    if (key.startsWith(prefix)) getCache.delete(key);
  }
  clearMailSnapshots(userID);
}

function retainMailCacheForUser(userID: number) {
  for (const cachedUserID of mailCacheEpochs.keys()) {
    if (cachedUserID !== userID) clearMailCache(cachedUserID);
  }
  for (const key of getCache.keys()) {
    const match = key.match(/^user:(\d+):mail-epoch:/);
    if (match && Number(match[1]) !== userID) getCache.delete(key);
  }
  clearOtherMailSnapshots(userID);
  clearOtherCollapsedAccounts(userID);
}


// The api object is deliberately explicit rather than generated: it documents
// the route surface used by the current frontend and keeps response shapes close
// to the call sites that depend on them.
export const api = {
  bootstrap: () => getJSON<Bootstrap>("/api/bootstrap"),
  setup: (csrf: string, body: { email: string; name: string; password: string }) =>
    postJSON<{ ok: boolean }>("/api/setup", csrf, body),
  login: (csrf: string, body: { email: string; password: string }) => postJSON<{ ok: boolean }>("/api/login", csrf, body),
  logout: (csrf: string) => postJSON<{ ok: boolean }>("/api/logout", csrf),
  pushVAPIDPublicKey: () => getJSON<{ public_key: string }>("/api/push/vapid-public-key"),
  savePushSubscription: (csrf: string, subscription: unknown) =>
    postJSON<{ ok: boolean; subscription_id: number }>("/api/push/subscription", csrf, subscription),
  deletePushSubscription: (csrf: string, endpoint: string) =>
    deleteJSONBody<{ ok: boolean }>("/api/push/subscription", csrf, { endpoint }),
  mail: loadMail,
  cachedMail,
  prefetchMail,
  clearMailCache,
  retainMailCacheForUser,
  snoozes: (page: number) => getJSON<SnoozeListResponse>(`/api/snoozes?${new URLSearchParams({ page: String(page) })}`),
  snoozeMessage: (csrf: string, id: number, until: Date, options?: MutationRequestOptions) =>
    putJSON<{ ok: boolean; snoozed: boolean; snooze: MessageSnooze }>(`/api/messages/${id}/snooze`, csrf, { until: until.toISOString() }, options),
  unsnoozeMessage: (csrf: string, id: number) =>
    deleteJSON<{ ok: boolean; snoozed: boolean }>(`/api/messages/${id}/snooze`, csrf),
  search: (query: string, page: number) =>
    getJSON<{ conversations: Conversation[]; page: number; has_prev: boolean; has_next: boolean }>(searchListURL(query, page)),
  prefetchSearch: (query: string, page: number) =>
    prefetchJSON<{ conversations: Conversation[]; page: number; has_prev: boolean; has_next: boolean }>(searchListURL(query, page)),
  brandIcons: (domains: string[]) => {
    const q = new URLSearchParams();
    domains.slice(0, 40).forEach((domain) => q.append("domain", domain));
    return getJSON<{ icons: Record<string, string> }>(`/api/brand-icons?${q}`);
  },
  message: (id: string, showImages: boolean, highlightQuery = "") =>
    getJSON<{
      message: { id: number; account_id: number; subject: string; mailbox_id: number };
      thread: ThreadMessage[];
      compose_from: string;
      from_identities: ComposeIdentity[];
      mailbox_id: number;
        conversation: number;
        snoozed_until?: string;
      }>(messageURL(id, showImages, highlightQuery)),
  prewarmMessage: (id: string | number) =>
    prefetchJSON(`/api/messages/${id}/prefetch`),
  messageLoadStatus: (id: string) =>
    getJSON<{
      conversation: number;
      imap_fetch_count: number;
      local_blob_count: number;
      indexed_count: number;
      unavailable_count: number;
      source: "imap" | "local_blob" | "local" | "indexed" | "preview" | string;
    }>(`/api/messages/${id}/load-status`),
  messageOriginal: (id: number) => getJSON<MessageOriginalSource>(`/api/messages/${id}/original`),
  searchExplanation: (id: number, query: string, hitID = 0) => {
    const params = new URLSearchParams();
    params.set("q", query.trim());
    if (hitID > 0) params.set("hit", String(hitID));
    return getJSON<SearchExplanation>(`/api/messages/${id}/search-explanation?${params}`);
  },
  trustImages: (csrf: string, id: number) => postJSON<{ ok: boolean }>(`/api/messages/${id}/images/trust`, csrf),
  unsubscribe: (csrf: string, id: number) =>
    postJSON<{ ok: boolean; already_sent: boolean; sent_at: string }>(`/api/messages/${id}/unsubscribe`, csrf),
  setStarred: (csrf: string, id: number, starred: boolean) =>
    postJSON<{ ok: boolean; message: { id: number; is_starred: boolean } }>(`/api/messages/${id}/star`, csrf, { starred }),
  moveMessage: (csrf: string, id: number, mailboxID: number, options?: MutationRequestOptions) =>
    postJSON<{ ok: boolean; mailbox: string }>(`/api/messages/${id}/move`, csrf, { mailbox_id: mailboxID }, options),
  bulkMoveMessages: (csrf: string, ids: number[], mailboxID: number, options?: MutationRequestOptions) =>
    postJSON<{ ok: boolean; queued: boolean; moved?: number; run_id?: number; mailbox: string }>("/api/messages/bulk-move", csrf, {
      message_ids: ids,
      mailbox_id: mailboxID
    }, options),
  bulkCopyMessages: (csrf: string, ids: number[], mailboxID: number) =>
    postJSON<{ ok: boolean; queued: boolean; copied?: number; run_id?: number; mailbox: string }>("/api/messages/bulk-copy", csrf, {
      message_ids: ids,
      mailbox_id: mailboxID
    }),
  bulkRead: (csrf: string, ids: number[], read: boolean, options?: MutationRequestOptions) =>
    postJSON<{ ok: boolean; updated: number }>("/api/messages/bulk-read", csrf, { ids, read }, options),
  compose: (query: string) =>
    getJSON<{ compose: ComposeForm; compose_from: string; from_identities: ComposeIdentity[] }>(`/api/compose${query ? `?${query}` : ""}`),
  // Compose sends pure JSON when possible, then switches to multipart only when
  // there are file bodies. Inline files are represented in the JSON payload by
  // stable form field names and Content-ID metadata.
  send: (csrf: string, form: ComposeForm, attachments: ComposeAttachmentUpload[] = []) => {
    const payload = composeSendPayload(form);
    if (attachments.length === 0) {
      return postJSON<{ ok: boolean; message_id: number }>("/api/compose", csrf, payload);
    }
    const body = new FormData();
    body.append("payload", JSON.stringify({
      ...payload,
      attachments: attachments.map((attachment) => ({
        field: attachment.field,
        filename: attachment.filename,
        content_type: attachment.content_type,
        content_id: attachment.content_id,
        inline: attachment.inline,
        size: attachment.size
      }))
    }));
    attachments.forEach((attachment) => body.append(attachment.field, attachment.file, attachment.filename));
    return postForm<{ ok: boolean; message_id: number }>("/api/compose", csrf, body);
  },
  saveDraft: (csrf: string, form: ComposeForm, attachments: ComposeAttachmentUpload[] = []) => {
    const payload = composeSendPayload(form);
    if (attachments.length === 0) {
      return postJSON<{ ok: boolean; message_id: number }>("/api/compose/draft", csrf, payload);
    }
    const body = new FormData();
    body.append("payload", JSON.stringify({
      ...payload,
      attachments: attachments.map((attachment) => ({
        field: attachment.field,
        filename: attachment.filename,
        content_type: attachment.content_type,
        content_id: attachment.content_id,
        inline: attachment.inline,
        size: attachment.size
      }))
    }));
    attachments.forEach((attachment) => body.append(attachment.field, attachment.file, attachment.filename));
    return postForm<{ ok: boolean; message_id: number }>("/api/compose/draft", csrf, body);
  },
  contacts: (query = "") => {
    const q = query.trim() ? `?${new URLSearchParams({ q: query.trim() })}` : "";
    return getJSON<{ contacts: Contact[] }>(`/api/contacts${q}`);
  },
  contactAutocomplete: (query: string) =>
    getJSON<{ contacts: ContactAutocomplete[] }>(`/api/contacts/autocomplete?${new URLSearchParams({ q: query })}`),
  createContact: (csrf: string, contact: Contact) => postJSON<{ contact: Contact }>("/api/contacts", csrf, contact),
  updateContact: (csrf: string, contact: Contact) => putJSON<{ contact: Contact }>(`/api/contacts/${contact.id}`, csrf, contact),
  deleteContact: (csrf: string, id: number) => deleteJSON<{ ok: boolean }>(`/api/contacts/${id}`, csrf),
  uploadContactIcon: (csrf: string, id: number, file: File) => {
    const form = new FormData();
    form.append("icon", file);
    return postForm<{ contact: Contact }>(`/api/contacts/${id}/icon`, csrf, form);
  },
  deleteContactIcon: (csrf: string, id: number) => deleteJSON<{ contact: Contact }>(`/api/contacts/${id}/icon`, csrf),
  importContacts: (csrf: string, file: File) => {
    const form = new FormData();
    form.append("file", file);
    return postForm<{ ok: boolean; imported: number; updated: number }>("/api/contacts/import", csrf, form);
  },
  addSenderContact: (csrf: string, id: number) =>
    postJSON<{ contact: Contact; created: boolean }>(`/api/messages/${id}/contacts/add-sender`, csrf),
  syncStatus: () => getJSON<{ running: boolean; latest: SyncRun | null }>("/api/sync/status"),
  account: () =>
    getJSON<{
      imap_accounts: Account[];
      smtp_accounts: SMTPAccount[];
      identities: MailIdentity[];
      me_contacts: Contact[];
      sync_runs: SyncRun[];
      sync_folders: SyncFolder[];
      storage?: StorageStats;
      notice: string;
      account_needs_password?: boolean;
    }>("/api/account"),
  folderProgress: () => getJSON<{ folders: FolderProgress[] }>("/api/account/folders/progress"),
  storage: () => getJSON<StorageStats>("/api/storage"),
  plugins: () => getJSON<{ enabled: string[] }>("/api/plugins"),
  saveProfile: (csrf: string, profile: {
    backup_email: string;
    date_locale: string;
    date_format: string;
    theme: string;
    search_preset: string;
    search_recency_bias: string;
    search_fuzzy: string;
    search_sender_boost: boolean;
    search_sender_history: string;
    search_contact_boost: string;
    search_attachment_weight: string;
    search_compact_splitting: boolean;
  }) =>
    postJSON<{ user: User }>("/api/profile", csrf, profile),
  saveSwipePreferences: (csrf: string, preferences: SwipePreferences) =>
    postJSON<{ swipe_preferences: SwipePreferences }>("/api/profile/swipes", csrf, preferences),
  requestPasswordReset: (csrf: string, email: string) =>
    postJSON<{ ok: boolean }>("/api/password-reset/request", csrf, { email }),
  completePasswordReset: (csrf: string, token: string, password: string) =>
    postJSON<{ ok: boolean }>("/api/password-reset/complete", csrf, { token, password }),
  saveIMAPAccount: (csrf: string, account: Record<string, unknown>) =>
    postJSON<{ ok: boolean; account: Account }>("/api/account/imap", csrf, account),
  imapAccountPurgeEstimate: (id: number) =>
    getJSON<AccountPurgeEstimate>(`/api/account/imap/${id}/purge-estimate`),
  deleteIMAPAccount: (csrf: string, id: number, confirm: string) =>
    postJSON<{ ok: boolean; queued: boolean; run_id: number; estimate: AccountPurgeEstimate }>(`/api/account/imap/${id}/delete`, csrf, { confirm }),
  createIMAPFolder: (csrf: string, accountID: number, name: string) =>
    postJSON<{ ok: boolean; mailbox: Mailbox }>(`/api/account/imap/${accountID}/folders`, csrf, { name }),
  saveSMTPAccount: (csrf: string, account: Record<string, unknown>) =>
    postJSON<{ ok: boolean; smtp_account: SMTPAccount }>("/api/account/smtp", csrf, account),
  deleteSMTPAccount: (csrf: string, id: number) =>
    deleteJSON<{ ok: boolean }>(`/api/account/smtp/${id}`, csrf),
  saveMailIdentity: (csrf: string, identity: Record<string, unknown>) =>
    postJSON<{ ok: boolean; identity: MailIdentity; identities: MailIdentity[] }>("/api/account/identities", csrf, identity),
  syncAccount: (csrf: string) => postJSON<{ ok: boolean }>("/api/account/sync", csrf),
  rebuildIMAPAccountSearchIndex: (csrf: string, id: number) =>
    postJSON<{ ok: boolean; queued: boolean; run_id: number }>(`/api/account/imap/${id}/rebuild-search-index`, csrf),
  setFolderMode: (csrf: string, id: number, syncMode: string) =>
    postJSON<{ ok: boolean }>(`/api/account/folders/${id}/mode`, csrf, { sync_mode: syncMode }),
  saveFolderSettings: (csrf: string, id: number, settings: Record<string, unknown>) =>
    postJSON<{ ok: boolean; queued?: boolean; run_id?: number }>(`/api/account/folders/${id}/settings`, csrf, settings),
  syncFolder: (csrf: string, id: number) => postJSON<{ ok: boolean }>(`/api/account/folders/${id}/sync`, csrf),
  rebuildFolderSearchIndex: (csrf: string, id: number) =>
    postJSON<{ ok: boolean; queued: boolean; run_id: number }>(`/api/account/folders/${id}/search-index/rebuild`, csrf),
  purgeFolderSearchIndex: (csrf: string, id: number) =>
    postJSON<{ ok: boolean; queued: boolean; run_id: number }>(`/api/account/folders/${id}/search-index/purge`, csrf),
  purgeFolderLocalReferences: (csrf: string, id: number) =>
    postJSON<{ ok: boolean; queued: boolean; run_id: number }>(`/api/account/folders/${id}/local-references/purge`, csrf),
  users: () => getJSON<{ users: User[]; password_reset_from_address?: string }>("/api/admin/users"),
  createUser: (csrf: string, body: { email: string; name: string; password: string; is_admin: boolean }) =>
    postJSON<{ ok: boolean }>("/api/admin/users", csrf, body),
  setUserPassword: (csrf: string, id: number, password: string) =>
    postJSON<{ ok: boolean }>(`/api/admin/users/${id}/password`, csrf, { password }),
  deleteUser: (csrf: string, id: number) =>
    deleteJSON<{ ok: boolean }>(`/api/admin/users/${id}`, csrf),
  savePasswordResetSettings: (csrf: string, fromAddress: string) =>
    postJSON<{ ok: boolean; from_address: string }>("/api/admin/password-reset", csrf, { from_address: fromAddress }),
  adminPlugins: () => getJSON<{ plugins: PluginSetting[] }>("/api/admin/plugins"),
  setAdminPlugin: (csrf: string, id: string, enabled: boolean) =>
    postJSON<{ ok: boolean; plugins: PluginSetting[] }>(`/api/admin/plugins/${encodeURIComponent(id)}`, csrf, { enabled }),
  remoteImageBlocklist: () => getJSON<{ patterns: string[] }>("/api/admin/remote-image-blocklist"),
  saveRemoteImageBlocklist: (csrf: string, patterns: string[]) =>
    postJSON<{ ok: boolean; patterns: string[] }>("/api/admin/remote-image-blocklist", csrf, { patterns }),
  syncRun: (id: string) => getJSON<{ sync_run: SyncRun; live: SyncRunLiveDetail }>(`/api/sync-runs/${id}`),
  cancelSyncRun: (csrf: string, id: number) => postJSON<{ ok: boolean }>(`/api/sync-runs/${id}/cancel`, csrf)
};
