//! `configuration.validate.v1` steps that need no lease, claim or transport:
//! binding a signed command to the pinned catalog and shaping the result.

use crate::protocol::command::VerifiedToolkitExecuteReadCommand;
use crate::protocol::elitea::runtime::v1::{
    ConfigurationValidationIssueV1, ConfigurationValidationResultV1, DigestV1,
    ExecutionInputBundleReferenceV1, ExecutionInputEntryV1, worker_command_v1,
};
use crate::protocol::output::RuntimeFailureKind;
use crate::validation::{BindingRefusal, ConfigurationCatalog, Issue, TypeRules};

/// Bind the signed command to the catalog this worker carries, before any
/// settings are fetched. A type outside the table is refused with the
/// registered `UNSUPPORTED_CAPABILITY` failure and its name in the log; it is
/// never answered as valid.
pub(super) fn select_rules(
    verified: &VerifiedToolkitExecuteReadCommand,
) -> Result<&'static TypeRules, RuntimeFailureKind> {
    let Some(worker_command_v1::CapabilityCommand::ConfigurationValidation(command)) =
        verified.command().capability_command.as_ref()
    else {
        return Err(RuntimeFailureKind::InvalidInput);
    };
    let Some(catalog) = ConfigurationCatalog::pinned() else {
        tracing::error!(
            event = "configuration_validation_catalog_unavailable",
            "the embedded configuration rules could not be loaded"
        );
        return Err(RuntimeFailureKind::Internal);
    };
    catalog.bind(command).map_err(|refusal| {
        match &refusal {
            BindingRefusal::UnsupportedConfigurationType { configuration_type } => {
                tracing::warn!(
                    event = "configuration_validation_unsupported_type",
                    configuration_type = %configuration_type,
                    "this worker cannot validate the configuration type"
                );
            }
            BindingRefusal::IncompatibleVersion => {
                tracing::warn!(
                    event = "configuration_validation_binding_refused",
                    refusal = ?refusal,
                );
            }
        }
        refusal.failure()
    })
}

/// Restate the command's identity and the admitted input binding next to the
/// verdict. The result carries no settings, no rejected value and no raw
/// validator text.
pub(super) fn bind_result(
    verified: &VerifiedToolkitExecuteReadCommand,
    bundle: &ExecutionInputBundleReferenceV1,
    entry: &ExecutionInputEntryV1,
    issues: &[Issue],
) -> Result<ConfigurationValidationResultV1, RuntimeFailureKind> {
    let Some(worker_command_v1::CapabilityCommand::ConfigurationValidation(command)) =
        verified.command().capability_command.as_ref()
    else {
        return Err(RuntimeFailureKind::InvalidInput);
    };
    let content = entry
        .content
        .as_ref()
        .ok_or(RuntimeFailureKind::InvalidInput)?;
    let digest = |value: Option<&DigestV1>| {
        value
            .cloned()
            .map(Some)
            .ok_or(RuntimeFailureKind::InvalidInput)
    };
    Ok(ConfigurationValidationResultV1 {
        configuration_revision_id: command.configuration_revision_id.clone(),
        configuration_type: command.configuration_type.clone(),
        catalog_revision: command.catalog_revision.clone(),
        catalog_digest: digest(command.catalog_digest.as_ref())?,
        schema_id: command.schema_id.clone(),
        schema_revision: command.schema_revision.clone(),
        schema_digest: digest(command.schema_digest.as_ref())?,
        input_bundle_id: bundle.input_bundle_id.clone(),
        input_bundle_digest: digest(bundle.digest.as_ref())?,
        settings_entry_id: entry.entry_id.clone(),
        settings_entry_version: entry.immutable_version.clone(),
        settings_content_digest: digest(content.digest.as_ref())?,
        valid: issues.is_empty(),
        issues: issues
            .iter()
            .map(|issue| ConfigurationValidationIssueV1 {
                code: issue.code.code().to_owned(),
                json_pointer: issue.json_pointer.clone(),
                safe_message: issue.code.safe_message().to_owned(),
            })
            .collect(),
    })
}

/// The cross-language golden vectors the Python worker is held to
/// (`testdata/proto/runtime/v1/configuration-validation`). The Rust worker must
/// produce the same terminal frame, byte for byte, for the same signed command
/// and settings: the verdict, the issue order, the identities, the digests, the
/// settlement proposal and the refusal text.
#[cfg(test)]
mod tests {
    use std::path::PathBuf;

    use prost::Message;
    use ring::digest;

    use super::*;
    use crate::protocol::command::{
        TestOnlyConformanceHmacAuthenticator, VerifiedExecutionCommandKind,
        parse_and_verify_execution_command,
    };
    use crate::protocol::elitea::runtime::v1::{ExecutionInputBundleV1, WorkerExecutionEnvelopeV1};
    use crate::protocol::output::{
        ToolkitExecuteReadTerminalOutput, build_toolkit_execute_read_terminal_output_frame,
    };

    fn read(name: &str, file: &str) -> Vec<u8> {
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../testdata/proto/runtime/v1/configuration-validation")
            .join(name)
            .join(file);
        std::fs::read(&path).unwrap_or_else(|error| panic!("{}: {error}", path.display()))
    }

    /// Run the worker's real binding, evaluation and frame construction over
    /// one golden fixture and return the frame bytes it would publish.
    fn produce(name: &str, cancelled: bool) -> Vec<u8> {
        let envelope = WorkerExecutionEnvelopeV1::decode(read(name, "envelope.pb").as_slice())
            .expect("envelope");
        let signed = envelope.signed_command.expect("signed command");
        let VerifiedExecutionCommandKind::ToolkitExecuteRead(verified) =
            parse_and_verify_execution_command(
                &signed.encode_to_vec(),
                Some(&TestOnlyConformanceHmacAuthenticator),
            )
            .unwrap_or_else(|error| panic!("{name} does not verify: {error:?}"))
        else {
            panic!("{name} is not a direct-execution command");
        };
        let manifest = ExecutionInputBundleV1::decode(read(name, "input-bundle.pb").as_slice())
            .expect("manifest");
        let entry = manifest.entries.first().expect("settings entry").clone();
        let bundle = verified.command().input_bundle_ref.clone().expect("bundle");

        let terminal = if cancelled {
            ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::Cancelled)
        } else {
            match select_rules(&verified) {
                Err(failure) => ToolkitExecuteReadTerminalOutput::Failure(failure),
                Ok(rules) => {
                    let settings = read(name, "settings.json");
                    let content = entry.content.as_ref().expect("content");
                    assert_eq!(
                        content.digest.as_ref().map(|d| d.value.as_slice()),
                        Some(digest::digest(&digest::SHA256, &settings).as_ref()),
                        "{name}: settings.json is the bundle's content"
                    );
                    match rules.evaluate(&settings) {
                        Ok(issues) => ToolkitExecuteReadTerminalOutput::ConfigurationValidation(
                            Box::new(bind_result(&verified, &bundle, &entry, &issues).unwrap()),
                        ),
                        Err(refusal) => {
                            ToolkitExecuteReadTerminalOutput::Failure(refusal.failure())
                        }
                    }
                }
            }
        };
        build_toolkit_execute_read_terminal_output_frame(
            &verified,
            &envelope.fence.expect("fence"),
            terminal,
            1,
            1_700_000_000_000,
            0,
        )
        .expect("frame")
        .encode_to_vec()
    }

    #[test]
    fn valid_invalid_and_unsupported_match_the_python_golden_bytes() {
        for name in ["valid", "invalid", "unsupported"] {
            assert_eq!(
                produce(name, false),
                read(name, "expected-output.pb"),
                "{name}"
            );
        }
    }

    #[test]
    fn cancellation_matches_the_python_golden_bytes() {
        for name in ["valid", "invalid", "unsupported"] {
            assert_eq!(
                produce(name, true),
                read(name, "expected-cancelled-output.pb"),
                "{name}"
            );
        }
    }
}
