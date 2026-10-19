/**
 * The agent editor's Toolkits/internal-tools section — the one piece of
 * ELITEA-0141 ("Agent Detail Page - Configuration and Tabs") that no sibling
 * spec in this directory actually asserts.
 *
 * The rest of ELITEA-0141 is already covered elsewhere, and is NOT
 * re-asserted here:
 *  - the Information section (Agent ID / Version ID) —
 *    `agents.editor.spec.ts`'s "the agent editor shows an Information
 *    section…";
 *  - the Configuration/History tabs — `agents.run-history.spec.ts` drives
 *    `edit-application-tab-history` on the same `edit-application-editor-
 *    tabs` strip (`edit-application-tab-configuration` is its sibling, the
 *    default-selected tab);
 *  - the Instructions field carrying stored text —
 *    `agents.editor.spec.ts`'s "the editor shows the instructions the agent
 *    was stored with".
 *
 * What none of those touch: `features/agents/ui/ApplicationTools.tsx`'s own
 * "Show all" collapsing — a fresh agent shows only its first
 * `max(4, selectedCount)` internal tools sorted with enabled ones first, and
 * with none enabled that is exactly 4 of the (at least) 8 in
 * `INTERNAL_TOOLS_LIST`. Two of the three ELITEA-0141 names — Python sandbox
 * (`pyodide`) and Smart Tools Selection (`lazy_tools_mode`) — sort behind
 * that fold on a brand-new agent, so "Show all" has to be clicked before
 * they exist on screen at all.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createAgent, deleteAgent } from '../../fixtures/api';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}internal-tools-${stem}-${String(Date.now()).slice(-7)}`;
}

/*
 * ELITEA-0141 (Toolkits-section half) — the section is visible with at
 * least the three named switches (Smart Tools Selection, Python sandbox,
 * Data Analysis), each carrying its own real `role="switch"` control.
 */
/* onetest: ELITEA-0141 — the toolkits section shows Smart Tools Selection, Python sandbox and Data Analysis switches */
test('J14c-tools: the toolkits section shows Smart Tools Selection, Python sandbox and Data Analysis switches', async ({
  page,
  request,
}) => {
  const name = uniqueName('agent');
  const agent = await createAgent(request, name);
  try {
    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    const panel = page.getByTestId('edit-application-configuration-tab-panel');
    await expect(panel).toBeVisible({ timeout: 20_000 });

    const toolkitsSection = panel.getByTestId('agent-toolkits-section');
    await expect(toolkitsSection).toBeVisible({ timeout: 20_000 });

    // Data Analysis sorts inside the default fold (index 2 of the
    // `INTERNAL_TOOLS_LIST` order); Python sandbox and Smart Tools Selection
    // do not — "Show all" must be clicked to reach them, and only if the
    // fold control exists at all (a build with fewer than 5 internal tools
    // would never render one).
    const showAll = toolkitsSection.getByRole('button', { name: /show all/i });
    if (await showAll.isVisible({ timeout: 5_000 }).catch(() => false)) {
      await showAll.click();
    }

    const dataAnalysis = toolkitsSection.getByTestId('internal-tool-data_analysis');
    const pythonSandbox = toolkitsSection.getByTestId('internal-tool-pyodide');
    const smartToolSelection = toolkitsSection.getByTestId('internal-tool-lazy_tools_mode');

    await expect(dataAnalysis, 'Data Analysis switch must be present').toBeVisible({ timeout: 10_000 });
    await expect(dataAnalysis).toContainText('Data Analysis');
    await expect(dataAnalysis.getByRole('switch')).toBeVisible();

    await expect(pythonSandbox, 'Python sandbox switch must be present').toBeVisible({ timeout: 10_000 });
    await expect(pythonSandbox).toContainText('Python sandbox');
    await expect(pythonSandbox.getByRole('switch')).toBeVisible();

    await expect(smartToolSelection, 'Smart Tools Selection switch must be present').toBeVisible({ timeout: 10_000 });
    await expect(smartToolSelection).toContainText('Smart Tools Selection');
    await expect(smartToolSelection.getByRole('switch')).toBeVisible();
  } finally {
    await deleteAgent(request, agent.id);
  }
});
