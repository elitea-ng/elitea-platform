//! Process-wide diagnostic safety boundaries.

pub(crate) mod failure;
#[cfg(test)]
mod runtime_log_safety_tests;

use std::fmt;
use std::io::{self, Write};
use std::sync::Once;
use std::time::Duration;

use opentelemetry::global;
use opentelemetry::propagation::{Extractor, TextMapPropagator as _};
use opentelemetry::trace::{TraceContextExt as _, TracerProvider as _};
use opentelemetry_otlp::{Protocol, WithExportConfig as _, WithHttpConfig as _};
use opentelemetry_sdk::Resource;
use opentelemetry_sdk::propagation::TraceContextPropagator;
use opentelemetry_sdk::runtime;
use opentelemetry_sdk::trace::SdkTracerProvider;
use opentelemetry_sdk::trace::span_processor_with_async_runtime::BatchSpanProcessor;
use tracing::Span;
use tracing_opentelemetry::OpenTelemetrySpanExt as _;
use tracing_subscriber::EnvFilter;
use tracing_subscriber::Layer as _;
use tracing_subscriber::fmt::MakeWriter;
use tracing_subscriber::fmt::format::FmtSpan;
use tracing_subscriber::layer::SubscriberExt as _;
use tracing_subscriber::registry::LookupSpan;
use tracing_subscriber::util::SubscriberInitExt as _;

static PANIC_HOOK: Once = Once::new();
const LOG_LEVEL_ENVIRONMENT: &str = "ELITEA_RUST_LOG";
const TRACE_LEVEL_ENVIRONMENT: &str = "ELITEA_RUST_TRACE";
const OTEL_DISABLED_ENVIRONMENT: &str = "OTEL_SDK_DISABLED";
const OTEL_ENDPOINT_ENVIRONMENT: &str = "OTEL_EXPORTER_OTLP_ENDPOINT";
const OTEL_TRACES_ENDPOINT_ENVIRONMENT: &str = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT";
const OTEL_SERVICE_NAME: &str = "elitea-worker-rust";
const OTEL_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(5);
/// Elitea-owned crates whose events are reviewed for field safety. The Worker
/// and the reusable runtime it hosts (ADR-0027) share one level; every other
/// crate stays off.
pub(crate) const OWNED_TARGETS: [&str; 2] = ["elitea_worker_rust", "elitea_agent_runtime"];

/// Process-level tracing setup failures with no environment contents exposed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DiagnosticInitError {
    CryptoProviderUnavailable,
    InvalidLogLevel,
    InvalidTraceLevel,
    InvalidFailureDiagnostics,
    ExporterUnavailable,
    SubscriberUnavailable,
}

impl fmt::Display for DiagnosticInitError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::CryptoProviderUnavailable => {
                formatter.write_str("the process TLS crypto provider could not be installed")
            }
            Self::InvalidLogLevel => formatter
                .write_str("ELITEA_RUST_LOG must be off, error, warn, info, debug, or trace"),
            Self::InvalidTraceLevel => formatter
                .write_str("ELITEA_RUST_TRACE must be off, error, warn, info, debug, or trace"),
            Self::InvalidFailureDiagnostics => {
                formatter.write_str("ELITEA_RUST_FAILURE_DIAGNOSTICS must be off or on")
            }
            Self::ExporterUnavailable => {
                formatter.write_str("the OpenTelemetry trace exporter could not be configured")
            }
            Self::SubscriberUnavailable => {
                formatter.write_str("the structured tracing subscriber could not be installed")
            }
        }
    }
}

impl std::error::Error for DiagnosticInitError {}

/// Select the process-wide rustls crypto provider before any HTTP client is
/// constructed.
///
/// The worker deliberately uses ring for its direct TLS clients. Some
/// dependencies also enable the AWS-LC rustls feature, so leaving provider
/// selection implicit makes `reqwest::Client::new` panic when OTLP is enabled.
/// Calling this function before diagnostics construction keeps startup
/// deterministic. A provider installed earlier by the embedding process is
/// retained.
///
/// # Errors
///
/// Returns [`DiagnosticInitError::CryptoProviderUnavailable`] only when no
/// provider is available after the installation attempt.
pub fn install_tls_crypto_provider() -> Result<(), DiagnosticInitError> {
    if rustls::crypto::CryptoProvider::get_default().is_some() {
        return Ok(());
    }
    let _installation = rustls::crypto::ring::default_provider().install_default();
    if rustls::crypto::CryptoProvider::get_default().is_some() {
        Ok(())
    } else {
        Err(DiagnosticInitError::CryptoProviderUnavailable)
    }
}

/// Process-owned exporter lifetime. The provider must outlive every worker
/// span and receive an explicit bounded shutdown before process exit.
pub struct DiagnosticGuard {
    provider: Option<SdkTracerProvider>,
}

impl DiagnosticGuard {
    /// Flush and stop the optional exporter without changing worker outcome.
    ///
    /// # Errors
    ///
    /// Returns a static error when the exporter cannot flush inside its own
    /// bounded shutdown policy.
    pub fn shutdown(&mut self) -> Result<(), DiagnosticShutdownError> {
        let Some(provider) = self.provider.take() else {
            return Ok(());
        };
        provider
            .shutdown_with_timeout(OTEL_SHUTDOWN_TIMEOUT)
            .map_err(|_| DiagnosticShutdownError)
    }
}

impl Drop for DiagnosticGuard {
    fn drop(&mut self) {
        let _ignored = self.shutdown();
    }
}

/// Redacted exporter-shutdown failure.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DiagnosticShutdownError;

impl fmt::Display for DiagnosticShutdownError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the OpenTelemetry exporter did not shut down cleanly")
    }
}

impl std::error::Error for DiagnosticShutdownError {}

/// Install the process subscriber for safe Elitea-owned spans.
///
/// Only a single level is accepted and it is applied exclusively to the
/// Elitea-owned crates in [`OWNED_TARGETS`]. Arbitrary `RUST_LOG` directives are deliberately ignored so enabling
/// local diagnostics cannot expose dependency-owned HTTP, model, SMTP or SQL
/// fields. Span close events supply phase duration without logging payloads.
///
/// # Errors
///
/// Returns [`DiagnosticInitError::InvalidLogLevel`] for an unsupported log
/// level and [`DiagnosticInitError::SubscriberUnavailable`] if another global
/// subscriber is already installed or process-level installation fails.
pub fn install_tracing_subscriber() -> Result<DiagnosticGuard, DiagnosticInitError> {
    let failure_setting = std::env::var("ELITEA_RUST_FAILURE_DIAGNOSTICS").ok();
    let capture_failures = failure::configured(failure_setting.as_deref())?;
    let configured = std::env::var(LOG_LEVEL_ENVIRONMENT).ok();
    let format = log_layer(configured.as_deref(), io::stdout)?;
    let provider = build_trace_provider()?;
    match provider.as_ref() {
        Some(provider) => {
            let configured = std::env::var(TRACE_LEVEL_ENVIRONMENT).ok();
            let directive = trace_directive(configured.as_deref())?;
            let trace_filter = EnvFilter::try_new(directive)
                .map_err(|_| DiagnosticInitError::InvalidTraceLevel)?;
            let tracer = provider.tracer(OTEL_SERVICE_NAME);
            let telemetry = tracing_opentelemetry::layer()
                .with_tracer(tracer)
                .with_filter(trace_filter);
            tracing_subscriber::registry()
                .with(format)
                .with(telemetry)
                .try_init()
                .map_err(|_| DiagnosticInitError::SubscriberUnavailable)?;
            global::set_tracer_provider(provider.clone());
        }
        None => tracing_subscriber::registry()
            .with(format)
            .try_init()
            .map_err(|_| DiagnosticInitError::SubscriberUnavailable)?,
    }
    failure::enable(capture_failures);
    Ok(DiagnosticGuard { provider })
}

/// The process log layer: compact, uncoloured, owned crates only.
fn log_layer<S, W>(
    configured: Option<&str>,
    writer: W,
) -> Result<impl tracing_subscriber::Layer<S> + Send + Sync, DiagnosticInitError>
where
    S: tracing::Subscriber + for<'span> LookupSpan<'span>,
    W: for<'writer> MakeWriter<'writer> + Send + Sync + 'static,
{
    let directive = tracing_directive(configured)?;
    let filter = EnvFilter::try_new(directive).map_err(|_| DiagnosticInitError::InvalidLogLevel)?;
    Ok(tracing_subscriber::fmt::layer()
        .compact()
        .with_ansi(false)
        .with_target(true)
        .with_thread_ids(true)
        .with_span_events(FmtSpan::CLOSE)
        .with_writer(writer)
        .with_filter(filter))
}

fn tracing_directive(configured: Option<&str>) -> Result<String, DiagnosticInitError> {
    level_directive(configured, DiagnosticInitError::InvalidLogLevel)
}

fn trace_directive(configured: Option<&str>) -> Result<String, DiagnosticInitError> {
    level_directive(configured, DiagnosticInitError::InvalidTraceLevel)
}

fn level_directive(
    configured: Option<&str>,
    invalid: DiagnosticInitError,
) -> Result<String, DiagnosticInitError> {
    let level = configured.unwrap_or("info").trim().to_ascii_lowercase();
    if !matches!(
        level.as_str(),
        "off" | "error" | "warn" | "info" | "debug" | "trace"
    ) {
        return Err(invalid);
    }
    Ok(OWNED_TARGETS
        .iter()
        .map(|target| format!("{target}={level}"))
        .collect::<Vec<_>>()
        .join(","))
}

fn build_trace_provider() -> Result<Option<SdkTracerProvider>, DiagnosticInitError> {
    let disabled = std::env::var(OTEL_DISABLED_ENVIRONMENT).ok();
    let traces_endpoint = std::env::var(OTEL_TRACES_ENDPOINT_ENVIRONMENT).ok();
    let endpoint = std::env::var(OTEL_ENDPOINT_ENVIRONMENT).ok();
    if !trace_export_enabled(
        disabled.as_deref(),
        traces_endpoint.as_deref(),
        endpoint.as_deref(),
    ) {
        return Ok(None);
    }
    // Do not let the exporter construct its implicit reqwest client. Its
    // fallback path calls `Client::new()` after a builder error, which panics
    // when the process trust-store path is temporarily unavailable. The
    // worker owns startup failure classification and must keep it fallible.
    let http_client = reqwest_mcp::Client::builder()
        .build()
        .map_err(|_| DiagnosticInitError::ExporterUnavailable)?;
    let exporter = opentelemetry_otlp::SpanExporter::builder()
        .with_http()
        .with_http_client(http_client)
        .with_protocol(Protocol::HttpBinary)
        .build()
        .map_err(|_| DiagnosticInitError::ExporterUnavailable)?;
    let resource = Resource::builder()
        .with_service_name(OTEL_SERVICE_NAME)
        .build();
    let batch = BatchSpanProcessor::builder(exporter, runtime::Tokio).build();
    Ok(Some(
        SdkTracerProvider::builder()
            .with_span_processor(batch)
            .with_resource(resource)
            .build(),
    ))
}

fn trace_export_enabled(
    disabled: Option<&str>,
    traces_endpoint: Option<&str>,
    endpoint: Option<&str>,
) -> bool {
    if disabled.is_some_and(|value| value.trim().eq_ignore_ascii_case("true")) {
        return false;
    }
    [traces_endpoint, endpoint]
        .into_iter()
        .flatten()
        .any(|value| !value.trim().is_empty())
}

/// Attach a validated W3C command trace to a newly created execution span.
/// Header contents are never recorded. Invalid or absent context starts a new
/// local trace and returns `false` for a safe diagnostic field.
#[must_use]
pub fn attach_command_trace_parent(span: &Span, traceparent: &str, tracestate: &str) -> bool {
    if traceparent.is_empty() {
        return false;
    }
    let propagator = TraceContextPropagator::new();
    let context = propagator.extract(&CommandTraceHeaders {
        traceparent,
        tracestate,
    });
    if !context.span().span_context().is_valid() {
        return false;
    }
    span.set_parent(context).is_ok()
}

struct CommandTraceHeaders<'a> {
    traceparent: &'a str,
    tracestate: &'a str,
}

impl Extractor for CommandTraceHeaders<'_> {
    fn get(&self, key: &str) -> Option<&str> {
        match key {
            "traceparent" => Some(self.traceparent),
            "tracestate" if !self.tracestate.is_empty() => Some(self.tracestate),
            _ => None,
        }
    }

    fn keys(&self) -> Vec<&str> {
        if self.tracestate.is_empty() {
            vec!["traceparent"]
        } else {
            vec!["traceparent", "tracestate"]
        }
    }
}

/// Install a static panic hook before worker tasks or external adapters run.
///
/// Rust's default hook prints the arbitrary panic payload and source location.
/// Provider, model, or tool code can panic with request data, so the worker
/// replaces that hook instead of treating a redacted task error as sufficient.
/// Repeated calls are harmless.
pub fn install_redacted_panic_hook() {
    PANIC_HOOK.call_once(|| {
        std::panic::set_hook(Box::new(|information| {
            let mut stderr = io::stderr().lock();
            match information.location().and_then(|location| {
                panic_source_label(location.file()).map(|source| (source, location.line()))
            }) {
                Some((source, line)) => {
                    let _ignored = writeln!(
                        stderr,
                        "elitea worker task panicked; payload redacted; source={source}:{line}"
                    );
                }
                None => {
                    let _ignored =
                        stderr.write_all(b"elitea worker task panicked; details redacted\n");
                }
            }
        }));
    });
}

fn panic_source_label(file: &str) -> Option<&str> {
    let label = file.rsplit(['/', '\\']).next()?;
    (!label.is_empty()
        && label.len() <= 80
        && label
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-')))
    .then_some(label)
}

#[cfg(test)]
mod tests {
    use super::{
        DiagnosticInitError, install_tls_crypto_provider, log_layer, panic_source_label,
        trace_directive, trace_export_enabled, tracing_directive,
    };
    use tracing_subscriber::layer::SubscriberExt as _;
    use tracing_subscriber::util::SubscriberInitExt as _;

    #[test]
    fn explicit_tls_provider_supports_reqwest_after_feature_unification() {
        install_tls_crypto_provider().expect("install TLS crypto provider");

        reqwest_mcp::Client::builder()
            .build()
            .expect("build reqwest client with the selected provider");
    }

    #[test]
    fn tracing_level_is_crate_scoped_and_rejects_directives() {
        assert_eq!(
            tracing_directive(None).expect("default tracing directive"),
            "elitea_worker_rust=info,elitea_agent_runtime=info"
        );
        assert_eq!(
            tracing_directive(Some(" DEBUG ")).expect("debug tracing directive"),
            "elitea_worker_rust=debug,elitea_agent_runtime=debug"
        );
        assert_eq!(
            trace_directive(Some("warn")).expect("warn trace directive"),
            "elitea_worker_rust=warn,elitea_agent_runtime=warn"
        );
        assert_eq!(
            tracing_directive(Some("trace,hyper=trace")),
            Err(DiagnosticInitError::InvalidLogLevel)
        );
        assert_eq!(
            trace_directive(Some("trace,hyper=trace")),
            Err(DiagnosticInitError::InvalidTraceLevel)
        );
    }

    const FILTER_CHILD_ENV: &str = "ELITEA_DIAGNOSTICS_FILTER_CHILD";

    /// Runtime code moved into `elitea_agent_runtime` (ADR-0027) must stay
    /// visible at the configured level while dependencies stay off. A child
    /// process owns the global subscriber, so parallel tests cannot change
    /// callsite interest and turn a missing line into a silent pass.
    #[test]
    fn log_filter_admits_owned_crates_at_the_configured_level_only() {
        let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
            .args([
                "--exact",
                "diagnostics::tests::log_filter_child",
                "--nocapture",
                "--test-threads=1",
            ])
            .env(FILTER_CHILD_ENV, "1")
            .output()
            .expect("log filter child process");
        let logged = String::from_utf8_lossy(&output.stdout);
        assert!(
            output.status.success(),
            "{logged}\n{}",
            String::from_utf8_lossy(&output.stderr)
        );
        assert!(logged.contains("CHILD-RAN"), "{logged}");
        assert!(logged.contains("runtime-warn-visible"), "{logged}");
        assert!(logged.contains("worker-warn-visible"), "{logged}");
        for hidden in [
            "runtime-info-hidden",
            "worker-info-hidden",
            "dependency-warn-hidden",
            "dependency-error-hidden",
        ] {
            assert!(!logged.contains(hidden), "{hidden} leaked: {logged}");
        }
    }

    #[test]
    fn log_filter_child() {
        if std::env::var(FILTER_CHILD_ENV).is_err() {
            return;
        }
        tracing_subscriber::registry()
            .with(log_layer(Some("warn"), std::io::stdout).expect("production log layer"))
            .init();
        tracing::warn!(target: "elitea_agent_runtime::graph::state_reducers", "runtime-warn-visible");
        tracing::info!(target: "elitea_agent_runtime::graph::router", "runtime-info-hidden");
        tracing::warn!(target: "elitea_worker_rust::runner", "worker-warn-visible");
        tracing::info!(target: "elitea_worker_rust::runner", "worker-info-hidden");
        tracing::warn!(target: "hyper::proto", "dependency-warn-hidden");
        tracing::error!(target: "sqlx::query", "dependency-error-hidden");
        println!("CHILD-RAN");
    }

    #[test]
    fn trace_export_requires_an_endpoint_and_honors_standard_disable() {
        assert!(!trace_export_enabled(None, None, None));
        assert!(trace_export_enabled(
            None,
            None,
            Some("http://collector:4318")
        ));
        assert!(trace_export_enabled(
            None,
            Some("http://collector:4318/v1/traces"),
            None,
        ));
        assert!(!trace_export_enabled(
            Some(" TRUE "),
            Some("http://collector:4318/v1/traces"),
            None,
        ));
    }

    #[test]
    fn panic_location_keeps_only_a_safe_source_label() {
        assert_eq!(
            panic_source_label("/private/build/secret/runtime.rs"),
            Some("runtime.rs")
        );
        assert_eq!(
            panic_source_label(r"C:\private\build\task-core.rs"),
            Some("task-core.rs")
        );
        assert_eq!(panic_source_label("unsafe source.rs"), None);
    }
}
