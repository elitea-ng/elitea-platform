// node --test scripts/stamp-version.test.mjs (npm run test:scripts)
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';

import { TAURI_DIR } from './tauri-brand.mjs';
import { stampCargo, stampConf } from './stamp-version.mjs';

test('Cargo.toml takes the full version on the [package] line only', () => {
  const text = '[package]\nname = "x"\nversion = "0.1.0"\n\n[dependencies]\nfoo = { version = "1" }\n[dep]\nversion = "9.9.9"\n';
  const out = stampCargo(text, '1.62.0-dev.7');
  assert.match(out, /^version = "1\.62\.0-dev\.7"$/m);
  assert.match(out, /^version = "9\.9\.9"$/m);
  assert.throws(() => stampCargo('[package]\n', '1.0.0'), /no `version/);
  assert.match(stampCargo(readFileSync(join(TAURI_DIR, 'Cargo.toml'), 'utf8'), '2.0.0'), /^version = "2\.0\.0"$/m);
});

test('tauri.conf.json takes X.Y.Z and the build number', () => {
  const conf = { version: '0.1.0', bundle: { macOS: { hardenedRuntime: true } } };
  const out = stampConf(conf, '1.62.0-rc.1', 412);
  assert.equal(out.version, '1.62.0');
  assert.deepEqual(out.bundle.macOS, { hardenedRuntime: true, bundleVersion: '412' });
  assert.equal(stampConf(conf, '1.62.0').bundle.macOS.bundleVersion, undefined);
  assert.throws(() => stampConf(conf, 'v1.62'), /not X\.Y\.Z/);
  assert.throws(() => stampConf(conf, '1.0.0', 'x'), /not an integer/);
});
