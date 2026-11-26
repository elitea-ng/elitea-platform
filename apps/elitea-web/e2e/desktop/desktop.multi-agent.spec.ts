/**
 * Several agents in one desktop thread (ADR-0050 decision 7f, #1806): a
 * thread with a LOCAL and a CLOUD agent, on the desktop shell's dev harness
 * (`/?harness`: the fake host and canned API, no stack, no Tauri).
 *
 * What it proves, end to end in the UI:
 *  - the composer adds a second agent to the thread as a cloud agent;
 *  - the first send adds both agents to the conversation, the cloud one with
 *    `entity_settings.executor: cloud`;
 *  - a message naming both ("@Coder … @Reviewer") starts on the first one
 *    named, with `addressed_to` in the text's order and no "Run to
 *    completion" grant (the host follows the floor instead);
 *  - each answer shows which agent gave it and where it ran; the second
 *    agent's turn (started by the host, `turn_continued` kind `floor`) takes
 *    no prompt of its own;
 *  - in a thread with a lead (#1917), a message that names no agent carries
 *    no `addressed_to` (a composer pick is not an address) and starts on the
 *    lead; a message naming an agent starts on that agent;
 *  - a thread run by a saved team (#3053) writes the team's lead and starts
 *    on it.
 *
 * The host's side — following the floor, the cloud relay with text only — is
 * D0's `floor_tests.rs`; Main's routing is `localturn/floor_test.go`.
 */
import { expect, test, type Page } from '@playwright/test';

interface ParticipantRow {
  entity_name: string;
  entity_meta?: { id?: number };
  entity_settings?: { executor?: string };
}

interface HarnessWindow {
  __harness: {
    ipc: { calls: { started: Record<string, unknown>[]; exchangeStops: string[] } };
    participantPosts: ParticipantRow[][];
    metaWrites: unknown[];
    playFloor: () => void;
    playExchange: () => void;
    stopExchange: () => void;
    setLead: (participantId: number) => void;
    playTurn: () => void;
  };
}

const harness = (page: Page) => ({
  started: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.ipc.calls.started),
  posts: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.participantPosts),
  metaWrites: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.metaWrites),
  playFloor: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.playFloor()),
  playExchange: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.playExchange()),
  stopExchange: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.stopExchange()),
  exchangeStops: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.ipc.calls.exchangeStops),
  setLead: (id: number) => page.evaluate((lead) => (window as unknown as HarnessWindow).__harness.setLead(lead), id),
  playTurn: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.playTurn()),
});

// A cold Vite server compiles the whole shell on the first load.
test.setTimeout(180_000);

test('a thread with a local and a cloud agent answers one message in turn', async ({ page }) => {
  const h = harness(page);
  await page.goto('/?harness&at=/workspaces/w1');

  // Coder (local) is the composer's agent; add Reviewer as a cloud agent.
  const chip = page.getByTestId('agent-chip');
  await expect(chip).toContainText('Coder', { timeout: 150_000 });
  await chip.click();
  await page.getByTestId('agent-add-cloud').click();
  await page.getByTestId('agent-add-cloud-6').click();
  await expect(chip).toContainText('Reviewer');

  await page.getByRole('textbox').fill('@Coder fix the flaky test, then @Reviewer check it');
  await page.getByTestId('workspace-send-button').click();

  // The turn starts on the first agent named, addressed to both in the text's order.
  await expect.poll(async () => (await h.started()).length).toBe(1);
  const [request] = await h.started();
  expect(request?.['addressed_to']).toEqual([77, 88]);
  expect(request?.['application_id']).toBe(5);
  expect(request?.['version_id']).toBe(9);
  expect(request).not.toHaveProperty('cloud_agent');
  expect(request).not.toHaveProperty('unattended');

  // Both agents joined the conversation; the cloud one's mapping says so.
  const agents = (await h.posts())
    .flat().filter((row) => row.entity_name === 'application');
  expect(agents.map((row) => row.entity_meta?.id)).toEqual([5, 6]);
  expect(agents.find((row) => row.entity_meta?.id === 6)?.entity_settings?.executor).toBe('cloud');
  expect(agents.find((row) => row.entity_meta?.id === 5)?.entity_settings?.executor).toBeUndefined();

  // The host answers: the first agent here, then the next one in the cloud.
  await h.playFloor();
  const answered = page.getByTestId('turn-agent');
  await expect(answered).toHaveCount(2);
  await expect(answered.nth(0)).toHaveAttribute('data-executor', 'local');
  await expect(answered.nth(0)).toContainText('Coder');
  await expect(answered.nth(1)).toHaveAttribute('data-executor', 'cloud');
  await expect(answered.nth(1)).toContainText('Reviewer');
  await expect(page.getByText('Reviewed from the conversation: the fix looks right.')).toBeVisible();
  // One message, two answers: the second agent's turn shows no prompt of its own, nor a chain marker.
  await expect(page.getByTestId('your-prompt')).toHaveCount(1);
  await expect(page.getByTestId('turn-continued-marker')).toHaveCount(0);
  // The thread is free again once the last agent is done.
  await expect(page.getByTestId('workspace-send-button')).toBeVisible();
});

/**
 * A thread with a lead (ADR-0050 decisions 7c and 8, #1917): Reviewer (participant 88, a cloud agent) leads, while the
 * composer shows Coder. A composer pick is not an address: a message that names no agent is sent with no
 * `addressed_to` and starts on the lead; a message naming an agent starts on that agent, addressed to it.
 */
test('in a lead thread an unaddressed message goes to the lead, an addressed one to its addressee', async ({ page }) => {
  const h = harness(page);
  await page.goto('/?harness&at=/workspaces/w1');

  const chip = page.getByTestId('agent-chip');
  await expect(chip).toContainText('Coder', { timeout: 150_000 });
  await chip.click();
  await page.getByTestId('agent-add-cloud').click();
  await page.getByTestId('agent-add-cloud-6').click();
  await expect(chip).toContainText('Reviewer');

  // The first message creates the thread with both agents.
  await page.getByRole('textbox').fill('@Coder look at the flaky test');
  await page.getByTestId('workspace-send-button').click();
  await expect.poll(async () => (await h.started()).length).toBe(1);
  await h.playTurn();
  await expect(page.getByTestId('workspace-send-button')).toBeVisible();

  // The thread gets a lead: Reviewer.
  await h.setLead(88);

  // Names no agent: the lead answers it, though the composer still shows Coder.
  await expect(chip).toContainText('Coder');
  await page.getByRole('textbox').fill('fix the flaky test');
  await page.getByTestId('workspace-send-button').click();
  await expect.poll(async () => (await h.started()).length).toBe(2);
  const unaddressed = (await h.started())[1];
  expect(unaddressed).not.toHaveProperty('addressed_to');
  expect(unaddressed?.['application_id']).toBe(6);
  expect(unaddressed?.['version_id']).toBe(10);
  expect(unaddressed?.['cloud_agent']).toBe(true);
  await h.playTurn();
  await expect(page.getByTestId('workspace-send-button')).toBeVisible();

  // Names Coder: addressed to it, and Coder answers, not the lead.
  await page.getByRole('textbox').fill('@Coder add a regression test');
  await page.getByTestId('workspace-send-button').click();
  await expect.poll(async () => (await h.started()).length).toBe(3);
  const addressed = (await h.started())[2];
  expect(addressed?.['addressed_to']).toEqual([77]);
  expect(addressed?.['application_id']).toBe(5);
  expect(addressed).not.toHaveProperty('cloud_agent');
});

/**
 * A thread run by a saved team (#3053): "Run as a team ›" lists the project's teams; one with an agent of another
 * project is disabled and says why. Picking "Review crew" (Coder and Reviewer, Reviewer leads) makes Reviewer the
 * composer's agent on the team's version and Coder the thread's other agent, both on this computer. The first send adds
 * both as Teams → Start adds them, writes Reviewer as the thread's lead (`meta.floor`), and starts on the lead.
 */
test('a thread run by a team writes its lead and starts on it', async ({ page }) => {
  const h = harness(page);
  await page.goto('/?harness&at=/workspaces/w1');

  const chip = page.getByTestId('agent-chip');
  await expect(chip).toContainText('Coder', { timeout: 150_000 });
  await chip.click();
  await page.getByTestId('agent-team').click();
  await expect(page.getByTestId('agent-team-4')).toHaveAttribute('aria-disabled', 'true');
  await expect(page.getByTestId('agent-team-4')).toContainText('Agents of another project');
  await page.getByTestId('agent-team-3').click();
  await expect(chip).toContainText('Review crew: Reviewer · base + Coder');
  const notice = page.getByTestId('workspace-team-notice');
  await expect(notice).toContainText('goes to Reviewer (lead)');
  await expect(notice).toContainText('Only an agent that cannot run on this computer runs in the cloud without the folder');

  await page.getByRole('textbox').fill('fix the flaky test');
  await page.getByTestId('workspace-send-button').click();

  // The turn starts on the lead, on this computer, with no address (a message that names no agent).
  await expect.poll(async () => (await h.started()).length).toBe(1);
  const [request] = await h.started();
  expect(request?.['application_id']).toBe(6);
  expect(request?.['version_id']).toBe(10);
  expect(request?.['participant_id']).toBe(88);
  expect(request).not.toHaveProperty('addressed_to');
  expect(request).not.toHaveProperty('cloud_agent');

  // Both members joined on the team's versions, neither in the cloud; the lead was written once.
  const agents = (await h.posts()).flat().filter((row) => row.entity_name === 'application');
  expect(agents.map((row) => row.entity_meta?.id)).toEqual([6, 5]);
  expect(agents.every((row) => row.entity_settings?.executor === undefined)).toBe(true);
  const writes = await h.metaWrites();
  expect(writes).toHaveLength(1);
  expect(writes[0]).toMatchObject({ floor: { policy: 'lead', lead_participant_id: 88 } });

  await h.playTurn();
  await expect(page.getByTestId('workspace-send-button')).toBeVisible();
});

/**
 * ADR-0061 decisions 2b and 2c (#3056, #3058), in a thread run by a team (Coder and Reviewer, Reviewer leads): "@" lists
 * the thread's agents first, and picking one addresses the message to it; while a message's agents answer in turn, the
 * exchange strip shows who answers now and next, and Stop exchange tells the host to stop (no later agent starts).
 */
test('the @ menu addresses an agent, and Stop exchange starts no later agent', async ({ page }) => {
  const h = harness(page);
  await page.goto('/?harness&at=/workspaces/w1');

  const chip = page.getByTestId('agent-chip');
  await expect(chip).toContainText('Coder', { timeout: 150_000 });
  await chip.click();
  await page.getByTestId('agent-team').click();
  await page.getByTestId('agent-team-3').click();
  await expect(chip).toContainText('Review crew');
  await page.getByRole('textbox').fill('look at the flaky test');
  await page.getByTestId('workspace-send-button').click();
  await expect.poll(async () => (await h.started()).length).toBe(1);
  await h.playTurn();
  await expect(page.getByTestId('workspace-send-button')).toBeVisible();

  // "@Rev": Reviewer leads the menu; picking it puts "@Reviewer " into the message.
  const input = page.getByRole('textbox');
  await input.click();
  await input.pressSequentially('@Rev');
  const menu = page.getByTestId('workspace-file-menu');
  await expect(menu.getByRole('option').first()).toHaveAccessibleName('@Reviewer');
  await menu.getByRole('option', { name: '@Reviewer' }).click();
  await input.pressSequentially('check the fix');
  await expect(page.getByTestId('workspace-to-chip')).toContainText('Reviewer');
  await page.getByTestId('workspace-send-button').click();
  await expect.poll(async () => (await h.started()).length).toBe(2);
  expect((await h.started())[1]?.['addressed_to']).toEqual([88]);
  await h.playTurn();
  await expect(page.getByTestId('workspace-send-button')).toBeVisible();

  // Two agents answer in turn: the strip names them, and Stop exchange stops the rest.
  await input.fill('@Coder run the tests, then @Reviewer check them');
  await page.getByTestId('workspace-send-button').click();
  await expect.poll(async () => (await h.started()).length).toBe(3);
  await h.playExchange();
  const strip = page.getByTestId('workspace-exchange-strip').first();
  await expect(strip).toBeVisible();
  await expect(strip.getByTestId('workspace-exchange-now')).toContainText('Coder');
  await expect(strip.getByTestId('workspace-exchange-next')).toContainText('Reviewer');
  await strip.getByTestId('workspace-exchange-stop').click();
  await expect.poll(async () => (await h.exchangeStops()).length).toBe(1);
  await h.stopExchange();
  await expect(page.getByTestId('turn-floor-stopped')).toContainText('You stopped the exchange.');
  await expect(page.getByTestId('workspace-exchange-strip')).toHaveCount(0);
  expect(await h.started()).toHaveLength(3);
});

