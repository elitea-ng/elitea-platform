import { agentParticipantRow, applicationDetail, candidatesPage } from '@/api/__fixtures__/chat11';
import { fetchAgentVersion } from '@/api/queries/agents';
import {
  ParticipantError,
  addAgentParticipant,
  addPersonParticipant,
  fetchParticipantCandidates,
  participantErrorText,
  removeParticipant,
} from '@/api/queries/participants';
import { prepareTurn } from '@/api/queries/sendMessage';
import { chatTarget } from '@/features/chat/messageModel';
import { clearTargets, getTarget, setTarget, targetKey } from '@/features/chat/targetStore';
import { endSession } from '@/security/endSession';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const UUID = '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c';

beforeEach(() => {
  __resetSessionForTests();
  clearTargets();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 't' });
});

describe('agent version (getApplication)', () => {
  it('reads the current version id and its internal tools', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/application/prompt_lib/7/31', { status: 200, body: applicationDetail() });
    await expect(fetchAgentVersion(7, 31, server.client)).resolves.toEqual({
      versionId: 310,
      internalTools: ['attachments', 'ask_user'],
      iconPath: null,
      starters: [],
      welcomeMessage: null,
    });
  });

  it('reads the icon, the conversation starters (strings only, at most four) and the welcome message', async () => {
    const server = fakeServer();
    const detail = applicationDetail();
    const body = {
      ...detail,
      version_details: {
        ...detail.version_details,
        meta: { ...detail.version_details.meta, icon_meta: { name: 'a.png', url: '/icons/7/a.png' } },
        conversation_starters: ['  First  ', 2, '', 'Second', 'Third', 'Fourth', 'Fifth'],
        welcome_message: '  Hi there.  ',
      },
    };
    server.on('GET', '/elitea_core/application/prompt_lib/7/31', { status: 200, body });
    await expect(fetchAgentVersion(7, 31, server.client)).resolves.toMatchObject({
      iconPath: '/icons/7/a.png',
      starters: ['First', 'Second', 'Third', 'Fourth'],
      welcomeMessage: 'Hi there.',
    });
  });
});

describe('addAgentParticipant', () => {
  it('pins the agent to its current version', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/application/prompt_lib/7/31', { status: 200, body: applicationDetail() });
    server.on('POST', '/elitea_core/participants/prompt_lib/7/42', { status: 200, body: [agentParticipantRow(5, 31, 310)] });
    const row = await addAgentParticipant(7, '42', { id: 31, name: 'Approver' }, server.client);
    expect(row.id).toBe(5);
    const post = server.requests.find((r) => r.method === 'POST')!;
    expect(JSON.parse(post.body!)).toEqual([
      { entity_name: 'application', entity_meta: { id: 31, project_id: 7, name: 'Approver' }, entity_settings: { version_id: 310 } },
    ]);
  });

  it('refuses an agent without a version and adds nothing', async () => {
    const server = fakeServer();
    const { version_details: _, ...noVersion } = applicationDetail();
    server.on('GET', '/elitea_core/application/prompt_lib/7/31', { status: 200, body: noVersion });
    await expect(addAgentParticipant(7, '42', { id: 31, name: 'Approver' }, server.client)).rejects.toMatchObject({ reason: 'no_version' });
    expect(server.requests.some((r) => r.method === 'POST')).toBe(false);
  });

  it('maps a 403 to "not allowed" and an older server to "not available"', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/application/prompt_lib/7/31', { status: 200, body: applicationDetail() });
    server.on('POST', '/elitea_core/participants/prompt_lib/7/42', { status: 403, body: { error: 'forbidden' } });
    const forbidden = await addAgentParticipant(7, '42', { id: 31, name: 'Approver' }, server.client).catch((e: unknown) => e);
    expect(forbidden).toBeInstanceOf(ParticipantError);
    expect(participantErrorText(forbidden)).toBe("You can't change who is in this conversation.");
    server.on('POST', '/elitea_core/participants/prompt_lib/7/42', { status: 404, body: { error: 'not found' } });
    await expect(addAgentParticipant(7, '42', { id: 31, name: 'Approver' }, server.client)).rejects.toMatchObject({ reason: 'not_available' });
  });
});

describe('people', () => {
  it('adds a person by USER id', async () => {
    const server = fakeServer();
    server.on('POST', '/elitea_core/participants/prompt_lib/7/42', {
      status: 200,
      body: [{ id: 6, entity_name: 'user', entity_meta: { id: 42 }, meta: { user_name: 'Bob' } }],
    });
    await addPersonParticipant(7, '42', { userId: 42, name: 'Bob' }, server.client);
    expect(JSON.parse(server.requests[0].body!)).toEqual([{ entity_name: 'user', entity_meta: { id: 42, user_name: 'Bob' } }]);
  });

  it('lists candidates with the search on the server, and reads "none offered" from an older one', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/participant_candidates/prompt_lib/7/42', {
      status: 200,
      body: candidatesPage([
        { user_id: 42, name: 'Bob', email: 'bob@example.com', already_participant: false },
        { user_id: 9, name: 'Alice Smith', already_participant: true },
      ], 'c2'),
    });
    const page = await fetchParticipantCandidates(7, '42', ' bo ', null, server.client);
    expect(server.requests[0].url.searchParams.get('q')).toBe('bo');
    expect(server.requests[0].url.searchParams.get('type')).toBe('user');
    expect(page).toEqual({
      nextCursor: 'c2',
      rows: [
        { userId: 42, name: 'Bob', email: 'bob@example.com', alreadyParticipant: false },
        { userId: 9, name: 'Alice Smith', email: null, alreadyParticipant: true },
      ],
    });
    server.on('GET', '/elitea_core/participant_candidates/prompt_lib/7/42', { status: 404, body: { error: 'not found' } });
    await expect(fetchParticipantCandidates(7, '42', '', null, server.client)).resolves.toBeNull();
  });

  it('removes a participant by its row id (204), and treats an already-gone one as removed', async () => {
    const server = fakeServer();
    server.on('DELETE', '/elitea_core/participant/prompt_lib/7/42/6', { status: 204 });
    await removeParticipant(7, '42', 6, server.client);
    expect(server.requests[0]).toMatchObject({ method: 'DELETE' });
    server.on('DELETE', '/elitea_core/participant/prompt_lib/7/42/6', { status: 404, body: { error: 'gone' } });
    await expect(removeParticipant(7, '42', 6, server.client)).resolves.toBeUndefined();
    await expect(removeParticipant(7, '42', 0, server.client)).rejects.toMatchObject({ reason: 'invalid' });
  });
});

describe('the chosen agent', () => {
  it('a send goes to the chosen participant', () => {
    const detail = {
      participants: [
        { id: 1, entity_name: 'user', entity_meta: { id: 9 } },
        agentParticipantRow(2, 30, 300),
        agentParticipantRow(5, 31, 310),
      ],
    };
    const key = targetKey(7, '42');
    setTarget(key, 5);
    const target = chatTarget(detail, { targetParticipantId: getTarget(key) });
    if (!target.ok || target.kind !== 'agent') throw new Error('expected an agent target');
    const body = JSON.parse(
      prepareTurn({ projectId: 7, conversationUuid: UUID, participantId: target.participantId, questionId: '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f', userInput: 'hi' }).bodyJson,
    );
    expect(body.participant_id).toBe(5);
  });

  it('is forgotten when the session ends', async () => {
    setTarget(targetKey(7, '42'), 5);
    await endSession({ reason: 'user_logout' });
    expect(getTarget(targetKey(7, '42'))).toBeNull();
  });
});
