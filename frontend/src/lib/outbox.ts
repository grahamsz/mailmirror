// File overview: Durable outbox for compose sends made while offline. Failed
// network sends are queued with their exact prepared payload (post-encryption),
// then replayed oldest-first whenever connectivity returns. Server rejections
// are marked failed instead of retried forever.

import { api, ApiError } from "../api";
import type { ComposeAttachmentUpload, ComposeForm } from "../types";
import { isNetworkError } from "./errors";
import {
  countOutboxItems,
  deleteOutboxItem,
  enqueueOutboxItem,
  getOutboxItem,
  listOutboxItems,
  updateOutboxItem,
  type OutboxRecord
} from "./offlineStore";

export type OutboxSnapshot = {
  queued: number;
  failed: number;
};

export type OutboxFlushHooks = {
  onSent?: (item: OutboxRecord) => void;
  onFailed?: (item: OutboxRecord) => void;
};

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
 * Attachments are copied into the queue so later edits cannot corrupt it.
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
    const id = await enqueueOutboxItem(record);
    if (id === null) return false;
    publish({ ...snapshot, queued: snapshot.queued + 1 });
    void recount(userID);
    return true;
  } catch {
    return false;
  }
}

/** retryQueuedSend re-arms one failed item for the next flush. */
export async function retryQueuedSend(id: number): Promise<void> {
  const item = await getOutboxItem(id);
  if (!item || item.status === "queued") return;
  item.status = "queued";
  item.last_error = "";
  item.updated_at = Date.now();
  await updateOutboxItem(item);
  await recount(item.user_id);
}

/** discardQueuedSend removes one queue entry at the user's request. */
export async function discardQueuedSend(id: number): Promise<void> {
  const item = await getOutboxItem(id);
  if (!item) return;
  await deleteOutboxItem(id);
  await recount(item.user_id);
}

/** listOutboxForUser returns display-ready queue entries, oldest first. */
export async function listOutboxForUser(userID: number): Promise<OutboxRecord[]> {
  return listOutboxItems(userID);
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
    const items = await listOutboxItems(userID, "queued");
    for (const item of items) {
      if (typeof navigator !== "undefined" && navigator.onLine === false) break;
      const attachments = item.attachments.map((attachment) => ({
        field: attachment.field,
        filename: attachment.filename,
        content_type: attachment.content_type,
        content_id: attachment.content_id,
        inline: attachment.inline,
        size: attachment.size,
        file: new File([attachment.bytes], attachment.filename || "attachment", { type: attachment.content_type || "application/octet-stream" })
      }));
      try {
        await api.send(csrf, item.form, attachments);
        if (item.id) await deleteOutboxItem(item.id);
        hooks?.onSent?.(item);
      } catch (err) {
        if (isNetworkError(err)) break;
        item.status = "failed";
        item.attempts += 1;
        item.last_error = err instanceof ApiError ? err.message : "The server rejected this message.";
        item.updated_at = Date.now();
        await updateOutboxItem(item);
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
