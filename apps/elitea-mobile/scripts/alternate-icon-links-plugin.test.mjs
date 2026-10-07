// Android: switching to the "Zefir Night" icon disables .MainActivity and enables its
// activity-alias. plugins/withAlternateIconLinks.js copies MainActivity's link filters onto the
// alias, so connect links and the sign-in redirect still reach the app with that icon on.
// Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, resolve } from 'node:path';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const require = createRequire(import.meta.url);
const { copyLinkFiltersToAliases } = require('../plugins/withAlternateIconLinks.js');

const launcher = {
  action: [{ $: { 'android:name': 'android.intent.action.MAIN' } }],
  category: [{ $: { 'android:name': 'android.intent.category.LAUNCHER' } }],
};
const links = {
  action: [{ $: { 'android:name': 'android.intent.action.VIEW' } }],
  category: [{ $: { 'android:name': 'android.intent.category.DEFAULT' } }, { $: { 'android:name': 'android.intent.category.BROWSABLE' } }],
  data: [{ $: { 'android:scheme': 'agentzefir' } }, { $: { 'android:scheme': 'ca.zefir.agent' } }],
};

function manifest() {
  return {
    manifest: {
      application: [
        {
          activity: [{ $: { 'android:name': '.MainActivity' }, 'intent-filter': [launcher, links] }],
          'activity-alias': [
            { $: { 'android:name': '.MainActivityZefirNight', 'android:targetActivity': '.MainActivity' }, 'intent-filter': [launcher] },
            { $: { 'android:name': '.Other', 'android:targetActivity': '.SomethingElse' }, 'intent-filter': [launcher] },
          ],
        },
      ],
    },
  };
}

describe('alternate icon alias keeps the app links (withAlternateIconLinks)', () => {
  it('copies the VIEW/BROWSABLE scheme filters onto the alias of MainActivity, not the launcher twice', () => {
    const out = copyLinkFiltersToAliases(manifest());
    const [alias, other] = out.manifest.application[0]['activity-alias'];
    assert.deepEqual(alias['intent-filter'], [launcher, links]);
    assert.deepEqual(other['intent-filter'], [launcher]);
  });

  it('is idempotent (prebuild without --clean runs it again)', () => {
    const once = copyLinkFiltersToAliases(manifest());
    const twice = copyLinkFiltersToAliases(once);
    assert.equal(twice.manifest.application[0]['activity-alias'][0]['intent-filter'].length, 2);
  });

  it('is registered before expo-alternate-app-icons, so it runs after the alias exists', () => {
    const config = readFileSync(resolve(root, 'app.config.ts'), 'utf8');
    const mine = config.indexOf("'./plugins/withAlternateIconLinks'");
    const icons = config.indexOf("'expo-alternate-app-icons'");
    assert.ok(mine > 0 && icons > mine);
  });
});
