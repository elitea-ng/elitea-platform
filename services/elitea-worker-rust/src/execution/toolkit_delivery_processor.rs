//! Whole-delivery lifecycle for one external-MCP direct toolkit read.

use std::fmt;
use std::sync::Arc;

use tracing::Instrument as _;

use super::agent_lease::{
    ClaimLeaseActivation, ClaimLeaseError, ClaimLeaseMonitor, UnixMillisClock,
};
use super::agent_preparation::{AgentInputMaterializer, AgentPreparationConfig};
use super::configuration_validation;
use super::invocation_admission::{InvocationAdmission, InvocationAdmissionError};
use super::output_delivery::{
    AgentOutputPreflight, AgentOutputPreflightError, AgentTerminalRecoveryConfig,
    PreparedAgentOutput, ToolkitTerminalReplay,
};
use super::toolkit_delivery::{
    FreshToolkitDelivery, ToolkitDeliveryError, ToolkitDeliveryRoute, ToolkitDeliveryRouter,
    ToolkitOutputRecoveryDelivery,
};
use super::toolkit_output::{
    EmptyToolkitOutputRecovery, ToolkitOutputPreflightOutcome, ToolkitTerminalError,
    ToolkitTerminalRecovery, prepare_empty_toolkit_recovery, prepare_toolkit_output,
    replay_toolkit_terminal_with_replacement,
};
use crate::indexing::tools::LazyStepbackModel;
use crate::indexing::{
    ChatEntries, ChatEntrySource, ChatUnavailable, IndexIngestRuntime, IndexRun, IndexRunFailure,
    Interruption, interrupt_summary, run_index,
};
use crate::protocol::ProtocolError;
use crate::protocol::command::{ToolkitCommandKind, VerifiedToolkitExecuteReadCommand};
use crate::protocol::control::{
    AgentControlClient, AgentExecutionOutputAuthority, AgentOutputRecoveryKind,
    AuthorizedToolkitExecution, BeginAgentExecution, DesiredExecutionState,
    InvocationAuthorizationDecision, LeaseMonitoredAgentExecution, ToolkitInvocationPayload,
};
use crate::protocol::elitea::runtime::v1::{
    ConfigurationValidationResultV1, DigestV1, ExecutionInputEntryV1, IndexIngestCommandV1,
    IndexIngestInputBindingV1, IndexIngestResultV1, IndexIngestSummaryV1,
    ToolkitAuthorizationRequiredV1, ToolkitAvailableToolsResultV1, ToolkitCallToolResultV1,
    ToolkitCallToolStatusV1, ToolkitCallToolSummaryV1, ToolkitExecuteReadResultV1,
    worker_command_v1,
};
use crate::protocol::output::{RuntimeFailureKind, ToolkitExecuteReadTerminalOutput};
use crate::toolkits::{
    DirectToolkitRequest, DirectToolkitRequestErrorCode, DirectToolkitRuntime,
    DirectToolkitRuntimeErrorCode, IndexToolSupport, IndexToolsWithheld,
};
use crate::transport::command_bus::{
    CommandBusError, CommandDelivery, CommandRetirementClient, CommandRetirer,
};
use crate::transport::office_host::{ClaimOfficeHost, NoRoundTripImagesHost, OfficeRuntime};
use crate::transport::platform_client::PlatformClient;
use crate::transport::platform_writer::HostOnlyPlatformWriter;
use crate::transport::runtime_context::RuntimeContextError;
use crate::transport::{ControlRpc, InputContentError, MaterializedInput};
use crate::validation::SettingsRefusal;

/// What a direct toolkit call is lent for its Office host.
#[derive(Debug, PartialEq, Eq)]
enum DirectLending {
    /// A host that does nothing: no claim redemption, no platform call.
    NoRoundTrip,
    /// The claim's PAT redeemed for the platform object API.
    Claim,
    /// Nothing: the toolkit is unsupported on this path.
    None,
}

/// The families whose tools act through the Office host (ADR-0032).
fn is_office_family(toolkit_type: &str) -> bool {
    matches!(toolkit_type, "pptx" | "docx" | "xlsx")
}

/// What to lend a direct call of `toolkit_type`. `imagegen`'s tools are all
/// effectful and refused on this path, so it never needs the claim; the Office
/// families (`pptx`, `docx`, `xlsx`) read objects, so a call needs it (when
/// this worker has the Office runtime); `sharepoint` is lent the host only
/// for the tools that read text, whose document reader it carries. Its
/// uploads are effects and are refused here.
///
/// A discovery (`toolkit.available_tools.v1`) lists tools and runs none, so
/// it needs no claim: every family that is built on a host (Office,
/// `imagegen`) and `artifact` are lent the do-nothing host, whatever this
/// worker carries. That is what keeps the tool list reachable when a call
/// would be refused (a deployment with no Office runtime still lists them).
fn direct_lending(
    toolkit_type: &str,
    tool_name: &str,
    has_office_runtime: bool,
    discovery: bool,
) -> DirectLending {
    if discovery && (is_office_family(toolkit_type) || toolkit_type == "artifact") {
        return DirectLending::NoRoundTrip;
    }
    match toolkit_type {
        "imagegen" => DirectLending::NoRoundTrip,
        office if is_office_family(office) && has_office_runtime => DirectLending::Claim,
        "sharepoint"
            if has_office_runtime
                && elitea_agent_runtime::toolkits::sharepoint_tool_reads_text(tool_name) =>
        {
            DirectLending::Claim
        }
        _ => DirectLending::None,
    }
}

#[cfg(test)]
mod direct_lending_tests {
    use super::{DirectLending, direct_lending};

    #[test]
    fn imagegen_is_lent_without_redeeming_the_claim() {
        // Whether or not the worker has an Office runtime: no round trip.
        for runtime in [true, false] {
            assert_eq!(
                direct_lending("imagegen", "", runtime, false),
                DirectLending::NoRoundTrip
            );
        }
        assert_eq!(github_none(), DirectLending::None);
        // SharePoint is lent the host for its text readers only.
        assert_eq!(
            direct_lending("sharepoint", "read_document", true, false),
            DirectLending::Claim
        );
        assert_eq!(
            direct_lending("sharepoint", "read_document", false, false),
            DirectLending::None
        );
        for tool in ["get_lists", "upload_file", "onenote_get_page_content", ""] {
            assert_eq!(
                direct_lending("sharepoint", tool, true, false),
                DirectLending::None,
                "{tool}"
            );
        }
    }

    fn github_none() -> DirectLending {
        direct_lending("github", "", true, false)
    }

    /// D2: `docx` and `xlsx` were refused as an unsupported family because
    /// only `pptx` was lent the host.
    #[test]
    fn every_office_family_is_lent_the_claim_for_a_call() {
        for family in ["pptx", "docx", "xlsx"] {
            assert_eq!(
                direct_lending(family, "read_document", true, false),
                DirectLending::Claim,
                "{family}"
            );
            assert_eq!(
                direct_lending(family, "read_document", false, false),
                DirectLending::None,
                "{family} without an Office runtime"
            );
        }
    }

    /// D2: a discovery lists tools and runs none, so it redeems nothing and
    /// needs no Office runtime; `artifact` was a 500 here for want of a lent
    /// authority.
    #[test]
    fn discovery_is_lent_the_do_nothing_host_without_a_claim() {
        for family in ["pptx", "docx", "xlsx", "artifact", "imagegen"] {
            for runtime in [true, false] {
                assert_eq!(
                    direct_lending(family, "", runtime, true),
                    DirectLending::NoRoundTrip,
                    "{family} runtime={runtime}"
                );
            }
        }
        assert_eq!(
            direct_lending("github", "", true, true),
            DirectLending::None
        );
    }
}

pub(super) struct ToolkitDeliveryProcessor<R, RC, T, K, I> {
    router: ToolkitDeliveryRouter<R, RC>,
    output: AgentOutputPreflight,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<CommandRetirer<RC>>,
    replay: Arc<T>,
    input: Arc<I>,
    clock: Arc<K>,
    admission: InvocationAdmission,
    preparation: AgentPreparationConfig,
    terminal_recovery: AgentTerminalRecoveryConfig,
    runtime: DirectToolkitRuntime,
    /// The claim's PAT redemption, for an index run's gateway and platform
    /// calls.
    platform: Arc<PlatformClient>,
    /// `index.ingest.v1` (ADR-0030 I1); `None` on a deployment without a
    /// vector target, which refuses the capability.
    index_runtime: Option<Arc<IndexIngestRuntime>>,
    /// The platform object client, for a `toolkit.call_tool.v1` of a toolkit
    /// that reads platform objects (`pptx`); `None` leaves it unsupported on
    /// this path.
    office: Option<Arc<OfficeRuntime>>,
}

impl<R, RC, T, K, I> ToolkitDeliveryProcessor<R, RC, T, K, I>
where
    R: ControlRpc + 'static,
    RC: CommandRetirementClient + 'static,
    T: ToolkitTerminalReplay + 'static,
    K: UnixMillisClock,
    I: AgentInputMaterializer + 'static,
{
    #[allow(clippy::too_many_arguments)]
    pub(super) fn new(
        output: AgentOutputPreflight,
        control: Arc<AgentControlClient<R>>,
        retirer: Arc<CommandRetirer<RC>>,
        replay: Arc<T>,
        input: Arc<I>,
        clock: Arc<K>,
        admission: InvocationAdmission,
        preparation: AgentPreparationConfig,
        terminal_recovery: AgentTerminalRecoveryConfig,
        runtime: DirectToolkitRuntime,
        platform: Arc<PlatformClient>,
        index_runtime: Option<Arc<IndexIngestRuntime>>,
    ) -> Self {
        Self {
            router: ToolkitDeliveryRouter::new(Arc::clone(&control), Arc::clone(&retirer)),
            output,
            control,
            retirer,
            replay,
            input,
            clock,
            admission,
            preparation,
            terminal_recovery,
            runtime,
            platform,
            index_runtime,
            office: None,
        }
    }

    /// Lend the Office runtime to direct toolkit calls.
    #[must_use]
    pub(super) fn with_office(mut self, runtime: Option<Arc<OfficeRuntime>>) -> Self {
        self.office = runtime;
        self
    }

    pub(super) async fn process_verified(
        &self,
        delivery: CommandDelivery,
        verified: VerifiedToolkitExecuteReadCommand,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        let route = self
            .router
            .route(delivery, verified, self.clock.now_unix_millis())
            .await
            .map_err(ToolkitProcessError::Delivery)?;
        match route {
            ToolkitDeliveryRoute::Fresh(fresh) => Box::pin(self.process_fresh(*fresh)).await,
            ToolkitDeliveryRoute::OutputRecovery(recovery) => {
                Box::pin(self.process_output_recovery(*recovery)).await
            }
            ToolkitDeliveryRoute::RetryLaterNoAck => Ok(ToolkitProcessOutcome::retained(
                "toolkit_delivery.retry_later",
                true,
            )),
            ToolkitDeliveryRoute::Completed => Ok(ToolkitProcessOutcome::completed(
                "toolkit_delivery.redelivery_retired",
            )),
        }
    }

    async fn process_fresh(
        &self,
        fresh: FreshToolkitDelivery,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        match prepare_toolkit_output(&self.output, fresh, self.clock.now_unix_millis())
            .await
            .map_err(ToolkitProcessError::OutputPreflight)?
        {
            ToolkitOutputPreflightOutcome::Empty(empty) => {
                let (fresh, output) = empty.into_parts();
                Box::pin(self.execute_fresh(fresh, output)).await
            }
            ToolkitOutputPreflightOutcome::Terminal(recovery) => {
                Box::pin(self.recover_terminal(*recovery)).await?;
                Ok(ToolkitProcessOutcome::completed(
                    "toolkit_delivery.accepted_terminal_retired",
                ))
            }
        }
    }

    async fn process_output_recovery(
        &self,
        recovery: ToolkitOutputRecoveryDelivery,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        let failure = match (
            recovery.recovery().kind(),
            recovery.recovery().desired_state(),
        ) {
            (AgentOutputRecoveryKind::AmbiguousInvocation, DesiredExecutionState::Running) => {
                RuntimeFailureKind::Internal
            }
            (AgentOutputRecoveryKind::Running, DesiredExecutionState::Cancelled) => {
                RuntimeFailureKind::Cancelled
            }
            _ => {
                return Ok(ToolkitProcessOutcome::retained(
                    "toolkit_delivery.output_recovery_pending",
                    true,
                ));
            }
        };
        let Some(recovery) = prepare_empty_toolkit_recovery(&self.output, recovery)
            .await
            .map_err(ToolkitProcessError::OutputPreflight)?
        else {
            return Ok(ToolkitProcessOutcome::retained(
                "toolkit_delivery.output_recovery_pending",
                true,
            ));
        };
        Box::pin(self.reconcile_empty_recovery(recovery, failure)).await?;
        Ok(ToolkitProcessOutcome::completed(
            "toolkit_delivery.ambiguous_execution_reconciled",
        ))
    }

    async fn execute_fresh(
        &self,
        fresh: FreshToolkitDelivery,
        output: PreparedAgentOutput,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        let reservation = match self.admission.reserve().await {
            Ok(reservation) => reservation,
            Err(error) if error.retryable() => {
                return Ok(ToolkitProcessOutcome::retained(
                    "toolkit_delivery.admission_retry",
                    true,
                ));
            }
            Err(error) => return Err(ToolkitProcessError::Admission(error)),
        };
        let (delivery, verified, claim) = fresh.into_parts();
        let preparing = match self.control.begin_agent_execution(claim).await {
            Ok(BeginAgentExecution::Preparing(preparing)) => preparing,
            Ok(BeginAgentExecution::AlreadyStarted(_)) => {
                return Ok(ToolkitProcessOutcome::retained(
                    "toolkit_delivery.already_started",
                    true,
                ));
            }
            Err(error) => return Err(ToolkitProcessError::Control(error)),
        };
        let mut lease = ClaimLeaseMonitor::start(
            Arc::clone(&self.control),
            preparing.start_lease_monitor(),
            Arc::clone(&self.clock),
            self.preparation.lease_config(),
        );
        let execution = match lease.activate().await {
            ClaimLeaseActivation::Active(execution) => execution,
            ClaimLeaseActivation::Inactive { execution, error } => {
                let output_authority = execution.into_output_authority();
                let failure = lease_failure(&error);
                let outcome = Box::pin(self.publish_fresh_terminal(
                    delivery,
                    verified,
                    output_authority,
                    output,
                    lease,
                    ToolkitExecuteReadTerminalOutput::Failure(failure),
                ))
                .await;
                drop(reservation);
                outcome?;
                return Ok(ToolkitProcessOutcome::completed(
                    "toolkit_delivery.activation_terminal_retired",
                ));
            }
            ClaimLeaseActivation::Unavailable(error) => {
                return Err(ToolkitProcessError::Lease(error));
            }
        };

        let outcome =
            Box::pin(self.execute_preparing(delivery, verified, execution, output, lease)).await;
        drop(reservation);
        outcome
    }

    async fn execute_preparing(
        &self,
        delivery: CommandDelivery,
        verified: VerifiedToolkitExecuteReadCommand,
        execution: LeaseMonitoredAgentExecution,
        output: PreparedAgentOutput,
        mut lease: ClaimLeaseMonitor,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        if verified.kind() == ToolkitCommandKind::ConfigurationValidate {
            return Box::pin(self.execute_validation(delivery, verified, execution, output, lease))
                .await;
        }
        if verified.kind() == ToolkitCommandKind::IndexIngest {
            return Box::pin(
                self.execute_index_ingest(delivery, verified, execution, output, lease),
            )
            .await;
        }
        let request = match self
            .materialize_request(&verified, &execution, &mut lease)
            .await
        {
            Ok(request) => request,
            Err(failure) => {
                Box::pin(self.publish_fresh_terminal(
                    delivery,
                    verified,
                    execution.into_output_authority(),
                    output,
                    lease,
                    ToolkitExecuteReadTerminalOutput::Failure(failure),
                ))
                .await?;
                return Ok(ToolkitProcessOutcome::completed(
                    "toolkit_delivery.preparation_terminal_retired",
                ));
            }
        };
        // Record possible submission before entering any toolkit operation.
        // An uncertain authorization response must not publish a terminal result
        // or enter provider code; a replacement owns reconciliation.
        let execution = match self
            .control
            .authorize_agent_invocation(execution.bind_invocation(ToolkitInvocationPayload))
            .await
        {
            InvocationAuthorizationDecision::AuthorizedNow(execution) => *execution,
            InvocationAuthorizationDecision::AlreadyAuthorized(terminal)
            | InvocationAuthorizationDecision::Rejected(terminal) => {
                let failure = terminal.cause.runtime_failure_kind();
                Box::pin(self.publish_fresh_terminal(
                    delivery,
                    verified,
                    terminal.output,
                    output,
                    lease,
                    ToolkitExecuteReadTerminalOutput::Failure(failure),
                ))
                .await?;
                return Ok(ToolkitProcessOutcome::completed(
                    "toolkit_delivery.authorization_terminal_retired",
                ));
            }
            InvocationAuthorizationDecision::Unknown(unknown) => {
                lease.close().await.map_err(ToolkitProcessError::Lease)?;
                return Err(ToolkitProcessError::Control(unknown.error));
            }
        };
        let outcome = self
            .execute_authorized(&verified, &execution, &mut lease, &request)
            .await;
        let output_authority = execution.into_output_authority();
        let terminal = match outcome {
            Ok(result) => result,
            Err(failure) => ToolkitExecuteReadTerminalOutput::Failure(failure),
        };
        let published = Box::pin(self.publish_fresh_terminal(
            delivery,
            verified,
            output_authority,
            output,
            lease,
            terminal,
        ))
        .await;
        published?;
        Ok(ToolkitProcessOutcome::completed(
            "toolkit_delivery.executed_retired",
        ))
    }

    /// Pure schema validation. It has no effect to guard, so unlike a tool
    /// run it never records an invocation authorization: a worker lost
    /// mid-validation leaves the job re-claimable (`BeginExecution` answers
    /// `StartedNow` again while the invocation is still `PREPARING`) and the
    /// repeat is harmless.
    async fn execute_validation(
        &self,
        delivery: CommandDelivery,
        verified: VerifiedToolkitExecuteReadCommand,
        execution: LeaseMonitoredAgentExecution,
        output: PreparedAgentOutput,
        mut lease: ClaimLeaseMonitor,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        let terminal = match self
            .validate_configuration(&verified, &execution, &mut lease)
            .await
        {
            Ok(result) => {
                ToolkitExecuteReadTerminalOutput::ConfigurationValidation(Box::new(result))
            }
            Err(failure) => ToolkitExecuteReadTerminalOutput::Failure(failure),
        };
        Box::pin(self.publish_fresh_terminal(
            delivery,
            verified,
            execution.into_output_authority(),
            output,
            lease,
            terminal,
        ))
        .await?;
        Ok(ToolkitProcessOutcome::completed(
            "toolkit_delivery.validation_retired",
        ))
    }

    async fn validate_configuration(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        execution: &LeaseMonitoredAgentExecution,
        lease: &mut ClaimLeaseMonitor,
    ) -> Result<ConfigurationValidationResultV1, RuntimeFailureKind> {
        // Binding first, as the Python handler does: a command this worker
        // cannot validate must not cost a settings fetch.
        let rules = configuration_validation::select_rules(verified)?;
        let settings = self.fetch_input(verified, execution, lease).await?;
        let issues = rules.evaluate(settings.as_bytes()).map_err(|refusal| {
            record_toolkit_execution_failure(
                "settings_validation",
                match refusal {
                    SettingsRefusal::InvalidInput => "configuration_validation.invalid_input",
                    SettingsRefusal::ResourceExhausted => {
                        "configuration_validation.resource_exhausted"
                    }
                },
                false,
            );
            refusal.failure()
        })?;
        lease
            .check_now()
            .await
            .map_err(|error| lease_failure(&error))?;
        if deadline_exceeded(verified, self.clock.as_ref()) {
            record_toolkit_deadline("post_validation_deadline");
            return Err(RuntimeFailureKind::DeadlineExceeded);
        }
        configuration_validation::bind_result(
            verified,
            execution.input_bundle_ref(),
            execution.request_entry(),
            &issues,
        )
    }

    /// `index.ingest.v1` (ADR-0030 I1). The bundle's entries are fetched,
    /// the invocation is authorized (a run has effects: vectors are written
    /// and deleted), the claim's PAT is redeemed for the gateway and the
    /// platform object API, and the run executes as the effect-owning
    /// phase: awaited directly, never under the lease's pre-invocation race,
    /// which would drop it inside a store call. The run watches the lease
    /// itself and stops at a document boundary. Its typed summary is the
    /// terminal, Stop and the deadline included once the store was touched
    /// (the summary then describes the store and settles cancelled or
    /// failed); before that they are runtime failures, as is a malformed
    /// input. A lost lease publishes nothing: another worker may own the
    /// claim.
    async fn execute_index_ingest(
        &self,
        delivery: CommandDelivery,
        verified: VerifiedToolkitExecuteReadCommand,
        execution: LeaseMonitoredAgentExecution,
        output: PreparedAgentOutput,
        mut lease: ClaimLeaseMonitor,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        let Some(runtime) = self.index_runtime.as_ref() else {
            tracing::warn!(
                event = "index_ingest_unsupported",
                "this worker has no vector_target, so index.ingest.v1 is refused"
            );
            Box::pin(self.publish_fresh_terminal(
                delivery,
                verified,
                execution.into_output_authority(),
                output,
                lease,
                ToolkitExecuteReadTerminalOutput::Failure(
                    RuntimeFailureKind::UnsupportedCapability,
                ),
            ))
            .await?;
            return Ok(ToolkitProcessOutcome::completed(
                "toolkit_delivery.index_unsupported_retired",
            ));
        };
        let inputs = match self
            .materialize_index_inputs(&verified, &execution, &mut lease)
            .await
        {
            Ok(inputs) => inputs,
            Err(failure) => {
                Box::pin(self.publish_fresh_terminal(
                    delivery,
                    verified,
                    execution.into_output_authority(),
                    output,
                    lease,
                    ToolkitExecuteReadTerminalOutput::Failure(failure),
                ))
                .await?;
                return Ok(ToolkitProcessOutcome::completed(
                    "toolkit_delivery.preparation_terminal_retired",
                ));
            }
        };
        let authorized = match self
            .control
            .authorize_agent_invocation(execution.bind_invocation(ToolkitInvocationPayload))
            .await
        {
            InvocationAuthorizationDecision::AuthorizedNow(execution) => *execution,
            InvocationAuthorizationDecision::AlreadyAuthorized(terminal)
            | InvocationAuthorizationDecision::Rejected(terminal) => {
                let failure = terminal.cause.runtime_failure_kind();
                Box::pin(self.publish_fresh_terminal(
                    delivery,
                    verified,
                    terminal.output,
                    output,
                    lease,
                    ToolkitExecuteReadTerminalOutput::Failure(failure),
                ))
                .await?;
                return Ok(ToolkitProcessOutcome::completed(
                    "toolkit_delivery.authorization_terminal_retired",
                ));
            }
            InvocationAuthorizationDecision::Unknown(unknown) => {
                lease.close().await.map_err(ToolkitProcessError::Lease)?;
                return Err(ToolkitProcessError::Control(unknown.error));
            }
        };
        let outcome = self
            .run_index_ingest(&verified, &authorized, &mut lease, runtime, &inputs)
            .await;
        let output_authority = authorized.into_output_authority();
        let terminal = match index_terminal(outcome) {
            Ok(terminal) => terminal,
            Err(error) => {
                close_lease_logged(lease).await;
                return Err(ToolkitProcessError::Lease(error));
            }
        };
        Box::pin(self.publish_fresh_terminal(
            delivery,
            verified,
            output_authority,
            output,
            lease,
            terminal,
        ))
        .await?;
        Ok(ToolkitProcessOutcome::completed(
            "toolkit_delivery.index_executed_retired",
        ))
    }

    async fn materialize_index_inputs(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        execution: &LeaseMonitoredAgentExecution,
        lease: &mut ClaimLeaseMonitor,
    ) -> Result<IndexInputs, RuntimeFailureKind> {
        let Some(worker_command_v1::CapabilityCommand::IndexIngest(index)) =
            verified.command().capability_command.as_ref()
        else {
            return Err(RuntimeFailureKind::InvalidInput);
        };
        // The toolkit configuration is the claim's request entry.
        let toolkit_configuration = self.fetch_input(verified, execution, lease).await?;
        let tool_parameters = self
            .fetch_entry(execution, lease, &index.tool_parameters_entry_id)
            .await?;
        // Parsed once for the claim; the run reuses it. Unreadable arguments
        // leave `None` and the run reports them. The chat model entries
        // (ADR-0030 I4) are NOT fetched here: the run asks for them, both
        // together, the first time a step needs the chat model.
        let parsed = crate::indexing::inputs::ParsedArgs::parse(tool_parameters.as_bytes()).ok();
        let embedding_binding = match index.embedding_binding.as_ref() {
            Some(binding) => Some(
                self.fetch_entry(execution, lease, &binding.entry_id)
                    .await?,
            ),
            None => None,
        };
        Ok(IndexInputs {
            toolkit_configuration,
            tool_parameters,
            parsed,
            embedding_binding,
        })
    }

    async fn fetch_entry(
        &self,
        execution: &LeaseMonitoredAgentExecution,
        lease: &mut ClaimLeaseMonitor,
        entry_id: &str,
    ) -> Result<MaterializedInput, RuntimeFailureKind> {
        lease
            .run_pre_invocation(self.input.materialize_entry(execution, entry_id))
            .await
            .map_err(|error| lease_failure(&error))?
            .map_err(|error| {
                record_toolkit_input_failure("index_input_materialization", &error);
                input_failure(&error)
            })
    }

    async fn run_index_ingest(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        authorized: &AuthorizedToolkitExecution,
        lease: &mut ClaimLeaseMonitor,
        runtime: &IndexIngestRuntime,
        inputs: &IndexInputs,
    ) -> IndexRunEnd {
        let command = verified.command();
        let Some(worker_command_v1::CapabilityCommand::IndexIngest(index)) =
            command.capability_command.as_ref()
        else {
            return IndexRunEnd::Failure(RuntimeFailureKind::InvalidInput);
        };
        if deadline_exceeded(verified, self.clock.as_ref()) {
            record_toolkit_deadline("pre_index_deadline");
            return IndexRunEnd::Failure(RuntimeFailureKind::DeadlineExceeded);
        }
        // Redeeming the context has no effect on the store: it may still be
        // dropped by a Stop.
        let context = match lease
            .run_pre_invocation(
                self.platform
                    .redeem_elitea_context(authorized.runtime_context()),
            )
            .await
        {
            Ok(Ok(context)) => context,
            Ok(Err(error)) => return IndexRunEnd::Failure(runtime_context_failure(&error)),
            Err(error) => return IndexRunEnd::Failure(lease_failure(&error)),
        };
        let execution = authorized.execution();
        let probe = lease.state_probe();
        let clock = Arc::clone(&self.clock);
        let now = move || clock.now_unix_millis();
        // The chat entries are fetched lazily, together, the first time a
        // step needs the chat model; a run that never does fetches nothing.
        let wants_chat = inputs
            .parsed
            .as_ref()
            .is_some_and(crate::indexing::inputs::ParsedArgs::needs_chat_model);
        let chat_entries = (wants_chat
            && !(index.llm_model_entry_id.is_empty()
                && index.llm_configuration_entry_id.is_empty()))
        .then(|| ClaimChatEntries {
            input: self.input.as_ref(),
            execution,
            model_entry_id: &index.llm_model_entry_id,
            configuration_entry_id: &index.llm_configuration_entry_id,
        });
        let run = IndexRun {
            command: index,
            toolkit_configuration: inputs.toolkit_configuration.as_bytes(),
            tool_parameters: inputs.tool_parameters.as_bytes(),
            parsed: inputs.parsed.as_ref(),
            embedding_binding: inputs
                .embedding_binding
                .as_ref()
                .map(MaterializedInput::as_bytes),
            llm_model: None,
            llm_configuration: None,
            chat_entries: chat_entries
                .as_ref()
                .map(|entries| entries as &dyn ChatEntrySource),
            vector_token: execution.vector_token(),
            context: &context,
            lease: &probe,
            deadline_unix_millis: command.deadline_unix_millis,
            now_unix_millis: &now,
        };
        // The run owns the store effects: it is awaited to its end and
        // observes Stop, the deadline and lease loss itself, between
        // documents. A Stop or deadline that arrives after it returns is
        // folded into the terminal by `publish_fresh_terminal`.
        let summary = match run_index(runtime, run).await {
            Ok(summary) => summary,
            Err(IndexRunFailure::Cancelled) => {
                return IndexRunEnd::Failure(RuntimeFailureKind::Cancelled);
            }
            Err(IndexRunFailure::DeadlineExceeded) => {
                record_toolkit_deadline("index_deadline");
                return IndexRunEnd::Failure(RuntimeFailureKind::DeadlineExceeded);
            }
            Err(IndexRunFailure::LeaseLost) => {
                return IndexRunEnd::LeaseLost(lease.ensure_running().err().unwrap_or(
                    ClaimLeaseError::LeaseLost("the claim lease was lost during the index run"),
                ));
            }
            Err(IndexRunFailure::InvalidInput(reason)) => {
                tracing::warn!(
                    event = "index_ingest_input_invalid",
                    reason,
                    "an index input entry is malformed"
                );
                return IndexRunEnd::Failure(RuntimeFailureKind::InvalidInput);
            }
        };
        match bind_index_result(execution, index, summary) {
            Ok(result) => IndexRunEnd::Result(Box::new(result)),
            Err(failure) => IndexRunEnd::Failure(failure),
        }
    }

    async fn materialize_request(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        execution: &LeaseMonitoredAgentExecution,
        lease: &mut ClaimLeaseMonitor,
    ) -> Result<DirectToolkitRequest, RuntimeFailureKind> {
        let materialized = self.fetch_input(verified, execution, lease).await?;
        if verified.kind() != ToolkitCommandKind::ExecuteRead {
            return self
                .materialize_shared_request(verified, execution, lease, materialized.as_bytes())
                .await;
        }
        let request = match DirectToolkitRequest::parse(materialized.as_bytes()) {
            Ok(request) => request,
            Err(error) => {
                let code = error.code();
                record_toolkit_execution_failure(
                    "request_validation",
                    request_error_code(code),
                    false,
                );
                return Err(request_failure(code));
            }
        };
        Ok(request)
    }

    async fn fetch_input(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        execution: &LeaseMonitoredAgentExecution,
        lease: &mut ClaimLeaseMonitor,
    ) -> Result<MaterializedInput, RuntimeFailureKind> {
        if deadline_exceeded(verified, self.clock.as_ref()) {
            record_toolkit_deadline("pre_materialization_deadline");
            return Err(RuntimeFailureKind::DeadlineExceeded);
        }
        let materialized = match lease
            .run_pre_invocation(self.input.materialize(execution))
            .await
        {
            Ok(Ok(materialized)) => materialized,
            Ok(Err(error)) => {
                record_toolkit_input_failure("input_materialization", &error);
                return Err(input_failure(&error));
            }
            Err(error) => {
                record_toolkit_execution_failure(
                    "input_materialization_lease",
                    error.code().as_str(),
                    error.retryable(),
                );
                return Err(lease_failure(&error));
            }
        };
        Ok(materialized)
    }

    async fn execute_authorized(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        authorized: &AuthorizedToolkitExecution,
        lease: &mut ClaimLeaseMonitor,
        request: &DirectToolkitRequest,
    ) -> Result<ToolkitExecuteReadTerminalOutput, RuntimeFailureKind> {
        let execution = authorized.execution();
        if verified.kind() != ToolkitCommandKind::ExecuteRead {
            return self
                .execute_shared(verified, authorized, lease, request)
                .await;
        }
        if deadline_exceeded(verified, self.clock.as_ref()) {
            record_toolkit_deadline("pre_invocation_deadline");
            return Err(RuntimeFailureKind::DeadlineExceeded);
        }
        let result = match lease
            .run_pre_invocation(self.runtime.execute(
                request,
                &verified.command().execution_id,
                &verified.command().command_id,
            ))
            .await
        {
            Ok(Ok(result)) => result,
            Ok(Err(error)) => {
                let code = error.code();
                record_toolkit_execution_failure(
                    "toolkit_invocation",
                    runtime_error_code(code),
                    error.retryable(),
                );
                return Err(runtime_failure(code));
            }
            Err(error) => {
                record_toolkit_execution_failure(
                    "toolkit_invocation_lease",
                    error.code().as_str(),
                    error.retryable(),
                );
                return Err(lease_failure(&error));
            }
        };
        if let Err(error) = lease.check_now().await {
            record_toolkit_execution_failure(
                "post_invocation_lease",
                error.code().as_str(),
                error.retryable(),
            );
            return Err(lease_failure(&error));
        }
        if deadline_exceeded(verified, self.clock.as_ref()) {
            record_toolkit_deadline("post_invocation_deadline");
            return Err(RuntimeFailureKind::DeadlineExceeded);
        }
        match bind_result(execution, request, &result) {
            Ok(result) => Ok(ToolkitExecuteReadTerminalOutput::Result(Box::new(result))),
            Err(failure) => {
                record_toolkit_execution_failure(
                    "result_binding",
                    runtime_failure_code(failure),
                    false,
                );
                Err(failure)
            }
        }
    }

    async fn materialize_shared_request(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        execution: &LeaseMonitoredAgentExecution,
        lease: &mut ClaimLeaseMonitor,
        settings: &[u8],
    ) -> Result<DirectToolkitRequest, RuntimeFailureKind> {
        let command = verified.command();
        let context = lease
            .run_pre_invocation(
                self.input
                    .materialize_entry(execution, "toolkit-runtime-context"),
            )
            .await
            .map_err(|error| lease_failure(&error))?
            .map_err(|error| input_failure(&error))?;
        let request = match command.capability_command.as_ref() {
            Some(worker_command_v1::CapabilityCommand::ToolkitCallTool(call)) => {
                let arguments = lease
                    .run_pre_invocation(
                        self.input
                            .materialize_entry(execution, &call.arguments_entry_id),
                    )
                    .await
                    .map_err(|error| lease_failure(&error))?
                    .map_err(|error| input_failure(&error))?;
                DirectToolkitRequest::parse_call(
                    &call.toolkit_type,
                    &call.toolkit_id,
                    &call.tool_name,
                    settings,
                    arguments.as_bytes(),
                    context.as_bytes(),
                )
            }
            Some(worker_command_v1::CapabilityCommand::ToolkitAvailableTools(discovery)) => {
                DirectToolkitRequest::parse_discovery(
                    &discovery.toolkit_type,
                    &discovery.toolkit_id,
                    settings,
                    context.as_bytes(),
                )
            }
            _ => return Err(RuntimeFailureKind::InvalidInput),
        };
        request.map_err(|error| request_failure(error.code()))
    }

    /// The index tools' authority for a `toolkit.call_tool.v1` run that
    /// calls an index tool of a toolkit carrying an index catalog: this
    /// worker's vector store, the claim's vector token and its redeemed
    /// context, with no chat model (the step-back pair refuse here). Without
    /// a vector store or token the authority is withheld with that reason,
    /// and no context is redeemed.
    async fn index_tool_authority(
        &self,
        authorized: &AuthorizedToolkitExecution,
        lease: &mut ClaimLeaseMonitor,
    ) -> Result<Result<crate::toolkits::IndexToolAuthority, IndexToolsWithheld>, RuntimeFailureKind>
    {
        let Some(runtime) = self.index_runtime.as_ref() else {
            return Ok(Err(IndexToolsWithheld::NoVectorStore));
        };
        let Some(token) = authorized.execution().vector_token() else {
            return Ok(Err(IndexToolsWithheld::NoVectorToken));
        };
        let context = lease
            .run_pre_invocation(
                self.platform
                    .redeem_elitea_context(authorized.runtime_context()),
            )
            .await
            .map_err(|error| lease_failure(&error))?
            .map_err(|error| runtime_context_failure(&error))?;
        Ok(Ok(crate::toolkits::IndexToolAuthority::new(Arc::new(
            crate::indexing::tools::ClaimIndexToolHost::new(
                Arc::clone(runtime),
                token,
                Arc::new(context),
                None,
                authorized.runtime_context().index_removal_binding(),
            ),
        ))))
    }

    /// The Office-host authority for a direct call of a `pptx` or `imagegen`
    /// toolkit. `pptx` redeems the claim's PAT for the platform object API,
    /// with no chat model (a direct call binds none, as in a pipeline node).
    /// `imagegen` redeems nothing and calls nothing: its tools write
    /// artifacts, this path runs read-only tools only, and the runtime
    /// answers `EffectfulToolUnavailable` before one runs. `None` for any
    /// other toolkit, or `pptx` on a worker with no Office runtime; the
    /// toolkit is then unsupported here.
    async fn office_authority(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        authorized: &AuthorizedToolkitExecution,
        lease: &mut ClaimLeaseMonitor,
        request: &DirectToolkitRequest,
    ) -> Result<Option<crate::toolkits::ArtifactToolAuthority>, RuntimeFailureKind> {
        let discovery = verified.kind() == ToolkitCommandKind::AvailableTools;
        match direct_lending(
            request.toolkit_type(),
            request.tool_name(),
            self.office.is_some(),
            discovery,
        ) {
            DirectLending::NoRoundTrip => Ok(Some(
                crate::toolkits::ArtifactToolAuthority::new(Arc::new(HostOnlyPlatformWriter))
                    .with_office(Arc::new(NoRoundTripImagesHost)),
            )),
            DirectLending::Claim => {
                let Some(runtime) = self.office.as_ref() else {
                    return Ok(None);
                };
                let context = lease
                    .run_pre_invocation(
                        self.platform
                            .redeem_elitea_context(authorized.runtime_context()),
                    )
                    .await
                    .map_err(|error| lease_failure(&error))?
                    .map_err(|error| runtime_context_failure(&error))?;
                let Ok(host) = ClaimOfficeHost::new(
                    Arc::clone(runtime),
                    &context.model_facade_token(),
                    &context.resource_project_id().to_string(),
                    LazyStepbackModel::unbound(),
                ) else {
                    tracing::warn!(
                        event = "office_host_unavailable",
                        "a direct toolkit call is left out: the object API client could not be built"
                    );
                    return Ok(None);
                };
                Ok(Some(
                    crate::toolkits::ArtifactToolAuthority::new(Arc::new(HostOnlyPlatformWriter))
                        .with_office(Arc::new(host)),
                ))
            }
            DirectLending::None => Ok(None),
        }
    }

    async fn execute_shared(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        authorized: &AuthorizedToolkitExecution,
        lease: &mut ClaimLeaseMonitor,
        request: &DirectToolkitRequest,
    ) -> Result<ToolkitExecuteReadTerminalOutput, RuntimeFailureKind> {
        let execution = authorized.execution();
        let command = verified.command();
        let is_call = verified.kind() == ToolkitCommandKind::CallTool;
        if deadline_exceeded(verified, self.clock.as_ref()) {
            return Err(RuntimeFailureKind::DeadlineExceeded);
        }
        // The index tools (ADR-0030 I1) act under this claim's vector token
        // and its redeemed context (the query embedding), so the context is
        // redeemed here — only when the call names an index tool of a
        // toolkit that carries an index catalog (review round 1: a `github`
        // `read_file` on such a toolkit does not), since the redemption is a
        // platform round trip every other tool call does not need.
        let index_tools = if is_call && request.calls_index_tool() {
            self.index_tool_authority(authorized, lease).await?
        } else {
            Err(IndexToolsWithheld::NotRequested)
        };
        let index_tools = match index_tools.as_ref() {
            Ok(authority) => IndexToolSupport::Lent(authority),
            Err(reason) => IndexToolSupport::Withheld(*reason),
        };
        let office = self
            .office_authority(verified, authorized, lease, request)
            .await?;
        let operation = async {
            if is_call {
                self.runtime
                    .execute_test(
                        request,
                        &command.execution_id,
                        &command.command_id,
                        index_tools,
                        office.as_ref(),
                    )
                    .await
            } else {
                self.runtime.discover(request, office.as_ref()).await
            }
        };
        let result = lease
            .run_pre_invocation(operation)
            .await
            .map_err(|error| lease_failure(&error))?;
        lease
            .check_now()
            .await
            .map_err(|error| lease_failure(&error))?;
        if deadline_exceeded(verified, self.clock.as_ref()) {
            return Err(RuntimeFailureKind::DeadlineExceeded);
        }
        let bundle = execution.input_bundle_ref();
        let entry = execution.request_entry();
        let settings_digest = entry_digest(entry)?;
        if is_call {
            let summary = match result {
                Err(error) if error.authorization().is_some() => {
                    self.authorization_summary(verified, request, &error, lease)
                        .await?
                }
                outcome => call_summary(outcome)?,
            };
            return bind_call_result(execution, request, summary)
                .map(|result| ToolkitExecuteReadTerminalOutput::CallTool(Box::new(result)));
        }
        let value = match result {
            Err(error) if error.authorization().is_some() => {
                let summary = self
                    .authorization_summary(verified, request, &error, lease)
                    .await?;
                let challenge = summary
                    .authorization_required
                    .ok_or(RuntimeFailureKind::AuthorizationFailed)?;
                discovery_authorization_result(&challenge)?
            }
            outcome => outcome.map_err(|error| runtime_failure(error.code()))?,
        };
        let artifact_bytes =
            serde_json::to_vec(&value).map_err(|_| RuntimeFailureKind::InvalidInput)?;
        let artifact = lease
            .run_pre_invocation(
                self.input
                    .publish_toolkit_discovery(execution, &artifact_bytes),
            )
            .await
            .map_err(|error| lease_failure(&error))?
            .map_err(|error| input_failure(&error))?;
        Ok(ToolkitExecuteReadTerminalOutput::AvailableTools(Box::new(
            ToolkitAvailableToolsResultV1 {
                toolkit_type: request.toolkit_type().to_owned(),
                input_bundle_id: bundle.input_bundle_id.clone(),
                input_bundle_digest: clone_digest(bundle.digest.as_ref())?,
                settings_entry_id: entry.entry_id.clone(),
                settings_entry_version: entry.immutable_version.clone(),
                settings_content_digest: settings_digest,
                result_artifact: Some(artifact),
            },
        )))
    }

    async fn authorization_summary(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        request: &DirectToolkitRequest,
        error: &crate::toolkits::DirectToolkitRuntimeError,
        lease: &mut ClaimLeaseMonitor,
    ) -> Result<ToolkitCallToolSummaryV1, RuntimeFailureKind> {
        let requirement = error
            .authorization()
            .ok_or(RuntimeFailureKind::AuthorizationFailed)?;
        if requirement.toolkit_type() != request.toolkit_type()
            || requirement.toolkit_name() != request.toolkit_name()
        {
            return Err(RuntimeFailureKind::AuthorizationFailed);
        }
        let toolkit_id = match verified.command().capability_command.as_ref() {
            Some(worker_command_v1::CapabilityCommand::ToolkitCallTool(command)) => {
                &command.toolkit_id
            }
            Some(worker_command_v1::CapabilityCommand::ToolkitAvailableTools(command)) => {
                &command.toolkit_id
            }
            _ => return Err(RuntimeFailureKind::InvalidInput),
        };
        let resolved = lease
            .run_pre_invocation(requirement.resolve_public_metadata())
            .await
            .map_err(|error| lease_failure(&error))?;
        let metadata = resolved
            .resource_metadata()
            .map(serde_json::to_vec)
            .transpose()
            .map_err(|_| RuntimeFailureKind::InvalidInput)?
            .unwrap_or_default();
        if metadata.len() > 16 * 1024 {
            return Err(RuntimeFailureKind::ResourceExhausted);
        }
        Ok(ToolkitCallToolSummaryV1 {
            status: ToolkitCallToolStatusV1::AuthorizationRequired as i32,
            result_json: String::new(),
            truncated: false,
            error_message: crate::protocol::output::TOOLKIT_AUTHORIZATION_REQUIRED_MESSAGE
                .to_owned(),
            authorization_required: Some(ToolkitAuthorizationRequiredV1 {
                toolkit_name: resolved.toolkit_name().to_owned(),
                toolkit_type: resolved.toolkit_type().to_owned(),
                server_url: resolved.server_url().to_owned(),
                resource_metadata_url: resolved
                    .resource_metadata_url()
                    .unwrap_or_default()
                    .to_owned(),
                resource_metadata_json: metadata,
                toolkit_id: toolkit_id.clone(),
            }),
        })
    }

    #[allow(clippy::too_many_arguments)]
    async fn publish_fresh_terminal(
        &self,
        delivery: CommandDelivery,
        verified: VerifiedToolkitExecuteReadCommand,
        output_authority: AgentExecutionOutputAuthority,
        output: PreparedAgentOutput,
        lease: ClaimLeaseMonitor,
        proposed: ToolkitExecuteReadTerminalOutput,
    ) -> Result<(), ToolkitProcessError> {
        let proposed = call_tool_terminal(verified.kind(), proposed);
        let result = async {
            let terminal = match lease.check_now().await {
                Ok(()) if deadline_exceeded(&verified, self.clock.as_ref()) => {
                    interrupted_terminal(proposed, Interruption::DeadlineExceeded)
                }
                Ok(()) => proposed,
                Err(ClaimLeaseError::Cancelled(_)) => {
                    interrupted_terminal(proposed, Interruption::Stopped)
                }
                Err(error) => return Err(ToolkitProcessError::Lease(error)),
            };
            let occurred_at = valid_now(self.clock.as_ref())?;
            let frame = output_authority
                .bind_toolkit_execute_read_terminal(&verified, terminal, occurred_at)
                .map_err(ToolkitProcessError::Protocol)?;
            let (spool, reopener) = output
                .persist_terminal(&frame)
                .await
                .map_err(ToolkitProcessError::Output)?;
            let (acknowledged, _) = replay_toolkit_terminal_with_replacement(
                self.replay.as_ref(),
                &verified,
                frame,
                spool,
                reopener,
                self.terminal_recovery.max_output_sessions(),
            )
            .await
            .map_err(ToolkitProcessError::Terminal)?;
            let receipt = self
                .control
                .prepare_agent_settlement(acknowledged)
                .await
                .map_err(ToolkitProcessError::Control)?;
            self.retirer
                .retire_toolkit_execute_read_command(delivery, &verified, receipt.into())
                .await
                .map_err(ToolkitProcessError::CommandBus)
        }
        .await;
        close_lease_logged(lease).await;
        result
    }

    async fn recover_terminal(
        &self,
        recovery: ToolkitTerminalRecovery,
    ) -> Result<(), ToolkitProcessError> {
        let ToolkitTerminalRecovery {
            delivery,
            verified,
            claim,
            spool,
            frame,
            reopener,
        } = recovery;
        let lease = ClaimLeaseMonitor::start_recovery(
            Arc::clone(&self.control),
            claim,
            Arc::clone(&self.clock),
            self.preparation.lease_config(),
        );
        let result = async {
            let (acknowledged, _) = replay_toolkit_terminal_with_replacement(
                self.replay.as_ref(),
                &verified,
                frame,
                spool,
                reopener,
                self.terminal_recovery.max_output_sessions(),
            )
            .await
            .map_err(ToolkitProcessError::Terminal)?;
            let receipt = self
                .control
                .prepare_agent_settlement(acknowledged)
                .await
                .map_err(ToolkitProcessError::Control)?;
            self.retirer
                .retire_toolkit_execute_read_command(delivery, &verified, receipt.into())
                .await
                .map_err(ToolkitProcessError::CommandBus)
        }
        .await;
        if let Err(error) = lease.close().await {
            tracing::warn!(
                event = "toolkit_delivery_recovery_lease_close_failed",
                error_code = error.code().as_str(),
                retryable = error.retryable(),
            );
        }
        result
    }

    async fn reconcile_empty_recovery(
        &self,
        recovery: EmptyToolkitOutputRecovery,
        proposed: RuntimeFailureKind,
    ) -> Result<(), ToolkitProcessError> {
        let (delivery, verified, recovery, output) = recovery.into_parts();
        let (authority, lease_handle) = recovery.split_lease_authority();
        let lease = ClaimLeaseMonitor::start_output_recovery(
            Arc::clone(&self.control),
            lease_handle,
            Arc::clone(&self.clock),
            self.preparation.lease_config(),
        );
        let result = async {
            let failure = match lease.check_now().await {
                Ok(()) if deadline_exceeded(&verified, self.clock.as_ref()) => {
                    RuntimeFailureKind::DeadlineExceeded
                }
                Ok(()) => proposed,
                Err(ClaimLeaseError::Cancelled(_)) => RuntimeFailureKind::Cancelled,
                Err(error) => return Err(ToolkitProcessError::Lease(error)),
            };
            let frame = authority
                .bind_toolkit_execute_read_failure_terminal(
                    &verified,
                    failure,
                    valid_now(self.clock.as_ref())?,
                )
                .map_err(ToolkitProcessError::Protocol)?;
            let (spool, reopener) = output
                .persist_terminal(&frame)
                .await
                .map_err(ToolkitProcessError::Output)?;
            let (acknowledged, _) = replay_toolkit_terminal_with_replacement(
                self.replay.as_ref(),
                &verified,
                frame,
                spool,
                reopener,
                self.terminal_recovery.max_output_sessions(),
            )
            .await
            .map_err(ToolkitProcessError::Terminal)?;
            let receipt = self
                .control
                .prepare_agent_settlement(acknowledged)
                .await
                .map_err(ToolkitProcessError::Control)?;
            self.retirer
                .retire_toolkit_execute_read_command(delivery, &verified, receipt.into())
                .await
                .map_err(ToolkitProcessError::CommandBus)
        }
        .await;
        if let Err(error) = lease.close().await {
            tracing::warn!(
                event = "toolkit_delivery_reconcile_lease_close_failed",
                error_code = error.code().as_str(),
                retryable = error.retryable(),
            );
        }
        result
    }
}

/// The claim's chat model entries, fetched on demand and together (the run
/// asks the first time a step needs the chat model). A failure of either is
/// recorded as a toolkit input failure and fails the whole load: the run then
/// has no chat model, and never default settings for a configuration entry
/// that exists but could not be read.
struct ClaimChatEntries<'a, I> {
    input: &'a I,
    execution: &'a LeaseMonitoredAgentExecution,
    model_entry_id: &'a str,
    configuration_entry_id: &'a str,
}

#[async_trait::async_trait]
impl<I: AgentInputMaterializer> ChatEntrySource for ClaimChatEntries<'_, I> {
    async fn load(&self) -> Result<ChatEntries, ChatUnavailable> {
        let fetch = |entry_id: &str| {
            let entry_id = entry_id.to_owned();
            async move {
                if entry_id.is_empty() {
                    return Ok(None);
                }
                match self
                    .input
                    .materialize_entry(self.execution, &entry_id)
                    .await
                {
                    Ok(entry) => Ok(Some(entry)),
                    Err(error) => {
                        record_toolkit_input_failure("index_input_materialization", &error);
                        Err(ChatUnavailable)
                    }
                }
            }
        };
        let model = fetch(self.model_entry_id).await?;
        let configuration = fetch(self.configuration_entry_id).await?;
        Ok(ChatEntries {
            model,
            configuration,
        })
    }
}

/// The index bundle's materialized entries.
struct IndexInputs {
    toolkit_configuration: MaterializedInput,
    tool_parameters: MaterializedInput,
    /// `tool_parameters` parsed once, when they are readable.
    parsed: Option<crate::indexing::inputs::ParsedArgs>,
    embedding_binding: Option<MaterializedInput>,
}

/// The typed result: the admitted bundle and entries restated (Main checks
/// every binding against the claim), and the run's summary.
fn bind_index_result(
    execution: &LeaseMonitoredAgentExecution,
    index: &IndexIngestCommandV1,
    summary: IndexIngestSummaryV1,
) -> Result<IndexIngestResultV1, RuntimeFailureKind> {
    let bundle = execution.input_bundle_ref();
    let binding =
        |entry: &ExecutionInputEntryV1| -> Result<IndexIngestInputBindingV1, RuntimeFailureKind> {
            Ok(IndexIngestInputBindingV1 {
                entry_id: entry.entry_id.clone(),
                immutable_version: entry.immutable_version.clone(),
                content_digest: entry_digest(entry)?,
            })
        };
    let optional =
        |entry_id: &str| -> Result<Option<IndexIngestInputBindingV1>, RuntimeFailureKind> {
            if entry_id.is_empty() {
                return Ok(None);
            }
            let entry = execution
                .input_entry(entry_id)
                .ok_or(RuntimeFailureKind::InvalidInput)?;
            binding(entry).map(Some)
        };
    Ok(IndexIngestResultV1 {
        input_bundle_id: bundle.input_bundle_id.clone(),
        input_bundle_digest: clone_digest(bundle.digest.as_ref())?,
        toolkit_configuration: Some(binding(execution.request_entry())?),
        tool_parameters: Some(binding(
            execution
                .arguments_entry()
                .ok_or(RuntimeFailureKind::InvalidInput)?,
        )?),
        llm_model: optional(&index.llm_model_entry_id)?,
        llm_configuration: optional(&index.llm_configuration_entry_id)?,
        mcp_tokens: optional(&index.mcp_tokens_entry_id)?,
        result_artifact: None,
        result_summary: Some(summary),
        embedding_binding: index.embedding_binding.clone(),
    })
}

const fn runtime_context_failure(error: &RuntimeContextError) -> RuntimeFailureKind {
    match error {
        RuntimeContextError::AuthorizationFailed(_) => RuntimeFailureKind::AuthorizationFailed,
        RuntimeContextError::ResourceExhausted(_) => RuntimeFailureKind::ResourceExhausted,
        RuntimeContextError::InvalidConfiguration(_) => RuntimeFailureKind::Internal,
        RuntimeContextError::DependencyUnavailable(_)
        | RuntimeContextError::Transport(_)
        | RuntimeContextError::Timeout(_) => RuntimeFailureKind::DependencyUnavailable,
        _ => RuntimeFailureKind::InvalidInput,
    }
}

fn bind_call_result(
    execution: &LeaseMonitoredAgentExecution,
    request: &DirectToolkitRequest,
    summary: ToolkitCallToolSummaryV1,
) -> Result<ToolkitCallToolResultV1, RuntimeFailureKind> {
    let bundle = execution.input_bundle_ref();
    let entry = execution.request_entry();
    let arguments = execution
        .arguments_entry()
        .ok_or(RuntimeFailureKind::InvalidInput)?;
    Ok(ToolkitCallToolResultV1 {
        toolkit_type: request.toolkit_type().to_owned(),
        tool_name: request.tool_name().to_owned(),
        input_bundle_id: bundle.input_bundle_id.clone(),
        input_bundle_digest: clone_digest(bundle.digest.as_ref())?,
        settings_entry_id: entry.entry_id.clone(),
        settings_entry_version: entry.immutable_version.clone(),
        settings_content_digest: clone_digest(
            entry
                .content
                .as_ref()
                .ok_or(RuntimeFailureKind::InvalidInput)?
                .digest
                .as_ref(),
        )?,
        arguments_entry_id: arguments.entry_id.clone(),
        arguments_content_digest: clone_digest(
            arguments
                .content
                .as_ref()
                .ok_or(RuntimeFailureKind::InvalidInput)?
                .digest
                .as_ref(),
        )?,
        result_artifact: None,
        result_summary: Some(summary),
    })
}

/// A tool run's terminal as published. On `toolkit.call_tool.v1` a refusal
/// of the TOOL is its own typed result (`EFFECT_REFUSED` / `POLICY_REFUSED` / `SENSITIVE_REFUSED`), so
/// an authorization failure left over is always the run's own credentials or
/// context: it carries that sentence (#1292 round 2, item 4), which Main reads
/// as "the credentials" while an older worker's generic one reads as a
/// refusal of the tool.
fn call_tool_terminal(
    kind: ToolkitCommandKind,
    proposed: ToolkitExecuteReadTerminalOutput,
) -> ToolkitExecuteReadTerminalOutput {
    match proposed {
        ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::AuthorizationFailed)
            if kind == ToolkitCommandKind::CallTool =>
        {
            ToolkitExecuteReadTerminalOutput::Failure(
                RuntimeFailureKind::ToolkitRunAuthorizationFailed,
            )
        }
        other => other,
    }
}

/// Main's `ToolkitEffectfulToolMessage` and `ToolkitPolicyRefusedMessage`.
pub(crate) const EFFECTFUL_TOOL_MESSAGE: &str =
    "This tool changes data, and this path runs read-only tools only.";
pub(crate) const POLICY_REFUSED_MESSAGE: &str =
    "This tool is blocked or sensitive under this deployment's policy and cannot run here.";

fn call_summary(
    result: Result<serde_json::Value, crate::toolkits::DirectToolkitRuntimeError>,
) -> Result<ToolkitCallToolSummaryV1, RuntimeFailureKind> {
    let (status, value, error_message) = match result {
        Ok(value) => (ToolkitCallToolStatusV1::Ok, Some(value), ""),
        Err(error) => match error.code() {
            DirectToolkitRuntimeErrorCode::ToolError => (
                ToolkitCallToolStatusV1::ToolError,
                None,
                "The toolkit operation failed.",
            ),
            DirectToolkitRuntimeErrorCode::ToolNotSelected => (
                ToolkitCallToolStatusV1::UnknownTool,
                None,
                "The toolkit operation is not selected.",
            ),
            // A refusal of THIS tool is a verdict on the tool, not a failure
            // of the run: its own TYPED status (EFFECT_REFUSED /
            // POLICY_REFUSED), never confused with the run's credentials
            // being refused (a runtime failure, AuthorizationFailed) nor with
            // a toolkit that has no tool by that name (UNKNOWN_TOOL). The
            // sentence is Main's `ToolkitEffectfulToolMessage` /
            // `ToolkitPolicyRefusedMessage`, which Main still reads as a
            // fallback from a worker that sent UNKNOWN_TOOL (round 1).
            DirectToolkitRuntimeErrorCode::EffectfulToolUnavailable => (
                ToolkitCallToolStatusV1::EffectRefused,
                None,
                EFFECTFUL_TOOL_MESSAGE,
            ),
            DirectToolkitRuntimeErrorCode::ToolBlocked => (
                ToolkitCallToolStatusV1::PolicyRefused,
                None,
                POLICY_REFUSED_MESSAGE,
            ),
            DirectToolkitRuntimeErrorCode::SensitiveToolUnavailable => (
                ToolkitCallToolStatusV1::SensitiveRefused,
                None,
                POLICY_REFUSED_MESSAGE,
            ),
            DirectToolkitRuntimeErrorCode::UnsupportedToolkit => (
                ToolkitCallToolStatusV1::UnsupportedToolkit,
                None,
                "The toolkit family is not supported.",
            ),
            code => return Err(runtime_failure(code)),
        },
    };
    let result_json = value
        .map(|value| serde_json::to_string(&value))
        .transpose()
        .map_err(|_| RuntimeFailureKind::InvalidInput)?
        .unwrap_or_default();
    let truncated = result_json.len() > 48 * 1024;
    Ok(ToolkitCallToolSummaryV1 {
        status: status as i32,
        result_json: if truncated {
            String::new()
        } else {
            result_json
        },
        truncated,
        error_message: error_message.to_owned(),
        authorization_required: None,
    })
}

fn bind_result(
    execution: &LeaseMonitoredAgentExecution,
    request: &DirectToolkitRequest,
    result: &serde_json::Value,
) -> Result<ToolkitExecuteReadResultV1, RuntimeFailureKind> {
    let bundle = execution.input_bundle_ref();
    let entry = execution.request_entry();
    let content = entry
        .content
        .as_ref()
        .ok_or(RuntimeFailureKind::InvalidInput)?;
    let result_json = serde_json::to_vec(result).map_err(|_| RuntimeFailureKind::InvalidInput)?;
    Ok(ToolkitExecuteReadResultV1 {
        input_bundle_id: bundle.input_bundle_id.clone(),
        input_bundle_digest: clone_digest(bundle.digest.as_ref())?,
        request_entry_id: entry.entry_id.clone(),
        request_entry_version: entry.immutable_version.clone(),
        request_content_digest: clone_digest(content.digest.as_ref())?,
        result_json,
        toolkit_type: request.toolkit_type().to_owned(),
        toolkit_name: request.toolkit_name().to_owned(),
        tool_name: request.tool_name().to_owned(),
    })
}

fn entry_digest(
    entry: &crate::protocol::elitea::runtime::v1::ExecutionInputEntryV1,
) -> Result<Option<DigestV1>, RuntimeFailureKind> {
    clone_digest(
        entry
            .content
            .as_ref()
            .ok_or(RuntimeFailureKind::InvalidInput)?
            .digest
            .as_ref(),
    )
}

fn clone_digest(digest: Option<&DigestV1>) -> Result<Option<DigestV1>, RuntimeFailureKind> {
    let digest = digest.ok_or(RuntimeFailureKind::InvalidInput)?;
    Ok(Some(digest.clone()))
}

fn valid_now(clock: &impl UnixMillisClock) -> Result<i64, ToolkitProcessError> {
    let now = clock.now_unix_millis();
    if now <= 0 {
        return Err(ToolkitProcessError::Clock);
    }
    Ok(now)
}

/// How an index run ended, for the processor: a bound result (its summary,
/// the store's record included when the run touched it), a runtime failure
/// to publish, or a lost lease, which publishes nothing.
enum IndexRunEnd {
    Result(Box<IndexIngestResultV1>),
    Failure(RuntimeFailureKind),
    LeaseLost(ClaimLeaseError),
}

/// The terminal an index run's end proposes, or the lease error when there
/// is none to publish: another worker may own the claim, so this one records
/// the loss and leaves the command to the lease's no-ACK path.
fn index_terminal(end: IndexRunEnd) -> Result<ToolkitExecuteReadTerminalOutput, ClaimLeaseError> {
    match end {
        IndexRunEnd::Result(result) => Ok(ToolkitExecuteReadTerminalOutput::IndexIngest(result)),
        IndexRunEnd::Failure(failure) => {
            record_toolkit_execution_failure("index_ingest", runtime_failure_code(failure), false);
            Ok(ToolkitExecuteReadTerminalOutput::Failure(failure))
        }
        IndexRunEnd::LeaseLost(error) => {
            record_toolkit_execution_failure(
                "index_ingest_lease",
                error.code().as_str(),
                error.retryable(),
            );
            Err(error)
        }
    }
}

/// Closes the lease; a failure to close is logged, never surfaced, as the
/// terminal (or its absence) is already decided.
async fn close_lease_logged(lease: ClaimLeaseMonitor) {
    if let Err(error) = lease.close().await {
        tracing::warn!(
            event = "toolkit_delivery_lease_close_failed",
            error_code = error.code().as_str(),
            retryable = error.retryable(),
        );
    }
}

/// The terminal to publish when a Stop or the deadline is observed after
/// the proposed terminal was made. An index result whose run touched the
/// store keeps its summary, turned into the interrupted form (the store's
/// totals and record stay; the status says stopped or out of time), so Main
/// still records what the store holds; every other terminal becomes the
/// plain runtime failure, as before.
fn interrupted_terminal(
    proposed: ToolkitExecuteReadTerminalOutput,
    cause: Interruption,
) -> ToolkitExecuteReadTerminalOutput {
    let failure = match cause {
        Interruption::Stopped => RuntimeFailureKind::Cancelled,
        Interruption::DeadlineExceeded => RuntimeFailureKind::DeadlineExceeded,
    };
    match proposed {
        ToolkitExecuteReadTerminalOutput::IndexIngest(mut result) => {
            let touched = result
                .result_summary
                .as_mut()
                .is_some_and(|summary| interrupt_summary(summary, cause));
            if touched {
                ToolkitExecuteReadTerminalOutput::IndexIngest(result)
            } else {
                ToolkitExecuteReadTerminalOutput::Failure(failure)
            }
        }
        _ => ToolkitExecuteReadTerminalOutput::Failure(failure),
    }
}

fn deadline_exceeded(
    verified: &VerifiedToolkitExecuteReadCommand,
    clock: &impl UnixMillisClock,
) -> bool {
    let now = clock.now_unix_millis();
    now <= 0 || now >= verified.command().deadline_unix_millis
}

const fn input_failure(error: &InputContentError) -> RuntimeFailureKind {
    match error {
        InputContentError::InvalidInput(_) => RuntimeFailureKind::InvalidInput,
        InputContentError::ResourceExhausted(_) => RuntimeFailureKind::ResourceExhausted,
        InputContentError::AuthorizationFailed(_) => RuntimeFailureKind::AuthorizationFailed,
        InputContentError::InvalidConfiguration(_) => RuntimeFailureKind::Internal,
        InputContentError::DependencyUnavailable(_)
        | InputContentError::Transport(_)
        | InputContentError::Timeout(_) => RuntimeFailureKind::DependencyUnavailable,
    }
}

const fn request_failure(code: DirectToolkitRequestErrorCode) -> RuntimeFailureKind {
    match code {
        DirectToolkitRequestErrorCode::InvalidInput => RuntimeFailureKind::InvalidInput,
        DirectToolkitRequestErrorCode::ResourceExhausted => RuntimeFailureKind::ResourceExhausted,
        DirectToolkitRequestErrorCode::IncompatibleVersion => {
            RuntimeFailureKind::IncompatibleVersion
        }
    }
}

const fn request_error_code(code: DirectToolkitRequestErrorCode) -> &'static str {
    match code {
        DirectToolkitRequestErrorCode::InvalidInput => "toolkit_request.invalid_input",
        DirectToolkitRequestErrorCode::ResourceExhausted => "toolkit_request.resource_exhausted",
        DirectToolkitRequestErrorCode::IncompatibleVersion => {
            "toolkit_request.incompatible_version"
        }
    }
}

const fn runtime_failure(code: DirectToolkitRuntimeErrorCode) -> RuntimeFailureKind {
    match code {
        DirectToolkitRuntimeErrorCode::InvalidInput
        | DirectToolkitRuntimeErrorCode::InvalidConfiguration
        | DirectToolkitRuntimeErrorCode::ToolNotSelected => RuntimeFailureKind::InvalidInput,
        DirectToolkitRuntimeErrorCode::ResourceExhausted => RuntimeFailureKind::ResourceExhausted,
        DirectToolkitRuntimeErrorCode::UnsupportedToolkit
        | DirectToolkitRuntimeErrorCode::EffectfulToolUnavailable => {
            RuntimeFailureKind::UnsupportedCapability
        }
        DirectToolkitRuntimeErrorCode::ToolBlocked
        | DirectToolkitRuntimeErrorCode::SensitiveToolUnavailable
        | DirectToolkitRuntimeErrorCode::AuthorizationRequired => {
            RuntimeFailureKind::AuthorizationFailed
        }
        DirectToolkitRuntimeErrorCode::ToolError
        | DirectToolkitRuntimeErrorCode::DependencyUnavailable => {
            RuntimeFailureKind::DependencyUnavailable
        }
        DirectToolkitRuntimeErrorCode::DeadlineExceeded => RuntimeFailureKind::DeadlineExceeded,
    }
}

const fn runtime_error_code(code: DirectToolkitRuntimeErrorCode) -> &'static str {
    match code {
        DirectToolkitRuntimeErrorCode::InvalidInput => "toolkit_runtime.invalid_input",
        DirectToolkitRuntimeErrorCode::InvalidConfiguration => {
            "toolkit_runtime.invalid_configuration"
        }
        DirectToolkitRuntimeErrorCode::ResourceExhausted => "toolkit_runtime.resource_exhausted",
        DirectToolkitRuntimeErrorCode::UnsupportedToolkit => "toolkit_runtime.unsupported_toolkit",
        DirectToolkitRuntimeErrorCode::ToolNotSelected => "toolkit_runtime.tool_not_selected",
        DirectToolkitRuntimeErrorCode::ToolBlocked => "toolkit_runtime.tool_blocked",
        DirectToolkitRuntimeErrorCode::SensitiveToolUnavailable => {
            "toolkit_runtime.sensitive_tool_unavailable"
        }
        DirectToolkitRuntimeErrorCode::EffectfulToolUnavailable => {
            "toolkit_runtime.effectful_tool_unavailable"
        }
        DirectToolkitRuntimeErrorCode::AuthorizationRequired => {
            "toolkit_runtime.authorization_required"
        }
        DirectToolkitRuntimeErrorCode::ToolError
        | DirectToolkitRuntimeErrorCode::DependencyUnavailable => {
            "toolkit_runtime.dependency_unavailable"
        }
        DirectToolkitRuntimeErrorCode::DeadlineExceeded => "toolkit_runtime.deadline_exceeded",
    }
}

const fn runtime_failure_code(failure: RuntimeFailureKind) -> &'static str {
    match failure {
        RuntimeFailureKind::UnsupportedCapability => "runtime.unsupported_capability",
        RuntimeFailureKind::IncompatibleVersion => "runtime.incompatible_version",
        RuntimeFailureKind::InvalidInput => "runtime.invalid_input",
        RuntimeFailureKind::PipelineChildInputTypeInvalid => "pipeline.child_input_type_invalid",
        RuntimeFailureKind::PipelineNodeTypeNotAvailable => "pipeline.node_type_not_available",
        RuntimeFailureKind::ResourceExhausted => "runtime.resource_exhausted",
        RuntimeFailureKind::ExecutionInputLimit
        | RuntimeFailureKind::ExecutionInputFieldLimit(_) => "runtime.execution_input_limit",
        RuntimeFailureKind::DependencyUnavailable => "runtime.dependency_unavailable",
        RuntimeFailureKind::DeadlineExceeded => "runtime.deadline_exceeded",
        RuntimeFailureKind::AuthorizationFailed => "runtime.authorization_failed",
        RuntimeFailureKind::ToolkitRunAuthorizationFailed => {
            "runtime.toolkit_run_authorization_failed"
        }
        RuntimeFailureKind::Cancelled => "runtime.cancelled",
        RuntimeFailureKind::ResultRefused => "runtime.result_refused",
        RuntimeFailureKind::OutputContinuationExhausted => "runtime.output_continuation_exhausted",
        RuntimeFailureKind::ModelTimeout => "runtime.model_timeout",
        RuntimeFailureKind::ModelRateLimited => "runtime.model_rate_limited",
        RuntimeFailureKind::ModelAccessDenied => "runtime.model_access_denied",
        RuntimeFailureKind::ModelBudgetExhausted(_) => "runtime.model_budget_exhausted",
        RuntimeFailureKind::ModelRequestRejected => "runtime.model_request_rejected",
        RuntimeFailureKind::ModelResponseInvalid => "runtime.model_response_invalid",
        RuntimeFailureKind::ContextBudgetExceeded => "runtime.context_budget_exceeded",
        RuntimeFailureKind::ModelRequestTooLarge => "runtime.model_request_too_large",
        RuntimeFailureKind::ModelUnavailable => "runtime.model_unavailable",
        RuntimeFailureKind::ModelProviderFailure => "runtime.model_provider_failure",
        RuntimeFailureKind::PipelineInputInvalid => "pipeline.input_invalid",
        RuntimeFailureKind::PipelineInputLimit => "pipeline.input_limit",
        RuntimeFailureKind::PipelineToolUnavailable => "pipeline.tool_unavailable",
        RuntimeFailureKind::PipelineToolFailed => "pipeline.tool_failed",
        RuntimeFailureKind::PipelineResultInvalid => "pipeline.result_invalid",
        RuntimeFailureKind::PipelineResultLimit => "pipeline.result_limit",
        RuntimeFailureKind::PipelineCodeFailed => "pipeline.code_failed",
        RuntimeFailureKind::CodePreparationFailed => "pipeline.code_preparation_failed",
        RuntimeFailureKind::CodePreparationCancelled => "pipeline.code_preparation_cancelled",
        RuntimeFailureKind::CodePreparationUnconfirmed => "pipeline.code_preparation_unconfirmed",

        RuntimeFailureKind::OutputProjectionLimit => "runtime.output_delivery_limit",
        RuntimeFailureKind::Internal => "runtime.internal",
    }
}

fn record_toolkit_execution_failure(
    stage: &'static str,
    error_code: &'static str,
    retryable: bool,
) {
    tracing::warn!(
        event = "toolkit_execution_failed",
        stage,
        error_code,
        retryable,
        "direct toolkit execution failed at a redacted runtime boundary"
    );
}

fn record_toolkit_deadline(stage: &'static str) {
    record_toolkit_execution_failure(stage, "toolkit_execution.deadline_exceeded", false);
}

fn record_toolkit_input_failure(stage: &'static str, error: &InputContentError) {
    tracing::warn!(
        event = "toolkit_execution_failed",
        stage,
        error_code = error.code(),
        retryable = error.retryable(),
        safe_message = %error,
        "direct toolkit input materialization failed at a redacted runtime boundary"
    );
}

const fn lease_failure(error: &ClaimLeaseError) -> RuntimeFailureKind {
    match error {
        ClaimLeaseError::Cancelled(_) => RuntimeFailureKind::Cancelled,
        _ => RuntimeFailureKind::DependencyUnavailable,
    }
}

pub(super) enum ToolkitProcessOutcome {
    Completed { code: &'static str },
    RetainedNoAck { code: &'static str, retryable: bool },
}

impl ToolkitProcessOutcome {
    const fn completed(code: &'static str) -> Self {
        Self::Completed { code }
    }

    const fn retained(code: &'static str, retryable: bool) -> Self {
        Self::RetainedNoAck { code, retryable }
    }

    pub(super) fn record(self) {
        match self {
            Self::Completed { code } => tracing::info!(
                event = "toolkit_delivery_completed",
                outcome = "completed",
                result_code = code,
            ),
            Self::RetainedNoAck { code, retryable } => tracing::warn!(
                event = "toolkit_delivery_retained_no_ack",
                outcome = "retained_no_ack",
                result_code = code,
                retryable,
            ),
        }
    }
}

#[derive(Debug)]
pub(super) enum ToolkitProcessError {
    Delivery(ToolkitDeliveryError),
    OutputPreflight(AgentOutputPreflightError),
    Admission(InvocationAdmissionError),
    Control(crate::protocol::control::AgentControlError),
    Lease(ClaimLeaseError),
    Protocol(ProtocolError),
    Output(crate::transport::OutputGrpcError),
    Terminal(ToolkitTerminalError),
    CommandBus(CommandBusError),
    Clock,
}

impl ToolkitProcessError {
    pub(super) fn code(&self) -> &'static str {
        match self {
            Self::Delivery(error) => error.code(),
            Self::OutputPreflight(error) => error.code(),
            Self::Admission(error) => error.code().as_str(),
            Self::Control(error) if error.retryable() => "toolkit_delivery.control_unavailable",
            Self::Control(_) => "toolkit_delivery.control_rejected",
            Self::Lease(error) => error.code().as_str(),
            Self::Protocol(error) => match error {
                ProtocolError::ResourceExhausted(_) => "toolkit_delivery.resource_exhausted",
                _ => "toolkit_delivery.invalid_terminal",
            },
            Self::Output(error) if output_retryable(error) => "toolkit_delivery.output_unavailable",
            Self::Output(_) => "toolkit_delivery.output_rejected",
            Self::Terminal(error) => error.code(),
            Self::CommandBus(error) => error.code(),
            Self::Clock => "toolkit_delivery.invalid_clock",
        }
    }

    pub(super) fn retryable(&self) -> bool {
        match self {
            Self::Delivery(error) => error.retryable(),
            Self::OutputPreflight(error) => error.retryable(),
            Self::Admission(error) => error.retryable(),
            Self::Control(error) => error.retryable(),
            Self::Lease(error) => error.retryable(),
            Self::Terminal(error) => error.retryable(),
            Self::CommandBus(error) => error.retryable(),
            Self::Output(error) => output_retryable(error),
            Self::Protocol(_) | Self::Clock => false,
        }
    }
}

fn output_retryable(error: &crate::transport::OutputGrpcError) -> bool {
    matches!(
        error,
        crate::transport::OutputGrpcError::Unavailable(_)
            | crate::transport::OutputGrpcError::Protocol(
                crate::transport::OutputProtocolError::DependencyUnavailable
            )
            | crate::transport::OutputGrpcError::Spool(
                crate::spool::SpoolError::OwnershipUnavailable(_)
                    | crate::spool::SpoolError::Unavailable { .. }
            )
    )
}

impl fmt::Display for ToolkitProcessError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code())
    }
}

impl std::error::Error for ToolkitProcessError {}

pub(super) async fn process_toolkit_verified<R, RC, T, K, I>(
    processor: &ToolkitDeliveryProcessor<R, RC, T, K, I>,
    delivery: CommandDelivery,
    verified: VerifiedToolkitExecuteReadCommand,
) where
    R: ControlRpc + 'static,
    RC: CommandRetirementClient + 'static,
    T: ToolkitTerminalReplay + 'static,
    K: UnixMillisClock,
    I: AgentInputMaterializer + 'static,
{
    let command = verified.command();
    let span = tracing::info_span!(
        parent: None,
        "toolkit.delivery",
        execution_id = %command.execution_id,
        generation = command.generation,
        command_id = %command.command_id,
        tenant_id = %command.tenant_id,
        resource_project_id = %command.resource_project_id,
        projection_project_id = %command.projection_project_id,
        capability_id = %command.capability_id,
        nats_stream = %delivery.stream(),
        nats_stream_sequence = delivery.stream_sequence(),
    );
    Box::pin(
        async move {
            match Box::pin(processor.process_verified(delivery, verified)).await {
                Ok(outcome) => outcome.record(),
                Err(error) => tracing::warn!(
                    event = "toolkit_delivery_failed",
                    error_code = error.code(),
                    retryable = error.retryable(),
                ),
            }
        }
        .instrument(span),
    )
    .await;
}

#[cfg(test)]
mod shared_result_tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn preserves_null_and_empty_result_shapes() {
        for value in [
            json!(null),
            json!(""),
            json!(false),
            json!(0),
            json!([]),
            json!({}),
        ] {
            let summary = call_summary(Ok(value.clone())).expect("summary");
            assert_eq!(summary.status, ToolkitCallToolStatusV1::Ok as i32);
            assert_eq!(
                serde_json::from_str::<serde_json::Value>(&summary.result_json).expect("JSON"),
                value
            );
            assert!(!summary.truncated);
        }
    }

    /// A policy refusal of the tool is the tool's own result with Main's
    /// literal; the run's credentials being refused stays a runtime failure.
    #[test]
    fn tool_refusals_are_results_and_a_refused_run_is_a_failure() {
        use crate::toolkits::{DirectToolkitRuntimeErrorCode as Code, test_error};
        for (code, status, message) in [
            (
                Code::EffectfulToolUnavailable,
                ToolkitCallToolStatusV1::EffectRefused,
                EFFECTFUL_TOOL_MESSAGE,
            ),
            (
                Code::ToolBlocked,
                ToolkitCallToolStatusV1::PolicyRefused,
                POLICY_REFUSED_MESSAGE,
            ),
            (
                Code::SensitiveToolUnavailable,
                ToolkitCallToolStatusV1::SensitiveRefused,
                POLICY_REFUSED_MESSAGE,
            ),
        ] {
            let summary = call_summary(Err(test_error(code))).expect("a result");
            assert_eq!(summary.status, status as i32, "{code:?}");
            assert_eq!(summary.error_message, message);
            // The settlement the worker restores agrees: a refusal FAILS.
        }
        // A tool the toolkit lacks stays UNKNOWN_TOOL.
        assert_eq!(
            call_summary(Err(test_error(Code::ToolNotSelected)))
                .expect("a result")
                .status,
            ToolkitCallToolStatusV1::UnknownTool as i32
        );
        assert!(matches!(
            call_summary(Err(test_error(Code::AuthorizationRequired))),
            Err(RuntimeFailureKind::AuthorizationFailed)
        ));
    }

    /// Item 4: the run's own credentials refused on a tool run carry their
    /// own sentence; other commands keep the generic one.
    #[test]
    fn a_tool_runs_authorization_failure_names_the_credentials() {
        use crate::protocol::output::TOOLKIT_RUN_AUTHORIZATION_FAILED_MESSAGE;
        let failed =
            || ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::AuthorizationFailed);
        assert!(matches!(
            call_tool_terminal(ToolkitCommandKind::CallTool, failed()),
            ToolkitExecuteReadTerminalOutput::Failure(
                RuntimeFailureKind::ToolkitRunAuthorizationFailed
            )
        ));
        assert!(matches!(
            call_tool_terminal(ToolkitCommandKind::AvailableTools, failed()),
            ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::AuthorizationFailed)
        ));
        assert_eq!(
            TOOLKIT_RUN_AUTHORIZATION_FAILED_MESSAGE,
            "The platform could not authorize this tool run's credentials or context."
        );
    }

    #[test]
    fn marks_large_results_without_returning_partial_json() {
        let summary = call_summary(Ok(json!("x".repeat(48 * 1024)))).expect("summary");
        assert!(summary.truncated);
        assert!(summary.result_json.is_empty());
        assert_eq!(summary.status, ToolkitCallToolStatusV1::Ok as i32);
    }
}

/// Discovery can finish with a public consent challenge instead of a tool list.
fn discovery_authorization_result(
    challenge: &ToolkitAuthorizationRequiredV1,
) -> Result<serde_json::Value, RuntimeFailureKind> {
    let metadata = if challenge.resource_metadata_json.is_empty() {
        serde_json::json!({})
    } else {
        serde_json::from_slice::<serde_json::Value>(&challenge.resource_metadata_json)
            .map_err(|_| RuntimeFailureKind::InvalidInput)?
    };
    Ok(serde_json::json!({
        "tools": [], "args_schemas": {},
        "authorization_required": {
            "toolkit_id": challenge.toolkit_id,
            "toolkit_name": challenge.toolkit_name,
            "toolkit_type": challenge.toolkit_type,
            "server_url": challenge.server_url,
            "resource_metadata_url": challenge.resource_metadata_url,
            "resource_metadata": metadata,
        }
    }))
}

#[cfg(test)]
mod discovery_authorization_tests {
    use super::*;

    #[test]
    fn discovery_preserves_public_challenge_without_tool_results() {
        let challenge = ToolkitAuthorizationRequiredV1 {
            toolkit_id: "57".into(),
            toolkit_name: "mcp".into(),
            toolkit_type: "mcp".into(),
            server_url: "https://example.invalid/mcp".into(),
            resource_metadata_url: String::new(),
            resource_metadata_json: br#"{"authorization_servers":["https://example.invalid"]}"#
                .to_vec(),
        };
        let result = discovery_authorization_result(&challenge).expect("public challenge");
        assert_eq!(result["authorization_required"]["toolkit_id"], "57");
        assert_eq!(
            result["authorization_required"]["resource_metadata"]["authorization_servers"][0],
            "https://example.invalid"
        );
        assert!(result["tools"].as_array().unwrap().is_empty());
        assert!(result["args_schemas"].as_object().unwrap().is_empty());
        let invalid = ToolkitAuthorizationRequiredV1 {
            resource_metadata_json: b"invalid".to_vec(),
            ..challenge
        };
        assert!(discovery_authorization_result(&invalid).is_err());
    }
}
