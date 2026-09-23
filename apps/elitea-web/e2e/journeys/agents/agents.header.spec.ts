/**
 * The agent editor page's HEADER — a name shown next to a back arrow, fixed
 * in place while the configuration below it scrolls (onetest wave-1 agents
 * package: ELITEA-0106, ELITEA-0109).
 *
 * The hint sheet marked both "COVERED | agents.editor.spec.ts". That file
 * asserts the Name FIELD's value (`agent-name-input`, inside the form), not
 * a page HEADER distinct from it — this file covers the header itself:
 * `EditApplicationHeader` (`pages/agents/ui/EditApplicationHeader.tsx`, #897)
 * mounts a back-arrow `IconButton` beside the agent's name
 * (`data-testid="edit-application-header"`), and the enclosing bar
 * (`EditApplication.styles.ts`'s `tabBarSx`) is `position: sticky` so it
 * keeps its vertical position while the configuration panel scrolls beneath
 * it.
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
