/**
 * Journey 14b: publish an agent, see it under Published and in the Catalog,
 * unpublish it, fork it, import it back, and attach a skill to it.
 *
 * A SEPARATE file from `agents.lifecycle.spec.ts` on purpose. That file's J14
 * is the create-and-persist journey and its J15 the version journey; both are
 * owned elsewhere in this wave. Putting these here keeps the two edits apart.
 *
 * Every assertion below is unconditional. The predecessor of this coverage was
 * an `if (await publishButton.isVisible().catch(() => false))` block, which
 * passed for a year because no such control existed (#119, #120). The rule
 * that replaces it: assert the ROW, through the route the UI itself calls, and
 * never accept a control's presence as evidence that it does anything.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createAgent, deleteAgent } from '../../fixtures/api';

import type { APIRequestContext, Page } from '@playwright/test';

/** A name unique per run, so two runs on one stack cannot collide on the version-name UNIQUE. */
function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

/**
 * Creates an agent that CAN pass publish validation.
 *
 * `createAgent` seeds "You are a helpful assistant." — 28 characters — and the
 * server's own validation raises a CRITICAL issue below 50
 * (`runPublishValidation`, internal/api/v2/eliteacore/handler.go). An agent
 * built by the shared fixture therefore cannot be published at all, so the
 * publishable one is created here with instructions long enough to pass and
 * the starters and tags the warnings ask for.
 */
async function createPublishableAgent(
  request: APIRequestContext,
  name: string,
): Promise<{ readonly id: string; readonly versionId: string }> {
  const response = await request.post(`${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name,
      description: 'An agent created by the publishing journey, with enough content to pass validation.',
      type: 'agent',
      versions: [
        {
          name: 'base',
          agent_type: 'openai',
          instructions:
            'You are a meeting preparation assistant. Turn the notes, transcripts and agendas the user gives you into a short briefing that names the people involved, the decisions still open and the questions worth asking.',
          welcome_message: 'Send me your notes and I will prepare a briefing.',
          conversation_starters: ['Summarise these notes.', 'What should I ask in this meeting?'],
        },
      ],
    },
  });
  if (!response.ok()) {
    throw new Error(`createPublishableAgent: ${response.status()}: ${(await response.text()).slice(0, 300)}`);
  }
  const body = await response.json();
  return { id: String(body.id), versionId: String(body.version_details?.id) };
}

/** Opens the editor of an agent that already exists, on its `all` tab. */
async function openAgentEditor(page: Page, agentId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
}


/**
 * Chooses a publish category.
 *
 * REQUIRED by the wizard: ELITEA Catalog buckets published agents by
 * `meta.category` and renders no bucket for one that has none, so a publish
 * without a category is published and invisible.
 */
async function chooseCategory(page: Page, name = 'Development'): Promise<void> {
  await page.getByTestId('publish-category').click();
  await page.getByRole('option', { name, exact: true }).click();
}

async function openLifecycleMenu(page: Page): Promise<void> {
  await page.getByTestId('agent-lifecycle-menu-button').click();
}

/** Reads the catalogue through the route ELITEA Catalog itself calls. */
async function catalogNames(request: APIRequestContext): Promise<readonly string[]> {
  const response = await request.get(`${API_BASE}/elitea_core/public_applications/prompt_lib`);
  expect(response.ok(), `public_applications returned ${response.status()}`).toBe(true);
  const body = await response.json();
  const rows: readonly { readonly name?: string }[] = body?.rows ?? [];
  return rows.map((row) => row.name ?? '');
}

test.describe('J14b: the agent publish plane', () => {
  test('publish puts the agent under Published and into the Catalog; unpublish takes it out of both', async ({
    page,
    request,
  }) => {
    test.setTimeout(120_000);
    const name = uniqueName('publishable');
    const agent = await createPublishableAgent(request, name);
    const versionName = `rel${String(Date.now()).slice(-6)}`;

    try {
      await openAgentEditor(page, agent.id);

      await openLifecycleMenu(page);
      await page.getByTestId('agent-publish-menuitem').click();
      await expect(page.getByTestId('publish-version-dialog')).toBeVisible();

      await page.getByTestId('publish-version-name').fill(versionName);
      await chooseCategory(page);
      await page.getByTestId('publish-terms-agree').check();

      // Step 1 → 2. The wizard advances only when the validation ANSWER
      // arrives, so the status element is the proof the round trip happened.
      const validateResponse = page.waitForResponse(
        (response) => response.url().includes('/publish_validate/') && response.request().method() === 'POST',
      );
      await page.getByRole('button', { name: 'Continue' }).click();
      await validateResponse;
      await expect(page.getByTestId('publish-validation-status')).toBeVisible({ timeout: 20_000 });

      // Step 2 → 3, and the publish itself. The response is read directly
      // rather than inferred from a toast: the catalogue ids are what tell
      // the two halves of "published" apart.
      const publishResponse = page.waitForResponse(
        (response) =>
          /\/publish\/prompt_lib\//.test(response.url()) && response.request().method() === 'POST',
      );
      await page.getByRole('button', { name: 'Publish' }).click();
      const published = await publishResponse;
      expect(published.status(), await published.text()).toBe(200);
      const publishBody = await published.json();
      expect(publishBody.public_version_id, 'publish named no version').toBeTruthy();

      // (a) the agent's OWN project now holds a published version — this is
      // what the Published tab lists.
      const versionsResponse = await request.get(
        `${API_BASE}/elitea_core/application/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}`,
      );
      expect(versionsResponse.ok()).toBe(true);
      const detail = await versionsResponse.json();
      const versions: readonly { readonly name?: string; readonly status?: string }[] = detail?.versions ?? [];
      expect(
        versions.some((version) => version.name === versionName && version.status === 'published'),
        `no published version named ${versionName} in ${JSON.stringify(versions)}`,
      ).toBe(true);

      // (b) ELITEA Catalog serves it. Before the catalogue twin landed this
      // was the half that silently did not happen: the tab listed the agent
      // and the catalogue stayed empty, with no error on either side.
      await expect
        .poll(async () => (await catalogNames(request)).includes(name), { timeout: 20_000 })
        .toBe(true);

      // The category rides along on the catalogue row. Without it the hub
      // buckets the agent into a category it renders no chip for, so the
      // Catalog page shows "No agents found" over a row the API is serving.
      const catalogued = await request.get(`${API_BASE}/elitea_core/public_applications/prompt_lib`);
      const cataloguedBody = await catalogued.json();
      const row = (cataloguedBody.rows ?? []).find((candidate: { name?: string }) => candidate.name === name);
      expect(row?.meta?.category, 'the catalogue row carries no category').toBe('Development');

      // (c) the Published TAB shows it — the screen, not only the API.
      await page.goto(`${BASE_URL}/app/agents/published`);
      await expect(page.getByText(name).first()).toBeVisible({ timeout: 20_000 });

      // ── unpublish ────────────────────────────────────────────────────────
      await openAgentEditor(page, agent.id);
      // The version selector must be on the PUBLISHED version for the menu to
      // offer Unpublish; the editor opens on the default version, so the
      // published clone is selected through the URL.
      await page.goto(`${BASE_URL}/app/agents/all/${agent.id}/${String(publishBody.public_version_id)}`);
      await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });

      await openLifecycleMenu(page);
      const unpublishResponse = page.waitForResponse(
        (response) => response.url().includes('/unpublish/') && response.request().method() === 'POST',
      );
      await page.getByTestId('agent-unpublish-menuitem').click();
      const unpublished = await unpublishResponse;
      expect(unpublished.status(), await unpublished.text()).toBe(200);

      // Out of the catalogue again. An agent that stays there after its author
      // took it down is the one failure the publish terms promise cannot happen.
      await expect
        .poll(async () => (await catalogNames(request)).includes(name), { timeout: 20_000 })
        .toBe(false);
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  // The other half of the wizard, and the one a sparse agent actually meets.
  // `publish_validate` answers 422 on FAIL with the same body it uses for a
  // pass; rendered as a transport error, the author would be told "the request
  // failed" for the one case where it ran and said no.
  test('a sparse agent is refused by validation and never reaches publish', async ({ page, request }) => {
    const name = uniqueName('sparse');
    // The shared fixture's instructions are 28 characters. The server raises a
    // CRITICAL issue below 50, so this agent cannot be published — which is
    // exactly the state under test.
    const agent = await createAgent(request, name);

    try {
      await openAgentEditor(page, agent.id);
      await openLifecycleMenu(page);
      await page.getByTestId('agent-publish-menuitem').click();

      await page.getByTestId('publish-version-name').fill(`rel${String(Date.now()).slice(-6)}`);
      await chooseCategory(page);
      await page.getByTestId('publish-terms-agree').check();

      const validateResponse = page.waitForResponse(
        (response) => response.url().includes('/publish_validate/') && response.request().method() === 'POST',
      );
      await page.getByRole('button', { name: 'Continue' }).click();
      const validated = await validateResponse;
      expect(validated.status(), 'a sparse agent must FAIL validation').toBe(422);

      await expect(page.getByTestId('publish-validation-status')).toHaveText('FAIL');
      await expect(page.getByTestId('publish-validation-report')).toContainText('instructions are too short');

      // The button is still there; clicking it must reach no publish route.
      let publishCalls = 0;
      page.on('request', (request_) => {
        if (/\/publish\/prompt_lib\//.test(request_.url())) publishCalls += 1;
      });
      await page.getByRole('button', { name: 'Publish' }).click();
      await page.waitForTimeout(1_000);
      expect(publishCalls, 'a FAILed validation must not publish').toBe(0);
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  test('fork copies the agent into the chosen project', async ({ page, request }) => {
    const name = uniqueName('forkable');
    const agent = await createAgent(request, name);

    try {
      await openAgentEditor(page, agent.id);
      await openLifecycleMenu(page);
      await page.getByTestId('agent-fork-menuitem').click();
      await expect(page.getByTestId('fork-entity-dialog')).toBeVisible();

      // The dialog pre-selects the current project, which is the "copy into
      // the current project" case. The two requests are asserted in order:
      // the fork document is READ with `?fork=true`, then WRITTEN.
      const exportResponse = page.waitForResponse(
        (response) => response.url().includes('/export_import/') && response.url().includes('fork=true'),
      );
      const forkResponse = page.waitForResponse(
        (response) => /\/fork\/prompt_lib\//.test(response.url()) && response.request().method() === 'POST',
      );
      await page.getByRole('button', { name: 'Fork' }).click();
      await exportResponse;
      const forked = await forkResponse;
      expect([201, 207], await forked.text()).toContain(forked.status());

      // The copy exists as its own row. Two agents of this name, not one.
      await expect
        .poll(
          async () => {
            const list = await request.get(
              `${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}?agents_type=classic`,
            );
            const body = await list.json();
            const rows: readonly { readonly name?: string }[] = body?.rows ?? body?.items ?? [];
            return rows.filter((row) => row.name === name).length;
          },
          { timeout: 20_000 },
        )
        .toBeGreaterThan(1);
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  test('import round-trips an exported agent back into the project', async ({ page, request }) => {
    const name = uniqueName('exportable');
    const agent = await createAgent(request, name);

    try {
      // The file the user would pick: this app's own export, read through the
      // same route the Export button uses.
      const exported = await request.get(
        `${API_BASE}/elitea_core/export_import/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}`,
      );
      expect(exported.ok(), `export returned ${exported.status()}`).toBe(true);
      const document = await exported.text();

      await page.goto(`${BASE_URL}/app/agents/all`);
      await expect(page.getByTestId('agents-import-button')).toBeVisible({ timeout: 20_000 });

      await page.setInputFiles('[data-testid="agents-import-input"]', {
        name: 'agent.json',
        mimeType: 'application/json',
        buffer: Buffer.from(document, 'utf8'),
      });

      // The preview names what is about to be created — the step that lets
      // someone notice they picked the wrong file.
      await expect(page.getByTestId('agents-import-dialog')).toBeVisible({ timeout: 10_000 });
      await expect(page.getByTestId('agents-import-dialog').getByText(name)).toBeVisible();

      const importResponse = page.waitForResponse(
        (response) => response.url().includes('/import_wizard/') && response.request().method() === 'POST',
      );
      await page.getByRole('button', { name: 'Import' }).click();
      const imported = await importResponse;
      expect([200, 201, 207], await imported.text()).toContain(imported.status());

      await expect
        .poll(
          async () => {
            const list = await request.get(
              `${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}?agents_type=classic`,
            );
            const body = await list.json();
            const rows: readonly { readonly name?: string }[] = body?.rows ?? body?.items ?? [];
            return rows.filter((row) => row.name === name).length;
          },
          { timeout: 20_000 },
        )
        .toBeGreaterThan(1);
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  test('a created skill can be attached to an agent and is listed on its editor', async ({ page, request }) => {
    const agentName = uniqueName('skilled');
    const skillName = uniqueName('skill');
    const agent = await createAgent(request, agentName);

    const created = await request.post(`${API_BASE}/elitea_core/skills/prompt_lib/${DEFAULT_PROJECT_ID}`, {
      data: {
        name: skillName,
        description: 'attached by the e2e journey',
        versions: [{ name: 'base', instructions: 'Always answer in one sentence.' }],
      },
    });
    expect(created.ok(), `create skill returned ${created.status()}: ${await created.text()}`).toBe(true);
    const skill = await created.json();

    try {
      await openAgentEditor(page, agent.id);

      const section = page.getByTestId('agent-skills-section');
      await expect(section).toBeVisible({ timeout: 20_000 });
      await expect(page.getByTestId('agent-skills-counter')).toContainText('0/5');

      await page.getByTestId('agent-add-skill-button').click();
      const attachResponse = page.waitForResponse(
        (response) => /\/skill\/prompt_lib\//.test(response.url()) && response.request().method() === 'PATCH',
      );
      await page.getByTestId(`agent-skill-option-${String(skill.id)}`).click();
      const attached = await attachResponse;
      expect([200, 201], await attached.text()).toContain(attached.status());

      // The counter and the row, not just a 201: the read route is a different
      // route from the write, and #367 is the record of it answering with the
      // wrong rows at 200.
      await expect(page.getByTestId('agent-skills-counter')).toContainText('1/5', { timeout: 20_000 });
      await expect(page.getByTestId('agent-attached-skill')).toContainText(skillName);

      // It survives a reload, which is what proves the row was written and not
      // merely held in client state.
      await page.reload();
      await expect(page.getByTestId('agent-attached-skill')).toContainText(skillName, { timeout: 20_000 });

      // And the server's own read agrees.
      const readBack = await request.get(
        `${API_BASE}/elitea_core/application_skills/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.versionId}`,
      );
      expect(readBack.ok()).toBe(true);
      const listed = await readBack.json();
      const items: readonly { readonly name?: string }[] = listed?.items ?? [];
      expect(items.map((item) => item.name)).toContain(skillName);
    } finally {
      await request.delete(`${API_BASE}/elitea_core/skill/prompt_lib/${DEFAULT_PROJECT_ID}/${String(skill.id)}`);
      await deleteAgent(request, agent.id);
    }
  });
});
