import userEvent from '@testing-library/user-event';
import { screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { Project } from '@/entities/project';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { PERMISSION_GROUPS, PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';
import { installWebStorageShim } from '@/test/webstorage';

import { useSidebarCollapsedStore } from '../model/sidebarCollapsed.store';
import { Sidebar } from '../ui/Sidebar';
import { renderAtPath } from './testRouter';

installWebStorageShim();

const projects: readonly Project[] = [
  { id: 11, name: 'Public', suspended: false },
  { id: 2, name: 'Alpha Co', suspended: false },
];

const viewPermissions: readonly string[] = Object.values(PERMISSION_GROUPS).flat();
const allPermissions = new Set([...viewPermissions, PERMISSIONS.chat.create, PERMISSIONS.applications.create, PERMISSIONS.toolkits.create]);

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
  useSidebarCollapsedStore.setState({ collapsed: false });
  window.localStorage.clear();
  // The Chats list reads the project's listing.
  server.use(
    http.get('*/social/author', () => HttpResponse.json({ id: 3, name: 'Me' })),
    http.get('*/elitea_core/folder/prompt_lib/:projectId', () => HttpResponse.json({ pinned: { conversations: [] }, folders: [], date_groups: [], total_folders: 0 })),
    http.get('*/elitea_core/conversations/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
  );
});

afterEach(() => {
  vi.unstubAllEnvs();
  resetConfigForTests();
});

function renderSidebar(permissions: ReadonlySet<string> = allPermissions) {
  return renderAtPath(
    '/agents',
    <Sidebar
      permissions={permissions}
      projects={projects}
      selectedProjectId="2"
      onSelectProject={vi.fn()}
    />,
  );
}

describe('Sidebar', () => {
  it('renders the Chats, every place and Credentials when every permission is granted (SHELL-001..009)', async () => {
    await renderSidebar();
    expect(screen.getByTestId('shell-chats-caption')).toHaveTextContent('Chats');
    for (const label of ['Agents', 'Pipelines', 'Skills', 'Toolkits & Indexes', 'MCPs', 'Credentials', 'Applications', 'Artifacts']) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it('hides permission-gated entries when their permission is missing (SHELL-010)', async () => {
    const user = userEvent.setup();
    await renderSidebar(new Set());
    expect(screen.queryByTestId('shell-chats-caption')).not.toBeInTheDocument();
    expect(screen.queryByText('Credentials')).not.toBeInTheDocument();
    expect(screen.queryByText('Agents')).not.toBeInTheDocument();
    // Ungated entries still render; Skills sits in the Workshop.
    expect(screen.getByText('Applications')).toBeInTheDocument();
    await user.click(within(screen.getByTestId('shell-elitea-workshop')).getByRole('button'));
    expect(screen.getByText('Skills')).toBeInTheDocument();
  });

  it('renders the project switcher showing the selected project name', async () => {
    await renderSidebar();
    expect(screen.getByText('Alpha Co')).toBeInTheDocument();
  });

  it('offers a "+" on each place the person may create in, and none without the right', async () => {
    await renderSidebar();
    expect(screen.getByRole('button', { name: 'New agent' })).toBeInTheDocument();
    expect(screen.getByTestId('shell-create-toolkits')).toHaveAccessibleName('New toolkit');
    expect(screen.getByTestId('shell-create-artifacts')).toHaveAccessibleName('New bucket');
    expect(screen.getByTestId('shell-chats-new')).toBeInTheDocument();
  });

  it('leaves the "+" out where the person may not create', async () => {
    await renderSidebar(new Set(viewPermissions));
    expect(screen.queryByTestId('shell-create-agents')).not.toBeInTheDocument();
    expect(screen.queryByTestId('shell-create-toolkits')).not.toBeInTheDocument();
    expect(screen.queryByTestId('shell-chats-new')).not.toBeInTheDocument();
    // Artifacts needs no create permission of its own.
    expect(screen.getByTestId('shell-create-artifacts')).toBeInTheDocument();
  });

  it('toggles collapsed state from the header\'s collapse button and persists it', async () => {
    const user = userEvent.setup();
    await renderSidebar();
    const toggle = screen.getByTestId('sidebar-collapse-toggle');
    // In the header row, not pinned over the rail's edge.
    expect(getComputedStyle(toggle).position).not.toBe('fixed');
    expect(useSidebarCollapsedStore.getState().collapsed).toBe(false);
    await user.click(toggle);
    expect(useSidebarCollapsedStore.getState().collapsed).toBe(true);
    expect(window.localStorage.getItem('el.sidebar.collapsed')).toBe('1');
  });

  it('collapsed: icons only, one "New chat", no chat list', async () => {
    // Set the PERSISTED value, not the store directly — Sidebar hydrates
    // from storage on every mount (`readPersistedCollapsed()`).
    window.localStorage.setItem('el.sidebar.collapsed', '1');
    const { container } = await renderSidebar();
    expect(screen.queryByText('Agents')).not.toBeInTheDocument();
    expect(container.querySelector('a[href="/agents"]')).not.toBeNull();
    expect(screen.getByTestId('sidebar-new-chat')).toHaveAccessibleName('New chat');
    expect(screen.queryByTestId('shell-chats-caption')).not.toBeInTheDocument();
  });

  describe('the chat list reads only while it is on screen', () => {
    let listingReads: number;
    beforeEach(() => {
      configureGeneratedClient({ baseUrl: '/api/v2' });
      listingReads = 0;
      server.use(
        http.get('*/auth/permissions/prompt_lib/:projectId', () => HttpResponse.json([{ name: 'models.chat.folders.get', enabled: true }])),
        http.get('*/elitea_core/folder/prompt_lib/:projectId', () => {
          listingReads += 1;
          return HttpResponse.json({ pinned: { conversations: [] }, folders: [], date_groups: [], total_folders: 0 });
        }),
      );
    });
    afterEach(() => resetGeneratedClient());
    const settle = () => new Promise((resolve) => setTimeout(resolve, 100));

    it('reads while the Chats list is on screen', async () => {
      await renderSidebar();
      await vi.waitFor(() => expect(listingReads).toBeGreaterThan(0));
    });

    it('reads nothing while the rail is collapsed', async () => {
      window.localStorage.setItem('el.sidebar.collapsed', '1');
      await renderSidebar();
      await settle();
      expect(listingReads).toBe(0);
    });

    it('reads nothing while the Chats list is closed, and reads once it opens', async () => {
      const user = userEvent.setup();
      window.localStorage.setItem('el.desktop.chatsOpen', 'false');
      await renderSidebar();
      await settle();
      expect(listingReads).toBe(0);

      await user.click(screen.getByTestId('shell-chats-toggle'));
      await vi.waitFor(() => expect(listingReads).toBeGreaterThan(0));
    });
  });

  it('the Agents nav link points at /agents', async () => {
    await renderSidebar();
    expect(screen.getByRole('link', { name: 'Agents' })).toHaveAttribute('href', '/agents');
  });

  it('R3: hides "Skills" when the selected project is the public project', async () => {
    // `projects[0]` (id 11) matches `VITE_PUBLIC_PROJECT_ID=11` stubbed above.
    await renderAtPath(
      '/agents',
      <Sidebar
        permissions={allPermissions}
        projects={projects}
        selectedProjectId="11"
        onSelectProject={vi.fn()}
      />,
    );
    expect(screen.queryByText('Skills')).not.toBeInTheDocument();
    // Unrelated siblings stay visible.
    expect(screen.getByText('Toolkits & Indexes')).toBeInTheDocument();
  });

  it('R3: shows "Skills" when the selected project is not the public project', async () => {
    await renderSidebar();
    expect(screen.getByText('Skills')).toBeInTheDocument();
  });
});
