/**
 * `meta.context` — the version's conversation history block (ADR-0050
 * decision 9): which messages of a shared conversation the agent sees. The
 * server validates it on save (422 `context_block_invalid`).
 */

/** `meta.context.history`. */
export type ContextHistoryMode = 'full' | 'since_joined' | 'addressed_only' | 'none';
/** `meta.context.include_side_talk`: side talk the agent is a party to, or none. */
export type ContextSideTalk = 'party' | 'none';

/** The block as the field edits it. `null` is "no block": every message, side talk it is a party to. */
export interface ContextBlock {
  readonly history: ContextHistoryMode;
  readonly include_side_talk: ContextSideTalk;
  readonly activity_notes?: boolean;
}

export const CONTEXT_HISTORY_MODES: readonly ContextHistoryMode[] = ['full', 'since_joined', 'addressed_only', 'none'];

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

/** The block a save sends: `null` (clear the key) for the default, so an agent nobody changed keeps no block. */
export function contextBlockForSave(block: ContextBlock | null): ContextBlock | null {
  if (block === null) return null;
  const isDefault = block.history === 'full' && block.include_side_talk === 'party' && block.activity_notes !== true;
  return isDefault ? null : block;
}
