/**
 * The NESTED AGENT-AS-TOOL turn: a parent agent with a DIFFERENT child agent
 * attached, driven through a complete turn with that relation in place.
 *
 * `chat.agent-tools.spec.ts` attaches a relation, reads it back, and then
 * DETACHES it before its own chat step, saying in as many words that "an
 * agent-as-tool TURN is a separate journey this stack cannot run yet". This is
 * that journey. It is also the file that settles what that detach implies, and
 * the answer is narrower than it looks:
 *
 * ── WHY THAT FILE HAS TO DETACH, AND THIS ONE DOES NOT ────────────────────
 *
 * That spec attaches an agent to ITSELF (it has only one agent, so the
 * relation's parent and child are the same pair), and a self-referencing
 * relation really does refuse every send with 422
 * `unsupported_agent_execution` — measured. The cause is NOT the freeze
 * failing to build the reference, which is what "the freeze cannot build the
 * stored reference's shape" in that file's comment suggests; it is the
 * nesting walk. `walkCurrentApplicationNesting`
 * (`services/elitea-main/internal/infra/db/repos/agent_nesting.go`) carries a
 * cycle set keyed by `(application_id, version_id)`, and a parent that lists
 * itself repeats its own key one hop down: "circular reference to application
 * N version M".
 *
 * A DISTINCT child never repeats a key, so it is admitted — and this file
 * proves it, which is the whole point of authoring TWO agents. The refusal is
 * a property of the SELF-reference, not of the agent-as-tool feature. Anyone
 * reading that detach as "nested agents are refused" would be wrong, and would
 * have no test to correct them.
 *
 * ── WHAT THIS PINS ────────────────────────────────────────────────────────
 *
 *  1. THE RELATION WRITE, IN THE DIRECTION THE UI SENDS IT. The URL names the
 *     CHILD (`.../application_relation/prompt_lib/{projectId}/{childAppId}/
 *     {childVersionId}` — the spec's own parameter names are
 *     `selected_application_id`/`selected_version_id`) and the BODY names the
 *     parent version being edited. Getting those two backwards writes a
 *     relation the other way round, which still answers 2xx.
 *
 *  2. THE PROJECTION THE FREEZE READS. The parent's version must come back
 *     carrying the child as an `application` tool whose settings hold the
 *     child pair. That projection is what
 *     `freezeCurrentStoredApplicationReference`
 *     (`services/elitea-main/internal/application/agentexecution/tools.go`)
 *     turns into the frozen reference, and it refuses on a surprising number
 *     of fields — a `name`/`toolkit_name` that disagree, a settings object
 *     with anything beyond the two identity keys, or a child `agent_type` that
 *     projects NULL because the stored `(application_id, version_id)` pair
 *     names no row. Every one of those is a 422 that says nothing about the
 *     child.
 *
 *  3. THE TURN IS ADMITTED WITH THE CHILD STILL ATTACHED. This is the
 *     assertion the detach in `chat.agent-tools.spec.ts` makes impossible
 *     there: the send happens with the relation IN PLACE, so the whole
 *     admission path — nesting walk, tier caps, freeze, projection — runs
 *     against a real parent/child pair.
 *
 * ── WHAT IS DELIBERATELY NOT ASSERTED: THE CHILD BEING CALLED ─────────────
 *
 * The nested invocation itself IS observable, and it was measured here — twice
 * — against the native Rust worker driven by a real tool-calling model:
 *
 *   worker: `agent.nested_application.invoke ... application_id=<child>
 *            version_id=<child version> outcome="succeeded"`, bracketed by
 *            `agent_tool_start`/`agent_tool_end` progress frames;
 *   store:  a `chat_message_trace_step` of kind `tool_call` whose `tool_name`
 *           is `elitea_agent_{applicationId}_v_{versionId}`
 *           (`services/elitea-worker-rust/src/agents/application_tools.rs`),
 *           readable from the browser over
 *           `GET /elitea_core/message_traces/prompt_lib/{projectId}/{conversationId}`.
 *
 * It is NOT asserted, because whether it happens is the MODEL's choice, and
 * the model this project runs against is not fixed. `chat-stream` is served by
 * `deploy/mock-llm/server.py`, which answers by echoing the prompt and emits a
 * tool call only for the prompt markers it implements — none of which is a
 * nested agent. So an assertion on the call would be green on a stack pointed
 * at a real model and red in continuous integration, for a reason that says
 * nothing about this feature. Measured directly: three consecutive runs of an
 * earlier draft that DID assert it went pass, pass, fail, and the failing run
 * was the one whose stored answer began `MOCK:`.
 *
 * That leaves this journey asserting ADMISSION with the child attached, which
 * is the half that regressed and the half that is deterministic. To check the
 * invocation by hand, point the stack at a tool-calling model and read the two
 * sources above; do not re-add a conditional assertion here, which would pass
 * by finding nothing on exactly the stack where it cannot find anything.
 *
 * The prompt below still names the child agent explicitly — the tool's
 * generated description carries the child's name ("Delegate a self-contained
 * task to the saved Elitea agent '<name>'"), which is the only handle a prompt
 * has on it — so that a manual run against a real model exercises the path
 * without editing the file.
 *
 * Lives in `streaming/` because a nested turn needs the full standalone stack
 * (`chat-stream` project, `scripts/chat-stream-e2e.sh`): the plain journeys
 * stack has no worker and no model.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgentThroughForm,
  expectStoredAssistantAnswer,
  sendComposerMessage,
  settleStoredTurn,
} from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

test('a parent agent with a different child agent attached completes a turn', async ({ page }) => {
  // Two agents, a relation, an admission, a model call, a NESTED model call
  // and the stream back. Every wait below is bounded well under this, so a
  // real hang fails on its own step rather than on the clock.
  test.setTimeout(300_000);

  // One stamp for both, so a leftover pair from a failed run is obviously one
  // pair. Both names must survive the form's 32-character `maxLength` — see
  // `createAgentThroughForm`, which asserts it rather than assuming it.
  const stamp = Date.now() % 1_000_000;
  const childName = `${AUTOTEST_PREFIX}child-${stamp}`;
  const parentName = `${AUTOTEST_PREFIX}parent-${stamp}`;

  // ── 1. Two agents, through the FORM ────────────────────────────────────
  // Through the form for the reason `chat.agent.spec.ts` gives in full: the
  // defects these journeys exist for were in what the form SEEDS, and a
  // version created by the API fixture carries whatever that fixture types.
  // The child first, so the browser is left on the PARENT's edit page — which
  // is where the Chat button this test needs lives.
  const child = await createAgentThroughForm(page, childName);
  const parent = await createAgentThroughForm(page, parentName);
  // The distinction this whole file is about, stated where it is established.
  expect(child.agentId, 'the child must be a DIFFERENT agent — a self-reference is refused by the cycle guard').not.toBe(
    parent.agentId,
  );
  expect(parent.projectId, 'both agents must live in the same project for the relation to resolve').toBe(child.projectId);
  const projectId = parent.projectId;

  // ── 2. Attach the child to the parent ──────────────────────────────────
  // Through the API rather than the "+ Agent" picker: the pick emits exactly
  // this PATCH (`useAgentPipelineAssociation` sends `has_relation: true`), and
  // the direction is the part worth pinning — CHILD in the URL, PARENT in the
  // body. See defect class 1 in the header.
  const relation = await page.request.patch(
    `${BASE_URL}/api/v2/elitea_core/application_relation/prompt_lib/${projectId}/${child.agentId}/${child.versionId}`,
    {
      data: {
        application_id: Number(parent.agentId),
        version_id: Number(parent.versionId),
        has_relation: true,
      },
    },
  );
  expect(
    relation.status(),
    `the child must attach to the parent version: ${(await relation.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  // ── 3. The projection the freeze reads ─────────────────────────────────
  // Read back off the PARENT, because a relation stored against the wrong
  // version — or against the child — leaves this list empty while the PATCH
  // above still answered 2xx.
  const stored = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${parent.agentId}`,
  );
  const detail = (await stored.json()) as {
    version_details?: {
      tools?: readonly {
        type?: string;
        name?: string;
        config?: { application_id?: number; application_version_id?: number };
      }[];
    };
  };
  const nested = (detail.version_details?.tools ?? []).find((tool) => tool.type === 'application');
  expect(
    nested,
    'the parent version must project the child as an `application` tool — without this row the freeze has nothing to build and the turn is refused',
  ).toBeDefined();
  // The identity pair, not just "a row exists": the freeze reads exactly these
  // two, and a projection naming the wrong version makes the child's
  // `agent_type` resolve to NULL, which refuses the turn with a message about
  // an execution path.
  expect(String(nested?.config?.application_id ?? ''), 'the reference must name the child application').toBe(
    child.agentId,
  );
  expect(String(nested?.config?.application_version_id ?? ''), 'the reference must name the child version').toBe(
    child.versionId,
  );
  // `name` is read, not synthesised, by the relation write — and the freeze
  // refuses a reference whose `name` and `toolkit_name` disagree, so a wrong
  // name here is a 422 later.
  expect(nested?.name, 'the reference must carry the child agent’s own name').toBe(childName);

  // ── 4. Chat with the PARENT, relation still attached ───────────────────
  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversation = (await (await conversationCreated).json()) as { id?: string | number };
  const conversationId = String(conversation.id ?? '');
  expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}$`), { timeout: 30_000 });

  // The parent, and the author the resolver's INNER JOIN needs. Asserted here
  // rather than trusted so a 422 below cannot be blamed on a missing user row.
  const conversationRead = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`,
  );
  const conversationBody = (await conversationRead.json()) as {
    participants?: readonly { entity_name?: string; entity_settings?: { version_id?: string | number } }[];
  };
  const participants = conversationBody.participants ?? [];
  expect(
    participants.map((p) => p.entity_name),
    'the resolver joins the author through the user mapping',
  ).toContain('user');
  expect(
    String(participants.find((p) => p.entity_name === 'application')?.entity_settings?.version_id ?? ''),
    'the participant must name the PARENT version — the version the relation was written against',
  ).toBe(parent.versionId);

  // ── 5. Send, and require the turn to be ADMITTED ───────────────────────
  // The prompt names the child agent, because the tool's generated description
  // is the only place that name appears to the model. Nothing below asserts
  // that the model took the offer — see "WHAT IS DELIBERATELY NOT ASSERTED" in
  // the header; the prompt is written this way so a manual run against a
  // tool-calling model exercises the delegation without editing the file.
  const startResponse = await sendComposerMessage(page,
    `Delegate to the saved Elitea agent named ${childName}: ask it for the single word "ready". ` +
      'You must call that agent as a tool, then report exactly what it returned.',
    START_RE,
  );
  // Spelled out because 422 `unsupported_agent_execution` is what a
  // self-reference produces here, and what a broken relation write, a bad
  // projection or a failed freeze all produce too — the body is the sentence a
  // maintainer will search for, and it names none of them.
  expect(
    startResponse.status(),
    `the turn was refused with a DISTINCT child attached — that is not the cycle guard: ${(await startResponse.text()).slice(0, 400)}`,
  ).toBe(200);

  // ── 6. A stored answer, not an error card ──────────────────────────────
  // The STORED reply, not the on-screen bubble — the `IS_ERROR` discrimination
  // and why nothing on screen can stand in for it are in
  // `expectStoredAssistantAnswer`. A nested child that fails mid-turn lands
  // here as an `is_error` row, which is exactly what this rules out.
  //
  // The turn is settled whatever the assertion decides (#2181): a failure here
  // used to leave it running, and the next journey's start POST queued behind
  // it until its own admission bound expired.
  try {
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 120_000,
      message: 'the nested turn stored no answer — the parent was admitted and then failed while delegating',
    });
  } finally {
    await settleStoredTurn(page, projectId, conversationId);
  }

  // Cleanup is best-effort and deliberately last: a failure above should leave
  // both agents, and the relation between them, in place for inspection. The
  // parent first, so the relation row goes with it rather than being orphaned
  // by a child delete.
  await page.request.delete(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${parent.agentId}`,
  );
  await page.request.delete(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${child.agentId}`,
  );
});
