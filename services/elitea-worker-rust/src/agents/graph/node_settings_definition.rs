//! Compile-time admission of per-node `settings:` (5b-1).
//!
//! The block is stripped from every raw node before the strict family
//! parsers run (as `recovery:` is), parsed and bounded by the shared policy
//! (`elitea_agent_runtime::graph::node_settings`), and admitted against the
//! parsed graph: the family table, fan-out ownership, the error route, the
//! Router default and the reducers. Refusals are typed and name the node.

use std::collections::{BTreeMap, BTreeSet};

use serde_yaml_ng::Value;

use super::{PipelineConfigurationError, PipelineNodeDefinition, StateReducer};
use elitea_agent_runtime::graph::node_settings::{
    DeterministicFamily, NODE_ERRORS_KEY, NodeSettings, NodeSettingsRefusal, OnNodeError,
    SettingsFamily, bind_digest, settings_digest,
};

/// One raw node's block and the raw facts admission needs from the YAML.
struct RawNodeSettings {
    settings: NodeSettings,
    node_type: String,
    /// The node also declares `recovery:` or `failure_handler:`.
    with_recovery: bool,
    /// The raw YAML names `default_output` (a Router): the parser fills END
    /// when it is absent, which never counts as a declared default (C5).
    declares_default_output: bool,
}

#[derive(Default)]
pub(super) struct RawSettingsCatalog {
    nodes: BTreeMap<String, RawNodeSettings>,
}

/// One admitted node's settings and what its wrapper needs at run time.
#[derive(Clone, Debug)]
pub(crate) struct AdmittedNodeSettings {
    pub(crate) settings: NodeSettings,
    pub(crate) family: DeterministicFamily,
    /// `(key, declared state type)` of every declared output, for
    /// `always_output_data`.
    pub(crate) outputs: Vec<(String, String)>,
    /// A Router's default route: its target and its public label.
    pub(crate) router_default: Option<(String, String)>,
}

#[derive(Clone, Default)]
pub(crate) struct SettingsCatalog {
    nodes: BTreeMap<String, AdmittedNodeSettings>,
    digest: Option<[u8; 32]>,
}

fn refusal(node: &str, refusal: NodeSettingsRefusal) -> PipelineConfigurationError {
    PipelineConfigurationError::NodeSettings {
        node: node.to_owned(),
        refusal,
    }
}

impl RawSettingsCatalog {
    /// Strip `settings:` before the strict node-family parser runs. Must run
    /// before the recovery catalog strips `recovery:`/`failure_handler:`.
    pub(super) fn extract(nodes: &mut [Value]) -> Result<Self, PipelineConfigurationError> {
        let mut catalog = Self::default();
        for node in nodes {
            let Some(map) = node.as_mapping_mut() else {
                continue;
            };
            let Some(raw) = map.remove(Value::String("settings".into())) else {
                continue;
            };
            // Decision 1: a block with no keys is no block, on every family.
            if raw
                .as_mapping()
                .is_some_and(serde_yaml_ng::Mapping::is_empty)
            {
                continue;
            }
            let id = map
                .get(Value::String("id".into()))
                .and_then(Value::as_str)
                .ok_or(PipelineConfigurationError::Invalid(
                    "a pipeline node with settings has no identifier",
                ))?
                .to_owned();
            let settings = NodeSettings::parse(raw).map_err(|why| refusal(&id, why))?;
            let node_type = map
                .get(Value::String("type".into()))
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned();
            let with_recovery = map.contains_key(Value::String("recovery".into()))
                || map.contains_key(Value::String("failure_handler".into()));
            let declares_default_output = map.contains_key(Value::String("default_output".into()));
            let entry = RawNodeSettings {
                settings,
                node_type,
                with_recovery,
                declares_default_output,
            };
            if catalog.nodes.insert(id.clone(), entry).is_some() {
                return Err(PipelineConfigurationError::Invalid(
                    "pipeline node identifiers must be unique",
                ));
            }
        }
        Ok(catalog)
    }

    /// Whether some node can append to `node_errors` (D2): only then is the
    /// channel declared, and only then is a user's own key checked (C3).
    pub(super) fn uses_node_errors(&self) -> bool {
        self.nodes
            .values()
            .any(|node| node.settings.uses_node_errors())
    }

    /// Declare the `node_errors` channel when some node can write it (D2).
    /// A user's own key must be a list with no reducer or `append`; the
    /// channel then appends. Without such a node nothing is checked (C3).
    pub(super) fn declare_node_errors(
        &self,
        validated: super::ValidatedState,
    ) -> Result<super::ValidatedState, PipelineConfigurationError> {
        let (mut state, mut defaults, mut declaration_order, mut reducers) = validated;
        if !self.uses_node_errors() {
            return Ok((state, defaults, declaration_order, reducers));
        }
        // Decision 2: a user's own key must say `reducer: append` explicitly.
        match state.get(NODE_ERRORS_KEY).map(String::as_str) {
            Some("list") if matches!(reducers.get(NODE_ERRORS_KEY), Some(StateReducer::Append)) => {
            }
            Some(_) => {
                return Err(refusal(
                    NODE_ERRORS_KEY,
                    NodeSettingsRefusal::NodeErrorsType,
                ));
            }
            None => {
                state.insert(NODE_ERRORS_KEY.to_owned(), "list".to_owned());
                defaults.insert(NODE_ERRORS_KEY.to_owned(), serde_json::json!([]));
                declaration_order.push(NODE_ERRORS_KEY.to_owned());
            }
        }
        reducers.insert(NODE_ERRORS_KEY.to_owned(), StateReducer::Append);
        Ok((state, defaults, declaration_order, reducers))
    }

    pub(super) fn admit(
        self,
        nodes: &[PipelineNodeDefinition],
        state: &BTreeMap<String, String>,
        reducers: &BTreeMap<String, StateReducer>,
        owned: &BTreeSet<String>,
    ) -> Result<SettingsCatalog, PipelineConfigurationError> {
        // C4: a document that owns `node_errors` never maps it into or out
        // of a nested pipeline; the child keeps its own entries.
        if self.uses_node_errors() {
            for node in nodes {
                if let PipelineNodeDefinition::Application(agent) = node
                    && (agent.variable_keys().any(|key| key == NODE_ERRORS_KEY)
                        || agent.output_keys().iter().any(|key| key == NODE_ERRORS_KEY))
                {
                    return Err(refusal(
                        agent.id(),
                        NodeSettingsRefusal::NodeErrorsNestedMapping,
                    ));
                }
            }
        }
        let mut admitted = BTreeMap::new();
        let mut digested = BTreeMap::new();
        for (id, raw) in self.nodes {
            if raw.with_recovery {
                return Err(refusal(&id, NodeSettingsRefusal::WithRecovery));
            }
            let family = SettingsFamily::of(&raw.node_type);
            raw.settings
                .check_family(family)
                .map_err(|why| refusal(&id, why))?;
            let SettingsFamily::Deterministic(family) = family else {
                return Err(refusal(&id, NodeSettingsRefusal::FamilyNotYet));
            };
            if owned.contains(&id) {
                return Err(refusal(&id, NodeSettingsRefusal::FanoutOwned));
            }
            let node = nodes.iter().find(|node| node.id() == id).ok_or(
                PipelineConfigurationError::Invalid(
                    "a pipeline node with settings is not admitted",
                ),
            )?;
            if let Some(target) = raw.settings.error_transition.as_deref()
                && target != "END"
                && (target == id
                    || owned.contains(target)
                    || !nodes.iter().any(|node| node.id() == target))
            {
                return Err(refusal(&id, NodeSettingsRefusal::ErrorTransitionTarget));
            }
            let router_default = match node {
                PipelineNodeDefinition::Router(router) => {
                    if raw.settings.on_error == OnNodeError::Continue
                        && !raw.declares_default_output
                    {
                        return Err(refusal(
                            &id,
                            NodeSettingsRefusal::RouterContinueNeedsDefault,
                        ));
                    }
                    let (target, label) = router.default_route();
                    Some((target.to_owned(), label.to_owned()))
                }
                _ => None,
            };
            let outputs: Vec<(String, String)> = node
                .output_keys()
                .iter()
                .map(|key| {
                    (
                        key.clone(),
                        state.get(key).cloned().unwrap_or_else(|| "str".to_owned()),
                    )
                })
                .collect();
            if raw.settings.always_output_data
                && outputs.iter().any(|(key, _)| {
                    matches!(
                        reducers.get(key),
                        Some(StateReducer::SumInt | StateReducer::Merge)
                    )
                })
            {
                return Err(refusal(&id, NodeSettingsRefusal::AlwaysOutputReducer));
            }
            if raw.settings.is_default() {
                continue;
            }
            digested.insert(id.clone(), raw.settings.clone());
            admitted.insert(
                id,
                AdmittedNodeSettings {
                    settings: raw.settings,
                    family,
                    outputs,
                    router_default,
                },
            );
        }
        Ok(SettingsCatalog {
            digest: settings_digest(&digested),
            nodes: admitted,
        })
    }
}

impl SettingsCatalog {
    pub(crate) fn get(&self, id: &str) -> Option<&AdmittedNodeSettings> {
        self.nodes.get(id)
    }

    /// Whether some admitted node writes `node_errors`.
    pub(crate) fn uses_node_errors(&self) -> bool {
        self.nodes
            .values()
            .any(|node| node.settings.uses_node_errors())
    }

    /// Whether the settings own state channel `key`: `node_errors` when some
    /// node continues on error. An owned channel is never a result candidate,
    /// a mappable child variable, or a reason for the nested reducer refusal.
    pub(crate) fn owns_key(&self, key: &str) -> bool {
        key == NODE_ERRORS_KEY && self.uses_node_errors()
    }

    /// Whether some admitted node runs once per turn: only then is the
    /// private run-once channel declared.
    pub(crate) fn uses_execute_once(&self) -> bool {
        self.nodes.values().any(|node| node.settings.execute_once)
    }

    /// The error route of `id`, a successor like its transitions.
    pub(crate) fn error_transition(&self, id: &str) -> Option<&str> {
        self.nodes
            .get(id)
            .and_then(|node| node.settings.error_transition.as_deref())
    }

    /// Fold the settings into the definition digest; a document without
    /// non-default settings keeps its digest byte for byte.
    pub(crate) fn bind_digest(&self, definition: [u8; 32]) -> [u8; 32] {
        bind_digest(definition, self.digest)
    }
}
