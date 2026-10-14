//! Embeddings over the Elitea `/llm` gateway, on the model facade's channel.
//!
//! The indexing runtime (ADR-0030 I1) embeds chunks and queries through the
//! same origin-bound mTLS HTTP/2 channel, the same claim-scoped PAT and the
//! same caller contract (`elitea-llm-wire`) as the agent's chat completions:
//! `POST /llm/v1/embeddings` with the project selector and the execution id,
//! answered as the `OpenAI` embeddings object. Nothing here adds a transport.
//!
//! Every failure is a stable, data-free code: the gateway's own refusal
//! codes from the status, or one of this module's shape codes.

use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use elitea_llm_wire::refusal::Refusal;
use serde::Deserialize;

use super::gateway_call::{CallError, build_request, send};
use super::openai_compatible_facade::{ModelGatewayClient, ModelGatewayTransport};
use super::runtime_context::ClaimScopedEliteaContext;

const EMBEDDINGS_ROUTE: &str = elitea_llm_wire::route::EMBEDDINGS_ROUTE;
/// The most inputs one request carries (`LangChain`'s default batch).
pub(crate) const MAX_BATCH_INPUTS: usize = 64;
/// The most bytes of one request body.
const MAX_REQUEST_BYTES: usize = 8 * 1024 * 1024;
/// A response body cap: 64 inputs of 4096 dimensions as JSON floats is about
/// 6 MiB, so this leaves room without being unbounded.
const MAX_RESPONSE_BYTES: usize = 32 * 1024 * 1024;
/// The most dimensions a vector may have (elitea-vector's `MAX_DIMENSION`).
const MAX_DIMENSION: usize = 65_536;

/// A stable, data-free embeddings failure.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) enum EmbeddingsError {
    /// The gateway refused the call; `code` is the caller contract's refusal
    /// code (`elitea_llm_wire::refusal::Refusal::code`).
    Refused { code: &'static str, retryable: bool },
    /// The channel could not carry the request or the response.
    Transport,
    /// The response is not the embeddings object, or its vectors disagree
    /// in length or count with the request.
    InvalidResponse(&'static str),
    /// A request would exceed the request or response bound.
    TooLarge,
}

impl fmt::Display for EmbeddingsError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Refused { code, .. } => write!(formatter, "the model gateway refused: {code}"),
            Self::Transport => formatter.write_str("the model gateway transport failed"),
            Self::InvalidResponse(reason) => {
                write!(formatter, "the embeddings response is malformed: {reason}")
            }
            Self::TooLarge => formatter.write_str("the embeddings request exceeds its bound"),
        }
    }
}

impl std::error::Error for EmbeddingsError {}

impl EmbeddingsError {
    /// Whether another attempt could succeed.
    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        match self {
            Self::Refused { retryable, .. } => *retryable,
            Self::Transport => true,
            Self::InvalidResponse(_) | Self::TooLarge => false,
        }
    }
}

/// One answered batch.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct Embedded {
    /// One vector per input, in input order.
    pub(crate) vectors: Vec<Vec<f32>>,
    /// The model the gateway says produced them (may differ in spelling
    /// from the requested name; the requested name is what the index stamps).
    pub(crate) model: String,
}

/// The gateway's embeddings route, bound to the shared channel.
#[derive(Clone)]
pub(crate) struct EmbeddingsClient {
    transport: Arc<dyn ModelGatewayTransport>,
    response_timeout: Duration,
}

impl fmt::Debug for EmbeddingsClient {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("EmbeddingsClient")
            .finish_non_exhaustive()
    }
}

#[derive(Deserialize)]
struct EmbeddingsResponse {
    data: Vec<EmbeddingItem>,
    #[serde(default)]
    model: String,
}

#[derive(Deserialize)]
struct EmbeddingItem {
    embedding: Vec<f32>,
    #[serde(default)]
    index: Option<usize>,
}

impl EmbeddingsClient {
    /// The embeddings route over `gateway`'s channel and timeouts.
    #[must_use]
    pub(crate) fn from_gateway(gateway: &ModelGatewayClient) -> Self {
        Self {
            transport: gateway.transport.clone(),
            response_timeout: gateway.config.response_header_timeout
                + gateway.config.stream_idle_timeout,
        }
    }

    #[cfg(test)]
    pub(crate) fn with_transport(
        transport: Arc<dyn ModelGatewayTransport>,
        response_timeout: Duration,
    ) -> Self {
        Self {
            transport,
            response_timeout,
        }
    }

    /// Embeds `inputs` (1 to [`MAX_BATCH_INPUTS`]) with `model` for the
    /// claim `context` was redeemed for. The vectors come back in input
    /// order and all of one length.
    ///
    /// # Errors
    ///
    /// A gateway refusal, a transport failure, a malformed response, or a
    /// request over the size bound.
    pub(crate) async fn embed(
        &self,
        context: &ClaimScopedEliteaContext,
        model: &str,
        inputs: &[String],
    ) -> Result<Embedded, EmbeddingsError> {
        if inputs.is_empty() || inputs.len() > MAX_BATCH_INPUTS || model.is_empty() {
            return Err(EmbeddingsError::InvalidResponse("the request is malformed"));
        }
        let body = serde_json::to_vec(&serde_json::json!({
            "model": model,
            "input": inputs,
            "encoding_format": "float",
        }))
        .map_err(|_| EmbeddingsError::TooLarge)?;
        if body.len() > MAX_REQUEST_BYTES {
            return Err(EmbeddingsError::TooLarge);
        }
        let token = context.model_facade_token();
        let request = build_request(
            EMBEDDINGS_ROUTE,
            "application/json",
            Bytes::from(body),
            context.resource_project_id(),
            &token,
            context.execution_id(),
        )
        .map_err(|_| EmbeddingsError::InvalidResponse("the request is malformed"))?;
        let collected = send(
            &self.transport,
            request,
            self.response_timeout,
            MAX_RESPONSE_BYTES,
        )
        .await
        .map_err(embeddings_error)?;
        parse_response(&collected, inputs.len())
    }
}

/// A shared gateway-call failure as the embeddings caller reads it.
fn embeddings_error(error: CallError) -> EmbeddingsError {
    match error {
        CallError::Refused(refusal) => {
            let retryable = matches!(
                refusal,
                Refusal::UpstreamTimeout
                    | Refusal::RateLimited
                    | Refusal::Conflict
                    | Refusal::Unavailable
            );
            EmbeddingsError::Refused {
                code: refusal.code(),
                retryable,
            }
        }
        CallError::Transport => EmbeddingsError::Transport,
        CallError::TooLarge => EmbeddingsError::TooLarge,
        CallError::Malformed => EmbeddingsError::InvalidResponse("the request is malformed"),
    }
}

/// The embeddings object, checked for count, order and one dimension.
fn parse_response(raw: &[u8], expected: usize) -> Result<Embedded, EmbeddingsError> {
    let parsed: EmbeddingsResponse = serde_json::from_slice(raw)
        .map_err(|_| EmbeddingsError::InvalidResponse("not the embeddings object"))?;
    if parsed.data.len() != expected {
        return Err(EmbeddingsError::InvalidResponse(
            "the vector count differs from the input count",
        ));
    }
    let mut ordered: Vec<Option<Vec<f32>>> = vec![None; expected];
    for (position, item) in parsed.data.into_iter().enumerate() {
        let index = item.index.unwrap_or(position);
        if index >= expected || ordered[index].is_some() {
            return Err(EmbeddingsError::InvalidResponse(
                "the vector indexes are not a permutation of the inputs",
            ));
        }
        if item.embedding.is_empty()
            || item.embedding.len() > MAX_DIMENSION
            || item.embedding.iter().any(|value| !value.is_finite())
        {
            return Err(EmbeddingsError::InvalidResponse(
                "a vector is empty, too long or not finite",
            ));
        }
        ordered[index] = Some(item.embedding);
    }
    let vectors: Vec<Vec<f32>> = ordered.into_iter().flatten().collect();
    if vectors.len() != expected {
        return Err(EmbeddingsError::InvalidResponse(
            "the vector indexes are not a permutation of the inputs",
        ));
    }
    let dimension = vectors[0].len();
    if vectors.iter().any(|vector| vector.len() != dimension) {
        return Err(EmbeddingsError::InvalidResponse(
            "the vectors do not share one dimension",
        ));
    }
    Ok(Embedded {
        vectors,
        model: parsed.model,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_response_is_reordered_by_index_and_checked() {
        let raw = br#"{"object":"list","model":"m","data":[
            {"object":"embedding","index":1,"embedding":[0.5,0.5]},
            {"object":"embedding","index":0,"embedding":[1.0,0.0]}],
            "usage":{"prompt_tokens":2,"total_tokens":2}}"#;
        let embedded = parse_response(raw, 2).expect("parses");
        assert_eq!(embedded.vectors, vec![vec![1.0, 0.0], vec![0.5, 0.5]]);
        assert_eq!(embedded.model, "m");
        assert_eq!(
            parse_response(raw, 3),
            Err(EmbeddingsError::InvalidResponse(
                "the vector count differs from the input count"
            ))
        );
        let ragged = br#"{"data":[{"embedding":[1.0]},{"embedding":[1.0,2.0]}]}"#;
        assert_eq!(
            parse_response(ragged, 2),
            Err(EmbeddingsError::InvalidResponse(
                "the vectors do not share one dimension"
            ))
        );
        let duplicate =
            br#"{"data":[{"index":0,"embedding":[1.0]},{"index":0,"embedding":[2.0]}]}"#;
        assert!(parse_response(duplicate, 2).is_err());
        assert!(parse_response(b"not json", 1).is_err());
    }
}
