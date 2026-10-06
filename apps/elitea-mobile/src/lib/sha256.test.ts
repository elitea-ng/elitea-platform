import { sha256Hex } from '@/lib/sha256';

// FIPS 180-4 / NIST CAVP vectors.
it.each([
  ['', 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'],
  ['abc', 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'],
  [
    'abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq',
    '248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1',
  ],
  ['a'.repeat(1000), '41edece42d63e8d9bf515a9ba6932e1c20cbc9f5a5d134645adb5db1b9737ea3'],
])('sha256Hex(%#)', (input, digest) => {
  expect(sha256Hex(input)).toBe(digest);
});

it('hashes UTF-8, not UTF-16 code units', () => {
  // echo -n 'é' | shasum -a 256
  expect(sha256Hex('é')).toBe('4a99557e4033c3539de2eb65472017cad5f9557f7a0625a09f1c3f6e2ba69c4c');
});
