/**
 * The chat CRUD contract: what a conversation write answers, what filing and
 * unfiling one does to it, and what a message request that this deployment
 * cannot run is allowed to say.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's conversation-lifecycle and message-request cases:
 * filing a conversation into a chat folder and taking it back out, renaming one
 * while keeping it private, and the three refusals a message POST owes its
 * caller — an out-of-range wait timeout, a conversation that does not exist,
 * and a participant that is not in the conversation.
 *
 * Only the CRUD half is here. The refusal half needs the route to be MOUNTED,
 * and this stack does not mount it (see the next section), so the two refusal
 * bodies are asserted in `e2e/streaming/chat.messageRefusals.spec.ts` — the
 * `chat-stream` project, whose stack composes the runtime plane. What is left
 * here is the statement that only THIS stack can make, and it is worth making:
 * a deployment with no runtime plane must refuse a send outright rather than
 * answer a task handle for a turn nobody will ever run.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE STACK FACT EVERY CASE BELOW IS WRITTEN AGAINST
 * ─────────────────────────────────────────────────────────────────────────────
 * `POST /elitea_core/messages/prompt_lib/{p}/{conversationUUID}` is registered
 * by `internal/api/production_router.go` only when `cfg.CurrentAgentStart` is
 * composed, and that happens only when the runtime plane is. The journeys stack
 * has no worker and no runtime plane, so the path exists for its GET (the
 * transcript read) and for no POST at all, and chi answers the verb it does not
 * serve. That is one refusal for all three legacy cases, which is exactly why
 * the discriminating assertions are not attempted here: three inputs that
 * cannot produce three answers cannot be told apart, and a journey that
 * pretended otherwise would be reporting the stack's configuration as the
 * product's contract.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every case creates its own `autotest_*` conversation, folder and
 * agent through the API and removes them in a `finally`; no case reads another
 * case's rows or depends on the order they run in.
 */
import { randomUUID } from 'node:crypto';

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
const CONVERSATIONS_PATH = `${API_BASE}/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}`;
const FOLDERS_PATH = `${API_BASE}/elitea_core/folder/prompt_lib/${DEFAULT_PROJECT_ID}`;
const PARTICIPANTS_PATH = `${API_BASE}/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}`;
const MESSAGES_PATH = `${API_BASE}/elitea_core/messages/prompt_lib/${DEFAULT_PROJECT_ID}`;

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/** One chat folder, created and answered by the folder route. */
async function createFolder(request: APIRequestContext, name: string): Promise<string> {
  const response = await request.post(FOLDERS_PATH, { data: { name } });
  expect(response.status(), `the folder was not created: ${(await response.text()).slice(0, 200)}`).toBe(201);
  const id = ((await response.json()) as { id?: unknown }).id;
  expect(String(id ?? ''), 'the folder create must answer an id').toMatch(/^\d+$/);
  return String(id);
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

/* ── filing and unfiling ──────────────────────────────────────────────────── */

/**
 * `folder_id` is a top-level key of the conversation PATCH, and `null` is a
 * value it accepts.
 *
 * The filing half is driven through the sidebar by `chat.folders.spec.ts`; the
 * UNFILING half has no control of its own — the product performs it by dragging
 * a conversation back out of a folder — and it is the half with a distinct
 * server behaviour, because "no folder" is a NULL column and not an id. The
 * handler maps a JSON `null` to an empty string and the repository turns that
 * into `folder_id = NULL`, so a write that merely dropped the key (the usual
 * "absent means leave alone" rule this same handler applies to `meta` and
 * `is_private`) would file the conversation forever.
 */
test('a conversation is filed into a folder and unfiled again', async ({ request }) => {
  const conversationId = await createConversation(request, autotestName('filed'));
  const folderId = await createFolder(request, autotestName('folder'));
  try {
    const filed = await request.put(`${CONVERSATION_PATH}/${conversationId}`, {
      data: { folder_id: folderId },
    });
    expect(filed.status(), `filing was refused: ${(await filed.text()).slice(0, 200)}`).toBe(200);
    // The WRITE's own echo describes the conversation it just changed, so a
    // client that refreshes its cache from the mutation response sees the move.
    expect((await filed.json())['folder_id']).toBe(folderId);
    expect((await readConversation(request, conversationId))['folder_id']).toBe(folderId);

    const unfiled = await request.put(`${CONVERSATION_PATH}/${conversationId}`, {
      data: { folder_id: null },
    });
    expect(unfiled.status(), `unfiling was refused: ${(await unfiled.text()).slice(0, 200)}`).toBe(200);
    // The write's own struct omits the key entirely for a conversation in no
    // folder, so the two shapes an unfiled conversation can take are read
    // together here; what must not survive is the folder id.
    expect(
      (await unfiled.json())['folder_id'] ?? null,
      'a null folder_id must clear the column, not be read as "leave it alone"',
    ).toBeNull();

    // Read back through the details route, which is a different projection from
    // the write echo — it always carries the key, as an explicit null — and the
    // echo is the half a test can be fooled by.
    expect((await readConversation(request, conversationId))['folder_id']).toBeNull();
  } finally {
    await deleteConversation(request, conversationId);
    await request.delete(`${FOLDERS_PATH}/${folderId}`);
  }
});

/* ── the partial update answers the whole conversation ────────────────────── */

/**
 * A two-key PATCH answers every field the details route answers.
 *
 * The client applies this response to its store instead of re-reading, so an
 * update that echoed only the keys it was given would blank the conversation's
 * owner, folder and privacy in the UI until the next full load. `created_by`
 * is named here because it is the field that really did come back empty on
 * every update at one point.
 */
test('a partial update answers the whole conversation, not just the keys it was sent', async ({
  request,
}) => {
  const original = autotestName('rename');
  const conversationId = await createConversation(request, original);
  try {
    const before = await readConversation(request, conversationId);

    const renamed = `${original}_v2`;
    const response = await request.put(`${CONVERSATION_PATH}/${conversationId}`, {
      data: { name: renamed, is_private: true },
    });
    expect(response.status(), `the update was refused: ${(await response.text()).slice(0, 200)}`).toBe(200);
    const body = (await response.json()) as Record<string, unknown>;

    expect(body['name']).toBe(renamed);
    expect(body['is_private'], 'the conversation stays private').toBe(true);
    expect(String(body['id'] ?? '')).toBe(conversationId);
    expect(String(body['uuid'] ?? ''), 'the update answers the uuid the details route answers').toBe(
      String(before['uuid'] ?? ''),
    );
    expect(
      String(body['created_by'] ?? ''),
      'the update must name the owner; a client that refreshes its cache from this response loses it otherwise',
    ).toBe(String(before['created_by'] ?? ''));
    expect(
      body['folder_id'] ?? null,
      'an update that names no folder must not invent one',
    ).toBeNull();

    // Durable, not echoed: the read is the only thing that goes back to the row.
    // The details projection states every key explicitly, so `meta` and
    // `is_private` are asserted there rather than against the write's struct,
    // which omits an empty document.
    const after = await readConversation(request, conversationId);
    expect(after['name']).toBe(renamed);
    expect(after['is_private']).toBe(true);
    expect(after['meta'], 'the details read always carries meta, as an object').not.toBeUndefined();
    expect(typeof after['meta']).toBe('object');
  } finally {
    await deleteConversation(request, conversationId);
  }
});

/* ── a conversation create answers both identifiers ───────────────────────── */

/**
 * Both keys, because the product addresses one conversation by two of them: the
 * serial `id` is what every conversation route takes in its path, and the
 * `uuid` is what the message route takes. A create that answered only one would
 * leave a client unable to start a turn in the conversation it just made.
 */
test('a conversation create answers a serial id and a uuid, and starts empty', async ({ request }) => {
  const name = autotestName('identity');
  const response = await request.post(CONVERSATIONS_PATH, { data: { name } });
  expect(response.status(), `the create was refused: ${(await response.text()).slice(0, 200)}`).toBe(201);
  const created = (await response.json()) as Record<string, unknown>;
  const conversationId = String(created['id'] ?? '');
  try {
    expect(conversationId).toMatch(/^\d+$/);
    expect(String(created['uuid'] ?? '')).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
    );
    expect(created['name']).toBe(name);
    expect(created['message_count'], 'a new conversation holds no messages').toBe(0);

    // The details route must resolve the SAME conversation from either
    // identifier — the uuid path used to be a 500.
    const byUuid = await readConversation(request, String(created['uuid']));
    expect(String(byUuid['id'] ?? '')).toBe(conversationId);
    expect((byUuid['participants'] as unknown[]) ?? [], 'a new conversation has no participants').toEqual([]);
  } finally {
    await deleteConversation(request, conversationId);
  }
});

/* ── the participant a message would be addressed to ──────────────────────── */

/**
 * The project id an attach stores when the caller left it out.
 *
 * A participant's identity is keyed on `entity_name` AND `entity_meta`, so the
 * same agent attached once with a `project_id` and once without would become
 * two participants in one conversation. The handler therefore fills the path's
 * project id into `entity_meta` before the row is written, and this is the
 * assertion that it reaches the STORED document rather than only the echo.
 *
 * It matters beyond hygiene: the turn resolver reads
 * `entity_meta.project_id` to decide which tenant schema the agent's version is
 * read from, and the entity-settings rule reads it to decide whether an
 * override is allowed at all (`api.chat-entity-settings.spec.ts`).
 */
test('an attached agent participant records the project it belongs to', async ({ request }) => {
  const conversationId = await createConversation(request, autotestName('attach'));
  const agentName = autotestName('attachag');
  const agent = await createAgent(request, agentName);
  try {
    const attached = await request.post(`${PARTICIPANTS_PATH}/${conversationId}`, {
      data: [
        {
          entity_name: 'application',
          // No project_id: the handler must supply the path's.
          entity_meta: { id: agent.id, name: agentName },
          entity_settings: { version_id: agent.versionId },
        },
      ],
    });
    expect(attached.status(), `the attach was refused: ${(await attached.text()).slice(0, 200)}`).toBe(200);

    const stored = await readConversation(request, conversationId);
    const participants = (stored['participants'] as readonly Record<string, unknown>[]) ?? [];
    expect(participants, 'the attach must be visible through the conversation read').toHaveLength(1);
    const entityMeta = participants[0]?.['entity_meta'] as Record<string, unknown>;
    expect(String(entityMeta?.['id'] ?? '')).toBe(agent.id);
    expect(
      String(entityMeta?.['project_id'] ?? ''),
      'the stored participant carries no project id, so the same agent attached twice becomes two participants',
    ).toBe(DEFAULT_PROJECT_ID);

    // Attaching the SAME agent again is idempotent — the identity lookup runs
    // before the insert, and the mapping's unique key catches the rest.
    const again = await request.post(`${PARTICIPANTS_PATH}/${conversationId}`, {
      data: [
        {
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: DEFAULT_PROJECT_ID, name: agentName },
          entity_settings: { version_id: agent.versionId },
        },
      ],
    });
    expect(again.status()).toBe(200);
    expect(
      ((await readConversation(request, conversationId))['participants'] as unknown[]) ?? [],
      'the same agent attached twice must stay one participant',
    ).toHaveLength(1);
  } finally {
    await deleteConversation(request, conversationId);
    await deleteAgent(request, agent.id);
  }
});

/* ── the message request, on a deployment that cannot run one ─────────────── */

/**
 * A send this deployment cannot serve must REFUSE, and must not look served.
 *
 * The legacy suite makes three separate statements here — a wait timeout
 * outside its bounds, a conversation that does not exist, and a participant
 * that is not in the conversation — and each has its own body on a deployment
 * that mounts the route. This stack mounts no POST at all (module header), so
 * all three arrive at the same refusal and the file header explains where the
 * discriminating half is asserted.
 *
 * What IS provable here, and is not provable anywhere else in the suite, is
 * the property that makes the absence safe: the refusal carries no task handle
 * and writes no message. A route that answered `{"task_id": …}` for a turn that
 * will never run would leave every client polling an execution that does not
 * exist, and one that stored the question alone would leave the user's own
 * words in a conversation with no answer coming.
 */
test('a message request is refused, answers no task handle, and stores nothing', async ({
  request,
}) => {
  const conversationId = await createConversation(request, autotestName('send'));
  try {
    const conversation = await readConversation(request, conversationId);
    const conversationUuid = String(conversation['uuid'] ?? '');
    expect(conversationUuid, 'the message route addresses a conversation by uuid').not.toBe('');

    const sends: readonly { readonly what: string; readonly uuid: string; readonly participantId: number }[] = [
      { what: 'a wait timeout outside its bounds', uuid: conversationUuid, participantId: 1 },
      {
        what: 'a conversation uuid that names nothing',
        uuid: '00000000-0000-4000-8000-000000000000',
        participantId: 1,
      },
      { what: 'a participant that is in no conversation', uuid: conversationUuid, participantId: 99999 },
    ];

    for (const send of sends) {
      const response = await request.post(
        `${MESSAGES_PATH}/${send.uuid}?execution_contract=agent.execute.application.v1`,
        {
          data: {
            project_id: Number(DEFAULT_PROJECT_ID),
            conversation_uuid: send.uuid,
            participant_id: send.participantId,
            // A start body is well formed only with a question id, and it must
            // be a uuid. The bodies here are the ones the streaming sibling
            // sends, so the two files differ in the STACK and not in the input.
            question_id: randomUUID(),
            await_task_timeout: -20,
            payload: { user_input: `${AUTOTEST_PREFIX}refused` },
          },
        },
      );
      const text = (await response.text()).slice(0, 300);
      expect(
        response.ok(),
        `${send.what}: the send was ACCEPTED on a deployment with no runtime plane — ${response.status()} ${text}`,
      ).toBe(false);
      expect(
        text,
        `${send.what}: the refusal named a task to poll for a turn nobody will run`,
      ).not.toContain('task_id');
    }

    // The conversation is untouched: no half-written question, no group.
    const after = await readConversation(request, conversationId);
    expect(after['message_count'], 'a refused send stored a message').toBe(0);
    const transcript = await request.get(
      `${MESSAGES_PATH}/${conversationId}?sort_order=asc&limit=100`,
    );
    expect(transcript.status()).toBe(200);
    expect(
      ((await transcript.json()) as { items?: readonly unknown[] }).items ?? [],
      'a refused send left rows in the transcript',
    ).toEqual([]);
  } finally {
    await deleteConversation(request, conversationId);
  }
});
