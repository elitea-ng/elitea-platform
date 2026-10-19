/**
 * lib/executionHierarchy.ts — the tool-action ancestry contract (issue #93).
 *
 * Ported from `EliteaUI/src/[fsd]/features/chat/lib/helpers/
 * executionHierarchy.helpers.js:19-67` (`normalizeAgentPath`,
 * `normalizeExecutionHierarchy`). Prerequisite for the reducer's tool-node
 * slice: every tool action carries the ancestry that decides which agent's UI
 * chip owns it, and the same shape has to come out of live socket metadata,
 * persisted thinking steps and already-built UI actions — otherwise a replayed
 * conversation groups its sub-agents differently from a live one.
 *
 * `isDelegationWrapperAction`, `getActionOwnerPath`, `getSubAgentName` and
 * `getSubAgentInstanceKey` (`executionHierarchy.helpers.js:5-17,69-117`) are
 * the rendering-side half: they decide which sub-agent instance owns an
 * action, for the thinking panel and the approval buckets alike.
 */

/** Non-empty, trimmed string or `''` — the baseline's `asNonEmptyString`. */
function asNonEmptyString(value: unknown): string {
  return typeof value === 'string' && value.trim() ? value : '';
}

/** One ancestry tier: which agent invoked this, and under which call. */
export interface AgentPathTier {
  readonly name: string;
  readonly call_id: string;
  readonly sibling_ordinal?: number | undefined;
  readonly [key: string]: unknown;
}

export interface ExecutionHierarchy {
  readonly parent_agent_name: string;
  readonly parent_agent_call_id: string;
  readonly parent_agent_path: readonly AgentPathTier[];
}

interface HierarchySource {
  readonly parent_agent_path?: unknown;
  readonly [key: string]: unknown;
}

/**
 * Normalise an ancestry path, dropping tiers that identify nothing.
 *
 * `sibling_ordinal` survives only as a positive integer: the baseline uses it
 * to disambiguate concurrent siblings, and a zero or non-integer would make two
 * different sub-agents look like the same chip.
 */
export function normalizeAgentPath(path: unknown): readonly AgentPathTier[] {
  if (!Array.isArray(path)) return [];
  return path
    .filter((tier): tier is Record<string, unknown> => Boolean(tier) && typeof tier === 'object')
    .map((tier) => {
      const ordinal = tier['sibling_ordinal'];
      const valid = typeof ordinal === 'number' && Number.isInteger(ordinal) && ordinal > 0;
      // Assigned unconditionally, as the baseline does. A conditional spread
      // would leave the ORIGINAL value in place via `...tier` — so an invalid
      // ordinal (0, fractional) would survive normalisation and two concurrent
      // siblings could still collapse onto one chip.
      const normalised: AgentPathTier = {
        ...tier,
        name: asNonEmptyString(tier['name']),
        call_id: asNonEmptyString(tier['call_id']),
        sibling_ordinal: valid ? ordinal : undefined,
      };
      return normalised;
    })
    .filter((tier) => tier.name || tier.call_id);
}

/** How much ancestry a source carries, so the richest one wins. */
function pathScore(source: HierarchySource): number {
  return normalizeAgentPath(source.parent_agent_path).reduce(
    (total, tier) => total + 10 + (tier.call_id ? 2 : 0) + (tier.sibling_ordinal ? 1 : 0),
    0,
  );
}

/**
 * Read hierarchy fields from the first source that has them, so socket
 * metadata, persisted steps and UI actions converge on one shape.
 *
 * The path is taken from the DEEPEST populated source rather than the first:
 * one record can hold both a task overlay and richer producer metadata, and
 * letting argument order win would make persisted rendering discard a tier the
 * live stream had kept. Scalars still resolve first-wins, which is what lets a
 * caller pass a specific override ahead of the general metadata.
 */
export function normalizeExecutionHierarchy(...sources: readonly unknown[]): ExecutionHierarchy {
  const valid = sources.filter(
    (source): source is HierarchySource => Boolean(source) && typeof source === 'object',
  );
  const pathSource = valid
    .filter((source) => Array.isArray(source.parent_agent_path))
    .sort((left, right) => pathScore(right) - pathScore(left))[0];
  const parentAgentPath = normalizeAgentPath(pathSource?.parent_agent_path);
  const lastTier = parentAgentPath[parentAgentPath.length - 1];

  const findString = (field: string): string => {
    for (const source of valid) {
      const value = asNonEmptyString(source[field]);
      if (value) return value;
    }
    return '';
  };

  return {
    parent_agent_name: findString('parent_agent_name') || lastTier?.name || '',
    parent_agent_call_id: findString('parent_agent_call_id') || lastTier?.call_id || '',
    parent_agent_path: parentAgentPath,
  };
}

/** Whitespace-free name, for the self-named wrapper test. */
function compactName(value: unknown): string {
  return asNonEmptyString(value).replace(/\s/g, '');
}

interface OwnedAction {
  readonly type?: unknown;
  readonly name?: unknown;
  readonly original_name?: unknown;
  readonly parent_agent_call_id?: unknown;
  readonly sibling_ordinal?: unknown;
  readonly toolMeta?: Readonly<Record<string, unknown>> | undefined;
  readonly [key: string]: unknown;
}

/** True only for a tool action that actually delegates to an agent or pipeline. */
function isDelegationWrapperAction(action: OwnedAction | undefined): boolean {
  if (!action || action.type !== 'tool') return false;
  const toolkitType = action.toolMeta?.['toolkit_type'];
  if (toolkitType === 'application' || toolkitType === 'pipeline') return true;
  // Older persisted wrappers may carry only agent_type. That field is also
  // inherited by ordinary LLM/tool actions, so it is not sufficient alone.
  const selfNamed = compactName(action.name) !== '' && compactName(action.name) === compactName(action.original_name);
  const stableCallId = asNonEmptyString(action.parent_agent_call_id) || asNonEmptyString(action.toolMeta?.['parent_agent_call_id']);
  return Boolean(action.toolMeta?.['agent_type'] && selfNamed && stableCallId);
}

function positiveOrdinal(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isInteger(value) && value > 0 ? value : undefined;
}

/**
 * The ancestry path whose final tier owns this action's UI group.
 *
 * Root delegation wrappers have no parent path, so their owner tier is
 * synthesised and the wrapper shares one key with its child's inner events.
 */
export function getActionOwnerPath(action: OwnedAction | undefined): readonly AgentPathTier[] {
  if (!action) return [];
  const hierarchy = normalizeExecutionHierarchy(action, action.toolMeta);
  if (hierarchy.parent_agent_path.length) return hierarchy.parent_agent_path;
  const wrapperName = isDelegationWrapperAction(action) ? asNonEmptyString(action.original_name) : '';
  const name = wrapperName || hierarchy.parent_agent_name;
  if (!name) return [];
  return [
    {
      name,
      call_id: hierarchy.parent_agent_call_id,
      sibling_ordinal: positiveOrdinal(action.sibling_ordinal ?? action.toolMeta?.['sibling_ordinal']),
    },
  ];
}

/** The display name of the sub-agent that owns an action, or `''` for the coordinator. */
export function getSubAgentName(action: OwnedAction | undefined): string {
  if (!action) return '';
  const owner = getActionOwnerPath(action);
  if (owner.length) return owner[owner.length - 1]?.name ?? '';
  const hierarchy = normalizeExecutionHierarchy(action, action.toolMeta);
  if (hierarchy.parent_agent_name) return hierarchy.parent_agent_name;
  return isDelegationWrapperAction(action) ? asNonEmptyString(action.original_name) : '';
}

/**
 * One key per sub-agent INSTANCE: the owner path's `[name, call_id]` tiers.
 * `sibling_ordinal` is display metadata, not identity — a resumed child can
 * be renumbered while its call id stays stable.
 */
export function getSubAgentInstanceKey(action: OwnedAction | undefined): string {
  if (!action) return '';
  const owner = getActionOwnerPath(action);
  if (owner.length) return JSON.stringify(owner.map(({ name, call_id }) => [name, call_id]));
  return getSubAgentName(action);
}
