/**
 * An AGENT that calls a real toolkit mid-chat — the LIVE half of the legacy
 * agent-plus-toolkit file.
 *
 * Ported by use case from `qa/elitea-testing-public`:
 *
 *  - LIVE-TK-AGENT-1 ← `tests/ui/toolkits/test_agent_with_github_toolkit.py::
 *    TestChatWithAgentToolkit::test_agent_chat_with_github_toolkit`
 *  - LIVE-TK-AGENT-2 ← `tests/ui/toolkits/test_agent_with_toolkit_chat.py::
 *    TestAgentWithToolkitInChat::test_agent_with_toolkit_executes_in_chat`
 *
 * The two legacy bodies differ in ONE thing and it is the thing worth keeping
 * apart: the first names the tool in the message, the second asks in plain
 * language and leaves the choice to the model. Both are the AGENT path — the
 * toolkit hangs off a saved agent and the agent is the chat participant —
 * which is what separates them from `toolkits.<provider>.spec.ts`, where the
 * toolkit is the participant directly.
 *
 * Gated on GitHub alone, as both legacy files are. The attach half of this
 * path (agent ↔ toolkit mapping, and its detach) is already proven on EVERY
 * stack by `e2e/journeys/toolkits/*` with a placeholder credential; what only
 * a live lane adds is the call actually reaching GitHub.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  attachToolkitThroughPicker,
  createAgentThroughForm,
  expectStoredAssistantAnswer,
  fillComposer,
  readStoredAssistantAnswer,
} from '../fixtures/api';

import { LIVE_TOOLKIT_PROVIDERS } from './liveEnv';
import {
  provisionLiveToolkit,
  removeLiveToolkit,
  runTag,
  selectedProjectId,
  type ProvisionedLiveToolkit,
} from './liveToolkits';

const provider = LIVE_TOOLKIT_PROVIDERS.github;

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const EVENTS_RE = /\/executions\/(\d+)\/[^/]+\/events/;
const PARTICIPANTS_RE = /\/elitea_core\/participants\/prompt_lib\/(\d+)\/([^/?]+)/;

/**
 * Author an agent, attach the live toolkit to it through the agent page's own
 * picker, and run one turn.
 *
 * Through the PICKER and not a PATCH, for the reason `chat.toolkit.spec.ts`
 * states: the picker deliberately omits `selected_tools` from its request
 * (#248), a presence-sensitive distinction no hand-written body reproduces,
 * and the execution freeze reads exactly that field.
 */
async function runAgentToolkitTurn(
  page: import('@playwright/test').Page,
  prompt: string,
  tag: string,
): Promise<{ projectId: string; conversationId: string; cleanup: () => Promise<void> }> {
  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 60_000 });
  const projectId = await selectedProjectId(page);

  let provisioned: ProvisionedLiveToolkit | undefined;
  let agentId = '';
  const cleanup = async (): Promise<void> => {
    if (agentId !== '') {
      await page.request
        .delete(`${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${agentId}`)
        .catch(() => undefined);
    }
    await removeLiveToolkit(page.request, provisioned);
  };

  provisioned = await provisionLiveToolkit(page.request, projectId, provider, { tag });

  const agentName = `${AUTOTEST_PREFIX}tkagent_${tag}`.slice(0, 32);
  const agent = await createAgentThroughForm(page, agentName);
  agentId = agent.agentId;
  expect(
    agent.projectId,
    'the agent and the toolkit must land in the same project, or the attach addresses nothing',
  ).toBe(projectId);

  await attachToolkitThroughPicker(page, provisioned.toolkitName);

  // Back to the chat surface, and attach the AGENT — the toolkit reaches the
  // turn through it, which is the whole difference from the per-provider file.
  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 60_000 });

  const attached = page.waitForResponse(
    (r) => PARTICIPANTS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await page.getByTestId('plus-menu-button').click();
  const category = page.getByRole('menuitem').filter({ hasText: 'Agents' }).first();
  await expect(category, 'the "+" menu must offer Agents').toBeVisible({ timeout: 15_000 });
  await category.click();
  const row = page.locator(`[data-testid="plus-submenu-item"][data-item-key="agent-${agentId}"]`);
  await expect(row, 'the agent must be listed under Agents').toBeVisible({ timeout: 30_000 });
  await row.click();

  const attachResponse = await attached;
  expect(
    attachResponse.status(),
    `attaching the agent was refused: ${(await attachResponse.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
  const conversationId = PARTICIPANTS_RE.exec(new URL(attachResponse.url()).pathname)?.[2] ?? '';
  expect(conversationId, 'the pick must have created a conversation to attach to').not.toBe('');

  const sendButton = await fillComposer(page, prompt);
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  const streamed = page.waitForResponse((r) => EVENTS_RE.test(r.url()), { timeout: 60_000 });
  await sendButton.click();

  const startResponse = await started;
  expect(
    startResponse.status(),
    `the agent turn was refused: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  expect((await streamed).status(), 'the browser must be able to read the stream').toBe(200);

  return { projectId, conversationId, cleanup };
}

test(`LIVE-TK-AGENT-1: an agent calls its ${provider.displayName} tool mid-chat and reports what the provider returned (legacy test_agent_chat_with_github_toolkit)`, async ({
  page,
}) => {
  test.setTimeout(600_000);

  const tag = `${runTag()}c`;
  let cleanup: (() => Promise<void>) | undefined;
  try {
    const turn = await runAgentToolkitTurn(page, provider.probePrompt, tag);
    cleanup = turn.cleanup;

    const evidence = provider.probeEvidence();
    await expectStoredAssistantAnswer(page, turn.projectId, turn.conversationId, {
      timeout: 240_000,
      contains: evidence,
      message:
        `the agent never reported ${evidence} — the toolkit either did not reach the agent's ` +
        'toolset or the call was refused by the provider',
    });
  } finally {
    if (cleanup !== undefined) await cleanup();
  }
});

test(`LIVE-TK-AGENT-2: an agent's ${provider.displayName} toolkit executes as part of a plain-language chat turn (legacy test_agent_with_toolkit_executes_in_chat)`, async ({
  page,
}) => {
  test.setTimeout(600_000);

  const tag = `${runTag()}d`;
  let cleanup: (() => Promise<void>) | undefined;
  try {
    // The legacy body names no tool: the model has to choose one out of the
    // toolset the agent's execution freeze handed it.
    const turn = await runAgentToolkitTurn(page, provider.chatPrompt, tag);
    cleanup = turn.cleanup;

    await expectStoredAssistantAnswer(page, turn.projectId, turn.conversationId, {
      timeout: 240_000,
      message: 'the agent turn was streamed but never stored',
    });
    const stored = await readStoredAssistantAnswer(page, turn.projectId, turn.conversationId);
    expect(stored.isError, 'the turn must not be stored as a refusal').toBe(false);
    const answer = stored.content.toLowerCase();
    expect(
      provider.answerKeywords.some((keyword) => answer.includes(keyword)),
      `the answer mentions none of ${provider.answerKeywords.join(', ')}: ${stored.content.slice(0, 300)}`,
    ).toBe(true);
  } finally {
    if (cleanup !== undefined) await cleanup();
  }
});
