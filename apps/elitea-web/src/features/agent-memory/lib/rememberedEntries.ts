/**
 * The agent's `remember` calls in one answer, read for the "remembered" chip
 * (ADR-0050 decision 1b, issue 1608).
 *
 * The worker's `remember` tool (services/elitea-worker-rust/src/agents/
 * agent_memory_tools.rs) takes `{kind, content, about_user?}` and answers
 * `{status: "active" | "pending", entry_id, note}`; a refusal answers
 * `{status: "error", ...}` and saved nothing, so it has no chip. A stored
 * output may arrive as an object, as JSON text, or JSON-encoded twice (the
 * native runtime persists a string output JSON-encoded), so each layer is
 * decoded until an object comes out.
 */
import { ToolActionStatus } from '@/shared/lib/chat';

const REMEMBER_TOOL_NAME = 'remember';

/** The tool-action fields read here (a `ToolActionDraft`). */
export interface RememberToolAction {
  readonly id?: unknown;
  readonly name?: unknown;
  readonly status?: unknown;
  readonly toolInputs?: unknown;
  readonly toolOutputs?: unknown;
  readonly content?: unknown;
  readonly traceStepId?: number;
  readonly traceMessageGroupId?: number;
}

export interface RememberedEntry {
  readonly entryId: number;
  /** What the agent saved, as it called the tool. */
  readonly content: string;
  /** `pending` waits for a person's approval before it is recalled. */
  readonly status: 'active' | 'pending';
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

/** The completed `remember` calls of an answer, in call order. */
export function rememberActions<T extends RememberToolAction>(toolActions: readonly T[] | null | undefined): readonly T[] {
  if (toolActions === null || toolActions === undefined) return [];
  return toolActions.filter((action) => action.name === REMEMBER_TOOL_NAME && action.status === ToolActionStatus.complete);
}

/**
 * The entry one call saved, or `undefined` when it saved none (a refusal, an
 * output this client cannot read, an id that is not a positive integer).
 */
export function rememberedEntryOf(toolInputs: unknown, toolOutputs: unknown): RememberedEntry | undefined {
  const output = objectOf(toolOutputs);
  const status = output?.['status'];
  const rawId = output?.['entry_id'];
  if (status !== 'active' && status !== 'pending') return undefined;
  if (typeof rawId !== 'string' && typeof rawId !== 'number') return undefined;
  if (!/^[1-9]\d{0,14}$/.test(String(rawId))) return undefined;
  const content = objectOf(toolInputs)?.['content'];
  return { entryId: Number(rawId), status, content: typeof content === 'string' ? content.trim() : '' };
}
