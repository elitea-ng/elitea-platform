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
//!
//! # The context-length fallback
//!
//! The windows are counted in `cl100k_base` tokens, but the model counts
//! with its own tokenizer. A model whose tokenizer counts more tokens for
//! the same text (Qwen3-Embedding behind vLLM, 8192 tokens of context)
//! refuses a full window with HTTP 400. `LangChain` sent the cl100k token
//! IDS, which never passed the limit but gave meaningless vectors to a
//! non-OpenAI model; this client sends text, so it must recover:
//!
//! 1. A request refused by [`is_context_length_refusal`] is sent again one
//!    window at a time, in the same task (so within `concurrency`).
//! 2. A window refused on its own is cut in two with the same tokenizer;
//!    each half is embedded the same way (and cut again if needed), and
//!    the halves' vectors are averaged by token count, exactly as the
//!    windows of a long text are. The window's place and weight do not
//!    change.
//! 3. A window of [`MIN_SPLIT_TOKENS`] or fewer is not cut: the run fails
//!    and names `ELITEA_DEEPWIKI_EMBED_CTX_TOKENS`. The extra requests of
//!    one client are capped at [`FALLBACK_REQUESTS_PER_WINDOW`] per window
//!    plus [`FALLBACK_REQUESTS_BASE`]; past the cap the run fails too.
//!
//! Any other refusal fails the call, as before. The client counts the
//! windows it cut ([`EmbeddingClient::split_windows`]) and logs the count
//! once, when the run's last client handle goes.

use super::settings::ModelSettings;
use super::tokens::{EMBEDDING_CTX_LENGTH, Window, embedding_split};
use super::transport::{BodyError, Call, PostError, Transport, read_limited};
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

/// The smallest window the context-length fallback cuts in two, in
/// `cl100k_base` tokens; also the smallest `ELITEA_DEEPWIKI_EMBED_CTX_TOKENS`.
pub const MIN_SPLIT_TOKENS: usize = 256;

/// The fallback's extra requests allowed per window this client embedded.
/// Sending a refused batch again one window at a time costs one per
/// window; each cut costs two.
pub const FALLBACK_REQUESTS_PER_WINDOW: u64 = 4;

/// Extra requests allowed on top of the per-window share, so that a small
/// run with one long window can still cut it down to the floor.
pub const FALLBACK_REQUESTS_BASE: u64 = 64;

/// The name an operator sets to shrink the windows.
const CTX_SETTING: &str = "ELITEA_DEEPWIKI_EMBED_CTX_TOKENS";

/// Phrases that mark a refusal as a context-length refusal, lower case.
///
/// * `maximum context length`: `OpenAI` ("This model's maximum context
///   length is 8192 tokens, however you requested …") and vLLM's
///   OpenAI-compatible server (same sentence);
/// * `context_length_exceeded`: `OpenAI`'s error `code`;
/// * `max context`: vLLM's newer wording ("max context 8192 tokens, the
///   prompt had at least 8193");
/// * `maximum model length`: vLLM's prompt check ("… is longer than the
///   maximum model length of 8192").
const CONTEXT_PHRASES: [&str; 4] = [
    "maximum context length",
    "context_length_exceeded",
    "max context",
    "maximum model length",
];

/// Whether a refusal is the model saying an input is over its context.
///
/// Only HTTP 400 counts, and only when the body names the context limit:
/// every other 400 (a bad model name, a malformed request) and every other
/// status keeps failing the call. The body is matched as raw text, so a
/// gateway that wraps the provider's error in its own envelope still
/// matches.
#[must_use]
pub fn is_context_length_refusal(status: u16, body: &str) -> bool {
    if status != 400 {
        return false;
    }
    let body = body.to_lowercase();
    CONTEXT_PHRASES.iter().any(|phrase| body.contains(phrase))
}

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
    /// Windows embedded (the fallback's request budget grows with it).
    windows: AtomicU64,
    /// Windows the context-length fallback cut in two.
    split_windows: AtomicU64,
    /// Requests the context-length fallback added.
    fallback_requests: AtomicU64,
}

impl Drop for Inner {
    fn drop(&mut self) {
        let split = *self.split_windows.get_mut();
        if split > 0 {
            tracing::warn!(
                model = %self.model,
                split_windows = split,
                extra_requests = *self.fallback_requests.get_mut(),
                ctx_tokens = self.options.ctx_length,
                "the embedding model refused windows as over its context; they were cut and their halves averaged. Set {CTX_SETTING} below the model's context to avoid the extra requests"
            );
        }
    }
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
                windows: AtomicU64::new(0),
                split_windows: AtomicU64::new(0),
                fallback_requests: AtomicU64::new(0),
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

    /// Windows the context-length fallback cut so far. A window counts
    /// once, however many times its pieces were cut again.
    #[must_use]
    pub fn split_windows(&self) -> u64 {
        self.inner.split_windows.load(Ordering::Relaxed)
    }

    /// Requests the context-length fallback added so far.
    #[must_use]
    pub fn fallback_requests(&self) -> u64 {
        self.inner.fallback_requests.load(Ordering::Relaxed)
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
        self.inner
            .windows
            .fetch_add(inputs.len() as u64, Ordering::Relaxed);
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
                let batch: Vec<Window> = inputs[requests[next].clone()]
                    .iter()
                    .map(|(_, window)| window.clone())
                    .collect();
                let client = self.clone();
                let stop = stop.clone();
                let index = next;
                tasks.spawn(async move { (index, client.embed_batch(batch, &stop).await) });
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

    /// One planned request, with the context-length fallback.
    async fn embed_batch(&self, batch: Vec<Window>, stop: &StopSignal) -> BatchResult {
        let texts: Vec<String> = batch.iter().map(|window| window.text.clone()).collect();
        match self.request_classified(Value::from(texts), stop).await {
            Ok(vectors) => Ok(vectors),
            Err(failure) if refused_for_context(&failure) => {
                tracing::debug!(
                    model = %self.inner.model,
                    windows = batch.len(),
                    "embedding request refused as over the model's context; sending its windows one at a time"
                );
                let single = batch.len() == 1;
                let mut vectors = Vec::with_capacity(batch.len());
                for window in batch {
                    let vector = if single {
                        // That request WAS this window alone.
                        self.split_refused(window, 0, stop).await?
                    } else {
                        self.embed_alone(window, 0, stop).await?
                    };
                    vectors.push(vector);
                }
                Ok(vectors)
            }
            Err(failure) => Err(failure.error),
        }
    }

    /// Embed one window in its own request; cut it if the model refuses it
    /// for context. `depth` is 0 for a planned window, more for a piece.
    async fn embed_alone(
        &self,
        window: Window,
        depth: u32,
        stop: &StopSignal,
    ) -> Result<Vec<f32>, EngineError> {
        self.take_fallback_requests(1)?;
        match self
            .request_classified(Value::from(vec![window.text.clone()]), stop)
            .await
        {
            Ok(mut vectors) => vectors
                .pop()
                .ok_or_else(|| self.call().protocol_error("no vector for a window")),
            Err(failure) if refused_for_context(&failure) => {
                Box::pin(self.split_refused(window, depth, stop)).await
            }
            Err(failure) => Err(failure.error),
        }
    }

    /// A window the model refused on its own: cut it in two, embed the
    /// pieces, and average them by token count.
    async fn split_refused(
        &self,
        window: Window,
        depth: u32,
        stop: &StopSignal,
    ) -> Result<Vec<f32>, EngineError> {
        if window.tokens <= MIN_SPLIT_TOKENS {
            return Err(runtime(format!(
                "Embedding inference failed for model '{}': the model refused a window of {} cl100k_base tokens as over its context, and a window of {MIN_SPLIT_TOKENS} tokens or fewer is not cut further; check the model's maximum context and set {CTX_SETTING} (now {}) below it",
                self.inner.model, window.tokens, self.inner.options.ctx_length
            )));
        }
        if stop.is_requested() {
            return Err(EngineError::cancelled());
        }
        let Window { text, tokens } = window;
        let pieces = tokio::task::spawn_blocking(move || halve(&text, tokens))
            .await
            .map_err(|_| runtime("the embedding tokenizer task stopped unexpectedly"))??;
        if depth == 0 {
            self.inner.split_windows.fetch_add(1, Ordering::Relaxed);
        }
        let mut parts = Vec::with_capacity(pieces.len());
        for piece in pieces {
            let weight = piece.tokens;
            parts.push((self.embed_alone(piece, depth + 1, stop).await?, weight));
        }
        Ok(weighted_average(&parts))
    }

    /// Reserve `count` fallback requests, or fail past the cap.
    fn take_fallback_requests(&self, count: u64) -> Result<(), EngineError> {
        let inner = &self.inner;
        let cap = inner
            .windows
            .load(Ordering::Relaxed)
            .saturating_mul(FALLBACK_REQUESTS_PER_WINDOW)
            .saturating_add(FALLBACK_REQUESTS_BASE);
        let used = inner.fallback_requests.fetch_add(count, Ordering::Relaxed) + count;
        if used > cap {
            return Err(runtime(format!(
                "Embedding inference failed for model '{}': the model refused so many windows as over its context that the fallback reached its cap of {cap} extra requests; set {CTX_SETTING} (now {}) below the model's maximum context",
                inner.model, inner.options.ctx_length
            )));
        }
        Ok(())
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
        self.request_classified(input, stop)
            .await
            .map_err(|failure| failure.error)
    }

    /// [`EmbeddingClient::request`], keeping a refusal for the fallback.
    async fn request_classified(
        &self,
        input: Value,
        stop: &StopSignal,
    ) -> Result<Vec<Vec<f32>>, PostError> {
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
        let mut response = self
            .inner
            .transport
            .post_classified(&call, body, stop)
            .await?;
        let read = tokio::select! {
            read = read_limited(&mut response, MAX_RESPONSE_BYTES, self.inner.transport.timeouts().request) => read,
            () = stop.stopped() => return Err(EngineError::cancelled().into()),
        };
        let bytes = match read {
            Ok(bytes) => bytes,
            Err(BodyError::Timeout) => {
                return Err(call
                    .timeout_error(self.inner.transport.timeouts().request)
                    .into());
            }
            Err(BodyError::TooLarge) => {
                return Err(call
                    .protocol_error("the response exceeded its size cap")
                    .into());
            }
            Err(BodyError::Transport(detail)) => {
                return Err(call
                    .protocol_error(&format!("the response broke off: {detail}"))
                    .into());
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

fn refused_for_context(failure: &PostError) -> bool {
    failure
        .refusal
        .as_ref()
        .is_some_and(|refusal| is_context_length_refusal(refusal.status, &refusal.body))
}

/// Cut a window of `tokens` `cl100k_base` tokens into pieces of at most
/// half of that. Decoding and encoding again can merge tokens across the
/// old window edges, so when the first cut gives one piece, cut by the
/// piece's own count.
fn halve(text: &str, tokens: usize) -> Result<Vec<Window>, EngineError> {
    let mut pieces = embedding_split(text, tokens.div_ceil(2))?;
    if let [only] = pieces.as_slice()
        && only.tokens > 1
    {
        let again = only.tokens.div_ceil(2);
        pieces = embedding_split(text, again)?;
    }
    Ok(pieces)
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

    #[test]
    fn context_refusals_are_recognised_by_status_and_phrase() {
        // vLLM's OpenAI-compatible server (0.6–0.9).
        let vllm = r#"{"object":"error","message":"This model's maximum context length is 8192 tokens. However, you requested 8193 tokens in the input for embedding generation. Please reduce the length of the input.","type":"BadRequestError","param":null,"code":400}"#;
        // vLLM's newer wording, as the benchmark saw it.
        let vllm_new = r#"{"error":{"message":"max context 8192 tokens, the prompt had at least 8193","type":"BadRequestError","param":null,"code":400}}"#;
        // vLLM's prompt-length check.
        let vllm_prompt = r#"{"object":"error","message":"The decoder prompt (length 8193) is longer than the maximum model length of 8192. Make sure that `max_model_len` is no smaller than the number of text tokens.","type":"BadRequestError","code":400}"#;
        // OpenAI.
        let openai = r#"{"error":{"message":"This model's maximum context length is 8192 tokens, however you requested 9100 tokens (9100 in your prompt; 0 for the completion). Please reduce your prompt; or completion length.","type":"invalid_request_error","param":null,"code":"context_length_exceeded"}}"#;
        let openai_code_only =
            r#"{"error":{"message":"Input too long.","code":"context_length_exceeded"}}"#;
        // A gateway that wraps the provider's text in its own envelope.
        let wrapped = r#"{"error":{"message":"provider error: {\"message\":\"This model's Maximum Context Length is 8192 tokens\"}","type":"provider_error"}}"#;
        for body in [
            vllm,
            vllm_new,
            vllm_prompt,
            openai,
            openai_code_only,
            wrapped,
        ] {
            assert!(is_context_length_refusal(400, body), "{body}");
        }
        // The phrase under another status is not this refusal.
        for status in [413, 422, 429, 500, 503] {
            assert!(!is_context_length_refusal(status, openai), "{status}");
        }
        // Other 400s keep failing.
        for body in [
            r#"{"error":{"message":"The model `emb` does not exist.","type":"NotFoundError","code":400}}"#,
            r#"{"error":{"message":"'input' is a required property","type":"invalid_request_error"}}"#,
            r#"{"detail":"bad request"}"#,
            "",
            "<html>Bad Request</html>",
        ] {
            assert!(!is_context_length_refusal(400, body), "{body}");
        }
    }

    #[test]
    fn a_window_is_halved_by_its_own_tokenizer() {
        let text = "alpha beta gamma delta epsilon zeta eta theta iota kappa ".repeat(60);
        let whole = embedding_split(&text, usize::MAX).unwrap_or_default();
        assert_eq!(whole.len(), 1);
        let tokens = whole[0].tokens;
        let halves = halve(&text, tokens).unwrap_or_default();
        assert!(halves.len() >= 2, "{}", halves.len());
        assert!(halves.iter().all(|h| h.tokens <= tokens.div_ceil(2)));
        let joined: String = halves.iter().map(|h| h.text.as_str()).collect();
        assert_eq!(joined, text);
    }

    #[test]
    fn the_fallback_requests_are_capped() {
        let transport = Transport::new(&super::super::TransportSettings::default())
            .unwrap_or_else(|e| panic!("{e}"));
        let settings = ModelSettings::from_llm_settings(&json!({
            "api_base": "http://127.0.0.1:9/v1",
            "api_key": "sk-test",
            "model_name": "m",
        }))
        .unwrap_or_else(|e| panic!("{e}"));
        let client = EmbeddingClient::new(transport, settings, "emb", EmbeddingOptions::default());
        // No window yet: the base allowance only.
        assert!(
            client
                .take_fallback_requests(FALLBACK_REQUESTS_BASE)
                .is_ok()
        );
        let refused = client.take_fallback_requests(1);
        assert!(
            refused
                .as_ref()
                .is_err_and(|e| e.message.contains(CTX_SETTING)),
            "{refused:?}"
        );
        // The allowance grows with the windows embedded.
        client.inner.windows.store(10, Ordering::Relaxed);
        assert!(
            client
                .take_fallback_requests(10 * FALLBACK_REQUESTS_PER_WINDOW - 1)
                .is_ok()
        );
        assert!(client.take_fallback_requests(1).is_err());
    }
}
