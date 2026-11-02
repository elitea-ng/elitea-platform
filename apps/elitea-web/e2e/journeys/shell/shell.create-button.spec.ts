/**
 * Shell: creating from the sidebar. Every place row that can create shows a
 * "+" at its end (New agent, New pipeline, …) on hover and on keyboard
 * focus; the Chats caption has New chat; the collapsed rail keeps one New
 * chat icon. The split "+ Chat ▾" button these cases used to cover is gone
 * (onetest package w1-sidebar-menu/create-button-redesign,
 * EliteaAI/elitea_issues#3846 — ELITEA-1034..1048 now map here).
 *
 * Real components: `widgets/chat-rail/ui/NavRows.tsx` + `ui/rows.tsx`
 * (`RowCreateButton`), `ui/ChatsCaption.tsx`; the gate is
 * `widgets/sidebar/lib/createGate.ts`. Settings-only creates (token, secret,
 * configuration, invite) have their own buttons on their settings pages.
 */
import { expect, test, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { DEFAULT_PROJECT_NAME, ensureProjectSelected } from '../../fixtures/project';

/** The rows inside the Workshop group (closed until opened). */
const WORKSHOP = new Set(['agents', 'pipelines', 'skills', 'toolkits', 'mcps']);

async function gotoSettled(page: Page, path: string): Promise<void> {
  await page.goto(BASE_URL + path, { waitUntil: 'domcontentloaded' });
  // The Chats caption's New chat shows once the permission query resolved.
  await expect(page.getByTestId('shell-chats-new')).toBeVisible({ timeout: 20_000 });
}

async function openWorkshop(page: Page): Promise<void> {
  const workshop = page.getByTestId('shell-elitea-workshop').getByRole('button').first();
  if ((await workshop.getAttribute('aria-expanded')) !== 'true') await workshop.click();
}

async function rowCreate(page: Page, value: string) {
  if (WORKSHOP.has(value)) await openWorkshop(page);
  await page.getByTestId(`shell-elitea-${value}`).hover();
  return page.getByTestId(`shell-create-${value}`);
}

/* onetest: ELITEA-1042, ELITEA-1043 — each place's "+" is named for what it creates and opens that create surface. */
test('CR01: each creatable row has a named "+" that opens its create page', async ({ page }) => {
  // One navigation and one create-form load per row, seven rows.
  test.setTimeout(90_000);
  const rows: ReadonlyArray<{ value: string; name: string; dest: RegExp }> = [
    { value: 'agents', name: 'New agent', dest: /\/agents\/create/ },
    { value: 'pipelines', name: 'New pipeline', dest: /\/pipelines\/create/ },
    { value: 'skills', name: 'New skill', dest: /\/skills\/create/ },
    { value: 'toolkits', name: 'New toolkit', dest: /\/toolkits\/create/ },
    { value: 'mcps', name: 'New MCP', dest: /\/mcps\/create/ },
    { value: 'applications', name: 'New application', dest: /\/apps\/catalog/ },
    { value: 'artifacts', name: 'New bucket', dest: /\/artifacts\/create-bucket/ },
  ];
  for (const row of rows) {
    await gotoSettled(page, '/app/chat');
    const create = await rowCreate(page, row.value);
    await expect(create, `the ${row.value} row's "+"`).toHaveAccessibleName(row.name);
    await create.click();
    await expect(page).toHaveURL(row.dest, { timeout: 15_000 });
  }
  await checkA11y(page);
});

/* onetest: ELITEA-1046 — the "+" is reachable without a pointer: keyboard focus shows it. */
test('CR02: the "+" shows on keyboard focus and has a tooltip', async ({ page }) => {
  await gotoSettled(page, '/app/apps/applications');
  const create = page.getByTestId('shell-create-applications');
  await create.focus();
  await expect(create).toHaveCSS('opacity', '1');
  await expect(page.getByRole('tooltip', { name: 'New application' })).toBeVisible({ timeout: 5_000 });
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/\/apps\/catalog/, { timeout: 15_000 });
});

/* onetest: ELITEA-1037 — the Chats caption's New chat starts a new chat, also from a conversation. */
test('CR03: the Chats caption starts a new chat', async ({ page }) => {
  await gotoSettled(page, '/app/agents/all');
  await page.getByTestId('shell-chats-new').click();
  await expect(page).toHaveURL(/\/chat\?create=1/, { timeout: 15_000 });
  await expect(page.getByTestId('shell-chats')).toBeVisible();
});

/* onetest: ELITEA-1034 — the "+" follows the selected project. */
test('CR04: the "+" survives a project switch', async ({ page }) => {
  await gotoSettled(page, '/app/agents/all');
  await expect(await rowCreate(page, 'agents')).toHaveAccessibleName('New agent');

  await ensureProjectSelected(page, 'Private');
  const create = await rowCreate(page, 'agents');
  await create.click();
  await expect(page).toHaveURL(/\/agents\/create/, { timeout: 15_000 });

  await gotoSettled(page, '/app/agents/all');
  await ensureProjectSelected(page, DEFAULT_PROJECT_NAME);
  await expect(await rowCreate(page, 'agents')).toBeAttached();
});

/* onetest: ELITEA-1041, ELITEA-1045, ELITEA-1047 — collapsed, the rail keeps one named New chat icon and no chat list. */
test('CR05: collapsed rail — one New chat icon, no chat list', async ({ page }) => {
  await gotoSettled(page, '/app/agents/all');
  await page.getByTestId('sidebar-collapse-toggle').click();
  const newChat = page.getByTestId('sidebar-new-chat');
  await expect(newChat).toHaveAccessibleName('New chat');
  await expect(page.getByTestId('shell-chats')).toHaveCount(0);
  await expect(page.locator('[data-testid^="shell-create-"]')).toHaveCount(0);
  await newChat.hover();
  await expect(page.getByRole('tooltip', { name: 'New chat' })).toBeVisible({ timeout: 5_000 });
  await newChat.click();
  await expect(page).toHaveURL(/\/chat\?create=1/, { timeout: 15_000 });
  await page.getByTestId('sidebar-collapse-toggle').click();
  await expect(page.getByTestId('shell-chats')).toBeVisible();
  await checkA11y(page);
});

/* The split create button is gone for good. */
test('CR06: there is no split create button', async ({ page }) => {
  await gotoSettled(page, '/app/settings/personalization');
  await expect(page.getByRole('button', { name: 'Choose what to create' })).toHaveCount(0);
  await expect(page.getByTestId('sidebar-create-button')).toHaveCount(0);
});
