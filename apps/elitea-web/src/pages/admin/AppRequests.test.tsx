/**
 * Rendering and behaviour tests for `pages/admin/AppRequests.tsx` (unit A14).
 *
 * The bar is not "the page renders". Every endpoint behind this page answered
 * 200 before this unit while doing nothing, so a screenshot proved nothing;
 * each test below asserts one of:
 *
 *  - the REQUEST the control produced. An approve button that sends the wrong
 *    id, or that smuggles a field the server refuses, looks identical on screen
 *    to one that does not — and this write notifies a real person.
 *  - that the queue's filters and paging are asked of the SERVER. The table is
 *    server-paged, so a filter applied only in the browser reorders one page and
 *    reads as working.
 *  - that a refusal is shown in the server's own words, rather than swallowed
 *    the way the reference page swallows every failure.
 *  - that a control this user may not use is not offered as though it worked.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, renderHook, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminAppRequests } from './AppRequests';
import { ADMIN_APP_REQUESTS_PAGE_SIZE, useAdminAppRequestsPage } from './useAdminAppRequestsPage';
import { renderAdminRoute } from './__tests__/testRouter';

/** Mirrors `./__tests__/testRouter`'s client, for the hook-only cases. */
function createHookQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
}

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

interface AppRequestFixture {
  readonly id: number;
  readonly user_id: number;
  readonly user_email: string;
  readonly project_id: number;
  readonly issue_type: string;
  readonly entity_id: string;
  readonly description: string;
  readonly status: string;
  readonly rejection_comment: string | null;
  readonly created_at: string;
  readonly updated_at: string;
  readonly created_project_id?: number;
}

let recorded: RecordedRequest[] = [];

const REQUESTS: AppRequestFixture[] = [
  {
    id: 501,
    user_id: 4001,
    user_email: 'ada@example.com',
    project_id: 1,
    issue_type: 'Wikis',
    entity_id: 'wikis_Wikis',
    description: 'We need the wiki toolkit for onboarding docs.',
    status: 'pending',
    rejection_comment: null,
    created_at: '2026-08-01 09:00:00',
    updated_at: '2026-08-01 09:00:00',
  },
  {
    id: 502,
    user_id: 4001,
    user_email: 'ada@example.com',
    project_id: 1,
    issue_type: 'Inventory',
    entity_id: 'inventory',
    description: 'Asset tracking for the hardware lab.',
    status: 'approved',
    rejection_comment: null,
    created_at: '2026-08-02 09:00:00',
    updated_at: '2026-08-02 10:00:00',
  },
  {
    // Carries a reason on purpose: the reference page has no column for it, so
    // the operator's own words vanish the moment the reject dialog closes.
    id: 503,
    user_id: 4002,
    user_email: 'grace@example.com',
    project_id: 2,
    issue_type: 'Wikis',
    entity_id: 'wikis_Wikis',
    description: 'Second team wants the same toolkit.',
    status: 'rejected',
    rejection_comment: 'Not licensed for this tenant.',
    created_at: '2026-08-03 09:00:00',
    updated_at: '2026-08-03 11:00:00',
  },
  {
    // #871: approving a "Project Request" row provisions a real project,
    // unlike every issue type above — the queue is the only place an
    // operator can see WHICH project one of their approvals created.
    id: 504,
    user_id: 4003,
    user_email: 'alice@example.com',
    project_id: 3,
    issue_type: 'Project Request',
    entity_id: 'Marketing Automation',
    description: 'We need a project for the new campaign.',
    status: 'approved',
    rejection_comment: null,
    created_at: '2026-08-04 09:00:00',
    updated_at: '2026-08-04 09:05:00',
    created_project_id: 42,
  },
];

let listBody: () => Response = () =>
  HttpResponse.json({ rows: REQUESTS, total: REQUESTS.length });
let decisionBody: () => Response = () => HttpResponse.json({ id: 501 });

function useAppRequestHandlers(): void {
  server.use(
    http.get('*/admin/moderation_statuses/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return listBody();
    }),
    http.put('*/admin/moderation_status/administration', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return decisionBody();
    }),
  );
}

/** The permission list the Go adminui handler injects for a valid session. */
function grantAdminUiPermissions(permissions: string[]): void {
  window.admin_ui_config = { permissions, vite_server_url: '/api/v2' };
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method === 'PUT');
}

function reads(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method === 'GET');
}

/**
 * The queue's OWN reads, discriminated from the issue-type filter's separate
 * options-sample query by `limit` — the queue always asks for
 * `ADMIN_APP_REQUESTS_PAGE_SIZE`, the sample always asks for more. Both fire
 * on first mount, and nothing orders their two responses relative to each
 * other, so "the last GET recorded" is not reliably the queue's.
 */
function queueReads(): RecordedRequest[] {
  return reads().filter(
    (entry) => new URL(entry.url).searchParams.get('limit') === String(ADMIN_APP_REQUESTS_PAGE_SIZE),
  );
}

function lastReadParams(): URLSearchParams {
  const all = queueReads();
  return new URL(all[all.length - 1]!.url).searchParams;
}

async function waitForQueue(): Promise<void> {
  await screen.findByText('We need the wiki toolkit for onboarding docs.');
}

beforeEach(() => {
  recorded = [];
  listBody = () => HttpResponse.json({ rows: REQUESTS, total: REQUESTS.length });
  decisionBody = () => HttpResponse.json({ id: 501 });
  configureGeneratedClient({ baseUrl: '/api/v2' });
  grantAdminUiPermissions(['admin.moderation', 'admin.moderation.edit']);
  useAppRequestHandlers();
});

afterEach(() => {
  resetGeneratedClient();
  delete window.admin_ui_config;
});

describe('AdminAppRequests — the queue', () => {
  it('renders the requester, the label and the catalogue key of each request', async () => {
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    // The requester is the whole point of the queue: an operator answers a
    // person, not a row id.
    // `getAllBy`: ada filed two of the three fixture rows, and a `getBy` here
    // would fail on the multiple match rather than on anything about the page.
    expect(screen.getAllByText('ada@example.com')).toHaveLength(2);
    expect(screen.getByText('grace@example.com')).toBeInTheDocument();

    // "Application" is `issue_type` (the label the requesting client showed)
    // with `entity_id` beneath it. The reference renders `entity_id` alone,
    // capitalised with underscores stripped, which turns `wikis_Wikis` into
    // "Wikis Wikis" and elitea-web's synthetic key into a bare number.
    expect(screen.getAllByText('Wikis')).not.toHaveLength(0);
    expect(screen.getAllByText('wikis_Wikis')).not.toHaveLength(0);
    expect(screen.getByText('inventory')).toBeInTheDocument();
  });

  it('renders a rejection reason back on the row that carries it', async () => {
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    expect(screen.getByText(/Not licensed for this tenant\./)).toBeInTheDocument();
  });

  it('renders the created project id on an approved Project Request row (#871)', async () => {
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    expect(screen.getByText('Marketing Automation')).toBeInTheDocument();
    expect(screen.getByText('Project #42 created')).toBeInTheDocument();
  });

  it('reads timestamps as UTC rather than as the viewer local time', async () => {
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    // The column is `TIMESTAMP` without a zone and the server sends it bare.
    // Rendering it as local time shifts every row by the viewer offset, which
    // looks plausible and is wrong.
    const expected = new Date('2026-08-01T09:00:00Z').toLocaleString();
    expect(screen.getByText(expected)).toBeInTheDocument();
  });

  it('asks the SERVER for the pending queue on first load', async () => {
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    const params = lastReadParams();
    // Pending is the default because it is the only tab with anything to do.
    expect(params.get('status')).toBe('pending');
    expect(params.get('limit')).toBe('20');
    expect(params.get('offset')).toBe('0');
    expect(params.get('sort_by')).toBe('created_at');
    expect(params.get('sort_order')).toBe('desc');
  });

  it('sends the status filter to the server, and sends none at all for All', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    await user.click(screen.getByRole('tab', { name: 'Rejected' }));
    await waitFor(() => expect(lastReadParams().get('status')).toBe('rejected'));

    // `all` is the ABSENCE of the filter, not a fourth value the server knows.
    // Sending `status=all` would filter the queue down to nothing.
    await user.click(screen.getByRole('tab', { name: 'All' }));
    await waitFor(() => expect(lastReadParams().has('status')).toBe(false));
  });

  it('sends the search term to the server rather than filtering the page', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    await user.type(screen.getByRole('textbox'), 'grace');
    await waitFor(() => expect(lastReadParams().get('search')).toBe('grace'));
  });

  it('sends the issue-type filter to the server, and sends none at all for All', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    // The fixture rows' distinct `issue_type`s ("Wikis", "Inventory") are the
    // options offered — there is no fixed enum for this filter (see
    // `useAdminAppRequestsPage`'s module doc), so the control has nothing to
    // show until the queue's own data supplies it.
    await user.click(screen.getByRole('combobox', { name: 'Issue Type' }));
    await user.click(await screen.findByRole('option', { name: 'Inventory' }));
    await waitFor(() => expect(lastReadParams().get('issue_type')).toBe('Inventory'));

    // `All` is the absence of the filter, not a value the server knows — the
    // same contract the status tabs use.
    await user.click(screen.getByRole('combobox', { name: 'Issue Type' }));
    await user.click(await screen.findByRole('option', { name: 'All' }));
    await waitFor(() => expect(lastReadParams().has('issue_type')).toBe(false));
  });

  it('renders a Model Connection Request row with a readable entity label', async () => {
    listBody = () =>
      HttpResponse.json({
        rows: [
          {
            id: 601,
            user_id: 4010,
            user_email: 'grace2@example.com',
            project_id: 5,
            issue_type: 'Model Connection Request',
            entity_id: 'provider:openai',
            description: 'Need OpenAI connectivity for the support bot.',
            status: 'pending',
            rejection_comment: null,
            created_at: '2026-08-20 09:00:00',
            updated_at: '2026-08-20 09:00:00',
          },
          {
            id: 602,
            user_id: 4011,
            user_email: 'hank@example.com',
            project_id: 5,
            issue_type: 'Model Connection Request',
            // Percent-encoded, matching `buildModelConnectionEntityId` in
            // `features/settings/ui/ai-configuration/RequestModelConnection.tsx`
            // — a vendor-prefixed model id's `/` would otherwise split the
            // path segment this travels as.
            entity_id: `model:${encodeURIComponent('meta-llama/Llama-3.1-70B')}`,
            description: 'Need the Llama 3.1 70B model available.',
            status: 'pending',
            rejection_comment: null,
            created_at: '2026-08-21 09:00:00',
            updated_at: '2026-08-21 09:00:00',
          },
        ],
        total: 2,
      });
    renderAdminRoute(<AdminAppRequests />);

    await screen.findByText('Need OpenAI connectivity for the support bot.');

    expect(screen.getAllByText('Model Connection Request')).toHaveLength(2);
    // The `provider:<type>` / `model:<name>` convention renders as a label
    // rather than the raw wire value — every other issue type's `entity_id`
    // is opaque and renders as-is (see "renders the requester, the label and
    // the catalogue key…" above). The model name also comes back DECODED,
    // not as the percent-escaped value the server actually stores.
    expect(screen.getByText('Provider: openai')).toBeInTheDocument();
    expect(screen.getByText('Model: meta-llama/Llama-3.1-70B')).toBeInTheDocument();
  });

  it('shows the server own words when the queue read is refused', async () => {
    listBody = () =>
      HttpResponse.json({ error: 'failed to read app requests' }, { status: 500 });
    renderAdminRoute(<AdminAppRequests />);

    const alert = await screen.findByTestId('admin-app-requests-unavailable');
    // Not "Failed to load": the generic sentence would hide the only line that
    // says which of several refusals this was.
    expect(alert).toHaveTextContent('failed to read app requests');
  });
});

describe('AdminAppRequests — the decision', () => {
  it('approves by id, and sends nothing else', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    await user.click(screen.getByRole('button', { name: 'Approve request: Wikis' }));
    await waitFor(() => expect(writes()).toHaveLength(1));

    const body = writes()[0]!.body as Record<string, unknown>;
    expect(body['id']).toBe(501);
    expect(body['status']).toBe('approved');
    // The server refuses a body carrying any of these with a 400, because the
    // moderator may not rewrite what was asked. Sending them would turn every
    // approval into a refusal.
    for (const forbidden of [
      'entity_id',
      'issue_type',
      'description',
      'user_id',
      'project_id',
      'meta',
      'rejection_comment',
    ]) {
      expect(body).not.toHaveProperty(forbidden);
    }
  });

  it('says the requester was notified, because that is what approving does', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    await user.click(screen.getByRole('button', { name: 'Approve request: Wikis' }));
    const saved = await screen.findByTestId('admin-app-requests-saved');
    expect(saved).toHaveTextContent('notified');
  });

  it('refuses to send a rejection with no reason', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    await user.click(screen.getByRole('button', { name: 'Reject request: Wikis' }));
    await user.click(screen.getByTestId('admin-app-requests-reject-confirm'));

    expect(await screen.findByText('A reason is required.')).toBeInTheDocument();
    // The server would answer 400. A dialog that sent it anyway would be making
    // a request it knows fails, and reporting the failure as if it were a
    // server problem.
    expect(writes()).toHaveLength(0);
  });

  it('sends the reason with the rejection', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    await user.click(screen.getByRole('button', { name: 'Reject request: Wikis' }));
    // The dialog names the row being rejected. The reference dialog shows only
    // the words "Reject Request", so an operator working a queue confirms
    // without seeing which one.
    expect(screen.getByTestId('admin-app-requests-reject-subject')).toHaveTextContent(
      'ada@example.com',
    );

    await user.type(
      screen.getByTestId('admin-app-requests-reject-reason'),
      '  Not licensed for this tenant.  ',
    );
    await user.click(screen.getByTestId('admin-app-requests-reject-confirm'));

    await waitFor(() => expect(writes()).toHaveLength(1));
    const body = writes()[0]!.body as Record<string, unknown>;
    expect(body).toEqual({
      id: 501,
      status: 'rejected',
      rejection_comment: 'Not licensed for this tenant.',
    });

    // The banner reports the decision that was actually taken. Reporting
    // "approved" after a rejection would tell the operator the opposite of what
    // the requester was just sent.
    expect(await screen.findByTestId('admin-app-requests-saved')).toHaveTextContent(
      'Request rejected',
    );
  });

  it('does not send a rejection whose reason is only whitespace, even if the dialog let it through', async () => {
    // The dialog blocks an empty reason too. This asserts the guard in the HOOK,
    // which is the layer that builds the request — mutation testing showed the
    // two guards masking each other, so each is pinned where it lives.
    const client = createHookQueryClient();
    const { result } = renderHook(() => useAdminAppRequestsPage(), {
      wrapper: ({ children }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    });
    await waitFor(() => expect(result.current.rows).toHaveLength(REQUESTS.length));

    act(() => result.current.onOpenReject?.(result.current.rows[0]!));
    await waitFor(() => expect(result.current.rejecting).not.toBeNull());

    act(() => result.current.onConfirmReject?.('   '));
    await waitFor(() => expect(reads().length).toBeGreaterThan(0));
    expect(writes()).toHaveLength(0);
    // …and the dialog stays open on that request, rather than closing as though
    // the decision had been taken.
    expect(result.current.rejecting).not.toBeNull();
  });

  it('shows the server own refusal instead of a generic failure', async () => {
    decisionBody = () =>
      HttpResponse.json(
        { error: 'rejection_comment is required when rejecting a request' },
        { status: 400 },
      );
    const user = userEvent.setup();
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    await user.click(screen.getByRole('button', { name: 'Approve request: Wikis' }));
    const alert = await screen.findByTestId('admin-app-requests-error');
    expect(alert).toHaveTextContent('rejection_comment is required');
  });

  it('offers no decision control on a request that is already decided', async () => {
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    const table = screen.getByRole('grid');
    const decided = within(table).getByText('Asset tracking for the hardware lab.').closest('[role="row"]');
    expect(decided).not.toBeNull();
    expect(within(decided as HTMLElement).queryByRole('button')).toBeNull();
  });

  it('offers no decision control at all when the panel advertises no edit permission', async () => {
    // Presentation only — the server refuses regardless — but a button that
    // always fails is worse than no button.
    grantAdminUiPermissions(['admin.moderation']);
    renderAdminRoute(<AdminAppRequests />);
    await waitForQueue();

    expect(screen.queryByRole('button', { name: /Approve request/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /Reject request/ })).toBeNull();
  });
});
