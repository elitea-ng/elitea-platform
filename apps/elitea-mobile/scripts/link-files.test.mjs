import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';

import { aasaProblems, assetlinksProblems, LINK_PATH, normalizeFingerprint } from './link-files.mjs';

const FP = Array.from({ length: 32 }, (_, i) => i.toString(16).padStart(2, '0').toUpperCase()).join(':');
const APP = 'ABCDE12345.com.example.sample';

describe('link files', () => {
  it('checks the same path the app config declares', () => {
    const config = readFileSync(new URL('../app.config.ts', import.meta.url), 'utf8');
    assert.match(config, new RegExp(`LINK_PATH_PREFIX = '${LINK_PATH}'`));
  });

  it('normalizes SHA-256 fingerprints and refuses anything else', () => {
    assert.equal(normalizeFingerprint(FP.toLowerCase().replace(/:/g, '')), FP);
    assert.equal(normalizeFingerprint('AB:CD'), null);
    assert.equal(normalizeFingerprint('zz'.repeat(32)), null);
  });

  it('apple-app-site-association: passes for the app on /connect, in either format', () => {
    assert.deepEqual(aasaProblems({ applinks: { details: [{ appIDs: [APP], components: [{ '/': '/connect*' }] }] } }, APP), []);
    assert.deepEqual(aasaProblems({ applinks: { apps: [], details: [{ appID: APP, paths: ['/connect'] }] } }, APP), []);
    assert.deepEqual(aasaProblems({ applinks: { details: [{ appIDs: [APP], components: [{ '/': '*' }] }] } }, APP), []);
  });

  it('apple-app-site-association: fails for another app, an excluded or missing path, or no details', () => {
    assert.equal(aasaProblems({ applinks: { details: [{ appIDs: ['ABCDE12345.ai.elitea.app'], paths: ['*'] }] } }, APP).length, 1);
    assert.equal(aasaProblems({ applinks: { details: [{ appIDs: [APP], components: [{ '/': '/connect*', exclude: true }] }] } }, APP).length, 1);
    assert.equal(aasaProblems({ applinks: { details: [{ appIDs: [APP], paths: ['NOT /connect', '/chats/*'] }] } }, APP).length, 1);
    assert.equal(aasaProblems({}, APP).length, 1);
  });

  const statement = (pkg, fps) => ({
    relation: ['delegate_permission/common.handle_all_urls'],
    target: { namespace: 'android_app', package_name: pkg, sha256_cert_fingerprints: fps },
  });

  it('assetlinks.json: passes when the package is linked to every given certificate', () => {
    assert.deepEqual(assetlinksProblems([statement('com.example.sample', [FP.toLowerCase()])], 'com.example.sample', [FP]), []);
  });

  it('assetlinks.json: fails for another package, another certificate, or not an array', () => {
    assert.equal(assetlinksProblems([statement('ai.elitea.app', [FP])], 'com.example.sample', [FP]).length, 1);
    const other = FP.replace(/^00/, 'FF');
    assert.equal(assetlinksProblems([statement('com.example.sample', [other])], 'com.example.sample', [FP]).length, 1);
    assert.equal(assetlinksProblems({}, 'com.example.sample', [FP]).length, 1);
  });
});
