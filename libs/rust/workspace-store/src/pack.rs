//! A git pack (version 2) of whole objects: no deltas, so writing it needs
//! nothing but the objects and zlib, and every reader (git, gix) indexes it.
//! This is the writer of a host without git (the cloud worker). The desktop
//! builds deltified packs with its hardened git (`git pack-objects`) and
//! wraps them with [`crate::bundle::write_compact_with_pack`]; readers take
//! either.

use std::io::Write as _;

use gix_hash::ObjectId;
use gix_object::Kind;

use crate::error::StoreError;
use crate::objects::ObjectDb;

fn type_number(kind: Kind) -> u8 {
    match kind {
        Kind::Commit => 1,
        Kind::Tree => 2,
        Kind::Blob => 3,
        Kind::Tag => 4,
    }
}

/// The entry header: type and size, little-endian 7-bit groups.
fn entry_header(kind: Kind, size: usize, out: &mut Vec<u8>) {
    let mut size = size as u64;
    let mut byte = (type_number(kind) << 4) | u8::try_from(size & 0x0f).unwrap_or(0);
    size >>= 4;
    while size > 0 {
        out.push(byte | 0x80);
        byte = u8::try_from(size & 0x7f).unwrap_or(0);
        size >>= 7;
    }
    out.push(byte);
}

/// Append a pack of `ids` (read from `db`) to `out`: whole objects, so a
/// reader (a segment, or a git server's `receive-pack`) needs no base.
///
/// # Errors
///
/// An object of `ids` is missing from `db`, or there are too many.
pub fn write(db: &ObjectDb, ids: &[ObjectId], out: &mut Vec<u8>) -> Result<(), StoreError> {
    let start = out.len();
    let count = u32::try_from(ids.len())
        .map_err(|_| StoreError::Invalid("too many objects for one pack".into()))?;
    out.extend_from_slice(b"PACK");
    out.extend_from_slice(&2u32.to_be_bytes());
    out.extend_from_slice(&count.to_be_bytes());
    for id in ids {
        let Some((kind, data)) = db.find(id)? else {
            return Err(StoreError::Integrity(format!("object {id} is missing")));
        };
        entry_header(kind, data.len(), out);
        let mut encoder =
            flate2::write::ZlibEncoder::new(Vec::new(), flate2::Compression::default());
        encoder
            .write_all(&data)
            .map_err(|error| StoreError::io("cannot compress an object", &error))?;
        let compressed = encoder
            .finish()
            .map_err(|error| StoreError::io("cannot compress an object", &error))?;
        out.extend_from_slice(&compressed);
    }
    let mut hasher = gix_hash::hasher(gix_hash::Kind::Sha1);
    hasher.update(&out[start..]);
    let trailer = hasher
        .try_finalize()
        .map_err(|_| StoreError::Integrity("the pack checksum could not be computed".into()))?;
    out.extend_from_slice(trailer.as_slice());
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::entry_header;
    use gix_object::Kind;

    #[test]
    fn entry_headers_are_git_varints() {
        let mut out = Vec::new();
        entry_header(Kind::Blob, 5, &mut out);
        assert_eq!(out, [0x35]);
        out.clear();
        entry_header(Kind::Commit, 300, &mut out);
        // 300 = 0b1_0010_1100: low nibble 0xc, then 300 >> 4 = 18.
        assert_eq!(out, [0x80 | 0x10 | 0x0c, 18]);
    }
}
