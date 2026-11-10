/**
 * The agent's `propose_agent` calls in one answer, read for the proposal card
 * (ADR-0050 decision 5a, issue 1615).
 *
 * The worker's `propose_agent` tool (services/elitea-worker-rust/src/agents/
 * propose_agent_tool.rs) answers what elitea-main decided:
 * `{status, proposal_id?, message}`. `proposed` is a new proposal and
 * `already_proposed` the same one again (a retried call); both name the row.
 * Every other status (`proposal_limit`, `proposal_suppressed`, `not_allowed`,
 * `invalid_arguments`, ...) saved nothing, so it has no card. A stored output
 * may arrive as an object, as JSON text, or JSON-encoded twice, so each layer
 * is decoded until an object comes out.
 */
import { ToolActionStatus } from '@/shared/lib/chat';

const PROPOSE_AGENT_TOOL_NAME = 'propose_agent';

/** The tool-action fields read here (a `ToolActionDraft`). */
export interface ProposeToolAction {
  readonly id?: unknown;
  readonly name?: unknown;
  readonly status?: unknown;
  readonly toolOutputs?: unknown;
  readonly content?: unknown;
  readonly traceStepId?: number;
  readonly traceMessageGroupId?: number;
}

function objectOf(raw: unknown): Record<string, unknown> | undefined {
  let value = raw;
  for (let depth = 0; depth < 3 && typeof value === 'string'; depth += 1) {
    try {
      value = JSON.parse(value) as unknown;
    } catch {
      return undefined;
    }
  }
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined;
}

/** The completed `propose_agent` calls of an answer, in call order. */
export function proposeActions<T extends ProposeToolAction>(toolActions: readonly T[] | null | undefined): readonly T[] {
  if (toolActions === null || toolActions === undefined) return [];
  return toolActions.filter((action) => action.name === PROPOSE_AGENT_TOOL_NAME && action.status === ToolActionStatus.complete);
}

/** The proposal one call made, or `undefined` when it made none. */
export function proposalIdOf(toolOutputs: unknown): number | undefined {
  const output = objectOf(toolOutputs);
  const status = output?.['status'];
  const rawId = output?.['proposal_id'];
  if (status !== 'proposed' && status !== 'already_proposed') return undefined;
  if (typeof rawId !== 'string' && typeof rawId !== 'number') return undefined;
  if (!/^[1-9]\d{0,14}$/.test(String(rawId))) return undefined;
  return Number(rawId);
}
