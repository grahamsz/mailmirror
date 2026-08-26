// Tests for the offline outbox against a real IndexedDB implementation, with
// api.send mocked so flush policy is observable: oldest-first replay, network
// failures stop quietly, server rejections mark items failed, and 401/403
// halt the run entirely.

// @vitest-environment jsdom
import "fake-indexeddb/auto";
import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import type { ComposeForm } from "../types";
import { ApiError, api } from "../api";
import {
  discardQueuedSend,
  enqueueOfflineSend,
  flushOutbox,
  getOutboxSnapshot,
  listOutboxForUser,
  refreshOutboxSnapshot,
  retryQueuedSend,
  startAutoFlush,
  subscribeOutbox
} from "./offlineOutbox";

vi.mock("../api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api")>();
  return { ...actual, api: { ...actual.api, send: vi.fn() } };
});

const mockedSend = vi.mocked(api.send);

const USER = 12;

function formFor(subject: string): ComposeForm {
  return {
    to: "dest@example.test",
    cc: "",
    bcc: "",
    subject,
    body: `body of ${subject}`,
    body_html: `<p>body of ${subject}</p>`,
    draft_message_id: 0,
    in_reply_to_id: 0,
    from_identity_id: 1
  };
}

function fileFor(text: string): File {
  return new File([new TextEncoder().encode(text)], "notes.txt", { type: "text/plain" });
}

async function resetStores(): Promise<void> {
  const { OUTBOX_STORE, runInTransaction } = await import("./offlineDb");
  await runInTransaction([OUTBOX_STORE], "readwrite", (transaction) => {
    transaction.objectStore(OUTBOX_STORE).clear();
  });
}

beforeEach(async () => {
  mockedSend.mockReset();
  mockedSend.mockResolvedValue({ ok: true, message_id: 1 });
  // Restore connectivity in case a prior run left it overridden.
  Object.defineProperty(navigator, "onLine", { configurable: true, value: true });
  await refreshOutboxSnapshot(USER);
  await resetStores();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("enqueueOfflineSend", () => {
  it("stores the prepared payload plus copied attachment bytes", async () => {
    const queued = await enqueueOfflineSend(USER, formFor("With files"), [
      { field: "attach-1", filename: "notes.txt", content_type: "text/plain", content_id: "", inline: false, size: 5, file: fileFor("hello") }
    ]);

    expect(queued).toBe(true);
    const [item] = await listOutboxForUser(USER);
    expect(item.status).toBe("queued");
    expect(item.subject).toBe("With files");
    expect(new TextDecoder().decode(item.attachments[0].bytes)).toBe("hello");
  });

  it("rejects invalid users", async () => {
    expect(await enqueueOfflineSend(0, formFor("nope"))).toBe(false);
  });

  it("enforces the per-user queue cap", async () => {
    for (let index = 0; index < 25; index += 1) {
      expect(await enqueueOfflineSend(USER, formFor(`msg ${index}`))).toBe(true);
    }
    expect(await enqueueOfflineSend(USER, formFor("one too many"))).toBe(false);
    expect(await listOutboxForUser(USER)).toHaveLength(25);
  });

  it("notifies snapshot subscribers when the queue changes", async () => {
    const seen: Array<{ queued: number; failed: number }> = [];
    const unsubscribe = subscribeOutbox(() => seen.push({ ...getOutboxSnapshot() }));
    expect(await enqueueOfflineSend(USER, formFor("watch me"))).toBe(true);
    // Force the authoritative recount so subscribers observe settled state,
    // not just the optimistic publish computed from a possibly stale counter.
    await refreshOutboxSnapshot(USER);
    unsubscribe();

    const storedCount = (await listOutboxForUser(USER)).length;
    expect(storedCount).toBeGreaterThan(0);
    expect(seen.some((entry) => entry.queued === storedCount)).toBe(true);
  });
});

describe("flushOutbox", () => {
  it("replays queued sends oldest-first and removes delivered items", async () => {
    await enqueueOfflineSend(USER, formFor("first"));
    await new Promise((resolve) => setTimeout(resolve, 5));
    await enqueueOfflineSend(USER, formFor("second"));

    const sentSubjects: string[] = [];
    mockedSend.mockImplementation(async (_csrf, form) => {
      sentSubjects.push(form.subject);
      return { ok: true, message_id: 5 };
    });

    const result = await flushOutbox("csrf-token", USER);
    expect(result).toBe("idle");
    expect(sentSubjects).toEqual(["first", "second"]);
    expect(mockedSend).toHaveBeenCalledWith("csrf-token", expect.anything(), expect.anything());
    expect(await listOutboxForUser(USER)).toHaveLength(0);
    expect(getOutboxSnapshot()).toEqual({ queued: 0, failed: 0 });
  });

  it("rebuilds attachments from stored bytes for multipart sends", async () => {
    await enqueueOfflineSend(USER, formFor("attachment"), [
      { field: "attach-1", filename: "data.bin", content_type: "application/octet-stream", content_id: "", inline: false, size: 4, file: fileFor("abcd") }
    ]);
    await flushOutbox("csrf-token", USER);

    expect(mockedSend).toHaveBeenCalledWith("csrf-token", expect.anything(), [
      expect.objectContaining({ field: "attach-1", filename: "data.bin", size: 4 })
    ]);
  });

  it("stops quietly at the first network failure and keeps later items queued", async () => {
    await enqueueOfflineSend(USER, formFor("will send"));
    await enqueueOfflineSend(USER, formFor("network fail"));
    await enqueueOfflineSend(USER, formFor("never attempted"));

    mockedSend.mockImplementation(async (_csrf, form) => {
      if (form.subject === "network fail") throw new TypeError("Failed to fetch");
      return { ok: true, message_id: 6 };
    });

    await flushOutbox("csrf-token", USER);

    const remaining = await listOutboxForUser(USER);
    expect(remaining.map((item) => item.subject)).toEqual(["network fail", "never attempted"]);
    expect(getOutboxSnapshot().queued).toBe(2);
  });

  it("marks items failed on server rejection but keeps processing the queue", async () => {
    await enqueueOfflineSend(USER, formFor("rejected"));
    await enqueueOfflineSend(USER, formFor("fine"));

    mockedSend.mockImplementation(async (_csrf, form) => {
      if (form.subject === "rejected") throw new ApiError(400, "invalid recipient");
      return { ok: true, message_id: 7 };
    });

    await flushOutbox("csrf-token", USER);

    const failed = (await listOutboxForUser(USER)).find((item) => item.subject === "rejected");
    expect(failed?.status).toBe("failed");
    expect(failed?.last_error).toBe("invalid recipient");
    expect(failed?.attempts).toBe(1);
    expect(getOutboxSnapshot()).toEqual({ queued: 0, failed: 1 });
  });

  it("halts entirely when the session is rejected", async () => {
    await enqueueOfflineSend(USER, formFor("unauthorized"));
    await enqueueOfflineSend(USER, formFor("still waiting"));

    mockedSend.mockRejectedValue(new ApiError(401, "session expired"));

    await flushOutbox("csrf-token", USER);

    const remaining = await listOutboxForUser(USER);
    expect(remaining.map((item) => item.status)).toEqual(["failed", "queued"]);
  });

  it("refuses to run without credentials, a user, or connectivity", async () => {
    await enqueueOfflineSend(USER, formFor("held back"));

    expect(await flushOutbox("", USER)).toBe("blocked");

    // jsdom defines onLine on the prototype, so capture-and-restore via
    // property descriptors silently no-ops; always put a truthy value back.
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    try {
      expect(await flushOutbox("csrf-token", USER)).toBe("blocked");
    } finally {
      Object.defineProperty(navigator, "onLine", { configurable: true, value: true });
    }

    expect(await listOutboxForUser(USER)).toHaveLength(1);
    expect(mockedSend).not.toHaveBeenCalled();
  });

  it("serializes concurrent flushes", async () => {
    await enqueueOfflineSend(USER, formFor("once"));
    let releaseFirst!: () => void;
    const firstGate = new Promise<void>((resolve) => {
      releaseFirst = resolve;
    });
    mockedSend.mockImplementationOnce(() => firstGate.then(() => ({ ok: true, message_id: 8 })));

    const first = flushOutbox("csrf-token", USER);
    const second = await flushOutbox("csrf-token", USER);

    expect(second).toBe("busy");
    releaseFirst();
    expect(await first).toBe("idle");
  });
});

describe("retryQueuedSend and discardQueuedSend", () => {
  it("re-arms a failed item for the next flush", async () => {
    await enqueueOfflineSend(USER, formFor("retry me"));
    mockedSend.mockRejectedValueOnce(new ApiError(422, "nope"));
    await flushOutbox("csrf-token", USER);
    const [failed] = await listOutboxForUser(USER);

    await retryQueuedSend(failed.id as number);

    const [rearmed] = await listOutboxForUser(USER);
    expect(rearmed.status).toBe("queued");
    expect(rearmed.last_error).toBe("");
    expect(getOutboxSnapshot().queued).toBe(1);
  });

  it("discards an item at the user's request", async () => {
    await enqueueOfflineSend(USER, formFor("discard me"));
    const [item] = await listOutboxForUser(USER);

    await discardQueuedSend(item.id as number);

    expect(await listOutboxForUser(USER)).toHaveLength(0);
    expect(getOutboxSnapshot()).toEqual({ queued: 0, failed: 0 });
  });
});

describe("startAutoFlush", () => {
  it("flushes on reconnect events and stops after cleanup", async () => {
    await enqueueOfflineSend(USER, formFor("auto"));
    const onSent = vi.fn();
    const cleanup = startAutoFlush("csrf-token", USER, { onSent });
    // The immediate trigger already consumed the queue.
    await vi.waitFor(() => {
      expect(mockedSend).toHaveBeenCalledTimes(1);
      expect(onSent).toHaveBeenCalledTimes(1);
    });

    await enqueueOfflineSend(USER, formFor("auto two"));
    window.dispatchEvent(new Event("online"));
    await vi.waitFor(() => expect(mockedSend).toHaveBeenCalledTimes(2));

    cleanup();
    await enqueueOfflineSend(USER, formFor("ignored"));
    window.dispatchEvent(new Event("online"));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(mockedSend).toHaveBeenCalledTimes(2);
  });
});
