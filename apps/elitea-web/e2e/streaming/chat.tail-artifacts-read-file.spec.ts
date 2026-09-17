/**
 * The Artifact toolkit's READ path and its size limit — the
 * `artifacts/artifacts-toolkit-read-file` onetest folder's agent-path cases.
 *
 * The folder's claim is a pair: a file under the agent path's 200,000-character
 * cap comes back IN FULL (the old 60k limit no longer blocks it), and a file
 * over the cap comes back as the size-limit error string rather than as
 * truncated content that reads like the whole file. Both halves matter and
 * only asserting one of them is how a cap that never fires, or one that fires
 * at the wrong threshold, stays invisible.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * BOTH TESTS ARE FAIL-MARKED ON THE NATIVE (rust) LEG, FOR A GAP ALREADY FILED
 * ─────────────────────────────────────────────────────────────────────────────
 * `artifact` is absent from the native worker's `supported_tool_types`
 * (`services/elitea-worker-rust/src/toolkits/materialize.rs`, and its own
 * capability snapshot lists every family it does have). The toolkit is skipped
 * at assembly — `agent_toolkit_skipped reason_code=unsupported_toolkit_family`
 * — and the turn then either fails outright
 * (`native_agent.invalid_configuration`) or answers with the toolkit simply
 * absent; `chat.artifacts-toolkit.spec.ts` measured both shapes and fail-marks
 * its own WRITE-path case (#906) on exactly this. Either way `read_file` is
 * never dispatched, so neither half of the size contract is observable.
 *
 * THE python LEG'S CONTRACT IS THE OPPOSITE — the SDK's artifact family is a
 * real port — which is why the calls below are SCRIPTED with the SDK's own
 * argument shape (`filename`) rather than left for a model to invent: on that
 * leg every assertion here must pass for real.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT IS PORTED HERE AND WHAT IS NOT
 * ─────────────────────────────────────────────────────────────────────────────
 * The AGENT path only. The folder's other rows run `read_file` through a
 * Pipeline Toolkit Node, a Pipeline Code Node and the Toolkit "Test Settings"
 * panel; none of those has a composed use case on this stack (see
 * S/tail/not-applicable.md), and a test that could not dispatch the call on
 * either leg would assert nothing on either.
 *
 * The file is uploaded through the REAL Artifacts API — the same backend the
 * Artifacts page drives — so the "upload it on the Artifacts page, then read
 * it with an agent" shape of ELITEA-0354 is end to end rather than seeded
 * behind the product's back.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  MOCK_CALL_TOOL_SENTINEL,
  callToolWithArgumentsPrompt,
  createAgentWithVersion,
  expectStoredAssistantAnswer,
  fillComposer,
  readCallerIdentity,
  readCallerPersonalProjectId,
  readStoredTranscript,
} from '../fixtures/api';

const API = `${BASE_URL}/api/v2`;

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The model the standalone stack seeds; overridable for the real-model lane. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/** `chat-stream-e2e.sh` exports this; the local default matches its own. */
const IS_NATIVE_RUNTIME = (process.env['E2E_WORKER'] ?? 'rust') === 'rust';

/** The SDK's artifact-toolkit tool names, as `chat.artifacts-toolkit.spec.ts` lists them. */
const ARTIFACT_TOOL_NAMES = [
  'list_files',
  'create_file',
  'read_file',
  'read_multiple_files',
  'get_file_metadata',
  'delete_file',
  'append_data',
  'create_new_bucket',
] as const;

/**
 * How the agent path reports a read above the cap.
 *
 * The folder quotes a bare string — `[Content has N lines and exceeds size
 * limit. Use partial read options.]` — and the toolkit has since replaced it
 * with a STRUCTURED refusal, measured on a python-worker stack:
 *
 *   {'__result_status__': 'content_too_large', … 'read_limits':
 *    {'max_output_chars': 200000, 'full_read_allowed': False},
 *    'context': {'limit_chars': 200000, 'actual_chars': 300000,
 *    'requested': 'full file read'}, 'total_lines': 4167}
 *
 * which is strictly better (it names the limit, the actual size and the way
 * to read a slice). Both spellings are accepted so the assertion is about the
 * CONTRACT — the read is refused, and refused for size — rather than about a
 * message that has already changed once.
 */
const SIZE_LIMIT_MARK = /content_too_large|exceeds size limit/;
/** The cap itself, which the structured refusal names and the prose one does not. */
const SIZE_LIMIT_CHARS = '200000';

/** A file of `size` characters whose every 64-character window is unique-ish. */
function filler(size: number, token: string): string {
  const line = `${token} the quick brown fox jumps over the lazy dog 0123456789\n`;
  return line.repeat(Math.ceil(size / line.length)).slice(0, size);
}

interface ArtifactAgentFixture {
  readonly projectId: string;
  readonly bucketName: string;
  readonly toolkitId: string;
  readonly agentId: string;
  readonly conversationId: string;
  readonly dispose: () => Promise<void>;
}

/**
 * A real bucket, an `artifact` toolkit pointing at it, an agent with the
 * toolkit attached, and a conversation the agent participates in.
 *
 * The toolkit is created over the API with the type's own stored schema
 * (`bucket` + `selected_tools`) — the same shape and the same route
 * `chat.artifacts-toolkit.spec.ts` uses, which is the evidence that CREATING
 * and ATTACHING one both work and that materialization is the layer that does
 * not.
 */
async function setUpArtifactAgent(page: Page, label: string): Promise<ArtifactAgentFixture> {
  const suffix = `${String(Date.now() % 1_000_000)}${label}`;
  const bucketName = `autotest-artrd-${suffix}`;
  const toolkitName = `${AUTOTEST_PREFIX}artrd-${suffix}`;
  const agentName = `${AUTOTEST_PREFIX}artrdagent-${suffix}`;

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);

  const bucketCreated = await page.request.post(`${API}/artifacts/buckets/${projectId}`, {
    data: { name: bucketName },
  });
  expect(
    [200, 201, 409],
    `the artifacts bucket must be creatable: ${(await bucketCreated.text()).slice(0, 300)}`,
  ).toContain(bucketCreated.status());

  const toolkitCreated = await page.request.post(`${API}/elitea_core/tools/prompt_lib/${projectId}`, {
    data: {
      name: toolkitName,
      type: 'artifact',
      settings: { bucket: bucketName, selected_tools: [...ARTIFACT_TOOL_NAMES] },
    },
  });
  expect(
    toolkitCreated.status(),
    `the artifact toolkit must be creatable: ${(await toolkitCreated.text()).slice(0, 300)}`,
  ).toBe(201);
  const toolkitId = String(((await toolkitCreated.json()) as { id?: unknown }).id ?? '');

  const agent = await createAgentWithVersion(
    page.request,
    agentName,
    {
      instructions: 'You are an autotest agent. Read the files you are asked to read.',
      model: { modelName: MOCK_MODEL },
    },
    projectId,
  );

  const attached = await page.request.patch(`${API}/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`, {
    data: {
      entity_version_id: Number(agent.versionId),
      entity_id: Number(agent.id),
      entity_type: 'agent',
      has_relation: true,
    },
  });
  expect(
    attached.status(),
    `the toolkit must attach to the agent version: ${(await attached.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  const created = await page.request.post(`${API}/elitea_core/conversations/prompt_lib/${projectId}`, {
    data: { name: `${AUTOTEST_PREFIX}artrdconv-${suffix}`, is_private: true },
  });
  expect(created.status(), `the conversation must be created: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const conversationId = String(((await created.json()) as { id?: unknown }).id ?? '');

  const participants = await page.request.post(
    `${API}/elitea_core/participants/prompt_lib/${projectId}/${conversationId}`,
    {
      data: [
        {
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: Number(projectId), name: agentName },
          entity_settings: { version_id: agent.versionId },
        },
        { entity_name: 'user', entity_meta: { id: Number(caller.id) } },
      ],
    },
  );
  expect(
    participants.status(),
    `the participants must be added: ${(await participants.text()).slice(0, 300)}`,
  ).toBe(200);

  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 45_000 });

  return {
    projectId,
    bucketName,
    toolkitId,
    agentId: agent.id,
    conversationId,
    dispose: async (): Promise<void> => {
      await page.request.delete(`${API}/elitea_core/application/prompt_lib/${projectId}/${agent.id}`);
      await page.request.delete(`${API}/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`);
    },
  };
}

/**
 * Upload one file into the bucket over the REAL Artifacts API, and read the
 * listing back.
 *
 * Read back rather than trusted: an upload that answered 2xx and stored
 * nothing would make every assertion afterwards a statement about an empty
 * bucket, and "the agent could not read the file" would look like the size
 * contract failing rather than like the file never being there.
 */
async function uploadArtifact(
  page: Page,
  projectId: string,
  bucketName: string,
  fileName: string,
  content: string,
): Promise<void> {
  // `/artifacts/objects/…`, with `overwrite=true` so a re-run replaces the
  // object instead of 409-ing — the same route and the same flag
  // `e2e/journeys/artifacts/artifacts.lifecycle.spec.ts` drives.
  const uploaded = await page.request.post(
    `${API}/artifacts/objects/${projectId}/${bucketName}?overwrite=true`,
    { multipart: { file: { name: fileName, mimeType: 'text/plain', buffer: Buffer.from(content, 'utf8') } } },
  );
  expect(uploaded.status(), `the artifact must upload: ${(await uploaded.text()).slice(0, 300)}`).toBe(201);
  expect(
    ((await uploaded.json()) as { size_bytes?: number }).size_bytes,
    'the stored object must be the size of the file that was sent',
  ).toBe(content.length);

  // Read the LISTING back. An upload that answered 201 and stored nothing
  // would make every assertion afterwards a statement about an empty bucket,
  // and "the agent could not read the file" would look like the size contract
  // failing rather than like the file never being there.
  const listed = await page.request.get(`${API}/artifacts/objects/${projectId}/${bucketName}`);
  expect(listed.status(), 'the bucket listing must be readable').toBe(200);
  expect(
    await listed.text(),
    'the uploaded file must appear in the bucket listing',
  ).toContain(fileName);
}

/** Send one prompt through the composer and require the start to be ADMITTED. */
async function sendTurn(page: Page, prompt: string): Promise<void> {
  const send = await fillComposer(page, prompt);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await send.click();
  const response = await started;
  expect(response.status(), `the turn was refused: ${(await response.text()).slice(0, 300)}`).toBe(200);
}

/** The newest stored assistant reply, once the mock's END sentinel has arrived. */
async function settledAnswer(page: Page, projectId: string, conversationId: string): Promise<string> {
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 180_000,
    message:
      'the read_file turn never produced a reply quoting a tool result — the artifact toolkit was ' +
      'skipped at assembly and the call was never dispatched',
    contains: MOCK_CALL_TOOL_SENTINEL,
  });
  return (
    (await readStoredTranscript(page, projectId, conversationId))
      .filter((row) => row.role === 'assistant')
      .at(-1)?.content ?? ''
  );
}

/* onetest: ELITEA-0362, ELITEA-0354, ELITEA-0350, ELITEA-0355 — a file between 60k and 200k
 * characters, uploaded through the Artifacts API, is read back IN FULL by an agent using the
 * Artifact toolkit: no size-limit error, and the content the file actually holds. */
test('an agent reads an 80k-character artifact back in full', async ({ page }) => {
  test.setTimeout(420_000);

  // FAIL-MARKED ON BOTH LEGS, for two DIFFERENT measured reasons — which is
  // why the message names both rather than picking one.
  //
  //  - rust: `artifact` is absent from the native worker's
  //    `supported_tool_types`, so materialize.rs skips the toolkit
  //    (`agent_toolkit_skipped reason_code=unsupported_toolkit_family`) and
  //    `read_file` is never dispatched at all; the turn is stored flagged
  //    `is_error`. The same gap `chat.artifacts-toolkit.spec.ts` fail-marks
  //    for the WRITE path (#906).
  //  - python: the toolkit works and the read IS dispatched — and then the
  //    80k result is refused by a DIFFERENT limit downstream of it:
  //    `IS_ERROR:MOCK: tool read_file said Error executing read_file:
  //    RESOURCE_EXHAUSTED: The agent event exceeds its output limit.` So the
  //    toolkit's own 200k cap is not the binding one on the agent path, and a
  //    file the cap admits still cannot be read whole. That is the case's
  //    claim failing for real, not a harness artefact.
  test.fail(
    true,
    'ELITEA-0362: product gap — a file under the artifact toolkit’s 200k agent-path cap still cannot ' +
      'be read in full. On the native worker `artifact` is an unsupported toolkit family (#906) and ' +
      'the call is never dispatched; on the SDK worker the call runs and its 80k result is refused ' +
      'downstream with RESOURCE_EXHAUSTED "The agent event exceeds its output limit". ' +
      'See S/tail/defects.md.',
  );

  let fixture: ArtifactAgentFixture | undefined;
  try {
    fixture = await setUpArtifactAgent(page, 'medium');
    const token = `AUTOTESTMED${String(Date.now() % 1_000_000)}`;
    const fileName = `${AUTOTEST_PREFIX}medium_file.txt`;
    // ~80k: comfortably over the OLD 60k limit the case is about and well
    // under the 200k agent-path cap, so this file distinguishes the two.
    const content = filler(80_000, token);
    await uploadArtifact(page, fixture.projectId, fixture.bucketName, fileName, content);

    // SCRIPTED with the SDK's own argument shape. A model deciding to call
    // `read_file` is not what this case is about, and the mock decides nothing.
    await sendTurn(
      page,
      callToolWithArgumentsPrompt('read_file', { filename: fileName }, `read the full content of ${fileName}`),
    );

    const answer = await settledAnswer(page, fixture.projectId, fixture.conversationId);
    expect(
      answer,
      'a file under the 200k agent-path cap must not be refused for its size — the old 60k limit is gone',
    ).not.toMatch(SIZE_LIMIT_MARK);
    expect(
      answer,
      'the reply must quote the file’s real content, not a summary of it or an empty result',
    ).toContain(token);
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-0364 — a file OVER 200k characters read through the agent path comes back as the
 * size-limit error string, not as content: the cap is enforced on that path. */
test('an agent reading a 300k-character artifact gets the size-limit error', async ({ page }) => {
  test.setTimeout(420_000);

  if (IS_NATIVE_RUNTIME) {
    test.fail(
      true,
      'ELITEA-0364 (#906): product gap — the same unsupported `artifact` family: the toolkit is ' +
        'skipped before the model is reached, so the 200k cap is not observable on this leg. ' +
        'See S/tail/defects.md.',
    );
  }

  let fixture: ArtifactAgentFixture | undefined;
  try {
    fixture = await setUpArtifactAgent(page, 'xlarge');
    const token = `AUTOTESTXL${String(Date.now() % 1_000_000)}`;
    const fileName = `${AUTOTEST_PREFIX}xlarge_file.txt`;
    const content = filler(300_000, token);
    await uploadArtifact(page, fixture.projectId, fixture.bucketName, fileName, content);

    await sendTurn(
      page,
      callToolWithArgumentsPrompt('read_file', { filename: fileName }, `read the full content of ${fileName}`),
    );

    const answer = await settledAnswer(page, fixture.projectId, fixture.conversationId);
    expect(
      answer,
      'a file over the 200k agent-path cap must come back as the size-limit refusal, so a caller can ' +
        'tell a refused read from a short file',
    ).toMatch(SIZE_LIMIT_MARK);
    expect(
      answer,
      'the refusal must NAME the cap it enforced — a refusal that does not say the limit leaves a ' +
        'caller unable to choose a slice that would fit',
    ).toContain(SIZE_LIMIT_CHARS);
    // AND NOT the content. A cap that reported itself and returned the file
    // anyway would satisfy the assertion above on its own, and the whole point
    // of the cap is that the payload does not reach the model.
    expect(
      answer.length,
      'the refused read returned the file as well as the error — the cap did not actually cap anything',
    ).toBeLessThan(200_000);
  } finally {
    await fixture?.dispose();
  }
});
