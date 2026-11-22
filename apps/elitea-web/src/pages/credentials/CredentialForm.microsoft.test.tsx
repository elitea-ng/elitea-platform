/**
 * The Microsoft credential form (ADR-0057, S1d) end to end against a mocked Main (MSW)
 * and a stubbed popup: scopes as tick boxes with the consent note; only Custom app until
 * an administrator registers a Default app; Login saves first, signs in through the popup,
 * stores the token where runs read it and checks the connection with it; a refused token
 * asks for a new sign-in; Sign out forgets the token.
 */
import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { getAccessToken } from '@/features/mcps';

import { renderCredentialsRoute } from './__tests__/testRouter';
import { CredentialForm } from './CredentialForm';
import type { CredentialFormContext } from './CredentialForm';

const BASE = '/api/v2';
const UUID = '5b0f4a42-0f0f-4c1f-9c55-0a7d8f1a2b3c';
const TOKEN_KEY = `${UUID}:https://login.microsoftonline.com/tenant-1`;
const AUTHORIZE = 'https://login.microsoftonline.com/tenant-1/oauth2/v2.0/authorize?client_id=abc&state=state-1';
const CONTEXT: CredentialFormContext = { projectId: '7', isTeamProject: false, canUpdate: true, canDelete: true };

const NOTE = 'Microsoft Graph permissions requested at sign-in.\n\n- The token always holds every permission you already approved for this Entra app.';

const OUTLOOK_TYPE = {
  type: 'outlook',
  section: 'credentials',
  has_test_connection: true,
  config_schema: {
    title: 'OutlookConfiguration',
    properties: {
      data: {
        metadata: {
          sections: {
            auth: {
              required: true,
              subsections: [
                { name: 'Default app', fields: ['oauth_app_id', 'scopes', 'mailbox'] },
                { name: 'Custom app', fields: ['client_id', 'client_secret', 'oauth_discovery_endpoint', 'scopes', 'auto_refresh_token', 'mailbox'] },
              ],
            },
          },
        },
        properties: {
          oauth_app_id: { anyOf: [{ type: 'string' }, { type: 'null' }], default: null, title: 'App', ui_component: 'oauth_app_select' },
          client_id: { anyOf: [{ type: 'string' }, { type: 'null' }], default: null, title: 'Client Id' },
          client_secret: { anyOf: [{ type: 'string', format: 'password' }, { type: 'null' }], default: null, title: 'Client Secret' },
          oauth_discovery_endpoint: { anyOf: [{ type: 'string' }, { type: 'null' }], default: null, title: 'Oauth Discovery Endpoint' },
          scopes: {
            anyOf: [{ type: 'array', items: { type: 'string' } }, { type: 'null' }],
            default: ['Mail.Read'],
            title: 'Scopes',
            description: NOTE,
            ui_component: 'checkbox_list',
            checkbox_options: [
              { value: 'Mail.Read', label: 'Mail.Read', description: 'Read your mail' },
              { value: 'Mail.Send', label: 'Mail.Send', description: 'Send mail as you' },
            ],
          },
          auto_refresh_token: { anyOf: [{ type: 'boolean' }, { type: 'null' }], default: true, title: 'Auto Refresh Token' },
          mailbox: { anyOf: [{ type: 'string' }, { type: 'null' }], default: null, title: 'Mailbox' },
        },
      },
    },
  },
};

const START = {
  authorization_url: AUTHORIZE,
  state: 'state-1',
  expires_at: new Date(Date.now() + 600_000).toISOString(),
  app: 'custom',
  handoff_ttl_seconds: 60,
  channel_name: 'elitea-microsoft-login',
};

const REDEEMED = {
  access_token: 'access-token-1',
  token_type: 'Bearer',
  expires_at: new Date(Date.now() + 3_600_000).toISOString(),
  expires_in: 3600,
  scope: ['Mail.Read'],
  credential_id: UUID,
  credential_type: 'outlook',
  app: 'custom',
  tenant_id: 'tenant-1',
  account_name: 'Ada Lovelace',
};

class ResizeObserverStub {
  observe(): void {
    // no-op
  }
  disconnect(): void {
    // no-op
  }
}

interface FakePopup {
  readonly location: { href: string };
  readonly close: ReturnType<typeof vi.fn>;
}

let popup: FakePopup;

function renderForm(ui: ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderCredentialsRoute(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

function mockRegistry(apps: readonly { id: string; name: string }[], customAllowed = true): void {
  server.use(
    http.get(`${BASE}/microsoft_oauth_apps/7`, () =>
      HttpResponse.json({ credential_type: 'outlook', apps: apps.map((app) => ({ ...app, description: '' })), custom_app_allowed: customAllowed }),
    ),
  );
}

beforeEach(() => {
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
  configureGeneratedClient({ baseUrl: BASE });
  window.sessionStorage.clear();
  popup = { location: { href: '' }, close: vi.fn() };
  vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
  server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OUTLOOK_TYPE])));
  mockRegistry([]);
});

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function createForm(onSaved = vi.fn()) {
  renderForm(
    <CredentialForm
      context={CONTEXT}
      mode={{ kind: 'create', credentialType: 'outlook' }}
      onSaved={onSaved}
      onDiscarded={vi.fn()}
    />,
  );
  return onSaved;
}

function mockSignInBackend(check: () => Response | Promise<Response>) {
  const calls = { created: [] as unknown[], updated: [] as unknown[], checks: [] as Record<string, unknown>[], redeemed: 0 };
  server.use(
    http.post(`${BASE}/configurations/configurations/7`, async ({ request }) => {
      calls.created.push(await request.json());
      return HttpResponse.json({ id: 11, uuid: UUID, uid: UUID, type: 'outlook' });
    }),
    http.put(`${BASE}/configurations/configuration/7/11`, async ({ request }) => {
      calls.updated.push(await request.json());
      return HttpResponse.json({ id: 11, uuid: UUID, uid: UUID, type: 'outlook' });
    }),
    http.post(`${BASE}/microsoft_login\\:start`, () => HttpResponse.json(START)),
    http.post(`${BASE}/microsoft_login\\:redeem`, () => {
      calls.redeemed += 1;
      return HttpResponse.json(REDEEMED);
    }),
    http.post(`${BASE}/configurations/check_connection/7/outlook`, async ({ request }) => {
      calls.checks.push((await request.json()) as Record<string, unknown>);
      return check();
    }),
  );
  return calls;
}

function popupAnswers(extra: Record<string, unknown>): void {
  window.dispatchEvent(
    new MessageEvent('message', { data: { type: 'elitea:microsoft-login', state: 'state-1', ...extra }, origin: window.location.origin }),
  );
}

describe('Microsoft credential form — fields', () => {
  it('shows the scopes as tick boxes with their help and the consent note, ticked as the schema defaults', async () => {
    createForm();
    expect(await screen.findByRole('checkbox', { name: 'Mail.Read' })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: 'Mail.Send' })).not.toBeChecked();
    expect(screen.getByText('Read your mail')).toBeInTheDocument();
    expect(screen.getByTestId('microsoft-scopes-note')).toHaveTextContent('every permission you already approved');
  });

  it('keeps the scopes in the list\'s own order, whatever order they are ticked in', async () => {
    const calls = mockSignInBackend(() => HttpResponse.json({ success: true }));
    createForm();
    const send = await screen.findByRole('checkbox', { name: 'Mail.Send' });
    fireEvent.click(send);
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'my-outlook' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(calls.created).toHaveLength(1));
    expect((calls.created[0] as { data: { scopes: string[] } }).data.scopes).toEqual(['Mail.Read', 'Mail.Send']);
  });

  it('offers only Custom app while the registry is empty: no Default app choice, no empty select', async () => {
    createForm();
    await screen.findByRole('checkbox', { name: 'Mail.Read' });
    expect(screen.queryByRole('radio', { name: 'Default app' })).not.toBeInTheDocument();
    expect(screen.getByLabelText('Client Id')).toBeInTheDocument();
  });

  it('offers Default app and Custom app once an administrator registered an app, and picks the first app', async () => {
    mockRegistry([{ id: 'app-1', name: 'Contoso Entra app' }]);
    const calls = mockSignInBackend(() => HttpResponse.json({ success: true }));
    createForm();
    expect(await screen.findByRole('radio', { name: 'Default app' })).toBeChecked();
    expect(screen.getByRole('radio', { name: 'Custom app' })).not.toBeChecked();
    // The Default app subsection holds no client of its own.
    expect(screen.queryByLabelText('Client Id')).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'my-outlook' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(calls.created).toHaveLength(1));
    expect((calls.created[0] as { data: Record<string, unknown> }).data['oauth_app_id']).toBe('app-1');
  });

  it('leaves only the Default apps when an administrator turned Custom app off', async () => {
    mockRegistry([{ id: 'app-1', name: 'Contoso Entra app' }], false);
    createForm();
    await screen.findByRole('checkbox', { name: 'Mail.Read' });
    expect(screen.queryByRole('radio')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Client Id')).not.toBeInTheDocument();
  });
});

describe('Microsoft credential form — sign-in', () => {
  async function signIn(onSaved = vi.fn()) {
    createForm(onSaved);
    await screen.findByRole('checkbox', { name: 'Mail.Read' });
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'my-outlook' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save and sign in with Microsoft' }));
    await waitFor(() => expect(popup.location.href).toBe(AUTHORIZE));
    popupAnswers({ handoff: 'handoff-1' });
    return onSaved;
  }

  it('saves first, signs in through the popup, shows the account, stores the token for runs and checks the connection with it', async () => {
    const calls = mockSignInBackend(() => HttpResponse.json({ success: true }));
    const onSaved = await signIn();

    expect(await screen.findByText('Signed in as Ada Lovelace')).toBeInTheDocument();
    expect(screen.getByText('Microsoft tenant: tenant-1')).toBeInTheDocument();
    expect(screen.getByText('Runs act as Ada Lovelace')).toBeInTheDocument();
    // The credential was created by the sign-in's own save, with the form's values.
    expect(calls.created).toHaveLength(1);
    expect(calls.created[0]).toMatchObject({ type: 'outlook', label: 'my-outlook', data: { scopes: ['Mail.Read'] } });
    // The form stays open: no navigation until the person saves or discards.
    expect(onSaved).not.toHaveBeenCalled();
    // Runs read the token under {credential uuid}:{authority}.
    expect(getAccessToken(TOKEN_KEY)).toBe('access-token-1');
    // The check after the sign-in carries the token, and passes.
    await waitFor(() => expect(calls.checks).toHaveLength(1));
    expect(calls.checks[0]).toMatchObject({ access_token: 'access-token-1' });
    expect(await screen.findByTestId('credential-test-result')).toHaveTextContent('Connection successful');
  });

  it('updates the credential the sign-in created, instead of creating a second one, when the person saves afterwards', async () => {
    const calls = mockSignInBackend(() => HttpResponse.json({ success: true }));
    const onSaved = await signIn();
    await screen.findByText('Signed in as Ada Lovelace');
    // A rename after the sign-in changes the label, never the stable elitea_title other domains resolve the credential by.
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'renamed-outlook' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith('11'));
    expect(calls.created).toHaveLength(1);
    expect(calls.updated).toHaveLength(1);
    expect(calls.updated[0]).toMatchObject({ label: 'renamed-outlook', elitea_title: 'my-outlook' });
  });

  it('asks for a new sign-in and drops the token when the check says the credential needs one again (401)', async () => {
    mockSignInBackend(() =>
      HttpResponse.json({ success: false, requires_authorization: true, auth_metadata: { server_url: 'x' } }, { status: 401 }),
    );
    await signIn();
    expect(await screen.findByTestId('microsoft-login-expired')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign in with Microsoft' })).toBeInTheDocument();
    await waitFor(() => expect(getAccessToken(TOKEN_KEY)).toBeNull());
    expect(screen.getByTestId('credential-test-signin')).toBeInTheDocument();
  });

  it('signs in again from the signed-in summary', async () => {
    mockSignInBackend(() => HttpResponse.json({ success: true }));
    await signIn();
    await screen.findByText('Signed in as Ada Lovelace');
    popup.location.href = '';
    fireEvent.click(screen.getByRole('button', { name: 'Sign in again' }));
    await waitFor(() => expect(popup.location.href).toBe(AUTHORIZE));
  });

  it('signs out: the token is forgotten and the last check result is cleared', async () => {
    mockSignInBackend(() => HttpResponse.json({ success: true }));
    await signIn();
    await screen.findByTestId('credential-test-result');
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    await waitFor(() => expect(getAccessToken(TOKEN_KEY)).toBeNull());
    expect(await screen.findByRole('button', { name: 'Sign in with Microsoft' })).toBeInTheDocument();
    expect(screen.queryByTestId('credential-test-result')).not.toBeInTheDocument();
  });

  it('does not check the connection with no token: it says to sign in first, and sends nothing', async () => {
    const calls = mockSignInBackend(() => HttpResponse.json({ success: true }));
    createForm();
    await screen.findByRole('checkbox', { name: 'Mail.Read' });
    fireEvent.click(screen.getByTestId('credential-test-connection'));
    expect(await screen.findByTestId('credential-test-signin')).toBeInTheDocument();
    expect(calls.checks).toHaveLength(0);
  });

  it('shows why a sign-in the callback page reported as failed did not finish', async () => {
    mockSignInBackend(() => HttpResponse.json({ success: true }));
    createForm();
    await screen.findByRole('checkbox', { name: 'Mail.Read' });
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'my-outlook' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save and sign in with Microsoft' }));
    await waitFor(() => expect(popup.location.href).toBe(AUTHORIZE));
    popupAnswers({ error: 'tenant_mismatch' });
    expect(await screen.findByRole('alert')).toHaveTextContent('different Microsoft tenant');
    expect(getAccessToken(TOKEN_KEY)).toBeNull();
  });

  it('shows "Login managed by a project admin" to a person who cannot update credentials', async () => {
    renderForm(
      <CredentialForm
        context={{ ...CONTEXT, canUpdate: false }}
        mode={{ kind: 'create', credentialType: 'outlook' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    expect(await screen.findByText('Login managed by a project admin')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /sign in with microsoft/i })).not.toBeInTheDocument();
  });
});

describe('Other credential types are untouched', () => {
  it('shows no Microsoft sign-in block on a credential that does not sign in with Microsoft', async () => {
    server.use(
      http.get(`${BASE}/configurations/available/`, () =>
        HttpResponse.json([
          {
            type: 'openai',
            section: 'credentials',
            config_schema: { title: 'OpenAI', properties: { data: { properties: { api_key: { type: 'string', title: 'API Key', secret: true } } } } },
          },
        ]),
      ),
    );
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'openai' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await screen.findByText('API Key');
    expect(screen.queryByTestId('microsoft-login-panel')).not.toBeInTheDocument();
  });
});
