/**
 * A turn's token usage in its view (#1227): while it runs, the sum of its own
 * model calls (`usage`); once its run ended, the host's `usage_total`, which
 * also counts compaction summaries and sub-agents (what the gateway logged
 * for the turn's execution id). A thread's total is the sum of its turns'.
 */
import type { UsageCounts, UsagePayload, UsageTotalPayload } from '@/shared/desktop/turnUsage';

export interface TurnUsage extends UsageCounts {
  /** The sub-agents' share (only from `usage_total`). */
  subagents?: UsageCounts;
  /** `usage_total` arrived: these are the turn's final figures. */
  final: boolean;
  /** The model of the turn's latest call. */
  model?: string;
}

const NONE: UsageCounts = { prompt: 0, completion: 0, cached: 0, cache_write: 0, reasoning: 0, calls: 0 };

/** A running turn's usage after one more call; a settled turn's stays. */
export function addCall(usage: TurnUsage | undefined, call: UsagePayload): TurnUsage {
  if (usage?.final === true) return usage;
  const base = usage ?? { ...NONE, final: false };
  return {
    prompt: base.prompt + call.prompt_tokens,
    completion: base.completion + call.completion_tokens,
    cached: base.cached + (call.cached_tokens ?? 0),
    cache_write: base.cache_write + (call.cache_write_tokens ?? 0),
    reasoning: base.reasoning + (call.reasoning_tokens ?? 0),
    calls: base.calls + 1,
    final: false,
    ...((call.model ?? base.model) === undefined ? {} : { model: call.model ?? base.model }),
  };
}

/** The turn's final usage, from `usage_total`. */
export function settle(usage: TurnUsage | undefined, total: UsageTotalPayload): TurnUsage {
  const { subagents, ...counts } = total;
  return { ...NONE, ...counts, final: true, ...(subagents === undefined ? {} : { subagents }), ...(usage?.model === undefined ? {} : { model: usage.model }) };
}

/** The sum of some turns' usage; `undefined` when none reported any. */
export function sumUsage(all: readonly (UsageCounts | undefined)[]): UsageCounts | undefined {
  const present = all.filter((usage): usage is UsageCounts => usage !== undefined);
  if (present.length === 0) return undefined;
  return present.reduce<UsageCounts>(
    (sum, usage) => ({
      prompt: sum.prompt + usage.prompt,
      completion: sum.completion + usage.completion,
      cached: sum.cached + usage.cached,
      cache_write: sum.cache_write + usage.cache_write,
      reasoning: sum.reasoning + usage.reasoning,
      calls: sum.calls + usage.calls,
    }),
    NONE,
  );
}
