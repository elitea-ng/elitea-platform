//! Raster dimensions from the file header, before any decoder runs.

/// A raster format this crate can read the dimensions of.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Format {
    Png,
    Jpeg,
    Gif,
    Bmp,
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
    Probe::NotRaster
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
}
