//! Process tracing for an engine: log lines on stderr, and the spans
//! exported over OTLP when the deployment asks for it.
//!
//! The switches are the OpenTelemetry SDK's own, read as the agent worker
//! reads them (`services/elitea-worker-rust/src/diagnostics.rs`), so one
//! collector setting covers both: export is on when
//! `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` is
//! set and `OTEL_SDK_DISABLED` is not `true`; the protocol is HTTP/protobuf.
//! The model client's `engine.model.request` span is the one a collector
//! reads per model call.
//!
//! Log lines stay exactly as they were — `RUST_LOG` filter (default `info`),
//! stderr — because a worker child's stderr is relayed by its parent.
//! Telemetry never fails a start: an exporter that cannot be built is
//! reported on stderr and the process runs without it.

use opentelemetry::trace::TracerProvider as _;
use opentelemetry_otlp::{Protocol, WithExportConfig as _, WithHttpConfig as _};
use opentelemetry_sdk::Resource;
use opentelemetry_sdk::runtime;
use opentelemetry_sdk::trace::SdkTracerProvider;
use opentelemetry_sdk::trace::span_processor_with_async_runtime::BatchSpanProcessor;
use tracing_subscriber::EnvFilter;
use tracing_subscriber::Layer as _;
use tracing_subscriber::layer::SubscriberExt as _;
use tracing_subscriber::util::SubscriberInitExt as _;

const OTEL_DISABLED: &str = "OTEL_SDK_DISABLED";
const OTEL_ENDPOINT: &str = "OTEL_EXPORTER_OTLP_ENDPOINT";
const OTEL_TRACES_ENDPOINT: &str = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT";

/// How long a shutdown waits for the last spans (the worker's value).
pub const SHUTDOWN_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(5);

/// The installed tracing. Keep it for the life of the process and call
/// [`Telemetry::shutdown`] before exit, so the last spans are flushed.
#[must_use]
pub struct Telemetry {
    provider: Option<SdkTracerProvider>,
}

impl Telemetry {
    /// Whether spans are exported.
    #[must_use]
    pub fn exporting(&self) -> bool {
        self.provider.is_some()
    }

    /// Flush and stop the exporter, waiting at most [`SHUTDOWN_TIMEOUT`]
    /// (nothing when export is off). The flush blocks, so it runs off the
    /// async workers.
    pub async fn shutdown(self) {
        let Some(provider) = self.provider else {
            return;
        };
        let flushed =
            tokio::task::spawn_blocking(move || provider.shutdown_with_timeout(SHUTDOWN_TIMEOUT))
                .await;
        if !matches!(flushed, Ok(Ok(()))) {
            eprintln!("telemetry: the span exporter did not shut down cleanly");
        }
    }
}

/// Whether the environment turns span export on (see the module docs).
#[must_use]
pub fn export_enabled(
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

/// Install the process's tracing as `service_name`. Must run inside a Tokio
/// runtime (the batch exporter is a task on it). A second call in one
/// process installs nothing and exports nothing.
pub fn init(service_name: &'static str) -> Telemetry {
    let filter = || EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    let fmt = tracing_subscriber::fmt::layer()
        .with_writer(std::io::stderr)
        .with_filter(filter());
    let provider = provider(service_name);
    let otel = provider.as_ref().map(|provider| {
        tracing_opentelemetry::layer()
            .with_tracer(provider.tracer(service_name))
            .with_filter(filter())
    });
    if tracing_subscriber::registry()
        .with(fmt)
        .with(otel)
        .try_init()
        .is_err()
    {
        // Already installed (a test, or a second call): keep the first.
        if let Some(provider) = provider {
            let _ = provider.shutdown();
        }
        return Telemetry { provider: None };
    }
    Telemetry { provider }
}

fn provider(service_name: &'static str) -> Option<SdkTracerProvider> {
    let read = |name: &str| std::env::var(name).ok();
    if !export_enabled(
        read(OTEL_DISABLED).as_deref(),
        read(OTEL_TRACES_ENDPOINT).as_deref(),
        read(OTEL_ENDPOINT).as_deref(),
    ) {
        return None;
    }
    // An explicit client: the exporter's implicit one falls back to
    // `Client::new()`, which panics when the trust store is unreadable.
    let client = match reqwest::Client::builder().build() {
        Ok(client) => client,
        Err(error) => {
            eprintln!("telemetry: no HTTP client for the span exporter, export off: {error}");
            return None;
        }
    };
    let exporter = match opentelemetry_otlp::SpanExporter::builder()
        .with_http()
        .with_http_client(client)
        .with_protocol(Protocol::HttpBinary)
        .build()
    {
        Ok(exporter) => exporter,
        Err(error) => {
            eprintln!("telemetry: the span exporter cannot start, export off: {error}");
            return None;
        }
    };
    let resource = Resource::builder().with_service_name(service_name).build();
    Some(
        SdkTracerProvider::builder()
            .with_span_processor(BatchSpanProcessor::builder(exporter, runtime::Tokio).build())
            .with_resource(resource)
            .build(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn export_follows_the_sdk_switches() {
        assert!(!export_enabled(None, None, None));
        assert!(!export_enabled(None, Some(" "), None));
        assert!(export_enabled(None, None, Some("http://collector:4318")));
        assert!(export_enabled(
            None,
            Some("http://collector:4318/v1/traces"),
            None
        ));
        assert!(!export_enabled(
            Some("TRUE"),
            None,
            Some("http://collector:4318")
        ));
        assert!(export_enabled(
            Some("false"),
            None,
            Some("http://collector:4318")
        ));
    }
}
