/**
 * The "take back requested" toast: a notification of kind
 * `workspace_reclaim_requested` for a thread this computer holds shows a
 * toast whose Take back action opens the thread and asks its page to start
 * the take back. Other notifications and other computers' threads show
 * nothing.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { recordThread, WorkspaceIpcProvider } from '@/features/workspace';
import { rememberThreadUuid, useTakeBackRequests } from '@/features/workspace-handoff';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { clearNamespace } from '@/shared/lib/storage';
import { ThemeProvider } from '@mui/material/styles';
import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { server } from '../../../test/setup';

import { ReclaimRequestToast } from './ReclaimRequestToast';

const BASE = '/api/v2';
const UUID = '0b1d6f4e-3c52-4d3a-9a54-5f1c9a0e7c11';
const APP: Workspace = { id: 'w1', path: '/Users/me/code/app', name: 'app', project_id: 42, is_git: true };
const globals = globalThis as unknown as Record<string, unknown>;

interface Served {
  read: string[];
}

function serve(rows: unknown[]): Served {
  const served: Served = { read: [] };
  server.use(
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me', personal_project_id: 7 })),
    http.get(`${BASE}/notifications/notifications/prompt_lib/7`, () => HttpResponse.json({ rows, total: rows.length })),
    http.put(`${BASE}/notifications/notification/prompt_lib/7/:id`, ({ params }) => {
      served.read.push(String(params['id']));
      return HttpResponse.json({});
    }),
  );
  return served;
}

const reclaim = (id: number, conversation: string, extra: Record<string, unknown> = {}) => ({
  id,
  event_type: 'workspace_reclaim_requested',
  created_at: '2026-10-15T10:00:00Z',
  is_seen: false,
  meta: { conversation_id: conversation },
  ...extra,
});

function mount(run = vi.fn()): { run: ReturnType<typeof vi.fn> } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <ThemeProvider theme={buildEliteaTheme(DEFAULT_BRAND_PACK)}>
      <QueryClientProvider client={client}>
        <WorkspaceIpcProvider ipc={createFakeWorkspaceIpc({ workspaces: [APP] })}>
          <ReclaimRequestToast run={run} />
        </WorkspaceIpcProvider>
      </QueryClientProvider>
    </ThemeProvider>,
  );
  return { run };
}

beforeEach(() => {
  clearNamespace();
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
  recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
  rememberThreadUuid('w1', '77', UUID);
  for (const pending of ['77', UUID, 'another-thread']) useTakeBackRequests.getState().clear(pending);
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('ReclaimRequestToast', () => {
  it('shows the request and, on Take back, opens the thread and asks its page to take it back', async () => {
    const served = serve([reclaim(5, '77')]);
    const user = userEvent.setup();
    const { run } = mount();

    expect(await screen.findByTestId('reclaim-request-toast')).toHaveTextContent('You asked to take a thread back from the cloud');
    await user.click(screen.getByRole('button', { name: 'Take back' }));

    expect(run).toHaveBeenCalledWith({ type: 'open_workspace', workspaceId: 'w1', conversationId: '77' });
    expect(useTakeBackRequests.getState().requested).toBe('77');
    // The notification is read, so it does not come back.
    await waitFor(() => expect(served.read).toEqual(['5']));
    await waitFor(() => expect(screen.queryByTestId('reclaim-request-toast')).toBeNull());
  });

  it('finds the thread by the UUID the notification carries', async () => {
    serve([reclaim(6, UUID.toUpperCase())]);
    const user = userEvent.setup();
    const { run } = mount();
    await user.click(await screen.findByRole('button', { name: 'Take back' }));
    expect(run).toHaveBeenCalledWith({ type: 'open_workspace', workspaceId: 'w1', conversationId: '77' });
    expect(useTakeBackRequests.getState().requested).toBe(UUID);
  });

  it('is dismissed without taking anything back', async () => {
    const served = serve([reclaim(5, '77')]);
    const user = userEvent.setup();
    const { run } = mount();
    await screen.findByTestId('reclaim-request-toast');
    await user.click(screen.getByRole('button', { name: 'Dismiss' }));
    await waitFor(() => expect(screen.queryByTestId('reclaim-request-toast')).toBeNull());
    expect(run).not.toHaveBeenCalled();
    expect(useTakeBackRequests.getState().requested).toBeNull();
    await waitFor(() => expect(served.read).toEqual(['5']));
  });

  it('shows nothing for a thread this computer does not hold', async () => {
    serve([reclaim(5, 'some-other-conversation')]);
    mount();
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(screen.queryByTestId('reclaim-request-toast')).toBeNull();
  });

  it('shows nothing for other kinds of notification', async () => {
    serve([{ id: 9, event_type: 'rates', created_at: '2026-10-15T10:00:00Z', is_seen: false, meta: { conversation_id: '77' } }]);
    mount();
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(screen.queryByTestId('reclaim-request-toast')).toBeNull();
  });
});
