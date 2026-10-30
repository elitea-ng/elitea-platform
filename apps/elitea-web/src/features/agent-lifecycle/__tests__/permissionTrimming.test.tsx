/**
 * Governance › Permission trimming (ADR-0044 decision 5, #1501): the agent's
 * report from the project route, the one-click Remove (the body that reached
 * the server and the re-read after it), the last-selected-tool refusal shown
 * instead of a button, and a server refusal's sentence.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { PermissionTrimmingItem, PermissionTrimmingReport } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { GovernanceTrimmingTab } from '../ui/GovernanceTrimmingTab';
import { renderWithProviders } from './testUtils';

const BASE = '/api/v2';
const CORE = `${BASE}/elitea_core`;

function item(overrides: Partial<PermissionTrimmingItem>): PermissionTrimmingItem {
  return {
    kind: 'tool',
    key: '11/51/create_issue',
    project_id: 2,
    application_id: 7,
    application_name: 'Audit Agent',
    version_id: 11,
    version_name: 'base',
    toolkit_id: 51,
    toolkit_name: 'Jira',
    tool: 'create_issue',
    scopes: [],
    unused_scopes: [],
    used: false,
    uses: 0,
    last_used_at: null,
    candidate: true,
    removable: true,
    ...overrides,
  };
}

let report: PermissionTrimmingReport;
let reads: URLSearchParams[];
let removals: unknown[];
let removeStatus: number;

beforeEach(() => {
  reads = [];
  removals = [];
  removeStatus = 200;
  report = {
    window_days: 30,
    since: '2026-09-29T00:00:00Z',
    items: [
      item({}),
      item({
        key: '11/51/search',
        tool: 'search',
        used: true,
        uses: 4,
        last_used_at: '2026-10-20T00:00:00Z',
        candidate: false,
      }),
      item({
        key: '11/52/only',
        toolkit_id: 52,
        toolkit_name: 'Slack',
        tool: 'only',
        removable: false,
        not_removable_reason: 'last_selected_tool',
      }),
    ],
  };
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${CORE}/permission_trimming/prompt_lib/:p`, ({ request }) => {
      reads.push(new URL(request.url).searchParams);
      return HttpResponse.json(report);
    }),
    http.post(`${CORE}/permission_trimming/prompt_lib/:p/remove`, async ({ request }) => {
      removals.push(await request.json());
      if (removeStatus !== 200) {
        return HttpResponse.json(
          { code: 'not_removable', message: 'the version is published' },
          { status: removeStatus },
        );
      }
      report = { ...report, items: report.items.slice(1) };
      return HttpResponse.json({
        kind: 'tool',
        key: '11/51/create_issue',
        removed_at: '2026-10-29T09:00:00Z',
      });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('GovernanceTrimmingTab', () => {
  it('reads the agent report and offers Remove only for removable candidates', async () => {
    renderWithProviders(<GovernanceTrimmingTab projectId="2" applicationId={7} />);
    await screen.findByTestId('permission-trimming-table');
    expect(reads[0]?.get('application_id')).toBe('7');
    expect(screen.getByTestId('permission-trimming-remove-tool-11/51/create_issue')).toBeInTheDocument();
    expect(screen.queryByTestId('permission-trimming-remove-tool-11/51/search')).not.toBeInTheDocument();
    expect(screen.getByTestId('permission-trimming-row-tool-11/51/search')).toHaveTextContent('4 uses');
    expect(screen.queryByTestId('permission-trimming-remove-tool-11/52/only')).not.toBeInTheDocument();
    expect(screen.getByTestId('permission-trimming-row-tool-11/52/only')).toHaveTextContent(
      'remove the toolkit instead',
    );
  });

  it('removes one item with its key and re-reads the report', async () => {
    const user = userEvent.setup();
    renderWithProviders(<GovernanceTrimmingTab projectId="2" applicationId={7} />);
    await user.click(await screen.findByTestId('permission-trimming-remove-tool-11/51/create_issue'));
    await waitFor(() => expect(removals).toEqual([{ application_id: 7, kind: 'tool', key: '11/51/create_issue' }]));
    await waitFor(() =>
      expect(screen.queryByTestId('permission-trimming-row-tool-11/51/create_issue')).not.toBeInTheDocument(),
    );
    expect(reads.length).toBeGreaterThan(1);
  });

  it('shows the server refusal of a removal', async () => {
    removeStatus = 409;
    const user = userEvent.setup();
    renderWithProviders(<GovernanceTrimmingTab projectId="2" applicationId={7} />);
    await user.click(await screen.findByTestId('permission-trimming-remove-tool-11/51/create_issue'));
    expect(await screen.findByText(/the version is published/)).toBeInTheDocument();
  });
});
