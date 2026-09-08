/**
 * The cross-project bulk invite dialog (issue 247).
 *
 * Three properties are worth asserting, and each one is a defect this platform
 * has actually shipped somewhere:
 *
 *  1. The submit REACHES THE SERVER with the body the Go handler decodes —
 *     `{users, projects, role}` as id arrays and one name. A control that
 *     renders and sends nothing is the #130/#180 class.
 *  2. A 200 whose report carries failures is NOT reported as a success. The
 *     route answers 200 for a partial batch on purpose, so a dialog that
 *     branched on the promise resolving would tell an operator that forty
 *     refusals were forty invitations.
 *  3. The per-pair outcomes are rendered apart. pylon returned one log blob, so
 *     "added" and "was already a member" and "no such role here" were
 *     indistinguishable without reading every line.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { configure, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminBulkInviteDialog } from './AdminBulkInviteDialog';
import { renderAdminRoute } from './__tests__/testRouter';

configure({ asyncUtilTimeout: 5_000 });
vi.setConfig({ testTimeout: 30_000 });

const USERS_BODY = {
  rows: [
    { id: 11, name: 'Ada Admin', email: 'ada@example.com', last_login: null, suspended: false, is_admin: true, admin_role: 'admin' },
    { id: 12, name: 'Bo Builder', email: 'bo@example.com', last_login: null, suspended: false, is_admin: false, admin_role: null },
  ],
  total: 2,
  counts: { platform: 2, system: 0 },
};

const PROJECTS_BODY = {
  rows: [
    { id: 7, name: 'alpha-team', owner_id: 11, owner_name: 'Ada', admin_names: [], status: 'active', suspended: false, create_success: true, is_personal: false },
    { id: 8, name: 'beta-team', owner_id: 11, owner_name: 'Ada', admin_names: [], status: 'active', suspended: false, create_success: true, is_personal: false },
  ],
  total: 2,
  counts: { team: 2, personal: 0 },
};

/** The plain array `/admin/roles/administration/{id}` answers. */
const ROLES_BODY = [
  { id: 1, name: 'admin' },
  { id: 2, name: 'editor' },
  { id: 3, name: 'viewer' },
];

let posted: unknown[] = [];
/** Swapped per test so one file can drive both the clean and the mixed batch. */
let report: Record<string, unknown> = {};

beforeEach(() => {
  posted = [];
  report = {
    ok: true,
    role: 'editor',
    requested: 2,
    added: 2,
    skipped: 0,
    failed: 0,
    results: [
      { user_id: 11, user_email: 'ada@example.com', project_id: 7, project_name: 'alpha-team', status: 'ok', outcome: 'added', msg: 'added' },
      { user_id: 11, user_email: 'ada@example.com', project_id: 8, project_name: 'beta-team', status: 'ok', outcome: 'added', msg: 'added' },
    ],
  };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/auth_users/administration', () => HttpResponse.json(USERS_BODY)),
    http.get('*/admin/projects/administration', () => HttpResponse.json(PROJECTS_BODY)),
    http.get('*/admin/roles/administration/*', () => HttpResponse.json(ROLES_BODY)),
    http.post('*/admin/invites_bulk/administration', async ({ request }) => {
      posted.push(await request.json());
      return HttpResponse.json(report);
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

/**
 * Pick one option out of a MUI Autocomplete by its visible label.
 *
 * The popup is opened only when it is CLOSED. `disableCloseOnSelect` keeps it
 * open after a selection, and a second click on an open combobox toggles it
 * shut — so an unconditional click makes the second pick of the same list
 * find no options at all.
 */
async function pick(user: ReturnType<typeof userEvent.setup>, testId: string, label: string) {
  const input = within(screen.getByTestId(testId)).getByRole('combobox');
  if (input.getAttribute('aria-expanded') !== 'true') await user.click(input);
  await user.click(await screen.findByRole('option', { name: label }));
}

async function chooseRole(user: ReturnType<typeof userEvent.setup>, name: string) {
  await user.click(within(screen.getByTestId('bulk-invite-role')).getByRole('combobox'));
  await user.click(await screen.findByRole('option', { name }));
}

describe('Admin › Users › Bulk invite', () => {
  it('posts {users, projects, role} — the cross product, as ids and one name', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminBulkInviteDialog open onClose={() => {}} />);

    await pick(user, 'bulk-invite-users', 'Ada Admin (ada@example.com)');
    await pick(user, 'bulk-invite-projects', 'alpha-team');
    await pick(user, 'bulk-invite-projects', 'beta-team');
    await chooseRole(user, 'editor');

    await user.click(screen.getByTestId('bulk-invite-submit'));

    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({ users: [11], projects: [7, 8], role: 'editor' });
  });

  it('cannot submit before a user, a project and a role are chosen', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminBulkInviteDialog open onClose={() => {}} />);

    expect(screen.getByTestId('bulk-invite-submit')).toBeDisabled();

    await pick(user, 'bulk-invite-users', 'Ada Admin (ada@example.com)');
    expect(screen.getByTestId('bulk-invite-submit')).toBeDisabled();

    await pick(user, 'bulk-invite-projects', 'alpha-team');
    // The projects are chosen but no role is: the roles only load once a
    // project names them, so the button stays disabled rather than posting a
    // blank role the server would refuse.
    expect(screen.getByTestId('bulk-invite-submit')).toBeDisabled();

    await chooseRole(user, 'admin');
    await waitFor(() => expect(screen.getByTestId('bulk-invite-submit')).toBeEnabled());
    expect(posted).toHaveLength(0);
  });

  it('reports a partial batch as a warning and names every outcome', async () => {
    report = {
      ok: false,
      role: 'editor',
      requested: 2,
      added: 1,
      skipped: 0,
      failed: 1,
      results: [
        { user_id: 11, user_email: 'ada@example.com', project_id: 7, project_name: 'alpha-team', status: 'ok', outcome: 'added', msg: 'added' },
        { user_id: 11, user_email: 'ada@example.com', project_id: 8, project_name: 'beta-team', status: 'error', outcome: 'unknown_role', msg: 'no such role' },
      ],
    };
    const user = userEvent.setup();
    renderAdminRoute(<AdminBulkInviteDialog open onClose={() => {}} />);

    await pick(user, 'bulk-invite-users', 'Ada Admin (ada@example.com)');
    await pick(user, 'bulk-invite-projects', 'alpha-team');
    await chooseRole(user, 'editor');
    await user.click(screen.getByTestId('bulk-invite-submit'));

    // The 200 is NOT reported as an unqualified success.
    const summary = await screen.findByTestId('bulk-invite-summary');
    expect(summary).toHaveTextContent('1 added, 0 already members, 1 refused of 2 pairs.');
    expect(summary.className).toContain('MuiAlert-colorWarning');

    // Each pair keeps its own outcome, which pylon's log blob could not do.
    const results = within(screen.getByTestId('bulk-invite-results'));
    expect(results.getByText('Added')).toBeInTheDocument();
    expect(results.getByText('Role not defined here')).toBeInTheDocument();
    expect(results.getByText('beta-team')).toBeInTheDocument();
  });

  it('reports a clean batch as a success', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminBulkInviteDialog open onClose={() => {}} />);

    await pick(user, 'bulk-invite-users', 'Ada Admin (ada@example.com)');
    await pick(user, 'bulk-invite-projects', 'alpha-team');
    await pick(user, 'bulk-invite-projects', 'beta-team');
    await chooseRole(user, 'editor');
    await user.click(screen.getByTestId('bulk-invite-submit'));

    const summary = await screen.findByTestId('bulk-invite-summary');
    expect(summary.className).toContain('MuiAlert-colorSuccess');
    expect(screen.queryByTestId('bulk-invite-error')).not.toBeInTheDocument();
  });

  it('shows the server error when the request itself is refused', async () => {
    server.use(
      http.post('*/admin/invites_bulk/administration', () =>
        HttpResponse.json({ error: 'insufficient permissions' }, { status: 403 }),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminBulkInviteDialog open onClose={() => {}} />);

    await pick(user, 'bulk-invite-users', 'Ada Admin (ada@example.com)');
    await pick(user, 'bulk-invite-projects', 'alpha-team');
    await chooseRole(user, 'editor');
    await user.click(screen.getByTestId('bulk-invite-submit'));

    expect(await screen.findByTestId('bulk-invite-error')).toBeInTheDocument();
    expect(screen.queryByTestId('bulk-invite-summary')).not.toBeInTheDocument();
  });

  it('renders nothing while it is closed, so a second batch starts clean', () => {
    renderAdminRoute(<AdminBulkInviteDialog open={false} onClose={() => {}} />);
    expect(screen.queryByTestId('bulk-invite-submit')).not.toBeInTheDocument();
  });
});
