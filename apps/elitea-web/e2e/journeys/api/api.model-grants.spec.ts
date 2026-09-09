/**
 * WHO a platform model is available to, over the API.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS COVERS
 * ─────────────────────────────────────────────────────────────────────────────
 * A platform (catalogue) model used to be one thing: published, and therefore
 * offered to every project on the deployment. It now carries a GRANT — every
 * project, no project, or a chosen set — and the grant is enforced in three
 * places that no single-layer test can join up:
 *
 *   1. the model catalogue a project reads (`GET /configurations/models/{id}
 *      ?include_shared=true`), which is what the model picker offers;
 *   2. the LLM gateway's own model resolution, so a project cannot dispatch an
 *      id it can no longer see;
 *   3. the publish guard, which requires a model granted to ALL projects —
 *      a published agent is read by every project that opens the catalogue.
 *
 * This journey drives 1 and 3 end to end through the product's own routes. 2 is
 * pinned in the gateway's own module (it is a different service, reached
 * through a signed identity this suite does not mint) and in
 * `services/elitea-llm-gateway/internal/llmproxy/model_grant_test.go`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE ROWS THIS CREATES ARE SCOPED SO THEY REACH NO OTHER JOURNEY
 * ─────────────────────────────────────────────────────────────────────────────
 * A platform credential and a platform model resolve for EVERY project on the
 * deployment, and these journeys share one stack. `admin.configuration.spec.ts`
 * publishes neither for exactly that reason.
 *
 * The grant is what makes it safe to publish one here: every row this file
 * creates is `autotest_`-named and lives at `none` or `projects` — invisible to
 * every project but the one under test — for all but the two assertions that
 * are ABOUT the `all` scope. Those two run back to back, the row is returned to
 * `none` immediately after them, and everything is deleted in a `finally`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE READ ASKS FOR `include_shared`
 * ─────────────────────────────────────────────────────────────────────────────
 * The route defaults it to false and the product sends it as
 * `projectId !== publicProjectId`. Without it the answer is the project's own
 * rows, and a platform model is absent from all of them — so an assertion made
 * on the default would pass whatever the grant said. That is the shape this
 * repository keeps meeting: absence read as correctness.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  deleteAgent,
  readApplicationVersions,
  readCallerPersonalProjectId,
  readCatalogue,
  readProjectModels,
  resolveCatalogueProjectId,
  resolvePublishAuthorProjectId,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

import type { APIRequestContext, APIResponse } from '@playwright/test';

test.use({ storageState: STORAGE_STATE.admin });

/** The admin facade that authors platform providers and platform models. */
const PROVIDERS_URL = `${API_BASE}/admin/gateway/providers`;
const MODELS_URL = `${API_BASE}/admin/gateway/platform_models`;

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/** A version name in the alphabet the publish route accepts (`^[a-zA-Z0-9._-]+$`). */
function versionName(tag: string): string {
  return `${tag}-${Math.random().toString(36).slice(2, 8)}`;
}

/** The response, as text, for a message that says what actually happened. */
async function detail(response: APIResponse): Promise<string> {
  return `${response.status()}: ${(await response.text()).slice(0, 400)}`;
}

/**
 * The instructions a publishable agent needs.
 *
 * The pre-publish check raises a CRITICAL issue for instructions under 50
 * characters, and the publish runs it inline when no approval token is sent. An
 * agent built from the plain fixture cannot be published at all, so the refusal
 * cases below would pass for the wrong reason.
 */
const QUALITY_INSTRUCTIONS =
  'You are a release notes assistant. Turn the commits, tickets and review notes the user ' +
  'gives you into a short summary that names what changed, who it affects and what is still ' +
  'open.';

/** One platform credential, created through the admin facade. */
async function createPlatformProvider(
  request: APIRequestContext,
  title: string,
): Promise<void> {
  const response = await request.post(PROVIDERS_URL, {
    data: {
      elitea_title: title,
      type: 'open_ai',
      // No credential material. The create contacts no provider, and this row
      // exists only so a platform model has a link that resolves.
      data: { api_base: 'http://autotest.invalid/v1' },
    },
  });
  expect(response.status(), await detail(response)).toBe(201);
}

/**
 * One platform model, created through the admin facade at a stated grant.
 *
 * It is created at the RESTRICTED scope it is asked for, never at `all` and
 * then narrowed: a row that existed at `all` for even one request has been
 * offered to every project on a shared deployment.
 */
async function createPlatformModel(
  request: APIRequestContext,
  title: string,
  provider: string,
  grant: Record<string, unknown>,
): Promise<number> {
  const response = await request.post(MODELS_URL, {
    data: {
      elitea_title: title,
      type: 'llm_model',
      data: {
        name: title,
        ai_credentials: { elitea_title: provider },
        ...grant,
      },
    },
  });
  expect(response.status(), await detail(response)).toBe(201);
  const body = (await response.json()) as { id?: unknown };
  return Number(body.id);
}

/** One row of the admin platform-model listing. */
interface ListedPlatformModel {
  readonly id: number;
  readonly elitea_title: string;
  readonly share_scope: string;
  readonly shared_with: readonly number[];
}

/** The admin listing's row for one model, by title. */
async function readPlatformModel(
  request: APIRequestContext,
  title: string,
): Promise<ListedPlatformModel> {
  const response = await request.get(MODELS_URL);
  expect(response.status(), await detail(response)).toBe(200);
  const body = (await response.json()) as { items?: readonly ListedPlatformModel[] };
  const found = (body.items ?? []).find((item) => item.elitea_title === title);
  expect(found, `the platform listing carries no model called ${title}`).toBeDefined();
  return found as ListedPlatformModel;
}

/** Re-grant one platform model, keeping everything else it holds. */
async function regrantPlatformModel(
  request: APIRequestContext,
  model: { readonly id: number; readonly title: string; readonly provider: string },
  grant: Record<string, unknown>,
): Promise<void> {
  const response = await request.put(`${MODELS_URL}/${String(model.id)}`, {
    data: {
      elitea_title: model.title,
      // `data` is replaced WHOLE, so the link is restated with the grant. The
      // admin dialog does the same, over the object the listing reported.
      data: {
        name: model.title,
        ai_credentials: { elitea_title: model.provider },
        ...grant,
      },
    },
  });
  expect(response.status(), await detail(response)).toBe(200);
}

/** Whether one project's catalogue read offers this model. */
async function projectSeesModel(
  request: APIRequestContext,
  projectId: string,
  modelName: string,
): Promise<boolean> {
  const models = await readProjectModels(request, projectId, { includeShared: true });
  return models.some((model) => model.name === modelName);
}

/** Delete one platform row, tolerating a row a failed create never made. */
async function deletePlatformRow(
  request: APIRequestContext,
  url: string,
  id: number | undefined,
): Promise<void> {
  if (id === undefined || Number.isNaN(id)) return;
  await request.delete(`${url}/${String(id)}`);
}

/** The provider row's id, so the cleanup can address it. */
async function readPlatformProviderId(
  request: APIRequestContext,
  title: string,
): Promise<number | undefined> {
  const response = await request.get(PROVIDERS_URL);
  if (!response.ok()) return undefined;
  const body = (await response.json()) as {
    items?: readonly { readonly id?: unknown; readonly elitea_title?: unknown }[];
  };
  const found = (body.items ?? []).find((item) => item.elitea_title === title);
  return found === undefined ? undefined : Number(found.id);
}

test('a platform model is offered to the projects it is granted to, and to no others', async ({
  request,
}) => {
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const grantedProjectId = await resolvePublishAuthorProjectId(request);
  const ungrantedProjectId = await readCallerPersonalProjectId(request);

  // The two projects must really be two, or every assertion below is about one
  // of them twice.
  expect(
    ungrantedProjectId,
    'the caller has no personal project, so this journey has no second project to compare',
  ).not.toBe('');
  expect(ungrantedProjectId).not.toBe(grantedProjectId);
  expect(ungrantedProjectId).not.toBe(catalogueProjectId);

  const provider = autotestName('grant_prov');
  const modelTitle = autotestName('grant_model');
  let modelId: number | undefined;
  let providerId: number | undefined;

  try {
    await createPlatformProvider(request, provider);
    providerId = await readPlatformProviderId(request, provider);
    modelId = await createPlatformModel(request, modelTitle, provider, {
      share_scope: 'projects',
      shared_with: [Number(grantedProjectId)],
    });
    const model = { id: modelId, title: modelTitle, provider };

    // ── granted to ONE project ────────────────────────────────────────────
    //
    // Both halves, on one row. A read that only checked the granted project
    // would pass against a server that published the model to everybody.
    expect(await projectSeesModel(request, grantedProjectId, modelTitle)).toBe(true);
    expect(await projectSeesModel(request, ungrantedProjectId, modelTitle)).toBe(false);

    // The admin listing reports the grant back, which is what the edit dialog
    // opens on. A screen that could not read it would clear the grant of every
    // model it saved.
    const listed = await readPlatformModel(request, modelTitle);
    expect(listed.share_scope).toBe('projects');
    expect(listed.shared_with).toEqual([Number(grantedProjectId)]);

    // ── granted to NO project ─────────────────────────────────────────────
    await regrantPlatformModel(request, model, { share_scope: 'none' });
    expect(await projectSeesModel(request, grantedProjectId, modelTitle)).toBe(false);
    expect(await projectSeesModel(request, ungrantedProjectId, modelTitle)).toBe(false);
    // The CATALOGUE project still holds it: it reads its own schema as its own
    // rows, which is what keeps a withdrawn model recoverable from the screen
    // that has to re-grant it.
    expect(await projectSeesModel(request, catalogueProjectId, modelTitle)).toBe(true);
    expect((await readPlatformModel(request, modelTitle)).shared_with).toEqual([]);

    // ── granted to EVERY project ──────────────────────────────────────────
    //
    // The two reads run back to back and the row is returned to `none`
    // immediately: for the length of this step the model is offered to every
    // project on a stack these journeys share.
    await regrantPlatformModel(request, model, { share_scope: 'all' });
    try {
      expect(await projectSeesModel(request, grantedProjectId, modelTitle)).toBe(true);
      expect(await projectSeesModel(request, ungrantedProjectId, modelTitle)).toBe(true);
    } finally {
      await regrantPlatformModel(request, model, { share_scope: 'none' });
    }
  } finally {
    await deletePlatformRow(request, MODELS_URL, modelId);
    await deletePlatformRow(request, PROVIDERS_URL, providerId);
  }
});

test('an agent on a restricted platform model cannot be published, and one on an open model can', async ({
  request,
}) => {
  const authorProjectId = await resolvePublishAuthorProjectId(request);
  const catalogueProjectId = await resolveCatalogueProjectId(request);

  const provider = autotestName('pubgrant_prov');
  const modelTitle = autotestName('pubgrant_model');
  let modelId: number | undefined;
  let providerId: number | undefined;
  let agentId: string | undefined;

  try {
    await createPlatformProvider(request, provider);
    providerId = await readPlatformProviderId(request, provider);
    modelId = await createPlatformModel(request, modelTitle, provider, {
      share_scope: 'projects',
      shared_with: [Number(authorProjectId)],
    });
    const model = { id: modelId, title: modelTitle, provider };

    const name = autotestName('pubgrant_agent');
    const agent = await createAgentWithVersion(
      request,
      name,
      {
        instructions: QUALITY_INSTRUCTIONS,
        welcomeMessage: 'Send me the commits and I will draft the notes.',
        conversationStarters: ['Summarise this release.', 'What is still open?'],
        // The model IS the catalogue's, so the existing owning-project guard
        // admits it. What refuses the publish is the grant alone.
        model: { modelName: modelTitle, modelProjectId: catalogueProjectId },
      },
      authorProjectId,
    );
    agentId = agent.id;

    const refused = await request.post(
      `${API_BASE}/elitea_core/publish/prompt_lib/${authorProjectId}/${agent.versionId}`,
      { data: { version_name: versionName('rel') } },
    );
    expect(refused.status(), await detail(refused)).toBe(400);
    const refusal = (await refused.json()) as Record<string, unknown>;
    // The same CODE as the owning-project refusal — it is the same finding, and
    // the publish dialog already renders it beside the model picker. The `msg`
    // is what names the scope and says what to change.
    expect(refusal).toMatchObject({ error: 'llm_not_shared' });
    expect(String(refusal['msg'])).toContain(modelTitle);
    expect(String(refusal['msg'])).toContain('all projects');

    // Nothing was cloned and nothing reached the catalogue. A guard that
    // refused AFTER the clone answers the same 400.
    const versions = await readApplicationVersions(request, agent.id, authorProjectId);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
    expect((await readCatalogue(request)).some((row) => row.name === name)).toBe(false);

    // ── the same agent, on the same model, granted to everyone ────────────
    //
    // The other half, and the one that makes the refusal meaningful: a guard
    // that refused every catalogue model would pass the case above and publish
    // nothing at all. The model returns to `none` as soon as the publish is
    // answered.
    await regrantPlatformModel(request, model, { share_scope: 'all' });
    try {
      const published = await request.post(
        `${API_BASE}/elitea_core/publish/prompt_lib/${authorProjectId}/${agent.versionId}`,
        { data: { version_name: versionName('rel') } },
      );
      expect(published.status(), await detail(published)).toBe(200);
    } finally {
      await regrantPlatformModel(request, model, { share_scope: 'none' });
    }
  } finally {
    if (agentId !== undefined) {
      // Withdraw before deleting: a live published version refuses the delete
      // of the agent that owns it, so a cleanup that only deleted would leave
      // the agent AND its catalogue twin behind.
      const versions = await readApplicationVersions(request, agentId, authorProjectId);
      for (const version of versions) {
        if (version.status === 'published') {
          await request.post(
            `${API_BASE}/elitea_core/unpublish/prompt_lib/${authorProjectId}/${version.id}`,
            { data: {} },
          );
        }
      }
      await deleteAgent(request, agentId, authorProjectId);
    }
    await deletePlatformRow(request, MODELS_URL, modelId);
    await deletePlatformRow(request, PROVIDERS_URL, providerId);
  }
});
