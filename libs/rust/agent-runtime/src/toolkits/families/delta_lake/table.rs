//! One Delta table, read through delta-kernel-rs and its default Arrow
//! engine: the three operations the tools serve, each bounded.
//!
//! Every function here is synchronous and blocking (the kernel's engine runs
//! its I/O on its own runtime and waits); the tools call them from
//! `spawn_blocking`.

use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

use delta_kernel::arrow::array::{Array, ArrayRef, AsArray, Float64Array};
use delta_kernel::arrow::compute::cast;
use delta_kernel::arrow::datatypes::{DataType, Float64Type, Schema as ArrowSchema};
use delta_kernel::arrow::record_batch::RecordBatch;
use delta_kernel::engine::arrow_conversion::TryFromKernel as _;
use delta_kernel::engine::arrow_data::ArrowEngineData;
use delta_kernel::object_store::ClientOptions;
use delta_kernel::object_store::DynObjectStore;
use delta_kernel::object_store::RetryConfig;
use delta_kernel::object_store::aws::AmazonS3Builder;
use delta_kernel::snapshot::SnapshotRef;
use delta_kernel::{Engine, Snapshot};
use delta_kernel_default_engine::DefaultEngineBuilder;
use serde_json::{Map, Value};

use super::config::{DeltaLakeToolkitConfig, s3_endpoint};
use super::filter::RowFilter;
use super::render::{cell_json, schema_text};

/// The most rows a `query_table` or `vector_search` answers.
pub(in crate::toolkits) const MAX_RESULT_ROWS: usize = 1_000;
/// The most serialized bytes of rows an answer carries.
pub(in crate::toolkits) const MAX_RESULT_BYTES: usize = 1_024 * 1_024;
/// The most `k` a `vector_search` honours.
pub(in crate::toolkits) const MAX_K: usize = 100;
/// The most values an embedding (the query vector, or a stored one) has.
pub(in crate::toolkits) const MAX_EMBEDDING_DIMENSIONS: usize = 8_192;
/// A scan stops with an error past this many rows read.
const MAX_SCAN_ROWS: usize = 5_000_000;
/// A scan stops with an error past this many bytes of decoded Arrow data.
const MAX_SCAN_BYTES: usize = 512 * 1_024 * 1_024;
/// A scan stops with an error past this long.
const SCAN_DEADLINE: Duration = Duration::from_secs(60);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const MAX_ERROR_CHARS: usize = 400;

/// A failure with the sentence the tool answers.
#[derive(Debug)]
pub(in crate::toolkits) struct TableError {
    message: String,
}

impl TableError {
    fn new(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
        }
    }

    fn engine(context: &str, error: impl std::fmt::Display) -> Self {
        let text = error.to_string();
        let bounded = text.chars().take(MAX_ERROR_CHARS).collect::<String>();
        Self::new(format!("{context}: {bounded}"))
    }

    pub(in crate::toolkits) fn message(&self) -> &str {
        &self.message
    }
}

/// The rows an answer carries, and whether more existed.
#[derive(Debug)]
pub(in crate::toolkits) struct RowSet {
    pub(in crate::toolkits) rows: Vec<Map<String, Value>>,
    pub(in crate::toolkits) truncated: bool,
}

/// A `query_table` request, already parsed.
pub(in crate::toolkits) struct QueryRequest {
    pub(in crate::toolkits) columns: Option<Vec<String>>,
    pub(in crate::toolkits) filter: RowFilter,
}

/// A `vector_search` request, already parsed.
pub(in crate::toolkits) struct VectorRequest {
    pub(in crate::toolkits) embedding: Vec<f64>,
    pub(in crate::toolkits) k: usize,
    pub(in crate::toolkits) column: String,
}

/// One Delta table in an object store.
pub(in crate::toolkits) struct DeltaTable {
    store: Arc<DynObjectStore>,
    url: String,
    engine: OnceLock<Arc<dyn Engine>>,
    max_scan_rows: usize,
}

impl DeltaTable {
    /// The table of a toolkit's configuration, in S3. The endpoint is the
    /// region's own (`s3_endpoint`) over HTTPS; the key pair is given
    /// explicitly, so the store never consults the instance-metadata service
    /// or the process environment.
    pub(in crate::toolkits) fn connect(
        config: &DeltaLakeToolkitConfig,
    ) -> Result<Self, TableError> {
        Self::connect_endpoint(config, &s3_endpoint(config.region()), false)
    }

    /// [`Self::connect`] against an explicit endpoint, for the signing test's
    /// local stub. Production passes the region's own HTTPS endpoint.
    pub(in crate::toolkits) fn connect_endpoint(
        config: &DeltaLakeToolkitConfig,
        endpoint: &str,
        allow_http: bool,
    ) -> Result<Self, TableError> {
        let options = ClientOptions::new()
            .with_allow_http(allow_http)
            .with_timeout(REQUEST_TIMEOUT)
            .with_connect_timeout(CONNECT_TIMEOUT);
        let retry = RetryConfig {
            max_retries: 2,
            retry_timeout: REQUEST_TIMEOUT,
            ..RetryConfig::default()
        };
        let mut builder = AmazonS3Builder::new()
            .with_bucket_name(config.location().bucket())
            .with_region(config.region())
            .with_endpoint(endpoint)
            .with_access_key_id(config.access_key_id())
            .with_secret_access_key(config.secret_access_key())
            .with_virtual_hosted_style_request(false)
            .with_allow_http(allow_http)
            .with_client_options(options)
            .with_retry(retry);
        if let Some(token) = config.session_token() {
            builder = builder.with_token(token);
        }
        let store = builder
            .build()
            .map_err(|error| TableError::engine("Error initializing DeltaTable", error))?;
        Ok(Self::with_store(Arc::new(store), config.location().url()))
    }

    /// The table at `url` (`<scheme>://<bucket>/<prefix>/`) in `store`, whose
    /// root is the URL's bucket.
    pub(in crate::toolkits) fn with_store(store: Arc<DynObjectStore>, url: String) -> Self {
        Self {
            store,
            url,
            engine: OnceLock::new(),
            max_scan_rows: MAX_SCAN_ROWS,
        }
    }

    /// The same table with a smaller row budget, for the oversize test.
    #[cfg(test)]
    pub(in crate::toolkits) fn with_max_scan_rows(mut self, rows: usize) -> Self {
        self.max_scan_rows = rows;
        self
    }

    /// `get_table_schema`: the schema as pyarrow prints it.
    pub(in crate::toolkits) fn schema_text(&self) -> Result<String, TableError> {
        let (_, snapshot) = self.open()?;
        let schema = ArrowSchema::try_from_kernel(snapshot.schema().as_ref())
            .map_err(|error| TableError::engine("Error reading the table schema", error))?;
        Ok(schema_text(&schema))
    }

    /// `query_table`: the rows that satisfy the filter, in table order.
    pub(in crate::toolkits) fn query(&self, request: &QueryRequest) -> Result<RowSet, TableError> {
        let (engine, snapshot) = self.open()?;
        let schema = snapshot.schema();
        for column in request
            .columns
            .iter()
            .flatten()
            .map(String::as_str)
            .chain(request.filter.columns())
        {
            if schema.field(column).is_none() {
                return Err(TableError::new(format!(
                    "Column '{column}' not found in table."
                )));
            }
        }
        // Read only what is needed: the requested columns and the filter's.
        let read: Option<Vec<String>> = request.columns.as_ref().map(|columns| {
            let mut read = columns.clone();
            for column in request.filter.columns() {
                if !read.iter().any(|name| name == column) {
                    read.push(column.to_owned());
                }
            }
            read
        });
        let output: Option<Vec<String>> = request.columns.clone();
        let mut rows: Vec<Map<String, Value>> = Vec::new();
        let mut bytes = 0usize;
        let mut truncated = false;
        self.scan(&engine, &snapshot, read.as_deref(), |batch| {
            let mask = request.filter.matches(batch).map_err(TableError::new)?;
            let names = output_names(batch, output.as_deref());
            for (row, keep) in mask.iter().enumerate() {
                if !*keep {
                    continue;
                }
                if rows.len() >= MAX_RESULT_ROWS {
                    truncated = true;
                    return Ok(false);
                }
                let object = row_object(batch, &names, row);
                bytes = bytes.saturating_add(serialized_len(&object));
                if bytes > MAX_RESULT_BYTES && !rows.is_empty() {
                    truncated = true;
                    return Ok(false);
                }
                rows.push(object);
            }
            Ok(true)
        })?;
        Ok(RowSet { rows, truncated })
    }

    /// `vector_search`: the `k` rows whose embedding column is most similar
    /// (cosine) to the query vector, best first. Rows without an embedding
    /// (null, empty or all zeros) are skipped, as the SDK skips null ones.
    pub(in crate::toolkits) fn vector_search(
        &self,
        request: &VectorRequest,
    ) -> Result<RowSet, TableError> {
        let (engine, snapshot) = self.open()?;
        let column = request.column.as_str();
        if snapshot.schema().field(column).is_none() {
            return Err(TableError::new(format!(
                "Embedding column '{column}' not found in table."
            )));
        }
        let query_norm = norm(&request.embedding);
        if query_norm == 0.0 {
            return Err(TableError::new(
                "The query embedding must have at least one non-zero value.",
            ));
        }
        let mut best: Vec<(f64, Map<String, Value>)> = Vec::with_capacity(request.k);
        self.scan(&engine, &snapshot, None, |batch| {
            let index = batch.schema().index_of(column).map_err(|_| {
                TableError::new(format!("Embedding column '{column}' not found in table."))
            })?;
            let embeddings = batch.column(index);
            let names = output_names(batch, None);
            for row in 0..batch.num_rows() {
                let Some(vector) = embedding_at(embeddings, row, column)? else {
                    continue;
                };
                if vector.len() != request.embedding.len() {
                    return Err(TableError::new(format!(
                        "The query embedding has {} values but the rows of '{column}' have {}.",
                        request.embedding.len(),
                        vector.len()
                    )));
                }
                let row_norm = norm(&vector);
                if row_norm == 0.0 {
                    continue;
                }
                let dot: f64 = vector
                    .iter()
                    .zip(&request.embedding)
                    .map(|(left, right)| left * right)
                    .sum();
                let score = dot / (row_norm * query_norm);
                if !score.is_finite() {
                    continue;
                }
                admit(&mut best, request.k, score, || {
                    row_object(batch, &names, row)
                });
            }
            Ok(true)
        })?;
        best.sort_by(|left, right| right.0.total_cmp(&left.0));
        let mut rows = Vec::with_capacity(best.len());
        let mut bytes = 0usize;
        let mut truncated = false;
        for (_, object) in best {
            bytes = bytes.saturating_add(serialized_len(&object));
            if bytes > MAX_RESULT_BYTES && !rows.is_empty() {
                truncated = true;
                break;
            }
            rows.push(object);
        }
        Ok(RowSet { rows, truncated })
    }

    fn engine(&self) -> Arc<dyn Engine> {
        Arc::clone(
            self.engine.get_or_init(|| {
                Arc::new(DefaultEngineBuilder::new(Arc::clone(&self.store)).build())
            }),
        )
    }

    fn open(&self) -> Result<(Arc<dyn Engine>, SnapshotRef), TableError> {
        let engine = self.engine();
        let snapshot = Snapshot::builder_for(&self.url)
            .build(engine.as_ref())
            .map_err(|error| TableError::engine("Error initializing DeltaTable", error))?;
        Ok((engine, snapshot))
    }

    /// Read the table in kernel batches, calling `visit` on each until it
    /// answers `false`. `columns` restricts the read (`None` reads all). A
    /// scan past its row, byte or time budget is an error, never a partial
    /// answer taken for a complete one.
    fn scan(
        &self,
        engine: &Arc<dyn Engine>,
        snapshot: &SnapshotRef,
        columns: Option<&[String]>,
        mut visit: impl FnMut(&RecordBatch) -> Result<bool, TableError>,
    ) -> Result<(), TableError> {
        let mut builder = Arc::clone(snapshot).scan_builder();
        if let Some(columns) = columns {
            let projected = snapshot
                .schema()
                .project(columns)
                .map_err(|error| TableError::engine("Error selecting columns", error))?;
            builder = builder.with_schema(projected);
        }
        let scan = builder
            .build()
            .map_err(|error| TableError::engine("Error planning the table scan", error))?;
        let batches = scan
            .execute(Arc::clone(engine))
            .map_err(|error| TableError::engine("Error reading the table", error))?;
        let started = Instant::now();
        let (mut rows, mut bytes) = (0usize, 0usize);
        for data in batches {
            let data =
                data.map_err(|error| TableError::engine("Error reading the table", error))?;
            let batch: RecordBatch = ArrowEngineData::try_from_engine_data(data)
                .map_err(|error| TableError::engine("Error reading the table", error))?
                .into();
            rows = rows.saturating_add(batch.num_rows());
            bytes = bytes.saturating_add(batch.get_array_memory_size());
            if rows > self.max_scan_rows || bytes > MAX_SCAN_BYTES {
                return Err(TableError::new(format!(
                    "The table is too large to scan within this toolkit's limits ({} rows, {} MiB). Narrow the read with columns and filters.",
                    self.max_scan_rows,
                    MAX_SCAN_BYTES / (1_024 * 1_024)
                )));
            }
            if started.elapsed() > SCAN_DEADLINE {
                return Err(TableError::new(format!(
                    "Reading the table took longer than {} seconds. Narrow the read with columns and filters.",
                    SCAN_DEADLINE.as_secs()
                )));
            }
            if !visit(&batch)? {
                break;
            }
        }
        Ok(())
    }
}

/// The output columns of a batch, in the requested order (the schema's order
/// when none is requested).
fn output_names(batch: &RecordBatch, requested: Option<&[String]>) -> Vec<(String, usize)> {
    let schema = batch.schema();
    match requested {
        Some(requested) => requested
            .iter()
            .filter_map(|name| {
                schema
                    .index_of(name)
                    .ok()
                    .map(|index| (name.clone(), index))
            })
            .collect(),
        None => schema
            .fields()
            .iter()
            .enumerate()
            .map(|(index, field)| (field.name().clone(), index))
            .collect(),
    }
}

fn row_object(batch: &RecordBatch, names: &[(String, usize)], row: usize) -> Map<String, Value> {
    let mut object = Map::with_capacity(names.len());
    for (name, index) in names {
        object.insert(name.clone(), cell_json(batch.column(*index), row));
    }
    object
}

fn serialized_len(object: &Map<String, Value>) -> usize {
    serde_json::to_vec(object).map_or(usize::MAX, |bytes| bytes.len())
}

fn norm(vector: &[f64]) -> f64 {
    vector.iter().map(|value| value * value).sum::<f64>().sqrt()
}

/// Keep `candidate` among the best `k` by score, rendering its row only when
/// it is kept.
fn admit(
    best: &mut Vec<(f64, Map<String, Value>)>,
    k: usize,
    score: f64,
    row: impl FnOnce() -> Map<String, Value>,
) {
    if best.len() < k {
        best.push((score, row()));
        return;
    }
    let Some((worst, _)) = best
        .iter()
        .enumerate()
        .min_by(|left, right| left.1.0.total_cmp(&right.1.0))
    else {
        return;
    };
    if score > best[worst].0 {
        best[worst] = (score, row());
    }
}

/// The embedding of one row as floats: `None` for a null or empty one, an
/// error when the column is not a list of numbers.
fn embedding_at(
    array: &ArrayRef,
    row: usize,
    column: &str,
) -> Result<Option<Vec<f64>>, TableError> {
    if array.is_null(row) {
        return Ok(None);
    }
    let values: ArrayRef = match array.data_type() {
        DataType::List(_) => array.as_list::<i32>().value(row),
        DataType::LargeList(_) => array.as_list::<i64>().value(row),
        DataType::FixedSizeList(..) => array.as_fixed_size_list().value(row),
        _ => {
            return Err(TableError::new(format!(
                "Column '{column}' is not a list of numbers, so it cannot hold embeddings."
            )));
        }
    };
    if values.is_empty() {
        return Ok(None);
    }
    if values.len() > MAX_EMBEDDING_DIMENSIONS {
        return Err(TableError::new(format!(
            "A row of '{column}' has {} values; at most {MAX_EMBEDDING_DIMENSIONS} are supported.",
            values.len()
        )));
    }
    let floats = cast(&values, &DataType::Float64).map_err(|_| {
        TableError::new(format!(
            "Column '{column}' is not a list of numbers, so it cannot hold embeddings."
        ))
    })?;
    let numbers: &Float64Array = floats.as_primitive::<Float64Type>();
    if numbers.null_count() != 0 {
        return Ok(None);
    }
    Ok(Some(numbers.values().to_vec()))
}
