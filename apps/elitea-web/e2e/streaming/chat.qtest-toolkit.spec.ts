/**
 * The qTest toolkit's WRITE surface against a fake qTest backend — the onetest
 * `toolkits-credentials` cases ELITEA-2243..2251 (#939 group 4).
 *
 * ── Why these cases had nowhere to run, and what changed ────────────────────
 * qTest has no `e2e/live` lane in this repo and `deploy/mock-llm` has no
 * qTest-shaped API, so every one of these nine cases was stream-deferred: they
 * are about what reaches the TEST-MANAGEMENT BACKEND when the model calls
 * `update_test_run_status` / `upload_attachment_to_test_run`, and there was no
 * backend to reach. `deploy/qtest-mock/` is that backend now — the four routes
 * the SDK's wrapper actually calls (`elitea_sdk/tools/qtest/api_wrapper.py`:
 * DQL `search`, `test-runs/execution-statuses`, `test-logs`, `blob-handles`),
 * with the shapes its parsers read, a bearer token it refuses to answer
 * without, and a journal of every request.
 *
 * ── Where each assertion is read ───────────────────────────────────────────
 * On the MOCK's own state, not on the model's prose. The whole subject of
 * these cases is that the status/attachment PERSISTED, and an answer that
 * says so is written by the same model that was told what to say. So each
 * test reads `GET /__state` (the runs, their latest execution log and the
 * attachments) and `GET /__journal` (what was called, with which credential)
 * from the fake backend, and uses the stored transcript only for the claims
 * that really are about what the user is shown — the error messages.
 *
 * ── The python leg ─────────────────────────────────────────────────────────
 * `qtest` is an SDK toolkit family: it exists in the pinned
 * `current_toolkit_schema_snapshot.json` and has no counterpart in
 * `services/elitea-worker-rust/src/toolkits/families/`. A rust-leg run would
 * fail at toolset materialization with `unsupported_toolkit_family`, which is
 * a fact about the native worker's roadmap, not about these cases — so the
 * file requires the python leg rather than asserting that fact nine times.
 */
import { createHash } from 'node:crypto';

import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  callToolWithArgumentsPrompt,
  createAgentWithVersion,
  expectStoredAssistantAnswer,
  readCallerIdentity,
  readCallerPersonalProjectId,
  readStoredTranscript,
} from '../fixtures/api';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/** Where the fake qTest is published on the host — the journal/state reads below. */
const QTEST_HOST = `http://localhost:${process.env['STANDALONE_QTEST_PORT'] ?? '8096'}`;
/** Where the TOOLKIT reaches it: a compose-network address, never the published one. */
const QTEST_BASE_URL = process.env['STANDALONE_QTEST_URL'] ?? 'http://qtest-mock:8096';
const QTEST_TOKEN = process.env['STANDALONE_QTEST_TOKEN'] ?? 'e2e-qtest-token';
/** The seeded project and runs — `deploy/qtest-mock/server.py`'s `_seed`. */
const QTEST_PROJECT_ID = 1;
const SEEDED_RUN = 'TR-1';
const ABSENT_RUN = 'TR-999999';

test.skip(
  (process.env['E2E_WORKER'] ?? 'rust') !== 'python',
  'the qtest toolkit family is SDK-only — the native worker refuses it at materialization (unsupported_toolkit_family)',
);

interface QtestLog {
  readonly id: number;
  readonly test_run_id: number;
  readonly status: { readonly id: number; readonly name: string };
  readonly note: string | null;
  readonly test_case_version_id: number | null;
}

interface QtestAttachment {
  readonly id: number;
  readonly name: string;
  readonly content_type: string;
  readonly size: number;
  readonly md5: string;
  readonly object_type: string;
  readonly object_id: number;
}

interface QtestState {
  readonly runs: Record<string, { readonly id: number; readonly latest_test_log: QtestLog | null }>;
  readonly logs: readonly QtestLog[];
  readonly attachments: Record<string, readonly QtestAttachment[]>;
}

interface QtestJournalEntry {
  readonly method: string;
  readonly path: string;
  readonly status: number;
  readonly auth_ok: boolean;
  readonly body_bytes: number;
  readonly file_name: string | null;
}

async function readQtestState(page: Page): Promise<QtestState> {
  const response = await page.request.get(`${QTEST_HOST}/__state`);
  expect(response.ok(), `the fake qTest must serve its state at ${QTEST_HOST}/__state`).toBe(true);
  return (await response.json()) as QtestState;
}

async function readQtestJournal(page: Page): Promise<readonly QtestJournalEntry[]> {
  const response = await page.request.get(`${QTEST_HOST}/__journal`);
  expect(response.ok(), `the fake qTest must serve its journal at ${QTEST_HOST}/__journal`).toBe(true);
  return (await response.json()) as readonly QtestJournalEntry[];
}

/** Back to the seeded state, journal included — every test owns the whole backend. */
async function resetQtest(page: Page): Promise<void> {
  const response = await page.request.post(`${QTEST_HOST}/__reset`);
  expect(response.ok(), 'the fake qTest must be resettable between tests').toBe(true);
}

interface QtestAgent {
  readonly projectId: string;
  readonly conversationId: string;
  readonly dispose: () => Promise<void>;
}

/**
 * A qTest toolkit pointed at the fake backend, attached to an agent pinned to
 * the deterministic model, with a conversation open in the browser.
 *
 * Built over the API rather than through the forms for the reason
 * `createMockToolAgent` gives: the subject here is what happens DURING a turn,
 * and the toolkit form's own behaviour is another file's job. The attach is
 * READ BACK, because the relation route has answered 200 while writing
 * nothing before (`chat.agent-tools.spec.ts`'s defect class 2).
 */
async function createQtestAgent(page: Page, label: string): Promise<QtestAgent> {
  const suffix = `${String(Date.now() % 1_000_000)}${label}`;
  const credentialTitle = `${AUTOTEST_PREFIX}qtestcred-${suffix}`;
  const toolkitName = `${AUTOTEST_PREFIX}qtesttk-${suffix}`;
  const agentName = `${AUTOTEST_PREFIX}qtestagent-${suffix}`;

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);

  // The CREDENTIAL. `section: credentials` is decided by the type, and the two
  // fields are `QtestConfiguration`'s own (`sdk_config_schemas.json`).
  const credential = await page.request.post(`${API_BASE}/configurations/configurations/${projectId}`, {
    data: {
      type: 'qtest',
      elitea_title: credentialTitle,
      label: credentialTitle,
      shared: false,
      data: { base_url: QTEST_BASE_URL, qtest_api_token: QTEST_TOKEN },
    },
  });
  expect(
    credential.status(),
    `the qtest credential must be creatable: ${(await credential.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  const toolkit = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${projectId}`, {
    data: {
      name: toolkitName,
      type: 'qtest',
      settings: {
        // An OBJECT reference with `private: false` — a `true` there resolves
        // the title in the CALLER's personal project and answers
        // `configuration_not_found` for a row that plainly exists.
        qtest_configuration: { elitea_title: credentialTitle, private: false },
        qtest_project_id: QTEST_PROJECT_ID,
        // Sent explicitly although the catalogue declares `default: 10`: the
        // create route stores the key as JSON `null` when a body omits it, and
        // `QtestApiWrapper` types it `int` — so a toolkit without it dies at
        // materialization with `1 validation error … Input should be a valid
        // integer`, which reaches the user as an empty is_error turn. Measured
        // on this stack before the field was added.
        no_of_tests_shown_in_dql_search: 10,
        selected_tools: ['update_test_run_status', 'upload_attachment_to_test_run'],
      },
    },
  });
  expect(
    toolkit.status(),
    `the qtest toolkit must be creatable: ${(await toolkit.text()).slice(0, 300)}`,
  ).toBe(201);
  const toolkitId = String(((await toolkit.json()) as { id?: unknown }).id ?? '');
  expect(toolkitId, 'the created toolkit must carry an id').not.toBe('');

  const agent = await createAgentWithVersion(
    page.request,
    agentName,
    {
      instructions: 'You are an autotest agent. Call the tools you are asked to call.',
      model: { modelName: MOCK_MODEL },
    },
    projectId,
  );

  const attached = await page.request.patch(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`, {
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
  const storedAgent = await page.request.get(`${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${agent.id}`);
  const tools =
    ((await storedAgent.json()) as { version_details?: { tools?: readonly { name?: string }[] } }).version_details
      ?.tools ?? [];
  expect(
    tools.map((tool) => tool.name),
    'the agent version carries no reference to the toolkit — the attach was a no-op',
  ).toContain(toolkitName);

  const conversation = await page.request.post(`${API_BASE}/elitea_core/conversations/prompt_lib/${projectId}`, {
    data: { name: `${AUTOTEST_PREFIX}qtestconv-${suffix}`, is_private: true },
  });
  expect(
    conversation.status(),
    `the conversation must be created: ${(await conversation.text()).slice(0, 300)}`,
  ).toBe(201);
  const conversationId = String(((await conversation.json()) as { id?: unknown }).id ?? '');

  // BOTH participants: a conversation POSTed to the API carries neither, and
  // its first send 422s at admission.
  const participants = await page.request.post(
    `${API_BASE}/elitea_core/participants/prompt_lib/${projectId}/${conversationId}`,
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
    conversationId,
    dispose: async (): Promise<void> => {
      await page.request.delete(`${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${agent.id}`);
      await page.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`);
    },
  };
}

/** Send one scripted tool call and wait for the turn to be STORED. */
async function callTool(
  page: Page,
  agent: QtestAgent,
  tool: string,
  args: Readonly<Record<string, unknown>>,
  tail: string,
): Promise<string> {
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 30_000 });
  await input.fill(callToolWithArgumentsPrompt(tool, args, tail));
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();
  const startResponse = await started;
  expect(
    startResponse.status(),
    `the qtest turn was refused: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  await expectStoredAssistantAnswer(page, agent.projectId, agent.conversationId, {
    timeout: 180_000,
    message: 'the qtest turn produced no stored answer — the toolkit may not have materialized',
  });
  const rows = await readStoredTranscript(page, agent.projectId, agent.conversationId);
  return rows
    .filter((row) => row.role !== 'user')
    .map((row) => row.content)
    .join('\n');
}

/** Upload one object into a fresh bucket, and return its `/bucket/name` filepath. */
async function seedArtifact(
  page: Page,
  projectId: string,
  bucket: string,
  fileName: string,
  body: Buffer,
  mimeType: string,
): Promise<string> {
  const created = await page.request.post(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}`, {
    data: { name: bucket },
  });
  expect([200, 201, 409]).toContain(created.status());
  const uploaded = await page.request.post(
    `${BASE_URL}/api/v2/artifacts/objects/${projectId}/${bucket}?overwrite=true`,
    { multipart: { file: { name: fileName, mimeType, buffer: body } } },
  );
  expect(uploaded.status(), await uploaded.text()).toBe(201);
  return `/${bucket}/${fileName}`;
}

async function dropBucket(page: Page, projectId: string, bucket: string): Promise<void> {
  await page.request.delete(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}/${bucket}`).catch(() => {});
}

test.beforeEach(async ({ page }) => {
  await resetQtest(page);
});

/* onetest: ELITEA-2244, ELITEA-2250 — a manual execution status with the optional note reaches the qTest
 * backend, and it is still there after the page that wrote it is gone. */
test('a status update with a note is recorded on the qTest backend and survives a reload', async ({ page }) => {
  test.setTimeout(300_000);
  const agent = await createQtestAgent(page, 'status');
  try {
    const note = `autotest note ${String(Date.now() % 1_000_000)} — database connection timeout during login`;
    const answer = await callTool(
      page,
      agent,
      'update_test_run_status',
      { test_run_id: SEEDED_RUN, status: 'Failed', note },
      'record the failure',
    );
    expect(answer, 'the tool result must name the run it recorded').toContain(SEEDED_RUN);

    const state = await readQtestState(page);
    expect(state.logs, 'exactly one execution log must have been written').toHaveLength(1);
    const log = state.logs[0];
    expect(log?.status.name, 'the status the tool was asked for is the status qTest stored').toBe('Failed');
    expect(log?.note, 'the OPTIONAL note parameter must reach the backend, not be dropped').toBe(note);
    expect(
      state.runs[SEEDED_RUN]?.latest_test_log?.id,
      'the run must now point at that log — a log nothing references is not an execution record',
    ).toBe(log?.id);

    // The CREDENTIAL, read from the backend's own journal: every /api/v3 route
    // answers 401 without the bearer, so a recorded call with `auth_ok` proves
    // the toolkit resolved the configuration rather than that the mock is lax.
    const journal = await readQtestJournal(page);
    const submits = journal.filter((entry) => entry.path.endsWith('/test-logs') && entry.method === 'POST');
    expect(submits, 'the submit must appear in the backend journal exactly once').toHaveLength(1);
    expect(submits.every((entry) => entry.auth_ok), 'every call must carry the configured bearer token').toBe(true);

    // PERSISTENCE (ELITEA-2250): the session that wrote it is discarded, and
    // the backend is asked again.
    await page.reload();
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 45_000 });
    const after = await readQtestState(page);
    expect(after.logs, 'the execution log must still be there after the page is reloaded').toHaveLength(1);
    expect(after.logs[0]?.status.name).toBe('Failed');
    expect(after.logs[0]?.note).toBe(note);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2245 — a testRunId that exists nowhere is refused with a message naming it, and nothing is
 * written to the backend. */
test('an unknown test run is refused and leaves the backend untouched', async ({ page }) => {
  test.setTimeout(300_000);
  const agent = await createQtestAgent(page, 'badrun');
  try {
    const answer = await callTool(
      page,
      agent,
      'update_test_run_status',
      { test_run_id: ABSENT_RUN, status: 'Passed' },
      'record the pass',
    );
    expect(answer, 'the failure must name the run that was not found').toContain(ABSENT_RUN);
    expect(answer.toLowerCase(), 'the message must say the run was not found').toContain('not found');

    const state = await readQtestState(page);
    expect(state.logs, 'a refused update must write no execution log at all').toHaveLength(0);
    const journal = await readQtestJournal(page);
    expect(
      journal.filter((entry) => entry.path.endsWith('/test-logs')),
      'the submit must never be attempted for a run the search could not resolve',
    ).toHaveLength(0);
    expect(
      journal.some((entry) => entry.path.endsWith('/search')),
      'the toolkit must have ASKED the backend — a client-side guess would refuse a real run too',
    ).toBe(true);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2246 — a status the project does not define is refused, the message lists the ones it does,
 * and no execution log is written. */
test('an unknown status is refused with the list of the ones the project defines', async ({ page }) => {
  test.setTimeout(300_000);
  const agent = await createQtestAgent(page, 'badstatus');
  try {
    const answer = await callTool(
      page,
      agent,
      'update_test_run_status',
      { test_run_id: SEEDED_RUN, status: 'InvalidStatus' },
      'record the result',
    );
    expect(answer, 'the message must name the status that was refused').toContain('InvalidStatus');
    // The nine the fake project defines — the message has to EDUCATE, which is
    // the case's own wording, so the two ends of the list are both required.
    expect(answer, 'the message must list the allowed values').toContain('Passed');
    expect(answer, 'the message must list the allowed values').toContain('Unexecuted');

    const state = await readQtestState(page);
    expect(state.logs, 'a refused status must write no execution log').toHaveLength(0);
    const journal = await readQtestJournal(page);
    expect(
      journal.some((entry) => entry.path.endsWith('/execution-statuses')),
      'the allowed values must come from the PROJECT, not from a hardcoded list',
    ).toBe(true);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2243, ELITEA-2249, ELITEA-2251 — a file that lives in an Artifacts bucket is attached to the
 * test run's execution log, the answer confirms filename and target, and the attachment is still on the
 * backend after the session that uploaded it is gone. */
test('a file from an artifacts bucket is attached to the test run and persists', async ({ page }) => {
  test.setTimeout(420_000);
  test.fail(true, '#978: product gap — the SDK reads a filepath through GET /api/v2/artifacts/artifact/default/{project}/{bucket}/{key}, a route elitea-main does not serve, so every attach answers "Resource not found" whatever the path');
  const agent = await createQtestAgent(page, 'attach');
  // Hyphens, never the `autotest_` prefix's underscore: a bucket name is an
  // object-store name and the create route refuses one with 400.
  const bucket = `autotest-qtest-${String(Date.now() % 1_000_000)}`;
  const fileName = 'execution-report.txt';
  const body = Buffer.from(`autotest execution report ${String(Date.now())}\nstep 1: ok\nstep 2: ok\n`);
  try {
    const filepath = await seedArtifact(page, agent.projectId, bucket, fileName, body, 'text/plain');

    // A test LOG is the attach target for `attachment_type: 'test-log'`, and a
    // run that has never been executed has none — so the status comes first,
    // exactly as the tool's own error tells a user to do.
    await callTool(
      page,
      agent,
      'update_test_run_status',
      { test_run_id: SEEDED_RUN, status: 'Passed' },
      'record the pass',
    );
    const answer = await callTool(
      page,
      agent,
      'upload_attachment_to_test_run',
      { test_run_id: SEEDED_RUN, attachment_type: 'test-log', filepath },
      'attach the report',
    );
    expect(answer, 'the confirmation must name the file it uploaded').toContain(fileName);
    expect(answer, 'the confirmation must name the run it attached to').toContain(SEEDED_RUN);
    expect(answer, 'the confirmation must name what it attached to').toContain('test-log');

    const state = await readQtestState(page);
    const logId = state.runs[SEEDED_RUN]?.latest_test_log?.id;
    expect(logId, 'the run must carry the execution log the attachment hangs off').toBeTruthy();
    const attached = state.attachments[`test-logs/${String(logId)}`] ?? [];
    expect(attached, 'the execution log must carry exactly one attachment').toHaveLength(1);
    expect(attached[0]?.name, 'the attachment must keep the artifact filename').toBe(fileName);
    expect(attached[0]?.size, 'the attachment must be the whole file').toBe(body.byteLength);
    // The BYTES, by digest: a truncated or re-encoded upload cannot read as
    // equal, and a size alone can.
    expect(
      attached[0]?.md5,
      'the bytes qTest received must be the bytes the artifact holds, not a re-encoding of them',
    ).toBe(createHash('md5').update(body).digest('hex'));

    const journal = await readQtestJournal(page);
    const uploads = journal.filter((entry) => entry.path.endsWith('/blob-handles'));
    expect(uploads, 'the upload must appear in the backend journal exactly once').toHaveLength(1);
    expect(uploads[0]?.file_name, 'the upload must carry the File-Name header qTest keys the blob by').toBe(fileName);
    expect(uploads[0]?.auth_ok, 'the upload must carry the configured bearer token').toBe(true);

    // PERSISTENCE (ELITEA-2251).
    await page.reload();
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 45_000 });
    const after = await readQtestState(page);
    expect(
      (after.attachments[`test-logs/${String(logId)}`] ?? []).map((item) => item.name),
      'the attachment must still be there after the page is reloaded',
    ).toContain(fileName);
  } finally {
    await dropBucket(page, agent.projectId, bucket);
    await agent.dispose();
  }
});

/* onetest: ELITEA-2247 — an attachment whose file source resolves to nothing is refused with a message about
 * the file, and nothing is attached. (The case's own `file=None` cannot be sent: `filepath` is a REQUIRED
 * field of the tool's argument schema, so an omitted one is refused before the tool runs. A path that names
 * no artifact is the same claim through the door the model can actually reach.) */
test('an attachment whose file source resolves to nothing is refused', async ({ page }) => {
  test.setTimeout(300_000);
  const agent = await createQtestAgent(page, 'badfile');
  try {
    await callTool(
      page,
      agent,
      'update_test_run_status',
      { test_run_id: SEEDED_RUN, status: 'Passed' },
      'record the pass',
    );
    const answer = await callTool(
      page,
      agent,
      'upload_attachment_to_test_run',
      { test_run_id: SEEDED_RUN, attachment_type: 'test-log', filepath: '/autotest-missing/nothing-here.txt' },
      'attach the report',
    );
    // NOT A STRONG PROOF WHILE #978 STANDS, and saying so here is the point:
    // a VALID path answers with this same sentence today, because the route
    // the SDK reads a filepath through is unported. What this case can still
    // show is the half that is about the platform's own behaviour — the
    // failure is reported to the user and NOTHING is uploaded or attached.
    expect(answer.toLowerCase(), 'the message must be about the FILE, not about the test run').toMatch(
      /artifact|file|download|not found/,
    );

    const journal = await readQtestJournal(page);
    expect(
      journal.filter((entry) => entry.path.endsWith('/blob-handles')),
      'nothing may be uploaded when the file source resolved to nothing',
    ).toHaveLength(0);
    const state = await readQtestState(page);
    expect(Object.keys(state.attachments), 'the attachment list must be unchanged').toHaveLength(0);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2248 — an attachmentType outside {test-case, test-log} must be refused, naming the two that
 * are allowed. The guard is the ARGUMENT SCHEMA, not the method body: `upload_attachment_to_test_run`'s
 * `attachment_type` is a Literal, so the refusal happens before the run is searched and before any bytes are
 * read — which is why this case is unaffected by #978 below it. */
test('an unknown attachment type is refused, naming the allowed ones', async ({ page }) => {
  test.setTimeout(300_000);
  const agent = await createQtestAgent(page, 'badtype');
  try {
    await callTool(
      page,
      agent,
      'update_test_run_status',
      { test_run_id: SEEDED_RUN, status: 'Passed' },
      'record the pass',
    );
    const filepath = await seedArtifact(
      page,
      agent.projectId,
      `autotest-qtesttype-${String(Date.now() % 1_000_000)}`,
      'screenshot_execution.txt',
      Buffer.from('autotest screenshot stand-in'),
      'text/plain',
    );
    const answer = await callTool(
      page,
      agent,
      'upload_attachment_to_test_run',
      { test_run_id: SEEDED_RUN, attachment_type: 'invalid-type', filepath },
      'attach the report',
    );
    expect(answer, 'the message must name the two allowed attachment types').toContain('test-case');
    expect(answer, 'the message must name the two allowed attachment types').toContain('test-log');
    const journal = await readQtestJournal(page);
    expect(
      journal.filter((entry) => entry.path.endsWith('/blob-handles')),
      'an unknown attachment type must upload nothing',
    ).toHaveLength(0);
  } finally {
    await agent.dispose();
  }
});
