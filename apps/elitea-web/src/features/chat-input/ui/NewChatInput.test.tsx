import { createRef, type ReactElement } from 'react';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext, createSocketClient } from '@/shared/api/socket/client';
import type { SocketIoFactory } from '@/shared/api/socket/client';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { resetConfigForTests } from '@/shared/config/get-config';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';

import { NewChatInput } from './NewChatInput';
import type { NewChatInputHandle, NewChatInputProps } from './NewChatInput';

const BASE = '/api/v2';

/**
 * `NewChatInput` mounts `useSpeakingModeLoop`, which unconditionally mounts
 * `useStreamingSpeechRecognition` (needs a `SocketClient`) and calls
 * `useModelsList` (needs an MSW handler) — same harness ingredients as
 * `lib/hooks/useSpeakingModeLoop.test.ts`'s own, reused at the minimum this
 * file needs (every scenario here keeps `voice.isSpeakingMode: false`, so
 * recording never actually starts — no Web Audio fakes required).
 */
function createFakeSocket() {
  const listeners = new Map<string, Set<(...args: unknown[]) => void>>();
  const fake = {
    on: (event: string, handler: (...args: unknown[]) => void) => {
      let set = listeners.get(event);
      if (!set) {
        set = new Set();
        listeners.set(event, set);
      }
      set.add(handler);
    },
    off: (event: string, handler: (...args: unknown[]) => void) => listeners.get(event)?.delete(handler),
    emit: () => fake,
    disconnect: vi.fn(),
    io: { on: vi.fn(), off: vi.fn() },
  };
  return fake;
}

function makeSocketClient() {
  const fakeSocket = createFakeSocket();
  const ioFactory = vi.fn(() => fakeSocket as unknown as ReturnType<SocketIoFactory>) as unknown as SocketIoFactory;
  return createSocketClient({ url: 'http://socket.test', ioFactory });
}

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderInput(ui: ReactElement, options: { projectId?: string } = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const socketClient = makeSocketClient();
  function RootComponent() {
    return (
      <QueryClientProvider client={queryClient}>
        <SocketClientContext.Provider value={socketClient}>
          <ThemeProvider
            theme={theme}
            defaultMode={DEFAULT_COLOR_SCHEME}
          >
            {ui}
          </ThemeProvider>
        </SocketClientContext.Provider>
      </QueryClientProvider>
    );
  }
  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => options.projectId } },
  });
  return render(<RouterProvider router={router} />);
}

class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

const ALL_ENV_KEYS = ['VITE_SERVER_URL', 'VITE_BASE_URI', 'VITE_SOCKET_SERVER', 'VITE_SOCKET_PATH', 'VITE_PUBLIC_PROJECT_ID'] as const;
const g = globalThis as unknown as Record<string, unknown>;
const realProcessEnv = (g['process'] as { env: Record<string, string | undefined> }).env;

function getTextarea(): HTMLTextAreaElement {
  return screen.getByTestId('chat-message-input') as HTMLTextAreaElement;
}

function baseProps(overrides: Partial<NewChatInputProps> = {}): NewChatInputProps {
  return {
    agentEditor: {
      activeParticipant: undefined,
      activeParticipantDetails: undefined,
      onSelectVersion: vi.fn(),
      variables: [],
      onChangeVariables: vi.fn(),
    },
    ...overrides,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  resetConfigForTests();
  for (const key of ALL_ENV_KEYS) delete realProcessEnv[key];
  vi.stubEnv('VITE_SERVER_URL', BASE);
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_SOCKET_SERVER', 'http://localhost');
  vi.stubEnv('VITE_SOCKET_PATH', '/socket.io');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', 'public-proj');
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
  const originalDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetWidth');
  Object.defineProperty(HTMLElement.prototype, 'offsetWidth', { configurable: true, value: 800 });
  restoreOffsetWidth = () => {
    if (originalDescriptor) Object.defineProperty(HTMLElement.prototype, 'offsetWidth', originalDescriptor);
  };
  server.use(getPermissionListMockHandler([{ name: PERMISSIONS.applications.update, enabled: true }]));
  server.use(http.get(`${BASE}/configurations/models/:projectId`, () => HttpResponse.json({ items: [], total: 0 })));
});

let restoreOffsetWidth: (() => void) | undefined;

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
  resetConfigForTests();
  restoreOffsetWidth?.();
});

// `VoiceControlButton` reads the platform's Voice Features switches
// (`useVoiceFeatureFlags`, A14/issue 200) — it used to hardcode them as module
// constants. That is a real network read, and `src/test/setup.ts` runs MSW with
// `onUnhandledRequest: 'error'`, so the endpoint is stubbed here rather than
// left to race the test's own teardown.
beforeEach(() => {
  server.use(
    http.get('*/elitea_core/platform_settings/prompt_lib', () =>
      HttpResponse.json({ voice_features_enabled: true, voice_features_temporarily_disabled: false }),
    ),
  );
});

describe('NewChatInput', () => {
  it('sends the typed question on Enter', async () => {
    const onSend = vi.fn();
    renderInput(<NewChatInput {...baseProps({ callbacks: { onSend } })} />, { projectId: 'proj-1' });
    const textarea = await waitFor(() => getTextarea());
    fireEvent.change(textarea, { target: { value: 'hello' } });
    fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onSend).toHaveBeenCalledWith('hello', 'hello');
  });

  it('renders the injected modelSelector slot when there is no application/pipeline participant', async () => {
    renderInput(
      <NewChatInput {...baseProps({ slots: { modelSelector: <div data-testid="model-selector" /> } })} />,
      { projectId: 'proj-1' },
    );
    await waitFor(() => expect(screen.getByTestId('model-selector')).toBeInTheDocument());
    expect(screen.queryByRole('button', { name: 'Switch Agent' })).not.toBeInTheDocument();
  });

  it('renders AgentEditorPanel (not the modelSelector slot) for an application participant outside the agents page', async () => {
    renderInput(
      <NewChatInput
        {...baseProps({
          agentEditor: {
            activeParticipant: {
              id: 'p1',
              entityName: 'application',
              entityMeta: { id: 'a1', projectId: 'proj-1' },
              entitySettings: { agentType: 'chat' },
            },
            activeParticipantDetails: { id: 'a1', name: 'My Agent', versions: [] },
            onSelectVersion: vi.fn(),
            variables: [],
            onChangeVariables: vi.fn(),
          },
          slots: { modelSelector: <div data-testid="model-selector" /> },
        })}
      />,
      { projectId: 'proj-1' },
    );
    await waitFor(() => expect(screen.getByText('My Agent')).toBeInTheDocument());
    expect(screen.queryByTestId('model-selector')).not.toBeInTheDocument();
  });

  it('silently no-ops a drop when no attachmentButtonRef is injected, matching baseline', async () => {
    const onAttachFiles = vi.fn();
    renderInput(
      <NewChatInput {...baseProps({ attachments: { onAttachFiles } })} />,
      { projectId: 'proj-1' },
    );
    const textarea = await waitFor(() => getTextarea());
    const container = textarea.closest('[data-testid="chat-input"]')?.parentElement?.parentElement;
    expect(container).toBeTruthy();
    const file = new File(['x'], 'a.txt');
    expect(() => fireEvent.drop(container as Element, { dataTransfer: { files: [file] } })).not.toThrow();
    expect(onAttachFiles).not.toHaveBeenCalled();
  });

  /**
   * The staged-file chips. `UserInput` renders them from `slots.attachmentList`
   * and `NewChatInput` is the only thing that can put that slot there; before
   * this test it did not, so `renderAttachmentList` was handed `undefined`
   * every time and returned `null`. Nothing failed — the composer just never
   * showed a picked file.
   */
  it('forwards slots.attachmentList and the staged items down to UserInput', async () => {
    const file = new File(['x'], 'brief.txt');
    const onDeleteAttachment = vi.fn();
    renderInput(
      <NewChatInput
        {...baseProps({
          attachments: { items: [file], onDeleteAttachment },
          slots: {
            attachmentList: (slotProps) => (
              <button
                type="button"
                onClick={() => slotProps.onDeleteAttachment?.(0)}
              >
                {`chip:${slotProps.attachments.length}`}
              </button>
            ),
          },
        })}
      />,
      { projectId: 'proj-1' },
    );
    const chip = await waitFor(() => screen.getByRole('button', { name: 'chip:1' }));
    fireEvent.click(chip);
    expect(onDeleteAttachment).toHaveBeenCalledWith(0);
  });

  it('renders no attachment list while nothing is staged', async () => {
    const attachmentList = vi.fn(() => <span>chips</span>);
    renderInput(
      <NewChatInput {...baseProps({ attachments: { items: [] }, slots: { attachmentList } })} />,
      { projectId: 'proj-1' },
    );
    await waitFor(() => getTextarea());
    expect(attachmentList).not.toHaveBeenCalled();
  });

  it('exposes an imperative handle proxying to the underlying UserInput, plus pauseSpeakingMode', async () => {
    const ref = createRef<NewChatInputHandle>();
    renderInput(<NewChatInput
      {...baseProps()}
      ref={ref}
    />, { projectId: 'proj-1' });
    await waitFor(() => getTextarea());
    expect(typeof ref.current?.focus).toBe('function');
    expect(typeof ref.current?.pauseSpeakingMode).toBe('function');
    expect(() => ref.current?.pauseSpeakingMode()).not.toThrow();
  });
});
