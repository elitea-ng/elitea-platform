import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { AGENT_STATE } from '../stream/frameTypes';
import {
  ONE_SHOT_MS,
  activeAnimatedParticipant,
  applyAgentStateFrame,
  avatarMotion,
  motionAllowed,
  newestSince,
  nextChangeAt,
  settleAvatarStates,
  shownState,
  type AvatarStates,
} from './avatarState';
import { MOTION_SPEC } from './motionSpec';

const frame = (participant_id: unknown, state: unknown) => ({ type: 'agent_state', participant_id, state });
const motion = { reducedMotion: false };

describe('applyAgentStateFrame', () => {
  it('records a state and when it began', () => {
    const states = applyAgentStateFrame({}, frame(7, 'thinking'), 100);
    assert.deepEqual(states, { 7: { state: 'thinking', since: 100 } });
  });

  it('keeps the same object for a repeat, another frame type or a malformed frame', () => {
    const states = applyAgentStateFrame({}, frame(7, 'thinking'), 100);
    assert.equal(applyAgentStateFrame(states, frame(7, 'thinking'), 200), states);
    assert.equal(applyAgentStateFrame(states, { type: 'agent_llm_chunk', participant_id: 7, state: 'speaking' }, 200), states);
    for (const bad of [frame('7', 'speaking'), frame(0, 'speaking'), frame(7, 'dancing'), frame(1.5, 'speaking')]) {
      assert.equal(applyAgentStateFrame(states, bad, 200), states);
    }
  });

  it('walks a turn: thinking, working, speaking, success', () => {
    let states: AvatarStates = {};
    for (const [at, state] of [[0, 'thinking'], [10, 'working'], [20, 'speaking'], [30, 'success']] as const) {
      states = applyAgentStateFrame(states, frame(7, state), at);
    }
    assert.deepEqual(states[7], { state: 'success', since: 30 });
  });
});

describe('shownState', () => {
  it('an unknown participant is idle', () => {
    assert.equal(shownState(undefined, 0), AGENT_STATE.Idle);
  });

  it('success and error show for ONE_SHOT_MS, then idle', () => {
    for (const state of [AGENT_STATE.Success, AGENT_STATE.Error]) {
      assert.equal(shownState({ state, since: 0 }, ONE_SHOT_MS - 1), state);
      assert.equal(shownState({ state, since: 0 }, ONE_SHOT_MS), AGENT_STATE.Idle);
    }
  });

  it('a lasting state does not expire', () => {
    assert.equal(shownState({ state: AGENT_STATE.Waiting, since: 0 }, 10 * ONE_SHOT_MS), AGENT_STATE.Waiting);
  });
});

describe('nextChangeAt', () => {
  it('is the earliest one-shot end still ahead', () => {
    const states: AvatarStates = { 1: { state: 'success', since: 100 }, 2: { state: 'error', since: 50 }, 3: { state: 'speaking', since: 0 } };
    assert.equal(nextChangeAt(states, 60), 50 + ONE_SHOT_MS);
    assert.equal(nextChangeAt(states, 50 + ONE_SHOT_MS), 100 + ONE_SHOT_MS);
    assert.equal(nextChangeAt(states, 100 + ONE_SHOT_MS), undefined);
  });
});

describe('activeAnimatedParticipant', () => {
  it('is the participant whose animated state began last', () => {
    const states: AvatarStates = { 1: { state: 'speaking', since: 10 }, 2: { state: 'working', since: 20 } };
    assert.equal(activeAnimatedParticipant(states, 30), 2);
  });

  it('skips an idle participant and an expired one-shot', () => {
    const states: AvatarStates = { 1: { state: 'speaking', since: 10 }, 2: { state: 'success', since: 20 }, 3: { state: 'idle', since: 30 } };
    assert.equal(activeAnimatedParticipant(states, 25), 2);
    assert.equal(activeAnimatedParticipant(states, 20 + ONE_SHOT_MS), 1);
    assert.equal(activeAnimatedParticipant({ 3: { state: 'idle', since: 0 } }, 0), undefined);
  });
});

describe('settleAvatarStates', () => {
  it('ends every looping state with the final one and keeps waiting and one-shots', () => {
    const states: AvatarStates = { 1: { state: 'speaking', since: 10 }, 2: { state: 'waiting', since: 11 }, 3: { state: 'success', since: 12 }, 4: { state: 'working', since: 13 } };
    assert.deepEqual(settleAvatarStates(states, 'success', 50), {
      1: { state: 'success', since: 50 }, 2: { state: 'waiting', since: 11 }, 3: { state: 'success', since: 12 }, 4: { state: 'success', since: 50 },
    });
    const quiet: AvatarStates = { 2: { state: 'waiting', since: 11 }, 5: { state: 'idle', since: 1 } };
    assert.equal(settleAvatarStates(quiet, 'idle', 50), quiet);
  });

  it('newestSince is the latest change, 0 for none', () => {
    assert.equal(newestSince({ 1: { state: 'speaking', since: 10 }, 2: { state: 'idle', since: 30 } }), 30);
    assert.equal(newestSince({}), 0);
  });
});

describe('avatarMotion', () => {
  const states: AvatarStates = { 1: { state: 'speaking', since: 10 }, 2: { state: 'thinking', since: 20 } };

  it('only the active participant newest message moves', () => {
    assert.equal(avatarMotion({ participantId: 2, newest: true }, states, 30, motion), 'thinking');
    assert.equal(avatarMotion({ participantId: 2, newest: false }, states, 30, motion), 'still');
    assert.equal(avatarMotion({ participantId: 1, newest: true }, states, 30, motion), 'still');
    assert.equal(avatarMotion({ participantId: undefined, newest: true }, states, 30, motion), 'still');
  });

  it('reduced motion and data-saver show the still', () => {
    assert.equal(avatarMotion({ participantId: 2, newest: true }, states, 30, { reducedMotion: true }), 'still');
    assert.equal(avatarMotion({ participantId: 2, newest: true }, states, 30, { reducedMotion: false, dataSaver: true }), 'still');
    assert.equal(motionAllowed({ reducedMotion: false, dataSaver: false }), true);
  });
});

describe('MOTION_SPEC', () => {
  it('has a motion for every state; only success and error are one-shots with a badge', () => {
    for (const state of Object.values(AGENT_STATE)) {
      const spec = MOTION_SPEC[state];
      assert.ok(spec.durationMs > 0, state);
      const oneShot = state === AGENT_STATE.Success || state === AGENT_STATE.Error;
      assert.equal(spec.loop, !oneShot, state);
      assert.equal(spec.badge !== undefined, oneShot, state);
      assert.ok(spec.durationMs <= ONE_SHOT_MS || spec.loop, state);
    }
  });
});
