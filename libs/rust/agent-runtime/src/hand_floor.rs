//! The turn-taking `hand_floor` tool's model-facing contract (ADR-0050
//! decision 11, ADR-0061 decision 3d): its name, description, parameter
//! schema and the argument check, shared by the cloud worker
//! (`elitea-worker-rust` `agents/hand_floor_tool.rs`) and the desktop host
//! (`d0/hand_floor.rs`), so the model sees one tool in both places.
//!
//! Main decides everything that authorizes a hand-off (who is in the
//! conversation, the order, the loop guards); a host only checks the
//! arguments against the peers Main named for the turn, so a malformed call
//! never leaves it. Every failure is a tool RESULT the model reads.

use std::collections::HashSet;
use std::fmt::Write as _;
use std::hash::BuildHasher;

use serde_json::{Value, json};

/// The toolset the tool sits in.
pub const TOOLSET_NAME: &str = "turn_taking";
/// The tool's name.
pub const HAND_FLOOR: &str = "hand_floor";
/// The most agents one hand-off names (Main's `turntaking.MaxHandTargets`).
pub const MAX_HAND_OFF_PEERS: usize = 4;
/// The longest hand-off message, in bytes (Main's
/// `turntaking.MaxHandMessageBytes`).
pub const MAX_MESSAGE_BYTES: usize = 4_000;

/// One agent the floor may be handed to.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Peer {
    pub participant_id: i64,
    pub name: String,
    pub role: String,
}

/// A checked call.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HandFloorArgs {
    pub participant_ids: Vec<i64>,
    pub message: String,
}

/// The tool's description for `peers`.
#[must_use]
pub fn describe(peers: &[Peer]) -> String {
    let mut description = String::from(
        "Hand the floor to other agents in this conversation. They answer after your turn ends, in order. Use it only when another agent is better placed to do part of the work; do not hand the floor back and forth. A loop guard stops the exchange after a few hand-offs.\n\nThe agents you can hand the floor to:",
    );
    for peer in peers {
        let _ = write!(
            description,
            "\n- {} ({}) — participant {}",
            peer.name, peer.role, peer.participant_id
        );
    }
    description
}

/// The tool's parameter schema.
#[must_use]
pub fn parameters_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "participant_ids": {
                "type": "array",
                "items": {"type": "integer"},
                "minItems": 1,
                "maxItems": MAX_HAND_OFF_PEERS,
                "uniqueItems": true
            },
            "message": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_MESSAGE_BYTES,
                "description": "What you want them to do."
            }
        },
        "required": ["participant_ids", "message"],
        "additionalProperties": false
    })
}

/// A tool result the model reads as a failure.
#[must_use]
pub fn error(code: &str, message: &str) -> Value {
    json!({"status": "error", "code": code, "message": message})
}

/// The arguments, checked against this turn's peers.
///
/// # Errors
///
/// The tool result (`invalid_argument`) the model reads.
pub fn validate<S: BuildHasher>(
    arguments: &Value,
    peers: &HashSet<i64, S>,
) -> Result<HandFloorArgs, Value> {
    let invalid = |message: &str| error("invalid_argument", message);
    let Some(object) = arguments.as_object() else {
        return Err(invalid("The arguments must be an object."));
    };
    if object
        .keys()
        .any(|key| key != "participant_ids" && key != "message")
    {
        return Err(invalid("Only participant_ids and message are accepted."));
    }
    let Some(ids) = object.get("participant_ids").and_then(Value::as_array) else {
        return Err(invalid(
            "participant_ids must be a list of participant ids.",
        ));
    };
    if ids.is_empty() || ids.len() > MAX_HAND_OFF_PEERS {
        return Err(invalid("participant_ids must name one to four agents."));
    }
    let mut seen = HashSet::with_capacity(ids.len());
    let mut participant_ids = Vec::with_capacity(ids.len());
    for id in ids {
        let Some(id) = id.as_i64() else {
            return Err(invalid("participant_ids must be integers."));
        };
        if !peers.contains(&id) {
            return Err(invalid(
                "Every participant id must be one of the agents listed in this tool's description.",
            ));
        }
        if !seen.insert(id) {
            return Err(invalid("participant_ids must not repeat an agent."));
        }
        participant_ids.push(id);
    }
    let message = object
        .get("message")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim();
    if message.is_empty() || message.len() > MAX_MESSAGE_BYTES {
        return Err(invalid(
            "message must say what you want them to do, in at most 4000 bytes.",
        ));
    }
    Ok(HandFloorArgs {
        participant_ids,
        message: message.to_owned(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn peers() -> HashSet<i64> {
        [7, 9].into_iter().collect()
    }

    #[test]
    fn the_description_names_every_peer() {
        let text = describe(&[
            Peer {
                participant_id: 7,
                name: "Ada".into(),
                role: "reviewer".into(),
            },
            Peer {
                participant_id: 9,
                name: "Lin".into(),
                role: String::new(),
            },
        ]);
        assert!(text.contains("\n- Ada (reviewer) — participant 7"));
        assert!(text.contains("\n- Lin () — participant 9"));
    }

    #[test]
    fn a_call_is_checked_against_the_peers() {
        let ok = validate(
            &json!({"participant_ids": [9], "message": " go "}),
            &peers(),
        );
        assert_eq!(
            ok,
            Ok(HandFloorArgs {
                participant_ids: vec![9],
                message: "go".into()
            })
        );
        for bad in [
            json!([]),
            json!({"participant_ids": [], "message": "x"}),
            json!({"participant_ids": [1, 2, 3, 4, 5], "message": "x"}),
            json!({"participant_ids": [8], "message": "x"}),
            json!({"participant_ids": [7, 7], "message": "x"}),
            json!({"participant_ids": [7], "message": "  "}),
            json!({"participant_ids": [7], "message": "x".repeat(MAX_MESSAGE_BYTES + 1)}),
            json!({"participant_ids": [7], "message": "x", "extra": 1}),
        ] {
            let refused = validate(&bad, &peers()).expect_err("refused");
            assert_eq!(refused["code"], "invalid_argument", "{bad}");
        }
    }
}
