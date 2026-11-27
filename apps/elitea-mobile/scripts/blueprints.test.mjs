// The app's blueprint gallery data is the repository's blueprints, as the
// builder reads them (issue 3137). A blueprint edit without a regenerated
// file fails here.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import { appBlueprints, GENERATED } from './blueprints.mjs';

test('the committed gallery data matches blueprints/', async () => {
  assert.equal(readFileSync(GENERATED, 'utf8'), await appBlueprints(), 'run: node scripts/blueprints.mjs');
});

test('every blueprint has a brief to craft from', async () => {
  const text = await appBlueprints();
  const data = JSON.parse(text.slice(text.indexOf('= {') + 2, text.lastIndexOf(';')));
  assert.ok(data.agents.length > 0 && data.teams.length > 0);
  for (const item of [...data.agents, ...data.teams]) {
    assert.ok(item.brief.length >= 40, `${item.slug} has no brief`);
  }
});
