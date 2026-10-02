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
 * end is before its start is refused; a healthy agent participant comes back
 * carrying its RESOLVED TOOL LIST; and a participant whose stored settings lost
 * their version still loads — the participant degrades, the conversation does
 * not.
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
 * Canvas authorization precedes range validation. Unknown groups answer 404
 * for valid and inverted ranges. The streaming canvas journey checks the
 * inverted-range 400 against a real message owned by its caller.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE PARTICIPANT'S TOOL LIST IS RESOLVED ON THE READ
 * ─────────────────────────────────────────────────────────────────────────────
 * A participant's `meta` is written once, at attach time, from the entity's
 * display name. The tool list is resolved on every READ instead, from the
 * version the participant's own settings name, because an author changes an
 * agent's toolkits after attaching it and the rail must draw what the agent
 * HAS. The case below attaches a real toolkit and reads it back through
 * `meta.tools`, and the degraded half asserts the other side of the same rule:
 * a participant whose version cannot be resolved is still served, with no tool
 * list rather than no participant.
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
  createGithubToolkit,
  deleteAgent,
  deleteConversation,
  deleteGithubToolkit,
} from '../../fixtures/api';
import { attachToolkitToVersion } from '../../fixtures/exportImport';
import { STORAGE_STATE } from '../../../playwright.config';

import type { GithubToolkitFixture } from '../../fixtures/api';

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

interface ApplicationParticipantRow {
  readonly id?: unknown;
  readonly entity_name?: string;
  readonly entity_meta?: Readonly<Record<string, unknown>>;
  readonly entity_settings?: Readonly<Record<string, unknown>>;
  readonly meta?: Readonly<Record<string, unknown>>;
}

function applicationParticipant(rows: readonly ApplicationParticipantRow[], agentId: string) {
  const row = rows.find((candidate) => candidate.entity_name === 'application' &&
    String(candidate.entity_meta?.['id']) === agentId &&
    String(candidate.entity_meta?.['project_id']) === DEFAULT_PROJECT_ID);
  expect(row, 'the application participant must match its entity and project').toBeDefined();
  return row;
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
 * Unknown message groups answer the same 404 for both range shapes.
 * This preserves the resource boundary before range validation.
 */
test('the canvas route hides unknown and missing message groups before range validation', async ({
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
  ).toBe(404);
  expect(await inverted.json()).toEqual({ error: 'chat resource not found' });

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
  ).toBe(404);
  expect(await unknownMessage.json()).toEqual({ error: 'chat resource not found' });

  const missingGroup = await request.post(CANVASES_PATH, {
    data: {
      message_item_id: 999_999_999,
      name: autotestName('canvas'),
      canvas_content_starts_at: 0,
      canvas_content_ends_at: 5,
    },
  });
  expect(missingGroup.status(), 'an item cannot authorize a canvas without its group').toBe(404);
  expect(await missingGroup.json()).toEqual({ error: 'chat resource not found' });
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
  let toolkit: GithubToolkitFixture | undefined;
  try {
    // A REAL toolkit on the agent's version. Without it the tool-list
    // assertion below would pass against a read that always answered an empty
    // list, which is the shape a missing resolution takes.
    toolkit = await createGithubToolkit(request, DEFAULT_PROJECT_ID, `${agentName}_tk`, {});
    await attachToolkitToVersion(request, toolkit.toolkitId, {
      applicationId: agent.id,
      versionId: agent.versionId,
    });
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
    const attachedRows = (await attached.json()) as readonly ApplicationParticipantRow[];
    const participantId = String(applicationParticipant(attachedRows, agent.id)?.id ?? '');
    expect(participantId).not.toBe('');

    // A healthy participant first, so the degraded read below is a comparison
    // and not an isolated observation.
    const healthy = await readConversation(request, conversationId);
    const healthyRows = (healthy['participants'] as readonly ApplicationParticipantRow[]) ?? [];
    const healthyRow = applicationParticipant(healthyRows, agent.id);
    expect(String((healthyRow?.['entity_settings'] as Record<string, unknown>)?.['version_id'] ?? '')).toBe(
      agent.versionId,
    );
    // `meta` is always an object and carries the display name the rail draws…
    expect(typeof healthyRow?.['meta']).toBe('object');
    const healthyMeta = healthyRow?.['meta'] as Record<string, unknown>;
    expect(healthyMeta?.['name']).toBe(agentName);
    // …and the tool list the read RESOLVES from the participant's version. The
    // toolkit attached above is the one this must name; a stored snapshot taken
    // when the agent was attached could not carry it.
    const tools = healthyMeta?.['tools'] as readonly Record<string, unknown>[] | undefined;
    expect(Array.isArray(tools), `no resolved tool list: ${JSON.stringify(healthyMeta)}`).toBe(true);
    expect(tools ?? []).toHaveLength(1);
    expect(tools?.[0]?.['name']).toBe(`${agentName}_tk`);
    expect(tools?.[0]?.['type']).toBe('github');

    // Now take the version away, exactly as a whole-object replace does.
    const stripped = await request.put(
      `${API_BASE}/elitea_core/entity_settings/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}/${participantId}`,
      { data: { chat_settings: { some_client_setting: true } } },
    );
    expect(stripped.status(), `the settings write was refused: ${(await stripped.text()).slice(0, 200)}`).toBe(200);

    const degraded = await readConversation(request, conversationId);
    const rows = (degraded['participants'] as readonly ApplicationParticipantRow[]) ?? [];
    expect(
      rows,
      'the conversation dropped the participant it could not resolve, instead of degrading it',
    ).toHaveLength(healthyRows.length);
    expect(rows.map((row) => row.id), 'the read must preserve every participant identity').toEqual(
      expect.arrayContaining(healthyRows.map((row) => row.id)),
    );
    const degradedRow = applicationParticipant(rows, agent.id);
    expect(String(degradedRow?.id)).toBe(participantId);
    const settings = degradedRow?.entity_settings;
    expect(
      settings?.['version_id'],
      'the write was meant to REPLACE the document; a merge would have put the version back',
    ).toBeUndefined();
    expect(settings?.['chat_settings'], 'the replacement document is what was stored').not.toBeUndefined();
    // The participant that lost its version carries NO tool list. There is
    // nothing to resolve, and inventing an empty one would tell the rail this
    // agent has no toolkits when the truth is that nobody could tell.
    expect(degradedRow?.meta?.['tools']).toBeUndefined();
    expect(degradedRow?.meta?.['name']).toBe(agentName);
    // …and the rest of the conversation is intact.
    expect(String(degraded['id'] ?? '')).toBe(conversationId);
    expect(String(degraded['uuid'] ?? '')).not.toBe('');
  } finally {
    await deleteConversation(request, conversationId);
    await deleteGithubToolkit(request, DEFAULT_PROJECT_ID, toolkit);
    await deleteAgent(request, agent.id);
  }
});
