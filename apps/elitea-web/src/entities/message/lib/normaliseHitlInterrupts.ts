/**
 * lib/normaliseHitlInterrupts.ts — a stored pause, read back on reload.
 *
 * Split out of `./normalise.ts` for the §3.5 file-length budget. A reloaded
 * response must render and GROUP its cards exactly like the live stream did
 * (`features/chat-messages/lib/hitlInterrupts.ts` `normalizeHitlInterrupt`),
 * so this reader keeps the same ancestry fields under the same bounds. The
 * entity layer cannot import the feature, which is why the bounds are restated
 * here; `normalise.test.ts` feeds one recorded pause through both readers and
 * asserts identical hierarchy fields.
 */
import type { AssistantMessage } from '../model/types';
import type { HitlInterruptRawWire, MessageGroupWire } from './wire';

/** The Worker's ancestry bounds (`MAX_AGENT_PATH_TIERS`, `MAX_CONTEXT_TEXT_BYTES`, the 512-byte tool identity). */
const MAX_AGENT_PATH_TIERS = 3;
const MAX_AGENT_NAME_CHARS = 2048;
const MAX_AGENT_CALL_ID_CHARS = 512;
/** `elitea.graph.parallel-interrupt.v1` is a reconcile-only summary, never a card. */
const PARALLEL_INTERRUPT_SCHEMA = 'elitea.graph.parallel-interrupt.v1';
const PARALLEL_INTERRUPT_MESSAGE = 'Parallel branches paused.';

type RawRecord = Readonly<Record<string, unknown>>;

interface StoredAgentPathTier {
  readonly name: string;
  readonly call_id: string;
  readonly sibling_ordinal?: number;
}

function text(value: unknown): string {
  return typeof value === 'string' && value.trim() ? value : '';
}

function positiveOrdinal(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isInteger(value) && value > 0 ? value : undefined;
}

/**
 * `parent_agent_path` as `[{name, call_id, sibling_ordinal?}]`, or `[]`.
 *
 * Tiers naming nothing are dropped, as live; a path past the Worker's bounds is
 * not one it sends and is dropped whole rather than cut into a different owner.
 */
function agentPathTiers(value: unknown): readonly StoredAgentPathTier[] {
  if (!Array.isArray(value)) return [];
  return value
    .filter((tier): tier is RawRecord => Boolean(tier) && typeof tier === 'object')
    .map((tier) => {
      const ordinal = positiveOrdinal(tier['sibling_ordinal']);
      return {
        name: text(tier['name']),
        call_id: text(tier['call_id']),
        ...(ordinal !== undefined ? { sibling_ordinal: ordinal } : {}),
      };
    })
    .filter((tier) => tier.name || tier.call_id);
}

function boundedAgentPath(tiers: readonly StoredAgentPathTier[]): readonly StoredAgentPathTier[] {
  if (tiers.length > MAX_AGENT_PATH_TIERS) return [];
  const bounded = tiers.every(
    (tier) => tier.name.length <= MAX_AGENT_NAME_CHARS && tier.call_id.length <= MAX_AGENT_CALL_ID_CHARS,
  );
  return bounded ? tiers : [];
}

/** Is this stored interrupt the parallel-node aggregate? It is never restored as a card. */
function isParallelAggregate(raw: RawRecord): boolean {
  const data = raw['data'];
  return (
    raw['schema'] === PARALLEL_INTERRUPT_SCHEMA ||
    (Boolean(data) && typeof data === 'object' && (data as RawRecord)['schema'] === PARALLEL_INTERRUPT_SCHEMA) ||
    (raw['message'] === PARALLEL_INTERRUPT_MESSAGE && Array.isArray(raw['cards']))
  );
}

/**
 * The ancestry the live reader keeps: owner from the path's last tier when not
 * stated — read before the bound, exactly as live does.
 */
function hierarchyFields(raw: RawRecord): Record<string, unknown> {
  const tiers = agentPathTiers(raw['parent_agent_path']);
  const path = boundedAgentPath(tiers);
  const owner = tiers[tiers.length - 1];
  const ordinal = positiveOrdinal(raw['sibling_ordinal']);
  return {
    parent_agent_name: text(raw['parent_agent_name']) || owner?.name || '',
    parent_agent_call_id: text(raw['parent_agent_call_id']) || owner?.call_id || '',
    parent_agent_path: path,
    ...(ordinal !== undefined ? { sibling_ordinal: ordinal } : {}),
  };
}

/**
 * The clarifying questions of a stored `ask_user` pause, or `[]`. Anything
 * that is not an array is dropped — the card iterates this.
 */
function hitlQuestionsField(raw: RawRecord): readonly unknown[] {
  const questions = raw['questions'];
  return Array.isArray(questions) ? questions : [];
}

/** The stored actions when they are a list of strings; otherwise the answerable default, as live. */
function storedHitlActions(value: unknown): readonly string[] {
  return Array.isArray(value) && value.every((action) => typeof action === 'string')
    ? value
    : ['approve', 'reject'];
}

/** chat.helpers.js:88-105 `buildHitlInterruptFromRaw`, the display half. Typed as the live `pickString` reads them. */
function hitlDisplayFields(wire: HitlInterruptRawWire): Record<string, unknown> {
  const message: unknown = wire.message;
  return {
    message: typeof message === 'string' && message ? message : 'Please review and take action.',
    node_name: wire.node_name || '',
    available_actions: storedHitlActions(wire.available_actions),
    routes: wire.routes || {},
    edit_state_key: wire.edit_state_key || '',
    guardrail_type: wire.guardrail_type || '',
    tool_name: wire.tool_name || '',
    toolkit_name: wire.toolkit_name || '',
  };
}

/** chat.helpers.js:88-105 `buildHitlInterruptFromRaw`, ported, plus the hierarchy the live path keeps. */
function buildHitlInterruptFromRaw(wire: HitlInterruptRawWire): Record<string, unknown> {
  const raw = wire as RawRecord;
  const childThreadId = wire.child_thread_id || '';
  return {
    ...hitlDisplayFields(wire),
    // A pause reloaded from the store must render the same controls the live
    // stream rendered. Without this the answer card came back after a refetch
    // with its question and no way to answer it.
    questions: hitlQuestionsField(raw),
    interrupt_id: text(raw['interrupt_id']),
    toolkit_type: wire.toolkit_type || '',
    action_label: wire.action_label || '',
    tool_args: wire.tool_args ?? null,
    policy_message: wire.policy_message || '',
    tool_call_id: wire.tool_call_id || '',
    child_thread_id: childThreadId,
    thread_id: wire.thread_id || '',
    resume_strategy: text(raw['resume_strategy']) || (childThreadId ? 'aggregate_child' : 'single'),
    ...hierarchyFields(raw),
  };
}

/** HITL resume-state reconstruction (convertChatConversationMessages.js:271-285) — #4823. */
export function assistantHitlFields(
  meta: MessageGroupWire['meta'],
): Partial<Pick<AssistantMessage, 'hitlInterrupt' | 'hitlInterrupts' | 'threadId'>> {
  const rawInterrupts = (Array.isArray(meta?.hitl_interrupts) ? meta.hitl_interrupts : []).filter(
    (raw) => !isParallelAggregate(raw as RawRecord),
  );
  const hitlInterrupts = rawInterrupts.length > 0 ? rawInterrupts.map(buildHitlInterruptFromRaw) : undefined;
  const single = meta?.hitl_interrupt;
  const hitlInterrupt =
    single && !isParallelAggregate(single as RawRecord) ? buildHitlInterruptFromRaw(single) : hitlInterrupts?.[0];
  return {
    ...(hitlInterrupt !== undefined ? { hitlInterrupt } : {}),
    ...(hitlInterrupts !== undefined ? { hitlInterrupts } : {}),
    ...(meta?.thread_id !== undefined ? { threadId: meta.thread_id } : {}),
  };
}
