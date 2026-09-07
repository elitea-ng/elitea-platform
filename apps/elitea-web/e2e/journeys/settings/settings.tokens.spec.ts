/**
 * Journey 23: Settings: create personal token (JRNY-023)
 *
 * Spec §8.5 acceptance (from parity/manifest/tokens.json JRNY-023).
 * Acceptance: the token value is shown once and the list updates;
 * navigation away with unsaved input is blocked.
 *
 * COVERAGE GAP (product, not test): the second acceptance clause —
 * "navigation away with unsaved input is blocked" — CANNOT be asserted,
 * because the guard does not exist. `src/routes/_shell/settings/
 * create-personal-token.tsx` says so in its own header ("No `useNavBlocker`
 * hook (Wave-2 concern)"), and the only `useNavBlockerStore.setBlockNav`
 * callers are in `src/processes/chat/**`, none on a settings route. So
 * `widgets/app-shell/ui/NavBlockerDialog.tsx` (data-testid
 * "nav-blocker-dialog") can never appear from this form. The nearest real
 * affordance is the in-page DiscardButton, whose enabled-state transition IS
 * asserted below — it is NOT a substitute for the nav guard, and this journey
 * is deliberately left short of that clause rather than faking it.
 *
 * Everything else runs unconditionally: no test.skip, no early return, no
 * `.catch(() => false)`, no `.or()` fallback. Every wait is a web-first
 * expect on a URL, a value, or a row count.
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX } from '../../fixtures/api';

/** Unique per run AND per file (`-tok`) so concurrent agents never collide. */
const stamp = (): string => `${Date.now()}-tok`;

/**
 * Navigate into the shell and WAIT FOR THE ROUTER TO COMMIT the location
 * before returning, then assert that it left the URL alone.
 *
 * Every `/app/*` route USED TO rewrite a bare URL into the shell's full
 * default search schema (`?author_id=&...&viewMode=owner&page_size=20&
 * createSecret="0"`) with a client-side replace that fires AFTER the `load`
 * event `page.goto` waits for. On chromium that replace won the race with
 * whatever the test did next; on webkit it landed late and aborted the NEXT
 * `page.goto` with `Navigation to "…/app/settings/tokens" is interrupted by
 * another navigation to "…/app/settings/tokens?author_id=&…"` (reproduced 4/6
 * webkit runs, 0/6 chromium).
 *
 * `routes/__root.tsx` now strips every defaulted key with `stripSearchParams`,
 * so the rewrite does not happen and there is no race left to wait out. This
 * helper therefore waits on the SHELL, which proves the router committed, and
 * then pins the new contract: a defaulted key never reaches the address bar.
 * The old wait — `toHaveURL(/viewMode=owner/)` — asserted the 359-character
 * URL that change removed.
 */
async function gotoSettled(page: import('@playwright/test').Page, url: string): Promise<void> {
  await page.goto(url);
  await expect(page.getByTestId('sidebar-collapse-toggle')).toBeVisible({ timeout: 30_000 });
  await expect(page).toHaveURL(url);
}

test('J23: settings: create personal token', async ({ page }) => {
  const seedName = `${AUTOTEST_PREFIX}j23_seed_${stamp()}`;
  let seedUUID = '';

  try {
    /* ── seed one token via the real API ───────────────────────────────────
     * The tokens page renders its empty state (and NO DrawerPageHeader, so no
     * "Generate new token" button) while the list is empty — scripts/
     * e2e-stack.sh seeds no tokens. Seeding through POST /auth/token/ both
     * guarantees the table path and proves the create endpoint end-to-end.
     */
    await gotoSettled(page, BASE_URL + '/app/settings/tokens');
    const created = await page.request.post(`${API_BASE}/auth/token/`, {
      data: { name: seedName, expires: { measure: 'days', value: 30 } },
    });
    expect(created.status()).toBe(200);
    const createdBody = (await created.json()) as { uuid: string; name: string; token: string };
    seedUUID = createdBody.uuid;
    expect(createdBody.name).toBe(seedName);
    expect(createdBody.uuid).toMatch(/^[0-9a-f-]{36}$/i);
    // HS512 PAT minted by internal/infra/authsvc/pat_issuer.go — a shape no
    // stub can fabricate. Asserted on the API response only; the value itself
    // is never logged.
    expect(createdBody.token).toMatch(/^eyJ[\w-]+\.[\w-]+\.[\w-]+$/);

    /* ── the list renders the seeded token, from the server ───────────────── */
    await gotoSettled(page, BASE_URL + '/app/settings/tokens');
    const table = page.getByRole('table');
    await expect(table.getByRole('columnheader', { name: 'Token value' })).toBeVisible();

    const seedRow = table.getByRole('row').filter({ hasText: seedName });
    await expect(seedRow).toHaveCount(1);
    // maskedTokenValue (entities/token/model/selectors.ts:41) = '...' + last 4
    // chars of the already-server-masked value. Only a real round trip
    // produces it.
    await expect(seedRow.getByText(/^\.\.\..{4}$/)).toBeVisible();
    // ExpiryCell (TokenRow.tsx:36-40) derives this from the server `expires`
    // timestamp; 30 days out is "Safe".
    await expect(seedRow.getByText('Safe', { exact: true })).toBeVisible();

    await checkA11y(page);

    /* ── the real entry point (no fallback to the empty-state <div>) ─────── */
    const add = page.getByRole('button', { name: 'Generate new token' });
    await expect(add).toBeEnabled();
    await add.click();
    await expect(page).toHaveURL(/\/settings\/create-personal-token(\?|$)/);

    /* ── the create form is POPULATED, not merely present ────────────────── */
    const nameInput = page.getByRole('textbox', { name: 'Name', exact: true });
    const measure = page.getByRole('combobox', { name: 'Expiration period', exact: true });
    const expiration = page.getByRole('spinbutton', { name: 'Value', exact: true });
    const generate = page.getByRole('button', { name: 'Generate', exact: true });
    const discard = page.getByRole('button', { name: 'Discard', exact: true });

    await expect(measure).toHaveValue('days');
    await expect(expiration).toHaveValue('30');
    // isGenerateDisabled = !isValid || isGenerating || !hasChanged
    await expect(generate).toBeDisabled();
    await expect(discard).toBeDisabled();

    await checkA11y(page);

    /* ── live zod validation (TOKEN_NAME_PATTERN, mode: 'onChange') ──────── */
    await nameInput.fill('bad name!');
    await expect(
      page.getByText('Only alphanumeric characters, underscore and hyphen are allowed'),
    ).toBeVisible();
    await expect(generate).toBeDisabled();

    /* ── conditional render a stub cannot fake ───────────────────────────── */
    await measure.selectOption('never');
    await expect(expiration).toHaveCount(0);
    await measure.selectOption('days');
    await expect(expiration).toHaveValue('30');

    /* ── the enable transition, then submit ──────────────────────────────── */
    const uiName = `${AUTOTEST_PREFIX}j23ui_${stamp()}`;
    await nameInput.fill(uiName);
    await expect(
      page.getByText('Only alphanumeric characters, underscore and hyphen are allowed'),
    ).toHaveCount(0);
    await expect(discard).toBeEnabled();
    await expect(generate).toBeEnabled();
    await generate.click();

    /* ── the token value is shown ONCE, inside the dialog ────────────────── */
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByText('New token generated!')).toBeVisible();
    await expect(
      dialog.getByText('This token will only be shown once, so make sure to copy and save it.'),
    ).toBeVisible();
    await expect(dialog.getByText(uiName, { exact: true })).toBeVisible();
    // The generated PAT: same HS512 JWT shape, only obtainable from a real
    // POST /auth/token/ that revealed the full value.
    await expect(dialog.getByText(/^eyJ[\w-]+\.[\w-]+\.[\w-]+$/)).toHaveCount(1);

    /* ── the copy affordance's state machine ─────────────────────────────── */
    const copy = dialog.getByRole('button', { name: 'Copy', exact: true });
    await expect(copy).toBeEnabled();
    await copy.click();
    await expect(dialog.getByRole('button', { name: 'Copied!', exact: true })).toBeDisabled();

    /* ── dismissal auto-navigates and the list refetches ──────────────────
     * Escape is dispatched ON THE DIALOG, not via page.keyboard, and that is
     * a product defect worth recording: after the Copy button disables itself
     * (GeneratedTokenDialog.tsx:53-62) keyboard focus is dropped to <body>,
     * outside MUI's modal root, so a plain page-level Escape no longer
     * reaches the dialog's keydown handler and the dialog cannot be dismissed
     * from the keyboard. Verified directly: Escape closes the dialog when
     * Copy has NOT been clicked, and does nothing once it has.
     * No page.goto here on purpose: the route change must come from
     * onClose -> onCancel -> navigate(), and the new row can only appear
     * because invalidateQueries refetched from the server.
     */
    //
    // The REFETCH is waited for by response, not by the row it produces
    // (#545). `toHaveCount(1)` on the row carries the default 5 s budget and
    // covers a chain of three steps — the invalidation fires, the server
    // answers, the table renders — so its failure names none of them. It is
    // this journey's recorded webkit flake.
    //
    // Armed before the Escape that causes it, and asserted on the BODY: a
    // refetch that does not carry the token now fails here, naming the
    // response, and only a table that failed to render a token the response
    // carries reaches the row assertion. Neither assertion is weakened.
    const refetched = page.waitForResponse(
      (res) => res.request().method() === 'GET' && res.url().includes('/auth/token/'),
      { timeout: 20_000 },
    );
    await dialog.press('Escape');
    await expect(page).toHaveURL(/\/settings\/tokens(\?|$)/);
    const listing = await refetched;
    expect(listing.status(), await listing.text()).toBe(200);
    expect(
      ((await listing.json()) as { name: string }[]).map((t) => t.name),
      'the refetch that follows the create must carry the new token',
    ).toContain(uiName);

    const uiRow = page.getByRole('table').getByRole('row').filter({ hasText: uiName });
    await expect(uiRow).toHaveCount(1);
    await expect(uiRow.getByText(/^\.\.\..{4}$/)).toBeVisible();

    /* ── close the loop: delete round trip through the UI ────────────────── */
    await uiRow.getByRole('button', { name: 'Delete token', exact: true }).click();
    const afterDelete = page.waitForResponse(
      (res) => res.request().method() === 'GET' && res.url().includes('/auth/token/'),
      { timeout: 20_000 },
    );
    await page.getByRole('button', { name: 'Delete', exact: true }).click();
    expect((await afterDelete).status()).toBe(200);
    // Wait for the table to SETTLE on the seeded row first: a bare
    // `toHaveCount(0)` on the deleted row is also satisfied by the transient
    // loading skeleton (TokensTable renders no table while isFetching), which
    // made a failed DELETE look like a successful one.
    await expect(
      page.getByRole('table').getByRole('row').filter({ hasText: seedName }),
    ).toHaveCount(1);
    await expect(
      page.getByRole('table').getByRole('row').filter({ hasText: uiName }),
    ).toHaveCount(0);
  } finally {
    if (seedUUID !== '') {
      await page.request.delete(`${API_BASE}/auth/token/${seedUUID}`);
    }
  }
});
