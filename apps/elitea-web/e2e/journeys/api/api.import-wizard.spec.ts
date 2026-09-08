/**
 * IMP + FRK: what the import wizard does with a bundle that is PARTLY wrong,
 * and what a fork records about where the copy came from.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's multi-entity import cases IMP-02…05 and its fork case
 * FRK-01. IMP-01 — "a single tool-less agent imports and is readable" — is
 * already covered by EXP-009 in `api.export-import-agents.spec.ts`, so it is
 * re-verified there rather than written twice.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE ERROR CONTRACT IS THE SUBJECT
 * ─────────────────────────────────────────────────────────────────────────────
 * The import answers ONE body for four different outcomes. Every entity that
 * landed is in `result`, every entity that did not is in `errors`, and the
 * status is 201 / 207 / 400 depending only on how those two counts came out
 * (`services/elitea-main/internal/api/v2/eliteacore/handler.go`,
 * `ExportImportPost`). A caller that reads the status alone therefore cannot
 * tell a complete import from one that wrote an agent with no toolkit — the
 * exact reading the wizard makes, since it marks the entity the user selected
 * by the INDEX each error entry carries.
 *
 * So each case here asserts three things together: the status, the channel the
 * fault was reported on, and the index that names which submitted entry it was
 * about. A wrong index is not cosmetic: the wizard dereferences
 * `selectedData[item.index]`, so an index that names the wrong entry marks the
 * wrong row red and an out-of-range one throws inside the wizard.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every entity is `autotest_*`-named and deleted in a `finally`,
 * including the rows a PARTLY failed import wrote — those are the ones most
 * easily forgotten, because the request they came from reported a failure.
 * The fork case deletes its copy out of the second project it wrote into.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createAgentWithVersion,
  createGithubToolkit,
  deleteGithubToolkit,
  resolvePublishAuthorProjectId,
  type GithubToolkitFixture,
} from '../../fixtures/api';
import {
  FIXTURE_MODEL,
  attachToolkitToVersion,
  bundleToImportEntities,
  defaultVersionOf,
  deleteApplication,
  deleteApplicationIn,
  exportBundle,
  forkBundle,
  importWizard,
  readApplication,
  readApplicationIn,
  variablesAsRecord,
  type ImportAnswer,
} from '../../fixtures/exportImport';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

/** A name nobody else in the run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/** One agent entity for the wizard, with a single version and the given tool refs. */
function agentEntity(
  name: string,
  options: { readonly toolImportUuid?: string; readonly versions?: unknown[] } = {},
): Record<string, unknown> {
  const entity: Record<string, unknown> = {
    entity: 'agents',
    name,
    description: 'An agent submitted by the import-wizard journey.',
    original_exported: true,
    import_uuid: `${name}-app`,
  };
  entity['versions'] = options.versions ?? [
    {
      name: 'base',
      import_version_uuid: `${name}-base`,
      agent_type: 'openai',
      instructions: 'You are an agent built by the import-wizard journey.',
      llm_settings: { model_name: FIXTURE_MODEL, temperature: 0.7, max_tokens: 1024 },
      meta: { step_limit: 25, internal_tools: [] },
      variables: [],
      tags: [],
      conversation_starters: [],
      welcome_message: '',
      tools:
        options.toolImportUuid === undefined
          ? []
          : [{ import_uuid: options.toolImportUuid, selected_tools: [] }],
    },
  ];
  return entity;
}

/** One toolkit entity for the wizard. */
function toolkitEntity(
  name: string,
  overrides: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    entity: 'toolkits',
    import_uuid: `${name}-tk`,
    name,
    type: 'github',
    description: 'A toolkit submitted by the import-wizard journey.',
    settings: { repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo` },
    ...overrides,
  };
}

/** Every agent id an answer reports, so a `finally` can remove them. */
function importedAgentIds(answer: ImportAnswer): readonly string[] {
  return (answer.body.result?.agents ?? []).map((agent) => agent.id);
}

/** Every toolkit id an answer reports. */
function importedToolkitIds(answer: ImportAnswer): readonly string[] {
  return (answer.body.result?.toolkits ?? []).map((toolkit) => toolkit.id);
}

/**
 * The toolkit rows one stored agent's default version holds, as the agent
 * editor reloads them.
 *
 * The read is the authority here, not the write's echo: the answer that names
 * a link and the row that holds it are two different things, and this suite
 * exists because the first one used to be able to lie about the second.
 */
function storedToolsOf(stored: { readonly version_details?: unknown }): readonly Record<string, unknown>[] {
  const details = (stored.version_details ?? {}) as Record<string, unknown>;
  return (details['tools'] as readonly Record<string, unknown>[] | undefined) ?? [];
}

/** Remove a toolkit row the import created. Best effort — it runs in a `finally`. */
async function deleteImportedToolkit(
  request: Parameters<typeof deleteApplication>[0],
  id: string,
): Promise<void> {
  await request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`);
}

/* ── the multi-entity error contracts ─────────────────────────────────────── */

test('IMP-02: a sub-agent toolkit pointing at an import id nobody sent is reported by index', async ({
  request,
}) => {
  const agentName = autotestName('imp02_agent');
  const toolkitName = autotestName('imp02_tk');
  let answer: ImportAnswer | undefined;
  try {
    // A toolkit of type `application` is a SUB-AGENT reference: its
    // `settings.import_uuid` names an agent in the same bundle, which the
    // import resolves to the row it just wrote. This one names an agent the
    // body does not carry.
    answer = await importWizard(request, [
      agentEntity(agentName),
      toolkitEntity(toolkitName, {
        type: 'application',
        settings: { import_uuid: `${AUTOTEST_PREFIX}absent_agent` },
      }),
    ]);

    // 207, not 400: the agent DID import. A body that reported the whole
    // request as refused would tell the user to send it again and make a
    // second copy of the agent.
    expect(answer.status, `the import answered ${answer.text.slice(0, 400)}`).toBe(207);
    expect(answer.body.result?.agents ?? []).toHaveLength(1);
    expect(answer.body.result?.toolkits ?? [], 'the unresolvable toolkit was written anyway').toHaveLength(0);

    const reported = answer.body.errors?.toolkits ?? [];
    expect(reported, 'the fault was not reported on the toolkits channel').toHaveLength(1);
    expect(reported[0]?.name).toBe(toolkitName);
    // The toolkit is the SECOND entry of the body. The wizard marks the row it
    // finds at this index.
    expect(reported[0]?.index).toBe(1);
    expect(reported[0]?.msg).toContain('Unable to link toolkit_import_uuid');
    expect(reported[0]?.msg).toContain(`${AUTOTEST_PREFIX}absent_agent`);
    // The agent is not blamed for the toolkit's own fault.
    expect(answer.body.errors?.agents ?? []).toHaveLength(0);
  } finally {
    for (const id of importedAgentIds(answer ?? { status: 0, body: {}, text: '' })) {
      await deleteApplication(request, id);
    }
  }
});

test('IMP-03: an agent referencing a toolkit the bundle does not carry is reported, not dropped', async ({
  request,
}) => {
  const agentName = autotestName('imp03_agent');
  let answer: ImportAnswer | undefined;
  try {
    answer = await importWizard(request, [
      agentEntity(agentName, { toolImportUuid: `${AUTOTEST_PREFIX}absent_toolkit` }),
    ]);

    expect(answer.status, `the import answered ${answer.text.slice(0, 400)}`).toBe(207);
    const imported = answer.body.result?.agents ?? [];
    expect(imported, 'the agent itself must still import').toHaveLength(1);

    const reported = answer.body.errors?.agents ?? [];
    expect(reported, 'the unresolvable reference was dropped in silence').toHaveLength(1);
    expect(reported[0]?.index).toBe(0);
    expect(reported[0]?.name).toBe(agentName);
    expect(reported[0]?.msg).toContain('unable to link tools');

    // The answer must not name a link that has no row: the agent came back
    // with no toolkit, and it says so.
    expect(imported[0]?.version_details?.tools ?? []).toStrictEqual([]);
    const stored = await readApplication(request, imported[0]?.id ?? '');
    expect(stored.name).toBe(agentName);
    expect(storedToolsOf(stored)).toStrictEqual([]);
  } finally {
    for (const id of importedAgentIds(answer ?? { status: 0, body: {}, text: '' })) {
      await deleteApplication(request, id);
    }
  }
});

test('IMP-04: a broken agent does not stop the toolkit beside it from importing', async ({
  request,
}) => {
  const brokenName = autotestName('imp04_broken');
  const toolkitName = autotestName('imp04_tk');
  let answer: ImportAnswer | undefined;
  try {
    // An agent with no versions is nothing the platform can write: an
    // application row with no version is unreadable and unrunnable. It is
    // refused, and the toolkit sent with it is not.
    answer = await importWizard(request, [
      { entity: 'agents', name: brokenName, description: 'no versions at all', versions: [] },
      toolkitEntity(toolkitName),
    ]);

    expect(answer.status, `the import answered ${answer.text.slice(0, 400)}`).toBe(207);
    const toolkits = answer.body.result?.toolkits ?? [];
    expect(toolkits, 'the good toolkit was refused along with the broken agent').toHaveLength(1);
    expect(toolkits[0]?.name).toBe(toolkitName);
    expect(toolkits[0]?.type).toBe('github');

    const reported = answer.body.errors?.agents ?? [];
    expect(reported).toHaveLength(1);
    expect(reported[0]?.index).toBe(0);
    expect(reported[0]?.name).toBe(brokenName);
    expect(reported[0]?.msg).toContain('no versions provided');
    expect(answer.body.result?.agents ?? [], 'the refused agent left a row behind').toHaveLength(0);
  } finally {
    for (const id of importedToolkitIds(answer ?? { status: 0, body: {}, text: '' })) {
      await deleteImportedToolkit(request, id);
    }
  }
});

test('IMP-05: a bundle whose only entity names an unknown type is refused outright', async ({
  request,
}) => {
  const name = autotestName('imp05_unknown');
  // An `entity` the handler does not know takes the AGENT branch — the
  // handler switches on `toolkits` and `skills` and treats everything else as
  // an agent — so the entry is judged by the agent contract and refused for
  // carrying nothing an agent can be made of. The refusal is what the case is
  // about; the message names the contract that refused it.
  const answer = await importWizard(request, [
    { entity: 'quantum_entity', name, description: 'an entity type nothing serves' },
  ]);

  // 400, not 207: nothing at all was imported, so there is no partial success
  // to report.
  expect(answer.status, `the import answered ${answer.text.slice(0, 400)}`).toBe(400);
  expect(answer.body.result?.agents ?? []).toHaveLength(0);
  expect(answer.body.result?.toolkits ?? []).toHaveLength(0);
  expect(answer.body.result?.skills ?? []).toHaveLength(0);

  const reported = answer.body.errors?.agents ?? [];
  expect(reported, 'a refused body must still say which entry it refused').toHaveLength(1);
  expect(reported[0]?.index).toBe(0);
  expect(reported[0]?.name).toBe(name);

  // …and it wrote nothing. `count()` on the server's own list, because the
  // question is about absence.
  const listed = await request.get(
    `${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}?search=${encodeURIComponent(name)}`,
  );
  expect(listed.ok(), `the agent list answered ${listed.status()}`).toBe(true);
  const rows = ((await listed.json()) as { rows?: readonly { name?: string }[] }).rows ?? [];
  expect(rows.filter((row) => row.name === name)).toHaveLength(0);
});

/* ── the round trip that names its toolkit ────────────────────────────────── */

test('IMP-06: the import names each toolkit it linked, with the toolkit’s own name and type', async ({
  request,
}) => {
  /*
   * The defect this case is the acceptance test for: the link phase described
   * every toolkit it attached as `{"type": "custom", "name": ""}`, a pair of
   * literals, whatever the toolkit was. `result.toolkits` — built one phase
   * earlier out of the same two values — named it correctly, so ONE response
   * carried both the truth and a placeholder for it, and the wizard shows the
   * version summary. An import of a `github` toolkit reported an unnamed
   * `custom` one on the agent it had just attached it to.
   *
   * It is asserted on a bundle this platform EXPORTED, so the case also states
   * the whole promise the two halves make together: an agent with a toolkit
   * leaves as a file and comes back as an agent with that toolkit.
   */
  const name = autotestName('imp06');
  let sourceId: string | undefined;
  let toolkit: GithubToolkitFixture | undefined;
  let answer: ImportAnswer | undefined;
  try {
    const agent = await createAgentWithVersion(request, name, {
      instructions: 'Answer questions about the repository.',
      model: { modelName: FIXTURE_MODEL },
      variables: [{ name: 'repository', value: 'autotest' }],
    });
    sourceId = agent.id;
    toolkit = await createGithubToolkit(request, DEFAULT_PROJECT_ID, `${name}_tk`, {});
    await attachToolkitToVersion(request, toolkit.toolkitId, {
      applicationId: agent.id,
      versionId: agent.versionId,
      selectedTools: ['get_issue'],
    });

    const exported = await exportBundle(request, agent.id);
    answer = await importWizard(request, bundleToImportEntities(exported.bundle));
    expect(answer.status, `the import answered ${answer.text.slice(0, 400)}`).toBe(201);

    const importedToolkits = answer.body.result?.toolkits ?? [];
    expect(importedToolkits, 'the bundle’s toolkit did not import').toHaveLength(1);
    const importedAgents = answer.body.result?.agents ?? [];
    expect(importedAgents).toHaveLength(1);

    const linked = importedAgents[0]?.version_details?.tools ?? [];
    expect(linked, 'the imported version reports no toolkit').toHaveLength(1);
    // The three keys, against the toolkit the SAME answer reports one channel
    // over. A summary that names no toolkit is what this case exists to catch.
    expect(linked[0]?.id).toBe(importedToolkits[0]?.id);
    expect(linked[0]?.name).toBe(toolkit.toolkitName);
    expect(linked[0]?.type).toBe('github');

    // …and the copy really holds it, read back through the route the agent
    // editor reloads through rather than out of the write's own echo.
    const stored = await readApplication(request, importedAgents[0]?.id ?? '');
    const storedTools = storedToolsOf(stored);
    expect(storedTools, 'the import answered a link the database does not hold').toHaveLength(1);
    expect(String(storedTools[0]?.['tool_id'] ?? '')).toBe(importedToolkits[0]?.id);
    expect(storedTools[0]?.['selected_tools']).toStrictEqual(['get_issue']);
    expect(variablesAsRecord(stored.version_details)).toStrictEqual({ repository: 'autotest' });
  } finally {
    for (const id of importedAgentIds(answer ?? { status: 0, body: {}, text: '' })) {
      await deleteApplication(request, id);
    }
    for (const id of importedToolkitIds(answer ?? { status: 0, body: {}, text: '' })) {
      await deleteImportedToolkit(request, id);
    }
    await deleteGithubToolkit(request, DEFAULT_PROJECT_ID, toolkit);
    if (sourceId !== undefined) await deleteApplication(request, sourceId);
  }
});

/* ── the fork ─────────────────────────────────────────────────────────────── */

test('FRK-01: forking into another project copies the agent, rewrites the model project and records the parent', async ({
  request,
}) => {
  /*
   * CROSS-PROJECT, and that is the whole case. A fork into the SAME project
   * cannot tell the two rewrites apart from doing nothing: the model project
   * it writes and the parent project it records are both the project the agent
   * already lived in. The destination is the seeded author project, resolved
   * by NAME through the product's own project listing.
   *
   * The fork is also not the import. The route was once served by the import
   * handler, and a copy made that way kept `llm_settings.model_project_id`
   * pointing at the SOURCE project — a model the new owner may not be able to
   * reach — and recorded nothing about where it came from, so the read path
   * reported `is_forked: false` on a fork.
   */
  const name = autotestName('frk01');
  const destinationProjectId = await resolvePublishAuthorProjectId(request);
  expect(
    destinationProjectId,
    'the fork case needs a second project; project 1 cannot prove a cross-project copy',
  ).not.toBe(DEFAULT_PROJECT_ID);

  let sourceId: string | undefined;
  let copyId: string | undefined;
  try {
    const agent = await createAgentWithVersion(request, name, {
      instructions: 'You are the agent this journey forks into another project.',
      welcomeMessage: 'Ask me about the fork.',
      conversationStarters: ['What was I forked from?'],
      variables: [{ name: 'origin', value: 'autotest' }],
      // The model belongs to the SOURCE project, which is the value the fork
      // has to rewrite.
      model: { modelName: FIXTURE_MODEL, modelProjectId: DEFAULT_PROJECT_ID, temperature: 0.3 },
    });
    sourceId = agent.id;

    // The document the Fork button reads — the fork flavour, which is the only
    // one carrying the `owner_id` the copy records as its parent project.
    const exported = await exportBundle(request, agent.id, { fork: true });
    const answer = await forkBundle(request, destinationProjectId, exported.bundle);
    expect(answer.status, `the fork answered ${answer.text.slice(0, 400)}`).toBe(201);

    const forked = answer.body.result?.agents ?? [];
    expect(forked, 'the fork wrote no agent').toHaveLength(1);
    copyId = forked[0]?.id;
    expect(copyId, 'the fork answered no id').toBeDefined();
    expect(copyId).not.toBe(agent.id);
    expect(forked[0]?.owner_id).toBe(destinationProjectId);
    expect(forked[0]?.version_details?.is_forked).toBe(true);

    // Read the copy back out of the DESTINATION project. The echo alone cannot
    // say a row exists there — a fork that answered a body and wrote nothing
    // would satisfy every assertion above.
    const copy = await readApplicationIn(request, destinationProjectId, copyId ?? '');
    expect(copy.name).toBe(name);
    const copiedVersion = defaultVersionOf(copy);
    expect(copiedVersion.instructions).toBe(
      'You are the agent this journey forks into another project.',
    );
    expect(copiedVersion.welcome_message).toBe('Ask me about the fork.');
    expect(copiedVersion.conversation_starters).toStrictEqual(['What was I forked from?']);
    expect(variablesAsRecord(copiedVersion)).toStrictEqual({ origin: 'autotest' });

    // THE REWRITE. The copy runs on a model published in the project that now
    // owns it, not on one in the project it came from.
    expect(copiedVersion.llm_settings?.['model_name']).toBe(FIXTURE_MODEL);
    expect(
      String(copiedVersion.llm_settings?.['model_project_id'] ?? ''),
      'the copy still points at the source project’s model',
    ).toBe(destinationProjectId);

    // THE PROVENANCE. Three keys, and each one answers a different question:
    // which agent this came from, which project it came from, and who made the
    // copy. `is_forked` on every read is derived from the first of them.
    const meta = (copiedVersion.meta ?? {}) as Record<string, unknown>;
    expect(String(meta['parent_entity_id'] ?? '')).toBe(agent.id);
    expect(String(meta['parent_project_id'] ?? '')).toBe(DEFAULT_PROJECT_ID);
    expect(String(meta['parent_author_id'] ?? ''), 'the copy records no author').not.toBe('');

    // The original is untouched: a fork copies, it does not move.
    const original = await readApplication(request, agent.id);
    expect(original.name).toBe(name);
    expect(
      String(defaultVersionOf(original).llm_settings?.['model_project_id'] ?? ''),
      'the fork rewrote the SOURCE agent’s model project',
    ).toBe(DEFAULT_PROJECT_ID);
  } finally {
    if (copyId !== undefined) await deleteApplicationIn(request, destinationProjectId, copyId);
    if (sourceId !== undefined) await deleteApplication(request, sourceId);
  }
});
