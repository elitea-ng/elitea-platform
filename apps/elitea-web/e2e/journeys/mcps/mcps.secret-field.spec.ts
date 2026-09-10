/**
 * MCP config form: the "Create new secret" shortcut on the Client Secret
 * field (onetest package w1-mcp-servers, issue EliteaAI/elitea_issues#5116).
 *
 * Ported from ELITEA-0726 ("MCP Configuration (MCPs Page): Secret Field Shows
 * 'New Secret' Shortcut"). See `S/port/ledger-P5-mcp-shell.tsv` for the full
 * per-case disposition.
 *
 * ── What the case asked for, and what actually renders ──────────────────
 * The case expects the CREATE entry to read "New Private Secret" or "New
 * Project Secret", matching the project type. The real component
 * (`shared/ui/SecretField/SecretField.tsx`'s `SecretSelect`) renders ONE
 * generic entry, `t('shared.ui.secretField.createSecret', 'Create new
 * secret')`, regardless of project type — there is no project-type branch
 * anywhere in `SecretField`, `SecretFieldInput`
 * (`features/toolkits/ui/form/ToolBase/ToolBaseProperty.renderers.tsx:387`)
 * or `useSecretFieldOptions` (`entities/secret/model/
 * useSecretFieldOptions.ts`). That's a real, narrow product gap — the
 * shortcut itself works end to end (verified live against this stack before
 * writing this file: the dropdown opens with a CREATE entry plus a "Saved
 * secrets" subheader, and clicking CREATE opens `/app/settings/secrets?
 * createSecret=1` in a new tab, leaving the MCP form tab untouched) — so it
 * is asserted here as PASSING for the mechanism and `test.fail`-marked only
 * for the label wording.
 *
 * The MCP type catalogue publishes exactly one MCP-flavoured entry, `mcp`
 * ("Remote MCP"), and its schema's `client_secret` property is
 * `format: "password"` (verified live via `GET /elitea_core/toolkits/
 * prompt_lib/{project}`), which is what routes it through `isSecretField` →
 * the `secret` field kind → `SecretFieldInput`. No mock or stub needed: this
 * is the same schema-driven path `mcps.oauth.spec.ts` already drives.
 */
import { expect, test } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import { readsPlatformFlags } from '../../fixtures/platformFlags';

/** Real component copy (`SecretField.tsx`'s `t('shared.ui.secretField.createSecret', …)`). */
const CREATE_ENTRY = 'Create new secret';
/** `SecretField.tsx`'s `ListSubheader` (`t('shared.ui.secretField.savedSecrets', …)`). */
const SAVED_SECRETS_HEADER = 'Saved secrets';
/**
 * `useSecretFieldOptions.ts`'s `SECRETS_SETTINGS_PATH` lands here, but the
 * exact query string does not survive: `routes/__root.tsx`'s search-schema
 * defaulting rewrites `?createSecret=1` into a JSON-quoted `createSecret="1"`
 * shortly after load (the same router behaviour `settings.tokens.spec.ts`'s
 * `gotoSettled` doc comment and `settings.secrets.spec.ts`'s J21 both already
 * document and route around) — measured live: the popup settles on
 * `createSecret=%221%22`, not `=1`. The PATH is the stable half.
 */
const SETTINGS_SECRETS_PATH = /\/app\/settings\/secrets(\?|$)/;

// Gated the same way `mcps.oauth.spec.ts` gates every MCP surface it drives —
// see that file's header for the CI history (#519) this shares.
readsPlatformFlags(test);

/**
 * Switches the "Client Secret" field to Secret mode and opens its picker.
 * `combo` is scoped by accessible name so it never collides with the
 * edit page's separate "Tool" selector — both are real MUI comboboxes on
 * that screen, and a bare `getByRole('combobox')` resolves to both (measured
 * live against this stack while writing this file).
 */
async function openClientSecretPicker(page: import('@playwright/test').Page): Promise<void> {
  await page.getByRole('button', { name: 'Secret', exact: true }).click();
  const combo = page.getByRole('combobox', { name: 'Client Secret' });
  await expect(combo).toBeVisible({ timeout: 10_000 });
  await combo.click();
  await expect(page.getByRole('option', { name: CREATE_ENTRY })).toBeVisible({ timeout: 10_000 });
}

/**
 * Clicks the CREATE entry and returns the popup it opens — asserting it is a
 * NEW TAB (the field's `onCreate` is a `window.open`, never an in-app
 * navigation: routing away would drop whatever the user had already typed
 * into the rest of the MCP form).
 */
async function clickCreateAndCapturePopup(
  page: import('@playwright/test').Page,
  context: import('@playwright/test').BrowserContext,
): Promise<import('@playwright/test').Page> {
  const [popup] = await Promise.all([
    context.waitForEvent('page'),
    page.getByRole('option', { name: CREATE_ENTRY }).click(),
  ]);
  await popup.waitForLoadState('domcontentloaded');
  return popup;
}

test.describe('MCP config form — Client Secret "Create new secret" shortcut', () => {
  /*
   * onetest: ELITEA-0726 — Create MCP flow half. Assertions verified live:
   * the picker exposes CREATE + "Saved secrets", CREATE opens Settings →
   * Secrets in a new tab with `?createSecret=1`, and the original tab is
   * untouched.
   */
  test('MCP01: Create MCP form — Client Secret Secret-mode picker offers Create new secret + Saved secrets', async ({
    page,
    context,
  }) => {
    await page.goto(`${BASE_URL}/app/mcps/create`, { waitUntil: 'domcontentloaded' });
    await page.getByRole('button', { name: 'Remote MCP' }).click();

    // A real, schema-driven field — not a stub: `Toolkit Name *`/`Url *` sit
    // beside it in the same form (verified live), proving this is the actual
    // `mcp` type's property list, not a placeholder screen.
    await expect(page.getByText('Client Secret', { exact: true })).toBeVisible({ timeout: 15_000 });

    await openClientSecretPicker(page);
    await expect(page.getByRole('option', { name: SAVED_SECRETS_HEADER })).toBeVisible();

    const beforeUrl = page.url();
    const popup = await clickCreateAndCapturePopup(page, context);
    await expect(popup).toHaveURL(SETTINGS_SECRETS_PATH);
    // The create-mode affordance actually opened (PARAM-060), not just the
    // bare list — `settings.secrets.spec.ts`'s J21 asserts the same button.
    await expect(popup.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
      timeout: 15_000,
    });

    // The MCP create tab is untouched — still on the create form, nothing
    // submitted, nothing navigated.
    expect(page.url()).toBe(beforeUrl);
    await expect(page.getByText('Client Secret', { exact: true })).toBeVisible();

    await checkA11y(page);
    await popup.close();
  });

  /*
   * onetest: ELITEA-0726 — Edit MCP flow half ("Repeat steps 2–5 using the
   * Create MCP flow" is covered by MCP01 above; this is the edit-page half
   * the case's steps 2-6 exercise first).
   */
  test('MCP02: Edit MCP form — Client Secret Secret-mode picker offers the same shortcut', async ({
    page,
    context,
  }) => {
    const name = `${AUTOTEST_PREFIX}mcp-secret-field-edit`;
    const createResp = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
      data: { name, type: 'mcp', description: `${AUTOTEST_PREFIX}secret field edit fixture`, settings: { url: 'https://example.invalid/mcp' } },
    });
    expect(
      createResp.status(),
      `seed MCP must be created; got ${createResp.status()} ${(await createResp.text()).slice(0, 300)}`,
    ).toBe(201);
    const created = (await createResp.json()) as { id: string };

    try {
      await page.goto(`${BASE_URL}/app/mcps/all/${created.id}`, { waitUntil: 'domcontentloaded' });
      await expect(page.getByText(name, { exact: true })).toBeVisible({ timeout: 15_000 });

      await openClientSecretPicker(page);
      await expect(page.getByRole('option', { name: SAVED_SECRETS_HEADER })).toBeVisible();

      const beforeUrl = page.url();
      const popup = await clickCreateAndCapturePopup(page, context);
      await expect(popup).toHaveURL(SETTINGS_SECRETS_PATH);
      await expect(popup.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
        timeout: 15_000,
      });

      expect(page.url()).toBe(beforeUrl);
      await expect(page.getByText(name, { exact: true })).toBeVisible();

      await checkA11y(page);
      await popup.close();
    } finally {
      await page.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${created.id}`);
    }
  });

  /*
   * onetest: ELITEA-0726 — the CREATE label's exact wording. Written as it
   * SHOULD pass per the case ("New Private Secret" or "New Project Secret",
   * matching the project type); marked failing for the documented reason —
   * see this file's header. `Create new secret` is the real, generic string
   * every project type gets.
   */
  test('MCP03: the CREATE entry names the project type, not a generic label', async ({ page }) => {
    test.fail(
      true,
      'ELITEA-0726 (#902): product gap — SecretField.tsx renders ONE generic "Create new secret" ' +
        'entry for every project type; no caller (SecretFieldInput/useSecretFieldOptions) passes ' +
        'a project-type-specific createLabel, though the prop (SecretFieldSecretsOptions.createLabel) exists.',
    );

    await page.goto(`${BASE_URL}/app/mcps/create`, { waitUntil: 'domcontentloaded' });
    await page.getByRole('button', { name: 'Remote MCP' }).click();
    await openClientSecretPicker(page);

    await expect(
      page.getByRole('option', { name: /^New (Private|Project) Secret$/ }),
    ).toBeVisible({ timeout: 5_000 });
  });
});
