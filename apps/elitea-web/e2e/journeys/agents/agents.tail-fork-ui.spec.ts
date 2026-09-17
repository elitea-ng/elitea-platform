/**
 * Onetest wave-1 tail (T1b, folder `export-import/fork`) — the Fork wizard's
 * own UI (`ForkEntityDialog.tsx`) and the "Forked from" indicator
 * (`ApplicationInformation.tsx`'s `ForkedFromRow`), neither of which
 * `agents.publishing.spec.ts`'s "fork copies the agent into the chosen
 * project" test drives: that test forks into the CURRENT project (the
 * dialog's pre-selected default), so it never opens the target-project
 * dropdown, and it never re-opens the COPY's own editor to look at its
 * Information panel.
 *
 * ELITEA-0676 (dialog title, MAIN ENTITY summary, source project excluded
 * from the target dropdown) and ELITEA-0670 ("Forked from" link, working
 * navigation, present on the card/table list view) are both PARTIALLY real:
 * measured directly against `ForkEntityDialog.tsx`/
 * `useForkTargetProjects.ts`/`ApplicationInformation.tsx` — the title and
 * MAIN ENTITY summary are real; the target list is NOT filtered (source
 * project rendering-ordering only); the "Forked from" text is real but is
 * plain non-navigable `Typography` (module doc comment: "dropped `href`"),
 * and it renders ONLY inside the entity's own Information panel — nowhere
 * on the Agents dashboard's card or table row.
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createAgent, deleteAgent } from '../../fixtures/api';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

async function openAgentEditor(page: Page, agentId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
}

async function openLifecycleMenu(page: Page): Promise<void> {
  await page.getByTestId('agent-lifecycle-menu-button').click();
}

async function forkedCopyId(
  request: Parameters<typeof createAgent>[0],
  projectId: string,
  name: string,
  sourceId: string,
): Promise<string | undefined> {
  const list = await request.get(`${API_BASE}/elitea_core/applications/prompt_lib/${projectId}?agents_type=classic`);
  expect(list.ok(), `the agent list answered ${list.status()}`).toBe(true);
  const body = await list.json();
  const rows: readonly { readonly id?: unknown; readonly name?: string }[] = body?.rows ?? body?.items ?? [];
  return rows.filter((row) => row.name === name && String(row.id) !== sourceId).map((row) => String(row.id))[0];
}

/* onetest: ELITEA-0676 — the Fork dialog opens titled "Fork parameters", shows the MAIN ENTITY name and type, and the Fork action is named "Fork". */
test('the fork dialog is titled "Fork parameters" and names the entity being forked', async ({ page, request }) => {
  const name = uniqueName('forkdialog');
  const agent = await createAgent(request, name);
  try {
    await openAgentEditor(page, agent.id);
    await openLifecycleMenu(page);
    await page.getByTestId('agent-fork-menuitem').click();

    const dialog = page.getByTestId('fork-entity-dialog');
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText('Fork parameters')).toBeVisible();
    await expect(dialog.getByTestId('fork-entity-name')).toHaveText(name);
    await expect(dialog.getByText('Type: agent')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Fork', exact: true })).toBeVisible();
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/* onetest: ELITEA-0676 — product gap: the fork target-project dropdown is claimed to exclude the source project; it does not — `useForkTargetProjects` maps every project `useProjectOptions` returns, unfiltered. */
test('the fork target-project dropdown excludes the source project', async ({ page, request }) => {
  const name = uniqueName('forkdropdown');
  const agent = await createAgent(request, name);
  try {
    await openAgentEditor(page, agent.id);
    await openLifecycleMenu(page);
    await page.getByTestId('agent-fork-menuitem').click();
    await expect(page.getByTestId('fork-entity-dialog')).toBeVisible();

    await page.getByTestId('fork-target-project').click();
    const options = page.getByRole('option');
    await expect(options.first()).toBeVisible({ timeout: 5_000 });

    test.fail(
      true,
      'ELITEA-0676: product gap — useForkTargetProjects (src/pages/agents/lib/useForkTargetProjects.ts) never ' +
        'filters the current project out of the target dropdown',
    );

    // The SAME route the dropdown itself reads (`GET /projects/project/
    // default/{publicProjectId}`) — read directly, so "must be absent" is
    // checked against the current project's REAL name, not a UI guess.
    const projectList = await request.get(`${API_BASE}/projects/project/default/1`);
    expect(projectList.ok()).toBe(true);
    const rows = (await projectList.json()) as readonly { readonly id?: unknown; readonly name?: unknown }[];
    const currentProjectName = rows.find((row) => String(row.id) === DEFAULT_PROJECT_ID)?.name;
    expect(currentProjectName, 'could not resolve the current project name from the projects list').toBeDefined();
    await expect(options.filter({ hasText: String(currentProjectName) })).toHaveCount(0);
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/* onetest: ELITEA-0670 (info-panel half, real) — the forked copy's own Information panel names the original agent under "Forked from:". */
test('the forked copy names its original under "Forked from:" on its own Information panel', async ({ page, request }) => {
  test.setTimeout(60_000);
  const name = uniqueName('forkedinfo');
  const agent = await createAgent(request, name);
  let copyId: string | undefined;
  try {
    await openAgentEditor(page, agent.id);
    await openLifecycleMenu(page);
    await page.getByTestId('agent-fork-menuitem').click();
    await expect(page.getByTestId('fork-entity-dialog')).toBeVisible();
    const forkResponse = page.waitForResponse(
      (response) => /\/fork\/prompt_lib\//.test(response.url()) && response.request().method() === 'POST',
    );
    await page.getByRole('button', { name: 'Fork', exact: true }).click();
    const forked = await forkResponse;
    expect([201, 207]).toContain(forked.status());

    copyId = await forkedCopyId(request, DEFAULT_PROJECT_ID, name, agent.id);
    expect(copyId, 'the fork wrote no second row for this name').toBeDefined();

    await openAgentEditor(page, copyId ?? '');
    await page.getByTestId('agent-information-section').click();
    await expect(page.getByText('Forked from:')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(name, { exact: true }).last()).toBeVisible();
  } finally {
    if (copyId !== undefined) await deleteAgent(request, copyId);
    await deleteAgent(request, agent.id);
  }
});

/* onetest: ELITEA-0670 — product gap: "Forked from" is claimed to render as a working navigation LINK, in both card view and table view on the Agents dashboard. It is plain, non-interactive text, confined to the entity's own Information panel — absent from the dashboard's card AND table rows entirely. */
test('"Forked from" is a working link, visible on the Agents dashboard card and table rows', async ({ page, request }) => {
  test.setTimeout(60_000);
  const name = uniqueName('forkedcard');
  const agent = await createAgent(request, name);
  let copyId: string | undefined;
  try {
    await openAgentEditor(page, agent.id);
    await openLifecycleMenu(page);
    await page.getByTestId('agent-fork-menuitem').click();
    await expect(page.getByTestId('fork-entity-dialog')).toBeVisible();
    const forkResponse = page.waitForResponse(
      (response) => /\/fork\/prompt_lib\//.test(response.url()) && response.request().method() === 'POST',
    );
    await page.getByRole('button', { name: 'Fork', exact: true }).click();
    const forked = await forkResponse;
    expect([201, 207]).toContain(forked.status());

    copyId = await forkedCopyId(request, DEFAULT_PROJECT_ID, name, agent.id);
    expect(copyId, 'the fork wrote no second row for this name').toBeDefined();

    test.fail(
      true,
      'ELITEA-0670: product gap — "Forked from" renders only inside ApplicationInformation (the entity\'s own ' +
        'editor), as plain non-navigable text with no href/onClick (see that file\'s own doc comment); the ' +
        'Agents dashboard card/table view shows no such indicator at all',
    );

    await page.goto(`${BASE_URL}/app/agents/my`);
    await page.getByPlaceholder(/search/i).fill(name);
    const card = page.getByText(name, { exact: true }).first().locator('..').locator('..');
    const forkedLink = card.getByRole('link', { name: /forked from/i });
    await expect(forkedLink, 'the card view must show a "Forked from" link').toBeVisible({ timeout: 10_000 });
    await forkedLink.click();
    await expect(page).toHaveURL(new RegExp(`/agents/all/${agent.id}`));
  } finally {
    if (copyId !== undefined) await deleteAgent(request, copyId);
    await deleteAgent(request, agent.id);
  }
});
