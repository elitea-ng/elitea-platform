import { base64Url, codeChallenge, constantTimeEqual, createPkce, createState } from '@/auth/pkce';

// Node's crypto is the independent reference implementation (jest runs in node).
interface NodeHash {
  update(data: Uint8Array | string): NodeHash;
  digest(): Uint8Array;
  digest(encoding: 'base64url'): string;
}
const nodeCrypto = require('crypto') as {
  createHash(algorithm: 'sha256'): NodeHash;
  randomBytes(size: number): Uint8Array;
};
const nodeBuffer = (require('buffer') as { Buffer: { from(bytes: Uint8Array): { toString(enc: 'base64url'): string } } })
  .Buffer;
const { createHash, randomBytes } = nodeCrypto;

const sha256 = async (data: Uint8Array) => new Uint8Array(createHash('sha256').update(data).digest());

describe('PKCE', () => {
  it('matches the RFC 7636 appendix B known-answer vector', async () => {
    expect(await codeChallenge('dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk', { sha256 })).toBe(
      'E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM',
    );
  });

  it('creates a 43-character unreserved verifier whose challenge is its S256 hash', async () => {
    const { verifier, challenge } = await createPkce({
      randomBytes: (n) => new Uint8Array(randomBytes(n)),
      sha256,
    });
    expect(verifier).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(challenge).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(challenge).toBe(createHash('sha256').update(verifier).digest('base64url'));
  });

  it('encodes unpadded base64url for every remainder length', () => {
    for (const length of [0, 1, 2, 3, 4, 5, 31, 32, 33]) {
      const bytes = new Uint8Array(randomBytes(length));
      expect(base64Url(bytes)).toBe(nodeBuffer.from(bytes).toString('base64url'));
    }
  });
});

describe('state', () => {
  it('is 43 base64url characters from 32 random bytes', () => {
    const state = createState({ randomBytes: (n) => new Uint8Array(n).fill(7) });
    expect(state).toHaveLength(43);
    expect(state).toMatch(/^[A-Za-z0-9_-]+$/);
  });

  it('compares exactly', () => {
    expect(constantTimeEqual('abc', 'abc')).toBe(true);
    expect(constantTimeEqual('abc', 'abd')).toBe(false);
    expect(constantTimeEqual('abc', 'ab')).toBe(false);
    expect(constantTimeEqual('', '')).toBe(true);
  });
});
