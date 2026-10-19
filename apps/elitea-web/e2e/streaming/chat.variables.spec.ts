/**
 * An agent whose instructions reference a variable must reach the model with
 * that variable SUBSTITUTED — and this journey reads the substituted prompt
 * back off the wire rather than inferring it from an answer.
 *
 * ── WHAT THIS SPEC USED TO SAY, AND WHY IT CHANGED ─────────────────────────
 *
 * Until the native runtime grew variable substitution this file pinned the
 * opposite: `validate_application_meta_variables` admitted `meta.variables`
 * only when it was absent, an empty array or an empty object, and refused a
 * POPULATED list as `UnsupportedCapability` — an honest refusal while nothing
 * substituted them, and one this spec existed to make fail loudly the day it
 * stopped being true. That day arrived. `services/elitea-worker-rust/src/
 * agents/variables.rs` now renders the instructions before the profile is
 * built, and `assembly.rs`'s `validate_application_meta_variables` admits the
 * populated shapes the renderer consumes. The refusal shape this file used to
 * demonstrate is still pinned elsewhere — `chat.attachments.spec.ts` (an
 * attached file) and `chat.mcp.spec.ts` (an `http` MCP endpoint).
 *
 * ── THE CONTRACT, TRACED TO THE SDK IT PORTS ───────────────────────────────
 *
 * The renderer is a port of the (retired) Python worker's SDK behaviour,
 * measured at the revision that worker pinned (elitea-sdk 0.9.8 @ b5113a12): `runtime/langchain/assistant.py:557-576`
 * captures `{name, value}` rows whose value is neither null nor empty, and
 * `:597-657` renders the instructions through
 * `SandboxedEnvironment(undefined=DebugUndefined)` — real Jinja2, not string
 * replacement, so an UNDEFINED name survives as the literal `{{ name }}` and a
 * malformed template reaches the model unrendered instead of ending the turn.
 * The Rust port reproduces all three (its unit tests pin the undefined,
 * malformed and fast-path cases); this journey pins the one thing a unit test
 * cannot: that the value the USER stored through the API reaches the MODEL.
 *
 * ── WHY THE ASSERTION READS THE MOCK'S JOURNAL, NOT THE ANSWER ─────────────
 *
 * Substitution rewrites the SYSTEM prompt and nothing else. The offline mock
 * answers with an echo of the last USER message (`deploy/mock-llm/server.py`,
 * `_reply_for`), so an assertion on the stored answer would read back the
 * question — identical whether `{{topic}}` was substituted, left literal, or
 * dropped. It would pass against every one of those, which is to say it would
 * assert nothing. The mock's MODEL journal records each request's system
 * prompt for exactly this reason (`_system_text`, surfaced as
 * `MockLlmJournalEntry.instructions`), and that is the only server-side,
 * model-independent evidence of what the runtime assembled.
 *
 * ── WHY THE MODEL IS PINNED ON THE VERSION ─────────────────────────────────
 *
 * Same reason `chat.hitl.spec.ts` pins it: an application turn runs on
 * `version_details.llm_settings.model_name`, and an empty `llm_settings` falls
 * back to whatever the project catalogue calls its default — which on a stack
 * that also carries a real provider is not the mock, and then the journal this
 * spec reads would hold no entry for the turn at all.
 *
 * ── WHY THIS SPEC SETS THE VARIABLE THROUGH THE API, NOT THE FORM ──────────
 *
 * Unchanged from the original, and still measured: there is no form control
 * that AUTHORS a new variable NAME in this app's ported manual edit page.
 * `ApplicationVariables`/`AgentVariables`
 * (`src/features/agents/ui/ApplicationVariables.tsx`, `AgentVariables.tsx`)
 * both render existing rows only — each returns `null` outright when its
 * `variables` prop is empty, so there is no "add variable" affordance to
 * click. The one place this codebase DOES derive a variable NAME from
 * `{{name}}` placeholders in the instructions text — porting the legacy app's
 * `contextResolver`/`extractPlaceholders` (`src/shared/lib/string.ts`) — is
 * wired into exactly one flow, the AI-generate-agent draft approval
 * (`src/features/agents/ui/generate-agent-modal/useAgentDraftApproval.ts:151`).
 * The manual `CreateAgentForm`/`InstructionsInput.tsx` path this suite's own
 * `createAgentThroughForm` drives has no such wiring, so typing `{{topic}}`
 * into the instructions box does nothing to `version_details.variables`. This
 * journey therefore reaches for the same door the form's own save uses:
 * `PUT .../version/prompt_lib/{project}/{application}/{version}` with a
 * `variables` array, exactly the `VersionWriteRequest.variables:
 * VersionVariable[]` shape `useSaveVersion.ts` sends (and which
 * `services/elitea-main/internal/api/v2/applications/handler.go:915-933`
 * folds into `application_versions.meta` — the identical column the runtime
 * reads). If a variables-authoring control is ever added to this edit page,
 * this spec should switch to driving it.
 *
 * ── WHERE THIS LIVES ───────────────────────────────────────────────────────
 *
 * `streaming/`, matched by the `chat-stream` project (`playwright.config.ts`),
 * because an agent turn needs the full standalone stack's runtime plane and a
 * real worker — see `chat.agent.spec.ts`'s header for why the plain
 * `journeys/` stack cannot run this at all.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  clearMockLlmJournal,
  createAgentThroughForm,
  expectStoredAssistantAnswer,
  readMockLlmJournal,
} from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const CONVERSATION_CREATED_RE = /\/elitea_core\/conversations\/prompt_lib\/\d+$/;

/** See the header: without this the turn may not reach the mock at all. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

test('an agent variable is substituted into the instructions the model receives', async ({ page }) => {
  test.setTimeout(180_000);

  const name = `${AUTOTEST_PREFIX}vars-${Date.now() % 1_000_000}`;
  const variableName = 'topic';
  // Unique per run, and shaped so it can only have arrived by substitution:
  // it appears nowhere in the instructions, the question, or any fixture.
  const variableValue = `${AUTOTEST_PREFIX}topic-${Date.now() % 1_000_000}`;
  // Deliberately UNSPACED. Jinja2 prints an undefined name back in its own
  // canonical spacing, so a `{{ topic }}` in the journal would mean the
  // template ran but the VALUE never reached it — a different failure from
  // `{{topic}}`, which means nothing rendered at all.
  const instructions = `Answer only about {{${variableName}}}.`;

  // ── 1. Author the agent through the form ────────────────────────────────
  const { projectId, agentId, versionId } = await createAgentThroughForm(page, name);

  // ── 2. Give the version the variable, the instructions that use it, and ──
  //      the deterministic model — through the version PUT, the same door the
  //      form's own save uses (see the header for why the form has no
  //      add-variable control to click).
  //
  //      `meta` is deliberately ABSENT from this body: `UpdateVersion`
  //      REPLACES meta wholesale when the key is present, and folds
  //      `variables` into the STORED meta when it is not
  //      (`applications/handler.go:903-933`) — so sending `variables` alone
  //      adds the variable without dropping `step_limit`, which the runtime
  //      reads.
  const putResponse = await page.request.put(
    `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${projectId}/${agentId}/${versionId}`,
    {
      data: {
        variables: [{ name: variableName, value: variableValue }],
        instructions,
        // `openai_compatible` and `model_project_id` are deliberately absent:
        // elitea-main owns both and derives them from the project's catalogue
        // row, overwriting whatever a version carries.
        llm_settings: { model_name: MOCK_MODEL },
      },
    },
  );
  expect(
    putResponse.status(),
    `the version must accept a variable: ${(await putResponse.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  // What the server STORED, not what this test sent — the same discipline
  // `createAgentThroughForm` uses for `internal_tools`. A save that accepted
  // the variable and discarded it would otherwise look exactly like a runtime
  // that failed to substitute it.
  const readback = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
  );
  expect(readback.ok(), 'the agent must be readable after the save').toBe(true);
  const stored = (await readback.json()) as {
    version_details?: {
      instructions?: string;
      variables?: readonly { name?: string; value?: string }[];
      llm_settings?: { model_name?: string };
    };
  };
  expect(
    stored.version_details?.variables ?? [],
    'the variable must round-trip through the write, or nothing below is about variables at all',
  ).toEqual([{ name: variableName, value: variableValue }]);
  expect(
    stored.version_details?.instructions,
    'the instructions must reference the variable, or there is nothing to substitute',
  ).toBe(instructions);
  expect(
    stored.version_details?.llm_settings?.model_name,
    'the mock model must be pinned on the version, or the turn never reaches the journal this spec reads',
  ).toBe(MOCK_MODEL);

  // ── 3. Open chat with this agent from its own edit page ────────────────
  const conversationCreated = page.waitForResponse(
    (r) => CONVERSATION_CREATED_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversation = (await (await conversationCreated).json()) as { id?: string | number };
  const conversationId = String(conversation.id ?? '');
  expect(conversationId, 'the Chat button must create a conversation to attach to').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}$`), { timeout: 30_000 });

  // Bound the journal window to THIS turn: the stack is shared, and an
  // earlier journey's request carrying no instructions would otherwise be a
  // candidate for the assertion below.
  await clearMockLlmJournal(page);

  // ── 4. Send, and require the turn to be ADMITTED ────────────────────────
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.fill(`autotest variables ${Date.now()}`);
  await page.getByTestId('chat-send-button').click();

  const startResponse = await started;
  expect(
    startResponse.status(),
    `a populated variable must not be refused at admission: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  // ── 5. …and to ANSWER ───────────────────────────────────────────────────
  // The row must NOT be flagged `metadata.is_error`. This is the assertion
  // that was inverted when the runtime grew substitution: a refusal here means
  // an agent with a variable cannot hold a conversation at all.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 90_000,
    message:
      'an agent carrying a populated variable did not answer — either the runtime refuses ' +
      'the populated shape again (assembly.rs `validate_application_meta_variables`) or the ' +
      'renderer (variables.rs) turned a valid template into a failure',
  });

  // ── 6. …and prove the VALUE reached the model ──────────────────────────
  const journal = await readMockLlmJournal(page);
  const chatRequests = journal.filter((entry) => entry.path === '/v1/chat/completions');
  expect(
    chatRequests.length,
    'the turn answered but the mock recorded no chat request — the answer came from ' +
      'somewhere this assertion cannot see, so the model is not pinned to the mock',
  ).toBeGreaterThan(0);

  const prompts = chatRequests.map((entry) => entry.instructions);
  expect(
    prompts.some((prompt) => prompt.includes(variableValue)),
    `no request carried the substituted value. System prompts seen: ${JSON.stringify(prompts)}`,
  ).toBe(true);
  // Both spellings, because they fail differently: the source spelling means
  // nothing rendered, the canonical one means the template ran with the
  // variable undefined (`DebugUndefined`). Either is a substitution that did
  // not happen, and an assertion on the value alone would miss a prompt that
  // somehow carried both.
  for (const prompt of prompts) {
    expect(
      prompt,
      'a request reached the model with the placeholder still in it',
    ).not.toContain(`{{${variableName}}}`);
    expect(
      prompt,
      'a request reached the model with the variable UNDEFINED — the template rendered ' +
        'but the version list never reached the context',
    ).not.toContain(`{{ ${variableName} }}`);
  }

  await page.request.delete(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
  );
});
