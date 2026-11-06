//! The browser tool contract (ADR-0048 decision 3, ADR-0049 decision 3).
//!
//! The agent sees the same browser tools whichever executor runs its thread:
//! the desktop's system-webview driver (#1589) and the cloud session's CDP
//! driver (#1594) both implement this crate's [`BrowserDriver`], and neither
//! of them defines a name, a schema, a result field or an error code of its
//! own. The crate links no browser engine and no transport:
//!
//! * [`spec`] — the tools ([`TOOLS`]): names, descriptions, JSON schemas and
//!   their [`ToolKind`] (`BrowserRead`, `BrowserNavigate`, `BrowserAct`, or
//!   the hand-over question);
//! * [`call`] — a model's arguments parsed into a typed [`BrowserCall`],
//!   refs bound to a snapshot ([`ElementRef`]), and the scheme hard limit
//!   (ADR-0048 decision 5) checked before any driver sees a URL;
//! * [`snapshot`] — the snapshot grammar, `- <role> "<name>" [ref=eN]` plus
//!   state flags, one line per node: rendering, parsing, ref numbering, the
//!   node budget, secure fields that never show a value, and ref resolution
//!   that fails with `stale_ref`;
//! * [`frame`] — the untrusted-content boundary: page text in a frame with a
//!   per-call nonce, so a page cannot close it (ADR-0043 decision 4);
//! * [`error`] — the typed errors every driver returns ([`ErrorCode`]);
//! * [`driver`] — the [`BrowserDriver`] trait, its outputs, and [`dispatch`],
//!   which turns a tool call into the one result shape both hosts return;
//! * [`rate`] — the `BrowserAct` rate limit (10 per second per pane);
//! * [`conformance`] — fixture pages, golden snapshots and the parity run
//!   a driver's own tests call ([`conformance::run`]).
#![cfg_attr(
    not(test),
    deny(
        clippy::expect_used,
        clippy::panic,
        clippy::todo,
        clippy::unimplemented,
        clippy::unwrap_used
    )
)]

pub mod call;
pub mod conformance;
pub mod driver;
pub mod error;
pub mod frame;
pub mod rate;
pub mod snapshot;
pub mod spec;

pub use call::{BrowserCall, ElementRef, SnapshotId};
pub use driver::{BrowserDriver, ControlInfo, ToolOutput, dispatch};
pub use error::{BrowserError, ErrorCode};
pub use snapshot::{Node, Snapshot};
pub use spec::{TOOLS, ToolKind, ToolSpec};
