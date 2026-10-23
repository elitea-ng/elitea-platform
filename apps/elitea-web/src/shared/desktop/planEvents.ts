/**
 * The plan-mode events (#1225, IPC.md "Plan mode") picked out of the turn's
 * `AgentEvent` stream: `plan_updated`, `plan_proposed`, `plan_accepted`.
 */
import type { AgentEvent } from './workspaceIpc';

/** The plan-mode events (`plan_updated`, `plan_proposed`, `plan_accepted`). */
export type PlanEvent = Extract<AgentEvent, { kind: 'plan_updated' | 'plan_proposed' | 'plan_accepted' }>;

export function isPlanEvent(event: AgentEvent): event is PlanEvent {
  return event.kind === 'plan_updated' || event.kind === 'plan_proposed' || event.kind === 'plan_accepted';
}
