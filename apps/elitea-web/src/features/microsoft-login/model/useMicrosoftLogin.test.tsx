/**
 * The web sign-in hook against a mocked Main (MSW): the popup is opened first, the
 * credential is saved before the sign-in starts, the handoff is matched on `state`
 * (postMessage and BroadcastChannel), and the redeemed token goes to the caller.
 */
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MockInstance } from 'vitest';

import { server } from '../../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { MESSAGE_TYPE } from '../lib/handoff';
import { readSession, rememberSession } from '../lib/session';
import { useMicrosoftLogin } from './useMicrosoftLogin';
import type { UseMicrosoftLoginParams } from './useMicrosoftLogin';

const BASE = '/api/v2';
const UUID = '5b0f4a42-0f0f-4c1f-9c55-0a7d8f1a2b3c';
const AUTHORIZE = 'https://login.microsoftonline.com/tenant-1/oauth2/v2.0/authorize?client_id=abc&state=state-1';

const START = {
  authorization_url: AUTHORIZE,
  state: 'state-1',
  expires_at: new Date(Date.now() + 600_000).toISOString(),
  app: 'custom',
  handoff_ttl_seconds: 60,
  channel_name: 'elitea-microsoft-login',
};

const REDEEMED = {
  access_token: 'access-token-1',
  token_type: 'Bearer',
  expires_at: new Date(Date.now() + 3_600_000).toISOString(),
  expires_in: 3600,
  scope: ['Mail.Read'],
  credential_id: UUID,
  credential_type: 'outlook',
  app: 'custom',
  tenant_id: 'tenant-1',
  account_name: 'Ada Lovelace',
};

interface FakePopup {
  readonly location: { href: string };
  readonly close: ReturnType<typeof vi.fn>;
}

let popup: FakePopup;
let openSpy: MockInstance<Window['open']>;

function callbackMessage(extra: Record<string, unknown>): void {
  window.dispatchEvent(new MessageEvent('message', { data: { type: MESSAGE_TYPE, state: 'state-1', ...extra }, origin: window.location.origin }));
}

function setup(overrides: Partial<UseMicrosoftLoginParams> = {}) {
  const onSignedIn = vi.fn<UseMicrosoftLoginParams['onSignedIn']>();
  const ensureSaved = vi.fn<UseMicrosoftLoginParams['ensureSaved']>().mockResolvedValue(UUID);
  const view = renderHook(() =>
    useMicrosoftLogin({
      projectId: 7,
      credentialUuid: undefined,
      ensureSaved,
      credentialData: { oauth_discovery_endpoint: 'https://login.microsoftonline.com/tenant-1' },
      onSignedIn,
      ...overrides,
    }),
  );
  return { ...view, onSignedIn, ensureSaved };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  popup = { location: { href: '' }, close: vi.fn() };
  openSpy = vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
  window.sessionStorage.clear();
});

afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

describe('useMicrosoftLogin', () => {
  it('opens the popup at once, saves first, starts, sends the popup to Microsoft and redeems the matching handoff', async () => {
    const order: string[] = [];
    let startBody: unknown;
    let redeemBody: unknown;
    server.use(
      http.post(`${BASE}/microsoft_login\\:start`, async ({ request }) => {
        order.push('start');
        startBody = await request.json();
        return HttpResponse.json(START);
      }),
      http.post(`${BASE}/microsoft_login\\:redeem`, async ({ request }) => {
        order.push('redeem');
        redeemBody = await request.json();
        return HttpResponse.json(REDEEMED);
      }),
    );
    const ensureSaved = vi.fn<UseMicrosoftLoginParams['ensureSaved']>().mockImplementation(() => {
      order.push('save');
      return Promise.resolve(UUID);
    });
    const { result, onSignedIn } = setup({ ensureSaved });

    act(() => {
      result.current.login();
    });
    // The popup was opened inside the click, before any await.
    expect(openSpy).toHaveBeenCalledTimes(1);
    expect(openSpy.mock.calls[0]?.[0]).toBe('about:blank');

    await waitFor(() => expect(popup.location.href).toBe(AUTHORIZE));
    expect(result.current.phase).toBe('waiting');
    act(() => {
      callbackMessage({ handoff: 'handoff-1' });
    });

    await waitFor(() => expect(result.current.phase).toBe('signedIn'));
    expect(order).toEqual(['save', 'start', 'redeem']);
    expect(startBody).toEqual({ project_id: 7, credential_id: UUID, client_kind: 'web' });
    expect(redeemBody).toEqual({ project_id: 7, handoff: 'handoff-1', state: 'state-1' });
    const signedIn = onSignedIn.mock.calls[0]?.[0];
    expect(signedIn?.credentialUuid).toBe(UUID);
    expect(signedIn?.redeemed.access_token).toBe('access-token-1');
    // Custom app: the credential's own discovery endpoint names the authority.
    expect(signedIn?.tokenKey).toBe(`${UUID}:https://login.microsoftonline.com/tenant-1`);
    expect(result.current.session).toMatchObject({ account: 'Ada Lovelace', tenant: 'tenant-1' });
    // Display facts only: the token is not in what is remembered.
    expect(JSON.stringify(readSession(UUID))).not.toContain('access-token-1');
  });

  it('keys a Default app sign-in by the authority the sign-in went to', async () => {
    server.use(
      http.post(`${BASE}/microsoft_login\\:start`, () => HttpResponse.json({ ...START, app: 'app-1' })),
      http.post(`${BASE}/microsoft_login\\:redeem`, () => HttpResponse.json({ ...REDEEMED, app: 'app-1' })),
    );
    const { result, onSignedIn } = setup({ credentialData: { oauth_app_id: 'app-1' } });
    act(() => {
      result.current.login();
    });
    await waitFor(() => expect(popup.location.href).toBe(AUTHORIZE));
    act(() => {
      callbackMessage({ handoff: 'h' });
    });
    await waitFor(() => expect(onSignedIn).toHaveBeenCalled());
    expect(onSignedIn.mock.calls[0]?.[0].tokenKey).toBe(`${UUID}:https://login.microsoftonline.com/tenant-1`);
  });

  it('ignores a handoff for another sign-in state', async () => {
    server.use(
      http.post(`${BASE}/microsoft_login\\:start`, () => HttpResponse.json(START)),
      http.post(`${BASE}/microsoft_login\\:redeem`, () => HttpResponse.json(REDEEMED)),
    );
    const { result, onSignedIn } = setup();
    act(() => {
      result.current.login();
    });
    await waitFor(() => expect(popup.location.href).toBe(AUTHORIZE));
    act(() => {
      window.dispatchEvent(
        new MessageEvent('message', { data: { type: MESSAGE_TYPE, state: 'not-mine', handoff: 'foreign' }, origin: window.location.origin }),
      );
    });
    expect(result.current.phase).toBe('waiting');
    expect(onSignedIn).not.toHaveBeenCalled();
  });

  it('fails with popup_blocked when the browser refuses the window, and starts nothing', () => {
    openSpy.mockReturnValue(null);
    const { result, ensureSaved } = setup();
    act(() => {
      result.current.login();
    });
    expect(result.current.phase).toBe('failed');
    expect(result.current.failure).toBe('popup_blocked');
    expect(ensureSaved).not.toHaveBeenCalled();
  });

  it('does not start a sign-in for a credential that could not be saved, and closes the popup', async () => {
    const start = vi.fn();
    server.use(http.post(`${BASE}/microsoft_login\\:start`, () => {
      start();
      return HttpResponse.json(START);
    }));
    const { result } = setup({ ensureSaved: vi.fn<UseMicrosoftLoginParams['ensureSaved']>().mockResolvedValue(undefined) });
    act(() => {
      result.current.login();
    });
    await waitFor(() => expect(result.current.failure).toBe('save_failed'));
    expect(start).not.toHaveBeenCalled();
    expect(popup.close).toHaveBeenCalled();
  });

  it('reports the stable code of a refused start', async () => {
    server.use(
      http.post(`${BASE}/microsoft_login\\:start`, () =>
        HttpResponse.json({ error: 'Custom apps are off', code: 'custom_apps_disabled' }, { status: 409 }),
      ),
    );
    const { result } = setup();
    act(() => {
      result.current.login();
    });
    await waitFor(() => expect(result.current.failure).toBe('custom_apps_disabled'));
    expect(result.current.phase).toBe('failed');
    expect(popup.close).toHaveBeenCalled();
  });

  it('reports an error the callback page posted, and never redeems', async () => {
    const redeem = vi.fn();
    server.use(
      http.post(`${BASE}/microsoft_login\\:start`, () => HttpResponse.json(START)),
      http.post(`${BASE}/microsoft_login\\:redeem`, () => {
        redeem();
        return HttpResponse.json(REDEEMED);
      }),
    );
    const { result } = setup();
    act(() => {
      result.current.login();
    });
    await waitFor(() => expect(popup.location.href).toBe(AUTHORIZE));
    act(() => {
      callbackMessage({ error: 'tenant_mismatch' });
    });
    await waitFor(() => expect(result.current.failure).toBe('tenant_mismatch'));
    expect(redeem).not.toHaveBeenCalled();
  });

  it('reports a refused redemption as handoff_invalid', async () => {
    server.use(
      http.post(`${BASE}/microsoft_login\\:start`, () => HttpResponse.json(START)),
      http.post(`${BASE}/microsoft_login\\:redeem`, () =>
        HttpResponse.json({ error: 'not valid', code: 'handoff_invalid' }, { status: 400 }),
      ),
    );
    const { result, onSignedIn } = setup();
    act(() => {
      result.current.login();
    });
    await waitFor(() => expect(popup.location.href).toBe(AUTHORIZE));
    act(() => {
      callbackMessage({ handoff: 'used-already' });
    });
    await waitFor(() => expect(result.current.failure).toBe('handoff_invalid'));
    expect(onSignedIn).not.toHaveBeenCalled();
  });

  it('refuses to send the popup anywhere but https', async () => {
    server.use(http.post(`${BASE}/microsoft_login\\:start`, () => HttpResponse.json({ ...START, authorization_url: 'javascript:alert(1)' })));
    const { result } = setup();
    act(() => {
      result.current.login();
    });
    await waitFor(() => expect(result.current.phase).toBe('failed'));
    expect(popup.location.href).toBe('');
  });

  it('cancel ends the wait without an error and closes the popup', async () => {
    server.use(http.post(`${BASE}/microsoft_login\\:start`, () => HttpResponse.json(START)));
    const { result } = setup();
    act(() => {
      result.current.login();
    });
    await waitFor(() => expect(result.current.phase).toBe('waiting'));
    act(() => {
      result.current.cancel();
    });
    await waitFor(() => expect(result.current.phase).toBe('idle'));
    expect(result.current.failure).toBeUndefined();
    expect(popup.close).toHaveBeenCalled();
  });

  it('clear forgets the remembered sign-in', async () => {
    server.use(
      http.post(`${BASE}/microsoft_login\\:start`, () => HttpResponse.json(START)),
      http.post(`${BASE}/microsoft_login\\:redeem`, () => HttpResponse.json(REDEEMED)),
    );
    const { result } = setup();
    act(() => {
      result.current.login();
    });
    await waitFor(() => expect(popup.location.href).toBe(AUTHORIZE));
    act(() => {
      callbackMessage({ handoff: 'h' });
    });
    await waitFor(() => expect(result.current.phase).toBe('signedIn'));
    expect(readSession(UUID)).toBeDefined();
    act(() => {
      result.current.clear();
    });
    expect(result.current.session).toBeUndefined();
    expect(readSession(UUID)).toBeUndefined();
  });

  it('picks up the sign-in remembered for the credential after a reload, unless its token has ended', () => {
    const remembered = { account: 'Ada Lovelace', tenant: 'tenant-1', app: 'custom', tokenKey: `${UUID}:x` };
    rememberSession(UUID, { ...remembered, expiresAt: new Date(Date.now() + 60_000).toISOString() });
    expect(setup({ credentialUuid: UUID }).result.current.session).toMatchObject({ account: 'Ada Lovelace' });

    rememberSession(UUID, { ...remembered, expiresAt: new Date(Date.now() - 60_000).toISOString() });
    expect(setup({ credentialUuid: UUID }).result.current.session).toBeUndefined();
  });
});
