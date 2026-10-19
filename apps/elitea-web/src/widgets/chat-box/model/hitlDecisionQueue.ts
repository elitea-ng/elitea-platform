/**
 * model/hitlDecisionQueue.ts — the decisions a paused response is still
 * collecting, kept across a reload.
 *
 * Every pause card resolves on its own in the UI, but today's continuation
 * route consumes the WHOLE pending set at once (`decisionsMatchCurrentHITLInterrupts`
 * and `decisionsMatchCurrentAuthorizationRequests`,
 * `services/elitea-main/internal/application/agentexecution/continue.go`). So
 * a decided card waits here until its last sibling is decided, and then the
 * set is submitted as one continuation (`decideHitlCard`,
 * `../ui/hooks/useChatBoxHandlers.hitl.ts`). Wave 2's per-card POST replaces
 * that wait, not this module's callers.
 *
 * WHAT IS STORED. Only `{interruptId, toolCallId?, action, value?}` per card,
 * where `value` is the text the user typed (an edit, a comment, an answer). No
 * tool arguments, prompts or credentials — authorization tokens never come
 * here. Nothing in this module logs a value.
 *
 * WHERE. `sessionStorage` through `@/shared/lib/storage`, under the `el.`
 * namespace so the logout sweep removes it. `createStorage` is called lazily,
 * never at module scope (see `./queuedMessages.store.ts`).
 *
 * BOUNDS, checked before every write and again on every read
 * (`HITL_QUEUE_LIMITS`). A stored blob that breaks any rule — an unknown key,
 * a wrong type, a bound exceeded — is treated as absent, never thrown. A
 * decision whose value is too large to persist still counts for this page life
 * (it is held in memory) and simply does not survive a reload.
 */
import { createStorage } from '@/shared/lib/storage';

/** Which continuation contract a queue feeds. */
type HitlQueueKind = 'hitl' | 'authorization';

/** One paused response's queue: conversation + response message + contract. */
export interface HitlQueueScope {
  readonly conversation: string;
  readonly message: string;
  readonly kind: HitlQueueKind;
}

/** One card's decision. `interruptId` is the card identity the queue is keyed by. */
export interface QueuedHitlDecision {
  readonly interruptId: string;
  readonly toolCallId?: string | undefined;
  readonly action: string;
  readonly value?: string | undefined;
}

/** Result of queueing one decision. */
type QueueHitlDecisionOutcome =
  | { readonly queued: true }
  | { readonly queued: false; readonly reason: 'too-many' | 'invalid' };

/**
 * The bounds, named so the tests and the readers share them.
 *
 * `decisionsPerResponse` is Main's `maxCurrentHITLDecisions` and the Worker's
 * `MAX_PENDING_PAUSE_CARDS`; `identityChars` is Main's 512-byte id bound.
 */
export const HITL_QUEUE_LIMITS = {
  decisionsPerResponse: 16,
  valueBytes: 8 * 1024,
  responses: 8,
  identityChars: 512,
} as const;

const STORAGE_KEY = 'chat.hitlDecisions';
const STORAGE_VERSION = 1;

const ACTIONS: Readonly<Record<HitlQueueKind, ReadonlySet<string>>> = {
  hitl: new Set(['approve', 'reject', 'edit', 'block_with_comment', 'answer']),
  authorization: new Set(['authorize', 'skip']),
};

interface StoredResponse {
  readonly conversation: string;
  readonly message: string;
  readonly kind: HitlQueueKind;
  readonly decisions: readonly QueuedHitlDecision[];
}

/* ── validation ──────────────────────────────────────────────────────────── */

const encoder = new TextEncoder();

function utf8Bytes(value: string): number {
  return encoder.encode(value).length;
}

function isIdentity(value: unknown): value is string {
  return typeof value === 'string' && value !== '' && value.length <= HITL_QUEUE_LIMITS.identityChars;
}

function onlyKeys(record: Readonly<Record<string, unknown>>, allowed: readonly string[]): boolean {
  return Object.keys(record).every((key) => allowed.includes(key));
}

function isRecord(value: unknown): value is Readonly<Record<string, unknown>> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** No value, or a typed HITL value within the persisted byte bound. Authorization decisions carry none. */
function persistableValue(value: unknown, kind: HitlQueueKind): value is string | undefined {
  if (value === undefined) return true;
  return kind === 'hitl' && typeof value === 'string' && utf8Bytes(value) <= HITL_QUEUE_LIMITS.valueBytes;
}

/** A decision fit to persist, or `undefined`. */
function readDecision(raw: unknown, kind: HitlQueueKind): QueuedHitlDecision | undefined {
  if (!isRecord(raw) || !onlyKeys(raw, ['interruptId', 'toolCallId', 'action', 'value'])) return undefined;
  const { interruptId, toolCallId, action, value } = raw;
  if (!isIdentity(interruptId) || typeof action !== 'string' || !ACTIONS[kind].has(action)) return undefined;
  if (toolCallId !== undefined && !isIdentity(toolCallId)) return undefined;
  if (!persistableValue(value, kind)) return undefined;
  return {
    interruptId,
    action,
    ...(toolCallId !== undefined ? { toolCallId } : {}),
    ...(value !== undefined ? { value } : {}),
  };
}

function readResponse(raw: unknown): StoredResponse | undefined {
  if (!isRecord(raw) || !onlyKeys(raw, ['conversation', 'message', 'kind', 'decisions'])) return undefined;
  const { conversation, message, kind, decisions } = raw;
  if (!isIdentity(conversation) || !isIdentity(message) || (kind !== 'hitl' && kind !== 'authorization')) return undefined;
  if (!Array.isArray(decisions) || decisions.length === 0 || decisions.length > HITL_QUEUE_LIMITS.decisionsPerResponse) {
    return undefined;
  }
  const read = decisions.map((decision) => readDecision(decision, kind));
  if (read.some((decision) => decision === undefined)) return undefined;
  const valid = read as readonly QueuedHitlDecision[];
  if (new Set(valid.map((decision) => decision.interruptId)).size !== valid.length) return undefined;
  return { conversation, message, kind, decisions: valid };
}

/**
 * The stored responses, or `[]` for an absent or invalid blob.
 *
 * All or nothing: a blob with one bad entry is not trusted for any of them,
 * because it was not written by this module.
 */
export function parseStoredHitlDecisions(raw: unknown): readonly StoredResponse[] {
  if (!isRecord(raw) || !onlyKeys(raw, ['version', 'responses']) || raw['version'] !== STORAGE_VERSION) return [];
  const responses = raw['responses'];
  if (!Array.isArray(responses) || responses.length > HITL_QUEUE_LIMITS.responses) return [];
  const read = responses.map(readResponse);
  if (read.some((response) => response === undefined)) return [];
  const valid = read as readonly StoredResponse[];
  const scopes = new Set(valid.map(scopeKey));
  return scopes.size === valid.length ? valid : [];
}

/* ── storage ─────────────────────────────────────────────────────────────── */

function session(): ReturnType<typeof createStorage> | undefined {
  try {
    return createStorage('session');
  } catch {
    // Handled (§3.6): no Web Storage in this context; the memory overlay below
    // keeps the queue for this page life.
    return undefined;
  }
}

function loadStored(): readonly StoredResponse[] {
  try {
    return parseStoredHitlDecisions(session()?.getJSON<unknown>(STORAGE_KEY) ?? null);
  } catch {
    // Handled (§3.6): an unreadable store is an absent store.
    return [];
  }
}

/** Writes the blob, or reports that nothing could be written. Never throws. */
function saveStored(responses: readonly StoredResponse[]): boolean {
  const store = session();
  if (!store) return false;
  try {
    if (responses.length === 0) store.remove(STORAGE_KEY);
    else store.setJSON(STORAGE_KEY, { version: STORAGE_VERSION, responses });
    return true;
  } catch {
    // Handled (§3.6): a full or disabled quota keeps the memory overlay only.
    return false;
  }
}

function scopeKey(scope: Pick<HitlQueueScope, 'conversation' | 'message' | 'kind'>): string {
  return JSON.stringify([scope.conversation, scope.message, scope.kind]);
}

/**
 * Decisions that could not be persisted (too large, or no storage): kept for
 * this page life only, keyed by scope. Bounded like the store.
 */
const unpersisted = new Map<string, Map<string, QueuedHitlDecision>>();

function rememberUnpersisted(scope: HitlQueueScope, decision: QueuedHitlDecision): void {
  const key = scopeKey(scope);
  const held = unpersisted.get(key) ?? new Map<string, QueuedHitlDecision>();
  held.set(decision.interruptId, decision);
  unpersisted.delete(key);
  unpersisted.set(key, held);
  while (unpersisted.size > HITL_QUEUE_LIMITS.responses) {
    const oldest = unpersisted.keys().next().value;
    if (oldest === undefined) break;
    unpersisted.delete(oldest);
  }
}

function persistable(decision: QueuedHitlDecision): boolean {
  return decision.value === undefined || utf8Bytes(decision.value) <= HITL_QUEUE_LIMITS.valueBytes;
}

/* ── API ─────────────────────────────────────────────────────────────────── */

/** The decisions queued for one response, persisted and in-memory, in decision order. */
export function queuedHitlDecisions(scope: HitlQueueScope): readonly QueuedHitlDecision[] {
  const key = scopeKey(scope);
  const stored = loadStored().find((response) => scopeKey(response) === key)?.decisions ?? [];
  const held = unpersisted.get(key);
  if (!held || held.size === 0) return stored;
  const ids = new Set(stored.map((decision) => decision.interruptId));
  return [...stored, ...[...held.values()].filter((decision) => !ids.has(decision.interruptId))];
}

/** Rewrites one response's queue (an empty list removes it), evicting the oldest response past the bound. */
function writeQueue(scope: HitlQueueScope, decisions: readonly QueuedHitlDecision[]): void {
  const key = scopeKey(scope);
  const others = loadStored().filter((response) => scopeKey(response) !== key);
  const kept = decisions.filter(persistable);
  const next = kept.length > 0 ? [...others, { ...scope, decisions: kept }] : others;
  const written = saveStored(next.slice(-HITL_QUEUE_LIMITS.responses));
  const held = unpersisted.get(key);
  held?.clear();
  for (const decision of decisions) {
    if (!written || !persistable(decision)) rememberUnpersisted(scope, decision);
  }
  if (held && held.size === 0) unpersisted.delete(key);
}

/**
 * Queue (or replace) one card's decision.
 *
 * Refused, with nothing written, when the decision is malformed or would take
 * the response past `decisionsPerResponse`.
 */
export function queueHitlDecision(scope: HitlQueueScope, given: QueuedHitlDecision): QueueHitlDecisionOutcome {
  // A card with no tool call (a HITL node, a fan-out member) sends `''`: that
  // is "absent", not a malformed identity.
  const { toolCallId, ...rest } = given;
  const decision: QueuedHitlDecision = { ...rest, ...(toolCallId ? { toolCallId } : {}) };
  const shaped = readDecision({ ...decision, value: undefined }, scope.kind);
  const valueOk = decision.value === undefined || (scope.kind === 'hitl' && typeof decision.value === 'string');
  if (!shaped || !valueOk || !isIdentity(scope.conversation) || !isIdentity(scope.message)) {
    return { queued: false, reason: 'invalid' };
  }
  const current = queuedHitlDecisions(scope).filter((entry) => entry.interruptId !== decision.interruptId);
  if (current.length + 1 > HITL_QUEUE_LIMITS.decisionsPerResponse) return { queued: false, reason: 'too-many' };
  const entry: QueuedHitlDecision = { ...shaped, ...(decision.value !== undefined ? { value: decision.value } : {}) };
  writeQueue(scope, [...current, entry]);
  return { queued: true };
}

/** Drop one response's queue — its set was submitted, consumed elsewhere, or refused. */
export function forgetHitlDecisions(scope: HitlQueueScope): void {
  writeQueue(scope, []);
  unpersisted.delete(scopeKey(scope));
}

/**
 * Keep only the decisions whose card is still pending, and return them.
 *
 * Called with the pending identities of a freshly hydrated response: a decision
 * for a card the server no longer holds pending answers nothing.
 */
export function retainHitlDecisions(scope: HitlQueueScope, pending: ReadonlySet<string>): readonly QueuedHitlDecision[] {
  const current = queuedHitlDecisions(scope);
  const kept = current.filter((decision) => pending.has(decision.interruptId));
  if (kept.length !== current.length) writeQueue(scope, kept);
  return kept;
}

/**
 * Drop what this module holds in memory (values too large to persist, and
 * in-flight claims). Called when the conversation on screen changes; a logout
 * is a full navigation, which ends this memory with the document, and
 * `clearNamespace()` sweeps the persisted half.
 */
export function clearHitlDecisionMemory(): void {
  unpersisted.clear();
  submitting.clear();
}

/** Responses whose set is being submitted right now: a second submit of the same set is refused. */
const submitting = new Set<string>();

/** Claim the one submission of a response's set; `false` when one is already in flight. */
export function claimHitlSubmission(scope: HitlQueueScope): boolean {
  const key = scopeKey(scope);
  if (submitting.has(key)) return false;
  submitting.add(key);
  return true;
}

export function releaseHitlSubmission(scope: HitlQueueScope): void {
  submitting.delete(scopeKey(scope));
}
