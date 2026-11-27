import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';

import { getGetApplicationMockHandler, getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { getGetCurrentAuthorMockHandler } from '@/shared/api/generated/social/social.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';
import { SidePanelHarness } from '@/test/sidePanelHarness';

import { Applications } from './Applications';
import { renderAgentsRoute } from './__tests__/testRouter';

const globals = globalThis as unknown as Record<string, unknown>;

interface Row {
  readonly id: string;
  readonly name: string;
  readonly status: string;
}

const ROWS: readonly Row[] = [
  { id: '1', name: 'Draft agent', status: 'draft' },
  { id: '2', name: 'Published agent', status: 'published' },
  { id: '3', name: 'Second draft', status: 'draft' },
];

function page(rows: readonly Row[], facets = { statuses: { draft: 2, published: 1 }, origins: { own: 3 } }) {
  return {
    rows: rows.map((row) => ({
      ...row,
      description: '',
      tags: [],
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
      owner_id: 'user-1',
      is_forked: false,
      meta: null,
      has_interrupt: false,
    })),
    total: rows.length,
    page: 1,
    page_size: 20,
    total_pages: 1,
    facets,
  };
}

beforeEach(() => {
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    getListApplicationsMockHandler((info) => {
      const status = new URL(info.request.url).searchParams.get('status');
      return page(status === null ? ROWS : ROWS.filter((row) => row.status === status));
    }),
    getPermissionListMockHandler([{ name: 'models.applications.applications.create', enabled: true }]),
  );
});

afterEach(() => {
  resetGeneratedClient();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
});

describe('AgentsWorkshop (ADR-0051 M1)', () => {
  it('shows the status as a badge in text on every card', async () => {
    renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9' });

    const cards = await screen.findAllByTestId('entity-card');
    expect(cards).toHaveLength(3);
    expect(within(cards[0] as HTMLElement).getByTestId('entity-card-status')).toHaveTextContent(/^(Draft|Published)$/);
  });

  it('offers only the status and origin values that occur, with the server counts', async () => {
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '9' });

    expect(await screen.findByTestId('agent-facet-statuses-draft')).toHaveTextContent('Draft (2)');
    expect(screen.getByTestId('agent-facet-statuses-published')).toHaveTextContent('Published (1)');
    expect(screen.getByTestId('agent-facet-origin-own')).toHaveTextContent('Own (3)');
    // Forked, and the three statuses no agent can hold, are never offered.
    expect(screen.queryByTestId('agent-facet-origin-forked')).toBeNull();
    for (const gone of ['moderation', 'approval', 'rejected', 'on_moderation', 'user_approval']) {
      expect(screen.queryByTestId(`agent-facet-statuses-${gone}`)).toBeNull();
    }
  });

  it('names every sidebar section as a group for screen readers (Status, Origin, Sort, View)', async () => {
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '9' });

    for (const name of ['Status', 'Origin', 'Sort', 'View']) {
      expect(await screen.findByRole('group', { name })).toBeInTheDocument();
    }
    expect(within(screen.getByRole('group', { name: 'Status' })).getByTestId('agent-facet-statuses-draft')).toBeInTheDocument();
  });

  it('asks the server for the chosen status and keeps the facet counts of the whole project', async () => {
    const seen: (string | null)[] = [];
    server.use(
      getListApplicationsMockHandler((info) => {
        const status = new URL(info.request.url).searchParams.get('status');
        seen.push(status);
        return page(status === null ? ROWS : ROWS.filter((row) => row.status === status));
      }),
    );
    const user = userEvent.setup();
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '9' });

    await user.click(await screen.findByTestId('agent-facet-statuses-draft'));

    await waitFor(() => expect(seen).toContain('draft'));
    await waitFor(() => expect(screen.getAllByTestId('entity-card')).toHaveLength(2));
    expect(screen.getByTestId('agent-facet-statuses-draft')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('agent-facet-statuses-published')).toHaveTextContent('Published (1)');
  });

  /** Usability task T5: show only drafts, then clear the filter with the sidebar closed. */
  it('T5: the summary bar shows the active status filter and clears it when the sidebar is closed', async () => {
    const user = userEvent.setup();
    // No side panel in the tree: the sidebar is closed, so the summary bar carries the filter.
    const { router } = renderAgentsRoute(<Applications />, '/agents/all?statuses=draft', { projectId: '9' });

    const bar = await screen.findByTestId('agent-summary-bar');
    await waitFor(() => expect(within(bar).getByTestId('agent-summary-chip-statuses-draft')).toHaveTextContent('Status: Draft'));
    await waitFor(() => expect(screen.getAllByTestId('entity-card')).toHaveLength(2));

    await user.click(within(bar).getByTestId('agent-summary-clear'));

    await waitFor(() => expect(screen.getAllByTestId('entity-card')).toHaveLength(3));
    expect(screen.queryByTestId('agent-summary-chip-statuses-draft')).toBeNull();
    expect(router.state.location.search).not.toHaveProperty('statuses', ['draft']);
  });

  it('keeps New agent one click away in the summary bar', async () => {
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9' });
    const visited: string[] = [];
    router.subscribe('onBeforeNavigate', (event) => visited.push(event.toLocation.pathname));

    const bar = await screen.findByTestId('agent-summary-bar');
    await user.click(await within(bar).findByRole('button', { name: 'New agent' }));

    await waitFor(() => expect(visited).toContain('/agents/create'));
  });

  it('hides the summary bar while the sidebar is open beside the page', async () => {
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '9' });

    await screen.findByTestId('agent-list-sidebar');
    expect(screen.queryByTestId('agent-summary-bar')).toBeNull();
  });

  it('selects cards with ⌘-click and offers Add tag, Export and Delete in the selection section', async () => {
    const user = userEvent.setup();
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '9' });

    const cards = await screen.findAllByTestId('entity-card');
    await user.keyboard('{Meta>}');
    await user.click(within(cards[0] as HTMLElement).getByTestId('entity-card-name'));
    await user.click(within(cards[1] as HTMLElement).getByTestId('entity-card-name'));
    await user.keyboard('{/Meta}');

    const section = await screen.findByTestId('agent-selection');
    expect(within(section).getByTestId('agent-selection-count')).toHaveTextContent('2 agents selected');
    expect(within(section).getByTestId('agent-selection-add-tag')).toBeInTheDocument();
    expect(within(section).getByTestId('agent-selection-export')).toBeInTheDocument();
    expect(within(section).getByTestId('agent-selection-delete')).toBeInTheDocument();

    await user.click(within(section).getByTestId('agent-selection-clear'));
    await waitFor(() => expect(screen.queryByTestId('agent-selection')).toBeNull());
  });

  it('deletes the selected agents after a confirmation, one request each', async () => {
    const deleted: string[] = [];
    server.use(
      http.delete('*/elitea_core/application/prompt_lib/:projectId/:id', ({ params }) => {
        deleted.push(String(params['id']));
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '9' });

    const cards = await screen.findAllByTestId('entity-card');
    await user.click(within(cards[0] as HTMLElement).getByRole('checkbox'));
    await user.click(within(cards[2] as HTMLElement).getByRole('checkbox'));
    await user.click(await screen.findByTestId('agent-selection-delete'));
    await user.click(await screen.findByRole('button', { name: 'Delete', hidden: false }));

    await waitFor(() => expect(deleted.length).toBe(2));
  });

  it('puts every list control in the List tab: search, New agent, Import, sort and the view switch', async () => {
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '9' });

    const tab = await screen.findByTestId('side-panel-tab-list');
    expect(await within(tab).findByTestId('agent-search-input')).toBeInTheDocument();
    expect(within(tab).getByTestId('agents-create')).toBeInTheDocument();
    expect(within(tab).getByTestId('agents-import-button')).toBeInTheDocument();
    expect(within(tab).getByTestId('agent-sort-field')).toBeInTheDocument();
    expect(within(tab).getByTestId('agent-table-view-button')).toBeInTheDocument();
  });

  it('opens the sidebar and focuses its search with the / key', async () => {
    const user = userEvent.setup();
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '9' });

    await screen.findByTestId('agent-search-input');
    await user.keyboard('/');

    expect(screen.getByTestId('agent-search-input')).toHaveFocus();
  });

  it('reaches every row the counts claim: "Load more" fetches the next server page', async () => {
    const many: Row[] = Array.from({ length: 90 }, (_, i) => ({ id: String(i + 1), name: `Agent ${String(i + 1).padStart(3, '0')}`, status: 'draft' }));
    const offsets: (string | null)[] = [];
    server.use(
      getListApplicationsMockHandler((info) => {
        const url = new URL(info.request.url);
        const offset = Number(url.searchParams.get('offset') ?? 0);
        const limit = Number(url.searchParams.get('limit') ?? 20);
        offsets.push(url.searchParams.get('offset'));
        return { ...page(many.slice(offset, offset + limit)), total: many.length };
      }),
    );
    const user = userEvent.setup();
    renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9' });

    await waitFor(() => expect(screen.getAllByTestId('entity-card')).toHaveLength(40));
    await user.click(await screen.findByRole('button', { name: 'Load more' }));
    await waitFor(() => expect(screen.getAllByTestId('entity-card')).toHaveLength(80));
    await user.click(await screen.findByRole('button', { name: 'Load more' }));
    await waitFor(() => expect(screen.getAllByTestId('entity-card')).toHaveLength(90));
    expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull();
    expect(offsets).toEqual(expect.arrayContaining(['0', '40', '80']));
  });

  it('applies every status of a link with several, and the facet is a multi-select', async () => {
    const seen: (string | null)[] = [];
    server.use(
      getListApplicationsMockHandler((info) => {
        seen.push(new URL(info.request.url).searchParams.get('status'));
        return page(ROWS);
      }),
    );
    const user = userEvent.setup();
    const statuses = encodeURIComponent(JSON.stringify(['draft', 'published']));
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, `/agents/all?statuses=${statuses}`, { projectId: '9' });

    await waitFor(() => expect(seen).toContain('draft,published'));
    expect(await screen.findByTestId('agent-facet-statuses-draft')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('agent-facet-statuses-published')).toHaveAttribute('aria-pressed', 'true');

    await user.click(screen.getByTestId('agent-facet-statuses-published'));
    await waitFor(() => expect(seen).toContain('draft'));
    expect(screen.getByTestId('agent-facet-statuses-draft')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('agent-facet-statuses-published')).toHaveAttribute('aria-pressed', 'false');
  });

  it('after a partial bulk failure keeps only the failed rows selected, with the error', async () => {
    server.use(
      http.delete('*/elitea_core/application/prompt_lib/:projectId/:id', ({ params }) =>
        params['id'] === '3' ? HttpResponse.json({ error: 'published' }, { status: 409 }) : new HttpResponse(null, { status: 204 }),
      ),
    );
    const user = userEvent.setup();
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '9' });

    const cards = await screen.findAllByTestId('entity-card');
    await user.click(within(cards[0] as HTMLElement).getByRole('checkbox'));
    await user.click(within(cards[2] as HTMLElement).getByRole('checkbox'));
    await user.click(await screen.findByTestId('agent-selection-delete'));
    await user.click(await screen.findByRole('button', { name: 'Delete', hidden: false }));

    expect(await screen.findByTestId('agent-selection-count')).toHaveTextContent('1 agents selected');
    expect(await screen.findByRole('alert')).toHaveTextContent('could not be deleted');
  });

  // Issue 3133: the card's Chat starts a private conversation with the agent's default version and opens it.
  it('starts a chat with the agent from its card and opens the conversation', async () => {
    let participants: unknown;
    let creates = 0;
    server.use(
      getGetCurrentAuthorMockHandler({ id: 5, email: 'me@example.com', name: 'Me' } as never),
      getGetApplicationMockHandler({
        id: '1',
        name: 'Draft agent',
        description: '',
        icon: '',
        owner_id: 'user-1',
        created_at: '2026-01-01T00:00:00Z',
        versions: [{ id: '11', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z', is_default: true }],
      }),
      http.post('*/elitea_core/conversations/prompt_lib/:projectId', () => {
        creates += 1;
        return HttpResponse.json({ id: '77', name: 'Draft agent' }, { status: 201 });
      }),
      http.post('*/elitea_core/participants/prompt_lib/:projectId/:conversationId', async ({ request }) => {
        participants = await request.json();
        return HttpResponse.json([], { status: 200 });
      }),
    );
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9' });

    const cards = await screen.findAllByTestId('entity-card');
    expect(cards[0]).toHaveAttribute('data-variant', 'rich');
    const chat = within(cards[0] as HTMLElement).getByTestId('entity-card-primary-action');
    await waitFor(() => expect(chat).toBeEnabled());
    // A double-click makes one conversation, not two.
    await user.dblClick(chat);

    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/77'));
    expect(creates).toBe(1);
    expect(JSON.stringify(participants)).toContain('"version_id":"11"');
  });

  // Issue 3134: the Teams mode names itself and offers team actions, not agent ones.
  it('offers New team and Import team in the side panel of the Teams mode, not New agent', async () => {
    server.use(
      getPermissionListMockHandler([
        { name: 'models.applications.applications.create', enabled: true },
        { name: 'models.teams.list', enabled: true },
        { name: 'models.teams.create', enabled: true },
      ]),
      http.get('*/elitea_core/agent_teams/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
    );
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/teams', { projectId: '9' });

    expect(await screen.findByTestId('agent-teams-empty')).toHaveTextContent('No teams yet');
    expect(await screen.findByTestId('teams-create')).toHaveTextContent('New team');
    expect(screen.getByTestId('teams-import-button')).toHaveTextContent('Import team');
    expect(screen.getByTestId('teams-header-import-button')).toHaveTextContent('Import team');
    expect(screen.queryByTestId('agents-create')).toBeNull();
    expect(screen.queryByTestId('agents-import-button')).toBeNull();
  });
});
