//! The temporary D0 assembler (ADR-0029 delivery phase D0): the first
//! runnable LOCAL agent turn, isolated in this one module.
//!
//! It composes the shared runtime's host interface
//! (`elitea_agent_runtime::host`) from desktop adapters and runs one
//! ordinary agent turn on the patched adk 2.2. Extraction stage 7 moves
//! assembly into the runtime; this module is then deleted and the adapters
//! (`model`, `remote_tools`, `approvals`, `events`) are what the desktop
//! host keeps.
//!
//! * [`api`] — the platform operations (resolved definition, local turn
//!   start/commit, remote toolkit call) over HTTPS with the native token;
//! * [`cache`] — the session's in-memory caches of the agent versions and
//!   the conversations' answering participants (a warm turn's one round trip);
//! * [`catalogue`] — the project's chat models a turn's chosen model is
//!   checked against, and the family flag of reasoning replay (#1228);
//! * [`commits`] — a turn's commit after its `done`, one FIFO lane per
//!   conversation, retried with backoff;
//! * [`definition`] — what D0 runs, and the refusals for what it does not;
//! * [`model`] — `ModelTransport` over `/llm` (llm-wire + reqwest);
//! * [`remote_tools`] — `RemoteToolkit` tools (decision 3);
//! * [`approvals`] — the person's prompt, bridged to the UI over IPC;
//! * [`tools`] — the per-tool events, steps and before-images;
//! * [`recorder`] — what the commit and the changed-files card report;
//! * [`events`] — the `agent://event` stream;
//! * [`mentions`] — the "@" file picker and a turn's referenced paths;
//! * [`skills`] — a skill picked with "/", applied to one turn;
//! * [`slash_commands`] — a custom command picked with "/", expanded and
//!   framed as the person's message;
//! * [`framing`] — framing text the instructions carry but do not author;
//! * [`environment`] — the host's `## Environment` section of the prompt;
//! * [`session`] — a turn's session: reopened from `sessions.sqlite`, seeded
//!   from the server conversation when it has nothing (the execution
//!   request's `chat_history` stays empty: the session carries the history);
//! * [`context`] — the model's context limits, the step compaction's budget,
//!   marker and meter (#1217);
//! * [`turn`] — the assembler and the host's turn table.

pub mod api;
pub mod approvals;
pub mod cache;
pub mod catalogue;
pub mod commits;
pub mod context;
pub mod definition;
pub mod environment;
pub mod events;
pub mod framing;
pub mod mentions;
pub mod model;
pub mod recorder;
pub mod remote_tools;
pub mod session;
pub mod skills;
pub mod slash_commands;
pub mod tools;
pub mod turn;

#[cfg(test)]
pub(crate) mod tests;
