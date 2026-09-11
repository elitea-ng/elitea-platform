import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { useConfigOAuthModal, useMcpAuthModal } from './useMcpAuthModal';

describe('useMcpAuthModal', () => {
  it('starts closed with no metadata', () => {
    const { result } = renderHook(() => useMcpAuthModal());
    expect(result.current.showModal).toBe(false);
    expect(result.current.getModalProps().open).toBe(false);
  });

  it('handleMcpAuthRequired opens the modal and normalises the message into mcpAuthMetadata', () => {
    const { result } = renderHook(() => useMcpAuthModal({ values: { id: 'tk-1', type: 'mcp', settings: { url: 'https://mcp.example.com' } } }));

    act(() => {
      result.current.handleMcpAuthRequired({
        response_metadata: {
          resource_metadata: { authorization_servers: ['https://as.example.com'], oauth_authorization_server: { authorization_endpoint: 'a', token_endpoint: 't' } },
          server_url: 'https://mcp.example.com',
        },
      });
    });

    expect(result.current.showModal).toBe(true);
    expect(result.current.mcpAuthMetadata?.authServers).toEqual(['https://as.example.com']);
    expect(result.current.getModalProps().open).toBe(true);
    expect(result.current.getModalProps().serverUrl).toBe('https://mcp.example.com');
    expect(result.current.getModalProps().tokenStorageKey).toBeUndefined();
  });

  it('derives a credential-scoped tokenStorageKey when configurationUuid + an auth endpoint are both present', () => {
    const { result } = renderHook(() => useMcpAuthModal({ values: {} }));

    act(() => {
      result.current.handleMcpAuthRequired({
        response_metadata: {
          resource_metadata: {
            authorization_servers: ['https://tenant.example.com/oauth2'],
            configuration_uuid: 'config-uuid-1',
          },
        },
      });
    });

    expect(result.current.getModalProps().tokenStorageKey).toBe('config-uuid-1:https://tenant.example.com/oauth2');
  });

  it('handleCloseModal(true) calls onSuccess and resets state; handleCloseModal(false) does not call onSuccess', () => {
    const onSuccess = vi.fn();
    const { result } = renderHook(() => useMcpAuthModal({ onSuccess }));

    act(() => result.current.handleMcpAuthRequired({}));
    expect(result.current.showModal).toBe(true);

    act(() => result.current.handleCloseModal(false));
    expect(onSuccess).not.toHaveBeenCalled();
    expect(result.current.showModal).toBe(false);

    act(() => result.current.handleMcpAuthRequired({}));
    act(() => result.current.handleCloseModal(true));
    expect(onSuccess).toHaveBeenCalledTimes(1);
  });

  it('handleCancelModal resets state without invoking onSuccess', () => {
    const onSuccess = vi.fn();
    const { result } = renderHook(() => useMcpAuthModal({ onSuccess }));
    act(() => result.current.handleMcpAuthRequired({}));
    act(() => result.current.handleCancelModal());
    expect(onSuccess).not.toHaveBeenCalled();
    expect(result.current.showModal).toBe(false);
  });

  it('openModal opens directly with pre-built metadata (e.g. from a toolActions source)', () => {
    const { result } = renderHook(() => useMcpAuthModal());
    act(() => result.current.openModal({ authServers: ['https://as.example.com'] }));
    expect(result.current.showModal).toBe(true);
    expect(result.current.mcpAuthMetadata?.authServers).toEqual(['https://as.example.com']);
  });

  it('marks remote and pre-built MCP grants for resource audience binding', () => {
    const prebuild = renderHook(() => useMcpAuthModal({ values: { type: 'mcp_github' } }));
    expect(prebuild.result.current.getModalProps().toolkitType).toBe('mcp_github');

    const remote = renderHook(() => useMcpAuthModal({ values: { type: 'mcp' } }));
    expect(remote.result.current.getModalProps().toolkitType).toBe('mcp');
    const openapi = renderHook(() => useMcpAuthModal({ values: { type: 'openapi' } }));
    expect(openapi.result.current.getModalProps().toolkitType).toBeUndefined();
  });
});

describe('useConfigOAuthModal', () => {
  it('handleConfigAuthRequired is a no-op without an auth_metadata field', () => {
    const { result } = renderHook(() => useConfigOAuthModal());
    act(() => result.current.handleConfigAuthRequired({}));
    expect(result.current.showModal).toBe(false);
  });

  it('opens with a fixed "Configuration OAuth" title and the provided server URL override', () => {
    const { result } = renderHook(() => useConfigOAuthModal({ credentials: { client_id: 'cid' } }));

    act(() => {
      result.current.handleConfigAuthRequired(
        { auth_metadata: { resource_metadata: { authorization_servers: ['https://as.example.com'] } } },
        'https://override.example.com',
      );
    });

    const props = result.current.getModalProps();
    expect(props.title).toBe('Configuration OAuth');
    expect(props.serverUrl).toBe('https://override.example.com');
    expect(props.formClientId).toBe('cid');
  });

  it('uses a credential-scoped tokenStorageKeyOverride when supplied, distinct from the display serverUrl', () => {
    const { result } = renderHook(() => useConfigOAuthModal());
    act(() => {
      result.current.handleConfigAuthRequired(
        { auth_metadata: { resource_metadata: {} } },
        'https://display-url.example.com',
        'uuid-1:https://tenant.example.com',
      );
    });
    const props = result.current.getModalProps();
    expect(props.serverUrl).toBe('https://display-url.example.com');
    expect(props.tokenStorageKey).toBe('uuid-1:https://tenant.example.com');
  });

  it('handleCloseModal(true) fires onSuccess', () => {
    const onSuccess = vi.fn();
    const { result } = renderHook(() => useConfigOAuthModal({ onSuccess }));
    act(() => result.current.handleConfigAuthRequired({ auth_metadata: { resource_metadata: {} } }, 'https://x.example.com'));
    act(() => result.current.handleCloseModal(true));
    expect(onSuccess).toHaveBeenCalledTimes(1);
  });
});
