#!/usr/bin/env node
/**
 * Preflight for the manual EAS workflows (.github/workflows/mobile-eas-*.yml at the repository root): fails
 * fast, with the runbook section to read, when the repository is not ready yet
 * (docs/release.md), instead of a long EAS error after the queue.
 *
 *   node scripts/eas-preflight.mjs build
 *   node scripts/eas-preflight.mjs submit <ios|android|all>
 *
 * For an iOS submit it also writes the brand's App Store Connect app id (brand.json
 * `mobile.ascAppId`) into the runner's copy of eas.json: the committed file names no
 * identifier, and `eas submit` reads the id from nowhere else (ADR-0052 W1.5).
 */
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

/** Problems that block `mode` (empty: ready). Pure, for tests. */
export function preflightProblems(mode, platform, { config, ascAppId }) {
  const problems = [];
  if (!config?.extra?.eas?.projectId) {
    problems.push('The app is not linked to an EAS project: run `eas init` and write the project id and owner into the brand manifest\'s `eas` (docs/release.md, "Link the EAS project").');
  }
  if (mode === 'submit') {
    if (!['ios', 'android', 'all'].includes(platform)) problems.push(`Unknown platform "${platform}".`);
    if ((platform === 'ios' || platform === 'all') && !/^\d+$/.test(String(ascAppId ?? ''))) {
      problems.push('The brand manifest\'s mobile.ascAppId is not set: copy the numeric Apple ID of the App Store Connect app into brand.json (docs/release.md, "App Store Connect app record").');
    }
  } else if (mode !== 'build') {
    problems.push(`Unknown mode "${mode}" (build or submit).`);
  }
  return problems;
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
  const npx = process.platform === 'win32' ? 'npx.cmd' : 'npx';
  const config = JSON.parse(
    execFileSync(npx, ['expo', 'config', '--type', 'public', '--json'], {
      cwd: root,
      env: process.env,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'inherit'],
    }),
  );
  const { loadBrand } = await import('../../../scripts/brand/load.mjs');
  const ascAppId = loadBrand().manifest.mobile.ascAppId ?? null;
  const [mode, platform] = process.argv.slice(2);
  const problems = preflightProblems(mode, platform, { config, ascAppId });
  for (const problem of problems) console.error(`::error::${problem}`);
  if (problems.length > 0) process.exit(1);
  if (mode === 'submit' && ascAppId) {
    // The runner's checkout only: never committed (eas-config.test.mjs keeps eas.json id-free).
    const file = resolve(root, 'eas.json');
    const eas = JSON.parse(readFileSync(file, 'utf8'));
    eas.submit.production.ios = { ...eas.submit.production.ios, ascAppId };
    writeFileSync(file, `${JSON.stringify(eas, null, 2)}\n`);
  }
  console.log(`eas preflight (${mode}${platform ? ` ${platform}` : ''}): project ${config.extra.eas.projectId} is ready`);
}
