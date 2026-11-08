// node --test scripts/brand/schema.test.mjs
import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

import { BRANDS_DIR, BrandError, loadBrand, loadBrandFrom, resolveBrandDir } from './load.mjs';
import { validManifest } from './fixtures.mjs';
import { contrastFindings, contrastRatio, isAppId, isAppScheme, isHost, isHttpsUrl, isReverseDns, PERMANENT_FIELDS, RESERVED_IDS, validateManifest } from './schema.mjs';

const errorsFor = (m) => validateManifest(m);
const hasError = (m, field) => errorsFor(m).some((e) => e.startsWith(`${field}:`) || e.startsWith(`${field}[`));

test('a valid manifest has no problems', () => {
  assert.deepEqual(errorsFor(validManifest()), []);
});

test('formatVersion, unknown fields and non-objects', () => {
  assert.ok(hasError(validManifest({ formatVersion: 2 }), 'formatVersion'));
  assert.ok(hasError(validManifest({ colour: 'red' }), 'colour'));
  assert.ok(hasError({ ...validManifest(), mobile: { ...validManifest().mobile, extra: 1 } }, 'mobile.extra'));
  assert.deepEqual(validateManifest([]), ['brand.json: must be a JSON object']);
  assert.deepEqual(validateManifest(null), ['brand.json: must be a JSON object']);
});

test('ids: shape and the brand guide folder names', () => {
  for (const bad of ['A', 'a', '1abc', 'acme_corp', 'Acme', 'x'.repeat(33)]) assert.ok(hasError(validManifest({ id: bad }), 'id'), bad);
  for (const reserved of RESERVED_IDS) assert.ok(hasError(validManifest({ id: reserved }), 'id'), reserved);
  assert.deepEqual(RESERVED_IDS, ['assets', 'scripts', 'test', 'dist']);
});

test('reverse-DNS ids and app ids', () => {
  assert.ok(isReverseDns('ai.elitea.app'));
  assert.ok(isReverseDns('com.example.my-app'));
  for (const bad of ['elitea.app', 'Ai.Elitea.App', 'ai..app', 'ai.elitea.app.', '.ai.elitea', 'ai.elitea.app-', 'ai.1elitea.app', 'ai elitea app']) assert.ok(!isReverseDns(bad), bad);
  assert.ok(isAppId('ai.elitea.app'));
  // A hyphen is fine on iOS but not in an Android package; a Java keyword is not a package segment.
  assert.ok(!isAppId('com.example.my-app'));
  assert.ok(!isAppId('com.example.new'));
  const m = validManifest();
  m.mobile.bundleId = 'com.example.my-app';
  assert.ok(hasError(m, 'mobile.bundleId'));
});

test('mobile: schemes, redirect scheme, App Group and widget id', () => {
  const mk = (mobile) => validManifest({ mobile: { ...validManifest().mobile, ...mobile } });
  assert.ok(hasError(mk({ connectSchemes: [] }), 'mobile.connectSchemes'));
  assert.ok(hasError(mk({ connectSchemes: ['https'] }), 'mobile.connectSchemes'));
  assert.ok(hasError(mk({ connectSchemes: ['Acme'] }), 'mobile.connectSchemes'));
  assert.ok(hasError(mk({ connectSchemes: ['acme', 'acme'] }), 'mobile.connectSchemes'));
  assert.ok(hasError(mk({ oauthRedirectScheme: 'acme' }), 'mobile.oauthRedirectScheme'));
  assert.ok(hasError(mk({ oauthRedirectScheme: 'com.other.acme' }), 'mobile.oauthRedirectScheme'));
  assert.ok(!hasError(mk({ oauthRedirectScheme: 'com.example.acme.auth' }), 'mobile.oauthRedirectScheme'));
  assert.ok(hasError(mk({ connectSchemes: ['com.example.acme'], oauthRedirectScheme: 'com.example.acme' }), 'mobile.oauthRedirectScheme'));
  assert.ok(hasError(mk({ appGroup: 'com.example.acme' }), 'mobile.appGroup'));
  assert.ok(hasError(mk({ appGroup: 'group.acme' }), 'mobile.appGroup'));
  assert.ok(hasError(mk({ widgetBundleId: 'com.example.widgets' }), 'mobile.widgetBundleId'));
  assert.ok(isAppScheme('elitea'));
  assert.ok(!isAppScheme('mailto'));
});

test('identifiers must be unique', () => {
  const m = validManifest({ desktop: { identifier: 'com.example.acme' } });
  assert.ok(errorsFor(m).some((e) => e.startsWith('desktop.identifier:') && e.includes('unique')));
  assert.ok(hasError(validManifest({ desktop: { identifier: 'com.example.acme.app' } }), 'desktop.identifier'));
});

test('URLs, host names and the support address', () => {
  assert.ok(isHttpsUrl('https://elitea.ai'));
  assert.ok(isHttpsUrl('https://elitea.ai/'));
  assert.ok(isHttpsUrl('https://elitea.ai/privacy'));
  for (const bad of ['http://elitea.ai', 'https://elitea.ai:8443', 'https://user@elitea.ai', 'https://elitea.ai/#x', 'https://localhost', 'https://10.0.0.1', 'https://Elitea.AI', 'elitea.ai', 'https://elitea.ai/a b', '']) {
    assert.ok(!isHttpsUrl(bad), bad);
  }
  assert.ok(isHost('elitea.ai'));
  for (const bad of ['https://elitea.ai', 'elitea.ai/x', 'elitea', 'Elitea.ai', 'elitea.ai:443']) assert.ok(!isHost(bad), bad);
  for (const field of ['webUrl', 'privacyUrl', 'supportUrl']) assert.ok(hasError(validManifest({ [field]: 'http://acme.example.com' }), field));
  assert.ok(hasError(validManifest({ linkDomain: 'https://acme.example.com' }), 'linkDomain'));
  assert.ok(!hasError(validManifest({ linkDomain: null }), 'linkDomain'));
  assert.ok(hasError(validManifest({ supportEmail: 'nobody' }), 'supportEmail'));
  assert.ok(!hasError(validManifest({ supportEmail: null }), 'supportEmail'));
  assert.ok(!hasError(validManifest({ docsUrl: 'https://docs.example.com/' }), 'docsUrl'));
  assert.ok(hasError(validManifest({ docsUrl: '//evil.example.com/' }), 'docsUrl'));
  assert.ok(hasError(validManifest({ docsUrl: 'docs' }), 'docsUrl'));
});

test('names and publisher', () => {
  assert.ok(hasError(validManifest({ productName: '' }), 'productName'));
  assert.ok(hasError(validManifest({ productName: ' Acme' }), 'productName'));
  assert.ok(hasError(validManifest({ shortName: 'x'.repeat(21) }), 'shortName'));
  assert.ok(hasError(validManifest({ publisher: undefined }), 'publisher'));
});

test('colours: hue shape and scheme backgrounds', () => {
  assert.ok(hasError(validManifest({ brand: { hue: 'indigo' } }), 'brand.hue'));
  assert.ok(hasError(validManifest({ brand: { hue: '#0e7490', onBrand: 'white' } }), 'brand.onBrand'));
  assert.ok(hasError(validManifest({ brand: { hue: '#0e7490', schemes: { hc: {} } } }), 'brand.schemes.hc'));
  assert.ok(hasError(validManifest({ brand: { hue: '#0e7490', schemes: { light: { 'background.default': 'white' } } } }), 'brand.schemes.light.background.default'));
});

test('contrast of the hue on both schemes and of its label', () => {
  assert.equal(Math.round(contrastRatio('#000000', '#ffffff')), 21);
  assert.equal(contrastRatio('#4a4de8', '#4a4de8'), 1);
  assert.ok(contrastFindings(validManifest()).every((f) => f.ok));
  // Pale yellow: invisible on white, and white text on it is unreadable.
  const pale = contrastFindings(validManifest({ brand: { hue: '#fff59d' } }));
  assert.equal(pale.filter((f) => !f.ok).length, 2);
  // Near-black: invisible on the dark scheme.
  const dark = contrastFindings(validManifest({ brand: { hue: '#111122' } }));
  assert.ok(dark.some((f) => !f.ok && f.pair.includes('dark background')));
  // A scheme override moves the background the hue is checked against.
  const custom = contrastFindings(validManifest({ brand: { hue: '#0e7490', schemes: { dark: { 'background.default': '#0e7490' } } } }));
  assert.ok(custom.some((f) => !f.ok && f.pair.includes('#0e7490 on the dark background #0e7490')));
});

test('eas, updates and push', () => {
  assert.ok(hasError(validManifest({ eas: { projectId: 'nope', owner: 'acme' } }), 'eas.projectId'));
  assert.ok(hasError(validManifest({ eas: { projectId: null, owner: 'acme' } }), 'eas'));
  assert.ok(!hasError(validManifest({ eas: { projectId: '2d2b8e3c-1a7c-4c4e-9f0a-1b2c3d4e5f60', owner: 'acme' } }), 'eas'));
  assert.ok(hasError(validManifest({ updates: { mode: 'yes' } }), 'updates.mode'));
  // Updates on: the brand's own EAS project and a code signing certificate.
  assert.ok(hasError(validManifest({ updates: { mode: 'on', codeSigningCertificate: 'certs/update.pem' } }), 'updates.mode'));
  const linked = { eas: { projectId: '2d2b8e3c-1a7c-4c4e-9f0a-1b2c3d4e5f60', owner: 'acme' } };
  assert.ok(hasError(validManifest({ ...linked, updates: { mode: 'on' } }), 'updates.codeSigningCertificate'));
  assert.ok(hasError(validManifest({ ...linked, updates: { mode: 'on', codeSigningCertificate: '../x.pem' } }), 'updates.codeSigningCertificate'));
  assert.deepEqual(errorsFor(validManifest({ ...linked, updates: { mode: 'on', codeSigningCertificate: 'certs/update.pem' } })), []);
  assert.ok(hasError(validManifest({ push: { relay: 'EPAM relay' } }), 'push.relay'));
  assert.ok(!hasError(validManifest({ push: { relay: 'epam' } }), 'push.relay'));
});

test('the permanent fields are the store and data-folder ids', () => {
  assert.deepEqual(Object.keys(PERMANENT_FIELDS), ['mobile.bundleId', 'mobile.appGroup', 'desktop.identifier']);
});

test('ELITEA_BRAND resolution', () => {
  assert.equal(resolveBrandDir(undefined), join(BRANDS_DIR, 'elitea'));
  assert.equal(resolveBrandDir('  '), join(BRANDS_DIR, 'elitea'));
  assert.equal(resolveBrandDir('sample'), join(BRANDS_DIR, 'sample'));
  assert.equal(resolveBrandDir('./x/acme', { cwd: '/tmp/r' }), '/tmp/r/x/acme');
  assert.equal(resolveBrandDir('../acme', { cwd: '/tmp/r' }), '/tmp/acme');
  assert.equal(resolveBrandDir('/abs/acme'), '/abs/acme');
  for (const reserved of RESERVED_IDS) assert.throws(() => resolveBrandDir(reserved), BrandError);
  assert.throws(() => resolveBrandDir('Not_An_Id'), BrandError);
});

test('loadBrandFrom reads, validates and freezes', () => {
  const root = mkdtempSync(join(tmpdir(), 'brand-load-'));
  try {
    const dir = join(root, 'acme');
    mkdirSync(dir);
    assert.throws(() => loadBrandFrom(dir), /no brand.json/);
    writeFileSync(join(dir, 'brand.json'), '{ not json');
    assert.throws(() => loadBrandFrom(dir), /not valid JSON/);
    writeFileSync(join(dir, 'brand.json'), JSON.stringify(validManifest({ webUrl: 'http://x.example.com' })));
    assert.throws(() => loadBrandFrom(dir), (err) => err instanceof BrandError && err.problems.some((p) => p.startsWith('webUrl:')));
    writeFileSync(join(dir, 'brand.json'), JSON.stringify(validManifest()));
    const brand = loadBrandFrom(dir);
    assert.equal(brand.id, 'acme');
    assert.equal(brand.manifest.mobile.bundleId, 'com.example.acme');
    assert.equal(brand.assetsDir, join(dir, 'assets'));
    assert.ok(Object.isFrozen(brand.manifest.mobile));
    // The folder name is the id.
    const other = join(root, 'other');
    mkdirSync(other);
    writeFileSync(join(other, 'brand.json'), JSON.stringify(validManifest()));
    assert.throws(() => loadBrandFrom(other), /must match the folder name "other"/);
    // ELITEA_BRAND as a path, relative to cwd.
    assert.equal(loadBrand({ env: { ELITEA_BRAND: './acme' }, cwd: root }).dir, dir);
    assert.throws(() => loadBrandFrom(join(root, 'missing')), /not found/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
