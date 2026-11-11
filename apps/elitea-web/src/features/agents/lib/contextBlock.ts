/**
 * `meta.context` — the version's conversation history block (ADR-0050
 * decision 9): which messages of a shared conversation the agent sees. The
 * server validates it on save (422 `context_block_invalid`).
 */

import { CONTEXT_HISTORY_MODES, type ContextHistoryMode } from '@/shared/lib/contextHistory';

export { CONTEXT_HISTORY_MODES, type ContextHistoryMode };
/** `meta.context.include_side_talk`: side talk the agent is a party to, or none. */
export type ContextSideTalk = 'party' | 'none';

/** The block as the field edits it. `null` is "no block": every message, side talk it is a party to. */
export interface ContextBlock {
  readonly history: ContextHistoryMode;
  readonly include_side_talk: ContextSideTalk;
  readonly activity_notes?: boolean;
}

function isHistoryMode(value: unknown): value is ContextHistoryMode {
  return typeof value === 'string' && (CONTEXT_HISTORY_MODES as readonly string[]).includes(value);
}

/**
 * The stored `meta.context` as the field shows it; `null` when there is none.
 * An unknown mode shows as the default, and is left for the server to refuse
 * if it is saved unchanged.
 */
export function readContextBlock(raw: unknown): ContextBlock | null {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) return null;
  const record = raw as Record<string, unknown>;
  const notes = record['activity_notes'];
  return {
    history: isHistoryMode(record['history']) ? record['history'] : 'full',
    include_side_talk: record['include_side_talk'] === 'none' ? 'none' : 'party',
    ...(typeof notes === 'boolean' ? { activity_notes: notes } : {}),
  };
}

/**
 * Two blocks say the same thing (issue 1894): field by field, so key order and
 * an absent `activity_notes` (read as off) never mark a draft dirty.
 */
export function areContextBlocksEqual(a: ContextBlock | null, b: ContextBlock | null): boolean {
  if (a === null || b === null) return a === b;
  return a.history === b.history && a.include_side_talk === b.include_side_talk && (a.activity_notes ?? false) === (b.activity_notes ?? false);
}

/** The block a save sends: `null` (clear the key) for the default, so an agent nobody changed keeps no block. */
export function contextBlockForSave(block: ContextBlock | null): ContextBlock | null {
  if (block === null) return null;
  const isDefault = block.history === 'full' && block.include_side_talk === 'party' && block.activity_notes !== true;
  return isDefault ? null : block;
}
