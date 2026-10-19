/**
 * lib/staticPauseCards.ts — one inline card per authored (static) pipeline pause.
 *
 * A static pause (`interrupt_before` / `interrupt_after`) has no EliteaUI
 * screen of its own; it renders as the same inline HITL card, placed like any
 * other approval: a root pipeline's pause on the coordinator, a saved
 * pipeline's pause (invoked as a tool) inside its spawning instance's
 * approval accordion, keyed by that tool call — the same key
 * `buildHitlBuckets` gives the instance's HITL cards, so both share one
 * accordion. Raw thread ids and proof kinds are never shown.
 */
import { getActionOwnerPath, type AgentPathTier } from './executionHierarchy';
import { computeBreadcrumbs } from './subAgentBreadcrumbs';
import type { StaticPauseBinding } from './staticPipelinePause.types';

export interface StaticPauseCard {
  /** React key: the pause id. */
  readonly key: string;
  /** `''` = coordinator; otherwise the spawning tool call id. */
  readonly instanceKey: string;
  /** The authored node the run paused at. */
  readonly nodeName: string;
  /** The leaf to continue (`tools` pauses); `undefined` for a root pause. */
  readonly pauseId: string | undefined;
}

export interface StaticPauseBucket {
  readonly instanceKey: string;
  readonly name: string;
  readonly label: string | undefined;
  readonly agentType: string;
}

type ActionRecord = Readonly<Record<string, unknown>> & { readonly toolMeta?: Readonly<Record<string, unknown>> };

function wrapperFor(toolActions: readonly unknown[], toolCallId: string): ActionRecord | undefined {
  return (toolActions as readonly ActionRecord[]).find((action) => action?.['id'] === toolCallId);
}

function nameOf(wrapper: ActionRecord | undefined): string {
  const value = wrapper?.['original_name'] ?? wrapper?.['name'];
  return typeof value === 'string' && value ? value : '';
}

/** Cards in inventory order, plus the buckets the `tools` leaves open. */
export function buildStaticPauseCards(
  pause: StaticPauseBinding | undefined,
  toolActions: readonly unknown[] = [],
  fallbackName = 'Pipeline',
): { readonly cards: readonly StaticPauseCard[]; readonly buckets: readonly StaticPauseBucket[] } {
  if (!pause) return { cards: [], buckets: [] };
  if (pause.kind === 'root') {
    return { cards: [{ key: pause.proof.pause_id, instanceKey: '', nodeName: pause.proof.node_name, pauseId: undefined }], buckets: [] };
  }
  const cards: StaticPauseCard[] = [];
  const paths = new Map<string, { name: string; path: readonly AgentPathTier[]; agentType: string }>();
  for (const leaf of pause.inventory.pauses) {
    cards.push({ key: leaf.proof.pause_id, instanceKey: leaf.tool_call_id, nodeName: leaf.proof.node_name, pauseId: leaf.proof.pause_id });
    if (paths.has(leaf.tool_call_id)) continue;
    const wrapper = wrapperFor(toolActions, leaf.tool_call_id);
    const name = nameOf(wrapper) || fallbackName;
    const owner = getActionOwnerPath(wrapper);
    paths.set(leaf.tool_call_id, {
      name,
      path: owner.length ? owner : [{ name, call_id: leaf.tool_call_id }],
      agentType: 'pipeline',
    });
  }
  const context = (toolActions as readonly ActionRecord[])
    .map((action, index) => ({ instanceKey: `trace-${index}`, agentPath: getActionOwnerPath(action) }))
    .filter((entry) => entry.agentPath.length > 0);
  const labels = computeBreadcrumbs([...paths].map(([instanceKey, entry]) => ({ instanceKey, agentPath: entry.path })), context);
  return {
    cards,
    buckets: [...paths].map(([instanceKey, entry]) => ({ instanceKey, name: entry.name, label: labels.get(instanceKey), agentType: entry.agentType })),
  };
}
