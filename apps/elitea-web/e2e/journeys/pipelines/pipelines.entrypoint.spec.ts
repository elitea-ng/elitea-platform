/**
 * The Entrypoint node's own "Trigger" surface (`TriggerTypeSelector`,
 * `src/features/pipelines/ui/settings/TriggerTypeSelector.tsx`), mounted by
 * `BaseNode/NodeCard.tsx` only `{isEntrypoint && <TriggerTypeSelector ... />}`.
 *
 * ── What #899 fixed, and what these tests therefore assert ────────────────
 *
 * Two independent gaps made the whole Schedule/Webhook half unreachable:
 *
 *  1. `backendCapabilities.ts` pinned `pipelineTriggers: false`, so
 *     `chatMessageOnly` was permanently true and the dropdown offered exactly
 *     one option. It had been pinned against pylon's DELETED
 *     `/elitea_core/pipeline_trigger/.../trigger` route — but the Go stack
 *     serves two REPLACEMENT facilities, `/pipeline_schedules/...` and
 *     `/pipeline_triggers/...` (`internal/api/v2/pipelinetriggers`), which
 *     the SPA was simply not speaking to.
 *  2. `NodeCard.tsx` never received a `triggerProps` value from any caller,
 *     so `projectId`/`versionId` were always `undefined`. They now ride
 *     `FlowEditorContext.triggerScope`, supplied by the editor — the one
 *     place that has them.
 *
 * The surface that replaced the baseline's single-valued trigger row is one
 * dropdown (what to ADD) plus a LIST of what is configured, because the two
 * Go facilities are independent and a pipeline may hold both at once. The
 * tests below drive exactly that: create a schedule, create a webhook and
 * reveal its secret, see both listed, delete both.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline, PIPELINE_STARTER_ENTRY_NODE_ID, type CreatedPipeline } from '../../fixtures/pipelines';

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline !== undefined) await deletePipeline(page.request, pipeline);
  }
});

/**
 * The Trigger selector carries no `data-testid` of its own. `NodeCard.tsx`
 * renders it — `{isEntrypoint && <TriggerTypeSelector .../>}` — as the FIRST
 * thing inside the node body, before `NodeAdmissionIssues` (which owns no
 * select) and before every node-type-specific field (`{children}`). On the
 * Entrypoint node the first `combobox` in the card is therefore always this
 * selector, never an LLM-node field that happens to sit beside it.
 */
function triggerCombobox(card: ReturnType<Page['locator']>): ReturnType<Page['locator']> {
  return card.getByRole('combobox').first();
}

async function openEditor(page: Page, pipeline: CreatedPipeline) {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  const card = page.locator(`.react-flow__node[data-id="${PIPELINE_STARTER_ENTRY_NODE_ID}"]`);
  await expect(card).toBeVisible({ timeout: 20_000 });
  const trigger = triggerCombobox(card);
  // The selector is disabled while its two reads are in flight — deliberately,
  // so a click cannot pick "Webhook" against a stale "no trigger yet" and
  // ROTATE a credential somebody else is already holding. Wait it out rather
  // than clicking into the gap (a click on a disabled MUI select is silently
  // swallowed, which is what an un-waited test would actually be asserting).
  await expect(trigger).not.toHaveAttribute('aria-disabled', 'true', { timeout: 20_000 });
  return trigger;
}

/*
 * elitea_issues: #6516, #6449, #5265, #5014, #5085, #5001, #6128, #4977,
 * #4975 — the trigger-type family. Schedule and Webhook are reachable again
 * as of #899, so the dropdown case below is real. The narrower cases those
 * ids name (a Schedule TIMEZONE conversion (#6516/#6449), a webhook secret
 * leaked by a `/webhook/prompt_lib/.../custom` endpoint (#5001), a
 * per-provider webhook type (#6128)) are moot in a different way now: the Go
 * schedule stores no timezone (it fires on the platform's clock), the
 * credential is minted server-side and revealed only through an operation
 * carrying the write permission, and the inbound route verifies ONE bearer
 * secret rather than a GitHub/GitLab/Custom signature mode.
 * onetest: ELITEA-0889, ELITEA-0865, ELITEA-0867, ELITEA-0868, ELITEA-0877,
 * ELITEA-0883, ELITEA-0886.
 */
test('the Trigger surface offers Schedule and Webhook, creates and lists both, and deletes them', async ({ page }) => {
  // Six round trips to two facilities plus the editor load — past the 30s
  // default on a cold canvas, and none of the individual waits is slow.
  test.setTimeout(120_000);

  const name = `${AUTOTEST_PREFIX}trig-opts-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const trigger = await openEditor(page, pipeline);

  // The selector is exclusive to the Entrypoint node.
  await expect(page.locator('.react-flow__node[data-id="END"]').getByRole('combobox')).toHaveCount(0);
  // "Chat Message" is the absence of both unattended facilities.
  await expect(trigger).toHaveText('Chat Message');

  // ── Schedule and Webhook are both offered ───────────────────────────────
  await trigger.click();
  await expect(page.getByRole('option', { name: 'Schedule' })).toBeVisible({ timeout: 5_000 });
  await expect(page.getByRole('option', { name: 'Webhook' })).toBeVisible({ timeout: 5_000 });

  // ── Create a schedule ───────────────────────────────────────────────────
  const scheduleSaved = page.waitForResponse(
    (response) => response.request().method() === 'PUT' && response.url().includes('/pipeline_schedules/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await page.getByRole('option', { name: 'Schedule' }).click();
  await expect(page.getByText('Schedule settings')).toBeVisible({ timeout: 10_000 });
  // The modal opens on a valid five-field default; Apply saves it as-is.
  await page.getByRole('button', { name: 'Apply' }).click();
  await scheduleSaved;
  await expect(page.getByTestId('pipeline-trigger-row-schedule')).toBeVisible({ timeout: 15_000 });

  // ── Create a webhook and reveal its secret ──────────────────────────────
  const triggerMinted = page.waitForResponse(
    (response) => response.request().method() === 'POST' && response.url().includes('/pipeline_triggers/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await trigger.click();
  await page.getByRole('option', { name: 'Webhook' }).click();
  await triggerMinted;
  await expect(page.getByText('Webhook settings')).toBeVisible({ timeout: 10_000 });
  // The create answered WITH the credential — it is shown, masked.
  const secretField = page.getByTestId('pipeline-webhook-secret');
  await expect(secretField).toBeVisible({ timeout: 10_000 });
  await expect(page.getByTestId('pipeline-webhook-url')).not.toHaveValue('');

  // Reveal fetches it again through the dedicated write-permission route.
  const revealed = page.waitForResponse(
    (response) => response.url().includes('/pipeline_triggers/secret/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await page.getByTestId('pipeline-webhook-reveal').click();
  await revealed;
  await page.getByLabel('Show secret').click();
  await expect(secretField).not.toHaveValue(/^•+$/);
  await page.getByRole('button', { name: 'Done' }).click();

  // ── Both are listed side by side ────────────────────────────────────────
  await expect(page.getByTestId('pipeline-trigger-row-schedule')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId('pipeline-trigger-row-webhook')).toBeVisible({ timeout: 15_000 });

  // ── Delete both ─────────────────────────────────────────────────────────
  const webhookRevoked = page.waitForResponse(
    (response) => response.request().method() === 'DELETE' && response.url().includes('/pipeline_triggers/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await page.getByTestId('pipeline-trigger-delete-webhook').click();
  await webhookRevoked;
  await expect(page.getByTestId('pipeline-trigger-row-webhook')).toHaveCount(0, { timeout: 15_000 });

  const scheduleDeleted = page.waitForResponse(
    (response) => response.request().method() === 'DELETE' && response.url().includes('/pipeline_schedules/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await page.getByTestId('pipeline-trigger-delete-schedule').click();
  await scheduleDeleted;
  await expect(page.getByTestId('pipeline-trigger-row-schedule')).toHaveCount(0, { timeout: 15_000 });
  await expect(trigger).toHaveText('Chat Message');
});

/* onetest: ELITEA-0866 — with Chat Message selected (the only reachable state), ordinary pipeline edits do not switch the trigger to anything else. */
test('the trigger stays Chat Message across ordinary pipeline edits and a reload', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}trig-stay-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const trigger = await openEditor(page, pipeline);
  await expect(trigger).toHaveText('Chat Message');

  // Add an LLM node (a non-interrupting node) and save — the kind of edit
  // ELITEA-0866/0867 both name as one that must never move the trigger.
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'LLM', exact: true }).click();
  const persisted = page.waitForResponse(
    (response) => response.request().method() === 'PUT' && response.url().includes('/version/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);

  await page.reload();
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  const reloadedCard = page.locator(`.react-flow__node[data-id="${PIPELINE_STARTER_ENTRY_NODE_ID}"]`);
  await expect(reloadedCard).toBeVisible({ timeout: 20_000 });
  await expect(triggerCombobox(reloadedCard)).toHaveText('Chat Message');
});
