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
 * WHY A BARE-MODEL CONVERSATION, ADDRESSED THROUGH THE COMPOSER
 * ─────────────────────────────────────────────────────────────────────────────
 * `pyodide` is a conversation-level toggle (`meta.internal_tools`), not
 * something an agent version or a toolkit attachment gates — the shortest
 * path to it is a bare ad-hoc chat, exactly the shape `chat.streaming.spec.ts`
 * drives. The conversation is created with `meta.internal_tools` already set
 * (the `Create` handler accepts `meta` directly,
 * `services/elitea-main/internal/api/v2/conversations/handler.go`), so there
 * is no race against the composer's own first-send participant
 * provisioning — the model is only picked, and the participants
 * `useChatBoxSend` adds on send read the conversation's meta as it already
 * stands.
 *
 * Lives in `streaming/` because a real turn needs the FULL standalone stack
 * (`chat-stream` project, `scripts/chat-stream-e2e.sh`) — the plain journeys
 * stack has no worker and no model.
 */
import { expect, test } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
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

/**
 * Create a bare conversation with `meta.internal_tools` already set.
 *
 * `Create` reads `body.meta` straight onto the row
 * (`services/elitea-main/internal/api/v2/conversations/handler.go`), so the
 * toggle is a fact about the conversation before the composer ever loads it
 * — no separate PUT, and nothing to race against the first send's own
 * participant provisioning.
 */
async function createConversationWithInternalTools(
  request: APIRequestContext,
  projectId: string,
  name: string,
  internalTools: readonly string[],
): Promise<string> {
  const created = await request.post(`${API_BASE}/elitea_core/conversations/prompt_lib/${projectId}`, {
    data: { name, meta: { internal_tools: internalTools } },
  });
  expect(created.status(), `the conversation must be created: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const body = (await created.json()) as { id?: unknown };
  const id = String(body.id ?? '');
  expect(id, 'the conversation route addresses by id').not.toBe('');
  return id;
}

test('a scripted call to pyodide_sandbox runs for real and its output comes back verbatim', async ({ page }) => {
  test.skip(
    WORKER !== 'python',
    'pyodide only executes on the Python worker (#872); the Rust worker skips it before the model ever sees it',
  );
  test.setTimeout(180_000);

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

  const conversationId = await createConversationWithInternalTools(
    page.request,
    projectId,
    `${AUTOTEST_PREFIX}pyodide-${String(nonce)}`,
    ['pyodide'],
  );

  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);

  // Ad-hoc turns carry the model in a `dummy` participant that `useChatBoxSend`
  // provisions on first send from whatever the picker currently holds — an
  // empty selection is refused before the request leaves the browser (see
  // `chat.streaming.spec.ts`).
  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

  const prompt = callToolWithArgumentsPrompt('pyodide_sandbox', { code }, `run this in the sandbox ${String(nonce)}`);
  const sendButton = await fillComposer(page, prompt);
  await sendButton.click();

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
