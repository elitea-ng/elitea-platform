import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getAccessToken, getTokenInfo, setAccessToken } from '@/features/mcps/lib/storage';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getLogoutMarkerEventKey, getLogoutMarkerStorageKey, loadLogoutMarker } from '@/shared/lib/oauthLogoutSync';
import { createStorage } from '@/shared/lib/storage';
import { server } from '@/test/setup';

import { EditToolkit } from '../EditToolkit';
import { delegatedCredentialKey } from '../lib/DelegatedCredentialStatus';
import { renderToolkitsRoute } from './testRouter';

const ISSUER = 'https://identity.example.test/tenant';
const KEY = `credential-uuid:${ISSUER}`;
const OTHER_KEY = `other-credential:${ISSUER}`;

function installHandlers(type: string, delegated = true) {
  const credential = {
    id: 27, uuid: 'credential-uuid', type, elitea_title: 'selected-credential', label: 'Selected credential',
    project_id: 'proj-1', section: 'credentials',
    data: delegated ? { oauth_discovery_endpoint: ISSUER, client_secret: '{{secret.SAVED_SECRET}}' } : { client_id: 'app', client_secret: '{{secret.SAVED_SECRET}}' },
  };
  server.use(
    http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [{
      id: 'tk-1', type, name: 'Delegated fixture', settings: { account: { elitea_title: 'selected-credential', private: false } }, meta: {},
    }], total: 1 })),
    http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ [type]: {
      title: type, type: 'object', metadata: { label: 'Fixture' },
      $defs: { credential: { type: 'object', metadata: { section: 'credentials', type } } },
      properties: { account: { $ref: '#/$defs/credential', configuration_types: [type] } },
    } })),
    http.get('/api/v2/configurations/configurations/:projectId', () => HttpResponse.json({ items: [credential], total: 1, limit: 500, offset: 0 })),
    http.get('/api/v2/configurations/available/', () => HttpResponse.json([])),
    http.post('/api/v2/configurations/check_stored_connections/:projectId', () => HttpResponse.json([])),
  );
}

function renderEditor() {
  return renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });
}

beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => {
  resetGeneratedClient();
  window.localStorage.clear();
  window.sessionStorage.clear();
  vi.restoreAllMocks();
});

describe('credential-scoped toolkit logout through the mounted editor', () => {
  it.each(['openapi', 'delegated_fixture'])('works for %s without a toolkit-name branch', async (type) => {
    installHandlers(type);
    setAccessToken(KEY, 'access', 3600, undefined, undefined, 'refresh');
    setAccessToken(OTHER_KEY, 'other-access', 3600, undefined, undefined, 'other-refresh');
    renderEditor();
    const status = await screen.findByTestId('delegated-credential-status');
    expect(within(status).getByText('Saved authorization')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(within(status).getByRole('button', { name: 'Log out' }));
    const modal = await screen.findByTestId('mcp-logout-modal');
    expect(within(modal).getByText('Selected credential')).toBeInTheDocument();
    expect(modal.textContent).not.toContain('credential-uuid');
    expect(modal.textContent).not.toContain('SAVED_SECRET');
    await user.click(within(modal).getByRole('button', { name: 'Log out' }));
    await waitFor(() => expect(getTokenInfo(KEY)).toBeNull());
    expect(getAccessToken(OTHER_KEY)).toBe('other-access');
    expect(loadLogoutMarker(KEY)).toBeGreaterThan(0);
    expect(await screen.findByText('Authorize this credential when you use the toolkit.')).toBeInTheDocument();
  });

  it('keeps logout available for an expired access token and clears the refresh grant', async () => {
    installHandlers('openapi');
    setAccessToken(KEY, 'expired-access', -10, undefined, undefined, 'refresh');
    expect(getAccessToken(KEY)).toBeNull();
    renderEditor();
    const user = userEvent.setup();
    const status = await screen.findByTestId('delegated-credential-status');
    expect(within(status).getByText('Saved authorization')).toBeInTheDocument();
    await user.click(within(status).getByRole('button', { name: 'Log out' }));
    await user.click(within(await screen.findByTestId('mcp-logout-modal')).getByRole('button', { name: 'Log out' }));
    expect(getTokenInfo(KEY)).toBeNull();
  });

  it('invalidates a copied grant on a cross-tab logout event without a reload', async () => {
    installHandlers('delegated_fixture');
    setAccessToken(KEY, 'access', 3600, undefined, undefined, 'refresh');
    setAccessToken(OTHER_KEY, 'other-access', 3600, undefined, undefined, 'other-refresh');
    renderEditor();
    await screen.findByText('Saved authorization');
    const marker = String(Date.now() + 1);
    createStorage('local').set(getLogoutMarkerStorageKey(KEY)!, marker);
    act(() => { window.dispatchEvent(new StorageEvent('storage', { key: getLogoutMarkerEventKey(KEY), newValue: marker })); });
    expect(await screen.findByText('Authorize this credential when you use the toolkit.')).toBeInTheDocument();
    expect(getTokenInfo(KEY)).toBeNull();
    expect(getAccessToken(OTHER_KEY)).toBe('other-access');
  });

  it('leaves authorization unchanged when logout is cancelled', async () => {
    installHandlers('openapi');
    setAccessToken(KEY, 'access', 3600, undefined, undefined, 'refresh');
    renderEditor();
    const user = userEvent.setup();
    await user.click(within(await screen.findByTestId('delegated-credential-status')).getByRole('button', { name: 'Log out' }));
    await user.click(within(await screen.findByTestId('mcp-logout-modal')).getByRole('button', { name: 'Cancel' }));
    expect(getAccessToken(KEY)).toBe('access');
    expect(loadLogoutMarker(KEY)).toBe(0);
  });

  it('does not show delegated logout for client credentials', async () => {
    installHandlers('openapi', false);
    renderEditor();
    await screen.findByRole('combobox', { name: /Account/i });
    await screen.findByText('Selected credential');
    expect(screen.queryByTestId('delegated-credential-status')).not.toBeInTheDocument();
  });

  it('never substitutes a numeric id for a missing authorization UUID', () => {
    expect(delegatedCredentialKey({ id: '27', type: 'openapi', data: { oauth_discovery_endpoint: ISSUER }, eliteaTitle: 'title', displayLabel: 'Title', isPrivate: false, ownerProjectId: '2' })).toBeUndefined();
  });
});
