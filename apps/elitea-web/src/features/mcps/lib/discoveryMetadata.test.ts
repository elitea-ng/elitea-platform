import { describe, expect, it } from 'vitest';

import { extractAuthServerMetadata, extractConfigAuthMetadata, extractMcpAuthMetadata } from './discoveryMetadata';

describe('extractMcpAuthMetadata', () => {
  it('preserves confidential DCR requirements in the metadata supplied to consent', () => {
    const server = {
      registration_endpoint: 'https://issuer.example.com/register',
      token_endpoint_auth_methods_supported: ['client_secret_post'],
      scopes_supported: ['records.read', 'offline_access'],
      private_extension: 'not projected',
    };
    const result = extractMcpAuthMetadata({ response_metadata: { resource_metadata: { oauth_authorization_server: server } } });
    expect(result.oauthMetadata?.token_endpoint_auth_methods_supported).toEqual(['client_secret_post']);
    expect(result.oauthMetadata?.scopes_supported).toEqual(['records.read', 'offline_access']);
    expect(result.oauthMetadata).not.toHaveProperty('private_extension');
  });

  it('reads response_metadata.resource_metadata (the streamed-message shape)', () => {
    const result = extractMcpAuthMetadata({
      response_metadata: {
        resource_metadata: {
          oauth_authorization_server: { token_endpoint: 't', authorization_endpoint: 'a' },
          authorization_servers: ['https://as.example.com'],
          scopes_supported: ['read'],
          configuration_uuid: 'uuid-1',
        },
      },
    });

    expect(result.authServers).toEqual(['https://as.example.com']);
    expect(result.oauthAuthorizationServer).toEqual({ token_endpoint: 't', authorization_endpoint: 'a' });
    expect(result.resourceScopes).toEqual(['read']);
    expect(result.configurationUuid).toBe('uuid-1');
  });

  it('reads toolMeta.resource_metadata (the toolActions shape)', () => {
    const result = extractMcpAuthMetadata({
      toolMeta: {
        resource_metadata: { authorization_servers: ['https://tool-meta.example.com'] },
        toolkit_id: 'tk-1',
      },
    });
    expect(result.authServers).toEqual(['https://tool-meta.example.com']);
    expect(result.toolkitId).toBe('tk-1');
  });

  it('falls back to toolOutputs.authorization_servers when nothing else has it', () => {
    const result = extractMcpAuthMetadata({ toolOutputs: { authorization_servers: ['https://fallback.example.com'] } });
    expect(result.authServers).toEqual(['https://fallback.example.com']);
  });

  it('response_metadata.authorization_servers wins over toolOutputs but not resource_metadata', () => {
    const result = extractMcpAuthMetadata({
      response_metadata: { authorization_servers: ['https://rm.example.com'] },
      toolOutputs: { authorization_servers: ['https://ignored.example.com'] },
    });
    expect(result.authServers).toEqual(['https://rm.example.com']);
  });

  it('prefers response_metadata.provided_settings, then toolMeta, then resource_metadata', () => {
    const rmWins = extractMcpAuthMetadata({
      response_metadata: { provided_settings: { mcp_client_id: 'from-rm' } },
      toolMeta: { provided_settings: { mcp_client_id: 'from-toolMeta' } },
    });
    expect(rmWins.providedSettings?.mcp_client_id).toBe('from-rm');

    const resourceFallback = extractMcpAuthMetadata({
      response_metadata: {
        resource_metadata: { provided_settings: { mcp_client_id: 'from-resource' } },
      },
    });
    expect(resourceFallback.providedSettings?.mcp_client_id).toBe('from-resource');
  });

  it('toolkitId prefers response_metadata, then toolMeta, then resource_metadata', () => {
    const result = extractMcpAuthMetadata({
      toolMeta: { resource_metadata: { toolkit_id: 'from-resource-meta' } },
    });
    expect(result.toolkitId).toBe('from-resource-meta');
  });

  it('oauthMetadata is null when there is no oauth_authorization_server', () => {
    const result = extractMcpAuthMetadata({});
    expect(result.oauthAuthorizationServer).toBeUndefined();
    expect(result.oauthMetadata).toBeNull();
  });

  it('handles a completely empty/undefined source without throwing', () => {
    expect(() => extractMcpAuthMetadata(undefined)).not.toThrow();
    expect(extractMcpAuthMetadata(undefined).authServers).toBeUndefined();
  });
});

describe('extractConfigAuthMetadata', () => {
  it('returns null for a null/undefined authMetadata (no auth_metadata on the 401)', () => {
    expect(extractConfigAuthMetadata(null)).toBeNull();
    expect(extractConfigAuthMetadata(undefined)).toBeNull();
  });

  it('prefers resource_metadata.authorization_servers over the top-level field', () => {
    const result = extractConfigAuthMetadata({
      resource_metadata: { authorization_servers: ['https://resource.example.com'] },
      authorization_servers: ['https://top-level.example.com'],
    });
    expect(result?.authServers).toEqual(['https://resource.example.com']);
  });

  it('falls back to an empty array when neither location has authorization_servers', () => {
    const result = extractConfigAuthMetadata({});
    expect(result?.authServers).toEqual([]);
  });

  it('carries the oauth_authorization_server and scopes through', () => {
    const result = extractConfigAuthMetadata({
      resource_metadata: {
        oauth_authorization_server: { authorization_endpoint: 'a', token_endpoint: 't', issuer: 'iss' },
        scopes_supported: ['read', 'write'],
      },
    });
    expect(result?.oauthAuthorizationServer?.issuer).toBe('iss');
    expect(result?.resourceScopes).toEqual(['read', 'write']);
  });
});

describe('extractAuthServerMetadata', () => {
  it('uses oauth_authorization_server directly when both endpoints are present', () => {
    const result = extractAuthServerMetadata({
      oauth_authorization_server: { authorization_endpoint: 'https://a', token_endpoint: 'https://t' },
    });
    expect(result).toEqual({ authorization_endpoint: 'https://a', token_endpoint: 'https://t' });
  });

  it('accepts authorization_server as an alias for oauth_authorization_server', () => {
    const result = extractAuthServerMetadata({
      authorization_server: { authorization_endpoint: 'https://a', token_endpoint: 'https://t' },
    });
    expect(result.authorization_endpoint).toBe('https://a');
  });

  it('treats the metadata object itself as the AS metadata when it directly carries both endpoints', () => {
    const result = extractAuthServerMetadata({ authorization_endpoint: 'https://direct-a', token_endpoint: 'https://direct-t' });
    expect(result.authorization_endpoint).toBe('https://direct-a');
  });

  it('falls back to constructing GitHub-style endpoints from authorization_servers[0] when nothing else has both endpoints', () => {
    const result = extractAuthServerMetadata({ authorization_servers: ['https://github.com/login/oauth/'] });
    expect(result.authorization_endpoint).toBe('https://github.com/login/oauth/authorize');
    expect(result.token_endpoint).toBe('https://github.com/login/oauth/access_token');
  });

  it('the fallback MERGES onto a partial oauth_authorization_server rather than discarding it', () => {
    const result = extractAuthServerMetadata({
      oauth_authorization_server: { issuer: 'keep-me' }, // missing both endpoints
      authorization_servers: ['https://github.com/login/oauth'],
    });
    expect(result.issuer).toBe('keep-me');
    expect(result.authorization_endpoint).toBe('https://github.com/login/oauth/authorize');
  });

  it('throws NO_AUTH_SERVERS when nothing at all is usable', () => {
    expect(() => extractAuthServerMetadata({})).toThrow('No authorization server found in MCP resource metadata');
    expect(() => extractAuthServerMetadata(null)).toThrow('No authorization server found in MCP resource metadata');
  });

  it('throws MISSING_ENDPOINTS when a server object exists but lacks one of the two required endpoints', () => {
    expect(() => extractAuthServerMetadata({ oauth_authorization_server: { authorization_endpoint: 'https://a' } })).toThrow(
      'Authorization server metadata is missing endpoints',
    );
  });
});
