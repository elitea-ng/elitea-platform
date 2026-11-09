// node --test scripts/store-paperwork.test.mjs (npm run test:scripts)
//
// Store paperwork per brand (ADR-0052 decision 10, #1715). docs/ holds ELITEA's
// answers: every reverse-DNS identifier and every elitea.ai host it names is a
// value of brand/elitea/brand.json, and no partner or hosted-deployment name
// remains. docs/store/template/ is the set for any other brand: placeholders
// only, each `{field}` a real manifest field.
import assert from 'node:assert/strict';
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import { test } from 'node:test';

import { BRANDS_DIR, REPO_ROOT } from '../../../scripts/brand/load.mjs';

const DOCS = join(import.meta.dirname, '..', 'docs');
const TEMPLATE = join(DOCS, 'store', 'template');
const ELITEA = JSON.parse(readFileSync(join(BRANDS_DIR, 'elitea', 'brand.json'), 'utf8'));

function markdown(dir) {
  return readdirSync(dir, { withFileTypes: true, recursive: true })
    .filter((e) => e.isFile() && e.name.endsWith('.md'))
    .map((e) => join(e.parentPath, e.name));
}

/** Every string value in the manifest, flattened. */
function values(node, out = new Set()) {
  if (typeof node === 'string') out.add(node);
  else if (Array.isArray(node)) node.forEach((v) => values(v, out));
  else if (node && typeof node === 'object') Object.values(node).forEach((v) => values(v, out));
  return out;
}

/** The dotted field paths of the manifest (`mobile.bundleId`, `mobile.connectSchemes[0]`, ...). */
function fields(node, prefix = '', out = new Set()) {
  if (Array.isArray(node)) node.forEach((v, i) => fields(v, `${prefix}[${i}]`, out));
  else if (node && typeof node === 'object') for (const [k, v] of Object.entries(node)) fields(v, prefix ? `${prefix}.${k}` : k, out);
  else out.add(prefix);
  return out;
}

// Reverse-DNS app identifiers under the TLDs app ids use; Apple's own keys
// (com.apple.*) and Maven coordinates of third-party libraries are not app ids.
// Any reverse-DNS id: a 2-3 letter TLD label, then two or more labels.
const APP_ID = /\b(?:group\.)?(?:[a-z]{2,3})\.[a-z][a-z0-9-]*(?:\.[a-z][a-z0-9-]*)+\b/g;
const NOT_APP_IDS = [
  /^com\.apple\./,
  /^io\.github\.ronickg$/,
  // Files, symbols and keys the docs name, not app ids.
  /\.(?:ts|tsx|js|mjs|json|md|kt|swift|xml|png|pem|plist|yml)$/,
  /^(?:app|src|res|ios|net)\.[a-z]/,
  /^(?:text|arrow|xmark|models|az|www)\./,
];
const ELITEA_HOST = /\b(?:[a-z0-9-]+\.)*elitea\.ai\b/g;

test("ELITEA's paperwork names only brand/elitea's identifiers and hosts", () => {
  const allowed = values(ELITEA);
  const linkHost = ELITEA.linkDomain;
  const problems = [];
  for (const file of markdown(DOCS).filter((f) => !f.startsWith(TEMPLATE))) {
    const text = readFileSync(file, 'utf8');
    for (const [id] of text.matchAll(APP_ID)) {
      if (NOT_APP_IDS.some((re) => re.test(id))) continue;
      if (!allowed.has(id) && !id.endsWith('.example.com') && !/\.example$/.test(id)) problems.push(`${relative(REPO_ROOT, file)}: ${id}`);
    }
    for (const [host] of text.matchAll(ELITEA_HOST)) if (host !== linkHost) problems.push(`${relative(REPO_ROOT, file)}: ${host}`);
    if (/solutions ?hub/i.test(text)) problems.push(`${relative(REPO_ROOT, file)}: SolutionsHub`);
  }
  assert.deepEqual(problems, []);
});

test('the template names no ELITEA value, and each {field} is a manifest field', () => {
  const known = fields(ELITEA);
  const eliteaIds = [ELITEA.mobile.bundleId, ELITEA.mobile.appGroup, ELITEA.linkDomain, ELITEA.supportEmail, ...ELITEA.mobile.connectSchemes.map((s) => `${s}://`)];
  const files = markdown(TEMPLATE);
  assert.ok(files.length >= 4, 'README, listing, privacy policy, support');
  for (const file of files) {
    const text = readFileSync(file, 'utf8');
    for (const id of eliteaIds) assert.ok(!text.includes(id), `${relative(REPO_ROOT, file)} names ${id}`);
    assert.doesNotMatch(text, /Elitea AI|EPAM|solutions ?hub/i, relative(REPO_ROOT, file));
    for (const [, field] of text.matchAll(/\{([A-Za-z.[\]0-9]+)\}/g)) assert.ok(known.has(field), `${relative(REPO_ROOT, file)}: {${field}} is not a brand.json field`);
  }
});

test('the review notes say push and Local Push are absent, and the app agrees', () => {
  for (const file of [join(DOCS, 'store', 'listing.md'), join(TEMPLATE, 'listing.md')]) {
    const text = readFileSync(file, 'utf8');
    assert.match(text, /only on a device\s+> enrolled in an organization's MDM/, relative(REPO_ROOT, file));
    assert.match(text, /never asks for a push token, and has no Local Push/, relative(REPO_ROOT, file));
  }
  // The claim holds in the code: no push token call, no notifications module, no
  // Network Extension (Local Push) entitlement. When one lands, change the notes.
  const APP = join(import.meta.dirname, '..');
  const pkg = JSON.parse(readFileSync(join(APP, 'package.json'), 'utf8'));
  assert.ok(!('expo-notifications' in { ...pkg.dependencies, ...pkg.devDependencies }), 'expo-notifications is a dependency: update the review notes');
  const sources = [join(APP, 'app.config.ts'), ...['src', 'plugins', 'app-intents', 'native-modules'].filter((d) => existsSync(join(APP, d))).flatMap((d) =>
    readdirSync(join(APP, d), { withFileTypes: true, recursive: true })
      .filter((e) => e.isFile() && /\.(?:ts|tsx|js|swift|kt)$/.test(e.name) && !/\.test\./.test(e.name))
      .map((e) => join(e.parentPath, e.name)),
  )];
  const PUSH = /getDevicePushTokenAsync|getExpoPushTokenAsync|registerForRemoteNotifications|NEAppPushProvider|com\.apple\.developer\.networking\.networkextension|aps-environment/;
  const hits = sources.filter((f) => PUSH.test(readFileSync(f, 'utf8'))).map((f) => relative(REPO_ROOT, f));
  assert.deepEqual(hits, [], 'push or Local Push is in the app: update the review notes');
});
