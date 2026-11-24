/**
 * The "Before" and "After" values the Run Details dialog shows for one state
 * variable at one timeline step (#6883 + EL-3734 grouped timelines).
 *
 * A timeline entry carries the state the run had when that step finished,
 * so "After" is the entry's own state and "Before" is the preceding entry in
 * the same execution scope (`findPreviousScopedStep` — outer steps compare
 * against the previous outer step, nested child steps compare against the
 * previous nested step with the same `scope_id`).
 *
 * THE TERMINAL STEP. The last outer step of a finished run often carries no
 * state of its own (`pipeline_finish` has no trailing transitional edge). For
 * that step (`!currentStep.is_nested && selectedStep === lastOuterIndex`),
 * "After" is the run's final outer state: the newest outer-step state the
 * timeline holds. The dialog labels it "Final state".
 */
import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import {
  findLastOuterStepIndex,
  findPreviousScopedStep,
} from '../../lib/run-state-timeline/timelineGrouping.helpers';
import type { GroupedTimelineStep } from '../../lib/run-state-timeline/timeline.types';

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
  readonly state?: Readonly<Record<string, unknown>> | undefined;
  readonly is_nested?: boolean | undefined;
  readonly scope_id?: string | undefined;
}

export interface StepStateValues {
  readonly before: unknown;
  readonly after: unknown;
  /** True for the last outer step of a terminal run (see `isRunFinished`). */
  readonly isFinal: boolean;
}

function hasVariable(step: RunStateStep | undefined, variable: string): boolean {
  return step?.state !== undefined && Object.prototype.hasOwnProperty.call(step.state, variable);
}

/** The newest value of `variable` across outer (non-nested) steps, scanning back from the end. */
function finalOuterValueOf(timeline: readonly RunStateStep[], variable: string): unknown {
  for (let index = timeline.length - 1; index >= 0; index -= 1) {
    const step = timeline[index];
    if (!step || step.is_nested) continue;
    if (hasVariable(step, variable)) return step.state?.[variable];
  }
  return undefined;
}

export function stepStateValues(
  timeline: readonly RunStateStep[],
  selectedStep: number,
  variable: string,
  runFinished: boolean,
): StepStateValues {
  const steps = timeline as readonly GroupedTimelineStep[];
  const currentStep = timeline[selectedStep];
  const previousStep = findPreviousScopedStep(steps, selectedStep);
  const before = previousStep ? previousStep.state?.[variable] : '';
  const lastOuterIndex = findLastOuterStepIndex(steps);
  const isFinal =
    runFinished &&
    !currentStep?.is_nested &&
    lastOuterIndex >= 0 &&
    selectedStep === lastOuterIndex;

  if (isFinal) return { before, after: finalOuterValueOf(timeline, variable), isFinal };
  return { before, after: currentStep?.state?.[variable], isFinal };
}
