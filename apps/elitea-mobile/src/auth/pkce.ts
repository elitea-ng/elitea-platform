import * as Crypto from 'expo-crypto';

/**
 * PKCE (RFC 7636, S256 only) and the `state` value of the native
 * authorization request (RFC 8252). Randomness and SHA-256 come from
 * expo-crypto (platform CSPRNG and digest); both are injectable for tests.
 */

const B64URL = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_';

/** Unpadded base64url (RFC 4648 §5). */
export function base64Url(bytes: Uint8Array): string {
  let out = '';
  let i = 0;
  for (; i + 2 < bytes.length; i += 3) {
    const n = (bytes[i] << 16) | (bytes[i + 1] << 8) | bytes[i + 2];
    out += B64URL[(n >> 18) & 63] + B64URL[(n >> 12) & 63] + B64URL[(n >> 6) & 63] + B64URL[n & 63];
  }
  const rest = bytes.length - i;
  if (rest === 1) {
    const n = bytes[i] << 16;
    out += B64URL[(n >> 18) & 63] + B64URL[(n >> 12) & 63];
  } else if (rest === 2) {
    const n = (bytes[i] << 16) | (bytes[i + 1] << 8);
    out += B64URL[(n >> 18) & 63] + B64URL[(n >> 12) & 63] + B64URL[(n >> 6) & 63];
  }
  return out;
}

export interface PkceDeps {
  randomBytes?: (count: number) => Uint8Array;
  sha256?: (data: Uint8Array) => Promise<Uint8Array>;
}

const defaultRandom = (count: number) => Crypto.getRandomBytes(count);
const defaultSha256 = async (data: Uint8Array) =>
  new Uint8Array(await Crypto.digest(Crypto.CryptoDigestAlgorithm.SHA256, data as Uint8Array<ArrayBuffer>));

function asciiBytes(text: string): Uint8Array {
  const out = new Uint8Array(text.length);
  for (let i = 0; i < text.length; i++) out[i] = text.charCodeAt(i);
  return out;
}

/** The S256 challenge of a verifier: BASE64URL(SHA256(ASCII(verifier))). */
export async function codeChallenge(verifier: string, deps: PkceDeps = {}): Promise<string> {
  return base64Url(await (deps.sha256 ?? defaultSha256)(asciiBytes(verifier)));
}

/** A verifier of 43 unreserved characters (32 random bytes) and its S256 challenge. */
export async function createPkce(deps: PkceDeps = {}): Promise<{ verifier: string; challenge: string }> {
  const verifier = base64Url((deps.randomBytes ?? defaultRandom)(32));
  return { verifier, challenge: await codeChallenge(verifier, deps) };
}

/** An unguessable `state` (32 random bytes, 43 base64url characters). */
export function createState(deps: Pick<PkceDeps, 'randomBytes'> = {}): string {
  return base64Url((deps.randomBytes ?? defaultRandom)(32));
}

/** Length-independent-time string comparison for the `state` check. */
export function constantTimeEqual(a: string, b: string): boolean {
  let diff = a.length ^ b.length;
  const length = Math.max(a.length, b.length);
  for (let i = 0; i < length; i++) diff |= (a.charCodeAt(i) || 0) ^ (b.charCodeAt(i) || 0);
  return diff === 0;
}
