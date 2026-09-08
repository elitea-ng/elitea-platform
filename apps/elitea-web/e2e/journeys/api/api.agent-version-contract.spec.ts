/**
 * The agent-version WRITE contract: what a version save accepts, what it
 * refuses, and what it must not destroy on the way through.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's application write cases APP-01 to APP-13 and APP-15,
 * one journey each, named by their use case. They are API-only by nature: every
 * one is a statement about a request body and the row it produces, and half of
 * them are refusals no screen can author — the agent editor cannot send a
 * version whose `application_id` belongs to a different agent.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE ASSERTIONS READ THE ROW BACK
 * ─────────────────────────────────────────────────────────────────────────────
 * Every write on this surface answers 201 with an ECHO the handler builds in
 * Go, and this endpoint family has a documented history of echoes that were
 * true about the response and false about the database: variables accepted and
 * discarded, tags accepted and discarded, a pipeline graph accepted and
 * discarded. So a journey here asserts on `readVersion` — the editor's own
 * reload — and uses the echo only to say the request was not refused.
 *
 * The meta round trip goes further and reads the version through the EXPANDED
 * projection as well, behind the project's `X-SECRET`. That is the read the
 * runtime makes, and `meta.step_limit` is one of the gates the runtime admits a
 * stored agent on: an agent that lost it is an agent that no longer runs, so
 * the two projections have to agree.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every journey creates its own `autotest_*` agents through the API
 * and deletes them in a `finally`, and no journey reads another's rows or
 * depends on the order they run in, so both engines can run the file at once.
 *
 * That once had an exception: the tag the meta round trip saves beside its
 * variables. Deleting the agent removes the ASSOCIATION and leaves the `tags`
 * row, and `DELETE /elitea_core/tags/prompt_lib/{project}/{tag}` answered 204
 * and removed nothing, so the row had to stay. The delete works now
 * (`api.tags-authors.spec.ts` is where it is pinned), so that journey removes
 * its own tag and this file leaves no row at all.
 */
import { test, expect, type APIRequestContext } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  agentVersionBody,
  createAgentWithVersion,
  deleteAgent,
  deleteAutotestTags,
  readTags,
  readVersion,
  readVersionExpanded,
  resolveProjectSecretHeader,
  type AgentVersionInput,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

const applicationURL = (applicationId: string): string =>
  `${API_BASE}/elitea_core/application/prompt_lib/${DEFAULT_PROJECT_ID}/${applicationId}`;

const versionsURL = (applicationId: string): string =>
  `${API_BASE}/elitea_core/versions/prompt_lib/${DEFAULT_PROJECT_ID}/${applicationId}`;

const versionURL = (applicationId: string, versionId: string): string =>
  `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/${applicationId}/${versionId}`;

/**
 * `PUT /application/...` with a nested `version` stub — the shape every id
 * guard is written against.
 *
 * The stub is passed through `agentVersionBody`, so a key the caller does not
 * name is ABSENT from the request rather than sent as null. Two of the guards
 * below are specifically about an absent key, and a builder that always emitted
 * every field could not express them.
 */
async function putApplicationVersion(
  request: APIRequestContext,
  applicationId: string,
  version: AgentVersionInput,
): Promise<{ readonly status: number; readonly error: string; readonly body: Record<string, unknown> }> {
  const response = await request.put(applicationURL(applicationId), {
    data: { version: agentVersionBody(version) },
  });
  let body: Record<string, unknown> = {};
  try {
    body = (await response.json()) as Record<string, unknown>;
  } catch {
    body = {};
  }
  return { status: response.status(), error: String(body['error'] ?? ''), body };
}

/* ── the id guards on the agent endpoint ──────────────────────────────────── */

test('a version whose id belongs to a different agent is refused', async ({ request }) => {
  const first = await createAgentWithVersion(request, autotestName('idguard_a'), {});
  const second = await createAgentWithVersion(request, autotestName('idguard_b'), {});
  try {
    const refused = await putApplicationVersion(request, first.id, {
      id: second.versionId,
      applicationId: first.id,
      name: autotestName('stolen'),
    });
    expect(refused.status, `the save answered ${refused.status}: ${JSON.stringify(refused.body)}`).toBe(400);
    expect(refused.error).toContain('version id mismatch');

    // The other agent's version is untouched. A guard that refused the request
    // AFTER writing would satisfy the status assertion alone.
    const untouched = await readVersion(request, second.id, second.versionId);
    expect(untouched.name).toBe('base');
  } finally {
    await deleteAgent(request, first.id);
    await deleteAgent(request, second.id);
  }
});

test('a version carrying another agent id AND another version id is refused', async ({ request }) => {
  const first = await createAgentWithVersion(request, autotestName('bothguard_a'), {});
  const second = await createAgentWithVersion(request, autotestName('bothguard_b'), {});
  try {
    const refused = await putApplicationVersion(request, first.id, {
      id: second.versionId,
      applicationId: second.id,
      name: autotestName('stolen'),
    });
    expect(refused.status).toBe(400);
    // The agent id is checked FIRST, and says so: a caller that sent both
    // wrong needs to know which one the server looked at.
    expect(refused.error).toContain('application_id mismatch');
  } finally {
    await deleteAgent(request, first.id);
    await deleteAgent(request, second.id);
  }
});

test('a version carrying agent id zero is refused', async ({ request }) => {
  const agent = await createAgentWithVersion(request, autotestName('zeroapp'), {});
  try {
    const refused = await putApplicationVersion(request, agent.id, {
      id: agent.versionId,
      applicationId: '0',
      name: autotestName('renamed'),
    });
    expect(refused.status).toBe(400);
    expect(refused.error).toContain('application_id mismatch');
  } finally {
    await deleteAgent(request, agent.id);
  }
});

test('a version carrying version id zero is refused as not found', async ({ request }) => {
  const agent = await createAgentWithVersion(request, autotestName('zeroversion'), {});
  try {
    const refused = await putApplicationVersion(request, agent.id, {
      id: '0',
      applicationId: agent.id,
      name: autotestName('renamed'),
    });
    expect(refused.status).toBe(400);
    expect(refused.error).toContain('version id mismatch');
  } finally {
    await deleteAgent(request, agent.id);
  }
});

test('a version with no agent id is refused', async ({ request }) => {
  const agent = await createAgentWithVersion(request, autotestName('noappid'), {});
  try {
    // No `applicationId` — so the key is absent from the body, which is a
    // different request from one that sends it empty.
    const refused = await putApplicationVersion(request, agent.id, {
      id: agent.versionId,
      name: autotestName('renamed'),
    });
    expect(refused.status).toBe(400);
    expect(refused.error).toContain('application_id is required');
  } finally {
    await deleteAgent(request, agent.id);
  }
});

test('a version with no id is refused', async ({ request }) => {
  const agent = await createAgentWithVersion(request, autotestName('noid'), {});
  try {
    const refused = await putApplicationVersion(request, agent.id, {
      applicationId: agent.id,
      name: autotestName('renamed'),
    });
    expect(refused.status).toBe(400);
    expect(refused.error).toContain('version id is required');
  } finally {
    await deleteAgent(request, agent.id);
  }
});

test('the protected base version cannot be renamed', async ({ request }) => {
  const agent = await createAgentWithVersion(request, autotestName('protected'), {});
  try {
    const refused = await putApplicationVersion(request, agent.id, {
      id: agent.versionId,
      applicationId: agent.id,
      name: autotestName('not_base'),
    });
    expect(refused.status).toBe(400);
    expect(refused.error).toContain('base/latest');

    const stored = await readVersion(request, agent.id, agent.versionId);
    expect(stored.name, 'the refusal must leave the protected name in place').toBe('base');
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/* ── the writes that must land ────────────────────────────────────────────── */

test('a version created with an empty name is refused', async ({ request }) => {
  const agent = await createAgentWithVersion(request, autotestName('emptyname'), {});
  try {
    const response = await request.post(versionsURL(agent.id), {
      data: agentVersionBody({ name: '', instructions: 'Follow the brief.' }),
    });
    expect(response.status()).toBe(400);
    expect(String(((await response.json()) as { error?: string }).error ?? '')).toContain(
      'version name is required',
    );

    // …and no second version was made. The refusal is only worth anything if
    // the row it refused does not exist.
    const listed = await request.get(versionsURL(agent.id));
    const versions = ((await listed.json()) as { items?: readonly { name?: string }[] }).items ?? [];
    expect(versions.map((version) => version.name)).toEqual(['base']);
  } finally {
    await deleteAgent(request, agent.id);
  }
});

test('a second version can be created and then renamed', async ({ request }) => {
  const agent = await createAgentWithVersion(request, autotestName('secondversion'), {});
  const secondName = autotestName('v2');
  const renamed = autotestName('v2_renamed');
  try {
    const created = await request.post(versionsURL(agent.id), {
      data: agentVersionBody({
        name: secondName,
        agentType: 'openai',
        instructions: 'The second version follows a different brief.',
      }),
    });
    expect(created.status(), `create answered ${(await created.text()).slice(0, 300)}`).toBe(201);
    const secondVersionId = String(((await created.json()) as { id?: string }).id ?? '');
    expect(secondVersionId, 'the version create answered no id').not.toBe('');

    // A rename is a version save through the AGENT endpoint, which is the
    // only one that carries a nested version stub.
    const saved = await putApplicationVersion(request, agent.id, {
      id: secondVersionId,
      applicationId: agent.id,
      name: renamed,
    });
    expect(saved.status, `rename answered ${JSON.stringify(saved.body)}`).toBe(201);

    const stored = await readVersion(request, agent.id, secondVersionId);
    expect(stored.name).toBe(renamed);
    expect(
      stored.instructions,
      'a rename must not touch the version body it renames',
    ).toBe('The second version follows a different brief.');

    // The base version is still there and still called base: the rename
    // addressed one row, not the agent.
    const base = await readVersion(request, agent.id, agent.versionId);
    expect(base.name).toBe('base');
  } finally {
    await deleteAgent(request, agent.id);
  }
});

test('editing the instructions through the agent endpoint persists them', async ({ request }) => {
  const agent = await createAgentWithVersion(request, autotestName('instructions'), {
    instructions: 'The original brief.',
  });
  try {
    const saved = await putApplicationVersion(request, agent.id, {
      id: agent.versionId,
      applicationId: agent.id,
      instructions: 'The revised brief.',
    });
    expect(saved.status, `save answered ${JSON.stringify(saved.body)}`).toBe(201);
    const echo = saved.body['version_details'] as { instructions?: string } | undefined;
    expect(echo?.instructions).toBe('The revised brief.');

    const stored = await readVersion(request, agent.id, agent.versionId);
    expect(stored.instructions).toBe('The revised brief.');
    expect(stored.name, 'an instructions edit must not rename the version').toBe('base');
  } finally {
    await deleteAgent(request, agent.id);
  }
});

test('a new version is given the default step limit', async ({ request }) => {
  const agent = await createAgentWithVersion(request, autotestName('steplimit'), {});
  try {
    // No `meta` at all — the point of the case is that the server supplies
    // the gate rather than trusting the client to.
    const created = await request.post(versionsURL(agent.id), {
      data: agentVersionBody({ name: autotestName('v2'), agentType: 'openai' }),
    });
    expect(created.status()).toBe(201);
    const body = (await created.json()) as { id?: string; meta?: Record<string, unknown> };
    const stepLimit = body.meta?.['step_limit'];
    expect(
      typeof stepLimit,
      `step_limit is ${JSON.stringify(stepLimit)} — the Rust runtime admits an agent on this ` +
        'number, and a missing or non-numeric one makes the agent unrunnable',
    ).toBe('number');
    expect(stepLimit as number).toBeGreaterThan(0);

    // The row says the same thing, not just the echo.
    const stored = await readVersion(request, agent.id, String(body.id ?? ''));
    expect(stored.meta['step_limit']).toBe(stepLimit);
  } finally {
    await deleteAgent(request, agent.id);
  }
});

test('an agent naming a model project that does not exist is refused at creation', async ({
  request,
}) => {
  const response = await request.post(
    `${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}`,
    {
      data: {
        name: autotestName('badmodelproject'),
        description: `${AUTOTEST_PREFIX}model project refusal`,
        type: 'agent',
        versions: [
          agentVersionBody({
            name: 'base',
            agentType: 'openai',
            // `model_project_id` names the PROJECT a model is published in.
            // This one is not a project reference at all.
            model: { modelName: 'gpt-4o-mini', modelProjectId: `${AUTOTEST_PREFIX}no_such_project` },
          }),
        ],
      },
    },
  );
  // A created agent would have to be cleaned up; a refused one must not exist,
  // so the absence of a cleanup block here is part of the assertion.
  expect(
    response.status(),
    `the create answered ${response.status()}: ${(await response.text()).slice(0, 300)}`,
  ).toBe(400);
  expect(String(((await response.json()) as { error?: string }).error ?? '')).toContain(
    'model_project_id',
  );
});


/* ── an agent attached as another agent's toolkit ─────────────────────────── */

/**
 * Attach or detach the agent named in the URL as a tool of the version named
 * in the body. The URL names the CHILD and the body names the PARENT, which
 * is the opposite of what the path reads like, so it is written once here.
 */
async function setAgentRelation(
  request: APIRequestContext,
  child: { readonly id: string; readonly versionId: string },
  parentVersionId: string,
  parentApplicationId: string,
  hasRelation: boolean,
) {
  return request.patch(
    `${API_BASE}/elitea_core/application_relation/prompt_lib/${DEFAULT_PROJECT_ID}` +
      `/${child.id}/${child.versionId}`,
    {
      data: {
        application_id: Number(parentApplicationId),
        version_id: Number(parentVersionId),
        has_relation: hasRelation,
      },
    },
  );
}

/** The sub-agent tool rows on one parent version. */
async function subAgentToolsOf(
  request: APIRequestContext,
  parentApplicationId: string,
  parentVersionId: string,
) {
  const stored = await readVersion(request, parentApplicationId, parentVersionId);
  return stored.tools.filter((tool) => tool['type'] === 'application');
}

test('attaching an agent to another agent records the caller as the toolkit author', async ({
  request,
}) => {
  const parent = await createAgentWithVersion(request, autotestName('parent'), {});
  const childName = autotestName('child');
  const child = await createAgentWithVersion(request, childName, {});
  try {
    const attached = await setAgentRelation(request, child, parent.versionId, parent.id, true);
    expect(
      attached.status(),
      `the attach answered ${(await attached.text()).slice(0, 300)}`,
    ).toBe(201);

    const tools = await subAgentToolsOf(request, parent.id, parent.versionId);
    expect(tools, 'the attach answered 201 and wrote no tool row').toHaveLength(1);
    expect(tools[0]?.['name']).toBe(childName);

    // WHO the row says made it. The author is what the moderation and
    // publish paths attribute a nested agent by, and it is read from the
    // toolkit's own detail route rather than from the attach echo — the echo
    // carries no author at all.
    const toolId = String(tools[0]?.['tool_id'] ?? tools[0]?.['id'] ?? '');
    expect(toolId, 'the version read served a tool with no id').not.toBe('');
    const detail = await request.get(
      `${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${toolId}`,
    );
    expect(detail.status()).toBe(200);
    const toolkit = (await detail.json()) as {
      author_id?: unknown;
      author?: { id?: string; email?: string };
    };
    const me = await request.get(`${API_BASE}/social/author/`);
    expect(me.status()).toBe(200);
    const caller = (await me.json()) as { id?: string; email?: string };
    expect(String(toolkit.author_id ?? '')).toBe(String(caller.id ?? ''));
    expect(toolkit.author?.email).toBe(caller.email);
  } finally {
    await deleteAgent(request, parent.id);
    await deleteAgent(request, child.id);
  }
});

test('attaching the same agent twice is refused and leaves one toolkit', async ({ request }) => {
  const parent = await createAgentWithVersion(request, autotestName('dupparent'), {});
  const child = await createAgentWithVersion(request, autotestName('dupchild'), {});
  try {
    const first = await setAgentRelation(request, child, parent.versionId, parent.id, true);
    expect(first.status()).toBe(201);

    const second = await setAgentRelation(request, child, parent.versionId, parent.id, true);
    expect(
      second.status(),
      `the duplicate attach answered ${(await second.text()).slice(0, 300)}`,
    ).toBe(400);
    expect(String(((await second.json()) as { error?: string }).error ?? '')).toContain(
      'already exists',
    );

    // The refusal's whole point: the parent must still hold exactly one, not
    // two and not none. A duplicate that wrote a second row would double the
    // child in every reader, including the runtime's tool snapshot.
    const tools = await subAgentToolsOf(request, parent.id, parent.versionId);
    expect(tools).toHaveLength(1);
  } finally {
    await deleteAgent(request, parent.id);
    await deleteAgent(request, child.id);
  }
});

/* ── the meta round trip ──────────────────────────────────────────────────── */

/**
 * A save of the version's variables must not destroy the rest of `meta`.
 *
 * `meta` is a bag of keys owned by different features — `step_limit`,
 * `icon_meta`, `internal_tools`, `variables`, and the three fork-provenance
 * keys — and the request below is the ordinary one an editor makes when a user
 * changes a variable: `variables`, `tags`, and no `meta` at all. The server
 * folds that into a one-key `meta`, so while the write replaced the column
 * instead of merging into it, this request erased everything else and answered
 * 201 while doing it.
 *
 * `step_limit` is the assertion that matters most: it is one of the four gates
 * the Rust runtime admits a stored agent on, so an agent could be edited into
 * being unrunnable by a save that reported success.
 */
test("saving a version's variables keeps the rest of its meta", async ({ request }) => {
  const tagName = autotestName('tag');
  const agent = await createAgentWithVersion(request, autotestName('metaroundtrip'), {
    instructions: 'Follow the brief.',
    variables: [{ name: 'region', value: 'emea' }],
    meta: {
      step_limit: 7,
      icon_meta: { icon: 'bolt' },
      internal_tools: ['canvas'],
      parent_entity_id: 41,
      parent_project_id: 2,
      parent_author_id: 3,
    },
  });
  try {
    const seeded = await readVersion(request, agent.id, agent.versionId);
    expect(seeded.meta['step_limit'], 'the fixture did not seed the gate it is about').toBe(7);

    const saved = await request.put(versionURL(agent.id, agent.versionId), {
      data: agentVersionBody({
        name: 'base',
        variables: [{ name: 'region', value: 'apac' }],
        tags: [{ name: tagName }],
      }),
    });
    expect(saved.status(), `the save answered ${(await saved.text()).slice(0, 300)}`).toBe(201);

    const stored = await readVersion(request, agent.id, agent.versionId);
    expect(
      stored.meta['step_limit'],
      'a variables edit destroyed the number the runtime admits this agent on',
    ).toBe(7);
    expect(stored.meta['icon_meta'], 'the agent lost its icon to a variables edit').toEqual({
      icon: 'bolt',
    });
    expect(stored.meta['internal_tools']).toEqual(['canvas']);
    for (const key of ['parent_entity_id', 'parent_project_id', 'parent_author_id']) {
      expect(
        stored.meta[key],
        `a forked agent lost its ${key} provenance to a variables edit`,
      ).toBeDefined();
    }

    // …and the edit itself landed, in both stores the two read paths use.
    expect(stored.meta['variables']).toEqual([{ name: 'region', value: 'apac' }]);
    expect(stored.variables).toEqual([{ name: 'region', value: 'apac' }]);

    // The tag sent beside the variables is a real association row, not an
    // echo: the project's own tag list is a different table and a different
    // read.
    const tags = await readTags(request);
    expect(tags.map((tag) => tag.name)).toContain(tagName);
    expect(stored.tags.map((tag) => tag.name)).toContain(tagName);

    // THE RUNTIME'S OWN PROJECTION. `readVersion` above is the editor's
    // reload; this is the read the worker makes, behind the project's
    // X-SECRET. The two are built by different code over the same row, and
    // the gate has to survive in both or the agent stops running while the
    // editor still shows it as healthy.
    const secret = await resolveProjectSecretHeader(request);
    const expanded = await readVersionExpanded(request, agent.id, agent.versionId, secret);
    const runtimeMeta = (expanded['meta'] as Record<string, unknown> | undefined) ?? {};
    expect(runtimeMeta['step_limit']).toBe(7);
  } finally {
    await deleteAgent(request, agent.id);
    // The agent delete takes the ASSOCIATION and leaves the `tags` row, so
    // the row is removed here — the only way to remove it used to be SQL.
    await deleteAutotestTags(request, [tagName]);
  }
});
