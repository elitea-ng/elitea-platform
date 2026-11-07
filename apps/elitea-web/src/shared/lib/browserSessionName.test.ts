import { describe, expect, it } from 'vitest';

import { BROWSER_SESSION_NAME, browserSessionNameFor, sha256Hex } from './browserSessionName';

const hex = (text: string): string => sha256Hex(new TextEncoder().encode(text));

describe('sha256Hex', () => {
  it('matches the standard vectors', () => {
    expect(hex('')).toBe('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');
    expect(hex('abc')).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
    // Two blocks, and a message whose padding needs a block of its own.
    expect(hex('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq')).toBe('248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1');
    expect(hex('a'.repeat(1000))).toBe('41edece42d63e8d9bf515a9ba6932e1c20cbc9f5a5d134645adb5db1b9737ea3');
  });
});

describe('browserSessionNameFor', () => {
  it("derives Main's and the worker's session name (shasum of the same bytes)", () => {
    expect(browserSessionNameFor('exec-1')).toBe('elitea-br-a6d26444007f61016ea36439');
    expect(browserSessionNameFor('0123456789abcdef0123456789abcdef')).toBe('elitea-br-86b40f0fcfcf8dcc546fcf51');
  });

  it('has the shape the view route admits, one per run', () => {
    expect(browserSessionNameFor('exec-1')).toMatch(BROWSER_SESSION_NAME);
    expect(browserSessionNameFor('exec-1')).not.toBe(browserSessionNameFor('exec-2'));
  });
});
