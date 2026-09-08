/**
 * The toolkit editor's "Test settings" pane, on the stack — the half of
 * JRNY-017 ("the test result is displayed") that `toolkits.lifecycle.spec.ts`
 * recorded as uncoverable, and the platform half of the legacy public suite's
 * `TestToolkitTestSettings` cases
 * (`qa/elitea-testing-public/automation/tests/ui/toolkits/`, ported for a real
 * provider as `e2e/live/toolkits.*.spec.ts`'s `LIVE-TK-<provider>-1`).
 *
 * ## What this stack can and cannot prove
 *
 * `deploy/docker-compose.e2e-standalone.yml` runs no worker: no
 * `ELITEA_RUNTIME_ENABLED`, no `elitea-worker-python` service. So
 * `toolkits.Handler.TestTool` has no use case composed and answers its own
 * `503 indexer service not available` (`internal/api/v2/toolkitrun/response.go`,
 * `WriteUnavailable`) — an HONEST refusal, and the one this deployment should
 * give.
 *
 * That is enough to state the thing that was actually missing, and it is a
 * composition claim rather than a claim about the worker: the pane exists, it
 * lists the toolkit's own tools, it draws the picked tool's argument form from
 * the served schema, Run reaches the real route with the right body, and
 * whatever the server answers is rendered as its own outcome rather than as a
 * blank panel. What a worker returns for a real tool is the live lane's
 * business, and `LIVE-TK-<provider>-1` asserts it there against a real
 * provider.
 *
 * `artifact` is the type used because it is one the worker image import-verifies
 * (`services/elitea-main/internal/runtimecomposition/
 * current_python_worker_toolkit_capability_snapshot.json` lists the thirteen it
 * does NOT), it needs no credential, and its `list_files` tool has no required
 * argument — so the journey is about the pane and not about filling a form.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

/** The synchronous run route the pane presses Run onto. */
const TEST_TOOL_RE = /\/elitea_core\/test_tool\/prompt_lib\/(\d+)\/([^/?]+)/;

/** Every settled outcome the result panel can carry — `running` is not one of them. */
const SETTLED_STATUSES = ['ok', 'toolError', 'unsupportedToolkit', 'unknownTool', 'timeout', 'failure'];

/** Ids this file created, removed in afterAll. Never a blanket `autotest_` sweep — other specs run concurrently. */
const createdIds: string[] = [];

test.afterAll(async ({ browser }) => {
  if (createdIds.length === 0) return;
  const context = await browser.newContext();
  for (const id of createdIds) {
    await context.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`);
  }
  await context.close();
});

test('J17b: the toolkit editor offers a Test settings pane that runs one tool and shows what came back', async ({ page }) => {
  test.setTimeout(180_000);

  // Created through the API, as the harness rule says: the create form is
  // already covered by JRNY-017 and this journey is about the pane.
  const name = `${AUTOTEST_PREFIX}tp${Date.now()}`;
  const created = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: { name, type: 'artifact', settings: { selected_tools: ['list_files'] } },
  });
  expect(created.status(), `creating the artifact toolkit answered ${created.status()}: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const toolkitId = String(((await created.json()) as { id?: string | number }).id ?? '');
  expect(toolkitId, 'the created toolkit must carry an id').not.toBe('');
  createdIds.push(toolkitId);

  // The server read, polled, before the UI is asked anything about it. There is
  // no GET-single toolkit route (the app derives the detail from this same
  // collection — `pages/toolkits/lib/useToolkitDetail.ts`), so the list is what
  // the screen itself will read.
  await expect
    .poll(
      async () => {
        const list = await page.request.get(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`);
        if (!list.ok()) return false;
        const rows = ((await list.json()) as { rows?: readonly { id?: string | number }[] }).rows ?? [];
        return rows.some((row) => String(row.id) === toolkitId);
      },
      { timeout: 60_000, message: 'the created toolkit never appeared in the collection the editor reads' },
    )
    .toBe(true);

  await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });

  // The pane itself. This used to be an empty `<Box>` with this exact testid —
  // present, and containing nothing — so its mere attachment is NOT the
  // assertion; the controls inside it are.
  const pane = page.getByTestId('edit-toolkit-test-pane-slot');
  await expect(pane).toBeAttached({ timeout: 60_000 });

  const picker = pane.getByRole('combobox').first();
  await expect(picker, 'the Test settings pane must offer a tool picker').toBeVisible({ timeout: 60_000 });
  await picker.click();

  // The toolkit's OWN tool, from its saved `selected_tools` — the picker's
  // first tier. A picker listing every type's tools would show more than one.
  const option = page.getByRole('option', { name: /list files/i });
  await expect(option, 'the picker must offer the toolkit’s own tool').toBeVisible({ timeout: 30_000 });
  await option.click();

  const runRequest = page.waitForRequest((request) => TEST_TOOL_RE.test(new URL(request.url()).pathname) && request.method() === 'POST', {
    timeout: 60_000,
  });
  const runButton = pane.getByRole('button', { name: /run tool/i });
  await expect(runButton, 'a tool with no required argument must be runnable at once').toBeEnabled({ timeout: 30_000 });
  await runButton.click();

  // The request carries the picked tool and names this toolkit — the page, not
  // the pane, is what resolves the project and the toolkit id.
  const request = await runRequest;
  const matched = TEST_TOOL_RE.exec(new URL(request.url()).pathname);
  expect(matched?.[2], 'the run must name the toolkit being edited').toBe(toolkitId);
  expect(request.postDataJSON()).toMatchObject({ tool_name: 'list_files' });

  // …and the answer is rendered as its own outcome. `running` is excluded on
  // purpose: a panel stuck on the spinner is exactly the failure a bare
  // "the panel appeared" assertion would pass.
  const result = pane.getByTestId('test-tool-result');
  await expect(result).toBeVisible({ timeout: 60_000 });
  await expect
    .poll(async () => result.getAttribute('data-status'), {
      timeout: 60_000,
      message: 'the result panel must settle on one of the outcomes the run route can answer',
    })
    .not.toBe('running');
  const status = await result.getAttribute('data-status');
  expect(SETTLED_STATUSES, `the panel reported an outcome nothing writes: ${String(status)}`).toContain(status);
  // Whatever it settled on, it says something. A settled panel with no text is
  // the blank-slot state this journey exists to prevent coming back.
  expect((await result.innerText()).trim().length).toBeGreaterThan(0);
});
