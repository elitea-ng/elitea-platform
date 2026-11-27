/**
 * The cloud-thread banner offers the pull-request credential picker only when
 * the folder has an upstream: a folder that is not a repository travels in a
 * private shadow copy, which can never push or open a pull request.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { HandoffStatus } from '@/shared/desktop/handoffIpc';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { HandoffBanner } from './HandoffControls';
import type { ThreadHandoff } from './useThreadHandoff';

const BASE = '/api/v2';
const UUID = '0b1d6f4e-3c52-4d3a-9a54-5f1c9a0e7c11';

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

function handoff(status: Partial<HandoffStatus>): ThreadHandoff {
  return {
    ipc: undefined,
    uuid: UUID,
    status: { state: 'CLOUD_IDLE', epoch: 1, executor: 'cloud', read_only: true, sent_from_here: true, ...status },
    offLaptop: true,
    canSend: false,
    dialog: null,
    open: vi.fn(),
    close: vi.fn(),
    moved: vi.fn(),
  };
}

function mount(value: ThreadHandoff): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <HandoffBanner handoff={value} projectId={42} />
    </QueryClientProvider>,
  );
}

describe('HandoffBanner', () => {
  it('hides the pull-request picker for a folder with no upstream, and says why in one line', async () => {
    mount(handoff({ has_upstream: false }));
    expect(await screen.findByTestId('handoff-no-pull-requests')).toHaveTextContent('This folder has no remote repository, so the cloud cannot push or open pull requests');
    expect(screen.queryByTestId('cloud-pr-credential-button')).toBeNull();
  });

  it('offers the picker when the folder has an upstream', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/workspace_handoff/prompt_lib/42/${UUID}`, () => HttpResponse.json({ state: 'CLOUD_IDLE', epoch: 3, pull_request_credential: null })),
      http.get(`${BASE}/configurations/configurations/42`, () => HttpResponse.json({ items: [], total: 0, offset: 0, limit: 200 })),
      http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 1, name: 'Alice', personal_project_id: '42' })),
    );
    mount(handoff({ has_upstream: true }));
    expect(await screen.findByTestId('cloud-pr-credential-button')).toBeInTheDocument();
    expect(screen.queryByTestId('handoff-no-pull-requests')).toBeNull();
  });
});
