import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import type { SharepointResolvedConfig } from './useResolvedSharepointConfig.hooks';
import { authRequiredErrorData, useSharepointCheckConnection } from './useSharepointCheckConnection.hooks';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

const spConfig: SharepointResolvedConfig = {
  oauth_discovery_endpoint: 'https://login.microsoftonline.com/tenant',
  site_url: 'https://contoso.sharepoint.com',
  configuration_uuid: 'uuid-1',
};

describe('useSharepointCheckConnection', () => {
  it('does nothing when spConfig or projectId is missing', async () => {
    const { result } = renderHook(() => useSharepointCheckConnection({ projectId: undefined, spConfig }));
    await act(async () => {
      await result.current.runCheck();
    });
    expect(result.current.isRunning).toBe(false);
  });

  it('calls onSuccess when the connection test succeeds', async () => {
    server.use(http.post('*/api/v2/configurations/check_connection/proj-1/sharepoint', () => HttpResponse.json({})));
    const onSuccess = vi.fn();
    const { result } = renderHook(() => useSharepointCheckConnection({ projectId: 'proj-1', spConfig, onSuccess }));

    await act(async () => {
      await result.current.runCheck();
    });

    expect(onSuccess).toHaveBeenCalledTimes(1);
    expect(result.current.isRunning).toBe(false);
  });

  it('says why the sign-in could not start when the check fails for another reason (#1998)', async () => {
    server.use(
      http.post('*/api/v2/configurations/check_connection/proj-1/sharepoint', () =>
        HttpResponse.json({ success: false, message: 'The credential has no client id.' }, { status: 400 }),
      ),
    );
    const handleConfigAuthRequired = vi.fn();
    const { result } = renderHook(() => useSharepointCheckConnection({ projectId: 'proj-1', spConfig }));

    await act(async () => {
      await result.current.runCheck(handleConfigAuthRequired, 'token-key-1');
    });

    expect(handleConfigAuthRequired).not.toHaveBeenCalled();
    expect(result.current.error).toBe('The credential has no client id.');
  });

  describe('a live 401 with requires_authorization now reaches handleConfigAuthRequired', () => {
    it("calls handleConfigAuthRequired with the real 401 body (was: the reauth interceptor stripped it, so the OAuth modal could never open)", async () => {
      server.use(
        http.post('*/api/v2/configurations/check_connection/proj-1/sharepoint', () =>
          HttpResponse.json(
            { requires_authorization: true, auth_metadata: { server_url: 'https://x', resource_metadata: {} } },
            { status: 401 },
          ),
        ),
      );
      const handleConfigAuthRequired = vi.fn();
      const { result } = renderHook(() => useSharepointCheckConnection({ projectId: 'proj-1', spConfig }));

      await act(async () => {
        await result.current.runCheck(handleConfigAuthRequired, 'token-key-1');
      });

      // INVERTED ASSERTION, deliberately: this case used to assert
      // `not.toHaveBeenCalled()` and cite the gap as "not a defect introduced
      // by this port". True as far as it went — but it also meant the whole
      // delegated-login path could never work, and the passing test made that
      // look intentional. `shared/api/http.ts` now surfaces a 401 whose body
      // carries `requires_authorization` as `kind: 'http'` WITH that body
      // (a resource-authorization protocol response is not a session
      // failure), so `authRequiredErrorData` finds it and the modal opens.
      expect(handleConfigAuthRequired).toHaveBeenCalledTimes(1);
      expect(handleConfigAuthRequired).toHaveBeenCalledWith(
        { requires_authorization: true, auth_metadata: { server_url: 'https://x', resource_metadata: {} } },
        spConfig.oauth_discovery_endpoint,
        'token-key-1',
      );
    });

    it('leaves an ordinary 401 (no requires_authorization) on the re-auth path — handleConfigAuthRequired is not called', async () => {
      server.use(
        http.post('*/api/v2/configurations/check_connection/proj-1/sharepoint', () =>
          HttpResponse.json({ error: 'session expired' }, { status: 401 }),
        ),
      );
      const handleConfigAuthRequired = vi.fn();
      const { result } = renderHook(() => useSharepointCheckConnection({ projectId: 'proj-1', spConfig }));

      await act(async () => {
        await result.current.runCheck(handleConfigAuthRequired, 'token-key-1');
      });

      expect(handleConfigAuthRequired).not.toHaveBeenCalled();
    });
  });

  describe('authRequiredErrorData (pure) — proves the detection logic itself is correct and forward-compatible', () => {
    it('extracts the body from an EliteaApiError-shaped http failure with requires_authorization: true', () => {
      const caught = { failure: { kind: 'http', status: 401, body: { requires_authorization: true, auth_metadata: { server_url: 'https://x' } } } };
      expect(authRequiredErrorData(caught)).toEqual({ requires_authorization: true, auth_metadata: { server_url: 'https://x' } });
    });

    it('returns undefined for an EliteaApiError-shaped auth failure with no body (the shape a genuine session 401 still produces)', () => {
      const caught = { failure: { kind: 'auth', status: 401 } };
      expect(authRequiredErrorData(caught)).toBeUndefined();
    });

    it('returns undefined for an EliteaApiError-shaped http failure whose body lacks requires_authorization', () => {
      const caught = { failure: { kind: 'http', status: 401, body: { error: 'bad credentials' } } };
      expect(authRequiredErrorData(caught)).toBeUndefined();
    });

    it('returns undefined for a non-401 EliteaApiError-shaped failure', () => {
      const caught = { failure: { kind: 'http', status: 400, body: { requires_authorization: true } } };
      expect(authRequiredErrorData(caught)).toBeUndefined();
    });

    it('extracts a plain object with requires_authorization: true directly (baseline shape, forward-compatibility branch)', () => {
      const caught = { requires_authorization: true, auth_metadata: {} };
      expect(authRequiredErrorData(caught)).toBe(caught);
    });

    it('returns undefined for a plain error with no requires_authorization', () => {
      expect(authRequiredErrorData(new Error('network error'))).toBeUndefined();
      expect(authRequiredErrorData(undefined)).toBeUndefined();
      expect(authRequiredErrorData(null)).toBeUndefined();
    });
  });

  it('silently ignores a non-auth error (e.g. 400) without calling handleConfigAuthRequired', async () => {
    server.use(
      http.post('*/api/v2/configurations/check_connection/proj-1/sharepoint', () => HttpResponse.json({ error: 'bad' }, { status: 400 })),
    );
    const handleConfigAuthRequired = vi.fn();
    const { result } = renderHook(() => useSharepointCheckConnection({ projectId: 'proj-1', spConfig }));

    await act(async () => {
      await result.current.runCheck(handleConfigAuthRequired);
    });

    expect(handleConfigAuthRequired).not.toHaveBeenCalled();
  });

  it('sets isRunning true while the request is in flight', async () => {
    let resolveRequest: () => void = () => {};
    const pending = new Promise<void>((resolve) => {
      resolveRequest = resolve;
    });
    server.use(
      http.post('*/api/v2/configurations/check_connection/proj-1/sharepoint', async () => {
        await pending;
        return HttpResponse.json({});
      }),
    );
    const { result } = renderHook(() => useSharepointCheckConnection({ projectId: 'proj-1', spConfig }));

    let runPromise!: Promise<void>;
    act(() => {
      runPromise = result.current.runCheck();
    });

    await waitFor(() => expect(result.current.isRunning).toBe(true));

    resolveRequest();
    await act(async () => {
      await runPromise;
    });

    expect(result.current.isRunning).toBe(false);
  });

  it('ignores a concurrent runCheck call while one is already in flight', async () => {
    let resolveRequest: () => void = () => {};
    const pending = new Promise<void>((resolve) => {
      resolveRequest = resolve;
    });
    let requestCount = 0;
    server.use(
      http.post('*/api/v2/configurations/check_connection/proj-1/sharepoint', async () => {
        requestCount += 1;
        await pending;
        return HttpResponse.json({});
      }),
    );
    const { result } = renderHook(() => useSharepointCheckConnection({ projectId: 'proj-1', spConfig }));

    let firstRun!: Promise<void>;
    act(() => {
      firstRun = result.current.runCheck();
    });
    await waitFor(() => expect(result.current.isRunning).toBe(true));

    await act(async () => {
      await result.current.runCheck();
    });

    resolveRequest();
    await act(async () => {
      await firstRun;
    });

    expect(requestCount).toBe(1);
  });
});
