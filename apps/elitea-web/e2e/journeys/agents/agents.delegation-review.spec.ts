/**
 * Attaching a specialist agent: the review of what it brings (ADR-0044
 * amendment 1, A1.2 rule 2; epic 2156 story S3, issue 2159).
 *
 * The standard specialist pattern: Agent1 holds a git toolkit, Agent2 holds
 * none, and Agent2's editor attaches Agent1 on purpose to get git without
 * holding it. Before this story the attach wrote the link and a pending grant
 * nobody saw. Now the editor reviews what Agent1 will be able to use, approves
 * it in one step, and the row says where the edge stands.
 *
 * ## Asserted on the SERVER
 *
 * Each case reads the grant back over the API (`delegation_grants`), because a
 * dialog that closes and a row that says "approved" are both client state. The
 * discriminating reads are: the stored grant's state, what it lists, and what
 * it left out; and, for the cancelled review, that no link and no grant exist.
 *
 * What is NOT driven here: the approver who lacks access to a toolkit or a
 * credential. That needs a second user whose project role lacks the toolkit
 * permission, which this stack's one signed-in admin cannot be. It is covered
 * against a real Postgres (`delegation_access` and the folder access overlay,
 * in `worker_principal_postgres_integration_test.go`) and in the dialog's unit
 * tests (disabled, with the reason). The governed profile is a stack flag this
 * journey does not own.
 */
import { expect, test, type Page } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  createAgent,
  createAgentThroughForm,
  createGithubToolkit,
  deleteAgent,
  deleteGithubToolkit,
  DEFAULT_PROJECT_ID,
  type GithubToolkitFixture,
} from '../../fixtures/api';

const RUN_ID = String(Date.now()).slice(-6);

interface StoredGrant {
  readonly id: number;
  readonly callee_application_id: number;
  readonly state: string;
  readonly items: readonly { readonly kind: string; readonly name: string }[];
  readonly omitted: readonly { readonly kind: string; readonly name: string }[];
}

interface Fixture {
  readonly projectId: string;
  readonly parentId: string;
  readonly specialistId: string;
  readonly specialistName: string;
  readonly toolkits: readonly GithubToolkitFixture[];
}

async function readGrants(page: Page, fixture: Fixture): Promise<readonly StoredGrant[]> {
  const response = await page.request.get(
    `${API_BASE}/elitea_core/delegation_grants/prompt_lib/${fixture.projectId}/${fixture.parentId}`,
  );
  expect(response.ok(), `the grant list was refused: ${(await response.text()).slice(0, 300)}`).toBe(true);
  return ((await response.json()) as { grants?: readonly StoredGrant[] }).grants ?? [];
}

/**
 * The specialist holds `toolkitCount` github toolkits (over the API, as the
 * Tools panel's own attach would write them); the parent holds none and is
 * opened in the editor.
 */
async function openParentWithSpecialist(page: Page, suffix: string, toolkitCount: number): Promise<Fixture> {
  const toolkits: GithubToolkitFixture[] = [];
  for (let index = 0; index < toolkitCount; index += 1) {
    toolkits.push(
      await createGithubToolkit(page.request, DEFAULT_PROJECT_ID, `${AUTOTEST_PREFIX}dg_${suffix}${index}_${RUN_ID}`),
    );
  }
  const specialistName = `${AUTOTEST_PREFIX}spec_${suffix}_${RUN_ID}`;
  const specialist = await createAgent(page.request, specialistName);
  for (const toolkit of toolkits) {
    const attached = await page.request.patch(
      `${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${toolkit.toolkitId}`,
      {
        data: {
          entity_version_id: Number(specialist.versionId),
          entity_id: Number(specialist.id),
          entity_type: 'agent',
          has_relation: true,
        },
      },
    );
    expect(attached.ok(), `the toolkit attach was refused: ${(await attached.text()).slice(0, 300)}`).toBe(true);
  }

  const parent = await createAgentThroughForm(page, `${AUTOTEST_PREFIX}par_${suffix}_${RUN_ID}`);
  await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 30_000 });
  return {
    projectId: parent.projectId,
    parentId: parent.agentId,
    specialistId: specialist.id,
    specialistName,
    toolkits,
  };
}

async function chooseSpecialist(page: Page, fixture: Fixture): Promise<void> {
  const picker = page.getByRole('button', { name: 'Agent', exact: true });
  await expect(picker).toBeEnabled({ timeout: 30_000 });
  await picker.click();
  await page.getByPlaceholder('Search agents...').fill(fixture.specialistName);
  await page.getByRole('menuitem', { name: new RegExp(fixture.specialistName) }).first().click();
}

async function cleanUp(page: Page, fixture: Fixture | undefined): Promise<void> {
  if (fixture === undefined) return;
  await deleteAgent(page.request, fixture.parentId).catch(() => {});
  await deleteAgent(page.request, fixture.specialistId).catch(() => {});
  for (const toolkit of fixture.toolkits) {
    await deleteGithubToolkit(page.request, fixture.projectId, toolkit).catch(() => {});
  }
}

test('DR-1: attaching an agent that brings git shows what it brings, and Approve and attach approves it in one step', async ({
  page,
}) => {
  test.setTimeout(240_000);
  let fixture: Fixture | undefined;
  try {
    fixture = await openParentWithSpecialist(page, 'one', 1);
    await chooseSpecialist(page, fixture);

    const dialog = page.getByRole('dialog', { name: `What ${fixture.specialistName} will be able to use` });
    await expect(dialog, 'the review must open before anything is attached').toBeVisible({ timeout: 30_000 });
    await expect(dialog.getByRole('checkbox', { name: fixture.toolkits[0]?.toolkitName ?? '' })).toBeChecked();
    expect(await readGrants(page, fixture), 'nothing may be written before the editor decides').toEqual([]);

    await dialog.getByRole('button', { name: 'Approve and attach' }).click();
    await expect(dialog).toBeHidden({ timeout: 30_000 });

    // The row says where the edge stands…
    const status = page.getByTestId('delegation-grant-status');
    await expect(status).toHaveAttribute('data-grant-state', 'approved', { timeout: 30_000 });
    await expect(status).toContainText('Delegation approved');

    // …and the server agrees: one approved grant, listing the toolkit, leaving nothing out.
    const grants = await readGrants(page, fixture);
    expect(grants).toHaveLength(1);
    expect(grants[0]?.state).toBe('approved');
    expect(grants[0]?.callee_application_id).toBe(Number(fixture.specialistId));
    expect(grants[0]?.items.map((item) => item.name)).toContain(`toolkit:${fixture.toolkits[0]?.toolkitId}`);
    expect(grants[0]?.omitted).toEqual([]);

    // It survives a reload: the row is drawn from the server.
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('delegation-grant-status')).toHaveAttribute('data-grant-state', 'approved', {
      timeout: 30_000,
    });
  } finally {
    await cleanUp(page, fixture);
  }
});

test('DR-2: leaving a toolkit unticked approves only the rest, and Review delegation shows what was approved', async ({
  page,
}) => {
  test.setTimeout(240_000);
  let fixture: Fixture | undefined;
  try {
    fixture = await openParentWithSpecialist(page, 'two', 2);
    await chooseSpecialist(page, fixture);

    const dialog = page.getByRole('dialog', { name: `What ${fixture.specialistName} will be able to use` });
    await expect(dialog).toBeVisible({ timeout: 30_000 });
    const [first, second] = fixture.toolkits;
    await dialog.getByRole('checkbox', { name: second?.toolkitName ?? '' }).click();
    await dialog.getByRole('button', { name: 'Approve and attach' }).click();
    await expect(dialog).toBeHidden({ timeout: 30_000 });

    await expect(page.getByTestId('delegation-grant-status')).toHaveAttribute('data-grant-state', 'approved', {
      timeout: 30_000,
    });
    const grants = await readGrants(page, fixture);
    expect(grants).toHaveLength(1);
    const kept = grants[0]?.items.map((item) => item.name) ?? [];
    expect(kept).toContain(`toolkit:${first?.toolkitId}`);
    expect(kept, 'the unticked toolkit must stay out of the grant').not.toContain(`toolkit:${second?.toolkitId}`);
    expect(grants[0]?.omitted.map((item) => item.name)).toContain(`toolkit:${second?.toolkitId}`);

    // The row's Review delegation opens the same dialog, read-only for an approved grant.
    await page.getByTestId('delegation-review-button').click();
    const review = page.getByRole('dialog');
    await expect(review).toBeVisible({ timeout: 30_000 });
    await expect(review.getByText(/left out of the approval/)).toBeVisible();
    await expect(review.getByRole('button', { name: 'Approve' })).toHaveCount(0);
  } finally {
    await cleanUp(page, fixture);
  }
});

test('DR-3: cancelling the review attaches nothing and writes no grant', async ({ page }) => {
  test.setTimeout(240_000);
  let fixture: Fixture | undefined;
  try {
    fixture = await openParentWithSpecialist(page, 'cx', 1);
    await chooseSpecialist(page, fixture);

    const dialog = page.getByRole('dialog', { name: `What ${fixture.specialistName} will be able to use` });
    await expect(dialog).toBeVisible({ timeout: 30_000 });
    await dialog.getByRole('button', { name: 'Cancel' }).click();
    await expect(dialog).toBeHidden({ timeout: 15_000 });

    await expect(page.getByTestId('delegation-grant-status')).toHaveCount(0);
    expect(await readGrants(page, fixture)).toEqual([]);
  } finally {
    await cleanUp(page, fixture);
  }
});
