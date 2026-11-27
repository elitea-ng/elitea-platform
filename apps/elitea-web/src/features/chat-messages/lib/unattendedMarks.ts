/**
 * The unattended-run marks of a persisted message (ADR-0045 decision 4): Main
 * writes `meta.continued_automatically` (with `meta.unattended.continuation_index`)
 * on the messages of a continuation that it started by itself, and
 * `meta.step_limit_reached` on a response that stopped at its step limit. A
 * live response carries the step-limit flag in its `response_metadata`.
 * Decision 11 adds `meta.run_brief` on the chain root's answer and
 * `meta.goal_check` on the answer of a checked run.
 */
import { readGoalCheck, readRunBrief, type GoalCheckView, type RunBriefView } from '@/entities/run-brief';
import { projectInstructionsOf, ranInFullAuto, ranOutsideSandbox, type ProjectInstructionsRecord } from '@/shared/lib/localWork';

import type { ChatMessage } from './convertMessagesToChatHistory.types';

export interface UnattendedMarks {
  readonly continuedAutomatically?: true;
  readonly continuationIndex?: number;
  readonly stepLimitReached?: true;
  readonly runBrief?: RunBriefView;
  readonly goalCheck?: GoalCheckView;
  /** `meta.local_work.ran_outside_sandbox` (ADR-0061 decision 5): a Local work turn ran a command outside the sandbox. */
  readonly ranOutsideSandbox?: true;
  /** `meta.local_work.full_auto` (ADR-0061 amendment of 2026-11-26): a Local work turn ran in Full auto. */
  readonly fullAuto?: true;
  /** `meta.local_work.project_instructions` (issue 3130): whether a Local work turn applied its folder's AGENTS.md. */
  readonly projectInstructions?: ProjectInstructionsRecord;
}

const isRecord = (value: unknown): value is Readonly<Record<string, unknown>> => typeof value === 'object' && value !== null;

/** The marks in a message's `meta`; an empty object when it has none. */
export function unattendedMarksOf(meta: unknown): UnattendedMarks {
  if (!isRecord(meta)) return {};
  const index = isRecord(meta['unattended']) ? meta['unattended']['continuation_index'] : undefined;
  const runBrief = readRunBrief(meta['run_brief']);
  const goalCheck = readGoalCheck(meta['goal_check']);
  const projectInstructions = projectInstructionsOf(meta);
  return {
    ...(meta['continued_automatically'] === true ? { continuedAutomatically: true } : {}),
    ...(meta['continued_automatically'] === true && typeof index === 'number' && Number.isInteger(index) && index > 0 ? { continuationIndex: index } : {}),
    ...(meta['step_limit_reached'] === true ? { stepLimitReached: true } : {}),
    ...(runBrief === undefined ? {} : { runBrief }),
    ...(goalCheck === undefined ? {} : { goalCheck }),
    ...(ranOutsideSandbox(meta) ? { ranOutsideSandbox: true } : {}),
    ...(ranInFullAuto(meta) ? { fullAuto: true } : {}),
    ...(projectInstructions === undefined ? {} : { projectInstructions }),
  };
}

/** True when the answer stopped at its step limit: the stored mark, or the live final frame's metadata. */
export function stoppedAtStepLimit(message: Pick<ChatMessage, 'stepLimitReached' | 'responseMetadata'>): boolean {
  return message.stepLimitReached === true || message.responseMetadata?.['step_limit_reached'] === true;
}
