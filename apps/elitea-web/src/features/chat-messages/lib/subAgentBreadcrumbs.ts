/**
 * lib/subAgentBreadcrumbs.ts — the sub-agent hierarchy label contract.
 *
 * Ported from `EliteaUI/src/[fsd]/features/chat/lib/helpers/
 * subAgentGrouping.helpers.js:393-461` (`computeBreadcrumbs`,
 * `selectRichestAgentPath`). One label rule serves the thinking panel and the
 * approval buckets, live and reloaded, so a customer sees the same
 * `Parent (1) ▸ Sub` everywhere:
 *
 * - tiers are joined with ` ▸ `;
 * - only the ROOT tier is numbered, and only when that root name repeats;
 * - backend `sibling_ordinal` orders the roots; the fallback is lexical
 *   identity order; the displayed rank is contiguous (1, 3 → 1, 2);
 * - context entries (paths seen elsewhere in the same response) take part in
 *   numbering without being labelled, so a later batch holding one paused
 *   sibling keeps its `(2)`.
 */
import type { AgentPathTier } from './executionHierarchy';

/** One labelled instance: its key and its ancestry, root first. */
export interface BreadcrumbEntry {
  readonly instanceKey: string;
  readonly agentPath: readonly AgentPathTier[];
}

/** Fixed parent scope for tier 0 (no real ancestor call id above the root). */
const ROOT_SCOPE = '__root__';

/** Separator between tiers — the reference's exact glyph. */
const BREADCRUMB_SEPARATOR = ' ▸ ';

function rootIdentity(root: AgentPathTier): string {
  return root.call_id || JSON.stringify([ROOT_SCOPE, root.name, root.sibling_ordinal]);
}

function rootOrdinals(entries: readonly BreadcrumbEntry[]): Map<string, Map<string, number>> {
  const rootsByName = new Map<string, Map<string, AgentPathTier>>();
  for (const { agentPath } of entries) {
    const root = agentPath[0];
    if (!root) continue;
    const roots = rootsByName.get(root.name) ?? new Map<string, AgentPathTier>();
    rootsByName.set(root.name, roots);
    const identity = rootIdentity(root);
    if (!roots.has(identity)) roots.set(identity, root);
  }
  const result = new Map<string, Map<string, number>>();
  rootsByName.forEach((roots, name) => {
    const ordered = [...roots.entries()].sort(([idA, tierA], [idB, tierB]) => {
      const ordinalA = tierA.sibling_ordinal || Number.MAX_SAFE_INTEGER;
      const ordinalB = tierB.sibling_ordinal || Number.MAX_SAFE_INTEGER;
      return ordinalA - ordinalB || idA.localeCompare(idB);
    });
    result.set(name, new Map(ordered.map(([identity], index) => [identity, index + 1])));
  });
  return result;
}

/** `instanceKey → label` for every entry with a non-empty path. */
export function computeBreadcrumbs(
  entries: readonly BreadcrumbEntry[],
  contextEntries: readonly BreadcrumbEntry[] = [],
): Map<string, string> {
  const ordinals = rootOrdinals([...entries, ...contextEntries]);
  const result = new Map<string, string>();
  for (const { instanceKey, agentPath } of entries) {
    const root = agentPath[0];
    if (!root) continue;
    const roots = ordinals.get(root.name);
    const ordinal = roots?.get(rootIdentity(root)) ?? 1;
    const head = roots && roots.size > 1 ? `${root.name} (${ordinal})` : root.name;
    result.set(instanceKey, [head, ...agentPath.slice(1).map((tier) => tier.name)].join(BREADCRUMB_SEPARATOR));
  }
  return result;
}

function pathScore(path: readonly AgentPathTier[]): number {
  return path.reduce((total, tier) => total + 10 + (tier.call_id ? 2 : 0) + (tier.sibling_ordinal ? 1 : 0), 0);
}

/** The deepest, most fully identified path, independent of arrival order. */
export function selectRichestAgentPath(paths: readonly (readonly AgentPathTier[])[]): readonly AgentPathTier[] {
  return paths.reduce<readonly AgentPathTier[]>((best, candidate) => (pathScore(candidate) > pathScore(best) ? candidate : best), []);
}
