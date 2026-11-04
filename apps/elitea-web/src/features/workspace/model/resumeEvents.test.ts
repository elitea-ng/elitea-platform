import { describe, expect, it } from 'vitest';

import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import { offersResume } from './resumeEvents';
import { activeView, earlierViews, initialTurnsState, turnReducer, type TurnAction, type TurnsState } from './turnReducer';

const OLD = 'turn-before-restart';
const NEXT = 'turn-resumed';

const apply = (actions: TurnAction[], from: TurnsState = initialTurnsState): TurnsState => actions.reduce(turnReducer, from);
const event = (e: AgentEvent): TurnAction => ({ type: 'event', event: e });
const resumedBy = (kind: 'resume' | 'step_limit' = 'resume'): AgentEvent => ({
  turn_id: OLD,
  seq: 7,
  kind: 'turn_continued',
  payload: { next_turn_id: NEXT, execution_id: 'e9', continuation_index: 3, chain_root_execution_id: 'root', kind, resumed_after_restart: true },
});
const offered: AgentEvent = { turn_id: OLD, seq: 7, kind: 'resume_offered', payload: { workspace_id: 'w1', conversation_id: 42 } };

describe('resume after restart (ADR-0045 decision 7)', () => {
  it('follows a host resume of a recorded turn the page watches, also one heard before the page named its turns', () => {
    // Another page's turn (not watched): nothing follows.
    expect(apply([event(resumedBy())]).activeTurnId).toBeNull();
    // Watched first, then heard.
    const watched = apply([{ type: 'watch', turnIds: [OLD] }, event(resumedBy())]);
    expect(watched.activeTurnId).toBe(NEXT);
    expect(watched.begun).toEqual([NEXT]);
    expect(offersResume(watched.resumes, OLD, true)).toBe(false);
    // Heard first, then watched.
    const early = apply([event(resumedBy()), { type: 'watch', turnIds: [OLD] }]);
    expect(early.activeTurnId).toBe(NEXT);
    expect(offersResume(early.resumes, OLD, true)).toBe(false);
    // Only a resume: a recorded turn's other link kinds are not this page's to follow.
    expect(apply([{ type: 'watch', turnIds: [OLD] }, event(resumedBy('step_limit'))]).activeTurnId).toBeNull();
  });

  it('offers Continue on a watched turn the host offered it for, until Continue here starts the next turn', () => {
    const state = apply([{ type: 'watch', turnIds: [OLD] }, event(offered)]);
    expect(offersResume(state.resumes, OLD, false)).toBe(true);
    expect(offersResume(state.resumes, 'other', false)).toBe(false);
    const continued = apply([{ type: 'begin', turnId: NEXT, resumedFrom: OLD }], state);
    expect(offersResume(continued.resumes, OLD, true)).toBe(false);
    // The turn Continue started stands without a prompt of the person.
    expect(activeView(continued).resumed).toBe(true);
    const after = apply([{ type: 'begin', turnId: 'turn-3' }], continued);
    expect(earlierViews(after)[0]?.view.resumed).toBe(true);
    expect(activeView(after).resumed).toBeUndefined();
  });
});
