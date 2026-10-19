/**
 * lib/fanoutFrames.ts — readers for the fan-out frame metadata of contract
 * `libs/proto/contracts/fanout-interrupt-decisions-v1.md` (v1.1, §3.3, §3.4,
 * §4, §8, §9, §9.1).
 *
 * The schemas under `libs/jsonschema/runtime/v1/fanout-*.schema.json` own the
 * shapes. These readers check only what the Web relies on, ignore every other
 * field, and return `undefined` for anything that does not fit: a stream is no
 * place to throw from, and a malformed frame must change nothing.
 */
import type { NormalizedHitlInterrupt } from './hitlInterrupts';
import { normalizeAgentPath, type AgentPathTier } from './executionHierarchy';

/** The frame `type`s of contract §9 the Web reads. */
export const FanoutFrameType = {
  InterruptPending: 'agent_interrupt_pending',
  HitlResolved: 'agent_hitl_resolved',
  FanoutMember: 'agent_fanout_member',
  FanoutProgress: 'agent_fanout_progress',
} as const;

/** `resume_strategy` of a card decided through the public decision API (§6). */
const LEDGER_RESUME_STRATEGY = 'interrupt_ledger';

type Raw = Readonly<Record<string, unknown>>;

const INTERRUPT_KEY = /^[0-9a-f]{64}$/u;
const MEMBER_CALL_ID = /^fo1_[0-9a-f]{64}$/u;
/** Contract §3.2: printable ASCII, 1–512 characters. */
const INTERRUPT_ID = /^[\x20-\x7e]{1,512}$/u;
const MEMBER_STATUSES = new Set(['start', 'end', 'paused', 'failed', 'cancelled']);
/** §1: the actions each interrupt kind admits. */
const KIND_ACTIONS: Readonly<Record<string, ReadonlySet<string>>> = {
  tool_guard: new Set(['approve', 'reject', 'edit', 'block_with_comment']),
  hitl_node: new Set(['approve', 'reject', 'edit', 'continue']),
  ask_user: new Set(['answer']),
  delegated_auth: new Set(['authorize', 'skip']),
};
/** §3.4: a Map has at most 64 members; Parallel at most 16. */
const MAX_MEMBER_ORDINAL = 64;
/** §9.1: at most 64 members per progress frame. */
const MAX_PROGRESS_MEMBERS = 64;

function record(value: unknown): Raw | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Raw) : undefined;
}

function text(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function ordinal(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isInteger(value) && value >= 1 && value <= MAX_MEMBER_ORDINAL ? value : undefined;
}

export function isInterruptKey(value: unknown): value is string {
  return typeof value === 'string' && INTERRUPT_KEY.test(value);
}

function isInterruptId(value: unknown): value is string {
  return typeof value === 'string' && INTERRUPT_ID.test(value);
}

/** The three hierarchy fields (§3.3). `undefined` when the coherence rule fails. */
interface MemberHierarchy {
  readonly parent_agent_name: string;
  readonly parent_agent_call_id: string;
  readonly parent_agent_path: readonly AgentPathTier[];
}

function hierarchyOf(raw: Raw): MemberHierarchy | undefined {
  const path = normalizeAgentPath(raw['parent_agent_path']);
  const name = text(raw['parent_agent_name']);
  const callId = text(raw['parent_agent_call_id']);
  if (path.length === 0 && !name && !callId) return { parent_agent_name: '', parent_agent_call_id: '', parent_agent_path: [] };
  if (path.length === 0 || path.length > 3 || !name || !callId) return undefined;
  return { parent_agent_name: name, parent_agent_call_id: callId, parent_agent_path: path };
}

/** `fanout_v1` (§3.4): grouping metadata only. */
interface FanoutMemberV1 {
  readonly kind: string;
  readonly node: string;
  readonly activation: string;
  readonly member: string;
  readonly ordinal: number;
  readonly total: number;
}

function memberV1(value: unknown): FanoutMemberV1 | undefined {
  const raw = record(value);
  if (!raw) return undefined;
  const memberOrdinal = ordinal(raw['ordinal']);
  const total = ordinal(raw['total']);
  if (memberOrdinal === undefined || total === undefined || memberOrdinal > total) return undefined;
  return {
    kind: text(raw['kind']),
    node: text(raw['node']),
    activation: text(raw['activation']),
    member: text(raw['member']),
    ordinal: memberOrdinal,
    total,
  };
}

/** One card (`fanout-interrupt-card`), with the fields the card view reads. */
export interface FanoutInterruptCard extends MemberHierarchy {
  readonly interrupt_key: string;
  readonly interrupt_id: string;
  readonly kind: string;
  readonly available_actions: readonly string[];
  readonly display: Raw;
  readonly fanout_v1: FanoutMemberV1 | undefined;
}

/** `interrupt_card_v1` of an `agent_interrupt_pending` frame, or a list entry's `card`. */
export function readInterruptCard(value: unknown): FanoutInterruptCard | undefined {
  const raw = record(value);
  if (!raw || !isInterruptKey(raw['interrupt_key']) || !isInterruptId(raw['interrupt_id'])) return undefined;
  const kind = text(raw['kind']);
  const actions = raw['available_actions'];
  const display = record(raw['display']);
  const admitted = KIND_ACTIONS[kind];
  if (!admitted || !display || !Array.isArray(actions) || actions.length < 1 || actions.length > 4
    || !actions.every((action) => typeof action === 'string' && admitted.has(action))) return undefined;
  const hierarchy = hierarchyOf(raw);
  if (!hierarchy) return undefined;
  // §4 coherence: a member card names its member tier (with the ordinal); a
  // coordinator card carries no fan-out metadata at all.
  const fanout = memberV1(raw['fanout_v1']);
  if (raw['fanout_v1'] !== null && raw['fanout_v1'] !== undefined && !fanout) return undefined;
  if (fanout && !hierarchy.parent_agent_path.some((tier) => tier.sibling_ordinal === fanout.ordinal)) return undefined;
  return {
    ...hierarchy,
    interrupt_key: raw['interrupt_key'],
    interrupt_id: raw['interrupt_id'],
    kind,
    available_actions: actions as readonly string[],
    display,
    fanout_v1: fanout,
  };
}

/** `interrupt_resolved_v1` (§9): which card closed. The action and any value are never read. */
export interface FanoutInterruptResolved {
  readonly interrupt_key: string;
  readonly interrupt_id: string;
  readonly state: string;
}

export function readInterruptResolved(value: unknown): FanoutInterruptResolved | undefined {
  const raw = record(value);
  if (!raw || !isInterruptKey(raw['interrupt_key']) || !isInterruptId(raw['interrupt_id'])) return undefined;
  return { interrupt_key: raw['interrupt_key'], interrupt_id: raw['interrupt_id'], state: text(raw['state']) };
}

/** `fanout_member_status_v1` (§9): one member's lifecycle. */
export interface FanoutMemberStatus extends MemberHierarchy {
  readonly status: string;
  readonly fanout_v1: FanoutMemberV1;
  /** The member tier (`fo1_…`) call id, which progress entries name. */
  readonly memberCallId: string;
  readonly memberName: string;
}

export function readMemberStatus(value: unknown): FanoutMemberStatus | undefined {
  const raw = record(value);
  const fanout = memberV1(raw?.['fanout_v1']);
  const status = text(raw?.['status']);
  if (!raw || !fanout || !MEMBER_STATUSES.has(status)) return undefined;
  const hierarchy = hierarchyOf(raw);
  const memberTier = hierarchy?.parent_agent_path.find((tier) => MEMBER_CALL_ID.test(tier.call_id));
  if (!hierarchy || !memberTier) return undefined;
  return { ...hierarchy, status, fanout_v1: fanout, memberCallId: memberTier.call_id, memberName: memberTier.name };
}

/** One entry of `fanout_progress_v1.members` (§9.1). */
export interface FanoutProgressEntry {
  readonly ordinal: number;
  readonly call_id: string;
  readonly text: string;
}

/** The member entries of a progress frame, in frame order; `undefined` for a malformed frame. */
export function readProgress(value: unknown): readonly FanoutProgressEntry[] | undefined {
  const members = record(value)?.['members'];
  if (!Array.isArray(members) || members.length < 1 || members.length > MAX_PROGRESS_MEMBERS) return undefined;
  const entries: FanoutProgressEntry[] = [];
  for (const member of members) {
    const raw = record(member);
    const memberOrdinal = ordinal(raw?.['ordinal']);
    const callId = raw?.['call_id'];
    const delta = raw?.['text'];
    if (memberOrdinal === undefined || typeof callId !== 'string' || !MEMBER_CALL_ID.test(callId) || typeof delta !== 'string') return undefined;
    entries.push({ ordinal: memberOrdinal, call_id: callId, text: delta });
  }
  return entries;
}

/** `fanout_parked_v1` (§8): the cards still pending when the execution parked. */
export function readParked(value: unknown): ReadonlySet<string> | undefined {
  const pending = record(value)?.['pending'];
  // A park always has 1..16 open cards (§5 open cap); anything else is not a park.
  if (!Array.isArray(pending) || pending.length < 1 || pending.length > 16) return undefined;
  const keys = new Set<string>();
  for (const entry of pending) {
    const key = record(entry)?.['interrupt_key'];
    if (!isInterruptKey(key)) return undefined;
    keys.add(key);
  }
  return keys;
}

function toolArgs(value: unknown): unknown {
  if (typeof value !== 'string' || value === '') return null;
  try {
    return JSON.parse(value) as unknown;
  } catch {
    return value;
  }
}

/** The ledger state a card is known in: `PENDING` from a frame, either state from the list. */
export interface LedgerState {
  readonly state: 'PENDING' | 'DECIDED';
  readonly revision: number;
}

/**
 * A ledger card as the card view and the decision path read it. Placement
 * comes from the hierarchy fields alone (`buildHitlBuckets`), so a member's
 * card lands in that member's accordion exactly like any sub-agent card.
 */
export function cardToInterrupt(card: FanoutInterruptCard, ledger: LedgerState = { state: 'PENDING', revision: 1 }): NormalizedHitlInterrupt {
  const display = card.display;
  const memberTier = card.parent_agent_path[card.parent_agent_path.length - 1];
  return {
    message: text(display['message']) || 'Please review and take action.',
    node_name: text(display['node_name']),
    available_actions: card.available_actions,
    questions: [],
    routes: {},
    edit_state_key: text(display['edit_state_key']),
    guardrail_type: text(display['guardrail_type']),
    tool_name: text(display['tool_name']),
    toolkit_name: text(display['toolkit_name']),
    toolkit_type: text(display['toolkit_type']),
    action_label: text(display['action_label']),
    tool_args: toolArgs(display['tool_args_json']),
    policy_message: text(display['policy_message']),
    interrupt_id: card.interrupt_id,
    tool_call_id: text(display['tool_call_id']),
    child_thread_id: '',
    thread_id: '',
    resume_strategy: LEDGER_RESUME_STRATEGY,
    parent_agent_name: card.parent_agent_name,
    parent_agent_call_id: card.parent_agent_call_id,
    parent_agent_path: card.parent_agent_path,
    ...(memberTier?.sibling_ordinal !== undefined ? { sibling_ordinal: memberTier.sibling_ordinal } : {}),
    interrupt_key: card.interrupt_key,
    ledger_kind: card.kind,
    ledger_revision: ledger.revision,
    server_url: text(display['server_url']),
    ...(ledger.state === 'DECIDED' ? { decided: true, hidden: true } : {}),
  };
}

/** Is this card decided through the public decision API rather than a continuation? */
export function isLedgerInterrupt(interrupt: Raw | undefined): boolean {
  return interrupt?.['resume_strategy'] === LEDGER_RESUME_STRATEGY && isInterruptKey(interrupt['interrupt_key']);
}
