/**
 * elitea_issues package C-admin — four independent CLOSED feature requests
 * confirmed absent by reading the current implementation, each too small to
 * carry its own file but none sharing a root cause with the others.
 *
 *  - #5896: `ServiceDescriptors.tsx`'s own doc comment says the search box "is
 *    still absent" — there is no search input of any kind on the page.
 *  - #5892: no "restore to built-in defaults" affordance exists for EITHER
 *    Agent Publishing's or Skill Publishing's validation-rules field
 *    (`grep -i 'restore\|built.in.default'` over `Features.tsx` — zero hits);
 *    the issue's premise (Skill Publishing already has it, Agent Publishing
 *    doesn't) does not hold against this app — neither has it.
 *  - #5942: `AdminMcpServersEditor.tsx` has no category-name or
 *    Elitea/EPAM/Other grouping concept at all.
 *  - #5772: no MCP Server entry supports a `{project_id}` URL template or a
 *    `personal_token` config-schema field — the "Elitea Internal MCPs" preset
 *    this issue asks for does not exist.
 *  - #5936: no Surveys feature exists anywhere in the bundle
 *    (`grep -ri survey apps/elitea-web/src` — zero hits) — no admin
 *    configuration section, no end-user widget, no Reports menu.
 */
import { expect, test as adminTest } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

/* elitea_issues: #5896 — product gap: Service Descriptors has no search field at all (not even by provider/URL, let alone by Project ID) */
adminTest('J-config-gap: Service Descriptors has no search field', async ({ page }) => {
  adminTest.fail(
    true,
    '#5896: product gap — Admin > Configuration > Service Descriptors renders no search input (ServiceDescriptors.tsx doc comment: "The search box is still absent")',
  );

  await page.goto(`${BASE_URL}/admin/app/service-descriptors`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Service Descriptors' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByPlaceholder(/search/i)).toBeVisible({ timeout: 2_000 });
});

/* elitea_issues: #5892 — product gap: neither Agent Publishing nor Skill Publishing offers a "restore to built-in defaults" action for its validation-rules field */
adminTest('J-config-gap: Agent Publishing has no restore-to-defaults action for validation rules', async ({ page }) => {
  adminTest.fail(
    true,
    '#5892: product gap — no "restore to built-in defaults" icon/action exists for the Publish Validation Rules field in either Agent Publishing or Skill Publishing on Admin > Features',
  );

  await page.goto(`${BASE_URL}/admin/app/features`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Features' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('button', { name: /restore to built-in defaults/i })).toBeVisible({
    timeout: 2_000,
  });
});

/* elitea_issues: #5942 — product gap: MCP toolkit category name/grouping (Elitea/EPAM/Other) is not configurable */
adminTest('J-config-gap: MCP Servers has no category name or grouping configuration', async ({ page }) => {
  adminTest.fail(
    true,
    '#5942: product gap — Admin > Configuration > MCP Servers has no configurable category name and no Elitea/EPAM/Other sub-grouping for MCP toolkits',
  );

  await page.goto(`${BASE_URL}/admin/app/configuration`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Configuration' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByLabel(/category name/i)).toBeVisible({ timeout: 2_000 });
});

/* elitea_issues: #5772 — product gap: no configurable "Elitea Internal MCPs" preset ({project_id} URL templating, personal_token auth) exists */
adminTest('J-config-gap: MCP Servers has no configurable Elitea Internal MCP preset', async ({ page }) => {
  adminTest.fail(
    true,
    '#5772: product gap — Admin > Configuration > MCP Servers has no way to add a preconfigured "Elitea Internal MCP" ({project_id}-templated URL, personal_token auth) reachable from every project\'s Toolkits > MCP',
  );

  await page.goto(`${BASE_URL}/admin/app/configuration`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Configuration' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('button', { name: /add elitea internal mcp/i })).toBeVisible({ timeout: 2_000 });
});

/* elitea_issues: #5936 — product gap: the Surveys feature (admin config, end-user widget, Reports menu) does not exist at all */
adminTest('J-config-gap: Surveys feature does not exist', async ({ page }) => {
  adminTest.fail(
    true,
    '#5936: product gap — no Surveys section exists under Admin > Features, no in-app survey widget, and no Reports menu; this platform has no Surveys feature at all',
  );

  await page.goto(`${BASE_URL}/admin/app/features`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Features' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('button', { name: /surveys/i })).toBeVisible({ timeout: 2_000 });
});
