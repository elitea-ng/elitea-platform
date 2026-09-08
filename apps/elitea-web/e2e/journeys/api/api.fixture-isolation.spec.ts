/**
 * API-FX: OUR fixtures, tested — the port of the legacy suite's two
 * `*Isolation` classes.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS, AND WHY IT IS NOT A TRANSCRIPTION
 * ─────────────────────────────────────────────────────────────────────────────
 * `qa/elitea-testing-public`:
 *
 *  - `automation/tests/ui/chat/test_conversation_management.py::
 *    TestConversationIsolation::{test_fixture_creates_fresh_conversation,
 *    test_fixture_cleanup_cycle}`                       -> API-FX1, API-FX3
 *  - `automation/tests/ui/pipelines/test_pipeline_management.py::
 *    TestPipelineIsolation::{test_fixture_creates_fresh_pipeline,
 *    test_fixture_cleanup_cycle}`                       -> API-FX2, API-FX3
 *
 * Those four assert the LEGACY suite's own fixtures — `conversation_id`,
 * `pipeline_id`, `conversation_api`, `pipeline_api` — none of which exist
 * here. The coverage map called them "harness-internal, nothing to port", and
 * that reading is half right: there is no PRODUCT use case in them. There is a
 * HARNESS claim, and this suite makes exactly the same one in exactly the same
 * places, so the port is to restate the claim against `e2e/fixtures/*` rather
 * than to drop it.
 *
 * It is worth restating. Both claims are the kind that fails silently:
 *
 *  - A fixture that hands a journey a conversation someone else already
 *    wrote in does not fail; it makes the journey's first assertion about a
 *    message it did not send. `chat.multiturn.spec.ts` counts message groups,
 *    and a "fresh" conversation with one row in it moves every one of its
 *    numbers by one.
 *  - A cleanup that reports success and deletes nothing does not fail either.
 *    It fails the NEXT run, somewhere else — `toolkits.emptyList.spec.ts` has
 *    a whole Playwright project of its own because leftover rows in the
 *    shared project made a count assertion unsatisfiable, and
 *    `admin.app-requests.spec.ts` grew a teardown for the same reason
 *    (#544). Both are this failure, discovered the expensive way.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY AN API JOURNEY AND NOT A `scripts` VITEST
 * ─────────────────────────────────────────────────────────────────────────────
 * Every claim here is about what the SERVER holds after the fixture ran. A
 * unit test would have to mock the routes, and a mocked delete deletes
 * nothing — it would prove the helper calls a URL, which is the one thing that
 * was never in doubt.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing, and API-FX3 is the proof: it ends by asserting the shared project
 * holds no `autotest_` row this file created. The one row it deliberately
 * keeps out of the sweep's reach is deleted by name in the same test.
 */
import { expect, test, type APIRequestContext } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  createConversation,
  deleteConversation,
  DEFAULT_PROJECT_ID,
  sweepAutotestEntities,
} from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline } from '../../fixtures/pipelines';

// The operator persona, which holds a role on the seeded shared project every
// helper here addresses by default.
test.use({ storageState: STORAGE_STATE.admin });

const RUN = String(Date.now() % 1_000_000);

/** The named rows of one entity list, as the server holds them right now. */
async function listNames(request: APIRequestContext, path: string): Promise<readonly string[]> {
  const response = await request.get(`${API_BASE}/${path}/prompt_lib/${DEFAULT_PROJECT_ID}?limit=200`);
  expect(response.status(), await response.text()).toBe(200);
  const body = (await response.json()) as {
    rows?: readonly { name?: string }[];
    items?: readonly { name?: string }[];
  };
  const rows = body.rows ?? body.items ?? [];
  return rows.map((row) => row.name ?? '');
}

test('API-FX1: a conversation the fixture creates is FRESH — no message another journey wrote (legacy TestConversationIsolation::test_fixture_creates_fresh_conversation)', async ({
  request,
}) => {
  const name = `${AUTOTEST_PREFIX}fx1_${RUN}`;
  const conversationId = await createConversation(request, name);
  try {
    // The SERVER's copy of the conversation, with its message groups embedded.
    // `messages_limit` is what makes the handler embed them at all
    // (`api/v2/conversations/handler.go`); omitting it answers a well-formed
    // 200 with no groups, which would let this assertion pass against a
    // conversation full of somebody else's messages.
    const stored = await request.get(
      `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}` +
        '?messages_limit=50&sort_order=asc',
    );
    expect(stored.status(), await stored.text()).toBe(200);
    const body = (await stored.json()) as {
      name?: string;
      message_groups?: readonly unknown[];
      participants?: readonly unknown[];
    };

    expect(body.name, 'the fixture must return the conversation it named').toBe(name);
    expect(
      body.message_groups ?? [],
      'a fixture-created conversation must carry no messages — a journey that counts ' +
        'message groups reads every one of them as its own',
    ).toEqual([]);
    expect(
      body.participants ?? [],
      'and no participants: a leftover agent would answer a turn the journey never armed',
    ).toEqual([]);
  } finally {
    await deleteConversation(request, conversationId);
  }
});

test('API-FX2: a pipeline the fixture creates is FRESH, and is stored as a pipeline (legacy TestPipelineIsolation::test_fixture_creates_fresh_pipeline)', async ({
  request,
}) => {
  const name = `${AUTOTEST_PREFIX}fx2_${RUN}`;
  const pipeline = await createPipelineThroughApi(request, name);
  try {
    const stored = await request.get(
      `${API_BASE}/elitea_core/application/prompt_lib/${pipeline.projectId}/${pipeline.id}`,
    );
    expect(stored.status(), await stored.text()).toBe(200);
    const body = (await stored.json()) as {
      name?: string;
      version_details?: { agent_type?: string; instructions?: string };
      versions?: readonly unknown[];
    };

    expect(body.name, 'the fixture must return the pipeline it named').toBe(name);
    // The legacy body asserts the name starts with `autotest_`; here that is
    // the naming convention the sweep below depends on, so it is asserted as
    // such rather than as a spelling.
    expect(
      name.startsWith(AUTOTEST_PREFIX),
      'a fixture row that does not carry the autotest_ prefix is invisible to the sweep',
    ).toBe(true);
    // The discriminator the fixture already throws on, restated at the level
    // a journey reads: a row stored as an AGENT is served by this route
    // identically and disappears only from a list filtered by agents_type.
    expect(body.version_details?.agent_type, 'the row must be stored as a pipeline').toBe('pipeline');
    // Exactly one version — `base`. A fixture that left a second one behind
    // would make `pipelines.versioning.spec.ts`'s numbers start at two.
    expect(body.versions ?? [], 'a fresh pipeline owns exactly its base version').toHaveLength(1);
  } finally {
    await deletePipeline(request, pipeline);
  }
});

test('API-FX3: the autotest_ sweep really removes the rows, and removes ONLY those (legacy test_fixture_cleanup_cycle, both classes)', async ({
  request,
}) => {
  const conversationName = `${AUTOTEST_PREFIX}fx3_conv_${RUN}`;
  const pipelineName = `${AUTOTEST_PREFIX}fx3_pipe_${RUN}`;
  // Deliberately WITHOUT the prefix. This is the half a "nothing is left"
  // assertion cannot state on its own: a sweep that deleted the whole project
  // would satisfy it perfectly, and the next journey would fail on a seeded
  // row it never touched.
  const bystanderName = `e2e_fixture_bystander_${RUN}`;

  const conversationId = await createConversation(request, conversationName);
  const pipeline = await createPipelineThroughApi(request, pipelineName);
  const bystander = await createPipelineThroughApi(request, bystanderName);

  try {
    // ── the rows exist before the sweep ──────────────────────────────────
    // Asserted, not assumed: a sweep run against an empty project passes
    // every assertion below while proving nothing at all.
    await expect
      .poll(async () => listNames(request, 'elitea_core/conversations'), {
        timeout: 20_000,
        message: 'the created conversation never appeared in the list',
      })
      .toContain(conversationName);
    const applicationsBefore = await listNames(request, 'elitea_core/applications');
    expect(applicationsBefore).toContain(pipelineName);
    expect(applicationsBefore).toContain(bystanderName);

    // ── the sweep ────────────────────────────────────────────────────────
    await sweepAutotestEntities(request);

    const conversationsAfter = await listNames(request, 'elitea_core/conversations');
    expect(
      conversationsAfter,
      'the sweep reported success and left the conversation behind',
    ).not.toContain(conversationName);

    const applicationsAfter = await listNames(request, 'elitea_core/applications');
    expect(applicationsAfter, 'the sweep left the pipeline behind').not.toContain(pipelineName);
    expect(
      applicationsAfter,
      'the sweep removed a row that does not carry the autotest_ prefix — it is scoped by ' +
        'NAME, and a sweep that ignores the scope destroys the seeded project',
    ).toContain(bystanderName);

    // …and the deleted conversation is gone from the server, not only from a
    // list: a list that filtered it out and a delete that ran are different
    // facts, and only the second one frees the name.
    const reread = await request.get(
      `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
    );
    expect(
      reread.status(),
      'a swept conversation must be gone from the server, not merely unlisted',
    ).toBe(404);
  } finally {
    // The bystander is this test's own row and the sweep will never take it.
    await deletePipeline(request, bystander);
    await deletePipeline(request, pipeline).catch(() => undefined);
    await deleteConversation(request, conversationId).catch(() => undefined);
  }
});

test('API-FX4: two journeys running against the same project do not see each other rows (legacy TestConversationIsolation + TestPipelineIsolation, the claim both classes are named for)', async ({
  request,
}) => {
  // Two tags standing for two journeys. They share a project, a persona and a
  // stack — which is the arrangement this suite actually runs in, and the one
  // the legacy `*Isolation` classes exist to state something about.
  const alpha = `${AUTOTEST_PREFIX}fx4a_${RUN}`;
  const beta = `${AUTOTEST_PREFIX}fx4b_${RUN}`;

  const alphaConversation = await createConversation(request, alpha);
  const betaConversation = await createConversation(request, beta);
  const alphaPipeline = await createPipelineThroughApi(request, alpha);
  const betaPipeline = await createPipelineThroughApi(request, beta);

  try {
    // ── identity, not merely a different name ────────────────────────────
    // Two creates that returned the SAME id would satisfy every name-shaped
    // assertion in this suite: both journeys would write into one row and
    // each would read the other's messages as its own.
    expect(
      alphaConversation,
      'two fixture conversations must be two rows, not one row named twice',
    ).not.toBe(betaConversation);
    expect(alphaPipeline.id, 'two fixture pipelines must be two rows').not.toBe(betaPipeline.id);
    expect(
      alphaPipeline.versionId,
      'and two versions — a shared version row makes one journey a publish of the other',
    ).not.toBe(betaPipeline.versionId);

    // ── each row answers with its OWN name ───────────────────────────────
    for (const [conversationId, expected] of [
      [alphaConversation, alpha],
      [betaConversation, beta],
    ] as const) {
      const stored = await request.get(
        `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}` +
          '?messages_limit=50&sort_order=asc',
      );
      expect(stored.status(), await stored.text()).toBe(200);
      const body = (await stored.json()) as { name?: string; message_groups?: readonly unknown[] };
      expect(body.name).toBe(expected);
      expect(
        body.message_groups ?? [],
        'a conversation must not carry the neighbouring journey messages',
      ).toEqual([]);
    }

    // ── deleting one leaves the other alone ──────────────────────────────
    // The failure this closes is a delete addressed by NAME rather than by
    // id: two journeys with a shared prefix would take each other rows down
    // mid-run, and the survivor would fail on a 404 for a row it created.
    await deleteConversation(request, alphaConversation);
    await deletePipeline(request, alphaPipeline);

    const survivingConversation = await request.get(
      `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${betaConversation}`,
    );
    expect(
      survivingConversation.status(),
      'deleting one journey conversation must not take the neighbour with it',
    ).toBe(200);
    const survivingPipeline = await request.get(
      `${API_BASE}/elitea_core/application/prompt_lib/${betaPipeline.projectId}/${betaPipeline.id}`,
    );
    expect(
      survivingPipeline.status(),
      'deleting one journey pipeline must not take the neighbour with it',
    ).toBe(200);
  } finally {
    await deleteConversation(request, betaConversation).catch(() => undefined);
    await deletePipeline(request, betaPipeline).catch(() => undefined);
    await deleteConversation(request, alphaConversation).catch(() => undefined);
    await deletePipeline(request, alphaPipeline).catch(() => undefined);
  }
});
