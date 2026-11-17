/**
 * The Skills section of the conversation panel (skills epic 2169, ADR-0050
 * amendment 1, A1.3), against the REAL stack.
 *
 * It proves the part no unit test can: that the conversation-skills routes are
 * mounted in the production composition root and that the section reads, adds and
 * removes a pin through them. A 404 here would mean the routes are not registered,
 * and the web hides the section on a 404, so this journey is what would show it.
 *
 * Every entity carries the autotest prefix and is removed afterwards.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { openParticipants } from '../../fixtures/sidePanel';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createConversation,
  deleteConversation,
} from '../../fixtures/api';

const SUFFIX = '-convskills';

test('a skill is pinned to a conversation, shown apart from the participants, and removed', async ({ page }) => {
  const request = page.request;
  const skillName = `${AUTOTEST_PREFIX}pinned-skill${SUFFIX}-${Date.now()}`;
  const created = await request.post(`${API_BASE}/elitea_core/skills/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name: skillName,
      description: `${AUTOTEST_PREFIX}conversation skill fixture`,
      versions: [{ name: 'base', instructions: 'Answer in one short sentence.', tags: [] }],
    },
  });
  expect(created.status()).toBe(201);
  const skillId = Number((await created.json()).id);
  const conversationId = await createConversation(request, `${AUTOTEST_PREFIX}convskills-${Date.now()}`);
  const pinsPath = `${API_BASE}/elitea_core/conversation_skills/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`;

  try {
    // The route is mounted: an empty list for a fresh conversation, not a 404.
    const empty = await request.get(pinsPath);
    expect(empty.status()).toBe(200);
    expect(((await empty.json()) as { rows: unknown[] }).rows).toEqual([]);

    // Pin through the API, as another client would.
    const pinned = await request.post(pinsPath, { data: { skill_id: skillId } });
    expect(pinned.status()).toBe(201);
    const pin = (await pinned.json()) as { id: number; kind: string; name: string; available: boolean };
    expect(pin).toMatchObject({ kind: 'project', name: skillName, available: true });
    // Pinning again answers the same pin.
    const again = await request.post(pinsPath, { data: { skill_id: skillId } });
    expect(again.status()).toBe(200);
    expect(((await again.json()) as { id: number }).id).toBe(pin.id);

    // The conversation panel shows it in its own section, not among the participants.
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await openParticipants(page);
    const section = page.getByTestId('conversation-skills');
    await expect(section).toBeVisible({ timeout: 20_000 });
    await expect(section.getByText(skillName)).toBeVisible();
    await expect(page.getByTestId('participants-container').getByText(skillName)).toHaveCount(0);

    // Removing it from the section removes it on the server.
    await page.getByTestId(`conversation-skill-remove-${pin.id}`).click();
    await expect(section.getByText(skillName)).toHaveCount(0, { timeout: 10_000 });
    const after = await request.get(pinsPath);
    expect(((await after.json()) as { rows: unknown[] }).rows).toEqual([]);
  } finally {
    await deleteConversation(request, conversationId);
    await request.delete(`${API_BASE}/elitea_core/skill/prompt_lib/${DEFAULT_PROJECT_ID}/${skillId}`);
  }
});
