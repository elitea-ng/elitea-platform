import { screen, waitFor } from '@testing-library/react';
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
  );
});

afterEach(() => {
  resetGeneratedClient();
  window.sessionStorage.clear();
});

describe('real MCP editor discovery composition', () => {
  it.each(['edit', 'create'] as const)('connects Load Tools in the %s page to the request and selection', async (mode) => {
    let requests = 0;
    server.use(http.post('/api/v2/elitea_core/mcp_sync_tools/prompt_lib/5', () => {
      requests++;
      return HttpResponse.json({ success: true, tools: [{ name: 'echo_marker', inputSchema: { type: 'object' } }] });
    }));
    const save = vi.fn().mockResolvedValue({ id: 'tk-1' });
    const content = mode === 'edit' ? <EditToolkit isMCP deps={{ saveToolkit: save }} /> : <CreateToolkit isMCP deps={{ createToolkit: save }} />;
    renderToolkitsRoute(content, mode === 'edit' ? '/toolkits/latest/tk-1' : '/mcps/create/mcp', { projectId: '5' });
    const user = userEvent.setup();
    if (mode === 'create') {
      await user.click(await screen.findByRole('button', { name: 'Remote MCP' }));
      const url = await screen.findByRole('textbox', { name: 'URL' });
      await user.clear(url);
      await user.type(url, 'https://resource.example.com/mcp');
    }
    await user.click(await screen.findByRole('button', { name: 'Load Tools' }));
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
    renderToolkitsRoute(<EditToolkit isMCP />, '/toolkits/latest/tk-1', { projectId: '5' });
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Load Tools' }));
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
    expect(screen.getByText('MCP Authorization')).toBeInTheDocument();
    expect(screen.queryByLabelText(/client secret/i)).not.toBeInTheDocument();
  });
});
