/**
 * THE AGENT `run_code` TOOL ON THE REAL CODE SANDBOX (#2206, #2208).
 *
 * The agent form's "Python sandbox" toggle (stored as `pyodide`) binds one tool,
 * `run_code`, that sends the model's program to the platform's code sandbox: a
 * supervisor on the Docker socket, one digest-pinned runner container per run.
 * PRs #2197 and #2207 built it and proved it against fakes and against the
 * supervisor with real adapter images. Nothing proved the whole chain — Main's
 * dispatch grant, the worker's journal, the supervisor's TLS Postgres receipt,
 * a real runner container — until this journey.
 *
 * WHAT IS SCRIPTED AND WHAT IS NOT. The model is the offline mock: the prompt
 * carries a `[[mock:call_tool run_code {…}]]` marker, which makes the mock
 * answer with a call to `run_code` carrying exactly those arguments. Everything
 * after that is real: the runtime binds the tool only when the deployment's
 * sandbox serves the language, the program runs in a runner container, and the
 * receipt the supervisor stored is what the tool returns. The mock then quotes
 * the tool result verbatim, ending in a sentinel, so the STORED assistant row
 * is the tool result itself. Every assertion below reads that row, never the
 * model's prose.
 *
 * Needs the stack from scripts/code-sandbox-e2e.sh. On the plain chat stack the
 * toggle is disabled and the tool is not bound, which is the contract
 * `api.runtime-capabilities.spec.ts` pins.
 *
 * The marker closes on the `]]` that balances its `[[` and the arguments are
 * JSON, so the programs below contain no double square brackets.
 */
import { expect, test } from '@playwright/test';
import type { Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgentThroughForm,
  deleteAgent as deleteAgentInProject,
  expectStoredAssistantAnswer,
  fillComposer,
  MOCK_CALL_TOOL_SENTINEL,
} from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** Unpinned, a version runs on the project default, which is not the mock on every stack. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/** The module the form stores for "Python sandbox". */
const CODE_SANDBOX_MODULE = 'pyodide';
const RUN_CODE_TOOL = 'run_code';

/**
 * A first run starts a fresh runner container (Python also loads its
 * interpreter, Rust compiles), so the budget is minutes, not seconds.
 */
const TURN_TIMEOUT_MS = 240_000;

/**
 * Every agent this file creates, deleted after each journey whether it passed
 * or not: a journey that fails between the form save and its last assertion
 * would otherwise leave an `autotest_` agent (and its conversation) behind on
 * a stack other journeys share.
 */
const createdAgents: { projectId: string; agentId: string }[] = [];

test.afterEach(async ({ page }) => {
  const agents = createdAgents.splice(0);
  for (const { projectId, agentId } of agents) {
    try {
      await deleteAgentInProject(page.request, agentId, projectId);
    } catch {
      // The delete is hygiene; the journey's own failure is the one to read.
    }
  }
});

interface PreparedAgent {
  readonly projectId: string;
  readonly conversationId: string;
}

/**
 * An agent with the Python sandbox toggle ON, saved the way the form saves it,
 * and a chat opened with it. `meta` is replaced wholesale by the version write,
 * so what the form stored is read back and only `internal_tools` changes.
 */
async function prepareSandboxAgent(page: Page, name: string): Promise<PreparedAgent> {
  const { projectId, agentId, versionId } = await createAgentThroughForm(page, name);
  createdAgents.push({ projectId, agentId });

  const stored = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
  );
  expect(stored.ok(), 'the agent the form created must be readable').toBe(true);
  const storedMeta =
    ((await stored.json()) as { version_details?: { meta?: Record<string, unknown> } }).version_details?.meta ?? {};

  const saved = await page.request.put(
    `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${projectId}/${agentId}/${versionId}`,
    {
      data: {
        meta: { ...storedMeta, internal_tools: [CODE_SANDBOX_MODULE] },
        llm_settings: { model_name: MOCK_MODEL },
      },
    },
  );
  expect(
    saved.status(),
    `the version must accept ${CODE_SANDBOX_MODULE}: ${(await saved.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  const readback = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
  );
  const detail = (await readback.json()) as {
    version_details?: { meta?: { internal_tools?: readonly string[] } };
  };
  expect(
    detail.version_details?.meta?.internal_tools ?? [],
    `${CODE_SANDBOX_MODULE} must survive the save, or the agent has no run_code tool to call`,
  ).toContain(CODE_SANDBOX_MODULE);

  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const created = (await (await conversationCreated).json()) as { id?: string | number };
  const conversationId = String(created.id ?? '');
  expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });
  return { projectId, conversationId };
}

/** Sends one prompt and requires the START to be admitted. */
async function sendTurn(page: Page, prompt: string): Promise<void> {
  const send = await fillComposer(page, prompt);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await send.click();
  const response = await started;
  expect(response.status(), `the turn was refused: ${(await response.text()).slice(0, 300)}`).toBe(200);
}

interface RunCodeResult {
  readonly status?: string;
  readonly stdout?: string;
  readonly output?: string;
  readonly result?: unknown;
  readonly message?: string;
  readonly traceback?: string;
}

/**
 * The stored assistant row's `run_code` result: the JSON the mock quoted
 * between `tool run_code said ` and its sentinel. Fails with the row's text,
 * so a refusal ("no code sandbox for that language", a sandbox failure) reads
 * as itself rather than as a missing field.
 */
async function storedRunCodeResult(
  page: Page,
  prepared: PreparedAgent,
  expectedStatus: 'success' | 'error' = 'success',
): Promise<RunCodeResult> {
  await expectStoredAssistantAnswer(page, prepared.projectId, prepared.conversationId, {
    contains: MOCK_CALL_TOOL_SENTINEL,
    timeout: TURN_TIMEOUT_MS,
    message: 'the run_code turn never stored its tool result',
  });
  const stored = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/messages/prompt_lib/${prepared.projectId}/${prepared.conversationId}`,
  );
  const items =
    ((await stored.json()) as { items?: readonly { role?: string; content?: string }[] }).items ?? [];
  const content = items.find((item) => item.role === 'assistant')?.content ?? '';

  const prefix = `tool ${RUN_CODE_TOOL} said `;
  const from = content.indexOf(prefix);
  expect(from, `the stored answer must quote the ${RUN_CODE_TOOL} result: ${content.slice(0, 600)}`).toBeGreaterThanOrEqual(0);
  const body = content.slice(from + prefix.length, content.lastIndexOf(MOCK_CALL_TOOL_SENTINEL)).trim();
  let parsed: RunCodeResult;
  try {
    parsed = JSON.parse(body) as RunCodeResult;
  } catch {
    throw new Error(`the ${RUN_CODE_TOOL} result is not JSON: ${body.slice(0, 600)}`);
  }
  expect(
    parsed.status,
    `${RUN_CODE_TOOL} must answer \`${expectedStatus}\` on the real sandbox: ${body.slice(0, 800)}`,
  ).toBe(expectedStatus);
  return parsed;
}

/** One journey per language: an agent, a scripted `run_code` call, the stored tool result. */
function journey(
  language: 'python' | 'javascript' | 'typescript' | 'rust',
  code: string,
  assertResult: (result: RunCodeResult) => void,
): void {
  test(`run_code runs ${language} on the real sandbox and returns the program's own output`, async ({ page }) => {
    test.setTimeout(TURN_TIMEOUT_MS + 120_000);
    const prepared = await prepareSandboxAgent(
      page,
      `${AUTOTEST_PREFIX}code-${language}-${Date.now() % 100_000}`,
    );
    const prompt = `[[mock:call_tool ${RUN_CODE_TOOL} ${JSON.stringify({ language, code })}]] compute 1+1 in ${language}`;
    await sendTurn(page, prompt);
    assertResult(await storedRunCodeResult(page, prepared));
  });
}

journey('python', 'print(1+1)', (result) => {
  // The wrapper returns what the program printed: `2`, not the model's words.
  expect((result.stdout ?? '').trim()).toBe('2');
});

journey('javascript', 'console.log(1+1);\nexport default 1+1;', (result) => {
  expect(result.result).toBe(2);
  expect((result.output ?? '').trim()).toBe('2');
});

journey('typescript', 'const n: number = 1+1;\nconsole.log(n);\nexport default n;', (result) => {
  expect(result.result).toBe(2);
  expect((result.output ?? '').trim()).toBe('2');
});

journey(
  'rust',
  'pub fn run(state: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> {\n    println!("{}", 1 + 1);\n    let _ = state;\n    Ok(serde_json::json!(1 + 1))\n}',
  (result) => {
    expect(result.result).toBe(2);
    // `output` is what the program printed and nothing else: cargo's compile
    // log is not the program's output (the lane found it there, #2206).
    expect((result.output ?? '').trim()).toBe('2');
  },
);

/**
 * A Rust program that does not compile. The model must be told WHY: the tool
 * result it receives carries the compiler's diagnostic (the error code and
 * message), not a bare "could not run the program". The path is the runner's
 * captured compiler stderr (`rust_execute.rs`), the supervisor's receipt, the
 * worker's `ProgramFailed` mapping (`graph/code_tool.rs`) and the tool's
 * `failure_text`; before this journey only each hop's unit test covered it.
 */
test('run_code reports a Rust compile error to the model with the compiler diagnostic', async ({ page }) => {
  test.setTimeout(TURN_TIMEOUT_MS + 120_000);
  const prepared = await prepareSandboxAgent(page, `${AUTOTEST_PREFIX}code-rustfail-${Date.now() % 100_000}`);
  const code =
    'pub fn run(state: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> {\n    let _ = state;\n    let n: i32 = "not a number";\n    Ok(serde_json::json!(n))\n}';
  const prompt = `[[mock:call_tool ${RUN_CODE_TOOL} ${JSON.stringify({ language: 'rust', code })}]] compile this rust`;
  await sendTurn(page, prompt);
  const result = await storedRunCodeResult(page, prepared, 'error');
  const message = result.message ?? '';
  expect(message, 'the model must be told the program failed to run').toContain('could not run the program');
  expect(
    message,
    `the compiler's error code must reach the model, not only the generic failure: ${message.slice(0, 800)}`,
  ).toContain('E0308');
  expect(message, 'the compiler message must reach the model').toContain('mismatched types');
});
