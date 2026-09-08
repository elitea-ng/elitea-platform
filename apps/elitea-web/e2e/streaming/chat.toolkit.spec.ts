/**
 * A REAL TOOLKIT INVOCATION: an `openapi` toolkit authored in the toolkit
 * form, attached with the agent page's own "+ Toolkit" picker, and then
 * actually CALLED by the model inside a chat turn — with the proof taken
 * server-side rather than off the screen.
 *
 * Everything between the form and the call was unvalidated end to end. Each
 * half has unit coverage and each half is individually correct; what nothing
 * could see is the WIRING, which is where this repository's defects keep
 * landing (the "composition root" class). Four things had to hold at once and
 * only a browser driving the whole path can say whether they do:
 *
 *  1. the toolkit FORM has to store a specification the runtime can parse.
 *     `settings.selected_tools` is populated by the form as a side effect of
 *     parsing the pasted schema, so a toolkit created by POST carries whatever
 *     the caller typed and proves nothing about what a user gets;
 *  2. the PICKER has to write the agent↔toolkit mapping. It sends a PATCH that
 *     deliberately omits `selected_tools` (#248), a presence-sensitive
 *     distinction no hand-written request reproduces;
 *  3. the execution freeze has to hand the worker a toolkit it can
 *     MATERIALIZE. It did NOT, for the toolkit the form produces, and this
 *     journey is what measured it: the `openapi` schema the freeze reads
 *     declares an `openapi_configuration` credential reference, an anonymous
 *     API needs none, and `resolveConfigurationField` froze the absent field
 *     to an explicit JSON `null` — which the native worker's
 *     `merged_auth_settings` refuses, killing the whole TURN with
 *     `native_agent.invalid_configuration` and storing an empty assistant row
 *     flagged `is_error`. The freeze now writes `{}` there, the shape the
 *     Python SDK toolkit has always normalized a missing credential to
 *     (`settings.get('openapi_configuration') or {}`) and the one the native
 *     worker reads as "no auth". The journey therefore authors the toolkit
 *     PURELY in the form. The two-request workaround it used to carry — a
 *     fixture named `attachOpenApiConfiguration`, which created a credential
 *     by API and PUT the reference into the saved toolkit — is deleted, and
 *     deleting it is the point: a spec that hands the runtime a credential no
 *     user of an anonymous API would create cannot fail when that user's
 *     toolkit is the one that breaks;
 *  4. the runtime has to OFFER the operation to the model and DISPATCH the
 *     call the model makes.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE ASSERTIONS ARE SHAPED THIS WAY
 * ─────────────────────────────────────────────────────────────────────────────
 * A stored answer cannot carry this journey on its own. A turn whose toolkit
 * was dropped at assembly still answers, and answers in the same shape — the
 * mock echoes, the row is not flagged `is_error`, the screen looks identical.
 * So the load-bearing assertions are read out of the MOCK's two journals,
 * which are written by the server side of each hop:
 *
 *  - the MODEL journal's `tools` proves the toolkit MATERIALIZED: those are
 *    the function names the runtime put in the request. A toolkit that failed
 *    to materialize contributes none;
 *  - a second journal entry in `call_tool_resumed` mode proves the call was
 *    DISPATCHED and its RESULT fed back: the mock only takes that branch when
 *    the transcript already carries a `tool` message, which nothing but a
 *    real dispatch produces;
 *  - the TOOL journal proves the tool itself RAN. That last hop is asserted
 *    only where the worker can reach the mock — see `MockToolSpec.reachable`
 *    and the note at step 7, which states exactly what the native runtime's
 *    `https_only()` client can and cannot do here.
 *
 * ── WHY THIS RUNS ON BOTH WORKERS, AND WHAT USED TO STOP IT ────────────────
 *
 * This file used to skip the python leg with `native-runtime openapi toolkit
 * shape; the SDK worker admits a different configuration`. That reason was
 * ASSERTED, never measured — the sentence it came from is about the SDK
 * accepting `http://` base URLs and remote specification URLs, which is TRUE
 * (`elitea_sdk/tools/openapi/api_wrapper.py` `_is_absolute_url` admits both
 * schemes where the native worker's `parse_https_base` refuses everything but
 * `https`) and IRRELEVANT here: this journey never authors either shape. It
 * pastes the document the mock serves, at the `https://llm-mock:8090/tool`
 * address the mock advertises, which BOTH workers admit.
 *
 * What actually kept it red on the python leg was the freeze, not the toolkit:
 * a version pinned to a model with no `temperature` key reached the SDK
 * worker, `elitea-sdk` read `data['llm_settings']['temperature']` with a
 * SUBSCRIPT, and the turn died as a bare `builtins.KeyError` in an empty
 * `is_error` row — the same failure for every spec on this page that pins
 * `MOCK_MODEL`. `resolveCurrentAgentModel` now normalizes the family when the
 * key is ABSENT and not only when it conflicts
 * (`services/elitea-main/internal/application/agentexecution/tools.go`, pinned
 * by `TestCurrentApplicationToolSnapshotAlwaysCarriesATemperature`).
 *
 * MEASURED, unmodified, against a python-worker standalone stack: green. The
 * whole contract holds on both runtimes — the form's specification, the
 * picker's mapping, the `{}` credential (`or {}` in the SDK's `get_toolkit` is
 * what the freeze was taught to write), the `operationId`-named functions in
 * the model request, the dispatch, and the tool-action row. So there is one
 * assertion here and it is the same on both legs.
 *
 * The one thing that stays leg-shaped is `MockToolSpec.reachable`, and it is
 * derived from the address the mock advertises rather than from the worker —
 * see step 7. On the default stack that address is `https://`, which neither
 * worker can complete (the mock serves plain HTTP behind it), so both legs
 * assert the same empty tool journal.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  EMPTY_TOOLKIT_GUARDRAILS,
  MOCK_CALL_TOOL_SENTINEL,
  MOCK_TOOL_EFFECTFUL_OPERATION,
  MOCK_TOOL_READ_OPERATION,
  attachToolkitThroughPicker,
  callToolPrompt,
  clearMockLlmJournal,
  clearMockToolJournal,
  createAgentThroughForm,
  createOpenApiToolkitThroughForm,
  expectStoredAssistantAnswer,
  fetchMockToolSpec,
  readMockLlmJournal,
  readMockToolJournal,
  readStoredTranscript,
  setToolkitGuardrails,
} from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/**
 * The model the turn depends on. Pinned onto the agent VERSION for the reason
 * `chat.hitl.spec.ts` states: an application turn runs on
 * `version_details.llm_settings.model_name`, and an empty `llm_settings` falls
 * back to the project catalogue's default, which on a stack that also carries
 * a real provider is not this mock.
 */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

test('an openapi toolkit authored in the form is offered to the model and its call is dispatched', async ({
  page,
}) => {
  // No worker skip: see the header. This journey is green on BOTH legs, and
  // the branch it used to carry named a configuration difference this file
  // never authors.
  // Two model round trips with a tool dispatch between them, on top of two
  // form flows. Every wait below is bounded well under this, so a real hang
  // fails on its own step rather than on the clock.
  test.setTimeout(420_000);

  const suffix = String(Date.now() % 1_000_000);
  const toolkitName = `${AUTOTEST_PREFIX}tk-${suffix}`;
  const agentName = `${AUTOTEST_PREFIX}tkagent-${suffix}`;

  // ── 0. Preconditions ────────────────────────────────────────────────────
  //
  // The guardrail policy is GLOBAL and shared with `chat.toolkit-hitl.spec.ts`,
  // which marks these very operations sensitive. That spec restores it from a
  // hook that runs on failure — but a run killed between the two would leave
  // the entry behind, and this journey would then park on a pause instead of
  // calling anything. Clearing it here makes this spec independent of the
  // other's cleanup rather than of the other's success.
  await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS);
  await clearMockToolJournal(page);
  await clearMockLlmJournal(page);
  const spec = await fetchMockToolSpec(page);

  // ── 1. Author the toolkit through the form ──────────────────────────────
  const { projectId, toolkitId } = await createOpenApiToolkitThroughForm(page, toolkitName, spec.text);

  // What the SERVER stored, not what the form displayed. The specification is
  // the one thing the runtime reads, and a form that saved an empty string
  // would leave the create page looking identical.
  const storedToolkit = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`,
  );
  expect(storedToolkit.ok(), 'the toolkit the form created must be readable').toBe(true);
  const toolkitSettings =
    ((await storedToolkit.json()) as { settings?: { spec?: string; selected_tools?: readonly string[] } })
      .settings ?? {};
  expect(
    toolkitSettings.spec ?? '',
    'the specification must survive the save — the runtime parses this string and nothing else',
  ).toContain(MOCK_TOOL_READ_OPERATION);
  expect(
    toolkitSettings.selected_tools ?? [],
    'both operations must be selected, or the effectful one is invisible to the HITL journey too',
  ).toEqual(expect.arrayContaining([MOCK_TOOL_READ_OPERATION, MOCK_TOOL_EFFECTFUL_OPERATION]));

  // NOTHING IS ATTACHED HERE, and that is the point. This journey used to
  // insert a step between the form and the agent: it created an `openapi`
  // credential by API and PUT a two-key `openapi_configuration` reference into
  // the toolkit the form had just saved, because without it the freeze wrote a
  // JSON `null` there and the whole turn died at toolset materialization. The
  // freeze now writes `{}` for an absent credential (see step 5's note), so a
  // toolkit against an anonymous API is authored ENTIRELY in the form — which
  // is what a user does — and the workaround is gone from the fixtures.

  // ── 2. Author the agent, pin the deterministic model ────────────────────
  const agent = await createAgentThroughForm(page, agentName);
  expect(
    agent.projectId,
    'the agent and the toolkit must land in the same project, or the attach addresses nothing',
  ).toBe(projectId);

  const storedAgent = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agent.agentId}`,
  );
  expect(storedAgent.ok(), 'the agent the form created must be readable').toBe(true);
  const storedMeta =
    ((await storedAgent.json()) as { version_details?: { meta?: Record<string, unknown> } }).version_details
      ?.meta ?? {};
  // `meta` is REPLACED wholesale by `UpdateVersion`, so what the form stored is
  // read back first — a bare `{}` would drop `step_limit`, which the runtime reads.
  const pinned = await page.request.put(
    `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${projectId}/${agent.agentId}/${agent.versionId}`,
    { data: { meta: storedMeta, llm_settings: { model_name: MOCK_MODEL } } },
  );
  expect(
    pinned.status(),
    `the mock model must be pinnable: ${(await pinned.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  // ── 3. Attach the toolkit through the agent page's picker ───────────────
  // No navigation: `createAgentThroughForm` leaves the browser on the agent's
  // edit page, which is where the Tools panel and the Chat button live. A
  // `goto` would have to guess that page's route (`/agents/$tab/$agentId`), and
  // guessing it wrong looks exactly like a missing Tools panel.
  await expect(
    page.getByTestId('agent-toolkits-section'),
    'the save must land on the agent edit page, where the Tools panel lives',
  ).toBeVisible({ timeout: 30_000 });
  await attachToolkitThroughPicker(page, toolkitName);

  // The MAPPING, read off the version the runtime will freeze. The picker's
  // PATCH answered 200 for a route that wrote nothing once already
  // (`chat.agent-tools.spec.ts`, defect 2), so the write is read back rather
  // than inferred from the status.
  const withTools = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agent.agentId}`,
  );
  const frozenTools =
    ((await withTools.json()) as {
      version_details?: { tools?: readonly { id?: number; tool_id?: number; type?: string }[] };
    }).version_details?.tools ?? [];
  // `tool_id`, NOT `id`. On this row `id` is the MAPPING's own key
  // (`entity_tool_mapping.id`) and `tool_id` is the toolkit's — measured, they
  // differ, and matching on `id` reports "the attach was a no-op" about a
  // mapping that is right there.
  const attached = frozenTools.find((tool) => String(tool.tool_id ?? '') === toolkitId);
  expect(
    attached,
    'the picker reported success but the version carries no mapping — the attach was a no-op',
  ).toBeDefined();
  expect(attached?.type, 'the mapped toolkit must be the openapi one').toBe('openapi');

  // ── 4. Chat, and ask for the READ-ONLY operation ────────────────────────
  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversation = (await (await conversationCreated).json()) as { id?: string | number };
  const conversationId = String(conversation.id ?? '');
  expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });

  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 30_000 });
  await input.fill(callToolPrompt(MOCK_TOOL_READ_OPERATION, `read the status ${suffix}`));
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();

  const startResponse = await started;
  expect(
    startResponse.status(),
    `the toolkit turn was refused: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  // ── 5. The turn finishes, quoting what the tool returned ────────────────
  //
  // The mock's continuation script echoes the tool result verbatim, so the
  // stored reply is where the RESULT becomes readable. `expectStoredAssistantAnswer`
  // rules out the `is_error` row a refused turn leaves in the same place — and
  // that is the row a toolkit the worker cannot materialize produces.
  //
  // THIS is the assertion that guards the freeze fix. Before it, a toolkit
  // carrying no `openapi_configuration` — what the form saves whenever its
  // credential picker is left empty, which is the right answer for an API that
  // needs no key — froze that field to `null`, `merged_auth_settings` refused a
  // non-object, and the turn ended here as an empty `is_error` row. Nothing
  // upstream of this point changes when the fix regresses: the form still
  // saves, the picker still maps, the start POST still answers 200.
  // Polled on the mock's END sentinel, not on the operation name: the stored
  // row is readable while it is still being written and the name arrives in
  // its first words, so a poll on the name settles mid-reply and every later
  // assertion then reads a prefix. See `MOCK_CALL_TOOL_SENTINEL`.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 180_000,
    message:
      'no stored answer quoting the tool — either the toolkit was not materialized (the turn ' +
      'is then stored flagged is_error with empty content) or the call was never dispatched',
    contains: MOCK_CALL_TOOL_SENTINEL,
  });

  const answered = await readStoredTranscript(page, projectId, conversationId);
  expect(
    answered.filter((row) => row.role === 'assistant').at(-1)?.content ?? '',
    'the finished reply must quote the result of the operation the model called',
  ).toContain(MOCK_TOOL_READ_OPERATION);

  // ── 6. The server-side proof ────────────────────────────────────────────
  const llmJournal = await readMockLlmJournal(page);
  const offer = llmJournal.find((entry) => entry.mode === 'call_tool');
  expect(
    offer,
    'the mock never took the call_tool branch — the marker did not reach the model, so nothing was scripted',
  ).toBeDefined();
  // THE materialization proof. These are the function names the RUNTIME put in
  // the request; a toolkit dropped at assembly contributes none, and the turn
  // still answers and still looks the same on screen.
  expect(
    offer?.tools ?? [],
    'the toolkit was attached but its operations were never offered to the model — it did not materialize',
  ).toEqual(expect.arrayContaining([MOCK_TOOL_READ_OPERATION, MOCK_TOOL_EFFECTFUL_OPERATION]));
  // THE dispatch proof. The mock takes this branch only when the transcript it
  // receives already holds a `tool` message, which exists only because the
  // runtime executed the call and fed the result back.
  expect(
    llmJournal.map((entry) => entry.mode),
    'no continuation carrying a tool result — the model asked for the call and the runtime never made it',
  ).toContain('call_tool_resumed');

  // THE tool-side proof, asserted where it can be.
  //
  // The native worker's OpenAPI client is `https_only()` and verifies against
  // the public root bundle its image carries, so a `https://llm-mock:…` base
  // URL is dispatched to and then fails IN THE TRANSPORT — the call is real,
  // the request never arrives. Asserting a hit there would assert a stack
  // property nobody has, and asserting nothing would let a silently skipped
  // dispatch pass. So the expectation is derived from the address the mock
  // actually advertises: set `MOCK_LLM_TOOL_BASE_URL` to an `http://` address
  // on a stack whose worker can use one, and this becomes a positive
  // assertion on the same code path.
  const toolJournal = await readMockToolJournal(page);
  expect(
    toolJournal.filter((entry) => entry.method === 'GET').map((entry) => entry.operation),
    spec.reachable
      ? 'the call was dispatched but the tool was never reached'
      : `the tool was reached over ${spec.baseUrl}, which the native worker's https-only client cannot use — ` +
        'something other than this turn called it',
  ).toEqual(spec.reachable ? [MOCK_TOOL_READ_OPERATION] : []);

  // ── 7. The tool-action row the UI renders for that call ─────────────────
  //
  // Addressed by `data-tool-action-name` rather than by text: the tool name
  // also appears in the answer beside it (the mock quotes it), so a text match
  // cannot tell a rendered INVOCATION from a mention of one.
  //
  // OPENED FIRST, and this is a real change of what the assertion claims.
  // The chat parity work (298dd2c7) put the tool rows where production keeps
  // them: inside the turn's `Thought for …` panel, which is forced open while
  // the turn streams and closes the moment it settles. By the time this line
  // runs the turn is settled — §5 above waited on the STORED answer — so the
  // row is mounted and hidden, which is what production shows a reader too.
  // Asserting bare visibility here asserted a state production does not have.
  //
  // What the product owes the reader is DISCOVERABILITY: the panel is the one
  // affordance, and opening it must reveal the call. So the check opens every
  // thinking panel in the transcript the way a reader does and then requires
  // the row. That keeps the original defect in range (no row at all, or a row
  // under a different name, still fails) and adds the one this found: a panel
  // that cannot be opened at all hides the call forever, and reads identically
  // to a missing row from the outside.
  //
  // Polled rather than clicked once: `isStreaming` can still be true for a
  // frame after the stored answer lands, and a click on an already-open panel
  // would CLOSE it.
  const toolRow = page
    .locator(`[data-testid="chat-tool-action"][data-tool-action-name="${MOCK_TOOL_READ_OPERATION}"]`)
    .first();
  const thoughtSummaries = page
    .getByTestId('chat-answer-thought-accordion')
    .getByRole('button', { name: /Thought for/ });
  await expect(
    thoughtSummaries.last(),
    'the turn ran tools but rendered no thinking panel to hold their rows',
  ).toBeVisible({ timeout: 60_000 });
  await expect
    .poll(
      async () => {
        for (const summary of await thoughtSummaries.all()) {
          try {
            if ((await summary.getAttribute('aria-expanded')) !== 'true') {
              await summary.click({ timeout: 5_000 });
            }
          } catch {
            // A re-render between the read and the click detaches the node.
            // The next tick of this poll addresses its replacement, so a
            // detached handle is not a result — it is a retry.
          }
        }
        return toolRow.isVisible();
      },
      {
        timeout: 60_000,
        message:
          'the runtime dispatched the call but the transcript shows no tool-action row for it ' +
          '(the row is missing, or its thinking panel does not open)',
      },
    )
    .toBe(true);

  const transcript = await readStoredTranscript(page, projectId, conversationId);
  expect(
    transcript.filter((row) => row.isError).map((row) => row.content.slice(0, 200)),
    'no row may be flagged is_error — a refused turn is stored as an assistant row and renders like an answer',
  ).toEqual([]);

  // Cleanup is best-effort and deliberately last: a failure above should leave
  // the agent and the toolkit in place for inspection.
  await page.request.delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agent.agentId}`);
  await page.request.delete(`${BASE_URL}/api/v2/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`);
});
