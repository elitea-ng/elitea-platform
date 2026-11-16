import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../test/setup';

import { ConversationSkillsSection, type ConversationSkillsSectionProps } from './ConversationSkillsSection';

interface Pin {
  id: number;
  kind: 'project' | 'listing';
  skill_id?: number | undefined;
  listing_id?: number | undefined;
  listing_version_id?: number | undefined;
  name: string;
  description: string;
  version_name?: string;
  trust: { state: 'trusted' | 'untrusted' };
  pinned_by: number;
  pinned_at: string;
}

function pin(id: number, name: string, overrides: Partial<Pin> = {}): Pin {
  return {
    id,
    kind: 'project',
    skill_id: 100 + id,
    name,
    description: '',
    trust: { state: 'trusted' },
    pinned_by: 7,
    pinned_at: '2026-11-16T10:00:00Z',
    ...overrides,
  };
}

const LIST = '/api/v2/elitea_core/conversation_skills/prompt_lib/:projectId/:conversationId';

interface Backend {
  pins: Pin[];
  posted: unknown[];
  deleted: string[];
  pinStatus: number;
}

function mockBackend(initial: Pin[], overrides: Partial<Backend> = {}): Backend {
  const backend: Backend = { pins: [...initial], posted: [], deleted: [], pinStatus: 201, ...overrides };
  server.use(
    http.get('/api/v2/social/author', () => HttpResponse.json({ id: '7', name: 'Dana' })),
    http.get(LIST, () => HttpResponse.json({ rows: backend.pins, total: backend.pins.length, max: 5 })),
    http.post(LIST, async ({ request }) => {
      const body = (await request.json()) as { skill_id: number };
      backend.posted.push(body);
      if (backend.pinStatus !== 201) return HttpResponse.json({ error: 'nope' }, { status: backend.pinStatus });
      const created = pin(backend.pins.length + 20, `Skill ${body.skill_id}`, { skill_id: body.skill_id });
      backend.pins.push(created);
      return HttpResponse.json(created, { status: 201 });
    }),
    http.delete('/api/v2/elitea_core/conversation_skill/prompt_lib/:projectId/:conversationId/:pinId', ({ params }) => {
      backend.deleted.push(String(params['pinId']));
      backend.pins = backend.pins.filter((entry) => String(entry.id) !== params['pinId']);
      return new HttpResponse(null, { status: 204 });
    }),
    http.get('/api/v2/elitea_core/skills/prompt_lib/:projectId', () =>
      HttpResponse.json({
        rows: [
          { id: 101, name: 'Triage' },
          { id: 200, name: 'Release notes' },
          { id: 201, name: 'Changelog' },
        ],
        total: 3,
      }),
    ),
  );
  return backend;
}

function mount(props: Partial<ConversationSkillsSectionProps> = {}): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const section = (): ReactNode => (
    <ConversationSkillsSection projectId="5" conversationId="conv-1" {...props} />
  );
  const rootRoute = createRootRoute({ component: section });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/marketplace', component: () => null }),
    ]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  });
  renderWithTheme(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe('ConversationSkillsSection', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('lists the pins apart from the participants, with who pinned them, a Marketplace label and an untrusted badge', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockBackend([
      pin(1, 'Triage'),
      pin(2, 'Forked', { trust: { state: 'untrusted' }, pinned_by: 9 }),
      pin(3, 'Reviewer', { kind: 'listing', skill_id: undefined, listing_id: 4, listing_version_id: 6, version_name: 'v1.2', trust: { state: 'untrusted' } }),
    ]);
    mount({ personName: (id) => (id === 9 ? 'Alex' : undefined) });

    const section = await screen.findByTestId('conversation-skills');
    expect(await within(section).findByText('Triage')).toBeInTheDocument();
    expect(within(section).getByText('Skills (3)')).toBeInTheDocument();
    expect(within(section).getByTestId('conversation-skill-pinned-by-1')).toHaveTextContent('Pinned by you');
    expect(within(section).getByTestId('conversation-skill-pinned-by-2')).toHaveTextContent('Pinned by Alex');
    expect(within(section).getByTestId('conversation-skill-untrusted-2')).toHaveTextContent('Untrusted');
    expect(within(section).getByTestId('conversation-skill-untrusted-3')).toBeInTheDocument();
    expect(within(section).queryByTestId('conversation-skill-untrusted-1')).not.toBeInTheDocument();
    expect(within(section).getByText('Marketplace v1.2')).toBeInTheDocument();
  });

  it('shows no section for a conversation that is not saved yet', () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockBackend([]);
    mount({ conversationId: undefined });
    expect(screen.queryByTestId('conversation-skills')).not.toBeInTheDocument();
  });

  it('says so when there are no pins', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockBackend([]);
    mount();
    expect(await screen.findByTestId('conversation-skills-empty')).toHaveTextContent('No skills pinned.');
  });

  it('adds a project skill that is not pinned yet and shows the new list', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const backend = mockBackend([pin(1, 'Triage')]);
    mount();
    const user = userEvent.setup();

    await user.click(await screen.findByTestId('conversation-skills-add'));
    const menu = await screen.findByTestId('conversation-skills-menu');
    // Triage (id 101) is pinned already, so it is not offered again.
    expect(await within(menu).findByText('Changelog')).toBeInTheDocument();
    expect(within(menu).queryByText('Triage')).not.toBeInTheDocument();
    await user.click(within(menu).getByTestId('conversation-skills-option-200'));

    await waitFor(() => expect(backend.posted).toEqual([{ skill_id: 200 }]));
    expect(await screen.findByText('Skills (2)')).toBeInTheDocument();
  });

  it('removes a pin', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const backend = mockBackend([pin(1, 'Triage'), pin(2, 'Release notes')]);
    mount();
    const user = userEvent.setup();

    await user.click(await screen.findByRole('button', { name: 'Remove Triage from this conversation' }));
    await waitFor(() => expect(backend.deleted).toEqual(['1']));
    expect(await screen.findByText('Skills (1)')).toBeInTheDocument();
    expect(screen.queryByText('Triage')).not.toBeInTheDocument();
  });

  it('stops offering Add at five skills', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockBackend([pin(1, 'A'), pin(2, 'B'), pin(3, 'C'), pin(4, 'D'), pin(5, 'E')]);
    mount();
    await screen.findByText('Skills (5)');
    expect(screen.getByTestId('conversation-skills-add')).toBeDisabled();
  });

  it('gives the server\'s refusal in words: five skills, or a skill the caller cannot use', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const backend = mockBackend([pin(1, 'Triage')], { pinStatus: 409 });
    mount();
    const user = userEvent.setup();
    await user.click(await screen.findByTestId('conversation-skills-add'));
    await user.click(await screen.findByTestId('conversation-skills-option-200'));
    expect(await screen.findByTestId('conversation-skills-refusal')).toHaveTextContent('A conversation holds at most five skills.');

    backend.pinStatus = 404;
    await user.click(screen.getByTestId('conversation-skills-add'));
    await user.click(await screen.findByTestId('conversation-skills-option-201'));
    await waitFor(() =>
      expect(screen.getByTestId('conversation-skills-refusal')).toHaveTextContent('That skill is not available to you.'),
    );
  });

  it('only lists for a reader who may not write to the conversation', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockBackend([pin(1, 'Triage')]);
    mount({ readOnly: true });
    await screen.findByText('Triage');
    expect(screen.queryByTestId('conversation-skills-add')).not.toBeInTheDocument();
    expect(screen.queryByTestId('conversation-skill-remove-1')).not.toBeInTheDocument();
  });

  it('says so when the list cannot be read', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.get('/api/v2/social/author', () => HttpResponse.json({ id: '7' })),
      http.get(LIST, () => HttpResponse.json({ error: 'boom' }, { status: 500 })),
    );
    mount();
    expect(await screen.findByTestId('conversation-skills-error')).toHaveTextContent('could not be loaded');
  });
});
