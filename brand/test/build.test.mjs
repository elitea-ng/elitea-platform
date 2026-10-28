import assert from 'node:assert/strict';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { after, before, test } from 'node:test';

import { build, contrast, expand, renderMarkdown, tokensCss } from '../scripts/build.mjs';

const tokens = JSON.parse(readFileSync(new URL('../../libs/ts/client-core/src/brand/tokens.json', import.meta.url), 'utf8'));
let out;
let files;

before(() => {
  out = mkdtempSync(join(tmpdir(), 'elitea-brand-'));
  ({ files } = build(out));
});
after(() => rmSync(out, { recursive: true, force: true }));

const read = (path) => readFileSync(join(out, path), 'utf8');
const localLinks = (text, pattern) => [...text.matchAll(pattern)].map((m) => m[1]).filter((href) => !/^(https?:|mailto:|#)/.test(href));

test('the build writes the guide, the agent entry points and the tokens', () => {
  for (const path of ['index.html', 'BRAND.md', 'llms.txt', 'tokens.css', 'tokens.json', 'brand.json', 'assets/elitea-mark.svg', 'fonts/manrope.woff2']) {
    assert.ok(files.includes(path), path);
  }
});

test('every local link in llms.txt, BRAND.md and index.html resolves inside the build', () => {
  const links = [
    ...localLinks(read('llms.txt'), /\]\(([^)]+)\)/g),
    ...localLinks(read('BRAND.md'), /\]\(([^)]+)\)/g),
    ...localLinks(read('index.html'), /(?:href|src)="([^"]+)"/g),
  ];
  assert.ok(links.length > 20, `only ${links.length} links found`);
  for (const href of links) assert.ok(existsSync(join(out, href.split('#')[0])), `${href} does not resolve`);
});

test('every URL is relative, so the site works under any Pages path', () => {
  const html = read('index.html');
  assert.doesNotMatch(html, /(?:href|src)="\//);
  const external = [...html.matchAll(/(?:href|src)="(https?:[^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(external, ['https://github.com/elitea-ng/elitea-platform']);
  assert.doesNotMatch(read('styles.css'), /url\((?!"?fonts\/)/);
});

test('no placeholder survives into the outputs', () => {
  for (const path of ['index.html', 'BRAND.md']) assert.doesNotMatch(read(path), /\{\{[\w:.]+\}\}/, path);
});

test('tokens.css declares every scheme token for both themes', () => {
  const css = read('tokens.css');
  for (const name of Object.keys(tokens.schemes.dark)) {
    const prop = `--elitea-${name.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`)}:`;
    assert.equal(css.split(prop).length - 1, 3, `${prop} should appear in light, dark and the system-dark block`);
  }
  assert.equal(css, tokensCss(tokens));
});

test('tokens.json is W3C design-token shaped', () => {
  const dtcg = JSON.parse(read('tokens.json'));
  assert.equal(dtcg.palette.indigo.$type, 'color');
  assert.equal(dtcg.palette.indigo.$value, '#4A4DE8');
  assert.deepEqual(Object.keys(dtcg.light).sort(), Object.keys(tokens.schemes.light).sort());
});

test('the contrast table is computed, and a failing pair stops the build', () => {
  assert.match(read('BRAND.md'), /`onPrimary` #FFFFFF on `primary` #4A4DE8 \| 5\.97:1/);
  const broken = structuredClone(tokens);
  broken.schemes.light.textMuted = '#C0C4CC';
  assert.throws(() => expand('{{table:contrast}}', broken, ''), /contrast below minimum: .*light textMuted\/bg/);
  assert.ok(contrast('#FFFFFF', '#000000') > 20.9);
});

test('an unknown placeholder fails the build', () => {
  assert.throws(() => expand('{{nope}}', tokens, ''), /unknown placeholder \{\{nope\}\}/);
});

test('the markdown subset renders lists, tables, code and escapes HTML', () => {
  const html = renderMarkdown('- **a** `b`\n\n| x | y |\n| --- | --- |\n| 1 | <i> |\n\n```css\na{}\n```');
  assert.match(html, /<ul class="rules"><li><strong>a<\/strong> <code>b<\/code><\/li><\/ul>/);
  assert.match(html, /<td>&lt;i&gt;<\/td>/);
  assert.match(html, /<pre class="code"><code>a\{\}<\/code><\/pre>/);
});

test('the build reads the fonts it ships from the web app, not a copy', () => {
  assert.equal(read('fonts/sora.woff2').length > 0, true);
  assert.ok(existsSync(join(dirname(new URL(import.meta.url).pathname), '../../apps/elitea-web/public/brand/fonts/sora.woff2')));
});
