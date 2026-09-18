/**
 * FRK: the two Fork assertions not already covered by
 * `api.import-wizard.spec.ts`'s FRK-01 (agent, cross-project, provenance keys
 * and model rewrite) or `agents.publishing.spec.ts` (`fork copies the agent
 * into the chosen project`, same-project UI fork).
 *
 * FRK-01 is AGENT-only and asserts the happy path; this file adds the
 * refusal shape (`POST /fork/prompt_lib/{projectId}` on a payload that names
 * nothing) and the PIPELINE half of the same route — `/fork` and `/export_import`
 * both key off `agent_type`, not a hard-coded entity kind, so a pipeline is
 * exercised through the exact same helpers (`exportBundle`/`forkBundle`) FRK-01
 * uses for an agent.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  DEFAULT_PROJECT_ID,
  createAgentWithVersion,
  createGithubToolkit,
  deleteAgent,
  deleteGithubToolkit,
  resolvePublishAuthorProjectId,
  type GithubToolkitFixture,
} from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline, parseStoredGraph, storedNodeIds } from '../../fixtures/pipelines';
import {
  attachToolkitToVersion,
  deleteApplicationIn,
  exportBundle,
  forkBundle,
  readApplicationIn,
} from '../../fixtures/exportImport';

function autotestName(stem: string): string {
  return `autotest_frk_${stem}_${String(Date.now()).slice(-7)}`;
}

/*
 * onetest: ELITEA-0673 — the fork route now rejects an entity of an
 * unrecognised type the way its sibling import route does.
 *
 * `api.import-wizard.spec.ts`'s IMP-05 proves `POST /import_wizard/…` answers
 * 400 outright for a bundle whose only entity names a type the handler
 * cannot place (`{ entity: 'quantum_entity', ... }`) — "nothing at all was
 * imported, so there is no partial success to report". `Fork` (handler.go)
 * takes the identical unrecognised-type shape down its agent branch (there is
 * no `entity`-type switch on this route at all — the same shape a version-less
 * entry takes), and used to create a bare `version_details: null, versions: []`
 * shell anyway; it now treats "no version content at all" as a refusal,
 * rolls back the orphaned `applications` row, and answers 400 like `import_wizard`.
 */
test('the fork route answers 400 on a payload naming an entity type nothing can import', async ({ request }) => {
  const destinationProjectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('fork-400');
  let createdId: string | undefined;
  try {
    const response = await request.post(`${API_BASE}/elitea_core/fork/prompt_lib/${destinationProjectId}`, {
      data: { applications: [{ entity: 'quantum_entity', name, description: 'an entity type nothing serves' }] },
    });
    const body = (await response.json()) as { result?: { agents?: readonly { readonly id?: string }[] } };
    createdId = body.result?.agents?.[0]?.id;
    expect(response.status(), JSON.stringify(body)).toBe(400);
  } finally {
    if (createdId !== undefined) await deleteApplicationIn(request, destinationProjectId, createdId);
  }
});

/* onetest: ELITEA-0677 — forking a PIPELINE (not an agent) sets `is_forked`, records the parent, and carries only the version active at fork time */
test('forking a pipeline into another project sets is_forked and records the parent pipeline', async ({
  request,
}) => {
  test.setTimeout(60_000);
  const name = autotestName('pipeline');
  const destinationProjectId = await resolvePublishAuthorProjectId(request);
  expect(destinationProjectId, 'the fork case needs a second project').not.toBe(DEFAULT_PROJECT_ID);

  let sourceId: string | undefined;
  let copyId: string | undefined;
  try {
    const pipeline = await createPipelineThroughApi(request, name);
    sourceId = pipeline.id;

    const exported = await exportBundle(request, pipeline.id, { fork: true });
    const application = exported.bundle.applications?.[0];
    expect(application?.type, 'the fork export named no pipeline entity').toBeTruthy();

    const answer = await forkBundle(request, destinationProjectId, exported.bundle);
    expect(answer.status, `the pipeline fork answered ${answer.text.slice(0, 400)}`).toBe(201);

    const forked = answer.body.result?.agents ?? [];
    expect(forked, 'the fork wrote no pipeline').toHaveLength(1);
    copyId = forked[0]?.id;
    expect(copyId, 'the fork answered no id').toBeDefined();
    expect(forked[0]?.version_details?.is_forked).toBe(true);

    const copy = await readApplicationIn(request, destinationProjectId, copyId ?? '');
    expect(copy.name).toBe(name);
    const copiedVersion = copy.version_details ?? copy.versions?.[0];
    expect(copiedVersion?.agent_type, 'the forked copy is not a pipeline').toBe('pipeline');

    const meta = (copiedVersion?.meta ?? {}) as Record<string, unknown>;
    expect(String(meta['parent_entity_id'] ?? '')).toBe(pipeline.id);
    expect(String(meta['parent_project_id'] ?? '')).toBe(DEFAULT_PROJECT_ID);

    // Only the version active at fork time travels — the graph the source
    // pipeline was built with, not a second copy of every saved version.
    const graph = parseStoredGraph(copiedVersion?.instructions ?? '');
    expect(storedNodeIds(graph).length, 'the forked pipeline graph lost its nodes').toBeGreaterThan(0);
    expect((copy.versions ?? [copy.version_details]).length).toBe(1);
  } finally {
    if (copyId !== undefined) await deleteApplicationIn(request, destinationProjectId, copyId);
    if (sourceId !== undefined) await deletePipeline(request, { id: sourceId, projectId: DEFAULT_PROJECT_ID });
  }
});

/*
 * onetest: ELITEA-0672 (addendum) — #918. A toolkit attached to an agent's
 * version survives a CROSS-PROJECT fork: the reference is kept (its
 * credentials scrubbed by the export, which is ELITEA-0675's half), not
 * dropped.
 *
 * The bug was one key wide in two places at once. `forkBundle`'s body carried
 * `{applications, skills}` and no `toolkits`, and `Fork` never read a
 * `toolkits` key or wrote a toolkit row anywhere — so the forked copy came
 * back with an EMPTY `tools` array and the author had to rebuild every
 * attachment by hand. The export had carried the array all along.
 */
test('a toolkit attached to a version survives a cross-project fork', async ({ request }) => {
  test.setTimeout(60_000);
  const name = autotestName('toolkit');
  const destinationProjectId = await resolvePublishAuthorProjectId(request);
  expect(destinationProjectId, 'the fork case needs a second project').not.toBe(DEFAULT_PROJECT_ID);

  let sourceId: string | undefined;
  let copyId: string | undefined;
  let toolkit: GithubToolkitFixture | undefined;
  try {
    const agent = await createAgentWithVersion(request, name, {
      instructions: 'Answer questions about the repository for the release notes.',
    });
    sourceId = agent.id;
    toolkit = await createGithubToolkit(request, DEFAULT_PROJECT_ID, `${name}_tk`, {});
    await attachToolkitToVersion(request, toolkit.toolkitId, {
      applicationId: agent.id,
      versionId: agent.versionId,
      selectedTools: ['get_issue'],
    });

    const exported = await exportBundle(request, agent.id, { fork: true });
    expect(
      (exported.bundle.toolkits ?? []).some((entry) => entry.type === 'github'),
      `the export carried no github toolkit: ${exported.text.slice(0, 400)}`,
    ).toBe(true);

    const answer = await forkBundle(request, destinationProjectId, exported.bundle);
    expect(answer.status, `the fork answered ${answer.text.slice(0, 400)}`).toBe(201);
    copyId = (answer.body.result?.agents ?? [])[0]?.id;
    expect(copyId, 'the fork answered no id').toBeDefined();

    // The fork's own answer names the toolkit it copied — the channel that
    // was documented as "always empty".
    expect(
      (answer.body.result?.toolkits ?? []).some((entry) => entry.type === 'github'),
      `the fork result named no toolkit: ${answer.text.slice(0, 400)}`,
    ).toBe(true);

    // And the STORED copy carries the attachment, which is what the author
    // opens the editor to find.
    const copy = await readApplicationIn(request, destinationProjectId, copyId ?? '');
    const copiedVersion = copy.version_details ?? copy.versions?.[0];
    const tools = (copiedVersion?.tools ?? []) as readonly Record<string, unknown>[];
    expect(
      tools.some((tool) => tool['type'] === 'github'),
      `the forked copy carries no github-typed tool: ${JSON.stringify(tools)}`,
    ).toBe(true);
  } finally {
    if (copyId !== undefined) await deleteApplicationIn(request, destinationProjectId, copyId);
    if (sourceId !== undefined) await deleteAgent(request, sourceId);
    await deleteGithubToolkit(request, DEFAULT_PROJECT_ID, toolkit);
  }
});
