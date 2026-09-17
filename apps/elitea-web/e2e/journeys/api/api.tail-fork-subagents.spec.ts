/**
 * Onetest wave-1 tail (T1b, folder `export-import/fork`) — ELITEA-0674:
 * forking a master agent that has a sub-agent attached is claimed to copy
 * the sub-agent along with it, in ONE fork operation, with the copy's own
 * toolkit reference rewritten to point at the NEW sub-agent in the
 * destination project (not the source's).
 *
 * `api.export-import-fork-gaps.spec.ts`/`api.import-wizard.spec.ts` only
 * fork a single, standalone agent or pipeline — nothing in this suite forks
 * one with an attached sub-agent. Measured directly against the running
 * stack: `exportBundle(parent, {fork:true})` carries exactly ONE entry in
 * `applications` — the parent — and the fork answer's copy carries
 * `tools: []`. The sub-agent ATTACHMENT itself does not survive the fork,
 * let alone get a copy of its own: `POST /fork/prompt_lib/{project}` reads
 * only the named entity's own bundle and never walks its
 * `application_relation` rows.
 */
import { test, expect } from '@playwright/test';

import {
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  attachSubAgent,
  createAgentWithVersion,
  deleteAgent,
  resolvePublishAuthorProjectId,
} from '../../fixtures/api';
import { deleteApplicationIn, exportBundle, forkBundle, readApplicationIn } from '../../fixtures/exportImport';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

/* onetest: ELITEA-0674 — product gap: forking a master agent does not copy its attached sub-agent, and the copy's toolkit reference is not rewritten to any destination-project entity — it is simply dropped. */
test('forking a master agent with a sub-agent attached copies the sub-agent too, reference rewritten to the copy', async ({
  request,
}) => {
  test.setTimeout(60_000);
  const destinationProjectId = await resolvePublishAuthorProjectId(request);
  expect(destinationProjectId, 'the fork case needs a second project').not.toBe(DEFAULT_PROJECT_ID);

  const parentName = uniqueName('forkmaster');
  const subName = uniqueName('forksub');
  const parent = await createAgentWithVersion(request, parentName, { instructions: 'The parent agent brief.' });
  const sub = await createAgentWithVersion(request, subName, { instructions: 'The sub-agent brief.' });
  let copyId: string | undefined;
  let subCopyId: string | undefined;

  try {
    expect(
      (await attachSubAgent(request, parent.versionId, { applicationId: sub.id, versionId: sub.versionId })).ok(),
    ).toBe(true);

    const exported = await exportBundle(request, parent.id, { fork: true });
    const answer = await forkBundle(request, destinationProjectId, exported.bundle);
    expect([201, 207], JSON.stringify(answer.body).slice(0, 500)).toContain(answer.status);

    const forked = answer.body.result?.agents ?? [];
    copyId = forked.find((row: { readonly name?: string }) => row.name === parentName)?.id;
    subCopyId = forked.find((row: { readonly name?: string }) => row.name === subName)?.id;
    expect(copyId, 'the fork wrote no parent copy').toBeDefined();

    test.fail(
      true,
      'ELITEA-0674 (#918): product gap — POST /fork only bundles the named entity; a sub-agent reached through ' +
        'application_relation is neither exported nor copied, so the forked parent carries no sub-agent at all',
    );

    expect(subCopyId, 'the sub-agent must exist as its own standalone entity in the destination project').toBeDefined();

    const copy = await readApplicationIn(request, destinationProjectId, copyId ?? '');
    const copiedVersion = copy.version_details ?? copy.versions?.[0];
    const toolIds = ((copiedVersion as { readonly tool_ids?: readonly unknown[] } | undefined)?.tool_ids ?? []) as readonly (
      | string
      | number
    )[];
    expect(
      toolIds.map(String),
      "the copied parent's toolkit reference must point at the sub-agent's NEW copy, not be empty",
    ).toContain(subCopyId);
  } finally {
    if (subCopyId !== undefined) await deleteApplicationIn(request, destinationProjectId, subCopyId);
    if (copyId !== undefined) await deleteApplicationIn(request, destinationProjectId, copyId);
    await deleteAgent(request, sub.id);
    await deleteAgent(request, parent.id);
  }
});
