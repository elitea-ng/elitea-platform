import userEvent from '@testing-library/user-event';
import { screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { Project } from '@/entities/project';
import { resetConfigForTests } from '@/shared/config/get-config';
import { PERMISSION_GROUPS } from '@/shared/lib/permissions';
import { installWebStorageShim } from '@/test/webstorage';

import { useSidebarCollapsedStore } from '../model/sidebarCollapsed.store';
import { Sidebar } from '../ui/Sidebar';
import { renderAtPath } from './testRouter';

installWebStorageShim();

const projects: readonly Project[] = [
  { id: 11, name: 'Public', suspended: false },
  { id: 2, name: 'Alpha Co', suspended: false },
];

const allPermissions = new Set(Object.values(PERMISSION_GROUPS).flat());

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
  useSidebarCollapsedStore.setState({ collapsed: false });
  window.localStorage.clear();
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
  it('renders all 9 nav entries when every permission is granted (SHELL-001..009)', async () => {
    await renderSidebar();
    for (const label of ['Chats', 'Agents', 'Pipelines', 'Skills', 'Toolkits & Indexes', 'MCPs', 'Credentials', 'Applications', 'Artifacts']) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it('hides permission-gated entries when their permission is missing (SHELL-010)', async () => {
    await renderSidebar(new Set());
    expect(screen.queryByText('Chats')).not.toBeInTheDocument();
    expect(screen.queryByText('Credentials')).not.toBeInTheDocument();
    // Ungated entries still render.
    expect(screen.getByText('Skills')).toBeInTheDocument();
    expect(screen.getByText('Applications')).toBeInTheDocument();
  });

  it('renders the project switcher showing the selected project name', async () => {
    await renderSidebar();
    expect(screen.getByText('Alpha Co')).toBeInTheDocument();
  });

  it('renders the global create button', async () => {
    await renderSidebar();
    expect(screen.getByTestId('sidebar-create-button')).toBeInTheDocument();
  });

  it('toggles collapsed state on the collapse button and persists it', async () => {
    const user = userEvent.setup();
    await renderSidebar();
    expect(useSidebarCollapsedStore.getState().collapsed).toBe(false);
    await user.click(screen.getByTestId('sidebar-collapse-toggle'));
    expect(useSidebarCollapsedStore.getState().collapsed).toBe(true);
    expect(window.localStorage.getItem('el.sidebar.collapsed')).toBe('1');
  });

  it('collapsing hides nav labels but keeps the underlying links', async () => {
    // Set the PERSISTED value, not the store directly — Sidebar hydrates
    // from storage on every mount (`readPersistedCollapsed()`), which would
    // otherwise immediately overwrite a direct `setState` call.
    window.localStorage.setItem('el.sidebar.collapsed', '1');
    const { container } = await renderSidebar();
    expect(screen.queryByText('Chats')).not.toBeInTheDocument();
    expect(container.querySelector('a[href="/agents"]')).not.toBeNull();
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
