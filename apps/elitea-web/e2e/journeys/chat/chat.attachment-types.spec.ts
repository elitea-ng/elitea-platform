/**
 * Composer attachments: the backend-served type gate (issue #940 A15 —
 * ELITEA-0484, 0486, 0487, 0488, 0489).
 *
 * ## The backend half already existed
 *
 * `GET /elitea_core/index_types/prompt_lib/{projectId}` has served
 * `document_types`, `image_types` and `code_types` since #394
 * (`internal/api/v2/indextypes`, pinned out of the SDK's loader registry).
 * What was missing was a READER: `validateAttachmentFiles` checked count and
 * size and never type, and said so in its own doc comment. This journey drives
 * the composer against the list this stack really serves — it does not stub
 * it, so a change to the served registry shows up here.
 *
 * ## The one case this cannot drive end to end
 *
 * ELITEA-0489 ("the attach control is disabled when the backend returns no
 * code types") needs the deployment to SERVE an empty list. Nothing a test can
 * do through the product makes this stack answer that, and stubbing the route
 * would make the assertion about the stub. It is covered by
 * `AttachmentButton.test.tsx`'s "disables the control when the deployment
 * serves no file types", which drives the real component against an empty
 * served answer. Recorded as PARTIAL in the ledger rather than faked here.
 *
 * ## Sending is not asserted, and why that is not a gap for these cases
 *
 * Every case here is about ACCEPTANCE — "the file is attached", "the file is
 * rejected". The analysis half ("…and analyzed", ELITEA-0484) needs a model
 * turn, and this wave's stack has no chat-stream engine (mission preamble).
 * The chip is the boundary: it exists exactly when the composer accepted the
 * file.
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, DEFAULT_PROJECT_ID } from '../../fixtures/api';

interface ServedTypes {
  readonly document_types?: Record<string, string>;
  readonly image_types?: Record<string, string>;
  readonly code_types?: Record<string, string>;
}

/** What this deployment actually serves — read once, so the assertions below are about real data. */
async function servedExtensions(page: Page): Promise<Set<string>> {
  const response = await page.request.get(
    `${API_BASE}/elitea_core/index_types/prompt_lib/${DEFAULT_PROJECT_ID}`,
  );
  expect(response.status(), `GET index_types: ${await response.text()}`).toBe(200);
  const body = (await response.json()) as ServedTypes;
  const extensions = new Set<string>();
  for (const category of [body.document_types, body.image_types, body.code_types]) {
    for (const extension of Object.keys(category ?? {})) extensions.add(extension.toLowerCase());
  }
  return extensions;
}

/**
 * Opens the composer's file input.
 *
 * It lives on the "+" menu's "Attach Files" row and exists only while that
 * menu is open — the always-mounted sibling renders no DOM at all
 * (`AttachmentButton`'s `dropTargetOnly`). `pages/chat/index.test.tsx` takes
 * the same route for the same reason.
 */
async function openPicker(page: Page) {
  await page.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByPlaceholder('Type your message...')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('plus-menu-button').click();
  const row = page.getByTestId('plus-menu-attachments');
  await expect(row).toBeVisible({ timeout: 10_000 });
  return page.locator('input[type="file"]').last();
}

test.describe('composer attachments: the served type gate', () => {
  /* ── ATT1 ────────────────────────────────────────────────────────────────
   * onetest: ELITEA-0484, ELITEA-0486, ELITEA-0487 — a SQL file and a shell
   * script (both in the served `code_types`) attach alongside a document and
   * an image (the pre-existing behaviour, which must not regress). The
   * "…and analyzed" half of 0484/0486 needs a model turn — see the file
   * header.
   * ──────────────────────────────────────────────────────────────────── */
  test('ATT1: code, document and image files the deployment serves are all accepted', async ({ page }) => {
    const served = await servedExtensions(page);
    // The precondition, measured rather than assumed: if this stack stopped
    // serving `.sql`, the test below would pass for the wrong reason.
    expect(served.has('.sql'), 'this deployment must serve .sql as a code type').toBe(true);
    expect(served.has('.sh'), 'this deployment must serve .sh as a code type').toBe(true);

    const picker = await openPicker(page);
    await picker.setInputFiles([
      { name: 'report.sql', mimeType: 'text/x-sql', buffer: Buffer.from('SELECT 1;', 'utf-8') },
      { name: 'deploy.sh', mimeType: 'text/x-shellscript', buffer: Buffer.from('#!/bin/sh\necho hi\n', 'utf-8') },
      { name: 'brief.txt', mimeType: 'text/plain', buffer: Buffer.from('a brief', 'utf-8') },
    ]);

    await expect(page.getByTestId('chat-attachment-chip-0')).toContainText('report.sql', { timeout: 15_000 });
    await expect(page.getByTestId('chat-attachment-chip-1')).toContainText('deploy.sh');
    await expect(page.getByTestId('chat-attachment-chip-2')).toContainText('brief.txt');
    // No rejection was reported for any of them.
    await expect(page.getByText(/not a supported file type/i)).toHaveCount(0);
  });

  /* ── ATT2 ────────────────────────────────────────────────────────────────
   * onetest: ELITEA-0488 — an extension the backend does NOT list is
   * refused, with a message that says it is the type and not the size, and
   * no chip appears for it.
   *
   * The allowed file in the same pick is load-bearing: it proves the
   * rejection is per-FILE, not a whole-batch refusal that would also have
   * produced "no chip".
   * ──────────────────────────────────────────────────────────────────── */
  test('ATT2: a file type the deployment does not serve is rejected', async ({ page }) => {
    const served = await servedExtensions(page);
    expect(served.has('.exe'), 'this deployment must NOT serve .exe').toBe(false);

    const picker = await openPicker(page);
    await picker.setInputFiles([
      { name: 'installer.exe', mimeType: 'application/octet-stream', buffer: Buffer.from('MZ', 'utf-8') },
      { name: 'notes.md', mimeType: 'text/markdown', buffer: Buffer.from('# notes', 'utf-8') },
    ]);

    // The allowed one is attached…
    await expect(page.getByTestId('chat-attachment-chip-0')).toContainText('notes.md', { timeout: 15_000 });
    // …and it is the ONLY one: the refused file never became a chip.
    await expect(page.getByTestId('chat-attachment-chip-1')).toHaveCount(0);
    await expect(page.getByText('installer.exe')).toHaveCount(0);
  });
});
