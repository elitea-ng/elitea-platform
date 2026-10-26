//! Worker-minted fan-out identities (contract
//! `libs/proto/contracts/fanout-interrupt-decisions-v1.md` §3).
//!
//! - [`interrupt_key`]: the opaque card key Main stores and never parses (§3.1).
//! - [`member_call_id`]: the member tier `call_id` (§3.3).
//!
//! Both are pure functions of frozen identities, so a worker restart or a
//! continuation re-derives the same values. Vectors:
//! `libs/jsonschema/runtime/v1/fixtures/fanout-interrupt-key-vectors-v1.json`.

use std::fmt::Write as _;

use ring::digest;

const INTERRUPT_KEY_DOMAIN: &[u8] = b"elitea.graph.fanout-interrupt.v1\0";
const MEMBER_CALL_DOMAIN: &[u8] = b"elitea.graph.fanout-member-call.v1\0";

/// Every input of one `interrupt_key` (contract §3.1).
#[derive(Clone, Copy, Debug)]
pub struct InterruptKeyInput<'a> {
    /// The conversation's root graph thread.
    pub root_thread: &'a str,
    /// The fan-out node; `""` for a coordinator card.
    pub fanout_node_id: &'a str,
    /// The fan-out node's super-step; `0` for a coordinator card.
    pub step: u64,
    /// Lowercase hex of the compiled config digest; `""` for a coordinator.
    pub config_digest: &'a str,
    /// 1-based member ordinal; `0` for a coordinator card.
    pub ordinal: u64,
    /// The frozen child thread (the root thread for a coordinator card).
    pub child_thread: &'a str,
    /// The child checkpoint that holds the pause.
    pub child_paused_checkpoint_id: &'a str,
    pub interrupt_id: &'a str,
    /// `""` when the interrupt has no tool call.
    pub tool_call_id: &'a str,
}

/// `lowercase_hex(SHA-256(domain || LP… || U64…))`, exactly as the contract
/// spells the preimage.
#[must_use]
pub fn interrupt_key(input: &InterruptKeyInput<'_>) -> String {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(INTERRUPT_KEY_DOMAIN);
    length_prefixed(&mut context, input.root_thread);
    length_prefixed(&mut context, input.fanout_node_id);
    context.update(&input.step.to_be_bytes());
    length_prefixed(&mut context, input.config_digest);
    context.update(&input.ordinal.to_be_bytes());
    length_prefixed(&mut context, input.child_thread);
    length_prefixed(&mut context, input.child_paused_checkpoint_id);
    length_prefixed(&mut context, input.interrupt_id);
    length_prefixed(&mut context, input.tool_call_id);
    lowercase_hex(context.finish().as_ref())
}

/// `"fo1_" + lowercase_hex(SHA-256(domain || LP(child_thread)))`.
#[must_use]
pub fn member_call_id(child_thread: &str) -> String {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(MEMBER_CALL_DOMAIN);
    length_prefixed(&mut context, child_thread);
    format!("fo1_{}", lowercase_hex(context.finish().as_ref()))
}

/// Lowercase hex, as the contract renders every digest.
#[must_use]
pub fn lowercase_hex(bytes: &[u8]) -> String {
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut text, byte| {
            let _ = write!(text, "{byte:02x}");
            text
        })
}

fn length_prefixed(context: &mut digest::Context, value: &str) {
    // Every input is bounded far below 4 GiB by its producer (ids ≤ 512 B).
    let length = u32::try_from(value.len()).unwrap_or(u32::MAX);
    context.update(&length.to_be_bytes());
    context.update(value.as_bytes());
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

    const VECTORS: &str = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../jsonschema/runtime/v1/fixtures/fanout-interrupt-key-vectors-v1.json"
    );

    fn vectors() -> Value {
        serde_json::from_str(&std::fs::read_to_string(VECTORS).expect("read the shared vectors"))
            .expect("the shared vectors are JSON")
    }

    fn text<'a>(value: &'a Value, field: &str) -> &'a str {
        value[field].as_str().expect("a string vector field")
    }

    #[test]
    fn every_shared_interrupt_key_vector_matches() {
        let vectors = vectors();
        let cases = vectors["interrupt_keys"].as_array().expect("key vectors");
        assert_eq!(cases.len(), 3, "parallel, map ordinal 64 and coordinator");
        for case in cases {
            let inputs = &case["inputs"];
            let key = interrupt_key(&InterruptKeyInput {
                root_thread: text(inputs, "root_thread"),
                fanout_node_id: text(inputs, "fanout_node_id"),
                step: inputs["step"].as_u64().expect("step"),
                config_digest: text(inputs, "config_digest"),
                ordinal: inputs["ordinal"].as_u64().expect("ordinal"),
                child_thread: text(inputs, "child_thread"),
                child_paused_checkpoint_id: text(inputs, "child_paused_checkpoint_id"),
                interrupt_id: text(inputs, "interrupt_id"),
                tool_call_id: text(inputs, "tool_call_id"),
            });
            assert_eq!(key, text(case, "interrupt_key"), "{}", text(case, "name"));
        }
    }

    #[test]
    fn every_shared_member_call_id_vector_matches() {
        let vectors = vectors();
        let cases = vectors["member_call_ids"]
            .as_array()
            .expect("call id vectors");
        assert_ne!(cases.as_slice(), &[] as &[serde_json::Value]);
        for case in cases {
            assert_eq!(
                member_call_id(text(case, "child_thread")),
                text(case, "call_id")
            );
        }
    }

    #[test]
    fn a_later_pause_of_the_same_child_is_a_new_key() {
        let base = InterruptKeyInput {
            root_thread: "root",
            fanout_node_id: "gather",
            step: 3,
            config_digest: "00",
            ordinal: 1,
            child_thread: "child",
            child_paused_checkpoint_id: "cp-1",
            interrupt_id: "card",
            tool_call_id: "",
        };
        let later = InterruptKeyInput {
            child_paused_checkpoint_id: "cp-2",
            ..base
        };
        assert_ne!(interrupt_key(&base), interrupt_key(&later));
        assert_eq!(interrupt_key(&base), interrupt_key(&base));
        assert_eq!(interrupt_key(&base).len(), 64);
    }
}
