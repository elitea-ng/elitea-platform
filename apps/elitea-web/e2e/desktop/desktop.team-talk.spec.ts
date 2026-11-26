/**
 * Agents talking to each other in a Local work thread run by a saved team (ADR-0061 decisions 2a, 3d, 3e and 4c;
 * #3064 #3065 #3055 #3057), on the desktop shell's dev harness (`/?harness`: the fake host plays the contract's
 * events, `devHarnessTeamTalk.ts`).
 *
 * What it proves, end to end in the UI:
 *  - the lead (Reviewer) hands the floor to Coder: the hand-off is a folded side row "Reviewer → Coder", Coder's turn
 *    (`turn_continued` kind `hand`) takes no prompt of its own, and Reviewer's avatar says it handed to Coder;
 *  - Coder consults Reviewer: Coder's avatar waits on Reviewer, and the question and the reply are side rows;
 *  - "Make lead" in the Participants tab writes the new lead to the thread.
 */
import { expect, test, type Page } from '@playwright/test';

interface HarnessWindow {
  __harness: {
    ipc: { calls: { started: unknown[] } };
    metaWrites: unknown[];
    playTurn: () => void;
    playHandOff: (stage: 'handing' | 'handed' | 'consulting' | 'done') => void;
  };
}

const harness = (page: Page) => ({
  started: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.ipc.calls.started.length),
  metaWrites: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.metaWrites),
  playTurn: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.playTurn()),
  playHandOff: (stage: 'handing' | 'handed' | 'consulting' | 'done') => page.evaluate((s) => (window as unknown as HarnessWindow).__harness.playHandOff(s), stage),
});

// A cold Vite server compiles the whole shell on the first load.
test.setTimeout(180_000);

/** Opens a thread run by "Review crew" (Coder and Reviewer, Reviewer leads) and sends its first message. */
async function teamThread(page: Page): Promise<void> {
  const h = harness(page);
  await page.goto('/?harness&at=/workspaces/w1');
  const chip = page.getByTestId('agent-chip');
  await expect(chip).toContainText('Coder', { timeout: 150_000 });
  await chip.click();
  await page.getByTestId('agent-team').click();
  await page.getByTestId('agent-team-3').click();
  await expect(chip).toContainText('Review crew');
  await page.getByRole('textbox').fill('lint-web is red, please fix it');
  await page.getByTestId('workspace-send-button').click();
  await expect.poll(() => h.started()).toBe(1);
}

/** The thread's Participants tab in the inspector (shown first: the thread has two agents). */
async function participantsTab(page: Page) {
  const toggle = page.getByTestId('inspector-toggle');
  if (await toggle.isVisible()) await toggle.click();
  await page.getByRole('tab', { name: /Participants/ }).click();
  const tab = page.getByTestId('workspace-participants-tab');
  await expect(tab).toBeVisible();
  return tab;
}

const stateOf = (page: Page, participantId: number) => page.locator(`[data-testid="participant-state"][data-participant-id="${String(participantId)}"]`);

test('the lead hands off to a member, the member consults another, all with avatars', async ({ page }) => {
  const h = harness(page);
  await teamThread(page);
  const tab = await participantsTab(page);

  // Reviewer hands the floor to Coder: its avatar says so (a one-shot state, 3 s).
  await h.playHandOff('handing');
  await expect(stateOf(page, 88)).toContainText('Handed to Coder');
  // Reviewer's turn is done and the host starts Coder's turn.
  await h.playHandOff('handed');
  const sideTalk = page.getByTestId('workspace-side-talk');
  await expect(sideTalk).toHaveCount(1);
  await expect(sideTalk.first().getByTestId('chat-side-talk-chip')).toHaveText(/Side conversation \(1\)/);
  await sideTalk.first().getByTestId('chat-side-talk-chip').click();
  await expect(sideTalk.first().getByTestId('chat-side-talk-label')).toHaveText('Reviewer → Coder');
  await expect(sideTalk.first()).toContainText('Can you take lint-web?');
  // One message: the handed agent's turn has no prompt of its own.
  await expect(page.getByTestId('your-prompt')).toHaveCount(1);
  await expect(page.getByTestId('turn-agent').last()).toContainText('Coder');

  // Coder asks Reviewer and waits on it.
  await h.playHandOff('consulting');
  await expect(stateOf(page, 77)).toContainText('Waiting for Reviewer');
  await expect(tab.getByTestId('participant-state')).not.toHaveCount(0);

  // Reviewer replies; Coder finishes.
  await h.playHandOff('done');
  await expect(sideTalk).toHaveCount(2);
  const consult = sideTalk.nth(1);
  await expect(consult.getByTestId('chat-side-talk-chip')).toHaveText(/Side conversation \(2\)/);
  await consult.getByTestId('chat-side-talk-chip').click();
  const rows = consult.getByTestId('chat-side-talk-row');
  await expect(rows.nth(0)).toContainText('Coder asked Reviewer: Is the import-order rule on for tests too?');
  await expect(rows.nth(1).getByTestId('chat-side-talk-label')).toHaveText('Reviewer → Coder');
  await expect(rows.nth(1)).toContainText('Yes, tests included.');
  await expect(page.getByText('Fixed the import order in lint-web; the job is green.')).toBeVisible();
  await expect(page.getByTestId('workspace-send-button')).toBeVisible();
});

test('Make lead in the Participants tab writes the new lead', async ({ page }) => {
  const h = harness(page);
  await teamThread(page);
  await h.playTurn();
  await expect(page.getByTestId('workspace-send-button')).toBeVisible();
  // The first send wrote Reviewer (88) as the team's lead.
  await expect.poll(async () => (await h.metaWrites()).length).toBe(1);

  const tab = await participantsTab(page);
  const coder = tab.locator('[data-testid="participant-lead"][data-participant-id="77"]');
  await expect(coder).toBeVisible();
  await expect(coder.getByTestId('participant-lead-badge')).toHaveCount(0);
  // Hover first: the row's hover actions take room and move the menu (see chat.multiAgent).
  await coder.hover();
  await coder.getByTestId('participant-lead-menu').click();
  await page.getByTestId('participant-lead-make').click();

  await expect.poll(async () => (await h.metaWrites()).length).toBe(2);
  expect((await h.metaWrites())[1]).toMatchObject({ floor: { policy: 'lead', lead_participant_id: 77 } });
  await expect(coder.getByTestId('participant-lead-badge')).toBeVisible();
});
