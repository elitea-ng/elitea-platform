//! The worker's half of a cloud browser session (ADR-0049 decisions 1, 4
//! and 5): a [`BrowserSession`] over the supervisor's
//! `BrowserSessionService` (its dedicated `browser` profile), gRPC and mTLS.
//!
//! * **Start.** At the run's first call: a fresh Main grant for the
//!   session's spec, then `StartBrowserSession` with the egress (the proxy
//!   as an IP literal, and the run token). The token rides the request body
//!   only: never a log field, an argument or the environment.
//! * **Call.** One `BrowserCall` per tool call, each with its own grant (a
//!   grant lives at most 30 seconds).
//! * **End.** `DestroyBrowserSession` with `RUN_ENDED` (the session lingers
//!   for a viewer) or `NOW`. A session that never started is not ended.
//!
//! A refusal maps to a fixed [`SessionError`] from the `elitea-browser-error`
//! metadata and the status code. No transport text reaches the model.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use elitea_agent_runtime::toolkits::{BrowserSession, SessionAnswer, SessionError, SessionImage};
use serde_json::Value;
use tokio::sync::Mutex;
use tonic::transport::Channel;
use zeroize::Zeroizing;

use crate::protocol::elitea::runtime::v1::{
    BrowserCallRequestV1, BrowserEngineV1, BrowserSessionEgressV1, BrowserSessionEndV1,
    BrowserSessionSpecV1, DestroyBrowserSessionRequestV1, SignedSandboxJobGrantV1,
    StartBrowserSessionRequestV1, browser_session_service_client::BrowserSessionServiceClient,
};
use crate::sandbox::browser_session::spec_digest;

/// The metadata key of a typed refusal (the supervisor's `ERROR_METADATA`).
const ERROR_METADATA: &str = "elitea-browser-error";
/// The deadline of a start (the supervisor waits up to 30 s for a slot,
/// then the engine starts).
const START_DEADLINE: Duration = Duration::from_secs(120);
/// The deadline of one call (a `browser_wait` is bounded below this).
const CALL_DEADLINE: Duration = Duration::from_secs(120);
/// The deadline of a destroy.
const END_DEADLINE: Duration = Duration::from_secs(30);
/// The most bytes of one answer (an 8 MiB screenshot and its JSON).
const MAX_MESSAGE_BYTES: usize = 10 * 1024 * 1024;
/// The longest arguments object the supervisor admits.
const MAX_ARGUMENTS_BYTES: usize = 64 * 1024;

/// Main's sandbox grants for one run's browser session.
#[async_trait]
pub(crate) trait BrowserGrants: Send + Sync {
    /// A grant for `digest` with the session's name as its activation.
    async fn grant(
        &self,
        activation_id: &str,
        digest: [u8; 32],
    ) -> Result<SignedSandboxJobGrantV1, SessionError>;
}

/// Main's grants over the claim's sandbox authority (production).
pub(crate) struct ClaimBrowserGrants {
    pub(crate) control: Arc<
        crate::protocol::control::AgentControlClient<
            crate::transport::control_grpc::TonicControlRpc,
        >,
    >,
    pub(crate) authority: Arc<crate::protocol::control::ClaimBoundSandboxAuthority>,
    pub(crate) audience: String,
}

#[async_trait]
impl BrowserGrants for ClaimBrowserGrants {
    async fn grant(
        &self,
        activation_id: &str,
        digest: [u8; 32],
    ) -> Result<SignedSandboxJobGrantV1, SessionError> {
        self.control
            .authorize_workspace_run(&self.authority, &self.audience, activation_id, &digest)
            .await
            .map_err(|_| SessionError::Refused)
    }
}

/// The session's resources: the supervisor's profile bounds each one.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct BrowserSessionShape {
    pub(crate) cpu_millis: u64,
    pub(crate) memory_bytes: u64,
    pub(crate) wall_seconds: u64,
    pub(crate) max_pages: u32,
}

/// The route out the session gets.
pub(crate) struct SessionEgress {
    /// `http://<ip literal>:<port>`.
    pub(crate) proxy_url: String,
    pub(crate) token: Zeroizing<String>,
}

/// One run's browser session on the supervisor.
pub(crate) struct GrpcBrowserSession {
    client: BrowserSessionServiceClient<Channel>,
    grants: Arc<dyn BrowserGrants>,
    spec: BrowserSessionSpecV1,
    egress: Option<SessionEgress>,
    /// A schedule or webhook run: nobody can be asked, so the start says so
    /// and `browser_request_user` answers `no_person` at once.
    unattended: bool,
    /// What a call answers when `egress` is `None`.
    no_route: SessionError,
    started: Mutex<bool>,
}

impl GrpcBrowserSession {
    /// A session named for the run (`elitea-br-<24 hex>`). `egress` `None`:
    /// the deployment gives the session no route out, and every call answers
    /// `browser_unavailable` without starting anything. `unattended`: the run
    /// has nobody to ask (a schedule or a webhook started it).
    #[must_use]
    pub(crate) fn new(
        channel: Channel,
        grants: Arc<dyn BrowserGrants>,
        execution_id: &str,
        shape: BrowserSessionShape,
        egress: Option<SessionEgress>,
        unattended: bool,
    ) -> Self {
        Self {
            client: BrowserSessionServiceClient::new(channel)
                .max_encoding_message_size(MAX_ARGUMENTS_BYTES + 64 * 1024)
                .max_decoding_message_size(MAX_MESSAGE_BYTES),
            grants,
            spec: BrowserSessionSpecV1 {
                revision: 1,
                name: session_name(execution_id),
                engine: BrowserEngineV1::Full as i32,
                cpu_millis: shape.cpu_millis,
                memory_bytes: shape.memory_bytes,
                wall_seconds: shape.wall_seconds,
                max_pages: shape.max_pages,
            },
            egress,
            unattended,
            no_route: SessionError::Unavailable,
            started: Mutex::new(false),
        }
    }

    /// What every call answers when the session has no route out (Main's
    /// egress `unavailable`): `Unavailable`, or `AllowlistTooLarge` when the
    /// project's merged allowlist is over the run token's bound.
    #[must_use]
    pub(crate) const fn with_no_route(mut self, no_route: SessionError) -> Self {
        self.no_route = no_route;
        self
    }

    /// The start request: the grant, the spec, the egress and whether a
    /// person can be asked.
    fn start_request(
        &self,
        grant: SignedSandboxJobGrantV1,
        egress: &SessionEgress,
    ) -> StartBrowserSessionRequestV1 {
        StartBrowserSessionRequestV1 {
            grant: Some(grant),
            spec: Some(self.spec.clone()),
            egress: Some(BrowserSessionEgressV1 {
                proxy_url: egress.proxy_url.clone(),
                proxy_token: egress.token.to_string(),
            }),
            unattended: self.unattended,
        }
    }

    async fn grant(&self) -> Result<SignedSandboxJobGrantV1, SessionError> {
        self.grants
            .grant(&self.spec.name, spec_digest(&self.spec))
            .await
    }

    /// Start the session once; a lost acknowledgement retried observes the
    /// live session (`created` false), so a repeat is safe.
    async fn ensure_started(&self) -> Result<(), SessionError> {
        let mut started = self.started.lock().await;
        if *started {
            return Ok(());
        }
        let egress = self.egress.as_ref().ok_or(self.no_route)?;
        let grant = self.grant().await?;
        let mut request = tonic::Request::new(self.start_request(grant, egress));
        request.set_timeout(START_DEADLINE);
        let answer = self.client.clone().start_browser_session(request).await;
        // The request (with its token copy) is gone; the answer has none.
        answer.map_err(|status| status_error(&status))?;
        *started = true;
        Ok(())
    }
}

#[async_trait]
impl BrowserSession for GrpcBrowserSession {
    async fn call(
        &self,
        call_id: &str,
        name: &str,
        arguments: &Value,
    ) -> Result<SessionAnswer, SessionError> {
        let arguments_json = serde_json::to_string(arguments).map_err(|_| SessionError::Refused)?;
        if arguments_json.len() > MAX_ARGUMENTS_BYTES {
            return Err(SessionError::Refused);
        }
        self.ensure_started().await?;
        let grant = self.grant().await?;
        let mut request = tonic::Request::new(BrowserCallRequestV1 {
            grant: Some(grant),
            spec: Some(self.spec.clone()),
            call_id: bounded_call_id(call_id),
            name: name.to_owned(),
            arguments_json,
        });
        request.set_timeout(CALL_DEADLINE);
        let answer = self
            .client
            .clone()
            .browser_call(request)
            .await
            .map_err(|status| call_status_error(&status))?
            .into_inner();
        // The engine ran the call: an unreadable answer is a lost one.
        let result: Value =
            serde_json::from_str(&answer.result_json).map_err(|_| SessionError::Lost)?;
        Ok(SessionAnswer {
            result,
            image: answer.image.map(|image| SessionImage {
                media_type: image.media_type,
                data: image.data,
            }),
        })
    }

    async fn end(&self, now: bool) {
        if !*self.started.lock().await {
            return;
        }
        let Ok(grant) = self.grant().await else {
            // The supervisor's idle and wall sweeps end it.
            tracing::warn!(
                event = "browser_session_end_ungranted",
                session = self.spec.name.as_str(),
                "the browser session end had no grant; the supervisor sweep ends it"
            );
            return;
        };
        let end = if now {
            BrowserSessionEndV1::Now
        } else {
            BrowserSessionEndV1::RunEnded
        };
        let mut request = tonic::Request::new(DestroyBrowserSessionRequestV1 {
            grant: Some(grant),
            spec: Some(self.spec.clone()),
            end: end as i32,
        });
        request.set_timeout(END_DEADLINE);
        if let Err(status) = self.client.clone().destroy_browser_session(request).await {
            tracing::warn!(
                event = "browser_session_end_failed",
                session = self.spec.name.as_str(),
                code = ?status.code(),
                "the browser session end failed; the supervisor sweep ends it"
            );
        }
    }
}

/// `elitea-br-<24 hex>` of the run: one session per execution. A session is
/// never reused, so a continued run (a new execution) gets a new one.
#[must_use]
pub(crate) fn session_name(execution_id: &str) -> String {
    let digest = crate::workspace_run::store::sha256_hex(
        format!("elitea.browser-session\0{execution_id}").as_bytes(),
    );
    format!("elitea-br-{}", &digest[..24])
}

/// The call id as the supervisor admits it: at most 128 bytes of
/// `[A-Za-z0-9_.:-]`; any other byte becomes `_`.
fn bounded_call_id(call_id: &str) -> String {
    let mut out: String = call_id
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || matches!(c, '_' | '.' | ':' | '-') {
                c
            } else {
                '_'
            }
        })
        .take(128)
        .collect();
    if out.is_empty() {
        out.push_str("call");
    }
    out
}

fn status_error(status: &tonic::Status) -> SessionError {
    let typed = status
        .metadata()
        .get(ERROR_METADATA)
        .and_then(|value| value.to_str().ok());
    match typed {
        Some("browser_capacity" | "browser_session_limit") => SessionError::Capacity,
        Some("browser_session_ended" | "browser_session_not_started") => SessionError::Ended,
        Some("browser_isolation_refused") => SessionError::Refused,
        Some("browser_agent_failed") => SessionError::Unavailable,
        _ => match status.code() {
            tonic::Code::ResourceExhausted => SessionError::Capacity,
            tonic::Code::PermissionDenied
            | tonic::Code::InvalidArgument
            | tonic::Code::Unauthenticated
            | tonic::Code::FailedPrecondition => SessionError::Refused,
            _ => SessionError::Unavailable,
        },
    }
}

/// A failed `BrowserCall` on a started session. A typed refusal (capacity,
/// a refused grant or argument, an ended session) never reached the engine.
/// Any other failure (a deadline, a dropped stream, the session agent failing
/// mid-call) was sent to the running session, which may have run it: its
/// time is charged ([`SessionError::Lost`]).
fn call_status_error(status: &tonic::Status) -> SessionError {
    match status_error(status) {
        SessionError::Unavailable => SessionError::Lost,
        other => other,
    }
}

/// The proxy URL as the session reaches it: `http://<ip literal>:<port>`.
/// The session has no resolver, so a proxy named by host is resolved here.
///
/// # Errors
///
/// The URL is malformed, or the name does not resolve.
pub(crate) async fn proxy_ip_url(proxy_url: &str) -> Result<String, SessionError> {
    let (host, port) = crate::workspace_run::context::proxy_host_port(proxy_url)
        .ok_or(SessionError::Unavailable)?;
    let address = if let Ok(ip) = host.parse::<std::net::IpAddr>() {
        ip
    } else {
        let resolved: Vec<std::net::SocketAddr> = tokio::time::timeout(
            Duration::from_secs(10),
            tokio::net::lookup_host((host, port)),
        )
        .await
        .map_err(|_| SessionError::Unavailable)?
        .map_err(|_| SessionError::Unavailable)?
        .collect();
        resolved
            .iter()
            .find(|address| address.is_ipv4())
            .or_else(|| resolved.first())
            .ok_or(SessionError::Unavailable)?
            .ip()
    };
    Ok(match address {
        std::net::IpAddr::V4(ip) => format!("http://{ip}:{port}"),
        std::net::IpAddr::V6(ip) => format!("http://[{ip}]:{port}"),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_session_name_is_a_supervisor_name() {
        let name = session_name("0123456789abcdef0123456789abcdef");
        assert!(
            crate::sandbox::browser_session::session_name(&name),
            "{name}"
        );
        assert_eq!(name, session_name("0123456789abcdef0123456789abcdef"));
        assert_ne!(name, session_name("fedcba9876543210fedcba9876543210"));
    }

    #[test]
    fn a_call_id_is_bounded() {
        assert_eq!(bounded_call_id("call_0"), "call_0");
        assert_eq!(bounded_call_id("a b/c"), "a_b_c");
        assert_eq!(bounded_call_id(""), "call");
        assert_eq!(bounded_call_id(&"x".repeat(300)).len(), 128);
    }

    #[test]
    fn refusals_map_to_fixed_errors() {
        let typed = |code, error: &'static str| {
            let mut status = tonic::Status::new(code, "supervisor text");
            status.metadata_mut().insert(
                ERROR_METADATA,
                tonic::metadata::MetadataValue::from_static(error),
            );
            status_error(&status)
        };
        assert_eq!(
            typed(tonic::Code::ResourceExhausted, "browser_capacity"),
            SessionError::Capacity
        );
        assert_eq!(
            typed(tonic::Code::ResourceExhausted, "browser_session_limit"),
            SessionError::Capacity
        );
        assert_eq!(
            typed(tonic::Code::FailedPrecondition, "browser_session_ended"),
            SessionError::Ended
        );
        assert_eq!(
            typed(tonic::Code::FailedPrecondition, "browser_isolation_refused"),
            SessionError::Refused
        );
        assert_eq!(
            status_error(&tonic::Status::permission_denied("x")),
            SessionError::Refused
        );
        assert_eq!(
            status_error(&tonic::Status::unavailable("x")),
            SessionError::Unavailable
        );
    }

    #[test]
    fn only_a_lost_call_answer_reached_the_engine() {
        let typed = |code, error: &'static str| {
            let mut status = tonic::Status::new(code, "supervisor text");
            status.metadata_mut().insert(
                ERROR_METADATA,
                tonic::metadata::MetadataValue::from_static(error),
            );
            call_status_error(&status)
        };
        // The session agent failed mid-call, or the answer was lost.
        for lost in [
            typed(tonic::Code::Internal, "browser_agent_failed"),
            call_status_error(&tonic::Status::deadline_exceeded("x")),
            call_status_error(&tonic::Status::unavailable("x")),
        ] {
            assert_eq!(lost, SessionError::Lost);
            assert!(lost.reached_engine());
        }
        // Refusals before the engine bill nothing.
        for refused in [
            typed(tonic::Code::ResourceExhausted, "browser_capacity"),
            typed(tonic::Code::FailedPrecondition, "browser_session_ended"),
            call_status_error(&tonic::Status::permission_denied("x")),
            call_status_error(&tonic::Status::invalid_argument("x")),
        ] {
            assert!(!refused.reached_engine(), "{refused:?}");
        }
    }

    #[tokio::test]
    async fn a_call_that_never_started_a_session_did_not_reach_the_engine() {
        // No egress: Unavailable before anything starts.
        let error = session(false)
            .call(
                "call-1",
                "browser_open",
                &serde_json::json!({"url": "https://a.example"}),
            )
            .await
            .expect_err("no egress");
        assert_eq!(error, SessionError::Unavailable);
        assert!(!error.reached_engine());
        // An allowlist too large for a run token says so.
        let error = session(false)
            .with_no_route(SessionError::AllowlistTooLarge)
            .call(
                "call-1",
                "browser_open",
                &serde_json::json!({"url": "https://a.example"}),
            )
            .await
            .expect_err("no route");
        assert_eq!(error, SessionError::AllowlistTooLarge);
        assert!(!error.reached_engine());
        // Oversized arguments: refused before the start.
        let big = serde_json::json!({"text": "x".repeat(MAX_ARGUMENTS_BYTES + 1)});
        let error = session(false)
            .call("call-1", "browser_type", &big)
            .await
            .expect_err("too large");
        assert!(!error.reached_engine());
    }

    struct RefusedGrants;

    #[async_trait]
    impl BrowserGrants for RefusedGrants {
        async fn grant(
            &self,
            _activation_id: &str,
            _digest: [u8; 32],
        ) -> Result<SignedSandboxJobGrantV1, SessionError> {
            Err(SessionError::Refused)
        }
    }

    fn session(unattended: bool) -> GrpcBrowserSession {
        GrpcBrowserSession::new(
            Channel::from_static("http://127.0.0.1:1").connect_lazy(),
            Arc::new(RefusedGrants),
            "0123456789abcdef0123456789abcdef",
            BrowserSessionShape {
                cpu_millis: 1000,
                memory_bytes: 1 << 30,
                wall_seconds: 600,
                max_pages: 4,
            },
            None,
            unattended,
        )
    }

    #[tokio::test]
    async fn the_start_says_whether_a_person_can_be_asked() {
        let egress = SessionEgress {
            proxy_url: "http://10.0.0.7:3128".to_owned(),
            token: Zeroizing::new("token".to_owned()),
        };
        let unattended = session(true).start_request(SignedSandboxJobGrantV1::default(), &egress);
        assert!(
            unattended.unattended,
            "a schedule or webhook run has nobody to ask"
        );
        let attended = session(false).start_request(SignedSandboxJobGrantV1::default(), &egress);
        assert!(!attended.unattended, "a person's run may ask");
        assert_eq!(
            attended.spec, unattended.spec,
            "the flag is outside the spec and its digest"
        );
        assert_eq!(
            attended.egress.map(|egress| egress.proxy_url),
            Some("http://10.0.0.7:3128".to_owned())
        );
    }

    #[tokio::test]
    async fn an_ip_proxy_needs_no_lookup() {
        assert_eq!(
            proxy_ip_url("http://10.0.0.7:3128").await,
            Ok("http://10.0.0.7:3128".to_owned())
        );
        assert_eq!(
            proxy_ip_url("http://localhost:3128").await,
            Ok("http://127.0.0.1:3128".to_owned())
        );
        assert_eq!(
            proxy_ip_url("http://run:x@proxy:3128").await,
            Err(SessionError::Unavailable)
        );
    }
}
