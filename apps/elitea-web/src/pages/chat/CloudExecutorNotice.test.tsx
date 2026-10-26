/**
 * The web's side of a handed-off thread: the Cloud executor badge, Take back
 * (owner only, a request to the desktop), and Erase cloud copy (owner, or a
 * project settings editor). Real hooks over MSW.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getGetWorkspaceHandoffChangesMockHandler, getGetWorkspaceHandoffChangesResponseMock } from '@/shared/api/generated/chat/chat.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../test/setup';

import CloudExecutorNotice, { type CloudExecutorNoticeProps } from './CloudExecutorNotice';

const BASE = '/api/v2';
const UUID = '0b1d6f4e-3c52-4d3a-9a54-5f1c9a0e7c11';
const HANDOFF = `${BASE}/elitea_core/workspace_handoff/prompt_lib/42/${UUID}`;
const REQUEST = `${BASE}/elitea_core/workspace_handoff_reclaim_request/prompt_lib/42/${UUID}`;

interface Served {
  requests: string[];
  erased: number;
}

function serve(options: { permissions?: string[]; info?: Record<string, unknown> } = {}): Served {
  const served: Served = { requests: [], erased: 0 };
  server.use(
    // The Files changed count (the contract's example).
    getGetWorkspaceHandoffChangesMockHandler(),
    http.get(`${BASE}/auth/permissions/prompt_lib/42`, () => HttpResponse.json((options.permissions ?? []).map((name) => ({ name, enabled: true })))),
    http.get(HANDOFF, () => HttpResponse.json(options.info ?? { state: 'CLOUD_IDLE', epoch: 2 })),
    http.delete(HANDOFF, () => {
      served.erased += 1;
      return HttpResponse.json({ state: 'LOCAL', epoch: 3 });
    }),
    http.post(REQUEST, () => {
      served.requests.push('reclaim');
      return HttpResponse.json({}, { status: 202 });
    }),
  );
  return served;
}

function mount(props: Partial<CloudExecutorNoticeProps> = {}): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <CloudExecutorNotice projectId="42" conversationUuid={UUID} executor="cloud" isOwner {...props} />
    </QueryClientProvider>,
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('CloudExecutorNotice', () => {
  it('offers Files changed with what the cloud turns changed since the handoff', async () => {
    serve();
    mount();
    // The example records three files since the handoff.
    expect(await screen.findByTestId('cloud-files-changed-button')).toHaveTextContent('Files changed (3)');
  });

  it('shows the Cloud executor badge', () => {
    serve();
    mount();
    expect(screen.getByTestId('cloud-executor-badge')).toHaveTextContent('Cloud executor');
    expect(screen.getByTestId('cloud-executor-notice')).toHaveTextContent('works on a copy of your desktop folder in the cloud');
  });

  it('asks the desktop to take the thread back, and says it did', async () => {
    const served = serve();
    const user = userEvent.setup();
    mount();
    await user.click(screen.getByRole('button', { name: 'Take back' }));
    expect(await screen.findByText('Sent to your desktop')).toBeInTheDocument();
    expect(served.requests).toEqual(['reclaim']);
    // Only a request: the thread is not changed here, and Erase was not touched.
    expect(served.erased).toBe(0);
  });

  it('says so when the request could not be sent', async () => {
    serve();
    server.use(http.post(REQUEST, () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })));
    const user = userEvent.setup();
    mount();
    await user.click(screen.getByRole('button', { name: 'Take back' }));
    expect(await screen.findByText('The request could not be sent. Try again.')).toBeInTheDocument();
  });

  it('shows a take back already asked for', async () => {
    serve({ info: { state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: true } });
    mount();
    expect(await screen.findByTestId('cloud-reclaim-pending')).toBeInTheDocument();
  });

  describe('who can do what', () => {
    it('lets the owner take back and erase', () => {
      serve();
      mount({ isOwner: true });
      expect(screen.getByRole('button', { name: 'Take back' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Erase cloud copy' })).toBeInTheDocument();
    });

    it('lets a project settings editor erase, but not take back', async () => {
      serve({ permissions: [PERMISSIONS.projectSettings.edit] });
      mount({ isOwner: false });
      expect(await screen.findByRole('button', { name: 'Erase cloud copy' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Take back' })).toBeNull();
    });

    it('lets anyone else do neither', async () => {
      serve({ permissions: ['models.project_context.edit'] });
      mount({ isOwner: false });
      // Give the permission read time to land: the button must stay away.
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(screen.queryByRole('button', { name: 'Erase cloud copy' })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Take back' })).toBeNull();
    });

    it('does not count a disabled permission', async () => {
      serve();
      server.use(http.get(`${BASE}/auth/permissions/prompt_lib/42`, () => HttpResponse.json([{ name: PERMISSIONS.projectSettings.edit, enabled: false }])));
      mount({ isOwner: false });
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(screen.queryByRole('button', { name: 'Erase cloud copy' })).toBeNull();
    });
  });

  describe('erase', () => {
    it('asks first and erases only on confirmation', async () => {
      const served = serve();
      const user = userEvent.setup();
      mount();
      await user.click(screen.getByRole('button', { name: 'Erase cloud copy' }));
      const dialog = await screen.findByRole('dialog', { name: 'Erase the cloud copy?' });
      expect(served.erased).toBe(0);
      await user.click(within(dialog).getByRole('button', { name: 'Erase cloud copy' }));
      expect(await screen.findByText('The cloud copy was erased')).toBeInTheDocument();
      expect(served.erased).toBe(1);
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    });

    it('keeps the cloud copy when backing out', async () => {
      const served = serve();
      const user = userEvent.setup();
      mount();
      await user.click(screen.getByRole('button', { name: 'Erase cloud copy' }));
      await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Keep it' }));
      expect(served.erased).toBe(0);
    });

    it('says when the erase failed', async () => {
      serve();
      server.use(http.delete(HANDOFF, () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })));
      const user = userEvent.setup();
      mount();
      await user.click(screen.getByRole('button', { name: 'Erase cloud copy' }));
      await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Erase cloud copy' }));
      expect(await screen.findByText('The cloud copy could not be erased. Try again.')).toBeInTheDocument();
    });
  });

  it('while the thread returns to the desktop, shows Returning and offers no Take back', () => {
    serve({ info: { state: 'RECLAIMING', epoch: 3 } });
    mount({ executor: 'returning' });
    expect(screen.getByTestId('cloud-executor-badge')).toHaveTextContent('Returning to desktop');
    expect(screen.queryByRole('button', { name: 'Take back' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Erase cloud copy' })).toBeInTheDocument();
  });
});

describe('CloudExecutorNotice live updates (ADR-0034 scope B)', () => {
  const globals = globalThis as unknown as Record<string, unknown>;
  let registry: TestEventSourceRegistry;
  beforeEach(() => {
    registry = installTestEventSource();
    globals['elitea_ui_config'] = { vite_server_url: BASE, vite_base_uri: '/', vite_public_project_id: 'public-1' };
    resetConfigForTests();
  });
  afterEach(() => {
    registry.restore();
    delete globals['elitea_ui_config'];
    resetConfigForTests();
  });

  it('shows a take back as soon as the project stream says so, without waiting for a poll', async () => {
    let reads = 0;
    let pending = false;
    serve();
    server.use(
      http.get(HANDOFF, () => {
        reads += 1;
        return HttpResponse.json({ state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: pending });
      }),
    );
    mount();
    await waitFor(() => {
      expect(registry.getOpen().some((source) => source.url === `${BASE}/events/prompt_lib/42`)).toBe(true);
    });
    await waitFor(() => {
      expect(reads).toBeGreaterThan(0);
    });
    expect(screen.queryByText(/Take back requested/)).not.toBeInTheDocument();
    // The desktop takes the thread back; the next poll is ten seconds away.
    pending = true;
    const before = reads;
    registry.emit('workspace.handoff', JSON.stringify({ conversation_uuid: UUID, state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: true }));
    expect(await screen.findByText(/Take back requested/)).toBeInTheDocument();
    // The frame drove it: at most the one re-read it asked for, no poll.
    expect(reads - before).toBeLessThanOrEqual(1);
  });

  it('shows Files changed once the first cloud turn ends, on the frame of the turn', async () => {
    const example = getGetWorkspaceHandoffChangesResponseMock();
    let recorded = false;
    serve({ info: { state: 'CLOUD_RUNNING', epoch: 2 } });
    server.use(
      getGetWorkspaceHandoffChangesMockHandler(() =>
        recorded ? example : { ...example, head_sha: null, cumulative: null, cumulative_commit: null, turns: [] },
      ),
    );
    mount();
    await waitFor(() => {
      expect(registry.getOpen().some((source) => source.url === `${BASE}/events/prompt_lib/42`)).toBe(true);
    });
    expect(await screen.findByTestId('cloud-executor-badge')).toBeInTheDocument();
    expect(screen.queryByTestId('cloud-files-changed-button')).toBeNull();
    // The turn ends: its record is in, and the thread's frame says the turn is over.
    recorded = true;
    registry.emit('workspace.handoff', JSON.stringify({ conversation_uuid: UUID, state: 'CLOUD_IDLE', epoch: 2, reclaim_pending: false }));
    expect(await screen.findByTestId('cloud-files-changed-button')).toHaveTextContent('Files changed (3)');
  });

  it('keeps a mixed-case UUID on the wire, lowercases it in the read, so the frame still reaches the notice', async () => {
    const mixed = `${UUID.slice(0, 8)}${UUID.slice(8).toUpperCase()}`;
    serve();
    server.use(http.get(`${BASE}/elitea_core/workspace_handoff/prompt_lib/42/${mixed}`, () => HttpResponse.json({ state: 'CLOUD_RUNNING', epoch: 2 })));
    mount({ conversationUuid: mixed });
    await waitFor(() => {
      expect(registry.getOpen().some((source) => source.url === `${BASE}/events/prompt_lib/42`)).toBe(true);
    });
    // The frame is newer than the server's answer (epoch 3 over 2): it must
    // reach the notice through the lowercased key and stay after the re-read.
    registry.emit('workspace.handoff', JSON.stringify({ conversation_uuid: UUID, state: 'CLOUD_RUNNING', epoch: 3, reclaim_pending: true }));
    expect(await screen.findByText(/Take back requested/)).toBeInTheDocument();
  });
});
