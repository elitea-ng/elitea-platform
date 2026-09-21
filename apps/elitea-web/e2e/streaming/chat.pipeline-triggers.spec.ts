/**
 * THE TWO UNATTENDED PIPELINE ENTRY POINTS, RUNNING FOR REAL (#939 group 1).
 *
 * `e2e/journeys/pipelines/pipelines.webhook-trigger.spec.ts` proves everything
 * about these two facilities that can be proved WITHOUT a runtime: the
 * credential, the carriers, the refusals, the body ceiling, the schedule write
 * contract. It cannot prove that anything RUNS — the journeys stack composes no
 * worker, so `admit()` answers 503 on its first line.
 *
 * This file is the other half, and it is here rather than there for exactly
 * that reason. Three runs of ONE pipeline, started three different ways, and
 * then the question the source cases actually ask: can a person tell them
 * apart afterwards?
 *
 *   webhook  → conversation "Webhook: <pipeline>"
 *   schedule → conversation "Schedule: <pipeline>"
 *   chat     → the conversation the sender named; no origin prefix at all
 *
 * ── HOW THE SCHEDULE IS MADE TO FIRE, WITHOUT WAITING FOR A CRON ─────────
 *
 * `schedulerun.go::timeToRun` opens with `if schedule.LastRun == nil { return
 * true }`, and the tick is registered at `* * * * *`
 * (`job.go::ScheduleJobCadence`). So a schedule that has never run is due on
 * the very NEXT tick whatever its expression says: saving one `active: true`
 * fires it within at most one tick, ~60 s, with no e2e-only endpoint, no
 * env-gated clock and no minute boundary for the test to race. That property
 * is documented once, on `savePipelineSchedule` in
 * `e2e/fixtures/pipelineTriggers.ts`, and relied on here.
 *
 * The tick is only registered inside `cmd/elitea-main/main.go`'s
 * `publicRoutes.AgentStart != nil` block, which is why this is the only stack
 * it can be asserted on.
 *
 * ── THIS FILE NEEDS THE LANE'S `--workers=1` PIN, AND IT IS NOT DECORATIVE ─
 *
 * Three model turns in one test, two of them started by the platform rather
 * than by the browser. `scripts/chat-stream-e2e.sh` passes `--workers=1` for
 * the reason `playwright.config.ts` states at length — four replay streams per
 * principal, two invocations per worker — and this file is the one that shows
 * what happens without it: run `--repeat-each=2 --workers=4` and the SCHEDULED
 * run's answer does not land inside 240 s, because it is queued behind the
 * other copy's turns. That failure is a statement about the harness, not about
 * schedules.
 *
 * ── WHERE THE ASSERTIONS ARE READ ───────────────────────────────────────
 *
 * Server-side, through the public routes, never the canvas: a run started by a
 * webhook has no browser watching it, so the only honest account of it is the
 * transcript the platform stored and the schedule row the platform stamped.
 * The mock echoes the last user message, so each run carries its own marker
 * and an answer that contains it cannot be another run's.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  deleteConversation,
  expectStoredAssistantAnswer,
  fillComposer,
  readCallerPersonalProjectId,
  readStoredTranscript,
} from '../fixtures/api';
import { createPipelineThroughApi, deletePipeline, type CreatedPipeline } from '../fixtures/pipelines';
import {
  createInboundTrigger,
  deletePipelineSchedule,
  readPipelineSchedule,
  revokeInboundTrigger,
  savePipelineSchedule,
  sendWebhook,
  webhookSender,
} from '../fixtures/pipelineTriggers';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** A per-run marker the offline mock echoes back, so an answer can be tied to the run that asked for it. */
function marker(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}${Date.now() % 1_000_000}`;
}

/**
 * The project the runs land in.
 *
 * The chat persona owns its own personal project (#290) and the app selects it,
 * so a pipeline created anywhere else would be started by a trigger whose
 * creator holds no `models.chat.messages.create` there — a 401 that says
 * nothing about triggers.
 */
async function runProjectId(request: APIRequestContext): Promise<string> {
  const personal = await readCallerPersonalProjectId(request);
  expect(personal, 'the chat persona must own a personal project — the seeder grants it (#290)').not.toBe('');
  return personal;
}

/** Conversations in the project whose name is exactly `expected`. */
async function conversationsNamed(
  request: APIRequestContext,
  projectId: string,
  expected: string,
): Promise<readonly { id: string; uuid: string; name: string }[]> {
  // PAGED, not "the first hundred and hope": this lane's other specs leave
  // conversations in the same personal project, and a run that had scrolled off
  // page one would read as a run that never happened. The route offers no name
  // filter, so the pages are walked.
  const found: { id: string; uuid: string; name: string }[] = [];
  for (let offset = 0; offset < 1_000; offset += 100) {
    const response = await request.get(
      `${API_BASE}/elitea_core/conversations/prompt_lib/${projectId}?limit=100&offset=${String(offset)}`,
    );
    expect(response.ok(), `the conversations list must answer: ${String(response.status())}`).toBe(true);
    const body = (await response.json()) as { rows?: readonly unknown[]; items?: readonly unknown[] };
    const rows = body.rows ?? body.items ?? [];
    for (const row of rows) {
      const typed = row as { id?: unknown; uuid?: unknown; name?: unknown };
      const name = typeof typed.name === 'string' ? typed.name : '';
      // BOTH identifiers. The list is keyed by the SERIAL id; the inbound
      // trigger's admission answers the UUID. Comparing one against the other
      // is how "the run I started is the run that is listed" turned into
      // `expected "470380a0-…" received "7"`.
      if (name === expected) found.push({ id: String(typed.id ?? ''), uuid: String(typed.uuid ?? ''), name });
    }
    if (rows.length < 100) break;
  }
  return found;
}

/**
 * Open the pipeline editor's own test chat and MINT its conversation.
 *
 * The chat leg goes through the UI and not through the start route directly,
 * and that is a finding rather than a preference: an API start assembled by
 * hand — a conversation created through its own route plus a participant
 * attached through its own route — is refused
 * `422 {"error":"unsupported_agent_execution"}` by the current-path resolver
 * (`internal/db/queries/agent_chat.sql`), whose ~25 conditions the editor's
 * own pane satisfies and a hand-built pair does not. The two UNATTENDED
 * entry points above need no browser precisely because `run.go` assembles
 * that same shape server-side; the interactive one has no such assembler
 * outside the app.
 *
 * The pane creates the conversation on the first composer interaction rather
 * than on mount, so this is also the only place its id is published.
 */
async function openEditorTestChat(page: Page, pipeline: CreatedPipeline): Promise<string> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  const pane = page.getByTestId('edit-pipeline-test-chat');
  await expect(pane, 'the editor’s test chat must mount').toBeVisible({ timeout: 60_000 });

  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  const input = pane.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 45_000 });
  await input.click();
  const response = await conversationCreated;
  expect(
    response.status(),
    `the pane must create its own conversation: ${(await response.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
  const conversationId = String(((await response.json()) as { id?: string | number }).id ?? '');
  expect(conversationId, 'the pane must create a conversation before it can send').not.toBe('');
  return conversationId;
}

/** Send one message in the editor's test chat and require the START to be ADMITTED. */
async function sendEditorTurn(page: Page, prompt: string): Promise<void> {
  const pane = page.getByTestId('edit-pipeline-test-chat');
  const sendButton = await fillComposer(pane, prompt);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await sendButton.click();
  const startResponse = await started;
  expect(startResponse.status(), `the chat turn was refused: ${(await startResponse.text()).slice(0, 400)}`).toBe(200);
}

/*
 * onetest: ELITEA-0881 (the run half — a valid POST is ADMITTED immediately,
 * without waiting for the pipeline, and the posted body's `input` becomes the
 * pipeline's input), ELITEA-0884 (the fire half — an active schedule fires by
 * itself and stamps its own row), ELITEA-0873 (run history — the Schedule and
 * Webhook runs each carry their origin and are distinguishable from each other
 * and from a chat run), ELITEA-0887 (a chat-message run executes and carries NO
 * trigger origin).
 *
 * ONE test for four cases because all four are claims about the SAME pipeline
 * observed after three runs, and each run costs a real model turn in a lane
 * that runs one worker. Splitting them would triple the runtime to re-establish
 * the same three transcripts.
 */
test('a pipeline runs from a webhook, from its schedule and from chat, and each run says which started it', async ({
  page,
}) => {
  // Three model turns plus a tick wait. The lane's own long journeys sit at
  // 420s; this one adds ≤60s of schedule latency on top of two of them.
  test.setTimeout(600_000);

  const projectId = await runProjectId(page.request);
  const name = `${AUTOTEST_PREFIX}trig-${Date.now() % 1_000_000}`;
  let pipeline: CreatedPipeline | undefined;
  let chatConversationId = '';
  const sender = await webhookSender();

  try {
    pipeline = await createPipelineThroughApi(page.request, name, { projectId });
    const scope = { projectId, versionId: pipeline.versionId };

    /* ── 1. THE WEBHOOK RUN ───────────────────────────────────────────── */
    const trigger = await createInboundTrigger(page.request, scope);
    const webhookMarker = marker('wh');
    const posted = await sendWebhook(sender, trigger, {
      secret: trigger.secret,
      body: JSON.stringify({ input: `Reply to this: ${webhookMarker}` }),
    });
    // 202, not 200: the answer is that the run was ADMITTED. It has not
    // finished and will not finish inside this request — which is the
    // "immediate, non-blocking" half of ELITEA-0881.
    expect(posted.status(), `the webhook must be admitted: ${(await posted.text()).slice(0, 300)}`).toBe(202);
    const admitted = (await posted.json()) as {
      execution_id?: string;
      conversation_id?: string;
      project_id?: number;
      version_id?: number;
      events_url?: string;
    };
    expect(admitted.execution_id, 'the admission names the execution').toBeTruthy();
    expect(admitted.conversation_id, 'the admission names the transcript it created').toBeTruthy();
    expect(String(admitted.project_id), 'the run belongs to the STORED row’s project').toBe(projectId);
    expect(admitted.events_url, 'the admission hands back the stream the caller follows').toBe(
      `/api/v2/executions/${projectId}/${String(admitted.execution_id)}/events`,
    );
    const webhookConversation = String(admitted.conversation_id);

    // The credential is now stamped as used — the one field only the ADMITTED
    // path writes, and the journeys-stack file asserts stays unset after every
    // refusal.
    await expect
      .poll(
        async () => {
          const response = await page.request.get(
            `${API_BASE}/pipeline_triggers/prompt_lib/${projectId}/${pipeline?.versionId ?? ''}`,
          );
          const body = (await response.json()) as { last_used_at?: unknown };
          return typeof body.last_used_at === 'string';
        },
        { timeout: 30_000, message: 'an admitted webhook must stamp the trigger as used' },
      )
      .toBe(true);

    /* ── 2. THE SCHEDULE RUN, fired by the platform’s own tick ────────── */
    // Saved BEFORE the webhook turn is waited out, so the ≤60s tick latency
    // overlaps the model turn instead of following it.
    const scheduleMarker = marker('sch');
    // A cron that will NOT come round again inside this test, and that still
    // fires AT ONCE. `timeToRun` returns true while `last_run` is NULL
    // whatever the expression says, so the first fire is the next tick; after
    // it, `next(last_run)` is the following January and nothing fires again.
    // `* * * * *` would have been due every minute for the rest of the run and
    // the "exactly one scheduled run" assertion below would have counted the
    // test's own remaining minutes.
    await savePipelineSchedule(page.request, scope, {
      cron: '0 3 1 1 *',
      active: true,
      input: `Reply to this: ${scheduleMarker}`,
    });

    // The webhook turn’s own answer, from the store. `contains` is the
    // discriminator: the mock echoes the last user message, so an answer
    // carrying THIS run’s marker cannot be the schedule run’s or a cached one.
    await expectStoredAssistantAnswer(page, projectId, webhookConversation, {
      timeout: 240_000,
      contains: webhookMarker,
      message: 'the webhook run stored no answer — it was admitted and then produced nothing',
    });
    const webhookTranscript = await readStoredTranscript(page, projectId, webhookConversation);
    // The POSTED BODY IS THE INPUT. `run.go::admit` puts `input` in front of
    // the pipeline and nothing else from the request selects anything, so the
    // user row is the proof the body reached the graph.
    expect(webhookTranscript[0]?.role).toBe('user');
    expect(webhookTranscript[0]?.content, 'the webhook body’s `input` IS the pipeline input').toContain(webhookMarker);

    // The schedule fires by itself: `last_run` stamped, `dispatched`, and an
    // execution id of its own. A poll on `last_run` alone would pass on a
    // `skipped_overlap` tick, which is a fire that started nothing.
    await expect
      .poll(
        async () => {
          const schedule = await readPipelineSchedule(page.request, scope);
          return {
            fired: typeof schedule['last_run'] === 'string',
            result: schedule['last_result'] ?? '',
            execution: typeof schedule['last_execution_id'] === 'string',
          };
        },
        {
          timeout: 180_000,
          intervals: [5_000],
          message: 'the schedule never fired — the tick runs every minute and an unfired schedule is due at once',
        },
      )
      .toEqual({ fired: true, result: 'dispatched', execution: true });

    /* ── 3. THE CHAT RUN ──────────────────────────────────────────────── */
    // A plain conversation the sender names itself, with the pipeline as its
    // participant — the Chat Message "trigger", which is the absence of both
    // unattended facilities rather than a configured third one.
    const chatMarker = marker('chat');
    chatConversationId = await openEditorTestChat(page, pipeline);
    await sendEditorTurn(page, `Reply to this: ${chatMarker}`);
    await expectStoredAssistantAnswer(page, projectId, chatConversationId, {
      timeout: 240_000,
      contains: chatMarker,
      message: 'the chat-message run stored no answer',
    });

    /* ── 4. RUN HISTORY: which run was started by what ────────────────── */
    const webhookRuns = await conversationsNamed(page.request, projectId, `Webhook: ${name}`);
    const scheduleRuns = await conversationsNamed(page.request, projectId, `Schedule: ${name}`);
    expect(webhookRuns, 'the webhook run is listed under its own origin').toHaveLength(1);
    expect(scheduleRuns, 'the scheduled run is listed under its own origin').toHaveLength(1);
    // WHICH run is listed, tied to the run this test started — by its
    // CONTENT, not by its id. MEASURED: the conversations list carries the
    // serial id and no `uuid`, while the inbound admission answers the uuid,
    // so the two identifiers cannot be compared at all (`expected
    // "faf9ca5f-…" received ""`). The transcript can: only this run was fed
    // this marker.
    const listedWebhookTranscript = await readStoredTranscript(page, projectId, webhookRuns[0]?.id ?? '');
    expect(
      listedWebhookTranscript.map((row) => row.content).join('\n'),
      'the run listed under Webhook is the run this test started',
    ).toContain(webhookMarker);
    // VISUALLY DISTINGUISHABLE, which is the whole of ELITEA-0873: the two
    // origins are different words on the list, without opening either run.
    expect(webhookRuns[0]?.name).not.toBe(scheduleRuns[0]?.name);
    // And the chat run carries NO origin at all, which is ELITEA-0887's "no
    // trigger badge": it is not one of the two origin-labelled rows. Read from
    // the LIST, so this is a statement about what the platform stored and not
    // about a string this test chose.
    expect(
      [...webhookRuns, ...scheduleRuns].map((row) => row.id),
      'a chat-message run must not be listed under any trigger origin',
    ).not.toContain(chatConversationId);

    // The scheduled run really ran, and ran with the SCHEDULE's input — not
    // the webhook's, and not empty.
    const scheduleConversation = String(scheduleRuns[0]?.id ?? '');
    await expectStoredAssistantAnswer(page, projectId, scheduleConversation, {
      timeout: 240_000,
      contains: scheduleMarker,
      message: 'the scheduled run was dispatched and then stored no answer',
    });
    const scheduleTranscript = await readStoredTranscript(page, projectId, scheduleConversation);
    expect(scheduleTranscript[0]?.content, 'a scheduled run is fed the schedule’s own input').toContain(scheduleMarker);
    expect(scheduleTranscript[0]?.content, 'a scheduled run is not fed the webhook’s input').not.toContain(
      webhookMarker,
    );
  } finally {
    await sender.dispose();
    if (pipeline !== undefined) {
      const scope = { projectId, versionId: pipeline.versionId };
      // The schedule first: a live one fires again every minute and would keep
      // creating transcripts after this test ends.
      await deletePipelineSchedule(page.request, scope);
      await revokeInboundTrigger(page.request, scope);
      for (const origin of ['Webhook', 'Schedule']) {
        for (const row of await conversationsNamed(page.request, projectId, `${origin}: ${name}`)) {
          await deleteConversation(page.request, row.id, projectId);
        }
      }
      await deletePipeline(page.request, { id: pipeline.id, projectId });
    }
    if (chatConversationId !== '') await deleteConversation(page.request, chatConversationId, projectId);
  }
});

/*
 * A pipeline must keep running in a project that has a Project Context in
 * effect — an ordinary product setting, injected into every turn in the
 * project since #946.
 *
 * IT DID NOT, ON EITHER RUNTIME, AND #971 IS WHY. A pipeline's `instructions`
 * field is not prose: it is the YAML graph the runtime compiles
 * (`PipelineDefinition::from_yaml(shell.instructions())`,
 * services/elitea-worker-rust/src/agents/pipeline.rs). #946's injection spliced
 * the `<project_context>` block onto that field, so the document the worker
 * compiled was no longer a graph and assembly failed with
 * `native_agent.invalid_input` — after admission, so the turn was stored as an
 * assistant row flagged `is_error` with EMPTY content and the user saw a failed
 * run with nothing to read. Measured four consecutive failures with a context
 * set and two consecutive passes with it emptied, same fixture, on the native
 * leg, and the same failure on the python leg.
 *
 * The fix is in `appendCurrentApplicationMemories`
 * (services/elitea-main/internal/application/agentexecution/memories.go), the
 * package's ONE splice primitive: a version whose `agent_type` is `pipeline`
 * comes back untouched, which covers the project-context injector that
 * delegates to it and #870's memory injector that is it. The documented limit
 * that remains: a pipeline run carries no project context at all, because the
 * runtime has no per-run slot to put one in.
 *
 * So this asserts the FIXED behaviour: the context is set, the pipeline runs
 * anyway, and its answer is this run's own.
 */
test('a pipeline still runs in a project that has a Project Context in effect', async ({ page }) => {
  test.setTimeout(420_000);

  const projectId = await runProjectId(page.request);
  const name = `${AUTOTEST_PREFIX}ctxpipe-${Date.now() % 1_000_000}`;
  const contextUrl = `${API_BASE}/elitea_core/project_context/prompt_lib/${projectId}/project-context`;
  let pipeline: CreatedPipeline | undefined;
  const sender = await webhookSender();

  try {
    const written = await page.request.put(contextUrl, {
      data: { content: `${AUTOTEST_PREFIX}This project is a Python REST API for order management.`, enabled: true },
    });
    expect(written.ok(), 'the project context must be written — it is the precondition, not the assertion').toBe(true);

    pipeline = await createPipelineThroughApi(page.request, name, { projectId });
    const trigger = await createInboundTrigger(page.request, {
      projectId,
      versionId: pipeline.versionId,
    });
    const contextMarker = marker('ctx');
    const posted = await sendWebhook(sender, trigger, {
      secret: trigger.secret,
      body: JSON.stringify({ input: `Reply to this: ${contextMarker}` }),
    });
    expect(posted.status(), 'the run is admitted — the refusal comes later, on the worker').toBe(202);
    const conversationId = String(((await posted.json()) as { conversation_id?: string }).conversation_id ?? '');

    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 180_000,
      contains: contextMarker,
      message: 'the pipeline run failed while the project carries a context — #971 has regressed',
    });
  } finally {
    // MANDATORY, not tidy: a context left in effect here fails every later
    // pipeline turn in this project, which is the whole subject of #971.
    await page.request.put(contextUrl, { data: { content: '', enabled: true } });
    await sender.dispose();
    if (pipeline !== undefined) {
      await revokeInboundTrigger(page.request, { projectId, versionId: pipeline.versionId });
      await deletePipeline(page.request, { id: pipeline.id, projectId });
    }
  }
});
