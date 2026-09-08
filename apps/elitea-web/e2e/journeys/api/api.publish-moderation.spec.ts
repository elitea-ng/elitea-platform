/**
 * Publishing from INSIDE the catalogue, the operator's dashboard over it, and
 * the model a published agent is allowed to name.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's moderator and model-sharing cases (PUB-11, PUB-25,
 * PUB-30, PUB-34, PUB-35, PUB-39, PUB-42, PUB-56, PUB-74…PUB-78 and PUB-80).
 * Three subjects, one file because all three need the same pair of projects:
 *
 *   1. a publish issued from the catalogue project itself — the moderator's
 *      in-place flow, which adds a sibling version to the agent that is
 *      already there instead of writing a second agent somewhere else;
 *   2. the operator's published-agents dashboard, which is the only surface
 *      that answers "what has this deployment published, and where did each
 *      one come from";
 *   3. the rule that a published agent may only name a model the catalogue can
 *      resolve — the reason the rig seeds a project-private model at all.
 *
 * The lifecycle, immutability and quality-gate cases live in their own three
 * files; nothing here repeats them. What is repeated is the SETUP, because
 * every case needs an agent that could be published if nothing else stopped it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE TWO PROJECTS, AND WHY EACH CASE PICKS ONE
 * ─────────────────────────────────────────────────────────────────────────────
 * Publishing is cross-project. The clone stays in the author's own schema and
 * a TWIN is written into the catalogue project's, which is the only schema
 * ELITEA Catalog reads (internal/api/v2/eliteacore/catalog_mirror.go).
 *
 *   - `e2e-publish-author` is NOT the catalogue, so a publish from it writes
 *     the twin. Every case that reads what the CATALOGUE got — the model on
 *     the published copy, the icon, the dashboard's origin column — starts
 *     here, because in the other project there is no second row to read.
 *   - the CATALOGUE project is where a moderator stands. `mirrorPublishedVersion`
 *     takes its `isPublicProject` short circuit there, so the clone IS the
 *     catalogue row and the response carries no twin ids. That short circuit
 *     is the whole of the in-place flow, and it is what these cases assert.
 *
 * Both ids are resolved, never written down: the author project by NAME
 * through the product's own project listing, the catalogue project from
 * `platform_settings`, which is where the browser learns it too.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * TWO PLACES THIS PLATFORM ANSWERS DIFFERENTLY FROM THE LEGACY ONE
 * ─────────────────────────────────────────────────────────────────────────────
 * Both are asserted as the platform behaves, and both are called out where
 * they happen rather than left for a reader to discover from a failing run:
 *
 *   - a moderator does NOT skip the pre-publish quality gate. The legacy flow
 *     let a publish issued from the catalogue project through unchecked;
 *     `Publish` has no such branch, so a sparse agent is refused wherever it
 *     stands. See "the quality gate applies inside the catalogue too".
 *   - a withdrawn release name stays TAKEN on its agent. Withdrawal reverts
 *     the published clone to a draft and the draft keeps the release name
 *     (api.publish-lifecycle.spec.ts owns that half), so publishing that name
 *     again collides with it. The legacy moderator flow allowed the reuse.
 *     See "a withdrawn release name stays taken on its agent".
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE REFUSALS ARE ASSERTED ON THEIR BODIES
 * ─────────────────────────────────────────────────────────────────────────────
 * `Publish` answers 400 for five different findings and 422 for four more, and
 * each one is rendered somewhere else in the publish dialog. A journey that
 * asserted the status alone would pass against a handler that refused
 * everything for the first reason it thought of. Every refusal below therefore
 * names the finding the handler writes (`internal/api/v2/eliteacore/handler.go`)
 * and re-reads the agent — a refusal that had already cloned the row answers
 * the same status.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every agent is withdrawn and then deleted, in that order: a live
 * published version refuses the delete of the agent that owns it, so a cleanup
 * that only deleted would leave the agent AND its catalogue row for the next
 * run's catalogue and dashboard assertions to trip over. The agents these
 * cases publish IN the catalogue project would be counted by every other
 * journey's catalogue read, which is why they are the ones withdrawn first.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  PRIVATE_MODEL_NAME,
  createAgentWithVersion,
  deleteAgent,
  readApplicationVersions,
  readCatalogue,
  readProjectModels,
  readVersion,
  resolveCatalogueProjectId,
  resolvePrivateModel,
  resolvePublishAuthorProjectId,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

import type { APIRequestContext, APIResponse } from '@playwright/test';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/** A version name in the alphabet the publish route accepts (`^[a-zA-Z0-9._-]+$`). */
function versionName(tag: string): string {
  return `${tag}-${Math.random().toString(36).slice(2, 8)}`;
}

/** The refusal body, as text, for a message that says what actually happened. */
async function refusal(response: APIResponse): Promise<string> {
  return `${response.status()}: ${(await response.text()).slice(0, 400)}`;
}

/** The instructions a publishable agent needs — see `createPublishableAgent`. */
const QUALITY_INSTRUCTIONS =
  'You are a release notes assistant. Turn the commits, tickets and review notes the user ' +
  'gives you into a short summary that names what changed, who it affects and what is still ' +
  'open.';

/**
 * An agent that CAN pass the quality gate.
 *
 * Publishing runs the pre-publish check inline when no approval token is sent,
 * and the check raises a CRITICAL issue for instructions under 50 characters.
 * An agent built from the plain fixture cannot be published at all, so every
 * case here — including the refusal cases, which must fail for their OWN
 * reason and not for a sparse agent — starts from this one.
 *
 * `model` is optional and is the subject of the last four cases: `llm_settings.
 * model_project_id` is what the publish guard compares against the catalogue
 * project, and an agent that names no model at all skips that guard entirely.
 */
function createPublishableAgent(
  request: APIRequestContext,
  name: string,
  projectId: string,
  model?: { readonly modelName: string; readonly modelProjectId: string },
): Promise<{ readonly id: string; readonly versionId: string }> {
  return createAgentWithVersion(
    request,
    name,
    {
      instructions: QUALITY_INSTRUCTIONS,
      welcomeMessage: 'Send me the commits and I will draft the notes.',
      conversationStarters: ['Summarise this release.', 'What is still open?'],
      ...(model === undefined ? {} : { model }),
    },
    projectId,
  );
}

function publish(
  request: APIRequestContext,
  projectId: string,
  versionId: string,
  body: Record<string, unknown>,
): Promise<APIResponse> {
  return request.post(`${API_BASE}/elitea_core/publish/prompt_lib/${projectId}/${versionId}`, {
    data: body,
  });
}

function unpublish(
  request: APIRequestContext,
  projectId: string,
  versionId: string,
): Promise<APIResponse> {
  return request.post(`${API_BASE}/elitea_core/unpublish/prompt_lib/${projectId}/${versionId}`, {
    data: {},
  });
}

/** Publish, assert it went through, and answer the whole response body. */
async function publishOK(
  request: APIRequestContext,
  projectId: string,
  versionId: string,
  release: string,
): Promise<Record<string, unknown>> {
  const response = await publish(request, projectId, versionId, { version_name: release });
  expect(response.status(), await refusal(response)).toBe(200);
  return (await response.json()) as Record<string, unknown>;
}

/**
 * Withdraw everything this agent has live, then delete it.
 *
 * The order is the product's own rule: a live published version refuses the
 * delete of the agent that owns it, so a cleanup that only deleted would leave
 * the agent, its clone AND — for the cross-project cases — its catalogue twin.
 */
async function withdrawAndDelete(
  request: APIRequestContext,
  projectId: string,
  applicationId: string,
): Promise<void> {
  const versions = await readApplicationVersions(request, applicationId, projectId);
  for (const version of versions) {
    if (version.status === 'published') {
      await unpublish(request, projectId, version.id);
    }
  }
  await deleteAgent(request, applicationId, projectId);
}

/** Every `rule` a 422 validation result carries. */
function rulesOf(body: unknown): readonly string[] {
  const result = (body as { validation_result?: { issues?: unknown } })?.validation_result;
  const issues = result?.issues;
  if (!Array.isArray(issues)) return [];
  return issues.map((issue) => String((issue as { rule?: unknown })?.rule ?? ''));
}

/**
 * The agents of one project whose name is EXACTLY `name`.
 *
 * Asked through the listing's own `query` filter rather than by paging: the
 * catalogue project holds every agent every other journey publishes, so a
 * first page cannot answer "how many agents carry this name" — which is the
 * whole question the in-place publish has to answer. `query` is an `ILIKE`
 * on the server, so the exact match is made here.
 */
async function agentsNamed(
  request: APIRequestContext,
  projectId: string,
  name: string,
): Promise<readonly string[]> {
  const response = await request.get(
    `${API_BASE}/elitea_core/applications/prompt_lib/${projectId}`,
    { params: { query: name } },
  );
  expect(response.status(), await refusal(response)).toBe(200);
  const body = (await response.json()) as { rows?: readonly Record<string, unknown>[] };
  return (body.rows ?? [])
    .filter((row) => row['name'] === name)
    .map((row) => String(row['id'] ?? ''));
}

/** One row of the operator's published-agents dashboard. */
interface DashboardEntry extends Record<string, unknown> {
  readonly public_agent_id?: number;
  readonly name?: string;
  readonly author_project_id?: number | null;
  readonly total_published_versions?: number;
  readonly published_versions?: readonly Record<string, unknown>[];
  readonly adoption?: Record<string, unknown>;
}

/** The dashboard envelope, exactly as `AdminPublishedAgents` writes it. */
interface Dashboard extends Record<string, unknown> {
  readonly items?: readonly DashboardEntry[];
  readonly total?: number;
  readonly page?: number;
  readonly page_size?: number;
}

/**
 * The operator's published-agents listing.
 *
 * `administration` is a STATIC segment, not a mode a caller chooses: it is the
 * only mode the route is registered under, and the permission that gates it
 * (`runtime.admin.published_agents`) is central, not per project. The listing
 * offers no filter narrower than the page, so the page is taken at the
 * handler's own maximum and the row is found by name; it is ordered
 * newest-first, which is what keeps a just-published agent on it.
 */
async function dashboard(
  request: APIRequestContext,
  pageSize = 100,
): Promise<Dashboard> {
  const response = await request.get(
    `${API_BASE}/elitea_core/admin_published_agents/administration`,
    { params: { page_size: pageSize } },
  );
  expect(response.status(), await refusal(response)).toBe(200);
  return (await response.json()) as Dashboard;
}

/** The dashboard row for one agent name, polled — the listing is a second read. */
async function dashboardEntry(
  request: APIRequestContext,
  name: string,
): Promise<DashboardEntry> {
  let found: DashboardEntry | undefined;
  await expect
    .poll(
      async () => {
        found = (await dashboard(request)).items?.find((row) => row['name'] === name);
        return found !== undefined;
      },
      { timeout: 20_000 },
    )
    .toBe(true);
  return found as DashboardEntry;
}

/**
 * A model the catalogue itself owns — the only kind a published agent may name.
 *
 * Resolved from the catalogue project's own model listing rather than written
 * down: which models a deployment serves is a seed decision, and a literal
 * would make "the shared model publishes" pass or fail on a row nobody in this
 * file chose. The private one is excluded by name for the same reason it
 * exists — it is the counter-example the next cases are about.
 */
async function resolveSharedModel(
  request: APIRequestContext,
): Promise<{ readonly modelName: string; readonly modelProjectId: string }> {
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const models = await readProjectModels(request, catalogueProjectId);
  const shared = models.find((model) => model.name !== PRIVATE_MODEL_NAME);
  expect(
    shared,
    `the catalogue project ${catalogueProjectId} serves no model other than the seeded ` +
      `private one: ${JSON.stringify(models).slice(0, 300)}`,
  ).toBeDefined();
  return { modelName: (shared as { name: string }).name, modelProjectId: catalogueProjectId };
}

/* ── the moderator's in-place publish ─────────────────────────────────────── */

test('publishing inside the catalogue adds a version to the same agent and creates no second one', async ({
  request,
}) => {
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const name = autotestName('mod_inplace');
  const release = versionName('rel');
  const agent = await createPublishableAgent(request, name, catalogueProjectId);

  try {
    // ONE agent before, and the same one after. A handler that mirrored
    // unconditionally would write the twin into the schema the source already
    // stands in, and the moderator would find their agent listed twice — with
    // the copy carrying none of the toolkits, because a twin never does.
    expect(await agentsNamed(request, catalogueProjectId, name)).toEqual([agent.id]);

    const body = await publishOK(request, catalogueProjectId, agent.versionId, release);
    expect(String(body['public_agent_id'])).toBe(agent.id);

    // NO twin ids. Their absence is the in-place flow: `mirrorPublishedVersion`
    // returns early when the source project already IS the catalogue, and the
    // response omits the pair rather than repeating the clone's own ids under
    // them — a repeat would tell a client there were two rows to withdraw.
    expect(body['catalog_agent_id'], `a twin was written in place: ${JSON.stringify(body)}`)
      .toBeUndefined();
    expect(body['catalog_version_id']).toBeUndefined();

    // The draft survives beside the published sibling, exactly as it does for
    // an ordinary author: the moderator keeps something to edit.
    const versions = await readApplicationVersions(request, agent.id, catalogueProjectId);
    const draft = versions.find((version) => version.id === agent.versionId);
    const clone = versions.find((version) => version.id === String(body['public_version_id']));
    expect(draft?.status, `the source version is now ${JSON.stringify(versions)}`).toBe('draft');
    expect(clone?.status).toBe('published');
    expect(clone?.name).toBe(release);

    // Still one agent, and the catalogue carries it once. Two rows here is the
    // duplicate shell this case exists to refuse.
    expect(await agentsNamed(request, catalogueProjectId, name)).toEqual([agent.id]);
    await expect
      .poll(
        async () => (await readCatalogue(request)).filter((row) => row.name === name).length,
        { timeout: 20_000 },
      )
      .toBe(1);
  } finally {
    await withdrawAndDelete(request, catalogueProjectId, agent.id);
  }
});

test('two differently named catalogue publishes are two live versions of one agent', async ({
  request,
}) => {
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const name = autotestName('mod_two');
  const first = versionName('rela');
  const second = versionName('relb');
  const agent = await createPublishableAgent(request, name, catalogueProjectId);

  try {
    const one = await publishOK(request, catalogueProjectId, agent.versionId, first);
    const two = await publishOK(request, catalogueProjectId, agent.versionId, second);

    // The SAME agent both times. A second publish that made a second agent
    // would answer a different `public_agent_id` and split the version history
    // of one product into two catalogue entries.
    expect(String(one['public_agent_id'])).toBe(agent.id);
    expect(String(two['public_agent_id'])).toBe(agent.id);
    expect(String(one['public_version_id'])).not.toBe(String(two['public_version_id']));

    const versions = await readApplicationVersions(request, agent.id, catalogueProjectId);
    const live = versions.filter((version) => version.status === 'published');
    expect(live.map((version) => version.name).sort()).toEqual([first, second].sort());
    expect(await agentsNamed(request, catalogueProjectId, name)).toEqual([agent.id]);
  } finally {
    await withdrawAndDelete(request, catalogueProjectId, agent.id);
  }
});

test('withdrawing one catalogue version leaves the other one live', async ({ request }) => {
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const name = autotestName('mod_partial');
  const kept = versionName('rela');
  const dropped = versionName('relb');
  const agent = await createPublishableAgent(request, name, catalogueProjectId);

  try {
    const keptVersion = String(
      (await publishOK(request, catalogueProjectId, agent.versionId, kept))['public_version_id'],
    );
    const droppedVersion = String(
      (await publishOK(request, catalogueProjectId, agent.versionId, dropped))['public_version_id'],
    );

    // The withdrawal names ONE version. `Unpublish` has a second branch that
    // takes down every published clone of an agent at once — it is the one a
    // DRAFT id selects — so a route that resolved the agent instead of the
    // version would answer the same 200 and empty the whole catalogue entry.
    const withdrawn = await unpublish(request, catalogueProjectId, droppedVersion);
    expect(withdrawn.status(), await refusal(withdrawn)).toBe(200);

    const versions = await readApplicationVersions(request, agent.id, catalogueProjectId);
    expect(versions.find((version) => version.id === droppedVersion)?.status).toBe('draft');
    expect(versions.find((version) => version.id === keptVersion)?.status).toBe('published');

    // …and the agent is still IN the catalogue, because one live version is
    // enough to keep it there.
    await expect
      .poll(async () => (await readCatalogue(request)).some((row) => row.name === name), {
        timeout: 20_000,
      })
      .toBe(true);
  } finally {
    await withdrawAndDelete(request, catalogueProjectId, agent.id);
  }
});

test('a withdrawn release name stays taken on its agent', async ({ request }) => {
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const name = autotestName('mod_reuse');
  const release = versionName('rel');
  const agent = await createPublishableAgent(request, name, catalogueProjectId);

  try {
    const cloneId = String(
      (await publishOK(request, catalogueProjectId, agent.versionId, release))['public_version_id'],
    );
    const withdrawn = await unpublish(request, catalogueProjectId, cloneId);
    expect(withdrawn.status(), await refusal(withdrawn)).toBe(200);

    // THIS PLATFORM ANSWERS DIFFERENTLY FROM THE LEGACY ONE, and the difference
    // is asserted rather than hidden. The legacy moderator flow allowed
    // publish → withdraw → publish again under the SAME name. Here a withdrawal
    // REVERTS the published clone to a draft and that draft keeps the release
    // name (api.publish-lifecycle.spec.ts owns that half), so the name is still
    // held by a row of this agent and the pre-check finds it. The version-name
    // column is unique per application, so the alternative to this refusal is a
    // 500 on a constraint violation, not a success.
    //
    // The consequence is that a release name is spent for good once it has been
    // published: the reported divergence, not an accident of this journey.
    const again = await publish(request, catalogueProjectId, agent.versionId, {
      version_name: release,
    });
    expect(again.status(), await refusal(again)).toBe(422);
    expect(rulesOf(await again.json())).toContain('version_name_exists_in_source');

    // …and the same refusal is what a name already used by ANY draft on the
    // agent gets, which is the second case this covers: the reverted clone is
    // an ordinary draft now, and the pre-check does not care how it got there.
    const versions = await readApplicationVersions(request, agent.id, catalogueProjectId);
    expect(versions.filter((version) => version.name === release)).toHaveLength(1);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
  } finally {
    await withdrawAndDelete(request, catalogueProjectId, agent.id);
  }
});

test('the quality gate applies inside the catalogue too', async ({ request }) => {
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const name = autotestName('mod_gate');
  // Under 50 characters of instructions — the one CRITICAL finding the
  // deterministic check raises on an agent with nothing else wrong.
  const agent = await createAgentWithVersion(
    request,
    name,
    { instructions: 'Answer questions.' },
    catalogueProjectId,
  );

  try {
    // THIS PLATFORM ANSWERS DIFFERENTLY FROM THE LEGACY ONE. The legacy flow
    // let a publish issued from inside the catalogue project skip the
    // pre-publish check, on the reasoning that whoever stands there is a
    // moderator. `Publish` has no such branch: the gate runs on the version,
    // not on who is publishing it, so a sparse agent is refused wherever it
    // stands. Asserted as the platform behaves, and reported as a divergence.
    const refused = await publish(request, catalogueProjectId, agent.versionId, {
      version_name: versionName('rel'),
    });
    expect(refused.status(), await refusal(refused)).toBe(422);
    const body = (await refused.json()) as {
      error?: unknown;
      validation_result?: { status?: unknown; critical_issues?: readonly Record<string, unknown>[] };
    };
    expect(body.error).toBe('validation_failed');
    expect(body.validation_result?.status).toBe('FAIL');
    expect(
      (body.validation_result?.critical_issues ?? []).map((issue) => String(issue['field'])),
    ).toContain('instructions');

    // Nothing was published. A gate that refused AFTER cloning would answer the
    // same 422 and leave a live version behind it.
    const versions = await readApplicationVersions(request, agent.id, catalogueProjectId);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
  } finally {
    await withdrawAndDelete(request, catalogueProjectId, agent.id);
  }
});

/* ── the operator's dashboard ─────────────────────────────────────────────── */

test('the dashboard lists a published agent with its versions, and measures no adoption', async ({
  request,
}) => {
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const name = autotestName('mod_dash');
  const release = versionName('rel');
  const agent = await createPublishableAgent(request, name, catalogueProjectId);

  try {
    const published = await publishOK(request, catalogueProjectId, agent.versionId, release);
    const entry = await dashboardEntry(request, name);

    expect(String(entry['public_agent_id'])).toBe(agent.id);
    const versions = (entry['published_versions'] as readonly Record<string, unknown>[]) ?? [];
    // The PUBLISHED versions only, and the count beside them. The draft this
    // agent kept must not be on the list: an operator reading it would count
    // every editable version as something the public can reach.
    expect(versions.map((version) => String(version['version_id']))).toEqual([
      String(published['public_version_id']),
    ]);
    expect(versions[0]['version_name']).toBe(release);
    expect(versions[0]['published_at']).toBeTruthy();
    expect(entry['total_published_versions']).toBe(1);

    // ADOPTION IS NULL, NOT ZERO, and that is the point of the endpoint. No
    // producer on this platform writes `meta.adoption`, so the honest answer is
    // "not measured". Two zeroes would read as "nobody uses any of these" and
    // an operator could not tell the two apart from the response.
    const adoption = (entry['adoption'] as Record<string, unknown>) ?? {};
    expect(Object.keys(adoption).sort()).toEqual(['conversation_count', 'project_count']);
    expect(adoption['conversation_count']).toBeNull();
    expect(adoption['project_count']).toBeNull();
  } finally {
    await withdrawAndDelete(request, catalogueProjectId, agent.id);
  }
});

test('the dashboard bounds the page a caller may ask for', async ({ request }) => {
  // The route clamps `page_size` at 100, so a caller cannot pull the whole
  // catalogue in one response. Asserted through the ECHO rather than the row
  // count: a deployment with fewer than 100 published agents answers a short
  // page whatever the clamp does, so counting rows would pass with no clamp
  // at all.
  const body = await dashboard(request, 500);
  expect(body['page_size']).toBe(100);
  expect(body['page']).toBe(1);
  expect(Array.isArray(body['items'])).toBe(true);
  expect(typeof body['total']).toBe('number');
  expect((body['items'] as readonly unknown[]).length).toBeLessThanOrEqual(100);
});

test('the dashboard names the project an agent was published from', async ({ request }) => {
  const authorProjectId = await resolvePublishAuthorProjectId(request);
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const crossName = autotestName('mod_origin_x');
  const inPlaceName = autotestName('mod_origin_i');
  const crossAgent = await createPublishableAgent(request, crossName, authorProjectId);
  const inPlaceAgent = await createPublishableAgent(request, inPlaceName, catalogueProjectId);

  try {
    await publishOK(request, authorProjectId, crossAgent.versionId, versionName('rel'));
    await publishOK(request, catalogueProjectId, inPlaceAgent.versionId, versionName('rel'));

    // The origin is the twin's `shared_owner_id`, which only a mirrored publish
    // has. The two rows are asserted TOGETHER because either one alone is
    // consistent with a column that always answers the same thing: a dashboard
    // that reported the catalogue project for everything would pass the second
    // assertion, and one that reported nothing would pass the first.
    const crossEntry = await dashboardEntry(request, crossName);
    expect(String(crossEntry['author_project_id'])).toBe(authorProjectId);

    const inPlaceEntry = await dashboardEntry(request, inPlaceName);
    expect(inPlaceEntry['author_project_id']).toBeNull();
  } finally {
    await withdrawAndDelete(request, authorProjectId, crossAgent.id);
    await withdrawAndDelete(request, catalogueProjectId, inPlaceAgent.id);
  }
});

/* ── the model a published agent may name ─────────────────────────────────── */

test('an agent wired to a project-private model cannot be published', async ({ request }) => {
  const authorProjectId = await resolvePublishAuthorProjectId(request);
  const privateModel = await resolvePrivateModel(request);
  const name = autotestName('mod_privmodel');
  const agent = await createPublishableAgent(request, name, authorProjectId, {
    modelName: privateModel.modelName,
    modelProjectId: privateModel.projectId,
  });

  try {
    // A REAL model, in a project the caller really belongs to, private for the
    // only reason that matters: its project is not the catalogue's. An id that
    // named nothing would also be refused — by a guard that merely checked the
    // project exists, which is a different guard.
    const refused = await publish(request, authorProjectId, agent.versionId, {
      version_name: versionName('rel'),
    });
    expect(refused.status(), await refusal(refused)).toBe(400);
    // `llm_not_shared`, and NOT the quality gate's `validation_failed`: the
    // model check runs BEFORE the inline check, so this agent — which would
    // otherwise pass — is refused for the model and told so. The publish dialog
    // renders the two differently, and only one of them is fixable by editing
    // the agent's text.
    expect((await refused.json()) as Record<string, unknown>).toMatchObject({
      error: 'llm_not_shared',
    });

    // Nothing was cloned, and nothing reached the catalogue.
    const versions = await readApplicationVersions(request, agent.id, authorProjectId);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
    expect((await readCatalogue(request)).some((row) => row.name === name)).toBe(false);
  } finally {
    await withdrawAndDelete(request, authorProjectId, agent.id);
  }
});

test('the pre-publish check reports the private model as blocking, and issues no token', async ({
  request,
}) => {
  const authorProjectId = await resolvePublishAuthorProjectId(request);
  const privateModel = await resolvePrivateModel(request);
  const agent = await createPublishableAgent(
    request,
    autotestName('mod_privcheck'),
    authorProjectId,
    { modelName: privateModel.modelName, modelProjectId: privateModel.projectId },
  );

  try {
    // The check exists so the dialog can say why the Publish button will not
    // work BEFORE the author presses it. A check that passed an agent the
    // publish then refuses is worse than no check: the author is told the
    // agent is ready and the refusal arrives with the two answers disagreeing.
    const checked = await request.post(
      `${API_BASE}/elitea_core/publish_validate/prompt_lib/${authorProjectId}/${agent.versionId}`,
      { data: { version_name: versionName('rel') } },
    );
    expect(checked.status(), await refusal(checked)).toBe(422);
    const result = (await checked.json()) as {
      status?: unknown;
      critical_issues?: readonly Record<string, unknown>[];
      validation_token?: unknown;
    };
    expect(result.status).toBe('FAIL');
    const critical = result.critical_issues ?? [];
    expect(critical.map((issue) => String(issue['field']))).toContain('llm_settings');
    expect(
      critical.find((issue) => issue['field'] === 'llm_settings')?.['issue'],
      `the finding says nothing about sharing: ${JSON.stringify(critical)}`,
    ).toContain('not shared');

    // No token. A FAIL that still handed one out would let the publish skip the
    // very check that just refused it.
    expect(result.validation_token).toBeNull();
  } finally {
    await withdrawAndDelete(request, authorProjectId, agent.id);
  }
});

test('an agent wired to a catalogue model publishes, and the catalogue copy keeps that model', async ({
  request,
}) => {
  const authorProjectId = await resolvePublishAuthorProjectId(request);
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const sharedModel = await resolveSharedModel(request);
  const name = autotestName('mod_sharedmodel');
  const agent = await createPublishableAgent(request, name, authorProjectId, sharedModel);

  try {
    const body = await publishOK(request, authorProjectId, agent.versionId, versionName('rel'));

    // The clone keeps what the draft named — the publish copies `llm_settings`
    // rather than resetting it to a platform default, which would silently
    // change what a published agent answers with.
    const clone = await readVersion(
      request,
      agent.id,
      String(body['public_version_id']),
      authorProjectId,
    );
    expect(clone.llmSettings['model_name']).toBe(sharedModel.modelName);

    // …and so does the TWIN, in the catalogue's own schema. This is the copy a
    // reader of ELITEA Catalog opens, and it is read through the catalogue
    // project's own ids: a twin that carried a model project the catalogue
    // cannot resolve would render an agent nobody outside the author's project
    // could run, which is exactly what the refusal above exists to prevent.
    const twin = await readVersion(
      request,
      String(body['catalog_agent_id']),
      String(body['catalog_version_id']),
      catalogueProjectId,
    );
    expect(twin.llmSettings['model_name']).toBe(sharedModel.modelName);
    expect(String(twin.llmSettings['model_project_id'])).toBe(catalogueProjectId);
  } finally {
    await withdrawAndDelete(request, authorProjectId, agent.id);
  }
});

test('a moderator cannot publish an agent whose model belongs to another project', async ({
  request,
}) => {
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const privateModel = await resolvePrivateModel(request);
  const name = autotestName('mod_privadmin');
  // The agent stands in the CATALOGUE project and names a model that does not.
  // The moderator's flow has no exemption here: the guard compares the model's
  // project, not the caller's standing, so publishing from inside the
  // catalogue does not make another project's model shared.
  const agent = await createPublishableAgent(request, name, catalogueProjectId, {
    modelName: privateModel.modelName,
    modelProjectId: privateModel.projectId,
  });

  try {
    const refused = await publish(request, catalogueProjectId, agent.versionId, {
      version_name: versionName('rel'),
    });
    expect(refused.status(), await refusal(refused)).toBe(400);
    expect((await refused.json()) as Record<string, unknown>).toMatchObject({
      error: 'llm_not_shared',
    });

    const versions = await readApplicationVersions(request, agent.id, catalogueProjectId);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
  } finally {
    await withdrawAndDelete(request, catalogueProjectId, agent.id);
  }
});

/* ── what else the published copy carries ─────────────────────────────────── */

test('an uploaded icon survives publication into the catalogue', async ({ request }) => {
  const authorProjectId = await resolvePublishAuthorProjectId(request);
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const name = autotestName('mod_icon');
  const agent = await createPublishableAgent(request, name, authorProjectId);

  try {
    // The shape the icon uploader stores: a name, the URL the browser fetches,
    // and the sizes the crop step recorded. Every key is asserted on the twin
    // below, because the copy is made with a jsonb merge — a merge that took
    // the top-level key only would answer a plausible `icon_meta` with the
    // measurements gone, and the catalogue card would still render.
    const iconMeta = {
      name: `${name}.png`,
      url: `/api/v2/elitea_core/download_icon/prompt_lib/${authorProjectId}/${name}.png`,
      resulting_file_size: 4096,
      initial_file_size: 51_200,
      size: 128,
    };
    const stored = await request.put(
      `${API_BASE}/elitea_core/upload_icon/prompt_lib/${authorProjectId}/${agent.versionId}`,
      { data: iconMeta },
    );
    expect(stored.status(), await refusal(stored)).toBe(200);

    const body = await publishOK(request, authorProjectId, agent.versionId, versionName('rel'));
    const twin = await readVersion(
      request,
      String(body['catalog_agent_id']),
      String(body['catalog_version_id']),
      catalogueProjectId,
    );
    expect(twin.meta['icon_meta']).toEqual(iconMeta);
  } finally {
    await withdrawAndDelete(request, authorProjectId, agent.id);
  }
});

test('a built-in icon survives publication into the catalogue', async ({ request }) => {
  const authorProjectId = await resolvePublishAuthorProjectId(request);
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const name = autotestName('mod_icondef');
  const agent = await createPublishableAgent(request, name, authorProjectId);

  try {
    // A built-in icon is the same key with a URL the platform ships rather than
    // one an upload produced, and it carries no measurements at all. It is
    // asserted separately because a publish that rebuilt `icon_meta` from an
    // uploaded file's fields — instead of copying the value — would lose
    // exactly this one and leave the catalogue card with the default avatar.
    const iconMeta = { name: 'bot.svg', url: '/default_entity_icons/bot.svg' };
    const stored = await request.put(
      `${API_BASE}/elitea_core/upload_icon/prompt_lib/${authorProjectId}/${agent.versionId}`,
      { data: iconMeta },
    );
    expect(stored.status(), await refusal(stored)).toBe(200);

    const body = await publishOK(request, authorProjectId, agent.versionId, versionName('rel'));
    const twin = await readVersion(
      request,
      String(body['catalog_agent_id']),
      String(body['catalog_version_id']),
      catalogueProjectId,
    );
    expect(twin.meta['icon_meta']).toEqual(iconMeta);
  } finally {
    await withdrawAndDelete(request, authorProjectId, agent.id);
  }
});
