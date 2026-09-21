/**
 * TWO MCP CONNECTIONS, ONE TOOL NAME (#983).
 *
 * An agent exposes its tools to the model as ONE flat list of function names.
 * Two MCP connections that both publish `echo` — and real servers commonly
 * share names such as `search` — therefore hand it two functions with one
 * name. Measured before the fix, with two plain `/mcp` connections and no
 * authorization anywhere in the run: the turn assembles
 * (`agent_native_assembly_completed`) and then dies ~75 ms later with
 * `native_agent.event_failed`, before any model round trip, with NO message
 * naming the collision or either connection. One connection alone completes
 * normally. The refusal comes from ADK's own `resolve_tools` ("duplicate tool
 * name '…': conflict between toolset '…' and toolset '…'"), which runs after
 * assembly has already reported success — which is why nothing the person saw
 * mentioned tools at all.
 *
 * THE POLICY IS NAMESPACING, not first-wins and not refusal. A name only one
 * toolset publishes is untouched (so every stored prompt, journal and
 * transcript is unchanged by the fix existing); a name two publish is exposed
 * once per connection as `<toolkit>__<tool>`, reduced to the characters a
 * provider accepts in a function name; and the run carries ONE notice naming
 * both connections and both renamed tools, seeded the way #866's skipped-tool
 * notice and #973's skipped-child notice are.
 *
 * ── WHAT THIS ASSERTS, AND WHY EACH OBSERVABLE ───────────────────────────────
 *
 *  1. The turn RUNS. Before the fix there was no model request at all, so the
 *     mock's journal holding a request for this project is already the
 *     difference between the defect and the fix.
 *  2. The model was offered BOTH tools, each under its own connection's name,
 *     and NOT the bare colliding name. The journal's `tools` array is the only
 *     server-side, model-independent record of the function list the runtime
 *     put on the wire — the reply cannot show it.
 *  3. BOTH are callable, and each call reaches its own server. The
 *     `[[mock:call_tool …]]` marker scripts the calls by their EXPOSED names,
 *     which is exactly what a model would have to do, and the echoed arguments
 *     come back in the transcript.
 *  4. The notice is in the run. It is seeded as a role-`tool` message before
 *     the model's first turn, so the journal's `history` is where it is
 *     observable — and that is the point of it: the model is the reader that
 *     has to reconcile an instruction saying "call echo" with a function
 *     called `<connection>__echo`.
 *
 * NATIVE RUNTIME ONLY. The naming pass lives in the Rust worker
 * (`services/elitea-worker-rust/src/agents/tool_namespacing.rs`). The python
 * leg builds its tool list inside elitea-sdk, which is cloned at image build
 * time and is not part of this repository, so there is nothing here to change
 * for it and nothing here that would prove it — see the skip's own reason.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  callToolWithArgumentsPrompt,
  clearMockLlmJournal,
  createAgentThroughForm,
  createMcpConnection,
  deleteAgent,
  deleteToolkit,
  fillComposer,
  MOCK_CALL_TOOL_SENTINEL,
  readMockLlmJournal,
  readStoredTranscript,
} from '../fixtures/api';

/** Which worker the stack under test is running. */
const IS_NATIVE_RUNTIME = (process.env['E2E_WORKER'] ?? 'rust') === 'rust';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The open endpoint. BOTH connections point at it — that is the collision. */
const OPEN_MCP_URL = 'https://mcp-mock:8443/mcp';

/** The name `deploy/mock-mcp/server.py` publishes, and both connections select. */
const COLLIDING_TOOL = 'echo';

/**
 * The exposed name the runtime mints, restated.
 *
 * This is the wire contract with `tool_namespacing.rs::function_name_slug` —
 * lower-case, `[a-z0-9_-]` kept, everything else folded to a single `_`, ends
 * trimmed — and it is restated rather than imported because the runtime is a
 * Rust crate in another service's tree. The pairing is ASSERTED rather than
 * trusted: the journal below must carry exactly these two names, so a change
 * to either side fails this test instead of passing it quietly.
 */
function exposedName(toolkitName: string, tool: string): string {
  const slug = toolkitName
    .split('')
    .map((character) => (/[A-Za-z0-9_-]/.test(character) ? character.toLowerCase() : '_'))
    .join('')
    .replace(/_+/g, '_')
    .replace(/^_+|_+$/g, '');
  return `${slug === '' ? 'toolkit' : slug}__${tool}`;
}

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

/*
 * elitea_issues: #983 — two MCP connections publishing the same tool name must
 * not end the turn anonymously; each tool is exposed under its connection and
 * the run says so.
 */
test('two MCP connections that publish the same tool name are both callable, under their own names', async ({
  page,
}) => {
  // The naming pass is the native runtime's. The python leg assembles its tool
  // list inside elitea-sdk (cloned at image build time, not in this
  // repository), so this case has nothing to measure there — a `test.fail`
  // would record an SDK property as a platform gap.
  test.skip(
    !IS_NATIVE_RUNTIME,
    'tool-name namespacing is implemented in the native runtime; the python leg builds its tool list in elitea-sdk',
  );
  test.setTimeout(420_000);

  const stamp = String(Date.now()).slice(-7);
  const agentName = `${AUTOTEST_PREFIX}mcpdup-${stamp}`;
  // Two names that cannot be confused for one another and cannot appear by
  // accident: the assertion is about WHICH connection each exposed name
  // belongs to, which a shared prefix would make unreadable in a failure.
  const firstName = `${AUTOTEST_PREFIX}dupalpha${stamp}`;
  const secondName = `${AUTOTEST_PREFIX}dupbeta${stamp}`;

  const { projectId, agentId } = await createAgentThroughForm(page, agentName);
  const createdConnectionIds: string[] = [];

  try {
    for (const name of [firstName, secondName]) {
      const connection = await createMcpConnection(page, projectId, name, {
        // THE SAME endpoint and THE SAME tool on both, which is the whole
        // scenario: one server published twice is the cheapest faithful stand-in
        // for two servers that happen to share a name.
        url: OPEN_MCP_URL,
        selected_tools: [COLLIDING_TOOL],
      });
      createdConnectionIds.push(connection.id);
    }

    const stored = await page.request.get(
      `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
    );
    expect(stored.ok(), 'the agent must be readable before it is wired').toBe(true);
    const versionId = String(
      ((await stored.json()) as { version_details?: { id?: unknown } }).version_details?.id ?? '',
    );
    expect(versionId, 'the agent must carry a version to attach to').not.toBe('');

    for (const toolkitId of createdConnectionIds) {
      const attached = await page.request.patch(
        `${BASE_URL}/api/v2/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`,
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

    // Read back: a relation stored against the wrong version leaves the tool
    // list empty behind a 200, and the turn would then dial ONE server — which
    // is the case that already worked, so the test would pass while measuring
    // nothing.
    const wired = await page.request.get(
      `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
    );
    const wiredTools = ((await wired.json()) as { version_details?: { tools?: readonly { name?: string }[] } })
      .version_details?.tools ?? [];
    expect(
      wiredTools.map((tool) => tool.name),
      'BOTH MCP connections must be on the version the turn will run',
    ).toEqual(expect.arrayContaining([firstName, secondName]));

    const firstExposed = exposedName(firstName, COLLIDING_TOOL);
    const secondExposed = exposedName(secondName, COLLIDING_TOOL);

    const conversationId = await openAgentChat(page, agentId);
    // Cleared AFTER the conversation exists and before the turn: the journal is
    // shared with whatever else this stack served, and the assertions below are
    // about the request THIS turn made.
    await clearMockLlmJournal(page);

    // ONE turn, TWO calls — one per connection, each by its EXPOSED name, which
    // is exactly what the model would have to emit. A turn that could only call
    // one of them would not distinguish namespacing from first-wins.
    const sendButton = await fillComposer(
      page,
      `${callToolWithArgumentsPrompt(firstExposed, { text: 'alpha-probe' }, '')} ` +
        callToolWithArgumentsPrompt(secondExposed, { text: 'beta-probe' }, MOCK_CALL_TOOL_SENTINEL),
    );
    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    await sendButton.click();
    expect((await started).status(), 'the turn must be admitted').toBe(200);

    // SETTLED, not merely non-empty. `is_error` is written ONLY by a terminal
    // projection, so its PRESENCE is the marker that the run has landed; an
    // absent key means "still running" and must never be read as "finished".
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

    const rows = await readStoredTranscript(page, projectId, conversationId);
    const assistant = rows.find((row) => row.role === 'assistant');
    expect(
      assistant?.metadata['is_error'],
      'the collision must not end the turn — before #983 it died as an anonymous runtime failure',
    ).toBe(false);

    // ── The function list the runtime actually put on the wire ───────────────
    const journal = await readMockLlmJournal(page, projectId);
    const offered = journal.flatMap((entry) => entry.tools);
    expect(
      offered,
      'each connection must contribute its own function name',
    ).toEqual(expect.arrayContaining([firstExposed, secondExposed]));
    expect(
      offered,
      'the colliding name itself must not be offered — two functions with one name is the defect',
    ).not.toContain(COLLIDING_TOOL);

    // ── Both tools answered, each with its own arguments ─────────────────────
    const said = rows.map((row) => `${row.content}\n${JSON.stringify(row.metadata)}`).join('\n---\n');
    expect(said, 'the first connection’s tool must have run').toContain('alpha-probe');
    expect(said, 'the second connection’s tool must have run').toContain('beta-probe');

    // ── The notice, which is what a reader (and the model) gets instead of a
    // dead turn ──────────────────────────────────────────────────────────────
    const history = journal.flatMap((entry) => entry.history.map((message) => message.text)).join('\n');
    expect(
      history,
      'the run must carry ONE notice naming the colliding tool, both connections and both new names',
    ).toContain(`tool '${COLLIDING_TOOL}' is published by more than one connection`);
    for (const [connection, exposed] of [
      [firstName, firstExposed],
      [secondName, secondExposed],
    ] as const) {
      expect(history, `the notice must name '${connection}' and its new tool name`).toContain(
        `'${exposed}' for '${connection}'`,
      );
    }
  } finally {
    // THE CONNECTIONS TOO, not only the agent: MCP rows left in the shared
    // project are seen by every later journey that enumerates its toolkits.
    await deleteAgent(page.request, agentId).catch(() => undefined);
    for (const toolkitId of createdConnectionIds) {
      await deleteToolkit(page, projectId, toolkitId).catch(() => undefined);
    }
  }
});
