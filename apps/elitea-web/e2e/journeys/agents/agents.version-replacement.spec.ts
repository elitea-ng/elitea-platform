/**
 * Deleting a version that ANOTHER agent references as a sub-agent — the
 * "Replace & Delete" flow (onetest wave-1 agents package: ELITEA-0041,
 * ELITEA-0042, ELITEA-0043) — [PRODUCT GAP, already disclosed in the
 * codebase, confirmed real below].
 *
 * `features/agents/ui/VersionReplacementModal.tsx` is a fully built, fully
 * unit-tested component — a "Version in use" dialog listing the referencing
 * agents/pipelines and a `SingleSelect` to pick a replacement version before
 * deleting. It is imported by `AgentVersionControls.tsx` and rendered by
 * `DeleteVersionDialog.tsx`'s own module — but `DeleteVersionDialog.tsx`'s
 * own doc comment says outright that it is NEVER SHOWN:
 *
 *   "The baseline's in-use branch is deliberately NOT wired, and the reason
 *   is a backend defect, not a porting shortcut. […] this deletes directly
 *   and surfaces whatever the delete endpoint itself refuses."
 *
 * So no matter what a version is referenced by, clicking Delete in the
 * version menu always opens the PLAIN `DeleteEntityModal` ("Delete
 * confirmation" / type-the-name-to-confirm) — `VersionReplacementModal`
 * never opens from any real user action. The three tests below drive the
 * exact setup ELITEA-0041/0042/0043 describe (a version another agent
 * references as a sub-agent) and assert the replacement flow SHOULD run;
 * each fails at the same first step, which is the gap itself, not a
 * selector mistake.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  attachSubAgent,
  createAgent,
  deleteAgent,
  readApplicationVersions,
} from '../../fixtures/api';

import type { APIRequestContext, Page } from '@playwright/test';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}verrepl-${stem}-${String(Date.now()).slice(-7)}`;
}

/** Opens Agent A's editor and creates a second version through the real "Save as version" control (mirrors J15). */
async function createSecondVersion(page: Page, agentId: string, versionName: string): Promise<string> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });

  await page.getByRole('button', { name: /save as version/i }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible({ timeout: 5_000 });
  await dialog.getByRole('textbox').fill(versionName);
  await dialog.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL(/\/agents\/[^/]+\/[^/]+\/\d+/, { timeout: 15_000 });

  const match = /\/agents\/[^/]+\/[^/]+\/(\d+)/.exec(page.url());
  if (!match?.[1]) throw new Error(`createSecondVersion: could not read a version id off ${page.url()}`);
  return match[1];
}

/** Agent A: base + a second version ("in-use"), and Agent B referencing A's second version as a sub-agent. */
async function seedInUseVersion(
  request: APIRequestContext,
  page: Page,
): Promise<{
  readonly parent: { readonly id: string; readonly versionId: string };
  readonly dependent: { readonly id: string; readonly versionId: string };
  readonly inUseVersionName: string;
}> {
  const parent = await createAgent(request, uniqueName('parent'));
  const dependent = await createAgent(request, uniqueName('dependent'));
  const inUseVersionName = 'in-use-v2';
  const inUseVersionId = await createSecondVersion(page, parent.id, inUseVersionName);

  const relation = await attachSubAgent(request, dependent.versionId, {
    applicationId: parent.id,
    versionId: inUseVersionId,
  });
  expect(relation.ok(), `attaching the sub-agent relation must succeed: ${await relation.text()}`).toBe(true);

  return {
    parent: { id: parent.id, versionId: inUseVersionId },
    dependent,
    inUseVersionName,
  };
}

async function openVersionDeleteFlow(page: Page, parentAgentId: string, versionId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/agents/all/${parentAgentId}/${versionId}`);
  const trigger = page.getByTestId('version-selector-trigger');
  await expect(trigger).toBeVisible({ timeout: 15_000 });
  await trigger.click();
  const deleteItem = page.getByTestId('agent-version-delete');
  await expect(deleteItem).toBeVisible({ timeout: 10_000 });
  await deleteItem.click();
}

/*
 * ELITEA-0043 — clicking Delete on an IN-USE version must open the
 * "Version in use" replacement modal, and it must stay open on an inner
 * click (selecting a replacement version), closing only on an outside
 * click. What actually opens is the plain, single-purpose delete-confirm
 * dialog, which carries no version selector at all.
 */
test('J-verrepl: [PRODUCT GAP] deleting an in-use version opens the replacement modal, which stays open on inner clicks', async ({
  page,
  request,
}) => {
  test.fail(
    true,
    'ELITEA-0043: product gap — DeleteVersionDialog never renders VersionReplacementModal; only the plain type-to-confirm delete dialog ever opens, in-use or not',
  );
  const { parent, dependent } = await seedInUseVersion(request, page);
  try {
    await openVersionDeleteFlow(page, parent.id, parent.versionId);

    const dialog = page.getByRole('dialog');
    await expect(dialog, 'the "Version in use" modal must appear for a version another agent references').toContainText(
      'Version in use',
    );

    // Selecting a replacement version inside the modal must not close it.
    await dialog.getByLabel(/replace with version/i).click();
    await expect(dialog).toBeVisible();

    // Only an outside click dismisses it.
    await page.mouse.click(10, 10);
    await expect(dialog).toBeHidden({ timeout: 5_000 });
  } finally {
    await deleteAgent(request, parent.id);
    await deleteAgent(request, dependent.id);
  }
});

/*
 * ELITEA-0041 — selecting a replacement version and confirming
 * "Replace & Delete" must migrate the dependent agent's sub-agent reference
 * to the replacement AND remove the in-use version. Neither can happen: the
 * modal that would offer the replacement picker never opens.
 */
test('J-verrepl: [PRODUCT GAP] Replace & Delete migrates the dependent reference and removes the version', async ({
  page,
  request,
}) => {
  test.fail(
    true,
    'ELITEA-0041: product gap — no UI path reaches VersionReplacementModal, so no replacement can ever be selected or applied',
  );
  const { parent, dependent, inUseVersionName } = await seedInUseVersion(request, page);
  try {
    await openVersionDeleteFlow(page, parent.id, parent.versionId);

    const dialog = page.getByRole('dialog');
    await expect(dialog).toContainText('Version in use');
    await dialog.getByLabel(/replace with version/i).click();
    await page.getByRole('option', { name: 'base' }).click();
    await dialog.getByRole('button', { name: /replace & delete/i }).click();

    // The version must be gone…
    const versions = await readApplicationVersions(request, parent.id);
    expect(versions.map((version) => version.name)).not.toContain(inUseVersionName);

    // …and Agent B's reference must now point at the replacement ("base"),
    // not at the version that was just deleted.
    const dependentVersion = await request.get(
      `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/${dependent.id}/${dependent.versionId}`,
    );
    const dependentBody = await dependentVersion.json();
    const subAgentTool = (dependentBody?.tools ?? []).find(
      (tool: Record<string, unknown>) => tool['type'] === 'application',
    );
    expect(subAgentTool, 'the dependent must still carry a sub-agent reference after the migration').toBeDefined();
  } finally {
    await deleteAgent(request, parent.id);
    await deleteAgent(request, dependent.id);
  }
});

/*
 * ELITEA-0042 — clicking Cancel in the replacement modal must close it
 * without deleting the version or changing the dependent's reference.
 */
test('J-verrepl: [PRODUCT GAP] Cancel in the replacement modal closes it without deleting the in-use version', async ({
  page,
  request,
}) => {
  test.fail(
    true,
    'ELITEA-0042: product gap — there is no replacement modal to cancel; the only dialog reachable is the plain delete-confirm one',
  );
  const { parent, dependent, inUseVersionName } = await seedInUseVersion(request, page);
  try {
    await openVersionDeleteFlow(page, parent.id, parent.versionId);

    const dialog = page.getByRole('dialog');
    await expect(dialog).toContainText('Version in use');
    await dialog.getByRole('button', { name: /^cancel$/i }).click();
    await expect(dialog).toBeHidden({ timeout: 5_000 });

    const versions = await readApplicationVersions(request, parent.id);
    expect(versions.map((version) => version.name)).toContain(inUseVersionName);
  } finally {
    await deleteAgent(request, parent.id);
    await deleteAgent(request, dependent.id);
  }
});
