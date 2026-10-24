//! The web families' [`Meter`] (#1238): `POST /llm/v1/metered/{admit,settle}`
//! on the model facade's channel, under the claim's credential and project,
//! exactly as a model call. Nothing here adds a transport.
//!
//! The execution id sent is the ATTRIBUTION id: the claim's own, or the
//! desktop turn a `remote_toolkit_call` was made for. Main's edge accepts the
//! latter only for the claim of that call's execution, and refuses a metered
//! call whose id it would drop (403 `execution_not_attributable`).
//!
//! A refusal is classified as a model refusal is
//! ([`elitea_llm_wire::refusal::Refusal::classify`]), so a 402 carries the
//! same `model_gateway.*` and public budget codes the chat renders.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use bytes::Bytes;
use elitea_agent_runtime::toolkits::web_meter::{
    Admission, FailureClass, Meter, MeterError, MeterRefusal, MeteredOp, SettleRequest, Settlement,
};
use elitea_agent_runtime::toolkits::{
    WebEgressAllowlist, WebEgressAllowlistError, WebToolHost, provider_egress_entries,
};
use elitea_llm_wire::refusal::{BudgetScope, Refusal};
use elitea_llm_wire::route::{METERED_ADMIT_ROUTE, METERED_SETTLE_ROUTE};
use http::header::RETRY_AFTER;
use http_body_util::BodyExt as _;
use serde_json::{Value, json};
use tokio::time::timeout;
use zeroize::Zeroizing;

use super::gateway_call::build_request;
use super::openai_compatible_facade::{ModelGatewayClient, ModelGatewayTransport};
use super::runtime_context::ClaimScopedEliteaContext;

/// What the worker lends the web families (#1238) beside a claim's meter:
/// the egress allowlist provider calls are held to, and the origin the
/// direct fetch's `User-Agent` names.
#[derive(Debug)]
pub(crate) struct WebRuntime {
    egress: WebEgressAllowlist,
    origin: String,
}

impl WebRuntime {
    /// `entries` is the deployment's `web_egress_allowlist`; absent, every
    /// provider origin the registry declares (each adapter is pinned to its
    /// own origins regardless). `origin` is the deployment's public base URL,
    /// else its platform origin.
    ///
    /// # Errors
    ///
    /// A malformed entry: the whole list is refused (fail-closed).
    pub(crate) fn new(
        entries: Option<&[String]>,
        origin: &str,
    ) -> Result<Self, WebEgressAllowlistError> {
        let defaults = provider_egress_entries();
        let entries = entries.unwrap_or(&defaults);
        Ok(Self {
            egress: WebEgressAllowlist::parse(entries.iter().map(String::as_str))?,
            origin: origin.to_owned(),
        })
    }

    /// The host of one claim: its meter attributed to `attribution` (or the
    /// claim's own execution).
    pub(crate) fn host(
        &self,
        facade: &super::model_facade::ModelFacade,
        context: &ClaimScopedEliteaContext,
        attribution: Option<&str>,
    ) -> WebToolHost {
        WebToolHost::new(
            Arc::new(facade.meter(context, attribution)),
            self.egress.clone(),
            &self.origin,
        )
    }
}

/// One deadline per admit or settle, from the first byte to the last.
const METER_DEADLINE: Duration = Duration::from_secs(15);
/// The most bytes of an answer (both are a few fields).
const MAX_ANSWER_BYTES: usize = 64 * 1024;

/// The metering client of one claim.
pub(crate) struct GatewayMeter {
    transport: Arc<dyn ModelGatewayTransport>,
    token: Zeroizing<String>,
    project_id: u64,
    execution_id: String,
}

impl std::fmt::Debug for GatewayMeter {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("GatewayMeter")
            .field("project_id", &self.project_id)
            .finish_non_exhaustive()
    }
}

struct Answer {
    status: u16,
    retry_after_secs: Option<u64>,
    body: Bytes,
}

impl GatewayMeter {
    /// The meter of `context`'s claim over `gateway`'s channel. `attribution`
    /// replaces the claim's execution id when the call was made for another
    /// (a non-empty `attribution_execution_id` of a remote toolkit call).
    pub(crate) fn new(
        gateway: &ModelGatewayClient,
        context: &ClaimScopedEliteaContext,
        attribution: Option<&str>,
    ) -> Self {
        Self::with_transport(gateway.transport.clone(), context, attribution)
    }

    pub(crate) fn with_transport(
        transport: Arc<dyn ModelGatewayTransport>,
        context: &ClaimScopedEliteaContext,
        attribution: Option<&str>,
    ) -> Self {
        let execution_id = attribution
            .filter(|id| !id.is_empty())
            .unwrap_or_else(|| context.execution_id())
            .to_owned();
        Self {
            transport,
            token: context.model_facade_token(),
            project_id: context.resource_project_id(),
            execution_id,
        }
    }

    async fn post(&self, route: &'static str, body: &Value) -> Result<Answer, MeterError> {
        let body = serde_json::to_vec(body).map_err(|_| MeterError::Unavailable)?;
        let request = build_request(
            route,
            "application/json",
            Bytes::from(body),
            self.project_id,
            &self.token,
            &self.execution_id,
        )
        .map_err(|_| MeterError::Unavailable)?;
        timeout(METER_DEADLINE, async {
            let response = self
                .transport
                .post(request)
                .await
                .map_err(|_| MeterError::Unavailable)?;
            let status = response.status().as_u16();
            let retry_after_secs = response
                .headers()
                .get(RETRY_AFTER)
                .and_then(|value| value.to_str().ok())
                .and_then(|value| value.trim().parse().ok());
            let body = http_body_util::Limited::new(response.into_body(), MAX_ANSWER_BYTES)
                .collect()
                .await
                .map_err(|_| MeterError::Unavailable)?
                .to_bytes();
            Ok(Answer {
                status,
                retry_after_secs,
                body,
            })
        })
        .await
        .map_err(|_| MeterError::Unavailable)?
    }
}

/// The gateway's own error code (`{"error":{"code":…}}`), if any.
fn error_code(body: &[u8]) -> Option<String> {
    let value: Value = serde_json::from_slice(body).ok()?;
    value
        .pointer("/error/code")
        .and_then(Value::as_str)
        .map(str::to_owned)
}

/// A refusal as the chat renders a model refusal.
fn refusal(answer: &Answer) -> MeterError {
    let classified = Refusal::classify(answer.status, &answer.body).unwrap_or(Refusal::Rejected);
    let public_code = match classified {
        Refusal::BudgetExhausted(BudgetScope::Project) => Some("PROJECT_BUDGET_EXHAUSTED"),
        Refusal::BudgetExhausted(BudgetScope::Member) => Some("MEMBER_BUDGET_EXHAUSTED"),
        _ => None,
    };
    MeterError::Refused(MeterRefusal {
        code: classified.code().to_owned(),
        public_code: public_code.map(str::to_owned),
        retry_after_ms: answer
            .retry_after_secs
            .map(|secs| secs.saturating_mul(1_000)),
    })
}

#[async_trait]
impl Meter for GatewayMeter {
    fn attribution_execution_id(&self) -> &str {
        &self.execution_id
    }

    async fn admit(&self, op: &MeteredOp) -> Result<Admission, MeterError> {
        let answer = self
            .post(
                METERED_ADMIT_ROUTE,
                &json!({
                    "operation": op.operation,
                    "provider": op.provider,
                    "estimate": op.estimate.to_json(),
                }),
            )
            .await?;
        if (200..300).contains(&answer.status) {
            let value: Value =
                serde_json::from_slice(&answer.body).map_err(|_| MeterError::Unavailable)?;
            let admission_id = value["admission_id"].as_str().filter(|id| !id.is_empty());
            let unit = value.pointer("/price/unit").and_then(Value::as_str);
            let nano = value
                .pointer("/price/nano_usd_per_unit")
                .and_then(Value::as_i64);
            return match (admission_id, unit, nano) {
                (Some(admission_id), Some(unit), Some(nano)) => Ok(Admission {
                    admission_id: admission_id.to_owned(),
                    unit: unit.to_owned(),
                    nano_usd_per_unit: nano,
                }),
                _ => Err(MeterError::Unavailable),
            };
        }
        if answer.status == 422
            && error_code(&answer.body).as_deref() == Some("metered_operation_unpriced")
        {
            return Err(MeterError::Unpriced);
        }
        Err(refusal(&answer))
    }

    async fn settle(&self, request: &SettleRequest) -> Result<Settlement, MeterError> {
        let answer = self
            .post(
                METERED_SETTLE_ROUTE,
                &json!({
                    "admission_id": request.admission_id,
                    "event_id": request.event_id,
                    "operation": request.op.operation,
                    "provider": request.op.provider,
                    "units": request.units.to_json(),
                    "outcome": request.outcome.as_str(),
                    // Whether the provider was reached and charges for the
                    // failure: the gateway bills a failure charge only for
                    // `target_failed` (#1238).
                    "failure": request.failure.map(FailureClass::as_str),
                    "audit": {
                        "tool": request.audit.tool,
                        "target": request.audit.target,
                        "status": request.outcome.as_str(),
                    },
                }),
            )
            .await?;
        match answer.status {
            200..=299 => {
                let value: Value =
                    serde_json::from_slice(&answer.body).map_err(|_| MeterError::Unavailable)?;
                Ok(Settlement {
                    billed_nano_usd: value["billed_nano_usd"].as_i64().unwrap_or_default(),
                    duplicate: value["duplicate"].as_bool().unwrap_or_default(),
                })
            }
            // Retried with the same event id: the gateway deduplicates it.
            408 | 429 | 500..=599 => Err(MeterError::Unavailable),
            _ => Err(refusal(&answer)),
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;

    use elitea_agent_runtime::toolkits::web_meter::{AuditTarget, Outcome, Units, event_id};
    use elitea_llm_wire::headers::{EXECUTION_HEADER, PROJECT_HEADER};
    use http::{Request, Response};
    use http_body_util::Full;
    use tonic::body::Body;

    use super::*;
    use crate::transport::openai_compatible_facade::ModelGatewayTransportError;

    #[derive(Default)]
    struct Recorded {
        path: String,
        execution: String,
        project: String,
        authorization: String,
        body: Value,
    }

    struct Gateway {
        status: u16,
        retry_after: Option<&'static str>,
        body: &'static str,
        seen: Mutex<Vec<Recorded>>,
    }

    impl Gateway {
        fn answering(status: u16, body: &'static str) -> Arc<Self> {
            Arc::new(Self {
                status,
                retry_after: None,
                body,
                seen: Mutex::default(),
            })
        }
    }

    #[async_trait]
    impl ModelGatewayTransport for Gateway {
        async fn post(
            &self,
            request: Request<Body>,
        ) -> Result<Response<Body>, ModelGatewayTransportError> {
            let (parts, body) = request.into_parts();
            let bytes = body.collect().await.expect("body").to_bytes();
            let header = |name: &str| {
                parts
                    .headers
                    .get(name)
                    .and_then(|value| value.to_str().ok())
                    .unwrap_or_default()
                    .to_owned()
            };
            self.seen.lock().expect("seen").push(Recorded {
                path: parts.uri.path().to_owned(),
                execution: header(EXECUTION_HEADER),
                project: header(PROJECT_HEADER),
                authorization: header("authorization"),
                body: serde_json::from_slice(&bytes).expect("JSON body"),
            });
            let mut response = Response::builder().status(self.status);
            if let Some(retry_after) = self.retry_after {
                response = response.header(RETRY_AFTER, retry_after);
            }
            Ok(response
                .body(Body::new(Full::new(Bytes::from_static(
                    self.body.as_bytes(),
                ))))
                .expect("response"))
        }
    }

    fn context() -> ClaimScopedEliteaContext {
        ClaimScopedEliteaContext::fixture_with_execution_id(17, "claim-pat", "claim-execution")
    }

    fn op() -> MeteredOp {
        MeteredOp {
            provider: "tavily",
            operation: "web_search",
            estimate: Units::credits(1),
        }
    }

    #[tokio::test]
    async fn meter_uses_model_facade_identity() {
        let gateway = Gateway::answering(
            200,
            r#"{"admission_id":"adm-1","price":{"unit":"credit","nano_usd_per_unit":8000000},"estimate_nano_usd":8000000}"#,
        );
        let meter = GatewayMeter::with_transport(gateway.clone(), &context(), None);
        let admission = meter.admit(&op()).await.expect("admitted");
        assert_eq!(admission.admission_id, "adm-1");
        assert_eq!(admission.unit, "credit");
        assert_eq!(admission.nano_usd_per_unit, 8_000_000);
        let seen = gateway.seen.lock().expect("seen");
        assert_eq!(seen[0].path, METERED_ADMIT_ROUTE);
        assert_eq!(seen[0].project, "17");
        assert_eq!(seen[0].authorization, "Bearer claim-pat");
        assert_eq!(seen[0].execution, "claim-execution");
        assert_eq!(
            seen[0].body,
            json!({"operation": "web_search", "provider": "tavily", "estimate": {"requests": 1, "credits": 1, "pages": 0}})
        );
        assert!(!format!("{meter:?}").contains("claim-pat"));
    }

    #[tokio::test]
    async fn meter_sends_attribution_execution_id_for_remote_calls() {
        let gateway = Gateway::answering(200, r#"{"billed_nano_usd":8000000,"duplicate":false}"#);
        let meter = GatewayMeter::with_transport(gateway.clone(), &context(), Some("desktop-turn"));
        assert_eq!(meter.attribution_execution_id(), "desktop-turn");
        let event = event_id("desktop-turn", "call-1", "invocation-1");
        let settled = meter
            .settle(&SettleRequest {
                admission_id: "adm-1".to_owned(),
                event_id: event.clone(),
                op: op(),
                units: Units::credits(2),
                outcome: Outcome::Ok,
                failure: None,
                audit: AuditTarget {
                    tool: "web_search",
                    target: "f".repeat(64),
                },
            })
            .await
            .expect("settled");
        assert_eq!(settled.billed_nano_usd, 8_000_000);
        let seen = gateway.seen.lock().expect("seen");
        assert_eq!(seen[0].path, METERED_SETTLE_ROUTE);
        assert_eq!(seen[0].execution, "desktop-turn");
        assert_eq!(seen[0].body["event_id"], event);
        assert_eq!(seen[0].body["units"]["credits"], 2);
        assert_eq!(seen[0].body["audit"]["status"], "ok");
        assert_eq!(seen[0].body["failure"], Value::Null);
        // An empty attribution is the claim's own execution.
        let own = GatewayMeter::with_transport(gateway.clone(), &context(), Some(""));
        assert_eq!(own.attribution_execution_id(), "claim-execution");
    }

    #[tokio::test]
    async fn budget_402_maps_to_model_gateway_codes() {
        for (body, code, public) in [
            (
                r#"{"error":{"type":"budget_exceeded","scope":"project"}}"#,
                "model_gateway.project_budget_exhausted",
                Some("PROJECT_BUDGET_EXHAUSTED"),
            ),
            (
                r#"{"error":{"type":"budget_exceeded","scope":"member"}}"#,
                "model_gateway.member_budget_exhausted",
                Some("MEMBER_BUDGET_EXHAUSTED"),
            ),
            ("{}", "model_gateway.budget_exhausted", None),
        ] {
            let meter =
                GatewayMeter::with_transport(Gateway::answering(402, body), &context(), None);
            let Err(MeterError::Refused(refusal)) = meter.admit(&op()).await else {
                panic!("a 402 is a refusal");
            };
            assert_eq!(refusal.code, code);
            assert_eq!(refusal.public_code.as_deref(), public);
        }
        let limited = Arc::new(Gateway {
            status: 429,
            retry_after: Some("3"),
            body: "{}",
            seen: Mutex::default(),
        });
        let meter = GatewayMeter::with_transport(limited, &context(), None);
        let Err(MeterError::Refused(refusal)) = meter.admit(&op()).await else {
            panic!("a 429 is a refusal");
        };
        assert_eq!(refusal.code, "model_gateway.rate_limited");
        assert_eq!(refusal.retry_after_ms, Some(3_000));
        let unpriced = GatewayMeter::with_transport(
            Gateway::answering(422, r#"{"error":{"code":"metered_operation_unpriced"}}"#),
            &context(),
            None,
        );
        assert_eq!(unpriced.admit(&op()).await, Err(MeterError::Unpriced));
        let attributed = GatewayMeter::with_transport(
            Gateway::answering(403, r#"{"error":{"code":"execution_not_attributable"}}"#),
            &context(),
            None,
        );
        assert!(matches!(
            attributed.admit(&op()).await,
            Err(MeterError::Refused(refusal)) if refusal.code == "model_gateway.forbidden"
        ));
    }

    #[tokio::test]
    async fn settle_retries_only_what_can_succeed() {
        let request = SettleRequest {
            admission_id: "adm-1".to_owned(),
            event_id: event_id("e", "c", "i"),
            op: op(),
            units: Units::credits(1),
            outcome: Outcome::ProviderError,
            failure: Some(FailureClass::TargetFailed),
            audit: AuditTarget {
                tool: "web_search",
                target: String::new(),
            },
        };
        for (status, retried) in [(503, true), (429, true), (403, false), (400, false)] {
            let gateway = Gateway::answering(status, "{}");
            let meter = GatewayMeter::with_transport(gateway.clone(), &context(), None);
            let error = meter.settle(&request).await.expect_err("refused");
            assert_eq!(error == MeterError::Unavailable, retried, "{status}");
            assert_eq!(
                gateway.seen.lock().expect("seen")[0].body["failure"],
                "target_failed"
            );
        }
    }
}
