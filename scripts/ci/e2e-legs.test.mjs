import { test } from 'node:test';
import assert from 'node:assert/strict';
import { LEGS, SMOKE_LEG, outputs, selectLegs } from './e2e-legs.mjs';

const names = (legs) => legs.map((leg) => leg.leg);

test('a pull request the pool runs keeps only the image smoke legs', () => {
  const options = { event: 'pull_request', pooled: true, reachesWebkit: true };
  assert.deepEqual(names(selectLegs('e2e', options)), ['chromium-1of2']);
  assert.deepEqual(names(selectLegs('chat-stream-rust', options)), ['1of3']);
});

test('a run off the pool, or not a pull request, keeps every leg', () => {
  for (const options of [
    { event: 'pull_request', pooled: false, reachesWebkit: true },
    { event: 'schedule', pooled: true, reachesWebkit: true },
    { event: 'workflow_dispatch', pooled: false, reachesWebkit: true },
  ]) {
    assert.deepEqual(names(selectLegs('e2e', options)), names(LEGS.e2e), JSON.stringify(options));
    assert.deepEqual(names(selectLegs('chat-stream-rust', options)), names(LEGS['chat-stream-rust']));
  }
});

test('the webkit legs need a change that reaches webkit', () => {
  const options = { event: 'pull_request', pooled: false, reachesWebkit: false };
  assert.deepEqual(names(selectLegs('e2e', options)), ['chromium-1of2', 'chromium-2of2', 'platform-flags']);
  // chat-stream-rust has no webkit legs.
  assert.equal(selectLegs('chat-stream-rust', options).length, 3);
});

test('no run is left with an empty matrix, and the smoke legs exist', () => {
  for (const event of ['pull_request', 'schedule']) {
    for (const pooled of [true, false]) {
      for (const reachesWebkit of [true, false]) {
        for (const lane of Object.keys(LEGS)) {
          assert.ok(selectLegs(lane, { event, pooled, reachesWebkit }).length > 0, `${lane} ${event} ${pooled} ${reachesWebkit}`);
        }
      }
    }
  }
  for (const [lane, leg] of Object.entries(SMOKE_LEG)) assert.ok(names(LEGS[lane]).includes(leg));
});

test('every sharded leg carries one weight per shard', () => {
  for (const legs of Object.values(LEGS)) {
    for (const leg of legs) {
      if (!leg.shard) {
        assert.equal(leg.weights, '', leg.leg);
        continue;
      }
      assert.equal(leg.weights.split(':').length, Number(leg.shard.split('/')[1]), leg.leg);
    }
  }
});

test('prints one GITHUB_OUTPUT line per lane, as JSON', () => {
  const lines = outputs({ EVENT_NAME: 'pull_request', POOLED: 'true', REACHES_WEBKIT: 'false' });
  assert.deepEqual(lines.map((l) => l.split('=')[0]), ['e2e-legs', 'chat-stream-rust-legs']);
  assert.deepEqual(JSON.parse(lines[0].slice('e2e-legs='.length)), [LEGS.e2e[0]]);
});
