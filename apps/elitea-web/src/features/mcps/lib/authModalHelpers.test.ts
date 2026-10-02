import { describe, expect, it } from 'vitest';

import {
  buildStartFlowOptions,
  ensureAuthServersAvailable,
  isStringArray,
  pickMcpAuthMetadataFields,
  resolveAuthModalDetailCopy,
  resolveFormDefaults,
  scopesToString,
} from './authModalHelpers';

describe('isStringArray', () => {
  it('returns true for string arrays', () => {
    expect(isStringArray(['a', 'b'])).toBe(true);
  });

  it('returns true for empty array', () => {
    expect(isStringArray([])).toBe(true);
  });

  it('returns false for non-arrays', () => {
    expect(isStringArray('hello')).toBe(false);
    expect(isStringArray(null)).toBe(false);
    expect(isStringArray(undefined)).toBe(false);
  });
});

describe('scopesToString', () => {
  it('joins array with space', () => {
    expect(scopesToString(['read', 'write'])).toBe('read write');
  });

  it('returns string as-is', () => {
    expect(scopesToString('openid profile')).toBe('openid profile');
  });

  it('returns empty for undefined', () => {
    expect(scopesToString(undefined)).toBe('');
  });

  it('trims array result', () => {
    expect(scopesToString([' read ', ' write '])).toBe('read   write');
  });
});

describe('pickMcpAuthMetadataFields', () => {
  it('extracts fields from metadata', () => {
    const meta = {
      authServers: ['https://auth.example.com'],
      oauthAuthorizationServer: { token_endpoint: 'https://token' },
      providedSettings: { mcp_client_id: 'id1' },
      resourceScopes: ['read'],
    };
    const result = pickMcpAuthMetadataFields(meta);
    expect(result.authServers).toEqual(['https://auth.example.com']);
    expect(result.resourceScopes).toEqual(['read']);
  });

  it('returns undefined fields for null metadata', () => {
    const result = pickMcpAuthMetadataFields(null);
    expect(result.authServers).toBeUndefined();
    expect(result.oauthAuthorizationServer).toBeUndefined();
  });
});

describe('resolveFormDefaults', () => {
  it('uses provided settings when available', () => {
    const result = resolveFormDefaults(
      { mcp_client_id: 'backend-id', mcp_client_secret: 'backend-secret', scopes: ['openid'] },
      'form-id', 'form-secret', 'form-scopes',
    );
    expect(result.clientId).toBe('backend-id');
    expect(result.clientSecret).toBe('backend-secret');
    expect(result.scopes).toEqual(['openid']);
    expect(result.hasBackendClientId).toBe(true);
    expect(result.hasBackendClientSecret).toBe(true);
  });

  it('falls back to form values when no provided settings', () => {
    const result = resolveFormDefaults(undefined, 'fid', 'fsec', 'fscopes');
    expect(result.clientId).toBe('fid');
    expect(result.clientSecret).toBe('fsec');
    expect(result.scopes).toBe('fscopes');
    expect(result.hasBackendClientId).toBe(false);
  });
});

describe('resolveAuthModalDetailCopy', () => {
  it('returns pre-registered copy when requiresClientSecret', () => {
    expect(resolveAuthModalDetailCopy(true, 'standard')).toContain('pre-registered');
  });

  it('returns flow-specific copy', () => {
    expect(resolveAuthModalDetailCopy(false, 'oidc')).toContain('OIDC');
    expect(resolveAuthModalDetailCopy(false, 'dcr')).toContain('automatic');
    expect(resolveAuthModalDetailCopy(false, 'pkce')).toContain('PKCE');
  });

  it('returns empty for standard flow without client secret', () => {
    expect(resolveAuthModalDetailCopy(false, 'standard')).toBe('');
  });
});

describe('ensureAuthServersAvailable', () => {
  it('throws when no authServers', () => {
    expect(() => ensureAuthServersAvailable(null)).toThrow('No authorization servers');
    expect(() => ensureAuthServersAvailable({ authServers: [] })).toThrow();
  });

  it('returns validated object when servers present', () => {
    const meta = { authServers: ['https://a.com'], oauthAuthorizationServer: { token_endpoint: 'x' }, oauthMetadata: null };
    const result = ensureAuthServersAvailable(meta);
    expect(result.authServers).toEqual(['https://a.com']);
  });
});

describe('buildStartFlowOptions', () => {
  it.each(['mcp', 'openapi', 'sharepoint'])('keeps the %s resource separate from credential storage', (toolkitType) => {
    const result = buildStartFlowOptions({
      storageKey: 'configuration:https://issuer.example',
      validated: { authServers: ['https://issuer.example'], oauthAuthorizationServer: undefined, oauthMetadata: undefined },
      authWindow: {} as Window,
      credentials: { clientId: 'client', clientSecret: '' },
      scope: 'records.read',
      flowContext: { toolkitId: '28', toolkitType, projectId: 2, serverUrl: 'https://protected.example/mcp' },
      isPrebuildMcp: false,
    });
    expect(result.serverUrl).toBe('configuration:https://issuer.example');
    expect(result.resourceUrl).toBe(toolkitType === 'mcp' ? 'https://protected.example/mcp' : undefined);
  });
  it('builds options from params', () => {
    const result = buildStartFlowOptions({
      storageKey: 'https://server.com',
      validated: { authServers: ['https://a.com'], oauthAuthorizationServer: { token_endpoint: 'tok' }, oauthMetadata: null },
      authWindow: {} as Window,
      credentials: { clientId: 'cid', clientSecret: 'csec' },
      scope: 'openid',
      flowContext: { toolkitId: 'tk1', toolkitType: 'mcp', projectId: 42 },
      isPrebuildMcp: true,
    });
    expect(result.serverUrl).toBe('https://server.com');
    expect(result.clientId).toBe('cid');
    expect(result.scope).toBe('openid');
    expect(result.toolkitType).toBe('mcp');
  });

  it('omits toolkitType when not isPrebuildMcp', () => {
    const result = buildStartFlowOptions({
      storageKey: undefined,
      validated: { authServers: ['x'], oauthAuthorizationServer: undefined, oauthMetadata: undefined },
      authWindow: {} as Window,
      credentials: { clientId: '', clientSecret: '' },
      scope: '',
      flowContext: { toolkitId: undefined, toolkitType: 'custom', projectId: undefined },
      isPrebuildMcp: false,
    });
    expect(result.toolkitType).toBeUndefined();
  });
});
