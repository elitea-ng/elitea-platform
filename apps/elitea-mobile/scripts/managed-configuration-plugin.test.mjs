// plugins/withManagedConfiguration.js (ADR-0052 W1b): Android declares the managed keys an
// EMM may set, and they are exactly the keys src/security/managedConfig.ts parses.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const require = createRequire(import.meta.url);
const plugin = require(join(root, 'plugins', 'withManagedConfiguration.js'));

test('declares every key the app parses, and no other', () => {
  const source = readFileSync(join(root, 'src', 'security', 'managedConfig.ts'), 'utf8');
  const schema = source.slice(source.indexOf('const Managed = z.looseObject({'));
  const parsed = [...schema.slice(0, schema.indexOf('});')).matchAll(/^\s+(\w+): field\(/gm)].map((m) => m[1]);
  assert.ok(parsed.length >= 5, 'the parser keys were not found');
  assert.deepEqual(plugin.RESTRICTIONS.map((r) => r.key).sort(), [...parsed].sort());
});

test('types match the parser: string, bool, integer', () => {
  const types = Object.fromEntries(plugin.RESTRICTIONS.map((r) => [r.key, r.type]));
  assert.deepEqual(types, {
    server_url: 'string',
    lock_server: 'bool',
    require_device_lock: 'bool',
    idle_lock_seconds: 'integer',
    allow_screenshots: 'bool',
  });
});

test('writes a restrictions resource with escaped, product-free titles', () => {
  const xml = plugin.restrictionsXml();
  assert.match(xml, /^<\?xml version="1.0" encoding="utf-8"\?>/);
  assert.match(xml, /<restrictions xmlns:android="http:\/\/schemas.android.com\/apk\/res\/android">/);
  for (const r of plugin.RESTRICTIONS) {
    assert.match(xml, new RegExp(`android:key="${r.key}"\\s+android:restrictionType="${r.type}"`));
  }
  assert.doesNotMatch(xml, /elitea/i);
  const escaped = plugin.restrictionsXml([{ key: 'k', type: 'string', title: 'a & <b>', description: '"q"' }]);
  assert.match(escaped, /android:title="a &amp; &lt;b&gt;"/);
  assert.match(escaped, /android:description="&quot;q&quot;"/);
});

test('names the resource on the application, once', () => {
  const manifest = { manifest: { application: [{ $: { 'android:name': '.MainApplication' } }] } };
  plugin.addRestrictionsMetaData(manifest);
  plugin.addRestrictionsMetaData(manifest);
  const meta = manifest.manifest.application[0]['meta-data'];
  assert.equal(meta.length, 1);
  assert.deepEqual(meta[0].$, { 'android:name': plugin.META_NAME, 'android:resource': '@xml/app_restrictions' });
});

test('app.config.ts registers the plugin', () => {
  const npx = process.platform === 'win32' ? 'npx.cmd' : 'npx';
  const out = execFileSync(npx, ['expo', 'config', '--type', 'prebuild', '--json'], {
    cwd: root,
    env: process.env,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'inherit'],
  });
  const names = (JSON.parse(out).plugins ?? []).map((p) => (Array.isArray(p) ? p[0] : p));
  assert.ok(
    names.some((name) => typeof name === 'string' && name.includes('withManagedConfiguration')),
    JSON.stringify(names),
  );
});
