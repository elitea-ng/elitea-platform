import test from 'node:test';
import assert from 'node:assert/strict';
import { Emitter } from '../src/emitter.mjs';

test('calls listeners in order with args and reports whether any ran', () => {
  const e = new Emitter();
  const calls = [];
  e.on('x', (a, b) => calls.push(['first', a, b]));
  e.on('x', (a, b) => calls.push(['second', a, b]));
  assert.equal(e.emit('x', 1, 2), true);
  assert.deepEqual(calls, [
    ['first', 1, 2],
    ['second', 1, 2],
  ]);
  assert.equal(e.emit('nobody'), false);
});

test('a listener removing itself does not make the next one skip', () => {
  const e = new Emitter();
  const calls = [];
  const a = () => {
    calls.push('a');
    e.off('x', a);
  };
  e.on('x', a);
  e.on('x', () => calls.push('b'));
  e.emit('x');
  assert.deepEqual(calls, ['a', 'b']);
});

test('removing a later listener during emit only affects later emits', () => {
  const e = new Emitter();
  const calls = [];
  const b = () => calls.push('b');
  e.on('x', () => {
    calls.push('a');
    e.off('x', b);
  });
  e.on('x', b);
  e.emit('x');
  e.emit('x');
  assert.deepEqual(calls, ['a', 'b', 'a']);
});

test('adding a listener during emit only affects later emits', () => {
  const e = new Emitter();
  const calls = [];
  let added = false;
  e.on('x', () => {
    calls.push('a');
    if (!added) {
      added = true;
      e.on('x', () => calls.push('late'));
    }
  });
  e.emit('x');
  e.emit('x');
  assert.deepEqual(calls, ['a', 'a', 'late']);
});

test('once runs only once, even with a re-entrant emit', () => {
  const e = new Emitter();
  let n = 0;
  e.once('x', () => {
    n++;
    e.emit('x');
  });
  e.emit('x');
  e.emit('x');
  assert.equal(n, 1);
  assert.equal(e.listenerCount('x'), 0);
});

test('a once listener removed during dispatch still runs in that dispatch', () => {
  const e = new Emitter();
  const calls = [];
  const later = () => calls.push('later');
  e.on('x', () => {
    calls.push('first');
    e.off('x', later);
  });
  e.once('x', later);
  e.emit('x');
  e.emit('x');
  assert.deepEqual(calls, ['first', 'later', 'first']);
});

test('off() with the original function removes a once registration', () => {
  const e = new Emitter();
  let n = 0;
  const f = () => n++;
  e.once('x', f);
  e.off('x', f);
  e.emit('x');
  assert.equal(n, 0);
  assert.equal(e.listenerCount('x'), 0);
});

test('off() removes only the most recently added registration of fn', () => {
  const e = new Emitter();
  let n = 0;
  const f = () => n++;
  e.on('x', f);
  e.once('x', f);
  e.off('x', f);
  e.emit('x');
  e.emit('x');
  assert.equal(n, 2, 'the on() registration must remain');
});

test('unsubscribe removes exactly its own registration and is idempotent', () => {
  const e = new Emitter();
  const f = () => {};
  const unsub1 = e.on('x', f);
  e.on('x', f);
  unsub1();
  unsub1();
  assert.equal(e.listenerCount('x'), 1);
});

test('unhandled error events throw', () => {
  const e = new Emitter();
  const boom = new Error('boom');
  assert.throws(() => e.emit('error', boom), (err) => err === boom);
  assert.throws(() => e.emit('error', 'disk full'), (err) => err instanceof Error && /disk full/.test(err.message));
  e.on('error', () => {});
  assert.equal(e.emit('error', boom), true);
});
