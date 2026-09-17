/**
 * Closed elitea_issues pins that live at the AGENT/SKILL boundary — the
 * agent editor's SKILLS section, not the skill editor itself. Kept out of
 * `skills.lifecycle.spec.ts`/`skills.versioning.spec.ts` (which drive the
 * skill's own page) and out of `agents.publishing.spec.ts` (owned by another
 * package's file) so this package's edits stay in files C-skills owns.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createAgent, deleteAgent } from '../../fixtures/api';

import type { Page } from '@playwright/test';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}skillint-${stem}-${String(Date.now()).slice(-7)}`;
}

async function openAgentEditor(page: Page, agentId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
}

/* elitea_issues: #5468 — "Save As Version" on an agent that has a Skill
   attached must carry that attachment onto the new version, the same way it
   already carries Toolkits and other configuration (Story 3/4: skill
   attachment is agent-version configuration, not a side channel).
   Reproduced against the current app: `SaveNewVersionButton.tsx`'s own doc
   comment records that the generated `SaveApplicationNewVersionBody` type
   LOOKS like it accepts a `copy_skills_from_version_id` passthrough field,
   but the real Go handler decodes the body into a plain `map[string]any`
   and reads only its own named keys — an unrecognised key is silently
   ignored — so nothing on either side ever asks the new version to copy the
   source version's skill attachments. This is a genuine, currently-present
   product gap, not a stale legacy-app description: `test.fail` below fails
   for exactly this reason (the new version's SKILLS section, and the
   `application_skills` read behind it, come back empty). */
test('a Skill attached to an agent survives Save As Version', async ({ page, request }) => {
  test.fail(
    true,
    '#5468: product gap — Save As Version does not preserve attached Skills (SaveNewVersionButton.tsx does not ' +
      'send copy_skills_from_version_id, and the Go version-create handler has no code path that would honour it ' +
      'if it did)',
  );

  const agentName = uniqueName('agent');
  const skillName = uniqueName('skill');
  const agent = await createAgent(request, agentName);

  const created = await request.post(`${API_BASE}/elitea_core/skills/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name: skillName,
      description: 'attached by the e2e journey, to survive Save As Version',
      versions: [{ name: 'base', instructions: 'Always answer in one sentence.' }],
    },
  });
  expect(created.ok(), `create skill returned ${created.status()}: ${await created.text()}`).toBe(true);
  const skill = await created.json();

  try {
    await openAgentEditor(page, agent.id);

    // Attach the skill to the base version.
    await expect(page.getByTestId('agent-skills-section')).toBeVisible({ timeout: 20_000 });
    await page.getByTestId('agent-add-skill-button').click();
    const attachResponse = page.waitForResponse(
      (response) => /\/skill\/prompt_lib\//.test(response.url()) && response.request().method() === 'PATCH',
    );
    await page.getByTestId(`agent-skill-option-${String(skill.id)}`).click();
    await attachResponse;
    await expect(page.getByTestId('agent-skills-counter')).toContainText('1/5', { timeout: 20_000 });

    // Save As Version.
    const versionName = uniqueName('v1').slice(0, 20);
    await page.getByRole('button', { name: 'Save As Version' }).click();
    const versionResponse = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' &&
        /\/versions\/prompt_lib\//.test(response.url()),
    );
    await page.getByLabel('Version name').fill(versionName);
    await page.getByRole('button', { name: 'Save', exact: true }).click();
    const versionRecorder = await versionResponse;
    expect(versionRecorder.ok(), await versionRecorder.text()).toBe(true);
    const newVersion = await versionRecorder.json();

    // The assertion `test.fail` above expects to fail: the new version's
    // skills read comes back empty, not carrying "skillName". A short
    // timeout here — this is expected to fail, not to be slow — keeps the
    // whole test inside its budget instead of burning the full default
    // timeout on a retrying assertion that was never going to pass.
    await expect(page.getByTestId('agent-skills-counter')).toContainText('1/5', { timeout: 3_000 });
    const readBack = await request.get(
      `${API_BASE}/elitea_core/application_skills/prompt_lib/${DEFAULT_PROJECT_ID}/${String(newVersion.id)}`,
    );
    const listed = await readBack.json();
    const items: readonly { readonly name?: string }[] = listed?.items ?? [];
    expect(items.map((item) => item.name)).toContain(skillName);
  } finally {
    await request.delete(`${API_BASE}/elitea_core/skill/prompt_lib/${DEFAULT_PROJECT_ID}/${String(skill.id)}`);
    await deleteAgent(request, agent.id);
  }
});
