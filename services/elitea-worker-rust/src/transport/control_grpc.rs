//! Deadline-bound runtime control gRPC transport.
//!
//! This module performs exactly one RPC attempt. Claim, lease and settlement
//! retry policy belongs to the delivery state machine, where idempotency and
//! side-effect authority are available together.

use std::fmt;
use std::time::Duration;

use async_trait::async_trait;
use prost::Message;
use tokio::time::timeout;
use tonic::metadata::MetadataValue;
use tonic::transport::Channel;
use tonic::{Request, Response, Status};

use crate::protocol::elitea::runtime::v1::{
    AuthorizeAgentModelCheckpointRequestV1, AuthorizeAgentModelCheckpointResponseV1,
    AuthorizeInvocationRequestV1, AuthorizeInvocationResponseV1, BeginExecutionRequestV1,
    BeginExecutionResponseV1, ClaimCommandRequestV1, ClaimCommandResponseV1,
    ObserveDesiredStateRequestV1, ObserveDesiredStateResponseV1, PrepareSettlementRequestV1,
    PrepareSettlementResponseV1, RenewLeaseRequestV1, RenewLeaseResponseV1,
    runtime_control_service_client::RuntimeControlServiceClient,
};

const MAX_CONTROL_REQUEST_BYTES: usize = 64 * 1024;
const MAX_CONTROL_RESPONSE_BYTES: usize = 80 * 1024;
const MAX_CONTROL_DEADLINE: Duration = Duration::from_mins(5);
const MAX_METADATA_BYTES: usize = 256;

/// Immutable transport identity and deadline for every control call.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ControlGrpcConfig {
    pub deadline: Duration,
    pub workload_session_id: String,
    pub producer_id: String,
}

impl ControlGrpcConfig {
    fn validate(&self) -> Result<(), ControlGrpcError> {
        if self.deadline.is_zero()
            || self.deadline > MAX_CONTROL_DEADLINE
            || !valid_metadata_value(&self.workload_session_id)
            || !valid_metadata_value(&self.producer_id)
        {
            return Err(ControlGrpcError::InvalidConfiguration(
                "the control gRPC configuration is malformed",
            ));
        }
        Ok(())
    }
}

/// Stable, data-free control transport failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ControlGrpcError {
    InvalidConfiguration(&'static str),
    ResourceExhausted(&'static str),
    Unavailable(&'static str),
}

impl fmt::Display for ControlGrpcError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidConfiguration(message)
            | Self::ResourceExhausted(message)
            | Self::Unavailable(message) => formatter.write_str(message),
        }
    }
}

impl std::error::Error for ControlGrpcError {}

/// Unary RPC surface used for dependency injection and cancellation tests.
#[async_trait]
pub trait ControlRpc: Send + Sync {
    async fn claim_command(
        &self,
        request: Request<ClaimCommandRequestV1>,
    ) -> Result<Response<ClaimCommandResponseV1>, Status>;

    async fn begin_execution(
        &self,
        request: Request<BeginExecutionRequestV1>,
    ) -> Result<Response<BeginExecutionResponseV1>, Status>;

    async fn authorize_invocation(
        &self,
        request: Request<AuthorizeInvocationRequestV1>,
    ) -> Result<Response<AuthorizeInvocationResponseV1>, Status>;

    async fn authorize_agent_model_checkpoint(
        &self,
        _request: Request<AuthorizeAgentModelCheckpointRequestV1>,
    ) -> Result<Response<AuthorizeAgentModelCheckpointResponseV1>, Status> {
        Err(Status::unimplemented(
            "model checkpoint recovery is unavailable",
        ))
    }

    async fn renew_lease(
        &self,
        request: Request<RenewLeaseRequestV1>,
    ) -> Result<Response<RenewLeaseResponseV1>, Status>;

    async fn observe_desired_state(
        &self,
        request: Request<ObserveDesiredStateRequestV1>,
    ) -> Result<Response<ObserveDesiredStateResponseV1>, Status>;

    async fn prepare_settlement(
        &self,
        request: Request<PrepareSettlementRequestV1>,
    ) -> Result<Response<PrepareSettlementResponseV1>, Status>;
}

/// Generated tonic client over a caller-verified channel.
#[derive(Clone)]
pub struct TonicControlRpc {
    client: RuntimeControlServiceClient<Channel>,
}

impl TonicControlRpc {
    #[must_use]
    pub fn new(channel: Channel) -> Self {
        let client = RuntimeControlServiceClient::new(channel)
            .max_encoding_message_size(MAX_CONTROL_REQUEST_BYTES)
            .max_decoding_message_size(MAX_CONTROL_RESPONSE_BYTES);
        Self { client }
    }
}

#[async_trait]
impl ControlRpc for TonicControlRpc {
    async fn claim_command(
        &self,
        request: Request<ClaimCommandRequestV1>,
    ) -> Result<Response<ClaimCommandResponseV1>, Status> {
        self.client.clone().claim_command(request).await
    }

    async fn begin_execution(
        &self,
        request: Request<BeginExecutionRequestV1>,
    ) -> Result<Response<BeginExecutionResponseV1>, Status> {
        self.client.clone().begin_execution(request).await
    }

    async fn authorize_invocation(
        &self,
        request: Request<AuthorizeInvocationRequestV1>,
    ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
        self.client.clone().authorize_invocation(request).await
    }

    async fn authorize_agent_model_checkpoint(
        &self,
        request: Request<AuthorizeAgentModelCheckpointRequestV1>,
    ) -> Result<Response<AuthorizeAgentModelCheckpointResponseV1>, Status> {
        self.client
            .clone()
            .authorize_agent_model_checkpoint(request)
            .await
    }

    async fn renew_lease(
        &self,
        request: Request<RenewLeaseRequestV1>,
    ) -> Result<Response<RenewLeaseResponseV1>, Status> {
        self.client.clone().renew_lease(request).await
    }

    async fn observe_desired_state(
        &self,
        request: Request<ObserveDesiredStateRequestV1>,
    ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
        self.client.clone().observe_desired_state(request).await
    }

    async fn prepare_settlement(
        &self,
        request: Request<PrepareSettlementRequestV1>,
    ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
        self.client.clone().prepare_settlement(request).await
    }
}

/// One-attempt control client with whole-message and metadata bounds.
#[derive(Clone)]
pub struct ControlGrpcClient<R> {
    rpc: R,
    config: ControlGrpcConfig,
}

impl ControlGrpcClient<TonicControlRpc> {
    /// Create a client over a channel whose mTLS trust policy was already
    /// validated by deployment composition.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration error for invalid identity or deadline.
    pub fn from_channel(
        channel: Channel,
        config: ControlGrpcConfig,
    ) -> Result<Self, ControlGrpcError> {
        Self::new(TonicControlRpc::new(channel), config)
    }
}

impl<R> ControlGrpcClient<R> {
    /// Build a control adapter. The RPC implementation may be a generated
    /// tonic client or a deterministic component-test transport.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration error for invalid identity or deadline.
    pub fn new(rpc: R, config: ControlGrpcConfig) -> Result<Self, ControlGrpcError> {
        config.validate()?;
        Ok(Self { rpc, config })
    }
}

impl<R: ControlRpc> ControlGrpcClient<R> {
    /// Perform one claim attempt without transport-level retry.
    ///
    /// # Errors
    ///
    /// Returns a stable size, configuration, or availability error.
    pub async fn claim_command(
        &self,
        message: ClaimCommandRequestV1,
    ) -> Result<ClaimCommandResponseV1, ControlGrpcError> {
        let request = self.request(message)?;
        let response = timeout(self.config.deadline, self.rpc.claim_command(request))
            .await
            .map_err(|_| unavailable())?
            .map_err(|_| unavailable())?
            .into_inner();
        validate_response(&response)?;
        Ok(response)
    }

    /// Perform one begin-execution fence attempt without retry.
    ///
    /// # Errors
    ///
    /// Returns a stable size, configuration, or availability error.
    pub async fn begin_execution(
        &self,
        message: BeginExecutionRequestV1,
    ) -> Result<BeginExecutionResponseV1, ControlGrpcError> {
        let request = self.request(message)?;
        let response = timeout(self.config.deadline, self.rpc.begin_execution(request))
            .await
            .map_err(|_| unavailable())?
            .map_err(|_| unavailable())?
            .into_inner();
        validate_response(&response)?;
        Ok(response)
    }

    /// Perform one invocation-authorization fence attempt without retry.
    ///
    /// # Errors
    ///
    /// Returns a stable size, configuration, or availability error.
    pub async fn authorize_invocation(
        &self,
        message: AuthorizeInvocationRequestV1,
    ) -> Result<AuthorizeInvocationResponseV1, ControlGrpcError> {
        let request = self.request(message)?;
        let response = timeout(self.config.deadline, self.rpc.authorize_invocation(request))
            .await
            .map_err(|_| unavailable())?
            .map_err(|_| unavailable())?
            .into_inner();
        validate_response(&response)?;
        Ok(response)
    }

    /// Perform one checkpoint-authorization attempt. An uncertain response is
    /// never retried here because it can follow a committed authority receipt.
    ///
    /// # Errors
    ///
    /// Returns a stable size, configuration, or availability error.
    pub async fn authorize_agent_model_checkpoint(
        &self,
        message: AuthorizeAgentModelCheckpointRequestV1,
    ) -> Result<AuthorizeAgentModelCheckpointResponseV1, ControlGrpcError> {
        let request = self.request(message)?;
        let response = timeout(
            self.config.deadline,
            self.rpc.authorize_agent_model_checkpoint(request),
        )
        .await
        .map_err(|_| unavailable())?
        .map_err(|_| unavailable())?
        .into_inner();
        validate_response(&response)?;
        Ok(response)
    }

    /// Perform one idempotent lease-renewal attempt without retry.
    ///
    /// # Errors
    ///
    /// Returns a stable size, configuration, or availability error.
    pub async fn renew_lease(
        &self,
        message: RenewLeaseRequestV1,
    ) -> Result<RenewLeaseResponseV1, ControlGrpcError> {
        let request = self.request(message)?;
        let response = timeout(self.config.deadline, self.rpc.renew_lease(request))
            .await
            .map_err(|_| unavailable())?
            .map_err(|_| unavailable())?
            .into_inner();
        validate_response(&response)?;
        Ok(response)
    }

    /// Perform one desired-state observation attempt without retry.
    ///
    /// # Errors
    ///
    /// Returns a stable size, configuration, or availability error.
    pub async fn observe_desired_state(
        &self,
        message: ObserveDesiredStateRequestV1,
    ) -> Result<ObserveDesiredStateResponseV1, ControlGrpcError> {
        let request = self.request(message)?;
        let response = timeout(
            self.config.deadline,
            self.rpc.observe_desired_state(request),
        )
        .await
        .map_err(|_| unavailable())?
        .map_err(|_| unavailable())?
        .into_inner();
        validate_response(&response)?;
        Ok(response)
    }

    /// Perform one idempotent `PrepareSettlement` attempt without retry.
    ///
    /// # Errors
    ///
    /// Returns a stable size, configuration, or availability error.
    pub async fn prepare_settlement(
        &self,
        message: PrepareSettlementRequestV1,
    ) -> Result<PrepareSettlementResponseV1, ControlGrpcError> {
        let request = self.request(message)?;
        let response = timeout(self.config.deadline, self.rpc.prepare_settlement(request))
            .await
            .map_err(|_| unavailable())?
            .map_err(|_| unavailable())?
            .into_inner();
        validate_response(&response)?;
        Ok(response)
    }

    fn request<M: Message>(&self, message: M) -> Result<Request<M>, ControlGrpcError> {
        if message.encoded_len() > MAX_CONTROL_REQUEST_BYTES {
            return Err(ControlGrpcError::ResourceExhausted(
                "the control request exceeds the transport limit",
            ));
        }
        let workload = MetadataValue::try_from(self.config.workload_session_id.as_str())
            .map_err(|_| invalid_metadata())?;
        let producer = MetadataValue::try_from(self.config.producer_id.as_str())
            .map_err(|_| invalid_metadata())?;
        let mut request = Request::new(message);
        request.set_timeout(self.config.deadline);
        request
            .metadata_mut()
            .insert("x-elitea-workload-session", workload);
        request
            .metadata_mut()
            .insert("x-elitea-producer-id", producer);
        Ok(request)
    }
}

fn validate_response<M: Message>(response: &M) -> Result<(), ControlGrpcError> {
    if response.encoded_len() > MAX_CONTROL_RESPONSE_BYTES {
        return Err(ControlGrpcError::ResourceExhausted(
            "the control response exceeds the transport limit",
        ));
    }
    Ok(())
}

const fn unavailable() -> ControlGrpcError {
    ControlGrpcError::Unavailable("the control gRPC service is unavailable")
}

const fn invalid_metadata() -> ControlGrpcError {
    ControlGrpcError::InvalidConfiguration("the control gRPC metadata is malformed")
}

fn valid_metadata_value(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_METADATA_BYTES
        && value.is_ascii()
        && !value.as_bytes().contains(&b'\0')
        && !value.as_bytes().contains(&b'\r')
        && !value.as_bytes().contains(&b'\n')
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;

    use super::*;
    use crate::protocol::elitea::runtime::v1::{
        AuthorizeInvocationDispositionV1, BeginExecutionDispositionV1, ClaimReceiptV1,
        RuntimeErrorV1,
    };

    #[derive(Default)]
    struct FakeRpc {
        calls: Mutex<Vec<&'static str>>,
        metadata: Mutex<Vec<(String, String)>>,
        timeout: Mutex<Option<String>>,
        claim_response: Mutex<ClaimCommandResponseV1>,
        begin_response: Mutex<BeginExecutionResponseV1>,
        authorize_response: Mutex<AuthorizeInvocationResponseV1>,
        renew_response: Mutex<RenewLeaseResponseV1>,
        observe_response: Mutex<ObserveDesiredStateResponseV1>,
        settlement_response: Mutex<PrepareSettlementResponseV1>,
        fail: bool,
        delay: Option<Duration>,
    }

    impl FakeRpc {
        fn record<M>(&self, name: &'static str, request: &Request<M>) -> Result<(), Status> {
            self.calls.lock().expect("calls lock").push(name);
            let mut metadata = self.metadata.lock().expect("metadata lock");
            metadata.clear();
            for key in ["x-elitea-workload-session", "x-elitea-producer-id"] {
                metadata.push((
                    key.to_owned(),
                    request
                        .metadata()
                        .get(key)
                        .expect("required metadata")
                        .to_str()
                        .expect("ASCII metadata")
                        .to_owned(),
                ));
            }
            *self.timeout.lock().expect("timeout lock") = Some(
                request
                    .metadata()
                    .get("grpc-timeout")
                    .expect("gRPC timeout")
                    .to_str()
                    .expect("timeout text")
                    .to_owned(),
            );
            if self.fail {
                return Err(Status::unavailable("test-only detail"));
            }
            Ok(())
        }

        async fn delay(&self) {
            if let Some(delay) = self.delay {
                tokio::time::sleep(delay).await;
            }
        }
    }

    #[async_trait]
    impl ControlRpc for FakeRpc {
        async fn claim_command(
            &self,
            request: Request<ClaimCommandRequestV1>,
        ) -> Result<Response<ClaimCommandResponseV1>, Status> {
            self.record("claim", &request)?;
            self.delay().await;
            Ok(Response::new(
                self.claim_response.lock().expect("claim lock").clone(),
            ))
        }

        async fn begin_execution(
            &self,
            request: Request<BeginExecutionRequestV1>,
        ) -> Result<Response<BeginExecutionResponseV1>, Status> {
            self.record("begin", &request)?;
            self.delay().await;
            Ok(Response::new(
                self.begin_response.lock().expect("begin lock").clone(),
            ))
        }

        async fn authorize_invocation(
            &self,
            request: Request<AuthorizeInvocationRequestV1>,
        ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
            self.record("authorize", &request)?;
            self.delay().await;
            Ok(Response::new(
                self.authorize_response
                    .lock()
                    .expect("authorize lock")
                    .clone(),
            ))
        }

        async fn authorize_agent_model_checkpoint(
            &self,
            request: Request<AuthorizeAgentModelCheckpointRequestV1>,
        ) -> Result<Response<AuthorizeAgentModelCheckpointResponseV1>, Status> {
            self.record("checkpoint", &request)?;
            self.delay().await;
            let response = self
                .authorize_response
                .lock()
                .expect("authorize lock")
                .clone();
            Ok(Response::new(AuthorizeAgentModelCheckpointResponseV1 {
                disposition: response.disposition,
                rejection: response.rejection,
            }))
        }

        async fn renew_lease(
            &self,
            request: Request<RenewLeaseRequestV1>,
        ) -> Result<Response<RenewLeaseResponseV1>, Status> {
            self.record("renew", &request)?;
            self.delay().await;
            Ok(Response::new(
                self.renew_response.lock().expect("renew lock").clone(),
            ))
        }

        async fn observe_desired_state(
            &self,
            request: Request<ObserveDesiredStateRequestV1>,
        ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
            self.record("observe", &request)?;
            self.delay().await;
            Ok(Response::new(
                self.observe_response.lock().expect("observe lock").clone(),
            ))
        }

        async fn prepare_settlement(
            &self,
            request: Request<PrepareSettlementRequestV1>,
        ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
            self.record("settlement", &request)?;
            self.delay().await;
            Ok(Response::new(
                self.settlement_response
                    .lock()
                    .expect("settlement lock")
                    .clone(),
            ))
        }
    }

    #[tokio::test]
    async fn checkpoint_authorization_is_one_attempt_on_failure_or_timeout() {
        for delay in [false, true] {
            let rpc = FakeRpc {
                fail: !delay,
                delay: delay.then_some(Duration::from_secs(1)),
                ..FakeRpc::default()
            };
            let mut settings = config();
            settings.deadline = Duration::from_millis(10);
            let client = ControlGrpcClient::new(rpc, settings).expect("client");
            let result = client
                .authorize_agent_model_checkpoint(AuthorizeAgentModelCheckpointRequestV1::default())
                .await;
            assert!(matches!(result, Err(ControlGrpcError::Unavailable(_))));
            assert_eq!(client.rpc.calls.lock().expect("calls").len(), 1);
        }
    }

    fn config() -> ControlGrpcConfig {
        ControlGrpcConfig {
            deadline: Duration::from_millis(1_250),
            workload_session_id: "session-1".to_owned(),
            producer_id: "worker-1".to_owned(),
        }
    }

    #[tokio::test(flavor = "current_thread")]
    async fn every_control_method_is_one_bounded_attempt_with_exact_metadata() {
        let rpc = FakeRpc::default();
        *rpc.begin_response.lock().expect("begin lock") = BeginExecutionResponseV1 {
            disposition: BeginExecutionDispositionV1::AlreadyStarted as i32,
            rejection: None,
        };
        *rpc.authorize_response.lock().expect("authorize lock") = AuthorizeInvocationResponseV1 {
            disposition: AuthorizeInvocationDispositionV1::AlreadyAuthorized as i32,
            rejection: None,
        };
        let client = ControlGrpcClient::new(rpc, config()).expect("control client");

        client
            .claim_command(ClaimCommandRequestV1::default())
            .await
            .expect("claim");
        client
            .begin_execution(BeginExecutionRequestV1::default())
            .await
            .expect("begin");
        client
            .authorize_invocation(AuthorizeInvocationRequestV1::default())
            .await
            .expect("authorize");
        let checkpoint = client
            .authorize_agent_model_checkpoint(AuthorizeAgentModelCheckpointRequestV1::default())
            .await
            .expect("checkpoint");
        assert_eq!(
            checkpoint.disposition,
            AuthorizeInvocationDispositionV1::AlreadyAuthorized as i32
        );
        client
            .renew_lease(RenewLeaseRequestV1::default())
            .await
            .expect("renew");
        client
            .observe_desired_state(ObserveDesiredStateRequestV1::default())
            .await
            .expect("observe");
        client
            .prepare_settlement(PrepareSettlementRequestV1::default())
            .await
            .expect("settlement");

        assert_eq!(
            *client.rpc.calls.lock().expect("calls lock"),
            [
                "claim",
                "begin",
                "authorize",
                "checkpoint",
                "renew",
                "observe",
                "settlement"
            ]
        );
        assert_eq!(
            *client.rpc.metadata.lock().expect("metadata lock"),
            [
                (
                    "x-elitea-workload-session".to_owned(),
                    "session-1".to_owned()
                ),
                ("x-elitea-producer-id".to_owned(), "worker-1".to_owned())
            ]
        );
        assert_eq!(
            client.rpc.timeout.lock().expect("timeout lock").as_deref(),
            Some("1250000u")
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn unavailable_transport_is_data_free_and_never_retried() {
        let client = ControlGrpcClient::new(
            FakeRpc {
                fail: true,
                ..FakeRpc::default()
            },
            config(),
        )
        .expect("control client");
        assert_eq!(
            client.claim_command(ClaimCommandRequestV1::default()).await,
            Err(ControlGrpcError::Unavailable(
                "the control gRPC service is unavailable"
            ))
        );
        assert_eq!(*client.rpc.calls.lock().expect("calls lock"), ["claim"]);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn local_deadline_cancels_a_noncooperative_rpc_attempt() {
        let mut bounded = config();
        bounded.deadline = Duration::from_millis(1);
        let client = ControlGrpcClient::new(
            FakeRpc {
                delay: Some(Duration::from_secs(1)),
                ..FakeRpc::default()
            },
            bounded,
        )
        .expect("control client");
        assert_eq!(
            client.claim_command(ClaimCommandRequestV1::default()).await,
            Err(ControlGrpcError::Unavailable(
                "the control gRPC service is unavailable"
            ))
        );
        assert_eq!(*client.rpc.calls.lock().expect("calls lock"), ["claim"]);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn request_and_response_whole_message_limits_are_exact() {
        let client = ControlGrpcClient::new(FakeRpc::default(), config()).expect("control client");
        let mut at_limit = ClaimCommandRequestV1::default();
        set_string_for_exact_size(
            &mut at_limit,
            MAX_CONTROL_REQUEST_BYTES,
            |message, value| {
                message.workload_session_id = value;
            },
        );
        client
            .claim_command(at_limit)
            .await
            .expect("request at limit");

        let mut over_limit = ClaimCommandRequestV1::default();
        set_string_for_exact_size(
            &mut over_limit,
            MAX_CONTROL_REQUEST_BYTES + 1,
            |message, value| message.workload_session_id = value,
        );
        assert_eq!(
            client.claim_command(over_limit).await,
            Err(ControlGrpcError::ResourceExhausted(
                "the control request exceeds the transport limit"
            ))
        );
        assert_eq!(client.rpc.calls.lock().expect("calls lock").len(), 1);

        let mut response_at_limit = ClaimCommandResponseV1 {
            receipt: Some(ClaimReceiptV1::default()),
            rejection: None,
        };
        set_response_for_exact_size(&mut response_at_limit, MAX_CONTROL_RESPONSE_BYTES);
        *client.rpc.claim_response.lock().expect("claim lock") = response_at_limit.clone();
        assert_eq!(
            client
                .claim_command(ClaimCommandRequestV1::default())
                .await
                .expect("response at limit"),
            response_at_limit
        );

        let mut response_over_limit = ClaimCommandResponseV1 {
            receipt: None,
            rejection: Some(RuntimeErrorV1::default()),
        };
        set_response_for_exact_size(&mut response_over_limit, MAX_CONTROL_RESPONSE_BYTES + 1);
        *client.rpc.claim_response.lock().expect("claim lock") = response_over_limit;
        assert_eq!(
            client.claim_command(ClaimCommandRequestV1::default()).await,
            Err(ControlGrpcError::ResourceExhausted(
                "the control response exceeds the transport limit"
            ))
        );
    }

    #[test]
    fn metadata_and_deadline_configuration_are_fail_closed() {
        for mut invalid in [
            ControlGrpcConfig {
                deadline: Duration::ZERO,
                ..config()
            },
            ControlGrpcConfig {
                deadline: MAX_CONTROL_DEADLINE + Duration::from_nanos(1),
                ..config()
            },
            ControlGrpcConfig {
                workload_session_id: String::new(),
                ..config()
            },
            ControlGrpcConfig {
                producer_id: "line\nfeed".to_owned(),
                ..config()
            },
            ControlGrpcConfig {
                producer_id: "é".to_owned(),
                ..config()
            },
            ControlGrpcConfig {
                producer_id: "x".repeat(257),
                ..config()
            },
        ] {
            assert!(matches!(
                ControlGrpcClient::new(FakeRpc::default(), invalid.clone()),
                Err(ControlGrpcError::InvalidConfiguration(_))
            ));
            invalid.producer_id.clear();
        }
    }

    fn set_string_for_exact_size<M: Message>(
        message: &mut M,
        size: usize,
        mut set_value: impl FnMut(&mut M, String),
    ) {
        let (mut low, mut high) = (0usize, size);
        while low <= high {
            let middle = low + (high - low) / 2;
            set_value(message, "x".repeat(middle));
            match message.encoded_len().cmp(&size) {
                std::cmp::Ordering::Less => low = middle + 1,
                std::cmp::Ordering::Greater => high = middle.saturating_sub(1),
                std::cmp::Ordering::Equal => return,
            }
        }
        panic!("cannot construct exact-size control message");
    }

    fn set_response_for_exact_size(message: &mut ClaimCommandResponseV1, size: usize) {
        if message.rejection.is_none() {
            message.rejection = Some(RuntimeErrorV1::default());
        }
        set_string_for_exact_size(message, size, |message, value| {
            message.rejection.as_mut().expect("rejection").safe_message = value;
        });
    }
}
