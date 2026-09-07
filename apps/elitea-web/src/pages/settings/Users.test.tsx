/**
 * Rendering regression guard for `pages/settings/Users.tsx`.
 *
 * Both assertions below exist because of one shipped bug: `eliteaFetch`
 * resolves to `{ data, status, headers }`, so `resp.data` IS the response
 * body, and this page read `resp.data.data` — one level too deep — for BOTH
 * queries. Nothing failed loudly: the members table just said "No users"
 * forever, and the invite dialog's role select offered only its disabled
 * "No options" row, which (Invite being gated on a selected role) meant
 * nobody could complete an invite at all.
 *
 * It shipped because the only tests naming this page were search-param
 * cases (`src/routes/__tests__/searchParams.settings-users.test.tsx`) — no
 * test ever rendered it. The two response bodies below were measured
 * against the running stack, and they deliberately differ from each other:
 * members is `{rows, total}`, roles is a BARE ARRAY. That asymmetry is the
 * trap, so both are covered.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { http, HttpResponse } from 'msw';

import { getRoleListMockHandler, getUserListMockHandler } from '@/shared/api/generated/admin/admin.msw';
import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';

import { Users } from './Users';
import { renderSettingsRoute } from './__tests__/testRouter';

/** Measured members body: `{"rows":[…],"total":2}`. */
const MEMBERS_BODY = {
  rows: [
    { id: 'u-1', email: 'alice@example.com', name: 'Alice Admin', roles: ['admin'] },
    { id: 'u-2', email: 'bob@example.com', name: 'Bob Viewer', roles: ['viewer'] },
  ],
  total: 2,
};

/** Measured roles body — a bare array, NOT `{rows, total}`. */
const ROLES_BODY = [
  { id: '1', name: 'admin' },
  { id: '2', name: 'editor' },
  { id: '3', name: 'viewer' },
];

const ALL_USER_PERMISSIONS = Object.values(PERMISSIONS.users).map((name) => ({
  name,
  enabled: true,
}));

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    getPermissionListMockHandler(ALL_USER_PERMISSIONS),
    getUserListMockHandler(MEMBERS_BODY),
    getRoleListMockHandler(ROLES_BODY),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Users (settings)', () => {
  it('renders one table row per member from a {rows,total} body, with no "No users" and a mounted pagination footer', async () => {
    renderSettingsRoute(<Users projectId="1" />, '/settings/users', { projectId: '1' });

    // One row per member, each carrying its own name/email/role. Reading
    // `resp.data.data.rows` leaves `rows` permanently [] — `UsersTable`
    // then short-circuits to its "No users" placeholder and none of these
    // cells exist.
    expect(await screen.findByText('Alice Admin')).toBeInTheDocument();
    expect(screen.getByText('alice@example.com')).toBeInTheDocument();
    expect(screen.getByText('Bob Viewer')).toBeInTheDocument();
    expect(screen.getByText('bob@example.com')).toBeInTheDocument();

    const grid = screen.getByRole('grid');
    expect(within(grid).getAllByRole('row').filter((row) => row.getAttribute('data-id'))).toHaveLength(2);
    expect(within(grid).getByText('admin')).toBeInTheDocument();
    expect(within(grid).getByText('viewer')).toBeInTheDocument();

    // The empty-state placeholder and the table are mutually exclusive
    // branches of `UsersTable`; assert the bug's visible symptom is gone.
    expect(screen.queryByText('No users')).not.toBeInTheDocument();

    // The pagination footer is gated on `total > 0`, and `total` came from
    // the same mis-read body — so it never mounted either.
    expect(screen.getByText('Showing 1–2 of 2')).toBeInTheDocument();
    expect(screen.getByLabelText('Rows per page')).toBeInTheDocument();
  });

  it('offers every role from the bare-array roles body in the invite dialog select (never "No options")', async () => {
    const user = userEvent.setup();
    renderSettingsRoute(<Users projectId="1" />, '/settings/users', { projectId: '1' });

    // Reach the dialog the way a user does — the header's Invite button —
    // rather than via `?inviteUsers=1`: that deep link does not survive its
    // own `navigate({search:{}})` cleanup under this router fixture (see the
    // note in the test-router file), and this test is about the roles body,
    // not about PARAM-061.
    await user.click(await screen.findByTitle('Invite users'));

    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('combobox'));

    const listbox = await screen.findByRole('listbox');
    await waitFor(() => {
      expect(within(listbox).getByRole('option', { name: 'admin' })).toBeInTheDocument();
    });
    expect(within(listbox).getByRole('option', { name: 'editor' })).toBeInTheDocument();
    expect(within(listbox).getByRole('option', { name: 'viewer' })).toBeInTheDocument();

    // `SingleSelect` renders exactly one disabled "No options" item when
    // `options` is empty — the precise thing every user saw here, and the
    // reason no invite could ever be completed.
    expect(within(listbox).queryByText('No options')).not.toBeInTheDocument();
  });

  /**
   * The bulk case, at the composition root: the server answers 400 with the
   * per-address array, and every `ok` row in it has ALREADY been written.
   *
   * Both halves of this were wrong before the port. `useInviteUsers` sent the
   * whole thing to `onError`, which set a red toast from `error.message`
   * ("eliteaFetch: 400 from …") and left the member list stale; and the dialog
   * closed on its own, so the array naming which address failed was rendered
   * nowhere. A twelve-address invite where one address was already a member
   * looked exactly like a batch where all twelve were refused.
   */
  it('keeps the dialog open on a mixed batch and names what happened to every address', async () => {
    const user = userEvent.setup();
    server.use(
      http.post('*/admin/users/default/1', () =>
        HttpResponse.json(
          [
            { email: 'new@x.io', status: 'ok', outcome: 'invited', msg: 'added', id: '9', invitation_delivered: true },
            { email: 'alice@example.com', status: 'error', outcome: 'already_member', msg: 'exists' },
            { email: 'oops', status: 'error', outcome: 'invalid_email', msg: 'Invalid email: oops' },
          ],
          { status: 400 },
        ),
      ),
    );

    renderSettingsRoute(<Users projectId="1" />, '/settings/users', { projectId: '1' });
    await user.click(await screen.findByTitle('Invite users'));

    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByRole('textbox', { name: /emails/i }), 'new@x.io, alice@example.com');
    await user.click(within(dialog).getByRole('combobox'));
    await user.click(await screen.findByTestId('select-option-admin'));
    await user.click(within(dialog).getByRole('button', { name: /^invite$/i }));

    const results = await screen.findByTestId('invite-results');
    expect(within(results).getByTestId('invite-result-new@x.io')).toHaveTextContent('Invited');
    expect(within(results).getByTestId('invite-result-alice@example.com')).toHaveTextContent('Already a member');
    expect(within(results).getByTestId('invite-result-oops')).toHaveTextContent('Invalid address');

    // The dialog is still mounted: the operator has to be able to READ that.
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    // And the toast says a partial result, not a failure.
    expect(await screen.findByText('Added 1 of 3. See the list below.')).toBeInTheDocument();
  });

  it('closes and says the plural on a batch where every address landed', async () => {
    const user = userEvent.setup();
    server.use(
      http.post('*/admin/users/default/1', () =>
        HttpResponse.json([
          { email: 'a@x.io', status: 'ok', outcome: 'invited', msg: 'added', id: '9', invitation_delivered: true },
          { email: 'b@x.io', status: 'ok', outcome: 'invited', msg: 'added', id: '10', invitation_delivered: true },
        ]),
      ),
    );

    renderSettingsRoute(<Users projectId="1" />, '/settings/users', { projectId: '1' });
    await user.click(await screen.findByTitle('Invite users'));

    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByRole('textbox', { name: /emails/i }), 'a@x.io, b@x.io');
    await user.click(within(dialog).getByRole('combobox'));
    await user.click(await screen.findByTestId('select-option-admin'));
    await user.click(within(dialog).getByRole('button', { name: /^invite$/i }));

    // The port had only the singular string, so a two-address invite reported
    // "The user has been invited by e-mail".
    expect(await screen.findByText('The users have been invited by e-mail')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });
});
