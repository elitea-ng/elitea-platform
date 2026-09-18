/**
 * draftPersistence — #935/ELITEA-0623 regression coverage.
 *
 * The root-cause fix this guards: a plain module-scope variable does not
 * survive a real top-level browser navigation (confirmed live against
 * `support.entrypoints.spec.ts`'s "shared across a page navigation" case —
 * its `page.goto(...)` calls are genuine document reloads, which wipe any
 * module-scope JS state along with the whole runtime). The draft must
 * therefore live in `sessionStorage`, which these tests exercise directly
 * by re-importing the module fresh each time — the same "new JS
 * environment" a real navigation produces — while asserting the value
 * still round-trips through the underlying storage.
 */
import { afterEach, describe, expect, it } from 'vitest';

import { installWebStorageShim } from '../../../test/webstorage';

installWebStorageShim();

import { getPersistedDraft, setPersistedDraft } from './draftPersistence';

afterEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

describe('getPersistedDraft / setPersistedDraft', () => {
  it('defaults to empty when nothing was ever persisted', () => {
    expect(getPersistedDraft()).toBe('');
  });

  it('round-trips a value through sessionStorage under the `el.` namespace', () => {
    setPersistedDraft('unsent message');
    expect(window.sessionStorage.getItem('el.support.draft')).toBe('unsent message');
    expect(getPersistedDraft()).toBe('unsent message');
  });

  it('clearing to the empty string removes the key rather than storing it', () => {
    setPersistedDraft('something');
    setPersistedDraft('');
    expect(window.sessionStorage.getItem('el.support.draft')).toBeNull();
    expect(getPersistedDraft()).toBe('');
  });

  it('does NOT write to localStorage — the draft is per-tab, not cross-tab', () => {
    setPersistedDraft('draft text');
    expect(window.localStorage.getItem('el.support.draft')).toBeNull();
  });

  it('reads back a value that was written directly to sessionStorage, not just through setPersistedDraft', () => {
    // This is the property a plain module-scope variable could never have:
    // the value has to come from storage, not from in-memory JS state, so a
    // fresh JS runtime (a real top-level navigation) still finds it.
    window.sessionStorage.setItem('el.support.draft', 'written by another code path');
    expect(getPersistedDraft()).toBe('written by another code path');
  });
});
