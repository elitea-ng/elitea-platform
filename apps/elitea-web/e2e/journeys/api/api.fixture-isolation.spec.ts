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
 * keeps out of the sweep's reach is deleted by id in the same test.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHEN THIS FILE RUNS
 * ─────────────────────────────────────────────────────────────────────────────
 * As `setup`'s TEARDOWN project (`fixtures-sweep`), so after every browser
 * project has finished. API-FX3's sweep is destructive and must never meet a
 * journey that is still running; a teardown is the only ordering Playwright
 * offers that guarantees that WITHOUT a dependency edge — and the edge is what
 * had to go, because a dependency's failure SKIPS its dependents and one
 * assertion in this file reported `308 did not run` for the whole chromium
 * suite. See `playwright.config.ts`'s own note.
 */
import { expect, test, type APIRequestContext } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  createConversation,
  deleteConversation,
  DEFAULT_PROJECT_ID,
  readProjectModels,
  resolveCatalogueProjectId,
  resolvePrivateModel,
  resolvePublishAuthorProjectId,
  sweepAutotestEntities,
} from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline } from '../../fixtures/pipelines';

// The operator persona, which holds a role on the seeded shared project every
// helper here addresses by default.
test.use({ storageState: STORAGE_STATE.admin });

const RUN = String(Date.now() % 1_000_000);

/**
 * How many rows of this listing carry exactly `name`.
 *
 * ── THE READ THIS REPLACES, AND WHY IT ANSWERED `[]` ──────────────────────
 * It used to page `.../applications/prompt_lib/{project}?limit=200` and search
 * the answer, and it received an empty array for a pipeline that had just been
 * created. Two independent reasons, both in the server:
 *
 *  1. THE LISTING IS TYPED. `applications.List` INNER JOINs
 *     `application_versions` on `agent_type != 'pipeline'` unless the caller
 *     sends `agents_type=pipeline` (`internal/infra/db/repos/applications.go`),
 *     so the default listing is the CLASSIC agents and never carries a
 *     pipeline at all. This file ran before any journey had created a classic
 *     agent, so the answer was legitimately empty.
 *  2. `limit=200` IS NOT 200. The handler clamps `limit` to 1..100 and falls
 *     back to its default of TWENTY for anything else
 *     (`internal/api/v2/applications/handler.go`), so the read asked for two
 *     hundred rows and would have received twenty — the #544 shape again.
 *
 * So the row is NAMED rather than hoped for: `query` is the applications
 * route's own search filter, and a filtered read cannot walk off the end of a
 * page. `count()` and not the name list, so callers poll it — a create is
 * visible to a list read a moment after the POST answers.
 *
 * ONLY the applications route is read this way. The conversations listing
 * offers no search parameter at all (`internal/api/v2/conversations/handler.go`
 * reads `limit`, `offset`, `source`, `entity_name`, `entity_meta_id` and
 * nothing else), and by the time this file runs — as `setup`'s teardown, after
 * every journey — the shared project can easily hold more than one page of
 * `autotest_` conversations. So the conversation is read by ID instead, which
 * is exact and cannot page past itself.
 */
async function countNamed(
  request: APIRequestContext,
  path: string,
  name: string,
  query = '',
): Promise<number> {
  const url =
    `${API_BASE}/${path}/prompt_lib/${DEFAULT_PROJECT_ID}` +
    `?limit=100&query=${encodeURIComponent(name)}${query}`;
  const response = await request.get(url);
  expect(response.status(), await response.text()).toBe(200);
  const body = (await response.json()) as {
    rows?: readonly { name?: string }[];
    items?: readonly { name?: string }[];
  };
  return (body.rows ?? body.items ?? []).filter((row) => row.name === name).length;
}

/** The pipeline listing — the typed half the default listing cannot answer. */
const PIPELINES = '&agents_type=pipeline';

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
    // every assertion below while proving nothing at all. The conversation by
    // id, the two pipelines through a polled list read — a create is visible
    // to a LIST a moment after its POST answers, which a single read races.
    const before = await request.get(
      `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
    );
    expect(before.status(), 'the conversation this test created must exist before the sweep').toBe(
      200,
    );
    await expect
      .poll(async () => countNamed(request, 'elitea_core/applications', pipelineName, PIPELINES), {
        timeout: 20_000,
        message: 'the created pipeline never appeared in the pipeline listing',
      })
      .toBe(1);
    await expect
      .poll(async () => countNamed(request, 'elitea_core/applications', bystanderName, PIPELINES), {
        timeout: 20_000,
        message: 'the bystander pipeline never appeared in the pipeline listing',
      })
      .toBe(1);

    // ── the sweep ────────────────────────────────────────────────────────
    const report = await sweepAutotestEntities(request);

    // What it DID, before what happened afterwards. A sweep whose list read
    // failed used to return `void` and look identical to one that found a
    // clean project — the failure list is what tells those apart, and the
    // counts are what stop "0 rows removed" from reading as success.
    expect(report.failures, 'the sweep must not swallow a failed read or delete').toEqual([]);
    expect(
      report.conversations,
      'the sweep must have removed at least the conversation this test created',
    ).toBeGreaterThanOrEqual(1);
    expect(
      report.pipelines,
      'the sweep must reach PIPELINES — the classic listing does not carry them, and a ' +
        'sweep that reads only that listing leaves every autotest_ pipeline behind forever',
    ).toBeGreaterThanOrEqual(1);

    // ── and the rows really are gone, read back from the server ──────────
    // The conversation by ID: gone from the SERVER, not merely unlisted. A
    // list that filtered it out and a delete that ran are different facts, and
    // only the second one frees the name.
    const reread = await request.get(
      `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
    );
    expect(
      reread.status(),
      'the sweep reported success and left the conversation behind',
    ).toBe(404);
    expect(
      await countNamed(request, 'elitea_core/applications', pipelineName, PIPELINES),
      'the sweep left the pipeline behind',
    ).toBe(0);
    expect(
      await countNamed(request, 'elitea_core/applications', bystanderName, PIPELINES),
      'the sweep removed a row that does not carry the autotest_ prefix — it is scoped by ' +
        'NAME, and a sweep that ignores the scope destroys the seeded project',
    ).toBe(1);
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

/**
 * API-FX5: the tenancy the publish journeys resolve is really provisioned.
 *
 * The publish journeys need a project that is NOT the catalogue, and a model
 * that is NOT the catalogue's — `scripts/e2e-stack.sh` seeds both, and
 * `e2e/fixtures/api.ts` finds them BY NAME so no journey repeats an id the
 * seed picked out of a reserved range.
 *
 * Stated here rather than left to the journeys that use it, because the way it
 * breaks is silent in both directions. A seed that never created the project
 * fails those journeys with "the caller is a member of no project of that
 * name", which reads as a broken stack; a deployment whose public project is
 * the SAME project would let every one of them pass while proving nothing —
 * the publish would take the "already in the catalogue" short circuit and the
 * private model would not be private. This test creates nothing and asserts
 * exactly those two properties.
 */
test('API-FX5: the seeded author project and its non-shared model exist and are not the catalogue', async ({
  request,
}) => {
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const authorProjectId = await resolvePublishAuthorProjectId(request);
  expect(
    authorProjectId,
    'the author project must not be the catalogue project, or the publish journeys prove nothing',
  ).not.toBe(catalogueProjectId);

  // The model is resolved through the picker's own route, and the resolver
  // refuses a model that is not where the guard expects it.
  const privateModel = await resolvePrivateModel(request);
  expect(privateModel.projectId).toBe(authorProjectId);

  // …and the catalogue project does NOT serve it. That is the whole meaning of
  // "non-shared" for the publish guard: a model that a published agent cannot
  // reach from the catalogue.
  const catalogueModels = await readProjectModels(request, catalogueProjectId);
  expect(catalogueModels.map((model) => model.name)).not.toContain(privateModel.modelName);
});
