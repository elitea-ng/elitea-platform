#!/usr/bin/env node
// What .github/workflows/desktop-package.yml does with the secrets it was given
// (ADR-0052 decisions 4, 6 and 8; W2b). The workflow reads every signing secret by its
// FIXED name and calls this script; the script decides, per platform, whether to sign
// or to build an unsigned dry run, and says so in one line.
//
// - A platform's secrets ALL present: sign, notarise (macOS), verify.
// - NONE present: an unsigned build, with a notice naming the missing secrets, so
//   forks and pull requests stay green (W2b acceptance), and so a release can carry
//   an unsigned Windows build until a Windows signer exists.
// - SOME present: a failure. A half-configured signer must never fall back to an
//   unsigned build in silence.
// - `--require-signed` (the release check, per platform: macOS yes, Windows not yet):
//   a platform not fully signed fails.
//
//   node scripts/desktop-package.mjs <macos|windows|linux> [--require-signed] [--dry-run] [--github-output]
//   node scripts/desktop-package.mjs asset-name <macos|windows|linux> <ext> <brand> <version> <true|false>
//   node scripts/desktop-package.mjs release-assets <dir> <notes-file> [--complete]
//   node scripts/desktop-package.mjs notes <body-file> <section-file>
//
// --dry-run: unsigned whatever is set (a pull request: its run must not sign with the
// repository's certificates, and a fork has none).
//
// Linux has no signer: its AppImage and .deb carry no code signature (the AppImage
// format's embedded signature and a GPG detached one need a key no secret holds), and
// a download is checked against SHA256SUMS. Its plan is always `sign=false`, and its
// asset names carry no `-unsigned`, which would promise a signed build to come.
//
// The plan prints `sign=true|false` (and, for Windows, `signer=trusted|pfx|none`); with
// --github-output it also writes them to $GITHUB_OUTPUT. It never prints a secret value.
//
// asset-name prints a release asset's file name (assetName). release-assets checks that
// every file in <dir> is a release asset, writes <dir>/SHA256SUMS and the release notes'
// "Desktop downloads" section to <notes-file>: publish.yml `desktop-assets` attaches
// them to the release (owner decision 2026-11-22, amending ADR-0052 decision 10).
// notes prints the release body with that section appended (or replaced on a re-run).

import { createHash } from 'node:crypto';
import { appendFileSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { join, resolve } from 'node:path';

/**
 * The secret names the brand owner sets (W5 publishes the same list). Apple's are the
 * names the Tauri CLI reads (Developer ID certificate, App Store Connect API key for
 * notarytool) plus the Developer ID Installer identity productbuild signs the .pkg with.
 */
export const SECRETS = Object.freeze({
  macos: Object.freeze([
    'APPLE_CERTIFICATE',
    'APPLE_CERTIFICATE_PASSWORD',
    'APPLE_SIGNING_IDENTITY',
    'APPLE_INSTALLER_SIGNING_IDENTITY',
    'APPLE_API_KEY',
    'APPLE_API_ISSUER',
    'APPLE_API_PRIVATE_KEY',
  ]),
  // Two ways to sign on Windows; a brand owner sets ONE of them.
  // WINDOWS_PUBLISHER is the certificate's subject (`CN=…, O=…`): the MSIX Publisher
  // must equal it, or the package does not install.
  windowsTrusted: Object.freeze([
    'WINDOWS_PUBLISHER',
    'AZURE_CLIENT_ID',
    'AZURE_CLIENT_SECRET',
    'AZURE_TENANT_ID',
    'AZURE_SIGNING_ENDPOINT',
    'AZURE_SIGNING_ACCOUNT',
    'AZURE_SIGNING_PROFILE',
  ]),
  windowsPfx: Object.freeze(['WINDOWS_PUBLISHER', 'WINDOWS_CERTIFICATE', 'WINDOWS_CERTIFICATE_PASSWORD']),
});

const present = (env, name) => typeof env[name] === 'string' && env[name].trim() !== '';

function partition(env, names) {
  const have = names.filter((n) => present(env, n));
  const missing = names.filter((n) => !present(env, n));
  return { have, missing, all: missing.length === 0, none: have.length === 0 };
}

/**
 * The plan for `platform` given the environment `env` (names only are read for
 * presence). Throws an Error whose message is the CI annotation when it must fail.
 * @returns {{ sign: boolean, signer?: 'trusted' | 'pfx' | 'none', notice: string }}
 */
export function plan(platform, env, { requireSigned = false, dryRun = false } = {}) {
  if (platform === 'linux') {
    if (requireSigned) throw new Error('Linux: --require-signed has nothing to require (Linux packages carry no code signature)');
    return { sign: false, notice: 'Linux: the AppImage and the .deb carry no code signature; a download is checked against SHA256SUMS' };
  }
  if (dryRun) {
    if (requireSigned) throw new Error('--dry-run and --require-signed exclude each other');
    if (platform !== 'macos' && platform !== 'windows') throw new Error(`unknown platform "${platform}" (macos, windows or linux)`);
    return {
      sign: false,
      ...(platform === 'windows' ? { signer: 'none' } : {}),
      notice: `${platform === 'macos' ? 'macOS' : 'Windows'}: unsigned dry run (a pull request never signs)`,
    };
  }
  if (platform === 'macos') {
    const p = partition(env, SECRETS.macos);
    if (p.all) return { sign: true, notice: 'macOS: signing with the Developer ID, notarising and stapling' };
    if (!p.none) throw new Error(`macOS signing is half configured: set ${p.missing.join(', ')} too, or none of them`);
    if (requireSigned) throw new Error(`macOS signing secrets not set: ${p.missing.join(', ')}`);
    return { sign: false, notice: `macOS: unsigned build (no signing secrets: ${SECRETS.macos.join(', ')})` };
  }
  if (platform === 'windows') {
    // WINDOWS_PUBLISHER belongs to both sets; which signer is meant is told by the rest.
    const only = (names) => names.filter((n) => n !== 'WINDOWS_PUBLISHER');
    const t = partition(env, SECRETS.windowsTrusted);
    const x = partition(env, SECRETS.windowsPfx);
    const tOwn = partition(env, only(SECRETS.windowsTrusted));
    const xOwn = partition(env, only(SECRETS.windowsPfx));
    if (!tOwn.none && !xOwn.none) throw new Error('Windows: set either the Azure Trusted Signing secrets or the PFX secrets, not both');
    if (t.all) return { sign: true, signer: 'trusted', notice: 'Windows: signing with Azure Trusted Signing' };
    if (x.all) return { sign: true, signer: 'pfx', notice: 'Windows: signing with the code-signing certificate (PFX)' };
    if (!tOwn.none) throw new Error(`Windows Trusted Signing is half configured: set ${t.missing.join(', ')} too, or none of them`);
    if (!xOwn.none || present(env, 'WINDOWS_PUBLISHER')) throw new Error(`Windows PFX signing is half configured: set ${x.missing.join(', ')} too, or none of them`);
    if (requireSigned) throw new Error('Windows signing secrets not set (Azure Trusted Signing or PFX)');
    return { sign: false, signer: 'none', notice: 'Windows: unsigned build (no Azure Trusted Signing or PFX secrets)' };
  }
  throw new Error(`unknown platform "${platform}" (macos, windows or linux)`);
}

/**
 * What a release carries per platform (owner decision 2026-11-22, amending ADR-0052
 * decision 10): the architecture each platform is built for and its file kinds.
 * macOS: the styled, notarised .dmg (the direct download: drag the app to
 * Applications; owner decision 2026-11-22, in place of a zipped .app) and the signed
 * .pkg (device management). Windows: the MSI and the MSIX. Linux (#3034): the AppImage
 * (the download for any distribution) and the .deb (Debian, Ubuntu), x86_64, the
 * architecture's Linux name; `signs: false`, so no name says `-unsigned`.
 */
export const ASSET_KINDS = Object.freeze({
  macos: Object.freeze({ arch: 'arm64', exts: Object.freeze(['dmg', 'pkg']), signs: true }),
  windows: Object.freeze({ arch: 'x64', exts: Object.freeze(['msi', 'msix']), signs: true }),
  linux: Object.freeze({ arch: 'x86_64', exts: Object.freeze(['AppImage', 'deb']), signs: false }),
});

/** The checksum file attached next to the packages. */
export const CHECKSUMS = 'SHA256SUMS';

const VERSION = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;
const BRAND = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
// The brand is lazy and the version starts with a digit: `Acme-2-1.0.0-…` reads as brand
// `Acme-2`, the only reading whose version is X.Y.Z.
const ASSET = /^(?<brand>[A-Za-z0-9][A-Za-z0-9._-]*?)-(?<version>\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)-(?<os>macos|windows|linux)-(?<arch>arm64|x64|x86_64)(?<unsigned>-unsigned)?\.(?<ext>dmg|pkg|msix|msi|AppImage|deb)$/;

/**
 * The brand as a file-name word: accents dropped (`Café` → `Cafe`), every run of other
 * characters (spaces, `&`, `'`, `/`) one `-`, no `-` at either end. A brand's shortName is
 * free text (scripts/brand/schema.mjs); its asset names must not fail a release.
 */
export function brandSlug(brand) {
  return String(brand ?? '')
    .normalize('NFKD')
    .replace(/[̀-ͯ]/g, '')
    .replace(/[^A-Za-z0-9._-]+/g, '-')
    .replace(/^[-._]+|[-._]+$/g, '');
}

/**
 * A release asset's file name: `<Brand>-<version>-<os>-<arch>[-unsigned].<ext>`, e.g.
 * `Elitea-7.8.0-macos-arm64.pkg` or `Elitea-7.8.0-windows-x64-unsigned.msi`. The brand
 * is the app's product name (the brand manifest's shortName) as brandSlug makes it; the
 * version is the full release version (a nightly keeps its `-nightly.…`). An unsigned
 * file says so in its name, on the platforms that sign (`Elitea-7.8.0-linux-x86_64.AppImage`
 * is unsigned and says nothing: Linux never signs, ASSET_KINDS `signs`).
 */
export function assetName({ os, ext, brand, version, signed }) {
  const kind = ASSET_KINDS[os];
  if (!kind) throw new Error(`asset name: unknown platform "${os}" (macos, windows or linux)`);
  if (!kind.exts.includes(ext)) throw new Error(`asset name: ${os} has no .${ext} asset (${kind.exts.join(', ')})`);
  const b = brandSlug(brand);
  if (!BRAND.test(b)) throw new Error(`asset name: brand "${brand}" has no letter or digit for a file name`);
  if (!VERSION.test(String(version ?? ''))) throw new Error(`asset name: "${version}" is not X.Y.Z or X.Y.Z-pre`);
  if (typeof signed !== 'boolean') throw new Error('asset name: signed must be true or false');
  if (!kind.signs && signed) throw new Error(`asset name: ${os} packages carry no code signature (signed must be false)`);
  return `${b}-${version}-${os}-${kind.arch}${signed || !kind.signs ? '' : '-unsigned'}.${ext}`;
}

/** The parts of a release asset's name, or null when `name` is not one. */
export function parseAssetName(name) {
  const m = ASSET.exec(name);
  if (!m) return null;
  const kind = ASSET_KINDS[m.groups.os];
  if (kind.arch !== m.groups.arch || !kind.exts.includes(m.groups.ext)) return null;
  // A platform that never signs never writes `-unsigned` (assetName).
  if (!kind.signs && m.groups.unsigned) return null;
  return { brand: m.groups.brand, version: m.groups.version, os: m.groups.os, arch: m.groups.arch, ext: m.groups.ext, signed: kind.signs && !m.groups.unsigned };
}

/**
 * Checks the files a release is about to carry: every name is a release asset, all of
 * one brand and version, at most one of each kind, and no macOS asset is unsigned (the
 * release builds macOS with `require_signed_macos`; an unsigned Mac app does not open).
 * `complete`: every kind of ASSET_KINDS must be there (the desktop job passed).
 * @param {string[]} names
 * @returns {{ os: string, ext: string, signed: boolean, name: string }[]} sorted by name
 */
export function checkReleaseAssets(names, { complete = false } = {}) {
  if (names.length === 0) throw new Error('release assets: no desktop package to attach');
  const seen = new Map();
  let first;
  const assets = [];
  for (const name of [...names].sort()) {
    const a = parseAssetName(name);
    if (!a) throw new Error(`release assets: "${name}" is not <Brand>-<version>-<os>-<arch>[-unsigned].<ext>`);
    first ??= a;
    if (a.version !== first.version || a.brand !== first.brand) throw new Error(`release assets: "${name}" is ${a.brand} ${a.version}, the others ${first.brand} ${first.version}`);
    const key = `${a.os}.${a.ext}`;
    if (seen.has(key)) throw new Error(`release assets: two .${a.ext} files for ${a.os} (${seen.get(key)}, ${name})`);
    seen.set(key, name);
    if (a.os === 'macos' && !a.signed) throw new Error(`release assets: ${name} is unsigned; a release carries macOS signed and notarised only`);
    assets.push({ os: a.os, ext: a.ext, signed: a.signed, name });
  }
  if (complete) {
    const missing = Object.entries(ASSET_KINDS).flatMap(([os, k]) => k.exts.filter((ext) => !seen.has(`${os}.${ext}`)).map((ext) => `${os} .${ext}`));
    if (missing.length) throw new Error(`release assets: the desktop job passed but ${missing.join(', ')} did not arrive`);
  }
  return assets;
}

/** SHA256SUMS text in `sha256sum` format (`<hex>  <name>`), which `shasum -a 256 -c` reads. */
export function sha256sums(dir, names) {
  return names.map((n) => `${createHash('sha256').update(readFileSync(join(dir, n))).digest('hex')}  ${n}\n`).join('');
}

/** The notes section's first and last lines: a re-run replaces what lies between, nothing else. */
export const NOTES_MARKER = '<!-- desktop-downloads -->';
export const NOTES_END = '<!-- /desktop-downloads -->';

/** The release notes' "Desktop downloads" section for the assets checkReleaseAssets returned. */
export function releaseNotesSection(assets) {
  const by = (os, ext) => assets.find((a) => a.os === os && a.ext === ext);
  const code = (a) => `\`${a.name}\``;
  const lines = [NOTES_MARKER, '## Desktop downloads', ''];
  const dmg = by('macos', 'dmg');
  const pkg = by('macos', 'pkg');
  if (dmg || pkg) {
    const files = [dmg && `${code(dmg)} (open it, then drag the app to Applications)`, pkg && `${code(pkg)} (installs into Applications; the package for device management)`];
    lines.push(`- **macOS 12 or later, Apple silicon (M1 or later) only. Signed and notarised.** ${files.filter(Boolean).join(', or ')}. Intel Macs are not supported.`);
  }
  const windows = [by('windows', 'msi'), by('windows', 'msix')].filter(Boolean);
  if (windows.length) {
    const [msi, msix] = [by('windows', 'msi'), by('windows', 'msix')];
    if (windows.every((a) => a.signed)) {
      lines.push(`- **Windows 10 2004 or later, x64. Signed.** ${windows.map(code).join(' and ')}.`);
    } else {
      const parts = [];
      if (msi) parts.push(`${code(msi)}: Windows SmartScreen says it protected your PC from an unrecognized app; choose **More info**, then **Run anyway**.`);
      // Unsigned MSIX installs need a recent Windows 11 (Add-AppxPackage -AllowUnsigned), not the MSI's floor.
      if (msix) parts.push(`${code(msix)} is for testing: it installs only on a recent Windows 11 with Developer Mode on (Settings › System › For developers), from PowerShell: \`Add-AppxPackage -Path <file> -AllowUnsigned\`. Use the MSI.`);
      lines.push(`- **Windows 10 2004 or later, x64. Unsigned** until a Windows code-signing certificate is set up. ${parts.join(' ')}`);
    }
  }
  const [appimage, deb] = [by('linux', 'AppImage'), by('linux', 'deb')];
  if (appimage || deb) {
    const files = [
      appimage && `${code(appimage)} (any distribution with glibc 2.39 or later, such as Ubuntu 24.04, Debian 13 or Fedora 40: \`chmod +x\` it and run it; without FUSE, run it with \`--appimage-extract-and-run\`)`,
      deb && `${code(deb)} (Debian and Ubuntu: \`sudo apt install ./<file>\`)`,
    ];
    lines.push(`- **Linux, x86_64. No code signature; check the download against \`${CHECKSUMS}\`.** ${files.filter(Boolean).join(', or ')}. Install bubblewrap (\`bwrap\`) for the sandbox local commands run in.`);
  }
  lines.push(`- \`${CHECKSUMS}\` holds each file's SHA-256: \`shasum -a 256 -c ${CHECKSUMS}\` on macOS, \`sha256sum -c --ignore-missing ${CHECKSUMS}\` on Linux, \`Get-FileHash <file>\` in PowerShell.`);
  lines.push(NOTES_END);
  return `${lines.join('\n')}\n`;
}

/**
 * Release notes `body` with the section appended, or put in place of the one a run before
 * added (between NOTES_MARKER and NOTES_END): whatever follows it stays.
 */
export function withNotesSection(body, section) {
  const text = String(body ?? '');
  const at = text.indexOf(NOTES_MARKER);
  if (at < 0) {
    const kept = text.replace(/\s+$/, '');
    return `${kept ? `${kept}\n\n` : ''}${section}`;
  }
  const end = text.indexOf(NOTES_END, at);
  // No end marker: a section from before the end marker existed; it ran to the end.
  const after = end < 0 ? '' : text.slice(end + NOTES_END.length).replace(/^\n/, '');
  return `${text.slice(0, at)}${section}${after}`;
}

function planCli(platform, flags) {
  const result = plan(platform, process.env, {
    requireSigned: flags.includes('--require-signed'),
    dryRun: flags.includes('--dry-run'),
  });
  const lines = [`sign=${result.sign}`, ...(result.signer ? [`signer=${result.signer}`] : [])];
  // A notice, not a warning: an unsigned dry run is the expected result on a fork.
  console.log(`::notice::${result.notice}`);
  for (const line of lines) console.log(line);
  if (flags.includes('--github-output')) {
    if (!process.env.GITHUB_OUTPUT) throw new Error('--github-output: GITHUB_OUTPUT is not set');
    appendFileSync(process.env.GITHUB_OUTPUT, `${lines.join('\n')}\n`);
  }
}

function releaseAssetsCli([dir, notesFile, ...flags]) {
  if (!dir || !notesFile || flags.some((f) => f !== '--complete')) throw new Error('usage: desktop-package.mjs release-assets <dir> <notes-file> [--complete]');
  const assets = checkReleaseAssets(readdirSync(dir).filter((n) => n !== CHECKSUMS), { complete: flags.includes('--complete') });
  writeFileSync(join(dir, CHECKSUMS), sha256sums(dir, assets.map((a) => a.name)));
  // The body edit is gh's (publish.yml); this writes only the section.
  writeFileSync(notesFile, releaseNotesSection(assets));
  for (const a of assets) console.log(`${a.name} (${a.signed ? 'signed' : 'unsigned'})`);
  console.log(CHECKSUMS);
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const [command, ...args] = process.argv.slice(2);
  try {
    if (command === 'asset-name') {
      const [os, ext, brand, version, signed] = args;
      if (signed !== 'true' && signed !== 'false') throw new Error('usage: desktop-package.mjs asset-name <macos|windows|linux> <ext> <brand> <version> <true|false>');
      console.log(assetName({ os, ext, brand, version, signed: signed === 'true' }));
    } else if (command === 'release-assets') {
      releaseAssetsCli(args);
    } else if (command === 'notes') {
      // notes <body-file> <section-file>: prints the body with the section in place.
      process.stdout.write(withNotesSection(readFileSync(args[0], 'utf8'), readFileSync(args[1], 'utf8')));
    } else {
      planCli(command, args);
    }
  } catch (error) {
    console.log(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
