/**
 * The conversations under the sidebar's Chats row: Pinned first, then the
 * date buckets with their disclosure captions (Pinned and Today open, older
 * groups closed with a count, each remembered; the group that holds the open
 * conversation opens for it), the open conversation highlighted (also after
 * the list is read again), Local work threads left out, Archive asks first
 * and takes the row away (and keeps it away when the list mounts again), and
 * the trailing "Archived" row opens the archived chats page.
 */
import { useState } from 'react';

import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider, useLocation } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { clearNamespace } from '@/shared/lib/storage';
import { server } from '@/test/setup';

import { ChatsSection } from '../ui/ChatsSection';

const globals = globalThis as unknown as Record<string, unknown>;
const at = new Date().toISOString();
const row = (id: string, name: string, extra: Record<string, unknown> = {}) => ({ id: Number(id), name, author_id: 3, updated_at: at, created_at: at, ...extra });

const LISTING = {
  pinned: { conversations: [row('81', 'Release checklist')] },
  folders: [],
  date_groups: [
    { name: 'Today', total: 2, offset: 2, conversations: [row('72', 'quick overview'), row('73', 'local thread', { source: 'local_work' })] },
    { name: 'Yesterday', total: 1, offset: 1, conversations: [row('75', 'Postgres connection math')] },
    { name: 'This week', total: 1, offset: 1, conversations: [row('77', 'Helm values')] },
  ],
  total_folders: 0,
};

function Section({ canArchive = true }: { readonly canArchive?: boolean }): React.JSX.Element {
  const location = useLocation();
  // A remount of the list over the same query cache (the sidebar collapsed and opened again).
  const [shown, setShown] = useState(true);
  return (
    <>
      <button type="button" onClick={() => setShown((current) => !current)}>
        toggle list
      </button>
      {shown && <ChatsSection projectId="42" pathname={location.pathname} canArchive={canArchive} />}
      <p data-testid="where">{location.pathname}</p>
      <Outlet />
    </>
  );
}

function mount(path = '/chat/72', canArchive = true) {
  const rootRoute = createRootRoute({ component: () => <Section canArchive={canArchive} /> });
  const page = () => null;
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/chat', component: page }),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat/archived', component: page }),
      createRoute({ getParentRoute: () => rootRoute, path: '/chat/$conversationId', component: page }),
    ]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return router;
}

let archived: string[];

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  clearNamespace();
  archived = [];
  listingReads = 0;
  server.use(
    http.get('*/auth/permissions/prompt_lib/:projectId', () => HttpResponse.json([{ name: 'models.chat.folders.get', enabled: true }])),
    http.get('*/social/author', () => HttpResponse.json({ id: 3, name: 'Me' })),
    // The server leaves an archived conversation out of the listing.
    http.get('*/elitea_core/folder/prompt_lib/:projectId', () => {
      listingReads += 1;
      return HttpResponse.json({
        ...LISTING,
        date_groups: LISTING.date_groups.map((group) => ({ ...group, conversations: group.conversations.filter((item) => !archived.includes(String(item.id))) })),
      });
    }),
    http.put('*/elitea_core/conversation_archive/prompt_lib/:projectId/:conversationId', ({ params }) => {
      archived.push(String(params['conversationId']));
      return HttpResponse.json({ ok: true });
    }),
    http.get('*/elitea_core/conversations/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

let listingReads: number;

const labels = (): string[] =>
  screen.getAllByTestId(/^shell-(chats-group|conversation)$/).map((node) => node.querySelector('.shell-group-caption')?.textContent ?? node.textContent ?? '');
// The group's disclosure button (the row's test id is on its frame, the button inside it).
const caption = (name: string): HTMLElement =>
  within(screen.getAllByTestId('shell-chats-group').find((node) => node.querySelector('.shell-group-caption')?.textContent === name) as HTMLElement).getByRole('button');

describe('ChatsSection', () => {
  it('lists Pinned first, then the date buckets with native captions, and leaves Local work threads out', async () => {
    mount();
    await screen.findByText('quick overview');
    // Pinned and Today are open; Yesterday and Previous 7 days are closed and show their counts.
    expect(labels()).toEqual(['Pinned', 'Release checklist', 'Today', 'quick overview', 'Yesterday', 'Previous 7 days']);
    expect(caption('Yesterday')).toHaveAttribute('aria-expanded', 'false');
    expect(within(caption('Yesterday')).getByTestId('shell-chats-group-count')).toHaveTextContent('1');
    expect(caption('Today')).toHaveAttribute('aria-expanded', 'true');
    expect(screen.queryByText('local thread')).not.toBeInTheDocument();
  });

  it('opens and closes a group from its caption, and remembers it when the list mounts again', async () => {
    const user = userEvent.setup();
    mount('/chat');
    await screen.findByText('quick overview');
    await user.click(caption('Yesterday'));
    expect(await screen.findByText('Postgres connection math')).toBeInTheDocument();
    await user.click(caption('Today'));
    expect(screen.queryByText('quick overview')).not.toBeInTheDocument();
    expect(JSON.parse(window.localStorage.getItem('el.desktop.chatGroupsOpen') ?? '{}')).toEqual({ yesterday: true, today: false });

    await user.click(screen.getByRole('button', { name: 'toggle list' }));
    await user.click(screen.getByRole('button', { name: 'toggle list' }));
    expect(await screen.findByText('Postgres connection math')).toBeInTheDocument();
    expect(screen.queryByText('quick overview')).not.toBeInTheDocument();
  });

  it('opens a closed group that holds the open conversation, keeps it selected after the list is read again, and lets the person close the group', async () => {
    const user = userEvent.setup();
    mount('/chat/77');
    const open = await screen.findByText('Helm values');
    expect(open.closest('button')).toHaveAttribute('aria-current', 'page');
    expect(caption('Previous 7 days')).toHaveAttribute('aria-expanded', 'true');
    // Opened for the conversation, not remembered.
    expect(window.localStorage.getItem('el.desktop.chatGroupsOpen')).toBeNull();

    // An archive of another row reads the listing again: the open row is still the selected one.
    const reads = listingReads;
    const other = screen.getAllByTestId('shell-conversation').find((node) => node.textContent?.includes('quick overview'));
    await user.click(within(other as HTMLElement).getByTestId('shell-conversation-more'));
    await user.click(await screen.findByRole('menuitem', { name: 'Archive' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Archive' }));
    await waitFor(() => expect(listingReads).toBeGreaterThan(reads));
    await waitFor(() => expect(screen.queryByText('quick overview')).not.toBeInTheDocument());
    expect((await screen.findByText('Helm values')).closest('button')).toHaveAttribute('aria-current', 'page');

    await user.click(caption('Previous 7 days'));
    expect(screen.queryByText('Helm values')).not.toBeInTheDocument();
  });

  it('highlights the open conversation and opens another on click', async () => {
    const user = userEvent.setup();
    const router = mount('/chat/72');
    const open = await screen.findByText('quick overview');
    expect(open.closest('button')).toHaveAttribute('aria-current', 'page');
    await user.click(caption('Previous 7 days'));
    expect(screen.getByText('Helm values').closest('button')).not.toHaveAttribute('aria-current');
    await user.click(screen.getByText('Helm values'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/77'));
  });

  it('archives from the ⋯ menu after a confirmation, and the row leaves the list', async () => {
    const user = userEvent.setup();
    mount();
    await screen.findByText('quick overview');
    await user.click(caption('Yesterday'));
    await screen.findByText('Postgres connection math');
    const target = screen.getAllByTestId('shell-conversation').find((node) => node.textContent?.includes('Postgres'));
    await user.click(within(target as HTMLElement).getByTestId('shell-conversation-more'));
    await user.click(await screen.findByRole('menuitem', { name: 'Archive' }));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('everyone in the project');
    await user.click(within(dialog).getByRole('button', { name: 'Archive' }));
    await waitFor(() => expect(screen.queryByText('Postgres connection math')).not.toBeInTheDocument());
    expect(archived).toEqual(['75']);
  });

  it('disables Archive without the archive right (the server would answer 403)', async () => {
    const user = userEvent.setup();
    mount('/chat/72', false);
    await screen.findByText('quick overview');
    await user.click(caption('Yesterday'));
    await screen.findByText('Postgres connection math');
    const target = screen.getAllByTestId('shell-conversation').find((node) => node.textContent?.includes('Postgres'));
    await user.click(within(target as HTMLElement).getByTestId('shell-conversation-more'));
    expect(await screen.findByRole('menuitem', { name: 'Archive' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('keeps an archived row out of the list when the list mounts again over the same cache', async () => {
    const user = userEvent.setup();
    mount();
    await screen.findByText('quick overview');
    await user.click(caption('Yesterday'));
    await screen.findByText('Postgres connection math');
    const target = screen.getAllByTestId('shell-conversation').find((node) => node.textContent?.includes('Postgres'));
    await user.click(within(target as HTMLElement).getByTestId('shell-conversation-more'));
    await user.click(await screen.findByRole('menuitem', { name: 'Archive' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Archive' }));
    await waitFor(() => expect(screen.queryByText('Postgres connection math')).not.toBeInTheDocument());
    expect(archived).toEqual(['75']);

    await user.click(screen.getByRole('button', { name: 'toggle list' }));
    expect(screen.queryByTestId('shell-chats')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'toggle list' }));
    await screen.findByText('quick overview');
    expect(screen.queryByText('Postgres connection math')).not.toBeInTheDocument();
  });

  it('opens the archived chats page from the trailing Archived row, and marks it there', async () => {
    const user = userEvent.setup();
    const router = mount('/chat');
    await user.click(within(await screen.findByTestId('shell-chats-archived')).getByRole('button'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/archived'));
    expect(within(screen.getByTestId('shell-chats-archived')).getByRole('button')).toHaveAttribute('aria-current', 'page');
    await screen.findByText('quick overview');
    expect(screen.getByText('quick overview').closest('button')).not.toHaveAttribute('aria-current');
  });
});
