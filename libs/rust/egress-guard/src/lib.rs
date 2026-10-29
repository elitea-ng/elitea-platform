//! The Rust twin of the platform's Go egress guard (#1238).
//!
//! The agent worker's `web_fetch` dials a URL the model chose, and worker
//! pods have unrestricted egress, so the worker needs the same SSRF rule
//! elitea-main applies (`libs/go/egresslib/addrcheck`):
//!
//! - [`classify`] decides one address: public, private (liftable by an
//!   allowlist entry naming it) or forbidden (never dialled: metadata,
//!   link-local, multicast, reserved). NAT64, 6to4 and IPv4-mapped forms are
//!   judged as the IPv4 address they reach.
//! - [`Allowlist`] is the egresslib grammar (`host`, `host:port`, `*.domain`,
//!   CIDR), fail-closed: a malformed entry is an error, never a skip.
//! - [`PublicPolicy`] narrows the public hosts a run may reach (ADR-0044
//!   decision 2, #1490): `Open` admits any host, `Allowlist` only the hosts
//!   its entries name. It never widens the SSRF rule.
//! - [`GuardedResolver`] is a `reqwest` resolver that resolves a name,
//!   classifies EVERY address and returns only the admitted ones, so the
//!   connection goes to an address that was checked. A name that later
//!   resolves elsewhere (DNS rebinding) changes nothing: the connector never
//!   resolves on its own.
//!
//! The Go and Rust classifiers are held identical by one vector file,
//! `libs/go/egresslib/testdata/classify_vectors.json`, which both test suites
//! read.

mod allowlist;
mod classify;
mod policy;
mod resolver;

pub use allowlist::{Allowlist, AllowlistError, PermitSet};
pub use classify::{Class, canonical, classify};
pub use policy::{MAX_PUBLIC_ALLOWLIST_ENTRIES, PublicPolicy, PublicPolicyError};
pub use resolver::{
    GuardedResolver, Lookup, LookupFuture, MAX_RESOLVED_ADDRESSES, ResolveError, SystemLookup,
    filter, resolve_checked, resolve_checked_with,
};
