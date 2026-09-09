/**
 * The `pyodide` internal tool ("Python sandbox") actually EXECUTES on the
 * Python worker — #872.
 *
 * `chat.agent-tools.spec.ts` already proves the toggle does not brick the
 * turn when it is on. It never drives the mock into actually CALLING the
 * tool, so it cannot tell "the tool ran" from "the tool was silently
 * dropped before the model ever saw it" — both look like an ordinary
 * answered turn from the outside. This journey drives the mock's
 * `[[mock:call_tool …]]` marker at `pyodide_sandbox` with a code snippet
 * whose output only a REAL sandbox run can produce (a value computed at
 * runtime, not a string this file could have typed into the mock's own
 * echo), and reads the stored reply for it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY A DIRECT `[[mock:call_tool pyodide_sandbox …]]` PROMPT AND NOT A REAL
 * MODEL DECIDING TO USE THE TOOL
 * ─────────────────────────────────────────────────────────────────────────────
 * The mock never decides anything — every journey in this directory scripts
 * the exact call it wants made and reads the exact result back, the same
 * discipline `chat.toolkit.spec.ts` and `chat.agent-turns.spec.ts` use for
 * every OTHER real tool call. `pyodide_sandbox` is the SDK's own registered
 * tool name (`elitea_sdk.runtime.tools.sandbox.PyodideSandboxTool.name`),
 * offered to the model the moment `pyodide` survives `serve_internal_tools`
 * on the worker, so naming it in the marker is exactly what a model that
 * chose to use the sandbox would have sent.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS IS PYTHON-WORKER ONLY
 * ─────────────────────────────────────────────────────────────────────────────
 * The Rust worker skips `pyodide` before it ever reaches the model
 * (`services/elitea-worker-rust/src/agents/internal_tools.rs`), so it is
 * never offered as a callable function there — a marker naming it would
 * script a call the runtime never lets the model make. This journey states
 * that with `test.skip`, by name, rather than leaving it to fail unexplained
 * on the `chat-stream-rust` leg (both legs run every file under this
 * directory through the SAME `scripts/chat-stream-e2e.sh` invocation, per
 * `E2E_WORKER`).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY TWO TURNS, NOT A `meta.internal_tools`-PRESET CONVERSATION
 * ─────────────────────────────────────────────────────────────────────────────
 * `pyodide` is a conversation-level toggle (`meta.internal_tools`), not
 * something an agent version or a toolkit attachment gates — but nothing in
 * the product can set that meta before the conversation itself exists.
 * `Create` (`services/elitea-main/internal/api/v2/conversations/handler.go`)
 * accepts a `meta` body, so a conversation CAN be created pre-toggled — but
 * `useChatBoxSend`'s ad-hoc `dummy` (model) participant is provisioned only
 * once, inside `createConversationForSend`, at the exact moment the FIRST
 * send creates the conversation through the composer
 * (`ChatBox.tsx`→`createConversationForSend`→`addParticipants`); the same is
 * true of the `user` participant `ResolveCurrentAdhocTurn` also joins on
 * (`services/elitea-main/internal/db/queries/agent_chat.sql`) — `Create`
 * itself writes no participant row at all
 * (`internal/infra/db/repos/conversations.go`'s `Create`). A conversation
 * made by POSTing straight to `/elitea_core/conversations/...` and then
 * landing on `/app/chat/:id` therefore carries NEITHER participant, and its
 * first send 422s at admission (`ResolveCurrentAdhocTurn` returns zero
 * rows) — the state a real user can never reach, since the product's own
 * "toggle a tool" control (`useChatBoxInternalTools`) refuses to run before
 * a conversation id exists in the first place.
 *
 * So this journey follows the only order the product actually supports:
 * an ordinary FIRST turn creates the conversation through the composer
 * (provisioning both participants as a side effect of that one send), THEN
 * `pyodide` is switched on via the same write the toggle button makes
 * (`PUT /elitea_core/conversation/...` with the updated `meta`,
 * `useChatBoxInternalTools`'s `onInternalToolsConfigChange`), THEN a SECOND
 * send — now on an existing, already-provisioned conversation — carries the
 * scripted call.
 *
 * Lives in `streaming/` because a real turn needs the FULL standalone stack
 * (`chat-stream` project, `scripts/chat-stream-e2e.sh`) — the plain journeys
 * stack has no worker and no model.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  MOCK_CALL_TOOL_SENTINEL,
  callToolWithArgumentsPrompt,
  expectStoredAssistantAnswer,
  fillComposer,
  readCallerPersonalProjectId,
} from '../fixtures/api';

/** `chat-stream-e2e.sh` sets this; the local default matches its own `STANDALONE_WORKER` default. */
const WORKER = process.env['E2E_WORKER'] ?? 'python';

/** The model `seed-llm` seeds into every personal project (see `chat.streaming.spec.ts`). */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/\d+\/[0-9a-f-]+/;

test('a scripted call to pyodide_sandbox runs for real and its output comes back verbatim', async ({ page }) => {
  test.skip(
    WORKER !== 'python',
    'pyodide only executes on the Python worker (#872); the Rust worker skips it before the model ever sees it',
  );
  // Two real turns (conversation create, admission, dispatch, a model call,
  // the stream back — twice) plus a meta write in between.
  test.setTimeout(300_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');

  // The Python SOURCE below names the arithmetic, never its result — the
  // digits in `proof` appear nowhere in the prompt the browser sends, so the
  // mock (which only forwards arguments and quotes back whatever result comes
  // out the other side) has no way to produce this string except by a real
  // interpreter actually multiplying. Distinct per run so a stale stored row
  // cannot pass a rerun.
  const nonce = Date.now() % 1_000_000;
  const proof = `ELITEA_PYODIDE_PROOF_${String(nonce * 2)}`;
  const code = `print(f"ELITEA_PYODIDE_PROOF_{${String(nonce)} * 2}")`;

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  // Ad-hoc turns carry the model in a `dummy` participant that
  // `createConversationForSend` provisions (alongside the `user` one) the
  // moment THIS first send creates the conversation — see the header for why
  // a conversation pre-toggled and pre-created some other way carries
  // neither.
  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

  // ── turn 1: an ordinary send, only to create the conversation for real ──
  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  const firstStarted = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 45_000,
  });
  const firstSend = await fillComposer(page, `autotest pyodide setup ${String(nonce)}`);
  await firstSend.click();

  const createdResponse = await created;
  expect(createdResponse.status(), 'the send must create a real conversation').toBe(201);
  const conversationId = ((await createdResponse.json()) as { id?: string }).id ?? '';
  expect(conversationId).toMatch(/^\d+$/);

  const firstStartResponse = await firstStarted;
  expect(
    firstStartResponse.status(),
    `the setup turn was refused: ${(await firstStartResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 90_000,
    message: 'the setup turn never stored an answer, so the conversation is not ready for the scripted one',
  });

  // ── turn 2: NOW switch `pyodide` on — the same write
  // `useChatBoxInternalTools`'s toggle button makes, on the conversation this
  // journey just proved has real participants ──
  const toggled = await page.request.put(`${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`, {
    data: { meta: { internal_tools: ['pyodide'] } },
  });
  expect(toggled.status(), `pyodide must be toggle-able: ${(await toggled.text()).slice(0, 300)}`).toBeLessThan(300);

  const prompt = callToolWithArgumentsPrompt('pyodide_sandbox', { code }, `run this in the sandbox ${String(nonce)}`);
  const secondStarted = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 45_000,
  });
  const sendButton = await fillComposer(page, prompt);
  await sendButton.click();

  const secondStartResponse = await secondStarted;
  expect(
    secondStartResponse.status(),
    `the scripted turn was refused: ${(await secondStartResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  // The mock quotes the tool's result VERBATIM once the runtime resumes the
  // turn with it (`_script_for`'s `call_tool_resumed` branch, deploy/mock-llm/
  // server.py) — so `proof` reaching the stored reply is `pyodide_sandbox`'s
  // OWN stdout, round-tripped through a real deno/Pyodide subprocess, not
  // anything the mock or this file invented. The sentinel is the same one
  // every other call-tool journey in this directory requires, so a run whose
  // tool call was never dispatched or never resumed fails the same way theirs
  // would.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 90_000,
    contains: proof,
    message:
      `the stored reply never quoted ${proof} back — either pyodide_sandbox was not offered to the ` +
      'model, the call was never dispatched, or it ran and produced something else',
  });
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 5_000,
    contains: MOCK_CALL_TOOL_SENTINEL,
    message: 'the reply quoted the proof but never reached the resumed-call sentinel',
  });

  await page.request.delete(`${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`);
});
