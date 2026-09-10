/**
 * Wave-1 package toolkits-credentials/C-providers — the `personalization`
 * cluster (`S/port/pkgs/w1-tk-C-providers.md`). These 8 legacy cases live
 * under `sidebar-menu/personalization` upstream but exercise Settings › AI
 * Personality (`src/pages/settings/AIPersonality.tsx` +
 * `src/features/settings/ui/ai-personality/**`), so they are ported here,
 * not under `toolkits/`, with a `toolkits-c-` file prefix per the package
 * brief (avoids clashing with other agents' `settings.*` files).
 *
 * Two of the eight (ELITEA-2493/2494) describe a PER-CONVERSATION "Context
 * Management" panel that inherits/overrides the Settings persona. No such
 * panel exists: grepping `src/widgets/chat-box` and `src/pages/chat` for
 * `context.?management` finds nothing, and the only "Context Management" in
 * this app is `src/features/settings/ui/profile/ProfileContextManagement.tsx`
 * — a GLOBAL settings toggle for long-term-memory context budgeting, with no
 * link to `persona`/`personality_instructions` and nothing reachable from an
 * open chat conversation. Recorded NA in the ledger, not tested here.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * PRODUCT GAP FOUND WHILE PORTING THE OTHER SIX: THE PAGE CANNOT SAVE AT ALL
 * ─────────────────────────────────────────────────────────────────────────
 * `AIPersonalityFormContent`/`SettingsFormProvider.tsx` PUT the WHOLE author
 * record on every autosave (persona select, or blurring the instructions
 * field), and `settingsProfileForm.ts`'s `buildAuthorUpdate` always includes
 * `default_summarization.summary_model_project_id` — even for an account
 * that has never touched Settings › Memory (measured: `GET /social/author`
 * answers `default_summarization: {}` for the seeded `member` persona).
 * `serializeSummarization` (lines ~120-135) falls back to the ROUTE's
 * `projectId` prop when the stored value is not a JS string, and
 * `buildAuthorUpdate` (line ~198) writes that value straight through as
 * `summary_model_project_id` with no int conversion — but project ids are
 * strings throughout this app, so the field is ALWAYS a string on the wire.
 * The server rejects it outright:
 *
 *   PUT /social/author → 400
 *   {"error":"summary_model_project_id must be of type int",
 *    "field":"default_summarization.summary_model_project_id"}
 *
 * Confirmed server-side-clean by a raw API PUT that OMITS the block
 * entirely (200 `{"ok":true}` — the handler's own doc comment says it keeps
 * the stored value when the body doesn't mention it, which is exactly the
 * omission this client never makes). So this is not a stack/seed artifact:
 * it reproduces on a fresh `GET /social/author`, unconditionally, for every
 * save this form ever makes.
 *
 * That means EVERY one of the five save-dependent cases below (ELITEA-2495,
 * 2496, 2497, 2498, 2499) fails at the exact same first `selectPersona`
 * call, before any of their own distinguishing behaviour (copy/paste,
 * undo/redo, a long value, whitespace, a second tab) is ever reached. One
 * full test is kept — ELITEA-2495, `test.fail`-marked — to demonstrate the
 * defect against the richest of the five scenarios; ELITEA-2496/2497/2498/
 * 2499 are recorded DUP in the ledger (same blocking failure, same line,
 * their own assertions unreachable) rather than five near-identical
 * `test.fail` bodies that would all die on line one for an identical reason.
 * ELITEA-2500 (browser back/forward) is DUP for the same reason: its setup
 * needs two successful persona saves before its own (separate) history-gap
 * claim could even be exercised.
 *
 * `S/port/defects.md` carries the one entry all six ids point at.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';

const AI_PERSONALITY_URL = `${BASE_URL}/app/settings/ai-personality`;
const RUN_ID = `${Date.now()}`;

async function gotoAiPersonality(page: Page): Promise<void> {
  await page.goto(AI_PERSONALITY_URL, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('ai-personality-form-content')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('persona-management-section')).toBeVisible({ timeout: 30_000 });
}

function personaCombobox(page: Page) {
  return page.getByTestId('persona-management-section').getByRole('combobox');
}

function instructionsField(page: Page) {
  return page.getByRole('textbox', { name: 'User instructions' });
}

/** Selects a persona from the dropdown and waits for the auto-save PUT the select's own `onChange` fires. */
async function selectPersona(page: Page, label: string): Promise<void> {
  await personaCombobox(page).click();
  const option = page.getByRole('option').filter({ hasText: label });
  await expect(option).toHaveCount(1, { timeout: 10_000 });
  const [response] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'PUT' && r.url().includes('/social/author'), { timeout: 20_000 }),
    option.click(),
  ]);
  expect(
    response.status(),
    `ELITEA-2495/2496/2497/2498/2499/2500: PUT /social/author answered ${response.status()}: ${await response.text()}`,
  ).toBeLessThan(300);
}

/**
 * Blurs the instructions field and waits for the auto-save PUT it triggers.
 * `AIPersonalityFormContent.tsx` wires `onBlur` on the section's own wrapper
 * `Box`, and React's synthetic blur bubbles, so clicking any stable sibling
 * inside that section (the "Default persona" label) blurs the textarea and
 * fires the save.
 */
async function blurInstructionsAndSave(page: Page): Promise<void> {
  const [response] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'PUT' && r.url().includes('/social/author'), { timeout: 20_000 }),
    page.getByText('Default persona').click(),
  ]);
  expect(response.status(), await response.text()).toBeLessThan(300);
}

test('ELITEA-2495: copying instructions from one persona to another leaves the source persona untouched', async ({ page }) => {
  /* onetest: ELITEA-2495 — copy/paste between two personas' own instruction slots: the source keeps its text, the destination gets the pasted text, and each round-trips through a reload. Written to demonstrate the intended behaviour; see the file header — it fails at the FIRST persona save, before copy/paste is ever exercised. */
  test.setTimeout(60_000);
  test.fail(
    true,
    'ELITEA-2495 (and 2496/2497/2498/2499/2500): product gap — PUT /social/author always 400s ' +
      '(default_summarization.summary_model_project_id sent as a string; server wants int) on every ' +
      'autosave this form makes, so no persona/instructions change can ever be saved',
  );

  const MOD = process.platform === 'darwin' ? 'Meta' : 'Control';
  await gotoAiPersonality(page);

  const sourceText = `${AUTOTEST_PREFIX}qa_instructions_${RUN_ID}`;
  await selectPersona(page, 'QA');
  await instructionsField(page).fill(sourceText);
  await blurInstructionsAndSave(page);

  // Select-all + copy out of the QA slot.
  await instructionsField(page).click();
  await page.keyboard.press(`${MOD}+a`);
  await page.keyboard.press(`${MOD}+c`);

  // Switch to a persona whose own slot starts empty, and paste into it.
  await selectPersona(page, 'Nerdy');
  await expect(instructionsField(page)).toHaveValue('');
  await instructionsField(page).click();
  await page.keyboard.press(`${MOD}+v`);
  await expect(instructionsField(page)).toHaveValue(sourceText);
  await blurInstructionsAndSave(page);

  // Reload and verify both slots independently: QA is unchanged, Nerdy now
  // carries the pasted copy.
  await gotoAiPersonality(page);
  await selectPersona(page, 'QA');
  await expect(instructionsField(page)).toHaveValue(sourceText);
  await selectPersona(page, 'Nerdy');
  await expect(instructionsField(page)).toHaveValue(sourceText);
});
