// File overview: User-scoped offline cache of recently viewed mail. Opening a
// conversation stores its render-ready body payload; list pages and thread
// opens refresh header rows for everything seen. Entries expire after seven
// days so the corpus tracks what this user actually looked at, and every
// record is keyed by user_id, mirroring the localStorage snapshot rules.
// Storage mechanics live in offlineDb; this module owns mail records only.

import type { ComposeIdentity, Conversation, ThreadMessage } from "../types";
import {
  BODIES_STORE,
  HEADERS_STORE,
  getAllFromStore,
  openOfflineDB,
  putInStore,
  runInTransaction,
  userKeyRange,
  visitCursorEntries
} from "./offlineDb";

/** OfflineThreadPayload mirrors the /api/messages/{id} response used to render a conversation. */
export type OfflineThreadPayload = {
  message: { id: number; account_id: number; subject: string; mailbox_id: number };
  thread: ThreadMessage[];
  compose_from: string;
  from_identities: ComposeIdentity[];
  mailbox_id: number;
  conversation: number;
  snoozed_until?: string;
};

export type CachedThread = {
  payload: OfflineThreadPayload;
  saved_at: number;
};

export type RecentConversationsOptions = {
  mailboxID?: number | null;
  offset?: number;
  limit?: number;
};

export type CachedThreadMeta = {
  thread_ids: number[];
  subject: string;
  from_addr: string;
  date: string;
  search_text: string;
};

export type RecentOfflinePage = {
  conversations: Conversation[];
  page: number;
  has_prev: boolean;
  has_next: boolean;
  total: number;
};

/** Retention and size caps. Bodies are far larger than headers, so they keep a tighter cap. */
const retentionMS = 7 * 24 * 60 * 60 * 1000;
const maxHeaderEntries = 4000;
const maxBodyEntries = 400;
const maxBodyBytes = 3_000_000;
const maxSearchTextChars = 150_000;
/** Pruning scans whole stores; throttle so hot paths pay that at most once a minute. */
const pruneMinIntervalMS = 60_000;

type HeaderRecord = {
  user_id: number;
  key: number;
  viewed_at: number;
  conversation: Conversation;
};

type BodyRecord = {
  user_id: number;
  root_id: number;
  thread_ids: number[];
  viewed_at: number;
  saved_at: number;
  mailbox_id: number;
  subject: string;
  from_addr: string;
  date: string;
  search_text: string;
  payload: OfflineThreadPayload;
};

let lastPruneAt = 0;

/** recordMailConversations upserts header rows so offline lists and search stay current. */
export async function recordMailConversations(userID: number, conversations: Conversation[]): Promise<void> {
  if (userID <= 0 || conversations.length === 0) return;
  const now = Date.now();
  await runInTransaction([HEADERS_STORE], "readwrite", (transaction) => {
    const store = transaction.objectStore(HEADERS_STORE);
    for (const conversation of conversations) {
      if (!validConversationRow(conversation)) continue;
      const record: HeaderRecord = { user_id: userID, key: conversation.message.id, viewed_at: now, conversation };
      store.put(record);
    }
  });
  // Awaited rather than fire-and-forget so callers never observe stores
  // mid-sweep; the throttle bounds how often that wait actually costs anything.
  await pruneWhenDue();
}

/** recordThreadPayload caches a rendered conversation plus header rows for every message in it. */
export async function recordThreadPayload(userID: number, payload: OfflineThreadPayload): Promise<void> {
  if (userID <= 0 || !payload?.message || !(payload.message.id > 0)) return;
  const now = Date.now();
  const record = buildBodyRecord(userID, payload, now);
  if (record) await putInStore(BODIES_STORE, record);
  // Headers are recorded even when the body was too large, so list and search
  // coverage never depends on body size.
  await recordMailConversations(userID, threadHeaderRows(payload));
}

function buildBodyRecord(userID: number, payload: OfflineThreadPayload, now: number): BodyRecord | null {
  let serialized = "";
  try {
    serialized = JSON.stringify(payload);
  } catch {
    serialized = "";
  }
  if (!serialized || serialized.length > maxBodyBytes) return null;
  const threadIDs = threadIDsOf(payload);
  return {
    user_id: userID,
    root_id: payload.message.id,
    thread_ids: threadIDs.length > 0 ? threadIDs : [payload.message.id],
    viewed_at: now,
    saved_at: now,
    mailbox_id: payload.mailbox_id || payload.message.mailbox_id,
    subject: String(payload.message.subject || ""),
    from_addr: firstSender(payload),
    date: firstDate(payload),
    search_text: threadSearchText(payload),
    payload
  };
}

/**
 * getCachedThread returns the stored render-ready conversation for any message
 * in the thread. It walks the message_id index and stops at the first hit, so
 * unrelated oversized payloads are never materialized during the lookup.
 */
export async function getCachedThread(userID: number, messageID: number): Promise<CachedThread | null> {
  if (userID <= 0 || !(messageID > 0)) return null;
  const db = await openOfflineDB();
  if (!db) return null;
  const found = await new Promise<BodyRecord | null>((resolve) => {
    let transaction: IDBTransaction;
    try {
      transaction = db.transaction([BODIES_STORE], "readonly");
    } catch {
      resolve(null);
      return;
    }
    let matched: BodyRecord | null = null;
    transaction.oncomplete = () => resolve(matched);
    transaction.onerror = () => resolve(null);
    transaction.onabort = () => resolve(null);
    visitCursorEntries(
      transaction.objectStore(BODIES_STORE).index("message_id").openCursor(IDBKeyRange.only(messageID)),
      (cursor) => {
        const record = cursor.value as BodyRecord;
        if (record?.user_id === userID && record.payload) {
          matched = record;
          return false;
        }
        return true;
      }
    );
  });
  if (!found) return null;
  void touchCachedThread(found);
  return { payload: found.payload, saved_at: found.saved_at };
}

/**
 * touchCachedThread refreshes the seven-day window when a cached conversation
 * is viewed again. Writes more than once a minute add churn without meaning.
 */
async function touchCachedThread(record: BodyRecord): Promise<void> {
  if (Date.now() - record.viewed_at < pruneMinIntervalMS) return;
  await putInStore(BODIES_STORE, { ...record, viewed_at: Date.now() });
}

/**
 * getRecentConversations returns cached header rows sorted by conversation
 * date, newest first, optionally filtered to one mailbox. Header rows are
 * small and capped, so a full range scan is the right query here.
 */
export async function getRecentConversations(userID: number, options: RecentConversationsOptions = {}): Promise<Conversation[]> {
  if (userID <= 0) return [];
  const records = await getAllFromStore<HeaderRecord>(HEADERS_STORE, userKeyRange(userID));
  const mailboxID = options.mailboxID ?? null;
  const rows = records
    .filter((record) => validConversationRow(record.conversation))
    .map((record) => record.conversation)
    .filter((conversation) => mailboxID === null || conversation.message.mailbox_id === mailboxID);
  rows.sort((left, right) => String(right.message.date || "").localeCompare(String(left.message.date || "")));
  const offset = Math.max(0, options.offset || 0);
  const limit = options.limit ?? rows.length;
  return rows.slice(offset, offset + limit);
}

/** buildRecentOfflinePage shapes recent headers like one mail list page for offline fallbacks. */
export async function buildRecentOfflinePage(userID: number, mailboxID: string | null, page: number, pageSize: number): Promise<RecentOfflinePage | null> {
  if (!(Number.isInteger(page) && page > 0)) return null;
  let numericMailboxID: number | null = null;
  if (mailboxID !== null) {
    numericMailboxID = Number(mailboxID);
    if (!(Number.isInteger(numericMailboxID) && numericMailboxID > 0)) return null;
  }
  const all = await getRecentConversations(userID, { mailboxID: numericMailboxID });
  if (all.length === 0) return null;
  const start = (page - 1) * pageSize;
  const conversations = all.slice(start, start + pageSize);
  if (conversations.length === 0) return null;
  return { conversations, page, has_prev: false, has_next: false, total: all.length };
}

/**
 * getCachedThreadMetas projects lightweight search fields from cached bodies.
 * A cursor projection keeps multi-megabyte payloads collectible per step
 * instead of holding every body in memory at once.
 */
export async function getCachedThreadMetas(userID: number): Promise<CachedThreadMeta[]> {
  if (userID <= 0) return [];
  const metas: CachedThreadMeta[] = [];
  await runInTransaction([BODIES_STORE], "readonly", (transaction) => {
    visitCursorEntries(transaction.objectStore(BODIES_STORE).openCursor(userKeyRange(userID)), (cursor) => {
      const record = cursor.value as BodyRecord;
      metas.push({
        thread_ids: Array.isArray(record.thread_ids) ? record.thread_ids : [],
        subject: String(record.subject || ""),
        from_addr: String(record.from_addr || ""),
        date: String(record.date || ""),
        search_text: String(record.search_text || "")
      });
    });
  });
  return metas;
}

/**
 * pruneWhenDue drops entries older than seven days and enforces size caps.
 * Runs after writes but throttled: each pass scans both stores end to end.
 * Pass force=true for explicit sweeps that must ignore the throttle window.
 */
export async function pruneWhenDue(force = false): Promise<void> {
  const now = Date.now();
  if (!force && now - lastPruneAt < pruneMinIntervalMS) return;
  lastPruneAt = now;
  const cutoff = now - retentionMS;
  await pruneStore(HEADERS_STORE, maxHeaderEntries, cutoff);
  await pruneStore(BODIES_STORE, maxBodyEntries, cutoff);
}

/** pruneStore deletes expired rows, then trims past the cap newest-first. */
async function pruneStore(storeName: string, cap: number, cutoff: number): Promise<void> {
  await runInTransaction([storeName], "readwrite", (transaction) => {
    const store = transaction.objectStore(storeName);
    visitCursorEntries(store.index("viewed_at").openCursor(IDBKeyRange.upperBound(cutoff)), (cursor) => {
      cursor.delete();
    });
  });
  const retained: Array<{ key: IDBValidKey; viewedAt: number }> = [];
  await runInTransaction([storeName], "readonly", (transaction) => {
    visitCursorEntries(transaction.objectStore(storeName).openCursor(), (cursor) => {
      const record = cursor.value as { viewed_at?: unknown };
      retained.push({ key: cursor.primaryKey, viewedAt: Number(record?.viewed_at || 0) });
    });
  });
  if (retained.length <= cap) return;
  retained.sort((left, right) => right.viewedAt - left.viewedAt);
  await runInTransaction([storeName], "readwrite", (transaction) => {
    const store = transaction.objectStore(storeName);
    for (let index = cap; index < retained.length; index += 1) store.delete(retained[index].key);
  });
}

/** clearOfflineMailDataForUser drops cached bodies/headers for one user; their outbox is preserved. */
export async function clearOfflineMailDataForUser(userID: number): Promise<void> {
  if (userID <= 0) return;
  await runInTransaction([HEADERS_STORE, BODIES_STORE], "readwrite", (transaction) => {
    transaction.objectStore(HEADERS_STORE).delete(userKeyRange(userID));
    transaction.objectStore(BODIES_STORE).delete(userKeyRange(userID));
  });
}

/** retainOfflineDataForUser removes every other user's cached mail data after an account switch. */
export async function retainOfflineDataForUser(keepUserID: number): Promise<void> {
  if (keepUserID <= 0) return;
  await runInTransaction([HEADERS_STORE, BODIES_STORE], "readwrite", (transaction) => {
    visitCursorEntries(transaction.objectStore(HEADERS_STORE).openCursor(), (cursor) => {
      if ((cursor.value as HeaderRecord).user_id !== keepUserID) cursor.delete();
    });
    visitCursorEntries(transaction.objectStore(BODIES_STORE).openCursor(), (cursor) => {
      if ((cursor.value as BodyRecord).user_id !== keepUserID) cursor.delete();
    });
  });
}

// --- helpers ----------------------------------------------------------------

function threadIDsOf(payload: OfflineThreadPayload): number[] {
  return Array.isArray(payload.thread)
    ? payload.thread.map((item) => Number(item?.message?.id)).filter((id) => Number.isInteger(id) && id > 0)
    : [];
}

function validConversationRow(value: unknown): value is Conversation {
  if (!value || typeof value !== "object") return false;
  const conversation = value as Conversation;
  return typeof conversation.snippet === "string" &&
    Boolean(conversation.message) && typeof conversation.message.id === "number" &&
    conversation.message.id > 0;
}

function firstSender(payload: OfflineThreadPayload): string {
  const first = payload.thread?.[0];
  return String(first?.sender_email || "");
}

function firstDate(payload: OfflineThreadPayload): string {
  const dates = [...(payload.thread || [])]
    .map((item) => String(item?.message?.date || ""))
    .filter(Boolean)
    .sort();
  return dates[dates.length - 1] || "";
}

/** threadHeaderRows derives list-searchable header rows from a thread payload. */
function threadHeaderRows(payload: OfflineThreadPayload): Conversation[] {
  const thread = Array.isArray(payload.thread) ? payload.thread : [];
  const rows: Conversation[] = [];
  for (const item of thread) {
    const message = item?.message;
    if (!message || !(message.id > 0)) continue;
    rows.push({
      message,
      starred_message_id: message.id,
      participants: item.sender_name || item.sender_email || String(message.from_addr || ""),
      recipient_participants: "",
      count: 1,
      is_read: Boolean(message.is_read),
      has_attachments: Boolean(message.has_attachments),
      attachment_names: (item.attachments || []).map((attachment) => attachment.filename).filter(Boolean),
      snippet: String(item.snippet || ""),
      snoozed_until: payload.snoozed_until || undefined
    });
  }
  return rows;
}

function threadSearchText(payload: OfflineThreadPayload): string {
  const parts: string[] = [];
  const thread = Array.isArray(payload.thread) ? payload.thread : [];
  for (const item of thread) {
    parts.push(htmlToText(item.body_doc));
    parts.push(htmlToText(item.full_body_doc));
  }
  return parts.join(" ").replace(/\s+/g, " ").trim().slice(0, maxSearchTextChars);
}

function htmlToText(html: string): string {
  if (!html || typeof DOMParser === "undefined") return "";
  try {
    const doc = new DOMParser().parseFromString(html, "text/html");
    return doc.body?.textContent || "";
  } catch {
    return "";
  }
}
