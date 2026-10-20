// EAS Build / Submit / Update wiring (docs/release.md). Structural checks of
// eas.json (the CLI's own validation needs an Expo login) and of the Expo
// config the way EAS evaluates it, linked and unlinked.
// Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const npx = process.platform === 'win32' ? 'npx.cmd' : 'npx';
const eas = JSON.parse(readFileSync(resolve(root, 'eas.json'), 'utf8'));

/** A build profile with its `extends` chain applied (shallow per platform, like EAS). */
function profile(name, seen = []) {
  const own = eas.build[name];
  assert.ok(own, `eas.json has no build profile "${name}"`);
  assert.ok(!seen.includes(name), `eas.json: extends cycle through "${name}"`);
  const parent = own.extends ? profile(own.extends, [...seen, name]) : {};
  return {
    ...parent,
    ...own,
    env: { ...parent.env, ...own.env },
    ios: { ...parent.ios, ...own.ios },
    android: { ...parent.android, ...own.android },
  };
}

function expoConfig(env) {
  const output = execFileSync(npx, ['expo', 'config', '--type', 'public', '--json'], {
    cwd: root,
    env: { ...process.env, EAS_PROJECT_ID: '', EXPO_OWNER: '', ...env },
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  return JSON.parse(output);
}

describe('eas.json', () => {
  it('keeps the build number on EAS servers', () => {
    assert.equal(eas.cli.appVersionSource, 'remote');
  });

  it('has the three release profiles with one EAS Update channel and environment each', () => {
    const expected = {
      development: { distribution: 'internal', channel: 'development', environment: 'development' },
      'development-simulator': { distribution: 'internal', channel: 'development', environment: 'development' },
      preview: { distribution: 'internal', channel: 'preview', environment: 'preview' },
      production: { distribution: 'store', channel: 'production', environment: 'production' },
    };
    for (const [name, want] of Object.entries(expected)) {
      const p = profile(name);
      assert.deepEqual(
        { distribution: p.distribution, channel: p.channel, environment: p.environment },
        want,
        `profile ${name}`,
      );
    }
  });

  it('builds a dev client for devices and one for the simulator', () => {
    assert.equal(profile('development').developmentClient, true);
    assert.notEqual(profile('development').ios.simulator, true);
    assert.equal(profile('development-simulator').ios.simulator, true);
    for (const name of ['preview', 'production']) assert.notEqual(profile(name).developmentClient, true, name);
  });

  it('bumps build numbers for every build that can be installed over another', () => {
    assert.equal(profile('preview').autoIncrement, true);
    assert.equal(profile('production').autoIncrement, true);
    assert.equal(profile('production').android.buildType, 'app-bundle');
    assert.equal(profile('preview').android.buildType, 'apk');
  });

  it('submits the production app to Play internal testing and to TestFlight', () => {
    const submit = eas.submit.production;
    assert.equal(submit.android.track, 'internal');
    assert.equal(submit.android.applicationId, 'ca.zefir.agent');
    assert.equal(submit.ios.bundleIdentifier, 'ca.zefir.agent');
  });

  it('holds no credential or key path (EAS-managed credentials only)', () => {
    const text = JSON.stringify(eas);
    for (const banned of ['serviceAccountKeyPath', 'ascApiKeyPath', 'ascApiKeyId', 'appleId', 'credentialsSource', 'EXPO_TOKEN']) {
      assert.ok(!text.includes(banned), `eas.json must not carry ${banned}`);
    }
  });
});

describe('Expo config for EAS', () => {
  it('unlinked (before eas init): no owner, no project id, EAS Update off', () => {
    const config = expoConfig({});
    assert.equal(config.owner, undefined);
    assert.equal(config.extra.eas?.projectId, undefined);
    assert.deepEqual(config.updates, { enabled: false });
    assert.deepEqual(config.runtimeVersion, { policy: 'fingerprint' });
  });

  it('linked: project id and owner from the environment, update URL for that project', () => {
    const projectId = '0f5e1c1e-1111-4222-8333-444455556666';
    const config = expoConfig({ EAS_PROJECT_ID: projectId, EXPO_OWNER: 'zefir-ca' });
    assert.equal(config.owner, 'zefir-ca');
    assert.equal(config.extra.eas.projectId, projectId);
    assert.equal(config.updates.enabled, true);
    assert.equal(config.updates.url, `https://u.expo.dev/${projectId}`);
    assert.equal(config.updates.fallbackToCacheTimeout, 0);
    // expo-widgets' extension entry survives the merge (EAS signs the extension from it).
    assert.equal(config.extra.eas.build.experimental.ios.appExtensions[0].bundleIdentifier, 'ca.zefir.agent.widgets');
    assert.equal(config.ios.bundleIdentifier, 'ca.zefir.agent');
    assert.equal(config.android.package, 'ca.zefir.agent');
  });

  it('refuses a malformed project id instead of building an unreachable update URL', () => {
    assert.throws(() => expoConfig({ EAS_PROJECT_ID: 'my-project' }));
  });

  it('declares export compliance and the iOS privacy manifest', () => {
    const config = expoConfig({});
    assert.equal(config.ios.infoPlist.ITSAppUsesNonExemptEncryption, false);
    const manifest = config.ios.privacyManifests;
    assert.equal(manifest.NSPrivacyTracking, false);
    const reasons = Object.fromEntries(
      manifest.NSPrivacyAccessedAPITypes.map((t) => [t.NSPrivacyAccessedAPIType.replace('NSPrivacyAccessedAPICategory', ''), t.NSPrivacyAccessedAPITypeReasons]),
    );
    assert.deepEqual(Object.keys(reasons).sort(), ['DiskSpace', 'FileTimestamp', 'SystemBootTime', 'UserDefaults']);
    assert.ok(reasons.UserDefaults.includes('1C8F.1'), 'App Group UserDefaults (widgets) needs 1C8F.1');
  });

  it("declares Expo's update installation id only when EAS Update is on", () => {
    const collected = (env) => expoConfig(env).ios.privacyManifests.NSPrivacyCollectedDataTypes.map((t) => t.NSPrivacyCollectedDataType);
    assert.ok(!collected({}).includes('NSPrivacyCollectedDataTypeDeviceID'));
    assert.ok(collected({ EAS_PROJECT_ID: '0f5e1c1e-1111-4222-8333-444455556666' }).includes('NSPrivacyCollectedDataTypeDeviceID'));
  });

  // Apple does not read static pods' manifests, so the app's manifest must carry every
  // required-reason API any installed native module declares (a new module, or a module
  // update, that adds one fails here instead of in an ITMS-91053 rejection email).
  it('covers every required-reason API that an installed native module declares', () => {
    const declared = new Map(
      expoConfig({}).ios.privacyManifests.NSPrivacyAccessedAPITypes.map((t) => [t.NSPrivacyAccessedAPIType, new Set(t.NSPrivacyAccessedAPITypeReasons)]),
    );
    const missing = [];
    for (const file of podManifests()) {
      const xml = readFileSync(file, 'utf8');
      const entries = xml.split('<key>NSPrivacyAccessedAPIType</key>').slice(1);
      for (const entry of entries) {
        const type = /<string>([^<]+)<\/string>/.exec(entry)?.[1];
        const reasonsBlock = /<key>NSPrivacyAccessedAPITypeReasons<\/key>\s*<array>([\s\S]*?)<\/array>/.exec(entry)?.[1] ?? '';
        for (const [, reason] of reasonsBlock.matchAll(/<string>([^<]+)<\/string>/g)) {
          if (!declared.get(type)?.has(reason)) missing.push(`${file.slice(root.length + 1)}: ${type} ${reason}`);
        }
      }
    }
    assert.deepEqual(missing, []);
  });
});

/** PrivacyInfo.xcprivacy files the iOS build links (native modules' ios/ trees and React Native). */
function podManifests() {
  const found = [];
  const walk = (dir, depth) => {
    let names;
    try {
      names = readdirSync(dir);
    } catch {
      return;
    }
    for (const name of names) {
      const path = resolve(dir, name);
      if (name === 'PrivacyInfo.xcprivacy') found.push(path);
      else if (depth > 0 && !['node_modules', 'Tests', 'example', 'android', 'build'].includes(name) && statSync(path).isDirectory()) walk(path, depth - 1);
    }
  };
  const modules = resolve(root, 'node_modules');
  for (const name of readdirSync(modules)) {
    const pkgs = name.startsWith('@') ? readdirSync(resolve(modules, name)).map((n) => `${name}/${n}`) : [name];
    for (const pkg of pkgs) {
      if (pkg === 'react-native') walk(resolve(modules, pkg), 5);
      else walk(resolve(modules, pkg, 'ios'), 4);
    }
  }
  assert.ok(found.length > 0, 'found no PrivacyInfo.xcprivacy at all: the scan is broken');
  return found;
}

describe('EAS workflow preflight', async () => {
  const { preflightProblems } = await import('./eas-preflight.mjs');
  const linked = { extra: { eas: { projectId: '0f5e1c1e-1111-4222-8333-444455556666' } } };

  it('refuses to build an unlinked project', () => {
    assert.equal(preflightProblems('build', undefined, { config: { extra: {} }, eas }).length, 1);
    assert.deepEqual(preflightProblems('build', undefined, { config: linked, eas }), []);
  });

  it('refuses an iOS submit until the App Store Connect app id is in eas.json', () => {
    assert.equal(preflightProblems('submit', 'ios', { config: linked, eas }).length, 1);
    assert.equal(preflightProblems('submit', 'all', { config: linked, eas }).length, 1);
    assert.deepEqual(preflightProblems('submit', 'android', { config: linked, eas }), []);
    const ready = { ...eas, submit: { production: { ...eas.submit.production, ios: { ascAppId: '6740000000' } } } };
    assert.deepEqual(preflightProblems('submit', 'ios', { config: linked, eas: ready }), []);
  });
});
