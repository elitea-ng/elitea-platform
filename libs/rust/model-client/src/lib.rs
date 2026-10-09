//! The model client (ADR-0026 decision 8): one small OpenAI-compatible
//! client that every later phase uses — embeddings for indexing and
//! queries, chat completions (blocking or streamed, with tool calls) for
//! the structure planner, page generation, `ask` and deep research.
//!
//! It is deliberately not an SDK: `reqwest` over rustls (the copy `gix`
//! already pulls), a bounded SSE decoder, and the request and response
//! shapes the Python engine's `LangChain` clients produced. Both provider
//! families of `llm_settings` reach the model through the platform
//! gateway's OpenAI-compatible surface (owner decision 4).
//!
//! * [`settings`] — `llm_settings` and `embedding_model`, strictly parsed;
//! * [`transport`] — the HTTP client, retries, timeouts, error mapping;
//! * [`embeddings`] — batched embeddings with dimension discovery;
//! * [`chat`] — chat completions and tool calls;
//! * [`sse`] — the bounded event-stream decoder;
//! * [`tokens`] — token counting with the embedded `o200k_base` BPE.
//!
//! The API key lives in a [`elitea_engine_core::secret::Secret`]: it is sent in
//! the `Authorization` header, marked sensitive, and never formatted into
//! an error or a log line.

pub mod chat;
pub mod embeddings;
pub mod settings;
pub mod sse;
pub mod tokens;
pub mod transport;

pub use chat::{
    ChatClient, ChatMessage, ChatRequest, ChatResponse, Sampling, SystemPrompt, ToolCall,
    ToolChoice, ToolDefinition, Usage,
};
pub use embeddings::{EmbeddingClient, EmbeddingOptions};
pub use settings::{
    ModelSettings, Provider, ReasoningEffort, embedding_model_name, valid_execution_id,
};
pub use tokens::count_tokens;
pub use transport::{
    BudgetScope, EXECUTION_HEADER, PROJECT_HEADER, Timeouts, Transport, TransportSettings,
    budget_scope, refusal_code, retryable,
};
