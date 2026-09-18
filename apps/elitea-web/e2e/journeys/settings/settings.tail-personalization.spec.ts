/**
 * Wave-1 tail package T1c — `sidebar-menu/personalization` folder (the
 * "Bare" persona cases, ELITEA-1718..1723).
 *
 * ## Where this collapses to ONE real access point
 *
 * The onetest source describes 1718 as a per-conversation "Context
 * Management" modal with its own persona dropdown, opened from a chat
 * conversation's header. No such modal exists in this build: the in-chat
 * "Context Budget" dialog (`chat.contextBudget.spec.ts`'s
 * `context-budget-edit-dialog`) edits only `max_context_tokens` — no persona
 * field, no "USER INSTRUCTIONS" section, nothing reachable from an open
 * conversation. `toolkits-c-ai-personality.spec.ts`'s own header records the
 * same finding for its sibling ids (ELITEA-2493/2494, NA). The ONE real
 * surface for persona selection in this app is Settings › Personalization
 * (`ProfilePersonalization.tsx`), which is what this file drives — folding
 * 1718 ("Bare" is listed) and the reachable half of 1719 ("Bare" is
 * selectable) into a single test at that surface.
 *
 * ## Why nothing here SAVES a persona
 *
 * `ProfileFormContent.tsx` wires `onBlur={onBlur}` on the whole section
 * wrapper (`useFormikAutoSaveOnBlur`), so selecting a persona and blurring
 * the control autosaves via `PUT /social/author` — the exact route
 * `toolkits-c-ai-personality.spec.ts`'s header documents as UNCONDITIONALLY
 * 400ing for this deployment's seeded accounts (`deserializeProfileFormData`
 * sends `summary_model_project_id` as the STRING project id the app uses
 * everywhere, and the Go handler wants an int). That test file already
 * carries one `test.fail`-marked case demonstrating the defect
 * (ELITEA-2495) and records every other save-dependent id in that folder as
 * DUP rather than repeating it. ELITEA-1719's PERSISTENCE half and
 * ELITEA-1720/1721/1722/1723 (which all need a persona SAVED, and the last
 * four also need a real model turn this stack's `journeys` project has no
 * worker for) are recorded the same way in this package's ledger — see
 * `S/tail/ledger-T1c.tsv`. What is asserted below never clicks the option
 * that would trigger the save: it opens the dropdown, reads the list, and
 * closes it with Escape.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

const PERSONALIZATION_URL = `${BASE_URL}/app/settings/personalization`;

/* onetest: ELITEA-1718, ELITEA-1719 (reachable half) — "Bare" is listed in the Default Personality
 * dropdown alongside every other persona, and is a real, clickable option — without ever committing
 * a selection through the broken autosave. */
test('ELITEA-1718: "Bare" appears in the Default Personality dropdown alongside every other persona', async ({
  page,
}) => {
  await page.goto(PERSONALIZATION_URL, { waitUntil: 'domcontentloaded' });

  // `ProfileUserInfo` (first section) renders no combobox at all, so the
  // FIRST one in document order is `ProfilePersonalization`'s persona select
  // — ahead of `ProfileContextManagement`'s and `VoicePersonalizationSection`'s
  // own selects, both later siblings in `ProfileFormContent.tsx`.
  const combobox = page.getByRole('combobox').first();
  await expect(combobox, 'the Personalization page must render the Default Personality select').toBeVisible({
    timeout: 30_000,
  });
  await combobox.click();

  const options = page.getByRole('option');
  await expect(options.first(), 'the persona dropdown must open with at least one option').toBeVisible({
    timeout: 10_000,
  });
  const labels = (await options.allTextContents()).map((label) => label.trim());

  expect(labels, 'the "Bare" persona must be listed').toContain('Bare');
  // The full catalogue is present alongside it — Bare is an ADDITION, not a
  // replacement for any of the named personas.
  for (const name of ['Generic', 'QA', 'Nerdy', 'Quirky', 'Cynical', 'None']) {
    expect(labels, `the "${name}" persona must still be listed beside "Bare"`).toContain(name);
  }

  // Bare is a real, clickable option (the client-side half of "selectable")
  // — selected optimistically without ever letting the autosave's blur fire.
  const bareOption = options.filter({ hasText: 'Bare' });
  await expect(bareOption).toHaveCount(1);
  await expect(bareOption, 'the Bare option must be enabled, not a disabled placeholder row').toBeEnabled();

  await page.keyboard.press('Escape');
  await expect(options).toHaveCount(0);
});
