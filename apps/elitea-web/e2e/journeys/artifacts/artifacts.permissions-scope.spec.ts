/**
 * Journey: "Manage access" should be a Team-project-only action.
 *
 * `BucketList.tsx` renders the "Manage access" icon button
 * (`aria-label="Manage access to ${name}"`) unconditionally for every bucket
 * row — there is no project-type/kind check anywhere in
 * `features/artifacts` (`BucketList.tsx`, `BucketSidebar.tsx`) gating it to
 * Team projects. This is provable without any new PROJECT fixture: every
 * signed-in persona in this stack already owns a genuinely PRIVATE project —
 * its own personal project (`ensurePersonalProject`; `GET /social/author`'s
 * `personal_project_id`) — next to the shared "Default Project" every journey
 * in this suite already treats as the Team-type project. The personal
 * project's own two system buckets (`reports`, `tasks`) are filtered out of
 * the sidebar entirely (`isSystemBucket`, confirmed against the running
 * stack: the panel renders "No buckets found" for a fresh personal project),
 * so a bucket is created there the same way `TEAM_BUCKET` is.
 */
import { test, expect, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

const ARTIFACTS_URL = `${BASE_URL}/app/artifacts`;
// Run-unique — see the sibling pinning/upload-conflict specs' own note on
// `--repeat-each=2` racing a fixed bucket name across parallel workers.
const RUN_TOKEN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}`;
const TEAM_BUCKET = `autotest-manage-access-scope-${RUN_TOKEN}`;
const PERSONAL_BUCKET = `autotest-manage-access-personal-${RUN_TOKEN}`;

async function openArtifacts(page: Page): Promise<void> {
  const response = await page.goto(ARTIFACTS_URL);
  expect(response?.status(), 'the artifacts page must be served at /app/artifacts').toBeLessThan(400);
}

async function selectedProjectId(page: Page): Promise<string> {
  const id = await page.evaluate(() => localStorage.getItem('el.project.id'));
  expect(id, 'app-shell must have persisted a selected project').not.toBeNull();
  return id as string;
}

async function ensureBucket(request: APIRequestContext, projectId: string, name: string): Promise<void> {
  const created = await request.post(`/api/v2/artifacts/buckets/${projectId}`, { data: { name } });
  expect([200, 201, 409]).toContain(created.status());
}

/* onetest: ELITEA-2475 — "Manage access" must be visible only in Team projects; PRODUCT GAP: the icon renders unconditionally regardless of project type */
test('bucket "Manage access" is visible in the shared Team project but must be absent in the caller\'s own private project', async ({
  page,
  request,
}) => {
  test.fail(
    true,
    'ELITEA-2475: product gap — BucketList.tsx (features/artifacts/ui) renders the "Manage access" icon for every bucket row with no project-type condition; it is not restricted to Team projects.',
  );

  let teamProjectId = '';
  let personalProjectId = '';
  try {
    // Half 1 (must PASS): the action is visible in the shared Team project —
    // already the state every other artifacts journey in this suite relies on.
    await openArtifacts(page);
    teamProjectId = await selectedProjectId(page);
    await ensureBucket(request, teamProjectId, TEAM_BUCKET);
    await page.reload();
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    await expect(page.getByLabel(`Manage access to ${TEAM_BUCKET}`)).toBeVisible({ timeout: 15_000 });

    // Half 2 (the actual claim under test — expected to FAIL here): switch
    // into the caller's OWN personal project via the sidebar's project
    // switcher (`ProjectSwitcher.tsx`; `orderedProjectOptions` renames a
    // `project_user_<id>` row to "Private" exactly when it is the CALLER's
    // own `personal_project_id` — verified against `widgets/sidebar/lib/
    // projectOptions.ts` before writing this test), then assert the action
    // is gone from a bucket there.
    await page.getByRole('button', { name: /Project:/ }).click();
    // The option's accessible name is prefixed with the avatar's
    // initial-letter text (`ProjectAvatar`, same shape as the trigger's own
    // "D Project: Default Project" — measured against the running stack), so
    // this matches on containment rather than an exact "Private".
    const privateOption = page.getByRole('option', { name: 'Private' });
    await expect(privateOption).toBeVisible({ timeout: 10_000 });
    await privateOption.click();
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });

    personalProjectId = await selectedProjectId(page);
    await ensureBucket(request, personalProjectId, PERSONAL_BUCKET);
    await page.reload();
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    await expect(page.getByRole('button', { name: PERSONAL_BUCKET, exact: true })).toBeVisible({ timeout: 15_000 });
    await expect(page.getByLabel(`Manage access to ${PERSONAL_BUCKET}`)).toHaveCount(0);
  } finally {
    // Run-unique names (see the constants' own note), but still worth
    // sweeping rather than leaving one bucket per run in each project.
    if (teamProjectId !== '') {
      await request.delete(`/api/v2/artifacts/buckets/${teamProjectId}/${TEAM_BUCKET}`).catch(() => undefined);
    }
    if (personalProjectId !== '') {
      await request.delete(`/api/v2/artifacts/buckets/${personalProjectId}/${PERSONAL_BUCKET}`).catch(() => undefined);
    }
  }
});
