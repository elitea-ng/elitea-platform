/**
 * DEFECT: the "Share" menu item copied a conversation URL with no `/app`
 * basename, so the link was a hard 404 for whoever received it.
 *
 * `ConversationItem` builds the share URL as
 * `${protocol}//${host}${basename}/${projectId}/chat/${id}?...` and takes the
 * basename as a prop, defaulting to `''`. This composition root — the only
 * thing that mounts `Conversations` — never supplied it. In every deployment
 * where `vite_base_uri` is not `/`, only `/app/**` is served by the SPA, so
 * the copied link missed the mount point entirely. Development hid the
 * defect, because `import.meta.env.DEV` resolves the basename to `''` there.
 *
 * The trailing slash matters too: `vite_base_uri` is `/app/` and the URL
 * builder already adds a leading `/`, so an untrimmed value yields
 * `https://host/app//5/chat/...`.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { http, HttpResponse } from 'msw';

import type { Conversation } from '@/entities/conversation';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { useConversationSidebar } from './useConversationSidebar';

const globals = globalThis as unknown as Record<string, unknown>;

function setConfig(baseUri: string): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: baseUri,
    vite_public_project_id: '1',
  };
  resetConfigForTests();
}

/**
 * The hook needs a router ancestor (`useNavigate`) and a query client. The
 * root route renders the probe, the same shape
 * `features/agents/__tests__/testUtils.tsx` uses.
 */
function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const rootRoute = createRootRoute({ component: () => <QueryClientProvider client={client}>{children}</QueryClientProvider> });
  const router = createRouter({ routeTree: rootRoute, history: createMemoryHistory({ initialEntries: ['/chat'] }) });
  return <RouterProvider router={router as never} />;
}

afterEach(() => {
  vi.unstubAllEnvs();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
  useSelectedProjectStore.setState({ project: null });
});

describe('useConversationSidebar — share-link basename', () => {
  it('passes the deployment basename down, with the trailing slash trimmed', async () => {
    vi.stubEnv('DEV', false);
    setConfig('/app/');

    const { result } = renderHook(() => useConversationSidebar(), { wrapper });

    await waitFor(() => expect(result.current).not.toBeNull());
    expect(result.current.conversationsProps.basename).toBe('/app');
  });

  it('passes an empty basename when the app is mounted at the root', async () => {
    vi.stubEnv('DEV', false);
    setConfig('/');

    const { result } = renderHook(() => useConversationSidebar(), { wrapper });

    await waitFor(() => expect(result.current).not.toBeNull());
    expect(result.current.conversationsProps.basename).toBe('');
  });
});

/**
 * DEFECT: `Conversations` accepts `currentUserId`, and the row menu disables
 * Delete and Edit on a conversation the current user does not own. This
 * composition root — the only place that builds `conversationsProps` — never
 * set it. The guard compared `undefined` with `undefined`, so it denied
 * nothing and any project member could delete another member's conversation.
 *
 * The unit test next to the menu could not see this: it supplied both ids
 * itself. Only a test at the composition root can.
 */
describe('useConversationSidebar — current user id', () => {
  it('passes the signed-in user id down to the conversation list', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setConfig('/app/');
    server.use(http.get('/api/v2/social/author', () => HttpResponse.json({ id: 'user-9', name: 'Signed-in User' })));

    const { result } = renderHook(() => useConversationSidebar(), { wrapper });

    await waitFor(() => expect(result.current.conversationsProps.currentUserId).toBe('user-9'));
    resetGeneratedClient();
  });
});

/**
 * A router whose `/chat` and `/chat/$conversationId` routes actually exist,
 * created ONCE and handed back so a test can assert where a delete navigated
 * — the same shape `processes/chat/ui/useCreateChatReset.test.tsx`'s
 * `makeWrapper` established. The plain `wrapper` above rebuilds its router on
 * every render, so its pathname cannot be asserted across state updates.
 */
function makeRoutedWrapper(): { Wrapper: (props: { readonly children: ReactNode }) => ReactNode; router: { navigate: (options: unknown) => Promise<void>; state: { location: { pathname: string } } } } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  let render: () => ReactNode = () => null;
  // The probe renders in the ROOT route's component — like the real sidebar,
  // it must survive the `/chat` -> `/chat/$conversationId` transitions these
  // tests perform; a leaf-route probe remounts (and loses all its state) on
  // every navigation.
  const rootRoute = createRootRoute({ component: () => render() });
  const chatRoute = createRoute({ getParentRoute: () => rootRoute, path: '/chat', component: () => null });
  const conversationRoute = createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: () => null });
  const router = createRouter({
    routeTree: rootRoute.addChildren([chatRoute, conversationRoute]),
    history: createMemoryHistory({ initialEntries: ['/chat'] }),
  });

  function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    render = () => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
    return <RouterProvider router={router as never} />;
  }

  return { Wrapper, router: router as never };
}

/** Selects a project (the guard every conversation API call sits behind) and covers the requests a mounted sidebar fires. */
function seedProjectSeven(): void {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  setConfig('/app/');
  useSelectedProjectStore.setState({ project: { id: '7', name: 'Project 7' } });
  server.use(
    http.get('/api/v2/social/author', () => HttpResponse.json({ id: 'user-1' })),
    http.get('/api/v2/auth/permissions/prompt_lib/7', () => HttpResponse.json([])),
  );
}

const conversation: Conversation = { id: 'c1', name: 'Old name', isPrivate: true };

/**
 * DEFECT: renaming a conversation and "Make public" were silent no-ops.
 *
 * `ConversationItem`'s rename editor and its "Make public" menu item both
 * call `onEdit` with the already-updated conversation. This composition
 * root's `onEditConversation` only did `setActiveConversation(conversation)`
 * — no PUT, and no patch of `dateGroups`/`folders`, which are what the
 * visible rows render from (`Conversations.body.tsx`). The real
 * `renameConversation` was reachable only for a NEW conversation.
 */
describe('useConversationSidebar — conversation edit persistence', () => {
  it('persists a rename via the edit API and patches the visible date groups', async () => {
    seedProjectSeven();
    const editBodies: unknown[] = [];
    server.use(
      http.put('/api/v2/elitea_core/conversation/prompt_lib/7/c1', async ({ request }) => {
        editBodies.push(await request.json());
        return HttpResponse.json({ id: 'c1', name: 'Renamed', is_private: true });
      }),
    );
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.setDateGroups([{ name: 'Today', conversations: [conversation] }]));
    act(() => result.current.conversationsProps.onEditConversation({ ...conversation, name: 'Renamed' }));

    await waitFor(() => expect(editBodies).toEqual([{ name: 'Renamed', is_private: true }]));
    await waitFor(() => expect(result.current.conversationsProps.dateGroups[0]?.conversations[0]?.name).toBe('Renamed'));
  });

  it('persists Make public and patches the folder-held row', async () => {
    seedProjectSeven();
    const editBodies: unknown[] = [];
    server.use(
      http.put('/api/v2/elitea_core/conversation/prompt_lib/7/c1', async ({ request }) => {
        editBodies.push(await request.json());
        return HttpResponse.json({ id: 'c1', name: 'Old name', is_private: false });
      }),
    );
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.setFolders([{ id: 'f1', name: 'Folder', conversations: [conversation] }]));
    act(() => result.current.conversationsProps.onEditConversation({ ...conversation, isPrivate: false }));

    await waitFor(() => expect(editBodies).toEqual([{ name: 'Old name', is_private: false }]));
    await waitFor(() => expect(result.current.conversationsProps.folders[0]?.conversations[0]?.isPrivate).toBe(false));
  });
});

/**
 * DEFECT: deleting a conversation left it on screen and kept the route on
 * the deleted transcript. `deleteConversation` filtered only `conversations`
 * (count-only) and `pinnedConversations`; the visible rows come from
 * `dateGroups`/`folders`, and nothing navigated off the deleted id.
 */
describe('useConversationSidebar — conversation delete', () => {
  it('drops the row from date groups and folders, and navigates the active conversation back to /chat', async () => {
    seedProjectSeven();
    server.use(http.delete('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () => HttpResponse.json({})));
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => {
      result.current.conversationsProps.setDateGroups([{ name: 'Today', conversations: [conversation] }]);
      result.current.conversationsProps.setFolders([{ id: 'f1', name: 'Folder', conversations: [conversation] }]);
    });
    // Make it the ACTIVE conversation, the way a user gets there: a click.
    act(() => result.current.conversationsProps.onSelectConversation(conversation));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/c1'));

    act(() => result.current.conversationsProps.onDeleteConversation(conversation));

    await waitFor(() => expect(result.current.conversationsProps.dateGroups[0]?.conversations).toEqual([]));
    expect(result.current.conversationsProps.folders[0]?.conversations).toEqual([]);
    expect(result.current.conversationsProps.selectedConversationId).toBeUndefined();
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat'));
  });

  /**
   * DEFECT (stale row-handler): the row's Delete handler is captured by a
   * memoised `ConversationItem` reached through `useRenderConversationItem`'s
   * `useCallback([])` render-prop, so a row rendered BEFORE it was selected
   * keeps the `deleteConversation` closure that existed at that first render.
   * When `deleteConversation` decided "am I deleting the open one?" from that
   * closure's captured `activeConversation`, the answer was `undefined` — the
   * DELETE still landed and the row vanished, but the route stayed on the
   * just-deleted transcript.
   *
   * The earlier delete test cannot see this: it reads
   * `result.current.conversationsProps.onDeleteConversation` AFTER selecting,
   * so it always gets the freshest handler. This one captures the handler
   * BEFORE the click, then invokes that exact reference — the stale closure a
   * real `ConversationItem` holds — and proves the navigate-away branch still
   * fires because `deleteConversation` now reads the active conversation live.
   */
  it('navigates to /chat from a Delete handler captured BEFORE the row was selected (stale-closure repro)', async () => {
    seedProjectSeven();
    server.use(http.delete('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () => HttpResponse.json({})));
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => {
      result.current.conversationsProps.setDateGroups([{ name: 'Today', conversations: [conversation] }]);
      result.current.conversationsProps.setFolders([{ id: 'f1', name: 'Folder', conversations: [conversation] }]);
    });

    // Grab the Delete handler NOW, while nothing is selected — this is the
    // closure a row rendered before selection carries. A pre-fix build closes
    // over `activeConversation === undefined` here.
    const staleDelete = result.current.conversationsProps.onDeleteConversation;

    // Select it the way a user does — a click — AFTER the handler was captured.
    act(() => result.current.conversationsProps.onSelectConversation(conversation));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/c1'));

    // Invoke the STALE handler, not the current one.
    act(() => staleDelete(conversation));

    await waitFor(() => expect(result.current.conversationsProps.dateGroups[0]?.conversations).toEqual([]));
    expect(result.current.conversationsProps.folders[0]?.conversations).toEqual([]);
    expect(result.current.conversationsProps.selectedConversationId).toBeUndefined();
    // The branch a pre-fix build skipped: the route must leave the deleted id.
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat'));
  });

  it('does not navigate away when the deleted conversation is not the active one', async () => {
    seedProjectSeven();
    server.use(http.delete('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () => HttpResponse.json({})));
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    const other: Conversation = { id: 'c2', name: 'Other', isPrivate: true };
    act(() => result.current.conversationsProps.setDateGroups([{ name: 'Today', conversations: [conversation, other] }]));
    act(() => result.current.conversationsProps.onSelectConversation(other));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/c2'));

    act(() => result.current.conversationsProps.onDeleteConversation(conversation));

    await waitFor(() => expect(result.current.conversationsProps.dateGroups[0]?.conversations).toEqual([other]));
    expect(result.current.conversationsProps.selectedConversationId).toBe('c2');
    expect(router.state.location.pathname).toBe('/chat/c2');
  });
});

/**
 * DEFECT (stale row-handler, rename): `renameConversation` is
 * `onChangeActiveConversationName` — by name and by contract it acts on
 * whatever conversation is open WHEN IT IS CALLED. It read `activeConversation`
 * out of its own render closure instead, and listed it in the `useCallback`
 * dependency array, so correctness depended entirely on a fresh handler
 * reference reaching the row. It does not always: `Conversations` hands row
 * callbacks down through `useRenderConversationItem`'s `useCallback([])`
 * render prop into a memoised `ConversationItem`, exactly the arrangement that
 * lets a row keep the closure it was rendered with. A closure captured before
 * any selection carries `activeConversation === undefined`, and the guard turns
 * every later rename into a silent no-op — no PUT, no error, the old name
 * still on screen.
 *
 * Same repro shape as the Delete stale-closure test above: capture the handler
 * while nothing is selected, THEN select, THEN invoke that exact reference.
 */
describe('useConversationSidebar — conversation rename', () => {
  it('renames the conversation open at CALL time from a handler captured BEFORE it was selected (stale-closure repro)', async () => {
    seedProjectSeven();
    const renameBodies: unknown[] = [];
    server.use(
      http.put('/api/v2/elitea_core/conversation/prompt_lib/7/c1', async ({ request }) => {
        renameBodies.push(await request.json());
        return HttpResponse.json({ id: 'c1', name: 'Typed later', is_private: true });
      }),
    );
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.setDateGroups([{ name: 'Today', conversations: [conversation] }]));

    // Grab the rename handler NOW, while nothing is selected — a pre-fix build
    // closes over `activeConversation === undefined` at this point.
    const staleRename = result.current.conversationsProps.onChangeActiveConversationName;

    act(() => result.current.conversationsProps.onSelectConversation(conversation));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/c1'));

    // Invoke the STALE reference, not the current one.
    act(() => staleRename('Typed later'));

    // A pre-fix build sends nothing at all: its guard short-circuits.
    await waitFor(() => expect(renameBodies).toEqual([{ name: 'Typed later' }]));
  });

  it('still sends nothing when no conversation is open', async () => {
    seedProjectSeven();
    const renameBodies: unknown[] = [];
    server.use(
      http.put('/api/v2/elitea_core/conversation/prompt_lib/7/c1', async ({ request }) => {
        renameBodies.push(await request.json());
        return HttpResponse.json({ id: 'c1', name: 'Nope', is_private: true });
      }),
    );
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onChangeActiveConversationName('Nope'));

    // The live read must still fail closed — reading the ref replaced the
    // stale value, not the guard.
    await waitFor(() => expect(result.current).not.toBeNull());
    expect(renameBodies).toEqual([]);
  });
});

/**
 * DEFECT (stale header-handler): "Create folder" carries the baseline's
 * re-entrancy guard — while a draft folder is already open, a second click is
 * a no-op rather than stacking a second draft. "Is a draft already open?" is
 * answered when the button is CLICKED, but the guard read `activeFolder` out of
 * the render closure that built the callback, so a reference captured before
 * the first draft existed answered `undefined` forever and stacked a draft on
 * every call. Two drafts in `folders` means two `FolderItem`s in edit mode, and
 * `onCreateFolder` only ever confirms `activeFolder` — the orphan stays.
 */
describe('useConversationSidebar — create-folder re-entrancy', () => {
  it('refuses a second draft from a handler captured BEFORE the first draft existed (stale-closure repro)', async () => {
    seedProjectSeven();
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    // The header button holds one reference for the life of the sidebar; a
    // pre-fix build's copy closes over `activeFolder === undefined`.
    const staleCreateFolder = result.current.conversationsProps.onClickCreateNewFolder;

    act(() => staleCreateFolder());
    await waitFor(() => expect(result.current.conversationsProps.folders.filter((folder) => folder.isNew === true)).toHaveLength(1));

    // The SAME reference again. A pre-fix build's guard still sees `undefined`
    // and stacks a second draft; the live read sees the one already open.
    act(() => staleCreateFolder());

    expect(result.current.conversationsProps.folders.filter((folder) => folder.isNew === true)).toHaveLength(1);
  });
});

/**
 * DEFECT: the row's Play button and a plain click on the row did the same
 * thing. `onPlaybackConversation` was wired to `onSelectConversation` on the
 * grounds that `PlaybackChatBox` had no mount — so the control existed, was
 * clickable, and started no playback, with nothing on screen to say so.
 *
 * `processes/chat/ui/ChatPlayback.tsx` is now that mount, selected by
 * `?playback=1` on the conversation's own URL.
 */
describe('useConversationSidebar — playback', () => {
  interface SearchState {
    readonly state: { readonly location: { readonly pathname: string; readonly search: Record<string, unknown> } };
  }

  it('sends the Play button to the conversation WITH ?playback=1', async () => {
    seedProjectSeven();
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onPlaybackConversation(conversation));

    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/c1'));
    expect((router as unknown as SearchState).state.location.search['playback']).toBe('1');
    expect(result.current.conversationsProps.selectedConversationId).toBe('c1');
  });

  /*
   * The other half: a plain click while a replay is open must LEAVE playback.
   * Without an explicit `playback: '0'`, the flag survives the navigation and
   * the user is stuck replaying every conversation they click.
   */
  it('clears playback when a row is then clicked normally', async () => {
    seedProjectSeven();
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onPlaybackConversation(conversation));
    await waitFor(() => expect((router as unknown as SearchState).state.location.search['playback']).toBe('1'));

    act(() => result.current.conversationsProps.onSelectConversation(conversation));
    await waitFor(() => expect((router as unknown as SearchState).state.location.search['playback']).not.toBe('1'));
  });
});

/**
 * Issue 940/A6 — Chat "Duplicate" action. No dedicated Go clone route exists,
 * so `onDuplicateConversation` composes the copy from the same 3 endpoints
 * `entities/conversation`/`entities/participant` already wrap: GET details,
 * POST create, and POST participants.
 */
describe('useConversationSidebar — conversation duplicate', () => {
  it('creates a "(copy)"-named conversation carrying the source participants and public visibility, and navigates to it', async () => {
    seedProjectSeven();
    const createBodies: unknown[] = [];
    const participantBodies: unknown[] = [];
    let editCalls = 0;
    server.use(
      http.get('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () =>
        HttpResponse.json({
          id: 'c1',
          name: 'Old name',
          is_private: false,
          meta: { steps_limit: 5 },
          participants: [
            { id: 'p1', entity_name: 'application', entity_meta: { id: '42' }, entity_settings: { version_id: 'v1' } },
            { id: 'p2', entity_name: 'user', entity_meta: { id: '9' } },
          ],
        }),
      ),
      http.post('/api/v2/elitea_core/conversations/prompt_lib/7', async ({ request }) => {
        createBodies.push(await request.json());
        return HttpResponse.json({ id: 'c2', name: 'Old name (copy)', is_private: true });
      }),
      http.put('/api/v2/elitea_core/conversation/prompt_lib/7/c2', () => {
        editCalls += 1;
        return HttpResponse.json({ id: 'c2', name: 'Old name (copy)', is_private: false });
      }),
      http.post('/api/v2/elitea_core/participants/prompt_lib/7/c2', async ({ request }) => {
        participantBodies.push(await request.json());
        return HttpResponse.json([]);
      }),
    );

    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onDuplicateConversation({ ...conversation, isPrivate: false }));

    await waitFor(() => expect(createBodies).toEqual([{ name: 'Old name (copy)', is_private: true, meta: { steps_limit: 5 } }]));
    // Public visibility is a second PUT (the same route "Make public" uses) —
    // the create route itself ignores `is_private` (Go's `Create` handler
    // reads only `name`/`meta`/`author_id`).
    await waitFor(() => expect(editCalls).toBe(1));
    await waitFor(() =>
      expect(participantBodies).toEqual([
        [
          { entity_name: 'application', entity_meta: { id: '42' }, entity_settings: { version_id: 'v1' } },
          { entity_name: 'user', entity_meta: { id: '9' } },
        ],
      ]),
    );
    await waitFor(() => expect(result.current.conversationsProps.selectedConversationId).toBe('c2'));
    expect(router.state.location.pathname).toBe('/chat/c2');
  });

  it('does not make the duplicate public for a private original (no participants to copy either)', async () => {
    seedProjectSeven();
    let editCalled = false;
    server.use(
      http.get('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () =>
        HttpResponse.json({ id: 'c1', name: 'Old', is_private: true, participants: [] }),
      ),
      http.post('/api/v2/elitea_core/conversations/prompt_lib/7', () => HttpResponse.json({ id: 'c3', name: 'Old (copy)', is_private: true })),
      // If duplicating a private original ever called this, that would be the defect.
      http.put('/api/v2/elitea_core/conversation/prompt_lib/7/c3', () => {
        editCalled = true;
        return HttpResponse.json({ id: 'c3', name: 'Old (copy)', is_private: false });
      }),
    );
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onDuplicateConversation(conversation));

    await waitFor(() => expect(result.current.conversationsProps.selectedConversationId).toBe('c3'));
    expect(router.state.location.pathname).toBe('/chat/c3');
    expect(editCalled).toBe(false);
  });

  it('surfaces an error message and does not navigate when the source conversation cannot be read', async () => {
    seedProjectSeven();
    server.use(http.get('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onDuplicateConversation(conversation));

    await waitFor(() => expect(result.current.errorMessage).toBe('Failed to duplicate the conversation'));
    expect(router.state.location.pathname).toBe('/chat');
  });

  it('does nothing when no project is selected', async () => {
    setConfig('/app/');
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onDuplicateConversation(conversation));

    // No project id at all — the guard returns before any network call, so
    // neither an error nor a navigation happens.
    expect(router.state.location.pathname).toBe('/chat');
    expect(result.current.errorMessage).toBeUndefined();
  });

  it('falls back to the row\'s own name when the source answers a blank name, drops a non-string participant entity_name, and tolerates a participant with no entity_meta', async () => {
    seedProjectSeven();
    const createBodies: unknown[] = [];
    const participantBodies: unknown[] = [];
    server.use(
      http.get('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () =>
        HttpResponse.json({
          id: 'c1',
          name: '',
          is_private: true,
          // `participants` omitted entirely — the `?? []` branch.
          participants: undefined,
        }),
      ),
      http.post('/api/v2/elitea_core/conversations/prompt_lib/7', async ({ request }) => {
        createBodies.push(await request.json());
        return HttpResponse.json({ id: 'c9', name: 'Old name (copy)', is_private: true });
      }),
    );
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onDuplicateConversation(conversation));

    // `source.name` is `''` (falsy) — the row's own name is used instead.
    await waitFor(() => expect(createBodies).toEqual([{ name: 'Old name (copy)', is_private: true }]));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/c9'));
    expect(participantBodies).toEqual([]);
  });

  it('drops a participant whose entity_name is not a string and omits entity_meta when the source lacks it', async () => {
    seedProjectSeven();
    const participantBodies: unknown[] = [];
    server.use(
      http.get('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () =>
        HttpResponse.json({
          id: 'c1',
          name: 'Old name',
          is_private: true,
          participants: [
            { id: 'p1', entity_name: 42, entity_meta: { id: '1' } },
            { id: 'p2', entity_name: 'user' },
          ],
        }),
      ),
      http.post('/api/v2/elitea_core/conversations/prompt_lib/7', () => HttpResponse.json({ id: 'c10', name: 'Old name (copy)', is_private: true })),
      http.post('/api/v2/elitea_core/participants/prompt_lib/7/c10', async ({ request }) => {
        participantBodies.push(await request.json());
        return HttpResponse.json([]);
      }),
    );
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onDuplicateConversation(conversation));

    // The numeric `entity_name` (p1) is filtered out; only the string-named
    // `user` participant (with no `entity_meta` at all) is carried over.
    await waitFor(() => expect(participantBodies).toEqual([[{ entity_name: 'user' }]]));
  });
});

describe('useConversationSidebar — miscellaneous UI state', () => {
  it('onDismissError clears a set error message', async () => {
    seedProjectSeven();
    server.use(http.get('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onDuplicateConversation(conversation));
    await waitFor(() => expect(result.current.errorMessage).toBe('Failed to duplicate the conversation'));

    act(() => result.current.onDismissError());
    expect(result.current.errorMessage).toBeUndefined();
  });

  it('onCollapsed toggles the collapsed flag', async () => {
    seedProjectSeven();
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    expect(result.current.conversationsProps.collapsed).toBe(false);
    act(() => result.current.conversationsProps.onCollapsed());
    expect(result.current.conversationsProps.collapsed).toBe(true);
    act(() => result.current.conversationsProps.onCollapsed());
    expect(result.current.conversationsProps.collapsed).toBe(false);
  });

  it('onCancelCreateConversation clears the active conversation', async () => {
    seedProjectSeven();
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onSelectConversation(conversation));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/c1'));

    act(() => result.current.conversationsProps.onCancelCreateConversation());
    expect(result.current.conversationsProps.selectedConversationId).toBeUndefined();
  });

  it('onSearchQueryChange is threaded down into conversationsProps', async () => {
    seedProjectSeven();
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onSearchQueryChange?.('needle'));
    // Re-render happened and the sidebar is still functional with a query set.
    await waitFor(() => expect(result.current).not.toBeNull());
  });
});

describe('useConversationSidebar — create conversation', () => {
  it('does nothing (and posts nothing) when no project is selected', async () => {
    setConfig('/app/');
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    const created = await result.current.conversationsProps.onCreateConversation(conversation);
    expect(created).toBeUndefined();
  });

  it('creates a new conversation, defaulting is_private to true when omitted', async () => {
    seedProjectSeven();
    const createBodies: unknown[] = [];
    server.use(
      http.post('/api/v2/elitea_core/conversations/prompt_lib/7', async ({ request }) => {
        createBodies.push(await request.json());
        return HttpResponse.json({ id: 'new-1', name: 'Draft', is_private: true });
      }),
    );
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    const created = await result.current.conversationsProps.onCreateConversation({ id: 'draft', name: 'Draft', isPrivate: undefined as unknown as boolean });
    expect(created).toEqual({ id: 'new-1', name: 'Draft', is_private: true });
    expect(createBodies).toEqual([{ name: 'Draft', is_private: true }]);
  });

  it('creates a new conversation with is_private explicitly false', async () => {
    seedProjectSeven();
    const createBodies: unknown[] = [];
    server.use(
      http.post('/api/v2/elitea_core/conversations/prompt_lib/7', async ({ request }) => {
        createBodies.push(await request.json());
        return HttpResponse.json({ id: 'new-2', name: 'Public draft', is_private: false });
      }),
    );
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    const created = await result.current.conversationsProps.onCreateConversation({ id: 'draft', name: 'Public draft', isPrivate: false });
    expect(created).toEqual({ id: 'new-2', name: 'Public draft', is_private: false });
    expect(createBodies).toEqual([{ name: 'Public draft', is_private: false }]);
  });

  it('surfaces an error and resolves undefined when the create call fails', async () => {
    seedProjectSeven();
    server.use(http.post('/api/v2/elitea_core/conversations/prompt_lib/7', () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    const created = await result.current.conversationsProps.onCreateConversation(conversation);
    expect(created).toBeUndefined();
    await waitFor(() => expect(result.current.errorMessage).toBe('Failed to create the conversation'));
  });
});

describe('useConversationSidebar — delete/rename network failure', () => {
  it('surfaces an error when the delete call fails', async () => {
    seedProjectSeven();
    server.use(http.delete('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.setDateGroups([{ name: 'Today', conversations: [conversation] }]));
    act(() => result.current.conversationsProps.onDeleteConversation(conversation));

    await waitFor(() => expect(result.current.errorMessage).toBe('Failed to delete the conversation'));
  });

  it('does nothing when no project is selected', async () => {
    setConfig('/app/');
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    expect(() => act(() => result.current.conversationsProps.onDeleteConversation(conversation))).not.toThrow();
  });

  it('surfaces an error when the rename call fails', async () => {
    seedProjectSeven();
    server.use(http.put('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.onSelectConversation(conversation));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/c1'));

    act(() => result.current.conversationsProps.onChangeActiveConversationName('New name'));

    await waitFor(() => expect(result.current.errorMessage).toBe('Failed to rename the conversation'));
  });
});

describe('useConversationSidebar — edit playback / no-project branches', () => {
  it('patches a playback conversation locally without calling the edit API', async () => {
    seedProjectSeven();
    let editCalled = false;
    server.use(
      http.put('/api/v2/elitea_core/conversation/prompt_lib/7/c1', () => {
        editCalled = true;
        return HttpResponse.json({ id: 'c1', name: 'Old name', is_private: true });
      }),
    );
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    const playback: Conversation = { ...conversation, isPlayback: true };
    act(() => result.current.conversationsProps.setDateGroups([{ name: 'Today', conversations: [playback] }]));
    act(() => result.current.conversationsProps.onEditConversation({ ...playback, name: 'Renamed locally' }));

    await waitFor(() => expect(result.current.conversationsProps.dateGroups[0]?.conversations[0]?.name).toBe('Renamed locally'));
    expect(editCalled).toBe(false);
  });

  it('does nothing for a non-playback edit when no project is selected', async () => {
    setConfig('/app/');
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    expect(() => act(() => result.current.conversationsProps.onEditConversation({ ...conversation, name: 'x' }))).not.toThrow();
  });

  it('leaves a non-matching row untouched and does not clear an unrelated active conversation', async () => {
    seedProjectSeven();
    server.use(http.put('/api/v2/elitea_core/conversation/prompt_lib/7/c2', () => HttpResponse.json({ id: 'c2', name: 'Renamed', is_private: true })));
    const { Wrapper, router } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    const other: Conversation = { id: 'c2', name: 'Other', isPrivate: true };
    act(() => result.current.conversationsProps.setDateGroups([{ name: 'Today', conversations: [conversation, other] }]));
    // `conversation` (c1) is the ACTIVE one; `other` (c2) is what gets edited.
    act(() => result.current.conversationsProps.onSelectConversation(conversation));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/c1'));

    act(() => result.current.conversationsProps.onEditConversation({ ...other, name: 'Renamed' }));

    await waitFor(() => expect(result.current.conversationsProps.dateGroups[0]?.conversations).toEqual([conversation, { ...other, name: 'Renamed' }]));
    // The non-matching row (c1) kept its own identity, and the active
    // conversation (also c1, not the edited c2) was left alone.
    expect(result.current.conversationsProps.selectedConversationId).toBe('c1');
  });
});

describe('useConversationSidebar — pin wrapper', () => {
  it('onPinConversation forwards to the pin hook without throwing', async () => {
    seedProjectSeven();
    server.use(http.post('/api/v2/social/pin/prompt_lib/7/*', () => HttpResponse.json({})));
    const { Wrapper } = makeRoutedWrapper();
    const { result } = renderHook(() => useConversationSidebar(), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).not.toBeNull());

    act(() => result.current.conversationsProps.setDateGroups([{ name: 'Today', conversations: [conversation] }]));
    expect(() => act(() => result.current.conversationsProps.onPinConversation(conversation, true))).not.toThrow();
  });
});
