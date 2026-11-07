/**
 * The cloud browser session of a run (ADR-0049): `elitea-br-` and the first
 * 24 hex digits of SHA-256 over `"elitea.browser-session\0"` and the
 * execution id. The worker derives it (`browser_run/session.rs`
 * `session_name`) and Main accepts a session report only for that name
 * (`domain/browsersession` `NameFor`), so a client that knows the run knows
 * its session without another read.
 *
 * SHA-256 is computed here, synchronously, instead of with
 * `crypto.subtle`: that exists only in a secure context, and a plain-http
 * deployment would then show no browser at all.
 */

const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

const rotr = (value: number, bits: number): number => (value >>> bits) | (value << (32 - bits));

/** The message schedule of one 64-byte block. */
function schedule(w: Uint32Array, view: DataView, block: number): void {
  for (let i = 0; i < 16; i += 1) w[i] = view.getUint32(block + i * 4);
  for (let i = 16; i < 64; i += 1) {
    const a = w[i - 15] ?? 0;
    const b = w[i - 2] ?? 0;
    const s0 = rotr(a, 7) ^ rotr(a, 18) ^ (a >>> 3);
    const s1 = rotr(b, 17) ^ rotr(b, 19) ^ (b >>> 10);
    w[i] = ((w[i - 16] ?? 0) + s0 + (w[i - 7] ?? 0) + s1) >>> 0;
  }
}

/** One compression round over the block at `block`, into `hash`. */
function compress(hash: Uint32Array, w: Uint32Array, view: DataView, block: number): void {
  schedule(w, view, block);
  const v = Array.from(hash);
  for (let i = 0; i < 64; i += 1) {
    const [a, b, c, d, e, f, g, h] = v as [number, number, number, number, number, number, number, number];
    const t1 = (h + (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) + ((e & f) ^ (~e & g)) + (K[i] ?? 0) + (w[i] ?? 0)) >>> 0;
    const t2 = ((rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) + ((a & b) ^ (a & c) ^ (b & c))) >>> 0;
    v.splice(0, 8, (t1 + t2) >>> 0, a, b, c, (d + t1) >>> 0, e, f, g);
  }
  for (let i = 0; i < 8; i += 1) hash[i] = ((hash[i] ?? 0) + (v[i] ?? 0)) >>> 0;
}

/** The SHA-256 of `bytes`, as lowercase hex. */
export function sha256Hex(bytes: Uint8Array): string {
  const bitLength = bytes.length * 8;
  const padded = new Uint8Array((((bytes.length + 9 + 63) >> 6) << 6));
  padded.set(bytes);
  padded[bytes.length] = 0x80;
  const view = new DataView(padded.buffer);
  view.setUint32(padded.length - 8, Math.floor(bitLength / 0x1_0000_0000));
  view.setUint32(padded.length - 4, bitLength >>> 0);
  const hash = new Uint32Array([0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19]);
  const w = new Uint32Array(64);
  for (let block = 0; block < padded.length; block += 64) compress(hash, w, view, block);
  return Array.from(hash, (word) => word.toString(16).padStart(8, '0')).join('');
}

/** The shape Main admits on the view route (`ValidName`). */
export const BROWSER_SESSION_NAME = /^elitea-br-[0-9a-f]{24}$/;

/** The cloud browser session name of the run `executionId`. */
export function browserSessionNameFor(executionId: string): string {
  const digest = sha256Hex(new TextEncoder().encode(`elitea.browser-session\u0000${executionId}`));
  return `elitea-br-${digest.slice(0, 24)}`;
}
