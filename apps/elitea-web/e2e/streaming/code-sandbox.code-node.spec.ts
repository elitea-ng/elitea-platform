/**
 * A pipeline CODE node on the real code sandbox (#2206, #2208).
 *
 * The sibling journey (`code-sandbox.run-code.spec.ts`) proves the agent tool;
 * this one proves the other consumer of the same sandbox, the graph `code`
 * node, through Main's dispatch grant, the worker's node journal and a real
 * runner container. The node computes `1+1` in JavaScript and returns
 * `sum:2`; the pipeline's terminal `answer` is read from the STORE. No model is
 * involved, so nothing here can pass on prose.
 *
 * Needs the stack from scripts/code-sandbox-e2e.sh.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { AUTOTEST_PREFIX, expectStoredAssistantAnswer, fillComposer } from '../fixtures/api';

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

const createdPipelines: { projectId: string; pipelineId: string }[] = [];

test.afterEach(async ({ page }) => {
  for (const { projectId, pipelineId } of createdPipelines.splice(0)) {
    try {
      await page.request.delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${pipelineId}`);
    } catch {
      // Hygiene only; the journey's own failure is the one to read.
    }
  }
});

test('a pipeline code node runs JavaScript on the real sandbox and its result is the answer', async ({ page }) => {
  test.setTimeout(300_000);
  const name = `${AUTOTEST_PREFIX}codenode-${Date.now() % 1_000_000}`;

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
        instructions: PIPELINE_YAML,
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

  const send = await fillComposer(page, `autotest code node ${Date.now()}`);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 45_000,
  });
  await send.click();
  const startResponse = await started;
  expect(startResponse.status(), `the pipeline turn was refused: ${(await startResponse.text()).slice(0, 400)}`).toBe(200);

  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    contains: 'sum:2',
    timeout: 180_000,
    message: 'the code node never produced `sum:2`: the sandbox did not run the node',
  });
});
