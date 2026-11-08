import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { aasaProblems, assetlinksProblems, normalizeFingerprint } from './link-files.mjs';

const PATH = '/connect';

const FP = Array.from({ length: 32 }, (_, i) => i.toString(16).padStart(2, '0').toUpperCase()).join(':');
const APP = 'ABCDE12345.com.example.sample';

describe('link files', () => {
  it('normalizes SHA-256 fingerprints and refuses anything else', () => {
    assert.equal(normalizeFingerprint(FP.toLowerCase().replace(/:/g, '')), FP);
    assert.equal(normalizeFingerprint('AB:CD'), null);
    assert.equal(normalizeFingerprint('zz'.repeat(32)), null);
  });

  it('apple-app-site-association: passes for the app on /connect, in either format', () => {
    assert.deepEqual(aasaProblems({ applinks: { details: [{ appIDs: [APP], components: [{ '/': '/connect*' }] }] } }, APP, PATH), []);
    assert.deepEqual(aasaProblems({ applinks: { apps: [], details: [{ appID: APP, paths: ['/connect'] }] } }, APP, PATH), []);
    assert.deepEqual(aasaProblems({ applinks: { details: [{ appIDs: [APP], components: [{ '/': '*' }] }] } }, APP, PATH), []);
  });

  it('apple-app-site-association: the first matching pattern decides, as on iOS', () => {
    const paths = (p) => aasaProblems({ applinks: { details: [{ appIDs: [APP], paths: p }] } }, APP, PATH);
    const components = (c) => aasaProblems({ applinks: { details: [{ appIDs: [APP], components: c }] } }, APP, PATH);
    assert.equal(paths(['NOT /connect*', '*']).length, 1);
    assert.deepEqual(paths(['/connect', 'NOT *']), []);
    assert.equal(components([{ '/': '/conn*', exclude: true }, { '/': '*' }]).length, 1);
    assert.deepEqual(components([{ '/': '/connect' }, { '/': '*', exclude: true }]), []);
    assert.equal(paths(['/connections']).length, 1);
  });

  it('apple-app-site-association: fails for another app, an excluded or missing path, or no details', () => {
    assert.equal(aasaProblems({ applinks: { details: [{ appIDs: ['ABCDE12345.ai.elitea.app'], paths: ['*'] }] } }, APP, PATH).length, 1);
    assert.equal(aasaProblems({ applinks: { details: [{ appIDs: [APP], components: [{ '/': '/connect*', exclude: true }] }] } }, APP, PATH).length, 1);
    assert.equal(aasaProblems({ applinks: { details: [{ appIDs: [APP], paths: ['NOT /connect', '/chats/*'] }] } }, APP, PATH).length, 1);
    assert.equal(aasaProblems({}, APP, PATH).length, 1);
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
