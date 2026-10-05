/**
 * The native client registry editor (Admin › Configuration › Native clients).
 *
 * What each case guards, and why a status-code test would miss it:
 *
 *  1. The page is REACHED: the Configuration page dispatches the server's
 *     `managed_surface: native_clients` to this editor, instead of the
 *     section's "registered on their own editor" refusal. Before this editor
 *     existed that refusal was the whole page.
 *  2. A FILE-layer client has no controls — nothing this page saves can change
 *     the deployment's file.
 *  3. Every write's BODY is inspected: a PUT that dropped `enabled` would
 *     re-enable a disabled client on rename, and the toggle must send the
 *     row's other fields unchanged.
 *  4. Disabling and removing confirm first and name the live-device count,
 *     because both revoke every device of the client.
 *  5. A 422 places each server reason beside the redirect URI it refused.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminConfiguration } from './Configuration';
import { AdminNativeClientsEditor } from './AdminNativeClientsEditor';
import { renderAdminRoute } from './__tests__/testRouter';

const CLIENTS = [
  {
    client_id: 'com.example.desktop',
    display_name: 'Example Desktop',
    redirect_uris: ['http://127.0.0.1/callback'],
    enabled: true,
    min_client_version: '1.2.0',
    source: 'db',
    overridden_file: false,
    active_devices: 3,
  },
  {
    client_id: 'com.example.mobile',
    display_name: 'Example Mobile',
    redirect_uris: ['com.example.mobile:/oauth/callback'],
    enabled: true,
    min_client_version: '',
    source: 'file',
    overridden_file: false,
    active_devices: 5,
  },
];

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

function useHandlers(options: { saveStatus?: number; saveBody?: Record<string, unknown>; listStatus?: number } = {}): void {
  server.use(
    http.get('*/admin/native_clients/administration', () => {
      if (options.listStatus !== undefined) {
        return HttpResponse.json({ error: 'not_found' }, { status: options.listStatus });
      }
      return HttpResponse.json({ rows: CLIENTS });
    }),
    http.put('*/admin/native_clients/administration/:clientId', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      if (options.saveStatus !== undefined) {
        return HttpResponse.json(options.saveBody, { status: options.saveStatus });
      }
      return HttpResponse.json({ client_id: 'x', revoked_devices: 0 });
    }),
    http.delete('*/admin/native_clients/administration/:clientId', ({ request }) => {
      recorded.push({ method: 'DELETE', url: request.url, body: null });
      return HttpResponse.json({ client_id: 'x', revoked_devices: 3 });
    }),
  );
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function rowOf(clientId: string): HTMLElement {
  return screen.getByTestId(`native-client-row-${clientId}`);
}

describe('AdminConfiguration → Native clients (composition)', () => {
  it('renders this editor for the server-declared managed surface, not the refusal', async () => {
    useHandlers();
    server.use(
      http.get('*/admin/plugin_config_schemas/administration', () =>
        HttpResponse.json({
          sections: [
            {
              id: 'native_clients',
              title: 'Native clients',
              managed_surface: 'native_clients',
              unavailable_reason: 'native clients are registered on their own editor',
              fields: [],
            },
          ],
        }),
      ),
    );

    renderAdminRoute(<AdminConfiguration />);

    expect(await screen.findByTestId('admin-native-clients-editor')).toBeVisible();
    expect(await screen.findByText('Example Desktop')).toBeVisible();
    expect(screen.queryByTestId('admin-configuration-unavailable')).not.toBeInTheDocument();
  });
});

describe('AdminNativeClientsEditor', () => {
  it('lists each client with its device count, and leaves file entries read-only', async () => {
    useHandlers();
    renderAdminRoute(<AdminNativeClientsEditor />);

    await screen.findByText('Example Desktop');
    expect(screen.getByTestId('native-client-devices-com.example.desktop')).toHaveTextContent('3');
    expect(screen.getByTestId('native-client-devices-com.example.mobile')).toHaveTextContent('5');

    const fileRow = rowOf('com.example.mobile');
    expect(within(fileRow).getByText('Read-only: set in the NATIVE_CLIENTS_PATH file.')).toBeVisible();
    expect(within(fileRow).queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument();
    expect(within(fileRow).queryByRole('button', { name: 'Remove' })).not.toBeInTheDocument();
    expect(within(fileRow).getByRole('switch', { name: 'Enable Example Mobile' })).toBeDisabled();

    const dbRow = rowOf('com.example.desktop');
    expect(within(dbRow).getByRole('button', { name: 'Edit' })).toBeEnabled();
  });

  it('registers a client with every field in the body', async () => {
    useHandlers();
    const user = userEvent.setup();
    renderAdminRoute(<AdminNativeClientsEditor />);
    await screen.findByText('Example Desktop');

    await user.click(screen.getByTestId('admin-native-clients-add'));
    await user.type(screen.getByTestId('native-client-id'), 'com.acme.app');
    await user.type(screen.getByTestId('native-client-display-name'), 'Acme');
    await user.type(
      screen.getByTestId('native-client-redirect-uris'),
      'com.acme.app:/oauth/callback{enter}  {enter}http://127.0.0.1/cb',
    );
    await user.click(screen.getByTestId('native-client-save'));

    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.url).toMatch(/\/admin\/native_clients\/administration\/com\.acme\.app$/);
    expect(recorded[0]!.body).toEqual({
      display_name: 'Acme',
      redirect_uris: ['com.acme.app:/oauth/callback', 'http://127.0.0.1/cb'],
      enabled: true,
      min_client_version: '',
    });
    await waitFor(() => expect(screen.queryByTestId('native-client-dialog')).not.toBeInTheDocument());
  });

  it('keeps the dialog open and places each 422 reason beside the URI it refused', async () => {
    useHandlers({
      saveStatus: 422,
      saveBody: {
        error: 'invalid_native_client',
        reasons: { 'redirect_uris[1]': 'https is not allowed: use a private-use scheme or loopback' },
      },
    });
    const user = userEvent.setup();
    renderAdminRoute(<AdminNativeClientsEditor />);
    await screen.findByText('Example Desktop');

    await user.click(screen.getByTestId('admin-native-clients-add'));
    await user.type(screen.getByTestId('native-client-id'), 'com.acme.app');
    await user.type(screen.getByTestId('native-client-display-name'), 'Acme');
    await user.type(
      screen.getByTestId('native-client-redirect-uris'),
      'com.acme.app:/cb{enter}https://acme.example/cb',
    );
    await user.click(screen.getByTestId('native-client-save'));

    expect(await screen.findByTestId('native-client-redirect-uri-error')).toHaveTextContent(
      'https://acme.example/cb: https is not allowed: use a private-use scheme or loopback',
    );
    expect(screen.getByTestId('native-client-dialog')).toBeVisible();
  });

  it('shows the server sentence when DEPLOYMENT_URL is missing (409)', async () => {
    useHandlers({
      saveStatus: 409,
      saveBody: { error: 'public_origin_required', message: 'set DEPLOYMENT_URL first' },
    });
    const user = userEvent.setup();
    renderAdminRoute(<AdminNativeClientsEditor />);
    await screen.findByText('Example Desktop');

    await user.click(within(rowOf('com.example.desktop')).getByRole('button', { name: 'Edit' }));
    expect(screen.getByTestId('native-client-id')).toBeDisabled();
    await user.click(screen.getByTestId('native-client-save'));

    expect(await screen.findByTestId('native-client-dialog-error')).toHaveTextContent('set DEPLOYMENT_URL first');
  });

  it('confirms before disabling, names the device count, and sends the row unchanged but disabled', async () => {
    useHandlers();
    const user = userEvent.setup();
    renderAdminRoute(<AdminNativeClientsEditor />);
    await screen.findByText('Example Desktop');

    await user.click(within(rowOf('com.example.desktop')).getByRole('switch', { name: 'Enable Example Desktop' }));
    const dialog = await screen.findByTestId('native-client-confirm-dialog');
    expect(dialog).toHaveTextContent('signs out all 3 of its signed-in devices');
    expect(recorded).toHaveLength(0);

    await user.click(within(dialog).getByTestId('native-client-confirm'));
    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.body).toEqual({
      display_name: 'Example Desktop',
      redirect_uris: ['http://127.0.0.1/callback'],
      enabled: false,
      min_client_version: '1.2.0',
    });
  });

  it('confirms before removing, then deletes and reports the devices signed out', async () => {
    useHandlers();
    const user = userEvent.setup();
    renderAdminRoute(<AdminNativeClientsEditor />);
    await screen.findByText('Example Desktop');

    await user.click(within(rowOf('com.example.desktop')).getByRole('button', { name: 'Remove' }));
    const dialog = await screen.findByTestId('native-client-confirm-dialog');
    await user.click(within(dialog).getByTestId('native-client-confirm'));

    await waitFor(() => expect(recorded.map((entry) => entry.method)).toEqual(['DELETE']));
    expect(recorded[0]!.url).toMatch(/\/admin\/native_clients\/administration\/com\.example\.desktop$/);
    expect(await screen.findByTestId('admin-native-clients-notice')).toHaveTextContent(
      'Saved. 3 signed-in devices were signed out.',
    );
  });

  it('says when the deployment serves no native sign-in (404)', async () => {
    useHandlers({ listStatus: 404 });
    renderAdminRoute(<AdminNativeClientsEditor />);

    expect(await screen.findByTestId('admin-native-clients-error')).toHaveTextContent(
      'This deployment does not serve native sign-in',
    );
  });
  // Regression: the PUT is an UPSERT. "Register" with an id a database row
  // already holds silently replaced that client's name and redirect URIs —
  // every installed copy of the real app then failed its next sign-in.
  it('refuses to register an id a database client already holds, without a PUT', async () => {
    useHandlers();
    const user = userEvent.setup();
    renderAdminRoute(<AdminNativeClientsEditor />);
    await screen.findByText('Example Desktop');

    await user.click(screen.getByTestId('admin-native-clients-add'));
    await user.type(screen.getByTestId('native-client-id'), 'com.example.desktop');
    await user.type(screen.getByTestId('native-client-display-name'), 'Impostor');
    await user.type(screen.getByTestId('native-client-redirect-uris'), 'com.example.desktop:/other');
    await user.click(screen.getByTestId('native-client-save'));

    expect(await screen.findByText('A client with this ID is already registered. Edit it instead.')).toBeVisible();
    expect(screen.getByTestId('native-client-dialog')).toBeVisible();
    expect(recorded).toHaveLength(0);
  });

  it('still lets Register override a FILE entry with the same id (a deliberate shadow)', async () => {
    useHandlers();
    const user = userEvent.setup();
    renderAdminRoute(<AdminNativeClientsEditor />);
    await screen.findByText('Example Desktop');

    await user.click(screen.getByTestId('admin-native-clients-add'));
    await user.type(screen.getByTestId('native-client-id'), 'com.example.mobile');
    await user.type(screen.getByTestId('native-client-display-name'), 'Mobile override');
    await user.type(screen.getByTestId('native-client-redirect-uris'), 'com.example.mobile:/oauth/callback');
    await user.click(screen.getByTestId('native-client-save'));

    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.url).toMatch(/\/administration\/com\.example\.mobile$/);
  });

  // Regression: a blank id PUT `/administration/`, which no route matches —
  // the operator got "Failed to save" and no hint which field was wrong.
  it('refuses a blank client id beside the field, without a PUT', async () => {
    useHandlers();
    const user = userEvent.setup();
    renderAdminRoute(<AdminNativeClientsEditor />);
    await screen.findByText('Example Desktop');

    await user.click(screen.getByTestId('admin-native-clients-add'));
    await user.type(screen.getByTestId('native-client-display-name'), 'Acme');
    await user.type(screen.getByTestId('native-client-redirect-uris'), 'com.acme.app:/cb');
    await user.click(screen.getByTestId('native-client-save'));

    expect(await screen.findByText('Enter the client ID the app sends.')).toBeVisible();
    expect(recorded).toHaveLength(0);
  });
});
