// node --test scripts/tauri-brand.test.mjs (npm run test:scripts)
//
// The Tauri overlay per brand (ADR-0052 decision 4): every brand value the
// desktop build takes, from the manifest only, by paths relative to src-tauri.
import assert from 'node:assert/strict';
import { readFileSync, statSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { test } from 'node:test';

import { BRANDS_DIR, loadBrandFrom, REPO_ROOT } from '../../../scripts/brand/load.mjs';
import { TAURI_DIR, tauriOverlay } from './tauri-brand.mjs';

const brand = (id) => loadBrandFrom(join(BRANDS_DIR, id));
/** Every file path the overlay names. */
const paths = (o) => [o.app.trayIcon.iconPath, ...o.bundle.icon];

test('brand/elitea builds ai.elitea.desktop named Elitea (the data folders stay where they are)', () => {
  const o = tauriOverlay(brand('elitea'));
  assert.equal(o.identifier, 'ai.elitea.desktop');
  assert.equal(o.productName, 'Elitea');
  assert.equal(o.bundle.publisher, brand('elitea').manifest.publisher);
});

for (const id of ['elitea', 'sample']) {
  test(`brand/${id}: every image is the brand's own, relative to src-tauri, and exists`, () => {
    const o = tauriOverlay(brand(id));
    for (const p of paths(o)) {
      assert.ok(!p.startsWith('/') && !p.includes(REPO_ROOT), `${p} is a machine path`);
      assert.ok(p.startsWith(`../../../brand/${id}/assets/`), `${p} is not in brand/${id}`);
      statSync(resolve(TAURI_DIR, p));
    }
    assert.equal(o.app.trayIcon.iconAsTemplate, true);
  });
}

test('the committed tauri.conf.json holds exactly brand/sample\'s values (a build without its overlay is the sample)', () => {
  const conf = JSON.parse(readFileSync(join(TAURI_DIR, 'tauri.conf.json'), 'utf8'));
  const o = tauriOverlay(brand('sample'));
  assert.equal(conf.productName, o.productName);
  assert.equal(conf.identifier, o.identifier);
  assert.deepEqual(conf.app.trayIcon, o.app.trayIcon);
  assert.deepEqual(conf.bundle.icon, o.bundle.icon);
  assert.equal(conf.bundle.publisher, o.bundle.publisher);
  assert.equal(conf.bundle.copyright, o.bundle.copyright);
});

test('the sample overlay names no ELITEA product, id or publisher', () => {
  assert.doesNotMatch(JSON.stringify(tauriOverlay(brand('sample'))), /elitea|epam/i);
});
