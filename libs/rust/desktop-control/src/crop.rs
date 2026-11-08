//! Cropping a window capture to a region and re-encoding it as PNG, for
//! every driver that has capture.

use crate::DesktopError;

/// Crops a window capture to the pixel box (`x`, `y`, `w`, `h`) and
/// re-encodes it as PNG.
pub(crate) fn crop(png: &[u8], x: u32, y: u32, w: u32, h: u32) -> Result<Vec<u8>, DesktopError> {
    let img = image::load_from_memory_with_format(png, image::ImageFormat::Png)
        .map_err(|e| DesktopError::Internal(format!("decode capture: {e}")))?;
    let cropped = img.crop_imm(x, y, w, h);
    let mut out = std::io::Cursor::new(Vec::new());
    cropped
        .write_to(&mut out, image::ImageFormat::Png)
        .map_err(|e| DesktopError::Internal(format!("encode capture: {e}")))?;
    Ok(out.into_inner())
}
