// DEF-2 (Agent Zefir E2E): the system sign-in sheet (ASWebAuthenticationSession)
// read '"AgentZefir" Wants to Use "localhost" to Sign In'. iOS words that alert
// with CFBundleName, which prebuild leaves as $(PRODUCT_NAME), the Xcode
// target name without the space; only CFBundleDisplayName (home screen) said
// "Agent Zefir". Evaluates the Info.plist the way prebuild writes it.
// Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { dirname, resolve } from 'node:path';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const npx = process.platform === 'win32' ? 'npx.cmd' : 'npx';

describe('iOS bundle name', () => {
  it('names the app with its display name everywhere iOS shows a name', () => {
    const output = execFileSync(npx, ['expo', 'config', '--type', 'introspect', '--json'], {
      cwd: root,
      env: process.env,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'inherit'],
    });
    const config = JSON.parse(output);
    const displayName = config.extra.identity.displayName;
    const plist = config.ios.infoPlist;
    assert.equal(plist.CFBundleDisplayName, displayName);
    assert.equal(plist.CFBundleName, displayName);
  });
});
