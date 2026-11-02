/**
 * The desktop's one notifications feed: the rows unread at start are only
 * counted (badge, bell), a row that arrives later shows one native notice —
 * unless Settings › Notifications turned them off — and the bell's state is
 * what the list says.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { NOTIFICATIONS_QUERY_ROOT } from '@/features/notifications';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { createFakePlatformNotice } from '@/shared/desktop/platformNotice.fake';
import { clearNamespace, createStorage } from '@/shared/lib/storage';
import { server } from '@/test/setup';

import { useDesktopBell, useDesktopNotifications } from './useDesktopNotifications';

const BASE = '/api/v2';
const globals = globalThis as unknown as Record<string, unknown>;

const row = (id: number, message: string) => ({ id, event_type: 'agent_run_finished', created_at: '2026-11-01T10:00:00Z', is_seen: false, meta: { message } });

let rows: ReturnType<typeof row>[];

beforeEach(() => {
  clearNamespace();
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
  installTestEventSource();
  rows = [row(1, 'Earlier run finished')];
  server.use(
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me', personal_project_id: 7 })),
    http.get(`${BASE}/notifications/notifications/prompt_lib/7`, () => HttpResponse.json({ rows, total: rows.length })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const notices = createFakePlatformNotice();
  const wrapper = ({ children }: { readonly children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  const hook = renderHook(
    () => {
      const feed = useDesktopNotifications(notices);
      return { feed, bell: useDesktopBell() };
    },
    { wrapper },
  );
  const arrive = async (next: ReturnType<typeof row>) => {
    rows = [next, ...rows];
    await act(() => client.invalidateQueries({ queryKey: NOTIFICATIONS_QUERY_ROOT }));
  };
  return { hook, notices, arrive };
}

describe('useDesktopNotifications', () => {
  it('counts the rows unread at start without announcing them, and lights the bell', async () => {
    const { hook, notices } = mount();
    await waitFor(() => expect(hook.result.current.feed.rows).toHaveLength(1));
    expect(notices.shown).toEqual([]);
    await waitFor(() => expect(notices.unread).toBe(1));
    expect(hook.result.current.bell).toEqual({ projectId: '7', hasUnread: true });
  });

  it('announces a notification that arrives later, its kind as the title', async () => {
    const { hook, notices, arrive } = mount();
    await waitFor(() => expect(hook.result.current.feed.rows).toHaveLength(1));
    await arrive(row(2, 'Nightly report is ready'));
    await waitFor(() => expect(notices.shown).toEqual([{ title: 'Agent run finished', body: 'Nightly report is ready' }]));
    expect(notices.unread).toBe(2);
  });

  it('stays quiet when system notifications are turned off', async () => {
    // Settings › Notifications › Show system notifications, off.
    createStorage('local').setJSON('desktop.systemNotifications', false);
    const { hook, notices, arrive } = mount();
    await waitFor(() => expect(hook.result.current.feed.rows).toHaveLength(1));
    await arrive(row(2, 'Nightly report is ready'));
    await waitFor(() => expect(hook.result.current.feed.rows).toHaveLength(2));
    expect(notices.shown).toEqual([]);
    // The count still reaches the Dock badge.
    expect(notices.unread).toBe(2);
  });
});
