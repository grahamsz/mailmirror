// File overview: Limited local search over the offline corpus. Query parsing
// and row scoring are pure and exported for testing; only corpus loading
// touches storage. It scans cached header rows plus extracted body text from
// opened messages, requires every plain term to match, and ranks field hits
// with a recency nudge. Search operators (in:, after:, lang:, ...) are
// ignored, not interpreted.

import type { Conversation } from "../types";
import { getCachedThreadMetas, getRecentConversations } from "./offlineMailCache";

export type OfflineSearchPage = {
  conversations: Conversation[];
  page: number;
  has_prev: boolean;
  has_next: boolean;
  total: number;
};

const defaultPageSize = 50;
const maxTerms = 8;
const minTermLength = 2;

/** Field weights for ranking; subject hits outrank body text hits. */
const fieldWeights = {
  subject: 6,
  participants: 4,
  snippet: 2,
  attachments: 2,
  body: 1.5,
  freshBonus: 1,
  freshDays: 2
} as const;

/** offlineSearch runs a best-effort AND match over recently viewed mail. */
export async function offlineSearch(userID: number, query: string, page: number, pageSize = defaultPageSize): Promise<OfflineSearchPage> {
  const terms = parseQueryTerms(query);
  if (!(userID > 0) || terms.length === 0 || !(Number.isInteger(page) && page > 0)) {
    return { conversations: [], page, has_prev: false, has_next: false, total: 0 };
  }
  const [headers, bodies] = await Promise.all([
    getRecentConversations(userID),
    getCachedThreadMetas(userID)
  ]);
  const scored = scoreOfflineCorpus(headers, bodies, terms);
  scored.sort((left, right) => right.score - left.score ||
    String(right.conversation.message.date || "").localeCompare(String(left.conversation.message.date || "")));

  const start = (page - 1) * pageSize;
  const conversations = scored.slice(start, start + pageSize).map((row) => ({
    ...row.conversation,
    match_terms: Array.from(row.matchedTerms)
  }));
  return {
    conversations,
    page,
    has_prev: false,
    has_next: start + conversations.length < scored.length,
    total: scored.length
  };
}

/**
 * scoreOfflineCorpus scores every header row against the terms, requiring an
 * AND match, and synthesizes rows for cached bodies that lack a header row.
 * Exported pure so ranking rules can be tested without IndexedDB.
 */
export function scoreOfflineCorpus(
  headers: Conversation[],
  bodies: Array<{ thread_ids: number[]; search_text: string; subject?: string; from_addr?: string; date?: string }>,
  terms: string[]
): Array<{ conversation: Conversation; score: number; matchedTerms: Set<string> }> {
  const bodyTextByMessageID = new Map<number, string>();
  for (const body of bodies) {
    for (const id of body.thread_ids) bodyTextByMessageID.set(id, body.search_text);
  }

  type ScoredRow = { conversation: Conversation; score: number; matchedTerms: Set<string> };
  const scored: ScoredRow[] = [];
  const coveredBodies = new Set<number>();
  for (const conversation of headers) {
    const row = scoreConversationRow(conversation, terms, bodyTextByMessageID);
    if (!row) continue;
    scored.push(row);
    coveredBodies.add(conversation.message.id);
    for (const id of conversation.message_ids || []) coveredBodies.add(id);
  }
  // A body can be cached without a header row (oversized payload pruning,
  // partial writes); synthesize a minimal row so those messages stay searchable.
  for (const body of bodies) {
    if (body.thread_ids.some((id) => coveredBodies.has(id))) continue;
    const row = scoreConversationRow(synthesizedConversation(body), terms, bodyTextByMessageID);
    if (!row) continue;
    scored.push(row);
    for (const id of body.thread_ids) coveredBodies.add(id);
  }
  return scored;
}

function scoreConversationRow(
  conversation: Conversation,
  terms: string[],
  bodyTextByMessageID: Map<number, string>
): { conversation: Conversation; score: number; matchedTerms: Set<string> } | null {
  const haystack = buildHaystack(conversation, bodyTextByMessageID);
  let score = 0;
  const matchedTerms = new Set<string>();
  for (const term of terms) {
    let termScore = 0;
    if (haystack.subject.includes(term)) termScore = Math.max(termScore, fieldWeights.subject);
    if (haystack.participants.includes(term)) termScore = Math.max(termScore, fieldWeights.participants);
    if (haystack.snippet.includes(term)) termScore = Math.max(termScore, fieldWeights.snippet);
    if (haystack.attachments.includes(term)) termScore = Math.max(termScore, fieldWeights.attachments);
    if (termScore === 0 && haystack.body.includes(term)) termScore = fieldWeights.body;
    if (termScore === 0) return null;
    matchedTerms.add(term);
    score += termScore;
  }
  if (messageAgeDays(conversation.message.date) < fieldWeights.freshDays) score += fieldWeights.freshBonus;
  return { conversation, score, matchedTerms };
}

function buildHaystack(conversation: Conversation, bodyTextByMessageID: Map<number, string>): {
  subject: string;
  participants: string;
  snippet: string;
  attachments: string;
  body: string;
} {
  let body = "";
  const ids = new Set<number>([conversation.message.id, ...(conversation.message_ids || [])]);
  for (const id of ids) {
    const text = bodyTextByMessageID.get(id);
    if (text) body = body ? `${body} ${text}` : text;
  }
  return {
    subject: String(conversation.message.subject || "").toLowerCase(),
    participants: `${conversation.message.from_addr} ${conversation.message.to_addr} ${conversation.message.cc_addr} ${conversation.participants} ${conversation.recipient_participants}`.toLowerCase(),
    snippet: String(conversation.snippet || "").toLowerCase(),
    attachments: (conversation.attachment_names || []).join(" ").toLowerCase(),
    body: body.toLowerCase()
  };
}

function synthesizedConversation(body: { thread_ids: number[]; subject?: string; from_addr?: string; date?: string; search_text: string }): Conversation {
  const anchor = body.thread_ids[0] || 0;
  const snippet = body.search_text.slice(0, 140);
  return {
    message: {
      id: anchor,
      account_id: 0,
      mailbox_id: 0,
      subject: String(body.subject || ""),
      from_addr: String(body.from_addr || ""),
      to_addr: "",
      cc_addr: "",
      date: String(body.date || ""),
      date_short: "",
      is_read: true,
      is_starred: false,
      has_attachments: false,
      is_encrypted: false,
      is_signed: false,
      snippet
    },
    message_ids: [...body.thread_ids],
    starred_message_id: anchor,
    participants: String(body.from_addr || ""),
    recipient_participants: "",
    count: Math.max(1, body.thread_ids.length),
    is_read: true,
    has_attachments: false,
    snippet
  };
}

/** parseQueryTerms extracts lowercase plain tokens, dropping operators and noise. */
export function parseQueryTerms(query: string): string[] {
  const seen = new Set<string>();
  const terms: string[] = [];
  for (const raw of query.toLowerCase().split(/\s+/)) {
    const token = raw.replace(/^["']+|["']+$/g, "");
    if (!token || token.includes(":") || token.startsWith("-") || token.length < minTermLength) continue;
    if (seen.has(token)) continue;
    seen.add(token);
    terms.push(token);
    if (terms.length >= maxTerms) break;
  }
  return terms;
}

function messageAgeDays(date: string): number {
  const parsed = Date.parse(date);
  if (!Number.isFinite(parsed)) return Number.MAX_SAFE_INTEGER;
  return (Date.now() - parsed) / 86_400_000;
}
