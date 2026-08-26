// File overview: Limited local search over the offline corpus. It scans cached
// header rows plus extracted body text from opened messages, requires every
// plain term to match, and ranks field hits with a recency nudge. Search
// operators (in:, after:, lang:, ...) are ignored, not interpreted.

import type { Conversation } from "../types";
import { getCachedThreadMetas, getRecentConversations } from "./offlineStore";

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

/** offlineSearch runs a best-effort AND match over recently viewed mail. */
export async function offlineSearch(userID: number, query: string, page: number, pageSize = defaultPageSize): Promise<OfflineSearchPage> {
  const terms = queryTerms(query);
  if (!(userID > 0) || terms.length === 0 || !(Number.isInteger(page) && page > 0)) {
    return { conversations: [], page, has_prev: false, has_next: false, total: 0 };
  }
  const [headers, bodies] = await Promise.all([
    getRecentConversations(userID),
    getCachedThreadMetas(userID)
  ]);
  const bodyTextByMessageID = new Map<number, string>();
  for (const body of bodies) {
    for (const id of body.thread_ids) bodyTextByMessageID.set(id, body.search_text);
  }

  type ScoredRow = { conversation: Conversation; score: number; terms: Set<string> };
  const scored: ScoredRow[] = [];
  const coveredBodies = new Set<number>();
  for (const conversation of headers) {
    const row = scoreConversation(conversation, terms, bodyTextByMessageID);
    if (!row) continue;
    scored.push(row);
    for (const id of conversation.message_ids || []) coveredBodies.add(id);
    coveredBodies.add(conversation.message.id);
  }
  // A body can be cached without a header row (oversized payload pruning,
  // partial writes); synthesize a minimal row so those messages stay searchable.
  for (let index = 0; index < bodies.length; index += 1) {
    const body = bodies[index];
    if (body.thread_ids.some((id) => coveredBodies.has(id))) continue;
    if (!body.thread_ids.some((id) => bodyTextByMessageID.has(id))) continue;
    const synthesized = synthesizedConversation(body);
    const row = scoreConversation(synthesized, terms, bodyTextByMessageID);
    if (row) scored.push(row);
  }

  scored.sort((left, right) => right.score - left.score ||
    String(right.conversation.message.date || "").localeCompare(String(left.conversation.message.date || "")));
  const start = (page - 1) * pageSize;
  const conversations = scored.slice(start, start + pageSize).map((row) => {
    const next = { ...row.conversation };
    next.match_terms = Array.from(row.terms);
    return next;
  });
  return {
    conversations,
    page,
    has_prev: false,
    has_next: start + conversations.length < scored.length,
    total: scored.length
  };
}

function scoreConversation(
  conversation: Conversation,
  terms: string[],
  bodyTextByMessageID: Map<number, string>
): { conversation: Conversation; score: number; terms: Set<string> } | null {
  const subject = String(conversation.message.subject || "").toLowerCase();
  const participants = `${conversation.message.from_addr} ${conversation.message.to_addr} ${conversation.message.cc_addr} ${conversation.participants} ${conversation.recipient_participants}`.toLowerCase();
  const snippet = `${conversation.snippet}`.toLowerCase();
  const attachments = (conversation.attachment_names || []).join(" ").toLowerCase();
  let bodyText = "";
  const ids = new Set<number>([(conversation.message.id), ...(conversation.message_ids || [])]);
  for (const id of ids) {
    const text = bodyTextByMessageID.get(id);
    if (text) bodyText = bodyText ? `${bodyText} ${text}` : text;
  }
  bodyText = bodyText.toLowerCase();

  let score = 0;
  const matchedTerms = new Set<string>();
  for (const term of terms) {
    let termScore = 0;
    if (subject.includes(term)) termScore = Math.max(termScore, 6);
    if (participants.includes(term)) termScore = Math.max(termScore, 4);
    if (snippet.includes(term)) termScore = Math.max(termScore, 2);
    if (attachments.includes(term)) termScore = Math.max(termScore, 2);
    if (termScore === 0 && bodyText.includes(term)) termScore = 1.5;
    if (termScore === 0) return null;
    matchedTerms.add(term);
    score += termScore;
  }
  const ageDays = messageAgeDays(conversation.message.date);
  if (ageDays < 2) score += 1;
  return { conversation, score, terms: matchedTerms };
}

function synthesizedConversation(body: { thread_ids: number[]; subject: string; from_addr: string; date: string; search_text: string }): Conversation {
  const anchor = body.thread_ids[0] || 0;
  return {
    message: {
      id: anchor,
      account_id: 0,
      mailbox_id: 0,
      subject: body.subject,
      from_addr: body.from_addr,
      to_addr: "",
      cc_addr: "",
      date: body.date,
      date_short: "",
      is_read: true,
      is_starred: false,
      has_attachments: false,
      is_encrypted: false,
      is_signed: false,
      snippet: body.search_text.slice(0, 140)
    },
    message_ids: [...body.thread_ids],
    starred_message_id: anchor,
    participants: body.from_addr,
    recipient_participants: "",
    count: Math.max(1, body.thread_ids.length),
    is_read: true,
    has_attachments: false,
    snippet: body.search_text.slice(0, 140)
  };
}

function queryTerms(query: string): string[] {
  const seen = new Set<string>();
  const terms: string[] = [];
  for (const raw of query.toLowerCase().split(/\s+/)) {
    const token = raw.replace(/^["']+|["']+$/g, "");
    // Skip operator-style tokens; local search matches plain words only.
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
