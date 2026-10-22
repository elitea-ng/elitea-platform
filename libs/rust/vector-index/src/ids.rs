//! The names the engines give their vectors in `elitea-vector`, shared by
//! the `DeepWiki` and Inventory engines so both name a space, a point and a
//! namespace by one rule.
//!
//! Every function here addresses STORED vectors: a change orphans them.
//! The tests pin their outputs.

use sha2::{Digest as _, Sha256};
use std::fmt::Write as _;
use uuid::Uuid;

/// The longest `document_key` / `chunk_id` `elitea-vector` accepts.
pub const MAX_KEY_BYTES: usize = 1024;
/// `elitea-vector`'s model-slug limit (`^[a-z0-9][a-z0-9-]{0,62}$`).
pub const MAX_SLUG_BYTES: usize = 63;
/// How many hex digits of the model name's SHA-256 a slug carries.
const SLUG_HASH_HEX: usize = 12;
/// Room for the readable part of a slug.
const MAX_SLUG_READABLE: usize = MAX_SLUG_BYTES - 1 - SLUG_HASH_HEX;

/// The embedding space's model slug of the model name `model` (`""` for
/// none): the platform's rule, byte for byte the indexing worker's
/// (`elitea-worker-rust/src/indexing/slug.rs`) — the lower-cased name's runs
/// of `[a-z0-9]` joined by single dashes, cut to 50 bytes (`model` when
/// nothing is left), then `-` and the first 12 hex digits of the SHA-256 of
/// the name as given. The hash keeps two names that read alike apart.
#[must_use]
pub fn model_slug(model: &str) -> String {
    let mut readable = String::with_capacity(model.len());
    let mut pending_dash = false;
    for character in model.to_lowercase().chars() {
        if character.is_ascii_lowercase() || character.is_ascii_digit() {
            if pending_dash && !readable.is_empty() {
                readable.push('-');
            }
            pending_dash = false;
            readable.push(character);
        } else {
            pending_dash = true;
        }
    }
    if readable.len() > MAX_SLUG_READABLE {
        readable.truncate(MAX_SLUG_READABLE);
        while readable.ends_with('-') {
            readable.pop();
        }
    }
    if readable.is_empty() {
        readable.push_str("model");
    }
    let digest = Sha256::digest(model.as_bytes());
    let mut slug = readable;
    slug.push('-');
    for byte in digest.iter().take(SLUG_HASH_HEX / 2) {
        let _ = write!(slug, "{byte:02x}");
    }
    slug
}

/// The `document_key` / `chunk_id` of the item `id` (an entity, a page
/// node): the id itself when `elitea-vector` can hold it as a key (1 to
/// [`MAX_KEY_BYTES`] bytes, no control character), else `sha256:<hex>` of
/// it.
#[must_use]
pub fn point_key(id: &str) -> String {
    if !id.is_empty() && id.len() <= MAX_KEY_BYTES && !id.chars().any(char::is_control) {
        return id.to_owned();
    }
    let mut key = String::from("sha256:");
    for byte in Sha256::digest(id.as_bytes()) {
        let _ = write!(key, "{byte:02x}");
    }
    key
}

/// The namespace id of `name` under the `UUIDv5` namespace `root` (RFC 9562
/// section 5.5), in the canonical lower-case text form `elitea-vector`
/// requires.
#[must_use]
pub fn namespace_v5(root: [u8; 16], name: &str) -> String {
    Uuid::new_v5(&Uuid::from_bytes(root), name.as_bytes())
        .hyphenated()
        .to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sha256_hex(text: &str) -> String {
        let mut hex = String::new();
        for byte in Sha256::digest(text.as_bytes()) {
            let _ = write!(hex, "{byte:02x}");
        }
        hex
    }

    /// `UUIDv5(NAMESPACE_URL, "https://elitea.ai/ns/inventory/graph-vectors")`.
    const INVENTORY_ROOT: [u8; 16] = [
        0x63, 0x4f, 0xdd, 0x4c, 0x7b, 0x6b, 0x56, 0x48, 0x80, 0xb0, 0x46, 0xde, 0x40, 0x56, 0x04,
        0x43,
    ];

    #[test]
    fn namespaces_are_byte_identical_to_the_stored_ids() {
        // The first three are the ids the Inventory engine's hand-rolled
        // UUIDv5 (SHA-1, version and variant bits set by hand) gave the
        // graphs (1, 1), (7, 42) and (i32::MAX, i64::MAX), pinned with
        // elitea-main's InventoryGraphNamespace (Go); every one is Python's
        // uuid.uuid5(root, name), an implementation independent of `uuid`.
        for (name, pinned) in [
            ("1/1", "09265e87-a97c-53e6-8062-d685fb6cf4ad"),
            ("7/42", "454412b3-aea7-5ecf-aefe-49264f4557d0"),
            (
                "2147483647/9223372036854775807",
                "3111a581-a4af-549c-af63-b217cec52eca",
            ),
            ("", "ae0f06ce-b56d-5979-8c88-e012672b23a7"),
            ("x", "62f8442b-9081-5f85-910d-ef75033a6101"),
            ("12/34", "a6925872-b453-5985-8225-fcafe729dfec"),
            (&"é".repeat(300), "de09a56e-86a6-55a4-80b8-4107de65c9fb"),
        ] {
            assert_eq!(namespace_v5(INVENTORY_ROOT, name), pinned, "{name}");
        }
        // The root itself is UUIDv5(NAMESPACE_URL, …).
        assert_eq!(
            namespace_v5(
                *Uuid::NAMESPACE_URL.as_bytes(),
                "https://elitea.ai/ns/inventory/graph-vectors"
            ),
            "634fdd4c-7b6b-5648-80b0-46de40560443"
        );
        assert_eq!(
            Uuid::from_bytes(INVENTORY_ROOT).hyphenated().to_string(),
            "634fdd4c-7b6b-5648-80b0-46de40560443"
        );
        // DeepWiki's: uuid.uuid5(UUID("5669574f-…"), "7/acme--notes--main").
        let deepwiki = 0x5669_574f_401d_4878_a59e_0c34_ecc4_3970_u128.to_be_bytes();
        assert_eq!(
            namespace_v5(deepwiki, "7/acme--notes--main"),
            "c903dad9-fa43-5b23-93df-c52ad96e4d9c"
        );
    }

    #[test]
    fn the_slug_is_the_platforms_vector_space_slug() {
        assert_eq!(
            model_slug("text-embedding-3-small"),
            format!(
                "text-embedding-3-small-{}",
                &sha256_hex("text-embedding-3-small")[..12]
            )
        );
        assert_eq!(
            model_slug("Org/Model.A"),
            format!("org-model-a-{}", &sha256_hex("Org/Model.A")[..12])
        );
        assert_eq!(
            model_slug("///"),
            format!("model-{}", &sha256_hex("///")[..12])
        );
        assert_eq!(model_slug(""), format!("model-{}", &sha256_hex("")[..12]));
        assert_eq!(model_slug(&"a".repeat(100)).len(), MAX_SLUG_BYTES);
        assert_ne!(model_slug("Model"), model_slug("model"));
        assert_ne!(
            model_slug("A_b"),
            model_slug("a-b"),
            "the hash keeps them apart"
        );
        for model in [
            "",
            "x",
            "Model",
            "Ünïcode-Ëmbed",
            "-x-",
            "a_b c",
            "Qwen/Qwen3-Embedding-4B",
            &"é".repeat(100),
            &"ab-".repeat(60),
        ] {
            let slug = model_slug(model);
            let bytes = slug.as_bytes();
            assert!(
                bytes[0].is_ascii_lowercase() || bytes[0].is_ascii_digit(),
                "{slug}"
            );
            assert!(slug.len() <= MAX_SLUG_BYTES, "{slug}");
            assert!(
                bytes
                    .iter()
                    .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || *b == b'-'),
                "{slug}"
            );
        }
    }

    #[test]
    fn keys_are_ids_unless_the_store_cannot_hold_them() {
        assert_eq!(point_key("py::mod::f"), "py::mod::f");
        assert_eq!(point_key(&"x".repeat(1024)), "x".repeat(1024));
        let long = "x".repeat(1025);
        assert_eq!(point_key(&long), format!("sha256:{}", sha256_hex(&long)));
        assert!(point_key("a\nb").starts_with("sha256:"));
        assert_eq!(point_key(""), format!("sha256:{}", sha256_hex("")));
    }
}
