/**
 * The chat composer keeps unsent text per conversation (`chatDraftKey` →
 * `NewChatInput`'s `content.draftKey`): type in A, open B, come back to A and
 * the text is there; a send empties the input and drops the draft.
 *
 * Harness: `features/chat-input/ui/NewChatInput.test.tsx`'s (socket client
 * for the speaking-mode loop, MSW for the models list and voice settings).
 */
import { useState, type ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { NewChatInput } from '@/features/chat-input';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext, createSocketClient } from '@/shared/api/socket/client';
import type { SocketIoFactory } from '@/shared/api/socket/client';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { readDraft, writeDraft } from '@/shared/lib/composerDrafts';
import { server } from '@/test/setup';

import { chatDraftKey, rememberConversationDraftIds } from './ChatBox.draft';
import { unwrapChatBoxConversation } from './ChatBox.props';

const BASE = '/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function makeSocketClient() {
  const fake = { on: vi.fn(), off: vi.fn(), emit: vi.fn(), disconnect: vi.fn(), io: { on: vi.fn(), off: vi.fn() } };
  const ioFactory = vi.fn(() => fake as unknown as ReturnType<SocketIoFactory>) as unknown as SocketIoFactory;
  return createSocketClient({ url: 'http://socket.test', ioFactory });
}

/** One composer and a conversation switcher, as `ChatBox` passes the key. */
function Chat({ onSend }: { readonly onSend: (question: string) => void }): ReactElement {
  const [conversationId, setConversationId] = useState<number | undefined>(1);
  return (
    <>
      <button type="button" onClick={() => setConversationId(1)}>open A</button>
      <button type="button" onClick={() => setConversationId(2)}>open B</button>
      <button type="button" onClick={() => setConversationId(undefined)}>new chat</button>
      <NewChatInput
        agentEditor={{ activeParticipant: undefined, activeParticipantDetails: undefined, onSelectVersion: vi.fn(), variables: [], onChangeVariables: vi.fn() }}
        content={{ clearInputAfterSubmit: true, draftKey: chatDraftKey({ projectId: 7, conversationId, scratch: [] }) }}
        callbacks={{ onSend }}
      />
    </>
  );
}

function renderChat(onSend: (question: string) => void): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const socketClient = makeSocketClient();
  function Root(): ReactElement {
    return (
      <QueryClientProvider client={queryClient}>
        <SocketClientContext.Provider value={socketClient}>
          <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
            <Chat onSend={onSend} />
          </ThemeProvider>
        </SocketClientContext.Provider>
      </QueryClientProvider>
    );
  }
  const router = createRouter({
    routeTree: createRootRoute({ component: Root }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => '7' } },
  });
  render(<RouterProvider router={router} />);
}

const textarea = (): HTMLTextAreaElement => screen.getByTestId('chat-message-input') as HTMLTextAreaElement;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  window.localStorage.clear();
  vi.stubGlobal('ResizeObserver', class { observe(): void {} unobserve(): void {} disconnect(): void {} });
  server.use(
    http.get(`${BASE}/configurations/models/:projectId`, () => HttpResponse.json({ items: [], total: 0 })),
    http.get('*/elitea_core/platform_settings/prompt_lib', () => HttpResponse.json({ voice_features_enabled: false })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllGlobals();
  window.localStorage.clear();
});

describe('chatDraftKey', () => {
  it('names the conversation, or the project\'s new chat before one exists', () => {
    expect(chatDraftKey({ projectId: 7, conversationId: 12, scratch: [] })).toBe('chat:7:12');
    expect(chatDraftKey({ projectId: '7', conversationId: undefined, scratch: [] })).toBe('chat:7:new');
  });

  it('names the conversation being opened while it loads, so the key does not flip when it arrives (#2221)', () => {
    const loading = chatDraftKey({ projectId: 7, conversationId: undefined, pendingConversationId: 12, scratch: [] });
    const loaded = chatDraftKey({ projectId: 7, conversationId: 12, pendingConversationId: 12, scratch: [] });
    expect(loading).toBe('chat:7:12');
    expect(loading).toBe(loaded);
    // The URL's conversation wins over a stale one still on screen (12 -> 13).
    expect(chatDraftKey({ projectId: 7, conversationId: 12, pendingConversationId: 13, scratch: [] })).toBe('chat:7:13');
    // `ChatBox` receives the id through its `conversation` prop group.
    expect(unwrapChatBoxConversation({ pendingId: 13 }).pendingConversationId).toBe(13);
    // No conversation in the URL: still the project's new chat.
    expect(chatDraftKey({ projectId: 7, conversationId: undefined, pendingConversationId: undefined, scratch: [] })).toBe('chat:7:new');
  });

  it('a uuid deep link keeps one key from the first render through the load, and the draft is shared with the numeric id (#2229)', () => {
    const uuid = '0b9f8c1e-5d6a-4c7b-9e21-3a4b5c6d7e8f';
    const loading = chatDraftKey({ projectId: 7, conversationId: undefined, pendingConversationId: uuid, scratch: [] });
    const loaded = chatDraftKey({ projectId: 7, conversationId: 12, pendingConversationId: uuid, scratch: [] });
    expect(loading).toBe(`chat:7:${uuid}`);
    expect(loaded).toBe(loading);
    writeDraft('chat:7:12', 'typed in the numeric form');
    rememberConversationDraftIds({ projectId: 7, conversationId: 12, conversationUuid: uuid, scratch: [] });
    expect(readDraft(`chat:7:${uuid}`)).toBe('typed in the numeric form');
  });

  it('a numeric deep link names the conversation from the first render and registers its uuid on load (#2229)', () => {
    expect(chatDraftKey({ projectId: 7, conversationId: undefined, pendingConversationId: '12', scratch: [] })).toBe('chat:7:12');
    writeDraft('chat:7:u-x', 'typed under the uuid');
    rememberConversationDraftIds({ projectId: 7, conversationId: 12, conversationUuid: 'u-x', scratch: [] });
    expect(readDraft('chat:7:12')).toBe('typed under the uuid');
    rememberConversationDraftIds({ projectId: 7, conversationId: 12, conversationUuid: 'u-y', scratch: [{ editorTest: true }] });
    expect(readDraft('chat:7:u-y')).toBe('');
  });

  it('keeps no draft for an editor test chat or without a project', () => {
    expect(chatDraftKey({ projectId: 7, conversationId: 12, scratch: [undefined, { editorTest: true }] })).toBeUndefined();
    expect(chatDraftKey({ projectId: undefined, conversationId: 12, scratch: [] })).toBeUndefined();
  });
});

describe('chat composer drafts', () => {
  it('keeps text typed while a conversation is still loading (#2221)', async () => {
    function Opening(): ReactElement {
      const [active, setActive] = useState<number | undefined>(undefined);
      return (
        <>
          <button type="button" onClick={() => setActive(12)}>conversation loaded</button>
          <NewChatInput
            agentEditor={{ activeParticipant: undefined, activeParticipantDetails: undefined, onSelectVersion: vi.fn(), variables: [], onChangeVariables: vi.fn() }}
            content={{ clearInputAfterSubmit: true, draftKey: chatDraftKey({ projectId: 7, conversationId: active, pendingConversationId: 12, scratch: [] }) }}
            callbacks={{ onSend: vi.fn() }}
          />
        </>
      );
    }
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    const socketClient = makeSocketClient();
    function Root(): ReactElement {
      return (
        <QueryClientProvider client={queryClient}>
          <SocketClientContext.Provider value={socketClient}>
            <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
              <Opening />
            </ThemeProvider>
          </SocketClientContext.Provider>
        </QueryClientProvider>
      );
    }
    const router = createRouter({
      routeTree: createRootRoute({ component: Root }),
      history: createMemoryHistory({ initialEntries: ['/'] }),
      context: { auth: { getSelectedProjectId: () => '7' } },
    });
    render(<RouterProvider router={router} />);
    await waitFor(() => expect(textarea()).toBeInTheDocument());
    fireEvent.change(textarea(), { target: { value: 'typed before it loaded' } });
    fireEvent.click(screen.getByText('conversation loaded'));
    await waitFor(() => expect(screen.getByText('conversation loaded')).toBeInTheDocument());
    expect(textarea()).toHaveValue('typed before it loaded');
  });

  it('brings back what was typed in a conversation after another one was open', async () => {
    renderChat(vi.fn());
    await waitFor(() => expect(textarea()).toBeInTheDocument());
    fireEvent.change(textarea(), { target: { value: 'half a thought for A' } });

    fireEvent.click(screen.getByText('open B'));
    await waitFor(() => expect(textarea()).toHaveValue(''));
    fireEvent.change(textarea(), { target: { value: 'something for B' } });

    fireEvent.click(screen.getByText('open A'));
    await waitFor(() => expect(textarea()).toHaveValue('half a thought for A'));
    fireEvent.click(screen.getByText('open B'));
    await waitFor(() => expect(textarea()).toHaveValue('something for B'));
  });

  it('drops the draft once it is sent', async () => {
    const onSend = vi.fn();
    renderChat(onSend);
    await waitFor(() => expect(textarea()).toBeInTheDocument());
    fireEvent.change(textarea(), { target: { value: 'send me' } });
    fireEvent.keyDown(textarea(), { key: 'Enter' });
    expect(onSend).toHaveBeenCalledWith('send me', 'send me');
    await waitFor(() => expect(textarea()).toHaveValue(''));

    fireEvent.click(screen.getByText('open B'));
    await waitFor(() => expect(textarea()).toHaveValue(''));
    fireEvent.click(screen.getByText('open A'));
    // Past the save delay: nothing comes back for A.
    await act(async () => { await new Promise((resolve) => { setTimeout(resolve, 350); }); });
    expect(textarea()).toHaveValue('');
  });
});
