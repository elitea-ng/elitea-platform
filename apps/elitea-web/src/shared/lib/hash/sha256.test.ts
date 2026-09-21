import { describe, expect, it } from 'vitest';

import { sha256Hex } from './sha256';

describe('sha256Hex', () => {
  // Published vectors, not self-comparison: a digest function compared only
  // against itself proves nothing, because both sides can be wrong the same
  // way — and this one has to agree with three OTHER implementations (Go's
  // reassembly, and both workers' emit).
  it.each([
    ['', 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'],
    ['abc', 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'],
    [
      'abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq',
      '248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1',
    ],
  ])('matches the FIPS 180-4 vector for %j', (input, expected) => {
    expect(sha256Hex(input)).toBe(expected);
  });

  // A JS string is UTF-16. Hashing its code units would disagree with every
  // other component in this system, all of which hash UTF-8 — and it would
  // disagree only for non-ASCII output, which is the hardest kind of bug to
  // notice in a chat transcript.
  it.each([
    ['é', '4a99557e4033c3539de2eb65472017cad5f9557f7a0625a09f1c3f6e2ba69c4c'],
    ['🙂', 'd06f1525f791397809f9bc98682b5c13318eca4c3123433467fd4dffda44fd14'],
    ['проверка', 'dbdd1f31e722086974ba86e32d48bea04cc01601a390091c51d76efe1d590eb2'],
  ])('hashes the UTF-8 encoding of %j', (input, expected) => {
    expect(sha256Hex(input)).toBe(expected);
  });

  // 56 bytes is where the 8-byte length field stops fitting the first block,
  // so a padding bug shows here and nowhere shorter.
  it.each([
    ['a'.repeat(56), 'b35439a4ac6f0948b6d6f9e3c6af0f5f590ce20f1bde7090ef7970686ec6738a'],
    ['a'.repeat(64), 'ffe054fe7ae0cb6dc65c3af9b61d5209f439851db43d0ba5997337df154668eb'],
    ['x'.repeat(100_000), 'd69e68988157833272305aaf21f453c800346e8a3640db6578e260215542e5d4'],
  ])('spans block boundaries (%#)', (input, expected) => {
    expect(sha256Hex(input)).toBe(expected);
  });
});
