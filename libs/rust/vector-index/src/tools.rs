//! The index tools the SDK exposes (`list_indexes`, `search_index`,
//! `stepback_search_index`, `stepback_summary_index`, `remove_index`),
//! implemented on `elitea-vector`. `index_data` is the indexer's, not this
//! crate's.
//!
//! An SDK index *name* is an `index_registry` row in this platform
//! (ADR-0031): the worker resolves the project's rows into an
//! [`IndexCatalog`] (name, namespace UUID, embedding space) and hands it
//! over; nothing here reads the registry.

use serde::Deserialize;
use serde_json::Value;

use crate::client::toolkit_scope;
use crate::error::{Error, Result};
use crate::filter;
use crate::format::{
    Doctype, NO_INDEXES, format_hits, index_not_found_message, no_documents_message,
    project_fields, sdk_collection_filter, stepback_result_text,
};
use crate::hit::Hit;
use crate::pb;
use crate::rerank;
use crate::search::{FullText, QueryEmbedder, SearchPlan, VectorBackend, search_documents};
use crate::stepback::{StepbackModel, answer_prompt, extract_completion_text, stepback_prompt};

/// The most namespaces one search may span (the facade's limit).
const MAX_SEARCHED_INDEXES: usize = 64;
/// The index tools' default `cut_off` (the SDK's `DEFAULT_CUT_OFF`).
pub const DEFAULT_CUT_OFF: f64 = 0.1;
/// The default `search_top`.
pub const DEFAULT_SEARCH_TOP: usize = 10;

/// One index of the project: what the registry knows of it.
#[derive(Clone, Debug, PartialEq)]
pub struct IndexEntry {
    /// The name the user gave the index.
    pub name: String,
    /// The `index_registry` UUID, which is the point namespace.
    pub namespace_id: String,
    /// The embedding space the index was built in.
    pub space: pb::EmbeddingSpace,
}

/// The project's indexes of one toolkit, in listing order.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct IndexCatalog {
    entries: Vec<IndexEntry>,
}

impl IndexCatalog {
    /// A catalog of `entries`.
    #[must_use]
    pub fn new(entries: Vec<IndexEntry>) -> Self {
        Self { entries }
    }

    /// `list_indexes`: the names joined by commas, or `No indexed
    /// collections`.
    #[must_use]
    pub fn list_indexes(&self) -> String {
        if self.entries.is_empty() {
            NO_INDEXES.to_owned()
        } else {
            self.entries
                .iter()
                .map(|entry| entry.name.as_str())
                .collect::<Vec<_>>()
                .join(",")
        }
    }

    /// The catalog without the index `name`.
    #[must_use]
    pub fn without(&self, name: &str) -> Self {
        Self {
            entries: self
                .entries
                .iter()
                .filter(|entry| entry.name != name)
                .cloned()
                .collect(),
        }
    }

    fn entry(&self, name: &str) -> Option<&IndexEntry> {
        self.entries.iter().find(|entry| entry.name == name)
    }
}

/// Arguments of `search_index`, `stepback_search_index` and
/// `stepback_summary_index`, as the tool schema names them. Deserialize it
/// from the tool call's JSON.
#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default)]
pub struct SearchArgs {
    /// The query text.
    pub query: String,
    /// One index, or empty for all of the toolkit's.
    pub index_name: Option<String>,
    /// An SDK filter: an object, or a JSON string holding one.
    pub filter: Value,
    /// Minimum `|score|`, 0 to 1. Default 0.1; 0 turns it off.
    pub cut_off: Option<f64>,
    /// How many results to return. Default 10.
    pub search_top: Option<usize>,
    /// Legacy rerank rules (`search_index` only).
    pub reranker: Value,
    /// `{enabled, fields, language, weight}`.
    pub full_text_search: Value,
    /// Rerank rules.
    pub reranking_config: Value,
    /// Extra chunk types to search.
    pub extended_search: Option<Vec<String>>,
    /// Fields to return (`search_index` only).
    pub output_fields: Option<Vec<String>>,
    /// Chat history of the step-back tools. Printed into the answer prompt
    /// of `stepback_summary_index`; the step-back prompt ignores it.
    pub messages: Option<Vec<Value>>,
}

impl SearchArgs {
    /// Arguments for `query` with every default.
    #[must_use]
    pub fn new(query: impl Into<String>) -> Self {
        Self {
            query: query.into(),
            ..Self::default()
        }
    }

    fn index_name(&self) -> &str {
        self.index_name.as_deref().unwrap_or("")
    }

    fn cut_off(&self) -> Result<f64> {
        let cut_off = self.cut_off.unwrap_or(DEFAULT_CUT_OFF);
        if (0.0..=1.0).contains(&cut_off) {
            Ok(cut_off)
        } else {
            Err(Error::InvalidArgument(
                "cut_off must be between 0 and 1".to_owned(),
            ))
        }
    }

    fn top(&self) -> Result<usize> {
        match self.search_top.unwrap_or(DEFAULT_SEARCH_TOP) {
            0 => Err(Error::InvalidArgument(
                "search_top must be greater than 0".to_owned(),
            )),
            top => Ok(top),
        }
    }
}

/// What `search_index` returns: the result list, or a message.
#[derive(Clone, Debug, PartialEq)]
pub enum SearchOutput {
    /// `{page_content, metadata, score}` dicts (or their projection).
    Documents(Vec<Value>),
    /// `No documents found by query ...` or `Index '...' not found ...`.
    Message(String),
}

impl SearchOutput {
    /// The tool's JSON result: an array, or a string.
    #[must_use]
    pub fn into_value(self) -> Value {
        match self {
            Self::Documents(docs) => Value::Array(docs),
            Self::Message(message) => Value::String(message),
        }
    }
}

/// What the searched indexes resolve to.
enum Target {
    /// Nothing to search: the toolkit has no indexes.
    Nothing,
    /// The namespaces and their one embedding space.
    Found(Vec<String>, pb::EmbeddingSpace),
}

/// The index tools of one toolkit, for the claim's project.
pub struct IndexTools<'a, B, E> {
    backend: &'a B,
    embedder: &'a E,
    catalog: &'a IndexCatalog,
    doctype: Doctype,
}

impl<'a, B: VectorBackend, E: QueryEmbedder> IndexTools<'a, B, E> {
    /// Tools over `backend`, embedding queries with `embedder`, for the
    /// indexes in `catalog`.
    #[must_use]
    pub fn new(
        backend: &'a B,
        embedder: &'a E,
        catalog: &'a IndexCatalog,
        doctype: Doctype,
    ) -> Self {
        Self {
            backend,
            embedder,
            catalog,
            doctype,
        }
    }

    /// `list_indexes`.
    #[must_use]
    pub fn list_indexes(&self) -> String {
        self.catalog.list_indexes()
    }

    /// `search_index`.
    ///
    /// # Errors
    ///
    /// A malformed argument or filter, a query embedding of the wrong
    /// dimension, indexes in different embedding spaces, or the facade's
    /// refusal.
    pub async fn search_index(&self, args: &SearchArgs) -> Result<SearchOutput> {
        let index_name = args.index_name();
        let target = match self.resolve(index_name) {
            Ok(target) => target,
            Err(Error::NotFound(message)) => return Ok(SearchOutput::Message(message)),
            Err(error) => return Err(error),
        };
        let hits = self.run(args, &args.query, &target, true).await?;
        let mut docs = format_hits(&hits, self.doctype);
        if let Some(fields) = args
            .output_fields
            .as_deref()
            .filter(|fields| !fields.is_empty())
        {
            docs = project_fields(docs, fields);
        }
        if docs.is_empty() {
            let user_filter = filter::parse_input(&args.filter)?;
            return Ok(SearchOutput::Message(no_documents_message(
                &args.query,
                &sdk_collection_filter(user_filter.as_ref(), index_name),
            )));
        }
        Ok(SearchOutput::Documents(docs))
    }

    /// `stepback_search_index`: the model turns the question into a generic
    /// query, then the index is searched with it.
    ///
    /// # Errors
    ///
    /// As [`Self::search_index`], and the model's failure.
    pub async fn stepback_search_index<M: StepbackModel>(
        &self,
        args: &SearchArgs,
        model: &M,
    ) -> Result<String> {
        let docs = self.stepback_documents(args, model).await?;
        Ok(stepback_result_text(&docs))
    }

    /// `stepback_summary_index`: the step-back search, then a second model
    /// call that answers from its results. Returns the model's reply as it
    /// is (see [`crate::stepback::answer_section`] for the answer alone).
    ///
    /// # Errors
    ///
    /// As [`Self::search_index`], and the model's failure.
    pub async fn stepback_summary_index<M: StepbackModel>(
        &self,
        args: &SearchArgs,
        model: &M,
    ) -> Result<String> {
        let docs = self.stepback_documents(args, model).await?;
        let messages = args.messages.clone().unwrap_or_default();
        let prompt = answer_prompt(&args.query, &docs, &messages);
        Ok(extract_completion_text(&model.invoke(&prompt).await?))
    }

    /// `remove_index`: deletes every point of the named index.
    ///
    /// An empty name is refused. The SDK deleted every row without an
    /// index, reported "All indexes have been removed" and left the named
    /// ones, so no caller can have relied on it.
    ///
    /// # Errors
    ///
    /// [`Error::Refused`] for an empty name, [`Error::NotFound`] for an
    /// unknown index, or the facade's refusal.
    pub async fn remove_index(&self, index_name: &str) -> Result<String> {
        if index_name.trim().is_empty() {
            return Err(Error::Refused(
                "remove_index needs an index name: removing every index at once is not supported"
                    .to_owned(),
            ));
        }
        let Some(entry) = self.catalog.entry(index_name) else {
            return Err(Error::NotFound(index_not_found_message(
                index_name,
                &self.catalog.list_indexes(),
            )));
        };
        self.backend
            .delete_namespace(entry.space.clone(), &entry.namespace_id)
            .await?;
        Ok(format!(
            "Index '{index_name}' has been removed from the vector store.\nAvailable indexes: {}",
            self.catalog.without(index_name).list_indexes()
        ))
    }

    async fn stepback_documents<M: StepbackModel>(
        &self,
        args: &SearchArgs,
        model: &M,
    ) -> Result<Vec<Value>> {
        let reply = model.invoke(&stepback_prompt(&args.query)).await?;
        let query = extract_completion_text(&reply);
        // The SDK does not check the index exists here: an unknown name
        // simply finds nothing.
        let target = match self.resolve(args.index_name()) {
            Ok(target) => target,
            Err(Error::NotFound(_)) => Target::Nothing,
            Err(error) => return Err(error),
        };
        let hits = self.run(args, &query, &target, false).await?;
        Ok(format_hits(&hits, self.doctype))
    }

    fn resolve(&self, index_name: &str) -> Result<Target> {
        if index_name.is_empty() {
            let Some(first) = self.catalog.entries.first() else {
                return Ok(Target::Nothing);
            };
            if self
                .catalog
                .entries
                .iter()
                .any(|entry| entry.space != first.space)
            {
                return Err(Error::InvalidArgument(
                    "Global search cannot be completed since collections were indexed using different embedding models. Use search within a single collection.".to_owned(),
                ));
            }
            if self.catalog.entries.len() > MAX_SEARCHED_INDEXES {
                return Err(Error::InvalidArgument(format!(
                    "Global search covers at most {MAX_SEARCHED_INDEXES} indexes. Use search within a single collection."
                )));
            }
            return Ok(Target::Found(
                self.catalog
                    .entries
                    .iter()
                    .map(|entry| entry.namespace_id.clone())
                    .collect(),
                first.space.clone(),
            ));
        }
        match self.catalog.entry(index_name) {
            Some(entry) => Ok(Target::Found(
                vec![entry.namespace_id.clone()],
                entry.space.clone(),
            )),
            None => Err(Error::NotFound(index_not_found_message(
                index_name,
                &self.catalog.list_indexes(),
            ))),
        }
    }

    /// Validates the arguments, embeds the query and runs the search.
    async fn run(
        &self,
        args: &SearchArgs,
        query: &str,
        target: &Target,
        legacy_reranker: bool,
    ) -> Result<Vec<Hit>> {
        let cut_off = args.cut_off()?;
        let top = args.top()?;
        let translated = filter::translate(&args.filter)?;
        let rules = rerank::choose(
            Some(&args.reranking_config),
            legacy_reranker.then_some(&args.reranker),
        )?;
        let full_text = FullText::from_value(Some(&args.full_text_search))?;
        if query.trim().is_empty() {
            return Err(Error::InvalidArgument(
                "the query must not be empty".to_owned(),
            ));
        }
        let Target::Found(namespace_ids, space) = target else {
            return Ok(Vec::new());
        };
        let vector = self.embedder.embed_query(query, space).await?;
        if vector.len() != space.dimension as usize {
            return Err(Error::InvalidArgument(format!(
                "the query embedding has {} dimensions, but this index uses {} ({} dimensions). Re-index to change the embedding model.",
                vector.len(),
                space.model_slug,
                space.dimension
            )));
        }
        let plan = SearchPlan {
            scope: toolkit_scope(namespace_ids.clone()),
            space: space.clone(),
            query: query.to_owned(),
            vector,
            filter: translated,
            top,
            cut_off,
            extended: args.extended_search.clone().unwrap_or_default(),
            full_text,
            rerank: rules,
        };
        search_documents(self.backend, &plan).await
    }
}
