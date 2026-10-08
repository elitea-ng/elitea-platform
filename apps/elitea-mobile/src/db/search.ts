import type { ConversationSummary } from '@/db/repo';
import type { Db } from '@/db/sql';

/**
 * Offline chat search: FTS5 over the titles and the message text the device keeps
 * (migration 3). The two indexes are tables of the encrypted store itself, kept in step
 * with `conversations` and `messages` by triggers, so whatever the sync engine, retention
 * or a tombstone writes or removes is what search finds, and wiping the store wipes them
 * (no plaintext side table or file). Works the same with the network on or off: the
 * workspace offers no conversation search (client contract 1.3/1.4), so this IS the search.
 *
 * Matching: every word the user typed must appear (in any order); the last word also
 * matches as a prefix ("plan" finds "planning"), so results follow the typing. Case and
 * diacritics are ignored (`unicode61 remove_diacritics 2`).
 */

/** One part of a matched passage: `match` parts are the words that matched. */
export interface SnippetPart {
  text: string;
  match: boolean;
}

export interface SearchHit {
  id: number;
  name: string;
  /** The row's `updated_at` (server time), for the age column. */
  updatedAt: string;
  /** The list row, when this conversation is in the synced list (null: known from a thread read only). */
  row: ConversationSummary | null;
  /** Where it matched: null for the title; the passage of a message otherwise. */
  snippet: SnippetPart[] | null;
}

/** Most words a query uses; longer input keeps the first ones. */
const MAX_TERMS = 8;
const MAX_TERM_LENGTH = 64;
const OPEN = '\u0002';
const CLOSE = '\u0003';

/**
 * The FTS5 query for what the user typed, or null when nothing searchable is left. Every
 * word is a quoted string (so `AND`, `NEAR`, `*`, `-`, `:` and quotes are text, never
 * syntax), the last one with a prefix star.
 */
export function ftsQuery(input: string): string | null {
  const terms = input
    .normalize('NFC')
    .split(/\s+/)
    .map((term) => term.replace(/"/g, '').slice(0, MAX_TERM_LENGTH))
    // A "word" of only punctuation tokenizes to nothing and would make an empty phrase.
    .filter((term) => /[\p{L}\p{N}]/u.test(term))
    .slice(0, MAX_TERMS);
  if (terms.length === 0) return null;
  return terms.map((term, i) => `"${term}"${i === terms.length - 1 ? '*' : ''}`).join(' ');
}

/** Splits an FTS5 `snippet()` (matches wrapped in OPEN/CLOSE) into parts. */
export function snippetParts(raw: string): SnippetPart[] {
  const parts: SnippetPart[] = [];
  let match = false;
  let text = '';
  const flush = () => {
    if (text !== '') parts.push({ text, match });
    text = '';
  };
  for (const ch of raw.replace(/\s+/g, ' ')) {
    if (ch === OPEN || ch === CLOSE) {
      flush();
      match = ch === OPEN;
    } else {
      text += ch;
    }
  }
  flush();
  return parts;
}

interface HitRow {
  id: number;
  name: string;
  updated_ms: number | null;
  row_json: string | null;
  snip?: string | null;
}

function toHit(row: HitRow, snippet: SnippetPart[] | null): SearchHit {
  const summary = row.row_json ? (JSON.parse(row.row_json) as ConversationSummary) : null;
  const updatedAt = summary?.updated_at ?? (row.updated_ms !== null ? new Date(row.updated_ms).toISOString() : '');
  return { id: row.id, name: summary?.name ?? row.name, updatedAt, row: summary, snippet };
}

/**
 * Conversations of a project matching `input`: title matches first, then conversations
 * whose messages match (one hit each, with its best passage); each group newest first.
 */
export async function searchConversations(db: Db, projectId: number, input: string, limit = 50): Promise<SearchHit[]> {
  const query = ftsQuery(input);
  if (query === null) return [];
  const titles = await db.all<HitRow>(
    `SELECT c.id, c.name, c.updated_ms, c.row_json
       FROM conversation_search s JOIN conversations c ON c.rowid = s.rowid
      WHERE conversation_search MATCH ? AND c.project_id = ?
      ORDER BY COALESCE(c.updated_ms, 0) DESC, c.id DESC
      LIMIT ?`,
    [query, projectId, limit],
  );
  const hits = titles.map((row) => toHit(row, null));
  const seen = new Set(hits.map((hit) => hit.id));
  if (hits.length >= limit) return hits;
  // One row per conversation (its best-ranked message), chosen in SQL before the LIMIT: a
  // window of ranked messages would let one long chat with hundreds of matches fill it and
  // hide every other chat that has the word. FTS5 refuses snippet() under a window
  // function, so the chosen messages' passages are a second, rowid-bounded read.
  const best = await db.all<HitRow & { message_rowid: number }>(
    `WITH ranked AS (
       SELECT m.rowid AS message_rowid, m.project_id, m.conversation_id, s.rank AS score,
              row_number() OVER (PARTITION BY m.conversation_id ORDER BY s.rank, m.rowid) AS n
         FROM message_search s
         JOIN messages m ON m.rowid = s.rowid
        WHERE message_search MATCH ? AND m.project_id = ?
     )
     SELECT c.id, c.name, c.updated_ms, c.row_json, ranked.message_rowid
       FROM ranked JOIN conversations c ON c.project_id = ranked.project_id AND c.id = ranked.conversation_id
      WHERE ranked.n = 1
      ORDER BY ranked.score
      LIMIT ?`,
    [query, projectId, limit + hits.length],
  );
  const snippets = new Map<number, string>();
  if (best.length > 0) {
    const passages = await db.all<{ rowid: number; snip: string | null }>(
      `SELECT rowid, snippet(message_search, 0, char(2), char(3), '…', 12) AS snip
         FROM message_search
        WHERE message_search MATCH ? AND rowid IN (${best.map(() => '?').join(', ')})`,
      [query, ...best.map((row) => row.message_rowid)],
    );
    for (const passage of passages) if (passage.snip) snippets.set(passage.rowid, passage.snip);
  }
  const messages: HitRow[] = best.map((row) => ({ ...row, snip: snippets.get(row.message_rowid) ?? null }));
  const byMessage: { hit: SearchHit; updated: number }[] = [];
  for (const row of messages) {
    if (seen.has(row.id)) continue;
    seen.add(row.id);
    byMessage.push({ hit: toHit(row, row.snip ? snippetParts(row.snip) : null), updated: row.updated_ms ?? 0 });
  }
  byMessage.sort((a, b) => b.updated - a.updated || b.hit.id - a.hit.id);
  return [...hits, ...byMessage.map((entry) => entry.hit)].slice(0, limit);
}

/** The project's pinned conversations in the synced list (client contract 1.4 `is_pinned`), newest first. */
export async function readPinnedConversationRows(db: Db, projectId: number): Promise<ConversationSummary[]> {
  const rows = await db.all<{ row_json: string }>(
    `SELECT row_json FROM conversations
      WHERE project_id = ? AND row_json IS NOT NULL AND json_valid(row_json)
        AND json_extract(row_json, '$.is_pinned') = 1
      ORDER BY COALESCE(updated_ms, 0) DESC, id DESC`,
    [projectId],
  );
  return rows.map((r) => JSON.parse(r.row_json) as ConversationSummary);
}
