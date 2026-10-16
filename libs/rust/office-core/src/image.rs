//! Raster dimensions from the file header, before any decoder runs.

/// A raster format this crate can read the dimensions of.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Format {
    Png,
    Jpeg,
    Gif,
    Bmp,
    WebP,
    Tiff,
}

impl Format {
    /// The file extension that matches the sniffed signature (never a
    /// caller-supplied one).
    pub(crate) fn extension(self) -> &'static str {
        match self {
            Format::Png => "png",
            Format::Jpeg => "jpeg",
            Format::Gif => "gif",
            Format::Bmp => "bmp",
            Format::WebP => "webp",
            Format::Tiff => "tiff",
        }
    }
}

/// What a header probe found.
#[derive(Debug, PartialEq, Eq)]
pub(crate) enum Probe {
    /// Not a raster format this crate sniffs (SVG, fonts, XML, embedded
    /// workbooks, formats without a probe).
    NotRaster,
    /// PNG, GIF, JPEG or BMP with these dimensions.
    Raster {
        width: u64,
        height: u64,
        format: Format,
    },
    /// The magic matched but the header is truncated or inconsistent.
    Unreadable,
}

impl Probe {
    #[cfg(test)]
    pub(crate) fn pixels(&self) -> Option<u64> {
        match self {
            Probe::Raster { width, height, .. } => Some(width.saturating_mul(*height)),
            _ => None,
        }
    }
}

/// True when the first bytes are a known binary signature: PNG, JPEG, GIF,
/// BMP, WebP, TIFF, EMF, WMF, an OLE compound file or a zip. Anything else is
/// XML or an opaque (font-like) part: ADR-0032 classifies parts by content.
pub(crate) fn is_known_binary(b: &[u8]) -> bool {
    b.starts_with(b"\x89PNG\r\n\x1a\n")
        || b.starts_with(&[0xFF, 0xD8, 0xFF])
        || b.starts_with(b"GIF87a")
        || b.starts_with(b"GIF89a")
        || (b.starts_with(b"BM") && b.len() >= 26)
        || (b.starts_with(b"RIFF") && b.get(8..12) == Some(b"WEBP"))
        || b.starts_with(&[0x49, 0x49, 0x2A, 0x00])
        || b.starts_with(&[0x4D, 0x4D, 0x00, 0x2A])
        || (b.starts_with(&[1, 0, 0, 0]) && b.get(40..44) == Some(b" EMF"))
        || b.starts_with(&[0xD7, 0xCD, 0xC6, 0x9A])
        || b.starts_with(&[1, 0, 9, 0])
        || b.starts_with(&[2, 0, 9, 0])
        || b.starts_with(&[0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1])
        || b.starts_with(b"PK\x03\x04")
        || b.starts_with(b"PK\x05\x06")
        || b.starts_with(b"PK\x07\x08")
}

fn be16(b: &[u8], i: usize) -> Option<u64> {
    Some(u64::from(u16::from_be_bytes([*b.get(i)?, *b.get(i + 1)?])))
}
fn be32(b: &[u8], i: usize) -> Option<u64> {
    Some(u64::from(u32::from_be_bytes([
        *b.get(i)?,
        *b.get(i + 1)?,
        *b.get(i + 2)?,
        *b.get(i + 3)?,
    ])))
}
fn le16(b: &[u8], i: usize) -> Option<u64> {
    Some(u64::from(u16::from_le_bytes([*b.get(i)?, *b.get(i + 1)?])))
}
fn le_i32_abs(b: &[u8], i: usize) -> Option<u64> {
    let v = i32::from_le_bytes([*b.get(i)?, *b.get(i + 1)?, *b.get(i + 2)?, *b.get(i + 3)?]);
    Some(i64::from(v).unsigned_abs())
}

/// Probe the dimensions of `bytes` from its header alone.
pub(crate) fn probe(bytes: &[u8]) -> Probe {
    if bytes.starts_with(b"\x89PNG\r\n\x1a\n") {
        // IHDR is the first chunk: length(4) "IHDR" width(4) height(4).
        if bytes.get(12..16) == Some(b"IHDR")
            && let (Some(width), Some(height)) = (be32(bytes, 16), be32(bytes, 20))
        {
            return Probe::Raster {
                width,
                height,
                format: Format::Png,
            };
        }
        return Probe::Unreadable;
    }
    if bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a") {
        return match (le16(bytes, 6), le16(bytes, 8)) {
            (Some(width), Some(height)) => Probe::Raster {
                width,
                height,
                format: Format::Gif,
            },
            _ => Probe::Unreadable,
        };
    }
    if bytes.starts_with(b"BM") && bytes.len() >= 26 {
        // BITMAPINFOHEADER: width i32, height i32 (negative = top-down).
        return match (le_i32_abs(bytes, 18), le_i32_abs(bytes, 22)) {
            (Some(width), Some(height)) => Probe::Raster {
                width,
                height,
                format: Format::Bmp,
            },
            _ => Probe::Unreadable,
        };
    }
    if bytes.starts_with(&[0xFF, 0xD8]) {
        return probe_jpeg(bytes);
    }
    if bytes.starts_with(b"RIFF") && bytes.get(8..12) == Some(b"WEBP") {
        return probe_webp(bytes);
    }
    if bytes.starts_with(&[0x49, 0x49, 0x2A, 0x00]) || bytes.starts_with(&[0x4D, 0x4D, 0x00, 0x2A])
    {
        return probe_tiff(bytes);
    }
    Probe::NotRaster
}

fn webp(width: u64, height: u64) -> Probe {
    Probe::Raster {
        width,
        height,
        format: Format::WebP,
    }
}

/// WebP: the first chunk is `VP8 ` (lossy), `VP8L` (lossless) or `VP8X`
/// (extended, canvas size).
fn probe_webp(b: &[u8]) -> Probe {
    let Some(tag) = b.get(12..16) else {
        return Probe::Unreadable;
    };
    let d = 20usize; // chunk data
    match tag {
        b"VP8 " => {
            // frame tag(3) start code 9d 01 2a, then 14-bit width and height.
            if b.get(d + 3..d + 6) != Some(&[0x9D, 0x01, 0x2A]) {
                return Probe::Unreadable;
            }
            match (le16(b, d + 6), le16(b, d + 8)) {
                (Some(w), Some(h)) => webp(w & 0x3FFF, h & 0x3FFF),
                _ => Probe::Unreadable,
            }
        }
        b"VP8L" => {
            if b.get(d) != Some(&0x2F) {
                return Probe::Unreadable;
            }
            let bits: Option<[u64; 4]> = (|| {
                Some([
                    u64::from(*b.get(d + 1)?),
                    u64::from(*b.get(d + 2)?),
                    u64::from(*b.get(d + 3)?),
                    u64::from(*b.get(d + 4)?),
                ])
            })();
            match bits {
                Some([b1, b2, b3, b4]) => {
                    let w = 1 + (b1 | ((b2 & 0x3F) << 8));
                    let h = 1 + ((b2 >> 6) | (b3 << 2) | ((b4 & 0x0F) << 10));
                    webp(w, h)
                }
                None => Probe::Unreadable,
            }
        }
        b"VP8X" => {
            let le24 = |i: usize| -> Option<u64> {
                Some(
                    u64::from(*b.get(i)?)
                        | (u64::from(*b.get(i + 1)?) << 8)
                        | (u64::from(*b.get(i + 2)?) << 16),
                )
            };
            match (le24(d + 4), le24(d + 7)) {
                (Some(w), Some(h)) => webp(w + 1, h + 1),
                _ => Probe::Unreadable,
            }
        }
        _ => Probe::Unreadable,
    }
}

/// TIFF: width and length from the first IFD (tags 256 and 257, SHORT or LONG).
#[allow(clippy::many_single_char_names)]
fn probe_tiff(b: &[u8]) -> Probe {
    let little = b.starts_with(&[0x49, 0x49]);
    let r16 = |i: usize| -> Option<u64> {
        let v = [*b.get(i)?, *b.get(i + 1)?];
        Some(u64::from(if little {
            u16::from_le_bytes(v)
        } else {
            u16::from_be_bytes(v)
        }))
    };
    let r32 = |i: usize| -> Option<u64> {
        let v = [*b.get(i)?, *b.get(i + 1)?, *b.get(i + 2)?, *b.get(i + 3)?];
        Some(u64::from(if little {
            u32::from_le_bytes(v)
        } else {
            u32::from_be_bytes(v)
        }))
    };
    let Some(ifd) = r32(4).and_then(|o| usize::try_from(o).ok()) else {
        return Probe::Unreadable;
    };
    let Some(count) = r16(ifd) else {
        return Probe::Unreadable;
    };
    let (mut w, mut h) = (None, None);
    for k in 0..count.min(512) as usize {
        let e = ifd + 2 + k * 12;
        let (Some(tag), Some(ty)) = (r16(e), r16(e + 2)) else {
            return Probe::Unreadable;
        };
        let v = match ty {
            3 => r16(e + 8),
            4 => r32(e + 8),
            _ => None,
        };
        match tag {
            256 => w = v,
            257 => h = v,
            _ => {}
        }
    }
    match (w, h) {
        (Some(width), Some(height)) => Probe::Raster {
            width,
            height,
            format: Format::Tiff,
        },
        _ => Probe::Unreadable,
    }
}

fn probe_jpeg(b: &[u8]) -> Probe {
    let mut i = 2usize;
    loop {
        // Skip fill bytes, then expect a marker prefix.
        while b.get(i) == Some(&0xFF) && b.get(i + 1) == Some(&0xFF) {
            i += 1;
        }
        if b.get(i) != Some(&0xFF) {
            return Probe::Unreadable;
        }
        let Some(&m) = b.get(i + 1) else {
            return Probe::Unreadable;
        };
        i += 2;
        match m {
            0x00 | 0x01 | 0xD0..=0xD7 => continue,
            0xD9 | 0xDA => return Probe::Unreadable, // end / scan before any SOF
            _ => {}
        }
        let Some(len) = be16(b, i) else {
            return Probe::Unreadable;
        };
        if len < 2 {
            return Probe::Unreadable;
        }
        if matches!(m, 0xC0..=0xCF) && !matches!(m, 0xC4 | 0xC8 | 0xCC) {
            return match (be16(b, i + 3), be16(b, i + 5)) {
                (Some(height), Some(width)) => Probe::Raster {
                    width,
                    height,
                    format: Format::Jpeg,
                },
                _ => Probe::Unreadable,
            };
        }
        i = i.saturating_add(usize::try_from(len).unwrap_or(usize::MAX));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn png_header(w: u32, h: u32) -> Vec<u8> {
        let mut v = b"\x89PNG\r\n\x1a\n\0\0\0\rIHDR".to_vec();
        v.extend_from_slice(&w.to_be_bytes());
        v.extend_from_slice(&h.to_be_bytes());
        v.extend_from_slice(&[8, 2, 0, 0, 0]);
        v
    }

    #[test]
    fn png_dimensions_come_from_ihdr() {
        assert_eq!(
            probe(&png_header(12_000, 7)),
            Probe::Raster {
                width: 12_000,
                height: 7,
                format: Format::Png
            }
        );
        assert_eq!(
            probe(&png_header(100_000, 100_000)).pixels(),
            Some(10_000_000_000)
        );
        assert_eq!(probe(&png_header(1, 1)[..14]), Probe::Unreadable);
    }

    #[test]
    fn gif_bmp_jpeg() {
        let mut g = b"GIF89a".to_vec();
        g.extend_from_slice(&[3, 0, 4, 0]);
        assert_eq!(
            probe(&g),
            Probe::Raster {
                width: 3,
                height: 4,
                format: Format::Gif
            }
        );
        let jpg = [
            0xFF, 0xD8, 0xFF, 0xE0, 0, 4, 0, 0, 0xFF, 0xC0, 0, 11, 8, 0, 9, 0, 7, 3, 0, 0, 0,
        ];
        assert_eq!(
            probe(&jpg),
            Probe::Raster {
                width: 7,
                height: 9,
                format: Format::Jpeg
            }
        );
        assert_eq!(probe(&[0xFF, 0xD8, 0xFF, 0xD9]), Probe::Unreadable);
        let mut bmp = vec![0u8; 30];
        bmp[0] = b'B';
        bmp[1] = b'M';
        bmp[18] = 5;
        bmp[22..26].copy_from_slice(&(-6i32).to_le_bytes());
        assert_eq!(
            probe(&bmp),
            Probe::Raster {
                width: 5,
                height: 6,
                format: Format::Bmp
            }
        );
        assert_eq!(probe(b"<svg/>"), Probe::NotRaster);
    }

    #[test]
    fn truncated_jpeg_does_not_loop() {
        assert_eq!(probe(&[0xFF, 0xD8, 0xFF]), Probe::Unreadable);
        assert_eq!(
            probe(&[0xFF, 0xD8, 0xFF, 0xE0, 0xFF, 0xFF]),
            Probe::Unreadable
        );
    }

    #[test]
    fn known_binary_signatures() {
        assert!(is_known_binary(&png_header(1, 1)));
        assert!(is_known_binary(&[0xFF, 0xD8, 0xFF, 0xE0]));
        assert!(is_known_binary(b"GIF89a\0\0\0\0"));
        assert!(is_known_binary(b"RIFF\0\0\0\0WEBPVP8 "));
        assert!(is_known_binary(&[0x49, 0x49, 0x2A, 0x00, 8]));
        assert!(is_known_binary(&[
            0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1
        ]));
        assert!(is_known_binary(b"PK\x03\x04"));
        assert!(is_known_binary(&[0xD7, 0xCD, 0xC6, 0x9A, 0]));
        let mut emf = vec![0u8; 48];
        emf[0] = 1;
        emf[40..44].copy_from_slice(b" EMF");
        assert!(is_known_binary(&emf));
        assert!(!is_known_binary(b"<?xml version=\"1.0\"?>"));
        assert!(!is_known_binary(b"\0\x01\0\0font"));
        assert!(!is_known_binary(b""));
    }

    #[test]
    #[allow(clippy::many_single_char_names)]
    fn webp_dimensions() {
        // VP8 (lossy): 640 x 480
        let mut v = b"RIFF\0\0\0\0WEBPVP8 \0\0\0\0\x10\x00\x00\x9d\x01\x2a".to_vec();
        v.extend_from_slice(&[0x80, 0x02, 0xE0, 0x01]);
        assert_eq!(probe(&v).pixels(), Some(640 * 480));
        // VP8L (lossless): 1 + (w-1) with w = 300, h = 200
        let (w, h) = (299u32, 199u32);
        let mut l = b"RIFF\0\0\0\0WEBPVP8L\0\0\0\0\x2f".to_vec();
        let bits = w | (h << 14);
        l.extend_from_slice(&bits.to_le_bytes());
        assert_eq!(probe(&l).pixels(), Some(300 * 200));
        // VP8X: canvas (w-1, h-1) as 24-bit
        let mut x = b"RIFF\0\0\0\0WEBPVP8X\x0a\0\0\0\0\0\0\0".to_vec();
        x.extend_from_slice(&[0x0F, 0x27, 0x00, 0x0F, 0x27, 0x00]); // 9999 + 1
        assert_eq!(probe(&x).pixels(), Some(10_000 * 10_000));
        assert_eq!(probe(b"RIFF\0\0\0\0WEBPVP8 ").pixels(), None);
        assert_eq!(probe(b"RIFF\0\0\0\0WEBPVP8 "), Probe::Unreadable);
    }

    #[test]
    fn tiff_dimensions_both_byte_orders() {
        // little endian, IFD at 8, two entries: width 70000 (LONG), length 20 (SHORT)
        let mut t = vec![0x49, 0x49, 0x2A, 0, 8, 0, 0, 0, 2, 0];
        t.extend_from_slice(&[0, 1, 4, 0, 1, 0, 0, 0]);
        t.extend_from_slice(&70_000u32.to_le_bytes());
        t.extend_from_slice(&[1, 1, 3, 0, 1, 0, 0, 0]);
        t.extend_from_slice(&[20, 0, 0, 0]);
        assert_eq!(probe(&t).pixels(), Some(1_400_000));
        // big endian
        let mut b = vec![0x4D, 0x4D, 0, 0x2A, 0, 0, 0, 8, 0, 2];
        b.extend_from_slice(&[1, 0, 0, 3, 0, 0, 0, 1, 0x1F, 0x40, 0, 0]); // width 8000 short
        b.extend_from_slice(&[1, 1, 0, 3, 0, 0, 0, 1, 0x0F, 0xA0, 0, 0]); // length 4000
        assert_eq!(probe(&b).pixels(), Some(32_000_000));
        assert_eq!(probe(&[0x49, 0x49, 0x2A, 0, 8, 0, 0, 0]), Probe::Unreadable);
    }
}
