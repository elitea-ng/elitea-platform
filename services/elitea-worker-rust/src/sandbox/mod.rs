//! Code execution transport; runtime access is exclusive to the supervisor feature.
pub(crate) mod cargo_dependency_bundle;
pub mod client;
pub(crate) mod code_platform_owner;
pub use code_platform_owner::RetainedRuntimeKind;
pub(crate) mod code_recovery;
pub mod compiled_profile_config;
pub(crate) mod compiled_snapshot;
pub mod dependency_bundle;
#[cfg(feature = "sandbox-supervisor")]
pub mod dependency_content;
#[cfg(all(test, feature = "sandbox-supervisor"))]
mod dependency_runtime_docker_tests;
pub(crate) mod dispatch;
#[cfg(feature = "sandbox-supervisor")]
pub mod docker_supervisor;
#[cfg(feature = "sandbox-supervisor")]
pub mod kubernetes;
#[cfg(feature = "sandbox-supervisor")]
pub mod ledger;
#[cfg(feature = "sandbox-supervisor")]
pub mod material;
pub mod native_bundle;
#[cfg(feature = "sandbox-supervisor")]
pub mod peer_identity;
pub(crate) mod platform_client_binding;
pub mod preparation;
#[cfg(feature = "sandbox-supervisor")]
pub mod process;
pub mod request;
#[cfg(feature = "sandbox-supervisor")]
pub mod runtime;
#[cfg(feature = "sandbox-supervisor")]
pub mod runtime_workspace;
#[cfg(feature = "sandbox-supervisor")]
pub mod service;
pub mod workspace;
