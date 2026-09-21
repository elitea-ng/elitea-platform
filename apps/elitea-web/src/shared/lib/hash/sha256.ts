/**
 * shared/lib/hash/sha256.ts — a synchronous SHA-256 over a UTF-8 string.
 *
 * WHY NOT `crypto.subtle`. The platform digest is asynchronous, and the one
 * caller that needs this is a PURE REDUCER (`chatStreamToolOutputChunks.ts`):
 * a chunked tool result is verified against the digest its producer stamped on
 * every chunk, inside the same synchronous frame-reduction that assembles it.
 * Making that reducer async would make every caller of `applyChatStreamFrame`
 * async — the whole chat stream — to avoid forty lines of arithmetic, and it
 * would open a window in which the UI shows an unverified assembly as though
 * it were verified.
 *
 * `features/mcps/lib/crypto.ts` keeps its async `sha256` for PKCE, where the
 * call is already inside an async OAuth flow and the platform primitive is the
 * right one.
 *
 * The implementation is FIPS 180-4 verbatim. It is not a security boundary:
 * the digest it checks was produced by the server that also sent the bytes, so
 * this detects a LOST OR REORDERED chunk, not a hostile one.
 */

const ROUND_CONSTANTS = Uint32Array.from([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

function rotateRight(value: number, bits: number): number {
  return (value >>> bits) | (value << (32 - bits));
}

/** The 64-byte padded message: the bytes, `0x80`, zeroes, then the bit length. */
function padded(bytes: Uint8Array): Uint8Array {
  const blocks = Math.floor((bytes.length + 8) / 64) + 1;
  const result = new Uint8Array(blocks * 64);
  result.set(bytes);
  result[bytes.length] = 0x80;
  const bitLength = bytes.length * 8;
  const view = new DataView(result.buffer);
  // The high word of the 64-bit length: a string long enough to need it cannot
  // reach this reducer (the frame ceiling is megabytes), but writing only the
  // low word would be a silent wrong answer rather than a refused one.
  view.setUint32(result.length - 8, Math.floor(bitLength / 0x1_0000_0000), false);
  view.setUint32(result.length - 4, bitLength >>> 0, false);
  return result;
}

/**
 * One word of a typed array.
 *
 * `noUncheckedIndexedAccess` types every index read as possibly undefined, and
 * writing `?? 0` at each of the two dozen reads below pushes the compression
 * function past the complexity budget for no behavioural reason — the indices
 * are all in range by construction. One helper says that once.
 */
function word(values: Uint32Array, index: number): number {
  return values[index] ?? 0;
}

/** The 64-word message schedule of one 512-bit block. */
function scheduleOf(view: DataView, offset: number, schedule: Uint32Array): void {
  for (let index = 0; index < 16; index++) schedule[index] = view.getUint32(offset + index * 4, false);
  for (let index = 16; index < 64; index++) {
    const previous = word(schedule, index - 15);
    const recent = word(schedule, index - 2);
    const s0 = rotateRight(previous, 7) ^ rotateRight(previous, 18) ^ (previous >>> 3);
    const s1 = rotateRight(recent, 17) ^ rotateRight(recent, 19) ^ (recent >>> 10);
    schedule[index] = (word(schedule, index - 16) + s0 + word(schedule, index - 7) + s1) >>> 0;
  }
}

/** One block's compression, folded into the running state. */
function compress(state: Uint32Array, schedule: Uint32Array): void {
  let a = word(state, 0);
  let b = word(state, 1);
  let c = word(state, 2);
  let d = word(state, 3);
  let e = word(state, 4);
  let f = word(state, 5);
  let g = word(state, 6);
  let h = word(state, 7);
  for (let index = 0; index < 64; index++) {
    const s1 = rotateRight(e, 6) ^ rotateRight(e, 11) ^ rotateRight(e, 25);
    const choose = (e & f) ^ (~e & g);
    const temp1 = (h + s1 + choose + word(ROUND_CONSTANTS, index) + word(schedule, index)) >>> 0;
    const s0 = rotateRight(a, 2) ^ rotateRight(a, 13) ^ rotateRight(a, 22);
    const majority = (a & b) ^ (a & c) ^ (b & c);
    const temp2 = (s0 + majority) >>> 0;
    h = g;
    g = f;
    f = e;
    e = (d + temp1) >>> 0;
    d = c;
    c = b;
    b = a;
    a = (temp1 + temp2) >>> 0;
  }
  const round = Uint32Array.from([a, b, c, d, e, f, g, h]);
  for (let index = 0; index < 8; index++) {
    state[index] = (word(state, index) + word(round, index)) >>> 0;
  }
}

/** Lowercase hex SHA-256 of `value`'s UTF-8 encoding. */
export function sha256Hex(value: string): string {
  const message = padded(new TextEncoder().encode(value));
  const state = Uint32Array.from([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ]);
  const schedule = new Uint32Array(64);
  const view = new DataView(message.buffer);

  for (let offset = 0; offset < message.length; offset += 64) {
    scheduleOf(view, offset, schedule);
    compress(state, schedule);
  }

  let hex = '';
  for (const word of state) hex += word.toString(16).padStart(8, '0');
  return hex;
}
