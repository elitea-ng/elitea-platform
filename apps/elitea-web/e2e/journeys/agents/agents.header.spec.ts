/**
 * The agent editor page's HEADER — a name shown next to a back arrow, fixed
 * in place while the configuration below it scrolls (onetest wave-1 agents
 * package: ELITEA-0106, ELITEA-0109) — [PRODUCT GAP].
 *
 * The hint sheet marked both "COVERED | agents.editor.spec.ts". That file
 * asserts the Name FIELD's value (`agent-name-input`, inside the form), not
 * a page HEADER distinct from it. Read directly:
 *  - `grep -rn "ArrowBack" src/widgets src/pages/agents` — no match anywhere
 *    near this page; there is no back-arrow affordance at all.
 *  - `grep -rln "sticky" src/pages/agents src/widgets` — the only hits are
 *    `widgets/sidebar` and an unrelated draft-state helper; nothing in the
 *    agent editor's own tree positions anything `sticky`.
 *  - `pages/agents/EditApplication.tsx` composes `EditApplicationEditorTabs`
 *    (Configuration/Evaluation/History) directly above
 *    `EditApplicationConfigurationPanel`, inside a plain scrolling `Box`
 *    (`contentSx: { overflowY: 'auto' }`) — no header row sits above the
 *    tab strip at all.
 *
 * Written as the use case SHOULD behave — a header naming the agent, pinned
 * while the configuration scrolls — and marked failing for exactly the
 * absence measured above.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createAgent, deleteAgent } from '../../fixtures/api';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}header-${stem}-${String(Date.now()).slice(-7)}`;
}

/*
 * ELITEA-0106 — [PRODUCT GAP]. The agent's name should be shown in a page
 * header, next to a back arrow, distinct from the editable Name field
 * further down the form.
 */
/* onetest: ELITEA-0106 — the agent name is shown in a page header next to a back arrow */
test('J-header: [PRODUCT GAP] the agent name is shown in a page header next to a back arrow', async ({
  page,
  request,
}) => {
  test.fail(
    true,
    'ELITEA-0106: product gap — the agent editor page has no header row and no back-arrow control; only the editable Name FIELD inside the form shows the name',
  );
  const name = uniqueName('agent');
  const agent = await createAgent(request, name);
  try {
    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });

    const backArrow = page.getByRole('button', { name: /back/i });
    await expect(backArrow, 'a back-arrow control must sit beside the header name').toBeVisible({ timeout: 10_000 });

    const header = page.getByTestId('edit-application-header');
    await expect(header, 'a dedicated header element must exist, separate from the editable Name field').toBeVisible({
      timeout: 10_000,
    });
    await expect(header).toContainText(name);
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/*
 * ELITEA-0109 — [PRODUCT GAP]. That header should stay fixed at the top
 * while the configuration content below it scrolls.
 */
/* onetest: ELITEA-0109 — the header stays fixed at the top while the configuration scrolls */
test('J-header: [PRODUCT GAP] the header stays fixed at the top while the configuration scrolls', async ({
  page,
  request,
}) => {
  test.fail(
    true,
    'ELITEA-0109: product gap — the agent editor renders no sticky header at all; the whole page (tab strip included) scrolls as one block',
  );
  const name = uniqueName('sticky');
  const agent = await createAgent(request, name);
  try {
    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    const panel = page.getByTestId('edit-application-configuration-tab-panel');
    await expect(panel).toBeVisible({ timeout: 20_000 });

    const header = page.getByTestId('edit-application-header');
    await expect(header, 'a dedicated header element must exist to test stickiness on').toBeVisible({
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
