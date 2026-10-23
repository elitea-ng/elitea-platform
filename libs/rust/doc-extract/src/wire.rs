//! The request a document-extraction sidecar reads on stdin, so the host that
//! sends it and the sidecar that reads it agree on one definition.
//!
//! One request per process, no paths: the host reads the file through its
//! confined workspace and sends the bytes.
//!
//! ```text
//! offset size  field
//!      0    4  b"EDX1"
//!      4    2  mime length (u16, little endian)
//!      6    4  first page, 1-based (u32 LE)
//!     10    4  pages wanted (u32 LE); 0: to the last
//!     14    2  reserved, zero
//!     16    n  the media type (ASCII), then the file's bytes to end of input
//! ```
//!
//! The answer is one JSON object on stdout: `{"ok":true,"extractor":…,
//! "page_count":N,"pages":[{"n":1,"text":"…"}]}` or `{"ok":false,"error":
//! "unreadable"|"unsupported"|"timeout","message":"…"}`.

use crate::PageRange;

/// The first four bytes of a request.
pub const MAGIC: &[u8; 4] = b"EDX1";
/// The fixed part of a request.
pub const HEADER_LEN: usize = 16;
/// The longest media type a request may carry.
pub const MAX_MIME_LEN: usize = 255;

/// A request's fixed part, decoded.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Header {
    pub mime_len: usize,
    pub range: PageRange,
}

/// The request's fixed part and media type, followed (by the caller) by the
/// file's bytes.
///
/// # Errors
/// The media type is too long to carry.
pub fn encode_request_head(mime: &str, range: &PageRange) -> Result<Vec<u8>, String> {
    if mime.len() > MAX_MIME_LEN || !mime.is_ascii() {
        return Err(format!("`{mime}` is not a media type a request can carry"));
    }
    let count = range
        .to
        .map_or(0, |to| to.saturating_sub(range.from).saturating_add(1));
    let mut head = Vec::with_capacity(HEADER_LEN + mime.len());
    head.extend_from_slice(MAGIC);
    // Checked above: at most 255.
    head.extend_from_slice(&u16::try_from(mime.len()).unwrap_or(0).to_le_bytes());
    head.extend_from_slice(&range.from.to_le_bytes());
    head.extend_from_slice(&count.to_le_bytes());
    head.extend_from_slice(&[0, 0]);
    head.extend_from_slice(mime.as_bytes());
    Ok(head)
}

/// Decodes the fixed part of a request.
///
/// # Errors
/// The magic is wrong, or the first page is 0.
pub fn decode_header(bytes: &[u8; HEADER_LEN]) -> Result<Header, String> {
    if &bytes[..4] != MAGIC {
        return Err("not an EDX1 request".to_owned());
    }
    let mime_len = usize::from(u16::from_le_bytes([bytes[4], bytes[5]]));
    let from = u32::from_le_bytes([bytes[6], bytes[7], bytes[8], bytes[9]]);
    let count = u32::from_le_bytes([bytes[10], bytes[11], bytes[12], bytes[13]]);
    if from == 0 {
        return Err("pages are numbered from 1".to_owned());
    }
    let to = (count != 0).then(|| from.saturating_add(count - 1));
    Ok(Header {
        mime_len,
        range: PageRange { from, to },
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_request_head_round_trips() {
        for range in [
            PageRange::ALL,
            PageRange {
                from: 7,
                to: Some(9),
            },
            PageRange { from: 3, to: None },
        ] {
            let head = encode_request_head("application/pdf", &range).expect("head");
            let fixed: [u8; HEADER_LEN] = head[..HEADER_LEN].try_into().expect("16");
            let decoded = decode_header(&fixed).expect("decoded");
            assert_eq!(decoded.range, range);
            assert_eq!(&head[HEADER_LEN..], b"application/pdf");
            assert_eq!(decoded.mime_len, "application/pdf".len());
        }
    }

    #[test]
    fn a_bad_head_is_refused() {
        assert!(decode_header(&[0; HEADER_LEN]).is_err());
        assert!(encode_request_head("é", &PageRange::ALL).is_err());
    }
}
