use prost::Message;
use ring::{digest, hmac, signature};
use subtle::ConstantTimeEq;

use super::wire::{ScannedFields, Schema, scan_message};
use super::{
    ProtocolError,
    elitea::runtime::v1::{
        AgentExecutionCommandV1, ConfigurationValidationCommandV1, DigestAlgorithmV1, DigestV1,
        ExecutionInputBundleReferenceV1, IndexIngestCommandV1, SignatureProfileV1,
        SignedWorkerCommandEnvelopeV1, WorkerCommandTypeV1, WorkerCommandV1, worker_command_v1,
    },
};
use crate::agents::AgentExecutionKind;

pub const ENVELOPE_SCHEMA_REVISION: &str = "elitea.runtime.signed-worker-command.v1";
pub const PROTOCOL_REVISION: &str = "elitea.runtime.v1";
pub const LIMITS_REVISION: &str = "elitea.runtime.limits.conformance.v3";
pub const AGENT_EXECUTION_CAPABILITY_VERSION: &str = "1";
pub const AGENT_EXECUTE_APPLICATION_CAPABILITY_ID: &str = "agent.execute.application.v1";
pub const AGENT_EXECUTE_ADHOC_CAPABILITY_ID: &str = "agent.execute.adhoc.v1";
pub const TOOLKIT_EXECUTE_READ_CAPABILITY_ID: &str = "toolkit.execute.read.v1";
pub const TOOLKIT_EXECUTE_READ_CAPABILITY_VERSION: &str = "1";
pub const TOOLKIT_CALL_TOOL_CAPABILITY_ID: &str = "toolkit.call_tool.v1";
pub const TOOLKIT_AVAILABLE_TOOLS_CAPABILITY_ID: &str = "toolkit.available_tools.v1";
pub const CONFIGURATION_VALIDATE_CAPABILITY_ID: &str = "configuration.validate.v1";
pub const CONFIGURATION_VALIDATE_CAPABILITY_VERSION: &str = "1";
pub const INDEX_INGEST_CAPABILITY_ID: &str = "index.ingest.v1";
/// The version elitea-main dispatches and the Python worker accepts
/// (`elitea_worker/constants.py`).
pub const INDEX_INGEST_CAPABILITY_VERSION: &str = "2";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ToolkitCommandKind {
    ExecuteRead,
    CallTool,
    AvailableTools,
    /// `configuration.validate.v1`: a schema check of one SDK configuration.
    /// It shares the direct-execution claim, lease, output and retirement
    /// machinery with the toolkit kinds; only its input, result and logical
    /// output identity differ.
    ConfigurationValidate,
    /// `index.ingest.v1` (ADR-0030 I1): one toolkit index run. It rides the
    /// same claim, lease, output and retirement machinery; its inputs are the
    /// index bundle's entries, its result the typed `IndexIngestResultV1`.
    IndexIngest,
}

impl ToolkitCommandKind {
    #[must_use]
    pub const fn output_prefix(self) -> &'static str {
        match self {
            Self::ExecuteRead => "toolkit-execute-read",
            Self::CallTool => "toolkit-call-tool",
            Self::AvailableTools => "toolkit-available-tools",
            Self::ConfigurationValidate => "configuration-validation",
            Self::IndexIngest => "index-ingest",
        }
    }
}

/// The logical output id Main expects for a command's terminal frame
/// (`ExpectedRuntimeFailure` in the output inbox). Configuration validation is
/// keyed by its configuration revision; every toolkit kind by its execution;
/// agent executions by theirs. This is the one place the rule lives.
#[must_use]
pub(crate) fn terminal_logical_output_id(command: &WorkerCommandV1) -> String {
    use worker_command_v1::CapabilityCommand;
    let (prefix, key) = match command.capability_command.as_ref() {
        Some(CapabilityCommand::ConfigurationValidation(validation)) => (
            ToolkitCommandKind::ConfigurationValidate.output_prefix(),
            validation.configuration_revision_id.as_str(),
        ),
        Some(CapabilityCommand::ToolkitExecuteRead(_)) => (
            ToolkitCommandKind::ExecuteRead.output_prefix(),
            command.execution_id.as_str(),
        ),
        Some(CapabilityCommand::ToolkitCallTool(_)) => (
            ToolkitCommandKind::CallTool.output_prefix(),
            command.execution_id.as_str(),
        ),
        Some(CapabilityCommand::ToolkitAvailableTools(_)) => (
            ToolkitCommandKind::AvailableTools.output_prefix(),
            command.execution_id.as_str(),
        ),
        Some(CapabilityCommand::IndexIngest(_)) => (
            ToolkitCommandKind::IndexIngest.output_prefix(),
            command.execution_id.as_str(),
        ),
        _ => ("agent-execution", command.execution_id.as_str()),
    };
    format!("{prefix}:{key}")
}

const MAX_SIGNED_ENVELOPE_BYTES: usize = 48 * 1024;
const MAX_WORKER_COMMAND_BYTES: usize = 32 * 1024;
const MAX_INPUT_MANIFEST_BYTES: u64 = 64 * 1024;
const MAX_SAFE_STRING_BYTES: usize = 256;
const MAX_TRACE_STATE_BYTES: usize = 512;
const INPUT_BUNDLE_MEDIA_TYPE: &str = "application/x-protobuf";
const ED25519_DOMAIN: &[u8] = b"elitea.runtime.worker-command.ed25519.v1\0";

const CONFORMANCE_HMAC_KEY_ID: &str = "elitea-runtime-v1-conformance-hmac";
const CONFORMANCE_HMAC_KEY: &[u8] = b"ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET";

/// Authenticated command material shared by capability-specific wrappers.
///
/// This type stays private so a command is useful only after its selected
/// capability has passed strict semantic validation.
struct VerifiedWorkerCommand {
    exact_signed_envelope: Box<[u8]>,
    signed: SignedWorkerCommandEnvelopeV1,
    command: WorkerCommandV1,
}

/// The sealed common view needed by claim, settlement and command retirement.
/// Capability code receives a concrete wrapper and cannot reinterpret one
/// authenticated command as another capability after verification.
pub(crate) trait VerifiedExecutionCommand {
    fn exact_signed_envelope(&self) -> &[u8];
    fn signed(&self) -> &SignedWorkerCommandEnvelopeV1;
    fn command(&self) -> &WorkerCommandV1;
}

/// An authenticated, strictly decoded reference-only agent command.
pub struct VerifiedAgentCommand {
    common: VerifiedWorkerCommand,
    kind: AgentExecutionKind,
}

impl VerifiedAgentCommand {
    #[must_use]
    pub const fn signed(&self) -> &SignedWorkerCommandEnvelopeV1 {
        &self.common.signed
    }

    /// Return the exact authenticated outer-envelope bytes received from the
    /// command transport. This is crate-private because protocol consumers
    /// must not substitute a protobuf reserialization for the signed delivery.
    #[must_use]
    pub(crate) fn exact_signed_envelope(&self) -> &[u8] {
        &self.common.exact_signed_envelope
    }

    #[must_use]
    pub const fn command(&self) -> &WorkerCommandV1 {
        &self.common.command
    }

    #[must_use]
    pub const fn kind(&self) -> AgentExecutionKind {
        self.kind
    }
}

impl VerifiedExecutionCommand for VerifiedAgentCommand {
    fn exact_signed_envelope(&self) -> &[u8] {
        self.exact_signed_envelope()
    }

    fn signed(&self) -> &SignedWorkerCommandEnvelopeV1 {
        self.signed()
    }

    fn command(&self) -> &WorkerCommandV1 {
        self.command()
    }
}

/// An authenticated, strictly decoded toolkit command with immutable input references.
pub struct VerifiedToolkitExecuteReadCommand {
    common: VerifiedWorkerCommand,
    kind: ToolkitCommandKind,
}

pub type VerifiedToolkitCommand = VerifiedToolkitExecuteReadCommand;

/// Exhaustive authenticated command kind accepted by this worker binary.
/// The envelope is authenticated once before capability-specific decoding.
pub(crate) enum VerifiedExecutionCommandKind {
    Agent(VerifiedAgentCommand),
    ToolkitExecuteRead(VerifiedToolkitExecuteReadCommand),
}

impl VerifiedToolkitExecuteReadCommand {
    #[must_use]
    pub const fn signed(&self) -> &SignedWorkerCommandEnvelopeV1 {
        &self.common.signed
    }

    #[must_use]
    pub(crate) fn exact_signed_envelope(&self) -> &[u8] {
        &self.common.exact_signed_envelope
    }

    #[must_use]
    pub const fn command(&self) -> &WorkerCommandV1 {
        &self.common.command
    }

    #[must_use]
    pub const fn kind(&self) -> ToolkitCommandKind {
        self.kind
    }

    #[must_use]
    pub fn request_entry_id(&self) -> &str {
        match self.common.command.capability_command.as_ref() {
            Some(worker_command_v1::CapabilityCommand::ToolkitExecuteRead(command)) => {
                &command.request_entry_id
            }
            Some(worker_command_v1::CapabilityCommand::ToolkitCallTool(command)) => {
                &command.settings_entry_id
            }
            Some(worker_command_v1::CapabilityCommand::ToolkitAvailableTools(command)) => {
                &command.settings_entry_id
            }
            Some(worker_command_v1::CapabilityCommand::IndexIngest(command)) => {
                &command.toolkit_configuration_entry_id
            }
            Some(worker_command_v1::CapabilityCommand::ConfigurationValidation(command)) => {
                &command.settings_entry_id
            }
            _ => unreachable!("verified toolkit command lost its capability wrapper"),
        }
    }

    /// The logical output id Main expects for this command's terminal frame;
    /// see `terminal_logical_output_id`.
    #[must_use]
    pub fn logical_output_id(&self) -> String {
        terminal_logical_output_id(&self.common.command)
    }
}

impl VerifiedExecutionCommand for VerifiedToolkitExecuteReadCommand {
    fn exact_signed_envelope(&self) -> &[u8] {
        self.exact_signed_envelope()
    }

    fn signed(&self) -> &SignedWorkerCommandEnvelopeV1 {
        self.signed()
    }

    fn command(&self) -> &WorkerCommandV1 {
        self.command()
    }
}

/// Verifies an already digest-bound immutable command.
pub trait SignedCommandAuthenticator: Send + Sync {
    /// Authenticate the exact `worker_command_bytes` without reserialization.
    ///
    /// # Errors
    ///
    /// Returns [`ProtocolError::AuthorizationFailed`] for an unacceptable
    /// profile, key identity, or signature.
    fn authenticate(&self, signed: &SignedWorkerCommandEnvelopeV1) -> Result<(), ProtocolError>;
}

/// Resolves one exact Ed25519 key ID. There is deliberately no default key.
pub trait Ed25519PublicKeyResolver: Send + Sync {
    fn resolve_ed25519_public_key(&self, key_id: &str) -> Option<[u8; 32]>;
}

pub struct Ed25519CommandAuthenticator<R> {
    resolver: R,
}

impl<R> Ed25519CommandAuthenticator<R> {
    pub const fn new(resolver: R) -> Self {
        Self { resolver }
    }
}

impl<R> SignedCommandAuthenticator for Ed25519CommandAuthenticator<R>
where
    R: Ed25519PublicKeyResolver,
{
    fn authenticate(&self, signed: &SignedWorkerCommandEnvelopeV1) -> Result<(), ProtocolError> {
        if signed.signature_profile != SignatureProfileV1::Ed25519 as i32
            || signed.key_id.is_empty()
            || signed.key_id.len() > MAX_SAFE_STRING_BYTES
            || signed
                .key_id
                .bytes()
                .any(|byte| matches!(byte, b'\r' | b'\n' | b'\0'))
            || signed.signature.len() != 64
        {
            return Err(ProtocolError::AuthorizationFailed(
                "the production command signature profile is not accepted",
            ));
        }
        let public_key = self
            .resolver
            .resolve_ed25519_public_key(&signed.key_id)
            .ok_or(ProtocolError::AuthorizationFailed(
                "the worker command signature is invalid",
            ))?;
        let signing_input = ed25519_signing_input(&signed.worker_command_bytes)?;
        signature::UnparsedPublicKey::new(&signature::ED25519, public_key)
            .verify(&signing_input, &signed.signature)
            .map_err(|_| {
                ProtocolError::AuthorizationFailed("the worker command signature is invalid")
            })
    }
}

/// Public offline conformance profile. Production composition must never
/// construct this authenticator.
pub struct TestOnlyConformanceHmacAuthenticator;

impl SignedCommandAuthenticator for TestOnlyConformanceHmacAuthenticator {
    fn authenticate(&self, signed: &SignedWorkerCommandEnvelopeV1) -> Result<(), ProtocolError> {
        if signed.signature_profile != SignatureProfileV1::TestOnlyHmacSha256 as i32
            || signed.key_id != CONFORMANCE_HMAC_KEY_ID
        {
            return Err(ProtocolError::AuthorizationFailed(
                "the offline command signature profile is not accepted",
            ));
        }
        let key = hmac::Key::new(hmac::HMAC_SHA256, CONFORMANCE_HMAC_KEY);
        hmac::verify(&key, &signed.worker_command_bytes, &signed.signature).map_err(|_| {
            ProtocolError::AuthorizationFailed("the worker command signature is invalid")
        })
    }
}

/// Verify exact signed bytes before decoding the reference-only agent command.
///
/// # Errors
///
/// Returns a bounded [`ProtocolError`] for wire, version, digest, signature,
/// capability, or semantic validation failures. The command is never decoded
/// before its digest and signature pass.
pub fn parse_and_verify_agent_command(
    raw: &[u8],
    authenticator: Option<&dyn SignedCommandAuthenticator>,
) -> Result<VerifiedAgentCommand, ProtocolError> {
    let common = parse_and_verify_worker_command(raw, authenticator)?;
    scan_agent_worker_command(&common.signed.worker_command_bytes)?;
    let kind = validate_agent_command(&common.command)?;
    Ok(VerifiedAgentCommand { common, kind })
}

/// Verify exact signed bytes before decoding a supported toolkit command.
/// Authentication always precedes protobuf command decoding.
///
/// # Errors
///
/// Returns a bounded [`ProtocolError`] for wire, version, digest, signature,
/// capability, or semantic validation failures.
pub fn parse_and_verify_toolkit_execute_read_command(
    raw: &[u8],
    authenticator: Option<&dyn SignedCommandAuthenticator>,
) -> Result<VerifiedToolkitExecuteReadCommand, ProtocolError> {
    let common = parse_and_verify_worker_command(raw, authenticator)?;
    scan_toolkit_execute_read_worker_command(&common.signed.worker_command_bytes)?;
    let kind = validate_toolkit_execute_read_command(&common.command)?;
    Ok(VerifiedToolkitExecuteReadCommand { common, kind })
}

pub use parse_and_verify_toolkit_execute_read_command as parse_and_verify_toolkit_command;

/// Authenticate one delivery once and select its exact capability wrapper.
///
/// This is the production intake entrypoint. Capability-specific parsers stay
/// available for focused protocol consumers and conformance tests.
pub(crate) fn parse_and_verify_execution_command(
    raw: &[u8],
    authenticator: Option<&dyn SignedCommandAuthenticator>,
) -> Result<VerifiedExecutionCommandKind, ProtocolError> {
    let common = parse_and_verify_worker_command(raw, authenticator)?;
    match common.command.capability_command.as_ref() {
        Some(worker_command_v1::CapabilityCommand::AgentExecution(_)) => {
            scan_agent_worker_command(&common.signed.worker_command_bytes)?;
            let kind = validate_agent_command(&common.command)?;
            Ok(VerifiedExecutionCommandKind::Agent(VerifiedAgentCommand {
                common,
                kind,
            }))
        }
        Some(
            worker_command_v1::CapabilityCommand::ToolkitExecuteRead(_)
            | worker_command_v1::CapabilityCommand::ToolkitCallTool(_)
            | worker_command_v1::CapabilityCommand::ToolkitAvailableTools(_)
            | worker_command_v1::CapabilityCommand::ConfigurationValidation(_)
            | worker_command_v1::CapabilityCommand::IndexIngest(_),
        ) => {
            scan_toolkit_execute_read_worker_command(&common.signed.worker_command_bytes)?;
            let kind = validate_toolkit_execute_read_command(&common.command)?;
            Ok(VerifiedExecutionCommandKind::ToolkitExecuteRead(
                VerifiedToolkitExecuteReadCommand { common, kind },
            ))
        }
        _ => Err(ProtocolError::UnsupportedCapability(
            "the worker command capability is not supported",
        )),
    }
}

fn parse_and_verify_worker_command(
    raw: &[u8],
    authenticator: Option<&dyn SignedCommandAuthenticator>,
) -> Result<VerifiedWorkerCommand, ProtocolError> {
    if raw.is_empty() || raw.len() > MAX_SIGNED_ENVELOPE_BYTES {
        return Err(ProtocolError::ResourceExhausted(
            "the signed command envelope exceeds the conformance limit",
        ));
    }
    scan_signed_command_envelope(raw)?;
    let signed = SignedWorkerCommandEnvelopeV1::decode(raw).map_err(|_| {
        ProtocolError::InvalidInput("the signed worker command envelope is malformed")
    })?;
    if signed.envelope_schema_revision != ENVELOPE_SCHEMA_REVISION {
        return Err(ProtocolError::IncompatibleVersion(
            "the signed command envelope revision is not compatible",
        ));
    }
    require_sha256(
        signed.worker_command_digest.as_ref(),
        "the worker command digest is malformed",
    )?;
    if signed.worker_command_bytes.is_empty()
        || signed.worker_command_bytes.len() > MAX_WORKER_COMMAND_BYTES
    {
        return Err(ProtocolError::ResourceExhausted(
            "the worker command exceeds the conformance limit",
        ));
    }
    let Some(expected_digest) = signed.worker_command_digest.as_ref() else {
        return Err(ProtocolError::InvalidInput(
            "the worker command digest is malformed",
        ));
    };
    let calculated_digest = digest::digest(&digest::SHA256, &signed.worker_command_bytes);
    if calculated_digest
        .as_ref()
        .ct_eq(&expected_digest.value)
        .unwrap_u8()
        != 1
    {
        return Err(ProtocolError::AuthorizationFailed(
            "the worker command digest is invalid",
        ));
    }
    authenticator
        .ok_or(ProtocolError::AuthorizationFailed(
            "no production signed-command authenticator is configured",
        ))?
        .authenticate(&signed)?;

    let command = WorkerCommandV1::decode(signed.worker_command_bytes.as_slice())
        .map_err(|_| ProtocolError::InvalidInput("the worker command is malformed"))?;
    Ok(VerifiedWorkerCommand {
        exact_signed_envelope: raw.into(),
        signed,
        command,
    })
}

fn scan_signed_command_envelope(raw: &[u8]) -> Result<(), ProtocolError> {
    let fields = scan_message(raw, Schema::SignedCommandEnvelope)?;
    let digest = fields.length_field(5, "the worker command digest is missing")?;
    scan_message(digest, Schema::Digest)?;
    Ok(())
}

fn scan_agent_worker_command(raw: &[u8]) -> Result<(), ProtocolError> {
    let fields = scan_message(raw, Schema::WorkerCommand)?;
    scan_security_context_reference(&fields)?;
    scan_egress_policy(&fields)?;
    scan_delegation_policy(&fields)?;
    scan_approval_policy(&fields)?;
    scan_run_caps(&fields)?;
    scan_output_contracts(&fields)?;
    scan_content_controls(&fields)?;
    let input_reference = fields.length_field(16, "the input bundle reference is missing")?;
    let input_fields = scan_message(input_reference, Schema::InputBundleReference)?;
    let input_digest = input_fields.length_field(3, "the input bundle digest is missing")?;
    scan_message(input_digest, Schema::Digest)?;
    if fields.contains(35) {
        let agent_command = fields.length_field(35, "the agent execution command is missing")?;
        scan_message(agent_command, Schema::AgentExecutionCommand)?;
    }
    Ok(())
}

fn scan_toolkit_execute_read_worker_command(raw: &[u8]) -> Result<(), ProtocolError> {
    let fields = scan_message(raw, Schema::WorkerCommand)?;
    scan_security_context_reference(&fields)?;
    scan_egress_policy(&fields)?;
    let input_reference = fields.length_field(16, "the input bundle reference is missing")?;
    let input_fields = scan_message(input_reference, Schema::InputBundleReference)?;
    let input_digest = input_fields.length_field(3, "the input bundle digest is missing")?;
    scan_message(input_digest, Schema::Digest)?;
    if fields.contains(32) {
        let validation =
            fields.length_field(32, "the configuration validation command is missing")?;
        let validation_fields = scan_message(validation, Schema::ConfigurationValidationCommand)?;
        for digest_field in [4, 7] {
            let digest = validation_fields.length_field(
                digest_field,
                "the configuration validation digest is missing",
            )?;
            scan_message(digest, Schema::Digest)?;
        }
        return Ok(());
    }
    if fields.contains(34) {
        let index = fields.length_field(34, "the index ingest command is missing")?;
        let index_fields = scan_message(index, Schema::IndexIngestCommand)?;
        if index_fields.contains(9) {
            let binding = index_fields.length_field(9, "the embedding binding is missing")?;
            let binding_fields = scan_message(binding, Schema::IndexIngestInputBinding)?;
            let digest =
                binding_fields.length_field(3, "the embedding binding digest is missing")?;
            scan_message(digest, Schema::Digest)?;
        }
        return Ok(());
    }
    let (tag, schema) = if fields.contains(64) {
        (64, Schema::ToolkitExecuteReadCommand)
    } else if fields.contains(36) {
        (36, Schema::ToolkitCallToolCommand)
    } else {
        (33, Schema::ToolkitAvailableToolsCommand)
    };
    let toolkit_command = fields.length_field(tag, "the toolkit command is missing")?;
    scan_message(toolkit_command, schema)?;
    Ok(())
}

/// Field 65 (ADR-0039, #1115): its nested reference and digest admit no
/// unknown field either.
fn scan_security_context_reference(fields: &ScannedFields<'_>) -> Result<(), ProtocolError> {
    if fields.contains(65) {
        super::security_context::scan_reference(
            fields.length_field(65, "the security context reference is missing")?,
        )?;
    }
    Ok(())
}

/// Field 66 (ADR-0044 decision 2, #1490): the policy admits no unknown field.
fn scan_egress_policy(fields: &ScannedFields<'_>) -> Result<(), ProtocolError> {
    if fields.contains(66) {
        scan_message(
            fields.length_field(66, "the worker egress policy is missing")?,
            Schema::WorkerEgressPolicy,
        )?;
    }
    Ok(())
}

/// Field 67 (ADR-0044 decision 3, #1501): the policy admits no unknown
/// field. Only an agent execution carries it.
fn scan_delegation_policy(fields: &ScannedFields<'_>) -> Result<(), ProtocolError> {
    if fields.contains(67) {
        scan_message(
            fields.length_field(67, "the worker delegation policy is missing")?,
            Schema::WorkerDelegationPolicy,
        )?;
    }
    Ok(())
}

/// Field 68 (ADR-0041 decision 5, #1498): the policy admits no unknown
/// field. Only an agent execution carries it.
fn scan_approval_policy(fields: &ScannedFields<'_>) -> Result<(), ProtocolError> {
    if fields.contains(68) {
        scan_message(
            fields.length_field(68, "the worker approval policy is missing")?,
            Schema::WorkerApprovalPolicy,
        )?;
    }
    Ok(())
}

/// Field 69 (ADR-0041 decision 3, #1498): the run caps admit no unknown
/// field. Only an agent execution carries them.
fn scan_run_caps(fields: &ScannedFields<'_>) -> Result<(), ProtocolError> {
    if fields.contains(69) {
        scan_message(
            fields.length_field(69, "the worker run caps are missing")?,
            Schema::WorkerRunCaps,
        )?;
    }
    Ok(())
}

/// Field 70 (ADR-0043 decision 2, #1500): the output contracts admit no
/// unknown field. Only an agent execution whose frozen definition holds an
/// output contract carries them.
fn scan_output_contracts(fields: &ScannedFields<'_>) -> Result<(), ProtocolError> {
    if fields.contains(70) {
        scan_message(
            fields.length_field(70, "the worker output contracts are missing")?,
            Schema::WorkerOutputContracts,
        )?;
    }
    Ok(())
}

/// The output contract grammar revision this worker enforces (`json_schema`,
/// regex and the json, markdown and csv formats).
pub(crate) const OUTPUT_CONTRACTS_REVISION: u32 = 1;

/// Field 71 (ADR-0043 decisions 3 and 4, #1500): the content controls
/// admit no unknown field. Only an agent execution carries them.
fn scan_content_controls(fields: &ScannedFields<'_>) -> Result<(), ProtocolError> {
    if fields.contains(71) {
        scan_message(
            fields.length_field(71, "the worker content controls are missing")?,
            Schema::WorkerContentControls,
        )?;
    }
    Ok(())
}

fn validate_agent_command(command: &WorkerCommandV1) -> Result<AgentExecutionKind, ProtocolError> {
    if command.protocol_revision != PROTOCOL_REVISION || command.limits_revision != LIMITS_REVISION
    {
        return Err(ProtocolError::IncompatibleVersion(
            "the requested contract version is not compatible",
        ));
    }
    let (agent, kind) = select_agent_entrypoint(command)?;
    if command.capability_version != AGENT_EXECUTION_CAPABILITY_VERSION {
        return Err(ProtocolError::UnsupportedCapability(
            "the worker command capability version is not supported",
        ));
    }
    let input = command
        .input_bundle_ref
        .as_ref()
        .ok_or(ProtocolError::InvalidInput(
            "the worker command is missing a required reference or identity",
        ))?;
    validate_command_strings(command, input, agent)?;
    validate_command_invariants(command, input, agent)?;
    super::security_context::SecurityContextReference::from_command(command)?;
    super::egress::policy_from_command(command)?;
    super::delegation::policy_from_command(command)?;
    super::approval_policy::policy_from_command(command)?;
    // A revision this worker does not know names a contract it cannot
    // enforce: refused, never ignored.
    if command
        .output_contracts
        .as_ref()
        .is_some_and(|contracts| contracts.revision != OUTPUT_CONTRACTS_REVISION)
    {
        return Err(ProtocolError::IncompatibleVersion(
            "the output contract revision is not supported",
        ));
    }
    super::preamble::validate_command(command)?;
    Ok(kind)
}

#[allow(clippy::too_many_lines)] // One arm per direct-execution kind; each is a few lines.
fn validate_toolkit_execute_read_command(
    command: &WorkerCommandV1,
) -> Result<ToolkitCommandKind, ProtocolError> {
    if command.protocol_revision != PROTOCOL_REVISION || command.limits_revision != LIMITS_REVISION
    {
        return Err(ProtocolError::IncompatibleVersion(
            "the requested contract version is not compatible",
        ));
    }
    let (kind, required) = match &command.capability_command {
        Some(worker_command_v1::CapabilityCommand::ToolkitExecuteRead(toolkit))
            if command.capability_id == TOOLKIT_EXECUTE_READ_CAPABILITY_ID
                && command.command_type == WorkerCommandTypeV1::ToolkitExecuteRead as i32 =>
        {
            (
                ToolkitCommandKind::ExecuteRead,
                vec![toolkit.request_entry_id.as_str()],
            )
        }
        Some(worker_command_v1::CapabilityCommand::ToolkitCallTool(toolkit))
            if command.capability_id == TOOLKIT_CALL_TOOL_CAPABILITY_ID
                && command.command_type == WorkerCommandTypeV1::ToolkitCallTool as i32 =>
        {
            if toolkit.settings_entry_id == toolkit.arguments_entry_id {
                return Err(ProtocolError::InvalidInput(
                    "the toolkit input references must be distinct",
                ));
            }
            let mut required = vec![
                toolkit.toolkit_type.as_str(),
                toolkit.settings_entry_id.as_str(),
                toolkit.tool_name.as_str(),
                toolkit.arguments_entry_id.as_str(),
            ];
            for value in [
                &toolkit.toolkit_id,
                &toolkit.toolkit_version,
                &toolkit.attribution_execution_id,
            ] {
                if !value.is_empty() {
                    required.push(value.as_str());
                }
            }
            (ToolkitCommandKind::CallTool, required)
        }
        Some(worker_command_v1::CapabilityCommand::ToolkitAvailableTools(toolkit))
            if command.capability_id == TOOLKIT_AVAILABLE_TOOLS_CAPABILITY_ID
                && command.command_type == WorkerCommandTypeV1::ToolkitAvailableTools as i32 =>
        {
            (
                ToolkitCommandKind::AvailableTools,
                vec![
                    toolkit.toolkit_type.as_str(),
                    toolkit.settings_entry_id.as_str(),
                ],
            )
        }
        Some(worker_command_v1::CapabilityCommand::ConfigurationValidation(validation))
            if command.capability_id == CONFIGURATION_VALIDATE_CAPABILITY_ID
                && command.command_type == WorkerCommandTypeV1::ConfigurationValidate as i32 =>
        {
            (
                ToolkitCommandKind::ConfigurationValidate,
                configuration_validation_identities(validation)?,
            )
        }
        Some(worker_command_v1::CapabilityCommand::IndexIngest(index))
            if command.capability_id == INDEX_INGEST_CAPABILITY_ID
                && command.command_type == WorkerCommandTypeV1::IndexIngest as i32 =>
        {
            (
                ToolkitCommandKind::IndexIngest,
                index_ingest_identities(index)?,
            )
        }
        _ => {
            return Err(ProtocolError::UnsupportedCapability(
                "the worker command capability is not supported",
            ));
        }
    };
    let expected_version = match kind {
        ToolkitCommandKind::ConfigurationValidate => CONFIGURATION_VALIDATE_CAPABILITY_VERSION,
        ToolkitCommandKind::IndexIngest => INDEX_INGEST_CAPABILITY_VERSION,
        ToolkitCommandKind::ExecuteRead
        | ToolkitCommandKind::CallTool
        | ToolkitCommandKind::AvailableTools => TOOLKIT_EXECUTE_READ_CAPABILITY_VERSION,
    };
    if command.capability_version != expected_version {
        return Err(ProtocolError::UnsupportedCapability(
            "the worker command capability version is not supported",
        ));
    }
    let input = command
        .input_bundle_ref
        .as_ref()
        .ok_or(ProtocolError::InvalidInput(
            "the worker command is missing a required reference or identity",
        ))?;
    validate_common_command_strings(command, input, &required)?;
    if required.iter().any(|value| {
        value
            .bytes()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
    }) {
        return Err(ProtocolError::InvalidInput(
            "a toolkit command identity is malformed",
        ));
    }
    validate_common_command_invariants(command, input, "the toolkit root identity is malformed")?;
    super::security_context::SecurityContextReference::from_command(command)?;
    super::egress::policy_from_command(command)?;
    Ok(kind)
}

/// The identities a configuration-validation command must carry, after its two
/// digests have proven to be SHA-256.
fn configuration_validation_identities(
    validation: &ConfigurationValidationCommandV1,
) -> Result<Vec<&str>, ProtocolError> {
    require_sha256(
        validation.catalog_digest.as_ref(),
        "the configuration catalog digest is malformed",
    )?;
    require_sha256(
        validation.schema_digest.as_ref(),
        "the configuration schema digest is malformed",
    )?;
    Ok(vec![
        validation.configuration_revision_id.as_str(),
        validation.configuration_type.as_str(),
        validation.catalog_revision.as_str(),
        validation.schema_id.as_str(),
        validation.schema_revision.as_str(),
        validation.settings_entry_id.as_str(),
    ])
}

/// The identities an index ingest command must carry (the Python worker's
/// `_accepted_index_claim` and Main's control verifier, in one place): the two
/// required entry ids, the optional ones when named, distinct among
/// themselves; a closed initiator and `sio_event`; the expected embedding
/// space as a pair or absent; the index id as a canonical UUID; the embedding
/// binding, when present, with a SHA-256 content digest.
///
/// A command without an index id was dispatched under the retired Python
/// indexing runtime (`ELITEA_INDEXING_RUNTIME=python`), whose index has no
/// registry row and whose vectors lived in a per-project pgvector database
/// this worker never touches. It is refused as [`ProtocolError::UnsupportedCapability`] before
/// any claim, exactly as this worker refused every index command before it
/// served the capability: the delivery is dead-lettered as an unsupported
/// command, never claimed and never failed in the user's name.
fn index_ingest_identities(index: &IndexIngestCommandV1) -> Result<Vec<&str>, ProtocolError> {
    let malformed = || ProtocolError::InvalidInput("the index ingest command is malformed");
    if index.index_id.is_empty() {
        return Err(ProtocolError::UnsupportedCapability(
            "index.ingest.v1 without a registry index is the python runtime's; this worker serves the rust indexing runtime only",
        ));
    }
    let mut required = vec![
        index.toolkit_configuration_entry_id.as_str(),
        index.tool_parameters_entry_id.as_str(),
        index.initiator.as_str(),
    ];
    let mut entry_ids = vec![
        index.toolkit_configuration_entry_id.as_str(),
        index.tool_parameters_entry_id.as_str(),
    ];
    for optional in [
        &index.llm_model_entry_id,
        &index.llm_configuration_entry_id,
        &index.mcp_tokens_entry_id,
    ] {
        if !optional.is_empty() {
            required.push(optional.as_str());
            entry_ids.push(optional.as_str());
        }
    }
    if let Some(binding) = index.embedding_binding.as_ref() {
        require_sha256(
            binding.content_digest.as_ref(),
            "the embedding binding digest is malformed",
        )?;
        required.push(binding.entry_id.as_str());
        required.push(binding.immutable_version.as_str());
        entry_ids.push(binding.entry_id.as_str());
    }
    for (position, entry_id) in entry_ids.iter().enumerate() {
        if !entry_id.is_empty() && entry_ids[..position].contains(entry_id) {
            return Err(ProtocolError::InvalidInput(
                "the index input references must be distinct",
            ));
        }
    }
    if !matches!(
        index.sio_event.as_str(),
        "" | "chat_predict" | "test_toolkit_tool"
    ) || !matches!(index.initiator.as_str(), "user" | "llm" | "schedule")
    {
        return Err(malformed());
    }
    let model = index.expected_embedding_model.as_str();
    let dimension = index.expected_embedding_dimension;
    if (model.is_empty()) != (dimension == 0)
        || dimension > 65_535
        || model.len() > MAX_SAFE_STRING_BYTES
        || model
            .bytes()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
    {
        return Err(malformed());
    }
    if !canonical_uuid(&index.index_id) {
        return Err(malformed());
    }
    for text in [&index.client_stream_id, &index.client_message_id] {
        if text.len() > MAX_SAFE_STRING_BYTES
            || text
                .bytes()
                .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
        {
            return Err(malformed());
        }
    }
    Ok(required)
}

/// `8-4-4-4-12` lower-case hex: the text form elitea-vector accepts for a
/// namespace id.
pub(crate) fn canonical_uuid(value: &str) -> bool {
    let bytes = value.as_bytes();
    bytes.len() == 36
        && bytes.iter().enumerate().all(|(position, byte)| {
            if matches!(position, 8 | 13 | 18 | 23) {
                *byte == b'-'
            } else {
                byte.is_ascii_digit() || (b'a'..=b'f').contains(byte)
            }
        })
}

fn select_agent_entrypoint(
    command: &WorkerCommandV1,
) -> Result<(&AgentExecutionCommandV1, AgentExecutionKind), ProtocolError> {
    match &command.capability_command {
        Some(worker_command_v1::CapabilityCommand::AgentExecution(agent))
            if command.capability_id == AGENT_EXECUTE_APPLICATION_CAPABILITY_ID
                && command.command_type == WorkerCommandTypeV1::AgentExecuteApplication as i32 =>
        {
            Ok((agent, AgentExecutionKind::Application))
        }
        Some(worker_command_v1::CapabilityCommand::AgentExecution(agent))
            if command.capability_id == AGENT_EXECUTE_ADHOC_CAPABILITY_ID
                && command.command_type == WorkerCommandTypeV1::AgentExecuteAdhoc as i32 =>
        {
            Ok((agent, AgentExecutionKind::Adhoc))
        }
        _ => Err(ProtocolError::UnsupportedCapability(
            "the worker command capability is not supported",
        )),
    }
}

fn validate_command_strings(
    command: &WorkerCommandV1,
    input: &ExecutionInputBundleReferenceV1,
    agent: &AgentExecutionCommandV1,
) -> Result<(), ProtocolError> {
    validate_common_command_strings(
        command,
        input,
        &[
            agent.request_entry_id.as_str(),
            agent.client_stream_id.as_str(),
            agent.client_message_id.as_str(),
            agent.sio_event.as_str(),
        ],
    )
}

fn validate_common_command_strings(
    command: &WorkerCommandV1,
    input: &ExecutionInputBundleReferenceV1,
    capability_required: &[&str],
) -> Result<(), ProtocolError> {
    let required = [
        command.command_id.as_str(),
        command.idempotency_key.as_str(),
        command.execution_id.as_str(),
        command.root_execution_id.as_str(),
        command.tenant_id.as_str(),
        command.resource_project_id.as_str(),
        command.projection_project_id.as_str(),
        command.principal_ref.as_str(),
        input.input_bundle_id.as_str(),
        input.immutable_version.as_str(),
        input.media_type.as_str(),
        command.resource_class.as_str(),
        command.isolation_class.as_str(),
    ];
    if required.iter().any(|value| value.is_empty()) {
        return Err(ProtocolError::InvalidInput(
            "the worker command is missing a required reference or identity",
        ));
    }
    if capability_required.iter().any(|value| value.is_empty()) {
        return Err(ProtocolError::InvalidInput(
            "the worker command is missing a required reference or identity",
        ));
    }
    let bounded = required
        .into_iter()
        .chain(capability_required.iter().copied())
        .chain([
            command.capability_id.as_str(),
            command.capability_version.as_str(),
            command.protocol_revision.as_str(),
            command.limits_revision.as_str(),
            command.parent_execution_id.as_str(),
            command.parent_call_id.as_str(),
            command.traceparent.as_str(),
        ]);
    if bounded
        .into_iter()
        .any(|value| value.len() > MAX_SAFE_STRING_BYTES)
    {
        return Err(ProtocolError::ResourceExhausted(
            "a worker command reference exceeds the string limit",
        ));
    }
    if command.tracestate.len() > MAX_TRACE_STATE_BYTES {
        return Err(ProtocolError::ResourceExhausted(
            "the worker command trace state exceeds the string limit",
        ));
    }
    if command.parent_execution_id.is_empty() != command.parent_call_id.is_empty() {
        return Err(ProtocolError::InvalidInput(
            "the worker command parent identity is incomplete",
        ));
    }
    Ok(())
}

fn validate_command_invariants(
    command: &WorkerCommandV1,
    input: &ExecutionInputBundleReferenceV1,
    agent: &AgentExecutionCommandV1,
) -> Result<(), ProtocolError> {
    validate_common_command_invariants(
        command,
        input,
        "the agent-execution root identity is malformed",
    )?;
    if [
        agent.client_stream_id.as_str(),
        agent.client_message_id.as_str(),
    ]
    .into_iter()
    .any(|value| {
        value
            .bytes()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
    }) {
        return Err(ProtocolError::InvalidInput(
            "an agent-execution client correlation is malformed",
        ));
    }
    if !matches!(
        agent.sio_event.as_str(),
        "chat_predict" | "chat_continue_predict"
    ) {
        return Err(ProtocolError::InvalidInput(
            "the agent-execution event route is malformed",
        ));
    }
    Ok(())
}

fn validate_common_command_invariants(
    command: &WorkerCommandV1,
    input: &ExecutionInputBundleReferenceV1,
    root_error: &'static str,
) -> Result<(), ProtocolError> {
    if command.generation == 0
        || command.dispatch_ordinal == 0
        || command.priority == 0
        || command.deadline_unix_millis < 1
    {
        return Err(ProtocolError::InvalidInput(
            "the worker command scheduling identity is malformed",
        ));
    }
    if input.media_type != INPUT_BUNDLE_MEDIA_TYPE {
        return Err(ProtocolError::InvalidInput(
            "the input bundle reference has the wrong media type",
        ));
    }
    require_sha256(
        input.digest.as_ref(),
        "the input bundle digest is malformed",
    )?;
    if command.root_execution_id != command.execution_id
        || !command.parent_execution_id.is_empty()
        || !command.parent_call_id.is_empty()
    {
        return Err(ProtocolError::InvalidInput(root_error));
    }
    if input.byte_length == 0 || input.byte_length > MAX_INPUT_MANIFEST_BYTES {
        return Err(ProtocolError::ResourceExhausted(
            "the input bundle manifest exceeds the approved limit",
        ));
    }
    Ok(())
}

fn require_sha256(digest: Option<&DigestV1>, error: &'static str) -> Result<(), ProtocolError> {
    let Some(digest) = digest else {
        return Err(ProtocolError::InvalidInput(error));
    };
    if digest.algorithm != DigestAlgorithmV1::Sha256 as i32 || digest.value.len() != 32 {
        return Err(ProtocolError::InvalidInput(error));
    }
    Ok(())
}

fn ed25519_signing_input(exact_command_bytes: &[u8]) -> Result<Vec<u8>, ProtocolError> {
    if exact_command_bytes.is_empty() {
        return Err(ProtocolError::AuthorizationFailed(
            "the worker command signature is invalid",
        ));
    }
    let length = u64::try_from(exact_command_bytes.len()).map_err(|_| {
        ProtocolError::AuthorizationFailed("the worker command signature is invalid")
    })?;
    let mut input = Vec::with_capacity(ED25519_DOMAIN.len() + 8 + exact_command_bytes.len());
    input.extend_from_slice(ED25519_DOMAIN);
    input.extend_from_slice(&length.to_be_bytes());
    input.extend_from_slice(exact_command_bytes);
    Ok(input)
}
