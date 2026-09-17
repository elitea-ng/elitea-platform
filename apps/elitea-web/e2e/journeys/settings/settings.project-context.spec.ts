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
 * ── THE HEADLINE FINDING, AND ITS FIX (#888) ───────────────────────────────
 *
 * This file was first written against a backend that could not save Project
 * Context AT ALL. `UpdateProjectContext` answered 200 with the request body
 * echoed back whatever happened: its INSERT omitted `project_id` (which the
 * tenant `configuration` table declares NOT NULL, so every project without a
 * prior row — every project on a seeded stack — failed with SQLSTATE 23502),
 * its `ON CONFLICT (elitea_title) WHERE type = 'project_context'` named a
 * partial unique index that `001_initial.sql` never creates, and BOTH errors
 * were discarded. Every "save, reload, it is still there" case therefore
 * reduced to that one defect and was folded into `PJC-PERSIST` as a single
 * fail-marked regression.
 *
 * The handler now writes UPDATE-first, names `project_id`, and returns a
 * typed 500 (`project_context_write_failed`) instead of swallowing the error
 * — see `project_context_postgres_integration_test.go` for the Postgres-level
 * acceptance. The fail marks below are gone and the persistence assertions
 * they stood in for are real again.
 *
 * ── WHAT THAT FIX COST THIS FILE, AND WHY EVERY TEST NOW OWNS A PROJECT ────
 *
 * Project Context is ONE ROW PER PROJECT. While the save no-opped, "the
 * project starts empty and stays empty" held for every test at any
 * concurrency, and every persona here could share project 1. A working save
 * makes that false: under `fullyParallel: true`, `PJC-PERSIST`'s save,
 * `PJC03a`'s baseline, `PJC06`'s toggle reading and `PJC08`'s empty state —
 * plus `settings.p13-project-context.spec.ts`, a different FILE — all read and
 * write the same row. Interleaved, that is a clobber, not a flake a retry
 * fixes.
 *
 * A cross-worker mutex over that row was tried first, twice, and did not hold:
 * this file went on failing a DIFFERENT test on each CI run (chromium PJC01
 * and PJC06, webkit PJC04, and PJC03b/PJC02/PJC08 before them). A lock
 * serialises the WRITES; it does not stop the page's own React Query cache and
 * one-shot mount fetch from having read the row a moment before the lock
 * changed hands, and it leaves each test depending on the previous holder
 * having reset the row correctly. The failure looks exactly like the feature
 * being broken, which is why each round of "settle the debounce, wait
 * positively" bought a run or two and then moved the failure elsewhere.
 *
 * So no test here shares a row with anything any more. `beforeEach` PROVISIONS
 * A PROJECT (`fixtures/scratchProject.ts` — the real
 * `POST /projects/project/administration` pipeline, ~250 ms), the page is
 * switched to it through the product's own switcher, the assertions run there,
 * and `afterEach` deletes it. Project 1 is not read or written by this file at
 * all, the mutex is gone, and so are the "reset the shared row first" steps
 * that only existed to make a shared row survivable.
 *
 * ── Two projects, not "Private" and "Team" ─────────────────────────────────
 *
 * The onetest cases assume a Private-project/Team-project split. This stack's
 * journey personas hold no personal project that is private in that sense, so
 * there is no genuinely-private project to test against here. The one case
 * that needs a SECOND project (ELITEA-0957's "the page loads in more than one
 * project", plus its per-project independence half) provisions a second
 * scratch project of its own — two projects that exist only for that test,
 * which is a stronger statement than the old pairing of project 1 with a
 * seeded shared project could make.
 */
import { test, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import {
  readProjectContext,
  resetProjectContext,
  seedProjectContextAsAdmin,
} from '../../fixtures/projectContext';
import { createScratchProject, deleteScratchProject, type ScratchProject } from '../../fixtures/scratchProject';
import { ensureProjectSelected } from '../../fixtures/project';

const PROJECT_PARAMS_PAGE = `${BASE_URL}/app/settings/project-params`;
const MAX_CHARS = 2500;

/**
 * Puts the browser in `project` and lands on its Project Context tab.
 *
 * `ensureProjectSelected` reads the sidebar's switcher, which exists only once
 * the app shell has mounted — a fresh test starts on `about:blank`, so the
 * shell needs a navigation first (`auth.setup.ts` does the same before its own
 * call). The scratch project is not the one the persona's storage state pins,
 * so this always performs a real switch on the first call and costs one
 * `textContent()` read on any later one.
 */
async function gotoProjectParams(page: Page, project: ScratchProject): Promise<void> {
  await page.goto(`${BASE_URL}/app/`, { waitUntil: 'domcontentloaded' });
  await ensureProjectSelected(page, project.name);
  await page.goto(PROJECT_PARAMS_PAGE, { waitUntil: 'domcontentloaded' });
}

/**
 * Lands on the Project Context tab of `project` and leaves the EDITOR (not
 * just the page) open.
 *
 * Two gates stand between "the page is open" and "the editor is on screen":
 *
 *  - the EMPTY STATE (`ProjectContextEmptyState`), shown whenever the
 *    server has no saved (non-blank) content and `isEditing` is still
 *    false — clicking "Create" flips `isEditing` and bypasses it.
 *  - `deriveShowFlags`'s `showEditorContent = enabled || content.trim() ||
 *    !canEdit`. A freshly provisioned project has no saved row at all, which
 *    satisfies none of those — `EditorSection` does not mount, only the
 *    (always-rendered) toggle card does. Flipping that switch on is what makes
 *    the editor appear; only the LOCAL, optimistic half of that flip is waited
 *    on here, because a test that needs the SERVER's copy asserts it
 *    explicitly (`PJC-PERSIST`) rather than implicitly through a helper.
 */
async function openEditor(page: Page, project: ScratchProject): Promise<void> {
  await gotoProjectParams(page, project);
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
 * Waits until the editor's text has reached REACT state, not merely
 * CodeMirror's own document.
 *
 * CodeMirror's `onChange` is debounced (`CHANGE_DEBOUNCE_MS`, shared/ui/
 * CodeMirrorEditor), so the DOM shows a keystroke long before `content` — the
 * state the Preview pane and the "context is off" banner render from — has it.
 * Every check that reads one of those has to wait for the commit first, and
 * the fixed sleeps this file used to spend on it were a race against the
 * debounce under four workers rather than a proof: PJC07's Preview assertion
 * still lost one run in 33 on chromium behind a 300 ms one.
 *
 * The character counter is that same `content`, rendered as
 * `MAX_CHARS - content.length` (`EditorSection.tsx`), so the expected
 * remainder appearing there IS the commit — for a clear (`''`) exactly as for
 * a typed string. It is `visibility: hidden` while the editor is unfocused,
 * which does not affect `toContainText`.
 *
 * Not the Save button: `isDirty` is a flag that a change sets and only a SAVE
 * clears, so it says nothing about an edit that returns the editor to what is
 * stored (measured: after clearing, Save stays enabled).
 */
async function editorContentCommitted(page: Page, text: string): Promise<void> {
  await expect(page.getByTestId('project-context-char-counter')).toContainText(
    new RegExp(`\\b${String(MAX_CHARS - text.length)} characters left`),
    { timeout: 10_000 },
  );
}

/**
 * Clicks Save and waits for the FRONTEND to consider it settled
 * (`isDirty` reset, which only happens on `handleSave`'s success path).
 *
 * Deliberately does NOT verify the server's own row: the tests that use this
 * helper make claims about in-session/client behaviour, and `PJC-PERSIST` —
 * whose whole claim is about the stored row — reads the route itself.
 */
async function clickSave(page: Page): Promise<void> {
  const btn = page.getByTestId('project-context-save-button');
  await btn.click();
  await expect(btn).toBeDisabled({ timeout: 10_000 });
}

test.describe('project-context: settings/config coverage', () => {
  /*
   * ONE PROJECT PER TEST — see the file header.
   *
   * Provisioned in `beforeEach` and deprovisioned in `afterEach`, so a test
   * that throws mid-way still takes its tenant schema with it. The id is held
   * in a module-scope binding, which is safe because a worker runs its tests
   * one after another (the concurrency this isolates against is BETWEEN
   * workers, and between the two engines).
   *
   * The nested describes below (`as admin`, `as viewer`) inherit these hooks:
   * the scratch project makes ALL THREE personas members — admin and member as
   * project admins, viewer as a project `viewer` whose central grants withhold
   * `models.project_context.edit`.
   */
  let scratch: ScratchProject | undefined;

  test.beforeEach(async () => {
    scratch = await createScratchProject(`pjc_${test.info().project.name}`);
  });

  test.afterEach(async () => {
    const project = scratch;
    scratch = undefined;
    await deleteScratchProject(project);
  });

  /** The test's own project, or a loud failure rather than a silent fallback to a shared one. */
  function project(): ScratchProject {
    if (scratch === undefined) throw new Error('the scratch project was not provisioned');
    return scratch;
  }

  /* ── PJC-PERSIST ────────────────────────────────────────────────────
   * onetest: ELITEA-0942, ELITEA-0949, ELITEA-0950, ELITEA-0953,
   *          ELITEA-0956, ELITEA-0957 — the persistence half of each of
   *          these cases, which #888 made unprovable and this package
   *          restored. Three distinct claims, in order:
   *
   *            1. the SERVER's row holds what was saved (not the echo the
   *               broken handler used to answer with);
   *            2. a full page RELOAD reads it back — the app's own path,
   *               through the query cache, not just the raw route;
   *            3. the enable/disable TOGGLE persists the same way, including
   *               the `false` direction (ELITEA-0953), which a "write only
   *               non-empty fields" handler would silently drop.
   *
   *          The first save goes into a project that has never held a row
   *          (it was provisioned moments ago), so this covers the INSERT
   *          branch — the one the missing `project_id` column killed — and
   *          the second save covers the UPDATE branch.
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC-PERSIST: a saved Project Context survives a reload, content and toggle alike', async ({ page }) => {
    const target = project();
    await openEditor(page, target);
    const text = `${AUTOTEST_PREFIX}persist-probe-${Date.now()}`;
    await typeContent(page, text);
    await clickSave(page);

    // 1. The server's own row, not the response echo.
    await expect
      .poll(async () => (await readProjectContext(page.request, target.id)).content, {
        timeout: 8_000,
      })
      .toBe(text);
    expect((await readProjectContext(page.request, target.id)).enabled).toBe(true);

    // 2. A reload reads it back through the app.
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('project-context-body')).toBeVisible({ timeout: 20_000 });
    await expect(editorContent(page)).toContainText(text, { timeout: 10_000 });
    // Saved content means the empty state must NOT be what a reload lands on.
    await expect(page.getByTestId('project-context-empty-state')).toHaveCount(0);

    // 3. The toggle, in the direction that is easiest to lose: off.
    const toggle = page.getByRole('switch');
    await expect(toggle).toBeChecked();
    await toggle.click();
    await expect(toggle).not.toBeChecked();
    await expect
      .poll(async () => (await readProjectContext(page.request, target.id)).enabled, {
        timeout: 8_000,
      })
      .toBe(false);
    // …and the content it was saved with is not collateral damage of the
    // toggle's own write.
    expect((await readProjectContext(page.request, target.id)).content).toBe(text);

    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByRole('switch')).not.toBeChecked({ timeout: 20_000 });
  });

  /* ── PJC01 ────────────────────────────────────────────────────────────
   * onetest: ELITEA-0957 — the page loads without error in a SECOND
   * project (not the one the persona's storage state selects), for an
   * Admin, AND the two projects' contexts are independent: a save in one is
   * not a save in the other. That second half is what the schema predicate
   * in the handler's write actually buys, and it is checked here rather
   * than in PJC-PERSIST because this is the only test that holds two
   * projects at once — both of them its own, so "the other project's row is
   * still empty" is a statement about the write and not about what some
   * other worker happened to be doing.
   * ──────────────────────────────────────────────────────────────────── */
  test.describe('as admin, in the second project', () => {
    test.use({ storageState: STORAGE_STATE.admin });

    test('PJC01: Project Context is reachable and editable in a second project as Admin', async ({ page, request }) => {
      const target = project();
      // A project this test writes NOTHING into, to read back at the end.
      const untouched = await createScratchProject(`pjc01_other_${Date.now()}`);
      try {
        await openEditor(page, target);

        const text = `${AUTOTEST_PREFIX}QA admin context ${Date.now()}`;
        await typeContent(page, text);
        await expect(editorContent(page)).toContainText(text);
        await clickSave(page);
        // A SUCCESS toast is expected here (and is what `clickSave` already
        // confirmed via the button re-disabling); only an ERROR toast would
        // indicate the save was refused.
        await expect(page.getByRole('alert').filter({ hasText: /fail/i })).toHaveCount(0);

        // It landed in THIS project's row…
        await expect
          .poll(async () => (await readProjectContext(request, target.id)).content, { timeout: 8_000 })
          .toBe(text);
        // …and the other project's row is still empty: a write that ignored
        // the project id would have filled it too.
        expect((await readProjectContext(request, untouched.id)).content).toBe('');

        await checkA11y(page);
      } finally {
        await deleteScratchProject(untouched);
      }
    });
  });

  /* ── PJC02 ────────────────────────────────────────────────────────────
   * onetest: ELITEA-0942 — the in-session half: content survives edit →
   * replace → clear WITHIN one page load, and a Unicode/emoji upload
   * round-trips into both Edit and Preview mode without corruption. The
   * "survives a RELOAD" half is PJC-PERSIST's.
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC02: content survives edit/replace/clear in-session, and a Unicode upload renders correctly', async ({ page }) => {
    await openEditor(page, project());

    // Each edit below is settled — waited for POSITIVELY, through
    // `editorContentCommitted`, not slept past — before the next one starts.
    // Chaining edits without settling was measured to let a STALE debounced
    // `onChange` — from an EARLIER edit — land after a LATER one, clobbering
    // it back to the earlier value; this is what makes the Preview check
    // below meaningful rather than a coin flip.
    const versionA = `${AUTOTEST_PREFIX}Version A content`;
    await typeContent(page, versionA);
    await expect(editorContent(page)).toContainText('Version A content');
    await editorContentCommitted(page, versionA);

    const versionB = `${AUTOTEST_PREFIX}Version B content`;
    await typeContent(page, versionB);
    await expect(editorContent(page)).toContainText('Version B content');
    await expect(editorContent(page)).not.toContainText('Version A content');
    await editorContentCommitted(page, versionB);

    const editor = editorContent(page);
    await editor.click();
    await page.keyboard.press('ControlOrMeta+a');
    await page.keyboard.press('Delete');
    await expect(editor).toHaveText('');
    await editorContentCommitted(page, '');

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
    const target = project();
    await openEditor(page, target);
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

    // Reload WITHOUT saving the upload — it must be lost, and the SAVED
    // baseline must be what comes back. Both halves matter: "the upload is
    // gone" alone passed vacuously while no save persisted at all (#888),
    // because the reload landed on the empty state and there was nothing on
    // screen to contain the uploaded text.
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('project-context-body')).toBeVisible({ timeout: 20_000 });
    // 20 s, not 10: CodeMirror mounts after the query settles, and three
    // other workers are driving their own projects against the same stack.
    await expect(editorContent(page)).toContainText('baseline-before-upload', { timeout: 20_000 });
    await expect(editorContent(page)).not.toContainText('Respond concisely');

    // A 0-byte .md file is accepted and clears the editor, with no error toast.
    await openEditor(page, target);
    const placeholder = 'placeholder before clearing';
    await typeContent(page, placeholder);
    await expect(editorContent(page)).toContainText(placeholder);
    // A REAL wait, not just a DOM-text check — see PJC02's note on a stale
    // debounced `onChange` clobbering a later, faster change.
    await editorContentCommitted(page, placeholder);
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
    test.fail(true, 'ELITEA-0940 (#889): product gap — handleFileUpload never checks file name/type, only length');
    await openEditor(page, project());
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
    // against. A FIXED sleep here (previously 1.5s) is itself a race against
    // `FileReader`'s real completion time: under worker contention — webkit
    // especially, being the slower engine — the read can take longer than any
    // fixed budget, so the "not contain" check would then pass vacuously and
    // `test.fail()` would report "expected to fail, but passed" (observed
    // locally, ~30% of runs under load). Waiting POSITIVELY for the leaked
    // text to actually land — which the product gap guarantees it eventually
    // will — proves the read settled no matter how long it takes, and turns
    // the final assertion into a genuine, deterministic failure instead of a
    // coin flip.
    await expect(editorContent(page)).toContainText('Same content, wrong extension', { timeout: 10_000 });
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
    await openEditor(page, project());
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
    await openEditor(page, project());
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
   * after reload, is it independent per project) is PJC-PERSIST's.
   * ──────────────────────────────────────────────────────────────────── */
  test('PJC06: the Enable toggle changes the page immediately, in both directions', async ({ page }) => {
    await openEditor(page, project());
    const probe = `${AUTOTEST_PREFIX}toggle-probe`;
    await typeContent(page, probe);
    // The banner below is driven by REACT state (`content.trim()`), which
    // CodeMirror's `onChange` reaches only after its own debounce — same
    // race PJC02 and PJC07 wait out after typing, missing here previously.
    // `toggle` and `editorContent` were both already satisfied by
    // `openEditor`, so the two `expect`s right after typing resolve
    // instantly and give no real buffer on their own; on webkit, that was
    // thin enough (under worker contention) for the click below to land
    // before `content` had committed, so `!enabled && content.trim()`
    // read `content` as still empty and the "turned off" banner never
    // rendered. Waiting for the COMMIT makes it deterministic.
    await editorContentCommitted(page, probe);
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
    await openEditor(page, project());
    const markdown = `${AUTOTEST_PREFIX}# Heading\n\n**Bold text**`;
    await typeContent(page, markdown);
    // The Preview pane reads REACT state (`content`), which CodeMirror's
    // `onChange` reaches only after its own debounce — the CM DOCUMENT
    // updates instantly, but switching modes before the debounce fires
    // previews the PRIOR (empty) state. The editor's own text only confirms
    // CodeMirror settled; a fixed 300 ms sleep for the rest was a race, and
    // lost one run in 33 here under four workers. `editorContentCommitted`
    // waits for the state itself.
    await expect(editorContent(page)).toContainText('Bold text');
    await editorContentCommitted(page, markdown);

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

  /* ── PJC08 ────────────────────────────────────────────────────────────
   * onetest: ELITEA-0941 — Viewer Role Has Read-Only Access and Can View
   * Preview Mode (issue #940 Bucket D4).
   *
   * The viewer persona is a project `viewer` in the scratch project, added
   * by `fixtures/scratchProject.ts`. It is genuinely restricted there
   * WITHOUT the revoke `scripts/e2e-stack.sh seed` has to perform on
   * project 1: a freshly provisioned project writes no
   * `auth_core__project_role_permission` rows at all, so its roles resolve
   * the CENTRAL grants, where `models.project_context.edit` reaches
   * `admin`/`editor` only (shared/0068) and `.view` reaches every role
   * (shared/0062).
   *
   * BOTH HALVES ARE REAL NOW. ELITEA-0941's precondition is "Admin has saved
   * Markdown content", which #888 made unreachable on any persona — no
   * project on this stack could carry a saved row for a viewer to read back,
   * so the two assertions that depend on it (the read-only EDITOR showing
   * that content, and Preview rendering it as formatted Markdown) were left
   * to `ProjectContext.test.tsx` against a mocked response. With the save
   * fixed, this test arranges the precondition itself — as ADMIN, through
   * the API, because the viewer persona deliberately cannot write — and then
   * proves both halves end to end.
   *
   * The empty-state branch keeps its own test below.
   * ──────────────────────────────────────────────────────────────────────── */
  test.describe('as viewer', () => {
    test.use({ storageState: STORAGE_STATE.viewer });

    test('PJC08: a viewer reads saved Project Context read-only, in Edit and in Preview (canView, canEdit=false)', async ({
      page,
    }) => {
      const target = project();
      const saved = `${AUTOTEST_PREFIX}# Viewer Heading\n\n**Bold for the viewer**`;
      await seedProjectContextAsAdmin(target.id, { content: saved, enabled: true });

      await gotoProjectParams(page, target);

      // canView: the page loads at all — not the permission-denied banner
      // `ProjectContext.tsx` renders when `PERMISSIONS.projectContext.view`
      // is absent.
      await expect(page.getByText(/do not have permission to view this setting/i)).toHaveCount(0);

      // The saved content is on screen, in the editor a viewer gets
      // (`showEditorContent` is `enabled || content || !canEdit`).
      await expect(page.getByTestId('project-context-body')).toBeVisible({ timeout: 20_000 });
      await expect(editorContent(page)).toContainText('Bold for the viewer', { timeout: 10_000 });

      // …and it is read-only. `showEditorControls` is `enabled && canEdit`,
      // so the whole editor TOOLBAR is absent for this persona — Import,
      // Copy, the AI generator and the Edit/Preview tabs with them. Save and
      // Discard are rendered but disabled (`ProjectContextBody.tsx` keeps
      // them outside that flag and gates them on `canEdit`), so the check is
      // "disabled", not "absent": asserting absence here would pass for the
      // wrong reason if a later change hid them for everyone.
      await expect(page.getByTestId('project-context-save-button')).toBeDisabled();
      await expect(page.getByTestId('project-context-discard-button')).toBeDisabled();
      await expect(page.getByRole('button', { name: 'Import markdown file' })).toHaveCount(0);
      await expect(page.getByRole('tab', { name: 'Preview mode' })).toHaveCount(0);
      // The toggle is on screen (it is how an editor turns the context off)
      // and refuses this persona.
      await expect(page.getByRole('switch')).toBeDisabled();

      // ELITEA-0941's "and Preview renders it as Markdown" half is a legacy-
      // UI detail that does not survive the port: the mode tabs live inside
      // `showEditorControls`, so a viewer in THIS app has no Preview mode to
      // open at all. The equivalent behaviour — Preview rendering saved
      // content as formatted Markdown — is covered for an editor by PJC07,
      // and for the read-only component shape by
      // `ProjectContext.test.tsx`'s viewer tests.
    });

    test('PJC08b: with nothing saved, a viewer sees the empty state\'s read-only branch', async ({ page }) => {
      const target = project();
      // The project was provisioned moments ago and holds no row; writing the
      // empty state explicitly makes this test's precondition its OWN rather
      // than an inheritance from how provisioning happens to leave a project.
      await resetProjectContext(target.id);
      await gotoProjectParams(page, target);

      const empty = page.getByTestId('project-context-empty-state');
      await expect(empty).toBeVisible({ timeout: 20_000 });
      await expect(page.getByTestId('project-context-create-button')).toHaveCount(0);
      await expect(page.getByTestId('project-context-build-with-ai-button')).toHaveCount(0);
      await expect(page.getByText(/contact your project admin to configure it/i)).toBeVisible();
    });
  });
});
