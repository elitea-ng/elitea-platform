// A minimal raster writer: PNG (RGB or RGBA, 8-bit), .ico and .icns with PNG
// entries, and a plain geometric placeholder shape. Used by the brand-check
// tests for fixtures and by gen-sample.mjs for brand/sample's placeholder
// icons. Node's standard library only (zlib for the PNG stream).

import { deflateSync, inflateSync } from 'node:zlib';

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();

function crc32(buf) {
  let c = 0xffffffff;
  for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type, 'latin1'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(td));
  return Buffer.concat([len, td, crc]);
}

/**
 * Encode `pixels` (a function (x, y) → [r, g, b, a] in 0–255) as a PNG of
 * `width` x `height`. `alpha: false` writes RGB (colour type 2) and drops a.
 */
export function encodePng(width, height, pixels, { alpha = true } = {}) {
  const channels = alpha ? 4 : 3;
  const raw = Buffer.alloc((width * channels + 1) * height);
  let o = 0;
  for (let y = 0; y < height; y++) {
    raw[o++] = 0; // filter: none
    for (let x = 0; x < width; x++) {
      const px = pixels(x, y);
      raw[o++] = px[0];
      raw[o++] = px[1];
      raw[o++] = px[2];
      if (alpha) raw[o++] = px[3];
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8;
  ihdr[9] = alpha ? 6 : 2;
  return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), chunk('IHDR', ihdr), chunk('IDAT', deflateSync(raw, { level: 9 })), chunk('IEND', Buffer.alloc(0))]);
}

/** A Windows .ico holding one PNG per entry of `pngs` (`[{ size, png }]`). */
export function encodeIco(pngs) {
  const head = Buffer.alloc(6 + 16 * pngs.length);
  head.writeUInt16LE(0, 0);
  head.writeUInt16LE(1, 2);
  head.writeUInt16LE(pngs.length, 4);
  let offset = head.length;
  pngs.forEach(({ size, png }, i) => {
    const at = 6 + 16 * i;
    head[at] = size >= 256 ? 0 : size;
    head[at + 1] = size >= 256 ? 0 : size;
    head.writeUInt16LE(1, at + 4); // planes
    head.writeUInt16LE(32, at + 6); // bits per pixel
    head.writeUInt32LE(png.length, at + 8);
    head.writeUInt32LE(offset, at + 12);
    offset += png.length;
  });
  return Buffer.concat([head, ...pngs.map((p) => p.png)]);
}

/** A macOS .icns holding PNG elements (`[{ type, png }]`, e.g. type 'ic10' for 1024). */
export function encodeIcns(elements) {
  const parts = elements.map(({ type, png }) => {
    const h = Buffer.alloc(8);
    h.write(type, 0, 'latin1');
    h.writeUInt32BE(png.length + 8, 4);
    return Buffer.concat([h, png]);
  });
  const total = 8 + parts.reduce((n, p) => n + p.length, 0);
  const h = Buffer.alloc(8);
  h.write('icns', 0, 'latin1');
  h.writeUInt32BE(total, 4);
  return Buffer.concat([h, ...parts]);
}

/** The PNG element type of an .icns for each edge (one per size). */
export const ICNS_PNG_TYPE = Object.freeze({ 16: 'icp4', 32: 'ic11', 64: 'ic12', 128: 'ic07', 256: 'ic08', 512: 'ic09', 1024: 'ic10' });

/** `#rrggbb` → [r, g, b]. */
export const rgb = (hex) => [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));

/**
 * The placeholder shape, as a pixel function for an edge of `size`: a disc
 * of `fg` (radius `r` of the edge, anti-aliased) on `bg` (null for
 * transparent), optionally inside a rounded square tile.
 */
export function placeholderShape(size, { fg, bg = null, r = 0.3, tile = false }) {
  const f = rgb(fg);
  const b = bg ? rgb(bg) : null;
  const c = size / 2;
  const radius = size * r;
  const corner = size * 0.22;
  const inTile = (x, y) => {
    if (!tile) return 1;
    const dx = Math.max(corner - x, 0, x - (size - corner));
    const dy = Math.max(corner - y, 0, y - (size - corner));
    return Math.min(1, Math.max(0, corner - Math.hypot(dx, dy) + 0.5));
  };
  return (x, y) => {
    const d = Math.hypot(x + 0.5 - c, y + 0.5 - c);
    const cov = Math.min(1, Math.max(0, radius - d + 0.5));
    const base = b ? [...b, 255 * inTile(x, y)] : [0, 0, 0, 0];
    if (cov === 0) return base.map(Math.round);
    const a = base[3] / 255;
    const outA = cov + a * (1 - cov);
    const mix = (i) => (outA === 0 ? 0 : (f[i] * cov + base[i] * a * (1 - cov)) / outA);
    return [mix(0), mix(1), mix(2), outA * 255].map(Math.round);
  };
}

/** A square PNG of the placeholder shape. */
export const placeholderPng = (size, opts, { alpha = true } = {}) => encodePng(size, size, placeholderShape(size, opts), { alpha });

/** A solid square PNG. */
export const solidPng = (size, hex, { alpha = false } = {}) => {
  const c = rgb(hex);
  return encodePng(size, size, () => [...c, 255], { alpha });
};

/** A plain disc SVG on a rounded tile (or alone when `bg` is null). */
export function placeholderSvg({ fg, bg = null, viewBox = 512 }) {
  const tile = bg ? `<rect width="${viewBox}" height="${viewBox}" rx="${Math.round(viewBox * 0.22)}" fill="${bg}"/>` : '';
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${viewBox} ${viewBox}">${tile}<circle cx="${viewBox / 2}" cy="${viewBox / 2}" r="${Math.round(viewBox * 0.3)}" fill="${fg}"/></svg>\n`;
}

/**
 * The decoded content of every PNG inside `buf` (a PNG, or an .ico/.icns
 * that embeds PNGs), as one buffer: each image's IHDR and its inflated
 * pixel stream. Compressed bytes depend on the zlib build of the node that
 * wrote them; this does not, so two runs of the generator compare equal on
 * any node. Bytes outside the PNGs (container headers that hold offsets and
 * lengths) are left out; the image list itself carries the sizes.
 */
export function decodedPngs(buf) {
  const sig = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
  const out = [];
  let at = buf.indexOf(sig);
  while (at !== -1) {
    let p = at + 8;
    let ihdr = null;
    const idat = [];
    while (p + 8 <= buf.length) {
      const len = buf.readUInt32BE(p);
      const type = buf.toString('latin1', p + 4, p + 8);
      const data = buf.subarray(p + 8, p + 8 + len);
      if (type === 'IHDR') ihdr = data;
      if (type === 'IDAT') idat.push(data);
      p += 12 + len;
      if (type === 'IEND') break;
    }
    if (ihdr) out.push(Buffer.from(ihdr), inflateSync(Buffer.concat(idat)));
    at = buf.indexOf(sig, p);
  }
  return Buffer.concat(out);
}
