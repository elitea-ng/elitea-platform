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
 * #929 (FIXED) — every autosave 400'd; root-caused and closed
 * ─────────────────────────────────────────────────────────────────────────
 * `AIPersonalityFormContent`/`SettingsFormProvider.tsx` PUT the WHOLE author
 * record on every autosave (persona select, or blurring the instructions
 * field), and `settingsProfileForm.ts`'s `buildAuthorUpdate` always included
 * `default_summarization.summary_model_project_id` — even for an account
 * that has never touched Settings › Memory. `serializeSummarization` falls
 * back to the ROUTE's `projectId` prop when the stored value is not a JS
 * string, and the OLD `buildAuthorUpdate` wrote that value straight through
 * as `summary_model_project_id` with no int conversion — but project ids
 * are strings throughout this app, while this ONE wire field is genuinely
 * typed int (`MemorySummarization.summary_model_project_id`, `zod.int()`;
 * `internal/domain/contextsettings/userdefaults.go`'s `*int`), so the
 * server rejected it outright:
 *
 *   PUT /social/author → 400
 *   {"error":"summary_model_project_id must be of type int",
 *    "field":"default_summarization.summary_model_project_id"}
 *
 * Fixed at the wire boundary, not the form's own type: `deserializeMemoryBlocks`
 * now calls a new `projectIdField` helper that sends `Number(model_project_id)`
 * when it parses to an integer and OMITS the key otherwise (the server keeps
 * the stored value for an absent key, per its own doc comment) — `model_
 * project_id` itself stays the string id everywhere else in Formik state and
 * in the UI. All six cases below (ELITEA-2495-2500) were blocked on this one
 * failure at the first `selectPersona` call; ELITEA-2495 is kept as the full
 * scenario (copy/paste across personas) and 2496-2500 remain DUP in the
 * ledger — same mechanism, not a distinct assertion once the save itself
 * works.
 *
 * NEW, SEPARATE finding while verifying the fix: with the 400 gone, this
 * case now exposes an intermittent (roughly every-other-run) failure to fire
 * the autosave PUT AT ALL within the 20s window — confirmed NOT a server-side
 * issue (a direct authenticated `PUT /social/author` always answers 200 in
 * under 100ms even seconds after a UI-driven attempt times out; `podman logs`
 * shows no server-side slowness). The request never leaves the browser: a
 * timing/composition issue in `useFormikAutoSaveOnBlur`'s debounce+retry
 * (`shared/lib/hooks/useFormikAutoSaveOnBlur.ts`) or in how `SingleSelect`'s
 * `onChange`/the wrapping `Box`'s `onBlur` compose with it — pre-existing,
 * unrelated to #929's type mismatch, and never previously observable because
 * every save died on the 400 before this path could matter. Left unfixed
 * (root-causing a ~50%-flaky client timing issue is its own investigation);
 * flagged here for a new issue rather than silently left flaky.
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
  /* onetest: ELITEA-2495 — copy/paste between two personas' own instruction slots: the source keeps its text, the destination gets the pasted text, and each round-trips through a reload. #929 fixed: `summary_model_project_id` now goes out as an int (`settingsProfileForm.ts`'s `projectIdField`), so every autosave PUT succeeds. */
  test.setTimeout(60_000);

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
