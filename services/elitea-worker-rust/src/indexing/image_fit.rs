//! Fitting a picture under the gateway's inline image budget (ADR-0030 I4
//! part B).
//!
//! The SDK sends the original bytes of an image to the model whatever their
//! size. The `/llm` gateway admits an inline image of at most
//! [`MAX_INLINE_IMAGE_BYTES`](crate::transport::openai_compatible_facade::MAX_INLINE_IMAGE_BYTES)
//! and only as PNG, JPEG, GIF or WebP, so a bigger picture is downscaled
//! here, and a BMP (which no model takes) is rasterised to PNG.
//!
//! Decoding untrusted bytes is bounded on every axis:
//!
//! * the pixel count is read from the header first and refused above
//!   [`MAX_PIXELS`] (a decompression bomb is a few bytes of header);
//! * the decoder runs under an allocation limit ([`MAX_DECODE_BYTES`]);
//! * at most [`DECODES_AT_ONCE`] pictures are decoded at the same time (the
//!   run prepares documents concurrently, a decoded 40 MP picture is 160 MB);
//! * the work is on the blocking pool and the wait for it is bounded by
//!   [`FIT_TIMEOUT`].
//!
//! The picture is resized proportionally and re-encoded as PNG when it has
//! transparency (or was a BMP) and as JPEG (quality [`JPEG_QUALITY`])
//! otherwise, shrinking until it fits. A picture that cannot be fit, or
//! read, is a typed [`FitError`], which the run reports as a per-document
//! skip.

use std::io::Cursor;
use std::sync::OnceLock;
use std::time::Duration;

use bytes::Bytes;
use image::codecs::jpeg::JpegEncoder;
use image::imageops::FilterType;
use image::{DynamicImage, GenericImageView as _, ImageFormat, ImageReader, Limits};
use sha2::{Digest as _, Sha256};
use tokio::sync::Semaphore;

/// The most pixels of a picture that is decoded.
pub(crate) const MAX_PIXELS: u64 = 40_000_000;
/// The decoder's allocation limit.
const MAX_DECODE_BYTES: u64 = 256 * 1024 * 1024;
/// Pictures decoded at the same time, run-wide.
const DECODES_AT_ONCE: usize = 2;
/// How long one picture may take to decode, resize and encode.
const FIT_TIMEOUT: Duration = Duration::from_secs(30);
const JPEG_QUALITY: u8 = 85;
/// Resize and encode rounds before giving up.
const MAX_ROUNDS: u32 = 10;
/// A side shorter than this is no longer a picture a model can read.
const MIN_SIDE: u32 = 32;

/// What the bytes are, as far as the fit is concerned.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum ImageKind {
    /// PNG, JPEG, GIF or WebP, by its media type: sent as it is when it fits.
    Native(&'static str),
    /// A BMP: always rasterised.
    Bmp,
}

/// Why a picture could not be made to fit.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum FitError {
    /// Over the pixel or decode limits, or still too big at the smallest
    /// readable size: the skip `too_large`.
    TooLarge,
    /// The decoder refused the bytes: the skip `unreadable`.
    Unreadable,
}

/// A picture ready to send, and the digest of the ORIGINAL bytes (the cache
/// key: the same file is the same key whatever the fit made of it).
pub(crate) struct Fitted {
    pub(crate) bytes: Bytes,
    pub(crate) media_type: &'static str,
    pub(crate) digest: [u8; 32],
}

fn permits() -> &'static Semaphore {
    static PERMITS: OnceLock<Semaphore> = OnceLock::new();
    PERMITS.get_or_init(|| Semaphore::new(DECODES_AT_ONCE))
}

/// `original` as a picture of at most `budget` bytes.
///
/// # Errors
///
/// [`FitError`] when it cannot be read or cannot be made to fit.
pub(crate) async fn fit(
    original: Vec<u8>,
    kind: ImageKind,
    budget: usize,
) -> Result<Fitted, FitError> {
    let needs_decode = matches!(kind, ImageKind::Bmp)
        || matches!(kind, ImageKind::Native(_)) && original.len() > budget;
    if !needs_decode {
        let ImageKind::Native(media_type) = kind else {
            return Err(FitError::Unreadable);
        };
        // Hashing a small picture is quick, but it is still off the reactor.
        let work = tokio::task::spawn_blocking(move || {
            let digest = Sha256::digest(&original).into();
            Fitted {
                bytes: Bytes::from(original),
                media_type,
                digest,
            }
        });
        return work.await.map_err(|_| FitError::Unreadable);
    }
    let _permit = permits()
        .acquire()
        .await
        .map_err(|_| FitError::Unreadable)?;
    let work = tokio::task::spawn_blocking(move || {
        let digest: [u8; 32] = Sha256::digest(&original).into();
        shrink(&original, kind, budget).map(|(bytes, media_type)| Fitted {
            bytes: Bytes::from(bytes),
            media_type,
            digest,
        })
    });
    match tokio::time::timeout(FIT_TIMEOUT, work).await {
        Ok(Ok(result)) => result,
        Ok(Err(_)) => Err(FitError::Unreadable),
        Err(_) => {
            tracing::warn!(
                event = "index_image_fit_timed_out",
                "a picture took too long to decode; skipped too_large"
            );
            Err(FitError::TooLarge)
        }
    }
}

fn format_of(kind: ImageKind) -> ImageFormat {
    match kind {
        ImageKind::Bmp => ImageFormat::Bmp,
        ImageKind::Native("image/png") => ImageFormat::Png,
        ImageKind::Native("image/gif") => ImageFormat::Gif,
        ImageKind::Native("image/webp") => ImageFormat::WebP,
        ImageKind::Native(_) => ImageFormat::Jpeg,
    }
}

/// The decoded picture, within the limits.
fn decode(original: &[u8], format: ImageFormat) -> Result<DynamicImage, FitError> {
    let (width, height) = ImageReader::with_format(Cursor::new(original), format)
        .into_dimensions()
        .map_err(|_| FitError::Unreadable)?;
    if u64::from(width) * u64::from(height) > MAX_PIXELS {
        return Err(FitError::TooLarge);
    }
    let mut reader = ImageReader::with_format(Cursor::new(original), format);
    let mut limits = Limits::no_limits();
    limits.max_alloc = Some(MAX_DECODE_BYTES);
    reader.limits(limits);
    reader.decode().map_err(|error| match error {
        image::ImageError::Limits(_) => FitError::TooLarge,
        _ => FitError::Unreadable,
    })
}

/// Whether any pixel is not fully opaque.
fn has_transparency(picture: &DynamicImage) -> bool {
    picture.color().has_alpha() && picture.to_rgba8().pixels().any(|pixel| pixel.0[3] < 255)
}

fn encode(picture: &DynamicImage, png: bool) -> Result<Vec<u8>, FitError> {
    let mut out = Vec::new();
    if png {
        picture
            .write_to(&mut Cursor::new(&mut out), ImageFormat::Png)
            .map_err(|_| FitError::Unreadable)?;
    } else {
        // JPEG has no alpha channel.
        let rgb = DynamicImage::ImageRgb8(picture.to_rgb8());
        rgb.write_with_encoder(JpegEncoder::new_with_quality(&mut out, JPEG_QUALITY))
            .map_err(|_| FitError::Unreadable)?;
    }
    Ok(out)
}

#[allow(
    clippy::cast_precision_loss,
    clippy::cast_possible_truncation,
    clippy::cast_sign_loss
)] // Pixel sizes of a bounded picture, rounded to whole pixels.
fn scaled(side: u32, scale: f64) -> u32 {
    ((f64::from(side) * scale).round() as u32).max(1)
}

#[allow(clippy::cast_precision_loss)] // Byte counts only steer a ratio.
fn ratio(budget: usize, size: usize) -> f64 {
    budget as f64 / size.max(1) as f64
}

/// Decodes, shrinks and re-encodes until the picture fits `budget`.
fn shrink(
    original: &[u8],
    kind: ImageKind,
    budget: usize,
) -> Result<(Vec<u8>, &'static str), FitError> {
    let picture = decode(original, format_of(kind))?;
    let png = matches!(kind, ImageKind::Bmp) || has_transparency(&picture);
    let media_type = if png { "image/png" } else { "image/jpeg" };
    let (width, height) = picture.dimensions();
    // The first guess follows the file's own compression; a BMP (raw pixels)
    // is first tried whole.
    let mut scale = if matches!(kind, ImageKind::Bmp) {
        1.0
    } else {
        (ratio(budget, original.len()).sqrt() * 0.9).min(1.0)
    };
    for _ in 0..MAX_ROUNDS {
        let (w, h) = (scaled(width, scale), scaled(height, scale));
        if w < MIN_SIDE.min(width) || h < MIN_SIDE.min(height) {
            return Err(FitError::TooLarge);
        }
        let encoded = if w == width && h == height {
            encode(&picture, png)?
        } else {
            encode(&picture.resize_exact(w, h, FilterType::Triangle), png)?
        };
        if encoded.len() <= budget {
            return Ok((encoded, media_type));
        }
        scale *= (ratio(budget, encoded.len()).sqrt() * 0.9).min(0.85);
    }
    Err(FitError::TooLarge)
}

#[cfg(test)]
mod tests {
    use super::*;
    use image::{Rgb, RgbImage, Rgba, RgbaImage};

    const BUDGET: usize = 256 * 1024;

    /// Noise that does not compress: a pseudo-random picture.
    fn noisy(width: u32, height: u32) -> RgbImage {
        let mut state = 0x9e37_79b9_u32;
        RgbImage::from_fn(width, height, |_, _| {
            state = state.wrapping_mul(1_664_525).wrapping_add(1_013_904_223);
            let [a, b, c, _] = state.to_be_bytes();
            Rgb([a, b, c])
        })
    }

    fn encoded(picture: &DynamicImage, format: ImageFormat) -> Vec<u8> {
        let mut out = Vec::new();
        picture
            .write_to(&mut Cursor::new(&mut out), format)
            .expect("encode");
        out
    }

    #[tokio::test]
    async fn a_picture_within_the_budget_is_sent_as_it_is() {
        let png = encoded(&DynamicImage::ImageRgb8(noisy(16, 16)), ImageFormat::Png);
        let fitted = fit(png.clone(), ImageKind::Native("image/png"), BUDGET)
            .await
            .expect("fits");
        assert_eq!(fitted.bytes.as_ref(), png.as_slice());
        assert_eq!(fitted.media_type, "image/png");
        assert_eq!(fitted.digest, <[u8; 32]>::from(Sha256::digest(&png)));
    }

    #[tokio::test]
    async fn a_big_png_and_a_big_jpeg_are_downscaled_under_the_budget() {
        let picture = DynamicImage::ImageRgb8(noisy(900, 700));
        for (format, media) in [
            (ImageFormat::Png, "image/png"),
            (ImageFormat::Jpeg, "image/jpeg"),
        ] {
            let original = encoded(&picture, format);
            assert!(original.len() > BUDGET, "{media}: {}", original.len());
            let fitted = fit(original.clone(), ImageKind::Native(media), BUDGET)
                .await
                .unwrap_or_else(|error| panic!("{media}: {error:?}"));
            assert!(fitted.bytes.len() <= BUDGET, "{}", fitted.bytes.len());
            // Opaque pictures become JPEG; the digest is the original's.
            assert_eq!(fitted.media_type, "image/jpeg");
            assert_eq!(fitted.digest, <[u8; 32]>::from(Sha256::digest(&original)));
            let back = image::load_from_memory(&fitted.bytes).expect("decodes");
            let (w, h) = back.dimensions();
            assert!(w < 900 && h < 700 && w * 700 / 900 >= h - 2, "{w}x{h}");
        }
    }

    #[tokio::test]
    async fn a_big_picture_with_transparency_stays_a_png() {
        let mut rgba = RgbaImage::new(700, 700);
        let mut state = 7_u32;
        for pixel in rgba.pixels_mut() {
            state = state.wrapping_mul(1_664_525).wrapping_add(1_013_904_223);
            let [a, b, c, d] = state.to_be_bytes();
            *pixel = Rgba([a, b, c, if d < 128 { 0 } else { 255 }]);
        }
        let original = encoded(&DynamicImage::ImageRgba8(rgba), ImageFormat::Png);
        assert!(original.len() > BUDGET);
        let fitted = fit(original, ImageKind::Native("image/png"), BUDGET)
            .await
            .expect("fits");
        assert_eq!(fitted.media_type, "image/png");
        assert!(fitted.bytes.len() <= BUDGET);
    }

    #[tokio::test]
    async fn a_bmp_is_rasterised_to_png() {
        let bmp = encoded(&DynamicImage::ImageRgb8(noisy(40, 30)), ImageFormat::Bmp);
        let fitted = fit(bmp, ImageKind::Bmp, BUDGET).await.expect("fits");
        assert_eq!(fitted.media_type, "image/png");
        let back = image::load_from_memory(&fitted.bytes).expect("decodes");
        assert_eq!(back.dimensions(), (40, 30));
        // A BMP that is not one.
        assert_eq!(
            fit(b"BM not a bitmap".to_vec(), ImageKind::Bmp, BUDGET)
                .await
                .err(),
            Some(FitError::Unreadable)
        );
    }

    /// A PNG header that claims `width` x `height` and nothing else.
    fn bomb(width: u32, height: u32) -> Vec<u8> {
        fn crc32(bytes: &[u8]) -> u32 {
            let mut crc = !0_u32;
            for byte in bytes {
                crc ^= u32::from(*byte);
                for _ in 0..8 {
                    crc = if crc & 1 == 1 {
                        (crc >> 1) ^ 0xedb8_8320
                    } else {
                        crc >> 1
                    };
                }
            }
            !crc
        }
        let mut ihdr = Vec::new();
        ihdr.extend_from_slice(b"IHDR");
        ihdr.extend_from_slice(&width.to_be_bytes());
        ihdr.extend_from_slice(&height.to_be_bytes());
        ihdr.extend_from_slice(&[8, 2, 0, 0, 0]);
        let mut png = b"\x89PNG\r\n\x1a\n".to_vec();
        png.extend_from_slice(&13_u32.to_be_bytes());
        png.extend_from_slice(&ihdr);
        png.extend_from_slice(&crc32(&ihdr).to_be_bytes());
        // An image-data chunk, so the file is "over the budget" and the
        // header is all the decoder reads before it is refused.
        png.extend_from_slice(&u32::try_from(BUDGET).expect("len").to_be_bytes());
        png.extend_from_slice(b"IDAT");
        png.extend(std::iter::repeat_n(0_u8, BUDGET));
        png
    }

    #[tokio::test]
    async fn a_decompression_bomb_is_refused_by_the_pixel_limit() {
        // 100,000 x 100,000 = 10 gigapixels in a header.
        assert_eq!(
            fit(
                bomb(100_000, 100_000),
                ImageKind::Native("image/png"),
                BUDGET
            )
            .await
            .err(),
            Some(FitError::TooLarge)
        );
        // Just over the limit is refused, too.
        assert_eq!(
            fit(bomb(7_000, 6_000), ImageKind::Native("image/png"), BUDGET)
                .await
                .err(),
            Some(FitError::TooLarge)
        );
        // Bytes that are not the format are unreadable, not too large.
        assert_eq!(
            fit(
                vec![0xff; BUDGET + 1],
                ImageKind::Native("image/jpeg"),
                BUDGET
            )
            .await
            .err(),
            Some(FitError::Unreadable)
        );
    }
}
