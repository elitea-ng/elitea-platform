/**
 * Journey: a global administrator gives a person two administration roles on
 * the Users page, and both survive a reload (ADR-0059 R0.8c, issue 2586).
 *
 * The subject is a seeded row nobody signs in as (`scripts/e2e-stack.sh
 * seed`, "administration role fixtures"), one per browser project, so two
 * engines on one stack never edit the same person. The journey takes its
 * roles back before and after, through the API, so a run killed in between
 * does not leave the next one a different starting point.
 */
import { test as adminTest, expect, type APIRequestContext } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { confirmDangerousAction } from '../../fixtures/dangerousAction';
import { API_BASE } from '../../fixtures/api';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

const USERS_URL = `${API_BASE}/admin/auth_users/administration`;

function subjectFor(projectName: string): string {
  return projectName === 'chromium' ? 'e2e-roles-chromium@autotest.local' : 'e2e-roles-webkit@autotest.local';
}

interface ListedUser {
  readonly id: number;
  readonly email: string;
  readonly admin_roles: readonly string[];
}

async function listedUser(request: APIRequestContext, email: string): Promise<ListedUser> {
  const response = await request.get(`${USERS_URL}?limit=20&offset=0&search=${encodeURIComponent(email)}`);
  expect(response.status(), await response.text()).toBe(200);
  const row = ((await response.json()) as { rows: ListedUser[] }).rows.find((entry) => entry.email === email);
  expect(row, `the seed must carry ${email}`).toBeTruthy();
  return row!;
}

async function setRoles(request: APIRequestContext, userId: number, roleNames: readonly string[]): Promise<void> {
  const response = await request.post(USERS_URL, {
    data: { action: 'set_admin_role', user_id: userId, role_names: roleNames },
  });
  expect(response.status(), await response.text()).toBe(200);
}

adminTest('a global administrator gives a person two roles, and both survive a reload', async ({ page, request }, testInfo) => {
  adminTest.setTimeout(90_000);
  const subject = subjectFor(testInfo.project.name);
  const { id } = await listedUser(request, subject);
  await setRoles(request, id, []);

  try {
    await page.goto(BASE_URL + '/admin/app/users', { waitUntil: 'domcontentloaded' });
    await page.getByTestId('admin-users-search').fill(subject);
    // Wait for the search to narrow the table to the subject before touching
    // the picker: the unfiltered first page can hold the same row, and the
    // filtered listing arriving re-renders it, closing an open menu.
    const pickers = page.getByRole('main').getByRole('combobox', { name: 'Administration roles' });
    const picker = page.getByRole('main').getByRole('row').filter({ hasText: subject }).getByRole('combobox', {
      name: 'Administration roles',
    });
    await expect(pickers).toHaveCount(1, { timeout: 20_000 });
    await expect(picker).toHaveText('None');
    await picker.click();
    await page.getByRole('option', { name: /^LLM manager/ }).click();
    await page.getByRole('option', { name: /^Security auditor/ }).click();

    // Closing the menu asks for a reason (R3.8b, issue 2614), and confirming
    // saves the set: one request with both names.
    const saved = page.waitForRequest(
      (sent) => sent.url().includes('/admin/auth_users/administration') && sent.method() === 'POST',
    );
    await page.keyboard.press('Escape');
    await confirmDangerousAction(page, subject);
    const body = (await saved).postDataJSON() as { role_names?: readonly string[] };
    expect([...(body.role_names ?? [])].sort()).toEqual(['llm_manager', 'security_auditor']);
    await expect.poll(async () => [...(await listedUser(request, subject)).admin_roles].sort()).toEqual([
      'llm_manager',
      'security_auditor',
    ]);

    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.getByTestId('admin-users-search').fill(subject);
    await expect(pickers).toHaveCount(1, { timeout: 20_000 });
    await expect(picker).toHaveText('LLM manager, Security auditor');

    await checkA11y(page);
  } finally {
    await setRoles(request, id, []);
  }
});
