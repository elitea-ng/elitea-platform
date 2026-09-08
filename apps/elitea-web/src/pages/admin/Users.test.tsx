/**
 * Rendering + write-path guard for `pages/admin/Users.tsx` (unit A14).
 *
 * `pages/settings/Users.tsx` had NO rendering test before #130, which is how a
 * totally-empty members table shipped. This file exists so the admin twin
 * cannot repeat it, and it asserts the two properties that the reference page's
 * own defects show are worth asserting:
 *
 *  1. Rows actually render from the measured `{rows,total,counts}` body — the
 *     one shape `/admin/auth_users/administration` returns.
 *  2. Every enabled control REACHES THE SERVER with the body pylon defined, and
 *     every control with no server behind it is disabled with a stated reason.
 *     A control that renders but sends nothing is exactly the class #130/#180
 *     shipped; asserting only that a button exists would not catch it.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminUsers } from './Users';
import { renderAdminRoute } from './__tests__/testRouter';

/**
 * Measured against the Go handler: rows carry `suspended` (boolean) and
 * `admin_role` (string or null); `counts` is unfiltered and labels the tabs.
 */
const USERS_BODY = {
  rows: [
    {
      id: 11,
      name: 'Ada Admin',
      email: 'ada@example.com',
      last_login: '2026-08-01T10:00:00',
      suspended: false,
      is_admin: true,
      admin_role: 'admin',
    },
    {
      id: 12,
      name: 'Bo Blocked',
      email: 'bo@example.com',
      last_login: null,
      suspended: true,
      is_admin: false,
      admin_role: null,
    },
  ],
  total: 2,
  counts: { platform: 2, system: 1 },
};

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

/** Full admin-panel surface: the listing plus both write routes. */
function useAdminUserHandlers(): void {
  server.use(
    http.get('*/admin/auth_users/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json(USERS_BODY);
    }),
    http.post('*/admin/auth_users/administration', async ({ request }) => {
      recorded.push({ method: 'POST', url: request.url, body: await request.json() });
      return HttpResponse.json({ ok: true });
    }),
    http.put('*/admin/user_suspend/administration/*', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return HttpResponse.json({ id: 11, suspended: true });
    }),
    // The activity drawer's four reads. Empty, because what this file asserts
    // about the drawer is that it OPENS for the right row — the rows it then
    // shows are `UserActivityDrawer.test.tsx`'s subject. They are registered
    // even for the tests that never open it: `onUnhandledRequest: 'error'`
    // (R-M5) turns an unmocked audit call into a failure of whatever test
    // happened to trigger it.
    http.get('*/elitea_core/audit_traces/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json({ rows: [], total: 0 });
    }),
    http.get('*/elitea_core/audit/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json({ rows: [], total: 0 });
    }),
    http.get('*/elitea_core/audit_trace_heatmap/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json({ data: [], metadata: null });
    }),
    http.get('*/elitea_core/audit_heatmap/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json({ data: [], metadata: null });
    }),
  );
}

/** The permission list the Go adminui handler injects for a valid session. */
function grantAdminUiPermissions(permissions: string[]): void {
  window.admin_ui_config = { permissions, vite_server_url: '/api/v2' };
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method !== 'GET');
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  grantAdminUiPermissions(['admin.auth.users', 'admin.auth.users.super_admin']);
  useAdminUserHandlers();
});

afterEach(() => {
  resetGeneratedClient();
  delete window.admin_ui_config;
  // The export tests stub URL.createObjectURL and anchor clicks; leaking those
  // into a later file would make its downloads silently no-op.
  vi.restoreAllMocks();
});

describe('Admin › Users', () => {
  it('renders one row per user from the {rows,total,counts} body, with status from `suspended`', async () => {
    renderAdminRoute(<AdminUsers />);

    expect(await screen.findByText('Ada Admin')).toBeInTheDocument();
    expect(screen.getByText('ada@example.com')).toBeInTheDocument();
    expect(screen.getByText('Bo Blocked')).toBeInTheDocument();

    const grid = screen.getByRole('grid');
    const rows = within(grid)
      .getAllByRole('row')
      .filter((row) => row.getAttribute('data-id'));
    expect(rows).toHaveLength(2);

    // Status comes from the boolean `suspended`. The reference page reads a
    // `status` string that no response carries, so its chip was always "Active"
    // — this assertion is what separates the two readings.
    expect(within(grid).getByText('Active')).toBeInTheDocument();
    expect(within(grid).getByText('Suspended')).toBeInTheDocument();

    // A null `last_login` is "Never", not a blank cell or an Invalid Date.
    expect(within(grid).getByText('Never')).toBeInTheDocument();

    expect(screen.queryByText('No users')).not.toBeInTheDocument();
    // `counts` labels the tabs and is read from the same body.
    expect(screen.getByRole('tab', { name: 'Platform Users (2)' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'System Users (1)' })).toBeInTheDocument();
  });

  it('suspends a user by PUTting {suspended:true} to the real endpoint', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    await user.click(screen.getAllByRole('button', { name: 'Suspend user' })[0]!);

    await waitFor(() => expect(writes()).toHaveLength(1));
    const request = writes()[0]!;
    expect(request.method).toBe('PUT');
    // The id is in the PATH, mirroring pylon's `user_suspend/<mode>/<user_id>`.
    expect(request.url).toContain('/admin/user_suspend/administration/11');
    expect(request.body).toEqual({ suspended: true });
  });

  it('unsuspends the already-suspended user (the toggle reads the row, not a constant)', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Bo Blocked');

    await user.click(screen.getByRole('button', { name: 'Unsuspend user' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    // `suspended: false`. The reference computes `user.status !== 'suspended'`
    // against a field that does not exist, so it could only ever send `true` —
    // its unsuspend control was incapable of unsuspending anyone.
    expect(writes()[0]!.body).toEqual({ suspended: false });
    expect(writes()[0]!.url).toContain('/admin/user_suspend/administration/12');
  });

  it('sends the pylon set_admin_role body when the role select changes', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    await user.click(screen.getAllByRole('combobox', { name: 'Admin Role' })[0]!);
    await user.click(await screen.findByRole('option', { name: 'Viewer' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0]!.method).toBe('POST');
    expect(writes()[0]!.body).toEqual({
      action: 'set_admin_role',
      user_id: 11,
      role_name: 'viewer',
    });
  });

  it('clears every admin role by sending role_name: null, not the empty string', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    await user.click(screen.getAllByRole('combobox', { name: 'Admin Role' })[0]!);
    await user.click(await screen.findByRole('option', { name: 'None' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    // `''` is the DOM value; the server's validator accepts a role name or
    // `null`, and would reject `""` as an invalid role.
    expect(writes()[0]!.body).toEqual({
      action: 'set_admin_role',
      user_id: 11,
      role_name: null,
    });
  });

  it('deletes only after the confirmation modal is confirmed', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    await user.click(screen.getAllByRole('button', { name: 'Delete user' })[0]!);

    // Opening the modal must not itself write. Checked AFTER letting the event
    // loop settle: an eager `mutate()` on open resolves a tick later, so an
    // immediate assertion here would pass against that bug too.
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(writes()).toHaveLength(0);

    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0]!.method).toBe('POST');
    expect(writes()[0]!.body).toEqual({ action: 'delete', users: [{ id: 11 }] });
  });

  it('surfaces a refused write instead of swallowing it', async () => {
    const user = userEvent.setup();
    server.use(
      http.put('*/admin/user_suspend/administration/*', () =>
        HttpResponse.json({ error: 'insufficient permissions' }, { status: 403 }),
      ),
    );
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    await user.click(screen.getAllByRole('button', { name: 'Suspend user' })[0]!);

    // The reference page catches and discards every mutation error, so a 403
    // is indistinguishable from success followed by nothing happening.
    expect(await screen.findByRole('alert')).toBeInTheDocument();
  });

  // No "renders the unbacked controls as disabled" test survives here: this
  // page has none left. Export became a real CSV download, and activity is a
  // real drawer — the assertions below are what each of them replaced.
  it('opens the activity drawer for the row it was clicked on', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    const activityButtons = screen.getAllByRole('button', { name: 'User activity' });
    expect(activityButtons).toHaveLength(2);
    activityButtons.forEach((button) => expect(button).toBeEnabled());

    await user.click(activityButtons[1] as HTMLElement);

    // The SECOND row's user, not the first: a drawer wired to `rows[0]` looks
    // right until the operator opens it from any other row.
    expect(await screen.findByText('Bo Blocked (ID: 12)')).toBeInTheDocument();
  });

  /**
   * The export used to be a disabled button; asserting only that it is now
   * ENABLED would pass against a control that downloads an empty file. These
   * two assert the file's actual bytes and that the request carried the
   * on-screen filter — the "renders but sends nothing" class this file exists
   * to fence.
   */
  it('exports every row the current filter selects, as CSV', async () => {
    const user = userEvent.setup();
    let exported: Blob | undefined;
    vi.spyOn(URL, 'createObjectURL').mockImplementation((blob: Blob | MediaSource) => {
      exported = blob as Blob;
      return 'blob:mock-url';
    });
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);

    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    await user.click(screen.getByRole('button', { name: 'Export to CSV' }));
    await waitFor(() => expect(clickSpy).toHaveBeenCalledTimes(1));

    // The UTF-8 BOM is what makes Excel decode the file as UTF-8, so it is
    // asserted on the BYTES: `Blob.text()` strips a leading BOM per spec, and
    // an assertion on the decoded string would pass without it.
    const bytes = new Uint8Array(await exported!.arrayBuffer());
    expect([bytes[0], bytes[1], bytes[2]]).toEqual([0xef, 0xbb, 0xbf]);

    const lines = (await exported!.text()).split('\r\n');
    expect(lines[0]).toBe('Name,Email,Last login,Status,Admin Role');
    expect(lines[1]).toBe('Ada Admin,ada@example.com,2026-08-01T10:00:00,Active,Admin');
    // `suspended` again, and a null last_login as "Never" — same readings the
    // table makes, so the file cannot disagree with the screen.
    expect(lines[2]).toBe('Bo Blocked,bo@example.com,Never,Suspended,None');

    // The export walks the LIST endpoint, filtered by the active tab.
    const exportRead = recorded.filter((entry) => entry.method === 'GET').at(-1)!;
    expect(exportRead.url).toContain('user_type=platform');
    // 100, not a bigger number: the admin handler ignores a `limit` above 100
    // and silently serves 20, which the walk would read as the last page.
    expect(exportRead.url).toContain('limit=100');
  });

  it('reports a refused export instead of downloading an empty file', async () => {
    const user = userEvent.setup();
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    server.use(
      http.get('*/admin/auth_users/administration', () =>
        HttpResponse.json({ error: 'insufficient permissions' }, { status: 403 }),
      ),
    );

    await user.click(screen.getByRole('button', { name: 'Export to CSV' }));

    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(clickSpy).not.toHaveBeenCalled();
  });

  it('renders no write control at all when the served config advertises none', async () => {
    grantAdminUiPermissions([]);
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    // Presentation only — the server refuses these regardless. What matters
    // here is that hiding them does not also hide the LISTING.
    expect(screen.queryByRole('button', { name: 'Delete user' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Suspend user' })).not.toBeInTheDocument();
    expect(screen.queryByRole('combobox', { name: 'Admin Role' })).not.toBeInTheDocument();
    expect(screen.getByText('ada@example.com')).toBeInTheDocument();
  });

  it('switches to the system tab without offering the write controls there', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    await user.click(screen.getByRole('tab', { name: 'System Users (1)' }));

    await waitFor(() => {
      const listings = recorded.filter((entry) => entry.method === 'GET');
      expect(listings.some((entry) => entry.url.includes('user_type=system'))).toBe(true);
    });
    // System users are the platform's own service accounts; pylon offers no
    // role/suspend/delete control for them and neither does this port.
    expect(screen.queryByRole('button', { name: 'Delete user' })).not.toBeInTheDocument();
    expect(screen.queryByRole('combobox', { name: 'Admin Role' })).not.toBeInTheDocument();

    // Activity survives, and it is the whole reason the actions column is
    // pushed unconditionally: the column used to exist only `if (onToggleSuspended
    // || onDelete)`, and on this tab both are `undefined` — so restoring that
    // guard would delete this tab's only control with every other assertion
    // here still passing. One button per row of the (unfiltered) fixture.
    expect(screen.getAllByRole('button', { name: 'User activity' })).toHaveLength(
      USERS_BODY.rows.length,
    );
  });

  it('asks the server for the search term rather than filtering the loaded page', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    await user.type(screen.getByPlaceholderText('Search by name or email'), 'ada');

    // The listing is paginated server-side, so a client-side filter would only
    // ever search the 20 rows already loaded.
    await waitFor(() => {
      const listings = recorded.filter((entry) => entry.method === 'GET');
      expect(listings.some((entry) => entry.url.includes('search=ada'))).toBe(true);
    });
  });

  /*
   * COMPOSITION ROOT for the cross-project bulk invite (issue 247).
   *
   * `AdminBulkInviteDialog.test.tsx` drives the dialog directly, and every one
   * of its cases would pass with the dialog composed into no page at all — the
   * "dead code with no caller" class this repository keeps finding. This
   * asserts the page CARRIES the control and that pressing it mounts the real
   * dialog, which is the half a unit suite cannot see.
   */
  it('opens the bulk invite dialog from the Users page', async () => {
    server.use(
      http.get('*/admin/projects/administration', () =>
        HttpResponse.json({ rows: [], total: 0, counts: { team: 0, personal: 0 } }),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Ada Admin');

    expect(screen.queryByTestId('bulk-invite-submit')).not.toBeInTheDocument();
    await user.click(screen.getByTestId('admin-bulk-invite-open'));

    expect(await screen.findByTestId('bulk-invite-users')).toBeInTheDocument();
    expect(screen.getByTestId('bulk-invite-projects')).toBeInTheDocument();
    // Disabled until a user, a project and a role are chosen — so the control
    // cannot post an empty cross product the moment it is opened.
    expect(screen.getByTestId('bulk-invite-submit')).toBeDisabled();
    expect(writes()).toHaveLength(0);
  });
});
