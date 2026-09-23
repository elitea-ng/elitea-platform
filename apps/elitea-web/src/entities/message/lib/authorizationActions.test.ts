import { describe, expect, it } from 'vitest';

import { buildAuthorizationActions } from './authorizationActions';

describe('buildAuthorizationActions', () => {
  it.each(['sharepoint', 'openapi', 'custom-delegated'])('retains public auth settings and the fixed secret placeholder for %s', (toolkitType) => {
    const [action] = buildAuthorizationActions({
      interrupt_id: 'auth-configured',
      toolkit_type: toolkitType,
      resource_metadata: {
        configuration_uuid: 'config-1',
        toolkit_id: '12',
        authorization_servers: ['https://login.example.test'],
        provided_settings: { mcp_client_id: 'public-client', mcp_client_secret: '********' },
        oauth_authorization_server: { authorization_endpoint: 'https://login.example.test/authorize', token_endpoint: 'https://login.example.test/token' },
      },
    }, '', '2026-09-06T00:00:00Z');
    expect(action?.toolMeta).toMatchObject({ resource_metadata: { toolkit_id: '12', provided_settings: { mcp_client_id: 'public-client', mcp_client_secret: '********' } } });
    expect(action?.toolOutputs).toMatchObject({ server_url: 'config-1:https://login.example.test' });
  });

  it('keeps a durable Skip decision available when OAuth discovery metadata is missing', () => {
    const [action] = buildAuthorizationActions({
      authorization_requests: [{
        interrupt_id: 'auth-pending',
        tool_call_id: 'call-pending',
        guardrail_type: 'mcp_auth',
        available_actions: ['authorize', 'skip'],
        toolkit_type: 'sharepoint',
        resource_metadata: null,
        authorization_servers: null,
      }],
    }, '', '2026-09-06T00:00:00Z');

    expect(action).toMatchObject({
      authorizationRequestId: 'auth-pending',
      status: 'action_required',
      isError: false,
      toolMeta: { interrupt_id: 'auth-pending', tool_call_id: 'call-pending' },
    });
  });

  it('does not convert an ordinary discovery error into a durable pause', () => {
    const [action] = buildAuthorizationActions({
      tool_run_id: 'failed-call',
      server_url: 'https://mcp.example.test',
    }, '', '2026-09-06T00:00:00Z');

    expect(action).toMatchObject({ status: 'error', isError: true });
  });

  it('builds every unique exact request from a parallel terminal event', () => {
    const actions = buildAuthorizationActions({
      authorization_requests: [
        {
          interrupt_id: 'auth-1',
          tool_call_id: 'call-1',
          tool_name: 'SharePoint search',
          toolkit_type: 'sharepoint',
          server_url: 'https://sharepoint.example.test',
          resource_metadata: {
            resource_name: 'SharePoint',
            authorization_servers: ['https://login.example.test'],
          },
        },
        {
          interrupt_id: 'auth-2',
          tool_call_id: 'call-2',
          tool_name: 'SharePoint list',
          toolkit_type: 'sharepoint',
          server_url: 'https://sharepoint.example.test',
          resource_metadata: {
            resource_name: 'SharePoint',
            authorization_servers: ['https://login.example.test'],
          },
        },
        { interrupt_id: 'auth-1' },
      ],
    }, 'Authorization required.', '2026-09-03T00:00:00Z');

    expect(actions.map((action) => action.authorizationRequestId)).toEqual(['auth-1', 'auth-2']);
    expect(actions.map((action) => (action.toolOutputs as Record<string, unknown>)['server_url']))
      .toEqual(['https://login.example.test', 'https://login.example.test']);
  });

  it('removes private credential values before rendering or persistence', () => {
    const [action] = buildAuthorizationActions({
      interrupt_id: 'auth-1',
      tool_name: 'Remote MCP',
      server_url: 'https://mcp.example.test',
      authorization_servers: ['https://login.example.test'],
      access_token: 'secret-access',
      proxyAuthorization: 'secret-proxy',
      resource_metadata: {
        authorization_servers: ['https://login.example.test'],
        provided_settings: {
          mcp_client_id: 'public-client',
          mcp_client_secret: 'secret-client',
          clientSecret: 'secret-camel-case',
        },
      },
    }, '', '2026-09-03T00:00:00Z');

    expect(JSON.stringify(action)).not.toContain('secret-access');
    expect(JSON.stringify(action)).not.toContain('secret-client');
    expect(JSON.stringify(action)).not.toContain('secret-camel-case');
    expect(JSON.stringify(action)).not.toContain('secret-proxy');
    expect(JSON.stringify(action)).toContain('public-client');
  });
});
