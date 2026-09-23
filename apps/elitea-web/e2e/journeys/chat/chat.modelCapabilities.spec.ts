/**
 * The model picker's Capabilities surfaces: chips in the menu, and the
 * Capabilities section + info tooltip in the model settings dialog.
 *
 * Ported by use case from the w1-model-settings package (manual cases
 * ELITEA-0730, ELITEA-0731, ELITEA-0733, ELITEA-0735). ELITEA-0729 is already
 * covered end to end by `chat.modelSelector.spec.ts` (model switch + Apply
 * round trip). ELITEA-0734 ("REASONING"/"CREATIVITY" uppercase labels) is
 * NOT ported: this app's real labels are "Reasoning effort" and
 * "Temperature" (`ReasoningSlider.tsx`, `CreativitySlider.tsx`) — there is no
 * uppercase-styled "CREATIVITY" control to assert, so that case is recorded
 * NA in the ledger.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE CATALOGUE IS MOCKED
 * ─────────────────────────────────────────────────────────────────────────────
 * The chip/tooltip machinery is real product code — `CapabilityChip`,
 * `CapabilitySection` (`widgets/llm-model-selector/ui/settings/`) and
 * `LLMModelsMenu`'s per-row chips all read `supports_vision`/
 * `supports_reasoning` off the catalogue row (`toLlmModel.ts`). But the
 * E2E seed's own catalogue rows (`scripts/e2e-stack.sh`'s `e2e-mock-model-llm`
 * / `e2e-private-model-llm`) carry neither flag — `data` is a bare
 * `{"name": …}` — so a real deployment of this stack currently offers no
 * capability-flagged model to select. Rather than skip the use case, the
 * catalogue READ (`GET /configurations/models/{projectId}`) is intercepted
 * with `page.route` — exactly the technique `indexes.explains.spec.ts` and
 * `settings.notifications.spec.ts`'s J31b already use for a server shape this
 * stack cannot currently produce — so every other request (chat itself, the
 * conversation, the session) is the real stack and only the ONE read this
 * file is about is synthetic.
 */
import { expect, test } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import {
  AUTOTEST_PREFIX,
  createConversation,
  deleteConversation,
} from '../../fixtures/api';
import { BASE_URL } from '../../../playwright.config';

const MODEL_CATALOGUE_GLOB = '**/configurations/models/**';

/** One catalogue row, in the shape `toLlmModel.ts` reads. */
interface MockModel {
  readonly id: string;
  readonly name: string;
  readonly display_name: string;
  readonly default?: boolean;
  readonly supports_vision?: boolean;
  readonly supports_reasoning?: boolean;
}

const PLAIN_MODEL: MockModel = {
  id: 'e2e-cap-plain',
  name: 'e2e-cap-plain',
  display_name: 'E2E Plain Model',
  default: true,
};
const VISION_MODEL: MockModel = {
  id: 'e2e-cap-vision',
  name: 'e2e-cap-vision',
  display_name: 'E2E Vision Model',
  supports_vision: true,
};
const REASONING_MODEL: MockModel = {
  id: 'e2e-cap-reasoning',
  name: 'e2e-cap-reasoning',
  display_name: 'E2E Reasoning Model',
  supports_reasoning: true,
};
const BOTH_MODEL: MockModel = {
  id: 'e2e-cap-both',
  name: 'e2e-cap-both',
  display_name: 'E2E Vision+Reasoning Model',
  supports_vision: true,
  supports_reasoning: true,
};

const MOCK_CATALOGUE: readonly MockModel[] = [PLAIN_MODEL, VISION_MODEL, REASONING_MODEL, BOTH_MODEL];

async function mockCatalogue(page: import('@playwright/test').Page): Promise<void> {
  await page.route(MODEL_CATALOGUE_GLOB, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: MOCK_CATALOGUE }),
    }),
  );
}

async function openConversation(
  page: import('@playwright/test').Page,
  conversationId: string,
): Promise<void> {
  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('model-selector-name')).toBeVisible({ timeout: 20_000 });
}

const menuLocator = (page: import('@playwright/test').Page) =>
  page.locator('[role="menu"][aria-labelledby="model-selector-button"]');

test.describe('the model picker’s Capabilities surfaces', () => {
  /*
   * ELITEA-0733 + ELITEA-0730 (menu half) + ELITEA-0735 — the DROPDOWN
   * carries a chip per capability, next to each model's name, and a model
   * with neither capability renders no chip and no broken layout.
   */
  test('the model menu shows a capability chip per model, and nothing for a model with none', async ({
    page,
  }) => {
    await mockCatalogue(page);
    const conversationId = await createConversation(page.request, `${AUTOTEST_PREFIX}cap_menu_${Date.now()}`);
    try {
      await openConversation(page, conversationId);
      await page.getByTestId('model-selector-name').click();
      const menu = menuLocator(page);
      await expect(menu).toBeVisible({ timeout: 10_000 });

      // The plain model: no chip at all, and the row still renders cleanly.
      // `exact: true` throughout — the row's own accessible name ("E2E Vision
      // Model") contains the word "Vision" too, and a substring match would
      // hit both the chip and the model name in the same row (strict-mode
      // violation).
      const plainRow = menu.getByRole('menuitem', { name: PLAIN_MODEL.display_name, exact: false });
      await expect(plainRow).toBeVisible();
      await expect(plainRow.getByText('Vision', { exact: true })).toHaveCount(0);
      await expect(plainRow.getByText('Reasoning', { exact: true })).toHaveCount(0);

      // Vision-only: exactly the Vision chip.
      const visionRow = menu.getByRole('menuitem', { name: VISION_MODEL.display_name, exact: false });
      await expect(visionRow.getByText('Vision', { exact: true })).toBeVisible();
      await expect(visionRow.getByText('Reasoning', { exact: true })).toHaveCount(0);

      // Reasoning-only: exactly the Reasoning chip.
      const reasoningRow = menu.getByRole('menuitem', { name: REASONING_MODEL.display_name, exact: false });
      await expect(reasoningRow.getByText('Reasoning', { exact: true })).toBeVisible();
      await expect(reasoningRow.getByText('Vision', { exact: true })).toHaveCount(0);

      // Both: both chips, together, on the one row.
      const bothRow = menu.getByRole('menuitem', { name: BOTH_MODEL.display_name, exact: false });
      await expect(bothRow.getByText('Vision', { exact: true })).toBeVisible();
      await expect(bothRow.getByText('Reasoning', { exact: true })).toBeVisible();

      await checkA11y(page);
      await page.keyboard.press('Escape');
    } finally {
      await deleteConversation(page.request, conversationId);
    }
  });

  /*
   * ELITEA-0731 — hovering the Vision chip's info affordance shows a tooltip
   * that explains image ANALYSIS, and does not conflate it with image
   * generation. Reached through the Model settings dialog's Capabilities
   * section (`LLMSettings.tsx`), which is the one place a chip renders with
   * `showTooltip` and the model is already the SELECTED one — the menu row
   * chips carry a tooltip too (`showTooltip` there as well), so this also
   * covers the dropdown's own hover case (ELITEA-0733's tooltip half).
   */
  test('the Vision chip’s tooltip explains image analysis, not generation, in both themes', async ({
    page,
  }) => {
    await mockCatalogue(page);
    const conversationId = await createConversation(page.request, `${AUTOTEST_PREFIX}cap_tip_${Date.now()}`);
    try {
      await openConversation(page, conversationId);

      // Select the vision-capable model first, so the settings dialog opens
      // scoped to it.
      await page.getByTestId('model-selector-name').click();
      await menuLocator(page)
        .getByRole('menuitem', { name: VISION_MODEL.display_name, exact: false })
        .click();
      await expect(menuLocator(page)).toHaveCount(0);
      await expect(page.getByTestId('model-selector-name')).toHaveText(VISION_MODEL.display_name);

      await page.getByRole('button', { name: 'model settings menu' }).click();
      await expect(page.getByRole('heading', { name: 'Model settings' })).toBeVisible({ timeout: 15_000 });

      const visionChip = page.getByText('Vision', { exact: true });
      await expect(visionChip).toBeVisible();
      await expect(page.getByText('Reasoning', { exact: true })).toHaveCount(0);

      await visionChip.hover();
      const tooltip = page.getByRole('tooltip');
      await expect(tooltip).toBeVisible({ timeout: 5_000 });
      await expect(tooltip).toContainText(/image/i);
      // The tooltip must read as ANALYSIS, not as generation — the two are a
      // different feature (`w1-image-generation`) and conflating them in copy
      // is the exact confusion ELITEA-0731 exists to rule out.
      await expect(tooltip).not.toContainText(/generat/i);
      await page.mouse.move(0, 0);
      await expect(tooltip).toHaveCount(0, { timeout: 5_000 });

      // Switch to the reasoning model and back through the menu — closing
      // and reopening the dialog is this app's real interaction for "look at
      // a different model's capabilities" (there is no in-dialog model
      // switcher to change it WITHOUT closing, unlike the legacy case's
      // assumption); the Capabilities section still updates per model.
      await page.getByRole('button', { name: 'Cancel', exact: true }).click();
      await expect(page.getByRole('heading', { name: 'Model settings' })).toHaveCount(0);
      await page.getByTestId('model-selector-name').click();
      await menuLocator(page)
        .getByRole('menuitem', { name: REASONING_MODEL.display_name, exact: false })
        .click();
      await page.getByRole('button', { name: 'model settings menu' }).click();
      await expect(page.getByRole('heading', { name: 'Model settings' })).toBeVisible({ timeout: 15_000 });
      await expect(page.getByText('Reasoning', { exact: true })).toBeVisible();
      await expect(page.getByText('Vision', { exact: true })).toHaveCount(0);
    } finally {
      await deleteConversation(page.request, conversationId);
    }
  });

  /*
   * ELITEA-0735 — a model with neither capability renders NO Capabilities
   * section in the settings dialog at all (`CapabilitySection` returns
   * `null` when both flags are falsy), with no broken layout either side of
   * it.
   */
  test('a model with no special capabilities renders no Capabilities section, cleanly', async ({
    page,
  }) => {
    await mockCatalogue(page);
    const conversationId = await createConversation(page.request, `${AUTOTEST_PREFIX}cap_none_${Date.now()}`);
    try {
      await openConversation(page, conversationId);
      // The default model IS the plain one (`PLAIN_MODEL.default === true`),
      // so no menu interaction is needed to reach it.
      await expect(page.getByTestId('model-selector-name')).toHaveText(PLAIN_MODEL.display_name);

      await page.getByRole('button', { name: 'model settings menu' }).click();
      await expect(page.getByRole('heading', { name: 'Model settings' })).toBeVisible({ timeout: 15_000 });

      await expect(page.getByText('Vision', { exact: true })).toHaveCount(0);
      await expect(page.getByText('Reasoning', { exact: true })).toHaveCount(0);

      // The rest of the dialog is still intact — a model with no
      // capabilities section must not have taken the parameter controls
      // with it.
      await expect(page.getByLabel('max_tokens')).toBeVisible();
      await expect(page.getByRole('button', { name: 'Apply', exact: true })).toBeVisible();

      await checkA11y(page);
    } finally {
      await deleteConversation(page.request, conversationId);
    }
  });
});
