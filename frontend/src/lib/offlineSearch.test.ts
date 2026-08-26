// Tests for offline search: query-term parsing and corpus scoring are pure,
// while offlineSearch itself runs against mocked storage loaders so ranking
// and pagination stay observable without IndexedDB.

import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Conversation } from "../types";
import { offlineSearch, parseQueryTerms, scoreOfflineCorpus } from "./offlineSearch";
import { getCachedThreadMetas, getRecentConversations } from "./offlineMailCache";

vi.mock("./offlineMailCache", () => ({
  getRecentConversations: vi.fn(),
  getCachedThreadMetas: vi.fn()
}));

const mockedRecent = vi.mocked(getRecentConversations);
const mockedMetas = vi.mocked(getCachedThreadMetas);

function conversation(id: number, overrides: Partial<Conversation> = {}): Conversation {
  return {
    message: {
      id,
      account_id: 1,
      mailbox_id: 3,
      subject: "",
      from_addr: "",
      to_addr: "",
      cc_addr: "",
      date: "2026-08-20T10:00:00Z",
      date_short: "",
      is_read: false,
      is_starred: false,
      has_attachments: false,
      is_encrypted: false,
      is_signed: false,
      snippet: ""
    },
    starred_message_id: id,
    participants: "",
    recipient_participants: "",
    count: 1,
    is_read: false,
    has_attachments: false,
    snippet: "",
    ...overrides
  };
}

function daysAgoISO(days: number): string {
  return new Date(Date.now() - days * 86_400_000).toISOString();
}

describe("parseQueryTerms", () => {
  it("lowercases tokens and drops duplicates", () => {
    expect(parseQueryTerms("Hello HELLO world")).toEqual(["hello", "world"]);
  });

  it("strips surrounding quotes from tokens", () => {
    // Phrases are intentionally decomposed into an AND of tokens.
    expect(parseQueryTerms('"exact phrase"')).toEqual(["exact", "phrase"]);
  });

  it("drops operators, negations, and single characters", () => {
    expect(parseQueryTerms("in:inbox after:2020 -drafts a report")).toEqual(["report"]);
  });

  it("caps the term count", () => {
    const terms = parseQueryTerms("one two three four five six seven eight nine ten");
    expect(terms).toHaveLength(8);
    expect(terms[0]).toBe("one");
    expect(terms).not.toContain("nine");
  });

  it("returns nothing for empty or operator-only queries", () => {
    expect(parseQueryTerms("")).toEqual([]);
    expect(parseQueryTerms("in:inbox lang:de")).toEqual([]);
  });
});

describe("scoreOfflineCorpus", () => {
  it("requires every term to match somewhere (AND semantics)", () => {
    const rows = [conversation(1, { message: { ...conversation(1).message, subject: "quarterly report" } })];
    const scored = scoreOfflineCorpus(rows, [], ["quarterly", "budget"]);
    expect(scored).toHaveLength(0);

    const matched = scoreOfflineCorpus(rows, [], ["quarterly", "report"]);
    expect(matched).toHaveLength(1);
    expect(matched[0].matchedTerms).toEqual(new Set(["quarterly", "report"]));
  });

  it("scores subject matches above participant matches above body matches", () => {
    const subjectRow = conversation(1, { message: { ...conversation(1).message, subject: "alpha plan" } });
    const participantRow = conversation(2, {
      participants: "alpha",
      recipient_participants: ""
    });
    const bodyRow = conversation(3);
    const metas = [{ thread_ids: [3], search_text: "alpha details", subject: "", from_addr: "", date: "" }];
    // scoreOfflineCorpus is deliberately unsorted; offlineSearch owns ordering.
    const scoreByID = new Map(
      scoreOfflineCorpus([participantRow, subjectRow, bodyRow], metas, ["alpha"])
        .map((row) => [row.conversation.message.id, row.score])
    );
    expect(scoreByID.get(1)).toBeGreaterThan(scoreByID.get(2) as number);
    expect(scoreByID.get(2)).toBeGreaterThan(scoreByID.get(3) as number);
  });

  it("matches body text through non-anchor thread message IDs", () => {
    const row = conversation(100, { message_ids: [100, 101] });
    const metas = [{ thread_ids: [101], search_text: "the passphrase is lantern", subject: "", from_addr: "", date: "" }];
    const scored = scoreOfflineCorpus([row], metas, ["lantern"]);
    expect(scored).toHaveLength(1);
    expect(scored[0].matchedTerms.has("lantern")).toBe(true);
  });

  it("gives recent conversations a recency bonus over equally-matched older ones", () => {
    const fresh = conversation(1, { message: { ...conversation(1).message, subject: "alpha", date: daysAgoISO(1) } });
    const stale = conversation(2, { message: { ...conversation(2).message, subject: "alpha", date: daysAgoISO(30) } });
    // scoreOfflineCorpus is intentionally unsorted; offlineSearch applies the
    // ordering. Assert the bonus through the scores themselves.
    const scored = scoreOfflineCorpus([stale, fresh], [], ["alpha"]);
    const scoreByID = new Map(scored.map((row) => [row.conversation.message.id, row.score]));
    expect(scoreByID.get(1)).toBeGreaterThan(scoreByID.get(2) as number);
  });

  it("synthesizes searchable rows for cached bodies that lack a header row", () => {
    const metas = [{
      thread_ids: [9],
      search_text: "quokka sighting notes",
      subject: "",
      from_addr: "spotter@example.test",
      date: daysAgoISO(3)
    }];
    const scored = scoreOfflineCorpus([], metas, ["quokka"]);
    expect(scored).toHaveLength(1);
    expect(scored[0].conversation.message.id).toBe(9);
    expect(scored[0].conversation.participants).toBe("spotter@example.test");
  });

  it("does not duplicate rows for bodies already covered by a header row", () => {
    const covered = conversation(5, { message_ids: [5] });
    const metas = [{ thread_ids: [5], search_text: "alpha body text", subject: "", from_addr: "", date: "" }];
    const scored = scoreOfflineCorpus([covered], metas, ["alpha"]);
    expect(scored).toHaveLength(1);
  });

  it("returns an empty list for an empty corpus", () => {
    expect(scoreOfflineCorpus([], [], ["anything"])).toEqual([]);
  });
});

describe("offlineSearch", () => {
  beforeEach(() => {
    mockedRecent.mockReset();
    mockedMetas.mockReset();
    mockedRecent.mockResolvedValue([]);
    mockedMetas.mockResolvedValue([]);
  });

  it("short-circuits invalid users, pages, and queries", async () => {
    expect(await offlineSearch(0, "hello", 1)).toMatchObject({ total: 0, conversations: [] });
    expect(await offlineSearch(1, "hello", 0)).toMatchObject({ total: 0, conversations: [] });
    expect(await offlineSearch(1, "in:inbox", 1)).toMatchObject({ total: 0, conversations: [] });
  });

  it("paginates ranked results and reports totals", async () => {
    const rows = Array.from({ length: 55 }, (_, index) =>
      conversation(index + 1, { message: { ...conversation(index + 1).message, subject: "newsletter" } }));
    mockedRecent.mockResolvedValue(rows);

    const pageOne = await offlineSearch(7, "newsletter", 1);
    expect(pageOne.total).toBe(55);
    expect(pageOne.conversations).toHaveLength(50);
    expect(pageOne.has_next).toBe(true);

    const pageTwo = await offlineSearch(7, "newsletter", 2);
    expect(pageTwo.conversations).toHaveLength(5);
    expect(pageTwo.has_next).toBe(false);
  });

  it("orders final results by score, then recency", async () => {
    const subjectHit = conversation(1, { message: { ...conversation(1).message, subject: "alpha plan", date: daysAgoISO(20) } });
    const participantHit = conversation(2, { participants: "alpha" });
    const bodyHit = conversation(3);
    mockedRecent.mockResolvedValue([participantHit, subjectHit, bodyHit]);
    mockedMetas.mockResolvedValue([
      { thread_ids: [3], search_text: "alpha details", subject: "", from_addr: "", date: "" }
    ]);

    const result = await offlineSearch(7, "alpha", 1);
    expect(result.conversations.map((row) => row.message.id)).toEqual([1, 2, 3]);
  });

  it("attaches matched terms to returned conversations", async () => {
    mockedRecent.mockResolvedValue([
      conversation(11, { message: { ...conversation(11).message, subject: "invoice overdue" } })
    ]);
    const result = await offlineSearch(7, "overdue invoice", 1);
    expect(result.conversations).toHaveLength(1);
    expect(result.conversations[0].match_terms).toEqual(expect.arrayContaining(["overdue", "invoice"]));
  });
});
