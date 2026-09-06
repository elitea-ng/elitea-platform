import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { getGetPublicApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';
import type { ApplicationData } from '../types';

import AgentModal from './AgentModal';

const AGENT: ApplicationData = {
  project_id: 'pub-1',
  id: '42',
  name: 'Research Agent',
  description: 'Finds things.',
  version_id: 'v-1',
  version_name: 'v1',
  agent_type: 'agent',
  meta: null,
};

/**
 * Real `<RouterProvider>` + `<QueryClientProvider>` harness — `AgentModal`
 * now calls `useNavigate()` unconditionally (finding 3's fix) and
 * `useGetPublicApplication` via `useAgentVersionDetail` (finding 2's fix),
 * both of which throw without their respective providers. Mirrors
 * `pages/user-public/ui/ApplicationsPanel.test.tsx`'s own
 * `withQueryClient` helper for the identical reason.
 */
function withProviders(ui: ReactNode) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => <QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>,
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
  });
  return { ...renderWithTheme(<RouterProvider router={router} />), router };
}

describe('AgentModal', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('fetches and renders the real welcome message and conversation starters (adversarial-review fix, cluster A13-agents-hub, finding 2)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      getGetPublicApplicationMockHandler({
        id: '42',
        name: 'Research Agent',
        description: 'Finds things.',
        version_details: {
          id: 'v-1',
          application_id: '42',
          name: 'v1',
          status: 'published',
          welcome_message: 'Hi, I can help you research.',
          conversation_starters: ['Find me a paper', 'Summarize this'],
        },
      }),
    );

    withProviders(<AgentModal open agent={AGENT} onClose={() => {}} />);

    expect(await screen.findByText('Hi, I can help you research.')).toBeInTheDocument();
    expect(await screen.findByText('Find me a paper')).toBeInTheDocument();
    expect(screen.getByText('Summarize this')).toBeInTheDocument();
  });

  it('still shows the empty-state copy when the fetched version has no welcome message or starters', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      getGetPublicApplicationMockHandler({
        id: '42',
        name: 'Research Agent',
        description: 'Finds things.',
        version_details: { id: 'v-1', application_id: '42', name: 'v1', status: 'published' },
      }),
    );

    withProviders(<AgentModal open agent={AGENT} onClose={() => {}} />);

    expect(
      await screen.findByText('No welcome message set – the agent will start without a greeting.'),
    ).toBeInTheDocument();
    expect(
      screen.getByText('No predefined conversation starters – just type your request to begin.'),
    ).toBeInTheDocument();
  });

  it('navigates to /chat and closes the modal when "Start conversation" is clicked with no injected handler (adversarial-review fix, finding 3)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getGetPublicApplicationMockHandler());
    const onClose = vi.fn();
    const user = userEvent.setup();

    const { router } = withProviders(<AgentModal open agent={AGENT} onClose={onClose} />);

    await user.click(await screen.findByText('Start conversation'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/chat'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('calls an injected onStartConversation instead of navigating, and still closes the modal', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getGetPublicApplicationMockHandler());
    const onClose = vi.fn();
    const onStartConversation = vi.fn();
    const user = userEvent.setup();

    withProviders(
      <AgentModal open agent={AGENT} onClose={onClose} onStartConversation={onStartConversation} />,
    );

    await user.click(await screen.findByText('Start conversation'));

    expect(onStartConversation).toHaveBeenCalledWith(AGENT, undefined);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('closes the modal when a conversation-starter pill is clicked (adversarial-review fix, finding 3 — this previously did not close the modal at all)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      getGetPublicApplicationMockHandler({
        id: '42',
        name: 'Research Agent',
        description: 'Finds things.',
        version_details: {
          id: 'v-1',
          application_id: '42',
          name: 'v1',
          status: 'published',
          conversation_starters: ['Find me a paper'],
        },
      }),
    );
    const onClose = vi.fn();
    const onStartConversation = vi.fn();
    const user = userEvent.setup();

    withProviders(
      <AgentModal open agent={AGENT} onClose={onClose} onStartConversation={onStartConversation} />,
    );

    await user.click(await screen.findByText('Find me a paper'));

    expect(onStartConversation).toHaveBeenCalledWith(AGENT, 'Find me a paper');
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('registers and cleans up a window resize listener to keep the small-height layout live (adversarial-review fix, finding 11 — previously computed once at mount and never updated)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getGetPublicApplicationMockHandler());
    const addSpy = vi.spyOn(window, 'addEventListener');
    const removeSpy = vi.spyOn(window, 'removeEventListener');

    const { unmount } = withProviders(<AgentModal open agent={AGENT} onClose={() => {}} />);
    await screen.findByText('Research Agent');

    expect(addSpy.mock.calls.some(([type]) => type === 'resize')).toBe(true);

    unmount();
    expect(removeSpy.mock.calls.some(([type]) => type === 'resize')).toBe(true);

    addSpy.mockRestore();
    removeSpy.mockRestore();
  });
  // The dismiss control is an icon-only button. Without an `aria-label` it
  // announces as a bare "button" beside the like button.
  it('names the close button and closes the modal when it is clicked', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getGetPublicApplicationMockHandler());
    const onClose = vi.fn();
    const user = userEvent.setup();

    withProviders(<AgentModal open agent={AGENT} onClose={onClose} />);
    await screen.findByText('Research Agent');

    await user.click(screen.getByRole('button', { name: 'Close' }));

    expect(onClose).toHaveBeenCalledTimes(1);
  });

  /**
   * REGRESSION guard. The port kept the "Show context" text and its click
   * handler, but it discarded the state getter and rendered no dialog. The
   * control looked live and did nothing. Baseline: `AgentModal.jsx:263-269`.
   */
  it('opens the context dialog with the agent instructions when "Show context" is clicked', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      getGetPublicApplicationMockHandler({
        id: '42',
        name: 'Research Agent',
        description: 'Finds things.',
        version_details: {
          id: 'v-1',
          application_id: '42',
          name: 'v1',
          status: 'published',
          instructions: 'You are a helpful research assistant.',
        },
      }),
    );
    const user = userEvent.setup();

    withProviders(<AgentModal open agent={AGENT} onClose={() => {}} />);
    await screen.findByText('Research Agent');
    // The instructions are not on screen until the dialog opens.
    expect(screen.queryByText('You are a helpful research assistant.')).not.toBeInTheDocument();

    await user.click(screen.getByText('Show context'));

    expect(await screen.findByText('Context')).toBeInTheDocument();
    expect(await screen.findByText('You are a helpful research assistant.')).toBeInTheDocument();
  });
});

/**
 * #80's second adjacent gap — the agent share link.
 *
 * `routes/_shell/elitea-catalog.tsx` validates `agentId`, `AgentHub` reads it
 * back and opens the modal, and `routes/_shell/agents-hub.tsx` redirects the
 * legacy path carrying the same key. Every one of those is the READ half. The
 * WRITE half — the button that produces such a link — was dropped in the #71
 * pull request along with the `AGENT_ID` constant, so the app could open a
 * link nobody could make.
 */
describe('AgentModal — the share link', () => {
  afterEach(() => {
    Reflect.deleteProperty(globalThis.navigator, 'clipboard');
  });

  function stubClipboard(): ReturnType<typeof vi.fn<(value: string) => Promise<void>>> {
    const writeText = vi.fn<(value: string) => Promise<void>>().mockResolvedValue(undefined);
    // jsdom ships no `navigator.clipboard`, and the property is not writable.
    Object.defineProperty(globalThis.navigator, 'clipboard', { value: { writeText }, configurable: true });
    return writeText;
  }

  it('copies an absolute link carrying this agent’s id', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const writeText = stubClipboard();

    withProviders(
      <AgentModal
        open
        onClose={vi.fn()}
        agent={AGENT}
      />,
    );

    await userEvent.click(await screen.findByTestId('agent-modal-copy-link'));

    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    const link = writeText.mock.calls[0]?.[0] ?? '';
    // ABSOLUTE — the baseline builds `${protocol}//${host}${path}` because the
    // whole point is a link somebody can paste elsewhere.
    expect(link.startsWith(window.location.origin)).toBe(true);
    // The catalogue path, the agents tab, and THIS agent's id — the exact
    // three the read half needs. `agentId` is the key
    // `routes/_shell/elitea-catalog.tsx` validates.
    expect(link).toContain('/elitea-catalog');
    expect(link).toContain('tab=agents');
    expect(link).toContain('agentId=42');
  });

  it('offers no copy button for an agent with no id', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });

    withProviders(
      <AgentModal
        open
        onClose={vi.fn()}
        agent={{ ...AGENT, id: '' }}
      />,
    );

    await screen.findByText('Research Agent');
    // A button that copies a link to nothing is worse than no button.
    expect(screen.queryByTestId('agent-modal-copy-link')).not.toBeInTheDocument();
  });
});
