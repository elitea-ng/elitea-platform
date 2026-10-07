//! Embeddings from the gateway's OpenAI-compatible `/embeddings`
//! (ADR-0026 decision 3).
//!
//! What it keeps from `LangChain`'s `OpenAIEmbeddings` as the Python engine
//! used it:
//!
//! * the length split: a text above 8191 `cl100k_base` tokens is embedded
//!   in windows whose vectors are averaged, weighted by token count, and
//!   normalised (`_process_batched_chunked_embeddings`);
//! * a request carries at most `batch_size` inputs (the Python indexer
//!   called it with `WIKI_EMBED_BATCH_SIZE` texts, default 64) and at most
//!   300 000 tokens (`MAX_TOKENS_PER_REQUEST`);
//! * an empty text gets the vector of `""`, requested once;
//! * `max_retries` from `llm_settings`, default 2 (see `transport`).
//!
//! What it fixes:
//!
//! * the DIMENSION is read from the first response and enforced for every
//!   later vector, instead of the constant 1536 the Python `repo_vec` table
//!   assumed. A gateway that answers one batch from another model (a
//!   routing change mid-run) fails the run naming the model, rather than
//!   storing vectors no query can compare;
//! * a failed batch fails the call. The Python indexer logged it and
//!   carried on, so a wiki could publish with a fraction of its vectors.
//!   Whether a failure is fatal is the CALLER's decision now, made once.
//!
//! Requests run `concurrency` at a time (the Python engine ran them one by
//! one); a stop aborts every request in flight.

use super::settings::ModelSettings;
use super::tokens::{EMBEDDING_CTX_LENGTH, Window, embedding_split};
use super::transport::{BodyError, Call, Transport, read_limited};
use crate::errors::{EngineError, ErrorType};
use crate::runner::StopSignal;
use serde_json::{Value, json};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, OnceLock};
use tokio::task::JoinSet;

/// One request's vectors, in input order.
type BatchResult = Result<Vec<Vec<f32>>, EngineError>;

/// `LangChain`'s `MAX_TOKENS_PER_REQUEST`.
pub const MAX_TOKENS_PER_REQUEST: usize = 300_000;

/// The Python indexer's `WIKI_EMBED_BATCH_SIZE` default.
pub const DEFAULT_BATCH_SIZE: usize = 64;

/// Requests in flight at once.
pub const DEFAULT_CONCURRENCY: usize = 4;

/// A response body cap: 64 inputs of 4096 dimensions as JSON floats is
/// about 6 MiB, so this leaves room without being unbounded.
const MAX_RESPONSE_BYTES: usize = 64 * 1024 * 1024;

/// How the client batches.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct EmbeddingOptions {
    pub batch_size: usize,
    pub concurrency: usize,
    /// The window length, in `cl100k_base` tokens.
    pub ctx_length: usize,
}

impl Default for EmbeddingOptions {
    fn default() -> Self {
        Self {
            batch_size: DEFAULT_BATCH_SIZE,
            concurrency: DEFAULT_CONCURRENCY,
            ctx_length: EMBEDDING_CTX_LENGTH,
        }
    }
}

#[derive(Debug)]
struct Inner {
    transport: Transport,
    settings: ModelSettings,
    model: String,
    url: String,
    options: EmbeddingOptions,
    dimension: OnceLock<usize>,
    prompt_tokens: AtomicU64,
}

/// The embedding client of one invocation. Clones share the discovered
/// dimension.
#[derive(Debug, Clone)]
pub struct EmbeddingClient {
    inner: Arc<Inner>,
}

impl EmbeddingClient {
    /// A client for `model` over the invocation's transport settings.
    #[must_use]
    pub fn new(
        transport: Transport,
        settings: ModelSettings,
        model: impl Into<String>,
        options: EmbeddingOptions,
    ) -> Self {
        let url = format!("{}/embeddings", settings.api_base);
        Self {
            inner: Arc::new(Inner {
                transport,
                settings,
                model: model.into(),
                url,
                options: EmbeddingOptions {
                    batch_size: options.batch_size.max(1),
                    concurrency: options.concurrency.max(1),
                    ctx_length: options.ctx_length.max(1),
                },
                dimension: OnceLock::new(),
                prompt_tokens: AtomicU64::new(0),
            }),
        }
    }

    /// The model name requests carry.
    #[must_use]
    pub fn model(&self) -> &str {
        &self.inner.model
    }

    /// The dimension the first response established, if any came yet.
    #[must_use]
    pub fn dimension(&self) -> Option<usize> {
        self.inner.dimension.get().copied()
    }

    /// Prompt tokens the gateway reported so far (`usage.prompt_tokens`).
    #[must_use]
    pub fn prompt_tokens(&self) -> u64 {
        self.inner.prompt_tokens.load(Ordering::Relaxed)
    }

    /// One vector per text, in order.
    ///
    /// # Errors
    ///
    /// The first failed request (after its retries), a malformed response,
    /// a dimension mismatch, or the stop line.
    pub async fn embed_documents(
        &self,
        texts: &[String],
        stop: &StopSignal,
    ) -> Result<Vec<Vec<f32>>, EngineError> {
        if texts.is_empty() {
            return Ok(Vec::new());
        }
        let owned = texts.to_vec();
        let ctx = self.inner.options.ctx_length;
        // Tokenising a whole repository's worth of text is CPU work that
        // must not stall the socket's other invocations.
        let windows: Vec<Vec<Window>> = tokio::task::spawn_blocking(move || {
            owned
                .iter()
                .map(|text| embedding_split(text, ctx))
                .collect::<Result<Vec<_>, _>>()
        })
        .await
        .map_err(|_| runtime("the embedding tokenizer task stopped unexpectedly"))??;

        let mut inputs: Vec<(usize, Window)> = Vec::new();
        for (text, text_windows) in windows.into_iter().enumerate() {
            inputs.extend(text_windows.into_iter().map(|window| (text, window)));
        }
        let requests = plan_requests(&inputs, self.inner.options.batch_size);
        let vectors = self.run_requests(&inputs, &requests, stop).await?;

        let mut per_text: Vec<Vec<(Vec<f32>, usize)>> = vec![Vec::new(); texts.len()];
        for ((text, window), vector) in inputs.into_iter().zip(vectors) {
            per_text[text].push((vector, window.tokens));
        }
        let mut empty: Option<Vec<f32>> = None;
        let mut out = Vec::with_capacity(texts.len());
        for parts in per_text {
            let vector = match parts.len() {
                0 => {
                    if empty.is_none() {
                        empty = Some(self.embed_empty(stop).await?);
                    }
                    empty.clone().unwrap_or_default()
                }
                1 => parts.into_iter().next().map(|(v, _)| v).unwrap_or_default(),
                _ => weighted_average(&parts),
            };
            out.push(vector);
        }
        Ok(out)
    }

    /// The vector of one query text (`LangChain`'s `embed_query`).
    ///
    /// # Errors
    ///
    /// See [`EmbeddingClient::embed_documents`].
    pub async fn embed_query(
        &self,
        text: &str,
        stop: &StopSignal,
    ) -> Result<Vec<f32>, EngineError> {
        let mut vectors = self.embed_documents(&[text.to_owned()], stop).await?;
        vectors
            .pop()
            .ok_or_else(|| runtime("the embedding client returned no vector for the query"))
    }

    async fn run_requests(
        &self,
        inputs: &[(usize, Window)],
        requests: &[std::ops::Range<usize>],
        stop: &StopSignal,
    ) -> Result<Vec<Vec<f32>>, EngineError> {
        let mut results: Vec<Option<Vec<Vec<f32>>>> = vec![None; requests.len()];
        let mut tasks: JoinSet<(usize, BatchResult)> = JoinSet::new();
        let mut next = 0;
        loop {
            while next < requests.len() && tasks.len() < self.inner.options.concurrency {
                let batch: Vec<String> = inputs[requests[next].clone()]
                    .iter()
                    .map(|(_, window)| window.text.clone())
                    .collect();
                let client = self.clone();
                let stop = stop.clone();
                let index = next;
                tasks
                    .spawn(async move { (index, client.request(Value::from(batch), &stop).await) });
                next += 1;
            }
            let Some(joined) = tasks.join_next().await else {
                break;
            };
            let (index, result) =
                joined.map_err(|_| runtime("an embedding request task stopped unexpectedly"))?;
            // Dropping `tasks` on this early return aborts the others.
            results[index] = Some(result?);
        }
        Ok(results.into_iter().flatten().flatten().collect())
    }

    async fn embed_empty(&self, stop: &StopSignal) -> Result<Vec<f32>, EngineError> {
        let mut vectors = self.request(Value::from(""), stop).await?;
        vectors
            .pop()
            .ok_or_else(|| self.call().protocol_error("no vector for the empty text"))
    }

    fn call(&self) -> Call<'_> {
        let inner = &self.inner;
        Call {
            what: "Embedding",
            url: &inner.url,
            model: &inner.model,
            key: &inner.settings.api_key,
            organization: inner.settings.organization.as_deref(),
            max_retries: inner.settings.max_retries,
            streaming: false,
        }
    }

    /// One `/embeddings` call; `input` is a list of texts or one text.
    async fn request(&self, input: Value, stop: &StopSignal) -> Result<Vec<Vec<f32>>, EngineError> {
        let expected = input.as_array().map_or(1, Vec::len);
        let call = self.call();
        let body = json!({
            "model": self.inner.model,
            "input": input,
            // The SDK asks for base64 by default; a gateway in front of a
            // non-OpenAI model may not offer it. Floats always work.
            "encoding_format": "float",
        });
        let body = serde_json::to_vec(&body)
            .map_err(|_| runtime("the embedding request cannot be encoded"))?;
        let mut response = self.inner.transport.post(&call, body, stop).await?;
        let read = tokio::select! {
            read = read_limited(&mut response, MAX_RESPONSE_BYTES, self.inner.transport.timeouts().request) => read,
            () = stop.stopped() => return Err(EngineError::cancelled()),
        };
        let bytes = match read {
            Ok(bytes) => bytes,
            Err(BodyError::Timeout) => {
                return Err(call.timeout_error(self.inner.transport.timeouts().request));
            }
            Err(BodyError::TooLarge) => {
                return Err(call.protocol_error("the response exceeded its size cap"));
            }
            Err(BodyError::Transport(detail)) => {
                return Err(call.protocol_error(&format!("the response broke off: {detail}")));
            }
        };
        let vectors = self.parse(&call, &bytes, expected)?;
        Ok(vectors)
    }

    fn parse(
        &self,
        call: &Call<'_>,
        bytes: &[u8],
        expected: usize,
    ) -> Result<Vec<Vec<f32>>, EngineError> {
        let value: Value = serde_json::from_slice(bytes)
            .map_err(|_| call.protocol_error("the response is not JSON"))?;
        if let Some(tokens) = value
            .pointer("/usage/prompt_tokens")
            .and_then(Value::as_u64)
        {
            self.inner
                .prompt_tokens
                .fetch_add(tokens, Ordering::Relaxed);
        }
        let data = value
            .get("data")
            .and_then(Value::as_array)
            .ok_or_else(|| call.protocol_error("the response has no data list"))?;
        if data.len() != expected {
            return Err(call.protocol_error(&format!(
                "the response holds {} vectors for {expected} inputs",
                data.len()
            )));
        }
        let mut slots: Vec<Option<Vec<f32>>> = vec![None; expected];
        for (position, item) in data.iter().enumerate() {
            // `index` orders the vectors; a server that omits it is taken
            // to answer in input order.
            let index = match item.get("index") {
                None | Some(Value::Null) => position,
                Some(index) => index
                    .as_u64()
                    .and_then(|i| usize::try_from(i).ok())
                    .filter(|i| *i < expected)
                    .ok_or_else(|| call.protocol_error("a vector has an index out of range"))?,
            };
            let numbers = item
                .get("embedding")
                .and_then(Value::as_array)
                .ok_or_else(|| call.protocol_error("a vector is not a list of floats"))?;
            #[allow(clippy::cast_possible_truncation)]
            let vector: Vec<f32> = numbers
                .iter()
                .map(|n| n.as_f64().map(|f| f as f32))
                .collect::<Option<_>>()
                .ok_or_else(|| {
                    call.protocol_error("a vector holds a value that is not a number")
                })?;
            self.check_dimension(vector.len())?;
            if slots[index].replace(vector).is_some() {
                return Err(call.protocol_error("two vectors share one index"));
            }
        }
        Ok(slots.into_iter().flatten().collect())
    }

    fn check_dimension(&self, found: usize) -> Result<(), EngineError> {
        if found == 0 {
            return Err(self
                .call()
                .protocol_error("the gateway returned an empty vector"));
        }
        let expected = *self.inner.dimension.get_or_init(|| found);
        if found != expected {
            return Err(runtime(format!(
                "Embedding inference failed: model '{}' returned a {found}-dimension vector, but this run's earlier vectors have {expected} dimensions; check that the gateway routes '{}' to one model",
                self.inner.model, self.inner.model
            )));
        }
        Ok(())
    }
}

fn runtime(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Runtime, message)
}

/// `LangChain`'s request grouping: up to `batch_size` inputs, closing a
/// request before it would pass [`MAX_TOKENS_PER_REQUEST`] (a single input
/// above it still goes, alone).
fn plan_requests(inputs: &[(usize, Window)], batch_size: usize) -> Vec<std::ops::Range<usize>> {
    let mut requests = Vec::new();
    let mut start = 0;
    while start < inputs.len() {
        let mut tokens = 0;
        let mut end = start;
        while end < inputs.len() && end - start < batch_size {
            let next = inputs[end].1.tokens;
            if tokens + next > MAX_TOKENS_PER_REQUEST && end > start {
                break;
            }
            tokens += next;
            end += 1;
        }
        requests.push(start..end);
        start = end;
    }
    requests
}

/// `_process_batched_chunked_embeddings` for a text of several windows.
fn weighted_average(parts: &[(Vec<f32>, usize)]) -> Vec<f32> {
    let dimension = parts.first().map_or(0, |(v, _)| v.len());
    let total: f64 = parts.iter().map(|(_, w)| w_f64(*w)).sum();
    let mut average = vec![0.0_f64; dimension];
    for (vector, weight) in parts {
        for (slot, value) in average.iter_mut().zip(vector) {
            *slot += f64::from(*value) * w_f64(*weight);
        }
    }
    for slot in &mut average {
        *slot /= total;
    }
    let magnitude = average.iter().map(|v| v * v).sum::<f64>().sqrt();
    #[allow(clippy::cast_possible_truncation)]
    average
        .into_iter()
        .map(|v| {
            if magnitude > 0.0 {
                (v / magnitude) as f32
            } else {
                v as f32
            }
        })
        .collect()
}

#[allow(clippy::cast_precision_loss)]
fn w_f64(weight: usize) -> f64 {
    weight as f64
}

#[cfg(test)]
mod tests {
    use super::*;

    fn window(tokens: usize) -> (usize, Window) {
        (
            0,
            Window {
                text: String::new(),
                tokens,
            },
        )
    }

    #[test]
    fn requests_respect_count_and_token_caps() {
        let inputs: Vec<_> = (0..5).map(|_| window(10)).collect();
        assert_eq!(plan_requests(&inputs, 2), vec![0..2, 2..4, 4..5]);
        let heavy = vec![
            window(200_000),
            window(200_000),
            window(MAX_TOKENS_PER_REQUEST + 1),
            window(1),
        ];
        assert_eq!(plan_requests(&heavy, 64), vec![0..1, 1..2, 2..3, 3..4]);
    }

    #[test]
    fn several_windows_average_by_weight_and_normalise() {
        let averaged = weighted_average(&[(vec![1.0, 0.0], 3), (vec![0.0, 1.0], 1)]);
        // (0.75, 0.25) normalised.
        let norm = (0.75_f64.powi(2) + 0.25_f64.powi(2)).sqrt();
        #[allow(clippy::cast_possible_truncation)]
        let expected = [(0.75 / norm) as f32, (0.25 / norm) as f32];
        assert_eq!(averaged, expected);
    }
}
