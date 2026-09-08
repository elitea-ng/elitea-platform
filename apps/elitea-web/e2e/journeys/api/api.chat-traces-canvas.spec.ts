/**
 * The two reads a reopened conversation makes about what happened inside it —
 * the execution-trace list and one step's detail — the canvas route's refusals,
 * and the participant a conversation must survive.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's trace-step, canvas and malformed-participant cases: a
 * conversation with no agent turns shows no steps rather than an error; asking
 * for a step that does not exist is a clean not-found; a canvas selection whose
 * end is before its start is refused; and a participant whose stored settings
 * lost their version still loads — the participant degrades, the conversation
 * does not.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT IS ASSERTED HERE, AND WHAT IS ASSERTED ELSEWHERE
 * ─────────────────────────────────────────────────────────────────────────────
 * The trace reads have a light half and a heavy half — a listing that carries
 * labels, ordering and the bounded `attrs` sidecar, and a per-step detail that
 * carries `tool_inputs`, `tool_output`, `text` and `thinking`. Which columns
 * each may carry is a property of two SQL projections, and it is pinned where
 * rows can be seeded directly: `internal/api/v2/messagetraces`'s PostgreSQL
 * integration tests state the ordering, the blank-step exclusion, the group and
 * kind filters, the light listing, the heavy detail, and the cross-conversation
 * refusal. Repeating them from a browser would need a runtime plane that writes
 * trace rows, which this stack does not have — no worker, so no turn, so no
 * step.
 *
 * What no Go test can state, and what is asserted here, is the HTTP surface a
 * client actually meets: the envelope an empty conversation answers, the query
 * parameters the route accepts and refuses, the status a missing step gets, and
 * the fact that all of it is reachable with the session a browser holds.
 *
 * The canvas cases are split the same way: the two refusals below are decided
 * BEFORE the route looks at any message, so they hold on a stack that can store
 * none. Carving a real canvas out of a real answer needs a stored assistant
 * message, so it lives in `e2e/streaming/chat.canvasExtraction.spec.ts`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * A DISCLOSED GAP
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy suite also requires a healthy agent participant to come back from
 * the conversation read carrying its RESOLVED TOOL LIST (`meta.tools`). This
 * server resolves no such thing: a participant's `meta` is written once, at
 * attach time, from the entity's display name, and the read hands the stored
 * column back unchanged. Measured against a running deployment, an attached
 * agent's `meta` is `{"name": "<agent name>"}` and nothing else. The case below
 * therefore asserts what the read DOES answer for a healthy participant and
 * says here what it does not, rather than asserting a key that would silently
 * be `undefined`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every case creates its own `autotest_*` conversation and agent and
 * deletes both in a `finally`.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createAgent,
  createConversation,
  deleteAgent,
  deleteConversation,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

const CONVERSATION_PATH = `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;
const PARTICIPANTS_PATH = `${API_BASE}/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}`;
const TRACES_PATH = `${API_BASE}/elitea_core/message_traces/prompt_lib/${DEFAULT_PROJECT_ID}`;
const TRACE_PATH = `${API_BASE}/elitea_core/message_trace/prompt_lib/${DEFAULT_PROJECT_ID}`;
const CANVASES_PATH = `${API_BASE}/elitea_core/canvases/prompt_lib/${DEFAULT_PROJECT_ID}`;

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/** The conversation as its own details route serves it. */
async function readConversation(
  request: APIRequestContext,
  conversationId: string,
): Promise<Record<string, unknown>> {
  const response = await request.get(`${CONVERSATION_PATH}/${conversationId}`);
  expect(
    response.status(),
    `the conversation read failed: ${(await response.text()).slice(0, 200)}`,
  ).toBe(200);
  return (await response.json()) as Record<string, unknown>;
}

/* ── the trace list ───────────────────────────────────────────────────────── */

/**
 * A conversation nobody has run an agent in answers an EMPTY LISTING, not an
 * error and not a null.
 *
 * The distinction the envelope draws is between "not counted" and "counted
 * zero": `total` is null unless `include_total` asks for it, and `rows` is
 * always an array. A client that pages the pin strip reads both, and a route
 * that answered `null` rows for an empty conversation would make every new
 * chat look broken to it.
 */
test('a conversation with no agent turns lists no trace steps', async ({ request }) => {
  const conversationId = await createConversation(request, autotestName('notrace'));
  try {
    const counted = await request.get(`${TRACES_PATH}/${conversationId}?include_total=true`);
    expect(
      counted.status(),
      `the trace listing failed: ${(await counted.text()).slice(0, 200)}`,
    ).toBe(200);
    const withTotal = (await counted.json()) as Record<string, unknown>;
    expect(withTotal['total'], 'include_total was asked for and not answered').toBe(0);
    expect(withTotal['rows']).toEqual([]);

    // Without the parameter the count is deliberately absent rather than 0:
    // the two are different answers and a client can tell them apart.
    const uncounted = await request.get(`${TRACES_PATH}/${conversationId}`);
    expect(uncounted.status()).toBe(200);
    const withoutTotal = (await uncounted.json()) as Record<string, unknown>;
    expect(withoutTotal['total'], 'a listing nobody asked to count reported a count').toBeNull();
    expect(withoutTotal['rows']).toEqual([]);
  } finally {
    await deleteConversation(request, conversationId);
  }
});

/**
 * A filter the route cannot parse is REFUSED, not dropped.
 *
 * Both refusals are about the same failure mode from opposite ends. An unknown
 * `kind` that quietly matched nothing would read to a client as "this
 * conversation has no tool calls". A `message_group_id` that failed to parse
 * and was then ignored — which is what the reference implementation does —
 * would WIDEN the answer to every step in the conversation, and
 * `?message_group_id=undefined` is exactly the string a client produces from an
 * uninitialised variable.
 */
test('the trace listing refuses a filter it cannot honour', async ({ request }) => {
  const conversationId = await createConversation(request, autotestName('tracefilter'));
  try {
    const badKind = await request.get(`${TRACES_PATH}/${conversationId}?kind=nonsense`);
    expect(badKind.status(), 'an unknown kind matched nothing instead of being refused').toBe(400);
    expect(String(((await badKind.json()) as { error?: unknown }).error ?? '')).toContain(
      'unsupported trace-step kind',
    );

    const badGroup = await request.get(
      `${TRACES_PATH}/${conversationId}?message_group_id=undefined`,
    );
    expect(
      badGroup.status(),
      'an unparseable narrowing filter was dropped, which widens the answer instead of refusing it',
    ).toBe(400);
    expect(String(((await badGroup.json()) as { error?: unknown }).error ?? '')).toContain(
      'message_group_id',
    );

    // The two kinds the route DOES serve are accepted on the same conversation,
    // so the refusals above are about the value and not about the parameter.
    for (const kind of ['tool_call', 'thinking_step']) {
      const accepted = await request.get(`${TRACES_PATH}/${conversationId}?kind=${kind}`);
      expect(accepted.status(), `the route refused its own kind ${kind}`).toBe(200);
      expect((await accepted.json())['rows']).toEqual([]);
    }
  } finally {
    await deleteConversation(request, conversationId);
  }
});

/* ── one step's detail ────────────────────────────────────────────────────── */

/**
 * A step that does not exist is a clean 404, and the owning group is REQUIRED.
 *
 * The required parameter is the whole scoping story of this read: the detail
 * matches on the step id AND the group, so a bare step id borrowed from another
 * conversation resolves nothing. Dropping the requirement would leave a numeric
 * id addressing every trace row in the project — which is why a missing
 * `message_group_id` is a 400 and not a wider read.
 *
 * The not-found status is worth pinning on its own: the sibling routes in this
 * area answer 400 for a resource that is not there, and this one deliberately
 * does not.
 */
test('a trace step that does not exist is a clean not-found, and the owning group is required', async ({
  request,
}) => {
  const missing = await request.get(`${TRACE_PATH}/2000000000?message_group_id=1`);
  expect(
    missing.status(),
    `a missing step answered ${missing.status()}: ${(await missing.text()).slice(0, 200)}`,
  ).toBe(404);
  expect(String(((await missing.json()) as { error?: unknown }).error ?? '')).toContain(
    'no such trace step',
  );

  const unscoped = await request.get(`${TRACE_PATH}/2000000000`);
  expect(
    unscoped.status(),
    'a step id with no owning group was served; that id would address every trace in the project',
  ).toBe(400);
  expect(String(((await unscoped.json()) as { error?: unknown }).error ?? '')).toContain(
    'message_group_id is required',
  );
});

/* ── the canvas route's refusals ──────────────────────────────────────────── */

/**
 * A selection whose end is before its start is refused, and so is one that
 * names no message.
 *
 * Both are decided before the route reads anything, which is why they belong to
 * the plain journeys stack: the range check runs first, and the message lookup
 * that follows answers a refusal of its own rather than an error page. The
 * order matters — an inverted range that reached the slice would take the
 * request down instead of refusing it.
 */
test('the canvas route refuses an inverted selection and a message it cannot find', async ({
  request,
}) => {
  const inverted = await request.post(CANVASES_PATH, {
    data: {
      message_group_id: 999_999_999,
      message_item_id: 999_999_999,
      name: autotestName('canvas'),
      canvas_type: 'code',
      canvas_content_starts_at: 10,
      canvas_content_ends_at: 2,
    },
  });
  expect(
    inverted.status(),
    `an inverted range answered ${inverted.status()}: ${(await inverted.text()).slice(0, 200)}`,
  ).toBe(400);
  expect(String(((await inverted.json()) as { error?: unknown }).error ?? '')).toContain(
    'canvas_content_starts_at',
  );

  const unknownMessage = await request.post(CANVASES_PATH, {
    data: {
      message_group_id: 999_999_999,
      message_item_id: 999_999_999,
      name: autotestName('canvas'),
      canvas_type: 'code',
      canvas_content_starts_at: 0,
      canvas_content_ends_at: 5,
    },
  });
  expect(
    unknownMessage.status(),
    `a canvas over a message that does not exist answered ${unknownMessage.status()}`,
  ).toBe(400);
  expect(String(((await unknownMessage.json()) as { error?: unknown }).error ?? '')).toContain(
    'No such message',
  );
});

/* ── the participant a conversation must survive ──────────────────────────── */

/**
 * A participant whose stored settings lost their version must not take the
 * conversation with it.
 *
 * The settings write is a whole-object replace, so a client that resends the
 * document without `version_id` really does remove it — there is no merge to
 * put it back. The conversation read then has an agent participant it cannot
 * resolve a version for, and it used to answer a fabricated "no such
 * conversation" refusal for the whole conversation: the transcript, the other
 * participants and every message became unreachable because one mapping row was
 * incomplete.
 *
 * The rule this states is the one that matters to a user with a broken chat:
 * the PARTICIPANT degrades, the conversation does not.
 */
test('a participant whose settings lost their version still loads with the conversation', async ({
  request,
}) => {
  const conversationId = await createConversation(request, autotestName('broken'));
  const agentName = autotestName('brokenag');
  const agent = await createAgent(request, agentName);
  try {
    const attached = await request.post(`${PARTICIPANTS_PATH}/${conversationId}`, {
      data: [
        {
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: DEFAULT_PROJECT_ID, name: agentName },
          entity_settings: { version_id: agent.versionId },
        },
      ],
    });
    expect(attached.status(), `the attach was refused: ${(await attached.text()).slice(0, 200)}`).toBe(200);
    const participantId = String(((await attached.json()) as readonly { id?: unknown }[])[0]?.id ?? '');
    expect(participantId).not.toBe('');

    // A healthy participant first, so the degraded read below is a comparison
    // and not an isolated observation.
    const healthy = await readConversation(request, conversationId);
    const healthyRow = ((healthy['participants'] as readonly Record<string, unknown>[]) ?? [])[0];
    expect(String((healthyRow?.['entity_settings'] as Record<string, unknown>)?.['version_id'] ?? '')).toBe(
      agent.versionId,
    );
    // `meta` is always an object and carries the display name the rail draws.
    // It does NOT carry a resolved tool list — see the module header.
    expect(typeof healthyRow?.['meta']).toBe('object');
    expect((healthyRow?.['meta'] as Record<string, unknown>)?.['name']).toBe(agentName);

    // Now take the version away, exactly as a whole-object replace does.
    const stripped = await request.put(
      `${API_BASE}/elitea_core/entity_settings/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}/${participantId}`,
      { data: { chat_settings: { some_client_setting: true } } },
    );
    expect(stripped.status(), `the settings write was refused: ${(await stripped.text()).slice(0, 200)}`).toBe(200);

    const degraded = await readConversation(request, conversationId);
    const rows = (degraded['participants'] as readonly Record<string, unknown>[]) ?? [];
    expect(
      rows,
      'the conversation dropped the participant it could not resolve, instead of degrading it',
    ).toHaveLength(1);
    const settings = rows[0]?.['entity_settings'] as Record<string, unknown>;
    expect(
      settings?.['version_id'],
      'the write was meant to REPLACE the document; a merge would have put the version back',
    ).toBeUndefined();
    expect(settings?.['chat_settings'], 'the replacement document is what was stored').not.toBeUndefined();
    // …and the rest of the conversation is intact.
    expect(String(degraded['id'] ?? '')).toBe(conversationId);
    expect(String(degraded['uuid'] ?? '')).not.toBe('');
  } finally {
    await deleteConversation(request, conversationId);
    await deleteAgent(request, agent.id);
  }
});
