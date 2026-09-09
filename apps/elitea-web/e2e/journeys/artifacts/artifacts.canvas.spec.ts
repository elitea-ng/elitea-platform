/**
 * Journey: "Open in canvas" from a stored artifact file, and "Save to
 * artifacts" from the canvas header (issue #878).
 *
 * The canvas editor could edit a code/diagram/table block carved out of the
 * CURRENT chat turn and could not open a previously-stored file, nor publish
 * its own document back into the artifact store as a named, browsable
 * object. This journey drives the other half: upload a text file to a real
 * bucket, open it through the artifacts browser's own preview pane in the
 * richer canvas editor (not the plain text-area `FilePreviewCanvas` offers),
 * edit it, save it back through the SAME "Save to artifacts" route the
 * artifacts browser's own upload uses, and read the STORE — not the screen —
 * to confirm the edit actually landed.
 */
import { test, expect, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

function uniqueBucket(): string {
  return `autotest-cvs-${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}`;
}

const FILE_NAME = 'notes.md';
const ORIGINAL_BODY = '# Notes\n\nAUTOTEST original canvas content';

/** The project the app itself selected, read from the store the app writes. */
async function selectedProjectId(page: Page): Promise<string> {
  const id = await page.evaluate(() => localStorage.getItem('el.project.id'));
  expect(id, 'app-shell must have persisted a selected project').not.toBeNull();
  return id as string;
}

/** Idempotent backend fixture: a fresh bucket with one text object. */
async function seedBucketWithFile(request: APIRequestContext, projectId: string, bucket: string): Promise<void> {
  const created = await request.post(`/api/v2/artifacts/buckets/${projectId}`, { data: { name: bucket } });
  expect([200, 201, 409]).toContain(created.status());
  const uploaded = await request.post(`/api/v2/artifacts/objects/${projectId}/${bucket}?overwrite=true`, {
    multipart: { file: { name: FILE_NAME, mimeType: 'text/markdown', buffer: Buffer.from(ORIGINAL_BODY) } },
  });
  expect(uploaded.status(), await uploaded.text()).toBe(201);
}

test('a stored file opens in the canvas editor, and saving it writes the edit back to the SAME object', async ({ page, request }) => {
  test.setTimeout(180_000);
  const bucket = uniqueBucket();

  await page.goto(BASE_URL + '/app/artifacts');
  await page.waitForURL('**/artifacts**', { timeout: 15_000 });
  const projectId = await selectedProjectId(page);
  await seedBucketWithFile(request, projectId, bucket);

  try {
    await page.goto(`${BASE_URL}/app/artifacts?bucket=${bucket}&file=${FILE_NAME}`);
    await expect(page.getByText(ORIGINAL_BODY.split('\n')[0] ?? '')).toBeVisible({ timeout: 20_000 });

    // "Open in canvas" — the plain text preview's own header control
    // (`FilePreviewCanvas`'s new icon button, issue #878).
    await page.getByRole('button', { name: 'Open in canvas' }).click();

    const editorRoot = page.getByTestId('canvas-editor-root');
    await expect(editorRoot).toBeVisible({ timeout: 20_000 });
    const content = editorRoot.locator('.cm-content');
    await expect(content).toContainText('AUTOTEST original canvas content', { timeout: 20_000 });

    // Edit the document — append a token whose presence in the STORE, not the
    // screen, is the claim this journey makes.
    const editToken = `AUTOTEST-EDITED-${Date.now().toString(36)}`;
    await content.click();
    await page.keyboard.press('End');
    await page.keyboard.type(`\n\n${editToken}`);
    await expect(content).toContainText(editToken, { timeout: 10_000 });

    // "Save to artifacts" — pre-filled from the SOURCE this canvas was opened
    // from, so this is an ordinary re-save (no overwrite confirm).
    await page.getByTestId('canvas-edit-save-to-artifacts').click();
    const dialog = page.getByTestId('canvas-save-to-artifacts-dialog');
    await expect(dialog).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId('canvas-save-filename-input')).toHaveValue(FILE_NAME);

    const uploadResponse = page.waitForResponse(
      (r) => r.url().includes(`/api/v2/artifacts/objects/${projectId}/${bucket}`) && r.request().method() === 'POST',
      { timeout: 20_000 },
    );
    await page.getByTestId('canvas-save-submit').click();
    const uploaded = await uploadResponse;
    expect(uploaded.status(), await uploaded.text()).toBe(201);
    await expect(dialog).toBeHidden({ timeout: 10_000 });

    // THE STORE. The download route is the same one the plain artifacts
    // preview and the attachment download both already use — reading it back
    // here is what tells "the edit is really saved" apart from "a dialog
    // closed".
    const downloaded = await request.get(`/api/v2/artifacts/objects/${projectId}/${bucket}/${FILE_NAME}`);
    expect(downloaded.status()).toBe(200);
    const storedBody = await downloaded.text();
    expect(storedBody).toContain('AUTOTEST original canvas content');
    expect(storedBody).toContain(editToken);

    // Closing the canvas returns to the plain preview of the SAME (now
    // updated) file, not a blank state.
    await page.getByTestId('canvas-edit-close').click();
    await expect(editorRoot).toBeHidden({ timeout: 10_000 });
    await expect(page.getByText(editToken)).toBeVisible({ timeout: 15_000 });
  } finally {
    await request.delete(`/api/v2/artifacts/buckets/${projectId}/${bucket}`);
  }
});
