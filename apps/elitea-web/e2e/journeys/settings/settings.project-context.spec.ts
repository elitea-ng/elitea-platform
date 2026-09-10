/**
 * Settings: Project Context (onetest package w1-project-context).
 *
 * Ported from the onetest cases in `S/port/pkgs/w1-project-context.md`. Only
 * the SETTINGS/CONFIG half of that package is here: character-limit
 * enforcement, upload validation, and the enable/disable toggle's IN-SESSION
 * behaviour. Every case whose acceptance requires an actual model turn
 * (asking an agent/pipeline/chat a question and reading the answer) is out
 * of scope for this chromium-lane file — see `S/port/ledger-P3-settings.tsv`
 * (STREAM-DEFERRED) for the full list and what the mock model would need.
 *
 * ── THE HEADLINE FINDING: Project Context can never actually be saved ──────
 *
 * `UpdateProjectContext` (`services/elitea-main/internal/api/v2/eliteacore/
 * handler.go:623-651`) always answers 200 with the request body echoed back,
 * regardless of whether anything was written. Its `INSERT … ON CONFLICT
 * (elitea_title) WHERE type = 'project_context' DO UPDATE …` names a PARTIAL
 * unique index that does not exist — the migration
 * (`internal/infra/db/migrations/001_initial.sql:823`) gives `elitea_title`
 * only a plain, non-partial `UNIQUE` constraint, which does not satisfy a
 * `WHERE`-qualified ON CONFLICT inference. Postgres therefore refuses that
 * INSERT on every call (a static/plan-time error, not a real conflict), the
 * handler falls back to `UPDATE … WHERE type = 'project_context'` (which
 * touches zero rows on a project that has never had one), swallows BOTH
 * errors, and writes the 200 anyway. Verified directly against this stack
 * with `PUT` then an immediate `GET` on both project 1 and a second project —
 * neither ever reflects a save, ever. See `S/port/defects.md` for the full
 * writeup and `PJC-PERSIST` below for the pinned regression.
 *
 * Consequently, most onetest cases whose entire point is "Save, then reload,
 * and the content is still there" reduce to the SAME one defect and are not
 * repeated as separate tests — they are folded into `PJC-PERSIST`'s tag list.
 * What remains PORTED here is everything genuinely independent of that bug:
 * in-session editor behaviour (typing, uploading, truncating, previewing)
 * that the FRONTEND gets right even though the backend never durably stores
 * any of it.
 *
 * ── Two projects, not "Private" and "Team" ─────────────────────────────────
 *
 * The onetest cases assume a Private-project/Team-project split. This stack's
 * two journey personas (`member`, `admin`) hold no personal project of their
 * own — `auth.setup.ts` documents that `resolvePersonalProjectID`'s third
 * branch answers project 1 for both, because neither has a
 * `project_user_<uid>` row. So there is no genuinely-private project to test
 * against here. The one case that still needs a SECOND project
 * (ELITEA-0957's "page loads in more than one project") uses project 1
 * (`DEFAULT_PROJECT_ID`) and the seeded `e2e-publish-author` project (both
 * personas hold the admin role there too, per `PUBLISH_AUTHOR_PROJECT_NAME`'s
 * doc comment).
 */
import { test, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, PUBLISH_AUTHOR_PROJECT_NAME, resolvePublishAuthorProjectId } from '../../fixtures/api';
import { ensureProjectSelected } from '../../fixtures/project';

const PROJECT_PARAMS_PAGE = `${BASE_URL}/app/settings/project-params`;
const MAX_CHARS = 2500;

/** GET the raw `{content, enabled}` blob for a project, bypassing the SPA — the one place this file asks what the SERVER actually stored. */
async function readProjectContext(
  page: Page,
  projectId: string,
): Promise<{ content: string; enabled: boolean }> {
  const resp = await page.request.get(
    `${API_BASE}/elitea_core/project_context/prompt_lib/${projectId}/project-context`,
  );
  expect(resp.status(), `GET project context for ${projectId}`).toBe(200);
  const body = (await resp.json()) as { data?: { content?: string; enabled?: boolean } };
  return { content: body.data?.content ?? '', enabled: body.data?.enabled ?? true };
}

/**
 * Lands on the Project Context tab of the CURRENTLY selected project and
 * leaves the EDITOR (not just the page) open.
 *
 * Two gates stand between "the page is open" and "the editor is on screen":
 *
 *  - the EMPTY STATE (`ProjectContextEmptyState`), shown whenever the
 *    server has no saved (non-blank) content and `isEditing` is still
 *    false — clicking "Create" flips `isEditing` and bypasses it.
 *  - `deriveShowFlags`'s `showEditorContent = enabled || content.trim() ||
 *    !canEdit`. This stack's projects seed `enabled: false` with empty
 *    content, which satisfies none of those — `EditorSection` does not
 *    mount, only the (always-rendered) toggle card does. Flipping that
 *    switch on is what makes the editor appear; only the LOCAL, optimistic
 *    half of that flip is checked here (see the file header — the switch's
 *    own save is exactly as durable as every other save on this page, i.e.
 *    not durable at all, so waiting on the SERVER to confirm it would hang).
 */
async function openEditor(page: Page): Promise<void> {
  await page.goto(PROJECT_PARAMS_PAGE, { waitUntil: 'domcontentloaded' });
  const body = page.getByTestId('project-context-body');
  const empty = page.getByTestId('project-context-empty-state');
  await expect(body.or(empty)).toBeVisible({ timeout: 20_000 });
  if (await empty.isVisible().catch(() => false)) {
    await page.getByTestId('project-context-create-button').click();
    await expect(body).toBeVisible({ timeout: 10_000 });
  }

  const toggle = page.getByRole('switch');
  const alreadyOpen = await editorContent(page).isVisible().catch(() => false);
  if (!alreadyOpen && (await toggle.isVisible().catch(() => false))) {
    await toggle.click();
    await expect(editorContent(page)).toBeVisible({ timeout: 10_000 });
  }
}

/** The CodeMirror content element — see `pipelines.validation.spec.ts` for why `insertText` and not `.fill()`. */
function editorContent(page: Page) {
  return page.getByTestId('project-context-body').locator('.cm-content');
}

async function typeContent(page: Page, text: string): Promise<void> {
  const editor = editorContent(page);
  await editor.click();
  await page.keyboard.press('ControlOrMeta+a');
  await page.keyboard.press('Delete');
  await page.keyboard.insertText(text);
}

/**
 * Clicks Save and waits for the FRONTEND to consider it settled
 * (`isDirty` reset, which only happens on `handleSave`'s success path).
 *
 * Deliberately does NOT verify the server's own row — seepage file header.
 * This is the helper every test EXCEPT `PJC-PERSIST` uses, because every
 * other test's claim is about in-session/client behaviour, not about
 * whether the write survives a reload (it never does, currently).
 */
async function clickSave(page: Page): Promise<void> {
  const btn = page.getByTestId('project-context-save-button');
  await btn.click();
  await expect(btn).toBeDisabled({ timeout: 10_000 });
}

test.describe('project-context: settings/config coverage', () => {
  /* ── PJC-PERSIST ────────────────────────────────────────────────────
   * onetest: ELITEA-0942, ELITEA-0949, ELITEA-0950, ELITEA-0953,
   *          ELITEA-0956, ELITEA-0957 — the persistence half of each of
   *          these cases. PRODUCT GAP, see the file header for the exact
   *          backend defect (a malformed `ON CONFLICT … WHERE` clause whose
   *          errors are swallowed, so every save is silently a no-op that
   *          still answers 200).
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC-PERSIST: a saved Project Context does not survive a reload — product gap', async ({ page }) => {
    test.fail(
      true,
      'ELITEA-0949 (and 0942/0950/0953/0956/0957\'s persistence half): product gap — UpdateProjectContext ' +
        'always answers 200 but never durably writes (handler.go:623-651, migrations/001_initial.sql:823)',
    );
    await openEditor(page);
    const text = `${AUTOTEST_PREFIX}persist-probe-${Date.now()}`;
    await typeContent(page, text);
    await clickSave(page);

    // SHOULD hold: the server's own row reflects what was just saved. It
    // never does — this poll times out holding the pre-save value.
    await expect
      .poll(async () => (await readProjectContext(page, DEFAULT_PROJECT_ID)).content, { timeout: 8_000 })
      .toBe(text);
  });

  /* ── PJC01 ────────────────────────────────────────────────────────────
   * onetest: ELITEA-0957 — the page loads without error in a SECOND
   * project (not just the default one), for an Admin. The "and the content
   * persists" half of this case is answered by PJC-PERSIST instead — see
   * the file header.
   * ──────────────────────────────────────────────────────────────────── */
  test.describe('as admin, in the second project', () => {
    test.use({ storageState: STORAGE_STATE.admin });

    test('PJC01: Project Context is reachable and editable in a second project as Admin', async ({ page, request }) => {
      await resolvePublishAuthorProjectId(request); // asserts the project resolves; id unused beyond that.
      // `ensureProjectSelected` reads the sidebar's switcher, which exists
      // only once the app shell has mounted — a fresh test starts on
      // `about:blank`, so the shell needs a navigation first (`auth.setup.ts`
      // does the same before its own call).
      await page.goto(BASE_URL + '/app/', { waitUntil: 'domcontentloaded' });
      await ensureProjectSelected(page, PUBLISH_AUTHOR_PROJECT_NAME);
      await openEditor(page);

      const text = `${AUTOTEST_PREFIX}QA admin context ${Date.now()}`;
      await typeContent(page, text);
      await expect(editorContent(page)).toContainText(text);
      await clickSave(page);
      // A SUCCESS toast is expected here (and is what `clickSave` already
      // confirmed via the button re-disabling); only an ERROR toast would
      // indicate the save was refused.
      await expect(page.getByRole('alert').filter({ hasText: /fail/i })).toHaveCount(0);

      await checkA11y(page);
    });
  });

  /* ── PJC02 ────────────────────────────────────────────────────────────
   * onetest: ELITEA-0942 — the in-session half: content survives edit →
   * replace → clear WITHIN one page load, and a Unicode/emoji upload
   * round-trips into both Edit and Preview mode without corruption. The
   * "survives a RELOAD" half is PJC-PERSIST's.
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC02: content survives edit/replace/clear in-session, and a Unicode upload renders correctly', async ({ page }) => {
    await openEditor(page);

    // Each edit below is settled (a real wait, past CodeMirrorEditor's own
    // `CHANGE_DEBOUNCE_MS`, not just a DOM-text check) before the next one
    // starts. Chaining edits without settling was measured to let a STALE
    // debounced `onChange` — from an EARLIER edit — land after a LATER one,
    // clobbering it back to the earlier value; this is what makes the
    // Preview check below meaningful rather than a coin flip.
    await typeContent(page, `${AUTOTEST_PREFIX}Version A content`);
    await expect(editorContent(page)).toContainText('Version A content');
    await page.waitForTimeout(400);

    await typeContent(page, `${AUTOTEST_PREFIX}Version B content`);
    await expect(editorContent(page)).toContainText('Version B content');
    await expect(editorContent(page)).not.toContainText('Version A content');
    await page.waitForTimeout(400);

    const editor = editorContent(page);
    await editor.click();
    await page.keyboard.press('ControlOrMeta+a');
    await page.keyboard.press('Delete');
    await expect(editor).toHaveText('');
    await page.waitForTimeout(400);

    // Unicode/emoji, via the .md upload path (issue 841's own regression case).
    const unicode = `# 🌍 Unicode Test\n\n中文: 人工智能\nRussian: Привет\nArabic: مرحبا\nEmoji: ✅🔴🟠`;
    await page.getByRole('button', { name: 'Import markdown file' }).click();
    await page.locator('input[type="file"]').setInputFiles({
      name: 'unicode.md',
      mimeType: 'text/markdown',
      buffer: Buffer.from(unicode, 'utf-8'),
    });
    await expect(editorContent(page)).toContainText('人工智能', { timeout: 10_000 });
    await expect(editorContent(page)).toContainText('✅🔴🟠');

    // Preview mode must render the same text without corruption.
    await page.getByRole('tab', { name: 'Preview mode' }).click();
    await expect(page.getByTestId('project-context-body')).toContainText('人工智能', { timeout: 5_000 });
    await expect(page.getByTestId('project-context-body')).toContainText('✅🔴🟠');
    await page.getByRole('tab', { name: 'Edit mode' }).click();
  });

  /* ── PJC03a ───────────────────────────────────────────────────────────
   * onetest: ELITEA-0940 — .md upload: valid content loads into the
   * editor; an UNSAVED upload is lost on reload (the client does not
   * silently persist a draft); a 0-byte .md file is accepted and clears
   * the editor with no error. The "and Save makes it survive a reload"
   * half is PJC-PERSIST's.
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC03a: a valid .md upload loads into the editor, and is lost on reload if not saved', async ({ page }) => {
    await openEditor(page);
    await typeContent(page, `${AUTOTEST_PREFIX}baseline-before-upload`);
    await clickSave(page);

    const uploaded = '# My Project\n\n**Stack:** Node.js, React\n\n**Rules:**\n- Respond concisely';
    await page.getByRole('button', { name: 'Import markdown file' }).click();
    await page.locator('input[type="file"]').setInputFiles({
      name: 'valid.md',
      mimeType: 'text/markdown',
      buffer: Buffer.from(uploaded, 'utf-8'),
    });
    await expect(editorContent(page)).toContainText('Respond concisely', { timeout: 10_000 });

    // Reload WITHOUT saving the upload — it must be lost. (The saved
    // baseline is ALSO lost, per PJC-PERSIST — the empty-state screen this
    // reload lands on either way is consistent with that, not a second bug.)
    await page.reload({ waitUntil: 'domcontentloaded' });
    const empty = page.getByTestId('project-context-empty-state');
    const body = page.getByTestId('project-context-body');
    await expect(body.or(empty)).toBeVisible({ timeout: 20_000 });
    if (await body.isVisible().catch(() => false)) {
      await expect(editorContent(page)).not.toContainText('Respond concisely');
    }

    // A 0-byte .md file is accepted and clears the editor, with no error toast.
    await openEditor(page);
    await typeContent(page, 'placeholder before clearing');
    await expect(editorContent(page)).toContainText('placeholder before clearing');
    // A REAL wait, not just a DOM-text check — see PJC02's note on a stale
    // debounced `onChange` clobbering a later, faster change.
    await page.waitForTimeout(400);
    await page.getByRole('button', { name: 'Import markdown file' }).click();
    await page.locator('input[type="file"]').setInputFiles({
      name: 'empty.md',
      mimeType: 'text/markdown',
      buffer: Buffer.from('', 'utf-8'),
    });
    await expect(editorContent(page)).toHaveText('', { timeout: 10_000 });
    await expect(page.getByRole('alert').filter({ hasText: /fail/i })).toHaveCount(0);
  });

  /* ── PJC03b ───────────────────────────────────────────────────────────
   * onetest: ELITEA-0940 — a non-.md file must be rejected (filtered from
   * the picker, or an explicit error). PRODUCT GAP: `handleFileUpload`
   * (`src/pages/settings/ProjectContext.tsx`) reads whatever `File` object
   * the input carries and checks only its LENGTH — never `file.name` or
   * `file.type` — so a `.txt` file with the same valid content is silently
   * ACCEPTED. The native file-picker's `accept=".md,text/markdown"` hint is
   * the only filter, and Playwright's `setInputFiles` (like a changed OS
   * filter) bypasses it, which is exactly the gap: nothing enforces the
   * restriction once a non-`.md` file reaches the input. Independent of the
   * persistence defect above — this is a client-side validation gap.
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC03b: a .txt file with the same content is rejected — product gap', async ({ page }) => {
    test.fail(true, 'ELITEA-0940: product gap — handleFileUpload never checks file name/type, only length');
    await openEditor(page);
    await typeContent(page, `${AUTOTEST_PREFIX}baseline-before-wrong-type`);

    await page.getByRole('button', { name: 'Import markdown file' }).click();
    await page.locator('input[type="file"]').setInputFiles({
      name: 'not-markdown.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from('# My Project\n\nSame content, wrong extension.', 'utf-8'),
    });
    // `FileReader.readAsText` is asynchronous, and `handleFileUpload` clears
    // `e.target.value` synchronously right after calling it — so checking
    // "not contain" too early passes VACUOUSLY (the read simply hasn't
    // resolved yet), which is exactly the false-negative this rulebook warns
    // against. Waiting for the read to genuinely settle first (confirmed via
    // manual reproduction: ~1s) is what makes the absence check meaningful.
    await page.waitForTimeout(1_500);
    // SHOULD hold: the .txt content must not reach the editor. It does.
    await expect(editorContent(page)).not.toContainText('Same content, wrong extension');
  });

  /* ── PJC04 ────────────────────────────────────────────────────────────
   * onetest: ELITEA-0947 — importing a .md file over the 2500-character
   * limit is rejected with an explicit error, and existing editor content
   * is left unchanged (no silent truncation). Entirely in-session — no
   * reload/persistence needed for this case's own claim.
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC04: a .md import over 2500 characters is rejected without clearing existing content', async ({ page }) => {
    await openEditor(page);
    const original = `${AUTOTEST_PREFIX}short project overview, well under the limit`;
    await typeContent(page, original);
    await clickSave(page);

    const tooLong = 'x'.repeat(MAX_CHARS + 1);
    await page.getByRole('button', { name: 'Import markdown file' }).click();
    await page.locator('input[type="file"]').setInputFiles({
      name: 'too-long.md',
      mimeType: 'text/markdown',
      buffer: Buffer.from(tooLong, 'utf-8'),
    });
    await expect(page.getByRole('alert').filter({ hasText: /2500 character/i })).toBeVisible({ timeout: 10_000 });
    await expect(editorContent(page)).toContainText(original, { timeout: 3_000 });

    // A much-longer file is refused the same way — no silent truncation.
    const wayTooLong = 'y'.repeat(MAX_CHARS * 3);
    await page.getByRole('button', { name: 'Import markdown file' }).click();
    await page.locator('input[type="file"]').setInputFiles({
      name: 'way-too-long.md',
      mimeType: 'text/markdown',
      buffer: Buffer.from(wayTooLong, 'utf-8'),
    });
    await expect(page.getByRole('alert').filter({ hasText: /2500 character/i })).toBeVisible({ timeout: 10_000 });
    await expect(editorContent(page)).toContainText(original, { timeout: 3_000 });
    await expect(editorContent(page)).not.toContainText('yyyy');
  });

  /* ── PJC05 ────────────────────────────────────────────────────────────
   * onetest: ELITEA-0950 — the editor enforces the 2500-character limit by
   * truncating input at the boundary, for both typed and pasted text. The
   * "and the SAVED content never exceeds it" half is PJC-PERSIST's (though
   * note it would pass VACUOUSLY under the persistence bug — an empty
   * saved row is trivially "at most 2500 characters" — which is exactly
   * why that half is not repeated here as if it were a real check).
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC05: typing and pasting are truncated at 2500 characters', async ({ page }) => {
    await openEditor(page);
    const editor = editorContent(page);
    await editor.click();
    await page.keyboard.press('ControlOrMeta+a');
    await page.keyboard.press('Delete');

    // Type past the limit in one shot — CodeMirror's `maxLength` extension
    // (see `shared/ui/CodeMirrorEditor`, used identically by the agent
    // welcome-message counter in `agents.editor.spec.ts`'s J14c) refuses
    // input past the cap.
    await page.keyboard.insertText('a'.repeat(MAX_CHARS + 500));
    const counter = page.getByTestId('project-context-char-counter');
    await expect(counter).toContainText('0', { timeout: 5_000 });
    await expect(counter).toContainText(/maximum character limit/i);
    const lengthAfterType = await editor.evaluate((el) => (el.textContent ?? '').length);
    expect(lengthAfterType).toBeLessThanOrEqual(MAX_CHARS);

    // Paste (insertText again, simulating a paste of a large block) — same cap.
    await page.keyboard.press('ControlOrMeta+a');
    await page.keyboard.press('Delete');
    await page.keyboard.insertText('b'.repeat(MAX_CHARS + 1000));
    const lengthAfterPaste = await editor.evaluate((el) => (el.textContent ?? '').length);
    expect(lengthAfterPaste).toBeLessThanOrEqual(MAX_CHARS);
  });

  /* ── PJC06 ────────────────────────────────────────────────────────────
   * onetest: ELITEA-0953 — the Enable toggle's IN-SESSION effect: flipping
   * it changes `showDisabledBanner`/the editor's visibility immediately,
   * without a page reload. The persistence half (does it stay flipped
   * after reload, is it independent per project) is PJC-PERSIST's — the
   * toggle saves through the exact same broken endpoint as the editor.
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC06: the Enable toggle changes the page immediately, in both directions', async ({ page }) => {
    await openEditor(page);
    await typeContent(page, `${AUTOTEST_PREFIX}toggle-probe`);
    const toggle = page.getByRole('switch');
    await expect(toggle).toBeChecked();
    await expect(editorContent(page)).toBeVisible();

    await toggle.click();
    await expect(toggle).not.toBeChecked();
    // Content is still non-blank, so `showEditorContent` stays true
    // (`enabled || content.trim() || !canEdit`) and the editor remains —
    // but the "context is off" banner must now appear.
    await expect(page.getByText(/Project Context is turned off/i)).toBeVisible({ timeout: 5_000 });

    await toggle.click();
    await expect(toggle).toBeChecked();
    await expect(page.getByText(/Project Context is turned off/i)).toHaveCount(0);
  });

  /* ── PJC07 ────────────────────────────────────────────────────────────
   * onetest: ELITEA-0955 — Edit ↔ Preview switches without a full page
   * reload, and content is not lost across repeated switches. Entirely
   * in-session — unaffected by the persistence defect.
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC07: Editor and Preview modes switch without reload and without losing content', async ({ page }) => {
    await openEditor(page);
    await typeContent(page, `${AUTOTEST_PREFIX}# Heading\n\n**Bold text**`);
    // The Preview pane reads REACT state (`content`), which CodeMirror's
    // `onChange` reaches only after its own debounce — the CM DOCUMENT
    // updates instantly, but switching modes before the debounce fires
    // would preview the PRIOR (empty) state. Waiting for the editor's own
    // text confirms CodeMirror settled; `pipelines.validation.spec.ts`
    // additionally sleeps past `CHANGE_DEBOUNCE_MS` for the same reason.
    await expect(editorContent(page)).toContainText('Bold text');
    await page.waitForTimeout(300);

    const navigations: string[] = [];
    page.on('framenavigated', (frame) => {
      if (frame === page.mainFrame()) navigations.push(frame.url());
    });

    await page.getByRole('tab', { name: 'Preview mode' }).click();
    await expect(page.getByTestId('project-context-body')).toContainText('Bold text', { timeout: 5_000 });
    // Rendered Markdown, not raw syntax.
    await expect(page.getByTestId('project-context-body').locator('strong', { hasText: 'Bold text' })).toBeVisible();

    await page.getByRole('tab', { name: 'Edit mode' }).click();
    await expect(editorContent(page)).toContainText('**Bold text**', { timeout: 5_000 });

    // Several times in a row — no crash, no blank screen.
    for (let i = 0; i < 3; i++) {
      await page.getByRole('tab', { name: 'Preview mode' }).click();
      await page.getByRole('tab', { name: 'Edit mode' }).click();
    }
    await expect(editorContent(page)).toContainText('Bold text', { timeout: 5_000 });
    expect(navigations, 'switching mode must not navigate the page').toHaveLength(0);

    await checkA11y(page);
  });
});
