import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { CreateToolkit } from '../CreateToolkit';
import { EditToolkit } from '../EditToolkit';
import { renderToolkitsRoute } from './testRouter';

const schema = {
  title: 'mcp', type: 'object', metadata: { label: 'Remote MCP' },
  properties: {
    url: { type: 'string', title: 'URL', default: 'https://resource.example.com/mcp' },
    selected_tools: { type: 'array', items: { type: 'string', enum: [] } },
  },
};
beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('/api/v2/elitea_core/platform_settings/prompt_lib', () => HttpResponse.json({ mcp_enabled: true, mcp_in_menu_enabled: true })),
    http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [{ id: 'tk-1', type: 'mcp', name: 'Discovery fixture', description: '', settings: { url: 'https://resource.example.com/mcp', selected_tools: [] }, meta: {} }], total: 1 })),
    http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ mcp: schema })),
    http.get('/api/v2/configurations/configurations/:projectId', () => HttpResponse.json({ items: [], total: 0, shared: { items: [], total: 0 } })),
    http.get('/api/v2/configurations/available/', () => HttpResponse.json([])),
    http.get('/api/v2/configurations/models/:projectId', () => HttpResponse.json({ items: [], total: 0 })),
    /*
     * Both of these used to fall through to the orval DEFAULT registry, whose
     * bodies are faker-random: 1–10 random-named tools for the saved toolkit,
     * and a random grant list. So every run rendered a different editor —
     * a different number of tool rows to build and query, different
     * permission-gated controls — behind the slowest requests in the file
     * (~240 ms each, against ~50 ms for the pinned ones). That variance on a
     * coverage-instrumented, 4-worker CI shard is what pushed the first test
     * past vitest's 5 s budget. Pinned: the saved toolkit has no
     * previously-discovered tools (discovery is what these tests drive), and
     * the caller holds no extra grants (none of the controls under test are
     * permission-gated).
     */
    http.get('/api/v2/elitea_core/toolkit_available_tools/prompt_lib/:projectId/:toolkitId', () => HttpResponse.json({ tools: [], args_schemas: {} })),
    http.get('/api/v2/auth/permissions/prompt_lib/:projectId', () => HttpResponse.json([])),
  );
});

afterEach(() => {
  resetGeneratedClient();
  window.sessionStorage.clear();
  window.localStorage.clear();
});

/**
 * Wait for a button by its label, then assert it as an accessible button ONCE.
 *
 * `findByRole` re-runs the full role query — accessible-name computation plus
 * a `getComputedStyle` visibility walk over the whole MUI editor — on every
 * DOM mutation and every 50 ms poll while the editor's request waterfall
 * settles. Measured on a coverage-instrumented, CPU-loaded run, that query
 * was ~65 % of the first test's time (~110 ms an evaluation) and is what
 * pushed it past 5 s on CI. A text match is a cheap, sufficient readiness
 * probe; the role assertion that follows keeps what this file proves — that
 * the control is exposed as an enabled-or-not, accessible, NOT-aria-hidden
 * button — at one evaluation instead of dozens.
 */
async function findButton(name: string): Promise<HTMLElement> {
  await screen.findAllByText(name, { selector: 'button, button *' });
  return screen.getByRole('button', { name });
}

describe('real MCP editor discovery composition', () => {
  it('connects saved MCP Login to REST discovery and its consent dialog', async () => {
    let requests = 0;
    server.use(http.post('/api/v2/elitea_core/mcp_sync_tools/prompt_lib/5', () => {
      requests++;
      return HttpResponse.json({ success: false, requires_authorization: true, response_metadata: {
        server_url: 'https://resource.example.com/mcp', resource_metadata: {
          authorization_servers: ['https://issuer.example.com'], oauth_authorization_server: {
            registration_endpoint: 'https://issuer.example.com/register',
            authorization_endpoint: 'https://issuer.example.com/authorize', token_endpoint: 'https://issuer.example.com/token',
          },
        },
      } });
    }));
    renderToolkitsRoute(<EditToolkit isMCP />, '/toolkits/latest/tk-1', { projectId: '5', instantTransitions: true });
    const user = userEvent.setup({ delay: null });
    await user.click(await findButton('Login'));
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
    expect(requests).toBe(1);
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }));
    // Zero-duration transitions: the dialog unmounts and the page behind it
    // leaves `aria-hidden` without waiting on the wall clock.
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Login' })).toBeEnabled();
  });

  it('clears only the selected toolkit grant after Logout confirmation', async () => {
    const resource = 'https://resource.example.com/mcp';
    const token = { access_token: 'fixture-token', refresh_token: 'fixture-refresh', expires_at: Date.now() + 3600000, granted_at: Date.now(), client_reference: 'fixture-reference' };
    window.sessionStorage.setItem('el.mcp.tokens', JSON.stringify({ [resource]: token, 'https://other.example.com/mcp': token }));
    renderToolkitsRoute(<EditToolkit isMCP />, '/toolkits/latest/tk-1', { projectId: '5', instantTransitions: true });
    const user = userEvent.setup({ delay: null });
    await user.click(await findButton('Logout'));
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Log out' }));
    expect(await screen.findByText('Not Connected')).toBeInTheDocument();
    const tokens = JSON.parse(window.sessionStorage.getItem('el.mcp.tokens') ?? '{}') as Record<string, unknown>;
    expect(tokens[resource]).toBeUndefined();
    expect(tokens['https://other.example.com/mcp']).toEqual(token);
    expect(Object.keys(window.localStorage).some(key => key.includes(resource))).toBe(true);
  });

  it.each(['edit', 'create'] as const)('connects Load Tools in the %s page to the request and selection', async (mode) => {
    let requests = 0;
    server.use(http.post('/api/v2/elitea_core/mcp_sync_tools/prompt_lib/5', () => {
      requests++;
      return HttpResponse.json({ success: true, tools: [{ name: 'echo_marker', inputSchema: { type: 'object' } }] });
    }));
    const save = vi.fn().mockResolvedValue({ id: 'tk-1' });
    const content = mode === 'edit' ? <EditToolkit isMCP deps={{ saveToolkit: save }} /> : <CreateToolkit isMCP deps={{ createToolkit: save }} />;
    renderToolkitsRoute(content, mode === 'edit' ? '/toolkits/latest/tk-1' : '/mcps/create/mcp', { projectId: '5', instantTransitions: true });
    const user = userEvent.setup({ delay: null });
    if (mode === 'create') {
      await user.click(await findButton('Remote MCP'));
      const url = await screen.findByRole('textbox', { name: 'URL' });
      await user.clear(url);
      // Paste once, as a user would, without rerendering the schema for every character.
      await user.paste('https://resource.example.com/mcp');
    }
    await user.click(await findButton('Load Tools'));
    await waitFor(() => expect(requests).toBe(1));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Load Tools' })).toBeEnabled());
    // The schema's empty enum must not hide the newly discovered operation.
    expect(await screen.findByText('Echo marker')).toBeInTheDocument();
  });

  it('opens DCR consent metadata from a discovery challenge, not a manual client-secret form', async () => {
    server.use(http.post('/api/v2/elitea_core/mcp_sync_tools/prompt_lib/5', () => HttpResponse.json({
      success: false, requires_authorization: true,
      response_metadata: {
        server_url: 'https://resource.example.com/mcp',
        resource_metadata: {
          authorization_servers: ['https://issuer.example.com'],
          oauth_authorization_server: {
            issuer: 'https://issuer.example.com', registration_endpoint: 'https://issuer.example.com/register',
            authorization_endpoint: 'https://issuer.example.com/authorize', token_endpoint: 'https://issuer.example.com/token',
          },
        },
      },
    })));
    renderToolkitsRoute(<EditToolkit isMCP />, '/toolkits/latest/tk-1', { projectId: '5', instantTransitions: true });
    await userEvent.setup({ delay: null }).click(await findButton('Load Tools'));
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
    expect(screen.getByText('MCP Authorization')).toBeInTheDocument();
    expect(screen.queryByLabelText(/client secret/i)).not.toBeInTheDocument();
  });
});
