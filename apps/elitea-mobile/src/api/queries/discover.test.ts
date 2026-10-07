import {
  DISCOVER_CAPTURE,
  addedPublicAgent,
  createdConversation,
  publicBadParam,
  publicDetail,
  publicDetailMissing,
  publicEmpty,
  publicList,
  publicPage2,
} from '@/api/__fixtures__/discover13';
import {
  StartPublicChatError,
  fetchPublicAgentDetail,
  fetchPublicAgents,
  flattenPublicAgentPages,
  nextPublicAgentOffset,
  publicAgentIconUrl,
  publicAgentOf,
  publicAgentPageOf,
  startConversationWithPublicAgent,
  startPublicChatErrorText,
  type PublicAgent,
} from '@/api/queries/discover';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const P = DISCOVER_CAPTURE.projectId;
const LIST = '/elitea_core/public_applications/prompt_lib';

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
});

const travel: PublicAgent = {
  id: 2,
  publicProjectId: 1,
  name: 'Zefir Travel Planner',
  description: 'Zefir Travel Planner: a public agent anyone can find in Discover.',
  versionId: 2,
  versionName: 'v1',
  agentType: 'openai',
  category: 'Project Management',
  tags: [
    { id: '2', name: 'planning' },
    { id: '1', name: 'travel' },
  ],
  iconPath: '/icons/1/076958cfcd17545b.png',
  likes: 0,
};

describe('Discover catalogue (client contract 1.3, captured shapes)', () => {
  it('reads the rows: string ids as numbers, the public project, the published version, tags and icon', () => {
    const page = publicAgentPageOf(publicList, 0);
    expect(page.total).toBe(2);
    expect(page.agents).toEqual([
      travel,
      {
        id: 1,
        publicProjectId: 1,
        name: 'Zefir Concierge',
        description: 'Zefir Concierge: a public agent anyone can find in Discover.',
        versionId: 1,
        versionName: 'v1',
        agentType: 'openai',
        category: 'Other',
        tags: [],
        iconPath: null,
        likes: 0,
      },
    ]);
  });

  it('asks for a page by offset with the search, most liked first', async () => {
    const server = fakeServer();
    server.on('GET', LIST, { status: 200, body: publicPage2 });
    const page = await fetchPublicAgents('  travel ', 1, server.client);
    const url = server.requests[0].url;
    expect(url.pathname).toBe(`/api/v2${LIST}`);
    expect(Object.fromEntries(url.searchParams)).toEqual({
      limit: '20',
      offset: '1',
      sort_by: 'likes',
      sort_order: 'desc',
      query: 'travel',
    });
    expect(page).toMatchObject({ offset: 1, received: 1, total: 2 });
  });

  it('no search sends no query parameter', async () => {
    const server = fakeServer();
    server.on('GET', LIST, { status: 200, body: publicList });
    await fetchPublicAgents('   ', 0, server.client);
    expect(server.requests[0].url.searchParams.has('query')).toBe(false);
  });

  it('pages until the filtered total is reached', () => {
    expect(nextPublicAgentOffset(publicAgentPageOf(publicList, 0))).toBeUndefined();
    const first = publicAgentPageOf({ ...publicPage2 }, 0);
    expect(nextPublicAgentOffset(first)).toBe(1);
    expect(nextPublicAgentOffset(publicAgentPageOf(publicPage2, 1))).toBeUndefined();
    expect(nextPublicAgentOffset(publicAgentPageOf(publicEmpty, 0))).toBeUndefined();
  });

  it('an invalid row is skipped but still counts toward paging; a repeated agent shows once', () => {
    const broken = { rows: [{ ...publicList.rows[0], version_id: 'x' }, publicList.rows[1]], total: 3 };
    const page = publicAgentPageOf(broken, 0);
    expect(page.agents.map((a) => a.id)).toEqual([1]);
    expect(nextPublicAgentOffset(page)).toBe(2);
    expect(flattenPublicAgentPages([publicAgentPageOf(publicList, 0), publicAgentPageOf(publicPage2, 1)]).map((a) => a.id)).toEqual([2, 1]);
  });

  it('a row without a name or a public project cannot be chatted with', () => {
    expect(publicAgentOf({ ...publicList.rows[0], name: '  ' })).toBeNull();
    expect(publicAgentOf({ ...publicList.rows[0], project_id: null })).toBeNull();
    expect(publicAgentOf({ ...publicList.rows[0], meta: null })).toMatchObject({ category: null, iconPath: null });
  });

  it('a 400 (a parameter outside its allowlist) is a failure, not an empty catalogue', async () => {
    const server = fakeServer();
    server.on('GET', LIST, { status: 400, body: publicBadParam });
    await expect(fetchPublicAgents('', 0, server.client)).rejects.toThrow();
  });
});

describe('icon URL', () => {
  it('resolves the stored /icons path against the workspace origin', () => {
    expect(publicAgentIconUrl(travel.iconPath, 'http://localhost:8094')).toBe('http://localhost:8094/icons/1/076958cfcd17545b.png');
    expect(publicAgentIconUrl('https://ai.example.com/icons/1/a.png', FAKE_ORIGIN)).toBe('https://ai.example.com/icons/1/a.png');
  });

  it('never loads from another host or another scheme', () => {
    expect(publicAgentIconUrl('https://tracker.example.net/p.png', FAKE_ORIGIN)).toBeNull();
    expect(publicAgentIconUrl('//tracker.example.net/p.png', FAKE_ORIGIN)).toBeNull();
    expect(publicAgentIconUrl('javascript:alert(1)', FAKE_ORIGIN)).toBeNull();
    expect(publicAgentIconUrl('data:image/png;base64,AAAA', FAKE_ORIGIN)).toBeNull();
    expect(publicAgentIconUrl('https://user:pw@ai.example.com/icons/1/a.png', FAKE_ORIGIN)).toBeNull();
    expect(publicAgentIconUrl(null, FAKE_ORIGIN)).toBeNull();
    expect(publicAgentIconUrl('/icons/1/a.png', null)).toBeNull();
  });
});

describe('agent details', () => {
  it('reads the welcome message and the conversation starters of the published version', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/public_application/prompt_lib/2/v1', { status: 200, body: publicDetail });
    await expect(fetchPublicAgentDetail(travel, server.client)).resolves.toEqual({
      welcomeMessage: 'Welcome to Zefir Travel Planner!',
      starters: ['What can you do?', 'Give me three ideas'],
    });
  });

  it('a 404 (no longer published) is "no details", not an error', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/public_application/prompt_lib/2/v1', { status: 404, body: publicDetailMissing });
    await expect(fetchPublicAgentDetail(travel, server.client)).resolves.toBeNull();
  });
});

describe('starting a chat (createConversation, then addConversationParticipants)', () => {
  const CREATE = `/elitea_core/conversations/prompt_lib/${P}`;
  const ADD = `/elitea_core/participants/prompt_lib/${P}/${createdConversation.uuid}`;

  it('creates the conversation in the active project and adds the agent from the public project at its published version', async () => {
    const server = fakeServer();
    server.on('POST', CREATE, { status: 201, body: createdConversation });
    server.on('POST', ADD, { status: 200, body: addedPublicAgent });
    await expect(startConversationWithPublicAgent(P, travel, server.client)).resolves.toEqual({
      id: 9,
      uuid: createdConversation.uuid,
    });
    expect(server.requests.map((r) => `${r.method} ${r.url.pathname}`)).toEqual([`POST /api/v2${CREATE}`, `POST /api/v2${ADD}`]);
    expect(JSON.parse(server.requests[0].body!)).toEqual({ name: 'Zefir Travel Planner', is_private: true, participants: [] });
    expect(JSON.parse(server.requests[1].body!)).toEqual([
      {
        entity_name: 'application',
        entity_meta: { id: 2, project_id: 1, name: 'Zefir Travel Planner' },
        entity_settings: { version_id: 2 },
      },
    ]);
  });

  it('when the agent cannot be added, deletes the empty conversation and says the agent is gone', async () => {
    const server = fakeServer();
    server.on('POST', CREATE, { status: 201, body: createdConversation });
    server.on('POST', ADD, { status: 400, body: { error: 'version not found' } });
    server.on('DELETE', `/elitea_core/conversation/prompt_lib/${P}/${createdConversation.uuid}`, { status: 204 });
    const failure = startConversationWithPublicAgent(P, travel, server.client);
    await expect(failure).rejects.toBeInstanceOf(StartPublicChatError);
    await expect(failure).rejects.toMatchObject({ reason: 'not_available' });
    expect(server.requests.map((r) => r.method)).toEqual(['POST', 'POST', 'DELETE']);
  });

  it('a project that refuses new conversations says so, and nothing is added', async () => {
    const server = fakeServer();
    server.on('POST', CREATE, { status: 403, body: { error: 'forbidden' } });
    const failure = startConversationWithPublicAgent(P, travel, server.client);
    await expect(failure).rejects.toMatchObject({ reason: 'not_allowed' });
    await failure.catch((e) => expect(startPublicChatErrorText(e)).toBe("You can't start conversations in this project."));
    expect(server.requests).toHaveLength(1);
  });

  it('a 404 or 410 from the create step blames the project, not the agent', async () => {
    for (const status of [404, 410]) {
      const server = fakeServer();
      server.on('POST', CREATE, { status, body: { error: 'project not found' } });
      const failure = startConversationWithPublicAgent(P, travel, server.client);
      await expect(failure).rejects.toMatchObject({ reason: 'project_unavailable', status });
      await failure.catch((e) =>
        expect(startPublicChatErrorText(e)).toBe("This project isn't available any more. Pick another project and try again."),
      );
      expect(server.requests).toHaveLength(1);
    }
  });

  it('a 403 while adding the agent says the agent is not shared with you, not that the project refuses', async () => {
    const server = fakeServer();
    server.on('POST', CREATE, { status: 201, body: createdConversation });
    server.on('POST', ADD, { status: 403, body: { error: 'forbidden' } });
    server.on('DELETE', `/elitea_core/conversation/prompt_lib/${P}/`, { status: 204 });
    const failure = startConversationWithPublicAgent(P, travel, server.client);
    await expect(failure).rejects.toMatchObject({ reason: 'not_shared', status: 403, leftover: false });
    await failure.catch((e) => expect(startPublicChatErrorText(e)).toBe("This agent isn't shared with you."));
  });

  it('a 404 while adding the agent says the agent is gone', async () => {
    const server = fakeServer();
    server.on('POST', CREATE, { status: 201, body: createdConversation });
    server.on('POST', ADD, { status: 404, body: { error: 'application not found' } });
    server.on('DELETE', `/elitea_core/conversation/prompt_lib/${P}/`, { status: 204 });
    await expect(startConversationWithPublicAgent(P, travel, server.client)).rejects.toMatchObject({ reason: 'not_available' });
  });

  it('a network failure while adding still removes the empty conversation', async () => {
    const server = fakeServer();
    server.on('POST', CREATE, { status: 201, body: createdConversation });
    server.on('POST', ADD, 'network_error');
    server.on('DELETE', `/elitea_core/conversation/prompt_lib/${P}/`, { status: 204 });
    const failure = startConversationWithPublicAgent(P, travel, server.client);
    await expect(failure).rejects.toMatchObject({ reason: 'failed', leftover: false });
    expect(server.requests.at(-1)?.method).toBe('DELETE');
  });

  it('when the link drops after the create and the clean-up fails too, it says an empty conversation may be left', async () => {
    const server = fakeServer();
    server.on('POST', CREATE, { status: 201, body: createdConversation });
    server.on('POST', ADD, 'network_error');
    server.on('DELETE', `/elitea_core/conversation/prompt_lib/${P}/`, 'network_error');
    const failure = startConversationWithPublicAgent(P, travel, server.client);
    await expect(failure).rejects.toMatchObject({ reason: 'failed', leftover: true });
    await failure.catch((e) =>
      expect(startPublicChatErrorText(e)).toBe("Couldn't start the conversation. An empty one may be left in your list."),
    );
  });

  it('a clean-up the server refuses (non-2xx) also counts as left behind', async () => {
    const server = fakeServer();
    server.on('POST', CREATE, { status: 201, body: createdConversation });
    server.on('POST', ADD, { status: 400, body: { error: 'version not found' } });
    server.on('DELETE', `/elitea_core/conversation/prompt_lib/${P}/`, { status: 500, body: { error: 'boom' } });
    await expect(startConversationWithPublicAgent(P, travel, server.client)).rejects.toMatchObject({
      reason: 'not_available',
      leftover: true,
    });
  });
});
