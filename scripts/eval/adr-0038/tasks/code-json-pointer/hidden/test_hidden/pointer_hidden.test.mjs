import test from 'node:test';
import assert from 'node:assert/strict';
import { parse, compile, get, set, remove } from '../src/pointer.mjs';

test('hidden: escape order on tricky tokens round-trips', () => {
  const tokens = ['~', '/', '~/', '/~', '~0', '~1', '~~//', '', 'plain'];
  for (const t of tokens) {
    assert.deepEqual(parse(compile([t])), [t], `token ${JSON.stringify(t)}`);
  }
  assert.equal(compile(['~/']), '/~0~1');
  assert.deepEqual(parse('/~10'), ['/0']);
});

test('hidden: malformed pointers throw in get/set/remove too', () => {
  assert.throws(() => get({ a: 1 }, 'a'), Error);
  assert.throws(() => get({ a: 1 }, '/a~'), Error);
  assert.throws(() => set({}, 'x', 1), Error);
  assert.throws(() => remove({ a: 1 }, '/~3'), Error);
});

test('hidden: numeric-looking keys on objects are plain keys', () => {
  const doc = { '01': 'a', '-': 'dash', '1e0': 'sci' };
  assert.equal(get(doc, '/01'), 'a');
  assert.equal(get(doc, '/-'), 'dash');
  set(doc, '/-', 'changed');
  assert.equal(doc['-'], 'changed');
  assert.equal(remove(doc, '/1e0'), 'sci');
});

test('hidden: invalid index forms against arrays', () => {
  const doc = { l: [10, 20] };
  for (const bad of ['-1', '1e0', ' 1', '+1', '1.0', '00']) {
    assert.equal(get(doc, `/l/${bad}`), undefined, `get ${bad}`);
    assert.throws(() => set({ l: [10, 20] }, `/l/${bad}`, 0), Error, `set ${bad}`);
    assert.throws(() => remove({ l: [10, 20] }, `/l/${bad}`), Error, `remove ${bad}`);
  }
});

test('hidden: falsy values are found, not treated as missing', () => {
  const doc = { a: { z: 0, n: null, f: false, e: '' }, l: [0, null] };
  assert.equal(get(doc, '/a/z'), 0);
  assert.equal(get(doc, '/a/n'), null);
  assert.equal(get(doc, '/a/f'), false);
  assert.equal(get(doc, '/l/1'), null);
  assert.equal(remove(doc, '/a/n'), null);
  assert.equal('n' in doc.a, false);
});

test('hidden: deep set through arrays and objects', () => {
  const doc = { a: [{ b: [] }] };
  set(doc, '/a/0/b/-', { c: 1 });
  set(doc, '/a/0/b/0/d', 2);
  assert.deepEqual(doc, { a: [{ b: [{ c: 1, d: 2 }] }] });
  assert.throws(() => set(doc, '/a/1/b', 1), Error, 'parent a/1 does not exist');
  assert.throws(() => set(doc, '/a/0/b/0/c/x', 1), Error, 'parent is a number');
});

test('hidden: prototype keys are not traversed or removed', () => {
  assert.equal(get({}, '/toString'), undefined);
  assert.equal(get({}, '/__proto__'), undefined);
  assert.throws(() => remove({}, '/toString'), Error);
  assert.throws(() => set({}, '/hasOwnProperty/x', 1), Error);
});
