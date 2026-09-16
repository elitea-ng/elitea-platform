/**
 * Shell: the sidebar's global "Create" button (onetest package
 * w1-sidebar-menu/create-button-redesign, EliteaAI/elitea_issues#3846).
 *
 * Ported from ELITEA-1034, 1035, 1036, 1037, 1038, 1039, 1040, 1041, 1042,
 * 1043, 1044, 1045, 1046, 1047, 1048. See `S/port/ledger-P5-mcp-shell.tsv`
 * for the full per-case disposition.
 *
 * Real component: `widgets/create-button/ui/CreateEntityButton.tsx` +
 * `../lib/{constants,command,routes}.ts` — read in full before writing this
 * file, and every locator/route/label below was verified live against this
 * stack first.
 *
 * ── REAL LABELS, not the cases' legacy copy ─────────────────────────────
 * `createEntityOptions()` (`lib/constants.ts`) names the 13 entities: Chat
 * (not "Conversation"), Agent, Skill, Pipeline, Credential, Toolkit,
 * Application, MCP, Artifact Bucket, Configuration (not "Integration"),
 * Token, Secret, Invite User. The cases below are ported against these real
 * strings — the same "assert what renders" rule `shell.resources.spec.ts`'s
 * header documents for the Help Center rename.
 *
 * ── Two real, narrow product gaps this file pins ────────────────────────
 *  - No `Tooltip` anywhere in `CreateEntityTrigger` — collapsed OR expanded,
 *    hovering the button shows nothing (verified live: `getByRole('tooltip')`
 *    stays at 0 in both states). ELITEA-1046 expects "Create New" collapsed.
 *  - `CreateEntityDropdown` paints the active kind with a highlighted
 *    background only — no checkmark glyph anywhere in its JSX. ELITEA-1044
 *    expects one.
 * Both are `test.fail`-marked below and recorded in `S/port/defects.md`.
 */
import { expect, test, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { ensureProjectSelected } from '../../fixtures/project';

function createButton(page: Page) {
  return page.getByTestId('sidebar-create-button');
}

async function gotoAndWaitForButton(page: Page, path: string): Promise<void> {
  await page.goto(BASE_URL + path, { waitUntil: 'domcontentloaded' });
  await expect(createButton(page)).toBeVisible({ timeout: 20_000 });
}

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1042, ELITEA-1043 — every main section's create button
 * shows the section's own entity label (expanded) and its label-half
 * navigates directly to that entity's create surface.
 * ──────────────────────────────────────────────────────────────────────── */
test('CB01: main-section create button labels and direct-click destinations', async ({ page }) => {
  // [section path, expected label, destination URL substring].
  const sections: ReadonlyArray<{ path: string; label: string; dest: string }> = [
    { path: '/agents/all', label: 'Agent', dest: '/agents/create' },
    { path: '/pipelines/all', label: 'Pipeline', dest: '/pipelines/create' },
    { path: '/credentials/all', label: 'Credential', dest: '/credentials/create-credential' },
    { path: '/toolkits/all', label: 'Toolkit', dest: '/toolkits/create' },
    { path: '/apps/applications', label: 'Application', dest: '/apps/catalog' },
    { path: '/mcps/all', label: 'MCP', dest: '/mcps/create' },
    { path: '/artifacts', label: 'Artifact Bucket', dest: '/artifacts/create-bucket' },
  ];

  for (const section of sections) {
    await gotoAndWaitForButton(page, `/app${section.path}`);
    const btn = createButton(page);
    await expect(btn, `on ${section.path} the button must read "${section.label}"`).toHaveText(section.label);
    await btn.click();
    await expect(page).toHaveURL(new RegExp(section.dest.replace(/\//g, '\\/')), { timeout: 15_000 });
  }

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1037 — the label updates immediately on each section
 * change, never showing a stale value from the previous section.
 * ──────────────────────────────────────────────────────────────────────── */
test('CB02: the label never carries a stale value across rapid section navigation', async ({ page }) => {
  await gotoAndWaitForButton(page, '/app/agents/all');
  await expect(createButton(page)).toHaveText('Agent');

  await gotoAndWaitForButton(page, '/app/pipelines/all');
  await expect(createButton(page)).toHaveText('Pipeline');

  await gotoAndWaitForButton(page, '/app/credentials/all');
  await expect(createButton(page)).toHaveText('Credential');

  // Chat -> Toolkits -> Artifacts -> Agents, the case's own rapid sequence.
  await gotoAndWaitForButton(page, '/app/chat');
  await expect(createButton(page)).toHaveText('Chat');
  await gotoAndWaitForButton(page, '/app/toolkits/all');
  await expect(createButton(page)).toHaveText('Toolkit');
  await gotoAndWaitForButton(page, '/app/artifacts');
  await expect(createButton(page)).toHaveText('Artifact Bucket');
  await gotoAndWaitForButton(page, '/app/agents/all');
  await expect(createButton(page)).toHaveText('Agent');

  await createButton(page).click();
  await expect(page).toHaveURL(/\/agents\/create/, { timeout: 15_000 });

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1034 — switching projects on the SAME section keeps the
 * label scoped to the section, not the project, and the button still opens
 * the create form in the newly selected project's context.
 * ──────────────────────────────────────────────────────────────────────── */
test('CB03: the label survives a project switch on the same section', async ({ page }) => {
  await gotoAndWaitForButton(page, '/app/agents/all');
  await expect(createButton(page)).toHaveText('Agent');

  await ensureProjectSelected(page, 'Private');
  await expect(createButton(page)).toHaveText('Agent');
  await createButton(page).click();
  await expect(page).toHaveURL(/\/agents\/create/, { timeout: 15_000 });

  // Back, still "Agent".
  await gotoAndWaitForButton(page, '/app/agents/all');
  await ensureProjectSelected(page, 'Default Project');
  await expect(createButton(page)).toHaveText('Agent');

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1036 — the Personalization page's button shows "+Create"
 * with a real label, not a bare unlabelled icon. The case's own "Actual
 * (Bug)" no longer reproduces (measured live): `SIMPLE_CREATE_ROUTE_SEGMENTS`
 * includes `settings/personalization`, and the simple-route branch of
 * `CreateEntityTrigger` renders `t('widgets.createButton.label', 'Create')`
 * whenever the rail is expanded. Ported as the PASSING behaviour.
 * ──────────────────────────────────────────────────────────────────────── */
test('CB04: the Personalization page shows a labelled Create button, not a bare icon', async ({ page }) => {
  await gotoAndWaitForButton(page, '/app/settings/personalization');
  await expect(createButton(page)).toHaveText('Create');

  await createButton(page).click();
  const menu = page.getByRole('menuitem');
  await expect(menu.first()).toBeVisible({ timeout: 10_000 });
  await expect(menu).toHaveCount(13);

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1035, ELITEA-1040 — the two routes documented as
 * "dropdown-list style" (`SIMPLE_CREATE_ROUTE_SEGMENTS`): the Agent
 * Studio/Catalog page (`/agents-hub` redirects to `/elitea-catalog`,
 * `routes/_shell/agents-hub.tsx`) and Settings > Analytics. Both show the
 * generic "Create" label, never an entity name, and the click opens the
 * full 13-entity list.
 * ──────────────────────────────────────────────────────────────────────── */
test('CB05: Agent Studio (Catalog) and Settings > Analytics show the generic dropdown-list button', async ({
  page,
}) => {
  await gotoAndWaitForButton(page, '/app/elitea-catalog');
  await expect(createButton(page)).toHaveText('Create');
  await createButton(page).click();
  await expect(page.getByRole('menuitem')).toHaveCount(13);
  await page.mouse.click(900, 500);
  await expect(page.getByRole('menuitem')).toHaveCount(0);

  await gotoAndWaitForButton(page, '/app/settings/analytics');
  await expect(createButton(page)).toHaveText('Create');
  await createButton(page).click();
  await expect(page.getByRole('menuitem')).toHaveCount(13);

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1038, ELITEA-1039 — Settings sub-section labels and their
 * click destinations. REAL labels: "Configuration" (not "Integration"),
 * "Token", "Secret", "Invite User" — all four verified live, all four work:
 * the "Invite User" click reaches `/settings/users?inviteUsers=1` and
 * `pages/settings/useInviteDialog.ts` (its own `useSearch({ strict: false
 * })` read of exactly that flag) opens the "Invite users" dialog — a real
 * mechanism, not a stub: `Users.test.tsx`/`useInviteDialog.test.tsx` already
 * unit-cover it, and this is the E2E half.
 * ──────────────────────────────────────────────────────────────────────── */
test('CB06: Settings sub-section labels — Configuration, Token, Secret', async ({ page }) => {
  await gotoAndWaitForButton(page, '/app/settings/model-configuration');
  await expect(createButton(page)).toHaveText('Configuration');
  await createButton(page).click();
  await expect(page).toHaveURL(/\/settings\/create-configuration/, { timeout: 15_000 });

  await gotoAndWaitForButton(page, '/app/settings/tokens');
  await expect(createButton(page)).toHaveText('Token');
  await createButton(page).click();
  await expect(page).toHaveURL(/\/settings\/create-personal-token/, { timeout: 15_000 });

  await gotoAndWaitForButton(page, '/app/settings/secrets');
  await expect(createButton(page)).toHaveText('Secret');
  await createButton(page).click();
  // The same PARAM-060 flag `settings.secrets.spec.ts`'s J21 verifies —
  // asserted on the button it enables, not the (JSON-quoted) query string,
  // for the same reason `mcps.secret-field.spec.ts` gives.
  await expect(page.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
    timeout: 15_000,
  });

  await checkA11y(page);
});

test('CB07: Settings > Users — Invite User label opens the real invite dialog', async ({ page }) => {
  await gotoAndWaitForButton(page, '/app/settings/users');
  await expect(createButton(page)).toHaveText('Invite User');

  await createButton(page).click();
  await expect(page.getByRole('dialog').getByText('Invite users', { exact: true })).toBeVisible({
    timeout: 10_000,
  });
  await expect(page.getByRole('textbox', { name: /Emails/i })).toBeVisible();

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1041, ELITEA-1045, ELITEA-1047 — collapsed sidebar: the
 * button shows ONLY the `+` icon (no label, on ANY section), clicking it
 * opens the full 13-entity dropdown, an outside click closes it without
 * navigating, and selecting an entity navigates to its create form.
 * ──────────────────────────────────────────────────────────────────────── */
test('CB08: collapsed rail — icon-only button, full dropdown, outside-click close, entity navigation', async ({
  page,
}) => {
  const REAL_OPTIONS = [
    'Chat',
    'Agent',
    'Skill',
    'Pipeline',
    'Credential',
    'Toolkit',
    'Application',
    'MCP',
    'Artifact Bucket',
    'Configuration',
    'Token',
    'Secret',
    'Invite User',
  ] as const;

  for (const section of ['/app/chat', '/app/agents/all', '/app/pipelines/all', '/app/credentials/all', '/app/artifacts', '/app/apps/applications']) {
    await gotoAndWaitForButton(page, section);
    await page.getByTestId('sidebar-collapse-toggle').click();
    const btn = createButton(page);
    // ELITEA-1047: no label text at all, for ANY section, once collapsed.
    await expect(btn).toHaveText('', { timeout: 10_000 });
    await page.getByTestId('sidebar-collapse-toggle').click();
    await expect(btn, `${section} must show its label again once expanded`).not.toHaveText('');
  }

  // The a11y sweep runs here, EXPANDED — see `shell.sidebar.spec.ts` SB01's
  // comment for why: collapsed, the create button has no accessible name at
  // all (ELITEA-1046's own defect), and that is not this test's subject.
  await checkA11y(page);

  // ELITEA-1041/1045: collapse once, open the dropdown, verify the full
  // catalogue, close it with an outside click (no navigation), then select
  // an entity and confirm it navigates.
  await gotoAndWaitForButton(page, '/app/agents/all');
  await page.getByTestId('sidebar-collapse-toggle').click();
  const collapsedBtn = createButton(page);
  await collapsedBtn.click();
  const menu = page.getByRole('menuitem');
  await expect(menu).toHaveCount(REAL_OPTIONS.length);
  await expect(menu.allTextContents()).resolves.toEqual([...REAL_OPTIONS]);

  // Outside click: closes without navigating.
  const beforeUrl = page.url();
  await page.mouse.click(900, 500);
  await expect(menu).toHaveCount(0);
  expect(page.url()).toBe(beforeUrl);

  // Re-open, select Pipeline: navigates to its create form.
  await collapsedBtn.click();
  await page.getByRole('menuitem', { name: 'Pipeline', exact: true }).click();
  await expect(page).toHaveURL(/\/pipelines\/create/, { timeout: 15_000 });
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1048 — expanded split button's chevron half opens the
 * same full 13-entity dropdown, and selecting an entity navigates.
 * ──────────────────────────────────────────────────────────────────────── */
test('CB09: expanded split button — chevron opens the full dropdown and navigates on selection', async ({
  page,
}) => {
  await gotoAndWaitForButton(page, '/app/agents/all');
  const chevron = page.getByRole('button', { name: 'Choose what to create' });
  await expect(chevron).toBeVisible({ timeout: 10_000 });
  await chevron.click();

  await expect(page.getByRole('menuitem')).toHaveCount(13);
  await page.getByRole('menuitem', { name: 'Credential', exact: true }).click();
  await expect(page).toHaveURL(/\/credentials\/create-credential/, { timeout: 15_000 });

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1044 — the collapsed/expanded dropdown marks the CURRENT
 * section's entity with a checkmark. `test.fail`-marked: no checkmark glyph
 * exists anywhere in `CreateEntityDropdown`'s JSX (`ui/CreateEntityButton.
 * tsx`) — the active item gets only a highlighted background
 * (`theme.vars.palette.split.pressed`), verified live (no `svg`/icon sits
 * beside "Agent" in the open menu while on the Agents page).
 * ──────────────────────────────────────────────────────────────────────── */
test('CB10: the active entity is marked with a checkmark in the dropdown', async ({ page }) => {
  test.fail(
    true,
    'ELITEA-1044 (#903): product gap — CreateEntityDropdown highlights the active item with a background ' +
      'colour only; no checkmark icon is rendered for any menu item.',
  );

  await gotoAndWaitForButton(page, '/app/agents/all');
  await createButton(page).click();
  const activeItem = page.getByRole('menuitem', { name: 'Agent', exact: true });
  await expect(activeItem.locator('svg, [data-testid*="check" i]')).toBeVisible({ timeout: 5_000 });
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1046 — collapsed, hovering `+` shows a universal
 * "Create New" tooltip; expanded, the split button shows none. `test.fail`-
 * marked: `CreateEntityTrigger` wraps neither branch in a `Tooltip` at all —
 * verified live, `getByRole('tooltip')` stays at 0 on hover in BOTH states.
 * ──────────────────────────────────────────────────────────────────────── */
test('CB11: collapsed create button shows a "Create New" tooltip on hover', async ({ page }) => {
  test.fail(
    true,
    'ELITEA-1046 (#904): product gap — CreateEntityTrigger has no Tooltip wrapper in either branch; ' +
      'hovering the collapsed "+" button shows nothing (also an axe button-name violation, since the ' +
      'collapsed button then has no accessible name at all — see shell.sidebar.spec.ts SB01).',
  );

  await gotoAndWaitForButton(page, '/app/agents/all');
  await page.getByTestId('sidebar-collapse-toggle').click();
  await createButton(page).hover();
  await expect(page.getByRole('tooltip', { name: 'Create New' })).toBeVisible({ timeout: 5_000 });
});
