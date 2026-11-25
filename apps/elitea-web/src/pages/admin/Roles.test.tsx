/**
 * Rendering and behaviour tests for `pages/admin/Roles.tsx` (unit A14).
 *
 * The bar these have to clear is not "the page renders". This surface grants
 * privilege, so each test asserts one of:
 *
 *  - the REQUEST the control produced (a Save that sends the wrong matrix is
 *    indistinguishable on screen from one that sends the right one);
 *  - that a control the server would refuse is not offered as though it worked;
 *  - that an operator's in-progress edit is not silently discarded.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, delay, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminRoles } from './Roles';
import { renderAdminRoute } from './__tests__/testRouter';
import { enabledWriteControls, findViewOnlyNotice, serveConsoleLevel } from './__tests__/viewOnly';

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

interface MatrixRow {
  readonly name: string;
  readonly [role: string]: string | boolean;
}

let recorded: RecordedRequest[] = [];

/**
 * `auditor` is deliberately NOT one of the five roles the page knows how to
 * order. A deployment may define others, and a column the page drops is a
 * privilege the operator cannot see, let alone revoke.
 */
const ADMIN_ROWS: MatrixRow[] = [
  { name: 'models.alpha.view', system: true, admin: true, editor: true, viewer: true, auditor: true },
  { name: 'models.alpha.edit', system: true, admin: true, editor: false, viewer: false, auditor: false },
  {
    name: 'configuration.roles.permissions.view',
    system: true,
    admin: true,
    editor: false,
    viewer: false,
    auditor: false,
  },
];

/** What the SECOND read of the admin matrix returns — see the refetch test. */
const ADMIN_ROWS_CHANGED: MatrixRow[] = ADMIN_ROWS.map((row) =>
  row.name === 'models.alpha.view' ? { ...row, viewer: false } : row,
);

let adminReads = 0;
let adminRowsAfterFirstRead: MatrixRow[] = ADMIN_ROWS;

const STANDARD_ROWS: MatrixRow[] = [
  { name: 'models.alpha.view', system: true, admin: true, editor: true, viewer: false },
];

/** `SUPPORT_PROJECT_ID` unset — the legacy API and roles.go both 404 with this. */
const SUPPORT_UNAVAILABLE = 'project not configured: set SUPPORT_PROJECT_ID';

function matrixBody(rows: MatrixRow[]): { rows: MatrixRow[]; total: number } {
  return { rows, total: rows.length };
}

function useRolesHandlers(): void {
  server.use(
    http.get('*/admin/permissions/administration/administration', async ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      adminReads += 1;
      if (adminReads === 1) return HttpResponse.json(matrixBody(ADMIN_ROWS));
      // Slow enough that the page's fetching indicator actually renders — the
      // refetch test needs an observable "React has seen this query settle"
      // point, and an instant response never produces one.
      await delay(30);
      return HttpResponse.json(matrixBody(adminRowsAfterFirstRead));
    }),
    http.get('*/admin/permissions/administration/default', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json(matrixBody(STANDARD_ROWS));
    }),
    http.get('*/admin/permissions/public/default', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json(matrixBody(STANDARD_ROWS));
    }),
    http.get('*/admin/permissions/support/default', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json({ error: SUPPORT_UNAVAILABLE }, { status: 404 });
    }),
    http.put('*/admin/permissions/:scope/:mode', async ({ request, params }) => {
      recorded.push({ method: 'PUT', url: String(params.scope), body: await request.json() });
      return HttpResponse.json({ ok: true, granted: 1, revoked: 0 });
    }),
    http.post('*/admin/permissions/:scope/:mode', async ({ request, params }) => {
      recorded.push({ method: 'POST', url: String(params.scope), body: await request.text() });
      return HttpResponse.json({ ok: true });
    }),
  );
}

/** The permission list the Go adminui handler injects for a valid session. */
function grantAdminUiPermissions(permissions: string[]): void {
  window.admin_ui_config = { permissions, vite_server_url: '/api/v2' };
}

async function expandGroup(user: ReturnType<typeof userEvent.setup>, group: string): Promise<void> {
  await user.click(await screen.findByRole('button', { name: `Expand permission group: ${group}` }));
}

function lastPut(): RecordedRequest | undefined {
  return [...recorded].reverse().find((entry) => entry.method === 'PUT');
}

beforeEach(() => {
  recorded = [];
  adminReads = 0;
  adminRowsAfterFirstRead = ADMIN_ROWS;
  configureGeneratedClient({ baseUrl: '/api/v2' });
  grantAdminUiPermissions([
    'configuration.roles.permissions.view',
    'configuration.roles.permissions.edit',
  ]);
  useRolesHandlers();
});

afterEach(() => {
  resetGeneratedClient();
  delete window.admin_ui_config;
});

describe('Admin › Roles', () => {
  it('groups permissions and reveals a group on demand', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    expect(await screen.findByText('models.alpha')).toBeInTheDocument();
    expect(screen.getByText('configuration.roles')).toBeInTheDocument();
    // Collapsed by default: the group's own permissions are not rendered.
    expect(screen.queryByText('view')).not.toBeInTheDocument();

    await expandGroup(user, 'models.alpha');
    expect(screen.getByText('view')).toBeInTheDocument();
    expect(screen.getByText('edit')).toBeInTheDocument();
  });

  it('renders one column per role the server reported', async () => {
    renderAdminRoute(<AdminRoles />);

    const table = await screen.findByRole('table', { name: 'Permission matrix' });
    for (const role of ['system', 'admin', 'editor', 'viewer', 'auditor']) {
      expect(within(table).getByRole('columnheader', { name: role })).toBeInTheDocument();
    }
  });

  it('renders a role the page has no ordering for rather than dropping it', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    // A dropped column is a privilege nobody can see, let alone revoke.
    const cell = screen.getByRole('checkbox', { name: 'auditor: models.alpha.view' });
    expect(cell).toBeChecked();

    await user.click(cell);
    await user.click(await screen.findByRole('button', { name: 'Save' }));
    await waitFor(() => expect(lastPut()).toBeDefined());
    const rows = lastPut()?.body as MatrixRow[];
    expect(rows.find((row) => row.name === 'models.alpha.view')?.auditor).toBe(false);
  });

  it('sends the whole matrix with the toggled cell flipped', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    // Nothing to save until something changed.
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();

    await user.click(screen.getByRole('checkbox', { name: 'editor: models.alpha.edit' }));
    await user.click(await screen.findByRole('button', { name: 'Save' }));

    await waitFor(() => expect(lastPut()).toBeDefined());
    const sent = lastPut();
    expect(sent?.url).toBe('administration');
    // The whole matrix goes back, not just the delta: the server diffs it, and
    // a partial body would only ever revoke what it happened to carry.
    expect(sent?.body).toEqual([
      { name: 'models.alpha.view', system: true, admin: true, editor: true, viewer: true, auditor: true },
      { name: 'models.alpha.edit', system: true, admin: true, editor: true, viewer: false, auditor: false },
      {
        name: 'configuration.roles.permissions.view',
        system: true,
        admin: true,
        editor: false,
        viewer: false,
        auditor: false,
      },
    ]);
    expect(await screen.findByText('Permissions saved.')).toBeInTheDocument();
  });

  it('explains a refusal by the no-escalation rule, with the role and the permissions', async () => {
    // Issue 2488: the server refuses a grant the caller does not hold with a
    // 403 and a stable code. The page shows what was refused and why, not
    // "Failed to save permissions.", and keeps the edit for the operator.
    server.use(
      http.put('*/admin/permissions/:scope/:mode', () =>
        HttpResponse.json(
          {
            error: 'you can grant or revoke only permissions that you hold',
            code: 'role_permission_not_held',
            role: 'editor',
            permissions: ['models.alpha.edit'],
          },
          { status: 403 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    await user.click(screen.getByRole('checkbox', { name: 'editor: models.alpha.edit' }));
    await user.click(await screen.findByRole('button', { name: 'Save' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(
      'You can grant, assign or remove only permissions that you hold yourself. Role: editor. Permissions: models.alpha.edit.',
    );
    expect(screen.queryByText('Failed to save permissions.')).not.toBeInTheDocument();
    expect(screen.queryByText('Permissions saved.')).not.toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'editor: models.alpha.edit' })).toBeChecked();
  });

  it('shows the reserved permission refusal in its own words', async () => {
    server.use(
      http.put('*/admin/permissions/:scope/:mode', () =>
        HttpResponse.json(
          {
            error: 'admin.auth.users.super_admin can be granted only to the administration super_admin role',
            code: 'role_permission_reserved',
            role: 'admin',
            permissions: ['admin.auth.users.super_admin'],
          },
          { status: 403 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    await user.click(screen.getByRole('checkbox', { name: 'editor: models.alpha.edit' }));
    await user.click(await screen.findByRole('button', { name: 'Save' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      "This permission is granted only by the platform's own setup, so no change here can add it to a role.",
    );
  });

  it.each([
    {
      code: 'role_self_assignment',
      error: 'you cannot change your own administration roles; another administrator must make this change',
      message: 'You cannot change your own administration roles. Ask another administrator. Role: editor.',
    },
    {
      code: 'role_set_locked',
      error: '"editor" is a built-in role and cannot be edited',
      message:
        'This is a built-in role, so its permissions are locked. Create a custom role for a different set. Role: editor.',
    },
  ])('shows the $code refusal in its own words', async ({ code, error, message }) => {
    server.use(
      http.put('*/admin/permissions/:scope/:mode', () =>
        HttpResponse.json({ error, code, role: 'editor' }, { status: 403 }),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    await user.click(screen.getByRole('checkbox', { name: 'editor: models.alpha.edit' }));
    await user.click(await screen.findByRole('button', { name: 'Save' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(message);
    expect(alert).not.toHaveTextContent(error);
  });

  it('names every built-in role a locked-set refusal is about', async () => {
    server.use(
      http.put('*/admin/permissions/:scope/:mode', () =>
        HttpResponse.json(
          { error: 'built-in', code: 'role_set_locked', role: 'admin', roles: ['admin', 'editor'] },
          { status: 403 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    await user.click(screen.getByRole('checkbox', { name: 'editor: models.alpha.edit' }));
    await user.click(await screen.findByRole('button', { name: 'Save' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'This is a built-in role, so its permissions are locked. Create a custom role for a different set. Roles: admin, editor.',
    );
  });

  it('marks the names the server deprecates, and explains a permission_deprecated refusal', async () => {
    // ADR-0059 R0.14a: the catalogue beside the rows marks runtime.plugins
    // deprecated, and adding it to a role answers 400 permission_deprecated.
    const legacyRows: MatrixRow[] = [
      ...ADMIN_ROWS,
      { name: 'runtime.plugins', system: true, admin: true, editor: false, viewer: false, auditor: false },
    ];
    server.use(
      http.get('*/admin/permissions/administration/administration', () =>
        HttpResponse.json({
          ...matrixBody(legacyRows),
          catalogue: legacyRows.map((row) => ({ name: row.name, deprecated: row.name === 'runtime.plugins' })),
        }),
      ),
      http.put('*/admin/permissions/:scope/:mode', async ({ request }) => {
        recorded.push({ method: 'PUT', url: 'administration', body: await request.json() });
        return HttpResponse.json(
          {
            error: 'runtime.plugins ends on the sunset date and can no longer be added to a role',
            code: 'permission_deprecated',
            role: 'editor',
            permissions: ['runtime.plugins'],
          },
          { status: 400 },
        );
      }),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'runtime.plugins');
    const marks = await screen.findAllByTestId('admin-roles-deprecated-mark');
    expect(marks).toHaveLength(1);
    expect(marks[0]).toHaveTextContent('Deprecated');
    const legacyRow = marks[0]?.closest('tr');
    expect(legacyRow).toHaveTextContent('runtime.plugins');
    await expandGroup(user, 'models.alpha');
    expect(screen.getAllByTestId('admin-roles-deprecated-mark')).toHaveLength(1);

    // The group row of `runtime.plugins` has a checkbox of the same name;
    // the cell is the one in the marked row. A role that does not hold the
    // deprecated name cannot be given it; the holder can still drop it.
    const row = within(legacyRow as HTMLElement);
    expect(row.getByRole('checkbox', { name: 'editor: runtime.plugins' })).toBeDisabled();
    expect(row.getByRole('checkbox', { name: 'admin: runtime.plugins' })).toBeEnabled();
    // The group toggle skips the locked cell rather than tick it.
    const groupRow = screen
      .getAllByTestId('admin-roles-group-row')
      .find((candidate) => candidate.textContent?.includes('runtime.plugins')) as HTMLElement;
    await user.click(within(groupRow).getByRole('checkbox', { name: 'editor: runtime.plugins' }));
    expect(row.getByRole('checkbox', { name: 'editor: runtime.plugins' })).not.toBeChecked();

    // A refusal still explains itself (another administrator's view was older).
    await user.click(row.getByRole('checkbox', { name: 'admin: runtime.plugins' }));
    await user.click(await screen.findByRole('button', { name: 'Save' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(
      'This permission is being retired, so it can no longer be added to a role. Grant the narrower permission that replaces it. Role: editor. Permissions: runtime.plugins.',
    );
    expect(alert).not.toHaveTextContent('sunset date');
    // The rows went back without the catalogue marks: every key but `name` is a role.
    const sent = lastPut()?.body as MatrixRow[];
    expect(sent.every((row) => !('deprecated' in row))).toBe(true);
  });

  it('keeps the en.json keys of the deprecated mark and its refusal', async () => {
    const catalogue = (await import('@/shared/i18n/en.json')).default as Record<string, string>;
    for (const key of [
      'pages.admin.roles.deprecated',
      'pages.admin.roles.deprecatedHint',
      'pages.admin.roles.refusal.deprecated',
    ]) {
      expect(catalogue[key], key).toBeTruthy();
    }
  });

  it('toggles a whole group from its group checkbox', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await user.click(await screen.findByRole('checkbox', { name: 'editor: models.alpha' }));
    await user.click(await screen.findByRole('button', { name: 'Save' }));

    await waitFor(() => expect(lastPut()).toBeDefined());
    const rows = lastPut()?.body as MatrixRow[];
    // `models.alpha.view` was already on and `models.alpha.edit` was off, so the
    // group aggregate was "some" — toggling it must turn the group ON, not off.
    expect(rows.filter((row) => row.name.startsWith('models.alpha')).every((row) => row.editor === true)).toBe(true);
    // …and must not touch the other group.
    expect(rows.find((row) => row.name === 'configuration.roles.permissions.view')?.editor).toBe(false);
  });

  it('discards an edit back to what the server sent', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    await user.click(screen.getByRole('checkbox', { name: 'viewer: models.alpha.edit' }));
    await user.click(await screen.findByRole('button', { name: 'Discard' }));

    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument(),
    );
    expect(screen.getByRole('checkbox', { name: 'viewer: models.alpha.edit' })).not.toBeChecked();
    expect(recorded.some((entry) => entry.method === 'PUT')).toBe(false);
  });

  it('keeps an in-progress edit when the data is refetched underneath it', async () => {
    const user = userEvent.setup();
    // The refetch must return DIFFERENT rows. React Query's structural sharing
    // hands back the identical reference for a deep-equal response, so a
    // re-seeding bug would be invisible against an unchanged fixture.
    adminRowsAfterFirstRead = ADMIN_ROWS_CHANGED;
    const { queryClient } = renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    await user.click(screen.getByRole('checkbox', { name: 'editor: models.alpha.edit' }));
    expect(screen.getByRole('checkbox', { name: 'editor: models.alpha.edit' })).toBeChecked();

    // The #191 shape: a background refetch must not re-seed the draft.
    //
    // `refetchQueries` is AWAITED rather than `invalidateQueries` fired and then
    // waited on by request count: the request is recorded before it is answered,
    // so counting it would let the assertions run before the new data — and the
    // effect it would have triggered — reached React at all. The mutant that
    // re-seeds unconditionally survives that version of the test.
    const refetch = queryClient.refetchQueries();
    // Waiting on the RECORDED request is not enough: it is recorded before it
    // is answered, so the assertions would run before the new data — and the
    // effect it would have triggered — reached React at all. The page's own
    // fetching indicator appearing and then going away is a signal that React
    // has rendered the settled query, and it is independent of the draft.
    await waitFor(() => expect(screen.getByRole('progressbar')).toBeInTheDocument());
    await refetch;
    await waitFor(() => expect(screen.queryByRole('progressbar')).not.toBeInTheDocument());
    expect(recorded.filter((entry) => entry.method === 'GET').length).toBeGreaterThan(1);

    expect(screen.getByRole('checkbox', { name: 'editor: models.alpha.edit' })).toBeChecked();
    expect(screen.getByRole('button', { name: 'Save' })).toBeInTheDocument();
    // The refetched CHANGE is not displayed either: the draft is the document
    // on screen until the operator saves or discards it.
    expect(screen.getByRole('checkbox', { name: 'viewer: models.alpha.view' })).toBeChecked();
  });

  it('never offers the system column, which the server refuses to write', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    expect(screen.getByRole('checkbox', { name: 'system: models.alpha.view' })).toBeDisabled();
    expect(screen.getByRole('checkbox', { name: 'system: models.alpha' })).toBeDisabled();
    // …while a writable column is not disabled, so the assertion above is about
    // `system` and not about the whole matrix being read-only.
    expect(screen.getByRole('checkbox', { name: 'admin: models.alpha.view' })).toBeEnabled();
  });

  // Read-only mode (R3.4b, issue 2613): the security auditor reads `access`,
  // so every write control is disabled, not hidden, and reading still works.
  it('is view only for a role that reads the access item: every write control disabled', async () => {
    const user = userEvent.setup();
    serveConsoleLevel('read', 'access');
    renderAdminRoute(<AdminRoles />);

    expect(await findViewOnlyNotice()).toHaveTextContent('Platform administrator');
    await expandGroup(user, 'models.alpha');
    await waitFor(() => expect(screen.getByRole('checkbox', { name: 'admin: models.alpha.view' })).toBeDisabled());
    expect(screen.getByTestId('admin-roles-new-role')).toBeDisabled();
    expect(enabledWriteControls()).toEqual([]);

    // Tabs and search keep working; the standard tab's Apply is disabled too.
    await user.click(screen.getByRole('tab', { name: 'Standard Roles' }));
    expect(await screen.findByTestId('admin-roles-apply')).toBeDisabled();
    await user.type(screen.getByTestId('admin-roles-search'), 'alpha');
    expect(enabledWriteControls()).toEqual([]);
    expect(recorded.filter((entry) => entry.method !== 'GET')).toEqual([]);
  });

  it('keeps the controls enabled for a role that writes the access item', async () => {
    const user = userEvent.setup();
    grantAdminUiPermissions([
      'configuration.roles.permissions.view',
      'configuration.roles.permissions.edit',
      'configuration.roles.roles.create',
    ]);
    serveConsoleLevel('write', 'access');
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    expect(screen.getByRole('checkbox', { name: 'admin: models.alpha.view' })).toBeEnabled();
    expect(screen.getByTestId('admin-roles-new-role')).toBeEnabled();
    expect(screen.queryByTestId('admin-view-only')).toBeNull();
  });

  // The item is `write` for any of its write permissions, so an action with a
  // narrower one is disabled, not hidden, for a caller without it.
  it('disables New role for a writer without roles.create, and keeps the matrix editable', async () => {
    const user = userEvent.setup();
    serveConsoleLevel('write', 'access');
    renderAdminRoute(<AdminRoles />);

    await expandGroup(user, 'models.alpha');
    expect(screen.getByRole('checkbox', { name: 'admin: models.alpha.view' })).toBeEnabled();
    expect(screen.getByTestId('admin-roles-new-role')).toBeDisabled();
    expect(screen.queryByTestId('admin-view-only')).toBeNull();
  });

  it('filters by search and expands what it matched', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await screen.findByText('models.alpha');
    await user.type(screen.getByTestId('admin-roles-search'), 'roles.permissions');

    await waitFor(() => expect(screen.queryByText('models.alpha')).not.toBeInTheDocument());
    // The matched group is open, not merely listed.
    expect(screen.getByRole('checkbox', { name: 'admin: configuration.roles.permissions.view' })).toBeInTheDocument();
  });

  it('offers Apply to Projects on the standard tab only, and posts it', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await screen.findByText('models.alpha');
    expect(screen.queryByRole('button', { name: 'Apply to Projects' })).not.toBeInTheDocument();

    await user.click(screen.getByRole('tab', { name: 'Standard Roles' }));
    await user.click(await screen.findByRole('button', { name: 'Apply to Projects' }));

    await waitFor(() =>
      expect(recorded.some((entry) => entry.method === 'POST' && entry.url === 'administration')).toBe(true),
    );
    expect(await screen.findByText('Permissions synced to shared projects.')).toBeInTheDocument();

    // It is defined for administration/default alone, so it must not follow the
    // operator to a project tab.
    await user.click(screen.getByRole('tab', { name: 'Public Project' }));
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Apply to Projects' })).not.toBeInTheDocument(),
    );
  });

  it('states why the support tab is unavailable instead of calling it a failure', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminRoles />);

    await screen.findByText('models.alpha');
    await user.click(screen.getByRole('tab', { name: 'Support Project' }));

    const notice = await screen.findByTestId('admin-roles-unavailable');
    expect(notice).toHaveTextContent(SUPPORT_UNAVAILABLE);
    expect(screen.queryByText('Failed to load the permission matrix.')).not.toBeInTheDocument();
    // No editable matrix is offered for a tab that has no project behind it.
    expect(screen.queryByRole('table', { name: 'Permission matrix' })).not.toBeInTheDocument();
  });
});
