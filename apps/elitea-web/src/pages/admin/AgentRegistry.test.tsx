/**
 * Admin › Agent registry (ADR-0040 decision 1, #1497): the cross-project
 * table with its filters, and the CSV and JSON exports. The exports are
 * asserted by the query that reached the server and the file handed to the
 * browser, not by the buttons existing.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import type { AgentRegistryRecord } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminAgentRegistry } from './AgentRegistry';
import { renderAdminRoute } from './__tests__/testRouter';

/**
 * The file handed to the browser: the blob given to `URL.createObjectURL`
 * and the `download` name of the anchor clicked for it.
 */
let downloads: { blob: Blob; filename: string }[];

const ROW: AgentRegistryRecord = {
  project_id: 12,
  application_id: 31,
  application_name: 'Refund assistant',
  business_owner_id: 4,
  technical_owner_id: 7,
  risk_tier: 'high',
  intended_use: '',
  out_of_scope_use: '',
  data_classes: ['customer_pii'],
  certification_state: 'certified',
  certified_release_id: 9,
  certified_at: '2026-10-29T09:00:00Z',
  recertify_by: '2027-04-27T09:00:00Z',
  state_reason: '',
  state_changed_at: '2026-10-29T09:00:00Z',
  suspension_id: null,
  circuit_breaker: {},
  updated_at: '2026-10-29T09:00:00Z',
};

let queries: URLSearchParams[];

beforeEach(() => {
  queries = [];
  downloads = [];
  let pending: Blob | undefined;
  vi.spyOn(URL, 'createObjectURL').mockImplementation((blob) => {
    pending = blob as Blob;
    return 'blob:registry';
  });
  vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
    if (pending !== undefined) downloads.push({ blob: pending, filename: this.download });
  });
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/agent_registry', ({ request }) => {
      const query = new URL(request.url).searchParams;
      queries.push(query);
      if (query.get('format') === 'csv') {
        return new HttpResponse('project_id,application_id\n12,31\n', {
          headers: { 'content-type': 'text/csv', 'content-disposition': 'attachment; filename="agent-registry-2026.csv"' },
        });
      }
      return HttpResponse.json({ total: 1, rows: [ROW] });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

describe('AdminAgentRegistry', () => {
  it('lists every agent with its tier and certification', async () => {
    renderAdminRoute(<AdminAgentRegistry />);
    const row = await screen.findByTestId('agent-registry-row-12-31');
    expect(row).toHaveTextContent('Refund assistant');
    expect(row).toHaveTextContent('high');
    expect(row).toHaveTextContent('certified');
    expect(row).toHaveTextContent('customer_pii');
  });

  it('sends the filters, and a project id that is not a positive integer is left out', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminAgentRegistry />);
    await screen.findByTestId('agent-registry-row-12-31');
    await user.type(screen.getByTestId('agent-registry-filter-owner'), '4');
    await user.type(screen.getByTestId('agent-registry-filter-project'), 'x');
    await waitFor(() => expect(queries.at(-1)?.get('owner_id')).toBe('4'));
    expect(queries.at(-1)?.has('project_id')).toBe(false);
  });

  it('exports CSV as the server renders it, under the server filename', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminAgentRegistry />);
    await screen.findByTestId('agent-registry-row-12-31');
    await user.click(screen.getByTestId('agent-registry-export-csv'));
    await waitFor(() => expect(downloads).toHaveLength(1));
    expect(queries.at(-1)?.get('format')).toBe('csv');
    expect(downloads[0]?.filename).toBe('agent-registry-2026.csv');
    expect(await downloads[0]?.blob.text()).toBe('project_id,application_id\n12,31\n');
  });

  it('exports JSON with every row', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminAgentRegistry />);
    await screen.findByTestId('agent-registry-row-12-31');
    await user.click(screen.getByTestId('agent-registry-export-json'));
    await waitFor(() => expect(downloads).toHaveLength(1));
    expect(downloads[0]?.filename).toBe('agent-registry.json');
    const document = JSON.parse(await (downloads[0]?.blob.text() ?? Promise.resolve('{}'))) as { rows: unknown[] };
    expect(document.rows).toEqual([ROW]);
  });
});
