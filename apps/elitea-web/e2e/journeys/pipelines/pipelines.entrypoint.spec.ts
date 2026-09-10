/**
 * The Entrypoint node's own "Trigger" dropdown (`TriggerTypeSelector`,
 * `src/features/pipelines/ui/settings/TriggerTypeSelector.tsx`), mounted by
 * `BaseNode/NodeCard.tsx` only `{isEntrypoint && <TriggerTypeSelector
 * {...triggerProps} />}`.
 *
 * ── The root cause every "entrypoint-trigger-type" case in this package
 *    traces back to ─────────────────────────────────────────────────────
 *
 * `NodeCard.tsx` never receives a `triggerProps` value from ANY caller
 * (`grep -rn 'triggerProps=' src/features/pipelines/` — zero hits), so the
 * selector always renders with `projectId`/`versionId`/`versionInstructions`
 * all `undefined`. That alone would only mean "the auto-reset-on-interactive
 * check never sees a real graph" — survivable, since `hasInteractiveElements`
 * then defaults `false`. What makes the WHOLE Schedule/Webhook half of this
 * feature unreachable in this build is independent of that gap:
 *
 *   `src/shared/config/backendCapabilities.ts`: `pipelineTriggers: false` —
 *   "the Go router registers no handler for [the pipeline-trigger endpoint]
 *   … chi answers `404 page not found` for it, in every profile" (that
 *   module's own doc comment, issues #192/#193).
 *
 * `TriggerTypeSelector.tsx`'s own `chatMessageOnly = hasInteractiveElements
 * || !hasBackendCapability('pipelineTriggers')` therefore evaluates to `true`
 * UNCONDITIONALLY, in every pipeline, forever — not only while a HITL/Printer
 * node is present. The dropdown always renders with exactly ONE selectable
 * option, "Chat Message". Every case that assumes a pipeline CAN be set to
 * Schedule or Webhook (auto-reset-back-to-Chat-Message on adding an
 * interactive node; Schedule/Webhook staying put across unrelated edits;
 * the Schedule modal's Default/Advanced modes; the Webhook modal's
 * Custom/GitHub/GitLab types and copy button) is unreachable through the UI
 * for the same one reason, pinned by the single test below:
 *
 *   ELITEA-0865, 0866 (only the "stays Chat Message" half is actually
 *   testable — see the second test), 0867, 0868, 0877, 0883, 0886, 0889.
 *
 * ELITEA-0871 (migration auto-assigns Chat Message) is moot for the same
 * reason and is `not-applicable.md` territory, not a bug to chase here.
 * ELITEA-0873/0879/0880/0881/0884 need an actual triggered RUN (schedule
 * firing, a real webhook POST reaching a running pipeline) — STREAM-DEFERRED,
 * tracked in the ledger, not here.
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
  return triggerCombobox(card);
}

/*
 * onetest: ELITEA-0889, ELITEA-0865, ELITEA-0867, ELITEA-0868, ELITEA-0877,
 * ELITEA-0883, ELITEA-0886 — product gap, see this file's own doc comment.
 * The Trigger dropdown IS mounted, exclusively on the Entrypoint node, and
 * DOES default to "Chat Message" (both real, both asserted below and left
 * PASSING) — but it can never be switched to Schedule or Webhook, so every
 * behaviour gated on having done that (reset-on-interactive-node, the
 * Schedule/Webhook config UI, OAuth-disables-the-option) is unreachable.
 */
test('the Trigger dropdown offers Schedule and Webhook alongside Chat Message — PRODUCT GAP', async ({ page }) => {
  test.fail(
    true,
    'ELITEA-0889: product gap — backendCapabilities.ts pins pipelineTriggers:false (no Go route for the pipeline-trigger endpoint in any profile), so TriggerTypeSelector.chatMessageOnly is permanently true and the dropdown only ever offers "Chat Message"',
  );

  const name = `${AUTOTEST_PREFIX}trig-opts-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const trigger = await openEditor(page, pipeline);

  // Real and passing: the selector is exclusive to the Entrypoint node.
  await expect(page.locator('.react-flow__node[data-id="END"]').getByRole('combobox')).toHaveCount(0);
  // Real and passing: it defaults to Chat Message.
  await expect(trigger).toHaveText('Chat Message');

  // Fails here: Schedule/Webhook are never offered.
  await trigger.click();
  await expect(page.getByRole('option', { name: 'Schedule' })).toBeVisible({ timeout: 5_000 });
  await expect(page.getByRole('option', { name: 'Webhook' })).toBeVisible({ timeout: 5_000 });
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
