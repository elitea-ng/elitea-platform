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
//! * at most [`DECODES_AT_ONCE`] pictures are decoded at the same time (a
//!   decoded 40 MP picture is 160 MB). The permit is owned by the blocking
//!   closure itself and released only when that work really ends, so a
//!   timed-out decode keeps its permit and the limit holds even after
//!   timeouts;
//! * the work is on the blocking pool and the caller's wait for it is
//!   bounded by [`FIT_TIMEOUT`] (a timeout is [`FitError::TimedOut`]);
//! * the decoded buffer and the one RGB copy a JPEG needs are counted
//!   against [`MAX_DECODE_BYTES`] together.
//!
//! The picture is resized proportionally and re-encoded as PNG when it has
//! transparency (or was a BMP) and as JPEG (quality [`JPEG_QUALITY`])
//! otherwise, shrinking until it fits. A picture that cannot be fit, or
//! read, is a typed [`FitError`], which the run reports as a per-document
//! skip.

use std::io::Cursor;
use std::sync::{Arc, OnceLock};
use std::time::Duration;

use bytes::Bytes;
use image::codecs::jpeg::JpegEncoder;
use image::imageops::FilterType;
use image::{DynamicImage, GenericImageView as _, ImageFormat, ImageReader, Limits};
use sha2::{Digest as _, Sha256};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};

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
    /// The decode did not end within [`FIT_TIMEOUT`]: the skip
    /// `extract_timeout`.
    TimedOut,
}

/// A picture ready to send.
pub(crate) struct Fitted {
    pub(crate) bytes: Bytes,
    pub(crate) media_type: &'static str,
}

/// sha256 of the ORIGINAL bytes: the description cache's key (the same file
/// is the same key whatever the fit makes of it). Hashed off the reactor.
pub(crate) async fn digest(original: Bytes) -> Option<[u8; 32]> {
    tokio::task::spawn_blocking(move || Sha256::digest(&original).into())
        .await
        .ok()
}

fn permits() -> &'static Arc<Semaphore> {
    static PERMITS: OnceLock<Arc<Semaphore>> = OnceLock::new();
    PERMITS.get_or_init(|| Arc::new(Semaphore::new(DECODES_AT_ONCE)))
}

/// `original` as a picture of at most `budget` bytes.
///
/// # Errors
///
/// [`FitError`] when it cannot be read or cannot be made to fit.
pub(crate) async fn fit(
    original: Bytes,
    kind: ImageKind,
    budget: usize,
) -> Result<Fitted, FitError> {
    fit_with(
        original,
        kind,
        budget,
        Arc::clone(permits()),
        FIT_TIMEOUT,
        shrink,
    )
    .await
}

/// The decode work of one picture, as [`shrink`].
type Work = fn(&[u8], ImageKind, usize) -> Result<(Vec<u8>, &'static str), FitError>;

async fn fit_with(
    original: Bytes,
    kind: ImageKind,
    budget: usize,
    permits: Arc<Semaphore>,
    timeout: Duration,
    work: Work,
) -> Result<Fitted, FitError> {
    let needs_decode = matches!(kind, ImageKind::Bmp)
        || matches!(kind, ImageKind::Native(_)) && original.len() > budget;
    if !needs_decode {
        let ImageKind::Native(media_type) = kind else {
            return Err(FitError::Unreadable);
        };
        return Ok(Fitted {
            bytes: original,
            media_type,
        });
    }
    let permit: OwnedSemaphorePermit = permits
        .acquire_owned()
        .await
        .map_err(|_| FitError::Unreadable)?;
    // The permit moves INTO the blocking closure: it is released when the
    // decode really ends, not when the caller gives up waiting for it.
    let job = tokio::task::spawn_blocking(move || {
        let _permit = permit;
        work(&original, kind, budget).map(|(bytes, media_type)| Fitted {
            bytes: Bytes::from(bytes),
            media_type,
        })
    });
    match tokio::time::timeout(timeout, job).await {
        Ok(Ok(result)) => result,
        Ok(Err(_)) => Err(FitError::Unreadable),
        Err(_) => {
            tracing::warn!(
                event = "index_image_fit_timed_out",
                "a picture took too long to decode; skipped extract_timeout"
            );
            Err(FitError::TimedOut)
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
    #[cfg(test)]
    decodes::record(original);
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

/// Whether any pixel is not fully opaque. Scans the picture's own buffer by
/// reference: no converted copy of a full-size picture.
fn has_transparency(picture: &DynamicImage) -> bool {
    match picture {
        DynamicImage::ImageLumaA8(image) => image.pixels().any(|pixel| pixel.0[1] < u8::MAX),
        DynamicImage::ImageRgba8(image) => image.pixels().any(|pixel| pixel.0[3] < u8::MAX),
        DynamicImage::ImageLumaA16(image) => image.pixels().any(|pixel| pixel.0[1] < u16::MAX),
        DynamicImage::ImageRgba16(image) => image.pixels().any(|pixel| pixel.0[3] < u16::MAX),
        DynamicImage::ImageRgba32F(image) => image.pixels().any(|pixel| pixel.0[3] < 1.0),
        _ => false,
    }
}

/// The picture a JPEG is encoded from: RGB, converted at most ONCE (an
/// already-RGB picture is moved, not copied).
fn into_rgb(picture: DynamicImage) -> DynamicImage {
    match picture {
        rgb @ DynamicImage::ImageRgb8(_) => rgb,
        other => DynamicImage::ImageRgb8(other.to_rgb8()),
    }
}

fn encode(picture: &DynamicImage, png: bool) -> Result<Vec<u8>, FitError> {
    let mut out = Vec::new();
    if png {
        picture
            .write_to(&mut Cursor::new(&mut out), ImageFormat::Png)
            .map_err(|_| FitError::Unreadable)?;
    } else {
        // JPEG has no alpha channel; `picture` is RGB already ([`into_rgb`]).
        picture
            .write_with_encoder(JpegEncoder::new_with_quality(&mut out, JPEG_QUALITY))
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
    // The decoded buffer and the RGB copy a JPEG needs live together once:
    // both count against the decode budget.
    let (width, height) = picture.dimensions();
    if !png && !matches!(picture, DynamicImage::ImageRgb8(_)) {
        let rgb_copy = u64::from(width) * u64::from(height) * 3;
        if u64::try_from(picture.as_bytes().len()).unwrap_or(u64::MAX) + rgb_copy > MAX_DECODE_BYTES
        {
            return Err(FitError::TooLarge);
        }
    }
    let picture = if png { picture } else { into_rgb(picture) };
    let media_type = if png { "image/png" } else { "image/jpeg" };
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

/// Test hook: how many times the bytes of a given picture were decoded
/// (keyed by their sha256, so tests of one process do not see each other).
#[cfg(test)]
pub(crate) mod decodes {
    use std::collections::HashMap;
    use std::sync::Mutex;

    use sha2::{Digest as _, Sha256};

    static COUNTS: Mutex<Option<HashMap<[u8; 32], usize>>> = Mutex::new(None);

    pub(super) fn record(original: &[u8]) {
        let key: [u8; 32] = Sha256::digest(original).into();
        *COUNTS
            .lock()
            .expect("counts")
            .get_or_insert_with(HashMap::new)
            .entry(key)
            .or_default() += 1;
    }

    /// Decodes of exactly these bytes so far.
    pub(crate) fn of(original: &[u8]) -> usize {
        let key: [u8; 32] = Sha256::digest(original).into();
        COUNTS
            .lock()
            .expect("counts")
            .as_ref()
            .and_then(|counts| counts.get(&key).copied())
            .unwrap_or(0)
    }
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
        let fitted = fit(png.clone().into(), ImageKind::Native("image/png"), BUDGET)
            .await
            .expect("fits");
        assert_eq!(fitted.bytes.as_ref(), png.as_slice());
        assert_eq!(fitted.media_type, "image/png");
        assert_eq!(
            digest(Bytes::from(png.clone())).await,
            Some(<[u8; 32]>::from(Sha256::digest(&png)))
        );
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
            let fitted = fit(original.clone().into(), ImageKind::Native(media), BUDGET)
                .await
                .unwrap_or_else(|error| panic!("{media}: {error:?}"));
            assert!(fitted.bytes.len() <= BUDGET, "{}", fitted.bytes.len());
            // Opaque pictures become JPEG.
            assert_eq!(fitted.media_type, "image/jpeg");
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
        let fitted = fit(original.into(), ImageKind::Native("image/png"), BUDGET)
            .await
            .expect("fits");
        assert_eq!(fitted.media_type, "image/png");
        assert!(fitted.bytes.len() <= BUDGET);
    }

    #[tokio::test]
    async fn a_bmp_is_rasterised_to_png() {
        let bmp = encoded(&DynamicImage::ImageRgb8(noisy(40, 30)), ImageFormat::Bmp);
        let fitted = fit(bmp.into(), ImageKind::Bmp, BUDGET).await.expect("fits");
        assert_eq!(fitted.media_type, "image/png");
        let back = image::load_from_memory(&fitted.bytes).expect("decodes");
        assert_eq!(back.dimensions(), (40, 30));
        // A BMP that is not one.
        assert_eq!(
            fit(
                Bytes::from_static(b"BM not a bitmap"),
                ImageKind::Bmp,
                BUDGET
            )
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
                bomb(100_000, 100_000).into(),
                ImageKind::Native("image/png"),
                BUDGET
            )
            .await
            .err(),
            Some(FitError::TooLarge)
        );
        // Just over the limit is refused, too.
        assert_eq!(
            fit(
                bomb(7_000, 6_000).into(),
                ImageKind::Native("image/png"),
                BUDGET
            )
            .await
            .err(),
            Some(FitError::TooLarge)
        );
        // Bytes that are not the format are unreadable, not too large.
        assert_eq!(
            fit(
                vec![0xff; BUDGET + 1].into(),
                ImageKind::Native("image/jpeg"),
                BUDGET
            )
            .await
            .err(),
            Some(FitError::Unreadable)
        );
    }

    #[test]
    fn transparency_is_read_from_the_pictures_own_buffer_in_every_alpha_layout() {
        let mut rgba = RgbaImage::from_pixel(4, 4, Rgba([1, 2, 3, 255]));
        assert!(!has_transparency(&DynamicImage::ImageRgba8(rgba.clone())));
        rgba.put_pixel(2, 2, Rgba([1, 2, 3, 254]));
        assert!(has_transparency(&DynamicImage::ImageRgba8(rgba)));
        let luma_a = image::GrayAlphaImage::from_pixel(2, 2, image::LumaA([9, 0]));
        assert!(has_transparency(&DynamicImage::ImageLumaA8(luma_a)));
        assert!(!has_transparency(&DynamicImage::ImageRgb8(noisy(4, 4))));
    }

    #[test]
    fn an_rgb_picture_is_moved_to_the_jpeg_encoder_not_copied() {
        let rgb = DynamicImage::ImageRgb8(noisy(64, 64));
        let before = rgb.as_bytes().as_ptr();
        let moved = into_rgb(rgb);
        assert_eq!(
            moved.as_bytes().as_ptr(),
            before,
            "no second full-size buffer"
        );
        // An opaque RGBA picture is converted once, to RGB.
        let rgba = DynamicImage::ImageRgba8(RgbaImage::from_pixel(8, 8, Rgba([1, 2, 3, 255])));
        assert!(matches!(into_rgb(rgba), DynamicImage::ImageRgb8(_)));
    }

    static RUNNING: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
    static PEAK: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);

    /// A decode that takes far longer than the test's timeout.
    fn slow_work(_: &[u8], _: ImageKind, _: usize) -> Result<(Vec<u8>, &'static str), FitError> {
        use std::sync::atomic::Ordering;
        let now = RUNNING.fetch_add(1, Ordering::SeqCst) + 1;
        PEAK.fetch_max(now, Ordering::SeqCst);
        std::thread::sleep(Duration::from_millis(250));
        RUNNING.fetch_sub(1, Ordering::SeqCst);
        Err(FitError::Unreadable)
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn decodes_at_once_stay_within_the_limit_even_after_timeouts() {
        use std::sync::atomic::Ordering;
        let permits = Arc::new(Semaphore::new(2));
        let mut calls = Vec::new();
        for _ in 0..6 {
            calls.push(tokio::spawn(fit_with(
                Bytes::from_static(b"BM"),
                ImageKind::Bmp,
                BUDGET,
                Arc::clone(&permits),
                Duration::from_millis(30),
                slow_work,
            )));
        }
        for call in calls {
            // The caller returns at its timeout, whatever the decode does.
            assert_eq!(call.await.expect("joined").err(), Some(FitError::TimedOut));
        }
        // Wait for the abandoned decodes to end.
        tokio::time::sleep(Duration::from_millis(400)).await;
        assert!(
            PEAK.load(Ordering::SeqCst) <= 2,
            "{}",
            PEAK.load(Ordering::SeqCst)
        );
        assert_eq!(RUNNING.load(Ordering::SeqCst), 0);
        assert_eq!(permits.available_permits(), 2, "every permit came back");
    }
}
