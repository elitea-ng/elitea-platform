import test from 'node:test';
import assert from 'node:assert/strict';
import { LRUCache } from '../src/lru.mjs';

function clock(start = 0) {
  let t = start;
  return { now: () => t, tick: (ms) => (t += ms) };
}

test('evicts the least recently used entry', () => {
  const c = new LRUCache({ max: 2 });
  c.set('a', 1).set('b', 2);
  c.get('a');
  c.set('c', 3);
  assert.deepEqual(c.keys(), ['c', 'a'], 'b was least recently used');
});

test('overwriting a key makes it most recently used', () => {
  const c = new LRUCache({ max: 2 });
  c.set('a', 1).set('b', 2).set('a', 10).set('c', 3);
  assert.equal(c.get('a'), 10);
  assert.equal(c.has('b'), false, 'b should have been evicted, not a');
});

test('has() does not change recency', () => {
  const c = new LRUCache({ max: 2 });
  c.set('a', 1).set('b', 2);
  assert.equal(c.has('a'), true);
  c.set('c', 3);
  assert.equal(c.has('a'), false, 'has() must not protect a from eviction');
  assert.deepEqual(c.keys(), ['c', 'b']);
});

test('falsy values are stored and reported', () => {
  const c = new LRUCache({ max: 3 });
  c.set('zero', 0).set('empty', '').set('undef', undefined);
  assert.equal(c.has('zero'), true);
  assert.equal(c.has('empty'), true);
  assert.equal(c.has('undef'), true);
  assert.equal(c.get('zero'), 0);
});

test('entries expire exactly at insertedAt + ttl', () => {
  const k = clock();
  const c = new LRUCache({ max: 5, ttlMs: 100, now: k.now });
  c.set('a', 1);
  k.tick(99);
  assert.equal(c.get('a'), 1);
  k.tick(1);
  assert.equal(c.get('a'), undefined);
  assert.equal(c.has('a'), false);
});

test('get() does not extend the expiry', () => {
  const k = clock();
  const c = new LRUCache({ max: 5, ttlMs: 100, now: k.now });
  c.set('a', 1);
  k.tick(60);
  assert.equal(c.get('a'), 1);
  k.tick(60);
  assert.equal(c.get('a'), undefined, 'expiry is measured from set(), not from the last get()');
});

test('set() on an existing key restarts the expiry; per-call ttl overrides default', () => {
  const k = clock();
  const c = new LRUCache({ max: 5, ttlMs: 100, now: k.now });
  c.set('a', 1);
  k.tick(80);
  c.set('a', 2);
  k.tick(80);
  assert.equal(c.get('a'), 2);
  c.set('long', 'x', { ttlMs: 1000 });
  c.set('forever', 'y', { ttlMs: 0 });
  k.tick(500);
  assert.equal(c.get('long'), 'x');
  k.tick(10_000);
  assert.equal(c.get('forever'), 'y');
  assert.equal(c.get('long'), undefined);
});

test('size and keys() ignore expired entries', () => {
  const k = clock();
  const c = new LRUCache({ max: 5, now: k.now });
  c.set('short', 1, { ttlMs: 10 });
  c.set('keep', 2);
  assert.equal(c.size, 2);
  k.tick(10);
  assert.equal(c.size, 1);
  assert.deepEqual(c.keys(), ['keep']);
});

test('expired entries are dropped before evicting a live LRU entry', () => {
  const k = clock();
  const c = new LRUCache({ max: 2, now: k.now });
  c.set('live-old', 1);
  c.set('stale', 2, { ttlMs: 5 });
  k.tick(5);
  c.set('new', 3);
  assert.equal(c.get('live-old'), 1, 'the live entry must survive; the expired one makes room');
  assert.deepEqual(c.keys(), ['live-old', 'new']);
});
