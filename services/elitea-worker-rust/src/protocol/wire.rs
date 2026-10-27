use std::collections::{BTreeMap, BTreeSet};

use super::ProtocolError;

#[cfg(test)]
mod merge_contract_tests {
    use super::{Schema, scan_message};
    use crate::protocol::elitea::runtime::v1::{
        ExecutionOutputEventTypeV1, ExecutionOutputFrameV1, ToolkitCallToolResultV1,
        ToolkitExecuteReadResultV1, WorkerCommandTypeV1, execution_output_frame_v1,
    };
    use prost::Message;

    #[test]
    fn toolkit_enum_and_output_numbers_preserve_main_contract() {
        assert_eq!(WorkerCommandTypeV1::ToolkitCallTool as i32, 10);
        assert_eq!(WorkerCommandTypeV1::ToolkitExecuteRead as i32, 11);
        assert_eq!(ExecutionOutputEventTypeV1::ToolkitCallToolResult as i32, 7);
        assert_eq!(
            ExecutionOutputEventTypeV1::ToolkitExecuteReadResult as i32,
            8
        );
        for (payload, expected) in [
            (
                execution_output_frame_v1::Payload::ToolkitCallTool(
                    ToolkitCallToolResultV1::default(),
                ),
                vec![0xca, 0x01, 0x00],
            ),
            (
                execution_output_frame_v1::Payload::ToolkitExecuteRead(
                    ToolkitExecuteReadResultV1::default(),
                ),
                vec![0x8a, 0x02, 0x00],
            ),
        ] {
            let frame = ExecutionOutputFrameV1 {
                payload: Some(payload),
                ..Default::default()
            };
            assert_eq!(frame.encode_to_vec(), expected);
        }
    }

    #[test]
    fn toolkit_command_fields_remain_distinct() {
        // These are distinct capabilities in one language-neutral schema.
        let call_tool = scan_message(&[0xa2, 0x02, 0x00], Schema::WorkerCommand).unwrap();
        let read = scan_message(&[0x82, 0x04, 0x00], Schema::WorkerCommand).unwrap();
        assert!(call_tool.contains(36));
        assert!(!call_tool.contains(64));
        assert!(read.contains(64));
        assert!(!read.contains(36));
    }

    #[test]
    fn the_security_context_reference_is_a_length_delimited_command_field() {
        // Field 65, wire type 2: tag 0x8a 0x04.
        let scanned = scan_message(&[0x8a, 0x04, 0x00], Schema::WorkerCommand).unwrap();
        assert!(scanned.contains(65));
        // A varint 65 is the wrong wire type.
        assert!(matches!(
            scan_message(&[0x88, 0x04, 0x00], Schema::WorkerCommand),
            Err(super::ProtocolError::InvalidInput(_))
        ));
        // Field 66 is still unknown.
        assert!(matches!(
            scan_message(&[0x92, 0x04, 0x00], Schema::WorkerCommand),
            Err(super::ProtocolError::IncompatibleVersion(_))
        ));
        // The reference holds 1-3; an unknown nested field 4 is refused.
        assert!(
            scan_message(
                &[0x0a, 0x00, 0x12, 0x00, 0x1a, 0x00],
                Schema::SecurityContextReference
            )
            .is_ok()
        );
        assert!(matches!(
            scan_message(&[0x22, 0x00], Schema::SecurityContextReference),
            Err(super::ProtocolError::IncompatibleVersion(_))
        ));
    }

    #[test]
    fn the_actor_chain_is_the_only_repeated_context_field() {
        let chain = scan_message(&[0x7a, 0x00, 0x7a, 0x00], Schema::SecurityContext).unwrap();
        assert_eq!(chain.repeated_field(15).len(), 2);
        assert!(matches!(
            scan_message(&[0x0a, 0x00, 0x0a, 0x00], Schema::SecurityContext),
            Err(super::ProtocolError::InvalidInput(_))
        ));
        // Field 18 is reserved, so unknown to this revision.
        assert!(matches!(
            scan_message(&[0x92, 0x01, 0x00], Schema::SecurityContext),
            Err(super::ProtocolError::IncompatibleVersion(_))
        ));
    }

    #[test]
    fn mixed_toolkit_commands_are_rejected_before_decode() {
        for raw in [
            [0xa2, 0x02, 0x00, 0x82, 0x04, 0x00],
            [0x82, 0x04, 0x00, 0xa2, 0x02, 0x00],
        ] {
            assert!(matches!(
                scan_message(&raw, Schema::WorkerCommand),
                Err(super::ProtocolError::InvalidInput(_))
            ));
        }
    }
}

#[derive(Clone, Copy)]
pub(crate) enum Schema {
    SignedCommandEnvelope,
    #[cfg(feature = "sandbox-supervisor")]
    SandboxGrant,
    #[cfg(feature = "sandbox-supervisor")]
    CompiledSnapshotGrant,
    #[cfg(feature = "sandbox-supervisor")]
    OriginalCodeVisitAccess,
    #[cfg(feature = "sandbox-supervisor")]
    OriginalCodeVisitReference,
    Digest,
    WorkerCommand,
    InputBundleReference,
    AgentExecutionCommand,
    ToolkitExecuteReadCommand,
    ToolkitCallToolCommand,
    ToolkitAvailableToolsCommand,
    ConfigurationValidationCommand,
    IndexIngestCommand,
    IndexIngestInputBinding,
    ToolkitExecuteReadInput,
    NodeEvent,
    /// `WorkerCommandV1.security_context_ref` (field 65, ADR-0039 #1115).
    SecurityContextReference,
    /// `ExecutionSecurityContextV1`, the governed context Main serves.
    SecurityContext,
    CallerContext,
    ActorLink,
}

#[derive(Clone, Copy)]
struct FieldRule {
    wire_type: u8,
    oneof: Option<u8>,
    /// A repeated field may occur more than once; every occurrence is kept.
    repeated: bool,
}

pub(crate) struct ScannedFields<'a> {
    values: BTreeMap<u32, (u8, &'a [u8])>,
    repeated: BTreeMap<u32, Vec<&'a [u8]>>,
}

impl<'a> ScannedFields<'a> {
    pub(crate) fn length_field(
        &self,
        field_number: u32,
        description: &'static str,
    ) -> Result<&'a [u8], ProtocolError> {
        let Some((wire_type, payload)) = self.values.get(&field_number) else {
            return Err(ProtocolError::InvalidInput(description));
        };
        if *wire_type != 2 {
            return Err(ProtocolError::InvalidInput(
                "a required protobuf field has the wrong wire type",
            ));
        }
        Ok(payload)
    }

    pub(crate) fn contains(&self, field_number: u32) -> bool {
        self.values.contains_key(&field_number) || self.repeated.contains_key(&field_number)
    }

    /// Every occurrence of a repeated length-delimited field, in wire order.
    pub(crate) fn repeated_field(&self, field_number: u32) -> &[&'a [u8]] {
        self.repeated.get(&field_number).map_or(&[], Vec::as_slice)
    }
}

pub(crate) fn scan_message(raw: &[u8], schema: Schema) -> Result<ScannedFields<'_>, ProtocolError> {
    let mut position = 0;
    let mut values = BTreeMap::new();
    let mut repeated: BTreeMap<u32, Vec<&[u8]>> = BTreeMap::new();
    let mut seen_oneofs = BTreeSet::new();
    while position < raw.len() {
        let tag = read_varint(raw, &mut position)?;
        let field_number = u32::try_from(tag >> 3).map_err(|_| malformed_wire())?;
        let wire_type = u8::try_from(tag & 7).map_err(|_| malformed_wire())?;
        if field_number == 0 || matches!(wire_type, 3 | 4 | 6 | 7) {
            return Err(malformed_wire());
        }
        let payload = read_payload(raw, &mut position, wire_type)?;
        let Some(rule) = field_rule(schema, field_number) else {
            return Err(ProtocolError::IncompatibleVersion(
                "the protobuf message contains an unknown v1 field",
            ));
        };
        if wire_type != rule.wire_type {
            return Err(ProtocolError::InvalidInput(
                "a protobuf field has the wrong wire type",
            ));
        }
        if rule.repeated {
            repeated.entry(field_number).or_default().push(payload);
            continue;
        }
        if values.contains_key(&field_number) {
            return Err(ProtocolError::InvalidInput(
                "a singular protobuf field is duplicated",
            ));
        }
        if let Some(oneof) = rule.oneof
            && !seen_oneofs.insert(oneof)
        {
            return Err(ProtocolError::InvalidInput(
                "a protobuf oneof field is duplicated",
            ));
        }
        values.insert(field_number, (wire_type, payload));
    }
    Ok(ScannedFields { values, repeated })
}

fn read_payload<'a>(
    raw: &'a [u8],
    position: &mut usize,
    wire_type: u8,
) -> Result<&'a [u8], ProtocolError> {
    match wire_type {
        0 => {
            let start = *position;
            read_varint(raw, position)?;
            raw.get(start..*position).ok_or_else(truncated_wire)
        }
        1 => take(raw, position, 8),
        2 => {
            let length = read_varint(raw, position)?;
            let length = usize::try_from(length).map_err(|_| truncated_wire())?;
            take(raw, position, length)
        }
        5 => take(raw, position, 4),
        _ => Err(malformed_wire()),
    }
}

fn take<'a>(raw: &'a [u8], position: &mut usize, length: usize) -> Result<&'a [u8], ProtocolError> {
    let end = position.checked_add(length).ok_or_else(truncated_wire)?;
    let payload = raw.get(*position..end).ok_or_else(truncated_wire)?;
    *position = end;
    Ok(payload)
}

fn read_varint(raw: &[u8], position: &mut usize) -> Result<u64, ProtocolError> {
    let mut value = 0_u64;
    for index in 0..10 {
        let octet = *raw.get(*position).ok_or_else(truncated_varint)?;
        *position += 1;
        if index == 9 && octet > 1 {
            return Err(ProtocolError::InvalidInput(
                "the protobuf varint exceeds its encoded limit",
            ));
        }
        value |= u64::from(octet & 0x7f) << (index * 7);
        if octet & 0x80 == 0 {
            return Ok(value);
        }
    }
    Err(ProtocolError::InvalidInput(
        "the protobuf varint exceeds its encoded limit",
    ))
}

const fn field_rule(schema: Schema, field: u32) -> Option<FieldRule> {
    match schema {
        #[cfg(feature = "sandbox-supervisor")]
        Schema::SandboxGrant => match field {
            1 | 3 | 9..=11 | 32 => Some(varint()),
            2 | 4..=8 | 33 => Some(length()),
            _ => None,
        },
        #[cfg(feature = "sandbox-supervisor")]
        Schema::CompiledSnapshotGrant => match field {
            1 | 3 | 9..=11 | 34 | 41 => Some(varint()),
            2 | 4..=8 | 35..=40 | 42 => Some(length()),
            _ => None,
        },
        #[cfg(feature = "sandbox-supervisor")]
        Schema::OriginalCodeVisitAccess => match field {
            1 | 2 | 5 => Some(length()),
            3 | 4 => Some(varint()),
            _ => None,
        },
        #[cfg(feature = "sandbox-supervisor")]
        Schema::OriginalCodeVisitReference => match field {
            1 | 3 => Some(length()),
            2 => Some(varint()),
            _ => None,
        },
        Schema::SignedCommandEnvelope => match field {
            1 | 3 | 4 | 5 | 6 => Some(length()),
            2 => Some(varint()),
            _ => None,
        },
        Schema::Digest => match field {
            1 => Some(varint()),
            2 => Some(length()),
            _ => None,
        },
        Schema::WorkerCommand => match field {
            // 65 is the governed security context reference (#1115).
            1..=3 | 5 | 8..=14 | 16..=20 | 23..=25 | 65 => Some(length()),
            4 | 6..=7 | 21..=22 => Some(varint()),
            // Preserve the complete language-neutral capability oneof.
            32..=36 | 64 => Some(FieldRule {
                wire_type: 2,
                oneof: Some(1),
                repeated: false,
            }),
            _ => None,
        },
        Schema::InputBundleReference => match field {
            1..=3 | 5 => Some(length()),
            4 => Some(varint()),
            _ => None,
        },
        Schema::AgentExecutionCommand => match field {
            1..=4 => Some(length()),
            _ => None,
        },
        Schema::ToolkitExecuteReadCommand => match field {
            1 => Some(length()),
            _ => None,
        },
        Schema::ToolkitCallToolCommand => match field {
            1..=6 => Some(length()),
            _ => None,
        },
        Schema::ToolkitAvailableToolsCommand => match field {
            1..=2 | 16 => Some(length()),
            _ => None,
        },
        Schema::ConfigurationValidationCommand => match field {
            1..=8 => Some(length()),
            _ => None,
        },
        // IndexIngestCommandV1: entry ids 1-5, client correlation 6-8, the
        // embedding binding message 9, initiator 16, the expected model 17
        // and index id 19 are length-delimited; the expected dimension 18 is
        // a varint. 10-15 are reserved.
        Schema::IndexIngestCommand => match field {
            1..=9 | 16 | 17 | 19 => Some(length()),
            18 => Some(varint()),
            _ => None,
        },
        Schema::IndexIngestInputBinding => match field {
            1..=3 => Some(length()),
            _ => None,
        },
        Schema::ToolkitExecuteReadInput => match field {
            1..=7 => Some(length()),
            _ => None,
        },
        Schema::NodeEvent => match field {
            1..=13 => Some(length()),
            _ => None,
        },
        Schema::SecurityContextReference
        | Schema::SecurityContext
        | Schema::CallerContext
        | Schema::ActorLink => security_context_rule(schema, field),
    }
}

/// The governed security context's messages (ADR-0039, #1115).
const fn security_context_rule(schema: Schema, field: u32) -> Option<FieldRule> {
    match (schema, field) {
        // content_entry_id 1, digest 2 (DigestV1), schema_revision 3.
        // ExecutionSecurityContextV1: the epochs, the tenant and the issue
        // time are varints; the actor chain 15 is the only repeated field.
        (Schema::SecurityContextReference, 1..=3)
        | (Schema::CallerContext | Schema::ActorLink, 1..=2)
        | (Schema::SecurityContext, 1..=4 | 6 | 7 | 9 | 11 | 13 | 14 | 17) => Some(length()),
        (Schema::SecurityContext, 5 | 8 | 10 | 12 | 16) => Some(varint()),
        (Schema::SecurityContext, 15) => Some(FieldRule {
            wire_type: 2,
            oneof: None,
            repeated: true,
        }),
        _ => None,
    }
}

const fn length() -> FieldRule {
    FieldRule {
        wire_type: 2,
        oneof: None,
        repeated: false,
    }
}

const fn varint() -> FieldRule {
    FieldRule {
        wire_type: 0,
        oneof: None,
        repeated: false,
    }
}

const fn malformed_wire() -> ProtocolError {
    ProtocolError::InvalidInput("the protobuf wire message is malformed")
}

const fn truncated_wire() -> ProtocolError {
    ProtocolError::InvalidInput("the protobuf wire message is truncated")
}

const fn truncated_varint() -> ProtocolError {
    ProtocolError::InvalidInput("the protobuf varint is truncated")
}
