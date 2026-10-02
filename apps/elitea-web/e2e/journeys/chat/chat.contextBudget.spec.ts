/**
 * Context presets are saved on the profile and copied into new conversations.
 * Existing conversation strategies keep their saved settings.
 * Capacity stays unavailable before the runtime measures an admitted model.
 *
 * ONE ACCOUNT, ONE WRITER: both tests change the same account-wide profile.
 * Run this file in one worker and restore the profile after each test.
 * Carry forward the author fields that the profile route replaces.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createConversation,
  deleteConversation,
} from '../../fixtures/api';

test.describe.configure({ mode: 'serial' });

type BudgetMode = 'balanced' | 'full';

interface AuthorRecord {
  readonly name?: string;
  readonly description?: string;
  readonly avatar?: string;
  readonly personalization?: unknown;
  readonly default_context_management?: Record<string, unknown>;
}

async function readAuthor(request: APIRequestContext): Promise<AuthorRecord> {
  const response = await request.get(`${API_BASE}/social/author`);
  expect(response.status(), 'the profile must be readable').toBe(200);
  return (await response.json()) as AuthorRecord;
}

async function writeContextDefaults(
  request: APIRequestContext,
  author: AuthorRecord,
  contextManagement: Record<string, unknown> | undefined,
): Promise<void> {
  const response = await request.put(`${API_BASE}/social/author`, {
    data: {
      name: author.name ?? '',
      description: author.description ?? '',
      avatar: author.avatar ?? '',
      personalization: author.personalization ?? {},
      default_context_management: contextManagement ?? {},
    },
  });
  expect(response.status(), `the profile write must succeed: ${(await response.text()).slice(0, 200)}`).toBe(200);
}

function preset(original: Record<string, unknown> | undefined, mode: BudgetMode) {
  const { max_context_tokens: _legacyLimit, ...rest } = original ?? {};
  return { ...rest, enabled: true, budget_mode: mode };
}

async function readFrozenStrategy(request: APIRequestContext, conversationId: string) {
  const response = await request.get(
    `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
  );
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { meta?: { context_strategy?: Record<string, unknown> } };
  expect(body.meta?.context_strategy, 'creation must save the resolved conversation strategy').toBeDefined();
  return body.meta?.context_strategy ?? {};
}

async function expectUnmeasuredStatus(request: APIRequestContext, conversationId: string, mode: BudgetMode) {
  const response = await request.get(
    `${API_BASE}/elitea_core/context_analytics/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
  );
  expect(response.status()).toBe(200);
  const status = (await response.json()) as Record<string, unknown>;
  expect(status).toMatchObject({ budget_mode: mode, max_tokens: 0, context_analytics_available: false });
  expect(status['unavailable'], 'the response must identify unknown capacity').toContain('max_tokens');
  expect(status['runtime_context'], 'an empty conversation must not invent a runtime measurement').toBeUndefined();
}

async function openBudgetPanel(page: Page, conversationId: string, mode: BudgetMode) {
  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
  await page.getByRole('button', { name: 'Expand participants' }).click();
  const panel = page.getByTestId('context-budget-panel');
  await expect(panel).toBeVisible({ timeout: 20_000 });
  await expect(panel.getByTestId('context-budget-stat-mode')).toContainText(mode === 'full' ? 'Full' : 'Balanced');
  await expect(panel.getByTestId('context-budget-tokens')).toHaveText('Usage not yet measured');
  await expect(panel.getByTestId('context-budget-utilization')).toHaveText('—');
  await expect(panel.getByTestId('context-budget-progress')).toHaveCount(0);
}

function conversationName(tag: string) {
  return `${AUTOTEST_PREFIX}${tag}-ctx-${Date.now()}`;
}

test('profile presets apply to new conversations while existing snapshots and unknown usage stay intact', async ({ page }) => {
  test.setTimeout(180_000);
  const author = await readAuthor(page.request);
  const original = author.default_context_management;
  const conversations: string[] = [];
  try {
    await writeContextDefaults(page.request, author, preset(original, 'balanced'));
    expect((await readAuthor(page.request)).default_context_management?.['budget_mode']).toBe('balanced');
    const balancedId = await createConversation(page.request, conversationName('balanced'));
    conversations.push(balancedId);
    const balancedSnapshot = await readFrozenStrategy(page.request, balancedId);
    expect(balancedSnapshot).toMatchObject({ enabled: true, budget_mode: 'balanced', max_context_tokens: 0 });
    await expectUnmeasuredStatus(page.request, balancedId, 'balanced');
    await openBudgetPanel(page, balancedId, 'balanced');
    await checkA11y(page);

    await writeContextDefaults(page.request, author, preset(original, 'full'));
    expect((await readAuthor(page.request)).default_context_management?.['budget_mode']).toBe('full');
    expect(await readFrozenStrategy(page.request, balancedId), 'a profile update must keep the existing snapshot').toEqual(balancedSnapshot);
    await expectUnmeasuredStatus(page.request, balancedId, 'balanced');

    const fullId = await createConversation(page.request, conversationName('full'));
    conversations.push(fullId);
    const fullSnapshot = await readFrozenStrategy(page.request, fullId);
    expect(fullSnapshot).toMatchObject({ enabled: true, budget_mode: 'full', max_context_tokens: 0 });
    await expectUnmeasuredStatus(page.request, fullId, 'full');
    await openBudgetPanel(page, fullId, 'full');
    await page.reload();
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
    await page.getByRole('button', { name: 'Expand participants' }).click();
    await expect(page.getByTestId('context-budget-stat-mode')).toContainText('Full');
    await openBudgetPanel(page, balancedId, 'balanced');
    expect(await readFrozenStrategy(page.request, fullId)).toEqual(fullSnapshot);
  } finally {
    await writeContextDefaults(page.request, author, original);
    for (const id of conversations) await deleteConversation(page.request, id);
  }
});

/* Legacy context editor coverage now uses the saved Balanced and Full presets. */
test('the Context Budget editor saves the default preset and preserves existing conversation settings', async ({ page }) => {
  test.setTimeout(180_000);
  const author = await readAuthor(page.request);
  const original = author.default_context_management;
  const conversations: string[] = [];
  try {
    await writeContextDefaults(page.request, author, preset(original, 'balanced'));
    const existingId = await createConversation(page.request, conversationName('budgetedit'));
    conversations.push(existingId);
    const existingSnapshot = await readFrozenStrategy(page.request, existingId);
    await openBudgetPanel(page, existingId, 'balanced');
    await page.getByTestId('context-budget-edit-button').click();
    const dialog = page.getByTestId('context-budget-edit-dialog');
    await expect(dialog).toBeVisible({ timeout: 10_000 });
    await expect(dialog.getByRole('radio', { name: /^Balanced/ })).toBeChecked();
    await checkA11y(page);
    await dialog.getByRole('radio', { name: /^Full/ }).check();
    const saved = page.waitForResponse((response) =>
      new URL(response.url()).pathname === '/api/v2/social/author' && response.request().method() === 'PUT',
    { timeout: 20_000 });
    await dialog.getByRole('button', { name: 'Save', exact: true }).click();
    const response = await saved;
    expect(response.status(), 'the editor must save through the profile route').toBe(200);
    const sent = response.request().postDataJSON() as { default_context_management?: Record<string, unknown> };
    expect(sent.default_context_management).toMatchObject({ budget_mode: 'full', enabled: true });
    expect(sent.default_context_management?.['max_context_tokens'], 'preset saves must remove the old numeric limit').toBeUndefined();
    await expect(dialog).toHaveCount(0, { timeout: 20_000 });
    await expect.poll(async () => (await readAuthor(page.request)).default_context_management?.['budget_mode'],
      { timeout: 20_000, message: 'the saved preset must reach the profile' }).toBe('full');
    expect(await readFrozenStrategy(page.request, existingId)).toEqual(existingSnapshot);
    await expectUnmeasuredStatus(page.request, existingId, 'balanced');
    await expect(page.getByTestId('context-budget-stat-mode')).toContainText('Balanced');

    await page.getByTestId('context-budget-edit-button').click();
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole('radio', { name: /^Full/ })).toBeChecked();
    await dialog.getByRole('button', { name: 'Cancel', exact: true }).click();
    const newId = await createConversation(page.request, conversationName('edited-full'));
    conversations.push(newId);
    expect(await readFrozenStrategy(page.request, newId)).toMatchObject({ budget_mode: 'full', max_context_tokens: 0 });
    await expectUnmeasuredStatus(page.request, newId, 'full');
    await openBudgetPanel(page, newId, 'full');
  } finally {
    await writeContextDefaults(page.request, author, original);
    for (const id of conversations) await deleteConversation(page.request, id);
  }
});
