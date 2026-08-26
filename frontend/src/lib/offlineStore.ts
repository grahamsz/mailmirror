// File overview: Bounded, user-scoped IndexedDB cache of recently viewed mail.
// Opening a conversation stores its render-ready body payload; list pages and
// thread opens refresh header rows for everything seen. Entries expire after
// seven days so the offline corpus tracks what this user actually looked at.
// Every record is keyed by user_id, mirroring the localStorage snapshot rules.

import type { ComposeForm, ComposeIdentity, Conversation, ThreadMessage } from "../types";

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

export type OutboxAttachment = {
  field: string;
  filename: string;
  content_type: string;
  content_id: string;
  inline: boolean;
  size: number;
  bytes: ArrayBuffer;
};

export type OutboxRecord = {
  id?: number;
  user_id: number;
  created_at: number;
  updated_at: number;
  attempts: number;
  status: "queued" | "failed";
  last_error: string;
  subject: string;
  recipients: string;
  form: ComposeForm;
  attachments: OutboxAttachment[];
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

const DB_NAME = "rolltop.offline";
const DB_VERSION = 1;
const HEADERS_STORE = "headers";
const BODIES_STORE = "bodies";
const OUTBOX_STORE = "outbox";
const retentionMS = 7 * 24 * 60 * 60 * 1000;
const maxHeaderEntries = 4000;
const maxBodyEntries = 400;
const maxBodyBytes = 3_000_000;
const maxSearchTextChars = 150_000;
const maxOutboxItemsPerUser = 25;
const maxOutboxAttachmentBytes = 64_000_000;

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

let openPromise: Promise<IDBDatabase | null> | null = null;

function openOfflineDB(): Promise<IDBDatabase | null> {
  if (openPromise) return openPromise;
  openPromise = new Promise<IDBDatabase | null>((resolve) => {
    try {
      if (typeof indexedDB === "undefined") {
        resolve(null);
        return;
      }
      const request = indexedDB.open(DB_NAME, DB_VERSION);
      request.onupgradeneeded = () => {
        const db = request.result;
        if (!db.objectStoreNames.contains(HEADERS_STORE)) {
          db.createObjectStore(HEADERS_STORE, { keyPath: ["user_id", "key"] }).createIndex("viewed_at", "viewed_at");
        }
        if (!db.objectStoreNames.contains(BODIES_STORE)) {
          const bodies = db.createObjectStore(BODIES_STORE, { keyPath: ["user_id", "root_id"] });
          bodies.createIndex("message_id", "thread_ids", { multiEntry: true });
          bodies.createIndex("viewed_at", "viewed_at");
        }
        if (!db.objectStoreNames.contains(OUTBOX_STORE)) {
          const outbox = db.createObjectStore(OUTBOX_STORE, { keyPath: "id", autoIncrement: true });
          outbox.createIndex("user_status", ["user_id", "status"]);
        }
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => resolve(null);
      request.onblocked = () => resolve(null);
    } catch {
      resolve(null);
    }
  });
  return openPromise;
}

/**
 * runInTransaction executes one best-effort transaction. Requests must be
 * queued synchronously inside `run`; follow-up work belongs in request
 * onsuccess handlers so the transaction stays alive until it finishes.
 */
async function runInTransaction(stores: string[], mode: IDBTransactionMode, run: (transaction: IDBTransaction) => void): Promise<boolean> {
  const db = await openOfflineDB();
  if (!db) return false;
  return new Promise<boolean>((resolve) => {
    let transaction: IDBTransaction;
    try {
      transaction = db.transaction(stores, mode);
    } catch {
      resolve(false);
      return;
    }
    transaction.oncomplete = () => resolve(true);
    transaction.onerror = () => resolve(false);
    transaction.onabort = () => resolve(false);
    try {
      run(transaction);
    } catch {
      try {
        transaction.abort();
      } catch {
        // Already committed or aborted; nothing left to clean up.
      }
    }
  });
}

/** getAllFromStore resolves every record in one store, optionally limited to a key range. */
async function getAllFromStore<T>(storeName: string, range?: IDBKeyRange): Promise<T[]> {
  const db = await openOfflineDB();
  if (!db) return [];
  return new Promise<T[]>((resolve) => {
    let transaction: IDBTransaction;
    try {
      transaction = db.transaction([storeName], "readonly");
    } catch {
      resolve([]);
      return;
    }
    const request = transaction.objectStore(storeName).getAll(range);
    request.onsuccess = () => resolve((request.result || []) as T[]);
    request.onerror = () => resolve([]);
  });
}

function deleteCursorEntries(cursor: IDBRequest<IDBCursorWithValue | null>, visit: (cursor: IDBCursorWithValue) => void): void {
  cursor.onsuccess = () => {
    const value = cursor.result;
    if (!value) return;
    visit(value);
    value.continue();
  };
}

function userKeyRange(userID: number): IDBKeyRange {
  return IDBKeyRange.bound([userID, 0], [userID, Number.MAX_SAFE_INTEGER]);
}

function validConversationRow(value: unknown): value is Conversation {
  if (!value || typeof value !== "object") return false;
  const conversation = value as Conversation;
  return typeof conversation.snippet === "string" &&
    Boolean(conversation.message) && typeof conversation.message.id === "number" &&
    conversation.message.id > 0;
}

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
  void pruneOfflineMail();
}

/** recordThreadPayload caches a rendered conversation plus header rows for every message in it. */
export async function recordThreadPayload(userID: number, payload: OfflineThreadPayload): Promise<void> {
  if (userID <= 0 || !payload?.message || !(payload.message.id > 0)) return;
  const now = Date.now();
  const threadIDs = Array.isArray(payload.thread)
    ? payload.thread.map((item) => Number(item?.message?.id)).filter((id) => Number.isInteger(id) && id > 0)
    : [];
  let serialized = "";
  try {
    serialized = JSON.stringify(payload);
  } catch {
    serialized = "";
  }
  if (serialized && serialized.length <= maxBodyBytes) {
    const record: BodyRecord = {
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
    await runInTransaction([BODIES_STORE], "readwrite", (transaction) => {
      transaction.objectStore(BODIES_STORE).put(record);
    });
  }
  await recordMailConversations(userID, threadHeaderRows(payload));
}

/** getCachedThread returns the stored render-ready conversation for any message in the thread. */
export async function getCachedThread(userID: number, messageID: number): Promise<CachedThread | null> {
  if (userID <= 0 || !(messageID > 0)) return null;
  const records = await getAllFromStore<BodyRecord>(BODIES_STORE, userKeyRange(userID));
  const record = records.find((item) => item.user_id === userID && item.payload &&
    (item.root_id === messageID || (Array.isArray(item.thread_ids) && item.thread_ids.includes(messageID))));
  if (!record) return null;
  void touchCachedThread(userID, record.root_id);
  return { payload: record.payload, saved_at: record.saved_at };
}

function touchCachedThread(userID: number, rootID: number): Promise<void> {
  return runInTransaction([BODIES_STORE], "readwrite", (transaction) => {
    const store = transaction.objectStore(BODIES_STORE);
    const request = store.get([userID, rootID]);
    request.onsuccess = () => {
      const record = request.result as BodyRecord | undefined;
      if (record) store.put({ ...record, viewed_at: Date.now() });
    };
  }).then(() => undefined);
}

/**
 * getRecentConversations returns cached header rows sorted by conversation
 * date, newest first, optionally filtered to one mailbox.
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
export async function buildRecentOfflinePage(
  userID: number,
  mailboxID: string | null,
  page: number,
  pageSize: number
): Promise<{ conversations: Conversation[]; page: number; has_prev: boolean; has_next: boolean; total: number } | null> {
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

/** getCachedThreadMetas exposes cached body text for offline search without payloads. */
export async function getCachedThreadMetas(userID: number): Promise<CachedThreadMeta[]> {
  if (userID <= 0) return [];
  const records = await getAllFromStore<BodyRecord>(BODIES_STORE, userKeyRange(userID));
  return records.map((record) => ({
    thread_ids: Array.isArray(record.thread_ids) ? record.thread_ids : [],
    subject: String(record.subject || ""),
    from_addr: String(record.from_addr || ""),
    date: String(record.date || ""),
    search_text: String(record.search_text || "")
  }));
}

/** pruneOfflineMail drops entries older than seven days and enforces global size caps. */
export async function pruneOfflineMail(): Promise<void> {
  const cutoff = Date.now() - retentionMS;
  const caps: Array<[string, number]> = [[HEADERS_STORE, maxHeaderEntries], [BODIES_STORE, maxBodyEntries]];
  for (const [storeName, cap] of caps) {
    await runInTransaction([storeName], "readwrite", (transaction) => {
      const store = transaction.objectStore(storeName);
      deleteCursorEntries(store.index("viewed_at").openCursor(IDBKeyRange.upperBound(cutoff)), (cursor) => {
        cursor.delete();
      });
      const retained: Array<{ key: IDBValidKey; viewedAt: number }> = [];
      const scan = store.openCursor();
      scan.onsuccess = () => {
        const value = scan.result;
        if (!value) {
          retained.sort((left, right) => right.viewedAt - left.viewedAt);
          for (let index = cap; index < retained.length; index += 1) store.delete(retained[index].key);
          return;
        }
        const record = value.value as { viewed_at?: unknown };
        retained.push({ key: value.primaryKey, viewedAt: Number(record?.viewed_at || 0) });
        value.continue();
      };
    });
  }
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
    deleteCursorEntries(transaction.objectStore(HEADERS_STORE).openCursor(), (cursor) => {
      if ((cursor.value as HeaderRecord).user_id !== keepUserID) cursor.delete();
    });
    deleteCursorEntries(transaction.objectStore(BODIES_STORE).openCursor(), (cursor) => {
      if ((cursor.value as BodyRecord).user_id !== keepUserID) cursor.delete();
    });
  });
}

// --- Outbox persistence -----------------------------------------------------

export async function enqueueOutboxItem(item: OutboxRecord): Promise<number | null> {
  if (item.user_id <= 0) return null;
  if (!(await outboxWithinBudget(item.user_id, item))) return null;
  const db = await openOfflineDB();
  if (!db) return null;
  return new Promise<number | null>((resolve) => {
    let transaction: IDBTransaction;
    try {
      transaction = db.transaction([OUTBOX_STORE], "readwrite");
    } catch {
      resolve(null);
      return;
    }
    const request = transaction.objectStore(OUTBOX_STORE).add(item);
    request.onsuccess = () => resolve(Number(request.result));
    request.onerror = () => resolve(null);
  });
}

export async function listOutboxItems(userID: number, status?: OutboxRecord["status"]): Promise<OutboxRecord[]> {
  if (userID <= 0) return [];
  const db = await openOfflineDB();
  if (!db) return [];
  const items = await new Promise<OutboxRecord[]>((resolve) => {
    let transaction: IDBTransaction;
    try {
      transaction = db.transaction([OUTBOX_STORE], "readonly");
    } catch {
      resolve([]);
      return;
    }
    const store = transaction.objectStore(OUTBOX_STORE);
    const request = status
      ? store.index("user_status").getAll(IDBKeyRange.bound([userID, status], [userID, status]))
      : store.getAll(userKeyRange(userID));
    request.onsuccess = () => resolve((request.result || []) as OutboxRecord[]);
    request.onerror = () => resolve([]);
  });
  return items.sort((left, right) => left.created_at - right.created_at || (left.id || 0) - (right.id || 0));
}

export async function countOutboxItems(userID: number, status?: OutboxRecord["status"]): Promise<number> {
  return (await listOutboxItems(userID, status)).length;
}

export async function getOutboxItem(id: number): Promise<OutboxRecord | null> {
  if (!(id > 0)) return null;
  const db = await openOfflineDB();
  if (!db) return null;
  return new Promise<OutboxRecord | null>((resolve) => {
    let transaction: IDBTransaction;
    try {
      transaction = db.transaction([OUTBOX_STORE], "readonly");
    } catch {
      resolve(null);
      return;
    }
    const request = transaction.objectStore(OUTBOX_STORE).get(id);
    request.onsuccess = () => resolve((request.result as OutboxRecord | undefined) || null);
    request.onerror = () => resolve(null);
  });
}

export async function updateOutboxItem(item: OutboxRecord): Promise<void> {
  if (!(item.id && item.id > 0)) return;
  await runInTransaction([OUTBOX_STORE], "readwrite", (transaction) => {
    transaction.objectStore(OUTBOX_STORE).put(item);
  });
}

export async function deleteOutboxItem(id: number): Promise<void> {
  if (!(id > 0)) return;
  await runInTransaction([OUTBOX_STORE], "readwrite", (transaction) => {
    transaction.objectStore(OUTBOX_STORE).delete(id);
  });
}

async function outboxWithinBudget(userID: number, incoming: OutboxRecord): Promise<boolean> {
  const existing = await listOutboxItems(userID);
  if (existing.length + 1 > maxOutboxItemsPerUser) return false;
  const incomingBytes = incoming.attachments.reduce((sum, attachment) => sum + (attachment.bytes?.byteLength || 0), 0);
  const totalBytes = existing.reduce((sum, item) =>
    sum + item.attachments.reduce((inner, attachment) => inner + (attachment.bytes?.byteLength || 0), 0), 0);
  return totalBytes + incomingBytes <= maxOutboxAttachmentBytes;
}

// --- helpers ----------------------------------------------------------------

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
