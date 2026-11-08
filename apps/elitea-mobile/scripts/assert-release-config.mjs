#!/usr/bin/env node
/**
 * Release-config assertions, run for every brand in the repository's brand/ folder
 * (ADR-0052), or only for ELITEA_BRAND when it is set:
 *
 * 1. Developer (personal access token) sign-in exists only in development builds
 *    (`__DEV__`). No build-time or runtime switch may re-enable it in a release build,
 *    so the public Expo config must not carry a `devSignIn` (or similar) flag at all.
 * 2. EAS Update is OFF unless the brand's own manifest turns it on (`updates.mode: "on"`)
 *    and names its own EAS project and code signing certificate: then the update URL is
 *    that project's and every update is checked against that certificate.
 *
 * Evaluates the config the way a production build would.
 */
import { execFileSync } from 'node:child_process';
import { existsSync, readdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { BRANDS_DIR, loadBrand, loadBrandFrom } from '../../../scripts/brand/load.mjs';
import { RESERVED_IDS } from '../../../scripts/brand/schema.mjs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const npx = process.platform === 'win32' ? 'npx.cmd' : 'npx';

/** The prebuild config: the public one plus what `--type public` strips (the update certificate). */
function expoConfig(brandDir) {
  const output = execFileSync(npx, ['expo', 'config', '--type', 'prebuild', '--json'], {
    cwd: root,
    env: { ...process.env, ELITEA_BRAND: brandDir },
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'inherit'],
  });
  return JSON.parse(output);
}

/** The problems with `brand`'s release config (empty when it passes). */
function problemsFor(brand) {
  const problems = [];
  const { extra = {}, updates } = expoConfig(brand.dir);
  const switches = Object.keys(extra).filter((key) => /dev.?sign.?in|pat/i.test(key));
  if (switches.length > 0) problems.push(`developer sign-in must not be configurable, found extra.${switches.join(', extra.')}`);

  const m = brand.manifest;
  const project = m.eas.projectId?.toLowerCase();
  if (m.updates.mode !== 'on' || !project) {
    if (updates?.enabled !== false || updates.url) problems.push(`EAS Update must be off (the manifest does not turn it on), got ${JSON.stringify(updates)}`);
  } else if (process.env.EAS_PROJECT_ID && process.env.EAS_PROJECT_ID.toLowerCase() !== project) {
    if (updates?.enabled !== false) problems.push('EAS Update must be off when EAS_PROJECT_ID is not the brand\'s project');
  } else {
    if (updates?.enabled !== true) problems.push('EAS Update must be on: the manifest turns it on for its own project');
    if (updates?.url !== `https://u.expo.dev/${project}`) problems.push(`update URL must be the brand's project (${project}), got ${updates?.url}`);
    const cert = brand.relativeTo(root, m.updates.codeSigningCertificate);
    if (updates?.codeSigningCertificate !== cert) problems.push(`updates must be checked against the brand's certificate ${cert}, got ${updates?.codeSigningCertificate}`);
    if (!brand.has(m.updates.codeSigningCertificate)) problems.push(`the brand's certificate ${m.updates.codeSigningCertificate} is missing`);
  }
  return problems;
}

const brands = process.env.ELITEA_BRAND
  ? [loadBrand()]
  : readdirSync(BRANDS_DIR, { withFileTypes: true })
      .filter((d) => d.isDirectory() && !RESERVED_IDS.includes(d.name) && existsSync(join(BRANDS_DIR, d.name, 'brand.json')))
      .map((d) => loadBrandFrom(join(BRANDS_DIR, d.name)));

let failed = false;
for (const brand of brands) {
  const problems = problemsFor(brand);
  if (problems.length) {
    failed = true;
    for (const p of problems) console.error(`release config [${brand.id}]: ${p}`);
  } else {
    console.log(`release config [${brand.id}]: no developer sign-in switch; EAS Update ${brand.manifest.updates.mode === 'on' ? 'on, signed with the brand\'s certificate' : 'off'}`);
  }
}
if (failed) process.exit(1);
