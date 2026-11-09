// node --test scripts/ci/brand-template.test.mjs (run by no-binaries.yml)
//
// The brand repository template (ADR-0052 W5, docs/brand-template/) against the
// tree it builds: the template's brand passes this release's brand-check and
// selects every client; this tree is a release the template can build; and the
// template passes every signing secret, by its fixed name, to the packaging
// actions and the EAS step the template calls.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import { SECRETS } from '../../apps/elitea-desktop/scripts/desktop-package.mjs';
import { placeholderAssets } from '../brand/placeholder-assets.mjs';
import { loadBrandFrom } from '../brand/load.mjs';
import { missing, REQUIRED } from '../../docs/brand-template/scripts/check-release.mjs';

const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const TEMPLATE = path.join(REPO, 'docs/brand-template');
const read = (rel) => fs.readFileSync(path.join(TEMPLATE, rel), 'utf8');
const BUILD = read('.github/workflows/build.yml');
const kebab = (name) => name.toLowerCase().replaceAll('_', '-');

/** The template's brand with placeholder assets, in a temp folder named for its id. */
function templateBrand() {
  const dir = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'brand-template-')), 'acme');
  fs.mkdirSync(dir);
  fs.copyFileSync(path.join(TEMPLATE, 'brand/acme/brand.json'), path.join(dir, 'brand.json'));
  for (const [rel, bytes] of placeholderAssets({ fg: '#7C3AED', bg: '#1F2933' })) {
    fs.mkdirSync(path.dirname(path.join(dir, 'assets', rel)), { recursive: true });
    fs.writeFileSync(path.join(dir, 'assets', rel), bytes);
  }
  return dir;
}

test('this tree is a release the template can build', () => {
  assert.deepEqual(missing(REPO), []);
  for (const [rel] of REQUIRED) assert.ok(fs.existsSync(path.join(REPO, rel)), rel);
  // The actions the template calls are the ones this tree's release check runs.
  const pkg = fs.readFileSync(path.join(REPO, '.github/workflows/desktop-package.yml'), 'utf8');
  assert.match(pkg, /uses: \.\/\.github\/actions\/desktop-macos-package/);
  assert.match(pkg, /uses: \.\/\.github\/actions\/desktop-windows-package/);
  assert.match(BUILD, /uses: \.\/elitea\/\.github\/actions\/desktop-macos-package/);
  assert.match(BUILD, /uses: \.\/elitea\/\.github\/actions\/desktop-windows-package/);
});

test("the template's brand passes this release's brand-check", () => {
  const dir = templateBrand();
  const run = spawnSync(process.execPath, [path.join(REPO, 'scripts/brand-check.mjs'), dir], { encoding: 'utf8' });
  assert.equal(run.status, 0, run.stdout + run.stderr);
  const brand = loadBrandFrom(dir);
  assert.equal(brand.id, 'acme');
  assert.match(BUILD, /^ {2}BRAND_DIR: brand\/acme$/m);
});

test('the template passes every signing secret by its fixed name', () => {
  for (const name of SECRETS.macos) assert.match(BUILD, new RegExp(`${kebab(name)}: \\$\\{\\{ secrets\\.${name} \\}\\}`), name);
  for (const name of new Set([...SECRETS.windowsTrusted, ...SECRETS.windowsPfx])) {
    assert.match(BUILD, new RegExp(`${kebab(name)}: \\$\\{\\{ secrets\\.${name} \\}\\}`), name);
  }
  assert.match(BUILD, /EXPO_TOKEN: \$\{\{ secrets\.EXPO_TOKEN \}\}/);
  // Only those names: a template secret this tree does not know is a typo.
  const known = new Set([...SECRETS.macos, ...SECRETS.windowsTrusted, ...SECRETS.windowsPfx, 'EXPO_TOKEN', 'RELEASE_WATCH_TOKEN']);
  const used = new Set([...BUILD.matchAll(/secrets\.([A-Z_0-9]+)/g), ...read('.github/workflows/release-watch.yml').matchAll(/secrets\.([A-Z_0-9]+)/g)].map((m) => m[1]));
  for (const name of used) assert.ok(known.has(name), `unknown secret ${name}`);
});

test('a pull request never signs, and nothing is kept except by hand', () => {
  for (const m of BUILD.matchAll(/dry-run: (.*)$/gm)) assert.equal(m[1], "${{ github.event_name == 'pull_request' }}");
  const uploads = BUILD.split('\n').flatMap((line, i, lines) => (line.includes('actions/upload-artifact@') ? [lines[i - 1]] : []));
  assert.equal(uploads.length, 2);
  for (const cond of uploads) assert.match(cond, /if: github\.event_name == 'workflow_dispatch' && inputs\.keep_artifacts$/);
  assert.doesNotMatch(BUILD, /^\s+push:/m, 'no push trigger');
  assert.match(BUILD, /^permissions:\n {2}contents: read$/m);
});

test('the template checks out the pinned release, never a branch', () => {
  const checkouts = [...BUILD.matchAll(/repository: (.*)\n\s+ref: (.*)\n\s+path: elitea/g)];
  assert.equal(checkouts.length, 4, 'prepare, mobile, macOS, Windows');
  for (const [, repository, ref] of checkouts) {
    assert.match(repository, /outputs\.repository \}\}$/);
    assert.match(ref, /outputs\.tag \}\}$/);
  }
  assert.match(BUILD, /node scripts\/upstream\.mjs github-output/);
});

test("the template's own tests pass", () => {
  const run = spawnSync(process.execPath, ['--test', path.join(TEMPLATE, 'scripts/upstream.test.mjs')], { encoding: 'utf8' });
  assert.equal(run.status, 0, run.stdout + run.stderr);
});
