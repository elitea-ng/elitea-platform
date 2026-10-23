//! Main's vector-space slug (`indexregistry.VectorSpaceSlug`), reproduced so
//! the worker writes into the collection the registry names.
//!
//! The slug is lower-case letters, digits and `-`, at most 63 bytes, starting
//! with a letter or digit: a readable part (the lower-cased model with every
//! run of other characters replaced by one `-`, trimmed and truncated), then
//! `-` and the first 12 hex digits of the SHA-256 of the EXACT model string.
//! The hash is what makes it lossless: two models that differ in case,
//! punctuation or only past the truncation point get different slugs.

/// The slug of `model`, byte for byte what Main computes: the platform's
/// rule, shared with the engines (`elitea_vector_index::ids::model_slug`).
#[must_use]
pub(crate) fn vector_space_slug(model: &str) -> String {
    elitea_vector_index::ids::model_slug(model)
}

#[cfg(test)]
mod tests {
    use super::*;
    use elitea_vector_index::ids::MAX_SLUG_BYTES;
    use ring::digest;
    use std::fmt::Write as _;

    /// Room for the readable part: the limit less `-` and the hash.
    const MAX_READABLE_BYTES: usize = MAX_SLUG_BYTES - 1 - 12;

    #[test]
    fn matches_mains_slug_rules() {
        // Go: strings.ToLower, runs of [^a-z0-9]+ -> "-", Trim "-", then the
        // first 12 hex digits of sha256(model).
        let slug = vector_space_slug("text-embedding-3-small");
        assert!(slug.starts_with("text-embedding-3-small-"));
        assert_eq!(slug.len(), "text-embedding-3-small-".len() + 12);
        assert_eq!(
            vector_space_slug("Org/Model.A"),
            format!("org-model-a-{}", &sha256_hex("Org/Model.A")[..12])
        );
        assert_eq!(
            vector_space_slug("///"),
            format!("model-{}", &sha256_hex("///")[..12])
        );
        let long = "a".repeat(100);
        let slug = vector_space_slug(&long);
        assert_eq!(slug.len(), MAX_SLUG_BYTES);
        assert!(slug.starts_with(&"a".repeat(MAX_READABLE_BYTES)));
        // Two models that differ only in case share a readable part but not
        // a slug.
        assert_ne!(vector_space_slug("Model"), vector_space_slug("model"));
        assert!(
            vector_space_slug("Model")
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        );
    }

    fn sha256_hex(text: &str) -> String {
        let mut hex = String::new();
        for byte in digest::digest(&digest::SHA256, text.as_bytes()).as_ref() {
            let _ = write!(hex, "{byte:02x}");
        }
        hex
    }
}
