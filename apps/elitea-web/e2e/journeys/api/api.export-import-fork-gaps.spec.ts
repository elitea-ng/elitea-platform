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

import { API_BASE, DEFAULT_PROJECT_ID, resolvePublishAuthorProjectId } from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline, parseStoredGraph, storedNodeIds } from '../../fixtures/pipelines';
import { deleteApplicationIn, exportBundle, forkBundle, readApplicationIn } from '../../fixtures/exportImport';

function autotestName(stem: string): string {
  return `autotest_frk_${stem}_${String(Date.now()).slice(-7)}`;
}

/*
 * onetest: ELITEA-0673 — product gap: the fork route does NOT reject an
 * entity of an unrecognised type the way its sibling import route does.
 *
 * `api.import-wizard.spec.ts`'s IMP-05 proves `POST /import_wizard/…` answers
 * 400 outright for a bundle whose only entity names a type the handler
 * cannot place (`{ entity: 'quantum_entity', ... }`) — "nothing at all was
 * imported, so there is no partial success to report". The FORK route does
 * NOT share that refusal: the identical payload here answers 201 and creates
 * a bare agent shell (`version_details: null, versions: []`) instead —
 * broken, but not refused. This is written as the case says fork SHOULD
 * behave (parity with import's 400), then `test.fail`ed against that gap.
 */
test('the fork route answers 400 on a payload naming an entity type nothing can import', async ({ request }) => {
  test.fail(
    true,
    "ELITEA-0673: product gap — /fork accepts an entity of an unrecognised type and creates a broken " +
      "empty-versions agent shell (201) instead of refusing it the way /import_wizard's IMP-05 does (400)",
  );
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
