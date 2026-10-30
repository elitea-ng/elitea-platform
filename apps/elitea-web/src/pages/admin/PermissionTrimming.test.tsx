/**
 * Admin › Permission trimming (ADR-0044 decision 5, #1501): the report across
 * projects with project and agent columns, the platform egress entry, and a
 * removal's body carrying the project id the admin route needs.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import type { PermissionTrimmingReport } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminPermissionTrimming } from './PermissionTrimming';
import { renderAdminRoute } from './__tests__/testRouter';

const REPORT: PermissionTrimmingReport = {
  window_days: 30,
  since: '2026-09-29T00:00:00Z',
  items: [
    {
      kind: 'toolkit',
      key: '11/51',
      project_id: 12,
      application_id: 31,
      application_name: 'Refund assistant',
      toolkit_id: 51,
      toolkit_name: 'Jira',
      scopes: [],
      unused_scopes: [],
      used: false,
      uses: 0,
      candidate: true,
      removable: true,
    },
    {
      kind: 'egress_host',
      key: 'platform:api.example.com',
      project_id: 0,
      application_id: 0,
      host: 'api.example.com',
      allowlist_level: 'platform',
      scopes: [],
      unused_scopes: [],
      used: false,
      uses: 0,
      candidate: true,
      removable: true,
    },
  ],
};

let queries: URLSearchParams[];
let removals: unknown[];

beforeEach(() => {
  queries = [];
  removals = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/permission_trimming', ({ request }) => {
      queries.push(new URL(request.url).searchParams);
      return HttpResponse.json(REPORT);
    }),
    http.post('*/admin/permission_trimming/remove', async ({ request }) => {
      removals.push(await request.json());
      return HttpResponse.json({
        kind: 'toolkit',
        key: '11/51',
        removed_at: '2026-10-29T09:00:00Z',
      });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminPermissionTrimming', () => {
  it('lists candidates across projects, the platform entry included', async () => {
    renderAdminRoute(<AdminPermissionTrimming />);
    const toolkit = await screen.findByTestId('permission-trimming-row-toolkit-11/51');
    expect(toolkit).toHaveTextContent('Refund assistant');
    expect(toolkit).toHaveTextContent('Jira');
    expect(screen.getByTestId('permission-trimming-row-egress_host-platform:api.example.com')).toHaveTextContent(
      'api.example.com (platform)',
    );
    expect(queries[0]?.get('candidates_only')).toBe('true');
  });

  it('removes a toolkit with its project and agent ids', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminPermissionTrimming />);
    await user.click(await screen.findByTestId('permission-trimming-remove-toolkit-11/51'));
    await waitFor(() =>
      expect(removals).toEqual([{ project_id: 12, application_id: 31, kind: 'toolkit', key: '11/51' }]),
    );
  });
});
