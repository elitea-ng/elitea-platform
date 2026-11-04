import test from 'node:test';
import assert from 'node:assert/strict';
import { render } from '../src/template.mjs';

test('interpolates with optional whitespace in tags', () => {
  assert.equal(render('Hi {{name}} / {{ name }}!', { name: 'Ada' }), 'Hi Ada / Ada!');
});

test('escapes HTML by default', () => {
  assert.equal(render('{{v}}', { v: `<a href="x">Tom & 'Jerry'</a>` }), '&lt;a href=&quot;x&quot;&gt;Tom &amp; &#39;Jerry&#39;&lt;/a&gt;');
});

test('triple mustache and & are raw', () => {
  assert.equal(render('{{{v}}}|{{& v}}', { v: '<b>&</b>' }), '<b>&</b>|<b>&</b>');
});

test('missing and null values render empty, numbers render', () => {
  assert.equal(render('[{{a}}][{{b}}][{{c}}][{{d}}]', { b: null, c: 0, d: 2.5 }), '[][][0][2.5]');
});

test('dotted names', () => {
  assert.equal(render('{{user.address.city}}', { user: { address: { city: 'Oslo' } } }), 'Oslo');
  assert.equal(render('[{{user.missing.city}}]', { user: {} }), '[]');
});

test('section iterates arrays with {{.}} and fields', () => {
  assert.equal(render('{{#xs}}<{{.}}>{{/xs}}', { xs: [1, 2, 3] }), '<1><2><3>');
  assert.equal(render('{{#people}}{{name}};{{/people}}', { people: [{ name: 'a' }, { name: 'b' }] }), 'a;b;');
});

test('section on truthy object pushes context; falsy and empty render nothing', () => {
  assert.equal(render('{{#user}}{{name}}{{/user}}', { user: { name: 'Bo' } }), 'Bo');
  assert.equal(render('[{{#f}}x{{/f}}][{{#e}}x{{/e}}][{{#m}}x{{/m}}]', { f: false, e: [] }), '[][][]');
});

test('inverted sections', () => {
  const t = '{{#items}}{{.}},{{/items}}{{^items}}none{{/items}}';
  assert.equal(render(t, { items: [] }), 'none');
  assert.equal(render(t, { items: ['a'] }), 'a,');
  assert.equal(render(t, {}), 'none');
});

test('inner sections can see outer names', () => {
  const t = '{{#items}}{{prefix}}-{{.}} {{/items}}';
  assert.equal(render(t, { prefix: '#', items: ['a', 'b'] }), '#-a #-b ');
});

test('comments are dropped and surrounding whitespace kept', () => {
  assert.equal(render('a {{! ignore me }} b\n', {}), 'a  b\n');
});

test('unclosed or mismatched sections throw', () => {
  assert.throws(() => render('{{#a}}x', { a: true }), Error);
  assert.throws(() => render('{{#a}}x{{/b}}', { a: true }), Error);
  assert.throws(() => render('x{{/a}}', {}), Error);
});
