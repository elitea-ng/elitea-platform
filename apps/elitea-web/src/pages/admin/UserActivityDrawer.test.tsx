/**
 * Rendering + query guard for `pages/admin/UserActivityDrawer.tsx`.
 *
 * The properties worth asserting are the ones a "does it render?" test passes
 * straight through:
 *
 *  1. Every query is SCOPED to the user the drawer was opened for. This drawer
 *     reuses the deployment-wide audit endpoints; a missing `user_id` would
 *     show the whole platform's traces under one person's name, and the rows
 *     would look perfectly plausible.
 *  2. It queries nothing at all while closed — the control sits on every row of
 *     a 20-row table, so an ungated drawer is 20 audit sweeps per page render.
 *  3. Opening it for a SECOND user does not inherit the first's state.
 *  4. A failed listing is reported, not rendered as a quiet user.
 */
import { useState } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { UserActivityDrawer } from './UserActivityDrawer';
import type { AdminUserRow } from './api/adminUsersApi';
import { renderAdminRoute } from './__tests__/testRouter';

function person(id: number, name: string): AdminUserRow {
  return {
    id,
    name,
    email: `${name.toLowerCase()}@example.com`,
    last_login: '2026-08-01T10:00:00',
    suspended: false,
    is_admin: false,
    admin_roles: [],
  };
}

const TRACES = {
  rows: [
    {
      trace_id: 'trace-aaaa-bbbb',
      start_time: '2026-08-01T10:00:00',
      duration_ms: 1250,
      span_count: 4,
      error_count: 0,
      has_error: false,
      user_email: 'ada@example.com',
      project_id: 41,
      event_types: ['api'],
      root_action: 'list_prompts',
      root_event_type: 'api',
      root_http_method: 'GET',
      root_status_code: 200,
    },
  ],
  total: 1,
};

let requestedUrls: string[] = [];

function useDrawerHandlers(): void {
  server.use(
    http.get('*/elitea_core/audit_traces/administration', ({ request }) => {
      requestedUrls.push(request.url);
      return HttpResponse.json(TRACES);
    }),
    http.get('*/elitea_core/audit/administration', ({ request }) => {
      requestedUrls.push(request.url);
      return HttpResponse.json({ rows: [], total: 0 });
    }),
    http.get('*/elitea_core/audit_trace_heatmap/administration', ({ request }) => {
      requestedUrls.push(request.url);
      return HttpResponse.json({ data: [], metadata: null });
    }),
    http.get('*/elitea_core/audit_heatmap/administration', ({ request }) => {
      requestedUrls.push(request.url);
      return HttpResponse.json({ data: [], metadata: null });
    }),
  );
}

function auditUrls(): string[] {
  return requestedUrls.filter((url) => url.includes('/elitea_core/'));
}

beforeEach(() => {
  requestedUrls = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  window.admin_ui_config = { permissions: [], vite_server_url: '/api/v2' };
  useDrawerHandlers();
});

afterEach(() => {
  resetGeneratedClient();
  delete window.admin_ui_config;
});

describe('Admin › Users › activity drawer', () => {
  it('names the user it was opened for', async () => {
    renderAdminRoute(<UserActivityDrawer user={person(11, 'Ada')} onClose={() => {}} />);

    expect(await screen.findByText('Ada (ID: 11)')).toBeInTheDocument();
  });

  it('falls back to the email for a user with no name', async () => {
    // System accounts carry an email and an empty name. The reference rendered
    // them as "  (ID: 7)".
    renderAdminRoute(
      <UserActivityDrawer user={{ ...person(7, 'Svc'), name: '' }} onClose={() => {}} />,
    );

    expect(await screen.findByText('svc@example.com (ID: 7)')).toBeInTheDocument();
  });

  it('scopes EVERY audit query to that user', async () => {
    renderAdminRoute(<UserActivityDrawer user={person(11, 'Ada')} onClose={() => {}} />);
    await screen.findByText('Ada (ID: 11)');

    // Both the listing and the heatmap must carry it: a heatmap filtered
    // differently from the table below it is a lie about the same data.
    await waitFor(() => expect(auditUrls().length).toBeGreaterThanOrEqual(2));
    for (const url of auditUrls()) {
      expect(url).toContain('user_id=11');
    }
  });

  it('renders trace rows from the shared audit endpoint', async () => {
    renderAdminRoute(<UserActivityDrawer user={person(11, 'Ada')} onClose={() => {}} />);

    expect(await screen.findByText('ada@example.com')).toBeInTheDocument();
  });

  it('switches to spans and re-queries the span endpoint, still scoped', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<UserActivityDrawer user={person(11, 'Ada')} onClose={() => {}} />);
    await screen.findByText('Ada (ID: 11)');

    await user.click(screen.getByRole('tab', { name: 'Spans' }));

    await waitFor(() =>
      expect(
        auditUrls().some(
          (url) => url.includes('/elitea_core/audit/administration') && url.includes('user_id=11'),
        ),
      ).toBe(true),
    );
  });

  it('does not query at all while it is closed', async () => {
    renderAdminRoute(<UserActivityDrawer user={null} onClose={() => {}} />);

    // The activity control sits on every row of the users table, so a drawer
    // that queried while closed would be one audit sweep per row.
    await waitFor(() => expect(auditUrls()).toHaveLength(0));
    expect(screen.queryByRole('tab', { name: 'Traces' })).not.toBeInTheDocument();
  });

  it('rebuilds its state for a second user rather than inheriting the first', async () => {
    // Switched from INSIDE the tree: `rerender` would replace the providers
    // `renderAdminRoute` mounted, which is not what the page does — it swaps
    // the `user` prop while everything above stays put.
    function SwitchableDrawer() {
      const [current, setCurrent] = useState(person(11, 'Ada'));
      return (
        <>
          <button type="button" onClick={() => setCurrent(person(12, 'Bo'))}>
            switch
          </button>
          <UserActivityDrawer user={current} onClose={() => {}} />
        </>
      );
    }

    const user = userEvent.setup();
    renderAdminRoute(<SwitchableDrawer />);
    await screen.findByText('Ada (ID: 11)');

    // Leave the first user on the Spans view.
    await user.click(screen.getByRole('tab', { name: 'Spans' }));
    await waitFor(() =>
      expect(screen.getByRole('tab', { name: 'Spans' })).toHaveAttribute('aria-selected', 'true'),
    );

    // `hidden: true`: MUI's modal Drawer marks its siblings `aria-hidden`, so
    // the switch control is outside the accessibility tree while it is open.
    await user.click(screen.getByRole('button', { name: 'switch', hidden: true }));

    expect(await screen.findByText('Bo (ID: 12)')).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Traces' })).toHaveAttribute('aria-selected', 'true');
    await waitFor(() =>
      expect(auditUrls().some((url) => url.includes('user_id=12'))).toBe(true),
    );
  });

  it('reports a failed listing instead of rendering it as a quiet user', async () => {
    server.use(
      http.get('*/elitea_core/audit_traces/administration', () =>
        HttpResponse.json({ error: 'failed to query audit traces' }, { status: 500 }),
      ),
    );
    renderAdminRoute(<UserActivityDrawer user={person(11, 'Ada')} onClose={() => {}} />);

    expect(await screen.findByText('Failed to load this user’s activity.')).toBeInTheDocument();
  });
});
