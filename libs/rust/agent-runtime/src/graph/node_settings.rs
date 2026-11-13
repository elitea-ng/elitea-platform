//! Per-node `settings:` (5b-1): the n8n-style retry, on-error, always-output,
//! run-once and timeout settings of one pipeline node, as one strict policy
//! type shared by every node family.
//!
//! The contract is `libs/proto/contracts/pipeline-node-settings-v1.json`; Main
//! and the Web editor read the same file. This module owns the parse, the
//! bounds, the per-family admission table, the `node_errors` entry, the
//! neutral values of `always_output_data`, the run-once set and the digest.
//! It performs no I/O and names no data: refusals and entries carry fixed
//! catalog text only.

use std::collections::BTreeMap;

use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};

use super::fanout::plan::{safe_message, support_ref_for, valid_code};
use super::fanout_budget::MAX_JOIN_ERROR_ENTRY_BYTES;
use super::node_recovery::MAX_NODE_ATTEMPTS;
use super::yaml::valid_graph_id;

/// The node key that holds the block.
pub const SETTINGS_KEY: &str = "settings";
/// The state channel a continued or routed failure appends one entry to.
pub const NODE_ERRORS_KEY: &str = "node_errors";
/// The private channel that records which nodes already ran in this turn.
pub const EXECUTED_ONCE_KEY: &str = "__elitea_executed_once_v1";

/// Fewest total tries, the first one included, when `retry_on_fail` is set.
pub const MIN_NODE_TRIES: u16 = 2;
/// Most total tries, the first one included.
pub const MAX_NODE_TRIES: u16 = 5;
/// Shortest wait between two tries (n8n allows 0; unspaced retries amplify
/// an outage, FU-302).
pub const MIN_WAIT_BETWEEN_TRIES_MS: u64 = 100;
/// Longest wait between two tries: the recovery policy's own backoff bound.
pub const MAX_WAIT_BETWEEN_TRIES_MS: u64 = 300_000;
/// The wait when `retry_on_fail` is set without one.
pub const DEFAULT_WAIT_BETWEEN_TRIES_MS: u64 = 1_000;
/// Shortest per-node timeout.
pub const MIN_NODE_TIMEOUT_MS: u64 = 1_000;
/// Longest per-node timeout; below the sandbox job limit of one hour.
pub const MAX_NODE_TIMEOUT_MS: u64 = 900_000;
/// Most entries `node_errors` holds in one turn; the next failure is not
/// continued and fails the node typed, so nothing is dropped.
pub const MAX_NODE_ERRORS: usize = 64;
/// Most serialized bytes of one whole `node_errors` entry.
pub const MAX_NODE_ERROR_ENTRY_BYTES: usize = 1_024;
/// Most serialized bytes of the entry's `error` object: the fan-out bound.
pub const MAX_NODE_ERROR_OBJECT_BYTES: usize = 512;
/// Most node ids the run-once set holds: the pipeline node bound.
pub const MAX_EXECUTED_ONCE_NODES: usize = 128;

const _: () = assert!(MAX_NODE_TRIES as usize <= MAX_NODE_ATTEMPTS as usize);
const _: () = assert!(MIN_NODE_TRIES >= 2 && MIN_NODE_TRIES <= MAX_NODE_TRIES);
const _: () = assert!(MAX_NODE_ERROR_OBJECT_BYTES == MAX_JOIN_ERROR_ENTRY_BYTES);
const _: () = assert!(MAX_WAIT_BETWEEN_TRIES_MS == 300_000);
const _: () = assert!(MAX_NODE_TIMEOUT_MS < 3_600_000);

const DIGEST_DOMAIN: &[u8] = b"elitea.pipeline.node-settings.v1\0";
const DEFINITION_DOMAIN: &[u8] = b"elitea.pipeline.node-settings-definition.v1\0";

/// What a node does when its body fails.
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum OnNodeError {
    /// The failure propagates and the run stops (today's behaviour).
    #[default]
    Stop,
    /// One `node_errors` entry, then the node's normal transition.
    Continue,
    /// One `node_errors` entry, then the declared `error_transition`.
    ErrorOutput,
}

impl OnNodeError {
    /// The `handled` value a `node_errors` entry records.
    #[must_use]
    pub const fn handled(self) -> Option<&'static str> {
        match self {
            Self::Stop => None,
            Self::Continue => Some("continue"),
            Self::ErrorOutput => Some("error_output"),
        }
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawNodeSettings {
    #[serde(default)]
    retry_on_fail: bool,
    max_tries: Option<serde_yaml_ng::Number>,
    wait_between_tries_ms: Option<serde_yaml_ng::Number>,
    #[serde(default)]
    on_error: OnNodeError,
    error_transition: Option<String>,
    #[serde(default)]
    always_output_data: bool,
    #[serde(default)]
    execute_once: bool,
    timeout_ms: Option<serde_yaml_ng::Number>,
}

/// The largest integer every layer reads exactly (`Number.MAX_SAFE_INTEGER`).
const MAX_SAFE_INTEGER: i64 = (1 << 53) - 1;

/// A count field: an integer within +/-(2^53 - 1), whatever its sign, so an
/// out-of-range value is the bound's range refusal in every layer; any other
/// number (a fraction, a wider integer) is malformed.
fn count(number: Option<&serde_yaml_ng::Number>) -> Result<Option<i64>, NodeSettingsRefusal> {
    number
        .map(|number| {
            number
                .as_i64()
                .filter(|value| (-MAX_SAFE_INTEGER..=MAX_SAFE_INTEGER).contains(value))
                .ok_or(NodeSettingsRefusal::Malformed)
        })
        .transpose()
}

/// `value` within `low..=high`, as the field's type, or the range refusal.
fn bounded<T: TryFrom<i64>>(
    value: Option<i64>,
    low: i64,
    high: i64,
    refusal: NodeSettingsRefusal,
) -> Result<Option<T>, NodeSettingsRefusal> {
    value
        .map(|value| {
            (low..=high)
                .contains(&value)
                .then(|| T::try_from(value).ok())
                .flatten()
                .ok_or(refusal)
        })
        .transpose()
}

/// One node's admitted settings. Field order is the digest order.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct NodeSettings {
    pub retry_on_fail: bool,
    pub max_tries: Option<u16>,
    pub wait_between_tries_ms: Option<u64>,
    pub on_error: OnNodeError,
    pub error_transition: Option<String>,
    pub always_output_data: bool,
    pub execute_once: bool,
    pub timeout_ms: Option<u64>,
}

/// Why a `settings:` block (or the document around it) is refused. Each
/// refusal has one stable key and one fixed text, both in the contract.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum NodeSettingsRefusal {
    Malformed,
    FamilyNotYet,
    FamilyPause,
    FanoutOwned,
    WithRecovery,
    RetryNotAllowed,
    TimeoutNotAllowed,
    MaxTriesRange,
    WaitRange,
    TimeoutRange,
    RetryFieldsNeedRetry,
    ErrorTransitionRequired,
    ErrorTransitionUnexpected,
    ErrorTransitionTarget,
    AlwaysOutputReducer,
    RouterContinueNeedsDefault,
    NodeErrorsType,
    NodeErrorsNestedMapping,
}

impl NodeSettingsRefusal {
    /// Every refusal, in contract order.
    pub const ALL: [Self; 18] = [
        Self::Malformed,
        Self::FamilyNotYet,
        Self::FamilyPause,
        Self::FanoutOwned,
        Self::WithRecovery,
        Self::RetryNotAllowed,
        Self::TimeoutNotAllowed,
        Self::MaxTriesRange,
        Self::WaitRange,
        Self::TimeoutRange,
        Self::RetryFieldsNeedRetry,
        Self::ErrorTransitionRequired,
        Self::ErrorTransitionUnexpected,
        Self::ErrorTransitionTarget,
        Self::AlwaysOutputReducer,
        Self::RouterContinueNeedsDefault,
        Self::NodeErrorsType,
        Self::NodeErrorsNestedMapping,
    ];

    /// The contract key.
    #[must_use]
    pub const fn key(self) -> &'static str {
        match self {
            Self::Malformed => "malformed",
            Self::FamilyNotYet => "family_not_yet",
            Self::FamilyPause => "family_pause",
            Self::FanoutOwned => "fanout_owned",
            Self::WithRecovery => "with_recovery",
            Self::RetryNotAllowed => "retry_not_allowed",
            Self::TimeoutNotAllowed => "timeout_not_allowed",
            Self::MaxTriesRange => "max_tries_range",
            Self::WaitRange => "wait_range",
            Self::TimeoutRange => "timeout_range",
            Self::RetryFieldsNeedRetry => "retry_fields_need_retry",
            Self::ErrorTransitionRequired => "error_transition_required",
            Self::ErrorTransitionUnexpected => "error_transition_unexpected",
            Self::ErrorTransitionTarget => "error_transition_target",
            Self::AlwaysOutputReducer => "always_output_reducer",
            Self::RouterContinueNeedsDefault => "router_continue_needs_default",
            Self::NodeErrorsType => "node_errors_type",
            Self::NodeErrorsNestedMapping => "node_errors_nested_mapping",
        }
    }

    /// The readable text, identical in the Worker, Main and the Web editor.
    #[must_use]
    pub const fn text(self) -> &'static str {
        match self {
            Self::Malformed => {
                "Its settings are malformed: use a mapping of the known settings with plain values."
            }
            Self::FamilyNotYet => "Settings are not available for this node type yet.",
            Self::FamilyPause => "Settings are not available for a node that pauses the run.",
            Self::FanoutOwned => {
                "Settings are not available for a node inside a Parallel or Map yet."
            }
            Self::WithRecovery => {
                "Settings cannot be combined with recovery or failure_handler on one node."
            }
            Self::RetryNotAllowed => "Retry on fail is not available for this node type.",
            Self::TimeoutNotAllowed => "Timeout is not available for this node type.",
            Self::MaxTriesRange => "Max tries must be between 2 and 5.",
            Self::WaitRange => "Wait between tries must be between 100 and 300000 ms.",
            Self::TimeoutRange => "Timeout must be between 1000 and 900000 ms.",
            Self::RetryFieldsNeedRetry => "Max tries and wait between tries need Retry on fail.",
            Self::ErrorTransitionRequired => "On error 'error output' needs an error transition.",
            Self::ErrorTransitionUnexpected => "An error transition needs On error 'error output'.",
            Self::ErrorTransitionTarget => {
                "The error transition must name END or another node of this pipeline outside any Parallel or Map."
            }
            Self::AlwaysOutputReducer => {
                "Always output data cannot write an output that uses the sum_int or merge reducer."
            }
            Self::RouterContinueNeedsDefault => {
                "On error 'continue' on a Router needs an explicit default output."
            }
            Self::NodeErrorsType => {
                "When a node continues on error, a declared node_errors must be a list with reducer: append."
            }
            Self::NodeErrorsNestedMapping => {
                "An Agent node cannot map node_errors into or out of a nested pipeline."
            }
        }
    }

    /// Whether the refusal is about the whole document, not one node.
    #[must_use]
    pub const fn document_level(self) -> bool {
        matches!(self, Self::NodeErrorsType)
    }

    /// The message a person reads: `Node <id>: <text>`, or the text alone
    /// for a document-level refusal. An id that is not a graph id is never
    /// echoed: it reads `A pipeline node: <text>`, as in Main and the Web.
    #[must_use]
    pub fn message(self, node: &str) -> String {
        if self.document_level() {
            self.text().to_owned()
        } else if valid_graph_id(node) {
            format!("Node {node}: {}", self.text())
        } else {
            format!("A pipeline node: {}", self.text())
        }
    }
}

/// The node families whose failures the settings layer can settle without
/// I/O: their bodies are synchronous and pure, so every body error is a node
/// failure and a re-run yields the same output.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DeterministicFamily {
    StateModifier,
    Router,
    SplitOut,
    Aggregate,
}

impl DeterministicFamily {
    /// The YAML `type:` of the family.
    #[must_use]
    pub const fn node_type(self) -> &'static str {
        match self {
            Self::StateModifier => "state_modifier",
            Self::Router => "router",
            Self::SplitOut => "split_out",
            Self::Aggregate => "aggregate",
        }
    }

    /// Every family that admits settings in this release, in contract order.
    pub const ALL: [Self; 4] = [
        Self::Aggregate,
        Self::Router,
        Self::SplitOut,
        Self::StateModifier,
    ];
}

/// How a node type takes a `settings:` block.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SettingsFamily {
    Deterministic(DeterministicFamily),
    /// A node type whose slice has not wired settings yet (5b-1 PR B and
    /// 5b-2..5b-5): the whole block is refused.
    NotYet,
    /// HITL and Printer pause the run: no settings, ever.
    Pause,
}

impl SettingsFamily {
    #[must_use]
    pub fn of(node_type: &str) -> Self {
        match node_type {
            "state_modifier" => Self::Deterministic(DeterministicFamily::StateModifier),
            "router" => Self::Deterministic(DeterministicFamily::Router),
            "split_out" => Self::Deterministic(DeterministicFamily::SplitOut),
            "aggregate" => Self::Deterministic(DeterministicFamily::Aggregate),
            "hitl" | "printer" => Self::Pause,
            _ => Self::NotYet,
        }
    }

    /// The settings keys this family admits (the contract's `families`).
    #[must_use]
    pub const fn admitted_keys(self) -> &'static [&'static str] {
        match self {
            Self::Deterministic(_) => &[
                "on_error",
                "error_transition",
                "always_output_data",
                "execute_once",
            ],
            Self::NotYet | Self::Pause => &[],
        }
    }
}

impl NodeSettings {
    /// Parse and bound one `settings:` value. Family-independent: the
    /// shape, the bounds and the couplings between fields.
    pub fn parse(value: serde_yaml_ng::Value) -> Result<Self, NodeSettingsRefusal> {
        if !value.is_mapping() {
            return Err(NodeSettingsRefusal::Malformed);
        }
        let raw: RawNodeSettings =
            serde_yaml_ng::from_value(value).map_err(|_| NodeSettingsRefusal::Malformed)?;
        let tries = count(raw.max_tries.as_ref())?;
        let wait = count(raw.wait_between_tries_ms.as_ref())?;
        let timeout = count(raw.timeout_ms.as_ref())?;
        let max_tries = bounded::<u16>(
            tries,
            i64::from(MIN_NODE_TRIES),
            i64::from(MAX_NODE_TRIES),
            NodeSettingsRefusal::MaxTriesRange,
        )?;
        let wait_between_tries_ms = bounded::<u64>(
            wait,
            MIN_WAIT_BETWEEN_TRIES_MS.cast_signed(),
            MAX_WAIT_BETWEEN_TRIES_MS.cast_signed(),
            NodeSettingsRefusal::WaitRange,
        )?;
        let timeout_ms = bounded::<u64>(
            timeout,
            MIN_NODE_TIMEOUT_MS.cast_signed(),
            MAX_NODE_TIMEOUT_MS.cast_signed(),
            NodeSettingsRefusal::TimeoutRange,
        )?;
        if !raw.retry_on_fail && (max_tries.is_some() || wait_between_tries_ms.is_some()) {
            return Err(NodeSettingsRefusal::RetryFieldsNeedRetry);
        }
        match (raw.on_error, raw.error_transition.as_deref()) {
            (OnNodeError::ErrorOutput, None) => {
                return Err(NodeSettingsRefusal::ErrorTransitionRequired);
            }
            (OnNodeError::Stop | OnNodeError::Continue, Some(_)) => {
                return Err(NodeSettingsRefusal::ErrorTransitionUnexpected);
            }
            (_, Some(target)) if target != "END" && !valid_graph_id(target) => {
                return Err(NodeSettingsRefusal::ErrorTransitionTarget);
            }
            _ => {}
        }
        Ok(Self {
            retry_on_fail: raw.retry_on_fail,
            max_tries,
            wait_between_tries_ms,
            on_error: raw.on_error,
            error_transition: raw.error_transition,
            always_output_data: raw.always_output_data,
            execute_once: raw.execute_once,
            timeout_ms,
        })
    }

    /// Whether this block changes nothing: such a node binds and digests
    /// exactly as a node without a block.
    #[must_use]
    pub fn is_default(&self) -> bool {
        *self == Self::default()
    }

    /// Whether this node can write a `node_errors` entry.
    #[must_use]
    pub const fn uses_node_errors(&self) -> bool {
        !matches!(self.on_error, OnNodeError::Stop)
    }

    /// The per-family admission of a parsed block (the contract table).
    pub fn check_family(&self, family: SettingsFamily) -> Result<(), NodeSettingsRefusal> {
        match family {
            SettingsFamily::NotYet => return Err(NodeSettingsRefusal::FamilyNotYet),
            SettingsFamily::Pause => return Err(NodeSettingsRefusal::FamilyPause),
            SettingsFamily::Deterministic(_) => {}
        }
        let admitted = family.admitted_keys();
        if self.retry_on_fail && !admitted.contains(&"retry_on_fail") {
            return Err(NodeSettingsRefusal::RetryNotAllowed);
        }
        if self.timeout_ms.is_some() && !admitted.contains(&"timeout_ms") {
            return Err(NodeSettingsRefusal::TimeoutNotAllowed);
        }
        Ok(())
    }
}

/// The digest of every non-default block, keyed by node id; `None` when no
/// node has one, so a document without settings keeps its definition digest.
#[must_use]
pub fn settings_digest(settings: &BTreeMap<String, NodeSettings>) -> Option<[u8; 32]> {
    let admitted: BTreeMap<&String, &NodeSettings> = settings
        .iter()
        .filter(|(_, settings)| !settings.is_default())
        .collect();
    if admitted.is_empty() {
        return None;
    }
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(DIGEST_DOMAIN);
    context.update(&(admitted.len() as u64).to_be_bytes());
    for (id, settings) in admitted {
        let encoded = serde_json::to_vec(settings).unwrap_or_default();
        for field in [id.as_bytes(), encoded.as_slice()] {
            context.update(&(field.len() as u64).to_be_bytes());
            context.update(field);
        }
    }
    let mut value = [0; 32];
    value.copy_from_slice(context.finish().as_ref());
    Some(value)
}

/// Fold a settings digest into a definition digest.
#[must_use]
pub fn bind_digest(definition: [u8; 32], settings: Option<[u8; 32]>) -> [u8; 32] {
    let Some(settings) = settings else {
        return definition;
    };
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(DEFINITION_DOMAIN);
    context.update(&definition);
    context.update(&settings);
    let mut value = [0; 32];
    value.copy_from_slice(context.finish().as_ref());
    value
}

/// One `node_errors` entry (schema `pipeline-node-error-entry.v1`). Every
/// field is either a validated id, a registered code with its fixed catalog
/// sentence, or a number: no state, template, model, tool or code text.
pub struct NodeErrorEntry<'a> {
    pub node: &'a str,
    pub node_type: &'a str,
    pub handled: OnNodeError,
    pub code: &'a str,
    pub message: &'a str,
    pub support_ref: String,
    pub retryable: bool,
    pub attempts: u16,
    pub input_step: u64,
}

/// The entry is larger than [`MAX_NODE_ERROR_ENTRY_BYTES`] or its `error`
/// object larger than [`MAX_NODE_ERROR_OBJECT_BYTES`], or a field is outside
/// its grammar.
#[derive(Debug, Eq, PartialEq)]
pub struct EntryRefused;

impl NodeErrorEntry<'_> {
    pub fn to_value(&self) -> Result<Value, EntryRefused> {
        let handled = self.handled.handled().ok_or(EntryRefused)?;
        if !valid_graph_id(self.node) || !valid_code(self.code) || self.attempts == 0 {
            return Err(EntryRefused);
        }
        let error = json!({
            "code": self.code,
            "message_safe": safe_message(self.message),
            "support_ref": self.support_ref,
        });
        if serialized_len(&error) > MAX_NODE_ERROR_OBJECT_BYTES {
            return Err(EntryRefused);
        }
        let mut entry = Map::new();
        entry.insert("attempts".to_owned(), json!(self.attempts));
        entry.insert("error".to_owned(), error);
        entry.insert("handled".to_owned(), json!(handled));
        entry.insert("input_step".to_owned(), json!(self.input_step));
        entry.insert("node".to_owned(), json!(self.node));
        entry.insert("node_type".to_owned(), json!(self.node_type));
        entry.insert("retryable".to_owned(), json!(self.retryable));
        entry.insert("status".to_owned(), json!("failed"));
        let entry = Value::Object(entry);
        check_entry_bytes(&entry)?;
        Ok(entry)
    }
}

/// The whole-entry bound.
pub fn check_entry_bytes(entry: &Value) -> Result<(), EntryRefused> {
    if serialized_len(entry) > MAX_NODE_ERROR_ENTRY_BYTES {
        return Err(EntryRefused);
    }
    Ok(())
}

/// The support reference of one continued failure: stable across replay and
/// process replacement because its inputs are the run, the node and the
/// checkpoint step whose state was the node's input.
#[must_use]
pub fn node_error_support_ref(scope: &str, node: &str, step: u64) -> String {
    support_ref_for(&format!("{scope}\0{node}\0{step}"))
}

/// How many entries `node_errors` holds; anything but a list counts as full,
/// so a malformed channel is never appended to.
#[must_use]
pub fn node_error_count(value: Option<&Value>) -> usize {
    match value {
        None | Some(Value::Null) => 0,
        Some(Value::Array(entries)) => entries.len(),
        Some(_) => usize::MAX,
    }
}

/// The neutral value `always_output_data` writes for a declared state type.
#[must_use]
pub fn neutral_value(state_type: &str) -> Value {
    match state_type {
        "str" | "string" => json!(""),
        "list" => json!([]),
        "dict" => json!({}),
        "int" | "number" => json!(0),
        "float" => json!(0.0),
        "bool" => json!(false),
        _ => Value::Null,
    }
}

/// Whether `node` already ran in this turn, and the route it took when it is
/// a Router (`Some(None)` for a node with no recorded route).
#[must_use]
pub fn executed_once(set: Option<&Value>, node: &str) -> Option<Option<String>> {
    let nodes = set?.get("nodes")?.as_object()?;
    nodes
        .get(node)
        .map(|route| route.as_str().map(ToOwned::to_owned))
}

/// The run-once set refuses more than [`MAX_EXECUTED_ONCE_NODES`] ids.
#[derive(Debug, Eq, PartialEq)]
pub struct ExecutedOnceFull;

/// The run-once set with `node` added (and its route, for a Router). The
/// channel overwrites, so this is the whole new value.
pub fn mark_executed_once(
    set: Option<&Value>,
    node: &str,
    route: Option<&str>,
) -> Result<Value, ExecutedOnceFull> {
    let mut nodes = set
        .and_then(|set| set.get("nodes"))
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default();
    nodes.insert(
        node.to_owned(),
        route.map_or(Value::Null, |route| json!(route)),
    );
    if nodes.len() > MAX_EXECUTED_ONCE_NODES {
        return Err(ExecutedOnceFull);
    }
    Ok(json!({ "nodes": nodes }))
}

fn serialized_len(value: &Value) -> usize {
    serde_json::to_vec(value).map_or(usize::MAX, |bytes| bytes.len())
}

#[cfg(test)]
#[path = "node_settings_tests.rs"]
mod tests;
