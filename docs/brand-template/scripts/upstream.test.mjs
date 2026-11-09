// node --test scripts/upstream.test.mjs
//
// The brand template's view of ELITEA's releases (ADR-0052 W5): upstream.json is
// validated, the newest release of a channel is picked by SemVer order (not by
// list order or string order), and a bump is written only for a newer tag.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import { bumped, compareTags, newestRelease, readUpstream, validate } from './upstream.mjs';

const pin = (tag, channel = 'stable') => JSON.stringify({ repository: 'elitea-ng/elitea-platform', tag, channel }, null, 2);
const release = (tag_name, prerelease = /-/.test(tag_name), draft = false) => ({ tag_name, prerelease, draft });

test('the committed upstream.json is valid', () => {
  assert.doesNotThrow(() => readUpstream());
});

test('upstream.json refuses what the watch cannot follow', () => {
  assert.deepEqual(validate(JSON.parse(pin('v7.6.0'))), []);
  assert.deepEqual(validate(JSON.parse(pin('v7.6.0-nightly.20261109.525', 'prerelease'))), []);
  assert.match(validate(JSON.parse(pin('7.6.0'))).join(), /tag must be a release tag/);
  assert.match(validate(JSON.parse(pin('main'))).join(), /tag must be a release tag/);
  assert.match(validate(JSON.parse(pin('v7.6.0-nightly.1'))).join(), /stable channel pins a stable tag/);
  assert.match(validate({ ...JSON.parse(pin('v7.6.0')), channel: 'nightly' }).join(), /channel must be one of/);
  assert.match(validate({ ...JSON.parse(pin('v7.6.0')), repository: 'nope' }).join(), /owner\/name/);
  assert.match(validate({ ...JSON.parse(pin('v7.6.0')), ref: 'main' }).join(), /unknown key "ref"/);
});

test('tags order by SemVer, prereleases before their release', () => {
  const order = ['v7.5.0', 'v7.6.0-nightly.20261107.523', 'v7.6.0-nightly.20261109.525', 'v7.6.0', 'v7.10.0', 'v10.0.0'];
  const shuffled = [...order].reverse();
  assert.deepEqual(shuffled.sort(compareTags), order);
  assert.ok(compareTags('v7.6.0-nightly.20261109.99', 'v7.6.0-nightly.20261109.100') < 0, 'numeric ids compare as numbers');
  assert.throws(() => compareTags('v7', 'v7.6.0'), /not a release tag/);
});

test('the newest release: stable skips prereleases and drafts, prerelease takes them', () => {
  const releases = [
    release('v7.6.0-nightly.20261109.525'),
    release('v7.5.0', false),
    release('v7.10.0', false, true),
    release('v7.4.0', false),
    release('docs-2026-11', false),
  ];
  assert.equal(newestRelease(releases, 'stable'), 'v7.5.0');
  assert.equal(newestRelease(releases, 'prerelease'), 'v7.6.0-nightly.20261109.525');
  assert.equal(newestRelease([], 'stable'), null);
  // A stable-looking tag GitHub marks as a prerelease stays out of `stable`.
  assert.equal(newestRelease([release('v8.0.0', true), release('v7.5.0', false)], 'stable'), 'v7.5.0');
});

test('a bump is written only for a newer tag, keeping the other keys', () => {
  const text = `${JSON.stringify({ $comment: 'keep me', repository: 'elitea-ng/elitea-platform', tag: 'v7.6.0', channel: 'stable' }, null, 2)}\n`;
  const next = bumped(text, 'v7.7.0');
  assert.equal(JSON.parse(next).tag, 'v7.7.0');
  assert.equal(JSON.parse(next).$comment, 'keep me');
  assert.ok(next.endsWith('}\n'));
  assert.equal(bumped(text, 'v7.6.0'), null);
  assert.equal(bumped(text, 'v7.5.0'), null);
  assert.throws(() => bumped(pin('main'), 'v7.7.0'), /upstream.json is invalid/);
});

test('the CLI writes the pin for the build jobs', () => {
  const out = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'upstream-')), 'out');
  fs.writeFileSync(out, '');
  const { spawnSync } = process.getBuiltinModule('node:child_process');
  const script = path.join(import.meta.dirname, 'upstream.mjs');
  const run = spawnSync(process.execPath, [script, 'github-output'], { env: { ...process.env, GITHUB_OUTPUT: out }, encoding: 'utf8' });
  assert.equal(run.status, 0, run.stdout);
  const { tag, repository } = readUpstream();
  assert.equal(fs.readFileSync(out, 'utf8'), `tag=${tag}\nrepository=${repository}\nversion=${tag.slice(1)}\n`);
  assert.equal(spawnSync(process.execPath, [script, 'nope'], { encoding: 'utf8' }).status, 2);
});
