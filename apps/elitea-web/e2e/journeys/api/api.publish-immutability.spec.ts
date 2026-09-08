/**
 * What a publish FREEZES, what the catalogue shows, and who gets the credit.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's immutability, catalogue-visibility and category cases
 * (PUB-12…PUB-15, PUB-21, PUB-22, PUB-26, PUB-27 and CAT-01…CAT-04). Publishing
 * an agent turns one editable draft into a live version and, where sub-agents
 * are attached, into a private embedded copy of each of them. Every one of
 * those rows is frozen: it cannot be edited, deleted, or re-wired while it is
 * live, and the agent that owns it cannot be deleted either. Withdraw first.
 *
 * It also carries the evidence for two defects this package fixed:
 *
 *   - the publish response named the CLONE under `source_version_id` instead
 *     of the draft it was made from (asserted in api.publish-lifecycle.spec.ts,
 *     which owns the round trip);
 *   - the published row carried no `published_by`, so the operator's
 *     published-agents dashboard could never say who published anything. That
 *     one is asserted here, through the dashboard route itself.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY EACH REFUSAL IS READ TWICE
 * ─────────────────────────────────────────────────────────────────────────────
 * Every guard below answers 400, and all four of them answer it with a
 * DIFFERENT message that a different screen renders. A journey that asserted
 * the status alone would pass against a handler that refused everything for
 * the first reason it thought of. So each case names the sentence the handler
 * writes (`internal/api/v2/applications/handler.go` and
 * `internal/api/v2/eliteacore/application_relation.go`) AND reads the row back
 * — a refusal that had already deleted or rewritten the row answers 400 too.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY A SECOND PROJECT
 * ─────────────────────────────────────────────────────────────────────────────
 * Publishing is cross-project: the clone stays in the author's schema and a
 * TWIN is written into the catalogue project's, which is the only schema the
 * catalogue reads. Project 1 IS the catalogue on this rig, so a publish issued
 * from there writes no twin and the two halves of every guard collapse into
 * one row. `scripts/e2e-stack.sh` seeds `e2e-publish-author` for this and the
 * id is resolved BY NAME.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every agent is withdrawn and then deleted, in that order — a live
 * version refuses the delete of the agent that owns it, which is case 3 below,
 * so a cleanup that only deleted would leave the agent, its clone AND its
 * catalogue row for the next run's catalogue assertions to trip over.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  deleteAgent,
  readApplicationVersions,
  readCallerIdentity,
  readCatalogue,
  readVersion,
  resolveCatalogueProjectId,
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

/** The `error` string a refusal carries, or the whole body when it carries none. */
async function errorOf(response: APIResponse): Promise<string> {
  const body = (await response.json()) as { error?: unknown };
  return typeof body.error === 'string' ? body.error : JSON.stringify(body);
}

/**
 * An agent that CAN pass the quality gate.
 *
 * Publishing runs the pre-publish check inline when no approval token is sent,
 * and the check raises a CRITICAL issue for instructions under 50 characters.
 * An agent built from the plain fixture cannot be published at all, so every
 * case here starts from this one.
 */
function createPublishableAgent(
  request: APIRequestContext,
  name: string,
  projectId: string,
): Promise<{ readonly id: string; readonly versionId: string }> {
  return createAgentWithVersion(
    request,
    name,
    {
      instructions:
        'You are a release notes assistant. Turn the commits, tickets and review notes the ' +
        'user gives you into a short summary that names what changed, who it affects and what ' +
        'is still open.',
      welcomeMessage: 'Send me the commits and I will draft the notes.',
      conversationStarters: ['Summarise this release.', 'What is still open?'],
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

/** The publish response, with the four ids every case below addresses. */
interface PublishedAgent {
  readonly applicationId: string;
  readonly draftVersionId: string;
  readonly cloneVersionId: string;
  readonly catalogueAgentId: string;
  readonly catalogueVersionId: string;
}

/** Create a publishable agent and publish it, asserting the round trip happened. */
async function createAndPublish(
  request: APIRequestContext,
  name: string,
  projectId: string,
  body: Record<string, unknown> = {},
): Promise<PublishedAgent> {
  const agent = await createPublishableAgent(request, name, projectId);
  const published = await publish(request, projectId, agent.versionId, {
    version_name: versionName('rel'),
    ...body,
  });
  expect(published.status(), await refusal(published)).toBe(200);
  const answer = (await published.json()) as Record<string, unknown>;
  // The catalogue pair is what says the mirror ran. It is omitted when the
  // author already stands in the catalogue project, and a case that silently
  // got `undefined` here would then assert nothing about the catalogue.
  expect(answer['catalog_agent_id'], `no catalogue twin in ${JSON.stringify(answer)}`).toBeTruthy();
  return {
    applicationId: agent.id,
    draftVersionId: agent.versionId,
    cloneVersionId: String(answer['public_version_id']),
    catalogueAgentId: String(answer['catalog_agent_id']),
    catalogueVersionId: String(answer['catalog_version_id']),
  };
}

/**
 * Withdraw everything this agent has live, then delete it.
 *
 * The order is the product's own rule, which case 3 below states: a live
 * published version refuses the delete of the agent that owns it.
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

/* ── the immutability guards ──────────────────────────────────────────────── */

test('a published version cannot be deleted where it was published', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('imm_delver');
  const agent = await createAndPublish(request, name, projectId);

  try {
    const refused = await request.delete(
      `${API_BASE}/elitea_core/version/prompt_lib/${projectId}/${agent.applicationId}/${agent.cloneVersionId}`,
    );
    expect(refused.status(), await refusal(refused)).toBe(400);
    expect(await errorOf(refused)).toBe('Unpublish first. Cannot delete a published version.');

    // The row survives. A refusal that deleted the version anyway answers the
    // same 400, and the author would find their release gone.
    const versions = await readApplicationVersions(request, agent.applicationId, projectId);
    const clone = versions.find((version) => version.id === agent.cloneVersionId);
    expect(clone?.status, `the clone is now ${JSON.stringify(versions)}`).toBe('published');
  } finally {
    await withdrawAndDelete(request, projectId, agent.applicationId);
  }
});

test('the catalogue copy of a published version cannot be deleted either', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const name = autotestName('imm_delcat');
  const agent = await createAndPublish(request, name, projectId);

  try {
    // The SAME guard, in the schema the catalogue reads. It matters separately:
    // this is the row a moderator standing in the catalogue project can reach,
    // and deleting it would empty the catalogue entry while the author's own
    // Published tab went on showing the agent as live.
    const refused = await request.delete(
      `${API_BASE}/elitea_core/version/prompt_lib/${catalogueProjectId}` +
        `/${agent.catalogueAgentId}/${agent.catalogueVersionId}`,
    );
    expect(refused.status(), await refusal(refused)).toBe(400);
    expect(await errorOf(refused)).toBe('Unpublish first. Cannot delete a published version.');

    await expect
      .poll(async () => (await readCatalogue(request)).some((row) => row.name === name), {
        timeout: 20_000,
      })
      .toBe(true);
  } finally {
    await withdrawAndDelete(request, projectId, agent.applicationId);
  }
});

test('the agent that owns a live version cannot be deleted', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('imm_delapp');
  const agent = await createAndPublish(request, name, projectId);

  try {
    const refused = await request.delete(
      `${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${agent.applicationId}`,
    );
    expect(refused.status(), await refusal(refused)).toBe(400);
    expect(await errorOf(refused)).toBe(
      'Unpublish first. Cannot delete application with published versions.',
    );

    // The agent, its draft and its clone all survive. The delete cascades to
    // every version, so a guard that ran after the delete would take the
    // catalogue entry with it and leave the twin pointing at nothing.
    const versions = await readApplicationVersions(request, agent.applicationId, projectId);
    expect(versions.map((version) => version.id)).toEqual(
      expect.arrayContaining([agent.draftVersionId, agent.cloneVersionId]),
    );
  } finally {
    await withdrawAndDelete(request, projectId, agent.applicationId);
  }
});

test('the catalogue agent cannot be deleted while it carries a live version', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  const name = autotestName('imm_delcatapp');
  const agent = await createAndPublish(request, name, projectId);

  try {
    const refused = await request.delete(
      `${API_BASE}/elitea_core/application/prompt_lib/${catalogueProjectId}/${agent.catalogueAgentId}`,
    );
    expect(refused.status(), await refusal(refused)).toBe(400);
    expect(await errorOf(refused)).toBe(
      'Unpublish first. Cannot delete application with published versions.',
    );

    const twinVersions = await readApplicationVersions(
      request,
      agent.catalogueAgentId,
      catalogueProjectId,
    );
    expect(twinVersions.map((version) => version.id)).toContain(agent.catalogueVersionId);
  } finally {
    await withdrawAndDelete(request, projectId, agent.applicationId);
  }
});

test('a published version is read-only and the draft beside it is not', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('imm_editver');
  const agent = await createAndPublish(request, name, projectId);
  const edited = 'You are a release notes assistant, and this sentence was written by an edit.';

  try {
    const refused = await request.put(
      `${API_BASE}/elitea_core/version/prompt_lib/${projectId}/${agent.applicationId}/${agent.cloneVersionId}`,
      { data: { instructions: edited } },
    );
    expect(refused.status(), await refusal(refused)).toBe(400);
    expect(await errorOf(refused)).toBe('Published version can not be updated. Unpublish first.');

    const clone = await readVersion(request, agent.applicationId, agent.cloneVersionId, projectId);
    expect(clone.instructions, 'the refused edit reached the published row').not.toContain(
      'written by an edit',
    );

    // The DRAFT is still editable, and that half is the point: the freeze is
    // about the published copy, not about the agent. An author who publishes a
    // release must be able to keep working on the next one.
    const saved = await request.put(
      `${API_BASE}/elitea_core/version/prompt_lib/${projectId}/${agent.applicationId}/${agent.draftVersionId}`,
      { data: { instructions: edited } },
    );
    expect([200, 201], await refusal(saved)).toContain(saved.status());
    const draft = await readVersion(request, agent.applicationId, agent.draftVersionId, projectId);
    expect(draft.instructions).toBe(edited);
  } finally {
    await withdrawAndDelete(request, projectId, agent.applicationId);
  }
});

test('a sub-agent cannot be attached to a published version, and can be attached to the draft', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const parentName = autotestName('imm_relparent');
  const childName = autotestName('imm_relchild');
  const child = await createPublishableAgent(request, childName, projectId);
  const parent = await createAndPublish(request, parentName, projectId);

  /** The URL names the CHILD and the body names the PARENT — the opposite of how it reads. */
  const attachTo = (parentVersionId: string): Promise<APIResponse> =>
    request.patch(
      `${API_BASE}/elitea_core/application_relation/prompt_lib/${projectId}` +
        `/${child.id}/${child.versionId}`,
      {
        data: {
          application_id: Number(parent.applicationId),
          version_id: Number(parentVersionId),
          has_relation: true,
        },
      },
    );

  try {
    const refused = await attachTo(parent.cloneVersionId);
    expect(refused.status(), await refusal(refused)).toBe(400);
    expect(await errorOf(refused)).toBe(
      'Cannot change relation on a published version. Unpublish first.',
    );

    // Nothing was wired. A refusal that had already inserted the row answers
    // the same 400, and the published agent would then call an agent its
    // readers never agreed to.
    const clone = await readVersion(request, parent.applicationId, parent.cloneVersionId, projectId);
    expect(clone.tools.filter((tool) => tool['type'] === 'application')).toHaveLength(0);

    // …and the draft accepts it, which is what makes the refusal a rule about
    // the published copy rather than about the pair of agents.
    const attached = await attachTo(parent.draftVersionId);
    expect(attached.status(), await refusal(attached)).toBe(201);
    const draft = await readVersion(request, parent.applicationId, parent.draftVersionId, projectId);
    expect(draft.tools.filter((tool) => tool['type'] === 'application')).toHaveLength(1);
  } finally {
    await withdrawAndDelete(request, projectId, parent.applicationId);
    await withdrawAndDelete(request, projectId, child.id);
  }
});

test('an embedded sub-agent copy is read-only, and so is the version that embeds it', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const parentName = autotestName('imm_embparent');
  const childName = autotestName('imm_embchild');
  const child = await createPublishableAgent(request, childName, projectId);
  const parent = await createPublishableAgent(request, parentName, projectId);

  try {
    const attached = await request.patch(
      `${API_BASE}/elitea_core/application_relation/prompt_lib/${projectId}` +
        `/${child.id}/${child.versionId}`,
      {
        data: {
          application_id: Number(parent.id),
          version_id: Number(parent.versionId),
          has_relation: true,
        },
      },
    );
    expect(attached.status(), await refusal(attached)).toBe(201);

    const published = await publish(request, projectId, parent.versionId, {
      version_name: versionName('rel'),
    });
    expect(published.status(), await refusal(published)).toBe(200);
    const cloneId = String(((await published.json()) as Record<string, unknown>)['public_version_id']);

    // The embedded copy, found through the published version's own tool list.
    // Publishing leaves TWO sub-agent references on the clone — the copied
    // mapping, which still names the author's own child, and the link to the
    // private embedded copy the publish made. The one that does not name the
    // child's version is the embedded copy, and it is polled for because the
    // embedding runs after the publish transaction commits.
    const embedded = await embeddedSubAgentOf(request, parent.id, cloneId, projectId, child.versionId);

    const refusedEmbedded = await request.put(
      `${API_BASE}/elitea_core/version/prompt_lib/${projectId}` +
        `/${embedded.applicationId}/${embedded.versionId}`,
      { data: { instructions: 'An edit no reader of the catalogue agreed to.' } },
    );
    expect(refusedEmbedded.status(), await refusal(refusedEmbedded)).toBe(400);
    expect(await errorOf(refusedEmbedded)).toBe(
      'Published version can not be updated. Unpublish first.',
    );

    // …and neither copy can be deleted while the parent is live. This is the
    // half that keeps a published agent from losing a sub-agent it calls: the
    // embedded row is the only copy the catalogue entry has.
    const refusedEmbeddedDelete = await request.delete(
      `${API_BASE}/elitea_core/version/prompt_lib/${projectId}` +
        `/${embedded.applicationId}/${embedded.versionId}`,
    );
    expect(refusedEmbeddedDelete.status(), await refusal(refusedEmbeddedDelete)).toBe(400);
    expect(await errorOf(refusedEmbeddedDelete)).toBe(
      'Unpublish first. Cannot delete a published version.',
    );

    const refusedParentDelete = await request.delete(
      `${API_BASE}/elitea_core/version/prompt_lib/${projectId}/${parent.id}/${cloneId}`,
    );
    expect(refusedParentDelete.status(), await refusal(refusedParentDelete)).toBe(400);
    expect(await errorOf(refusedParentDelete)).toBe(
      'Unpublish first. Cannot delete a published version.',
    );
  } finally {
    await withdrawAndDelete(request, projectId, parent.id);
    await withdrawAndDelete(request, projectId, child.id);
  }
});

/* ── what the catalogue shows ─────────────────────────────────────────────── */

test('the catalogue lists the published parent and not its embedded sub-agent copy', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const parentName = autotestName('cat_parent');
  const childName = autotestName('cat_child');
  const child = await createPublishableAgent(request, childName, projectId);
  const parent = await createPublishableAgent(request, parentName, projectId);

  try {
    const attached = await request.patch(
      `${API_BASE}/elitea_core/application_relation/prompt_lib/${projectId}` +
        `/${child.id}/${child.versionId}`,
      {
        data: {
          application_id: Number(parent.id),
          version_id: Number(parent.versionId),
          has_relation: true,
        },
      },
    );
    expect(attached.status(), await refusal(attached)).toBe(201);

    const published = await publish(request, projectId, parent.versionId, {
      version_name: versionName('rel'),
    });
    expect(published.status(), await refusal(published)).toBe(200);

    await expect
      .poll(async () => (await readCatalogue(request)).some((row) => row.name === parentName), {
        timeout: 20_000,
      })
      .toBe(true);

    // The embedded copy carries the CHILD's name, and it must not be a
    // catalogue entry of its own: it is an implementation detail of the
    // parent, has no author who chose to publish it, and a reader who opened
    // it would get an agent nobody offered.
    const rows = await readCatalogue(request);
    expect(rows.filter((row) => row.name === childName)).toHaveLength(0);
  } finally {
    await withdrawAndDelete(request, projectId, parent.id);
    await withdrawAndDelete(request, projectId, child.id);
  }
});

test('the operator dashboard names who published each version', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const caller = await readCallerIdentity(request);
  const name = autotestName('cat_publishedby');
  const agent = await createAndPublish(request, name, projectId);

  try {
    // The dashboard reads `meta.published_by` off the catalogue row. Nothing on
    // the agent publish path wrote that key, so this column was null for every
    // agent on every deployment — indistinguishable from "this platform does
    // not record it", and unfixable by any operator.
    const listed = await publishedAgentsDashboard(request);
    const entry = listed.find((row) => row['name'] === name);
    expect(entry, `the dashboard lists no agent named ${name}`).toBeDefined();
    const versions = (entry?.['published_versions'] as readonly Record<string, unknown>[]) ?? [];
    expect(versions.length, `no published versions on ${JSON.stringify(entry)}`).toBeGreaterThan(0);
    // The CALLER, not merely a number: an implementation that stamped the
    // agent's author would pass an "is not null" check while still answering
    // the wrong question on every publish made on somebody else's behalf.
    expect(String(versions[0]['published_by'])).toBe(caller.id);
  } finally {
    await withdrawAndDelete(request, projectId, agent.applicationId);
  }
});

/* ── the categories ───────────────────────────────────────────────────────── */

test('the categories route answers the set the publish dialog offers', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const response = await request.get(
    `${API_BASE}/elitea_core/agent_categories/prompt_lib/${projectId}`,
  );
  expect(response.status(), await refusal(response)).toBe(200);
  const body = (await response.json()) as { categories?: readonly Record<string, unknown>[] };
  const categories = body.categories ?? [];

  // The nine the platform ships. They are asserted as a SET rather than a
  // list: an operator may add their own through the admin Features page, and
  // those arrive with `is_default: false` beside these.
  const defaults = categories
    .filter((category) => category['is_default'] === true)
    .map((category) => String(category['name']));
  expect(new Set(defaults)).toEqual(
    new Set([
      'Business Analyst',
      'Quality Assurance',
      'Development',
      'DevOps',
      'Project Management',
      'Knowledge & Documentation',
      'Elitea',
      'Epam',
      'Other',
    ]),
  );
});

test('an agent published under a category is found by that filter and not under Other', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('cat_filtered');
  const agent = await createAndPublish(request, name, projectId, { category: 'Development' });

  try {
    await expect
      .poll(async () => (await readCatalogue(request, 'Development')).some((row) => row.name === name), {
        timeout: 20_000,
      })
      .toBe(true);

    // …and NOT in the catch-all. `Other` matches a missing category as well as
    // a stored one, so an agent appearing in both buckets would mean the
    // category never reached the catalogue row — which is what the reader
    // sees as "this agent is filed under nothing".
    const other = await readCatalogue(request, 'Other');
    expect(other.filter((row) => row.name === name)).toHaveLength(0);
  } finally {
    await withdrawAndDelete(request, projectId, agent.applicationId);
  }
});

test('an agent published with no category falls into Other', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('cat_other');
  const agent = await createAndPublish(request, name, projectId);

  try {
    // `Other` is the catch-all bucket: the filter matches a row whose category
    // is absent, empty, or the literal `Other`. Without it an uncategorised
    // publish would be served by the API and rendered by no tab.
    await expect
      .poll(async () => (await readCatalogue(request, 'Other')).some((row) => row.name === name), {
        timeout: 20_000,
      })
      .toBe(true);
  } finally {
    await withdrawAndDelete(request, projectId, agent.applicationId);
  }
});

test('a category the platform does not offer is refused and nothing is published', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('cat_unknown');
  const agent = await createPublishableAgent(request, name, projectId);

  try {
    const refused = await publish(request, projectId, agent.versionId, {
      version_name: versionName('rel'),
      category: 'NotARealCategory',
    });
    expect(refused.status(), await refusal(refused)).toBe(422);
    const issues =
      ((await refused.json()) as { validation_result?: { issues?: readonly Record<string, unknown>[] } })
        .validation_result?.issues ?? [];
    expect(issues.map((issue) => String(issue['rule']))).toContain('invalid_category');

    // Nothing was created. The category is checked before the clone is
    // written, so a published version here would mean the order had changed
    // and an agent had reached the catalogue under a category no tab renders.
    const versions = await readApplicationVersions(request, agent.id, projectId);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
  } finally {
    await withdrawAndDelete(request, projectId, agent.id);
  }
});

/* ── helpers that need the routes above ───────────────────────────────────── */

/**
 * The embedded copy of `childVersionId` that a publish made, polled for.
 *
 * The embedding runs AFTER the publish transaction commits (`embedSubAgents`),
 * so the tool list is read until it carries the second reference rather than
 * once — a single read would be a race that fails on a busy stack and passes
 * on an idle one.
 */
async function embeddedSubAgentOf(
  request: APIRequestContext,
  parentApplicationId: string,
  parentVersionId: string,
  projectId: string,
  childVersionId: string,
): Promise<{ readonly applicationId: string; readonly versionId: string }> {
  let found: { readonly applicationId: string; readonly versionId: string } | undefined;
  await expect
    .poll(
      async () => {
        const stored = await readVersion(request, parentApplicationId, parentVersionId, projectId);
        for (const tool of stored.tools) {
          if (tool['type'] !== 'application') continue;
          const settings = (tool['config'] ?? tool['settings']) as Record<string, unknown> | undefined;
          if (settings === undefined) continue;
          const versionId = String(settings['application_version_id'] ?? '');
          if (versionId === '' || versionId === childVersionId) continue;
          found = {
            applicationId: String(settings['application_id'] ?? ''),
            versionId,
          };
          return true;
        }
        return false;
      },
      { timeout: 20_000 },
    )
    .toBe(true);
  if (found === undefined) throw new Error('embeddedSubAgentOf: the poll passed and found nothing');
  return found;
}

/**
 * The operator's published-agents listing.
 *
 * It offers no filter narrower than the page, so the page is taken at its
 * maximum and the row is found by NAME. The listing is newest-first, which is
 * what keeps a just-published agent on it.
 */
async function publishedAgentsDashboard(
  request: APIRequestContext,
): Promise<readonly Record<string, unknown>[]> {
  const response = await request.get(
    `${API_BASE}/elitea_core/admin_published_agents/administration`,
    { params: { page_size: 100 } },
  );
  expect(response.status(), await refusal(response)).toBe(200);
  const body = (await response.json()) as { items?: readonly Record<string, unknown>[] };
  return body.items ?? [];
}
