import test from 'node:test';
import assert from 'node:assert/strict';
import { parseCSV, stringifyCSV } from '../src/csv.mjs';

test('hidden: CRLF inside a quoted field is preserved verbatim', () => {
  assert.deepEqual(parseCSV('"a\r\nb",c\r\n'), [['a\r\nb', 'c']]);
});

test('hidden: no trailing newline, last record intact', () => {
  assert.deepEqual(parseCSV('h1,h2\r\nv1,v2'), [
    ['h1', 'h2'],
    ['v1', 'v2'],
  ]);
});

test('hidden: field that is only an escaped quote', () => {
  assert.deepEqual(parseCSV('"""",""""""'), [['"', '""']]);
});

test('hidden: last record ends with an empty quoted field and CRLF', () => {
  assert.deepEqual(parseCSV('a,""\r\n'), [['a', '']]);
});

test('hidden: unterminated quote at very end throws', () => {
  assert.throws(() => parseCSV('"abc'), /unterminated/i);
  assert.throws(() => parseCSV('x,"a""'), /unterminated/i);
});

test('hidden: stringify quotes a bare CR and undefined becomes empty', () => {
  assert.equal(stringifyCSV([['a\rb', undefined, 0]]), '"a\rb",,0');
});

test('hidden: stringify with ; delimiter does not quote commas', () => {
  assert.equal(stringifyCSV([['a,b', 'c;d', 'e"f']], { delimiter: ';' }), 'a,b;"c;d";"e""f"');
});

test('hidden: round trip across many shapes and delimiters', () => {
  const fixtures = [
    [['only']],
    [['', ''], ['', '']],
    [['"start', 'end"', 'mid"dle']],
    [['\n'], ['\r\n'], ['x\n\ny']],
    [['1', '2', '3'], ['a,b', 'c;d', 'e|f']],
  ];
  for (const delimiter of [',', ';', '|', '\t']) {
    for (const rows of fixtures) {
      assert.deepEqual(parseCSV(stringifyCSV(rows, { delimiter }), { delimiter }), rows, `delimiter ${JSON.stringify(delimiter)}`);
    }
  }
});
