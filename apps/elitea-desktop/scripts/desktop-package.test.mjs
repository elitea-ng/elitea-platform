// node --test scripts/desktop-package.test.mjs (npm run test:scripts)
//
// Desktop packaging (ADR-0052 W2b): a dry run without secrets, signing with all of
// them, a failure for a half-configured signer, and the release check that requires
// signing.
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import { plan, SECRETS } from './desktop-package.mjs';

const all = (names) => Object.fromEntries(names.map((n) => [n, 'x']));
const SCRIPT = fileURLToPath(new URL('./desktop-package.mjs', import.meta.url));
const REPO = fileURLToPath(new URL('../../../', import.meta.url));

test('no secrets: an unsigned dry run on both platforms, with a notice', () => {
  assert.equal(plan('macos', {}).sign, false);
  assert.match(plan('macos', {}).notice, /unsigned dry run/);
  assert.equal(plan('windows', {}).signer, 'none');
  // An unset secret reaches the step as an empty string.
  const blank = Object.fromEntries([...SECRETS.macos, ...SECRETS.windowsPfx].map((n) => [n, ' ']));
  assert.equal(plan('macos', blank).sign, false);
  assert.equal(plan('windows', blank).sign, false);
});

test('every secret of a platform: sign', () => {
  assert.equal(plan('macos', all(SECRETS.macos)).sign, true);
  assert.deepEqual(
    [plan('windows', all(SECRETS.windowsTrusted)).signer, plan('windows', all(SECRETS.windowsPfx)).signer],
    ['trusted', 'pfx'],
  );
});

test('a half-configured signer fails instead of building unsigned', () => {
  assert.throws(() => plan('macos', { APPLE_CERTIFICATE: 'x' }), /half configured: set .*APPLE_API_KEY/);
  assert.throws(() => plan('windows', { AZURE_CLIENT_ID: 'x' }), /Trusted Signing is half configured/);
  assert.throws(() => plan('windows', { WINDOWS_CERTIFICATE: 'x' }), /PFX signing is half configured/);
  assert.throws(() => plan('windows', { ...all(SECRETS.windowsTrusted), ...all(SECRETS.windowsPfx) }), /not both/);
});

test('a pull request never signs, even with every secret set', () => {
  assert.equal(plan('macos', all(SECRETS.macos), { dryRun: true }).sign, false);
  assert.match(plan('windows', all(SECRETS.windowsPfx), { dryRun: true }).notice, /never signs/);
  assert.throws(() => plan('macos', {}, { dryRun: true, requireSigned: true }), /exclude/);
});

test('WINDOWS_PUBLISHER alone is a half-configured signer', () => {
  assert.throws(() => plan('windows', { WINDOWS_PUBLISHER: 'CN=Acme' }), /half configured/);
});

test('the release check requires signing', () => {
  assert.throws(() => plan('macos', {}, { requireSigned: true }), /not set/);
  assert.throws(() => plan('windows', {}, { requireSigned: true }), /not set/);
  assert.equal(plan('macos', all(SECRETS.macos), { requireSigned: true }).sign, true);
});

test('the CLI prints the plan and never a secret value', () => {
  const out = execFileSync(process.execPath, [SCRIPT, 'macos'], { env: { PATH: process.env.PATH }, encoding: 'utf8' });
  assert.match(out, /^::notice::macOS: unsigned dry run/m);
  assert.match(out, /^sign=false$/m);
  const secret = 'TOPSECRET-value';
  const signed = execFileSync(process.execPath, [SCRIPT, 'windows'], {
    env: { PATH: process.env.PATH, ...Object.fromEntries(SECRETS.windowsPfx.map((n) => [n, secret])) },
    encoding: 'utf8',
  });
  assert.match(signed, /^signer=pfx$/m);
  assert.doesNotMatch(signed, /TOPSECRET/);
  const half = spawnSync(process.execPath, [SCRIPT, 'macos'], { env: { PATH: process.env.PATH, APPLE_CERTIFICATE: secret }, encoding: 'utf8' });
  assert.equal(half.status, 1);
  assert.match(half.stdout, /^::error::macOS signing is half configured/m);
  assert.doesNotMatch(half.stdout, /TOPSECRET/);
});

test('the workflows pass every signing secret by its fixed name', () => {
  const wf = readFileSync(join(REPO, '.github/workflows/desktop-package.yml'), 'utf8');
  const publish = readFileSync(join(REPO, '.github/workflows/publish.yml'), 'utf8');
  for (const name of new Set([...SECRETS.macos, ...SECRETS.windowsTrusted, ...SECRETS.windowsPfx])) {
    assert.match(publish, new RegExp(`${name}: \\$\\{\\{ secrets\\.${name} \\}\\}`), `publish.yml ${name}`);
    assert.match(wf, new RegExp(`${name}: \\{ required: false \\}`), `workflow_call ${name}`);
    assert.match(wf, new RegExp(`${name}: \\$\\{\\{ secrets\\.${name} \\}\\}`), name);
  }
});

test('nothing reaches a release: only a run by hand keeps packages (owner decision 10)', () => {
  const wf = readFileSync(join(REPO, '.github/workflows/desktop-package.yml'), 'utf8');
  const publish = readFileSync(join(REPO, '.github/workflows/publish.yml'), 'utf8');
  const uploads = wf.split('\n').flatMap((line, i, lines) => (line.includes('actions/upload-artifact@') ? [lines[i - 1]] : []));
  assert.ok(uploads.length >= 2, 'the macOS and Windows keep steps');
  for (const cond of uploads) assert.match(cond, /if: github\.event_name == 'workflow_dispatch' && inputs\.keep_artifacts$/);
  assert.doesNotMatch(wf, /gh release upload|softprops\/action-gh-release|contents: write/);
  const desktop = publish.slice(publish.indexOf('\n  desktop:'), publish.indexOf('\n  # ── Job 6'));
  assert.match(desktop, /uses: \.\/\.github\/workflows\/desktop-package\.yml/);
  assert.match(desktop, /contents: read/);
  assert.match(desktop, /require_signed: true/);
});
