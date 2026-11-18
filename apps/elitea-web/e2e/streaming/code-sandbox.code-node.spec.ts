/**
 * A pipeline CODE node on the real code sandbox (#2206, #2208, #2227).
 *
 * The sibling journey (`code-sandbox.run-code.spec.ts`) proves the agent tool;
 * this one proves the other consumer of the same sandbox, the graph `code`
 * node, through Main's dispatch grant, the worker's node journal and a real
 * runner container. The node computes `1+1` in JavaScript and returns
 * `sum:2`; the pipeline's terminal `answer` is read from the STORE. No model is
 * involved, so nothing here can pass on prose.
 *
 * A node whose program throws ends the run with the registered Code failure
 * and the tail of the program's stderr after it (#2227). That text is read from
 * the stored assistant row (`metadata.error`), the same row the chat shows.
 *
 * Needs the stack from scripts/code-sandbox-e2e.sh.
 */
import { expect, type Page, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  expectStoredAssistantAnswer,
  fillComposer,
  readStoredTranscript,
  settleStoredTurn,
} from '../fixtures/api';

const APPLICATIONS_RE = /\/elitea_core\/applications\/prompt_lib\/(\d+)/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

const PIPELINE_YAML = `state:
  answer: str
entry_point: compute
nodes:
  - id: compute
    type: code
    language: javascript
    code: "export default async (state) => 'sum:' + (1 + 1);"
    output: [answer]
    transition: END
`;

/** The thrown message the failing node's program carries; unique so nothing else can contain it. */
const THROWN_MESSAGE = 'node-boom-2227';

const THROWING_PIPELINE_YAML = `state:
  answer: str
entry_point: compute
nodes:
  - id: compute
    type: code
    language: javascript
    code: "export default async (state) => { throw new Error('${THROWN_MESSAGE}'); };"
    output: [answer]
    transition: END
`;

const createdPipelines: { projectId: string; pipelineId: string }[] = [];

test.afterEach(async ({ page }) => {
  for (const { projectId, pipelineId } of createdPipelines.splice(0)) {
    // `request.delete` resolves on 4xx/5xx, so the status is read, not a catch.
    const response = await page.request
      .delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${pipelineId}`)
      .catch(() => undefined);
    if (response === undefined || (!response.ok() && response.status() !== 404)) {
      test.info().annotations.push({
        type: 'cleanup',
        description: `pipeline ${pipelineId} in project ${projectId} was not deleted (${String(response?.status())})`,
      });
    }
  }
});

/**
 * A pipeline made through the editor, its graph saved, one turn sent in a new
 * chat with it. Returns where the turn is stored.
 */
async function runPipelineTurn(
  page: Page,
  tag: string,
  yaml: string,
): Promise<{ projectId: string; conversationId: string }> {
  const name = `${AUTOTEST_PREFIX}${tag}-${Date.now() % 1_000_000}`;

  const created = page.waitForResponse(
    (r) => APPLICATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.goto(`${BASE_URL}/app/pipelines/create`);
  await expect(page.getByTestId('agent-name-input')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('agent-name-input').fill(name);
  await page.getByTestId('agent-description-input').fill(`${name} description`);
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('pipeline-save-button').click();
  const response = await created;
  expect(response.status(), `the pipeline must be created: ${(await response.text()).slice(0, 300)}`).toBe(201);
  const projectId = APPLICATIONS_RE.exec(new URL(response.url()).pathname)?.[1] ?? '';
  const body = (await response.json()) as { id?: string; version_details?: { id?: string; name?: string } };
  const pipelineId = body.id ?? '';
  const versionId = String(body.version_details?.id ?? '');
  expect(projectId).not.toBe('');
  expect(pipelineId).toMatch(/^\d+$/);
  // Deleted after the journey whether it passed or not (the agents of the
  // `run_code` journeys are cleaned the same way).
  createdPipelines.push({ projectId, pipelineId });
  expect(versionId).not.toBe('');

  const saved = await page.request.put(
    `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${projectId}/${pipelineId}/${versionId}`,
    {
      data: {
        name: body.version_details?.name ?? 'latest',
        agent_type: 'pipeline',
        instructions: yaml,
        conversation_starters: [],
        variables: [],
        meta: { step_limit: 25, internal_tools: [] },
        pipeline_settings: { nodes: [], edges: [] },
      },
    },
  );
  expect(saved.status(), `the graph must reach the version row: ${(await saved.text()).slice(0, 300)}`).toBeLessThan(300);
  await page.reload();

  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await expect(page.getByTestId('chat-with-pipeline-button')).toBeEnabled({ timeout: 30_000 });
  await page.getByTestId('chat-with-pipeline-button').click();
  const conversation = (await (await conversationCreated).json()) as { id?: string | number };
  const conversationId = String(conversation.id ?? '');
  expect(conversationId).not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}$`), { timeout: 30_000 });

  const send = await fillComposer(page, `autotest ${tag} ${Date.now()}`);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 45_000,
  });
  await send.click();
  const startResponse = await started;
  expect(startResponse.status(), `the pipeline turn was refused: ${(await startResponse.text()).slice(0, 400)}`).toBe(200);
  return { projectId, conversationId };
}

test('a pipeline code node runs JavaScript on the real sandbox and its result is the answer', async ({ page }) => {
  test.setTimeout(300_000);
  const { projectId, conversationId } = await runPipelineTurn(page, 'codenode', PIPELINE_YAML);

  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    contains: 'sum:2',
    timeout: 180_000,
    message: 'the code node never produced `sum:2`: the sandbox did not run the node',
  });
});

test('a pipeline code node that throws ends the run with the exception message in the stored error (#2227)', async ({
  page,
}) => {
  test.setTimeout(300_000);
  const { projectId, conversationId } = await runPipelineTurn(page, 'codenodethrow', THROWING_PIPELINE_YAML);

  expect(
    await settleStoredTurn(page, projectId, conversationId, { timeout: 180_000 }),
    'the failed code node never settled the turn',
  ).toBe(true);
  const assistant = (await readStoredTranscript(page, projectId, conversationId)).find(
    (row) => row.role === 'assistant',
  );
  expect(assistant, 'the failed turn must leave an assistant row').toBeDefined();
  expect(assistant?.isError, 'the thrown error must end the turn as an error, never an answer').toBe(true);
  expect(assistant?.metadata['error_code'], 'the run must end with the registered Code failure').toBe(
    'PIPELINE_CODE_FAILED',
  );

  const stored = String(assistant?.metadata['error'] ?? '');
  // The registered sentence still leads: the detail follows it, never replaces it.
  expect(stored, `the stored error must start with the Code failure sentence: ${stored.slice(0, 400)}`).toContain(
    'The pipeline stopped at a Code node.',
  );
  expect(stored, `the stored error must carry the program output section: ${stored.slice(0, 400)}`).toContain(
    'Program output:',
  );
  expect(stored, `the thrown message must reach the stored error: ${stored.slice(0, 800)}`).toContain(THROWN_MESSAGE);
  // The same bound as the model-facing tool: the sentence, the separator, 1,500 characters.
  expect(stored.length, 'the stored error must stay bounded').toBeLessThanOrEqual(256 + 20 + 1_500);
});
