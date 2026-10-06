//! Freeze existing Agent mappings before any fixed Parallel branch starts.

use serde::{Deserialize, Serialize};

use super::{
    ApplicationInputMapping, ApplicationNode, ApplicationNodeDefinition, MAX_MAPPING_VALUE_BYTES,
    node_failure, render_fstring,
};
use adk_rust::graph::{GraphError, Node, State};
use serde_json::Value;
use std::collections::BTreeMap;

pub(in crate::agents::graph) const PARALLEL_AGENT_INPUTS_STATE_KEY: &str =
    "__elitea_parallel_agent_inputs_v1";

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct FrozenAgentInputs {
    task: String,
    variables: BTreeMap<String, Value>,
}

impl ApplicationNodeDefinition {
    pub(in crate::agents::graph) fn freeze_parallel_input(
        &self,
        parent: &State,
    ) -> Result<State, GraphError> {
        super::super::parallel::validate_input_state(parent)?;
        super::super::parallel::validate_values(self.variables.values().filter_map(|mapping| {
            match mapping {
                ApplicationInputMapping::Fixed(value) => Some(value),
                _ => None,
            }
        }))?;
        let task = self.map_task(parent).map_err(|_| node_failure(self.id()))?;
        let mut variables = BTreeMap::new();
        for (key, mapping) in &self.variables {
            let value = match mapping {
                ApplicationInputMapping::Fixed(value) => value.clone(),
                ApplicationInputMapping::Variable(source) => parent
                    .get(source)
                    .cloned()
                    .ok_or_else(|| node_failure(self.id()))?,
                ApplicationInputMapping::Template(template) => Value::String(
                    render_fstring(template, parent).map_err(|_| node_failure(self.id()))?,
                ),
            };
            variables.insert(key.clone(), value);
        }
        let frozen = serde_json::to_value(FrozenAgentInputs { task, variables })
            .map_err(|_| node_failure(self.id()))?;
        super::super::parallel::ensure_bounded_json(&frozen, MAX_MAPPING_VALUE_BYTES, "input")
            .map_err(|_| node_failure(self.id()))?;
        let mut state = self
            .input
            .iter()
            .filter_map(|key| parent.get(key).map(|value| (key.clone(), value.clone())))
            .collect::<State>();
        state.insert(PARALLEL_AGENT_INPUTS_STATE_KEY.to_owned(), frozen);
        Ok(state)
    }
}

impl ApplicationNode {
    pub(in crate::agents::graph) fn with_parallel_inputs(mut self) -> Self {
        self.parallel_inputs = true;
        self
    }

    fn frozen_inputs(&self, state: &State) -> Result<FrozenAgentInputs, GraphError> {
        let raw = state
            .get(PARALLEL_AGENT_INPUTS_STATE_KEY)
            .ok_or_else(|| node_failure(self.name()))?;
        super::super::parallel::validate_values([raw])?;
        let frozen: FrozenAgentInputs =
            serde_json::from_value(raw.clone()).map_err(|_| node_failure(self.name()))?;
        if frozen.task.is_empty()
            || frozen.task.len() > MAX_MAPPING_VALUE_BYTES
            || frozen.task.contains('\0')
            || !frozen.variables.keys().eq(self.definition.variables.keys())
        {
            return Err(node_failure(self.name()));
        }
        Ok(frozen)
    }

    pub(super) fn mapped_task(&self, state: &State) -> Result<String, GraphError> {
        if self.parallel_inputs {
            Ok(self.frozen_inputs(state)?.task)
        } else {
            self.definition
                .map_task(state)
                .map_err(|_| node_failure(self.name()))
        }
    }

    pub(super) fn parallel_variable(&self, state: &State, key: &str) -> Result<Value, GraphError> {
        self.frozen_inputs(state)?
            .variables
            .remove(key)
            .ok_or_else(|| node_failure(self.name()))
    }
}
