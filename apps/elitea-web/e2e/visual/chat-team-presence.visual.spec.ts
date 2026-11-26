/**
 * Snapshot coverage for a chat's agents with live presence (ADR-0061 decisions 4c and 6):
 *
 *  - `chat-live-avatars` (#3067): two agents of an open conversation, one "Running jira_get_issue" on its row with the
 *    active badge, the other "Waiting for Mira";
 *  - `chat-local-work-team-readonly` (#3069): a desktop Local work thread opened on the web: the participants show
 *    where each agent runs (Local or Cloud) and the Lead chip, with no lead menu or other edit.
 *
 * Like `chat-message-classes.visual.spec.ts`, this file creates NO server state: it serves the conversation's reads
 * for ids no tenant holds, and the live states come from a held-open fixture stream (`fixtures/teamPresence.ts`).
 * Reduced motion makes every badge its still, so the shot is the same frame on every run; the motions are asserted in
 * `AgentAvatar.test.tsx`.
 */
import { expect, type Page } from '@playwright/test';

import { test } from './lib/test';

import { BASE_URL } from '../../playwright.config';
import { serveTeamPresence } from '../fixtures/teamPresence';
import { SNAPSHOT_TOLERANCE, settle, shellSettled, volatileRegions } from './lib/settle';

test.use({ viewport: { width: 1602, height: 1100 } });

async function open(page: Page, conversationId: string): Promise<void> {
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await page.goto(`${BASE_URL}/app/chat/${conversationId}`, { waitUntil: 'domcontentloaded' });
  await shellSettled(page);
}

test('@visual chat-live-avatars', async ({ page }) => {
  // @covers /chat/:conversationId
  await serveTeamPresence(page, '990301', { servedName: 'Visual baseline: live avatars' });
  await open(page, '990301');
  await expect(page.locator('[data-testid="participant-state"][data-participant-id="2"]')).toContainText('Running jira_get_issue', { timeout: 20_000 });
  await expect(page.locator('[data-testid="participant-state"][data-participant-id="3"]')).toContainText('Waiting for Mira');
  await settle(page);

  await expect(page).toHaveScreenshot('chat-live-avatars.png', { fullPage: false, mask: volatileRegions(page), ...SNAPSHOT_TOLERANCE });
});

test('@visual chat-local-work-team-readonly', async ({ page }) => {
  // @covers /chat/:conversationId
  await serveTeamPresence(page, '990302', { servedName: 'Visual baseline: Local work team', localWork: true, live: false });
  await open(page, '990302');
  const mira = page.locator('[data-testid="participant-lead"][data-participant-id="2"]');
  await expect(mira.getByTestId('participant-executor')).toHaveAttribute('data-executor', 'cloud', { timeout: 20_000 });
  await expect(mira.getByTestId('participant-lead-badge')).toBeVisible();
  await expect(page.locator('[data-testid="participant-lead"][data-participant-id="3"]').getByTestId('participant-executor')).toHaveAttribute('data-executor', 'local');
  // Read-only: no lead menu on any row.
  await expect(page.getByTestId('participant-lead-menu')).toHaveCount(0);
  await settle(page);

  await expect(page).toHaveScreenshot('chat-local-work-team-readonly.png', { fullPage: false, mask: volatileRegions(page), ...SNAPSHOT_TOLERANCE });
});
