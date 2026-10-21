import { parseServerTime } from '@/lib/time';

/**
 * The conversation activity status of a Chats row (client contract 1.12), with the semantics of
 * the web and the desktop lists (apps/elitea-web/src/shared/lib/conversationActivity.ts):
 *
 * - `running`: a turn of the conversation is in flight (a spinner);
 * - `needs_you`: the newest message waits for the user, a tool approval or an answer (an amber ring);
 * - `unread`: a recent answer the user has not opened (a filled check; the title is bold);
 * - `done`: a recent answer the user has read (a light check).
 *
 * A finish older than a day shows no glyph (an unread one keeps its bold title). A server before
 * 1.12 sends none of the fields: no glyph, never bold.
 */

export type ActivityGlyphKind = 'running' | 'needs_you' | 'unread' | 'done';

/** How long a finished turn keeps its check. */
export const RECENT_FINISH_MS = 24 * 60 * 60 * 1000;

/** The 1.12 fields of a list row, as any supported server sends them. */
export interface ActivityRowFields {
  activity?: { state?: unknown; since?: unknown } | null;
  last_activity_at?: unknown;
  unread?: unknown;
}

/** `running`, `needs_you` or `idle`; an unknown or absent state reads as `idle`. */
export function activityStateOf(row: ActivityRowFields): 'running' | 'needs_you' | 'idle' {
  const state = row.activity?.state;
  return state === 'running' || state === 'needs_you' ? state : 'idle';
}

/** The row's glyph, or null for none. */
export function activityGlyphOf(row: ActivityRowFields, now: number = Date.now()): ActivityGlyphKind | null {
  const state = activityStateOf(row);
  if (state !== 'idle') return state;
  const last = parseServerTime(row.last_activity_at);
  if (last === null || now - last >= RECENT_FINISH_MS) return null;
  return row.unread === true ? 'unread' : 'done';
}

/** The title is bold while the row holds an answer the user has not opened (and no turn runs). */
export function activityTitleBold(row: ActivityRowFields): boolean {
  return row.unread === true && activityStateOf(row) !== 'running';
}

/** How many rows wait for the user (a section header's "N needs you"). */
export function countNeedsYou(rows: readonly { needsYou?: boolean }[]): number {
  return rows.filter((row) => row.needsYou === true).length;
}

/** The spoken name of each glyph (read with the row's title; the glyph itself is not focusable). */
export const ACTIVITY_LABEL: Record<ActivityGlyphKind, string> = {
  running: 'Running',
  needs_you: 'Needs your input',
  unread: 'Finished, unread',
  done: 'Finished',
};

/** Said for a bold row without a glyph (an unread answer older than a day). */
export const UNREAD_LABEL = 'Unread';

/** A section header's count of rows waiting for the user. */
export function needsYouText(count: number): string {
  return `${count} needs you`;
}
