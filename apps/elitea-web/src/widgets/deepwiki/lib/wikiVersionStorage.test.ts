import { beforeEach, describe, expect, it } from 'vitest';

import { STORAGE_NAMESPACE, clearNamespace } from '@/shared/lib/storage';

import { createWikiVersionStorage } from './wikiVersionStorage';

function allStorageKeys(): string[] {
  const keys: string[] = [];
  for (let index = 0; index < window.localStorage.length; index += 1) {
    const key = window.localStorage.key(index);
    if (key !== null) keys.push(key);
  }
  return keys;
}

beforeEach(() => {
  window.localStorage.clear();
});

describe('createWikiVersionStorage', () => {
  it('gives back the wiki id it was given', () => {
    const storage = createWikiVersionStorage(7, 42);
    expect(storage.load()).toBeNull();
    storage.save('acme--notes-service--dev');
    expect(createWikiVersionStorage(7, 42).load()).toBe('acme--notes-service--dev');
  });

  it('keeps one choice per (project, toolkit)', () => {
    createWikiVersionStorage(7, 42).save('a--b--main');
    createWikiVersionStorage(7, 43).save('c--d--main');
    createWikiVersionStorage(8, 42).save('e--f--main');
    expect(createWikiVersionStorage(7, 42).load()).toBe('a--b--main');
    expect(createWikiVersionStorage(7, 43).load()).toBe('c--d--main');
    expect(createWikiVersionStorage(8, 42).load()).toBe('e--f--main');
  });

  it('forgets a choice when it is cleared', () => {
    const storage = createWikiVersionStorage(7, 42);
    storage.save('a--b--main');
    storage.clear();
    expect(storage.load()).toBeNull();
  });

  it('writes only inside the namespace the logout sweep clears', () => {
    // The defect this avoids: the legacy `deepwiki.selected_manifest.…` key was
    // written straight onto `localStorage`, and a raw key survives sign-out —
    // the next user of the machine opens the previous one's reading position
    // (issue #22). Enumerated through the Storage API rather than Object.keys,
    // which returns [] in jsdom and would pass against a store full of
    // un-namespaced keys.
    createWikiVersionStorage(7, 42).save('acme--notes-service--dev');
    const keys = allStorageKeys();
    expect(keys.length).toBeGreaterThan(0);
    for (const key of keys) expect(key.startsWith(STORAGE_NAMESPACE)).toBe(true);

    clearNamespace();
    expect(createWikiVersionStorage(7, 42).load()).toBeNull();
  });
});
