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

use crate::layout::{
    BM25_VECTOR, DENSE_VECTOR, DenseDatatype, Family, KEYWORD_INDEXES, Space, key,
};

/// Collection settings that come from the deployment.
#[derive(Clone, Copy, Debug)]
pub struct CollectionSettings {
    /// Qdrant `replication_factor` of a new collection.
    pub replication_factor: u32,
    /// Qdrant `shard_number` of a new collection.
    pub shard_number: u32,
    /// A **soft** cap on the collections this service owns (`emb_*` and
    /// `inv_*` together). Creating one beyond it is
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
    /// Collections this process has fully verified (dimension, the family's
    /// dense datatype and every payload index), with their dimension. A
    /// collection enters only after its indexes were confirmed, so a failed
    /// index creation is retried by the next request.
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

    /// The collection of `space` in `family` when it exists, after checking
    /// that its dense dimension is the space's and its dense datatype the
    /// family's.
    ///
    /// # Errors
    /// `FAILED_PRECONDITION` for a collection with another dimension or
    /// datatype; `UNAVAILABLE` when Qdrant fails.
    pub async fn existing(&self, family: Family, space: &Space) -> Result<Option<String>, Status> {
        let collection = space.collection(family);
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
        let (dimension, datatype) = self.dense_params(&collection).await?;
        check_dimension(space, dimension)?;
        check_datatype(&collection, family, datatype)?;
        // A collection made by an older build, or by a run that stopped
        // between creating it and indexing it, gets its indexes here. It is
        // remembered only once they are all there.
        self.ensure_indexes(&collection).await?;
        self.remember(&collection, dimension);
        Ok(Some(collection))
    }

    /// The collection of `space` in `family`, created when missing with the
    /// family's layout, with every payload index in place.
    ///
    /// # Errors
    /// As [`Store::existing`]; `RESOURCE_EXHAUSTED` when creating the
    /// collection would pass [`CollectionSettings::max_collections`].
    pub async fn ensure(&self, family: Family, space: &Space) -> Result<String, Status> {
        if let Some(collection) = self.existing(family, space).await? {
            return Ok(collection);
        }
        let collection = space.collection(family);
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
        if let Err(error) = self
            .client
            .create_collection(self.definition(family, space))
            .await
        {
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
        let ready = self.existing(family, space).await?.ok_or_else(|| {
            tracing::error!(collection, "collection vanished after creation");
            Status::unavailable("the vector store is unavailable")
        })?;
        tracing::info!(collection, "vector collection ready");
        Ok(ready)
    }

    /// The collection of `space` in `family`, made with the family's
    /// [`crate::layout::CollectionLayout`].
    fn definition(&self, family: Family, space: &Space) -> CreateCollectionBuilder {
        let layout = family.layout();
        let mut vectors = VectorsConfigBuilder::default();
        vectors.add_named_vector_params(
            DENSE_VECTOR,
            VectorParamsBuilder::new(u64::from(space.dimension()), Distance::Cosine)
                .datatype(qdrant_datatype(layout.datatype))
                // The originals on disk; the quantized copy (when the
                // family has one) in RAM.
                .on_disk(true),
        );
        let mut definition = CreateCollectionBuilder::new(space.collection(family))
            .vectors_config(vectors)
            // Every search is project-scoped: no global graph, one graph per
            // tenant (`payload_m`).
            .hnsw_config(HnswConfigDiffBuilder::default().m(0).payload_m(16))
            .replication_factor(self.settings.replication_factor)
            .shard_number(self.settings.shard_number);
        if layout.bm25 {
            let mut sparse = SparseVectorsConfigBuilder::default();
            sparse.add_named_vector_params(
                BM25_VECTOR,
                SparseVectorParamsBuilder::default().modifier(Modifier::Idf),
            );
            definition = definition.sparse_vectors_config(sparse);
        }
        if layout.quantized {
            definition = definition.quantization_config(
                ScalarQuantizationBuilder::default()
                    .r#type(QuantizationType::Int8.into())
                    .always_ram(true),
            );
        }
        definition
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

    /// The dimension and the datatype of the collection's dense vector.
    async fn dense_params(&self, collection: &str) -> Result<(u32, Option<Datatype>), Status> {
        let info = self
            .client
            .collection_info(collection)
            .await
            .map_err(|error| unavailable("collection_info", &error))?;
        let params = info
            .result
            .and_then(|info| info.config)
            .and_then(|config| config.params)
            .and_then(|params| params.vectors_config)
            .and_then(|vectors| vectors.config)
            .and_then(|config| match config {
                VectorsConfigKind::ParamsMap(map) => {
                    map.map.get(DENSE_VECTOR).map(|p| (p.size, p.datatype))
                }
                VectorsConfigKind::Params(_) => None,
            });
        params
            .and_then(|(size, datatype)| {
                let size = u32::try_from(size).ok()?;
                Some((size, datatype.and_then(|raw| Datatype::try_from(raw).ok())))
            })
            .ok_or_else(|| {
                tracing::error!(collection, "collection has no dense vector of this layout");
                Status::failed_precondition("the collection does not have this service's layout")
            })
    }

    /// Every collection this service owns, of every family, with its family
    /// and space, sorted by name.
    ///
    /// # Errors
    /// `UNAVAILABLE` when Qdrant fails.
    pub async fn collections(&self) -> Result<Vec<(String, Family, Space)>, Status> {
        let listed = self
            .client
            .list_collections()
            .await
            .map_err(|error| unavailable("list_collections", &error))?;
        let mut collections: Vec<(String, Family, Space)> = listed
            .collections
            .into_iter()
            .filter_map(|description| {
                Space::from_collection(&description.name)
                    .map(|(family, space)| (description.name, family, space))
            })
            .collect();
        collections.sort_by(|left, right| left.0.cmp(&right.0));
        Ok(collections)
    }

    /// The collections of `family` an operation applies to: the space's when
    /// given (and existing), else every collection of the family. A
    /// collection of another family is never among them.
    ///
    /// # Errors
    /// As [`Store::existing`] and [`Store::collections`].
    pub async fn targets(
        &self,
        family: Family,
        space: Option<&Space>,
    ) -> Result<Vec<String>, Status> {
        match space {
            Some(space) => Ok(self.existing(family, space).await?.into_iter().collect()),
            None => Ok(self
                .collections()
                .await?
                .into_iter()
                .filter(|(_, owner, _)| *owner == family)
                .map(|(name, _, _)| name)
                .collect()),
        }
    }

    /// Every collection of every family: what a whole-project drop spans.
    ///
    /// # Errors
    /// As [`Store::collections`].
    pub async fn every_target(&self) -> Result<Vec<String>, Status> {
        Ok(self
            .collections()
            .await?
            .into_iter()
            .map(|(name, _, _)| name)
            .collect())
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

/// The Qdrant datatype of a layout's dense vectors.
fn qdrant_datatype(datatype: DenseDatatype) -> Datatype {
    match datatype {
        DenseDatatype::Float16 => Datatype::Float16,
        DenseDatatype::Float32 => Datatype::Float32,
    }
}

/// Refuses a collection whose dense datatype is not its family's: an
/// Inventory point must never land in a float16 collection, nor a shared
/// point in a float32 one. Qdrant reports an unset (`Default`) datatype for
/// float32.
fn check_datatype(
    collection: &str,
    family: Family,
    datatype: Option<Datatype>,
) -> Result<(), Status> {
    let actual = match datatype {
        None | Some(Datatype::Default | Datatype::Float32) => Datatype::Float32,
        Some(other) => other,
    };
    if actual == qdrant_datatype(family.layout().datatype) {
        Ok(())
    } else {
        tracing::error!(
            collection,
            ?family,
            ?actual,
            "collection's dense datatype is not its family's"
        );
        Err(Status::failed_precondition(
            "the collection does not have this service's layout",
        ))
    }
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
    fn a_collection_must_have_its_familys_datatype() {
        for (family, datatype, ok) in [
            (Family::Shared, Some(Datatype::Float16), true),
            (Family::Shared, Some(Datatype::Float32), false),
            (Family::Shared, None, false),
            (Family::Inventory, Some(Datatype::Float32), true),
            (Family::Inventory, Some(Datatype::Default), true),
            (Family::Inventory, None, true),
            (Family::Inventory, Some(Datatype::Float16), false),
            (Family::Inventory, Some(Datatype::Uint8), false),
        ] {
            assert_eq!(
                check_datatype("c", family, datatype).is_ok(),
                ok,
                "{family:?} {datatype:?}"
            );
        }
    }

    #[test]
    fn each_family_is_defined_with_its_layout() {
        let store = Store::new(
            Qdrant::from_url("http://127.0.0.1:1")
                .skip_compatibility_check()
                .build()
                .expect("client"),
            CollectionSettings::default(),
        );
        let space = Space::new("m", 4).expect("space");
        for family in Family::ALL {
            let layout = family.layout();
            let definition: qdrant_client::qdrant::CreateCollection =
                store.definition(family, &space).into();
            assert_eq!(definition.collection_name, space.collection(family));
            let dense = match definition
                .vectors_config
                .and_then(|config| config.config)
                .expect("vectors")
            {
                VectorsConfigKind::ParamsMap(map) => map.map[DENSE_VECTOR],
                VectorsConfigKind::Params(_) => panic!("named vectors expected"),
            };
            assert_eq!(dense.size, 4);
            assert_eq!(
                dense.datatype,
                Some(qdrant_datatype(layout.datatype) as i32),
                "{family:?}"
            );
            assert_eq!(
                definition.quantization_config.is_some(),
                layout.quantized,
                "{family:?}"
            );
            assert_eq!(
                definition.sparse_vectors_config.is_some(),
                layout.bm25,
                "{family:?}"
            );
        }
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
