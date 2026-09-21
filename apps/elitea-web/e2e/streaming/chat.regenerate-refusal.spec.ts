/**
 * A REGENERATION THE SERVER CANNOT HONOUR (#939 group 8).
 *
 * ELITEA-0381 and ELITEA-0382 are the same claim with two different triggers:
 * something the agent depended on is taken away between the answer and the
 * Regenerate click — a toolkit credential (0381), or the published version
 * itself (0382) — and the product must then
 *
 *   1. TELL the person, with an error toast, and
 *   2. KEEP the previous answer on screen: no blank, no empty state.
 *
 * Rule 2 is the one that actually protects the user, and it is the one a
 * client that optimistically clears the answer before the request lands gets
 * wrong. It is asserted here against the STORE as well as the screen, because
 * a client that blanked the bubble while the row survived and a server that
 * destroyed the row are the same picture and very different defects.
 *
 * ── WHAT THIS FILE MEASURED, AND WHY IT DOES NOT CLAIM 0381 ──────────────
 *
 * Whether unpublishing actually makes the regeneration fail was a question
 * about the platform, not a premise — and the answer is NO. A withdrawal
 * REVERTS the published clone rather than deleting it
 * (`agents.publishing.spec.ts`), so the version this conversation is bound to
 * still resolves, and the regeneration is accepted and completes normally.
 * That is a defensible product choice — an open conversation keeps working
 * after its agent leaves the catalogue, which is also what #972's preserved
 * half asks for — and it makes ELITEA-0382's step 5 unreachable through this
 * trigger.
 *
 * So this test asserts the branch the platform actually takes, and ARMS the
 * refused branch rather than deleting it: if a future change starts refusing
 * these regenerations, the toast-and-preserve rules the cases describe are
 * already written and will be enforced from that run on.
 *
 * ELITEA-0381 is NOT claimed here. Its trigger is different in kind — a
 * deleted toolkit credential makes the server fail while RESOLVING the
 * toolkit, which is a real 5xx path rather than a still-resolvable version —
 * and reaching it needs a GitHub toolkit, a stored credential and a turn that
 * really invokes it. It stays on the ledger with that recipe.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  deleteAgent,
  expectStoredAssistantAnswer,
  fillComposer,
  PUBLISHABLE_TAGS,
  readCallerPersonalProjectId,
  readStoredTranscript,
  resolveCatalogueProjectId,
  unpublishAllVersions,
} from '../fixtures/api';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const REGENERATE_RE = /\/elitea_core\/regenerate\/prompt_lib\/(\d+)\/([0-9a-f-]+)$/;

const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

function uniqueToken(tag: string): string {
  return `${tag}${Date.now().toString(36)}${Math.floor(Math.random() * 46_656).toString(36)}`;
}

/** Every toast on screen — the union of the MUI Snackbar/Alert content this app's toast layer renders. */
async function toastTexts(page: Page): Promise<readonly string[]> {
  return page.getByRole('alert').allTextContents();
}

/*
 * onetest: ELITEA-0382 — the agent is unpublished between the answer and the
 * Regenerate click. MEASURED: the regeneration is accepted and completes
 * cleanly (see the header), so what is asserted is that behaviour plus the
 * silence ELITEA-0380 requires of a successful run; the toast-and-preserve
 * rules the case states are kept, armed, on the refused branch.
 */
test('a regeneration after the agent is unpublished still completes, silently — and a refused one would keep the answer and say so', async ({ page }) => {
  // One model call plus a publish, an unpublish and the regeneration round trip.
  test.setTimeout(420_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'the chat persona must own a personal project (#290)').not.toBe('');

  // The publish hard-check reads `llm_settings.model_project_id` two ways and
  // both are Criticals: ABSENT is "settings are incomplete", present-and-not-
  // the-catalogue is `llm_not_shared` (#908). So a publishable agent must pin
  // the CATALOGUE project's model — measured here as a 400
  // `validation_failed … missing model_project_id` on the first run.
  const catalogueProjectId = await resolveCatalogueProjectId(page.request);
  expect(
    projectId,
    'the author project must not be the catalogue project, or there is no publish to withdraw',
  ).not.toBe(catalogueProjectId);

  const token = uniqueToken('rgf');
  const agentName = `${AUTOTEST_PREFIX}rgf-${String(Date.now()).slice(-7)}`;
  let agentId = '';

  try {
    const agent = await createAgentWithVersion(
      page.request,
      agentName,
      {
        instructions:
          'You are a regeneration fixture. Echo back exactly what the user gives you and nothing else.',
        welcomeMessage: 'Give me something to echo.',
        conversationStarters: ['Echo this.'],
        model: { modelName: MOCK_MODEL, modelProjectId: catalogueProjectId },
        meta: { step_limit: 25, internal_tools: [] },
        tags: PUBLISHABLE_TAGS,
      },
      projectId,
      `${AUTOTEST_PREFIX}regeneration refusal fixture`,
    );
    agentId = agent.id;

    const published = await page.request.post(
      `${API_BASE}/elitea_core/publish/prompt_lib/${projectId}/${agent.versionId}`,
      { data: { version_name: `rel${String(Date.now()).slice(-6)}` } },
    );
    expect(published.status(), `the publish was refused: ${(await published.text()).slice(0, 300)}`).toBe(200);
    const publicVersionId = String(((await published.json()) as { public_version_id?: unknown }).public_version_id ?? '');
    expect(publicVersionId, 'the publish must answer the public version id').not.toBe('');

    // ── One completed turn with the PUBLISHED agent ───────────────────────
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

    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    const sendButton = await fillComposer(page, `Echo exactly: ${token}`);
    await sendButton.click();
    expect((await started).status(), 'the turn to be regenerated was itself refused').toBe(200);

    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 180_000,
      contains: token,
      message: 'there is no answer to regenerate — the first turn never stored one',
    });
    const before = await readStoredTranscript(page, projectId, conversationId);
    expect(before.map((row) => row.role)).toEqual(['user', 'assistant']);
    const answerBefore = before[1]?.content ?? '';
    expect(answerBefore, 'the stored answer must carry this turn’s marker').toContain(token);

    // ── The thing the agent depended on is taken away ─────────────────────
    const withdrawn = await page.request.post(
      `${API_BASE}/elitea_core/unpublish/prompt_lib/${projectId}/${publicVersionId}`,
      { data: {} },
    );
    expect(withdrawn.status(), `the unpublish was refused: ${(await withdrawn.text()).slice(0, 300)}`).toBe(200);

    // ── Regenerate ────────────────────────────────────────────────────────
    const toastsBefore = await toastTexts(page);
    const answerCard = page.getByTestId('application-answer').first();
    await answerCard.getByTestId('skill-test-last-response').hover();
    const regenerate = answerCard.getByRole('button', { name: 'Regenerate' });
    await expect(regenerate, 'a completed answer must offer Regenerate').toBeVisible({ timeout: 20_000 });

    const answered = page.waitForResponse(
      (r) => REGENERATE_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST' && r.status() !== 409,
      { timeout: 90_000 },
    );
    await regenerate.click();
    const response = await answered;
    const status = response.status();

    // THE BRANCH IS TAKEN FIRST, and reading the transcript before it was the
    // first draft's mistake: an ACCEPTED regeneration clears the answer row
    // while it re-runs, so "the previous answer is still there" read a
    // half-second into a successful run fails for the one reason the cases are
    // not about. The preservation rule belongs to the REFUSED path; the
    // accepted path has its own rule, which is ELITEA-0380's.
    if (status === 200) {
      // MEASURED OUTCOME, recorded rather than asserted away: a withdrawal
      // REVERTS the published clone instead of deleting it
      // (`agents.publishing.spec.ts`), so the version this conversation is
      // bound to still resolves and the regeneration is honoured. That is a
      // defensible product choice — an open conversation keeps working — and
      // it makes these two cases' step 5 unreachable through this trigger.
      // What must then hold is that the run completes cleanly and silently.
      await expectStoredAssistantAnswer(page, projectId, conversationId, {
        timeout: 180_000,
        contains: token,
        message: 'the regeneration was accepted after the unpublish and then produced nothing',
      });
      const after = await readStoredTranscript(page, projectId, conversationId);
      expect(
        after.map((row) => row.role),
        'regenerating must not add or remove a row',
      ).toEqual(['user', 'assistant']);
      expect(after[1]?.isError, 'the regenerated answer must not be stored as a refusal').toBe(false);
      const raised = (await toastTexts(page)).filter((text) => !toastsBefore.includes(text));
      expect(
        raised,
        `the regeneration succeeded after the unpublish, so it must raise no error toast: ${JSON.stringify(raised)}`,
      ).toEqual([]);
      return;
    }

    // ── The refused path: tell the person, and keep what was there ────────
    await expect
      .poll(async () => (await toastTexts(page)).filter((text) => !toastsBefore.includes(text)).length, {
        timeout: 20_000,
        message: `the regeneration was refused with ${String(status)} and nothing told the user`,
      })
      .toBeGreaterThan(0);

    // Read from the STORE as well as the screen: a client that blanked the
    // bubble over an intact row and a server that destroyed the row are the
    // same picture and very different defects.
    const after = await readStoredTranscript(page, projectId, conversationId);
    expect(
      after.map((row) => row.role),
      'a refused regeneration must not add or remove a row',
    ).toEqual(['user', 'assistant']);
    expect(
      after[1]?.content ?? '',
      'the previous answer must still be there — a refused regeneration may not blank it',
    ).toContain(token);
    await expect(
      page.getByTestId('application-answer').first(),
      'the answer card must still be rendered after a refused regeneration',
    ).toBeVisible({ timeout: 15_000 });
  } finally {
    if (agentId !== '') {
      await unpublishAllVersions(page.request, agentId).catch(() => undefined);
      await deleteAgent(page.request, agentId).catch(() => undefined);
    }
  }
});
