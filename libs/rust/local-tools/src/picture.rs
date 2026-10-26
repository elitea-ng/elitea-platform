//! Images the model can see: PNG, JPEG, WebP and GIF, sniffed from their
//! bytes (never the extension), kept within the limits every provider
//! accepts, shared by `read_file` and the desktop's composer attachments
//! (#1229) so both bound an image the same way.
//!
//! * A source over [`MAX_SOURCE_BYTES`] is refused (`TooLarge`).
//! * An image within [`MAX_SIDE`] px and [`MAX_IMAGE_BYTES`] goes through
//!   byte for byte (an animated GIF stays one; nothing is decoded).
//! * Any other is decoded within bounds on the canvas and working memory (a small file can
//!   declare a huge canvas), shrunk to [`MAX_SIDE`] on its longest side
//!   and encoded again: JPEG stays JPEG, anything else becomes PNG (a GIF
//!   keeps its first frame, a WebP its pixels). A PNG still over the limit
//!   is sent as JPEG with its transparency flattened onto white; one that
//!   no quality brings under the limit is refused.

use std::io::Cursor;

use image::codecs::jpeg::JpegEncoder;
use image::imageops::FilterType;
use image::{DynamicImage, ImageFormat, ImageReader, Limits, Rgb, RgbImage};

/// The longest side, in pixels, an image keeps.
pub const MAX_SIDE: u32 = 2048;
/// The most bytes an image's base64 may take in a request: a provider's
/// image limit counts the encoded image (Anthropic's is 5 MiB), and this
/// keeps a margin under it.
pub const MAX_ENCODED_BYTES: usize = 5_000_000;
/// The most bytes an image keeps after [`prepare`]: what encodes to at most
/// [`MAX_ENCODED_BYTES`] of base64 (4 characters per 3 bytes).
pub const MAX_IMAGE_BYTES: usize = MAX_ENCODED_BYTES / 4 * 3;
/// The most bytes of source [`prepare`] reads (the model-input cap adk-core
/// puts on one inline part).
pub const MAX_SOURCE_BYTES: usize = 10 * 1024 * 1024;
/// The most bytes of a stored thumbnail.
pub const MAX_THUMBNAIL_BYTES: usize = 64 * 1024;
/// The longest side of a thumbnail.
const THUMBNAIL_SIDE: u32 = 192;
/// The decoder's bounds: canvas and working memory.
const DECODE_LIMITS_SIDE: u32 = 16_384;
const DECODE_LIMITS_ALLOC: u64 = 256 * 1024 * 1024;
/// JPEG qualities tried, best first.
const QUALITIES: [u8; 3] = [85, 70, 50];

/// An image format the tools take.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    Png,
    Jpeg,
    Webp,
    Gif,
}

impl Kind {
    #[must_use]
    pub fn mime(self) -> &'static str {
        match self {
            Self::Png => "image/png",
            Self::Jpeg => "image/jpeg",
            Self::Webp => "image/webp",
            Self::Gif => "image/gif",
        }
    }

    fn format(self) -> ImageFormat {
        match self {
            Self::Png => ImageFormat::Png,
            Self::Jpeg => ImageFormat::Jpeg,
            Self::Webp => ImageFormat::WebP,
            Self::Gif => ImageFormat::Gif,
        }
    }
}

/// The image format `bytes` start as (their signature), if any.
#[must_use]
pub fn sniff(bytes: &[u8]) -> Option<Kind> {
    if bytes.starts_with(b"\x89PNG\r\n\x1a\n") {
        Some(Kind::Png)
    } else if bytes.starts_with(&[0xFF, 0xD8, 0xFF]) {
        Some(Kind::Jpeg)
    } else if bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a") {
        Some(Kind::Gif)
    } else if bytes.len() >= 12 && &bytes[..4] == b"RIFF" && &bytes[8..12] == b"WEBP" {
        Some(Kind::Webp)
    } else {
        None
    }
}

/// Why an image was not taken.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum PictureError {
    /// Not PNG, JPEG, WebP or GIF.
    Unsupported,
    /// Over a limit, and no re-encoding brings it under.
    TooLarge(String),
    /// Starts like an image but does not decode.
    Corrupt,
}

impl PictureError {
    /// What the person (or the model) is told.
    #[must_use]
    pub fn message(&self) -> String {
        match self {
            Self::Unsupported => "only PNG, JPEG, WebP and GIF images are supported".to_owned(),
            Self::TooLarge(why) => why.clone(),
            Self::Corrupt => "the image could not be read".to_owned(),
        }
    }
}

/// An image ready to send.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Picture {
    pub mime: &'static str,
    pub bytes: Vec<u8>,
    pub width: u32,
    pub height: u32,
    /// The source's size, before any re-encoding.
    pub source_bytes: usize,
}

fn reader(bytes: &[u8], kind: Kind) -> ImageReader<Cursor<&[u8]>> {
    let mut reader = ImageReader::with_format(Cursor::new(bytes), kind.format());
    let mut limits = Limits::default();
    limits.max_image_width = Some(DECODE_LIMITS_SIDE);
    limits.max_image_height = Some(DECODE_LIMITS_SIDE);
    limits.max_alloc = Some(DECODE_LIMITS_ALLOC);
    reader.limits(limits);
    reader
}

/// The pixel size of `bytes`, from the header (nothing is decoded).
#[must_use]
pub fn dimensions(bytes: &[u8]) -> Option<(u32, u32)> {
    let kind = sniff(bytes)?;
    reader(bytes, kind).into_dimensions().ok()
}

/// Take `source` as an image within the limits (see the module).
///
/// # Errors
///
/// [`PictureError`].
pub fn prepare(source: &[u8]) -> Result<Picture, PictureError> {
    let kind = sniff(source).ok_or(PictureError::Unsupported)?;
    if source.len() > MAX_SOURCE_BYTES {
        return Err(PictureError::TooLarge(format!(
            "the image is {} MiB; the limit is {} MiB",
            source.len().div_ceil(1024 * 1024),
            MAX_SOURCE_BYTES / (1024 * 1024)
        )));
    }
    let (width, height) = reader(source, kind)
        .into_dimensions()
        .map_err(|_| PictureError::Corrupt)?;
    if width == 0 || height == 0 {
        return Err(PictureError::Corrupt);
    }
    if width.max(height) <= MAX_SIDE && source.len() <= MAX_IMAGE_BYTES {
        return Ok(Picture {
            mime: kind.mime(),
            bytes: source.to_vec(),
            width,
            height,
            source_bytes: source.len(),
        });
    }
    let decoded = reader(source, kind)
        .decode()
        .map_err(|_| PictureError::Corrupt)?;
    let decoded = if width.max(height) > MAX_SIDE {
        decoded.resize(MAX_SIDE, MAX_SIDE, FilterType::Triangle)
    } else {
        decoded
    };
    let (width, height) = (decoded.width(), decoded.height());
    let too_large = || {
        PictureError::TooLarge(format!(
            "the image is over {} KB even shrunk to {MAX_SIDE} px",
            MAX_IMAGE_BYTES / 1000
        ))
    };
    if kind != Kind::Jpeg {
        let mut out = Vec::new();
        decoded
            .write_to(&mut Cursor::new(&mut out), ImageFormat::Png)
            .map_err(|_| PictureError::Corrupt)?;
        if out.len() <= MAX_IMAGE_BYTES {
            return Ok(Picture {
                mime: Kind::Png.mime(),
                bytes: out,
                width,
                height,
                source_bytes: source.len(),
            });
        }
    }
    let flat = flattened(&decoded);
    for quality in QUALITIES {
        let out = jpeg(&flat, quality).ok_or(PictureError::Corrupt)?;
        if out.len() <= MAX_IMAGE_BYTES {
            return Ok(Picture {
                mime: Kind::Jpeg.mime(),
                bytes: out,
                width,
                height,
                source_bytes: source.len(),
            });
        }
    }
    Err(too_large())
}

/// `image` on white where it is transparent, as JPEG has no alpha.
fn flattened(image: &DynamicImage) -> RgbImage {
    if !image.color().has_alpha() {
        return image.to_rgb8();
    }
    let rgba = image.to_rgba8();
    let mut out = RgbImage::new(rgba.width(), rgba.height());
    for (x, y, pixel) in rgba.enumerate_pixels() {
        let alpha = u32::from(pixel[3]);
        let blend = |channel: u8| {
            let mixed = (u32::from(channel) * alpha + 255 * (255 - alpha) + 127) / 255;
            u8::try_from(mixed).unwrap_or(u8::MAX)
        };
        out.put_pixel(
            x,
            y,
            Rgb([blend(pixel[0]), blend(pixel[1]), blend(pixel[2])]),
        );
    }
    out
}

fn jpeg(image: &RgbImage, quality: u8) -> Option<Vec<u8>> {
    let mut out = Vec::new();
    JpegEncoder::new_with_quality(&mut out, quality)
        .encode_image(image)
        .ok()?;
    Some(out)
}

/// A small JPEG of `bytes` for the thread history, at most
/// [`MAX_THUMBNAIL_BYTES`]; `None` when `bytes` is not a decodable image.
#[must_use]
pub fn thumbnail(bytes: &[u8]) -> Option<Vec<u8>> {
    let kind = sniff(bytes)?;
    let decoded = reader(bytes, kind).decode().ok()?;
    let small = flattened(&decoded.thumbnail(THUMBNAIL_SIDE, THUMBNAIL_SIDE));
    [70, 50, 30]
        .into_iter()
        .filter_map(|quality| jpeg(&small, quality))
        .find(|out| out.len() <= MAX_THUMBNAIL_BYTES)
}

#[cfg(test)]
mod tests {
    use image::{ImageBuffer, Rgba};

    use super::*;

    fn png(width: u32, height: u32) -> Vec<u8> {
        let image = ImageBuffer::from_fn(width, height, |x, y| {
            Rgba([
                u8::try_from(x % 251).unwrap_or(0),
                u8::try_from(y % 241).unwrap_or(0),
                7,
                255,
            ])
        });
        let mut out = Vec::new();
        DynamicImage::ImageRgba8(image)
            .write_to(&mut Cursor::new(&mut out), ImageFormat::Png)
            .expect("encode");
        out
    }

    #[test]
    fn the_kind_comes_from_the_bytes() {
        assert_eq!(sniff(&png(2, 2)), Some(Kind::Png));
        assert_eq!(sniff(&[0xFF, 0xD8, 0xFF, 0xE0]), Some(Kind::Jpeg));
        assert_eq!(sniff(b"GIF89a...."), Some(Kind::Gif));
        assert_eq!(sniff(b"RIFF\0\0\0\0WEBPVP8 "), Some(Kind::Webp));
        assert_eq!(sniff(b"<svg xmlns='x'></svg>"), None);
        assert_eq!(sniff(b""), None);
    }

    #[test]
    fn a_small_image_passes_through_untouched() {
        let source = png(40, 30);
        let picture = prepare(&source).expect("prepare");
        assert_eq!(picture.bytes, source);
        assert_eq!((picture.width, picture.height), (40, 30));
        assert_eq!(picture.mime, "image/png");
    }

    #[test]
    fn a_large_canvas_is_shrunk_to_the_longest_side() {
        let picture = prepare(&png(3000, 1500)).expect("prepare");
        assert_eq!((picture.width, picture.height), (2048, 1024));
        assert_eq!(picture.mime, "image/png");
        assert_eq!(dimensions(&picture.bytes), Some((2048, 1024)));
    }

    #[test]
    fn an_image_whose_base64_is_over_the_limit_is_recompressed() {
        // Noise does not compress: a PNG of it within the side but past
        // the bytes that encode to the provider's limit.
        let mut seed = 0x2545_f491_u32;
        let noise = RgbImage::from_fn(1200, 1100, |_, _| {
            let mut next = || {
                seed = seed.wrapping_mul(1_103_515_245).wrapping_add(12_345);
                u8::try_from(seed >> 24).unwrap_or(0)
            };
            Rgb([next(), next(), next()])
        });
        let mut source = Vec::new();
        DynamicImage::ImageRgb8(noise)
            .write_to(&mut Cursor::new(&mut source), ImageFormat::Png)
            .expect("encode");
        assert!(source.len() > MAX_IMAGE_BYTES && source.len() < 5 * 1024 * 1024);
        let picture = prepare(&source).expect("prepare");
        assert_eq!(picture.mime, "image/jpeg");
        assert!(picture.bytes.len().div_ceil(3) * 4 <= MAX_ENCODED_BYTES);
    }

    #[test]
    fn text_and_corrupt_images_are_refused() {
        assert_eq!(prepare(b"hello"), Err(PictureError::Unsupported));
        let mut broken = png(10, 10);
        broken.truncate(30);
        assert_eq!(prepare(&broken).unwrap_err(), PictureError::Corrupt);
    }

    #[test]
    fn a_source_over_the_cap_is_refused_before_it_is_read() {
        let mut huge = png(4, 4);
        huge.resize(MAX_SOURCE_BYTES + 1, 0);
        assert!(matches!(
            prepare(&huge),
            Err(PictureError::TooLarge(why)) if why.contains("limit is 10 MiB")
        ));
    }

    #[test]
    fn a_thumbnail_is_a_small_jpeg() {
        let thumb = thumbnail(&png(800, 600)).expect("thumbnail");
        assert!(thumb.len() <= MAX_THUMBNAIL_BYTES);
        assert_eq!(sniff(&thumb), Some(Kind::Jpeg));
        let (width, height) = dimensions(&thumb).expect("size");
        assert!(width.max(height) <= THUMBNAIL_SIDE);
        assert!(thumbnail(b"not an image").is_none());
    }

    #[test]
    fn transparency_is_flattened_onto_white() {
        let clear = DynamicImage::ImageRgba8(ImageBuffer::from_pixel(2, 2, Rgba([0, 0, 0, 0])));
        assert_eq!(flattened(&clear).get_pixel(0, 0), &Rgb([255, 255, 255]));
    }
}
