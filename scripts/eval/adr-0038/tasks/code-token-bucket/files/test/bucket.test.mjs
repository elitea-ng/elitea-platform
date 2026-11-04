import test from 'node:test';
import assert from 'node:assert/strict';
import { TokenBucket } from '../src/bucket.mjs';

function makeClock(start = 1_000_000) {
  let t = start;
  return {
    now: () => t,
    advance(ms) {
      t += ms;
    },
    set(ms) {
      t = ms;
    },
  };
}

const drained = (opts) => {
  const clock = makeClock();
  const b = new TokenBucket({ ...opts, now: clock.now });
  assert.equal(b.tryRemove(opts.capacity), true, 'a new bucket starts full');
  return { b, clock };
};

test('starts full and refuses more than available without taking any', () => {
  const clock = makeClock();
  const b = new TokenBucket({ capacity: 5, refillPerSecond: 1, now: clock.now });
  assert.equal(b.available(), 5);
  assert.equal(b.tryRemove(6), false);
  assert.equal(b.available(), 5, 'failed tryRemove must not consume tokens');
  assert.equal(b.tryRemove(5), true);
  assert.equal(b.available(), 0);
});

test('fractional refill accumulates across frequent polls', () => {
  const { b, clock } = drained({ capacity: 10, refillPerSecond: 2 });
  clock.advance(250);
  assert.equal(b.tryRemove(), false, '0.5 tokens after 250ms');
  clock.advance(250);
  assert.equal(b.tryRemove(), true, '1 token after 500ms total');
});

test('available() floors partial tokens', () => {
  const { b, clock } = drained({ capacity: 10, refillPerSecond: 2 });
  clock.advance(1400);
  assert.equal(b.available(), 2, '2.8 tokens -> 2 whole tokens');
});

test('never refills beyond capacity', () => {
  const clock = makeClock();
  const b = new TokenBucket({ capacity: 3, refillPerSecond: 10, now: clock.now });
  clock.advance(60_000);
  assert.equal(b.available(), 3);
  assert.equal(b.tryRemove(4), false);
});

test('msUntil reports milliseconds, rounded up', () => {
  const { b, clock } = drained({ capacity: 10, refillPerSecond: 2 });
  assert.equal(b.msUntil(1), 500);
  clock.advance(100);
  assert.equal(b.msUntil(1), 400, 'msUntil must account for time already elapsed');
  assert.equal(b.msUntil(3), 1400);
});

test('msUntil rounds a fractional wait up to the next millisecond', () => {
  const { b } = drained({ capacity: 5, refillPerSecond: 3 });
  assert.equal(b.msUntil(1), 334);
});

test('msUntil is 0 when tokens are already there and Infinity above capacity', () => {
  const clock = makeClock();
  const b = new TokenBucket({ capacity: 4, refillPerSecond: 1, now: clock.now });
  assert.equal(b.msUntil(4), 0);
  assert.equal(b.msUntil(5), Infinity);
});

test('a clock going backwards does not remove tokens', () => {
  const clock = makeClock(10_000);
  const b = new TokenBucket({ capacity: 4, refillPerSecond: 1, now: clock.now });
  b.tryRemove(2);
  clock.set(5_000);
  assert.equal(b.available(), 2);
  assert.equal(b.tryRemove(2), true);
});

test('constructor validates options', () => {
  assert.throws(() => new TokenBucket({ capacity: 0, refillPerSecond: 1 }), RangeError);
  assert.throws(() => new TokenBucket({ capacity: 5, refillPerSecond: 0 }), RangeError);
  assert.throws(() => new TokenBucket({ capacity: 5, refillPerSecond: -1 }), RangeError);
  assert.throws(() => new TokenBucket({ capacity: Infinity, refillPerSecond: 1 }), RangeError);
});
