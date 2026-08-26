// File overview: Durable outbox for compose sends made while offline. This
// module owns the whole feature end to end: IndexedDB persistence of prepared
// payloads, queue bookkeeping for UI subscribers, replay policy, and the
// reconnect/wake triggers that start flushes. Sends made offline queue with
// their exact prepared payload (post-encryption) and replay oldest-first;
// server rejections are marked failed instead of retried forever.

import { api, ApiError } from "../api";
import type { ComposeAttachmentUpload, ComposeForm } from "../types";
import { isNetworkError } from "./errors";
import {
  OUTBOX_STORE,
  deleteFromStore,
  getAllFromIndex,
  getAllFromStore,
  getFromStore,
  openOfflineDB,
  putInStore,
  userKeyRange
} from "./offlineDb";

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

export type OutboxSnapshot = {
  queued: number;
  failed: number;
};

export type OutboxFlushHooks = {
  onSent?: (item: OutboxRecord) => void;
  onFailed?: (item: OutboxRecord) => void;
};

/** Queue bounds keep a lost device's exposure and IDB size predictable. */
const maxOutboxItemsPerUser = 25;
const maxOutboxAttachmentBytes = 64_000_000;
const autoFlushIntervalMS = 45_000;

const listeners = new Set<() => void>();
let snapshot: OutboxSnapshot = { queued: 0, failed: 0 };
let flushing = false;

export function subscribeOutbox(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function getOutboxSnapshot(): OutboxSnapshot {
  return snapshot;
}

function publish(next: OutboxSnapshot): void {
  snapshot = next;
  for (const listener of Array.from(listeners)) {
    try {
      listener();
    } catch {
      // A broken subscriber must not break queue bookkeeping.
    }
  }
}

async function recount(userID: number): Promise<void> {
  if (!(userID > 0)) return;
  const [queued, failed] = await Promise.all([
    countOutboxItems(userID, "queued"),
    countOutboxItems(userID, "failed")
  ]);
  if (queued !== snapshot.queued || failed !== snapshot.failed) publish({ queued, failed });
}

/** refreshOutboxSnapshot recounts stored items; call once at app start. */
export async function refreshOutboxSnapshot(userID: number): Promise<void> {
  await recount(userID);
}

/**
 * enqueueOfflineSend stores a prepared compose payload for later delivery.
 * Attachment bytes are copied into the queue so later edits cannot corrupt it.
 */
export async function enqueueOfflineSend(userID: number, form: ComposeForm, attachments: ComposeAttachmentUpload[] = []): Promise<boolean> {
  if (!(userID > 0)) return false;
  try {
    const storedAttachments = await Promise.all(attachments.map(async (attachment) => ({
      field: attachment.field,
      filename: attachment.filename,
      content_type: attachment.content_type || "application/octet-stream",
      content_id: attachment.content_id || "",
      inline: Boolean(attachment.inline),
      size: attachment.size,
      bytes: await attachment.file.arrayBuffer()
    })));
    const now = Date.now();
    const record: OutboxRecord = {
      user_id: userID,
      created_at: now,
      updated_at: now,
      attempts: 0,
      status: "queued",
      last_error: "",
      subject: String(form.subject || "(no subject)").slice(0, 200),
      recipients: String(form.to || "").slice(0, 500),
      form,
      attachments: storedAttachments
    };
    // Budget check is best-effort: two concurrent enqueues can overshoot the
    // byte cap slightly, which is harmless at these magnitudes.
    if (!(await withinQueueBudget(userID, record))) return false;
    const id = await addOutboxRecord(record);
    if (id === null) return false;
    publish({ ...snapshot, queued: snapshot.queued + 1 });
    void recount(userID);
    return true;
  } catch {
    return false;
  }
}

function addOutboxRecord(record: OutboxRecord): Promise<number | null> {
  return openOfflineDB().then((db) => new Promise<number | null>((resolve) => {
    if (!db) {
      resolve(null);
      return;
    }
    let transaction: IDBTransaction;
    try {
      transaction = db.transaction([OUTBOX_STORE], "readwrite");
    } catch {
      resolve(null);
      return;
    }
    const request = transaction.objectStore(OUTBOX_STORE).add(record);
    request.onsuccess = () => resolve(Number(request.result));
    request.onerror = () => resolve(null);
  }));
}

async function withinQueueBudget(userID: number, incoming: OutboxRecord): Promise<boolean> {
  const existing = await listOutboxForUser(userID);
  if (existing.length + 1 > maxOutboxItemsPerUser) return false;
  const incomingBytes = incoming.attachments.reduce((sum, attachment) => sum + (attachment.bytes?.byteLength || 0), 0);
  const totalBytes = existing.reduce((sum, item) =>
    sum + item.attachments.reduce((inner, attachment) => inner + (attachment.bytes?.byteLength || 0), 0), 0);
  return totalBytes + incomingBytes <= maxOutboxAttachmentBytes;
}

/** listOutboxForUser returns display-ready entries, oldest first. */
export async function listOutboxForUser(userID: number): Promise<OutboxRecord[]> {
  return sortOldestFirst(await getAllFromStore<OutboxRecord>(OUTBOX_STORE, userKeyRange(userID)));
}

async function countOutboxItems(userID: number, status?: OutboxRecord["status"]): Promise<number> {
  if (!(userID > 0)) return 0;
  if (!status) return (await listOutboxForUser(userID)).length;
  return (await listStatus(userID, status)).length;
}

/** retryQueuedSend re-arms one failed item for the next flush. */
export async function retryQueuedSend(id: number): Promise<void> {
  const item = await getFromStore<OutboxRecord>(OUTBOX_STORE, id);
  if (!item || item.status === "queued") return;
  item.status = "queued";
  item.last_error = "";
  item.updated_at = Date.now();
  await putInStore(OUTBOX_STORE, item);
  await recount(item.user_id);
}

/** discardQueuedSend removes one entry at the user's request. */
export async function discardQueuedSend(id: number): Promise<void> {
  const item = await getFromStore<OutboxRecord>(OUTBOX_STORE, id);
  if (!item) return;
  await deleteFromStore(OUTBOX_STORE, id);
  await recount(item.user_id);
}

/**
 * flushOutbox replays queued sends for the signed-in user. It stops quietly at
 * the first network failure and stops entirely when the session is rejected.
 */
export async function flushOutbox(csrf: string, userID: number, hooks?: OutboxFlushHooks): Promise<"idle" | "busy" | "blocked"> {
  if (flushing) return "busy";
  if (!(userID > 0) || !csrf) return "blocked";
  if (typeof navigator !== "undefined" && navigator.onLine === false) return "blocked";
  flushing = true;
  try {
    const items = sortOldestFirst(await listStatus(userID, "queued"));
    for (const item of items) {
      if (typeof navigator !== "undefined" && navigator.onLine === false) break;
      try {
        await api.send(csrf, item.form, rebuildAttachments(item));
        if (item.id) await deleteFromStore(OUTBOX_STORE, item.id);
        hooks?.onSent?.(item);
      } catch (err) {
        if (isNetworkError(err)) break;
        item.status = "failed";
        item.attempts += 1;
        item.last_error = err instanceof ApiError ? err.message : "The server rejected this message.";
        item.updated_at = Date.now();
        await putInStore(OUTBOX_STORE, item);
        hooks?.onFailed?.(item);
        if (err instanceof ApiError && (err.status === 401 || err.status === 403)) break;
      }
    }
  } finally {
    flushing = false;
    await recount(userID);
  }
  return "idle";
}

async function listStatus(userID: number, status: OutboxRecord["status"]): Promise<OutboxRecord[]> {
  const items = await getAllFromIndex<OutboxRecord>(
    OUTBOX_STORE,
    "user_status",
    IDBKeyRange.bound([userID, status], [userID, status])
  );
  return sortOldestFirst(items);
}

function rebuildAttachments(item: OutboxRecord): ComposeAttachmentUpload[] {
  return item.attachments.map((attachment) => ({
    field: attachment.field,
    filename: attachment.filename,
    content_type: attachment.content_type,
    content_id: attachment.content_id,
    inline: attachment.inline,
    size: attachment.size,
    file: new File([attachment.bytes], attachment.filename || "attachment", { type: attachment.content_type || "application/octet-stream" })
  }));
}

function sortOldestFirst(items: OutboxRecord[]): OutboxRecord[] {
  return [...items].sort((left, right) =>
    left.created_at - right.created_at || (left.id || 0) - (right.id || 0));
}

/**
 * startAutoFlush wires the environment triggers that keep the outbox moving:
 * immediately, on reconnect, when the app becomes visible, and on a slow
 * interval as a safety net. It returns its own cleanup function so callers
 * can stay React-only where they want to be.
 */
export function startAutoFlush(csrf: string, userID: number, hooks?: OutboxFlushHooks): () => void {
  const runFlush = () => {
    void flushOutbox(csrf, userID, hooks);
  };
  runFlush();
  window.addEventListener("online", runFlush);
  const onVisibility = () => {
    if (document.visibilityState === "visible") runFlush();
  };
  document.addEventListener("visibilitychange", onVisibility);
  const interval = window.setInterval(runFlush, autoFlushIntervalMS);
  return () => {
    window.removeEventListener("online", runFlush);
    document.removeEventListener("visibilitychange", onVisibility);
    window.clearInterval(interval);
  };
}
