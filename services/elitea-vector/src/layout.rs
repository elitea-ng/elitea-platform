//! The Qdrant layout of ADR-0031 decision 2: collection names, payload keys,
//! sources, namespaces and point ids.

use tonic::Status;
use uuid::Uuid;

use crate::pb;

/// Every collection this service owns starts with this prefix.
pub const COLLECTION_PREFIX: &str = "emb_";
/// The named dense vector of every point.
pub const DENSE_VECTOR: &str = "dense";
/// The named sparse vector that Qdrant fills with BM25 term weights.
pub const BM25_VECTOR: &str = "bm25";
/// The server-side BM25 model (Qdrant's built-in inference).
pub const BM25_MODEL: &str = "qdrant/bm25";
/// The largest dimension Qdrant accepts.
pub const MAX_DIMENSION: u32 = 65_536;

/// Payload keys.
pub mod key {
    pub const PROJECT_ID: &str = "project_id";
    pub const SOURCE: &str = "source";
    pub const NAMESPACE_ID: &str = "namespace_id";
    pub const GENERATION: &str = "generation";
    pub const DOCUMENT_KEY: &str = "document_key";
    pub const DOCUMENT_VERSION: &str = "document_version";
    pub const PARENT_ID: &str = "parent_id";
    pub const CHUNK_ID: &str = "chunk_id";
    pub const CHUNK_TYPE: &str = "chunk_type";
    pub const ACL: &str = "acl";
    pub const TEXT: &str = "text";
    pub const METADATA: &str = "metadata";
}

/// The keyword-indexed payload keys, `project_id` (the tenant) excluded.
pub const KEYWORD_INDEXES: [&str; 8] = [
    key::SOURCE,
    key::NAMESPACE_ID,
    key::GENERATION,
    key::DOCUMENT_KEY,
    key::DOCUMENT_VERSION,
    key::PARENT_ID,
    key::CHUNK_TYPE,
    key::ACL,
];

/// One embedding space: a model slug and a dimension.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct Space {
    slug: String,
    dimension: u32,
}

impl Space {
    /// Validates a requested space.
    ///
    /// # Errors
    /// `INVALID_ARGUMENT` for a missing space, a bad slug or a bad dimension.
    pub fn from_proto(space: Option<&pb::EmbeddingSpace>) -> Result<Self, Status> {
        let space = space.ok_or_else(|| Status::invalid_argument("space is required"))?;
        Self::new(&space.model_slug, space.dimension)
    }

    /// Validates a space.
    ///
    /// # Errors
    /// `INVALID_ARGUMENT` for a bad slug or dimension.
    pub fn new(slug: &str, dimension: u32) -> Result<Self, Status> {
        if !valid_slug(slug) {
            return Err(Status::invalid_argument(
                "space.model_slug must match ^[a-z0-9][a-z0-9-]{0,62}$",
            ));
        }
        if dimension == 0 || dimension > MAX_DIMENSION {
            return Err(Status::invalid_argument(
                "space.dimension must be 1 to 65536",
            ));
        }
        Ok(Self {
            slug: slug.to_owned(),
            dimension,
        })
    }

    /// The collection name, `emb_<slug>_<dimension>`.
    #[must_use]
    pub fn collection(&self) -> String {
        format!("{COLLECTION_PREFIX}{}_{}", self.slug, self.dimension)
    }

    /// The model slug.
    #[must_use]
    pub fn slug(&self) -> &str {
        &self.slug
    }

    /// The dimension.
    #[must_use]
    pub fn dimension(&self) -> u32 {
        self.dimension
    }

    /// Parses a collection name back into a space. A name this service did
    /// not make gives `None`.
    #[must_use]
    pub fn from_collection(name: &str) -> Option<Self> {
        let rest = name.strip_prefix(COLLECTION_PREFIX)?;
        let (slug, dimension) = rest.rsplit_once('_')?;
        let dimension = dimension.parse::<u32>().ok()?;
        if dimension.to_string() != rest.rsplit_once('_')?.1 {
            return None;
        }
        Self::new(slug, dimension).ok()
    }

    /// The protocol form.
    #[must_use]
    pub fn to_proto(&self) -> pb::EmbeddingSpace {
        pb::EmbeddingSpace {
            model_slug: self.slug.clone(),
            dimension: self.dimension,
        }
    }
}

fn valid_slug(slug: &str) -> bool {
    let bytes = slug.as_bytes();
    !bytes.is_empty()
        && bytes.len() <= 63
        && (bytes[0].is_ascii_lowercase() || bytes[0].is_ascii_digit())
        && bytes
            .iter()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || *byte == b'-')
}

/// The payload value of a source.
///
/// # Errors
/// `INVALID_ARGUMENT` for an unset source; `FAILED_PRECONDITION` for
/// `inventory`, which ADR-0031 decision 7 reserves.
pub fn source_keyword(source: i32) -> Result<&'static str, Status> {
    match pb::Source::try_from(source) {
        Ok(pb::Source::ToolkitIndex) => Ok("toolkit_index"),
        Ok(pb::Source::Deepwiki) => Ok("deepwiki"),
        Ok(pb::Source::Inventory) => Err(Status::failed_precondition(
            "source inventory is reserved and not served",
        )),
        Ok(pb::Source::Unspecified) | Err(_) => Err(Status::invalid_argument("source is required")),
    }
}

/// The source of a payload value; an unknown value gives `Unspecified`.
#[must_use]
pub fn source_from_keyword(keyword: &str) -> pb::Source {
    match keyword {
        "toolkit_index" => pb::Source::ToolkitIndex,
        "deepwiki" => pb::Source::Deepwiki,
        "inventory" => pb::Source::Inventory,
        _ => pb::Source::Unspecified,
    }
}

/// Validates a namespace id: a UUID in canonical, lower-case text form.
///
/// # Errors
/// `INVALID_ARGUMENT` for anything else.
pub fn namespace_uuid(namespace_id: &str) -> Result<Uuid, Status> {
    Uuid::parse_str(namespace_id)
        .ok()
        .filter(|uuid| uuid.hyphenated().to_string() == namespace_id)
        .ok_or_else(|| Status::invalid_argument("namespace_id must be a canonical lower-case UUID"))
}

/// Validates a generation. Empty means "no generation".
///
/// # Errors
/// `INVALID_ARGUMENT` for more than 128 bytes or a byte outside
/// `[A-Za-z0-9._:-]`.
pub fn generation(value: &str) -> Result<Option<&str>, Status> {
    if value.is_empty() {
        return Ok(None);
    }
    if value.len() > 128
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b':' | b'-'))
    {
        return Err(Status::invalid_argument(
            "generation must be 1 to 128 bytes of [A-Za-z0-9._:-]",
        ));
    }
    Ok(Some(value))
}

/// The point id: `UUIDv5` under the namespace UUID of (project, generation,
/// document key, chunk id), each part length-prefixed.
///
/// The project is part of the name although ADR-0031 lists only
/// (`namespace_id`, `generation`, `document_key`, `chunk_id`): a namespace id is a
/// caller's argument, and without the project a caller in project B that
/// names project A's namespace would compute A's point ids and overwrite A's
/// points. With it, ids of two projects never collide.
///
/// The source is part of the name for the same reason one level down: a
/// token is admitted for its own sources only, and without the source a
/// `deepwiki` writer naming a toolkit index's namespace id would compute that
/// index's point ids and overwrite its points.
#[must_use]
pub fn point_id(
    project_id: i64,
    source: &str,
    namespace: Uuid,
    generation: Option<&str>,
    document_key: &str,
    chunk_id: &str,
) -> Uuid {
    let generation = generation.unwrap_or("");
    let name = format!(
        "{project_id}\u{1f}{source}\u{1f}{}:{generation}{}:{document_key}{}:{chunk_id}",
        generation.len(),
        document_key.len(),
        chunk_id.len(),
    );
    Uuid::new_v5(&namespace, name.as_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn collection_names_round_trip() {
        let space = Space::new("text-embedding-3-small", 1536).expect("space");
        assert_eq!(space.collection(), "emb_text-embedding-3-small_1536");
        assert_eq!(Space::from_collection(&space.collection()), Some(space));
        assert_eq!(Space::from_collection("other_x_3"), None);
        assert_eq!(Space::from_collection("emb_x_03"), None);
        assert_eq!(Space::from_collection("emb_x"), None);
    }

    #[test]
    fn slugs_and_dimensions_are_validated() {
        for slug in ["", "A", "-a", "a_b", "a/b", &"a".repeat(64)] {
            assert!(Space::new(slug, 3).is_err(), "{slug}");
        }
        assert!(Space::new("a", 0).is_err());
        assert!(Space::new("a", MAX_DIMENSION + 1).is_err());
        assert!(Space::new("qwen3-embedding-4b", 2560).is_ok());
    }

    #[test]
    fn namespaces_must_be_canonical() {
        assert!(namespace_uuid("0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b").is_ok());
        assert!(namespace_uuid("0B0F8C1E-6C1A-4D8E-9B1A-2F6D1C3E4A5B").is_err());
        assert!(namespace_uuid("0b0f8c1e6c1a4d8e9b1a2f6d1c3e4a5b").is_err());
        assert!(namespace_uuid("x").is_err());
    }

    #[test]
    fn generations_are_bounded() {
        assert_eq!(generation("").expect("empty"), None);
        assert_eq!(generation("build-1").expect("ok"), Some("build-1"));
        assert!(generation("a b").is_err());
        assert!(generation(&"a".repeat(129)).is_err());
    }

    #[test]
    fn point_ids_are_stable_and_project_bound() {
        let namespace = Uuid::from_u128(7);
        let t = "toolkit_index";
        let a = point_id(1, t, namespace, Some("g"), "doc", "0");
        assert_eq!(a, point_id(1, t, namespace, Some("g"), "doc", "0"));
        assert_ne!(a, point_id(2, t, namespace, Some("g"), "doc", "0"));
        assert_ne!(a, point_id(1, "deepwiki", namespace, Some("g"), "doc", "0"));
        assert_ne!(a, point_id(1, t, namespace, None, "doc", "0"));
        // Length prefixes keep the parts apart.
        assert_ne!(
            point_id(1, t, namespace, None, "ab", "c"),
            point_id(1, t, namespace, None, "a", "bc")
        );
    }

    #[test]
    fn inventory_is_reserved() {
        assert_eq!(
            source_keyword(pb::Source::Inventory as i32)
                .expect_err("reserved")
                .code(),
            tonic::Code::FailedPrecondition
        );
        assert!(source_keyword(0).is_err());
        assert_eq!(
            source_keyword(pb::Source::Deepwiki as i32).expect("ok"),
            "deepwiki"
        );
    }
}
