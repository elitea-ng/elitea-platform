/**
 * WHICH MCP SERVER IS ASKING FOR AUTHORIZATION (#939 group 11).
 *
 * ELITEA-2735..2740 are one claim seen from six places: when a remote MCP
 * server demands authorization, the message the agent shows must NAME the
 * toolkit, so a person with several MCP connections knows which one to
 * authorize. The six differ only in where the turn was started (agent page,
 * chat page, a nested agent, a pipeline) and how many connections are
 * attached.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * THE CAPABILITY THIS NEEDED, AND WHY IT IS A SECOND PATH
 * ─────────────────────────────────────────────────────────────────────────
 *
 * `chat.mcp.spec.ts` covers discovery (the worker dials, the catalogue comes
 * back, the agent answers) and a refused dial. Neither reaches an
 * authorization challenge: the mock MCP server answered every request, so
 * `AdkHttpMcpConnector::connect`'s `is_authorization_required()` branch
 * (`toolkits/mcp.rs:244`) was unreachable from this suite.
 *
 * `deploy/mock-mcp/server.py` now serves `/mcp-auth` beside `/mcp`: every
 * request to it is answered `401` with
 *
 *   WWW-Authenticate: Bearer realm="mock-mcp", resource_metadata="https://…"
 *
 * which is the shape the client reports as "authorization required" and
 * `toolkits/mcp.rs::authorization_required` turns into a
 * `DelegatedAuthorizationRequirement` carrying `config.toolkit_name()` — the
 * very field these cases are about.
 *
 * A SECOND PATH rather than a mode switch on `/mcp`: the behaviour is then
 * chosen by the URL a toolkit is configured with, so no global state can leak
 * into a journey running beside this one, and every existing MCP journey is
 * untouched.
 *
 * ── WHAT IT MEASURED ─────────────────────────────────────────────────────
 *
 * ORIGINALLY: the challenge was reached — the mock logs `401 authorization
 * required on /mcp-auth` — and the turn then failed with the GENERIC
 * `{"error":"The runtime operation failed.","is_error":true}`: no prompt, no
 * affordance, no toolkit name. Everything the runtime assembled was discarded
 * before the transcript.
 *
 * FIXED (#982). Two diagnoses were measured and discarded on the way, and both
 * are recorded because each one read as obviously right.
 *
 * WRONG #1: "the challenge aborts ASSEMBLY". It does not. A `401` is answered
 * by `materialize_mcp_toolsets_with_tokens_and_authorization`
 * (`toolkits/mcp.rs`) installing an `McpAuthorizationRequiredTool` placeholder
 * per selected tool and recording the requirement in the
 * `DelegatedAuthorizationCatalog`, so the turn can PAUSE and ask rather than
 * die. Assembly completes on purpose.
 *
 * WRONG #2: "nothing ever dials /mcp-auth" — read off a truncated `podman
 * logs` window in which health checks crowded out the real traffic. Restart
 * mcp-mock to clear its log and the dial is there every run:
 *
 *     mock-mcp "POST /mcp-auth HTTP/1.1" 401 -
 *     mock-mcp 401 authorization required on /mcp-auth
 *
 * WHAT WAS ACTUALLY BROKEN was this case's own setup, in two ways.
 *
 *  1. TWO CONNECTIONS, ONE TOOL NAME. An agent exposes its tools to the model
 *     as one flat list of function names, and mcp-mock published exactly one
 *     tool, so both connections contributed `echo` and the turn died before
 *     any model round trip. Measured control with no `401` anywhere in it: two
 *     PLAIN `/mcp` connections fail identically
 *     (`agent_native_assembly_completed`, then `native agent event stream
 *     failed error_code="native_agent.event_failed"` ~75ms later); one
 *     connection alone completes normally. The mock now publishes `reverse`
 *     beside `echo` so the control connection can be attached without
 *     colliding. The collision itself is a real defect — today it ends the
 *     turn anonymously — and it belongs to its own case, not to this one.
 *  2. NOBODY CALLED THE TOOL. Asking in prose gets prose back; the challenged
 *     placeholder is never reached and the turn ends with an ordinary answer
 *     that names nothing, which reads exactly like the gap this case is about.
 *     The `[[mock:call_tool …]]` marker scripts the call.
 *
 * The current native runtime hides protected operations during discovery.
 * It declares one authorization proxy for each toolkit that needs authorization.
 * This fixture calls the declared proxy and checks the exact stored toolkit card.
 *
 * RUST leg: `toolkits/mcp.rs` is the native runtime's family. The gate inside
 * the test says why the python leg is still skipped, and — since the trust
 * bundle landed — why that reason is no longer the one it used to be.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  clearMockLlmJournal,
  createAgentThroughForm,
  callToolWithArgumentsPrompt,
  createMcpConnection,
  deleteAgent,
  deleteToolkit,
  expectStoredAssistantAnswer,
  fillComposer,
  MOCK_CALL_TOOL_SENTINEL,
  readMockLlmJournal,
  readStoredTranscript,
} from '../fixtures/api';

/** Which worker the stack under test is running. */
const IS_NATIVE_RUNTIME = (process.env['E2E_WORKER'] ?? 'rust') === 'rust';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The authorization-demanding endpoint the mock now serves. */
const AUTH_MCP_URL = 'https://mcp-mock:8443/mcp-auth';

/** The open endpoint, for the connection that must NOT be the one named. */
const OPEN_MCP_URL = 'https://mcp-mock:8443/mcp';

/** The tool the CHALLENGING connection selects — the one the turn will call. */
const MOCK_MCP_TOOL = 'echo';

/**
 * The tool the OPEN connection selects.
 *
 * DIFFERENT ON PURPOSE, and the reason is the whole shape of this case. An
 * agent exposes its tools to the model as one flat list of function names, so
 * two connections that both select `echo` hand it two functions with one name
 * and the turn dies before any model round trip — measured with two PLAIN
 * `/mcp` connections and no `401` anywhere in the run, so it is not this
 * case's subject. `deploy/mock-mcp` publishes `reverse` beside `echo` so the
 * control connection can be genuinely attached without colliding.
 */
const MOCK_MCP_OTHER_TOOL = 'reverse';

async function openAgentChat(page: Page, agentId: string): Promise<string> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversationId = String(((await (await conversationCreated).json()) as { id?: unknown }).id ?? '');
  expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });
  return conversationId;
}

/**
 * Everything the turn left behind, as one string.
 *
 * The authorization requirement can surface as an assistant row, as its
 * metadata, or as a rendered card; joining the stored rows and their metadata
 * is what makes the assertion about the TURN rather than about one particular
 * presentation of it.
 */
async function turnText(page: Page, projectId: string, conversationId: string): Promise<string> {
  const rows = await readStoredTranscript(page, projectId, conversationId);
  const assistant = rows.filter((row) => row.role === 'assistant').at(-1);
  return `${assistant?.content ?? ''}\n${JSON.stringify(assistant?.metadata ?? {})}`;
}

/*
 * onetest: ELITEA-2735 (an MCP that needs authorization names its toolkit in
 * the agent's message) and ELITEA-2736 (with more than one MCP attached, the
 * name is the one that actually challenged). ELITEA-2737/2738/2739/2740 are
 * the same claim from the chat page, a nested agent and a pipeline — the
 * message is produced by `toolkits/mcp.rs` during MATERIALIZATION, before any
 * of those surfaces differ, so they are recorded against this test rather than
 * re-driven four times.
 */
test('an MCP server that demands authorization is named by its toolkit in the agent’s message', async ({ page }) => {
  // STILL NATIVE-ONLY — but for a DIFFERENT reason than the one this gate
  // originally carried, and the swap is the whole point of writing it down.
  //
  // THE OLD REASON (retired). The mcp-mock trust bundle was wired onto the
  // NATIVE worker only, while the python worker's
  // `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE` held the runtime CA alone — and a
  // bundle REPLACES the trust store rather than adding to it, so the python
  // worker could not complete a TLS handshake with mcp-mock at all
  // (`mcp_adapter.py _preflight_auth_check` → `ClientConnectorCertificateError`).
  // `docker-compose.standalone-full.yml`'s `worker-trust` one-shot now builds
  // that worker a runtime-CA + mcp-mock-CA bundle, and the python leg does
  // reach the server: measured on a python-leg stack, mcp-mock logs
  // `POST /mcp-auth HTTP/1.1" 401` and `401 authorization required on
  // /mcp-auth` for this case's own connection. `chat.mcp.spec.ts`'s discovery
  // case is un-gated as a result and passes on both legs.
  //
  // THE REASON IT IS STILL GATED. Reaching the challenge is not answering it.
  // On the python leg the challenged toolkit contributes NO tools — the SDK
  // has no equivalent of the native runtime's `McpAuthorizationRequiredTool`
  // placeholder, so there is nothing for the turn to pause on. MEASURED with
  // the bundle in place: the turn completes with
  // `MOCK: tool echo said Tool 'echo' not available`, `is_error:false`, and
  // names no connection at all. That is the SDK's own contract, not the
  // product gap #982 closed in the native runtime; asserting it here would
  // record a runtime-shaped difference as a platform defect. If the SDK grows
  // the placeholder, delete this skip — the assertions below need no change.
  test.skip(
    !IS_NATIVE_RUNTIME,
    'the SDK leg reaches the 401 but installs no authorization placeholder — see the note above',
  );
  test.setTimeout(420_000);

  const stamp = String(Date.now()).slice(-7);
  const agentName = `${AUTOTEST_PREFIX}mcpauth-${stamp}`;
  // Names that cannot be confused for one another, and cannot appear by
  // accident: the assertion is that the CHALLENGING one is named and the other
  // is not, which a shared prefix would make unreadable in a failure.
  const challengingName = `${AUTOTEST_PREFIX}needsauth${stamp}`;
  const openName = `${AUTOTEST_PREFIX}openmcp${stamp}`;

  const { projectId, agentId } = await createAgentThroughForm(page, agentName);
  // Declared OUTSIDE the try so the cleanup can see whatever was created
  // before a failure, rather than only what a fully successful run made.
  const createdConnectionIds: string[] = [];

  try {
    const challenging = await createMcpConnection(page, projectId, challengingName, {
      url: AUTH_MCP_URL,
      selected_tools: [MOCK_MCP_TOOL],
    });
    createdConnectionIds.push(challenging.id);
    // The second connection is the control for ELITEA-2736: with two attached,
    // the message must name the one that challenged and not simply "an MCP".
    const open = await createMcpConnection(page, projectId, openName, {
      url: OPEN_MCP_URL,
      selected_tools: [MOCK_MCP_OTHER_TOOL],
    });
    createdConnectionIds.push(open.id);

    // ATTACHED, over the same relation write the picker performs. The first
    // draft of this test only READ the agent back and never attached anything,
    // so the turn dialled no MCP at all and the assertion below failed for the
    // one reason it must never fail for.
    const stored = await page.request.get(
      `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
    );
    expect(stored.ok(), 'the agent must be readable before it is wired').toBe(true);
    const versionId = String(
      ((await stored.json()) as { version_details?: { id?: unknown } }).version_details?.id ?? '',
    );
    expect(versionId, 'the agent must carry a version to attach to').not.toBe('');

    for (const connection of [challenging, open]) {
      const attached = await page.request.patch(
        `${BASE_URL}/api/v2/elitea_core/tool/prompt_lib/${projectId}/${connection.id}`,
        {
          data: {
            entity_version_id: Number(versionId),
            entity_id: Number(agentId),
            entity_type: 'agent',
            has_relation: true,
          },
        },
      );
      expect(
        attached.status(),
        `the MCP connection must attach to the agent version: ${(await attached.text()).slice(0, 300)}`,
      ).toBeLessThan(300);
    }

    // Read back: a relation stored against the wrong version leaves the
    // agent's tool list empty behind a 200, and the turn would then dial
    // nothing while this test believed it had wired two servers.
    const wired = await page.request.get(
      `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
    );
    const wiredTools = ((await wired.json()) as { version_details?: { tools?: readonly { name?: string }[] } })
      .version_details?.tools ?? [];
    expect(
      wiredTools.map((tool) => tool.name),
      'both MCP connections must be on the version the turn will run',
    ).toEqual(expect.arrayContaining([challengingName, openName]));

    const conversationId = await openAgentChat(page, agentId);
    let operation = MOCK_MCP_TOOL;
    let argumentsForCall: Record<string, unknown> = { text: 'mcp auth probe' };
    if (IS_NATIVE_RUNTIME) {
      // Protected operations stay hidden. A plain catalogue turn records the
      // model-visible proxy without calling it or pausing discovery.
      await clearMockLlmJournal(page);
      const cataloguePrompt = `mcp catalogue probe ${stamp}`;
      const probe = await fillComposer(page, cataloguePrompt);
      const admitted = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
        timeout: 60_000,
      });
      await probe.click();
      expect((await admitted).status(), 'the catalogue turn must be admitted').toBe(200);
      await expectStoredAssistantAnswer(page, projectId, conversationId, { contains: cataloguePrompt });
      const offered = [...new Set((await readMockLlmJournal(page, projectId))
        .filter((entry) => entry.history.some((message) => message.role === 'user' && message.text === cataloguePrompt))
        .flatMap((entry) => entry.tools))];
      expect(offered, 'the protected operation must remain hidden').not.toContain(MOCK_MCP_TOOL);
      expect(offered, 'the open connection must remain callable').toContain(MOCK_MCP_OTHER_TOOL);
      const proxies = offered.filter((name) => /^mcp_authorize_[0-9a-f]{32}$/.test(name));
      expect(proxies, 'only the challenged connection may declare an authorization proxy').toHaveLength(1);
      operation = proxies[0] ?? '';
      argumentsForCall = {};
    }

    const sendButton = await fillComposer(
      page,
      callToolWithArgumentsPrompt(operation, argumentsForCall, MOCK_CALL_TOOL_SENTINEL),
    );
    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    await sendButton.click();
    expect((await started).status(), 'the turn must be admitted — the refusal comes from the runtime').toBe(200);

    // Require the exact stored authorization card. Ordinary prose naming a
    // toolkit cannot satisfy this pause and authority assertion.
    await expect
      .poll(
        async () => {
          const rows = await readStoredTranscript(page, projectId, conversationId);
          const assistant = rows.filter((row) => row.role === 'assistant').at(-1);
          expect(assistant?.metadata['is_error'], 'the authorization call must not fail').not.toBe(true);
          return assistant?.metadata['authorization_requests'];
        },
        { timeout: 240_000, message: 'the turn never stored its exact MCP authorization card' },
      )
      .toEqual([expect.objectContaining({
        guardrail_type: 'mcp_auth',
        toolkit_name: challengingName,
        toolkit_type: 'mcp',
        tool_name: operation,
        server_url: AUTH_MCP_URL,
      })]);
    await expect(page.getByRole('button', { name: 'Skip Auth', exact: true })).toBeVisible();

    const said = await turnText(page, projectId, conversationId);
    expect(
      said,
      'the authorization message must NAME the toolkit that challenged — a person with several MCP ' +
        'connections cannot act on "an MCP server needs authorization"',
    ).toContain(challengingName);
    // ELITEA-2736: the OTHER connection answered every request, so naming it
    // would send the reader to authorize a server that never asked.
    expect(said, 'the connection that did not challenge must not be named').not.toContain(openName);
  } finally {
    // THE CONNECTIONS TOO, not only the agent.
    //
    // Deleting the agent leaves its two MCP rows in the project, and they are
    // not inert there: every later journey that enumerates the project's
    // toolkits sees them, and one of them points at an endpoint that answers
    // `401` to everything. A run that left them behind accumulated eighteen
    // such rows in the shared project (measured), which is exactly the kind of
    // residue the `autotest_` sweep exists to catch rather than to depend on.
    //
    // After the agent, so the relation rows go with it first, and each guarded
    // on its own: a connection that was never created (an early failure) must
    // not turn this block into a second failure that hides the first.
    await deleteAgent(page.request, agentId).catch(() => undefined);
    for (const toolkitId of createdConnectionIds) {
      await deleteToolkit(page, projectId, toolkitId).catch(() => undefined);
    }
  }
});
