/**
 * Voice settings say when no speech model is configured.
 *
 * This stack's member project has a chat model and no text-to-speech model
 * (same default state `chat.voice-input.spec.ts` states for transcription).
 * Read-aloud then uses the browser voice, and the user who expected the
 * project's model used to get a different voice — or, where the browser has
 * none, silence — with no reason given. The voice settings dialog now names
 * the cause above its controls.
 *
 * Nothing is created, so nothing is cleaned up.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

test('the voice settings dialog says that no speech model is configured', async ({ page }) => {
  await page.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const settings = page.getByTestId('chat-voice-settings-button');
  await expect(settings).toBeVisible({ timeout: 20_000 });
  await settings.click();

  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible();
  await expect(dialog.getByTestId('voice-no-model-notice')).toHaveText('No speech model is configured for this project.');

  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toHaveCount(0);
});
