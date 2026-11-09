#!/usr/bin/env node
// What .github/workflows/desktop-package.yml does with the secrets it was given
// (ADR-0052 decisions 4, 6 and 8; W2b). The workflow reads every signing secret by its
// FIXED name and calls this script; the script decides, per platform, whether to sign
// or to build an unsigned dry run, and says so in one line.
//
// - A platform's secrets ALL present: sign, notarise (macOS), verify.
// - NONE present: an unsigned dry run, with a notice naming the missing secrets, so
//   forks and pull requests stay green (W2b acceptance).
// - SOME present: a failure. A half-configured signer must never fall back to an
//   unsigned build in silence.
// - `--require-signed` (the release check): any platform not fully signed fails.
//
//   node scripts/desktop-package.mjs <macos|windows> [--require-signed] [--dry-run] [--github-output]
//
// --dry-run: unsigned whatever is set (a pull request: its run must not sign with the
// repository's certificates, and a fork has none).
//
// It prints `sign=true|false` (and, for Windows, `signer=trusted|pfx|none`); with
// --github-output it also writes them to $GITHUB_OUTPUT. It never prints a secret value.

import { appendFileSync } from 'node:fs';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

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
  if (dryRun) {
    if (requireSigned) throw new Error('--dry-run and --require-signed exclude each other');
    if (platform !== 'macos' && platform !== 'windows') throw new Error(`unknown platform "${platform}" (macos or windows)`);
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
    return { sign: false, notice: `macOS: unsigned dry run (no signing secrets: ${SECRETS.macos.join(', ')})` };
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
    return { sign: false, signer: 'none', notice: 'Windows: unsigned dry run (no Azure Trusted Signing or PFX secrets)' };
  }
  throw new Error(`unknown platform "${platform}" (macos or windows)`);
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const [platform, ...flags] = process.argv.slice(2);
  try {
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
  } catch (error) {
    console.log(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
