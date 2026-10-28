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
import { server } from '@/test/setup';

import { chatDraftKey } from './ChatBox.draft';

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

  it('keeps no draft for an editor test chat or without a project', () => {
    expect(chatDraftKey({ projectId: 7, conversationId: 12, scratch: [undefined, { editorTest: true }] })).toBeUndefined();
    expect(chatDraftKey({ projectId: undefined, conversationId: 12, scratch: [] })).toBeUndefined();
  });
});

describe('chat composer drafts', () => {
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
