/**
 * SKILL-1: a skill's own lifecycle through the UI — create it on the form,
 * read it back off the server, and delete it from its editor.
 *
 * Deliberately NOT given a `J<n>` title. Those numbers name entries in
 * `parity/manifest/*.json`, and the manifest holds no skills journey — JRNY-026
 * is a waived CHAT journey. A borrowed number would point a reader at the wrong
 * acceptance text.
 *
 * Ported BY USE CASE from the legacy public suite's single skills test,
 * `qa/elitea-testing-public/automation/tests/ui/skills/
 * test_skill_management.py::TestCreateSkill::
 * test_create_skill_and_verify_execution`.
 *
 * `e2e/journeys/skills/` was empty before this file. The one existing skill
 * journey lives in `agents.publishing.spec.ts` ("a created skill can be
 * attached to an agent…") and covers ATTACHING a skill that the API created —
 * nothing drove the skills form, and nothing deleted a skill.
 *
 * NOT PORTED, and not stubbed: the legacy test's middle three steps run the
 * skill in the test panel and assert the model answered in capitals. That
 * panel is `pages/skills/SkillTestPanel.tsx`, and `pages/skills/EditSkill.tsx`
 * mounts it behind `hasBackendCapability('llmPredictStreaming')`, which is OFF
 * — `POST /elitea_core/predict_llm/...` is served in its BLOCKING mode only,
 * and the streaming mode needs an `application_predict` socket.io transport
 * the Go stack does not have at all (`shared/config/backendCapabilities.ts`).
 * So the panel renders for nobody, and a journey for it would assert on a pane
 * that is deliberately absent. The uppercase assertion would not survive the
 * port either: CI runs against the mock LLM, whose reply does not depend on the
 * instructions.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

import type { APIRequestContext } from '@playwright/test';

/** A name unique per run, so two runs on one stack cannot collide. */
function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

/**
 * The names the server lists for this project.
 *
 * Read through the route the page itself calls, so "gone from the list" means
 * the row is gone rather than that a card scrolled out of view.
 */
async function skillNames(request: APIRequestContext): Promise<readonly string[]> {
  const response = await request.get(`${API_BASE}/elitea_core/skills/prompt_lib/${DEFAULT_PROJECT_ID}`);
  expect(response.ok(), `skills list returned ${response.status()}`).toBe(true);
  const body = await response.json();
  const rows: readonly { readonly name?: string }[] = body?.items ?? body?.rows ?? [];
  return rows.map((row) => row.name ?? '');
}

test('SKILL-1: a skill is created on the form, opens on its own page, and is deleted from there', async ({
  page,
  request,
}) => {
  const name = uniqueName('skill');
  let skillId: string | undefined;
  try {
    await page.goto(BASE_URL + '/app/skills/all');

    // The list header's own create control, not the sidebar's: this is the
    // path a person on the skills page takes.
    await page.getByRole('button', { name: 'Create skill' }).click();
    await page.waitForURL(/\/skills\/create/, { timeout: 15_000 });

    const nameInput = page.getByTestId('skill-name-input');
    await expect(nameInput).toBeVisible({ timeout: 15_000 });
    await nameInput.fill(name);
    await page.getByTestId('skill-description-input').fill('Created by the skills lifecycle journey.');
    await page.getByTestId('skill-instructions-input').fill('Answer in one short sentence.');

    // The POST itself, and the id it answered with — a form that navigated
    // without saving would still land on a URL.
    const createResponse = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' && /\/elitea_core\/skills\/prompt_lib\//.test(response.url()),
    );
    await page.getByRole('button', { name: 'Save' }).click();
    const created = await createResponse;
    expect([200, 201], await created.text()).toContain(created.status());
    skillId = String((await created.json())?.id);

    await page.waitForURL(new RegExp(`/skills/all/${skillId}`), { timeout: 15_000 });

    // The editor reads the stored skill back. Asserted after a RELOAD, because
    // the create page hands its own in-memory value to the editor on
    // navigation; only a reload re-reads the server.
    await page.reload();
    await expect(page.getByTestId('skill-name-input')).toHaveValue(name, { timeout: 20_000 });
    await expect(page.getByTestId('skill-instructions-input')).toHaveValue('Answer in one short sentence.');

    /*
     * DISCLOSED DEVIATION: the legacy test deletes through a three-dot
     * overflow menu. This app puts Delete straight on the editor toolbar
     * (`features/skills/ui/SkillEditorToolbar.tsx`) behind a confirmation
     * dialog that does NOT ask for the typed name — unlike the agent editor's,
     * which does. The use case is the same; the control is not.
     */
    const deleteResponse = page.waitForResponse(
      (response) =>
        response.request().method() === 'DELETE' && /\/elitea_core\/skill\/prompt_lib\//.test(response.url()),
    );
    await page.getByRole('button', { name: 'Delete', exact: true }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible({ timeout: 10_000 });
    await dialog.getByRole('button', { name: 'Delete', exact: true }).click();
    const removed = await deleteResponse;
    expect(removed.status(), await removed.text()).toBeLessThan(400);

    // Back on the list, and gone from it — on screen and on the server.
    await page.waitForURL(/\/skills\/all$/, { timeout: 15_000 });
    await expect(page.getByText(name)).toHaveCount(0, { timeout: 20_000 });
    await expect.poll(async () => (await skillNames(request)).includes(name), { timeout: 20_000 }).toBe(false);
    skillId = undefined;
  } finally {
    if (skillId !== undefined) {
      await request.delete(`${API_BASE}/elitea_core/skill/prompt_lib/${DEFAULT_PROJECT_ID}/${skillId}`);
    }
  }
});
