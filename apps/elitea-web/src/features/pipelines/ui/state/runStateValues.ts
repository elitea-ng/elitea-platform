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
 */

export interface RunStateStep {
  readonly state?: Readonly<Record<string, unknown>>;
}

export interface StepStateValues {
  readonly before: unknown;
  readonly after: unknown;
  /** True for the last step of a run that is no longer in progress. */
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
