/**
 * The agent editor page's HEADER — a name shown next to a back arrow, fixed
 * in place while the configuration below it scrolls (onetest wave-1 agents
 * package: ELITEA-0106, ELITEA-0109).
 *
 * The header is the shell's title row now (`widgets/app-shell`'s
 * `ShellTopBar`): a breadcrumb `Agents ▸ <name>`, the name the page's heading
 * (`shell-title`), and the "back arrow" its `Agents` crumb, a link to the
 * list. The row sits above the page's scroll box, so it keeps its place while
 * the configuration scrolls beneath it.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createAgent, deleteAgent } from '../../fixtures/api';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}header-${stem}-${String(Date.now()).slice(-7)}`;
}

/*
 * ELITEA-0106 — the agent's name is shown in a page header, next to a back
 * arrow, distinct from the editable Name field further down the form.
 */
/* onetest: ELITEA-0106 — the agent name is shown in a page header next to a back arrow */
test('J-header: the agent name is shown in a page header next to a back arrow', async ({
  page,
  request,
}) => {
  const name = uniqueName('agent');
  const agent = await createAgent(request, name);
  try {
    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });

    const crumbs = page.getByRole('navigation', { name: 'Breadcrumb' });
    const title = crumbs.getByTestId('shell-title');
    await expect(title, 'the title row must name the agent, apart from the editable Name field').toHaveText(name, {
      timeout: 10_000,
    });

    // The way back sits beside the name: the section crumb, a link to the list.
    const back = crumbs.getByRole('link', { name: 'Agents', exact: true });
    await expect(back, 'a way back must sit beside the header name').toBeVisible();
    await back.click();
    await expect(page).toHaveURL(/\/app\/agents(\/|$|\?)/, { timeout: 15_000 });
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/*
 * ELITEA-0109 — that header stays fixed at the top while the configuration
 * content below it scrolls.
 */
/* onetest: ELITEA-0109 — the header stays fixed at the top while the configuration scrolls */
test('J-header: the header stays fixed at the top while the configuration scrolls', async ({
  page,
  request,
}) => {
  const name = uniqueName('sticky');
  const agent = await createAgent(request, name);
  try {
    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    const panel = page.getByTestId('edit-application-configuration-tab-panel');
    await expect(panel).toBeVisible({ timeout: 20_000 });

    const header = page.getByRole('navigation', { name: 'Breadcrumb' });
    await expect(header.getByTestId('shell-title'), 'the title row must name the agent').toHaveText(name, {
      timeout: 10_000,
    });

    const before = await header.boundingBox();
    await page.mouse.wheel(0, 2000);
    await panel.getByTestId('agent-toolkits-section').scrollIntoViewIfNeeded();
    const after = await header.boundingBox();

    expect(before, 'the header must be measurable before scrolling').not.toBeNull();
    expect(after, 'the header must still be on screen after scrolling').not.toBeNull();
    expect(
      after?.y,
      'a sticky header keeps the same vertical position across a scroll of the content beneath it',
    ).toBe(before?.y);
  } finally {
    await deleteAgent(request, agent.id);
  }
});
