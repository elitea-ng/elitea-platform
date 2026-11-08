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

/** brand/elitea asset → the copy an app reads today. */
const COPIES = {
  'ios/icon-1024.png': 'apps/elitea-mobile/assets/images/icon.png',
  'ios/app.icon/icon.json': 'apps/elitea-mobile/assets/elitea.icon/icon.json',
  'ios/app.icon/Assets/elitea.png': 'apps/elitea-mobile/assets/elitea.icon/Assets/elitea.png',
  'android/icon-512.png': 'brand/assets/icon-pack/colored-dark/android/elitea-colored-dark-android-512.png',
  'android/adaptive-foreground.png': 'apps/elitea-mobile/assets/images/android-icon-foreground.png',
  'android/adaptive-background.png': 'apps/elitea-mobile/assets/images/android-icon-background.png',
  'android/adaptive-monochrome.png': 'apps/elitea-mobile/assets/images/android-icon-monochrome.png',
  'splash/splash-icon.png': 'apps/elitea-mobile/assets/images/splash-icon.png',
  'web/favicon-48.png': 'apps/elitea-mobile/assets/images/favicon.png',
  'macos/icon.icns': 'apps/elitea-desktop/src-tauri/icons/icon.icns',
  'windows/icon.ico': 'apps/elitea-desktop/src-tauri/icons/icon.ico',
  'desktop/32x32.png': 'apps/elitea-desktop/src-tauri/icons/32x32.png',
  'desktop/128x128.png': 'apps/elitea-desktop/src-tauri/icons/128x128.png',
  'desktop/128x128@2x.png': 'apps/elitea-desktop/src-tauri/icons/128x128@2x.png',
  'desktop/icon.png': 'apps/elitea-desktop/src-tauri/icons/icon.png',
  'desktop/tray-template.png': 'apps/elitea-desktop/src-tauri/icons/tray-template.png',
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
  assert.match(identity, new RegExp(`name: '${brand.manifest.productName}', shortName: '${brand.manifest.shortName}'`));
  assert.match(identity, new RegExp(`DEFAULT_BRAND_HUE = '${brand.manifest.brand.hue}'`, 'i'));
  const links = repo('apps/elitea-web/src/shared/brand/brandLinks.ts').toString();
  assert.match(links, new RegExp(`FALLBACK_SUPPORT_EMAIL = '${brand.manifest.supportEmail.replace('.', '\\.')}'`));
});

test('brand/elitea desktop identifier and product match tauri.conf.json', () => {
  const conf = JSON.parse(repo('apps/elitea-desktop/src-tauri/tauri.conf.json'));
  assert.equal(conf.identifier, brand.manifest.desktop.identifier);
});

test('brand/elitea mobile links and connect scheme match app.config.ts', () => {
  const config = repo('apps/elitea-mobile/app.config.ts').toString();
  for (const key of ['webUrl', 'privacyUrl', 'supportUrl']) assert.match(config, new RegExp(`${key}: '${brand.manifest[key]}'`));
  assert.match(config, new RegExp(`connectSchemes: \\['${brand.manifest.mobile.connectSchemes.join("', '")}'\\]`));
  assert.match(config, new RegExp(`displayName: '${brand.manifest.productName}'`));
  // The bundle id, redirect scheme, App Group and widget id move to brand/elitea in W1a (#1681).
});
