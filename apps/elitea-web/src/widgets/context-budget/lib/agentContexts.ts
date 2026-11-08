/**
 * Each agent's own context meter (ADR-0050 decision 9b2, issue 1610).
 *
 * Every agent in a conversation has its own model and its own history, so
 * each has its own occupancy. The context status carries one meter per agent
 * participant in `agent_contexts`: its latest answer's measurement, or its
 * latest idle compaction's when that is newer (`idle_compacted`). The
 * participant list shows it in the agent's row; the panel below the list
 * keeps the latest answer's.
 */
import { formatNumberWithSpaces } from './contextStatus';

/** @public */
export interface AgentContextMeter {
  readonly participantId: number;
  readonly currentTokens: number;
  readonly maxTokens: number;
  /** 0..100, capped at 100. */
  readonly percentage: number;
  readonly idleCompacted: boolean;
  readonly active: boolean;
  /** `"12 000 / 128 000"`. */
  readonly tokensDisplay: string;
}

function readMeter(value: unknown): AgentContextMeter | undefined {
  if (typeof value !== 'object' || value === null) return undefined;
  const row = value as Record<string, unknown>;
  const { participant_id: id, current_tokens: current, max_tokens: max } = row;
  if (typeof id !== 'number' || !Number.isSafeInteger(id) || id <= 0) return undefined;
  if (typeof current !== 'number' || !Number.isFinite(current) || current < 0) return undefined;
  if (typeof max !== 'number' || !Number.isFinite(max) || max <= 0) return undefined;
  return {
    participantId: id,
    currentTokens: current,
    maxTokens: max,
    percentage: Math.min(100, Math.round((current / max) * 100)),
    idleCompacted: row.idle_compacted === true,
    active: row.active === true,
    tokensDisplay: `${formatNumberWithSpaces(current)} / ${formatNumberWithSpaces(max)}`,
  };
}

/** The meters keyed by participant id; empty for an older server or a malformed body. */
export function readAgentContexts(wire: unknown): ReadonlyMap<number, AgentContextMeter> {
  const meters = new Map<number, AgentContextMeter>();
  if (typeof wire !== 'object' || wire === null) return meters;
  const list = (wire as Record<string, unknown>).agent_contexts;
  if (!Array.isArray(list)) return meters;
  for (const item of list) {
    const meter = readMeter(item);
    if (meter) meters.set(meter.participantId, meter);
  }
  return meters;
}
