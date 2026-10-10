//! Qdrant collections: created on demand with the ADR-0031 decision 2
//! settings, and checked for their dimension before use.

use std::collections::HashMap;
use std::sync::Mutex;

use qdrant_client::Qdrant;
use qdrant_client::qdrant::vectors_config::Config as VectorsConfigKind;
use qdrant_client::qdrant::{
    CreateCollectionBuilder, CreateFieldIndexCollectionBuilder, Datatype, Distance, FieldType,
    HnswConfigDiffBuilder, KeywordIndexParamsBuilder, Modifier, QuantizationType,
    ScalarQuantizationBuilder, SparseVectorParamsBuilder, SparseVectorsConfigBuilder,
    TextIndexParamsBuilder, TokenizerType, VectorParamsBuilder, VectorsConfigBuilder,
};
use tonic::Status;

use crate::layout::{BM25_VECTOR, COLLECTION_PREFIX, DENSE_VECTOR, KEYWORD_INDEXES, Space, key};

/// Collection settings that come from the deployment.
#[derive(Clone, Copy, Debug)]
pub struct CollectionSettings {
    /// Qdrant `replication_factor` of a new collection.
    pub replication_factor: u32,
    /// Qdrant `shard_number` of a new collection.
    pub shard_number: u32,
}

impl Default for CollectionSettings {
    fn default() -> Self {
        Self {
            replication_factor: 1,
            shard_number: 1,
        }
    }
}

/// The Qdrant client and what this process knows about collections.
pub struct Store {
    client: Qdrant,
    settings: CollectionSettings,
    /// Collections this process has checked (or made), with their dimension.
    known: Mutex<HashMap<String, u32>>,
}

/// A Qdrant failure, logged with its cause and returned without it.
pub fn unavailable(operation: &str, error: &qdrant_client::QdrantError) -> Status {
    tracing::error!(operation, %error, "qdrant request failed");
    Status::unavailable("the vector store is unavailable")
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
        self.remember(&collection, dimension);
        Ok(Some(collection))
    }

    /// The collection of `space`, created when missing.
    ///
    /// # Errors
    /// As [`Store::existing`].
    pub async fn ensure(&self, space: &Space) -> Result<String, Status> {
        if let Some(collection) = self.existing(space).await? {
            return Ok(collection);
        }
        let collection = space.collection();
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
        self.create_indexes(&collection).await?;
        let dimension = self.dense_dimension(&collection).await?;
        check_dimension(space, dimension)?;
        self.remember(&collection, dimension);
        tracing::info!(collection, "vector collection ready");
        Ok(collection)
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

    async fn create_indexes(&self, collection: &str) -> Result<(), Status> {
        let tenant =
            CreateFieldIndexCollectionBuilder::new(collection, key::PROJECT_ID, FieldType::Keyword)
                .field_index_params(KeywordIndexParamsBuilder::default().is_tenant(true))
                .wait(true);
        self.client
            .create_field_index(tenant)
            .await
            .map_err(|error| unavailable("create_field_index", &error))?;
        for field in KEYWORD_INDEXES {
            self.client
                .create_field_index(
                    CreateFieldIndexCollectionBuilder::new(collection, field, FieldType::Keyword)
                        .wait(true),
                )
                .await
                .map_err(|error| unavailable("create_field_index", &error))?;
        }
        let text = CreateFieldIndexCollectionBuilder::new(collection, key::TEXT, FieldType::Text)
            .field_index_params(
                TextIndexParamsBuilder::new(TokenizerType::Word)
                    .lowercase(true)
                    .ascii_folding(true),
            )
            .wait(true);
        self.client
            .create_field_index(text)
            .await
            .map_err(|error| unavailable("create_field_index", &error))?;
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

fn check_dimension(space: &Space, dimension: u32) -> Result<(), Status> {
    if dimension == space.dimension() {
        Ok(())
    } else {
        Err(Status::failed_precondition(
            "the collection's dimension differs from the requested space",
        ))
    }
}
