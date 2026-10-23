/**
 * A local turn's token usage as the desktop host reports it (#1227; IPC.md
 * "Events"): `usage` per model call of the turn's own, `usage_total` once its
 * run ended. Tokens only: a cost is the catalogue's price times these, never
 * a figure the host or the client invents.
 */
import type { ContextMeter } from './contextMeter';

/** One of the turn's own model calls, as the gateway reported it. `cached_tokens` and `cache_write_tokens` are parts of `prompt_tokens`; `reasoning_tokens` of `completion_tokens`. */
export interface UsagePayload {
  model?: string;
  /** The call's place in the turn, from 1. */
  step?: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  cached_tokens?: number;
  cache_write_tokens?: number;
  reasoning_tokens?: number;
  /** The call's request occupancy (the composer's context meter). */
  context?: ContextMeter;
}

/** What a set of model calls added up to. */
export interface UsageCounts {
  prompt: number;
  completion: number;
  cached: number;
  cache_write: number;
  reasoning: number;
  calls: number;
}

/** Every model call on the turn's execution id (its steps, compaction summaries and sub-agents); `subagents` is the sub-agents' share. */
export interface UsageTotalPayload extends UsageCounts {
  subagents?: UsageCounts;
}
