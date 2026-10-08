#!/usr/bin/env node
/**
 * Preflight for the manual EAS workflows (.github/workflows/eas-*.yml): fails
 * fast, with the runbook section to read, when the repository is not ready yet
 * (docs/release.md), instead of a long EAS error after the queue.
 *
 *   node scripts/eas-preflight.mjs build
 *   node scripts/eas-preflight.mjs submit <ios|android|all>
 */
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

/** Problems that block `mode` (empty: ready). Pure, for tests. */
export function preflightProblems(mode, platform, { config, eas }) {
  const problems = [];
  if (!config?.extra?.eas?.projectId) {
    problems.push('The app is not linked to an EAS project: run `eas init` and write the project id and owner into app.config.ts `easLink` (docs/release.md, "Link the EAS project").');
  }
  if (mode === 'submit') {
    if (!['ios', 'android', 'all'].includes(platform)) problems.push(`Unknown platform "${platform}".`);
    const ios = eas?.submit?.production?.ios ?? {};
    if ((platform === 'ios' || platform === 'all') && !/^\d+$/.test(String(ios.ascAppId ?? ''))) {
      problems.push('eas.json submit.production.ios.ascAppId is not set: copy the numeric Apple ID of the App Store Connect app (docs/release.md, "App Store Connect app record").');
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
  const eas = JSON.parse(readFileSync(resolve(root, 'eas.json'), 'utf8'));
  const [mode, platform] = process.argv.slice(2);
  const problems = preflightProblems(mode, platform, { config, eas });
  for (const problem of problems) console.error(`::error::${problem}`);
  if (problems.length > 0) process.exit(1);
  console.log(`eas preflight (${mode}${platform ? ` ${platform}` : ''}): project ${config.extra.eas.projectId} is ready`);
}
