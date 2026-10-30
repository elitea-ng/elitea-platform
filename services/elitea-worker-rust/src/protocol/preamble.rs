//! The command's content controls (ADR-0043 decisions 3 and 4, #1500).
//!
//! Main puts `WorkerCommandV1.content_controls` (field 71) on a signed agent
//! command once an administrator saved the platform configuration section
//! `ai_disclosure` or `platform_preamble`, or the approval policy's
//! `untrusted_read_then_act_tiers`: the project's disclosure text and
//! opt-outs, the administrator's preamble with its revision, and the "read
//! untrusted, then act" switch for the root agent's tier.
//!
//! Absent is the worker's defaults: the default marker on every listed
//! outbound tool, the built-in preamble only, and the rule on when the
//! approval policy (field 68) names the high or critical tier, off without
//! it. A preamble longer than 8 KiB, or more than 64 opt-out types, fails
//! the command closed at verification.

use std::sync::Arc;

use elitea_agent_runtime::approval_policy::{ApprovalPolicy, RiskTier};
use elitea_agent_runtime::disclosure::DisclosurePolicy;
use elitea_agent_runtime::preamble::{ContentControls, MAX_PLATFORM_PREAMBLE_BYTES};
use elitea_agent_runtime::untrusted::UntrustedReads;

use super::ProtocolError;
use super::elitea::runtime::v1::WorkerCommandV1;

/// The most opt-out toolkit types a command carries.
const MAX_OPT_OUT_TOOLKIT_TYPES: usize = 64;

/// The longest opt-out toolkit type, in bytes.
const MAX_TOOLKIT_TYPE_BYTES: usize = 128;

/// The tiers whose runs have the "read untrusted, then act" rule when Main
/// sent no content controls: the ADR default, where `external_send`,
/// `delete` and `payment` already ask.
const DEFAULT_RULE_TIERS: [RiskTier; 2] = [RiskTier::High, RiskTier::Critical];

/// Check the command's content controls.
///
/// # Errors
///
/// [`ProtocolError::InvalidInput`] for a preamble over 8 KiB, a NUL in a
/// text, or too many or too long opt-out toolkit types.
pub fn validate_command(command: &WorkerCommandV1) -> Result<(), ProtocolError> {
    let Some(controls) = command.content_controls.as_ref() else {
        return Ok(());
    };
    if controls.platform_preamble.len() > MAX_PLATFORM_PREAMBLE_BYTES
        || controls.platform_preamble.contains('\0')
        || controls.disclosure_text.contains('\0')
    {
        return Err(ProtocolError::InvalidInput(
            "the worker content controls carry an invalid text",
        ));
    }
    if controls.disclosure_opt_out_toolkit_types.len() > MAX_OPT_OUT_TOOLKIT_TYPES
        || controls
            .disclosure_opt_out_toolkit_types
            .iter()
            .any(|toolkit| toolkit.is_empty() || toolkit.len() > MAX_TOOLKIT_TYPE_BYTES)
    {
        return Err(ProtocolError::InvalidInput(
            "the worker content controls carry invalid opt-out toolkit types",
        ));
    }
    Ok(())
}

/// The content controls of one execution of the command: a fresh record of
/// untrusted reads, the disclosure marker naming the execution.
#[must_use]
pub fn controls_from_command(
    command: &WorkerCommandV1,
    approval: &ApprovalPolicy,
) -> Arc<ContentControls> {
    let Some(controls) = command.content_controls.as_ref() else {
        let rule = approval
            .tier()
            .is_some_and(|tier| DEFAULT_RULE_TIERS.contains(&tier));
        return Arc::new(ContentControls::new(
            "",
            0,
            DisclosurePolicy::default().with_run_id(&command.execution_id),
            UntrustedReads::new(rule),
        ));
    };
    Arc::new(ContentControls::new(
        &controls.platform_preamble,
        controls.platform_preamble_revision,
        DisclosurePolicy::new(
            &controls.disclosure_text,
            &controls.disclosure_opt_out_toolkit_types,
        )
        .with_run_id(&command.execution_id),
        UntrustedReads::new(controls.untrusted_read_then_act),
    ))
}

#[cfg(test)]
mod tests {
    use elitea_agent_runtime::preamble::BUILTIN_PREAMBLE;
    use elitea_agent_runtime::tool_category::ToolCategory;

    use super::super::elitea::runtime::v1::WorkerContentControlsV1;
    use super::*;

    fn command(controls: Option<WorkerContentControlsV1>) -> WorkerCommandV1 {
        WorkerCommandV1 {
            execution_id: "exec-7".to_owned(),
            content_controls: controls,
            ..WorkerCommandV1::default()
        }
    }

    #[test]
    fn absent_is_the_worker_default_and_the_rule_follows_the_tier() {
        let none = controls_from_command(&command(None), &ApprovalPolicy::none());
        assert_eq!(&**none.preamble(), BUILTIN_PREAMBLE);
        assert_eq!(none.disclosure().text(), "Generated with Elitea AI");
        assert_eq!(none.disclosure().run_id(), Some("exec-7"));
        assert!(!none.untrusted().act_rule());
        for (tier, rule) in [
            (RiskTier::Low, false),
            (RiskTier::Medium, false),
            (RiskTier::High, true),
            (RiskTier::Critical, true),
        ] {
            let controls = controls_from_command(
                &command(None),
                &ApprovalPolicy::new(tier, [ToolCategory::Delete]),
            );
            assert_eq!(controls.untrusted().act_rule(), rule, "{tier}");
        }
    }

    #[test]
    fn present_controls_are_taken_as_given() {
        let controls = controls_from_command(
            &command(Some(WorkerContentControlsV1 {
                disclosure_text: "Drafted by AI".to_owned(),
                disclosure_opt_out_toolkit_types: vec!["slack".to_owned()],
                platform_preamble: "You work for ACME.".to_owned(),
                platform_preamble_revision: 3,
                untrusted_read_then_act: true,
            })),
            &ApprovalPolicy::none(),
        );
        assert_eq!(
            &**controls.preamble(),
            format!("{BUILTIN_PREAMBLE}\n\nYou work for ACME.")
        );
        assert_eq!(controls.preamble_revision(), 3);
        assert_eq!(controls.disclosure().text(), "Drafted by AI");
        assert!(controls.disclosure().opted_out("Slack"));
        assert!(controls.untrusted().act_rule());
        // An explicit off wins over the tier default.
        let off = controls_from_command(
            &command(Some(WorkerContentControlsV1::default())),
            &ApprovalPolicy::new(RiskTier::Critical, []),
        );
        assert!(!off.untrusted().act_rule());
        assert_eq!(off.disclosure().text(), "Generated with Elitea AI");
    }

    #[test]
    fn oversized_or_invalid_controls_fail_closed() {
        assert!(validate_command(&command(None)).is_ok());
        for controls in [
            WorkerContentControlsV1 {
                platform_preamble: "x".repeat(MAX_PLATFORM_PREAMBLE_BYTES + 1),
                ..WorkerContentControlsV1::default()
            },
            WorkerContentControlsV1 {
                disclosure_text: "a\0b".to_owned(),
                ..WorkerContentControlsV1::default()
            },
            WorkerContentControlsV1 {
                disclosure_opt_out_toolkit_types: vec![String::new()],
                ..WorkerContentControlsV1::default()
            },
            WorkerContentControlsV1 {
                disclosure_opt_out_toolkit_types: vec!["slack".to_owned(); 65],
                ..WorkerContentControlsV1::default()
            },
        ] {
            assert!(matches!(
                validate_command(&command(Some(controls))),
                Err(ProtocolError::InvalidInput(_))
            ));
        }
    }
}
