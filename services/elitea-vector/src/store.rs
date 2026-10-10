//! Qdrant collections: created on demand with the ADR-0031 decision 2
//! settings, and checked for their dimension before use.

use std::collections::{HashMap, HashSet};
use std::sync::Mutex;

use qdrant_client::Qdrant;
use qdrant_client::qdrant::vectors_config::Config as VectorsConfigKind;
use qdrant_client::qdrant::{
    CreateCollectionBuilder, CreateFieldIndexCollectionBuilder, Datatype, Distance, FieldType,
    HnswConfigDiffBuilder, KeywordIndexParamsBuilder, Modifier, QuantizationType,
    ScalarQuantizationBuilder, SparseVectorParamsBuilder, SparseVectorsConfigBuilder,
    TextIndexParamsBuilder, TokenizerType, VectorParamsBuilder, VectorsConfigBuilder,
};
use tonic::{Code, Status};

use crate::layout::{BM25_VECTOR, COLLECTION_PREFIX, DENSE_VECTOR, KEYWORD_INDEXES, Space, key};

/// Collection settings that come from the deployment.
#[derive(Clone, Copy, Debug)]
pub struct CollectionSettings {
    /// Qdrant `replication_factor` of a new collection.
    pub replication_factor: u32,
    /// Qdrant `shard_number` of a new collection.
    pub shard_number: u32,
    /// A **soft** cap on `emb_*` collections. Creating one beyond it is
    /// `RESOURCE_EXHAUSTED`, but the count is read from Qdrant just before a
    /// create and nothing serializes creators, so concurrent creators (in one
    /// process or across replicas) can overshoot it by their number. It
    /// bounds ordinary growth; it is not a quota. The hard bound is
    /// `ELITEA_VECTOR_ALLOWED_SPACES` ([`crate::service::SpaceLimits::allowed`]),
    /// which production deployments should set.
    pub max_collections: usize,
}

/// The default of [`CollectionSettings::max_collections`].
pub const DEFAULT_MAX_COLLECTIONS: usize = 64;

impl Default for CollectionSettings {
    fn default() -> Self {
        Self {
            replication_factor: 1,
            shard_number: 1,
            max_collections: DEFAULT_MAX_COLLECTIONS,
        }
    }
}

/// The Qdrant client and what this process knows about collections.
pub struct Store {
    client: Qdrant,
    settings: CollectionSettings,
    /// Collections this process has fully verified (dimension and every
    /// payload index), with their dimension. A collection enters only after
    /// its indexes were confirmed, so a failed index creation is retried by
    /// the next request.
    known: Mutex<HashMap<String, u32>>,
}

/// A Qdrant failure, logged with its cause and returned without it.
///
/// * Unreachable, timed out, overloaded or misconfigured (credentials) is
///   `UNAVAILABLE`: the caller may retry.
/// * Qdrant refusing the request's content (a bad filter, a payload that is
///   too large, a vector of the wrong size) is `INVALID_ARGUMENT`.
/// * A missing or conflicting collection is `FAILED_PRECONDITION`.
///
/// The message is fixed text; Qdrant's own never reaches the caller.
pub fn unavailable(operation: &str, error: &qdrant_client::QdrantError) -> Status {
    use qdrant_client::QdrantError;
    let code = match error {
        QdrantError::ResponseError { status } => status.code(),
        QdrantError::ResourceExhaustedError { .. } => Code::ResourceExhausted,
        // The request could not even be built from what the caller sent.
        QdrantError::ConversionError(_) | QdrantError::JsonToPayload(_) => Code::InvalidArgument,
        _ => Code::Unavailable,
    };
    match code {
        Code::InvalidArgument | Code::OutOfRange => {
            tracing::warn!(operation, %error, "qdrant refused the request");
            Status::invalid_argument("the vector store refused the request as invalid")
        }
        Code::FailedPrecondition | Code::NotFound | Code::AlreadyExists | Code::Aborted => {
            tracing::warn!(operation, %error, "qdrant precondition failed");
            Status::failed_precondition("the vector store state does not allow this request")
        }
        _ => {
            tracing::error!(operation, %error, "qdrant request failed");
            Status::unavailable("the vector store is unavailable")
        }
    }
}

impl Store {
    /// A store over `client`.
    #[must_use]
    pub fn new(client: Qdrant, settings: CollectionSettings) -> Self {
        Self {
            client,
            settings,
            known: Mutex::new(HashMap::new()),
        }
    }

    /// The Qdrant client.
    #[must_use]
    pub fn client(&self) -> &Qdrant {
        &self.client
    }

    fn remembered(&self, collection: &str) -> Option<u32> {
        self.known.lock().ok()?.get(collection).copied()
    }

    fn remember(&self, collection: &str, dimension: u32) {
        if let Ok(mut known) = self.known.lock() {
            known.insert(collection.to_owned(), dimension);
        }
    }

    /// The collection of `space` when it exists, after checking that its
    /// dense dimension is the space's.
    ///
    /// # Errors
    /// `FAILED_PRECONDITION` for a collection with another dimension;
    /// `UNAVAILABLE` when Qdrant fails.
    pub async fn existing(&self, space: &Space) -> Result<Option<String>, Status> {
        let collection = space.collection();
        if let Some(dimension) = self.remembered(&collection) {
            return check_dimension(space, dimension).map(|()| Some(collection));
        }
        let exists = self
            .client
            .collection_exists(&collection)
            .await
            .map_err(|error| unavailable("collection_exists", &error))?;
        if !exists {
            return Ok(None);
        }
        let dimension = self.dense_dimension(&collection).await?;
        check_dimension(space, dimension)?;
        // A collection made by an older build, or by a run that stopped
        // between creating it and indexing it, gets its indexes here. It is
        // remembered only once they are all there.
        self.ensure_indexes(&collection).await?;
        self.remember(&collection, dimension);
        Ok(Some(collection))
    }

    /// The collection of `space`, created when missing, with every payload
    /// index in place.
    ///
    /// # Errors
    /// As [`Store::existing`]; `RESOURCE_EXHAUSTED` when creating the
    /// collection would pass [`CollectionSettings::max_collections`].
    pub async fn ensure(&self, space: &Space) -> Result<String, Status> {
        if let Some(collection) = self.existing(space).await? {
            return Ok(collection);
        }
        let collection = space.collection();
        let present = self.collections().await?.len();
        if present >= self.settings.max_collections {
            tracing::warn!(
                collection,
                present,
                max = self.settings.max_collections,
                "collection cap reached; not creating"
            );
            return Err(Status::resource_exhausted(format!(
                "the vector store holds its limit of {} embedding spaces; \
                 ask an administrator to raise ELITEA_VECTOR_MAX_COLLECTIONS or use an existing space",
                self.settings.max_collections
            )));
        }
        if let Err(error) = self.client.create_collection(self.definition(space)).await {
            // Another replica may have made it first.
            let exists = self
                .client
                .collection_exists(&collection)
                .await
                .map_err(|error| unavailable("collection_exists", &error))?;
            if !exists {
                return Err(unavailable("create_collection", &error));
            }
        }
        // Verifies the dimension and creates the indexes, whoever made it.
        let ready = self.existing(space).await?.ok_or_else(|| {
            tracing::error!(collection, "collection vanished after creation");
            Status::unavailable("the vector store is unavailable")
        })?;
        tracing::info!(collection, "vector collection ready");
        Ok(ready)
    }

    fn definition(&self, space: &Space) -> CreateCollectionBuilder {
        let mut vectors = VectorsConfigBuilder::default();
        vectors.add_named_vector_params(
            DENSE_VECTOR,
            VectorParamsBuilder::new(u64::from(space.dimension()), Distance::Cosine)
                .datatype(Datatype::Float16)
                // The originals on disk; the quantized copy in RAM.
                .on_disk(true),
        );
        let mut sparse = SparseVectorsConfigBuilder::default();
        sparse.add_named_vector_params(
            BM25_VECTOR,
            SparseVectorParamsBuilder::default().modifier(Modifier::Idf),
        );
        CreateCollectionBuilder::new(space.collection())
            .vectors_config(vectors)
            .sparse_vectors_config(sparse)
            // Every search is project-scoped: no global graph, one graph per
            // tenant (`payload_m`).
            .hnsw_config(HnswConfigDiffBuilder::default().m(0).payload_m(16))
            .quantization_config(
                ScalarQuantizationBuilder::default()
                    .r#type(QuantizationType::Int8.into())
                    .always_ram(true),
            )
            .replication_factor(self.settings.replication_factor)
            .shard_number(self.settings.shard_number)
    }

    /// Creates the payload indexes the collection lacks. Creating one is
    /// idempotent here: the collection's payload schema is read first, and a
    /// concurrent creator's "already exists" is not a failure.
    async fn ensure_indexes(&self, collection: &str) -> Result<(), Status> {
        let info = self
            .client
            .collection_info(collection)
            .await
            .map_err(|error| unavailable("collection_info", &error))?;
        let present: HashSet<String> = info
            .result
            .map(|info| info.payload_schema.into_keys().collect())
            .unwrap_or_default();
        for field in required_indexes() {
            if present.contains(field.name) {
                continue;
            }
            let builder = match field.kind {
                FieldType::Keyword if field.name == key::PROJECT_ID => {
                    CreateFieldIndexCollectionBuilder::new(
                        collection,
                        field.name,
                        FieldType::Keyword,
                    )
                    .field_index_params(KeywordIndexParamsBuilder::default().is_tenant(true))
                }
                FieldType::Keyword => CreateFieldIndexCollectionBuilder::new(
                    collection,
                    field.name,
                    FieldType::Keyword,
                ),
                kind => CreateFieldIndexCollectionBuilder::new(collection, field.name, kind)
                    .field_index_params(
                        TextIndexParamsBuilder::new(TokenizerType::Word)
                            .lowercase(true)
                            .ascii_folding(true),
                    ),
            };
            match self.client.create_field_index(builder.wait(true)).await {
                Ok(_) => {}
                Err(error) if is_already_exists(&error) => {}
                Err(error) => return Err(unavailable("create_field_index", &error)),
            }
        }
        Ok(())
    }

    async fn dense_dimension(&self, collection: &str) -> Result<u32, Status> {
        let info = self
            .client
            .collection_info(collection)
            .await
            .map_err(|error| unavailable("collection_info", &error))?;
        let size = info
            .result
            .and_then(|info| info.config)
            .and_then(|config| config.params)
            .and_then(|params| params.vectors_config)
            .and_then(|vectors| vectors.config)
            .and_then(|config| match config {
                VectorsConfigKind::ParamsMap(map) => map.map.get(DENSE_VECTOR).map(|p| p.size),
                VectorsConfigKind::Params(_) => None,
            });
        size.and_then(|size| u32::try_from(size).ok())
            .ok_or_else(|| {
                tracing::error!(collection, "collection has no dense vector of this layout");
                Status::failed_precondition("the collection does not have this service's layout")
            })
    }

    /// Every collection this service owns, with its space.
    ///
    /// # Errors
    /// `UNAVAILABLE` when Qdrant fails.
    pub async fn collections(&self) -> Result<Vec<(String, Space)>, Status> {
        let listed = self
            .client
            .list_collections()
            .await
            .map_err(|error| unavailable("list_collections", &error))?;
        let mut collections: Vec<(String, Space)> = listed
            .collections
            .into_iter()
            .filter(|description| description.name.starts_with(COLLECTION_PREFIX))
            .filter_map(|description| {
                Space::from_collection(&description.name).map(|space| (description.name, space))
            })
            .collect();
        collections.sort_by(|left, right| left.0.cmp(&right.0));
        Ok(collections)
    }

    /// The collections an operation applies to: the space's when given (and
    /// existing), else every collection.
    ///
    /// # Errors
    /// As [`Store::existing`] and [`Store::collections`].
    pub async fn targets(&self, space: Option<&Space>) -> Result<Vec<String>, Status> {
        match space {
            Some(space) => Ok(self.existing(space).await?.into_iter().collect()),
            None => Ok(self
                .collections()
                .await?
                .into_iter()
                .map(|(name, _)| name)
                .collect()),
        }
    }

    /// Whether Qdrant answers.
    pub async fn ready(&self) -> bool {
        self.client.health_check().await.is_ok()
    }
}

/// One payload index the collections need.
struct RequiredIndex {
    name: &'static str,
    kind: FieldType,
}

/// Every payload index of ADR-0031 decision 2: the tenant key first.
fn required_indexes() -> Vec<RequiredIndex> {
    let mut indexes = vec![RequiredIndex {
        name: key::PROJECT_ID,
        kind: FieldType::Keyword,
    }];
    indexes.extend(KEYWORD_INDEXES.iter().map(|name| RequiredIndex {
        name,
        kind: FieldType::Keyword,
    }));
    indexes.push(RequiredIndex {
        name: key::TEXT,
        kind: FieldType::Text,
    });
    indexes
}

fn is_already_exists(error: &qdrant_client::QdrantError) -> bool {
    matches!(
        error,
        qdrant_client::QdrantError::ResponseError { status }
            if status.code() == Code::AlreadyExists
                || status.message().to_ascii_lowercase().contains("already exist")
    )
}

fn check_dimension(space: &Space, dimension: u32) -> Result<(), Status> {
    if dimension == space.dimension() {
        Ok(())
    } else {
        Err(Status::failed_precondition(
            "the collection's dimension differs from the requested space",
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use qdrant_client::QdrantError;

    fn response(code: Code) -> QdrantError {
        QdrantError::ResponseError {
            status: Status::new(code, "secret detail from qdrant"),
        }
    }

    #[test]
    fn qdrant_errors_map_to_the_right_codes_without_their_detail() {
        for (error, expected) in [
            // Unreachable, timed out, overloaded or refusing our credentials.
            (response(Code::Unavailable), Code::Unavailable),
            (response(Code::DeadlineExceeded), Code::Unavailable),
            (response(Code::Unknown), Code::Unavailable),
            (response(Code::Internal), Code::Unavailable),
            (response(Code::Unauthenticated), Code::Unavailable),
            (response(Code::PermissionDenied), Code::Unavailable),
            (
                QdrantError::ResourceExhaustedError {
                    status: Status::new(Code::ResourceExhausted, "slow down"),
                    retry_after_seconds: 3,
                },
                Code::Unavailable,
            ),
            (
                QdrantError::Io(std::io::Error::from(std::io::ErrorKind::ConnectionRefused)),
                Code::Unavailable,
            ),
            // Qdrant refusing the content: a bad filter, a payload too
            // large, a vector of the wrong size.
            (response(Code::InvalidArgument), Code::InvalidArgument),
            (response(Code::OutOfRange), Code::InvalidArgument),
            (
                QdrantError::ConversionError("bad".to_owned()),
                Code::InvalidArgument,
            ),
            // Collection state.
            (response(Code::FailedPrecondition), Code::FailedPrecondition),
            (response(Code::NotFound), Code::FailedPrecondition),
            (response(Code::AlreadyExists), Code::FailedPrecondition),
        ] {
            let status = unavailable("test", &error);
            assert_eq!(status.code(), expected, "{error}");
            assert!(
                !status.message().contains("secret detail"),
                "qdrant's own message must not reach the caller"
            );
        }
    }

    #[test]
    fn an_already_exists_answer_is_recognised() {
        assert!(is_already_exists(&response(Code::AlreadyExists)));
        assert!(is_already_exists(&QdrantError::ResponseError {
            status: Status::new(Code::Unknown, "Index already exists"),
        }));
        assert!(!is_already_exists(&response(Code::Unavailable)));
    }

    #[test]
    fn every_payload_index_is_required_once() {
        let indexes = required_indexes();
        let names: HashSet<_> = indexes.iter().map(|index| index.name).collect();
        assert_eq!(names.len(), indexes.len());
        assert_eq!(indexes[0].name, key::PROJECT_ID);
        assert!(names.contains(key::TEXT));
        assert!(KEYWORD_INDEXES.iter().all(|name| names.contains(name)));
    }
}
