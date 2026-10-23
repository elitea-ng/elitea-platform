// Issue #15: iPad turns every way and runs in Split View / Stage Manager; phones stay portrait.
// Evaluates the Info.plist the way prebuild writes it (expo config --type introspect), and
// checks no screen locks its own orientation (react-native-screens' `orientation` would
// override the plist on iPad too). Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const npx = process.platform === 'win32' ? 'npx.cmd' : 'npx';

const ALL = [
  'UIInterfaceOrientationPortrait',
  'UIInterfaceOrientationPortraitUpsideDown',
  'UIInterfaceOrientationLandscapeLeft',
  'UIInterfaceOrientationLandscapeRight',
];

function sources(dir) {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return sources(path);
    return /\.tsx?$/.test(name) && !/\.test\.tsx?$/.test(name) ? [path] : [];
  });
}

describe('iPad orientation and multitasking', () => {
  it('iPad: all four orientations and resizable; phone: portrait only', () => {
    const output = execFileSync(npx, ['expo', 'config', '--type', 'introspect', '--json'], {
      cwd: root,
      env: process.env,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'inherit'],
    });
    const config = JSON.parse(output);
    const plist = config.ios.infoPlist;
    assert.equal(config.ios.supportsTablet, true);
    assert.equal(plist.UIRequiresFullScreen, false);
    assert.deepEqual([...plist['UISupportedInterfaceOrientations~ipad']].sort(), [...ALL].sort());
    // The phone key: portrait (upside down is ignored on Face ID iPhones), never landscape.
    assert.ok(plist.UISupportedInterfaceOrientations.every((o) => o.startsWith('UIInterfaceOrientationPortrait')));
    assert.equal(config.orientation, 'portrait');
  });

  it('no screen or navigator locks its own orientation', () => {
    const offenders = sources(join(root, 'src')).filter((path) =>
      /\b(orientation|screenOrientation)\s*:\s*['"]/.test(readFileSync(path, 'utf8')),
    );
    assert.deepEqual(offenders, []);
  });
});
