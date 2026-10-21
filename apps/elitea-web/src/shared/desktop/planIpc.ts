/**
 * Plan mode on the desktop (#1225, IPC.md "Plan mode"): the agent's
 * checklist (`plan_updated`), the plan `exit_plan_mode` proposes
 * (`plan_proposed`) and its acceptance (`plan_accepted`). The events
 * themselves are in `planEvents.ts` (this file is imported by
 * `workspaceIpc.ts`, so it imports nothing from it).
 */
/** One step of the agent's checklist (`plan_updated`). */
export interface PlanStep {
  n: number;
  step: string;
  status: 'pending' | 'in_progress' | 'completed' | 'cancelled';
}

/** The agent's checklist as `plan_updated` sends it: the whole plan each time. */
export interface PlanView {
  title: string;
  explanation?: string | undefined;
  status: 'in_progress' | 'completed';
  steps: PlanStep[];
}

/** `plan_proposed`: the agent's `exit_plan_mode`, answered with `accept` or `decline` (+ feedback). */
export interface PlanProposalPayload {
  request_id: string;
  call_id: string;
  /** Markdown, model-written: render without HTML. */
  plan: string;
}
