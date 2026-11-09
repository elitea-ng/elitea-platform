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

test('the Build your own brand guide lists exactly the secrets the template uses', () => {
  const guide = fs.readFileSync(path.join(REPO, 'apps/elitea-docs/src/content/docs/development/build-your-own-brand.mdx'), 'utf8');
  const listed = new Set([...guide.matchAll(/`([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)`/g)].map((m) => m[1]).filter((n) => !['ELITEA_BRAND', 'BRAND_DIR', 'DESKTOP_WINDOWS_PACKAGE'].includes(n)));
  const used = new Set([...BUILD.matchAll(/secrets\.([A-Z_0-9]+)/g), ...read('.github/workflows/release-watch.yml').matchAll(/secrets\.([A-Z_0-9]+)/g)].map((m) => m[1]));
  assert.deepEqual([...listed].sort(), [...used].sort());
  assert.match(guide, /DESKTOP_WINDOWS_PACKAGE/);
  assert.match(BUILD, /vars\.DESKTOP_WINDOWS_PACKAGE/);
});

test('a signed mobile build puts the brand inside the tree EAS uploads, selected by id', () => {
  const step = BUILD.slice(BUILD.indexOf('- name: Build on EAS'), BUILD.indexOf('  desktop-macos:'));
  assert.match(step, /cp -R "\$GITHUB_WORKSPACE\/\$BRAND_DIR" "\.\.\/\.\.\/brand\/\$brand_id"/);
  assert.match(step, /export ELITEA_BRAND="\$brand_id"/);
  assert.ok(step.indexOf('export ELITEA_BRAND') < step.indexOf('eas build'));
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

/** The release watch's github-script body, run against a fake GitHub (a fake release tag). */
async function runWatch({ releases, forced = '', openPulls = [], branchExists = false }) {
  const yml = read('.github/workflows/release-watch.yml');
  const body = yml.slice(yml.indexOf('          script: |\n') + '          script: |\n'.length).split('\n').map((l) => l.slice(12)).join('\n');
  const workspace = fs.mkdtempSync(path.join(os.tmpdir(), 'release-watch-'));
  fs.cpSync(path.join(TEMPLATE, 'scripts'), path.join(workspace, 'scripts'), { recursive: true });
  fs.copyFileSync(path.join(TEMPLATE, 'upstream.json'), path.join(workspace, 'upstream.json'));
  const calls = [];
  const rec = (name, result = {}) => async (args) => {
    calls.push([name, args]);
    if (name === 'updateRef' && !branchExists) throw Object.assign(new Error('not found'), { status: 422 });
    return { data: result };
  };
  const github = {
    paginate: async (fn, args) => (calls.push(['listReleases', args]), releases),
    rest: {
      repos: {
        listReleases: () => {},
        getContent: rec('getContent', { sha: 'blob-sha' }),
        createOrUpdateFileContents: rec('createOrUpdateFileContents'),
      },
      git: { getRef: rec('getRef', { object: { sha: 'main-sha' } }), updateRef: rec('updateRef'), createRef: rec('createRef') },
      pulls: { list: rec('list', openPulls), create: rec('create', { number: 7 }), update: rec('update') },
    },
  };
  const notices = [];
  const core = { notice: (m) => notices.push(m) };
  const context = { repo: { owner: 'acme', repo: 'acme-brand' }, payload: { repository: { default_branch: 'main' } } };
  const env = { ...process.env, GITHUB_WORKSPACE: workspace, FORCED_TAG: forced };
  const saved = process.env;
  process.env = env;
  try {
    const AsyncFunction = (async () => {}).constructor;
    await new AsyncFunction('github', 'context', 'core', 'require', body)(github, context, core, (m) => process.getBuiltinModule(m));
  } finally {
    process.env = saved;
  }
  return { calls, notices, byName: (n) => calls.filter(([c]) => c === n).map(([, a]) => a) };
}

test('the release watch opens one pull request for a newer release (a fake tag)', async () => {
  const pinned = JSON.parse(read('upstream.json')).tag;
  const fake = 'v99.0.0';
  const run = await runWatch({ releases: [{ tag_name: fake, prerelease: false, draft: false }, { tag_name: pinned, prerelease: false, draft: false }] });
  const [write] = run.byName('createOrUpdateFileContents');
  assert.equal(write.branch, 'release-watch/upstream');
  assert.equal(write.path, 'upstream.json');
  assert.equal(JSON.parse(Buffer.from(write.content, 'base64').toString()).tag, fake);
  assert.equal(run.byName('createRef')[0].sha, 'main-sha', 'the branch starts from the default branch');
  const [pr] = run.byName('create');
  assert.equal(pr.title, `Build ELITEA ${fake}`);
  assert.equal(pr.head, 'release-watch/upstream');
  assert.equal(pr.base, 'main');
  assert.match(pr.body, new RegExp(`builds ${pinned.replaceAll('.', '\\.')}`));
});

test('the release watch updates its open pull request, and does nothing when nothing is newer', async () => {
  const again = await runWatch({ releases: [], forced: 'v99.1.0', openPulls: [{ number: 5 }], branchExists: true });
  assert.equal(again.byName('listReleases').length, 0, 'a forced tag skips the release list');
  assert.equal(again.byName('create').length, 0);
  assert.equal(again.byName('update')[0].pull_number, 5);
  assert.equal(again.byName('updateRef')[0].force, true);
  const pinned = JSON.parse(read('upstream.json')).tag;
  const same = await runWatch({ releases: [{ tag_name: pinned, prerelease: false, draft: false }, { tag_name: 'v99.0.0-nightly.1', prerelease: true, draft: false }] });
  assert.equal(same.byName('createOrUpdateFileContents').length, 0);
  assert.match(same.notices.join(), /is not newer/);
  const none = await runWatch({ releases: [] });
  assert.match(none.notices.join(), /no stable release/);
});
