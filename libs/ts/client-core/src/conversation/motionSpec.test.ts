import assert from 'node:assert/strict';
import { test } from 'node:test';

import { BRAND_TOKENS } from '../brand/tokens.gen';
import { AGENT_STATE } from '../stream/frameTypes';
import { HANDOFF_MS, MOTION_EASING, MOTION_SPEC, tokenMs } from './motionSpec';

test('MOTION_SPEC reads its durations from the motion.avatar tokens', () => {
  const avatar = BRAND_TOKENS.motion.avatar;
  for (const state of ['idle', 'thinking', 'speaking', 'working', 'waiting', 'waking'] as const) {
    assert.equal(MOTION_SPEC[state].durationMs, tokenMs(avatar[state].duration, -1), state);
    assert.equal(MOTION_SPEC[state].loop, avatar[state].loop, state);
  }
  for (const state of [AGENT_STATE.Success, AGENT_STATE.Error]) {
    assert.equal(MOTION_SPEC[state].durationMs, tokenMs(avatar.oneShot.duration, -1));
    assert.equal(MOTION_SPEC[state].loop, false);
  }
  assert.equal(HANDOFF_MS, tokenMs(BRAND_TOKENS.motion.handoff, -1));
  assert.equal(MOTION_EASING, BRAND_TOKENS.motion.easing);
});

test('the default tokens keep the durations the avatars always had', () => {
  const durations = Object.fromEntries(Object.entries(MOTION_SPEC).map(([state, spec]) => [state, [spec.durationMs, spec.loop]]));
  assert.deepEqual(durations, {
    idle: [4000, true],
    thinking: [1200, true],
    speaking: [900, true],
    working: [1600, true],
    waiting: [2000, true],
    success: [400, false],
    error: [400, false],
    waking: [1400, true],
  });
  assert.equal(HANDOFF_MS, 400);
});

test('a duration token reads ms, seconds or a bare number, and falls back on anything else', () => {
  assert.equal(tokenMs(' 250ms ', 0), 250);
  assert.equal(tokenMs('1.2s', 0), 1200);
  assert.equal(tokenMs('300', 0), 300);
  // A brand value that is no duration falls back to the default timing instead of breaking every client at import.
  assert.equal(tokenMs('fast', 400), 400);
  assert.equal(tokenMs(undefined, 900), 900);
});
