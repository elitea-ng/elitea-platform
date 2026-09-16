/**
 * JRNY-TOOLKIT-GROUPS — the toolkit editor's Tools section, grouped.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT WAS MISSING
 * ─────────────────────────────────────────────────────────────────────────────
 * The Tools section rendered ONE flat chip row. For a GitHub toolkit that is
 * 44 chips in a single wrap, with `delete_file` sitting between `create_file`
 * and `get_file_metadata` and nothing telling them apart. The person ticking
 * those chips is deciding what an autonomous agent will be permitted to do in
 * a real repository, and the screen gave them no way to see which of the
 * boxes was destructive — nor any way to find one tool among forty-four.
 *
 * This file covers the fourteen `toolkits-credentials/group-tools-within-
 * toolkit` cases: the fixed group order and what each header carries, the
 * select-all-per-group control, alphabetical order inside a group, the search
 * box and what it does and does not filter, the counts staying whole under a
 * filter, the MCP toggle that closes the section, and — the regression half —
 * a toolkit with no served classification still getting the flat list.
 *
 * THE CLASSIFICATION IS THE SERVER'S. elitea-main serves
 * `properties.selected_tools.tool_groups` and `.tool_group_order`
 * (`internal/api/v2/toolkits/tool_groups.go`); this file reads the same served
 * catalogue the app reads and asserts the screen against it, so a change to
 * either side fails here rather than drifting apart silently.
 *
 * THE STORED SHAPE IS UNCHANGED, and that is asserted rather than assumed:
 * §"persistence" saves through the real UI and re-reads the toolkit over the
 * API, where `settings.selected_tools` must still be the same flat array of
 * tool names every agent, pipeline and MCP client already reads.
 */
import { expect, test } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createGithubToolkit, deleteGithubToolkit } from '../../fixtures/api';
import type { GithubToolkitFixture } from '../../fixtures/api';
import { createConfiguration, deleteConfiguration } from '../../fixtures/configurations';
import { BASE_URL } from '../../../playwright.config';

const RUN_ID = String(Date.now()).slice(-6);
const fixtures: GithubToolkitFixture[] = [];

interface ServedToolGroups {
  readonly order: readonly string[];
  readonly groups: Readonly<Record<string, string>>;
  readonly tools: readonly string[];
}

/** The group classification the SERVER serves for one toolkit type — the same read the app makes. */
async function readServedToolGroups(request: APIRequestContext, type: string): Promise<ServedToolGroups> {
  const response = await request.get(`${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`);
  expect(response.status(), await response.text()).toBe(200);
  const catalogue = (await response.json()) as Record<
    string,
    { properties?: { selected_tools?: { args_schemas?: Record<string, unknown>; tool_groups?: Record<string, string>; tool_group_order?: string[] } } }
  >;
  const selectedTools = catalogue[type]?.properties?.selected_tools;
  expect(selectedTools?.tool_groups, `the served ${type} schema must carry tool_groups`).toBeTruthy();
  return {
    order: selectedTools?.tool_group_order ?? [],
    groups: selectedTools?.tool_groups ?? {},
    tools: Object.keys(selectedTools?.args_schemas ?? {}),
  };
}

/** The chip label the picker renders for a raw tool name (`ToolActionsSelector`'s own `toOption`). */
function chipLabel(tool: string): string {
  return (tool.charAt(0).toUpperCase() + tool.slice(1)).replaceAll('_', ' ');
}

async function openToolkitEditor(page: Page, toolkitId: string) {
  await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });
  await expect(page.getByTestId('tool-groups'), 'the Tools section must render its grouped view').toBeVisible({ timeout: 30_000 });
}

async function makeGithubToolkit(page: Page, suffix: string, selectedTools: readonly string[] = []) {
  const fixture = await createGithubToolkit(page.request, DEFAULT_PROJECT_ID, `${AUTOTEST_PREFIX}tkgroups_${suffix}_${RUN_ID}`);
  fixtures.push(fixture);
  if (selectedTools.length > 0) {
    const patch = await page.request.patch(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${fixture.toolkitId}`, {
      data: {
        name: fixture.toolkitName,
        type: 'github',
        settings: {
          repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
          github_configuration: { elitea_title: fixture.credentialTitle, private: false },
          selected_tools: [...selectedTools],
        },
      },
    });
    expect(patch.ok(), `seeding selected_tools answered ${patch.status()}: ${(await patch.text()).slice(0, 200)}`).toBeTruthy();
  }
  return fixture;
}

/**
 * An `aha` toolkit, used by the two tests that press SAVE.
 *
 * NOT github, and the reason is a real gate rather than a preference: the
 * editor refuses Save for a toolkit whose selected credential failed its
 * connection check (`pages/toolkits/lib/useCredentialSaveGate.ts`), and the
 * placeholder GitHub endpoint every fixture here uses is deliberately
 * unroutable, so a github toolkit is never saveable in this suite. The `aha`
 * credential type has no connection check at all on this stack — its own
 * `check_connection_func` answers `unsupported_type`, which the gate
 * correctly does not treat as a refusal (see `toolkits.aha.spec.ts`'s
 * AHA-10) — so it is the type whose Save button this suite can actually
 * press. It carries the same served group classification.
 */
const ahaToolkitIds: string[] = [];
const ahaCredentialIds: string[] = [];

async function makeAhaToolkit(page: Page, suffix: string): Promise<string> {
  const title = `${AUTOTEST_PREFIX}tkgroups_${suffix}_${RUN_ID}_cred`;
  const credential = await createConfiguration(page.request, 'credentials', {
    title,
    type: 'aha',
    shared: true,
    data: { base_url: 'https://autotest-aha.invalid.example', api_key: 'autotest-placeholder-aha-key' },
  });
  ahaCredentialIds.push(credential.id);

  const created = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name: `${AUTOTEST_PREFIX}tkgroups_${suffix}_${RUN_ID}`,
      type: 'aha',
      description: 'JRNY-TOOLKIT-GROUPS fixture',
      settings: { aha_configuration: { elitea_title: title, private: false }, selected_tools: [] },
    },
  });
  expect(created.status(), await created.text()).toBe(201);
  const id = String(((await created.json()) as { id?: string | number }).id ?? '');
  ahaToolkitIds.push(id);
  return id;
}

test.afterAll(async ({ browser }) => {
  const context = await browser.newContext();
  for (const fixture of fixtures) {
    await deleteGithubToolkit(context.request, DEFAULT_PROJECT_ID, fixture);
  }
  for (const id of ahaToolkitIds) {
    await context.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`).catch(() => {});
  }
  for (const id of ahaCredentialIds) {
    await deleteConfiguration(context.request, id).catch(() => {});
  }
  await context.close();
});

/* onetest: ELITEA-2684 — Tool Groups Render in Correct Fixed Order with All Required UI Elements. */
/* onetest: ELITEA-2686 — Tools Remain Alphabetically Sorted Within Each Group. */
test('ELITEA-2684/2686: the groups render in the served fixed order, each labelled, badged, explained and counted — and sorted inside', async ({ page }) => {
  test.setTimeout(120_000);
  const served = await readServedToolGroups(page.request, 'github');
  const fixture = await makeGithubToolkit(page, 'order');
  await openToolkitEditor(page, fixture.toolkitId);

  // ORDER. Read against the DOM order of the rendered sections, so a change
  // that rendered the right four in the wrong sequence fails here.
  const rendered = await page.locator('[data-testid^="tool-group-"]').evaluateAll((nodes) =>
    nodes
      .map((node) => (node as HTMLElement).dataset['testid'] ?? '')
      .filter((id) => ['tool-group-read', 'tool-group-create-update', 'tool-group-delete', 'tool-group-execute'].includes(id)),
  );
  expect(rendered).toEqual(['tool-group-read', 'tool-group-create-update', 'tool-group-delete', 'tool-group-execute']);

  // Each header carries a label, a consequence badge, an explanation and a count.
  for (const [id, badge] of [
    ['read', 'Read-only'],
    ['create-update', 'Changes data'],
    ['delete', 'Destructive'],
    ['execute', 'Unrestricted'],
  ] as const) {
    await expect(page.getByTestId(`tool-group-badge-${id}`)).toHaveText(badge);
    await expect(page.getByTestId(`tool-group-hint-${id}`)).toBeVisible();
    await expect(page.getByTestId(`tool-group-count-${id}`)).toHaveText(/^\d+ \/ \d+$/);
  }

  // The counts are the SERVER's own classification, not a number the screen invented.
  const expectedTotals = new Map<string, number>();
  for (const group of Object.values(served.groups)) expectedTotals.set(group, (expectedTotals.get(group) ?? 0) + 1);
  for (const [group, total] of expectedTotals) {
    const id = group.replaceAll('_', '-');
    await expect(page.getByTestId(`tool-group-count-${id}`), `the ${group} header must count the served ${group} tools`).toHaveText(`0 / ${String(total)}`);
  }

  // ALPHABETICAL inside each group.
  for (const group of served.order) {
    const section = page.getByTestId(`tool-group-${group.replaceAll('_', '-')}`);
    if ((await section.count()) === 0) continue;
    const labels = await section.getByRole('button').allInnerTexts();
    const chips = labels.filter((label) => label.trim() !== '');
    const sorted = [...chips].sort((left, right) => left.localeCompare(right, undefined, { sensitivity: 'base' }));
    expect(chips, `the ${group} group must be alphabetical`).toEqual(sorted);
  }
});

/* onetest: ELITEA-2685 — Group Header Click Toggles Selection for All Tools in That Group. */
/* onetest: ELITEA-2690 — Selected/Total Counts in Group Headers Include Filtered-Out Tools. */
test('ELITEA-2685/2690: a group header selects its whole group and nothing else, and the counts follow', async ({ page }) => {
  test.setTimeout(120_000);
  const served = await readServedToolGroups(page.request, 'github');
  const fixture = await makeGithubToolkit(page, 'toggle');
  await openToolkitEditor(page, fixture.toolkitId);

  const deleteTotal = Object.values(served.groups).filter((group) => group === 'delete').length;
  const readTotal = Object.values(served.groups).filter((group) => group === 'read').length;
  expect(deleteTotal, 'the github toolkit must offer at least one destructive tool for this test to mean anything').toBeGreaterThan(0);

  const deleteToggle = page.getByTestId('tool-group-toggle-delete').getByRole('checkbox');
  await deleteToggle.click();
  await expect(page.getByTestId('tool-group-count-delete')).toHaveText(`${String(deleteTotal)} / ${String(deleteTotal)}`);
  // Every OTHER group is untouched.
  await expect(page.getByTestId('tool-group-count-read'), 'selecting one group must not touch another').toHaveText(`0 / ${String(readTotal)}`);

  // Clicking again clears it.
  await deleteToggle.click();
  await expect(page.getByTestId('tool-group-count-delete')).toHaveText(`0 / ${String(deleteTotal)}`);
});

/* onetest: ELITEA-2695 — Tool Search Filters Tools by Display Name and Raw Tool Name. */
/* onetest: ELITEA-2696 — Search Filtering Does Not Change Tool Selection State. */
test('ELITEA-2695/2696: the search filters across groups by either spelling, and changes no selection', async ({ page }) => {
  test.setTimeout(120_000);
  const served = await readServedToolGroups(page.request, 'github');
  const readTotal = Object.values(served.groups).filter((group) => group === 'read').length;
  // One already-selected tool, so the "filtering changes nothing" half has
  // something to be true about.
  const preselected = Object.entries(served.groups).find(([, group]) => group === 'read')?.[0] as string;
  const fixture = await makeGithubToolkit(page, 'search', [preselected]);
  await openToolkitEditor(page, fixture.toolkitId);

  const search = page.getByTestId('tool-group-search');
  await expect(search).toHaveAttribute('placeholder', 'Search tools');
  await expect(search).toHaveValue('');
  await expect(page.getByTestId('tool-group-count-read')).toHaveText(`1 / ${String(readTotal)}`);

  // The RAW name finds the tool whose chip shows a prettified label.
  await search.fill(preselected);
  await expect(page.getByRole('button', { name: chipLabel(preselected), exact: true })).toBeVisible();
  // …and the count still answers for the WHOLE group, not the filtered view.
  await expect(page.getByTestId('tool-group-count-read'), 'a filter narrows the view, not the decision').toHaveText(`1 / ${String(readTotal)}`);

  // Case-insensitive, and the same result either way.
  await search.fill(preselected.toUpperCase());
  await expect(page.getByRole('button', { name: chipLabel(preselected), exact: true })).toBeVisible();

  // A term that matches nothing says so, inline, and hides every group.
  await search.fill('xyznonexistent123');
  await expect(page.getByTestId('tool-group-no-matches')).toHaveText('No tools match "xyznonexistent123"');
  await expect(page.getByTestId('tool-group-count-read')).toHaveCount(0);

  // Clearing it brings everything back with the selection intact.
  await search.fill('');
  await expect(page.getByTestId('tool-group-count-read')).toHaveText(`1 / ${String(readTotal)}`);
});

/* onetest: ELITEA-2692 — Tool Selection Persistence After Save with Grouped UI. */
/* onetest: ELITEA-2689 — Pipeline Toolkit Node Shows Correct (Chosen) Tool. PARTIAL, ported by use case: the stack runs no model, so a pipeline EXECUTION cannot be driven here. What a pipeline node reads is the saved toolkit's `settings.selected_tools`, and that is what this asserts — over the API, in the exact shape it had before the grouped UI existed. */
/* onetest: ELITEA-2694 — Agent Toolkit Attachment Shows Correct (Chosen) Tool. PARTIAL, same reason and same evidence as ELITEA-2689. */
/* onetest: ELITEA-2697 — Chat Toolkit Tool Selection with Grouping. PARTIAL, same reason: the chat surface reads the same saved array, which this asserts; the prompt-driven halves of the case need a model turn. */
test('ELITEA-2692/2689/2694/2697: a selection made in the grouped UI saves as the same flat array every consumer already reads', async ({ page }) => {
  test.setTimeout(180_000);
  const served = await readServedToolGroups(page.request, 'aha');
  const toolkitId = await makeAhaToolkit(page, 'persist');
  await openToolkitEditor(page, toolkitId);

  const deleteTools = Object.entries(served.groups)
    .filter(([, group]) => group === 'delete')
    .map(([tool]) => tool)
    .sort();
  const readTool = Object.entries(served.groups).find(([, group]) => group === 'read')?.[0] as string;

  // One tool picked by hand, one whole group picked by its header.
  await page.getByRole('button', { name: chipLabel(readTool), exact: true }).click();
  await page.getByTestId('tool-group-toggle-delete').getByRole('checkbox').click();

  const saveButton = page.getByTestId('toolkit-save-button');
  await expect(saveButton, 'ticking tools must make the toolkit saveable').toBeEnabled({ timeout: 20_000 });
  await saveButton.click();
  await expect
    .poll(
      async () => {
        const response = await page.request.get(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${toolkitId}`);
        if (!response.ok()) return null;
        const body = (await response.json()) as { settings?: { selected_tools?: unknown } };
        return body.settings?.selected_tools ?? null;
      },
      { timeout: 30_000, message: 'the saved toolkit must carry the selection made in the grouped UI' },
    )
    .not.toBeNull();

  const response = await page.request.get(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${toolkitId}`);
  const saved = ((await response.json()) as { settings?: { selected_tools?: unknown } }).settings?.selected_tools;
  // A FLAT ARRAY OF NAMES — the shape agents, pipelines and MCP already read.
  // Not an object keyed by group, which is what a grouped UI would break on.
  expect(Array.isArray(saved), `selected_tools must stay a flat array; got ${JSON.stringify(saved)}`).toBe(true);
  const savedTools = [...(saved as string[])].sort();
  expect(savedTools).toEqual([readTool, ...deleteTools].sort());

  // …and reopening shows exactly that selection back in the right groups.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('tool-groups')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('tool-group-count-delete')).toHaveText(`${String(deleteTools.length)} / ${String(deleteTools.length)}`);
});

/* onetest: ELITEA-2687 — MCP Access Toggle Replaces Checkbox with Correct Default State. */
/* onetest: ELITEA-2693 — MCP Toggle Preserves Existing Toolkit State. PARTIAL, ported by use case: the case's last two steps connect an MCP client and call a tool, which needs a running MCP client this stack does not have. The state half — the stored setting is what the toggle shows, both directions, across a save and a reload — is asserted in full. */
test('ELITEA-2687/2693: the MCP control is a toggle at the END of the Tools section, off by default and persisted both ways', async ({ page }) => {
  test.setTimeout(180_000);
  const toolkitId = await makeAhaToolkit(page, 'mcp');
  await openToolkitEditor(page, toolkitId);

  const toggle = page.getByRole('switch', { name: 'Enable MCP access for selected tools' });
  if ((await toggle.count()) === 0) {
    test.skip(true, 'this deployment does not offer MCP exposure (useIsMcpVisible is false), so the control has nothing to govern');
  }

  // OFF for a toolkit that never had it, and it is a SWITCH, not a checkbox.
  await expect(toggle).not.toBeChecked();
  await expect(page.getByTestId('toolkit-mcp-access-hint')).toBeVisible();

  // AFTER the groups — the control governs the selection above it.
  const groups = page.getByTestId('tool-groups');
  const position = await groups.evaluate((node, selector) => {
    const control = document.querySelector(selector);
    return control === null ? 0 : node.compareDocumentPosition(control);
  }, '[data-testid="toolkit-mcp-access-toggle"]');
  expect(position & 4, 'the MCP toggle must follow the tool groups').toBeTruthy();

  // On, saved, reloaded — still on.
  await toggle.click();
  await expect(toggle).toBeChecked();
  const saveButton = page.getByTestId('toolkit-save-button');
  await expect(saveButton, 'turning the toggle on must make the toolkit saveable').toBeEnabled({ timeout: 20_000 });
  const [saveResponse] = await Promise.all([
    page.waitForResponse((response) => response.request().method() === 'PUT' && /\/elitea_core\/tool\/prompt_lib\//.test(response.url()), { timeout: 30_000 }),
    saveButton.click(),
  ]);
  expect(saveResponse.ok(), `saving the MCP toggle answered ${String(saveResponse.status())}`).toBe(true);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('tool-groups')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByRole('switch', { name: 'Enable MCP access for selected tools' }), 'an existing toolkit opens with its stored MCP setting').toBeChecked();
});

/* onetest: ELITEA-2688 — Toolkit Without Group Metadata Renders Flat List (No Regression). */
/* onetest: ELITEA-2691 — Unavailable Tools Render in Separate Block Above Grouped Tools. */
test('ELITEA-2688/2691: an unclassified toolkit still gets the flat list, and an unavailable tool is shown above the groups and never filtered away', async ({ page }) => {
  test.setTimeout(120_000);

  // §1 — NO SERVED CLASSIFICATION -> the flat list, exactly as before.
  const flatFixture = await makeGithubToolkit(page, 'flat', ['get_issue']);
  await page.route(/\/elitea_core\/toolkits\/prompt_lib\//, async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as Record<string, { properties?: { selected_tools?: Record<string, unknown> } }>;
    const selectedTools = body['github']?.properties?.selected_tools;
    if (selectedTools !== undefined) {
      delete selectedTools['tool_groups'];
      delete selectedTools['tool_group_order'];
    }
    await route.fulfill({ response, json: body });
  });
  await page.goto(`${BASE_URL}/app/toolkits/all/${flatFixture.toolkitId}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });
  await expect(page.getByRole('button', { name: 'Get issue', exact: true }), 'the chips must still render').toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('tool-groups'), 'a toolkit with no served classification keeps the flat list').toHaveCount(0);
  await expect(page.getByTestId('tool-group-search')).toHaveCount(0);
  await page.unroute(/\/elitea_core\/toolkits\/prompt_lib\//);

  // §2 — an UNAVAILABLE tool (selected, but the type no longer offers it).
  const unavailableFixture = await makeGithubToolkit(page, 'unavail', ['autotest_retired_tool']);
  await openToolkitEditor(page, unavailableFixture.toolkitId);
  const unavailableChip = page.getByRole('button', { name: 'autotest_retired_tool', exact: true });
  await expect(unavailableChip, 'a selected tool the type no longer offers must still be shown').toBeVisible({ timeout: 20_000 });

  // ABOVE the groups.
  const groups = page.getByTestId('tool-groups');
  const position = await groups.evaluate((node, selector) => {
    const chip = document.querySelector(selector);
    return chip === null ? 0 : node.compareDocumentPosition(chip);
  }, '[data-testid="tool-groups"]');
  expect(position).toBeDefined();
  const unavailableBeforeGroups = await page.evaluate(() => {
    const groupsNode = document.querySelector('[data-testid="tool-groups"]');
    const chips = Array.from(document.querySelectorAll('.MuiChip-root'));
    const retired = chips.find((chip) => chip.textContent?.includes('autotest_retired_tool'));
    if (groupsNode === null || retired === undefined) return false;
    return Boolean(groupsNode.compareDocumentPosition(retired) & Node.DOCUMENT_POSITION_PRECEDING);
  });
  expect(unavailableBeforeGroups, 'the unavailable block belongs above the groups').toBe(true);

  // …and the search never hides it: an unavailable tool is a problem to
  // clear, and a filter that hid it would hide the problem.
  await page.getByTestId('tool-group-search').fill('xyznonexistent123');
  await expect(page.getByTestId('tool-group-no-matches')).toBeVisible();
  await expect(unavailableChip, 'the unavailable block is outside the search').toBeVisible();
});
