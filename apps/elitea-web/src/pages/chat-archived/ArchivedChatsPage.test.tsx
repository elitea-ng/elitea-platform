/**
 * `/chat/archived`: the project's archived chats (`archived=only`), each with
 * who archived it and when; Unarchive sends it back to the chat lists (and
 * takes it off this page), a failed Unarchive puts it back here.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import ArchivedChatsPage from './ArchivedChatsPage';

const BASE = '/api/v2';
const globals = globalThis as unknown as Record<string, unknown>;
const hoursAgo = (hours: number): string => new Date(Date.now() - hours * 3_600_000).toISOString();
const row = (id: number, name: string, extra: Record<string, unknown>) => ({ id, name, created_at: hoursAgo(900), updated_at: hoursAgo(800), duration: -1, message_groups_count: 1, is_pinned: false, is_archived: true, ...extra });

let unarchived: string[];
let archivedParam: string | null;
let unarchiveStatus: number;

function mount(): void {
  const rootRoute = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/chat/archived', component: ArchivedChatsPage }),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat', component: () => null }),
    ]),
    history: createMemoryHistory({ initialEntries: ['/chat/archived'] }),
  });
  render(
    <AppProviders>
      <RouterProvider router={router as never} />
    </AppProviders>,
  );
}

beforeEach(() => {
  unarchived = [];
  archivedParam = null;
  unarchiveStatus = 200;
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
  useSelectedProjectStore.setState({ project: { id: '42', name: 'Platform' } });
  server.use(
    http.get(`${BASE}/elitea_core/conversations/prompt_lib/42`, ({ request }) => {
      archivedParam = new URL(request.url).searchParams.get('archived');
      const rows = [
        row(61, 'Old sprint retro', { archived_by: { id: 4, name: 'Ana Ruiz' }, archived_at: hoursAgo(5) }),
        row(62, 'Spike: socket.io removal', { archived_by: { id: 3, name: '' }, archived_at: hoursAgo(50) }),
      ].filter((candidate) => !unarchived.includes(String(candidate.id)));
      return HttpResponse.json({ rows, total: rows.length });
    }),
    http.delete(`${BASE}/elitea_core/conversation_archive/prompt_lib/42/:conversationId`, ({ params }) => {
      if (unarchiveStatus !== 200) return HttpResponse.json({ error: 'nope' }, { status: unarchiveStatus });
      unarchived.push(String(params['conversationId']));
      return HttpResponse.json({ ok: true });
    }),
    http.get(`${BASE}/auth/permissions/prompt_lib/42`, () => HttpResponse.json([])),
  );
});

afterEach(() => {
  useSelectedProjectStore.setState({ project: null });
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

const rowOf = (name: string): HTMLElement => screen.getAllByTestId('archived-conversation').find((node) => node.textContent?.includes(name)) as HTMLElement;

describe('ArchivedChatsPage', () => {
  it("lists the project's archived chats with who archived them and when", async () => {
    mount();
    expect(await screen.findByText('Old sprint retro')).toBeInTheDocument();
    expect(archivedParam).toBe('only');
    expect(rowOf('Old sprint retro')).toHaveTextContent('Archived by Ana Ruiz · about 5 hours ago');
    // A name the server could not resolve says only when.
    expect(rowOf('Spike')).toHaveTextContent('Archived · 2 days ago');
  });

  it('unarchives a chat: the request goes out and the row leaves the page', async () => {
    const user = userEvent.setup();
    mount();
    await screen.findByText('Old sprint retro');
    await user.click(within(rowOf('Old sprint retro')).getByTestId('archived-conversation-unarchive'));
    await waitFor(() => expect(screen.queryByText('Old sprint retro')).not.toBeInTheDocument());
    expect(unarchived).toEqual(['61']);
    expect(screen.getByText('Spike: socket.io removal')).toBeInTheDocument();
  });

  it('puts the row back when the unarchive fails', async () => {
    unarchiveStatus = 500;
    const user = userEvent.setup();
    mount();
    await screen.findByText('Old sprint retro');
    await user.click(within(rowOf('Old sprint retro')).getByTestId('archived-conversation-unarchive'));
    expect(await screen.findByText('Failed to unarchive the conversation')).toBeInTheDocument();
    expect(screen.getByText('Old sprint retro')).toBeInTheDocument();
    expect(unarchived).toEqual([]);
  });
});
