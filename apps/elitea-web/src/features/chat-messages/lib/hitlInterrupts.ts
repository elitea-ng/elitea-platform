/**
 * lib/hitlInterrupts.ts — human-in-the-loop interrupt shaping (issue #93).
 *
 * Ported from `EliteaUI/src/[fsd]/features/chat/lib/helpers/hitl.helpers.js:5-53`
 * (`getInterruptIdentity`, `normalizeHitlInterrupt`) and `:93-105`
 * (`reconcileRootHitlInterrupts`, widened here to every pause shape as
 * `reconcileHitlInterrupts`). The reducer's interrupt slice needs all three: a pause can arrive as one
 * legacy top-level blob, or as N entries in `hitl_interrupts`, and the same
 * child can re-announce a pause it already reported. Everything downstream —
 * the approval buckets and `decideHitlCard`, which routes a decision by
 * `getInterruptIdentity` — reads the normalised shape, so a raw interrupt must
 * never reach state.
 *
 * `getPendingHitlMessage` and `getHitlResumeGroup` are NOT ported here: the
 * chat surface already derives the pending message itself
 * (`useChatBoxData`'s `pendingHitlMessage`), and a second implementation would
 * be a second answer to the same question.
 */
import { normalizeExecutionHierarchy, type AgentPathTier } from './executionHierarchy';

function nonEmpty(value: unknown): string {
  return typeof value === 'string' && value.trim() ? value : '';
}

/** One selectable answer of a clarifying question (`AskUserOption`, runtime-side). */
type HitlQuestionOption = {
  readonly label?: string;
  readonly description?: string;
};

/**
 * One clarifying question of an `ask_user` pause.
 *
 * The field set is the native runtime's `AskUserQuestion` as it SERIALISES it
 * (`services/elitea-worker-rust/src/agents/internal_tools.rs`): `multiSelect`
 * is renamed to camelCase by serde while `allow_other` is not, so both
 * spellings of the multi-select flag are read — the tool ARGUMENTS the model
 * emits are admitted under either name, and `tool_args` reaches the UI
 * unchanged on some paths.
 */
export type HitlQuestion = {
  readonly id?: string;
  readonly question?: string;
  readonly header?: string;
  readonly options?: readonly HitlQuestionOption[];
  readonly multiSelect?: boolean;
  readonly multi_select?: boolean;
  readonly allow_other?: boolean;
  /**
   * Whether the user may move past this question without answering it (#940
   * A8, ELITEA-2792). Absent means REQUIRED, which is what every question was
   * before the runtime grew the field — a stored pause from before it must not
   * become skippable because the key is missing.
   */
  readonly optional?: boolean;
};

/**
 * The `questions` array of an `ask_user` pause, or `[]`.
 *
 * Anything that is not an array is dropped rather than passed through: the
 * card iterates this, and a non-iterable here would take the whole message
 * down instead of the one card.
 */
function asHitlQuestions(value: unknown): readonly HitlQuestion[] {
  return Array.isArray(value) ? (value as readonly HitlQuestion[]) : [];
}

/**
 * One paused approval, in the shape the UI and the resume path both read.
 *
 * A type alias with EXACTLY these members, not an open interface: it has to be
 * assignable both to `Record<string, unknown>` (so the merge can re-derive
 * identity from an already-normalised entry) and to the renderer's
 * `ChatHitlActions.HitlInterrupt`, and an `unknown` index signature would
 * defeat the second — `decided?: boolean` cannot be satisfied by `unknown`.
 * The normaliser writes a closed set of fields, so nothing is lost by closing
 * the type; `hitlInterrupts.test.ts` pins the assignability in both directions.
 */
export type NormalizedHitlInterrupt = {
  readonly message: string;
  readonly node_name: string;
  readonly available_actions: readonly string[];
  /**
   * The clarifying questions of an `ask_user` pause; `[]` for every other
   * pause shape. Carried because the card that renders an `answer` pause has
   * no other source for them — dropping the field here left the user a
   * question with no controls under it.
   */
  readonly questions: readonly HitlQuestion[];
  readonly routes: Record<string, unknown>;
  readonly edit_state_key: string;
  readonly guardrail_type: string;
  readonly tool_name: string;
  readonly toolkit_name: string;
  readonly toolkit_type: string;
  readonly action_label: string;
  readonly tool_args: unknown;
  readonly policy_message: string;
  readonly interrupt_id: string;
  readonly tool_call_id: string;
  readonly child_thread_id: string;
  readonly thread_id: string;
  readonly resume_strategy: string;
  readonly parent_agent_name: string;
  readonly parent_agent_call_id: string;
  readonly parent_agent_path: readonly AgentPathTier[];
  /** The interrupt's own sibling ordinal, when the raw interrupt states a positive integer one. */
  readonly sibling_ordinal?: number;
  /** The decision for this card was submitted with its whole set; nothing is owed on it. */
  readonly decided?: boolean;
  /** The user decided this card; the decision waits for its siblings (`hitlDecisionQueue`). */
  readonly queued?: boolean;
  /** Not rendered. Set together with `queued` or `decided`. */
  readonly hidden?: boolean;
};

type RawInterrupt = Readonly<Record<string, unknown>>;

/**
 * The identity two announcements of the same pause share.
 *
 * `interrupt_id` when the backend supplies one; otherwise the thread/tool pair,
 * because one aggregate child can emit several approvals and they must not
 * collapse onto each other. An interrupt identifying nothing returns `''`,
 * which the merge treats as "always append" rather than "matches everything" —
 * silently overwriting a different pause would lose an approval the user still
 * owes an answer to.
 */
export function getInterruptIdentity(interrupt: RawInterrupt | undefined): string {
  const interruptId = nonEmpty(interrupt?.['interrupt_id']);
  if (interruptId) return interruptId;
  const threadId = nonEmpty(interrupt?.['child_thread_id']) || nonEmpty(interrupt?.['thread_id']);
  const toolCallId = nonEmpty(interrupt?.['tool_call_id']);
  if (!threadId && !toolCallId) return '';
  return JSON.stringify([threadId, toolCallId]);
}

/** A list of strings, or the approve/reject default — a card with no usable action would stall the run. */
function answerableActions(value: unknown): readonly string[] {
  return Array.isArray(value) && value.every((action) => typeof action === 'string')
    ? (value as readonly string[])
    : ['approve', 'reject'];
}

/**
 * Build one UI-shaped interrupt from a raw one plus an overlay.
 *
 * The overlay carries what the raw interrupt cannot know: for a fan-out child,
 * the indexer stamps the parent agent name and the child's thread into event
 * metadata, because the child does not know it is a child.
 *
 * Defaults are the baseline's, and `available_actions` defaulting to
 * approve/reject matters — an interrupt with no actions would render a card the
 * user cannot answer, stalling the run.
 */
export function normalizeHitlInterrupt(raw: RawInterrupt = {}, overlay: RawInterrupt = {}): NormalizedHitlInterrupt {
  const hierarchy = normalizeExecutionHierarchy(raw, overlay);
  const childThreadId = nonEmpty(raw['child_thread_id']) || nonEmpty(overlay['child_thread_id']);
  const threadId = nonEmpty(raw['thread_id']) || nonEmpty(overlay['thread_id']) || childThreadId;
  const pick = (field: string): unknown => raw[field] ?? overlay[field];
  const pickString = (field: string): string =>
    (typeof raw[field] === 'string' && raw[field] ? (raw[field] as string) : '') ||
    (typeof overlay[field] === 'string' && overlay[field] ? (overlay[field] as string) : '');

  const ordinal = raw['sibling_ordinal'] ?? overlay['sibling_ordinal'];
  return {
    message: pickString('message') || 'Please review and take action.',
    node_name: pickString('node_name'),
    available_actions: answerableActions(pick('available_actions')),
    questions: asHitlQuestions(pick('questions')),
    routes: (pick('routes') as Record<string, unknown> | undefined) ?? {},
    edit_state_key: pickString('edit_state_key'),
    guardrail_type: pickString('guardrail_type'),
    tool_name: pickString('tool_name'),
    toolkit_name: pickString('toolkit_name'),
    toolkit_type: pickString('toolkit_type'),
    action_label: pickString('action_label'),
    tool_args: raw['tool_args'] ?? overlay['tool_args'] ?? null,
    policy_message: pickString('policy_message'),
    interrupt_id: nonEmpty(raw['interrupt_id']) || nonEmpty(overlay['interrupt_id']),
    tool_call_id: nonEmpty(raw['tool_call_id']) || nonEmpty(overlay['tool_call_id']),
    child_thread_id: childThreadId,
    thread_id: threadId,
    // A child pause resumes on its own thread; the strategy is what tells the
    // resume path which of the two routes to take.
    resume_strategy:
      nonEmpty(raw['resume_strategy']) ||
      nonEmpty(overlay['resume_strategy']) ||
      (childThreadId ? 'aggregate_child' : 'single'),
    ...hierarchy,
    parent_agent_path: boundAgentPath(hierarchy.parent_agent_path),
    ...(isPositiveOrdinal(ordinal) ? { sibling_ordinal: ordinal } : {}),
  };
}

/**
 * The Worker's ancestry bounds (`MAX_AGENT_PATH_TIERS`, `MAX_CONTEXT_TEXT_BYTES`
 * and the 512-byte tool identity, `services/elitea-worker-rust/src/agents/events.rs`).
 * The reload path applies the same three (`entities/message/lib/normaliseHitlInterrupts.ts`),
 * so a reloaded response groups exactly like the live one.
 */
const MAX_AGENT_PATH_TIERS = 3;
const MAX_AGENT_NAME_CHARS = 2048;
const MAX_AGENT_CALL_ID_CHARS = 512;

function isPositiveOrdinal(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value > 0;
}

/** A path past the Worker's bounds is not one it sends: it is dropped, never truncated into a different owner. */
function boundAgentPath(path: readonly AgentPathTier[]): readonly AgentPathTier[] {
  if (path.length > MAX_AGENT_PATH_TIERS) return [];
  const withinBounds = path.every(
    (tier) => tier.name.length <= MAX_AGENT_NAME_CHARS && tier.call_id.length <= MAX_AGENT_CALL_ID_CHARS,
  );
  return withinBounds ? path : [];
}

/**
 * The legacy parallel-node aggregate (`elitea.graph.parallel-interrupt.v1`,
 * `services/elitea-worker-rust/src/agents/graph/parallel.rs`, message
 * "Parallel branches paused."). It summarises the member cards and is never a
 * card itself (`libs/proto/contracts/fanout-interrupt-decisions-v1.md` §
 * "reconcile-only").
 */
const PARALLEL_INTERRUPT_SCHEMA = 'elitea.graph.parallel-interrupt.v1';
const PARALLEL_INTERRUPT_MESSAGE = 'Parallel branches paused.';

function aggregateBody(raw: RawInterrupt | undefined): RawInterrupt | undefined {
  if (!raw) return undefined;
  if (raw['schema'] === PARALLEL_INTERRUPT_SCHEMA) return raw;
  const data = raw['data'];
  if (data && typeof data === 'object' && (data as RawInterrupt)['schema'] === PARALLEL_INTERRUPT_SCHEMA) {
    return data as RawInterrupt;
  }
  return raw['message'] === PARALLEL_INTERRUPT_MESSAGE && Array.isArray(raw['cards']) ? raw : undefined;
}

/** Is this raw interrupt the reconcile-only parallel aggregate? */
export function isParallelAggregateInterrupt(raw: RawInterrupt | undefined): boolean {
  return aggregateBody(raw) !== undefined;
}

/** The member interrupt ids a parallel aggregate names, or `[]`. */
export function parallelAggregateMemberIds(raw: RawInterrupt | undefined): readonly string[] {
  const cards = aggregateBody(raw)?.['cards'];
  if (!Array.isArray(cards)) return [];
  return cards
    .map((card: unknown) => (card && typeof card === 'object' ? nonEmpty((card as RawInterrupt)['interrupt_id']) : ''))
    .filter(Boolean);
}

/** A card the user still owes an answer to: neither decided nor waiting in the queue. */
function isAnswerableInterrupt(interrupt: NormalizedHitlInterrupt): boolean {
  return !interrupt.decided && !interrupt.queued && !interrupt.hidden;
}

/** The first card still owed an answer — what the singular `hitlInterrupt` mirrors. */
export function firstAnswerableInterrupt(
  interrupts: readonly NormalizedHitlInterrupt[],
): NormalizedHitlInterrupt | undefined {
  return interrupts.find(isAnswerableInterrupt);
}

/**
 * Fold a pause frame's cards into the cards already on the message.
 *
 * Port of `reconcileRootHitlInterrupts` (`hitl.helpers.js:93-105`) widened to
 * every pause shape: the Worker publishes each card as its own progress frame
 * and then one aggregate, so the list ACCUMULATES by identity. A re-announced
 * card keeps the user's local `queued`/`decided` state and stays hidden.
 *
 * Two kinds of existing entry are dropped: a `decided` entry the new frame does
 * not name (an earlier continuation consumed it), and — when a parallel
 * aggregate names its members — any entry it does not name. The aggregate only
 * ever removes; it never creates a card.
 */
export function reconcileHitlInterrupts(
  existing: readonly NormalizedHitlInterrupt[],
  incoming: readonly NormalizedHitlInterrupt[],
  memberIds: readonly string[] = [],
): readonly NormalizedHitlInterrupt[] {
  const incomingByIdentity = new Map(
    incoming.map((interrupt) => [getInterruptIdentity(interrupt), interrupt] as const).filter(([identity]) => identity),
  );
  const members = new Set(memberIds);
  const kept = existing.filter((interrupt) => {
    const identity = getInterruptIdentity(interrupt);
    if (identity && incomingByIdentity.has(identity)) return true;
    if (interrupt.decided) return false;
    return members.size === 0 || members.has(interrupt.interrupt_id);
  });
  const result = kept.map((local) => {
    const fresh = incomingByIdentity.get(getInterruptIdentity(local));
    if (!fresh) return local;
    if (!local.queued && !local.decided && !local.hidden) return fresh;
    return { ...fresh, queued: Boolean(local.queued), decided: Boolean(local.decided), hidden: true };
  });
  const present = new Set(result.map(getInterruptIdentity));
  for (const interrupt of incoming) {
    const identity = getInterruptIdentity(interrupt);
    if (identity && present.has(identity)) continue;
    result.push(interrupt);
    if (identity) present.add(identity);
  }
  return result;
}
