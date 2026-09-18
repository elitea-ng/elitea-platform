/**
 * SW-sweep: does an "editor" project role let a user delete an ENTITY it did
 * not create? (elitea_issues #3421, checklist #3419 — legacy pylon reports.)
 *
 * The legacy bug report says the Editor role could delete agents/pipelines/
 * credentials/toolkits/artifacts created by OTHER users, not only its own.
 * elitea-main's RBAC (`legacyrbac`, `shared/0068` etc.) grants permissions
 * like `models.prompts.prompt.delete` per ROLE, scoped to the PROJECT — there
 * is no per-row "owner" check layered on top anywhere in
 * `internal/api/v2/eliteacore` or the applications repo (grepped: no
 * `created_by`/`CreatedBy` comparison gates any delete handler). So the
 * legacy report's premise (an ownership-scoped delete permission) was never
 * implemented as a concept in the Go platform: any project role that HOLDS
 * the delete permission can delete ANY entity of that type in the project,
 * by design of the flat role-permission model.
 *
 * This test proves that reading of the code with a real HTTP call: an
 * "editor" on a fresh project deletes an agent the project's ADMIN created.
 */
import { test, expect, request as apiRequest } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, createAgent, readCallerIdentity } from '../../fixtures/api';
import { createScratchProject, deleteScratchProject, type ScratchProject } from '../../fixtures/scratchProject';

/* elitea_issues: #3421 — an editor must not be able to delete an entity another project member created */
test('SW-3421: an editor cannot delete an agent created by the project admin', async () => {
  test.fail(true, '#3421: product gap — RBAC has no per-owner delete gate; any role holding the delete permission can delete any entity of that type in the project');
  test.setTimeout(90_000);
  let project: ScratchProject | undefined;
  const admin = await apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.admin });
  const editor = await apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.viewer });
  try {
    project = await createScratchProject('editor-deletion');

    // The scratch project already made this persona a `viewer` there
    // (`grantViewer`). Re-point that SAME membership row at `editor` with the
    // role-change route the member dialog itself submits (PUT, not POST —
    // POST refuses an existing member with "already exists in project").
    const { id: editorUserId } = await readCallerIdentity(editor);
    const roleChange = await admin.put(`${API_BASE}/admin/users/administration/${project.id}`, {
      data: { userId: editorUserId, roles: ['editor'] },
    });
    expect(
      roleChange.ok(),
      `changing the persona's role to 'editor' -> ${roleChange.status()} ${(await roleChange.text()).slice(0, 400)}`,
    ).toBe(true);

    // ADMIN creates the agent (the "other user"'s entity).
    const agent = await createAgent(admin, `${AUTOTEST_PREFIX}sweep_3421_${Date.now()}`);

    // EDITOR (not the creator) deletes it.
    const del = await editor.delete(
      `${API_BASE}/elitea_core/application/prompt_lib/${project.id}/${agent.id}`,
    );

    // Written as the issue says it SHOULD behave (refused with 403). It is
    // not: elitea-main answers 204 — the delete succeeds. `test.fail` above
    // makes this failure the expected, pinned outcome instead of a red CI run.
    expect(del.status(), 'editor delete of another user\'s agent must be refused').toBe(403);
  } finally {
    await deleteScratchProject(project);
    await admin.dispose();
    await editor.dispose();
  }
});
