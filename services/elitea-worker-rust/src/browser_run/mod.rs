//! The cloud browser of a run (ADR-0049 decisions 3, 5, 7 and 8, #1596).
//!
//! The agent-runtime `browser` family holds the tools and their rules (the
//! categories, the unattended rule, the budget). This module gives the
//! family what only the worker has:
//!
//! * [`session::GrpcBrowserSession`]: the run's session on the supervisor's
//!   `browser` profile, gRPC and mTLS, with a Main grant per call.
//! * [`context::BrowserRuntimeContext`]: the runtime-context `browser` field
//!   Main serves (the egress proxy and run token, the limits, the origin).
//! * [`BrowserSessions::host`]: one run's [`BrowserToolHost`]: the session,
//!   the gateway meter (provider `browser`, operation `engine_seconds`) and
//!   the shared [`BrowserRun`] (the gate, the budget, the grant's hosts).
//! * [`BrowserGate`]: the argument-aware confirmation the agent binds.
//!
//! Composition: `bootstrap.rs` builds [`BrowserSessions`] when the deployment
//! names `browser_sessions`; `agents::ordinary` takes it
//! (`with_browser_sessions`) and builds a host for a run whose snapshot has a
//! `browser` toolkit, or whose handed-off thread's project allows the browser
//! (the field is present). Without it the browser tools answer
//! `browser_unavailable`.

pub(crate) mod context;
pub(crate) mod session;

use std::sync::Arc;

use async_trait::async_trait;
use elitea_agent_runtime::approval_policy::ApprovalPolicy;
use elitea_agent_runtime::run_caps::RunCapsMeter;
use elitea_agent_runtime::toolkits::{
    BrowserBudget, BrowserLimits, BrowserRun, BrowserSession, BrowserToolHost, RunOrigin,
};
use serde_json::Value;
use tonic::transport::Channel;
use zeroize::Zeroizing;

use self::context::{BrowserEgressMode, BrowserRuntimeContext};
use self::session::{
    BrowserGrants, BrowserSessionShape, ClaimBrowserGrants, GrpcBrowserSession, SessionEgress,
};
use crate::protocol::control::{AgentControlClient, ClaimBoundSandboxAuthority};
use crate::protocol::elitea::runtime::v1::BrowserEngineV1;
use crate::transport::control_grpc::TonicControlRpc;

/// The toolkit name the browser tools carry when a handed-off thread is
/// offered them without a configured toolkit.
pub(crate) const OFFERED_TOOLKIT_NAME: &str = "browser";

/// What the deployment gives every run's browser: the channel to the
/// supervisor's browser profile, the grant audience and the session shape.
pub(crate) struct BrowserSessions {
    channel: Channel,
    control: Arc<AgentControlClient<TonicControlRpc>>,
    audience: String,
    /// The `full` (and `auto`) session size.
    shape: BrowserSessionShape,
    /// The `lite` size, when the deployment sets one.
    lite: Option<(u64, u64)>,
}

impl std::fmt::Debug for BrowserSessions {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("BrowserSessions")
            .field("audience", &self.audience)
            .finish_non_exhaustive()
    }
}

/// One run's inputs to its browser host.
pub(crate) struct BrowserClaim<'a> {
    pub(crate) execution_id: &'a str,
    pub(crate) field: Option<&'a BrowserRuntimeContext>,
    pub(crate) sandbox: Option<Arc<ClaimBoundSandboxAuthority>>,
    pub(crate) meter: Arc<dyn elitea_agent_runtime::toolkits::web_meter::Meter>,
    pub(crate) approval: Arc<ApprovalPolicy>,
    pub(crate) caps: Arc<RunCapsMeter>,
    /// The hosts the run's unattended grant lets submit without asking.
    pub(crate) submit_hosts: Vec<String>,
}

impl BrowserSessions {
    #[must_use]
    pub(crate) fn new(
        channel: Channel,
        control: Arc<AgentControlClient<TonicControlRpc>>,
        config: &crate::config::BrowserSessionsConfig,
    ) -> Self {
        Self {
            channel,
            control,
            audience: config.audience.clone(),
            lite: config.lite_cpu_millis.zip(config.lite_memory_bytes),
            shape: BrowserSessionShape {
                engine: BrowserEngineV1::Full,
                cpu_millis: config.cpu_millis,
                memory_bytes: config.memory_bytes,
                wall_seconds: config.wall_seconds,
                max_pages: config.max_pages,
            },
        }
    }

    /// The run's browser host. A run whose project does not allow the
    /// browser (no field), or that holds no sandbox authority, or whose
    /// egress is unavailable, still gets a host: its session refuses every
    /// call with `browser_unavailable`, so the tools fail closed and say so.
    pub(crate) async fn host(&self, claim: BrowserClaim<'_>) -> BrowserToolHost {
        let field = claim.field;
        let egress = match field.map(|field| &field.egress) {
            Some(egress) if egress.mode == BrowserEgressMode::Proxy => {
                if let Ok(proxy_url) = session::proxy_ip_url(&egress.proxy_url).await {
                    Some(SessionEgress {
                        proxy_url,
                        token: Zeroizing::new(egress.token.expose().to_owned()),
                    })
                } else {
                    tracing::warn!(
                        event = "browser_egress_proxy_unresolved",
                        "the egress proxy name did not resolve; the browser tools are unavailable"
                    );
                    None
                }
            }
            _ => None,
        };
        // Why there is no route out, for the model: only the allowlist size
        // is the project's to fix; every other reason is the deployment's.
        let no_route = if field.is_some_and(|field| {
            field.egress.mode == BrowserEgressMode::Unavailable
                && field.egress.reason == context::BROWSER_ALLOWLIST_TOO_LARGE
        }) {
            elitea_agent_runtime::toolkits::SessionError::AllowlistTooLarge
        } else {
            elitea_agent_runtime::toolkits::SessionError::Unavailable
        };
        let grants: Arc<dyn BrowserGrants> = match claim.sandbox {
            Some(authority) => Arc::new(ClaimBrowserGrants {
                control: Arc::clone(&self.control),
                authority,
                audience: self.audience.clone(),
            }),
            None => Arc::new(NoBrowserGrants),
        };
        // A schedule or a webhook started the run: nobody can answer
        // (ADR-0045). The same fact picks the run origin below.
        let unattended = field.is_some_and(|field| field.unattended);
        let session = GrpcBrowserSession::new(
            self.channel.clone(),
            grants,
            claim.execution_id,
            shape_for(
                self.shape,
                self.lite,
                field.map(|field| field.engine.as_str()),
            ),
            egress,
            unattended,
        )
        .with_no_route(no_route);
        let limits = field.map_or_else(BrowserLimits::default, |field| BrowserLimits {
            max_run_seconds: field.max_run_seconds,
            monthly_quota_seconds: field.monthly_quota_seconds,
            month_used_seconds: field.month_used_seconds,
        });
        let origin = if unattended {
            RunOrigin::Unattended
        } else {
            RunOrigin::Person
        };
        let run = BrowserRun::new(
            origin,
            claim.submit_hosts,
            claim.approval,
            BrowserBudget::new(limits, claim.caps),
        );
        BrowserToolHost::new(
            Arc::new(session) as Arc<dyn BrowserSession>,
            claim.meter,
            run,
        )
    }
}

/// The session shape for the engine Main named: `lite` gets the lite size
/// (the full one when the deployment sets none), `auto` the full size, since
/// it may end on Chrome.
fn shape_for(
    full: BrowserSessionShape,
    lite: Option<(u64, u64)>,
    engine: Option<&str>,
) -> BrowserSessionShape {
    match engine {
        Some(elitea_agent_runtime::toolkits::BROWSER_ENGINE_LITE) => {
            let (cpu_millis, memory_bytes) = lite.unwrap_or((full.cpu_millis, full.memory_bytes));
            BrowserSessionShape {
                engine: BrowserEngineV1::Lite,
                cpu_millis,
                memory_bytes,
                ..full
            }
        }
        Some(elitea_agent_runtime::toolkits::BROWSER_ENGINE_AUTO) => BrowserSessionShape {
            engine: BrowserEngineV1::Auto,
            ..full
        },
        _ => full,
    }
}

/// No sandbox authority on this claim: every grant is refused.
struct NoBrowserGrants;

#[async_trait]
impl BrowserGrants for NoBrowserGrants {
    async fn grant(
        &self,
        _activation_id: &str,
        _digest: [u8; 32],
    ) -> Result<
        crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1,
        elitea_agent_runtime::toolkits::SessionError,
    > {
        Err(elitea_agent_runtime::toolkits::SessionError::Unavailable)
    }
}

/// The browser's argument-aware confirmation (ADR-0048 decision 8): an
/// escalated call (a submit, a consequential or a payment control) asks
/// unless the run's grant answers it, and a plain call asks when the
/// approval policy names its category. It composes with the gate the run
/// already has (the workspace command's): either one asking asks.
pub(crate) struct BrowserGate {
    run: Arc<BrowserRun>,
    next: Option<Arc<dyn adk_rust::agent::ToolCallConfirmation>>,
}

impl BrowserGate {
    #[must_use]
    pub(crate) fn compose(
        run: Arc<BrowserRun>,
        next: Option<Arc<dyn adk_rust::agent::ToolCallConfirmation>>,
    ) -> Arc<dyn adk_rust::agent::ToolCallConfirmation> {
        Arc::new(Self { run, next })
    }
}

#[async_trait]
impl adk_rust::agent::ToolCallConfirmation for BrowserGate {
    async fn requires_confirmation(&self, tool_name: &str, args: &Value) -> bool {
        if self.run.requires_confirmation(tool_name, args) {
            return true;
        }
        match &self.next {
            Some(next) => next.requires_confirmation(tool_name, args).await,
            None => false,
        }
    }
}

/// The end of a run's browser session on every exit (ADR-0049 decision 8).
///
/// A completed run ends it with [`Self::end`]`(false)` (`RUN_ENDED`: a viewer
/// may still look). A paused run keeps it for the resume ([`Self::keep`]).
/// Any other exit (a failure, a cap, a cancellation, a lost lease) drops the
/// guard, and the drop ends the session at once (`NOW`). The session is
/// ended at most once.
pub(crate) struct BrowserRunEnd {
    session: Option<Arc<dyn BrowserSession>>,
}

impl BrowserRunEnd {
    #[must_use]
    pub(crate) fn new(session: Option<Arc<dyn BrowserSession>>) -> Self {
        Self { session }
    }

    /// End the session now (`now`) or as a finished run. Idempotent.
    pub(crate) async fn end(&mut self, now: bool) {
        if let Some(session) = self.session.take() {
            session.end(now).await;
        }
    }

    /// The run paused for a person: keep the session for the resume.
    pub(crate) fn keep(&mut self) {
        self.session = None;
    }
}

impl Drop for BrowserRunEnd {
    fn drop(&mut self) {
        let Some(session) = self.session.take() else {
            return;
        };
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            drop(runtime.spawn(async move { session.end(true).await }));
        } else {
            // No runtime to end it on: the supervisor's idle and wall
            // sweeps end it.
            tracing::warn!(
                event = "browser_session_end_no_runtime",
                "the run ended outside a runtime; the supervisor sweep ends its browser session"
            );
        }
    }
}

impl std::fmt::Debug for BrowserRunEnd {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("BrowserRunEnd")
            .field("armed", &self.session.is_some())
            .finish()
    }
}

/// The hosts of an unattended grant's `browser_submits`, as the run uses
/// them (lowercase, at most [`elitea_agent_runtime::toolkits::MAX_SUBMIT_HOSTS`]).
#[must_use]
pub(crate) fn submit_hosts(
    grant: Option<&crate::agents::unattended::UnattendedGrant>,
) -> Vec<String> {
    grant.map_or_else(Vec::new, |grant| grant.browser_submits().to_vec())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    /// The spec engine and size follow the engine Main named.
    #[test]
    fn the_session_shape_follows_the_engine() {
        let full = BrowserSessionShape {
            engine: BrowserEngineV1::Full,
            cpu_millis: 2_000,
            memory_bytes: 2 << 30,
            wall_seconds: 3_600,
            max_pages: 3,
        };
        let sized = Some((1_000, 512 << 20));
        assert_eq!(shape_for(full, sized, Some("full")), full);
        assert_eq!(shape_for(full, sized, None), full);
        let lite = shape_for(full, sized, Some("lite"));
        assert_eq!(
            (
                lite.engine,
                lite.cpu_millis,
                lite.memory_bytes,
                lite.max_pages
            ),
            (BrowserEngineV1::Lite, 1_000, 512 << 20, 3)
        );
        let auto = shape_for(full, sized, Some("auto"));
        assert_eq!(
            (auto.engine, auto.cpu_millis, auto.memory_bytes),
            (BrowserEngineV1::Auto, 2_000, 2 << 30)
        );
        // No lite size set: lite asks for the full size.
        let unsized_lite = shape_for(full, None, Some("lite"));
        assert_eq!(
            (unsized_lite.engine, unsized_lite.cpu_millis),
            (BrowserEngineV1::Lite, 2_000)
        );
    }

    struct Always;

    #[async_trait]
    impl adk_rust::agent::ToolCallConfirmation for Always {
        async fn requires_confirmation(&self, tool_name: &str, _args: &Value) -> bool {
            tool_name == "run_command"
        }
    }

    fn run() -> Arc<BrowserRun> {
        BrowserRun::new(
            RunOrigin::Person,
            Vec::new(),
            ApprovalPolicy::none(),
            BrowserBudget::new(BrowserLimits::default(), RunCapsMeter::unlimited()),
        )
    }

    #[tokio::test]
    async fn the_gate_composes_with_the_next_one() {
        let run = run();
        run.bind_name("browser_press", "browser_press");
        let gate = BrowserGate::compose(Arc::clone(&run), Some(Arc::new(Always)));
        // An Enter press is a submit: asked.
        assert!(
            gate.requires_confirmation("browser_press", &json!({"key": "Enter"}))
                .await
        );
        assert!(
            !gate
                .requires_confirmation("browser_press", &json!({"key": "a"}))
                .await
        );
        // The next gate still decides its own tools.
        assert!(gate.requires_confirmation("run_command", &json!({})).await);
        let alone = BrowserGate::compose(run, None);
        assert!(!alone.requires_confirmation("run_command", &json!({})).await);
    }

    /// A session that records its ends.
    #[derive(Default)]
    struct EndRecorder {
        ended: std::sync::Mutex<Vec<bool>>,
    }

    #[async_trait]
    impl BrowserSession for EndRecorder {
        async fn call(
            &self,
            _call_id: &str,
            _name: &str,
            _arguments: &Value,
        ) -> Result<
            elitea_agent_runtime::toolkits::SessionAnswer,
            elitea_agent_runtime::toolkits::SessionError,
        > {
            Err(elitea_agent_runtime::toolkits::SessionError::Unavailable)
        }

        async fn end(&self, now: bool) {
            self.ended.lock().expect("ended").push(now);
        }
    }

    impl EndRecorder {
        async fn ends(&self) -> Vec<bool> {
            for _ in 0..200 {
                if !self.ended.lock().expect("ended").is_empty() {
                    break;
                }
                tokio::task::yield_now().await;
            }
            self.ended.lock().expect("ended").clone()
        }
    }

    #[tokio::test]
    async fn a_dropped_run_end_ends_the_session_now() {
        let session = Arc::new(EndRecorder::default());
        drop(BrowserRunEnd::new(Some(
            Arc::clone(&session) as Arc<dyn BrowserSession>
        )));
        assert_eq!(session.ends().await, [true]);
    }

    #[tokio::test]
    async fn a_run_end_ends_once_and_a_pause_keeps_the_session() {
        let session = Arc::new(EndRecorder::default());
        let mut end = BrowserRunEnd::new(Some(Arc::clone(&session) as Arc<dyn BrowserSession>));
        end.end(false).await;
        end.end(true).await;
        drop(end);
        assert_eq!(
            session.ends().await,
            [false],
            "ended once, as a finished run"
        );

        let kept = Arc::new(EndRecorder::default());
        let mut end = BrowserRunEnd::new(Some(Arc::clone(&kept) as Arc<dyn BrowserSession>));
        end.keep();
        drop(end);
        tokio::task::yield_now().await;
        assert!(kept.ended.lock().expect("ended").is_empty());
    }

    #[test]
    fn the_grant_names_the_submit_hosts() {
        assert_eq!(submit_hosts(None), Vec::<String>::new());
        let grant = crate::agents::unattended::UnattendedGrant::fixture(&[], false)
            .with_browser_submits(&["shop.example.com"]);
        assert_eq!(submit_hosts(Some(&grant)), ["shop.example.com"]);
    }
}
