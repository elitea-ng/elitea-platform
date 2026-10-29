/**
 * Admin › Configuration › Worker Egress: the hosts reached and refused
 * (ADR-0044 decision 2, #1490), and "Copy as allowlist".
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import type { WorkerEgressHostsResponse } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { WorkerEgressHostsPanel } from './WorkerEgressHostsPanel';
import { renderAdminRoute } from './__tests__/testRouter';
import { allowlistFromReached } from './api/adminWorkerEgressApi';

const BODY: WorkerEgressHostsResponse = {
  days: 30,
  since: '2026-09-28T00:00:00Z',
  source: 'all',
  project_id: null,
  reached: [
    { project_id: 7, agent_id: 31, host: 'api.github.com', port: 443, connections: 12, executions: 4, last_seen: '2026-10-28T09:15:02Z' },
    { project_id: 7, agent_id: null, host: 'jira.example.com', port: 8443, connections: 2, executions: 1, last_seen: '2026-10-27T09:15:02Z' },
  ],
  refused: [
    { project_id: 7, agent_id: 31, host: 'paste.example', port: 443, reason: 'egress_host_not_allowed', count: 3, executions: 1, last_seen: '2026-10-28T10:00:00Z' },
  ],
  reached_truncated: false,
  refused_truncated: false,
};

let queries: string[];

beforeEach(() => {
  queries = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/worker_egress/hosts', ({ request }) => {
      queries.push(new URL(request.url).search);
      return HttpResponse.json(BODY);
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('WorkerEgressHostsPanel', () => {
  it('lists reached and refused hosts for the last 30 days, both sources', async () => {
    renderAdminRoute(<WorkerEgressHostsPanel />);

    await screen.findByText('api.github.com');
    const reached = screen.getByTestId('worker-egress-reached');
    expect(within(reached).getByText('api.github.com')).toBeInTheDocument();
    expect(within(reached).getByText('No agent')).toBeInTheDocument();
    const refused = screen.getByTestId('worker-egress-refused');
    expect(within(refused).getByText('paste.example')).toBeInTheDocument();
    expect(within(refused).getByText('egress_host_not_allowed')).toBeInTheDocument();
    expect(queries[0]).toContain('days=30');
    expect(queries[0]).toContain('source=all');
  });

  it('copies the reached hosts as allowlist entries', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<WorkerEgressHostsPanel />);

    await screen.findByText('api.github.com');
    await user.click(screen.getByRole('button', { name: 'Copy as allowlist' }));

    await waitFor(async () => {
      expect(await navigator.clipboard.readText()).toBe('api.github.com\njira.example.com:8443');
    });
    expect(screen.getByRole('button', { name: 'Copied' })).toBeInTheDocument();
  });
});

describe('allowlistFromReached', () => {
  it('uses the bare host when a default port was reached, host:port otherwise', () => {
    expect(
      allowlistFromReached([
        { host: 'A.example', port: 443 },
        { host: 'a.example', port: 8080 },
        { host: 'b.example', port: 8080 },
        { host: 'b.example', port: 9090 },
        { host: '', port: 443 },
      ]),
    ).toEqual(['a.example', 'b.example:8080', 'b.example:9090']);
  });
});
