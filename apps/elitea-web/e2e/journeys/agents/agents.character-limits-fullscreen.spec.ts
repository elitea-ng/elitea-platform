/**
 * Two more use cases from the same onetest character-limit family
 * `agents.editor.spec.ts` already ports the collapsed half of
 * (ELITEA-0059/0060/0061/0062/0063/0064 — see that file's own module doc
 * comment): the ADD-A-NEW-STARTER-VIA-BUTTON round trip (ELITEA-0060), and
 * the FULL SCREEN editor for the two character-LIMITED fields (Welcome
 * Message, Conversation Starters), as opposed to the unlimited Instructions
 * field `agents.editor.spec.ts` already opens full screen.
 *
 * ── Why these are separate from `agents.editor.spec.ts` ────────────────────
 *
 * That file's own comment records the `+ Starter` click as "still open work"
 * (issue #848's own follow-up note) and seeds starters through the API
 * instead. #848 fixed the BLUR-SWALLOWS-THE-CLICK defect it documents, so the
 * button is clickable now — this file drives it for real, which is the one
 * thing that comment says nobody had done yet.
 *
 * The FULL SCREEN half is a DIFFERENT control from the one that file already
 * exercises: `InstructionsInput` opens its own bespoke fullscreen dialog
 * (`agent-instructions-fullscreen-button`/`-dialog`), unrelated to the
 * generic one below. Welcome Message and Conversation Starters instead go
 * through `shared/ui/StyledInputEnhancer`'s built-in toolbar action
 * (`InputBase`'s "Full screen view" button, `showFullScreen: true` by
 * default) — read directly off `StyledInputEnhancer.tsx`: its `BaseModal`
 * content is a SECOND, bare `InputBase` sharing the same `value`/`onChange`,
 * carrying `expand`/`slotProps.htmlInput['aria-label']` and NOTHING else —
 * no `maxLength`, no `CharacterCounter`. The collapsed instance's
 * `slotProps.htmlInput.maxLength`/`data-testid` and the
 * `CharacterCounter` rendered beside it belong to `WelcomeMessageInput`/
 * `ConversationStartersEditor`'s OWN JSX, sitting outside `StyledInputEnhancer`
 * entirely — the full-screen modal never sees either.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createAgent, deleteAgent } from '../../fixtures/api';

import type { Page } from '@playwright/test';

const MAX_TEXT_FIELD_CHARS = 768;

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}cs-fullscreen-${stem}-${String(Date.now()).slice(-7)}`;
}

async function openAgentEditor(page: Page, agentId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
}

/*
 * ELITEA-0060 — clicking "+ Starter" adds a real row, typing a long value
 * into it and into the Welcome Message and saving persists BOTH across a
 * reload.
 */
/* onetest: ELITEA-0060 — clicking + Starter adds a real row, and both it and the welcome message persist */
test('J14c-fs: clicking + Starter adds a real row, and both it and the welcome message persist', async ({
  page,
  request,
}) => {
  const name = uniqueName('add-starter');
  const agent = await createAgent(request, name);
  const welcomeText = 'W'.repeat(700);
  const starterText = 'S'.repeat(700);
  try {
    await openAgentEditor(page, agent.id);
    const panel = page.getByTestId('edit-application-configuration-tab-panel');

    const welcome = panel.getByTestId('agent-welcome-message-input');
    await welcome.fill(welcomeText);
    // The counter update while typing — the collapsed-mode half of ELITEA-0064,
    // already asserted in full by `agents.editor.spec.ts`; reasserted here only
    // as a cheap "the right field got the text" signal, not re-derived logic.
    await expect(panel.getByTestId('agent-welcome-message-counter')).toHaveText(
      `${MAX_TEXT_FIELD_CHARS - 700} characters left`,
    );

    // No starter rows exist yet — this agent was seeded with none.
    await expect(panel.getByTestId('agent-conversation-starter-input')).toHaveCount(0);

    const addButton = panel.getByTestId('agent-conversation-starter-add');
    await expect(addButton).toBeVisible({ timeout: 10_000 });
    await addButton.click();

    const starterInput = panel.getByTestId('agent-conversation-starter-input').first();
    await expect(starterInput, 'the + Starter click must add a real, typeable row').toBeVisible({ timeout: 10_000 });
    // #848 — the new row takes focus on add, so a direct `.fill` lands on it
    // without a separate click; asserted as the row's OWN behaviour, not
    // assumed.
    await starterInput.fill(starterText);
    await expect(starterInput).toHaveValue(starterText);

    const saved = page.waitForResponse(
      (response) => response.request().method() === 'PUT' && response.url().includes('/elitea_core/version/'),
    );
    await page.getByTestId('agent-save-button').click();
    const saveResponse = await saved;
    expect(saveResponse.status()).toBeLessThan(400);

    await page.reload();
    await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId('agent-welcome-message-input')).toHaveValue(welcomeText, { timeout: 20_000 });
    await expect(page.getByTestId('agent-conversation-starter-input').first()).toHaveValue(starterText, {
      timeout: 20_000,
    });

    const stored = await request.get(
      `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}/${agent.versionId}`,
    );
    const storedBody = await stored.json();
    expect(storedBody?.welcome_message).toBe(welcomeText);
    expect(storedBody?.conversation_starters).toEqual([starterText]);
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/*
 * ELITEA-0059, ELITEA-0062, ELITEA-0064 (their FULL-SCREEN halves; the
 * collapsed-mode halves of all three are already asserted by
 * `agents.editor.spec.ts`) — [PRODUCT GAP].
 *
 * Written as the use case SHOULD behave: opening the Welcome Message field
 * full-screen should carry the same 768-character limit and the same
 * counter the collapsed field has. It does not — `StyledInputEnhancer`'s
 * modal `InputBase` carries neither `maxLength` nor a `CharacterCounter`
 * (see this file's own module doc comment for the exact lines), so typing
 * past 768 characters in the full-screen editor is silently accepted with
 * no counter and no warning at all.
 */
/* onetest: ELITEA-0059, ELITEA-0062, ELITEA-0064 — the full-screen Welcome Message editor enforces the 768-char limit and shows a counter */
test('J14c-fs: [PRODUCT GAP] the full-screen Welcome Message editor enforces the 768-char limit and shows a counter', async ({
  page,
  request,
}) => {
  test.fail(
    true,
    'ELITEA-0059/0062/0064: product gap — StyledInputEnhancer’s full-screen modal InputBase carries no maxLength and no CharacterCounter, so the 768-char contract and its counter/warning are dropped in full-screen mode',
  );
  const name = uniqueName('fs-gap');
  const agent = await createAgent(request, name);
  try {
    await openAgentEditor(page, agent.id);

    // The Name, Description and Advanced/Model fields carry their OWN
    // "Full screen view" toolbar actions too (`InputBase`'s generic toolbar,
    // same component) — a bare `getByRole('button', { name: 'Full screen
    // view' })` matched 4 elements on this exact page, measured. Scoped to
    // the accordion REGION that actually contains the Welcome Message
    // textarea (identified by its own placeholder text), which is
    // unambiguous regardless of how many other fields on the page carry the
    // same generic action.
    const welcomeRegion = page
      .getByRole('region')
      .filter({ has: page.getByPlaceholder('Input your welcome message') });
    const fullScreenButton = welcomeRegion.getByRole('button', { name: 'Full screen view' });
    await expect(fullScreenButton).toBeVisible({ timeout: 10_000 });
    await fullScreenButton.click();

    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible({ timeout: 10_000 });
    const fullScreenTextbox = dialog.getByRole('textbox');
    await expect(fullScreenTextbox).toBeVisible({ timeout: 10_000 });

    // The 768-char CONTRACT: the browser-enforced `maxlength` the collapsed
    // field carries (`WelcomeMessageInput.tsx`'s `slotProps.htmlInput.maxLength`)
    // must survive into the full-screen editor.
    await expect(fullScreenTextbox).toHaveAttribute('maxlength', String(MAX_TEXT_FIELD_CHARS));

    await fullScreenTextbox.fill('X'.repeat(MAX_TEXT_FIELD_CHARS));
    await fullScreenTextbox.press('End');
    await fullScreenTextbox.pressSequentially('Z');
    expect(
      (await fullScreenTextbox.inputValue()).length,
      'typing past the limit in full screen must be refused exactly as the collapsed field refuses it',
    ).toBe(MAX_TEXT_FIELD_CHARS);

    // The COUNTER: the same "0 characters left. You have reached the MAXIMUM
    // character limit" sentence the collapsed field shows at the limit.
    await expect(dialog.getByText(/characters left/i)).toBeVisible({ timeout: 5_000 });
  } finally {
    await deleteAgent(request, agent.id);
  }
});
