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
 * PARTLY FIXED (#982), and the REMAINING half is not the one this header
 * originally named. Recorded in full because two plausible diagnoses were
 * measured and both were wrong.
 *
 * WHAT IS DONE. The runtime now carries the sanitized
 * `DelegatedAuthorizationRequirement` to the transcript on BOTH paths a
 * challenge can take: `NativeAgentAssemblyError::authorization` for a toolkit
 * that fails materialization, and `NativeAgentRuntimeError::delegated_authorization`
 * for one that fails the event stream. Either publishes a `full_message` that
 * NAMES the connection, with the structured block in
 * `response_metadata.mcp_authorization_required`, and settles the turn as an
 * answer rather than as "The runtime operation failed.". Unit-tested in
 * `agents/events_tests.rs`.
 *
 * WHY THIS STILL FAILS, measured on the standalone stack (rust worker, images
 * rebuilt from this branch):
 *
 *   - assembly does NOT fail — the worker logs
 *     `agent_native_assembly_completed`, then `native agent event stream
 *     failed error_code="native_agent.event_failed"` half a second later;
 *   - and that stream error carries no requirement, because the challenge was
 *     never reached: `deploy/mock-mcp`'s log for the whole run shows only
 *     `GET /healthz`. NOTHING EVER DIALLED `/mcp-auth`.
 *
 * So the turn fails for a reason that is not an authorization challenge at
 * all, and the notice has nothing to publish. The open question is why the two
 * attached MCP connections are not materialized for this turn even though the
 * test reads the agent back and asserts both are on the version it runs — an
 * admission/materialization gap, not a message-plumbing one. Until that is
 * answered this case cannot measure what it is about.
 *
 * RUST leg: `toolkits/mcp.rs` is the native runtime's family.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgentThroughForm,
  createMcpConnection,
  deleteAgent,
  fillComposer,
  readStoredTranscript,
} from '../fixtures/api';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The authorization-demanding endpoint the mock now serves. */
const AUTH_MCP_URL = 'https://mcp-mock:8443/mcp-auth';

/** The open endpoint, for the connection that must NOT be the one named. */
const OPEN_MCP_URL = 'https://mcp-mock:8443/mcp';

/** The one tool the mock's catalogue offers. */
const MOCK_MCP_TOOL = 'echo';

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
  test.fail(
    true,
    '#982: the runtime now carries the requirement to the transcript on both the assembly and the event-stream paths, but this turn never dials the MCP server at all — mock-mcp logs only /healthz for the whole run, assembly completes, and the stream then fails for an unrelated reason. The gap left is materialization/admission of the attached MCP connections, not the notice.',
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

  try {
    const challenging = await createMcpConnection(page, projectId, challengingName, {
      url: AUTH_MCP_URL,
      selected_tools: [MOCK_MCP_TOOL],
    });
    // The second connection is the control for ELITEA-2736: with two attached,
    // the message must name the one that challenged and not simply "an MCP".
    const open = await createMcpConnection(page, projectId, openName, {
      url: OPEN_MCP_URL,
      selected_tools: [MOCK_MCP_TOOL],
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
    const sendButton = await fillComposer(page, `Use the ${MOCK_MCP_TOOL} tool please`);
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
  } finally {
    await deleteAgent(page.request, agentId).catch(() => undefined);
  }
});
