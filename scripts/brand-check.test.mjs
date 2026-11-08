// node --test scripts/brand-check.test.mjs
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { copyFileSync, cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, unlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';

import { checkAsset, checkBrand, crossBrandErrors, loadReference, run } from './brand-check.mjs';
import { icnsSizes, icoSizes, pngInfo } from './brand/images.mjs';
import { writePlaceholderAssets } from './brand/placeholder-assets.mjs';
import { encodeIcns, encodeIco, encodePng, placeholderPng, solidPng } from './brand/raster.mjs';
import { ASSETS } from './brand/schema.mjs';
import { validManifest } from './brand/fixtures.mjs';

const ROOT = mkdtempSync(join(tmpdir(), 'brand-check-'));
after(() => rmSync(ROOT, { recursive: true, force: true }));

let n = 0;
/** A fresh brand folder `<tmp>/<n>/<id>` with a manifest and placeholder assets. */
function makeBrand(manifest = validManifest(), colours = { fg: '#0E7490', bg: '#1F2933' }) {
  const dir = join(ROOT, String(n++), manifest.id);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'brand.json'), JSON.stringify(manifest, null, 2));
  writePlaceholderAssets(dir, colours);
  return dir;
}

/** A stand-in for brand/elitea, in its own colours. */
function eliteaLike() {
  return makeBrand(
    validManifest({
      id: 'elitea',
      productName: 'Elitea AI',
      shortName: 'Elitea',
      publisher: 'EPAM Systems',
      mobile: { bundleId: 'ai.elitea.app', connectSchemes: ['elitea'], oauthRedirectScheme: 'ai.elitea.app', appGroup: 'group.ai.elitea.app', widgetBundleId: 'ai.elitea.app.widgets' },
      desktop: { identifier: 'ai.elitea.desktop' },
      linkDomain: 'elitea.ai',
      webUrl: 'https://elitea.ai',
      privacyUrl: 'https://elitea.ai/privacy',
      supportUrl: 'https://elitea.ai/support',
      supportEmail: 'support@elitea.ai',
      brand: { hue: '#4a4de8' },
    }),
    { fg: '#4A4DE8', bg: '#0B0B18' },
  );
}

const errorsOf = (dir, opts) => checkBrand(dir, opts).errors;

test('image headers: PNG, ICO and ICNS', () => {
  const rgba = encodePng(3, 2, () => [1, 2, 3, 4]);
  assert.deepEqual(pngInfo(rgba), { width: 3, height: 2, alpha: true });
  assert.deepEqual(pngInfo(solidPng(5, '#000000')), { width: 5, height: 5, alpha: false });
  assert.equal(pngInfo(Buffer.from('not a png at all, really not one')), null);
  assert.deepEqual(icoSizes(encodeIco([16, 256].map((s) => ({ size: s, png: solidPng(s, '#000000') })))), [16, 256]);
  assert.equal(icoSizes(Buffer.from([1, 2, 3, 4, 5, 6])), null);
  assert.deepEqual(icnsSizes(encodeIcns([{ type: 'ic10', png: solidPng(4, '#000000') }, { type: 'icp4', png: solidPng(4, '#000000') }])), [16, 1024]);
  const broken = encodeIcns([{ type: 'ic10', png: solidPng(4, '#000000') }]);
  broken.writeUInt32BE(9999, 4);
  assert.equal(icnsSizes(broken), null);
});

test('the real Tauri icons parse as the asset table expects', () => {
  const icons = fileURLToPath(new URL('../apps/elitea-desktop/src-tauri/icons/', import.meta.url));
  const spec = (p) => ASSETS.find((a) => a.path === p);
  assert.deepEqual(checkAsset(spec('macos/icon.icns'), readFileSync(join(icons, 'icon.icns'))), []);
  assert.deepEqual(checkAsset(spec('windows/icon.ico'), readFileSync(join(icons, 'icon.ico'))), []);
});

test('a complete placeholder brand passes', () => {
  const ref = loadReference(eliteaLike());
  const r = checkBrand(makeBrand(), { reference: ref });
  assert.deepEqual(r.errors, []);
  assert.ok(r.ok);
  assert.deepEqual(
    r.permanent.map((p) => [p.field, p.value]),
    [
      ['mobile.bundleId', 'com.example.acme'],
      ['mobile.appGroup', 'group.com.example.acme'],
      ['desktop.identifier', 'com.example.acme.desktop'],
    ],
  );
});

test('an invalid manifest reports every problem and stops', () => {
  const dir = makeBrand();
  writeFileSync(join(dir, 'brand.json'), JSON.stringify(validManifest({ webUrl: 'http://acme.example.com', mobile: { ...validManifest().mobile, bundleId: 'Acme' } })));
  const errs = errorsOf(dir);
  assert.ok(errs.some((e) => e.startsWith('webUrl:')));
  assert.ok(errs.some((e) => e.startsWith('mobile.bundleId:')));
});

test('a missing icon size fails', () => {
  const dir = makeBrand();
  // .ico without 256, .icns without 1024, a missing PNG.
  writeFileSync(join(dir, 'assets/windows/icon.ico'), encodeIco([16, 24, 32, 48, 64, 128].map((s) => ({ size: s, png: solidPng(s, '#123456') }))));
  writeFileSync(join(dir, 'assets/macos/icon.icns'), encodeIcns([16, 32, 64, 128, 256, 512].map((s) => ({ type: { 16: 'icp4', 32: 'ic11', 64: 'ic12', 128: 'ic07', 256: 'ic08', 512: 'ic09' }[s], png: solidPng(s, '#123456') }))));
  unlinkSync(join(dir, 'assets/desktop/tray-template.png'));
  const errs = errorsOf(dir);
  assert.ok(errs.includes('assets/windows/icon.ico: missing size(s) 256 (has 16, 24, 32, 48, 64, 128)'), errs.join('\n'));
  assert.ok(errs.some((e) => e.startsWith('assets/macos/icon.icns: missing size(s) 1024')));
  assert.ok(errs.some((e) => e.startsWith('assets/desktop/tray-template.png: missing')));
});

test('a wrong pixel size, alpha where it is forbidden and none where it is needed', () => {
  const dir = makeBrand();
  writeFileSync(join(dir, 'assets/ios/icon-1024.png'), placeholderPng(1024, { fg: '#0E7490', bg: '#1F2933' }));
  writeFileSync(join(dir, 'assets/android/adaptive-foreground.png'), solidPng(1024, '#0E7490'));
  writeFileSync(join(dir, 'assets/android/icon-512.png'), solidPng(500, '#0E7490'));
  writeFileSync(join(dir, 'assets/web/favicon.svg'), '<svg><circle/></svg>');
  writeFileSync(join(dir, 'assets/desktop/icon.png'), 'GIF89a');
  const errs = errorsOf(dir);
  assert.ok(errs.some((e) => e.startsWith('assets/ios/icon-1024.png: must be opaque')));
  assert.ok(errs.some((e) => e.startsWith('assets/android/adaptive-foreground.png: needs an alpha channel')));
  assert.ok(errs.some((e) => e.startsWith('assets/android/icon-512.png: is 500x500, must be 512x512')));
  assert.ok(errs.some((e) => e.startsWith('assets/web/favicon.svg: not an SVG')));
  assert.ok(errs.includes('assets/desktop/icon.png: not a PNG'));
});

test('a hue with too little contrast fails', () => {
  const errs = errorsOf(makeBrand(validManifest({ brand: { hue: '#fff59d' } })));
  assert.ok(errs.some((e) => e.startsWith('brand.hue: contrast') && e.includes('light background')));
  assert.ok(errs.some((e) => e.startsWith('brand.hue: contrast') && e.includes('brand.onBrand')));
});

test('stray files and unknown firebase files fail', () => {
  const dir = makeBrand();
  writeFileSync(join(dir, 'notes.txt'), 'x');
  mkdirSync(join(dir, 'firebase'));
  writeFileSync(join(dir, 'firebase/service-account.json'), '{}');
  writeFileSync(join(dir, 'pack.json'), '[]');
  const errs = errorsOf(dir);
  assert.ok(errs.some((e) => e.startsWith('notes.txt: not part of the brand folder format')));
  assert.ok(errs.some((e) => e.startsWith('firebase/service-account.json:')));
  assert.ok(errs.some((e) => e.startsWith('pack.json: must be a JSON object')));
});

test('a brand that reuses ELITEA\'s icon fails', () => {
  const elitea = eliteaLike();
  const dir = makeBrand();
  copyFileSync(join(elitea, 'assets/ios/icon-1024.png'), join(dir, 'assets/ios/icon-1024.png'));
  const errs = errorsOf(dir, { reference: loadReference(elitea) });
  assert.deepEqual(errs, ['assets/ios/icon-1024.png: identical to brand/elitea/assets/ios/icon-1024.png; a brand uses its own artwork']);
});

test('a brand that reuses ELITEA\'s names, ids or links fails', () => {
  const ref = loadReference(eliteaLike());
  const m = validManifest({ productName: 'Elitea Helper', linkDomain: 'elitea.ai', supportEmail: 'support@elitea.ai' });
  m.mobile.connectSchemes = ['acme', 'elitea'];
  const errs = errorsOf(makeBrand(m), { reference: ref });
  assert.ok(errs.includes('productName: "Elitea Helper" contains brand/elitea\'s name "Elitea"'), errs.join('\n'));
  assert.ok(errs.includes('linkDomain: "elitea.ai" is brand/elitea\'s; a brand uses its own'));
  assert.ok(errs.includes('supportEmail: "support@elitea.ai" is brand/elitea\'s; a brand uses its own'));
  assert.ok(errs.includes('mobile.connectSchemes: "elitea" is brand/elitea\'s; a brand uses its own'));
  // ELITEA itself is never compared with itself.
  const elitea = eliteaLike();
  assert.deepEqual(errorsOf(elitea, { reference: loadReference(elitea) }), []);
});

test('brands checked together may not share an identifier', () => {
  const a = checkBrand(makeBrand());
  const b = checkBrand(makeBrand(validManifest({ id: 'other', desktop: { identifier: 'com.example.acme.desktop' } }), { fg: '#2F6F4E', bg: '#111827' }));
  const cross = crossBrandErrors([a, b]);
  // Bundle id, App Group, widget id and desktop identifier; the brand ids differ.
  assert.equal(cross.length, 4);
  assert.ok(cross.some((e) => e.startsWith('desktop.identifier: "com.example.acme.desktop" is used by both')));
});

test('run() and the CLI over the repository brands', () => {
  const result = run([eliteaLike(), makeBrand()], { referenceDir: eliteaLike() });
  assert.ok(result.ok, JSON.stringify(result.cross));
  const cli = fileURLToPath(new URL('./brand-check.mjs', import.meta.url));
  const bad = makeBrand(validManifest({ id: 'broken', supportUrl: 'ftp://x' }));
  let status = 0;
  try {
    execFileSync(process.execPath, [cli, bad], { stdio: 'pipe' });
  } catch (err) {
    status = err.status;
    assert.match(String(err.stdout), /FAIL .*broken/);
    assert.match(String(err.stdout), /supportUrl: must be an absolute https URL/);
  }
  assert.equal(status, 1);
  let usage = 0;
  try {
    execFileSync(process.execPath, [cli, '--nope'], { stdio: 'pipe' });
  } catch (err) {
    usage = err.status;
  }
  assert.equal(usage, 2);
});

test('the iOS Icon Composer document must name images that exist', () => {
  const dir = makeBrand();
  const doc = join(dir, 'assets/ios/app.icon');
  mkdirSync(join(doc, 'Assets'), { recursive: true });
  writeFileSync(join(doc, 'icon.json'), JSON.stringify({ groups: [{ layers: [{ 'image-name': 'mark.png' }] }] }));
  assert.ok(errorsOf(dir).includes('assets/ios/app.icon/Assets/mark.png: named by icon.json but missing'));
  cpSync(join(dir, 'assets/android/adaptive-foreground.png'), join(doc, 'Assets/mark.png'));
  assert.deepEqual(errorsOf(dir), []);
});

