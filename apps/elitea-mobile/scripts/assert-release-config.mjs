#!/usr/bin/env node
/**
 * Release-config assertion: developer (personal access token) sign-in exists
 * only in development builds (`__DEV__`). No build-time or runtime switch may
 * re-enable it in a release build, so the public Expo config must not carry a
 * `devSignIn` (or similar) flag at all. Evaluates the public config the way a
 * production build would.
 */
import { execFileSync } from 'node:child_process';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const npx = process.platform === 'win32' ? 'npx.cmd' : 'npx';
const output = execFileSync(npx, ['expo', 'config', '--type', 'public', '--json'], {
  cwd: root,
  env: process.env,
  encoding: 'utf8',
  stdio: ['ignore', 'pipe', 'inherit'],
});
const config = JSON.parse(output);
const extra = config.extra ?? {};
const switches = Object.keys(extra).filter((key) => /dev.?sign.?in|pat/i.test(key));
if (switches.length > 0) {
  console.error(`release config: developer sign-in must not be configurable, found extra.${switches.join(', extra.')}`);
  process.exit(1);
}
console.log('release config: no developer sign-in switch (developer sign-in is __DEV__ only)');
