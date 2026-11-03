/**
 * The thread's recorded turns that ran when the app stopped (ADR-0045 decision 7), as an open page follows them. The
 * page names its recorded turns (`watch`). A host resume of one of them (`turn_continued` with `kind` `resume`, on the
 * old turn) makes the new turn the page's active turn; a `resume_offered` makes the old turn offer Continue; and a turn
 * that was continued, by the host or by Continue here, offers Continue no more. A turn that Continue here started has
 * no prompt of the person above it.
 */
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

export interface ResumeState {
  /** The page's recorded turns. */
  watched: readonly string[];
  /** Watched turns the host offered Continue for after the page read them (`resume_offered`). */
  offered: readonly string[];
  /** Turns that were continued already (by the host, or by Continue here): no Continue any more. */
  moved: readonly string[];
  /** The turns that Continue here started. */
  started: readonly string[];
}

export const NO_RESUMES: ResumeState = { watched: [], offered: [], moved: [], started: [] };

const plus = (list: readonly string[], id: string): readonly string[] => (list.includes(id) ? list : [...list, id]);

/** The turn that a host resume of a watched turn started; null for any other event. */
function resumedTurn(resumes: ResumeState, event: AgentEvent): string | null {
  if (event.kind !== 'turn_continued' || event.payload.kind !== 'resume' || !resumes.watched.includes(event.turn_id)) return null;
  return event.payload.next_turn_id;
}

/** `resumes` after `event` (the same object when it changes nothing). */
function withResumeEvent(resumes: ResumeState, event: AgentEvent): ResumeState {
  if (event.kind === 'resume_offered' && resumes.watched.includes(event.turn_id) && !resumes.offered.includes(event.turn_id)) {
    return { ...resumes, offered: [...resumes.offered, event.turn_id] };
  }
  if (resumedTurn(resumes, event) !== null && !resumes.moved.includes(event.turn_id)) return { ...resumes, moved: [...resumes.moved, event.turn_id] };
  return resumes;
}

/** Continue here started `turnId` from the recorded turn `from` (absent: a start of the person's; `state` as it is). */
export function resumedHere<S extends { resumes: ResumeState }>(state: S, from: string | undefined, turnId: string): S {
  if (from === undefined) return state;
  const { resumes } = state;
  return { ...state, resumes: { ...resumes, moved: plus(resumes.moved, from), started: plus(resumes.started, turnId) } };
}

/** The part of the turn reducer's state that following a resume reads and changes. */
interface Following {
  begun: readonly string[];
  resumes: ResumeState;
  turns: Partial<Record<string, { events: readonly AgentEvent[] }>>;
}

/** The resume part of `event`: a host resume of a watched recorded turn makes the new turn the active one (`begin`). */
export function withResume<S extends Following>(state: S, event: AgentEvent, begin: (state: S, turnId: string) => S): S {
  const resumes = withResumeEvent(state.resumes, event);
  if (resumes === state.resumes) return state;
  const resumed = resumedTurn(state.resumes, event);
  return resumed === null || state.begun.includes(resumed) ? { ...state, resumes } : begin({ ...state, resumes }, resumed);
}

/** The page names its recorded turns; a resume heard before that (kept with the old turn's events) is followed now. */
export function watchResumes<S extends Following>(state: S, turnIds: readonly string[], begin: (state: S, turnId: string) => S): S {
  let next: S = { ...state, resumes: { ...state.resumes, watched: turnIds } };
  for (const turnId of turnIds) for (const event of state.turns[turnId]?.events ?? []) next = withResume(next, event, begin);
  return next;
}

/** Whether the recorded turn `turnId` offers Continue: the host kept its record (`resumable`, or offered since), and it was not continued. */
export function offersResume(resumes: ResumeState, turnId: string, resumable: boolean): boolean {
  return (resumable || resumes.offered.includes(turnId)) && !resumes.moved.includes(turnId);
}
