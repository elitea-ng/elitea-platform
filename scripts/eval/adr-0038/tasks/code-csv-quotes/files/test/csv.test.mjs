import test from 'node:test';
import assert from 'node:assert/strict';
import { parseCSV, stringifyCSV } from '../src/csv.mjs';

test('parses simple records', () => {
  assert.deepEqual(parseCSV('a,b,c\n1,2,3\n'), [
    ['a', 'b', 'c'],
    ['1', '2', '3'],
  ]);
});

test('empty input yields no records', () => {
  assert.deepEqual(parseCSV(''), []);
});

test('doubled quotes inside a quoted field become one quote', () => {
  assert.deepEqual(parseCSV('"say ""hi""",x'), [['say "hi"', 'x']]);
});

test('quoted field may contain the delimiter', () => {
  assert.deepEqual(parseCSV('"a,b",c'), [['a,b', 'c']]);
});

test('CRLF line endings do not leak \\r into fields', () => {
  assert.deepEqual(parseCSV('a,b\r\nc,d\r\n'), [
    ['a', 'b'],
    ['c', 'd'],
  ]);
});

test('quoted field can span lines', () => {
  assert.deepEqual(parseCSV('id,note\n1,"line one\nline two"\n2,x'), [
    ['id', 'note'],
    ['1', 'line one\nline two'],
    ['2', 'x'],
  ]);
});

test('trailing empty field on the last record is kept', () => {
  assert.deepEqual(parseCSV('a,b,\n1,2,'), [
    ['a', 'b', ''],
    ['1', '2', ''],
  ]);
});

test('quoted empty field is an empty string', () => {
  assert.deepEqual(parseCSV('"",x'), [['', 'x']]);
});

test('unterminated quote throws', () => {
  assert.throws(() => parseCSV('a,"oops\n'), /unterminated/i);
});

test('stringify quotes and escapes when needed and uses CRLF', () => {
  const out = stringifyCSV([
    ['plain', 'with,comma', 'with "quote"'],
    ['multi\nline', null, 3],
  ]);
  assert.equal(out, 'plain,"with,comma","with ""quote"""\r\n"multi\nline",,3');
});

test('round trip preserves tricky values', () => {
  const rows = [
    ['x', '"', '""', ''],
    ['a\r\nb', ' spaced ', 'tab\tok', ','],
  ];
  assert.deepEqual(parseCSV(stringifyCSV(rows)), rows);
});

test('custom delimiter', () => {
  assert.deepEqual(parseCSV('a;"b;c";d', { delimiter: ';' }), [['a', 'b;c', 'd']]);
  assert.equal(stringifyCSV([['a', 'b;c']], { delimiter: ';' }), 'a;"b;c"');
});
