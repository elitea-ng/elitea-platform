/**
 * §6.2 discipline: no `vi.mock()` of application modules (R-M1). `Authorize`
 * drives the REAL `startMcpAuthFlow` (`../lib/oauthFlow`, already covered
 * end-to-end by `oauthFlow.test.ts`); the only stub here is the actual
 * browser global `window.open`, plus MSW for the network boundary and a
 * real `postMessage` to resolve the popup — same technique as
 * `oauthFlow.test.ts`'s header explains in full.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';
import { getSavedCredentials } from '../lib/storage';

import { McpAuthModal } from './McpAuthModal';

interface FakePopup {
  closed: boolean;
  close: () => void;
  location: { href: string };
  document: { body: { style: { cssText: string }; appendChild: (node: unknown) => void }; createElement: (tag: string) => { style: Record<string, unknown>; textContent: string } };
}

function stubPopup(): FakePopup {
  const popup: FakePopup = {
    closed: false,
    close: () => {
      popup.closed = true;
    },
    location: { href: '' },
    document: { body: { style: { cssText: '' }, appendChild: () => {} }, createElement: () => ({ style: {}, textContent: '' }) },
  };
  vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
  return popup;
}

function stateFromPopupUrl(popup: FakePopup): string {
  const url = new URL(popup.location.href);
  const state = url.searchParams.get('state');
  if (!state) throw new Error(`popup never navigated to an authorize URL (got "${popup.location.href}")`);
  return state;
}

function deliverAuthResult(result: { code?: string; error?: string; error_description?: string }, state: string): void {
  window.dispatchEvent(new MessageEvent('message', { origin: window.location.origin, data: { type: 'mcp-auth-result', state, ...result } }));
}

afterEach(() => {
  window.sessionStorage.clear();
  resetGeneratedClient();
  vi.restoreAllMocks();
});

function baseProps(overrides: Partial<Parameters<typeof McpAuthModal>[0]> = {}) {
  return {
    serverUrl: 'https://mcp.example.com',
    mcpAuthMetadata: {
      authServers: ['https://as.example.com'],
      oauthAuthorizationServer: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token', registration_endpoint: 'https://as.example.com/register' },
    },
    open: true,
    onClose: vi.fn(),
    onCancel: vi.fn(),
    ...overrides,
  };
}

describe('McpAuthModal', () => {
  it('uses configured consent scopes instead of replacing them with resource scopes', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const user = userEvent.setup();
    const popup = stubPopup();
    renderWithTheme(
      <McpAuthModal
        {...baseProps({
          mcpAuthMetadata: {
            authServers: ['https://as.example.com'],
            oauthAuthorizationServer: {
              authorization_endpoint: 'https://as.example.com/authorize',
              token_endpoint: 'https://as.example.com/token',
              code_challenge_methods_supported: ['S256'],
            },
            providedSettings: { mcp_client_id: 'configured-client', scopes: ['records.read', 'offline_access'] },
            resourceScopes: ['records.read'],
          },
        })}
      />,
    );
    expect(screen.getByPlaceholderText('Enter OAuth scopes (space-separated)')).toHaveValue('records.read offline_access');
    await user.click(screen.getByRole('button', { name: 'Authorize' }));
    await waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'));
    expect(new URL(popup.location.href).searchParams.get('scope')).toBe('records.read offline_access');
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
  });

  it('DCR-capable server: no client-id/secret fields shown, Authorize is enabled', () => {
    renderWithTheme(<McpAuthModal {...baseProps()} />);
    expect(screen.queryByLabelText(/Client ID/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/Client Secret/i)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Authorize' })).toBeEnabled();
  });

  it('a server needing a pre-registered secret shows both fields and disables Authorize until filled', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <McpAuthModal
        {...baseProps({
          mcpAuthMetadata: {
            authServers: ['https://as.example.com'],
            oauthAuthorizationServer: {
              authorization_endpoint: 'https://as.example.com/authorize',
              token_endpoint: 'https://as.example.com/token',
              token_endpoint_auth_methods_supported: ['client_secret_post'],
            },
          },
        })}
      />,
    );

    expect(screen.getByRole('button', { name: 'Authorize' })).toBeDisabled();
    await user.type(screen.getByLabelText(/Client ID/i), 'my-client-id');
    await user.type(screen.getByLabelText(/Client Secret/i), 'my-secret');
    expect(screen.getByRole('button', { name: 'Authorize' })).toBeEnabled();
  });

  it('clicking Authorize drives the real OAuth popup flow end-to-end, then shows success and closes', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const user = userEvent.setup();
    const popup = stubPopup();
    let exchangeBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/1', () => HttpResponse.json({ client_id: 'dcr-client' })),
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', async ({ request }) => {
        exchangeBody = await request.json();
        return HttpResponse.json({ access_token: 'issued-token' });
      }),
    );
    const onClose = vi.fn();
    renderWithTheme(<McpAuthModal {...baseProps({ onClose })} />);

    await user.click(screen.getByRole('button', { name: 'Authorize' }));
    await waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'));
    deliverAuthResult({ code: 'real-auth-code' }, stateFromPopupUrl(popup));

    expect(await screen.findByText(/Authorization successful/i)).toBeInTheDocument();
    expect(exchangeBody).toMatchObject({ code: 'real-auth-code', client_id: 'dcr-client' });
  });

  it('shows the rejection message inline when the OAuth provider reports an error, without closing', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const user = userEvent.setup();
    const popup = stubPopup();
    server.use(http.post('*/api/v2/elitea_core/mcp_dcr_proxy/1', () => HttpResponse.json({ client_id: 'dcr-client' })));
    const onClose = vi.fn();
    renderWithTheme(<McpAuthModal {...baseProps({ onClose })} />);

    await user.click(screen.getByRole('button', { name: 'Authorize' }));
    await waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'));
    deliverAuthResult({ error: 'Authorization cancelled by user' }, stateFromPopupUrl(popup));

    expect(await screen.findByText('Authorization cancelled by user')).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('shows POPUP_BLOCKED inline when window.open returns null', async () => {
    vi.spyOn(window, 'open').mockReturnValue(null);
    const user = userEvent.setup();
    renderWithTheme(<McpAuthModal {...baseProps()} />);

    await user.click(screen.getByRole('button', { name: 'Authorize' }));
    expect(await screen.findByText(/Popup blocked/i)).toBeInTheDocument();
  });

  it('checking "remember credentials" persists the entered credentials on success', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const user = userEvent.setup();
    const popup = stubPopup();
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () => HttpResponse.json({ access_token: 'tok' })));

    renderWithTheme(
      <McpAuthModal
        {...baseProps({
          mcpAuthMetadata: {
            authServers: ['https://as.example.com'],
            oauthAuthorizationServer: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token', token_endpoint_auth_methods_supported: ['client_secret_post'] },
          },
        })}
      />,
    );

    await user.type(screen.getByLabelText(/Client ID/i), 'saved-id');
    await user.type(screen.getByLabelText(/Client Secret/i), 'saved-secret');
    await user.click(screen.getByRole('checkbox'));
    await user.click(screen.getByRole('button', { name: 'Authorize' }));

    await waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'));
    deliverAuthResult({ code: 'auth-code' }, stateFromPopupUrl(popup));

    await waitFor(() => expect(getSavedCredentials('https://mcp.example.com')).toEqual({ client_id: 'saved-id', client_secret: 'saved-secret' }));
  });

  it('Cancel calls onCancel (no popup opened yet — exercises the null-authWindowRef guard)', async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();
    renderWithTheme(<McpAuthModal {...baseProps({ onCancel })} />);

    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('Cancel after Authorize closes the still-open popup window', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const user = userEvent.setup();
    const popup = stubPopup();
    const closeSpy = vi.spyOn(popup, 'close');
    server.use(http.post('*/api/v2/elitea_core/mcp_dcr_proxy/1', () => HttpResponse.json({ client_id: 'dcr-client' })));
    const onCancel = vi.fn();
    renderWithTheme(<McpAuthModal {...baseProps({ onCancel })} />);

    await user.click(screen.getByRole('button', { name: 'Authorize' }));
    await waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'));

    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(closeSpy).toHaveBeenCalledTimes(1);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('an overridden title renders instead of the default "MCP Authorization"', () => {
    renderWithTheme(<McpAuthModal {...baseProps({ title: 'Configuration OAuth' })} />);
    expect(screen.getByText('Configuration OAuth')).toBeInTheDocument();
    expect(screen.queryByText('MCP Authorization')).not.toBeInTheDocument();
  });

  it('re-derives the scope field when the modal is re-targeted at a different resource while it stays open', () => {
    const { rerender } = renderWithTheme(
      <McpAuthModal
        {...baseProps({
          mcpAuthMetadata: {
            authServers: ['https://as.example.com'],
            oauthAuthorizationServer: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token' },
            resourceScopes: ['repo:read'],
          },
        })}
      />,
    );
    // Not `getByLabelText` — the label wraps an `InfoTooltip` info button
    // whenever `availableScopes` is non-empty, and that button inherits the
    // same implicit label text (a pre-existing `OAuthFormFields` quirk,
    // out of this fix's scope), which would make the query ambiguous.
    expect(screen.getByPlaceholderText('Enter OAuth scopes (space-separated)')).toHaveValue('repo:read');

    // Same `open`/`serverUrl` (this modal instance never unmounts) but a
    // NEW `mcpAuthMetadata` for a different resource's scopes — regression
    // guard for the effect that must re-run and re-derive `scope`, not
    // keep showing the previous resource's value.
    rerender(
      <McpAuthModal
        {...baseProps({
          mcpAuthMetadata: {
            authServers: ['https://as.example.com'],
            oauthAuthorizationServer: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token' },
            resourceScopes: ['issues:write'],
          },
        })}
      />,
    );
    expect(screen.getByPlaceholderText('Enter OAuth scopes (space-separated)')).toHaveValue('issues:write');
  });
});
