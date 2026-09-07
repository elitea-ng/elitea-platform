/*
 * `SHAREPOINT_AUTH_MODALS` — the one place in the app that can put a real MCP
 * OAuth modal behind SharePoint's `renderAuthModal` slot.
 *
 * It had no test. Its whole body is the THREE shape differences its own module
 * comment enumerates, and each of them is silent when it is wrong:
 *
 *  1. `oauthAuthorizationServer` is `... | null` on the SharePoint side and
 *     `... | undefined` on the MCP side. A `null` reaching the modal is a
 *     discovery document the modal treats as present and empty.
 *  2. `providedSettings` arrives as an opaque object straight off a 401 body.
 *     `readString`/`readScopes` drop anything that is not the type the modal
 *     declares — a number where a client id is expected must not reach it.
 *  3. `onClose` is `(success: boolean)` here and `(success?: boolean)` there.
 *     An `undefined` widened to `true` would report a cancelled login as a
 *     successful one.
 *
 * The slot functions are CALLED, not rendered: they build an element and the
 * assertions read its props. Rendering would mount the real OAuth modal, which
 * opens a popup and runs discovery — none of which is what this file is about.
 */
import type { ReactElement } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { SHAREPOINT_AUTH_MODALS } from './sharepointAuthModals';

interface AuthModalProps {
  readonly mcpAuthMetadata: {
    readonly authServers?: readonly string[];
    readonly oauthAuthorizationServer?: unknown;
    readonly providedSettings?: {
      readonly mcp_client_id?: string;
      readonly mcp_client_secret?: string;
      readonly scopes?: string | readonly string[];
    };
    readonly resourceScopes?: readonly string[];
  } | null;
  readonly onClose: (success?: boolean) => void;
  readonly serverUrl: string;
  readonly title: string;
}

function authModalProps(metadata: unknown, overrides: Record<string, unknown> = {}): AuthModalProps {
  const element = SHAREPOINT_AUTH_MODALS.renderAuthModal({
    open: true,
    serverUrl: 'https://contoso.sharepoint.com',
    tokenStorageKey: 'sp-token',
    mcpAuthMetadata: metadata as never,
    formClientId: 'client-1',
    formClientSecret: 'secret-1',
    projectId: '1',
    toolkitId: 'tk-1',
    title: 'SharePoint',
    onClose: vi.fn(),
    onCancel: vi.fn(),
    ...overrides,
  }) as ReactElement;
  return element.props as AuthModalProps;
}

describe('SHAREPOINT_AUTH_MODALS.renderAuthModal', () => {
  it('passes a null auth metadata straight through', () => {
    expect(authModalProps(null).mcpAuthMetadata).toBeNull();
  });

  it('turns a null oauthAuthorizationServer into undefined', () => {
    // Difference 1. `null` here would reach the modal as "a discovery document
    // exists and is empty", not as "there is none".
    const props = authModalProps({ oauthAuthorizationServer: null });
    expect(props.mcpAuthMetadata?.oauthAuthorizationServer).toBeUndefined();
    expect(Object.hasOwn(props.mcpAuthMetadata as object, 'oauthAuthorizationServer')).toBe(true);
  });

  it('carries a real discovery document unchanged', () => {
    const server = { issuer: 'https://login.microsoftonline.com/' };
    expect(authModalProps({ oauthAuthorizationServer: server }).mcpAuthMetadata?.oauthAuthorizationServer)
      .toBe(server);
  });

  it('reads the three provided settings it knows and drops the rest', () => {
    const props = authModalProps({
      providedSettings: {
        mcp_client_id: 'id-1',
        mcp_client_secret: 'secret-2',
        scopes: 'Sites.Read.All',
        an_unknown_key: 'must not survive',
      },
      authServers: ['https://auth.example'],
      resourceScopes: ['Sites.Read.All'],
    });
    expect(props.mcpAuthMetadata?.providedSettings).toEqual({
      mcp_client_id: 'id-1',
      mcp_client_secret: 'secret-2',
      scopes: 'Sites.Read.All',
    });
    expect(props.mcpAuthMetadata?.authServers).toEqual(['https://auth.example']);
    expect(props.mcpAuthMetadata?.resourceScopes).toEqual(['Sites.Read.All']);
  });

  it('accepts an array of scopes and refuses a mixed array', () => {
    expect(authModalProps({ providedSettings: { scopes: ['a', 'b'] } }).mcpAuthMetadata?.providedSettings?.scopes)
      .toEqual(['a', 'b']);
    // One non-string member and the whole value is dropped: a half-read scope
    // list is a scope list the login would silently be missing entries from.
    expect(authModalProps({ providedSettings: { scopes: ['a', 7] } }).mcpAuthMetadata?.providedSettings?.scopes)
      .toBeUndefined();
  });

  it('drops a non-string client id rather than passing the wrong type on', () => {
    const props = authModalProps({ providedSettings: { mcp_client_id: 42, mcp_client_secret: null } });
    expect(props.mcpAuthMetadata?.providedSettings?.mcp_client_id).toBeUndefined();
    expect(props.mcpAuthMetadata?.providedSettings?.mcp_client_secret).toBeUndefined();
  });

  it('treats an absent providedSettings as an empty one', () => {
    expect(authModalProps({}).mcpAuthMetadata?.providedSettings).toEqual({
      mcp_client_id: undefined,
      mcp_client_secret: undefined,
      scopes: undefined,
    });
  });

  it('reports an undefined close as a FAILED login, never a successful one', () => {
    // Difference 3. This is the assertion that costs a user their session if it
    // is wrong: the caller writes a token only on `true`.
    const onClose = vi.fn();
    const props = authModalProps({}, { onClose });
    props.onClose(undefined);
    expect(onClose).toHaveBeenCalledWith(false);
    props.onClose(true);
    expect(onClose).toHaveBeenLastCalledWith(true);
  });
});

describe('SHAREPOINT_AUTH_MODALS.renderLogoutModal', () => {
  it('forwards the modal\'s own four props unchanged', () => {
    const onClose = vi.fn();
    const onConfirm = vi.fn();
    const element = SHAREPOINT_AUTH_MODALS.renderLogoutModal({
      serverUrl: 'https://contoso.sharepoint.com',
      open: true,
      onClose,
      onConfirm,
    }) as ReactElement;
    const props = element.props as {
      serverUrl: string;
      open: boolean;
      onClose: () => void;
      onConfirm: () => void;
    };
    expect(props.serverUrl).toBe('https://contoso.sharepoint.com');
    expect(props.open).toBe(true);
    // Identity, not a wrapper: the logout modal has no shape difference to
    // bridge, and a wrapper here would be a place for one to appear unnoticed.
    expect(props.onClose).toBe(onClose);
    expect(props.onConfirm).toBe(onConfirm);
  });
});
