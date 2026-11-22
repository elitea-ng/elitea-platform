// node --test scripts/desktop-package.test.mjs (npm run test:scripts)
//
// Desktop packaging (ADR-0052 W2b): an unsigned build without secrets, signing with all
// of them, a failure for a half-configured signer, the release check that requires
// signing per platform (macOS yes, Windows not until a signer exists), and the release
// assets: their names, SHA256SUMS and the release notes' "Desktop downloads" section
// (owner decision 2026-11-22, amending ADR-0052 decision 10).
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  assetName,
  brandSlug,
  CHECKSUMS,
  checkReleaseAssets,
  NOTES_END,
  NOTES_MARKER,
  parseAssetName,
  plan,
  releaseNotesSection,
  SECRETS,
  sha256sums,
  withNotesSection,
} from './desktop-package.mjs';

const all = (names) => Object.fromEntries(names.map((n) => [n, 'x']));
const SCRIPT = fileURLToPath(new URL('./desktop-package.mjs', import.meta.url));
const REPO = fileURLToPath(new URL('../../../', import.meta.url));

test('no secrets: an unsigned build on both platforms, with a notice', () => {
  assert.equal(plan('macos', {}).sign, false);
  assert.match(plan('macos', {}).notice, /unsigned build/);
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
  assert.match(out, /^::notice::macOS: unsigned build/m);
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
    // The build is a composite action: each secret goes in as its kebab-case input.
    const input = name.toLowerCase().replaceAll('_', '-');
    assert.match(wf, new RegExp(`${input}: \\$\\{\\{ secrets\\.${name} \\}\\}`), name);
  }
});

test('the composite actions take every secret of their platform as an input', () => {
  const actions = {
    macos: readFileSync(join(REPO, '.github/actions/desktop-macos-package/action.yml'), 'utf8'),
    windows: readFileSync(join(REPO, '.github/actions/desktop-windows-package/action.yml'), 'utf8'),
  };
  const names = { macos: SECRETS.macos, windows: [...new Set([...SECRETS.windowsTrusted, ...SECRETS.windowsPfx])] };
  for (const [platform, list] of Object.entries(names)) {
    for (const name of list) {
      const input = name.toLowerCase().replaceAll('_', '-');
      assert.match(actions[platform], new RegExp(`^  ${input}: \\{ description: "Secret ${name}`, 'm'), `${platform} input ${input}`);
      // ...and hands it to the planner under its fixed name.
      assert.match(actions[platform], new RegExp(`${name}: \\$\\{\\{ inputs\\.${input} \\}\\}`), `${platform} ${name}`);
    }
  }
});

test('signing is required per platform: macOS always, Windows not until a signer exists', () => {
  // The release's two calls, as desktop-package.yml makes them.
  assert.equal(plan('macos', all(SECRETS.macos), { requireSigned: true }).sign, true);
  assert.throws(() => plan('macos', {}, { requireSigned: true }), /macOS signing secrets not set/);
  const windows = plan('windows', {}, { requireSigned: false });
  assert.deepEqual([windows.sign, windows.signer], [false, 'none']);
  assert.match(windows.notice, /unsigned build/);
  // A signer's full set signs with no change to the call; a partial set still fails.
  assert.equal(plan('windows', all(SECRETS.windowsPfx), { requireSigned: false }).signer, 'pfx');
  assert.throws(() => plan('windows', { WINDOWS_CERTIFICATE: 'x' }, { requireSigned: false }), /half configured/);
  assert.throws(() => plan('windows', { AZURE_CLIENT_ID: 'x' }, { requireSigned: false }), /half configured/);
});

test('the workflows wire require_signed per platform, each as its own input', () => {
  const wf = readFileSync(join(REPO, '.github/workflows/desktop-package.yml'), 'utf8');
  const ci = readFileSync(join(REPO, '.github/workflows/ci.yml'), 'utf8');
  const publish = readFileSync(join(REPO, '.github/workflows/publish.yml'), 'utf8');
  assert.match(wf, /^ {6}require_signed_macos:\n {8}description: .*\n {8}type: boolean\n {8}default: true$/m);
  assert.match(wf, /^ {6}require_signed_windows:\n(?: {8}.*\n)+? {8}type: boolean\n {8}default: false$/m);
  assert.doesNotMatch(wf, /inputs\.require_signed\b(?!_)/, 'no shared require_signed input');
  const macos = wf.slice(wf.indexOf('\n  macos:'), wf.indexOf('\n  windows:'));
  const windowsJob = wf.slice(wf.indexOf('\n  windows:'));
  assert.match(macos, /require-signed: \$\{\{ inputs\.require_signed_macos == true \|\| inputs\.keep_artifacts == true \}\}/);
  assert.match(windowsJob, /require-signed: \$\{\{ inputs\.require_signed_windows == true \}\}/);
  // The Windows job is on: no variable gates it.
  assert.doesNotMatch(wf, /vars\.DESKTOP_WINDOWS_PACKAGE/);
  assert.doesNotMatch(windowsJob.split('\n    steps:')[0], /^ {4}if:/m);
  const call = ci.slice(ci.indexOf('\n  desktop-package:'), ci.indexOf('\n  helm-install-smoke:'));
  assert.match(call, /require_signed_macos: false/);
  assert.match(call, /require_signed_windows: false/);
  assert.doesNotMatch(call, /upload_release_assets/);
  const desktop = publish.slice(publish.indexOf('\n  desktop:'), publish.indexOf('\n  # ── Job 5b'));
  assert.match(desktop, /uses: \.\/\.github\/workflows\/desktop-package\.yml/);
  assert.match(desktop, /contents: read/);
  assert.match(desktop, /require_signed_macos: true/);
  assert.match(desktop, /require_signed_windows: false/);
  assert.match(desktop, /upload_release_assets: true/);
});

test('the packages reach the release through one job, the only one that writes to it', () => {
  const wf = readFileSync(join(REPO, '.github/workflows/desktop-package.yml'), 'utf8');
  const publish = readFileSync(join(REPO, '.github/workflows/publish.yml'), 'utf8');
  // desktop-package.yml never writes to a release; it uploads one-day artifacts on a release call only.
  assert.doesNotMatch(wf, /gh release|softprops\/action-gh-release|contents: write/);
  const uploads = wf.split('\n').flatMap((line, i, lines) => (line.includes('actions/upload-artifact@') ? [lines.slice(i - 2, i + 6).join('\n')] : []));
  assert.equal(uploads.length, 4, 'a keep step and a release step per platform');
  for (const u of uploads) {
    if (/name: desktop-release-(macos|windows)/.test(u)) {
      assert.match(u, /if: inputs\.upload_release_assets\n/);
      assert.match(u, /retention-days: 1/);
    } else {
      assert.match(u, /if: github\.event_name == 'workflow_dispatch' && inputs\.keep_artifacts\n/);
    }
  }
  const job = publish.slice(publish.indexOf('\n  desktop-assets:'), publish.indexOf('\n  # ── Job 6'));
  assert.match(job, /needs: \[release, desktop\]/);
  assert.match(job, /if: \$\{\{ !cancelled\(\) && needs\.release\.outputs\.new_release_published == 'true' \}\}/);
  assert.match(job, /permissions:\n {6}contents: write\n {4}steps:/);
  assert.match(job, /pattern: desktop-release-\*/);
  assert.match(job, /node "\$script" release-assets "\$dir"/);
  assert.match(job, /gh release upload "\$TAG" "\$dir"\/\* --clobber/);
  assert.match(job, /gh release edit "\$TAG" .*--notes-file/);
  // Rollback never waits on the desktop: a failed package must not delete a release.
  const rollback = publish.slice(publish.indexOf('\n  rollback:'));
  assert.doesNotMatch(rollback.match(/^ {4}needs: .*$/m)[0], /desktop/);
});

test('asset names: <Brand>-<version>-<os>-<arch>[-unsigned].<ext>', () => {
  const name = (os, ext, signed, version = '7.8.0', brand = 'Elitea') => assetName({ os, ext, brand, version, signed });
  assert.equal(name('macos', 'pkg', true), 'Elitea-7.8.0-macos-arm64.pkg');
  assert.equal(name('macos', 'app.zip', true), 'Elitea-7.8.0-macos-arm64.app.zip');
  assert.equal(name('windows', 'msi', false), 'Elitea-7.8.0-windows-x64-unsigned.msi');
  assert.equal(name('windows', 'msix', false), 'Elitea-7.8.0-windows-x64-unsigned.msix');
  assert.equal(name('windows', 'msi', true), 'Elitea-7.8.0-windows-x64.msi');
  // A nightly keeps its full version; a product name with spaces becomes one word.
  assert.equal(name('windows', 'msix', false, '7.9.0-nightly.20261122.412'), 'Elitea-7.9.0-nightly.20261122.412-windows-x64-unsigned.msix');
  assert.equal(name('macos', 'pkg', true, '1.0.0', 'Acme Agent'), 'Acme-Agent-1.0.0-macos-arm64.pkg');
  assert.throws(() => name('linux', 'deb', false), /unknown platform/);
  assert.throws(() => name('macos', 'msi', true), /macos has no \.msi asset/);
  assert.throws(() => name('macos', 'pkg', true, '7.8'), /not X\.Y\.Z/);
  // Any shortName yields a file name: accents dropped, other characters one `-`.
  assert.equal(brandSlug('Café Bot'), 'Cafe-Bot');
  assert.equal(brandSlug("Acme & O'Brien / Co."), 'Acme-O-Brien-Co');
  assert.equal(name('windows', 'msi', false, '1.0.0', 'Café Bot'), 'Cafe-Bot-1.0.0-windows-x64-unsigned.msi');
  assert.ok(parseAssetName(name('macos', 'pkg', true, '1.0.0', 'Acme & Co')));
  assert.throws(() => name('macos', 'pkg', true, '7.8.0', '***'), /no letter or digit/);
  assert.throws(() => assetName({ os: 'macos', ext: 'pkg', brand: 'E', version: '1.0.0', signed: 'true' }), /signed must be/);
});

test('asset names parse back, and nothing else parses', () => {
  for (const [os, ext, signed] of [['macos', 'pkg', true], ['macos', 'app.zip', true], ['windows', 'msi', false], ['windows', 'msix', true]]) {
    const n = assetName({ os, ext, brand: 'Acme-2', version: '1.2.3-nightly.20261122.7', signed });
    assert.deepEqual(parseAssetName(n), { brand: 'Acme-2', version: '1.2.3-nightly.20261122.7', os, arch: os === 'macos' ? 'arm64' : 'x64', ext, signed });
  }
  for (const n of ['Elitea_7.8.0_x64_en-US.msi', 'Elitea-7.8.0-arm64.pkg', 'Elitea-7.8.0-macos-x64.pkg', 'Elitea-7.8.0-windows-x64.pkg', 'Elitea-7.8.0-macos-arm64.zip', CHECKSUMS]) {
    assert.equal(parseAssetName(n), null, n);
  }
});

const RELEASE = ['Elitea-7.8.0-macos-arm64.pkg', 'Elitea-7.8.0-macos-arm64.app.zip', 'Elitea-7.8.0-windows-x64-unsigned.msi', 'Elitea-7.8.0-windows-x64-unsigned.msix'];

test('release assets: one version, one of each kind, never an unsigned Mac app', () => {
  const assets = checkReleaseAssets(RELEASE, { complete: true });
  assert.deepEqual(assets.map((a) => a.name), [...RELEASE].sort());
  assert.deepEqual(assets.filter((a) => !a.signed).map((a) => a.os), ['windows', 'windows']);
  // A failed Windows build: the macOS packages alone, but not when the job passed.
  assert.equal(checkReleaseAssets(RELEASE.slice(0, 2)).length, 2);
  assert.throws(() => checkReleaseAssets(RELEASE.slice(0, 2), { complete: true }), /windows \.msi, windows \.msix did not arrive/);
  assert.throws(() => checkReleaseAssets([]), /no desktop package/);
  assert.throws(() => checkReleaseAssets([...RELEASE, 'Elitea_7.8.0_x64_en-US.msi']), /is not <Brand>/);
  assert.throws(() => checkReleaseAssets([...RELEASE.slice(1), 'Elitea-7.8.1-macos-arm64.pkg']), /7\.8\.1, the others/);
  assert.throws(() => checkReleaseAssets([...RELEASE, 'Elitea-7.8.0-windows-x64.msi']), /two \.msi files for windows/);
  assert.throws(() => checkReleaseAssets(['Elitea-7.8.0-macos-arm64-unsigned.pkg']), /unsigned; a release carries macOS signed/);
});

test('SHA256SUMS is sha256sum format over every asset', () => {
  const dir = mkdtempSync(join(tmpdir(), 'desktop-assets-'));
  try {
    writeFileSync(join(dir, 'a.msi'), 'abc');
    writeFileSync(join(dir, 'b.pkg'), '');
    assert.equal(
      sha256sums(dir, ['a.msi', 'b.pkg']),
      'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad  a.msi\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  b.pkg\n',
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('the notes section: macOS signed and notarised, Windows unsigned with the way past SmartScreen', () => {
  const section = releaseNotesSection(checkReleaseAssets(RELEASE));
  assert.ok(section.startsWith(`${NOTES_MARKER}\n## Desktop downloads\n`));
  assert.match(section, /macOS 12 or later, Apple silicon \(M1 or later\) only\. Signed and notarised\.\*\* `Elitea-7\.8\.0-macos-arm64\.pkg`/);
  assert.match(section, /`Elitea-7\.8\.0-macos-arm64\.app\.zip` \(unzip/);
  assert.match(section, /Intel Macs are not supported/);
  assert.match(section, /Windows 10 2004 or later, x64\. Unsigned\*\*/);
  assert.match(section, /SmartScreen .*\*\*More info\*\*, then \*\*Run anyway\*\*/);
  assert.match(section, /recent Windows 11 with Developer Mode on .*Add-AppxPackage -Path <file> -AllowUnsigned/);
  assert.match(section, /`SHA256SUMS` holds each file's SHA-256: `shasum -a 256 -c SHA256SUMS`/);
  // Signed Windows: no warning to click through.
  const signed = releaseNotesSection(checkReleaseAssets(['Elitea-7.8.0-windows-x64.msi', 'Elitea-7.8.0-windows-x64.msix']));
  assert.match(signed, /x64\. Signed\.\*\* `Elitea-7\.8\.0-windows-x64\.msi` and `Elitea-7\.8\.0-windows-x64\.msix`\./);
  assert.doesNotMatch(signed, /SmartScreen|Apple silicon/);
  // macOS alone (a failed Windows build): no Windows line.
  assert.doesNotMatch(releaseNotesSection(checkReleaseAssets(RELEASE.slice(0, 2))), /Windows 10/);
});

test('the notes section is appended once: a re-run replaces it', () => {
  const section = releaseNotesSection(checkReleaseAssets(RELEASE));
  const once = withNotesSection('## Features\n\n* thing\n\n', section);
  assert.equal(once, `## Features\n\n* thing\n\n${section}`);
  assert.equal(withNotesSection(once, section), once);
  const fewer = releaseNotesSection(checkReleaseAssets(RELEASE.slice(0, 2)));
  assert.equal(withNotesSection(once, fewer), `## Features\n\n* thing\n\n${fewer}`);
  assert.equal(withNotesSection('', section), section);
  assert.ok(section.trimEnd().endsWith(NOTES_END));
  // What follows the section (a hand edit, a later section) survives a re-run.
  const edited = `${once}\n## Known issues\n\n* one\n`;
  assert.equal(withNotesSection(edited, fewer), `## Features\n\n* thing\n\n${fewer}\n## Known issues\n\n* one\n`);
  // A section without its end marker ran to the end of the body.
  assert.equal(withNotesSection(`a\n\n${NOTES_MARKER}\nold\n`, section), `a\n\n${section}`);
});

test('the release-assets CLI checks the folder, writes SHA256SUMS and the section', () => {
  const dir = mkdtempSync(join(tmpdir(), 'desktop-assets-'));
  try {
    const assets = join(dir, 'assets');
    mkdirSync(assets);
    for (const n of RELEASE) writeFileSync(join(assets, n), n);
    const out = execFileSync(process.execPath, [SCRIPT, 'release-assets', assets, join(dir, 'section.md'), '--complete'], { encoding: 'utf8' });
    assert.match(out, /^Elitea-7\.8\.0-windows-x64-unsigned\.msi \(unsigned\)$/m);
    const sums = readFileSync(join(assets, CHECKSUMS), 'utf8').trim().split('\n');
    assert.deepEqual(sums.map((l) => l.split('  ')[1]), [...RELEASE].sort());
    assert.match(readFileSync(join(dir, 'section.md'), 'utf8'), /## Desktop downloads/);
    // A re-run (SHA256SUMS already there) is fine; a stray file is not.
    execFileSync(process.execPath, [SCRIPT, 'release-assets', assets, join(dir, 'section.md')], { encoding: 'utf8' });
    writeFileSync(join(assets, 'notes.txt'), '');
    const bad = spawnSync(process.execPath, [SCRIPT, 'release-assets', assets, join(dir, 'section.md')], { encoding: 'utf8' });
    assert.equal(bad.status, 1);
    assert.match(bad.stdout, /^::error::release assets: "notes\.txt" is not/m);
    const name = execFileSync(process.execPath, [SCRIPT, 'asset-name', 'windows', 'msi', 'Elitea', '7.8.0', 'false'], { encoding: 'utf8' });
    assert.equal(name, 'Elitea-7.8.0-windows-x64-unsigned.msi\n');
    writeFileSync(join(dir, 'body.md'), 'notes\n');
    const body = execFileSync(process.execPath, [SCRIPT, 'notes', join(dir, 'body.md'), join(dir, 'section.md')], { encoding: 'utf8' });
    assert.ok(body.startsWith(`notes\n\n${NOTES_MARKER}\n`));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
