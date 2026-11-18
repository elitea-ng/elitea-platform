/**
 * The delegation notice (client contract 1.60, ADR-0044 Amendment 1, A1.2
 * rule 5).
 *
 * A run that refused or narrowed a delegation to a saved agent or pipeline
 * called as a tool streams a `delegation_refused` or `delegation_narrowed`
 * frame: `response_metadata.delegation_v1` is one notice. Main keeps the same
 * notices on the answer's `metadata.delegation_notices_v1`, so a reload reads
 * them. This module reads both into ONE shape, with Main's bounds
 * (services/elitea-main/internal/application/delegationnotice), so the live
 * notice and the reloaded notice agree.
 *
 * `canReview` and `editors` are derived for the viewer when the notice is
 * served, never stored. A server that cannot say leaves `canReview` unset;
 * show the notice without the action.
 *
 * Every text here is untrusted: render it as plain text.
 */

export type DelegationNoticeKind = 'refused' | 'narrowed';
export type DelegationNoticeReason = 'no_grant' | 'stale' | 'principal' | 'depth';
export type DelegationGrantKind = 'toolkit' | 'tool' | 'credential' | 'data_class' | 'egress_host';

/** One grant a notice names. */
export interface DelegationMissing {
  readonly kind: DelegationGrantKind;
  readonly name: string;
  readonly toolkit?: string;
}

/** One refused or narrowed delegation of a cloud answer. */
export interface DelegationNotice {
  /** The `tool_run_id` of the call it answers: the notice sits at that row. */
  readonly id: string;
  readonly kind: DelegationNoticeKind;
  readonly reason: DelegationNoticeReason;
  readonly calleeName: string;
  readonly callerName?: string;
  /** The calling saved agent; absent when the caller has no saved identity. */
  readonly callerId?: number;
  readonly calleeId?: number;
  readonly missing: readonly DelegationMissing[];
  /** How many there were when more than `missing` lists. */
  readonly missingTotal: number;
  readonly grantId?: number;
  readonly depth?: number;
  readonly maxDepth?: number;
  /** True when the viewer may open the grant review. Unset when the server did not say. */
  readonly canReview?: boolean;
  /** The people to ask when the viewer may not review. */
  readonly editors: readonly string[];
}

export const MAX_DELEGATION_NOTICES = 16;
const MAX_MISSING = 16;
const MAX_EDITORS = 3;
const MAX_TEXT_CHARS = 120;
const MAX_ID_CHARS = 128;

/** What a callee is called when its name is blank (Main's `UnnamedAgent`). */
const UNNAMED_AGENT = 'an agent';

const KINDS: ReadonlySet<string> = new Set(['refused', 'narrowed']);
const EDGE_KINDS: ReadonlySet<string> = new Set(['agent_tool', 'pipeline_child', 'sub_agent', 'pipeline_node']);
const REASONS: ReadonlySet<string> = new Set(['no_grant', 'stale', 'principal', 'depth']);
const GRANT_KINDS: ReadonlySet<string> = new Set(['toolkit', 'tool', 'credential', 'data_class', 'egress_host']);

function record(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined;
}

function text(value: unknown): string | undefined {
  if (typeof value !== 'string') return undefined;
  const trimmed = value.trim();
  return trimmed === '' ? undefined : trimmed.slice(0, MAX_TEXT_CHARS);
}

function positive(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0 ? value : undefined;
}

/** One notice from a frame's `delegation_v1` or a stored `delegation_notices_v1` entry. */
export function parseDelegationNotice(value: unknown): DelegationNotice | undefined {
  const raw = record(value);
  if (raw === undefined) return undefined;
  const id = typeof raw.id === 'string' && raw.id !== '' && raw.id.length <= MAX_ID_CHARS ? raw.id : undefined;
  const kind = typeof raw.kind === 'string' && KINDS.has(raw.kind) ? (raw.kind as DelegationNoticeKind) : undefined;
  const reason = typeof raw.reason === 'string' && REASONS.has(raw.reason) ? (raw.reason as DelegationNoticeReason) : undefined;
  const edge = record(raw.edge);
  if (id === undefined || kind === undefined || reason === undefined) return undefined;
  // Main's rule, so the live notice and the reloaded one agree: a narrowing is
  // drift (`stale`) and drift is a narrowing.
  if ((kind === 'narrowed') !== (reason === 'stale')) return undefined;
  if (typeof edge?.kind !== 'string' || !EDGE_KINDS.has(edge.kind)) return undefined;
  const calleeName = text(edge.callee_name) ?? UNNAMED_AGENT;
  const missing: DelegationMissing[] = [];
  for (const entry of Array.isArray(raw.missing) ? raw.missing.slice(0, MAX_MISSING) : []) {
    const item = record(entry);
    const name = text(item?.name);
    if (item === undefined || name === undefined || typeof item.kind !== 'string' || !GRANT_KINDS.has(item.kind)) continue;
    const toolkit = text(item.toolkit);
    missing.push({ kind: item.kind as DelegationGrantKind, name, ...(toolkit === undefined ? {} : { toolkit }) });
  }
  const editors = (Array.isArray(raw.editors) ? raw.editors : [])
    .map(text)
    .filter((name): name is string => name !== undefined)
    .slice(0, MAX_EDITORS);
  const callerName = text(edge?.caller_name);
  const callerId = positive(edge?.caller_id);
  const calleeId = positive(edge?.callee_id);
  const grantId = positive(raw.grant_id);
  const depth = positive(raw.depth);
  const maxDepth = positive(raw.max_depth);
  return {
    id,
    kind,
    reason,
    calleeName,
    ...(callerName === undefined ? {} : { callerName }),
    ...(callerId === undefined ? {} : { callerId }),
    ...(calleeId === undefined ? {} : { calleeId }),
    missing,
    missingTotal: Math.max(missing.length, positive(raw.missing_total) ?? 0),
    ...(grantId === undefined ? {} : { grantId }),
    ...(depth === undefined ? {} : { depth }),
    ...(maxDepth === undefined ? {} : { maxDepth }),
    ...(typeof raw.can_review === 'boolean' ? { canReview: raw.can_review } : {}),
    editors,
  };
}

/** A settled answer's stored notices; anything that is not one is dropped. */
export function parseDelegationNotices(value: unknown): readonly DelegationNotice[] {
  if (!Array.isArray(value)) return [];
  return foldAll([], value.slice(0, MAX_DELEGATION_NOTICES).map(parseDelegationNotice));
}

function foldAll(into: readonly DelegationNotice[], notices: readonly (DelegationNotice | undefined)[]): readonly DelegationNotice[] {
  let out = into;
  for (const notice of notices) {
    if (notice !== undefined) out = foldDelegationNotice(out, notice);
  }
  return out;
}

/**
 * Adds a live notice: one per call (a replayed frame replaces its notice, so
 * a reattach does not double it), at most `MAX_DELEGATION_NOTICES`, in order.
 */
export function foldDelegationNotice(notices: readonly DelegationNotice[], notice: DelegationNotice): readonly DelegationNotice[] {
  const at = notices.findIndex((existing) => existing.id === notice.id);
  if (at >= 0) return notices.map((existing, index) => (index === at ? notice : existing));
  if (notices.length >= MAX_DELEGATION_NOTICES) return notices;
  return [...notices, notice];
}

/** The notice of a live frame's `response_metadata`, if it carries one. */
export function delegationNoticeOf(responseMetadata: unknown): DelegationNotice | undefined {
  return parseDelegationNotice(record(responseMetadata)?.delegation_v1);
}

/**
 * Whether the notice offers Review delegation. A principal or depth refusal
 * never does (an editor cannot fix it), and neither does a viewer the server
 * did not clear: only an explicit `canReview` of true.
 */
export function offersReview(notice: DelegationNotice): boolean {
  return notice.canReview === true && notice.reason !== 'principal' && notice.reason !== 'depth' && notice.callerId !== undefined;
}

/**
 * The names a notice lists, for its sentence: `Git`, `Git, Jira` or
 * `Git, Jira and 2 more`. A tool is named with its toolkit.
 */
export function missingNames(notice: DelegationNotice): readonly string[] {
  return notice.missing.map((item) => (item.kind === 'tool' && item.toolkit !== undefined ? `${item.toolkit} / ${item.name}` : item.name));
}
