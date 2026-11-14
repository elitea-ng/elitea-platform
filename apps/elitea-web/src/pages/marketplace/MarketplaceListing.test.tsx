import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../test/setup';

import { MarketplaceListing } from './MarketplaceListing';

const AGENT = {
  project_id: '1',
  id: '7',
  name: 'Release Notes Writer',
  description: 'Drafts release notes from merged pull requests.',
  version_id: '70',
  version_name: 'v2',
  agent_type: 'agent',
  meta: { category: 'Development' },
  tags: [],
  likes: 4,
  is_liked: false,
};

let createBodies: unknown[] = [];
let participantBodies: unknown[] = [];

function mockAgent(): void {
  createBodies = [];
  participantBodies = [];
  server.use(
    http.get('/api/v2/elitea_core/public_applications/prompt_lib', () => HttpResponse.json({ rows: [AGENT], total: 1 })),
    http.post('/api/v2/elitea_core/conversations/prompt_lib/:projectId', async ({ request }) => {
      createBodies.push(await request.json());
      return HttpResponse.json({ id: '55', name: AGENT.name }, { status: 201 });
    }),
    http.post('/api/v2/elitea_core/participants/prompt_lib/:projectId/:conversationId', async ({ request }) => {
      participantBodies.push(await request.json());
      return HttpResponse.json([]);
    }),
    http.get('/api/v2/elitea_core/public_application/prompt_lib/7/v2', () =>
      HttpResponse.json({
        id: '7',
        name: AGENT.name,
        description: AGENT.description,
        version_details: { id: '70', application_id: '7', name: 'v2', status: 'published', agent_type: 'agent', instructions: 'Write kindly.', welcome_message: 'Hello there', conversation_starters: ['Summarise the last release'] },
      }),
    ),
  );
}

function mount(path: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <Outlet />
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/marketplace/$kind/$listingId', component: MarketplaceListing }),
      createRoute({ getParentRoute: () => rootRoute, path: '/marketplace', component: () => <p data-testid="marketplace-home" /> }),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: () => <p data-testid="chat-page" /> }),
    ]),
    history: createMemoryHistory({ initialEntries: [path] }),
    // The project the conversation starts in.
    context: { auth: { getSelectedProjectId: () => '5' } },
  } as never);
  renderWithTheme(<RouterProvider router={router} />);
  return router;
}

describe('MarketplaceListing', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('shows an agent as a page with its version, overview and starters, and no modal', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockAgent();
    mount('/marketplace/agents/7');

    expect(await screen.findByTestId('marketplace-listing-name')).toHaveTextContent('Release Notes Writer');
    expect(screen.getByTestId('marketplace-listing-version')).toHaveTextContent('v2');
    expect(screen.getByText('Drafts release notes from merged pull requests.')).toBeInTheDocument();
    expect(screen.getByText('Category: Development')).toBeInTheDocument();
    expect(await screen.findByText('Summarise the last release')).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('keeps Try in chat: it starts a conversation with that agent, not just the chat page', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockAgent();
    const router = mount('/marketplace/agents/7');
    const user = userEvent.setup();

    await user.click(await screen.findByTestId('marketplace-try-in-chat'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/55'));
    expect(createBodies).toEqual([{ name: 'Release Notes Writer', participants: [] }]);
    // The participant is the public copy at its published version, sent as the bare array the route takes.
    expect(participantBodies).toEqual([
      [{ entity_name: 'application', entity_meta: { id: '7', name: 'Release Notes Writer', project_id: '1' }, entity_settings: { version_id: '70' } }],
    ]);
  });

  it('shows the instructions on request', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockAgent();
    mount('/marketplace/agents/7');
    const user = userEvent.setup();

    await user.click(await screen.findByTestId('marketplace-show-context'));
    expect(await screen.findByText('Write kindly.')).toBeInTheDocument();
  });

  it('says so, with one way back, when the listing is gone', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockAgent();
    mount('/marketplace/agents/999');

    expect(await screen.findByTestId('marketplace-listing-not-found')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to the Marketplace' })).toHaveAttribute('href', '/marketplace');
  });

  it('shows a skill as a page with its tags and instructions, and Add to an agent', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.get('/api/v2/elitea_core/public_skill/prompt_lib/41', () =>
        HttpResponse.json({
          id: 41,
          name: 'PR Reviewer',
          description: 'Reviews pull requests',
          versions: [{ id: 6, name: 'v1.0', status: 'published' }],
          version_details: { id: 6, name: 'v1.0', status: 'published', instructions: 'Check the diff.', tags: ['Development'] },
        }),
      ),
    );
    mount('/marketplace/skills/41');

    expect(await screen.findByTestId('marketplace-listing-name')).toHaveTextContent('PR Reviewer');
    expect(screen.getByText('Development')).toBeInTheDocument();
    expect(screen.getByTestId('marketplace-skill-instructions')).toHaveTextContent('Check the diff.');
    expect(screen.getByTestId('marketplace-add-skill')).toHaveTextContent('Add to an agent');
  });
});
