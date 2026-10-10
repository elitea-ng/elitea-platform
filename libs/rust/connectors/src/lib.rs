//! One HTTP client per source provider (ADR-0027 decision 8, ADR-0030
//! decision 3).
//!
//! The agent-runtime toolkit families (#1207) and the indexing connectors
//! share these clients: the families keep their tool layer (schemas,
//! rendering, SDK-shaped output) and call the clients here; the connectors
//! list and fetch documents through the same clients.
//!
//! * [`transport`] — the request, the streamed response and the
//!   [`transport::Transport`] every client sends through. The crate pins no
//!   TLS stack: each binary supplies its reqwest through [`reqwest012`] or
//!   [`reqwest013`], and a test supplies fixtures.
//! * [`base_url`] — the one strict base-URL rule.
//! * [`egress`] — the shared, fail-closed host allowlist and the guard
//!   transport that applies it to every request.
//! * per provider (feature `providers`): its configuration (secrets
//!   zeroized), its errors (data-free), its wire layer and its request
//!   building and authentication — [`ado`], [`bitbucket`], [`github`], [`gitlab`];
//!   and [`artifact`], the platform's own object API.
//! * `ContentSource` connectors over those clients (feature
//!   `content-source`): [`source`] holds what they share (egress guard,
//!   bounded 429 backoff, listing and fetch caps); each provider module holds
//!   its own `source`.

// The provider modules were crate-private in agent-runtime and are public
// here only so the families can call them: their accessors are plain getters
// and every error is a data-free code documented on its type.
#[cfg(feature = "providers")]
#[allow(clippy::must_use_candidate, clippy::missing_errors_doc)]
pub mod ado;
#[cfg(feature = "providers")]
pub mod artifact;
pub mod base_url;
#[cfg(feature = "providers")]
#[allow(clippy::must_use_candidate, clippy::missing_errors_doc)]
pub mod bitbucket;
pub mod egress;
mod git_id;
#[cfg(feature = "providers")]
#[allow(clippy::must_use_candidate, clippy::missing_errors_doc)]
pub mod github;
#[cfg(feature = "providers")]
#[allow(clippy::must_use_candidate, clippy::missing_errors_doc)]
pub mod gitlab;
mod reqwest_adapter;
#[cfg(feature = "content-source")]
pub mod source;
pub mod transport;

pub use reqwest_adapter::{BuildError, ClientPolicy};

/// The [`transport::Transport`] over a reqwest 0.12 client.
#[cfg(feature = "reqwest-012")]
pub mod reqwest012 {
    crate::reqwest_adapter::reqwest_adapter!(reqwest012);
}

/// The [`transport::Transport`] over a reqwest 0.13 client.
#[cfg(feature = "reqwest-013")]
pub mod reqwest013 {
    crate::reqwest_adapter::reqwest_adapter!(reqwest013);
}
