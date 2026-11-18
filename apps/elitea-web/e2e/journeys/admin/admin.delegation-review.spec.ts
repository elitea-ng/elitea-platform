/**
 * Delegations awaiting review (ADR-0044 Amendment 1, A1.3; epic 2156 story S6,
 * issue 2162): the migration of existing compositions leaves a PENDING grant,
 * flagged legacy, for every attached agent that holds more than its caller, and
 * approves none. The administrator sees the queue; the owner of the calling agent
 * sees a banner and approves the delegation; approval clears it from both.
 *
 * ## The seeded legacy edge
 *
 * An edge that existed BEFORE the upgrade is an attachment with no grant. This
 * stack is already upgraded, so the journey makes the same state: Agent2 (the
 * caller, holding nothing) attaches Agent1 (the specialist, holding a toolkit)
 * through the relation route, which proposes a grant; that grant is then revoked,
 * which leaves the attachment with no active grant, exactly what an existing
 * composition looked like. The administrator's "Scan compositions again" is the
 * migration run on demand: it writes the pending legacy grant.
 *
 * ## Asserted on the SERVER
 *
 * Each step reads the grant back over the API: the stored state, the legacy flag
 * and the origin are what the migration promises; a banner and a table row are
 * both client state. The discriminating reads are that the grant is `pending`
 * (never `approved`) after the scan, and that approval removes the edge from the
 * administrator's queue.
 *
 * What is NOT driven here: the governed profile (a stack flag this journey does
 * not own; the maker-checker of a governed legacy grant is covered against a real
 * Postgres in `delegation_edges_postgres_integration_test.go`) and the startup run
 * itself (it ran before this journey existed; the scan is the same code).
 */
import { expect, test, type APIRequestContext } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  attachSubAgent,
  createAgent,
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
  readonly legacy: boolean;
  readonly origin: string;
}

interface Review {
  readonly pending: number;
  readonly legacy: number;
  readonly projects: readonly {
    readonly project_id: number;
    readonly pending: number;
    readonly callers: readonly { readonly application_id: number; readonly pending: number }[];
  }[];
}

async function readGrants(request: APIRequestContext, callerId: string): Promise<readonly StoredGrant[]> {
  const response = await request.get(
    `${API_BASE}/elitea_core/delegation_grants/prompt_lib/${DEFAULT_PROJECT_ID}/${callerId}`,
  );
  expect(response.ok(), `the grant list was refused: ${(await response.text()).slice(0, 300)}`).toBe(true);
  return ((await response.json()) as { grants?: readonly StoredGrant[] }).grants ?? [];
}

async function readReview(request: APIRequestContext): Promise<Review> {
  const response = await request.get(`${API_BASE}/admin/delegation_review`);
  expect(response.ok(), `the review queue was refused: ${(await response.text()).slice(0, 300)}`).toBe(true);
  return (await response.json()) as Review;
}

function callerOf(review: Review, callerId: string): { readonly pending: number } | undefined {
  return review.projects
    .find((project) => String(project.project_id) === DEFAULT_PROJECT_ID)
    ?.callers.find((caller) => String(caller.application_id) === callerId);
}

test('DLR-1: a seeded legacy edge is pending, shown to the administrator and to the caller, and approval clears it', async ({
  page,
  browser,
}) => {
  test.setTimeout(300_000);
  const admin = await browser.newContext({ storageState: STORAGE_STATE.admin });
  const adminPage = await admin.newPage();
  const created: string[] = [];
  let toolkit: GithubToolkitFixture | undefined;
  try {
    // The specialist holds a toolkit; the caller holds none.
    toolkit = await createGithubToolkit(page.request, DEFAULT_PROJECT_ID, `${AUTOTEST_PREFIX}dlr_${RUN_ID}`);
    const specialist = await createAgent(page.request, `${AUTOTEST_PREFIX}dlr_spec_${RUN_ID}`);
    created.push(specialist.id);
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
    const caller = await createAgent(page.request, `${AUTOTEST_PREFIX}dlr_caller_${RUN_ID}`);
    created.push(caller.id);

    // The attachment proposes a grant; revoking it leaves the edge as an existing
    // composition has it: attached, with no active grant.
    const link = await attachSubAgent(
      page.request,
      { id: caller.id, versionId: caller.versionId },
      { applicationId: specialist.id, versionId: specialist.versionId },
    );
    expect(link.ok(), `the attach was refused: ${(await link.text()).slice(0, 300)}`).toBe(true);
    const proposed = await readGrants(page.request, caller.id);
    expect(proposed).toHaveLength(1);
    const revoked = await page.request.delete(
      `${API_BASE}/elitea_core/delegation_grant/prompt_lib/${DEFAULT_PROJECT_ID}/${caller.id}/${proposed[0]?.id}`,
    );
    expect(revoked.ok(), `the revoke was refused: ${(await revoked.text()).slice(0, 300)}`).toBe(true);
    expect(await readGrants(page.request, caller.id)).toEqual([]);

    // The migration, run on demand by the administrator.
    const scan = await adminPage.request.post(`${API_BASE}/admin/delegation_review/scan`);
    expect(scan.ok(), `the scan was refused: ${(await scan.text()).slice(0, 300)}`).toBe(true);
    const grants = await readGrants(page.request, caller.id);
    expect(grants).toHaveLength(1);
    expect(grants[0]).toMatchObject({
      callee_application_id: Number(specialist.id),
      state: 'pending',
      legacy: true,
      origin: 'migration',
    });
    expect(grants.some((grant) => grant.state === 'approved'), 'the migration must never approve').toBe(false);

    // The administrator's queue lists the caller, over the API and on the page.
    expect(callerOf(await readReview(adminPage.request), caller.id)?.pending).toBe(1);
    await adminPage.goto(`${BASE_URL}/admin/app/delegation-review`, { waitUntil: 'domcontentloaded' });
    const open = adminPage.getByTestId(`delegation-review-open-${DEFAULT_PROJECT_ID}-${caller.id}`);
    await expect(open).toBeVisible({ timeout: 30_000 });
    await expect(adminPage.getByTestId('delegation-review-count')).toContainText('from the migration of existing compositions');

    // The caller's owner sees the banner and the row, pending.
    await page.goto(`${BASE_URL}/app/agents/all/${caller.id}`, { waitUntil: 'domcontentloaded' });
    const banner = page.getByTestId('delegation-pending-banner');
    await expect(banner).toBeVisible({ timeout: 30_000 });
    await expect(banner).toContainText('from before this release await review');
    await expect(page.getByTestId('delegation-grant-status')).toHaveAttribute('data-grant-state', 'pending');

    // Approving it in Review delegation clears the banner and the queue.
    await page.getByTestId('delegation-review-button').click();
    const dialog = page.getByTestId('delegation-review-dialog');
    await expect(dialog).toBeVisible({ timeout: 30_000 });
    await dialog.getByRole('button', { name: 'Approve', exact: true }).click();
    await expect(page.getByTestId('delegation-grant-status')).toHaveAttribute('data-grant-state', 'approved', {
      timeout: 30_000,
    });
    await expect(banner).toHaveCount(0);
    const approved = await readGrants(page.request, caller.id);
    expect(approved).toHaveLength(1);
    expect(approved[0]?.state).toBe('approved');
    await expect.poll(async () => callerOf(await readReview(adminPage.request), caller.id), { timeout: 30_000 }).toBeUndefined();
  } finally {
    for (const id of created) await deleteAgent(page.request, id).catch(() => {});
    if (toolkit !== undefined) await deleteGithubToolkit(page.request, DEFAULT_PROJECT_ID, toolkit).catch(() => {});
    await admin.close();
  }
});
