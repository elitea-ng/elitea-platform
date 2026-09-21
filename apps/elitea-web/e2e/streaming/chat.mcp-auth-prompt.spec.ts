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
 * With both corrected the turn reaches the placeholder, ADK raises the tool
 * confirmation that `require_tool_confirmation` asked for, `session.rs` stamps
 * the requirement onto it, and the projector renders an authorization prompt
 * that NAMES the challenging connection and not the other one.
 *
 * RUST leg: `toolkits/mcp.rs` is the native runtime's family.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgentThroughForm,
  callToolWithArgumentsPrompt,
  createMcpConnection,
  deleteAgent,
  fillComposer,
  MOCK_CALL_TOOL_SENTINEL,
  readStoredTranscript,
} from '../fixtures/api';

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
  return rows.map((row) => `${row.content}\n${JSON.stringify(row.metadata)}`).join('\n---\n');
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
  test.setTimeout(420_000);

  const stamp = String(Date.now()).slice(-7);
  const agentName = `${AUTOTEST_PREFIX}mcpauth-${stamp}`;
  // Names that cannot be confused for one another, and cannot appear by
  // accident: the assertion is that the CHALLENGING one is named and the other
  // is not, which a shared prefix would make unreadable in a failure.
  const challengingName = `${AUTOTEST_PREFIX}needsauth${stamp}`;
  const openName = `${AUTOTEST_PREFIX}openmcp${stamp}`;

  const { projectId, agentId } = await createAgentThroughForm(page, agentName);

  try {
    const challenging = await createMcpConnection(page, projectId, challengingName, {
      url: AUTH_MCP_URL,
      selected_tools: [MOCK_MCP_TOOL],
    });
    // The second connection is the control for ELITEA-2736: with two attached,
    // the message must name the one that challenged and not simply "an MCP".
    const open = await createMcpConnection(page, projectId, openName, {
      url: OPEN_MCP_URL,
      selected_tools: [MOCK_MCP_OTHER_TOOL],
    });

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
    // THE MODEL MUST ACTUALLY CALL THE TOOL. Asking in prose does not do it:
    // the mock answers prose with prose, the challenged tool is never reached,
    // and the turn then ends with an ordinary answer that names nothing —
    // which reads exactly like the gap this case is about. The marker scripts
    // the call, and `echo` is the CHALLENGING connection's selection, so the
    // call lands on its authorization placeholder.
    const sendButton = await fillComposer(
      page,
      callToolWithArgumentsPrompt(MOCK_MCP_TOOL, { text: 'mcp auth probe' }, MOCK_CALL_TOOL_SENTINEL),
    );
    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    await sendButton.click();
    expect((await started).status(), 'the turn must be admitted — the refusal comes from the runtime').toBe(200);

    // SETTLED, not merely non-empty. The first draft polled the transcript's
    // total length, which the USER row satisfies the instant the turn starts —
    // so it read the answer 3 seconds in and reported a gap that might only
    // have been a race. `is_error` is written ONLY by a terminal projection, so
    // its PRESENCE is the marker that the run has landed; an absent key means
    // "still running" and must not be read as "finished with nothing".
    await expect
      .poll(
        async () => {
          const rows = await readStoredTranscript(page, projectId, conversationId);
          const assistant = rows.find((row) => row.role === 'assistant');
          if (assistant === undefined) return 'no assistant row';
          return 'is_error' in assistant.metadata ? 'settled' : 'running';
        },
        { timeout: 240_000, message: 'the turn never settled' },
      )
      .toBe('settled');

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
    await deleteAgent(page.request, agentId).catch(() => undefined);
  }
});
