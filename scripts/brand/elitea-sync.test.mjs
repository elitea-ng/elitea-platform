// node --test scripts/brand/elitea-sync.test.mjs
//
// brand/elitea is the source the generators of W1 to W3 will read. Until
// they do, the apps still hold their own copies of the same values and
// files; this test holds the two equal, so the brand folder cannot drift
// from what ships. Each row goes once its app reads the brand folder.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';

import { BRANDS_DIR, loadBrandFrom, REPO_ROOT } from './load.mjs';

const brand = loadBrandFrom(join(BRANDS_DIR, 'elitea'));
const repo = (p) => readFileSync(join(REPO_ROOT, p));
const asset = (p) => readFileSync(join(brand.assetsDir, p));
/** A literal string as a RegExp source. */
const lit = (v) => String(v).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

/** brand/elitea asset → the copy an app reads today. */
const COPIES = {
  'android/icon-512.png': 'brand/assets/icon-pack/colored-dark/android/elitea-colored-dark-android-512.png',
  // The mobile app reads its icons from brand/elitea itself (ADR-0052 W1a); the copies are gone.
  // The desktop app reads its icons from brand/elitea too (ADR-0052 W2a); its copies are gone.
  'web/favicon.svg': 'apps/elitea-web/public/brand/favicon.svg',
  'logo/logo-full.svg': 'libs/ts/client-core/src/brand/logo-full.svg',
  'logo/logo-mark.svg': 'libs/ts/client-core/src/brand/logo-mark.svg',
};

for (const [mine, theirs] of Object.entries(COPIES)) {
  test(`brand/elitea/assets/${mine} equals ${theirs}`, () => {
    assert.ok(asset(mine).equals(repo(theirs)), `copy ${theirs} to brand/elitea/assets/${mine} (or the reverse), whichever changed on purpose`);
  });
}

test('brand/elitea names, hue and support address match the web default pack and client-core', () => {
  const pack = JSON.parse(repo('apps/elitea-web/src/shared/brand/tokens/default.pack.json'));
  assert.equal(brand.manifest.productName, pack.product.name);
  assert.equal(brand.manifest.shortName, pack.product.shortName);
  assert.equal(brand.manifest.docsUrl, pack.product.docsUrl);
  assert.equal(brand.manifest.brand.hue.toLowerCase(), pack.brand.hue.toLowerCase());
  const identity = repo('libs/ts/client-core/src/brand/identity.ts').toString();
  assert.match(identity, new RegExp(`name: '${lit(brand.manifest.productName)}', shortName: '${lit(brand.manifest.shortName)}'`));
  assert.match(identity, new RegExp(`DEFAULT_BRAND_HUE = '${lit(brand.manifest.brand.hue)}'`, 'i'));
  const links = repo('apps/elitea-web/src/shared/brand/brandLinks.ts').toString();
  assert.match(links, new RegExp(`FALLBACK_SUPPORT_EMAIL = '${lit(brand.manifest.supportEmail)}'`));
});
