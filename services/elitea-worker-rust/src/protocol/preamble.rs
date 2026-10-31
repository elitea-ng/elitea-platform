//! The command's content controls (ADR-0043 decisions 3 and 4, #1500).
//!
//! Main puts `WorkerCommandV1.content_controls` (field 71) on a signed agent
//! command once an administrator saved the platform configuration section
//! `ai_disclosure` or `platform_preamble`, or the approval policy's
//! `untrusted_read_then_act_tiers`: the project's disclosure text and
//! opt-outs, the revision and SHA-256 of the administrator's preamble, and
//! the "read untrusted, then act" switch for the root agent's tier.
//!
//! The preamble's text is a prompt, so it never travels on the command bus:
//! admission froze it into the run's input (`AgentExecutionInputV1`
//! field 67), whose bundle digest the signed command covers. The worker
//! takes the text from the input only when the command names the same
//! revision and digest.
//!
//! Absent is the worker's defaults: the default marker on every listed
//! outbound tool, the built-in preamble and the input's preamble (the bundle
//! digest covers it), and the rule on when the approval policy (field 68)
//! names the high or critical tier, off without it. More than 64 opt-out
//! types, or a malformed digest, fails the command closed at verification.

use std::sync::Arc;

use elitea_agent_runtime::approval_policy::{ApprovalPolicy, RiskTier};
use elitea_agent_runtime::disclosure::DisclosurePolicy;
use elitea_agent_runtime::preamble::ContentControls;
use elitea_agent_runtime::request::PlatformPreambleSnapshot;
use elitea_agent_runtime::untrusted::UntrustedReads;

use super::ProtocolError;
use super::elitea::runtime::v1::WorkerCommandV1;

/// The most opt-out toolkit types a command carries.
const MAX_OPT_OUT_TOOLKIT_TYPES: usize = 64;

/// The longest opt-out toolkit type, in bytes.
const MAX_TOOLKIT_TYPE_BYTES: usize = 128;

/// The longest disclosure text a command carries, in bytes (the worker
/// bounds the marker to 200 bytes; the command is refused well above it).
const MAX_DISCLOSURE_TEXT_BYTES: usize = 1024;

/// The tiers whose runs have the "read untrusted, then act" rule when Main
/// sent no content controls: the ADR default, where `external_send`,
/// `delete` and `payment` already ask.
const DEFAULT_RULE_TIERS: [RiskTier; 2] = [RiskTier::High, RiskTier::Critical];

/// Check the command's content controls.
///
/// # Errors
///
/// [`ProtocolError::InvalidInput`] for a NUL or an oversized disclosure
/// text, a preamble digest that is not lowercase hex SHA-256, or too many
/// or too long opt-out toolkit types.
pub fn validate_command(command: &WorkerCommandV1) -> Result<(), ProtocolError> {
    let Some(controls) = command.content_controls.as_ref() else {
        return Ok(());
    };
    let digest = &controls.platform_preamble_sha256;
    let digest_ok = digest.is_empty()
        || (digest.len() == 64
            && digest
                .bytes()
                .all(|byte| matches!(byte, b'0'..=b'9' | b'a'..=b'f')));
    if controls.disclosure_text.contains('\0')
        || controls.disclosure_text.len() > MAX_DISCLOSURE_TEXT_BYTES
        || !digest_ok
        || (digest.is_empty() != (controls.platform_preamble_revision == 0))
    {
        return Err(ProtocolError::InvalidInput(
            "the worker content controls are malformed",
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
/// untrusted reads, the disclosure marker naming the execution, and the
/// preamble of the run's input.
///
/// # Errors
///
/// [`ProtocolError::InvalidInput`] when the command names a preamble that
/// the input does not carry with the same revision and digest, or the input
/// carries one that the command does not name.
pub fn controls_from_command(
    command: &WorkerCommandV1,
    approval: &ApprovalPolicy,
    input_preamble: Option<&PlatformPreambleSnapshot>,
) -> Result<Arc<ContentControls>, ProtocolError> {
    let Some(controls) = command.content_controls.as_ref() else {
        let rule = approval
            .tier()
            .is_some_and(|tier| DEFAULT_RULE_TIERS.contains(&tier));
        let (text, revision) = input_preamble.map_or(("", 0), |preamble| {
            (preamble.text.as_str(), preamble.revision)
        });
        return Ok(Arc::new(ContentControls::new(
            text,
            revision,
            DisclosurePolicy::default().with_run_id(&command.execution_id),
            UntrustedReads::new(rule),
        )));
    };
    let text = match input_preamble {
        None if controls.platform_preamble_sha256.is_empty() => "",
        Some(preamble)
            if preamble.sha256 == controls.platform_preamble_sha256
                && preamble.revision == controls.platform_preamble_revision =>
        {
            preamble.text.as_str()
        }
        _ => {
            return Err(ProtocolError::InvalidInput(
                "the platform preamble of the input does not match the command",
            ));
        }
    };
    Ok(Arc::new(ContentControls::new(
        text,
        controls.platform_preamble_revision,
        DisclosurePolicy::new(
            &controls.disclosure_text,
            &controls.disclosure_opt_out_toolkit_types,
        )
        .with_run_id(&command.execution_id),
        UntrustedReads::new(controls.untrusted_read_then_act),
    )))
}

#[cfg(test)]
mod tests {
    use elitea_agent_runtime::preamble::BUILTIN_PREAMBLE;
    use elitea_agent_runtime::tool_category::ToolCategory;

    use super::super::elitea::runtime::v1::WorkerContentControlsV1;
    use super::*;

    const ACME: &str = "You work for ACME.";
    const ACME_SHA256: &str = "1e2d426da442f63183012018263268f8fa7277f9651858cbb9ea8bdd55034653";

    fn command(controls: Option<WorkerContentControlsV1>) -> WorkerCommandV1 {
        WorkerCommandV1 {
            execution_id: "exec-7".to_owned(),
            content_controls: controls,
            ..WorkerCommandV1::default()
        }
    }

    fn acme() -> PlatformPreambleSnapshot {
        PlatformPreambleSnapshot {
            revision: 3,
            sha256: ACME_SHA256.to_owned(),
            text: ACME.to_owned(),
        }
    }

    fn named(sha256: &str, revision: u32) -> WorkerContentControlsV1 {
        WorkerContentControlsV1 {
            disclosure_text: "Drafted by AI".to_owned(),
            disclosure_opt_out_toolkit_types: vec!["slack".to_owned()],
            platform_preamble_sha256: sha256.to_owned(),
            platform_preamble_revision: revision,
            untrusted_read_then_act: true,
        }
    }

    #[test]
    fn absent_is_the_worker_default_and_the_rule_follows_the_tier() {
        let none =
            controls_from_command(&command(None), &ApprovalPolicy::none(), None).expect("controls");
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
                None,
            )
            .expect("controls");
            assert_eq!(controls.untrusted().act_rule(), rule, "{tier}");
        }
        // The input's preamble applies also without field 71: the bundle
        // digest covers it.
        let input = controls_from_command(&command(None), &ApprovalPolicy::none(), Some(&acme()))
            .expect("controls");
        assert_eq!(&**input.preamble(), format!("{BUILTIN_PREAMBLE}\n\n{ACME}"));
    }

    #[test]
    fn the_preamble_comes_from_the_input_when_the_command_names_it() {
        let controls = controls_from_command(
            &command(Some(named(ACME_SHA256, 3))),
            &ApprovalPolicy::none(),
            Some(&acme()),
        )
        .expect("controls");
        assert_eq!(
            &**controls.preamble(),
            format!("{BUILTIN_PREAMBLE}\n\n{ACME}")
        );
        assert_eq!(controls.preamble_revision(), 3);
        assert_eq!(controls.disclosure().text(), "Drafted by AI");
        assert!(controls.disclosure().opted_out("Slack"));
        assert!(controls.untrusted().act_rule());
        // An explicit off wins over the tier default.
        let off = controls_from_command(
            &command(Some(WorkerContentControlsV1::default())),
            &ApprovalPolicy::new(RiskTier::Critical, []),
            None,
        )
        .expect("controls");
        assert!(!off.untrusted().act_rule());
        assert_eq!(&**off.preamble(), BUILTIN_PREAMBLE);
    }

    #[test]
    fn a_preamble_the_command_does_not_name_fails_closed() {
        let other = "0".repeat(64);
        for (controls, input) in [
            (named(&other, 3), Some(acme())),
            (named(ACME_SHA256, 4), Some(acme())),
            (named(ACME_SHA256, 3), None),
            (named("", 0), Some(acme())),
        ] {
            assert!(matches!(
                controls_from_command(
                    &command(Some(controls)),
                    &ApprovalPolicy::none(),
                    input.as_ref()
                ),
                Err(ProtocolError::InvalidInput(_))
            ));
        }
    }

    #[test]
    fn oversized_or_invalid_controls_fail_closed() {
        assert!(validate_command(&command(None)).is_ok());
        assert!(validate_command(&command(Some(named(ACME_SHA256, 3)))).is_ok());
        for controls in [
            WorkerContentControlsV1 {
                disclosure_text: "a\0b".to_owned(),
                ..WorkerContentControlsV1::default()
            },
            named(&"A".repeat(64), 3),
            named("abc", 3),
            named(ACME_SHA256, 0),
            named("", 2),
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
