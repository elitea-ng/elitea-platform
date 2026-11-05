import test from 'node:test';
import assert from 'node:assert/strict';
import { parse, compile, get, set, remove } from '../src/pointer.mjs';

// The example document from RFC 6901 section 5.
const rfcDoc = () => ({
  foo: ['bar', 'baz'],
  '': 0,
  'a/b': 1,
  'c%d': 2,
  'e^f': 3,
  'g|h': 4,
  'i\\j': 5,
  'k"l': 6,
  ' ': 7,
  'm~n': 8,
});

test('parse handles root, empty tokens and escapes', () => {
  assert.deepEqual(parse(''), []);
  assert.deepEqual(parse('/'), ['']);
  assert.deepEqual(parse('/a~1b/m~0n'), ['a/b', 'm~n']);
  assert.deepEqual(parse('/~01'), ['~1'], '~01 must decode to the literal "~1"');
});

test('parse rejects malformed pointers', () => {
  assert.throws(() => parse('foo'), Error);
  assert.throws(() => parse('/a~2b'), Error);
  assert.throws(() => parse('/trailing~'), Error);
});

test('compile is the inverse of parse', () => {
  assert.equal(compile(['a/b', 'm~n', '']), '/a~1b/m~0n/');
  assert.equal(compile(['~1']), '/~01');
  assert.equal(compile([]), '');
});

test('get resolves every RFC 6901 example', () => {
  const doc = rfcDoc();
  assert.deepEqual(get(doc, ''), doc);
  assert.deepEqual(get(doc, '/foo'), ['bar', 'baz']);
  assert.equal(get(doc, '/foo/0'), 'bar');
  assert.equal(get(doc, '/'), 0);
  assert.equal(get(doc, '/a~1b'), 1);
  assert.equal(get(doc, '/c%d'), 2);
  assert.equal(get(doc, '/ '), 7);
  assert.equal(get(doc, '/m~0n'), 8);
});

test('get returns undefined for missing paths and prototype keys', () => {
  const doc = rfcDoc();
  assert.equal(get(doc, '/nope'), undefined);
  assert.equal(get(doc, '/foo/2'), undefined);
  assert.equal(get(doc, '/foo/-'), undefined);
  assert.equal(get(doc, '/foo/01'), undefined, 'leading zero is not an index');
  assert.equal(get(doc, '/nope/deeper'), undefined);
  assert.equal(get({}, '/constructor'), undefined);
  assert.equal(get({ a: [1] }, '/a/length'), undefined);
});

test('set replaces root and object members', () => {
  assert.equal(set({ a: 1 }, '', 42), 42);
  const doc = { a: { b: 1 } };
  assert.equal(set(doc, '/a/b', 2), doc);
  set(doc, '/a/c~1d', 3);
  assert.deepEqual(doc, { a: { b: 2, 'c/d': 3 } });
});

test('set on arrays: replace, append with length or -', () => {
  const doc = { list: ['x', 'y'] };
  set(doc, '/list/0', 'X');
  set(doc, '/list/2', 'z');
  set(doc, '/list/-', 'w');
  assert.deepEqual(doc.list, ['X', 'y', 'z', 'w']);
});

test('set rejects bad array indices and missing parents', () => {
  assert.throws(() => set({ l: [] }, '/l/1', 'v'), Error, 'index beyond length');
  assert.throws(() => set({ l: [1, 2] }, '/l/01', 'v'), Error, 'leading zero');
  assert.throws(() => set({ l: [1] }, '/l/x', 'v'), Error, 'non-numeric');
  assert.throws(() => set({}, '/missing/child', 1), Error, 'no auto-create');
});

test('remove from object and array', () => {
  const doc = { a: 1, list: ['p', 'q', 'r'] };
  assert.equal(remove(doc, '/a'), 1);
  assert.equal(remove(doc, '/list/1'), 'q');
  assert.deepEqual(doc, { list: ['p', 'r'] });
});

test('remove errors on missing targets, "-" and root', () => {
  assert.throws(() => remove({ a: 1 }, '/b'), Error);
  assert.throws(() => remove({ l: [1] }, '/l/1'), Error);
  assert.throws(() => remove({ l: [1] }, '/l/-'), Error);
  assert.throws(() => remove({ a: 1 }, ''), Error);
});
