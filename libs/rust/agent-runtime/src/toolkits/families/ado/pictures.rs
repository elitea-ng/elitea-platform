//! The picture checks and the description call that the ADO families share
//! (#985): `get_image_by_url` and the inline descriptions of `ado_boards`, and
//! the wiki page reads of `ado_wiki`.

use crate::toolkits::families::attachment_host::{
    AttachmentError, AttachmentHost, MAX_PICTURE_BYTES, NO_READABLE_TEXT, SDK_IMAGE_PROMPT,
    detect_media_type, is_describable_image,
};

/// `_MAX_IMAGE_PIXELS_FOR_LLM`.
const MAX_IMAGE_PIXELS: u64 = 36_000_000;

/// The checks every picture passes before the model sees it (the SDK's
/// `_fetch_validated_image` after the download): at most 5 MB, a PNG, JPEG,
/// GIF or WebP with a readable size, and at most [`MAX_IMAGE_PIXELS`]
/// pixels. `Ok` is the media type; `Err` is the reason, as a sentence.
pub(crate) fn checked_picture(bytes: &[u8], name: &str) -> Result<&'static str, String> {
    if bytes.len() as u64 > MAX_PICTURE_BYTES {
        return Err("Image exceeds the 5 MB processing limit".to_owned());
    }
    let media_type = detect_media_type(bytes, name);
    let Some((width, height)) =
        image_dimensions(bytes).filter(|_| is_describable_image(media_type))
    else {
        return Err("Could not read image dimensions".to_owned());
    };
    if width == 0 || height == 0 {
        return Err("Could not read image dimensions".to_owned());
    }
    if width.saturating_mul(height) > MAX_IMAGE_PIXELS {
        return Err(format!(
            "Image dimensions {width}x{height} exceed the processing limit"
        ));
    }
    Ok(media_type)
}

/// Describe one checked picture with the run's chat model: `prompt` when it
/// is not empty, else the SDK's default image prompt ([`SDK_IMAGE_PROMPT`]).
/// An empty reply is the SDK's placeholder ([`NO_READABLE_TEXT`]).
///
/// This does not use `attachment_host::describe_for_text`: that function
/// returns a failure of one picture as description text. The ADO tools keep
/// such a failure apart: `get_image_by_url` reports it as an error, and a
/// wiki page keeps the picture's URL with a note instead of putting the
/// failure text in place of the URL.
pub(crate) async fn describe_picture(
    host: &AttachmentHost,
    bytes: Vec<u8>,
    media_type: &'static str,
    prompt: Option<&str>,
) -> Result<String, AttachmentError> {
    let prompt = prompt
        .filter(|prompt| !prompt.is_empty())
        .unwrap_or(SDK_IMAGE_PROMPT)
        .to_owned();
    let description = host.describe_picture(bytes, media_type, prompt).await?;
    if description.trim().is_empty() {
        Ok(NO_READABLE_TEXT.to_owned())
    } else {
        Ok(description)
    }
}

/// The pixel size of a PNG, GIF, JPEG or WebP, read from its header.
fn image_dimensions(bytes: &[u8]) -> Option<(u64, u64)> {
    let be32 = |at: usize| {
        bytes
            .get(at..at + 4)
            .map(|b| u64::from(u32::from_be_bytes([b[0], b[1], b[2], b[3]])))
    };
    let be16 = |at: usize| {
        bytes
            .get(at..at + 2)
            .map(|b| u64::from(u16::from_be_bytes([b[0], b[1]])))
    };
    let le16 = |at: usize| {
        bytes
            .get(at..at + 2)
            .map(|b| u64::from(u16::from_le_bytes([b[0], b[1]])))
    };
    let le24 = |at: usize| {
        bytes
            .get(at..at + 3)
            .map(|b| u64::from(b[0]) | u64::from(b[1]) << 8 | u64::from(b[2]) << 16)
    };
    if bytes.starts_with(&[0x89, b'P', b'N', b'G', 0x0D, 0x0A, 0x1A, 0x0A]) {
        return Some((be32(16)?, be32(20)?));
    }
    if bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a") {
        return Some((le16(6)?, le16(8)?));
    }
    if bytes.len() >= 30 && &bytes[..4] == b"RIFF" && &bytes[8..12] == b"WEBP" {
        return match &bytes[12..16] {
            b"VP8X" => Some((le24(24)? + 1, le24(27)? + 1)),
            b"VP8L" => {
                let bits = u32::from_le_bytes([
                    *bytes.get(21)?,
                    *bytes.get(22)?,
                    *bytes.get(23)?,
                    *bytes.get(24)?,
                ]);
                Some((
                    u64::from(bits & 0x3FFF) + 1,
                    u64::from((bits >> 14) & 0x3FFF) + 1,
                ))
            }
            b"VP8 " => Some((le16(26)? & 0x3FFF, le16(28)? & 0x3FFF)),
            _ => None,
        };
    }
    if bytes.starts_with(&[0xFF, 0xD8]) {
        let mut at = 2;
        while at + 4 <= bytes.len() {
            if bytes[at] != 0xFF {
                return None;
            }
            let marker = bytes[at + 1];
            if marker == 0xFF {
                at += 1;
                continue;
            }
            if (0xD0..=0xD9).contains(&marker) || marker == 0x01 {
                at += 2;
                continue;
            }
            let length = usize::try_from(be16(at + 2)?).ok()?;
            if matches!(marker, 0xC0..=0xC3 | 0xC5..=0xC7 | 0xC9..=0xCB | 0xCD..=0xCF) {
                return Some((be16(at + 7)?, be16(at + 5)?));
            }
            at += 2 + length;
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dimensions_come_from_the_headers_of_the_four_picture_types() {
        let png = crate::toolkits::attachment_test_support::PNG;
        assert_eq!(image_dimensions(png), Some((1, 1)));
        let mut gif = b"GIF89a".to_vec();
        gif.extend([3, 0, 2, 0]);
        assert_eq!(image_dimensions(&gif), Some((3, 2)));
        // SOI, APP0 (length 4), SOF0 with 7x5.
        let jpeg = [
            0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x04, 0x00, 0x00, 0xFF, 0xC0, 0x00, 0x0B, 0x08, 0x00,
            0x05, 0x00, 0x07, 0x01,
        ];
        assert_eq!(image_dimensions(&jpeg), Some((7, 5)));
        let mut webp = b"RIFF\0\0\0\0WEBPVP8X\x0a\0\0\0\0\0\0\0".to_vec();
        webp.extend([9, 0, 0, 3, 0, 0]);
        assert_eq!(image_dimensions(&webp), Some((10, 4)));
        assert_eq!(image_dimensions(b"not a picture"), None);
    }
}
