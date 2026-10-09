#![forbid(unsafe_code)]
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

pub mod agents;
#[cfg(any(target_os = "linux", target_os = "macos"))]
mod bootstrap;
use elitea_agent_runtime::bounded_yaml;
pub mod capabilities;
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub mod config;
pub mod diagnostics;
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub mod execution;
pub mod protocol;
pub mod sandbox;
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub mod security;
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub mod spool;
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub mod state;
mod toolkits;
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub mod transport;

use std::fmt;
use std::io::{self, Write};
use std::path::Path;

pub use execution::production::ProductionServeError;

#[derive(Debug)]
pub enum CliError {
    InvalidArguments,
    Serialize(serde_json::Error),
    Write(io::Error),
    Serve(ProductionServeError),
}

impl CliError {
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::InvalidArguments => "worker_cli.invalid_arguments",
            Self::Serialize(_) => "worker_cli.serialization_failed",
            Self::Write(_) => "worker_cli.output_failed",
            Self::Serve(error) => error.code(),
        }
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        match self {
            Self::Serve(error) => error.retryable(),
            Self::InvalidArguments | Self::Serialize(_) | Self::Write(_) => false,
        }
    }
}

impl fmt::Display for CliError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidArguments => formatter.write_str(concat!(
                "usage: elitea-worker-rust capabilities --json | ",
                "elitea-worker-rust serve --config <absolute-path> ",
                "--toolkit-security-config <absolute-path>"
            )),
            Self::Serialize(_) => formatter.write_str("failed to serialize capabilities"),
            Self::Write(_) => formatter.write_str("failed to write capabilities"),
            Self::Serve(error) => error.fmt(formatter),
        }
    }
}

impl std::error::Error for CliError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::InvalidArguments => None,
            Self::Serialize(error) => Some(error),
            Self::Write(error) => Some(error),
            Self::Serve(error) => Some(error),
        }
    }
}

/// Execute the deliberately small reconstruction CLI.
///
/// # Errors
///
/// Returns [`CliError::InvalidArguments`] for every command that is not yet
/// admitted, and preserves serialization or output failures without exposing
/// their internal details through its [`std::fmt::Display`] implementation.
pub async fn run<I, S, W>(arguments: I, mut output: W) -> Result<(), CliError>
where
    I: IntoIterator<Item = S>,
    S: AsRef<str>,
    W: Write,
{
    let arguments = arguments
        .into_iter()
        .map(|argument| argument.as_ref().to_owned())
        .collect::<Vec<_>>();
    match arguments.as_slice() {
        [command, format] if command == "capabilities" && format == "--json" => {
            serde_json::to_writer(&mut output, &capabilities::report())
                .map_err(CliError::Serialize)?;
            output.write_all(b"\n").map_err(CliError::Write)
        }
        [command, config_flag, deployment, policy_flag, policy]
            if command == "serve"
                && config_flag == "--config"
                && policy_flag == "--toolkit-security-config" =>
        {
            Box::pin(execution::production::serve_from_config(
                Path::new(deployment),
                Path::new(policy),
            ))
            .await
            .map_err(CliError::Serve)
        }
        _ => Err(CliError::InvalidArguments),
    }
}

#[cfg(test)]
mod tests {
    use super::{CliError, run};

    #[tokio::test]
    async fn capabilities_json_is_deterministic_and_fail_closed() {
        let mut output = Vec::new();

        run(["capabilities", "--json"], &mut output)
            .await
            .expect("capabilities command");

        assert_eq!(
            String::from_utf8(output).expect("UTF-8 output"),
            concat!(
                "{\"schema_revision\":\"elitea.rust-worker.capability-foundation.v1\",",
                "\"implementation_name\":\"elitea-worker-rust\",",
                "\"language\":\"rust\",\"runtime_version\":\"0.1.0\",",
                "\"startup_mode\":\"offline_conformance\",",
                "\"production_registration_enabled\":false,",
                "\"production_capabilities\":[]}\n"
            )
        );
    }

    #[tokio::test]
    async fn unsupported_commands_fail_without_output() {
        let mut output = Vec::new();

        let error = run(["serve"], &mut output)
            .await
            .expect_err("incomplete serve command is rejected");

        assert!(matches!(error, CliError::InvalidArguments));
        assert!(output.is_empty());
    }

    #[tokio::test]
    async fn serve_requires_two_independent_absolute_snapshots() {
        let mut output = Vec::new();

        let error = run(
            [
                "serve",
                "--config",
                "relative-runtime.json",
                "--toolkit-security-config",
                "relative-security.json",
            ],
            &mut output,
        )
        .await
        .expect_err("unsafe paths fail before dependency composition");

        assert_eq!(error.code(), "worker_serve.invalid_configuration");
        assert!(!error.retryable());
        assert!(output.is_empty());
    }
}
