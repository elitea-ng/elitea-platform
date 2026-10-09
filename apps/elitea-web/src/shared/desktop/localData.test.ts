import { afterEach, describe, expect, it, vi } from 'vitest';

import { installWebStorageShim } from '../../test/webstorage';

installWebStorageShim();

import { clearAllLocalData } from './localData';

afterEach(() => {
  vi.unstubAllGlobals();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

describe('clearAllLocalData', () => {
  it('clears the el.* namespace, raw keys in both areas, and every IndexedDB database', async () => {
    window.localStorage.setItem('el.project.id', '1');
    window.localStorage.setItem('raw-key', 'x'); // outside the namespace: the sweep alone misses it
    window.sessionStorage.setItem('other', 'y');
    const deleted: string[] = [];
    vi.stubGlobal('indexedDB', {
      databases: () => Promise.resolve([{ name: 'a' }, { name: 'b' }, {}]),
      deleteDatabase: (name: string) => {
        deleted.push(name);
        const request: { onsuccess?: () => void } = {};
        queueMicrotask(() => request.onsuccess?.());
        return request;
      },
    });

    await clearAllLocalData();

    expect(window.localStorage.length).toBe(0);
    expect(window.sessionStorage.length).toBe(0);
    expect(deleted).toEqual(['a', 'b']);
  });

  it('survives an environment without indexedDB.databases()', async () => {
    window.localStorage.setItem('k', 'v');
    vi.stubGlobal('indexedDB', {});
    await expect(clearAllLocalData()).resolves.toBeUndefined();
    expect(window.localStorage.length).toBe(0);
  });
});
