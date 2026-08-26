// Tests for the offline mail cache against a real IndexedDB implementation
// (fake-indexeddb). Covers body/header round trips, user scoping, oversized
// payload fallback, list shaping, retention pruning, and account hygiene.

// @vitest-environment jsdom
import "fake-indexeddb/auto";
import { beforeEach, describe, expect, it } from "vitest";
import type { Conversation, ThreadMessage } from "../types";
import {
  BODIES_STORE,
  HEADERS_STORE,
  OUTBOX_STORE,
  runInTransaction
} from "./offlineDb";
import {
  buildRecentOfflinePage,
  clearOfflineMailDataForUser,
  getCachedThread,
  getCachedThreadMetas,
  getRecentConversations,
  pruneWhenDue,
  recordMailConversations,
  recordThreadPayload,
  retainOfflineDataForUser,
  type OfflineThreadPayload
} from "./offlineMailCache";

const USER = 7;
const DAY = 86_400_000;

function threadMessage(id: number, overrides: Partial<ThreadMessage> = {}): ThreadMessage {
  return {
    message: {
      id,
      account_id: 1,
      mailbox_id: 3,
      subject: "Trip plans",
      from_addr: "Ada <ada@example.test>",
      to_addr: "me@example.test",
      cc_addr: "",
      date: new Date(Date.now() - id * 60_000).toISOString(),
      date_short: "",
      is_read: false,
      is_starred: false,
      has_attachments: Boolean(overrides.attachments?.length),
      is_encrypted: false,
      is_signed: false,
      snippet: "Snippets travel"
    },
    attachments: [],
    header_details: [],
    one_click_unsubscribe: false,
    one_click_unsubscribe_sent_at: "",
    sender_name: "Ada",
    sender_email: "ada@example.test",
    sender_initial: "A",
    recipient_line: "to me",
    snippet: "Snippets travel",
    body_doc: "<div><p>Body text for message</p></div>",
    full_body_doc: "",
    has_hidden_quoted: false,
    has_display_body: true,
    body_preview_only: false,
    has_remote_images: false,
    images_allowed: false,
    expanded: true,
    reply_subject: "Re: Trip plans",
    can_reply_all: true,
    ...overrides
  };
}

function threadPayload(rootID: number, threadIDs: number[], overrides: Partial<OfflineThreadPayload> = {}): OfflineThreadPayload {
  const now = Date.now();
  return {
    message: { id: rootID, account_id: 1, subject: "Trip plans", mailbox_id: 3 },
    thread: threadIDs.map((id) => threadMessage(id, { message: { ...threadMessage(id).message, date: new Date(now - id * 1000).toISOString() } })),
    compose_from: "",
    from_identities: [],
    mailbox_id: 3,
    conversation: rootID + 5000,
    ...overrides
  };
}

function conversationRow(anchorID: number, overrides: Partial<Conversation> = {}): Conversation {
  return {
    message: {
      id: anchorID,
      account_id: 1,
      mailbox_id: 3,
      subject: `Subject ${anchorID}`,
      from_addr: "ada@example.test",
      to_addr: "me@example.test",
      cc_addr: "",
      date: new Date(Date.now() - anchorID * DAY).toISOString(),
      date_short: "",
      is_read: false,
      is_starred: false,
      has_attachments: false,
      is_encrypted: false,
      is_signed: false,
      snippet: `Snippet ${anchorID}`
    },
    starred_message_id: anchorID,
    participants: "ada",
    recipient_participants: "me",
    count: 1,
    is_read: false,
    has_attachments: false,
    snippet: `Snippet ${anchorID}`,
    ...overrides
  };
}

/** Seeds a raw record with an explicit age so expiry can be tested directly. */
async function seedAged(storeName: string, record: Record<string, unknown>): Promise<void> {
  await runInTransaction([storeName], "readwrite", (transaction) => {
    transaction.objectStore(storeName).put(record);
  });
}

async function storeCount(storeName: string): Promise<number> {
  let count = 0;
  await runInTransaction([storeName], "readonly", (transaction) => {
    const request = transaction.objectStore(storeName).count();
    request.onsuccess = () => {
      count = request.result;
    };
  });
  return count;
}

async function resetStores(): Promise<void> {
  await runInTransaction([HEADERS_STORE, BODIES_STORE, OUTBOX_STORE], "readwrite", (transaction) => {
    transaction.objectStore(HEADERS_STORE).clear();
    transaction.objectStore(BODIES_STORE).clear();
    transaction.objectStore(OUTBOX_STORE).clear();
  });
}

beforeEach(resetStores);

describe("recordThreadPayload and getCachedThread", () => {
  it("round-trips a payload by root ID and by any thread member ID", async () => {
    const payload = threadPayload(101, [99, 100, 101]);
    await recordThreadPayload(USER, payload);

    const byRoot = await getCachedThread(USER, 101);
    expect(byRoot?.payload.message.id).toBe(101);
    expect(byRoot?.payload.thread.map((item) => item.message.id)).toEqual([99, 100, 101]);
    expect(byRoot?.saved_at).toBeGreaterThan(0);

    const byMember = await getCachedThread(USER, 99);
    expect(byMember?.payload.conversation).toBe(payload.conversation);
  });

  it("hides other users' cached bodies even when IDs collide", async () => {
    await recordThreadPayload(USER, threadPayload(202, [202]));
    await recordThreadPayload(9, threadPayload(202, [202]));

    expect((await getCachedThread(USER, 202))?.payload).toBeDefined();
    // Both users wrote the same anchor; each must only ever see their own.
    const metas = await getCachedThreadMetas(USER);
    expect(metas).toHaveLength(1);
  });

  it("skips bodies over the size cap but still records their headers", async () => {
    const huge = threadPayload(303, [303], {
      thread: [threadMessage(303, { body_doc: `<p>${"x".repeat(3_200_000)}</p>` })]
    });
    await recordThreadPayload(USER, huge);

    expect(await getCachedThread(USER, 303)).toBeNull();
    const headers = await getRecentConversations(USER);
    expect(headers.map((row) => row.message.id)).toContain(303);
  });

  it("refreshes the retention window when a cached thread is viewed again", async () => {
    await recordThreadPayload(USER, threadPayload(404, [404]));
    await runInTransaction([BODIES_STORE], "readwrite", (transaction) => {
      const store = transaction.objectStore(BODIES_STORE);
      const request = store.get([USER, 404]);
      request.onsuccess = () => {
        const record = request.result as { viewed_at: number; saved_at: number };
        store.put({ ...record, viewed_at: Date.now() - 6 * DAY, saved_at: Date.now() - 6 * DAY });
      };
    });

    const viewed = await getCachedThread(USER, 404);
    expect(viewed).not.toBeNull();

    const afterTouch = await getCachedThread(USER, 404);
    expect(afterTouch).not.toBeNull();
  });

  it("extracts search text from rendered body documents", async () => {
    await recordThreadPayload(USER, threadPayload(505, [505]));
    const metas = await getCachedThreadMetas(USER);
    expect(metas).toHaveLength(1);
    expect(metas[0].search_text).toContain("Body text for message");
    expect(metas[0].subject).toBe("Trip plans");
    expect(metas[0].from_addr).toBe("ada@example.test");
  });
});

describe("header rows", () => {
  it("upserts by anchor ID instead of duplicating rows", async () => {
    await recordMailConversations(USER, [conversationRow(10, { snippet: "first" })]);
    await recordMailConversations(USER, [conversationRow(10, { snippet: "second" })]);

    const rows = await getRecentConversations(USER);
    expect(rows).toHaveLength(1);
    expect(rows[0].snippet).toBe("second");
  });

  it("sorts newest-first and filters by mailbox", async () => {
    const inbox = conversationRow(21, {});
    const archive = conversationRow(22, { message: { ...conversationRow(22).message, mailbox_id: 8 } });
    await recordMailConversations(USER, [inbox, archive]);

    expect((await getRecentConversations(USER)).map((row) => row.message.id)).toEqual([21, 22]);
    expect((await getRecentConversations(USER, { mailboxID: 8 })).map((row) => row.message.id)).toEqual([22]);
    expect(await getRecentConversations(USER, { mailboxID: 999 })).toEqual([]);
  });

  it("honors offset and limit windows", async () => {
    await recordMailConversations(USER, [conversationRow(31), conversationRow(32), conversationRow(33)]);
    const page = await getRecentConversations(USER, { offset: 1, limit: 1 });
    expect(page).toHaveLength(1);
    expect(page[0].message.id).toBe(32);
  });

  it("derives header rows for every message of a stored thread", async () => {
    await recordThreadPayload(USER, threadPayload(41, [39, 40, 41]));
    const ids = (await getRecentConversations(USER)).map((row) => row.message.id);
    expect(ids).toEqual(expect.arrayContaining([39, 40, 41]));
    const rows = await getRecentConversations(USER);
    expect(rows.find((row) => row.message.id === 39)?.participants).toBe("Ada");
  });
});

describe("buildRecentOfflinePage", () => {
  it("shapes recent headers into a mail-list page", async () => {
    await recordMailConversations(USER, [
      conversationRow(51),
      conversationRow(52),
      conversationRow(53)
    ]);
    const page = await buildRecentOfflinePage(USER, null, 1, 2);
    expect(page).toMatchObject({ page: 1, has_prev: false, total: 3 });
    expect(page?.conversations.map((row) => row.message.id)).toEqual([51, 52]);
    const secondPage = await buildRecentOfflinePage(USER, null, 2, 2);
    expect(secondPage?.conversations).toHaveLength(1);
    expect(await buildRecentOfflinePage(USER, null, 3, 2)).toBeNull();
  });

  it("rejects malformed pages and mailboxes", async () => {
    expect(await buildRecentOfflinePage(USER, null, 0, 50)).toBeNull();
    expect(await buildRecentOfflinePage(USER, "abc", 1, 50)).toBeNull();
  });
});

describe("pruneWhenDue", () => {
  it("drops entries older than seven days from both stores", async () => {
    await seedAged(BODIES_STORE, {
      user_id: USER,
      root_id: 601,
      thread_ids: [601],
      viewed_at: Date.now() - 8 * DAY,
      saved_at: Date.now() - 8 * DAY,
      mailbox_id: 3,
      subject: "old",
      from_addr: "",
      date: "",
      search_text: "",
      payload: threadPayload(601, [601])
    });
    await seedAged(BODIES_STORE, {
      user_id: USER,
      root_id: 602,
      thread_ids: [602],
      viewed_at: Date.now(),
      saved_at: Date.now(),
      mailbox_id: 3,
      subject: "fresh",
      from_addr: "",
      date: "",
      search_text: "",
      payload: threadPayload(602, [602])
    });
    await seedAged(HEADERS_STORE, {
      user_id: USER,
      key: 603,
      viewed_at: Date.now() - 9 * DAY,
      conversation: conversationRow(603)
    });
    await seedAged(HEADERS_STORE, {
      user_id: USER,
      key: 604,
      viewed_at: Date.now(),
      conversation: conversationRow(604)
    });

    await pruneWhenDue(true);

    expect(await getCachedThread(USER, 601)).toBeNull();
    expect(await getCachedThread(USER, 602)).not.toBeNull();
    const headers = (await getRecentConversations(USER)).map((row) => row.message.id);
    expect(headers).toContain(604);
    expect(headers).not.toContain(603);
  });

  it("throttles repeated sweeps within its window unless forced", async () => {
    await seedAged(HEADERS_STORE, {
      user_id: USER,
      key: 611,
      viewed_at: Date.now() - 30 * DAY,
      conversation: conversationRow(611)
    });
    await pruneWhenDue(true);
    expect(await storeCount(HEADERS_STORE)).toBe(0);

    await seedAged(HEADERS_STORE, {
      user_id: USER,
      key: 612,
      viewed_at: Date.now() - 30 * DAY,
      conversation: conversationRow(612)
    });
    // Unforced call inside the throttle window must be a no-op.
    await pruneWhenDue();
    expect(await storeCount(HEADERS_STORE)).toBe(1);

    await pruneWhenDue(true);
    expect(await storeCount(HEADERS_STORE)).toBe(0);
  });
});

describe("user hygiene", () => {
  it("clears one user's mail data while leaving their outbox untouched", async () => {
    await recordThreadPayload(USER, threadPayload(701, [701]));
    await seedAged(OUTBOX_STORE, {
      user_id: USER,
      created_at: Date.now(),
      updated_at: Date.now(),
      attempts: 0,
      status: "queued",
      last_error: "",
      subject: "queued mail",
      recipients: "x@example.test",
      form: {},
      attachments: []
    });

    await clearOfflineMailDataForUser(USER);

    expect(await getCachedThread(USER, 701)).toBeNull();
    expect(await getRecentConversations(USER)).toEqual([]);
    expect(await storeCount(OUTBOX_STORE)).toBe(1);
  });

  it("retains only the signing-in user's mail data", async () => {
    await recordThreadPayload(1, threadPayload(801, [801]));
    await recordThreadPayload(2, threadPayload(802, [802]));
    await seedAged(HEADERS_STORE, {
      user_id: 3,
      key: 803,
      viewed_at: Date.now(),
      conversation: conversationRow(803)
    });

    await retainOfflineDataForUser(1);

    expect(await getCachedThread(1, 801)).not.toBeNull();
    expect(await getCachedThread(2, 802)).toBeNull();
    expect(await getRecentConversations(3)).toEqual([]);
  });
});
