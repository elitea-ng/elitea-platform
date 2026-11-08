// DEF-2 (mobile E2E): the system sign-in sheet (ASWebAuthenticationSession)
// read '"EliteaAI" Wants to Use "localhost" to Sign In'. iOS words that alert
// with CFBundleName, which prebuild leaves as $(PRODUCT_NAME), the Xcode
// target name without the space; only CFBundleDisplayName (home screen) said
// "Elitea AI". Evaluates the Info.plist the way prebuild writes it.
// Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { dirname, resolve } from 'node:path';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const npx = process.platform === 'win32' ? 'npx.cmd' : 'npx';

const introspect = (brand) =>
  JSON.parse(
    execFileSync(npx, ['expo', 'config', '--type', 'introspect', '--json'], {
      cwd: root,
      env: { ...process.env, ELITEA_BRAND: brand },
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'inherit'],
    }),
  );

describe('iOS bundle name', () => {
  // Also what Siri and Shortcuts say for `\(.applicationName)` in the App Intents
  // phrases (app-intents/Intents.swift), so they follow the brand (ADR-0052 W1.4).
  for (const [brand, name] of [['', 'Elitea AI'], ['sample', 'Sample Assistant']]) {
    it(`names the app with its display name everywhere iOS shows a name (${brand || 'elitea'})`, () => {
      const config = introspect(brand);
      const displayName = config.extra.identity.displayName;
      assert.equal(displayName, name);
      const plist = config.ios.infoPlist;
      assert.equal(plist.CFBundleDisplayName, displayName);
      assert.equal(plist.CFBundleName, displayName);
    });
  }
});
