/**
 * The "Before" and "After" values the Run Details dialog shows for one state
 * variable at one timeline step (#6883).
 *
 * A timeline entry carries the state the run had when that step finished,
 * so "After" is the entry's own state and "Before" is the entry before it.
 *
 * THE TERMINAL STEP. The last step of a finished run often carries no state
 * of its own. A terminal agent node finishes on `pipeline_finish`, which has
 * no transitional edge after it to copy the state onto the node's entry, and
 * the state of the agent's last edge lands on the agent's last TOOL entry
 * (both share the agent's `langgraph_node`). The dialog then showed an empty
 * "After", or the step before it, and never the final state the run produced.
 * For that step, "After" is the run's final state: the newest state the
 * timeline holds. The dialog labels it "Final state".
 *
 * Only a TERMINAL run has a final state (`isRunFinished`). `Interrupt` is a
 * run paused at a human-in-the-loop node: it resumes later and produces a
 * different state, so its last step keeps the plain "After" and its own
 * value. An unknown status is treated the same way.
 */
import { FlowEditorConstants } from '../../lib/flow-editor/constants';

const TERMINAL_RUN_STATUSES: ReadonlySet<string> = new Set([
  FlowEditorConstants.PipelineStatus.Completed,
  FlowEditorConstants.PipelineStatus.Error,
  FlowEditorConstants.PipelineStatus.Stopped,
]);

/** True when a run with this status has ended and will not change again. */
export function isRunFinished(status: string | undefined): boolean {
  return status !== undefined && TERMINAL_RUN_STATUSES.has(status);
}

export interface RunStateStep {
  readonly state?: Readonly<Record<string, unknown>>;
}

export interface StepStateValues {
  readonly before: unknown;
  readonly after: unknown;
  /** True for the last step of a terminal run (see `isRunFinished`). */
  readonly isFinal: boolean;
}

function hasVariable(step: RunStateStep | undefined, variable: string): boolean {
  return step?.state !== undefined && Object.prototype.hasOwnProperty.call(step.state, variable);
}

/** The newest value of `variable` in the timeline, scanning back from the end. */
function finalValueOf(timeline: readonly RunStateStep[], variable: string): unknown {
  for (let index = timeline.length - 1; index >= 0; index -= 1) {
    const step = timeline[index];
    if (hasVariable(step, variable)) return step?.state?.[variable];
  }
  return undefined;
}

export function stepStateValues(
  timeline: readonly RunStateStep[],
  selectedStep: number,
  variable: string,
  runFinished: boolean,
): StepStateValues {
  const before = selectedStep > 0 ? timeline[selectedStep - 1]?.state?.[variable] : '';
  const isFinal = runFinished && timeline.length > 0 && selectedStep === timeline.length - 1;
  if (isFinal) return { before, after: finalValueOf(timeline, variable), isFinal };
  return { before, after: timeline[selectedStep]?.state?.[variable], isFinal };
}

/**
 * 5b-1 (D12): the timeline steps whose node's failure the run settled
 * through its settings. A step's own state holds `node_errors` as it was when
 * that visit finished, so the visit that appended an entry naming its own node
 * is the one marked: a node visited again without failing is not. The steps
 * carry no checkpoint step, so the append itself keys the visit. The dialog
 * marks those steps "Failed · continued" with the existing failed styling.
 */
export function continuedFailureSteps(timeline: readonly (RunStateStep & { readonly id?: string })[]): ReadonlySet<number> {
  const marked = new Set<number>();
  let seen = 0;
  timeline.forEach((step, index) => {
    const entries = step.state?.['node_errors'];
    if (!Array.isArray(entries)) return;
    const appended = entries.slice(seen);
    seen = Math.max(seen, entries.length);
    if (appended.some((entry: unknown) => isContinuedEntryOf(entry, step.id))) marked.add(index);
  });
  return marked;
}

function isContinuedEntryOf(entry: unknown, node: string | undefined): boolean {
  if (typeof entry !== 'object' || entry === null || node === undefined) return false;
  const { node: owner, handled } = entry as { readonly node?: unknown; readonly handled?: unknown };
  return owner === node && (handled === 'continue' || handled === 'error_output');
}
