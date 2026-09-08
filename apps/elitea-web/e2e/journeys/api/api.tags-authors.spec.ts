/**
 * Tags and authors: the two small read surfaces the agent lists are filtered
 * and attributed by.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's tag cases (the list envelope, the three
 * `entity_coverage` filters, and the tags of a tagged agent) and its author
 * cases (the profile, its counters, and the counter moving when an agent is
 * created). No existing journey touched either route.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE TWO DEFECTS THESE JOURNEYS FOUND
 * ─────────────────────────────────────────────────────────────────────────────
 * The tag WRITE API was a pair of stubs. `POST` decoded the body and echoed it
 * back with 201 and an `id` of 0, touching no table; `DELETE` wrote 204 and
 * returned. So a tag created through the API was in no list and its id
 * addressed nothing, and a tag could not be removed at all — the only way a
 * tag row ever appeared was as a side effect of a version save, and the only
 * way one was ever removed was with SQL.
 *
 * Tags sent when an agent was CREATED were dropped. The same payload persisted
 * its tags through a save and lost them through a create, so a user who tagged
 * an agent in the create form had to open it again and tag it a second time.
 * That is why the tag journeys below assert that a tag is PRESENT after both
 * writes, and read it back from the project's own tag list rather than from
 * the write's echo.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE COUNTERS ARE ASSERTED AS DIRECTIONS, NOT AS NUMBERS
 * ─────────────────────────────────────────────────────────────────────────────
 * The author counters count everything the persona owns across every project
 * it belongs to, and both engines run the whole suite on the same persona at
 * the same time — so "exactly one more than before" is a statement about the
 * other workers, not about this journey. What IS this journey's to state is
 * that creating an agent MOVES the counter, which a counter reading a constant
 * (or the wrong column) fails.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Agents are deleted in a `finally`, and so are the tag ROWS: an
 * agent delete takes the association and leaves the row, so every journey here
 * removes the `autotest_` tags it created through the delete this package
 * makes work.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  agentVersionBody,
  createAgentWithVersion,
  createTag,
  deleteAgent,
  deleteAutotestTags,
  deleteTag,
  readAuthorProfile,
  readCallerIdentity,
  readTags,
  readTagsEnvelope,
  readVersion,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

const named = (tags: readonly { readonly name: string }[]): string[] =>
  tags.map((tag) => tag.name);

/* ── the tag list ─────────────────────────────────────────────────────────── */

test('the project tag list answers a total and rows envelope', async ({ request }) => {
  const tagName = autotestName('tag_envelope');
  const created = await createTag(request, tagName, { color: 'blue' });
  try {
    const envelope = await readTagsEnvelope(request);
    expect(typeof envelope['total'], `the list answered ${JSON.stringify(envelope).slice(0, 200)}`).toBe(
      'number',
    );
    expect(Array.isArray(envelope['rows'])).toBe(true);

    const rows = (envelope['rows'] as readonly Record<string, unknown>[]) ?? [];
    const mine = rows.find((row) => row['name'] === tagName);
    expect(mine, 'the tag this journey created is not in the project list').toBeDefined();
    // Every row carries the three keys the tag control binds to.
    expect(typeof mine?.['id']).not.toBe('undefined');
    expect(mine?.['name']).toBe(tagName);
    expect(mine?.['data']).toEqual({ color: 'blue' });
  } finally {
    await deleteTag(request, created.id);
  }
});

/**
 * The write API stores a row and removes it again.
 *
 * Both halves were stubs, and each hid the other: a create that stored
 * nothing left nothing for the delete to fail on.
 */
test('a tag can be created and then deleted', async ({ request }) => {
  const tagName = autotestName('tag_write');
  const created = await createTag(request, tagName, { color: 'green' });

  expect(created.id, 'the create answered an id of 0, which addresses no row').not.toBe('0');
  expect(created.id).not.toBe('');
  expect(named(await readTags(request))).toContain(tagName);

  // Idempotent on the NAME: one name is one row per project, shared by every
  // version that carries it, so a second create must answer the same row.
  const again = await createTag(request, tagName);
  expect(again.id, 'a second create of the same name made a second row').toBe(created.id);

  expect(await deleteTag(request, created.id)).toBe(204);
  expect(
    named(await readTags(request)),
    'the delete answered success and the tag is still in the list',
  ).not.toContain(tagName);

  // …and deleting it again says so, rather than answering the same 204 a real
  // delete does.
  expect(await deleteTag(request, created.id)).toBe(404);
});

/* ── the entity_coverage filters ──────────────────────────────────────────── */

/**
 * One project, one tagged agent, one tagged pipeline and one tag on nothing:
 * the three narrowing values must each answer a different set.
 *
 * The assertions are about THIS journey's own tag names only. Every other
 * worker's tags are in the same project and in the same answer, so a journey
 * that counted rows would be measuring the suite.
 */
test('the tag list can be narrowed to agents, to pipelines, and to everything', async ({
  request,
}) => {
  const agentTag = autotestName('tag_on_agent');
  const pipelineTag = autotestName('tag_on_pipeline');
  const looseTag = autotestName('tag_on_nothing');

  const agent = await createAgentWithVersion(request, autotestName('coverage_agent'), {
    agentType: 'openai',
    tags: [{ name: agentTag }],
  });
  const pipeline = await createAgentWithVersion(request, autotestName('coverage_pipeline'), {
    agentType: 'pipeline',
    instructions: 'nodes: []\nedges: []',
    tags: [{ name: pipelineTag }],
  });
  const loose = await createTag(request, looseTag);
  try {
    const application = named(await readTags(request, { coverage: 'application' }));
    expect(application, 'the agent’s tag is missing from the agent coverage').toContain(agentTag);
    expect(application, 'a pipeline’s tag is answered as an agent tag').not.toContain(pipelineTag);
    expect(application, 'a tag on nothing is answered as an agent tag').not.toContain(looseTag);

    const pipelines = named(await readTags(request, { coverage: 'pipeline' }));
    expect(pipelines, 'the pipeline’s tag is missing from the pipeline coverage').toContain(
      pipelineTag,
    );
    expect(pipelines, 'an agent’s tag is answered as a pipeline tag').not.toContain(agentTag);

    // `all` is every tag row the project holds, which is the only coverage a
    // tag nothing carries yet appears in.
    const everything = named(await readTags(request, { coverage: 'all' }));
    for (const tag of [agentTag, pipelineTag, looseTag]) {
      expect(everything, `${tag} is missing from the whole-project list`).toContain(tag);
    }

    // The skill coverage is a third set, and neither of the two above is in
    // it: these tags are on applications.
    const skills = named(await readTags(request, { coverage: 'skill' }));
    expect(skills).not.toContain(agentTag);
    expect(skills).not.toContain(pipelineTag);

    // A coverage nobody serves is REFUSED. Answering an empty list — which is
    // what the legacy platform does — reads exactly like a project with no
    // tags, so a misspelt filter and an empty project would be one answer.
    const refused = await request.get(
      `${API_BASE}/elitea_core/tags/prompt_lib/${DEFAULT_PROJECT_ID}`,
      { params: { entity_coverage: 'agents' } },
    );
    expect(refused.status(), await refused.text()).toBe(400);
  } finally {
    await deleteAgent(request, agent.id);
    await deleteAgent(request, pipeline.id);
    await deleteTag(request, loose.id);
    await deleteAutotestTags(request, [agentTag, pipelineTag, looseTag]);
  }
});

/* ── the tags an agent carries ────────────────────────────────────────────── */

/**
 * Tags reach the project's tag list from BOTH writes — the create and the
 * save. The create was the one that dropped them.
 */
test('the tags given to an agent are stored, at creation and at save', async ({ request }) => {
  const atCreation = autotestName('tag_at_creation');
  const atSave = autotestName('tag_at_save');
  const agent = await createAgentWithVersion(request, autotestName('tagged_agent'), {
    agentType: 'openai',
    tags: [{ name: atCreation, data: { color: 'blue' } }],
  });
  try {
    // The ROW, through the editor's own reload — not the create's echo.
    const created = await readVersion(request, agent.id, agent.versionId);
    expect(
      named(created.tags),
      'the tags sent at creation were accepted with a 201 and never stored',
    ).toContain(atCreation);

    // …and the project's tag list, which is a different table and a
    // different read, carries it with the colour it was given.
    const listed = await readTags(request);
    expect(named(listed)).toContain(atCreation);
    expect(listed.find((tag) => tag.name === atCreation)?.data).toEqual({ color: 'blue' });

    // The SAVE half of the same contract. It replaces the set exactly: the
    // tag that is no longer sent leaves the version, and the new one
    // arrives — both halves must be true, or a save cannot remove a tag.
    const response = await request.put(
      `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}/${agent.versionId}`,
      { data: agentVersionBody({ name: 'base', tags: [{ name: atSave }] }) },
    );
    expect(response.status(), `the save answered ${(await response.text()).slice(0, 200)}`).toBe(201);

    const saved = await readVersion(request, agent.id, agent.versionId);
    expect(named(saved.tags)).toContain(atSave);
    expect(named(saved.tags), 'the tag the save dropped is still on the version').not.toContain(
      atCreation,
    );
    expect(named(await readTags(request))).toContain(atSave);
  } finally {
    await deleteAgent(request, agent.id);
    await deleteAutotestTags(request, [atCreation, atSave]);
  }
});

/* ── the author profile ───────────────────────────────────────────────────── */

test('an author profile answers identity plus integer counters', async ({ request }) => {
  const caller = await readCallerIdentity(request);
  const profile = await readAuthorProfile(request, caller.id);

  expect(String(profile['id'] ?? ''), 'the profile answers another author').toBe(caller.id);
  expect(profile['email']).toBe(caller.email);
  expect(profile['name']).toBeTruthy();
  // The optional identity fields are present as keys, even when empty: the
  // profile page binds to them.
  for (const key of ['avatar', 'title', 'description']) {
    expect(Object.hasOwn(profile, key), `the profile carries no ${key}`).toBe(true);
  }
  // Every counter is a number. A missing counter renders as "undefined" on
  // the profile page; a string one breaks the arithmetic beside it.
  for (const key of [
    'total_conversations',
    'public_conversations',
    'public_applications',
    'total_applications',
    'public_pipelines',
    'total_pipelines',
    'total_toolkits',
    'rewards',
  ]) {
    expect(typeof profile[key], `${key} is ${JSON.stringify(profile[key])}`).toBe('number');
  }
});

test('the author counters reflect the agents and pipelines the author owns', async ({
  request,
}) => {
  const caller = await readCallerIdentity(request);
  const agent = await createAgentWithVersion(request, autotestName('counted_agent'), {
    agentType: 'openai',
  });
  const pipeline = await createAgentWithVersion(request, autotestName('counted_pipeline'), {
    agentType: 'pipeline',
    instructions: 'nodes: []\nedges: []',
  });
  try {
    const profile = await readAuthorProfile(request, caller.id);
    expect(
      profile['total_applications'] as number,
      'the caller owns an agent and the counter says none — it used to count by the owning PROJECT',
    ).toBeGreaterThan(0);
    expect(
      profile['total_pipelines'] as number,
      'the caller owns a pipeline and the pipeline counter says none',
    ).toBeGreaterThan(0);
  } finally {
    await deleteAgent(request, agent.id);
    await deleteAgent(request, pipeline.id);
  }
});

/**
 * Creating an agent moves the author's agent counter.
 *
 * The delta is asserted as a DIRECTION and not as "exactly one": the suite
 * runs on one persona in two engines at once, so any other worker's agent is
 * in the same number.
 *
 * THE MEASUREMENT IS REPEATED, and that is not a retry for luck. This counter
 * belongs to a persona every worker shares, and the other workers DELETE their
 * agents as well as create them: a delete landing inside this window subtracts
 * exactly what the create added, and the counter comes back the same number it
 * started at. Measured on webkit — baseline 1, agent created, still 1 fifteen
 * seconds later. Each attempt below is a whole measurement of its own — a
 * fresh baseline, a fresh agent, its own poll — so passing needs one window in
 * which nobody else deleted, and failing means the counter really did not
 * follow three consecutive creates.
 */
test('creating an agent increments the author agent counter', async ({ request }) => {
  test.setTimeout(120_000);
  const caller = await readCallerIdentity(request);
  const attempts = 3;
  const seen: number[] = [];

  for (let attempt = 1; attempt <= attempts; attempt += 1) {
    const before = (await readAuthorProfile(request, caller.id))['total_applications'] as number;
    expect(typeof before).toBe('number');

    const agent = await createAgentWithVersion(request, autotestName(`counter_increment_${attempt}`), {
      agentType: 'openai',
    });
    let moved = false;
    try {
      // The poll is the harness's own, intervals included. It THROWS on
      // timeout, and that throw is caught here rather than allowed to end the
      // test: an attempt that did not see the move must be able to make way
      // for the next one. A caught assertion is not a recorded failure —
      // `expect.soft` is the form that records one.
      await expect
        .poll(
          async () => {
            const now = (await readAuthorProfile(request, caller.id))['total_applications'] as number;
            seen.push(now);
            return now;
          },
          { message: 'creating an agent did not move the author’s agent counter', timeout: 15_000 },
        )
        .toBeGreaterThan(before);
      moved = true;
    } catch {
      moved = false;
    } finally {
      await deleteAgent(request, agent.id);
    }
    if (moved) return;
  }

  expect(
    seen.length,
    'the counter was never read — the author profile answered nothing to measure',
  ).toBeGreaterThan(0);
  throw new Error(
    `creating an agent did not move the author’s agent counter in ${String(attempts)} independent ` +
      `measurements; the counter read ${JSON.stringify(seen)}`,
  );
});
