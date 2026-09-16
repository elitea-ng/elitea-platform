/**
 * The built-in `artifact` TOOLKIT TYPE, attached to an agent and asked — in an
 * ordinary chat turn — to write a file into a REAL Artifacts bucket
 * (`/api/v2/artifacts/...`, the same backend `e2e/journeys/artifacts/
 * artifacts.lifecycle.spec.ts` drives from the UI).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS, AND WHAT IS ACTUALLY MEASURED AGAINST THE LIVE STACK
 * ─────────────────────────────────────────────────────────────────────────────
 * The S1 ledger's `artifacts-toolkit-multi-file` cases (ELITEA-1334, 1337,
 * 1338) all share one shape: an agent with the Artifact toolkit attached
 * creates N files, and the Artifacts page's own bucket listing is read back to
 * confirm exactly those N files landed, with real timestamps. `elitea-main`'s
 * type catalogue (`internal/api/v2/toolkits/handler.go`'s `toolkitTypeSchemas
 * ["artifact"]`) accepts the toolkit exactly as the onetest cases describe it
 * — a `bucket` field, and tools named `list_buckets`/`list_artifacts`/
 * `read_artifact`/`upload_artifact`/`delete_artifact`/`index_data`/
 * `search_data` — so CREATING one, and ATTACHING it to an agent, both work.
 *
 * Materialization is the layer that does not, and the FAILURE MODE was
 * measured directly against this stack (`STANDALONE_WORKER=rust`) rather than
 * only inferred from source — the two disagree in a way worth recording. The
 * static read of `services/elitea-worker-rust/src/toolkits/materialize.rs`'s
 * capability manifest (`current_rust_worker_toolkit_capability_snapshot.json`
 * — `supported_tool_types` lists azure, azure_search, elastic, gcp, github,
 * gitlab_org, google_places, k8s, keycloak, openapi, postman, rally,
 * report_portal, salesforce, service_now, sharepoint, slack, sonar, sql,
 * yagmail, zephyr, zephyr_squad; no `artifact`, and no `artifact/` directory
 * under `.../toolkits/families/`) predicted a silent skip — the toolkit
 * omitted, the turn answering normally with zero tools. The worker's own log
 * says otherwise:
 *
 *   agent_toolkit_skipped reason_code=unsupported_toolkit_family
 *     toolkit_type=artifact toolkit_id=…
 *   native_agent.invalid_configuration  (agents/ordinary.rs, outcome=failed)
 *   native agent assembly failed after invocation authorization
 *     error_code=native_agent.invalid_configuration
 *
 * The unsupported toolkit is skipped (as the manifest implies). What happens
 * next for an agent whose only participant tool is the one just skipped was
 * measured NOT to be deterministic across runs: sometimes the assembler
 * treats the resulting toolset as inadmissible and fails the WHOLE turn
 * before the model is ever called (`native_agent.invalid_configuration`,
 * confirmed by the mock's own `__journal` gaining no entry for the turn, and
 * a stored `chat_message_group` flagged `metadata.is_error: true`,
 * `metadata.error: "The runtime operation failed."`); sometimes the turn
 * answers normally with the toolkit simply absent. Either way, no upload call
 * is ever dispatched and the bucket never gains the file — which is the
 * assertion this file's `test.fail` is anchored to, rather than to either
 * failure shape alone.
 *
 * Written as ELITEA-1334/1337/1338 assume it works (an agent turn creates a
 * file, and the REAL Artifacts bucket — read over its own API, never the
 * screen — holds it afterwards) and marked `test.fail`, per the porting
 * rulebook's "no `test.skip` for a product gap" rule. No `[[mock:call_tool]]`
 * marker is scripted on THIS leg: the model is never reached at all (see
 * above), so scripting one would prove nothing this turn does not already
 * show without it.
 *
 * THE python LEG'S CONTRACT IS THE OPPOSITE, and DOES need a scripted call —
 * the mock never calls a tool on its own, marker or not. The gap above is
 * entirely a `materialize.rs` (native rust worker) omission — the SDK-backed
 * python worker's artifact toolkit family is a real port, and on that leg the
 * upload really is dispatched and the bucket really does gain the file,
 * PROVIDED the turn actually asks the mock to call the tool the SDK exposes.
 * `internal/api/v2/toolkits/handler.go`'s `toolkitTypeSchemas["artifact"]` —
 * an earlier draft of this file's `selected_tools` quoted exactly this map —
 * says of itself that "every artifact tool except index_data...name[s] no
 * SDK tool at all": its
 * `upload_artifact`/`list_buckets`/`list_artifacts`/`read_artifact`/
 * `delete_artifact` are hand-written placeholders, never implemented by
 * `elitea_sdk.runtime.tools.artifact.ArtifactWrapper`. The real names are the
 * pinned `current_toolkit_schema_snapshot.json`'s `artifact` entry:
 * `list_files`, `create_file`, `read_file`, `get_file_metadata`,
 * `delete_file`, `append_data`, `create_new_bucket`, plus the indexing family.
 * Passing the placeholder names as `selected_tools` therefore filtered the
 * real `create_file` tool OUT of the toolkit the SDK builds (`ArtifactToolkit
 * .get_toolkit`'s `if tool["name"] not in selected_tools: continue`) — only
 * `index_data` ever survived that filter, coincidentally spelled the same in
 * both lists. So even with a marker, the model would have had no upload tool
 * to call. `REAL_ARTIFACT_TOOL_NAMES` below selects the real names, and the
 * turn now scripts `[[mock:call_tool create_file {...}]]` naming that real
 * tool with its real argument shape (`filename` required, `filedata` the
 * content). `test.fail` below is gated on `IS_NATIVE_RUNTIME` (same
 * `E2E_WORKER` read `chat.toolkit-hitl.spec.ts` uses): rust leg keeps the
 * product-gap marker, python leg runs every assertion below for real and must
 * pass them.
 *
 * WHY THE BUCKET/TOOLKIT/AGENT CLEANUP IS A `finally`, UNLIKE MOST FILES HERE.
 * `expectStoredAssistantAnswer`'s "best-effort, deliberately last" convention
 * assumes the created rows are inert if a failure leaves them behind. A
 * leftover ARTIFACTS BUCKET is not inert: `chat.canvasDocument.spec.ts`'s own
 * save dialog auto-fills its bucket SELECT with "the lone bucket" in the
 * project — measured, a first (uncorrected) run of this file that threw
 * before reaching its own cleanup left `autotest-arttk-…` behind, and the
 * very next file in this alphabetically-sorted, `--workers=1` suite
 * (`chat.canvasDocument.spec.ts`) then failed asserting THAT bucket's name
 * where it expected its own. So the bucket this file creates is deleted in a
 * `finally`, unconditionally — and on the python leg the bucket is genuinely
 * NON-EMPTY (the file above really landed), so the files are deleted first,
 * then the bucket, rather than trusting an empty-bucket assumption that only
 * held on the rust leg.
 *
 * WHY IT LIVES HERE: a toolkit chat turn needs the FULL standalone stack
 * (runtime plane, worker, model) — `journeys/**` has no runtime plane at all,
 * so the picker/attach half (which DOES work) has no turn to prove wrong.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  EMPTY_TOOLKIT_GUARDRAILS,
  MOCK_CALL_TOOL_SENTINEL,
  attachToolkitThroughPicker,
  callToolWithArgumentsPrompt,
  createAgentThroughForm,
  readCallerPersonalProjectId,
  readStoredTranscript,
  setToolkitGuardrails,
} from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The model the turn depends on — same pin `chat.hitl.spec.ts`/`chat.toolkit.spec.ts` use, and for the same reason: an empty `llm_settings` falls back to the project catalogue's default, which is not this mock on a stack that also carries a real provider. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/**
 * Which runtime is answering the turn — `scripts/chat-stream-e2e.sh` exports
 * this; local default is the native-runtime dev stack, as elsewhere (see
 * `chat.toolkit-hitl.spec.ts`). Read once, to gate `test.fail` below: the
 * "artifact" toolkit family is a genuine rust-worker gap (see header), not a
 * gap on the python/SDK leg.
 */
const IS_NATIVE_RUNTIME = (process.env['E2E_WORKER'] ?? 'rust') === 'rust';

/**
 * The REAL tool names `elitea_sdk.runtime.tools.artifact.ArtifactWrapper.
 * get_available_tools` implements, confirmed against the pinned
 * `services/elitea-main/internal/runtimecomposition/
 * current_toolkit_schema_snapshot.json`'s `artifact` entry's `args_schemas`
 * keys. `selected_tools` below must use these, not `ARTIFACT_TOOL_NAMES`
 * above, or the toolkit the SDK builds never carries `create_file`.
 */
const REAL_ARTIFACT_TOOL_NAMES = [
  'list_files',
  'create_file',
  'read_file',
  'get_file_metadata',
  'delete_file',
  'append_data',
  'create_new_bucket',
] as const;

interface ObjectSummary {
  readonly key?: string;
}

test('an agent with the artifact toolkit writes a file that a real bucket listing then shows', async ({ page }) => {
  test.setTimeout(180_000);

  const suffix = String(Date.now() % 1_000_000);
  const bucketName = `autotest-arttk-${suffix}`;
  const toolkitName = `${AUTOTEST_PREFIX}arttk-${suffix}`;
  const agentName = `${AUTOTEST_PREFIX}arttkagent-${suffix}`;
  const fileName = `${AUTOTEST_PREFIX}upload-${suffix}.txt`;

  // Rust leg only — see header's "python LEG'S CONTRACT IS THE OPPOSITE".
  // The SDK/python worker's artifact toolkit family is a real port, so on
  // that leg every assertion below (steps 6-7) must pass for real.
  if (IS_NATIVE_RUNTIME) {
    test.fail(
      true,
      'ELITEA-1334 (#906)/1337/1338: product gap — "artifact" is absent from the native rust worker\'s ' +
        '`supported_tool_types`, so materialize.rs skips it (agent_toolkit_skipped, ' +
        'reason_code=unsupported_toolkit_family). Measured non-deterministic across runs which of two ' +
        'ways that then surfaces: sometimes assembly fails outright (native_agent.invalid_configuration, ' +
        'an is_error row, model never called) and sometimes the turn answers normally with the toolkit ' +
        'simply absent — but either way no upload call is ever dispatched and no file is ever written. ' +
        'See S/port/defects.md.',
    );
  }

  // ── 0. Preconditions ─────────────────────────────────────────────────────
  await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS);
  // The chat persona works inside its OWN personal project (#290) — the
  // bucket and the toolkit below must land in that SAME project, or the
  // agent page's toolkit picker (project-scoped) never lists what this test
  // just created.
  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');

  let toolkitId = '';
  let agentId = '';
  try {
    // ── 1. A real bucket, over the real Artifacts API ────────────────────
    const bucketCreated = await page.request.post(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}`, {
      data: { name: bucketName },
    });
    expect(
      [200, 201, 409],
      `the artifacts bucket must be creatable: ${(await bucketCreated.text()).slice(0, 300)}`,
    ).toContain(bucketCreated.status());

    // ── 2. The `artifact` toolkit, authored directly over the API ────────
    // The type's own stored schema — `bucket` plus `selected_tools` — same
    // create route `createGithubToolkit` uses for `github`.
    const toolkitCreated = await page.request.post(`${BASE_URL}/api/v2/elitea_core/tools/prompt_lib/${projectId}`, {
      data: {
        name: toolkitName,
        type: 'artifact',
        settings: { bucket: bucketName, selected_tools: [...REAL_ARTIFACT_TOOL_NAMES] },
      },
    });
    expect(
      toolkitCreated.status(),
      `the artifact toolkit must be creatable — the type schema accepts exactly this shape: ${(await toolkitCreated.text()).slice(0, 300)}`,
    ).toBe(201);
    toolkitId = String(((await toolkitCreated.json()) as { id?: string | number }).id ?? '');
    expect(toolkitId, 'the created toolkit must carry an id').not.toBe('');

    // ── 3. An agent, pinned to the deterministic model ───────────────────
    const agent = await createAgentThroughForm(page, agentName);
    agentId = agent.agentId;
    const storedAgent = await page.request.get(
      `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${agent.projectId}/${agent.agentId}`,
    );
    const storedMeta =
      ((await storedAgent.json()) as { version_details?: { meta?: Record<string, unknown> } }).version_details
        ?.meta ?? {};
    const pinned = await page.request.put(
      `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${agent.projectId}/${agent.agentId}/${agent.versionId}`,
      { data: { meta: storedMeta, llm_settings: { model_name: MOCK_MODEL } } },
    );
    expect(
      pinned.status(),
      `the mock model must be pinnable: ${(await pinned.text()).slice(0, 300)}`,
    ).toBeLessThan(300);

    // ── 4. Attach the artifact toolkit through the agent page's own picker ─
    await expect(
      page.getByTestId('agent-toolkits-section'),
      'the save must land on the agent edit page, where the Tools panel lives',
    ).toBeVisible({ timeout: 30_000 });
    await attachToolkitThroughPicker(page, toolkitName);

    // ── 5. Chat: a scripted call to the REAL SDK tool ────────────────────
    // `[[mock:call_tool create_file {...}]]` — see the header: on the
    // rust leg the toolkit is skipped before the model is ever reached, so
    // the marker is inert there (harmless — the turn fails the same way
    // either way); on the python leg it is what actually makes the mock
    // dispatch `create_file` with the real SDK argument shape (`filename`
    // required, `filedata` the content), rather than merely echoing an
    // ordinary instruction as text with no tool call at all.
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

    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 30_000 });
    await input.fill(
      callToolWithArgumentsPrompt(
        'create_file',
        { filename: fileName, filedata: 'hello from the artifact toolkit' },
        `autotest: upload a file named ${fileName} with the content "hello from the artifact toolkit"`,
      ),
    );
    await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
    await page.getByTestId('chat-send-button').click();

    const startResponse = await started;
    expect(startResponse.status(), `the turn was refused: ${(await startResponse.text()).slice(0, 300)}`).toBe(200);

    // ── 6. THE materialization proof — read off the STORED row, polled until
    // it is actually TERMINAL rather than merely present.
    //
    // `readStoredTranscript`'s own `isError` field is `metadata.is_error ===
    // true` — an ABSENT key (a row still streaming; see
    // `expectStoredAssistantAnswer`'s own comment on why the key is present
    // ONLY once a terminal projection has run) reads as `false` exactly like a
    // CONFIRMED non-error answer does. Polling on the row merely EXISTING (an
    // earlier draft of this file did, reasoning that the rust leg's assembly
    // failure is near-instant so waiting for a NEVER-arriving success would
    // waste the whole budget) caught the python leg's row the instant it was
    // created — content `""`, no `is_error` key yet, the model's real
    // round-trip still in flight — and then read that absence as "finished,
    // no error", which is the exact "ABSENT reads as success" trap this
    // suite's other files are full of (see `disclosed-gap-doc-comments`).
    // Polling on the KEY'S PRESENCE (true OR false) rather than on the row's
    // existence keeps both properties: still fast on the rust leg (is_error
    // lands near-instantly there too), and it no longer settles early on the
    // python leg's still-streaming row.
    await expect
      .poll(
        async () => {
          const rows = await readStoredTranscript(page, projectId, conversationId);
          const row = rows.find((r) => r.role === 'assistant');
          return row !== undefined && row.metadata['is_error'] !== undefined;
        },
        {
          timeout: 60_000,
          message: 'the turn never reached a TERMINAL assistant row (metadata.is_error was never set) — not even a refusal',
        },
      )
      .toBe(true);
    const rows = await readStoredTranscript(page, projectId, conversationId);
    const answer = rows.find((row) => row.role === 'assistant');
    expect(
      answer?.isError,
      'the turn must finish with a real, non-error reply — the artifact toolkit must materialize and ' +
        'the upload must be dispatched',
    ).toBe(false);
    // Cheap, no extra poll: `answer` above is already the terminal row (isError
    // checked false, which only a finalized projection ever sets — see
    // `expectStoredAssistantAnswer`), so its content cannot grow further. The
    // sentinel's presence tells "the model actually called `create_file` and
    // the mock's continuation quoted the result" apart from "the model wrote
    // ordinary text mentioning the filename without calling anything" — the
    // ABSENT-as-success trap this file's own header warns against.
    expect(
      answer?.content ?? '',
      'the finished reply must show create_file actually ran (quoting MOCK_CALL_TOOL_SENTINEL), not ' +
        'merely a non-error text answer that never called the tool',
    ).toContain(MOCK_CALL_TOOL_SENTINEL);

    // ── 7. THE bucket proof — the claim the onetest cases actually make ──
    const listed = await page.request.get(`${BASE_URL}/api/v2/artifacts/objects/${projectId}/${bucketName}`);
    expect(listed.ok(), `the seeded bucket must be listable: ${(await listed.text()).slice(0, 300)}`).toBe(true);
    const objects = ((await listed.json()) as { objects?: readonly ObjectSummary[] }).objects ?? [];
    expect(
      objects.map((object) => object.key),
      'the file the turn asked for must be present in the real bucket listing',
    ).toContain(fileName);
  } finally {
    // Unconditional, and NOT best-effort-and-last like this directory's other
    // cleanups — see the header's note on why a leftover BUCKET here is not
    // inert. FILES FIRST, then the bucket: on the python leg (see header)
    // the upload really landed, so the bucket is genuinely non-empty here,
    // not just on the rust leg's trivially-empty one.
    try {
      const objectsLeft = await page.request.get(`${BASE_URL}/api/v2/artifacts/objects/${projectId}/${bucketName}`);
      if (objectsLeft.ok()) {
        const keys = (((await objectsLeft.json()) as { objects?: readonly ObjectSummary[] }).objects ?? [])
          .map((object) => object.key)
          .filter((key): key is string => typeof key === 'string' && key !== '');
        if (keys.length > 0) {
          await page.request.post(`${BASE_URL}/api/v2/artifacts/objects/${projectId}/${bucketName}:batchDelete`, {
            data: { keys },
          });
        }
      }
    } catch {
      // Best-effort: the bucket delete right below still cascades its own
      // object deletion server-side (`DeleteBucket` → `deleteAllObjects`) —
      // this pass is defense-in-depth, not the only thing standing between
      // a failed turn and a leftover file.
    }
    await page.request.delete(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}/${bucketName}`).catch(() => {});
    if (toolkitId !== '') {
      await page.request.delete(`${BASE_URL}/api/v2/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`).catch(() => {});
    }
    if (agentId !== '') {
      await page.request
        .delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`)
        .catch(() => {});
    }
  }
});
