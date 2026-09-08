import { beforeEach, describe, expect, it } from 'vitest';

import { STORAGE_NAMESPACE, clearNamespace } from '@/shared/lib/storage';

import {
  createWikiChatStorage,
  createWikiConversationKey,
  forgetLocalWikiMessages,
  readLocalWikiMessages,
} from './chatStorage';

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

describe('createWikiChatStorage', () => {
  it('writes only inside the namespace, so a logout sweeps it', () => {
    // The defect this replaces: `deepwiki-chat-1-2` on the bare global
    // survives clearNamespace() and the next user finds the last one's
    // questions (issue #22).
    const storage = createWikiChatStorage(1, 2);
    storage.saveCapability('research');
    createWikiConversationKey(1, 2, () => 'key-1').read();

    // Enumerated through the Storage API rather than Object.keys: jsdom does
    // not expose entries as own properties, so Object.keys returns [] and the
    // assertion would pass against a store full of un-namespaced keys.
    const keys = allStorageKeys();
    expect(keys.length).toBeGreaterThan(0);
    for (const key of keys) {
      expect(key.startsWith(STORAGE_NAMESPACE)).toBe(true);
    }

    clearNamespace();
    expect(allStorageKeys()).toHaveLength(0);
  });

  it('reads an unrecognised capability as none', () => {
    window.localStorage.setItem(`${STORAGE_NAMESPACE}deepwiki.chat.capability.1.2`, 'telepathy');
    expect(createWikiChatStorage(1, 2).loadCapability()).toBeNull();

    createWikiChatStorage(1, 2).saveCapability('research');
    expect(createWikiChatStorage(1, 2).loadCapability()).toBe('research');
  });

  // THE TRANSCRIPT IS NOT WRITTEN HERE ANY MORE. elitea-main records both
  // turns of a wiki chat, so a copy kept in the browser would be a second
  // record to disagree with the first — and the one that is invisible to
  // every other device.
  it('never writes a transcript of its own', () => {
    const storage = createWikiChatStorage(1, 2);
    storage.saveCapability('ask');
    expect(allStorageKeys()).not.toContain(`${STORAGE_NAMESPACE}deepwiki.chat.1.2`);
  });
});

describe('createWikiConversationKey', () => {
  // The key is how this browser says "the same chat as last time" to the
  // server. A key that changed per read would file every question into a new
  // conversation.
  it('mints one key and then keeps returning it', () => {
    let minted = 0;
    const key = createWikiConversationKey(1, 2, () => `key-${(minted += 1)}`);

    expect(key.read()).toBe('key-1');
    expect(key.read()).toBe('key-1');
    // A fresh reader on the same wiki resumes the same conversation, which is
    // what makes a reload land back in it.
    expect(createWikiConversationKey(1, 2, () => 'unused').read()).toBe('key-1');
  });

  it('keeps one conversation per wiki', () => {
    createWikiConversationKey(1, 2, () => 'key-a').read();
    createWikiConversationKey(1, 3, () => 'key-b').read();

    expect(createWikiConversationKey(1, 2, () => 'unused').read()).toBe('key-a');
    expect(createWikiConversationKey(1, 3, () => 'unused').read()).toBe('key-b');
  });

  // "Clear" now means "start a new conversation", not "erase the old one":
  // the previous conversation stays stored and readable on the server.
  it('renews to a new key', () => {
    let minted = 0;
    const key = createWikiConversationKey(1, 2, () => `key-${(minted += 1)}`);

    expect(key.read()).toBe('key-1');
    expect(key.renew()).toBe('key-2');
    expect(key.read()).toBe('key-2');
  });

  // `resolve` says whether THIS call minted the key, and that is what the
  // drawer keys adoption on: a browser that already had one has answered the
  // question, and adopting over it would resurrect a cleared conversation.
  it('reports the mint, and only the mint', () => {
    const key = createWikiConversationKey(1, 2, () => 'key-1');
    expect(key.resolve()).toEqual({ key: 'key-1', minted: true });
    expect(key.resolve()).toEqual({ key: 'key-1', minted: false });
  });

  it('reports no mint after a renew, so a cleared conversation stays cleared', () => {
    let minted = 0;
    const key = createWikiConversationKey(1, 2, () => `key-${(minted += 1)}`);
    key.resolve();
    key.renew();
    expect(key.resolve()).toEqual({ key: 'key-2', minted: false });
  });

  it('adopt takes over a key minted somewhere else', () => {
    const key = createWikiConversationKey(1, 2, () => 'key-local');
    key.adopt('key-from-the-laptop');
    expect(key.read()).toBe('key-from-the-laptop');
    expect(key.resolve()).toEqual({ key: 'key-from-the-laptop', minted: false });
  });
});

describe('the local conversation left by the pre-server drawer', () => {
  const legacyKey = `${STORAGE_NAMESPACE}deepwiki.chat.1.2`;

  it('is still readable, so nothing disappears on upgrade', () => {
    window.localStorage.setItem(
      legacyKey,
      JSON.stringify([{ role: 'user', content: 'asked before the server kept history' }]),
    );
    expect(readLocalWikiMessages(1, 2)).toEqual([
      { role: 'user', content: 'asked before the server kept history' },
    ]);
  });

  it('reads a corrupt one as an empty one', () => {
    window.localStorage.setItem(legacyKey, '{not json');
    expect(readLocalWikiMessages(1, 2)).toEqual([]);

    // Valid JSON of the WRONG SHAPE is just as bad: rendering an object as a
    // message list crashes the drawer on open.
    window.localStorage.setItem(legacyKey, '{"a":1}');
    expect(readLocalWikiMessages(1, 2)).toEqual([]);
  });

  it('is forgotten only when something asks it to be', () => {
    window.localStorage.setItem(legacyKey, JSON.stringify([{ role: 'user', content: 'old' }]));
    expect(readLocalWikiMessages(1, 2)).toHaveLength(1);

    forgetLocalWikiMessages(1, 2);
    expect(readLocalWikiMessages(1, 2)).toEqual([]);
    // And only this wiki's.
    expect(allStorageKeys()).not.toContain(legacyKey);
  });
});
