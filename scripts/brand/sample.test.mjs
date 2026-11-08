// node --test scripts/brand/sample.test.mjs
//
// brand/sample is neutral and regenerable: its assets are exactly what
// placeholder-assets.mjs writes, and nothing in the folder names ELITEA or EPAM.
import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import { test } from 'node:test';

import { BRANDS_DIR } from './load.mjs';
import { PLACEHOLDER_COLOURS, placeholderAssets } from './placeholder-assets.mjs';

const SAMPLE = join(BRANDS_DIR, 'sample');
const files = (dir) => readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? files(join(dir, e.name)) : [join(dir, e.name)]));

test('brand/sample assets are exactly the generated placeholders', () => {
  const expected = placeholderAssets(PLACEHOLDER_COLOURS);
  const actual = files(join(SAMPLE, 'assets')).map((f) => relative(join(SAMPLE, 'assets'), f).split('\\').join('/'));
  assert.deepEqual(actual.sort(), [...expected.keys()].sort());
  for (const [path, bytes] of expected) {
    assert.ok(readFileSync(join(SAMPLE, 'assets', path)).equals(bytes), `assets/${path} differs: run node scripts/brand/placeholder-assets.mjs brand/sample`);
  }
});

test('brand/sample names no ELITEA or EPAM, outside its README', () => {
  for (const f of files(SAMPLE)) {
    if (f.endsWith('README.md')) continue;
    const text = readFileSync(f).toString('latin1');
    assert.doesNotMatch(text, /elitea|epam/i, relative(SAMPLE, f));
  }
});
