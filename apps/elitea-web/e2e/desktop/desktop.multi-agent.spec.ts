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
 *    lead; a message naming an agent starts on that agent.
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
    ipc: { calls: { started: Record<string, unknown>[] } };
    participantPosts: ParticipantRow[][];
    playFloor: () => void;
    setLead: (participantId: number) => void;
    playTurn: () => void;
  };
}

const harness = (page: Page) => ({
  started: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.ipc.calls.started),
  posts: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.participantPosts),
  playFloor: () => page.evaluate(() => (window as unknown as HarnessWindow).__harness.playFloor()),
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

