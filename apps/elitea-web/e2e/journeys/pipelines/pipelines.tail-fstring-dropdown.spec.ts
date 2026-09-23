/**
 * Onetest wave-1 tail (T1b, folder `pipeline-nodes`) — the F-String
 * (`{variable}`) autocomplete dropdown (`FStringAutocompletePopper` /
 * `useFStringInputAutocomplete`, `src/features/pipelines/model/
 * useFStringInputAutocomplete.ts`). No existing journey drives this through
 * the real editor — `pipelines.hitl-node.spec.ts` only switches a field TO
 * F-String to unlock a sibling control, and `pipelines.yaml-view.spec.ts`
 * only reads a pre-seeded `fstring` value back — so every case below is a
 * genuine port, not a re-judgment of existing coverage.
 *
 * All four tests drive the STARTER pipeline's own `LLM_1` node
 * (`PIPELINE_STARTER_TEMPLATE`), whose `input_mapping` carries two rows in
 * this fixed order: `system` (already F-String) then `task` (starts
 * `variable`). `taskType`/`taskValue` below address the SECOND `Type`/`Value`
 * control on the card for exactly that reason — the row order is the
 * template's own, not a guess.
 *
 * `task` is one of the five `variableName`s `FSTRING_AUTOCOMPLETE_VARIABLES`
 * gates the dropdown on (`flowEditor.constants.ts`), so switching it to
 * F-String is what turns the dropdown on for this specific field.
 *
 * ELITEA-1335's own second half — a variable reference actually RESOLVING at
 * runtime — needs a real pipeline run and is STREAM-DEFERRED; only the YAML
 * half is asserted here.
 */
import { expect, test, type Page, type Locator } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import {
  createPipelineThroughApi,
  deletePipeline,
  parseStoredGraph,
  readStoredPipelineVersion,
  resolveLatestPipelineVersionId,
  type CreatedPipeline,
} from '../../fixtures/pipelines';

const LLM_NODE_ID = 'LLM_1';

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline !== undefined) await deletePipeline(page.request, pipeline);
  }
});

async function openEditor(page: Page, pipeline: CreatedPipeline): Promise<Locator> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  const card = page.locator(`.react-flow__node[data-id="${LLM_NODE_ID}"]`);
  await expect(card).toBeVisible({ timeout: 20_000 });
  return card;
}

/** The `task` row's own `Type` control — the SECOND of the card's three (`system`, `task`, `chat_history`), by DOM order. */
function taskType(card: Locator): Locator {
  return card.getByRole('combobox', { name: 'Type' }).nth(1);
}

/**
 * The `task` row's own Value control. `NodeFieldInput` (Fixed/F-String) mints
 * `id="task-value"`; the `variable`-type branch renders a `SingleSelect`
 * instead, with no id of its own — every "Value"-labelled field on this card
 * shares that generic label, so a role/name lookup alone is ambiguous the
 * moment more than one row is a plain text field (`system` is F-String and
 * `chat_history` is Fixed from the moment the card mounts).
 */
function taskValueField(card: Locator): Locator {
  return card.locator('#task-value');
}

async function setTaskType(card: Locator, page: Page, label: 'Fixed' | 'Variable' | 'F-String'): Promise<void> {
  await taskType(card).click();
  await page.getByRole('option', { name: label, exact: true }).click();
}

async function saveAndAwaitPersist(page: Page): Promise<void> {
  const persisted = page.waitForResponse(
    (response) =>
      response.request().method() === 'PUT' && response.url().includes('/version/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

async function readTaskMapping(
  page: Page,
  pipeline: CreatedPipeline,
): Promise<{ type?: unknown; value?: unknown }> {
  const versionId = await resolveLatestPipelineVersionId(page.request, pipeline.projectId, pipeline.id);
  const stored = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, versionId);
  const graph = parseStoredGraph(stored.instructions);
  const llmNode = graph.nodes.find((node) => node['id'] === LLM_NODE_ID) as
    | { input_mapping?: Record<string, { type?: unknown; value?: unknown }> }
    | undefined;
  const mapping = llmNode?.input_mapping?.['task'];
  if (mapping === undefined) throw new Error('the stored LLM_1 node carries no task mapping');
  return mapping;
}

/* onetest: ELITEA-1334 — the `{` keystroke does not open the dropdown when the field is Fixed or Variable, only when it is F-String. */
test('the F-String dropdown is gated on the field actually being F-String type', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}fstr-gate-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const card = await openEditor(page, pipeline);
  const popper = page.getByTestId('fstring-autocomplete-popper');

  // Starter default: `task` is `variable` — a dedicated Value picker, no free text.
  await expect(card.getByRole('combobox', { name: 'Value' })).toBeVisible();

  // Fixed: a plain text field; `{` is typed literally, no popper.
  await setTaskType(card, page, 'Fixed');
  const fixedField = taskValueField(card);
  await fixedField.fill('');
  await fixedField.pressSequentially('{');
  await expect(popper).not.toBeVisible();
  await expect(fixedField).toHaveValue('{');

  // Back to Variable: the dedicated picker is back, not a free-text field — `task-value` (the id `NodeFieldInput` mints) is gone.
  await setTaskType(card, page, 'Variable');
  await expect(taskValueField(card)).toHaveCount(0);

  // F-String: NOW `{` opens the popper.
  await setTaskType(card, page, 'F-String');
  const fstringField = taskValueField(card);
  await fstringField.click();
  await fstringField.pressSequentially('{');
  await expect(popper).toBeVisible({ timeout: 5_000 });
});

/* onetest: ELITEA-1337, ELITEA-1335 (editor half) — `{` opens the dropdown, a prefix filters it, selecting an option inserts `{name}` with the closing brace, and the stored YAML carries the exact text with no double braces. */
test('the dropdown filters by prefix, inserts the closing brace, and the value is stored verbatim', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}fstr-insert-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const card = await openEditor(page, pipeline);
  await setTaskType(card, page, 'F-String');

  const field = taskValueField(card);
  const popper = page.getByTestId('fstring-autocomplete-popper');
  const options = popper.getByTestId('fstring-autocomplete-option');

  await field.click();
  await field.pressSequentially('Summarize ');
  await field.pressSequentially('{');
  await expect(popper).toBeVisible({ timeout: 5_000 });
  // The two default state variables, unfiltered.
  await expect(options).toHaveCount(2);
  await expect(options.filter({ hasText: 'input' })).toBeVisible();
  await expect(options.filter({ hasText: 'messages' })).toBeVisible();

  // Prefix-filter to just `messages`.
  await field.pressSequentially('m');
  await expect(options).toHaveCount(1);
  await expect(options.filter({ hasText: 'messages' })).toBeVisible();

  await options.filter({ hasText: 'messages' }).click();
  await expect(popper).not.toBeVisible();
  await expect(field).toHaveValue('Summarize {messages}');

  await saveAndAwaitPersist(page);
  const mapping = await readTaskMapping(page, pipeline);
  expect(mapping.type).toBe('fstring');
  expect(mapping.value).toBe('Summarize {messages}');
});

/* onetest: ELITEA-1336 — a state variable added through the States drawer appears in the dropdown immediately, with no page reload. */
test('the dropdown reflects a newly added state variable without a reload', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}fstr-newvar-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const card = await openEditor(page, pipeline);
  await setTaskType(card, page, 'F-String');

  const field = taskValueField(card);
  const popper = page.getByTestId('fstring-autocomplete-popper');
  const options = popper.getByTestId('fstring-autocomplete-option');

  await field.click();
  await field.pressSequentially('{');
  await expect(options).toHaveCount(2, { timeout: 5_000 });
  // `closeAutocomplete` runs on blur (`NodeFieldInput`'s `onBlur`) — Escape is
  // not wired to it, so the field is blurred directly instead.
  await field.evaluate((node) => (node as HTMLElement).blur());
  await expect(popper).not.toBeVisible();

  // Add `new_result` through the States drawer — no reload anywhere below.
  // The configuration panel overlaps the drawer's own "Context" button at
  // default width (the same intercept `pipelines.node-actions.spec.ts`'s
  // `openCanvas` collapses out of the way), so it is collapsed first.
  await page.getByRole('button', { name: 'Collapse the configuration panel' }).click();
  await page.getByRole('button', { name: 'State' }).click();
  await page.getByRole('button', { name: 'Context' }).click();
  const newVarName = page.getByPlaceholder('name');
  await newVarName.fill('new_result');
  await newVarName.press('Enter');
  await expect(page.getByText('new_result', { exact: true })).toBeVisible({ timeout: 10_000 });
  await page.getByRole('button', { name: 'Close' }).click();

  // Clean slate — the field still carries the unclosed `{` from above.
  await field.fill('');
  await field.click();
  await field.pressSequentially('{');
  await expect(options).toHaveCount(3, { timeout: 5_000 });
  await expect(options.filter({ hasText: 'new_result' })).toBeVisible();

  await options.filter({ hasText: 'new_result' }).click();
  await expect(field).toHaveValue('{new_result}');
});

/* onetest: ELITEA-1338 — text typed inside `{}` that matches no state variable is accepted and preserved, not auto-deleted or replaced, on screen and in the stored document. */
test('manual text inside {} that matches no variable is preserved verbatim', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}fstr-manual-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const card = await openEditor(page, pipeline);
  await setTaskType(card, page, 'F-String');

  const field = taskValueField(card);
  const popper = page.getByTestId('fstring-autocomplete-popper');

  await field.click();
  await field.pressSequentially('{custom_label');
  // No option in the popper can match — the popper shows no results (or is
  // gone outright); either way nothing about the typed text is touched.
  await expect(popper.getByTestId('fstring-autocomplete-option').filter({ hasText: 'custom_label' })).toHaveCount(0);
  await page.keyboard.press('Escape');
  await field.pressSequentially('}');
  await expect(field).toHaveValue('{custom_label}');

  await saveAndAwaitPersist(page);
  const mapping = await readTaskMapping(page, pipeline);
  expect(mapping.value).toBe('{custom_label}');
});
