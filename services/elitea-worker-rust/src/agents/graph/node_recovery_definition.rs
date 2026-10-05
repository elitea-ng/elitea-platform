//! Versioned, strict admission of node retry and dedicated failure handlers.

use std::collections::{BTreeMap, BTreeSet};

use ring::digest;
use serde::{Deserialize, Serialize};
use serde_yaml_ng::Value;

use super::{PipelineConfigurationError, PipelineNodeDefinition};
use crate::agents::graph::node_recovery::{
    ErrorRouteContract, NodeBackoff, NodeErrorRoute, NodeFailureClass, NodeRecoveryPolicy,
    NodeRetryMode, RetryCondition,
};

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct RawRecovery {
    #[serde(default = "one")]
    max_attempts: u16,
    #[serde(default)]
    retry_mode: NodeRetryMode,
    #[serde(default)]
    retry_on: BTreeSet<RetryCondition>,
    backoff: Option<NodeBackoff>,
    max_elapsed_ms: Option<u64>,
    on_failure: Option<RawErrorRoute>,
}

const fn one() -> u16 {
    1
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct RawErrorRoute {
    route: String,
    error_input: String,
    classes: BTreeSet<NodeFailureClass>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct RawFailureHandler {
    error_input: String,
}

#[derive(Default)]
pub(super) struct RawRecoveryCatalog {
    nodes: BTreeMap<String, RawRecovery>,
    handlers: BTreeMap<String, RawFailureHandler>,
    fixed_handlers: BTreeSet<String>,
}

#[derive(Clone)]
pub(super) struct RecoveryBinding {
    pub(super) policy: NodeRecoveryPolicy,
    pub(super) error_route: Option<(String, String)>,
}

#[derive(Clone, Default)]
pub(super) struct RecoveryCatalog {
    nodes: BTreeMap<String, RecoveryBinding>,
    digest: Option<[u8; 32]>,
}

impl RawRecoveryCatalog {
    /// Strip owned fields before the strict node-family parser runs.
    pub(super) fn extract(nodes: &mut [Value]) -> Result<Self, PipelineConfigurationError> {
        let mut catalog = Self::default();
        for node in nodes {
            let map = node.as_mapping_mut().ok_or_else(invalid)?;
            let id = map
                .get(Value::String("id".into()))
                .and_then(Value::as_str)
                .ok_or_else(invalid)?
                .to_owned();
            if let Some(raw) = map.remove(Value::String("recovery".into())) {
                let policy = serde_yaml_ng::from_value(raw).map_err(|_| invalid())?;
                if catalog.nodes.insert(id.clone(), policy).is_some() {
                    return Err(invalid());
                }
            }
            if let Some(raw) = map.remove(Value::String("failure_handler".into())) {
                let handler = serde_yaml_ng::from_value(raw).map_err(|_| invalid())?;
                if catalog.handlers.insert(id.clone(), handler).is_some() {
                    return Err(invalid());
                }
                let code = map.get(Value::String("code".into())).ok_or_else(invalid)?;
                let fixed = code.is_string()
                    || code.as_mapping().is_some_and(|source| {
                        source
                            .get(Value::String("type".into()))
                            .and_then(Value::as_str)
                            == Some("fixed")
                    });
                if fixed {
                    catalog.fixed_handlers.insert(id);
                }
            }
        }
        Ok(catalog)
    }

    pub(super) fn admit(
        self,
        nodes: &[PipelineNodeDefinition],
        state: &BTreeMap<String, String>,
        entry: &str,
        owned: &BTreeSet<String>,
    ) -> Result<RecoveryCatalog, PipelineConfigurationError> {
        let mut result = RecoveryCatalog::default();
        let mut used_handlers = BTreeSet::new();
        let mut error_inputs = BTreeSet::new();
        for (id, raw) in &self.nodes {
            let producer = nodes
                .iter()
                .find(|node| node.id() == id)
                .ok_or_else(invalid)?;
            if !matches!(producer, PipelineNodeDefinition::Code(_)) || owned.contains(id) {
                return Err(PipelineConfigurationError::Unsupported(
                    "node recovery requires an admitted Code attempt authority",
                ));
            }
            let (error_route, route_policy) = match &raw.on_failure {
                None => (None, None),
                Some(route) => {
                    let declared = self.handlers.get(&route.route).ok_or_else(invalid)?;
                    let target = nodes
                        .iter()
                        .find(|node| node.id() == route.route)
                        .ok_or_else(invalid)?;
                    let PipelineNodeDefinition::Code(handler) = target else {
                        return Err(invalid());
                    };
                    if route.route == *id
                        || route.route == entry
                        || owned.contains(&route.route)
                        || declared.error_input != route.error_input
                        || !self.fixed_handlers.contains(&route.route)
                        || state.get(&route.error_input).map(String::as_str) != Some("dict")
                        || handler.input_keys() != [route.error_input.as_str()]
                        || handler.output_keys().is_empty()
                        || handler.transition() != Some("END")
                        || handler.validated_digest() == producer.config_digest()
                        || nodes
                            .iter()
                            .any(|node| node.route_targets().contains(&route.route.as_str()))
                        || nodes
                            .iter()
                            .any(|node| node.output_keys().contains(&route.error_input))
                        || nodes.iter().any(|node| {
                            node.id() != route.route
                                && node.input_keys().contains(&route.error_input)
                        })
                    {
                        return Err(invalid());
                    }
                    // The authored contract isolates this handler from all success paths.
                    // V1 has no product authorization contract for denial handlers.
                    let encoded =
                        serde_json::to_vec(&(id, route, declared)).map_err(|_| invalid())?;
                    let identity = hash(b"elitea.pipeline.node-error-route.v1\0", &encoded);
                    let admitted = NodeErrorRoute::admit(
                        identity,
                        route.classes.clone(),
                        ErrorRouteContract {
                            dedicated_typed_error_input: true,
                            requires_success_output: false,
                            reexecutes_failed_operation: false,
                            explicitly_handles_denial: false,
                        },
                    )
                    .map_err(|_| invalid())?;
                    used_handlers.insert(route.route.clone());
                    if !error_inputs.insert(route.error_input.clone()) {
                        return Err(invalid());
                    }
                    (
                        Some((route.route.clone(), route.error_input.clone())),
                        Some(admitted),
                    )
                }
            };
            let policy = NodeRecoveryPolicy::admit(
                raw.max_attempts,
                raw.retry_on.clone(),
                raw.backoff,
                raw.max_elapsed_ms,
                route_policy,
            )
            .and_then(|policy| policy.with_retry_mode(raw.retry_mode))
            .map_err(|_| invalid())?;
            result.nodes.insert(
                id.clone(),
                RecoveryBinding {
                    policy,
                    error_route,
                },
            );
        }
        if self.handlers.keys().any(|id| !used_handlers.contains(id)) {
            return Err(invalid());
        }
        if !result.nodes.is_empty() {
            let encoded =
                serde_json::to_vec(&(self.nodes, self.handlers)).map_err(|_| invalid())?;
            result.digest = Some(hash(b"elitea.pipeline.node-recovery-policy.v1\0", &encoded));
        }
        Ok(result)
    }
}

impl RecoveryCatalog {
    pub(super) fn get(&self, id: &str) -> Option<&RecoveryBinding> {
        self.nodes.get(id)
    }
    pub(super) fn bind_digest(&self, definition: [u8; 32]) -> [u8; 32] {
        let Some(policy) = self.digest else {
            return definition;
        };
        let mut bytes = [0; 64];
        bytes[..32].copy_from_slice(&definition);
        bytes[32..].copy_from_slice(&policy);
        hash(b"elitea.pipeline.node-recovery-definition.v1\0", &bytes)
    }
}

fn hash(domain: &[u8], bytes: &[u8]) -> [u8; 32] {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(domain);
    context.update(bytes);
    let mut value = [0; 32];
    value.copy_from_slice(context.finish().as_ref());
    value
}
fn invalid() -> PipelineConfigurationError {
    PipelineConfigurationError::Invalid(
        "the node recovery policy or dedicated failure handler is invalid",
    )
}
