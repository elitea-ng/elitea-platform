/**
 * The MCP leg of an agent turn: a connection attached through the agent
 * editor's own "+ MCP" picker, materialized by the native runtime against a
 * REAL remote MCP server, and answered.
 *
 * MCP is the one toolkit family in this runtime that reaches a third-party
 * process over the network before the agent can say anything.
 * `services/elitea-worker-rust/src/toolkits/mcp.rs` opens a Streamable HTTP
 * session against the connection's `settings.url`
 * (`AdkHttpMcpConnector::connect`, :215-248) and asks ADK to discover the
 * server's catalogue; every other family is materialized from stored settings
 * alone. That is why this journey needs a server and not a fixture: without
 * one on the other end, a runtime that speaks MCP and a runtime that refuses
 * MCP produce the SAME failed turn, and a spec written against that could not
 * tell them apart. `deploy/mock-mcp/` is that server — see its header for the
 * wire shape, and `deploy/docker-compose.standalone-full.yml` (`mcp-mock`,
 * `mcp-mock-trust`) for how it is wired in.
 *
 * ── WHAT THE RUNTIME ACTUALLY DOES, TRACED TO SOURCE ───────────────────────
 *
 * A stored toolkit row whose `type` is `mcp` (or `mcp_config`, or anything
 * `mcp_`-prefixed) is classified `FrozenToolKind::Mcp` by
 * `snapshot.rs:516`/`:327-353`, which puts it on a DIFFERENT path from every
 * other toolkit: `materialize_configured_toolsets_*` skips it, and
 * `materialize_mcp_toolsets_with_tokens_and_authorization` (`mcp.rs:284`,
 * called from `agents/ordinary.rs:405`, `agents/pipeline.rs:424,648` and
 * `agents/application_tools.rs:1211`) picks it up instead.
 *
 * `RemoteMcpConfig::parse` (`mcp.rs:125-158`) then applies rules that have no
 * counterpart on the platform side:
 *
 *   * `tool_type()` must be exactly `mcp` — a saved `mcp_config` definition
 *     has a different authority owner and fails closed (`mcp.rs:129-131`);
 *   * `settings.url` must be `https`, with no userinfo, no query and no
 *     fragment (`parse_endpoint`, `mcp.rs:592-609`);
 *   * static `headers`, `client_id`, `client_secret` and `scopes` are refused
 *     outright (`reject_unowned_auth`, `mcp.rs:576-590`) — a continuation
 *     token is accepted ONLY from Main's claim-fetched token map;
 *   * `ssl_verify: false` is refused (`mcp.rs:146-153`). There is no way to
 *     turn verification off, which is why the mock serves real TLS and the
 *     worker is handed a trust bundle rather than a bypass.
 *
 * Every failure on that path is FATAL to the assembly — `mcp_materialization_error`
 * (`agents/ordinary.rs:471-490`) turns it into a `NativeAgentAssemblyError` and
 * the turn dies. MCP references are never silently dropped, which is what the
 * second test below exists to keep true.
 *
 * ── WHY THE MCP CONNECTION IS CREATED THROUGH THE API ──────────────────────
 *
 * Not for convenience: there is no form to drive. `/app/mcps/create` builds
 * its type tiles from the project's toolkit-type catalogue filtered to the
 * mcp-flavoured entries (`useGetCurrentMCPSchemas.hooks.ts:54-56`), and this
 * stack's catalogue publishes none — `GET /elitea_core/toolkits/prompt_lib/
 * {project}` answers `application, artifact, custom, database, datasource,
 * github, jira, openapi`. The page renders its "Still no local MCP available"
 * empty state, the one `e2e/visual/routes.visual.spec.ts:369-374` snapshots.
 * `createMcpConnection` (`e2e/fixtures/api.ts`) therefore reaches for the same
 * door the form's own save would use, exactly as `chat.variables.spec.ts` does
 * for a variable name no control can author, and as the existing MCP journeys
 * already do inline (`e2e/journeys/mcps/mcps.oauth.spec.ts:229-232`).
 *
 * The ATTACH, by contrast, IS driven through the UI, because that control
 * exists and nothing exercised it: `ToolMenu.tsx`'s "+ MCP" section had no
 * testid until this spec, and the J18 journeys cover the MCPs list and detail
 * pages only — no spec had ever attached an MCP to an agent, let alone taken a
 * turn with one.
 *
 * ── WHY ONE TEST IS PINNED TO THE NATIVE LEG AND THE OTHER IS NOT ──────────
 *
 * `E2E_WORKER` comes from `scripts/chat-stream-e2e.sh`, the one place that
 * knows which worker the stack runs. The two assertions below used to share a
 * gate; they no longer do, because only one of the two reasons was ever about
 * the stack rather than about the product:
 *
 *   * DISCOVERY (the first test) ran on the native leg alone because the trust
 *     bundle that makes `mcp-mock`'s certificate verifiable was wired onto the
 *     RUST worker only, while the Python worker's
 *     `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE` held the runtime CA and nothing
 *     else — so the mock was untrusted there and the test would have failed
 *     for a DEPLOYMENT reason. That is now fixed at the deployment: the
 *     `worker-trust` one-shot in `docker-compose.standalone-full.yml` builds
 *     the python worker a runtime-CA + mcp-mock-CA bundle (no public roots,
 *     deliberately), so BOTH legs can dial the mock and this test runs on
 *     both. MCP discovery is a product capability of each runtime, not a
 *     native-only contract, and it should be asserted on whichever one the
 *     stack is running.
 *   * THE ENDPOINT RULE (the second test) stays pinned, and no bundle changes
 *     it: the SDK leg reaches MCP servers with pylon's rules, which accept
 *     shapes `RemoteMcpConfig::parse` refuses — `http` endpoints and static
 *     headers among them — so that test pins a disagreement, not a shared
 *     contract. Skipping is still the honest answer there; asserting the SDK's
 *     own outcome for that leg would be a second test, the way
 *     `chat.variables.spec.ts` asserts a per-leg contract.
 *
 * ── WHERE THIS LIVES ───────────────────────────────────────────────────────
 *
 * `streaming/`, matched by the `chat-stream` project (`playwright.config.ts`),
 * because an agent turn needs the full standalone stack's runtime plane, a
 * worker and a model backend — see `chat.agent.spec.ts`'s header for why the
 * plain `journeys/` stack cannot run this at all. It additionally needs the
 * `mcp-mock`/`mcp-mock-trust` services and the worker's `SSL_CERT_FILE`. All
 * three live in the compose files themselves rather than in an environment
 * variable, deliberately: `GATEWAY_EGRESS_ALLOWLIST` has to be repeated on
 * EVERY `compose up` or the mock LLM stops being reachable, and a second
 * variable with the same hazard would be one more way to bring the stack up
 * in a shape that fails this spec for the wrong reason. Nothing needs to be
 * added to that allowlist for MCP: it is read only by the LLM gateway
 * (`services/elitea-llm-gateway/internal/config/config.go:349`) and governs a
 * tenant credential's `api_base`; the worker's MCP egress has no allowlist at
 * all, only the scheme and syntax rules above.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgentThroughForm,
  createMcpConnection,
  deleteToolkit,
  expectStoredAssistantAnswer,
  expectStoredTurnRefusal,
  readAttachedToolkits,
  readStoredTranscript,
} from '../fixtures/api';
import { readsPlatformFlags } from '../fixtures/platformFlags';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const CONVERSATION_CREATED_RE = /\/elitea_core\/conversations\/prompt_lib\/\d+$/;
/** The attach the "+ MCP" picker performs — `setToolkitRelation` in `features/agents/lib/toolRelation.ts:59-83`. */
const TOOL_RELATION_RE = /\/elitea_core\/tool\/prompt_lib\/\d+\/\d+$/;

/**
 * The mock MCP server, as the WORKER dials it — a compose-network name, not a
 * host port. It is the worker that connects, never the browser and never this
 * process, so the endpoint stored here has to resolve inside the stack.
 */
const MOCK_MCP_URL = 'https://mcp-mock:8443/mcp';

/** The one tool `deploy/mock-mcp/server.py` publishes. */
const MOCK_MCP_TOOL = 'echo';

/**
 * A URL Main stores and its OWN MCP proxy validator explicitly permits —
 * `validateMCPProxyURL` allows plain `http` on a loopback host
 * (`services/elitea-main/internal/api/v2/eliteacore/handler.go:3521-3530`) —
 * and that `parse_endpoint` refuses. Port 9 is the discard port: nothing can
 * be listening, so if the scheme rule ever stopped applying, the next failure
 * would be a CONNECT failure carrying a different message, and the assertion
 * below would catch the change instead of passing for a new reason.
 */
const UNSUPPORTED_SCHEME_URL = 'http://localhost:9/mcp';

/** `runtimeFailurePolicyFor` — `internal/transport/runtimegrpc/output/server.go:910-911`. */
const UNSUPPORTED_CAPABILITY_MESSAGE = 'Configuration type is not supported.';

/** The native runtime is the only leg this stack can hold to these contracts — see the header. */
const IS_NATIVE_RUNTIME = (process.env['E2E_WORKER'] ?? 'rust') === 'rust';

readsPlatformFlags(test);

/**
 * Attach `toolkitName` to the agent whose editor is open, through the editor's
 * own "+ MCP" picker.
 *
 * The picker's rows come from a query issued before this test created the
 * connection, so the page is reloaded first — a real user reaches this control
 * on a page load, and a picker that cannot see a just-created connection is a
 * different defect from the one this journey is about.
 */
async function attachMcpThroughPicker(
  page: Page,
  projectId: string,
  toolkitName: string,
): Promise<void> {
  await page.reload();
  await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 30_000 });

  const addMcp = page.getByTestId('agent-add-mcp-button');
  await expect(
    addMcp,
    'the agent editor must offer an MCP picker — it is gated on the platform `mcp_enabled` flag ' +
    '(`useIsMcpVisible`), which is why this spec takes the platform-flag read lock',
  ).toBeVisible({ timeout: 30_000 });
  await expect(addMcp, 'the picker is disabled until the agent is saved').toBeEnabled({ timeout: 30_000 });
  await addMcp.click();

  const menu = page.getByRole('menu');
  await expect(menu).toBeVisible({ timeout: 15_000 });

  // Drive the picker the way a user does: type the connection's name into the
  // MCP section's own search box. The section pages the toolkit listing until a
  // matching row is fetched (`useToolkitInstancePager` + `InstanceAddSection`'s
  // auto-page effect), so a connection that sorts behind 20+ non-MCP toolkits is
  // reachable by name alone — the picker's own pagination now guarantees this,
  // which is why the earlier scroll-the-sibling-Toolkit-dropdown workaround is
  // gone. A picker that could not reach it would fail the `toHaveCount(1)` below.
  await page.getByPlaceholder('Search mcps...').fill(toolkitName);

  // NOT `exact`: the row renders name and description in one `ListItemText`
  // (`ToolMenuDropdown.tsx:131-142`), so its accessible name is both strings
  // joined — an exact match on the name alone finds nothing. The per-run
  // suffix keeps the substring unambiguous.
  const row = page.getByRole('menuitem', { name: toolkitName });

  const attached = page.waitForResponse(
    (r) => TOOL_RELATION_RE.test(new URL(r.url()).pathname) && r.request().method() === 'PATCH',
    { timeout: 30_000 },
  );
  await expect(
    row,
    `the "+ MCP" picker must offer ${toolkitName} — an MCP-typed toolkit belongs in the MCP ` +
    'section, not the Toolkit one (`isMcpToolkit`, `entities/toolkit/model/selectors.ts:30-33`); ' +
    'the wait covers the search debounce and the section paging the listing until it surfaces',
  ).toHaveCount(1, { timeout: 30_000 });
  await row.click();

  const response = await attached;
  expect(
    response.status(),
    `the "+ MCP" picker must attach the connection: ${(await response.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
  expect(
    new URL(response.url()).pathname,
    'the attach must address the project the agent lives in',
  ).toContain(`/prompt_lib/${projectId}/`);
}

/**
 * Open chat with the agent whose editor is open, send `prompt`, and return the
 * conversation the Chat button created.
 */
async function chatWithOpenAgent(page: Page, prompt: string): Promise<string> {
  const conversationCreated = page.waitForResponse(
    (r) => CONVERSATION_CREATED_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversation = (await (await conversationCreated).json()) as { id?: string | number };
  const conversationId = String(conversation.id ?? '');
  expect(conversationId, 'the Chat button must create a conversation to attach to').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}$`), { timeout: 30_000 });

  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.fill(prompt);
  await page.getByTestId('chat-send-button').click();

  const startResponse = await started;
  // An MCP toolkit is decided by the WORKER, never at admission: the
  // version/participant joins the admission gates run
  // (`services/elitea-main/internal/db/queries/agent_chat.sql:27-110`) serve
  // every mapped `elitea_tools` row without an opinion on its type. A 422 here
  // would mean an admission gate grew one, which is a different — and much
  // earlier — regression than anything below.
  expect(
    startResponse.status(),
    `an MCP-bearing agent must not be refused at admission: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  return conversationId;
}

/** Remove the agent and the connection whatever the assertions did. */
async function cleanUp(
  page: Page,
  projectId: string,
  agentId: string,
  toolkitId: string,
): Promise<void> {
  await page.request.delete(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
  );
  await deleteToolkit(page, projectId, toolkitId);
}

// BOTH LEGS. The trust bundle that used to make this native-only is now built
// for the python worker too (`worker-trust`, docker-compose.standalone-full.yml)
// — see this file's header.
test('an MCP connection attached through the picker is discovered, and the agent answers', async ({ page }) => {
  test.setTimeout(240_000);

  const suffix = Date.now() % 1_000_000;
  const agentName = `${AUTOTEST_PREFIX}mcp-${suffix}`;
  const toolkitName = `${AUTOTEST_PREFIX}mcp_kit_${suffix}`;

  // ── 1. Author the agent through the form ────────────────────────────────
  const { projectId, agentId } = await createAgentThroughForm(page, agentName);

  // ── 2. Author the MCP connection ────────────────────────────────────────
  // `selected_tools` names the ONE tool the mock publishes. It is not
  // decoration: `parse_selected_tools` (`mcp.rs:612-634`) carries it into the
  // admission policy, and it is what the delegated-authorization branch would
  // build placeholder tools from if the server answered an OAuth challenge
  // instead of a catalogue.
  const connection = await createMcpConnection(page, projectId, toolkitName, {
    url: MOCK_MCP_URL,
    selected_tools: [MOCK_MCP_TOOL],
  });
  expect(
    connection.settings['url'],
    'the endpoint must round-trip through the write — the runtime reads the STORED settings, not the request',
  ).toBe(MOCK_MCP_URL);

  try {
    // ── 3. Attach it through the editor's own "+ MCP" picker ──────────────
    await attachMcpThroughPicker(page, projectId, toolkitName);

    // What the SERVER stored, never the mutation's status code: the sibling
    // agent-as-tool attach once answered 200 and wrote nothing at all
    // (`chat.agent-tools.spec.ts`, defect class 2). This is also the exact
    // projection the worker's frozen snapshot parses — a row that arrived with
    // the wrong `type` would be classified `Configured` rather than `Mcp`
    // (`snapshot.rs:516`) and would never reach the MCP path.
    const attached = await readAttachedToolkits(page, projectId, agentId);
    const mcpRow = attached.find((tool) => tool.name === toolkitName);
    expect(mcpRow, 'the attached MCP connection must be served back on the version').toBeDefined();
    expect(
      mcpRow?.type,
      'the row must stay `mcp`-typed, or the runtime materializes it as an ordinary toolkit',
    ).toBe('mcp');
    expect(mcpRow?.settings['url'], 'the endpoint the runtime will dial').toBe(MOCK_MCP_URL);
    // The CONNECTION's own selection, not the mapping's. The picker attaches
    // with no per-version narrowing — `setToolkitRelation` omits
    // `selected_tools` when the caller passes none, and `attachMcp` passes
    // none (`features/agents/lib/toolRelation.ts:59-83`,
    // `ToolMenu.tsx:239-247`) — so the admitted set is whatever the connection
    // itself stores, and that is the array `parse_selected_tools`
    // (`mcp.rs:612-634`) reads.
    expect(
      mcpRow?.settings['selected_tools'],
      'the admitted tool must survive to the settings the runtime reads',
    ).toContain(MOCK_MCP_TOOL);
    expect(
      mcpRow?.selectedTools,
      'the picker attaches the whole connection: a per-version narrowing here would mean the UI ' +
      'started sending `selected_tools`, and the intersection in agent_chat.sql would then decide ' +
      'the admitted set instead of the connection',
    ).toEqual([]);

    // ── 4. Take a turn ────────────────────────────────────────────────────
    const conversationId = await chatWithOpenAgent(page, `autotest mcp ${suffix}`);

    // ── 5. …and require a real answer ─────────────────────────────────────
    // This is the whole point. A stored, non-error assistant row means
    // assembly got PAST `materialize_mcp_toolsets_with_tokens_and_authorization`
    // — which, for this reference, means the worker completed a TLS handshake
    // with `mcp-mock`, ran the MCP `initialize` exchange and read a catalogue
    // back. None of that is inferable from stored state, so the answer is the
    // evidence: every failure on that path is fatal to the assembly
    // (`agents/ordinary.rs:471-490`), and the second test is the control that
    // proves an unusable MCP reference is refused rather than skipped.
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 120_000,
      message:
        'the agent never answered with an MCP connection attached — the native runtime could not ' +
        'materialize the toolset. `podman logs elitea-standalone-mcp-mock-1` discriminates: no ' +
        'POST /mcp at all means the worker never reached the server (trust bundle or ' +
        'SSL_CERT_FILE), a "TLS handshake failed" line means it reached it and rejected the ' +
        'certificate',
    });
  } finally {
    await cleanUp(page, projectId, agentId, connection.id);
  }
});

test('an MCP endpoint the platform stores but the runtime will not dial refuses the turn', async ({ page }) => {
  test.skip(
    !IS_NATIVE_RUNTIME,
    'the SDK leg follows pylon’s endpoint rules, which admit shapes this one refuses — see the header',
  );
  test.setTimeout(240_000);

  const suffix = Date.now() % 1_000_000;
  const agentName = `${AUTOTEST_PREFIX}mcpx-${suffix}`;
  const toolkitName = `${AUTOTEST_PREFIX}mcp_bad_${suffix}`;

  const { projectId, agentId } = await createAgentThroughForm(page, agentName);

  // Plain `http` on a loopback host. The platform's own MCP proxy validator
  // permits exactly this (`handler.go:3521-3530`), and the toolkit write does
  // not inspect the URL at all — so this is a connection a user can really
  // create and really attach.
  const connection = await createMcpConnection(page, projectId, toolkitName, {
    url: UNSUPPORTED_SCHEME_URL,
    selected_tools: [MOCK_MCP_TOOL],
  });
  expect(
    connection.settings['url'],
    'the premise of this test is that the PLATFORM stores this endpoint — if it starts rejecting ' +
    'it, the disagreement below is gone and this spec should be retired rather than repaired',
  ).toBe(UNSUPPORTED_SCHEME_URL);

  try {
    await attachMcpThroughPicker(page, projectId, toolkitName);

    const conversationId = await chatWithOpenAgent(page, `autotest mcp scheme ${suffix}`);

    // Admitted, then refused — the same late shape
    // `chat.attachments.spec.ts` pins for an attached file, and for the same
    // structural reason: no admission gate reads a toolkit's settings, only
    // the worker does.
    await expectStoredTurnRefusal(page, projectId, conversationId, {
      timeout: 120_000,
      message:
        'the native runtime answered a turn carrying an `http` MCP endpoint — either ' +
        '`parse_endpoint` (mcp.rs:592-609) no longer refuses a non-https scheme, or MCP ' +
        'references are being skipped instead of refused. Either way this spec pins a contract ' +
        'that is no longer true',
    });

    // WHICH refusal, not just that there was one. `unsupported_authority()`
    // maps to `UnsupportedCapability` (`agents/ordinary.rs:476-478`), which
    // Main renders as this exact sentence
    // (`runtimegrpc/output/server.go:910-911`). Asserting only `is_error`
    // would pass just as happily if the worker had crashed, if the model
    // gateway were down, or if the endpoint had merely been unreachable — the
    // last of which is a DIFFERENT code and a different sentence ("A required
    // runtime dependency is unavailable."), and is precisely the outcome this
    // URL is chosen to rule out.
    const rows = await readStoredTranscript(page, projectId, conversationId);
    const refusal = rows.find((row) => row.role === 'assistant' && row.isError);
    expect(refusal, 'the refused turn must have stored an assistant row').toBeDefined();
    expect(
      refusal?.metadata['error'],
      'the refusal must be the SCHEME refusal — a connect failure here would mean the endpoint ' +
      'was dialled, which is the rule this test says is applied before any dialling happens',
    ).toBe(UNSUPPORTED_CAPABILITY_MESSAGE);
  } finally {
    await cleanUp(page, projectId, agentId, connection.id);
  }
});
