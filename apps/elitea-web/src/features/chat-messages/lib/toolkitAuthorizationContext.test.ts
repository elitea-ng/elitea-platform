/**
 * Golden cases ported from the baseline's `mcpAuthorization.helpers.test.js`.
 * The baseline builds its action through `buildMcpAuthorizationToolAction`,
 * which this app does not port; the action literals below are what that
 * builder produces for the same metadata (`toolMeta` = the stripped metadata
 * plus its normalised hierarchy).
 */
import { describe, expect, it } from 'vitest';

import { buildToolkitAuthorizationMessage, getToolkitAuthorizationContext } from './toolkitAuthorizationContext';

describe('toolkit authorization context', () => {
  it('keeps non-secret SharePoint OAuth hints', () => {
    const action = {
      name: 'search_files',
      toolOutputs: { toolkit_name: 'team-documents', toolkit_type: 'sharepoint' },
      toolMeta: {
        tool_run_id: 'call-sharepoint-1',
        tool_name: 'search_files',
        toolkit_name: 'team-documents',
        toolkit_type: 'sharepoint',
        server_url: 'https://sharepoint.example/mcp',
        resource_metadata_url: 'https://sharepoint.example/.well-known/oauth-protected-resource',
        resource_metadata: {
          resource_name: 'SharePoint',
          configuration_uuid: 'configuration-1',
          authorization_servers: ['https://login.example/tenant'],
          scopes_supported: ['Files.Read'],
        },
        metadata: {
          parent_agent_name: 'Researcher',
          parent_agent_call_id: 'agent-call-1',
          parent_agent_path: [
            { name: 'Coordinator', call_id: 'root-call-1' },
            { name: 'Researcher', call_id: 'agent-call-1' },
          ],
        },
        parent_agent_name: 'Researcher',
        parent_agent_call_id: 'agent-call-1',
        parent_agent_path: [
          { name: 'Coordinator', call_id: 'root-call-1' },
          { name: 'Researcher', call_id: 'agent-call-1' },
        ],
      },
      parent_agent_name: 'Researcher',
      parent_agent_call_id: 'agent-call-1',
    };

    expect(getToolkitAuthorizationContext(action)).toEqual({
      toolkitName: 'team-documents',
      toolkitType: 'SharePoint',
      originatorName: 'Researcher',
      originatorPath: ['Coordinator', 'Researcher'],
      serverUrl: 'https://sharepoint.example/mcp',
      resourceMetadataUrl: 'https://sharepoint.example/.well-known/oauth-protected-resource',
      authorizationServers: ['https://login.example/tenant'],
      scopes: ['Files.Read'],
    });
    expect(buildToolkitAuthorizationMessage(getToolkitAuthorizationContext(action))).toBe(
      'Authorization is required for toolkit "team-documents". Authorize now or skip it for this run?',
    );
  });

  it('keeps direct cards flat', () => {
    const direct = {
      name: 'list_direct',
      toolMeta: {
        tool_run_id: 'direct-call',
        tool_name: 'list_direct',
        toolkit_name: 'direct-toolkit',
        server_url: 'https://direct.example',
        authorization_servers: ['https://login.example'],
        parent_agent_name: '',
        parent_agent_call_id: '',
        parent_agent_path: [],
      },
    };

    expect(getToolkitAuthorizationContext(direct)).toMatchObject({
      toolkitName: 'direct-toolkit',
      originatorName: '',
      originatorPath: [],
      authorizationServers: ['https://login.example'],
    });
    expect(buildToolkitAuthorizationMessage(getToolkitAuthorizationContext(direct))).toBe(
      'Authorization is required for toolkit "direct-toolkit". Authorize now or skip it for this run?',
    );
  });

  it('never names a toolkit by URL, strips the mcp_authorize_ prefix and normalises types', () => {
    expect(
      getToolkitAuthorizationContext({ name: 'mcp_authorize_github', toolMeta: { toolkit_name: 'https://x.example' } }),
    ).toMatchObject({ toolkitName: 'github', toolkitType: '' });
    expect(getToolkitAuthorizationContext({ toolMeta: { toolkit_type: 'mcp_github' } })).toMatchObject({
      toolkitName: 'mcp_github',
      toolkitType: 'Remote MCP',
    });
    expect(getToolkitAuthorizationContext({ toolOutputs: { toolkit_type: 'open_api' } })).toMatchObject({
      toolkitType: 'OpenAPI',
    });
    expect(getToolkitAuthorizationContext({ toolMeta: { scopes: 'read' } })).toMatchObject({
      toolkitName: 'Toolkit',
      scopes: ['read'],
    });
  });

  it('falls back to the given message, then the default, without a context', () => {
    expect(buildToolkitAuthorizationMessage(null, 'custom')).toBe('custom');
    expect(buildToolkitAuthorizationMessage(null)).toBe('Toolkit authorization is required.');
  });
});
