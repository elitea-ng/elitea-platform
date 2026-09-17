/**
 * elitea_issues package I1-agents — #6627.
 *
 * The filed report ("[BUG] Agent header icon does not update after
 * selecting a new icon — only appears after a page reload") was written
 * against the OLD EliteaUI (its own file citations: `src/api/applications.js`,
 * `EntityIcon.jsx`, `SelectIconDialog.jsx` — none of which exist in this
 * app). Re-judged: DEFECT-CHECK — reproduced not as "the update silently
 * fails", but as the underlying feature being ABSENT altogether
 * (`ApplicationEditForm.tsx`'s and `CreateAgentForm.tsx`'s own doc comments
 * disclosed it: "a future unit that needs an EDITABLE entity icon should
 * build that mode fresh"). Fixed at the root
 * (`src/features/agents/ui/AgentIconEditor.tsx` +
 * `src/features/agents/api/applicationIconApi.ts`, wired into
 * `src/pages/agents/ui/EditApplicationConfigurationPanel.tsx`'s `iconSlot`).
 * This is the regression journey for that fix.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createAgent, deleteAgent } from '../../fixtures/api';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}icon-${stem}-${String(Date.now()).slice(-7)}`;
}

// A genuine minimal 1x1 transparent PNG — `IconPickerDialog`'s own upload
// path decodes the file through a real `Image()` in the browser to measure
// its (capped) pixel dimensions before calling back, so arbitrary bytes
// would hang the picker rather than fail loudly.
const ONE_PIXEL_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=',
  'base64',
);

/* onetest: elitea_issues #6627 — uploading an icon updates the header immediately, no reload, and survives one */
test('J-icon: uploading an agent icon updates the header immediately and survives a reload', async ({
  page,
  request,
}) => {
  const name = uniqueName('agent');
  const agent = await createAgent(request, name);
  try {
    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });

    const iconButton = page.getByTestId('agent-icon-edit-button');
    await expect(iconButton).toBeVisible({ timeout: 10_000 });
    // Before: no per-agent icon has ever been set — the fallback glyph, no <img>.
    await expect(iconButton.locator('img')).toHaveCount(0);

    await iconButton.click();
    await expect(page.getByRole('dialog')).toBeVisible({ timeout: 10_000 });

    const uploadResponse = page.waitForResponse(
      (response) => response.url().includes('/upload_icon/prompt_lib/') && response.request().method() === 'POST',
    );
    const bindResponse = page.waitForResponse(
      (response) => response.url().includes('/upload_icon/prompt_lib/') && response.request().method() === 'PUT',
    );
    await page.setInputFiles('[data-testid="icon-picker-file-input"]', {
      name: 'agent-icon.png',
      mimeType: 'image/png',
      buffer: ONE_PIXEL_PNG,
    });
    const uploaded = await uploadResponse;
    expect(uploaded.ok(), await uploaded.text()).toBe(true);
    const bound = await bindResponse;
    expect(bound.ok(), await bound.text()).toBe(true);

    // Immediately, with the dialog still open (the upload path does not
    // auto-close it) and NO page reload: the trigger button's own icon
    // already shows the newly uploaded image — the exact "immediately, no
    // reload" assertion #6627 was filed over.
    const uploadedUrl = ((await uploaded.json()) as { readonly icon_meta?: { readonly url?: string } }).icon_meta?.url;
    expect(uploadedUrl, 'the upload must answer a real icon_meta.url to assert against').toBeTruthy();
    await expect(iconButton.locator('img')).toHaveAttribute('src', uploadedUrl ?? '', { timeout: 10_000 });

    await page.keyboard.press('Escape');
    await page.reload();
    await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId('agent-icon-edit-button').locator('img')).toHaveAttribute(
      'src',
      uploadedUrl ?? '',
      { timeout: 10_000 },
    );
  } finally {
    await deleteAgent(request, agent.id);
  }
});
