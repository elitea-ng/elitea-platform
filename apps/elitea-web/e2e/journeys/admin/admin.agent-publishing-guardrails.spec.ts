/**
 * Admin › Features › Agent Publishing — the exception whitelist, and the
 * default OFF state / persistence across a reload.
 *
 * Ported by use case from the w1-admin-portal package's
 * `admin-portal/agent-publishing-guardrails` cases (ELITEA-0015, ELITEA-0017,
 * ELITEA-0019). What this file does NOT re-prove:
 *
 *  - ELITEA-0018 ("Reload required" indicator + toast wording) is NOT ported.
 *    This platform enforces the guardrail LIVE off the same write — there is
 *    no elitea_core plugin to reload and no "reload required" concept at all
 *    (`admin.features.spec.ts`'s J36c PUTs the flag and reads a 403 on the
 *    very next request, no reload step anywhere in between). The save toast
 *    this app actually shows is the generic `admin-features-saved` ("Feature
 *    settings saved."), already asserted by J36c/J36g rather than re-asserted
 *    here.
 *  - ELITEA-0016 (a multi-select DROPDOWN listing Team/Private projects, with
 *    Public excluded) is NOT ported — genuinely absent. The real control
 *    (`ConfigurationListEditor.tsx`) is a plain integer-id LIST editor: the
 *    operator types numeric project ids by hand, there is no project picker,
 *    no project names, and therefore no Team/Private/Public filtering to
 *    assert. Recorded NA with this evidence.
 *  - ELITEA-0020 (publish blocked, empty whitelist) is exactly
 *    `admin.features.spec.ts`'s J36c — recorded DUP, not repeated.
 *  - ELITEA-1694 through ELITEA-1698 (the PGVector Configuration dropdown on
 *    the agent/toolkit form) are a different area entirely — they belong
 *    under `e2e/journeys/toolkits/`, owned by a concurrent package this run.
 *    Recorded REROUTE toolkits, with the real field name
 *    (`pgvector_configuration`, `features/toolkits/ui/form/ToolBase/
 *    ToolConfluence.tsx` / `ToolJira.tsx`) for whoever picks them up.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE EXCEPTION CASE IS ASSERTED AT THE GUARDRAIL, NOT THROUGH A REAL
 * AGENT VERSION
 * ─────────────────────────────────────────────────────────────────────────────
 * `Handler.Publish` (`internal/api/v2/eliteacore/handler.go`) checks the
 * guardrail FIRST, "before the body is decoded and before the version is
 * looked up" (the handler's own comment) — a blocked deployment must answer
 * 403 for every caller whose PROJECT PERMISSION check already passed. That
 * ordering is what makes the exception case answerable without a real
 * agent or a real draft version: the guardrail's own `allows(projectID)`
 * (`platform_flags.go`) is a pure membership test against
 * `publish_whitelist_project_ids`, decided before the version is looked up
 * at all. It is NOT the first thing the ROUTE checks, though — measured
 * below: `router.go`'s `requirePublish` project-permission middleware runs
 * ahead of the handler, so a project id this caller has no membership in at
 * all is refused by THAT check first ("insufficient permissions"), and
 * never reaches the guardrail. A SECOND real project the admin persona DOES
 * hold publish permission on (90500, seeded for the publish journeys) is
 * what lets the guardrail's own refusal be observed, and a project id that
 * IS on the whitelist gets past the guardrail to whatever the next check
 * says — which is the observable difference ELITEA-0015 is about ("Publish
 * stays enabled in exception projects, disabled elsewhere"), read the way
 * this suite prefers: through the server's own decision, not through
 * a menu's enabled/disabled pixel.
 *
 * Every write below goes through `page.request` (an `APIRequestContext` bound
 * to the admin session cookie), never `page.evaluate` — the ELITEA-0019 test
 * writes before ever navigating anywhere, and a relative `fetch` inside
 * `page.evaluate` on `about:blank` has no origin to resolve against
 * (`admin.guardrails.spec.ts`'s file header states the same trap).
 */
import { expect, test as adminTest, type APIRequestContext, type Page } from '@playwright/test';

import { API_BASE } from '../../fixtures/api';
import { withPlatformFlagLock } from '../../fixtures/platformFlags';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

const SECTION_URL = `${API_BASE}/admin/plugin_config_values/administration/agent_publishing`;

async function putAgentPublishing(
  request: APIRequestContext,
  values: Record<string, unknown>,
): Promise<{ status: number; body: string }> {
  const response = await request.put(SECTION_URL, { data: { values } });
  return { status: response.status(), body: await response.text() };
}

async function getAgentPublishing(request: APIRequestContext): Promise<Record<string, unknown>> {
  const response = await request.get(SECTION_URL);
  expect(response.status(), 'reading the section back must succeed').toBe(200);
  // The raw body is `{values: {...}}` (`adminConfigurationApi.ts`'s
  // `useAdminConfigValues` peels the SAME envelope through `unwrapBody`) —
  // not the values themselves.
  const body = (await response.json()) as { values?: Record<string, unknown> };
  return body.values ?? {};
}

/** Keeps trying until the server both accepts the restore write and the write actually lands — the same discipline `admin.features.spec.ts`'s `restoreSection` documents in full. */
async function restoreAgentPublishing(request: APIRequestContext): Promise<void> {
  const deadline = Date.now() + 6_000;
  let last = { status: 0, body: 'no attempt was made' };
  for (;;) {
    last = await putAgentPublishing(request, { is_publish_blocked: false, publish_whitelist_project_ids: [] });
    if (last.status === 200) return;
    if (Date.now() > deadline) break;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  expect(
    last.status,
    `agent_publishing was NOT restored, so the guardrail is left ON for every other journey. Last response: ${last.status} ${last.body}`,
  ).toBe(200);
}

/** `POST /elitea_core/publish/prompt_lib/{projectID}/{versionID}` — a version id that names nothing, deliberately (see file header: the guardrail decides before the lookup). */
async function attemptPublish(
  request: APIRequestContext,
  projectId: string,
): Promise<{ status: number; body: string }> {
  const response = await request.post(`${API_BASE}/elitea_core/publish/prompt_lib/${projectId}/99999999`, {
    data: { version_name: 'e2e-agent-publishing-guardrail-probe' },
  });
  return { status: response.status(), body: await response.text() };
}

async function openFeatures(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/features', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA must serve the features route, not 404').toBeLessThan(400);
  await expect(page.getByRole('switch', { name: 'Enable MCP' })).toBeVisible({ timeout: 20_000 });
  await page.getByRole('button', { name: /Agent Publishing/ }).click();
  await expect(page.getByRole('switch', { name: 'Block Agent Publishing' })).toBeVisible({ timeout: 15_000 });
}

adminTest.afterEach(async ({ request }) => {
  // A net for a test that failed inside its own window and never reached its
  // `finally` — see `admin.guardrails.spec.ts`'s identical net for the full
  // reasoning. Unconditional, because every test in this file is inside
  // `withPlatformFlagLock` for the whole of its write.
  await restoreAgentPublishing(request).catch(() => undefined);
});

/*
 * ELITEA-0019: the section renders with its guardrail OFF by default, and no
 * exception editor showing while it is off.
 */
adminTest(
  'the Block Agent Publishing section starts OFF, with no exception list shown',
  async ({ page, request }) => {
    await withPlatformFlagLock(async () => {
      await restoreAgentPublishing(request);
      await openFeatures(page);

      const toggle = page.getByRole('switch', { name: 'Block Agent Publishing' });
      await expect(toggle).toBeVisible();
      await expect(toggle, 'the default-restored state is OFF').not.toBeChecked();
      await expect(page.getByTestId('admin-config-list-publish_whitelist_project_ids')).toHaveCount(0);
    });
  },
);

/*
 * ELITEA-0015 + ELITEA-0020 (0020 is the same 403, DUP of J36c) — an
 * exception project publishes past the guardrail; a non-exception project is
 * refused, with the guardrail's own sentence.
 */
adminTest(
  'ELITEA-0015: blocking publishing with a project exception lets that project through and refuses every other one',
  async ({ request }) => {
    await withPlatformFlagLock(async () => {
      try {
        const saved = await putAgentPublishing(request, {
          is_publish_blocked: true,
          publish_whitelist_project_ids: [1],
        });
        expect(saved.status, saved.body).toBe(200);

        // The write landed under the RIGHT keys — reading the section back is
        // the server's own confirmation, not an assumption about the PUT.
        const stored = await getAgentPublishing(request);
        expect(stored['is_publish_blocked']).toBe(true);
        expect(stored['publish_whitelist_project_ids']).toEqual([1]);

        // Project 1 is the exception: the guardrail must let it through — a
        // 403 with the guardrail's own body would be the regression this
        // proves against.
        const exceptionAttempt = await attemptPublish(request, '1');
        expect(
          exceptionAttempt.status,
          `project 1 is whitelisted; a 403 here means the exception was not honoured: ${exceptionAttempt.body}`,
        ).not.toBe(403);

        // A project NOT on the list is refused, with the guardrail's own
        // sentence. Project 90500 rather than an arbitrary/nonexistent id:
        // the publish ROUTE is also gated by a project-scoped PERMISSION
        // middleware (`router.go`'s `requirePublish`), ahead of the
        // guardrail itself, so a project the caller has no membership in at
        // all (e.g. `999999`) is refused by THAT check first
        // ("insufficient permissions") and never reaches the guardrail —
        // measured, not assumed. 90500 is the one project (besides the
        // default) this admin persona holds
        // `models.applications.publish.post` on
        // (`scripts/e2e-stack.sh`'s "publish journeys' project-private
        // model" seed), so the guardrail is the FIRST and only reason this
        // call can be refused.
        const blockedAttempt = await attemptPublish(request, '90500');
        expect(blockedAttempt.status).toBe(403);
        expect(blockedAttempt.body).toContain('publishing is blocked');
      } finally {
        await restoreAgentPublishing(request);
      }

      // And the exception is gone once the guardrail is cleared — project
      // 90500 must no longer be refused BY THE GUARDRAIL specifically (some
      // other check may still refuse it, but not with this sentence).
      const afterRestore = await attemptPublish(request, '90500');
      expect(afterRestore.body).not.toContain('publishing is blocked');
    });
  },
);

/*
 * ELITEA-0017: the toggle and the exception list persist across a page
 * refresh — nothing about this section's storage is scoped to one browser
 * tab.
 */
adminTest(
  'ELITEA-0017: the guardrail toggle and exception list persist across a reload',
  async ({ page, request }) => {
    await withPlatformFlagLock(async () => {
      try {
        const saved = await putAgentPublishing(request, {
          is_publish_blocked: true,
          publish_whitelist_project_ids: [1, 4],
        });
        expect(saved.status, saved.body).toBe(200);

        await openFeatures(page);
        await expect(page.getByRole('switch', { name: 'Block Agent Publishing' })).toBeChecked();
        const rows = page.getByTestId('admin-config-list-publish_whitelist_project_ids').getByRole('textbox');
        await expect(rows).toHaveCount(2);
        await expect(rows.nth(0)).toHaveValue('1');
        await expect(rows.nth(1)).toHaveValue('4');

        // The refresh a browser session or logout/login round trip both
        // amount to: a fresh load of the same server-stored section.
        await page.reload({ waitUntil: 'domcontentloaded' });
        await openFeatures(page);
        await expect(page.getByRole('switch', { name: 'Block Agent Publishing' })).toBeChecked();
        const rowsAfterReload = page
          .getByTestId('admin-config-list-publish_whitelist_project_ids')
          .getByRole('textbox');
        await expect(rowsAfterReload).toHaveCount(2);
        await expect(rowsAfterReload.nth(0)).toHaveValue('1');
        await expect(rowsAfterReload.nth(1)).toHaveValue('4');
      } finally {
        await restoreAgentPublishing(request);
      }
    });
  },
);
