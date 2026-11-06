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
    shape: BrowserSessionShape,
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
            shape: BrowserSessionShape {
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
            self.shape,
            egress,
            unattended,
        );
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

    #[test]
    fn the_grant_names_the_submit_hosts() {
        assert_eq!(submit_hosts(None), Vec::<String>::new());
        let grant = crate::agents::unattended::UnattendedGrant::fixture(&[], false)
            .with_browser_submits(&["shop.example.com"]);
        assert_eq!(submit_hosts(Some(&grant)), ["shop.example.com"]);
    }
}
