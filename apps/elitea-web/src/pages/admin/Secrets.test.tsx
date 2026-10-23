/**
 * Rendering + write-path guard for `pages/admin/Secrets.tsx` (unit A14).
 *
 * The properties asserted here are the ones this page's specific hazards make
 * worth asserting:
 *
 *  1. **Every request goes to the `administration` mode.** The whole reason this
 *     surface needed a new Go handler is that the project handler keys on the
 *     `{projectID}` segment while admin_ui sends the placeholder `0` — so a
 *     client that fell back to `default` would silently edit project 0's vault.
 *     Every recorded URL is checked for the mode, not just for the path.
 *  2. **The bodies are the legacy ones.** Create sends `{secret}`; update sends
 *     the NESTED `{secret:{old_name, value}}`. A control that renders but sends
 *     the wrong shape is the #130/#180 class, and a status assertion cannot see
 *     it.
 *  3. **Plaintext appears only after the operator asks.** The listing carries a
 *     mask; the value must not be on screen until the reveal is clicked.
 *  4. **Internal secrets are read-only** — and shown, not hidden.
 *  5. **A refusal renders the SERVER's own sentence.** "The vault rows exist and
 *     will not open" and "there is no vault" are different answers with
 *     different fixes, and the handler is careful to distinguish them; a page
 *     that flattened both into "Failed to load" would waste that. Note the one
 *     refusal that CANNOT reach the page this way is a 403 — `shared/api/http.ts`
 *     escalates it into the re-auth flow, which discards the body. That is a
 *     shared-client policy, not this page's, and it is stated in
 *     `./api/adminSecretsApi` rather than papered over.
 *
 * No fixture value here is or resembles a credential.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminSecrets } from './Secrets';
import { renderAdminRoute } from './__tests__/testRouter';

/**
 * The listing body the Go handler returns: a bare array, every row masked.
 * `auth_token` is classified INTERNAL by name and is the one entry the reference
 * deployment's global vault actually holds.
 */
const LISTING = [
  { name: 'auth_token', secret: '******' },
  { name: 'shared_marker', secret: '******' },
];

const REVEALED = 'marker-one';

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

function useSecretHandlers(
  options: { listStatus?: number; listBody?: Record<string, string> } = {},
): void {
  server.use(
    http.get('*/secrets/secrets/administration/*', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      if (options.listStatus !== undefined) {
        return HttpResponse.json(options.listBody, { status: options.listStatus });
      }
      return HttpResponse.json(LISTING);
    }),
    http.get('*/secrets/secret/administration/*', ({ request }) => {
      recorded.push({ method: 'GET_ONE', url: request.url, body: null });
      return HttpResponse.json({ secret: REVEALED });
    }),
    http.post('*/secrets/secret/administration/*', async ({ request }) => {
      recorded.push({ method: 'POST', url: request.url, body: await request.json() });
      return HttpResponse.json({ message: 'Project secret was saved' });
    }),
    http.put('*/secrets/secret/administration/*', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return HttpResponse.json({ message: 'Project secret was updated' });
    }),
    http.delete('*/secrets/secret/administration/*', ({ request }) => {
      recorded.push({ method: 'DELETE', url: request.url, body: null });
      return new HttpResponse(null, { status: 204 });
    }),
  );
}

/** The permission list the Go adminui handler injects for a valid session. */
function grantAdminUiPermissions(permissions: string[]): void {
  window.admin_ui_config = { permissions, vite_server_url: '/api/v2' };
}

const ALL_SECRET_PERMISSIONS = [
  'configuration.secrets.secret.list',
  'configuration.secrets.secret.create',
  'configuration.secrets.secret.edit',
  'configuration.secrets.secret.delete',
];

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method !== 'GET' && entry.method !== 'GET_ONE');
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  grantAdminUiPermissions(ALL_SECRET_PERMISSIONS);
  useSecretHandlers();
});

afterEach(() => {
  resetGeneratedClient();
  delete window.admin_ui_config;
});

async function findUserSecretRow(): Promise<HTMLElement> {
  // Wait for the CELL, not just the grid: the grid element exists before the
  // listing resolves, so finding it first would search an empty body.
  await screen.findByText('shared_marker');
  const grid = screen.getByRole('grid');
  const row = within(grid)
    .getAllByRole('row')
    .find((candidate) => candidate.getAttribute('data-id') === 'shared_marker');
  if (!row) throw new Error('no grid row for shared_marker');
  return row;
}

describe('Admin › Secrets', () => {
  it('splits the vault into User and Internal tabs by NAME and lists only user secrets first', async () => {
    renderAdminRoute(<AdminSecrets />);

    expect(await screen.findByText('shared_marker')).toBeInTheDocument();
    // `auth_token` is internal, so it is NOT on the first tab…
    expect(screen.queryByText('auth_token')).not.toBeInTheDocument();
    // …and both tabs are labelled with the count they hold.
    expect(screen.getByRole('tab', { name: 'User Secrets (1)' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Internal (1)' })).toBeInTheDocument();
  });

  it('addresses the administration mode, never the project vault', async () => {
    renderAdminRoute(<AdminSecrets />);
    await screen.findByText('shared_marker');

    expect(recorded.length).toBeGreaterThan(0);
    for (const entry of recorded) {
      expect(entry.url).toContain('/administration/');
      expect(entry.url).not.toContain('/default/');
    }
  });

  it('masks values until the operator reveals one, then fetches the plaintext', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSecrets />);
    const row = await findUserSecretRow();

    // Nothing plaintext on screen from the listing alone.
    expect(screen.queryByText(REVEALED)).not.toBeInTheDocument();

    await user.click(within(row).getByRole('button', { name: /show|reveal/i }));

    expect(await screen.findByText(REVEALED)).toBeInTheDocument();
    const reveals = recorded.filter((entry) => entry.method === 'GET_ONE');
    expect(reveals).toHaveLength(1);
    expect(reveals[0]?.url).toContain('/secrets/secret/administration/0/shared_marker');
  });

  it('creates a secret with the legacy {secret} body', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSecrets />);
    await screen.findByText('shared_marker');

    await user.click(screen.getByRole('button', { name: 'Create secret' }));
    const dialog = await screen.findByTestId('admin-secret-dialog');
    await user.type(within(dialog).getByLabelText(/Secret name/), 'new_marker');
    await user.type(within(dialog).getByLabelText(/Secret value/), 'marker-two');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    const write = writes()[0];
    expect(write?.method).toBe('POST');
    expect(write?.url).toContain('/secrets/secret/administration/0/new_marker');
    expect(write?.body).toEqual({ secret: 'marker-two' });
  });

  it('rejects a name the server would reject, without sending anything', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSecrets />);
    await screen.findByText('shared_marker');

    await user.click(screen.getByRole('button', { name: 'Create secret' }));
    const dialog = await screen.findByTestId('admin-secret-dialog');
    await user.type(within(dialog).getByLabelText(/Secret name/), 'not.a.valid.name');
    await user.type(within(dialog).getByLabelText(/Secret value/), 'marker-two');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));

    expect(
      await within(dialog).findByText('Name must contain only letters, digits and underscores.'),
    ).toBeInTheDocument();
    expect(writes()).toHaveLength(0);
  });

  it('refuses a duplicate name locally, and the duplicate check sees the INTERNAL names too', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSecrets />);
    await screen.findByText('shared_marker');

    await user.click(screen.getByRole('button', { name: 'Create secret' }));
    const dialog = await screen.findByTestId('admin-secret-dialog');
    // `auth_token` is not on the visible tab, but it IS in the vault — a
    // duplicate check scoped to the rendered rows would let this through and
    // the server would then have to refuse it.
    await user.type(within(dialog).getByLabelText(/Secret name/), 'auth_token');
    await user.type(within(dialog).getByLabelText(/Secret value/), 'marker-two');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));

    expect(
      await within(dialog).findByText('A secret with that name already exists.'),
    ).toBeInTheDocument();
    expect(writes()).toHaveLength(0);
  });

  it('updates a value with the legacy NESTED {secret:{old_name,value}} body', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSecrets />);
    const row = await findUserSecretRow();

    await user.click(within(row).getByRole('button', { name: 'Edit: shared_marker' }));
    const dialog = await screen.findByTestId('admin-secret-dialog');
    // The name is fixed on edit — the reference disables it, and a rename here
    // would be a different write.
    expect(within(dialog).getByLabelText(/Secret name/)).toBeDisabled();
    await user.type(within(dialog).getByLabelText(/New value/), 'marker-three');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    const write = writes()[0];
    expect(write?.method).toBe('PUT');
    expect(write?.url).toContain('/secrets/secret/administration/0/shared_marker');
    expect(write?.body).toEqual({ secret: { old_name: 'shared_marker', value: 'marker-three' } });
  });

  it('requires the name to be retyped before it will delete', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSecrets />);
    const row = await findUserSecretRow();

    await user.click(within(row).getByRole('button', { name: 'Delete: shared_marker' }));
    const modal = await screen.findByTestId('admin-secrets-delete-modal');
    const confirm = within(modal).getByRole('button', { name: /confirm|delete/i });
    expect(confirm).toBeDisabled();
    expect(writes()).toHaveLength(0);

    await user.type(within(modal).getByRole('textbox'), 'shared_marker');
    await user.click(confirm);

    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0]?.method).toBe('DELETE');
    expect(writes()[0]?.url).toContain('/secrets/secret/administration/0/shared_marker');
  });

  it('shows internal secrets read-only rather than hiding them', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSecrets />);
    await screen.findByText('shared_marker');

    await user.click(screen.getByRole('tab', { name: 'Internal (1)' }));

    expect(await screen.findByText('auth_token')).toBeInTheDocument();
    expect(screen.getByTestId('admin-secrets-internal-notice')).toBeInTheDocument();
    // No edit and no delete on this tab, for anyone.
    expect(screen.queryByRole('button', { name: 'Edit: auth_token' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Delete: auth_token' })).not.toBeInTheDocument();
    // …and Create is offered on the user tab only.
    expect(screen.queryByRole('button', { name: 'Create secret' })).not.toBeInTheDocument();
  });

  it('renders the server’s own reason when the vault cannot be opened', async () => {
    recorded = [];
    server.resetHandlers();
    // The handler distinguishes "the vault rows exist and will not open" from
    // "there is no vault" on purpose: reporting the second here would invite a
    // write that replaced the first. The page must not flatten them back
    // together into "Failed to load".
    useSecretHandlers({ listStatus: 500, listBody: { message: 'global vault is unreadable' } });

    renderAdminRoute(<AdminSecrets />);

    const banner = await screen.findByTestId('admin-secrets-unavailable');
    expect(banner).toHaveTextContent('global vault is unreadable');
    expect(banner).not.toHaveTextContent('Failed to load the global secrets.');
  });

  it('renders the server’s refusal in the dialog when a create is rejected', async () => {
    const user = userEvent.setup();
    server.use(
      http.post('*/secrets/secret/administration/*', async ({ request }) => {
        recorded.push({ method: 'POST', url: request.url, body: await request.json() });
        return HttpResponse.json({ message: 'Secret "new_marker" already exists' }, { status: 400 });
      }),
    );
    renderAdminRoute(<AdminSecrets />);
    await screen.findByText('shared_marker');

    await user.click(screen.getByRole('button', { name: 'Create secret' }));
    const dialog = await screen.findByTestId('admin-secret-dialog');
    await user.type(within(dialog).getByLabelText(/Secret name/), 'new_marker');
    await user.type(within(dialog).getByLabelText(/Secret value/), 'marker-two');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));

    expect(await within(dialog).findByText('Secret "new_marker" already exists')).toBeInTheDocument();
    // The dialog stays open, holding what was typed, rather than closing on a
    // refusal and reporting success.
    expect(within(dialog).getByLabelText(/Secret name/)).toHaveValue('new_marker');
  });

  it('renders no write controls when the config advertises none, and still lists', async () => {
    grantAdminUiPermissions(['configuration.secrets.secret.list']);
    renderAdminRoute(<AdminSecrets />);

    expect(await screen.findByText('shared_marker')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Create secret' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Edit: shared_marker' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Delete: shared_marker' })).not.toBeInTheDocument();
  });
});
