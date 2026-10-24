//! Spend for the web family goes through the LLM gateway's budget engine
//! (#1238): every provider call is ADMITTED before any provider I/O and
//! SETTLED after it, through `POST /llm/v1/metered/{admit,settle}` on Main's
//! `/llm` edge. The host implements [`Meter`] over the transport its model
//! calls use (the worker: `transport/metering.rs`); the family never decides
//! budget itself and never states a scope, a member or a price. It states
//! units only.
//!
//! A toolset built without a meter refuses every call with
//! `metering_unavailable`, so a host that forgets to wire one fails closed,
//! not free. The desktop never builds one: web tools never run on the laptop.

use std::fmt;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{Value, json};

/// What one provider call consumed, as the provider reported it where it
/// does, else as computed from the request.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Units {
    pub requests: u64,
    pub credits: u64,
    pub pages: u64,
}

impl Units {
    #[must_use]
    pub const fn requests(requests: u64) -> Self {
        Self {
            requests,
            credits: 0,
            pages: 0,
        }
    }

    #[must_use]
    pub const fn credits(credits: u64) -> Self {
        Self {
            requests: 1,
            credits,
            pages: 0,
        }
    }

    #[must_use]
    pub const fn pages(pages: u64) -> Self {
        Self {
            requests: 1,
            credits: 0,
            pages,
        }
    }

    /// The units as the metered wire carries them.
    #[must_use]
    pub fn to_json(self) -> Value {
        json!({"requests": self.requests, "credits": self.credits, "pages": self.pages})
    }

    /// The count of `unit` (`request`, `credit` or `page`).
    #[must_use]
    pub fn of(self, unit: &str) -> Option<u64> {
        match unit {
            "request" => Some(self.requests),
            "credit" => Some(self.credits),
            "page" => Some(self.pages),
            _ => None,
        }
    }
}

/// One operation to admit: who is paid (`provider`) for what (`operation`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct MeteredOp {
    pub provider: &'static str,
    pub operation: &'static str,
    pub estimate: Units,
}

/// The gateway's admission of one operation.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Admission {
    /// Opaque, signed by the gateway; settle presents it back.
    pub admission_id: String,
    /// The unit the price is per (`request`, `credit`, `page`).
    pub unit: String,
    pub nano_usd_per_unit: i64,
}

/// How the provider call ended.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Outcome {
    Ok,
    ProviderError,
    RefusedUpstream,
}

impl Outcome {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Ok => "ok",
            Self::ProviderError => "provider_error",
            Self::RefusedUpstream => "refused_upstream",
        }
    }
}

/// What Main's audit records for one call: the URL host, the query hash or
/// the looked-up IP. Never a full URL or a query.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuditTarget {
    pub tool: &'static str,
    pub target: String,
}

/// One settlement.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SettleRequest {
    pub admission_id: String,
    pub event_id: String,
    pub op: MeteredOp,
    pub units: Units,
    pub outcome: Outcome,
    pub audit: AuditTarget,
}

/// The gateway's answer to a settlement.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Settlement {
    pub billed_nano_usd: i64,
    pub duplicate: bool,
}

/// A budget or policy refusal, classified by the host exactly as a model
/// refusal is (`llm_wire::Refusal::classify`): `code` is the
/// `model_gateway.*` code and `public_code` the public one
/// (`PROJECT_BUDGET_EXHAUSTED`, ...), which the web and the desktop render
/// with the same Settings > Usage link as an LLM refusal.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MeterRefusal {
    pub code: String,
    pub public_code: Option<String>,
    pub retry_after_ms: Option<u64>,
}

/// Why a meter call did not admit or settle.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum MeterError {
    /// Budget, rate ceiling or enforcement refusal (402, 429, 503).
    Refused(MeterRefusal),
    /// The operation has no price row (422 `metered_operation_unpriced`).
    Unpriced,
    /// The hop failed; a settle may be retried with the same event id.
    Unavailable,
}

impl fmt::Display for MeterError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Refused(_) => "the metered operation was refused",
            Self::Unpriced => "the metered operation has no price",
            Self::Unavailable => "the metering service is unavailable",
        })
    }
}

impl std::error::Error for MeterError {}

/// The host's metering client, scoped to one claim.
#[async_trait]
pub trait Meter: Send + Sync {
    /// The execution the spend is attributed to: the claim's own, or the
    /// local turn's for a desktop `remote_toolkit_call`. It seeds the event
    /// id, so a redelivered command settles under the same id.
    fn attribution_execution_id(&self) -> &str;

    /// Admit one operation before the provider is called.
    async fn admit(&self, op: &MeteredOp) -> Result<Admission, MeterError>;

    /// Settle one admitted operation. Idempotent by `event_id`.
    async fn settle(&self, request: &SettleRequest) -> Result<Settlement, MeterError>;
}

/// The namespace of metered event ids (version 5 UUIDs).
const METERED_NAMESPACE: [u8; 16] = [
    0x6f, 0x1c, 0x5e, 0x8a, 0x2b, 0x0d, 0x5c, 0x47, 0x9e, 0x3a, 0x12, 0x38, 0xa0, 0xc0, 0xff, 0xee,
];

/// `UUIDv5(NS_ELITEA_METERED, execution_id ‖ tool_call_id ‖ attempt)`: the
/// same tool call of the same execution always settles under the same id,
/// so a retried settle or a redelivered command bills once.
#[must_use]
pub fn event_id(execution_id: &str, tool_call_id: &str, attempt: u32) -> String {
    let name = format!("{execution_id}\u{0}{tool_call_id}\u{0}{attempt}");
    uuid_v5(&METERED_NAMESPACE, name.as_bytes())
}

/// RFC 9562 version 5 (SHA-1) UUID, hyphenated.
#[must_use]
pub fn uuid_v5(namespace: &[u8; 16], name: &[u8]) -> String {
    let mut context = ring::digest::Context::new(&ring::digest::SHA1_FOR_LEGACY_USE_ONLY);
    context.update(namespace);
    context.update(name);
    let digest = context.finish();
    let mut bytes = [0_u8; 16];
    bytes.copy_from_slice(&digest.as_ref()[..16]);
    bytes[6] = (bytes[6] & 0x0f) | 0x50;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    let hex = hex(&bytes);
    format!(
        "{}-{}-{}-{}-{}",
        &hex[..8],
        &hex[8..12],
        &hex[12..16],
        &hex[16..20],
        &hex[20..]
    )
}

/// Lower-case hex of `bytes`.
pub(crate) fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut out, byte| {
            let _ = write!(out, "{byte:02x}");
            out
        })
}

/// The longest a settlement is retried: inside the gateway's NATS dedupe
/// window (at least 12 minutes), so every retry is still deduplicated.
pub const SETTLE_RETRY_WINDOW: Duration = Duration::from_mins(10);
const SETTLE_FIRST_BACKOFF: Duration = Duration::from_millis(500);
const SETTLE_MAX_BACKOFF: Duration = Duration::from_secs(30);

static SETTLE_DROPPED: AtomicU64 = AtomicU64::new(0);

/// `web_meter_settle_dropped_total`: settlements given up on (retry window
/// spent, or the gateway refused the admission). The worker exports it.
#[must_use]
pub fn settle_dropped_total() -> u64 {
    SETTLE_DROPPED.load(Ordering::Relaxed)
}

/// Settle, retrying an unavailable hop with the same event id until
/// [`SETTLE_RETRY_WINDOW`] is spent. Awaited before the tool result is
/// returned, so a turn cannot outrun its own billing.
pub async fn settle_with_retry(meter: &dyn Meter, request: &SettleRequest) -> Option<Settlement> {
    let started = tokio::time::Instant::now();
    let mut backoff = SETTLE_FIRST_BACKOFF;
    loop {
        match meter.settle(request).await {
            Ok(settlement) => return Some(settlement),
            Err(MeterError::Unavailable) if started.elapsed() + backoff < SETTLE_RETRY_WINDOW => {
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(SETTLE_MAX_BACKOFF);
            }
            Err(_) => {
                SETTLE_DROPPED.fetch_add(1, Ordering::Relaxed);
                tracing::warn!(
                    event = "web_meter_settle_dropped",
                    provider = request.op.provider,
                    operation = request.op.operation,
                    event_id = %request.event_id,
                    "a metered web operation could not be settled; it stays unbilled"
                );
                return None;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn uuid_v5_matches_the_rfc_vector() {
        // RFC 9562 appendix A.4: v5 of "www.example.com" in the DNS namespace.
        let dns = [
            0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4,
            0x30, 0xc8,
        ];
        assert_eq!(
            uuid_v5(&dns, b"www.example.com"),
            "2ed6657d-e927-568b-95e1-2665a8aea6a2"
        );
    }

    #[test]
    fn event_ids_are_stable_and_distinct() {
        let first = event_id("exec-1", "call-1", 0);
        assert_eq!(first, event_id("exec-1", "call-1", 0));
        assert_ne!(first, event_id("exec-1", "call-2", 0));
        assert_ne!(first, event_id("exec-1", "call-1", 1));
        assert_ne!(first, event_id("exec-2", "call-1", 0));
    }
}
