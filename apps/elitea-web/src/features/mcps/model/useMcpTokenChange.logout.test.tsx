import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createStorage } from '@/shared/lib/storage';

import { getLogoutMarkerEventKey, getLogoutMarkerStorageKey } from '../lib/logoutSync';
import { getStorageKey, setAccessToken } from '../lib/storage';
import { useMcpTokenChange } from './useMcpTokenChange';

afterEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  vi.restoreAllMocks();
});

describe('useMcpTokenChange', () => {
  it('removes a stale session token after another tab publishes logout', async () => {
    const serverUrl = 'https://cross-tab.example.test';
    vi.spyOn(Date, 'now').mockReturnValue(100);
    setAccessToken(serverUrl, 'stale-token', null, undefined, undefined, undefined);
    const { result } = renderHook(() => useMcpTokenChange(serverUrl));
    expect(result.current.isLoggedIn).toBe(true);

    const storageKey = getStorageKey({ serverUrl })!;
    const markerKey = getLogoutMarkerStorageKey(storageKey)!;
    createStorage('local').set(markerKey, '200');
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', {
        key: getLogoutMarkerEventKey(storageKey),
        newValue: '200',
      }));
    });

    await waitFor(() => expect(result.current.isLoggedIn).toBe(false));
  });
});
