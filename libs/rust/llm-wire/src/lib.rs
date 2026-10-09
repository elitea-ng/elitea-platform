//! The platform gateway's `/llm` caller contract, without a transport.
//!
//! Two Rust programs call the `/llm` edge: the agent worker
//! (`services/elitea-worker-rust`, a tonic mTLS HTTP/2 channel, strict) and
//! the engines' model client (`libs/rust/model-client`, `reqwest`,
//! lenient). This crate holds what both must agree on, so that the edge,
//! the budget gate and the spend analytics cannot tell them apart
//! (`model-client/docs/llm-caller-contract.md`, and the fixture both suites
//! read, `conformance/llm-caller/contract.json`):
//!
//! * [`route`] — the paths under the gateway origin;
//! * [`headers`] — the header names, the bearer form, the execution-id
//!   rule, and the bounded header text rule;
//! * [`refusal`] — a refusal's stable code and retry hint from its status
//!   (and, for 402, the budget scope in its body), and the bounded,
//!   scrubbed upstream detail;
//! * [`sse`] — a bounded Server-Sent Events splitter with two dialects;
//! * [`openai`] — chat-completion chunk primitives: usage (reasoning and
//!   cached tokens), finish reason, reasoning text, and the strict chunk
//!   reader;
//! * [`tool_calls`] — streamed tool-call delta assembly with count, name,
//!   id and argument limits.
//!
//! Nothing here performs I/O, holds a credential, or formats provider text
//! into an error. Each client maps these results onto its own error
//! contract: the worker into data-free `AdkError` codes, the engines into
//! `EngineError` messages.
//!
//! # Profiles
//!
//! The two callers read the same wire with different strictness, and
//! neither may change. Where they differ the piece takes a profile:
//!
//! | Piece | Lenient (model client) | Strict (worker) |
//! | --- | --- | --- |
//! | SSE lines | `\n`, `\r\n`, lone `\r`; BOM skipped; unknown fields ignored | `\n` (a trailing `\r` dropped); only `data:`, `event:` and comments |
//! | SSE `event:` | any, last wins | once, before data, non-empty, charged to the event cap; optionally refused |
//! | Usage | absent or invalid counts read as 0 | counts must be whole numbers within a cap; `total_tokens` must equal the sum |
//! | Reasoning | a non-string field is absent | a non-string field is malformed |
//! | Tool calls | `index` optional; a reused index with a new id or name is a new call; ids default to `call_N` | `index` required and bounded; ids and names checked; no gaps; ids unique |
//!
//! # What stays in each client
//!
//! * **Request bodies.** Each client's exact bytes are asserted by its
//!   tests, and they are not the same document (the worker sends ADK
//!   contents, a merged leading instruction and image parts; the engines
//!   send `LangChain`'s shapes).
//! * **The transport, the claim token, retries and timeouts.**
//! * **The per-event state machine.** The worker refuses events after
//!   completion and `[DONE]` before it, and builds ADK responses; the model
//!   client streams text to a callback as it arrives and splits a bare
//!   `</think>` off the answer. Both build on the primitives here.
//! * **A blocking completion's parse** (model client only).
//! * **The native Anthropic stream** (worker only): it uses [`sse`] and
//!   [`refusal`], and keeps its own event grammar.

pub mod headers;
pub mod openai;
pub mod refusal;
pub mod route;
pub mod sse;
pub mod tool_calls;
