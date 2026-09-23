//! Strict direct-toolkit input protocol decoding.

use prost::Message;

use super::ProtocolError;
use super::elitea::runtime::v1::ToolkitExecuteReadInputV1;
use super::wire::{Schema, scan_message};

pub(crate) const TOOLKIT_EXECUTE_READ_INPUT_SCHEMA_REVISION: &str =
    "elitea.runtime.toolkit-execute-read-input.v1";
const MAX_TOOLKIT_EXECUTE_READ_INPUT_BYTES: usize = 1024 * 1024;

/// Decode one canonical, bounded direct-toolkit request.
///
/// The structural scan rejects unknown and duplicated fields before Prost can
/// normalize them. Re-encoding then rejects alternate varints, field order,
/// explicit defaults, and every other non-canonical v1 representation.
pub(crate) fn parse_toolkit_execute_read_input(
    raw: &[u8],
) -> Result<ToolkitExecuteReadInputV1, ProtocolError> {
    if raw.is_empty() || raw.len() > MAX_TOOLKIT_EXECUTE_READ_INPUT_BYTES {
        return Err(ProtocolError::ResourceExhausted(
            "the direct toolkit input exceeds its approved limit",
        ));
    }
    scan_message(raw, Schema::ToolkitExecuteReadInput)?;
    let input = ToolkitExecuteReadInputV1::decode(raw)
        .map_err(|_| ProtocolError::InvalidInput("the direct toolkit input is malformed"))?;
    if input.encode_to_vec() != raw {
        return Err(ProtocolError::InvalidInput(
            "the direct toolkit input is not canonical protocol v1",
        ));
    }
    if input.schema_revision != TOOLKIT_EXECUTE_READ_INPUT_SCHEMA_REVISION {
        return Err(ProtocolError::IncompatibleVersion(
            "the direct toolkit input revision is not supported",
        ));
    }
    Ok(input)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid_input() -> ToolkitExecuteReadInputV1 {
        ToolkitExecuteReadInputV1 {
            schema_revision: TOOLKIT_EXECUTE_READ_INPUT_SCHEMA_REVISION.to_owned(),
            toolkit: br#"{"id":1}"#.to_vec(),
            toolkit_type: "openapi".to_owned(),
            toolkit_name: "orders".to_owned(),
            tool_name: "get_order".to_owned(),
            arguments: br#"{"id":"42"}"#.to_vec(),
            toolkit_guardrails: b"{}".to_vec(),
        }
    }

    #[test]
    fn accepts_exact_canonical_input() {
        let input = valid_input();
        assert_eq!(
            parse_toolkit_execute_read_input(&input.encode_to_vec()).expect("valid input"),
            input
        );
    }

    #[test]
    fn rejects_unknown_fields_before_decode() {
        let mut raw = valid_input().encode_to_vec();
        raw.extend_from_slice(&[0x80, 0x01, 0x01]);
        assert!(matches!(
            parse_toolkit_execute_read_input(&raw),
            Err(ProtocolError::IncompatibleVersion(_))
        ));
    }

    #[test]
    fn rejects_duplicate_fields_before_decode() {
        let mut raw = valid_input().encode_to_vec();
        raw.extend_from_slice(&[0x1a, 0x07]);
        raw.extend_from_slice(b"openapi");
        assert!(matches!(
            parse_toolkit_execute_read_input(&raw),
            Err(ProtocolError::InvalidInput(_))
        ));
    }
}
