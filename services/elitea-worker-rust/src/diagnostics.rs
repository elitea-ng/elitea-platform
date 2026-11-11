//! Process-wide diagnostic safety boundaries.
//!
//! # The operator stream (FU-299)
//!
//! stdout carries the operator stream: JSON lines, one object per tracing
//! EVENT (`elitea_faults::OperatorJsonLayer`), never a span open or close
//! line. Each object has `time` (RFC 3339, UTC), `level`, `msg`, `target`,
//! `span` (the innermost span's name), the event's fields, then every field of
//! the spans in scope, inner span first (`execution_id`, `support_ref`,
//! `tenant_id`, the project ids, `command_id`, …). `ELITEA_RUST_LOG` sets the
//! one level for the crates in `OWNED_TARGETS`, as before.
//!
//! Span timing (`close time.busy=… time.idle=…`) goes to OpenTelemetry only.
//! For local performance evidence start the process with
//! `ELITEA_RUST_SPAN_TIMING=stderr`: a compact text layer then writes the span
//! close lines, and only those, to stderr at the same level. Any other value,
//! or none, leaves it off.
//!
//! A panic always writes one redacted line to stderr (source file and line,
//! never the payload), even at `ELITEA_RUST_LOG=off`. When ERROR is enabled it
//! also logs one ERROR line with code `internal.panic`: the payload's type
//! (never its text), the source file and line, the span fields, and a
//! backtrace bounded to 8 KiB and limited to one per second (over the limit
//! the line says `"backtrace":"rate-limited"`). A panic raised inside the
//! operator stream's own code gets the stderr line only.

pub(crate) mod cycle;
pub(crate) mod failure;
#[cfg(test)]
mod runtime_log_safety_tests;

use std::fmt;
use std::io::{self, Write};
use std::sync::Once;
use std::time::Duration;

use elitea_faults::codes::INTERNAL_PANIC;
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
use tracing_subscriber::filter::{FilterExt as _, filter_fn};
use tracing_subscriber::fmt::MakeWriter;
use tracing_subscriber::fmt::format::FmtSpan;
use tracing_subscriber::layer::SubscriberExt as _;
use tracing_subscriber::registry::LookupSpan;
use tracing_subscriber::util::SubscriberInitExt as _;

static PANIC_HOOK: Once = Once::new();
thread_local! {
    /// Set while this thread runs the panic hook (a panic inside it must not
    /// log through tracing again).
    static PANIC_HOOK_ACTIVE: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
}
const LOG_LEVEL_ENVIRONMENT: &str = "ELITEA_RUST_LOG";
const SPAN_TIMING_ENVIRONMENT: &str = "ELITEA_RUST_SPAN_TIMING";
const TRACE_LEVEL_ENVIRONMENT: &str = "ELITEA_RUST_TRACE";
const OTEL_DISABLED_ENVIRONMENT: &str = "OTEL_SDK_DISABLED";
const OTEL_ENDPOINT_ENVIRONMENT: &str = "OTEL_EXPORTER_OTLP_ENDPOINT";
const OTEL_TRACES_ENDPOINT_ENVIRONMENT: &str = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT";
const OTEL_SERVICE_NAME: &str = "elitea-worker-rust";
const OTEL_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(5);
/// Elitea-owned crates whose events are reviewed for field safety. The Worker
/// and the libraries it hosts (ADR-0027) share one level; every other crate
/// stays off.
pub(crate) const OWNED_TARGETS: [&str; 6] = [
    "elitea_worker_rust",
    "elitea_agent_runtime",
    "elitea_vector_index",
    "elitea_connectors",
    // ADR-0034: the cloud executor's bundle codec.
    "elitea_workspace_store",
    // FU-299: fault lines take the caller's target; listed because it links
    // `tracing` (every hosted crate that does is owned).
    "elitea_faults",
];

/// The Worker's safe cause renderers for `elitea_faults::fault!` (FU-299).
/// A type is listed only when its rendering is data-free: registered faults by
/// code and sentence, types whose Display is built from `&'static str`
/// literals by that Display (each `SafeCause` impl says why), `io::Error` by
/// kind and a gRPC status by its code. Anything else in a chain renders as
/// `elitea_faults::UNREGISTERED_CAUSE`.
pub(crate) const SAFE_CAUSES: &[elitea_faults::Renderer] = &[
    elitea_faults::render_fault::<crate::execution::AgentOutputPreflightError>,
    elitea_faults::render_fault::<crate::execution::output_delivery::AgentProgressPublishError>,
    elitea_faults::render_safe::<crate::transport::output_grpc::OutputGrpcError>,
    elitea_faults::render_safe::<crate::transport::OutputProtocolError>,
    elitea_faults::render_safe::<crate::spool::SpoolError>,
    elitea_faults::render_safe::<crate::protocol::ProtocolError>,
    elitea_faults::render_safe::<std::io::Error>,
    render_grpc_status,
];

/// A gRPC status by its code only: the message can echo request data.
fn render_grpc_status(error: &(dyn std::error::Error + 'static)) -> Option<String> {
    error
        .downcast_ref::<tonic::Status>()
        .map(|status| format!("grpc: {:?}", status.code()))
}

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
/// Elitea-owned crates in `OWNED_TARGETS`. Arbitrary `RUST_LOG` directives
/// are deliberately ignored so enabling local diagnostics cannot expose
/// dependency-owned HTTP, model, SMTP or SQL fields. stdout is the JSON
/// operator stream (module docs); span timing goes to OpenTelemetry, and to
/// stderr only under `ELITEA_RUST_SPAN_TIMING=stderr`.
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
    let span_timing_setting = std::env::var(SPAN_TIMING_ENVIRONMENT).ok();
    let span_timing = span_timing_layer(
        configured.as_deref(),
        span_timing_setting.as_deref(),
        io::stderr,
    )?;
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
                .with(span_timing)
                .with(telemetry)
                .try_init()
                .map_err(|_| DiagnosticInitError::SubscriberUnavailable)?;
            global::set_tracer_provider(provider.clone());
        }
        None => tracing_subscriber::registry()
            .with(format)
            .with(span_timing)
            .try_init()
            .map_err(|_| DiagnosticInitError::SubscriberUnavailable)?,
    }
    failure::enable(capture_failures);
    Ok(DiagnosticGuard { provider })
}

/// The operator stream: JSON lines, events only, owned crates only.
pub(crate) fn log_layer<S, W>(
    configured: Option<&str>,
    writer: W,
) -> Result<impl tracing_subscriber::Layer<S> + Send + Sync, DiagnosticInitError>
where
    S: tracing::Subscriber + for<'span> LookupSpan<'span>,
    W: for<'writer> MakeWriter<'writer> + Send + Sync + 'static,
{
    let directive = tracing_directive(configured)?;
    let filter = EnvFilter::try_new(directive).map_err(|_| DiagnosticInitError::InvalidLogLevel)?;
    Ok(elitea_faults::OperatorJsonLayer::new(writer).with_filter(filter))
}

/// Span close lines (`time.busy`, `time.idle`) in compact text, and nothing
/// else, when `setting` is `stderr`; `None` (off) for any other value.
fn span_timing_layer<S, W>(
    configured: Option<&str>,
    setting: Option<&str>,
    writer: W,
) -> Result<Option<impl tracing_subscriber::Layer<S> + Send + Sync>, DiagnosticInitError>
where
    S: tracing::Subscriber + for<'span> LookupSpan<'span>,
    W: for<'writer> MakeWriter<'writer> + Send + Sync + 'static,
{
    if setting.map(str::trim) != Some("stderr") {
        return Ok(None);
    }
    let directive = tracing_directive(configured)?;
    let filter = EnvFilter::try_new(directive).map_err(|_| DiagnosticInitError::InvalidLogLevel)?;
    // Spans only: the layer's own close lines are written from `on_close`,
    // which a span-only filter still reaches; events stay on stdout.
    let spans_only = filter.and(filter_fn(|metadata: &tracing::Metadata<'_>| {
        metadata.is_span()
    }));
    Ok(Some(
        tracing_subscriber::fmt::layer()
            .compact()
            .with_ansi(false)
            .with_target(true)
            .with_thread_ids(true)
            .with_span_events(FmtSpan::CLOSE)
            .with_writer(writer)
            .with_filter(spans_only),
    ))
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
///
/// Every panic writes the redacted stderr line (source file name and line,
/// no payload), whatever the log level, so a panic is never silent. When
/// ERROR events are enabled it also logs one operator-stream line (code
/// `internal.panic`, module docs): the payload's TYPE, never its text; the
/// source; the current span fields; a bounded, rate-limited backtrace. That
/// event is skipped when the panic was raised inside the operator stream's own
/// code (`elitea_faults::in_operator_stream`) or inside this hook, because
/// logging through the layer again could deadlock on the span extensions it
/// holds or panic again and abort.
///
/// In the unit-test binary the hook is process-wide: one test installing it
/// would otherwise strip the message from every later panic, test assertions
/// included, and print its line straight to stderr past libtest's per-test
/// capture. There it also runs the hook it replaced (the default), so a
/// failing test keeps its full message in its own captured output.
pub fn install_redacted_panic_hook() {
    PANIC_HOOK.call_once(|| {
        #[cfg(test)]
        let previous = std::panic::take_hook();
        std::panic::set_hook(Box::new(move |information| {
            let source = information.location().and_then(|location| {
                panic_source_label(location.file()).map(|source| (source, location.line()))
            });
            let reentered = PANIC_HOOK_ACTIVE.with(|active| active.replace(true));
            report_panic(
                panic_payload_type(information.payload()),
                source,
                !reentered && !elitea_faults::in_operator_stream(),
            );
            if !reentered {
                PANIC_HOOK_ACTIVE.with(|active| active.set(false));
            }
            #[cfg(test)]
            previous(information);
        }));
    });
}

/// The payload's type only: its text can hold request data.
fn panic_payload_type(payload: &(dyn std::any::Any + Send)) -> &'static str {
    if payload.is::<&'static str>() {
        "&str"
    } else if payload.is::<String>() {
        "String"
    } else {
        "other"
    }
}

fn report_panic(payload_type: &'static str, source: Option<(&str, u32)>, log_event: bool) {
    {
        let mut stderr = io::stderr().lock();
        let _ignored = match source {
            Some((source, line)) => writeln!(
                stderr,
                "elitea worker task panicked; payload redacted; source={source}:{line}"
            ),
            None => stderr.write_all(b"elitea worker task panicked; details redacted\n"),
        };
    }
    if !log_event || !tracing::enabled!(tracing::Level::ERROR) {
        return;
    }
    let source = source.map(|(source, line)| format!("{source}:{line}"));
    let backtrace = failure::panic_backtrace();
    tracing::error!(
        code = INTERNAL_PANIC.as_str(),
        action = INTERNAL_PANIC.action(),
        payload_type,
        source = source.as_deref().unwrap_or("redacted"),
        backtrace = backtrace.as_deref().unwrap_or("rate-limited"),
        "{}",
        INTERNAL_PANIC.sentence()
    );
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
        DiagnosticInitError, OWNED_TARGETS, install_tls_crypto_provider, log_layer,
        panic_source_label, span_timing_layer, trace_directive, trace_export_enabled,
        tracing_directive,
    };
    use elitea_faults::logtest;
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
            "elitea_worker_rust=info,elitea_agent_runtime=info,elitea_vector_index=info,elitea_connectors=info,elitea_workspace_store=info,elitea_faults=info"
        );
        assert_eq!(
            tracing_directive(Some(" DEBUG ")).expect("debug tracing directive"),
            "elitea_worker_rust=debug,elitea_agent_runtime=debug,elitea_vector_index=debug,elitea_connectors=debug,elitea_workspace_store=debug,elitea_faults=debug"
        );
        assert_eq!(
            trace_directive(Some("warn")).expect("warn trace directive"),
            "elitea_worker_rust=warn,elitea_agent_runtime=warn,elitea_vector_index=warn,elitea_connectors=warn,elitea_workspace_store=warn,elitea_faults=warn"
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

    /// A library crate under `libs/rust` that the Worker hosts and that emits
    /// tracing events must be an owned target, or its events vanish in deployed
    /// Workers the way the runtime crate's did after ADR-0027.
    #[test]
    fn every_hosted_library_crate_that_traces_is_an_owned_target() {
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        let read = |path: std::path::PathBuf| -> toml::Table {
            std::fs::read_to_string(&path)
                .expect("manifest")
                .parse()
                .expect("manifest TOML")
        };
        let manifest = read(root.join("Cargo.toml"));
        let dependencies = manifest["dependencies"].as_table().expect("dependencies");
        let mut hosted = 0;
        let mut missing = Vec::new();
        for (name, spec) in dependencies {
            let Some(path) = spec.get("path").and_then(toml::Value::as_str) else {
                continue;
            };
            if !path.starts_with("../../libs/rust/") || path.starts_with("../../libs/rust/vendor/")
            {
                continue;
            }
            hosted += 1;
            let library = read(root.join(path).join("Cargo.toml"));
            let traces = library
                .get("dependencies")
                .and_then(toml::Value::as_table)
                .is_some_and(|table| table.contains_key("tracing"));
            let target = name.replace('-', "_");
            if traces && !OWNED_TARGETS.contains(&target.as_str()) {
                missing.push(target);
            }
        }
        assert!(hosted >= 2, "the hosted libs/rust crates were not found");
        assert!(
            missing.is_empty(),
            "these hosted crates emit tracing events but are not in OWNED_TARGETS: {missing:?}"
        );
    }

    const STREAM_CHILD_ENV: &str = "ELITEA_DIAGNOSTICS_OPERATOR_STREAM_CHILD";
    const TIMING_CHILD_ENV: &str = "ELITEA_DIAGNOSTICS_SPAN_TIMING_CHILD";
    const PANIC_CHILD_ENV: &str = "ELITEA_DIAGNOSTICS_PANIC_CHILD";
    const PANIC_SECRET: &str = "PANIC-PAYLOAD-SECRET-CANARY";

    /// Emits a run's lines the way the delivery spans do: span fields set at
    /// the root, one recorded later, an event inside a child span.
    fn emit_run_lines() {
        let root = tracing::info_span!(
            target: "elitea_worker_rust::execution::agent_delivery_processor",
            "agent.delivery",
            execution_id = "exec-7",
            support_ref = "exec-7",
            tenant_id = "tenant-1",
            resource_project_id = "2",
            command_id = "cmd-9",
            outcome = tracing::field::Empty,
        );
        let _root = root.enter();
        let child = tracing::info_span!(
            target: "elitea_worker_rust::execution::native_agent_lifecycle",
            "agent.native_lifecycle",
            generation = 4_u64,
        );
        {
            let _child = child.enter();
            tracing::warn!(
                target: "elitea_worker_rust::execution::output_delivery",
                event = "operator_stream_probe",
                "probe line"
            );
        }
        drop(child);
        root.record("outcome", "completed");
    }

    /// The operator stream is JSON per line with the run's span fields on
    /// every line and no span open or close lines.
    #[test]
    fn operator_stream_is_json_lines_with_span_fields_and_no_span_lines() {
        let output = logtest::run_child(
            "diagnostics::tests::operator_stream_child",
            STREAM_CHILD_ENV,
        );
        for line in output.stdout.lines() {
            let trimmed = line.trim();
            let libtest = trimmed.is_empty()
                || trimmed.starts_with("running ")
                || trimmed.starts_with("test ")
                || trimmed == "ok"
                || trimmed.starts_with("test result")
                || trimmed == logtest::CHILD_RAN;
            assert!(
                libtest || trimmed.starts_with('{'),
                "not a JSON line: {line}"
            );
        }
        let lines = output.operator_lines();
        assert_eq!(lines.len(), 1, "{}", output.stdout);
        let line = &lines[0];
        assert_eq!(line["msg"], "probe line");
        assert_eq!(line["level"], "WARN");
        assert_eq!(
            line["target"],
            "elitea_worker_rust::execution::output_delivery"
        );
        assert_eq!(line["span"], "agent.native_lifecycle");
        assert_eq!(line["event"], "operator_stream_probe");
        assert_eq!(line["execution_id"], "exec-7");
        assert_eq!(line["support_ref"], "exec-7");
        assert_eq!(line["tenant_id"], "tenant-1");
        assert_eq!(line["resource_project_id"], "2");
        assert_eq!(line["command_id"], "cmd-9");
        assert_eq!(line["generation"], 4);
        let time = line["time"].as_str().expect("time");
        assert!(time.ends_with('Z') && time.contains('T'), "{time}");
        for stream in [&output.stdout, &output.stderr] {
            assert!(!stream.contains("time.busy"), "{stream}");
            assert!(!stream.contains("close"), "{stream}");
        }
    }

    #[test]
    fn operator_stream_child() {
        if !logtest::is_child(STREAM_CHILD_ENV) {
            return;
        }
        // The production pair: span timing unset, so off.
        tracing_subscriber::registry()
            .with(log_layer(Some("info"), std::io::stdout).expect("operator stream"))
            .with(span_timing_layer(Some("info"), None, std::io::stderr).expect("span timing"))
            .init();
        emit_run_lines();
        logtest::child_ran();
    }

    /// `ELITEA_RUST_SPAN_TIMING=stderr` sends span close lines to stderr and
    /// nothing else there; stdout keeps only the JSON events.
    #[test]
    fn span_timing_stderr_routes_close_lines_to_stderr_only() {
        let output = logtest::run_child_with(
            "diagnostics::tests::span_timing_child",
            TIMING_CHILD_ENV,
            &[("ELITEA_RUST_SPAN_TIMING", "stderr")],
        );
        assert!(!output.stdout.contains("time.busy"), "{}", output.stdout);
        assert_eq!(output.operator_lines().len(), 1, "{}", output.stdout);
        let closes: Vec<&str> = output
            .stderr
            .lines()
            .filter(|line| line.contains("time.busy"))
            .collect();
        assert_eq!(closes.len(), 2, "{}", output.stderr);
        assert!(closes.iter().any(|line| line.contains("agent.delivery")));
        assert!(
            closes
                .iter()
                .any(|line| line.contains("outcome=\"completed\""))
        );
        assert!(!output.stderr.contains("probe line"), "{}", output.stderr);
    }

    #[test]
    fn span_timing_child() {
        if !logtest::is_child(TIMING_CHILD_ENV) {
            return;
        }
        let setting = std::env::var("ELITEA_RUST_SPAN_TIMING").ok();
        tracing_subscriber::registry()
            .with(log_layer(Some("info"), std::io::stdout).expect("operator stream"))
            .with(
                span_timing_layer(Some("info"), setting.as_deref(), std::io::stderr)
                    .expect("span timing"),
            )
            .init();
        emit_run_lines();
        logtest::child_ran();
    }

    #[test]
    fn span_timing_is_off_unless_set_to_stderr_and_keeps_the_level_rules() {
        type Registry = tracing_subscriber::Registry;
        for setting in [None, Some(""), Some("stdout"), Some("on"), Some("STDERR")] {
            assert!(
                span_timing_layer::<Registry, _>(Some("info"), setting, std::io::stderr)
                    .expect("valid level")
                    .is_none(),
                "{setting:?}"
            );
        }
        assert!(
            span_timing_layer::<Registry, _>(Some("info"), Some(" stderr "), std::io::stderr)
                .expect("valid level")
                .is_some()
        );
        assert!(matches!(
            span_timing_layer::<Registry, _>(Some("loud"), Some("stderr"), std::io::stderr),
            Err(DiagnosticInitError::InvalidLogLevel)
        ));
    }

    /// A panic is one ERROR line: code, registry sentence, payload type, the
    /// span fields and a bounded backtrace; never the payload text. A second
    /// panic within the second has no backtrace.
    #[test]
    fn panic_hook_logs_one_redacted_fault_line_with_a_bounded_rate_limited_backtrace() {
        let output = logtest::run_child("diagnostics::tests::panic_child", PANIC_CHILD_ENV);
        assert!(!output.stdout.contains(PANIC_SECRET), "{}", output.stdout);
        let lines = output.operator_lines();
        assert_eq!(lines.len(), 2, "{}", output.stdout);
        let sentence = elitea_faults::codes::INTERNAL_PANIC.sentence();
        for (line, payload_type) in lines.iter().zip(["String", "&str"]) {
            assert_eq!(line["level"], "ERROR");
            assert_eq!(line["msg"], sentence);
            assert_eq!(line["code"], "internal.panic");
            assert_eq!(line["payload_type"], payload_type);
            assert_eq!(line["execution_id"], "exec-panic");
            assert_eq!(line["support_ref"], "exec-panic");
            let source = line["source"].as_str().expect("source");
            assert!(source.starts_with("diagnostics.rs:"), "{source}");
        }
        let backtrace = lines[0]["backtrace"].as_str().expect("backtrace");
        assert!(!backtrace.is_empty() && backtrace != "rate-limited");
        assert!(backtrace.len() <= 8192, "{} bytes", backtrace.len());
        assert_eq!(lines[1]["backtrace"], "rate-limited");
        assert_eq!(redacted_lines(&output.stderr), 2, "{}", output.stderr);
    }

    /// The stderr lines the hook writes for panics raised in this file.
    fn redacted_lines(stderr: &str) -> usize {
        stderr
            .lines()
            .filter(|line| {
                line.starts_with(
                    "elitea worker task panicked; payload redacted; source=diagnostics.rs:",
                )
            })
            .count()
    }

    const PANIC_OFF_CHILD_ENV: &str = "ELITEA_DIAGNOSTICS_PANIC_OFF_CHILD";
    const PANIC_REENTRY_CHILD_ENV: &str = "ELITEA_DIAGNOSTICS_PANIC_REENTRY_CHILD";

    /// With `ELITEA_RUST_LOG=off` a panic still writes the redacted stderr
    /// line; the operator stream stays empty.
    #[test]
    fn a_panic_is_never_silent_with_logging_off() {
        let output = logtest::run_child("diagnostics::tests::panic_off_child", PANIC_OFF_CHILD_ENV);
        assert!(output.operator_lines().is_empty(), "{}", output.stdout);
        assert_eq!(redacted_lines(&output.stderr), 1, "{}", output.stderr);
    }

    #[test]
    fn panic_off_child() {
        if !logtest::is_child(PANIC_OFF_CHILD_ENV) {
            return;
        }
        tracing_subscriber::registry()
            .with(log_layer(Some("off"), std::io::stdout).expect("operator stream"))
            .init();
        super::install_redacted_panic_hook();
        assert!(std::panic::catch_unwind(|| panic!("PANIC-PAYLOAD-SECRET-CANARY")).is_err());
        logtest::child_ran();
    }

    /// Debug that panics, as a field value can.
    struct PanickingDebug;

    impl std::fmt::Debug for PanickingDebug {
        fn fmt(&self, _formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            panic!("PANIC-PAYLOAD-SECRET-CANARY from a field")
        }
    }

    /// A panic raised inside the operator stream's own code (a span field
    /// recorded later, an event field) writes the stderr line only and never
    /// re-enters the layer: no deadlock (the child is bounded by
    /// `logtest::CHILD_TIMEOUT`), no abort, and the next ordinary panic logs
    /// its fault line again.
    #[test]
    fn a_panic_inside_the_operator_stream_writes_stderr_only_and_does_not_deadlock() {
        let output = logtest::run_child(
            "diagnostics::tests::panic_reentry_child",
            PANIC_REENTRY_CHILD_ENV,
        );
        assert!(
            !output.stdout.contains("PANIC-PAYLOAD-SECRET-CANARY"),
            "{}",
            output.stdout
        );
        let panics: Vec<_> = output
            .operator_lines()
            .into_iter()
            .filter(|line| line["code"] == "internal.panic")
            .collect();
        assert_eq!(panics.len(), 1, "{}", output.stdout);
        assert_eq!(panics[0]["payload_type"], "&str");
        assert_eq!(panics[0]["support_ref"], "exec-reentry");
        assert_eq!(redacted_lines(&output.stderr), 3, "{}", output.stderr);
    }

    #[test]
    fn panic_reentry_child() {
        if !logtest::is_child(PANIC_REENTRY_CHILD_ENV) {
            return;
        }
        tracing_subscriber::registry()
            .with(log_layer(Some("info"), std::io::stdout).expect("operator stream"))
            .init();
        super::install_redacted_panic_hook();
        let span = tracing::info_span!(
            "agent.delivery",
            execution_id = "exec-reentry",
            support_ref = "exec-reentry",
            probe = tracing::field::Empty
        );
        let _entered = span.enter();
        let record = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            span.record("probe", tracing::field::debug(PanickingDebug));
        }));
        assert!(record.is_err());
        let event = std::panic::catch_unwind(|| {
            tracing::warn!(probe = ?PanickingDebug, "an event whose field panics");
        });
        assert!(event.is_err());
        assert!(!elitea_faults::in_operator_stream());
        assert!(std::panic::catch_unwind(|| panic!("an ordinary panic afterwards")).is_err());
        logtest::child_ran();
    }

    #[test]
    fn panic_child() {
        if !logtest::is_child(PANIC_CHILD_ENV) {
            return;
        }
        tracing_subscriber::registry()
            .with(log_layer(Some("info"), std::io::stdout).expect("operator stream"))
            .init();
        super::install_redacted_panic_hook();
        let span = tracing::info_span!(
            "agent.delivery",
            execution_id = "exec-panic",
            support_ref = "exec-panic"
        );
        let _entered = span.enter();
        let secret = PANIC_SECRET.to_owned();
        assert!(std::panic::catch_unwind(move || panic!("{secret}")).is_err());
        assert!(std::panic::catch_unwind(|| panic!("PANIC-PAYLOAD-SECRET-CANARY")).is_err());
        logtest::child_ran();
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
