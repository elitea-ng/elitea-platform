/**
 * Role create / rename / delete on Admin › Roles (gap G9).
 *
 * A separate file from `Roles.test.tsx` because it is about a different
 * surface: that one covers the permission MATRIX — the cells — and this one
 * covers the role DEFINITIONS, which change the columns.
 *
 * Each test asserts one of:
 *
 *   - the REQUEST the control produced, including the body shape the Go handler
 *     and pylon before it read (`{name}` / `{name, new_name}`);
 *   - that the new column actually APPEARS afterwards. That is the assertion a
 *     seed-once draft breaks: the page keeps its draft across a background
 *     refetch on purpose (#191), so a create whose refetch is not honoured
 *     answers 201 and changes nothing on screen;
 *   - that a control the server would refuse is not offered at all;
 *   - that a refusal is shown in the operator's own words, in the dialog that
 *     produced it, and does not close it.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminRoles } from './Roles';
import { renderAdminRoute } from './__tests__/testRouter';

interface MatrixRow {
  readonly name: string;
  readonly [role: string]: string | boolean;
}

interface RecordedRequest {
  readonly method: string;
  readonly scope: string;
  readonly mode: string;
  readonly url: string;
  readonly body: unknown;
}

/** The three built-ins the server refuses to rename or delete, plus one that is not. */
const BASE_ROWS: MatrixRow[] = [
  { name: 'models.alpha.view', system: true, admin: true, editor: true, viewer: false },
  { name: 'models.alpha.edit', system: true, admin: true, editor: false, viewer: false },
];

/** The same matrix after `security_reviewer` was created — one more column. */
const ROWS_WITH_NEW_ROLE: MatrixRow[] = BASE_ROWS.map((row) => ({
  ...row,
  security_reviewer: false,
}));

let recorded: RecordedRequest[] = [];
/** What the administration matrix answers next. Swapped by the create handler. */
let adminRows: MatrixRow[] = BASE_ROWS;
/** What the next role write answers, so a test can arm a refusal. */
let roleWriteResponse: (() => Response) | null = null;

function matrixBody(rows: MatrixRow[]): { rows: MatrixRow[]; total: number } {
  return { rows, total: rows.length };
}

function useRoleDefinitionHandlers(): void {
  server.use(
    http.get('*/admin/permissions/administration/administration', () =>
      HttpResponse.json(matrixBody(adminRows)),
    ),
    http.get('*/admin/permissions/:scope/default', () => HttpResponse.json(matrixBody(BASE_ROWS))),
    http.post('*/admin/roles/:scope/:mode', async ({ request, params }) => {
      recorded.push({
        method: 'POST',
        scope: String(params.scope),
        mode: String(params.mode),
        url: request.url,
        body: await request.json(),
      });
      if (roleWriteResponse) return roleWriteResponse();
      // The server's create is what makes the column exist, so the fixture's
      // matrix has to change with it. Without this the "new column appears"
      // assertion could pass against a page that never refetched.
      adminRows = ROWS_WITH_NEW_ROLE;
      return HttpResponse.json({ ok: true, name: 'security_reviewer' }, { status: 201 });
    }),
    http.put('*/admin/roles/:scope/:mode', async ({ request, params }) => {
      recorded.push({
        method: 'PUT',
        scope: String(params.scope),
        mode: String(params.mode),
        url: request.url,
        body: await request.json(),
      });
      if (roleWriteResponse) return roleWriteResponse();
      return HttpResponse.json({ ok: true, projects_renamed: 0 });
    }),
    http.delete('*/admin/roles/:scope/:mode', async ({ request, params }) => {
      recorded.push({
        method: 'DELETE',
        scope: String(params.scope),
        mode: String(params.mode),
        url: request.url,
        body: await request.text(),
      });
      if (roleWriteResponse) return roleWriteResponse();
      adminRows = BASE_ROWS;
      return HttpResponse.json({ ok: true, project_roles_removed: 0 });
    }),
  );
}

function grantAdminUiPermissions(permissions: string[]): void {
  window.admin_ui_config = { permissions, vite_server_url: '/api/v2' };
}

const ALL_ROLE_PERMISSIONS = [
  'configuration.roles.permissions.view',
  'configuration.roles.permissions.edit',
  'configuration.roles.roles.create',
  'configuration.roles.roles.edit',
  'configuration.roles.roles.delete',
];

function lastWrite(method: string): RecordedRequest | undefined {
  return [...recorded].reverse().find((entry) => entry.method === method);
}

beforeEach(() => {
  recorded = [];
  adminRows = BASE_ROWS;
  roleWriteResponse = null;
  configureGeneratedClient({ baseUrl: '/api/v2' });
  grantAdminUiPermissions(ALL_ROLE_PERMISSIONS);
  useRoleDefinitionHandlers();
});

afterEach(() => {
  resetGeneratedClient();
  delete window.admin_ui_config;
});

describe('Admin › Roles › role definitions', () => {
  it('creates a role and shows its new column', async () => {
    const user = userEvent.setup({ delay: null });
    renderAdminRoute(<AdminRoles />);

    const table = await screen.findByRole('table', { name: 'Permission matrix' });
    expect(
      within(table).queryByRole('columnheader', { name: 'security reviewer' }),
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'New role' }));
    await user.click(await screen.findByRole('textbox', { name: 'Role name' }));
    await user.paste('security_reviewer');
    await user.click(screen.getByRole('button', { name: 'Create role' }));

    await waitFor(() => expect(lastWrite('POST')).toBeDefined());
    const sent = lastWrite('POST');
    // The pair the page's tab is keyed on, and pylon's body shape.
    expect(sent?.scope).toBe('administration');
    expect(sent?.mode).toBe('administration');
    expect(sent?.body).toEqual({ name: 'security_reviewer' });

    // THE ASSERTION THAT MATTERS. The draft is deliberately seeded once per tab
    // so a background refetch cannot discard an edit in progress; a create that
    // did not re-seed it would answer 201 and leave the screen unchanged.
    await waitFor(() =>
      expect(
        within(screen.getByRole('table', { name: 'Permission matrix' })).getByRole('columnheader', {
          name: 'security reviewer',
        }),
      ).toBeInTheDocument(),
    );
    expect(await screen.findByText(/Role created/)).toBeInTheDocument();
  });

  it('will not submit a name the server would refuse', async () => {
    const user = userEvent.setup({ delay: null });
    renderAdminRoute(<AdminRoles />);

    await screen.findByRole('table', { name: 'Permission matrix' });
    await user.click(screen.getByRole('button', { name: 'New role' }));
    const field = await screen.findByRole('textbox', { name: 'Role name' });
    const submit = screen.getByRole('button', { name: 'Create role' });

    // Empty: nothing to submit yet, and no error shouted at an untouched field.
    expect(submit).toBeDisabled();

    // `name` is the key the matrix puts each row's PERMISSION under, so a role
    // by that name would hide every permission name.
    await user.click(field);
    await user.paste('name');
    expect(await screen.findByText(/is reserved/)).toBeInTheDocument();
    expect(submit).toBeDisabled();

    await user.clear(field);
    await user.paste('a role');
    expect(await screen.findByText(/letters, digits, underscores or hyphens/)).toBeInTheDocument();
    expect(submit).toBeDisabled();

    // A role the tab already defines.
    await user.clear(field);
    await user.paste('admin');
    expect(await screen.findByText(/already exists/)).toBeInTheDocument();
    expect(submit).toBeDisabled();

    // …and a legal one arms it, so the assertions above are about the names and
    // not about a button that is always disabled.
    await user.clear(field);
    await user.paste('security_reviewer');
    await waitFor(() => expect(submit).toBeEnabled());
    expect(recorded).toHaveLength(0);
  });

  it('offers no rename or delete for a built-in role', async () => {
    const user = userEvent.setup({ delay: null });
    adminRows = ROWS_WITH_NEW_ROLE;
    renderAdminRoute(<AdminRoles />);

    await screen.findByRole('table', { name: 'Permission matrix' });
    // `roles_crud.go` refuses all five built-ins for both writes: the service
    // names them in its own SQL and Go, which a rename would not follow.
    for (const role of ['system', 'admin', 'editor', 'viewer']) {
      expect(screen.queryByRole('button', { name: `Rename role: ${role}` })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: `Delete role: ${role}` })).not.toBeInTheDocument();
    }
    // The deployment-defined one has both, so the loop above is about built-ins
    // and not about a page that renders no controls at all.
    expect(
      screen.getByRole('button', { name: 'Rename role: security_reviewer' }),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Delete role: security_reviewer' }));
    expect(await screen.findByRole('heading', { name: 'Delete role' })).toBeInTheDocument();
  });

  it('renames a role with pylon’s body shape', async () => {
    const user = userEvent.setup({ delay: null });
    adminRows = ROWS_WITH_NEW_ROLE;
    renderAdminRoute(<AdminRoles />);

    await screen.findByRole('table', { name: 'Permission matrix' });
    await user.click(screen.getByRole('button', { name: 'Rename role: security_reviewer' }));
    const field = await screen.findByRole('textbox', { name: 'Role name' });
    // The dialog opens on the current name, and submitting it unchanged is not
    // a rename — the control must not fire a no-op write.
    expect(field).toHaveValue('security_reviewer');
    expect(screen.getByRole('button', { name: 'Rename role' })).toBeDisabled();

    await user.clear(field);
    await user.paste('risk_reviewer');
    await user.click(screen.getByRole('button', { name: 'Rename role' }));

    await waitFor(() => expect(lastWrite('PUT')).toBeDefined());
    expect(lastWrite('PUT')?.body).toEqual({
      name: 'security_reviewer',
      new_name: 'risk_reviewer',
    });
    expect(await screen.findByText('Role renamed.')).toBeInTheDocument();
  });

  it('deletes only after the role name is typed back', async () => {
    const user = userEvent.setup({ delay: null });
    adminRows = ROWS_WITH_NEW_ROLE;
    renderAdminRoute(<AdminRoles />);

    await screen.findByRole('table', { name: 'Permission matrix' });
    await user.click(screen.getByRole('button', { name: 'Delete role: security_reviewer' }));
    const confirm = screen.getByRole('button', { name: 'Delete role' });
    expect(confirm).toBeDisabled();

    // A near miss does not arm it: four tabs show four role lists that share
    // names, so typing the name is what proves WHICH role is meant.
    const field = await screen.findByRole('textbox', {
      name: 'Type security_reviewer to confirm',
    });
    await user.click(field);
    await user.paste('security_review');
    expect(confirm).toBeDisabled();
    expect(recorded).toHaveLength(0);

    await user.paste('er');
    await waitFor(() => expect(confirm).toBeEnabled());
    await user.click(confirm);

    await waitFor(() => expect(lastWrite('DELETE')).toBeDefined());
    const sent = lastWrite('DELETE');
    // Body AND query: a DELETE body is legal but is dropped by enough
    // intermediaries that the server reads `?name=` as a fallback.
    expect(sent?.body).toBe(JSON.stringify({ name: 'security_reviewer' }));
    expect(sent?.url).toContain('name=security_reviewer');
    expect(await screen.findByText('Role deleted.')).toBeInTheDocument();
  });

  it('shows the server’s own refusal, with its count, and keeps the dialog open', async () => {
    const user = userEvent.setup({ delay: null });
    adminRows = ROWS_WITH_NEW_ROLE;
    const refusal =
      '"security_reviewer" is still assigned to 3 user(s) in the central default roles: remove the assignments before deleting the role';
    roleWriteResponse = () => HttpResponse.json({ error: refusal, members: 3 }, { status: 409 });
    renderAdminRoute(<AdminRoles />);

    await screen.findByRole('table', { name: 'Permission matrix' });
    await user.click(screen.getByRole('button', { name: 'Delete role: security_reviewer' }));
    await user.click(screen.getByRole('textbox', { name: 'Type security_reviewer to confirm' }));
    await user.paste('security_reviewer');
    await user.click(screen.getByRole('button', { name: 'Delete role' }));

    // The COUNT is the fact the operator acts on. A generic "Failed to delete
    // the role." would hide the only actionable part of the answer.
    const notice = await screen.findByTestId('admin-roles-delete-error');
    expect(notice).toHaveTextContent('3 user(s)');
    expect(screen.queryByText('Failed to delete the role.')).not.toBeInTheDocument();
    // The dialog stays open, so the operator can read it beside the control.
    expect(screen.getByRole('heading', { name: 'Delete role' })).toBeInTheDocument();
    // A refusal is never reported as a success.
    expect(screen.queryByText('Role deleted.')).not.toBeInTheDocument();

    // …and once it is dismissed the column is still there, because nothing was
    // deleted. The check runs AFTER the dialog closes: MUI marks the rest of
    // the app `aria-hidden` while a modal is open, so the table is not
    // reachable by role until then.
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(
      within(await screen.findByRole('table', { name: 'Permission matrix' })).getByRole(
        'columnheader',
        { name: 'security reviewer' },
      ),
    ).toBeInTheDocument();
  });

  it('offers no role-definition control without the permissions', async () => {
    grantAdminUiPermissions([
      'configuration.roles.permissions.view',
      'configuration.roles.permissions.edit',
    ]);
    adminRows = ROWS_WITH_NEW_ROLE;
    renderAdminRoute(<AdminRoles />);

    await screen.findByRole('table', { name: 'Permission matrix' });
    expect(screen.queryByRole('button', { name: 'New role' })).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: 'Rename role: security_reviewer' }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: 'Delete role: security_reviewer' }),
    ).not.toBeInTheDocument();
    // The matrix itself is still editable, so this is about the three role
    // permissions and not about a page that rendered read-only.
    expect(screen.getByRole('checkbox', { name: 'admin: models.alpha' })).toBeEnabled();
  });
});
