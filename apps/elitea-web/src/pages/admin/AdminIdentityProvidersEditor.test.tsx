/**
 * Rendering + write-path guard for the identity provider editor.
 *
 * The properties asserted here are the ones this screen's hazards make worth
 * asserting, and each one is invisible to a status-code test:
 *
 *  1. **The tri-state `secret`.** Absent, `''` and a value mean leave it, clear
 *     it, and re-seal it. The dialog cannot echo the stored secret, so a save
 *     that always sent the field would destroy the credential every time an
 *     operator edited a URL — and on this screen that credential is what the
 *     deployment's single sign-on runs on. This is the one bug here that is
 *     silent, permanent and impossible to notice from the UI, so the BODY of
 *     every write is inspected, not just its status.
 *  2. **No plaintext secret is ever rendered.** The listing carries a mask and
 *     there is no reveal, because nothing can reveal it.
 *  3. **Exactly one protocol document is sent**, and it is the one the kind
 *     names. Sending both would ask the server to store a definition of one
 *     protocol carrying the other's values.
 *  4. **A refusal renders the SERVER's own sentence**, which on this surface
 *     names the field it refused.
 *
 * No fixture value here is or resembles a real credential.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminIdentityProvidersEditor } from './AdminIdentityProvidersEditor';
import {
  isLoginDomain,
  normalizeProviderKey,
  resolveProviderSecretForSave,
  splitLoginDomains,
} from './adminIdentityProviderForm';
import { renderAdminRoute } from './__tests__/testRouter';

const PROVIDERS = [
  {
    key: 'corporate',
    kind: 'oidc' as const,
    display_name: 'Corporate SSO',
    enabled: true,
    revision: 3,
    secret: '******',
    oidc: {
      issuer: 'https://idp.example.com',
      client_id: 'elitea',
      redirect_uri: 'https://elitea.example.com/auth/oidc/callback',
      scopes: ['openid', 'profile', 'email'],
      require_email_verified: false,
    },
  },
  {
    key: 'legacy_saml',
    kind: 'saml' as const,
    display_name: 'Legacy SAML',
    enabled: false,
    revision: 1,
    saml: {
      idp_entity_id: 'https://idp.example.com/metadata',
      idp_sso_url: 'https://idp.example.com/sso',
      sp_entity_id: 'https://elitea.example.com/saml',
      acs_url: 'https://elitea.example.com/auth/saml/acs',
      idp_certificates: [],
    },
  },
];

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

function useProviderHandlers(
  options: { saveStatus?: number; saveBody?: Record<string, string> } = {},
): void {
  server.use(
    http.get('*/admin/identity_providers/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json({ providers: PROVIDERS, total: PROVIDERS.length });
    }),
    http.put('*/admin/identity_providers/administration/*', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      if (options.saveStatus !== undefined) {
        return HttpResponse.json(options.saveBody, { status: options.saveStatus });
      }
      return HttpResponse.json({ key: 'corporate', kind: 'oidc', display_name: 'Corporate SSO' });
    }),
    http.delete('*/admin/identity_providers/administration/*', ({ request }) => {
      recorded.push({ method: 'DELETE', url: request.url, body: null });
      return HttpResponse.json({ deleted: 'corporate' });
    }),
  );
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method !== 'GET');
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  useProviderHandlers();
});

afterEach(() => {
  resetGeneratedClient();
});

async function openEditFor(name: string): Promise<void> {
  const user = userEvent.setup();
  await screen.findByText(name);
  const row = screen.getByText(name).closest('tr');
  if (!row) throw new Error(`no row for ${name}`);
  const buttons = Array.from(row.querySelectorAll('button'));
  const editButton = buttons.find((button) => button.textContent === 'Edit');
  if (!editButton) throw new Error(`no edit button for ${name}`);
  await user.click(editButton);
}

describe('Admin › Authentication › identity providers', () => {
  it('lists providers with their key, protocol and live state', async () => {
    renderAdminRoute(<AdminIdentityProvidersEditor />);

    expect(await screen.findByText('Corporate SSO')).toBeInTheDocument();
    // The KEY is shown, not only the display name: it is the URL segment of the
    // surface and the name the sealed secret is derived from.
    expect(screen.getByText('corporate')).toBeInTheDocument();
    expect(screen.getByText('Legacy SAML')).toBeInTheDocument();
    expect(screen.getByText('Live')).toBeInTheDocument();
    expect(screen.getByText('Not in use')).toBeInTheDocument();
  });

  it('renders the mask for a stored secret and never a plaintext value', async () => {
    renderAdminRoute(<AdminIdentityProvidersEditor />);

    expect(await screen.findByText('******')).toBeInTheDocument();
    // The provider with no secret says so rather than rendering an empty cell,
    // so "no secret" and "a secret you may not read" stay distinguishable.
    expect(screen.getByText('None')).toBeInTheDocument();
    // There is no reveal control, because there is nothing to reveal.
    expect(screen.queryByRole('button', { name: /reveal|show/i })).not.toBeInTheDocument();
  });

  it('reads the providers from the administration mode', async () => {
    renderAdminRoute(<AdminIdentityProvidersEditor />);
    await screen.findByText('Corporate SSO');

    expect(recorded[0]?.url).toContain('/admin/identity_providers/administration');
  });

  it('OMITS secret when the operator did not touch it', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminIdentityProvidersEditor />);
    await openEditFor('Corporate SSO');

    // Change only the redirect URI — the case that happens constantly and must
    // not destroy the sealed credential.
    const redirect = await screen.findByLabelText(/Redirect URI/);
    await user.clear(redirect);
    await user.type(redirect, 'https://elitea.example.com/new/callback');
    await user.click(screen.getByTestId('identity-provider-save'));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    const body = writes()[0]?.body as Record<string, unknown>;
    const oidc = body['oidc'] as Record<string, unknown>;
    expect(oidc['redirect_uri']).toBe('https://elitea.example.com/new/callback');
    expect(Object.hasOwn(body, 'secret')).toBe(false);
  });

  it('sends exactly the document its kind names, and not the other', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminIdentityProvidersEditor />);
    await openEditFor('Corporate SSO');

    await user.click(screen.getByTestId('identity-provider-save'));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    const body = writes()[0]?.body as Record<string, unknown>;
    expect(body['kind']).toBe('oidc');
    expect(Object.hasOwn(body, 'oidc')).toBe(true);
    expect(Object.hasOwn(body, 'saml')).toBe(false);
  });

  it('keeps the dialog open on a refusal and shows the field the server named', async () => {
    useProviderHandlers({
      saveStatus: 400,
      saveBody: {
        error: 'the URL must use https: a plaintext federation endpoint exposes the login',
        field: 'issuer',
      },
    });
    const user = userEvent.setup();
    renderAdminRoute(<AdminIdentityProvidersEditor />);
    await openEditFor('Corporate SSO');

    await user.click(screen.getByTestId('identity-provider-save'));

    // The server's own sentence, not "Failed to save": it is the only text that
    // says which value to fix.
    expect(await screen.findByText(/plaintext federation endpoint/)).toBeInTheDocument();
    // And the dialog is still open, still holding what was typed.
    expect(screen.getByTestId('identity-provider-save')).toBeInTheDocument();
  });

  it('sends the login domains inside the document its kind names', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminIdentityProvidersEditor />);
    await openEditFor('Corporate SSO');

    const domains = await screen.findByTestId('identity-provider-login-domains');
    await user.type(domains, 'Corp.example.com, @example.org');
    await user.click(screen.getByTestId('identity-provider-save'));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    const body = writes()[0]?.body as Record<string, unknown>;
    const oidc = body['oidc'] as Record<string, unknown>;
    expect(oidc['login_domains']).toEqual(['corp.example.com', 'example.org']);
  });

  it('refuses a login domain that is not a domain name before it saves', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminIdentityProvidersEditor />);
    await openEditFor('Corporate SSO');

    await user.type(await screen.findByTestId('identity-provider-login-domains'), 'localhost');
    await user.click(screen.getByTestId('identity-provider-save'));

    expect(await screen.findByText(/is not a domain name/)).toBeInTheDocument();
    expect(writes()).toHaveLength(0);
  });

  it('states that a first provider needs a restart, before a save appears to do nothing', async () => {
    renderAdminRoute(<AdminIdentityProvidersEditor />);

    // Which browser-auth plane owns /auth is fixed at boot, so adding
    // the first provider to a deployment that federated none cannot mount its
    // routes under the running process. Saying so on the page is the difference
    // between a documented limit and a screen that looks broken.
    expect(await screen.findByTestId('admin-identity-providers-restart-note')).toBeInTheDocument();
  });
});

/* ── the two pure rules, asserted directly ─────────────────────────────── */

describe('identity provider form rules', () => {
  it('resolves the secret tri-state, with clearing winning over a stale keystroke', () => {
    expect(resolveProviderSecretForSave('', false)).toBeUndefined();
    expect(resolveProviderSecretForSave('typed', false)).toBe('typed');
    expect(resolveProviderSecretForSave('', true)).toBe('');
    // The checkbox disables the field, so a value left over from before it was
    // ticked must not resurrect a secret the operator chose to remove.
    expect(resolveProviderSecretForSave('left over', true)).toBe('');
  });

  it('derives the key the same way the server does', () => {
    // Must match `internal/identityproviders.NormalizeKey`. A divergence would
    // let an operator author a provider under one key and have the sealed
    // secret derived from another.
    expect(normalizeProviderKey('Corporate Okta')).toBe('corporate_okta');
    expect(normalizeProviderKey('corporate-okta')).toBe('corporate_okta');
    expect(normalizeProviderKey('  CORPORATE__OKTA  ')).toBe('corporate_okta');
    expect(normalizeProviderKey('---')).toBe('');
  });

  it('splits and checks login domains the way the server does', () => {
    expect(splitLoginDomains(' A.example.com,b.example.com\n@A.example.com ')).toEqual([
      'a.example.com',
      'b.example.com',
    ]);
    expect(splitLoginDomains('')).toEqual([]);
    expect(isLoginDomain('contoso.onmicrosoft.com')).toBe(true);
    expect(isLoginDomain('localhost')).toBe(false);
    expect(isLoginDomain('-bad.example.com')).toBe(false);
    expect(isLoginDomain('a..example.com')).toBe(false);
  });
});
