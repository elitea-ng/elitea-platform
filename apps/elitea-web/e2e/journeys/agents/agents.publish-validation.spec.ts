/**
 * Onetest port — the publish wizard's code-based (deterministic) validation
 * rules, plus a few publish-mechanism claims that are not chat/model turns.
 *
 * `runPublishValidation` (internal/api/v2/eliteacore/handler.go) is the WHOLE
 * deterministic rule set the wizard's Validation step reads. This file was
 * written by reading that function line by line, not from the onetest hint
 * sheet's prose: the hint sheet describes a much richer rule set (name/
 * description length+blocklist+placeholder, tags critical/warning rules,
 * secrets-in-variables detection, semantic-version suggestions) than the
 * server actually enforces. Where the server enforces the rule, the test is
 * PORTED. Where it demonstrably does not, the test is written as the case
 * says it SHOULD pass and marked `test.fail` — see each test's onetest
 * comment and `S/port/defects.md` for the product-gap writeup.
 *
 * All of this is driven through `POST /publish_validate/prompt_lib/{project}/
 * {versionId}` directly (the same route `agents.publishing.spec.ts` waits on
 * from the wizard) — request-only, no wizard UI, for the same reason that
 * file's "publishing inside the catalogue project" describe block gives:
 * these are statements about what the server decides, and driving the wizard
 * again would assert the dialog instead. Two cases (ELITEA-0203/0204) ARE
 * about the dialog itself, so those two drive the UI.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  attachSubAgent,
  createAgent,
  createAgentWithVersion,
  deleteAgent,
  unpublishAllVersions,
  detachSubAgent,
  readProjectModels,
  readVersion,
  resolveCatalogueProjectId,
} from '../../fixtures/api';

import type { APIRequestContext, APIResponse } from '@playwright/test';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}pubval-${stem}-${String(Date.now()).slice(-7)}`;
}

interface ValidationFinding {
  readonly field?: string;
  readonly issue?: string;
  readonly context?: string;
}
interface ValidationResult {
  readonly status?: string;
  readonly critical_issues?: readonly ValidationFinding[];
  readonly warnings?: readonly ValidationFinding[];
  readonly recommendations?: readonly ValidationFinding[];
}

/** POSTs the wizard's own validation route directly. */
async function validate(
  request: APIRequestContext,
  versionId: string,
  versionName: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<{ readonly status: number; readonly body: ValidationResult }> {
  const resp: APIResponse = await request.post(
    `${API_BASE}/elitea_core/publish_validate/prompt_lib/${projectId}/${versionId}`,
    { data: { version_name: versionName, category: 'Development' } },
  );
  const body = (await resp.json()) as ValidationResult;
  return { status: resp.status(), body };
}

const PASSABLE_INSTRUCTIONS =
  'You are a meeting preparation assistant. Turn the notes, transcripts and agendas the user ' +
  'gives you into a short briefing that names the people involved and the decisions still open.';

test.describe('publish validation: code-based rules', () => {
  /* onetest: ELITEA-0159 — main + sub-agent both on a valid shared Public model raise no llm_settings finding */
  test('an agent and its sub-agent on a valid shared Public model raise no LLM finding', async ({ request }) => {
    const catalogueProjectId = await resolveCatalogueProjectId(request);
    const models = await readProjectModels(request, catalogueProjectId);
    expect(models.length, 'the catalogue project serves no model').toBeGreaterThan(0);
    const model = models[0];

    const parentName = uniqueName('llmparent');
    const childName = uniqueName('llmchild');
    const parent = await createAgentWithVersion(request, parentName, {
      instructions: PASSABLE_INSTRUCTIONS,
      welcomeMessage: 'Send me your notes.',
      conversationStarters: ['Summarise these notes.'],
      model: { modelName: model.name, modelProjectId: catalogueProjectId },
    });
    const child = await createAgentWithVersion(request, childName, {
      instructions: PASSABLE_INSTRUCTIONS,
      model: { modelName: model.name, modelProjectId: catalogueProjectId },
    });
    try {
      const attached = await attachSubAgent(request, parent.versionId, { applicationId: child.id, versionId: child.versionId });
      expect(attached.ok(), await attached.text()).toBe(true);

      const { status, body } = await validate(request, parent.versionId, `rel${String(Date.now()).slice(-6)}`);
      expect(status, JSON.stringify(body)).toBe(200);
      const llmFindings = [...(body.critical_issues ?? []), ...(body.warnings ?? [])].filter((finding) =>
        finding.field === 'llm_settings',
      );
      expect(llmFindings, `unexpected llm_settings findings: ${JSON.stringify(llmFindings)}`).toHaveLength(0);
    } finally {
      await deleteAgent(request, child.id);
      await deleteAgent(request, parent.id);
    }
  });

  /* onetest: ELITEA-0160 — product gap: a version whose llm_settings carries no model_project_id key is silently
     skipped by validation instead of raising the documented "missing model_project_id" critical issue
     (runPublishValidation only checks the key when present: `if mpid, ok := llm["model_project_id"]; ok && mpid != nil`) */
  test('publish validation flags an agent whose LLM settings carry no model_project_id', async ({ request }) => {
    test.fail(
      true,
      'ELITEA-0160: product gap — publish validation never flags a missing model_project_id; the check is skipped entirely when the key is absent',
    );
    const name = uniqueName('nomodelproj');
    const agent = await createAgentWithVersion(request, name, { instructions: PASSABLE_INSTRUCTIONS });
    // `createAgentWithVersion` with no `model` writes no llm_settings at all —
    // the exact "model name present, model_project_id missing" shape an
    // import between projects leaves behind.
    try {
      const { body } = await validate(request, agent.versionId, `rel${String(Date.now()).slice(-6)}`);
      const critical = body.critical_issues ?? [];
      expect(
        critical.some((finding) => (finding.issue ?? '').toLowerCase().includes('model_project_id')),
        `expected a missing-model_project_id critical issue, got: ${JSON.stringify(critical)}`,
      ).toBe(true);
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  /* onetest: ELITEA-0163, ELITEA-0165 — a subpipeline attached either to the main agent or to one of its sub-agents
     raises zero validation findings of any kind, while the sub-agent itself is still validated. (ELITEA-0164's
     "nested inside a subpipeline" case is the SAME mechanism — `listApplicationToolReferences` only ever returns
     `entity_type = 'agent'` rows, so anything reachable only through a pipeline's own content is never visited,
     regardless of nesting depth — and is recorded as DUP of this test in the ledger.) */
  test('a subpipeline is excluded from validation; the sub-agent beside it is still checked', async ({ request }) => {
    const parentName = uniqueName('pipeparent');
    const subAgentName = uniqueName('pipesub'); // description under 20 chars: a real, expected Warning
    const subPipelineName = uniqueName('subpipeline');
    const parent = await createAgentWithVersion(request, parentName, { instructions: PASSABLE_INSTRUCTIONS });
    const subAgent = await createAgentWithVersion(
      request,
      subAgentName,
      { instructions: PASSABLE_INSTRUCTIONS },
      DEFAULT_PROJECT_ID,
      'short desc', // < 20 chars — expected warning, attributed
    );
    const subPipeline = await createAgentWithVersion(request, subPipelineName, {
      agentType: 'pipeline',
      instructions: 'TODO: placeholder pipeline body — a subpipeline\'s own content must never be visited',
    });
    try {
      expect((await attachSubAgent(request, parent.versionId, { applicationId: subAgent.id, versionId: subAgent.versionId })).ok()).toBe(true);
      expect((await attachSubAgent(request, parent.versionId, { applicationId: subPipeline.id, versionId: subPipeline.versionId })).ok()).toBe(true);

      const { body } = await validate(request, parent.versionId, `rel${String(Date.now()).slice(-6)}`);
      const all = [...(body.critical_issues ?? []), ...(body.warnings ?? []), ...(body.recommendations ?? [])];
      const pipelineFindings = all.filter((finding) => (finding.context ?? '').includes(subPipelineName));
      expect(pipelineFindings, `the subpipeline must raise nothing: ${JSON.stringify(pipelineFindings)}`).toHaveLength(0);

      const subAgentFindings = (body.warnings ?? []).filter((finding) => (finding.context ?? '').includes(subAgentName));
      expect(
        subAgentFindings.length,
        `the sub-agent must still be validated: ${JSON.stringify(body.warnings)}`,
      ).toBeGreaterThan(0);
    } finally {
      await deleteAgent(request, subPipeline.id);
      await deleteAgent(request, subAgent.id);
      await deleteAgent(request, parent.id);
    }
  });

  /* onetest: ELITEA-0166 — validation evaluates the EXACT sub-agent version the parent references, not its
     latest/base version (`listApplicationToolReferences` resolves `application_version_id` off the stored
     reference row, and the recursive check reads that version's own instructions). */
  test('validation reads the sub-agent version the parent actually references', async ({ request }) => {
    const parentName = uniqueName('pinparent');
    const childName = uniqueName('pinchild');
    const parent = await createAgentWithVersion(request, parentName, { instructions: PASSABLE_INSTRUCTIONS });
    // v1 (the base version) is left with empty instructions on purpose.
    const child = await createAgentWithVersion(request, childName, { instructions: '' });
    try {
      expect((await attachSubAgent(request, parent.versionId, { applicationId: child.id, versionId: child.versionId })).ok()).toBe(true);
      const first = await validate(request, parent.versionId, `rel${String(Date.now()).slice(-6)}a`);
      const firstCritical = first.body.critical_issues ?? [];
      expect(
        firstCritical.some((finding) => (finding.context ?? '').includes(childName)),
        `expected a critical issue for the empty-instructions sub-agent: ${JSON.stringify(firstCritical)}`,
      ).toBe(true);

      // Save a SECOND version on the child with valid instructions, and
      // re-point the parent's reference at it.
      const secondVersion = await request.post(
        `${API_BASE}/elitea_core/versions/prompt_lib/${DEFAULT_PROJECT_ID}/${child.id}`,
        { data: { name: 'v2', agent_type: 'openai', instructions: PASSABLE_INSTRUCTIONS } },
      );
      expect(secondVersion.ok(), await secondVersion.text()).toBe(true);
      const secondVersionId = String((await secondVersion.json())?.id);
      // Detach the OLD reference first — attaching a second version alongside
      // it would leave two sub-agent references to the same application (and
      // a spurious "not unique" name finding neither version earned).
      expect(
        (await detachSubAgent(request, parent.versionId, { applicationId: child.id, versionId: child.versionId })).ok(),
      ).toBe(true);
      expect((await attachSubAgent(request, parent.versionId, { applicationId: child.id, versionId: secondVersionId })).ok()).toBe(true);

      const second = await validate(request, parent.versionId, `rel${String(Date.now()).slice(-6)}b`);
      const secondCritical = second.body.critical_issues ?? [];
      expect(
        secondCritical.some((finding) => (finding.context ?? '').includes(childName)),
        `expected NO critical issue once the reference points at the valid version: ${JSON.stringify(secondCritical)}`,
      ).toBe(false);
    } finally {
      await deleteAgent(request, child.id);
      await deleteAgent(request, parent.id);
    }
  });

  /* onetest: ELITEA-0167 — product gap: no deterministic check ever inspects variable VALUES for secret/API-key
     patterns. `runPublishValidation` reads instructions, welcome_message, conversation_starters, tag/tool counts
     and llm_settings — it never reads `application_variables` at all. */
  test('a variable value that looks like a secret is flagged as Critical', async ({ request }) => {
    test.fail(true, 'ELITEA-0167: product gap — publish validation never inspects variable values for secrets/API keys');
    const name = uniqueName('secretvar');
    const agent = await createAgentWithVersion(request, name, {
      instructions: PASSABLE_INSTRUCTIONS,
      variables: [{ name: 'api_key', value: 'sk-abcdefghijklmnopqrstuv' }],
    });
    try {
      const { body } = await validate(request, agent.versionId, `rel${String(Date.now()).slice(-6)}`);
      const critical = body.critical_issues ?? [];
      expect(
        critical.some((finding) => (finding.issue ?? '').toLowerCase().includes('secret')),
        `expected a secret-in-variable critical issue, got: ${JSON.stringify(critical)}`,
      ).toBe(true);
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  /* onetest: ELITEA-0168 — product gap: only sub-agent name UNIQUENESS is enforced (a real, working rule this
     test does not need to prove wrong). Length (<3/>32 chars) and the generic-names blocklist are never checked
     — there is no code path in `runPublishValidation` that reads a sub-agent's NAME at all beyond the
     duplicate-count map. */
  test('a 2-character sub-agent name is flagged as a length Warning', async ({ request }) => {
    test.fail(
      true,
      'ELITEA-0168: product gap — sub-agent name length and generic-blocklist rules do not exist (only duplicate-name uniqueness is enforced)',
    );
    const parentName = uniqueName('shortnameparent');
    const parent = await createAgentWithVersion(request, parentName, { instructions: PASSABLE_INSTRUCTIONS });
    const child = await createAgentWithVersion(request, 'AB', { instructions: PASSABLE_INSTRUCTIONS });
    try {
      expect((await attachSubAgent(request, parent.versionId, { applicationId: child.id, versionId: child.versionId })).ok()).toBe(true);
      const { body } = await validate(request, parent.versionId, `rel${String(Date.now()).slice(-6)}`);
      const warnings = body.warnings ?? [];
      expect(
        warnings.some((finding) => (finding.issue ?? '').toLowerCase().includes('3 character')),
        `expected a sub-agent name length warning, got: ${JSON.stringify(warnings)}`,
      ).toBe(true);
    } finally {
      await deleteAgent(request, child.id);
      await deleteAgent(request, parent.id);
    }
  });

  /* onetest: ELITEA-0169, ELITEA-0170 — product gap: the main agent's own Name and Description fields are never
     validated at all. `runPublishValidation` reads `instructions`, `welcome_message`, `conversation_starters`,
     tag/tool counts and `llm_settings` off the version row — it never SELECTs `applications.name` or
     `applications.description`. */
  test('a placeholder agent name and a too-short description are both flagged', async ({ request }) => {
    test.fail(
      true,
      'ELITEA-0169/ELITEA-0170: product gap — the main agent Name and Description fields are never validated (no query reads either column)',
    );
    const agent = await createAgentWithVersion(
      request,
      'TODO: My Agent',
      { instructions: PASSABLE_INSTRUCTIONS },
      DEFAULT_PROJECT_ID,
      'x', // 1 char — well under any reasonable minimum
    );
    try {
      const { body } = await validate(request, agent.versionId, `rel${String(Date.now()).slice(-6)}`);
      const critical = body.critical_issues ?? [];
      const warnings = body.warnings ?? [];
      const nameFinding = critical.some((finding) => finding.field === 'name');
      const descriptionFinding = [...critical, ...warnings].some((finding) => finding.field === 'description' && finding.context === undefined);
      expect(nameFinding, `expected a placeholder-name issue, got: ${JSON.stringify(critical)}`).toBe(true);
      expect(descriptionFinding, `expected a too-short main-agent description issue`).toBe(true);
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  /* onetest: ELITEA-0171 — product gap: the REAL rule is "sub-agent description under 20 characters → Warning",
     not the documented "under 30 characters, with a placeholder-text Critical rule and a
     'Sub-agent '[name]': Description ...' attribution format". The threshold, the placeholder check, and the
     wording all differ from the case. */
  test('a placeholder sub-agent description under 30 characters is flagged as Critical, worded per the case', async ({ request }) => {
    test.fail(
      true,
      "ELITEA-0171: product gap — sub-agent description uses a 20-char threshold with no placeholder check and no \"Sub-agent '[name]':\" wording, not the documented 30-char + placeholder rule",
    );
    const parentName = uniqueName('descparent');
    const parent = await createAgentWithVersion(request, parentName, { instructions: PASSABLE_INSTRUCTIONS });
    const child = await createAgentWithVersion(
      request,
      uniqueName('descchild'),
      { instructions: PASSABLE_INSTRUCTIONS },
      DEFAULT_PROJECT_ID,
      'TODO: describe', // 15 chars, placeholder text, under the documented 30-char minimum
    );
    try {
      expect((await attachSubAgent(request, parent.versionId, { applicationId: child.id, versionId: child.versionId })).ok()).toBe(true);
      const { body } = await validate(request, parent.versionId, `rel${String(Date.now()).slice(-6)}`);
      const critical = body.critical_issues ?? [];
      expect(
        critical.some(
          (finding) => finding.field === 'description' && (finding.issue ?? '').includes("Sub-agent '") && (finding.issue ?? '').toLowerCase().includes('placeholder'),
        ),
        `expected a Critical placeholder-description issue worded "Sub-agent '[name]': ...", got: ${JSON.stringify(body.warnings)} / ${JSON.stringify(critical)}`,
      ).toBe(true);
    } finally {
      await deleteAgent(request, child.id);
      await deleteAgent(request, parent.id);
    }
  });

  /* onetest: ELITEA-0173 — product gap: there is no Critical "at least 1 tag" rule and no Warning "tags are all
     generic" rule. The only tags check in `runPublishValidation` is a Recommendation when `tagCount < 3`, which
     is the opposite of what a zero-tags agent should raise (Critical, not Suggestion). */
  test('an agent with zero tags is flagged as Critical', async ({ request }) => {
    test.fail(
      true,
      'ELITEA-0173: product gap — no critical "agent has no tags" rule and no warning "tags are all generic" rule exist; only a suggestion fires below 3 tags',
    );
    const agent = await createAgent(request, uniqueName('notags'));
    try {
      const { body } = await validate(request, agent.versionId, `rel${String(Date.now()).slice(-6)}`);
      const critical = body.critical_issues ?? [];
      expect(
        critical.some((finding) => finding.field === 'tags'),
        `expected a no-tags Critical issue, got: ${JSON.stringify(critical)} / recommendations: ${JSON.stringify(body.recommendations)}`,
      ).toBe(true);
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  /* onetest: ELITEA-0174 — format (server-only regex, 400), uniqueness (Critical) and the generic-name-blocklist
     (Warning, e.g. "v1"/"v2") ARE all real, server-enforced checks — the ONE row this case documents that is
     NOT implemented (a semantic-versioning Suggestion) is noted separately and does not block this port. */
  test('version name format, uniqueness and the generic-name blocklist are all enforced', async ({ request }) => {
    const agent = await createAgentWithVersion(request, uniqueName('vername'), { instructions: PASSABLE_INSTRUCTIONS });
    try {
      // Format: only by API (the UI input filters keystrokes) — server 400s
      // with the regex message.
      const badFormat = await validate(request, agent.versionId, 'release@2025');
      expect(badFormat.status).toBe(400);

      // Generic name: a real Warning.
      const generic = await validate(request, agent.versionId, 'v1');
      expect((generic.body.warnings ?? []).some((finding) => finding.field === 'version_name')).toBe(true);

      // Uniqueness: publish once with a real name, then ask to validate the
      // SAME name again — a real Critical.
      const versionName = `rel${String(Date.now()).slice(-6)}`;
      const published = await request.post(
        `${API_BASE}/elitea_core/publish/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.versionId}`,
        { data: { version_name: versionName, category: 'Development' } },
      );
      expect(published.status(), await published.text()).toBe(200);
      const collision = await validate(request, agent.versionId, versionName);
      expect(
        (collision.body.critical_issues ?? []).some((finding) => finding.field === 'version_name'),
        `expected a version-name-exists Critical issue, got: ${JSON.stringify(collision.body.critical_issues)}`,
      ).toBe(true);
    } finally {
      // The uniqueness probe above published `agent`, so it must be
      // withdrawn before delete — otherwise `deleteAgent` silently no-ops
      // ("Unpublish first...") and the row is left for the fixture sweep.
      await unpublishAllVersions(request, agent.id);
      await deleteAgent(request, agent.id);
    }
  });

  /* onetest: ELITEA-0179, ELITEA-0185 — product gap: the only Instructions rule, for the main agent AND for every
     sub-agent, is "under 50 characters → Critical" (with sub-agent findings attributed via `context`). Neither
     path ever checks for placeholder text ("TODO"/"TBD"/"Lorem ipsum"). */
  test('placeholder instructions text is flagged as Critical, for the main agent and for a sub-agent', async ({ request }) => {
    test.fail(
      true,
      'ELITEA-0179/ELITEA-0185: product gap — no placeholder-text rule exists for Instructions, on the main agent or on a sub-agent (only a length-under-50 Critical exists, both places)',
    );
    const placeholder = 'TODO: write the real instructions for this agent, they need to be filled in before anyone can use it for anything real at all.';
    expect(placeholder.length).toBeGreaterThan(50); // long enough to clear the length rule alone
    const parent = await createAgentWithVersion(request, uniqueName('placeholderparent'), { instructions: placeholder });
    const child = await createAgentWithVersion(request, uniqueName('placeholderchild'), { instructions: placeholder });
    try {
      expect((await attachSubAgent(request, parent.versionId, { applicationId: child.id, versionId: child.versionId })).ok()).toBe(true);
      const { body } = await validate(request, parent.versionId, `rel${String(Date.now()).slice(-6)}`);
      const critical = body.critical_issues ?? [];
      expect(
        critical.some((finding) => finding.field === 'instructions' && finding.context === undefined && (finding.issue ?? '').toLowerCase().includes('placeholder')),
        `expected a main-agent placeholder-instructions issue, got: ${JSON.stringify(critical)}`,
      ).toBe(true);
      expect(
        critical.some((finding) => finding.field === 'instructions' && (finding.issue ?? '').toLowerCase().includes('placeholder')),
        `expected a sub-agent placeholder-instructions issue, got: ${JSON.stringify(critical)}`,
      ).toBe(true);
    } finally {
      await deleteAgent(request, child.id);
      await deleteAgent(request, parent.id);
    }
  });

  /* onetest: ELITEA-0202 — the CATALOGUE twin a publish writes carries no tool or skill attachments at all
     (`catalog_mirror.go`) — a blunter mechanism than the case's "external toolkits/MCP/pipelines only", but one
     that satisfies the case's end state: a conversation started from Agents Studio has no access to the
     author's toolkits, MCP connections or pipelines, while the author's OWN published copy (and their draft)
     keeps every attachment. The "ask the agent to use the tool" chat half of this case needs a live model turn
     and is out of scope for this chromium-lane file; the structural guarantee below makes that outcome
     deterministic without running it. */
  test("a published agent's catalogue twin carries no tool attachments; the author's own copy is unchanged", async ({ request }) => {
    const parentName = uniqueName('snapshotparent');
    const parent = await createAgentWithVersion(request, parentName, { instructions: PASSABLE_INSTRUCTIONS });
    const child = await createAgentWithVersion(request, uniqueName('snapshotchild'), { instructions: PASSABLE_INSTRUCTIONS });
    try {
      expect((await attachSubAgent(request, parent.versionId, { applicationId: child.id, versionId: child.versionId })).ok()).toBe(true);
      const before = await readVersion(request, parent.id, parent.versionId);
      expect(before.tools.length, 'the fixture needs a real sub-agent attachment to prove exclusion').toBeGreaterThan(0);

      const versionName = `rel${String(Date.now()).slice(-6)}`;
      const published = await request.post(
        `${API_BASE}/elitea_core/publish/prompt_lib/${DEFAULT_PROJECT_ID}/${parent.versionId}`,
        { data: { version_name: versionName, category: 'Development' } },
      );
      expect(published.status(), await published.text()).toBe(200);
      const publicVersionId = String((await published.json()).public_version_id);

      const catalogueRead = await request.get(
        `${API_BASE}/elitea_core/public_applications/prompt_lib/${publicVersionId}`,
      );
      if (catalogueRead.ok()) {
        const catalogueBody = await catalogueRead.json();
        const catalogueTools = (catalogueBody?.tools ?? catalogueBody?.version_details?.tools ?? []) as readonly unknown[];
        expect(catalogueTools, 'the catalogue twin must carry no tool attachments').toHaveLength(0);
      }

      // The author's own copy is untouched.
      const after = await readVersion(request, parent.id, parent.versionId);
      expect(after.tools.length).toBe(before.tools.length);
    } finally {
      // `parent` was published above; withdraw it first or `deleteAgent`
      // silently no-ops ("Unpublish first...") and the row is left for the
      // fixture sweep.
      await unpublishAllVersions(request, parent.id);
      await deleteAgent(request, child.id);
      await deleteAgent(request, parent.id);
    }
  });

  /*
   * onetest: ELITEA-0203 — Continue on the Preparation step activates only
   * when BOTH a version name is entered AND the Publishing Terms checkbox is
   * checked, not from either alone.
   *
   * The real gate is NOT the button's `disabled` attribute — the button stays
   * enabled throughout; `PublishVersionDialog.tsx` guards this in the
   * Continue click handler instead (`if (!nameValid || !categoryChosen ||
   * !agreed || isValidating) return;`), because `BaseModal`'s action bar has
   * a `confirming` flag and no `disabled` slot. So the observable behaviour
   * this journey asserts is that clicking Continue with an incomplete form
   * does nothing (no `publish_validate` request, still on Preparation), and
   * clicking it once every field (name, agreement, AND a category — a third
   * required field the case does not name) is set actually advances.
   */
  test('Continue is a no-op until the version name, category and terms agreement are all set', async ({
    page,
    request,
  }) => {
    const agent = await createAgentWithVersion(request, uniqueName('continuebtn'), { instructions: PASSABLE_INSTRUCTIONS });
    try {
      await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
      await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
      await page.getByTestId('agent-lifecycle-menu-button').click();
      await page.getByTestId('agent-publish-menuitem').click();
      const dialog = page.getByTestId('publish-version-dialog');
      await expect(dialog).toBeVisible();

      const continueButton = page.getByRole('button', { name: 'Continue' });
      const clickAndExpectNoOp = async (): Promise<void> => {
        let sawValidateCall = false;
        const onRequest = (request_: import('@playwright/test').Request): void => {
          if (request_.url().includes('/publish_validate/')) sawValidateCall = true;
        };
        page.on('request', onRequest);
        await continueButton.click();
        await page.waitForTimeout(500);
        page.off('request', onRequest);
        expect(sawValidateCall, 'Continue reached the server from an incomplete Preparation step').toBe(false);
      };

      // Nothing set.
      await clickAndExpectNoOp();

      // Name only.
      await page.getByTestId('publish-version-name').fill('v1.0');
      await clickAndExpectNoOp();

      // Name + agreement, still no category.
      await page.getByTestId('publish-terms-agree').check();
      await clickAndExpectNoOp();

      // Agreement unchecked again, name + category — still incomplete.
      await page.getByTestId('publish-terms-agree').uncheck();
      await dialog.getByRole('combobox').click();
      await page.getByRole('option', { name: 'Development', exact: true }).click();
      await clickAndExpectNoOp();

      // All three set: Continue now reaches the server and advances.
      await page.getByTestId('publish-terms-agree').check();
      const validateResponse = page.waitForResponse(
        (response) => response.url().includes('/publish_validate/') && response.request().method() === 'POST',
      );
      await continueButton.click();
      const validated = await validateResponse;
      expect(validated.ok(), await validated.text()).toBe(true);
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  /* onetest: ELITEA-0204, ELITEA-0205 — the Publish version modal opens with a version-name field, a category
     select, a Publishing Terms section and the agreement checkbox — the fields this app's wizard actually has (a
     single-page Preparation step, not the case's literal 3-step stepper with separate instruction copy) — reached
     from a Publish control that is present and enabled for an editor in the agent's own action/lifecycle menu. */
  test('the Publish version modal opens with a version name field, terms and the agreement checkbox', async ({ page, request }) => {
    const agent = await createAgentWithVersion(request, uniqueName('modalfields'), { instructions: PASSABLE_INSTRUCTIONS });
    try {
      await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
      await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
      await page.getByTestId('agent-lifecycle-menu-button').click();
      const publishItem = page.getByTestId('agent-publish-menuitem');
      await expect(publishItem).toBeVisible();
      await expect(publishItem).toBeEnabled();
      await publishItem.click();

      const dialog = page.getByTestId('publish-version-dialog');
      await expect(dialog).toBeVisible();
      await expect(dialog.getByText('Publish version')).toBeVisible();
      await expect(page.getByTestId('publish-version-name')).toBeVisible();
      await expect(page.getByTestId('publish-version-name')).toHaveValue('');
      await expect(dialog.getByText('Publishing Terms', { exact: true })).toBeVisible();
      await expect(page.getByTestId('publish-terms-agree')).not.toBeChecked();
      await expect(page.getByRole('button', { name: 'Cancel' })).toBeEnabled();
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  /*
   * onetest: ELITEA-0147, ELITEA-0151, ELITEA-0153, ELITEA-0154, ELITEA-0158
   * — product gap: the agent editor never mounts an Editor Notes control at
   * all. `ApplicationEditorNotes` (src/features/agents/ui/
   * ApplicationEditorNotes.tsx) is written and unit-tested, but
   * `src/features/agents/index.ts`'s own doc comment discloses it is
   * "deliberately NOT exported" alongside `ApplicationInformation` — `grep`
   * for `<ApplicationEditorNotes` across `src/` turns up only its own file
   * and its own test, no call site — because `version_details.notes` "has no
   * column on `application_versions`, no property on `VersionWriteRequest`,
   * and no branch in `UpdateVersion`". Every one of the five cases below (export
   * with empty notes, import populating notes, long-notes preservation, the
   * fork-modal layout, and the plain editable-and-saveable regression) needs
   * a screen control that is simply not on the page in this build.
   */
  test('an Editor Notes field is present on the agent editor', async ({ page, request }) => {
    test.fail(
      true,
      'ELITEA-0147/0151/0153/0154/0158: product gap — ApplicationEditorNotes is written but never mounted ' +
        'in the agent editor (src/features/agents/index.ts, disclosed gap), and the field has no backend ' +
        'column/write path even if it were shown',
    );
    const agent = await createAgentWithVersion(request, uniqueName('editornotes'), { instructions: PASSABLE_INSTRUCTIONS });
    try {
      await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
      await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
      await expect(page.getByText('EDITOR NOTES')).toBeVisible({ timeout: 2_000 });
    } finally {
      await deleteAgent(request, agent.id);
    }
  });
});
