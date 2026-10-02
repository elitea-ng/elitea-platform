use std::collections::BTreeSet;
use std::fmt;

use ring::digest;
use serde::Deserialize;
use serde::de::{Deserializer, SeqAccess, Visitor};
use thiserror::Error;

const MAX_YAML_NODE_BYTES: usize = 64 * 1024;
pub(crate) const MAX_NODE_ID_BYTES: usize = 128;
const MAX_OUTPUT_KEY_BYTES: usize = 256;
const MAX_BRANCHES: usize = 64;
const MAX_CONCURRENCY: u32 = 32;
const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.parallel.config.v1\0";

/// The only scheduling barrier currently exposed by the Elitea YAML contract.
///
/// ADK-Rust 2.0.0's action merge modes inspect results already present in
/// state. They are not early-release schedulers, so `one` and `many` remain
/// invalid until their completed set, deadline, sibling cancellation and late
/// results have a durable contract.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub enum ParallelWaitPolicy {
    All,
}

/// What a `wait: all` node does after one or more branches fail.
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub enum ParallelErrorPolicy {
    #[default]
    FailAfterDrain,
}

/// One stable branch of a YAML `parallel` node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ParallelBranchDefinition {
    id: String,
    node: String,
}

impl ParallelBranchDefinition {
    #[must_use]
    pub fn id(&self) -> &str {
        &self.id
    }

    #[must_use]
    pub fn node(&self) -> &str {
        &self.node
    }
}

/// Strict v1 YAML contract for a true graph parallel node.
///
/// Each branch names a compiler-owned child graph entry. A single-node branch
/// can wrap an existing node; a multi-node branch can compile a subgraph. The
/// branch graph is checkpointed independently, which is what lets a completed
/// short branch survive a process loss while a longer sibling is still active.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ParallelNodeDefinition {
    id: String,
    node_type: String,
    branches: Vec<ParallelBranchDefinition>,
    max_concurrency: u32,
    wait: ParallelWaitPolicy,
    error_policy: ParallelErrorPolicy,
    output: String,
    transition: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawParallelBranchDefinition {
    id: String,
    node: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawParallelNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    #[serde(deserialize_with = "deserialize_branches")]
    branches: Vec<RawParallelBranchDefinition>,
    max_concurrency: u32,
    wait: ParallelWaitPolicy,
    #[serde(default)]
    error_policy: ParallelErrorPolicy,
    #[serde(deserialize_with = "deserialize_outputs")]
    output: Vec<String>,
    #[serde(default)]
    transition: Option<String>,
}

fn deserialize_branches<'de, D>(
    deserializer: D,
) -> Result<Vec<RawParallelBranchDefinition>, D::Error>
where
    D: Deserializer<'de>,
{
    struct BranchesVisitor;

    impl<'de> Visitor<'de> for BranchesVisitor {
        type Value = Vec<RawParallelBranchDefinition>;

        fn expecting(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
            formatter.write_str("at most 64 parallel branch mappings")
        }

        fn visit_seq<A>(self, mut sequence: A) -> Result<Self::Value, A::Error>
        where
            A: SeqAccess<'de>,
        {
            let mut branches = Vec::new();
            while let Some(branch) = sequence.next_element()? {
                if branches.len() == MAX_BRANCHES {
                    return Err(serde::de::Error::custom(
                        "the parallel branch count exceeds its resource bound",
                    ));
                }
                branches.push(branch);
            }
            Ok(branches)
        }
    }

    deserializer.deserialize_seq(BranchesVisitor)
}

fn deserialize_outputs<'de, D>(deserializer: D) -> Result<Vec<String>, D::Error>
where
    D: Deserializer<'de>,
{
    struct OutputsVisitor;

    impl<'de> Visitor<'de> for OutputsVisitor {
        type Value = Vec<String>;

        fn expecting(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
            formatter.write_str("exactly one parallel output channel")
        }

        fn visit_seq<A>(self, mut sequence: A) -> Result<Self::Value, A::Error>
        where
            A: SeqAccess<'de>,
        {
            let mut outputs = Vec::new();
            while let Some(output) = sequence.next_element()? {
                if outputs.len() == 1 {
                    return Err(serde::de::Error::custom(
                        "the parallel output count exceeds its resource bound",
                    ));
                }
                outputs.push(output);
            }
            Ok(outputs)
        }
    }

    deserializer.deserialize_seq(OutputsVisitor)
}

impl ParallelNodeDefinition {
    /// Parse and validate one YAML node mapping.
    ///
    /// The full pipeline compiler will deserialize this type only after it has
    /// selected `type: parallel`. This bounded helper exists for focused API and
    /// compatibility tests; it must not be used to parse an unbounded document.
    ///
    /// # Errors
    ///
    /// Returns a stable, data-free error for malformed YAML or an unsupported
    /// scheduling contract. The parser source is retained only for a trusted
    /// diagnostic sink.
    pub fn from_yaml(yaml: &str) -> Result<Self, ParallelConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_YAML_NODE_BYTES {
            return Err(ParallelConfigurationError::ResourceExhausted);
        }
        let raw = serde_yaml_ng::from_str::<RawParallelNodeDefinition>(yaml)
            .map_err(|source| ParallelConfigurationError::MalformedYaml { source })?;
        let definition = Self::from_raw(raw)?;
        definition.validate()?;
        Ok(definition)
    }

    #[must_use]
    pub fn id(&self) -> &str {
        &self.id
    }

    #[must_use]
    pub fn branches(&self) -> &[ParallelBranchDefinition] {
        &self.branches
    }

    #[must_use]
    pub const fn max_concurrency(&self) -> u32 {
        self.max_concurrency
    }

    #[must_use]
    pub const fn wait(&self) -> ParallelWaitPolicy {
        self.wait
    }

    #[must_use]
    pub const fn error_policy(&self) -> ParallelErrorPolicy {
        self.error_policy
    }

    #[must_use]
    pub fn output_key(&self) -> &str {
        &self.output
    }

    #[must_use]
    pub fn transition(&self) -> Option<&str> {
        self.transition.as_deref()
    }

    /// Stable digest bound into every child checkpoint thread identity.
    pub(crate) fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        digest_field(&mut context, self.output_key().as_bytes());
        digest_field(&mut context, &u64::from(self.max_concurrency).to_be_bytes());
        digest_field(&mut context, b"wait:all");
        digest_field(&mut context, b"fail_after_drain");
        match &self.transition {
            Some(transition) => digest_field(&mut context, transition.as_bytes()),
            None => digest_field(&mut context, &[]),
        }
        for branch in &self.branches {
            digest_field(&mut context, branch.id.as_bytes());
            digest_field(&mut context, branch.node.as_bytes());
        }
        digest_bytes(context.finish().as_ref())
    }

    pub(crate) fn validate(&self) -> Result<(), ParallelConfigurationError> {
        if self.node_type != "parallel" {
            return Err(ParallelConfigurationError::Invalid(
                "the node type must be parallel",
            ));
        }
        if !valid_graph_id(&self.id) {
            return Err(ParallelConfigurationError::Invalid(
                "the parallel node ID is malformed",
            ));
        }
        if self.branches.len() < 2 || self.branches.len() > MAX_BRANCHES {
            return Err(ParallelConfigurationError::Invalid(
                "a parallel node must declare between 2 and 64 branches",
            ));
        }
        let branch_count = u32::try_from(self.branches.len()).map_err(|_| {
            ParallelConfigurationError::Invalid("the parallel branch count overflowed")
        })?;
        if self.max_concurrency == 0
            || self.max_concurrency > MAX_CONCURRENCY
            || self.max_concurrency > branch_count
        {
            return Err(ParallelConfigurationError::Invalid(
                "parallel max_concurrency is outside the supported bound",
            ));
        }
        if !valid_output_key(&self.output) {
            return Err(ParallelConfigurationError::Invalid(
                "the parallel output channel is malformed",
            ));
        }
        if self
            .transition
            .as_deref()
            .is_some_and(|transition| transition != "END" && !valid_graph_id(transition))
        {
            return Err(ParallelConfigurationError::Invalid(
                "the parallel transition target is malformed",
            ));
        }

        let mut branch_ids = BTreeSet::new();
        for branch in &self.branches {
            if !valid_graph_id(&branch.id) || !valid_graph_id(&branch.node) {
                return Err(ParallelConfigurationError::Invalid(
                    "a parallel branch ID or target node is malformed",
                ));
            }
            if branch.node == self.id {
                return Err(ParallelConfigurationError::Invalid(
                    "a parallel branch cannot invoke its parent node",
                ));
            }
            if !branch_ids.insert(branch.id.as_str()) {
                return Err(ParallelConfigurationError::Invalid(
                    "parallel branch IDs must be unique",
                ));
            }
        }
        Ok(())
    }

    fn from_raw(raw: RawParallelNodeDefinition) -> Result<Self, ParallelConfigurationError> {
        if raw.output.len() != 1 {
            return Err(ParallelConfigurationError::Invalid(
                "a parallel node must declare exactly one valid output channel",
            ));
        }
        let mut output = raw.output.into_iter();
        let output = output.next().ok_or(ParallelConfigurationError::Invalid(
            "a parallel node must declare exactly one valid output channel",
        ))?;
        Ok(Self {
            id: raw.id,
            node_type: raw.node_type,
            branches: raw
                .branches
                .into_iter()
                .map(|branch| ParallelBranchDefinition {
                    id: branch.id,
                    node: branch.node,
                })
                .collect(),
            max_concurrency: raw.max_concurrency,
            wait: raw.wait,
            error_policy: raw.error_policy,
            output,
            transition: raw.transition,
        })
    }
}

/// Safe configuration failure for the YAML compiler boundary.
#[derive(Debug, Error)]
pub enum ParallelConfigurationError {
    #[error("the parallel YAML node exceeds its resource bound")]
    ResourceExhausted,
    #[error("the parallel YAML node is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("the parallel YAML node is invalid: {0}")]
    Invalid(&'static str),
}

impl ParallelConfigurationError {
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::ResourceExhausted => "graph.parallel.configuration_resource_exhausted",
            Self::MalformedYaml { .. } => "graph.parallel.malformed_yaml",
            Self::Invalid(_) => "graph.parallel.invalid_configuration",
        }
    }
}

pub(crate) fn valid_graph_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_NODE_ID_BYTES
        && value
            .as_bytes()
            .iter()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.' | b':'))
}

pub(crate) fn valid_output_key(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_OUTPUT_KEY_BYTES
        && !value
            .as_bytes()
            .iter()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
}

fn digest_bytes(value: &[u8]) -> [u8; 32] {
    let mut bytes = [0_u8; 32];
    bytes.copy_from_slice(value);
    bytes
}

fn digest_field(context: &mut digest::Context, value: &[u8]) {
    let length = u64::try_from(value.len()).unwrap_or(u64::MAX);
    context.update(&length.to_be_bytes());
    context.update(value);
}
