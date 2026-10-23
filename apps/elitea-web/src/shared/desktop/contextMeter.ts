/**
 * The composer's context meter as the desktop host reports it (IPC.md
 * "Context window"): on `usage` and `context` agent events.
 */
/** A model call's request occupancy, as the host measured it before sending (IPC.md "Context window"): estimates, never content. */
export interface ContextMeter {
  used_tokens: number;
  /** The window the budget uses: the model's, at most 272k. */
  window_tokens: number;
  phase: 'measured' | 'compacting' | 'compacted';
  /** The turn compacted the context. */
  compacted: boolean;
  /** When it last did (RFC 3339). */
  compacted_at?: string;
  /** The catalogue did not name the model's window: the host's 128k default. */
  window_fallback: boolean;
}
