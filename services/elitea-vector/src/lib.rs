//! `elitea-vector`: the platform's vector facade (ADR-0031).
//!
//! It is the only client of Qdrant. Every request carries a project-bound
//! token that elitea-main verifies ([`auth`]), or comes from an
//! administrator mTLS identity. The verified project is injected as a
//! `must` condition into every read, write, count and delete ([`scope`]).
//! The facade checks shape (dimension, keys, limits) and nothing else: it
//! does not chunk, embed or call a model.

pub mod auth;
pub mod config;
pub mod health;
pub mod identity;
pub mod layout;
pub mod scope;
pub mod service;
pub mod store;

/// The generated `elitea.vector.v1` bindings.
#[allow(clippy::all, clippy::pedantic, missing_docs, reason = "generated code")]
pub mod proto {
    tonic::include_proto!("elitea");
}

pub use proto::elitea::vector::v1 as pb;

/// Installs ring as the process's rustls crypto provider. Call it before any
/// TLS client or server is built: two providers are linked (the OTLP
/// exporter's HTTP client brings aws-lc-rs), and rustls then picks none. A
/// provider installed earlier is kept.
pub fn install_crypto_provider() {
    let _ = rustls::crypto::ring::default_provider().install_default();
}
