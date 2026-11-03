/**
 * The unattended-run marks of a persisted message (ADR-0045 decision 4): Main
 * writes `meta.continued_automatically` (with `meta.unattended.continuation_index`)
 * on the messages of a continuation that it started by itself, and
 * `meta.step_limit_reached` on a response that stopped at its step limit. A
 * live response carries the step-limit flag in its `response_metadata`.
 */
import type { ChatMessage } from './convertMessagesToChatHistory.types';

export interface UnattendedMarks {
  readonly continuedAutomatically?: true;
  readonly continuationIndex?: number;
  readonly stepLimitReached?: true;
}

const isRecord = (value: unknown): value is Readonly<Record<string, unknown>> => typeof value === 'object' && value !== null;

/** The marks in a message's `meta`; an empty object when it has none. */
export function unattendedMarksOf(meta: unknown): UnattendedMarks {
  if (!isRecord(meta)) return {};
  const index = isRecord(meta['unattended']) ? meta['unattended']['continuation_index'] : undefined;
  return {
    ...(meta['continued_automatically'] === true ? { continuedAutomatically: true } : {}),
    ...(meta['continued_automatically'] === true && typeof index === 'number' && Number.isInteger(index) && index > 0 ? { continuationIndex: index } : {}),
    ...(meta['step_limit_reached'] === true ? { stepLimitReached: true } : {}),
  };
}

/** True when the answer stopped at its step limit: the stored mark, or the live final frame's metadata. */
export function stoppedAtStepLimit(message: Pick<ChatMessage, 'stepLimitReached' | 'responseMetadata'>): boolean {
  return message.stepLimitReached === true || message.responseMetadata?.['step_limit_reached'] === true;
}
