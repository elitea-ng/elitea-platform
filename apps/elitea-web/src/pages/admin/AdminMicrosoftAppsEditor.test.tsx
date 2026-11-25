/**
 * The Microsoft Default app registry editor (Admin › Configuration › Microsoft apps).
 *
 * What each case guards, and why a status-code test would miss it:
 *
 *  1. The page is REACHED through the server-declared `managed_surface`.
 *  2. With an empty registry an Entra administrator still reads every redirect URI and
 *     permission to configure, and the credential forms are not blocked (the message
 *     says only Custom app is offered).
 *  3. The client secret is WRITE-ONLY: no screen shows it, an edit starts with the
 *     field empty, and an untouched field sends no `secret` key (an empty string
 *     would clear the sealed one).
 *  4. A refusal names a field and lands beside it with the dialog still open.
 *  5. The row switch, Test and the Custom app switch call their own routes.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminConfiguration } from './Configuration';
import { AdminMicrosoftAppsEditor } from './AdminMicrosoftAppsEditor';
import { renderAdminRoute } from './__tests__/testRouter';
import { enabledWriteControls, findViewOnlyNotice, serveConsoleLevel } from './__tests__/viewOnly';
import { testTimeout } from '@/test/timeouts';

const SECRET = 's3cr3t~value.from.Entra';
const APP_ID = '11111111-1111-4111-8111-111111111111';
const CLIENT_ID = '7f8d0c1e-0000-4000-8000-000000000001';
const REDIRECT = 'https://elitea.example.com/api/v2/microsoft_login:callback';

const TYPES = [
  {
    type: 'outlook',
    api: 'Microsoft Graph',
    offered: ['Mail.Read', 'Mail.ReadWrite', 'Mail.Send'].map((name) => ({ name, admin_consent: false })),
    default: ['Mail.Read'],
  },
  {
    type: 'teams',
    api: 'Microsoft Graph',
    offered: [
      { name: 'Chat.Read', admin_consent: false },
      { name: 'ChannelMessage.Read.All', admin_consent: true },
    ],
    default: ['Chat.Read'],
  },
  { type: 'ms_graph', api: 'Microsoft Graph', offered: [], default: ['User.Read'] },
  { type: 'dataverse', api: 'Dynamics CRM', offered: [], default: ['user_impersonation'] },
  { type: 'sharepoint', api: 'Microsoft Graph', offered: [], default: ['Sites.Read.All'] },
];

const CHECKLIST = {
  redirect_uri: REDIRECT,
  account_types: 'multi',
  permissions: [
    { api: 'Microsoft Graph', permissions: [{ name: 'openid', admin_consent: false }, { name: 'Mail.Read', admin_consent: false }] },
  ],
  admin_consent_url: `https://login.microsoftonline.com/{tenant-id}/adminconsent?client_id=${CLIENT_ID}`,
  needs_admin_consent: false,
  secret_expiry: 'expiring',
  secret_days_left: 12,
};

const APP = {
  id: APP_ID,
  name: 'Company Microsoft',
  description: '',
  provider: 'entra',
  client_id: CLIENT_ID,
  authority: 'https://login.microsoftonline.com/organizations',
  secret_set: true,
  secret_expires_at: '2026-11-30T00:00:00Z',
  credential_types: ['outlook'],
  scope_ceiling: {},
  effective_ceiling: { outlook: ['Mail.Read'] },
  auto_refresh_token: true,
  project_scope: 'all',
  project_ids: [],
  state: 'active',
  state_reason: '',
  managed_by: 'helm',
  created_at: '2026-11-18T00:00:00Z',
  updated_at: '2026-11-18T00:00:00Z',
  revision: 1,
  checklist: CHECKLIST,
};

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

interface Options {
  readonly apps?: readonly unknown[];
  readonly customApps?: boolean;
  readonly saveStatus?: number;
  readonly saveBody?: Record<string, unknown>;
  readonly listStatus?: number;
}

function useHandlers(options: Options = {}): void {
  const record = async (request: Request): Promise<void> => {
    let body: unknown = null;
    try {
      body = await request.json();
    } catch {
      body = null;
    }
    recorded.push({ method: request.method, url: request.url, body });
  };
  server.use(
    http.get('*/admin/microsoft_oauth_apps', () => {
      if (options.listStatus !== undefined) {
        return HttpResponse.json({ error: 'the Microsoft app registry is not available on this deployment' }, { status: options.listStatus });
      }
      const apps = options.apps ?? [];
      return HttpResponse.json({
        apps,
        total: apps.length,
        settings: { custom_apps: options.customApps ?? true },
        checklist: { redirect_uri: REDIRECT, types: TYPES },
      });
    }),
    http.post('*/admin/microsoft_oauth_apps', async ({ request }) => {
      await record(request);
      if (options.saveStatus !== undefined) return HttpResponse.json(options.saveBody, { status: options.saveStatus });
      return HttpResponse.json(APP, { status: 201 });
    }),
    http.put('*/admin/microsoft_oauth_apps/settings', async ({ request }) => {
      await record(request);
      return HttpResponse.json({ custom_apps: false });
    }),
    http.put('*/admin/microsoft_oauth_apps/:id', async ({ request }) => {
      await record(request);
      return HttpResponse.json(APP);
    }),
    http.post('*/admin/microsoft_oauth_apps/:id/disable', async ({ request }) => {
      await record(request);
      return HttpResponse.json({ ...APP, state: 'disabled' });
    }),
    http.post('*/admin/microsoft_oauth_apps/:id/enable', async ({ request }) => {
      await record(request);
      return HttpResponse.json(APP);
    }),
    http.post('*/admin/microsoft_oauth_apps/:id/test', async ({ request }) => {
      await record(request);
      return HttpResponse.json({
        ok: false,
        checks: [
          { id: 'client_id', ok: true },
          { id: 'authority', ok: false, detail: 'unreachable' },
          { id: 'secret_expiry', ok: true, warn: true, detail: 'expiring' },
        ],
      });
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

async function fillNewApp(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(await screen.findByTestId('admin-microsoft-apps-add'));
  await user.type(screen.getByTestId('microsoft-app-name'), 'Company Microsoft');
  await user.type(screen.getByTestId('microsoft-app-client-id'), CLIENT_ID);
  await user.click(screen.getByRole('checkbox', { name: 'Outlook' }));
  await user.type(screen.getByTestId('microsoft-app-secret'), SECRET);
}

describe('AdminConfiguration → Microsoft apps (composition)', () => {
  it('renders this editor for the server-declared managed surface, not the refusal', async () => {
    useHandlers();
    server.use(
      http.get('*/admin/plugin_config_schemas/administration', () =>
        HttpResponse.json({
          sections: [
            {
              id: 'microsoft_apps',
              title: 'Microsoft apps',
              managed_surface: 'microsoft_apps',
              unavailable_reason: 'Microsoft apps are registered on their own editor',
              fields: [],
            },
          ],
        }),
      ),
    );

    renderAdminRoute(<AdminConfiguration />);

    expect(await screen.findByTestId('admin-microsoft-apps-editor')).toBeVisible();
    expect(screen.queryByTestId('admin-configuration-unavailable')).not.toBeInTheDocument();
  });
});

describe('AdminMicrosoftAppsEditor', { timeout: testTimeout(20_000) }, () => {
  it('prints every redirect URI and permission while the registry is empty, and says only Custom app is offered', async () => {
    useHandlers();
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    const checklist = await screen.findByTestId('microsoft-deployment-checklist');
    expect(within(checklist).getByTestId('microsoft-app-redirect-uri')).toHaveValue(REDIRECT);
    // Every credential type's permissions are listed, with the one that needs consent marked.
    expect(within(checklist).getByTestId('microsoft-type-teams')).toHaveTextContent('ChannelMessage.Read.All');
    expect(within(checklist).getByTestId('microsoft-type-dataverse')).toHaveTextContent('user_impersonation');
    expect(within(checklist).getByTestId('microsoft-type-outlook')).toHaveTextContent('Mail.ReadWrite');
    expect(await screen.findByTestId('admin-microsoft-apps-empty')).toHaveTextContent('only Custom app');
    expect(screen.queryByTestId('admin-microsoft-apps-table')).not.toBeInTheDocument();
    expect(screen.getByRole('switch', { name: /Allow Custom app/ })).toBeChecked();
  });

  it('warns that Microsoft refuses a plain http redirect URI', async () => {
    useHandlers();
    server.use(
      http.get('*/admin/microsoft_oauth_apps', () =>
        HttpResponse.json({
          apps: [],
          total: 0,
          settings: { custom_apps: true },
          checklist: { redirect_uri: 'http://elitea.example.com/api/v2/microsoft_login:callback', redirect_issue: 'https_required', types: TYPES },
        }),
      ),
    );
    renderAdminRoute(<AdminMicrosoftAppsEditor />);
    expect(await screen.findByTestId('microsoft-app-redirect-issue')).toHaveTextContent('DEPLOYMENT_URL');
  });

  it('lists an app without any secret, and says one is stored', async () => {
    useHandlers({ apps: [APP] });
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    const row = await screen.findByTestId(`microsoft-app-row-${APP_ID}`);
    expect(within(row).getByText('Company Microsoft')).toBeVisible();
    expect(within(row).getByText(CLIENT_ID)).toBeVisible();
    expect(within(row).getByText('Stored, hidden')).toBeVisible();
    expect(within(row).getByText('Expires in 12 days')).toBeVisible();
    expect(within(row).getByText('Seeded from Helm')).toBeVisible();
    expect(within(row).getByText('Active')).toBeVisible();
    expect(document.body.textContent).not.toContain(SECRET);
  });

  it('creates an app: the secret rides in that one request and is gone from the page after', async () => {
    useHandlers();
    const user = userEvent.setup();
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    await fillNewApp(user);
    await user.click(screen.getByTestId('microsoft-app-save'));

    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.method).toBe('POST');
    expect(recorded[0]!.body).toMatchObject({
      name: 'Company Microsoft',
      client_id: CLIENT_ID,
      authority: 'https://login.microsoftonline.com/organizations',
      credential_types: ['outlook'],
      scope_ceiling: { outlook: ['Mail.Read'] },
      auto_refresh_token: true,
      project_scope: 'all',
      project_ids: [],
      secret: SECRET,
    });
    await waitFor(() => expect(screen.queryByTestId('microsoft-app-dialog')).not.toBeInTheDocument());
    expect(document.body.textContent).not.toContain(SECRET);
    expect(screen.getByTestId('admin-microsoft-apps-notice')).toHaveTextContent('Saved.');
  }, testTimeout(20_000));

  it('edits without sending a secret unless one is typed, and starts the field empty', async () => {
    useHandlers({ apps: [APP] });
    const user = userEvent.setup();
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    await user.click(await screen.findByRole('button', { name: 'Edit' }));
    const secretInput = await screen.findByTestId('microsoft-app-secret');
    expect(secretInput).toHaveValue('');
    expect(screen.getByText(/cannot be shown/)).toBeVisible();
    // A Helm-seeded app says its values come from the chart.
    expect(screen.getByTestId('microsoft-app-helm-note')).toHaveTextContent('Helm');
    // The app's Entra checklist is part of its edit dialog.
    expect(screen.getByTestId('microsoft-app-checklist')).toBeVisible();

    await user.click(screen.getByTestId('microsoft-app-save'));
    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.method).toBe('PUT');
    expect(recorded[0]!.url).toMatch(new RegExp(`/admin/microsoft_oauth_apps/${APP_ID}$`));
    expect(recorded[0]!.body).not.toHaveProperty('secret');
  });

  it('removes the stored secret only when asked', async () => {
    useHandlers({ apps: [APP] });
    const user = userEvent.setup();
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    await user.click(await screen.findByRole('button', { name: 'Edit' }));
    await user.click(await screen.findByRole('checkbox', { name: /Remove the stored secret/ }));
    expect(screen.getByTestId('microsoft-app-secret')).toBeDisabled();
    await user.click(screen.getByTestId('microsoft-app-save'));

    await waitFor(() => expect(recorded).toHaveLength(1));
    expect((recorded[0]!.body as { secret?: string }).secret).toBe('');
  });

  it('keeps the dialog open and places the server refusal beside the field it names', async () => {
    useHandlers({ saveStatus: 400, saveBody: { error: 'the client id must be the application (client) ID GUID', field: 'client_id' } });
    const user = userEvent.setup();
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    await fillNewApp(user);
    await user.click(screen.getByTestId('microsoft-app-save'));

    expect(await screen.findByText('the client id must be the application (client) ID GUID')).toBeVisible();
    expect(screen.getByTestId('microsoft-app-dialog')).toBeVisible();
    expect(screen.getByTestId('microsoft-app-client-id')).toHaveAttribute('aria-invalid', 'true');
    // What was typed is still there.
    expect(screen.getByTestId('microsoft-app-name')).toHaveValue('Company Microsoft');
  });

  it('refuses an invalid project list before any request is sent', async () => {
    useHandlers();
    const user = userEvent.setup();
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    await fillNewApp(user);
    await user.click(screen.getByRole('radio', { name: 'Selected projects' }));
    await user.type(screen.getByTestId('microsoft-app-project-ids'), '3, abc');
    await user.click(screen.getByTestId('microsoft-app-save'));

    expect(await screen.findByText('Enter numeric project IDs separated by commas.')).toBeVisible();
    expect(recorded).toHaveLength(0);
  });

  it('disables and enables an app from its row', async () => {
    useHandlers({ apps: [APP] });
    const user = userEvent.setup();
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    await user.click(await screen.findByRole('switch', { name: 'Enable Company Microsoft' }));
    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.method).toBe('POST');
    expect(recorded[0]!.url).toMatch(new RegExp(`/admin/microsoft_oauth_apps/${APP_ID}/disable$`));
  });

  it('runs a Test and reports each check in words, never Entra text', async () => {
    useHandlers({ apps: [APP] });
    const user = userEvent.setup();
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    await user.click(await screen.findByRole('button', { name: 'Test' }));

    const panel = await screen.findByTestId('microsoft-app-test-result');
    expect(within(panel).getByTestId('microsoft-app-check-authority')).toHaveTextContent('could not reach login.microsoftonline.com');
    expect(within(panel).getByTestId('microsoft-app-check-client_id')).toBeVisible();
    expect(within(panel).getByText(/Consent cannot be tested without a person/)).toBeVisible();
    expect(recorded[0]!.url).toMatch(new RegExp(`/admin/microsoft_oauth_apps/${APP_ID}/test$`));
  });

  it('writes the Custom app switch to the settings route', async () => {
    useHandlers();
    const user = userEvent.setup();
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    await user.click(await screen.findByRole('switch', { name: /Allow Custom app/ }));
    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]!.method).toBe('PUT');
    expect(recorded[0]!.url).toMatch(/\/admin\/microsoft_oauth_apps\/settings$/);
    expect(recorded[0]!.body).toEqual({ custom_apps: false });
  });

  it('says the registry is unavailable when the deployment has no vault key (503)', async () => {
    useHandlers({ listStatus: 503 });
    renderAdminRoute(<AdminMicrosoftAppsEditor />);
    expect(await screen.findByTestId('admin-microsoft-apps-error')).toHaveTextContent('vault master key');
  });
});

// Read-only mode (R3.4b, issue 2613): Microsoft apps are the Client
// applications item. At its read level every write, the test included, is
// disabled; the checklist still reads.
describe('AdminMicrosoftAppsEditor - view only', () => {
  it('disables the app writes at the read level', async () => {
    useHandlers({ apps: [APP] });
    serveConsoleLevel('read', 'client-applications');
    renderAdminRoute(<AdminMicrosoftAppsEditor />);

    const row = await screen.findByTestId(`microsoft-app-row-${APP_ID}`);
    await findViewOnlyNotice();
    await waitFor(() => expect(screen.getByTestId('admin-microsoft-apps-add')).toBeDisabled());
    expect(within(screen.getByTestId('admin-microsoft-apps-custom')).getByRole('switch')).toBeDisabled();
    for (const button of within(row).getAllByRole('button')) expect(button).toBeDisabled();
    expect(screen.getByTestId('microsoft-deployment-checklist')).toBeVisible();
    expect(enabledWriteControls()).toEqual([]);
    expect(recorded.filter((entry) => entry.method !== 'GET')).toEqual([]);
  });
});
