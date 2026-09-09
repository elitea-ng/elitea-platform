import { describe, expect, it } from 'vitest';

import { buildSharedConversationUrl } from './shareUrl';

/*
 * The link a share token opens is the one string in this app whose only
 * reader is somebody else, so nothing the owner sees can report it wrong.
 * These cases are the address itself.
 */
describe('buildSharedConversationUrl', () => {
  it('mounts the page under the router basename', () => {
    expect(
      buildSharedConversationUrl({ origin: 'https://elitea.example', basename: '/app/', token: 'tok-1' }),
    ).toBe('https://elitea.example/app/shared/chat/tok-1');
  });

  it('accepts a basename with no trailing slash', () => {
    expect(
      buildSharedConversationUrl({ origin: 'https://elitea.example', basename: '/app', token: 'tok-1' }),
    ).toBe('https://elitea.example/app/shared/chat/tok-1');
  });

  it('builds a root-relative link when the app is mounted at the root (dev)', () => {
    expect(
      buildSharedConversationUrl({ origin: 'http://localhost:5173', basename: '', token: 'tok-1' }),
    ).toBe('http://localhost:5173/shared/chat/tok-1');
  });

  it('never emits a double slash between the origin and the path', () => {
    const url = buildSharedConversationUrl({ origin: 'https://elitea.example', basename: '/app/', token: 'tok-1' });
    expect(url.slice('https://'.length)).not.toContain('//');
  });

  it('encodes the token rather than pasting it into the path', () => {
    expect(
      buildSharedConversationUrl({ origin: 'https://elitea.example', basename: '/app/', token: 'a/b?c' }),
    ).toBe('https://elitea.example/app/shared/chat/a%2Fb%3Fc');
  });
});
