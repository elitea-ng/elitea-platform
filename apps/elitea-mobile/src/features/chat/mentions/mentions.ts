/**
 * Composer mentions, pure (no React): the trigger under the caret, the text
 * edit a pick makes, the mentions a finished message carries, and the spans a
 * bubble highlights.
 *
 * `deriveMentions` follows the web client's rule (elitea-web
 * `useMentionDetection`, written afresh here): at every `@`, the LONGEST
 * candidate name that the text continues with, case-insensitively, counts
 * when it is followed by the end of the text, whitespace or `.,!?;:`. So
 * "@Anna" is Anna even when "Ann" is also a person, and "@Annabel" is nobody.
 * Mentions are re-derived from the final text on send, so an edit after a
 * pick (deleting a name, typing one by hand) is always what is sent.
 */

/** The characters that open a suggestion strip. */
export type TriggerChar = '@' | '#' | '~' | '/';

export interface ActiveTrigger {
  trigger: TriggerChar;
  /** What was typed after the trigger, up to the caret. */
  query: string;
  /** Index of the trigger character. */
  start: number;
  /** The caret (exclusive end of the token). */
  end: number;
}

export interface Selection {
  start: number;
  end: number;
}

const WHITESPACE = /\s/;
/** The characters that may follow a complete mention. */
const MENTION_END = /[\s.,!?;:]/;
/** Longest query kept open: past this the strip closes (nobody types a 60-char name). */
const MAX_QUERY = 60;

/**
 * The trigger the caret is in, if any: a trigger character at the start of
 * the text or after whitespace, followed (up to the caret) by no whitespace.
 * A non-collapsed selection has no trigger. `/` additionally allows one inner
 * `/` (`/Toolkit/tool`), so the second phase keeps the same token.
 */
export function detectTrigger(
  text: string,
  selection: Selection,
  triggers: readonly TriggerChar[],
): ActiveTrigger | null {
  if (selection.start !== selection.end) return null;
  const caret = Math.min(Math.max(selection.end, 0), text.length);
  for (let i = caret - 1; i >= 0 && caret - i <= MAX_QUERY + 1; i--) {
    const char = text[i];
    if (WHITESPACE.test(char)) return null;
    if (!(triggers as readonly string[]).includes(char)) continue;
    if (i > 0 && !WHITESPACE.test(text[i - 1])) {
      // `/` inside a `/Toolkit/` token: keep scanning back to its opening slash.
      if (char === '/' && triggers.includes('/')) continue;
      return null;
    }
    const query = text.slice(i + 1, caret);
    if (char !== '/' && query.includes(char)) return null;
    if (char === '/' && (query.match(/\//g) ?? []).length > 1) return null;
    return { trigger: char as TriggerChar, query, start: i, end: caret };
  }
  return null;
}

/** Replaces `[start, end)` with `insert`; returns the new text and the caret after the insert. */
export function replaceRange(text: string, start: number, end: number, insert: string): { text: string; caret: number } {
  const from = Math.max(0, Math.min(start, text.length));
  const to = Math.max(from, Math.min(end, text.length));
  return { text: text.slice(0, from) + insert + text.slice(to), caret: from + insert.length };
}

/** A person a message can mention. */
export interface MentionCandidate {
  /** USER id (`entity_meta.id`), never a participant row id. */
  userId: number;
  name: string;
}

/** The display name of the "everyone" entry ("@Everyone"). */
export const EVERYONE = 'Everyone';

export interface DerivedMentions {
  userIds: number[];
  everyone: boolean;
}

interface NameMatch {
  name: string;
  /** The candidate, or null for Everyone. */
  candidate: MentionCandidate | null;
}

function lower(value: string): string {
  return value.toLocaleLowerCase();
}

/** Names sorted longest first (so "Anna" wins over "Ann"). */
function sortedNames(candidates: readonly MentionCandidate[], withEveryone: boolean): NameMatch[] {
  const names: NameMatch[] = candidates
    .filter((c) => typeof c.name === 'string' && c.name.trim() !== '')
    .map((c) => ({ name: c.name, candidate: c }));
  if (withEveryone) names.push({ name: EVERYONE, candidate: null });
  return names.sort((a, b) => b.name.length - a.name.length);
}

/** The longest name `rest` begins with that ends at a word boundary, or null. */
function matchAt(rest: string, names: readonly NameMatch[]): NameMatch | null {
  const restLower = lower(rest);
  for (const entry of names) {
    const nameLower = lower(entry.name);
    if (!restLower.startsWith(nameLower)) continue;
    const after = rest[entry.name.length];
    if (after === undefined || MENTION_END.test(after)) return entry;
  }
  return null;
}

export interface MentionSpan {
  start: number;
  end: number;
  match: NameMatch;
}

/** Every `@Name` in `text` that names a candidate (or Everyone), left to right, non-overlapping. */
export function findMentions(
  text: string,
  candidates: readonly MentionCandidate[],
  options: { everyone?: boolean } = {},
): MentionSpan[] {
  const names = sortedNames(candidates, options.everyone !== false);
  if (names.length === 0) return [];
  const spans: MentionSpan[] = [];
  let from = 0;
  for (;;) {
    const at = text.indexOf('@', from);
    if (at < 0) break;
    const match = matchAt(text.slice(at + 1), names);
    if (match) {
      const end = at + 1 + match.name.length;
      spans.push({ start: at, end, match });
      from = end;
    } else {
      from = at + 1;
    }
  }
  return spans;
}

/** The mentions a finished message carries: deduped user ids in order, and `@Everyone`. */
export function deriveMentions(
  text: string,
  candidates: readonly MentionCandidate[],
  options: { everyone?: boolean } = {},
): DerivedMentions {
  const userIds: number[] = [];
  let everyone = false;
  for (const span of findMentions(text, candidates, options)) {
    if (span.match.candidate === null) everyone = true;
    else if (!userIds.includes(span.match.candidate.userId)) userIds.push(span.match.candidate.userId);
  }
  return { userIds, everyone };
}

export type SpanKind = 'text' | 'mention' | 'agent' | 'tool' | 'skill';

export interface TextSpan {
  kind: SpanKind;
  text: string;
}

/**
 * Splits a message into plain text and the tokens a bubble tints: `@Name`
 * (a known person or Everyone), `#Agent` (a known agent name), and the
 * text-only `~skill` and `/Toolkit/tool` tokens (at the start or after
 * whitespace). A heuristic for display only; nothing is sent from it.
 */
export function highlightSpans(
  text: string,
  names: { people?: readonly string[]; agents?: readonly string[] } = {},
): TextSpan[] {
  const marks: { start: number; end: number; kind: SpanKind }[] = [];
  const people = (names.people ?? []).map((name, i) => ({ userId: i + 1, name }));
  for (const span of findMentions(text, people)) marks.push({ start: span.start, end: span.end, kind: 'mention' });

  const agents = [...(names.agents ?? [])].filter((n) => n.trim() !== '').sort((a, b) => b.length - a.length);
  const tokenRe = /(^|\s)([#~/])(\S+)/g;
  let token: RegExpExecArray | null;
  while ((token = tokenRe.exec(text)) !== null) {
    const start = token.index + token[1].length;
    const sigil = token[2];
    let end = start + 1 + token[3].length;
    let kind: SpanKind | null = null;
    if (sigil === '#') {
      const rest = text.slice(start + 1);
      const agent = agents.find((name) => {
        if (!lower(rest).startsWith(lower(name))) return false;
        const after = rest[name.length];
        return after === undefined || MENTION_END.test(after);
      });
      if (agent) {
        kind = 'agent';
        end = start + 1 + agent.length;
      }
    } else if (sigil === '~' && /^[\p{L}\p{N}_.-]+/u.test(token[3])) {
      kind = 'skill';
      end = start + 1 + (/^[\p{L}\p{N}_.-]+/u.exec(token[3])?.[0].length ?? 0);
    } else if (sigil === '/' && /^[^/\s]+\/[^/\s]+/.test(token[3])) {
      kind = 'tool';
      end = start + 1 + (/^[^/\s]+\/[^/\s]+/.exec(token[3])?.[0].length ?? 0);
    }
    if (kind && !marks.some((m) => start < m.end && end > m.start)) marks.push({ start, end, kind });
  }

  marks.sort((a, b) => a.start - b.start);
  const out: TextSpan[] = [];
  let last = 0;
  for (const mark of marks) {
    if (mark.start > last) out.push({ kind: 'text', text: text.slice(last, mark.start) });
    out.push({ kind: mark.kind, text: text.slice(mark.start, mark.end) });
    last = mark.end;
  }
  if (last < text.length) out.push({ kind: 'text', text: text.slice(last) });
  return out;
}
