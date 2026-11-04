import test from 'node:test';
import assert from 'node:assert/strict';
import { render } from '../src/template.mjs';

test('hidden: dotted name does not fall through once the head is found', () => {
  const data = { a: { b: 'outer' }, inner: { a: {} } };
  assert.equal(render('{{#inner}}[{{a.b}}]{{/inner}}', data), '[]');
});

test('hidden: dotted name head resolved from outer context', () => {
  const data = { cfg: { sep: '/' }, rows: [{ v: 1 }, { v: 2 }] };
  assert.equal(render('{{#rows}}{{v}}{{cfg.sep}}{{/rows}}', data), '1/2/');
});

test('hidden: nested sections with the same name', () => {
  const data = { a: { a: { x: 'deep' }, x: 'mid' } };
  assert.equal(render('{{#a}}{{x}}{{#a}}-{{x}}{{/a}}{{/a}}', data), 'mid-deep');
});

test('hidden: nested arrays use the innermost {{.}}', () => {
  assert.equal(render('{{#m}}[{{#.}}{{.}}{{/.}}]{{/m}}', { m: [[1, 2], [3]] }), '[12][3]');
});

test('hidden: raw tags respect whitespace and dotted names', () => {
  assert.equal(render('{{{ a.b }}}{{&  a.b }}', { a: { b: '<i>' } }), '<i><i>');
});

test('hidden: escaping in sections and of dotted values', () => {
  assert.equal(render('{{#xs}}{{.}}{{/xs}}', { xs: ['<', '&', '"', "'", '>'] }), '&lt;&amp;&quot;&#39;&gt;');
});

test('hidden: truthy scalar section and zero is falsy', () => {
  assert.equal(render('{{#n}}n={{n}}{{/n}}{{^z}}zero{{/z}}', { n: 5, z: 0 }), 'n=5zero');
});

test('hidden: inverted section does not push a context', () => {
  assert.equal(render('{{^missing}}{{name}}{{/missing}}', { name: 'kept' }), 'kept');
});

test('hidden: newlines and whitespace around sections are kept verbatim', () => {
  const t = 'List:\n{{#xs}}\n- {{.}}\n{{/xs}}\nEnd';
  assert.equal(render(t, { xs: ['a', 'b'] }), 'List:\n\n- a\n\n- b\n\nEnd');
});

test('hidden: closing tag mismatch deep inside throws', () => {
  assert.throws(() => render('{{#a}}{{#b}}{{/a}}{{/b}}', { a: 1, b: 1 }), Error);
  assert.throws(() => render('{{^a}}{{#b}}x{{/b}}', {}), Error);
});
