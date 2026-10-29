/**
 * Rendering and behaviour tests for `pages/admin/Retention.tsx` (issue 1495).
 *
 * The page writes data-destruction policy and legal holds, so each write test
 * asserts the REQUEST the control produced, not only what the screen shows.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import type { LegalHold, RetentionPolicy } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminRetention } from './Retention';
import { renderAdminRoute } from './__tests__/testRouter';

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

function policy(overrides: Partial<RetentionPolicy> & { data_class: string }): RetentionPolicy {
  return {
    project_id: null,
    min_age_days: 0,
    max_age_days: null,
    allow_project_override: false,
    overrides_supported: false,
    floor_days: 1,
    enforced_by: `${overrides.data_class} sweep`,
    updated_by: null,
    updated_at: '2026-10-28T00:00:00Z',
    ...overrides,
  };
}

const POLICIES: RetentionPolicy[] = [
  policy({ data_class: 'conversation', allow_project_override: true, overrides_supported: true }),
  policy({ data_class: 'gateway_request_log', min_age_days: 1, max_age_days: 92 }),
  policy({ data_class: 'admin_audit', min_age_days: 7, max_age_days: 365, floor_days: 7 }),
  policy({ data_class: 'conversation', project_id: 4, max_age_days: 30, overrides_supported: true }),
];

const HOLD: LegalHold = {
  id: 11,
  scope: 'user',
  project_id: null,
  agent_id: null,
  user_id: 42,
  reason: 'matter 7',
  active: true,
  created_by: 1,
  created_at: '2026-10-28T00:00:00Z',
  released_by: null,
  released_at: null,
  release_reason: null,
};

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/retention/policies', () => HttpResponse.json({ policies: POLICIES })),
    http.put('*/admin/retention/policies/:dataClass', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return HttpResponse.json(POLICIES[0]);
    }),
    http.put('*/admin/retention/policies/:dataClass/projects/:projectId', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return HttpResponse.json(POLICIES[3]);
    }),
    http.delete('*/admin/retention/policies/:dataClass/projects/:projectId', ({ request }) => {
      recorded.push({ method: 'DELETE', url: request.url, body: null });
      return new HttpResponse(null, { status: 204 });
    }),
    http.get('*/admin/legal_holds', () => HttpResponse.json({ holds: [HOLD] })),
    http.post('*/admin/legal_holds', async ({ request }) => {
      recorded.push({ method: 'POST', url: request.url, body: await request.json() });
      return HttpResponse.json({ ...HOLD, id: 12 }, { status: 201 });
    }),
    http.post('*/admin/legal_holds/:id/release', async ({ request }) => {
      recorded.push({ method: 'POST', url: request.url, body: await request.json() });
      return HttpResponse.json({ ...HOLD, active: false });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminRetention - policies', () => {
  it('lists every platform class and the project overrides', async () => {
    renderAdminRoute(<AdminRetention />);
    await screen.findByTestId('admin-retention-row-conversation');
    expect(screen.getByText('LLM gateway request log')).toBeInTheDocument();
    expect(
      screen.getByText('Not saved yet: the deployment setting applies until you save this row.'),
    ).toBeInTheDocument();
    expect(screen.getByLabelText('Maximum (days): LLM gateway request log')).toHaveValue('92');
    const overrides = screen.getByRole('table', { name: 'Project overrides' });
    expect(within(overrides).getByText('4')).toBeInTheDocument();
    expect(within(overrides).getByText('30')).toBeInTheDocument();
  });

  it('saves a platform row with the maximum the operator typed, and an empty maximum as null', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRetention />);
    const max = await screen.findByLabelText('Maximum (days): Conversations');
    await user.type(max, '365');
    const row = screen.getByTestId('admin-retention-row-conversation');
    await user.click(within(row).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.url).toMatch(/\/admin\/retention\/policies\/conversation$/);
    expect(recorded[0]!.body).toEqual({ min_age_days: 0, max_age_days: 365, allow_project_override: true });

    const gatewayMax = screen.getByLabelText('Maximum (days): LLM gateway request log');
    await user.clear(gatewayMax);
    const gatewayRow = screen.getByTestId('admin-retention-row-gateway_request_log');
    await user.click(within(gatewayRow).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(recorded).toHaveLength(2));
    expect(recorded[1]!.body).toEqual({ min_age_days: 1, max_age_days: null, allow_project_override: false });
  });

  it('removes a project override', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRetention />);
    const overrides = await screen.findByRole('table', { name: 'Project overrides' });
    await user.click(within(overrides).getByRole('button', { name: 'Remove' }));
    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.method).toBe('DELETE');
    expect(recorded[0]!.url).toMatch(/\/admin\/retention\/policies\/conversation\/projects\/4$/);
  });
});

describe('AdminRetention - legal holds', () => {
  it('places a user hold with only the ids that scope needs', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRetention />);
    await user.click(screen.getByRole('tab', { name: 'Legal holds' }));
    await screen.findByTestId('admin-legal-hold-11');
    await user.click(screen.getByRole('button', { name: 'Place a legal hold' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('combobox'));
    await user.click(await screen.findByRole('option', { name: 'User' }));
    await user.type(within(dialog).getByLabelText('User id'), '42');
    await user.type(within(dialog).getByLabelText('Reason'), 'matter 8');
    await user.click(within(dialog).getByRole('button', { name: 'Place hold' }));
    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.url).toMatch(/\/admin\/legal_holds$/);
    expect(recorded[0]!.body).toEqual({ scope: 'user', user_id: 42, reason: 'matter 8' });
  });

  it('releases a hold with the reason given, after saying deletion resumes', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRetention />);
    await user.click(screen.getByRole('tab', { name: 'Legal holds' }));
    const row = await screen.findByTestId('admin-legal-hold-11');
    expect(within(row).getByText('User 42')).toBeInTheDocument();
    await user.click(within(row).getByRole('button', { name: 'Release' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/resumes on the next retention sweep/)).toBeInTheDocument();
    await user.type(within(dialog).getByLabelText('Release reason (optional)'), 'matter closed');
    await user.click(within(dialog).getByRole('button', { name: 'Release' }));
    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.url).toMatch(/\/admin\/legal_holds\/11\/release$/);
    expect(recorded[0]!.body).toEqual({ reason: 'matter closed' });
  });
});
