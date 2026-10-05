import { describe, expect, it } from 'vitest';

import { createMemoryStorage } from './webstorage.testshim';

/**
 * The shim has to behave like a real `Storage`, including the part a test
 * reaches for without thinking: a real `Storage` exposes its KEYS as its own
 * enumerable properties, so `Object.keys(localStorage)` lists the stored keys.
 * The first shim was a plain object literal of methods, so on Node >= 25 —
 * where the shim is what `window.localStorage` actually is — `Object.keys`
 * returned `['length', 'key', 'getItem', ...]` and a test asserting a key was
 * present failed (mcpDiscoveryWiring "clears only the selected toolkit grant").
 */
describe('createMemoryStorage', () => {
  it('enumerates stored keys, and only stored keys, like a real Storage', () => {
    const storage = createMemoryStorage();
    storage.setItem('alpha', '1');
    storage.setItem('beta', '2');
    expect(Object.keys(storage)).toEqual(['alpha', 'beta']);
    storage.removeItem('alpha');
    expect(Object.keys(storage)).toEqual(['beta']);
    storage.clear();
    expect(Object.keys(storage)).toEqual([]);
  });

  it('reads a stored key as a named property and keeps the Storage methods', () => {
    const storage = createMemoryStorage();
    storage.setItem('alpha', 'one');
    expect((storage as unknown as Record<string, unknown>)['alpha']).toBe('one');
    expect('alpha' in storage).toBe(true);
    expect(storage.length).toBe(1);
    expect(storage.key(0)).toBe('alpha');
    expect(storage.key(1)).toBeNull();
    expect(storage.getItem('missing')).toBeNull();
  });

  it('does not let a stored key named like a method shadow the method', () => {
    const storage = createMemoryStorage();
    storage.setItem('getItem', 'x');
    expect(storage.getItem('getItem')).toBe('x');
    expect(Object.keys(storage)).toEqual(['getItem']);
  });

  it('coerces keys and values to strings', () => {
    const storage = createMemoryStorage();
    storage.setItem(1 as unknown as string, 2 as unknown as string);
    expect(storage.getItem('1')).toBe('2');
  });
});
