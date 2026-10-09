import { describe, expect, it, vi, type Mock } from 'vitest';

import type { HostBridge } from './hostBridge';
import { createHostTransport } from './hostTransport';

interface Calls {
  accessToken: Mock<HostBridge['accessToken']>;
  refresh: Mock<HostBridge['refresh']>;
  wipe: Mock<HostBridge['wipe']>;
  signOut: Mock<HostBridge['signOut']>;
}

/** A host bridge whose spies are returned apart from it, so no method is read off the object. */
function bridge(overrides: Partial<Calls> = {}): HostBridge & { calls: Calls } {
  const calls: Calls = {
    accessToken: vi.fn<HostBridge['accessToken']>().mockResolvedValue({ token: 'a1', expiresIn: 900 }),
    refresh: vi.fn<HostBridge['refresh']>().mockResolvedValue('refreshed'),
    wipe: vi.fn<HostBridge['wipe']>().mockResolvedValue(),
    signOut: vi.fn<HostBridge['signOut']>().mockResolvedValue(),
    ...overrides,
  };
  return { state: vi.fn(), connect: vi.fn(), signIn: vi.fn(), cancelSignIn: vi.fn(), openExternal: vi.fn(), ...calls, calls };
}

function make(b: HostBridge, now = () => 1_000_000) {
  const onSignedOut = vi.fn();
  const onUpgradeRequired = vi.fn();
  const clearLocalData = vi.fn<() => Promise<void>>().mockResolvedValue();
  const transport = createHostTransport({
    bridge: b,
    clientVersion: '0.1.0',
    origin: 'https://h.example',
    onSignedOut,
    onUpgradeRequired,
    clearLocalData,
    now,
  });
  return { transport, onSignedOut, onUpgradeRequired, clearLocalData };
}

describe('createHostTransport', () => {
  it('caches the access token in memory until shortly before it expires', async () => {
    const b = bridge();
    let t = 1_000_000;
    const { transport } = make(b, () => t);
    expect(await transport.accessToken()).toBe('a1');
    expect(await transport.accessToken()).toBe('a1');
    expect(b.calls.accessToken).toHaveBeenCalledTimes(1);
    t += 900_000 - 29_000; // inside the 30 s skew
    await transport.accessToken();
    expect(b.calls.accessToken).toHaveBeenCalledTimes(2);
  });

  it('coalesces concurrent token reads', async () => {
    const b = bridge();
    const { transport } = make(b);
    await Promise.all([transport.accessToken(), transport.accessToken(), transport.accessToken()]);
    expect(b.calls.accessToken).toHaveBeenCalledTimes(1);
  });

  it('reports signed out when the host has no token', async () => {
    const { transport } = make(bridge({ accessToken: vi.fn().mockResolvedValue(null) }));
    expect(await transport.accessToken()).toBeUndefined();
  });

  it('refresh is single-flight, because refresh tokens rotate', async () => {
    const b = bridge({
      accessToken: vi
        .fn<HostBridge['accessToken']>()
        .mockResolvedValueOnce({ token: 'a1', expiresIn: 900 })
        .mockResolvedValue({ token: 'a2', expiresIn: 900 }),
    });
    const { transport } = make(b);
    await transport.accessToken();
    const results = await Promise.all([transport.refresh('a1'), transport.refresh('a1'), transport.refresh('a1')]);
    expect(results).toEqual(['refreshed', 'refreshed', 'refreshed']);
    expect(b.calls.refresh).toHaveBeenCalledTimes(1);
    expect(await transport.accessToken()).toBe('a2');
  });

  it('a caller holding an already-replaced token does not trigger another exchange', async () => {
    const b = bridge({
      accessToken: vi
        .fn<HostBridge['accessToken']>()
        .mockResolvedValueOnce({ token: 'a1', expiresIn: 900 })
        .mockResolvedValue({ token: 'a2', expiresIn: 900 }),
    });
    const { transport } = make(b);
    await transport.accessToken();
    await transport.refresh('a1');
    await transport.refresh('a1'); // late 401 from a request sent with a1
    expect(b.calls.refresh).toHaveBeenCalledTimes(1);
  });

  it('refresh reports ended when the server refused the session', async () => {
    const { transport } = make(bridge({ refresh: vi.fn().mockResolvedValue('ended') }));
    expect(await transport.refresh('a1')).toBe('ended');
  });

  it('a transient failure is unavailable, never a sign-out', async () => {
    const onlyHost = bridge({ refresh: vi.fn().mockRejectedValue(new Error('ipc')) });
    const { transport, onSignedOut } = make(onlyHost);
    expect(await transport.refresh('a1')).toBe('unavailable');
    const down = make(bridge({ refresh: vi.fn().mockResolvedValue('unavailable') }));
    expect(await down.transport.refresh('a1')).toBe('unavailable');
    expect(onSignedOut).not.toHaveBeenCalled();
    expect(down.onSignedOut).not.toHaveBeenCalled();
  });

  it('signOut wipes through the host once, then reports; later reads see no token', async () => {
    const b = bridge();
    const { transport, onSignedOut } = make(b);
    await transport.accessToken();
    transport.signOut('device_revoked');
    transport.signOut('refresh_failed');
    await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledTimes(1));
    expect(onSignedOut).toHaveBeenCalledWith('device_revoked');
    expect(b.calls.wipe).toHaveBeenCalledTimes(1);
    expect(await transport.accessToken()).toBeUndefined();
    expect(await transport.refresh('a1')).toBe('ended');
  });

  it('declares the client version header and an EventSource factory', () => {
    const { transport } = make(bridge());
    expect(transport.headers).toEqual({ 'X-Client-Version': '0.1.0' });
    expect(typeof transport.createEventSource).toBe('function');
  });

  it('device_revoked wipes the webview data as well as the host', async () => {
    const b = bridge();
    const { transport, onSignedOut, clearLocalData } = make(b);
    transport.signOut('device_revoked');
    await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledWith('device_revoked'));
    expect(clearLocalData).toHaveBeenCalledTimes(1);
    expect(b.calls.wipe).toHaveBeenCalledTimes(1);
  });

  it('a refresh_failed end goes through the host sign-out (revoke), never a bare wipe', async () => {
    const b = bridge();
    const { transport, onSignedOut } = make(b);
    transport.signOut('refresh_failed');
    await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledWith('refresh_failed'));
    expect(b.calls.signOut).toHaveBeenCalledTimes(1);
    expect(b.calls.wipe).not.toHaveBeenCalled();
  });

  it('logout revokes through the host, clears local data, then reports', async () => {
    const b = bridge();
    const order: string[] = [];
    b.calls.signOut.mockImplementation(() => {
      order.push('host');
      return Promise.resolve();
    });
    const { transport, onSignedOut, clearLocalData } = make(b);
    clearLocalData.mockImplementation(() => {
      order.push('local');
      return Promise.resolve();
    });
    onSignedOut.mockImplementation(() => order.push('reload'));
    await transport.logout?.();
    expect(order).toEqual(['local', 'host', 'reload']);
    expect(onSignedOut).toHaveBeenCalledWith('logout');
    expect(b.calls.wipe).not.toHaveBeenCalled();
    expect(await transport.accessToken()).toBeUndefined();
  });

  it('logout still returns to the sign-in screen when the host call fails', async () => {
    const b = bridge();
    b.calls.signOut.mockRejectedValue('credentials file locked');
    const { transport, onSignedOut } = make(b);
    await transport.logout?.();
    expect(onSignedOut).toHaveBeenCalledWith('logout');
  });

  it('a 426 from the token endpoint reaches onUpgradeRequired and is not a sign-out', async () => {
    const { transport, onUpgradeRequired, onSignedOut } = make(
      bridge({ refresh: vi.fn().mockResolvedValue('upgrade_required') }),
    );
    expect(await transport.refresh('a1')).toBe('unavailable');
    expect(onUpgradeRequired).toHaveBeenCalledTimes(1);
    expect(onSignedOut).not.toHaveBeenCalled();
  });
});
