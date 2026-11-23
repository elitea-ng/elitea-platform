// Image headers for the brand check: PNG, ICO and ICNS, read with node's
// standard library only: the sizes and the alpha channel the platforms care
// about, and (decodePng) the pixels of an 8-bit RGB or RGBA PNG, for the disk
// image background's label contrast.

import { inflateSync } from 'node:zlib';

const PNG_SIGNATURE = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

/**
 * The IHDR of a PNG: `{ width, height, alpha }`, or null when the bytes are
 * not a PNG. `alpha` is true for colour types 4 and 6 (grey or RGB with an
 * alpha channel) and for a palette or truecolour image with a tRNS chunk.
 */
export function pngInfo(buf) {
  if (buf.length < 33 || !buf.subarray(0, 8).equals(PNG_SIGNATURE)) return null;
  if (buf.toString('latin1', 12, 16) !== 'IHDR') return null;
  const width = buf.readUInt32BE(16);
  const height = buf.readUInt32BE(20);
  const colourType = buf[25];
  let alpha = colourType === 4 || colourType === 6;
  if (!alpha) {
    // A tRNS chunk gives a palette or truecolour image transparency.
    let at = 8;
    while (at + 8 <= buf.length) {
      const len = buf.readUInt32BE(at);
      const type = buf.toString('latin1', at + 4, at + 8);
      if (type === 'tRNS') alpha = true;
      if (type === 'IDAT' || type === 'IEND') break;
      at += 12 + len;
    }
  }
  return { width, height, alpha };
}

/**
 * The image sizes inside a Windows .ico: an array of square edge lengths
 * (a 0 byte in the directory means 256). Null when the bytes are not an icon.
 * Non-square entries are reported as `{ width, height }` objects instead.
 */
export function icoSizes(buf) {
  if (buf.length < 6 || buf.readUInt16LE(0) !== 0 || buf.readUInt16LE(2) !== 1) return null;
  const count = buf.readUInt16LE(4);
  if (buf.length < 6 + 16 * count) return null;
  const sizes = [];
  for (let i = 0; i < count; i++) {
    const at = 6 + 16 * i;
    const width = buf[at] || 256;
    const height = buf[at + 1] || 256;
    sizes.push(width === height ? width : { width, height });
  }
  return sizes;
}

// ICNS element types that hold an icon image, and the pixel edge each holds.
// https://en.wikipedia.org/wiki/Apple_Icon_Image_format#Icon_types
export const ICNS_TYPES = Object.freeze({
  icp4: 16,
  icp5: 32,
  icp6: 64,
  ic04: 16,
  ic05: 32,
  ic07: 128,
  ic08: 256,
  ic09: 512,
  ic10: 1024,
  ic11: 32,
  ic12: 64,
  ic13: 256,
  ic14: 512,
});

/**
 * The pixel edges a macOS .icns holds (sorted, unique), or null when the
 * bytes are not an icns file or its element table is broken.
 */
export function icnsSizes(buf) {
  if (buf.length < 8 || buf.toString('latin1', 0, 4) !== 'icns') return null;
  if (buf.readUInt32BE(4) !== buf.length) return null;
  const sizes = new Set();
  let at = 8;
  while (at < buf.length) {
    if (at + 8 > buf.length) return null;
    const type = buf.toString('latin1', at, at + 4);
    const len = buf.readUInt32BE(at + 4);
    if (len < 8 || at + len > buf.length) return null;
    if (type in ICNS_TYPES) sizes.add(ICNS_TYPES[type]);
    at += len;
  }
  return [...sizes].sort((a, b) => a - b);
}

/** True when the text is an SVG document with a viewBox. */
export function isSvg(text) {
  return /^\s*(<\?xml[^>]*>\s*)?<svg\b[^>]*\bviewBox="[^"]+"/.test(text) && /<\/svg>\s*$/.test(text);
}

/**
 * The pixels of a non-interlaced 8-bit RGB or RGBA PNG: `{ width, height, pixel(x, y) }`,
 * `pixel` returning [r, g, b, a]. Throws on any other kind of PNG.
 */
export function decodePng(buf) {
  const info = pngInfo(buf);
  if (!info) throw new Error('not a PNG');
  const depth = buf[24];
  const type = buf[25];
  const interlace = buf[28];
  if (depth !== 8 || (type !== 2 && type !== 6) || interlace !== 0) throw new Error('decodePng reads 8-bit RGB or RGBA PNGs, not interlaced');
  const channels = type === 6 ? 4 : 3;
  const idat = [];
  for (let p = 8; p + 8 <= buf.length; ) {
    const len = buf.readUInt32BE(p);
    const kind = buf.toString('latin1', p + 4, p + 8);
    if (kind === 'IDAT') idat.push(buf.subarray(p + 8, p + 8 + len));
    p += 12 + len;
  }
  const raw = inflateSync(Buffer.concat(idat));
  const stride = info.width * channels;
  const out = Buffer.alloc(stride * info.height);
  for (let y = 0; y < info.height; y++) {
    const filter = raw[y * (stride + 1)];
    const src = y * (stride + 1) + 1;
    const row = y * stride;
    for (let i = 0; i < stride; i++) {
      const a = i >= channels ? out[row + i - channels] : 0;
      const b = y > 0 ? out[row - stride + i] : 0;
      const c = i >= channels && y > 0 ? out[row - stride + i - channels] : 0;
      let pred = 0;
      if (filter === 1) pred = a;
      else if (filter === 2) pred = b;
      else if (filter === 3) pred = (a + b) >> 1;
      else if (filter === 4) {
        const pa = Math.abs(b - c);
        const pb = Math.abs(a - c);
        const pc = Math.abs(a + b - 2 * c);
        pred = pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
      } else if (filter !== 0) throw new Error(`PNG row ${y}: unknown filter ${filter}`);
      out[row + i] = (raw[src + i] + pred) & 0xff;
    }
  }
  return {
    width: info.width,
    height: info.height,
    pixel: (x, y) => {
      const o = y * stride + x * channels;
      return [out[o], out[o + 1], out[o + 2], channels === 4 ? out[o + 3] : 255];
    },
  };
}
