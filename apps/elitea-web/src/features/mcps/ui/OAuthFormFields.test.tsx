import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { writePersistedProject } from '@/shared/lib/selectedProjectPersistence';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { OAuthFormFields } from './OAuthFormFields';

const BASE = '/api/v2';
const PERMISSIONS_PATH = `${BASE}/auth/permissions/prompt_lib/:projectId`;
const SECRETS_PATH = `${BASE}/secrets/secrets/default/:projectId`;

function baseProps() {
  return {
    clientId: '',
    clientSecret: '',
    scope: '',
    onClientIdChange: vi.fn(),
    onClientSecretChange: vi.fn(),
    onScopeChange: vi.fn(),
    onSaveCredentialsChange: vi.fn(),
  };
}

/** `OAuthFormFields`'s Client Secret field is `SecretField` (A13) — every render needs a `QueryClient`, same as `SecretFieldInput.permission.test.tsx`'s identical wrapper. */
function renderField(props: ReturnType<typeof baseProps> & Record<string, unknown>) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrap = (ui: ReactElement) => <QueryClientProvider client={client}>{ui}</QueryClientProvider>;
  const result = renderWithTheme(wrap(<OAuthFormFields {...props} />));
  return {
    ...result,
    rerender: (nextProps: ReturnType<typeof baseProps> & Record<string, unknown>) =>
      result.rerender(wrap(<OAuthFormFields {...nextProps} />)),
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  // No persisted project: `useSecretFieldOptions()`'s two queries stay
  // `enabled: false`, so no MSW handler is needed for the tests that don't
  // touch the secret picker.
});

afterEach(() => {
  resetGeneratedClient();
});

describe('OAuthFormFields', () => {
  it('renders only the scope field when neither client id nor secret is needed', () => {
    renderField(baseProps());
    expect(screen.queryByLabelText(/Client ID/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/Client Secret/i)).not.toBeInTheDocument();
    expect(screen.getByLabelText(/Scope/i)).toBeInTheDocument();
  });

  it('shows the client ID field when needClientId is true, and reports edits', async () => {
    const user = userEvent.setup();
    const onClientIdChange = vi.fn();
    renderField({ ...baseProps(), needClientId: true, onClientIdChange });

    const field = screen.getByLabelText(/Client ID/i);
    await user.type(field, 'x');
    expect(onClientIdChange).toHaveBeenCalled();
  });

  it('shows the client secret field as a password input when needSecret is true', () => {
    renderField({ ...baseProps(), needSecret: true });
    const field = screen.getByLabelText(/Client Secret/i);
    expect(field).toHaveAttribute('type', 'password');
  });

  it('shows the "remember credentials" checkbox only when showSaveCredentials is true', () => {
    const { rerender } = renderField({ ...baseProps(), showSaveCredentials: false });
    expect(screen.queryByText(/Remember credentials/i)).not.toBeInTheDocument();

    rerender({ ...baseProps(), showSaveCredentials: true });
    expect(screen.getByText(/Remember credentials/i)).toBeInTheDocument();
  });

  it('toggling the checkbox calls onSaveCredentialsChange with the new checked state', async () => {
    const user = userEvent.setup();
    const onSaveCredentialsChange = vi.fn();
    renderField({ ...baseProps(), showSaveCredentials: true, saveCredentials: false, onSaveCredentialsChange });

    await user.click(screen.getByRole('checkbox'));
    expect(onSaveCredentialsChange).toHaveBeenCalledWith(true);
  });

  it('shows the scope-support tooltip trigger only when availableScopes is non-empty', () => {
    const { rerender } = renderField({ ...baseProps(), availableScopes: [] });
    expect(screen.queryByRole('button', { name: /MCP server supports/i })).not.toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: /Scope/i })).toBeInTheDocument();

    rerender({ ...baseProps(), availableScopes: ['read', 'write'] });
    expect(screen.getByRole('button', { name: /MCP server supports: read, write\./i })).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: /Scope/i })).toBeInTheDocument();
  });

  describe('Client Secret "Create new secret" shortcut (A13, ELITEA-0725)', () => {
    it('offers the saved-secret picker with a "Create new secret" entry that opens the secrets settings page', async () => {
      writePersistedProject({ id: 'proj-1', name: 'Acme' });
      server.use(
        http.get(PERMISSIONS_PATH, () =>
          HttpResponse.json([
            { name: 'configuration.secrets.secret.list', enabled: true },
            { name: 'configuration.secrets.secret.create', enabled: true },
          ]),
        ),
        http.get(SECRETS_PATH, () => HttpResponse.json([{ name: 'mcp_api_key', secret_name: 'mcp_api_key', is_default: false }])),
      );
      const openSpy = vi.spyOn(window, 'open').mockReturnValue(null);
      const user = userEvent.setup();

      renderField({ ...baseProps(), needSecret: true });

      // Starts in "password" mode (raw value); switch to "Secret" to reach the picker.
      await user.click(screen.getByRole('button', { name: 'Secret' }));
      await waitFor(() => {
        expect(screen.getByRole('combobox')).toBeInTheDocument();
      });
      await user.click(screen.getByRole('combobox'));
      await waitFor(() => {
        expect(screen.getByRole('option', { name: 'Create new secret' })).toBeInTheDocument();
      });
      expect(screen.getByRole('option', { name: 'mcp_api_key' })).toBeInTheDocument();

      await user.click(screen.getByRole('option', { name: 'Create new secret' }));
      expect(openSpy).toHaveBeenCalledTimes(1);
      expect(openSpy.mock.calls[0]?.[0]).toContain('/settings/secrets?createSecret=1');
    });
  });
});
