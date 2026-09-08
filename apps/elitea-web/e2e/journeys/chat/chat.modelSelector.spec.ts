/**
 * The composer's model picker: what it says is running, and what it offers.
 *
 * Ported by use case from the legacy public suite:
 *  - `tests/ui/chat/test_chat_interface.py::TestConversationUIElements::test_model_selector_opens_menu`
 *    (TC-CHAT-010 / TC-CHAT-020)
 *
 * The legacy test accepted EITHER a menu OR a navigation to settings, because
 * it ran against deployments that did both. This app does exactly one of them
 * (`LLMModelSelector` opens `LLMModelsMenu` in place), so the disjunction is
 * dropped rather than carried: an assertion that passes on two different
 * outcomes cannot fail when the picker stops working.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT DISCRIMINATES, AND WHY IT IS READ FROM THE SERVER
 * ─────────────────────────────────────────────────────────────────────────────
 * The picker has a documented failure mode that a "the button shows some text"
 * assertion cannot see: it renders the literal string `None`
 * (`resolveModelDisplayName`'s fallback) whenever the catalogue it reads is
 * empty or wrong. That is not hypothetical — the picker was once pointed at
 * `/configurations/configurations/{id}?section=models`, which returns model
 * CREDENTIALS with no `name` field at all, and every row rendered with an empty
 * label (#293); and the seed's own comment records that a catalogue with no
 * `section = 'llm'` row makes it say `None`.
 *
 * So the expected label is not hardcoded here. It is read from the catalogue
 * route the app itself reads, `GET /configurations/models/{projectId}`, through
 * the browser's own session — and the assertions are that the button names the
 * FIRST catalogue entry (`useChatBoxData`'s `defaultModel`: the `default` one,
 * else the first), that the menu lists exactly the catalogue, and that the
 * named row is the one marked as selected. A picker fed from the wrong route
 * fails all three; a picker fed from the right route with a broken label fails
 * the first.
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createConversation,
  deleteConversation,
} from '../../fixtures/api';

/** The MODEL CATALOGUE route (`shared/api/configurationsApi.ts`'s `modelCatalogPath`), with the same query the app sends. */
const MODEL_CATALOGUE = `${API_BASE}/configurations/models/${DEFAULT_PROJECT_ID}?include_shared=true`;

/** One catalogue row, in the subset the picker renders (`ConfigModel` -> `toLlmModel`). */
interface CatalogueModel {
  readonly name: string;
  readonly display_name?: string;
  readonly default?: boolean;
}

/** The label the picker must render for a row — `display_name` first, then `name` (`resolveModelDisplayName`). */
function labelOf(model: CatalogueModel): string {
  return model.display_name !== undefined && model.display_name !== '' ? model.display_name : model.name;
}

test('the model picker names the project’s default model and its menu lists the whole catalogue', async ({ page }) => {
  // The catalogue first, through the browser's session: everything below is
  // measured against it rather than against a constant this test invented.
  const catalogue = await page.request.get(MODEL_CATALOGUE);
  expect(catalogue.status(), 'the model catalogue must resolve, or the picker has nothing to be right about').toBe(200);
  const items = ((await catalogue.json()) as { items?: readonly CatalogueModel[] }).items ?? [];
  expect(items.length, 'this stack seeds at least one llm-section model row').toBeGreaterThan(0);

  const expectedDefault = items.find((model) => model.default === true) ?? items[0];
  const expectedLabel = labelOf(expectedDefault as CatalogueModel);
  expect(expectedLabel, 'a catalogue row with no name at all is the #293 shape').not.toBe('');

  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  // The button carries the resolved name, not the fallback. Both halves are
  // asserted: `None` is what a broken catalogue produces, and it is also a
  // legal display name for a model literally called "None", so the positive
  // match on the catalogue row is what actually decides it.
  const name = page.getByTestId('model-selector-name');
  await expect(name).toBeVisible({ timeout: 15_000 });
  await expect(name).toHaveText(expectedLabel, { timeout: 15_000 });
  expect(expectedLabel, 'the picker must not be reporting its empty-catalogue fallback').not.toBe('None');

  await checkA11y(page);

  // Clicking the name opens the menu IN PLACE — no navigation.
  const urlBefore = page.url();
  await name.click();

  // Located by the list's own `aria-labelledby` (`LLMModelsMenu`'s
  // `slotProps.list`), so this cannot accidentally match the composer's "+"
  // menu, which is also a `role="menu"`.
  const menu = page.locator('[role="menu"][aria-labelledby="model-selector-button"]');
  await expect(menu).toBeVisible({ timeout: 10_000 });
  expect(page.url(), 'the picker opens a menu; it does not navigate away').toBe(urlBefore);

  // Exactly the catalogue — no more rows than the server offers, and none
  // fewer. A picker reading the credentials route renders a different count
  // and blank labels.
  const options = menu.getByRole('menuitem');
  await expect(options).toHaveCount(items.length);
  for (const model of items) {
    await expect(menu.getByRole('menuitem', { name: labelOf(model), exact: false })).toHaveCount(1);
  }

  // …and the row the button names is the one the menu marks as chosen.
  //
  // Read as the `Mui-selected` CLASS, deliberately, and not as `aria-selected`:
  // MUI 9's `MenuItem` treats `selected` as presentational for `role="menuitem"`
  // (it derives `aria-checked` only for the `menuitemcheckbox`/`menuitemradio`
  // roles — `MenuItem.js`), so there is no ARIA attribute to read here and an
  // assertion on one would fail for a reason that has nothing to do with the
  // picker. The class is what `LLMModelsMenu`'s `selected={item.id ===
  // selectedModel?.id}` really produces, and it is also what draws the check.
  const chosen = menu.getByRole('menuitem', { name: expectedLabel, exact: false });
  await expect(chosen).toHaveClass(/Mui-selected/);

  // Escape closes it and leaves the picker as it was — a menu that dismissed by
  // choosing something would change the button's label here.
  await page.keyboard.press('Escape');
  await expect(menu).toHaveCount(0);
  await expect(name).toHaveText(expectedLabel);
});

/*
 * The SETTINGS half of the same legacy case: the menu behind the picker, which
 * edits the model PARAMETERS rather than the model choice.
 *
 * Nothing covered this. The gear renders beside the model name in the same
 * `ButtonGroup`, and it is `disabled` unless a settings sink is composed
 * (`LLMModelSelector`'s `disabled={!onSetLLMSettings}`) — so a chat surface
 * that lost the wiring would still SHOW the control, greyed, and every
 * screenshot would look right. That is the reason the assertions below end at
 * the server rather than at the dialog.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY STEPS LIMIT IS THE FIELD THIS DRIVES
 * ─────────────────────────────────────────────────────────────────────────────
 * Apply persists through two different writes (`pages/chat/
 * useChatModelSettings.ts`): the model parameters go onto the signed-in user's
 * PARTICIPANT row, and `steps_limit` goes into the conversation's own `meta`
 * because it bounds the agent loop rather than one model request. A
 * conversation created over the API carries no participants at all — the
 * create route inserts none — so the participant write has no row to land on,
 * and only the `meta` write can be read back. Asserting the temperature here
 * would be asserting a write that this fixture cannot receive, which would
 * fail for a reason that is not the picker's.
 *
 * `steps_limit` is also the honest end-to-end statement: it leaves the browser,
 * is stored by the server, and comes back through a RELOAD below, so a dialog
 * that only mutated its own local state cannot pass.
 */
test('the settings menu behind the picker edits the model parameters, and Apply persists them', async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(180_000);

  const name = `${AUTOTEST_PREFIX}model-settings-${testInfo.project.name}-${Date.now()}`;
  const conversationId = await createConversation(page.request, name);
  try {
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId('model-selector-name')).toBeVisible({ timeout: 20_000 });

    /* ── the control itself ─────────────────────────────────────────────── */
    const gear = page.getByRole('button', { name: 'model settings menu' });
    await expect(gear, 'the picker must offer a settings entry beside the model name').toBeVisible();
    await expect(
      gear,
      'the gear is disabled wherever no settings sink is composed, so an enabled one is the wiring',
    ).toBeEnabled();
    await gear.click();

    await expect(page.getByRole('heading', { name: 'Model settings' })).toBeVisible({ timeout: 15_000 });

    /* ── the parameters it offers ───────────────────────────────────────── */
    // Named controls, so a panel that rendered an empty shell fails here
    // rather than at the write below.
    // Either slider, because WHICH one the panel renders is a property of the
    // model: `LLMSettings` swaps the temperature slider for the reasoning-effort
    // one on a model whose catalogue row declares `supports_reasoning`. Pinning
    // one of them here would fail the day the seeded model gains that flag —
    // which is not this control's regression.
    await expect(
      page
        .getByRole('slider', { name: 'Temperature' })
        .or(page.getByRole('slider', { name: 'Reasoning effort' })),
      'the settings panel must carry the model parameters, not only the buttons',
    ).toBeVisible();
    await expect(page.getByLabel('max_tokens')).toBeVisible();

    /*
     * The steps-limit field, located by its INPUT TYPE.
     *
     * It is the only numeric spinner the panel renders — `max_tokens` and
     * `remaining_tokens` are text fields carrying an `Auto` word as often as a
     * number — and it carries no label element of its own (its caption is a
     * sibling `Typography`, not a `<label>`), so there is no accessible name to
     * ask for. The count assertion is what keeps this honest: the day a second
     * numeric field lands in the panel this fails loudly instead of silently
     * driving the wrong one.
     */
    const stepsLimit = page.locator('input[type="number"]');
    await expect(stepsLimit, 'exactly one numeric field: the steps limit').toHaveCount(1);
    // The default the panel seeds when the conversation carries none.
    await expect(stepsLimit).toHaveValue('25');

    await stepsLimit.fill('7');

    const apply = page.getByRole('button', { name: 'Apply', exact: true });
    await expect(apply, 'exactly one Apply, so this is the panel’s own').toHaveCount(1);
    await apply.click();
    await expect(page.getByRole('heading', { name: 'Model settings' })).toHaveCount(0, { timeout: 15_000 });

    /* ── it reached the SERVER ──────────────────────────────────────────── */
    await expect
      .poll(
        async () => {
          const detail = await request.get(
            `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
          );
          const body = (await detail.json()) as { meta?: { steps_limit?: unknown } };
          return body.meta?.steps_limit;
        },
        {
          timeout: 30_000,
          message: 'Apply must persist the edited parameter; a panel that only moved its own state passes nothing here',
        },
      )
      .toBe(7);

    /* ── and it comes BACK, through a cold load ─────────────────────────── */
    await page.reload();
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
    await page.getByRole('button', { name: 'model settings menu' }).click();
    await expect(page.getByRole('heading', { name: 'Model settings' })).toBeVisible({ timeout: 15_000 });
    await expect(
      page.locator('input[type="number"]'),
      'the panel must seed from the stored conversation, not from the default',
    ).toHaveValue('7');

    /* ── Cancel is not Apply ────────────────────────────────────────────── */
    await page.locator('input[type="number"]').fill('9');
    const cancel = page.getByRole('button', { name: 'Cancel', exact: true });
    await expect(cancel).toHaveCount(1);
    await cancel.click();
    await expect(page.getByRole('heading', { name: 'Model settings' })).toHaveCount(0, { timeout: 15_000 });

    const afterCancel = await request.get(
      `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
    );
    const body = (await afterCancel.json()) as { meta?: { steps_limit?: unknown } };
    expect(
      body.meta?.steps_limit,
      'a dismissed panel must write nothing: the stored value stays the one Apply left',
    ).toBe(7);
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});
