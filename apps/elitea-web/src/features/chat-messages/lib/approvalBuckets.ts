/**
 * lib/approvalBuckets.ts — which spawning instance owns each approval card.
 *
 * Ported from EliteaUI: the HITL bucketing in
 * `features/chat/lib/hooks/useApplicationAnswerState.hooks.js:292-352` and
 * `groupToolkitAuthorizationActions` in
 * `features/chat/lib/helpers/mcpAuthorization.helpers.js:275-321`.
 *
 * Coordinator (root) cards stay flat. Every other card lands in one bucket per
 * spawning agent/pipeline INSTANCE, keyed `parent_agent_call_id` →
 * `child_thread_id` → `thread_id` → `JSON(parent_agent_path)` → name, and is
 * labelled by `computeBreadcrumbs` with the response's trace as numbering
 * context. Two parallel calls of one sub-agent therefore get two buckets,
 * `Sub (1)` and `Sub (2)`; they are never folded by name while any identity
 * exists. Fan-out members (Parallel branches, Map items) reuse this contract.
 */
import { getActionOwnerPath, normalizeExecutionHierarchy, type AgentPathTier } from './executionHierarchy';
import { computeBreadcrumbs, type BreadcrumbEntry } from './subAgentBreadcrumbs';

/** Agent kind for the bucket header icon: `'pipeline'`, `'application'` or `''`. */
type BucketAgentType = 'pipeline' | 'application' | '';

/** Participant kind by sub-agent display name (the reference's `subAgentTypeByName`). */
export type SubAgentTypeByName = Readonly<Record<string, string | undefined>>;

type Record_ = Readonly<Record<string, unknown>>;

/** One card and its position in the message's list. */
interface BucketEntry<T> {
  readonly interrupt: T;
  readonly index: number;
}

interface HitlBucket<T> {
  readonly instanceKey: string;
  readonly name: string;
  readonly label: string | undefined;
  readonly agentType: BucketAgentType;
  readonly agentPath: readonly AgentPathTier[];
  readonly entries: readonly BucketEntry<T>[];
}

export interface HitlBuckets<T> {
  readonly coordinator: readonly BucketEntry<T>[];
  readonly subAgents: readonly HitlBucket<T>[];
  readonly hasSubAgents: boolean;
}

function nonEmpty(value: unknown): string {
  return typeof value === 'string' && value.trim() ? value : '';
}

function agentTypeOf(value: unknown): BucketAgentType {
  if (value === 'pipeline') return 'pipeline';
  return nonEmpty(value) ? 'application' : '';
}

/** Participant map first, then the first source that carries `agent_type`. */
function resolveAgentType(name: string, sources: readonly Record_[], byName: SubAgentTypeByName | undefined): BucketAgentType {
  const participant = byName?.[name];
  if (participant) return participant === 'pipeline' ? 'pipeline' : 'application';
  for (const source of sources) {
    const meta = source['toolMeta'] as Record_ | undefined;
    const type = agentTypeOf(source['agent_type'] ?? meta?.['agent_type']);
    if (type) return type;
  }
  return '';
}

/** Trace paths that keep root numbering stable (`trace-${index}` keys never render). */
function traceContext(toolActions: readonly unknown[]): BreadcrumbEntry[] {
  return toolActions
    .map((action, index) => ({ instanceKey: `trace-${index}`, agentPath: getActionOwnerPath(action as Record_) }))
    .filter((entry) => entry.agentPath.length > 0);
}

/** HITL cards (already filtered to visible ones) grouped by spawning instance. */
export function buildHitlBuckets<T extends object>(
  interrupts: readonly T[],
  toolActions: readonly unknown[] = [],
  subAgentTypeByName?: SubAgentTypeByName,
): HitlBuckets<T> {
  const coordinator: BucketEntry<T>[] = [];
  const order: string[] = [];
  const byInstance = new Map<string, { name: string; agentPath: readonly AgentPathTier[]; entries: BucketEntry<T>[] }>();
  interrupts.forEach((interrupt, index) => {
    const raw = interrupt as Record_;
    const hierarchy = normalizeExecutionHierarchy(raw);
    const name = hierarchy.parent_agent_name;
    const entry = { interrupt, index };
    if (!name) {
      coordinator.push(entry);
      return;
    }
    const key =
      hierarchy.parent_agent_call_id ||
      nonEmpty(raw['child_thread_id']) ||
      nonEmpty(raw['thread_id']) ||
      (hierarchy.parent_agent_path.length ? JSON.stringify(hierarchy.parent_agent_path) : name);
    let bucket = byInstance.get(key);
    if (!bucket) {
      const ordinal = raw['sibling_ordinal'];
      const agentPath: readonly AgentPathTier[] = hierarchy.parent_agent_path.length
        ? hierarchy.parent_agent_path
        : [{ name, call_id: key, sibling_ordinal: typeof ordinal === 'number' && Number.isInteger(ordinal) && ordinal > 0 ? ordinal : undefined }];
      bucket = { name, agentPath, entries: [] };
      byInstance.set(key, bucket);
      order.push(key);
    }
    bucket.entries.push(entry);
  });
  const labels = computeBreadcrumbs(
    order.map((key) => ({ instanceKey: key, agentPath: byInstance.get(key)?.agentPath ?? [] })),
    traceContext(toolActions),
  );
  return {
    coordinator,
    subAgents: order.flatMap((key) => {
      const bucket = byInstance.get(key);
      if (!bucket) return [];
      const sources = bucket.entries.map((entry) => entry.interrupt as Record_);
      return [{ ...bucket, instanceKey: key, label: labels.get(key), agentType: resolveAgentType(bucket.name, sources, subAgentTypeByName) }];
    }),
    hasSubAgents: order.length > 0,
  };
}

interface AuthorizationBucket<A> {
  readonly instanceKey: string;
  readonly name: string;
  readonly label: string | undefined;
  readonly agentType: BucketAgentType;
  readonly agentPath: readonly AgentPathTier[];
  readonly actions: readonly A[];
}

export interface AuthorizationBuckets<A> {
  readonly coordinator: readonly A[];
  readonly subAgents: readonly AuthorizationBucket<A>[];
  readonly hasSubAgents: boolean;
}

/** Direct toolkit authorization cards stay flat; delegated ones group by origin instance. */
export function groupToolkitAuthorizationActions<A extends object>(
  actions: readonly A[],
  contextActions: readonly unknown[] = [],
  subAgentTypeByName?: SubAgentTypeByName,
): AuthorizationBuckets<A> {
  const coordinator: A[] = [];
  const order: string[] = [];
  const byInstance = new Map<string, { name: string; agentPath: readonly AgentPathTier[]; actions: A[] }>();
  for (const action of actions) {
    const raw = action as Record_;
    const path = getActionOwnerPath(raw);
    if (!path.length) {
      coordinator.push(action);
      continue;
    }
    const hierarchy = normalizeExecutionHierarchy(raw, raw['toolMeta']);
    const key = hierarchy.parent_agent_call_id || JSON.stringify(path.map(({ name, call_id }) => [name, call_id]));
    let bucket = byInstance.get(key);
    if (!bucket) {
      bucket = { name: path[path.length - 1]?.name || hierarchy.parent_agent_name, agentPath: path, actions: [] };
      byInstance.set(key, bucket);
      order.push(key);
    }
    bucket.actions.push(action);
  }
  const labels = computeBreadcrumbs(
    order.map((key) => ({ instanceKey: key, agentPath: byInstance.get(key)?.agentPath ?? [] })),
    traceContext(contextActions),
  );
  return {
    coordinator,
    subAgents: order.flatMap((key) => {
      const bucket = byInstance.get(key);
      if (!bucket) return [];
      const agentType = resolveAgentType(bucket.name, bucket.actions as readonly Record_[], subAgentTypeByName);
      return [{ ...bucket, instanceKey: key, label: labels.get(key), agentType }];
    }),
    hasSubAgents: order.length > 0,
  };
}
