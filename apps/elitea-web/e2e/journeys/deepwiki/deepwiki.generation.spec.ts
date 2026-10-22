/**
 * DWIKI-005 / 006 / 010 / 011 — the journeys that MUTATE toolkit 9002.
 *
 * Serial on purpose: generation produces the wiki that delete removes, and
 * settings are saved before the run so the run carries them. Everything on
 * the platform side is real — the facade's vault credential resolution, the
 * mTLS hop, the callback bearer, the upload into the bucket; the provider
 * runs its fixture engine (ELITEA_DEEPWIKI_RUNNER=fixture), which is the
 * real runner with canned tool results.
 *
 * DWIKI-005 and 006 act on a run IN FLIGHT (stop it, reload over it), so the
 * fixture's run must outlast those interactions. scripts/deepwiki-e2e.sh
 * paces it at 3s a step (~15s a run): at 1s the whole run fit inside one slow
 * invoke + poll round trip on CI, the first poll drained every step AND the
 * terminal status, the panel went idle → completed without ever showing
 * running, and that run's wiki LANDED.
 *
 * A RETRY RESTARTS THE FILE, so it must not inherit an earlier attempt's
 * wiki or run: with the landed wiki above, every retried DWIKI-005 met the
 * "regenerate?" confirm and failed by construction, hiding the real failure.
 * beforeAll removes 9002's wiki; afterEach stops a run a failed test left going.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { SEEDED, clearWiki, listKeys, openDeepWiki, readObject, replaceEditorText } from './helpers';

const TOOLKIT_PATH = `/app/deepwiki/${SEEDED.mutable.toolkitId}`;
const GENERATION_TIMEOUT = 90_000;
const INVOKE_PATH = `/api/v2/deepwiki/tools/${SEEDED.projectId}/wikis/generate_wiki/invoke`;

test.describe.configure({ mode: 'serial' });

test.describe('DeepWiki generation', () => {
  // A provider round trip through the facade, plus the fixture's paced steps:
  // longer than Playwright's 30s default, which killed a passing answer mid-poll.
  test.setTimeout(120_000);

  test.use({ storageState: STORAGE_STATE.member });

  test.beforeAll(async ({ browser }) => {
    const context = await browser.newContext({ storageState: STORAGE_STATE.member });
    try {
      await clearWiki(context.request, SEEDED.mutable.wikiId);
    } finally {
      await context.close();
    }
  });

  // The generation this test started, read off the invoke response.
  let startedInvocation: string | null = null;
  test.beforeEach(({ page }) => {
    startedInvocation = null;
    page.on('response', (response) => {
      if (response.request().method() !== 'POST' || !response.url().endsWith(INVOKE_PATH)) return;
      void response
        .json()
        .then((body: { invocation_id?: string; data?: { invocation_id?: string } }) => {
          startedInvocation = body.invocation_id ?? body.data?.invocation_id ?? null;
        })
        .catch(() => undefined);
    });
  });
  test.afterEach(async ({ page }, testInfo) => {
    if (startedInvocation === null || testInfo.status === testInfo.expectedStatus) return;
    // A failed test can leave its run going; finished, it would land a wiki
    // the next attempt trips over. Stopped, it lands nothing.
    await page.request.delete(
      `${BASE_URL}/api/v2/deepwiki/invocations/${SEEDED.projectId}/wikis/generate_wiki/${startedInvocation}`,
      { failOnStatusCode: false },
    );
  });

  test('DWIKI-010: toolkit settings are edited and saved, and a bad draft is refused against the field', async ({
    page,
  }) => {
    await openDeepWiki(page, TOOLKIT_PATH);
    await page.getByTestId('wiki-settings-toggle').click();
    await expect(page.getByTestId('wiki-settings-panel')).toBeVisible();

    // The refusal names the FIELD, not the document: a draft with no repository
    // is reported as it is typed, and Save is disabled until it is fixed — so
    // there is no click here. (A click was a race: on a fast machine it landed
    // inside the editor's 30ms debounce and hit a still-enabled button; on the
    // CI runner Playwright waited two minutes for a button that stays disabled.)
    await replaceEditorText(page, JSON.stringify({ branch: 'main', llm_model: 'gpt-4o-mini', code_toolkit: 9010 }));
    await expect(page.getByTestId('wiki-settings-problem')).toHaveAttribute('data-field', 'repository');
    await expect(page.getByTestId('wiki-settings-save')).toBeDisabled();
    await expect(page.getByTestId('wiki-settings-saved')).toHaveCount(0);

    // A good draft saves, and survives a reload — it was PUT, not kept in memory.
    const settings = {
      repository: SEEDED.mutable.repository,
      branch: 'main',
      llm_model: 'gpt-4o-mini',
      code_toolkit: 9010,
      planner_type: 'cluster',
    };
    await replaceEditorText(page, JSON.stringify(settings, null, 2));
    await page.getByTestId('wiki-settings-save').click();
    await expect(page.getByTestId('wiki-settings-saved')).toBeVisible({ timeout: 15_000 });

    await page.reload({ waitUntil: 'networkidle' });
    await page.getByTestId('wiki-settings-toggle').click();
    await expect(page.locator('.cm-content').last()).toContainText('"planner_type": "cluster"');
  });

  test('DWIKI-005: generation streams progress and can be stopped', async ({ page }) => {
    await openDeepWiki(page, TOOLKIT_PATH);
    await page.getByTestId('wiki-generate').click();
    // No wiki exists for 9002 (beforeAll), so there is no "regenerate?" confirm.
    await expect(page.getByTestId('wiki-generation-status')).toHaveAttribute('data-status', 'running', {
      timeout: 20_000,
    });
    // The provider's progress events reach the log verbatim.
    await expect(page.getByTestId('wiki-generation-log')).toContainText('Cloning the repository', { timeout: 20_000 });

    await page.getByTestId('wiki-generate-stop').click();
    // Stopped: the run leaves `running`, the Stop control goes away, and
    // nothing landed — a cancelled run must not half-write a wiki.
    await expect(page.getByTestId('wiki-generation-status')).not.toHaveAttribute('data-status', 'running', {
      timeout: 20_000,
    });
    await expect(page.getByTestId('wiki-generate-stop')).toHaveCount(0);
    await expect(page.getByTestId('wiki-generate')).toBeVisible();
    expect(await listKeys(page, SEEDED.mutable.wikiId)).toEqual([]);
  });

  test('DWIKI-006: generation state survives a page reload', async ({ page }) => {
    await openDeepWiki(page, TOOLKIT_PATH);
    await page.getByTestId('wiki-generate').click();
    await expect(page.getByTestId('wiki-generation-status')).toHaveAttribute('data-status', 'running', {
      timeout: 20_000,
    });

    await page.reload({ waitUntil: 'networkidle' });

    // Resumed from storage: still running, still stoppable, log still filling.
    await expect(page.getByTestId('wiki-generation-status')).toHaveAttribute('data-status', 'running', {
      timeout: 20_000,
    });
    await expect(page.getByTestId('wiki-generate-stop')).toBeVisible();
    await page.getByTestId('wiki-generate-stop').click();
    await expect(page.getByTestId('wiki-generate-stop')).toHaveCount(0, { timeout: 20_000 });
  });

  test('DWIKI-005b: a completed generation LANDS in the bucket and the browser reads it', async ({ page }) => {
    await openDeepWiki(page, TOOLKIT_PATH);
    await page.getByTestId('wiki-generate').click();
    await expect(page.getByTestId('wiki-generation-status')).toHaveAttribute('data-status', 'completed', {
      timeout: GENERATION_TIMEOUT,
    });
    await expect(page.getByTestId('wiki-generation-log')).toContainText('Uploaded 6 wiki objects');

    // What landed, read back through the API: the manifest and every page it names.
    const keys = await listKeys(page, SEEDED.mutable.wikiId);
    expect(keys).toEqual(
      expect.arrayContaining([
        `${SEEDED.mutable.wikiId}/wiki_manifest_fixture-1.json`,
        `${SEEDED.mutable.wikiId}/wiki_pages/overview/getting-started.md`,
        `${SEEDED.mutable.wikiId}/wiki_pages/architecture/request-flow.md`,
        `${SEEDED.mutable.wikiId}/wiki_pages/components/storage.md`,
      ]),
    );
    const page1 = await readObject(page, `${SEEDED.mutable.wikiId}/wiki_pages/overview/getting-started.md`);
    expect(page1.status).toBe(200);
    expect(page1.text).toContain(`Generated by the fixture runner for \`${SEEDED.mutable.repository}\``);

    // And the browser shows it without a reload: completion invalidated the wiki reads.
    await expect(page.getByText('e2e-generated wiki')).toBeVisible({ timeout: 20_000 });
    await page.getByText('wiki_pages/overview/getting-started.md').click();
    await expect(page.getByTestId('wiki-page-content').locator('h1')).toHaveText('Getting started');
  });

  test('DWIKI-011: a wiki and its artifacts can be deleted', async ({ page }) => {
    await openDeepWiki(page, TOOLKIT_PATH);
    await expect(page.getByText('e2e-generated wiki')).toBeVisible({ timeout: 20_000 });
    expect((await listKeys(page, SEEDED.mutable.wikiId)).length).toBeGreaterThan(0);

    await page.getByTestId('wiki-delete').click();
    await page.getByRole('dialog').getByRole('button', { name: /delete/i }).click();

    await expect(page.getByText(/No wiki has been generated/i)).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId('wiki-delete-partial')).toHaveCount(0);
    await expect(page.getByTestId('wiki-delete-error')).toHaveCount(0);
    expect(await listKeys(page, SEEDED.mutable.wikiId)).toEqual([]);
  });
});
